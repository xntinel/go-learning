# Exercise 8: Why My Stringer Does Not Fire — Pointer vs Value in fmt

You add a `String()` method to an order-state type so logs read `shipped`, not a
raw number — and the logs still show the raw number. The cause is the method-set
rule meeting `fmt`'s runtime check: `String()` on the pointer receiver is not in
the value's method set, so passing a value to `fmt` skips it. This module
reproduces the trap and fixes it by putting `String()` on the value receiver.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
orderstate/                    independent module: example.com/orderstate
  go.mod                       module path + go directive
  state.go                     OrderState (value String, fires); BadState (pointer-only String, trap)
  cmd/
    demo/
      main.go                  print both; watch the trap and the fix
  state_test.go                assert value-Stringer fires; assert pointer-only trap leaves raw output
```

- Files: `state.go`, `cmd/demo/main.go`, `state_test.go`.
- Implement: an `OrderState` with `String()` on the value receiver (fires for both value and pointer), and a `BadState` with `String()` on the pointer receiver only (does not fire for a value).
- Test: `fmt.Sprint(orderState)` returns the label; `fmt.Sprint(badState)` returns the raw number (the trap); `fmt.Sprint(&badState)` returns the label (the pointer form).
- Verify: `go vet ./...`, `go test -count=1 -race ./...`.

### Why fmt skips a pointer-only Stringer for a value

`fmt` does not know your type at compile time. For each argument it checks at
runtime whether the argument's concrete type satisfies `fmt.Stringer`
(`String() string`) or `error`, and if so calls that method; otherwise it falls
back to reflecting over the value and printing its underlying representation — for
a named integer type, the integer.

That runtime check uses the argument's method set. If `String()` is declared on
the pointer receiver, it is in the method set of `*OrderState` only. Pass an
`OrderState` value to `fmt.Sprint` and the value's method set has no `String`, so
the check fails and `fmt` prints the raw number. The method exists, compiles, and
is even called correctly when you happen to pass a pointer — so the bug is
maddening: `log.Printf("%v", &state)` looks right, `log.Printf("%v", state)`
prints `2`, and nothing in the code looks wrong.

The fix is to declare `String()` on the value receiver. A value method is in the
method set of both `OrderState` and `*OrderState`, so `fmt` finds it whether you
pass a value or a pointer, and the label prints either way. This is why domain
enums and status types almost always put `String()` on the value receiver: they
are passed around as values, and `fmt`/logging must format them.

`OrderState` below is the correct version; `BadState` is the same enum with
`String()` on the pointer receiver, kept so a test can prove the trap: a `BadState`
value prints the raw number, a `*BadState` prints the label.

Create `state.go`:

```go
package orderstate

// OrderState is an order lifecycle state. String() is on the VALUE receiver, so
// it is in the method set of both OrderState and *OrderState and fmt formats
// either form as the label.
type OrderState int

const (
	Pending OrderState = iota
	Paid
	Shipped
	Delivered
)

func (s OrderState) String() string {
	switch s {
	case Pending:
		return "pending"
	case Paid:
		return "paid"
	case Shipped:
		return "shipped"
	case Delivered:
		return "delivered"
	default:
		return "unknown"
	}
}

// BadState is the same enum but with String() on the POINTER receiver only. A
// BadState value does NOT satisfy fmt.Stringer, so fmt prints the raw integer;
// only a *BadState formats as the label. Kept to demonstrate the trap.
type BadState int

const (
	BadPending BadState = iota
	BadPaid
	BadShipped
	BadDelivered
)

func (s *BadState) String() string {
	switch *s {
	case BadPending:
		return "pending"
	case BadPaid:
		return "paid"
	case BadShipped:
		return "shipped"
	case BadDelivered:
		return "delivered"
	default:
		return "unknown"
	}
}
```

### The runnable demo

The demo prints the correct type as a value (label appears), then the bad type as
a value (raw number, the trap) and as a pointer (label appears).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/orderstate"
)

func main() {
	good := orderstate.Shipped
	// Value Stringer fires for a value.
	fmt.Printf("good value: %v\n", good)

	bad := orderstate.BadShipped
	// Pointer-only Stringer does NOT fire for a value; it does for a pointer.
	fmt.Printf("bad value:  %v\n", bad)
	fmt.Printf("bad pointer: %v\n", &bad)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good value: shipped
bad value:  2
bad pointer: shipped
```

### Tests

The tests assert the value Stringer fires for a value, that the pointer-only
Stringer leaves a value's output as the raw number (pinning the trap), and that
the pointer form of the bad type does format.

Create `state_test.go`:

```go
package orderstate

import (
	"fmt"
	"testing"
)

func TestValueStringerFires(t *testing.T) {
	t.Parallel()
	if got := fmt.Sprint(Shipped); got != "shipped" {
		t.Fatalf("fmt.Sprint(Shipped) = %q, want shipped", got)
	}
	// Pointer form works too, because a value method is in *OrderState's set.
	s := Paid
	if got := fmt.Sprint(&s); got != "paid" {
		t.Fatalf("fmt.Sprint(&Paid) = %q, want paid", got)
	}
}

func TestPointerOnlyStringerTrap(t *testing.T) {
	t.Parallel()
	// A BadState VALUE does not satisfy fmt.Stringer, so fmt prints the raw int.
	if got := fmt.Sprint(BadShipped); got != "2" {
		t.Fatalf("fmt.Sprint(BadShipped) = %q, want raw \"2\" (the trap)", got)
	}
	// A *BadState does satisfy it and formats as the label.
	b := BadShipped
	if got := fmt.Sprint(&b); got != "shipped" {
		t.Fatalf("fmt.Sprint(&BadShipped) = %q, want shipped", got)
	}
}

func ExampleOrderState_String() {
	fmt.Println(Delivered)
	// Output: delivered
}
```

## Review

The type formats correctly when `String()` is in the value's method set. The
value-receiver `OrderState` proves it: `fmt.Sprint(Shipped)` returns `shipped`
whether you pass a value or a pointer. `BadState` pins the trap in the other
direction: `fmt.Sprint(BadShipped)` returns the raw `2` because a pointer-only
`String()` is not in the value's method set, so `fmt`'s runtime `Stringer` check
misses it and dumps the underlying integer.

The mistake this module exists to prevent is declaring `String()` (or `Error()`)
on the pointer receiver of a type you pass around by value — enums, small value
objects, domain states. Put those methods on the value receiver so both forms
format. The failure is silent: the code compiles and even works when you happen to
pass a pointer, so a test that asserts the value form is how you catch it. Run
`go vet` (its printf/stringer checks) and `go test -race`.

## Resources

- [`fmt.Stringer`](https://pkg.go.dev/fmt#Stringer) — the interface `fmt` checks at runtime for custom formatting.
- [`fmt` package overview](https://pkg.go.dev/fmt#hdr-Printing) — how operands satisfying Stringer/error are formatted, and the fallback otherwise.
- [Go Language Specification: Method sets](https://go.dev/ref/spec#Method_sets) — why a pointer-receiver method is absent from the value's method set.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-batch-update-slice-range-copy.md](09-batch-update-slice-range-copy.md)
