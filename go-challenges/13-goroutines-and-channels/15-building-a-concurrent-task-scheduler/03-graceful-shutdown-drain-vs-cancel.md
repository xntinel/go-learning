# Exercise 3: Graceful Shutdown — Drain In-Flight vs. Hard-Cancel

During a rolling deploy a service needs two distinct termination verbs, exactly
like `net/http.Server`: `Shutdown(ctx)` stops accepting new work and waits for
in-flight tasks to finish (bounded by a deadline), while `Close()` cancels running
tasks immediately. This module builds both and makes them idempotent, so a
deploy neither drops in-flight work nor hangs forever.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
graceful-shutdown/             module example.com/graceful-shutdown
  go.mod                       go 1.25
  scheduler.go                 Shutdown(ctx) drain; Close() hard-cancel; both idempotent
  cmd/
    demo/
      main.go                  demo: drain finishes jobs; Close cancels a wedged job
  scheduler_test.go            drain-nil, drain-deadline, idempotent, -race
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `Shutdown(ctx) error` (drain, returns `ctx.Err()` on timeout); `Close() error` (cancel running tasks via `context.CancelCauseFunc`); both idempotent.
Test: submit sleeping tasks, `Shutdown` with a generous deadline, assert all results arrive and it returns nil; repeat with a deadline shorter than task duration and assert `context.DeadlineExceeded` while workers still join eventually; assert a second `Shutdown`/`Close` does not panic.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Two verbs, one drain primitive

The design turns on tracking *accepted tasks*, not workers. An `inflight`
`sync.WaitGroup` counts every task from the moment `Submit` accepts it (before it
is even enqueued) until a worker finishes it. Both shutdown verbs are then built
on "wait for `inflight` to reach zero":

- `Shutdown(ctx)` flips the `closed` flag (no new submits), then waits for
  `inflight` to drain — but only until `ctx` is done. If the drain completes
  first, it stops the workers and returns nil; if the deadline fires first, it
  returns `ctx.Err()` and leaves the workers draining (they join on a later
  `Shutdown` or `Close`). This mirrors `http.Server.Shutdown`: finish what you
  started, bounded by a deadline.
- `Close()` cancels the shared base context via a `context.CancelCauseFunc`, so
  every running (and every still-queued) task observes cancellation through
  `ctx.Done()` and returns fast; then it drains and joins. This mirrors
  `http.Server.Close`: stop now.

Crucially, the task channel is *never closed*. Closing a channel that producers
may still send to is the send-on-closed panic; instead workers exit by observing a
`workerQuit` channel closed exactly once via `sync.Once`. Because tasks carry the
base context, `Close`'s cancellation reaches even a queued task: a worker that
picks it up after the cancel runs it with an already-cancelled context, so the
task returns immediately and its slot is accounted. Idempotency falls out of three
mechanisms: the `closed` flag is a set (setting it twice is harmless), `cancel` is
safe to call repeatedly, and `sync.Once` guards the single channel close.

The base context uses `context.WithCancelCause`, so a cancelled task can report
*why* it was cancelled via `context.Cause(ctx)` — here the sentinel `ErrClosed`.

Create `scheduler.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"sync"
)

var (
	// ErrShuttingDown is returned by Submit after Shutdown or Close.
	ErrShuttingDown = errors.New("scheduler shutting down")
	// ErrClosed is the cancellation cause attached by Close.
	ErrClosed = errors.New("scheduler closed")
)

// Task is a unit of work that must cooperate with ctx cancellation.
type Task func(ctx context.Context) (any, error)

type Result struct {
	Value any
	Err   error
}

type task struct {
	fn   Task
	done chan Result
}

// Scheduler supports graceful drain (Shutdown) and hard cancel (Close).
type Scheduler struct {
	tasks      chan task
	baseCtx    context.Context
	cancel     context.CancelCauseFunc
	workerQuit chan struct{}
	quitOnce   sync.Once

	workerWG sync.WaitGroup // worker goroutines
	inflight sync.WaitGroup // accepted-but-not-finished tasks

	mu     sync.Mutex
	closed bool
}

// New starts a Scheduler with the given number of workers.
func New(workers int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	s := &Scheduler{
		tasks:      make(chan task, workers*2),
		baseCtx:    ctx,
		cancel:     cancel,
		workerQuit: make(chan struct{}),
	}
	for range workers {
		s.workerWG.Add(1)
		go s.worker()
	}
	return s
}

func (s *Scheduler) worker() {
	defer s.workerWG.Done()
	for {
		select {
		case t := <-s.tasks:
			v, err := t.fn(s.baseCtx)
			t.done <- Result{Value: v, Err: err}
			s.inflight.Done()
		case <-s.workerQuit:
			return
		}
	}
}

// Submit accepts fn unless the scheduler is shutting down.
func (s *Scheduler) Submit(fn Task) (<-chan Result, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrShuttingDown
	}
	done := make(chan Result, 1)
	s.inflight.Add(1)
	s.mu.Unlock()

	s.tasks <- task{fn: fn, done: done}
	return done, nil
}

func (s *Scheduler) stopWorkers() {
	s.quitOnce.Do(func() { close(s.workerQuit) })
	s.workerWG.Wait()
}

// Shutdown stops accepting work and waits for in-flight tasks to finish. If ctx
// is done before the drain completes it returns ctx.Err(); the workers keep
// draining and join on a later Shutdown or Close.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	drained := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		s.stopWorkers()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close cancels running tasks immediately, then drains and joins. It is
// idempotent and safe to call after a timed-out Shutdown.
func (s *Scheduler) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()

	s.cancel(ErrClosed)
	s.inflight.Wait()
	s.stopWorkers()
	return nil
}
```

### The runnable demo

The demo shows both paths: a graceful `Shutdown` that lets three short jobs finish,
then a `Close` that cancels a job which would otherwise run for an hour. The
cancelled job reports its cause via `context.Cause`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/graceful-shutdown"
)

func main() {
	// Graceful drain: in-flight work finishes.
	s := scheduler.New(2)
	var dones []<-chan scheduler.Result
	for i := range 3 {
		d, _ := s.Submit(func(ctx context.Context) (any, error) {
			time.Sleep(10 * time.Millisecond)
			return fmt.Sprintf("job-%d done", i), nil
		})
		dones = append(dones, d)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		fmt.Println("shutdown error:", err)
	}
	for _, d := range dones {
		fmt.Println((<-d).Value)
	}

	// Hard cancel: a wedged job is aborted immediately.
	s2 := scheduler.New(1)
	d, _ := s2.Submit(func(ctx context.Context) (any, error) {
		select {
		case <-time.After(time.Hour):
			return "never", nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	})
	s2.Close()
	fmt.Println("hard cancel:", (<-d).Err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job-0 done
job-1 done
job-2 done
hard cancel: scheduler closed
```

### Tests

`TestShutdownDrains` submits sleeping tasks and shuts down with a generous
deadline, asserting every result arrives and `Shutdown` returns nil.
`TestShutdownDeadline` uses a deadline shorter than the task duration and asserts
`context.DeadlineExceeded`, then proves the workers still join by calling `Close`.
`TestIdempotentShutdownClose` proves a second `Shutdown` and a following `Close` do
not panic on a double-close.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func cooperativeSleep(d time.Duration) Task {
	return func(ctx context.Context) (any, error) {
		select {
		case <-time.After(d):
			return "done", nil
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

func TestShutdownDrains(t *testing.T) {
	t.Parallel()

	s := New(4)
	var dones []<-chan Result
	for range 6 {
		d, err := s.Submit(cooperativeSleep(20 * time.Millisecond))
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		dones = append(dones, d)
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}
	for i, d := range dones {
		if r := <-d; r.Err != nil || r.Value != "done" {
			t.Fatalf("task %d: got (%v, %v), want (done, nil)", i, r.Value, r.Err)
		}
	}

	// Submitting after shutdown is rejected.
	if _, err := s.Submit(cooperativeSleep(time.Millisecond)); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Submit after Shutdown = %v, want ErrShuttingDown", err)
	}
}

func TestShutdownDeadline(t *testing.T) {
	t.Parallel()

	s := New(2)
	for range 4 {
		if _, err := s.Submit(cooperativeSleep(500 * time.Millisecond)); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want DeadlineExceeded", err)
	}

	// Workers still join eventually: Close cancels the long tasks and joins.
	if err := s.Close(); err != nil {
		t.Fatalf("Close after timed-out Shutdown = %v, want nil", err)
	}
}

func TestIdempotentShutdownClose(t *testing.T) {
	t.Parallel()

	s := New(2)
	if _, err := s.Submit(cooperativeSleep(10 * time.Millisecond)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown = %v, want nil", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close after Shutdown = %v, want nil", err)
	}
}

func TestCloseCancelsRunning(t *testing.T) {
	t.Parallel()

	s := New(1)
	d, err := s.Submit(cooperativeSleep(time.Hour))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if r := <-d; !errors.Is(r.Err, ErrClosed) {
		t.Fatalf("cancelled task Err = %v, want ErrClosed", r.Err)
	}
}
```

## Review

The two verbs are correct when they mirror `http.Server`: `Shutdown` finishes
in-flight work bounded by a deadline (returning `context.DeadlineExceeded` if the
drain overruns), and `Close` cancels running tasks immediately via the base
context's `CancelCauseFunc`. Both are built on one drain primitive —
`inflight.Wait()` — and stay idempotent because the channel close is guarded by
`sync.Once`, `cancel` is safe to repeat, and `closed` is a set. The task channel
is never closed, so a late `Submit` cannot panic; it is rejected with
`ErrShuttingDown`. The subtle failure this design avoids is a `Shutdown` that
stops workers before in-flight tasks finish, dropping their results; here the
workers are only stopped after `inflight` reaches zero. Run `go test -race
-count=1 ./...`.

## Resources

- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — the graceful-drain contract this mirrors.
- [`net/http.Server.Close`](https://pkg.go.dev/net/http#Server.Close) — the hard-cancel counterpart.
- [`context.WithCancelCause` and `context.Cause`](https://pkg.go.dev/context#WithCancelCause) — cancelling with an attached reason.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-nonblocking-submit-backpressure.md](02-nonblocking-submit-backpressure.md) | Next: [04-priority-scheduler-heap.md](04-priority-scheduler-heap.md)
