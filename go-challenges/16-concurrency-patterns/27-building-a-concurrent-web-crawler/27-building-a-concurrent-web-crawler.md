# 27. Building a Concurrent Web Crawler

A concurrent web crawler is the canonical example for combining worker pools, deduplication, dynamic work discovery, and clean shutdown. This lesson builds a real, testable crawler from first principles using only the standard library.

## Concepts

### The coordination problem

A sequential crawler is trivial: fetch a page, collect links, push them onto a stack, repeat. A concurrent crawler breaks that simplicity because multiple workers discover new URLs simultaneously. Worker A and worker B can both find the same URL at the same time, and without coordination, both will fetch it. Coordination must be cheap — a mutex around a visited map is enough — but the lock must be held for the entire check-and-mark operation, not split into two separate steps.

### Dynamic work and termination detection

Static work (a fixed slice of items to process) is easy: create a WaitGroup with the initial count, each worker calls Done, main waits. Dynamic work is harder because workers add items to the queue while other workers are still processing. The standard pattern:

1. Hold a WaitGroup that tracks *enqueued but not yet completed* items (not workers).
2. Increment before pushing to the queue.
3. Decrement when the item is fully processed — including after all child URLs have been enqueued.
4. A separate goroutine calls `wg.Wait()` and then closes the queue channel.
5. Workers range over the channel; they exit when the channel closes.

The ordering constraint is critical: you must `wg.Add(1)` and enqueue the URL *inside* the mutex that checks visited, before releasing the lock. If you release the lock first, another goroutine might observe zero in-flight items and close the queue before the new item lands.

### Queue capacity and backpressure

A blocking send to a full channel inside a worker goroutine creates a potential deadlock: all workers are blocked trying to enqueue, no workers are consuming. Two safe approaches:

- Make the queue buffer large enough that it never fills in practice (works for bounded graphs).
- Use a goroutine per send (`go func() { queue <- item }()`), which separates the WaitGroup increment from the actual channel send. This adds goroutines but eliminates deadlock.

For the implementation here, a buffer of 1024 is used. If you need to crawl graphs with thousands of nodes at depth 0, increase the buffer or switch to the goroutine-per-send pattern.

### The Fetcher interface

Production code makes HTTP calls. Tests do not want to make HTTP calls. Introducing a `Fetcher` interface with a single `Fetch(url string) ([]string, error)` method lets tests inject a deterministic in-memory graph. The implementation here provides `MapFetcher` — a `map[string][]string` that directly implements `Fetcher`. Real HTTP fetching is left to the caller, keeping the core package free of network dependencies.

### Race conditions to watch for

- Checking `visited[url]` and then setting it in two separate critical sections.
- Calling `wg.Done()` before enqueuing all child URLs (allows the waiting goroutine to close the queue too early).
- Reading `results` in the main goroutine before all workers have written to it.

All three are avoided by the implementation below.

## Exercises

### Setup

```bash
mkdir -p ~/go-exercises/crawler/internal/crawler ~/go-exercises/crawler/cmd/demo
cd ~/go-exercises/crawler
go mod init example.com/crawler
```

### Exercise 1: Core crawler package

Create `internal/crawler/crawler.go` with the implementation below. Study the ordering of `wg.Add`, the mutex scope, and the shutdown sequence.

```go
package crawler

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrMaxDepth is returned when the depth limit prevents enqueuing a URL.
var ErrMaxDepth = errors.New("max depth reached")

// ErrMaxPages is returned when the page limit prevents enqueuing a URL.
var ErrMaxPages = errors.New("max pages reached")

// ErrVisited is returned when a URL has already been seen.
var ErrVisited = errors.New("already visited")

// Fetcher abstracts the HTTP fetch operation for testability.
type Fetcher interface {
	Fetch(url string) (links []string, err error)
}

// MapFetcher is a Fetcher backed by a static map, useful for tests.
type MapFetcher map[string][]string

// Fetch implements Fetcher.
func (m MapFetcher) Fetch(url string) ([]string, error) {
	links, ok := m[url]
	if !ok {
		return nil, errors.New("not found: " + url)
	}
	return links, nil
}

// CrawlResult holds the result of crawling one URL.
type CrawlResult struct {
	URL   string
	Depth int
	Err   error
}

// Config controls crawler behavior.
type Config struct {
	MaxDepth int
	MaxPages int
	Workers  int
}

// Crawler is a concurrent web crawler.
type Crawler struct {
	cfg Config
}

// New creates a Crawler with the given Config.
// Zero values are replaced with safe defaults.
func New(cfg Config) *Crawler {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.MaxPages <= 0 {
		cfg.MaxPages = 100
	}
	if cfg.MaxDepth < 0 {
		cfg.MaxDepth = 5
	}
	return &Crawler{cfg: cfg}
}

type workItem struct {
	url   string
	depth int
}

// Crawl starts from seed and returns results for all crawled URLs.
// It blocks until crawling is complete.
func (c *Crawler) Crawl(seed string, f Fetcher) []CrawlResult {
	var (
		mu      sync.Mutex
		visited = make(map[string]bool)
		results []CrawlResult
		total   atomic.Int64
		wg      sync.WaitGroup
	)

	// Buffer is large enough to avoid deadlock for typical test graphs.
	queue := make(chan workItem, 1024)

	// resultCh collects results from workers without holding mu.
	resultCh := make(chan CrawlResult, 1024)

	// Collector goroutine drains resultCh into the results slice.
	var collDone sync.WaitGroup
	collDone.Add(1)
	go func() {
		defer collDone.Done()
		for r := range resultCh {
			results = append(results, r)
		}
	}()

	// enqueue attempts to add url at depth to the queue.
	// It must be called with mu NOT held; it acquires mu internally.
	enqueue := func(url string, depth int) {
		mu.Lock()
		if visited[url] || depth > c.cfg.MaxDepth || int(total.Load()) >= c.cfg.MaxPages {
			mu.Unlock()
			return
		}
		visited[url] = true
		total.Add(1)
		// wg.Add must happen before mu.Unlock so wg.Wait() cannot return
		// before this item is actually in the queue.
		wg.Add(1)
		mu.Unlock()
		queue <- workItem{url, depth}
	}

	// Start worker goroutines. They exit when queue is closed.
	var workerWg sync.WaitGroup
	for i := 0; i < c.cfg.Workers; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for item := range queue {
				links, err := f.Fetch(item.url)
				resultCh <- CrawlResult{URL: item.url, Depth: item.depth, Err: err}
				if err == nil {
					for _, link := range links {
						enqueue(link, item.depth+1)
					}
				}
				// Done AFTER all children are enqueued.
				wg.Done()
			}
		}()
	}

	// Seed the crawl.
	enqueue(seed, 0)

	// When all items are processed, close the queue so workers exit.
	wg.Wait()
	close(queue)

	// Wait for workers to drain and exit.
	workerWg.Wait()

	// No more writes to resultCh; close it so the collector exits.
	close(resultCh)
	collDone.Wait()

	return results
}
```

### Exercise 2: Tests

Write the test file at `internal/crawler/crawler_test.go`. Each test targets a specific invariant.

```go
package crawler_test

import (
	"fmt"
	"sort"
	"testing"

	"example.com/crawler/internal/crawler"
)

func TestCrawlAll(t *testing.T) {
	t.Parallel()
	f := crawler.MapFetcher{
		"http://example.com":   {"http://example.com/a", "http://example.com/b"},
		"http://example.com/a": {"http://example.com/c"},
		"http://example.com/b": nil,
		"http://example.com/c": nil,
	}
	c := crawler.New(crawler.Config{MaxDepth: 3, MaxPages: 10, Workers: 2})
	results := c.Crawl("http://example.com", f)
	if len(results) != 4 {
		t.Errorf("want 4 results, got %d", len(results))
	}
}

func TestMaxDepth(t *testing.T) {
	t.Parallel()
	f := crawler.MapFetcher{
		"http://example.com":   {"http://example.com/a"},
		"http://example.com/a": {"http://example.com/b"},
		"http://example.com/b": {"http://example.com/c"},
		"http://example.com/c": nil,
	}
	// MaxDepth: 1 means depth 0 (seed) and depth 1 (/a) are crawled; /b is depth 2 and skipped.
	c := crawler.New(crawler.Config{MaxDepth: 1, MaxPages: 10, Workers: 2})
	results := c.Crawl("http://example.com", f)
	if len(results) != 2 {
		t.Errorf("want 2 results (depth 0 and 1), got %d", len(results))
	}
}

func TestNoRevisit(t *testing.T) {
	t.Parallel()
	f := crawler.MapFetcher{
		"http://example.com":   {"http://example.com/a", "http://example.com/a"},
		"http://example.com/a": {"http://example.com"},
	}
	c := crawler.New(crawler.Config{MaxDepth: 5, MaxPages: 20, Workers: 2})
	results := c.Crawl("http://example.com", f)
	seen := make(map[string]int)
	for _, r := range results {
		seen[r.URL]++
	}
	for url, count := range seen {
		if count > 1 {
			t.Errorf("URL %s visited %d times, want 1", url, count)
		}
	}
}

func TestMaxPages(t *testing.T) {
	t.Parallel()
	f := crawler.MapFetcher{
		"http://example.com":   {"http://example.com/a", "http://example.com/b", "http://example.com/c"},
		"http://example.com/a": nil,
		"http://example.com/b": nil,
		"http://example.com/c": nil,
	}
	c := crawler.New(crawler.Config{MaxDepth: 5, MaxPages: 2, Workers: 2})
	results := c.Crawl("http://example.com", f)
	if len(results) > 2 {
		t.Errorf("want at most 2 results, got %d", len(results))
	}
}

func TestFetchError(t *testing.T) {
	t.Parallel()
	// /missing is not in the map, so Fetch returns an error.
	f := crawler.MapFetcher{
		"http://example.com": {"http://example.com/missing"},
	}
	c := crawler.New(crawler.Config{MaxDepth: 2, MaxPages: 10, Workers: 2})
	results := c.Crawl("http://example.com", f)
	// Both the seed and /missing should appear in results.
	if len(results) != 2 {
		t.Errorf("want 2 results, got %d", len(results))
	}
	var errCount int
	for _, r := range results {
		if r.Err != nil {
			errCount++
		}
	}
	if errCount != 1 {
		t.Errorf("want 1 error result, got %d", errCount)
	}
}

func ExampleCrawler() {
	f := crawler.MapFetcher{
		"http://example.com":   {"http://example.com/a"},
		"http://example.com/a": nil,
	}
	c := crawler.New(crawler.Config{MaxDepth: 2, MaxPages: 10, Workers: 2})
	results := c.Crawl("http://example.com", f)
	urls := make([]string, len(results))
	for i, r := range results {
		urls[i] = r.URL
	}
	sort.Strings(urls)
	for _, u := range urls {
		fmt.Println(u)
	}
	// Output:
	// http://example.com
	// http://example.com/a
}
```

### Exercise 3: Demo binary

Create `cmd/demo/main.go` to exercise the crawler with a static graph.

```go
package main

import (
	"fmt"
	"sort"

	"example.com/crawler/internal/crawler"
)

func main() {
	f := crawler.MapFetcher{
		"http://example.com":            {"http://example.com/about", "http://example.com/blog"},
		"http://example.com/about":      nil,
		"http://example.com/blog":       {"http://example.com/blog/post1"},
		"http://example.com/blog/post1": nil,
	}
	c := crawler.New(crawler.Config{MaxDepth: 3, MaxPages: 20, Workers: 4})
	results := c.Crawl("http://example.com", f)

	type line struct {
		depth int
		url   string
	}
	lines := make([]line, len(results))
	for i, r := range results {
		lines[i] = line{r.Depth, r.URL}
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].depth != lines[j].depth {
			return lines[i].depth < lines[j].depth
		}
		return lines[i].url < lines[j].url
	})
	fmt.Printf("crawled %d pages\n", len(results))
	for _, l := range lines {
		fmt.Printf("  depth=%d %s\n", l.depth, l.url)
	}
}
```

### Exercise 4: Extend the Fetcher interface

Extend the system by writing a `FetcherFunc` adapter so any `func(string) ([]string, error)` can be used as a `Fetcher`. Add a test that uses it with a closure capturing a call counter to verify the crawler does not call `Fetch` for already-visited URLs.

The adapter is one line:

```go
package crawler

// FetcherFunc is a function type that implements Fetcher.
type FetcherFunc func(url string) ([]string, error)

// Fetch implements Fetcher.
func (fn FetcherFunc) Fetch(url string) ([]string, error) {
	return fn(url)
}
```

A test using `FetcherFunc`:

```go
package crawler_test

import (
	"sync/atomic"
	"testing"

	"example.com/crawler/internal/crawler"
)

func TestFetcherFuncNotCalledForVisited(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"http://example.com":   {"http://example.com/a", "http://example.com/a"},
		"http://example.com/a": nil,
	}
	var calls atomic.Int64
	f := crawler.FetcherFunc(func(url string) ([]string, error) {
		calls.Add(1)
		return graph[url], nil
	})
	c := crawler.New(crawler.Config{MaxDepth: 3, MaxPages: 20, Workers: 2})
	c.Crawl("http://example.com", f)
	// Seed + /a = 2 unique URLs, so Fetch should be called exactly 2 times.
	if got := calls.Load(); got != 2 {
		t.Errorf("want 2 Fetch calls, got %d", got)
	}
}
```

## Common Mistakes

### Releasing the mutex before wg.Add

Wrong:

```go
mu.Lock()
visited[url] = true
mu.Unlock()
wg.Add(1)          // another goroutine may see zero in-flight here
queue <- workItem{url, depth}
```

What happens: between `mu.Unlock()` and `wg.Add(1)`, the goroutine watching `wg.Wait()` can observe zero and close the queue prematurely. Items already in the queue get processed, but the new item is never enqueued.

Fix: call `wg.Add(1)` while the mutex is held, before `mu.Unlock()`.

### Calling wg.Done before enqueuing children

Wrong:

```go
links, err := f.Fetch(item.url)
wg.Done()           // signals this item complete before children are enqueued
for _, link := range links {
    enqueue(link, item.depth+1)
}
```

What happens: `wg.Done` may bring the counter to zero while `enqueue` is still adding children. The closer goroutine sees zero and closes the queue before those children land. The crawl ends short.

Fix: enqueue all children first, then call `wg.Done()`.

### Forgetting to wait for workers after closing the queue

Wrong:

```go
wg.Wait()
close(queue)
close(resultCh)   // closed while workers may still be writing to resultCh
```

What happens: a worker reading from the closed queue exits its range loop, but it may still be in the middle of writing to `resultCh`. Closing `resultCh` while a worker writes to it panics.

Fix: after closing `queue`, call `workerWg.Wait()` to let all workers exit cleanly, then close `resultCh`.

### Queue deadlock with small buffer

Wrong: using `make(chan workItem)` (unbuffered) when workers enqueue children.

What happens: every worker is blocked sending to the channel while waiting for a receiver. All workers block. The watcher never sees zero. Deadlock.

Fix: use a large enough buffer (`make(chan workItem, 1024)`) or enqueue in a new goroutine so the send is non-blocking for the worker.

## Verification

After creating the files, run these commands from `~/go-exercises/crawler`:

```bash
gofmt -l ./...
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Expected output for `go test`:

```
ok  	example.com/crawler/internal/crawler	0.XXXs
```

No output from `gofmt -l` means all files are properly formatted. The `-race` flag verifies there are no data races in the concurrent crawler logic.

## Summary

- A concurrent crawler's hardest problem is termination detection: use a WaitGroup that tracks enqueued items, not workers.
- `wg.Add(1)` must happen inside the same critical section as the visited check, before releasing the lock.
- `wg.Done()` must happen after all child URLs are enqueued, not before.
- After closing the work queue, wait for all worker goroutines to exit before closing the result channel.
- A `Fetcher` interface decouples the crawler from HTTP, making it trivially testable with a `MapFetcher`.
- A buffer of 1024 on the queue channel avoids deadlock for bounded graphs; unbuffered channels deadlock.

## What's Next

Next: [Fan-Out with Priority Queues](../28-fan-out-with-priority-queues/28-fan-out-with-priority-queues.md).

## Resources

- [Go blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out/fan-in and context-based shutdown
- [The Go Tour: Web Crawler](https://go.dev/tour/concurrency/10) — the classic introductory version of this problem
- [sync package docs](https://pkg.go.dev/sync) — WaitGroup, Mutex, and their subtle ordering guarantees
- [Go memory model](https://go.dev/ref/mem) — why happens-before matters when combining atomics and mutexes
- [net/url package](https://pkg.go.dev/net/url) — URL parsing and resolution for a real HTTP fetcher
