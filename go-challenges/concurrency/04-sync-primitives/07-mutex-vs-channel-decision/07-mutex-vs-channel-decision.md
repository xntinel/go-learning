# Exercise 7: Mutex vs Channel: Decision Criteria

Go gives you two mechanisms for concurrent coordination -- mutexes and channels
-- and both are correct; neither is universally better. Choosing the wrong one
produces code that is harder to read, harder to maintain, and quietly more
bug-prone: a channel doing a mutex's job, or a mutex forcing a polling loop where
a channel would block cleanly. This exercise takes two real production scenarios
and builds each one both ways, so you can feel which tool fits naturally and
which one fights you.

## What you'll build

```text
07-mutex-vs-channel-decision/
  main.go        a shared metrics map and an image pipeline, each built
                 with a mutex and with channels, plus a decision guide
```

- Build: two production scenarios -- a shared metrics map and an image-processing pipeline -- each implemented with a mutex and with channels.
- Implement: `MetricsStore` (mutex) versus `ChannelMetrics` (owner goroutine), and a channel pipeline versus a `SharedPipelineState` polling version.
- Verify: `go run -race main.go`.

### Why the proverb is guidance, not a ban on mutexes

The Go proverb says: **"Do not communicate by sharing memory; share memory by communicating."** This does not mean "never use mutexes." It means: when goroutines need to exchange information or coordinate work, channels are usually clearer. When goroutines need to protect a piece of shared state from concurrent access, mutexes are usually simpler.

This exercise presents two real scenarios from production systems and implements each with both approaches, so you can see which fits naturally and which feels forced.

## Step 1 -- Scenario 1: Shared Metrics Map (Better with Mutex)

An HTTP server tracks request counts per endpoint. Multiple handler goroutines increment counters concurrently. A metrics endpoint reads the totals.

**Mutex approach -- natural fit:**

```go
package main

import (
	"fmt"
	"sync"
)

const simulatedRequests = 1000

type MetricsStore struct {
	mu       sync.Mutex
	counters map[string]int64
}

func NewMetricsStore() *MetricsStore {
	return &MetricsStore{counters: make(map[string]int64)}
}

func (m *MetricsStore) Increment(endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counters[endpoint]++
}

func (m *MetricsStore) Snapshot() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]int64, len(m.counters))
	for k, v := range m.counters {
		result[k] = v
	}
	return result
}

func simulateTraffic(metrics *MetricsStore, endpoints []string, requestCount int) {
	var wg sync.WaitGroup

	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(reqID int) {
			defer wg.Done()
			endpoint := endpoints[reqID%len(endpoints)]
			metrics.Increment(endpoint)
		}(i)
	}

	wg.Wait()
}

func printMetricsReport(title string, snap map[string]int64) {
	fmt.Printf("=== %s ===\n", title)
	total := int64(0)
	for endpoint, count := range snap {
		fmt.Printf("  %-20s %d requests\n", endpoint, count)
		total += count
	}
	fmt.Printf("  %-20s %d requests\n", "TOTAL", total)
}

func main() {
	metrics := NewMetricsStore()
	endpoints := []string{"/api/users", "/api/orders", "/api/products", "/healthz"}

	simulateTraffic(metrics, endpoints, simulatedRequests)
	printMetricsReport("Request Metrics (mutex approach)", metrics.Snapshot())
}
```

Expected output:
```
=== Request Metrics (mutex approach) ===
  /api/users           250 requests
  /api/orders          250 requests
  /api/products        250 requests
  /healthz             250 requests
  TOTAL                1000 requests
```

This is clean: the mutex protects the map, each method is short, and the API is intuitive.

### Verification
```bash
go run -race main.go
```
Total should be exactly 1000, no race conditions.

## Step 2 -- Scenario 1 with Channels (Forced Fit)

Now implement the same metrics store using a channel-based goroutine owner:

```go
package main

import (
	"fmt"
	"sync"
)

const simulatedRequests = 1000

type opKind int

const (
	opIncrement opKind = iota
	opSnapshot
)

type metricsOp struct {
	kind     opKind
	endpoint string
	response chan map[string]int64
}

type ChannelMetrics struct {
	ops  chan metricsOp
	done chan struct{}
}

func NewChannelMetrics() *ChannelMetrics {
	m := &ChannelMetrics{
		ops:  make(chan metricsOp),
		done: make(chan struct{}),
	}
	go m.eventLoop()
	return m
}

func (m *ChannelMetrics) eventLoop() {
	counters := make(map[string]int64)
	for op := range m.ops {
		switch op.kind {
		case opIncrement:
			counters[op.endpoint]++
		case opSnapshot:
			op.response <- copyCounters(counters)
		}
	}
	close(m.done)
}

func copyCounters(src map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(src))
	for k, v := range src {
		result[k] = v
	}
	return result
}

func (m *ChannelMetrics) Increment(endpoint string) {
	m.ops <- metricsOp{kind: opIncrement, endpoint: endpoint}
}

func (m *ChannelMetrics) Snapshot() map[string]int64 {
	resp := make(chan map[string]int64)
	m.ops <- metricsOp{kind: opSnapshot, response: resp}
	return <-resp
}

func (m *ChannelMetrics) Close() {
	close(m.ops)
	<-m.done
}

func simulateTraffic(metrics *ChannelMetrics, endpoints []string, requestCount int) {
	var wg sync.WaitGroup

	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func(reqID int) {
			defer wg.Done()
			endpoint := endpoints[reqID%len(endpoints)]
			metrics.Increment(endpoint)
		}(i)
	}

	wg.Wait()
}

func printMetricsReport(title string, snap map[string]int64) {
	fmt.Printf("=== %s ===\n", title)
	total := int64(0)
	for endpoint, count := range snap {
		fmt.Printf("  %-20s %d requests\n", endpoint, count)
		total += count
	}
	fmt.Printf("  %-20s %d requests\n", "TOTAL", total)
}

func main() {
	metrics := NewChannelMetrics()
	endpoints := []string{"/api/users", "/api/orders", "/api/products", "/healthz"}

	simulateTraffic(metrics, endpoints, simulatedRequests)
	printMetricsReport("Request Metrics (channel approach)", metrics.Snapshot())
	metrics.Close()
}
```

This works but requires more code: an operation struct, a response channel, a background goroutine, and a Close method. For simple state protection, the channel approach adds complexity without clarity.

### Verification
```bash
go run -race main.go
```
Same result, but more ceremony to achieve it.

## Step 3 -- Scenario 2: Processing Pipeline (Better with Channels)

An image processing pipeline: fetch URLs, download images, resize them, and write to disk. Each stage runs independently and passes work to the next.

**Channel approach -- natural fit:**

```go
package main

import (
	"fmt"
	"time"
)

const (
	fetchDelay  = 20 * time.Millisecond
	resizeDelay = 30 * time.Millisecond
	writeDelay  = 10 * time.Millisecond
	pipelineBuf = 2
)

type ImageJob struct {
	URL   string
	Stage string
	Data  string
}

func fetchImages(urls []string, out chan<- ImageJob) {
	for _, url := range urls {
		time.Sleep(fetchDelay)
		out <- ImageJob{URL: url, Stage: "fetched", Data: "raw-bytes"}
		fmt.Printf("  [fetch]  %s\n", url)
	}
	close(out)
}

func resizeImages(in <-chan ImageJob, out chan<- ImageJob) {
	for job := range in {
		time.Sleep(resizeDelay)
		job.Stage = "resized"
		job.Data = "resized-bytes"
		out <- job
		fmt.Printf("  [resize] %s\n", job.URL)
	}
	close(out)
}

func writeImages(in <-chan ImageJob, done chan<- struct{}) {
	for job := range in {
		time.Sleep(writeDelay)
		fmt.Printf("  [write]  %s -> saved\n", job.URL)
	}
	close(done)
}

func runImagePipeline(urls []string) time.Duration {
	start := time.Now()

	fetchCh := make(chan ImageJob, pipelineBuf)
	resizeCh := make(chan ImageJob, pipelineBuf)
	done := make(chan struct{})

	go fetchImages(urls, fetchCh)
	go resizeImages(fetchCh, resizeCh)
	go writeImages(resizeCh, done)

	<-done
	return time.Since(start)
}

func main() {
	urls := []string{
		"cdn.example.com/img/001.jpg",
		"cdn.example.com/img/002.jpg",
		"cdn.example.com/img/003.jpg",
		"cdn.example.com/img/004.jpg",
		"cdn.example.com/img/005.jpg",
	}

	fmt.Println("=== Image Processing Pipeline (channels) ===")
	elapsed := runImagePipeline(urls)
	fmt.Printf("\nPipeline complete: %d images in %v\n", len(urls), elapsed.Round(time.Millisecond))
}
```

Expected output:
```
=== Image Processing Pipeline (channels) ===
  [fetch]  cdn.example.com/img/001.jpg
  [resize] cdn.example.com/img/001.jpg
  [fetch]  cdn.example.com/img/002.jpg
  [write]  cdn.example.com/img/001.jpg -> saved
  ...

Pipeline complete: 5 images in ~180ms
```

The pipeline stages are naturally connected by channels. Each stage runs independently, processes items as they arrive, and the entire pipeline overlaps fetch/resize/write for maximum throughput.

### Verification
```bash
go run main.go
```
All 5 images should flow through the pipeline. Total time should be less than doing all stages sequentially.

## Step 4 -- Scenario 2 with Mutex (Forced Fit)

Now try the same pipeline with mutexes. You end up polling shared state:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	fetchSimDelay  = 20 * time.Millisecond
	resizeSimDelay = 30 * time.Millisecond
	writeSimDelay  = 10 * time.Millisecond
	pollInterval   = 5 * time.Millisecond
)

type ImageJob struct {
	URL   string
	Stage string
}

type SharedPipelineState struct {
	mu           sync.Mutex
	fetchedQueue []ImageJob
	resizedQueue []ImageJob
	fetchDone    bool
	resizeDone   bool
}

func (s *SharedPipelineState) appendFetched(job ImageJob) {
	s.mu.Lock()
	s.fetchedQueue = append(s.fetchedQueue, job)
	s.mu.Unlock()
}

func (s *SharedPipelineState) markFetchDone() {
	s.mu.Lock()
	s.fetchDone = true
	s.mu.Unlock()
}

func (s *SharedPipelineState) appendResized(job ImageJob) {
	s.mu.Lock()
	s.resizedQueue = append(s.resizedQueue, job)
	s.mu.Unlock()
}

func (s *SharedPipelineState) markResizeDone() {
	s.mu.Lock()
	s.resizeDone = true
	s.mu.Unlock()
}

func runMutexFetcher(state *SharedPipelineState, urls []string, wg *sync.WaitGroup) {
	defer wg.Done()
	for _, url := range urls {
		time.Sleep(fetchSimDelay)
		state.appendFetched(ImageJob{URL: url, Stage: "fetched"})
		fmt.Printf("  [fetch] %s\n", url)
	}
	state.markFetchDone()
}

func runMutexResizer(state *SharedPipelineState, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		state.mu.Lock()
		if len(state.fetchedQueue) > 0 {
			job := state.fetchedQueue[0]
			state.fetchedQueue = state.fetchedQueue[1:]
			state.mu.Unlock()
			time.Sleep(resizeSimDelay)
			job.Stage = "resized"
			state.appendResized(job)
			fmt.Printf("  [resize] %s\n", job.URL)
		} else if state.fetchDone {
			state.mu.Unlock()
			state.markResizeDone()
			return
		} else {
			state.mu.Unlock()
			time.Sleep(pollInterval) // POLLING -- wasteful
		}
	}
}

func runMutexWriter(state *SharedPipelineState, wg *sync.WaitGroup) {
	defer wg.Done()
	for {
		state.mu.Lock()
		if len(state.resizedQueue) > 0 {
			job := state.resizedQueue[0]
			state.resizedQueue = state.resizedQueue[1:]
			state.mu.Unlock()
			time.Sleep(writeSimDelay)
			fmt.Printf("  [write] %s -> saved\n", job.URL)
		} else if state.resizeDone {
			state.mu.Unlock()
			return
		} else {
			state.mu.Unlock()
			time.Sleep(pollInterval) // POLLING -- wasteful
		}
	}
}

func main() {
	fmt.Println("=== Image Processing Pipeline (mutex -- awkward) ===")

	state := &SharedPipelineState{}
	urls := []string{
		"cdn.example.com/img/001.jpg",
		"cdn.example.com/img/002.jpg",
		"cdn.example.com/img/003.jpg",
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go runMutexFetcher(state, urls, &wg)
	go runMutexResizer(state, &wg)
	go runMutexWriter(state, &wg)

	wg.Wait()
	fmt.Println("\nThis works, but the polling loops are ugly, wasteful, and error-prone.")
	fmt.Println("Channels are the natural fit for pipeline coordination.")
}
```

The mutex version works but requires polling loops, manual "done" flags, and careful lock ordering. The channel version is half the code and clearly communicates intent.

### Verification
```bash
go run main.go
```
Same functional result, but the code is noticeably more complex and harder to reason about.

## Step 5 -- Decision Guide

```
Use MUTEX when:
  - Protecting internal state of a struct (counters, caches, config maps)
  - Simple read/write access patterns
  - Performance is critical (lower per-operation overhead)
  - The protected data has a clear owner (a single struct)

Use CHANNELS when:
  - Transferring data ownership between goroutines (pipelines)
  - Coordinating sequential phases of work
  - Fan-out/fan-in patterns
  - Select-based multiplexing with timeouts or cancellation
  - Signaling events (done, shutdown, ready)

Code smell indicators:
  - Using a buffered channel of size 1 as a lock -> use a mutex
  - Polling a mutex-protected flag in a loop -> use a channel
  - Channel with no data (chan struct{}) only for signaling -> consider sync.Once or context
```

## Common Mistakes

### Channel as a Mutex

```go
sem := make(chan struct{}, 1)
sem <- struct{}{} // "lock"
counter++
<-sem             // "unlock"
```

**Why this is a code smell:** It works but is a mutex in disguise. A real `sync.Mutex` is clearer, lighter, and has better tooling support (race detector, deadlock detection).

### Mutex for Pipeline Coordination

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

func main() {
	var mu sync.Mutex
	var phase1Done bool

	go func() {
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		phase1Done = true
		mu.Unlock()
	}()

	// Polling loop -- wasteful and ugly
	for {
		mu.Lock()
		done := phase1Done
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Println("phase 1 done (but this code is terrible)")
}
```

**Why this is a code smell:** This is coordination, not state protection. A channel is far cleaner:
```go
phase1Done := make(chan struct{})
go func() {
    doPhase1()
    close(phase1Done)
}()
<-phase1Done // blocks cleanly, no polling
```

### Over-Channeling Simple State
Not every shared variable needs a channel. A cache miss counter, a request count, a configuration flag -- these are naturally protected by a mutex or even `sync/atomic`. Using channels for them adds unnecessary goroutines and complexity.

## Review

Both mutexes and channels are valid concurrency tools, and the lesson's two
scenarios show the split cleanly. A mutex excels at protecting the internal state
of a struct with a clear owner -- the metrics map, a counter, a cache -- where
locking is a small, self-contained detail; forced into channels, the same
feature grows an operation struct, a response channel, a background goroutine,
and a Close method for no gain. Channels excel at moving data between stages and
coordinating phases of work -- the fetch/resize/write pipeline connects
naturally, while the mutex version degenerates into polling loops and manual done
flags. Using a channel as a lock, or a mutex to coordinate a pipeline, are the
two code smells to watch for; the proverb is guidance toward the clearer choice,
not dogma.

To make the distinction your own, build a concurrent log aggregator twice. In the
mutex version, many goroutines append entries to a shared slice under a lock
while a flush goroutine periodically reads and clears it; in the channel version,
goroutines send entries to a single writer goroutine that batches and flushes
them. Compare the two for clarity and correctness and decide which reads as
intent for this problem -- and when in doubt, the rule of thumb holds: if a
struct owns the data, use a mutex; if goroutines pass data, use a channel.

## Resources
- [Go Wiki: MutexOrChannel](https://go.dev/wiki/MutexOrChannel) -- the canonical decision guide for exactly this choice.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- goroutines, channels, and when shared memory is the simpler tool.
- [Go Proverbs: Rob Pike](https://go-proverbs.github.io/) -- the source of "share memory by communicating" and its intended nuance.
- [Bryan Mills - Rethinking Classical Concurrency Patterns](https://www.youtube.com/watch?v=5zXAHh5tJqQ) -- a deeper look at when channel patterns help and when they hurt.

---

Back to [Concurrency](../../concurrency.md) | Next: [08-nested-locking-deadlock](../08-nested-locking-deadlock/08-nested-locking-deadlock.md)
