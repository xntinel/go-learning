# Exercise 33: Process Ordered Events with Watermark Progress Signals

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A stream processor windowing events by time — clicks per minute, orders per
hour — cannot simply close a window when its own wall clock reaches the
window's end, because events from a real network arrive out of order and a
window closed by wall clock alone would drop every event still in flight
when that clock ticked over. Systems like Flink and the Dataflow model
solve this with watermarks: an explicit signal, computed by whatever is
upstream of the processor, asserting "event time has now advanced to at
least T, it is safe to close everything ending at or before T." Production
watermark systems actually carry more than one flavor of that assertion —
a plain time-advanced signal, a stronger completeness guarantee that also
promises a specific count of events, and a straggler notification for an
event whose window already closed — and a processor that does not
classify which flavor it received will either close windows too
aggressively or silently corrupt an aggregate that downstream consumers
already received. This module is fully self-contained: its own `go mod
init`, all code inline, its own demo and tests.

## What you'll build

```text
watermark-stream-processor/   independent module: example.com/watermark-stream-processor
  go.mod                       go 1.24
  streamproc.go                (*Processor).Ingest(ev Event) error; (*Processor).Handle(signal any) ([]WindowResult, error)
  cmd/
    demo/
      main.go                  buffers out-of-order events, closes a window, handles a straggler
  streamproc_test.go             table covering ordering, duplicates, rollback, and window-closed logic
```

- Files: `streamproc.go`, `cmd/demo/main.go`, `streamproc_test.go`.
- Implement: `(*Processor).Ingest(ev Event) error`;
  `(*Processor).Handle(signal any) ([]WindowResult, error)`,
  type-switching on `ProgressWatermark`, `CompletenessWatermark`, and
  `LateArrival`.
- Test: out-of-order events within a window aggregating together, a
  duplicate watermark being an idempotent no-op, a regressed watermark
  being rejected, an event for an already-closed window being refused by
  `Ingest`, a late arrival for a closed window being counted rather than
  merged, a completeness watermark rejecting an undercount and rolling
  back so a corrected retry still succeeds, and an unsupported signal type.

Set up the module:

```bash
mkdir -p ~/go-exercises/watermark-stream-processor/cmd/demo
cd ~/go-exercises/watermark-stream-processor
go mod init example.com/watermark-stream-processor
go mod edit -go=1.24
```

Windows here close only in response to a watermark signal, never by
consulting a real clock — `advance` is a pure function of the watermark
value it is given, which is what makes replaying the exact same event and
watermark sequence always close windows in the same order with the same
contents, regardless of when the code actually runs. `ProgressWatermark`
and `CompletenessWatermark` both funnel into the same `advance` helper with
different `expectedCount` values (`-1` meaning "no check"), which keeps the
window-closing and result-aggregation logic — walking every buffered
window, sorting its keys for deterministic output, summing values — from
existing in two copies that could drift. What is not shared is the
commit point: `advance` computes every closed window's results *before*
mutating `p.windows` or `p.watermark`, and only commits both after the
completeness check (when there is one) has already passed. A
`CompletenessWatermark` that undercounts rolls back entirely, leaving the
processor able to accept more events for that still-open window and retry
the same watermark once the count is right — exactly the same
validate-before-commit discipline a config reloader uses to avoid ever
exposing a half-applied state. `Ingest` refuses an event whose window has
already closed rather than silently reopening it, because that window's
aggregate may already be in a downstream consumer's hands; a straggler
detected upstream must instead be reported through
`Handle(LateArrival{...})`, which only counts it and never touches
watermark state, since there is no open window left to merge it into.

Create `streamproc.go`:

```go
package streamproc

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// ErrWatermarkRegressed is returned when a watermark signal reports a time
// behind the processor's current watermark, which would require reopening
// windows already closed and emitted.
var ErrWatermarkRegressed = errors.New("streamproc: watermark moved backward")

// ErrIncompleteWindow is returned when a CompletenessWatermark's declared
// ExpectedCount does not match the total number of events actually
// buffered across the windows it closes.
var ErrIncompleteWindow = errors.New("streamproc: completeness watermark undercounts events")

const windowSize = time.Minute

// Event is one data point in the stream, keyed and timestamped by its
// source.
type Event struct {
	Key       string
	Timestamp time.Time
	Value     int
}

// ProgressWatermark asserts that event time has advanced to at least Time:
// every window ending at or before Time is safe to close and emit,
// whatever count of events it holds.
type ProgressWatermark struct{ Time time.Time }

// CompletenessWatermark asserts both that event time has advanced to Time
// AND that every window closing as a result holds exactly ExpectedCount
// events in total — a stronger guarantee than ProgressWatermark, typically
// derived from an upstream system that can actually count its own output
// (a committed partition offset range) rather than merely estimating
// readiness from clock skew.
type CompletenessWatermark struct {
	Time          time.Time
	ExpectedCount int
}

// LateArrival reports an event whose window has already closed and been
// emitted by the time it reached the processor — a straggler the upstream
// system detected itself, rather than an ordinary Event the processor is
// expected to buffer directly.
type LateArrival struct{ Event Event }

// WindowResult is one closed window's aggregate: the sum of every value
// buffered for Key within [Start, Start+windowSize).
type WindowResult struct {
	Key   string
	Start time.Time
	Sum   int
}

// Processor buffers Events into fixed windows and closes them only when
// told to by a watermark signal — never by wall-clock time — so replaying
// the same stream and watermark sequence always closes windows in the same
// order with the same contents, regardless of when the code actually runs.
type Processor struct {
	watermark   time.Time
	windows     map[time.Time]map[string]int
	lateDropped int
}

// NewProcessor returns a Processor whose watermark starts at the zero
// time, so the first watermark signal received establishes the initial
// horizon.
func NewProcessor() *Processor {
	return &Processor{windows: make(map[time.Time]map[string]int)}
}

func windowStart(ts time.Time) time.Time {
	return ts.Truncate(windowSize)
}

// Ingest buffers ev into its window. It refuses an event whose window has
// already closed (its window start is before the current watermark),
// because a closed window's aggregate has already been emitted and
// mutating it after the fact would silently corrupt a result a downstream
// consumer already received. Such an event must instead be reported
// through Handle(LateArrival{ev}).
func (p *Processor) Ingest(ev Event) error {
	start := windowStart(ev.Timestamp)
	if start.Before(p.watermark) {
		return fmt.Errorf("streamproc: event for closed window %s already emitted; report via LateArrival", start.Format(time.RFC3339))
	}
	if p.windows[start] == nil {
		p.windows[start] = make(map[string]int)
	}
	p.windows[start][ev.Key] += ev.Value
	return nil
}

// Handle dispatches one watermark-class signal by its concrete type.
// ProgressWatermark and CompletenessWatermark both advance the horizon and
// close every window ending at or before the signal's time; a duplicate
// signal at the current watermark is an idempotent no-op, and a signal
// behind the current watermark is rejected as a regression rather than
// silently reopening already-closed windows. LateArrival never touches
// watermark state at all — it only counts the straggler, because the
// window it belongs to is already gone and there is nothing left to merge
// it into.
func (p *Processor) Handle(signal any) ([]WindowResult, error) {
	switch s := signal.(type) {
	case ProgressWatermark:
		return p.advance(s.Time, -1)

	case CompletenessWatermark:
		return p.advance(s.Time, s.ExpectedCount)

	case LateArrival:
		p.lateDropped++
		return nil, nil

	default:
		return nil, fmt.Errorf("streamproc: unsupported signal type %T", signal)
	}
}

// LateDroppedCount reports how many LateArrival signals have been recorded
// so far.
func (p *Processor) LateDroppedCount() int {
	return p.lateDropped
}

// advance closes every window ending at or before newWatermark.
// expectedCount of -1 means "no completeness check" (a plain
// ProgressWatermark); any other value must equal the total of every value
// summed across every window closed by this call, or the tick is rejected
// as incomplete and neither the windows nor the watermark are mutated,
// leaving the processor able to retry once more events arrive.
func (p *Processor) advance(newWatermark time.Time, expectedCount int) ([]WindowResult, error) {
	if newWatermark.Before(p.watermark) {
		return nil, fmt.Errorf("%w: got %s, current %s", ErrWatermarkRegressed, newWatermark.Format(time.RFC3339), p.watermark.Format(time.RFC3339))
	}
	if newWatermark.Equal(p.watermark) {
		return nil, nil // duplicate watermark tick: idempotent no-op
	}

	var closedStarts []time.Time
	for start := range p.windows {
		end := start.Add(windowSize)
		if !newWatermark.Before(end) {
			closedStarts = append(closedStarts, start)
		}
	}
	sort.Slice(closedStarts, func(i, j int) bool { return closedStarts[i].Before(closedStarts[j]) })

	var results []WindowResult
	total := 0
	for _, start := range closedStarts {
		keyed := p.windows[start]
		keys := make([]string, 0, len(keyed))
		for k := range keyed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			results = append(results, WindowResult{Key: k, Start: start, Sum: keyed[k]})
			total += keyed[k]
		}
	}

	if expectedCount >= 0 && total != expectedCount {
		return nil, fmt.Errorf("%w: got %d events, expected %d", ErrIncompleteWindow, total, expectedCount)
	}

	for _, start := range closedStarts {
		delete(p.windows, start)
	}
	p.watermark = newWatermark
	return results, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/watermark-stream-processor"
)

func main() {
	p := streamproc.NewProcessor()
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	// Two events arrive out of order within the same 1-minute window; the
	// processor buffers by event timestamp, not arrival order, so both
	// still land in the same window.
	_ = p.Ingest(streamproc.Event{Key: "clicks", Timestamp: base.Add(40 * time.Second), Value: 3})
	_ = p.Ingest(streamproc.Event{Key: "clicks", Timestamp: base.Add(10 * time.Second), Value: 2})

	// A ProgressWatermark reaching the end of that window closes it.
	results, err := p.Handle(streamproc.ProgressWatermark{Time: base.Add(time.Minute)})
	if err != nil {
		fmt.Println("progress error:", err)
	}
	for _, r := range results {
		fmt.Printf("closed window %s key=%s sum=%d\n", r.Start.Format("15:04:05"), r.Key, r.Sum)
	}

	// A straggler for the window that just closed arrives late.
	late, err := p.Handle(streamproc.LateArrival{Event: streamproc.Event{Key: "clicks", Timestamp: base.Add(5 * time.Second), Value: 1}})
	if err != nil {
		fmt.Println("late-arrival error:", err)
	}
	fmt.Println("late-arrival result:", late, "total dropped:", p.LateDroppedCount())

	// A duplicate watermark at the same time is a no-op.
	dup, err := p.Handle(streamproc.ProgressWatermark{Time: base.Add(time.Minute)})
	fmt.Println("duplicate watermark results:", dup, "err:", err)

	// A regressed watermark is rejected.
	_, err = p.Handle(streamproc.ProgressWatermark{Time: base})
	fmt.Println("regressed watermark err:", err)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
closed window 12:00:00 key=clicks sum=5
late-arrival result: [] total dropped: 1
duplicate watermark results: [] err: <nil>
regressed watermark err: streamproc: watermark moved backward: got 2026-04-01T12:00:00Z, current 2026-04-01T12:01:00Z
```

The two out-of-order events (arriving at +40s then +10s, but both
timestamped within the same window starting at `12:00:00`) sum to `5`
regardless of arrival order. The late arrival for that same window is
counted (`total dropped: 1`) rather than added to the already-emitted sum
of `5`.

### Tests

Create `streamproc_test.go`:

```go
package streamproc

import (
	"errors"
	"testing"
	"time"
)

func TestProcessor(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	t.Run("out-of-order events within a window are aggregated together", func(t *testing.T) {
		t.Parallel()
		p := NewProcessor()
		if err := p.Ingest(Event{Key: "k", Timestamp: base.Add(40 * time.Second), Value: 3}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		if err := p.Ingest(Event{Key: "k", Timestamp: base.Add(10 * time.Second), Value: 2}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		results, err := p.Handle(ProgressWatermark{Time: base.Add(time.Minute)})
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if len(results) != 1 || results[0].Sum != 5 {
			t.Fatalf("results = %+v, want one window summing to 5", results)
		}
	})

	t.Run("duplicate watermark at the current time is an idempotent no-op", func(t *testing.T) {
		t.Parallel()
		p := NewProcessor()
		wm := base.Add(time.Minute)
		if _, err := p.Handle(ProgressWatermark{Time: wm}); err != nil {
			t.Fatalf("first Handle: %v", err)
		}
		results, err := p.Handle(ProgressWatermark{Time: wm})
		if err != nil {
			t.Fatalf("duplicate Handle: %v", err)
		}
		if results != nil {
			t.Fatalf("duplicate watermark results = %v, want nil", results)
		}
	})

	t.Run("regressed watermark is rejected", func(t *testing.T) {
		t.Parallel()
		p := NewProcessor()
		if _, err := p.Handle(ProgressWatermark{Time: base.Add(2 * time.Minute)}); err != nil {
			t.Fatalf("first Handle: %v", err)
		}
		_, err := p.Handle(ProgressWatermark{Time: base.Add(time.Minute)})
		if !errors.Is(err, ErrWatermarkRegressed) {
			t.Fatalf("Handle err = %v, want ErrWatermarkRegressed", err)
		}
	})

	t.Run("event for an already-closed window is refused by Ingest", func(t *testing.T) {
		t.Parallel()
		p := NewProcessor()
		if _, err := p.Handle(ProgressWatermark{Time: base.Add(time.Minute)}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		err := p.Ingest(Event{Key: "k", Timestamp: base.Add(5 * time.Second), Value: 1})
		if err == nil {
			t.Fatal("expected Ingest to refuse an event for an already-closed window")
		}
	})

	t.Run("late arrival for a closed window is counted, not merged", func(t *testing.T) {
		t.Parallel()
		p := NewProcessor()
		if _, err := p.Handle(ProgressWatermark{Time: base.Add(time.Minute)}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if _, err := p.Handle(LateArrival{Event: Event{Key: "k", Timestamp: base.Add(5 * time.Second), Value: 1}}); err != nil {
			t.Fatalf("Handle(LateArrival): %v", err)
		}
		if p.LateDroppedCount() != 1 {
			t.Fatalf("LateDroppedCount() = %d, want 1", p.LateDroppedCount())
		}
	})

	t.Run("completeness watermark rejects an undercount and rolls back", func(t *testing.T) {
		t.Parallel()
		p := NewProcessor()
		if err := p.Ingest(Event{Key: "k", Timestamp: base.Add(10 * time.Second), Value: 5}); err != nil {
			t.Fatalf("Ingest: %v", err)
		}
		_, err := p.Handle(CompletenessWatermark{Time: base.Add(time.Minute), ExpectedCount: 10})
		if !errors.Is(err, ErrIncompleteWindow) {
			t.Fatalf("Handle err = %v, want ErrIncompleteWindow", err)
		}
		// Rolled back: the window is still open, so a late-arriving event
		// for it is accepted by Ingest rather than refused.
		if err := p.Ingest(Event{Key: "k", Timestamp: base.Add(15 * time.Second), Value: 5}); err != nil {
			t.Fatalf("Ingest after rollback: %v", err)
		}
		results, err := p.Handle(CompletenessWatermark{Time: base.Add(time.Minute), ExpectedCount: 10})
		if err != nil {
			t.Fatalf("Handle after correction: %v", err)
		}
		if len(results) != 1 || results[0].Sum != 10 {
			t.Fatalf("results = %+v, want one window summing to 10", results)
		}
	})

	t.Run("unsupported signal type is an error", func(t *testing.T) {
		t.Parallel()
		p := NewProcessor()
		if _, err := p.Handle("not-a-signal"); err == nil {
			t.Fatal("expected error for unsupported signal type")
		}
	})
}
```

Verify: `go test -count=1 ./...`

## Review

`advance` is correct because it computes `results` and `total` entirely
from `p.windows` without mutating anything, and only writes `p.windows` and
`p.watermark` after the completeness check has already passed — an
implementation that deleted closed windows before checking the count would
lose the buffered events on a failed completeness check, making a
corrected retry impossible instead of just delayed. The window-closed
guard inside `Ingest` is what keeps a straggler from silently reopening and
mutating an aggregate that may have already been read and shipped
downstream by the time the straggler arrives — refusing it and pushing it
through the dedicated `LateArrival` path is what keeps "already emitted"
actually meaning something. Comparing `newWatermark.Equal(p.watermark)`
before treating anything as new work is the detail a naive implementation
skips, and skipping it would make a duplicated watermark tick — a
perfectly normal occurrence when an upstream watermark generator retries
its own delivery — re-run the same window-closing logic a second time
against windows that have already been deleted, which happens to be
harmless here only because the loop over `p.windows` would simply find
nothing, but is exactly the kind of accidental correctness that breaks the
moment the closing logic does anything with side effects beyond returning
a slice.

## Resources

- [The Dataflow Model (Akidau et al., VLDB 2015)](https://research.google/pubs/pub43864/)
- [Apache Flink: Event Time and Watermarks](https://nightlies.apache.org/flink/flink-docs-stable/docs/concepts/time/)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [time.Time.Truncate](https://pkg.go.dev/time#Time.Truncate)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-request-coalescing-singleflight.md](32-request-coalescing-singleflight.md) | Next: [34-hierarchical-quota-token-enforcement.md](34-hierarchical-quota-token-enforcement.md)
