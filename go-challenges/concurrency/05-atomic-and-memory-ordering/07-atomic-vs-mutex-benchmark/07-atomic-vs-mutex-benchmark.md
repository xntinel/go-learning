# Exercise 7: Atomic vs Mutex Benchmark

"Atomics are faster than mutexes" is a dangerous oversimplification. Whether it
holds depends on the access pattern, the number of contending goroutines, how
long the critical section runs, and the CPU. Guess wrong and you either reach
for atomics where a mutex would be clearer and just as fast, or pay a 10x
penalty using a mutex where an atomic would fly. The only honest answer is
measurement, and Go ships the tools to get it: `testing.B`, `b.N`, and
`b.RunParallel`.

## What you'll build

```text
07-atomic-vs-mutex-benchmark/
  go.mod                  module for the benchmark suite (go mod init)
  counter_bench_test.go   atomic/mutex/RWMutex counters + Benchmark functions
  main.go                 standalone runner timing all three patterns
```

- Build: a benchmark suite and a decision guide that pit atomic, mutex, and RWMutex against each other across three realistic access patterns.
- Implement: `AtomicCounter`, `MutexCounter`, `RWMutexCounter`, a multi-field `ServiceStats`, and `Benchmark*` functions driven by `b.RunParallel` and `b.SetParallelism`; plus a standalone runner that times each pattern and prints when to use which.
- Verify: `go test -bench=. -benchmem -count=3` for the benchmarks, `go run main.go` for the runner.

### Why measure instead of guess

Go's `testing` package provides built-in benchmarking. Functions starting with
`Benchmark` receive a `*testing.B` and run measured code `b.N` times
(auto-calibrated). `b.RunParallel` distributes iterations across goroutines to
measure concurrent performance.

In this exercise, you benchmark three realistic patterns that occur in every
production service:
1. **Pure counter increment** (write-only): request counters, byte counters, error counters
2. **Read-heavy gauge** (90% reads, 10% writes): connection pool size, queue depth, cache hit ratio
3. **Complex state update** (multi-field): updating related fields that must be consistent

## Step 1 -- Define the Benchmark Targets

Create `counter_bench_test.go` with the counter implementations and their benchmarks. All three patterns in one file:

```go
package main

import (
	"sync"
	"sync/atomic"
	"testing"
)

// --- Counter Implementations ---

type AtomicCounter struct {
	val atomic.Int64
}

func NewAtomicCounter() *AtomicCounter     { return &AtomicCounter{} }
func (c *AtomicCounter) Inc()              { c.val.Add(1) }
func (c *AtomicCounter) Get() int64        { return c.val.Load() }

type MutexCounter struct {
	mu  sync.Mutex
	val int64
}

func NewMutexCounter() *MutexCounter { return &MutexCounter{} }

func (c *MutexCounter) Inc() {
	c.mu.Lock()
	c.val++
	c.mu.Unlock()
}

func (c *MutexCounter) Get() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.val
}

type RWMutexCounter struct {
	mu  sync.RWMutex
	val int64
}

func NewRWMutexCounter() *RWMutexCounter { return &RWMutexCounter{} }

func (c *RWMutexCounter) Inc() {
	c.mu.Lock()
	c.val++
	c.mu.Unlock()
}

func (c *RWMutexCounter) Get() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.val
}

// --- Counter interface for read-heavy gauge benchmarks ---

type Counter interface {
	Inc()
	Get() int64
}

const writeEveryN = 10 // 10% writes, 90% reads

func runReadHeavyBenchmark(b *testing.B, c Counter) {
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if i%writeEveryN == 0 {
				c.Inc()
			} else {
				c.Get()
			}
			i++
		}
	})
}

// --- Pattern 1 Benchmarks: Pure Counter ---

func BenchmarkCounter_Atomic_Sequential(b *testing.B) {
	c := NewAtomicCounter()
	for i := 0; i < b.N; i++ {
		c.Inc()
	}
}

func BenchmarkCounter_Mutex_Sequential(b *testing.B) {
	c := NewMutexCounter()
	for i := 0; i < b.N; i++ {
		c.Inc()
	}
}

func BenchmarkCounter_Atomic_Parallel(b *testing.B) {
	c := NewAtomicCounter()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

func BenchmarkCounter_Mutex_Parallel(b *testing.B) {
	c := NewMutexCounter()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

const highContentionParallelism = 100

func BenchmarkCounter_Atomic_HighContention(b *testing.B) {
	c := NewAtomicCounter()
	b.SetParallelism(highContentionParallelism)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

func BenchmarkCounter_Mutex_HighContention(b *testing.B) {
	c := NewMutexCounter()
	b.SetParallelism(highContentionParallelism)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

// --- Pattern 2 Benchmarks: Read-Heavy Gauge (90% reads, 10% writes) ---

func BenchmarkGauge_Atomic_ReadHeavy(b *testing.B)  { runReadHeavyBenchmark(b, NewAtomicCounter()) }
func BenchmarkGauge_Mutex_ReadHeavy(b *testing.B)   { runReadHeavyBenchmark(b, NewMutexCounter()) }
func BenchmarkGauge_RWMutex_ReadHeavy(b *testing.B) { runReadHeavyBenchmark(b, NewRWMutexCounter()) }

// --- Stats Implementations (multi-field) ---

type ServiceStats struct {
	Requests   int64
	BytesIn    int64
	BytesOut   int64
	Errors     int64
	AvgLatency float64
}

type AtomicServiceStats struct {
	requests atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
	errors   atomic.Int64
}

func (s *AtomicServiceStats) Record(bytesIn, bytesOut int64, isError bool) {
	s.requests.Add(1)
	s.bytesIn.Add(bytesIn)
	s.bytesOut.Add(bytesOut)
	if isError {
		s.errors.Add(1)
	}
}

type MutexServiceStats struct {
	mu    sync.Mutex
	stats ServiceStats
}

func (s *MutexServiceStats) Record(bytesIn, bytesOut int64, isError bool) {
	s.mu.Lock()
	s.stats.Requests++
	s.stats.BytesIn += bytesIn
	s.stats.BytesOut += bytesOut
	if isError {
		s.stats.Errors++
	}
	s.stats.AvgLatency = float64(s.stats.BytesOut) / float64(s.stats.Requests)
	s.mu.Unlock()
}

// --- Pattern 3 Benchmarks: Complex State ---

const (
	benchBytesIn  int64 = 256
	benchBytesOut int64 = 1024
)

func BenchmarkStats_Atomic_Parallel(b *testing.B) {
	s := &AtomicServiceStats{}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Record(benchBytesIn, benchBytesOut, false)
		}
	})
}

func BenchmarkStats_Mutex_Parallel(b *testing.B) {
	s := &MutexServiceStats{}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Record(benchBytesIn, benchBytesOut, false)
		}
	})
}

func BenchmarkStats_Atomic_HighContention(b *testing.B) {
	s := &AtomicServiceStats{}
	b.SetParallelism(highContentionParallelism)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Record(benchBytesIn, benchBytesOut, false)
		}
	})
}

func BenchmarkStats_Mutex_HighContention(b *testing.B) {
	s := &MutexServiceStats{}
	b.SetParallelism(highContentionParallelism)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Record(benchBytesIn, benchBytesOut, false)
		}
	})
}
```

### Verification

This file must be placed in a directory with a `go.mod`. Initialize and run:
```bash
go mod init bench_demo
go test -bench=Counter_Atomic_Sequential -benchmem -count=1
```
Expected: a benchmark result with ns/op.

## Step 2 -- Run Pattern 1: Pure Counter Increment

Measure the base cost and parallel performance of atomic vs mutex for the most common pattern -- a simple counter:

```bash
go test -bench=BenchmarkCounter -benchmem -count=3
```

Expected results pattern:

| Benchmark | ns/op (approx) | Notes |
|-----------|----------------|-------|
| Counter_Atomic_Sequential | 1-5 ns | Single CPU instruction |
| Counter_Mutex_Sequential | 10-25 ns | Lock + unlock overhead |
| Counter_Atomic_Parallel | 20-60 ns | Cache line bouncing |
| Counter_Mutex_Parallel | 50-150 ns | Lock contention + parking |
| Counter_Atomic_HighContention | 50-200 ns | Still no parking |
| Counter_Mutex_HighContention | 100-500 ns | Goroutine parking dominates |

**Verdict:** For pure counters, atomic wins at every contention level. Use `atomic.Int64` for request counters, error counters, and byte counters.

## Step 3 -- Run Pattern 2: Read-Heavy Gauge

Measure the read-heavy access pattern. This is where RWMutex becomes competitive:

```bash
go test -bench=BenchmarkGauge -benchmem -count=3
```

Expected results pattern:

| Benchmark | ns/op (approx) | Notes |
|-----------|----------------|-------|
| Gauge_Atomic_ReadHeavy | 5-15 ns | Lock-free reads and writes |
| Gauge_Mutex_ReadHeavy | 30-100 ns | All operations serialize |
| Gauge_RWMutex_ReadHeavy | 15-50 ns | Readers run concurrently |

**Verdict:** Atomic is fastest. RWMutex is a viable alternative when you need to read multiple related fields consistently. Plain Mutex serializes everything and is slowest for read-heavy patterns.

## Step 4 -- Run Pattern 3: Complex State Update

Measure multi-field updates. This is where the limitations of atomics become visible:

```bash
go test -bench=BenchmarkStats -benchmem -count=3
```

Expected results pattern:

| Benchmark | ns/op (approx) | Notes |
|-----------|----------------|-------|
| Stats_Atomic_Parallel | 30-80 ns | Multiple atomic ops, but no derived values |
| Stats_Mutex_Parallel | 40-120 ns | Single lock protects ALL fields + computation |
| Stats_Atomic_HighContention | 100-300 ns | Multiple cache line bounces |
| Stats_Mutex_HighContention | 200-600 ns | But can compute AvgLatency |

**Verdict:** Atomic is still faster, BUT the mutex version can compute derived values (like running average) that atomics cannot. When you need multi-field consistency or derived calculations, mutex is the right tool.

## Step 5 -- Run the Complete Suite and Analyze

A standalone program that runs all patterns and prints a decision guide:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	benchGoroutines = 64
	benchIterations = 50000
	readWriteRatio  = 9 // 9 reads per 1 write = 90% reads
	statsBytesIn    = 256
	statsBytesOut   = 1024
)

type PatternResult struct {
	AtomicDuration time.Duration
	OtherDuration  time.Duration
	OtherLabel     string
}

func (r PatternResult) SpeedupRatio() float64 {
	return float64(r.OtherDuration) / float64(r.AtomicDuration)
}

func measureConcurrent(goroutines, iterations int, work func()) time.Duration {
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				work()
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func benchmarkPureCounter() PatternResult {
	var ac atomic.Int64
	var mu sync.Mutex
	var mc int64

	atomicTime := measureConcurrent(benchGoroutines, benchIterations, func() { ac.Add(1) })
	mutexTime := measureConcurrent(benchGoroutines, benchIterations, func() {
		mu.Lock()
		mc++
		mu.Unlock()
	})

	fmt.Println("Pattern 1: Pure Counter Increment")
	fmt.Printf("  Atomic:  %v  (count=%d)\n", atomicTime, ac.Load())
	fmt.Printf("  Mutex:   %v  (count=%d)\n", mutexTime, mc)

	return PatternResult{AtomicDuration: atomicTime, OtherDuration: mutexTime, OtherLabel: "Mutex"}
}

func benchmarkReadHeavyGauge() PatternResult {
	var ag atomic.Int64
	var rwmu sync.RWMutex
	var rv int64

	atomicTime := measureConcurrent(benchGoroutines, benchIterations, func() {
		for k := 0; k < readWriteRatio; k++ {
			ag.Load()
		}
		ag.Add(1)
	})
	rwmTime := measureConcurrent(benchGoroutines, benchIterations, func() {
		for k := 0; k < readWriteRatio; k++ {
			rwmu.RLock()
			_ = rv
			rwmu.RUnlock()
		}
		rwmu.Lock()
		rv++
		rwmu.Unlock()
	})

	fmt.Println("Pattern 2: Read-Heavy Gauge (90% reads, 10% writes)")
	fmt.Printf("  Atomic:   %v\n", atomicTime)
	fmt.Printf("  RWMutex:  %v\n", rwmTime)

	return PatternResult{AtomicDuration: atomicTime, OtherDuration: rwmTime, OtherLabel: "RWMutex"}
}

type AtomicMultiFieldStats struct {
	reqs     atomic.Int64
	bytesIn  atomic.Int64
	bytesOut atomic.Int64
}

func (s *AtomicMultiFieldStats) Record() {
	s.reqs.Add(1)
	s.bytesIn.Add(statsBytesIn)
	s.bytesOut.Add(statsBytesOut)
}

type MutexMultiFieldStats struct {
	mu       sync.Mutex
	reqs     int64
	bytesIn  int64
	bytesOut int64
	avgBytes float64
}

func (s *MutexMultiFieldStats) Record() {
	s.mu.Lock()
	s.reqs++
	s.bytesIn += statsBytesIn
	s.bytesOut += statsBytesOut
	s.avgBytes = float64(s.bytesOut) / float64(s.reqs)
	s.mu.Unlock()
}

func benchmarkComplexState() (PatternResult, float64) {
	as := &AtomicMultiFieldStats{}
	ms := &MutexMultiFieldStats{}

	atomicTime := measureConcurrent(benchGoroutines, benchIterations, as.Record)
	mutexTime := measureConcurrent(benchGoroutines, benchIterations, ms.Record)

	fmt.Println("Pattern 3: Complex State (multi-field + derived value)")
	fmt.Printf("  Atomic:  %v  (but CANNOT compute avgBytes)\n", atomicTime)
	fmt.Printf("  Mutex:   %v  (avgBytes=%.2f)\n", mutexTime, ms.avgBytes)

	return PatternResult{AtomicDuration: atomicTime, OtherDuration: mutexTime, OtherLabel: "Mutex"}, ms.avgBytes
}

func printDecisionGuide() {
	fmt.Println("=== Decision Guide ===")
	fmt.Println()
	fmt.Println("  Use atomic.Int64 / atomic.Bool when:")
	fmt.Println("    - Single counter or flag")
	fmt.Println("    - Independent variables (no consistency between them)")
	fmt.Println("    - Maximum performance matters")
	fmt.Println()
	fmt.Println("  Use sync.RWMutex when:")
	fmt.Println("    - Read-heavy access to multiple related fields")
	fmt.Println("    - Readers must see a consistent snapshot of all fields")
	fmt.Println("    - Write frequency is low")
	fmt.Println()
	fmt.Println("  Use sync.Mutex when:")
	fmt.Println("    - Multi-field updates that must be atomic together")
	fmt.Println("    - Derived values computed during update (running averages, etc)")
	fmt.Println("    - Critical section includes I/O or complex logic")
	fmt.Println("    - Simplicity matters more than raw throughput")
}

func main() {
	fmt.Println("=== Atomic vs Mutex: Data-Driven Decision Guide ===")
	fmt.Printf("Configuration: %d goroutines x %d iterations\n\n", benchGoroutines, benchIterations)

	p1 := benchmarkPureCounter()
	fmt.Printf("  Winner:  Atomic (%.1fx faster)\n\n", p1.SpeedupRatio())

	p2 := benchmarkReadHeavyGauge()
	fmt.Printf("  Winner:  Atomic (%.1fx faster)\n\n", p2.SpeedupRatio())

	benchmarkComplexState()
	fmt.Printf("  Winner:  Depends on requirements\n\n")

	printDecisionGuide()
}
```

### Verification
```bash
go run main.go
```
The output shows real timing data for each pattern and a decision guide based on measured performance.

## Verification

Run the full benchmark suite:
```bash
go test -bench=. -benchmem -count=3
```
For cross-CPU analysis:
```bash
go test -bench=. -benchmem -cpu=1,2,4,8
```

## Common Mistakes

### Benchmark Does Not Use b.N

**Wrong:**
```go
func BenchmarkBad(b *testing.B) {
	c := &AtomicCounter{}
	for i := 0; i < 1000; i++ { // fixed count -- framework cannot calibrate
		c.Inc()
	}
}
```

**Fix:** Always loop to `b.N`:
```go
func BenchmarkGood(b *testing.B) {
	c := &AtomicCounter{}
	for i := 0; i < b.N; i++ {
		c.Inc()
	}
}
```

### Compiler Optimizes Away the Work

**Wrong:**
```go
func BenchmarkGet(b *testing.B) {
	c := &AtomicCounter{}
	for i := 0; i < b.N; i++ {
		c.Get() // result unused -- compiler may eliminate
	}
}
```

**Fix:** Assign to a package-level variable to prevent elimination:
```go
var sink int64

func BenchmarkGet(b *testing.B) {
	c := &AtomicCounter{}
	var s int64
	for i := 0; i < b.N; i++ {
		s = c.Get()
	}
	sink = s
}
```

### Measuring Setup Instead of Work

**Wrong:**
```go
func BenchmarkWithSetup(b *testing.B) {
	data := expensiveSetup() // included in measurement!
	for i := 0; i < b.N; i++ {
		process(data)
	}
}
```

**Fix:** Reset the timer after setup:
```go
func BenchmarkWithSetup(b *testing.B) {
	data := expensiveSetup()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		process(data)
	}
}
```

### Drawing Conclusions from a Single Run

Run benchmarks at least three times (`-count=3`) and on the target hardware. A single run can be misleading due to thermal throttling, background processes, and garbage collection pauses.

## Review

The benchmarks turn a slogan into data. `testing.B` runs your code `b.N` times
with the count auto-calibrated, and `b.RunParallel` spreads those iterations
across `GOMAXPROCS` goroutines so the numbers reflect real contention rather than
a single-threaded best case. Across the three patterns the verdict is not
uniform. For a pure counter, atomic wins at every contention level -- reach for
`atomic.Int64` on request, error, and byte counters. For a read-heavy gauge,
atomic is still fastest while `RWMutex` stays competitive when readers need a
consistent snapshot of several fields. For a multi-field update that also
computes a derived value like a running average, a plain mutex is required,
because atomics can bump independent fields but cannot make several of them
consistent or compute across them. The through-line is that "atomics are faster"
is an oversimplification: benchmark your specific pattern, on target hardware,
several times.

Be ready to explain why `b.RunParallel` gives more realistic results than
hand-spawned goroutines (it uses the framework's own iteration pacing across
GOMAXPROCS), which access pattern lets `RWMutex` beat a plain `Mutex`
(read-heavy, where readers run concurrently), why atomics cannot compute a
running average (no way to make a read-modify-write atomic across several
fields), what `-benchmem` adds (allocations per op, which separate a cheap path
from an allocating one), and why running `-cpu=1,2,4,8` matters (it shows how
each approach scales as cores are added).

## Resources
- [testing.B](https://pkg.go.dev/testing#B) -- the benchmark harness and `b.N` calibration.
- [b.RunParallel](https://pkg.go.dev/testing#B.RunParallel) -- driving a benchmark across GOMAXPROCS goroutines.
- [benchstat tool](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) -- comparing benchmark runs with statistical significance.
- [Go Performance Wiki](https://go.dev/wiki/Performance) -- broader guidance on measuring and reading Go performance.

---

Back to [Concurrency](../../concurrency.md) | Next: [01-context-background-and-todo](../../06-context/01-context-background-and-todo/01-context-background-and-todo.md)
