# Exercise 1: A Lock-Free Metrics Counter Proven Race-Free Under Concurrent Load

Every server has a hot path that bumps a counter on each request: requests
served, cache hits, bytes written. The counter is touched by every worker
goroutine at once, so it is the canonical place a data race hides. This exercise
builds that counter with `sync/atomic`, then proves it race-free by hammering it
with a thousand concurrent increments and a hundred concurrent reads under
`go test -race`.

This module is fully self-contained: its own `go mod init`, its own demo, its
own tests. Nothing here imports another exercise.

## What you'll build

```text
metrics/                    independent module: example.com/metrics
  go.mod                    go 1.26
  counter.go                type Counter (atomic.Int64); New, Inc, Add, Get
  cmd/
    demo/
      main.go               runs the concurrent path and prints the total
  counter_test.go           serial + 1000-goroutine Inc + 100-goroutine Get, all under -race
```

Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
Implement: a `Counter` backed by `atomic.Int64` with `Inc`, `Add`, and `Get`.
Test: serial correctness; 1000 concurrent `Inc` asserting `Get == 1000`; 100
concurrent `Get` on a primed counter asserting every read is valid; 10000 serial
increments.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metrics/cmd/demo
cd ~/go-exercises/metrics
go mod init example.com/metrics
```

### Why atomic and not a mutex here

The counter's entire state is a single 64-bit integer, and the only operation is
"add a delta and observe the result." That is exactly the shape `sync/atomic`
covers: `atomic.Int64.Add` is one indivisible read-modify-write, and
`atomic.Int64.Load` reads the current value with the acquire semantics that pair
with `Add`'s release. There is no compound invariant spanning two fields, so a
mutex would only add a critical section and contention with no benefit. This is
the first branch of the fix-by-access-pattern decision tree: a single machine
word with independent updates maps to `sync/atomic`.

The subtle correctness point is that atomicity of the operation is what makes the
final total exact. If the counter were a plain `int64` bumped with `c.value++`,
that increment compiles to load, add, store -- three steps that two goroutines
can interleave so both read the same old value and one increment is lost. Under
`-race` that plain increment is reported as a write-write data race. `Add(1)`
collapses the three steps into one hardware atomic, so a thousand concurrent
increments always sum to exactly a thousand, and the detector sees the accesses
as properly ordered.

`atomic.Int64` must not be copied after first use, so `Counter` is always passed
by pointer (`New` returns `*Counter`). Copying it would duplicate the internal
word and split the count.

Create `counter.go`:

```go
package metrics

import "sync/atomic"

// Counter is a concurrency-safe monotone counter for a hot server path
// (requests served, cache hits, bytes written). Its whole state is one 64-bit
// word, so it uses sync/atomic rather than a mutex. Do not copy a Counter after
// first use; pass it by pointer.
type Counter struct {
	value atomic.Int64
}

// New returns a Counter at zero.
func New() *Counter { return &Counter{} }

// Inc adds one and returns the new value.
func (c *Counter) Inc() int64 { return c.value.Add(1) }

// Add adds delta and returns the new value.
func (c *Counter) Add(delta int64) int64 { return c.value.Add(delta) }

// Get returns the current value.
func (c *Counter) Get() int64 { return c.value.Load() }
```

### The runnable demo

The demo drives the concurrent path so you can watch it complete: it launches a
thousand goroutines that each call `Inc`, waits for them, and prints the total.
Run it with `-race` and it stays silent (no report) and prints `1000`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/metrics"
)

func main() {
	c := metrics.New()

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

	fmt.Printf("requests served: %d\n", c.Get())
}
```

Run it:

```bash
go run -race ./cmd/demo
```

Expected output:

```text
requests served: 1000
```

### Tests

`TestCounterSerial` pins the basic contract: the first `Inc` returns 1 and `Get`
then returns 1. `TestCounterUnderConcurrentInc` is the main proof: it launches
1000 goroutines each calling `Inc`, waits, and asserts `Get() == 1000` -- an
exact total, which is only possible because `Add` is atomic. Under `-race` this
test creates the contention the detector needs; if the counter were a plain
`int64`, this is where the report would fire. `TestCounterConcurrentRead` primes
the counter to a known value and then runs 100 concurrent `Get` calls, asserting
each read is a valid monotone value in range -- pinning the "concurrent reads are
safe" contract. `TestCounterManySerialIncs` sanity-checks 10000 serial
increments sum exactly.

The `WaitGroup` matters for the assertion, not just for tidiness: `wg.Wait`
happens-after every `wg.Done`, so the final `Get` is ordered after all the
`Inc`s and observes their full effect. Without it the read could run before some
increments and the assertion would be meaningless.

Create `counter_test.go`:

```go
package metrics

import (
	"fmt"
	"sync"
	"testing"
)

func TestCounterSerial(t *testing.T) {
	t.Parallel()

	c := New()
	if got := c.Inc(); got != 1 {
		t.Fatalf("Inc() = %d, want 1", got)
	}
	if got := c.Get(); got != 1 {
		t.Fatalf("Get() = %d, want 1", got)
	}
}

func TestCounterUnderConcurrentInc(t *testing.T) {
	t.Parallel()

	c := New()
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
		t.Fatalf("Get() = %d, want %d", got, n)
	}
}

func TestCounterConcurrentRead(t *testing.T) {
	t.Parallel()

	c := New()
	const primed = 500
	for range primed {
		c.Inc()
	}

	const readers = 100
	var wg sync.WaitGroup
	errs := make([]int64, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := c.Get()
			if got != primed {
				errs[i] = got
			}
		}()
	}
	wg.Wait()

	for i, bad := range errs {
		if bad != 0 {
			t.Fatalf("reader %d saw Get() = %d, want %d", i, bad, primed)
		}
	}
}

func TestCounterManySerialIncs(t *testing.T) {
	t.Parallel()

	c := New()
	const n = 10000
	for range n {
		c.Inc()
	}
	if got := c.Get(); got != n {
		t.Fatalf("Get() = %d, want %d", got, n)
	}
}

func ExampleCounter() {
	c := New()
	c.Add(10)
	c.Inc()
	fmt.Println(c.Get())
	// Output: 11
}
```

## Review

The counter is correct when its total is an exact function of the increments
applied: 1000 concurrent `Inc`s sum to exactly 1000, and 100 concurrent readers
of a primed counter every see the same value with no torn read. The proof is
that `TestCounterUnderConcurrentInc` and `TestCounterConcurrentRead` pass under
`go test -race` -- the detector saw the contention and found no unordered write.
If you replaced `atomic.Int64` with a plain `int64` and `c.value++`, the
concurrent-increment test would both report a data race and (nondeterministically)
under-count.

The mistakes to avoid are structural. Do not copy a `Counter` value after first
use -- `atomic.Int64` carries state that must not be duplicated, which is why the
API is pointer-based. Do not drop the `WaitGroup`: the final `Get` is only a
valid assertion because `wg.Wait` orders it after every `Inc`. And do not treat a
single-goroutine `-race` run as evidence of concurrency safety; the thousand-way
hammer is what makes the test meaningful. Run `go test -count=1 -race ./...` to
confirm.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) -- `atomic.Int64.Add`, `atomic.Int64.Load`, and the "do not copy after first use" rule.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) -- the `Add`/`Done`/`Wait` happens-before ordering the final read relies on.
- [cmd/go: Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) -- `-race` and `-count` on `go test`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-diagnose-the-race-report.md](02-diagnose-the-race-report.md)
