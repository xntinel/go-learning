# Exercise 5: GOMAXPROCS and Parallelism

Concurrency is about structure; parallelism is about execution. A program is
concurrent when it is built as multiple independently executing tasks; it is
parallel only when those tasks actually run at the same instant on different
CPU cores. The `go` keyword gives you the first for free -- but the second is
governed by `GOMAXPROCS`, and confusing the two is how a service ends up with a
thousand goroutines and no speedup, or a fleet of cores that a database-bound
endpoint never uses. This exercise makes the difference measurable.

## What you'll build

```text
05-gomaxprocs-and-parallelism/
  main.go        four benchmarks -- CPU-bound image filter, IO-bound DB queries,
                 and a mixed API handler -- each swept across GOMAXPROCS values
```

- Build: a set of benchmarks that measure wall-clock speedup as `GOMAXPROCS` changes, across CPU-bound, IO-bound, and mixed workloads.
- Implement: an image-filter simulation (`FilterAllConcurrently`, `applyConvolutionFilter`), a per-procs benchmark loop, an IO-bound database-query benchmark, and a mixed CPU-plus-IO API handler.
- Verify: `go run main.go` for each step and compare the speedup columns.

### Why the workload decides the speedup

With `GOMAXPROCS=1`, all goroutines share a single logical processor. They are
concurrent -- they can each make progress -- but not parallel: only one runs at
any given instant. Increasing GOMAXPROCS lets multiple goroutines execute truly
simultaneously on different cores.

The practical impact depends on the workload. CPU-bound work (computation,
hashing, sorting) benefits enormously from parallelism because more Ps mean more
work happening simultaneously. IO-bound work (network calls, disk reads,
database queries) benefits less because goroutines spend most of their time
waiting, not computing. Understanding this distinction is essential for tuning
real Go applications.

## Step 1 -- Image Filter Simulation: Concurrency vs Parallelism

Imagine a service that applies a CPU-intensive filter (e.g., blur, edge detection) to a batch of uploaded images. Each image is a large slice of data. With GOMAXPROCS=1, the filters run one at a time. With GOMAXPROCS=NumCPU, they run on separate cores simultaneously.

```go
package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

const (
	numImages      = 4
	pixelsPerImage = 2_000_000
	kernelWeight   = 0.333
)

type ImageBatch struct {
	Images [][]float64
}

func NewImageBatch(count, size int) *ImageBatch {
	images := make([][]float64, count)
	for i := range images {
		images[i] = make([]float64, size)
		for j := range images[i] {
			images[i][j] = float64(j%256) / 255.0
		}
	}
	return &ImageBatch{Images: images}
}

func applyConvolutionFilter(imageData []float64) float64 {
	var result float64
	for i := 1; i < len(imageData)-1; i++ {
		result += math.Sqrt(imageData[i-1]*imageData[i-1]+
			imageData[i]*imageData[i]+
			imageData[i+1]*imageData[i+1]) * kernelWeight
	}
	return result
}

func (batch *ImageBatch) FilterAllConcurrently() time.Duration {
	start := time.Now()
	var wg sync.WaitGroup

	for i := range batch.Images {
		wg.Add(1)
		go func(imgIdx int) {
			defer wg.Done()
			imgStart := time.Now()
			checksum := applyConvolutionFilter(batch.Images[imgIdx])
			fmt.Printf("  image %d filtered: %v (checksum: %.2f)\n",
				imgIdx, time.Since(imgStart).Round(time.Millisecond), checksum)
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func main() {
	batch := NewImageBatch(numImages, pixelsPerImage)

	for _, procs := range []int{1, runtime.NumCPU()} {
		runtime.GOMAXPROCS(procs)
		fmt.Printf("GOMAXPROCS=%d:\n", procs)

		wallClock := batch.FilterAllConcurrently()
		fmt.Printf("  Total wall-clock: %v\n\n", wallClock.Round(time.Millisecond))
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
}
```

**What's happening here:** Four workers each apply a mathematical filter to a 2M-element slice (simulating image processing). With GOMAXPROCS=1, they share one P, so they run sequentially: total time is ~4x one image. With GOMAXPROCS=NumCPU, all four run simultaneously on different cores: total time approaches 1x one image.

**Key insight:** The `go` keyword gives you concurrency (structure). GOMAXPROCS gives you parallelism (simultaneous execution). Without multiple Ps, goroutines take turns -- your image processing pipeline is no faster than sequential code.

**What would happen with GOMAXPROCS=2?** Two images would be processed simultaneously, then the other two. Total time would be ~2x one image instead of ~4x.

### Verification
```bash
go run main.go
```
Expected output pattern:
```
GOMAXPROCS=1:
  image 0 filtered: 85ms (checksum: 123456.78)
  image 1 filtered: 84ms (checksum: 123456.78)
  image 2 filtered: 85ms (checksum: 123456.78)
  image 3 filtered: 84ms (checksum: 123456.78)
  Total wall-clock: 340ms

GOMAXPROCS=8:
  image 1 filtered: 86ms (checksum: 123456.78)
  image 3 filtered: 87ms (checksum: 123456.78)
  image 0 filtered: 87ms (checksum: 123456.78)
  image 2 filtered: 88ms (checksum: 123456.78)
  Total wall-clock: 90ms
```

## Step 2 -- Image Processing Benchmark Across GOMAXPROCS Values

Measure the exact speedup you get at each GOMAXPROCS level for the image filter workload.

```go
package main

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	benchmarkImages    = 8
	benchmarkPixels    = 1_500_000
	convolutionWeight  = 0.333
)

type BenchmarkResult struct {
	Procs    int
	Elapsed  time.Duration
	Speedup  float64
}

func applyConvolutionFilter(data []float64) float64 {
	var result float64
	for i := 1; i < len(data)-1; i++ {
		result += math.Sqrt(data[i-1]*data[i-1]+
			data[i]*data[i]+
			data[i+1]*data[i+1]) * convolutionWeight
	}
	return result
}

func generateTestImages(count, size int) [][]float64 {
	images := make([][]float64, count)
	for i := range images {
		images[i] = make([]float64, size)
		for j := range images[i] {
			images[i][j] = float64(j%256) / 255.0
		}
	}
	return images
}

func benchmarkFilterAtProcs(images [][]float64, procs int) time.Duration {
	runtime.GOMAXPROCS(procs)
	start := time.Now()
	var wg sync.WaitGroup

	for i := range images {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			applyConvolutionFilter(images[idx])
		}(i)
	}

	wg.Wait()
	return time.Since(start)
}

func buildProcsRange() []int {
	procsRange := []int{1, 2, 4}
	if runtime.NumCPU() >= 8 {
		procsRange = append(procsRange, 8)
	}
	if runtime.NumCPU() >= 16 {
		procsRange = append(procsRange, 16)
	}
	return procsRange
}

func main() {
	images := generateTestImages(benchmarkImages, benchmarkPixels)
	applyConvolutionFilter(images[0]) // warm up CPU caches

	procsRange := buildProcsRange()

	fmt.Printf("Filtering %d images (%d pixels each):\n\n", benchmarkImages, benchmarkPixels)
	fmt.Printf("%-12s %-15s %-10s\n", "GOMAXPROCS", "Wall-Clock", "Speedup")
	fmt.Println(strings.Repeat("-", 40))

	var baselineTime time.Duration
	for _, procs := range procsRange {
		elapsed := benchmarkFilterAtProcs(images, procs)
		if procs == 1 {
			baselineTime = elapsed
		}
		speedup := float64(baselineTime) / float64(elapsed)
		fmt.Printf("%-12d %-15v %-10.2fx\n", procs, elapsed.Round(time.Millisecond), speedup)
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Println()
	fmt.Println("Speedup is roughly linear because image filtering is pure CPU work.")
	fmt.Println("Each core processes one image independently with no shared state.")
}
```

**What's happening here:** Eight images are filtered using different GOMAXPROCS values. Doubling Ps roughly halves the wall-clock time because the filter work distributes across more cores.

**Key insight:** Speedup is roughly linear for CPU-bound work until you hit the physical core count. Beyond that, adding Ps provides no benefit because there are no more cores to use. This is why your image processing service should set worker count to match available CPUs, not an arbitrary large number.

### Verification
```bash
go run main.go
```
Expected output pattern:
```
Filtering 8 images (1500000 pixels each):

GOMAXPROCS   Wall-Clock      Speedup
----------------------------------------
1            520ms           1.00x
2            265ms           1.96x
4            135ms           3.85x
8            70ms            7.43x
```

## Step 3 -- IO-Bound Workload: Database Query Simulation

Show that simulated database queries (IO-bound work) benefit minimally from additional Ps. In a real service, this is why adding CPU cores does not speed up a database-heavy endpoint.

```go
package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"
)

const dbRoundTripLatency = 50 * time.Millisecond

func simulateDBQuery(queryName string) string {
	time.Sleep(dbRoundTripLatency)
	return queryName + ": 42 rows"
}

func benchmarkQueriesAtProcs(queries []string, procs int) time.Duration {
	runtime.GOMAXPROCS(procs)
	start := time.Now()
	var wg sync.WaitGroup

	for _, query := range queries {
		wg.Add(1)
		go func(q string) {
			defer wg.Done()
			simulateDBQuery(q)
		}(query)
	}

	wg.Wait()
	return time.Since(start)
}

func printIOBoundBenchmark(queries []string) {
	fmt.Printf("Running %d database queries (%v each, IO-bound):\n\n", len(queries), dbRoundTripLatency)
	fmt.Printf("%-12s %-15s %-10s\n", "GOMAXPROCS", "Wall-Clock", "Speedup")
	fmt.Println(strings.Repeat("-", 40))

	var baselineTime time.Duration
	for _, procs := range []int{1, 2, 4, runtime.NumCPU()} {
		elapsed := benchmarkQueriesAtProcs(queries, procs)
		if procs == 1 {
			baselineTime = elapsed
		}
		speedup := float64(baselineTime) / float64(elapsed)
		fmt.Printf("%-12d %-15v %-10.2fx\n", procs, elapsed.Round(time.Millisecond), speedup)
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Println()
	fmt.Println("IO-bound queries show ~1.0x speedup regardless of GOMAXPROCS.")
	fmt.Println("Goroutines park while waiting for the database. Adding cores")
	fmt.Println("does not make the database respond faster.")
}

func main() {
	queries := []string{
		"SELECT users", "SELECT orders", "SELECT products",
		"SELECT reviews", "SELECT inventory", "SELECT payments",
		"SELECT sessions", "SELECT audit_log", "SELECT configs",
		"SELECT metrics", "SELECT alerts", "SELECT schedules",
	}
	printIOBoundBenchmark(queries)
}
```

**What's happening here:** Twelve simulated database queries each take 50ms. Sleeping goroutines do NOT occupy a P -- they are parked by the runtime and the P is free to run other goroutines. So all twelve can sleep concurrently even with GOMAXPROCS=1.

**Key insight:** IO-bound work shows ~1.0x speedup regardless of GOMAXPROCS because the goroutines spend almost no time on the CPU. They are waiting for the database, not computing. In production, if your service is slow because of database latency, adding more CPU cores will not help. You need to optimize queries, add caching, or scale the database.

### Verification
```bash
go run main.go
```
Expected output:
```
Running 12 database queries (50ms each, IO-bound):

GOMAXPROCS   Wall-Clock      Speedup
----------------------------------------
1            52ms            1.00x
2            51ms            1.02x
4            51ms            1.02x
8            51ms            1.02x

IO-bound queries show ~1.0x speedup regardless of GOMAXPROCS.
Goroutines park while waiting for the database. Adding cores
does not make the database respond faster.
```

## Step 4 -- Mixed Workload: API Handler with Compute + IO

Real API handlers mix CPU and IO. A request might validate input (CPU), query the database (IO), then serialize a response (CPU). The speedup from GOMAXPROCS is proportional to the CPU fraction.

```go
package main

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	validationIterations   = 5_000_000
	serializationIterations = 1_000_000
	dbQueryLatency         = 20 * time.Millisecond
	concurrentRequests     = 8
)

func validateInput() {
	result := 0.0
	for i := 0; i < validationIterations; i++ {
		result += math.Sin(float64(i))
	}
	_ = result
}

func queryDatabase() {
	time.Sleep(dbQueryLatency)
}

func serializeResponse() {
	result := 0.0
	for i := 0; i < serializationIterations; i++ {
		result += math.Cos(float64(i))
	}
	_ = result
}

func handleAPIRequest() {
	validateInput()
	queryDatabase()
	serializeResponse()
}

func benchmarkMixedWorkload(requestCount, procs int) time.Duration {
	runtime.GOMAXPROCS(procs)
	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < requestCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handleAPIRequest()
		}()
	}

	wg.Wait()
	return time.Since(start)
}

func main() {
	fmt.Printf("Processing %d API requests (CPU validation + DB query + CPU serialization):\n\n", concurrentRequests)
	fmt.Printf("%-12s %-15s %-10s\n", "GOMAXPROCS", "Wall-Clock", "Speedup")
	fmt.Println(strings.Repeat("-", 40))

	var baselineTime time.Duration
	for _, procs := range []int{1, 2, 4, runtime.NumCPU()} {
		elapsed := benchmarkMixedWorkload(concurrentRequests, procs)
		if procs == 1 {
			baselineTime = elapsed
		}
		speedup := float64(baselineTime) / float64(elapsed)
		fmt.Printf("%-12d %-15v %-10.2fx\n", procs, elapsed.Round(time.Millisecond), speedup)
	}

	runtime.GOMAXPROCS(runtime.NumCPU())
	fmt.Println()
	fmt.Println("Mixed workload: speedup is between pure-CPU (linear) and pure-IO (flat).")
	fmt.Println("The CPU phases parallelize, but the IO phase does not benefit from more Ps.")
	fmt.Println("This is Amdahl's Law: speedup is limited by the sequential fraction.")
}
```

**What's happening here:** Each API request handler does ~50ms of CPU work (validation + serialization) plus ~20ms of IO wait (database query). With GOMAXPROCS=1, the CPU portions serialize (8 * 50ms = 400ms) plus 20ms IO = ~420ms. With GOMAXPROCS=8, the CPU portions parallelize (~50ms) plus 20ms IO = ~70ms.

**Key insight:** Speedup is proportional to the CPU fraction of the workload. If 70% of the time is CPU-bound, you can speed that part up linearly. The IO fraction does not benefit from more Ps. This is Amdahl's Law in practice. When profiling a slow endpoint, first determine the CPU/IO split before throwing hardware at it.

### Verification
```bash
go run main.go
```
Expected output: speedup between pure CPU (linear) and pure IO (flat).

## Common Mistakes

### Setting GOMAXPROCS Higher Than CPU Count

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

const excessiveProcs = 100

func computeSqrtSum(iterations int) float64 {
	result := 0.0
	for j := 0; j < iterations; j++ {
		result += math.Sqrt(float64(j))
	}
	return result
}

func main() {
	runtime.GOMAXPROCS(excessiveProcs) // on a 4-core machine: no benefit

	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = computeSqrtSum(50_000_000)
		}()
	}
	wg.Wait()
	fmt.Printf("Wall-clock: %v\n", time.Since(start))
}
```

**What happens:** For CPU-bound work, GOMAXPROCS > NumCPU provides no benefit and may slightly hurt performance due to context switching overhead. The hardware has only NumCPU physical execution units. Your image filters will not run faster on 100 Ps if you only have 8 cores.

**Fix:** Leave GOMAXPROCS at its default (`runtime.NumCPU()`). Only tune it when benchmarks prove a different value is better.

### Assuming More Goroutines Means More Parallelism

**Wrong thinking:** "If I create 1000 goroutines for 1000 images, they'll all filter in parallel."

**What happens:** Only GOMAXPROCS goroutines can execute Go code simultaneously. The rest wait in run queues. With 8 cores, only 8 images are filtered at once; the other 992 wait their turn.

**Fix:** For CPU-bound work, creating more goroutines than Ps increases scheduling overhead without improving throughput. Match worker count to GOMAXPROCS for CPU-bound image processing.

### Benchmarking Without Warming Up

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"math"
	"time"
)

func main() {
	// First run includes GC, CPU cache cold starts
	start := time.Now()
	result := 0.0
	for i := 0; i < 50_000_000; i++ {
		result += math.Sqrt(float64(i))
	}
	fmt.Printf("Time: %v (result: %.0f)\n", time.Since(start), result)
}
```

**What happens:** The first measurement includes one-time costs (GC warmup, CPU cache cold starts) that inflate the result.

**Correct -- warm up first:**
```go
package main

import (
	"fmt"
	"math"
	"time"
)

func doFilter() float64 {
	result := 0.0
	for i := 0; i < 50_000_000; i++ {
		result += math.Sqrt(float64(i))
	}
	return result
}

func main() {
	doFilter() // warmup run: fill CPU caches

	start := time.Now()
	result := doFilter()
	fmt.Printf("Time: %v (result: %.0f)\n", time.Since(start), result)
}
```

## Review

The `go` keyword buys you concurrency; `GOMAXPROCS` buys you parallelism, and
the two are not the same lever. GOMAXPROCS caps the number of logical processors
(Ps), which caps how many goroutines can execute Go code at the same instant.
For CPU-bound work -- image filtering, checksums -- speedup is roughly linear as
you add Ps, but only up to the physical core count; past that the hardware has
no more execution units and extra Ps just add scheduling overhead. IO-bound work
barely moves, because its goroutines are parked waiting on the database or the
network rather than burning CPU. Mixed workloads land in between, at a speedup
proportional to their CPU fraction -- Amdahl's Law made concrete. The default of
`GOMAXPROCS=NumCPU()` is right for almost everything, and spawning more
goroutines than Ps never adds parallelism for CPU-bound work; it only adds queue
depth.

To prove you own the distinction, build one program that defines three workload
types -- an image filter (CPU-bound transformation), a batch of database queries
(IO-bound 50ms sleeps), and an API handler that mixes both -- runs each across
GOMAXPROCS from 1 to `NumCPU()`, and prints a summary table of GOMAXPROCS,
wall-clock, and speedup for each. You should be able to explain, before you run
it, why the optimal GOMAXPROCS differs between the three, and why the IO-bound
column stays flat while the CPU-bound one climbs.

## Resources

- [runtime.GOMAXPROCS](https://pkg.go.dev/runtime#GOMAXPROCS) -- the API that sets and reports how many Ps are available for parallel execution.
- [Go Blog: Concurrency is not parallelism](https://go.dev/blog/waza-talk) -- Rob Pike's essay drawing exactly the structure-versus-execution line this exercise measures.
- [Rob Pike: Concurrency is not Parallelism (video)](https://www.youtube.com/watch?v=oV9rvDllKEg) -- the talk that popularized the distinction, with worked diagrams.
- [Amdahl's Law (Wikipedia)](https://en.wikipedia.org/wiki/Amdahl%27s_law) -- the formula that bounds speedup by a workload's sequential fraction.

---

Back to [Concurrency](../../concurrency.md) | Next: [06-cooperative-scheduling](../06-cooperative-scheduling/06-cooperative-scheduling.md)
