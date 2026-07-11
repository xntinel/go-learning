# Exercise 8: A Million Goroutines

Go developers hear that goroutines are "cheap" and that you can spin up
millions of them. This exercise puts the claim on a scale: it builds a load
testing simulator that launches goroutines as stand-in users and measures what
each one actually costs -- launch time, stack and heap memory, and the GC
pause that grows as stacks pile up. The point is not to celebrate a big number
but to learn where the goroutine-per-task pattern stops paying for itself and
unbounded creation starts getting your process OOM-killed.

## What you'll build

```text
08-million-goroutines/
  main.go        launch/memory/GC benchmarks from 1K to 1M goroutines, plus a
                 bounded-semaphore worker pool for the production pattern
```

- Build: a load-test simulator that profiles goroutine cost at 1K, 10K, 100K, 500K, and 1M.
- Implement: `benchmarkLaunch`, `measureMemoryAtScale`, `measureGCImpact`, `processWithGoroutines`, `profileAtScale`, and a semaphore-bounded `processWithSemaphore`.
- Verify: `go run main.go` (each step is its own runnable program).

### Why "cheap" has a measurable ceiling

Go developers often hear that goroutines are "cheap" and that you can create millions of them. This exercise puts that claim to the test by simulating a load testing tool: launching goroutines that simulate users making requests against your service. By systematically measuring the cost of creating 1K, 10K, 100K, and 1M simulated users, you will develop an intuitive understanding of exactly how cheap (or expensive) goroutines are.

More importantly, this exercise teaches you when NOT to create goroutines. Just because you can create a million goroutines does not mean you should. Each goroutine consumes memory, occupies scheduler run queues, and competes for CPU time. For CPU-bound work, the optimal number of goroutines is typically `runtime.NumCPU()`, not "as many as possible." For IO-bound work, goroutines are often the right abstraction, but unbounded creation can exhaust memory.

Understanding these tradeoffs separates informed Go developers from those who use goroutines indiscriminately. After this exercise, you will be able to reason about goroutine costs in real systems and make architecture decisions grounded in measurement rather than folklore.

## Step 1 -- Load Test Simulation: Launch Time at Scale

Imagine building a load testing tool like `hey` or `k6`. Each simulated user is a goroutine that makes a request to your service. How fast can you spin up users?

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	minThinkTimeMs = 1
	maxThinkTimeMs = 10
	cleanupDelay   = 100 * time.Millisecond
)

type LaunchBenchmark struct {
	UserCount   int
	LaunchTime  time.Duration
	PerUser     time.Duration
	UsersPerSec float64
}

func simulateUser(ready *int64, done <-chan struct{}) {
	atomic.AddInt64(ready, 1)
	time.Sleep(time.Duration(rand.IntN(maxThinkTimeMs)+minThinkTimeMs) * time.Millisecond)
	<-done
}

func benchmarkLaunch(userCount int) LaunchBenchmark {
	done := make(chan struct{})
	var ready int64

	start := time.Now()
	for i := 0; i < userCount; i++ {
		go simulateUser(&ready, done)
	}

	for atomic.LoadInt64(&ready) < int64(userCount) {
		runtime.Gosched()
	}
	launchTime := time.Since(start)

	close(done)
	time.Sleep(cleanupDelay)
	runtime.GC()

	return LaunchBenchmark{
		UserCount:   userCount,
		LaunchTime:  launchTime,
		PerUser:     launchTime / time.Duration(userCount),
		UsersPerSec: float64(userCount) / launchTime.Seconds(),
	}
}

func printLaunchTable(counts []int) {
	fmt.Println("=== Load Test Simulator: Goroutine Launch Benchmark ===")
	fmt.Printf("%-12s %-15s %-15s %-15s\n", "Users", "Spin-up Time", "Per User", "Users/sec")
	fmt.Println(strings.Repeat("-", 60))

	for _, count := range counts {
		b := benchmarkLaunch(count)
		fmt.Printf("%-12d %-15v %-15v %-15.0f\n",
			b.UserCount, b.LaunchTime.Round(time.Millisecond), b.PerUser, b.UsersPerSec)
	}
}

func main() {
	counts := []int{1_000, 10_000, 100_000, 500_000, 1_000_000}
	printLaunchTable(counts)
}
```

**What's happening here:** For each user count, we create goroutines that simulate load test users: each does a small random delay (think time) before waiting for a signal. We measure how long it takes to spin up all users and calculate per-user cost and throughput.

**Key insight:** Each goroutine takes roughly 500ns-1us to create. This means you can spin up approximately 1 million simulated users per second. For a load testing tool, this is the startup overhead before any actual requests are sent.

**What would happen if you didn't clean up between iterations?** Memory from the previous round's goroutines would still be in use, inflating the next round's measurements and potentially causing OOM for large counts.

### Verification
```bash
go run main.go
```
Expected output pattern (values vary by machine):
```
=== Load Test Simulator: Goroutine Launch Benchmark ===
Users        Spin-up Time    Per User        Users/sec
------------------------------------------------------------
1000         1ms             1us             1000000
10000        8ms             800ns           1250000
100000       75ms            750ns           1333333
500000       380ms           760ns           1315789
1000000      780ms           780ns           1282051
```

## Step 2 -- Memory Footprint per Simulated User

Use `runtime.MemStats` to measure how much memory each simulated user consumes. This determines how many concurrent users your load testing machine can sustain.

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const settleDelay = 50 * time.Millisecond

type MemoryMeasurement struct {
	UserCount int
	StackDiff uint64
	HeapDiff  uint64
	SysDiff   uint64
	PerUser   uint64
}

func formatBytes(b uint64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.2f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.2f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func simulateUserWithState(ready *int64, done <-chan struct{}) {
	userID := rand.IntN(1_000_000)
	requestPath := fmt.Sprintf("/api/resource/%d", userID)
	_ = requestPath
	atomic.AddInt64(ready, 1)
	<-done
}

func measureMemoryAtScale(userCount int) MemoryMeasurement {
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	done := make(chan struct{})
	var ready int64
	for i := 0; i < userCount; i++ {
		go simulateUserWithState(&ready, done)
	}

	for atomic.LoadInt64(&ready) < int64(userCount) {
		runtime.Gosched()
	}
	time.Sleep(settleDelay)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	stackDiff := after.StackInuse - before.StackInuse
	heapDiff := after.HeapInuse - before.HeapInuse
	total := stackDiff + heapDiff

	close(done)
	time.Sleep(200 * time.Millisecond)
	runtime.GC()

	return MemoryMeasurement{
		UserCount: userCount,
		StackDiff: stackDiff,
		HeapDiff:  heapDiff,
		SysDiff:   after.Sys - before.Sys,
		PerUser:   total / uint64(userCount),
	}
}

func printMemoryTable(counts []int) {
	fmt.Println("=== Memory per Simulated User ===")
	fmt.Printf("%-12s %-15s %-15s %-15s %-15s\n",
		"Users", "StackInUse", "HeapInUse", "Sys (Total)", "Per User")
	fmt.Println(strings.Repeat("-", 75))

	for _, count := range counts {
		m := measureMemoryAtScale(count)
		fmt.Printf("%-12d %-15s %-15s %-15s %-15s\n",
			m.UserCount,
			formatBytes(m.StackDiff),
			formatBytes(m.HeapDiff),
			formatBytes(m.SysDiff),
			formatBytes(m.PerUser),
		)
	}

	fmt.Println()
	fmt.Println("At ~8 KB per user, your load test machine's RAM determines the ceiling.")
	fmt.Println("A 16 GB machine can sustain ~1M simulated users.")
	fmt.Println("A 4 GB machine caps out at ~250K users.")
}

func main() {
	counts := []int{1_000, 10_000, 100_000, 500_000}
	printMemoryTable(counts)
}
```

**What's happening here:** For each user count, we measure three memory metrics. `StackInuse` is the dominant cost: stack memory for each user goroutine. `HeapInuse` includes goroutine descriptor structs and any heap allocations. Each simulated user has local state (user ID, request path) to be more realistic than a bare channel receive.

**Key insight:** At ~8 KB per goroutine (stack + heap), 1 million users consume roughly 8 GB of memory. When planning a load test, your machine's RAM directly determines how many concurrent users you can simulate. This is why load testing tools like `k6` and `locust` are careful about per-user memory.

### Verification
```bash
go run main.go
```
Expected output pattern:
```
=== Memory per Simulated User ===
Users        StackInUse      HeapInUse       Sys (Total)     Per User
---------------------------------------------------------------------------
1000         8.00 MB         0.50 MB         10.00 MB        8.50 KB
10000        80.00 MB        5.00 MB         90.00 MB        8.50 KB
100000       800.00 MB       50.00 MB        900.00 MB       8.50 KB
500000       3.91 GB         250.00 MB       4.50 GB         8.50 KB
```

## Step 3 -- Measuring GC Impact on Latency

In a load testing tool or a production server, GC pauses directly affect request latency. Show how goroutine count affects GC pause times.

```go
package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const gcMeasureSettleDelay = 50 * time.Millisecond

type GCMeasurement struct {
	GoroutineCount int
	GCPause        time.Duration
	NumGC          uint32
	AllocRateMB    float64
}

func measureGCImpact(goroutineCount int) GCMeasurement {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	done := make(chan struct{})
	var ready int64
	for i := 0; i < goroutineCount; i++ {
		go func() {
			atomic.AddInt64(&ready, 1)
			<-done
		}()
	}
	for atomic.LoadInt64(&ready) < int64(goroutineCount) {
		runtime.Gosched()
	}
	time.Sleep(gcMeasureSettleDelay)

	gcStart := time.Now()
	runtime.GC()
	gcPause := time.Since(gcStart)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	close(done)
	time.Sleep(200 * time.Millisecond)
	runtime.GC()

	return GCMeasurement{
		GoroutineCount: goroutineCount,
		GCPause:        gcPause,
		NumGC:          after.NumGC - before.NumGC,
		AllocRateMB:    float64(after.TotalAlloc-before.TotalAlloc) / (1024 * 1024),
	}
}

func printGCImpactTable(counts []int) {
	fmt.Println("=== GC Impact at Scale ===")
	fmt.Println("More goroutines = more stacks for GC to scan = longer pauses")
	fmt.Println()
	fmt.Printf("%-12s %-15s %-15s %-15s\n",
		"Goroutines", "GC Pause", "Num GC", "Alloc Rate")
	fmt.Println(strings.Repeat("-", 60))

	for _, count := range counts {
		m := measureGCImpact(count)
		fmt.Printf("%-12d %-15v %-15d %-15.2f MB\n",
			m.GoroutineCount, m.GCPause.Round(time.Microsecond), m.NumGC, m.AllocRateMB)
	}

	fmt.Println()
	fmt.Println("GC pauses are the hidden cost of massive goroutine counts.")
	fmt.Println("For latency-sensitive services (p99 < 10ms), keep goroutine")
	fmt.Println("counts under the threshold where GC pauses start to matter.")
}

func main() {
	counts := []int{1_000, 10_000, 100_000, 500_000}
	printGCImpactTable(counts)
}
```

**What's happening here:** We create goroutines, then force a GC cycle and measure how long it takes. GC pause time scales with goroutine count because the garbage collector must scan every goroutine's stack for pointers.

**Key insight:** GC pause time is the hidden cost of millions of goroutines. While individual goroutines are cheap, the GC must scan all their stacks during every cycle. At 500K goroutines, GC pauses can reach tens of milliseconds. For a service with p99 latency requirements under 10ms, this is unacceptable. This is why production servers use bounded worker pools instead of unbounded goroutine creation.

### Verification
```bash
go run main.go
```
Expected output (GC pause grows with count):
```
=== GC Impact at Scale ===
More goroutines = more stacks for GC to scan = longer pauses

Goroutines   GC Pause        Num GC          Alloc Rate
------------------------------------------------------------
1000         200us           2               0.50 MB
10000        1ms             3               5.00 MB
100000       15ms            5               50.00 MB
500000       75ms            8               250.00 MB
```

## Step 4 -- When NOT to Create Goroutines: CPU-Bound Work

Demonstrate that for CPU-bound work (like processing request payloads), more goroutines does NOT mean faster processing. The optimal count is `runtime.NumCPU()`.

```go
package main

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"time"
)

const dataPointCount = 10_000_000

type ChunkResult struct {
	Sum float64
}

func generateDataPoints(count int) []float64 {
	data := make([]float64, count)
	for i := range data {
		data[i] = float64(i) * 0.001
	}
	return data
}

func processChunk(chunk []float64) float64 {
	var sum float64
	for _, v := range chunk {
		sum += math.Sqrt(v)
	}
	return sum
}

func processWithGoroutines(data []float64, goroutineCount int) (float64, time.Duration) {
	chunkSize := len(data) / goroutineCount
	if chunkSize == 0 {
		chunkSize = 1
	}

	start := time.Now()
	results := make(chan float64, goroutineCount)
	launched := 0

	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		launched++
		go func(chunk []float64) {
			results <- processChunk(chunk)
		}(data[i:end])
	}

	var total float64
	for i := 0; i < launched; i++ {
		total += <-results
	}

	return total, time.Since(start)
}

func printOverheadTable(data []float64, goroutineCounts []int) {
	fmt.Printf("Processing %d data points (CPU-bound work):\n\n", len(data))
	fmt.Printf("%-15s %-15s %-15s\n", "Goroutines", "Wall-Clock", "vs Baseline")
	fmt.Println(strings.Repeat("-", 48))

	var baselineTime time.Duration
	for _, numG := range goroutineCounts {
		_, elapsed := processWithGoroutines(data, numG)
		if numG == 1 {
			baselineTime = elapsed
		}
		overhead := float64(elapsed) / float64(baselineTime)
		fmt.Printf("%-15d %-15v %-15.2fx\n", numG, elapsed.Round(time.Microsecond), overhead)
	}

	fmt.Println()
	fmt.Printf("Optimal: %d goroutines (= NumCPU)\n", runtime.NumCPU())
	fmt.Println("More goroutines add scheduling overhead without additional parallelism.")
	fmt.Println("For CPU-bound request processing, use a fixed worker pool, not goroutine-per-item.")
}

func main() {
	data := generateDataPoints(dataPointCount)
	goroutineCounts := []int{1, runtime.NumCPU(), 100, 1_000, 10_000}
	printOverheadTable(data, goroutineCounts)
}
```

**What's happening here:** We process 10 million data points using different numbers of goroutines. With 1 goroutine: no parallelism. With NumCPU: optimal parallelism. With 10,000: scheduling overhead dominates, making it SLOWER than 1 goroutine.

**Key insight:** For CPU-bound work, creating more goroutines than cores hurts performance. Each goroutine adds scheduling overhead (run queue insertion, context switching) without doing any additional useful work. When processing request payloads, computing reports, or transforming data, use `runtime.NumCPU()` workers, not one goroutine per data item.

### Verification
```bash
go run main.go
```
Expected output pattern:
```
Processing 10000000 data points (CPU-bound work):

Goroutines      Wall-Clock      vs Baseline
------------------------------------------------
1               12ms            1.00x
8               3ms             0.25x   (speedup from parallelism)
100             4ms             0.33x   (slight overhead)
1000            8ms             0.67x   (overhead growing)
10000           45ms            3.75x   (SLOWER than 1 goroutine!)

Optimal: 8 goroutines (= NumCPU)
More goroutines add scheduling overhead without additional parallelism.
For CPU-bound request processing, use a fixed worker pool, not goroutine-per-item.
```

## Step 5 -- Complete Scalability Profile for Your Machine

Create a comprehensive report combining all measurements. This is the kind of capacity planning data you would use to configure a production load testing tool or set goroutine limits in your server.

```go
package main

import (
	"fmt"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const profileSettleDelay = 50 * time.Millisecond

type ScalabilityMeasurement struct {
	Count      int
	LaunchTime time.Duration
	StackMem   uint64
	HeapMem    uint64
	GCPause    time.Duration
}

func formatBytes(b uint64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.2f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.2f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func profileAtScale(count int) ScalabilityMeasurement {
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	done := make(chan struct{})
	var ready int64

	launchStart := time.Now()
	for i := 0; i < count; i++ {
		go func() {
			atomic.AddInt64(&ready, 1)
			<-done
		}()
	}
	for atomic.LoadInt64(&ready) < int64(count) {
		runtime.Gosched()
	}
	launchTime := time.Since(launchStart)
	time.Sleep(profileSettleDelay)

	gcStart := time.Now()
	runtime.GC()
	gcPause := time.Since(gcStart)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	close(done)
	time.Sleep(200 * time.Millisecond)

	return ScalabilityMeasurement{
		Count:      count,
		LaunchTime: launchTime,
		StackMem:   after.StackInuse - before.StackInuse,
		HeapMem:    after.HeapInuse - before.HeapInuse,
		GCPause:    gcPause,
	}
}

func printScalabilityProfile(counts []int) {
	fmt.Println("=== Goroutine Scalability Profile ===")
	fmt.Println("Use this data for capacity planning: load testing, server limits,")
	fmt.Println("and worker pool sizing.")
	fmt.Println()

	var measurements []ScalabilityMeasurement
	for _, count := range counts {
		measurements = append(measurements, profileAtScale(count))
	}

	fmt.Printf("%-10s %-12s %-12s %-12s %-12s %-12s\n",
		"Count", "Spin-up", "Stack", "Heap", "GC Pause", "KB/goroutine")
	fmt.Println(strings.Repeat("-", 72))

	for _, m := range measurements {
		perG := float64(m.StackMem+m.HeapMem) / float64(m.Count) / 1024
		fmt.Printf("%-10d %-12v %-12s %-12s %-12v %-12.1f\n",
			m.Count,
			m.LaunchTime.Round(time.Millisecond),
			formatBytes(m.StackMem),
			formatBytes(m.HeapMem),
			m.GCPause.Round(time.Microsecond),
			perG,
		)
	}

	printCapacityGuidelines()
}

func printCapacityGuidelines() {
	fmt.Println()
	fmt.Println("--- Capacity Planning Guidelines ---")
	fmt.Printf("CPU cores:           %d\n", runtime.NumCPU())
	fmt.Printf("CPU-bound optimal:   %d goroutines (one per core)\n", runtime.NumCPU())
	fmt.Println("IO-bound (server):   1 goroutine per connection, bounded by semaphore")
	fmt.Println("IO-bound (load test): 1 goroutine per simulated user, limited by RAM")
	fmt.Println("Practical ceiling:   depends on RAM; ~100K-1M for most machines")
	fmt.Println()
	fmt.Println("Rule of thumb: if goroutine count can grow unbounded in production,")
	fmt.Println("add a semaphore. Unbounded goroutine creation is the #1 cause of")
	fmt.Println("Go service OOM kills under load.")
}

func main() {
	counts := []int{100, 1_000, 10_000, 100_000}
	printScalabilityProfile(counts)
}
```

**What's happening here:** For each goroutine count, we measure launch time, stack memory, heap memory, and GC pause time. The result is a comprehensive profile specific to your machine, suitable for making real capacity decisions.

**Key insight:** This gives you concrete numbers for YOUR hardware. A machine with 16 GB RAM can comfortably sustain ~500K idle goroutines; a machine with 4 GB caps out at ~100K. Never assume goroutine costs -- measure them on the hardware where your code will actually run.

### Verification
```bash
go run main.go
```
Expected output:
```
=== Goroutine Scalability Profile ===
Use this data for capacity planning: load testing, server limits,
and worker pool sizing.

Count      Spin-up      Stack        Heap         GC Pause     KB/goroutine
------------------------------------------------------------------------
100        0ms          800.00 KB    50.00 KB     50us         8.5
1000       1ms          8.00 MB     500.00 KB     200us        8.5
10000      8ms          80.00 MB    5.00 MB       2ms          8.5
100000     75ms         800.00 MB   50.00 MB      15ms         8.5

--- Capacity Planning Guidelines ---
CPU cores:           8
CPU-bound optimal:   8 goroutines (one per core)
IO-bound (server):   1 goroutine per connection, bounded by semaphore
IO-bound (load test): 1 goroutine per simulated user, limited by RAM
Practical ceiling:   depends on RAM; ~100K-1M for most machines
```

## Deep Dive: Bounded Concurrency with Semaphores

In production, never create goroutines without bounds. A load testing tool or server that creates unbounded goroutines will OOM under heavy traffic. Use a semaphore pattern:

```go
package main

import (
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

const (
	totalBoundedRequests = 1000
	maxConcurrentSlots   = 50
	minRequestLatencyMs  = 1
	maxRequestLatencyMs  = 10
)

func simulateBoundedRequest(completed *int64, totalExpected int64, done chan struct{}) {
	time.Sleep(time.Duration(rand.IntN(maxRequestLatencyMs)+minRequestLatencyMs) * time.Millisecond)
	if atomic.AddInt64(completed, 1) == totalExpected {
		close(done)
	}
}

func processWithSemaphore(totalRequests, maxConcurrent int) time.Duration {
	sem := make(chan struct{}, maxConcurrent)
	done := make(chan struct{})
	var completed int64

	start := time.Now()

	for i := 0; i < totalRequests; i++ {
		sem <- struct{}{} // acquire slot (blocks when at capacity)
		go func() {
			defer func() { <-sem }() // release slot when done
			simulateBoundedRequest(&completed, int64(totalRequests), done)
		}()
	}

	<-done
	return time.Since(start)
}

func main() {
	elapsed := processWithSemaphore(totalBoundedRequests, maxConcurrentSlots)
	fmt.Printf("Processed %d requests in %v\n", totalBoundedRequests, elapsed.Round(time.Millisecond))
	fmt.Printf("Max concurrent: %d (bounded by semaphore)\n", maxConcurrentSlots)
	fmt.Println()
	fmt.Println("Without the semaphore, all 1000 goroutines would exist simultaneously.")
	fmt.Println("With it, at most 50 run at once, keeping memory predictable.")
}
```

**What's happening here:** The semaphore channel has a buffer of 50. Sending to it blocks when 50 goroutines are already running. Each goroutine releases its slot on completion. This ensures memory usage is bounded regardless of the total request count.

**Key insight:** This is the production pattern for goroutine-per-task at scale. Unbounded creation is fine for small numbers (hundreds), but anything that could grow to thousands or more needs a bound. Every production Go server should have a concurrency limit.

## Common Mistakes

### Unbounded Goroutine Creation in Servers

**Wrong -- complete program:**
```go
package main

import (
	"fmt"
	"net"
)

func main() {
	ln, _ := net.Listen("tcp", ":8080")
	for {
		conn, _ := ln.Accept()
		go func(c net.Conn) { // unbounded: can create millions under load
			defer c.Close()
			buf := make([]byte, 1024)
			c.Read(buf)
			fmt.Fprintf(c, "hello\n")
		}(conn)
	}
}
```

**What happens:** Under a traffic spike or DDoS, goroutine count grows without limit, consuming ~8 KB each. At 100K connections, that is 800 MB of stack alone. The process gets OOM-killed, and ALL in-flight requests are lost.

**Correct -- bounded with semaphore:**
```go
package main

import (
	"fmt"
	"net"
)

const maxConcurrentConnections = 10_000

func handleConnection(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 1024)
	if _, err := conn.Read(buf); err != nil {
		return
	}
	fmt.Fprintf(conn, "hello\n")
}

func main() {
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		fmt.Printf("listen failed: %v\n", err)
		return
	}
	sem := make(chan struct{}, maxConcurrentConnections)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		sem <- struct{}{} // blocks when at capacity -- applies backpressure
		go func(c net.Conn) {
			defer func() { <-sem }()
			handleConnection(c)
		}(conn)
	}
}
```

### Ignoring Memory When Scaling Goroutines

**Wrong thinking:** "Goroutines are free, so I'll create one per row in my 10M-row dataset."

**What happens:** 10M goroutines * ~8 KB each = ~80 GB of memory. The process is immediately OOM-killed. In Kubernetes, the pod gets evicted and your batch job never completes.

**Fix:** Use a worker pool:
```go
package main

import (
	"fmt"
	"runtime"
	"sync"
)

const (
	datasetSize   = 10_000_000
	workQueueSize = 1000
)

func processItem(item int) int {
	return item * item
}

func startWorkerPool(workerCount int, work <-chan int) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range work {
				_ = processItem(item)
			}
		}()
	}
	return &wg
}

func main() {
	data := make([]int, datasetSize)
	for i := range data {
		data[i] = i
	}

	work := make(chan int, workQueueSize)
	workerCount := runtime.NumCPU()
	wg := startWorkerPool(workerCount, work)

	for _, item := range data {
		work <- item
	}
	close(work)
	wg.Wait()

	fmt.Printf("Processed %d items with %d workers (bounded memory)\n",
		len(data), workerCount)
}
```

### Not Measuring Before Deciding

**Wrong thinking:** "I'll use exactly 1000 goroutines because someone said that's a good number."

**What happens:** The optimal number depends on your workload (CPU-bound vs IO-bound), your machine's RAM, and the per-goroutine memory usage of your specific code. 1000 might be perfect for IO-bound API calls but terrible for CPU-bound data processing.

**Fix:** Benchmark with different goroutine counts and measure actual performance. Use this exercise's profiling approach to find the sweet spot for your specific workload and hardware.

## Review

The numbers this exercise produces are the whole lesson. A goroutine costs
roughly 500ns to a microsecond to create -- about a million per second -- and
carries two to eight kilobytes of stack plus heap overhead, so a million idle
goroutines is eight to sixteen gigabytes of resident memory, not a rounding
error. The cost that hides from a casual reading is the garbage collector:
every cycle scans every live goroutine's stack, so pause time climbs with the
count and turns into tail latency long before you run out of RAM. None of this
makes goroutines expensive -- it makes them measurable. For CPU-bound work the
right count is `runtime.NumCPU()`, because extra goroutines add scheduling
overhead without adding parallelism; for IO-bound work goroutine-per-task is
the right shape, but only behind a bound. A semaphore or a fixed worker pool is
what separates a service that degrades gracefully under load from one that gets
OOM-killed, which is the single most common way Go services die.

You should be able to build the profiling harness yourself now without
re-reading the steps: launch a batch of goroutines, read `runtime.MemStats`
before and after, force a GC and time it, then print launch time, stack, heap,
and pause against the count. Push it the way the exercise asks -- binary-search
from 100K for the largest simulated-user count that stays under two gigabytes,
give each user realistic think time and request work, and report a practical
ceiling with a fifty-percent safety margin. The habit that matters is measuring
on the hardware the code will actually run on, not trusting a number someone
quoted you.

## Resources
- [runtime.MemStats](https://pkg.go.dev/runtime#MemStats) -- the struct whose StackInuse, HeapInuse, and Sys fields this exercise reads to size one goroutine.
- [runtime.ReadMemStats](https://pkg.go.dev/runtime#ReadMemStats) -- how to snapshot those stats before and after launching a batch.
- [Go Blog: Go GC: Prioritizing low latency and simplicity](https://go.dev/blog/go15gc) -- why stack scanning makes GC pause time scale with goroutine count.
- [Concurrency in Go (Katherine Cox-Buday)](https://www.oreilly.com/library/view/concurrency-in-go/9781491941294/) -- book-length treatment of worker pools and bounded concurrency.

---

Back to [Concurrency](../../concurrency.md) | Next: [09-goroutine-lifecycle](../09-goroutine-lifecycle/09-goroutine-lifecycle.md)
