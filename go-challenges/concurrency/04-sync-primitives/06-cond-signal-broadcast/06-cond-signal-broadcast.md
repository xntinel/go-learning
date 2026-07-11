# Exercise 6: Cond: Signal and Broadcast

`sync.Cond` is a condition variable -- a synchronization primitive that lets
goroutines wait until some shared condition becomes true. Channels solve many
signaling problems, but a condition variable earns its place when many goroutines
must wait on the same condition, when that condition depends on state guarded by
a mutex, or when you need to wake exactly one waiter versus all of them. The
running example is a bounded in-memory job queue: handlers enqueue, workers
dequeue, workers block when it is empty, producers block when it is full, and
shutdown must wake everyone at once.

## What you'll build

```text
06-cond-signal-broadcast/
  main.go        bounded job queue on sync.Cond: workers wait when empty,
                 producers wait when full, Broadcast wakes all on shutdown
```

- Build: a bounded producer-consumer job queue coordinated with `sync.Cond`.
- Implement: a `JobQueue`/`BoundedQueue` whose `Enqueue` and `Dequeue` `Wait` inside a `for` loop, `Signal` for one-at-a-time handoff, and a `WorkerPool` that uses `Broadcast` to release all workers on shutdown.
- Verify: `go run main.go`.

### Why a condition variable, not a channel

`sync.Cond` is a condition variable -- a synchronization primitive that allows goroutines to wait until a particular condition becomes true. While channels can solve many signaling problems, `sync.Cond` excels in specific scenarios:

1. **Multiple goroutines waiting for the same condition**: With channels, you need complex fan-out logic. With `Broadcast`, you wake all waiters in one call.
2. **Condition that must be checked under a lock**: The condition depends on shared state protected by a mutex. `Cond.Wait` atomically releases the lock and suspends the goroutine, then re-acquires the lock when woken.
3. **Fine-grained notification**: `Signal` wakes exactly one waiter, useful for work-stealing or producer-consumer where only one consumer should proceed.

A real use case: a bounded in-memory job queue. API handlers enqueue jobs, worker goroutines dequeue and process them. Workers wait when the queue is empty. Producers wait when the queue is full. On shutdown, all workers need to be notified simultaneously.

The critical pattern is **always Wait in a loop**:
```go
cond.L.Lock()
for !condition() {
    cond.Wait()
}
// condition is true, proceed while holding the lock
cond.L.Unlock()
```

Why a loop? Because after `Wait` returns, the condition might no longer be true -- another goroutine might have consumed the item between the signal and the wakeup. This is known as a spurious wakeup, and the loop re-checks the condition before proceeding.

## Step 1 -- Bounded Job Queue: Consumer Waits for Work

Build a job queue where worker goroutines wait for jobs to arrive. When a producer enqueues a job, it signals one waiting worker:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	queueCapacity     = 5
	jobCount          = 5
	workerProcessTime = 50 * time.Millisecond
	producerDelay     = 30 * time.Millisecond
	shutdownDelay     = 200 * time.Millisecond
)

type Job struct {
	ID      int
	Payload string
}

type JobQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	jobs     []Job
	maxSize  int
	shutdown bool
}

func NewJobQueue(maxSize int) *JobQueue {
	jq := &JobQueue{
		jobs:    make([]Job, 0, maxSize),
		maxSize: maxSize,
	}
	jq.cond = sync.NewCond(&jq.mu)
	return jq
}

func (jq *JobQueue) Dequeue() (Job, bool) {
	jq.cond.L.Lock()
	for len(jq.jobs) == 0 && !jq.shutdown {
		fmt.Println("Worker: queue empty, waiting...")
		jq.cond.Wait()
	}
	if len(jq.jobs) == 0 && jq.shutdown {
		jq.cond.L.Unlock()
		return Job{}, false
	}
	job := jq.jobs[0]
	jq.jobs = jq.jobs[1:]
	jq.cond.L.Unlock()
	jq.cond.Signal() // notify producer that space is available
	return job, true
}

func (jq *JobQueue) Enqueue(job Job) {
	jq.cond.L.Lock()
	jq.jobs = append(jq.jobs, job)
	fmt.Printf("Producer: enqueued job %d\n", job.ID)
	jq.cond.L.Unlock()
	jq.cond.Signal()
}

func (jq *JobQueue) Shutdown() {
	jq.cond.L.Lock()
	jq.shutdown = true
	jq.cond.L.Unlock()
	jq.cond.Signal()
}

func runWorker(queue *JobQueue, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		job, ok := queue.Dequeue()
		if !ok {
			fmt.Println("Worker: shutdown received, exiting.")
			return
		}
		fmt.Printf("Worker: processing job %d (%s)\n", job.ID, job.Payload)
		time.Sleep(workerProcessTime)
	}
}

func produceJobs(queue *JobQueue, count int) {
	for i := 1; i <= count; i++ {
		queue.Enqueue(Job{ID: i, Payload: fmt.Sprintf("task-%d", i)})
		time.Sleep(producerDelay)
	}
}

func main() {
	queue := NewJobQueue(queueCapacity)
	var wg sync.WaitGroup

	wg.Add(1)
	go runWorker(queue, &wg)

	produceJobs(queue, jobCount)

	time.Sleep(shutdownDelay)
	queue.Shutdown()
	wg.Wait()
	fmt.Println("All jobs processed.")
}
```

Expected output:
```
Worker: queue empty, waiting...
Producer: enqueued job 1
Worker: processing job 1 (task-1)
Producer: enqueued job 2
Worker: processing job 2 (task-2)
...
Worker: shutdown received, exiting.
All jobs processed.
```

### Verification
```bash
go run main.go
```
The worker should wait when the queue is empty, process jobs as they arrive, and exit cleanly on shutdown.

## Step 2 -- Producer Waits When Queue Is Full

In a bounded queue, producers must also wait when the queue is at capacity. This prevents unbounded memory growth:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

type Job struct {
	ID int
}

type BoundedQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	jobs     []Job
	maxSize  int
	shutdown bool
}

func NewBoundedQueue(maxSize int) *BoundedQueue {
	bq := &BoundedQueue{
		jobs:    make([]Job, 0, maxSize),
		maxSize: maxSize,
	}
	bq.cond = sync.NewCond(&bq.mu)
	return bq
}

func (bq *BoundedQueue) Enqueue(job Job) bool {
	bq.cond.L.Lock()
	defer bq.cond.L.Unlock()
	for len(bq.jobs) >= bq.maxSize && !bq.shutdown {
		fmt.Printf("  Producer: queue full (%d/%d), waiting...\n", len(bq.jobs), bq.maxSize)
		bq.cond.Wait()
	}
	if bq.shutdown {
		return false
	}
	bq.jobs = append(bq.jobs, job)
	bq.cond.Signal() // wake one consumer
	return true
}

func (bq *BoundedQueue) Dequeue() (Job, bool) {
	bq.cond.L.Lock()
	defer bq.cond.L.Unlock()
	for len(bq.jobs) == 0 && !bq.shutdown {
		bq.cond.Wait()
	}
	if len(bq.jobs) == 0 {
		return Job{}, false
	}
	job := bq.jobs[0]
	bq.jobs = bq.jobs[1:]
	bq.cond.Signal() // wake producer waiting for space
	return job, true
}

func main() {
	queue := NewBoundedQueue(3)
	var wg sync.WaitGroup

	// Start 1 slow consumer
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			job, ok := queue.Dequeue()
			if !ok {
				fmt.Println("Consumer: queue closed, exiting.")
				return
			}
			fmt.Printf("Consumer: processing job %d\n", job.ID)
			time.Sleep(100 * time.Millisecond) // slow consumer
		}
	}()

	// Fast producer: enqueue 8 jobs into a queue of capacity 3
	for i := 1; i <= 8; i++ {
		fmt.Printf("Producer: enqueuing job %d\n", i)
		queue.Enqueue(Job{ID: i})
	}

	// Wait for processing to finish
	time.Sleep(1 * time.Second)
	queue.cond.L.Lock()
	queue.shutdown = true
	queue.cond.L.Unlock()
	queue.cond.Broadcast()
	wg.Wait()
	fmt.Println("Done.")
}
```

Expected output:
```
Producer: enqueuing job 1
Producer: enqueuing job 2
Producer: enqueuing job 3
Producer: enqueuing job 4
  Producer: queue full (3/3), waiting...
Consumer: processing job 1
Producer: enqueuing job 5
  Producer: queue full (3/3), waiting...
Consumer: processing job 2
...
Done.
```

### Verification
```bash
go run main.go
```
Producer should block when the queue reaches capacity 3 and resume when the consumer makes space.

## Step 3 -- Broadcast: Graceful Shutdown of All Workers

In production, you run multiple workers and need to shut them all down simultaneously. `Broadcast` wakes all waiting goroutines at once:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	workerCount        = 4
	jobsToEnqueue      = 6
	workerProcessDelay = 40 * time.Millisecond
	workerStartDelay   = 50 * time.Millisecond
	processingWait     = 300 * time.Millisecond
	initialCapacity    = 10
)

type Job struct {
	ID int
}

type WorkerPool struct {
	mu       sync.Mutex
	cond     *sync.Cond
	jobs     []Job
	shutdown bool
}

func NewWorkerPool() *WorkerPool {
	wp := &WorkerPool{jobs: make([]Job, 0, initialCapacity)}
	wp.cond = sync.NewCond(&wp.mu)
	return wp
}

func (wp *WorkerPool) TakeJob() (Job, bool) {
	wp.cond.L.Lock()
	for len(wp.jobs) == 0 && !wp.shutdown {
		wp.cond.Wait()
	}
	if len(wp.jobs) == 0 && wp.shutdown {
		wp.cond.L.Unlock()
		return Job{}, false
	}
	job := wp.jobs[0]
	wp.jobs = wp.jobs[1:]
	wp.cond.L.Unlock()
	return job, true
}

func (wp *WorkerPool) SubmitBatch(jobs []Job) {
	wp.cond.L.Lock()
	wp.jobs = append(wp.jobs, jobs...)
	wp.cond.L.Unlock()
	wp.cond.Broadcast() // wake ALL workers to grab jobs
}

func (wp *WorkerPool) Shutdown() {
	wp.cond.L.Lock()
	wp.shutdown = true
	wp.cond.L.Unlock()
	wp.cond.Broadcast() // wake ALL workers so they can exit
}

func (wp *WorkerPool) RunWorker(workerID int, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		job, ok := wp.TakeJob()
		if !ok {
			fmt.Printf("  Worker %d: shutdown, exiting.\n", workerID)
			return
		}
		fmt.Printf("  Worker %d: processing job %d\n", workerID, job.ID)
		time.Sleep(workerProcessDelay)
	}
}

func startWorkers(pool *WorkerPool, count int, wg *sync.WaitGroup) {
	for i := 0; i < count; i++ {
		wg.Add(1)
		go pool.RunWorker(i, wg)
	}
}

func buildJobBatch(count int) []Job {
	jobs := make([]Job, count)
	for i := range jobs {
		jobs[i] = Job{ID: i + 1}
	}
	return jobs
}

func main() {
	pool := NewWorkerPool()
	var wg sync.WaitGroup

	fmt.Printf("Starting %d workers...\n", workerCount)
	startWorkers(pool, workerCount, &wg)

	time.Sleep(workerStartDelay)

	fmt.Printf("\nEnqueuing %d jobs...\n", jobsToEnqueue)
	pool.SubmitBatch(buildJobBatch(jobsToEnqueue))

	time.Sleep(processingWait)

	fmt.Println("\nShutting down all workers...")
	pool.Shutdown()

	wg.Wait()
	fmt.Println("\nAll workers stopped. Clean shutdown complete.")
}
```

Expected output:
```
Starting 4 workers...

Enqueuing 6 jobs...
  Worker 0: processing job 1
  Worker 1: processing job 2
  Worker 2: processing job 3
  Worker 3: processing job 4
  Worker 0: processing job 5
  Worker 1: processing job 6

Shutting down all workers...
  Worker 2: shutdown, exiting.
  Worker 3: shutdown, exiting.
  Worker 0: shutdown, exiting.
  Worker 1: shutdown, exiting.

All workers stopped. Clean shutdown complete.
```

### Verification
```bash
go run main.go
```
All 4 workers should process jobs, then all exit cleanly after the shutdown broadcast.

## Step 4 -- Wait-in-Loop: Why FOR, Not IF

Two consumers compete for jobs from the same queue. The loop ensures correctness when one consumer grabs the job before the other wakes up:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	competingConsumers = 2
	jobsPerConsumer    = 3
	totalJobs          = competingConsumers * jobsPerConsumer
	producerInterval   = 30 * time.Millisecond
)

type JobCounter struct {
	mu    sync.Mutex
	cond  *sync.Cond
	count int
}

func NewJobCounter() *JobCounter {
	jc := &JobCounter{}
	jc.cond = sync.NewCond(&jc.mu)
	return jc
}

func (jc *JobCounter) WaitAndTake() int {
	jc.cond.L.Lock()
	for jc.count == 0 { // FOR, not IF -- re-check after wakeup
		jc.cond.Wait()
	}
	jc.count--
	remaining := jc.count
	jc.cond.L.Unlock()
	return remaining
}

func (jc *JobCounter) Add() int {
	jc.cond.L.Lock()
	jc.count++
	current := jc.count
	jc.cond.L.Unlock()
	jc.cond.Signal()
	return current
}

func runCompetingConsumer(counter *JobCounter, workerID, jobsToConsume int, wg *sync.WaitGroup) {
	defer wg.Done()
	for j := 0; j < jobsToConsume; j++ {
		remaining := counter.WaitAndTake()
		fmt.Printf("Worker %d: took job (remaining: %d)\n", workerID, remaining)
	}
}

func produceOneAtATime(counter *JobCounter, count int) {
	for i := 0; i < count; i++ {
		time.Sleep(producerInterval)
		current := counter.Add()
		fmt.Printf("Producer: added job (count: %d)\n", current)
	}
}

func main() {
	counter := NewJobCounter()
	var wg sync.WaitGroup

	for i := 0; i < competingConsumers; i++ {
		wg.Add(1)
		go runCompetingConsumer(counter, i, jobsPerConsumer, &wg)
	}

	produceOneAtATime(counter, totalJobs)

	wg.Wait()
	fmt.Printf("Both workers processed %d jobs each.\n", jobsPerConsumer)
}
```

Expected output:
```
Producer: added job (count: 1)
Worker 0: took job (remaining: 0)
Producer: added job (count: 1)
Worker 1: took job (remaining: 0)
...
Both workers processed 3 jobs each.
```

If you used `if` instead of `for`, a consumer might wake up and find `jobCount == 0` because the other consumer already took the job. The `for` loop re-checks and goes back to sleep.

### Verification
```bash
go run main.go
```
Both consumers should each consume exactly 3 items without panicking.

## Common Mistakes

### Wait Without Holding the Lock

```go
cond.Wait() // panic: sync: unlock of unlocked mutex
```

**What happens:** `Wait` calls `L.Unlock()` internally. If the lock is not held, it panics.

**Fix:** Always acquire `cond.L.Lock()` before calling `Wait`.

### Using if Instead of for

```go
cond.L.Lock()
if len(jobs) == 0 { // NOT safe -- another consumer may grab the job first
    cond.Wait()
}
job := jobs[0] // might panic: index out of range
```

**Fix:** Always use `for`:
```go
for len(jobs) == 0 {
    cond.Wait()
}
```

### Signal Without Changing the Condition

```go
cond.Signal() // wake a waiter, but the condition has not changed
```

**What happens:** The waiter wakes up, re-checks the condition in the loop, finds it still false, and goes back to sleep. Not a bug, but a wasted wakeup that burns CPU cycles.

### Broadcast When Signal Suffices
Using `Broadcast` when only one goroutine should proceed causes a thundering herd: all waiters wake up, re-check the condition, and all but one go back to sleep. Use `Signal` for single-consumer patterns and `Broadcast` only for shutdown or barrier-style notifications.

## Review

`sync.Cond` lets a goroutine sleep until a condition over shared state becomes
true. `Wait` is the primitive that makes it work: it atomically releases the
associated mutex and suspends the goroutine, then re-acquires the lock before
returning, so the condition is always re-checked while the lock is held. That
re-check must sit in a `for` loop, never an `if`, because a woken goroutine can
find the condition already false again -- another consumer grabbed the job
between the signal and the wakeup. `Signal` wakes exactly one waiter and fits
producer-consumer handoff where one job should go to one worker; `Broadcast`
wakes all of them and is the right tool for shutdown or barrier-style
notifications. The bounded queue leans on both directions of `Signal`: a
consumer signals a producer that space opened, a producer signals a consumer
that a job arrived. For simple one-to-one communication, a channel is still the
cleaner choice.

To cement it, build a rate-limited job queue on `sync.Cond` that accepts jobs
from several producers, dispatches to several consumers, and caps in-flight work
at N: use `Signal` to wake a single consumer when a job lands and `Broadcast` to
release everyone on shutdown. Exercise it with three producers, five consumers, a
concurrency limit of two, and twenty jobs, and confirm no consumer ever proceeds
on a false condition and no goroutine is left blocked after the broadcast.

## Resources
- [sync.Cond documentation](https://pkg.go.dev/sync#Cond) -- Wait/Signal/Broadcast and the locking contract they require.
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) -- the channel-first philosophy that Cond deliberately steps outside of.
- [Bryan Mills - Rethinking Classical Concurrency Patterns](https://www.youtube.com/watch?v=5zXAHh5tJqQ) -- when condition variables are worth it and when channels win.

---

Back to [Concurrency](../../concurrency.md) | Next: [07-mutex-vs-channel-decision](../07-mutex-vs-channel-decision/07-mutex-vs-channel-decision.md)
