# Exercise 2: Backpressure — Reject or Fast-Fail When the Queue Is Full

An ingestion handler that blocks the request goroutine on a full job queue turns a
downstream slowdown into a request pileup and, eventually, an out-of-memory kill.
This module builds the two admission paths a production scheduler needs: a
non-blocking `TrySubmit` that sheds load with `ErrQueueFull`, and a blocking
`Submit` that never parks longer than the caller's context deadline.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
nonblocking-submit-backpressure/   module example.com/nonblocking-submit-backpressure
  go.mod                           go 1.25
  scheduler.go                     TrySubmit (select+default), Submit (ctx deadline), QueueDepth
  cmd/
    demo/
      main.go                      demo: fill the queue, watch TrySubmit shed
  scheduler_test.go                ErrQueueFull, DeadlineExceeded, drain, depth==0
```

Files: `scheduler.go`, `cmd/demo/main.go`, `scheduler_test.go`.
Implement: `TrySubmit(Task) (<-chan Result, error)` returning `ErrQueueFull`; `Submit(ctx, Task) (<-chan Result, error)` honoring the deadline; `QueueDepth() int64`.
Test: fill the queue with gated tasks; assert `TrySubmit` returns `ErrQueueFull` immediately; assert blocking `Submit` with a 10 ms deadline returns `context.DeadlineExceeded`; release the gate and assert the parked tasks complete and depth returns to 0.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Two admission APIs, one bounded queue

Here `Submit` and `TrySubmit` return `(<-chan Result, error)` — a two-value
signature that is correct *because the error is an admission error*, raised before
the task is ever enqueued and distinct from the task's own result (which still
arrives on the channel). `TrySubmit` fails with `ErrQueueFull`; blocking `Submit`
fails with the context's error. This is the opposite of Exercise 1's uniform
one-value contract, and the difference is meaningful: an admission decision ("we
refused to accept this work") is not a task outcome and should not masquerade as
one.

`TrySubmit` is a `select` with a `default` clause — the canonical non-blocking
send. If the queue channel has room, the send succeeds; otherwise the `default`
runs immediately and returns `ErrQueueFull` without ever parking the caller's
goroutine. That is load shedding: an overloaded service returns "try again" now
rather than accumulating unbounded backlog.

Blocking `Submit` is a `select` over the send and `ctx.Done()`. It will wait for
queue space — but only until the caller's deadline. An HTTP handler passes a
context derived from its request budget; when the queue stays full past that
budget, `Submit` returns `context.DeadlineExceeded` and the handler answers `503`
instead of holding the request open indefinitely. The queue-depth counter is an
atomic incremented on a successful enqueue and decremented when a worker dequeues,
so `QueueDepth()` is a consistent read under `-race` and returns to zero once the
backlog drains.

Create `scheduler.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	// ErrQueueFull is returned by TrySubmit when the bounded queue has no room.
	ErrQueueFull = errors.New("task queue full")
	// ErrShuttingDown is returned once the scheduler has been stopped.
	ErrShuttingDown = errors.New("scheduler shutting down")
)

type Task func() (any, error)

type Result struct {
	Value any
	Err   error
}

type task struct {
	fn   Task
	done chan Result
}

// Scheduler is a worker pool with an explicit bounded queue and two admission
// policies: fast-fail (TrySubmit) and block-with-deadline (Submit).
type Scheduler struct {
	tasks chan task
	quit  chan struct{}
	wg    sync.WaitGroup

	mu     sync.Mutex
	closed bool

	depth atomic.Int64
}

// New starts workers reading from a queue with the given capacity.
func New(workers, queueSize int) *Scheduler {
	if workers < 1 {
		workers = 1
	}
	if queueSize < 0 {
		queueSize = 0
	}
	s := &Scheduler{
		tasks: make(chan task, queueSize),
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
			s.depth.Add(-1)
			v, err := t.fn()
			t.done <- Result{Value: v, Err: err}
		case <-s.quit:
			return
		}
	}
}

func (s *Scheduler) admit() (chan Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false
	}
	return make(chan Result, 1), true
}

// TrySubmit enqueues fn without blocking. It returns ErrQueueFull if the queue
// is full and ErrShuttingDown if the scheduler has stopped.
func (s *Scheduler) TrySubmit(fn Task) (<-chan Result, error) {
	done, ok := s.admit()
	if !ok {
		return nil, ErrShuttingDown
	}
	select {
	case s.tasks <- task{fn: fn, done: done}:
		s.depth.Add(1)
		return done, nil
	default:
		return nil, ErrQueueFull
	}
}

// Submit blocks until there is queue space or ctx is done. It returns ctx.Err()
// (e.g. context.DeadlineExceeded) if the deadline passes first.
func (s *Scheduler) Submit(ctx context.Context, fn Task) (<-chan Result, error) {
	done, ok := s.admit()
	if !ok {
		return nil, ErrShuttingDown
	}
	select {
	case s.tasks <- task{fn: fn, done: done}:
		s.depth.Add(1)
		return done, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.quit:
		return nil, ErrShuttingDown
	}
}

// QueueDepth reports how many tasks are enqueued but not yet dequeued.
func (s *Scheduler) QueueDepth() int64 { return s.depth.Load() }

// Stop wakes the workers and joins them.
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
}
```

### The runnable demo

The demo uses a single worker and a single queue slot, then blocks the worker on a
gate so the state is deterministic: one task occupies the worker, one fills the
queue slot, and the third `TrySubmit` sheds.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/nonblocking-submit-backpressure"
)

func main() {
	s := scheduler.New(1, 1) // one worker, one queue slot
	defer s.Stop()

	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	block := func() (any, error) {
		started <- struct{}{}
		<-gate
		return "ok", nil
	}

	// Occupy the single worker, and wait until it is actually running.
	s.TrySubmit(block)
	<-started

	// Fill the single queue slot.
	s.TrySubmit(block)

	// Queue is now full: this must shed.
	if _, err := s.TrySubmit(block); err != nil {
		fmt.Println("shed:", err)
	}
	fmt.Println("queue depth:", s.QueueDepth())

	close(gate)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shed: task queue full
queue depth: 1
```

### Tests

The test fills two workers and a two-slot queue with gated tasks, proving that
`TrySubmit` sheds and blocking `Submit` times out while the queue is saturated,
then releases the gate and confirms every parked task completes and the depth
counter returns to zero.

Create `scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackpressure(t *testing.T) {
	t.Parallel()

	s := New(2, 2) // 2 workers, queue buffer 2
	defer s.Stop()

	gate := make(chan struct{})
	started := make(chan struct{}, 2)
	block := func() (any, error) {
		started <- struct{}{}
		<-gate
		return "done", nil
	}

	var dones []<-chan Result

	// Occupy both workers and wait until both are actually running.
	for range 2 {
		d, err := s.TrySubmit(block)
		if err != nil {
			t.Fatalf("TrySubmit (worker fill): %v", err)
		}
		dones = append(dones, d)
	}
	for range 2 {
		<-started
	}

	// Fill the queue buffer (2 slots).
	for range 2 {
		d, err := s.TrySubmit(block)
		if err != nil {
			t.Fatalf("TrySubmit (queue fill): %v", err)
		}
		dones = append(dones, d)
	}

	// The queue is now full: TrySubmit must fast-fail immediately.
	if _, err := s.TrySubmit(block); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("TrySubmit on full queue: err = %v, want ErrQueueFull", err)
	}

	// Blocking Submit with a short deadline must time out, not park forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.Submit(ctx, block); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Submit on full queue: err = %v, want DeadlineExceeded", err)
	}

	// Release the gate: every parked task must complete.
	close(gate)
	for i, d := range dones {
		r := <-d
		if r.Err != nil || r.Value != "done" {
			t.Fatalf("task %d: got (%v, %v), want (done, nil)", i, r.Value, r.Err)
		}
	}

	// The backlog has drained, so depth must be back to zero.
	if got := s.QueueDepth(); got != 0 {
		t.Fatalf("QueueDepth after drain = %d, want 0", got)
	}
}

func TestTrySubmitEmptyQueueSucceeds(t *testing.T) {
	t.Parallel()

	s := New(1, 1)
	defer s.Stop()

	d, err := s.TrySubmit(func() (any, error) { return 7, nil })
	if err != nil {
		t.Fatalf("TrySubmit on empty queue: %v", err)
	}
	if r := <-d; r.Err != nil || r.Value != 7 {
		t.Fatalf("result = (%v, %v), want (7, nil)", r.Value, r.Err)
	}
}
```

## Review

Backpressure is correct when a full queue never parks a caller past its budget.
`TrySubmit` is a `select` with `default`, so it returns `ErrQueueFull` in constant
time with no goroutine parked; blocking `Submit` selects on `ctx.Done()`, so a
10 ms deadline yields `context.DeadlineExceeded` rather than an indefinite hang.
The `(<-chan Result, error)` signature is deliberate: an admission refusal is not
a task outcome. The depth counter is an atomic pair with the enqueue and dequeue,
so it stays coherent under `-race` and returns to zero once the backlog drains —
if it drifts, look for an enqueue that forgot its `depth.Add(1)` or a dequeue path
that skipped the decrement. Run `go test -race -count=1 ./...`.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded queues and shedding work under load.
- [`context` — WithTimeout](https://pkg.go.dev/context#WithTimeout) — the deadline that bounds a blocking `Submit`.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — the `default` clause that makes a send non-blocking.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-worker-pool-scheduler.md](01-worker-pool-scheduler.md) | Next: [03-graceful-shutdown-drain-vs-cancel.md](03-graceful-shutdown-drain-vs-cancel.md)
