# Exercise 10: End-to-End Pipeline with Cancellation

Real concurrent systems are not single patterns -- they are compositions. A
production pipeline threads a generator into fan-out for parallelism, fan-in for
aggregation, error handling, and cancellation, and the hard part is making all of
them work together without dropping an error or leaking a goroutine. This
capstone builds a CSV import system -- read, validate, enrich via a worker pool,
write -- that a user can cancel mid-run. On cancel it must stop reading, let
in-flight records finish, report how many imported, and leave zero goroutines
behind.

## What you'll build

```text
10-end-to-end-pipeline-with-cancel/
  main.go        pipeline types, the CSV reader, validate + enrich stages, a
                 fan-out enrichment pool, and the full cancelable pipeline
```

- Build: a cancelable CSV-import pipeline that composes reader, validator, fan-out enrichment, and DB writer, with goroutine-leak verification.
- Implement: `ReadCSV`, `validateRecord`/`Validate`, `enrichWorker`/`FanOutEnrich`, `WriteToDB`, `RunPipeline`, and `checkGoroutineLeaks` over `runtime.NumGoroutine`.
- Verify: `go run main.go` on each step.

### Why composition is the real challenge

Consider a real scenario: your team builds a CSV data import system. Users upload
large CSV files (100K+ rows) containing customer records. Each record must be
validated (format checks, required fields), enriched (look up company info from
an external API), and written to the database. The user can cancel the import at
any time. When they do, the system must stop reading new records, let in-flight
records finish processing, report how many records were imported successfully,
and clean up all goroutines.

This exercise is the capstone of the concurrency-patterns section. You will
combine pipeline + worker pool + context cancellation + error propagation into
one system.

```
  CSV Import Pipeline

  readCSV(ctx) --> validate(ctx) --> enrich(ctx, 3 workers) --> writeToDB(ctx) --> report
       |                |                   |                       |
    reads CSV       validates each    3 parallel workers       writes to DB
    line by line    record, marks     enrich with external     (simulated)
    (cancelable)    errors            data, merge outputs

  Context cancellation propagates through ALL stages.
  Errors flow as data (ImportResult.Error), never silently dropped.
  On cancel: stop reading, drain in-flight, report progress.
```

## Step 1 -- Define the Pipeline Data Types

Start with clear types for the data flowing through the pipeline. Every record carries enough context to trace it through all stages.

```go
package main

import (
	"context"
	"fmt"
)

const (
	statusImported = "imported"
	statusSkipped  = "skipped"
)

// CSVRecord represents a single row from the CSV file.
type CSVRecord struct {
	LineNum int
	Fields  map[string]string
}

// ValidatedRecord carries the validation outcome for a CSV row.
type ValidatedRecord struct {
	Record CSVRecord
	Valid  bool
	Error  string
}

// EnrichedRecord adds external data to a validated row.
type EnrichedRecord struct {
	Record      CSVRecord
	CompanyInfo string
	Valid       bool
	Error       string
	WorkerID    int
}

// ImportResult is the final outcome for each row: imported, skipped, or error.
type ImportResult struct {
	LineNum int
	Status  string
	Detail  string
}

// DataImporter orchestrates the CSV import pipeline.
type DataImporter struct {
	enrichWorkers int
}

func NewDataImporter(enrichWorkers int) *DataImporter {
	return &DataImporter{enrichWorkers: enrichWorkers}
}

func main() {
	ctx := context.Background()
	_ = ctx
	_ = NewDataImporter(3)
	fmt.Println("Pipeline types defined. Ready to build stages.")
}
```

Each stage transforms one type into the next. Errors are carried as data inside the struct, not as separate error returns. This prevents silent error swallowing and lets the final collector see every outcome.

## Step 2 -- Build the CSV Reader Stage

Create a generator that reads CSV records and respects context cancellation. When the user cancels the import, this stage stops emitting records.

```go
package main

import (
	"context"
	"fmt"
)

// CSVRecord represents a single row from the CSV file.
type CSVRecord struct {
	LineNum int
	Fields  map[string]string
}

// DataImporter orchestrates the CSV import pipeline.
type DataImporter struct {
	enrichWorkers int
}

func NewDataImporter(enrichWorkers int) *DataImporter {
	return &DataImporter{enrichWorkers: enrichWorkers}
}

func (di *DataImporter) ReadCSV(ctx context.Context, data []map[string]string) <-chan CSVRecord {
	out := make(chan CSVRecord)
	go func() {
		defer close(out)
		for i, row := range data {
			record := CSVRecord{LineNum: i + 1, Fields: row}
			select {
			case out <- record:
			case <-ctx.Done():
				fmt.Printf("  [reader] canceled at line %d: %v\n", i+1, ctx.Err())
				return
			}
		}
		fmt.Printf("  [reader] all %d records emitted\n", len(data))
	}()
	return out
}

func main() {
	data := []map[string]string{
		{"name": "Alice Johnson", "email": "alice@acme.com", "company": "Acme Corp"},
		{"name": "Bob Smith", "email": "bob@widgets.io", "company": "Widgets Inc"},
		{"name": "Charlie Brown", "email": "charlie@example.com", "company": "Example LLC"},
	}

	importer := NewDataImporter(3)
	records := importer.ReadCSV(context.Background(), data)

	fmt.Println("=== CSV Reader Test ===")
	for r := range records {
		fmt.Printf("  Line %d: %s (%s)\n", r.LineNum, r.Fields["name"], r.Fields["email"])
	}
}
```

### Verification
```bash
go run main.go
```
Expected:
```
=== CSV Reader Test ===
  Line 1: Alice Johnson (alice@acme.com)
  Line 2: Bob Smith (bob@widgets.io)
  Line 3: Charlie Brown (charlie@example.com)
  [reader] all 3 records emitted
```

## Step 3 -- Build Validation and Enrichment Stages

Create two processing stages: validate checks required fields, enrich simulates looking up company data from an external API.

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const enrichAPILatency = 30 * time.Millisecond

// CSVRecord represents a single row from the CSV file.
type CSVRecord struct {
	LineNum int
	Fields  map[string]string
}

// ValidatedRecord carries the validation outcome for a CSV row.
type ValidatedRecord struct {
	Record CSVRecord
	Valid  bool
	Error  string
}

// EnrichedRecord adds external data to a validated row.
type EnrichedRecord struct {
	Record      CSVRecord
	CompanyInfo string
	Valid       bool
	Error       string
	WorkerID    int
}

// DataImporter orchestrates the CSV import pipeline.
type DataImporter struct {
	enrichWorkers int
}

func NewDataImporter(enrichWorkers int) *DataImporter {
	return &DataImporter{enrichWorkers: enrichWorkers}
}

func validateRecord(record CSVRecord) ValidatedRecord {
	name := strings.TrimSpace(record.Fields["name"])
	email := strings.TrimSpace(record.Fields["email"])

	if name == "" {
		return ValidatedRecord{Record: record, Valid: false, Error: "missing required field: name"}
	}
	if email == "" {
		return ValidatedRecord{Record: record, Valid: false, Error: "missing required field: email"}
	}
	if !strings.Contains(email, "@") {
		return ValidatedRecord{Record: record, Valid: false, Error: fmt.Sprintf("invalid email format: %s", email)}
	}
	return ValidatedRecord{Record: record, Valid: true}
}

func (di *DataImporter) Validate(ctx context.Context, in <-chan CSVRecord) <-chan ValidatedRecord {
	out := make(chan ValidatedRecord)
	go func() {
		defer close(out)
		for record := range in {
			select {
			case out <- validateRecord(record):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (di *DataImporter) Enrich(ctx context.Context, id int, in <-chan ValidatedRecord) <-chan EnrichedRecord {
	out := make(chan EnrichedRecord)
	go func() {
		defer close(out)
		for vr := range in {
			if !vr.Valid {
				select {
				case out <- EnrichedRecord{Record: vr.Record, Valid: false, Error: vr.Error, WorkerID: id}:
				case <-ctx.Done():
					return
				}
				continue
			}

			time.Sleep(enrichAPILatency)
			companyInfo := fmt.Sprintf("%s (verified, 50 employees)", vr.Record.Fields["company"])

			select {
			case out <- EnrichedRecord{Record: vr.Record, CompanyInfo: companyInfo, Valid: true, WorkerID: id}:
			case <-ctx.Done():
				fmt.Printf("  [enricher %d] canceled during enrichment\n", id)
				return
			}
		}
	}()
	return out
}

func emitRecords(data []CSVRecord) <-chan CSVRecord {
	in := make(chan CSVRecord)
	go func() {
		defer close(in)
		for _, r := range data {
			in <- r
		}
	}()
	return in
}

func main() {
	csvData := []CSVRecord{
		{1, map[string]string{"name": "Alice", "email": "alice@acme.com", "company": "Acme"}},
		{2, map[string]string{"name": "", "email": "bob@widgets.io", "company": "Widgets"}},
		{3, map[string]string{"name": "Charlie", "email": "invalid-email", "company": "Ex"}},
		{4, map[string]string{"name": "Diana", "email": "diana@corp.com", "company": "Corp"}},
	}

	importer := NewDataImporter(3)
	ctx := context.Background()

	in := emitRecords(csvData)
	validated := importer.Validate(ctx, in)
	enriched := importer.Enrich(ctx, 1, validated)

	fmt.Println("=== Validate + Enrich Test ===")
	for er := range enriched {
		if !er.Valid {
			fmt.Printf("  Line %d: INVALID - %s\n", er.Record.LineNum, er.Error)
		} else {
			fmt.Printf("  Line %d: OK - %s [%s]\n",
				er.Record.LineNum, er.Record.Fields["name"], er.CompanyInfo)
		}
	}
}
```

### Verification
```bash
go run main.go
```
Expected: line 1 and 4 pass, lines 2 and 3 fail validation:
```
=== Validate + Enrich Test ===
  Line 1: OK - Alice [Acme (verified, 50 employees)]
  Line 2: INVALID - missing required field: name
  Line 3: INVALID - invalid email format: invalid-email
  Line 4: OK - Diana [Corp (verified, 50 employees)]
```

## Step 4 -- Fan-Out Enrichment with Worker Pool

Parallelize the enrichment stage (the bottleneck, since it calls an external API) with multiple workers and merge their outputs.

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	enrichAPILatency   = 50 * time.Millisecond
	defaultEnrichWorkers = 3
)

// CSVRecord represents a single row from the CSV file.
type CSVRecord struct {
	LineNum int
	Fields  map[string]string
}

// ValidatedRecord carries the validation outcome for a CSV row.
type ValidatedRecord struct {
	Record CSVRecord
	Valid  bool
	Error  string
}

// EnrichedRecord adds external data to a validated row.
type EnrichedRecord struct {
	Record      CSVRecord
	CompanyInfo string
	Valid       bool
	Error       string
	WorkerID    int
}

// DataImporter orchestrates the CSV import pipeline.
type DataImporter struct {
	enrichWorkers int
}

func NewDataImporter(enrichWorkers int) *DataImporter {
	return &DataImporter{enrichWorkers: enrichWorkers}
}

func validateRecord(record CSVRecord) ValidatedRecord {
	name := strings.TrimSpace(record.Fields["name"])
	email := strings.TrimSpace(record.Fields["email"])
	if name == "" {
		return ValidatedRecord{Record: record, Valid: false, Error: "missing required field: name"}
	}
	if !strings.Contains(email, "@") {
		return ValidatedRecord{Record: record, Valid: false, Error: fmt.Sprintf("invalid email: %s", email)}
	}
	return ValidatedRecord{Record: record, Valid: true}
}

func (di *DataImporter) Validate(ctx context.Context, in <-chan CSVRecord) <-chan ValidatedRecord {
	out := make(chan ValidatedRecord)
	go func() {
		defer close(out)
		for record := range in {
			select {
			case out <- validateRecord(record):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (di *DataImporter) enrichWorker(ctx context.Context, id int, in <-chan ValidatedRecord) <-chan EnrichedRecord {
	out := make(chan EnrichedRecord)
	go func() {
		defer close(out)
		for vr := range in {
			if !vr.Valid {
				select {
				case out <- EnrichedRecord{Record: vr.Record, Valid: false, Error: vr.Error, WorkerID: id}:
				case <-ctx.Done():
					return
				}
				continue
			}
			time.Sleep(enrichAPILatency)
			select {
			case out <- EnrichedRecord{
				Record: vr.Record, Valid: true, WorkerID: id,
				CompanyInfo: fmt.Sprintf("%s (verified)", vr.Record.Fields["company"]),
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (di *DataImporter) FanOutEnrich(ctx context.Context, in <-chan ValidatedRecord) <-chan EnrichedRecord {
	workers := make([]<-chan EnrichedRecord, di.enrichWorkers)
	for i := 0; i < di.enrichWorkers; i++ {
		workers[i] = di.enrichWorker(ctx, i+1, in)
	}

	out := make(chan EnrichedRecord)
	var wg sync.WaitGroup
	for _, ch := range workers {
		wg.Add(1)
		go func(c <-chan EnrichedRecord) {
			defer wg.Done()
			for er := range c {
				select {
				case out <- er:
				case <-ctx.Done():
					return
				}
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func generateTestRecords(names []string) <-chan CSVRecord {
	in := make(chan CSVRecord)
	go func() {
		defer close(in)
		for i, name := range names {
			in <- CSVRecord{
				LineNum: i + 1,
				Fields: map[string]string{
					"name":    name,
					"email":   fmt.Sprintf("%s@company.com", strings.ToLower(name)),
					"company": fmt.Sprintf("Company_%d", i+1),
				},
			}
		}
	}()
	return in
}

func main() {
	ctx := context.Background()
	names := []string{"Alice", "Bob", "", "Diana", "Eve", "Frank", "Grace",
		"Henry", "Ivy", "", "Kate", "Leo", "Mia", "Noah", "Olivia"}

	fmt.Printf("=== Fan-Out Enrichment (%d workers, %d records) ===\n\n", defaultEnrichWorkers, len(names))
	start := time.Now()

	importer := NewDataImporter(defaultEnrichWorkers)
	in := generateTestRecords(names)
	validated := importer.Validate(ctx, in)
	enriched := importer.FanOutEnrich(ctx, validated)

	var imported, skipped int
	for er := range enriched {
		if !er.Valid {
			skipped++
			fmt.Printf("  Line %2d: SKIP  - %s (worker %d)\n", er.Record.LineNum, er.Error, er.WorkerID)
		} else {
			imported++
			fmt.Printf("  Line %2d: OK    - %s [%s] (worker %d)\n",
				er.Record.LineNum, er.Record.Fields["name"], er.CompanyInfo, er.WorkerID)
		}
	}

	fmt.Printf("\n  Imported: %d, Skipped: %d, Total: %v\n", imported, skipped, time.Since(start))
}
```

### Verification
```bash
go run main.go
```
Expected: 13 imported, 2 skipped, distributed across 3 workers.

## Step 5 -- Full Pipeline with Cancellation and Progress Reporting

Build the complete system: CSV reader -> validate -> fan-out enrich -> write to DB -> report. Support cancellation via context and verify zero goroutine leaks.

```go
package main

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	enrichAPILatency   = 30 * time.Millisecond
	dbWriteLatency     = 5 * time.Millisecond
	goroutineSettleMs  = 100 * time.Millisecond
	cancelTimeout      = 100 * time.Millisecond
	defaultEnrichWorkers = 3
	totalCSVRecords    = 30
	statusImported     = "imported"
	statusSkipped      = "skipped"
)

// CSVRecord represents a single row from the CSV file.
type CSVRecord struct {
	LineNum int
	Fields  map[string]string
}

// ValidatedRecord carries the validation outcome for a CSV row.
type ValidatedRecord struct {
	Record CSVRecord
	Valid  bool
	Error  string
}

// EnrichedRecord adds external data to a validated row.
type EnrichedRecord struct {
	Record      CSVRecord
	CompanyInfo string
	Valid       bool
	Error       string
	WorkerID    int
}

// ImportResult is the final outcome for each row.
type ImportResult struct {
	LineNum int
	Status  string
	Detail  string
}

// DataImporter orchestrates the full CSV import pipeline with cancellation.
type DataImporter struct {
	enrichWorkers int
}

func NewDataImporter(enrichWorkers int) *DataImporter {
	return &DataImporter{enrichWorkers: enrichWorkers}
}

func (di *DataImporter) ReadCSV(ctx context.Context, data []map[string]string) <-chan CSVRecord {
	out := make(chan CSVRecord)
	go func() {
		defer close(out)
		for i, row := range data {
			record := CSVRecord{LineNum: i + 1, Fields: row}
			select {
			case out <- record:
			case <-ctx.Done():
				fmt.Printf("  [reader] stopped at line %d\n", i+1)
				return
			}
		}
	}()
	return out
}

func validateRecord(record CSVRecord) ValidatedRecord {
	name := strings.TrimSpace(record.Fields["name"])
	email := strings.TrimSpace(record.Fields["email"])
	if name == "" {
		return ValidatedRecord{Record: record, Valid: false, Error: "missing name"}
	}
	if !strings.Contains(email, "@") {
		return ValidatedRecord{Record: record, Valid: false, Error: "invalid email"}
	}
	return ValidatedRecord{Record: record, Valid: true}
}

func (di *DataImporter) Validate(ctx context.Context, in <-chan CSVRecord) <-chan ValidatedRecord {
	out := make(chan ValidatedRecord)
	go func() {
		defer close(out)
		for record := range in {
			select {
			case out <- validateRecord(record):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (di *DataImporter) enrichWorker(ctx context.Context, id int, in <-chan ValidatedRecord) <-chan EnrichedRecord {
	out := make(chan EnrichedRecord)
	go func() {
		defer close(out)
		for vr := range in {
			if !vr.Valid {
				select {
				case out <- EnrichedRecord{Record: vr.Record, Valid: false, Error: vr.Error, WorkerID: id}:
				case <-ctx.Done():
					return
				}
				continue
			}
			select {
			case <-time.After(enrichAPILatency):
			case <-ctx.Done():
				return
			}
			select {
			case out <- EnrichedRecord{
				Record: vr.Record, Valid: true, WorkerID: id,
				CompanyInfo: vr.Record.Fields["company"] + " (verified)",
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (di *DataImporter) FanOutEnrich(ctx context.Context, in <-chan ValidatedRecord) <-chan EnrichedRecord {
	workers := make([]<-chan EnrichedRecord, di.enrichWorkers)
	for i := 0; i < di.enrichWorkers; i++ {
		workers[i] = di.enrichWorker(ctx, i+1, in)
	}
	out := make(chan EnrichedRecord)
	var wg sync.WaitGroup
	for _, ch := range workers {
		wg.Add(1)
		go func(c <-chan EnrichedRecord) {
			defer wg.Done()
			for er := range c {
				select {
				case out <- er:
				case <-ctx.Done():
					return
				}
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func (di *DataImporter) WriteToDB(ctx context.Context, in <-chan EnrichedRecord) <-chan ImportResult {
	out := make(chan ImportResult)
	go func() {
		defer close(out)
		for er := range in {
			if !er.Valid {
				select {
				case out <- ImportResult{LineNum: er.Record.LineNum, Status: statusSkipped, Detail: er.Error}:
				case <-ctx.Done():
					return
				}
				continue
			}
			time.Sleep(dbWriteLatency)
			select {
			case out <- ImportResult{
				LineNum: er.Record.LineNum,
				Status:  statusImported,
				Detail:  fmt.Sprintf("%s -> %s", er.Record.Fields["name"], er.CompanyInfo),
			}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

func (di *DataImporter) RunPipeline(ctx context.Context, csvData []map[string]string) <-chan ImportResult {
	records := di.ReadCSV(ctx, csvData)
	validated := di.Validate(ctx, records)
	enriched := di.FanOutEnrich(ctx, validated)
	return di.WriteToDB(ctx, enriched)
}

func buildCSVData(names []string) []map[string]string {
	csvData := make([]map[string]string, len(names))
	for i, name := range names {
		email := ""
		if name != "" {
			email = strings.ToLower(name) + "@company.com"
		}
		csvData[i] = map[string]string{
			"name":    name,
			"email":   email,
			"company": fmt.Sprintf("Corp_%d", i+1),
		}
	}
	return csvData
}

func collectResults(results <-chan ImportResult) (imported, skipped int) {
	for r := range results {
		switch r.Status {
		case statusImported:
			imported++
		case statusSkipped:
			skipped++
			fmt.Printf("  Line %2d: SKIP - %s\n", r.LineNum, r.Detail)
		}
	}
	return
}

func checkGoroutineLeaks(label string, baseline int) {
	time.Sleep(goroutineSettleMs)
	current := runtime.NumGoroutine()
	leaked := current - baseline
	if leaked > 0 {
		fmt.Printf("  WARNING: %d goroutine(s) leaked %s\n", leaked, label)
	} else {
		fmt.Printf("  Goroutines: OK %s (before=%d, after=%d)\n", label, baseline, current)
	}
}

func main() {
	goroutinesBefore := runtime.NumGoroutine()

	names := []string{
		"Alice", "Bob", "", "Diana", "Eve", "Frank", "Grace", "Henry",
		"Ivy", "", "Kate", "Leo", "Mia", "Noah", "Olivia", "Pete",
		"Quinn", "Rose", "", "Sam", "Tina", "Uma", "Vic", "Wendy",
		"Xander", "Yara", "Zoe", "", "Aaron", "Beth",
	}
	csvData := buildCSVData(names)
	importer := NewDataImporter(defaultEnrichWorkers)

	// Run 1: Complete import (no cancellation)
	fmt.Printf("=== Complete Import (%d records) ===\n\n", totalCSVRecords)
	start := time.Now()
	results := importer.RunPipeline(context.Background(), csvData)
	imported, skipped := collectResults(results)

	fmt.Printf("\n  Imported: %d, Skipped: %d\n", imported, skipped)
	fmt.Printf("  Completed in %v\n", time.Since(start))
	checkGoroutineLeaks("", goroutinesBefore)

	// Run 2: User cancels import after 100ms
	fmt.Printf("\n=== Canceled Import (user cancels after %v) ===\n\n", cancelTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	start = time.Now()
	results2 := importer.RunPipeline(ctx, csvData)
	cancelImported, cancelSkipped := collectResults(results2)

	fmt.Printf("  Import canceled after %v\n", time.Since(start))
	fmt.Printf("  Partial results: %d imported, %d skipped\n", cancelImported, cancelSkipped)
	fmt.Printf("  (Remaining records were NOT processed -- clean shutdown)\n")
	checkGoroutineLeaks("after cancel", goroutinesBefore)
}
```

### Verification
```bash
go run main.go
```
Expected:
```
=== Complete Import (30 records) ===

  Line  3: SKIP - missing name
  Line 10: SKIP - missing name
  Line 19: SKIP - missing name
  Line 28: SKIP - missing name

  Imported: 26, Skipped: 4
  Completed in 350ms
  Goroutines: OK (before=2, after=2)

=== Canceled Import (user cancels after 100ms) ===

  [reader] stopped at line 12
  Import canceled after 102ms
  Partial results: 8 imported, 1 skipped
  (Remaining records were NOT processed -- clean shutdown)
  Goroutines: OK after cancel (before=2, after=2)
```

## Common Mistakes

### Not Checking ctx.Done() in Every Stage
**Wrong:**
```go
for record := range in {
	out <- processRecord(record) // no cancellation check
}
```
**What happens:** Even after cancel is called, the stage continues processing until the input channel closes, wasting CPU.

**Fix:** Always wrap sends in `select { case out <- result: case <-ctx.Done(): return }`.

### Goroutine Leak from Unclosed Channels
**Wrong:**
```go
func stage(in <-chan CSVRecord) <-chan CSVRecord {
	out := make(chan CSVRecord)
	go func() {
		for r := range in {
			out <- r
		}
		// forgot close(out)
	}()
	return out
}
```
**What happens:** Downstream stages that `range` over this output block forever.

**Fix:** Always `defer close(out)` at the top of the goroutine.

### Not Propagating Errors Through the Pipeline
**Wrong:** Dropping errors silently.
```go
if err != nil {
	continue // error swallowed, consumer never knows
}
```
**Fix:** Wrap errors in the result struct and forward them. Let the final collector decide how to handle errors.

### Using runtime.NumGoroutine Without a Settling Delay
After canceling, goroutines need a moment to respond to ctx.Done() and exit. Check the count after a brief sleep to avoid false positive leak reports.

## Review

A production pipeline is a composition: a generator feeds validate, validate
feeds a fan-out of enrichment workers, fan-in merges them, and a writer consumes
the result -- errors riding through as data on the struct, never dropped. The
rule that holds it together is that every goroutine checks `ctx.Done()` on every
blocking send and receive, so cancellation signalled anywhere propagates through
all stages, and every stage `defer close(out)`s so the shutdown cascade reaches
the consumer. Because the stages communicate only through channels, you scale the
design by adding stages or parallelizing the bottleneck without touching the
others, and you confirm the whole thing cleaned up with `runtime.NumGoroutine()`.

Running the program should let you confirm every claim yourself. The complete
import reports 26 imported and 4 skipped -- the four records with empty names --
and the goroutine count returns to its baseline. The canceled run stops the
reader partway, reports whatever imported before the deadline, and again leaves no
goroutines behind. Watch two things: that every stage honors `ctx.Done()`
(otherwise a canceled run keeps burning CPU until its input closes), and that
failures arrive as `ImportResult` values the collector counts rather than errors
swallowed with a bare `continue`. And because goroutines need a beat to observe
cancellation and exit, the leak check samples the count only after a short
settling sleep, so it does not report a false positive.

## Resources
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- the stage-ownership and done-channel model this pipeline generalizes.
- [Go Blog: Context](https://go.dev/blog/context) -- how context cancellation propagates through a call graph.
- [Concurrency in Go (Katherine Cox-Buday)](https://www.oreilly.com/library/view/concurrency-in-go/) -- a comprehensive reference for these composed patterns.
- [Advanced Go Concurrency Patterns](https://www.youtube.com/watch?v=QDDwwePbDtw) -- Sameer Ajmani's talk on combining select, context, and cancellation.

---

Back to [Concurrency](../../concurrency.md) | Next: [01-errgroup-basics](../../08-errgroup/01-errgroup-basics/01-errgroup-basics.md)
