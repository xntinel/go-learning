# Exercise 9: Starvation detection against a per-class max-wait SLO

Strict priority silently starves the low class, so starvation is invisible until a
customer complains. This exercise builds the observability layer that turns it
into an alert: a detector that records the last-served time per class and emits a
`StarvationEvent` the moment a class exceeds its max-wait SLO — once per breach,
on the transition, driven by an injectable clock so the behavior is deterministic.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
slodetect/                   module example.com/slodetect
  go.mod
  detector.go                Clock; StarvationEvent; Sink; type Detector; NewDetector; Served
  cmd/
    demo/
      main.go                fake clock: saturate high, detect low starvation, print event
  detector_test.go           one-event-per-breach, no-events-within-SLO, transition-not-every-call
```

Files: `detector.go`, `cmd/demo/main.go`, `detector_test.go`.
Implement: `Detector` wrapping a priority consumer; `Served(class)` records a
service and checks every class against its `slo[i]` max-wait, emitting one
`StarvationEvent` on the transition into breach (gated so it does not refire until
the class is served back within SLO). Time comes from an injected `Clock`.
Test (with a fake clock): exactly one event per breach with the correct class and
observed wait; zero events under service within SLO; the event fires on the
transition, not on every call.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/08-select-priority-and-starvation/09-starvation-slo-detector/cmd/demo
cd go-solutions/14-select-and-context/08-select-priority-and-starvation/09-starvation-slo-detector
```

### Making an invisible fairness bug observable

The detector layers on top of any priority consumer (the scheduler from Exercise
5, say): every time the consumer serves an item, it calls `Served(class)`. The
detector keeps, per class, the timestamp of its last service. On each call it
checks every class: if `now - lastServed[c]` exceeds that class's max-wait SLO,
the class is being starved, and an event is emitted to a `Sink` (a metrics or
alerting hook, modeled here as an interface so tests can substitute a recorder).

Two design points make it usable rather than noisy:

- **Fire on the transition, once per episode.** A naive detector that emits every
  time it sees a class over its SLO would produce a flood — one event per dispatch
  for as long as the starvation lasts. A `breached[c]` flag gates it: the event
  fires only when a class *crosses* from within-SLO to over-SLO, and does not
  refire until the class is served back within its budget (which clears the flag).
  One breach episode produces exactly one event.
- **Update the served class before checking.** `Served(class)` records the
  service and clears that class's breach flag *first*, then runs the check. So the
  class you just served is never reported as starved by its own service call, and
  the check only surfaces the *other* classes that have gone quiet.

The clock is injected as a `Clock` function (`func() time.Time`) rather than
calling `time.Now` directly, because the detector's whole contract is time-based:
a test must be able to drive time forward deterministically to assert "at exactly
600 ms over a 500 ms SLO, one event fires." Production passes `time.Now`; the test
passes a fake clock it advances by hand. (A `testing/synctest` bubble is the
alternative when the code under test must call `time.Now` directly; here the
detector is a library whose clock is naturally a dependency, so a plain injected
clock is the simpler fit.)

Create `detector.go`:

```go
package slodetect

import "time"

// Clock returns the current time. Production passes time.Now; tests pass a fake.
type Clock func() time.Time

// StarvationEvent reports that a class exceeded its max-wait SLO.
type StarvationEvent struct {
	Class  int
	Waited time.Duration
}

// Sink receives starvation events; it is the metrics/alerting hook.
type Sink interface {
	Starved(StarvationEvent)
}

// Detector wraps a priority consumer with per-class starvation detection against
// a max-wait SLO. It is single-consumer; call Served from one goroutine.
type Detector struct {
	now        Clock
	slo        []time.Duration
	lastServed []time.Time
	breached   []bool
	sink       Sink
}

// NewDetector builds a Detector. slo[i] is the maximum time class i may go
// unserved before a StarvationEvent fires. Each class's clock starts now.
func NewDetector(now Clock, slo []time.Duration, sink Sink) *Detector {
	start := now()
	last := make([]time.Time, len(slo))
	for i := range last {
		last[i] = start
	}
	return &Detector{
		now:        now,
		slo:        slo,
		lastServed: last,
		breached:   make([]bool, len(slo)),
		sink:       sink,
	}
}

// Served records that class was just served, then checks every class for an SLO
// breach as of now. A breach emits exactly one event on the transition into
// breach and does not refire until the class is served back within its SLO.
func (d *Detector) Served(class int) {
	d.lastServed[class] = d.now()
	d.breached[class] = false
	d.check()
}

func (d *Detector) check() {
	t := d.now()
	for c := range d.slo {
		waited := t.Sub(d.lastServed[c])
		if waited > d.slo[c] {
			if !d.breached[c] {
				d.sink.Starved(StarvationEvent{Class: c, Waited: waited})
				d.breached[c] = true
			}
		} else {
			d.breached[c] = false
		}
	}
}
```

### The runnable demo

The demo uses a fake clock so the output is deterministic. Class 0 (high, 1 s SLO)
is served repeatedly; class 1 (low, 500 ms SLO) goes unserved. After 600 ms of
serving only class 0, the detector reports class 1 starved.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/slodetect"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

type printSink struct{ count int }

func (p *printSink) Starved(e slodetect.StarvationEvent) {
	p.count++
	fmt.Printf("starvation: class=%d waited=%s\n", e.Class, e.Waited)
}

func main() {
	clk := &clock{t: time.Unix(0, 0).UTC()}
	sink := &printSink{}
	d := slodetect.NewDetector(clk.now, []time.Duration{time.Second, 500 * time.Millisecond}, sink)

	d.Served(0) // serve high
	clk.advance(600 * time.Millisecond)
	d.Served(0) // serve high again; low is now 600ms > 500ms SLO -> event

	fmt.Printf("events emitted: %d\n", sink.count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
starvation: class=1 waited=600ms
events emitted: 1
```

### Tests

A fake clock makes every assertion exact. `TestOneEventPerBreach` saturates class 0
past class 1's SLO and asserts precisely one event, with the right class and
observed wait, even though class 0 is served several more times while class 1 stays
starved (proving the transition gate). `TestNoEventsWithinSLO` serves both classes
inside their budgets and asserts zero events. `TestRefiresAfterRecovery` shows a
class can breach, recover (be served), and breach again as two distinct events.

Create `detector_test.go`:

```go
package slodetect

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

type recordingSink struct{ events []StarvationEvent }

func (r *recordingSink) Starved(e StarvationEvent) { r.events = append(r.events, e) }

func TestOneEventPerBreach(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	sink := &recordingSink{}
	d := NewDetector(clk.now, []time.Duration{time.Second, 500 * time.Millisecond}, sink)

	d.Served(0)
	clk.advance(600 * time.Millisecond)
	d.Served(0) // class 1 waited 600ms > 500ms: one event
	clk.advance(100 * time.Millisecond)
	d.Served(0) // class 1 waited 700ms, still breached: no new event
	clk.advance(100 * time.Millisecond)
	d.Served(0) // class 1 waited 800ms, still breached: no new event

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want exactly 1 (transition, not every call)", len(sink.events))
	}
	got := sink.events[0]
	if got.Class != 1 || got.Waited != 600*time.Millisecond {
		t.Fatalf("event = %+v, want class 1 waited 600ms", got)
	}
}

func TestNoEventsWithinSLO(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	sink := &recordingSink{}
	d := NewDetector(clk.now, []time.Duration{time.Second, 500 * time.Millisecond}, sink)

	// Serve both classes well within their SLOs.
	for range 5 {
		d.Served(0)
		clk.advance(100 * time.Millisecond)
		d.Served(1)
		clk.advance(100 * time.Millisecond)
	}
	if len(sink.events) != 0 {
		t.Fatalf("events = %d, want 0 (both served within SLO)", len(sink.events))
	}
}

func TestRefiresAfterRecovery(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{t: time.Unix(0, 0).UTC()}
	sink := &recordingSink{}
	d := NewDetector(clk.now, []time.Duration{time.Second, 500 * time.Millisecond}, sink)

	d.Served(0)
	clk.advance(600 * time.Millisecond)
	d.Served(0) // breach 1 for class 1
	d.Served(1) // class 1 recovers (served within SLO), clearing the flag
	clk.advance(600 * time.Millisecond)
	d.Served(0) // breach 2 for class 1

	if len(sink.events) != 2 {
		t.Fatalf("events = %d, want 2 (breach, recover, breach)", len(sink.events))
	}
}
```

## Review

The detector is correct when a breach produces exactly one event on the transition
(`TestOneEventPerBreach`), service within budget produces none
(`TestNoEventsWithinSLO`), and a recovered-then-restarved class produces a second
distinct event (`TestRefiresAfterRecovery`). The mistake that ruins it in
production is emitting on every observation rather than on the transition — the
`breached[c]` flag is what converts a starvation flood into a single actionable
alert, and dropping it turns the detector into the noise operators learn to
ignore. Updating the served class before the check keeps a class from reporting
itself starved. The injected `Clock` is what makes the time-based contract
testable without sleeping; in production you pass `time.Now`, and the detector
becomes the metric that tells you tenant B is being starved before tenant B tells
you.

## Resources

- [`time.Time.Sub`](https://pkg.go.dev/time#Time.Sub) and [`time.Duration`](https://pkg.go.dev/time#Duration) — computing and comparing the observed wait.
- [Google SRE Workbook: Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/) — firing on breach transitions, not on every sample.
- [Go spec: Method values](https://go.dev/ref/spec#Method_values) — passing `clk.now` as a `Clock` function.

---

Back to [08-dynamic-priority-reflect-select.md](08-dynamic-priority-reflect-select.md) | Next: [../09-context-in-http-servers-clients/00-concepts.md](../09-context-in-http-servers-clients/00-concepts.md)
