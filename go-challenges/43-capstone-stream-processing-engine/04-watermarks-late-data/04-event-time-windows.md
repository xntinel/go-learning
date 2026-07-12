# Exercise 4: The Event-Time Tumbling Window Operator

The watermark is only useful because it drives firing. This exercise builds the operator that ties everything together: it assigns each record to a fixed-size tumbling window aligned to the epoch, accumulates a per-window aggregate, and fires a window exactly when the watermark passes its end — emitting all eligible windows in deterministic order and routing records that arrive too late to a side output.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
operator.go            Window, Result, LateRecord, Operator (WindowFor, OnRecord, Advance)
cmd/
  demo/
    main.go            out-of-order ingest, stepwise watermark, one dropped straggler
operator_test.go       assignment, fire-on-watermark, fire-once, ordering, late drop, monotonic
example_test.go        runnable doc example for WindowFor
```

- Files: `operator.go`, `cmd/demo/main.go`, `operator_test.go`, `example_test.go`.
- Implement: `Operator` with `WindowFor(eventTime) Window`, `OnRecord(value, eventTime)`, `Advance(watermark) []Result`, and `LateDropped() int64`, accumulating a sum and count per window.
- Test: events land in the correct window; a window fires only when the watermark reaches its end; it fires exactly once; multiple eligible windows fire in ascending order; a record whose window already fired is dropped and recorded; a regressing watermark is ignored.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Assignment, accumulation, and watermark-triggered firing

A tumbling window of size D partitions the event-time axis into contiguous, non-overlapping intervals aligned to the epoch. An event at timestamp t belongs to `[floor(t/D)*D, floor(t/D)*D + D)`. `WindowFor` does exactly this arithmetic in milliseconds, so an event at 00:00:07 in a 10-second pipeline lands in `[00:00:00, 00:00:10)` and an event at 00:00:12 lands in `[00:00:10, 00:00:20)`. Assignment is pure: it depends only on the timestamp, never on arrival order, which is what makes the operator deterministic.

`OnRecord` assigns the record, then makes one decision: is the record late? It is late if the operator already holds a watermark that has reached or passed the record's window end — the watermark previously promised nothing earlier would arrive, and this record breaks that promise, so its window has already fired and been purged. A late record is counted and appended to the `Late` side output rather than folded into a window that no longer exists. An in-time record is added to its window's running sum and count.

Firing happens in `Advance`. The operator clamps the incoming watermark to be monotonic — a watermark that tries to go backwards is ignored, because results for earlier windows are already out — and then collects every window whose end is at or below the new watermark and has not yet fired. The collected windows are sorted by start time and emitted in ascending order, so the output is byte-identical across runs regardless of the order records arrived. Each fired window's state is deleted and its start recorded as fired, so a later `Advance` past the same boundary cannot fire it twice and a straggler for it is correctly classified as late.

Create `operator.go`:

```go
// Package etwindow implements an event-time tumbling window operator. Records
// are assigned to fixed-size windows aligned to the epoch; a window fires when
// the watermark reaches its end. Records whose window has already fired are
// routed to a side output. The operator is single-threaded by design.
package etwindow

import (
	"fmt"
	"sort"
	"time"
)

// Window is a half-open time interval [Start, End).
type Window struct {
	Start time.Time
	End   time.Time
}

// String returns a human-readable label for the window in UTC.
func (w Window) String() string {
	const f = "15:04:05"
	return fmt.Sprintf("[%s, %s)", w.Start.UTC().Format(f), w.End.UTC().Format(f))
}

// Result is the aggregate emitted when a window fires.
type Result struct {
	Window Window
	Sum    int64
	Count  int
}

// LateRecord is a record that arrived after its window had already fired.
type LateRecord struct {
	Value     int64
	EventTime time.Time
	Window    Window
}

// pane holds the running aggregate for one open window.
type pane struct {
	sum   int64
	count int
}

// Operator assigns records to tumbling windows and fires them from the
// watermark. It is not safe for concurrent use: drive it from a single
// goroutine, as a stream operator runs in one task thread.
type Operator struct {
	size      time.Duration
	sizeMs    int64
	panes     map[int64]*pane // keyed by window-start UnixMilli
	fired     map[int64]bool  // window-start UnixMilli -> already fired
	watermark time.Time
	hasWM     bool
	dropped   int64

	// Late accumulates records dropped because their window had already fired.
	Late []LateRecord
}

// NewOperator creates an Operator for tumbling windows of the given size.
func NewOperator(size time.Duration) *Operator {
	return &Operator{
		size:   size,
		sizeMs: size.Milliseconds(),
		panes:  make(map[int64]*pane),
		fired:  make(map[int64]bool),
	}
}

// WindowFor returns the tumbling window that contains eventTime.
func (op *Operator) WindowFor(eventTime time.Time) Window {
	ms := eventTime.UnixMilli()
	startMs := (ms / op.sizeMs) * op.sizeMs
	start := time.UnixMilli(startMs).UTC()
	return Window{Start: start, End: start.Add(op.size)}
}

// OnRecord assigns the record to its window. If the watermark has already
// passed that window's end, the record is late: it is counted and appended to
// Late instead of being aggregated.
func (op *Operator) OnRecord(value int64, eventTime time.Time) {
	w := op.WindowFor(eventTime)
	if op.hasWM && !op.watermark.Before(w.End) {
		op.dropped++
		op.Late = append(op.Late, LateRecord{Value: value, EventTime: eventTime, Window: w})
		return
	}
	startMs := w.Start.UnixMilli()
	p := op.panes[startMs]
	if p == nil {
		p = &pane{}
		op.panes[startMs] = p
	}
	p.sum += value
	p.count++
}

// Advance moves the watermark forward (monotonically) and fires every window
// whose end is at or below the new watermark and has not yet fired. Results are
// returned in ascending window-start order. A watermark that does not advance
// returns no results.
func (op *Operator) Advance(watermark time.Time) []Result {
	if op.hasWM && !watermark.After(op.watermark) {
		return nil
	}
	op.watermark = watermark
	op.hasWM = true

	var starts []int64
	for startMs, p := range op.panes {
		end := time.UnixMilli(startMs).Add(op.size)
		if !watermark.Before(end) && p != nil { // watermark >= window.End
			starts = append(starts, startMs)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	results := make([]Result, 0, len(starts))
	for _, startMs := range starts {
		p := op.panes[startMs]
		start := time.UnixMilli(startMs).UTC()
		w := Window{Start: start, End: start.Add(op.size)}
		results = append(results, Result{Window: w, Sum: p.sum, Count: p.count})
		delete(op.panes, startMs)
		op.fired[startMs] = true
	}
	return results
}

// LateDropped returns the number of records dropped because their window had
// already fired.
func (op *Operator) LateDropped() int64 {
	return op.dropped
}
```

The `fired` map is not strictly needed to avoid double-firing — a fired window's pane is deleted, so `Advance` cannot re-collect it — but it documents intent and would matter if the operator were extended with allowed lateness, where a fired window's pane is kept alive past its end. Keeping the late-classification rule purely on the watermark (`watermark >= window.End`) rather than on map membership means a record for a window that has been fired and purged is still correctly judged late.

### The runnable demo

The demo ingests records out of order into two 10-second windows, advances the watermark in steps, and shows windows firing as the watermark crosses their ends. One straggler arrives for the first window after it has already fired and is routed to the side output.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/event-time-windows"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const f = "15:04:05"
	op := etwindow.NewOperator(10 * time.Second)

	// Ingest into window [00:00:00, 00:00:10).
	op.OnRecord(1, base.Add(3*time.Second))
	op.OnRecord(2, base.Add(7*time.Second))
	op.OnRecord(5, base.Add(12*time.Second)) // window [00:00:10, 00:00:20)

	// Watermark at 00:00:08: not past either window end, nothing fires.
	for _, r := range op.Advance(base.Add(8 * time.Second)) {
		fmt.Printf("fired %s sum=%d count=%d\n", r.Window, r.Sum, r.Count)
	}

	op.OnRecord(3, base.Add(5*time.Second)) // still in-time for [00:00:00, 00:00:10)

	// Watermark at 00:00:11: first window fires.
	for _, r := range op.Advance(base.Add(11 * time.Second)) {
		fmt.Printf("fired %s sum=%d count=%d\n", r.Window, r.Sum, r.Count)
	}

	op.OnRecord(9, base.Add(4*time.Second))  // late: window already fired
	op.OnRecord(7, base.Add(18*time.Second)) // in-time for [00:00:10, 00:00:20)

	// Watermark at 00:00:25: second window fires.
	for _, r := range op.Advance(base.Add(25 * time.Second)) {
		fmt.Printf("fired %s sum=%d count=%d\n", r.Window, r.Sum, r.Count)
	}

	fmt.Printf("dropped %d late record(s):\n", op.LateDropped())
	for _, lr := range op.Late {
		fmt.Printf("  value=%d eventtime=%s window=%s\n",
			lr.Value, lr.EventTime.UTC().Format(f), lr.Window)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fired [00:00:00, 00:00:10) sum=6 count=3
fired [00:00:10, 00:00:20) sum=12 count=2
dropped 1 late record(s):
  value=9 eventtime=00:00:04 window=[00:00:00, 00:00:10)
```

The first window sums `1 + 2 + 3 = 6` because the out-of-order record of 3 arrived while the watermark was still at 8s, before the window fired. The straggler of 9 arrived after the watermark had passed 00:00:10, so it was dropped to the side output rather than corrupting an already-emitted result.

### Tests

`TestWindowForAssignment` pins the epoch-aligned assignment. `TestFiresWhenWatermarkPasses` and `TestDoesNotFireEarly` bracket the firing boundary. `TestFiresExactlyOnce` advances past a boundary twice and asserts a single fire. `TestMultipleWindowsFireInOrder` proves ascending-order emission. `TestLateRecordRoutedToSideOutput` proves a straggler is dropped and recorded. `TestWatermarkMonotonic` proves a regressing watermark fires nothing.

Create `operator_test.go`:

```go
package etwindow

import (
	"testing"
	"time"
)

var base = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestWindowForAssignment(t *testing.T) {
	t.Parallel()
	op := NewOperator(10 * time.Second)
	w := op.WindowFor(base.Add(7 * time.Second))
	if !w.Start.Equal(base) || !w.End.Equal(base.Add(10*time.Second)) {
		t.Fatalf("window = %s, want [00:00:00, 00:00:10)", w)
	}
	w2 := op.WindowFor(base.Add(12 * time.Second))
	if !w2.Start.Equal(base.Add(10*time.Second)) || !w2.End.Equal(base.Add(20*time.Second)) {
		t.Fatalf("window = %s, want [00:00:10, 00:00:20)", w2)
	}
}

func TestFiresWhenWatermarkPasses(t *testing.T) {
	t.Parallel()
	op := NewOperator(10 * time.Second)
	op.OnRecord(4, base.Add(3*time.Second))
	op.OnRecord(6, base.Add(8*time.Second))

	results := op.Advance(base.Add(10 * time.Second)) // watermark == window.End
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Sum != 10 || results[0].Count != 2 {
		t.Fatalf("result = sum %d count %d, want sum 10 count 2", results[0].Sum, results[0].Count)
	}
}

func TestDoesNotFireEarly(t *testing.T) {
	t.Parallel()
	op := NewOperator(10 * time.Second)
	op.OnRecord(4, base.Add(3*time.Second))

	if results := op.Advance(base.Add(9 * time.Second)); len(results) != 0 {
		t.Fatalf("fired %d windows at watermark 9s, want 0 (window ends at 10s)", len(results))
	}
}

func TestFiresExactlyOnce(t *testing.T) {
	t.Parallel()
	op := NewOperator(10 * time.Second)
	op.OnRecord(4, base.Add(3*time.Second))

	if results := op.Advance(base.Add(11 * time.Second)); len(results) != 1 {
		t.Fatalf("first advance fired %d, want 1", len(results))
	}
	if results := op.Advance(base.Add(12 * time.Second)); len(results) != 0 {
		t.Fatalf("second advance fired %d, want 0 (already fired)", len(results))
	}
}

func TestMultipleWindowsFireInOrder(t *testing.T) {
	t.Parallel()
	op := NewOperator(10 * time.Second)
	op.OnRecord(1, base.Add(25*time.Second)) // [20,30)
	op.OnRecord(2, base.Add(5*time.Second))  // [0,10)
	op.OnRecord(3, base.Add(15*time.Second)) // [10,20)

	results := op.Advance(base.Add(35 * time.Second))
	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	wantStarts := []time.Time{base, base.Add(10 * time.Second), base.Add(20 * time.Second)}
	for i, w := range wantStarts {
		if !results[i].Window.Start.Equal(w) {
			t.Fatalf("result[%d] start = %s, want %s", i, results[i].Window.Start, w)
		}
	}
}

func TestLateRecordRoutedToSideOutput(t *testing.T) {
	t.Parallel()
	op := NewOperator(10 * time.Second)
	op.OnRecord(4, base.Add(3*time.Second))
	op.Advance(base.Add(11 * time.Second)) // fire [0,10)

	op.OnRecord(9, base.Add(4*time.Second)) // late: window already fired
	if op.LateDropped() != 1 {
		t.Fatalf("LateDropped = %d, want 1", op.LateDropped())
	}
	if len(op.Late) != 1 || op.Late[0].Value != 9 {
		t.Fatalf("Late = %+v, want one record with value 9", op.Late)
	}
}

func TestWatermarkMonotonic(t *testing.T) {
	t.Parallel()
	op := NewOperator(10 * time.Second)
	op.OnRecord(1, base.Add(25*time.Second)) // [20,30)
	op.Advance(base.Add(30 * time.Second))   // fire [20,30)

	// A regressing watermark must fire nothing and must not resurrect state.
	op.OnRecord(2, base.Add(15*time.Second)) // [10,20)
	if results := op.Advance(base.Add(18 * time.Second)); len(results) != 0 {
		t.Fatalf("regressing watermark fired %d windows, want 0", len(results))
	}
}
```

Create `example_test.go`:

```go
package etwindow_test

import (
	"fmt"
	"time"

	"example.com/event-time-windows"
)

func ExampleOperator_WindowFor() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	op := etwindow.NewOperator(10 * time.Second)
	fmt.Println(op.WindowFor(base.Add(7 * time.Second)))
	fmt.Println(op.WindowFor(base.Add(12 * time.Second)))
	// Output:
	// [00:00:00, 00:00:10)
	// [00:00:10, 00:00:20)
}
```

## Review

The operator is correct when firing depends only on the watermark and never on arrival order or wall-clock time. The most common mistake is firing on a record count or a processing-time timer, which reintroduces the non-determinism event-time windows exist to remove; here a window fires solely when `watermark >= window.End`. The second mistake is firing windows in map-iteration order, which Go randomizes, so two runs would emit the same windows in different orders — the `sort.Slice` on the collected starts makes the output reproducible. The third is mis-classifying a straggler: a record must be judged late against `window.End <= watermark`, not against whether its pane still exists, so a window that has fired and had its pane purged still correctly routes its stragglers to the side output. `TestWatermarkMonotonic` guards the last hazard, that a regressing watermark neither fires nor resurrects purged state.

## Resources

- [Apache Flink: Window Assigners](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/windows/#window-assigners) — tumbling, sliding, and session assigners; this exercise implements the tumbling case.
- [Streaming 101 (Tyler Akidau, O'Reilly)](https://www.oreilly.com/radar/the-world-beyond-batch-streaming-101/) — windowing and the difference between event-time and processing-time triggers.
- [sort.Slice](https://pkg.go.dev/sort#Slice) — the deterministic ordering applied to the windows fired in a single advance.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-watermark-generators.md](03-watermark-generators.md) | Next: [05-watermark-lag-monitor.md](05-watermark-lag-monitor.md)
