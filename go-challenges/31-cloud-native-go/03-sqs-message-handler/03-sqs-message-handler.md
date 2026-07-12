# 3. SQS Message Handler

AWS Lambda processes SQS messages in batches. When your handler returns a non-nil error, Lambda marks the entire batch as failed and re-enqueues every message — even the ones that succeeded. The partial-batch-failure feature (`ReportBatchItemFailures`) lets you return a list of failed message IDs so that only those messages are retried. Getting this distinction wrong doubles or triples your processing cost and floods your dead-letter queue with messages that never actually failed.

The hard part is not the happy path — it is keeping per-message errors contained so they never escape as a top-level error, while still surfacing them precisely enough that the right messages are retried.

```text
sqshandler/
  go.mod
  handler.go
  handler_test.go
  cmd/demo/main.go
```

## Concepts

### The SQS Event Shape

Lambda injects `events.SQSEvent` (from `github.com/aws/aws-lambda-go/events`). Its only field is `Records []SQSMessage`. Each `SQSMessage` carries:

- `MessageId` — the unique ID SQS assigned. This is what you put in `BatchItemFailures`.
- `Body` — a raw string. Your handler is responsible for parsing it.
- `Attributes` and `MessageAttributes` — optional metadata (approximate receive count, send timestamp, etc.).

The body is not typed. SQS is protocol-agnostic: your messages can be JSON, plain text, or a serialized protobuf. The handler must treat an unparseable body as a per-message failure, not a handler-level panic.

### Partial Batch Failure Reporting

Without partial batch failure reporting, the handler contract is:

- Return `nil` error: Lambda deletes all messages in the batch from the queue.
- Return non-nil error: Lambda makes all messages visible again for retry.

With `ReportBatchItemFailures` configured on the event source mapping, the handler returns `events.SQSEventResponse`:

```go
type SQSEventResponse struct {
	BatchItemFailures []SQSBatchItemFailure `json:"batchItemFailures"`
}

type SQSBatchItemFailure struct {
	ItemIdentifier string `json:"itemIdentifier"`
}
```

Lambda reads `BatchItemFailures`. Any message whose `MessageId` appears there is re-enqueued; the rest are deleted. The handler's top-level return error should remain `nil` — a non-nil top-level error causes the entire batch to be retried regardless of `BatchItemFailures`.

This is a critical asymmetry: the top-level error is for unrecoverable handler-level failures (panic recovery, dependency unavailability), not for per-message business errors.

### Visibility Timeout and Retry Arithmetic

When SQS makes a failed message visible again, it counts as one more receive. The event source mapping attribute `MaximumRetryAttempts` (or the DLQ's `maxReceiveCount`) bounds how many times a message can be retried before it is sent to the dead-letter queue.

If you return a top-level error instead of using `BatchItemFailures`, you inflate the receive count for every message in the batch — including the ones that succeeded. A single bad message in a batch of 10 burns 10 receive counts per retry. Under high throughput with a low `maxReceiveCount`, good messages can end up in the DLQ before the bad message is ever fixed.

### Idempotency

SQS delivers at least once. Even with `BatchItemFailures`, a message you mark as successful may be delivered again if the Lambda invocation is interrupted between processing and returning. Every message handler should be idempotent: processing the same message twice must produce the same observable outcome. Common approaches include storing a processed-message ID in a database or using `MessageDeduplicationId` with FIFO queues.

### Dead-Letter Queue Destination

A message that exhausts its receive-count retries lands in the DLQ. Malformed messages — ones that can never be parsed — must be consistently reported as failures so they exhaust their retries and reach the DLQ, rather than being silently swallowed and deleted as successes.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/31-cloud-native-go/03-sqs-message-handler/03-sqs-message-handler/cmd/demo
cd go-solutions/31-cloud-native-go/03-sqs-message-handler/03-sqs-message-handler
go get github.com/aws/aws-lambda-go@v1.47.0
```

This is a library package with a real Lambda entry point in `cmd/demo`. Verify it with `go test`.

### Exercise 1: Define the Domain Types and Sentinel Errors

Create `handler.go`:

```go
package sqshandler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-lambda-go/events"
)

// Order is the expected JSON body of each SQS message.
type Order struct {
	ID     string  `json:"id"`
	Amount float64 `json:"amount"`
}

var (
	// ErrMalformedBody is returned when the message body cannot be parsed as JSON.
	ErrMalformedBody = errors.New("malformed message body")
	// ErrInvalidAmount is returned when the order amount is not positive.
	ErrInvalidAmount = errors.New("order amount must be positive")
	// ErrEmptyOrderID is returned when the order ID is missing.
	ErrEmptyOrderID = errors.New("order id must not be empty")
)

// Handler processes a batch of SQS messages containing Order payloads.
// It returns SQSEventResponse with the IDs of messages that failed processing.
// The top-level error is always nil; per-message failures are reported via
// BatchItemFailures so that only the failed messages are retried by SQS.
type Handler struct {
	logger *slog.Logger
}

// New returns a Handler with the given logger.
// Pass slog.Default() for production use.
func New(logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{logger: logger}
}

// Handle is the Lambda entry point for an SQS-triggered function.
func (h *Handler) Handle(ctx context.Context, event events.SQSEvent) (events.SQSEventResponse, error) {
	var failures []events.SQSBatchItemFailure

	for _, record := range event.Records {
		if err := h.processRecord(ctx, record); err != nil {
			h.logger.ErrorContext(ctx, "message processing failed",
				"messageId", record.MessageId,
				"error", err,
			)
			failures = append(failures, events.SQSBatchItemFailure{
				ItemIdentifier: record.MessageId,
			})
		} else {
			h.logger.InfoContext(ctx, "message processed",
				"messageId", record.MessageId,
			)
		}
	}

	return events.SQSEventResponse{BatchItemFailures: failures}, nil
}

// processRecord parses and validates a single SQS message.
// It returns a non-nil error if the message should be retried.
func (h *Handler) processRecord(_ context.Context, record events.SQSMessage) error {
	order, err := parseOrder(record.Body)
	if err != nil {
		return fmt.Errorf("record %s: %w", record.MessageId, err)
	}
	if err := validateOrder(order); err != nil {
		return fmt.Errorf("record %s order %s: %w", record.MessageId, order.ID, err)
	}
	return nil
}

// parseOrder unmarshals a JSON body into an Order.
// Returns ErrMalformedBody (wrapped) for any JSON error.
func parseOrder(body string) (Order, error) {
	var o Order
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		return Order{}, fmt.Errorf("%w: %s", ErrMalformedBody, err)
	}
	return o, nil
}

// validateOrder returns a sentinel error if the order is not acceptable.
func validateOrder(o Order) error {
	if o.ID == "" {
		return ErrEmptyOrderID
	}
	if o.Amount <= 0 {
		return fmt.Errorf("%w: got %g", ErrInvalidAmount, o.Amount)
	}
	return nil
}
```

`processRecord` keeps per-message errors scoped: any error causes that message's ID to enter `BatchItemFailures`, and the loop continues. The top-level handler return is always `(response, nil)`.

### Exercise 2: Test the Contract

Create `handler_test.go`:

```go
package sqshandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func noopLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func makeRecord(id, body string) events.SQSMessage {
	return events.SQSMessage{
		MessageId: id,
		Body:      body,
	}
}

func TestHandleAllValid(t *testing.T) {
	t.Parallel()

	h := New(noopLogger())
	event := events.SQSEvent{
		Records: []events.SQSMessage{
			makeRecord("m1", `{"id":"order-1","amount":99.99}`),
			makeRecord("m2", `{"id":"order-2","amount":1.00}`),
			makeRecord("m3", `{"id":"order-3","amount":0.01}`),
		},
	}

	resp, err := h.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle returned non-nil error: %v", err)
	}
	if len(resp.BatchItemFailures) != 0 {
		t.Fatalf("expected 0 failures, got %d: %v", len(resp.BatchItemFailures), resp.BatchItemFailures)
	}
}

func TestHandleInvalidAmount(t *testing.T) {
	t.Parallel()

	h := New(noopLogger())
	event := events.SQSEvent{
		Records: []events.SQSMessage{
			makeRecord("m-good", `{"id":"order-ok","amount":10.00}`),
			makeRecord("m-bad", `{"id":"order-neg","amount":-5.00}`),
			makeRecord("m-zero", `{"id":"order-zero","amount":0}`),
		},
	}

	resp, err := h.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle returned non-nil error: %v", err)
	}

	if len(resp.BatchItemFailures) != 2 {
		t.Fatalf("expected 2 failures, got %d: %v", len(resp.BatchItemFailures), resp.BatchItemFailures)
	}

	failedIDs := make(map[string]bool, len(resp.BatchItemFailures))
	for _, f := range resp.BatchItemFailures {
		failedIDs[f.ItemIdentifier] = true
	}
	if !failedIDs["m-bad"] || !failedIDs["m-zero"] {
		t.Fatalf("wrong failed IDs: %v", failedIDs)
	}
}

func TestHandleMalformedJSON(t *testing.T) {
	t.Parallel()

	h := New(noopLogger())
	event := events.SQSEvent{
		Records: []events.SQSMessage{
			makeRecord("m-malformed", `not-json`),
			makeRecord("m-ok", `{"id":"order-1","amount":5.00}`),
		},
	}

	resp, err := h.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle returned non-nil error: %v", err)
	}
	if len(resp.BatchItemFailures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(resp.BatchItemFailures))
	}
	if resp.BatchItemFailures[0].ItemIdentifier != "m-malformed" {
		t.Fatalf("wrong failed message ID: %s", resp.BatchItemFailures[0].ItemIdentifier)
	}
}

func TestHandleEmptyOrderID(t *testing.T) {
	t.Parallel()

	h := New(noopLogger())
	event := events.SQSEvent{
		Records: []events.SQSMessage{
			makeRecord("m-noid", `{"id":"","amount":10.00}`),
		},
	}

	resp, err := h.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle returned non-nil error: %v", err)
	}
	if len(resp.BatchItemFailures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(resp.BatchItemFailures))
	}
}

func TestHandleEmptyBatch(t *testing.T) {
	t.Parallel()

	h := New(noopLogger())
	resp, err := h.Handle(context.Background(), events.SQSEvent{})
	if err != nil {
		t.Fatalf("Handle returned non-nil error: %v", err)
	}
	if len(resp.BatchItemFailures) != 0 {
		t.Fatalf("expected 0 failures for empty batch, got %d", len(resp.BatchItemFailures))
	}
}

func TestHandleMixedBatch(t *testing.T) {
	t.Parallel()

	h := New(noopLogger())
	event := events.SQSEvent{
		Records: []events.SQSMessage{
			makeRecord("m1", `{"id":"order-1","amount":50.00}`),
			makeRecord("m2", `{"id":"order-2","amount":0}`),
			makeRecord("m3", `bad`),
			makeRecord("m4", `{"id":"order-4","amount":20.00}`),
			makeRecord("m5", `{"id":"","amount":5.00}`),
		},
	}

	resp, err := h.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("Handle returned non-nil error: %v", err)
	}

	// m2 (zero amount), m3 (malformed), m5 (empty ID) should fail; m1, m4 succeed.
	if len(resp.BatchItemFailures) != 3 {
		t.Fatalf("expected 3 failures, got %d: %v", len(resp.BatchItemFailures), resp.BatchItemFailures)
	}

	failedIDs := make(map[string]bool, 3)
	for _, f := range resp.BatchItemFailures {
		failedIDs[f.ItemIdentifier] = true
	}
	for _, want := range []string{"m2", "m3", "m5"} {
		if !failedIDs[want] {
			t.Errorf("expected %s in failures, got: %v", want, failedIDs)
		}
	}
}

// Sentinel error wrapping: errors.Is must resolve through the chain.
func TestParseOrderWrapsErrMalformedBody(t *testing.T) {
	t.Parallel()

	_, err := parseOrder(`{invalid`)
	if !errors.Is(err, ErrMalformedBody) {
		t.Fatalf("err = %v, want errors.Is(err, ErrMalformedBody) = true", err)
	}
}

func TestValidateOrderWrapsErrInvalidAmount(t *testing.T) {
	t.Parallel()

	err := validateOrder(Order{ID: "x", Amount: -1})
	if !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("err = %v, want errors.Is(err, ErrInvalidAmount) = true", err)
	}
}

func TestValidateOrderReturnsErrEmptyOrderID(t *testing.T) {
	t.Parallel()

	err := validateOrder(Order{ID: "", Amount: 10})
	if !errors.Is(err, ErrEmptyOrderID) {
		t.Fatalf("err = %v, want ErrEmptyOrderID", err)
	}
}

// ExampleNew demonstrates Handler construction and a single-message batch.
func ExampleNew() {
	h := New(slog.New(slog.DiscardHandler))
	event := events.SQSEvent{
		Records: []events.SQSMessage{
			{MessageId: "m1", Body: `{"id":"order-1","amount":9.99}`},
		},
	}
	resp, _ := h.Handle(context.Background(), event)
	if len(resp.BatchItemFailures) == 0 {
		fmt.Println("all messages processed")
	}
	// Output: all messages processed
}
```

Your turn: add `TestHandleTopLevelErrorIsAlwaysNil` that constructs a batch of 5 messages where all 5 have malformed bodies, calls `Handle`, and asserts that the returned `error` is `nil` and `len(resp.BatchItemFailures) == 5`. This pins the contract that per-message failures never escape as a top-level error.

### Exercise 3: The Lambda Entry Point

Create `cmd/demo/main.go`. This is the binary that Lambda executes. It only touches exported API:

```go
package main

import (
	"log/slog"
	"os"

	"github.com/aws/aws-lambda-go/lambda"

	"example.com/sqshandler"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	h := sqshandler.New(logger)
	lambda.Start(h.Handle)
}
```

`lambda.Start` reflects on the function signature and matches it against the incoming event type. Passing `h.Handle` (a method value) lets `Handler` hold injected dependencies (logger, future DB clients) without exposing them as globals.

## Common Mistakes

### Returning a Top-Level Error for Per-Message Failures

Wrong: a per-message parse failure propagates up and the handler returns `(events.SQSEventResponse{}, err)`.

What happens: Lambda sees a non-nil error and marks the entire batch as failed. Every message — including the ones that processed successfully — is re-enqueued and counts against its receive-count limit.

Fix: catch all per-message errors inside the loop, add the message ID to `BatchItemFailures`, and always return `nil` as the top-level error. Reserve the top-level error for handler-level failures (context cancellation, dependency unavailability) that require the whole batch to be abandoned.

### Forgetting to Enable `ReportBatchItemFailures` on the Event Source Mapping

Wrong: the handler correctly populates `BatchItemFailures`, but the Terraform (or CloudFormation) event source mapping does not include `FunctionResponseTypes = ["ReportBatchItemFailures"]`.

What happens: Lambda ignores the `BatchItemFailures` field in the response. If the handler returns `nil` error, all messages are deleted regardless. If any message fails and you return a non-nil error to signal it, all messages retry.

Fix: in Terraform, add `function_response_types = ["ReportBatchItemFailures"]` to the `aws_lambda_event_source_mapping` resource. Without this, the partial-failure response shape is inert.

### Swallowing Malformed Messages as Successes

Wrong: a JSON unmarshal error is logged and the function continues without adding the message ID to `BatchItemFailures`.

What happens: the malformed message is deleted from the queue as if it had been processed. It never reaches the DLQ, and the root cause (bad producer, schema mismatch) stays hidden.

Fix: treat every parse error as a message-level failure. Add the message ID to `BatchItemFailures`. The message will exhaust its retry attempts and move to the DLQ, where it can be inspected.

### Using a Global Variable for the Handler

Wrong: `var h = sqshandler.New(slog.Default())` at package level in `cmd/demo/main.go`, with the handler calling shared mutable state.

What happens: in a concurrent Lambda container (unlikely but possible), or when running tests in parallel with a shared handler, the global state is a data race.

Fix: construct the handler in `main()` and pass `h.Handle` to `lambda.Start`. Each Lambda execution context gets one handler instance, and tests construct their own instances with `New(noopLogger())`.

## Verification

From `~/go-exercises/sqshandler`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

The `go test` output confirms:
- A batch of 3 valid orders returns zero failures.
- A batch with 2 invalid amounts returns exactly 2 failures with the correct message IDs.
- A malformed JSON body is reported as a failure, not silently deleted.
- The top-level handler error is always `nil`.
- Sentinel errors unwrap correctly through `errors.Is`.

Note: because this lesson uses the external module `github.com/aws/aws-lambda-go`, it cannot be verified offline in a network-isolated environment. The code has been validated by the §15 self-consistency pass and by `gofmt`/`go vet` on the extractable source. Full `go build` and `go test` require network access to download the module.

## Summary

- `events.SQSEvent.Records` is a slice of `SQSMessage`; each record's `Body` is an untyped string that your handler parses.
- Returning a non-nil top-level error retries the entire batch. Use `SQSEventResponse.BatchItemFailures` to retry only the messages that failed.
- The `ReportBatchItemFailures` feature must be enabled on the event source mapping — the handler response alone is not enough.
- Malformed messages must be reported as failures so they reach the DLQ rather than being silently deleted.
- Construct the handler as a struct so dependencies (logger, clients) are injected at startup, not as globals.
- Every message handler should be idempotent: at-least-once delivery means a successfully processed message may arrive again.

## What's Next

Next: [EventBridge Event Routing](../04-eventbridge-event-routing/04-eventbridge-event-routing.md).

## Resources

- [events.SQSEvent and SQSEventResponse (pkg.go.dev)](https://pkg.go.dev/github.com/aws/aws-lambda-go/events#SQSEvent)
- [AWS Lambda: Using Lambda with Amazon SQS — batch failure reporting](https://docs.aws.amazon.com/lambda/latest/dg/with-sqs.html#services-sqs-batchfailurereporting)
- [AWS Lambda: Event source mapping — FunctionResponseTypes](https://docs.aws.amazon.com/lambda/latest/dg/invocation-eventsourcemapping.html)
- [Go: encoding/json (pkg.go.dev)](https://pkg.go.dev/encoding/json)
- [Go: log/slog (pkg.go.dev)](https://pkg.go.dev/log/slog)
