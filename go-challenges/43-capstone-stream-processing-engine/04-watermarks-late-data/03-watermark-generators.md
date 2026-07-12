# Exercise 3: Periodic and Punctuated Watermark Generators

The per-source tracker from exercise 1 answers "what is the watermark right now?" but a real engine also has to decide *when to emit one*. There are two strategies, and a senior engineer is expected to know the difference because it sets the latency-versus-overhead profile of the whole pipeline: a periodic generator emits on a timer regardless of traffic, and a punctuated generator emits only at explicit markers in the stream. This exercise builds both behind one interface, with non-advancing watermarks suppressed in both.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
generator.go           Watermark, Generator, PeriodicGenerator, PunctuatedGenerator
cmd/
  demo/
    main.go            same event stream through both strategies
generator_test.go      periodic timer emission, punctuated marker emission, suppression
example_test.go        runnable doc example for the periodic generator
```

- Files: `generator.go`, `cmd/demo/main.go`, `generator_test.go`, `example_test.go`.
- Implement: a `Generator` interface with `OnEvent(eventTime, marker)` and `Emit()`; `PeriodicGenerator` that advances on `Emit`; `PunctuatedGenerator` that advances on a marked `OnEvent`. Both compute `max_observed - bound` and suppress non-advancing watermarks.
- Test: a periodic generator emits the running maximum minus the bound only when `Emit` is called, ignores out-of-order events, and suppresses a tick that adds nothing; a punctuated generator emits only at marker events and suppresses a marker that does not advance the maximum.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Two emission cadences over one watermark formula

Both generators compute the same value — the largest event timestamp seen so far minus a fixed out-of-orderness bound — and both refuse to emit a watermark that is not strictly greater than the last one they emitted. They differ only in *when* the candidate watermark is published. A periodic generator updates its internal maximum on every `OnEvent` but stays silent; the engine's timer calls `Emit` every so often, and only then does a watermark come out. This decouples watermark progress from event arrival: a quiet source still advances on schedule, and a firehose source does not drown the pipeline in one watermark per record. A punctuated generator inverts the control: `Emit` is a no-op, and a watermark is published inside `OnEvent` exactly when the event carries a marker — an end-of-batch sentinel, a flush record, or a boundary flag the source attaches when it knows a safe point has been reached.

The suppression rule is what keeps both honest. The internal maximum can be revisited many times without growing — a periodic timer that fires twice with no new events in between, or two markers around an out-of-order dip — and re-publishing the same watermark would tell downstream operators "progress happened" when none did. The shared `tracker.candidate` method computes the watermark, compares it against the last emitted value, and returns `ok=false` unless it strictly advances. Because the maximum only ever rises, the emitted watermarks form a strictly increasing sequence, which is exactly the monotonicity a downstream window operator relies on.

These generators run inside a single operator thread, as they do in production engines, so they hold no locks and use no atomics; their determinism is what makes them straightforward to test against a fixed event script.

Create `generator.go`:

```go
// Package wmgen implements periodic and punctuated event-time watermark
// generators. Both compute a watermark as the maximum observed event timestamp
// minus a fixed out-of-orderness bound, and both suppress a watermark that does
// not strictly advance the last one emitted. They differ in emission cadence:
// a periodic generator advances on a timer (Emit), a punctuated generator
// advances on a marked event (OnEvent).
package wmgen

import "time"

// Watermark is an emitted event-time watermark.
type Watermark struct {
	Time time.Time
}

// Generator produces watermarks from a stream of event timestamps. OnEvent is
// called for every record; Emit is called by the engine's periodic timer. A
// generator returns (watermark, true) only when the watermark strictly advances.
type Generator interface {
	OnEvent(eventTime time.Time, marker bool) (Watermark, bool)
	Emit() (Watermark, bool)
}

// tracker holds the running maximum and the last emitted watermark shared by
// both generator strategies.
type tracker struct {
	bound       time.Duration
	hasMax      bool
	maxTime     time.Time
	lastEmitted time.Time
	hasEmitted  bool
}

// observe folds an event timestamp into the running maximum.
func (t *tracker) observe(eventTime time.Time) {
	if !t.hasMax || eventTime.After(t.maxTime) {
		t.hasMax = true
		t.maxTime = eventTime
	}
}

// candidate returns the current watermark (max - bound) and reports whether it
// strictly advances the last emitted watermark. A non-advancing candidate is
// suppressed: it returns (Watermark{}, false) and does not update state.
func (t *tracker) candidate() (Watermark, bool) {
	if !t.hasMax {
		return Watermark{}, false
	}
	wm := t.maxTime.Add(-t.bound)
	if t.hasEmitted && !wm.After(t.lastEmitted) {
		return Watermark{}, false
	}
	t.lastEmitted = wm
	t.hasEmitted = true
	return Watermark{Time: wm}, true
}

// PeriodicGenerator emits a watermark only when Emit is called by the engine's
// timer. OnEvent updates the running maximum but never emits.
type PeriodicGenerator struct {
	t tracker
}

// NewPeriodicGenerator creates a periodic generator with the given
// out-of-orderness bound.
func NewPeriodicGenerator(bound time.Duration) *PeriodicGenerator {
	return &PeriodicGenerator{t: tracker{bound: bound}}
}

// OnEvent folds the event timestamp into the running maximum. The marker is
// ignored by a periodic generator. It never emits.
func (g *PeriodicGenerator) OnEvent(eventTime time.Time, marker bool) (Watermark, bool) {
	g.t.observe(eventTime)
	return Watermark{}, false
}

// Emit publishes the current watermark if it strictly advances. The engine
// calls this on a fixed timer.
func (g *PeriodicGenerator) Emit() (Watermark, bool) {
	return g.t.candidate()
}

// PunctuatedGenerator emits a watermark inside OnEvent when the event carries a
// marker. Emit is a no-op.
type PunctuatedGenerator struct {
	t tracker
}

// NewPunctuatedGenerator creates a punctuated generator with the given
// out-of-orderness bound.
func NewPunctuatedGenerator(bound time.Duration) *PunctuatedGenerator {
	return &PunctuatedGenerator{t: tracker{bound: bound}}
}

// OnEvent folds the event timestamp into the running maximum and, if the event
// carries a marker, publishes the current watermark when it strictly advances.
func (g *PunctuatedGenerator) OnEvent(eventTime time.Time, marker bool) (Watermark, bool) {
	g.t.observe(eventTime)
	if !marker {
		return Watermark{}, false
	}
	return g.t.candidate()
}

// Emit is a no-op for a punctuated generator: it advances only on marked events.
func (g *PunctuatedGenerator) Emit() (Watermark, bool) {
	return Watermark{}, false
}
```

Both concrete types satisfy `Generator`, so an engine can hold a `Generator` and call `OnEvent` on every record and `Emit` on every timer tick without caring which strategy is wired in — a periodic generator simply returns nothing from `OnEvent`, and a punctuated one returns nothing from `Emit`.

### The runnable demo

The demo runs the same six-event stream — including one out-of-order event and two markers — through both generators. The punctuated generator emits at the two marker events; the periodic generator emits on explicit timer ticks, and a final tick with no new events demonstrates suppression.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/watermark-generators"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	const f = "15:04:05"

	type ev struct {
		offset time.Duration
		marker bool
	}
	events := []ev{
		{5 * time.Second, false},
		{8 * time.Second, false},
		{7 * time.Second, false}, // out of order; maximum stays at 8s
		{12 * time.Second, true}, // marker
		{11 * time.Second, false},
		{20 * time.Second, true}, // marker
	}

	// Punctuated: emits inline, only at marker events.
	punct := wmgen.NewPunctuatedGenerator(2 * time.Second)
	for _, e := range events {
		if wm, ok := punct.OnEvent(base.Add(e.offset), e.marker); ok {
			fmt.Printf("punctuated: marker at %s -> watermark %s\n",
				base.Add(e.offset).UTC().Format(f), wm.Time.UTC().Format(f))
		}
	}

	// Periodic: OnEvent only updates the maximum; the timer drives emission.
	periodic := wmgen.NewPeriodicGenerator(2 * time.Second)
	feed := func(from, to int) {
		for _, e := range events[from:to] {
			periodic.OnEvent(base.Add(e.offset), e.marker)
		}
	}
	tick := func(label string) {
		if wm, ok := periodic.Emit(); ok {
			fmt.Printf("periodic:   %s -> watermark %s\n", label, wm.Time.UTC().Format(f))
		} else {
			fmt.Printf("periodic:   %s -> no advance\n", label)
		}
	}

	feed(0, 3)
	tick("tick 1")
	feed(3, 6)
	tick("tick 2")
	tick("tick 3") // no new events since tick 2
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
punctuated: marker at 00:00:12 -> watermark 00:00:10
punctuated: marker at 00:00:20 -> watermark 00:00:18
periodic:   tick 1 -> watermark 00:00:06
periodic:   tick 2 -> watermark 00:00:18
periodic:   tick 3 -> no advance
```

The out-of-order event at 7s never lowers the maximum (still 8s after the first three events), so tick 1 emits `8s - 2s = 6s`. Both generators converge on `18s` because both have absorbed the same maximum of 20s. Tick 3 sees no new events and is correctly suppressed.

### Tests

`TestPeriodicEmitsMaxMinusBound` pins the formula and the timer cadence. `TestPeriodicNoEventsNoEmit` checks the empty case. `TestPeriodicSuppressesNonAdvancing` proves a second tick with no new maximum emits nothing. `TestPeriodicIgnoresOutOfOrder` proves an older event does not lower the watermark. `TestPunctuatedEmitsOnlyAtMarker` checks that unmarked events stay silent and a marked one emits. `TestPunctuatedSuppressesRegression` proves a marker that does not raise the maximum is suppressed. `TestEmittedWatermarksAreMonotonic` drives a mixed stream and asserts the emitted sequence is strictly increasing.

Create `generator_test.go`:

```go
package wmgen

import (
	"testing"
	"time"
)

var base = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestPeriodicEmitsMaxMinusBound(t *testing.T) {
	t.Parallel()
	g := NewPeriodicGenerator(2 * time.Second)
	g.OnEvent(base.Add(5*time.Second), false)
	g.OnEvent(base.Add(8*time.Second), false)

	wm, ok := g.Emit()
	if !ok {
		t.Fatal("Emit returned ok=false, want a watermark after events")
	}
	want := base.Add(6 * time.Second) // max 8s - bound 2s
	if !wm.Time.Equal(want) {
		t.Fatalf("watermark = %v, want %v", wm.Time, want)
	}
}

func TestPeriodicNoEventsNoEmit(t *testing.T) {
	t.Parallel()
	g := NewPeriodicGenerator(2 * time.Second)
	if _, ok := g.Emit(); ok {
		t.Fatal("Emit returned ok=true before any event")
	}
}

func TestPeriodicSuppressesNonAdvancing(t *testing.T) {
	t.Parallel()
	g := NewPeriodicGenerator(2 * time.Second)
	g.OnEvent(base.Add(8*time.Second), false)

	if _, ok := g.Emit(); !ok {
		t.Fatal("first Emit should produce a watermark")
	}
	// No new events; a second tick must be suppressed.
	if _, ok := g.Emit(); ok {
		t.Fatal("second Emit advanced with no new events; want suppression")
	}
}

func TestPeriodicIgnoresOutOfOrder(t *testing.T) {
	t.Parallel()
	g := NewPeriodicGenerator(2 * time.Second)
	g.OnEvent(base.Add(10*time.Second), false)
	g.OnEvent(base.Add(4*time.Second), false) // older; must not lower the max

	wm, ok := g.Emit()
	if !ok {
		t.Fatal("Emit returned ok=false")
	}
	want := base.Add(8 * time.Second) // max 10s - bound 2s
	if !wm.Time.Equal(want) {
		t.Fatalf("watermark = %v, want %v (out-of-order event ignored)", wm.Time, want)
	}
}

func TestPunctuatedEmitsOnlyAtMarker(t *testing.T) {
	t.Parallel()
	g := NewPunctuatedGenerator(2 * time.Second)

	if _, ok := g.OnEvent(base.Add(8*time.Second), false); ok {
		t.Fatal("unmarked event emitted a watermark")
	}
	wm, ok := g.OnEvent(base.Add(12*time.Second), true)
	if !ok {
		t.Fatal("marked event did not emit a watermark")
	}
	want := base.Add(10 * time.Second) // max 12s - bound 2s
	if !wm.Time.Equal(want) {
		t.Fatalf("watermark = %v, want %v", wm.Time, want)
	}
}

func TestPunctuatedSuppressesRegression(t *testing.T) {
	t.Parallel()
	g := NewPunctuatedGenerator(2 * time.Second)
	g.OnEvent(base.Add(12*time.Second), true) // emits 10s

	// A marker on an older event does not raise the maximum, so it is suppressed.
	if _, ok := g.OnEvent(base.Add(6*time.Second), true); ok {
		t.Fatal("marker on older event emitted; want suppression")
	}
}

func TestEmittedWatermarksAreMonotonic(t *testing.T) {
	t.Parallel()
	g := NewPunctuatedGenerator(time.Second)
	offsets := []time.Duration{5, 3, 9, 7, 14, 2, 20}
	var last time.Time
	have := false
	for _, off := range offsets {
		if wm, ok := g.OnEvent(base.Add(off*time.Second), true); ok {
			if have && !wm.Time.After(last) {
				t.Fatalf("watermark %v did not advance past %v", wm.Time, last)
			}
			last = wm.Time
			have = true
		}
	}
	if !have {
		t.Fatal("no watermark was ever emitted")
	}
}
```

Create `example_test.go`:

```go
package wmgen_test

import (
	"fmt"
	"time"

	"example.com/watermark-generators"
)

func ExamplePeriodicGenerator() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	g := wmgen.NewPeriodicGenerator(2 * time.Second)
	g.OnEvent(base.Add(5*time.Second), false)
	g.OnEvent(base.Add(8*time.Second), false)

	wm, ok := g.Emit()
	fmt.Println(ok)
	fmt.Println(wm.Time.UTC().Format("15:04:05"))
	// Output:
	// true
	// 00:00:06
}
```

## Review

A generator is correct when its emitted watermarks are strictly increasing and it never reports progress that did not happen. The first mistake is re-emitting on every periodic tick: without the `candidate` suppression, a downstream operator receives the same watermark repeatedly and cannot distinguish a stalled source from a progressing one. The second is letting an out-of-order event lower the maximum; `observe` only ever raises `maxTime`, so a late dip is absorbed without effect, which is exactly why subtracting the bound from the maximum yields a monotonic watermark. The third is conflating the two strategies — calling `Emit` on a punctuated generator expecting a watermark, or expecting `OnEvent` to emit on a periodic one. The interface makes the contract explicit: periodic advances on the timer, punctuated advances on the marker, and each returns nothing from the other path.

## Resources

- [Apache Flink: WatermarkGenerator](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/event-time/generating_watermarks/#writing-a-periodic-watermarkgenerator) — the `onEvent` / `onPeriodicEmit` contract this exercise models, including the periodic-versus-punctuated distinction.
- [The Dataflow Model (Akidau et al., VLDB 2015)](https://research.google/pubs/the-dataflow-model-a-practical-approach-to-balancing-correctness-latency-and-cost-in-massive-scale-unbounded-out-of-order-data-processing/) — watermarks as heuristic completeness estimates and the cost of emitting them too eagerly.
- [time.Time.After](https://pkg.go.dev/time#Time.After) — the strict comparison that powers both the running-maximum update and the non-advancing suppression.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-late-data-handler.md](02-late-data-handler.md) | Next: [04-event-time-windows.md](04-event-time-windows.md)
