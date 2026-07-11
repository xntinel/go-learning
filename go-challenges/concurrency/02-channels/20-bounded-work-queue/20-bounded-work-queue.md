# Exercise 20: Bounded Work Queue

An API server receives bursts of requests, and each one spawns a background job
-- sending an email, resizing an image, generating a report. Accept every job
without a limit and a traffic spike accumulates unbounded work: memory grows,
latency climbs, and the process eventually crashes. A bounded work queue draws
the line at capacity N -- a submission succeeds instantly if there is room and
fails immediately if there is not, so no goroutine ever blocks waiting for
space and the caller always gets a definite answer it can turn into an HTTP 503
or a retry.

## What you'll build

```text
20-bounded-work-queue/
  main.go        a WorkQueue over a fixed-capacity buffered channel, then
                 overfill, drain-and-retry, and a 50-producer load test
```

- Build: a bounded `WorkQueue` backed by a fixed-capacity buffered channel that rejects rather than blocks.
- Implement: `Submit` using the try-send idiom, `Process`/`Drain` consumers over `range`, and a 50-producer, 3-consumer load test that tracks acceptance versus rejection rates.
- Verify: `go run main.go`, and `go run -race main.go` for the load test.

### Why a hard limit beats unbounded acceptance

A bounded work queue sets a hard limit. The queue has capacity N. Submitting a
job succeeds instantly if there is room, or fails immediately if the queue is
full. The caller gets a clear "queue full" error and can respond to the client
with HTTP 503 or retry logic. No goroutine blocks waiting for space.

The mechanism is a buffered channel combined with the try-send pattern:
`select { case ch <- job: default: }`. The `select` with a `default` branch
attempts the send and falls through immediately if the channel buffer is full.

> **Note:** This exercise introduces the try-send pattern as a channel capacity probe. The `select` statement is covered in depth in [section 03-select-and-multiplexing](../../03-select-and-multiplexing/01-select-basics/01-select-basics.md). For now, treat `select { case ch <- v: default: }` as a single idiom meaning "send if possible, otherwise do nothing."

## Step 1 -- Basic Queue: Submit Within Capacity

Create a `WorkQueue` backed by a buffered channel. Submit jobs and process them. When the queue has capacity, submissions succeed.

```go
package main

import (
	"errors"
	"fmt"
)

const queueCapacity = 5

// ErrQueueFull is returned when the work queue cannot accept more jobs.
var ErrQueueFull = errors.New("queue is full")

// Job represents a unit of work with an ID and a description.
type Job struct {
	ID          int
	Description string
}

// WorkQueue is a bounded job queue backed by a buffered channel.
type WorkQueue struct {
	jobs chan Job
}

// NewWorkQueue creates a queue with the given maximum capacity.
func NewWorkQueue(capacity int) *WorkQueue {
	return &WorkQueue{
		jobs: make(chan Job, capacity),
	}
}

// Submit attempts to enqueue a job without blocking.
// Returns ErrQueueFull if the queue is at capacity.
func (q *WorkQueue) Submit(job Job) error {
	select {
	case q.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

// Size returns the number of jobs currently in the queue.
func (q *WorkQueue) Size() int {
	return len(q.jobs)
}

// Process removes and returns the next job from the queue.
// Blocks until a job is available.
func (q *WorkQueue) Process() Job {
	return <-q.jobs
}

func main() {
	queue := NewWorkQueue(queueCapacity)

	jobs := []Job{
		{ID: 1, Description: "send-welcome-email"},
		{ID: 2, Description: "resize-avatar"},
		{ID: 3, Description: "generate-invoice"},
	}

	fmt.Printf("Queue capacity: %d\n\n", queueCapacity)

	for _, job := range jobs {
		if err := queue.Submit(job); err != nil {
			fmt.Printf("REJECTED job %d: %v\n", job.ID, err)
		} else {
			fmt.Printf("ACCEPTED job %d (%s) -- queue size: %d\n",
				job.ID, job.Description, queue.Size())
		}
	}

	fmt.Printf("\nProcessing %d jobs:\n", queue.Size())
	for i := 0; i < len(jobs); i++ {
		job := queue.Process()
		fmt.Printf("  processed: job %d (%s)\n", job.ID, job.Description)
	}
}
```

Key observations:
- `select { case q.jobs <- job: default: }` is the try-send idiom -- non-blocking
- `Submit` returns immediately whether the job fits or not
- `Process` blocks until a job is available (standard channel receive)
- `Size` uses `len(ch)` to check the current buffer occupancy

### Verification
```bash
go run main.go
# Expected: all 3 jobs accepted (capacity is 5), then all 3 processed
```

## Step 2 -- Overfill: Rejection Under Pressure

Submit more jobs than the queue can hold. The first N succeed, the rest are rejected immediately. No goroutine blocks.

```go
package main

import (
	"errors"
	"fmt"
)

const smallQueueCapacity = 3

var ErrQueueFull = errors.New("queue is full")

type Job struct {
	ID          int
	Description string
}

type WorkQueue struct {
	jobs chan Job
}

func NewWorkQueue(capacity int) *WorkQueue {
	return &WorkQueue{jobs: make(chan Job, capacity)}
}

func (q *WorkQueue) Submit(job Job) error {
	select {
	case q.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

func (q *WorkQueue) Size() int { return len(q.jobs) }

func main() {
	queue := NewWorkQueue(smallQueueCapacity)

	accepted := 0
	rejected := 0

	for i := 1; i <= 7; i++ {
		job := Job{ID: i, Description: fmt.Sprintf("task-%d", i)}
		if err := queue.Submit(job); err != nil {
			rejected++
			fmt.Printf("  REJECTED job %d: %v\n", job.ID, err)
		} else {
			accepted++
			fmt.Printf("  ACCEPTED job %d -- queue size: %d/%d\n",
				job.ID, queue.Size(), smallQueueCapacity)
		}
	}

	fmt.Printf("\nResults: %d accepted, %d rejected (capacity: %d)\n",
		accepted, rejected, smallQueueCapacity)
}
```

The first 3 jobs fill the buffer. Jobs 4 through 7 hit the `default` branch and return `ErrQueueFull` instantly. The main goroutine never blocks.

### Verification
```bash
go run main.go
# Expected: jobs 1-3 accepted, jobs 4-7 rejected
# Results: 3 accepted, 4 rejected (capacity: 3)
```

## Step 3 -- Consumer Drains Queue, Enabling New Submissions

Add a consumer goroutine that processes jobs. As it drains the queue, previously rejected jobs can now be accepted. This demonstrates the dynamic nature of channel capacity.

```go
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	drainQueueCapacity = 3
	processDelay       = 100 * time.Millisecond
	submissionPause    = 250 * time.Millisecond
)

var ErrQueueFull = errors.New("queue is full")

type Job struct {
	ID          int
	Description string
}

type WorkQueue struct {
	jobs chan Job
}

func NewWorkQueue(capacity int) *WorkQueue {
	return &WorkQueue{jobs: make(chan Job, capacity)}
}

func (q *WorkQueue) Submit(job Job) error {
	select {
	case q.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

func (q *WorkQueue) Size() int { return len(q.jobs) }

// Drain processes all available jobs until the queue is closed.
func (q *WorkQueue) Drain(workerID int, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range q.jobs {
		fmt.Printf("  [worker-%d] processing job %d (%s)\n",
			workerID, job.ID, job.Description)
		time.Sleep(processDelay)
	}
}

// Close signals no more jobs will be submitted.
func (q *WorkQueue) Close() {
	close(q.jobs)
}

func main() {
	queue := NewWorkQueue(drainQueueCapacity)
	var wg sync.WaitGroup

	// Fill the queue completely.
	fmt.Println("=== Phase 1: Fill queue ===")
	for i := 1; i <= 3; i++ {
		job := Job{ID: i, Description: fmt.Sprintf("task-%d", i)}
		_ = queue.Submit(job)
		fmt.Printf("  submitted job %d -- queue: %d/%d\n", i, queue.Size(), drainQueueCapacity)
	}

	// Attempt to submit when full.
	overflow := Job{ID: 4, Description: "task-4"}
	if err := queue.Submit(overflow); err != nil {
		fmt.Printf("  job 4 rejected: %v\n", err)
	}

	// Start consumer to drain.
	fmt.Println()
	fmt.Println("=== Phase 2: Start consumer ===")
	wg.Add(1)
	go queue.Drain(1, &wg)

	// Wait for consumer to create space.
	time.Sleep(submissionPause)

	// Retry previously rejected job.
	fmt.Println()
	fmt.Println("=== Phase 3: Retry after drain ===")
	if err := queue.Submit(overflow); err != nil {
		fmt.Printf("  job 4 still rejected: %v\n", err)
	} else {
		fmt.Printf("  job 4 NOW ACCEPTED -- queue: %d/%d\n",
			queue.Size(), drainQueueCapacity)
	}

	// Submit a few more.
	for i := 5; i <= 6; i++ {
		job := Job{ID: i, Description: fmt.Sprintf("task-%d", i)}
		if err := queue.Submit(job); err != nil {
			fmt.Printf("  job %d rejected: %v\n", i, err)
		} else {
			fmt.Printf("  job %d accepted -- queue: %d/%d\n",
				i, queue.Size(), drainQueueCapacity)
		}
	}

	queue.Close()
	wg.Wait()
	fmt.Println()
	fmt.Println("All jobs processed")
}
```

### Verification
```bash
go run main.go
# Expected:
# Phase 1: jobs 1-3 accepted, job 4 rejected
# Phase 2: consumer starts processing
# Phase 3: job 4 accepted after consumer created space
```

## Step 4 -- 50 Producers, 3 Consumers: Acceptance vs Rejection Rates

Simulate a realistic load: 50 concurrent producers submit jobs to a queue with capacity 10. Three consumer goroutines drain the queue. Track and print acceptance and rejection rates.

```go
package main

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	loadQueueCapacity = 10
	producerCount     = 50
	consumerCount     = 3
	consumerDelay     = 20 * time.Millisecond
)

var ErrQueueFull = errors.New("queue is full")

type Job struct {
	ID         int
	ProducerID int
}

type WorkQueue struct {
	jobs chan Job
}

func NewWorkQueue(capacity int) *WorkQueue {
	return &WorkQueue{jobs: make(chan Job, capacity)}
}

func (q *WorkQueue) Submit(job Job) error {
	select {
	case q.jobs <- job:
		return nil
	default:
		return ErrQueueFull
	}
}

func (q *WorkQueue) Close() { close(q.jobs) }

// Stats tracks submission outcomes using atomics for safe concurrent access.
type Stats struct {
	accepted atomic.Int64
	rejected atomic.Int64
}

func (s *Stats) RecordAccepted() { s.accepted.Add(1) }
func (s *Stats) RecordRejected() { s.rejected.Add(1) }

func producer(id int, queue *WorkQueue, stats *Stats, wg *sync.WaitGroup) {
	defer wg.Done()
	job := Job{ID: id, ProducerID: id}
	if err := queue.Submit(job); err != nil {
		stats.RecordRejected()
	} else {
		stats.RecordAccepted()
	}
}

func consumer(id int, queue *WorkQueue, processed *atomic.Int64, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range queue.jobs {
		_ = job // simulate processing
		time.Sleep(consumerDelay)
		processed.Add(1)
	}
}

func main() {
	queue := NewWorkQueue(loadQueueCapacity)
	stats := &Stats{}
	var processed atomic.Int64

	var consumerWG sync.WaitGroup
	for i := 1; i <= consumerCount; i++ {
		consumerWG.Add(1)
		go consumer(i, queue, &processed, &consumerWG)
	}

	var producerWG sync.WaitGroup
	start := time.Now()
	for i := 1; i <= producerCount; i++ {
		producerWG.Add(1)
		go producer(i, queue, stats, &producerWG)
	}

	producerWG.Wait()
	submitDuration := time.Since(start).Round(time.Millisecond)

	queue.Close()
	consumerWG.Wait()
	totalDuration := time.Since(start).Round(time.Millisecond)

	accepted := stats.accepted.Load()
	rejected := stats.rejected.Load()
	total := accepted + rejected

	fmt.Println("=== Bounded Work Queue Load Test ===")
	fmt.Printf("Queue capacity:   %d\n", loadQueueCapacity)
	fmt.Printf("Producers:        %d\n", producerCount)
	fmt.Printf("Consumers:        %d\n", consumerCount)
	fmt.Println()
	fmt.Printf("Submitted:        %d jobs\n", total)
	fmt.Printf("Accepted:         %d (%.1f%%)\n", accepted, float64(accepted)/float64(total)*100)
	fmt.Printf("Rejected:         %d (%.1f%%)\n", rejected, float64(rejected)/float64(total)*100)
	fmt.Printf("Processed:        %d\n", processed.Load())
	fmt.Println()
	fmt.Printf("Submit phase:     %v\n", submitDuration)
	fmt.Printf("Total (w/ drain): %v\n", totalDuration)
}
```

Because 50 producers submit nearly simultaneously to a queue with capacity 10, most submissions beyond the first 10 + whatever consumers drain in time will be rejected. The exact ratio varies per run due to goroutine scheduling, but the pattern is clear: the queue protects the system from overload.

### Verification
```bash
go run -race main.go
# Expected: ~10-15 accepted, ~35-40 rejected (varies per run)
# All accepted jobs are eventually processed
# No race warnings
```

## Common Mistakes

### Using a Blocking Send Instead of Try-Send

**Wrong:**
```go
func (q *WorkQueue) Submit(job Job) {
    q.jobs <- job // blocks if queue is full -- defeats the purpose!
}
```

**What happens:** The caller (e.g., HTTP handler goroutine) blocks until a consumer creates space. During a traffic spike, all handler goroutines block, and the server stops responding.

**Fix:** Use the try-send idiom with `select`/`default`:
```go
func (q *WorkQueue) Submit(job Job) error {
    select {
    case q.jobs <- job:
        return nil
    default:
        return ErrQueueFull
    }
}
```

### Forgetting to Close the Queue Channel

**Wrong:**
```go
queue.Close() // never called
// consumers range over queue.jobs forever
```

**What happens:** Consumer goroutines block on `range` indefinitely after all producers finish. The program hangs or leaks goroutines.

**Fix:** Always close the queue after all producers finish. Use a `sync.WaitGroup` to know when producers are done:
```go
producerWG.Wait()
queue.Close()
```

### Reading `len(ch)` as an Exact Guarantee

**Wrong:**
```go
if len(q.jobs) < cap(q.jobs) {
    q.jobs <- job // another goroutine might fill it between check and send!
}
```

**What happens:** Between checking `len` and sending, another goroutine may have filled the remaining capacity. The send blocks.

**Fix:** The try-send idiom is atomic -- it checks and sends in one operation. Never separate the capacity check from the send.

## Review

The queue is a buffered channel of a fixed capacity, and every design choice
follows from wanting rejection instead of blocking. The try-send idiom --
`select { case ch <- v: default: }` -- is one atomic operation: it sends if the
buffer has room and takes the `default` branch the instant it does not,
returning `ErrQueueFull` so the caller can shed load with an HTTP 503, a retry,
or a drop rather than parking a handler goroutine. Consumers drain with `range`,
which ends cleanly the moment the queue is closed, so the shape of the whole
system is producers that never block and consumers that always terminate. The
one trap is `len(ch)`: it reports current occupancy but a separate
check-then-send races another goroutine into the last slot, which is exactly
why the atomic try-send exists.

Make sure you can answer why. Why does a `select` with `default` refuse to block
on a full queue where a bare `ch <- v` would wait? What breaks if you back the
queue with an unbuffered channel -- what capacity does `make(chan Job)` actually
give you? And why does the accepted-versus-rejected split shift from run to run
in the load test, when the queue capacity and the number of producers never
change?

## Resources
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- how buffered channels behave and where capacity comes from.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- the producer-consumer model the queue implements.
- [Go Spec: Select statements](https://go.dev/ref/spec#Select_statements) -- the exact semantics of `select` with a `default` branch that make try-send non-blocking.

---

Back to [Concurrency](../../concurrency.md) | Next: [21-channel-circuit-breaker](../21-channel-circuit-breaker/21-channel-circuit-breaker.md)
