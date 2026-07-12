# Exercise 9: The Event Router That Silently Dropped Unknown Messages

An event dispatcher decodes a stream of domain events into an interface and routes
on the concrete type via a type switch. It shipped with a `default` arm that
returned `nil`, so an event type nobody added a case for was silently dropped
instead of surfaced as an error — data loss with nothing to page on. And one arm
filed events into the wrong bucket. You will reproduce with a known-unhandled type,
diagnose the type-switch arms and default, and fix the routing.

## What you'll build

```text
events/                    module example.com/events
  go.mod
  events.go                Event interface; typed events; Handler.Dispatch; ErrUnhandledEvent
  cmd/demo/
    main.go                runnable demo: route a stream including an unhandled type
  events_test.go           per-type routing table, unhandled + nil rows, Example
```

- Files: `events.go`, `cmd/demo/main.go`, `events_test.go`.
- Implement: `Handler.Dispatch(Event) error` with a type switch that routes each known event to its bucket and a `default` returning `fmt.Errorf("%w: %T", ErrUnhandledEvent, e)`.
- Test: a table over each concrete type asserting the correct side effect; a row with an unregistered type asserting `errors.Is(err, ErrUnhandledEvent)`; a nil row; a check that `PaymentFailed` lands in the failed bucket, not another.
- Verify: `go test -count=1 -race ./...`.

### The artifact and the planted bug

The dispatcher is a policy point: every event on the stream is either handled or
explicitly rejected, never silently discarded. The version that shipped got the
policy backwards and mis-filed one type:

```go
func (h *Handler) Dispatch(e Event) error {
	switch ev := e.(type) {
	case OrderPlaced:
		h.Placed = append(h.Placed, ev.OrderID)
	case OrderShipped:
		h.Shipped = append(h.Shipped, ev.OrderID)
	case PaymentFailed:
		h.Shipped = append(h.Shipped, ev.OrderID) // BUG: filed under Shipped, not Failed
	default:
		// BUG: no error; an unrecognized event vanishes
	}
	return nil
}
```

Two defects. The `default` arm returns `nil`, so any event type that upstream
added but the router was never taught about — a new `OrderCancelled`, say — is
dropped and the caller sees success. In an event pipeline that looks exactly like
data loss with no error to alert on. And the `PaymentFailed` arm appends to
`Shipped`, so failed payments are counted as shipments. Both pass review: the
common events route correctly, and the `default` is easy to skim past. (Note that
`fallthrough` is not permitted in a type switch, so a "leak into the next arm" bug
there is always a mis-filed case body like this one, never a literal fall-through.)

The failing rows read:

```text
--- FAIL: TestDispatch/unregistered_type (0.00s)
    events_test.go:61: Dispatch = <nil>, want ErrUnhandledEvent
--- FAIL: TestDispatch/payment_failed (0.00s)
    events_test.go:66: handler side effects wrong: &{[] [o3] []}
```

The fix files each event under its own bucket and returns
`ErrUnhandledEvent` from the `default` for anything unregistered (and for a nil
interface, which also lands in `default`).

Create `events.go`:

```go
package events

import (
	"errors"
	"fmt"
)

// ErrUnhandledEvent reports an event whose concrete type has no route. Returning
// it instead of nil turns a forgotten case into a visible error, not data loss.
var ErrUnhandledEvent = errors.New("unhandled event type")

// Event is a domain event carried on the stream. The unexported method keeps the
// set of event types closed to this package.
type Event interface{ isEvent() }

type OrderPlaced struct {
	OrderID string
	Amount  int
}

type OrderShipped struct {
	OrderID string
	Carrier string
}

type PaymentFailed struct {
	OrderID string
	Reason  string
}

// OrderCancelled is a valid Event with no route yet: it stands in for a type
// added upstream that the dispatcher has not been taught to handle.
type OrderCancelled struct {
	OrderID string
}

func (OrderPlaced) isEvent()    {}
func (OrderShipped) isEvent()   {}
func (PaymentFailed) isEvent()  {}
func (OrderCancelled) isEvent() {}

// Handler records the side effects of routing. In production these would be
// repository writes, outbound messages, and metrics.
type Handler struct {
	Placed  []string
	Shipped []string
	Failed  []string
}

// Dispatch routes an event to its handler by concrete type. Each known type is
// filed under its own bucket; anything unregistered (or a nil event) returns
// ErrUnhandledEvent rather than being silently dropped.
func (h *Handler) Dispatch(e Event) error {
	switch ev := e.(type) {
	case OrderPlaced:
		h.Placed = append(h.Placed, ev.OrderID)
		return nil
	case OrderShipped:
		h.Shipped = append(h.Shipped, ev.OrderID)
		return nil
	case PaymentFailed:
		h.Failed = append(h.Failed, ev.OrderID)
		return nil
	default:
		return fmt.Errorf("%w: %T", ErrUnhandledEvent, e)
	}
}
```

Each arm returns immediately after filing its event, so no arm can leak into
another, and the `default` makes the "unregistered type" policy explicit: it
returns a typed error that upstream can match with `errors.Is`. A nil `Event`
matches `default` too (none of the concrete cases match a nil dynamic type), so
the nil path is covered without a special case.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/events"
)

func main() {
	h := &events.Handler{}
	stream := []events.Event{
		events.OrderPlaced{OrderID: "o-1", Amount: 4200},
		events.OrderShipped{OrderID: "o-1", Carrier: "dhl"},
		events.PaymentFailed{OrderID: "o-2", Reason: "card declined"},
		events.OrderCancelled{OrderID: "o-3"},
	}
	for _, e := range stream {
		if err := h.Dispatch(e); err != nil {
			fmt.Printf("DROP %T: %v\n", e, err)
			continue
		}
		fmt.Printf("OK   %T\n", e)
	}
	fmt.Printf("placed=%v shipped=%v failed=%v\n", h.Placed, h.Shipped, h.Failed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
OK   events.OrderPlaced
OK   events.OrderShipped
OK   events.PaymentFailed
DROP events.OrderCancelled: unhandled event type: events.OrderCancelled
placed=[o-1] shipped=[o-1] failed=[o-2]
```

### Tests

`TestDispatch` is the table: one row per known type asserting it lands in the right
bucket and nowhere else, a row for the unregistered `OrderCancelled` asserting
`errors.Is(err, ErrUnhandledEvent)`, and a nil-event row pinning the nil path. The
`payment_failed` row is what catches the mis-filed arm, since it checks the event
went to `Failed` and not `Shipped`.

Create `events_test.go`:

```go
package events

import (
	"errors"
	"fmt"
	"testing"
)

func TestDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		event   Event
		check   func(*Handler) bool
		wantErr error
	}{
		{
			name:  "order placed",
			event: OrderPlaced{OrderID: "o1"},
			check: func(h *Handler) bool {
				return len(h.Placed) == 1 && h.Placed[0] == "o1" && len(h.Shipped) == 0 && len(h.Failed) == 0
			},
		},
		{
			name:  "order shipped",
			event: OrderShipped{OrderID: "o2"},
			check: func(h *Handler) bool {
				return len(h.Shipped) == 1 && h.Shipped[0] == "o2" && len(h.Placed) == 0 && len(h.Failed) == 0
			},
		},
		{
			name:  "payment failed",
			event: PaymentFailed{OrderID: "o3"},
			check: func(h *Handler) bool {
				return len(h.Failed) == 1 && h.Failed[0] == "o3" && len(h.Shipped) == 0 && len(h.Placed) == 0
			},
		},
		{
			name:    "unregistered type",
			event:   OrderCancelled{OrderID: "o4"},
			check:   func(h *Handler) bool { return len(h.Placed) == 0 && len(h.Shipped) == 0 && len(h.Failed) == 0 },
			wantErr: ErrUnhandledEvent,
		},
		{
			name:    "nil event",
			event:   nil,
			check:   func(h *Handler) bool { return len(h.Placed) == 0 && len(h.Shipped) == 0 && len(h.Failed) == 0 },
			wantErr: ErrUnhandledEvent,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := &Handler{}
			err := h.Dispatch(tc.event)
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Dispatch = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Dispatch = %v, want %v", err, tc.wantErr)
			}
			if !tc.check(h) {
				t.Fatalf("handler side effects wrong: %+v", h)
			}
		})
	}
}

func ExampleHandler_Dispatch() {
	h := &Handler{}
	_ = h.Dispatch(OrderPlaced{OrderID: "1001"})
	err := h.Dispatch(OrderCancelled{OrderID: "1002"})
	fmt.Println(h.Placed, errors.Is(err, ErrUnhandledEvent))
	// Output: [1001] true
}
```

## Review

The dispatcher is correct when every known event lands in its own bucket and every
unregistered or nil event returns `ErrUnhandledEvent`. The type switch is a
dispatch table over concrete types, and its `default` arm is a policy decision:
returning `nil` there silently drops any type you forgot to add, which in a
pipeline is indistinguishable from lost data. Return a typed error instead so the
gap is visible and matchable with `errors.Is`. Because `fallthrough` is illegal in
a type switch, the "leak into the next arm" failure shows up as a mis-filed case
body — which is why each row asserts the event went to its bucket *and* not to any
other. When you add an event type, add its case in lockstep; the switch is the
source of truth.

## Resources

- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches) — the type switch and why `fallthrough` is not allowed.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching the typed `ErrUnhandledEvent` sentinel.
- [Effective Go: Type switch](https://go.dev/doc/effective_go#type_switch) — routing on the dynamic type of an interface.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-labeled-break-batch-scan.md](08-labeled-break-batch-scan.md) | Next: [10-continue-skips-cleanup-leaked-handle.md](10-continue-skips-cleanup-leaked-handle.md)
