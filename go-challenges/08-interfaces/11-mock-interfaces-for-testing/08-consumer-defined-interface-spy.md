# Exercise 8: Accept Interfaces at the Consumer — A Capturing Spy for an Event Publisher

Go's idiom is to define the interface where it is *used*, sized to exactly what
the caller needs. An `OrderService` that publishes domain events needs only one
method, so it declares its own one-method `EventPublisher` — not the broker's full
API. A capturing spy over that tiny port lets tests assert the exact event
published, and prove an idempotent command publishes at most once.

Fully self-contained: its own module, package, demo, and test.

## What you'll build

```text
orderevents/                 independent module: example.com/orderevents
  go.mod                     go 1.26
  order.go                   Event; EventPublisher (consumer-defined); OrderService
  cmd/
    demo/
      main.go                runnable demo with a stdout publisher
  order_test.go             capturing spy; event-content and exactly-once tests
```

- Files: `order.go`, `cmd/demo/main.go`, `order_test.go`.
- Implement: `OrderService.PlaceOrder(ctx, cmd)` that publishes one `OrderPlaced` event, guarded by an idempotency key so a replayed command does not republish.
- Test: a mutex-guarded spy capturing every published `Event`; assert one event with the right type and fields; a replay of the same command asserts exactly one `Publish`.
- Verify: `go test -count=1 -race ./...`

### The consumer defines the interface

A real message broker's client has a wide surface: publish, subscribe, ack,
transactions, headers, partitions. The `OrderService` uses exactly one thing —
publishing an event — so it declares an `EventPublisher` interface with a single
`Publish` method, right next to the code that consumes it. It does not import the
broker's interface; the broker's concrete client happens to satisfy this small
one. That is "accept interfaces, return structs, define the interface at the
consumer" in practice, and its direct payoff for testing is a three-line spy: a
fat, producer-defined interface would force the double to implement methods the
service never calls, dead weight that asserts nothing. Keeping the port minimal is
what makes the seam cheap to double.

### The exactly-once contract

`PlaceOrder` takes a command carrying an idempotency key — the standard defense
against a client retrying a request whose response it never received. The service
records processed keys; a second command with the same key returns success without
publishing again. That "publish at most once per key" property is the contract the
test must pin, and a capturing spy pins it directly: replay the command and assert
the spy recorded exactly one `Publish`. If `Publish` fails, the service rolls back
the recorded key so a genuine retry can republish — otherwise a transient broker
error would permanently swallow the event.

The spy captures each `Event` under a mutex and returns a defensive copy, so it is
correct even though `PlaceOrder` is safe for concurrent callers. The test asserts
both the *content* (one `OrderPlaced` with the right order id and amount) and the
*count* (exactly one across a replay).

Create `order.go`:

```go
package orderevents

import (
	"context"
	"fmt"
	"sync"
)

// Event is a domain event published by the service.
type Event struct {
	Type    string
	OrderID string
	Amount  int64
}

// EventPublisher is the outbound port, defined here at the consumer and exactly
// one method wide. A real broker client satisfies it without importing this.
type EventPublisher interface {
	Publish(ctx context.Context, e Event) error
}

// PlaceOrderCommand is the input, carrying an idempotency key.
type PlaceOrderCommand struct {
	IdempotencyKey string
	OrderID        string
	Amount         int64
}

// OrderService places orders and publishes exactly one event per idempotency key.
type OrderService struct {
	publisher EventPublisher
	mu        sync.Mutex
	seen      map[string]struct{}
}

func NewOrderService(p EventPublisher) *OrderService {
	return &OrderService{publisher: p, seen: make(map[string]struct{})}
}

// PlaceOrder publishes an OrderPlaced event unless this idempotency key was
// already processed. On a publish failure it rolls back the key so a retry works.
func (s *OrderService) PlaceOrder(ctx context.Context, cmd PlaceOrderCommand) error {
	s.mu.Lock()
	if _, done := s.seen[cmd.IdempotencyKey]; done {
		s.mu.Unlock()
		return nil // already processed; do not republish
	}
	s.seen[cmd.IdempotencyKey] = struct{}{}
	s.mu.Unlock()

	e := Event{Type: "OrderPlaced", OrderID: cmd.OrderID, Amount: cmd.Amount}
	if err := s.publisher.Publish(ctx, e); err != nil {
		s.mu.Lock()
		delete(s.seen, cmd.IdempotencyKey)
		s.mu.Unlock()
		return fmt.Errorf("publish order-placed %s: %w", cmd.OrderID, err)
	}
	return nil
}
```

### The runnable demo

The demo wires a stdout publisher and places the same order twice with one
idempotency key, showing the event is published exactly once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/orderevents"
)

// stdoutPublisher is a trivial real EventPublisher for the demo.
type stdoutPublisher struct{}

func (stdoutPublisher) Publish(_ context.Context, e orderevents.Event) error {
	fmt.Printf("published: %s order=%s amount=%d\n", e.Type, e.OrderID, e.Amount)
	return nil
}

func main() {
	svc := orderevents.NewOrderService(stdoutPublisher{})
	ctx := context.Background()
	cmd := orderevents.PlaceOrderCommand{IdempotencyKey: "key-1", OrderID: "order-42", Amount: 1500}

	_ = svc.PlaceOrder(ctx, cmd)
	_ = svc.PlaceOrder(ctx, cmd) // replay: must not publish again

	fmt.Println("done")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
published: OrderPlaced order=order-42 amount=1500
done
```

### Tests

`TestPublishesOrderPlaced` asserts the spy captured exactly one event equal to the
expected `OrderPlaced`. `TestIdempotentReplayPublishesOnce` replays the same
command and asserts the spy still holds one event. `TestRepublishAfterFailure`
proves a failed publish is retryable: a spy that fails once then succeeds ends with
one captured event after a retry.

Create `order_test.go`:

```go
package orderevents

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// spyPublisher captures every published event, concurrency-safe.
type spyPublisher struct {
	mu     sync.Mutex
	events []Event
	failN  int // fail the first failN publishes
}

func (s *spyPublisher) Publish(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failN > 0 {
		s.failN--
		return errors.New("broker unavailable")
	}
	s.events = append(s.events, e)
	return nil
}

func (s *spyPublisher) Captured() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func assertSingleEvent(t *testing.T, got []Event, want Event) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("captured %d events, want 1: %+v", len(got), got)
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("event = %+v, want %+v", got[0], want)
	}
}

func TestPublishesOrderPlaced(t *testing.T) {
	t.Parallel()

	spy := &spyPublisher{}
	svc := NewOrderService(spy)

	cmd := PlaceOrderCommand{IdempotencyKey: "k1", OrderID: "order-42", Amount: 1500}
	if err := svc.PlaceOrder(context.Background(), cmd); err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}

	assertSingleEvent(t, spy.Captured(), Event{Type: "OrderPlaced", OrderID: "order-42", Amount: 1500})
}

func TestIdempotentReplayPublishesOnce(t *testing.T) {
	t.Parallel()

	spy := &spyPublisher{}
	svc := NewOrderService(spy)

	cmd := PlaceOrderCommand{IdempotencyKey: "k1", OrderID: "order-42", Amount: 1500}
	for range 3 {
		if err := svc.PlaceOrder(context.Background(), cmd); err != nil {
			t.Fatalf("PlaceOrder: %v", err)
		}
	}

	assertSingleEvent(t, spy.Captured(), Event{Type: "OrderPlaced", OrderID: "order-42", Amount: 1500})
}

func TestRepublishAfterFailure(t *testing.T) {
	t.Parallel()

	spy := &spyPublisher{failN: 1}
	svc := NewOrderService(spy)
	cmd := PlaceOrderCommand{IdempotencyKey: "k1", OrderID: "order-42", Amount: 1500}

	if err := svc.PlaceOrder(context.Background(), cmd); err == nil {
		t.Fatal("first PlaceOrder should fail (broker unavailable)")
	}
	// The key was rolled back, so a retry republishes.
	if err := svc.PlaceOrder(context.Background(), cmd); err != nil {
		t.Fatalf("retry PlaceOrder: %v", err)
	}

	assertSingleEvent(t, spy.Captured(), Event{Type: "OrderPlaced", OrderID: "order-42", Amount: 1500})
}
```

## Review

Defining `EventPublisher` at the consumer, one method wide, is what makes the spy
trivial and the test decoupled from the broker's full surface. The capturing spy
gives state-based verification of the outbound event: `TestPublishesOrderPlaced`
pins the content, and `TestIdempotentReplayPublishesOnce` pins the exactly-once
contract by replaying the command and asserting a single captured event — the
property that actually matters for idempotency.

The subtle correctness point is the rollback: `TestRepublishAfterFailure` proves
that a transient publish failure does not permanently mark the key as done, so a
retry republishes and the system does not silently lose the event. Keep the spy's
slice behind its mutex with a defensive-copy accessor, and assert both content and
count — a spy you capture into but never assert on is the classic no-op that
manufactures false confidence.

## Resources

- [Go Code Review Comments: interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — accept interfaces, and define them at the consumer.
- [Effective Go: interfaces](https://go.dev/doc/effective_go#interfaces) — small interfaces and implicit satisfaction.
- [`reflect.DeepEqual`](https://pkg.go.dev/reflect#DeepEqual) — comparing captured event structs in the assertion helper.

---

Back to [07-testify-mock-notification-service.md](07-testify-mock-notification-service.md) | Next: [09-table-driven-stubbed-error-classification.md](09-table-driven-stubbed-error-classification.md)
