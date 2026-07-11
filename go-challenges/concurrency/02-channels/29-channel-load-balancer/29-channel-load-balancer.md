# Exercise 29: Channel-Based Load Balancer

A simple worker pool -- N goroutines pulling from one shared channel -- works only while every worker is identical and every task takes roughly the same time. Production is rarely that uniform: one worker sits on a slower node, some requests take ten times longer than others, and a worker can go unhealthy and stop processing. Under a shared-pull model a slow worker grabs a request and holds its slot while fast workers idle on the channel, so the system runs at the slowest active worker's pace and there is no way to steer around an unhealthy one short of killing its goroutine. An active load balancer inverts the control: a central goroutine receives every request and decides who gets it, workers report their pending load on a feedback channel, and the balancer routes to the least-loaded worker and simply stops sending to any it has marked down. It is the channel-based equivalent of a reverse proxy doing least-connections routing.

## What you'll build

```text
29-channel-load-balancer/
  main.go        round-robin baseline, then least-connections with a feedback
                 channel, worker health reporting, and a distribution comparison
```

- Build: an active, channel-based load balancer that routes to the least-loaded healthy worker.
- Implement: per-worker input channels, a `roundRobinBalancer` baseline, a `leastLoadedBalancer` that tracks pending counts from a `Completion` feedback channel inside a `select`, and health reporting that evicts down workers.
- Verify: `go run main.go`, and `go run -race main.go` on the feedback and health steps.

### Why a feedback channel and a select are the core

The balancer's accuracy depends on keeping its load view current, and that is entirely a `select` problem: the same statement must accept incoming requests and drain worker completions with equal priority. Read feedback only inside the request case and completed work piles up unseen -- the load view goes stale and the balancer keeps routing to workers that are already busy.

Health reporting extends the same idea. Workers announce liveness on a channel and the balancer removes any that report unhealthy or miss too many ticks, so an unhealthy node is routed around instead of killed. Active routing beats round-robin precisely when workers differ: the balancer detects the slow worker's queue growing and shifts load to the fast ones, which round-robin, blind to load, cannot do.

## Step 1 -- Round-Robin Baseline

Start with the simplest active routing: round-robin. The balancer goroutine cycles through workers in order, sending each request to the next worker.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	workerCount   = 3
	requestCount  = 12
	baseWorkerLag = 30 * time.Millisecond
)

// Request represents work to be processed.
type Request struct {
	ID    int
	Reply chan Response
}

// NewRequest creates a request with an initialized reply channel.
func NewRequest(id int) Request {
	return Request{ID: id, Reply: make(chan Response, 1)}
}

// Response carries the result of processing a request.
type Response struct {
	RequestID int
	WorkerID  int
	Duration  time.Duration
}

// Worker processes requests from its personal channel.
type Worker struct {
	ID    int
	Input chan Request
}

// NewWorker creates a worker with a buffered input channel.
func NewWorker(id int) *Worker {
	return &Worker{ID: id, Input: make(chan Request, 10)}
}

// Run processes requests until the input channel is closed.
func (w *Worker) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	for req := range w.Input {
		start := time.Now()
		time.Sleep(baseWorkerLag)
		req.Reply <- Response{
			RequestID: req.ID,
			WorkerID:  w.ID,
			Duration:  time.Since(start).Round(time.Millisecond),
		}
	}
}

// roundRobinBalancer distributes requests in order across workers.
func roundRobinBalancer(intake <-chan Request, workers []*Worker) {
	idx := 0
	for req := range intake {
		workers[idx].Input <- req
		idx = (idx + 1) % len(workers)
	}
	for _, w := range workers {
		close(w.Input)
	}
}

func main() {
	workers := make([]*Worker, workerCount)
	var wg sync.WaitGroup
	for i := range workers {
		workers[i] = NewWorker(i + 1)
		wg.Add(1)
		go workers[i].Run(&wg)
	}

	intake := make(chan Request, requestCount)
	go roundRobinBalancer(intake, workers)

	requests := make([]Request, requestCount)
	for i := range requests {
		requests[i] = NewRequest(i + 1)
		intake <- requests[i]
	}
	close(intake)

	counts := make(map[int]int)
	for _, req := range requests {
		resp := <-req.Reply
		counts[resp.WorkerID]++
		fmt.Printf("  req %2d -> worker %d (%v)\n", resp.RequestID, resp.WorkerID, resp.Duration)
	}

	wg.Wait()
	fmt.Println()
	fmt.Println("=== Distribution ===")
	for id := 1; id <= workerCount; id++ {
		fmt.Printf("  worker %d: %d requests\n", id, counts[id])
	}
}
```

Key observations:
- Each worker has its own input channel -- the balancer decides who gets what
- Round-robin gives perfectly even distribution (12 requests / 3 workers = 4 each)
- This works when all workers and requests are identical, but breaks with variable load

### Verification
```bash
go run main.go
```
Expected output:
```
  req  1 -> worker 1 (30ms)
  req  2 -> worker 2 (30ms)
  ...
  req 12 -> worker 3 (30ms)

=== Distribution ===
  worker 1: 4 requests
  worker 2: 4 requests
  worker 3: 4 requests
```

## Step 2 -- Least-Connections with Feedback Channel

Replace round-robin with load-aware routing. Workers report completion events on a shared feedback channel. The balancer tracks pending counts and always routes to the least-loaded worker.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	lcWorkerCount  = 3
	lcRequestCount = 15
)

type Request struct {
	ID    int
	Reply chan Response
}

func NewRequest(id int) Request {
	return Request{ID: id, Reply: make(chan Response, 1)}
}

type Response struct {
	RequestID int
	WorkerID  int
	Duration  time.Duration
}

// Completion is sent by a worker when it finishes a request.
type Completion struct {
	WorkerID int
}

type Worker struct {
	ID       int
	Input    chan Request
	Feedback chan<- Completion
	lag      time.Duration
}

func NewWorker(id int, lag time.Duration, feedback chan<- Completion) *Worker {
	return &Worker{
		ID:       id,
		Input:    make(chan Request, 10),
		Feedback: feedback,
		lag:      lag,
	}
}

func (w *Worker) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	for req := range w.Input {
		start := time.Now()
		time.Sleep(w.lag)
		req.Reply <- Response{
			RequestID: req.ID,
			WorkerID:  w.ID,
			Duration:  time.Since(start).Round(time.Millisecond),
		}
		w.Feedback <- Completion{WorkerID: w.ID}
	}
}

// leastLoadedBalancer routes each request to the worker with the
// fewest pending requests. It updates load counts from the feedback channel.
func leastLoadedBalancer(intake <-chan Request, workers []*Worker, feedback <-chan Completion) {
	pending := make(map[int]int)
	for _, w := range workers {
		pending[w.ID] = 0
	}

	for {
		select {
		case req, ok := <-intake:
			if !ok {
				for _, w := range workers {
					close(w.Input)
				}
				return
			}
			best := workers[0]
			for _, w := range workers[1:] {
				if pending[w.ID] < pending[best.ID] {
					best = w
				}
			}
			pending[best.ID]++
			best.Input <- req

		case comp := <-feedback:
			if pending[comp.WorkerID] > 0 {
				pending[comp.WorkerID]--
			}
		}
	}
}

func main() {
	feedback := make(chan Completion, lcRequestCount)

	// Worker 3 is 4x slower -- simulates a degraded node.
	lags := []time.Duration{
		20 * time.Millisecond,
		20 * time.Millisecond,
		80 * time.Millisecond,
	}

	workers := make([]*Worker, lcWorkerCount)
	var wg sync.WaitGroup
	for i := range workers {
		workers[i] = NewWorker(i+1, lags[i], feedback)
		wg.Add(1)
		go workers[i].Run(&wg)
	}

	intake := make(chan Request, lcRequestCount)
	go leastLoadedBalancer(intake, workers, feedback)

	requests := make([]Request, lcRequestCount)
	for i := range requests {
		requests[i] = NewRequest(i + 1)
		intake <- requests[i]
	}
	close(intake)

	counts := make(map[int]int)
	for _, req := range requests {
		resp := <-req.Reply
		counts[resp.WorkerID]++
		fmt.Printf("  req %2d -> worker %d (%v)\n", resp.RequestID, resp.WorkerID, resp.Duration)
	}

	wg.Wait()
	fmt.Println()
	fmt.Println("=== Distribution ===")
	for id := 1; id <= lcWorkerCount; id++ {
		fmt.Printf("  worker %d: %d requests (lag: %v)\n", id, counts[id], lags[id-1])
	}
}
```

Key observation: worker 3 is 4x slower. With round-robin it would get the same count as others, creating a bottleneck. With least-connections feedback, the balancer detects worker 3's queue is growing and routes fewer requests to it. Workers 1 and 2 absorb more of the load.

### Verification
```bash
go run -race main.go
```
Expected output (distribution approximate):
```
  req  1 -> worker 1 (20ms)
  req  2 -> worker 2 (20ms)
  req  3 -> worker 3 (80ms)
  ...

=== Distribution ===
  worker 1: 6 requests (lag: 20ms)
  worker 2: 6 requests (lag: 20ms)
  worker 3: 3 requests (lag: 80ms)
```
Fast workers handle roughly 2x as many requests as the slow worker.

## Step 3 -- Worker Health Reporting

Add health status so the balancer can remove unhealthy workers from the routing pool. Workers send periodic health reports. If a worker misses too many reports, the balancer marks it unavailable.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	healthWorkerCount  = 3
	healthRequestCount = 20
	healthInterval     = 50 * time.Millisecond
	missedThreshold    = 3
)

type Request struct {
	ID    int
	Reply chan Response
}

func NewRequest(id int) Request {
	return Request{ID: id, Reply: make(chan Response, 1)}
}

type Response struct {
	RequestID int
	WorkerID  int
}

type Completion struct {
	WorkerID int
}

// HealthReport is sent periodically by each worker.
type HealthReport struct {
	WorkerID int
	Healthy  bool
}

// WorkerState tracks the balancer's view of a worker.
type WorkerState struct {
	Worker      *Worker
	Pending     int
	Healthy     bool
	MissedTicks int
}

type Worker struct {
	ID       int
	Input    chan Request
	Feedback chan<- Completion
	Health   chan<- HealthReport
	lag      time.Duration
	failAt   int
}

func NewWorker(id int, lag time.Duration, failAt int, feedback chan<- Completion, health chan<- HealthReport) *Worker {
	return &Worker{
		ID:       id,
		Input:    make(chan Request, 10),
		Feedback: feedback,
		Health:   health,
		lag:      lag,
		failAt:   failAt,
	}
}

func (w *Worker) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	processed := 0
	for req := range w.Input {
		processed++
		if w.failAt > 0 && processed >= w.failAt {
			req.Reply <- Response{RequestID: req.ID, WorkerID: -1}
			w.Health <- HealthReport{WorkerID: w.ID, Healthy: false}
			for discard := range w.Input {
				discard.Reply <- Response{RequestID: discard.ID, WorkerID: -1}
			}
			return
		}
		time.Sleep(w.lag)
		req.Reply <- Response{RequestID: req.ID, WorkerID: w.ID}
		w.Feedback <- Completion{WorkerID: w.ID}
	}
}

func (w *Worker) RunHealthReporter(done <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	ticker := time.NewTicker(healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			w.Health <- HealthReport{WorkerID: w.ID, Healthy: true}
		}
	}
}

func healthBalancer(
	intake <-chan Request,
	states map[int]*WorkerState,
	feedback <-chan Completion,
	health <-chan HealthReport,
	healthTick <-chan time.Time,
) {
	for {
		select {
		case req, ok := <-intake:
			if !ok {
				for _, s := range states {
					close(s.Worker.Input)
				}
				return
			}
			var best *WorkerState
			for _, s := range states {
				if !s.Healthy {
					continue
				}
				if best == nil || s.Pending < best.Pending {
					best = s
				}
			}
			if best == nil {
				req.Reply <- Response{RequestID: req.ID, WorkerID: -1}
				continue
			}
			best.Pending++
			best.Worker.Input <- req

		case comp := <-feedback:
			if s, ok := states[comp.WorkerID]; ok && s.Pending > 0 {
				s.Pending--
			}

		case report := <-health:
			if s, ok := states[report.WorkerID]; ok {
				if report.Healthy {
					s.MissedTicks = 0
				} else {
					s.Healthy = false
					fmt.Printf("  [balancer] worker %d reported unhealthy\n", report.WorkerID)
				}
			}

		case <-healthTick:
			for id, s := range states {
				if !s.Healthy {
					continue
				}
				s.MissedTicks++
				if s.MissedTicks >= missedThreshold {
					s.Healthy = false
					fmt.Printf("  [balancer] worker %d missed %d health ticks, marked down\n",
						id, s.MissedTicks)
				}
			}
		}
	}
}

func main() {
	feedback := make(chan Completion, healthRequestCount)
	healthCh := make(chan HealthReport, healthWorkerCount*10)
	done := make(chan struct{})

	configs := []struct {
		lag    time.Duration
		failAt int
	}{
		{20 * time.Millisecond, 0},
		{20 * time.Millisecond, 0},
		{20 * time.Millisecond, 5},
	}

	states := make(map[int]*WorkerState)
	var workerWG, healthWG sync.WaitGroup
	for i, cfg := range configs {
		w := NewWorker(i+1, cfg.lag, cfg.failAt, feedback, healthCh)
		states[w.ID] = &WorkerState{Worker: w, Healthy: true}
		workerWG.Add(1)
		go w.Run(&workerWG)
		healthWG.Add(1)
		go w.RunHealthReporter(done, &healthWG)
	}

	healthTicker := time.NewTicker(healthInterval * 2)
	defer healthTicker.Stop()

	intake := make(chan Request, healthRequestCount)
	go healthBalancer(intake, states, feedback, healthCh, healthTicker.C)

	requests := make([]Request, healthRequestCount)
	for i := range requests {
		requests[i] = NewRequest(i + 1)
		intake <- requests[i]
		time.Sleep(10 * time.Millisecond)
	}
	close(intake)

	successCount, failCount := 0, 0
	counts := make(map[int]int)
	for _, req := range requests {
		resp := <-req.Reply
		if resp.WorkerID == -1 {
			failCount++
			fmt.Printf("  req %2d -> FAILED (no healthy worker)\n", resp.RequestID)
		} else {
			successCount++
			counts[resp.WorkerID]++
			fmt.Printf("  req %2d -> worker %d\n", resp.RequestID, resp.WorkerID)
		}
	}

	close(done)
	workerWG.Wait()
	healthWG.Wait()

	fmt.Printf("\n=== Summary ===\n")
	fmt.Printf("  success: %d, failed: %d\n", successCount, failCount)
	for id := 1; id <= healthWorkerCount; id++ {
		status := "healthy"
		if !states[id].Healthy {
			status = "DOWN"
		}
		fmt.Printf("  worker %d: %d requests [%s]\n", id, counts[id], status)
	}
}
```

Worker 3 fails after processing 5 requests and reports itself unhealthy. The balancer stops routing to it. Remaining requests go to workers 1 and 2.

### Verification
```bash
go run -race main.go
```
Expected output (approximate):
```
  [balancer] worker 3 reported unhealthy
  req  1 -> worker 1
  ...
  req 20 -> worker 2

=== Summary ===
  success: 15, failed: 5
  worker 1: 8 requests [healthy]
  worker 2: 7 requests [healthy]
  worker 3: 5 requests [DOWN]
```

## Step 4 -- Distribution Comparison

Compare round-robin vs least-connections under variable load to see the throughput difference.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	cmpWorkerCount  = 3
	cmpRequestCount = 18
)

type Request struct {
	ID    int
	Reply chan Response
}

func NewRequest(id int) Request {
	return Request{ID: id, Reply: make(chan Response, 1)}
}

type Response struct {
	RequestID int
	WorkerID  int
}

type Completion struct {
	WorkerID int
}

type Worker struct {
	ID       int
	Input    chan Request
	Feedback chan<- Completion
	lag      time.Duration
}

func NewWorker(id int, lag time.Duration, feedback chan<- Completion) *Worker {
	return &Worker{
		ID:       id,
		Input:    make(chan Request, 20),
		Feedback: feedback,
		lag:      lag,
	}
}

func (w *Worker) Run(wg *sync.WaitGroup) {
	defer wg.Done()
	for req := range w.Input {
		time.Sleep(w.lag)
		req.Reply <- Response{RequestID: req.ID, WorkerID: w.ID}
		if w.Feedback != nil {
			w.Feedback <- Completion{WorkerID: w.ID}
		}
	}
}

func runRoundRobin(workers []*Worker, requests []Request) (map[int]int, time.Duration) {
	intake := make(chan Request, len(requests))
	go func() {
		idx := 0
		for req := range intake {
			workers[idx].Input <- req
			idx = (idx + 1) % len(workers)
		}
		for _, w := range workers {
			close(w.Input)
		}
	}()

	start := time.Now()
	for _, req := range requests {
		intake <- req
	}
	close(intake)
	counts := make(map[int]int)
	for _, req := range requests {
		resp := <-req.Reply
		counts[resp.WorkerID]++
	}
	return counts, time.Since(start)
}

func runLeastConn(workers []*Worker, requests []Request, feedback <-chan Completion) (map[int]int, time.Duration) {
	intake := make(chan Request, len(requests))
	go func() {
		pending := make(map[int]int)
		for _, w := range workers {
			pending[w.ID] = 0
		}
		for {
			select {
			case req, ok := <-intake:
				if !ok {
					for _, w := range workers {
						close(w.Input)
					}
					return
				}
				best := workers[0]
				for _, w := range workers[1:] {
					if pending[w.ID] < pending[best.ID] {
						best = w
					}
				}
				pending[best.ID]++
				best.Input <- req
			case comp := <-feedback:
				if pending[comp.WorkerID] > 0 {
					pending[comp.WorkerID]--
				}
			}
		}
	}()

	start := time.Now()
	for _, req := range requests {
		intake <- req
	}
	close(intake)
	counts := make(map[int]int)
	for _, req := range requests {
		resp := <-req.Reply
		counts[resp.WorkerID]++
	}
	return counts, time.Since(start)
}

func main() {
	lags := []time.Duration{
		10 * time.Millisecond,
		10 * time.Millisecond,
		60 * time.Millisecond,
	}

	// Round-robin test.
	rrFeedback := make(chan Completion, cmpRequestCount)
	rrWorkers := make([]*Worker, cmpWorkerCount)
	var rrWG sync.WaitGroup
	for i := range rrWorkers {
		rrWorkers[i] = NewWorker(i+1, lags[i], rrFeedback)
		rrWG.Add(1)
		go rrWorkers[i].Run(&rrWG)
	}
	rrRequests := make([]Request, cmpRequestCount)
	for i := range rrRequests {
		rrRequests[i] = NewRequest(i + 1)
	}
	rrCounts, rrTime := runRoundRobin(rrWorkers, rrRequests)
	rrWG.Wait()

	// Least-connections test.
	lcFeedback := make(chan Completion, cmpRequestCount)
	lcWorkers := make([]*Worker, cmpWorkerCount)
	var lcWG sync.WaitGroup
	for i := range lcWorkers {
		lcWorkers[i] = NewWorker(i+1, lags[i], lcFeedback)
		lcWG.Add(1)
		go lcWorkers[i].Run(&lcWG)
	}
	lcRequests := make([]Request, cmpRequestCount)
	for i := range lcRequests {
		lcRequests[i] = NewRequest(i + 1)
	}
	lcCounts, lcTime := runLeastConn(lcWorkers, lcRequests, lcFeedback)
	lcWG.Wait()

	fmt.Println("=== Round-Robin ===")
	for id := 1; id <= cmpWorkerCount; id++ {
		fmt.Printf("  worker %d: %d requests (lag: %v)\n", id, rrCounts[id], lags[id-1])
	}
	fmt.Printf("  wall time: %v\n", rrTime.Round(time.Millisecond))

	fmt.Println()
	fmt.Println("=== Least-Connections ===")
	for id := 1; id <= cmpWorkerCount; id++ {
		fmt.Printf("  worker %d: %d requests (lag: %v)\n", id, lcCounts[id], lags[id-1])
	}
	fmt.Printf("  wall time: %v\n", lcTime.Round(time.Millisecond))

	improvement := float64(rrTime-lcTime) / float64(rrTime) * 100
	fmt.Printf("\nLeast-connections is %.0f%% faster\n", improvement)
}
```

### Verification
```bash
go run -race main.go
```
Expected output (approximate):
```
=== Round-Robin ===
  worker 1: 6 requests (lag: 10ms)
  worker 2: 6 requests (lag: 10ms)
  worker 3: 6 requests (lag: 60ms)
  wall time: 360ms

=== Least-Connections ===
  worker 1: 8 requests (lag: 10ms)
  worker 2: 8 requests (lag: 10ms)
  worker 3: 2 requests (lag: 60ms)
  wall time: 120ms

Least-connections is 67% faster
```

## Common Mistakes

### Reading Feedback Only When a Request Arrives
**What happens:** If the balancer only checks the feedback channel inside the request-handling case, completed requests pile up in the feedback buffer. The balancer's load view becomes stale, and it keeps routing to already-busy workers.

**Fix:** Use `select` so the balancer processes feedback completions and incoming requests with equal priority. The feedback channel must be checked in the same select as the intake channel.

### Blocking on Worker Input Channel
**What happens:** If a worker's input channel is unbuffered or full, the balancer blocks on `best.Input <- req`, unable to process feedback from other workers. The entire system stalls.

**Fix:** Buffer worker input channels generously. Alternatively, use a non-blocking send with a select and a fallback to the next-least-loaded worker.

### Not Closing Worker Input Channels
**What happens:** When the intake channel closes but worker input channels are not closed, worker goroutines block forever on `range w.Input`, leaking goroutines.

**Fix:** The balancer must close all worker input channels after the intake channel is drained:
```go
case req, ok := <-intake:
    if !ok {
        for _, w := range workers {
            close(w.Input)
        }
        return
    }
```

## Review

The design replaces a shared pull queue with a central goroutine that actively chooses a worker for every request, giving each worker its own input channel instead of a common one. That choice is what makes load-awareness possible: workers report completions on a feedback channel, the balancer keeps a pending-count view, and least-connections routing sends the next request to whoever has the fewest outstanding. Health reporting layers on top -- workers that report unhealthy or miss too many ticks drop out of the pool -- so the balancer routes around a failing node rather than killing it. All of this hinges on the `select`: intake and feedback must be handled in the same statement or the load view rots, and with variable worker speeds this active routing decisively beats round-robin, which distributes evenly precisely because it is blind to load.

To push past the exercise, extend it in two directions without re-reading the code. Add a weighted mode where each worker carries a capacity multiplier that the least-connections calculation divides by, so a beefier node earns proportionally more work. Then add a circuit breaker: pull a worker after three consecutive request timeouts and reintroduce it after a cooldown, which is the same eviction logic as health reporting but triggered by observed failures rather than self-reported liveness.

## Resources
- [Go Concurrency Patterns (Rob Pike)](https://go.dev/talks/2012/concurrency.slide) -- the foundational talk on wiring goroutines together with channels, including load balancing.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) -- why routing decisions live in a goroutine's mailbox instead of shared, locked state.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the channel and `select` mechanics the balancer is built from.
- [Advanced Go Concurrency Patterns](https://go.dev/talks/2013/advconc.slide) -- deeper patterns for feedback, cancellation, and dynamic worker sets.

---

Back to [Concurrency](../../concurrency.md) | Next: [30-channel-broadcast](../30-channel-broadcast/30-channel-broadcast.md)
