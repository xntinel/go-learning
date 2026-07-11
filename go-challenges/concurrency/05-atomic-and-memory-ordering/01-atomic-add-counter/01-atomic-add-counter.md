# Exercise 1: Atomic Add Counter

Every production web server keeps request metrics -- total requests, bytes transferred, errors -- and every handler goroutine bumps those counters on every request, thousands of times a second across dozens of goroutines. The trap is that `counter++` is not one operation but three: load, add, store. When two goroutines run it at once they can both load the same value, both add one, and both store the same result, so one increment vanishes. At 10,000 requests a second you lose hundreds of increments a second, and the damage is quiet: dashboards undercount, error rates look artificially low, and capacity planning runs on numbers that are wrong. This exercise fixes that with `sync/atomic`, whose read-modify-write executes as a single indivisible CPU instruction no goroutine can catch mid-flight.

## What you'll build

```text
01-atomic-add-counter/
  main.go        a web-server metrics counter: the racy version, the atomic
                 fix, a typed atomic.Int64 collector, and an atomic-vs-mutex bench
```

- Build: a concurrent request-metrics counter that stays exact under load.
- Implement: an `UnsafeMetrics` that loses counts, an `AtomicRawMetrics` using `atomic.AddInt64`, a `RequestMetrics` built on typed `atomic.Int64` fields with `Load`/`Add`, and a benchmark comparing atomic against mutex counters.
- Verify: `go run main.go`, and `go run -race main.go` to see the race appear and then vanish.

### Why atomics beat a mutex for a plain counter

`sync/atomic` performs the whole read-modify-write as one indivisible instruction (`LOCK XADD` on x86), so no goroutine can observe an intermediate state and no increment is lost. For a simple counter that is faster than a mutex because it avoids lock acquisition and the goroutine parking a contended `Lock` can trigger.

The Go 1.19 typed wrappers -- `atomic.Int64` and friends -- are the preferred form: they are method-based and their underlying value is unexported, so you cannot accidentally read or write it non-atomically. The rule that makes the whole scheme sound is all-or-nothing: if any access to a variable is atomic, every access must be, with no mixing.

## Step 1 -- Observe Lost Request Counts Without Atomics

Simulate a web server where 100 handler goroutines each process 1,000 requests. Each handler increments a shared request counter. Without atomic operations, the final count is wrong:

```go
package main

import (
	"fmt"
	"sync"
)

const (
	handlerCount       = 100
	requestsPerHandler = 1000
	bytesPerRequest    = 256
	errorEveryN        = 50
)

type UnsafeMetrics struct {
	TotalRequests int64
	TotalBytes    int64
	TotalErrors   int64
}

func (m *UnsafeMetrics) RecordRequest(reqIndex int) {
	m.TotalRequests++ // BUG: load-modify-store, three separate operations
	m.TotalBytes += bytesPerRequest
	if reqIndex%errorEveryN == 0 {
		m.TotalErrors++
	}
}

func (m *UnsafeMetrics) Report() {
	fmt.Println("=== Request Metrics (BROKEN - no synchronization) ===")
	fmt.Printf("Total requests: %d (expected %d)\n", m.TotalRequests, handlerCount*requestsPerHandler)
	fmt.Printf("Total bytes:    %d (expected %d)\n", m.TotalBytes, handlerCount*requestsPerHandler*bytesPerRequest)
	fmt.Printf("Total errors:   %d (expected %d)\n", m.TotalErrors, handlerCount*(requestsPerHandler/errorEveryN))
}

func simulateHandlers(metrics *UnsafeMetrics) {
	var wg sync.WaitGroup
	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := 0; req < requestsPerHandler; req++ {
				metrics.RecordRequest(req)
			}
		}()
	}
	wg.Wait()
}

func main() {
	metrics := &UnsafeMetrics{}
	simulateHandlers(metrics)
	metrics.Report()
}
```

### Verification
```bash
go run main.go
```
Run it several times. The counts vary and never reach the expected values. Confirm the data race:
```bash
go run -race main.go
```
The race detector reports `DATA RACE` warnings pointing to the `counter++` lines.

## Step 2 -- Fix with atomic.AddInt64

Replace every `counter++` with `atomic.AddInt64`. The entire read-add-store happens as one CPU instruction (e.g., `LOCK XADD` on x86). No goroutine can see a half-updated value:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	handlerCount       = 100
	requestsPerHandler = 1000
	bytesPerRequest    = 256
	errorEveryN        = 50
)

type AtomicRawMetrics struct {
	totalRequests int64
	totalBytes    int64
	totalErrors   int64
}

func (m *AtomicRawMetrics) RecordRequest(reqIndex int) {
	atomic.AddInt64(&m.totalRequests, 1)
	atomic.AddInt64(&m.totalBytes, bytesPerRequest)
	if reqIndex%errorEveryN == 0 {
		atomic.AddInt64(&m.totalErrors, 1)
	}
}

func (m *AtomicRawMetrics) Report() {
	fmt.Println("=== Request Metrics (FIXED - atomic operations) ===")
	fmt.Printf("Total requests: %d (expected %d)\n", m.totalRequests, handlerCount*requestsPerHandler)
	fmt.Printf("Total bytes:    %d (expected %d)\n", m.totalBytes, handlerCount*requestsPerHandler*bytesPerRequest)
	fmt.Printf("Total errors:   %d (expected %d)\n", m.totalErrors, handlerCount*(requestsPerHandler/errorEveryN))
}

func simulateHandlers(metrics *AtomicRawMetrics) {
	var wg sync.WaitGroup
	for i := 0; i < handlerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for req := 0; req < requestsPerHandler; req++ {
				metrics.RecordRequest(req)
			}
		}()
	}
	wg.Wait()
}

func main() {
	metrics := &AtomicRawMetrics{}
	simulateHandlers(metrics)
	metrics.Report()
}
```

### Verification
```bash
go run -race main.go
```
All counts are exact every run. No race warnings.

## Step 3 -- Use Typed atomic.Int64 for a Metrics Struct

Go 1.19 introduced typed wrappers like `atomic.Int64`. These are method-based and harder to misuse because the underlying value is unexported -- you cannot accidentally access it non-atomically. Build a proper metrics collector:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const (
	concurrentHandlers  = 50
	requestsPerHandler  = 200
	minResponseBytes    = 64
	maxResponseBytes    = 4096
	errorRatePercent    = 5
	reportingIntervalMs = 10
)

type RequestMetrics struct {
	TotalRequests atomic.Int64
	TotalBytes    atomic.Int64
	TotalErrors   atomic.Int64
	ActiveConns   atomic.Int64
}

func (m *RequestMetrics) RecordRequest(bytes int64, isError bool) {
	m.TotalRequests.Add(1)
	m.TotalBytes.Add(bytes)
	if isError {
		m.TotalErrors.Add(1)
	}
}

func (m *RequestMetrics) ConnOpen()  { m.ActiveConns.Add(1) }
func (m *RequestMetrics) ConnClose() { m.ActiveConns.Add(-1) }

func (m *RequestMetrics) Snapshot() string {
	return fmt.Sprintf(
		"requests=%d bytes=%d errors=%d active_conns=%d",
		m.TotalRequests.Load(),
		m.TotalBytes.Load(),
		m.TotalErrors.Load(),
		m.ActiveConns.Load(),
	)
}

func simulateHandler(metrics *RequestMetrics, wg *sync.WaitGroup) {
	defer wg.Done()

	metrics.ConnOpen()
	defer metrics.ConnClose()

	for req := 0; req < requestsPerHandler; req++ {
		bytes := int64(minResponseBytes + rand.IntN(maxResponseBytes))
		isError := rand.IntN(100) < errorRatePercent
		metrics.RecordRequest(bytes, isError)
		time.Sleep(time.Microsecond)
	}
}

func startLiveReporter(metrics *RequestMetrics) chan struct{} {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(reportingIntervalMs * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Printf("[live] %s\n", metrics.Snapshot())
			}
		}
	}()
	return done
}

func main() {
	metrics := &RequestMetrics{}
	var wg sync.WaitGroup

	for i := 0; i < concurrentHandlers; i++ {
		wg.Add(1)
		go simulateHandler(metrics, &wg)
	}

	done := startLiveReporter(metrics)
	wg.Wait()
	close(done)

	fmt.Printf("\n[final] %s\n", metrics.Snapshot())
	fmt.Printf("Expected total requests: %d\n", concurrentHandlers*requestsPerHandler)
}
```

### Verification
```bash
go run -race main.go
```
Live metrics update while handlers run. Final request count is exactly 10,000. Active connections end at 0. No race warnings.

## Step 4 -- Benchmark: Atomic vs Mutex Counters

Measure the real performance difference. This program runs both approaches with the same workload and reports elapsed time:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type ContentionScenario struct {
	Name       string
	Goroutines int
	Iterations int
}

type BenchmarkResult struct {
	AtomicDuration time.Duration
	MutexDuration  time.Duration
}

func (r BenchmarkResult) Ratio() float64 {
	return float64(r.MutexDuration) / float64(r.AtomicDuration)
}

func benchmarkAtomicCounter(goroutines, iterations int) time.Duration {
	var counter atomic.Int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				counter.Add(1)
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func benchmarkMutexCounter(goroutines, iterations int) time.Duration {
	var mu sync.Mutex
	var counter int64
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				mu.Lock()
				counter++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func runScenario(s ContentionScenario) BenchmarkResult {
	return BenchmarkResult{
		AtomicDuration: benchmarkAtomicCounter(s.Goroutines, s.Iterations),
		MutexDuration:  benchmarkMutexCounter(s.Goroutines, s.Iterations),
	}
}

func printResult(s ContentionScenario, r BenchmarkResult) {
	fmt.Printf("%s:\n", s.Name)
	fmt.Printf("  Atomic: %v\n", r.AtomicDuration)
	fmt.Printf("  Mutex:  %v\n", r.MutexDuration)
	fmt.Printf("  Mutex/Atomic ratio: %.2fx\n\n", r.Ratio())
}

func main() {
	scenarios := []ContentionScenario{
		{"Low contention (4 goroutines)", 4, 100000},
		{"Medium contention (64 goroutines)", 64, 10000},
		{"High contention (1000 goroutines)", 1000, 1000},
	}

	fmt.Println("=== Atomic vs Mutex Counter Benchmark ===")
	fmt.Println()

	for _, s := range scenarios {
		result := runScenario(s)
		printResult(s, result)
	}
}
```

### Verification
```bash
go run main.go
```
Under all contention levels, atomic is faster for simple counter increments. The gap widens under high contention because mutex must park and wake goroutines while atomic uses a single CPU instruction.

## Verification

Run the race detector on each step to confirm correctness:
```bash
go run -race main.go
```
All versions except Step 1 should produce zero race warnings and exact expected counts.

## Common Mistakes

### Mixing Atomic and Non-Atomic Access on the Same Variable

**Wrong:**
```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const workerCount = 100

func incrementWithAtomic(counter *int64, wg *sync.WaitGroup) {
	defer wg.Done()
	atomic.AddInt64(counter, 1)
}

func main() {
	var requests int64
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go incrementWithAtomic(&requests, &wg)
	}

	wg.Wait()
	fmt.Println(requests) // BUG: direct read while other goroutines may use atomic writes
}
```

**What happens:** Reading `requests` directly is a data race. ALL access must be atomic if ANY access is atomic.

**Fix:** Use `atomic.LoadInt64(&requests)` to read, or in this specific case the read is safe only because `wg.Wait()` guarantees all writers finished. The rule: after full synchronization (WaitGroup, channel), a direct read is safe. Before that, always use atomic reads.

### Copying an atomic.Int64

**Wrong:**
```go
package main

import (
	"fmt"
	"sync/atomic"
)

func printCopied(original atomic.Int64) {
	// BUG: atomic.Int64 is passed by value, copying internal state
	fmt.Println(original.Load())
}

func main() {
	var a atomic.Int64
	a.Store(42)
	b := a // BUG: copies the internal state — undefined behavior
	fmt.Println(b.Load())
	printCopied(a)
}
```

**What happens:** `atomic.Int64` contains internal state that must not be copied. The compiler may warn, and the behavior is undefined.

**Fix:** Always use pointers to atomic types, or embed them in structs that are never copied.

### Using the Wrong Address with atomic.AddInt64

**Wrong:**
```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const workerCount = 100

func incrementLocalCopy(counter int64, wg *sync.WaitGroup) {
	defer wg.Done()
	c := counter           // copies value into local variable
	atomic.AddInt64(&c, 1) // increments LOCAL copy, not the shared counter
}

func main() {
	var counter int64
	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go incrementLocalCopy(counter, &wg)
	}
	wg.Wait()
	fmt.Printf("Expected: %d, Got: %d\n", workerCount, counter) // always 0
}
```

**Fix:** Always pass the address of the original shared variable: `atomic.AddInt64(&counter, 1)`.

## Review

A non-atomic `counter++` is a load-modify-store trio that interleaves under concurrency and silently drops increments, which is why the unsafe metrics never reach their expected totals. `atomic.AddInt64(&counter, delta)` collapses that trio into one indivisible CPU instruction, and the Go 1.19 `atomic.Int64` wrapper is the form to prefer -- method-based, with unexported internals that make accidental non-atomic access impossible. For independent web-server counters (requests, bytes, errors) these atomics are the right tool: lock-free, fast, zero allocation, and 2-10x quicker than a mutex depending on contention, since the mutex must park and wake goroutines while the atomic issues a single instruction. The discipline that keeps it correct is that all access to a shared variable must be atomic if any access is, and that atomics fit independent counters while multi-field state that must change together still wants a mutex.

Make sure you can answer the four questions the exercise turns on: why `counter++` produces wrong results across goroutines (the load-add-store interleave), what instruction `atomic.AddInt64` compiles to on x86 (`LOCK XADD`), when to prefer `atomic.Int64` over the bare `atomic.AddInt64` function (whenever you want the compiler to forbid non-atomic access), and why `sync.Mutex` still exists if atomics are faster for counters (because a mutex can guard a whole critical section of several fields as one atomic step, which no single atomic can).

## Resources
- [sync/atomic package](https://pkg.go.dev/sync/atomic) -- the full set of atomic add/load/store/CAS operations and the typed wrappers.
- [atomic.Int64 type](https://pkg.go.dev/sync/atomic#Int64) -- the method-based counter with unexported internals used in the metrics collector.
- [Go Race Detector](https://go.dev/doc/articles/race_detector) -- how `-race` finds the mixed-access and unsynchronized-read bugs this exercise demonstrates.

---

Back to [Concurrency](../../concurrency.md) | Next: [02-atomic-load-store](../02-atomic-load-store/02-atomic-load-store.md)
