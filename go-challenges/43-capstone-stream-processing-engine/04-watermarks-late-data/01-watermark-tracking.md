# Exercise 1: Per-Source and Global Watermark Tracking

A watermark pipeline starts with two cooperating pieces: a per-source tracker that turns a stream of event timestamps into a bounded-out-of-orderness watermark, and a global tracker that combines the per-source watermarks into one monotonically advancing pipeline watermark by taking the minimum across active sources. This exercise builds both, lock-free where it matters, with idle-source exclusion so one silent partition cannot freeze the whole pipeline.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
watermark.go           Window, Record, IsLate, SourceTracker, GlobalTracker, ErrNoActiveSources
cmd/
  demo/
    main.go            two-source pipeline; min-across-partitions; idle exclusion
watermark_test.go      per-source advance + monotonicity, global minimum, idle exclusion
example_test.go        runnable doc examples for IsLate and SourceTracker
```

- Files: `watermark.go`, `cmd/demo/main.go`, `watermark_test.go`, `example_test.go`.
- Implement: `SourceTracker` with `Observe`, `Watermark`, `IsIdle`; `GlobalTracker` with `AddSource`, `Advance`, `Global`; plus `Window`, `Record`, and the `IsLate` predicate.
- Test: per-source watermark starts at zero, advances on the maximum observed timestamp, never regresses on an older observation; the global watermark is the minimum across active sources, stays monotonic when a slow source is added, and excludes idle sources.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/04-watermarks-late-data/01-watermark-tracking/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/04-watermarks-late-data/01-watermark-tracking
go mod edit -go=1.26
```

### Why the per-source watermark is max-minus-bound, and why that is monotonic

The per-source watermark answers a single question: given everything this partition has shown me, what is the latest event time I am willing to declare "complete"? The answer is the largest event timestamp seen so far, minus a fixed out-of-orderness bound. The bound is the slack: it is how far behind the maximum the watermark sits so that records that are merely a little late still land before their window closes. Subtracting the bound from the *maximum* is what makes the watermark naturally monotonic — the maximum only ever grows, so an out-of-order record with a small timestamp never lowers it, and therefore never lowers the watermark. `Observe` enforces this with a CAS loop that only ever raises `maxObservedMs`; a smaller incoming timestamp is dropped.

Reads of the watermark are lock-free. `maxObservedMs` and `lastActivityMs` are both `atomic.Int64`, so `Watermark()` and `IsIdle()` can be called from any goroutine at any time without taking a lock, which matters because in a real engine the watermark is read far more often than it is advanced. A source that has observed nothing reports the zero time, which the global tracker treats as "this source has no opinion yet" rather than "this source's watermark is the beginning of time."

### Why the global watermark is the minimum, and how idle sources are excluded

A window can close only when *every* source has passed its boundary, because any source could still deliver an in-time record. So the global watermark is the minimum of the per-source watermarks — the slowest source sets the pace. That is correct but fragile: a single partition that stops producing would pin the minimum forever. `IsIdle` breaks the deadlock by excluding any source that has not been observed within its idle timeout, on the assumption that a silent source has either finished or stalled outside the pipeline. The `Advance` loop skips idle sources and sources with no events, counts the contributors, and returns `ErrNoActiveSources` when none remain so the caller can hold rather than fire on a meaningless watermark.

The final clamp in `Advance` is the monotonicity guarantee. The raw minimum can fall — for example when a new, slow source is registered on a running pipeline — but the global watermark must not, because results for earlier windows have already been emitted. The CAS loop refuses any proposed value that is not strictly greater than the current global, so the watermark is `max(old, new_minimum)` and never travels backwards.

Create `watermark.go`:

```go
// Package watermark tracks per-source and global event-time watermarks for a
// stream-processing pipeline. A per-source watermark lags the maximum observed
// event timestamp by a fixed out-of-orderness bound; the global watermark is
// the minimum across active sources and advances monotonically.
package watermark

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNoActiveSources is returned by GlobalTracker.Advance when all sources are
// idle or have not observed any events yet.
var ErrNoActiveSources = errors.New("watermark: no active sources with observed events")

// RecordKind distinguishes normal output records from corrections emitted in
// accumulating-and-retracting mode by downstream operators.
type RecordKind int

const (
	KindNormal     RecordKind = iota // standard aggregation result
	KindRetraction                   // negation of a previously emitted result
	KindUpdate                       // replacement value following a retraction
)

// Window is a half-open time interval [Start, End).
type Window struct {
	Start time.Time
	End   time.Time
}

// Contains reports whether t falls within [Start, End).
func (w Window) Contains(t time.Time) bool {
	return !t.Before(w.Start) && t.Before(w.End)
}

// String returns a human-readable label for the window.
func (w Window) String() string {
	const f = "15:04:05"
	return fmt.Sprintf("[%s, %s)", w.Start.Format(f), w.End.Format(f))
}

// Record is a value flowing through the pipeline. Kind distinguishes normal
// records from retractions and updates.
type Record struct {
	Value     int64
	EventTime time.Time
	Kind      RecordKind
	Window    Window
}

// IsLate reports whether r arrived after the watermark has passed its window
// boundary (watermark >= window.End).
func IsLate(r Record, watermark time.Time) bool {
	return !watermark.Before(r.Window.End)
}

// SourceTracker tracks the watermark for one input partition. Reads of the
// current watermark are lock-free: maxObservedMs and lastActivityMs are both
// atomic.Int64 values.
type SourceTracker struct {
	maxObservedMs  atomic.Int64  // UnixMilli of the maximum event timestamp seen
	lastActivityMs atomic.Int64  // UnixMilli of the last call to Observe
	outOfOrderness time.Duration // subtracted from max to form the watermark
	idleTimeout    time.Duration // after this idle period the source is excluded
}

// NewSourceTracker creates a tracker for one source partition. outOfOrderness is
// the expected bound on out-of-order arrival: the source watermark lags the
// maximum observed timestamp by this amount so in-flight records have time to
// arrive before their window closes. idleTimeout controls how long a source may
// be quiet before it is excluded from the global minimum.
func NewSourceTracker(outOfOrderness, idleTimeout time.Duration) *SourceTracker {
	st := &SourceTracker{
		outOfOrderness: outOfOrderness,
		idleTimeout:    idleTimeout,
	}
	st.maxObservedMs.Store(math.MinInt64)
	st.lastActivityMs.Store(time.Now().UnixMilli())
	return st
}

// Observe records an event timestamp and advances the per-source watermark if
// the new timestamp is greater than the previous maximum. Multiple goroutines
// may call Observe concurrently without additional synchronization.
func (st *SourceTracker) Observe(eventTime time.Time) {
	ms := eventTime.UnixMilli()
	for {
		old := st.maxObservedMs.Load()
		if ms <= old {
			break
		}
		if st.maxObservedMs.CompareAndSwap(old, ms) {
			break
		}
	}
	st.lastActivityMs.Store(time.Now().UnixMilli())
}

// Watermark returns this source's current watermark:
// max_observed_event_time - outOfOrderness. It returns the zero time if no
// events have been observed yet.
func (st *SourceTracker) Watermark() time.Time {
	ms := st.maxObservedMs.Load()
	if ms == math.MinInt64 {
		return time.Time{}
	}
	return time.UnixMilli(ms).Add(-st.outOfOrderness)
}

// IsIdle reports whether the source has received no records for longer than its
// idleTimeout.
func (st *SourceTracker) IsIdle(now time.Time) bool {
	last := time.UnixMilli(st.lastActivityMs.Load())
	return now.Sub(last) > st.idleTimeout
}

// GlobalTracker computes the global watermark as the minimum watermark across
// all active (non-idle) source trackers and enforces strict monotonicity.
type GlobalTracker struct {
	mu       sync.RWMutex
	sources  map[string]*SourceTracker
	globalMs atomic.Int64 // monotonically non-decreasing; math.MinInt64 = no events yet
}

// NewGlobalTracker creates a GlobalTracker with no sources registered. Call
// AddSource before the first call to Advance.
func NewGlobalTracker() *GlobalTracker {
	gt := &GlobalTracker{
		sources: make(map[string]*SourceTracker),
	}
	gt.globalMs.Store(math.MinInt64)
	return gt
}

// AddSource registers a SourceTracker under the given partition identifier. It
// is safe to call AddSource while Advance is running on another goroutine.
func (gt *GlobalTracker) AddSource(id string, st *SourceTracker) {
	gt.mu.Lock()
	defer gt.mu.Unlock()
	gt.sources[id] = st
}

// Advance recomputes the global watermark from all active sources and advances
// it monotonically. now is used for idle detection; pass time.Now() in
// production. Returns (zero, ErrNoActiveSources) if all sources are idle or have
// not observed any events.
func (gt *GlobalTracker) Advance(now time.Time) (time.Time, error) {
	gt.mu.RLock()
	sources := make([]*SourceTracker, 0, len(gt.sources))
	for _, st := range gt.sources {
		sources = append(sources, st)
	}
	gt.mu.RUnlock()

	minMs := int64(math.MaxInt64)
	active := 0
	for _, st := range sources {
		if st.IsIdle(now) {
			continue
		}
		wm := st.Watermark()
		if wm.IsZero() {
			continue // no events observed yet on this source
		}
		active++
		if ms := wm.UnixMilli(); ms < minMs {
			minMs = ms
		}
	}
	if active == 0 {
		return time.Time{}, ErrNoActiveSources
	}

	// Enforce monotonicity: the global watermark must never decrease.
	for {
		old := gt.globalMs.Load()
		if minMs <= old {
			return time.UnixMilli(old), nil
		}
		if gt.globalMs.CompareAndSwap(old, minMs) {
			return time.UnixMilli(minMs), nil
		}
	}
}

// Global returns the current global watermark without recomputing it. Returns
// the zero time if Advance has never advanced past the initial state.
func (gt *GlobalTracker) Global() time.Time {
	ms := gt.globalMs.Load()
	if ms == math.MinInt64 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
```

`GlobalTracker` holds its read lock only while copying the source slice, then releases it before the computation loop so the per-source watermark reads do not contend on the map lock. The two CAS loops — one in `Observe`, one in `Advance` — are the only places a value is mutated under concurrency, and both only ever move their counter in one direction.

### The runnable demo

The demo builds a two-source pipeline where one partition has progressed to 30 seconds and a slow partition only to 10 seconds. The global watermark is held back by the slow partition: `min(30s - 2s, 10s - 2s) = 8s`. Then the fast source is allowed to go idle, and the global watermark jumps to the slow source's watermark because the idle one is excluded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/watermark-tracking"
)

func main() {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	gt := watermark.NewGlobalTracker()
	fast := watermark.NewSourceTracker(2*time.Second, 50*time.Millisecond)
	slow := watermark.NewSourceTracker(2*time.Second, time.Hour)
	gt.AddSource("partition-fast", fast)
	gt.AddSource("partition-slow", slow)

	fast.Observe(base.Add(30 * time.Second))
	slow.Observe(base.Add(10 * time.Second))

	// global = min(fast.wm = 30s-2s = 28s, slow.wm = 10s-2s = 8s) = 8s.
	wm, err := gt.Advance(time.Now())
	if err != nil {
		panic(err)
	}
	fmt.Printf("both active:  global watermark %s (slow partition limits progress)\n",
		wm.UTC().Format("15:04:05"))

	// Let the fast source go idle (its idleTimeout is 50ms).
	time.Sleep(80 * time.Millisecond)

	// Only the slow source contributes now; the global jumps to its watermark.
	wm, err = gt.Advance(time.Now())
	if err != nil {
		panic(err)
	}
	fmt.Printf("fast idle:    global watermark %s (idle source excluded)\n",
		wm.UTC().Format("15:04:05"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
both active:  global watermark 12:00:08 (slow partition limits progress)
fast idle:    global watermark 12:00:08 (idle source excluded)
```

The global watermark prints `12:00:08` both times, but for different reasons: first because the slow source pins the minimum at 8s, then because the fast source is excluded as idle and the slow source's own watermark is also 8s. The value did not regress when the fast (higher) source dropped out — monotonicity held.

### Tests

The suite pins each behaviour. `TestSourceTrackerWatermarkStartsZero` checks the no-events case. `TestSourceTrackerAdvancesOnObserve` checks the max-minus-bound formula. `TestSourceTrackerMonotonicity` feeds an older timestamp after a newer one and asserts the watermark does not regress. `TestGlobalWatermarkIsMinimum` proves the slowest source wins; `TestGlobalWatermarkMonotonicity` adds a slow source after the fact and asserts the global does not go backwards. `TestGlobalNoActiveSources` checks the sentinel error, and `TestIdleSourceExcluded` proves a quiet source is dropped from the minimum.

Create `watermark_test.go`:

```go
package watermark

import (
	"errors"
	"testing"
	"time"
)

var (
	base = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	w10  = Window{Start: base, End: base.Add(10 * time.Second)}
)

func TestSourceTrackerWatermarkStartsZero(t *testing.T) {
	t.Parallel()
	st := NewSourceTracker(5*time.Second, time.Minute)
	if !st.Watermark().IsZero() {
		t.Fatal("watermark must be zero before any events are observed")
	}
}

func TestSourceTrackerAdvancesOnObserve(t *testing.T) {
	t.Parallel()
	st := NewSourceTracker(5*time.Second, time.Minute)
	t0 := base.Add(10 * time.Second)
	st.Observe(t0)
	want := t0.Add(-5 * time.Second)
	if !st.Watermark().Equal(want) {
		t.Fatalf("watermark = %v, want %v", st.Watermark(), want)
	}
}

func TestSourceTrackerMonotonicity(t *testing.T) {
	t.Parallel()
	st := NewSourceTracker(0, time.Minute)
	t0 := base.Add(10 * time.Second)
	t1 := base.Add(5 * time.Second) // earlier; must not regress watermark
	st.Observe(t0)
	st.Observe(t1)
	if !st.Watermark().Equal(t0) {
		t.Fatalf("watermark regressed to %v after older observation; want %v", st.Watermark(), t0)
	}
}

func TestIsLate(t *testing.T) {
	t.Parallel()
	r := Record{Window: w10}
	cases := []struct {
		wm   time.Time
		want bool
	}{
		{base.Add(8 * time.Second), false}, // wm < window.End -> not late
		{base.Add(10 * time.Second), true}, // wm == window.End -> late
		{base.Add(12 * time.Second), true}, // wm > window.End -> late
	}
	for _, tc := range cases {
		got := IsLate(r, tc.wm)
		if got != tc.want {
			t.Errorf("IsLate(wm=%v) = %v, want %v", tc.wm, got, tc.want)
		}
	}
}

func TestGlobalWatermarkIsMinimum(t *testing.T) {
	t.Parallel()
	gt := NewGlobalTracker()
	s1 := NewSourceTracker(0, time.Minute)
	s2 := NewSourceTracker(0, time.Minute)
	gt.AddSource("s1", s1)
	gt.AddSource("s2", s2)

	s1.Observe(base.Add(20 * time.Second))
	s2.Observe(base.Add(10 * time.Second))

	wm, err := gt.Advance(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// global watermark = min(s1.wm=20s, s2.wm=10s) = 10s
	if !wm.Equal(base.Add(10 * time.Second)) {
		t.Fatalf("global watermark = %v, want %v", wm, base.Add(10*time.Second))
	}
}

func TestGlobalWatermarkMonotonicity(t *testing.T) {
	t.Parallel()
	gt := NewGlobalTracker()
	s1 := NewSourceTracker(0, time.Minute)
	gt.AddSource("s1", s1)
	s1.Observe(base.Add(10 * time.Second))
	wm1, _ := gt.Advance(time.Now())

	// Add a slow second source that would pull the min backwards.
	s2 := NewSourceTracker(0, time.Minute)
	gt.AddSource("s2", s2)
	s2.Observe(base.Add(2 * time.Second)) // much older than wm1
	wm2, _ := gt.Advance(time.Now())

	if wm2.Before(wm1) {
		t.Fatalf("global watermark went backwards: %v then %v", wm1, wm2)
	}
}

func TestGlobalNoActiveSources(t *testing.T) {
	t.Parallel()
	gt := NewGlobalTracker()
	_, err := gt.Advance(time.Now())
	if !errors.Is(err, ErrNoActiveSources) {
		t.Fatalf("err = %v, want ErrNoActiveSources", err)
	}
}

func TestIdleSourceExcluded(t *testing.T) {
	t.Parallel()
	gt := NewGlobalTracker()
	fast := NewSourceTracker(0, time.Millisecond) // goes idle after 1ms
	slow := NewSourceTracker(0, time.Minute)
	gt.AddSource("fast", fast)
	gt.AddSource("slow", slow)

	fast.Observe(base.Add(20 * time.Second))
	slow.Observe(base.Add(5 * time.Second))

	time.Sleep(5 * time.Millisecond) // fast becomes idle

	wm, err := gt.Advance(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Only slow is active; global watermark = slow.wm = base+5s.
	if !wm.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("watermark = %v, want %v (idle source excluded)", wm, base.Add(5*time.Second))
	}
}
```

Create `example_test.go`:

```go
package watermark_test

import (
	"fmt"
	"time"

	"example.com/watermark-tracking"
)

func ExampleIsLate() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	w := watermark.Window{Start: base, End: base.Add(10 * time.Second)}
	r := watermark.Record{Window: w}

	fmt.Println(watermark.IsLate(r, base.Add(8*time.Second)))  // watermark before window.End
	fmt.Println(watermark.IsLate(r, base.Add(10*time.Second))) // watermark at window.End
	fmt.Println(watermark.IsLate(r, base.Add(12*time.Second))) // watermark past window.End
	// Output:
	// false
	// true
	// true
}

func ExampleSourceTracker_Watermark() {
	st := watermark.NewSourceTracker(5*time.Second, time.Minute)
	t0 := time.Date(2024, 1, 1, 0, 0, 10, 0, time.UTC)
	st.Observe(t0)
	want := t0.Add(-5 * time.Second)
	fmt.Println(st.Watermark().Equal(want))
	// Output:
	// true
}
```

## Review

The tracker is correct when the per-source watermark only ever rises and the global watermark only ever rises. The most common error is feeding an unobserved source (watermark zero) into the minimum, which drags the global to the zero time and prevents any window from firing; the `wm.IsZero()` skip and the `active` counter guard against it. The second error is recomputing the raw minimum and returning it directly, which lets a freshly added slow source pull the global backwards; the CAS clamp `if minMs <= old { return old }` is the fix, and `TestGlobalWatermarkMonotonicity` is the proof. The third is reading `maxObservedMs` non-atomically — under `go test -race` the concurrent `Observe`/`Watermark` calls would report a data race, which is why both fields are `atomic.Int64`. Idle exclusion is the escape hatch that keeps one silent partition from pinning the pipeline; without it, a quiet Kafka partition would hold the global watermark at its last value indefinitely.

## Resources

- [The Dataflow Model (Akidau et al., VLDB 2015)](https://research.google/pubs/the-dataflow-model-a-practical-approach-to-balancing-correctness-latency-and-cost-in-massive-scale-unbounded-out-of-order-data-processing/) — the formal treatment of watermarks that underlies Beam and Dataflow.
- [Apache Flink: Generating Watermarks](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/event-time/generating_watermarks/) — the reference production implementation, including idle-source handling.
- [sync/atomic](https://pkg.go.dev/sync/atomic) — `atomic.Int64.CompareAndSwap`, the building block for the lock-free monotonic advance.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-late-data-handler.md](02-late-data-handler.md)
