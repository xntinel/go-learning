# Exercise 1: Atomic Counter Service: Pointer Receivers and a Mutating Interface

A request-metrics counter shared across every handler goroutine is the canonical
place where pointer receivers are not a style choice but a correctness
requirement. This module builds that counter on `sync/atomic.Int64` with all
pointer receivers and a `Settable` interface that only `*Counter` satisfies, and
proves the method-set rule with a compile-time contract and a `-race` test.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports another exercise.

## What you'll build

```text
countersvc/                 independent module: example.com/countersvc
  go.mod                    go 1.25
  counter.go                type Counter (atomic.Int64, pointer receivers); Settable interface
  cmd/
    demo/
      main.go               concurrent increments then a Set
  counter_test.go           receiver rules, interface satisfaction, -race concurrency
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: a `Counter` with pointer-receiver `Inc`, `Add`, `Set`, `Get`, `CompareAndSwap`, and a `Settable` interface (`Set`, `Get`) satisfied only by `*Counter`.
- Test: pointer-receiver methods mutate shared state, `Set` replaces the value, `var _ Settable = (*Counter)(nil)` compiles while a `Counter` value is documented as rejected, and 1000 concurrent `Inc` calls sum correctly under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why every method takes a pointer receiver

`Counter` wraps a `sync/atomic.Int64`. Two independent reasons force pointer
receivers on it. First, `atomic.Int64` must not be copied after first use — the
`go vet` copylocks/noCopy analysis treats atomic types as non-copyable, and a
value receiver would copy the whole `Counter` (and the atomic inside it) on every
call, giving each call its own private counter. Second, the methods mutate: `Inc`,
`Add`, `Set`, and `CompareAndSwap` all change the stored value and the change must
persist. Either reason alone would require a pointer receiver; together they make
it non-negotiable. Following the convention, `Get` — which only reads — is a
pointer receiver too, so the method set is uniform and `*Counter` implements every
interface consistently.

The `Settable` interface requires `Set` and `Get`, both of which are
pointer-receiver methods on `Counter`. Therefore only `*Counter` is in the method
set that satisfies `Settable`; a `Counter` *value* is not. The compile-time
contract `var _ Settable = (*Counter)(nil)` documents and enforces this at the
definition site. The mirror assertion — `var _ Settable = Counter{}` — would fail
to compile, because `Counter{}`'s method set (value methods only) is empty here;
that line is shown as a comment, not compiled, precisely because it must not
build.

Create `counter.go`:

```go
// counter.go
package countersvc

import "sync/atomic"

// Counter is a concurrency-safe request-metrics counter shared across handler
// goroutines. It wraps atomic.Int64, which must not be copied, so every method
// has a pointer receiver.
type Counter struct {
	value atomic.Int64
}

// Settable is the write side of the counter. Because Set has a pointer receiver
// on Counter, only *Counter satisfies Settable; a Counter value does not.
type Settable interface {
	Set(n int64)
	Get() int64
}

// Compile-time contract: *Counter satisfies Settable. The value form
//
//	var _ Settable = Counter{}
//
// would NOT compile, because Set is not in Counter's (value) method set.
var _ Settable = (*Counter)(nil)

// New returns a Counter initialized to zero.
func New() *Counter {
	return &Counter{}
}

// Inc atomically increments the counter and returns the new value.
func (c *Counter) Inc() int64 {
	return c.value.Add(1)
}

// Add atomically adds n and returns the new value.
func (c *Counter) Add(n int64) int64 {
	return c.value.Add(n)
}

// Set atomically replaces the stored value.
func (c *Counter) Set(n int64) {
	c.value.Store(n)
}

// Get atomically loads the stored value.
func (c *Counter) Get() int64 {
	return c.value.Load()
}

// CompareAndSwap atomically sets the value to newVal if it currently equals old,
// reporting whether the swap happened. Useful for a one-shot latch.
func (c *Counter) CompareAndSwap(old, newVal int64) bool {
	return c.value.CompareAndSwap(old, newVal)
}
```

### The runnable demo

The demo launches 1000 goroutines that each increment the shared counter, waits
for them with a `sync.WaitGroup`, prints the total, then demonstrates `Set` and
`CompareAndSwap` through the `Settable` interface.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"sync"

	"example.com/countersvc"
)

func main() {
	c := countersvc.New()

	var wg sync.WaitGroup
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	fmt.Printf("after 1000 concurrent Inc: %d\n", c.Get())

	var s countersvc.Settable = c
	s.Set(42)
	fmt.Printf("after Set(42): %d\n", s.Get())

	ok := c.CompareAndSwap(42, 100)
	fmt.Printf("CompareAndSwap(42,100)=%v -> %d\n", ok, c.Get())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after 1000 concurrent Inc: 1000
after Set(42): 42
CompareAndSwap(42,100)=true -> 100
```

### Tests

The tests pin the receiver rules and interface satisfaction. `TestConcurrentInc`
is the hardened replacement for the original channel-based test: a `sync.WaitGroup`
fans out 1000 increments, and under `-race` it proves the atomic actually
serializes the writes. `TestPointerSatisfiesSettable` assigns `*Counter` to a
`Settable` variable and drives it. `TestValueMethodSet` documents, in a comment,
that a `Counter` value cannot be assigned to `Settable`, and exercises the value's
own zero behavior.

Create `counter_test.go`:

```go
// counter_test.go
package countersvc

import (
	"fmt"
	"sync"
	"testing"
)

func TestPointerReceiverMethodsMutate(t *testing.T) {
	t.Parallel()

	c := New()
	if got := c.Inc(); got != 1 {
		t.Fatalf("Inc() = %d, want 1", got)
	}
	if got := c.Add(10); got != 11 {
		t.Fatalf("Add(10) = %d, want 11", got)
	}
	if got := c.Get(); got != 11 {
		t.Fatalf("Get() = %d, want 11", got)
	}
}

func TestSetReplacesValue(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set(42)
	if got := c.Get(); got != 42 {
		t.Fatalf("Get() = %d, want 42", got)
	}
}

func TestCompareAndSwap(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set(5)
	if !c.CompareAndSwap(5, 9) {
		t.Fatal("CompareAndSwap(5,9) = false, want true")
	}
	if c.CompareAndSwap(5, 12) {
		t.Fatal("CompareAndSwap(5,12) = true after value moved to 9, want false")
	}
	if got := c.Get(); got != 9 {
		t.Fatalf("Get() = %d, want 9", got)
	}
}

func TestPointerSatisfiesSettable(t *testing.T) {
	t.Parallel()

	// *Counter satisfies Settable; the value form does not compile:
	//	var _ Settable = Counter{}
	var s Settable = New()
	s.Set(100)
	if got := s.Get(); got != 100 {
		t.Fatalf("Get() = %d, want 100", got)
	}
}

func TestValueMethodSetZero(t *testing.T) {
	t.Parallel()

	// A Counter value has an empty method set for Settable's purposes, but the
	// methods are still callable directly on an addressable variable.
	var c Counter
	if got := c.Get(); got != 0 {
		t.Fatalf("zero Counter Get() = %d, want 0", got)
	}
}

func TestConcurrentInc(t *testing.T) {
	t.Parallel()

	c := New()
	var wg sync.WaitGroup
	for range 1000 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	if got := c.Get(); got != 1000 {
		t.Fatalf("after 1000 concurrent Inc, Get() = %d, want 1000", got)
	}
}

func ExampleCounter() {
	c := New()
	c.Add(3)
	c.Inc()
	fmt.Println(c.Get())
	// Output: 4
}
```

## Review

The counter is correct when every mutation goes through the atomic and no method
copies the receiver. The compile-time contract `var _ Settable = (*Counter)(nil)`
is doing real work: if someone "simplified" a method to a value receiver, either
the contract line or `go vet` copylocks would fail immediately at the definition,
not far away at a call site. `TestConcurrentInc` under `-race` is the proof that
the atomic — not luck — serializes the 1000 writes; drop the atomic for a plain
`int64` and the race detector flags it at once. The one rule to internalize: a
type that owns an `atomic.Int64` (or a mutex) must be used and passed as `*T`
only, and all its methods take pointer receivers.

## Resources

- [sync/atomic: Int64](https://pkg.go.dev/sync/atomic#Int64) — `Add`, `Load`, `Store`, `CompareAndSwap`.
- [Go Specification: Method sets](https://go.dev/ref/spec#Method_sets) — why `*T` includes pointer methods and `T` does not.
- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — the "if one method needs a pointer, all do" convention.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-read-only-value-receiver-interface.md](02-read-only-value-receiver-interface.md)
