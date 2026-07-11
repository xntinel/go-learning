# Exercise 6: Errgroup Parallel Pipeline -- Multi-Stage Data Processor

An ETL pipeline moves customer records through three stages: a reader pulling
from the source, a pool of workers validating and enriching each record, and a
writer persisting the results. Any stage can fail on its own -- a corrupt row, an
unreachable enrichment API, a dropped database connection -- and when one does,
the whole pipeline has to stop cleanly: the reader stops producing, the workers
finish their current record and exit, the writer flushes what it holds. Get the
shutdown wrong and you leak goroutines and strand partial work on every failed
run, which is why the real subject of this exercise is coordinated cancellation,
not the transformation itself.

## What you'll build

```text
06-errgroup-parallel-pipeline/
  main.go        Reader -> bounded Worker Pool -> Writer, wired through a
                 shared context that cancels every stage on the first error
```

- Build: an ETL pipeline whose three stages share one context and shut down together the moment any stage fails.
- Implement: a reader that closes its output with `defer close(out)`, a semaphore-bounded worker pool that closes its output only after `wg.Wait()`, a writer that drains until close, and a `runPipeline` coordinator built on `context.WithCancel`.
- Verify: `go run main.go`

### Why coordinated shutdown is the hard part

Your ETL system processes customer records through three stages:
1. **Reader**: Reads records from a data source (CSV file, API, database)
2. **Validator/Enricher**: Validates each record and enriches it with additional data (pool of N workers for throughput)
3. **Writer**: Writes valid records to the destination (database, API, file)

Each stage can fail independently: the reader might encounter a corrupt record, a validator might hit an unreachable enrichment API, the writer might get a database connection error. When any stage fails, the entire pipeline must shut down cleanly: the reader stops producing, workers finish their current record and stop, the writer flushes what it has.

Without coordinated shutdown, you get:
- Goroutines stuck trying to send to channels nobody reads
- The reader continues producing records that will never be processed
- Partial writes without proper cleanup
- Goroutine leaks that accumulate on every failed pipeline run

The architecture: **Reader -> channel -> Worker Pool (bounded) -> channel -> Writer**, all sharing a context for coordinated cancellation.

## Step 1 -- Pipeline Architecture

The pipeline processes records through three stages connected by channels:

```
[Reader] --recordsCh--> [Worker Pool (N workers)] --resultsCh--> [Writer]
     |                           |                                    |
     +------------ shared context (WithCancel) ----------------------+
```

When any stage fails:
1. It returns an error
2. The context is cancelled (via the error handler calling `cancel()`)
3. All other stages detect `ctx.Done()` and shut down
4. Channel close semantics propagate "done" signals: Reader -> Workers -> Writer

## Step 2 -- Build the Reader Stage

The reader sends records on a channel and closes it when done. The `defer close(out)` is critical -- without it, the workers' `range` loop never ends and the pipeline deadlocks:

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const readerLatencyPerRecord = 20 * time.Millisecond

type Record struct {
	ID      int
	Name    string
	Email   string
	Country string
	Amount  float64
}

type PipelineReader struct {
	records []Record
}

func NewPipelineReader(records []Record) *PipelineReader {
	return &PipelineReader{records: records}
}

func (pr *PipelineReader) ReadAll(ctx context.Context, out chan<- Record) error {
	defer close(out) // CRITICAL: signals workers that no more records are coming

	for _, rec := range pr.records {
		select {
		case <-ctx.Done():
			fmt.Printf("  [reader]  stopped: %v (sent %d/%d records)\n", ctx.Err(), rec.ID-1, len(pr.records))
			return ctx.Err()
		case out <- rec:
			fmt.Printf("  [reader]  sent record %d (%s)\n", rec.ID, rec.Name)
			time.Sleep(readerLatencyPerRecord)
		}
	}
	fmt.Printf("  [reader]  done: sent all %d records\n", len(pr.records))
	return nil
}

func main() {
	records := []Record{
		{ID: 1, Name: "Alice", Email: "alice@example.com", Country: "US", Amount: 120.00},
		{ID: 2, Name: "Bruno", Email: "bruno@example.com", Country: "DE", Amount: 80.50},
		{ID: 3, Name: "Chiyo", Email: "chiyo@example.com", Country: "JP", Amount: 240.75},
	}

	reader := NewPipelineReader(records)
	out := make(chan Record)
	errCh := make(chan error, 1)

	go func() { errCh <- reader.ReadAll(context.Background(), out) }()

	for rec := range out {
		fmt.Printf("  [main]    received record %d\n", rec.ID)
	}
	if err := <-errCh; err != nil {
		fmt.Printf("  [reader]  error: %v\n", err)
	}
}
```

Key design decisions:
- `defer close(out)` at the top ensures the channel is closed even if the reader returns early due to cancellation
- `select` on `ctx.Done()` allows the reader to stop if a downstream stage fails
- The reader is a single goroutine -- it is the producer

## Step 3 -- Build the Worker Pool Stage

The worker pool is the parallelized stage. It uses a semaphore channel to limit concurrency to N workers. Each worker validates the record, enriches it, and sends the result downstream:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const enrichmentLatency = 60 * time.Millisecond

type Record struct {
	ID      int
	Name    string
	Email   string
	Country string
	Amount  float64
}

type EnrichedRecord struct {
	Record
	TaxRate      float64
	TotalWithTax float64
	Region       string
	Valid        bool
}

type PipelineWorkerPool struct {
	numWorkers int
}

func NewPipelineWorkerPool(numWorkers int) *PipelineWorkerPool {
	return &PipelineWorkerPool{numWorkers: numWorkers}
}

func (wp *PipelineWorkerPool) Process(ctx context.Context, in <-chan Record, out chan<- EnrichedRecord) error {
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error

	sem := make(chan struct{}, wp.numWorkers)

	for rec := range in {
		select {
		case <-ctx.Done():
			go func() { for range in {} }()
			wg.Wait()
			close(out)
			return ctx.Err()
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			select {
			case <-ctx.Done():
				return
			default:
			}

			enriched, err := wp.validateAndEnrich(rec)
			if err != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("worker: record %d: %w", rec.ID, err)
				})
				return
			}

			select {
			case <-ctx.Done():
				return
			case out <- enriched:
				fmt.Printf("  [worker]  processed record %d (%s) -> $%.2f with tax\n",
					enriched.ID, enriched.Name, enriched.TotalWithTax)
			}
		}()
	}

	wg.Wait()
	close(out) // SAFE: all workers are done -- no more sends
	return firstErr
}

func (wp *PipelineWorkerPool) validateAndEnrich(rec Record) (EnrichedRecord, error) {
	time.Sleep(enrichmentLatency)

	if rec.Amount <= 0 {
		return EnrichedRecord{}, fmt.Errorf("invalid amount: $%.2f", rec.Amount)
	}
	if rec.Email == "" {
		return EnrichedRecord{}, fmt.Errorf("missing email for %s", rec.Name)
	}

	taxRate, region := wp.resolveTaxInfo(rec.Country)

	return EnrichedRecord{
		Record:       rec,
		TaxRate:      taxRate,
		TotalWithTax: rec.Amount * (1 + taxRate),
		Region:       region,
		Valid:        true,
	}, nil
}

func (wp *PipelineWorkerPool) resolveTaxInfo(country string) (float64, string) {
	switch country {
	case "US":
		return 0.08, "North America"
	case "DE", "FR":
		return 0.19, "Europe"
	case "JP":
		return 0.10, "Asia-Pacific"
	default:
		return 0.15, "International"
	}
}

func main() {
	records := []Record{
		{ID: 1, Name: "Alice", Email: "alice@example.com", Country: "US", Amount: 120.00},
		{ID: 2, Name: "Bruno", Email: "bruno@example.com", Country: "DE", Amount: 80.50},
		{ID: 3, Name: "Chiyo", Email: "chiyo@example.com", Country: "JP", Amount: 240.75},
	}

	in := make(chan Record)
	out := make(chan EnrichedRecord)

	go func() {
		defer close(in)
		for _, rec := range records {
			in <- rec
		}
	}()

	pool := NewPipelineWorkerPool(2)
	errCh := make(chan error, 1)
	go func() { errCh <- pool.Process(context.Background(), in, out) }()

	count := 0
	for range out {
		count++
	}
	if err := <-errCh; err != nil {
		fmt.Printf("  [pool]    error: %v\n", err)
	}
	fmt.Printf("  [main]    enriched %d records with %d workers\n", count, pool.numWorkers)
}
```

Critical design decisions:
- **Semaphore `sem`** ensures at most N records are processed concurrently
- **`range in`** reads from the input channel until the reader closes it
- **`close(out)` comes AFTER `wg.Wait()`** -- closing before Wait means a still-running worker panics on send
- Context check before semaphore acquire prevents blocking when pipeline is shutting down

## Step 4 -- Build the Writer Stage

The writer collects results from the output channel. It could write to a database, file, or API. Here it aggregates into a results slice:

```go
package main

import (
	"context"
	"fmt"
	"sync"
)

type Record struct {
	ID      int
	Name    string
	Email   string
	Country string
	Amount  float64
}

type EnrichedRecord struct {
	Record
	TaxRate      float64
	TotalWithTax float64
	Region       string
	Valid        bool
}

type PipelineStats struct {
	Processed int
	TotalTax  float64
	Revenue   float64
}

type PipelineWriter struct {
	mu      *sync.Mutex
	results *[]EnrichedRecord
	stats   *PipelineStats
}

func NewPipelineWriter(mu *sync.Mutex, results *[]EnrichedRecord, stats *PipelineStats) *PipelineWriter {
	return &PipelineWriter{mu: mu, results: results, stats: stats}
}

func (pw *PipelineWriter) WriteAll(ctx context.Context, in <-chan EnrichedRecord) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case rec, ok := <-in:
			if !ok {
				fmt.Printf("  [writer]  done: received all results\n")
				return nil
			}
			pw.collectResult(rec)
			fmt.Printf("  [writer]  wrote record %d (%s, %s)\n", rec.ID, rec.Name, rec.Region)
		}
	}
}

func (pw *PipelineWriter) collectResult(rec EnrichedRecord) {
	pw.mu.Lock()
	defer pw.mu.Unlock()
	*pw.results = append(*pw.results, rec)
	pw.stats.Processed++
	pw.stats.Revenue += rec.Amount
	pw.stats.TotalTax += rec.TotalWithTax - rec.Amount
}

func main() {
	var mu sync.Mutex
	var results []EnrichedRecord
	stats := &PipelineStats{}

	in := make(chan EnrichedRecord)
	go func() {
		defer close(in)
		in <- EnrichedRecord{
			Record:       Record{ID: 1, Name: "Alice", Country: "US", Amount: 120.00},
			TaxRate:      0.08,
			TotalWithTax: 129.60,
			Region:       "North America",
			Valid:        true,
		}
		in <- EnrichedRecord{
			Record:       Record{ID: 2, Name: "Bruno", Country: "DE", Amount: 80.50},
			TaxRate:      0.19,
			TotalWithTax: 95.80,
			Region:       "Europe",
			Valid:        true,
		}
	}()

	writer := NewPipelineWriter(&mu, &results, stats)
	if err := writer.WriteAll(context.Background(), in); err != nil {
		fmt.Printf("  [writer]  error: %v\n", err)
	}

	fmt.Printf("\n  processed=%d revenue=$%.2f tax=$%.2f\n",
		stats.Processed, stats.Revenue, stats.TotalTax)
}
```

The writer reads until the channel is closed by the worker pool. It uses a mutex because the results and stats pointers are shared with the caller.

## Step 5 -- Wire Everything Together

The `runPipeline` function ties all stages together with a shared context. When any stage fails, the context is cancelled and all stages shut down:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	readerLatencyPerRecord = 20 * time.Millisecond
	enrichmentLatency      = 60 * time.Millisecond
	defaultWorkerCount     = 3
)

type Record struct {
	ID      int
	Name    string
	Email   string
	Country string
	Amount  float64
}

type EnrichedRecord struct {
	Record
	TaxRate      float64
	TotalWithTax float64
	Region       string
	Valid        bool
}

type PipelineStats struct {
	Processed int
	TotalTax  float64
	Revenue   float64
}

type PipelineRunner struct {
	records    []Record
	numWorkers int
}

func NewPipelineRunner(records []Record, numWorkers int) *PipelineRunner {
	return &PipelineRunner{records: records, numWorkers: numWorkers}
}

func (pr *PipelineRunner) Run() ([]EnrichedRecord, PipelineStats, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	recordsCh := make(chan Record)
	resultsCh := make(chan EnrichedRecord)
	var mu sync.Mutex
	var results []EnrichedRecord
	var stats PipelineStats

	var pipelineWg sync.WaitGroup
	var pipelineOnce sync.Once
	var pipelineErr error

	captureError := func(err error) {
		if err != nil {
			pipelineOnce.Do(func() {
				pipelineErr = err
				cancel()
			})
		}
	}

	pipelineWg.Add(1)
	go func() {
		defer pipelineWg.Done()
		captureError(pr.readRecords(ctx, recordsCh))
	}()

	pipelineWg.Add(1)
	go func() {
		defer pipelineWg.Done()
		captureError(pr.processRecords(ctx, recordsCh, resultsCh))
	}()

	pipelineWg.Add(1)
	go func() {
		defer pipelineWg.Done()
		captureError(pr.writeResults(ctx, resultsCh, &mu, &results, &stats))
	}()

	pipelineWg.Wait()
	return results, stats, pipelineErr
}

func (pr *PipelineRunner) readRecords(ctx context.Context, out chan<- Record) error {
	defer close(out)
	for _, rec := range pr.records {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- rec:
			time.Sleep(readerLatencyPerRecord)
		}
	}
	return nil
}

func (pr *PipelineRunner) processRecords(ctx context.Context, in <-chan Record, out chan<- EnrichedRecord) error {
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	sem := make(chan struct{}, pr.numWorkers)

	for rec := range in {
		select {
		case <-ctx.Done():
			go func() { for range in {} }()
			wg.Wait()
			close(out)
			return ctx.Err()
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			select {
			case <-ctx.Done():
				return
			default:
			}

			enriched, err := pr.validateAndEnrich(rec)
			if err != nil {
				once.Do(func() {
					firstErr = fmt.Errorf("record %d (%s): %w", rec.ID, rec.Name, err)
				})
				return
			}

			select {
			case <-ctx.Done():
				return
			case out <- enriched:
			}
		}()
	}

	wg.Wait()
	close(out)
	return firstErr
}

func (pr *PipelineRunner) validateAndEnrich(rec Record) (EnrichedRecord, error) {
	time.Sleep(enrichmentLatency)

	if rec.Amount <= 0 {
		return EnrichedRecord{}, fmt.Errorf("invalid amount: $%.2f", rec.Amount)
	}
	if rec.Email == "" {
		return EnrichedRecord{}, fmt.Errorf("missing email")
	}

	taxRate, region := pr.resolveTaxInfo(rec.Country)

	return EnrichedRecord{
		Record:       rec,
		TaxRate:      taxRate,
		TotalWithTax: rec.Amount * (1 + taxRate),
		Region:       region,
		Valid:        true,
	}, nil
}

func (pr *PipelineRunner) resolveTaxInfo(country string) (float64, string) {
	switch country {
	case "US":
		return 0.08, "North America"
	case "DE", "FR":
		return 0.19, "Europe"
	case "JP":
		return 0.10, "Asia-Pacific"
	default:
		return 0.15, "International"
	}
}

func (pr *PipelineRunner) writeResults(ctx context.Context, in <-chan EnrichedRecord, mu *sync.Mutex, results *[]EnrichedRecord, stats *PipelineStats) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case rec, ok := <-in:
			if !ok {
				return nil
			}
			mu.Lock()
			*results = append(*results, rec)
			stats.Processed++
			stats.Revenue += rec.Amount
			stats.TotalTax += rec.TotalWithTax - rec.Amount
			mu.Unlock()
		}
	}
}

func printResults(results []EnrichedRecord, stats PipelineStats, err error) {
	if err != nil {
		fmt.Printf("\nPipeline ERROR: %v\n", err)
	}

	fmt.Printf("\nProcessed %d records:\n", stats.Processed)
	for _, r := range results {
		fmt.Printf("  #%d %-18s %-15s $%8.2f -> $%8.2f (tax: %.0f%%)\n",
			r.ID, r.Name, r.Region, r.Amount, r.TotalWithTax, r.TaxRate*100)
	}
	fmt.Printf("\nRevenue: $%.2f | Tax: $%.2f | Total: $%.2f\n",
		stats.Revenue, stats.TotalTax, stats.Revenue+stats.TotalTax)
}

func main() {
	records := []Record{
		{1, "Alice Johnson", "alice@example.com", "US", 299.99},
		{2, "Hans Mueller", "hans@example.de", "DE", 449.50},
		{3, "Yuki Tanaka", "yuki@example.jp", "JP", 189.00},
		{4, "Marie Dupont", "marie@example.fr", "FR", 520.00},
		{5, "Bob Smith", "bob@example.com", "US", 75.00},
		{6, "Chen Wei", "chen@example.cn", "CN", 330.00},
		{7, "Sara Lopez", "sara@example.mx", "MX", 210.50},
		{8, "James Brown", "james@example.com", "US", 155.00},
	}

	fmt.Printf("=== Data Processing Pipeline (%d records, %d workers) ===\n\n", len(records), defaultWorkerCount)

	fmt.Println("--- Scenario 1: All records valid ---")
	pipeline := NewPipelineRunner(records, defaultWorkerCount)
	results, stats, err := pipeline.Run()
	printResults(results, stats, err)

	fmt.Println("\n--- Scenario 2: Record 4 has invalid amount ---")
	badRecords := make([]Record, len(records))
	copy(badRecords, records)
	badRecords[3].Amount = -50.00
	pipeline = NewPipelineRunner(badRecords, defaultWorkerCount)
	results, stats, err = pipeline.Run()
	printResults(results, stats, err)
}
```

**Expected output (Scenario 1 - all valid):**
```
=== Data Processing Pipeline (8 records, 3 workers) ===

--- Scenario 1: All records valid ---

Processed 8 records:
  #1 Alice Johnson      North America    $  299.99 -> $  323.99 (tax: 8%)
  #2 Hans Mueller       Europe           $  449.50 -> $  534.91 (tax: 19%)
  #3 Yuki Tanaka        Asia-Pacific     $  189.00 -> $  207.90 (tax: 10%)
  #4 Marie Dupont       Europe           $  520.00 -> $  618.80 (tax: 19%)
  #5 Bob Smith          North America    $   75.00 -> $   81.00 (tax: 8%)
  #6 Chen Wei           International    $  330.00 -> $  379.50 (tax: 15%)
  #7 Sara Lopez         International    $  210.50 -> $  242.08 (tax: 15%)
  #8 James Brown        North America    $  155.00 -> $  167.40 (tax: 8%)

Revenue: $2228.99 | Tax: $326.58 | Total: $2555.57
```

**Expected output (Scenario 2 - record 4 invalid):**
```
--- Scenario 2: Record 4 has invalid amount ---

Pipeline ERROR: record 4 (Marie Dupont): invalid amount: $-50.00

Processed 3 records:
  #1 Alice Johnson      North America    $  299.99 -> $  323.99 (tax: 8%)
  #2 Hans Mueller       Europe           $  449.50 -> $  534.91 (tax: 19%)
  #3 Yuki Tanaka        Asia-Pacific     $  189.00 -> $  207.90 (tax: 10%)

Revenue: $938.49 | Tax: $138.31 | Total: $1076.80
```

The beauty of this design:
- A shared context connects all three stages
- If the reader fails, it closes its output channel, workers finish current work and close their output, writer sees the closed channel and returns
- If a worker fails, `cancel()` is called, reader stops producing, other workers bail out, writer detects cancellation
- Channel close semantics propagate "done" signals naturally: Reader -> Workers -> Writer
- Partial results are available even after an error

The `golang.org/x/sync/errgroup` package simplifies the top-level coordination:

```go
// With errgroup.WithContext, the runPipeline function becomes:
//   g, ctx := errgroup.WithContext(context.Background())
//   g.Go(func() error { return reader(ctx, records, recordsCh) })
//   g.Go(func() error { return workerPool(ctx, numWorkers, recordsCh, resultsCh) })
//   g.Go(func() error { return writer(ctx, resultsCh, &mu, &results, &stats) })
//   err := g.Wait()
//
// No pipelineWg, no pipelineOnce, no captureError function.
// errgroup.WithContext automatically cancels the context on first error.
```

## Verification

At this point, verify:
1. The successful pipeline processes all 8 records with correct tax calculations
2. The failing pipeline stops after the error and produces partial results
3. All goroutines exit cleanly (no goroutine leaks)
4. Workers run with bounded concurrency (3 at a time)

## Common Mistakes

### Forgetting to close channels

**Wrong:**
```go
func reader(ctx context.Context, out chan<- Record) error {
    for _, rec := range records {
        out <- rec
    }
    // forgot close(out)!
    return nil
}
```

**What happens:** The workers' `range in` loop never ends. The pipeline deadlocks because nobody signals "no more data."

**Fix:** Always `defer close(out)` at the top of the producing function.

### Closing the output channel before workers finish

**Wrong:**
```go
func workerPool(..., out chan<- EnrichedRecord) error {
    for rec := range in {
        go func() {
            out <- result
        }()
    }
    close(out) // BUG: workers may still be sending!
    return nil
}
```

**What happens:** A worker that is still running sends on the closed channel -- panic: `send on closed channel`.

**Fix:** Wait for all workers to finish, then close:
```go
wg.Wait()
close(out) // SAFE: all workers are done
```

### Not checking ctx.Done() on channel sends

**Wrong:**
```go
out <- result // blocks forever if writer is cancelled
```

**What happens:** If the writer stops reading (due to cancellation), this send blocks forever. The pipeline deadlocks instead of shutting down.

**Fix:** Always pair channel sends with `ctx.Done()`:
```go
select {
case <-ctx.Done():
    return ctx.Err()
case out <- result:
}
```

### Using buffered channels to "fix" deadlocks

**Wrong:**
```go
recordsCh := make(chan Record, 10000) // large buffer to avoid blocking
```

**What happens:** Buffers mask the real problem. If a stage fails, the pipeline should shut down promptly. Large buffers mean the reader keeps filling the channel long after the context is cancelled, wasting CPU and memory. The correct fix is `ctx.Done()` checks on sends, not bigger buffers.

**Fix:** Use unbuffered channels and proper `ctx.Done()` checks. Unbuffered channels enforce backpressure -- the reader cannot get ahead of the workers.

### Not draining the input channel on cancellation

**Subtle bug:**
```go
func workerPool(ctx context.Context, in <-chan Record, out chan<- EnrichedRecord) error {
    for rec := range in {
        select {
        case <-ctx.Done():
            // EXIT: but the reader is blocked on `out <- rec`!
            close(out)
            return ctx.Err()
        default:
        }
        // ... process
    }
}
```

**What happens:** The reader is blocked trying to send on `in`. If the worker pool exits without draining `in`, the reader's goroutine is stuck forever.

**Fix:** Drain the input channel in a goroutine before exiting:
```go
go func() { for range in {} }() // drain to unblock reader
wg.Wait()
close(out)
```

## Review

A pipeline is a chain of stages connected by channels, and its correctness lives
almost entirely in channel lifecycle and cancellation. One `context.WithCancel`
is the top-level coordinator: a single context, a single cancellation signal
that every stage watches through `select` on `ctx.Done()` for both its sends and
its receives, so a failure anywhere propagates everywhere. Ownership of closing
is strict -- a producer closes its own output with `defer close(out)`, while a
worker pool closes its output only after `wg.Wait()`, because closing while a
worker is still sending panics on a closed channel. Bounded parallelism inside a
stage comes from a semaphore channel of fixed capacity, and unbuffered channels
between stages give real backpressure that large buffers would only mask. The
last, easy-to-miss detail is draining the input channel on cancellation so a
blocked upstream producer is not stranded. Once you see this shape,
`golang.org/x/sync/errgroup.WithContext` is the natural simplification: it folds
the WaitGroup, the first-error capture, and the context cancellation into one
object.

Run the full program and confirm the design holds in both scenarios. With all
records valid, all eight are processed with correct tax calculations. With
record 4's amount made invalid, the pipeline stops after that failure and still
yields partial results for records 1 through 3 -- no deadlock, no panic, and the
worker pool never exceeds its bound of three in flight. If either scenario hangs
or panics, one of the closing or `ctx.Done()` rules above was violated, and the
symptom points straight at which one.

## Resources
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- the reference construction for staged channels and who closes what.
- [context.WithCancel documentation](https://pkg.go.dev/context#WithCancel) -- the cancellation primitive that ties all stages to one signal.
- [errgroup package documentation](https://pkg.go.dev/golang.org/x/sync/errgroup) -- the helper that combines WaitGroup, first-error capture, and context cancellation.
- [Bryan Mills: Rethinking Classical Concurrency Patterns (GopherCon 2018)](https://www.youtube.com/watch?v=5zXAHh5tJqQ) -- why draining and cancellation matter, and the failure modes of naive pipelines.

---

Back to [Concurrency](../../concurrency.md) | Next: [01-your-first-data-race](../../09-data-races-and-detector/01-your-first-data-race/01-your-first-data-race.md)
