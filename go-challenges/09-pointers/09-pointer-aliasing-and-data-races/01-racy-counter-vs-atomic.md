# Exercise 1: Fix a Racy Hit Counter with sync/atomic

Every backend keeps counters: requests served, cache hits, bytes written. The
naive `c.value++` on a shared counter is a data race, and under load it silently
loses increments. This module builds the racy counter and its `sync/atomic` fix,
then proves the fix is correct with 1000 concurrent goroutines under `-race`.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
hitcount/                  independent module: example.com/hitcount
  go.mod                   module example.com/hitcount
  hitcount.go              Counter (racy, teaching) + SafeCounter (atomic.Int64): Inc, Get, Store, Load
  cmd/
    demo/
      main.go              runnable demo: 1000 goroutines Inc a SafeCounter
  hitcount_test.go         1000-goroutine stress (-race), Store/Load round-trip table
```

- Files: `hitcount.go`, `cmd/demo/main.go`, `hitcount_test.go`.
- Implement: a racy `Counter` (non-atomic `value++`) kept as a teaching example, and a `SafeCounter` backed by `atomic.Int64` exposing `Inc() int64`, `Get() int64`, `Store(int64)`, `Load() int64`.
- Test: spin 1000 goroutines calling `Inc`, `WaitGroup.Wait`, assert `Get()==1000` under `-race`; a table test for `Store`/`Load` round-trip.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/09-pointer-aliasing-and-data-races/01-racy-counter-vs-atomic/cmd/demo
cd go-solutions/09-pointers/09-pointer-aliasing-and-data-races/01-racy-counter-vs-atomic
```

### Why `c.value++` is a race and `atomic.Int64.Add` is not

`c.value++` on a plain `int64` compiles to three steps: load the current value
into a register, add one, store it back. Those three steps are not atomic. Two
goroutines can both load 41, both add one to get 42, and both store 42 — one
increment is lost. Under 1000 concurrent goroutines this is not a rare corner case;
you routinely observe a final value well below 1000. Worse, per the Go memory
model the racy read-modify-write is undefined behavior, so "lost update" is only
the *mildest* possible outcome; a torn 64-bit value is also permitted.

`atomic.Int64.Add(1)` performs the load-add-store as a single indivisible hardware
operation (a lock-prefixed instruction or a load-linked/store-conditional loop), so
no other goroutine can interleave between the load and the store. It also
establishes the happens-before edge the memory model requires, so `Load` on another
goroutine sees a coherent value, never a torn one. `SafeCounter` embeds an
`atomic.Int64` value (not a pointer) and exposes it through methods; because the
struct now holds a live atomic, its methods take a `*SafeCounter` receiver and the
struct must never be copied after first use.

The racy `Counter` is kept deliberately, unexported-in-spirit as a teaching
artifact: it is what you must *not* ship. Its own concurrency test is not committed
here precisely because running `Counter.Inc` concurrently is undefined behavior;
the committed stress test targets `SafeCounter`, which passes cleanly under `-race`.

Create `hitcount.go`:

```go
package hitcount

import "sync/atomic"

// Counter is a deliberately racy hit counter kept as a teaching example of what
// NOT to ship. Inc performs a non-atomic read-modify-write, so concurrent callers
// lose updates and the access is a data race (undefined behavior). Do not use it
// from more than one goroutine.
type Counter struct {
	value int64
}

// Inc increments the counter. Racy under concurrency.
func (c *Counter) Inc() {
	c.value++
}

// Get returns the current value. Racy under concurrency.
func (c *Counter) Get() int64 {
	return c.value
}

// SafeCounter is a concurrency-safe hit counter backed by a single atomic word.
// Because it holds a live atomic.Int64, it must not be copied after first use;
// always use a *SafeCounter.
type SafeCounter struct {
	value atomic.Int64
}

// Inc atomically adds one and returns the new value.
func (c *SafeCounter) Inc() int64 {
	return c.value.Add(1)
}

// Get atomically loads the current value.
func (c *SafeCounter) Get() int64 {
	return c.value.Load()
}

// Store atomically replaces the value (used to reset or seed the counter).
func (c *SafeCounter) Store(v int64) {
	c.value.Store(v)
}

// Load is an alias for Get, matching the atomic vocabulary.
func (c *SafeCounter) Load() int64 {
	return c.value.Load()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/hitcount"
)

func main() {
	c := &hitcount.SafeCounter{}
	const n = 1000

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	fmt.Printf("after %d concurrent Inc: %d\n", n, c.Get())

	c.Store(0)
	fmt.Printf("after Store(0): %d\n", c.Get())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after 1000 concurrent Inc: 1000
after Store(0): 0
```

### Tests

`TestSafeCounterConcurrentInc` is the load-bearing test: 1000 goroutines each call
`Inc` once, `WaitGroup.Wait` establishes the happens-before edge for the final
read, and the assertion pins `Get()==1000`. Run under `-race` it fails the gate if
any synchronization is dropped. `TestStoreLoadRoundTrip` is a table test proving
`Store` then `Load` round-trips a range of values, including negatives and the
64-bit extremes.

Create `hitcount_test.go`:

```go
package hitcount

import (
	"fmt"
	"math"
	"sync"
	"testing"
)

func TestSafeCounterConcurrentInc(t *testing.T) {
	t.Parallel()

	c := &SafeCounter{}
	const n = 1000

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()

	if got := c.Get(); got != n {
		t.Fatalf("Get() = %d after %d concurrent Inc; want %d", got, n, n)
	}
}

func TestStoreLoadRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		v    int64
	}{
		{"zero", 0},
		{"positive", 42},
		{"negative", -7},
		{"max", math.MaxInt64},
		{"min", math.MinInt64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &SafeCounter{}
			c.Store(tc.v)
			if got := c.Load(); got != tc.v {
				t.Fatalf("Load() = %d after Store(%d); want %d", got, tc.v, tc.v)
			}
		})
	}
}

func TestIncReturnsNewValue(t *testing.T) {
	t.Parallel()

	c := &SafeCounter{}
	if got := c.Inc(); got != 1 {
		t.Fatalf("first Inc() = %d; want 1", got)
	}
	if got := c.Inc(); got != 2 {
		t.Fatalf("second Inc() = %d; want 2", got)
	}
}

func Example() {
	c := &SafeCounter{}
	c.Inc()
	c.Inc()
	c.Inc()
	fmt.Println(c.Get())
	// Output: 3
}
```

## Review

The counter is correct when the final value equals the number of increments under
concurrency, and the `-race` run is clean. The mistakes to avoid are the two
directions of the primitive-choice error. Do not wrap this single `int64` in a
`sync.Mutex` — a mutex is heavier than an atomic for one word and buys nothing here.
And do not "fix" the racy `Counter` by making `Get` alone atomic while leaving
`Inc` a plain `value++`: the read-modify-write itself is the race, so both the
mutation and the read must go through the atomic. The `WaitGroup.Wait` in the test
is not decoration — it is the happens-before edge that lets the final `Get` legally
observe all 1000 increments; without it, even the atomic reads would be unordered
relative to the goroutines still running. Run `go test -race` to confirm.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `Int64.Add`, `Load`, `Store`, and the "must not be copied" rule.
- [The Go Memory Model](https://go.dev/ref/mem) — why a non-atomic `value++` under concurrency is undefined behavior.
- [Go Blog: Introducing the Go Race Detector](https://go.dev/blog/race-detector) — what `-race` observes and reports.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pointer-aliasing-fundamentals.md](02-pointer-aliasing-fundamentals.md)
