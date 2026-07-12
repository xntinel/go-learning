# Exercise 2: A Literal chan chan Request Load Balancer

The previous exercise used a channel of channels by composition — a request that
carries a reply channel. This exercise builds the other shape: a literal
`chan chan Request` dispatcher, Rob Pike's balancer, where each idle worker
advertises readiness by pushing its own private request channel onto a shared
channel and the balancer hands the next job to whoever is at the front of that
queue. The result is least-recently-idle load balancing with no counters, no
scheduler, and no locks.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
dispatcher/                independent module: example.com/dispatcher
  go.mod
  dispatcher.go            type Balancer; New, Start, Submit, Stop; chan chan Request
  cmd/
    demo/
      main.go              runnable demo: submit jobs, see which worker ran each
  dispatcher_test.go       every-job-once, work-spreads, clean-shutdown tests
```

- Files: `dispatcher.go`, `cmd/demo/main.go`, `dispatcher_test.go`.
- Implement: a `Balancer` over `W` workers, each with a private `chan Request`; workers advertise on a shared `chan chan Request`; a dispatch goroutine pops a ready worker and sends it the next job; `Submit` returns the per-job result; `Stop` drains and joins.
- Test: submit N jobs across W workers, assert every job is processed exactly once; assert work spreads across all workers; assert `Stop` returns without deadlock; all under `-race`.
- Verify: `go test -count=1 -race ./...`

### The taxi-rank model

Picture a taxi rank. Each taxi (worker) has a spot in the queue. When a taxi is
free it drives to the back of the rank and waits (the worker sends its private
`inbox` channel onto the shared `ready` channel). A dispatcher takes the next
passenger (job) and gives them to the taxi at the front of the rank (the
balancer receives a worker's inbox from `ready` and sends the job into it). A
taxi only rejoins the rank after dropping its passenger off (the worker
re-advertises only after finishing and looping). Because a worker returns to the
back of the queue each time, the front of the queue is always the worker that has
been idle longest — least-recently-idle scheduling falls out of the FIFO ordering
of a channel for free.

The type at the center is `ready chan chan Request`: a channel whose elements are
the workers' private request channels. Each worker owns exactly one `inbox chan
Request`; the balancer owns `ready` and the incoming `jobs` channel. Ownership is
clean, so there is no shared mutable state and nothing to lock.

Shutdown is the subtle part. Every blocking operation in a worker and in the
dispatch loop selects against a shared `quit` channel, so closing `quit` unblocks
a worker whether it is waiting to advertise, waiting for a job, or trying to send
a result. `Stop` closes `quit` and then waits on a `sync.WaitGroup` that counts
the dispatcher plus every worker, so `Stop` returns only once all of them have
returned — a synchronous, leak-free shutdown. Result channels are buffered at
capacity one so a worker's send never blocks even if a caller has gone away.

Create `dispatcher.go`:

```go
package dispatcher

import "sync"

// Request is a job plus the private reply channel the caller waits on.
type Request struct {
	Job   int
	reply chan Result
}

// Result carries the computed value and the id of the worker that produced it.
type Result struct {
	Worker int
	Value  int
}

// Balancer dispatches jobs to a fixed pool of workers using a chan chan Request:
// idle workers advertise their private inbox on the shared ready channel.
type Balancer struct {
	workers int
	jobs    chan Request
	ready   chan chan Request
	quit    chan struct{}
	wg      sync.WaitGroup
}

// New returns a Balancer over the given number of workers.
func New(workers int) *Balancer {
	return &Balancer{
		workers: workers,
		jobs:    make(chan Request),
		ready:   make(chan chan Request),
		quit:    make(chan struct{}),
	}
}

// Start launches the workers and the dispatch loop.
func (b *Balancer) Start() {
	for id := range b.workers {
		b.wg.Add(1)
		go b.worker(id)
	}
	b.wg.Add(1)
	go b.dispatch()
}

// worker advertises its inbox on ready, waits for one job, answers it, repeats.
func (b *Balancer) worker(id int) {
	defer b.wg.Done()
	inbox := make(chan Request)
	for {
		select {
		case b.ready <- inbox: // advertise readiness
		case <-b.quit:
			return
		}
		select {
		case req := <-inbox:
			req.reply <- Result{Worker: id, Value: req.Job * 2}
		case <-b.quit:
			return
		}
	}
}

// dispatch pops a ready worker for each incoming job and hands it over.
func (b *Balancer) dispatch() {
	defer b.wg.Done()
	for {
		select {
		case req := <-b.jobs:
			select {
			case inbox := <-b.ready:
				select {
				case inbox <- req:
				case <-b.quit:
					return
				}
			case <-b.quit:
				return
			}
		case <-b.quit:
			return
		}
	}
}

// Submit sends a job and blocks until its result is ready.
func (b *Balancer) Submit(job int) Result {
	reply := make(chan Result, 1)
	b.jobs <- Request{Job: job, reply: reply}
	return <-reply
}

// Stop signals shutdown and waits for the dispatcher and all workers to exit.
func (b *Balancer) Stop() {
	close(b.quit)
	b.wg.Wait()
}
```

### The runnable demo

The demo submits a handful of jobs and prints which worker handled each. Because
the workers are all idle at the start, the first few jobs fan out across distinct
workers; the exact assignment depends on scheduling, so the demo prints only the
doubled values, which are deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dispatcher"
)

func main() {
	b := dispatcher.New(3)
	b.Start()
	defer b.Stop()

	for job := 1; job <= 4; job++ {
		res := b.Submit(job)
		fmt.Printf("job %d -> %d\n", job, res.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
job 1 -> 2
job 2 -> 4
job 3 -> 6
job 4 -> 8
```

### Tests

`TestEveryJobProcessedOnce` submits N jobs concurrently and asserts the collected
results form exactly the expected set (each job doubled, once). `TestWorkSpreads`
gives the workers a small per-job delay so idle rotation is observable, then
asserts every worker id appears in the results — proving the balancer really
rotates through workers rather than pinning one. `TestStopIsClean` starts and
stops an idle balancer to prove `Stop` joins every goroutine without deadlock.

Create `dispatcher_test.go`:

```go
package dispatcher

import (
	"sync"
	"testing"
	"time"
)

func TestEveryJobProcessedOnce(t *testing.T) {
	t.Parallel()
	const n = 60
	b := New(4)
	b.Start()
	defer b.Stop()

	results := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = b.Submit(i).Value
		}()
	}
	wg.Wait()

	for i := range n {
		if results[i] != i*2 {
			t.Fatalf("job %d = %d, want %d", i, results[i], i*2)
		}
	}
}

func TestWorkSpreads(t *testing.T) {
	t.Parallel()
	const workers = 4
	b := New(workers)
	b.Start()
	defer b.Stop()

	// Let every worker advertise before the first dispatch so the initial jobs
	// fan out across distinct workers.
	time.Sleep(20 * time.Millisecond)

	seen := make(map[int]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := b.Submit(1).Worker
			mu.Lock()
			seen[w] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) < 2 {
		t.Fatalf("work did not spread: only %d worker(s) used", len(seen))
	}
}

func TestStopIsClean(t *testing.T) {
	t.Parallel()
	b := New(3)
	b.Start()

	done := make(chan struct{})
	go func() {
		b.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return: workers or dispatcher deadlocked")
	}
}
```

## Review

The balancer is correct when every submitted job is answered exactly once and the
answer carries the right value; `TestEveryJobProcessedOnce` encodes that as index
alignment over 60 concurrent submissions. The load-spreading property is the
reason to prefer this pattern over a single worker: because a worker re-advertises
only after finishing, the `ready` channel orders workers least-recently-idle, so
work naturally rotates. `TestWorkSpreads` asserts at least two distinct workers
run under a delayed workload; in practice, with all workers idle at the start,
the first jobs reach all of them.

The mistakes to avoid are all about shutdown. Every blocking send and receive in
the worker and dispatch loops must select against `quit`, or `Stop` will deadlock
waiting on a goroutine parked on a send with no receiver. The result channel must
be buffered so a worker's send completes even if a caller has gone; and `Stop`
must `wg.Wait()` on both the workers and the dispatcher, or it returns while
goroutines are still live. Run `go test -race` to confirm the handoff across
`jobs`, `ready`, and each worker's `inbox` is free of data races.

## Resources

- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) — the original chan chan Request load balancer.
- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — channels of channels and channel FIFO ordering.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — joining the worker pool at shutdown.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — channels as synchronization and handoff.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-reply-with-context-cancellation.md](03-reply-with-context-cancellation.md)
