# Exercise 3: Errgroup SetLimit -- Bulk File Validator

Your CI pipeline validates a hundred-plus config files before deployment. Launch
a goroutine per file and each opens a descriptor at once -- past the OS limit
(often 1024) you get `open ...: too many open files`, and even below it,
unbounded reads can saturate a shared disk or NFS mount. The fix is bounded
concurrency: a buffered channel of capacity N acts as a token bucket, so a
goroutine acquires a token before working and releases it when done, and the
N+1th blocks until a slot frees. That blocking is natural backpressure. This
exercise builds the semaphore by hand, adds context cancellation, and shows how
`errgroup.SetLimit` packages the same behavior in one line.

## What you'll build

```text
03-errgroup-setlimit/
  main.go        file validator: unbounded vs semaphore-bounded concurrency,
                 context cancellation, and limit-selection guidelines
```

- Build: a bulk config-file validator with bounded concurrency and fail-fast cancellation.
- Implement: an unbounded `RunUnbounded` that peaks at 20 goroutines, a semaphore-bounded `RunBounded` that caps concurrency at 5, a `RunBoundedWithCancellation` that checks `ctx.Done()` before acquiring a token, and a concurrency-limit guideline table.
- Verify: `go run main.go`

### Why bounded concurrency, and how the semaphore delivers it

Even without hitting OS limits, unbounded concurrency wastes resources. If you are validating files on a shared NFS mount, 100 concurrent reads may saturate the I/O bus and slow everything down. Limiting to 5-10 concurrent validations keeps throughput high without resource exhaustion.

The semaphore pattern solves this: a buffered channel of capacity N acts as a token bucket. A goroutine acquires a token before starting work and releases it when done. If all tokens are taken, the next goroutine blocks until one becomes available. This is natural backpressure.

## Step 1 -- Unbounded Concurrency (The Problem)

Observe what happens when 20 file validations run simultaneously with no limit:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	fileValidationLatency = 100 * time.Millisecond
	invalidFilePath       = "config/service-07.json"
)

type FileValidator struct {
	files          []string
	peakConcurrent int
	mu             sync.Mutex
	activeCount    int
}

func NewFileValidator(files []string) *FileValidator {
	return &FileValidator{files: files}
}

func (fv *FileValidator) RunUnbounded() error {
	start := time.Now()

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, f := range fv.files {
		wg.Add(1)
		go func() {
			defer wg.Done()

			current := fv.trackStart()

			fmt.Printf("  [%v] validating %s (concurrent: %d)\n",
				time.Since(start).Round(time.Millisecond), f, current)

			if err := fv.validate(f); err != nil {
				once.Do(func() { firstErr = err })
			}

			fv.trackEnd()
		}()
	}

	wg.Wait()
	fmt.Printf("\nPeak concurrent goroutines: %d\n", fv.peakConcurrent)
	fmt.Printf("Total time: %v\n", time.Since(start).Round(time.Millisecond))
	return firstErr
}

func (fv *FileValidator) trackStart() int {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	fv.activeCount++
	if fv.activeCount > fv.peakConcurrent {
		fv.peakConcurrent = fv.activeCount
	}
	return fv.activeCount
}

func (fv *FileValidator) trackEnd() {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	fv.activeCount--
}

func (fv *FileValidator) validate(path string) error {
	time.Sleep(fileValidationLatency)
	if path == invalidFilePath {
		return fmt.Errorf("validate %s: missing required field 'port'", path)
	}
	return nil
}

func generateFileList(n int) []string {
	files := make([]string, n)
	for i := 0; i < n; i++ {
		files[i] = fmt.Sprintf("config/service-%02d.json", i)
	}
	return files
}

func main() {
	validator := NewFileValidator(generateFileList(20))

	fmt.Println("=== Unbounded Concurrency ===")
	if err := validator.RunUnbounded(); err != nil {
		fmt.Printf("First error: %v\n", err)
	}
}
```

**Expected output:**
```
=== Unbounded Concurrency ===
  [0ms] validating config/service-00.json (concurrent: 1)
  [0ms] validating config/service-01.json (concurrent: 2)
  ...all 20 start at ~0ms...
  [0ms] validating config/service-19.json (concurrent: 20)

Peak concurrent goroutines: 20
Total time: 100ms
First error: validate config/service-07.json: missing required field 'port'
```

All 20 goroutines start simultaneously. With real file I/O on 1000 files, this would open 1000 file descriptors at once. On most systems, `ulimit -n` is 1024 -- you would get `too many open files` errors.

## Step 2 -- Semaphore Channel (The Solution)

Add a buffered channel as a semaphore. Each goroutine must acquire a token (send to the channel) before starting and release it (receive from the channel) when done:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	defaultMaxConcurrent  = 5
	fileValidationLatency = 100 * time.Millisecond
	invalidFilePath       = "config/service-07.json"
)

type FileValidator struct {
	files          []string
	maxConcurrent  int
	peakConcurrent int
	activeCount    int
	mu             sync.Mutex
}

func NewFileValidator(files []string, maxConcurrent int) *FileValidator {
	return &FileValidator{
		files:         files,
		maxConcurrent: maxConcurrent,
	}
}

func (fv *FileValidator) RunBounded() error {
	start := time.Now()

	sem := make(chan struct{}, fv.maxConcurrent)

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	for _, f := range fv.files {

		sem <- struct{}{} // ACQUIRE: blocks if maxConcurrent goroutines are already running

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }() // RELEASE: free the slot when done

			fv.trackStart()

			if err := fv.validate(f); err != nil {
				once.Do(func() { firstErr = err })
			} else {
				fmt.Printf("  [%v] validated %s\n",
					time.Since(start).Round(time.Millisecond), f)
			}

			fv.trackEnd()
		}()
	}

	wg.Wait()
	fmt.Printf("\nPeak concurrent goroutines: %d\n", fv.peakConcurrent)
	fmt.Printf("Total time: %v (ceil(20/5) batches of ~100ms)\n",
		time.Since(start).Round(time.Millisecond))
	return firstErr
}

func (fv *FileValidator) trackStart() {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	fv.activeCount++
	if fv.activeCount > fv.peakConcurrent {
		fv.peakConcurrent = fv.activeCount
	}
}

func (fv *FileValidator) trackEnd() {
	fv.mu.Lock()
	defer fv.mu.Unlock()
	fv.activeCount--
}

func (fv *FileValidator) validate(path string) error {
	time.Sleep(fileValidationLatency)
	if path == invalidFilePath {
		return fmt.Errorf("validate %s: missing required field 'port'", path)
	}
	return nil
}

func generateFileList(n int) []string {
	files := make([]string, n)
	for i := 0; i < n; i++ {
		files[i] = fmt.Sprintf("config/service-%02d.json", i)
	}
	return files
}

func main() {
	validator := NewFileValidator(generateFileList(20), defaultMaxConcurrent)

	fmt.Println("=== Bounded Concurrency (semaphore, limit=5) ===")
	if err := validator.RunBounded(); err != nil {
		fmt.Printf("First error: %v\n", err)
	}
}
```

**Expected output:**
```
=== Bounded Concurrency (semaphore, limit=5) ===
  [100ms] validated config/service-00.json
  [100ms] validated config/service-01.json
  [100ms] validated config/service-02.json
  [100ms] validated config/service-03.json
  [100ms] validated config/service-04.json
  [200ms] validated config/service-05.json
  [200ms] validated config/service-06.json
  ...
  [400ms] validated config/service-19.json

Peak concurrent goroutines: 5
Total time: 400ms (ceil(20/5) batches of ~100ms)
First error: validate config/service-07.json: missing required field 'port'
```

With limit 5 and 20 files of 100ms each: `ceil(20/5) * 100ms = 400ms`. Peak concurrency never exceeds 5. The key is that `sem <- struct{}{}` happens BEFORE the goroutine launch -- this means the semaphore limits goroutine creation, not just goroutine execution.

## Step 3 -- Semaphore + Context Cancellation

In production, you want both: bounded concurrency AND stop-on-first-error. Combine the semaphore with `context.WithCancel`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	defaultMaxConcurrent  = 5
	fileValidationLatency = 100 * time.Millisecond
	invalidFilePath       = "config/service-07.json"
)

type ValidationStats struct {
	Validated int
	Cancelled int
	Total     int
}

type FileValidator struct {
	files         []string
	maxConcurrent int
}

func NewFileValidator(files []string, maxConcurrent int) *FileValidator {
	return &FileValidator{
		files:         files,
		maxConcurrent: maxConcurrent,
	}
}

func (fv *FileValidator) RunBoundedWithCancellation() (ValidationStats, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sem := make(chan struct{}, fv.maxConcurrent)

	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	var stats ValidationStats
	stats.Total = len(fv.files)
	var mu sync.Mutex

	for _, f := range fv.files {

		select {
		case <-ctx.Done():
			mu.Lock()
			stats.Cancelled++
			mu.Unlock()
			continue
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			select {
			case <-ctx.Done():
				mu.Lock()
				stats.Cancelled++
				mu.Unlock()
				return
			default:
			}

			if err := fv.validate(f); err != nil {
				once.Do(func() {
					firstErr = err
					cancel()
				})
				return
			}

			mu.Lock()
			stats.Validated++
			mu.Unlock()
		}()
	}

	wg.Wait()
	return stats, firstErr
}

func (fv *FileValidator) validate(path string) error {
	time.Sleep(fileValidationLatency)
	if path == invalidFilePath {
		return fmt.Errorf("validate %s: missing required field 'port'", path)
	}
	return nil
}

func generateFileList(n int) []string {
	files := make([]string, n)
	for i := 0; i < n; i++ {
		files[i] = fmt.Sprintf("config/service-%02d.json", i)
	}
	return files
}

func main() {
	validator := NewFileValidator(generateFileList(20), defaultMaxConcurrent)

	fmt.Println("=== Bounded + Cancellation (limit=5) ===")
	start := time.Now()

	stats, err := validator.RunBoundedWithCancellation()

	fmt.Printf("\nValidated: %d, Cancelled: %d, Total: %d\n", stats.Validated, stats.Cancelled, stats.Total)
	fmt.Printf("Total time: %v\n", time.Since(start).Round(time.Millisecond))
	if err != nil {
		fmt.Printf("First error: %v\n", err)
	}
}
```

**Expected output:**
```
=== Bounded + Cancellation (limit=5) ===

Validated: 6, Cancelled: 13, Total: 20
Total time: 200ms
First error: validate config/service-07.json: missing required field 'port'
```

The critical addition is checking `ctx.Done()` BEFORE acquiring the semaphore in the for loop. Without this check, the loop would block on `sem <- struct{}{}` even after the context is cancelled. The `select` on `ctx.Done()` allows the loop to skip remaining files immediately.

The `golang.org/x/sync/errgroup` package provides this exact behavior via `SetLimit`:

```go
// With errgroup, the same code becomes:
//   g, ctx := errgroup.WithContext(context.Background())
//   g.SetLimit(5)
//   for _, f := range files {
//       g.Go(func() error {
//           select {
//           case <-ctx.Done():
//               return ctx.Err()
//           default:
//           }
//           return validateFile(f)
//       })
//   }
//   err := g.Wait()
//
// SetLimit(5) replaces the semaphore channel.
// WithContext replaces the manual cancel.
// g.Go() blocks when 5 goroutines are running (backpressure built-in).
```

## Step 4 -- Choosing the Right Concurrency Limit

The limit depends on your bottleneck. Here is a practical guide:

```go
package main

import (
	"fmt"
	"runtime"
)

type ConcurrencyGuideline struct {
	Scenario string
	Limit    int
	Reason   string
}

func buildConcurrencyGuidelines() []ConcurrencyGuideline {
	return []ConcurrencyGuideline{
		{
			"File validation (disk I/O)",
			10,
			"Limited by file descriptors and disk throughput, not CPU",
		},
		{
			"HTTP health checks",
			20,
			"Limited by connection pool size and target server capacity",
		},
		{
			"CPU-heavy parsing (JSON schema validation)",
			runtime.NumCPU(),
			"Limited by CPU cores -- more goroutines just cause context switching",
		},
		{
			"Database queries",
			5,
			"Limited by connection pool size (typically 5-20 connections)",
		},
		{
			"External API calls (rate-limited)",
			3,
			"Limited by API rate limit (e.g., 100 req/sec = small bursts)",
		},
	}
}

func printGuidelines(guidelines []ConcurrencyGuideline) {
	for _, g := range guidelines {
		fmt.Printf("  %-45s limit=%d  (%s)\n", g.Scenario, g.Limit, g.Reason)
	}
}

func main() {
	fmt.Println("=== Choosing Concurrency Limits ===")
	fmt.Printf("CPU cores: %d\n\n", runtime.NumCPU())

	printGuidelines(buildConcurrencyGuidelines())
}
```

**Expected output:**
```
=== Choosing Concurrency Limits ===
CPU cores: 8

  File validation (disk I/O)                    limit=10   (Limited by file descriptors and disk throughput, not CPU)
  HTTP health checks                            limit=20   (Limited by connection pool size and target server capacity)
  CPU-heavy parsing (JSON schema validation)    limit=8    (Limited by CPU cores -- more goroutines just cause context switching)
  Database queries                              limit=5    (Limited by connection pool size (typically 5-20 connections))
  External API calls (rate-limited)             limit=3    (Limited by API rate limit (e.g., 100 req/sec = small bursts))
```

## Verification

At this point, verify:
1. Unbounded concurrency shows all 20 goroutines starting simultaneously
2. Semaphore limits peak concurrency to exactly 5
3. With cancellation, files after the error are skipped
4. Total time follows `ceil(files/limit) * duration_per_file`

## Common Mistakes

### Acquiring the semaphore inside the goroutine

**Wrong:**
```go
wg.Add(1)
go func() {
    defer wg.Done()
    sem <- struct{}{} // acquire INSIDE the goroutine
    defer func() { <-sem }()
    validateFile(f)
}()
```

**What happens:** The goroutine is already launched before acquiring the semaphore. You get unbounded goroutine creation -- 1000 goroutines are created immediately, they just block on the semaphore. Memory usage spikes because each goroutine allocates a stack (at least 2KB, often grows to 8KB+).

**Fix:** Acquire BEFORE launching the goroutine:
```go
sem <- struct{}{} // acquire OUTSIDE -- blocks goroutine creation
wg.Add(1)
go func() {
    defer wg.Done()
    defer func() { <-sem }()
    validateFile(f)
}()
```

### Not checking context before the semaphore acquire

**Wrong:**
```go
for _, f := range files {
    sem <- struct{}{} // blocks even if context is cancelled!
    go func() { ... }()
}
```

**What happens:** After a failure cancels the context, the loop still blocks on `sem <- struct{}{}` waiting for a goroutine to finish and release a token. The loop does not exit until all semaphore slots are freed.

**Fix:** Use `select` to check context first:
```go
select {
case <-ctx.Done():
    continue // skip remaining files
case sem <- struct{}{}:
}
```

### Setting limit to 0

**Wrong:**
```go
sem := make(chan struct{}, 0) // unbuffered -- send blocks until receive
```

**What happens:** `sem <- struct{}{}` blocks until some goroutine does `<-sem`. But no goroutine is running yet. Deadlock on the first iteration.

**Fix:** Always use a positive buffer size.

## Review

Bounded concurrency comes down to a token bucket: a buffered channel of capacity
N. A goroutine sends into it to acquire a slot and receives from it to release,
so at most N run at once and the N+1th send blocks -- that block is the
backpressure that keeps you from opening a thousand file descriptors or
saturating a disk. The one detail that makes or breaks it is where you acquire:
the token must be taken BEFORE the `go` statement, not inside the goroutine, or
you spawn every goroutine immediately and they merely queue on the semaphore,
defeating the whole purpose and burning a stack apiece. Layering
`context.WithCancel` on top gives fail-fast behavior -- the first error cancels
the context -- but only if the loop checks `ctx.Done()` in the same `select` as
the acquire, otherwise it blocks on a full semaphore even after cancellation.
`golang.org/x/sync/errgroup.SetLimit(N)` packages exactly this: a limit, a
shared context, and a `g.Go()` that blocks when the limit is reached.

Confirm the behavior by running the program and reading the numbers against the
theory. Unbounded, all twenty validations start at once and finish in about
100ms; with a semaphore of five, peak concurrency is exactly five and the run
takes roughly 400ms -- ceil(20/5) batches of 100ms. Add cancellation and the
files after the first failure are skipped rather than validated, cutting the
time further. Finally, the limit guideline table should print your machine's
actual core count for the CPU-bound row: the right limit is dictated by the
bottleneck -- file descriptors, connection pool, CPU cores, or an API rate limit
-- not by how many files you happen to have.

## Resources
- [Go Effective Go: Channels as semaphores](https://go.dev/doc/effective_go#channels) -- the buffered-channel-as-semaphore idiom this exercise builds by hand.
- [errgroup.Group.SetLimit documentation](https://pkg.go.dev/golang.org/x/sync/errgroup#Group.SetLimit) -- the one-line API that replaces the manual semaphore and cancel plumbing.
- [Linux file descriptor limits](https://man7.org/linux/man-pages/man2/getrlimit.2.html) -- the `getrlimit` man page behind the "too many open files" error that motivates the limit.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-errgroup-collect-results](../04-errgroup-collect-results/04-errgroup-collect-results.md)
