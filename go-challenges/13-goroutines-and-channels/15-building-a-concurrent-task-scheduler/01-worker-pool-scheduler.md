# Exercise 1: The Core Worker-Pool Scheduler

This is the foundational background-job runner a backend service uses to run work
off the request path: a bounded job queue feeding a fixed worker pool, each
`Submit` returning a per-task result channel, plus `Stop` (signal, then join
workers), `Done`, and a race-free `Stats` snapshot. Everything else in this
chapter is a variation on this core.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
worker-pool-scheduler/         module example.com/worker-pool-scheduler
  go.mod                       go 1.25
  scheduler.go                 Scheduler: New, Submit, Stop, Done, Stats; Task, Result
  cmd/
    demo/
      main.go                  runnable demo: submit tasks, read results, print Stats
  scheduler_test.go            table tests: results, errors, ErrShuttingDown, Stats, 100 tasks, cancelled ctx
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `New(workers)`, `Submit(ctx, Task) <-chan Result`, `Stop()`, `Done() <-chan struct{}`, `Stats() Stats`.
Test: submit N tasks and read N results; assert a `Value` and an error via `errors.Is`; assert `Submit` after `Stop` yields `ErrShuttingDown`; assert `Stats().Workers`; drive 100 tasks through 8 workers; cancel the context before `Submit` and assert `context.Canceled`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/15-building-a-concurrent-task-scheduler/01-worker-pool-scheduler/cmd/demo
cd go-solutions/13-goroutines-and-channels/15-building-a-concurrent-task-scheduler/01-worker-pool-scheduler
go mod edit -go=1.25
```

### The design contract

`Submit` returns a single `<-chan Result` — one value the caller reads once. That
single-return signature matters: an earlier version of this scheduler mixed a
two-value `done, err := Submit(...)` in its tests against a one-value signature,
which does not compile. Here the contract is uniform: `Submit` always hands back a
channel, and every failure mode (cancelled context, shutdown) is delivered *on
that channel* as a `Result` with a non-nil `Err`, never as a second return value.
The result channel is buffered with capacity one so a worker can always deliver
the outcome and move on, even if the caller has stopped reading.

Three details make this race-free under `-race`. First, the enqueue is a `select`
over three cases — the send, `ctx.Done()`, and a `quit` channel — so the caller's
context aborts a full-queue enqueue and a concurrent `Stop` cannot make `Submit`
block forever. Second, the scheduler *never closes the task channel*; workers exit
by observing the closed `quit` channel instead. Closing a channel that producers
may still send to is the classic send-on-closed-channel panic, and the `quit`
pattern sidesteps it entirely: `s.tasks <- t` can never panic because `s.tasks` is
never closed. Third, the in-flight gauge is derived as `started − finished` from
two monotonic atomics, so `Stats().Running` can never read negative or disagree
with itself under a concurrent observer.

`Stop` is the join: it flips a `closed` flag under a mutex (so it is idempotent —
a second `Stop` is a no-op, not a double-close panic), closes `quit` to wake the
workers, waits on the `WaitGroup` until every worker has actually returned, and
only then closes `done`. A `Stop` that signaled without `wg.Wait()` would return
while workers were still running — the leak the concepts warn about.

Create `scheduler.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// ErrShuttingDown is returned (on the result channel) when Submit is called
// after Stop.
var ErrShuttingDown = errors.New("scheduler shutting down")

// Task is a unit of background work. It returns a value and an error.
type Task func() (any, error)

// Result is the outcome of a Task, delivered on the channel Submit returns.
type Result struct {
	Value any
	Err   error
}

type task struct {
	fn   Task
	done chan Result
}

// Scheduler runs tasks on a fixed worker pool fed by a bounded queue.
type Scheduler struct {
	workers int
	tasks   chan task
	quit    chan struct{} // closed by Stop to wake workers
	done    chan struct{} // closed after all workers exit
	wg      sync.WaitGroup

	mu     sync.Mutex
	closed bool

	queued   atomic.Int64
	started  atomic.Int64
	finished atomic.Int64
}

// New starts a Scheduler with the given number of workers (at least one).
func New(workers int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	s := &Scheduler{
		workers: workers,
		tasks:   make(chan task, workers*2),
		quit:    make(chan struct{}),
		done:    make(chan struct{}),
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
			s.started.Add(1)
			v, err := t.fn()
			s.finished.Add(1)
			t.done <- Result{Value: v, Err: err}
		case <-s.quit:
			return
		}
	}
}

// Submit enqueues fn and returns a capacity-1 channel that will carry its
// Result. A cancelled context or a stopped scheduler yields a Result with a
// non-nil Err instead of blocking or panicking.
func (s *Scheduler) Submit(ctx context.Context, fn Task) <-chan Result {
	done := make(chan Result, 1)

	if err := ctx.Err(); err != nil {
		done <- Result{Err: err}
		return done
	}

	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		done <- Result{Err: ErrShuttingDown}
		return done
	}

	s.queued.Add(1)
	select {
	case s.tasks <- task{fn: fn, done: done}:
		s.queued.Add(-1)
	case <-ctx.Done():
		s.queued.Add(-1)
		done <- Result{Err: ctx.Err()}
	case <-s.quit:
		s.queued.Add(-1)
		done <- Result{Err: ErrShuttingDown}
	}
	return done
}

// Stop rejects new submits, wakes the workers, and blocks until every worker has
// returned. It is idempotent.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.quit)
	s.mu.Unlock()

	s.wg.Wait()
	close(s.done)
}

// Done is closed once Stop has joined all workers.
func (s *Scheduler) Done() <-chan struct{} { return s.done }

// Stats is a coherent point-in-time snapshot of the scheduler.
type Stats struct {
	Workers  int
	Queued   int64
	Running  int64
	Finished int64
}

// Stats returns a snapshot built from atomic loads. Running is derived as
// started minus finished so it can never read negative.
func (s *Scheduler) Stats() Stats {
	started := s.started.Load()
	finished := s.finished.Load()
	return Stats{
		Workers:  s.workers,
		Queued:   s.queued.Load(),
		Running:  started - finished,
		Finished: finished,
	}
}
```

### The runnable demo

The demo submits three tasks, reads each result synchronously, and prints a final
`Stats` snapshot. Because each result is read before the next submit, the output
order is deterministic, and `finished` is exactly three by the time `Stats` runs.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/worker-pool-scheduler"
)

func main() {
	s := scheduler.New(4)
	defer s.Stop()

	ctx := context.Background()
	for _, n := range []int{2, 3, 4} {
		r := <-s.Submit(ctx, func() (any, error) {
			return n * n, nil
		})
		fmt.Printf("%d squared = %v\n", n, r.Value)
	}

	st := s.Stats()
	fmt.Printf("workers=%d finished=%d\n", st.Workers, st.Finished)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
2 squared = 4
3 squared = 9
4 squared = 16
workers=4 finished=3
```

### Tests

The table preserves the original lesson's cases and repairs them to the one-value
`Submit` signature: every case reads a `Result` off the returned channel.
`TestSchedulerReportsError` asserts the task's error survives round-trip via
`errors.Is`. `TestSchedulerStopRejectsSubmits` proves a post-`Stop` submit yields
`ErrShuttingDown`. `TestSchedulerHandlesManyTasks` drives 100 tasks through 8
workers. The addendum `TestSchedulerSubmitsWithCancelledContext` cancels the
context *before* `Submit` and asserts `context.Canceled` — the "Submit respects
its context" contract.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"testing"
)

func TestSchedulerReportsResult(t *testing.T) {
	t.Parallel()

	s := New(2)
	defer s.Stop()

	r := <-s.Submit(context.Background(), func() (any, error) {
		return "hello", nil
	})
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.Value != "hello" {
		t.Fatalf("Value = %v, want hello", r.Value)
	}
}

func TestSchedulerReportsError(t *testing.T) {
	t.Parallel()

	s := New(2)
	defer s.Stop()

	want := errors.New("boom")
	r := <-s.Submit(context.Background(), func() (any, error) {
		return nil, want
	})
	if !errors.Is(r.Err, want) {
		t.Fatalf("Err = %v, want %v", r.Err, want)
	}
}

func TestSchedulerStopRejectsSubmits(t *testing.T) {
	t.Parallel()

	s := New(2)
	s.Stop()

	r := <-s.Submit(context.Background(), func() (any, error) { return nil, nil })
	if !errors.Is(r.Err, ErrShuttingDown) {
		t.Fatalf("Err = %v, want ErrShuttingDown", r.Err)
	}
}

func TestSchedulerStats(t *testing.T) {
	t.Parallel()

	s := New(4)
	defer s.Stop()

	if got := s.Stats().Workers; got != 4 {
		t.Fatalf("Workers = %d, want 4", got)
	}
}

func TestSchedulerHandlesManyTasks(t *testing.T) {
	t.Parallel()

	s := New(8)
	defer s.Stop()

	for i := range 100 {
		r := <-s.Submit(context.Background(), func() (any, error) {
			return i * i, nil
		})
		if r.Err != nil {
			t.Fatalf("task %d: %v", i, r.Err)
		}
		if r.Value != i*i {
			t.Fatalf("task %d: Value = %v, want %d", i, r.Value, i*i)
		}
	}

	if got := s.Stats().Finished; got != 100 {
		t.Fatalf("Finished = %d, want 100", got)
	}
}

func TestSchedulerSubmitsWithCancelledContext(t *testing.T) {
	t.Parallel()

	s := New(2)
	defer s.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Submit

	r := <-s.Submit(ctx, func() (any, error) { return "ran", nil })
	if !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("Err = %v, want context.Canceled", r.Err)
	}
	if r.Value != nil {
		t.Fatalf("Value = %v, want nil for a cancelled submit", r.Value)
	}
}
```

## Review

The core is correct when three invariants hold. `Submit` always returns a channel
that eventually carries exactly one `Result`, and every failure path (cancelled
context, shutdown) delivers that failure on the channel rather than as a second
return value or a panic. `Stop` returns only after `wg.Wait()` has joined every
worker — the difference between a clean shutdown and a leaked goroutine that a
`-race` run will surface. And `Stats().Running` is derived as `started − finished`
so it is always a coherent in-flight count. The most common way to break this is
to close the task channel from `Stop` and then race a `Submit` into a
send-on-closed panic; the `quit`-channel design avoids that class of bug because
`s.tasks` is never closed. Run `go test -race -count=1 ./...` and `go vet ./...`;
both must be clean before this scheduler is shippable.

## Resources

- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) — worker pools, channels, and `WaitGroup`.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — result channels and shutting a pipeline down cleanly.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` for the race-free counters behind `Stats`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-nonblocking-submit-backpressure.md](02-nonblocking-submit-backpressure.md)
