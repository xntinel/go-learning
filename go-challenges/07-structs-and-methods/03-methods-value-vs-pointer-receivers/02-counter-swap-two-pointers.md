# Exercise 2: Swapping State Across Two Pointer Receivers

A pointer-receiver method can mutate more than its own receiver: it can also
mutate a pointer passed as an argument. This module builds a `Swap` method that
exchanges the values of two counters in place — the kind of move a metrics
subsystem makes when it rotates a live counter with a fresh one at the start of a
scrape window — and shows why the constructor's non-nil guarantee lets callers
skip a nil check on the receiver while the argument still needs one.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
swapcounter/               independent module: example.com/swapcounter
  go.mod
  counter.go               type Counter; New, Inc, Add, Get, Swap(other *Counter)
  cmd/
    demo/
      main.go              rotate a live counter with a fresh one via Swap
  counter_test.go          swap exchanges values; nil argument is a no-op; no allocation
```

Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
Implement: `Counter` with pointer receivers, plus `Swap(other *Counter)` that exchanges both values with a `nil`-argument guard that no-ops.
Test: `Swap` exchanges the two values; `Swap(nil)` leaves the receiver unchanged and does not panic; `Swap` performs zero heap allocations.
Verify: `go test -count=1 -race ./...`

### One method, two mutations

`Swap` has a pointer receiver (`c *Counter`) and a pointer parameter
(`other *Counter`). Both are addresses, so a single tuple assignment
—`c.value, other.value = other.value, c.value`— writes through both pointers and
exchanges the stored values with no temporary and no copy. Evaluate the
right-hand side first, then assign left-to-right: Go guarantees the whole
right-hand tuple is evaluated before any assignment, which is what makes the swap
correct without an explicit temp variable.

The nil handling splits cleanly by role. The *receiver* is guaranteed non-nil
because the only way to get a `*Counter` is `New`, which returns `&Counter{}`;
callers never have to check it. The *argument*, however, comes from the caller
and could be nil, so `Swap` guards it: `if other == nil { return }`. This is the
practical payoff of a constructor that returns `*T` and forbids nil — the guard
burden moves to the boundaries where untrusted values actually enter, not to
every method body.

Create `counter.go`:

```go
package swapcounter

// Counter is a mutable in-memory counter. All methods use pointer receivers.
type Counter struct {
	value int64
}

// New returns a non-nil counter, so callers never nil-check the receiver.
func New() *Counter {
	return &Counter{}
}

// Inc adds one.
func (c *Counter) Inc() {
	c.value++
}

// Add increases the counter by n (n may be any value here; validation is the
// job of Exercise 1's Add).
func (c *Counter) Add(n int64) {
	c.value += n
}

// Get reports the current value.
func (c *Counter) Get() int64 {
	return c.value
}

// Swap exchanges the receiver's value with other's value. Because both are
// pointers, one tuple assignment mutates both. A nil argument is a no-op: the
// receiver is left unchanged and no panic occurs.
func (c *Counter) Swap(other *Counter) {
	if other == nil {
		return
	}
	c.value, other.value = other.value, c.value
}
```

### The runnable demo

The demo rotates a live counter (holding 42 requests) with a fresh zeroed
counter, the way a scrape cycle hands the old value off to be reported and starts
counting the next window from zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/swapcounter"
)

func main() {
	live := swapcounter.New()
	live.Add(42)

	fresh := swapcounter.New()

	fmt.Printf("before rotate: live=%d fresh=%d\n", live.Get(), fresh.Get())
	live.Swap(fresh)
	fmt.Printf("after rotate:  live=%d fresh=%d\n", live.Get(), fresh.Get())

	// A nil rotation target is safely ignored.
	live.Swap(nil)
	fmt.Printf("after nil swap: live=%d\n", live.Get())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before rotate: live=42 fresh=0
after rotate:  live=0 fresh=42
after nil swap: live=0
```

### Tests

`TestSwapExchangesValues` builds `a=3, b=0`, swaps, and asserts the values
crossed. `TestSwapNilArgumentIsNoOp` proves the guard: a nil argument neither
panics nor changes the receiver. `TestSwapDoesNotAllocate` uses
`testing.AllocsPerRun` to pin that the swap is a pure in-place tuple assignment —
zero heap allocations — which is the whole point of mutating through pointers
rather than returning new values.

Create `counter_test.go`:

```go
package swapcounter

import "testing"

func TestSwapExchangesValues(t *testing.T) {
	t.Parallel()

	a := New()
	a.Add(3)
	b := New()

	a.Swap(b)

	if got := a.Get(); got != 0 {
		t.Fatalf("a.Get() = %d, want 0", got)
	}
	if got := b.Get(); got != 3 {
		t.Fatalf("b.Get() = %d, want 3", got)
	}
}

func TestSwapNilArgumentIsNoOp(t *testing.T) {
	t.Parallel()

	a := New()
	a.Add(7)

	a.Swap(nil) // must not panic

	if got := a.Get(); got != 7 {
		t.Fatalf("a.Get() = %d after Swap(nil), want unchanged 7", got)
	}
}

func TestSwapDoesNotAllocate(t *testing.T) {
	// No t.Parallel: testing.AllocsPerRun must not run in a parallel test.
	a := New()
	a.Add(1)
	b := New()

	allocs := testing.AllocsPerRun(1000, func() {
		a.Swap(b)
	})
	if allocs != 0 {
		t.Fatalf("Swap allocated %v times per run, want 0", allocs)
	}
}
```

## Review

`Swap` is correct when the two counters exchange values and neither a nil
argument nor the swap itself introduces a panic or a heap allocation. The design
lesson is the asymmetry in nil handling: the receiver is trusted because `New`
guarantees non-nil, while the argument is untrusted and guarded. Do not be
tempted to "fix" that by adding a receiver nil check to every method — that would
paper over a real bug (a nil `*Counter` reaching a method means someone
bypassed `New`) instead of surfacing it. `TestSwapDoesNotAllocate` documents that
mutating through pointers is allocation-free, the concrete efficiency reason to
prefer in-place mutation over returning new copies for a mutable type.

## Resources

- [Go Spec: Assignments](https://go.dev/ref/spec#Assignments) — tuple assignment evaluates the whole right-hand side before assigning.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring per-call heap allocations in a test.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — mutating through pointer receivers.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-mutable-metrics-counter.md](01-mutable-metrics-counter.md) | Next: [03-concurrent-safe-counter.md](03-concurrent-safe-counter.md)
