# 5. runtime.Gosched

`runtime.Gosched()` is an explicit cooperative yield: the calling goroutine is placed back at the end of its P's run queue and another goroutine gets to run. With Go 1.14+ async preemption, tight loops are no longer fatal, but Gosched is still the right tool for spin-wait loops, round-robin fairness, and any algorithm that wants to cooperate with the scheduler at a known boundary. This lesson builds a testable round-robin task executor that uses Gosched to interleave concurrent work.

## Concepts

### What Gosched Does

`runtime.Gosched()` deschedules the calling goroutine and moves it to the back of the local run queue. Another goroutine on the same P gets to run. The call returns when the scheduler picks this goroutine again, which can be immediate if no other goroutine is runnable.

Gosched stays entirely in user space. It does not invoke the OS scheduler, does not sleep, and does not block on any resource. Its cost is roughly 100-200ns.

### Gosched vs time.Sleep

`time.Sleep` suspends the goroutine for a minimum duration and moves it to a timer heap. The goroutine does not consume CPU during the sleep. Gosched does not suspend; it merely relinquishes the current scheduling slot. If the run queue is empty, Gosched returns almost immediately.

### Gosched vs Channel Operations

A channel receive blocks the goroutine until a sender is ready, and the goroutine is moved off the run queue entirely. Gosched is non-blocking; the goroutine stays on the run queue. Use channels when you need coordination with another goroutine; use Gosched when you want to give other goroutines a chance to run without blocking on anything.

### When Gosched Is Useful

- Spin-wait loops that poll for a condition: call Gosched between retries to avoid burning a P without making progress.
- Round-robin fairness under GOMAXPROCS=1: without Gosched or yield points, a goroutine with a tight loop may run until preempted. Adding Gosched at natural boundaries makes the interleaving predictable.
- Cooperative algorithms where yield points are semantically meaningful, such as coroutine-style task executors.

### The Executor Pattern

The `Executor` in this lesson receives tasks as functions that accept a `yield func()`. The yield function calls `runtime.Gosched()`. Tasks call yield at natural step boundaries. Because yieldFn is a package-level variable, tests can replace it with a counter to verify that yield is called the expected number of times.

## Exercises

### Exercise 1: All Tasks Complete

Add 10 tasks to an Executor, each calling yield 5 times. Run the executor and verify that `Ran()` returns 10.

### Exercise 2: Empty Executor

Call `Run()` on an executor with no tasks. Verify it does not panic and that `Ran()` returns 0.

### Exercise 3: Verify Yield Is Called

Replace `yieldFn` with a counter in a test (using the package-level variable). Add one task that calls yield twice. Run the executor and assert that the counter reached exactly 2.

## Common Mistakes

Wrong: Using Gosched in a spin-wait loop without a backoff or termination condition. What happens: the goroutine burns CPU in a tight Gosched loop indefinitely. Fix: Add a bounded retry count or use a channel/condition variable for notification.

Wrong: Calling Gosched inside a mutex-protected critical section to let other goroutines run. What happens: the other goroutines cannot acquire the mutex; Gosched is useless here and causes unnecessary context switches. Fix: Release the lock before yielding, or redesign to avoid holding a lock while wanting other goroutines to run.

Wrong: Assuming Gosched guarantees that a specific other goroutine runs next. What happens: the scheduler picks any runnable goroutine; Gosched is a hint, not a synchronization primitive. Fix: Use channels or sync.Cond when you need to coordinate with a specific goroutine.

## Verification

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

`runtime.Gosched()` is a cheap, non-blocking cooperative yield that keeps the goroutine runnable while giving other goroutines a chance to execute. It is not a sleep and provides no ordering guarantees. Its appropriate uses are spin-wait polling loops, round-robin fairness under low parallelism, and cooperative task executors where yield points carry semantic meaning.

## What's Next

[06. Goroutine Stack Growth](../06-goroutine-stack-growth/06-goroutine-stack-growth.md)

## Resources

- https://pkg.go.dev/runtime#Gosched
- https://pkg.go.dev/runtime
- https://go.dev/doc/effective_go#goroutines

---

Create `go.mod`

```go
// go.mod
module example.com/gosched

go 1.26
```

Create `gosched.go`

```go
package gosched

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// Task is a unit of work. The function receives a yield func that
// should be called at natural boundaries to cooperate with the scheduler.
type Task func(yield func())

// yieldMu protects yieldFn so tests can swap it safely.
var yieldMu sync.RWMutex

// yieldFn calls runtime.Gosched, making it injectable in tests.
var yieldFn = func() { runtime.Gosched() }

// getYield returns the current yield function under a read lock.
func getYield() func() {
	yieldMu.RLock()
	defer yieldMu.RUnlock()
	return yieldFn
}

// setYield replaces the yield function under a write lock.
// It returns a restore function that puts the old value back.
func setYield(fn func()) func() {
	yieldMu.Lock()
	defer yieldMu.Unlock()
	old := yieldFn
	yieldFn = fn
	return func() {
		yieldMu.Lock()
		defer yieldMu.Unlock()
		yieldFn = old
	}
}

// Executor runs a set of Tasks to completion, yielding between steps.
type Executor struct {
	tasks []Task
	ran   atomic.Int64
}

// Add registers a task with the executor.
func (e *Executor) Add(t Task) {
	e.tasks = append(e.tasks, t)
}

// Run executes all registered tasks concurrently. Each task receives
// a yield func that calls runtime.Gosched(). Run blocks until all tasks finish.
func (e *Executor) Run() {
	yield := getYield()
	var wg sync.WaitGroup
	for _, t := range e.tasks {
		wg.Add(1)
		task := t
		go func() {
			defer wg.Done()
			task(yield)
			e.ran.Add(1)
		}()
	}
	wg.Wait()
}

// Ran returns the number of tasks that completed after the last Run call.
func (e *Executor) Ran() int {
	return int(e.ran.Load())
}
```

Create `gosched_test.go`

```go
package gosched

import (
	"sync/atomic"
	"testing"
)

func TestAllTasksComplete(t *testing.T) {
	t.Parallel()
	var e Executor
	const n = 10
	for i := 0; i < n; i++ {
		e.Add(func(yield func()) {
			for j := 0; j < 5; j++ {
				yield()
			}
		})
	}
	e.Run()
	if got := e.Ran(); got != n {
		t.Errorf("Ran() = %d, want %d", got, n)
	}
}

func TestTaskWithNoYield(t *testing.T) {
	t.Parallel()
	var e Executor
	e.Add(func(yield func()) {
		// intentionally no yield
	})
	e.Run()
	if e.Ran() != 1 {
		t.Errorf("Ran() = %d, want 1", e.Ran())
	}
}

func TestYieldIsCalledByTasks(t *testing.T) {
	// Not parallel: mutates package-level yieldFn.
	var calls atomic.Int64
	restore := setYield(func() { calls.Add(1) })
	defer restore()

	var e Executor
	e.Add(func(yield func()) {
		yield()
		yield()
	})
	e.Run()
	if calls.Load() != 2 {
		t.Errorf("yieldFn called %d times, want 2", calls.Load())
	}
}

func TestRunIsIdempotentAfterReset(t *testing.T) {
	t.Parallel()
	var e Executor
	e.Add(func(yield func()) {})
	e.Run()
	if e.Ran() != 1 {
		t.Fatalf("first Run: Ran() = %d, want 1", e.Ran())
	}
}

func TestEmptyExecutorRuns(t *testing.T) {
	t.Parallel()
	var e Executor
	e.Run() // must not panic
	if e.Ran() != 0 {
		t.Errorf("Ran() = %d, want 0", e.Ran())
	}
}
```

Create `example_test.go`

```go
package gosched_test

import (
	"fmt"
	"sync/atomic"

	"example.com/gosched"
)

func ExampleExecutor_Run() {
	var e gosched.Executor
	var count atomic.Int64
	for i := 0; i < 3; i++ {
		e.Add(func(yield func()) {
			count.Add(1)
			yield()
		})
	}
	e.Run()
	fmt.Println(e.Ran())
	// Output:
	// 3
}
```

Create `cmd/demo/main.go`

```go
package main

import (
	"fmt"
	"sync/atomic"

	"example.com/gosched"
)

func main() {
	var e gosched.Executor
	var steps atomic.Int64

	for id := 0; id < 5; id++ {
		taskID := id
		e.Add(func(yield func()) {
			for step := 0; step < 3; step++ {
				steps.Add(1)
				fmt.Printf("task %d step %d\n", taskID, step)
				yield()
			}
		})
	}

	e.Run()
	fmt.Printf("\nAll %d tasks done, %d total steps\n", e.Ran(), steps.Load())
}
```
