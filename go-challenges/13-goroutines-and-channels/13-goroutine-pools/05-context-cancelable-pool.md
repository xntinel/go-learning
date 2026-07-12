# Exercise 5: A Worker Pool Whose In-Flight Jobs Honor Cancellation

The pool in Exercise 1 drains gracefully but has no way to *abort*: if a worker is
stuck in a hung upstream call, `Close` waits for it forever. This exercise upgrades
the pool so `Job` is `func(ctx context.Context) error`, the pool carries a context
created in `New`, and a new `Shutdown` cancels that context so long-running jobs
observe `ctx.Done()` and stop early — while `Close` still offers the soft, drain
everything path. Two shutdown contracts, one pool.

This module is fully self-contained.

## What you'll build

```text
cpool/                     independent module: example.com/cpool
  go.mod                   go 1.25
  pool.go                  type Pool; New, Submit, Close (drain), Shutdown (cancel)
  cmd/
    demo/
      main.go              runnable demo: a blocking job aborted by Shutdown
  pool_test.go             abort-inflight, drain-short, skip-buffered, reject tests, -race
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a pool whose jobs receive a context; `Close` drains (lets in-flight finish), `Shutdown` cancels (aborts in-flight and skips not-yet-started jobs), both reject further `Submit`s and are idempotent.
- Test: `Shutdown` makes blocking jobs return promptly and workers exit; `Close` lets already-started short jobs finish; buffered jobs are skipped after `Shutdown`; `Submit` rejects after either.
- Verify: `go test -count=1 -race ./...`

### Drain and cancel are two contracts

Cancelling the worker *loop* is not enough — that only stops new jobs from
starting. A job already running a network call keeps running until that call
returns, because the call never learned it should stop. The fix is to pass a
context *into the job* and have the job select on `ctx.Done()`. The pool creates
one cancellable context in `New` and hands it to every job. `Shutdown` calls the
cancel function; every in-flight job that watches `ctx.Done()` returns at that
instant.

`Close` and `Shutdown` then encode the two contracts. `Close` is the soft drain:
it closes the job channel and waits, letting each in-flight job finish naturally —
the context is never cancelled, so a short job runs to completion. `Shutdown` is
the hard cancel: it cancels the context *first* (so in-flight jobs abort), then
closes the channel and waits. Because the context is already cancelled when the
workers drain the remaining buffered jobs, each worker checks `ctx.Err()` before
running a job and skips any that had not started — a job you never began after a
hard shutdown should not run at all.

Both paths flip the same `closed` flag under the mutex, so `Submit` rejects after
either, and both are idempotent: cancelling an already-cancelled context is a
no-op, and the flag makes the channel close happen exactly once.

Create `pool.go`:

```go
package cpool

import (
	"context"
	"sync"
)

// Job is a unit of work that receives the pool's context so it can abort when
// the pool is hard-shut-down.
type Job func(ctx context.Context) error

// Pool runs a fixed number of workers. Close drains in-flight work; Shutdown
// cancels it via the context passed to each job.
type Pool struct {
	ctx    context.Context
	cancel context.CancelFunc
	jobs   chan Job
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New starts workers goroutines over a cancellable context.
func New(workers int) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		ctx:    ctx,
		cancel: cancel,
		jobs:   make(chan Job, workers*2),
	}
	for range workers {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for job := range p.jobs {
		if p.ctx.Err() != nil {
			continue // hard shutdown: do not start new jobs
		}
		_ = job(p.ctx)
	}
}

// Submit enqueues job, returning false once the pool is closing or shut down.
func (p *Pool) Submit(job Job) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.jobs <- job
	return true
}

// closeChannel marks the pool closed and closes the job channel exactly once.
func (p *Pool) closeChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	close(p.jobs)
}

// Close is a soft drain: stop accepting work and block until every in-flight and
// queued job finishes. In-flight jobs run to completion (no cancellation).
func (p *Pool) Close() {
	p.closeChannel()
	p.wg.Wait()
}

// Shutdown is a hard cancel: cancel the context so in-flight jobs abort, then
// drain and wait. Jobs not yet started are skipped. Idempotent.
func (p *Pool) Shutdown() {
	p.cancel()
	p.closeChannel()
	p.wg.Wait()
}
```

Note `Shutdown` cancels before closing the channel: if it closed first, a worker
could pull and start a buffered job in the window before the cancel landed. Cancel
first, and the `p.ctx.Err() != nil` check in the loop reliably skips everything
not already running.

### The runnable demo

The demo submits a job that blocks until its context is cancelled, then calls
`Shutdown` and reports that the job aborted rather than ran to completion.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/cpool"
)

func main() {
	p := cpool.New(2)

	var aborted atomic.Bool
	started := make(chan struct{})
	p.Submit(func(ctx context.Context) error {
		close(started)
		<-ctx.Done() // block until Shutdown cancels
		aborted.Store(true)
		return ctx.Err()
	})

	<-started
	time.Sleep(5 * time.Millisecond)
	p.Shutdown() // cancels the context; the job returns

	fmt.Printf("aborted: %v\n", aborted.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
aborted: true
```

### Tests

`TestShutdownAbortsInflight` fills the pool with jobs that block on `ctx.Done()`,
waits until all have started, then asserts `Shutdown` returns promptly and every
job observed cancellation. `TestCloseDrainsShortJobs` submits short jobs that do
not watch the context and asserts a soft `Close` lets them all finish.
`TestBufferedJobsSkippedAfterShutdown` occupies the single worker with a blocking
job, queues another behind it, and asserts that after `Shutdown` the queued job
never ran. `TestSubmitRejectedAfterShutdown` asserts `Submit` returns `false`
after `Shutdown` and that a second `Shutdown` does not panic.

Create `pool_test.go`:

```go
package cpool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownAbortsInflight(t *testing.T) {
	t.Parallel()

	const n = 4
	p := New(n)

	var aborted atomic.Int64
	started := make(chan struct{}, n)
	for range n {
		p.Submit(func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			aborted.Add(1)
			return ctx.Err()
		})
	}
	for range n {
		<-started // all n jobs are running and blocked
	}

	done := make(chan struct{})
	go func() { p.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not return; in-flight jobs did not abort")
	}
	if got := aborted.Load(); got != n {
		t.Fatalf("aborted = %d, want %d", got, n)
	}
}

func TestCloseDrainsShortJobs(t *testing.T) {
	t.Parallel()

	p := New(4)
	var done atomic.Int64
	for range 10 {
		p.Submit(func(_ context.Context) error {
			time.Sleep(5 * time.Millisecond)
			done.Add(1)
			return nil
		})
	}
	p.Close() // drain: every started job finishes
	if got := done.Load(); got != 10 {
		t.Fatalf("done = %d after Close, want 10", got)
	}
}

func TestBufferedJobsSkippedAfterShutdown(t *testing.T) {
	t.Parallel()

	p := New(1)
	started := make(chan struct{})
	var queuedRan atomic.Bool

	p.Submit(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	<-started // the one worker is busy on the blocking job
	p.Submit(func(_ context.Context) error {
		queuedRan.Store(true)
		return nil
	})

	p.Shutdown()
	if queuedRan.Load() {
		t.Fatal("queued job ran after Shutdown; it should have been skipped")
	}
}

func TestSubmitRejectedAfterShutdown(t *testing.T) {
	t.Parallel()

	p := New(2)
	p.Shutdown()
	if p.Submit(func(context.Context) error { return nil }) {
		t.Fatal("Submit should return false after Shutdown")
	}
	p.Shutdown() // must not panic
}

func TestNoGoroutineLeak(t *testing.T) {
	p := New(4)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		p.Submit(func(_ context.Context) error {
			defer wg.Done()
			return nil
		})
	}
	wg.Wait()
	p.Close() // wg.Wait inside Close proves workers exited
}
```

## Review

The pool is correct when the two contracts are distinct and both terminate. Soft
`Close` never cancels the context, so `TestCloseDrainsShortJobs` sees all ten jobs
finish. Hard `Shutdown` cancels first, so `TestShutdownAbortsInflight` sees every
blocked job return and `Shutdown` itself return promptly, and
`TestBufferedJobsSkippedAfterShutdown` sees a queued-but-unstarted job skipped by
the `p.ctx.Err() != nil` guard. Both flip `closed`, so `Submit` rejects
afterward, and both are idempotent.

The mistakes to avoid: cancelling the worker loop instead of the job (a hung call
never learns to stop, so drain hangs — the whole reason `Job` takes a context);
closing the channel before cancelling in `Shutdown` (a worker can grab a buffered
job in the gap); and forgetting the `ctx.Err()` check in the loop (then buffered
jobs run even after a hard shutdown). Run `-race` to confirm the `closed` flag and
the context are accessed cleanly across `Submit`, `Close`, and `Shutdown`.

## Resources

- [`context`](https://pkg.go.dev/context) — `WithCancel`, `Done`, and the cancellation model jobs observe.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — propagating cancellation into in-flight work.
- [Go Blog: Contexts](https://go.dev/blog/context) — passing a context through an API boundary.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-weighted-semaphore-cost.md](04-weighted-semaphore-cost.md) | Next: [06-backpressure-load-shedding.md](06-backpressure-load-shedding.md)
