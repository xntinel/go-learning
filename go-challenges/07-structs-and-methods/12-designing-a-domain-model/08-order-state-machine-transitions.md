# Exercise 8: An Order Lifecycle as an Explicit State Machine

A lifecycle is a set of legal transitions, and scattering those rules across `if`
statements guarantees that some path eventually allows an illegal move like
`Shipped -> Pending`. This module encodes the order lifecycle as *data* — a
legal-transition table — and guards every move through one `Transition` method, so
illegal transitions are rejected with a sentinel and terminal states are final by
construction.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
order/                      independent module: example.com/order
  go.mod                    go 1.26
  order.go                  type Status (String); transition table; type Order; Transition
  cmd/
    demo/
      main.go               runnable demo: legal path, illegal move rejected
  order_test.go             tests: legal transitions, illegal transitions, terminal states, String
```

- Files: `order.go`, `cmd/demo/main.go`, `order_test.go`.
- Implement: a `Status` type with `String()`, a `map[Status]map[Status]bool` transition table, an `Order` with a status and history, and `Transition(to)` rejecting illegal moves with `ErrIllegalTransition`.
- Test: a table of legal transitions all succeed; a representative set of illegal ones all return `ErrIllegalTransition`; a terminal state rejects every further move; `String()` renders known values and an unknown fallback.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/12-designing-a-domain-model/08-order-state-machine-transitions/cmd/demo
cd go-solutions/07-structs-and-methods/12-designing-a-domain-model/08-order-state-machine-transitions
```

### Lifecycle rules belong in a table, not in scattered conditionals

The order lifecycle is `Pending -> Paid -> Shipped -> Delivered`, with `Cancelled`
reachable from the early states and both `Delivered` and `Cancelled` terminal.
Encoding this as data — a map from each status to the set of statuses it may move
to — makes the rules a single, readable, testable artifact. `Transition(to)` looks
up the current status in the table, checks whether `to` is in its allowed set, and
either records the move or returns `ErrIllegalTransition`. There is exactly one
place a transition can happen and exactly one place the rules live; a new rule is a
table edit, not a hunt through handlers.

Terminal states fall out for free. `Delivered` and `Cancelled` map to an empty
set, so every transition out of them fails the lookup and is rejected — they are
final by construction, with no special-case code. This is the payoff of data over
logic: "is this state terminal?" is answered by "is its row empty?", and you
cannot forget to guard a terminal state because the absence of an entry *is* the
guard.

`Status` is an `int` with a `String()` method so it prints as a name in logs and
errors rather than an opaque number. The `String()` includes a default branch that
renders an unknown value as `Status(N)` — important because a `Status` decoded from
a database or a wire payload might hold a number outside the defined range, and a
`String()` that panicked or returned empty on an unknown value would turn a data
glitch into a crash or a silent blank. The `Order` records its transition history,
so the lifecycle is auditable: you can see every state it passed through.

Create `order.go`:

```go
package order

import (
	"errors"
	"fmt"
)

var ErrIllegalTransition = errors.New("order: illegal state transition")

// Status is a point in the order lifecycle.
type Status int

const (
	Pending Status = iota
	Paid
	Shipped
	Delivered
	Cancelled
)

// String renders a Status as a name, with a fallback for unknown values.
func (s Status) String() string {
	switch s {
	case Pending:
		return "Pending"
	case Paid:
		return "Paid"
	case Shipped:
		return "Shipped"
	case Delivered:
		return "Delivered"
	case Cancelled:
		return "Cancelled"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// legalTransitions encodes the lifecycle rules as data. A status mapping to an
// empty set is terminal.
var legalTransitions = map[Status]map[Status]bool{
	Pending:   {Paid: true, Cancelled: true},
	Paid:      {Shipped: true, Cancelled: true},
	Shipped:   {Delivered: true},
	Delivered: {},
	Cancelled: {},
}

// Order is an entity whose state moves only through legal transitions.
type Order struct {
	id      string
	status  Status
	history []Status
}

// NewOrder starts a new order in the Pending state.
func NewOrder(id string) *Order {
	return &Order{id: id, status: Pending, history: []Status{Pending}}
}

func (o *Order) ID() string        { return o.id }
func (o *Order) Status() Status    { return o.status }
func (o *Order) History() []Status { return append([]Status(nil), o.history...) }

// Transition moves the order to "to" if the move is legal, else returns
// ErrIllegalTransition and leaves the order unchanged.
func (o *Order) Transition(to Status) error {
	if !legalTransitions[o.status][to] {
		return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, o.status, to)
	}
	o.status = to
	o.history = append(o.history, to)
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/order"
)

func main() {
	o := order.NewOrder("ord-1")
	fmt.Printf("start: %s\n", o.Status())

	for _, to := range []order.Status{order.Paid, order.Shipped, order.Delivered} {
		if err := o.Transition(to); err != nil {
			fmt.Printf("failed: %v\n", err)
			continue
		}
		fmt.Printf("now: %s\n", o.Status())
	}

	if err := o.Transition(order.Pending); errors.Is(err, order.ErrIllegalTransition) {
		fmt.Printf("rejected: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
start: Pending
now: Paid
now: Shipped
now: Delivered
rejected: order: illegal state transition: Delivered -> Pending
```

### Tests

The tests drive the table both ways. `TestLegalTransitions` walks every legal edge
and asserts each succeeds. `TestIllegalTransitions` is a table of representative
illegal moves — `Shipped -> Pending`, `Delivered -> Shipped`, `Cancelled -> Paid`
— each asserted to return `ErrIllegalTransition` and to leave the status unchanged.
`TestTerminalIsFinal` confirms a delivered order rejects every possible next state.
`TestStatusString` checks the names and the unknown fallback.

Create `order_test.go`:

```go
package order

import (
	"errors"
	"testing"
)

func TestLegalTransitions(t *testing.T) {
	t.Parallel()
	legal := []struct {
		from, to Status
	}{
		{Pending, Paid},
		{Pending, Cancelled},
		{Paid, Shipped},
		{Paid, Cancelled},
		{Shipped, Delivered},
	}
	for _, tc := range legal {
		t.Run(tc.from.String()+"_to_"+tc.to.String(), func(t *testing.T) {
			t.Parallel()
			o := NewOrder("ord-1")
			o.status = tc.from
			if err := o.Transition(tc.to); err != nil {
				t.Fatalf("legal %s -> %s failed: %v", tc.from, tc.to, err)
			}
			if o.Status() != tc.to {
				t.Fatalf("status = %s, want %s", o.Status(), tc.to)
			}
		})
	}
}

func TestIllegalTransitions(t *testing.T) {
	t.Parallel()
	illegal := []struct {
		from, to Status
	}{
		{Shipped, Pending},
		{Delivered, Shipped},
		{Cancelled, Paid},
		{Pending, Delivered},
		{Delivered, Cancelled},
	}
	for _, tc := range illegal {
		t.Run(tc.from.String()+"_to_"+tc.to.String(), func(t *testing.T) {
			t.Parallel()
			o := NewOrder("ord-1")
			o.status = tc.from
			if err := o.Transition(tc.to); !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("illegal %s -> %s err = %v, want ErrIllegalTransition", tc.from, tc.to, err)
			}
			if o.Status() != tc.from {
				t.Fatalf("illegal transition changed status to %s", o.Status())
			}
		})
	}
}

func TestTerminalIsFinal(t *testing.T) {
	t.Parallel()
	for _, terminal := range []Status{Delivered, Cancelled} {
		o := NewOrder("ord-1")
		o.status = terminal
		for _, to := range []Status{Pending, Paid, Shipped, Delivered, Cancelled} {
			if err := o.Transition(to); !errors.Is(err, ErrIllegalTransition) {
				t.Fatalf("terminal %s allowed transition to %s", terminal, to)
			}
		}
	}
}

func TestStatusString(t *testing.T) {
	t.Parallel()
	cases := map[Status]string{
		Pending:    "Pending",
		Delivered:  "Delivered",
		Status(99): "Status(99)",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Fatalf("Status(%d).String() = %q, want %q", int(s), got, want)
		}
	}
}
```

## Review

The state machine is correct when the transition table is the single source of
lifecycle truth and `Transition` is the only mover. The terminal-state test is the
one that proves the data-over-logic payoff: `Delivered` and `Cancelled` reject
everything with no dedicated code, purely because their table rows are empty. The
mistakes to avoid: encoding the rules as scattered conditionals (which drift out of
sync and eventually allow an illegal move), and a `String()` with no default branch
(which turns an out-of-range value from the database into a blank or a panic). The
history slice makes the lifecycle auditable, and `History()` returns a copy so the
audit trail cannot be edited from outside.

## Resources

- [Effective Go: The switch statement](https://go.dev/doc/effective_go#switch) — the `String()` method idiom with a default fallback.
- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the interface `Status.String` satisfies.
- [Rob Pike: Lexical Scanning in Go](https://go.dev/talks/2011/lex.slide) — state machines expressed as data/functions in Go.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-aggregate-root-ledger-invariant.md](07-aggregate-root-ledger-invariant.md) | Next: [09-optimistic-concurrency-version.md](09-optimistic-concurrency-version.md)
