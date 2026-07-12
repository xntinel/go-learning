# Exercise 5: The Watermark Lag Monitor

Watermark lag — the gap between wall-clock time and the watermark — is the single most important health signal of an event-time pipeline. This exercise builds a monitor that classifies a pipeline as healthy, lagging, or stalled from an injected clock, drawing the crucial distinction a single threshold cannot: a watermark that trails but keeps moving needs more capacity, while a watermark that has frozen needs someone to find the stuck source.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
monitor.go             Health, Monitor (Observe, MaxLag, Watermark)
cmd/
  demo/
    main.go            healthy -> lagging -> stalled progression on an injected clock
monitor_test.go        healthy, lagging, stalled, precedence, max-lag, regression
example_test.go        runnable doc example for Observe
```

- Files: `monitor.go`, `cmd/demo/main.go`, `monitor_test.go`, `example_test.go`.
- Implement: `Monitor` with `Observe(watermark, now) Health`, `MaxLag()`, and `Watermark()`; a `Health` enum of healthy, lagging, stalled with a `String` method.
- Test: a fresh, advancing watermark is healthy; a watermark that advances but trails beyond the lag threshold is lagging; a watermark that has not advanced beyond the stall timeout is stalled; stalled takes precedence over lagging; the maximum observed lag is tracked; a regressing watermark is ignored.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/04-watermarks-late-data/05-watermark-lag-monitor/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/04-watermarks-late-data/05-watermark-lag-monitor
go mod edit -go=1.26
```

### Two failure signatures, not one threshold

Lag is `now - watermark`. In a healthy pipeline it sits near the out-of-orderness bound plus a small processing delay and stays bounded. The naive monitor alerts when lag crosses a threshold — but that conflates two failures that demand opposite responses. If the watermark keeps advancing yet trails the wall clock by a growing margin, the pipeline is *lagging*: it is making progress but the backlog is building, and the fix is usually more parallelism. If the watermark stops advancing entirely while wall-clock time keeps moving, the pipeline is *stalled*: a source has gone silent or an operator is wedged, and the fix is to find and unblock it. Both show a large lag; only the second has a frozen watermark.

The monitor distinguishes them with one extra piece of state: the processing time at which the watermark last advanced. `Observe` updates the stored watermark and that `lastAdvance` timestamp only when the incoming watermark is strictly greater — a regressing or repeated watermark leaves `lastAdvance` untouched. Health is then a two-test cascade evaluated in priority order: if the time since the last advance exceeds the stall timeout, the pipeline is stalled regardless of the absolute lag; otherwise, if the current lag exceeds the lag threshold, it is lagging; otherwise it is healthy. Stalled is checked first because a frozen watermark inevitably also shows a large lag, and reporting "lagging" for a frozen pipeline would point the on-call engineer at the wrong remedy.

Because the clock is injected as the `now` argument rather than read from `time.Now`, the monitor is fully deterministic and its state machine can be driven through every transition by a fixed script of `(watermark, now)` pairs.

Create `monitor.go`:

```go
// Package lagmon classifies the health of an event-time pipeline from its
// watermark lag. It distinguishes a pipeline that is merely lagging (watermark
// advancing but behind wall-clock time) from one that is stalled (watermark
// frozen). The clock is injected, so the monitor is deterministic.
package lagmon

import "time"

// Health is the classified state of a pipeline's watermark.
type Health int

const (
	HealthHealthy Health = iota // lag within threshold and watermark advancing
	HealthLagging               // watermark advancing but lag exceeds threshold
	HealthStalled               // watermark has not advanced within the stall timeout
)

// String returns the lowercase name of the health state.
func (h Health) String() string {
	switch h {
	case HealthHealthy:
		return "healthy"
	case HealthLagging:
		return "lagging"
	case HealthStalled:
		return "stalled"
	default:
		return "unknown"
	}
}

// Monitor classifies watermark lag. It is not safe for concurrent use: drive it
// from the single goroutine that advances the watermark.
type Monitor struct {
	lagThreshold time.Duration
	stallTimeout time.Duration
	started      bool
	watermark    time.Time
	lastAdvance  time.Time // processing time when the watermark last advanced
	maxLag       time.Duration
}

// NewMonitor creates a Monitor. lagThreshold is the lag above which a still-
// advancing pipeline is reported as lagging. stallTimeout is how long the
// watermark may fail to advance before the pipeline is reported as stalled.
func NewMonitor(lagThreshold, stallTimeout time.Duration) *Monitor {
	return &Monitor{
		lagThreshold: lagThreshold,
		stallTimeout: stallTimeout,
	}
}

// Observe records the current watermark at processing time now and returns the
// pipeline health. The watermark and the last-advance time are updated only
// when the watermark strictly advances, so a regressing or repeated watermark
// counts toward the stall timeout.
func (m *Monitor) Observe(watermark, now time.Time) Health {
	if !m.started {
		m.started = true
		m.watermark = watermark
		m.lastAdvance = now
	} else if watermark.After(m.watermark) {
		m.watermark = watermark
		m.lastAdvance = now
	}

	lag := now.Sub(m.watermark)
	if lag > m.maxLag {
		m.maxLag = lag
	}

	switch {
	case now.Sub(m.lastAdvance) > m.stallTimeout:
		return HealthStalled
	case lag > m.lagThreshold:
		return HealthLagging
	default:
		return HealthHealthy
	}
}

// MaxLag returns the largest lag observed across all calls to Observe.
func (m *Monitor) MaxLag() time.Duration {
	return m.maxLag
}

// Watermark returns the most recent (highest) watermark observed.
func (m *Monitor) Watermark() time.Time {
	return m.watermark
}
```

The cascade order encodes the priority: stalled is tested before lagging because the two are not mutually exclusive — a frozen watermark accrues lag too — and the more specific, more actionable classification must win. Updating `lastAdvance` only on a strict advance is what makes a repeated watermark (the source went quiet but keeps re-asserting the same value) count as no progress and eventually trip the stall timeout.

### The runnable demo

The demo drives one monitor through the full lifecycle on an injected clock: two healthy observations where the watermark keeps pace, one where it advances but falls behind the lag threshold, and two where it freezes long enough to be declared stalled.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/watermark-lag-monitor"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const f = "15:04:05"
	m := lagmon.NewMonitor(5*time.Second, 10*time.Second)

	steps := []struct {
		wm  time.Duration // watermark offset from base
		now time.Duration // processing-time offset from base
	}{
		{0, 1 * time.Second},                // fresh, lag 1s
		{5 * time.Second, 6 * time.Second},  // advancing, lag 1s
		{6 * time.Second, 13 * time.Second}, // advanced, lag 7s > threshold
		{6 * time.Second, 20 * time.Second}, // frozen, lag 14s, 7s since advance
		{6 * time.Second, 25 * time.Second}, // frozen 12s > stall timeout
	}

	for _, s := range steps {
		wm := base.Add(s.wm)
		now := base.Add(s.now)
		health := m.Observe(wm, now)
		fmt.Printf("now=%s wm=%s lag=%s health=%s\n",
			now.UTC().Format(f), wm.UTC().Format(f), now.Sub(wm), health)
	}

	fmt.Printf("max lag observed: %s\n", m.MaxLag())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
now=00:00:01 wm=00:00:00 lag=1s health=healthy
now=00:00:06 wm=00:00:05 lag=1s health=healthy
now=00:00:13 wm=00:00:06 lag=7s health=lagging
now=00:00:20 wm=00:00:06 lag=14s health=lagging
now=00:00:25 wm=00:00:06 lag=19s health=stalled
```

The watermark advances to 6s at processing time 13s and then freezes. At 20s only 7s have passed since the last advance, under the 10s stall timeout, so the pipeline is still merely lagging. By 25s the watermark has been frozen for 12s, past the timeout, so it is reclassified as stalled even though its lag grew continuously across all three late observations. The max lag of 19s is the worst gap seen.

### Tests

`TestHealthyWhenAdvancing` checks the nominal case. `TestLaggingWhenBehindButAdvancing` proves a trailing-but-moving watermark is lagging, not stalled. `TestStalledWhenFrozen` proves a frozen watermark trips the stall timeout. `TestStallTakesPrecedence` proves stalled wins over lagging when both apply. `TestMaxLagTracked` checks the running maximum. `TestRegressingWatermarkIgnored` proves an out-of-order lower watermark neither lowers the stored watermark nor resets the advance clock.

Create `monitor_test.go`:

```go
package lagmon

import (
	"testing"
	"time"
)

var base = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestHealthyWhenAdvancing(t *testing.T) {
	t.Parallel()
	m := NewMonitor(5*time.Second, 10*time.Second)
	if h := m.Observe(base, base.Add(1*time.Second)); h != HealthHealthy {
		t.Fatalf("health = %s, want healthy", h)
	}
	if h := m.Observe(base.Add(5*time.Second), base.Add(6*time.Second)); h != HealthHealthy {
		t.Fatalf("health = %s, want healthy", h)
	}
}

func TestLaggingWhenBehindButAdvancing(t *testing.T) {
	t.Parallel()
	m := NewMonitor(5*time.Second, 10*time.Second)
	m.Observe(base, base.Add(1*time.Second))
	// Watermark advances to 6s, but now is 13s: lag 7s > 5s threshold, and the
	// watermark just advanced so it is not stalled.
	if h := m.Observe(base.Add(6*time.Second), base.Add(13*time.Second)); h != HealthLagging {
		t.Fatalf("health = %s, want lagging", h)
	}
}

func TestStalledWhenFrozen(t *testing.T) {
	t.Parallel()
	m := NewMonitor(5*time.Second, 10*time.Second)
	m.Observe(base.Add(6*time.Second), base.Add(6*time.Second)) // lastAdvance = 6s
	// Same watermark, 17s later: 11s since the last advance > 10s stall timeout.
	if h := m.Observe(base.Add(6*time.Second), base.Add(17*time.Second)); h != HealthStalled {
		t.Fatalf("health = %s, want stalled", h)
	}
}

func TestStallTakesPrecedence(t *testing.T) {
	t.Parallel()
	m := NewMonitor(5*time.Second, 10*time.Second)
	m.Observe(base, base) // lastAdvance = base, lag 0
	// Frozen for 12s (> stall timeout) and lag 12s (> lag threshold): both apply,
	// stalled must win.
	if h := m.Observe(base, base.Add(12*time.Second)); h != HealthStalled {
		t.Fatalf("health = %s, want stalled (precedence over lagging)", h)
	}
}

func TestMaxLagTracked(t *testing.T) {
	t.Parallel()
	m := NewMonitor(5*time.Second, time.Hour)
	m.Observe(base, base.Add(2*time.Second))                    // lag 2s
	m.Observe(base.Add(1*time.Second), base.Add(9*time.Second)) // lag 8s
	m.Observe(base.Add(2*time.Second), base.Add(7*time.Second)) // lag 5s
	if m.MaxLag() != 8*time.Second {
		t.Fatalf("MaxLag = %s, want 8s", m.MaxLag())
	}
}

func TestRegressingWatermarkIgnored(t *testing.T) {
	t.Parallel()
	m := NewMonitor(5*time.Second, time.Hour)
	m.Observe(base.Add(10*time.Second), base.Add(11*time.Second))
	// A lower watermark must not replace the stored one.
	m.Observe(base.Add(4*time.Second), base.Add(12*time.Second))
	if !m.Watermark().Equal(base.Add(10 * time.Second)) {
		t.Fatalf("watermark = %s, want 00:00:10 (regression ignored)", m.Watermark().UTC().Format("15:04:05"))
	}
}
```

Create `example_test.go`:

```go
package lagmon_test

import (
	"fmt"
	"time"

	"example.com/watermark-lag-monitor"
)

func ExampleMonitor_Observe() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	m := lagmon.NewMonitor(5*time.Second, 10*time.Second)

	fmt.Println(m.Observe(base, base.Add(1*time.Second)))                     // lag 1s
	fmt.Println(m.Observe(base.Add(6*time.Second), base.Add(13*time.Second))) // lag 7s, advancing
	fmt.Println(m.Observe(base.Add(6*time.Second), base.Add(25*time.Second))) // frozen 12s
	// Output:
	// healthy
	// lagging
	// stalled
}
```

## Review

The monitor is correct when it separates the two failure signatures a single lag threshold blurs together. The central mistake is alerting only on absolute lag: a frozen pipeline and a slow-but-moving one can show the same lag yet need opposite fixes, so the monitor tracks when the watermark last advanced and tests the stall condition first. The second mistake is updating `lastAdvance` on every observation rather than only on a strict advance — doing so would reset the stall clock each time the source re-asserts the same watermark, and the pipeline would never be reported as stalled. The third is letting a regressing watermark overwrite the stored one; `Observe` only ever moves the watermark forward, matching the monotonicity the upstream tracker guarantees. Because `now` is a parameter, the entire state machine is deterministic and every transition is reachable from a fixed test script with no sleeps and no flakiness.

## Resources

- [Apache Flink: Monitoring Watermarks](https://nightlies.apache.org/flink/flink-docs-stable/docs/concepts/time/#idleness-and-watermark-lag) — watermark lag and idleness as the operational signals of a healthy event-time pipeline.
- [Streaming Systems (Akidau, Chernyak, Lax)](https://www.oreilly.com/library/view/streaming-systems/9781491983867/) — watermark progress, lag, and the operational meaning of a stalled watermark.
- [time.Duration](https://pkg.go.dev/time#Duration) — the lag and timeout arithmetic the monitor performs on injected clock values.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-event-time-windows.md](04-event-time-windows.md) | Next: [../05-checkpointing/00-concepts.md](../05-checkpointing/00-concepts.md)
