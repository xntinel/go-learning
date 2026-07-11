# Exercise 9: Buffered Channel as Semaphore

A file downloader has 50 files to fetch from a CDN. One goroutine per file is
cheap in Go, but 50 simultaneous HTTP connections can overwhelm the server,
exhaust file descriptors, or trip rate limiting. You need a ceiling -- say three
downloads at once -- and Go gives you one without any OS primitive: a buffered
channel. Its capacity is the limit, a send is an acquire, a receive is a
release, and a full buffer blocks the next goroutine until a slot frees. This
exercise builds that semaphore and proves the cap holds.

## What you'll build

```text
09-buffered-channel-as-semaphore/
  main.go        concurrency-limited downloader: a Semaphore over a buffered
                 channel, a peak-concurrency tracker, and progress reporting
```

- Build: a file downloader that caps how many transfers run at once.
- Implement: a `Semaphore` type with `Acquire`/`Release` over `chan struct{}`, a `ConcurrencyTracker` that records peak concurrency with atomics, and a `FileDownloader` that reports progress as each file lands.
- Verify: `go run main.go`.

### Why a buffered channel is a semaphore

A buffered channel makes a natural semaphore. Create a channel with capacity N
-- that is your concurrency limit. Before starting a download, send a value into
the channel (acquire a slot). If N goroutines are already downloading, the
channel is full and the send blocks -- the goroutine waits its turn. When a
download finishes, receive from the channel (release the slot), making room for
the next goroutine.

This is lighter than OS semaphores and integrates naturally with Go's
channel-based concurrency.

## Step 1 -- The Problem: Unlimited Concurrent Downloads

Launch 10 downloads simultaneously with no limit. All 10 start at the same time, which would overwhelm the target server in production.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const simulatedDownloadTime = 300 * time.Millisecond

// downloadFile simulates downloading a single file.
func downloadFile(filename string, startedAt time.Time) {
	fmt.Printf("[%s] downloading %s...\n",
		time.Since(startedAt).Round(time.Millisecond), filename)
	time.Sleep(simulatedDownloadTime)
}

// downloadAllUnlimited launches one goroutine per file with no concurrency limit.
func downloadAllUnlimited(files []string) time.Duration {
	var wg sync.WaitGroup
	start := time.Now()

	for _, file := range files {
		wg.Add(1)
		go func(filename string) {
			defer wg.Done()
			downloadFile(filename, start)
		}(file)
	}

	wg.Wait()
	return time.Since(start).Round(time.Millisecond)
}

func main() {
	files := []string{
		"report-q1.pdf", "report-q2.pdf", "report-q3.pdf", "report-q4.pdf",
		"data-jan.csv", "data-feb.csv", "data-mar.csv",
		"image-logo.png", "image-banner.png", "image-hero.png",
	}

	elapsed := downloadAllUnlimited(files)
	fmt.Printf("All done in %v (all 10 ran simultaneously -- too many connections!)\n", elapsed)
}
```

### Verification
```bash
go run main.go
# Expected: all 10 downloads start at +0ms, total ~300ms
# In production, this could exhaust connections or trigger rate limiting
```

## Step 2 -- Add a Semaphore: Limit to 3 Concurrent Downloads

Create a buffered channel of capacity 3. Before downloading, acquire a slot. After finishing, release it. Now only 3 files download at any time.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	concurrencyLimit      = 3
	semDownloadSimulation = 300 * time.Millisecond
)

// Semaphore limits the number of concurrent operations using a buffered channel.
type Semaphore struct {
	slots chan struct{}
}

// NewSemaphore creates a semaphore with the given capacity.
func NewSemaphore(maxConcurrent int) *Semaphore {
	return &Semaphore{slots: make(chan struct{}, maxConcurrent)}
}

// Acquire blocks until a slot is available.
func (s *Semaphore) Acquire() { s.slots <- struct{}{} }

// Release frees a slot. Always defer this after Acquire.
func (s *Semaphore) Release() { <-s.slots }

// downloadWithSemaphore simulates a download while respecting the semaphore limit.
func downloadWithSemaphore(sem *Semaphore, filename string, startedAt time.Time) {
	sem.Acquire()
	defer sem.Release()

	elapsed := time.Since(startedAt).Round(time.Millisecond)
	fmt.Printf("[+%6s] downloading %s\n", elapsed, filename)
	time.Sleep(semDownloadSimulation)
}

func main() {
	files := []string{
		"report-q1.pdf", "report-q2.pdf", "report-q3.pdf", "report-q4.pdf",
		"data-jan.csv", "data-feb.csv", "data-mar.csv",
		"image-logo.png", "image-banner.png", "image-hero.png",
	}

	sem := NewSemaphore(concurrencyLimit)
	var wg sync.WaitGroup
	start := time.Now()

	for _, file := range files {
		wg.Add(1)
		go func(filename string) {
			defer wg.Done()
			downloadWithSemaphore(sem, filename, start)
		}(file)
	}

	wg.Wait()
	fmt.Printf("All done in %v (max %d concurrent downloads)\n",
		time.Since(start).Round(time.Millisecond), concurrencyLimit)
}
```

Now downloads start in batches of 3. The remaining 7 goroutines wait their turn. Total time is approximately `ceil(10/3) * 300ms = ~1.2s` instead of 300ms, but the server is not overwhelmed.

### Verification
```bash
go run main.go
# Expected: downloads start in batches of ~3, total ~1.2s
```

## Step 3 -- Verify the Concurrency Limit

Track the number of active downloads to prove the semaphore works. This is the kind of verification you would add to an integration test.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	verifyFileCount      = 15
	verifyConcurrency    = 3
	verifyDownloadTime   = 200 * time.Millisecond
)

// ConcurrencyTracker tracks active and peak concurrent goroutines using atomics.
type ConcurrencyTracker struct {
	active atomic.Int32
	peak   atomic.Int32
}

// Enter increments the active count and updates the peak if needed.
func (ct *ConcurrencyTracker) Enter() int32 {
	current := ct.active.Add(1)
	for {
		old := ct.peak.Load()
		if current <= old || ct.peak.CompareAndSwap(old, current) {
			break
		}
	}
	return current
}

// Exit decrements the active count.
func (ct *ConcurrencyTracker) Exit() { ct.active.Add(-1) }

// Peak returns the highest observed concurrency.
func (ct *ConcurrencyTracker) Peak() int32 { return ct.peak.Load() }

// trackedDownload simulates a download while recording concurrency metrics.
func trackedDownload(sem *Semaphore, tracker *ConcurrencyTracker, filename string, startedAt time.Time) {
	sem.Acquire()
	defer sem.Release()

	active := tracker.Enter()
	defer tracker.Exit()

	elapsed := time.Since(startedAt).Round(time.Millisecond)
	fmt.Printf("[+%6s] downloading %-15s (active: %d)\n", elapsed, filename, active)
	time.Sleep(verifyDownloadTime)
}

// Semaphore limits the number of concurrent operations using a buffered channel.
type Semaphore struct {
	slots chan struct{}
}

func NewSemaphore(maxConcurrent int) *Semaphore {
	return &Semaphore{slots: make(chan struct{}, maxConcurrent)}
}

func (s *Semaphore) Acquire() { s.slots <- struct{}{} }
func (s *Semaphore) Release() { <-s.slots }

func main() {
	files := make([]string, verifyFileCount)
	for i := range files {
		files[i] = fmt.Sprintf("file-%02d.dat", i+1)
	}

	sem := NewSemaphore(verifyConcurrency)
	tracker := &ConcurrencyTracker{}
	var wg sync.WaitGroup
	start := time.Now()

	for _, file := range files {
		wg.Add(1)
		go func(filename string) {
			defer wg.Done()
			trackedDownload(sem, tracker, filename, start)
		}(file)
	}

	wg.Wait()
	peak := tracker.Peak()
	fmt.Printf("\nPeak concurrent downloads: %d (limit was %d)\n", peak, verifyConcurrency)

	if peak <= int32(verifyConcurrency) {
		fmt.Println("PASS: concurrency limit respected")
	} else {
		fmt.Println("FAIL: concurrency limit exceeded!")
	}
}
```

### Verification
```bash
go run main.go
# Expected: peak concurrent never exceeds 3, PASS at the end
```

## Step 4 -- Comparing With and Without Semaphore

Side-by-side comparison showing the real consequence of not using a semaphore: all connections open at once versus controlled batching.

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	comparisonFileCount   = 12
	comparisonDownloadTime = 200 * time.Millisecond
	safeLimit             = 3
)

// DownloadBenchmark runs a batch of downloads with a concurrency limit,
// returning the elapsed time and peak observed concurrency.
type DownloadBenchmark struct {
	files       []string
	maxParallel int
}

// Run executes the benchmark and returns elapsed time and peak concurrency.
func (db *DownloadBenchmark) Run() (time.Duration, int32) {
	sem := make(chan struct{}, db.maxParallel)
	var wg sync.WaitGroup
	var peak atomic.Int32
	var active atomic.Int32
	start := time.Now()

	for _, file := range db.files {
		wg.Add(1)
		go func(filename string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			cur := active.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(comparisonDownloadTime)
			active.Add(-1)
		}(file)
	}

	wg.Wait()
	return time.Since(start).Round(time.Millisecond), peak.Load()
}

func generateFileList(count int) []string {
	files := make([]string, count)
	for i := range files {
		files[i] = fmt.Sprintf("file-%02d.dat", i+1)
	}
	return files
}

func main() {
	files := generateFileList(comparisonFileCount)

	unlimited := &DownloadBenchmark{files: files, maxParallel: comparisonFileCount}
	elapsed, peak := unlimited.Run()
	fmt.Printf("Unlimited:  %v, peak connections: %d (server overloaded!)\n", elapsed, peak)

	limited := &DownloadBenchmark{files: files, maxParallel: safeLimit}
	elapsed, peak = limited.Run()
	fmt.Printf("Limited(%d): %v, peak connections: %d (safe)\n", safeLimit, elapsed, peak)
}
```

### Verification
```bash
go run main.go
# Expected:
#   Unlimited:  ~200ms, peak connections: 12 (server overloaded!)
#   Limited(3): ~800ms, peak connections: 3 (safe)
```

## Step 5 -- Downloader with Progress Reporting

A production-ready downloader that limits concurrency and reports progress. This combines the semaphore with channel-based result collection.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	downloaderConcurrency = 3
	minDownloadLatency    = 100
	maxExtraLatency       = 300
	minFileSizeKB         = 1024
	maxExtraSizeKB        = 10240
)

// DownloadResult records the outcome of a single file download.
type DownloadResult struct {
	File     string
	Size     int
	Duration time.Duration
}

// FileDownloader manages concurrent downloads with a semaphore.
type FileDownloader struct {
	sem     *Semaphore
	results chan DownloadResult
	wg      sync.WaitGroup
}

// Semaphore limits the number of concurrent operations using a buffered channel.
type Semaphore struct {
	slots chan struct{}
}

func NewSemaphore(maxConcurrent int) *Semaphore {
	return &Semaphore{slots: make(chan struct{}, maxConcurrent)}
}

func (s *Semaphore) Acquire() { s.slots <- struct{}{} }
func (s *Semaphore) Release() { <-s.slots }

// NewFileDownloader creates a downloader with the given concurrency limit.
func NewFileDownloader(maxConcurrent int, fileCount int) *FileDownloader {
	return &FileDownloader{
		sem:     NewSemaphore(maxConcurrent),
		results: make(chan DownloadResult, fileCount),
	}
}

// Download simulates downloading a file and sends the result.
func (fd *FileDownloader) Download(filename string) {
	fd.wg.Add(1)
	go func() {
		defer fd.wg.Done()
		fd.sem.Acquire()
		defer fd.sem.Release()

		dlStart := time.Now()
		latency := time.Duration(minDownloadLatency+rand.IntN(maxExtraLatency)) * time.Millisecond
		time.Sleep(latency)

		sizeKB := minFileSizeKB + rand.IntN(maxExtraSizeKB)
		fd.results <- DownloadResult{
			File:     filename,
			Size:     sizeKB,
			Duration: time.Since(dlStart).Round(time.Millisecond),
		}
	}()
}

// CloseResultsWhenDone waits for all downloads and closes the results channel.
func (fd *FileDownloader) CloseResultsWhenDone() {
	go func() {
		fd.wg.Wait()
		close(fd.results)
	}()
}

// Results returns the channel to range over for completed downloads.
func (fd *FileDownloader) Results() <-chan DownloadResult {
	return fd.results
}

func main() {
	files := []string{
		"report-2024-q1.pdf", "report-2024-q2.pdf", "report-2024-q3.pdf",
		"dataset-users.csv", "dataset-orders.csv",
		"backup-db-full.tar.gz", "backup-db-incremental.tar.gz",
		"config-prod.yaml", "config-staging.yaml",
	}

	downloader := NewFileDownloader(downloaderConcurrency, len(files))
	start := time.Now()

	for _, file := range files {
		downloader.Download(file)
	}
	downloader.CloseResultsWhenDone()

	totalSizeKB := 0
	downloaded := 0
	for res := range downloader.Results() {
		downloaded++
		totalSizeKB += res.Size
		fmt.Printf("  [%d/%d] %-35s %5d KB in %v\n",
			downloaded, len(files), res.File, res.Size, res.Duration)
	}

	fmt.Printf("\nCompleted: %d files, %d KB total, %v elapsed (max %d concurrent)\n",
		downloaded, totalSizeKB, time.Since(start).Round(time.Millisecond), downloaderConcurrency)
}
```

### Verification
```bash
go run main.go
# Expected: files download in batches of 3, with progress reported as each completes
```

## Verification

Run the programs and confirm:
1. Without the semaphore, all goroutines start simultaneously
2. With the semaphore, peak concurrency never exceeds the limit
3. Total time increases proportionally to the number of batches
4. The `defer` release ensures slots are freed even if work panics

## Common Mistakes

### Forgetting to Release the Semaphore

**Wrong:**
```go
sem <- struct{}{}
doWork() // if this panics, the slot is never released!
<-sem    // never reached
```

**What happens:** One slot is permanently consumed. Eventually all slots are stuck and no new work can start. The program hangs.

**Fix:** Always use `defer` immediately after acquiring:
```go
sem <- struct{}{}
defer func() { <-sem }()
doWork() // even if this panics, defer releases the slot
```

### Using the Semaphore Backwards

**Wrong:**
```go
sem := make(chan struct{}, 3)
<-sem        // receive first -- blocks forever on empty channel!
defer func() { sem <- struct{}{} }()
```

**What happens:** The "acquire" receive blocks because the channel starts empty. No goroutine ever proceeds.

**Fix:** Send to acquire (fills buffer), receive to release (drains buffer):
```go
sem <- struct{}{}        // acquire: fill a slot
defer func() { <-sem }() // release: drain a slot
```

## Review

A semaphore in Go needs no library: `make(chan struct{}, N)` is a counting
semaphore whose capacity N is the concurrency limit. Sending an empty struct
acquires a slot and blocks when all N are taken; receiving releases one.
Wrapping that in an `Acquire`/`Release` pair and deferring the release right
after acquiring is what keeps a slot from leaking when the work panics -- forget
the release and the pool drains one slot at a time until the program wedges. The
buffer's capacity, not the number of goroutines you launch, is what actually
bounds the work, which is why a semaphore protects rate-limited resources -- API
connection caps, file descriptors, database pools -- where you want to launch a
goroutine per task but run only a few at a time.

You should be able to explain why the channel's buffer capacity equals the
concurrency limit rather than some larger number, what happens to a
permanently-held slot when a download panics without a deferred release, and when
a semaphore is the right tool instead of simply launching fewer goroutines -- the
answer being whenever the work is per-task and short-lived rather than a fixed
pool of long-running workers.

## Resources
- [Effective Go: Channels as semaphores](https://go.dev/doc/effective_go#channels) -- the language guide's own channels-as-semaphores example.
- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) -- the standard weighted semaphore, for when slots carry different costs.
- [Go Blog: Bounded concurrency](https://go.dev/blog/pipelines) -- how bounding concurrency fits into pipeline design.

---

Back to [Concurrency](../../concurrency.md) | Next: [10-channel-vs-shared-memory](../10-channel-vs-shared-memory/10-channel-vs-shared-memory.md)
