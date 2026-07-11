# Exercise 10: Build a Thread-Safe Metrics System

You have met Go's synchronization primitives one at a time; real systems force you to choose between them by access pattern, and choosing wrong is not a crash but a quiet tax -- an `RWMutex` on a write-heavy counter is slower than a plain `Mutex`, and an `atomic` where you needed a `Mutex` corrupts compound state. A production metrics system carries several counter shapes at once: a write-heavy request total, a read-heavy connections gauge, a high-frequency bytes counter. This exercise makes you implement all three with `sync.Mutex`, `sync.RWMutex`, and `sync/atomic`, benchmark them under realistic load, and assemble them into one registry that uses the right tool for each.

## What you'll build

```text
build-thread-safe-counter/
  main.go        a Mutex counter, an RWMutex gauge, an atomic counter, a
                 benchmark of all three under write- and read-heavy load, and
                 a complete metrics registry that picks a primitive per metric
```

- Build: a metrics registry composed of counter types, each backed by the primitive its access pattern favors.
- Implement: `MutexCounter`, `RWGauge`, and `AtomicCounter`; `runWriteHeavyBench` / `runReadHeavyBench`; and a `MetricsRegistry` pairing a `RequestCounter` (atomic), `ConnectionGauge` (RWMutex), and `LatencyHistogram` (mutex).
- Verify: `go run -race main.go` at each step -- correct final values and zero race warnings.

### Why the access pattern picks the primitive

Throughout this section you have learned individual sync primitives in isolation. Real-world systems require choosing between them based on the specific access pattern. A production metrics system has multiple counter types:

- **Simple counters** (total requests): write-heavy, rarely read. Every request increments.
- **Gauges** (active connections): read frequently by monitoring dashboards, written less often when connections open/close.
- **High-frequency counters** (bytes transferred): incremented on every packet, read only for periodic reporting.

Each counter type has a different read/write ratio, which determines the optimal sync primitive. This exercise forces you to implement all three, benchmark them under realistic conditions, and reason about when each approach is appropriate.

## Step 1 -- Simple Counter with Mutex

A request counter: every handler increments it, and the `/metrics` endpoint reads it. Writes heavily outnumber reads:

```go
package main

import (
	"fmt"
	"sync"
)

const incrementGoroutines = 1000

type MutexCounter struct {
	mu    sync.Mutex
	value int64
}

func (c *MutexCounter) Increment() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value++
}

func (c *MutexCounter) Add(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value += n
}

func (c *MutexCounter) Value() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value
}

func incrementConcurrently(counter *MutexCounter, goroutines int) {
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter.Increment()
		}()
	}

	wg.Wait()
}

func main() {
	counter := &MutexCounter{}
	incrementConcurrently(counter, incrementGoroutines)
	fmt.Printf("MutexCounter: %d (expected %d)\n", counter.Value(), incrementGoroutines)
}
```

Expected output:
```
MutexCounter: 1000 (expected 1000)
```

**Characteristics**: simple, correct, all operations serialized (including reads). Good default choice.

### Verification
```bash
go run -race main.go
```
The counter should report exactly 1000 with no race warnings.

## Step 2 -- Gauge with RWMutex (Concurrent Readers)

An active connections gauge: monitoring dashboards read it constantly, but only connection open/close events update it:

```go
package main

import (
	"fmt"
	"sync"
	"time"
)

const (
	simulatedConnections   = 10
	monitoringReaders      = 50
	connectionHoldDuration = 50 * time.Millisecond
)

type RWGauge struct {
	mu    sync.RWMutex
	value int64
}

func (g *RWGauge) Set(val int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value = val
}

func (g *RWGauge) Increment() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value++
}

func (g *RWGauge) Decrement() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.value--
}

func (g *RWGauge) Value() int64 {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.value
}

func simulateConnectionLifecycle(gauge *RWGauge, wg *sync.WaitGroup) {
	defer wg.Done()
	gauge.Increment()
	time.Sleep(connectionHoldDuration)
	gauge.Decrement()
}

func simulateMonitoringReads(gauge *RWGauge, readerCount int, wg *sync.WaitGroup) {
	for i := 0; i < readerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = gauge.Value()
		}()
	}
}

func main() {
	gauge := &RWGauge{}
	var wg sync.WaitGroup

	for i := 0; i < simulatedConnections; i++ {
		wg.Add(1)
		go simulateConnectionLifecycle(gauge, &wg)
	}

	simulateMonitoringReads(gauge, monitoringReaders, &wg)

	wg.Wait()
	fmt.Printf("RWGauge (active connections): %d (expected 0 -- all closed)\n", gauge.Value())
}
```

Expected output:
```
RWGauge (active connections): 0 (expected 0 -- all closed)
```

**Characteristics**: concurrent readers do not block each other. Writers get exclusive access. Optimal when reads significantly outnumber writes, like a Prometheus `/metrics` endpoint scraped every 15 seconds while connections change only a few times per second.

### Verification
```bash
go run -race main.go
```
Gauge should be 0 (all connections opened and closed).

## Step 3 -- High-Frequency Counter with Atomic

A bytes-transferred counter: incremented on every network packet (potentially millions of times per second), read only for periodic reporting:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
)

const (
	writerGoroutines     = 100
	incrementsPerWorker  = 10000
	expectedTotal        = writerGoroutines * incrementsPerWorker
)

type AtomicCounter struct {
	value atomic.Int64
}

func (c *AtomicCounter) Increment() {
	c.value.Add(1)
}

func (c *AtomicCounter) Add(n int64) {
	c.value.Add(n)
}

func (c *AtomicCounter) Value() int64 {
	return c.value.Load()
}

func runHighFrequencyIncrements(counter *AtomicCounter, workers, opsPerWorker int) {
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerWorker; j++ {
				counter.Increment()
			}
		}()
	}

	wg.Wait()
}

func main() {
	counter := &AtomicCounter{}
	runHighFrequencyIncrements(counter, writerGoroutines, incrementsPerWorker)
	fmt.Printf("AtomicCounter: %d (expected %d)\n", counter.Value(), expectedTotal)
}
```

Expected output:
```
AtomicCounter: 1000000 (expected 1000000)
```

**Characteristics**: lock-free, highest throughput. No deadlock possible. Limited to operations the CPU supports atomically (add, load, store, compare-and-swap). Cannot protect complex operations.

### Verification
```bash
go run -race main.go
```
Exactly 1,000,000 with no race warnings.

## Step 4 -- Benchmark All Three Under Realistic Workloads

Run all three counter types under identical conditions and measure throughput:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	benchGoroutines   = 100
	benchOpsPerG      = 10000
	readSampleRate    = 100 // read once per N writes
	benchWriterCount  = 2
	benchWriteRatio   = 10
)

type MutexCounter struct {
	mu    sync.Mutex
	value int64
}

func (c *MutexCounter) Increment() { c.mu.Lock(); c.value++; c.mu.Unlock() }
func (c *MutexCounter) Value() int64 { c.mu.Lock(); defer c.mu.Unlock(); return c.value }

type RWGauge struct {
	mu    sync.RWMutex
	value int64
}

func (g *RWGauge) Increment() { g.mu.Lock(); g.value++; g.mu.Unlock() }
func (g *RWGauge) Value() int64 { g.mu.RLock(); defer g.mu.RUnlock(); return g.value }

type AtomicCounter struct {
	value atomic.Int64
}

func (c *AtomicCounter) Increment() { c.value.Add(1) }
func (c *AtomicCounter) Value() int64 { return c.value.Load() }

type CounterOps struct {
	Name string
	Inc  func()
	Val  func() int64
}

func runWriteHeavyBench(ops CounterOps, goroutines, opsPerG int) {
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerG; j++ {
				ops.Inc()
				if j%readSampleRate == 0 {
					_ = ops.Val()
				}
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)
	fmt.Printf("  %-15s %v  (final value: %d)\n", ops.Name, elapsed.Round(time.Millisecond), ops.Val())
}

func runReadHeavyBench(ops CounterOps, readers, opsPerG int) {
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < benchWriterCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerG/benchWriteRatio; j++ {
				ops.Inc()
			}
		}()
	}

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < opsPerG; j++ {
				_ = ops.Val()
			}
		}()
	}

	wg.Wait()
	elapsed := time.Since(start)
	fmt.Printf("  %-15s %v  (final value: %d)\n", ops.Name, elapsed.Round(time.Millisecond), ops.Val())
}

func main() {
	fmt.Printf("=== Write-Heavy Benchmark (%d goroutines x %d ops) ===\n", benchGoroutines, benchOpsPerG)
	fmt.Println("Scenario: request counter (every handler writes, metrics endpoint reads rarely)")

	mc := &MutexCounter{}
	rg := &RWGauge{}
	ac := &AtomicCounter{}

	writeHeavy := []CounterOps{
		{"Mutex", mc.Increment, mc.Value},
		{"RWMutex", rg.Increment, rg.Value},
		{"Atomic", ac.Increment, ac.Value},
	}
	for _, ops := range writeHeavy {
		runWriteHeavyBench(ops, benchGoroutines, benchOpsPerG)
	}

	fmt.Printf("\n=== Read-Heavy Benchmark (%d writers, %d readers x %d ops) ===\n", benchWriterCount, benchGoroutines, benchOpsPerG)
	fmt.Println("Scenario: active connections gauge (dashboard reads constantly, few changes)")

	mc2 := &MutexCounter{}
	rg2 := &RWGauge{}
	ac2 := &AtomicCounter{}

	readHeavy := []CounterOps{
		{"Mutex", mc2.Increment, mc2.Value},
		{"RWMutex", rg2.Increment, rg2.Value},
		{"Atomic", ac2.Increment, ac2.Value},
	}
	for _, ops := range readHeavy {
		runReadHeavyBench(ops, benchGoroutines, benchOpsPerG)
	}

	fmt.Println("\n=== Recommendation ===")
	fmt.Println("  Request counters (write-heavy):   atomic > mutex > rwmutex")
	fmt.Println("  Gauges (read-heavy):              atomic > rwmutex > mutex")
	fmt.Println("  Complex state (multi-field):      mutex (atomic cannot protect compound ops)")
}
```

Expected output (times vary by machine):
```
=== Write-Heavy Benchmark (100 goroutines x 10000 ops) ===
Scenario: request counter (every handler writes, metrics endpoint reads rarely)
  Mutex            15ms  (final value: 1000000)
  RWMutex          20ms  (final value: 1000000)
  Atomic           3ms   (final value: 1000000)

=== Read-Heavy Benchmark (2 writers, 100 readers x 10000 ops) ===
Scenario: active connections gauge (dashboard reads constantly, few changes)
  Mutex            25ms  (final value: 2000)
  RWMutex          10ms  (final value: 2000)
  Atomic           4ms   (final value: 2000)

=== Recommendation ===
  Request counters (write-heavy):   atomic > mutex > rwmutex
  Gauges (read-heavy):              atomic > rwmutex > mutex
  Complex state (multi-field):      mutex (atomic cannot protect compound ops)
```

### Verification
```bash
go run main.go
```
All counters should report correct final values. Atomic should be fastest in both scenarios.

## Step 5 -- Complete Metrics Registry

Put it all together: a production-grade metrics registry that chooses the right primitive for each counter type:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	simulatedRequests  = 200
	dashboardReaders   = 20
	baseLatencyMs      = 5
	latencyRangeMs     = 50
	latencyScaleFactor = 10
	dashboardDelay     = 10 * time.Millisecond
	maxInt64           = int64(1<<63 - 1)
)

// RequestCounter uses atomic for high-throughput write-heavy counters.
type RequestCounter struct {
	value atomic.Int64
}

func (c *RequestCounter) Inc()         { c.value.Add(1) }
func (c *RequestCounter) Add(n int64)  { c.value.Add(n) }
func (c *RequestCounter) Value() int64 { return c.value.Load() }

// ConnectionGauge uses RWMutex for read-heavy gauges.
type ConnectionGauge struct {
	mu    sync.RWMutex
	value int64
}

func (g *ConnectionGauge) Inc()         { g.mu.Lock(); g.value++; g.mu.Unlock() }
func (g *ConnectionGauge) Dec()         { g.mu.Lock(); g.value--; g.mu.Unlock() }
func (g *ConnectionGauge) Value() int64 { g.mu.RLock(); defer g.mu.RUnlock(); return g.value }

// LatencyHistogram uses Mutex for complex multi-field state.
type LatencyHistogram struct {
	mu    sync.Mutex
	count int64
	sum   int64
	min   int64
	max   int64
}

func NewLatencyHistogram() *LatencyHistogram {
	return &LatencyHistogram{min: maxInt64}
}

func (h *LatencyHistogram) Record(latencyMs int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.count++
	h.sum += latencyMs
	if latencyMs < h.min {
		h.min = latencyMs
	}
	if latencyMs > h.max {
		h.max = latencyMs
	}
}

func (h *LatencyHistogram) Stats() (count, avg, min, max int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.count == 0 {
		return 0, 0, 0, 0
	}
	return h.count, h.sum / h.count, h.min, h.max
}

// MetricsRegistry groups all metric types with their optimal sync primitives.
type MetricsRegistry struct {
	Requests    *RequestCounter
	Connections *ConnectionGauge
	Latency     *LatencyHistogram
}

func NewMetricsRegistry() *MetricsRegistry {
	return &MetricsRegistry{
		Requests:    &RequestCounter{},
		Connections: &ConnectionGauge{},
		Latency:     NewLatencyHistogram(),
	}
}

func simulateHTTPRequest(registry *MetricsRegistry, reqID int) {
	registry.Connections.Inc()
	registry.Requests.Inc()

	latencyMs := int64(baseLatencyMs + reqID%latencyRangeMs)
	time.Sleep(time.Duration(latencyMs) * time.Millisecond / latencyScaleFactor)
	registry.Latency.Record(latencyMs)

	registry.Connections.Dec()
}

func simulateDashboardScrapes(registry *MetricsRegistry, scrapeCount int, wg *sync.WaitGroup) {
	for i := 0; i < scrapeCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(dashboardDelay)
			_ = registry.Requests.Value()
			_ = registry.Connections.Value()
		}()
	}
}

func runProductionSimulation(registry *MetricsRegistry) {
	var wg sync.WaitGroup

	for i := 0; i < simulatedRequests; i++ {
		wg.Add(1)
		go func(reqID int) {
			defer wg.Done()
			simulateHTTPRequest(registry, reqID)
		}(i)
	}

	simulateDashboardScrapes(registry, dashboardReaders, &wg)

	wg.Wait()
}

func printMetricsReport(registry *MetricsRegistry) {
	count, avg, min, max := registry.Latency.Stats()

	fmt.Println("=== Production Metrics Report ===")
	fmt.Printf("  Total requests:      %d\n", registry.Requests.Value())
	fmt.Printf("  Active connections:   %d (should be 0)\n", registry.Connections.Value())
	fmt.Printf("  Latency (ms):        count=%d avg=%d min=%d max=%d\n", count, avg, min, max)
	fmt.Println("\nPrimitive choices:")
	fmt.Println("  RequestCounter:      atomic (write-heavy, simple increment)")
	fmt.Println("  ConnectionGauge:     RWMutex (read-heavy, concurrent dashboard readers)")
	fmt.Println("  LatencyHistogram:    Mutex (must update count+sum+min+max atomically)")
}

func main() {
	registry := NewMetricsRegistry()
	runProductionSimulation(registry)
	printMetricsReport(registry)
}
```

Expected output:
```
=== Production Metrics Report ===
  Total requests:      200
  Active connections:   0 (should be 0)
  Latency (ms):        count=200 avg=29 min=5 max=54

Primitive choices:
  RequestCounter:      atomic (write-heavy, simple increment)
  ConnectionGauge:     RWMutex (read-heavy, concurrent dashboard readers)
  LatencyHistogram:    Mutex (must update count+sum+min+max atomically)
```

### Verification
```bash
go run -race main.go
```
All metrics correct, zero active connections, no race warnings.

## Common Mistakes

### Using Atomic for Complex Operations

```go
var balance atomic.Int64
func transfer(amount int64) {
    if balance.Load() >= amount { // check
        balance.Add(-amount)      // act -- NOT atomic with the check!
    }
}
```

**What happens:** The check-then-act is not atomic. Another goroutine can drain the balance between Load and Add. This is why the `LatencyHistogram` uses a Mutex -- updating count, sum, min, and max must happen as one atomic unit.

**Fix:** Use `CompareAndSwap` in a loop, or switch to a mutex for compound operations.

### RWMutex for Write-Heavy Counters
Using `RWMutex` for a request counter (mostly writes) adds overhead for read-lock tracking with no benefit. The write-heavy benchmark proves this: `RWMutex` is slower than plain `Mutex` when writes dominate.

### Forgetting to Choose Based on Access Pattern
The default should be:
1. Simple increment/read? -> `atomic`
2. Read-heavy with concurrent readers? -> `RWMutex`
3. Multi-field update that must be atomic? -> `Mutex`

Do not use `atomic` for everything (it cannot protect compound operations). Do not use `RWMutex` for everything (it is slower than `Mutex` for write-heavy workloads).

## Review

The exercise turns one decision into a habit: pick the primitive from the access pattern, never by default. Three axes decide it -- operation complexity, read/write ratio, and throughput. `sync/atomic` is lock-free and fastest, so it wins for a simple counter that only increments and loads, but it can protect only what the CPU does atomically and so cannot guard a compound update. `sync.RWMutex` lets readers proceed in parallel, which pays off exactly when reads dominate (a connections gauge, a config map, a cache), but its per-operation bookkeeping makes it slower than a plain `Mutex` when writes are frequent -- the write-heavy benchmark proves it. `sync.Mutex` is the honest default and the right choice for multi-field state like the latency histogram, where count, sum, min, and max must move as one atomic step. The benchmarks matter because intuition about these costs is routinely wrong; measuring under your actual read/write mix is the only reliable guide.

To confirm the reasoning transfers, extend the registry. Add an `ErrorRate` that tracks error count and total count and reports `errors/total` -- and notice that reading two atomics separately does not give you a consistent ratio, which tells you what primitive you actually need for a coherent snapshot. Add a `ResponseSizeHistogram` computing p50/p95/p99 from a sorted slice under a mutex, since a slice is compound state atomics cannot touch. Then benchmark all five and write the one-paragraph recommendation for which to use when. If you can justify each choice from the three axes without running the code first, the skill has generalized.

## Resources

- [sync package documentation](https://pkg.go.dev/sync) -- `Mutex` and `RWMutex`, including the note that `RWMutex` favors readers but costs more per call.
- [sync/atomic package documentation](https://pkg.go.dev/sync/atomic) -- the typed atomics and the exact set of operations the hardware guarantees.
- [Go Blog: Share Memory by Communicating](https://go.dev/blog/codelab-share) -- the wider guidance on when to reach for locks versus channels.
- [Go Wiki: MutexOrChannel](https://go.dev/wiki/MutexOrChannel) -- a short decision guide for choosing between a mutex and a channel for shared state.

---

Back to [Concurrency](../../concurrency.md) | Next: [11-waitgroup-patterns](../11-waitgroup-patterns/11-waitgroup-patterns.md)
