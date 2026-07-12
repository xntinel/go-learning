# Exercise 1: A Concurrency-Safe Request Counter Proven Under -race

Almost every backend process keeps in-memory counters: requests served, cache
hits, retries attempted, items flushed. When several goroutines increment the
same counter — one per in-flight request — the counter must be safe under
concurrent access, and your test must actually *exercise* that concurrency so
`-race` has something to observe. This module builds that counter on
`atomic.Int64` and proves it with a fan-out test.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
counter/                    independent module: example.com/counter
  go.mod
  counter.go                type Counter (atomic.Int64); New, Inc, Get
  cmd/
    demo/
      main.go               runnable demo: fan out 500 Inc calls, print total
  counter_test.go           serial contract + 1000-goroutine parallel test
```

Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
Implement: a `Counter` with `New`, `Inc() int64`, and `Get() int64`, backed by
`sync/atomic.Int64`.
Test: a serial contract, a 1000-goroutine fan-out asserting `Get()==1000`, a
parallel subtest fan-out, and a 10,000-increment serial pin.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/14-parallel-tests/01-parallel-safe-counter/cmd/demo
cd go-solutions/12-testing-ecosystem/14-parallel-tests/01-parallel-safe-counter
```

### Why atomic and not a mutex

A counter has exactly one operation that mutates state — add one — and one that
reads it. That is the textbook shape for a lock-free atomic: `atomic.Int64.Add`
performs a read-modify-write as a single indivisible hardware operation, and
`atomic.Int64.Load` reads the current value with the memory ordering guarantees
that make the read safe against concurrent adds. A `sync.Mutex` would also be
correct, but it serializes every increment through a lock and is heavier than a
single atomic instruction for this one-word case. For a hot request counter the
atomic is the right default.

The subtle point this exercise makes concrete is that `t.Parallel()` has nothing
to do with whether the counter's *internal* concurrency is exercised. The 1000
goroutines inside `TestCounterParallel` run concurrently because they are
goroutines, not because the test is parallel. `t.Parallel()` on that test only
lets it overlap with *sibling* tests. So the race detector's chance to observe a
race comes from the fan-out `go func()` calls, and it would observe one even if
the test were serial. Marking the test parallel is about suite throughput; the
`sync.WaitGroup` fan-out is about creating the concurrency to test.

Here is the proof that the test genuinely observes concurrency: if you replace
the `atomic.Int64` field with a plain `int64` and use `c.value++` in `Inc`, the
1000-goroutine test fails under `go test -race` with a reported data race on that
field. The fact that the atomic version passes `-race` while the plain-int
version fails it is what tells you the test is real, not that it merely happens to
produce the right number.

Create `counter.go`:

```go
package counter

import "sync/atomic"

// Counter is a concurrency-safe monotonic counter suitable for in-process
// request or metric counting. The zero value is not usable; call New.
type Counter struct {
	value atomic.Int64
}

// New returns a Counter starting at zero.
func New() *Counter {
	return &Counter{}
}

// Inc atomically adds one and returns the new value.
func (c *Counter) Inc() int64 {
	return c.value.Add(1)
}

// Get returns the current value with a race-free load.
func (c *Counter) Get() int64 {
	return c.value.Load()
}
```

### The runnable demo

The demo fans out 500 increments across goroutines, waits, and prints the total —
a miniature of a handler counting concurrent requests.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/counter"
)

func main() {
	c := counter.New()
	const n = 500
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc()
		}()
	}
	wg.Wait()
	fmt.Printf("requests counted: %d\n", c.Get())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests counted: 500
```

### Tests

`TestCounterSerial` pins the return-value contract: consecutive `Inc` calls
return 1 then 2. `TestCounterParallel` is the heart of the module — 1000
goroutines each call `Inc` once, a `sync.WaitGroup` awaits them, and the final
`Get()` must be exactly 1000; run under `-race`, this both checks correctness and
gives the detector concurrent accesses to instrument. `TestCounterParallelSubtests`
shows subtests can each be parallel while owning their *own* counter — the
isolation rule in miniature. `TestCounterManySerialIncs` pins the serial contract
at 10,000.

Create `counter_test.go`:

```go
package counter

import (
	"sync"
	"testing"
)

func TestCounterSerial(t *testing.T) {
	t.Parallel()

	c := New()
	if got := c.Inc(); got != 1 {
		t.Fatalf("Inc = %d, want 1", got)
	}
	if got := c.Inc(); got != 2 {
		t.Fatalf("Inc = %d, want 2", got)
	}
}

func TestCounterParallel(t *testing.T) {
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
		t.Fatalf("Get = %d, want %d", got, n)
	}
}

func TestCounterParallelSubtests(t *testing.T) {
	t.Parallel()

	for i := range 5 {
		t.Run("independent_counter", func(t *testing.T) {
			t.Parallel()
			// Each subtest owns its counter: no shared state across the
			// parallel group, so the fan-out below cannot race a sibling.
			c := New()
			var wg sync.WaitGroup
			for range 100 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					c.Inc()
				}()
			}
			wg.Wait()
			if got := c.Get(); got != 100 {
				t.Fatalf("subtest %d: Get = %d, want 100", i, got)
			}
		})
	}
}

func TestCounterManySerialIncs(t *testing.T) {
	t.Parallel()

	c := New()
	const n = 10_000
	for range n {
		c.Inc()
	}
	if got := c.Get(); got != n {
		t.Fatalf("Get = %d, want %d", got, n)
	}
}
```

## Review

The counter is correct when `Inc` is a single atomic add and `Get` a single
atomic load, so no interleaving of increments can lose an update. The evidence
that the test proves this — rather than merely producing the right total by luck
— is the `-race` behavior: swap `atomic.Int64` for a plain `int64` with `c.value++`
and the 1000-goroutine test reports a data race, while the atomic version stays
clean. That is the whole point of running `go test -race`.

The trap to avoid is reading `t.Parallel()` as the thing that creates
concurrency. It does not; the `sync.WaitGroup` fan-out does. `t.Parallel()` only
lets the whole test overlap with siblings, which is a throughput decision. The
other trap is sharing one counter across the parallel subtests — that would be a
genuine shared-state race; `TestCounterParallelSubtests` deliberately gives each
subtest its own `New()`.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `Int64.Add` and `Int64.Load` and their memory-ordering guarantees.
- [`testing.T.Parallel`](https://pkg.go.dev/testing#T.Parallel) — the pause/resume model.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` observes and its probabilistic nature.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-parallel-table-http-validation.md](02-parallel-table-http-validation.md)
