# Exercise 7: Route Domain Events With A Type-Switch Dispatcher

A worker that consumes a stream of domain events must route each to the right
handler by its concrete type. A type switch over pointer types is the direct way to
do this; the discipline is a `default` that returns a typed error so an event type
nobody handled is observable, not silently dropped.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
eventbus/                   independent module: example.com/eventbus
  go.mod                    module path
  eventbus.go               Event marker interface; OrderPlaced/OrderCancelled/PaymentCaptured; Dispatch
  cmd/
    demo/
      main.go               runnable demo dispatching a batch of events
  eventbus_test.go          each handler fires; unknown event -> ErrUnhandledEvent
```

Files: `eventbus.go`, `cmd/demo/main.go`, `eventbus_test.go`.
Implement: a marker interface `Event` with an unexported method, three concrete event types, a `Dispatcher` with side-effect-recording handlers, and `Dispatch(e Event) error` type-switching over the concrete types with a typed `default`.
Test: dispatch each known event and assert its handler ran; dispatch an unknown `Event` and assert `errors.Is(err, ErrUnhandledEvent)`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/eventbus/cmd/demo
cd ~/go-exercises/eventbus
go mod init example.com/eventbus
```

### The marker interface and the total switch

`Event` is a marker interface: it declares one unexported method `isEvent()` that
only types in this package can implement. That is the sealing trick — a consumer in
another package cannot accidentally (or maliciously) create a new `Event`, so the
set of concrete types the dispatcher must handle is closed and reviewable. The three
event types implement `isEvent()` with a pointer receiver, so an `Event` value is
always a `*OrderPlaced`, `*OrderCancelled`, or `*PaymentCaptured`.

`Dispatch` type-switches over those pointer types and calls the matching handler.
The `default` case returns `ErrUnhandledEvent` wrapped with the concrete type via
`%T`, so an event that slipped past every case shows up in logs and metrics with the
type that was missed — the difference between a silent data-loss bug and an alert.
Omitting the `default` would make an unhandled event a no-op, which in a worker is
exactly the failure you cannot see.

A note on ordering and scaling. Because the three cases are distinct concrete types
they cannot overlap, so their order is free. If instead you switched on an interface
that several events satisfied, the more specific case would have to come first. And
when the number of event types grows past what a switch reads well, the idiomatic
next step is a `map[reflect.Type]func(Event) error` registry keyed by
`reflect.TypeOf(e)` — the same dispatch, table-driven, at the cost of a reflect
lookup per event. The type switch is the right tool while the set is small and
fixed; the registry is the right tool when handlers are registered dynamically.

Create `eventbus.go`:

```go
package eventbus

import (
	"errors"
	"fmt"
)

// ErrUnhandledEvent is returned for an Event type Dispatch does not handle.
var ErrUnhandledEvent = errors.New("unhandled event")

// Event is a sealed marker interface: only types in this package implement it.
type Event interface {
	isEvent()
}

type OrderPlaced struct {
	OrderID string
	Amount  int
}

type OrderCancelled struct {
	OrderID string
	Reason  string
}

type PaymentCaptured struct {
	OrderID string
	Amount  int
}

func (*OrderPlaced) isEvent()     {}
func (*OrderCancelled) isEvent()  {}
func (*PaymentCaptured) isEvent() {}

// Dispatcher records the side effects of handling events.
type Dispatcher struct {
	Placed    []string
	Cancelled []string
	Captured  int
}

// Dispatch routes an event to its handler. An unrecognized event type returns
// ErrUnhandledEvent so the miss is observable rather than silently dropped.
func (d *Dispatcher) Dispatch(e Event) error {
	switch ev := e.(type) {
	case *OrderPlaced:
		d.Placed = append(d.Placed, ev.OrderID)
		return nil
	case *OrderCancelled:
		d.Cancelled = append(d.Cancelled, ev.OrderID)
		return nil
	case *PaymentCaptured:
		d.Captured += ev.Amount
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrUnhandledEvent, e)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/eventbus"
)

func main() {
	d := &eventbus.Dispatcher{}
	events := []eventbus.Event{
		&eventbus.OrderPlaced{OrderID: "o1", Amount: 100},
		&eventbus.PaymentCaptured{OrderID: "o1", Amount: 100},
		&eventbus.OrderPlaced{OrderID: "o2", Amount: 50},
		&eventbus.OrderCancelled{OrderID: "o2", Reason: "fraud"},
	}
	for _, e := range events {
		if err := d.Dispatch(e); err != nil {
			fmt.Println("error:", err)
		}
	}
	fmt.Printf("placed=%v cancelled=%v captured=%d\n", d.Placed, d.Cancelled, d.Captured)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
placed=[o1 o2] cancelled=[o2] captured=100
```

### Tests

Each known event asserts its side effect; the unknown event — a private type
declared in the test that also satisfies `Event` — asserts `ErrUnhandledEvent`
through `errors.Is`, proving the `default` case is the observable fallback.

Create `eventbus_test.go`:

```go
package eventbus

import (
	"errors"
	"fmt"
	"testing"
)

// unknownEvent satisfies Event (same package can implement isEvent) but is not
// handled by Dispatch, exercising the default case.
type unknownEvent struct{}

func (*unknownEvent) isEvent() {}

func TestDispatchRoutesKnownEvents(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{}

	if err := d.Dispatch(&OrderPlaced{OrderID: "o1"}); err != nil {
		t.Fatalf("OrderPlaced: %v", err)
	}
	if err := d.Dispatch(&OrderCancelled{OrderID: "o1"}); err != nil {
		t.Fatalf("OrderCancelled: %v", err)
	}
	if err := d.Dispatch(&PaymentCaptured{OrderID: "o1", Amount: 250}); err != nil {
		t.Fatalf("PaymentCaptured: %v", err)
	}

	if len(d.Placed) != 1 || d.Placed[0] != "o1" {
		t.Fatalf("Placed = %v", d.Placed)
	}
	if len(d.Cancelled) != 1 || d.Cancelled[0] != "o1" {
		t.Fatalf("Cancelled = %v", d.Cancelled)
	}
	if d.Captured != 250 {
		t.Fatalf("Captured = %d, want 250", d.Captured)
	}
}

func TestDispatchUnknownEventIsObservable(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{}
	err := d.Dispatch(&unknownEvent{})
	if !errors.Is(err, ErrUnhandledEvent) {
		t.Fatalf("err = %v, want ErrUnhandledEvent", err)
	}
}

func TestDispatchIsIdempotentPerCall(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{}
	for range 3 {
		if err := d.Dispatch(&OrderPlaced{OrderID: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.Placed) != 3 {
		t.Fatalf("Placed = %v, want three entries", d.Placed)
	}
}

func ExampleDispatcher_Dispatch() {
	d := &Dispatcher{}
	events := []Event{
		&OrderPlaced{OrderID: "o1", Amount: 100},
		&PaymentCaptured{OrderID: "o1", Amount: 100},
		&OrderCancelled{OrderID: "o1", Reason: "customer request"},
	}
	for _, e := range events {
		if err := d.Dispatch(e); err != nil {
			fmt.Println("error:", err)
		}
	}
	fmt.Printf("placed=%v cancelled=%v captured=%d\n", d.Placed, d.Cancelled, d.Captured)
	// Output:
	// placed=[o1] cancelled=[o1] captured=100
}
```

## Review

The dispatcher is correct when every known event lands in its case with the right
side effect and every unknown event returns `ErrUnhandledEvent` — never a silent
no-op. The sealed marker interface keeps the set of concrete types closed, so the
switch is reviewable and the `default` case is genuinely a "should not happen"
signal worth alerting on. The one design decision to keep in mind is when to move
from a type switch to a `map[reflect.Type]handler` registry: switch while the set is
small and fixed, registry when handlers register dynamically. Run `go test -race` to
confirm routing and the observable fallback.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [reflect.TypeOf](https://pkg.go.dev/reflect#TypeOf) — the key type for a registry-based dispatcher.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-typed-nil-interface-guard.md](06-typed-nil-interface-guard.md) | Next: [08-loggable-value-detection.md](08-loggable-value-detection.md)
