# Exercise 9: Goroutine Lifecycle

A goroutine is born when you call `go`, runs until its function returns, and
cannot be interrupted from the outside -- there is no `goroutine.Kill()`. That
single fact shapes every long-running worker you write: if you need one to
stop, you have to design it to listen for a signal and exit voluntarily. This
exercise builds a batch job processor whose main goroutine tracks worker states
through channels and requests graceful shutdown -- the same cooperative model
that queue consumers, health-check loops, and metrics exporters depend on. A
worker that silently stalls is worse than one that crashes loudly, so
observability into what each goroutine is doing is the whole point.

## What you'll build

```text
09-goroutine-lifecycle/
  main.go        worker lifecycle events, cooperative quit-channel
                 shutdown, and a state-tracking task manager
```

- Build: a batch job processor whose main goroutine observes worker lifecycle states and requests graceful shutdown.
- Implement: `worker` emitting `LifecycleEvent`s; `cooperativeWorker` that selects on a quit channel; obedient and stubborn workers that show why a kill switch is missing; a `TaskManager` with a live state dashboard.
- Verify: `go run main.go` for each step.

### Why cooperative termination is not optional

This model distinguishes Go from languages with thread interruption
mechanisms. There is no `goroutine.Kill()`. If you need a goroutine to stop,
you must design it to listen for a signal and exit voluntarily. This
cooperative model prevents the data corruption that forced termination causes
in other languages, but it requires discipline: every long-running goroutine
must have an exit path.

The batch job processor you build makes that discipline concrete. The main
goroutine tracks worker states and can request graceful shutdown -- the pattern
behind every Go service that manages background workers: queue consumers,
health check loops, metrics exporters, and scheduled jobs.

## Step 1 -- Tracking Worker States Through Channels

Build a task manager that receives lifecycle events from workers. Each worker reports when it starts, when it completes a unit of work, and when it finishes.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

type EventKind string

const (
	EventStarted   EventKind = "STARTED"
	EventProgress  EventKind = "PROGRESS"
	EventCompleted EventKind = "COMPLETED"
	EventFailed    EventKind = "FAILED"
)

type LifecycleEvent struct {
	WorkerID  int
	Kind      EventKind
	Message   string
	Timestamp time.Time
}

func worker(id int, taskCount int, events chan<- LifecycleEvent) {
	events <- LifecycleEvent{
		WorkerID:  id,
		Kind:      EventStarted,
		Message:   fmt.Sprintf("processing %d tasks", taskCount),
		Timestamp: time.Now(),
	}

	for i := 1; i <= taskCount; i++ {
		// Simulate work with variable duration
		time.Sleep(time.Duration(rand.IntN(40)+10) * time.Millisecond)

		// Simulate occasional failures
		if id == 3 && i == 2 {
			events <- LifecycleEvent{
				WorkerID:  id,
				Kind:      EventFailed,
				Message:   fmt.Sprintf("task %d/%d: corrupted input data", i, taskCount),
				Timestamp: time.Now(),
			}
			return
		}

		events <- LifecycleEvent{
			WorkerID:  id,
			Kind:      EventProgress,
			Message:   fmt.Sprintf("task %d/%d done", i, taskCount),
			Timestamp: time.Now(),
		}
	}

	events <- LifecycleEvent{
		WorkerID:  id,
		Kind:      EventCompleted,
		Message:   "all tasks finished",
		Timestamp: time.Now(),
	}
}

func main() {
	const workerCount = 5
	events := make(chan LifecycleEvent, 50)
	var wg sync.WaitGroup

	taskCounts := []int{3, 2, 4, 3, 2}

	fmt.Println("=== Batch Job Processor ===")
	fmt.Println()

	for i := 1; i <= workerCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			worker(id, taskCounts[id-1], events)
		}(i)
	}

	// Close events channel after all workers finish
	go func() {
		wg.Wait()
		close(events)
	}()

	// Monitor: collect and display lifecycle events
	completed, failed := 0, 0
	for evt := range events {
		icon := "  "
		switch evt.Kind {
		case EventStarted:
			icon = ">>"
		case EventCompleted:
			icon = "OK"
			completed++
		case EventFailed:
			icon = "!!"
			failed++
		}
		fmt.Printf("  [%s] worker-%d  %-10s  %s\n",
			icon, evt.WorkerID, evt.Kind, evt.Message)
	}

	fmt.Println(strings.Repeat("-", 55))
	fmt.Printf("  Completed: %d | Failed: %d | Total: %d\n",
		completed, failed, workerCount)
}
```

**What's happening here:** Five workers run concurrently, each processing a batch of tasks. They report lifecycle events through a shared channel. The main goroutine consumes events and tracks final outcomes. Worker 3 fails on its second task and exits early, reporting a `FAILED` event.

**Key insight:** The worker controls its own lifecycle. It decides to report events, it decides when to exit. The main goroutine observes but cannot force a worker to stop. This observability-through-channels pattern is how production Go services monitor background work.

**What would happen if a worker panicked without recovering?** The entire process would crash. Lifecycle reporting only works when workers cooperate by sending events before exiting. Panics bypass this entirely, which is why `defer/recover` is essential in worker goroutines (covered in exercise 11).

### Verification
```bash
go run main.go
```
Expected output (order varies):
```
=== Batch Job Processor ===

  [>>] worker-1  STARTED     processing 3 tasks
  [>>] worker-4  STARTED     processing 3 tasks
  [>>] worker-2  STARTED     processing 2 tasks
  [>>] worker-3  STARTED     processing 4 tasks
  [>>] worker-5  STARTED     processing 2 tasks
  [  ] worker-5  PROGRESS    task 1/2 done
  [  ] worker-2  PROGRESS    task 1/2 done
  [  ] worker-1  PROGRESS    task 1/3 done
  [  ] worker-4  PROGRESS    task 1/3 done
  [  ] worker-3  PROGRESS    task 1/4 done
  [!!] worker-3  FAILED      task 2/4: corrupted input data
  [  ] worker-5  PROGRESS    task 2/2 done
  [OK] worker-5  COMPLETED   all tasks finished
  ...
-------------------------------------------------------
  Completed: 4 | Failed: 1 | Total: 5
```

## Step 2 -- Cooperative Shutdown: You Cannot Kill a Goroutine

Demonstrate that goroutines must voluntarily exit. Build workers that check a quit channel on every iteration so the main goroutine can request graceful shutdown.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

type WorkerStatus struct {
	ID        int
	Processed int
	Reason    string
}

func cooperativeWorker(id int, quit <-chan struct{}, done chan<- WorkerStatus) {
	processed := 0

	for {
		select {
		case <-quit:
			done <- WorkerStatus{
				ID:        id,
				Processed: processed,
				Reason:    "shutdown requested",
			}
			return
		default:
			// Simulate processing one item
			time.Sleep(time.Duration(rand.IntN(30)+10) * time.Millisecond)
			processed++
		}
	}
}

func main() {
	const workerCount = 4
	quit := make(chan struct{})
	done := make(chan WorkerStatus, workerCount)

	fmt.Println("=== Cooperative Shutdown Demo ===")
	fmt.Println("Workers run until main signals quit.")
	fmt.Println()

	var wg sync.WaitGroup
	for i := 1; i <= workerCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			cooperativeWorker(id, quit, done)
		}(i)
	}

	// Let workers run for 200ms, then request shutdown
	runDuration := 200 * time.Millisecond
	fmt.Printf("  Letting workers run for %v...\n", runDuration)
	time.Sleep(runDuration)

	fmt.Println("  Sending shutdown signal...")
	close(quit) // broadcast: all workers receive this

	// Collect shutdown reports
	wg.Wait()
	close(done)

	fmt.Println()
	totalProcessed := 0
	for status := range done {
		fmt.Printf("  worker-%d: processed %d items, stopped: %s\n",
			status.ID, status.Processed, status.Reason)
		totalProcessed += status.Processed
	}
	fmt.Printf("\n  Total items processed: %d\n", totalProcessed)
	fmt.Println()
	fmt.Println("  Key point: workers stopped because they CHECKED the quit channel.")
	fmt.Println("  If a worker ignored quit, it would run forever. There is no force-kill.")
}
```

**What's happening here:** Four workers run in an infinite loop, processing items continuously. The main goroutine waits 200ms, then closes the quit channel. Closing a channel is a broadcast signal: every worker's `select` case `<-quit` fires simultaneously. Each worker exits its loop gracefully and reports how many items it processed.

**Key insight:** `close(quit)` is a broadcast to all listeners. Every goroutine blocked on `<-quit` receives the zero value immediately. This is the standard Go pattern for signaling multiple goroutines at once. A send (`quit <- struct{}{}`) would only unblock ONE goroutine; closing unblocks ALL of them.

**What if a worker has a tight CPU loop without the `select`?** It would never check the quit channel and would run forever, consuming a CPU core. This is why cooperative shutdown requires discipline: every long-running goroutine must periodically check for termination signals.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Cooperative Shutdown Demo ===
Workers run until main signals quit.

  Letting workers run for 200ms...
  Sending shutdown signal...

  worker-2: processed 8 items, stopped: shutdown requested
  worker-1: processed 7 items, stopped: shutdown requested
  worker-4: processed 9 items, stopped: shutdown requested
  worker-3: processed 6 items, stopped: shutdown requested

  Total items processed: 30

  Key point: workers stopped because they CHECKED the quit channel.
  If a worker ignored quit, it would run forever. There is no force-kill.
```

## Step 3 -- Uncooperative Goroutine: The Problem

Show what happens when a goroutine ignores the quit signal. This demonstrates why cooperative termination is not optional.

```go
package main

import (
	"fmt"
	"runtime"
	"sync"
	"time"
)

func obedientWorker(id int, quit <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		select {
		case <-quit:
			fmt.Printf("  worker-%d: received quit, exiting gracefully\n", id)
			return
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func stubbornWorker(id int, quit <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	// BUG: ignores quit channel entirely
	for i := 0; i < 10; i++ {
		time.Sleep(50 * time.Millisecond)
		fmt.Printf("  worker-%d: still running (iteration %d), ignoring quit\n", id, i+1)
	}
	fmt.Printf("  worker-%d: finally done after ignoring shutdown for 500ms\n", id)
}

func main() {
	fmt.Println("=== Uncooperative Goroutine Problem ===")
	fmt.Printf("  Goroutines at start: %d\n\n", runtime.NumGoroutine())

	quit := make(chan struct{})
	var wg sync.WaitGroup

	// Launch 2 obedient + 1 stubborn
	for i := 1; i <= 2; i++ {
		wg.Add(1)
		go obedientWorker(i, quit, &wg)
	}
	wg.Add(1)
	go stubbornWorker(3, quit, &wg)

	fmt.Printf("  Goroutines after launch: %d\n", runtime.NumGoroutine())

	// Give workers time to start, then signal shutdown
	time.Sleep(100 * time.Millisecond)
	fmt.Println()
	fmt.Println("  --- Sending quit signal ---")
	fmt.Println()
	close(quit)

	// Obedient workers exit quickly, but we must wait for the stubborn one
	wg.Wait()

	fmt.Printf("\n  Goroutines after all done: %d\n", runtime.NumGoroutine())
	fmt.Println()
	fmt.Println("  Lesson: the stubborn worker delayed shutdown by ~400ms.")
	fmt.Println("  In production, this means your service takes longer to restart,")
	fmt.Println("  in-flight requests may timeout, and Kubernetes may SIGKILL your pod.")
}
```

**What's happening here:** Two obedient workers check the quit channel and exit immediately. The stubborn worker ignores the quit channel and continues running for its full loop duration. The program cannot exit until the stubborn worker's `WaitGroup.Done()` is called, delaying shutdown by hundreds of milliseconds.

**Key insight:** There is no `goroutine.Kill()` in Go. If a goroutine does not check for a termination signal, you cannot stop it. In production, this translates to slow graceful shutdowns, Kubernetes SIGKILL after the grace period, and lost in-flight work. Every worker goroutine you write must have a quit check in its main loop.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Uncooperative Goroutine Problem ===
  Goroutines at start: 1

  Goroutines after launch: 4

  --- Sending quit signal ---

  worker-1: received quit, exiting gracefully
  worker-2: received quit, exiting gracefully
  worker-3: still running (iteration 3), ignoring quit
  worker-3: still running (iteration 4), ignoring quit
  ...
  worker-3: finally done after ignoring shutdown for 500ms

  Goroutines after all done: 1

  Lesson: the stubborn worker delayed shutdown by ~400ms.
  In production, this means your service takes longer to restart,
  in-flight requests may timeout, and Kubernetes may SIGKILL your pod.
```

## Step 4 -- Complete Task Manager with State Dashboard

Combine lifecycle tracking and cooperative shutdown into a task manager that provides a real-time state view of all workers.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

type State string

const (
	StateIdle      State = "IDLE"
	StateRunning   State = "RUNNING"
	StateDone      State = "DONE"
	StateFailed    State = "FAILED"
	StateCancelled State = "CANCELLED"
)

type StateChange struct {
	WorkerID  int
	NewState  State
	Detail    string
	Timestamp time.Time
}

type TaskManager struct {
	mu       sync.Mutex
	states   map[int]State
	events   []StateChange
	changeCh chan StateChange
}

func NewTaskManager(workerCount int) *TaskManager {
	states := make(map[int]State, workerCount)
	for i := 1; i <= workerCount; i++ {
		states[i] = StateIdle
	}
	return &TaskManager{
		states:   states,
		changeCh: make(chan StateChange, 100),
	}
}

func (tm *TaskManager) RecordChange(change StateChange) {
	tm.changeCh <- change
}

func (tm *TaskManager) ProcessEvents() {
	for change := range tm.changeCh {
		tm.mu.Lock()
		tm.states[change.WorkerID] = change.NewState
		tm.events = append(tm.events, change)
		tm.mu.Unlock()
	}
}

func (tm *TaskManager) PrintDashboard() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	fmt.Println()
	fmt.Println("  --- Worker Dashboard ---")
	for id := 1; id <= len(tm.states); id++ {
		state := tm.states[id]
		icon := " "
		switch state {
		case StateRunning:
			icon = ">"
		case StateDone:
			icon = "+"
		case StateFailed:
			icon = "!"
		case StateCancelled:
			icon = "x"
		}
		fmt.Printf("  [%s] worker-%d: %s\n", icon, id, state)
	}

	counts := map[State]int{}
	for _, s := range tm.states {
		counts[s]++
	}
	fmt.Printf("  Running: %d | Done: %d | Failed: %d | Cancelled: %d\n",
		counts[StateRunning], counts[StateDone],
		counts[StateFailed], counts[StateCancelled])
}

func managedWorker(id int, quit <-chan struct{}, tm *TaskManager, wg *sync.WaitGroup) {
	defer wg.Done()

	tm.RecordChange(StateChange{
		WorkerID: id, NewState: StateRunning,
		Detail: "starting work", Timestamp: time.Now(),
	})

	tasksDone := 0
	totalTasks := rand.IntN(4) + 2

	for i := 0; i < totalTasks; i++ {
		select {
		case <-quit:
			tm.RecordChange(StateChange{
				WorkerID: id, NewState: StateCancelled,
				Detail:    fmt.Sprintf("cancelled after %d/%d tasks", tasksDone, totalTasks),
				Timestamp: time.Now(),
			})
			return
		default:
			time.Sleep(time.Duration(rand.IntN(50)+20) * time.Millisecond)
			tasksDone++

			// Simulate random failure
			if id == 2 && tasksDone == 2 {
				tm.RecordChange(StateChange{
					WorkerID: id, NewState: StateFailed,
					Detail:    fmt.Sprintf("error on task %d: connection reset", tasksDone),
					Timestamp: time.Now(),
				})
				return
			}
		}
	}

	tm.RecordChange(StateChange{
		WorkerID: id, NewState: StateDone,
		Detail:    fmt.Sprintf("completed %d tasks", totalTasks),
		Timestamp: time.Now(),
	})
}

func main() {
	const workerCount = 6

	tm := NewTaskManager(workerCount)
	quit := make(chan struct{})
	var wg sync.WaitGroup

	// Start event processor
	go tm.ProcessEvents()

	fmt.Println("=== Task Manager with State Tracking ===")
	tm.PrintDashboard()

	// Launch all workers
	for i := 1; i <= workerCount; i++ {
		wg.Add(1)
		go managedWorker(i, quit, tm, &wg)
	}

	// Let workers run for a while
	time.Sleep(150 * time.Millisecond)
	tm.PrintDashboard()

	// Wait for all to finish naturally (or cancel if needed)
	wg.Wait()

	// Allow final events to be processed
	close(tm.changeCh)
	time.Sleep(10 * time.Millisecond)

	tm.PrintDashboard()

	fmt.Println()
	fmt.Println(strings.Repeat("-", 50))
	fmt.Println("  Event log:")
	for _, evt := range tm.events {
		fmt.Printf("    worker-%d -> %-10s  %s\n", evt.WorkerID, evt.NewState, evt.Detail)
	}
}
```

**What's happening here:** The `TaskManager` maintains a state map for each worker and processes state change events from a channel. Workers report transitions (IDLE -> RUNNING -> DONE/FAILED/CANCELLED) through the task manager. The dashboard can be printed at any point to see a snapshot of all worker states. Worker 2 simulates a failure, and the quit channel is available for cancellation if needed.

**Key insight:** This is the observability pattern used in production task queues and job schedulers. The task manager does not control workers directly -- it observes their self-reported state changes. Workers own their lifecycle; the manager owns the view. This separation keeps the system simple and avoids the complexity of external goroutine control.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Task Manager with State Tracking ===

  --- Worker Dashboard ---
  [ ] worker-1: IDLE
  [ ] worker-2: IDLE
  [ ] worker-3: IDLE
  [ ] worker-4: IDLE
  [ ] worker-5: IDLE
  [ ] worker-6: IDLE
  Running: 0 | Done: 0 | Failed: 0 | Cancelled: 0

  --- Worker Dashboard ---
  [>] worker-1: RUNNING
  [!] worker-2: FAILED
  [>] worker-3: RUNNING
  [+] worker-4: DONE
  [>] worker-5: RUNNING
  [>] worker-6: RUNNING
  Running: 4 | Done: 1 | Failed: 1 | Cancelled: 0

  --- Worker Dashboard ---
  [+] worker-1: DONE
  [!] worker-2: FAILED
  [+] worker-3: DONE
  [+] worker-4: DONE
  [+] worker-5: DONE
  [+] worker-6: DONE
  Running: 0 | Done: 5 | Failed: 1 | Cancelled: 0

--------------------------------------------------
  Event log:
    worker-1 -> RUNNING     starting work
    worker-2 -> RUNNING     starting work
    ...
    worker-2 -> FAILED      error on task 2: connection reset
    worker-4 -> DONE        completed 3 tasks
    ...
```

## Common Mistakes

### Assuming You Can Stop a Goroutine From Outside

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"time"
)

func main() {
	go func() {
		for {
			fmt.Println("working...")
			time.Sleep(100 * time.Millisecond)
		}
	}()

	time.Sleep(300 * time.Millisecond)
	// No way to stop the goroutine above!
	// The only "exit" is terminating the entire process.
	fmt.Println("cannot stop the goroutine -- exiting process")
}
```

**What happens:** The goroutine runs until the process exits. In a long-running server, this goroutine would run forever, consuming resources. There is no API to terminate it externally.

**Correct -- design for cooperative shutdown:**
```go
package main

import (
	"fmt"
	"time"
)

func main() {
	quit := make(chan struct{})

	go func() {
		for {
			select {
			case <-quit:
				fmt.Println("worker: received quit, cleaning up")
				return
			default:
				fmt.Println("working...")
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()

	time.Sleep(300 * time.Millisecond)
	close(quit)
	time.Sleep(50 * time.Millisecond) // allow cleanup
	fmt.Println("worker stopped gracefully")
}
```

### Using a Send Instead of Close for Multi-Goroutine Shutdown

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	quit := make(chan struct{}, 1)
	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			select {
			case <-quit:
				fmt.Printf("worker %d: quit\n", id)
			case <-time.After(1 * time.Second):
				fmt.Printf("worker %d: timed out waiting for quit\n", id)
			}
		}(i)
	}

	quit <- struct{}{} // only ONE goroutine receives this!
	wg.Wait()
}
```

**What happens:** Only one of the three goroutines receives the quit signal. The other two wait for the full second timeout. In production, two of your three workers keep running after you intended to shut down.

**Fix:** Use `close(quit)` to broadcast to all listeners simultaneously.

## Review

A goroutine's life has exactly three states -- created by `go`, running, and
terminated when its function returns -- and nothing outside it can force the
last transition. That is why every pattern here routes through a channel the
worker itself checks: lifecycle events flow out so the manager can observe
state without owning it, and a quit signal flows in so the worker can choose to
leave. The asymmetry between `close(quit)` and `quit <- struct{}{}` is the
load-bearing detail -- closing broadcasts the zero value to every goroutine
blocked on the channel at once, while a single send wakes exactly one. A worker
that never reaches its `select` never sees either signal, so the discipline is
non-negotiable: every long-running goroutine needs an exit path in its main
loop, and an uncooperative one drags out shutdown until Kubernetes escalates to
SIGKILL.

You should now be able to build a service supervisor without re-reading the
steps: launch a handful of workers that each simulate a microservice, have them
report their state (STARTING, HEALTHY, DEGRADED, STOPPED) over an event
channel, and drive a dashboard that reprints on a `time.NewTicker` while a
`select` multiplexes the ticker against lifecycle events and a timeout. When the
deadline passes, `close(quit)` to broadcast shutdown, let the DEGRADED workers
take longer to clean up, and print each worker's total uptime and final state.
If any worker keeps running after the broadcast, the bug is a missing quit
check -- not a missing kill switch, because Go does not have one.

## Resources
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how pipeline stages signal completion and propagate shutdown across goroutines.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) -- the context package as the standard way to carry cancellation across a goroutine tree.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- how channels synchronize goroutines and the idioms for signaling with them.
- [Go Tour: Select](https://go.dev/tour/concurrency/5) -- the select statement that lets a worker watch a quit channel without blocking on it.

---

Back to [Concurrency](../../concurrency.md) | Next: [10-goroutine-leak-detection](../10-goroutine-leak-detection/10-goroutine-leak-detection.md)
