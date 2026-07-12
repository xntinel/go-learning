# Exercise 4: A Cancellable Worker Pool That Drains Its Queue

A worker pool consuming a job channel is the backbone of every batch processor,
ingestion pipeline, and async task runner. The lifecycle question that separates a
toy pool from a production one is teardown: on shutdown, do you *drain* the queued
work or *drop* it? This exercise builds a pool that supports both, and accounts
for every job so nothing is silently lost.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise. It uses
`sync.WaitGroup.Go`, which requires Go 1.25+.

## What you'll build

```text
workerpool/                independent module: example.com/workerpool
  go.mod
  pool.go                  type Pool; New, Submit, Stop (drain), Cancel (drop); ErrPoolStopped
  cmd/
    demo/
      main.go              runnable demo: submit jobs, graceful drain, count processed
  pool_test.go             drain-processes-all, hard-cancel-accounts-for-remainder tests
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a fixed-size pool of workers launched with `wg.Go`; `Submit` enqueues onto a channel; `Stop` closes the channel and drains queued jobs; `Cancel` cancels the context, stops promptly, and returns the jobs it did not process.
- Test: graceful `Stop` processes all M jobs exactly once (no loss, no duplicates); hard `Cancel` mid-flight exits promptly and the processed count plus the returned remainder equals M (nothing lost silently).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/04-context-worker-pool/cmd/demo
cd go-solutions/13-goroutines-and-channels/11-goroutine-lifecycle-management/04-context-worker-pool
go mod edit -go=1.25
```

### Two teardown modes, one worker loop

The pool holds a buffered `jobs` channel, a `context` with its `cancel`, a
`sync.WaitGroup` for the workers, and bookkeeping to prove correctness. Each
worker runs the same loop: a `select` over `ctx.Done()` and the `jobs` channel.
That single loop supports both teardown modes because the two modes drive the two
`select` cases:

- **Graceful `Stop`** `close(jobs)`, then `wg.Wait()`. Closing the channel makes
  `<-jobs` eventually return `ok == false` once the buffer is empty, so each
  worker finishes every queued job and *then* returns. The context is not
  cancelled until after the wait, so the `ctx.Done()` case never fires during a
  drain. Result: all queued work is processed.
- **Hard `Cancel`** `cancel()`, then `wg.Wait()`. Cancelling closes `ctx.Done()`,
  so workers return on their next loop iteration — after finishing at most the one
  job already in hand. Jobs still sitting in the channel are left there. `Cancel`
  then drains the channel with a non-blocking `select`/`default` loop and returns
  those jobs, so the caller knows exactly what was dropped rather than losing it.

The correctness bookkeeping is the point of the exercise. A `handled` map (guarded
by a mutex) records each processed job ID; a `processed` atomic counts total
handle calls. If a job were processed twice, `processed` would exceed
`len(handled)`; if a job were lost, `processed + len(remaining)` would be less
than the number submitted. The tests assert both invariants.

Workers are launched with `wg.Go(fn)` (Go 1.25), which does `Add(1)`, `go`, and
`defer Done()` in one call — closing the classic race where `Add` is written
inside the goroutine and `Wait` can miss it.

Create `pool.go`:

```go
package workerpool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrPoolStopped is returned by Submit after the pool has been stopped or
// cancelled.
var ErrPoolStopped = errors.New("pool stopped")

// Job is a unit of work; ID identifies it for accounting.
type Job struct {
	ID  int
	Run func(context.Context)
}

// Pool is a fixed-size set of workers consuming a job channel.
type Pool struct {
	jobs      chan Job
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	processed atomic.Int64

	mu      sync.Mutex
	handled map[int]bool
}

// New starts workers goroutines reading from a queue of the given capacity.
func New(workers, queue int) *Pool {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		jobs:    make(chan Job, queue),
		ctx:     ctx,
		cancel:  cancel,
		handled: make(map[int]bool),
	}
	for range workers {
		p.wg.Go(p.worker)
	}
	return p
}

func (p *Pool) worker() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case j, ok := <-p.jobs:
			if !ok {
				return // channel drained and closed: graceful stop
			}
			if j.Run != nil {
				j.Run(p.ctx)
			}
			p.processed.Add(1)
			p.mu.Lock()
			p.handled[j.ID] = true
			p.mu.Unlock()
		}
	}
}

// Submit enqueues a job. It returns ErrPoolStopped if the pool has been
// cancelled. Submit must not be called concurrently with Stop or Cancel.
func (p *Pool) Submit(j Job) error {
	if p.ctx.Err() != nil {
		return ErrPoolStopped // already stopped: decide deterministically
	}
	select {
	case <-p.ctx.Done():
		return ErrPoolStopped
	case p.jobs <- j:
		return nil
	}
}

// Stop performs a graceful drain: it stops accepting jobs, lets the workers
// finish everything already queued, then releases the pool's context.
func (p *Pool) Stop() {
	close(p.jobs)
	p.wg.Wait()
	p.cancel()
}

// Cancel performs a hard stop: workers return after their current job, and the
// jobs left unprocessed are drained and returned so the caller can see exactly
// what was dropped.
func (p *Pool) Cancel() []Job {
	p.cancel()
	p.wg.Wait()

	var remaining []Job
	for {
		select {
		case j := <-p.jobs:
			remaining = append(remaining, j)
		default:
			return remaining
		}
	}
}

// Processed reports how many jobs the workers have handled.
func (p *Pool) Processed() int64 {
	return p.processed.Load()
}

// Handled reports how many distinct job IDs were processed.
func (p *Pool) Handled() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.handled)
}
```

There is a real contract in `Submit`'s doc comment: it must not run concurrently
with `Stop`, because `Stop` closes `jobs` and a concurrent `Submit` would send on
a closed channel and panic. This is the standard producer/closer discipline — the
component that closes a channel must be the sole party that could still be sending
on it. In these tests (and in a typical batch driver) submission happens on one
goroutine that then calls `Stop`, so the ordering is guaranteed.

### The runnable demo

The demo submits nine squaring jobs to a pool of three workers, drains gracefully,
and reports that all nine were processed exactly once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/workerpool"
)

func main() {
	var sum atomic.Int64
	p := workerpool.New(3, 16)

	const n = 9
	for i := 1; i <= n; i++ {
		id := i
		_ = p.Submit(workerpool.Job{
			ID: id,
			Run: func(ctx context.Context) {
				sum.Add(int64(id * id))
			},
		})
	}
	p.Stop() // graceful drain

	fmt.Println("submitted:", n)
	fmt.Println("processed:", p.Processed())
	fmt.Println("distinct handled:", p.Handled())
	fmt.Println("sum of squares:", sum.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
submitted: 9
processed: 9
distinct handled: 9
sum of squares: 285
```

### Tests

`TestGracefulDrainProcessesAll` submits M jobs, calls `Stop`, and asserts the
processed count and the distinct-handled count both equal M — proving every job
ran exactly once, with no loss and no duplicate. `TestHardCancelAccountsForAll`
submits M slow jobs, cancels mid-flight, and asserts the accounting invariant:
`Processed() + len(remaining) == M`. That is the real safety property — a hard
cancel is allowed to drop work, but it must *report* what it dropped, never lose
it silently. It also asserts the cancel exited promptly (fewer than all M were
processed), so cancellation is genuinely abrupt rather than a disguised drain.

Create `pool_test.go`:

```go
package workerpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestGracefulDrainProcessesAll(t *testing.T) {
	t.Parallel()

	const m = 200
	var counter atomic.Int64
	p := New(4, m)
	for i := range m {
		_ = p.Submit(Job{
			ID:  i,
			Run: func(ctx context.Context) { counter.Add(1) },
		})
	}
	p.Stop()

	if got := p.Processed(); got != m {
		t.Fatalf("Processed() = %d, want %d", got, m)
	}
	if got := p.Handled(); got != m {
		t.Fatalf("Handled() = %d, want %d (a duplicate or loss occurred)", got, m)
	}
	if got := counter.Load(); got != m {
		t.Fatalf("run count = %d, want %d", got, m)
	}
}

func TestHardCancelAccountsForAll(t *testing.T) {
	t.Parallel()

	const m = 40
	p := New(2, m)
	for i := range m {
		_ = p.Submit(Job{
			ID:  i,
			Run: func(ctx context.Context) { time.Sleep(20 * time.Millisecond) },
		})
	}

	time.Sleep(15 * time.Millisecond) // let a couple start
	remaining := p.Cancel()

	processed := int(p.Processed())
	if processed >= m {
		t.Fatalf("processed = %d, want < %d (cancel should be abrupt)", processed, m)
	}
	if processed+len(remaining) != m {
		t.Fatalf("processed(%d) + remaining(%d) = %d, want %d (a job was lost)",
			processed, len(remaining), processed+len(remaining), m)
	}
}

func TestSubmitAfterCancelRejected(t *testing.T) {
	t.Parallel()

	p := New(2, 4)
	p.Cancel()
	if err := p.Submit(Job{ID: 1}); err == nil {
		t.Fatal("Submit after Cancel returned nil, want ErrPoolStopped")
	}
}
```

## Review

The pool is correct when its accounting invariants hold under `-race`. A graceful
`Stop` processes every queued job exactly once: `Processed()` and `Handled()` both
equal M, so no job was dropped and none ran twice. A hard `Cancel` is abrupt but
honest: it exits before finishing all M, and `Processed() + len(remaining)` equals
M exactly, so every job is accounted for as either done or explicitly returned.
The trap this exercise inoculates against is the "lost work" bug — a cancel that
abandons queued jobs without reporting them, so downstream a batch silently
under-processes with no error. The second trap is the closed-channel send panic:
`Submit` must not race `Stop`, which is why the contract keeps submission and
teardown on the same goroutine. Run `go test -count=1 -race ./...`; `-race` is
what catches an unguarded `handled` map or a mis-synchronized counter.

## Resources

- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the Go 1.25 helper that fuses `Add`/`go`/`Done`.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the cancellation signal that drives the hard-stop path.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out over a channel and draining on close.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-errgroup-bounded-fanout.md](03-errgroup-bounded-fanout.md) | Next: [05-goroutine-leak-detection.md](05-goroutine-leak-detection.md)
