# Exercise 5: Test a ticker-driven flush worker with testing/synctest

A batch worker that accumulates items and flushes them on a `time.Ticker` — a
metrics aggregator, a log shipper, a write-behind buffer — is time-dependent
concurrent code, the hardest kind to test. Rather than inject a clock, this
exercise leaves the worker using the *real* `time.NewTicker` exactly as it ships,
and tests it inside a `testing/synctest` bubble where virtual time makes the ticks
fire instantly and `synctest.Wait` gives a race-free barrier before each
assertion.

## What you'll build

```text
flushworker/                   independent module: example.com/flushworker
  go.mod
  worker.go                    Worker: Enqueue, Run(ctx) — real time.NewTicker, flush on tick
  cmd/
    demo/
      main.go                  run a worker on a real short ticker; print flushed batches
  worker_test.go               synctest bubble: enqueue, advance a tick, Wait, assert batch
```

Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
Implement: a `Worker` that buffers enqueued items and, on each ticker tick, drains the buffer and calls a `flush` callback, stopping cleanly on `ctx` cancel — using `time.NewTicker` directly, no injected clock.
Test: `synctest.Test` — start the worker goroutine, enqueue, `time.Sleep(interval)` to fire one virtual tick, `synctest.Wait()` until the worker is durably blocked, assert the flush received the batch; cancel and confirm the goroutine exits.
Verify: `go test -count=1 -race ./...`

Set up the module (synctest is stable in Go 1.25):

```bash
mkdir -p go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/05-synctest-ticker-worker/cmd/demo
cd go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/05-synctest-ticker-worker
go mod edit -go=1.25
```

### Why no clock here, and what synctest.Wait buys you

The worker uses `time.NewTicker(interval)` and `select`s on its channel against
`ctx.Done()`. In production that is exactly what you want; injecting a `Clock`
would mean re-implementing ticker semantics behind an interface just to satisfy a
test. `synctest` removes that pressure: inside the bubble, `time.NewTicker` runs
on the virtual clock. A `time.Sleep(interval)` in the *test* advances virtual time
enough to fire one tick, and the worker reacts — all in microseconds.

The subtlety is synchronization. When the test's `time.Sleep(interval)` returns,
the tick has fired, but the worker goroutine may not have finished draining the
buffer and calling `flush` yet — reading the flushed result immediately would race
the worker. `synctest.Wait()` fixes this: it blocks until every *other* goroutine
in the bubble is durably blocked, which for this worker means it has finished its
flush and is parked back on the `select`. Only after `Wait` returns is it safe to
assert on the flushed batch. This is the canonical shape: advance time, `Wait`,
assert.

The worker must be stoppable, because `synctest.Test` waits for every bubble
goroutine to exit before returning and reports a deadlock if one leaks. The
`ctx.Done()` case plus `defer ticker.Stop()` guarantees a clean exit when the test
cancels. The `flush` results are recorded under a mutex; acquiring an uncontended
mutex does not block, so it is fine inside the bubble, and it keeps the `-race`
detector satisfied for the demo's real-time path too.

Create `worker.go`:

```go
package flushworker

import (
	"context"
	"sync"
	"time"
)

// Worker buffers enqueued items and flushes them in a batch on every ticker
// tick. It uses time.NewTicker directly so it can be tested as written under a
// synctest bubble.
type Worker struct {
	interval time.Duration
	flush    func([]int)

	mu  sync.Mutex
	buf []int
}

func NewWorker(interval time.Duration, flush func([]int)) *Worker {
	return &Worker{interval: interval, flush: flush}
}

// Enqueue appends an item to the pending buffer.
func (w *Worker) Enqueue(x int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, x)
}

// Run flushes the buffer on each tick until ctx is cancelled. A tick with an
// empty buffer flushes nothing.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.mu.Lock()
			batch := w.buf
			w.buf = nil
			w.mu.Unlock()
			if len(batch) > 0 {
				w.flush(batch)
			}
		}
	}
}
```

### The runnable demo

The demo runs the worker on a real 20ms ticker, enqueues three items, sleeps long
enough for a tick, then enqueues two more and sleeps again — printing each flushed
batch against the wall clock.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/flushworker"
)

func main() {
	var mu sync.Mutex
	var batches [][]int
	w := flushworker.NewWorker(20*time.Millisecond, func(b []int) {
		mu.Lock()
		batches = append(batches, b)
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)

	w.Enqueue(1)
	w.Enqueue(2)
	w.Enqueue(3)
	time.Sleep(50 * time.Millisecond)

	w.Enqueue(4)
	w.Enqueue(5)
	time.Sleep(50 * time.Millisecond)

	cancel()
	mu.Lock()
	fmt.Printf("batches: %v\n", batches)
	mu.Unlock()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batches: [[1 2 3] [4 5]]
```

### Tests

`TestFlushOnTick` runs inside a bubble: it starts the worker, enqueues three
items, sleeps one interval in virtual time to fire a tick, calls `synctest.Wait()`
so the worker has finished flushing, and asserts the batch. It then enqueues two
more, advances another tick, and asserts the second batch — proving successive
ticks flush successive buffers. `TestNoFlushWhenEmpty` proves an empty tick
flushes nothing. Both cancel the context and rely on the bubble to catch a leaked
goroutine.

Create `worker_test.go`:

```go
package flushworker

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestFlushOnTick(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var batches [][]int
		w := NewWorker(time.Second, func(b []int) {
			mu.Lock()
			batches = append(batches, b)
			mu.Unlock()
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go w.Run(ctx)

		w.Enqueue(1)
		w.Enqueue(2)
		w.Enqueue(3)
		time.Sleep(time.Second) // fire one virtual tick
		synctest.Wait()         // worker has flushed and parked

		w.Enqueue(4)
		w.Enqueue(5)
		time.Sleep(time.Second)
		synctest.Wait()

		mu.Lock()
		got := batches
		mu.Unlock()
		want := [][]int{{1, 2, 3}, {4, 5}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("batches = %v, want %v", got, want)
		}
	})
}

func TestNoFlushWhenEmpty(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		flushes := 0
		w := NewWorker(time.Second, func([]int) {
			mu.Lock()
			flushes++
			mu.Unlock()
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go w.Run(ctx)

		time.Sleep(3 * time.Second) // three empty ticks
		synctest.Wait()

		mu.Lock()
		got := flushes
		mu.Unlock()
		if got != 0 {
			t.Fatalf("flush called %d times on empty buffer, want 0", got)
		}
	})
}
```

## Review

The worker is correct when each tick flushes exactly the items enqueued since the
previous tick and an empty tick flushes nothing, and when it exits promptly on
cancellation. The bubble proves the timing without a clock abstraction: virtual
time fires the ticker, and `synctest.Wait` removes the race between the tick firing
and the flush completing. The mistakes this exercise guards against: asserting on
`batches` without `synctest.Wait` (a race — the worker may not have flushed yet),
and starting the worker with no way to stop it (the bubble reports the leak as a
deadlock). Note the outer test calls `t.Parallel()` but the bubble function does
not — `t.Parallel` on the bubble's `T` is forbidden. Run `go test -race` to
confirm the buffer mutex holds under the demo's real-time path.

## Resources

- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — `synctest.Test` and `synctest.Wait`.
- [Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the Go blog's worked ticker/timer examples.
- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) and [`Ticker.Stop`](https://pkg.go.dev/time#Ticker.Stop) — the real ticker the worker drives, virtualized in the bubble.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-ttl-cache-expiry.md](04-ttl-cache-expiry.md) | Next: [06-synctest-context-timeout.md](06-synctest-context-timeout.md)
