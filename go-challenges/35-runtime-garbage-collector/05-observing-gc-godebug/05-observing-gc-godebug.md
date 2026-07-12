# 5. Observing GC with GODEBUG and runtime/metrics

You cannot tune what you cannot measure. Go provides three complementary observability layers: `GODEBUG=gctrace=1` for per-cycle human-readable output, `runtime.ReadMemStats` for programmatic snapshots, and `runtime/metrics` for structured, STW-free metric queries. This lesson builds a monitoring toolkit and teaches you to read `gctrace` output field by field.

```text
gcobserve/
  go.mod
  monitor.go
  monitor_test.go
  cmd/demo/main.go
```

## Concepts

### GODEBUG=gctrace=1

Set this environment variable before running any Go program to receive one line per GC cycle on stderr. The format is:

```
gc N @Xs Y%: A+B+C ms clock, D+E/F/G+H ms cpu, I->J->K MB, L MB goal, M MB stacks, N MB globals, P P
```

- `N` — GC cycle number (monotonically increasing).
- `@Xs` — seconds since program start when this cycle began.
- `Y%` — percentage of total CPU time spent in GC since program start.
- `A+B+C ms clock` — wall-clock time split: A = sweep-termination STW, B = concurrent mark, C = mark-termination STW.
- `D+E/F/G+H ms cpu` — CPU time: D = GC assists, E+F+G = background mark workers (dedicated/fractional/idle), H = mark termination.
- `I->J->K MB` — heap size before GC, after GC, and live heap (post-collection).
- `L MB goal` — the heap target for the next cycle.
- `P P` — GOMAXPROCS.

The two STW pauses are A and C. In modern Go (1.14+) they are almost always under 200 µs for real-world workloads.

### runtime.ReadMemStats

`runtime.ReadMemStats(&m)` fills a `runtime.MemStats` with a consistent snapshot. It incurs a brief STW to ensure consistency, so do not call it in hot paths. Key fields:

- `m.NumGC` — total GC cycles completed.
- `m.PauseNs[(m.NumGC+255)%256]` — last STW pause duration in nanoseconds.
- `m.PauseTotalNs` — cumulative STW pause time.
- `m.GCCPUFraction` — fraction of CPU time spent in GC since program start.
- `m.HeapAlloc` — bytes of live heap objects.
- `m.NextGC` — heap size at which next GC will be triggered.
- `m.HeapObjects` — number of live heap objects.

### runtime/metrics

`runtime/metrics` (Go 1.16+) is the modern API. It does not stop the world. You declare which metrics you want, call `metrics.Read`, and read the values. Key GC metrics:

- `/gc/cycles/total:gc-cycles` — total GC cycles (uint64).
- `/gc/heap/allocs:bytes` — cumulative bytes allocated to the heap.
- `/gc/heap/goal:bytes` — current heap target (uint64).
- `/gc/heap/live:bytes` — live heap after last GC (uint64).
- `/gc/pauses:seconds` — histogram of GC pause durations.
- `/cpu/classes/gc/total:cpu-seconds` — cumulative CPU time spent in GC; divide by `/cpu/classes/total:cpu-seconds` to compute the GC CPU fraction.

The histogram-valued metrics (`/gc/pauses:seconds`) let you compute p99 pause latency without needing `gctrace` parsing.

### Monitoring Strategy

For production services:
- Export `/gc/cycles/total:gc-cycles`, `/cpu/classes/gc/total:cpu-seconds`, and `/gc/heap/goal:bytes` to your metrics system on a 15–30 second scrape interval.
- Alert on `GCCPUFraction > 0.05` (GC consuming more than 5% of CPU) and on `HeapAlloc > 0.8 * NextGC` as a leading indicator of an impending cycle.
- Use `gctrace=1` transiently (with stderr redirected to a log file) to diagnose specific pause spikes.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/35-runtime-garbage-collector/05-observing-gc-godebug/05-observing-gc-godebug/cmd/demo
cd go-solutions/35-runtime-garbage-collector/05-observing-gc-godebug/05-observing-gc-godebug
```

### Exercise 1: GC Monitor Using MemStats

Create `monitor.go`:

```go
package gcobserve

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

// Snapshot holds GC metrics captured at one point in time.
type Snapshot struct {
	Timestamp   time.Time
	NumGC       uint32
	LastPauseNs uint64
	HeapAllocB  uint64
	HeapInuseB  uint64
	HeapObjects uint64
	GCCPUFrac   float64
	NextGCB     uint64
}

// Monitor captures GC snapshots on a fixed interval using ReadMemStats.
// It is not safe to call Start more than once without first calling Stop.
type Monitor struct {
	mu        sync.Mutex
	snapshots []Snapshot
	done      chan struct{}
}

// NewMonitor creates a Monitor ready to start.
func NewMonitor() *Monitor {
	return &Monitor{done: make(chan struct{})}
}

// Start begins capturing snapshots every interval until Stop is called.
func (mon *Monitor) Start(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				mon.capture()
			case <-mon.done:
				return
			}
		}
	}()
}

func (mon *Monitor) capture() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s := Snapshot{
		Timestamp:   time.Now(),
		NumGC:       m.NumGC,
		LastPauseNs: m.PauseNs[(m.NumGC+255)%256],
		HeapAllocB:  m.HeapAlloc,
		HeapInuseB:  m.HeapInuse,
		HeapObjects: m.HeapObjects,
		GCCPUFrac:   m.GCCPUFraction,
		NextGCB:     m.NextGC,
	}
	mon.mu.Lock()
	mon.snapshots = append(mon.snapshots, s)
	mon.mu.Unlock()
}

// Stop ends snapshot capture and returns all snapshots taken.
func (mon *Monitor) Stop() []Snapshot {
	close(mon.done)
	mon.mu.Lock()
	defer mon.mu.Unlock()
	out := make([]Snapshot, len(mon.snapshots))
	copy(out, mon.snapshots)
	return out
}

// MetricSample reads a set of runtime/metrics values and returns them as a map.
// Keys are the metric names; values are the raw uint64 or float64 (histograms
// are represented as the count of their buckets for simplicity).
func MetricSample(names []string) map[string]float64 {
	samples := make([]metrics.Sample, len(names))
	for i, n := range names {
		samples[i].Name = n
	}
	metrics.Read(samples)

	out := make(map[string]float64, len(samples))
	for _, s := range samples {
		switch s.Value.Kind() {
		case metrics.KindUint64:
			out[s.Name] = float64(s.Value.Uint64())
		case metrics.KindFloat64:
			out[s.Name] = s.Value.Float64()
		case metrics.KindFloat64Histogram:
			h := s.Value.Float64Histogram()
			out[s.Name] = float64(len(h.Buckets))
		}
	}
	return out
}

// SnapshotStats computes summary statistics over a slice of Snapshots.
type SnapshotStats struct {
	TotalGCCycles  uint32
	MaxPauseNs     uint64
	AvgGCCPUFrac   float64
	MaxHeapObjects uint64
}

// Summarise computes summary statistics over the given snapshots.
func Summarise(snaps []Snapshot) SnapshotStats {
	if len(snaps) == 0 {
		return SnapshotStats{}
	}
	var stats SnapshotStats
	var totalFrac float64
	first := snaps[0].NumGC
	for _, s := range snaps {
		if s.LastPauseNs > stats.MaxPauseNs {
			stats.MaxPauseNs = s.LastPauseNs
		}
		totalFrac += s.GCCPUFrac
		if s.HeapObjects > stats.MaxHeapObjects {
			stats.MaxHeapObjects = s.HeapObjects
		}
	}
	stats.TotalGCCycles = snaps[len(snaps)-1].NumGC - first
	stats.AvgGCCPUFrac = totalFrac / float64(len(snaps))
	return stats
}
```

### Exercise 2: Example Function

Append to `monitor.go`:

```go
// ExampleMetricSample shows reading GC cycle count via runtime/metrics.
func ExampleMetricSample() {
	m := MetricSample([]string{"/gc/cycles/total:gc-cycles"})
	if m["/gc/cycles/total:gc-cycles"] >= 0 {
		fmt.Println("metric read OK")
	}
	// Output:
	// metric read OK
}
```

### Exercise 3: Tests

Create `monitor_test.go`:

```go
package gcobserve

import (
	"runtime"
	"testing"
	"time"
)

func TestMonitorCapturesSnapshots(t *testing.T) {
	t.Parallel()

	mon := NewMonitor()
	mon.Start(5 * time.Millisecond)

	// Generate some GC cycles.
	for i := 0; i < 5; i++ {
		_ = make([]byte, 1<<20) // 1 MiB
		runtime.GC()
	}
	time.Sleep(30 * time.Millisecond)

	snaps := mon.Stop()
	if len(snaps) == 0 {
		t.Fatal("expected at least one snapshot")
	}
}

func TestMonitorSnapshotNumGCIsNonDecreasing(t *testing.T) {
	t.Parallel()

	mon := NewMonitor()
	mon.Start(5 * time.Millisecond)

	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	time.Sleep(30 * time.Millisecond)

	snaps := mon.Stop()
	for i := 1; i < len(snaps); i++ {
		if snaps[i].NumGC < snaps[i-1].NumGC {
			t.Errorf("NumGC decreased at snapshot %d: %d < %d",
				i, snaps[i].NumGC, snaps[i-1].NumGC)
		}
	}
}

func TestMetricSampleGCCycles(t *testing.T) {
	t.Parallel()

	runtime.GC()
	m := MetricSample([]string{"/gc/cycles/total:gc-cycles"})
	v, ok := m["/gc/cycles/total:gc-cycles"]
	if !ok {
		t.Fatal("metric /gc/cycles/total:gc-cycles not returned")
	}
	if v <= 0 {
		t.Errorf("gc cycles = %v, want > 0", v)
	}
}

func TestMetricSampleMultipleKeys(t *testing.T) {
	t.Parallel()

	keys := []string{
		"/gc/cycles/total:gc-cycles",
		"/gc/heap/goal:bytes",
	}
	m := MetricSample(keys)
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q missing from MetricSample result", k)
		}
	}
}

func TestSummariseEmptySlice(t *testing.T) {
	t.Parallel()

	stats := Summarise(nil)
	if stats.TotalGCCycles != 0 || stats.MaxPauseNs != 0 {
		t.Errorf("Summarise(nil) = %+v, want zero struct", stats)
	}
}

func TestSummariseComputesMaxPause(t *testing.T) {
	t.Parallel()

	snaps := []Snapshot{
		{NumGC: 1, LastPauseNs: 500},
		{NumGC: 2, LastPauseNs: 1200},
		{NumGC: 3, LastPauseNs: 800},
	}
	stats := Summarise(snaps)
	if stats.MaxPauseNs != 1200 {
		t.Errorf("MaxPauseNs = %d, want 1200", stats.MaxPauseNs)
	}
	if stats.TotalGCCycles != 2 {
		t.Errorf("TotalGCCycles = %d, want 2", stats.TotalGCCycles)
	}
}

// Your turn: write TestSummariseAvgGCCPUFrac that creates three Snapshots
// with GCCPUFrac of 0.01, 0.03, 0.02 respectively, calls Summarise, and
// asserts that AvgGCCPUFrac is within 0.001 of 0.02.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"
	"time"

	"example.com/gcobserve"
)

func main() {
	fmt.Println("GC observation demo")
	fmt.Println("Run with: GODEBUG=gctrace=1 go run ./cmd/demo 2>gc.log")
	fmt.Println()

	mon := gcobserve.NewMonitor()
	mon.Start(10 * time.Millisecond)

	// Three allocation phases with different intensities.
	phases := []struct {
		name  string
		n     int
		sizeB int
	}{
		{"light", 5_000, 64},
		{"heavy", 50_000, 1024},
		{"burst", 20_000, 4096},
	}

	for _, p := range phases {
		fmt.Printf("Phase: %-6s  (%d x %d B)\n", p.name, p.n, p.sizeB)
		for i := 0; i < p.n; i++ {
			_ = make([]byte, p.sizeB)
		}
		runtime.GC()
	}

	snaps := mon.Stop()
	stats := gcobserve.Summarise(snaps)

	fmt.Println()
	fmt.Printf("Snapshots captured:  %d\n", len(snaps))
	fmt.Printf("GC cycles observed:  %d\n", stats.TotalGCCycles)
	fmt.Printf("Max STW pause:       %v\n", time.Duration(stats.MaxPauseNs))
	fmt.Printf("Avg GC CPU frac:     %.4f\n", stats.AvgGCCPUFrac)
	fmt.Printf("Peak heap objects:   %d\n", stats.MaxHeapObjects)

	fmt.Println()
	fmt.Println("runtime/metrics sample:")
	m := gcobserve.MetricSample([]string{
		"/gc/cycles/total:gc-cycles",
		"/gc/heap/goal:bytes",
		"/gc/heap/live:bytes",
	})
	for k, v := range m {
		fmt.Printf("  %s = %.0f\n", k, v)
	}
}
```

## Common Mistakes

### Parsing gctrace from Stdout

Wrong: redirecting stdout to capture gctrace output for programmatic parsing.

What happens: `gctrace=1` writes to stderr. Redirecting stdout captures nothing.

Fix: `GODEBUG=gctrace=1 go run main.go 2>gc.log` or `2>&1 | tee gc.log`.

### Calling ReadMemStats in Every Request Handler

Wrong: calling `runtime.ReadMemStats` on every incoming HTTP request to log GC metrics.

What happens: `ReadMemStats` stops the world briefly. At 10,000 req/s, this adds thousands of STW events per second — far more than GC itself would cause.

Fix: use a background goroutine that calls `ReadMemStats` on a 15–30 second interval, or use `runtime/metrics.Read` which does not stop the world.

### Misinterpreting GCCPUFraction

Wrong: seeing `GCCPUFraction = 0.002` immediately after startup and concluding the GC overhead is negligible.

What happens: `GCCPUFraction` is a cumulative average since program start, not the current rate. If your program just started and only one GC cycle has run, the fraction reflects that one cycle diluted by all the non-GC time since startup.

Fix: compare `GCCPUFraction` snapshots over a rolling window that represents steady-state load, not startup transients.

### Reading the Wrong PauseNs Index

Wrong: `m.PauseNs[m.NumGC % 256]` to get the last pause.

What happens: off-by-one. `NumGC` is already incremented after the cycle completes; the last pause is one position back in the circular buffer.

Fix: `m.PauseNs[(m.NumGC+255)%256]`.

## Verification

From `~/go-exercises/gcobserve`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

To see `gctrace` output:

```bash
GODEBUG=gctrace=1 go run ./cmd/demo 2>&1 | grep '^gc'
```

## Summary

- `GODEBUG=gctrace=1` prints one line per GC cycle on stderr with phase timings, heap sizes, CPU fraction, and GOMAXPROCS.
- `runtime.ReadMemStats` provides programmatic access but incurs a brief STW; call it infrequently.
- `runtime/metrics` is the modern, STW-free alternative with histogram support for pause duration distribution.
- `GCCPUFraction` is a cumulative average since startup; monitor it over a rolling window in steady state.
- The `PauseNs` circular buffer index for the last pause is `(m.NumGC+255)%256`.

## What's Next

Next: [GC Pacer and Target Heap](../06-gc-pacer/06-gc-pacer.md).

## Resources

- [runtime.MemStats](https://pkg.go.dev/runtime#MemStats) — complete field documentation
- [runtime/metrics](https://pkg.go.dev/runtime/metrics) — available metric names and kinds
- [Go GC Guide: Observing GC](https://tip.golang.org/doc/gc-guide#Observing) — official guidance on all three observation methods
- [GODEBUG documentation](https://pkg.go.dev/runtime#hdr-Environment_Variables) — all supported GODEBUG keys
