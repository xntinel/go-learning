# Exercise 9: Select with Context

The `context` package is Go's standard mechanism for cancellation, deadlines,
and request-scoped values, and every production Go program -- HTTP servers, gRPC
services, CLI tools, background workers -- uses it to signal "stop what you are
doing." The `select` statement is how a goroutine listens for that signal while
still doing its work. Without a `select` on `ctx.Done()`, a goroutine blocked on
a channel read has no way to learn it should stop; it hangs forever, leaking
memory and goroutines. This exercise drills the pattern that prevents that, in
the four shapes you will actually meet in a service.

## What you'll build

```text
09-select-with-context/
  main.go        cancellation-aware building blocks: request handler, queue
                 consumer, worker pool, and a graceful-drain service
```

- Build: cancellation-aware services that stop cleanly when their context is cancelled or times out.
- Implement: `handleRequests` (checking `ctx.Done()` on both receive and send), a `QueueConsumer.Run` under `context.WithTimeout`, a `WorkerPool` whose workers all stop on one cancel, and a `Service` that drains its buffer before shutting down.
- Verify: `go run -race main.go` for each step -- every one should exit cleanly with no race warnings.

### Why a blocked goroutine needs the signal

The pattern is simple and universal:

```go
select {
case <-ctx.Done():
    return ctx.Err()
case result := <-workCh:
    // process result
}
```

This appears in HTTP handlers that must respect client disconnects, in queue
consumers that must stop on shutdown, and in pipeline stages that must propagate
cancellation downstream. If you write Go professionally, you will write this
pattern multiple times per day. It is the fundamental building block for
cooperative cancellation.

Without `select` on `ctx.Done()`, a goroutine that is blocked on a channel read
has no way to learn that it should stop. It will hang forever, leaking memory and
goroutines. The combination of `select` and `context` is what makes Go's
concurrency model practical for real services.

## Step 1 -- Basic Request Handler with Context

Build a request handler that processes work from a channel but respects a cancellation signal. This is the simplest form of the pattern.

```go
package main

import (
	"context"
	"fmt"
	"time"
)

type Request struct {
	ID      int
	Payload string
}

type Result struct {
	RequestID int
	Output    string
}

func processRequest(req Request) Result {
	time.Sleep(30 * time.Millisecond)
	return Result{
		RequestID: req.ID,
		Output:    fmt.Sprintf("processed: %s", req.Payload),
	}
}

func handleRequests(ctx context.Context, requests <-chan Request) <-chan Result {
	results := make(chan Result)

	go func() {
		defer close(results)
		for {
			select {
			case <-ctx.Done():
				fmt.Printf("handler stopped: %v\n", ctx.Err())
				return
			case req, ok := <-requests:
				if !ok {
					fmt.Println("request channel closed, handler exiting")
					return
				}
				result := processRequest(req)
				select {
				case <-ctx.Done():
					fmt.Printf("handler stopped during send: %v\n", ctx.Err())
					return
				case results <- result:
				}
			}
		}
	}()

	return results
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	requests := make(chan Request)

	go func() {
		for i := 0; i < 20; i++ {
			requests <- Request{ID: i, Payload: fmt.Sprintf("task-%d", i)}
		}
		close(requests)
	}()

	results := handleRequests(ctx, requests)

	for i := 0; i < 5; i++ {
		r := <-results
		fmt.Printf("received result: request=%d output=%q\n", r.RequestID, r.Output)
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
	fmt.Println("main: shutdown complete")
}
```

The handler checks `ctx.Done()` at two points: when waiting for a request and when sending a result. Both are necessary. If the context is cancelled while the handler is blocked sending a result, the second `select` ensures it does not hang.

### Verification
```
received result: request=0 output="processed: task-0"
received result: request=1 output="processed: task-1"
received result: request=2 output="processed: task-2"
received result: request=3 output="processed: task-3"
received result: request=4 output="processed: task-4"
handler stopped: context canceled
main: shutdown complete
```
The handler processes 5 requests and exits cleanly when cancelled.

## Step 2 -- Queue Consumer with Timeout

Build a queue consumer that processes items until a timeout expires. This models a batch job that must finish within a deadline.

```go
package main

import (
	"context"
	"fmt"
	"time"
)

type Job struct {
	ID   int
	Name string
}

type QueueConsumer struct {
	processed int
}

func NewQueueConsumer() *QueueConsumer {
	return &QueueConsumer{}
}

func (qc *QueueConsumer) Run(ctx context.Context, jobs <-chan Job) {
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("consumer stopped after %d jobs: %v\n", qc.processed, ctx.Err())
			return
		case job, ok := <-jobs:
			if !ok {
				fmt.Printf("queue empty, consumed %d jobs\n", qc.processed)
				return
			}
			qc.processJob(job)
		}
	}
}

func (qc *QueueConsumer) processJob(job Job) {
	time.Sleep(40 * time.Millisecond)
	qc.processed++
	fmt.Printf("processed job %d: %s\n", job.ID, job.Name)
}

func produceJobs(count int) <-chan Job {
	jobs := make(chan Job)
	go func() {
		defer close(jobs)
		for i := 0; i < count; i++ {
			jobs <- Job{ID: i, Name: fmt.Sprintf("batch-item-%d", i)}
		}
	}()
	return jobs
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	jobs := produceJobs(100)
	consumer := NewQueueConsumer()
	consumer.Run(ctx, jobs)

	fmt.Println("batch processing finished")
}
```

The consumer uses `context.WithTimeout` to enforce a deadline. It processes as many jobs as it can within 200ms. When the timeout fires, `ctx.Done()` becomes readable and the consumer stops.

### Verification
```
processed job 0: batch-item-0
processed job 1: batch-item-1
processed job 2: batch-item-2
processed job 3: batch-item-3
consumer stopped after 4 jobs: context deadline exceeded
batch processing finished
```
The exact count depends on timing, but the consumer always stops at the deadline.

## Step 3 -- Cascading Cancellation Across Workers

Build a service with multiple workers that all stop when the parent context is cancelled. This demonstrates how one cancellation propagates through the entire worker pool.

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type WorkerPool struct {
	size    int
	results chan string
}

func NewWorkerPool(size int) *WorkerPool {
	return &WorkerPool{
		size:    size,
		results: make(chan string, size*10),
	}
}

func (wp *WorkerPool) Start(ctx context.Context, tasks <-chan string) {
	var wg sync.WaitGroup

	for i := 0; i < wp.size; i++ {
		wg.Add(1)
		go wp.worker(ctx, i, tasks, &wg)
	}

	go func() {
		wg.Wait()
		close(wp.results)
	}()
}

func (wp *WorkerPool) worker(ctx context.Context, id int, tasks <-chan string, wg *sync.WaitGroup) {
	defer wg.Done()
	processed := 0

	for {
		select {
		case <-ctx.Done():
			fmt.Printf("worker-%d: stopped after %d tasks (%v)\n", id, processed, ctx.Err())
			return
		case task, ok := <-tasks:
			if !ok {
				fmt.Printf("worker-%d: no more tasks, processed %d\n", id, processed)
				return
			}
			time.Sleep(20 * time.Millisecond)
			processed++
			wp.results <- fmt.Sprintf("worker-%d completed %s", id, task)
		}
	}
}

func (wp *WorkerPool) Results() <-chan string {
	return wp.results
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())

	tasks := make(chan string, 100)
	go func() {
		for i := 0; i < 100; i++ {
			tasks <- fmt.Sprintf("task-%d", i)
		}
	}()

	pool := NewWorkerPool(3)
	pool.Start(ctx, tasks)

	collected := 0
	for result := range pool.Results() {
		fmt.Println(result)
		collected++
		if collected >= 10 {
			cancel()
			break
		}
	}

	for range pool.Results() {
	}

	fmt.Printf("collected %d results, all workers stopped\n", collected)
}
```

Three workers share a task channel. When the main goroutine has enough results, it cancels the context. All three workers detect the cancellation through `ctx.Done()` and exit. The `WaitGroup` ensures the results channel is closed only after all workers have stopped.

### Verification
```
worker-0 completed task-0
worker-1 completed task-1
worker-2 completed task-2
worker-0 completed task-3
worker-1 completed task-4
worker-2 completed task-5
worker-0 completed task-6
worker-1 completed task-7
worker-2 completed task-8
worker-0 completed task-9
worker-0: stopped after 4 tasks (context canceled)
worker-1: stopped after 3 tasks (context canceled)
worker-2: stopped after 3 tasks (context canceled)
collected 10 results, all workers stopped
```
All workers stop promptly after cancellation. No goroutine leaks.

## Step 4 -- Service with Graceful Drain on Context Cancel

Build a service that, upon receiving a cancellation signal, finishes processing items already in its buffer before shutting down. This is the production pattern: stop accepting new work but complete in-flight work.

```go
package main

import (
	"context"
	"fmt"
	"time"
)

type Item struct {
	ID   int
	Data string
}

type Service struct {
	buffer  chan Item
	done    chan struct{}
}

func NewService(bufferSize int) *Service {
	return &Service{
		buffer: make(chan Item, bufferSize),
		done:   make(chan struct{}),
	}
}

func (s *Service) Submit(item Item) bool {
	select {
	case s.buffer <- item:
		return true
	default:
		return false
	}
}

func (s *Service) Run(ctx context.Context) {
	defer close(s.done)

	for {
		select {
		case <-ctx.Done():
			s.drain()
			return
		case item := <-s.buffer:
			s.processItem(item)
		}
	}
}

func (s *Service) drain() {
	fmt.Println("draining buffered items...")
	for {
		select {
		case item := <-s.buffer:
			s.processItem(item)
		default:
			fmt.Println("drain complete")
			return
		}
	}
}

func (s *Service) processItem(item Item) {
	time.Sleep(10 * time.Millisecond)
	fmt.Printf("processed item %d: %s\n", item.ID, item.Data)
}

func (s *Service) Wait() {
	<-s.done
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	svc := NewService(20)

	go svc.Run(ctx)

	for i := 0; i < 15; i++ {
		svc.Submit(Item{ID: i, Data: fmt.Sprintf("payload-%d", i)})
	}

	time.Sleep(60 * time.Millisecond)
	fmt.Println("cancelling service...")
	cancel()

	svc.Wait()
	fmt.Println("service shutdown complete")
}
```

When the context is cancelled, `Run` calls `drain()`, which processes all remaining buffered items using a `select` with `default`. The `default` case exits when the buffer is empty. The `done` channel signals the caller that shutdown is complete.

### Verification
```
processed item 0: payload-0
processed item 1: payload-1
processed item 2: payload-2
processed item 3: payload-3
processed item 4: payload-4
cancelling service...
draining buffered items...
processed item 5: payload-5
processed item 6: payload-6
...
processed item 14: payload-14
drain complete
service shutdown complete
```
Items processed before cancel run normally. Remaining buffered items are drained before shutdown.

## Verification

Run each step with the race detector:
```bash
go run -race main.go
```
Every step should complete without race warnings and exit cleanly.

## Common Mistakes

### 1. Checking ctx.Done() Only on Receive, Not on Send
If a goroutine sends to a channel without checking `ctx.Done()`, it blocks forever when the receiver has stopped:

```go
// BAD: blocks if nobody reads from results after cancel.
results <- processRequest(req)

// GOOD: can exit if context is cancelled while sending.
select {
case <-ctx.Done():
    return
case results <- processRequest(req):
}
```

### 2. Using context.Background() Instead of Propagating the Parent Context
Every function that does I/O or may block should accept a `context.Context` as its first parameter. Using `context.Background()` inside a function breaks the cancellation chain:

```go
// BAD: ignores parent cancellation.
func fetchData() string {
    ctx := context.Background()
    // ...
}

// GOOD: respects caller's cancellation.
func fetchData(ctx context.Context) (string, error) {
    // ...
}
```

### 3. Forgetting to Drain After Cancellation
When a worker sends to a buffered channel and the context is cancelled, values may remain in the channel. If nobody reads them, the sending goroutines block:

```go
cancel()
// REQUIRED: drain any in-flight values.
for range results {
}
```

### 4. Not Calling defer cancel()
Every `context.WithCancel` or `context.WithTimeout` returns a cancel function. If you do not call it, the context and its resources leak until the parent context is cancelled:

```go
ctx, cancel := context.WithTimeout(parentCtx, 5*time.Second)
defer cancel() // ALWAYS call this.
```

## Review

The `select { case <-ctx.Done(): case result := <-ch: }` pattern is the
foundation of cancellation in Go. Every goroutine that reads from or writes to a
channel in production must check `ctx.Done()` -- on both the receive and the send
side -- or it hangs when the work is no longer wanted. Cancellation propagates
through the entire goroutine tree: cancel the parent and every child stops,
whether the context was created with `context.WithCancel` (fires when you call
`cancel`) or `context.WithTimeout` (fires on `cancel` or when the deadline
passes, whichever comes first). For graceful shutdown, drain buffered channels
after cancellation so the goroutines still trying to send do not block forever.
The same shape drives HTTP servers, queue consumers, worker pools, and pipeline
stages -- any Go code that runs concurrently.

Check yourself against four questions. Can you explain why both the receive and
send sides of a worker need `ctx.Done()` checks? Can you describe how
`context.WithCancel` and `context.WithTimeout` differ in terms of when
`ctx.Done()` becomes readable? Can you implement a graceful drain that finishes
in-flight work before exiting? And can you trace how cancelling a parent context
reaches every child worker?

## Resources

- [context package documentation](https://pkg.go.dev/context) -- the full API for `Done`, `Err`, `WithCancel`, and `WithTimeout` used throughout this exercise.
- [Go Blog: Go Concurrency Patterns: Context](https://go.dev/blog/context) -- the original introduction to context and cancellation propagation.
- [Go Concurrency Patterns: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- how the done/cancel signal threads through multi-stage pipelines.
- [context.WithCancel](https://pkg.go.dev/context#WithCancel) -- the exact contract for the cancel function and when `Done` closes.

---

Back to [Concurrency](../../concurrency.md) | Next: [10-select-deadlock-prevention](../10-select-deadlock-prevention/10-select-deadlock-prevention.md)
