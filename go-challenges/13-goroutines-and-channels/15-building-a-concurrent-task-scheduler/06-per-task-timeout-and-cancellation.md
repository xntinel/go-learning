# Exercise 6: Per-Task Deadlines — Bound How Long Any Single Job May Run

A wedged downstream call — a slow DB query, a hung HTTP request — must not pin a
worker forever. This module adds `SubmitWithTimeout`, which derives a per-task
`context.Context` with a deadline and passes it into the task; the worker records
a timeout result and moves on. It also makes the honest point that context
signals, it does not preempt: a task that ignores its context is not actually
stopped.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
per-task-timeout/              module example.com/per-task-timeout
  go.mod                       go 1.25
  scheduler.go                 Task func(ctx); SubmitWithTimeout derives ctx, worker records timeout
  cmd/
    demo/
      main.go                  demo: a timed-out job and a fast job
  scheduler_test.go            DeadlineExceeded, real value, parent-cancel, worker freed, -race
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `Task func(ctx context.Context) (any, error)`; `SubmitWithTimeout(ctx, timeout, Task) <-chan Result` that derives a deadline context (with `defer cancel()`), and a worker that records a timeout `Result` and is freed for the next task.
Test: a task that selects on `ctx.Done()` with a 20 ms timeout yields `context.DeadlineExceeded` and frees the worker; a cooperative task that returns early yields its real value; cancelling the parent context propagates `context.Canceled`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/per-task-timeout/cmd/demo
cd ~/go-exercises/per-task-timeout
go mod init example.com/per-task-timeout
go mod edit -go=1.25
```

### Signal, not preempt — and free the worker either way

`context.WithTimeout` does not stop a running goroutine; Go has no preemptive
cancellation of arbitrary code. It closes `ctx.Done()` and sets `ctx.Err()`. So a
task can only be timed out if it *cooperates* — selects on `ctx.Done()` or passes
`ctx` into the blocking call that does. The task signature here is therefore
`func(ctx context.Context) (any, error)`: the deadline is handed to the task, and
observing it is the task's responsibility.

To keep the worker's accounting honest even when a task is slow to react, the
worker runs the task in a child goroutine and selects on either the task's result
or `ctx.Done()`. When the deadline fires first, the worker delivers a
`DeadlineExceeded` result (via `context.Cause`) and returns to the pool — the
worker *slot* is freed at the deadline. The child goroutine's result channel is
buffered (capacity one), so a task that finishes a moment later can still deliver
without blocking. This is exactly the distinction from the concepts: the deadline
frees the accounting immediately, but the goroutine is only freed if the task
actually observes cancellation. `defer cancel()` on the derived context is what
prevents leaking the timer behind every submitted task.

Create `scheduler.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrShuttingDown is delivered when SubmitWithTimeout races Stop.
var ErrShuttingDown = errors.New("scheduler shutting down")

// Task receives a per-task context carrying its deadline. It should observe
// ctx.Done() to be cancellable.
type Task func(ctx context.Context) (any, error)

type Result struct {
	Value any
	Err   error
}

type task struct {
	parent  context.Context
	timeout time.Duration
	fn      Task
	done    chan Result
}

// Scheduler runs tasks on a worker pool, each under its own deadline.
type Scheduler struct {
	tasks    chan task
	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

// New starts a Scheduler with the given number of workers.
func New(workers int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	s := &Scheduler{
		tasks: make(chan task, workers*2),
		quit:  make(chan struct{}),
	}
	for range workers {
		s.wg.Add(1)
		go s.worker()
	}
	return s
}

func (s *Scheduler) worker() {
	defer s.wg.Done()
	for {
		select {
		case t := <-s.tasks:
			s.run(t)
		case <-s.quit:
			return
		}
	}
}

// run executes one task under a derived deadline context. The worker slot is
// freed when the task returns or the deadline fires, whichever comes first.
func (s *Scheduler) run(t task) {
	ctx, cancel := context.WithTimeout(t.parent, t.timeout)
	defer cancel()

	res := make(chan Result, 1) // buffered: a late task can still deliver
	go func() {
		v, err := t.fn(ctx)
		res <- Result{Value: v, Err: err}
	}()

	select {
	case r := <-res:
		t.done <- r
	case <-ctx.Done():
		t.done <- Result{Err: context.Cause(ctx)}
	}
}

// SubmitWithTimeout enqueues fn to run under a per-task timeout derived from ctx.
func (s *Scheduler) SubmitWithTimeout(ctx context.Context, timeout time.Duration, fn Task) <-chan Result {
	done := make(chan Result, 1)
	if err := ctx.Err(); err != nil {
		done <- Result{Err: err}
		return done
	}
	t := task{parent: ctx, timeout: timeout, fn: fn, done: done}
	select {
	case s.tasks <- t:
	case <-ctx.Done():
		done <- Result{Err: ctx.Err()}
	case <-s.quit:
		done <- Result{Err: ErrShuttingDown}
	}
	return done
}

// Stop signals the workers and joins them.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.quit) })
	s.wg.Wait()
}
```

### The runnable demo

The demo submits a job that would run for an hour but is bounded to 20 ms, and a
fast job that returns immediately.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/per-task-timeout"
)

func main() {
	s := scheduler.New(2)
	defer s.Stop()

	ctx := context.Background()
	slow := s.SubmitWithTimeout(ctx, 20*time.Millisecond, func(c context.Context) (any, error) {
		select {
		case <-time.After(time.Hour):
			return "slow", nil
		case <-c.Done():
			return nil, c.Err()
		}
	})
	fast := s.SubmitWithTimeout(ctx, time.Second, func(c context.Context) (any, error) {
		return "fast-result", nil
	})

	fmt.Println("slow err:", (<-slow).Err)
	fmt.Println("fast value:", (<-fast).Value)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
slow err: context deadline exceeded
fast value: fast-result
```

### Tests

`TestTimeoutFreesWorker` uses a single worker: task A blocks on `ctx.Done()` with a
20 ms timeout, and task B is submitted behind it. Both results arrive — A with
`DeadlineExceeded`, B with its value — proving the worker was freed at A's
deadline. `TestCooperativeReturnsValue` checks an early return yields the real
value. `TestParentCancelPropagates` cancels the parent and asserts
`context.Canceled` reaches the task.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestTimeoutFreesWorker(t *testing.T) {
	t.Parallel()

	s := New(1) // a single worker, so B only runs once A frees it
	defer s.Stop()

	ctx := context.Background()
	aDone := s.SubmitWithTimeout(ctx, 20*time.Millisecond, func(c context.Context) (any, error) {
		<-c.Done()
		return nil, c.Err()
	})
	bDone := s.SubmitWithTimeout(ctx, time.Second, func(c context.Context) (any, error) {
		return "b-value", nil
	})

	if ra := <-aDone; !errors.Is(ra.Err, context.DeadlineExceeded) {
		t.Fatalf("A Err = %v, want DeadlineExceeded", ra.Err)
	}
	if rb := <-bDone; rb.Err != nil || rb.Value != "b-value" {
		t.Fatalf("B = (%v, %v), want (b-value, nil)", rb.Value, rb.Err)
	}
}

func TestCooperativeReturnsValue(t *testing.T) {
	t.Parallel()

	s := New(2)
	defer s.Stop()

	r := <-s.SubmitWithTimeout(context.Background(), time.Second, func(c context.Context) (any, error) {
		return 42, nil
	})
	if r.Err != nil || r.Value != 42 {
		t.Fatalf("result = (%v, %v), want (42, nil)", r.Value, r.Err)
	}
}

func TestParentCancelPropagates(t *testing.T) {
	t.Parallel()

	s := New(2)
	defer s.Stop()

	parent, cancel := context.WithCancel(context.Background())
	d := s.SubmitWithTimeout(parent, time.Hour, func(c context.Context) (any, error) {
		<-c.Done()
		return nil, c.Err()
	})
	cancel()

	if r := <-d; !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", r.Err)
	}
}

func Example() {
	s := New(1)
	defer s.Stop()

	r := <-s.SubmitWithTimeout(context.Background(), time.Second, func(ctx context.Context) (any, error) {
		return "ok", nil
	})
	fmt.Println(r.Value, r.Err)
	// Output: ok <nil>
}
```

## Review

Per-task timeouts are correct when the worker is freed at the deadline and the
task receives a `DeadlineExceeded` (or `Canceled`) result. The load-bearing
insight is that `context.WithTimeout` signals rather than preempts: a task that
never selects on `ctx.Done()` runs to completion no matter the deadline, so the
freed "worker" is an accounting fact, not a stopped goroutine — write tasks that
cooperate. Every derived context is paired with `defer cancel()`, which is what
keeps `-race` free of leaked timer goroutines. The child-goroutine result channel
is buffered so a late-finishing task never blocks on a worker that has already
moved on. Run `go test -race -count=1 ./...`.

## Resources

- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — deriving a deadline context and why `cancel` must always be called.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — reading why a context was cancelled.
- [Go Blog: Contexts](https://go.dev/blog/context) — propagating cancellation and deadlines across API boundaries.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-delayed-and-scheduled-tasks.md](05-delayed-and-scheduled-tasks.md) | Next: [07-panic-recovery-and-retry-backoff.md](07-panic-recovery-and-retry-backoff.md)
