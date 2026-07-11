# Exercise 2: Fan-Out: Distribute Work

Fan-out distributes work from a single source to many goroutines, and it is one
of the most natural patterns in Go because of how channels behave: when several
goroutines receive from the same channel, the runtime hands each value to
exactly one of them. No duplication, no external coordination -- the channel
itself is a thread-safe work queue. This exercise puts that to work on a health
checker: 20 service URLs that take roughly 20 seconds to check one at a time
drop to about 4 seconds across 5 workers, which can be the difference between
alerting inside your SLA and missing it.

## What you'll build

```text
02-fan-out-distribute-work/
  main.go        a sequential baseline, a worker-pool fan-out, and a
                 speed comparison across worker counts
```

- Build: a URL health checker that fans a list of URLs out to a pool of workers reading from one shared jobs channel.
- Implement: a sequential baseline; a `worker` that ranges over a jobs channel and reports on a results channel; a `WaitGroup`-closed results channel; a benchmark across worker counts.
- Verify: `go run main.go` for each step.

### Why channels distribute work for free

Consider a real scenario: you maintain an infrastructure monitoring system that
checks the health of 20+ service URLs every minute. Checking them sequentially
takes 20 seconds (1 second per URL with timeouts). By fanning out to 5 workers,
each checking URLs concurrently, you reduce the total time to roughly 4 seconds.
The difference between 20 seconds and 4 seconds determines whether your alerts
fire within your SLA.

```
              URL Health Checker - Fan-Out

  +----------+
  | URL list |
  +----+-----+
       |
    jobs channel (20 URLs)
       |
  +----+----+----+----+----+
  |    |    |    |    |    |
  w1   w2   w3   w4   w5   (workers compete for URLs)
  |    |    |    |    |
  +----+----+----+----+
       |
  results channel
       |
  +----------+
  | reporter |
  +----------+
```

## Step 1 -- Sequential URL Checking (The Problem)

First, see how slow sequential checking is. This establishes the baseline that fan-out will improve.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	healthyThreshold = 0.15
	statusOK         = 200
	statusDown       = 503
)

// HealthResult holds the outcome of a single URL check.
type HealthResult struct {
	URL        string
	StatusCode int
	Latency    time.Duration
	Healthy    bool
}

// HealthChecker runs health checks against a list of URLs.
type HealthChecker struct {
	urls []string
}

func NewHealthChecker(urls []string) *HealthChecker {
	return &HealthChecker{urls: urls}
}

func (hc *HealthChecker) checkSingleURL(url string) HealthResult {
	latency := time.Duration(50+rand.IntN(150)) * time.Millisecond
	time.Sleep(latency)

	healthy := rand.Float64() > healthyThreshold
	status := statusOK
	if !healthy {
		status = statusDown
	}

	return HealthResult{
		URL:        url,
		StatusCode: status,
		Latency:    latency,
		Healthy:    healthy,
	}
}

func (hc *HealthChecker) RunSequential() {
	fmt.Println("=== Sequential Health Check (20 URLs) ===")
	start := time.Now()

	var unhealthy int
	for _, url := range hc.urls {
		result := hc.checkSingleURL(url)
		if !result.Healthy {
			unhealthy++
			fmt.Printf("  DOWN: %s (status=%d, latency=%v)\n", result.URL, result.StatusCode, result.Latency)
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("  Total: %d URLs checked, %d unhealthy, took %v\n\n", len(hc.urls), unhealthy, elapsed)
}

func main() {
	urls := []string{
		"https://api.example.com/health",
		"https://auth.example.com/health",
		"https://payments.example.com/health",
		"https://notifications.example.com/health",
		"https://search.example.com/health",
		"https://analytics.example.com/health",
		"https://cdn.example.com/health",
		"https://db-primary.example.com/health",
		"https://db-replica.example.com/health",
		"https://cache.example.com/health",
		"https://queue.example.com/health",
		"https://storage.example.com/health",
		"https://gateway.example.com/health",
		"https://logging.example.com/health",
		"https://metrics.example.com/health",
		"https://scheduler.example.com/health",
		"https://email.example.com/health",
		"https://webhook.example.com/health",
		"https://admin.example.com/health",
		"https://docs.example.com/health",
	}

	checker := NewHealthChecker(urls)
	checker.RunSequential()
}
```

### Verification
```bash
go run main.go
```
Expected: all 20 URLs checked, taking roughly 2-3 seconds total (sequential):
```
=== Sequential Health Check (20 URLs) ===
  DOWN: https://payments.example.com/health (status=503, latency=120ms)
  ...
  Total: 20 URLs checked, 3 unhealthy, took 2.4s
```

## Step 2 -- Fan-Out with Worker Pool

Now distribute the same URLs to N workers that all read from a shared jobs channel. Observe the speed improvement.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	defaultWorkerCount = 5
	healthyThreshold   = 0.15
	statusOK           = 200
	statusDown         = 503
)

// HealthResult holds the outcome of a single URL check.
type HealthResult struct {
	URL        string
	StatusCode int
	Latency    time.Duration
	Healthy    bool
	WorkerID   int
}

// HealthChecker distributes URL checks across a pool of workers.
type HealthChecker struct {
	urls       []string
	numWorkers int
}

func NewHealthChecker(urls []string, numWorkers int) *HealthChecker {
	return &HealthChecker{urls: urls, numWorkers: numWorkers}
}

func simulateHTTPCheck(url string) (int, time.Duration, bool) {
	latency := time.Duration(50+rand.IntN(150)) * time.Millisecond
	time.Sleep(latency)
	healthy := rand.Float64() > healthyThreshold
	status := statusOK
	if !healthy {
		status = statusDown
	}
	return status, latency, healthy
}

func (hc *HealthChecker) worker(id int, urls <-chan string, results chan<- HealthResult, wg *sync.WaitGroup) {
	defer wg.Done()
	for url := range urls {
		status, latency, healthy := simulateHTTPCheck(url)
		results <- HealthResult{
			URL: url, StatusCode: status,
			Latency: latency, Healthy: healthy, WorkerID: id,
		}
	}
}

func (hc *HealthChecker) RunFanOut() {
	fmt.Printf("=== Fan-Out Health Check (%d workers, %d URLs) ===\n", hc.numWorkers, len(hc.urls))
	start := time.Now()

	jobs := make(chan string, len(hc.urls))
	results := make(chan HealthResult, len(hc.urls))

	var wg sync.WaitGroup
	for w := 1; w <= hc.numWorkers; w++ {
		wg.Add(1)
		go hc.worker(w, jobs, results, &wg)
	}

	for _, url := range hc.urls {
		jobs <- url
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	hc.reportResults(results, start)
}

func (hc *HealthChecker) reportResults(results <-chan HealthResult, start time.Time) {
	var unhealthy int
	workerCounts := make(map[int]int)

	for r := range results {
		workerCounts[r.WorkerID]++
		if !r.Healthy {
			unhealthy++
			fmt.Printf("  DOWN: %s (status=%d, latency=%v) [worker %d]\n",
				r.URL, r.StatusCode, r.Latency, r.WorkerID)
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("\n  Total: %d URLs checked, %d unhealthy, took %v\n", len(hc.urls), unhealthy, elapsed)
	fmt.Println("  Work distribution:")
	for id := 1; id <= hc.numWorkers; id++ {
		fmt.Printf("    worker %d: %d URLs\n", id, workerCounts[id])
	}
}

func main() {
	urls := []string{
		"https://api.example.com/health",
		"https://auth.example.com/health",
		"https://payments.example.com/health",
		"https://notifications.example.com/health",
		"https://search.example.com/health",
		"https://analytics.example.com/health",
		"https://cdn.example.com/health",
		"https://db-primary.example.com/health",
		"https://db-replica.example.com/health",
		"https://cache.example.com/health",
		"https://queue.example.com/health",
		"https://storage.example.com/health",
		"https://gateway.example.com/health",
		"https://logging.example.com/health",
		"https://metrics.example.com/health",
		"https://scheduler.example.com/health",
		"https://email.example.com/health",
		"https://webhook.example.com/health",
		"https://admin.example.com/health",
		"https://docs.example.com/health",
	}

	checker := NewHealthChecker(urls, defaultWorkerCount)
	checker.RunFanOut()
}
```

Five workers compete for 20 URLs. Each URL goes to exactly one worker. The distribution is non-deterministic -- Go's scheduler does not guarantee round-robin.

### Verification
```bash
go run main.go
```
Expected: same 20 URLs checked, but roughly 4-5x faster:
```
=== Fan-Out Health Check (5 workers, 20 URLs) ===
  DOWN: https://payments.example.com/health (status=503, latency=95ms) [worker 3]
  ...
  Total: 20 URLs checked, 3 unhealthy, took 520ms
  Work distribution:
    worker 1: 4 URLs
    worker 2: 4 URLs
    worker 3: 4 URLs
    worker 4: 4 URLs
    worker 5: 4 URLs
```

## Step 3 -- Compare Sequential vs Fan-Out

Measure the actual speedup with different worker counts to see diminishing returns.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const totalURLs = 20

// SpeedBenchmark compares fan-out performance across different worker counts.
type SpeedBenchmark struct {
	urls         []string
	workerCounts []int
}

func NewSpeedBenchmark(workerCounts []int) *SpeedBenchmark {
	urls := make([]string, totalURLs)
	for i := range urls {
		urls[i] = fmt.Sprintf("https://service-%02d.example.com/health", i+1)
	}
	return &SpeedBenchmark{urls: urls, workerCounts: workerCounts}
}

func simulateCheck(url string) {
	time.Sleep(time.Duration(50+rand.IntN(150)) * time.Millisecond)
}

func (sb *SpeedBenchmark) measureWithWorkers(numWorkers int) time.Duration {
	start := time.Now()
	jobs := make(chan string, len(sb.urls))
	var wg sync.WaitGroup

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for url := range jobs {
				simulateCheck(url)
			}
		}()
	}

	for _, url := range sb.urls {
		jobs <- url
	}
	close(jobs)
	wg.Wait()

	return time.Since(start)
}

func (sb *SpeedBenchmark) Run() {
	fmt.Println("=== Speed Comparison ===")
	for _, numWorkers := range sb.workerCounts {
		elapsed := sb.measureWithWorkers(numWorkers)
		fmt.Printf("  %2d workers: %v\n", numWorkers, elapsed)
	}
}

func main() {
	benchmark := NewSpeedBenchmark([]int{1, 2, 5, 10, 20})
	benchmark.Run()
}
```

### Verification
```bash
go run main.go
```
Expected: clear improvement up to the number of URLs, then diminishing returns:
```
=== Speed Comparison ===
   1 workers: 2.4s
   2 workers: 1.2s
   5 workers: 520ms
  10 workers: 280ms
  20 workers: 180ms
```

## Common Mistakes

### Not Closing the Jobs Channel
**Wrong:**
```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	jobs := make(chan string, 10)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for url := range jobs { // blocks forever
			fmt.Println("checking", url)
		}
	}()
	jobs <- "https://api.example.com/health"
	jobs <- "https://auth.example.com/health"
	// forgot close(jobs) -- worker blocks on range forever
	wg.Wait() // deadlock
}
```
**What happens:** Workers block on `range jobs` forever after all values are consumed. The program deadlocks.

**Fix:** Always close the channel after sending all values.

### Closing the Results Channel Too Early
**Wrong:**
```go
for w := 0; w < 5; w++ {
	go healthWorker(w, jobs, results, &wg)
}
close(results) // workers haven't finished yet!
```
**What happens:** Workers panic with "send on closed channel".

**Fix:** Use a separate goroutine with `WaitGroup` to close the results channel only after all workers complete.

### Assuming Even Distribution
Go's scheduler does not guarantee round-robin distribution. If one worker is slightly faster (its URL responded quickly), it may grab more jobs. The guarantee is only that each value goes to exactly one receiver.

## Review

Fan-out rests entirely on one channel guarantee: when N goroutines receive from
the same channel, each value goes to exactly one of them. That is what lets you
skip locks and hand-written queues -- you send jobs into a channel, start a pool
of workers that all `range` over it, and the runtime distributes the work. The
distribution is non-deterministic, not round-robin: a worker that finishes its
current job quickly simply grabs the next one, so a fast worker naturally takes
more. Two lifecycle rules make the pipeline terminate cleanly. Close the jobs
channel after the last send, or the workers `range` forever and deadlock. And
close the results channel only after every worker has finished -- which means
launching a separate goroutine that calls `wg.Wait()` and then `close(results)`,
because closing while a worker is still sending panics.

Run the program and read the output as a proof of each claim: the sequential
pass checks all 20 URLs in something over two seconds; the five-worker fan-out
checks the same 20 in roughly a fifth of that; the speed comparison shows a
clear win from one to five workers and diminishing returns past the point where
workers outnumber useful parallelism; and the per-worker counts come out roughly
even but never exactly, which is the non-determinism, not a bug. If the program
hangs instead of finishing, the cause is almost always a jobs channel that was
never closed or a results channel closed too early.

## Resources
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- how fan-out stages compose into larger pipelines and how to shut them down.
- [Go Concurrency Patterns (Rob Pike)](https://www.youtube.com/watch?v=f6kdp27TYZs) -- the talk that introduces fan-out, fan-in, and channel-as-queue thinking.
- [Advanced Go Concurrency Patterns](https://www.youtube.com/watch?v=QDDwwePbDtw) -- follow-up talk on coordinating and cancelling pools of workers.

---

Back to [Concurrency](../../concurrency.md) | Next: [03-fan-in-merge-results](../03-fan-in-merge-results/03-fan-in-merge-results.md)
