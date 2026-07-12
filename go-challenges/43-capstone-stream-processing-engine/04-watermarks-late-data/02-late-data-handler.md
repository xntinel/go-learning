# Exercise 2: The Late-Data Handler

Once a window has fired, the watermark has promised that nothing earlier will arrive — but reality breaks that promise, and a straggler shows up. The late-data handler is the component that decides what happens next: drop it, fold it into the already-fired window and re-emit, or push it to a side channel for auditing. This exercise builds a handler with all three policies, accumulating and accumulating-and-retracting re-fire modes, an allowed-lateness grace period, and lock-free metrics.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
types.go               Window, Record, RecordKind, LatePolicy, IsLate
handler.go             windowAccumulator, Metrics, LateHandler (WindowFired, Handle, Purge)
cmd/
  demo/
    main.go            accept-with-retraction re-fire; metrics snapshot
handler_test.go        discard, accept (accumulating + retracting), beyond-lateness, side output, purge
```

- Files: `types.go`, `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: `LateHandler` with `WindowFired(w, sum)`, `Handle(record, watermark)`, `Purge(now)`, and `SnapshotMetrics()`, across the `LateDiscard`, `LateAccept`, and `LateSideOutput` policies.
- Test: discard counts and drops; accept folds and re-fires (both accumulating and retracting); records beyond allowed lateness are dropped; side output receives beyond-lateness records; a purged window can no longer accept.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Allowed lateness is a grace window over already-fired state

The handler keeps a `windowAccumulator` for every fired window: the raw values it has absorbed, the last aggregate it emitted, and a `purgeAt` deadline equal to `window.End + allowedLateness`. A record is *within lateness* if its window is still registered and the watermark has not yet crossed that deadline. Within lateness, the record is folded into the accumulator and the window re-fires; past it, the window state is gone (or about to be) and the record is dropped or side-output. The deadline is the entire memory-versus-completeness trade made concrete: a longer `allowedLateness` keeps more accumulators alive for longer to catch more stragglers.

The discard policy short-circuits before touching any state — it increments a counter and returns, which is why it is the cheapest and also the only policy that loses data silently. The other two policies share the same fold logic and differ only at the end: `LateAccept` re-emits onto the main `Output`, while `LateSideOutput` sends beyond-lateness records to the separate `SideOutput` channel so nothing is lost without a trace.

### Accumulating versus accumulating-and-retracting

When a late record folds into a window that already emitted, say, 100, a downstream consumer needs to know how to reconcile the new total. In plain *accumulating* mode the handler emits the new total (105) as a `KindNormal` record; a consumer that overwrites the previous value by window key stays correct, but a consumer that blindly sums every emission would now have 100 + 105. In *accumulating-and-retracting* mode the handler first emits a `KindRetraction` carrying the previous value (100) and then a `KindUpdate` carrying the new total (105); a summing consumer computes `... - 100 + 105` and lands on the right answer. The `retracting` flag selects between them, and the `Retractions` metric counts the corrections so you can see how much downstream churn late data is causing.

The lock discipline is the subtle part. The accumulator is mutated only under `lh.mu`, but the channel sends happen *after* the unlock. Holding the mutex across a send would deadlock if `Output` filled up while the consumer was itself blocked trying to call `Handle`. So `Handle` captures `prev` and `newSum` under the lock, releases it, and only then sends — first the retraction, then the update. The metrics counters are `atomic.Int64`, so `SnapshotMetrics` never has to take `lh.mu` at all.

Create `types.go`:

```go
// Package latedata routes records that arrive after their window's watermark
// has passed, according to a configurable policy.
package latedata

import "time"

// RecordKind distinguishes normal output records from corrections emitted in
// accumulating-and-retracting mode.
type RecordKind int

const (
	KindNormal     RecordKind = iota // standard aggregation result
	KindRetraction                   // negation of a previously emitted result
	KindUpdate                       // replacement value following a retraction
)

// LatePolicy controls what happens when a record arrives after the watermark
// has passed its window boundary.
type LatePolicy int

const (
	LateDiscard    LatePolicy = iota // drop and count
	LateAccept                       // accept into already-fired window; re-fire
	LateSideOutput                   // redirect beyond-lateness records to SideOutput
)

// Window is a half-open time interval [Start, End).
type Window struct {
	Start time.Time
	End   time.Time
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
```

Create `handler.go`:

```go
package latedata

import (
	"sync"
	"sync/atomic"
	"time"
)

// windowAccumulator keeps the raw values assigned to a window so that re-fires
// triggered by late data can produce an updated aggregate.
type windowAccumulator struct {
	records   []int64
	lastValue int64     // most recently emitted aggregate, used for retraction
	purgeAt   time.Time // window.End + allowedLateness
}

// Metrics holds observable counters for the late-data subsystem. Values are
// safe to read directly on a snapshot returned by SnapshotMetrics.
type Metrics struct {
	LateAccepted int64
	LateDropped  int64
	Retractions  int64
}

// LateHandler routes records that arrive after the watermark has passed their
// window boundary according to a configurable LatePolicy.
//
// Output receives re-fired window results (initial fires and late re-fires).
// SideOutput receives records that arrive beyond the allowed-lateness deadline
// when policy is LateSideOutput.
//
// Both channels use non-blocking sends: if the buffer is full, the record is
// silently dropped. Size the buffer generously or drain the channels promptly.
type LateHandler struct {
	mu              sync.Mutex
	windows         map[Window]*windowAccumulator
	allowedLateness time.Duration
	policy          LatePolicy
	retracting      bool // true -> emit retraction before re-fire update

	Output     chan Record
	SideOutput chan Record

	lateAccepted atomic.Int64
	lateDropped  atomic.Int64
	retractions  atomic.Int64
}

// NewLateHandler creates a LateHandler. outputBuf controls the buffer size of
// both Output and SideOutput channels.
func NewLateHandler(policy LatePolicy, allowedLateness time.Duration, retracting bool, outputBuf int) *LateHandler {
	return &LateHandler{
		windows:         make(map[Window]*windowAccumulator),
		allowedLateness: allowedLateness,
		policy:          policy,
		retracting:      retracting,
		Output:          make(chan Record, outputBuf),
		SideOutput:      make(chan Record, outputBuf),
	}
}

// WindowFired emits the initial result for a window when the watermark first
// passes the window boundary and registers the window state for late-data
// tracking. Call exactly once per window.
func (lh *LateHandler) WindowFired(w Window, sum int64) {
	lh.mu.Lock()
	lh.windows[w] = &windowAccumulator{
		records:   []int64{sum},
		lastValue: sum,
		purgeAt:   w.End.Add(lh.allowedLateness),
	}
	lh.mu.Unlock()

	select {
	case lh.Output <- Record{Value: sum, Kind: KindNormal, Window: w}:
	default:
	}
}

// Handle routes a late record according to the handler's policy. watermark is
// the current watermark at the time the record arrived.
func (lh *LateHandler) Handle(r Record, watermark time.Time) {
	if lh.policy == LateDiscard {
		lh.lateDropped.Add(1)
		return
	}

	lh.mu.Lock()
	wa, known := lh.windows[r.Window]
	// withinLateness: the window is registered and the watermark has not yet
	// passed window.End + allowedLateness.
	withinLateness := known && watermark.Before(r.Window.End.Add(lh.allowedLateness))

	if !withinLateness {
		lh.mu.Unlock()
		lh.lateDropped.Add(1)
		if lh.policy == LateSideOutput {
			select {
			case lh.SideOutput <- r:
			default:
			}
		}
		return
	}

	// Accept into the window and recompute the aggregate.
	wa.records = append(wa.records, r.Value)
	var newSum int64
	for _, v := range wa.records {
		newSum += v
	}
	prev := wa.lastValue
	wa.lastValue = newSum
	lh.mu.Unlock()

	lh.lateAccepted.Add(1)

	if lh.retracting {
		lh.retractions.Add(1)
		select {
		case lh.Output <- Record{Value: prev, Kind: KindRetraction, Window: r.Window}:
		default:
		}
	}

	kind := KindNormal
	if lh.retracting {
		kind = KindUpdate
	}
	select {
	case lh.Output <- Record{Value: newSum, Kind: kind, Window: r.Window}:
	default:
	}
}

// Purge removes window state that is past its allowed-lateness deadline. Call
// periodically in the same goroutine that advances the watermark.
func (lh *LateHandler) Purge(now time.Time) {
	lh.mu.Lock()
	defer lh.mu.Unlock()
	for w, wa := range lh.windows {
		if now.After(wa.purgeAt) {
			delete(lh.windows, w)
		}
	}
}

// SnapshotMetrics returns a point-in-time copy of the current counters.
func (lh *LateHandler) SnapshotMetrics() Metrics {
	return Metrics{
		LateAccepted: lh.lateAccepted.Load(),
		LateDropped:  lh.lateDropped.Load(),
		Retractions:  lh.retractions.Load(),
	}
}
```

The non-blocking `select { case ch <- r: default: }` is deliberate: a slow consumer must never be able to block the handler and, through it, the watermark-advancing goroutine. The cost is that an undersized buffer silently drops output, which is why `WindowFired` and `Handle` size their channels through `outputBuf` and the demo drains promptly.

### The runnable demo

The demo fires a `[12:00:00, 12:00:10)` window with an initial aggregate of 1000, then a late record of 42 arrives while the watermark is at 12:00:15 — past the window's end but within its 30-second allowed lateness. Because the handler is in retracting mode, it emits a retraction of 1000 followed by an update of 1042, and the metrics show one acceptance and one retraction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/late-data-handler"
)

func main() {
	base := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	w := latedata.Window{Start: base, End: base.Add(10 * time.Second)}

	lh := latedata.NewLateHandler(latedata.LateAccept, 30*time.Second, true, 32)

	lh.WindowFired(w, 1000) // initial aggregate
	initial := <-lh.Output
	fmt.Printf("initial fire: value=%d kind=%d\n", initial.Value, initial.Kind)

	// Watermark 12:00:15: past window.End (10s) but within allowedLateness (40s).
	watermark := base.Add(15 * time.Second)
	late := latedata.Record{Value: 42, Kind: latedata.KindNormal, Window: w}
	lh.Handle(late, watermark)

	retraction := <-lh.Output
	update := <-lh.Output
	fmt.Printf("retraction:   value=%d kind=%d\n", retraction.Value, retraction.Kind)
	fmt.Printf("update:       value=%d kind=%d\n", update.Value, update.Kind)

	m := lh.SnapshotMetrics()
	fmt.Printf("metrics:      accepted=%d dropped=%d retractions=%d\n",
		m.LateAccepted, m.LateDropped, m.Retractions)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial fire: value=1000 kind=0
retraction:   value=1000 kind=1
update:       value=1042 kind=2
metrics:      accepted=1 dropped=0 retractions=1
```

The kinds print as `0` (`KindNormal`), `1` (`KindRetraction`), and `2` (`KindUpdate`). A downstream summing consumer sees `1000` then `-1000` then `+1042` and ends on the correct `1042`.

### Tests

`TestLateHandlerDiscard` proves the cheapest policy counts and drops. `TestLateHandlerAcceptAccumulating` and `TestLateHandlerAccumulatingRetracting` cover the two re-fire modes, the latter asserting the retraction-then-update order. `TestLateHandlerBeyondLatenessDropped` and `TestLateHandlerSideOutput` check the two ways a record past the deadline is handled. `TestLateHandlerMultipleLateRecords` proves the accumulator folds several stragglers correctly. `TestLateHandlerPurgeRemovesState` proves a purged window can no longer accept.

Create `handler_test.go`:

```go
package latedata

import (
	"testing"
	"time"
)

var (
	base = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	w10  = Window{Start: base, End: base.Add(10 * time.Second)}
)

func TestLateHandlerDiscard(t *testing.T) {
	t.Parallel()
	lh := NewLateHandler(LateDiscard, 10*time.Second, false, 16)
	lh.WindowFired(w10, 100)
	<-lh.Output

	watermark := base.Add(15 * time.Second)
	lh.Handle(Record{Value: 5, Kind: KindNormal, Window: w10}, watermark)

	m := lh.SnapshotMetrics()
	if m.LateDropped != 1 {
		t.Fatalf("LateDropped = %d, want 1", m.LateDropped)
	}
	if m.LateAccepted != 0 {
		t.Fatalf("LateAccepted = %d, want 0", m.LateAccepted)
	}
}

func TestLateHandlerAcceptAccumulating(t *testing.T) {
	t.Parallel()
	lh := NewLateHandler(LateAccept, 30*time.Second, false, 16)
	lh.WindowFired(w10, 100)
	<-lh.Output // drain initial fire

	// Watermark 15s: past window.End (10s) but within allowedLateness (40s).
	watermark := base.Add(15 * time.Second)
	lh.Handle(Record{Value: 5, Kind: KindNormal, Window: w10}, watermark)

	result := <-lh.Output
	if result.Kind != KindNormal {
		t.Fatalf("kind = %v, want KindNormal", result.Kind)
	}
	if result.Value != 105 {
		t.Fatalf("value = %d, want 105 (100 + 5)", result.Value)
	}
	m := lh.SnapshotMetrics()
	if m.LateAccepted != 1 {
		t.Fatalf("LateAccepted = %d, want 1", m.LateAccepted)
	}
}

func TestLateHandlerAccumulatingRetracting(t *testing.T) {
	t.Parallel()
	lh := NewLateHandler(LateAccept, 30*time.Second, true, 16)
	lh.WindowFired(w10, 100)
	<-lh.Output

	watermark := base.Add(15 * time.Second)
	lh.Handle(Record{Value: 5, Kind: KindNormal, Window: w10}, watermark)

	retraction := <-lh.Output
	if retraction.Kind != KindRetraction {
		t.Fatalf("first record kind = %v, want KindRetraction", retraction.Kind)
	}
	if retraction.Value != 100 {
		t.Fatalf("retraction value = %d, want 100 (previous result)", retraction.Value)
	}

	update := <-lh.Output
	if update.Kind != KindUpdate {
		t.Fatalf("second record kind = %v, want KindUpdate", update.Kind)
	}
	if update.Value != 105 {
		t.Fatalf("update value = %d, want 105", update.Value)
	}
	m := lh.SnapshotMetrics()
	if m.Retractions != 1 {
		t.Fatalf("Retractions = %d, want 1", m.Retractions)
	}
}

func TestLateHandlerBeyondLatenessDropped(t *testing.T) {
	t.Parallel()
	lh := NewLateHandler(LateAccept, 10*time.Second, false, 16)
	lh.WindowFired(w10, 100)
	<-lh.Output

	// window.End + allowedLateness = 10s + 10s = 20s; watermark at 25s -> beyond.
	watermark := base.Add(25 * time.Second)
	lh.Handle(Record{Value: 5, Kind: KindNormal, Window: w10}, watermark)

	m := lh.SnapshotMetrics()
	if m.LateDropped != 1 {
		t.Fatalf("LateDropped = %d, want 1 (beyond allowedLateness)", m.LateDropped)
	}
	if m.LateAccepted != 0 {
		t.Fatalf("LateAccepted = %d, want 0", m.LateAccepted)
	}
}

func TestLateHandlerSideOutput(t *testing.T) {
	t.Parallel()
	lh := NewLateHandler(LateSideOutput, 10*time.Second, false, 16)
	lh.WindowFired(w10, 100)
	<-lh.Output

	// window.End (10s) + allowedLateness (10s) = 20s; watermark 25s -> side output.
	watermark := base.Add(25 * time.Second)
	r := Record{Value: 42, Kind: KindNormal, Window: w10}
	lh.Handle(r, watermark)

	sideOut := <-lh.SideOutput
	if sideOut.Value != 42 {
		t.Fatalf("side output value = %d, want 42", sideOut.Value)
	}
	m := lh.SnapshotMetrics()
	if m.LateDropped != 1 {
		t.Fatalf("LateDropped = %d, want 1", m.LateDropped)
	}
}

func TestLateHandlerMultipleLateRecords(t *testing.T) {
	t.Parallel()
	lh := NewLateHandler(LateAccept, time.Minute, false, 32)
	lh.WindowFired(w10, 10)
	<-lh.Output

	watermark := base.Add(12 * time.Second)
	for _, v := range []int64{1, 2, 3} {
		lh.Handle(Record{Value: v, Kind: KindNormal, Window: w10}, watermark)
	}
	// Expected results: 10+1=11, 10+1+2=13, 10+1+2+3=16.
	wantSums := []int64{11, 13, 16}
	for i, want := range wantSums {
		r := <-lh.Output
		if r.Value != want {
			t.Fatalf("result[%d] = %d, want %d", i, r.Value, want)
		}
	}
}

func TestLateHandlerPurgeRemovesState(t *testing.T) {
	t.Parallel()
	lh := NewLateHandler(LateAccept, 30*time.Second, false, 16)
	lh.WindowFired(w10, 100)
	<-lh.Output

	// Purge with a time far past the window's purgeAt deadline.
	lh.Purge(base.Add(time.Hour))

	// The window state is gone, so a record that would otherwise be within
	// lateness is dropped instead of accepted.
	watermark := base.Add(15 * time.Second)
	lh.Handle(Record{Value: 5, Kind: KindNormal, Window: w10}, watermark)

	m := lh.SnapshotMetrics()
	if m.LateAccepted != 0 {
		t.Fatalf("LateAccepted = %d, want 0 (window purged)", m.LateAccepted)
	}
	if m.LateDropped != 1 {
		t.Fatalf("LateDropped = %d, want 1 (window purged)", m.LateDropped)
	}
}
```

## Review

The handler is correct when every late record is accounted for — accepted, dropped, or side-output — and the counters add up. The classic deadlock is sending on `Output` while holding `lh.mu`: if the channel is full and the consumer is blocked in `Handle` waiting on `lh.mu`, neither side progresses. The code captures `prev` and `newSum` under the lock and sends only after the unlock, and the non-blocking `select` makes a full channel a dropped record rather than a hang. The subtle correctness point is retraction ordering: the retraction (carrying the *previous* aggregate) must be emitted before the update (carrying the *new* aggregate), or a downstream summing consumer reconciles against the wrong baseline. `TestLateHandlerAccumulatingRetracting` pins that order. Finally, `Purge` and `Handle` both take `lh.mu`, so a purge can never race a fold; once a window's deadline passes and it is purged, `Handle` finds it unknown and drops.

## Resources

- [Streaming 102 (Tyler Akidau, O'Reilly)](https://www.oreilly.com/radar/the-world-beyond-batch-streaming-102/) — accumulation modes (discarding, accumulating, accumulating-and-retracting) explained with diagrams.
- [Apache Flink: Allowed Lateness](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/operators/windows/#allowed-lateness) — the production semantics of late firing and side output for dropped records.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the lock guarding the per-window accumulators.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-watermark-tracking.md](01-watermark-tracking.md) | Next: [03-watermark-generators.md](03-watermark-generators.md)
