# Exercise 6: Generator: Lazy Production

A generator is a function that returns a channel and produces values in a
background goroutine, with the consumer setting the pace: stop reading and the
generator blocks on its send. That is lazy evaluation through backpressure, and
it is why a paginated query can fetch page one, hand it over, and never touch
pages three through two hundred if the user stops scrolling. The failure mode to
respect is the leak -- a generator with no way to be told "stop" leaves its
goroutine blocked on a send forever.

## What you'll build

```text
06-generator-lazy-production/
  main.go        a paginated generator, early cancellation with a done
                 channel, an eager-vs-lazy memory comparison, and a
                 context-driven variant
```

- Build: a paginated data generator that fetches pages lazily and stops cleanly on cancellation.
- Implement: `PageGenerator.FetchPages` returning `<-chan Page`, a `FetchWithCancel` using a `done` channel, an eager-vs-lazy memory comparison, and a `QueryPages` variant driven by `context.Context`.
- Verify: `go run main.go`.

### Why backpressure makes generators lazy

Consider a real scenario: your application queries a database that returns 10,000 records, but the user only views the first 2 pages (50 records). Loading all 10,000 records upfront wastes database bandwidth, memory, and time. A paginated generator fetches one page at a time, on demand. If the user stops scrolling after page 2, the generator never fetches pages 3-200. This is the same principle behind database cursors and iterator patterns, implemented naturally with Go channels.

The key insight is that an unbuffered channel naturally synchronizes the producer and consumer. The producer only runs when the consumer is ready to receive. This makes generators memory-efficient even for huge datasets -- only one page exists in memory at a time.

```
  Paginated Database Generator

  generator() returns <-chan Page immediately
  Background goroutine fetches pages lazily:

  goroutine:  [fetch page 1] -> [send] -> [block] -> [fetch page 2] -> [send] -> [block]
  consumer:                  <- [recv]              <- [recv]         <- [stop]

  Only fetches what the consumer requests. No wasted work.
```

## Step 1 -- Basic Paginated Generator

Create a generator that simulates fetching database pages on demand.

```go
package main

import (
	"fmt"
	"time"
)

const dbQueryLatency = 50 * time.Millisecond

// Record represents a single database row.
type Record struct {
	ID   int
	Name string
}

// Page holds a batch of records fetched in one query.
type Page struct {
	Number  int
	Records []Record
}

// PageGenerator lazily fetches database pages on demand.
type PageGenerator struct {
	totalRecords int
	pageSize     int
}

func NewPageGenerator(totalRecords, pageSize int) *PageGenerator {
	return &PageGenerator{totalRecords: totalRecords, pageSize: pageSize}
}

func buildPage(pageNum, offset, end int) Page {
	records := make([]Record, 0, end-offset)
	for i := offset; i < end; i++ {
		records = append(records, Record{
			ID:   i + 1,
			Name: fmt.Sprintf("record_%04d", i+1),
		})
	}
	return Page{Number: pageNum, Records: records}
}

func (pg *PageGenerator) FetchPages() <-chan Page {
	out := make(chan Page)
	go func() {
		defer close(out)
		pageNum := 1
		for offset := 0; offset < pg.totalRecords; offset += pg.pageSize {
			end := offset + pg.pageSize
			if end > pg.totalRecords {
				end = pg.totalRecords
			}
			time.Sleep(dbQueryLatency)
			out <- buildPage(pageNum, offset, end)
			pageNum++
		}
	}()
	return out
}

func main() {
	fmt.Println("=== Paginated Generator (fetch all) ===")
	start := time.Now()

	gen := NewPageGenerator(50, 10)
	pages := gen.FetchPages()

	var totalRecords int
	for page := range pages {
		totalRecords += len(page.Records)
		fmt.Printf("  Page %d: %d records (first: %s, last: %s)\n",
			page.Number, len(page.Records),
			page.Records[0].Name,
			page.Records[len(page.Records)-1].Name)
	}
	fmt.Printf("  Total: %d records fetched in %v\n", totalRecords, time.Since(start))
}
```

The function returns immediately with the channel. Pages are fetched lazily as the consumer reads.

### Verification
```bash
go run main.go
```
Expected:
```
=== Paginated Generator (fetch all) ===
  Page 1: 10 records (first: record_0001, last: record_0010)
  Page 2: 10 records (first: record_0011, last: record_0020)
  Page 3: 10 records (first: record_0021, last: record_0030)
  Page 4: 10 records (first: record_0031, last: record_0040)
  Page 5: 10 records (first: record_0041, last: record_0050)
  Total: 50 records fetched in 252ms
```

## Step 2 -- Early Cancellation with Done Channel

Show memory efficiency: the consumer only needs 2 pages out of 100. Without cancellation the generator goroutine leaks. Fix it with a done channel.

```go
package main

import (
	"fmt"
	"time"
)

const (
	dbQueryLatency     = 50 * time.Millisecond
	pagesNeededForDemo = 3
)

// Record represents a single database row.
type Record struct {
	ID   int
	Name string
}

// Page holds a batch of records fetched in one query.
type Page struct {
	Number  int
	Records []Record
}

// PageGenerator lazily fetches database pages with cancellation support.
type PageGenerator struct {
	totalRecords int
	pageSize     int
}

func NewPageGenerator(totalRecords, pageSize int) *PageGenerator {
	return &PageGenerator{totalRecords: totalRecords, pageSize: pageSize}
}

func buildPage(pageNum, offset, end int) Page {
	records := make([]Record, 0, end-offset)
	for i := offset; i < end; i++ {
		records = append(records, Record{
			ID:   i + 1,
			Name: fmt.Sprintf("record_%04d", i+1),
		})
	}
	return Page{Number: pageNum, Records: records}
}

func (pg *PageGenerator) FetchWithCancel(done <-chan struct{}) <-chan Page {
	out := make(chan Page)
	go func() {
		defer close(out)
		pageNum := 1
		for offset := 0; offset < pg.totalRecords; offset += pg.pageSize {
			end := offset + pg.pageSize
			if end > pg.totalRecords {
				end = pg.totalRecords
			}

			time.Sleep(dbQueryLatency)
			fmt.Printf("    [generator] fetched page %d from DB\n", pageNum)

			select {
			case out <- buildPage(pageNum, offset, end):
				pageNum++
			case <-done:
				fmt.Printf("    [generator] canceled at page %d, stopping early\n", pageNum)
				return
			}
		}
		fmt.Println("    [generator] all pages sent")
	}()
	return out
}

func demoFetchAll(gen *PageGenerator) {
	fmt.Println("=== Without Early Stop: Fetch All 100 Pages ===")
	start := time.Now()
	done := make(chan struct{})
	pages := gen.FetchWithCancel(done)

	var count int
	for range pages {
		count++
	}
	close(done)
	fmt.Printf("  Fetched %d pages in %v\n\n", count, time.Since(start))
}

func demoEarlyStop(gen *PageGenerator) {
	fmt.Println("=== With Early Stop: Only Need 3 Pages ===")
	start := time.Now()
	done := make(chan struct{})
	pages := gen.FetchWithCancel(done)

	var count int
	for page := range pages {
		count++
		fmt.Printf("  Consumer got page %d (%d records)\n", page.Number, len(page.Records))
		if count >= pagesNeededForDemo {
			close(done)
			break
		}
	}
	fmt.Printf("  Fetched only %d pages in %v (saved 97 unnecessary queries)\n",
		count, time.Since(start))
}

func main() {
	gen := NewPageGenerator(1000, 10)
	demoFetchAll(gen)
	demoEarlyStop(gen)
}
```

The `select` statement lets the goroutine listen for both "consumer wants a page" and "consumer is done". Closing the `done` channel unblocks the `<-done` case and the goroutine exits cleanly.

### Verification
```bash
go run main.go
```
Expected: the second scenario is dramatically faster:
```
=== Without Early Stop: Fetch All 100 Pages ===
    [generator] fetched page 1 from DB
    ...
    [generator] fetched page 100 from DB
    [generator] all pages sent
  Fetched 100 pages in 5.1s

=== With Early Stop: Only Need 3 Pages ===
    [generator] fetched page 1 from DB
  Consumer got page 1 (10 records)
    [generator] fetched page 2 from DB
  Consumer got page 2 (10 records)
    [generator] fetched page 3 from DB
  Consumer got page 3 (10 records)
    [generator] canceled at page 4, stopping early
  Fetched only 3 pages in 153ms (saved 97 unnecessary queries)
```

## Step 3 -- Memory Comparison: Lazy vs Eager

Demonstrate the memory difference between loading all data upfront and using a lazy generator.

```go
package main

import (
	"fmt"
	"time"
)

const (
	totalRecordCount  = 10000
	recordPageSize    = 100
	pagesNeeded       = 3
	estimatedRecordKB = 280
)

// Record represents a database row with a payload.
type Record struct {
	ID   int
	Name string
	Data [256]byte
}

// MemoryComparison demonstrates eager vs lazy loading memory impact.
type MemoryComparison struct {
	totalRecords int
	pageSize     int
	pagesNeeded  int
}

func NewMemoryComparison() *MemoryComparison {
	return &MemoryComparison{
		totalRecords: totalRecordCount,
		pageSize:     recordPageSize,
		pagesNeeded:  pagesNeeded,
	}
}

func (mc *MemoryComparison) RunEager() {
	fmt.Println("=== Eager Loading ===")
	start := time.Now()

	allRecords := make([]Record, mc.totalRecords)
	for i := range allRecords {
		allRecords[i] = Record{ID: i + 1, Name: fmt.Sprintf("rec_%d", i+1)}
	}

	usedCount := mc.pagesNeeded * mc.pageSize
	wastedCount := len(allRecords) - usedCount

	fmt.Printf("  Allocated %d records (%d KB)\n", len(allRecords), len(allRecords)*estimatedRecordKB/1024)
	fmt.Printf("  Actually used: %d records\n", usedCount)
	fmt.Printf("  Wasted: %d records (%d KB)\n", wastedCount, wastedCount*estimatedRecordKB/1024)
	fmt.Printf("  Time: %v\n\n", time.Since(start))
}

func (mc *MemoryComparison) generatePages(done <-chan struct{}) <-chan []Record {
	out := make(chan []Record)
	go func() {
		defer close(out)
		for offset := 0; offset < mc.totalRecords; offset += mc.pageSize {
			end := offset + mc.pageSize
			if end > mc.totalRecords {
				end = mc.totalRecords
			}
			page := make([]Record, end-offset)
			for i := range page {
				page[i] = Record{ID: offset + i + 1, Name: fmt.Sprintf("rec_%d", offset+i+1)}
			}
			select {
			case out <- page:
			case <-done:
				return
			}
		}
	}()
	return out
}

func (mc *MemoryComparison) RunLazy() {
	fmt.Println("=== Lazy Generator ===")
	start := time.Now()

	done := make(chan struct{})
	pages := mc.generatePages(done)

	var lazyUsed, pageCount int
	for page := range pages {
		pageCount++
		lazyUsed += len(page)
		if pageCount >= mc.pagesNeeded {
			close(done)
			break
		}
	}

	fmt.Printf("  Allocated only %d records at a time (%d KB per page)\n",
		mc.pageSize, mc.pageSize*estimatedRecordKB/1024)
	fmt.Printf("  Total records processed: %d\n", lazyUsed)
	fmt.Printf("  Pages fetched: %d out of %d possible\n", pageCount, mc.totalRecords/mc.pageSize)
	fmt.Printf("  Time: %v\n", time.Since(start))
}

func main() {
	comparison := NewMemoryComparison()
	comparison.RunEager()
	comparison.RunLazy()
}
```

### Verification
```bash
go run main.go
```
```
=== Eager Loading ===
  Allocated 10000 records (2734 KB)
  Actually used: 300 records
  Wasted: 9700 records (2652 KB)
  Time: 5.2ms

=== Lazy Generator ===
  Allocated only 100 records at a time (27 KB per page)
  Total records processed: 300
  Pages fetched: 3 out of 100 possible
  Time: 0.4ms
```

## Step 4 -- Composable Generator with Context

Use `context.Context` instead of a raw done channel for production-ready cancellation.

```go
package main

import (
	"context"
	"fmt"
	"time"
)

const (
	simulatedTotalResults = 85
	contextQueryLatency   = 30 * time.Millisecond
	contextQueryTimeout   = 100 * time.Millisecond
)

// Record represents a single database row.
type Record struct {
	ID   int
	Name string
}

// Page holds a batch of records with pagination metadata.
type Page struct {
	Number  int
	Records []Record
	HasMore bool
}

// PageGenerator lazily fetches pages with context-based cancellation.
type PageGenerator struct {
	totalResults int
	pageSize     int
	query        string
}

func NewPageGenerator(query string, pageSize int) *PageGenerator {
	return &PageGenerator{
		totalResults: simulatedTotalResults,
		pageSize:     pageSize,
		query:        query,
	}
}

func (pg *PageGenerator) buildPage(pageNum, offset, end int) Page {
	records := make([]Record, 0, end-offset)
	for i := offset; i < end; i++ {
		records = append(records, Record{
			ID:   i + 1,
			Name: fmt.Sprintf("match_%d_for_%s", i+1, pg.query),
		})
	}
	return Page{
		Number:  pageNum,
		Records: records,
		HasMore: end < pg.totalResults,
	}
}

func (pg *PageGenerator) QueryPages(ctx context.Context) <-chan Page {
	out := make(chan Page)
	go func() {
		defer close(out)
		pageNum := 1
		for offset := 0; offset < pg.totalResults; offset += pg.pageSize {
			end := offset + pg.pageSize
			if end > pg.totalResults {
				end = pg.totalResults
			}

			time.Sleep(contextQueryLatency)

			select {
			case out <- pg.buildPage(pageNum, offset, end):
				pageNum++
			case <-ctx.Done():
				fmt.Printf("    [query] canceled: %v\n", ctx.Err())
				return
			}
		}
	}()
	return out
}

func main() {
	fmt.Println("=== Paginated Query with Context ===")
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), contextQueryTimeout)
	defer cancel()

	gen := NewPageGenerator("laptop", 25)
	for page := range gen.QueryPages(ctx) {
		fmt.Printf("  Page %d: %d results (hasMore: %v)\n",
			page.Number, len(page.Records), page.HasMore)
	}
	fmt.Println("\n  Generator stopped cleanly when context expired.")
}
```

### Verification
```bash
go run main.go
```
```
=== Paginated Query with Context ===

  Page 1: 25 results (hasMore: true)
  Page 2: 25 results (hasMore: true)
    [query] canceled: context deadline exceeded

  Generator stopped cleanly when context expired.
```

## Common Mistakes

### Goroutine Leak from Generators Without Cancellation
**Wrong:**
```go
pages := fetchPages(10000, 10)
firstPage := <-pages
// goroutine inside fetchPages() is stuck on send forever
```
**What happens:** The goroutine never exits. In a long-running service, this accumulates leaked goroutines.

**Fix:** Always use a `done` channel or `context.Context` with generators so you can signal the producer to stop.

### Buffering the Generator Channel
**Wrong:**
```go
out := make(chan Page, 100) // pre-produces 100 pages
```
**What happens:** The generator eagerly produces 100 pages before any consumer reads. This wastes memory and database connections, defeating laziness.

**Fix:** Use unbuffered channels for true lazy evaluation. Only buffer when you have measured a performance need.

### Closing a Channel Twice
**Wrong:**
```go
go func() {
	for i := 0; i < 10; i++ {
		out <- page
	}
	close(out)
	// ... later, done channel triggers
	close(out) // panic: close of closed channel
}()
```
**Fix:** Use `defer close(out)` once, and structure the goroutine to have a single exit path.

## Review

A generator is a function that returns `<-chan T` and produces values in a
background goroutine, and an unbuffered channel gives it natural backpressure:
each page is fetched only when the consumer is ready to receive it, so only one
page ever lives in memory at a time no matter how large the dataset. That
laziness is the whole point -- it avoids the database queries, memory, and time
that eager loading wastes when a consumer needs only a subset, exactly the way
database cursors, API pagination, and file streaming work. The one
non-negotiable is a cancellation mechanism: a `done` channel or a
`context.Context` in the send `select`, so that when the consumer stops early the
generator's goroutine exits instead of blocking forever on a send nobody will
receive.

Running `go run main.go` should show all four behaviors at once: the basic
generator fetching every page when fully consumed, early cancellation stopping
after three pages and skipping the other ninety-seven queries, lazy loading
touching a fraction of the memory that eager loading allocates for partial
consumption, and the context-based variant shutting down cleanly when its timeout
fires. If the early-stop or context cases keep fetching after you have stopped
reading, the `select` on `done` or `ctx.Done()` is missing or unreachable, and
the goroutine is leaking.

## Resources
- [Go Concurrency Patterns (Rob Pike)](https://www.youtube.com/watch?v=f6kdp27TYZs) -- generators and multiplexing, the source of this pattern.
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- how to shut down generator goroutines so they never leak.
- [Go Blog: Context](https://go.dev/blog/context) -- using context.Context as the cancellation signal for on-demand producers.

---

Back to [Concurrency](../../concurrency.md) | Next: [07-or-channel-first-to-finish](../07-or-channel-first-to-finish/07-or-channel-first-to-finish.md)
