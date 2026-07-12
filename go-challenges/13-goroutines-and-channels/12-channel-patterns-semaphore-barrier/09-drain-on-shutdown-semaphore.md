# Exercise 9: Graceful Drain — Acquiring All Slots to Fence In-Flight Work

Graceful shutdown has two steps: stop accepting new work, and wait for
outstanding work to finish (or a deadline to hit) before the process exits.
Acquiring *all* N slots of the semaphore that guards a worker pool is a clean
proof of the second step — it can only succeed once no task holds a slot, so a
successful full acquire means zero work is in flight. This exercise builds that
drain path with a shutdown budget.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It depends on `golang.org/x/sync`, which the gate fetches.

## What you'll build

```text
drain/                      independent module: example.com/drain
  go.mod                    go 1.26; requires golang.org/x/sync
  drain.go                  type Pool over semaphore.Weighted; Submit, Shutdown
  cmd/
    demo/
      main.go               submit tasks, drain, show post-drain submit rejected
  drain_test.go             drain waits for all; reject-while-draining; deadline (-race)
```

- Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
- Implement: a `Pool` bounded by a weighted semaphore of size N; `Submit` rejects new work once draining begins; `Shutdown(ctx)` acquires all N slots to prove no task is in flight, honoring a deadline.
- Test: start N tasks and assert `Shutdown` returns nil only after all finished; a `Submit` once draining begins returns `ErrDraining`; with a too-short deadline while a task hangs, `Shutdown` returns `context.DeadlineExceeded`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get golang.org/x/sync/semaphore
```

### Why acquiring all slots is a drain proof

The pool guards concurrency with a weighted semaphore of total N. Each accepted
task acquires one unit for the duration of its work and releases it when done, so
at any instant the number of held units equals the number of in-flight tasks.
`Shutdown` first flips a `draining` flag so no new task is admitted, then calls
`Acquire(ctx, N)` — a request for *all* N units. That acquire can only succeed
once every held unit has been released, which is to say once every in-flight task
has finished. So a successful full acquire is a proof that the pool is drained;
`Shutdown` then releases the N units and returns nil.

The deadline is what keeps a stuck task from hanging shutdown forever.
`Acquire(ctx, N)` honors the context: if the shutdown budget elapses while a task
is still holding a unit, the acquire returns `ctx.Err()` (`DeadlineExceeded`) and
`Shutdown` reports that instead of blocking indefinitely — the operator's signal
that some work did not drain in time.

`Submit` rejects new work in two places. It checks `draining` up front, and — to
close the race where `Shutdown` sets the flag between the check and the acquire —
it re-checks after acquiring a unit and releases it back if draining started
meanwhile. The order of a task's cleanup matters for the drain proof: a task
records its completion *before* releasing its unit, so once `Shutdown` has all N
units, every task's completion is already visible.

Create `drain.go`:

```go
package drain

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"
)

// ErrDraining is returned by Submit once Shutdown has begun.
var ErrDraining = errors.New("pool draining: not accepting new work")

// ErrBusy is returned by Submit when all worker slots are currently held.
var ErrBusy = errors.New("pool busy: no free slot")

// Pool runs tasks under a bounded concurrency of n, and drains on Shutdown by
// acquiring all n slots to fence in-flight work.
type Pool struct {
	sem      *semaphore.Weighted
	n        int64
	draining atomic.Bool
	done     atomic.Int64
	wg       sync.WaitGroup
}

// New returns a pool that runs at most n tasks concurrently.
func New(n int64) *Pool {
	return &Pool{sem: semaphore.NewWeighted(n), n: n}
}

// Completed reports how many tasks have finished.
func (p *Pool) Completed() int64 { return p.done.Load() }

// Draining reports whether Shutdown has begun.
func (p *Pool) Draining() bool { return p.draining.Load() }

// Submit runs task in a worker goroutine, or rejects it: ErrDraining if the pool
// is shutting down, ErrBusy if every slot is held.
func (p *Pool) Submit(task func()) error {
	if p.draining.Load() {
		return ErrDraining
	}
	if !p.sem.TryAcquire(1) {
		return ErrBusy
	}
	if p.draining.Load() { // Shutdown may have started between the checks
		p.sem.Release(1)
		return ErrDraining
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer p.sem.Release(1) // released after done is recorded (LIFO defers)
		task()
		p.done.Add(1)
	}()
	return nil
}

// Shutdown stops admitting new work and waits until all in-flight tasks finish
// by acquiring all n slots. It returns ctx.Err() if the deadline elapses first.
func (p *Pool) Shutdown(ctx context.Context) error {
	p.draining.Store(true)
	if err := p.sem.Acquire(ctx, p.n); err != nil {
		return err // e.g. context.DeadlineExceeded: some task did not drain in time
	}
	p.sem.Release(p.n)
	return nil
}
```

### The runnable demo

The demo submits two quick tasks to a size-2 pool, drains it with a generous
deadline, and shows that all tasks completed and a post-drain `Submit` is
rejected with `ErrDraining`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/drain"
)

func main() {
	p := drain.New(2)
	for range 2 {
		_ = p.Submit(func() { time.Sleep(5 * time.Millisecond) })
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := p.Shutdown(ctx)

	rejected := errors.Is(p.Submit(func() {}), drain.ErrDraining)
	fmt.Printf("shutdown_err=%v completed=%d post_drain_rejected=%t\n",
		err, p.Completed(), rejected)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shutdown_err=<nil> completed=2 post_drain_rejected=true
```

### Tests

`TestDrainWaitsForAll` starts four tasks on a size-4 pool, each sleeping briefly,
and asserts `Shutdown` returns nil and `Completed()` equals four — proving the
full acquire fenced all in-flight work. `TestRejectWhileDraining` drains an idle
pool, then asserts a `Submit` returns `ErrDraining`. `TestShutdownDeadline`
submits a task that hangs until the test releases it, calls `Shutdown` with a
short deadline, and asserts it returns `context.DeadlineExceeded`; the hung task
is then released so its goroutine exits cleanly.

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDrainWaitsForAll(t *testing.T) {
	t.Parallel()

	const n = 4
	p := New(n)
	for range n {
		if err := p.Submit(func() { time.Sleep(10 * time.Millisecond) }); err != nil {
			t.Fatalf("Submit err = %v, want nil", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
	if got := p.Completed(); got != n {
		t.Fatalf("Completed = %d after drain, want %d", got, n)
	}
}

func TestRejectWhileDraining(t *testing.T) {
	t.Parallel()

	p := New(4)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}

	if err := p.Submit(func() {}); !errors.Is(err, ErrDraining) {
		t.Fatalf("Submit after drain err = %v, want ErrDraining", err)
	}
}

func TestShutdownDeadline(t *testing.T) {
	t.Parallel()

	p := New(4)
	release := make(chan struct{})
	if err := p.Submit(func() { <-release }); err != nil {
		t.Fatalf("Submit err = %v, want nil", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want DeadlineExceeded", err)
	}

	close(release) // let the hung task finish so its goroutine exits
}
```

## Review

The drain is correct when `Shutdown` returns nil only after every in-flight task
has finished, and returns `context.DeadlineExceeded` when a task overstays the
budget. Acquiring all N slots is the elegant part: it reuses the semaphore that
already bounds concurrency as a barrier that fences in-flight work, with no extra
bookkeeping. Recording each task's completion before releasing its slot is what
makes `Completed()` accurate the moment the full acquire succeeds — reverse that
order and a task's slot could free before its effect is visible. The double
`draining` check in `Submit` closes the race where a shutdown starts mid-submit.
Run `-race`, and note the deadline test releases the hung task so no goroutine
leaks past the test.

## Resources

- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — `Acquire`, `TryAcquire`, `Release`; acquiring the full weight as a drain barrier.
- [net/http: Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the standard-library precedent for deadline-bounded graceful drain.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounding work and honoring a cancellation deadline.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-startup-readiness-barrier.md](08-startup-readiness-barrier.md) | Next: [10-semaphore-vs-workerpool-tradeoffs.md](10-semaphore-vs-workerpool-tradeoffs.md)
