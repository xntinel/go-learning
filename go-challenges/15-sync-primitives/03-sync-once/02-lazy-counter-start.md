# Exercise 2: Start-once worker counter with atomic proof of single start

A worker whose one-time "started" transition is guarded by `sync.Once` while its
ordinary counting stays lock-free with atomics. This is the everyday shape of a
background component: a single start/warm-up you must never run twice, wrapped
around hot-path operations that must not take a lock. The exercise proves, with an
atomic counter incremented inside the closure, that the start body ran exactly
once under heavy concurrency.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
lazy-counter-start/           module: example.com/lazy-counter-start
  go.mod
  counter.go                  type LazyCounter; Start, Started, Runs, Inc, Value
  cmd/
    demo/
      main.go                 runnable demo: start, increment, read
  counter_test.go             200-goroutine start-once, concurrent Inc totals
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`.
- Implement: a `LazyCounter` with `sync.Once`, `atomic.Bool` started, `atomic.Int64` count, and an `atomic.Int64` runs proving how many times the start body executed; `Start()`, `Started()`, `Runs()`, `Inc()`, `Value()`.
- Test: 200 goroutines all call `Start()`; assert `Started()` is true and `Runs()` is exactly 1; N goroutines each `Inc()` M times and assert `Value() == N*M`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/03-sync-once/02-lazy-counter-start/cmd/demo
cd go-solutions/15-sync-primitives/03-sync-once/02-lazy-counter-start
```

### Two kinds of state, two kinds of guard

This artifact deliberately separates two concerns that beginners often conflate.
The `started` transition happens once and must never repeat — that is `Once`'s
job. The `count` is mutated on the hot path by many goroutines and wants no lock
at all — that is an `atomic.Int64`'s job. Using a `Once` to guard the counter, or
an atomic to guard the one-time start, would be the wrong tool in each case: an
atomic cannot express "run this side-effecting body exactly once," and a `Once`
cannot express "add one, cheaply, a million times."

The `runs` field is the proof. It is incremented *inside* the `Do` closure, so its
final value is exactly the number of times the closure body executed. Under 200
goroutines all calling `Start`, a correct `Once` yields `runs == 1`. If it were 2,
the `Once` had been copied or misused. `started` is a separate `atomic.Bool` that
the closure `Store`s to true; `Started()` reads it. We keep them separate so that
`Started()` is a lock-free read on the hot path even though the transition it
reports was guarded by `Once`.

Create `counter.go`:

```go
package counter

import (
	"sync"
	"sync/atomic"
)

// LazyCounter starts exactly once and then counts lock-free. Start is guarded by
// sync.Once; Inc/Value are plain atomics safe for concurrent use. Hold it behind
// a pointer: a copy would carry a zeroed Once and re-run Start.
type LazyCounter struct {
	once    sync.Once
	started atomic.Bool
	runs    atomic.Int64
	count   atomic.Int64
}

// Start runs the one-time start body exactly once across all goroutines.
func (c *LazyCounter) Start() {
	c.once.Do(func() {
		c.runs.Add(1)
		c.started.Store(true)
	})
}

// Started reports whether Start has completed.
func (c *LazyCounter) Started() bool {
	return c.started.Load()
}

// Runs reports how many times the start body executed; it must be 0 or 1.
func (c *LazyCounter) Runs() int64 {
	return c.runs.Load()
}

// Inc adds one to the counter. Safe for concurrent use without a lock.
func (c *LazyCounter) Inc() {
	c.count.Add(1)
}

// Value reports the current count.
func (c *LazyCounter) Value() int64 {
	return c.count.Load()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lazy-counter-start"
)

func main() {
	c := &counter.LazyCounter{}
	fmt.Println("started before:", c.Started())

	c.Start()
	c.Start() // second call is a no-op
	fmt.Println("started after:", c.Started())
	fmt.Println("start body runs:", c.Runs())

	for range 5 {
		c.Inc()
	}
	fmt.Println("value:", c.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
started before: false
started after: true
start body runs: 1
value: 5
```

### Tests

`TestStartsExactlyOnce` fans 200 goroutines at `Start` and asserts both that the
component ended up started and that the start body ran exactly once — the `runs`
atomic is the ground truth. `TestConcurrentInc` runs N goroutines each calling
`Inc` M times and asserts the total is exactly `N*M`, which only holds if `Inc` is
atomic under `-race`.

Create `counter_test.go`:

```go
package counter

import (
	"fmt"
	"sync"
	"testing"
)

func TestStartsExactlyOnce(t *testing.T) {
	t.Parallel()

	c := &LazyCounter{}
	const goroutines = 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			c.Start()
		}()
	}
	wg.Wait()

	if !c.Started() {
		t.Fatal("Started() = false after concurrent Start")
	}
	if got := c.Runs(); got != 1 {
		t.Fatalf("Runs() = %d, want exactly 1", got)
	}
}

func TestConcurrentInc(t *testing.T) {
	t.Parallel()

	const (
		workers = 50
		perW    = 1000
	)
	c := &LazyCounter{}
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for range perW {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if got, want := c.Value(), int64(workers*perW); got != want {
		t.Fatalf("Value() = %d, want %d", got, want)
	}
}

func ExampleLazyCounter() {
	c := &LazyCounter{}
	c.Start()
	c.Start() // no-op
	c.Inc()
	c.Inc()
	fmt.Println(c.Started(), c.Runs(), c.Value())
	// Output: true 1 2
}
```

The `Example` pins the observable contract; the two concurrent tests and the
demo's Expected-output block carry the behavioral proof under load.

## Review

Correctness has two independent pieces. The start transition is exactly-once:
`Runs()` is 1 after 200 goroutines hammer `Start`, which is only true if the
`Once` ran its body a single time — the trap it guards against is a copied
`LazyCounter` (each copy has a fresh `Once` and would re-run start). The counting
is lock-free and exact: `TestConcurrentInc` gets `N*M` because `Add` and `Load`
are atomic; a plain `int64` here would lose increments and trip the race detector.
The design lesson is matching the guard to the state — `Once` for the one-time
side effect, atomics for the hot-path arithmetic — and never the reverse. Run
`go test -race` to confirm both.

## Resources

- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [sync/atomic — pkg.go.dev](https://pkg.go.dev/sync/atomic)
- [The Go Memory Model](https://go.dev/ref/mem)

---

Prev: [01-once-init-service.md](01-once-init-service.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-idempotent-close.md](03-idempotent-close.md)
