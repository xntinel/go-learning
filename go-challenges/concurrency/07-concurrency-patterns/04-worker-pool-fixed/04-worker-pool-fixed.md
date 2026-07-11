# Exercise 4: Worker Pool (Fixed)

The worker pool is the most widely used concurrency pattern in Go. It fuses
fan-out and fan-in into one structured unit: a fixed number of worker goroutines
pull jobs from a shared queue, process them, and push results into a collection
channel. The reason it matters is resource control -- without it, a thousand
uploads spawn a thousand goroutines all fighting for the same CPU cores, and the
context-switching and memory pressure make everything slower. A pool of N caps
concurrency at N, and when every worker is busy the submitter simply blocks,
which is backpressure: the system refuses to accept more work than it can
actually process.

## What you'll build

```text
04-worker-pool-fixed/
  main.go        typed Job/Result structs, a fixed pool over a jobs
                 channel, a throughput benchmark, and a backpressure demo
```

- Build: a fixed-size thumbnail worker pool with separate job and result channels.
- Implement: `ImageJob`/`ThumbnailResult` types; `ThumbnailPool` with a `worker` loop and a `Run`/`ProcessImages` that closes `jobs`, waits, and closes `results`; a `PoolBenchmark` across pool sizes; and a `BackpressureDemo`.
- Verify: `go run main.go`

### Why a fixed pool gives you backpressure

Consider a real scenario: your service handles user-uploaded images that need
thumbnail generation. Each resize operation takes 200-500ms of CPU time. Without
a pool, 1000 concurrent uploads would spawn 1000 goroutines all competing for
CPU cores, causing context switching overhead and memory pressure. A fixed pool
of N workers (where N matches your CPU count) caps resource usage while keeping
all cores busy. When all workers are occupied, new submissions block -- this is
backpressure, preventing your system from accepting more work than it can
handle.

```
  Image Thumbnail Worker Pool

  +--------+    +------+    +---------+
  |uploads | -> | jobs | -> | worker1 | --+
  +--------+    | chan  | -> | worker2 | --+--> results chan --> storage
                |      | -> | worker3 | --+
                |      | -> | worker4 | --+
                +------+    +---------+

  Flow: submit jobs -> close jobs -> workers drain queue
  -> workers exit -> WaitGroup zero -> close results
  -> collector finishes
```

## Step 1 -- Define Job and Result Types

Start by defining structured types for the image resize jobs and results. This makes the pool type-safe and traceable.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const thumbnailScaleFactor = 4

// ImageJob represents an image resize request.
type ImageJob struct {
	ID       int
	Filename string
	Width    int
	Height   int
}

// ThumbnailResult holds the outcome of processing a single image.
type ThumbnailResult struct {
	Job      ImageJob
	Output   string
	Duration time.Duration
	WorkerID int
	Error    error
}

// ThumbnailPool manages a pool of thumbnail workers.
type ThumbnailPool struct {
	numWorkers int
}

func NewThumbnailPool(numWorkers int) *ThumbnailPool {
	return &ThumbnailPool{numWorkers: numWorkers}
}

func generateOutputName(job ImageJob) string {
	return fmt.Sprintf("thumb_%dx%d_%s",
		job.Width/thumbnailScaleFactor,
		job.Height/thumbnailScaleFactor,
		job.Filename)
}

func (tp *ThumbnailPool) worker(id int, jobs <-chan ImageJob, results chan<- ThumbnailResult) {
	for job := range jobs {
		start := time.Now()
		workTime := time.Duration(50+rand.IntN(100)) * time.Millisecond
		time.Sleep(workTime)

		results <- ThumbnailResult{
			Job:      job,
			Output:   generateOutputName(job),
			Duration: time.Since(start),
			WorkerID: id,
		}
	}
}

func (tp *ThumbnailPool) ProcessImages(images []ImageJob) []ThumbnailResult {
	jobs := make(chan ImageJob, len(images))
	results := make(chan ThumbnailResult, len(images))

	var wg sync.WaitGroup
	for w := 1; w <= tp.numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tp.worker(id, jobs, results)
		}(w)
	}

	for _, img := range images {
		jobs <- img
	}
	close(jobs)

	wg.Wait()
	close(results)

	var collected []ThumbnailResult
	for r := range results {
		collected = append(collected, r)
	}
	return collected
}

func main() {
	images := []ImageJob{
		{ID: 1, Filename: "vacation.jpg", Width: 4000, Height: 3000},
		{ID: 2, Filename: "profile.png", Width: 2000, Height: 2000},
		{ID: 3, Filename: "banner.jpg", Width: 6000, Height: 2000},
	}

	pool := NewThumbnailPool(1)
	results := pool.ProcessImages(images)

	fmt.Println("=== Single Worker Test ===")
	for _, r := range results {
		fmt.Printf("  %s -> %s (%v)\n", r.Job.Filename, r.Output, r.Duration)
	}
}
```

Each `ThumbnailResult` carries the original job, the output filename, timing, and which worker processed it. This traceability is invaluable for debugging and monitoring in production.

### Verification
```bash
go run main.go
```
A single worker processes all jobs sequentially:
```
=== Single Worker Test ===
  vacation.jpg -> thumb_1000x750_vacation.jpg (87ms)
  profile.png -> thumb_500x500_profile.png (112ms)
  banner.jpg -> thumb_1500x500_banner.jpg (65ms)
```

## Step 2 -- Build the Pool

Now create the full pool: launch N workers, send jobs, and collect results. This is where the concurrency benefit becomes visible.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	poolWorkerCount    = 4
	poolImageCount     = 12
	thumbnailScaleFactor = 4
)

// ImageJob represents an image resize request.
type ImageJob struct {
	ID       int
	Filename string
	Width    int
	Height   int
}

// ThumbnailResult holds the outcome of processing a single image.
type ThumbnailResult struct {
	Job      ImageJob
	Output   string
	Duration time.Duration
	WorkerID int
}

// ThumbnailPool manages a fixed set of thumbnail workers.
type ThumbnailPool struct {
	numWorkers int
}

func NewThumbnailPool(numWorkers int) *ThumbnailPool {
	return &ThumbnailPool{numWorkers: numWorkers}
}

func generateOutputName(job ImageJob) string {
	return fmt.Sprintf("thumb_%dx%d_%s",
		job.Width/thumbnailScaleFactor,
		job.Height/thumbnailScaleFactor,
		job.Filename)
}

func (tp *ThumbnailPool) worker(id int, jobs <-chan ImageJob, results chan<- ThumbnailResult) {
	for job := range jobs {
		start := time.Now()
		time.Sleep(time.Duration(50+rand.IntN(100)) * time.Millisecond)
		results <- ThumbnailResult{
			Job: job, Output: generateOutputName(job),
			Duration: time.Since(start), WorkerID: id,
		}
	}
}

func (tp *ThumbnailPool) Run(images []ImageJob) {
	jobs := make(chan ImageJob, len(images))
	results := make(chan ThumbnailResult, len(images))

	var wg sync.WaitGroup
	for w := 1; w <= tp.numWorkers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tp.worker(id, jobs, results)
		}(w)
	}

	for _, img := range images {
		jobs <- img
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		fmt.Printf("  [worker %d] %s -> %s (%v)\n",
			r.WorkerID, r.Job.Filename, r.Output, r.Duration)
	}
}

func buildTestImages(count int) []ImageJob {
	images := make([]ImageJob, count)
	for i := range images {
		images[i] = ImageJob{
			ID:       i + 1,
			Filename: fmt.Sprintf("photo_%02d.jpg", i+1),
			Width:    2000 + rand.IntN(4000),
			Height:   1500 + rand.IntN(3000),
		}
	}
	return images
}

func main() {
	images := buildTestImages(poolImageCount)

	fmt.Printf("=== Thumbnail Pool (%d workers, %d images) ===\n", poolWorkerCount, poolImageCount)
	start := time.Now()

	pool := NewThumbnailPool(poolWorkerCount)
	pool.Run(images)

	fmt.Printf("  Completed in %v\n", time.Since(start))
}
```

### Verification
```bash
go run main.go
```
Expected: all 12 images processed, distributed across 4 workers:
```
=== Thumbnail Pool (4 workers, 12 images) ===
  [worker 2] photo_01.jpg -> thumb_750x500_photo_01.jpg (67ms)
  [worker 4] photo_03.jpg -> thumb_1250x875_photo_03.jpg (95ms)
  ...
  Completed in 310ms
```

## Step 3 -- Measure Pool Performance

Compare execution time with different pool sizes to see the concurrency benefit and observe diminishing returns.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const benchmarkImageCount = 24

// ImageJob represents an image resize request.
type ImageJob struct {
	ID       int
	Filename string
	Width    int
	Height   int
}

// ThumbnailResult holds the outcome of processing a single image.
type ThumbnailResult struct {
	Job      ImageJob
	Duration time.Duration
	WorkerID int
}

// PoolBenchmark measures throughput at different pool sizes.
type PoolBenchmark struct {
	imageCount int
	poolSizes  []int
}

func NewPoolBenchmark(poolSizes []int) *PoolBenchmark {
	return &PoolBenchmark{
		imageCount: benchmarkImageCount,
		poolSizes:  poolSizes,
	}
}

func (pb *PoolBenchmark) worker(id int, jobs <-chan ImageJob, results chan<- ThumbnailResult) {
	for job := range jobs {
		start := time.Now()
		time.Sleep(time.Duration(50+rand.IntN(100)) * time.Millisecond)
		results <- ThumbnailResult{Job: job, Duration: time.Since(start), WorkerID: id}
	}
}

func (pb *PoolBenchmark) buildImages() []ImageJob {
	images := make([]ImageJob, pb.imageCount)
	for i := range images {
		images[i] = ImageJob{
			ID: i + 1, Filename: fmt.Sprintf("img_%02d.jpg", i+1),
			Width: 3000, Height: 2000,
		}
	}
	return images
}

func (pb *PoolBenchmark) measurePoolSize(poolSize int, images []ImageJob) (time.Duration, time.Duration) {
	start := time.Now()
	jobs := make(chan ImageJob, len(images))
	results := make(chan ThumbnailResult, len(images))

	var wg sync.WaitGroup
	for w := 0; w < poolSize; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			pb.worker(id, jobs, results)
		}(w)
	}

	for _, img := range images {
		jobs <- img
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	var totalWork time.Duration
	for r := range results {
		totalWork += r.Duration
	}

	return time.Since(start), totalWork
}

func (pb *PoolBenchmark) Run() {
	fmt.Println("=== Pool Size vs Throughput ===")
	fmt.Printf("  Processing %d images\n\n", pb.imageCount)

	for _, poolSize := range pb.poolSizes {
		images := pb.buildImages()
		elapsed, totalWork := pb.measurePoolSize(poolSize, images)
		efficiency := float64(totalWork) / float64(elapsed*time.Duration(poolSize)) * 100
		fmt.Printf("  %2d workers: %v (total CPU work: %v, efficiency: %.0f%%)\n",
			poolSize, elapsed, totalWork, efficiency)
	}
}

func main() {
	benchmark := NewPoolBenchmark([]int{1, 2, 4, 8, 12, 24})
	benchmark.Run()
}
```

### Verification
```bash
go run main.go
```
More workers reduce wall-clock time, but notice diminishing returns:
```
=== Pool Size vs Throughput ===
  Processing 24 images

   1 workers: 2.1s  (total CPU work: 2.1s, efficiency: 100%)
   2 workers: 1.05s (total CPU work: 2.1s, efficiency: 100%)
   4 workers: 540ms (total CPU work: 2.1s, efficiency: 97%)
   8 workers: 280ms (total CPU work: 2.1s, efficiency: 94%)
  12 workers: 195ms (total CPU work: 2.1s, efficiency: 89%)
  24 workers: 120ms (total CPU work: 2.1s, efficiency: 72%)
```

## Step 4 -- Observe Backpressure

Demonstrate what happens when the job queue is small and all workers are busy: the producer blocks, providing natural backpressure.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	backpressureWorkers   = 2
	backpressureBuffer    = 1
	backpressureJobCount  = 8
	backpressureThreshold = 5 * time.Millisecond
)

// ImageJob represents an image resize request.
type ImageJob struct {
	ID       int
	Filename string
}

// BackpressureDemo shows how a tiny buffer causes the producer to block.
type BackpressureDemo struct {
	numWorkers int
	bufferSize int
	jobCount   int
}

func NewBackpressureDemo() *BackpressureDemo {
	return &BackpressureDemo{
		numWorkers: backpressureWorkers,
		bufferSize: backpressureBuffer,
		jobCount:   backpressureJobCount,
	}
}

func (bd *BackpressureDemo) worker(id int, jobs <-chan ImageJob, wg *sync.WaitGroup) {
	defer wg.Done()
	for job := range jobs {
		fmt.Printf("  [worker %d] processing %s\n", id, job.Filename)
		time.Sleep(time.Duration(100+rand.IntN(100)) * time.Millisecond)
		fmt.Printf("  [worker %d] finished  %s\n", id, job.Filename)
	}
}

func (bd *BackpressureDemo) submitJobs(jobs chan<- ImageJob) {
	for i := 1; i <= bd.jobCount; i++ {
		job := ImageJob{ID: i, Filename: fmt.Sprintf("upload_%02d.jpg", i)}
		fmt.Printf("  [producer] submitting %s...\n", job.Filename)
		submitStart := time.Now()
		jobs <- job
		if wait := time.Since(submitStart); wait > backpressureThreshold {
			fmt.Printf("  [producer] blocked for %v (backpressure!)\n", wait)
		}
	}
	close(jobs)
}

func (bd *BackpressureDemo) Run() {
	fmt.Printf("=== Backpressure Demo (%d workers, buffer=%d) ===\n", bd.numWorkers, bd.bufferSize)
	fmt.Println("  Producer blocks when both workers are busy and buffer is full")
	fmt.Println()

	jobs := make(chan ImageJob, bd.bufferSize)
	var wg sync.WaitGroup

	for w := 1; w <= bd.numWorkers; w++ {
		wg.Add(1)
		go bd.worker(w, jobs, &wg)
	}

	bd.submitJobs(jobs)
	wg.Wait()
	fmt.Println("\n  All images processed")
}

func main() {
	demo := NewBackpressureDemo()
	demo.Run()
}
```

### Verification
```bash
go run main.go
```
Expected: you see the producer blocking when workers are busy:
```
=== Backpressure Demo (2 workers, buffer=1) ===
  Producer blocks when both workers are busy and buffer is full

  [producer] submitting upload_01.jpg...
  [producer] submitting upload_02.jpg...
  [producer] submitting upload_03.jpg...
  [worker 1] processing upload_01.jpg
  [worker 2] processing upload_02.jpg
  [producer] submitting upload_04.jpg...
  [producer] blocked for 105ms (backpressure!)
  ...
```

## Common Mistakes

### Sending Results After the Channel is Closed
**Wrong:**
```go
go func() {
	wg.Wait()
	close(results)
}()
// Worker still running, sends to closed results -> panic
```
**What happens:** If the WaitGroup is not properly coordinated, a worker might try to send after results is closed.

**Fix:** Ensure every worker calls `wg.Done()` only after it has finished all sends. The `defer wg.Done()` at the top of the worker goroutine guarantees this, since `range jobs` exits only when jobs is closed and drained.

### Forgetting to Buffer the Channels
**Wrong:**
```go
jobs := make(chan ImageJob)       // unbuffered
results := make(chan ThumbnailResult) // unbuffered
```
**What happens:** With unbuffered channels, the sender blocks until a receiver is ready. If you try to send all jobs before collecting results, you deadlock (job send blocks because no worker can receive because it's blocked trying to send a result).

**Fix:** Buffer at least one of the channels, or send jobs and collect results concurrently.

### Pool Size of Zero
Always validate that the number of workers is at least 1. A pool with zero workers means nobody reads from the jobs channel, causing a deadlock.

## Review

A worker pool is a fixed set of goroutines all reading from one shared jobs
channel, with a separate results channel so input and output stay cleanly apart.
Typed `Job` and `Result` structs keep it type-safe and traceable -- each result
can carry which worker produced it and how long it took. The lifecycle is the
part worth internalizing: close the jobs channel to tell workers to stop, let
each worker's `range jobs` drain and exit, and only then close the results
channel -- which is why the close runs inside `go func(){ wg.Wait();
close(results) }()`, after every `wg.Done()`. Buffer the channels so a burst of
sends does not deadlock against a collector that has not started receiving yet.
The payoff is bounded concurrency with natural backpressure, and the tuning rule
is to match pool size to the bottleneck: CPU cores for CPU-bound work, connection
or rate limits for I/O-bound work, past which more workers only add overhead.

Run `go run main.go` and confirm the shape of each result: a single worker
processes the three images sequentially; four workers over twelve images collect
every result and finish roughly three times faster; the benchmark shows wall
time dropping as workers rise and then flattening into diminishing returns; and
the backpressure demo shows the producer visibly blocking once both workers are
busy and the tiny buffer is full. If you can explain why the producer blocks --
and why closing results before `wg.Wait()` would panic a still-running worker --
the pattern is yours.

## Resources
- [Go by Example: Worker Pools](https://gobyexample.com/worker-pools) -- a minimal jobs/results worker pool to compare against.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- how pools compose into larger pipelines and shut down cleanly.
- [Effective Go: Parallelization](https://go.dev/doc/effective_go#parallel) -- the language guide's take on splitting work across cores.

---

Back to [Concurrency](../../concurrency.md) | Next: [05-semaphore-bounded-concurrency](../05-semaphore-bounded-concurrency/05-semaphore-bounded-concurrency.md)
