# 4. EventBridge Event Routing

A single Lambda function is frequently the target of multiple EventBridge rules, each delivering a different `detail-type`. The hard part is not parsing JSON — it is building a router that stays readable as the number of event types grows, handles unknown types gracefully, and enforces source validation so that events from unexpected producers are silently dropped rather than processed or panicked on.

This lesson builds a realistic e-commerce order router that receives three `detail-type` values (`order.created`, `order.shipped`, `order.cancelled`) from a single source (`ecommerce.orders`), dispatches to dedicated processors, and is fully tested without deploying anything.

```text
orderrouter/
  go.mod
  router.go
  router_test.go
  cmd/demo/main.go
```

The package is `orderrouter`. Tests reach unexported state. The demo in `cmd/demo` exercises only the exported API.

## Concepts

### The EventBridge Envelope

Every EventBridge event delivered to a Lambda has the same outer structure:

```json
{
  "version": "0",
  "id": "abc-123",
  "source": "ecommerce.orders",
  "detail-type": "order.created",
  "account": "123456789012",
  "time": "2024-01-15T10:30:00Z",
  "region": "us-east-1",
  "resources": [],
  "detail": { "order_id": "ORD-001", "customer": "alice", "items": ["widget"] }
}
```

The Go type is `events.CloudWatchEvent` from `github.com/aws/aws-lambda-go/events` (the name is historical; this type handles both CloudWatch Events and modern EventBridge events):

```go
type CloudWatchEvent struct {
	Version    string          `json:"version"`
	ID         string          `json:"id"`
	DetailType string          `json:"detail-type"`
	Source     string          `json:"source"`
	AccountID  string          `json:"account"`
	Time       time.Time       `json:"time"`
	Region     string          `json:"region"`
	Resources  []string        `json:"resources"`
	Detail     json.RawMessage `json:"detail"`
}
```

`Detail` is `json.RawMessage` — a `[]byte` alias. It holds the raw JSON of the `detail` object, deferred until you know which struct to unmarshal into. This is the foundation of type-safe routing.

### Source Validation

A Lambda subscribed to an EventBridge bus can receive events from multiple rules and multiple sources. Source validation is the first filter: if `event.Source` is not the expected producer, log and return `nil`. Returning an error would cause Lambda to retry the event, which is wrong — the event is valid, it just is not for this handler.

### Detail-Type Dispatch

Once the source is validated, `event.DetailType` is the dispatch key. A `switch` statement over `DetailType` is idiomatic. Each case unmarshals `event.Detail` into the concrete struct for that event type:

```go
switch event.DetailType {
case "order.created":
	var d OrderCreated
	if err := json.Unmarshal(event.Detail, &d); err != nil {
		return fmt.Errorf("orderrouter: unmarshal order.created: %w", err)
	}
	return r.handleOrderCreated(ctx, d)
}
```

The unmarshal error is wrapped with context (`%w`) so callers can use `errors.Is`/`errors.As` without matching strings.

### Unknown Detail-Types Are Not Errors

A bus rule may evolve to publish new event types before the Lambda is updated. Unknown `DetailType` values must be logged and returned as `nil` — not errors. Lambda retries on error; an unknown event type is not a transient failure, so retrying it would flood the dead-letter queue.

### Testing Without Deploying

The Lambda handler is a plain function: `func(ctx context.Context, event events.CloudWatchEvent) error`. Tests construct `CloudWatchEvent` values directly from Go, marshal the `Detail` field, and call the handler. No mock infrastructure, no localstack, no network.

The `Router` type wraps the processors so tests can inject recorded calls and assert routing correctness.

## Exercises

Set up the module:

```bash
go get github.com/aws/aws-lambda-go@v1.47.0
```

### Exercise 1: Domain Types and Sentinel Errors

Create `router.go`. Start with the event detail structs and sentinel errors:

```go
package orderrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

// ErrUnknownSource is returned when an event arrives from an unexpected source.
// Callers use errors.Is to distinguish this from unmarshal errors.
var ErrUnknownSource = errors.New("orderrouter: unknown source")

// ErrMalformedDetail is the sentinel for unmarshal failures on event.Detail.
var ErrMalformedDetail = errors.New("orderrouter: malformed detail")

// ExpectedSource is the only source this router accepts.
const ExpectedSource = "ecommerce.orders"

// OrderCreated is the detail payload for the "order.created" detail-type.
type OrderCreated struct {
	OrderID  string   `json:"order_id"`
	Customer string   `json:"customer"`
	Items    []string `json:"items"`
}

// OrderShipped is the detail payload for the "order.shipped" detail-type.
type OrderShipped struct {
	OrderID        string `json:"order_id"`
	TrackingNumber string `json:"tracking_number"`
}

// OrderCancelled is the detail payload for the "order.cancelled" detail-type.
type OrderCancelled struct {
	OrderID string `json:"order_id"`
	Reason  string `json:"reason"`
}
```

### Exercise 2: The Router Type and Processor Interface

Continue `router.go` with the router type. Processors are injected via a struct so tests can record calls:

```go
// Processor holds the handler functions for each event type.
// Fields are functions so tests can inject recording closures.
type Processor struct {
	OnOrderCreated   func(ctx context.Context, e OrderCreated) error
	OnOrderShipped   func(ctx context.Context, e OrderShipped) error
	OnOrderCancelled func(ctx context.Context, e OrderCancelled) error
}

// Router dispatches EventBridge events to the correct processor.
type Router struct {
	source string
	proc   Processor
	log    *slog.Logger
}

// New returns a Router that accepts events from source and dispatches using p.
// source must be non-empty; p's handler fields must be non-nil.
func New(source string, p Processor, log *slog.Logger) (*Router, error) {
	if source == "" {
		return nil, fmt.Errorf("orderrouter: source must not be empty")
	}
	if p.OnOrderCreated == nil || p.OnOrderShipped == nil || p.OnOrderCancelled == nil {
		return nil, fmt.Errorf("orderrouter: all Processor fields must be non-nil")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Router{source: source, proc: p, log: log}, nil
}

// Handle is the Lambda handler signature. It validates the source,
// dispatches on DetailType, and returns nil for unknown types.
func (r *Router) Handle(ctx context.Context, event events.CloudWatchEvent) error {
	if event.Source != r.source {
		r.log.InfoContext(ctx, "ignoring event from unexpected source",
			"got", event.Source, "want", r.source)
		return nil
	}

	switch event.DetailType {
	case "order.created":
		var d OrderCreated
		if err := json.Unmarshal(event.Detail, &d); err != nil {
			return fmt.Errorf("%w: order.created: %w", ErrMalformedDetail, err)
		}
		return r.proc.OnOrderCreated(ctx, d)

	case "order.shipped":
		var d OrderShipped
		if err := json.Unmarshal(event.Detail, &d); err != nil {
			return fmt.Errorf("%w: order.shipped: %w", ErrMalformedDetail, err)
		}
		return r.proc.OnOrderShipped(ctx, d)

	case "order.cancelled":
		var d OrderCancelled
		if err := json.Unmarshal(event.Detail, &d); err != nil {
			return fmt.Errorf("%w: order.cancelled: %w", ErrMalformedDetail, err)
		}
		return r.proc.OnOrderCancelled(ctx, d)

	default:
		r.log.InfoContext(ctx, "ignoring unknown detail-type", "detail_type", event.DetailType)
		return nil
	}
}
```

`Handle` is registered with `lambda.Start(r.Handle)` in the real binary.

### Exercise 3: Tests

Create `router_test.go`. Tests build `events.CloudWatchEvent` values directly; no network or AWS infrastructure is needed:

```go
package orderrouter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func newTestRouter(t *testing.T, proc Processor) *Router {
	t.Helper()
	r, err := New(ExpectedSource, proc, slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func makeEvent(source, detailType string, detail json.RawMessage) events.CloudWatchEvent {
	return events.CloudWatchEvent{
		Version:    "0",
		Source:     source,
		DetailType: detailType,
		Detail:     detail,
	}
}

// TestHandleOrderCreated verifies routing and payload extraction for order.created.
func TestHandleOrderCreated(t *testing.T) {
	t.Parallel()

	var got OrderCreated
	proc := Processor{
		OnOrderCreated: func(_ context.Context, e OrderCreated) error {
			got = e
			return nil
		},
		OnOrderShipped:   func(_ context.Context, _ OrderShipped) error { return nil },
		OnOrderCancelled: func(_ context.Context, _ OrderCancelled) error { return nil },
	}
	r := newTestRouter(t, proc)

	want := OrderCreated{OrderID: "ORD-001", Customer: "alice", Items: []string{"widget", "gadget"}}
	ev := makeEvent(ExpectedSource, "order.created", mustMarshal(t, want))

	if err := r.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.OrderID != want.OrderID || got.Customer != want.Customer {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if len(got.Items) != 2 || got.Items[0] != "widget" {
		t.Fatalf("items = %v, want [widget gadget]", got.Items)
	}
}

// TestHandleOrderShipped verifies routing and payload extraction for order.shipped.
func TestHandleOrderShipped(t *testing.T) {
	t.Parallel()

	var got OrderShipped
	proc := Processor{
		OnOrderCreated:   func(_ context.Context, _ OrderCreated) error { return nil },
		OnOrderShipped:   func(_ context.Context, e OrderShipped) error { got = e; return nil },
		OnOrderCancelled: func(_ context.Context, _ OrderCancelled) error { return nil },
	}
	r := newTestRouter(t, proc)

	want := OrderShipped{OrderID: "ORD-001", TrackingNumber: "1Z999AA10123456784"}
	ev := makeEvent(ExpectedSource, "order.shipped", mustMarshal(t, want))

	if err := r.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.TrackingNumber != want.TrackingNumber {
		t.Fatalf("tracking = %q, want %q", got.TrackingNumber, want.TrackingNumber)
	}
}

// TestHandleOrderCancelled verifies routing and payload extraction for order.cancelled.
func TestHandleOrderCancelled(t *testing.T) {
	t.Parallel()

	var got OrderCancelled
	proc := Processor{
		OnOrderCreated:   func(_ context.Context, _ OrderCreated) error { return nil },
		OnOrderShipped:   func(_ context.Context, _ OrderShipped) error { return nil },
		OnOrderCancelled: func(_ context.Context, e OrderCancelled) error { got = e; return nil },
	}
	r := newTestRouter(t, proc)

	want := OrderCancelled{OrderID: "ORD-002", Reason: "customer request"}
	ev := makeEvent(ExpectedSource, "order.cancelled", mustMarshal(t, want))

	if err := r.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.Reason != want.Reason {
		t.Fatalf("reason = %q, want %q", got.Reason, want.Reason)
	}
}

// TestHandleWrongSourceIsIgnored checks that events from other sources are silently dropped.
func TestHandleWrongSourceIsIgnored(t *testing.T) {
	t.Parallel()

	called := false
	proc := Processor{
		OnOrderCreated:   func(_ context.Context, _ OrderCreated) error { called = true; return nil },
		OnOrderShipped:   func(_ context.Context, _ OrderShipped) error { called = true; return nil },
		OnOrderCancelled: func(_ context.Context, _ OrderCancelled) error { called = true; return nil },
	}
	r := newTestRouter(t, proc)

	ev := makeEvent("payments.service", "order.created", mustMarshal(t, OrderCreated{}))
	if err := r.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle should return nil for wrong source, got %v", err)
	}
	if called {
		t.Fatal("processor should not be called for wrong source")
	}
}

// TestHandleUnknownDetailType checks that unknown types are logged but not errored.
func TestHandleUnknownDetailType(t *testing.T) {
	t.Parallel()

	called := false
	proc := Processor{
		OnOrderCreated:   func(_ context.Context, _ OrderCreated) error { called = true; return nil },
		OnOrderShipped:   func(_ context.Context, _ OrderShipped) error { called = true; return nil },
		OnOrderCancelled: func(_ context.Context, _ OrderCancelled) error { called = true; return nil },
	}
	r := newTestRouter(t, proc)

	ev := makeEvent(ExpectedSource, "order.refunded", json.RawMessage(`{"order_id":"ORD-003"}`))
	if err := r.Handle(context.Background(), ev); err != nil {
		t.Fatalf("Handle should return nil for unknown detail-type, got %v", err)
	}
	if called {
		t.Fatal("processor should not be called for unknown detail-type")
	}
}

// TestHandleMalformedDetailReturnsError verifies ErrMalformedDetail wrapping.
func TestHandleMalformedDetailReturnsError(t *testing.T) {
	t.Parallel()

	proc := Processor{
		OnOrderCreated:   func(_ context.Context, _ OrderCreated) error { return nil },
		OnOrderShipped:   func(_ context.Context, _ OrderShipped) error { return nil },
		OnOrderCancelled: func(_ context.Context, _ OrderCancelled) error { return nil },
	}
	r := newTestRouter(t, proc)

	ev := makeEvent(ExpectedSource, "order.created", json.RawMessage(`not valid json`))
	err := r.Handle(context.Background(), ev)
	if !errors.Is(err, ErrMalformedDetail) {
		t.Fatalf("err = %v, want errors.Is(err, ErrMalformedDetail)", err)
	}
}

// TestNewRejectsEmptySource ensures the constructor validates its arguments.
func TestNewRejectsEmptySource(t *testing.T) {
	t.Parallel()

	proc := Processor{
		OnOrderCreated:   func(_ context.Context, _ OrderCreated) error { return nil },
		OnOrderShipped:   func(_ context.Context, _ OrderShipped) error { return nil },
		OnOrderCancelled: func(_ context.Context, _ OrderCancelled) error { return nil },
	}
	if _, err := New("", proc, nil); err == nil {
		t.Fatal("New with empty source should return an error")
	}
}

// TestNewRejectsNilProcessors ensures nil processor fields are rejected.
func TestNewRejectsNilProcessors(t *testing.T) {
	t.Parallel()

	proc := Processor{
		OnOrderCreated: func(_ context.Context, _ OrderCreated) error { return nil },
		// OnOrderShipped and OnOrderCancelled are nil
	}
	if _, err := New(ExpectedSource, proc, nil); err == nil {
		t.Fatal("New with nil processor fields should return an error")
	}
}

// ExampleRouter_Handle demonstrates constructing a router and processing a single event.
func ExampleRouter_Handle() {
	proc := Processor{
		OnOrderCreated: func(_ context.Context, e OrderCreated) error {
			fmt.Printf("created: %s for %s\n", e.OrderID, e.Customer)
			return nil
		},
		OnOrderShipped: func(_ context.Context, e OrderShipped) error {
			fmt.Printf("shipped: %s tracking %s\n", e.OrderID, e.TrackingNumber)
			return nil
		},
		OnOrderCancelled: func(_ context.Context, e OrderCancelled) error {
			fmt.Printf("cancelled: %s reason: %s\n", e.OrderID, e.Reason)
			return nil
		},
	}
	r, _ := New(ExpectedSource, proc, slog.New(slog.NewTextHandler(io.Discard, nil)))

	detail, _ := json.Marshal(OrderCreated{OrderID: "ORD-042", Customer: "bob", Items: []string{"book"}})
	ev := makeEvent(ExpectedSource, "order.created", detail)
	_ = r.Handle(context.Background(), ev)
	// Output: created: ORD-042 for bob
}
```

Note: `ExampleRouter_Handle` references `makeEvent` and `io.Discard` and `fmt` — add `"fmt"` and `"io"` to the import block of `router_test.go`.

Your turn: add `TestHandleOrderCreatedCallsProcessorOnce` that uses a counter to assert the `OnOrderCreated` processor is called exactly once per event, not zero times and not more than once.

### Exercise 4: Demo Binary

Create `cmd/demo/main.go`. This touches only exported API:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/events"

	"example.com/orderrouter"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	proc := orderrouter.Processor{
		OnOrderCreated: func(ctx context.Context, e orderrouter.OrderCreated) error {
			fmt.Printf("[created]   order=%s customer=%s items=%v\n", e.OrderID, e.Customer, e.Items)
			return nil
		},
		OnOrderShipped: func(ctx context.Context, e orderrouter.OrderShipped) error {
			fmt.Printf("[shipped]   order=%s tracking=%s\n", e.OrderID, e.TrackingNumber)
			return nil
		},
		OnOrderCancelled: func(ctx context.Context, e orderrouter.OrderCancelled) error {
			fmt.Printf("[cancelled] order=%s reason=%s\n", e.OrderID, e.Reason)
			return nil
		},
	}

	r, err := orderrouter.New(orderrouter.ExpectedSource, proc, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init error: %v\n", err)
		os.Exit(1)
	}

	samples := []struct {
		source     string
		detailType string
		detail     any
	}{
		{
			orderrouter.ExpectedSource,
			"order.created",
			orderrouter.OrderCreated{OrderID: "ORD-001", Customer: "alice", Items: []string{"widget", "gadget"}},
		},
		{
			orderrouter.ExpectedSource,
			"order.shipped",
			orderrouter.OrderShipped{OrderID: "ORD-001", TrackingNumber: "1Z999AA10123456784"},
		},
		{
			orderrouter.ExpectedSource,
			"order.cancelled",
			orderrouter.OrderCancelled{OrderID: "ORD-002", Reason: "customer request"},
		},
		{
			"payments.service", // wrong source — should be ignored
			"order.created",
			orderrouter.OrderCreated{OrderID: "ORD-003", Customer: "eve"},
		},
		{
			orderrouter.ExpectedSource,
			"order.refunded", // unknown detail-type — should be ignored
			map[string]string{"order_id": "ORD-004"},
		},
	}

	ctx := context.Background()
	for _, s := range samples {
		raw, _ := json.Marshal(s.detail)
		ev := events.CloudWatchEvent{
			Version:    "0",
			Source:     s.source,
			DetailType: s.detailType,
			Detail:     raw,
		}
		if err := r.Handle(ctx, ev); err != nil {
			fmt.Fprintf(os.Stderr, "handle error: %v\n", err)
		}
	}
}
```

Run with:

```bash
go run ./cmd/demo
```

Expected output (the wrong-source and unknown-detail-type events produce no output lines; the slog lines go to stderr):

```
[created]   order=ORD-001 customer=alice items=[widget gadget]
[shipped]   order=ORD-001 tracking=1Z999AA10123456784
[cancelled] order=ORD-002 reason=customer request
```

## Common Mistakes

### Returning an Error for Wrong Source or Unknown Detail-Type

Wrong: returning a non-nil error when `event.Source` does not match or when `event.DetailType` is unrecognized.

What happens: Lambda treats any non-nil error as a transient failure and retries the event. After exhausting retries the event lands in the dead-letter queue or is silently dropped, depending on configuration. This wastes retries and fills DLQs with unprocessable events.

Fix: log the ignored event at `Info` level and return `nil`. Retrying is only correct for transient failures (network timeouts, downstream errors); an event with the wrong source or an unknown type is not transient.

### Unmarshaling Detail Before Validating Source

Wrong: calling `json.Unmarshal(event.Detail, &d)` before checking `event.Source`.

What happens: you do unnecessary work and potentially process crafted events from an unexpected producer that smuggled in a valid payload structure.

Fix: validate `event.Source` first. Only unmarshal `event.Detail` after the source check passes.

### Using `map[string]interface{}` Instead of `json.RawMessage`

Wrong: defining `Detail map[string]interface{}` in a custom CloudWatchEvent replica.

What happens: all numeric values are decoded as `float64`; integers lose precision for large values; you lose the ability to re-marshal without drift.

Fix: use the library-provided `events.CloudWatchEvent` which has `Detail json.RawMessage`. Then unmarshal into a concrete, typed struct per `DetailType`.

### Nil-Checking the Processor at Call Time, Not Construction Time

Wrong: checking `if r.proc.OnOrderCreated == nil` inside `Handle`.

What happens: the check fires on every event invocation; a nil function causes a panic at the worst possible moment (during a production event burst).

Fix: validate that all `Processor` fields are non-nil in `New` and return an error from the constructor. `Handle` then calls without checking.

## Verification

From `~/go-exercises/orderrouter`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass with no output from the first command. Confirm the demo runs cleanly:

```bash
go run ./cmd/demo
```

The output must show exactly three lines (`[created]`, `[shipped]`, `[cancelled]`). The wrong-source and unknown-detail-type events produce no output lines.

## Summary

- EventBridge events arrive as `events.CloudWatchEvent`; `Detail` is `json.RawMessage`, deferred until the `DetailType` is known.
- Validate `event.Source` before doing any work; return `nil` (not an error) when the source is wrong — retrying is not the right response.
- Use a `switch` on `event.DetailType` and unmarshal `event.Detail` into a concrete struct per case; wrap unmarshal errors with `%w` so callers can use `errors.Is(err, ErrMalformedDetail)`.
- Unknown `DetailType` values are logged and dropped with `nil`; they are not errors.
- Inject processors via a struct of functions so the handler is fully testable without deploying to AWS.
- The Lambda entry point is one line: `lambda.Start(r.Handle)`.

## What's Next

Next: [S3 Event Processing](../05-s3-event-processing/05-s3-event-processing.md).

## Resources

- [events.CloudWatchEvent — pkg.go.dev](https://pkg.go.dev/github.com/aws/aws-lambda-go/events#CloudWatchEvent)
- [EventBridge event structure — AWS docs](https://docs.aws.amazon.com/eventbridge/latest/userguide/eb-events-structure.html)
- [Lambda with EventBridge — AWS docs](https://docs.aws.amazon.com/lambda/latest/dg/services-cloudwatchevents.html)
- [encoding/json — json.RawMessage](https://pkg.go.dev/encoding/json#RawMessage)
- [aws-lambda-go repository](https://github.com/aws/aws-lambda-go)
