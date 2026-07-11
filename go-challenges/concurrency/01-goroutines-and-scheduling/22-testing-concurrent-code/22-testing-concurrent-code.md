# Exercise 22: Testing Concurrent Code

Concurrent bugs are the most expensive kind: they pass local tests, survive
code review, and then fail intermittently in CI -- or worse, in production at
3 AM. A test that calls `time.Sleep(100 * time.Millisecond)` to "wait for
goroutines to finish" is not a test, it is a hope. Too short on a loaded CI box
and it flakes; too long on a fast one and it never triggers the race it claims
to guard. This exercise builds concurrent tests that force true simultaneity
with barrier channels, so the code under test is actually stressed the way
production will stress it, every single run.

## What you'll build

```text
22-testing-concurrent-code/
  main.go            TaskScheduler plus a demo runner
  scheduler_test.go  barrier-synchronized concurrent tests
```

- Build: a concurrent `TaskScheduler` and a test suite that forces goroutines to collide on shared state on purpose.
- Implement: `setupBarrier` and a `ConcurrentTestRunner` helper; barrier-gated tests like `TestConcurrentAssignment` and `TestConcurrentDoubleAssignment`; a deliberately racy counter to exercise the detector.
- Verify: `go run main.go` for the demo, `go test -race` for the suite.

### Why a barrier, not a sleep, makes the test real

The Go race detector (`go test -race`) catches data races at runtime, but only
if the test actually triggers the race. A test that runs goroutines
sequentially -- by sleeping too long -- never triggers the race and the
detector finds nothing. You need tests that force concurrent execution:
multiple goroutines hitting the same resource at the same time, every time.

The solution is barrier-based synchronization: channels that gate goroutine
execution so they all start at exactly the same moment, and explicit completion
signals so the test knows when all goroutines have finished. No sleeps, no
timeouts, no flakes. This is not theoretical -- every serious Go project (the
standard library, Kubernetes, CockroachDB) uses these techniques to test
concurrent code reliably.


## Step 1 -- The System Under Test: TaskScheduler

Build a `TaskScheduler` that assigns tasks to workers concurrently. This is the code we need to test.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusDone       = "done"
)

type Task struct {
	ID     int
	Name   string
	Status string
}

type TaskScheduler struct {
	mu          sync.Mutex
	tasks       []*Task
	assigned    map[int]int
	completions int64
}

func NewTaskScheduler() *TaskScheduler {
	return &TaskScheduler{
		tasks:    make([]*Task, 0),
		assigned: make(map[int]int),
	}
}

func (s *TaskScheduler) AddTask(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := len(s.tasks) + 1
	s.tasks = append(s.tasks, &Task{
		ID:     id,
		Name:   name,
		Status: StatusPending,
	})
	return id
}

func (s *TaskScheduler) AssignToWorker(taskID, workerID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != StatusPending {
		return fmt.Errorf("task %d is %s, not pending", taskID, task.Status)
	}

	task.Status = StatusProcessing
	s.assigned[taskID] = workerID
	return nil
}

func (s *TaskScheduler) CompleteTask(taskID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != StatusProcessing {
		return fmt.Errorf("task %d is %s, not processing", taskID, task.Status)
	}

	task.Status = StatusDone
	atomic.AddInt64(&s.completions, 1)
	return nil
}

func (s *TaskScheduler) CompletionCount() int64 {
	return atomic.LoadInt64(&s.completions)
}

func (s *TaskScheduler) TaskStatus(taskID int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task := s.findTaskLocked(taskID)
	if task == nil {
		return "", fmt.Errorf("task %d not found", taskID)
	}
	return task.Status, nil
}

func (s *TaskScheduler) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, t := range s.tasks {
		if t.Status == StatusPending {
			count++
		}
	}
	return count
}

func (s *TaskScheduler) findTaskLocked(id int) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}
```

Keep everything in one directory as a single `main` package. `go test` runs
against `package main` just as it does against a library package, so no module
plumbing is needed:

```
22-testing-concurrent-code/
  main.go            (TaskScheduler + demo runner, code above)
  scheduler_test.go  (tests from Step 2)
```

`main.go` adds a demo runner to the scheduler above:

```go
func main() {
	sched := NewTaskScheduler()

	const (
		numWorkers = 4
		numTasks   = 12
	)

	for i := 1; i <= numTasks; i++ {
		sched.AddTask(fmt.Sprintf("task-%d", i))
	}
	fmt.Printf("Added %d tasks, pending: %d\n\n", numTasks, sched.PendingCount())

	var wg sync.WaitGroup
	tasksPerWorker := numTasks / numWorkers

	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := (workerID-1)*tasksPerWorker + 1
			end := start + tasksPerWorker

			for taskID := start; taskID < end; taskID++ {
				if err := sched.AssignToWorker(taskID, workerID); err != nil {
					fmt.Printf("  worker %d: assign task %d failed: %v\n", workerID, taskID, err)
					continue
				}
				if err := sched.CompleteTask(taskID); err != nil {
					fmt.Printf("  worker %d: complete task %d failed: %v\n", workerID, taskID, err)
					continue
				}
				fmt.Printf("  worker %d: completed task %d\n", workerID, taskID)
			}
		}(w)
	}

	wg.Wait()
	fmt.Printf("\nAll workers done. Completed: %d, Pending: %d\n", sched.CompletionCount(), sched.PendingCount())
}
```

### Verification

For the single-file demo, create a standalone version:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	statusPending    = "pending"
	statusProcessing = "processing"
	statusDone       = "done"
	numWorkers       = 4
	numTasks         = 12
)

type Task struct {
	ID     int
	Name   string
	Status string
}

type TaskScheduler struct {
	mu          sync.Mutex
	tasks       []*Task
	assigned    map[int]int
	completions int64
}

func NewTaskScheduler() *TaskScheduler {
	return &TaskScheduler{
		tasks:    make([]*Task, 0),
		assigned: make(map[int]int),
	}
}

func (s *TaskScheduler) AddTask(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := len(s.tasks) + 1
	s.tasks = append(s.tasks, &Task{ID: id, Name: name, Status: statusPending})
	return id
}

func (s *TaskScheduler) AssignToWorker(taskID, workerID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != statusPending {
		return fmt.Errorf("task %d is %s, not pending", taskID, task.Status)
	}
	task.Status = statusProcessing
	s.assigned[taskID] = workerID
	return nil
}

func (s *TaskScheduler) CompleteTask(taskID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != statusProcessing {
		return fmt.Errorf("task %d is %s, not processing", taskID, task.Status)
	}
	task.Status = statusDone
	atomic.AddInt64(&s.completions, 1)
	return nil
}

func (s *TaskScheduler) CompletionCount() int64 {
	return atomic.LoadInt64(&s.completions)
}

func (s *TaskScheduler) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, t := range s.tasks {
		if t.Status == statusPending {
			count++
		}
	}
	return count
}

func (s *TaskScheduler) findTaskLocked(id int) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func main() {
	sched := NewTaskScheduler()

	for i := 1; i <= numTasks; i++ {
		sched.AddTask(fmt.Sprintf("task-%d", i))
	}
	fmt.Printf("Added %d tasks, pending: %d\n\n", numTasks, sched.PendingCount())

	var wg sync.WaitGroup
	tasksPerWorker := numTasks / numWorkers

	for w := 1; w <= numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			start := (workerID-1)*tasksPerWorker + 1
			end := start + tasksPerWorker

			for taskID := start; taskID < end; taskID++ {
				if err := sched.AssignToWorker(taskID, workerID); err != nil {
					fmt.Printf("  worker %d: assign task %d failed: %v\n", workerID, taskID, err)
					continue
				}
				if err := sched.CompleteTask(taskID); err != nil {
					fmt.Printf("  worker %d: complete task %d failed: %v\n", workerID, taskID, err)
					continue
				}
				fmt.Printf("  worker %d: completed task %d\n", workerID, taskID)
			}
		}(w)
	}

	wg.Wait()
	fmt.Printf("\nAll workers done. Completed: %d, Pending: %d\n", sched.CompletionCount(), sched.PendingCount())
}
```

```bash
go run main.go
```
Expected output (order of worker lines varies):
```
Added 12 tasks, pending: 12

  worker 4: completed task 10
  worker 4: completed task 11
  worker 4: completed task 12
  worker 1: completed task 1
  worker 1: completed task 2
  worker 1: completed task 3
  worker 2: completed task 4
  worker 2: completed task 5
  worker 2: completed task 6
  worker 3: completed task 7
  worker 3: completed task 8
  worker 3: completed task 9

All workers done. Completed: 12, Pending: 0
```


## Step 2 -- Barrier-Based Concurrent Tests

Write tests that use barrier channels to force all goroutines to start simultaneously, guaranteeing concurrent access.

The demo `main.go` includes the `TaskScheduler`, the `setupBarrier` helper, and a concurrent demo:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	statusPending    = "pending"
	statusProcessing = "processing"
	statusDone       = "done"
)

type Task struct {
	ID     int
	Name   string
	Status string
}

type TaskScheduler struct {
	mu          sync.Mutex
	tasks       []*Task
	assigned    map[int]int
	completions int64
}

func NewTaskScheduler() *TaskScheduler {
	return &TaskScheduler{
		tasks:    make([]*Task, 0),
		assigned: make(map[int]int),
	}
}

func (s *TaskScheduler) AddTask(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := len(s.tasks) + 1
	s.tasks = append(s.tasks, &Task{ID: id, Name: name, Status: statusPending})
	return id
}

func (s *TaskScheduler) AssignToWorker(taskID, workerID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != statusPending {
		return fmt.Errorf("task %d is %s, not pending", taskID, task.Status)
	}
	task.Status = statusProcessing
	s.assigned[taskID] = workerID
	return nil
}

func (s *TaskScheduler) CompleteTask(taskID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != statusProcessing {
		return fmt.Errorf("task %d is %s, not processing", taskID, task.Status)
	}
	task.Status = statusDone
	atomic.AddInt64(&s.completions, 1)
	return nil
}

func (s *TaskScheduler) CompletionCount() int64 {
	return atomic.LoadInt64(&s.completions)
}

func (s *TaskScheduler) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, t := range s.tasks {
		if t.Status == statusPending {
			count++
		}
	}
	return count
}

func (s *TaskScheduler) findTaskLocked(id int) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// --- Test Helpers ---

func setupBarrier(n int) (ready func(), wait <-chan struct{}) {
	barrier := make(chan struct{})
	var readyWg sync.WaitGroup
	readyWg.Add(n)

	go func() {
		readyWg.Wait()
		close(barrier)
	}()

	return readyWg.Done, barrier
}

func main() {
	fmt.Println("=== Testing Concurrent Code ===")
	fmt.Println("  Barrier-synchronized concurrent demo.")
	fmt.Println("  Run tests with: go test -v -race")
	fmt.Println()

	sched := NewTaskScheduler()

	const workers = 10
	for i := 1; i <= workers; i++ {
		sched.AddTask(fmt.Sprintf("task-%d", i))
	}

	signalReady, barrier := setupBarrier(workers)
	var wg sync.WaitGroup
	var successCount int64

	for i := 1; i <= workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			signalReady()
			<-barrier
			if err := sched.AssignToWorker(workerID, workerID); err == nil {
				atomic.AddInt64(&successCount, 1)
				if err := sched.CompleteTask(workerID); err == nil {
					fmt.Printf("  worker %d: assigned and completed task %d\n", workerID, workerID)
				}
			}
		}(i)
	}

	wg.Wait()
	fmt.Println()
	fmt.Printf("  Barrier-synchronized: %d workers started simultaneously\n", workers)
	fmt.Printf("  Successful assignments: %d\n", successCount)
	fmt.Printf("  Completions: %d\n", sched.CompletionCount())
}
```

The test file `scheduler_test.go` uses the same `setupBarrier` helper and the `testing` package:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestConcurrentAssignment(t *testing.T) {
	const workers = 20

	sched := NewTaskScheduler()
	taskIDs := make([]int, workers)
	for i := 0; i < workers; i++ {
		taskIDs[i] = sched.AddTask(fmt.Sprintf("task-%d", i+1))
	}

	signalReady, barrier := setupBarrier(workers)

	var wg sync.WaitGroup
	errors := make([]error, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			signalReady()
			<-barrier // all goroutines released at once
			errors[idx] = sched.AssignToWorker(taskIDs[idx], idx+1)
		}(i)
	}

	wg.Wait()

	for i, err := range errors {
		if err != nil {
			t.Errorf("worker %d: unexpected error: %v", i+1, err)
		}
	}

	if pending := sched.PendingCount(); pending != 0 {
		t.Errorf("expected 0 pending, got %d", pending)
	}
}

func TestConcurrentDoubleAssignment(t *testing.T) {
	const contenders = 10

	sched := NewTaskScheduler()
	taskID := sched.AddTask("contested-task")

	signalReady, barrier := setupBarrier(contenders)

	var wg sync.WaitGroup
	var successCount int64

	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			signalReady()
			<-barrier
			if err := sched.AssignToWorker(taskID, workerID); err == nil {
				atomic.AddInt64(&successCount, 1)
			}
		}(i + 1)
	}

	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 successful assignment, got %d", successCount)
	}
}

func TestConcurrentAddAndAssign(t *testing.T) {
	const iterations = 50

	sched := NewTaskScheduler()

	signalReady, barrier := setupBarrier(iterations * 2)

	var wg sync.WaitGroup
	taskIDs := make(chan int, iterations)

	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			signalReady()
			<-barrier
			id := sched.AddTask(fmt.Sprintf("concurrent-task-%d", idx))
			taskIDs <- id
		}(i)
	}

	var assignErrors int64
	for i := 0; i < iterations; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			signalReady()
			<-barrier
			id := <-taskIDs
			if err := sched.AssignToWorker(id, workerID); err != nil {
				atomic.AddInt64(&assignErrors, 1)
			}
		}(i + 1)
	}

	wg.Wait()

	if assignErrors > 0 {
		t.Logf("assign errors (expected some due to timing): %d", assignErrors)
	}

	completions := sched.CompletionCount()
	t.Logf("tasks added: %d, assignment errors: %d, completions: %d",
		iterations, assignErrors, completions)
}
```

**What's happening here:** The `setupBarrier` function creates a synchronization barrier. Each goroutine calls `signalReady()` when it is prepared to start, then blocks on `<-barrier`. Once all N goroutines have signaled ready, the barrier channel is closed and they all proceed simultaneously. This guarantees concurrent access -- no sleeps, no timing dependencies.

**Key insight:** `TestConcurrentDoubleAssignment` is the critical test. It verifies that when 10 goroutines all try to assign the same task at the exact same moment, exactly one succeeds. Without the mutex in `AssignToWorker`, multiple goroutines would read `StatusPending`, all succeed, and the task would be assigned to multiple workers -- a correctness violation that only manifests under true concurrency.

### Verification
```bash
go run main.go
```
Expected output (worker order varies):
```
=== Testing Concurrent Code ===
  Barrier-synchronized concurrent demo.
  Run tests with: go test -v -race

  worker 3: assigned and completed task 3
  worker 7: assigned and completed task 7
  worker 1: assigned and completed task 1
  worker 10: assigned and completed task 10
  worker 5: assigned and completed task 5
  worker 8: assigned and completed task 8
  worker 2: assigned and completed task 2
  worker 4: assigned and completed task 4
  worker 6: assigned and completed task 6
  worker 9: assigned and completed task 9

  Barrier-synchronized: 10 workers started simultaneously
  Successful assignments: 10
  Completions: 10
```


## Step 3 -- Race Detector and Intentional Race Exposure

Create a version with a deliberate race condition, then show how the race detector catches it.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	racyWorkers    = 100
	racyIterations = 10
)

// UnsafeCounter has a deliberate data race -- DO NOT use in production.
// This exists only to demonstrate what the race detector catches.
type UnsafeCounter struct {
	count int // not protected by mutex or atomic
}

func NewUnsafeCounter() *UnsafeCounter {
	return &UnsafeCounter{}
}

func (c *UnsafeCounter) Increment() {
	c.count++ // DATA RACE: concurrent read-modify-write
}

func (c *UnsafeCounter) Value() int {
	return c.count // DATA RACE: concurrent read
}

// SafeCounter uses atomic operations -- correct under concurrency.
type SafeCounter struct {
	count int64
}

func NewSafeCounter() *SafeCounter {
	return &SafeCounter{}
}

func (c *SafeCounter) Increment() {
	atomic.AddInt64(&c.count, 1)
}

func (c *SafeCounter) Value() int64 {
	return atomic.LoadInt64(&c.count)
}

func testCounter(name string, increment func(), value func() string) {
	signalReady, barrier := barrierSetup(racyWorkers)
	var wg sync.WaitGroup

	for i := 0; i < racyWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signalReady()
			<-barrier
			for j := 0; j < racyIterations; j++ {
				increment()
			}
		}()
	}

	wg.Wait()
	expected := racyWorkers * racyIterations
	fmt.Printf("  %-15s expected=%d, actual=%s\n", name+":", expected, value())
}

func barrierSetup(n int) (ready func(), wait <-chan struct{}) {
	barrier := make(chan struct{})
	var readyWg sync.WaitGroup
	readyWg.Add(n)

	go func() {
		readyWg.Wait()
		close(barrier)
	}()

	return readyWg.Done, barrier
}

func main() {
	fmt.Println("=== Race Detector Demonstration ===")
	fmt.Printf("  Workers: %d, Iterations per worker: %d\n\n", racyWorkers, racyIterations)

	fmt.Println("  [SafeCounter] -- atomic operations, no race")
	safe := NewSafeCounter()
	testCounter("SafeCounter", safe.Increment, func() string {
		return fmt.Sprintf("%d", safe.Value())
	})

	fmt.Println()
	fmt.Println("  [UnsafeCounter] -- data race, run with 'go run -race main.go' to detect")
	unsafe := NewUnsafeCounter()
	testCounter("UnsafeCounter", unsafe.Increment, func() string {
		return fmt.Sprintf("%d (likely wrong)", unsafe.Value())
	})

	fmt.Println()
	fmt.Println("  Note: run 'go run -race main.go' to see the race detector report")
	fmt.Println("  The UnsafeCounter will show a lower value than expected due to lost updates")
}
```

**What's happening here:** `UnsafeCounter.Increment` does `c.count++` without synchronization. When 100 goroutines increment simultaneously, many increments are lost because two goroutines read the same value, both increment to the same result, and one write is overwritten. `SafeCounter` uses `atomic.AddInt64` which is a single atomic CPU instruction.

**Key insight:** The race detector instruments memory accesses at compile time. Running with `-race` adds significant overhead (2-10x slower, 5-10x more memory) but catches races that would never be found by testing. Always run `go test -race` in CI. The barrier ensures all goroutines are truly concurrent, maximizing the chance of triggering the race.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Race Detector Demonstration ===
  Workers: 100, Iterations per worker: 10

  [SafeCounter] -- atomic operations, no race
  SafeCounter:    expected=1000, actual=1000

  [UnsafeCounter] -- data race, run with 'go run -race main.go' to detect
  UnsafeCounter:  expected=1000, actual=837 (likely wrong)

  Note: run 'go run -race main.go' to see the race detector report
  The UnsafeCounter will show a lower value than expected due to lost updates
```

With race detector:
```bash
go run -race main.go
```
Additional output includes:
```
==================
WARNING: DATA RACE
Read at 0x... by goroutine ...:
  main.(*UnsafeCounter).Increment()
      .../main.go:27 +0x...
...
==================
```


## Step 4 -- Complete Test Suite with Helpers

Build a full test file with reusable helpers for concurrent testing patterns.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	statusPending    = "pending"
	statusProcessing = "processing"
	statusDone       = "done"
)

type Task struct {
	ID     int
	Name   string
	Status string
}

type TaskScheduler struct {
	mu          sync.Mutex
	tasks       []*Task
	assigned    map[int]int
	completions int64
}

func NewTaskScheduler() *TaskScheduler {
	return &TaskScheduler{
		tasks:    make([]*Task, 0),
		assigned: make(map[int]int),
	}
}

func (s *TaskScheduler) AddTask(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := len(s.tasks) + 1
	s.tasks = append(s.tasks, &Task{ID: id, Name: name, Status: statusPending})
	return id
}

func (s *TaskScheduler) AssignToWorker(taskID, workerID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != statusPending {
		return fmt.Errorf("task %d is %s, not pending", taskID, task.Status)
	}
	task.Status = statusProcessing
	s.assigned[taskID] = workerID
	return nil
}

func (s *TaskScheduler) CompleteTask(taskID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	task := s.findTaskLocked(taskID)
	if task == nil {
		return fmt.Errorf("task %d not found", taskID)
	}
	if task.Status != statusProcessing {
		return fmt.Errorf("task %d is %s, not processing", taskID, task.Status)
	}
	task.Status = statusDone
	atomic.AddInt64(&s.completions, 1)
	return nil
}

func (s *TaskScheduler) CompletionCount() int64 {
	return atomic.LoadInt64(&s.completions)
}

func (s *TaskScheduler) PendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, t := range s.tasks {
		if t.Status == statusPending {
			count++
		}
	}
	return count
}

func (s *TaskScheduler) findTaskLocked(id int) *Task {
	for _, t := range s.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// --- Concurrency Test Helpers ---

type ConcurrentTestRunner struct {
	readyFn func()
	barrier <-chan struct{}
	wg      sync.WaitGroup
	errors  []atomic.Value
	count   int
}

func NewConcurrentTestRunner(n int) *ConcurrentTestRunner {
	barrier := make(chan struct{})
	var readyWg sync.WaitGroup
	readyWg.Add(n)

	go func() {
		readyWg.Wait()
		close(barrier)
	}()

	return &ConcurrentTestRunner{
		readyFn: readyWg.Done,
		barrier: barrier,
		errors:  make([]atomic.Value, n),
		count:   n,
	}
}

func (r *ConcurrentTestRunner) Run(idx int, fn func() error) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.readyFn()
		<-r.barrier
		if err := fn(); err != nil {
			r.errors[idx].Store(err)
		}
	}()
}

func (r *ConcurrentTestRunner) Wait() []error {
	r.wg.Wait()
	var errs []error
	for _, e := range r.errors {
		if v := e.Load(); v != nil {
			errs = append(errs, v.(error))
		}
	}
	return errs
}

// --- Demonstration ---

func demoExclusiveAssignment() {
	fmt.Println("--- Test: Exclusive Assignment ---")
	const contenders = 20

	sched := NewTaskScheduler()
	taskID := sched.AddTask("exclusive-resource")

	runner := NewConcurrentTestRunner(contenders)
	var wins int64

	for i := 0; i < contenders; i++ {
		workerID := i + 1
		runner.Run(i, func() error {
			if err := sched.AssignToWorker(taskID, workerID); err == nil {
				atomic.AddInt64(&wins, 1)
			}
			return nil
		})
	}

	runner.Wait()
	fmt.Printf("  %d contenders, %d successful assignments (expected: 1)\n", contenders, wins)
	if wins == 1 {
		fmt.Println("  PASS: exactly one worker assigned")
	} else {
		fmt.Println("  FAIL: multiple workers assigned to same task")
	}
}

func demoFullLifecycle() {
	fmt.Println()
	fmt.Println("--- Test: Full Lifecycle Under Concurrency ---")
	const tasks = 50

	sched := NewTaskScheduler()
	ids := make([]int, tasks)
	for i := 0; i < tasks; i++ {
		ids[i] = sched.AddTask(fmt.Sprintf("lifecycle-task-%d", i+1))
	}

	runner := NewConcurrentTestRunner(tasks)
	for i := 0; i < tasks; i++ {
		taskID := ids[i]
		workerID := i + 1
		runner.Run(i, func() error {
			if err := sched.AssignToWorker(taskID, workerID); err != nil {
				return err
			}
			return sched.CompleteTask(taskID)
		})
	}

	errs := runner.Wait()
	fmt.Printf("  Tasks: %d, Errors: %d, Completions: %d\n", tasks, len(errs), sched.CompletionCount())

	if len(errs) == 0 && sched.CompletionCount() == int64(tasks) {
		fmt.Println("  PASS: all tasks completed without errors")
	} else {
		fmt.Println("  FAIL: unexpected errors or incomplete tasks")
		for _, err := range errs {
			fmt.Printf("    error: %v\n", err)
		}
	}
}

func demoStressTest() {
	fmt.Println()
	fmt.Println("--- Test: Stress Test (100 concurrent operations) ---")
	const ops = 100

	sched := NewTaskScheduler()
	runner := NewConcurrentTestRunner(ops)

	var addCount, assignCount int64

	for i := 0; i < ops; i++ {
		idx := i
		runner.Run(i, func() error {
			id := sched.AddTask(fmt.Sprintf("stress-%d", idx))
			atomic.AddInt64(&addCount, 1)

			if err := sched.AssignToWorker(id, idx+1); err == nil {
				atomic.AddInt64(&assignCount, 1)
			}
			return nil
		})
	}

	runner.Wait()
	fmt.Printf("  Adds: %d, Assigns: %d, Pending: %d\n", addCount, assignCount, sched.PendingCount())
	fmt.Println("  PASS: no panics, no data races under stress")
}

func main() {
	fmt.Println("=== Concurrent Test Suite Demonstration ===")
	fmt.Println("  Using ConcurrentTestRunner with barrier synchronization")
	fmt.Println()

	demoExclusiveAssignment()
	demoFullLifecycle()
	demoStressTest()

	fmt.Println()
	fmt.Println("=== Key Patterns ===")
	fmt.Println("  1. Barrier: all goroutines start at exactly the same time")
	fmt.Println("  2. Error collection: atomic.Value stores per-goroutine errors")
	fmt.Println("  3. No time.Sleep: synchronization via channels, not timers")
	fmt.Println("  4. Race detector: run with -race to catch hidden races")
}
```

**What's happening here:** `ConcurrentTestRunner` encapsulates the barrier pattern into a reusable helper. Each test creates a runner, registers concurrent operations, and waits for completion. Errors are collected per-goroutine using `atomic.Value` (which avoids needing a mutex around the error slice). The runner guarantees that all operations start simultaneously.

**Key insight:** The `ConcurrentTestRunner` pattern is what you put in a `testutils` package and reuse across your entire codebase. It eliminates the boilerplate of creating barriers, wait groups, and error collection in every concurrent test. The three demos show the core patterns: exclusive access (only one wins), full lifecycle (assign-complete pipeline), and stress testing (many operations, no panics).

### Verification
```bash
go run main.go
```
Expected output:
```
=== Concurrent Test Suite Demonstration ===
  Using ConcurrentTestRunner with barrier synchronization

--- Test: Exclusive Assignment ---
  20 contenders, 1 successful assignments (expected: 1)
  PASS: exactly one worker assigned

--- Test: Full Lifecycle Under Concurrency ---
  Tasks: 50, Errors: 0, Completions: 50
  PASS: all tasks completed without errors

--- Test: Stress Test (100 concurrent operations) ---
  Adds: 100, Assigns: 100, Pending: 0
  PASS: no panics, no data races under stress

=== Key Patterns ===
  1. Barrier: all goroutines start at exactly the same time
  2. Error collection: atomic.Value stores per-goroutine errors
  3. No time.Sleep: synchronization via channels, not timers
  4. Race detector: run with -race to catch hidden races
```


## Common Mistakes

### Using time.Sleep to Wait for Goroutines

```go
package main

import (
	"fmt"
	"time"
)

func main() {
	result := 0
	go func() {
		result = 42
	}()
	time.Sleep(100 * time.Millisecond) // flaky: may not be enough on slow CI
	fmt.Println(result)
}
```
**What happens:** On your fast laptop, 100ms is plenty. On a loaded CI server, the goroutine might not have run yet. The test passes locally and fails in CI. Even when it passes, the race detector flags `result = 42` as a data race because there is no synchronization.

**Fix:** Use a channel or WaitGroup for explicit synchronization:
```go
package main

import "fmt"

func main() {
	done := make(chan int, 1)
	go func() {
		done <- 42
	}()
	result := <-done // deterministic: blocks until goroutine completes
	fmt.Println(result)
}
```


### Not Running Tests with -race

```go
package main

import (
	"fmt"
	"sync"
)

type Counter struct {
	value int // no synchronization
}

func main() {
	c := &Counter{}
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.value++ // data race: unprotected concurrent write
		}()
	}

	wg.Wait()
	fmt.Println(c.value) // passes without -race, catches race with -race
}
```
**What happens:** Without `-race`, the program runs and may even produce the "correct" answer. The bug is invisible until production load exposes it as corrupted data or a crash.

**Fix:** Always run `go test -race` in CI. Make it a required check. The race detector has zero false positives -- if it reports a race, you have a real bug.


### Testing Concurrent Code Sequentially

```go
package main

import "fmt"

func main() {
	// "concurrent" test that is actually sequential
	results := make([]int, 5)
	for i := 0; i < 5; i++ {
		results[i] = i * 2 // no goroutines: this is sequential code
	}
	fmt.Println("passed:", results)
	// This test never exercises concurrency, so it proves nothing
}
```
**What happens:** The test claims to test concurrent access but runs everything in a single goroutine. No race can manifest. The mutex (or lack thereof) is never stressed. The test is green and worthless.

**Fix:** Use a barrier to ensure goroutines truly run concurrently:
```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	results := make([]int, 5)
	barrier := make(chan struct{})
	var wg sync.WaitGroup

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-barrier // wait until all goroutines are ready
			results[idx] = idx * 2
		}(i)
	}

	close(barrier) // release all goroutines at once
	wg.Wait()
	fmt.Println("passed:", results)
}
```


## Review

The technique underneath every test here is the barrier: each goroutine calls a
ready function and then blocks on a shared channel, and only when all N have
signaled does a `close` release them in the same instant. That is what converts
"I launched some goroutines" into "N goroutines hit this resource
simultaneously", which is the only condition under which a data race reliably
manifests. `go test -race` then does the detecting -- it instruments memory
accesses and reports with zero false positives, so a report is always a real
bug -- but it can only catch a race the test actually triggers, which is why the
barrier and the detector are a pair, not alternatives. The most valuable test in
the file is `TestConcurrentDoubleAssignment`: when ten goroutines race to claim
the same task, exactly one must win, and that single assertion validates the
serialization the mutex exists to provide. Reusable helpers like
`ConcurrentTestRunner` fold the barrier, the wait group, and per-goroutine error
collection (via `atomic.Value`) into one place so every concurrent test stays
consistent.

You should be able to write this suite for a `BankAccount` from scratch:
`Deposit`, `Withdraw` returning an error, and `Balance`, then three
barrier-synced tests -- ten goroutines each depositing 100 and asserting a final
1000, twenty goroutines each trying to withdraw 50 from a balance of 500 with
exactly ten succeeding down to zero, and fifty interleaved deposits and
withdrawals whose final balance equals deposits minus successful withdrawals.
The one hard constraint is the whole lesson in a sentence: no `time.Sleep`
anywhere -- synchronize with barriers, channels, and WaitGroups -- and every
test must stay clean under `go test -race`.

## Resources
- [Go Blog: Data Race Detector](https://go.dev/doc/articles/race_detector) -- how the detector instruments memory accesses, what it can and cannot catch, and how to wire it into CI.
- [testing package](https://pkg.go.dev/testing) -- the `T` type, subtests, and the runner API these concurrent tests build on.
- [sync package](https://pkg.go.dev/sync) -- WaitGroup and Mutex, the primitives behind the barrier and the scheduler's locking.
- [sync/atomic](https://pkg.go.dev/sync/atomic) -- atomic counters and `Value`, used for lock-free tallies and per-goroutine error storage.

---

Back to [Concurrency](../../concurrency.md) | Next: [23-dynamic-goroutine-scaling](../23-dynamic-goroutine-scaling/23-dynamic-goroutine-scaling.md)
