# Exercise 1: Compose a Watermill Router Middleware Pipeline

You will build an event-processing service on `message.NewRouter` over an
in-memory GoChannel pub/sub: a handler consumes an input topic, validates and
enriches a JSON order event, and republishes it to an output topic. The point of
the exercise is the middleware stack — `CorrelationID`, `Retry`, `Recoverer`,
`Timeout` — composed in a deliberate order, and observing correlation-id
propagation and retry-then-succeed behavior.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pipeline/                    independent module: example.com/pipeline
  go.mod                     go 1.26; requires watermill v1.5.2
  pipeline.go                OrderEvent, ProcessedOrder, EnrichHandler, BuildRouter
  cmd/
    demo/
      main.go                runs the router over gochannel, publishes one order
  pipeline_test.go           happy-path enrichment + correlation; retry-then-succeed
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `EnrichHandler` (validate + enrich a JSON order, sentinel-wrapped errors) and `BuildRouter` (wires the handler with a `CorrelationID, Retry, Recoverer, Timeout` middleware stack over GoChannel).
- Test: assert the enriched payload lands on the output topic with the correlation id copied over; a second router with a flaky handler that fails twice then succeeds fires the retry hook once (the hook skips the first failed attempt).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get github.com/ThreeDotsLabs/watermill@v1.5.2
```

### The handler is a pure function of a message

`EnrichHandler` has the exact shape the router expects,
`func(*message.Message) ([]*message.Message, error)`. It unmarshals the payload,
rejects an invalid order by returning an error wrapped around the package sentinel
`ErrInvalidOrder` (so callers and tests assert with `errors.Is`), and otherwise
builds a `ProcessedOrder` — the same fields plus a `status` and a `processed_at`
timestamp — marshals it, and returns it as a single produced message. The handler
does *not* set a topic on the produced message: the router publishes it to
whatever `publishTopic` was given to `AddHandler`. Returning `nil, err` nacks the
input; returning `messages, nil` acks it and publishes the messages.

### The middleware stack, ordered on purpose

`BuildRouter` composes four middlewares with `AddMiddleware`, and the order is the
whole lesson. The list is applied outermost-first, so the effective wrapping is
`CorrelationID(Retry(Recoverer(Timeout(handler))))`:

- `CorrelationID` is outermost so the incoming correlation id is attached before
  anything inner logs or produces, and is copied onto the produced message.
- `Retry` wraps `Recoverer` so that a panic, once `Recoverer` has converted it to
  an error, is visible to the retry loop and gets retried like any other error.
  Flip these two and a panic unwinds past the loop, caught but never retried.
- `Recoverer` wraps `Timeout` and the handler, catching panics per attempt.
- `Timeout` is innermost so each individual attempt gets its own deadline instead
  of the whole retry sequence sharing one.

`Retry` carries an `OnRetryHook func(retryNum int, delay time.Duration)`, which
fires once per retry attempt. The tests pass a counter into it to prove
deterministically how many retries happened.

Create `pipeline.go`:

```go
package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
)

// Topics the pipeline consumes from and produces to.
const (
	InputTopic  = "orders.incoming"
	OutputTopic = "orders.processed"
)

// ErrInvalidOrder is the sentinel wrapped by every validation failure so callers
// can classify it with errors.Is.
var ErrInvalidOrder = errors.New("invalid order")

// OrderEvent is the inbound event shape.
type OrderEvent struct {
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
}

// ProcessedOrder is the enriched event the pipeline produces.
type ProcessedOrder struct {
	OrderID     string    `json:"order_id"`
	AmountCents int64     `json:"amount_cents"`
	Status      string    `json:"status"`
	ProcessedAt time.Time `json:"processed_at"`
}

// EnrichHandler validates an inbound order and republishes an enriched version.
// It is a message.HandlerFunc: returning (messages, nil) acks the input and
// publishes the messages; returning (nil, err) nacks it.
func EnrichHandler(msg *message.Message) ([]*message.Message, error) {
	var in OrderEvent
	if err := json.Unmarshal(msg.Payload, &in); err != nil {
		return nil, fmt.Errorf("%w: bad json: %v", ErrInvalidOrder, err)
	}
	if in.OrderID == "" {
		return nil, fmt.Errorf("%w: empty order_id", ErrInvalidOrder)
	}
	if in.AmountCents <= 0 {
		return nil, fmt.Errorf("%w: non-positive amount_cents=%d", ErrInvalidOrder, in.AmountCents)
	}

	out := ProcessedOrder{
		OrderID:     in.OrderID,
		AmountCents: in.AmountCents,
		Status:      "processed",
		ProcessedAt: time.Now().UTC(),
	}
	payload, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshal processed order: %w", err)
	}

	produced := message.NewMessage(watermill.NewUUID(), payload)
	return []*message.Message{produced}, nil
}

// BuildRouter wires EnrichHandler onto a router over the given GoChannel pub/sub
// with the standard middleware stack. onRetry, if non-nil, is invoked once per
// retry attempt.
func BuildRouter(
	pubSub *gochannel.GoChannel,
	logger watermill.LoggerAdapter,
	onRetry func(retryNum int, delay time.Duration),
) (*message.Router, error) {
	router, err := message.NewRouter(message.RouterConfig{CloseTimeout: 5 * time.Second}, logger)
	if err != nil {
		return nil, fmt.Errorf("new router: %w", err)
	}

	router.AddMiddleware(
		// Outermost: attach/propagate the correlation id first.
		middleware.CorrelationID,
		// Retry wraps Recoverer so recovered panics are retryable errors.
		middleware.Retry{
			MaxRetries:      3,
			InitialInterval: 10 * time.Millisecond,
			Multiplier:      2,
			MaxInterval:     time.Second,
			OnRetryHook:     onRetry,
			Logger:          logger,
		}.Middleware,
		// Recoverer turns a handler panic into an error.
		middleware.Recoverer,
		// Innermost: each attempt gets its own deadline.
		middleware.Timeout(5*time.Second),
	)

	router.AddHandler("enrich-orders", InputTopic, pubSub, OutputTopic, pubSub, EnrichHandler)
	return router, nil
}
```

### The runnable demo

The demo starts the router over a GoChannel, subscribes to the output topic,
publishes one order carrying a correlation id, prints the enriched result and the
propagated id, then cancels the context so `Run` drains and returns cleanly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"

	"example.com/pipeline"
)

func main() {
	logger := watermill.NopLogger{}
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)

	router, err := pipeline.BuildRouter(pubSub, logger, nil)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- router.Run(ctx) }()
	<-router.Running()

	out, err := pubSub.Subscribe(ctx, pipeline.OutputTopic)
	if err != nil {
		log.Fatal(err)
	}

	payload, _ := json.Marshal(pipeline.OrderEvent{OrderID: "ord-42", AmountCents: 2500})
	in := message.NewMessage(watermill.NewUUID(), payload)
	middleware.SetCorrelationID("trace-abc", in)
	if err := pubSub.Publish(pipeline.InputTopic, in); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("published order ord-42 correlation=%s\n", middleware.MessageCorrelationID(in))

	got := <-out
	got.Ack()
	var processed pipeline.ProcessedOrder
	if err := json.Unmarshal(got.Payload, &processed); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("processed order %s status=%s correlation=%s has_processed_at=%t\n",
		processed.OrderID, processed.Status,
		middleware.MessageCorrelationID(got), !processed.ProcessedAt.IsZero())

	cancel()
	if err := <-runErr; err != nil {
		log.Fatal(err)
	}
	_ = pubSub.Close()
	fmt.Println("shutdown: router stopped cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
published order ord-42 correlation=trace-abc
processed order ord-42 status=processed correlation=trace-abc has_processed_at=true
shutdown: router stopped cleanly
```

### Tests

The happy-path test starts the router, subscribes to the output topic, publishes
an order with a known correlation id, and asserts the enriched payload plus that
`middleware.MessageCorrelationID` was copied onto the produced message. The
retry test wires a second router with a handler that fails twice and then
succeeds. `OnRetryHook` carries an internal `retryNum > 0` guard that skips the
failed attempt which triggers the very first retry, so two failures before success
fire the hook exactly once — deterministic because a small `InitialInterval` and a
success on the third attempt bound the loop. The validation table exercises the
sentinel-wrapped errors directly against `EnrichHandler`, no router required.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/message/router/middleware"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
)

func TestEnrichHandlerValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"valid", `{"order_id":"ord-1","amount_cents":100}`, false},
		{"bad json", `{`, true},
		{"empty id", `{"order_id":"","amount_cents":100}`, true},
		{"non-positive amount", `{"order_id":"ord-1","amount_cents":0}`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg := message.NewMessage("uuid", []byte(tc.payload))
			out, err := EnrichHandler(msg)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidOrder) {
					t.Fatalf("err = %v; want wrapped ErrInvalidOrder", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("produced %d messages; want 1", len(out))
			}
		})
	}
}

func TestPipelineEnrichesAndPropagatesCorrelation(t *testing.T) {
	t.Parallel()
	logger := watermill.NopLogger{}
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)

	router, err := BuildRouter(pubSub, logger, nil)
	if err != nil {
		t.Fatalf("BuildRouter: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- router.Run(ctx) }()
	<-router.Running()

	out, err := pubSub.Subscribe(ctx, OutputTopic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	payload, _ := json.Marshal(OrderEvent{OrderID: "ord-9", AmountCents: 4200})
	in := message.NewMessage(watermill.NewUUID(), payload)
	middleware.SetCorrelationID("trace-xyz", in)
	if err := pubSub.Publish(InputTopic, in); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case got := <-out:
		got.Ack()
		if id := middleware.MessageCorrelationID(got); id != "trace-xyz" {
			t.Fatalf("correlation id = %q; want trace-xyz", id)
		}
		var processed ProcessedOrder
		if err := json.Unmarshal(got.Payload, &processed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if processed.OrderID != "ord-9" || processed.Status != "processed" {
			t.Fatalf("processed = %+v; want ord-9/processed", processed)
		}
		if processed.ProcessedAt.IsZero() {
			t.Fatal("processed_at not set")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for processed order")
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRetryThenSucceed(t *testing.T) {
	t.Parallel()
	logger := watermill.NopLogger{}
	pubSub := gochannel.NewGoChannel(gochannel.Config{}, logger)

	router, err := message.NewRouter(message.RouterConfig{CloseTimeout: time.Second}, logger)
	if err != nil {
		t.Fatalf("new router: %v", err)
	}

	var retries atomic.Int64
	router.AddMiddleware(
		middleware.Retry{
			MaxRetries:      5,
			InitialInterval: time.Millisecond,
			OnRetryHook:     func(retryNum int, delay time.Duration) { retries.Add(1) },
			Logger:          logger,
		}.Middleware,
		middleware.Recoverer,
	)

	var attempts atomic.Int64
	done := make(chan struct{})
	router.AddNoPublisherHandler("flaky", "flaky.in", pubSub, func(msg *message.Message) error {
		if attempts.Add(1) < 3 {
			return errors.New("transient failure")
		}
		close(done)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- router.Run(ctx) }()
	<-router.Running()

	if err := pubSub.Publish("flaky.in", message.NewMessage(watermill.NewUUID(), []byte("x"))); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never succeeded")
	}

	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d; want 3", got)
	}
	// OnRetryHook fires on each failed attempt after the first (its internal
	// retryNum > 0 guard skips the failure that triggers the very first retry),
	// so two failures before success produce exactly one hook call.
	if got := retries.Load(); got != 1 {
		t.Fatalf("retries = %d; want 1 (hook skips the first failed attempt)", got)
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func ExampleEnrichHandler() {
	payload, _ := json.Marshal(OrderEvent{OrderID: "ord-7", AmountCents: 1999})
	msg := message.NewMessage("uuid-1", payload)

	produced, err := EnrichHandler(msg)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	var out ProcessedOrder
	_ = json.Unmarshal(produced[0].Payload, &out)
	fmt.Printf("%s %s %d\n", out.OrderID, out.Status, out.AmountCents)
	// Output: ord-7 processed 1999
}
```

## Review

The pipeline is correct when the enriched message reaches the output topic with
the same correlation id it entered with, and when the retry count is a pure
function of how many times the handler failed. If the correlation id arrives
empty, `CorrelationID` was not the outermost middleware or you never set the id on
the input. If the retry hook fires zero times against a failing handler, check the
ordering: `Retry` must wrap `Recoverer`, not the other way around, or a returned
error will be swallowed before the loop sees it. The most common structural
mistake is expecting a `HandlerFunc` to publish by side effect — it publishes only
what it returns, and returning `nil, nil` acks the input while producing nothing.
Run `go test -race` to confirm the concurrent router goroutine and the test's
subscriber do not race on shared state; the atomic counters in the retry test are
there precisely because the hook runs on the router's goroutine.

## Resources

- [Watermill — Message Router](https://watermill.io/docs/messages-router/) — router, handlers, and how middleware wraps a handler.
- [Watermill — Middlewares](https://watermill.io/docs/middlewares/) — the built-in `CorrelationID`, `Retry`, `Recoverer`, `Timeout` and their configuration.
- [pkg.go.dev — router/middleware](https://pkg.go.dev/github.com/ThreeDotsLabs/watermill/message/router/middleware) — verified `Retry`, `CorrelationID`, `MessageCorrelationID` signatures.
- [pkg.go.dev — pubsub/gochannel](https://pkg.go.dev/github.com/ThreeDotsLabs/watermill/pubsub/gochannel) — `NewGoChannel`, `Config`, `Publish`, `Subscribe`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-poison-queue-and-retry-topology.md](02-poison-queue-and-retry-topology.md)
