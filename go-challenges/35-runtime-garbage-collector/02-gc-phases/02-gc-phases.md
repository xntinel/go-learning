# 2. GC Phases

Go's garbage collector is not a single monolithic pause. It proceeds through four distinct phases, only two of which stop the world — and those two pauses are typically measured in microseconds, not milliseconds. Understanding which phases run concurrently with your application and which do not lets you predict latency impact, read `gctrace` output correctly, and know when tuning will and will not help.

```text
gcphases/
  go.mod
  phases.go
  phases_test.go
  cmd/demo/main.go
```

## Concepts

### The Four Phases

A single GC cycle proceeds in order:

1. Sweep termination (STW). The runtime stops all goroutines briefly to finalise any in-progress sweeping from the previous cycle and transition to the "GC on" state.

2. Concurrent mark. The bulk of the work. Mark goroutines (background workers, one or more per P) scan the heap concurrently with your program. GC assists run here: if a goroutine allocates faster than the background workers can mark, the allocating goroutine is required to do proportional marking work before its allocation is satisfied. This is the mechanism that bounds heap growth during marking.

3. Mark termination (STW). The runtime stops all goroutines to flush remaining mark work, finalise the live heap size, and compute the heap goal for the next cycle. Since Go 1.14 this pause is rarely above a hundred microseconds on modern hardware.

4. Concurrent sweep. The runtime sweeps (reclaims) white spans on demand, concurrently with the next mutator generation. Swept memory is returned to the allocator's span pool or to the OS.

### Stop-the-World Is Not What People Think

The two STW pauses — sweep termination and mark termination — together are almost always under 500 µs on well-tuned workloads and under 200 µs since Go 1.14. The dominant GC overhead in production is GC assist latency (high-allocating goroutines doing marking work) and the CPU fraction consumed by background workers, not the STW pauses themselves.

### GC Assist: The Invisible Tax

GC assist is a debt mechanism. The runtime tracks how many bytes each goroutine has allocated since the GC cycle started. When a goroutine accumulates too much debt relative to the mark rate, its next allocation call is paused and the goroutine is forced to scan a proportional number of bytes before the allocation proceeds. This keeps the heap from growing unboundedly during marking, but it adds latency directly to allocation calls in high-throughput code.

### Reading MemStats and runtime/metrics

`runtime.MemStats.PauseNs` is a circular buffer of the last 256 individual STW pause durations in nanoseconds. `PauseTotalNs` is the cumulative sum. `GCCPUFraction` is the fraction of available CPU time the GC has consumed since program start. These are the primary programmatic observability knobs.

`runtime/metrics` (Go 1.16+) is the modern alternative: it reads metrics without the brief STW overhead that `ReadMemStats` incurs, and it exposes histograms that `MemStats` does not.

### The Heap Goal

After mark termination, the pacer sets the next trigger: the heap size at which the next mark phase should begin. The goal is approximately `live * (1 + GOGC/100)`. The trigger is set earlier than the goal to give the mark phase time to finish before the heap reaches the goal. If allocation is faster than expected, GC assists bridge the gap.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/35-runtime-garbage-collector/02-gc-phases/02-gc-phases/cmd/demo
cd go-solutions/35-runtime-garbage-collector/02-gc-phases/02-gc-phases
```

### Exercise 1: Capture Pause Statistics

Create `phases.go`:

```go
package gcphases

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"time"
)

// PauseSample holds the timing data from one forced GC cycle.
type PauseSample struct {
	CyclesBefore uint32
	CyclesAfter  uint32
	LastPauseNs  uint64
	TotalPauseNs uint64
	HeapAllocB   uint64
	HeapInuseB   uint64
	GCCPUFrac    float64
	Elapsed      time.Duration
}

// MeasureOneCycle forces a single GC cycle and returns timing data.
func MeasureOneCycle() PauseSample {
	var mBefore, mAfter runtime.MemStats
	runtime.ReadMemStats(&mBefore)

	start := time.Now()
	runtime.GC()
	elapsed := time.Since(start)

	runtime.ReadMemStats(&mAfter)

	return PauseSample{
		CyclesBefore: mBefore.NumGC,
		CyclesAfter:  mAfter.NumGC,
		LastPauseNs:  mAfter.PauseNs[(mAfter.NumGC+255)%256],
		TotalPauseNs: mAfter.PauseTotalNs,
		HeapAllocB:   mAfter.HeapAlloc,
		HeapInuseB:   mAfter.HeapInuse,
		GCCPUFrac:    mAfter.GCCPUFraction,
		Elapsed:      elapsed,
	}
}

// AllocAndMeasure allocates n slices of size bytes, then forces a GC cycle
// and returns the pause sample. The returned [][]byte prevents premature
// collection during measurement; callers should discard it to generate garbage.
func AllocAndMeasure(n, size int) (PauseSample, [][]byte) {
	buf := make([][]byte, n)
	for i := range buf {
		buf[i] = make([]byte, size)
	}
	sample := MeasureOneCycle()
	return sample, buf
}

// PauseStats summarises a slice of pause durations.
type PauseStats struct {
	Min, Max, Sum time.Duration
	Count         int
}

// CollectPauseStats runs the GC multiple times across a repeated allocation
// pattern and returns statistics on the last-pause duration per cycle.
func CollectPauseStats(cycles, allocsPerCycle, bytesPerAlloc int) PauseStats {
	var stats PauseStats
	for i := 0; i < cycles; i++ {
		buf := make([][]byte, allocsPerCycle)
		for j := range buf {
			buf[j] = make([]byte, bytesPerAlloc)
		}
		// Drop references so they become garbage.
		for j := range buf {
			buf[j] = nil
		}
		_ = buf

		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		pause := time.Duration(m.PauseNs[(m.NumGC+255)%256])

		if stats.Count == 0 || pause < stats.Min {
			stats.Min = pause
		}
		if pause > stats.Max {
			stats.Max = pause
		}
		stats.Sum += pause
		stats.Count++
	}
	return stats
}

// GCPercent returns the current GOGC value via debug.SetGCPercent.
// It does not change the value.
func GCPercent() int {
	v := debug.SetGCPercent(-1) // read trick: set to -1 and restore
	debug.SetGCPercent(v)
	return v
}
```

### Exercise 2: Example Function

Append to `phases.go`:

```go
// ExampleMeasureOneCycle demonstrates that runtime.GC() completes one full
// GC cycle and that the cycle counter increments the cycle counter (by at least one).
func ExampleMeasureOneCycle() {
	s := MeasureOneCycle()
	if s.CyclesAfter > s.CyclesBefore {
		fmt.Println("GC cycle completed")
	}
	// Output:
	// GC cycle completed
}
```

### Exercise 3: Test the Phase Measurements

Create `phases_test.go`:

```go
package gcphases

import (
	"testing"
	"time"
)

func TestMeasureOneCycleIncrementsCounter(t *testing.T) {
	t.Parallel()

	s := MeasureOneCycle()
	if s.CyclesAfter <= s.CyclesBefore {
		t.Errorf("expected at least one GC cycle: before=%d after=%d",
			s.CyclesBefore, s.CyclesAfter)
	}
}

func TestMeasureOneCyclePauseIsPositive(t *testing.T) {
	t.Parallel()

	s := MeasureOneCycle()
	if s.LastPauseNs == 0 {
		t.Error("LastPauseNs should be > 0 after a GC cycle")
	}
}

func TestMeasureOneCycleElapsedIsPositive(t *testing.T) {
	t.Parallel()

	s := MeasureOneCycle()
	if s.Elapsed <= 0 {
		t.Errorf("Elapsed = %v, want > 0", s.Elapsed)
	}
}

func TestAllocAndMeasureReturnsBuf(t *testing.T) {
	t.Parallel()

	s, buf := AllocAndMeasure(100, 1024)
	if len(buf) != 100 {
		t.Errorf("len(buf) = %d, want 100", len(buf))
	}
	if s.CyclesAfter <= s.CyclesBefore {
		t.Error("expected at least one GC cycle after AllocAndMeasure")
	}
}

func TestCollectPauseStatsMinLeMax(t *testing.T) {
	t.Parallel()

	stats := CollectPauseStats(5, 1000, 1024)
	if stats.Count != 5 {
		t.Errorf("count = %d, want 5", stats.Count)
	}
	if stats.Min > stats.Max {
		t.Errorf("Min (%v) > Max (%v)", stats.Min, stats.Max)
	}
}

func TestCollectPauseStatsPausesAreBelowReasonableThreshold(t *testing.T) {
	t.Parallel()

	stats := CollectPauseStats(3, 500, 512)
	// STW pauses in Go 1.14+ are almost always under 5 ms on developer hardware.
	const limit = 5 * time.Millisecond
	if stats.Max > limit {
		t.Logf("Max pause %v exceeded %v — this is a warning, not always a bug", stats.Max, limit)
	}
}

func TestGCPercentReturnsPositiveOrMinus1(t *testing.T) {
	t.Parallel()

	v := GCPercent()
	// Default is 100; anything >= 0 or -1 (disabled) is valid.
	if v < -1 {
		t.Errorf("GCPercent() = %d, want >= -1", v)
	}
}

// Your turn: write TestAllocAndMeasureGCCPUFracIsInRange that calls
// AllocAndMeasure(10000, 4096), checks that s.GCCPUFrac >= 0 and <= 1.0,
// and fails with a descriptive message if either bound is violated.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/gcphases"
)

func main() {
	fmt.Println("GC phases demonstration")
	fmt.Println()

	// Single forced cycle.
	s := gcphases.MeasureOneCycle()
	fmt.Printf("Single GC cycle\n")
	fmt.Printf("  Cycles before/after: %d -> %d\n", s.CyclesBefore, s.CyclesAfter)
	fmt.Printf("  Last STW pause:      %v\n", time.Duration(s.LastPauseNs))
	fmt.Printf("  GC CPU fraction:     %.4f\n", s.GCCPUFrac)
	fmt.Printf("  Heap alloc:          %d KB\n", s.HeapAllocB/1024)
	fmt.Println()

	// Allocation workload then measure.
	s2, _ := gcphases.AllocAndMeasure(50_000, 512)
	fmt.Printf("After 50k x 512 B allocations\n")
	fmt.Printf("  GC cycles: %d -> %d\n", s2.CyclesBefore, s2.CyclesAfter)
	fmt.Printf("  Elapsed:   %v\n", s2.Elapsed)
	fmt.Println()

	// Pause distribution.
	stats := gcphases.CollectPauseStats(10, 5_000, 1024)
	fmt.Printf("Pause stats over 10 cycles\n")
	fmt.Printf("  Min: %v\n", stats.Min)
	fmt.Printf("  Max: %v\n", stats.Max)
	fmt.Printf("  Avg: %v\n", stats.Sum/time.Duration(stats.Count))
	fmt.Println()
	fmt.Println("Tip: run with GODEBUG=gctrace=1 to see per-cycle phase timing.")
}
```

## Common Mistakes

### Calling ReadMemStats in a Hot Path

Wrong: calling `runtime.ReadMemStats(&m)` on every request or in a tight loop.

What happens: `ReadMemStats` stops the world briefly to capture a consistent snapshot. Calling it frequently makes your program cause more STW pauses than GC itself does.

Fix: call it once before and once after a benchmark, or use `runtime/metrics.Read` which does not stop the world.

### Assuming STW Pauses Dominate Latency

Wrong: spending effort minimising the two STW pauses while ignoring GC assist latency.

What happens: on a workload that allocates 100 MB/s, GC assist can add 50–200 µs to individual allocation calls, while the STW pauses are 30 µs. You optimise the wrong thing.

Fix: measure GC assist separately. `runtime/metrics` exposes `/sched/latencies:seconds` which includes assist delays. Reduce allocation rate (lesson 09) to shrink assist pressure.

### Forgetting That Sweep Is Concurrent

Wrong: assuming GC work is done after the two STW pauses, so the heap is completely clean immediately after mark termination.

What happens: allocation in the cycle following GC must sweep spans on demand before handing them to the allocator. This sweep cost appears as allocation latency, not as GC pause time.

Fix: understand that `runtime.GC()` returns after mark termination; the sweep runs lazily afterward. Profile with `GODEBUG=gctrace=1` to see the full picture.

### Misreading the PauseNs Circular Buffer Index

Wrong: reading `PauseNs[m.NumGC % 256]` for the last pause.

What happens: off-by-one. The last completed pause is at index `(m.NumGC + 255) % 256`, because `NumGC` is already incremented after the cycle completes.

Fix: use `PauseNs[(m.NumGC+255)%256]` as shown in `MeasureOneCycle`.

## Verification

From `~/go-exercises/gcphases`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Run the demo with gctrace to see raw phase output:

```bash
GODEBUG=gctrace=1 go run ./cmd/demo 2>&1 | head -30
```

Each `gc N @Xs Y%: A+B+C ms clock, D+E/F/G+H ms cpu, I->J->K MB, L MB goal, M P` line breaks down to: cycle number, program age, GC CPU fraction, wall-clock split across the two STW pauses and concurrent mark, CPU time split, heap before/after/live, heap goal, and GOMAXPROCS.

## Summary

- Four phases: sweep termination (STW), concurrent mark, mark termination (STW), concurrent sweep.
- The two STW pauses are typically under 200 µs in modern Go; they are not the primary source of latency overhead.
- GC assist is the main source of allocation-latency spikes: allocating goroutines are forced to mark when they allocate faster than background workers can keep up.
- `runtime.ReadMemStats` gives pause data but incurs a brief STW; use `runtime/metrics` for production monitoring.
- `GODEBUG=gctrace=1` outputs one line per cycle with phase timings, heap sizes, and CPU fraction.

## What's Next

Next: [GOGC and GOMEMLIMIT Tuning](../03-gogc-and-gomemlimit/03-gogc-and-gomemlimit.md).

## Resources

- [Go GC Guide](https://tip.golang.org/doc/gc-guide) — authoritative overview of all four phases and their interactions
- [runtime.MemStats](https://pkg.go.dev/runtime#MemStats) — full documentation of every field including PauseNs and GCCPUFraction
- [runtime/metrics](https://pkg.go.dev/runtime/metrics) — the modern, STW-free metrics API
- [Getting to Go: The Journey of Go's Garbage Collector](https://go.dev/blog/ismmkeynote) — design history including the STW reduction work in Go 1.14
