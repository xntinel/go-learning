# Exercise 3: WaitGroup: Wait for All

In a real backend you often fan a batch of work out across goroutines and then
summarize: validate 50 uploaded files, transform a batch of records, run health
checks against many services. What you need is a way to say "wait until all N
goroutines have finished" without guessing how long each takes. `sync.WaitGroup`
is that primitive -- a counter you raise before launching work and that each
goroutine lowers as it finishes -- and this exercise drills the one rule that
makes it correct: `Add` before the `go`, `Done` inside it, and always by
pointer.

## What you'll build

```text
03-waitgroup-wait-for-all/
  main.go        parallel file processing with sync.WaitGroup: Add-before-go,
                 batch Add, and an error-collecting batch report
```

- Build: a parallel batch processor that fans files out to goroutines and waits for all of them with a `WaitGroup`.
- Implement: `processFilesInParallel` with `Add` before each `go`; a fixed-count `Add(n)` variant; a full pipeline that collects per-file results and prints a summary.
- Verify: `go run main.go` for each step.

### Why Add goes before go

`sync.WaitGroup` is a counter-based synchronization primitive. You increment the
counter with `Add(n)` before launching goroutines, each goroutine decrements it
with `Done()` when it finishes, and the main goroutine blocks on `Wait()` until
the counter reaches zero. It is the simplest and most common way to synchronize
goroutine completion in Go.

The critical rule is: **call `Add` before the `go` statement, not inside the
goroutine**. If you call `Add` inside the goroutine, the main goroutine might
reach `Wait()` before the goroutine has called `Add`, causing it to return
immediately with work still running.

## Step 1 -- Parallel File Processor: Process Each File in Its Own Goroutine

Imagine a batch job that processes uploaded files: validate the format, transform the content, and write the output. Each file is independent, so they can run in parallel:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	minProcessingMs = 50
	maxExtraMs      = 150
	minFileSize     = 100
	maxExtraSize    = 10000
)

type FileResult struct {
	Name    string
	Status  string
	Size    int
	Elapsed time.Duration
}

func processFile(name string) FileResult {
	start := time.Now()
	processingTime := time.Duration(minProcessingMs+rand.IntN(maxExtraMs)) * time.Millisecond
	time.Sleep(processingTime)

	return FileResult{
		Name:    name,
		Status:  "ok",
		Size:    rand.IntN(maxExtraSize) + minFileSize,
		Elapsed: time.Since(start),
	}
}

func processFilesInParallel(files []string) ([]FileResult, time.Duration) {
	results := make([]FileResult, len(files))
	var wg sync.WaitGroup
	start := time.Now()

	for i, file := range files {
		wg.Add(1) // CORRECT: Add before the go statement
		go func(idx int, name string) {
			defer wg.Done()
			results[idx] = processFile(name) // each goroutine writes to its own index -- safe
		}(i, file)
	}

	wg.Wait()
	return results, time.Since(start)
}

func estimateSequentialTime(results []FileResult) time.Duration {
	total := time.Duration(0)
	for _, r := range results {
		total += r.Elapsed
	}
	return total
}

func printProcessingSummary(results []FileResult, parallelTime time.Duration) {
	fmt.Println("=== File Processing Summary ===")
	for _, r := range results {
		fmt.Printf("  %-35s %s  %5d bytes  (%v)\n", r.Name, r.Status, r.Size, r.Elapsed.Round(time.Millisecond))
	}
	fmt.Printf("\nProcessed %d files in %v (parallel)\n", len(results), parallelTime.Round(time.Millisecond))

	sequentialTime := estimateSequentialTime(results)
	fmt.Printf("Sequential would have taken ~%v\n", sequentialTime.Round(time.Millisecond))
}

func main() {
	files := []string{
		"report-2024-q1.csv",
		"users-export.json",
		"transactions-march.parquet",
		"inventory-snapshot.csv",
		"audit-log-2024.json",
	}

	results, parallelTime := processFilesInParallel(files)
	printProcessingSummary(results, parallelTime)
}
```

Expected output:
```
=== File Processing Summary ===
  report-2024-q1.csv                 ok   4231 bytes  (150ms)
  users-export.json                  ok   8712 bytes  (87ms)
  transactions-march.parquet         ok   1055 bytes  (192ms)
  inventory-snapshot.csv             ok   6421 bytes  (63ms)
  audit-log-2024.json                ok   3210 bytes  (134ms)

Processed 5 files in 195ms (parallel)
Sequential would have taken ~626ms
```

Note: each goroutine writes to a unique index in the results slice, so no mutex is needed. This is a common and safe pattern.

### Verification
```bash
go run main.go
```
All 5 files should be processed, and the parallel time should be close to the slowest individual file (not the sum of all).

## Step 2 -- Add Before Go, Not Inside

The correct pattern is `wg.Add(1)` before the `go` statement. This simulates a real batch validation pipeline:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const taskSimulationDelay = 50 * time.Millisecond

func runValidationTask(name string) {
	time.Sleep(taskSimulationDelay)
	fmt.Printf("  [done] %s\n", name)
}

func runAllValidations(tasks []string) {
	var wg sync.WaitGroup

	for _, task := range tasks {
		wg.Add(1) // CORRECT: Add before the go statement
		go func(name string) {
			defer wg.Done()
			runValidationTask(name)
		}(task)
	}

	wg.Wait()
}

func main() {
	tasks := []string{
		"validate-schema",
		"check-duplicates",
		"verify-checksums",
		"scan-malware",
	}

	runAllValidations(tasks)
	fmt.Println("All validation tasks completed. Safe to proceed with import.")
}
```

Expected output:
```
  [done] validate-schema
  [done] check-duplicates
  [done] verify-checksums
  [done] scan-malware
All validation tasks completed. Safe to proceed with import.
```

### Verification
```bash
go run main.go
```
All four tasks should print their completion message before the final "Safe to proceed" line.

## Step 3 -- Batch Add for Known Count

When you know the exact number of goroutines upfront, you can call `Add` once. This is common in data processing where you split work into fixed chunks:

```go
package main

import (
	"fmt"
	"sync"
)

const (
	numChunks       = 10
	recordsPerChunk = 100
)

func countRecordsInChunk(chunkID int) int {
	return (chunkID + 1) * recordsPerChunk
}

func processChunksInParallel(chunkCount int) []int {
	results := make([]int, chunkCount)
	var wg sync.WaitGroup

	wg.Add(chunkCount) // add all at once -- we know the count
	for i := 0; i < chunkCount; i++ {
		go func(chunkID int) {
			defer wg.Done()
			results[chunkID] = countRecordsInChunk(chunkID)
		}(i)
	}

	wg.Wait()
	return results
}

func sumResults(results []int) int {
	total := 0
	for _, count := range results {
		total += count
	}
	return total
}

func main() {
	chunkResults := processChunksInParallel(numChunks)
	totalRecords := sumResults(chunkResults)

	fmt.Printf("Chunk results: %v\n", chunkResults)
	fmt.Printf("Total valid records across all chunks: %d\n", totalRecords)
}
```

Expected output:
```
Chunk results: [100 200 300 400 500 600 700 800 900 1000]
Total valid records across all chunks: 5500
```

### Verification
```bash
go run main.go
```
Results should contain each chunk's count and the total should be 5500.

## Step 4 -- Full Pipeline: Process, Collect Errors, Print Summary

A realistic file processor needs to handle errors. Each goroutine reports success or failure, and the main goroutine summarizes:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

const (
	minProcessingDelayMs = 30
	maxExtraDelayMs      = 100
	failureRate          = 0.2
	minRecords           = 500
	maxExtraRecords      = 5000
)

type ProcessResult struct {
	FileName string
	Success  bool
	Error    string
	Records  int
	Duration time.Duration
}

func processFileWithErrors(name string) ProcessResult {
	start := time.Now()
	time.Sleep(time.Duration(minProcessingDelayMs+rand.IntN(maxExtraDelayMs)) * time.Millisecond)

	if rand.Float64() < failureRate {
		return ProcessResult{
			FileName: name,
			Success:  false,
			Error:    "invalid header: expected 5 columns, got 3",
			Duration: time.Since(start),
		}
	}

	return ProcessResult{
		FileName: name,
		Success:  true,
		Records:  rand.IntN(maxExtraRecords) + minRecords,
		Duration: time.Since(start),
	}
}

func processAllFiles(files []string) ([]ProcessResult, time.Duration) {
	results := make([]ProcessResult, len(files))
	var wg sync.WaitGroup
	start := time.Now()

	wg.Add(len(files))
	for i, file := range files {
		go func(idx int, name string) {
			defer wg.Done()
			results[idx] = processFileWithErrors(name)
		}(i, file)
	}

	wg.Wait()
	return results, time.Since(start)
}

func printBatchReport(results []ProcessResult, totalElapsed time.Duration) {
	succeeded := 0
	failed := 0
	totalRecords := 0

	fmt.Println("=== Batch Processing Report ===")
	for _, r := range results {
		if r.Success {
			succeeded++
			totalRecords += r.Records
			fmt.Printf("  OK   %-15s  %4d records  (%v)\n", r.FileName, r.Records, r.Duration.Round(time.Millisecond))
		} else {
			failed++
			fmt.Printf("  FAIL %-15s  %s  (%v)\n", r.FileName, r.Error, r.Duration.Round(time.Millisecond))
		}
	}

	fmt.Printf("\nCompleted in %v: %d succeeded, %d failed, %d total records\n",
		totalElapsed.Round(time.Millisecond), succeeded, failed, totalRecords)
}

func main() {
	files := []string{
		"batch-001.csv", "batch-002.csv", "batch-003.csv",
		"batch-004.csv", "batch-005.csv", "batch-006.csv",
		"batch-007.csv", "batch-008.csv", "batch-009.csv",
		"batch-010.csv",
	}

	results, elapsed := processAllFiles(files)
	printBatchReport(results, elapsed)
}
```

Expected output:
```
=== Batch Processing Report ===
  OK   batch-001.csv    2341 records  (78ms)
  OK   batch-002.csv    4102 records  (45ms)
  FAIL batch-003.csv    invalid header: expected 5 columns, got 3  (92ms)
  OK   batch-004.csv    1287 records  (51ms)
  ...

Completed in 105ms: 8 succeeded, 2 failed, 19432 total records
```

### Verification
```bash
go run main.go
```
All 10 files should be processed. Some will succeed and some fail. The total time should be near the slowest individual file.

## Common Mistakes

### Add Inside the Goroutine

```go
package main

import (
	"fmt"
	"sync"
)

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		go func(id int) {
			wg.Add(1) // RACE: main might reach Wait() before this executes
			defer wg.Done()
			fmt.Println(id)
		}(i)
	}
	wg.Wait() // might return immediately with goroutines still running
	fmt.Println("done -- but some goroutines may have been skipped!")
}
```

**What happens:** `Wait()` can return before all goroutines have called `Add`, so some goroutines may not be waited for. In a file processor, this means some files appear to have been processed when they have not.

**Fix:** Always call `Add` before the `go` statement.

### Negative WaitGroup Counter

```go
package main

import "sync"

func main() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		wg.Done()
		wg.Done() // panic: sync: negative WaitGroup counter
	}()
	wg.Wait()
}
```

**What happens:** Runtime panic. Each goroutine must call `Done` exactly once.

**Fix:** Use `defer wg.Done()` as the first line inside the goroutine to guarantee it is called exactly once.

### Forgetting Done

```go
package main

import "sync"

func main() {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		if true {
			return // early return -- Done is never called!
		}
		wg.Done()
	}()
	wg.Wait() // blocks forever -- deadlock
}
```

**What happens:** Deadlock. The counter never reaches zero. Your batch job hangs indefinitely.

**Fix:** Use `defer wg.Done()` so it runs regardless of how the goroutine exits.

### Passing WaitGroup by Value

```go
package main

import (
	"fmt"
	"sync"
)

func processFile(wg sync.WaitGroup, name string) { // receives a COPY
	defer wg.Done() // decrements the copy, not the original
	fmt.Printf("Processed %s\n", name)
}

func main() {
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go processFile(wg, fmt.Sprintf("file-%d.csv", i)) // passes by value
	}
	wg.Wait() // blocks forever -- deadlock
}
```

**What happens:** The original WaitGroup counter is never decremented. Deadlock.

**Fix:** Pass `*sync.WaitGroup` (pointer):
```go
func processFile(wg *sync.WaitGroup, name string) {
	defer wg.Done()
	fmt.Printf("Processed %s\n", name)
}
```

## Review

A `WaitGroup` is just a counter with three operations: `Add` raises it, `Done`
lowers it, and `Wait` blocks until it hits zero. The correctness of the whole
pattern rests on ordering and identity. `Add` must run before the `go`
statement, because if you increment inside the goroutine the main goroutine can
reach `Wait` before the counter was ever raised and sail past with work still
running. `Done` belongs in a `defer` at the top of the goroutine so it fires on
every exit path, including early returns and panics -- forget it and the counter
never reaches zero and `Wait` blocks forever. And the group must travel by
pointer: a `sync.WaitGroup` passed by value is copied, so the goroutine
decrements a copy while the original stays stuck. When the count is known up
front you can `Add(n)` once before the loop, and because each goroutine writes
its own slice index there is no shared mutation to guard.

To confirm you can build this cold, write a parallel health checker: ping ten
services simulated with random latency and a 30% failure rate, use a `WaitGroup`
to wait for every check, and print a summary of which services are healthy,
which are down, and the total duration. Have each goroutine write its result
into its own index of a preallocated slice so no mutex is needed, and run it
under `-race` to prove there are no data races -- the whole point being that the
WaitGroup, not a `time.Sleep`, is what tells you the batch is finished.

## Resources
- [sync.WaitGroup documentation](https://pkg.go.dev/sync#WaitGroup) -- the Add/Done/Wait contract and the warning that a WaitGroup must not be copied after first use.
- [Go by Example: WaitGroups](https://gobyexample.com/waitgroups) -- a minimal runnable walk-through of the Add-before-go, defer-Done pattern.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) -- goroutines and channels in context, the broader model WaitGroup fits into.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-once-singleton-init](../04-once-singleton-init/04-once-singleton-init.md)
