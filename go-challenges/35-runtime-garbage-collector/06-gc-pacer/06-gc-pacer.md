# 6. GC Pacer and Target Heap

The GC pacer is the controller that decides when to start a mark phase, how much concurrent work to do, and how aggressively to use GC assists. Its goal is to finish marking at exactly the moment the heap reaches the target — not before (wasted CPU) and not after (heap overshoot). This lesson implements a faithful simulation of the pacer's feedback loop so you can observe how it adapts to different allocation patterns.

```text
gcpacer/
  go.mod
  pacer.go
  pacer_test.go
  cmd/demo/main.go
```

## Concepts

### The Pacer's Job

After each GC cycle, the pacer answers two questions:

1. When should the next cycle start? (The trigger point, expressed as a heap size.)
2. How much background marking work should run concurrently?

The answer to (1) must balance two risks: starting too late means the heap overshoots the goal before marking finishes; starting too early wastes CPU on marking work when not much memory needs reclaiming.

### Heap Goal

The heap goal for the next cycle is:

```
goal = live * (1 + GOGC/100)
```

where `live` is the live heap size measured at the end of the previous mark phase. If `GOMEMLIMIT` is set, the goal is `min(goal, GOMEMLIMIT - overhead)`.

### Trigger Point

The trigger is set so that the mark phase finishes at exactly the moment the heap reaches the goal. The runtime estimates how long marking takes (based on the previous cycle's scan rate and allocation rate) and triggers the next cycle early enough:

```
trigger = goal - alloc_rate * estimated_mark_duration
```

If allocation is faster than expected, GC assists kick in, borrowing CPU from allocating goroutines to do extra marking work. If marking finishes early, the next trigger is adjusted upward.

### GC Assists

When a goroutine has accumulated too much allocation debt relative to the mark rate, its next `mallocgc` call pauses and forces the goroutine to scan a proportional number of bytes. This is visible as added latency on allocation calls, not as a GC pause in the traditional sense.

The assist ratio is:

```
assist_ratio = scan_work_remaining / alloc_budget_remaining
```

A goroutine that allocates 1 KB during marking must scan `1 KB * assist_ratio` bytes before the allocation completes.

### Exponential Smoothing of Estimates

The pacer smooths allocation rate and scan rate estimates across cycles using exponential moving averages, giving more weight to recent cycles. This is why the pacer adapts relatively quickly to a burst in allocation rate: within 2–3 cycles it adjusts the trigger point to match the new rate.

### Observing the Real Pacer

`runtime/metrics` exposes `/gc/heap/goal:bytes` and `/gc/heap/live:bytes`. Watching the ratio `goal / live` across cycles tells you whether GOGC is binding (`goal / live ≈ 1 + GOGC/100`) or GOMEMLIMIT is binding (`goal < live * (1 + GOGC/100)`).

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/gcpacer/cmd/demo
cd ~/go-exercises/gcpacer
go mod init example.com/gcpacer
```

### Exercise 1: Pacer Simulation

Create `pacer.go`:

```go
package gcpacer

import (
	"math"
)

// Config holds the GC tuning parameters for the pacer simulation.
type Config struct {
	GOGC        int     // GC percent; -1 means off
	MemLimit    float64 // bytes; 0 means no limit (MaxFloat64)
	ScanRate    float64 // bytes/second: how fast the background workers mark
	AllocRate   float64 // bytes/second: how fast the mutator allocates
	MarkCPUFrac float64 // fraction of CPU dedicated to background GC workers
}

// DefaultConfig returns a Config that approximates Go's defaults.
func DefaultConfig() Config {
	return Config{
		GOGC:        100,
		MemLimit:    math.MaxFloat64,
		ScanRate:    1e9, // 1 GB/s mark rate
		AllocRate:   5e7, // 50 MB/s allocation rate
		MarkCPUFrac: 0.25,
	}
}

// CycleReport summarises one simulated GC cycle.
type CycleReport struct {
	Cycle         int
	LiveBytes     float64 // live heap at end of this cycle
	Goal          float64 // heap goal for next cycle
	Trigger       float64 // heap size at which next mark phase starts
	HeapAtTrigger float64 // actual heap size when trigger was reached
	HeapAtEnd     float64 // heap size when marking finished
	Overshoot     float64 // HeapAtEnd - Goal (negative = undershoot)
	AssistRatio   float64 // scan bytes required per alloc byte during marking
	MarkDuration  float64 // estimated duration of the mark phase (seconds)
}

// Pacer simulates the GC pacer feedback loop.
type Pacer struct {
	cfg         Config
	smoothAlloc float64 // exponential moving average of alloc rate
	smoothScan  float64 // exponential moving average of scan rate
}

// NewPacer creates a Pacer with the given configuration.
func NewPacer(cfg Config) *Pacer {
	return &Pacer{
		cfg:         cfg,
		smoothAlloc: cfg.AllocRate,
		smoothScan:  cfg.ScanRate,
	}
}

const smoothAlpha = 0.5 // EMA weight for the current cycle

func (p *Pacer) heapGoal(live float64) float64 {
	if p.cfg.GOGC < 0 {
		// GOGC=off: goal is effectively unlimited (only GOMEMLIMIT applies).
		if p.cfg.MemLimit == math.MaxFloat64 {
			return math.MaxFloat64
		}
		return p.cfg.MemLimit
	}
	goal := live * (1.0 + float64(p.cfg.GOGC)/100.0)
	if p.cfg.MemLimit != math.MaxFloat64 && goal > p.cfg.MemLimit {
		goal = p.cfg.MemLimit
	}
	return goal
}

// SimulateCycles runs the pacer simulation for n cycles and returns one
// CycleReport per cycle. liveBytes is the initial live heap size.
func (p *Pacer) SimulateCycles(n int, liveBytes float64) []CycleReport {
	reports := make([]CycleReport, 0, n)
	live := liveBytes

	for i := 0; i < n; i++ {
		goal := p.heapGoal(live)

		// Estimated time to scan the live heap at the current scan rate.
		markDuration := live / (p.smoothScan * p.cfg.MarkCPUFrac)

		// Trigger: start marking early enough that it finishes at the goal.
		trigger := goal - p.smoothAlloc*markDuration
		if trigger < live {
			trigger = live // never trigger below the current live set
		}

		// Simulate: heap grows from trigger to heapAtEnd during marking.
		heapAtTrigger := trigger
		heapAtEnd := heapAtTrigger + p.smoothAlloc*markDuration

		// Assist ratio: if heap would overshoot, assists bridge the gap.
		allocBudget := goal - trigger
		assistRatio := 0.0
		if allocBudget > 0 && heapAtEnd > goal {
			excess := heapAtEnd - goal
			assistRatio = excess / allocBudget
		}

		overshoot := heapAtEnd - goal

		reports = append(reports, CycleReport{
			Cycle:         i + 1,
			LiveBytes:     live,
			Goal:          goal,
			Trigger:       trigger,
			HeapAtTrigger: heapAtTrigger,
			HeapAtEnd:     heapAtEnd,
			Overshoot:     overshoot,
			AssistRatio:   assistRatio,
			MarkDuration:  markDuration,
		})

		// After GC, the live heap grows by 10% per cycle (steady-state approximation).
		live = live * 1.1

		// Update EMA estimates.
		p.smoothAlloc = smoothAlpha*p.cfg.AllocRate + (1-smoothAlpha)*p.smoothAlloc
		p.smoothScan = smoothAlpha*p.cfg.ScanRate + (1-smoothAlpha)*p.smoothScan
	}

	return reports
}

// SetAllocRate updates the allocation rate for subsequent cycles (simulates
// a burst in allocation, e.g. a traffic spike).
func (p *Pacer) SetAllocRate(bytesPerSecond float64) {
	p.cfg.AllocRate = bytesPerSecond
}

// GoalForLive computes the heap goal for a given live heap size and the
// current pacer configuration. Exported for testing.
func (p *Pacer) GoalForLive(live float64) float64 {
	return p.heapGoal(live)
}
```

### Exercise 2: Example Function

An `Example` function must live in a `_test.go` file so that `go test`
compiles and verifies the `// Output:` block automatically. Add it to
`pacer_test.go` in the next exercise — not to `pacer.go`.

### Exercise 3: Tests

Create `pacer_test.go`:

```go
package gcpacer

import (
	"fmt"
	"math"
	"testing"
)

// ExamplePacer_GoalForLive demonstrates the heap goal formula.
// go test compiles and verifies the Output: comment automatically.
func ExamplePacer_GoalForLive() {
	p := NewPacer(DefaultConfig()) // GOGC=100
	goal := p.GoalForLive(100e6)   // 100 MB live
	fmt.Printf("goal = %.0f MB\n", goal/1e6)
	// Output:
	// goal = 200 MB
}

func TestGoalForLiveGOGC100(t *testing.T) {
	t.Parallel()

	p := NewPacer(DefaultConfig())
	got := p.GoalForLive(100e6)
	want := 200e6
	if math.Abs(got-want) > 1 {
		t.Errorf("GoalForLive(100 MB) = %.0f, want %.0f", got, want)
	}
}

func TestGoalForLiveGOGC200(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.GOGC = 200
	p := NewPacer(cfg)
	got := p.GoalForLive(100e6)
	want := 300e6 // 100 * (1 + 200/100)
	if math.Abs(got-want) > 1 {
		t.Errorf("GoalForLive(100 MB) with GOGC=200 = %.0f, want %.0f", got, want)
	}
}

func TestGoalForLiveGOGCOff(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.GOGC = -1
	p := NewPacer(cfg)
	got := p.GoalForLive(100e6)
	if got != math.MaxFloat64 {
		t.Errorf("GOGC=-1 with no MemLimit: goal = %.0f, want MaxFloat64", got)
	}
}

func TestGoalCappedByMemLimit(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	cfg.MemLimit = 150e6 // 150 MB
	p := NewPacer(cfg)
	// With 100 MB live and GOGC=100, uncapped goal would be 200 MB.
	got := p.GoalForLive(100e6)
	if got != 150e6 {
		t.Errorf("goal capped by MemLimit: got %.0f, want %.0f", got, cfg.MemLimit)
	}
}

func TestSimulateCyclesReturnsNReports(t *testing.T) {
	t.Parallel()

	p := NewPacer(DefaultConfig())
	reports := p.SimulateCycles(5, 50e6)
	if len(reports) != 5 {
		t.Errorf("len(reports) = %d, want 5", len(reports))
	}
}

func TestSimulateCyclesTriggerBelowGoal(t *testing.T) {
	t.Parallel()

	p := NewPacer(DefaultConfig())
	reports := p.SimulateCycles(10, 50e6)
	for _, r := range reports {
		if r.Trigger > r.Goal {
			t.Errorf("cycle %d: trigger (%.0f) > goal (%.0f)", r.Cycle, r.Trigger, r.Goal)
		}
	}
}

func TestSimulateCyclesCycleNumbersSequential(t *testing.T) {
	t.Parallel()

	p := NewPacer(DefaultConfig())
	reports := p.SimulateCycles(4, 50e6)
	for i, r := range reports {
		if r.Cycle != i+1 {
			t.Errorf("report[%d].Cycle = %d, want %d", i, r.Cycle, i+1)
		}
	}
}

func TestHigherAllocRateIncreasesAssistRatio(t *testing.T) {
	t.Parallel()

	// 10 MB/s: well below the 1 GB/s scan rate * 0.25 CPU = 250 MB/s effective
	// mark rate. The trigger is set so markDuration * 10MB/s fits inside
	// [live, goal]; heap ends at the goal exactly — no overshoot, no assists.
	cfgSlow := DefaultConfig()
	cfgSlow.AllocRate = 1e7 // 10 MB/s

	// 2.5 GB/s: the trigger formula gives goal - 2.5e9 * markDuration < live,
	// so it clamps to live. The heap then grows well past the goal during
	// marking, forcing assists to fire.
	cfgFast := DefaultConfig()
	cfgFast.AllocRate = 2.5e9 // 2.5 GB/s

	pSlow := NewPacer(cfgSlow)
	pFast := NewPacer(cfgFast)

	slowReports := pSlow.SimulateCycles(5, 50e6)
	fastReports := pFast.SimulateCycles(5, 50e6)

	// Sum assist ratios across cycles.
	var slowAssist, fastAssist float64
	for i := range slowReports {
		slowAssist += slowReports[i].AssistRatio
		fastAssist += fastReports[i].AssistRatio
	}
	if fastAssist == 0 {
		t.Errorf("fast allocator (2.5 GB/s) should produce non-zero assist, got %.6f", fastAssist)
	}
	if fastAssist <= slowAssist {
		t.Errorf("faster allocator should produce strictly more assist: slow=%.6f fast=%.6f",
			slowAssist, fastAssist)
	}
	t.Logf("assist totals: slow(10MB/s)=%.6f fast(2.5GB/s)=%.6f", slowAssist, fastAssist)
}

// Your turn: write TestGOGCOFFWithMemLimitUsesMemLimit that sets GOGC=-1,
// MemLimit=200e6, and asserts that GoalForLive(100e6) returns exactly 200e6.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime/metrics"

	"example.com/gcpacer"
)

func main() {
	fmt.Println("GC pacer simulation")
	fmt.Println()

	cfg := gcpacer.DefaultConfig()
	p := gcpacer.NewPacer(cfg)

	fmt.Printf("Config: GOGC=%d  AllocRate=%.0f MB/s  ScanRate=%.0f MB/s\n",
		cfg.GOGC, cfg.AllocRate/1e6, cfg.ScanRate/1e6)
	fmt.Println()
	fmt.Printf("  %-6s  %-10s  %-10s  %-10s  %-10s  %-10s\n",
		"cycle", "live MB", "goal MB", "trigger MB", "end MB", "overshoot MB")

	reports := p.SimulateCycles(8, 50e6)
	for _, r := range reports {
		fmt.Printf("  %-6d  %-10.1f  %-10.1f  %-10.1f  %-10.1f  %-10.1f\n",
			r.Cycle,
			r.LiveBytes/1e6,
			r.Goal/1e6,
			r.Trigger/1e6,
			r.HeapAtEnd/1e6,
			r.Overshoot/1e6,
		)
	}

	// Simulate a 60x allocation burst (50 MB/s -> 3 GB/s). At 3 GB/s the
	// trigger formula would set trigger below live, so it clamps to live and
	// the heap overshoots the goal by a large margin — GC assists fire.
	p2 := gcpacer.NewPacer(cfg)
	p2.SetAllocRate(3e9) // 60x burst: 3 GB/s
	fmt.Println()
	fmt.Println("After 60x allocation burst (3 GB/s):")
	burstReports := p2.SimulateCycles(5, 50e6)
	for _, r := range burstReports {
		fmt.Printf("  cycle %d: assist_ratio=%.3f  overshoot=%.1f MB\n",
			r.Cycle, r.AssistRatio, r.Overshoot/1e6)
	}

	// Read the real pacer goal from the running process.
	fmt.Println()
	fmt.Println("Real runtime/metrics:")
	samples := []metrics.Sample{
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(samples)
	for _, s := range samples {
		if s.Value.Kind() == metrics.KindUint64 {
			fmt.Printf("  %s = %d\n", s.Name, s.Value.Uint64())
		}
	}
}
```

## Common Mistakes

### Confusing Trigger and Goal

Wrong: thinking the GC starts when the heap reaches the goal.

What happens: the heap would overshoot the goal before marking finishes, because marking takes time and allocation continues during it.

Fix: the GC starts at the trigger, which is set below the goal. The pacer aims to have marking finish exactly when the heap hits the goal.

### Expecting the Pacer to Be a Hard Stop

Wrong: expecting that GOGC=100 means the heap will never exceed 2x live.

What happens: if allocation rate spikes faster than the pacer estimated, the heap can overshoot the goal. GC assists try to prevent this but cannot guarantee it.

Fix: understand the pacer as a proportional controller: it converges to the goal on average, not on every cycle. For hard ceilings, use GOMEMLIMIT.

### Ignoring GC Assist Latency When Analysing Pacer Behaviour

Wrong: evaluating pacer health only by looking at GC cycle count and heap size.

What happens: a pacer that runs infrequently but causes heavy GC assist work on every cycle can produce worse tail latency than a pacer that runs more frequently with lighter assists.

Fix: monitor both `/gc/cycles/total:gc-cycles` (frequency) and `/sched/latencies:seconds` (scheduling latency, which includes assist delays).

### Setting GOGC=off Without Understanding Assist Behavior

Wrong: setting `GOGC=-1` expecting zero GC assist overhead.

What happens: with `GOGC=off` and `GOMEMLIMIT` set, the pacer still triggers cycles when the limit is approached, and assists still run if allocation is fast enough.

Fix: `GOGC=off` disables ratio-triggered cycles, not GOMEMLIMIT-triggered ones. Assists can still run in GOMEMLIMIT-triggered cycles.

## Verification

From `~/go-exercises/gcpacer`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

## Summary

- The pacer sets the trigger point so marking finishes when the heap reaches the goal.
- Heap goal: `live * (1 + GOGC/100)`, capped by GOMEMLIMIT.
- Trigger: `goal - alloc_rate * estimated_mark_duration`.
- GC assists bridge the gap when allocation outpaces the mark rate estimate.
- The pacer uses EMA to smooth allocation and scan rate estimates across cycles.
- Observe real pacer state via `/gc/heap/goal:bytes` and `/gc/heap/live:bytes` in `runtime/metrics`.

## What's Next

Next: [Soft Memory Limit](../07-soft-memory-limit/07-soft-memory-limit.md).

## Resources

- [Go 1.18 GC Pacer Redesign](https://github.com/golang/proposal/blob/master/design/44167-gc-pacer-redesign.md) — the definitive design document for the current pacer algorithm
- [Go GC Guide](https://tip.golang.org/doc/gc-guide) — GOGC, GOMEMLIMIT, and pacer interaction
- [runtime/metrics](https://pkg.go.dev/runtime/metrics) — heap goal and live heap metrics
- [Go runtime mgc.go](https://github.com/golang/go/blob/master/src/runtime/mgc.go) — the actual pacer implementation
