# Exercise 11: WaitGroup Patterns

`sync.WaitGroup` is three methods -- Add, Done, Wait -- and every one of the hard
bugs lives in how you combine them. Call `Add` inside the goroutine instead of
before it and `Wait` can return before the counter ever rises. Beyond that first
trap, real systems need patterns the basics do not teach: nested WaitGroups for
stages that run in parallel internally but sequentially between each other, a
WaitGroup paired with a mutex to collect every worker's error rather than only
the first, and a WaitGroup wired to a done channel so any worker can abort the
rest. This exercise builds all four.

## What you'll build

```text
11-waitgroup-patterns/
  main.go        the dynamic-Add bug and its fix, a staged deployment
                 orchestrator, an error-collecting batch, a cancelable pool
```

- Build: four WaitGroup patterns -- the dynamic-Add bug and its fix, a staged deployment orchestrator, an error-collecting batch processor, and a cancelable worker pool.
- Implement: `runTasksBuggy`/`runTasksCorrect`, `runStage` over nested WaitGroups, a mutex-guarded `BatchProcessor`, and a `WorkerPool` with a done channel and `sync.Once`.
- Verify: `go run -race main.go` on each step.

### Why the patterns matter more than the primitive

A deployment orchestrator that rolls out infrastructure first, then services,
then health checks -- each stage parallel internally but sequential between
stages -- needs nested WaitGroups. A batch processor that must collect errors
from 50 parallel workers needs a WaitGroup combined with a mutex-protected error
slice. A pipeline that must abort all workers when the first error occurs needs a
WaitGroup integrated with a done channel.

These patterns appear in CI/CD pipelines, data-migration tools,
distributed-system coordinators, and any program that manages multi-stage
parallel operations. The WaitGroup itself is simple; the patterns around it are
where the real engineering lives.

## Step 1 -- The Dynamic Add Bug

The most common WaitGroup bug is calling `wg.Add(1)` inside a goroutine instead of before launching it. This creates a race: `wg.Wait()` might return before the goroutine has called `Add`.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type TaskResult struct {
	TaskID int
	Output string
}

func runTasksBuggy(taskCount int) []TaskResult {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []TaskResult

	for i := 0; i < taskCount; i++ {
		go func(id int) {
			// BUG: wg.Add(1) inside the goroutine.
			// wg.Wait() may return before this line executes.
			wg.Add(1)
			defer wg.Done()

			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			results = append(results, TaskResult{TaskID: id, Output: fmt.Sprintf("result-%d", id)})
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	return results
}

func runTasksCorrect(taskCount int) []TaskResult {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var results []TaskResult

	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			results = append(results, TaskResult{TaskID: id, Output: fmt.Sprintf("result-%d", id)})
			mu.Unlock()
		}(i)
	}

	wg.Wait()
	return results
}

func main() {
	fmt.Println("=== Buggy version (Add inside goroutine) ===")
	buggyResults := runTasksBuggy(10)
	fmt.Printf("expected 10 results, got %d (race condition: may vary)\n\n", len(buggyResults))

	fmt.Println("=== Correct version (Add before goroutine) ===")
	correctResults := runTasksCorrect(10)
	fmt.Printf("expected 10 results, got %d (always correct)\n", len(correctResults))

	fmt.Println("\n--- Rule ---")
	fmt.Println("ALWAYS call wg.Add(1) BEFORE the go statement, never inside the goroutine.")
	fmt.Println("wg.Wait() returns when the counter is zero. If Add has not been called yet,")
	fmt.Println("the counter is already zero and Wait returns immediately.")
}
```

In the buggy version, `wg.Wait()` can return before any goroutine has called `wg.Add(1)`, because the scheduler may execute `Wait` before any goroutine starts. The correct version calls `Add` in the launching goroutine, guaranteeing the counter is incremented before `Wait` is reached.

### Verification
```
=== Buggy version (Add inside goroutine) ===
expected 10 results, got 0 (race condition: may vary)

=== Correct version (Add before goroutine) ===
expected 10 results, got 10 (always correct)

--- Rule ---
ALWAYS call wg.Add(1) BEFORE the go statement, never inside the goroutine.
wg.Wait() returns when the counter is zero. If Add has not been called yet,
the counter is already zero and Wait returns immediately.
```
The buggy version returns 0 results most of the time. The correct version always returns 10.

## Step 2 -- Nested WaitGroups for Staged Deployments

Build a deployment orchestrator that runs stages sequentially, but tasks within each stage run in parallel. Stage 1 deploys infrastructure. Stage 2 deploys services (only after infrastructure is ready). Stage 3 runs health checks.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type DeployTask struct {
	Name     string
	Duration time.Duration
}

type StageResult struct {
	Stage    string
	Tasks    []string
	Elapsed  time.Duration
}

func runStage(stageName string, tasks []DeployTask) StageResult {
	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var completed []string

	for _, task := range tasks {
		wg.Add(1)
		go func(t DeployTask) {
			defer wg.Done()
			fmt.Printf("  [%s] starting: %s\n", stageName, t.Name)
			time.Sleep(t.Duration)
			mu.Lock()
			completed = append(completed, t.Name)
			mu.Unlock()
			fmt.Printf("  [%s] finished: %s\n", stageName, t.Name)
		}(task)
	}

	wg.Wait()
	return StageResult{
		Stage:   stageName,
		Tasks:   completed,
		Elapsed: time.Since(start),
	}
}

func deploy() []StageResult {
	stages := []struct {
		name  string
		tasks []DeployTask
	}{
		{
			name: "infrastructure",
			tasks: []DeployTask{
				{Name: "create-vpc", Duration: 80 * time.Millisecond},
				{Name: "create-database", Duration: 120 * time.Millisecond},
				{Name: "create-cache", Duration: 60 * time.Millisecond},
			},
		},
		{
			name: "services",
			tasks: []DeployTask{
				{Name: "deploy-api", Duration: 50 * time.Millisecond},
				{Name: "deploy-worker", Duration: 70 * time.Millisecond},
				{Name: "deploy-scheduler", Duration: 40 * time.Millisecond},
			},
		},
		{
			name: "health-checks",
			tasks: []DeployTask{
				{Name: "check-api-health", Duration: 30 * time.Millisecond},
				{Name: "check-worker-health", Duration: 30 * time.Millisecond},
				{Name: "check-database-health", Duration: 20 * time.Millisecond},
			},
		},
	}

	var results []StageResult
	for _, stage := range stages {
		fmt.Printf("\n--- Stage: %s ---\n", stage.name)
		result := runStage(stage.name, stage.tasks)
		results = append(results, result)
		fmt.Printf("--- Stage %s complete (%v) ---\n", stage.name, result.Elapsed.Round(time.Millisecond))
	}

	return results
}

func main() {
	fmt.Println("=== Deployment Orchestrator ===")

	results := deploy()

	fmt.Println("\n=== Deployment Summary ===")
	var totalTime time.Duration
	for _, r := range results {
		fmt.Printf("  %-20s %d tasks in %v\n", r.Stage, len(r.Tasks), r.Elapsed.Round(time.Millisecond))
		totalTime += r.Elapsed
	}
	fmt.Printf("  total deployment time: %v\n", totalTime.Round(time.Millisecond))
	fmt.Println("\nStages run sequentially. Tasks within each stage run in parallel.")
	fmt.Println("Each stage has its own WaitGroup. The outer loop is the sequential coordinator.")
}
```

Each stage gets its own WaitGroup. `runStage` blocks until all tasks in that stage are complete. The outer `for` loop ensures stages execute sequentially: infrastructure before services, services before health checks. Within each stage, tasks run in parallel (deploying vpc, database, and cache simultaneously).

### Verification
```
=== Deployment Orchestrator ===

--- Stage: infrastructure ---
  [infrastructure] starting: create-vpc
  [infrastructure] starting: create-database
  [infrastructure] starting: create-cache
  [infrastructure] finished: create-cache
  [infrastructure] finished: create-vpc
  [infrastructure] finished: create-database
--- Stage infrastructure complete (120ms) ---

--- Stage: services ---
  [services] starting: deploy-api
  [services] starting: deploy-worker
  [services] starting: deploy-scheduler
  [services] finished: deploy-scheduler
  [services] finished: deploy-api
  [services] finished: deploy-worker
--- Stage services complete (70ms) ---

--- Stage: health-checks ---
  [health-checks] starting: check-api-health
  [health-checks] starting: check-worker-health
  [health-checks] starting: check-database-health
  [health-checks] finished: check-database-health
  [health-checks] finished: check-api-health
  [health-checks] finished: check-worker-health
--- Stage health-checks complete (30ms) ---

=== Deployment Summary ===
  infrastructure       3 tasks in 120ms
  services             3 tasks in 70ms
  health-checks        3 tasks in 30ms
  total deployment time: 220ms
```
Each stage takes as long as its slowest task (parallel). Total time is the sum of stage times (sequential).

## Step 3 -- WaitGroup with Error Collection

Build a batch processor that runs N tasks in parallel, collects all errors, and reports them after all tasks complete. This requires combining WaitGroup with a mutex-protected error slice.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type BatchResult struct {
	Succeeded int
	Failed    int
	Errors    []TaskError
}

type TaskError struct {
	TaskID int
	Err    string
}

type BatchProcessor struct {
	mu      sync.Mutex
	errors  []TaskError
	success int
}

func NewBatchProcessor() *BatchProcessor {
	return &BatchProcessor{}
}

func (bp *BatchProcessor) recordSuccess() {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.success++
}

func (bp *BatchProcessor) recordError(taskID int, err string) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	bp.errors = append(bp.errors, TaskError{TaskID: taskID, Err: err})
}

func (bp *BatchProcessor) Result() BatchResult {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	return BatchResult{
		Succeeded: bp.success,
		Failed:    len(bp.errors),
		Errors:    append([]TaskError{}, bp.errors...),
	}
}

func (bp *BatchProcessor) processTask(id int) error {
	time.Sleep(20 * time.Millisecond)

	if id%4 == 0 {
		return fmt.Errorf("connection refused for task %d", id)
	}
	if id%7 == 0 {
		return fmt.Errorf("timeout processing task %d", id)
	}
	return nil
}

func (bp *BatchProcessor) Run(taskCount int) BatchResult {
	var wg sync.WaitGroup

	for i := 0; i < taskCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			if err := bp.processTask(id); err != nil {
				bp.recordError(id, err.Error())
				fmt.Printf("  task %2d: FAILED (%s)\n", id, err.Error())
			} else {
				bp.recordSuccess()
				fmt.Printf("  task %2d: OK\n", id)
			}
		}(i)
	}

	wg.Wait()
	return bp.Result()
}

func main() {
	fmt.Printf("=== Batch Processor with Error Collection ===\n\n")

	processor := NewBatchProcessor()
	result := processor.Run(16)

	fmt.Println("\n=== Batch Report ===")
	fmt.Printf("  succeeded: %d\n", result.Succeeded)
	fmt.Printf("  failed:    %d\n", result.Failed)

	if len(result.Errors) > 0 {
		fmt.Println("\n  errors:")
		for _, e := range result.Errors {
			fmt.Printf("    task %d: %s\n", e.TaskID, e.Err)
		}
	}

	fmt.Println("\nPattern: WaitGroup waits for all tasks. Mutex protects the shared error slice.")
	fmt.Println("All errors are collected, not just the first one.")
}
```

The key insight: WaitGroup ensures all tasks complete before reading the results. The mutex protects concurrent writes to the errors slice and success counter. This is the standard pattern when you need all errors from a parallel batch, not just the first.

### Verification
```
=== Batch Processor with Error Collection ===

  task  1: OK
  task  3: OK
  task  0: FAILED (connection refused for task 0)
  task  2: OK
  task  4: FAILED (connection refused for task 4)
  task  5: OK
  task  6: OK
  task  7: FAILED (timeout processing task 7)
  task  8: FAILED (connection refused for task 8)
  task  9: OK
  task 10: OK
  task 11: OK
  task 12: FAILED (connection refused for task 12)
  task 13: OK
  task 14: FAILED (timeout processing task 14)
  task 15: OK

=== Batch Report ===
  succeeded: 10
  failed:    6

  errors:
    task 0: connection refused for task 0
    task 4: connection refused for task 4
    task 7: timeout processing task 7
    task 8: connection refused for task 8
    task 12: connection refused for task 12
    task 14: timeout processing task 14

Pattern: WaitGroup waits for all tasks. Mutex protects the shared error slice.
All errors are collected, not just the first one.
```
All 16 tasks run. Failures are collected, not lost. The report shows every error.

## Step 4 -- WaitGroup with Channel-Based Cancellation

Build a worker pool that uses WaitGroup for completion tracking and a done channel for early abort. When the first critical error occurs, all workers stop.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type WorkItem struct {
	ID       int
	Priority string
}

type WorkerPool struct {
	workerCount int
	mu          sync.Mutex
	completed   []int
	firstError  string
}

func NewWorkerPool(workerCount int) *WorkerPool {
	return &WorkerPool{
		workerCount: workerCount,
	}
}

func (wp *WorkerPool) recordComplete(id int) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	wp.completed = append(wp.completed, id)
}

func (wp *WorkerPool) setFirstError(err string) {
	wp.mu.Lock()
	defer wp.mu.Unlock()
	if wp.firstError == "" {
		wp.firstError = err
	}
}

func (wp *WorkerPool) worker(id int, done <-chan struct{}, tasks <-chan WorkItem, wg *sync.WaitGroup, cancelFn func()) {
	defer wg.Done()

	for {
		select {
		case <-done:
			fmt.Printf("  worker-%d: received shutdown signal\n", id)
			return
		case task, ok := <-tasks:
			if !ok {
				fmt.Printf("  worker-%d: task channel closed\n", id)
				return
			}

			if task.ID == 7 && task.Priority == "critical" {
				errMsg := fmt.Sprintf("critical failure on task %d", task.ID)
				fmt.Printf("  worker-%d: %s -- aborting all workers\n", id, errMsg)
				wp.setFirstError(errMsg)
				cancelFn()
				return
			}

			time.Sleep(15 * time.Millisecond)
			wp.recordComplete(task.ID)
			fmt.Printf("  worker-%d: completed task %d (%s)\n", id, task.ID, task.Priority)
		}
	}
}

func (wp *WorkerPool) Run(items []WorkItem) (completed []int, firstErr string) {
	done := make(chan struct{})
	tasks := make(chan WorkItem, len(items))
	var wg sync.WaitGroup

	var cancelOnce sync.Once
	cancelFn := func() {
		cancelOnce.Do(func() { close(done) })
	}

	for i := 0; i < wp.workerCount; i++ {
		wg.Add(1)
		go wp.worker(i, done, tasks, &wg, cancelFn)
	}

	go func() {
		for _, item := range items {
			select {
			case <-done:
				return
			case tasks <- item:
			}
		}
		close(tasks)
	}()

	wg.Wait()

	wp.mu.Lock()
	defer wp.mu.Unlock()
	return append([]int{}, wp.completed...), wp.firstError
}

func main() {
	fmt.Printf("=== Worker Pool with Early Abort ===\n\n")

	items := []WorkItem{
		{ID: 1, Priority: "normal"},
		{ID: 2, Priority: "normal"},
		{ID: 3, Priority: "normal"},
		{ID: 4, Priority: "normal"},
		{ID: 5, Priority: "normal"},
		{ID: 6, Priority: "normal"},
		{ID: 7, Priority: "critical"},
		{ID: 8, Priority: "normal"},
		{ID: 9, Priority: "normal"},
		{ID: 10, Priority: "normal"},
	}

	pool := NewWorkerPool(3)
	completed, firstErr := pool.Run(items)

	fmt.Println("\n=== Result ===")
	fmt.Printf("  completed tasks: %v\n", completed)
	fmt.Printf("  total completed: %d out of %d\n", len(completed), len(items))
	if firstErr != "" {
		fmt.Printf("  aborted due to: %s\n", firstErr)
	}

	fmt.Println("\nPattern: WaitGroup tracks worker lifecycle.")
	fmt.Println("Done channel signals all workers to stop on critical error.")
	fmt.Println("sync.Once ensures done is closed exactly once.")
}
```

The done channel and cancel function give any worker the ability to abort the entire pool. `sync.Once` ensures the done channel is closed exactly once even if multiple workers encounter errors simultaneously. WaitGroup ensures the caller blocks until all workers have fully exited.

### Verification
```
=== Worker Pool with Early Abort ===

  worker-0: completed task 1 (normal)
  worker-1: completed task 2 (normal)
  worker-2: completed task 3 (normal)
  worker-0: completed task 4 (normal)
  worker-1: completed task 5 (normal)
  worker-2: completed task 6 (normal)
  worker-0: critical failure on task 7 -- aborting all workers
  worker-1: received shutdown signal
  worker-2: received shutdown signal

=== Result ===
  completed tasks: [1 2 3 4 5 6]
  total completed: 6 out of 10
  aborted due to: critical failure on task 7

Pattern: WaitGroup tracks worker lifecycle.
Done channel signals all workers to stop on critical error.
sync.Once ensures done is closed exactly once.
```
Workers 1-6 complete. Task 7 triggers abort. Workers exit. Tasks 8-10 are never processed.

## Verification

Run each step with the race detector:
```bash
go run -race main.go
```
All steps should complete without race warnings.

## Common Mistakes

### 1. Calling Add Inside the Goroutine
This is Step 1's core lesson. The fix is simple: always call `wg.Add(1)` before the `go` statement.

### 2. Calling Done More Times Than Add
If `Done` is called more times than `Add`, the WaitGroup counter goes negative and panics. This usually happens with incorrect loop logic or calling `Done` outside a defer:

```go
// BAD: Done called in both the success and error paths, plus the defer.
wg.Add(1)
go func() {
    defer wg.Done()
    if err := process(); err != nil {
        wg.Done() // PANIC: counter goes negative.
        return
    }
}()

// GOOD: single Done via defer.
wg.Add(1)
go func() {
    defer wg.Done()
    if err := process(); err != nil {
        return // defer handles Done.
    }
}()
```

### 3. Reusing a WaitGroup After Wait Returns
After `Wait` returns, you can reuse the WaitGroup by calling `Add` again. But if you call `Add` concurrently with `Wait`, you get undefined behavior. The safest approach is to create a new WaitGroup for each stage.

### 4. Not Protecting Shared State Alongside WaitGroup
WaitGroup synchronizes goroutine completion, not data access. If goroutines write to a shared slice or map, you still need a mutex:

```go
// BAD: data race on results slice.
go func() {
    defer wg.Done()
    results = append(results, compute()) // RACE
}()

// GOOD: mutex protects shared data.
go func() {
    defer wg.Done()
    result := compute()
    mu.Lock()
    results = append(results, result)
    mu.Unlock()
}()
```

## Review

WaitGroup patterns go beyond Add/Done/Wait. The dynamic-Add bug -- calling `Add`
inside the goroutine -- is the most common mistake, because the scheduler can
reach `Wait` before the goroutine runs its `Add`, and with the counter still
zero `Wait` returns immediately; always `Add` before the `go` statement. Nested
WaitGroups coordinate staged execution: parallel within a stage, sequential
between stages, each stage with its own group. A WaitGroup plus a
mutex-protected slice collects every failure from a parallel batch rather than
just the first. And a WaitGroup joined to a done channel enables early abort: any
worker can signal shutdown, `sync.Once` fires that signal exactly once even under
a simultaneous rush of errors, and the WaitGroup guarantees every worker has
fully exited before the caller reads the results.

Four questions confirm the patterns are yours. You should be able to explain why
`wg.Add(1)` inside the goroutine races -- `Wait` observes a zero counter and
returns before the goroutine increments it -- and why moving the `Add` before the
`go` closes that window. You should be able to sketch a two-stage pipeline where
each stage owns its own WaitGroup and the second only starts once the first's
`Wait` returns; to pair a WaitGroup with a mutex so parallel workers append
results to a shared slice without racing; and to wire a done channel into a
WaitGroup so the first critical error stops every worker while the group still
waits for them all to unwind cleanly.

## Resources
- [sync.WaitGroup documentation](https://pkg.go.dev/sync#WaitGroup) -- the Add/Done/Wait contract, including the rule that Add must precede Wait.
- [Go Concurrency Patterns: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- the done-channel cancellation idiom the worker pool uses.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- goroutines, channels, and the share-by-communicating guidance behind error collection.
- [sync.Once documentation](https://pkg.go.dev/sync#Once) -- the guarantee that closes the done channel exactly once across racing workers.

---

Back to [Concurrency](../../concurrency.md) | Next: [12-mutex-granularity](../12-mutex-granularity/12-mutex-granularity.md)
