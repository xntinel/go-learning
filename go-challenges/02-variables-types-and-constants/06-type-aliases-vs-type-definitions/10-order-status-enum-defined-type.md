# Exercise 10: A Defined String Type as a Validated Order-Status State Machine

An order's status is not an arbitrary string — it is a member of a small set with
legal transitions between members. This exercise models it as
`type OrderStatus string` with exported typed constants, a `Valid()` check, and a
`CanTransitionTo(next)` method that enforces a state-transition graph. Because the
type is defined, handlers cannot pass raw strings, and the transition rules live on
the domain type instead of being re-derived in scattered `switch` statements.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
orderstatus/              independent module: example.com/orderstatus
  go.mod                  go 1.24
  status.go               type OrderStatus string; Status* constants; Valid,
                          String, CanTransitionTo, ParseOrderStatus
  cmd/
    demo/
      main.go             drives a legal lifecycle and rejects an illegal jump
  status_test.go          transition matrix, Valid, parse-rejection tests
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: `OrderStatus` with constants `StatusPending`/`StatusPaid`/`StatusShipped`/`StatusCancelled`, `Valid()`, `String()`, `CanTransitionTo(next OrderStatus) bool`, and `ParseOrderStatus(raw string) (OrderStatus, error)`.
- Test: the full transition matrix (legal vs illegal), `Valid()` rejecting unknown values, and `ParseOrderStatus` rejecting untrusted unknown input via a wrapped sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### The domain type owns its rules

`type OrderStatus string` is a defined type, so a handler cannot pass a bare
`"shipped"` where an `OrderStatus` is required — it must go through
`ParseOrderStatus`, the single validating boundary that rejects any value not in
the known set. That is the same trust-boundary idea as the `SafeHTML` exercise: an
untrusted request string becomes a typed, validated value at exactly one place.

The transition rules live on the type as data plus a method, not as a `switch` that
gets copy-pasted into every handler. A small adjacency map encodes the legal graph —
`Pending` can go to `Paid` or `Cancelled`; `Paid` to `Shipped` or `Cancelled`;
`Shipped` and `Cancelled` are terminal — and `CanTransitionTo` consults it with
`slices.Contains`. When the rules change, they change in one place. `Valid()`
reports membership in the set; `String()` satisfies `fmt.Stringer` for logs.

The payoff is that illegal states and illegal transitions are caught by the domain
type, not by remembering to write the right `if` at each call site. A handler reads
`ParseOrderStatus(req.Status)`, then `current.CanTransitionTo(next)`, and the
business rule is enforced without the handler knowing the graph.

Create `status.go`:

```go
package orderstatus

import (
	"errors"
	"fmt"
	"slices"
)

// ErrUnknownStatus is returned when a raw string is not a known status.
var ErrUnknownStatus = errors.New("unknown order status")

// OrderStatus is a DEFINED string type, so a raw request string cannot be used
// where a status is required without going through ParseOrderStatus.
type OrderStatus string

const (
	StatusPending   OrderStatus = "pending"
	StatusPaid      OrderStatus = "paid"
	StatusShipped   OrderStatus = "shipped"
	StatusCancelled OrderStatus = "cancelled"
)

// transitions is the legal state-transition graph. Shipped and Cancelled are
// terminal (no outgoing edges).
var transitions = map[OrderStatus][]OrderStatus{
	StatusPending: {StatusPaid, StatusCancelled},
	StatusPaid:    {StatusShipped, StatusCancelled},
}

func (s OrderStatus) String() string { return string(s) }

// Valid reports whether s is one of the known statuses.
func (s OrderStatus) Valid() bool {
	switch s {
	case StatusPending, StatusPaid, StatusShipped, StatusCancelled:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether moving from s to next is a legal transition.
func (s OrderStatus) CanTransitionTo(next OrderStatus) bool {
	return slices.Contains(transitions[s], next)
}

// ParseOrderStatus converts an untrusted request string into a typed OrderStatus,
// rejecting any value outside the known set with an error wrapping ErrUnknownStatus.
func ParseOrderStatus(raw string) (OrderStatus, error) {
	s := OrderStatus(raw)
	if !s.Valid() {
		return "", fmt.Errorf("parse status %q: %w", raw, ErrUnknownStatus)
	}
	return s, nil
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/orderstatus"
)

func main() {
	// An untrusted status from a request goes through the validating constructor.
	current, err := orderstatus.ParseOrderStatus("pending")
	if err != nil {
		panic(err)
	}

	lifecycle := []orderstatus.OrderStatus{
		orderstatus.StatusPaid,
		orderstatus.StatusShipped,
	}
	for _, next := range lifecycle {
		if current.CanTransitionTo(next) {
			fmt.Printf("%s -> %s: ok\n", current, next)
			current = next
		} else {
			fmt.Printf("%s -> %s: rejected\n", current, next)
		}
	}

	// An illegal jump from a terminal state.
	fmt.Printf("%s -> %s: %v\n", current, orderstatus.StatusPending,
		current.CanTransitionTo(orderstatus.StatusPending))

	// An unknown status from the wire is rejected.
	if _, err := orderstatus.ParseOrderStatus("teleported"); err != nil {
		fmt.Println("parse error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
pending -> paid: ok
paid -> shipped: ok
shipped -> pending: false
parse error: parse status "teleported": unknown order status
```

### Tests

The tests drive the full transition matrix (every legal edge succeeds and a
selection of illegal ones fail), confirm `Valid()` accepts the four constants and
rejects an unknown value, and confirm `ParseOrderStatus` rejects untrusted input
via the wrapped sentinel.

Create `status_test.go`:

```go
package orderstatus

import (
	"errors"
	"fmt"
	"testing"
)

func TestTransitionMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		from OrderStatus
		to   OrderStatus
		want bool
	}{
		{StatusPending, StatusPaid, true},
		{StatusPending, StatusCancelled, true},
		{StatusPending, StatusShipped, false}, // must be paid first
		{StatusPaid, StatusShipped, true},
		{StatusPaid, StatusCancelled, true},
		{StatusPaid, StatusPending, false},      // no going back
		{StatusShipped, StatusCancelled, false}, // terminal
		{StatusShipped, StatusPaid, false},      // terminal
		{StatusCancelled, StatusPaid, false},    // terminal
	}
	for _, tc := range tests {
		name := fmt.Sprintf("%s_to_%s", tc.from, tc.to)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.from.CanTransitionTo(tc.to); got != tc.want {
				t.Fatalf("%s.CanTransitionTo(%s) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

func TestValid(t *testing.T) {
	t.Parallel()

	for _, s := range []OrderStatus{StatusPending, StatusPaid, StatusShipped, StatusCancelled} {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if OrderStatus("refunded").Valid() {
		t.Error("unknown status should be invalid")
	}
}

func TestParseOrderStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    OrderStatus
		wantErr bool
	}{
		{"pending", StatusPending, false},
		{"shipped", StatusShipped, false},
		{"", "", true},
		{"PENDING", "", true}, // case-sensitive; wire values are lower-case
		{"teleported", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseOrderStatus(tc.in)
			if tc.wantErr {
				if !errors.Is(err, ErrUnknownStatus) {
					t.Fatalf("ParseOrderStatus(%q) err = %v, want ErrUnknownStatus", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOrderStatus(%q) unexpected err: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseOrderStatus(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func ExampleOrderStatus_CanTransitionTo() {
	fmt.Println(StatusPending.CanTransitionTo(StatusPaid))
	fmt.Println(StatusShipped.CanTransitionTo(StatusPending))
	// Output:
	// true
	// false
}
```

## Review

The type is correct when every legal edge in the graph succeeds, every illegal or
terminal-state transition fails, and `ParseOrderStatus` refuses any string outside
the four known values. The mistake this exercise targets is scattering the rules:
re-deriving "is this a valid status" and "is this transition allowed" in `switch`
statements across handlers, and passing raw request strings straight through. Put
`Valid()` and `CanTransitionTo()` on the defined type and funnel untrusted input
through `ParseOrderStatus`, and the business rules are enforced in one place the
compiler steers callers toward. Assert parse failures with `errors.Is` against the
sentinel.

## Resources

- [`slices.Contains`](https://pkg.go.dev/slices#Contains) — the membership check behind the transition graph.
- [Go Language Spec: Type definitions](https://go.dev/ref/spec#Type_definitions) — why a defined string type rejects raw strings.
- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the interface `String()` satisfies for logging.

---

Prev: [09-method-set-loss-when-wrapping.md](09-method-set-loss-when-wrapping.md) | Next: [../07-numeric-precision-and-overflow/00-concepts.md](../07-numeric-precision-and-overflow/00-concepts.md)
