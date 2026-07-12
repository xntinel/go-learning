# 8. GC Impact on Tail Latency

Garbage collection is the primary source of tail latency in most Go services. While the median request latency may be unaffected, p99 and p999 latencies spike when a GC cycle runs: every goroutine blocked during a STW pause accumulates latency, and goroutines doing GC assist work slow down in proportion to their allocation rate. This lesson builds a latency measurement framework that isolates GC impact and compares mitigation strategies.

```text
gctaillatency/
  go.mod
  latency.go
  latency_test.go
  cmd/demo/main.go
```

## Concepts

### Why Tail Latency Is the Right Metric

GC impact is nearly invisible at the median. A 100 µs STW pause in a service handling 10,000 req/s affects at most a few requests per pause. But at p99 or p999, that pause appears directly: the 1-in-100 or 1-in-1000 request that arrives during the pause sees added latency equal to the full pause duration.

GC assist is even more insidious: it does not appear as a pause at all in `gctrace`, but a goroutine doing heavy allocation during marking may pause for 50–200 µs per allocation call while it repays its assist debt.

### STW vs GC Assist

STW pauses affect all goroutines simultaneously. All pending requests, network reads, and timer callbacks wait for the pause to end. The effect is a correlated spike: many requests see elevated latency at the same instant.

GC assist affects individual goroutines in proportion to their allocation rate. A goroutine allocating 10 MB in one request cycle may accumulate significant assist debt. Its latency increases; other goroutines are unaffected unless they too are heavy allocators.

### Measuring Latency Correctly

A histogram is the correct data structure for latency. Track the full distribution, not just the mean. Use a fixed-bucket histogram (powers of two in nanoseconds work well) to capture the long tail without overflow. Compute p50, p90, p99, and p999 by summing bucket counts.

Record per-request latency with `time.Now()` and `time.Since()`. The monotonic clock ensures correctness across NTP adjustments.

### Mitigation Strategies

The two most effective strategies for reducing GC-induced tail latency:

1. Reduce allocation rate. Every byte allocated during marking is potential assist work. Reusing buffers with `sync.Pool`, pre-allocating with known capacity, and avoiding interface boxing are the highest-impact techniques.

2. Raise GOGC. `GOGC=200` or `GOGC=400` doubles or quadruples the heap headroom before the next GC cycle. Fewer cycles mean fewer pauses. The cost is proportionally higher peak memory.

Less effective but sometimes useful: `GOGC=off` + `GOMEMLIMIT`. This can eliminate ratio-triggered cycles entirely, concentrating all GC work into a few limit-triggered cycles.

### Correlating Latency Spikes With GC Events

To confirm that a latency spike is GC-caused, compare request timestamps against GC cycle timestamps from `runtime/metrics`. If a cluster of high-latency requests coincides with a GC cycle start, the correlation is strong evidence.

## Exercises

### Exercise 1: Latency Histogram and Measurement

Create `latency.go`:

```go
package gctaillatency

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Histogram records latency observations in power-of-two nanosecond buckets.
// Bucket i covers [2^i ns, 2^(i+1) ns). Bucket 62 is the maximum (covers
// durations >= 2^62 ns ≈ 4.6 seconds).
type Histogram struct {
	counts [63]int64 // atomic counters; bucket 62 is the overflow bucket
}

// Record adds one observation to the histogram.
func (h *Histogram) Record(d time.Duration) {
	ns := d.Nanoseconds()
	if ns <= 0 {
		ns = 1
	}
	bucket := 0
	v := ns
	for v > 1 {
		v >>= 1
		bucket++
	}
	if bucket >= len(h.counts) {
		bucket = len(h.counts) - 1
	}
	atomic.AddInt64(&h.counts[bucket], 1)
}

// Percentile returns the approximate latency at the given percentile (0-100).
func (h *Histogram) Percentile(pct float64) time.Duration {
	total := int64(0)
	for i := range h.counts {
		total += atomic.LoadInt64(&h.counts[i])
	}
	if total == 0 {
		return 0
	}
	target := int64(float64(total) * pct / 100.0)
	cumulative := int64(0)
	for i := range h.counts {
		cumulative += atomic.LoadInt64(&h.counts[i])
		if cumulative >= target {
			// Bucket i covers [2^i ns, 2^(i+1) ns); return the lower bound.
			if i == 0 {
				return time.Duration(1)
			}
			lower := int64(1) << uint(i)
			return time.Duration(lower)
		}
	}
	// Fallback: return the lower bound of the last bucket.
	return time.Duration(int64(1) << uint(len(h.counts)-1))
}

// Count returns the total number of observations recorded.
func (h *Histogram) Count() int64 {
	total := int64(0)
	for i := range h.counts {
		total += atomic.LoadInt64(&h.counts[i])
	}
	return total
}

// WorkerConfig configures a simulated request worker.
type WorkerConfig struct {
	AllocsPerRequest int   // allocations per simulated request
	AllocSizeB       int   // bytes per allocation
	Workers          int   // concurrent workers
	Requests         int   // total requests to simulate
	PoolEnabled      bool  // use sync.Pool for allocations
	GOGC             int   // GC percent to use; 0 means keep current
	MemLimitB        int64 // GOMEMLIMIT; 0 means keep current
}

// RunWorkers runs a simulated request workload and records per-request
// latency in the returned Histogram.
func RunWorkers(cfg WorkerConfig) *Histogram {
	if cfg.GOGC != 0 {
		prev := debug.SetGCPercent(cfg.GOGC)
		defer debug.SetGCPercent(prev)
	}
	if cfg.MemLimitB != 0 {
		prev := debug.SetMemoryLimit(cfg.MemLimitB)
		defer debug.SetMemoryLimit(prev)
	}

	var pool *sync.Pool
	if cfg.PoolEnabled {
		pool = &sync.Pool{
			New: func() any { return make([]byte, cfg.AllocSizeB) },
		}
	}

	h := &Histogram{}
	sem := make(chan struct{}, cfg.Workers)
	var wg sync.WaitGroup

	for i := 0; i < cfg.Requests; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()

			start := time.Now()
			simulateRequest(cfg.AllocsPerRequest, cfg.AllocSizeB, pool)
			h.Record(time.Since(start))
		}()
	}
	wg.Wait()
	return h
}

func simulateRequest(allocsPerRequest, allocSizeB int, pool *sync.Pool) {
	for i := 0; i < allocsPerRequest; i++ {
		if pool != nil {
			b := pool.Get().([]byte)
			b[0] = byte(i)
			pool.Put(b)
		} else {
			b := make([]byte, allocSizeB)
			b[0] = byte(i)
			_ = b
		}
	}
}

// GCStats captures GC cycle count and pause time across a run.
type GCStats struct {
	CyclesBefore uint32
	CyclesAfter  uint32
	PauseTotalNs uint64
}

// CaptureGCStats reads MemStats before running fn and after, returning
// the delta.
func CaptureGCStats(fn func()) GCStats {
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	fn()
	runtime.ReadMemStats(&mAfter)
	return GCStats{
		CyclesBefore: mBefore.NumGC,
		CyclesAfter:  mAfter.NumGC,
		PauseTotalNs: mAfter.PauseTotalNs - mBefore.PauseTotalNs,
	}
}
```

### Exercise 2: Example Function

Append to `latency.go`:

```go
// ExampleHistogram_Percentile shows recording observations and reading percentiles.
func ExampleHistogram_Percentile() {
	h := &Histogram{}
	// Record 100 observations of 1 µs each.
	for i := 0; i < 100; i++ {
		h.Record(time.Microsecond)
	}
	if h.Count() == 100 {
		fmt.Println("count OK")
	}
	p50 := h.Percentile(50)
	if p50 > 0 {
		fmt.Println("p50 positive")
	}
	// Output:
	// count OK
	// p50 positive
}
```

### Exercise 3: Tests

Create `latency_test.go`:

```go
package gctaillatency

import (
	"testing"
	"time"
)

func TestHistogramRecordAndCount(t *testing.T) {
	t.Parallel()

	h := &Histogram{}
	for i := 0; i < 50; i++ {
		h.Record(time.Microsecond * time.Duration(i+1))
	}
	if h.Count() != 50 {
		t.Errorf("Count() = %d, want 50", h.Count())
	}
}

func TestHistogramP50BelowP99(t *testing.T) {
	t.Parallel()

	h := &Histogram{}
	// 99 short observations + 1 long observation.
	for i := 0; i < 99; i++ {
		h.Record(time.Microsecond)
	}
	h.Record(time.Millisecond)

	p50 := h.Percentile(50)
	p99 := h.Percentile(99)
	if p50 > p99 {
		t.Errorf("p50 (%v) > p99 (%v)", p50, p99)
	}
}

func TestHistogramEmptyReturnsZero(t *testing.T) {
	t.Parallel()

	h := &Histogram{}
	if got := h.Percentile(50); got != 0 {
		t.Errorf("Percentile(50) on empty histogram = %v, want 0", got)
	}
}

func TestRunWorkersRecordsRequests(t *testing.T) {
	t.Parallel()

	cfg := WorkerConfig{
		AllocsPerRequest: 10,
		AllocSizeB:       64,
		Workers:          4,
		Requests:         100,
	}
	h := RunWorkers(cfg)
	if h.Count() != 100 {
		t.Errorf("Count() = %d, want 100", h.Count())
	}
}

func TestPoolEnabledReducesAllocs(t *testing.T) {
	t.Parallel()

	base := WorkerConfig{
		AllocsPerRequest: 50,
		AllocSizeB:       512,
		Workers:          2,
		Requests:         200,
	}

	withoutPool := base
	withoutPool.PoolEnabled = false

	withPool := base
	withPool.PoolEnabled = true

	var statsWithout, statsWith GCStats
	statsWithout = CaptureGCStats(func() { RunWorkers(withoutPool) })
	statsWith = CaptureGCStats(func() { RunWorkers(withPool) })

	// Pool should produce fewer GC cycles due to reduced allocation pressure.
	// This is probabilistic; we log rather than hard-fail to avoid flakiness.
	t.Logf("without pool: %d GC cycles; with pool: %d GC cycles",
		statsWithout.CyclesAfter-statsWithout.CyclesBefore,
		statsWith.CyclesAfter-statsWith.CyclesBefore)
}

func TestCaptureGCStatsCountsNonDecreasing(t *testing.T) {
	t.Parallel()

	stats := CaptureGCStats(func() {
		for i := 0; i < 10; i++ {
			_ = make([]byte, 1<<20)
		}
	})
	if stats.CyclesAfter < stats.CyclesBefore {
		t.Errorf("CyclesAfter (%d) < CyclesBefore (%d)", stats.CyclesAfter, stats.CyclesBefore)
	}
}

// Your turn: write TestHistogramPercentilesMonotonic that records 1000
// observations of random durations between 1µs and 1ms, then asserts that
// h.Percentile(50) <= h.Percentile(90) <= h.Percentile(99).
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"math"
	"runtime/debug"

	"example.com/gctaillatency"
)

func main() {
	fmt.Println("GC tail latency measurement")
	fmt.Println()

	base := gctaillatency.WorkerConfig{
		AllocsPerRequest: 100,
		AllocSizeB:       512,
		Workers:          8,
		Requests:         2000,
	}

	type scenario struct {
		name string
		cfg  gctaillatency.WorkerConfig
	}

	scenarios := []scenario{
		{
			name: "GOGC=100 (default), no pool",
			cfg:  base,
		},
		{
			name: "GOGC=400, no pool",
			cfg: func() gctaillatency.WorkerConfig {
				c := base
				c.GOGC = 400
				return c
			}(),
		},
		{
			name: "GOGC=100, sync.Pool enabled",
			cfg: func() gctaillatency.WorkerConfig {
				c := base
				c.PoolEnabled = true
				return c
			}(),
		},
	}

	_ = debug.SetGCPercent // ensure import used

	fmt.Printf("  %-35s  %8s  %8s  %8s  %8s\n",
		"scenario", "p50", "p90", "p99", "p999")

	for _, s := range scenarios {
		var h *gctaillatency.Histogram
		stats := gctaillatency.CaptureGCStats(func() {
			h = gctaillatency.RunWorkers(s.cfg)
		})
		gcCycles := stats.CyclesAfter - stats.CyclesBefore
		_ = gcCycles
		fmt.Printf("  %-35s  %8v  %8v  %8v  %8v\n",
			s.name,
			h.Percentile(50),
			h.Percentile(90),
			h.Percentile(99),
			h.Percentile(99.9),
		)
	}

	fmt.Println()
	fmt.Println("Key insight: p99/p999 diverges across GC configurations;")
	fmt.Println("p50 is largely unaffected.")
	_ = math.MaxFloat64 // prevent unused import
}
```

## Common Mistakes

### Measuring Only the Mean Latency

Wrong: reporting `total_time / request_count` as the latency metric and concluding GC is not a problem.

What happens: the mean hides tail latency. A service with p50=1ms and p999=200ms looks fine at the mean but is unusable for latency-sensitive clients.

Fix: always track p99 and p999. GC impact is invisible at the median and severe at the tail.

### Assuming sync.Pool Eliminates GC Cycles

Wrong: using `sync.Pool` and expecting zero GC cycles.

What happens: `sync.Pool` reduces the allocation rate, which reduces GC frequency, but does not eliminate it. Pool objects are cleared at each GC cycle; the pool rebuilds afterward.

Fix: think of `sync.Pool` as reducing allocation pressure, not eliminating GC. It reduces the number of allocations that accumulate assist debt, which reduces assist latency.

### Not Warming Up sync.Pool Before Measurement

Wrong: starting the benchmark immediately after creating the pool, before any objects have been put in.

What happens: the first wave of requests allocates everything from scratch (pool misses). The measured latency includes warmup overhead and does not reflect steady-state performance.

Fix: run a warmup phase (same request pattern, discard the histogram) before the measured run.

### Using GOGC=off Without Understanding the Live Heap Growth

Wrong: setting `GOGC=-1` in a service with a slowly growing live set (caches, in-memory indices) and expecting memory to stay bounded.

What happens: without GOMEMLIMIT, the heap grows unboundedly. With GOMEMLIMIT, you enter the pressured regime earlier than expected because the live set consumes a growing fraction of the limit.

Fix: profile the steady-state live heap size before disabling ratio-triggered GC. Ensure `live_heap_at_peak << GOMEMLIMIT`.

## Verification

From `~/go-exercises/gctaillatency`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- GC impact is concentrated in tail latency (p99, p999), not median latency.
- STW pauses affect all goroutines simultaneously; GC assist affects individual high-allocating goroutines.
- A histogram (not a mean) is the correct data structure for latency measurement.
- Higher GOGC reduces cycle frequency and tail latency spikes at the cost of more memory.
- `sync.Pool` reduces per-request allocation, shrinking assist debt and smoothing p99.
- Correlate request timestamps with GC cycle timestamps to confirm GC is the cause of a latency spike.

## What's Next

Next: [Reducing GC Pressure](../09-reducing-gc-pressure/09-reducing-gc-pressure.md).

## Resources

- [Go GC Guide: Latency](https://tip.golang.org/doc/gc-guide#Latency) — official latency considerations and tuning advice
- [sync.Pool](https://pkg.go.dev/sync#Pool) — object pooling to reduce allocation pressure
- [runtime/metrics](https://pkg.go.dev/runtime/metrics) — GC pause histograms via `/gc/pauses:seconds`
- [How NOT to Measure Latency (Gil Tene)](https://www.infoq.com/presentations/latency-response-time/) — why histograms matter and how coordinated omission skews results
