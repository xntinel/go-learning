# Exercise 7: Nil Channel Behavior

A system that merges sorted data from multiple sources -- a database query
stream and a cache stream, say -- has to cope with each stream finishing at a
different time. When the cache stream is exhausted you must stop reading it
while continuing on the database stream until it too finishes. A nil channel
turns that bookkeeping into a single assignment: set the exhausted channel's
variable to nil and `select` simply stops considering it. What looks like a
degenerate value is actually the cleanest way to disable a communication.

## What you'll build

```text
07-nil-channel-behavior/
  main.go        proofs that nil channels block, then mergers that disable
                 exhausted sources by nil-ing them, and a pausable worker
```

- Build: a family of stream mergers that drop exhausted sources by nil-ing their channel variable, plus a worker that pauses and resumes the same way.
- Implement: `mergeSorted` over two streams, `mergeThreeSources` with dynamic disabling, and a `PausableWorker` whose `Run` toggles its task channel between the real channel and nil.
- Verify: `go run main.go`

### Why a nil channel is a feature, not a bug

Without nil channels, you would need complex boolean flags, nested
if-statements, and careful coordination to track which sources are still live.
With nil channels, when one source closes, you set its variable to nil. The
`select` naturally stops considering that case. The code is cleaner, shorter,
and harder to get wrong.

This pattern appears in production code for merging event streams, implementing
timeouts that can be canceled, and building state machines where available
operations change over time.

## Channel State Summary

| State | Send | Receive | Close |
|-------|------|---------|-------|
| nil | Block forever | Block forever | panic |
| open, empty | Block (if unbuffered or full) | Block | OK |
| open, has data | Send or block | Receive value | OK |
| closed | panic | Zero value (ok=false) | panic |

## Step 1 -- Nil Channel Blocks Forever

Demonstrate that a nil channel blocks on both send and receive. This is not a bug -- it is a property we will exploit in `select`.

```go
package main

import (
	"fmt"
	"time"
)

const nilProbeTimeout = 200 * time.Millisecond

// proveReceiveBlocks shows that receiving from a nil channel blocks forever,
// using a select with a timeout to avoid a deadlock.
func proveReceiveBlocks(ch <-chan int, timeout time.Duration) {
	select {
	case val := <-ch:
		fmt.Println("received:", val) // never happens
	case <-time.After(timeout):
		fmt.Println("receive on nil channel: blocked (timed out as expected)")
	}
}

// proveSendBlocks shows that sending on a nil channel blocks forever.
func proveSendBlocks(ch chan<- int, timeout time.Duration) {
	select {
	case ch <- 42:
		fmt.Println("sent") // never happens
	case <-time.After(timeout):
		fmt.Println("send on nil channel: blocked (timed out as expected)")
	}
}

func main() {
	var ch chan int // nil -- not initialized with make()
	proveReceiveBlocks(ch, nilProbeTimeout)
	proveSendBlocks(ch, nilProbeTimeout)
}
```

### Verification
```bash
go run main.go
# Expected:
#   receive on nil channel: blocked (timed out as expected)
#   send on nil channel: blocked (timed out as expected)
```

Without the `select` + timeout, `<-ch` on a nil channel would deadlock (or block the goroutine forever if other goroutines exist).

## Step 2 -- Nil Channel in Select Is Skipped

When a channel variable is nil, its `select` case is never chosen -- as if it does not exist. This is the key insight that makes nil channels useful.

```go
package main

import "fmt"

// selectFromStreams demonstrates that nil channels are skipped in select.
// Only non-nil channels with data are eligible for selection.
func selectFromStreams(dbStream <-chan string, cacheStream <-chan string) {
	select {
	case val := <-dbStream:
		fmt.Println("from database:", val) // never chosen -- dbStream is nil
	case val := <-cacheStream:
		fmt.Println("from cache:", val) // always chosen
	}
}

func main() {
	var dbStream chan string // nil -- this case will be skipped
	cacheStream := make(chan string, 1)
	cacheStream <- "user:42:cached"

	selectFromStreams(dbStream, cacheStream)
}
```

You can dynamically control which select cases are active by assigning channel variables to nil or to a real channel.

### Verification
```bash
go run main.go
# Expected: from cache: user:42:cached
```

## Step 3 -- Merging Sorted Streams

The core pattern: merge values from two sorted streams until both are exhausted. When one stream closes, set it to nil so `select` stops trying to read from it. The other stream continues until it also closes.

```go
package main

import "fmt"

// sortedStream sends each value in order and closes the channel.
func sortedStream(values []int) <-chan int {
	ch := make(chan int)
	go func() {
		for _, v := range values {
			ch <- v
		}
		close(ch)
	}()
	return ch
}

// mergeSorted merges two channels into one. When a source closes, it is
// set to nil so select stops considering it. The loop exits when both are nil.
func mergeSorted(streamA, streamB <-chan int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for streamA != nil || streamB != nil {
			select {
			case val, ok := <-streamA:
				if !ok {
					fmt.Println("  [merge] stream A exhausted -- disabling")
					streamA = nil
					continue
				}
				out <- val
			case val, ok := <-streamB:
				if !ok {
					fmt.Println("  [merge] stream B exhausted -- disabling")
					streamB = nil
					continue
				}
				out <- val
			}
		}
	}()
	return out
}

// drainAndCount receives all values from merged, prints them, and returns the count.
func drainAndCount(merged <-chan int) int {
	count := 0
	for val := range merged {
		fmt.Printf("  %d\n", val)
		count++
	}
	return count
}

func main() {
	streamA := sortedStream([]int{10, 30, 50})
	streamB := sortedStream([]int{20, 40, 60, 80, 100})

	fmt.Println("Merged output:")
	merged := mergeSorted(streamA, streamB)
	count := drainAndCount(merged)
	fmt.Printf("Merge complete: %d values from both streams\n", count)
}
```

When `a` is closed, we set `a = nil`. The next iteration still enters `select`, but `case <-a` is skipped because `a` is nil. Only `case <-b` is considered. When both are nil, the loop exits.

### Verification
```bash
go run main.go
# Expected: all 8 values from both streams (interleaved order may vary),
# then "Merge complete: 8 values from both streams"
```

## Step 4 -- Merging Three Data Sources with Dynamic Disabling

Extend the pattern to merge three sources -- a scenario you encounter when aggregating data from multiple microservices. Each service responds at a different speed and produces a different number of results.

```go
package main

import (
	"fmt"
	"time"
)

// Event represents a message from one of the microservices.
type Event struct {
	Source string
	Data   string
}

// eventStream produces events at the given rate, then closes the channel.
func eventStream(source string, events []string, delay time.Duration) <-chan Event {
	ch := make(chan Event)
	go func() {
		for _, data := range events {
			time.Sleep(delay)
			ch <- Event{Source: source, Data: data}
		}
		close(ch)
	}()
	return ch
}

// allSourcesExhausted returns true when every channel in the slice is nil.
func allSourcesExhausted(sources []<-chan Event) bool {
	for _, ch := range sources {
		if ch != nil {
			return false
		}
	}
	return true
}

// mergeThreeSources merges exactly three event channels using nil-disabling.
// For 3 known sources, explicit select is clearer than reflect-based approaches.
func mergeThreeSources(sources [3]<-chan Event) <-chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		active := sources[:]
		slice := make([]<-chan Event, len(active))
		copy(slice, active[:])

		for !allSourcesExhausted(slice) {
			select {
			case ev, ok := <-slice[0]:
				if !ok {
					slice[0] = nil
					continue
				}
				out <- ev
			case ev, ok := <-slice[1]:
				if !ok {
					slice[1] = nil
					continue
				}
				out <- ev
			case ev, ok := <-slice[2]:
				if !ok {
					slice[2] = nil
					continue
				}
				out <- ev
			}
		}
	}()
	return out
}

func main() {
	userSvc := eventStream("users", []string{"user-created", "user-updated"}, 30*time.Millisecond)
	orderSvc := eventStream("orders", []string{"order-placed", "order-shipped", "order-delivered"}, 20*time.Millisecond)
	paymentSvc := eventStream("payments", []string{"payment-received"}, 50*time.Millisecond)

	sources := [3]<-chan Event{userSvc, orderSvc, paymentSvc}
	merged := mergeThreeSources(sources)

	fmt.Println("Aggregating events from 3 services:")
	for ev := range merged {
		fmt.Printf("  [%-8s] %s\n", ev.Source, ev.Data)
	}
	fmt.Println("All services exhausted")
}
```

### Verification
```bash
go run main.go
# Expected: all 6 events from 3 services, interleaved by timing, then "All services exhausted"
```

## Step 5 -- Dynamic Enable/Disable: Pausable Worker

Use nil channels to model a worker with pause/resume capabilities. When paused, the task channel variable is set to nil, disabling task processing in the select. When resumed, it is restored.

```go
package main

import (
	"fmt"
	"time"
)

const (
	taskQueueSize        = 10
	totalDeployTasks     = 8
	taskExecDuration     = 40 * time.Millisecond
	preMaintenanceDelay  = 150 * time.Millisecond
	maintenanceDuration  = 200 * time.Millisecond
	postResumeDelay      = 250 * time.Millisecond
)

// PausableWorker processes tasks from a channel and can be paused/resumed
// at runtime by setting its active channel to nil or restoring it.
type PausableWorker struct {
	tasks    chan string
	pauseCh  chan struct{}
	resumeCh chan struct{}
	done     chan struct{}
}

// NewPausableWorker creates a worker with the given task queue size.
func NewPausableWorker(queueSize int) *PausableWorker {
	return &PausableWorker{
		tasks:    make(chan string, queueSize),
		pauseCh:  make(chan struct{}),
		resumeCh: make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Run processes tasks until the channel is closed. Pause/resume signals
// toggle the active channel between the real channel and nil.
func (pw *PausableWorker) Run() {
	active := pw.tasks
	for {
		select {
		case task, ok := <-active:
			if !ok {
				fmt.Println("Worker: all tasks complete, exiting")
				pw.done <- struct{}{}
				return
			}
			fmt.Println("Worker: executing", task)
			time.Sleep(taskExecDuration)
		case <-pw.pauseCh:
			fmt.Println("Worker: PAUSED (maintenance window)")
			active = nil
		case <-pw.resumeCh:
			fmt.Println("Worker: RESUMED")
			active = pw.tasks
		}
	}
}

// Pause signals the worker to stop processing tasks.
func (pw *PausableWorker) Pause()  { pw.pauseCh <- struct{}{} }

// Resume signals the worker to continue processing.
func (pw *PausableWorker) Resume() { pw.resumeCh <- struct{}{} }

// Shutdown closes the task channel and waits for the worker to exit.
func (pw *PausableWorker) Shutdown() {
	close(pw.tasks)
	<-pw.done
}

func main() {
	worker := NewPausableWorker(taskQueueSize)

	for i := 1; i <= totalDeployTasks; i++ {
		worker.tasks <- fmt.Sprintf("deploy-service-%d", i)
	}

	go worker.Run()

	time.Sleep(preMaintenanceDelay)
	worker.Pause()
	fmt.Println("Main: pause sent -- simulating maintenance window")

	time.Sleep(maintenanceDuration)
	worker.Resume()
	fmt.Println("Main: resume sent -- maintenance complete")

	time.Sleep(postResumeDelay)
	worker.Shutdown()
}
```

### Verification
```bash
go run main.go
# Expected: worker processes some tasks, pauses, resumes, processes remaining, exits
```

## Verification

Run the programs and confirm:
1. Nil channels block forever on send and receive
2. Nil channels are skipped in select statements
3. Setting a channel to nil after close prevents select from spinning on zero values
4. The merge pattern correctly handles sources that close at different times

## Common Mistakes

### Forgetting That var Declares a Nil Channel

**Wrong:**
```go
package main

func main() {
    var results chan int
    go func() {
        results <- 42 // blocks forever -- results is nil!
    }()
    <-results // also blocks forever
}
```

**What happens:** Both goroutines block permanently on the nil channel. Deadlock.

**Fix:** Always use `make(chan int)` to create a usable channel.

### Not Checking All Channels Are Nil Before Exiting

**Wrong:**
```go
for {
    select {
    case val, ok := <-a:
        if !ok { return } // exits when a closes, losing remaining b values!
    case val, ok := <-b:
        if !ok { return }
    }
}
```

**What happens:** When `a` closes, you return immediately, losing all remaining values in `b`.

**Correct:** Set to nil instead of returning. Only exit when both are nil:
```go
for a != nil || b != nil {
    select {
    case val, ok := <-a:
        if !ok { a = nil; continue }
        process(val)
    case val, ok := <-b:
        if !ok { b = nil; continue }
        process(val)
    }
}
```

## Review

A nil channel has one behavior worth memorizing: it blocks forever on both send
and receive, which means that inside a `select` its case can never be chosen and
is, in effect, switched off. That single fact powers the whole exercise. When a
source closes, a receive on it stops blocking and instead yields the zero value
with `ok == false` on every iteration -- a busy spin if you leave it in the
select. Assigning the channel variable to nil converts that spinning case back
into a dormant one, so the loop keeps servicing the sources that remain and
exits only once every channel has gone nil. The same toggle drives the pausable
worker: swap the task channel for nil to pause, restore it to resume. The one
rule you must not forget is that a bare `var ch chan T` is already nil, so any
channel you intend to use must come from `make`.

Test yourself on three questions before moving on. What happens when you read
from a nil channel outside of a select -- and why is that a deadlock rather than
a zero value? Why is nil-ing a closed channel better than re-checking `ok` on
every pass through the loop? And how would you merge ten channels with this
pattern without writing ten `select` cases -- what changes when the number of
sources is dynamic rather than fixed at three?

## Resources
- [Go Spec: Channel types (nil behavior)](https://go.dev/ref/spec#Channel_types) -- the language rule that a nil channel blocks forever on send and receive.
- [Go Spec: Select statements](https://go.dev/ref/spec#Select_statements) -- why a nil-channel case is never ready and never selected.
- [Dave Cheney: Channel Axioms](https://dave.cheney.net/2014/03/19/channel-axioms) -- the short list of channel rules this pattern relies on, nil behavior included.

---

Back to [Concurrency](../../concurrency.md) | Next: [08-channel-of-channels](../08-channel-of-channels/08-channel-of-channels.md)
