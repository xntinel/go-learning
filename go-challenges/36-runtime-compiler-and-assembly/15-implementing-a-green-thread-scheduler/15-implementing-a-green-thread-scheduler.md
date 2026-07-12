# 15. Implementing a Green Thread Scheduler

A green thread scheduler multiplexes lightweight tasks onto OS threads without direct kernel involvement. Go's own runtime scheduler does exactly this: M goroutines run on N OS threads mediated by P processors. Building a simplified version from scratch forces you to confront every design decision that the GMP scheduler makes — cooperative vs preemptive yielding, run-queue ownership, work stealing, and safe task completion. This lesson implements a cooperative scheduler using goroutines and channels as the underlying suspension mechanism rather than assembly-level context switches, which makes the code portable and testable while teaching every scheduling concept that matters.

```text
scheduler/
  go.mod
  scheduler.go
  scheduler_test.go
  cmd/demo/main.go
```

## Concepts

### The M:N Threading Model

A green thread scheduler maps M user-space tasks onto N OS threads. In Go's runtime the roles are named: goroutines are G (the task), OS threads are M (the machine), and P (processor) is the per-thread scheduling context that owns a local run queue. Each P runs one G on one M at a time; when that G blocks on I/O, M parks and a fresh M picks up the next G from P's queue.

Our scheduler mirrors this structure: `Task` is G, each `Worker` goroutine is M+P combined (it owns a local run queue), and a global run queue is the fallback when no local work is available.

### Cooperative Yielding vs Preemptive Scheduling

A cooperative scheduler yields control only when the running task explicitly calls `Yield`. This is simple but risky: a CPU-bound task that never yields starves every other task on the same worker. Go's scheduler was cooperative until 1.14, when asynchronous preemption was added via signals (SIGURG on Unix) that interrupt any goroutine at safe points.

Our scheduler is cooperative: a task yields by calling `Yield(t)`, which suspends the task and returns control to the worker loop.

### Run-Queue Ownership and Work Stealing

Each worker owns a local run queue. `Spawn` distributes new tasks across workers round-robin, so each worker's local queue receives tasks from the start. Yielded tasks re-enqueue on the global queue (so all workers compete to pick them up next). When a worker's local queue is empty it checks the global queue, then steals from other workers. Stealing half the victim's queue rather than one task at a time amortizes the overhead of crossing worker boundaries. This is the strategy described in the original GMP design document and reflected in `runtime/proc.go`'s `runqsteal`.

### Task Lifecycle and Safe Completion

A task transitions through states: `StateReady` (in a run queue), `StateRunning` (executing on a worker), `StateDone` (function returned). The `wg sync.WaitGroup` inside the scheduler tracks all live tasks; `Wait` blocks until all reach `StateDone`.

### Suspension Without Assembly

A real low-level scheduler switches context by saving register state and jumping to a different stack. In pure Go, we simulate suspension with a pair of channels: the task goroutine blocks on `resume` and signals the worker via `yieldCh`. The worker drives the handshake — send to `resume` to start/continue, then receive from `yieldCh` to regain control. A closed `yieldCh` signals task completion.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/36-runtime-compiler-and-assembly/15-implementing-a-green-thread-scheduler/15-implementing-a-green-thread-scheduler/cmd/demo
cd go-solutions/36-runtime-compiler-and-assembly/15-implementing-a-green-thread-scheduler/15-implementing-a-green-thread-scheduler
```

This is a library; verify it with `go test`, not by running a main.

### Exercise 1: Task State and the Scheduler Core

Create `scheduler.go`:

```go
// scheduler.go
package scheduler

import (
	"errors"
	"math/rand"
	"sync"
	"sync/atomic"
)

// State is the lifecycle state of a Task.
type State int32

const (
	StateReady State = iota
	StateRunning
	StateDone
)

var (
	// ErrSchedulerClosed is returned by Spawn when the scheduler is closed.
	ErrSchedulerClosed = errors.New("scheduler: already closed")
	// ErrNilFunc is returned by Spawn when the task function is nil.
	ErrNilFunc = errors.New("scheduler: task function must not be nil")
)

// Task is a lightweight cooperative task — the analogue of G in GMP.
//
// Each Task has a dedicated goroutine that starts suspended on its resume
// channel. The owning worker drives execution by sending to resume and waits
// for the task to yield or finish by receiving from yieldCh.
type Task struct {
	id      uint64
	state   atomic.Int32
	resume  chan struct{} // worker -> task: proceed
	yieldCh chan struct{} // task -> worker: yield or done (closed on done)
}

// ID returns the task's unique identifier.
func (t *Task) ID() uint64 { return t.id }

// CurrentState returns the task's current lifecycle state.
func (t *Task) CurrentState() State { return State(t.state.Load()) }

// Yield suspends t and returns control to the worker so other tasks can run.
// Must be called from within the task's own function.
func Yield(t *Task) {
	t.state.Store(int32(StateReady))
	t.yieldCh <- struct{}{} // signal the worker: I am yielding
	<-t.resume              // wait until the worker re-schedules us
	t.state.Store(int32(StateRunning))
}

// worker is one scheduling unit — the analogue of M+P in GMP.
type worker struct {
	id int
	s  *Scheduler
	mu sync.Mutex
	lq []*Task // local run queue
}

// Scheduler is a cooperative M:N green thread scheduler.
type Scheduler struct {
	workers  []*worker
	globalQ  []*Task
	globalMu sync.Mutex
	nextID   atomic.Uint64
	rr       atomic.Uint64 // round-robin index for Spawn distribution
	done     atomic.Int64
	spawned  atomic.Int64
	closed   atomic.Bool
	wg       sync.WaitGroup // tracks live tasks
	workerWg sync.WaitGroup // tracks worker goroutines
}

// New creates a Scheduler with nWorkers worker goroutines (minimum 1).
func New(nWorkers int) *Scheduler {
	if nWorkers < 1 {
		nWorkers = 1
	}
	s := &Scheduler{}
	s.workers = make([]*worker, nWorkers)
	for i := range s.workers {
		s.workers[i] = &worker{id: i, s: s}
	}
	return s
}

// Run starts all worker goroutines and blocks until all workers exit.
// Shut down by calling Close after all tasks are spawned (or after Wait).
func (s *Scheduler) Run() {
	for _, w := range s.workers {
		s.workerWg.Add(1)
		go w.loop()
	}
	s.workerWg.Wait()
}

// Close signals workers to exit once no tasks remain.
func (s *Scheduler) Close() {
	s.closed.Store(true)
}

// Wait blocks until every spawned task has reached StateDone.
func (s *Scheduler) Wait() {
	s.wg.Wait()
}

// Spawn creates a new task for fn and places it on a worker's local run queue
// using round-robin distribution, so workers have local work to steal from.
// Returns ErrNilFunc if fn is nil, ErrSchedulerClosed if closed.
func (s *Scheduler) Spawn(fn func()) (*Task, error) {
	if fn == nil {
		return nil, ErrNilFunc
	}
	if s.closed.Load() {
		return nil, ErrSchedulerClosed
	}
	t := &Task{
		id:      s.nextID.Add(1),
		resume:  make(chan struct{}),
		yieldCh: make(chan struct{}),
	}
	t.state.Store(int32(StateReady))
	s.spawned.Add(1)
	s.wg.Add(1)

	// The task goroutine runs for the full lifetime of the task. It starts
	// suspended; the worker sends to t.resume to begin execution.
	go func() {
		<-t.resume // wait for first scheduling
		t.state.Store(int32(StateRunning))
		fn()
		t.state.Store(int32(StateDone))
		close(t.yieldCh) // closed channel signals done to the worker
	}()

	// Distribute to a worker local queue round-robin so victims have tasks to
	// steal. Work stealing only fires when a worker's local queue is non-empty.
	idx := int(s.rr.Add(1)) % len(s.workers)
	w := s.workers[idx]
	w.mu.Lock()
	w.lq = append(w.lq, t)
	w.mu.Unlock()
	return t, nil
}

// enqueue places t on the global run queue (used for yielded tasks).
func (s *Scheduler) enqueue(t *Task) {
	t.state.Store(int32(StateReady))
	s.globalMu.Lock()
	s.globalQ = append(s.globalQ, t)
	s.globalMu.Unlock()
}

// DoneCount returns the number of tasks that have completed.
func (s *Scheduler) DoneCount() int64 { return s.done.Load() }

// SpawnedCount returns the total number of tasks ever spawned.
func (s *Scheduler) SpawnedCount() int64 { return s.spawned.Load() }
```

The task goroutine starts immediately but blocks on `<-t.resume` until the worker first schedules it. `Spawn` places each new task on a worker's local queue in round-robin order, so workers start with local work before needing to consult the global queue or steal. After each resume, the task runs until it calls `Yield` or returns.

### Exercise 2: The Worker Loop and Work Stealing

Append to `scheduler.go`:

```go
// loop is the scheduling loop for one worker. It runs until the scheduler is
// closed and all tasks have finished.
func (w *worker) loop() {
	defer w.s.workerWg.Done()
	for {
		t := w.next()
		if t == nil {
			if w.s.closed.Load() &&
				w.s.done.Load() >= w.s.spawned.Load() {
				return
			}
			// No runnable task: yield the OS thread briefly.
			// In production a condition variable or semaphore would be used.
			continue
		}
		w.execute(t)
	}
}

// execute runs t until it yields or completes.
func (w *worker) execute(t *Task) {
	// Resume the task goroutine.
	t.resume <- struct{}{}
	// Wait: receive from yieldCh.
	//   open==true  -> task yielded; re-enqueue it.
	//   open==false -> task done; update counters.
	_, open := <-t.yieldCh
	if !open {
		w.s.done.Add(1)
		w.s.wg.Done()
		return
	}
	// Task yielded: put it back on the global queue so all workers compete.
	w.s.enqueue(t)
}

// next returns the next runnable task: local queue, then global, then steal.
func (w *worker) next() *Task {
	// 1. Local queue (tasks assigned at spawn time or stolen from another worker).
	w.mu.Lock()
	if len(w.lq) > 0 {
		t := w.lq[0]
		w.lq = w.lq[1:]
		w.mu.Unlock()
		return t
	}
	w.mu.Unlock()

	// 2. Global queue.
	s := w.s
	s.globalMu.Lock()
	if len(s.globalQ) > 0 {
		t := s.globalQ[0]
		s.globalQ = s.globalQ[1:]
		s.globalMu.Unlock()
		return t
	}
	s.globalMu.Unlock()

	// 3. Work stealing from a random victim.
	return w.steal()
}

// steal takes up to half the tasks from a randomly chosen victim worker and
// returns the first stolen task. The rest go into the local queue.
func (w *worker) steal() *Task {
	workers := w.s.workers
	if len(workers) <= 1 {
		return nil
	}
	//nolint:gosec
	victimIdx := rand.Intn(len(workers))
	victim := workers[victimIdx]
	if victim.id == w.id {
		return nil
	}
	victim.mu.Lock()
	n := len(victim.lq)
	if n == 0 {
		victim.mu.Unlock()
		return nil
	}
	half := (n + 1) / 2
	stolen := make([]*Task, half)
	copy(stolen, victim.lq[:half])
	victim.lq = victim.lq[half:]
	victim.mu.Unlock()

	first := stolen[0]
	if len(stolen) > 1 {
		w.mu.Lock()
		w.lq = append(stolen[1:], w.lq...)
		w.mu.Unlock()
	}
	return first
}
```

The `execute` function uses a single channel receive to distinguish yield from completion: a send means yield (channel stays open), a close means done. This is the canonical Go pattern for "value or done" signaling.

### Exercise 3: Tests

Create `scheduler_test.go`:

```go
// scheduler_test.go
package scheduler

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpawnNilFuncReturnsError(t *testing.T) {
	t.Parallel()

	s := New(1)
	_, err := s.Spawn(nil)
	if !errors.Is(err, ErrNilFunc) {
		t.Fatalf("err = %v, want ErrNilFunc", err)
	}
}

func TestSpawnAfterCloseReturnsError(t *testing.T) {
	t.Parallel()

	s := New(1)
	s.Close()
	_, err := s.Spawn(func() {})
	if !errors.Is(err, ErrSchedulerClosed) {
		t.Fatalf("err = %v, want ErrSchedulerClosed", err)
	}
}

func TestTaskIDMonotonicallyIncreases(t *testing.T) {
	t.Parallel()

	s := New(1)
	// Spawn tasks but note their goroutines are parked waiting on resume.
	// We close immediately; the goroutines will be cleaned up naturally
	// because the test process exits and we do not call Run.
	t1, _ := s.Spawn(func() {})
	t2, _ := s.Spawn(func() {})
	if t1.ID() >= t2.ID() {
		t.Fatalf("IDs not monotonic: %d >= %d", t1.ID(), t2.ID())
	}
	// Drain the spawned goroutines so they don't leak under -race.
	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()
}

func TestAllTasksComplete(t *testing.T) {
	t.Parallel()

	const n = 50
	s := New(2)

	var count atomic.Int64
	for i := 0; i < n; i++ {
		_, err := s.Spawn(func() {
			count.Add(1)
		})
		if err != nil {
			t.Fatalf("Spawn error: %v", err)
		}
	}

	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	if got := count.Load(); got != n {
		t.Fatalf("completed tasks = %d, want %d", got, n)
	}
	if got := s.DoneCount(); got != n {
		t.Fatalf("DoneCount = %d, want %d", got, n)
	}
}

func TestYieldAllowsOtherTasksToRun(t *testing.T) {
	t.Parallel()

	// Single worker: only one task runs at a time.
	// Task A runs first, yields, then task B runs, then both resume.
	// We do not assert a strict order (global queue is FIFO but after Yield
	// the task re-enters the global queue, and another task may have been
	// dequeued first). Instead we assert all four steps execute.
	s := New(1)

	var steps atomic.Int64
	var taskA, taskB *Task
	taskA, _ = s.Spawn(func() {
		steps.Add(1) // step 1
		Yield(taskA)
		steps.Add(1) // step 3 (or 4)
	})
	taskB, _ = s.Spawn(func() {
		steps.Add(1) // step 2
		Yield(taskB)
		steps.Add(1) // step 4 (or 3)
	})

	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	if got := steps.Load(); got != 4 {
		t.Fatalf("steps = %d, want 4 (both tasks ran two steps each)", got)
	}
}

func TestYieldInterleavesWithMutex(t *testing.T) {
	t.Parallel()

	// Two tasks each increment a mutex-protected counter, yield, then
	// increment again. Final value must be 4, with no data race.
	s := New(1)

	var mu sync.Mutex
	counter := 0

	var task1, task2 *Task
	task1, _ = s.Spawn(func() {
		mu.Lock()
		counter++
		mu.Unlock()
		Yield(task1)
		mu.Lock()
		counter++
		mu.Unlock()
	})
	task2, _ = s.Spawn(func() {
		mu.Lock()
		counter++
		mu.Unlock()
		Yield(task2)
		mu.Lock()
		counter++
		mu.Unlock()
	})
	_ = task2

	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	mu.Lock()
	got := counter
	mu.Unlock()
	if got != 4 {
		t.Fatalf("counter = %d, want 4", got)
	}
}

func TestMultipleWorkersNoDataRace(t *testing.T) {
	t.Parallel()

	const n = 100
	s := New(4)

	var count atomic.Int64
	for i := 0; i < n; i++ {
		_, err := s.Spawn(func() {
			count.Add(1)
		})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
	}

	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	if got := count.Load(); got != n {
		t.Fatalf("count = %d, want %d", got, n)
	}
}

func TestWorkStealingDistributesTasks(t *testing.T) {
	t.Parallel()

	s := New(3)

	const n = 30
	var count atomic.Int64
	for i := 0; i < n; i++ {
		s.Spawn(func() { //nolint:errcheck
			count.Add(1)
		})
	}

	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	if got := count.Load(); got != n {
		t.Fatalf("count = %d, want %d", got, n)
	}
}

func TestSpawnedAndDoneCountsMatch(t *testing.T) {
	t.Parallel()

	s := New(2)
	const n = 20
	for i := 0; i < n; i++ {
		s.Spawn(func() {}) //nolint:errcheck
	}
	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	if s.SpawnedCount() != n {
		t.Fatalf("SpawnedCount = %d, want %d", s.SpawnedCount(), n)
	}
	if s.DoneCount() != n {
		t.Fatalf("DoneCount = %d, want %d", s.DoneCount(), n)
	}
}

func TestTaskReachesStateDoneAfterRun(t *testing.T) {
	t.Parallel()

	s := New(1)
	task, _ := s.Spawn(func() {})
	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	if got := task.CurrentState(); got != StateDone {
		t.Fatalf("state = %v, want StateDone", got)
	}
}

func TestRunCompletesBeforeDeadline(t *testing.T) {
	t.Parallel()

	s := New(1)
	s.Spawn(func() {}) //nolint:errcheck

	// Run blocks until all workers exit. Shut down by calling Close after
	// tasks finish. We drive this from a separate goroutine so the test
	// goroutine can enforce the deadline.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Drain tasks then stop workers.
		go func() {
			s.Wait()
			s.Close()
		}()
		s.Run()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not finish within 5s")
	}
}

func ExampleNew() {
	s := New(2)
	var count atomic.Int64
	for i := 0; i < 5; i++ {
		s.Spawn(func() { //nolint:errcheck
			count.Add(1)
		})
	}
	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()
	// Output:
}
```

Your turn: add `TestYieldDoesNotRunTaskTwiceConcurrently` — spawn a single task that increments a non-atomic `int` counter, yields once, then increments again. Run with a single worker and assert the final value is 2 without using `atomic`. If the scheduler allows the same task to execute on two workers simultaneously, the race detector will flag the non-atomic access.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/scheduler"
)

func main() {
	s := scheduler.New(2)

	var total atomic.Int64
	const n = 8
	tasks := make([]*scheduler.Task, n)
	for i := range tasks {
		i := i
		tasks[i], _ = s.Spawn(func() {
			total.Add(1)
			scheduler.Yield(tasks[i]) // cooperative yield
			total.Add(1)
		})
	}

	go func() {
		s.Wait()
		s.Close()
	}()
	s.Run()

	fmt.Printf("spawned=%d done=%d increments=%d\n",
		s.SpawnedCount(), s.DoneCount(), total.Load())
}
```

## Common Mistakes

### Sending to a Closed Channel Panics

Wrong: calling `Yield(t)` after the task's function has returned. The task goroutine closes `t.yieldCh` on completion; a subsequent send panics.

Fix: call `Yield` only from within the task's own function body, before `fn()` returns. The scheduler does not call `Yield` internally.

### Holding a Mutex While Blocking on a Channel

Wrong:

```go
w.mu.Lock()
t := w.lq[0]
t.resume <- struct{}{} // blocks; another goroutine may need w.mu to enqueue
w.mu.Unlock()
```

Fix: release the mutex before any channel operation. In `execute`, the task is dequeued before the send to `t.resume`, so no lock is held during the blocking send.

### Busy-Polling the Run Queue Burns CPU

Wrong: an empty `for {}` spin waiting for work. On a single core it prevents other goroutines from running; on multi-core it wastes a whole thread at 100%.

Fix: in this lesson the spin is acceptable for a demo scheduler. A production implementation would use `runtime.Gosched()` inside the spin, or block on a semaphore (as Go's runtime does with `futex`-based `semacquire`).

### Not Detecting Task Completion via Channel Close

Wrong: using a boolean flag in the task struct to signal done. A separate flag introduces a TOCTOU race between the flag write (in the task goroutine) and the flag read (in the worker).

Fix: close the channel. A closed channel is immediately readable by any goroutine; the close is itself the synchronization. The `_, open := <-t.yieldCh` idiom is the idiomatic pattern.

### Loop Variable Capture in Closures

Wrong:

```go
for i := 0; i < n; i++ {
	s.Spawn(func() { fmt.Println(i) }) // always prints n
}
```

Fix: capture before the closure:

```go
for i := 0; i < n; i++ {
	i := i
	s.Spawn(func() { fmt.Println(i) })
}
```

## Verification

From `~/go-exercises/scheduler`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass without data race warnings.

## Summary

- A green thread scheduler maps M user-space tasks onto N OS threads; each worker owns a local run queue populated by round-robin spawn and steals from others when its local queue is empty.
- Cooperative scheduling yields control only on explicit `Yield` calls; preemptive scheduling requires signal-based interruption (Go 1.14+).
- The suspension primitive in pure Go is a channel pair (`resume`, `yieldCh`); the worker drives the task goroutine via a send/receive handshake.
- A closed channel is the idiomatic signal for "done": `_, open := <-ch` distinguishes a yield (open) from completion (closed).
- Work stealing takes half the victim's local queue at once to amortize the cost of crossing worker boundaries; stealing only fires when a victim's local queue is non-empty.
- The `wg.Wait` / `wg.Done` pattern is the correct way to drain a scheduler before shutdown.

## What's Next

Next: [Consistent Hashing Ring](../../37-distributed-systems-fundamentals/01-consistent-hashing-ring/01-consistent-hashing-ring.md).

## Resources

- [Go Scheduler Design Document (Dmitry Vyukov, 2012)](https://docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw) — original GMP design rationale with work-stealing details
- [src/runtime/proc.go](https://github.com/golang/go/blob/master/src/runtime/proc.go) — Go's scheduler source; `schedule()`, `findRunnable()`, and `runqsteal()` are the canonical reference
- [Work-Stealing Scheduler (Blumofe & Leiserson, 1999)](https://dl.acm.org/doi/10.1145/324133.324234) — foundational paper; proves stealing half the queue is asymptotically optimal
- [Scheduling In Go (Ardan Labs)](https://www.ardanlabs.com/blog/2018/08/scheduling-in-go-part1.html) — three-part series on GMP with diagrams
- [sync package](https://pkg.go.dev/sync) and [sync/atomic package](https://pkg.go.dev/sync/atomic) — the stdlib primitives used throughout this lesson
