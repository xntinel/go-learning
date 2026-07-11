# Exercise 6: Cancellable Worker Pool (One Cancel Drains Every Worker)

A bounded pool of workers pulling jobs off a channel is the shape of nearly every
queue consumer in a backend. The lifecycle question is shutdown: a single
`cancel()` — or a closed input — must make every worker finish its current job and
exit, and a `Wait()` must block until all of them have returned. Get this right
and rolling a deploy drains cleanly; get it wrong and you either drop in-flight
jobs or hang on shutdown.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
pool/                      independent module: example.com/pool
  go.mod                   module example.com/pool
  pool.go                  Pool; New; Start(ctx); Wait()
  cmd/
    demo/
      main.go              3 workers drain 6 jobs; prints the total
  pool_test.go             drains-all, cancel-stops, no-goroutine-leak
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a `Pool` whose `Start(ctx)` launches a fixed set of context-aware workers over a shared input channel, and whose `Wait()` blocks until every worker has returned.
- Test: closing the input drains every job exactly once; a cancel mid-stream stops the workers promptly with no job run after cancel; no goroutine leaks after `Wait`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p pool/cmd/demo
cd pool
go mod init example.com/pool
```

### Why one shared context is enough

A pool is a fan-out: `N` worker goroutines, each running the same loop, all
reading from one input channel. The elegant part is that they can share a *single*
context. Contexts are safe for concurrent use, so one `Start(ctx)` hands the same
`ctx` to every worker, and one `cancel()` upstream reaches all of them at the same
instant — no per-worker signalling, no broadcast channel, no counting. This is
exactly the property the concepts file calls out: shared cancellation is what
makes worker pools trivial.

Each worker loops on a `select` with two arms: `ctx.Done()` and the input channel.
That gives it two ways to stop, and they mean different things. A closed input
means "no more work is coming" — the worker drains whatever remains (a receive
from a closed buffered channel keeps returning buffered values until empty, then
returns `ok == false`) and exits cleanly. A cancelled context means "stop now" —
the worker finishes the job it is currently handling (the `handle` call already in
progress is not preempted) and then, on its next loop, the `Done()` arm fires and
it returns. The difference matters operationally: a graceful drain lets queued
work finish, while a cancel is the "we are shutting down, stop taking new work"
lever.

`Wait()` is a `sync.WaitGroup` wait. `Start` does `wg.Add(1)` per worker and each
worker `defer wg.Done()`s, so `Wait` returns precisely when the last worker has
left its loop. That is the join point a graceful shutdown blocks on: cancel, then
`Wait`, and you know every worker is done.

One correctness note on `select` with both arms ready: if the context is cancelled
*and* a job is waiting, `select` may pick either. That is acceptable here — the
pool is allowed to process one more job during shutdown or to stop immediately;
what it must never do is process a job *after* it has observed `Done` and decided
to exit. The loop structure guarantees that: once the `Done()` arm is taken, the
worker returns and reads nothing further.

Create `pool.go`:

```go
package pool

import (
	"context"
	"sync"
)

// Pool fans jobs from a shared input channel across a fixed number of
// context-aware workers. A closed input drains the workers; a cancelled context
// stops them after their current job. Wait blocks until all workers return.
type Pool struct {
	workers int
	in      <-chan int
	handle  func(context.Context, int)
	wg      sync.WaitGroup
}

// New builds a pool of the given size that calls handle for each job read from
// in.
func New(workers int, in <-chan int, handle func(context.Context, int)) *Pool {
	return &Pool{workers: workers, in: in, handle: handle}
}

// Start launches the workers. Each selects on ctx.Done() and the input channel,
// so a single cancel stops the whole pool.
func (p *Pool) Start(ctx context.Context) {
	for range p.workers {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-p.in:
					if !ok {
						return
					}
					p.handle(ctx, job)
				}
			}
		}()
	}
}

// Wait blocks until every worker has returned.
func (p *Pool) Wait() {
	p.wg.Wait()
}
```

### The runnable demo

The demo feeds six jobs into a buffered channel, closes it, and lets three workers
drain it. The count of processed jobs is deterministic even though the
worker-to-job assignment is not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/pool"
)

func main() {
	in := make(chan int, 6)
	for i := 0; i < 6; i++ {
		in <- i
	}
	close(in)

	var processed atomic.Int64
	p := pool.New(3, in, func(ctx context.Context, job int) {
		processed.Add(1)
	})

	p.Start(context.Background())
	p.Wait()

	fmt.Println("processed", processed.Load(), "jobs")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed 6 jobs
```

### Tests

`TestProcessesAllJobsThenDrains` feeds `N` jobs, closes the input, waits, and
asserts every job was handled exactly once (a mutex-guarded map records the
counts). `TestCancelStopsWorkers` starts workers on an input that never delivers,
cancels, and requires `Wait` to return promptly. `TestNoGoroutineLeak` runs a pool
to completion and polls `runtime.NumGoroutine()` back to baseline.

Create `pool_test.go`:

```go
package pool

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestProcessesAllJobsThenDrains(t *testing.T) {
	t.Parallel()

	const n = 50
	in := make(chan int, n)
	for i := range n {
		in <- i
	}
	close(in)

	var mu sync.Mutex
	seen := make(map[int]int)
	p := New(4, in, func(ctx context.Context, job int) {
		mu.Lock()
		seen[job]++
		mu.Unlock()
	})

	p.Start(context.Background())
	p.Wait()

	if len(seen) != n {
		t.Fatalf("handled %d distinct jobs, want %d", len(seen), n)
	}
	for job, count := range seen {
		if count != 1 {
			t.Fatalf("job %d handled %d times, want 1", job, count)
		}
	}
}

func TestCancelStopsWorkers(t *testing.T) {
	t.Parallel()

	in := make(chan int) // never delivers
	p := New(4, in, func(ctx context.Context, job int) {})

	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)

	cancel()

	done := make(chan struct{})
	go func() {
		p.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait did not return within 1s of cancel")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	t.Parallel()

	base := runtime.NumGoroutine()

	in := make(chan int, 10)
	for i := range 10 {
		in <- i
	}
	close(in)

	p := New(3, in, func(ctx context.Context, job int) {})
	p.Start(context.Background())
	p.Wait()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: NumGoroutine=%d, base=%d",
				runtime.NumGoroutine(), base)
		}
		time.Sleep(time.Millisecond)
	}
}
```

## Review

The pool is correct when a closed input drains every job exactly once and a cancel
stops the workers after their current job with `Wait` returning promptly. The
"exactly once" check in `TestProcessesAllJobsThenDrains` is what proves the
workers cooperate over one channel without dropping or double-handling — and it
must run under `-race`, because the handler writes shared state from four
goroutines and needs a mutex (or atomics). The shared-context design is the point:
resist adding a per-worker done channel; one `ctx` reaches all workers at once.
Guard against the two failure modes the tests encode: a worker that keeps
processing after observing `Done` (it must return, not loop again) and a `Wait`
that hangs because a worker never exits. Run `go test -race`.

## Resources

- [context package](https://pkg.go.dev/context) — one shared context across many goroutines.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — the join that `Wait` is built on.
- [Go Blog: Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in and shared cancellation.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-aftercancel-cleanup-hook.md](07-aftercancel-cleanup-hook.md)
