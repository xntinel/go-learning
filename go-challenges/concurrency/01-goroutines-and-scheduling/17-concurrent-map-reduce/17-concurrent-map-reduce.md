# Exercise 17: Concurrent Map-Reduce

Map-reduce splits a large dataset into chunks, processes each chunk
independently, then combines the partial results. In a single-machine Go program
goroutines are the natural vehicle for the map phase: each chunk runs in its own
goroutine with zero shared state. This exercise builds that pipeline for an
e-commerce analytics job -- per-category revenue over 10,000 transactions -- and
then benchmarks it, because the subtle part is not writing the goroutines but
knowing how many to launch. Too few underuses your cores; too many drowns the
computation in goroutine and channel overhead, and the concurrent version loses
to the sequential one it was meant to beat.

## What you'll build

```text
17-concurrent-map-reduce/
  main.go        sequential baseline, concurrent map phase, a MapReducer with
                 Map/Reduce/Process, and a chunk-size benchmark sweep
```

- Build: a concurrent map-reduce revenue aggregator with a sequential baseline to check it against.
- Implement: `sequentialAggregate`, `partition`, `mapChunk`/`concurrentMap`, a `MapReducer` with `Map`/`Reduce`/`Process`, and a chunk-size benchmark.
- Verify: `go run main.go` on each step.

### Why the map phase is embarrassingly parallel

The real-world scenario is an e-commerce analytics pipeline: 10,000 transactions
for the day, and you need per-category revenue totals. Sequentially scanning them
works, but when the per-transaction computation grows -- discount rules, tax
calculations, currency conversion -- the sequential scan becomes the bottleneck.
Partitioning the work across goroutines lets you saturate every available CPU
core.

The map phase is embarrassingly parallel: each goroutine works on its own slice
of data and produces its own partial result, so there are no locks and no shared
memory -- just channels carrying partial results from the mappers to a single
reducer. The reduce phase merges those partials into a final summary.


## Step 1 -- Sequential Revenue Aggregation

Start with a clean sequential implementation. This establishes the baseline behavior and the data structures that the concurrent version will reuse.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	transactionCount = 10000
	categoryCount    = 8
)

var categories = []string{
	"Electronics", "Clothing", "Books", "Home & Garden",
	"Sports", "Toys", "Grocery", "Automotive",
}

type Transaction struct {
	ID       int
	Category string
	Amount   float64
}

type RevenueReport struct {
	CategoryTotals map[string]float64
	TotalRevenue   float64
	TxCount        int
}

func generateTransactions(count int) []Transaction {
	txs := make([]Transaction, count)
	for i := range txs {
		txs[i] = Transaction{
			ID:       i + 1,
			Category: categories[rand.IntN(categoryCount)],
			Amount:   float64(rand.IntN(9901)+100) / 100.0,
		}
	}
	return txs
}

func sequentialAggregate(txs []Transaction) RevenueReport {
	totals := make(map[string]float64)
	for _, tx := range txs {
		totals[tx.Category] += tx.Amount
	}

	var total float64
	for _, v := range totals {
		total += v
	}

	return RevenueReport{
		CategoryTotals: totals,
		TotalRevenue:   total,
		TxCount:        len(txs),
	}
}

func printReport(label string, report RevenueReport, elapsed time.Duration) {
	fmt.Printf("=== %s ===\n", label)
	fmt.Printf("  Transactions processed: %d\n", report.TxCount)
	for _, cat := range categories {
		if total, ok := report.CategoryTotals[cat]; ok {
			fmt.Printf("  %-15s $%10.2f\n", cat, total)
		}
	}
	fmt.Printf("  %-15s $%10.2f\n", "TOTAL", report.TotalRevenue)
	fmt.Printf("  Duration: %v\n\n", elapsed.Round(time.Microsecond))
}

func main() {
	transactions := generateTransactions(transactionCount)

	start := time.Now()
	report := sequentialAggregate(transactions)
	elapsed := time.Since(start)

	printReport("Sequential Aggregation", report, elapsed)
}
```

**What's happening here:** We generate 10,000 transactions with random categories and amounts between $1.00 and $99.99. The sequential aggregator walks every transaction once, accumulating per-category totals in a map. This is the behavior we need to replicate -- and beat -- with concurrency.

**Key insight:** The `RevenueReport` struct cleanly separates the computation result from its presentation. This separation is what makes it possible to produce reports from either sequential or concurrent code paths.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Sequential Aggregation ===
  Transactions processed: 10000
  Electronics     $  63284.17
  Clothing        $  62018.53
  Books           $  62550.91
  Home & Garden   $  63729.48
  Sports          $  61499.22
  Toys            $  62793.04
  Grocery         $  63105.88
  Automotive      $  62241.55
  TOTAL           $ 501222.78
  Duration: 312us
```


## Step 2 -- Concurrent Map Phase with Goroutines

Partition the transactions into chunks and launch one goroutine per chunk. Each goroutine computes per-category totals for its chunk and sends the partial result through a buffered channel.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	transactionCount = 10000
	categoryCount    = 8
	chunkSize        = 2500
)

var categories = []string{
	"Electronics", "Clothing", "Books", "Home & Garden",
	"Sports", "Toys", "Grocery", "Automotive",
}

type Transaction struct {
	ID       int
	Category string
	Amount   float64
}

type PartialResult struct {
	ChunkID        int
	CategoryTotals map[string]float64
	TxCount        int
}

type RevenueReport struct {
	CategoryTotals map[string]float64
	TotalRevenue   float64
	TxCount        int
}

func generateTransactions(count int) []Transaction {
	txs := make([]Transaction, count)
	for i := range txs {
		txs[i] = Transaction{
			ID:       i + 1,
			Category: categories[rand.IntN(categoryCount)],
			Amount:   float64(rand.IntN(9901)+100) / 100.0,
		}
	}
	return txs
}

func partition(txs []Transaction, size int) [][]Transaction {
	var chunks [][]Transaction
	for i := 0; i < len(txs); i += size {
		end := i + size
		if end > len(txs) {
			end = len(txs)
		}
		chunks = append(chunks, txs[i:end])
	}
	return chunks
}

func mapChunk(chunkID int, chunk []Transaction) PartialResult {
	totals := make(map[string]float64)
	for _, tx := range chunk {
		totals[tx.Category] += tx.Amount
	}
	return PartialResult{
		ChunkID:        chunkID,
		CategoryTotals: totals,
		TxCount:        len(chunk),
	}
}

func concurrentMap(chunks [][]Transaction) []PartialResult {
	results := make(chan PartialResult, len(chunks))

	for i, chunk := range chunks {
		go func(id int, data []Transaction) {
			results <- mapChunk(id, data)
		}(i, chunk)
	}

	partials := make([]PartialResult, 0, len(chunks))
	for i := 0; i < len(chunks); i++ {
		partials = append(partials, <-results)
	}
	return partials
}

func printPartials(partials []PartialResult) {
	fmt.Println("--- Partial Results from Map Phase ---")
	for _, p := range partials {
		fmt.Printf("  Chunk %d: %d txs, %d categories\n",
			p.ChunkID, p.TxCount, len(p.CategoryTotals))
	}
	fmt.Println()
}

func main() {
	transactions := generateTransactions(transactionCount)
	chunks := partition(transactions, chunkSize)

	fmt.Printf("Partitioned %d transactions into %d chunks of ~%d each\n\n",
		len(transactions), len(chunks), chunkSize)

	start := time.Now()
	partials := concurrentMap(chunks)
	elapsed := time.Since(start)

	printPartials(partials)
	fmt.Printf("Map phase completed in %v\n", elapsed.Round(time.Microsecond))
}
```

**What's happening here:** The `partition` function splits the transaction slice into sub-slices without copying data -- each chunk is a view into the original slice. `concurrentMap` launches one goroutine per chunk, each computing its own partial totals independently. The buffered channel holds all results without blocking any goroutine.

**Key insight:** Each goroutine works on a disjoint slice of the data. There is zero shared mutable state, so no locks or synchronization beyond the channel itself. This is the ideal concurrent workload.

### Verification
```bash
go run main.go
```
Expected output:
```
Partitioned 10000 transactions into 4 chunks of ~2500 each

--- Partial Results from Map Phase ---
  Chunk 2: 2500 txs, 8 categories
  Chunk 0: 2500 txs, 8 categories
  Chunk 3: 2500 txs, 8 categories
  Chunk 1: 2500 txs, 8 categories

Map phase completed in 187us
```


## Step 3 -- Reduce Phase and Complete Pipeline

Add the reduce phase that merges all partial results into a final `RevenueReport`. Wire map and reduce together into a complete `MapReducer`.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	transactionCount = 10000
	categoryCount    = 8
	defaultChunkSize = 2500
)

var categories = []string{
	"Electronics", "Clothing", "Books", "Home & Garden",
	"Sports", "Toys", "Grocery", "Automotive",
}

type Transaction struct {
	ID       int
	Category string
	Amount   float64
}

type PartialResult struct {
	ChunkID        int
	CategoryTotals map[string]float64
	TxCount        int
}

type RevenueReport struct {
	CategoryTotals map[string]float64
	TotalRevenue   float64
	TxCount        int
	ChunksUsed     int
}

type MapReducer struct {
	ChunkSize int
}

func NewMapReducer(chunkSize int) *MapReducer {
	return &MapReducer{ChunkSize: chunkSize}
}

func generateTransactions(count int) []Transaction {
	txs := make([]Transaction, count)
	for i := range txs {
		txs[i] = Transaction{
			ID:       i + 1,
			Category: categories[rand.IntN(categoryCount)],
			Amount:   float64(rand.IntN(9901)+100) / 100.0,
		}
	}
	return txs
}

func partition(txs []Transaction, size int) [][]Transaction {
	var chunks [][]Transaction
	for i := 0; i < len(txs); i += size {
		end := i + size
		if end > len(txs) {
			end = len(txs)
		}
		chunks = append(chunks, txs[i:end])
	}
	return chunks
}

func (mr *MapReducer) Map(chunks [][]Transaction) []PartialResult {
	results := make(chan PartialResult, len(chunks))

	for i, chunk := range chunks {
		go func(id int, data []Transaction) {
			totals := make(map[string]float64)
			for _, tx := range data {
				totals[tx.Category] += tx.Amount
			}
			results <- PartialResult{
				ChunkID:        id,
				CategoryTotals: totals,
				TxCount:        len(data),
			}
		}(i, chunk)
	}

	partials := make([]PartialResult, 0, len(chunks))
	for i := 0; i < len(chunks); i++ {
		partials = append(partials, <-results)
	}
	return partials
}

func (mr *MapReducer) Reduce(partials []PartialResult) RevenueReport {
	merged := make(map[string]float64)
	totalTx := 0

	for _, p := range partials {
		totalTx += p.TxCount
		for cat, amount := range p.CategoryTotals {
			merged[cat] += amount
		}
	}

	var totalRevenue float64
	for _, v := range merged {
		totalRevenue += v
	}

	return RevenueReport{
		CategoryTotals: merged,
		TotalRevenue:   totalRevenue,
		TxCount:        totalTx,
		ChunksUsed:     len(partials),
	}
}

func (mr *MapReducer) Process(txs []Transaction) RevenueReport {
	chunks := partition(txs, mr.ChunkSize)
	partials := mr.Map(chunks)
	return mr.Reduce(partials)
}

func sequentialAggregate(txs []Transaction) RevenueReport {
	totals := make(map[string]float64)
	for _, tx := range txs {
		totals[tx.Category] += tx.Amount
	}
	var total float64
	for _, v := range totals {
		total += v
	}
	return RevenueReport{
		CategoryTotals: totals,
		TotalRevenue:   total,
		TxCount:        len(txs),
		ChunksUsed:     1,
	}
}

func printReport(label string, report RevenueReport, elapsed time.Duration) {
	fmt.Printf("=== %s ===\n", label)
	fmt.Printf("  Transactions: %d | Chunks: %d\n", report.TxCount, report.ChunksUsed)
	for _, cat := range categories {
		if total, ok := report.CategoryTotals[cat]; ok {
			fmt.Printf("  %-15s $%10.2f\n", cat, total)
		}
	}
	fmt.Printf("  %-15s $%10.2f\n", "TOTAL", report.TotalRevenue)
	fmt.Printf("  Duration: %v\n\n", elapsed.Round(time.Microsecond))
}

func main() {
	transactions := generateTransactions(transactionCount)

	start := time.Now()
	seqReport := sequentialAggregate(transactions)
	seqElapsed := time.Since(start)

	mr := NewMapReducer(defaultChunkSize)

	start = time.Now()
	mrReport := mr.Process(transactions)
	mrElapsed := time.Since(start)

	printReport("Sequential", seqReport, seqElapsed)
	printReport("MapReduce (4 chunks)", mrReport, mrElapsed)

	fmt.Println("--- Verification ---")
	fmt.Printf("  Revenue match: %v\n",
		fmt.Sprintf("%.2f", seqReport.TotalRevenue) == fmt.Sprintf("%.2f", mrReport.TotalRevenue))
}
```

**What's happening here:** The `MapReducer` struct encapsulates the entire pipeline. `Map` fans out goroutines and collects partial results. `Reduce` merges all partials into a single `RevenueReport`. `Process` wires the two phases together. The sequential version serves as a correctness check -- both must produce identical totals.

**Key insight:** The reduce phase is intentionally sequential. Merging 4 partial maps is trivially fast compared to the map phase. Trying to parallelize the reduce for such a small number of partials would add complexity for no measurable gain. Know when concurrency helps and when it just adds overhead.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Sequential ===
  Transactions: 10000 | Chunks: 1
  Electronics     $  63284.17
  Clothing        $  62018.53
  Books           $  62550.91
  Home & Garden   $  63729.48
  Sports          $  61499.22
  Toys            $  62793.04
  Grocery         $  63105.88
  Automotive      $  62241.55
  TOTAL           $ 501222.78
  Duration: 298us

=== MapReduce (4 chunks) ===
  Transactions: 10000 | Chunks: 4
  Electronics     $  63284.17
  Clothing        $  62018.53
  Books           $  62550.91
  Home & Garden   $  63729.48
  Sports          $  61499.22
  Toys            $  62793.04
  Grocery         $  63105.88
  Automotive      $  62241.55
  TOTAL           $ 501222.78
  Duration: 215us

--- Verification ---
  Revenue match: true
```


## Step 4 -- Benchmark Different Chunk Sizes

Benchmark the MapReducer with various chunk sizes to find the sweet spot. Too few chunks underutilizes cores; too many chunks drown in goroutine overhead.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"time"
)

const (
	transactionCount = 10000
	categoryCount    = 8
	benchmarkRuns    = 50
)

var categories = []string{
	"Electronics", "Clothing", "Books", "Home & Garden",
	"Sports", "Toys", "Grocery", "Automotive",
}

type Transaction struct {
	ID       int
	Category string
	Amount   float64
}

type PartialResult struct {
	ChunkID        int
	CategoryTotals map[string]float64
	TxCount        int
}

type RevenueReport struct {
	CategoryTotals map[string]float64
	TotalRevenue   float64
	TxCount        int
	ChunksUsed     int
}

type MapReducer struct {
	ChunkSize int
}

func NewMapReducer(chunkSize int) *MapReducer {
	return &MapReducer{ChunkSize: chunkSize}
}

func generateTransactions(count int) []Transaction {
	txs := make([]Transaction, count)
	for i := range txs {
		txs[i] = Transaction{
			ID:       i + 1,
			Category: categories[rand.IntN(categoryCount)],
			Amount:   float64(rand.IntN(9901)+100) / 100.0,
		}
	}
	return txs
}

func partition(txs []Transaction, size int) [][]Transaction {
	var chunks [][]Transaction
	for i := 0; i < len(txs); i += size {
		end := i + size
		if end > len(txs) {
			end = len(txs)
		}
		chunks = append(chunks, txs[i:end])
	}
	return chunks
}

func (mr *MapReducer) Map(chunks [][]Transaction) []PartialResult {
	results := make(chan PartialResult, len(chunks))

	for i, chunk := range chunks {
		go func(id int, data []Transaction) {
			totals := make(map[string]float64)
			for _, tx := range data {
				totals[tx.Category] += tx.Amount
			}
			results <- PartialResult{
				ChunkID:        id,
				CategoryTotals: totals,
				TxCount:        len(data),
			}
		}(i, chunk)
	}

	partials := make([]PartialResult, 0, len(chunks))
	for i := 0; i < len(chunks); i++ {
		partials = append(partials, <-results)
	}
	return partials
}

func (mr *MapReducer) Reduce(partials []PartialResult) RevenueReport {
	merged := make(map[string]float64)
	totalTx := 0
	for _, p := range partials {
		totalTx += p.TxCount
		for cat, amount := range p.CategoryTotals {
			merged[cat] += amount
		}
	}
	var totalRevenue float64
	for _, v := range merged {
		totalRevenue += v
	}
	return RevenueReport{
		CategoryTotals: merged,
		TotalRevenue:   totalRevenue,
		TxCount:        totalTx,
		ChunksUsed:     len(partials),
	}
}

func (mr *MapReducer) Process(txs []Transaction) RevenueReport {
	chunks := partition(txs, mr.ChunkSize)
	partials := mr.Map(chunks)
	return mr.Reduce(partials)
}

func sequentialAggregate(txs []Transaction) RevenueReport {
	totals := make(map[string]float64)
	for _, tx := range txs {
		totals[tx.Category] += tx.Amount
	}
	var total float64
	for _, v := range totals {
		total += v
	}
	return RevenueReport{
		CategoryTotals: totals,
		TotalRevenue:   total,
		TxCount:        len(txs),
		ChunksUsed:     1,
	}
}

type BenchmarkResult struct {
	Label    string
	Chunks   int
	AvgTime  time.Duration
	Speedup  float64
}

func benchmark(label string, fn func() RevenueReport, runs int) time.Duration {
	var total time.Duration
	for i := 0; i < runs; i++ {
		start := time.Now()
		fn()
		total += time.Since(start)
	}
	return total / time.Duration(runs)
}

func main() {
	transactions := generateTransactions(transactionCount)

	seqAvg := benchmark("Sequential", func() RevenueReport {
		return sequentialAggregate(transactions)
	}, benchmarkRuns)

	chunkSizes := []int{10000, 5000, 2500, 1000, 500, 100, 10}
	results := make([]BenchmarkResult, 0, len(chunkSizes)+1)

	results = append(results, BenchmarkResult{
		Label:   "Sequential",
		Chunks:  1,
		AvgTime: seqAvg,
		Speedup: 1.0,
	})

	for _, cs := range chunkSizes {
		mr := NewMapReducer(cs)
		chunks := partition(transactions, cs)
		numChunks := len(chunks)

		avg := benchmark(fmt.Sprintf("MR(chunk=%d)", cs), func() RevenueReport {
			return mr.Process(transactions)
		}, benchmarkRuns)

		results = append(results, BenchmarkResult{
			Label:   fmt.Sprintf("MR(chunk=%d)", cs),
			Chunks:  numChunks,
			AvgTime: avg,
			Speedup: float64(seqAvg) / float64(avg),
		})
	}

	fmt.Printf("=== Map-Reduce Benchmark (%d runs each, %d transactions) ===\n\n",
		benchmarkRuns, transactionCount)
	fmt.Printf("  %-20s %8s %10s %8s\n", "Strategy", "Chunks", "Avg Time", "Speedup")
	fmt.Printf("  %-20s %8s %10s %8s\n", "--------", "------", "--------", "-------")

	for _, r := range results {
		fmt.Printf("  %-20s %8d %10v %7.2fx\n",
			r.Label, r.Chunks, r.AvgTime.Round(time.Microsecond), r.Speedup)
	}

	fmt.Println()
	fmt.Println("--- Analysis ---")
	fmt.Println("  1 chunk  = pure sequential (no goroutine overhead)")
	fmt.Println("  2-4 chunks = sweet spot (matches typical core count)")
	fmt.Println("  1000 chunks = goroutine creation overhead dominates")
	fmt.Println("  Optimal chunk count roughly equals runtime.NumCPU()")
}
```

**What's happening here:** We run each strategy 50 times and average the results. The benchmark sweeps chunk sizes from 10,000 (single chunk, effectively sequential with channel overhead) down to 10 (1,000 goroutines). The speedup column reveals where concurrency helps and where goroutine creation overhead starts to hurt.

**Key insight:** For CPU-bound work on small data, the optimal number of goroutines roughly matches the number of CPU cores. With 10,000 transactions split into 4 chunks on a 4-core machine, each goroutine has enough work to amortize its creation cost. At 1,000 goroutines (10 transactions each), the time spent creating goroutines and channel operations exceeds the actual computation. This is the fundamental tradeoff of concurrent map-reduce: granularity vs overhead.

### Verification
```bash
go run main.go
```
Expected output (times vary by machine):
```
=== Map-Reduce Benchmark (50 runs each, 10000 transactions) ===

  Strategy             Chunks   Avg Time  Speedup
  --------             ------   --------  -------
  Sequential                1       52us    1.00x
  MR(chunk=10000)           1       58us    0.90x
  MR(chunk=5000)            2       41us    1.27x
  MR(chunk=2500)            4       35us    1.49x
  MR(chunk=1000)           10       42us    1.24x
  MR(chunk=500)            20       61us    0.85x
  MR(chunk=100)           100      178us    0.29x
  MR(chunk=10)           1000     1.34ms    0.04x

--- Analysis ---
  1 chunk  = pure sequential (no goroutine overhead)
  2-4 chunks = sweet spot (matches typical core count)
  1000 chunks = goroutine creation overhead dominates
  Optimal chunk count roughly equals runtime.NumCPU()
```


## Common Mistakes

### Sharing the Accumulator Map Across Goroutines

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"sync"
)

type Transaction struct {
	Category string
	Amount   float64
}

func main() {
	txs := []Transaction{
		{"Books", 10.0}, {"Books", 20.0},
		{"Toys", 15.0}, {"Toys", 25.0},
	}

	totals := make(map[string]float64) // shared map, no protection
	var wg sync.WaitGroup
	for _, tx := range txs {
		wg.Add(1)
		go func(t Transaction) {
			defer wg.Done()
			totals[t.Category] += t.Amount // DATA RACE: concurrent map write
		}(tx)
	}
	wg.Wait()
	fmt.Println(totals)
}
```
**What happens:** Concurrent writes to a shared map cause a data race. Go's runtime will likely panic with `fatal error: concurrent map writes`. Even if it doesn't panic, the results are corrupted silently.

**Correct -- each goroutine uses its own map, merge after:**
```go
package main

import (
	"fmt"
	"sync"
)

type Transaction struct {
	Category string
	Amount   float64
}

func main() {
	txs := []Transaction{
		{"Books", 10.0}, {"Books", 20.0},
		{"Toys", 15.0}, {"Toys", 25.0},
	}

	results := make(chan map[string]float64, 2)
	var wg sync.WaitGroup

	chunks := [][]Transaction{txs[:2], txs[2:]}
	for _, chunk := range chunks {
		wg.Add(1)
		go func(data []Transaction) {
			defer wg.Done()
			local := make(map[string]float64)
			for _, t := range data {
				local[t.Category] += t.Amount
			}
			results <- local
		}(chunk)
	}

	wg.Wait()
	close(results)

	merged := make(map[string]float64)
	for partial := range results {
		for k, v := range partial {
			merged[k] += v
		}
	}
	fmt.Println(merged) // map[Books:30 Toys:40]
}
```

### Forgetting to Collect All Results from the Channel

**Wrong -- complete program:**
```go
package main

import "fmt"

func main() {
	results := make(chan int, 4)
	for i := 0; i < 4; i++ {
		go func(n int) {
			results <- n * n
		}(i)
	}

	// Only reads 2 of 4 results -- 2 goroutines are abandoned
	fmt.Println(<-results)
	fmt.Println(<-results)
}
```
**What happens:** Two goroutines complete and two remain blocked on the channel send, leaking memory. In a long-running service, this accumulates over time.

**Correct -- collect exactly as many results as goroutines launched:**
```go
package main

import "fmt"

func main() {
	numWorkers := 4
	results := make(chan int, numWorkers)
	for i := 0; i < numWorkers; i++ {
		go func(n int) {
			results <- n * n
		}(i)
	}

	for i := 0; i < numWorkers; i++ {
		fmt.Println(<-results)
	}
}
```


## Review

The pattern rests on isolation. Partitioning a slice with `txs[i:end]` creates
views into the original backing array without copying, and each mapper goroutine
accumulates into its own local map -- never a shared one, which is the
difference between a correct result and a `concurrent map writes` panic. A
buffered channel sized to the number of chunks lets every mapper deposit its
partial without blocking, and the reduce phase, deliberately sequential, folds
those partials into one report. The whole thing is only worth doing when the
chunk count roughly matches the core count: below that you leave cores idle,
above it goroutine creation and scheduling overhead can make the concurrent
version slower than the sequential one you started from. That is why the
sequential baseline is not scaffolding to delete -- it is the correctness oracle
you compare every concurrent result against.

You should now be able to build the same shape for a different reduction without
re-reading the steps. Generate 50,000 random words drawn from a 20-word
vocabulary, partition them by a configurable chunk size, and map each chunk in
its own goroutine so each returns an independent `map[string]int` of counts
through a buffered channel. Reduce by summing those partial maps into one
frequency table, print the top five words, and assert the counts total exactly
50,000 -- the same correctness role `sequentialAggregate` plays here. Then
benchmark chunk sizes of 50000, 10000, 5000, 1000, and 100 and watch the speedup
peak near your core count and collapse as the chunks get tiny.


## Resources
- [Go Blog: Concurrency is not Parallelism](https://go.dev/blog/waza-talk) -- Rob Pike on why structuring work concurrently is distinct from running it in parallel.
- [Effective Go: Parallelization](https://go.dev/doc/effective_go#parallel) -- the canonical fan-out-over-CPUs example this exercise generalizes.
- [Go Spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the semantics of the buffered channels that carry partial results.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) -- the alternative to counting channel receives when you only need completion, not values.

---

Back to [Concurrency](../../concurrency.md) | Next: [18-connection-pool](../18-connection-pool/18-connection-pool.md)
