# Exercise 6: The Background Worker That Wouldn't Shut Down

A background worker loops `for { select { ... } }` over a jobs channel and a
flush ticker. It shipped without a cancellation arm, so when the service began
graceful shutdown the goroutine kept running past `ctx` cancellation and the
process never drained. You will reproduce the leak with a test that cancels the
context and waits for the worker to exit, diagnose the missing `case
<-ctx.Done()`, and fix the select.

## What you'll build

```text
worker/                    module example.com/worker
  go.mod
  worker.go                Worker with Run(ctx); jobs channel + flush ticker + Done arm
  cmd/demo/
    main.go                runnable demo: feed jobs, let it flush, cancel, join
  worker_test.go           shutdown-before-deadline test, flush test, Example (all -race)
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: `Worker.Run(ctx)` — a `for { select { ... } }` with a jobs arm, a ticker arm, and an explicit `case <-ctx.Done(): return`, with the ticker `Stop()`ed on exit and shared counters guarded by a mutex.
- Test: start `Run` in a goroutine tracked by a `WaitGroup`, cancel `ctx`, and assert `Wait()` returns before a deadline; assert the ticker flushes; run under `-race`.
- Verify: `go test -count=1 -race ./...`.

### The artifact and the planted bug

The worker drains jobs and periodically flushes on a ticker — the shape of a
metrics batcher, a write-behind cache, an outbound queue. The version that shipped
selected over the jobs channel and the ticker, and nothing else:

```go
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case j := <-w.Jobs:
			w.handled += j
		case <-t.C:
			w.flushes++
		}
		// BUG: no case <-ctx.Done(): return.
		// Run never observes cancellation, so the goroutine leaks and Wait blocks
		// forever during graceful shutdown.
	}
}
```

There is no way out. When the server cancels `ctx`, this select never notices —
it only wakes for a job or a tick — so the goroutine runs forever. During
shutdown the supervisor's `WaitGroup.Wait()` blocks indefinitely and the process
hangs. A closely related variant adds a `default:` arm instead, which turns the
blocking select into a busy-spin that pins a CPU core at 100% because the loop
never parks. Both survive review: the happy path processes jobs correctly, and
nobody tests the shutdown path.

The failing test reads:

```text
--- FAIL: TestWorkerShutsDown (2.00s)
    worker_test.go:41: worker did not exit within 2s after cancel (goroutine leak)
```

The fix adds an explicit `case <-ctx.Done(): return` and keeps every other arm
blocking, so the loop parks between events and exits the instant the context is
cancelled.

Create `worker.go`:

```go
package worker

import (
	"context"
	"sync"
	"time"
)

// Worker drains jobs and periodically flushes on a ticker. Run blocks until ctx
// is cancelled, then returns after stopping the ticker.
type Worker struct {
	Jobs     chan int
	Interval time.Duration

	mu      sync.Mutex
	handled int
	flushes int
}

// New returns a Worker with an unbuffered jobs channel and the given flush
// interval.
func New(interval time.Duration) *Worker {
	return &Worker{Jobs: make(chan int), Interval: interval}
}

// Run is the worker loop. It processes jobs, flushes on the ticker, and returns
// as soon as ctx is cancelled. The ctx.Done arm is what makes shutdown possible;
// every other arm blocks, so the loop parks between events instead of spinning.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-w.Jobs:
			w.mu.Lock()
			w.handled += j
			w.mu.Unlock()
		case <-t.C:
			w.mu.Lock()
			w.flushes++
			w.mu.Unlock()
		}
	}
}

// Handled reports the summed value of all processed jobs.
func (w *Worker) Handled() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.handled
}

// Flushes reports how many ticker flushes have occurred.
func (w *Worker) Flushes() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushes
}
```

### The runnable demo

The demo feeds three jobs, lets the ticker fire a couple of times, cancels, and
joins the worker — the graceful-shutdown sequence in miniature.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/worker"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	w := worker.New(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	for _, j := range []int{1, 2, 3} {
		w.Jobs <- j
	}
	time.Sleep(50 * time.Millisecond) // let the ticker flush a couple of times
	cancel()
	<-done // join the worker

	fmt.Printf("handled=%d flushed-at-least-once=%v\n", w.Handled(), w.Flushes() >= 1)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
handled=6 flushed-at-least-once=true
```

### Tests

`TestWorkerShutsDown` is the reproducer: it starts `Run` under a `WaitGroup`,
sends two jobs, cancels the context, and asserts `Wait()` returns before a 2-second
deadline — a leak makes it hit the deadline and fail. Reading `Handled()` only
after the join is what keeps it race-free: by then `Run` has returned, so every
counter write has happened-before the read. `TestWorkerFlushes` uses a short
interval to prove the ticker arm fires. Both run clean under `-race`.

Create `worker_test.go`:

```go
package worker

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestWorkerShutsDown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	w := New(time.Hour) // long interval: the ticker will not fire during the test

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx)
	}()

	w.Jobs <- 3
	w.Jobs <- 4
	cancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not exit within 2s after cancel (goroutine leak)")
	}

	if got := w.Handled(); got != 7 {
		t.Fatalf("Handled() = %d, want 7", got)
	}
}

func TestWorkerFlushes(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := New(10 * time.Millisecond)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.Run(ctx)
	}()

	time.Sleep(60 * time.Millisecond)
	cancel()
	wg.Wait()

	if w.Flushes() == 0 {
		t.Fatal("Flushes() = 0, want at least one tick flushed")
	}
}

func ExampleWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	w := New(time.Hour)

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	w.Jobs <- 10
	w.Jobs <- 5
	cancel()
	<-done

	fmt.Println(w.Handled())
	// Output: 15
}
```

## Review

The worker is correct when it processes every delivered job, flushes on its
ticker, and — the property that was missing — exits promptly when its context is
cancelled. The shutdown test proves that with a deadline: a leaked goroutine never
lets `Wait()` return, so the test times out instead of passing. Two rules make the
loop sound. Give the select an explicit `case <-ctx.Done(): return` so cancellation
is observed, and keep every other arm blocking so the loop parks between events
rather than busy-spinning behind a `default`. Stopping the ticker on exit releases
its runtime timer. Verify shutdown under `-race`: the handshake between the
canceller and the worker touches shared state, and the mutex plus the
`WaitGroup`-ordered read are what keep it clean.

## Resources

- [context package](https://pkg.go.dev/context) — `WithCancel` and `Done` for shutdown signaling.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — blocking selects, `default`, and busy-spin.
- [time.NewTicker and (*Ticker).Stop](https://pkg.go.dev/time#NewTicker) — periodic work and releasing the timer.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-range-value-copy-mutation.md](05-range-value-copy-mutation.md) | Next: [07-recover-middleware-swallows-panic.md](07-recover-middleware-swallows-panic.md)
