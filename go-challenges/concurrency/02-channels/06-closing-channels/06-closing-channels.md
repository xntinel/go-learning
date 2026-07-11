# Exercise 6: Closing Channels

A task dispatcher that runs out of work needs to tell every worker in its pool
to shut down at once. You could send a "stop" message to each worker
individually, but that means knowing exactly how many workers exist and
delivering exactly one message to each. Closing the channel does it in a single
call: every worker blocking on `<-tasks` unblocks with a zero value the instant
the channel closes. This exercise is about that broadcast and its sharp edges --
when a read returns the zero value, how the comma-ok idiom tells a real zero
from a closed channel, and the two operations that panic.

## What you'll build

```text
06-closing-channels/
  main.go        zero-value reads, comma-ok detection, close-as-broadcast,
                 the send/double-close panics, and a graceful dispatcher
```

- Build: a task dispatcher that broadcasts shutdown to a worker pool by closing its channel.
- Implement: comma-ok receives that detect closure; a `close()` broadcast to many waiters; a `TaskDispatcher` whose workers exit cleanly when the task channel closes; recover guards around the send-on-closed and double-close panics.
- Verify: `go run main.go` for each step.

### Why close broadcasts and where it bites

`close()` solves the shutdown problem elegantly: closing a channel unblocks ALL
receivers simultaneously. Every worker blocking on `<-tasks` gets an immediate
zero-value response. This one-to-many broadcast is the standard way to signal
"no more work" in Go.

But `close()` has sharp edges. Sending on a closed channel panics. Closing an
already-closed channel panics. These are not bugs -- they are invariants that
protect you from data corruption. Understanding when reads return the zero
value, how the comma-ok idiom distinguishes "real zero" from "closed channel,"
and when to close (and when not to) separates confident Go programmers from
confused ones.

## Step 1 -- Zero-Value Reads After Close

When a task channel is closed, receives immediately return the zero value of the channel's type, forever. Workers must be able to detect this to know when to stop.

```go
package main

import "fmt"

const taskBufferSize = 3

// Task represents a unit of work with an identifier and name.
type Task struct {
	ID   int
	Name string
}

// demonstrateZeroValueReads shows that after a channel is closed and
// drained, every subsequent receive returns the zero value of the type.
func demonstrateZeroValueReads(tasks <-chan Task, readCount int) {
	for i := 1; i <= readCount; i++ {
		task := <-tasks
		label := "real task"
		if task == (Task{}) {
			label = "zero value -- channel closed and empty"
		}
		fmt.Printf("Read %d: %+v (%s)\n", i, task, label)
	}
}

func main() {
	tasks := make(chan Task, taskBufferSize)
	tasks <- Task{ID: 1, Name: "process-invoice"}
	tasks <- Task{ID: 2, Name: "send-email"}
	close(tasks)

	// First two reads return real tasks. After that, zero values forever.
	demonstrateZeroValueReads(tasks, 4)
}
```

After all buffered tasks are drained, every subsequent read returns `Task{ID: 0, Name: ""}`. For int channels, you get `0`. For string channels, `""`. For pointers, `nil`.

### Verification
```bash
go run main.go
# Expected:
#   Read 1: {ID:1 Name:process-invoice} (real task)
#   Read 2: {ID:2 Name:send-email} (real task)
#   Read 3: {ID:0 Name:} (zero value -- channel closed and empty)
#   Read 4: {ID:0 Name:} (zero value -- repeats forever)
```

## Step 2 -- The Comma-Ok Idiom: Distinguishing Real Data from Shutdown

A task with ID=0 could be a legitimate task or a zero value from a closed channel. The comma-ok idiom resolves this ambiguity.

```go
package main

import "fmt"

// receiveWithStatus uses the comma-ok idiom to distinguish real values
// from closed-channel zero values.
func receiveWithStatus(tasks <-chan int, label string) {
	id, ok := <-tasks
	status := "real task (happens to have ID 0)"
	if !ok {
		status = "channel closed, no more tasks"
	}
	fmt.Printf("id=%d, ok=%v -- %s [%s]\n", id, ok, status, label)
}

func main() {
	tasks := make(chan int, 2)
	tasks <- 0 // intentionally sending zero -- this is a real task ID
	close(tasks)

	receiveWithStatus(tasks, "first read")
	receiveWithStatus(tasks, "second read")
}
```

When `ok` is `false`, the channel is closed and drained -- the worker should stop. When `ok` is `true`, the value is a real task, even if it happens to be the zero value.

### Verification
```bash
go run main.go
# Expected:
#   id=0, ok=true -- real task (happens to have ID 0)
#   id=0, ok=false -- channel closed, no more tasks
```

## Step 3 -- Broadcasting Shutdown to Multiple Workers

Closing a channel unblocks ALL receivers simultaneously. This is the simplest way to tell a pool of workers "no more work." Sending on a channel would only wake ONE worker.

```go
package main

import (
	"fmt"
	"time"
)

const (
	numWorkers      = 5
	workerStartWait = 50 * time.Millisecond
)

// waitForShutdown blocks until the shutdown channel is closed,
// then signals completion on the done channel.
func waitForShutdown(id int, shutdown <-chan struct{}, done chan<- struct{}) {
	fmt.Printf("Worker %d: waiting for tasks...\n", id)
	<-shutdown
	fmt.Printf("Worker %d: received shutdown broadcast, cleaning up\n", id)
	done <- struct{}{}
}

// launchWorkerPool starts numWorkers goroutines, each waiting for shutdown.
func launchWorkerPool(count int, shutdown <-chan struct{}, done chan<- struct{}) {
	for id := 1; id <= count; id++ {
		go waitForShutdown(id, shutdown, done)
	}
}

// collectCompletions waits for exactly count workers to finish.
func collectCompletions(done <-chan struct{}, count int) {
	for i := 0; i < count; i++ {
		<-done
	}
}

func main() {
	shutdown := make(chan struct{})
	done := make(chan struct{})

	launchWorkerPool(numWorkers, shutdown, done)

	time.Sleep(workerStartWait)
	fmt.Printf("\nDispatcher: no more work -- broadcasting shutdown to %d workers...\n\n", numWorkers)
	close(shutdown) // ALL 5 workers unblock simultaneously

	collectCompletions(done, numWorkers)
	fmt.Println("Dispatcher: all workers shut down cleanly")
}
```

What if you used `shutdown <- struct{}{}` instead of `close(shutdown)`? Only ONE worker would receive the signal. You would need to send 5 times for 5 workers, and you would need to know exactly how many workers exist. `close()` is the one-to-many broadcast.

### Verification
```bash
go run main.go
# Expected: all 5 workers print their shutdown message
```

## Step 4 -- Panic: Send on Closed Channel and Double Close

These are the two sharp edges of close. Both result in unrecoverable panics.

```go
package main

import "fmt"

// safeExecute runs fn inside a deferred recover, printing any panic message.
func safeExecute(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("Caught panic: %v\n", r)
		}
	}()
	fn()
}

// demonstrateSendOnClosed triggers a panic by sending on a closed channel.
func demonstrateSendOnClosed() {
	tasks := make(chan int)
	close(tasks)
	tasks <- 42 // panic: send on closed channel
}

// demonstrateDoubleClose triggers a panic by closing an already-closed channel.
func demonstrateDoubleClose() {
	tasks := make(chan int)
	close(tasks)
	close(tasks) // panic: close of closed channel
}

func main() {
	safeExecute(demonstrateSendOnClosed)
	safeExecute(demonstrateDoubleClose)
	fmt.Println("Both panics caught and handled")
}
```

### Verification
```bash
go run main.go
# Expected:
#   Caught panic: send on closed channel
#   Caught panic: close of closed channel
#   Both panics caught and handled
```

## Step 5 -- Task Dispatcher with Graceful Shutdown

A realistic example combining everything: the dispatcher sends work items to workers through a buffered task channel. Workers use the comma-ok idiom to distinguish real tasks from shutdown. When work is done, the dispatcher closes the task channel, and all workers exit cleanly.

```go
package main

import (
	"fmt"
	"time"
)

const (
	taskBufferCapacity  = 5
	workerCount         = 3
	taskCount           = 10
	taskProcessDuration = 30 * time.Millisecond
)

// TaskDispatcher distributes work items to a pool of workers and
// coordinates graceful or emergency shutdown.
type TaskDispatcher struct {
	tasks chan string
	quit  chan struct{}
	done  chan struct{}
}

// NewTaskDispatcher creates a dispatcher with a buffered task channel.
func NewTaskDispatcher(bufferSize int) *TaskDispatcher {
	return &TaskDispatcher{
		tasks: make(chan string, bufferSize),
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// runWorker processes tasks until the task channel is closed or an
// emergency quit is signaled. It reports completion on the done channel.
func (d *TaskDispatcher) runWorker(id int) {
	defer func() { d.done <- struct{}{} }()
	for {
		select {
		case task, ok := <-d.tasks:
			if !ok {
				fmt.Printf("Worker %d: task channel closed, exiting\n", id)
				return
			}
			fmt.Printf("Worker %d: processing %s\n", id, task)
			time.Sleep(taskProcessDuration)
		case <-d.quit:
			fmt.Printf("Worker %d: emergency shutdown\n", id)
			return
		}
	}
}

// LaunchWorkers starts count worker goroutines.
func (d *TaskDispatcher) LaunchWorkers(count int) {
	for id := 1; id <= count; id++ {
		go d.runWorker(id)
	}
}

// SubmitTasks sends the given number of tasks, then closes the channel.
func (d *TaskDispatcher) SubmitTasks(count int) {
	for i := 1; i <= count; i++ {
		d.tasks <- fmt.Sprintf("task-%d", i)
	}
	close(d.tasks)
}

// WaitForWorkers blocks until all workers report completion.
func (d *TaskDispatcher) WaitForWorkers(count int) {
	for i := 0; i < count; i++ {
		<-d.done
	}
}

func main() {
	dispatcher := NewTaskDispatcher(taskBufferCapacity)
	dispatcher.LaunchWorkers(workerCount)
	dispatcher.SubmitTasks(taskCount)
	dispatcher.WaitForWorkers(workerCount)
	fmt.Println("Dispatcher: all workers finished")
}
```

### Verification
```bash
go run main.go
# Expected: workers process all 10 tasks, then each detects the closed channel and exits
```

## Verification

Run the programs and confirm:
1. Zero values are returned from closed channels indefinitely
2. The comma-ok idiom correctly distinguishes real data from closed-channel zero values
3. `close()` broadcasts to all blocked receivers simultaneously
4. Sending on a closed channel and double-closing both panic

## Common Mistakes

### Using Close as "I'm Done Receiving"

**Wrong:**
```go
// Consumer code:
task := <-tasks
close(tasks) // "I'm done reading"
```

**What happens:** If the dispatcher sends another task, it panics.

**Fix:** Only the sender closes the channel. The receiver just stops reading. If you need to signal the producer to stop, use a separate "quit" channel.

### No Built-In isOpen() Check

**Wrong approach:**
```go
if isOpen(ch) {
    ch <- value // race: channel might close between check and send
}
```

**What happens:** There is no `isOpen()` function in Go, and even if you check with comma-ok, the state can change between the check and your next operation.

**Fix:** Structure your code so that ownership is clear. The owner (sender) is the only one who closes. Use `select` for non-blocking operations.

## Review

Closing a channel is a one-way, permanent declaration that no more values are
coming, and everything else follows from that. Receives on a closed-and-drained
channel return the type's zero value immediately and forever, so a worker never
blocks waiting for work that will never arrive. The comma-ok form,
`val, ok := <-ch`, is what disambiguates a real zero from a closed channel: when
`ok` is false the channel is closed and empty. Because the signal is a state
change on the channel rather than a value handed to one receiver, `close`
reaches every blocked receiver at once -- that is the broadcast a plain send can
never be. The sharp edges are the flip side of the same rule: sending on a
closed channel and closing an already-closed channel both panic, which is why
closing is the sender's job alone and must happen exactly once.

Check yourself against three questions the steps answer directly. What does
`val, ok := <-ch` return when the channel is closed and empty, and why is that
different from receiving a legitimate zero value? Why does closing broadcast to
all receivers instead of waking just one, the way a send would? And why is
sending on a closed channel a programming error rather than a recoverable
condition -- what invariant would a post-close send violate?

## Resources
- [Go Spec: Close](https://go.dev/ref/spec#Close) -- the exact rules for `close`, including the panics on nil, closed, and receive-only channels.
- [Go Spec: Receive operator](https://go.dev/ref/spec#Receive_operator) -- how a receive behaves on a closed channel and what the comma-ok second value means.
- [Go FAQ: How do I know if a channel is closed?](https://go.dev/doc/faq#closechan) -- why there is no `isOpen` check and how to structure ownership so you never need one.

---

Back to [Concurrency](../../concurrency.md) | Next: [07-nil-channel-behavior](../07-nil-channel-behavior/07-nil-channel-behavior.md)
