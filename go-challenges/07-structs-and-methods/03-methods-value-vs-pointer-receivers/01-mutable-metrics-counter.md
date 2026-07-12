# Exercise 1: A Mutable In-Memory Request Counter (Pointer Receivers)

The smallest piece of production state a service keeps is a counter: requests
handled, errors seen, cache hits. This module builds that counter as an
in-memory metric the way HTTP middleware would use it, and in doing so pins the
baseline rule of this lesson: a method that must mutate the receiver needs a
pointer receiver, and the constructor of a mutable type returns a pointer.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
counter/                   independent module: example.com/counter
  go.mod
  counter.go               type Counter; New() *Counter; Inc, Add, Get, Reset (pointer receivers)
  cmd/
    demo/
      main.go              middleware-style demo: count requests, then reset
  counter_test.go          table + property tests, -race safe
```

Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
Implement: `Counter` with `New() *Counter`, `Inc()`, `Add(n int64) error`, `Get() int64`, `Reset()`, all pointer receivers; `Add` rejects a non-positive step with `ErrInvalidStep`.
Test: `New` starts at zero, `Inc` and `Add` accumulate, `Add(0)`/`Add(-1)` return `ErrInvalidStep` via `errors.Is`, `Reset` zeroes, and a value copy is independent of the original.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/03-methods-value-vs-pointer-receivers/01-mutable-metrics-counter/cmd/demo
cd go-solutions/07-structs-and-methods/03-methods-value-vs-pointer-receivers/01-mutable-metrics-counter
```

### Why every method is a pointer receiver

The counter exists to be mutated: middleware calls `Inc()` on each request and
the change must be visible to the next reader. If `Inc` were declared
`func (c Counter) Inc()`, it would increment a *copy* evaluated at call time and
throw it away — the code would compile, run without error, and never count
anything. That silent-loss failure is exactly what a pointer receiver prevents:
`func (c *Counter) Inc()` operates on the original.

Because `Inc`, `Add`, and `Reset` mutate, the consistency rule says every method
on the type — including the read-only `Get` — uses a pointer receiver too. And
because the type is mutable, `New` returns `*Counter`, not `Counter`: a value
return would hand callers a copy they could not usefully mutate.

`Add` guards its input. A metric step of zero or negative is a caller bug, not a
silent no-op, so `Add` returns the package sentinel `ErrInvalidStep` wrapped
context-free (the sentinel itself), and callers match it with `errors.Is`. The
counter's underlying value is `int64` because request counts overflow a 32-bit
integer on a busy service faster than you would like.

Create `counter.go`:

```go
package counter

import "errors"

// ErrInvalidStep is returned by Add when the step is not strictly positive.
var ErrInvalidStep = errors.New("counter: step must be positive")

// Counter is a mutable in-memory metric. Every method uses a pointer receiver
// because the counter is mutated in place and callers share one instance.
type Counter struct {
	value int64
}

// New returns a ready-to-use counter starting at zero. It returns *Counter so
// callers can invoke the mutating methods on a single shared identity.
func New() *Counter {
	return &Counter{}
}

// Inc adds one. This is the hot path a middleware calls per request.
func (c *Counter) Inc() {
	c.value++
}

// Add increases the counter by n, rejecting a non-positive step.
func (c *Counter) Add(n int64) error {
	if n <= 0 {
		return ErrInvalidStep
	}
	c.value += n
	return nil
}

// Get reports the current value.
func (c *Counter) Get() int64 {
	return c.value
}

// Reset returns the counter to zero (e.g. at the start of a scrape window).
func (c *Counter) Reset() {
	c.value = 0
}
```

### The runnable demo

The demo plays the middleware role: it counts a handful of handled requests, a
batch, reads the total, then resets for the next window.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/counter"
)

func main() {
	requests := counter.New()

	for range 3 {
		requests.Inc()
	}
	if err := requests.Add(10); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("requests handled: %d\n", requests.Get())

	if err := requests.Add(0); err != nil {
		fmt.Printf("rejected bad step: %v\n", err)
	}

	requests.Reset()
	fmt.Printf("after reset: %d\n", requests.Get())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests handled: 13
rejected bad step: counter: step must be positive
after reset: 0
```

### Tests

The table cases cover the ordinary accumulation and the error path. The property
test `TestValueCopyIsIndependent` is the one that pins the receiver semantics:
because the test is in the same package it can dereference the pointer and take a
plain value copy (`snapshot := *c`), then mutate the original and prove the
snapshot did not move. That is the observable meaning of "a value is a copy": the
snapshot froze at the moment it was taken.

Create `counter_test.go`:

```go
package counter

import (
	"errors"
	"testing"
)

func TestNewCounterStartsAtZero(t *testing.T) {
	t.Parallel()

	c := New()
	if got := c.Get(); got != 0 {
		t.Fatalf("Get() = %d, want 0", got)
	}
}

func TestIncIncrementsByOne(t *testing.T) {
	t.Parallel()

	c := New()
	c.Inc()
	c.Inc()
	c.Inc()
	if got := c.Get(); got != 3 {
		t.Fatalf("Get() = %d, want 3", got)
	}
}

func TestAddIncrementsByN(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		add  []int64
		want int64
	}{
		{"single", []int64{5}, 5},
		{"multiple", []int64{5, 10, 1}, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := New()
			for _, n := range tc.add {
				if err := c.Add(n); err != nil {
					t.Fatalf("Add(%d) unexpected error: %v", n, err)
				}
			}
			if got := c.Get(); got != tc.want {
				t.Fatalf("Get() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAddRejectsNonPositive(t *testing.T) {
	t.Parallel()

	for _, n := range []int64{0, -1, -100} {
		c := New()
		if err := c.Add(n); !errors.Is(err, ErrInvalidStep) {
			t.Fatalf("Add(%d) err = %v, want ErrInvalidStep", n, err)
		}
		if got := c.Get(); got != 0 {
			t.Fatalf("Add(%d) mutated the counter to %d; want unchanged 0", n, got)
		}
	}
}

func TestResetReturnsToZero(t *testing.T) {
	t.Parallel()

	c := New()
	c.Inc()
	c.Inc()
	c.Reset()
	if got := c.Get(); got != 0 {
		t.Fatalf("Get() = %d, want 0", got)
	}
}

func TestValueCopyIsIndependent(t *testing.T) {
	t.Parallel()

	c := New()
	if err := c.Add(2); err != nil {
		t.Fatal(err)
	}

	snapshot := *c // a plain value copy of the Counter, frozen at value 2
	c.Inc()        // mutates the original only

	if got := snapshot.Get(); got != 2 {
		t.Fatalf("snapshot moved to %d; a value copy must be independent", got)
	}
	if got := c.Get(); got != 3 {
		t.Fatalf("original Get() = %d, want 3", got)
	}
}
```

## Review

The counter is correct when a mutation made through one `*Counter` is visible to
every holder of that pointer, and when a plain value copy of it is frozen and
independent — which is precisely what `TestValueCopyIsIndependent` asserts. If
you had written any mutator with a value receiver, `Inc` would update a
throwaway copy and `TestIncIncrementsByOne` would read back zero. The error path
matters too: `Add` returns the sentinel `ErrInvalidStep` (matched with
`errors.Is`, never `==` on a formatted string) and leaves the counter untouched
on rejection, which `TestAddRejectsNonPositive` checks explicitly. Run
`go test -race` — even though this exercise is single-goroutine, the race build
confirms there is no accidental shared-copy aliasing, and the next exercises add
real concurrency.

## Resources

- [Go Spec: Method declarations](https://go.dev/ref/spec#Method_declarations) — how a receiver binds to a method.
- [Effective Go: Pointers vs. Values](https://go.dev/doc/effective_go#pointers_vs_values) — when a method needs a pointer receiver.
- [Go Code Review Comments: Receiver Type](https://go.dev/wiki/CodeReviewComments#receiver-type) — the consistency rule this counter follows.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-counter-swap-two-pointers.md](02-counter-swap-two-pointers.md)
