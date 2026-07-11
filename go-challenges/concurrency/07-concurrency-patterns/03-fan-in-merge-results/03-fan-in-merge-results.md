# Exercise 3: Fan-In: Merge Results

Fan-in is the complement of fan-out: where fan-out spreads work across many
workers, fan-in collects results from many producers back into a single
channel. Together they are the classic scatter-gather pattern -- split the work,
process it in parallel, merge the answers. The payoff is concrete. Querying a
user database, a product catalog, and an order history one after another costs
the sum of their latencies, but firing all three concurrently and merging with
fan-in costs only the slowest one, which is how a single search endpoint drops
from hundreds of milliseconds to the latency of its slowest backend.

## What you'll build

```text
03-fan-in-merge-results/
  main.go        a search aggregator that queries several backends at once
                 and merges their results into one stream, then a ranking pipeline
```

- Build: a search aggregator that queries several backends concurrently and merges their results into a single stream.
- Implement: `mergeResults` with one forwarding goroutine per input channel and a WaitGroup-driven closer, a sequential-versus-fan-in latency comparison, and a fan-out/fan-in ranking pipeline.
- Verify: `go run main.go`

### Why fan-in turns sum into max

Consider a real scenario: a user types a search query in your application. To
return comprehensive results, you need to query the user database, the product
catalog, and the order history -- three separate backends. Querying them
sequentially takes 900ms (300ms each). By querying all three concurrently and
merging their results with fan-in, the total latency drops to 300ms -- the time
of the slowest backend. Your API response time just improved by 3x.

```
         Search Aggregator - Fan-In

  "laptop" --> userDB (300ms)   ---+
           --> productDB (200ms) --+--> merged results --> API response
           --> orderDB (250ms)  ---+

  Total latency: max(300, 200, 250) = 300ms instead of 750ms
```

## Step 1 -- Query Multiple Backends Concurrently

Start by defining the backend queries as functions that return channels, then merge two of them.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const backendCount = 3

// SearchResult holds the response from a single backend query.
type SearchResult struct {
	Backend string
	Items   []string
	Latency time.Duration
}

// BackendQuery defines a simulated backend with its latency and items.
type BackendQuery struct {
	Name    string
	Latency time.Duration
	Items   []string
}

// SearchAggregator queries multiple backends and merges results.
type SearchAggregator struct {
	backends []BackendQuery
}

func NewSearchAggregator(query string) *SearchAggregator {
	return &SearchAggregator{
		backends: []BackendQuery{
			{
				Name: "users", Latency: 120 * time.Millisecond,
				Items: []string{"user:alice (matches '" + query + "')", "user:bob (matches '" + query + "')"},
			},
			{
				Name: "products", Latency: 80 * time.Millisecond,
				Items: []string{"product:Laptop Pro", "product:Laptop Air", "product:Laptop Stand"},
			},
			{
				Name: "orders", Latency: 150 * time.Millisecond,
				Items: []string{"order:#1042 Laptop Pro", "order:#1099 Laptop Air"},
			},
		},
	}
}

func (sa *SearchAggregator) queryBackend(bq BackendQuery) <-chan SearchResult {
	out := make(chan SearchResult)
	go func() {
		defer close(out)
		start := time.Now()
		time.Sleep(bq.Latency)
		out <- SearchResult{
			Backend: bq.Name,
			Items:   bq.Items,
			Latency: time.Since(start),
		}
	}()
	return out
}

func mergeResults(channels ...<-chan SearchResult) <-chan SearchResult {
	out := make(chan SearchResult)
	var wg sync.WaitGroup

	for _, ch := range channels {
		wg.Add(1)
		go func(c <-chan SearchResult) {
			defer wg.Done()
			for v := range c {
				out <- v
			}
		}(ch)
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

func (sa *SearchAggregator) Search(query string) {
	fmt.Printf("=== Search Aggregator for '%s' ===\n\n", query)
	start := time.Now()

	channels := make([]<-chan SearchResult, len(sa.backends))
	for i, bq := range sa.backends {
		channels[i] = sa.queryBackend(bq)
	}

	merged := mergeResults(channels...)

	var totalItems int
	for r := range merged {
		fmt.Printf("  [%s] %d results (latency: %v)\n", r.Backend, len(r.Items), r.Latency)
		for _, item := range r.Items {
			fmt.Printf("    - %s\n", item)
		}
		totalItems += len(r.Items)
	}

	fmt.Printf("\n  Total: %d items from %d backends in %v\n", totalItems, backendCount, time.Since(start))
}

func main() {
	query := "laptop"
	aggregator := NewSearchAggregator(query)
	aggregator.Search(query)
}
```

Each backend gets its own forwarding goroutine in the `merge` function. A separate goroutine waits for all to finish and closes the output.

### Verification
```bash
go run main.go
```
Expected: results from all three backends, total time around 150ms (the slowest backend):
```
=== Search Aggregator for 'laptop' ===

  [products] 3 results (latency: 80ms)
    - product:Laptop Pro
    - product:Laptop Air
    - product:Laptop Stand
  [users] 2 results (latency: 120ms)
    - user:alice (matches 'laptop')
    - user:bob (matches 'laptop')
  [orders] 2 results (latency: 150ms)
    - order:#1042 Laptop Pro
    - order:#1099 Laptop Air

  Total: 7 items from 3 backends in 152ms
```

## Step 2 -- Compare Sequential vs Fan-In

Show the real cost of NOT using fan-in by implementing both approaches and measuring the difference.

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

// SearchResult holds the response from a single backend query.
type SearchResult struct {
	Backend string
	Count   int
	Latency time.Duration
}

// BackendSpec defines a simulated backend with its expected latency and result count.
type BackendSpec struct {
	Name    string
	Latency time.Duration
	Count   int
}

// SearchAggregator compares sequential vs fan-in query strategies.
type SearchAggregator struct {
	backends []BackendSpec
}

func NewSearchAggregator() *SearchAggregator {
	return &SearchAggregator{
		backends: []BackendSpec{
			{"users", 120 * time.Millisecond, 15},
			{"products", 80 * time.Millisecond, 42},
			{"orders", 150 * time.Millisecond, 8},
			{"inventory", 100 * time.Millisecond, 23},
			{"reviews", 200 * time.Millisecond, 31},
		},
	}
}

func (sa *SearchAggregator) queryBackend(spec BackendSpec) <-chan SearchResult {
	out := make(chan SearchResult)
	go func() {
		defer close(out)
		start := time.Now()
		time.Sleep(spec.Latency)
		out <- SearchResult{Backend: spec.Name, Count: spec.Count, Latency: time.Since(start)}
	}()
	return out
}

func mergeResults(channels ...<-chan SearchResult) <-chan SearchResult {
	out := make(chan SearchResult)
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(c <-chan SearchResult) {
			defer wg.Done()
			for v := range c {
				out <- v
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func (sa *SearchAggregator) RunSequential() {
	fmt.Println("=== Sequential Queries ===")
	start := time.Now()
	var total int
	for _, b := range sa.backends {
		time.Sleep(b.Latency)
		total += b.Count
		fmt.Printf("  [%s] %d results (%v)\n", b.Name, b.Count, b.Latency)
	}
	fmt.Printf("  Total: %d results in %v\n\n", total, time.Since(start))
}

func (sa *SearchAggregator) RunFanIn() {
	fmt.Println("=== Fan-In Queries ===")
	start := time.Now()

	channels := make([]<-chan SearchResult, len(sa.backends))
	for i, b := range sa.backends {
		channels[i] = sa.queryBackend(b)
	}
	merged := mergeResults(channels...)

	var total int
	for r := range merged {
		total += r.Count
		fmt.Printf("  [%s] %d results (%v)\n", r.Backend, r.Count, r.Latency)
	}
	fmt.Printf("  Total: %d results in %v\n\n", total, time.Since(start))
}

func main() {
	aggregator := NewSearchAggregator()

	aggregator.RunSequential()
	aggregator.RunFanIn()

	fmt.Println("Fan-in latency = max(all backend latencies) instead of sum(all)")
}
```

### Verification
```bash
go run main.go
```
Expected: sequential takes ~650ms (sum), fan-in takes ~200ms (max):
```
=== Sequential Queries ===
  [users] 15 results (120ms)
  [products] 42 results (80ms)
  [orders] 8 results (150ms)
  [inventory] 23 results (100ms)
  [reviews] 31 results (200ms)
  Total: 119 results in 652ms

=== Fan-In Queries ===
  [products] 42 results (80ms)
  [inventory] 23 results (100ms)
  [users] 15 results (120ms)
  [orders] 8 results (150ms)
  [reviews] 31 results (200ms)
  Total: 119 results in 201ms

Fan-in latency = max(all backend latencies) instead of sum(all)
```

## Step 3 -- Fan-Out Workers + Fan-In Results

Combine fan-out and fan-in into a complete parallel processing pipeline. Multiple workers process search results and their outputs are merged into a single stream.

```go
package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	rankingWorkerCount = 3
	laptopBonus        = 5.0
	lengthFactor       = 0.1
)

// RawResult is an unranked item from a backend.
type RawResult struct {
	Backend string
	Item    string
}

// RankedResult is an item scored by a ranking worker.
type RankedResult struct {
	Item     string
	Score    float64
	WorkerID int
}

// SearchRankingPipeline fans out raw results to ranking workers, then merges.
type SearchRankingPipeline struct {
	items      []RawResult
	numWorkers int
}

func NewSearchRankingPipeline(numWorkers int) *SearchRankingPipeline {
	return &SearchRankingPipeline{
		numWorkers: numWorkers,
		items: []RawResult{
			{"users", "alice@company.com"},
			{"users", "bob@company.com"},
			{"products", "Laptop Pro 16"},
			{"products", "Laptop Air 13"},
			{"products", "USB-C Adapter"},
			{"orders", "Order #1042"},
			{"orders", "Order #1099"},
			{"products", "Laptop Stand"},
			{"users", "charlie@company.com"},
			{"orders", "Order #1150"},
		},
	}
}

func (srp *SearchRankingPipeline) generateResults() <-chan RawResult {
	out := make(chan RawResult)
	go func() {
		defer close(out)
		for _, item := range srp.items {
			out <- item
		}
	}()
	return out
}

func computeScore(item string) float64 {
	score := float64(len(item)) * lengthFactor
	if strings.Contains(strings.ToLower(item), "laptop") {
		score += laptopBonus
	}
	return score
}

func rankWorker(id int, in <-chan RawResult) <-chan RankedResult {
	out := make(chan RankedResult)
	go func() {
		defer close(out)
		for raw := range in {
			time.Sleep(30 * time.Millisecond)
			out <- RankedResult{
				Item:     fmt.Sprintf("[%s] %s", raw.Backend, raw.Item),
				Score:    computeScore(raw.Item),
				WorkerID: id,
			}
		}
	}()
	return out
}

func mergeRanked(channels ...<-chan RankedResult) <-chan RankedResult {
	out := make(chan RankedResult)
	var wg sync.WaitGroup
	for _, ch := range channels {
		wg.Add(1)
		go func(c <-chan RankedResult) {
			defer wg.Done()
			for v := range c {
				out <- v
			}
		}(ch)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}

func (srp *SearchRankingPipeline) Run() {
	fmt.Println("=== Fan-Out/Fan-In: Search Ranking Pipeline ===")
	fmt.Println()

	start := time.Now()
	input := srp.generateResults()

	workers := make([]<-chan RankedResult, srp.numWorkers)
	for i := 0; i < srp.numWorkers; i++ {
		workers[i] = rankWorker(i+1, input)
	}

	merged := mergeRanked(workers...)

	fmt.Println("  Ranked results:")
	var count int
	for r := range merged {
		count++
		fmt.Printf("    %.1f  %s  (worker %d)\n", r.Score, r.Item, r.WorkerID)
	}
	fmt.Printf("\n  %d results ranked by %d workers in %v\n", count, srp.numWorkers, time.Since(start))
}

func main() {
	pipeline := NewSearchRankingPipeline(rankingWorkerCount)
	pipeline.Run()
}
```

### Verification
```bash
go run main.go
```
Expected: all 10 items ranked, distributed across 3 workers:
```
=== Fan-Out/Fan-In: Search Ranking Pipeline ===

  Ranked results:
    1.7  [users] alice@company.com  (worker 1)
    6.6  [products] Laptop Pro 16  (worker 2)
    ...

  10 results ranked by 3 workers in 130ms
```

## Common Mistakes

### Closing Output Channel Inside the Forwarding Goroutine
**Wrong:**
```go
go func(c <-chan SearchResult) {
	for v := range c {
		out <- v
	}
	close(out) // other goroutines still sending!
}(ch)
```
**What happens:** The first goroutine to finish closes the channel, causing other goroutines to panic on send.

**Fix:** Close the output channel only once, after ALL forwarding goroutines complete. Use a WaitGroup and a dedicated closer goroutine.

### Capturing the Loop Variable (Go < 1.22)
**Pre-Go 1.22 hazard:**
```go
for _, ch := range channels {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := range ch { // pre-1.22: captured the shared loop variable
			out <- v
		}
	}()
}
```
**What happened:** Before Go 1.22, `ch` was a single variable shared across all iterations, so the forwarding goroutines could all end up ranging over the same (last) channel. As of Go 1.22 each iteration gets a fresh `ch`, so this specific pattern is now safe.

**Still good practice:** Passing `ch` as a function argument -- `go func(c <-chan SearchResult) { ... }(ch)` -- documents exactly which channel each goroutine forwards and works identically on every Go version.

### Not Buffering the Output Channel When Needed
If all producers send simultaneously and the consumer is slow, an unbuffered output channel creates contention. Consider buffering if throughput matters, but remember that unbuffered channels provide natural backpressure.

## Review

Fan-in merges N channels into one by giving each input its own forwarding
goroutine that copies values onto a shared output channel. The subtlety is
entirely in the close: because several goroutines send on that one output, no
single forwarder may close it -- the first to finish would panic the rest on a
send to a closed channel. The correct shape is a `sync.WaitGroup` that every
forwarder decrements as it drains its input, plus one dedicated goroutine that
waits on the group and closes the output exactly once, after all inputs are
spent. Composed with fan-out, this is the scatter-gather pattern, and its
practical value is the latency arithmetic: the merged stream is ready in the
time of the slowest producer, not the sum of all of them. Passing each channel
into its forwarding goroutine as an argument keeps the intent explicit, and
before Go 1.22 it was also required to avoid capturing the shared loop variable.

Run the program and confirm three things line up with that model. The search
aggregator should merge results from all three backends into one stream. The
sequential-versus-fan-in comparison should show fan-in finishing in the time of
the slowest backend rather than the sum. And the fan-out/fan-in pipeline should
rank every item and merge the workers' outputs without dropping or duplicating
any -- proof that the single-closer discipline held.

## Resources
- [Go Blog: Pipelines and Cancellation](https://go.dev/blog/pipelines) -- the canonical fan-in/fan-out construction and how ownership of closing flows.
- [Go Concurrency Patterns (Rob Pike)](https://www.youtube.com/watch?v=f6kdp27TYZs) -- the talk that introduces scatter-gather and channel composition.
- [Effective Go: Channels of Channels](https://go.dev/doc/effective_go#chan_of_chan) -- how channels compose into larger concurrent structures.

---

Back to [Concurrency](../../concurrency.md) | Next: [04-worker-pool-fixed](../04-worker-pool-fixed/04-worker-pool-fixed.md)
