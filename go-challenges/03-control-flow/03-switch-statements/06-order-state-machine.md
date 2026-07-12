# Exercise 6: Guard Order State Transitions With an Expression Switch

An order/payment lifecycle is a closed set of states with strict rules about
which transitions are legal: a `Delivered` order cannot go back to `Pending`, a
`Cancelled` one cannot ship. This module builds the transition guard as an
expression switch over the current state — the canonical use of switch as an
*exhaustiveness* tool for a closed domain enum.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
order/                     independent module: example.com/order-state-machine
  go.mod                   go 1.24
  order.go                 OrderState enum + String(); CanTransition(from, to) error
  cmd/
    demo/
      main.go              runnable demo of legal and illegal moves
  order_test.go            (from,to) matrix + all-states enumeration guard
```

- Files: `order.go`, `cmd/demo/main.go`, `order_test.go`.
- Implement: an `OrderState` enum with a `Stringer` and `CanTransition(from, to OrderState) error` (expression switch, `slices.Contains` per case, typed `ErrIllegalTransition`).
- Test: a matrix of legal/illegal pairs asserted with `errors.Is`, including self-transitions and terminal states, plus a test that drives every state through `CanTransition` to catch a missing case.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Switch as the shape of a closed domain

The states form a closed enum, and the transition rules are naturally expressed
as an expression switch on the *current* state where each case enumerates the
legal next states. That structure is deliberate: reading the switch top to
bottom is reading the state machine's transition table, and the compiler-visible
enum plus the per-case allow-lists make the domain legible.

`slices.Contains` checks membership in each case's allow-list, and
`ErrIllegalTransition` is the single typed error every rejection wraps, so callers
assert one sentinel with `errors.Is` regardless of which specific move was
refused. Terminal states (`Cancelled`, `Refunded`) have empty allow-lists, so
every move out of them is rejected — that is the whole point of a terminal state.

The subtle risk this exercise targets is the *forgotten case*. Six months from
now someone adds `StateReturned` to the enum. If the switch has no case for it,
`CanTransition(StateReturned, anything)` hits the `default` and every move out of
`StateReturned` is rejected — or worse, if the default were permissive, silently
allowed. `TestExhaustiveStates` drives every state through the switch so a
newly-added state with no case is caught immediately: the day the enum grows, the
test tells you which switch you forgot to update. (The `exhaustive` linter
enforces the same invariant in CI.)

Create `order.go`:

```go
package order

import (
	"errors"
	"fmt"
	"slices"
)

// ErrIllegalTransition is the typed error every rejected move wraps.
var ErrIllegalTransition = errors.New("illegal state transition")

// OrderState is the closed set of order lifecycle states.
type OrderState int

const (
	StatePending OrderState = iota
	StatePaid
	StateShipped
	StateDelivered
	StateCancelled
	StateRefunded
)

// allStates is the enumeration used by the exhaustiveness test; keep it in sync
// with the constants above.
var allStates = []OrderState{
	StatePending, StatePaid, StateShipped, StateDelivered, StateCancelled, StateRefunded,
}

func (s OrderState) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StatePaid:
		return "paid"
	case StateShipped:
		return "shipped"
	case StateDelivered:
		return "delivered"
	case StateCancelled:
		return "cancelled"
	case StateRefunded:
		return "refunded"
	default:
		return fmt.Sprintf("OrderState(%d)", int(s))
	}
}

// CanTransition reports whether moving from -> to is legal. Each case of the
// expression switch enumerates the legal next states for the current state;
// terminal states have none. An unknown current state fails closed.
func CanTransition(from, to OrderState) error {
	var allowed []OrderState
	switch from {
	case StatePending:
		allowed = []OrderState{StatePaid, StateCancelled}
	case StatePaid:
		allowed = []OrderState{StateShipped, StateRefunded, StateCancelled}
	case StateShipped:
		allowed = []OrderState{StateDelivered}
	case StateDelivered:
		allowed = []OrderState{StateRefunded}
	case StateCancelled, StateRefunded:
		allowed = nil // terminal states: no legal moves out
	default:
		return fmt.Errorf("%w: unknown state %s", ErrIllegalTransition, from)
	}
	if slices.Contains(allowed, to) {
		return nil
	}
	return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/order-state-machine"
)

func main() {
	moves := []struct{ from, to order.OrderState }{
		{order.StatePending, order.StatePaid},
		{order.StatePaid, order.StateShipped},
		{order.StateShipped, order.StateDelivered},
		{order.StateDelivered, order.StatePending},
		{order.StateCancelled, order.StateShipped},
	}
	for _, m := range moves {
		if err := order.CanTransition(m.from, m.to); err != nil {
			fmt.Printf("%-10s -> %-10s REJECT: %v\n", m.from, m.to, err)
		} else {
			fmt.Printf("%-10s -> %-10s OK\n", m.from, m.to)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pending    -> paid       OK
paid       -> shipped    OK
shipped    -> delivered  OK
delivered  -> pending    REJECT: illegal state transition: delivered -> pending
cancelled  -> shipped    REJECT: illegal state transition: cancelled -> shipped
```

### Tests

`TestCanTransition` walks a matrix of legal and illegal pairs, including a
self-transition and moves out of terminal states, asserting `nil` for legal and
`errors.Is(err, ErrIllegalTransition)` for illegal. `TestExhaustiveStates` drives
every state through `CanTransition` as the `from` argument and asserts none hits
the unknown-state default — the guard that catches a state added to the enum but
forgotten in the switch.

Create `order_test.go`:

```go
package order

import (
	"errors"
	"strings"
	"testing"
)

func TestCanTransition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		from, to OrderState
		wantOK   bool
	}{
		{StatePending, StatePaid, true},
		{StatePending, StateCancelled, true},
		{StatePending, StateShipped, false},
		{StatePaid, StateShipped, true},
		{StatePaid, StateRefunded, true},
		{StatePaid, StateDelivered, false},
		{StateShipped, StateDelivered, true},
		{StateShipped, StatePending, false},
		{StateDelivered, StateRefunded, true},
		{StateDelivered, StatePending, false},
		{StatePaid, StatePaid, false},         // self-transition rejected
		{StateCancelled, StateShipped, false}, // terminal: no moves out
		{StateRefunded, StatePaid, false},     // terminal: no moves out
	}

	for _, tc := range tests {
		err := CanTransition(tc.from, tc.to)
		if tc.wantOK {
			if err != nil {
				t.Errorf("CanTransition(%s, %s) = %v, want nil", tc.from, tc.to, err)
			}
			continue
		}
		if !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("CanTransition(%s, %s) err = %v, want errors.Is ErrIllegalTransition", tc.from, tc.to, err)
		}
	}
}

func TestExhaustiveStates(t *testing.T) {
	t.Parallel()

	// Every known state must be handled by a real case, not the unknown-state
	// default. If the enum grows and a case is forgotten, the new state lands in
	// default and its error mentions "unknown state", failing this test.
	for _, from := range allStates {
		err := CanTransition(from, StatePaid)
		if err != nil && strings.Contains(err.Error(), "unknown state") {
			t.Errorf("state %s hit the unknown-state default; add a case for it", from)
		}
	}
}

func TestUnknownStateFailsClosed(t *testing.T) {
	t.Parallel()

	if err := CanTransition(OrderState(99), StatePaid); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("CanTransition(unknown, paid) err = %v, want errors.Is ErrIllegalTransition", err)
	}
}
```

## Review

The guard is correct when it accepts exactly the legal transitions and rejects
everything else with one typed error. Reading the switch is reading the
transition table, which is why an expression switch over the enum is the right
shape: the closed domain is visible and the per-case allow-lists are the rules.
The two structural defenses are the ones that survive the enum growing:
`TestExhaustiveStates` catches a state that gained no case, and the fail-closed
`default` ensures an unknown state is rejected rather than silently allowed. Self
and terminal transitions being rejected is not an edge case to paper over — it is
the specification.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — expression switch with comma-list cases.
- [slices.Contains](https://pkg.go.dev/slices#Contains) — membership test used for each case's allow-list.
- [Go Wiki: Enums with iota and Stringer](https://go.dev/wiki/Iota) — modeling a closed domain as a typed enum.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-method-router-405.md](05-method-router-405.md) | Next: [07-domain-error-to-status.md](07-domain-error-to-status.md)
