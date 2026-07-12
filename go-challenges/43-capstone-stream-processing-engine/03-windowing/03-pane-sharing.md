# Exercise 3: Sliding Windows via Pane Sharing

The naive sliding-window assigner from exercise 1 places each record in `ceil(Size/Slide)` overlapping windows, and an operator that re-aggregates each of those windows from scratch does redundant work proportional to the overlap ratio. This exercise builds the standard fix: slice the timeline into non-overlapping *panes* of width `gcd(Size, Slide)`, fold each record into exactly one pane, and assemble each window by combining the panes it spans. Per-record work drops to O(1), and overlapping windows share every pane but one.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
panes.go               Pane, WindowResult, PaneAggregator, Add, Emit,
                       PaneUpdates, PaneCount
cmd/
  demo/
    main.go            stream one reading per minute, emit windows as they close
panes_test.go          window sums, pane-vs-direct equality, GC bound, O(1) updates
```

- Files: `panes.go`, `cmd/demo/main.go`, `panes_test.go`.
- Implement: `PaneAggregator` with `Add(ts, value)`, watermark-driven `Emit(watermark) []WindowResult`, and the introspection helpers `PaneUpdates()` and `PaneCount()`.
- Test: the emitted window sums for a known stream, that combining panes equals a direct aggregate, that pane GC keeps the map bounded, and that each record causes exactly one pane update.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why naive sliding aggregation is wasteful, and what panes fix

Consider a 60-minute window sliding every minute. Each record falls inside 60 overlapping windows, so an operator that keeps one accumulator per window updates 60 accumulators on every record and stores 60 partial sums that differ from each other by only the records at their non-shared edges. The redundancy grows with the overlap ratio `Size/Slide`, and for a daily window sliding every minute it is 1440-fold. This is the cost that makes naive sliding aggregation impractical at scale.

Pane sharing removes the redundancy by exploiting a simple observation: two adjacent sliding windows differ by exactly one `Slide` of data at each end. If you pre-aggregate the timeline into fixed, non-overlapping panes of width `gcd(Size, Slide)` — and when `Slide` divides `Size`, that width is exactly `Slide` — then every window is an integer number of consecutive panes, and adjacent windows share all but one. A record updates only the single pane its timestamp falls in (O(1)), and a window's result is the combination of its `Size/pane` constituent panes. The 60-minute-over-1-minute window becomes 60 panes combined per emission, with one pane updated per record instead of 60.

The catch is that the aggregate must be *combinable*: a partial state that can be merged associatively and commutatively, i.e. a commutative monoid. Sum, count, min, and max all qualify, and `(sum, count)` qualifies and yields an average on demand. A non-combinable aggregate such as an exact median or an exact distinct-count cannot be paned, because you cannot reconstruct it from per-pane partials, and such aggregates force the engine back to per-window buffers. This module aggregates `(sum, count)`, which is enough to show the mechanism and to compute an average.

### Watermark-driven emission and pane garbage collection

A pane holds data, but something has to decide when a window is closed and can be emitted. Here that signal is a watermark passed to `Emit`: a window `[s, s+Size)` is emitted once the watermark reaches `s+Size`, meaning event time has advanced past the window's end and no more in-order data for it will arrive. `Emit` walks window starts from a cursor, emits every window whose end is at or before the watermark, and stops at the first window that is not yet closed. Driving emission purely by the watermark — never by an artificial "infinite" flush — is what keeps the output to exactly the closed windows and avoids trailing windows full of empty panes.

Emission also bounds memory. Once window `[s, s+Size)` is emitted, the cursor advances to `s+Slide`, and pane `s` can never be referenced again because the next window starts one pane later. `Emit` therefore deletes pane `s` as it advances, so the pane map holds only the panes that still-open windows need — on the order of `Size/Slide` panes, not the whole history. `PaneCount` lets a test confirm the map stays bounded. (Records that arrive with a timestamp earlier than the cursor — true late data — create panes that no future window references; handling them is the watermark-and-late-data lesson that follows.)

Create `panes.go`:

```go
// Package panes computes sliding-window aggregates with pane sharing: the
// timeline is sliced into non-overlapping panes of width Slide, each record
// updates one pane, and each window combines the panes it spans.
package panes

import (
	"sync"
	"time"
)

// Pane is the combinable partial aggregate for one slice of the timeline.
// Sum and Count form a commutative monoid under addition, so panes combine.
type Pane struct {
	Sum   int64
	Count int64
}

// WindowResult is one emitted sliding window: its boundaries and the aggregate
// of every record that fell within [Start, End).
type WindowResult struct {
	Start time.Time
	End   time.Time
	Sum   int64
	Count int64
}

// PaneAggregator aggregates a single keyed stream into sliding windows using
// pane sharing. Size must be a positive multiple of Slide.
//
// PaneAggregator is safe for concurrent use.
type PaneAggregator struct {
	size           time.Duration
	slide          time.Duration
	slideNanos     int64
	panesPerWindow int64

	mu          sync.Mutex
	panes       map[int64]*Pane // pane index -> partial aggregate
	started     bool
	nextIdx     int64 // pane index of the next window to emit
	paneUpdates int64
}

// NewPaneAggregator builds an aggregator for windows of width size sliding every
// slide. size should be a positive multiple of slide so the panes tile windows
// exactly.
func NewPaneAggregator(size, slide time.Duration) *PaneAggregator {
	if slide <= 0 {
		slide = time.Nanosecond
	}
	if size < slide {
		size = slide
	}
	return &PaneAggregator{
		size:           size,
		slide:          slide,
		slideNanos:     int64(slide),
		panesPerWindow: int64(size / slide),
		panes:          make(map[int64]*Pane),
	}
}

// paneIndex returns the index of the pane that contains ts.
func (p *PaneAggregator) paneIndex(ts time.Time) int64 {
	return ts.UnixNano() / p.slideNanos
}

// Add folds value into the single pane that contains ts. It is O(1): exactly one
// pane is touched regardless of how many windows ts ultimately belongs to.
func (p *PaneAggregator) Add(ts time.Time, value int64) {
	idx := p.paneIndex(ts)

	p.mu.Lock()
	defer p.mu.Unlock()

	pane := p.panes[idx]
	if pane == nil {
		pane = &Pane{}
		p.panes[idx] = pane
	}
	pane.Sum += value
	pane.Count++
	p.paneUpdates++

	if !p.started {
		p.started = true
		p.nextIdx = idx
	}
}

// Emit returns every window whose end is at or before watermark and that has not
// been emitted yet, combining each from its shared panes. Panes that no remaining
// window can reference are deleted as the cursor advances.
func (p *PaneAggregator) Emit(watermark time.Time) []WindowResult {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.started {
		return nil
	}
	wmNanos := watermark.UnixNano()

	var out []WindowResult
	for {
		s := p.nextIdx
		endNanos := (s + p.panesPerWindow) * p.slideNanos
		if endNanos > wmNanos {
			break // window not yet closed
		}

		var sum, count int64
		for i := int64(0); i < p.panesPerWindow; i++ {
			if pane := p.panes[s+i]; pane != nil {
				sum += pane.Sum
				count += pane.Count
			}
		}
		start := time.Unix(0, s*p.slideNanos).UTC()
		out = append(out, WindowResult{
			Start: start,
			End:   start.Add(p.size),
			Sum:   sum,
			Count: count,
		})

		// The next window starts at s+1, so pane s is now unreachable.
		delete(p.panes, s)
		p.nextIdx = s + 1
	}
	return out
}

// PaneUpdates reports the total number of pane folds performed, which equals the
// number of records added: one fold per record, the O(1) property.
func (p *PaneAggregator) PaneUpdates() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.paneUpdates
}

// PaneCount reports how many panes are currently retained.
func (p *PaneAggregator) PaneCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.panes)
}
```

### The runnable demo

The demo streams one reading per minute into a 3-minute window that slides every minute, so each window spans three panes and adjacent windows share two. After each reading it advances the watermark to that reading's time and emits any window that just closed; a final watermark tick at 10:06 closes the last window. The pane-update count stays equal to the record count (one fold per record), and the pane map is left holding only the two panes the already-advanced cursor could not yet discard.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/panes"
)

func main() {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	agg := panes.NewPaneAggregator(3*time.Minute, 1*time.Minute)

	fmt.Println("=== Pane-shared sliding aggregation (size=3min, slide=1min, 3 panes/window) ===")

	values := []int64{1, 2, 3, 4, 5, 6} // one reading per minute, 10:00..10:05
	for m, v := range values {
		ts := base.Add(time.Duration(m) * time.Minute)
		agg.Add(ts, v)
		line := fmt.Sprintf("  reading %s value=%d", ts.Format("15:04"), v)
		for _, w := range agg.Emit(ts) {
			line += fmt.Sprintf(" -> window [%s,%s) sum=%d count=%d",
				w.Start.Format("15:04"), w.End.Format("15:04"), w.Sum, w.Count)
		}
		fmt.Println(line)
	}

	// Advance the watermark past the last window's end to close it.
	wm := base.Add(6 * time.Minute)
	for _, w := range agg.Emit(wm) {
		fmt.Printf("  watermark %s -> window [%s,%s) sum=%d count=%d\n",
			wm.Format("15:04"), w.Start.Format("15:04"), w.End.Format("15:04"), w.Sum, w.Count)
	}

	fmt.Printf("  total pane updates: %d\n", agg.PaneUpdates())
	fmt.Printf("  panes retained after GC: %d\n", agg.PaneCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== Pane-shared sliding aggregation (size=3min, slide=1min, 3 panes/window) ===
  reading 10:00 value=1
  reading 10:01 value=2
  reading 10:02 value=3
  reading 10:03 value=4 -> window [10:00,10:03) sum=6 count=3
  reading 10:04 value=5 -> window [10:01,10:04) sum=9 count=3
  reading 10:05 value=6 -> window [10:02,10:05) sum=12 count=3
  watermark 10:06 -> window [10:03,10:06) sum=15 count=3
  total pane updates: 6
  panes retained after GC: 2
```

### Tests

`TestSlidingWindowSums` feeds the same per-minute stream and asserts the exact four-window result, so any error in pane combination or cursor advance is caught. `TestPaneEqualsDirect` is the heart of the exercise: it computes each window two ways — by combining panes and by directly summing the raw records in the window's time range — and asserts they match, which is the correctness contract that justifies the optimization. `TestPaneGCBounded` confirms the pane map never holds more than `panesPerWindow` panes during a long stream, and `TestOneUpdatePerRecord` pins the O(1) property.

Create `panes_test.go`:

```go
package panes

import (
	"testing"
	"time"
)

var base = time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

func TestSlidingWindowSums(t *testing.T) {
	t.Parallel()
	agg := NewPaneAggregator(3*time.Minute, 1*time.Minute)
	for m, v := range []int64{1, 2, 3, 4, 5, 6} {
		agg.Add(base.Add(time.Duration(m)*time.Minute), v)
	}
	got := agg.Emit(base.Add(6 * time.Minute))

	want := []WindowResult{
		{Start: base, End: base.Add(3 * time.Minute), Sum: 6, Count: 3},
		{Start: base.Add(1 * time.Minute), End: base.Add(4 * time.Minute), Sum: 9, Count: 3},
		{Start: base.Add(2 * time.Minute), End: base.Add(5 * time.Minute), Sum: 12, Count: 3},
		{Start: base.Add(3 * time.Minute), End: base.Add(6 * time.Minute), Sum: 15, Count: 3},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d windows, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) ||
			got[i].Sum != want[i].Sum || got[i].Count != want[i].Count {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestPaneEqualsDirect(t *testing.T) {
	t.Parallel()
	size, slide := 4*time.Minute, 2*time.Minute
	type sample struct {
		min int
		val int64
	}
	samples := []sample{{0, 5}, {1, 7}, {2, 2}, {3, 9}, {4, 1}, {5, 4}, {6, 8}}

	agg := NewPaneAggregator(size, slide)
	for _, s := range samples {
		agg.Add(base.Add(time.Duration(s.min)*time.Minute), s.val)
	}
	got := agg.Emit(base.Add(20 * time.Minute))

	// Direct aggregate: for each emitted window, sum the raw samples whose
	// timestamp falls in [Start, End), independently of the pane machinery.
	for _, w := range got {
		var sum, count int64
		for _, s := range samples {
			ts := base.Add(time.Duration(s.min) * time.Minute)
			if !ts.Before(w.Start) && ts.Before(w.End) {
				sum += s.val
				count++
			}
		}
		if sum != w.Sum || count != w.Count {
			t.Errorf("window [%s,%s): pane=(%d,%d) direct=(%d,%d)",
				w.Start.Format("15:04"), w.End.Format("15:04"),
				w.Sum, w.Count, sum, count)
		}
	}
	if len(got) == 0 {
		t.Fatal("expected at least one window")
	}
}

func TestPaneGCBounded(t *testing.T) {
	t.Parallel()
	agg := NewPaneAggregator(3*time.Minute, 1*time.Minute) // 3 panes/window
	for m := 0; m < 500; m++ {
		ts := base.Add(time.Duration(m) * time.Minute)
		agg.Add(ts, 1)
		agg.Emit(ts)
		if got := agg.PaneCount(); got > 3 {
			t.Fatalf("pane map holds %d panes after minute %d, want <= 3", got, m)
		}
	}
}

func TestOneUpdatePerRecord(t *testing.T) {
	t.Parallel()
	agg := NewPaneAggregator(10*time.Minute, 1*time.Minute) // overlap ratio 10
	const n = 200
	for m := 0; m < n; m++ {
		agg.Add(base.Add(time.Duration(m)*time.Minute), 1)
	}
	// One fold per record despite each record belonging to up to 10 windows.
	if got := agg.PaneUpdates(); got != n {
		t.Fatalf("pane updates = %d, want %d (one per record)", got, n)
	}
}
```

## Review

The aggregator is correct when each window's value equals the direct aggregate of the records in its range, which is exactly what `TestPaneEqualsDirect` checks; if it ever fails, the cursor advance or the pane-combination loop is off by a pane. The most common error is emitting on an infinite or arbitrarily large watermark, which produces trailing windows whose later panes are empty — emission must be bounded by the watermark so only closed windows appear. The second is forgetting to delete pane `s` after emitting its window, which is harmless for correctness but lets the pane map grow without bound and defeats the whole point; `TestPaneGCBounded` guards it. The third is updating more than one pane per record — folding into every overlapping window instead of the single pane — which silently reintroduces the O(Size/Slide) cost that `TestOneUpdatePerRecord` exists to catch. The fourth is attempting to pane a non-combinable aggregate; sum, count, min, and max combine, but a median does not. Running under `go test -race` confirms the pane map and cursor are accessed under the mutex.

## Resources

- [No Pane, No Gain: Efficient Evaluation of Sliding-Window Aggregates over Data Streams (Li et al., SIGMOD Record 2005)](https://sigmodrecord.org/2005/03/08/no-pane-no-gain-efficient-evaluation-of-sliding-window-aggregates-over-data-streams/) — the paper that introduced pane-based sliding-window aggregation, the technique this module implements.
- [Apache Flink: Window Functions](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/windows/#window-functions) — `ReduceFunction` and `AggregateFunction` are Flink's combinable partial aggregates, the production analogue of a pane.
- [pkg.go.dev/time](https://pkg.go.dev/time) — `UnixNano`, `Unix`, and `Duration`, the arithmetic the pane index relies on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-global-window-eviction.md](02-global-window-eviction.md) | Next: [04-processing-time-timers.md](04-processing-time-timers.md)
