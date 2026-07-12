# Exercise 1: The Draining Worker Pool

A worker pool that can be shut down cleanly is the smallest complete example of the three-phase drain. This exercise builds a fixed-size `Pool` whose `Shutdown` stops accepting new jobs, runs every job already queued to completion, and honours a deadline — using a `quit` channel rather than a closed jobs channel so that `Submit` and `Shutdown` can never race into a panic.

This module is fully self-contained: it has its own `go mod init`, its own demo, and its own tests, and imports nothing from the other exercises.

## What you'll build

```text
drain/
  go.mod
  pool.go              Pool, New, Submit, Shutdown; quit-channel stop signal,
                       WaitGroup join, deadline + force, ErrPoolClosed
  cmd/
    demo/
      main.go          submit a batch, drain cleanly, report completion count
  pool_test.go         clean drain, deadline path, Submit-after-Shutdown,
                       idempotent Shutdown, and a no-goroutine-leak assertion
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `New(numWorkers, queueSize int) *Pool`, `(*Pool).Submit(func()) error`, `(*Pool).Shutdown(context.Context) error`.
- Test: every accepted job runs before `Shutdown` returns; a too-short deadline yields `context.DeadlineExceeded`; `Submit` after `Shutdown` returns `ErrPoolClosed`; `Shutdown` is idempotent; the goroutine count returns to baseline.
- Verify: `go test -race ./...`

## Why a quit channel, not a closed jobs channel

The classic worker-pool sketch closes the jobs channel to signal shutdown: workers `for job := range jobs`, and `close(jobs)` ends every loop. That design is correct only when nothing can still send. Here `Submit` is called by arbitrary external goroutines, so it can run concurrently with `Shutdown`. A send on a closed channel panics, and an atomic "closed" flag checked before the send does not save you: the channel can be closed in the gap between the check and the send, and a `Submit` already blocked on a full channel is past any check. The send path is not guardable.

So this pool never closes the jobs channel. It closes a separate `quit` channel — exactly once, via `sync.Once` — and everyone selects on it. `Submit` selects between handing off its job and observing `quit`, returning `ErrPoolClosed` the instant `quit` is closed, with no possibility of a send on a closed channel. Workers select between taking a job and observing `quit`. Closing `quit` is a broadcast, so all workers wake at once; a plain send would wake only one.

When `quit` closes, each worker still has buffered jobs to honour — that is the drain. The worker switches to a non-blocking loop that pulls buffered jobs until the channel is empty (`select` with a `default`), then returns. Every buffered job is finite and some worker pulls each, so nothing accepted is dropped. `Shutdown` then waits on a `WaitGroup` that counts the workers, but inside a `select` against the caller's context: if the deadline fires first it returns `ctx.Err()` and the still-running jobs are left to finish on their own — the force decision belongs to the caller, who now knows the drain did not complete in time.

Create `pool.go`:

```go
// pool.go
package drain

import (
	"context"
	"errors"
	"sync"
)

// ErrPoolClosed is returned by Submit after the pool has begun shutting down.
var ErrPoolClosed = errors.New("drain: pool is closed")

// Pool is a fixed-size worker pool that supports graceful draining. The zero
// value is not usable; construct one with New.
type Pool struct {
	jobs chan func()
	quit chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// New starts a Pool with numWorkers worker goroutines and a job queue buffered
// to queueSize. Both must be positive.
func New(numWorkers, queueSize int) *Pool {
	if numWorkers <= 0 {
		numWorkers = 1
	}
	if queueSize < 0 {
		queueSize = 0
	}
	p := &Pool{
		jobs: make(chan func(), queueSize),
		quit: make(chan struct{}),
	}
	p.wg.Add(numWorkers)
	for range numWorkers {
		go p.run()
	}
	return p
}

// run is the worker loop. It executes jobs until quit is closed, then drains any
// buffered jobs and exits, so no accepted job is dropped on shutdown.
func (p *Pool) run() {
	defer p.wg.Done()
	for {
		select {
		case job := <-p.jobs:
			job()
		case <-p.quit:
			for {
				select {
				case job := <-p.jobs:
					job()
				default:
					return
				}
			}
		}
	}
}

// Submit enqueues job for execution. It returns ErrPoolClosed once Shutdown has
// been called; otherwise it blocks until there is room in the queue. Submit
// never sends on a closed channel, so it cannot panic during shutdown.
func (p *Pool) Submit(job func()) error {
	select {
	case <-p.quit:
		return ErrPoolClosed
	default:
	}
	select {
	case p.jobs <- job:
		return nil
	case <-p.quit:
		return ErrPoolClosed
	}
}

// Shutdown stops the pool from accepting new jobs and waits for all in-flight and
// buffered jobs to finish. If ctx expires before draining completes, Shutdown
// returns ctx.Err() and leaves the remaining jobs running. Shutdown is safe to
// call multiple times and from multiple goroutines.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.once.Do(func() { close(p.quit) })

	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
```

The first `select` in `Submit` is a fast reject: if the pool is already shutting down, fail before touching the jobs channel. The second `select` is the real handoff; pairing the send with `<-p.quit` means a `Submit` blocked on a full queue unblocks the moment `Shutdown` is called instead of hanging or, in the broken design, panicking. The two-level loop in `run` is the drain: the outer `select` runs jobs normally, and once `quit` is observed the inner loop empties the buffer with a non-blocking `default` before the worker returns.

### The runnable demo

The demo submits a batch of jobs to a small pool, drains it with a generous deadline, and reports how many jobs ran. Each job only increments an atomic counter, so the total is deterministic regardless of which worker runs which job.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/drain"
)

func main() {
	p := drain.New(4, 32)

	const jobs = 12
	var completed atomic.Int64
	for range jobs {
		if err := p.Submit(func() {
			time.Sleep(5 * time.Millisecond)
			completed.Add(1)
		}); err != nil {
			fmt.Printf("submit error: %v\n", err)
		}
	}
	fmt.Printf("submitted %d jobs\n", jobs)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		fmt.Printf("drain timed out: %v\n", err)
		return
	}
	fmt.Printf("drained cleanly: %d jobs completed\n", completed.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
submitted 12 jobs
drained cleanly: 12 jobs completed
```

### Tests

The tests cover the four behaviours the pool promises plus the leak assertion. The clean-drain test submits jobs that sleep, then asserts all of them ran before `Shutdown` returned. The deadline test submits one job longer than the timeout and asserts `context.DeadlineExceeded`. The reject test asserts `Submit` after `Shutdown` returns `ErrPoolClosed`. The idempotence test calls `Shutdown` repeatedly. The leak test is deliberately not parallel — it measures `runtime.NumGoroutine()`, which is only stable when no parallel test is running — and polls until the worker goroutines have torn down.

Create `pool_test.go`:

```go
// pool_test.go
package drain_test

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"example.com/drain"
)

func TestPoolDrainsEveryAcceptedJob(t *testing.T) {
	t.Parallel()

	p := drain.New(3, 16)
	var count atomic.Int64
	const jobs = 20
	for range jobs {
		if err := p.Submit(func() {
			time.Sleep(2 * time.Millisecond)
			count.Add(1)
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if got := count.Load(); got != jobs {
		t.Errorf("completed = %d, want %d", got, jobs)
	}
}

func TestShutdownDeadlineReturnsError(t *testing.T) {
	t.Parallel()

	p := drain.New(1, 4)
	if err := p.Submit(func() { time.Sleep(500 * time.Millisecond) }); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want DeadlineExceeded", err)
	}
}

func TestSubmitAfterShutdownIsRejected(t *testing.T) {
	t.Parallel()

	p := drain.New(2, 4)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := p.Submit(func() {}); !errors.Is(err, drain.ErrPoolClosed) {
		t.Errorf("Submit after Shutdown = %v, want ErrPoolClosed", err)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	t.Parallel()

	p := drain.New(2, 4)
	ctx := context.Background()
	for i := range 3 {
		if err := p.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown call %d: %v", i+1, err)
		}
	}
}

func TestNoGoroutineLeakAfterShutdown(t *testing.T) {
	// Not parallel: NumGoroutine is only stable when no parallel test runs.
	base := runtime.NumGoroutine()

	p := drain.New(4, 16)
	for range 10 {
		if err := p.Submit(func() { time.Sleep(time.Millisecond) }); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	assertNoLeak(t, base)
}

// assertNoLeak polls until the goroutine count returns to base, allowing for the
// asynchronous teardown of goroutines that have already returned.
func assertNoLeak(t *testing.T, base int) {
	t.Helper()
	for range 100 {
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine leak: baseline %d, now %d", base, runtime.NumGoroutine())
}
```

## Review

The pool is correct when `Submit` can never panic and `Shutdown` never strands an accepted job. The key design decision is that the jobs channel is never closed: shutdown is signalled by closing a separate `quit` channel under `sync.Once`, which both `Submit` and the workers observe. A common wrong turn is to close the jobs channel and guard `Submit` with an atomic flag; that still panics, because the close can land between the flag check and the send, and a `Submit` blocked on a full queue is past the check entirely — the deadline test and the reject test exist to keep that design honest. Another is calling `wg.Add` inside the worker goroutine instead of in `New` before the `go`, which lets `Shutdown` return before the workers have even started. The drain itself lives in the worker's inner non-blocking loop: without it, closing `quit` would abandon buffered jobs, which the "drains every accepted job" test catches. Run the package under `go test -race`; the race detector is what proves the `quit` broadcast and the atomic counter are synchronized, and the non-parallel leak test proves every worker and the `wg.Wait` helper goroutine are gone once `Shutdown` returns cleanly.

## Resources

- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the join barrier; note that `Add` must precede the `go` statement.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — closing the `quit` channel exactly once even under concurrent `Shutdown` calls.
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) — the explicit-`quit`-channel pattern for signalling a fan-out of goroutines to stop.
- [`context` package](https://pkg.go.dev/context) — `WithTimeout` and `ctx.Done()` for the drain deadline.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-graceful-connection-server.md](02-graceful-connection-server.md)
</content>
