# Exercise 35: Watermark-Based Event Time Windowing — Process Out-of-Order Streams by Event Time, Not Arrival Time

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A distributed trace collector receives spans in whatever order the network
happens to deliver them -- a span timestamped a few seconds ago can arrive
after one timestamped a minute later, because they traveled different
network paths from different hosts. Grouping those spans by *arrival* time
into windows would put them in the wrong bucket entirely; grouping by
*event* time is correct, but it means a window can never close the instant
its wall-clock deadline passes, because a slightly-late event for it might
still be in flight. A watermark -- the latest event time seen so far, minus
a tolerated lateness budget -- is what tells the windower when it is finally
safe to stop waiting and close a window. This exercise is an independent
module with its own `go mod init`.

## What you'll build

```text
watermark/                 independent module: example.com/watermark-based-event-time-windowing
  go.mod                    module example.com/watermark-based-event-time-windowing
  watermark.go              Event, Window, Windower, New, Process, LateDrops
  cmd/
    demo/
      main.go               runnable demo: 6 out-of-order events, 3 windows, one late drop
  watermark_test.go          out-of-order acceptance, simultaneous window emission, EOF flush, early-stop, panics
```

Implement: `New(size, lateness time.Duration) *Windower`, `(*Windower) Process(events iter.Seq[Event]) iter.Seq[Window]` yielding each tumbling window as soon as the watermark proves it closed (plus any still-open windows at end-of-stream), and `(*Windower) LateDrops() int`.
Test: events out of order but within the lateness budget land in the correct window; two windows that both become ready from a single watermark advance are yielded together in ascending start order; the final open window is flushed at end-of-stream; an event whose window the watermark has already passed is dropped and counted, never fabricated into a phantom bucket; a consumer break stops the source; a non-positive window size or negative lateness panics.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/09-range-over-integers-and-functions/35-watermark-based-event-time-windowing/cmd/demo
cd go-solutions/03-control-flow/09-range-over-integers-and-functions/35-watermark-based-event-time-windowing
go mod edit -go=1.24
```

The single detail that makes `Process` correct instead of subtly
self-defeating is the *order* of two operations inside the loop: checking
whether the current event is too late has to use the watermark as it stood
*before* this event is folded in, and only afterward should the watermark
advance to account for this event's own timestamp. Reverse that order --
compute the new watermark from the event first, then check the event
against it -- and the newest event in the entire stream can end up
rejecting itself: its own timestamp minus the lateness budget can never
land after its own window's end, so a same-event self-check would always
say "too late." Checking against the *prior* watermark first is what
guarantees the newest event is always accepted while a genuinely stale one
-- whose window a previous, later event already proved closed -- still gets
correctly dropped. The other subtlety is that `Process` tracks every
currently open window in a map rather than assuming only one window can be
in flight at a time: because the lateness budget can span more than one
window's worth of wall-clock time, two or three windows can legitimately be
open simultaneously, and a single watermark advance can prove several of
them ready at once -- `emitReady` sorts and yields all of them together, in
ascending `Start` order, rather than only ever considering the single
oldest one.

Create `watermark.go`:

```go
package watermark

import (
	"iter"
	"sort"
	"time"
)

// Event is one timestamped observation, arriving in whatever order the
// network delivers it -- not necessarily sorted by Timestamp.
type Event struct {
	Timestamp time.Time
	Value     string
}

// Window is a closed tumbling window's collected values, in the order
// their events were accepted.
type Window struct {
	Start, End time.Time
	Values     []string
}

// Windower groups events into fixed-size tumbling windows keyed by event
// time (Timestamp), not arrival time, and uses a watermark to decide when a
// window is safe to close: the watermark is the latest Timestamp seen so
// far minus an allowed-lateness budget, and a window closes only once the
// watermark passes its End. This is what lets the windower tolerate
// realistic out-of-order delivery -- an event a few seconds late still
// lands in the right window -- while still eventually emitting every
// window instead of waiting forever for perfectly ordered input.
type Windower struct {
	size      time.Duration
	lateness  time.Duration
	lateDrops int
}

// New creates a Windower with tumbling windows of the given size and an
// allowed-lateness budget: an event is accepted into its window as long as
// the watermark has not yet advanced past that window's end. size must be
// positive and lateness must be non-negative.
func New(size, lateness time.Duration) *Windower {
	if size <= 0 {
		panic("watermark: size must be > 0")
	}
	if lateness < 0 {
		panic("watermark: lateness must be >= 0")
	}
	return &Windower{size: size, lateness: lateness}
}

// LateDrops reports how many events arrived after the watermark had already
// passed their window's end -- too late to accept even under the allowed-
// lateness budget. Like Compact's reclaimable count in a WAL, this is only
// meaningful once the whole stream has been consumed, so it should be read
// after the range loop over Process finishes.
func (w *Windower) LateDrops() int { return w.lateDrops }

// Process groups events into tumbling windows and yields each Window as
// soon as the watermark proves it is closed, plus any windows still open
// at end-of-stream, all in ascending Start order. The critical ordering
// rule inside the loop is that an event's own lateness check always uses
// the watermark as it stood *before* that event is folded in: computing the
// watermark from the current event first and then checking that same
// event against it would let the newest event in the stream reject itself,
// since its own timestamp minus lateness can never be later than its own
// window's end. Checking against the prior watermark first, and only
// advancing the watermark afterward, is what keeps the newest event always
// accepted while still rejecting genuinely stale ones.
func (w *Windower) Process(events iter.Seq[Event]) iter.Seq[Window] {
	return func(yield func(Window) bool) {
		open := make(map[time.Time]*Window)
		var watermark time.Time
		hasWatermark := false

		emitReady := func() bool {
			var ready []time.Time
			for start, win := range open {
				if !win.End.After(watermark) {
					ready = append(ready, start)
				}
			}
			sort.Slice(ready, func(i, j int) bool { return ready[i].Before(ready[j]) })
			for _, start := range ready {
				win := open[start]
				delete(open, start)
				if !yield(*win) {
					return false
				}
			}
			return true
		}

		for e := range events {
			ws := e.Timestamp.Truncate(w.size)
			we := ws.Add(w.size)

			if hasWatermark && !we.After(watermark) {
				w.lateDrops++
			} else {
				win, ok := open[ws]
				if !ok {
					win = &Window{Start: ws, End: we}
					open[ws] = win
				}
				win.Values = append(win.Values, e.Value)
			}

			candidate := e.Timestamp.Add(-w.lateness)
			if !hasWatermark || candidate.After(watermark) {
				watermark = candidate
				hasWatermark = true
			}

			if !emitReady() {
				return
			}
		}

		var remaining []time.Time
		for start := range open {
			remaining = append(remaining, start)
		}
		sort.Slice(remaining, func(i, j int) bool { return remaining[i].Before(remaining[j]) })
		for _, start := range remaining {
			if !yield(*open[start]) {
				return
			}
		}
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/watermark-based-event-time-windowing"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	// Arrival order, not event-time order.
	raw := []watermark.Event{
		{Timestamp: base.Add(5 * time.Second), Value: "a"},
		{Timestamp: base.Add(65 * time.Second), Value: "b"},
		{Timestamp: base.Add(40 * time.Second), Value: "c"},
		{Timestamp: base.Add(95 * time.Second), Value: "d"},
		{Timestamp: base.Add(50 * time.Second), Value: "e"}, // too late once window 0 has closed
		{Timestamp: base.Add(130 * time.Second), Value: "f"},
	}
	src := func(yield func(watermark.Event) bool) {
		for _, e := range raw {
			if !yield(e) {
				return
			}
		}
	}

	w := watermark.New(time.Minute, 30*time.Second)
	for win := range w.Process(src) {
		fmt.Printf("[%s,%s) values=%v\n", win.Start.Format("15:04:05"), win.End.Format("15:04:05"), win.Values)
	}
	fmt.Printf("late drops: %d\n", w.LateDrops())
}
```

### The runnable demo

```bash
go run ./cmd/demo
```

Expected output:

```
[00:00:00,00:01:00) values=[a c]
[00:01:00,00:02:00) values=[b d]
[00:02:00,00:03:00) values=[f]
late drops: 1
```

`c` (event time `00:40`) arrives after `b` (event time `01:05`) but still
lands correctly in the `[00:00,01:00)` window because the watermark, driven
only by `b`, has only reached `00:35` at that point -- well before the
30-second lateness budget would have closed window 0. `e` (event time
`00:50`) arrives after `d` (event time `01:35`) has pushed the watermark to
`01:05`, which is already past window 0's `01:00` end, so `e` is rejected
and counted in `late drops` instead of silently vanishing.

### Tests

Create `watermark_test.go`:

```go
package watermark

import (
	"testing"
	"time"
)

func eventSeq(events []Event) func(yield func(Event) bool) {
	return func(yield func(Event) bool) {
		for _, e := range events {
			if !yield(e) {
				return
			}
		}
	}
}

func TestProcessOutOfOrderEventsWithinLatenessBudget(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := []Event{
		{Timestamp: base.Add(5 * time.Second), Value: "a"},
		{Timestamp: base.Add(65 * time.Second), Value: "b"},
		{Timestamp: base.Add(40 * time.Second), Value: "c"}, // out of order, still within lateness
		{Timestamp: base.Add(95 * time.Second), Value: "d"},
		{Timestamp: base.Add(50 * time.Second), Value: "e"}, // too late: window 0 already closed
		{Timestamp: base.Add(130 * time.Second), Value: "f"},
	}

	w := New(time.Minute, 30*time.Second)
	var got []Window
	for win := range w.Process(eventSeq(raw)) {
		got = append(got, win)
	}

	if len(got) != 3 {
		t.Fatalf("got %d windows, want 3: %+v", len(got), got)
	}
	checks := []struct {
		wantStart time.Duration
		wantVals  []string
	}{
		{0, []string{"a", "c"}},
		{time.Minute, []string{"b", "d"}},
		{2 * time.Minute, []string{"f"}},
	}
	for i, c := range checks {
		if !got[i].Start.Equal(base.Add(c.wantStart)) {
			t.Fatalf("window[%d].Start = %v, want %v", i, got[i].Start, base.Add(c.wantStart))
		}
		if len(got[i].Values) != len(c.wantVals) {
			t.Fatalf("window[%d].Values = %v, want %v", i, got[i].Values, c.wantVals)
		}
		for j := range c.wantVals {
			if got[i].Values[j] != c.wantVals[j] {
				t.Fatalf("window[%d].Values[%d] = %s, want %s", i, j, got[i].Values[j], c.wantVals[j])
			}
		}
	}
	if w.LateDrops() != 1 {
		t.Fatalf("LateDrops() = %d, want 1", w.LateDrops())
	}
}

func TestProcessEmitsMultipleWindowsSimultaneouslyReady(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	// Window 0 [0,60) and window 1 [60,120) both get exactly one event each,
	// and a single far-future event advances the watermark enough to close
	// both at once, in ascending Start order.
	raw := []Event{
		{Timestamp: base.Add(10 * time.Second), Value: "w0"},
		{Timestamp: base.Add(70 * time.Second), Value: "w1"},
		{Timestamp: base.Add(10 * time.Hour), Value: "far-future"},
	}

	w := New(time.Minute, 0)
	var starts []time.Duration
	for win := range w.Process(eventSeq(raw)) {
		starts = append(starts, win.Start.Sub(base))
	}
	// The far-future event opens its own window too, flushed at EOF.
	if len(starts) != 3 {
		t.Fatalf("got %d windows, want 3: %v", len(starts), starts)
	}
	if starts[0] != 0 || starts[1] != time.Minute {
		t.Fatalf("first two windows out of order: %v", starts)
	}
}

func TestProcessFlushesOpenWindowsAtEOF(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := []Event{{Timestamp: base, Value: "solo"}}

	w := New(time.Minute, 30*time.Second)
	var got []Window
	for win := range w.Process(eventSeq(raw)) {
		got = append(got, win)
	}
	if len(got) != 1 || len(got[0].Values) != 1 || got[0].Values[0] != "solo" {
		t.Fatalf("got %+v, want a single flushed window with one value", got)
	}
	if w.LateDrops() != 0 {
		t.Fatalf("LateDrops() = %d, want 0", w.LateDrops())
	}
}

func TestProcessStopsUpstreamOnBreak(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	calls := 0
	src := func(yield func(Event) bool) {
		for i := 0; i < 100; i++ {
			calls++
			// One event exactly at each window boundary: with zero
			// lateness, event i's timestamp becomes the watermark that
			// closes window i-1, one call after that window opened.
			e := Event{Timestamp: base.Add(time.Duration(i) * time.Minute), Value: "v"}
			if !yield(e) {
				return
			}
		}
	}

	w := New(time.Minute, 0)
	seen := 0
	for range w.Process(src) {
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("seen = %d, want 2", seen)
	}
	// Window 0 closes only once event 1 arrives (calls=2), and window 1
	// only once event 2 arrives (calls=3) -- an event never closes the very
	// window it just opened, only a strictly later one.
	if calls != 3 {
		t.Fatalf("calls = %d, want 3: the source must stop, not run to completion", calls)
	}
}

func TestNewPanicsOnInvalidArgs(t *testing.T) {
	t.Parallel()

	t.Run("non-positive size", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		New(0, time.Second)
	})
	t.Run("negative lateness", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		New(time.Minute, -time.Second)
	})
}
```

## Review

`TestProcessStopsUpstreamOnBreak`'s comment is worth internalizing on its
own: an event can never close the very window it just opened, only a
strictly later one, because the watermark used to decide whether a window
is ready is computed from the *current* event and compared against windows
that already exist -- the window the current event just created is by
construction not yet older than the watermark that created it. The mistake
this design avoids is updating the watermark before checking the current
event's own lateness, which -- as this exercise's docstring on `Process`
explains -- would make the single newest event in any stream capable of
rejecting itself, a bug that would only ever surface with real, live,
out-of-order data and never in a hand-written test that happens to feed
events in timestamp order. The reason multiple windows can be open at once
also deserves attention: a naive single-open-window design (correct for
in-order or near-in-order streams) breaks the moment the lateness budget
spans more than one window's duration, silently dropping data that a
production system's SLA explicitly promised to tolerate.

## Resources

- [`iter.Seq` documentation](https://pkg.go.dev/iter#Seq)
- [Akidau, Chernyak, Lax: "Streaming Systems"](https://www.oreilly.com/library/view/streaming-systems/9781491983867/)
- [Apache Beam: watermarks and late data](https://beam.apache.org/documentation/programming-guide/#watermarks-and-late-data)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-write-ahead-log-compaction-iterator.md](34-write-ahead-log-compaction-iterator.md) | Next: [../10-control-flow-debugging-challenge/00-concepts.md](../10-control-flow-debugging-challenge/00-concepts.md)
