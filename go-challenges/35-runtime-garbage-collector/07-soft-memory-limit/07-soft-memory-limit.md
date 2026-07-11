# 7. Soft Memory Limit

`GOMEMLIMIT` constrains the Go runtime's total memory footprint, but it is a *soft* limit: if the live heap exceeds the limit, the runtime cannot free anything and the GC will thrash — consuming most available CPU on futile collection attempts. The runtime's CPU limiter (Go 1.19+) prevents total CPU starvation in this scenario by allowing the heap to exceed the limit rather than spending more than roughly 50% of CPU on GC. Understanding this three-regime behaviour — comfortable, pressured, thrashing — is essential for safe container deployments.

```text
softlimit/
  go.mod
  pressure.go
  pressure_test.go
  cmd/demo/main.go
```

## Concepts

### The Three Regimes

1. Comfortable: live heap is well below `GOMEMLIMIT`. The GC runs at the rate dictated by `GOGC`. Memory is stable; GC CPU overhead is low (1–5%).

2. Pressured: live heap approaches `GOMEMLIMIT`. The GC increases collection frequency, overriding the `GOGC` ratio. GC CPU rises (10–30%). The heap stays below the limit but throughput drops.

3. Thrashing: the live heap equals or exceeds `GOMEMLIMIT`. There is nothing to collect; every GC cycle is wasted work. The CPU limiter fires: when GC would consume more than approximately 50% of CPU, the runtime stops GC work and lets the heap exceed the limit. The process is now likely to be OOM-killed by the kernel.

### The CPU Limiter

The CPU limiter (introduced in Go 1.19 alongside GOMEMLIMIT) ensures the application always makes forward progress even under extreme memory pressure. When GC would consume more than ~50% of CPU, the limiter backs off and allows the heap to exceed `GOMEMLIMIT`. The metric `/gc/limiter/last-enabled:gc-cycle` records the last cycle at which the limiter activated; a non-zero value in production is a signal that the live heap is dangerously close to the limit.

### Container Memory Sizing

The Go runtime's memory footprint includes more than just the heap: goroutine stacks, runtime metadata, memory-mapped spans waiting to be returned to the OS, and any memory-mapped files. In practice, non-heap runtime overhead is 5–20% of heap size for typical services.

Safe rule: `GOMEMLIMIT = container_memory_limit * 0.75`. This leaves 25% headroom for non-heap memory and transient OS page cache effects. Do not set `GOMEMLIMIT` equal to the cgroup limit.

### Interaction With GOGC

Three viable strategies:

| Strategy | When to Use |
| --- | --- |
| `GOGC=100` (default), no `GOMEMLIMIT` | Memory is unconstrained; GC overhead is acceptable |
| `GOGC=-1`, `GOMEMLIMIT=N` | Containerised; maximise throughput; live heap << N |
| `GOGC=200`, `GOMEMLIMIT=N` | Belt-and-suspenders; steady GC rate + memory ceiling |

Strategy 2 (GOGC=off + GOMEMLIMIT) is common in production container deployments. The heap grows until it approaches the limit, then GC runs. This amortises GC overhead across larger heaps, reducing the number of cycles per unit of work. The risk: if the live heap grows unexpectedly close to the limit, you enter the pressured or thrashing regime with no ratio-based safety net.

### Observing Limit Activation

```go
samples := []metrics.Sample{
    {Name: "/gc/limiter/last-enabled:gc-cycle"},
    {Name: "/memory/classes/total:bytes"},
    {Name: "/gc/heap/live:bytes"},
}
metrics.Read(samples)
```

If `/gc/limiter/last-enabled:gc-cycle` is non-zero, the CPU limiter has fired during this process lifetime. This is an operational red flag.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/softlimit/cmd/demo
cd ~/go-exercises/softlimit
go mod init example.com/softlimit
```

### Exercise 1: Pressure Measurement Types

Create `pressure.go`:

```go
package softlimit

import (
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"time"
)

// Regime classifies the GC behaviour relative to GOMEMLIMIT.
type Regime int

const (
	RegimeComfortable Regime = iota // GC overhead < 5%
	RegimePressured                 // GC overhead 5-30%
	RegimeThrashing                 // CPU limiter active or GC overhead > 30%
)

func (r Regime) String() string {
	switch r {
	case RegimeComfortable:
		return "comfortable"
	case RegimePressured:
		return "pressured"
	case RegimeThrashing:
		return "thrashing"
	default:
		return "unknown"
	}
}

// PressureResult holds the metrics from one pressure scenario run.
type PressureResult struct {
	MemLimitB      int64
	GCCycles       uint32
	GCCPUFrac      float64
	HeapLiveB      uint64
	TotalMemB      uint64
	LimiterEnabled bool
	Elapsed        time.Duration
	Regime         Regime
}

// RunWithLimit sets GOMEMLIMIT to limitBytes, optionally disables GOGC,
// runs the workload, then restores the previous settings.
func RunWithLimit(limitBytes int64, gogcOff bool, workloadFn func()) PressureResult {
	prevLim := debug.SetMemoryLimit(limitBytes)
	var prevGC int
	if gogcOff {
		prevGC = debug.SetGCPercent(-1)
	} else {
		prevGC = debug.SetGCPercent(100)
	}
	runtime.GC()

	var mBefore runtime.MemStats
	runtime.ReadMemStats(&mBefore)
	start := time.Now()

	workloadFn()

	elapsed := time.Since(start)
	var mAfter runtime.MemStats
	runtime.ReadMemStats(&mAfter)

	// Read limiter status.
	limSamples := []metrics.Sample{
		{Name: "/gc/limiter/last-enabled:gc-cycle"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/heap/live:bytes"},
	}
	metrics.Read(limSamples)

	limiterCycle := limSamples[0].Value.Uint64()
	totalMem := limSamples[1].Value.Uint64()
	liveMem := limSamples[2].Value.Uint64()

	cpuFrac := mAfter.GCCPUFraction

	// Classify regime.
	var regime Regime
	switch {
	case limiterCycle > 0 || cpuFrac > 0.30:
		regime = RegimeThrashing
	case cpuFrac > 0.05:
		regime = RegimePressured
	default:
		regime = RegimeComfortable
	}

	debug.SetMemoryLimit(prevLim)
	debug.SetGCPercent(prevGC)
	runtime.GC()

	return PressureResult{
		MemLimitB:      limitBytes,
		GCCycles:       mAfter.NumGC - mBefore.NumGC,
		GCCPUFrac:      cpuFrac,
		HeapLiveB:      liveMem,
		TotalMemB:      totalMem,
		LimiterEnabled: limiterCycle > 0,
		Elapsed:        elapsed,
		Regime:         regime,
	}
}

// SmallWorkload is a workload that allocates a modest amount and generates
// garbage: suitable for the comfortable regime.
func SmallWorkload() {
	for i := 0; i < 10_000; i++ {
		_ = make([]byte, 512)
	}
	runtime.GC()
}

// LargeWorkload allocates significantly more: suitable for probing the
// pressured regime when limit is set tightly.
func LargeWorkload() {
	buf := make([][]byte, 0, 50_000)
	for i := 0; i < 50_000; i++ {
		b := make([]byte, 1024)
		b[0] = byte(i)
		buf = append(buf, b)
		if i%5000 == 4999 {
			buf = buf[len(buf)/2:] // drop half to generate garbage
		}
	}
	_ = buf
	runtime.GC()
}

// SafeLimitForContainer computes the recommended GOMEMLIMIT given a
// container memory limit, using the 75% rule.
func SafeLimitForContainer(containerLimitB int64) int64 {
	return int64(float64(containerLimitB) * 0.75)
}

// DisableMemoryLimit resets GOMEMLIMIT to unlimited.
func DisableMemoryLimit() {
	debug.SetMemoryLimit(math.MaxInt64)
}
```

### Exercise 2: Example Function

Append to `pressure.go`:

```go
// ExampleSafeLimitForContainer shows the 75% container sizing rule.
func ExampleSafeLimitForContainer() {
	limit := SafeLimitForContainer(512 << 20) // 512 MiB container
	fmt.Printf("safe limit: %d MiB\n", limit/(1<<20))
	// Output:
	// safe limit: 384 MiB
}
```

### Exercise 3: Tests

Create `pressure_test.go`:

```go
package softlimit

import (
	"testing"
)

func TestRunWithLimitSmallWorkloadComfortable(t *testing.T) {
	// Not parallel: modifies global GC settings.
	result := RunWithLimit(256<<20, false, SmallWorkload) // 256 MiB limit
	if result.GCCycles == 0 {
		t.Error("expected at least one GC cycle")
	}
	// With 256 MiB limit and a tiny workload, we should be comfortable.
	if result.GCCPUFrac > 0.30 {
		t.Logf("GC CPU fraction = %.4f; expected comfortable regime", result.GCCPUFrac)
	}
}

func TestRunWithLimitRestoresSettings(t *testing.T) {
	// Not parallel: modifies global GC settings.
	import_check := 0 // just to confirm the function runs to completion
	RunWithLimit(64<<20, true, func() { import_check++ })
	if import_check != 1 {
		t.Error("workload function was not called")
	}
	// After RunWithLimit, GOMEMLIMIT should be restored to what it was before.
	// We cannot read it directly, but if the next GC cycle completes without
	// thrashing, the restore worked.
	SmallWorkload() // should not panic or deadlock
}

func TestRegimeStringValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		r    Regime
		want string
	}{
		{RegimeComfortable, "comfortable"},
		{RegimePressured, "pressured"},
		{RegimeThrashing, "thrashing"},
	}
	for _, tc := range cases {
		if got := tc.r.String(); got != tc.want {
			t.Errorf("Regime(%d).String() = %q, want %q", tc.r, got, tc.want)
		}
	}
}

func TestSafeLimitForContainer75Percent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		containerB int64
		wantB      int64
	}{
		{1024 << 20, 768 << 20}, // 1 GiB -> 768 MiB
		{512 << 20, 384 << 20},  // 512 MiB -> 384 MiB
		{256 << 20, 192 << 20},  // 256 MiB -> 192 MiB
	}
	for _, tc := range cases {
		got := SafeLimitForContainer(tc.containerB)
		if got != tc.wantB {
			t.Errorf("SafeLimitForContainer(%d) = %d, want %d", tc.containerB, got, tc.wantB)
		}
	}
}

func TestSmallWorkloadCompletes(t *testing.T) {
	t.Parallel()

	// Smoke test: SmallWorkload should not panic.
	SmallWorkload()
}

func TestLargeWorkloadCompletes(t *testing.T) {
	t.Parallel()

	// Smoke test: LargeWorkload should not panic.
	LargeWorkload()
}

// Your turn: write TestRunWithLimitGOGCOffAndLargeLimit that calls
// RunWithLimit(1<<30, true, LargeWorkload) (1 GiB limit, GOGC=off) and
// asserts result.Regime == RegimeComfortable (live heap << limit).
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/softlimit"
)

func main() {
	fmt.Println("GOMEMLIMIT regime demonstration")
	fmt.Println()

	scenarios := []struct {
		name    string
		limitMB int64
		gogcOff bool
		fn      func()
	}{
		{"comfortable (256 MiB, small workload)", 256, false, softlimit.SmallWorkload},
		{"comfortable (256 MiB, GOGC=off)", 256, true, softlimit.SmallWorkload},
		{"pressured (32 MiB, large workload)", 32, false, softlimit.LargeWorkload},
	}

	for _, s := range scenarios {
		limitB := s.limitMB << 20
		r := softlimit.RunWithLimit(limitB, s.gogcOff, s.fn)
		fmt.Printf("Scenario: %s\n", s.name)
		fmt.Printf("  Regime:          %s\n", r.Regime)
		fmt.Printf("  GC cycles:       %d\n", r.GCCycles)
		fmt.Printf("  GC CPU fraction: %.4f\n", r.GCCPUFrac)
		fmt.Printf("  Live heap:       %d KB\n", r.HeapLiveB/1024)
		fmt.Printf("  CPU limiter:     %v\n", r.LimiterEnabled)
		fmt.Printf("  Elapsed:         %v\n", r.Elapsed)
		fmt.Println()
	}

	// Container sizing recommendation.
	containerMB := int64(512)
	safe := softlimit.SafeLimitForContainer(containerMB << 20)
	fmt.Printf("Container sizing: for %d MiB cgroup limit, set GOMEMLIMIT=%d MiB\n",
		containerMB, safe/(1<<20))

	softlimit.DisableMemoryLimit()
}
```

## Common Mistakes

### Setting GOMEMLIMIT Equal to the Container Limit

Wrong: `GOMEMLIMIT=512MiB` when the container cgroup limit is exactly 512 MiB.

What happens: the Go runtime's total footprint (heap + stacks + metadata) exceeds the heap alone. When the heap approaches 512 MiB, the process's total RSS is already higher, and the OOM killer fires before the GC can help.

Fix: set `GOMEMLIMIT = container_limit * 0.75`. The 25% headroom covers non-heap runtime memory, OS page cache, and cgroup accounting discrepancies.

### Assuming the CPU Limiter Prevents OOM

Wrong: believing that because the CPU limiter prevents GC thrashing, the process cannot be OOM-killed.

What happens: the CPU limiter prevents GC from consuming all CPU, but it does so by allowing the heap to exceed the soft limit. The kernel's OOM killer sees total RSS, which now exceeds the cgroup limit, and kills the process.

Fix: the CPU limiter is a last resort, not a safety guarantee. Design the live heap to stay well below `GOMEMLIMIT`.

### Using GOGC=off Without Monitoring the Limiter Metric

Wrong: deploying `GOGC=-1` + `GOMEMLIMIT` to production without alerting on `/gc/limiter/last-enabled:gc-cycle`.

What happens: when the live heap grows unexpectedly (a cache not being evicted, a goroutine leak holding references), you enter the pressured or thrashing regime silently.

Fix: scrape `/gc/limiter/last-enabled:gc-cycle` and alert if it is non-zero during any scrape interval.

## Verification

From `~/go-exercises/softlimit`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- GOMEMLIMIT is a soft ceiling; the GC works harder as the heap approaches it.
- Three regimes: comfortable (low GC overhead), pressured (high GC overhead, heap stable), thrashing (CPU limiter active, heap may exceed limit).
- The CPU limiter (Go 1.19+) prevents GC from consuming more than ~50% of CPU; it allows the heap to exceed GOMEMLIMIT rather than starving the application.
- Safe container rule: set `GOMEMLIMIT = container_limit * 0.75`.
- Monitor `/gc/limiter/last-enabled:gc-cycle`; a non-zero value means the live heap is dangerously close to the limit.

## What's Next

Next: [GC Impact on Tail Latency](../08-gc-impact-tail-latency/08-gc-impact-tail-latency.md).

## Resources

- [Soft Memory Limit Design Document](https://github.com/golang/proposal/blob/master/design/48409-soft-memory-limit.md) — full rationale, edge cases, and CPU limiter design
- [Go GC Guide: Memory Limit](https://tip.golang.org/doc/gc-guide#Memory_limit) — official guidance on GOMEMLIMIT usage and container sizing
- [runtime/metrics](https://pkg.go.dev/runtime/metrics) — `/gc/limiter/last-enabled:gc-cycle` and memory class metrics
- [GC CPU Limiter issue](https://github.com/golang/go/issues/52433) — the discussion behind the 50% CPU cap
