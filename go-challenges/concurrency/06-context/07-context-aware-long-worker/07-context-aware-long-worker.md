# Exercise 7: Context-Aware Long Worker

Real systems run long operations over thousands or millions of records -- monthly revenue reports, uploaded CSV processing, data migrations, analytics exports -- that take minutes or hours. Without context awareness they cannot be stopped cleanly, and killing one mid-flight risks leaving data half-written. A context-aware worker instead checks `ctx.Done()` at natural checkpoints -- between chunks, between files, after each page -- so when a deployment rolls out with SIGTERM, an admin cancels a report, or a deadline arrives, the worker finishes its current unit, saves progress, and exits without corruption. This exercise builds that discipline four ways, the mechanism identical each time (check context between units) while the partial-result strategy shifts with the domain: a report returns the 3,000 rows it managed, a migrator returns the records it fully committed.

## What you'll build

```text
07-context-aware-long-worker/
  main.go        four cancellable workers: a chunked report generator, a
                 deadline-aware file processor, a progress-reporting exporter,
                 and an atomic-unit data migrator
```

- Build: long-running workers that cancel cleanly at unit boundaries and return usable partial results.
- Implement: `ReportGenerator.Generate` checking `ctx.Done()` between chunks, `FileProcessor` that fails fast on `ctx.Deadline()`, a `DataExporter` that streams `ExportProgress`, and a `DataMigrator` that finishes each record's steps atomically.
- Verify: `go run main.go`

### Why the partial-result strategy is the real design decision

Checking `ctx.Done()` is mechanical; deciding what a cancelled operation leaves behind is where the design lives. A report generator can hand back the 3,000 rows it processed before the deadline and tell the caller which chunk to resume from. A file processor treats each file as a complete unit and can even fail fast -- skipping a file it knows cannot finish in the remaining budget -- rather than starting work it must abandon.

The migrator draws the sharpest line: cancellation is honored between records, but once a record's validate/transform/write sequence begins it runs to completion, so no record is ever left half-migrated. The checkpoint granularity, not the cancellation call, is what guarantees the data stays consistent.

## Step 1 -- Report Generator with Chunked Processing

Build a report generator that processes sales records in chunks. Between each chunk, it checks whether it should stop:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	reportTimeout         = 350 * time.Millisecond
	chunkProcessingDelay  = 100 * time.Millisecond
	revenuePerRecord      = 49.99
)

type ReportResult struct {
	TotalRecords    int
	ProcessedChunks int
	TotalChunks     int
	Revenue         float64
	Complete        bool
	CancelReason    string
}

type ReportGenerator struct {
	chunkSize int
}

func NewReportGenerator(chunkSize int) *ReportGenerator {
	return &ReportGenerator{chunkSize: chunkSize}
}

func (g *ReportGenerator) Generate(ctx context.Context, totalRecords int) ReportResult {
	totalChunks := (totalRecords + g.chunkSize - 1) / g.chunkSize
	result := ReportResult{TotalChunks: totalChunks}

	fmt.Printf("[report] starting: %d records in %d chunks of %d\n",
		totalRecords, totalChunks, g.chunkSize)

	for chunk := 0; chunk < totalChunks; chunk++ {
		select {
		case <-ctx.Done():
			result.CancelReason = ctx.Err().Error()
			fmt.Printf("[report] cancelled at chunk %d/%d: %v\n",
				chunk, totalChunks, ctx.Err())
			return result
		default:
		}

		recordsInChunk := g.chunkSize
		remaining := totalRecords - result.TotalRecords
		if remaining < g.chunkSize {
			recordsInChunk = remaining
		}

		fmt.Printf("[report] processing chunk %d/%d (%d records)...\n",
			chunk+1, totalChunks, recordsInChunk)
		time.Sleep(chunkProcessingDelay)

		result.TotalRecords += recordsInChunk
		result.ProcessedChunks++
		result.Revenue += float64(recordsInChunk) * revenuePerRecord
	}

	result.Complete = true
	return result
}

func printReportResult(result ReportResult, totalRecords int) {
	fmt.Println("\n=== Report Result ===")
	fmt.Printf("Complete:   %v\n", result.Complete)
	fmt.Printf("Processed:  %d/%d chunks\n", result.ProcessedChunks, result.TotalChunks)
	fmt.Printf("Records:    %d/%d\n", result.TotalRecords, totalRecords)
	fmt.Printf("Revenue:    $%.2f\n", result.Revenue)
	if result.CancelReason != "" {
		fmt.Printf("Cancelled:  %s\n", result.CancelReason)
		fmt.Println("Action:     partial report saved, can resume from chunk",
			result.ProcessedChunks+1)
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()

	generator := NewReportGenerator(1000)
	result := generator.Generate(ctx, 5000)
	printReportResult(result, 5000)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
[report] starting: 5000 records in 5 chunks of 1000
[report] processing chunk 1/5 (1000 records)...
[report] processing chunk 2/5 (1000 records)...
[report] processing chunk 3/5 (1000 records)...
[report] cancelled at chunk 3/5: context deadline exceeded

=== Report Result ===
Complete:   false
Processed:  3/5 chunks
Records:    3000/5000
Revenue:    $149970.00
Cancelled:  context deadline exceeded
Action:     partial report saved, can resume from chunk 4
```

The report processed 3 chunks before the deadline. The result contains the partial data (3000 records, partial revenue), and the caller knows exactly where to resume. No data is corrupted because the check happens between complete chunks.

## Step 2 -- File Processor that Respects Deadlines

Build a processor that reads and transforms files from a directory. Each file is a complete unit of work -- cancellation happens between files, not mid-file:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	fileBudget        = 350 * time.Millisecond
	processingRate    = 20 // ms per MB
)

type FileResult struct {
	Name   string
	Size   int
	Status string
}

type FileSpec struct {
	Name   string
	SizeMB int
}

type FileProcessor struct{}

func NewFileProcessor() *FileProcessor {
	return &FileProcessor{}
}

func (fp *FileProcessor) processFile(ctx context.Context, spec FileSpec) FileResult {
	processTime := time.Duration(spec.SizeMB*processingRate) * time.Millisecond

	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < processTime {
			return FileResult{
				Name:   spec.Name,
				Size:   spec.SizeMB,
				Status: fmt.Sprintf("skipped (needs %v, only %v left)", processTime, remaining.Round(time.Millisecond)),
			}
		}
	}

	fmt.Printf("[processor] processing %s (%dMB, ~%v)\n", spec.Name, spec.SizeMB, processTime)

	select {
	case <-time.After(processTime):
		return FileResult{Name: spec.Name, Size: spec.SizeMB, Status: "completed"}
	case <-ctx.Done():
		return FileResult{Name: spec.Name, Size: spec.SizeMB, Status: fmt.Sprintf("interrupted: %v", ctx.Err())}
	}
}

func (fp *FileProcessor) ProcessAll(ctx context.Context, files []FileSpec) []FileResult {
	var results []FileResult

	for _, f := range files {
		select {
		case <-ctx.Done():
			for _, remaining := range files[len(results):] {
				results = append(results, FileResult{
					Name:   remaining.Name,
					Size:   remaining.SizeMB,
					Status: "not started (context cancelled)",
				})
			}
			return results
		default:
		}

		result := fp.processFile(ctx, f)
		results = append(results, result)
	}

	return results
}

func printFileResults(results []FileResult) {
	fmt.Println("\n=== Results ===")
	for _, r := range results {
		fmt.Printf("  %-25s %3dMB  %s\n", r.Name, r.Size, r.Status)
	}

	completed := 0
	for _, r := range results {
		if r.Status == "completed" {
			completed++
		}
	}
	fmt.Printf("\nProcessed: %d/%d files\n", completed, len(results))
}

func main() {
	files := []FileSpec{
		{Name: "report-2024-01.csv", SizeMB: 5},
		{Name: "report-2024-02.csv", SizeMB: 3},
		{Name: "report-2024-03.csv", SizeMB: 8},
		{Name: "transactions.csv", SizeMB: 15},
		{Name: "summary.csv", SizeMB: 2},
	}

	ctx, cancel := context.WithTimeout(context.Background(), fileBudget)
	defer cancel()

	fmt.Printf("=== File Processing (budget: %v) ===\n\n", fileBudget)

	processor := NewFileProcessor()
	results := processor.ProcessAll(ctx, files)
	printFileResults(results)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== File Processing (budget: 350ms) ===

[processor] processing report-2024-01.csv (5MB, ~100ms)
[processor] processing report-2024-02.csv (3MB, ~60ms)
[processor] processing report-2024-03.csv (8MB, ~160ms)

=== Results ===
  report-2024-01.csv        5MB  completed
  report-2024-02.csv        3MB  completed
  report-2024-03.csv        8MB  completed
  transactions.csv          15MB  skipped (needs 300ms, only 29ms left)
  summary.csv                2MB  not started (context cancelled)

Processed: 3/5 files
```

The processor completed 3 files, then detected that the 4th file (15MB, needs 300ms) could not finish within the remaining budget and skipped it proactively. The 5th file was never started because the context expired.

## Step 3 -- Progress Reporting for Long Operations

Build a data export that reports progress, allowing the caller to monitor completion or cancel when they have enough data:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	exportTimeout    = 350 * time.Millisecond
	pageExportDelay  = 80 * time.Millisecond
	bytesPerPage     = 4096
)

type ExportProgress struct {
	Phase         string
	PagesExported int
	TotalPages    int
	BytesWritten  int
	Done          bool
	Err           error
}

type DataExporter struct{}

func NewDataExporter() *DataExporter {
	return &DataExporter{}
}

func (e *DataExporter) Export(ctx context.Context, totalPages int, progress chan<- ExportProgress) {
	defer close(progress)

	for page := 1; page <= totalPages; page++ {
		select {
		case <-ctx.Done():
			progress <- ExportProgress{
				Phase:         "cancelled",
				PagesExported: page - 1,
				TotalPages:    totalPages,
				BytesWritten:  (page - 1) * bytesPerPage,
				Done:          true,
				Err:           ctx.Err(),
			}
			return
		default:
		}

		progress <- ExportProgress{
			Phase:         "exporting",
			PagesExported: page,
			TotalPages:    totalPages,
			BytesWritten:  page * bytesPerPage,
		}

		time.Sleep(pageExportDelay)
	}

	progress <- ExportProgress{
		Phase:         "complete",
		PagesExported: totalPages,
		TotalPages:    totalPages,
		BytesWritten:  totalPages * bytesPerPage,
		Done:          true,
	}
}

func monitorExport(progress <-chan ExportProgress) {
	for p := range progress {
		if p.Done {
			if p.Err != nil {
				fmt.Printf("\nExport cancelled at page %d/%d (%d bytes written)\n",
					p.PagesExported, p.TotalPages, p.BytesWritten)
				fmt.Printf("Reason: %v\n", p.Err)
				fmt.Println("Partial export file is valid and can be downloaded.")
			} else {
				fmt.Printf("\nExport complete: %d pages, %d bytes\n",
					p.PagesExported, p.BytesWritten)
			}
			break
		}
		pct := float64(p.PagesExported) / float64(p.TotalPages) * 100
		fmt.Printf("  [%3.0f%%] page %d/%d exported (%d bytes)\n",
			pct, p.PagesExported, p.TotalPages, p.BytesWritten)
	}
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()

	exporter := NewDataExporter()
	progress := make(chan ExportProgress)
	go exporter.Export(ctx, 10, progress)

	fmt.Printf("=== Data Export Progress ===\n\n")
	monitorExport(progress)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Data Export Progress ===

  [ 10%] page 1/10 exported (4096 bytes)
  [ 20%] page 2/10 exported (8192 bytes)
  [ 30%] page 3/10 exported (12288 bytes)
  [ 40%] page 4/10 exported (16384 bytes)

Export cancelled at page 4/10 (16384 bytes written)
Reason: context deadline exceeded
Partial export file is valid and can be downloaded.
```

The progress channel lets the caller show a progress bar, log completion status, or decide to cancel early if partial data is sufficient.

## Step 4 -- Atomic Units: Finish Current Record Before Stopping

Sometimes each record has multiple steps that must complete together (like a database transaction). Check cancellation between records, but once a record starts, run all its steps to completion:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	migrationTimeout = 250 * time.Millisecond
	stepDelay        = 30 * time.Millisecond
)

type MigrationResult struct {
	Migrated []string
	Pending  []string
	Error    error
}

type DataMigrator struct{}

func NewDataMigrator() *DataMigrator {
	return &DataMigrator{}
}

func (m *DataMigrator) migrateRecord(recordID string) {
	fmt.Printf("    validate %s\n", recordID)
	time.Sleep(stepDelay)
	fmt.Printf("    transform %s\n", recordID)
	time.Sleep(stepDelay)
	fmt.Printf("    write %s\n", recordID)
	time.Sleep(stepDelay)
}

func (m *DataMigrator) Run(ctx context.Context, records []string) MigrationResult {
	result := MigrationResult{}

	for i, record := range records {
		select {
		case <-ctx.Done():
			result.Pending = records[i:]
			result.Error = ctx.Err()
			return result
		default:
		}

		fmt.Printf("[migration] record %d/%d: %s\n", i+1, len(records), record)
		m.migrateRecord(record)
		result.Migrated = append(result.Migrated, record)
		fmt.Printf("[migration] record %s committed\n\n", record)
	}

	return result
}

func printMigrationResult(result MigrationResult) {
	fmt.Println("=== Migration Result ===")
	fmt.Printf("Migrated: %v\n", result.Migrated)
	fmt.Printf("Pending:  %v\n", result.Pending)
	if result.Error != nil {
		fmt.Printf("Stopped:  %v\n", result.Error)
		fmt.Println("\nNo data corruption: each migrated record completed all 3 steps.")
		fmt.Printf("Resume migration from record: %s\n", result.Pending[0])
	}
}

func main() {
	records := []string{
		"user-001", "user-002", "user-003",
		"user-004", "user-005",
	}

	ctx, cancel := context.WithTimeout(context.Background(), migrationTimeout)
	defer cancel()

	fmt.Printf("=== Data Migration (budget: %v, ~%v per record) ===\n\n", migrationTimeout, stepDelay*3)

	migrator := NewDataMigrator()
	result := migrator.Run(ctx, records)
	printMigrationResult(result)
}
```

### Verification
```bash
go run main.go
```
Expected output:
```
=== Data Migration (budget: 250ms, ~90ms per record) ===

[migration] record 1/5: user-001
    validate user-001
    transform user-001
    write user-001
[migration] record user-001 committed

[migration] record 2/5: user-002
    validate user-002
    transform user-002
    write user-002
[migration] record user-002 committed

=== Migration Result ===
Migrated: [user-001 user-002]
Pending:  [user-003 user-004 user-005]
Stopped:  context deadline exceeded

No data corruption: each migrated record completed all 3 steps.
Resume migration from record: user-003
```

Each migrated record completed all three steps (validate, transform, write) atomically. No record is left half-processed. The caller knows exactly which records were migrated and where to resume.

## Common Mistakes

### Checking ctx.Done() Only at the Start
**Wrong:**
```go
for _, item := range items {
    select {
    case <-ctx.Done():
        return
    default:
    }
    veryLongOperation(item) // runs for minutes -- no cancellation check inside
}
```
**Fix:** Check `ctx.Done()` at multiple points within long operations, or break them into smaller steps. A worker that only checks at the top of each iteration is unresponsive to cancellation during the work phase.

### Blocking on Channel Send After Cancellation
**Wrong:**
```go
case <-ctx.Done():
    results <- partialResult // blocks forever if nobody is reading!
```
**Fix:**
```go
case <-ctx.Done():
    select {
    case results <- partialResult:
    default: // drop if nobody is listening
    }
```

### Not Returning After Cancellation
**Wrong:**
```go
select {
case <-ctx.Done():
    fmt.Println("cancelled")
    // falls through to continue working!
}
doMoreWork() // this still runs
```
**Fix:** Always `return` after handling cancellation. The `select` case does not break out of the surrounding loop or function.

### Using default in a Select with ctx.Done() and a Channel
**Caution:** When you have both `ctx.Done()` and a work channel in a select, adding a `default` case creates a busy loop that wastes CPU:
```go
select {
case <-ctx.Done(): return
case job := <-jobs: process(job)
default: // THIS SPINS THE CPU when both channels are empty!
}
```
Only use `default` in the non-blocking check pattern (the top-of-loop check in Step 1).

## Review

Cooperative cancellation comes down to checking `ctx.Done()` at natural checkpoints -- between chunks, between files, between records -- and returning partial results carrying enough state for the caller to resume, whether that is the next chunk index, the list of skipped files, or the pending records. Two refinements make it production-grade. Fail-fast: when the context has a deadline, check whether the next unit can even finish in the time left and skip it proactively rather than starting work you must abandon. Atomicity: for a multi-step record, check cancellation between records but run all of one record's steps to completion, so nothing is left half-done. Two control-flow rules keep it honest -- always `return` after handling cancellation, because a `select` case does not break the enclosing loop, and avoid a `default` in a select that also waits on a work channel, since that turns the wait into a CPU-burning busy loop.

To confirm the pattern has stuck, build a batch processor without re-reading the steps: ten records that each take 60ms, a 400ms timeout, checking `ctx.Done()` at the top of the loop and returning a result that separates the records it transformed from the ones it skipped, with the cancellation reason attached. Under that budget roughly six records should transform before the deadline exceeds and the remaining four land in the skipped list -- and you should be able to predict that split from the arithmetic before you run it.

## Resources
- [Go Blog: Pipelines](https://go.dev/blog/pipelines) -- staged processing and where cancellation checkpoints belong between units of work.
- [Go Concurrency Patterns](https://go.dev/talks/2012/concurrency.slide) -- the select-plus-done idioms these workers use to stay cancellable.
- [Package context](https://pkg.go.dev/context) -- `WithTimeout`, `Done`, `Deadline`, and `Err`, the exact surface this exercise drives.

---

Back to [Concurrency](../../concurrency.md) | Next: [08-graceful-shutdown-with-context](../08-graceful-shutdown-with-context/08-graceful-shutdown-with-context.md)
