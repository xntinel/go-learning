# Exercise 1: The Window Operator

Windowing is the act of giving an unbounded stream a finite scope so it can be aggregated. This exercise builds a composable windowing framework along the two axes the Dataflow model identifies as orthogonal: a `WindowAssigner` that maps each record to the windows it belongs to, and a `Trigger` that decides when an accumulated window emits. A single keyed `WindowOperator` ties them together, so tumbling, sliding, and session assigners each compose with event-time, count, and processing-time triggers in any combination.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
windowing.go           Record, Window, WindowKey, TriggerResult, WindowAssigner,
                       Trigger, ReduceFunc, KeyFunc, WindowOperator
assigner.go            TumblingWindowAssigner, SlidingWindowAssigner,
                       SessionWindowAssigner (gap-based merge)
trigger.go             EventTimeTrigger, CountTrigger, ProcessingTimeTrigger
cmd/
  demo/
    main.go            tumbling+event-time, sliding+count, session windows
windowing_test.go      assigners, triggers, operator routing, keyed isolation
```

- Files: `windowing.go`, `assigner.go`, `trigger.go`, `cmd/demo/main.go`, `windowing_test.go`.
- Implement: the `WindowAssigner` and `Trigger` interfaces, three assigners, three triggers, and a keyed `WindowOperator` with `Process`, `Flush`, and `WindowCount`.
- Test: each assigner's boundary math, each trigger's fire condition, the operator's routing and per-key isolation, and the session gap that separates two distinct sessions.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p window-operator/cmd/demo && cd window-operator
go mod init example.com/windowing
go mod edit -go=1.26
```

### Why assigners and triggers are separate axes

The single most important design decision here is that "which windows?" and "emit when?" are two independent questions. A `WindowAssigner` answers the first; a `Trigger` answers the second; the `WindowOperator` is the only component that knows about both. This separation is what lets one operator support every shape-and-policy pairing without a combinatorial explosion of bespoke operators. It is the assigner/trigger decomposition from the Google Dataflow paper, and it is worth internalizing because every production stream engine — Flink, Beam, Kafka Streams — is built on the same split.

The assigners differ only in how they compute boundaries. The tumbling assigner floors the timestamp to a multiple of `Size` with `Truncate`, returning one window. The sliding assigner walks backward from `ts.Truncate(Slide)` in `Slide`-sized steps, emitting a window for each start while the timestamp is still inside `[start, start+Size)`; the loop condition `r.Timestamp.Sub(start) < a.Size` is exactly the membership test `ts < start + Size`, and because `start` begins at or below `ts` and only decreases, the lower bound `ts >= start` is automatically satisfied. The session assigner is the one stateful member: each record opens a provisional session `[ts, ts+Gap)`, that session is merged into the key's existing sessions, and the merged window covering the record is returned. Merging is a sort by start time followed by a single left-to-right sweep that extends the current session's end whenever the next session begins at or before it.

The triggers differ only in what makes them fire. `EventTimeTrigger` fires when a record's own timestamp reaches the window end — proof, in event time, that the window has closed. `CountTrigger` fires when the per-window record count reaches its threshold, which is why the operator passes the post-increment count into `OnRecord` instead of making the trigger keep a counter. `ProcessingTimeTrigger` never fires from a record; it fires only from `OnTimer`, which the operator calls from `Flush` with a wall-clock time. All three return a `TriggerResult` — `Continue`, `Fire`, or `FireAndPurge` — and the operator does exactly what the result says.

### The operator and its locking discipline

The operator keeps a `map[WindowKey]*windowState`, where `WindowKey` is `{Key, Start}`. Two records with different keys, or the same key in different windows, get independent state. `Process` assigns a record to its windows, folds it into each window's accumulator with the `ReduceFunc`, and consults the trigger; `Flush` walks every open window and consults the trigger's `OnTimer`. The one subtlety is lock ordering: `Process` calls `AssignWindows` *before* it takes the operator's own mutex, because the session assigner holds its own lock, and holding the operator lock while waiting for the assigner lock — when another goroutine in `Flush` could hold them the other way around — is a classic nested-lock reversal. Releasing the assigner before locking the operator removes the hazard entirely.

Create `windowing.go`:

```go
package windowing

import (
	"sync"
	"time"
)

// Record is a timestamped, keyed payload. Metadata carries string annotations
// that operators — including the WindowOperator — attach to emitted records.
type Record struct {
	Key       []byte
	Value     []byte
	Timestamp time.Time
	Metadata  map[string]string
}

// Window holds the start and end boundaries of one window instance.
// The window covers the half-open interval [Start, End).
type Window struct {
	Start time.Time
	End   time.Time
}

// WindowKey uniquely identifies one window within a WindowOperator.
type WindowKey struct {
	Key   string
	Start time.Time
}

// TriggerResult tells the operator what to do when a Trigger fires.
type TriggerResult int

const (
	// Continue means the window keeps accumulating records.
	Continue TriggerResult = iota
	// Fire emits the window result and keeps state for further accumulation.
	Fire
	// FireAndPurge emits the window result and discards all accumulated state.
	FireAndPurge
)

// WindowAssigner maps a record to one or more windows based on its timestamp.
// Stateless assigners (tumbling, sliding) may be shared; stateful assigners
// (session) must not be.
type WindowAssigner interface {
	AssignWindows(r Record) []Window
}

// Trigger decides when an active window should emit its accumulated result.
// count is the number of records in the window after the current record is added.
type Trigger interface {
	// OnRecord is called immediately after a record is accumulated into a window.
	OnRecord(r Record, w Window, count int) TriggerResult
	// OnTimer is called when the operator's Flush method advances the clock.
	OnTimer(t time.Time, w Window) TriggerResult
}

// ReduceFunc folds an incoming Record into a running accumulator.
// The first record in a window is used as the initial accumulator.
type ReduceFunc func(acc, incoming Record) Record

// KeyFunc extracts a routing key from a record. Records with the same key
// are assigned to independent, non-interacting windows.
type KeyFunc func(r Record) string

// DefaultKeyFunc returns the record's Key field as a string.
func DefaultKeyFunc(r Record) string {
	return string(r.Key)
}

// windowState holds the mutable per-window accumulation state.
type windowState struct {
	window      Window
	accumulator Record
	count       int
	hasRecord   bool
}

// emit produces an output Record tagged with window boundary metadata.
func (ws *windowState) emit(key string) Record {
	out := ws.accumulator
	meta := make(map[string]string, len(out.Metadata)+3)
	for k, v := range out.Metadata {
		meta[k] = v
	}
	meta["window_start"] = ws.window.Start.UTC().Format(time.RFC3339)
	meta["window_end"] = ws.window.End.UTC().Format(time.RFC3339)
	meta["window_key"] = key
	out.Metadata = meta
	return out
}

// WindowOperator routes incoming records through a WindowAssigner, accumulates
// per-(key, window) state with a ReduceFunc, and emits results when a Trigger fires.
//
// WindowOperator is safe for concurrent use.
type WindowOperator struct {
	assigner WindowAssigner
	trigger  Trigger
	reduce   ReduceFunc
	keyFn    KeyFunc

	mu    sync.Mutex
	state map[WindowKey]*windowState
}

// NewWindowOperator constructs an operator with the given components.
// If keyFn is nil, DefaultKeyFunc is used.
func NewWindowOperator(a WindowAssigner, t Trigger, reduce ReduceFunc, keyFn KeyFunc) *WindowOperator {
	if keyFn == nil {
		keyFn = DefaultKeyFunc
	}
	return &WindowOperator{
		assigner: a,
		trigger:  t,
		reduce:   reduce,
		keyFn:    keyFn,
		state:    make(map[WindowKey]*windowState),
	}
}

// Process assigns r to its windows, accumulates it, and fires any triggered windows.
// Returned records carry window boundary metadata.
func (op *WindowOperator) Process(r Record) []Record {
	// Assign windows before acquiring op.mu; stateful assigners (session windows)
	// protect their own state with a separate lock.
	key := op.keyFn(r)
	windows := op.assigner.AssignWindows(r)

	op.mu.Lock()
	defer op.mu.Unlock()

	var results []Record
	for _, w := range windows {
		wk := WindowKey{Key: key, Start: w.Start}
		ws := op.state[wk]
		if ws == nil {
			ws = &windowState{window: w}
			op.state[wk] = ws
		}
		if ws.hasRecord {
			ws.accumulator = op.reduce(ws.accumulator, r)
		} else {
			ws.accumulator = r
			ws.hasRecord = true
		}
		ws.count++

		switch op.trigger.OnRecord(r, ws.window, ws.count) {
		case Fire:
			results = append(results, ws.emit(key))
		case FireAndPurge:
			results = append(results, ws.emit(key))
			delete(op.state, wk)
		}
	}
	return results
}

// Flush calls OnTimer for every active window, emitting and optionally purging
// windows for which the trigger fires. Pass time.Now() for processing-time
// triggers, or the latest observed record timestamp for event-time triggers.
func (op *WindowOperator) Flush(now time.Time) []Record {
	op.mu.Lock()
	defer op.mu.Unlock()

	var results []Record
	var toDelete []WindowKey
	for wk, ws := range op.state {
		switch op.trigger.OnTimer(now, ws.window) {
		case Fire:
			results = append(results, ws.emit(wk.Key))
		case FireAndPurge:
			results = append(results, ws.emit(wk.Key))
			toDelete = append(toDelete, wk)
		}
	}
	for _, wk := range toDelete {
		delete(op.state, wk)
	}
	return results
}

// WindowCount returns the number of open (not yet emitted) windows.
func (op *WindowOperator) WindowCount() int {
	op.mu.Lock()
	defer op.mu.Unlock()
	return len(op.state)
}
```

Create `assigner.go`:

```go
package windowing

import (
	"sort"
	"sync"
	"time"
)

// TumblingWindowAssigner assigns each record to exactly one non-overlapping window.
// A record with timestamp t belongs to [t.Truncate(Size), t.Truncate(Size)+Size).
type TumblingWindowAssigner struct {
	Size time.Duration
}

// AssignWindows returns a single window whose boundaries align to multiples of Size.
func (a *TumblingWindowAssigner) AssignWindows(r Record) []Window {
	start := r.Timestamp.Truncate(a.Size)
	return []Window{{Start: start, End: start.Add(a.Size)}}
}

// SlidingWindowAssigner assigns each record to one or more overlapping windows.
// A record with timestamp t belongs to all windows [s, s+Size) where s is a
// multiple of Slide and s <= t < s+Size. The number of windows per record is
// at most ceil(Size/Slide).
type SlidingWindowAssigner struct {
	Size  time.Duration
	Slide time.Duration
}

// AssignWindows returns all overlapping windows for the record's timestamp.
func (a *SlidingWindowAssigner) AssignWindows(r Record) []Window {
	// lastStart is the largest multiple of Slide that is <= r.Timestamp.
	lastStart := r.Timestamp.Truncate(a.Slide)
	var windows []Window
	// Walk backwards: include start while ts is strictly inside [start, start+Size).
	for start := lastStart; r.Timestamp.Sub(start) < a.Size; start = start.Add(-a.Slide) {
		windows = append(windows, Window{Start: start, End: start.Add(a.Size)})
	}
	return windows
}

// SessionWindowAssigner groups records into gap-separated sessions.
// It is stateful: each call to AssignWindows updates the internal session list for
// the record's key and returns the resulting merged session window.
//
// SessionWindowAssigner must not be shared between multiple WindowOperators.
type SessionWindowAssigner struct {
	Gap      time.Duration
	mu       sync.Mutex
	sessions map[string][]Window // key -> sorted, non-overlapping session windows
}

// NewSessionWindowAssigner returns a new stateful session window assigner.
func NewSessionWindowAssigner(gap time.Duration) *SessionWindowAssigner {
	return &SessionWindowAssigner{
		Gap:      gap,
		sessions: make(map[string][]Window),
	}
}

// AssignWindows merges the record into its key's session list and returns the
// session window that contains the record's timestamp.
func (a *SessionWindowAssigner) AssignWindows(r Record) []Window {
	key := string(r.Key)
	newSession := Window{Start: r.Timestamp, End: r.Timestamp.Add(a.Gap)}

	a.mu.Lock()
	defer a.mu.Unlock()

	merged := mergeSessionWindows(append(a.sessions[key], newSession))
	a.sessions[key] = merged

	for _, w := range merged {
		if !r.Timestamp.Before(w.Start) && r.Timestamp.Before(w.End) {
			return []Window{w}
		}
	}
	return merged
}

// ActiveSessions returns a copy of the current merged session windows for a key.
func (a *SessionWindowAssigner) ActiveSessions(key string) []Window {
	a.mu.Lock()
	defer a.mu.Unlock()
	cp := make([]Window, len(a.sessions[key]))
	copy(cp, a.sessions[key])
	return cp
}

// mergeSessionWindows sorts windows by Start and merges any overlapping pair.
// Two sessions merge when the next session's start is not after the previous end.
func mergeSessionWindows(windows []Window) []Window {
	if len(windows) == 0 {
		return nil
	}
	sort.Slice(windows, func(i, j int) bool {
		return windows[i].Start.Before(windows[j].Start)
	})
	result := []Window{windows[0]}
	for _, w := range windows[1:] {
		last := &result[len(result)-1]
		if !w.Start.After(last.End) {
			if w.End.After(last.End) {
				last.End = w.End
			}
		} else {
			result = append(result, w)
		}
	}
	return result
}
```

Create `trigger.go`:

```go
package windowing

import "time"

// EventTimeTrigger fires when a record's timestamp proves the window's time span
// has passed. In production systems a watermark (not raw record timestamps) drives
// this trigger; watermarks are covered in the next lesson.
type EventTimeTrigger struct{}

// OnRecord fires when the record's timestamp is at or after the window end,
// indicating that the window boundary has been reached.
func (t *EventTimeTrigger) OnRecord(r Record, w Window, count int) TriggerResult {
	if !r.Timestamp.Before(w.End) {
		return FireAndPurge
	}
	return Continue
}

// OnTimer fires when the supplied time is at or after the window end.
func (t *EventTimeTrigger) OnTimer(tm time.Time, w Window) TriggerResult {
	if !tm.Before(w.End) {
		return FireAndPurge
	}
	return Continue
}

// CountTrigger fires when the window accumulates Threshold or more records.
type CountTrigger struct {
	Threshold int
}

// OnRecord fires if the count after adding this record meets or exceeds Threshold.
func (t *CountTrigger) OnRecord(r Record, w Window, count int) TriggerResult {
	if count >= t.Threshold {
		return FireAndPurge
	}
	return Continue
}

// OnTimer never fires for a count trigger; it is driven by record arrivals only.
func (t *CountTrigger) OnTimer(tm time.Time, w Window) TriggerResult {
	return Continue
}

// ProcessingTimeTrigger fires when the wall-clock time supplied to Flush meets or
// exceeds the window's end. The caller registers a ticker or time.AfterFunc outside
// the operator and calls op.Flush(time.Now()) on each tick.
type ProcessingTimeTrigger struct{}

// OnRecord never fires immediately; processing-time triggering is driven by Flush.
func (t *ProcessingTimeTrigger) OnRecord(r Record, w Window, count int) TriggerResult {
	return Continue
}

// OnTimer fires when the supplied wall-clock time is at or after the window end.
func (t *ProcessingTimeTrigger) OnTimer(tm time.Time, w Window) TriggerResult {
	if !tm.Before(w.End) {
		return FireAndPurge
	}
	return Continue
}
```

### The runnable demo

The demo runs the same operator machinery three ways. A tumbling window with an event-time trigger accumulates three readings and emits one result when `Flush` advances past the window end. A sliding window with a count trigger of 1 shows that one record at 10:03 belongs to two overlapping 5-minute windows. A session window with a 5-minute gap shows two sessions for one user: the first two clicks merge, and a purchase ten minutes later opens a second session.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/windowing"
)

func main() {
	base := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	fmt.Println("=== Tumbling window (5 min, event-time trigger) ===")
	{
		assigner := &windowing.TumblingWindowAssigner{Size: 5 * time.Minute}
		trigger := &windowing.EventTimeTrigger{}
		reduce := func(acc, incoming windowing.Record) windowing.Record {
			acc.Value = append(append(acc.Value, ','), incoming.Value...)
			return acc
		}
		op := windowing.NewWindowOperator(assigner, trigger, reduce, nil)

		events := []windowing.Record{
			{Key: []byte("sensor-1"), Value: []byte("12.5"), Timestamp: base.Add(1 * time.Minute)},
			{Key: []byte("sensor-1"), Value: []byte("13.1"), Timestamp: base.Add(2 * time.Minute)},
			{Key: []byte("sensor-1"), Value: []byte("11.8"), Timestamp: base.Add(3 * time.Minute)},
		}
		for _, r := range events {
			op.Process(r)
		}
		fmt.Printf("  open windows before flush: %d\n", op.WindowCount())

		out := op.Flush(base.Add(5 * time.Minute))
		fmt.Printf("  emitted: %d record(s)\n", len(out))
		for _, r := range out {
			fmt.Printf("  key=%s value=%s window=%s/%s\n",
				r.Key, r.Value,
				r.Metadata["window_start"],
				r.Metadata["window_end"],
			)
		}
		fmt.Printf("  open windows after flush: %d\n", op.WindowCount())
	}

	fmt.Println("\n=== Sliding window (5 min size, 2 min slide, count trigger=1) ===")
	{
		assigner := &windowing.SlidingWindowAssigner{
			Size:  5 * time.Minute,
			Slide: 2 * time.Minute,
		}
		trigger := &windowing.CountTrigger{Threshold: 1}
		reduce := func(acc, incoming windowing.Record) windowing.Record { return acc }
		op := windowing.NewWindowOperator(assigner, trigger, reduce, nil)

		r := windowing.Record{
			Key:       []byte("sensor-2"),
			Value:     []byte("99.0"),
			Timestamp: base.Add(3 * time.Minute),
		}
		out := op.Process(r)
		fmt.Printf("  windows for record at base+3min: %d\n", len(out))
		for _, w := range out {
			fmt.Printf("    window_start=%s window_end=%s\n",
				w.Metadata["window_start"],
				w.Metadata["window_end"],
			)
		}
	}

	fmt.Println("\n=== Session window (5 min gap) ===")
	{
		assigner := windowing.NewSessionWindowAssigner(5 * time.Minute)
		trigger := &windowing.EventTimeTrigger{}
		reduce := func(acc, incoming windowing.Record) windowing.Record {
			acc.Value = append(append(acc.Value, '+'), incoming.Value...)
			return acc
		}
		op := windowing.NewWindowOperator(assigner, trigger, reduce, nil)

		events := []windowing.Record{
			{Key: []byte("user-A"), Value: []byte("click"), Timestamp: base},
			{Key: []byte("user-A"), Value: []byte("view"), Timestamp: base.Add(3 * time.Minute)},
			// 10 min gap: new session.
			{Key: []byte("user-A"), Value: []byte("purchase"), Timestamp: base.Add(13 * time.Minute)},
		}
		for _, r := range events {
			op.Process(r)
		}
		fmt.Printf("  open sessions: %d\n", op.WindowCount())
		fmt.Printf("  active session windows for user-A: %v\n",
			assigner.ActiveSessions("user-A"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== Tumbling window (5 min, event-time trigger) ===
  open windows before flush: 1
  emitted: 1 record(s)
  key=sensor-1 value=12.5,13.1,11.8 window=2024-01-15T10:00:00Z/2024-01-15T10:05:00Z
  open windows after flush: 0

=== Sliding window (5 min size, 2 min slide, count trigger=1) ===
  windows for record at base+3min: 2
    window_start=2024-01-15T10:02:00Z window_end=2024-01-15T10:07:00Z
    window_start=2024-01-15T10:00:00Z window_end=2024-01-15T10:05:00Z

=== Session window (5 min gap) ===
  open sessions: 2
  active session windows for user-A: [{2024-01-15 10:00:00 +0000 UTC 2024-01-15 10:08:00 +0000 UTC} {2024-01-15 10:13:00 +0000 UTC 2024-01-15 10:18:00 +0000 UTC}]
```

### Tests

The tests pin every axis independently and then in combination. Each assigner's boundary math is checked alone — one tumbling window, two non-overlapping tumbling buckets, the sliding window count for an interior and a boundary record, session creation, session merge, and per-key isolation. Each trigger's fire condition is checked alone. Finally the operator is exercised with several assigner/trigger pairings, including the keyed-isolation case that proves two keys occupy independent windows. `TestSessionGapSeparatesTwoSessions` feeds two records farther apart than the gap and asserts that the assigner keeps them as two distinct, non-overlapping sessions.

Create `windowing_test.go`:

```go
package windowing

import (
	"fmt"
	"testing"
	"time"
)

var (
	base   = time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	minute = time.Minute
)

// concatReduce appends incoming.Value to acc.Value with a '+' separator.
func concatReduce(acc, incoming Record) Record {
	combined := make([]byte, 0, len(acc.Value)+1+len(incoming.Value))
	combined = append(combined, acc.Value...)
	combined = append(combined, '+')
	combined = append(combined, incoming.Value...)
	acc.Value = combined
	return acc
}

func rec(key string, ts time.Time, val string) Record {
	return Record{Key: []byte(key), Value: []byte(val), Timestamp: ts}
}

// --- TumblingWindowAssigner ---

func TestTumblingAssignsOneWindow(t *testing.T) {
	t.Parallel()
	a := &TumblingWindowAssigner{Size: 5 * minute}
	r := rec("k", base.Add(3*minute), "v")
	wins := a.AssignWindows(r)
	if len(wins) != 1 {
		t.Fatalf("want 1 window, got %d", len(wins))
	}
	if !wins[0].Start.Equal(base) {
		t.Errorf("start = %v, want %v", wins[0].Start, base)
	}
	if !wins[0].End.Equal(base.Add(5 * minute)) {
		t.Errorf("end = %v, want %v", wins[0].End, base.Add(5*minute))
	}
}

func TestTumblingNonOverlapping(t *testing.T) {
	t.Parallel()
	a := &TumblingWindowAssigner{Size: 5 * minute}
	r1 := rec("k", base.Add(2*minute), "a")
	r2 := rec("k", base.Add(6*minute), "b")
	w1 := a.AssignWindows(r1)[0]
	w2 := a.AssignWindows(r2)[0]
	if w1.Start.Equal(w2.Start) {
		t.Error("records in different 5-minute buckets must have different window starts")
	}
	if !w2.Start.Equal(base.Add(5 * minute)) {
		t.Errorf("second window start = %v, want %v", w2.Start, base.Add(5*minute))
	}
}

// --- SlidingWindowAssigner ---

func TestSlidingWindowCount(t *testing.T) {
	t.Parallel()
	// ts=base+3min, Size=5min, Slide=2min.
	// Windows: [base, base+5min) and [base+2min, base+7min) -> 2 windows.
	a := &SlidingWindowAssigner{Size: 5 * minute, Slide: 2 * minute}
	r := rec("k", base.Add(3*minute), "v")
	wins := a.AssignWindows(r)
	if len(wins) != 2 {
		t.Errorf("want 2 windows, got %d", len(wins))
	}
}

func TestSlidingWindowBoundaryRecord(t *testing.T) {
	t.Parallel()
	// ts=base (minute boundary), Size=5min, Slide=1min.
	// base is in: [base, base+5), [base-1, base+4), ..., [base-4, base+1) -> 5 windows.
	a := &SlidingWindowAssigner{Size: 5 * minute, Slide: 1 * minute}
	r := rec("k", base, "v")
	wins := a.AssignWindows(r)
	if len(wins) != 5 {
		t.Errorf("want 5 windows for ts at boundary, got %d", len(wins))
	}
}

// --- SessionWindowAssigner ---

func TestSessionCreatesNewSession(t *testing.T) {
	t.Parallel()
	a := NewSessionWindowAssigner(5 * minute)
	r := rec("k", base, "v")
	wins := a.AssignWindows(r)
	if len(wins) != 1 {
		t.Fatalf("want 1 window, got %d", len(wins))
	}
	if !wins[0].Start.Equal(base) {
		t.Errorf("start = %v, want %v", wins[0].Start, base)
	}
	if !wins[0].End.Equal(base.Add(5 * minute)) {
		t.Errorf("end = %v, want %v", wins[0].End, base.Add(5*minute))
	}
}

func TestSessionMergesOverlapping(t *testing.T) {
	t.Parallel()
	a := NewSessionWindowAssigner(5 * minute)
	// r1 -> session [base, base+5min).
	// r2 at base+3min -> new session [base+3min, base+8min); overlaps with the first.
	// Merged: [base, base+8min).
	r1 := rec("k", base, "a")
	r2 := rec("k", base.Add(3*minute), "b")
	a.AssignWindows(r1)
	wins := a.AssignWindows(r2)
	if len(wins) != 1 {
		t.Fatalf("want 1 merged window, got %d", len(wins))
	}
	if !wins[0].Start.Equal(base) {
		t.Errorf("merged start = %v, want %v", wins[0].Start, base)
	}
	if !wins[0].End.Equal(base.Add(8 * minute)) {
		t.Errorf("merged end = %v, want %v", wins[0].End, base.Add(8*minute))
	}
}

func TestSessionKeepsDistinctKeys(t *testing.T) {
	t.Parallel()
	a := NewSessionWindowAssigner(5 * minute)
	a.AssignWindows(rec("a", base, "v"))
	a.AssignWindows(rec("b", base, "v"))
	if got := len(a.ActiveSessions("a")); got != 1 {
		t.Errorf("key a: want 1 session, got %d", got)
	}
	if got := len(a.ActiveSessions("b")); got != 1 {
		t.Errorf("key b: want 1 session, got %d", got)
	}
}

func TestSessionGapSeparatesTwoSessions(t *testing.T) {
	t.Parallel()
	a := NewSessionWindowAssigner(5 * minute)
	// Two records more than the gap apart must stay two distinct sessions.
	a.AssignWindows(rec("k", base, "first"))
	a.AssignWindows(rec("k", base.Add(20*minute), "second"))
	sessions := a.ActiveSessions("k")
	if len(sessions) != 2 {
		t.Fatalf("want 2 distinct sessions, got %d: %v", len(sessions), sessions)
	}
	if !sessions[0].Start.Equal(base) || !sessions[0].End.Equal(base.Add(5*minute)) {
		t.Errorf("session 0 = %v, want [base, base+5min)", sessions[0])
	}
	if !sessions[1].Start.Equal(base.Add(20*minute)) || !sessions[1].End.Equal(base.Add(25*minute)) {
		t.Errorf("session 1 = %v, want [base+20min, base+25min)", sessions[1])
	}
	// The sessions must not overlap: session 0 ends before session 1 begins.
	if sessions[0].End.After(sessions[1].Start) {
		t.Errorf("sessions overlap: %v and %v", sessions[0], sessions[1])
	}
}

// --- EventTimeTrigger ---

func TestEventTimeTriggerContinuesBeforeBoundary(t *testing.T) {
	t.Parallel()
	trig := &EventTimeTrigger{}
	w := Window{Start: base, End: base.Add(5 * minute)}
	r := rec("k", base.Add(2*minute), "v")
	if got := trig.OnRecord(r, w, 1); got != Continue {
		t.Errorf("OnRecord before window end = %d, want Continue", got)
	}
}

func TestEventTimeTriggerFiresAtBoundary(t *testing.T) {
	t.Parallel()
	trig := &EventTimeTrigger{}
	w := Window{Start: base, End: base.Add(5 * minute)}
	// A record at exactly the window end proves the window has closed.
	r := rec("k", base.Add(5*minute), "v")
	if got := trig.OnRecord(r, w, 2); got != FireAndPurge {
		t.Errorf("OnRecord at window end = %d, want FireAndPurge", got)
	}
}

func TestEventTimeTriggerOnTimer(t *testing.T) {
	t.Parallel()
	trig := &EventTimeTrigger{}
	w := Window{Start: base, End: base.Add(5 * minute)}
	if got := trig.OnTimer(base.Add(4*minute), w); got != Continue {
		t.Errorf("OnTimer before end = %d, want Continue", got)
	}
	if got := trig.OnTimer(base.Add(5*minute), w); got != FireAndPurge {
		t.Errorf("OnTimer at end = %d, want FireAndPurge", got)
	}
}

// --- CountTrigger ---

func TestCountTriggerContinuesBeforeThreshold(t *testing.T) {
	t.Parallel()
	trig := &CountTrigger{Threshold: 3}
	w := Window{Start: base, End: base.Add(5 * minute)}
	r := rec("k", base, "v")
	if got := trig.OnRecord(r, w, 2); got != Continue {
		t.Errorf("OnRecord(count=2) = %d, want Continue", got)
	}
}

func TestCountTriggerFiresAtThreshold(t *testing.T) {
	t.Parallel()
	trig := &CountTrigger{Threshold: 3}
	w := Window{Start: base, End: base.Add(5 * minute)}
	r := rec("k", base, "v")
	if got := trig.OnRecord(r, w, 3); got != FireAndPurge {
		t.Errorf("OnRecord(count=3) = %d, want FireAndPurge", got)
	}
}

// --- WindowOperator ---

func TestWindowOperatorTumblingCountTrigger(t *testing.T) {
	t.Parallel()
	a := &TumblingWindowAssigner{Size: 5 * minute}
	op := NewWindowOperator(a, &CountTrigger{Threshold: 2}, concatReduce, nil)

	r1 := rec("k", base.Add(1*minute), "a")
	r2 := rec("k", base.Add(2*minute), "b")
	if out := op.Process(r1); len(out) != 0 {
		t.Fatalf("first record: expected no output, got %d", len(out))
	}
	out := op.Process(r2)
	if len(out) != 1 {
		t.Fatalf("second record: expected 1 output, got %d", len(out))
	}
	if string(out[0].Value) != "a+b" {
		t.Errorf("value = %q, want %q", out[0].Value, "a+b")
	}
	if out[0].Metadata["window_start"] == "" {
		t.Error("emitted record must carry window_start metadata")
	}
	if out[0].Metadata["window_end"] == "" {
		t.Error("emitted record must carry window_end metadata")
	}
}

func TestWindowOperatorKeyedIsolation(t *testing.T) {
	t.Parallel()
	a := &TumblingWindowAssigner{Size: 5 * minute}
	op := NewWindowOperator(a, &CountTrigger{Threshold: 3}, concatReduce, nil)

	// Two records with different keys should occupy independent windows.
	op.Process(rec("a", base.Add(1*minute), "x"))
	op.Process(rec("b", base.Add(1*minute), "y"))
	if got := op.WindowCount(); got != 2 {
		t.Errorf("want 2 open windows (one per key), got %d", got)
	}
}

func TestWindowOperatorFlushEventTime(t *testing.T) {
	t.Parallel()
	a := &TumblingWindowAssigner{Size: 5 * minute}
	op := NewWindowOperator(a, &EventTimeTrigger{}, concatReduce, nil)

	op.Process(rec("k", base.Add(1*minute), "a"))
	op.Process(rec("k", base.Add(2*minute), "b"))
	if op.WindowCount() != 1 {
		t.Fatalf("want 1 open window before flush, got %d", op.WindowCount())
	}

	out := op.Flush(base.Add(5 * minute))
	if len(out) != 1 {
		t.Fatalf("Flush: expected 1 output, got %d", len(out))
	}
	if op.WindowCount() != 0 {
		t.Error("window must be purged after FireAndPurge")
	}
}

func TestWindowOperatorSlidingMultipleEmissions(t *testing.T) {
	t.Parallel()
	a := &SlidingWindowAssigner{Size: 5 * minute, Slide: 2 * minute}
	op := NewWindowOperator(a, &CountTrigger{Threshold: 1}, concatReduce, nil)

	// A record at base+3min belongs to 2 overlapping windows.
	// CountTrigger(1) fires immediately on the first record in each.
	out := op.Process(rec("k", base.Add(3*minute), "v"))
	if len(out) != 2 {
		t.Errorf("want 2 window results for sliding window, got %d", len(out))
	}
}

func TestWindowOperatorProcessingTimeTrigger(t *testing.T) {
	t.Parallel()
	a := &TumblingWindowAssigner{Size: 5 * minute}
	op := NewWindowOperator(a, &ProcessingTimeTrigger{}, concatReduce, nil)

	op.Process(rec("k", base.Add(1*minute), "a"))
	// Before the processing-time window end: Flush should not emit yet.
	before := op.Flush(base.Add(4 * minute))
	if len(before) != 0 {
		t.Errorf("premature Flush: expected 0 outputs, got %d", len(before))
	}
	// At or after the window end: Flush must emit.
	after := op.Flush(base.Add(5 * minute))
	if len(after) != 1 {
		t.Errorf("Flush at window end: expected 1 output, got %d", len(after))
	}
}

// ExampleTumblingWindowAssigner_AssignWindows documents the tumbling boundary
// calculation for a record at 10:03 UTC with a 5-minute window.
func ExampleTumblingWindowAssigner_AssignWindows() {
	ts := time.Date(2024, 1, 15, 10, 3, 0, 0, time.UTC)
	a := &TumblingWindowAssigner{Size: 5 * time.Minute}
	wins := a.AssignWindows(Record{Timestamp: ts})
	fmt.Printf("count=%d start=%s end=%s\n",
		len(wins),
		wins[0].Start.UTC().Format("15:04"),
		wins[0].End.UTC().Format("15:04"),
	)
	// Output:
	// count=1 start=10:00 end=10:05
}
```

## Review

The framework is correct when the two axes stay independent and the operator obeys both. The most common error is folding trigger logic into the assigner — for example, making the sliding assigner decide emission — which destroys the combinatorial freedom that justifies the design. The second is using `Round` instead of `Truncate` for boundaries, which lands records in the window after the one they belong to. The third is sharing a single `*SessionWindowAssigner` between operators, or mutating it from two goroutines without its lock; the session assigner is the one stateful component and each operator must own its own. The fourth is expecting `Flush` to emit on a count-trigger operator: `CountTrigger.OnTimer` always returns `Continue`, so a time-driven flush never fires it. Running the suite under `go test -race` exercises concurrent `Process` and `Flush` against the shared state map and the session assigner's lock together.

## Resources

- [pkg.go.dev/time](https://pkg.go.dev/time) — `Truncate`, `Duration`, `AfterFunc`, and `Ticker`; the canonical reference for the time operations every assigner and trigger uses.
- [The Dataflow Model (Akidau et al., VLDB 2015)](https://research.google/pubs/the-dataflow-model-a-practical-approach-to-balancing-correctness-latency-and-cost-in-massive-scale-unbounded-out-of-order-data-processing/) — the academic source of the assigner/trigger decomposition; section 2 defines the windowing axes.
- [Apache Flink Windows documentation](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/windows/) — a production implementation of the same model; read the session-window merge section alongside this code.
- [Streaming 101 (Tyler Akidau, O'Reilly)](https://www.oreilly.com/radar/the-world-beyond-batch-streaming-101/) — accessible introduction to event time vs processing time and why the distinction matters.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-global-window-eviction.md](02-global-window-eviction.md)
