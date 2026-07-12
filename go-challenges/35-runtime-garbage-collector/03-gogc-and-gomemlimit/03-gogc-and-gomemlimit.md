# 3. GOGC and GOMEMLIMIT Tuning

Two environment variables control almost all of Go's GC behaviour: `GOGC` (how often) and `GOMEMLIMIT` (how much). Getting either wrong in production means paying either too much CPU (too frequent GC) or too much memory (infrequent GC with a large heap). This lesson builds a tuning harness that measures the effect of each knob independently, then shows how they interact.

```text
gctuning/
  go.mod
  tuning.go
  tuning_test.go
  cmd/demo/main.go
```

## Concepts

### GOGC: Frequency as a Percentage of Live Heap

`GOGC=100` (the default) means: trigger the next GC cycle when the heap grows to twice the live heap size after the previous collection. In formula terms, the heap goal is:

```
goal = live * (1 + GOGC/100)
```

So `GOGC=100` → goal is 2x live; `GOGC=200` → 3x live; `GOGC=50` → 1.5x live. A higher `GOGC` means less frequent GC, lower CPU cost, and higher peak memory. A lower `GOGC` means more frequent GC, higher CPU cost, and lower peak memory. Setting `GOGC=-1` (or `GOGC=off`) disables ratio-triggered collection entirely.

`debug.SetGCPercent` adjusts `GOGC` at runtime and returns the previous value. This is the safe way to temporarily change it and restore it afterward.

### GOMEMLIMIT: A Soft Memory Ceiling

Introduced in Go 1.19, `GOMEMLIMIT` sets an upper bound on the Go runtime's memory footprint (heap + stacks + metadata). When the runtime's memory usage approaches the limit, the GC runs more aggressively — effectively overriding the `GOGC` ratio — to stay under the limit.

It is a *soft* limit: the runtime guarantees best effort, not a hard cap. If the live heap itself exceeds the limit, the GC cannot free anything and will thrash. The runtime's CPU limiter (Go 1.19+) prevents it from spending more than roughly 50% of CPU on futile GC work; above that threshold it lets the heap exceed the limit rather than starving the application.

`debug.SetMemoryLimit` sets `GOMEMLIMIT` at runtime. `math.MaxInt64` effectively disables it.

### When to Use Which

Use `GOGC` alone when your program's peak memory is not bounded by a container limit and you want to trade CPU for memory headroom. Raising `GOGC` to 200–400 is a common way to reduce GC CPU overhead for batch jobs.

Use `GOMEMLIMIT` (often with `GOGC=off`) in containerised environments. Setting `GOGC=-1` and `GOMEMLIMIT` to 70–80% of the container memory limit maximises throughput by letting the heap grow freely until it approaches the container ceiling, then GC triggers. This avoids premature GC cycles driven by the ratio formula.

Use both together for belt-and-suspenders: keep `GOGC` at a conservative value (200–400) to bound heap growth in normal operation, and `GOMEMLIMIT` as a safety net for memory spikes.

### Interaction

When both are set, the GC triggers at whichever threshold is reached first: the `GOGC` ratio goal or the `GOMEMLIMIT` ceiling. If `GOGC=400` and the heap grows to 5x live before hitting `GOMEMLIMIT`, the limit wins and the GC runs.

### Measuring the Trade-off

The key metrics are: GC cycles per unit of work (CPU cost), peak heap size (memory cost), total pause time, and GC CPU fraction. A good tuning experiment holds the work constant and varies the knob.

## Exercises

### Exercise 1: Workload and Measurement Types

Create `tuning.go`:

```go
package gctuning

import (
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"time"
)

// WorkloadResult holds the measurements from one workload run.
type WorkloadResult struct {
	GCCycles    uint32
	PauseNs     uint64 // cumulative STW pause time in ns
	HeapSysB    uint64 // peak HeapSys during the run
	TotalAllocB uint64 // total bytes allocated during the run
	Elapsed     time.Duration
}

// RunWorkload performs a fixed allocation pattern and returns measurements.
// It allocates n slices of size bytes, periodically dropping half to create
// a mix of live and dead objects. The same pattern runs regardless of GC
// configuration, so results across configurations are comparable.
func RunWorkload(n, size int) WorkloadResult {
	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	start := time.Now()

	var sink []*[]byte
	for i := 0; i < n; i++ {
		b := make([]byte, size)
		b[0] = byte(i)
		sink = append(sink, &b)
		if i%1000 == 999 {
			// Drop the older half to create garbage.
			sink = sink[len(sink)/2:]
		}
	}
	_ = sink
	runtime.GC()

	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	return WorkloadResult{
		GCCycles:    mAfter.NumGC - mBefore.NumGC,
		PauseNs:     mAfter.PauseTotalNs - mBefore.PauseTotalNs,
		HeapSysB:    mAfter.HeapSys,
		TotalAllocB: mAfter.TotalAlloc - mBefore.TotalAlloc,
		Elapsed:     time.Since(start),
	}
}

// BenchGOGC runs RunWorkload under each GOGC value in gcValues, restoring
// the original value after each run. It returns one result per value.
func BenchGOGC(gcValues []int, workloadN, workloadSize int) []WorkloadResult {
	results := make([]WorkloadResult, len(gcValues))
	for i, v := range gcValues {
		prev := debug.SetGCPercent(v)
		runtime.GC() // start clean
		results[i] = RunWorkload(workloadN, workloadSize)
		debug.SetGCPercent(prev)
		runtime.GC()
	}
	return results
}

// BenchGOMEMLIMIT runs RunWorkload under each memory limit in limitBytes,
// with GOGC=100, restoring the original limit after each run.
func BenchGOMEMLIMIT(limitBytes []int64, workloadN, workloadSize int) []WorkloadResult {
	results := make([]WorkloadResult, len(limitBytes))
	for i, lim := range limitBytes {
		prevLim := debug.SetMemoryLimit(lim)
		prevGC := debug.SetGCPercent(100)
		runtime.GC()
		results[i] = RunWorkload(workloadN, workloadSize)
		debug.SetMemoryLimit(prevLim)
		debug.SetGCPercent(prevGC)
		runtime.GC()
	}
	return results
}

// BenchGOGCOff runs the workload with GOGC disabled and a 64 MiB soft limit,
// demonstrating the container pattern.
func BenchGOGCOff(workloadN, workloadSize int) WorkloadResult {
	prevGC := debug.SetGCPercent(-1)          // GOGC=off
	prevLim := debug.SetMemoryLimit(64 << 20) // 64 MiB
	runtime.GC()

	r := RunWorkload(workloadN, workloadSize)

	debug.SetGCPercent(prevGC)
	debug.SetMemoryLimit(prevLim)
	runtime.GC()
	return r
}

// DisableGOMEMLIMIT resets GOMEMLIMIT to effectively unlimited.
func DisableGOMEMLIMIT() {
	debug.SetMemoryLimit(math.MaxInt64)
}
```

### Exercise 2: Example Function

Append to `tuning.go`:

```go
// ExampleBenchGOGC shows that a higher GOGC value results in fewer GC cycles
// for the same workload.
func ExampleBenchGOGC() {
	results := BenchGOGC([]int{50, 200}, 10_000, 512)
	// GOGC=50 triggers more frequently than GOGC=200.
	if results[0].GCCycles >= results[1].GCCycles {
		fmt.Println("higher GOGC -> fewer cycles")
	}
	// Output:
	// higher GOGC -> fewer cycles
}
```

### Exercise 3: Tests

Create `tuning_test.go`:

```go
package gctuning

import (
	"math"
	"runtime/debug"
	"testing"
)

func TestRunWorkloadRecordsAllocations(t *testing.T) {
	t.Parallel()

	r := RunWorkload(1000, 256)
	if r.TotalAllocB == 0 {
		t.Error("TotalAllocB should be > 0")
	}
	if r.Elapsed == 0 {
		t.Error("Elapsed should be > 0")
	}
}

func TestHigherGOGCFeedwerCycles(t *testing.T) {
	t.Parallel()

	results := BenchGOGC([]int{25, 400}, 20_000, 512)
	low := results[0].GCCycles  // GOGC=25
	high := results[1].GCCycles // GOGC=400
	if low <= high {
		t.Errorf("GOGC=25 produced %d cycles, GOGC=400 produced %d: want low > high",
			low, high)
	}
}

func TestLowerGOMEMLIMITMoreCycles(t *testing.T) {
	t.Parallel()

	// Small limit forces more frequent GC; large limit allows fewer.
	results := BenchGOMEMLIMIT([]int64{8 << 20, 256 << 20}, 10_000, 512)
	small := results[0].GCCycles // 8 MiB
	large := results[1].GCCycles // 256 MiB
	if small <= large {
		t.Errorf("8 MiB limit produced %d cycles, 256 MiB produced %d: want small > large",
			small, large)
	}
}

func TestBenchGOGCOffReturnsResult(t *testing.T) {
	t.Parallel()

	r := BenchGOGCOff(5_000, 256)
	if r.TotalAllocB == 0 {
		t.Error("TotalAllocB should be > 0 even with GOGC=off")
	}
}

func TestDisableGOMEMLIMITResetsToMax(t *testing.T) {
	// Not parallel: modifies global GC settings.
	DisableGOMEMLIMIT()
	// After reset, the current limit should be math.MaxInt64. We verify
	// by setting it and checking the returned previous value.
	prev := debug.SetMemoryLimit(math.MaxInt64)
	if prev != math.MaxInt64 {
		t.Errorf("after DisableGOMEMLIMIT, limit = %d, want MaxInt64", prev)
	}
}

func TestBenchGOGCRestoresOriginalPercent(t *testing.T) {
	// Not parallel: reads and verifies global GC setting.
	original := debug.SetGCPercent(100)
	debug.SetGCPercent(original) // restore immediately

	BenchGOGC([]int{50, 200}, 1000, 64)

	after := debug.SetGCPercent(original)
	debug.SetGCPercent(original) // restore again
	if after != original {
		t.Errorf("GOGC after BenchGOGC = %d, want %d", after, original)
	}
}

// Your turn: write TestRunWorkloadGCCyclesPositive that runs RunWorkload with
// a large enough workload (n=50_000, size=1024) and asserts that GCCycles > 0.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/gctuning"
)

func main() {
	const n, size = 30_000, 512

	fmt.Println("GOGC comparison (same workload, varying GOGC)")
	fmt.Printf("  %-8s  %-8s  %-14s  %-12s  %s\n",
		"GOGC", "cycles", "total pause", "heap sys", "elapsed")

	gcValues := []int{25, 100, 200, 500}
	results := gctuning.BenchGOGC(gcValues, n, size)
	for i, r := range results {
		fmt.Printf("  %-8d  %-8d  %-14v  %-12d KB  %v\n",
			gcValues[i],
			r.GCCycles,
			time.Duration(r.PauseNs),
			r.HeapSysB/1024,
			r.Elapsed)
	}

	fmt.Println()
	fmt.Println("GOMEMLIMIT comparison (GOGC=100, varying limit)")
	fmt.Printf("  %-12s  %-8s  %-12s  %s\n",
		"limit", "cycles", "heap sys", "elapsed")

	limits := []int64{16 << 20, 64 << 20, 256 << 20}
	limitResults := gctuning.BenchGOMEMLIMIT(limits, n, size)
	for i, r := range limitResults {
		fmt.Printf("  %-12d  %-8d  %-12d KB  %v\n",
			limits[i]/(1<<20),
			r.GCCycles,
			r.HeapSysB/1024,
			r.Elapsed)
	}

	fmt.Println()
	fmt.Println("GOGC=off + 64 MiB GOMEMLIMIT (container pattern)")
	r := gctuning.BenchGOGCOff(n, size)
	fmt.Printf("  cycles=%d  heap=%d KB  elapsed=%v\n",
		r.GCCycles, r.HeapSysB/1024, r.Elapsed)

	gctuning.DisableGOMEMLIMIT()
}
```

## Common Mistakes

### Setting GOMEMLIMIT Equal to the Container Limit

Wrong: `GOMEMLIMIT=512MiB` when the container cgroup limit is also 512 MiB.

What happens: the Go runtime's memory footprint includes stacks, metadata, and memory-mapped spans in addition to the heap. If the heap alone fills the limit, non-heap runtime memory pushes the process over the cgroup limit and the OOM killer fires.

Fix: set `GOMEMLIMIT` to 70–80% of the container memory limit to leave headroom for non-heap runtime memory and OS overhead.

### Disabling GOGC Without GOMEMLIMIT

Wrong: `GOGC=off` with no `GOMEMLIMIT`.

What happens: the heap grows without bound until the process is OOM-killed. There is no ratio-based trigger and no ceiling.

Fix: always pair `GOGC=-1` with a `GOMEMLIMIT`. The pattern is: `GOGC=off` + `GOMEMLIMIT = container_limit * 0.75`.

### Ignoring GC CPU Fraction When Tuning

Wrong: lowering `GOGC` to 10 to "keep memory usage low" without checking `GCCPUFraction`.

What happens: the GC runs so frequently that it consumes 20–30% of available CPU. Throughput collapses even though memory usage is low.

Fix: monitor `MemStats.GCCPUFraction` or `/gc/cpu/fraction:cpu-seconds` from `runtime/metrics`. Keep it below 5% for latency-sensitive services.

### Calling SetGCPercent Without Restoring

Wrong: changing `GOGC` in a test without capturing and restoring the previous value.

What happens: later tests in the same process run under an unexpected GOGC setting, producing non-reproducible results.

Fix: `prev := debug.SetGCPercent(v); defer debug.SetGCPercent(prev)`.

## Verification

From `~/go-exercises/gctuning`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

The `ExampleBenchGOGC` function is auto-verified by `go test`.

## Summary

- `GOGC=N` triggers GC when the heap reaches `live * (1 + N/100)`; default N=100 means 2x live.
- `GOMEMLIMIT` sets a soft ceiling; the runtime increases GC frequency to stay under it.
- Setting `GOGC=-1` (off) disables ratio-triggered GC; always pair it with `GOMEMLIMIT`.
- In containers, set `GOMEMLIMIT` to 70–80% of the cgroup limit; leave `GOGC` at a high value or off.
- Monitor `GCCPUFraction` to ensure GC CPU overhead stays below 5% in production.
- `debug.SetGCPercent` and `debug.SetMemoryLimit` allow runtime tuning without restart.

## What's Next

Next: [Write Barriers and GC Invariants](../04-write-barriers/04-write-barriers.md).

## Resources

- [Go GC Guide](https://tip.golang.org/doc/gc-guide) — explains GOGC, GOMEMLIMIT, and their interaction in detail
- [runtime/debug.SetGCPercent](https://pkg.go.dev/runtime/debug#SetGCPercent) — API documentation
- [runtime/debug.SetMemoryLimit](https://pkg.go.dev/runtime/debug#SetMemoryLimit) — API documentation
- [Soft Memory Limit Design](https://github.com/golang/proposal/blob/master/design/48409-soft-memory-limit.md) — the full design rationale including the CPU limiter
- [Go 1.19 Release Notes: GOMEMLIMIT](https://go.dev/doc/go1.19#runtime) — introduction of the feature
