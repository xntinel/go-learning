# Exercise 31: Watermark-Based Stream Window Aggregation with Late Arrivals

**Nivel: Intermedio** — validacion rapida (un test corto).

A stream processor ingesting from multiple partitions — one per Kafka
partition, one per producer shard — sees events in roughly time order within
a partition but with no ordering guarantee across partitions, so a window
aggregator that closes `[0,10)` the instant it sees any event with
timestamp `>= 10` would close it while a lagging partition still has events
timestamped `2` in flight, silently losing them. A watermark solves this by
tracking, per partition, the highest event timestamp seen, and treating the
stream's overall progress as the *slowest* partition's progress — a window
only closes once every partition has individually advanced past its end.
This module tracks per-partition high-water marks, buffers events per
window, ranges the buffered windows to emit every one the watermark has now
proven complete, and separately counts the straggler events that arrive
after their own window has already closed. The module is fully self-
contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
streamwin/                  independent module: example.com/watermark-stream-window-aggregation
  go.mod                    go 1.24
  streamwin.go              type Aggregator; Ingest, Poll, LateCount
  cmd/
    demo/
      main.go               runnable demo: 2 partitions, out-of-order arrival, 1 straggler
  streamwin_test.go          table-style tests: window closing, straggler counting, self-advance edge case
```

- Files: `streamwin.go`, `cmd/demo/main.go`, `streamwin_test.go`.
- Implement: `Aggregator.Ingest`, `Aggregator.Poll`, and
  `Aggregator.LateCount`, all built on ranging per-partition watermarks and
  buffered windows.
- Test: a window-closing case, a straggler-counting case, and the edge case
  where an event must not be rejected as late against its own advance.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/05-range-over-collections/31-watermark-stream-window-aggregation/cmd/demo
cd go-solutions/03-control-flow/05-range-over-collections/31-watermark-stream-window-aggregation
go mod edit -go=1.24
```

### The watermark is a range over partitions, not a single running maximum

A single-stream aggregator can track "progress" as one running maximum
timestamp, but a multi-partition stream cannot: if partition A is at
timestamp 25 and partition B is still catching up from timestamp 9, the
*stream's* watermark is 9, not 25, because partition B could still deliver
an event timestamped anywhere up to 9 that has not arrived yet. `watermark`
ranges every entry in `a.partitionMax` and returns the minimum, which is
exactly the guarantee a window's closure depends on: `Poll` only emits a
window once `start+size <= watermark`, meaning *every* partition has proven
it is done producing events for that window, not just the fastest one.
`Poll`'s own range is over the buffered windows, not the events inside
them — it collects the window keys whose end has fallen behind the
watermark, sorts them for a deterministic emission order, and only then
ranges each window's buffered events to fold them into a `WindowResult`.

The subtlety that makes `Ingest` correct rather than merely plausible is
*which* watermark it compares a new event's window against. If `Ingest`
computed the watermark *after* updating `a.partitionMax` for the incoming
event and used that updated value to decide lateness, a partition's own
first event past a threshold would sometimes be judged "late" against a
watermark that its own arrival just caused to advance — a self-inflicted
rejection with no real straggler behind it. `Ingest` instead captures
`prevWatermark` *before* touching `a.partitionMax`, so the lateness check
only ever fires for a truly out-of-order arrival: an event whose window
closed under watermark progress that other partitions, not this one, drove
forward.

Create `streamwin.go`:

```go
package streamwin

import "sort"

// Event is one timestamped measurement arriving from a named partition of a
// stream. Timestamp is a Unix-seconds event time, not the time it arrived —
// the whole point of a watermark is to reason about event time under
// out-of-order arrival.
type Event struct {
	Partition string
	Timestamp int64
	Value     float64
}

// WindowResult is the aggregated output of one fully closed window.
type WindowResult struct {
	Start int64
	End   int64
	Count int
	Sum   float64
}

// Aggregator buffers events per fixed-size window and emits a window's
// result only once the stream's watermark proves no more events for it can
// arrive.
type Aggregator struct {
	size         int64
	partitionMax map[string]int64  // each partition's highest event timestamp seen
	buffers      map[int64][]Event // window start -> buffered events
	late         int
}

// New builds an Aggregator with fixed window length size (in seconds).
func New(size int64) *Aggregator {
	return &Aggregator{
		size:         size,
		partitionMax: make(map[string]int64),
		buffers:      make(map[int64][]Event),
	}
}

func windowStart(ts, size int64) int64 {
	return ts - (ts % size)
}

// watermark ranges every partition's high-water mark and returns the
// minimum of them: the stream as a whole has only progressed as far as its
// slowest partition, so no window can be considered closed until every
// partition has advanced past its end.
func (a *Aggregator) watermark() int64 {
	first := true
	var wm int64
	for _, t := range a.partitionMax {
		if first || t < wm {
			wm = t
			first = false
		}
	}
	if first {
		return 0
	}
	return wm
}

// Ingest records one event. The event's own window is compared against the
// watermark as it stood *before* this event's partition advances, so an
// event that itself pushes a lagging partition forward is never rejected as
// late for its own window — only a genuinely out-of-order arrival, whose
// window closed under a watermark this event had no part in advancing, is
// counted as a straggler.
func (a *Aggregator) Ingest(e Event) {
	prevWatermark := a.watermark()

	if e.Timestamp > a.partitionMax[e.Partition] {
		a.partitionMax[e.Partition] = e.Timestamp
	}

	start := windowStart(e.Timestamp, a.size)
	end := start + a.size
	if end <= prevWatermark {
		a.late++
		return
	}
	a.buffers[start] = append(a.buffers[start], e)
}

// Poll ranges the buffered windows and emits every one whose end has fallen
// at or behind the current watermark, removing them from the buffer. The
// results are sorted by window start for a deterministic return order.
func (a *Aggregator) Poll() []WindowResult {
	wm := a.watermark()

	var starts []int64
	for start := range a.buffers {
		if start+a.size <= wm {
			starts = append(starts, start)
		}
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

	results := make([]WindowResult, 0, len(starts))
	for _, start := range starts {
		events := a.buffers[start]
		delete(a.buffers, start)

		r := WindowResult{Start: start, End: start + a.size}
		for _, e := range events {
			r.Count++
			r.Sum += e.Value
		}
		results = append(results, r)
	}
	return results
}

// LateCount reports how many ingested events arrived after their window had
// already closed under the watermark.
func (a *Aggregator) LateCount() int {
	return a.late
}
```

### The runnable demo

The demo feeds two partitions through window `[0,10)` and `[10,20)`,
polling once each partition has advanced enough to close a window, then
ingests one genuine straggler for the already-closed first window.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/watermark-stream-window-aggregation"
)

func main() {
	a := streamwin.New(10)

	a.Ingest(streamwin.Event{Partition: "p1", Timestamp: 2, Value: 2})
	a.Ingest(streamwin.Event{Partition: "p2", Timestamp: 1, Value: 1})
	a.Ingest(streamwin.Event{Partition: "p1", Timestamp: 8, Value: 8})
	a.Ingest(streamwin.Event{Partition: "p2", Timestamp: 9, Value: 9})
	a.Ingest(streamwin.Event{Partition: "p1", Timestamp: 15, Value: 15})
	a.Ingest(streamwin.Event{Partition: "p2", Timestamp: 19, Value: 19})

	for _, w := range a.Poll() {
		fmt.Printf("window[%d,%d) count=%d sum=%.0f\n", w.Start, w.End, w.Count, w.Sum)
	}

	// A genuinely out-of-order arrival: this event's own window [0,10)
	// already closed under the watermark established above.
	a.Ingest(streamwin.Event{Partition: "p2", Timestamp: 4, Value: 4})

	a.Ingest(streamwin.Event{Partition: "p1", Timestamp: 25, Value: 25})
	a.Ingest(streamwin.Event{Partition: "p2", Timestamp: 29, Value: 29})

	for _, w := range a.Poll() {
		fmt.Printf("window[%d,%d) count=%d sum=%.0f\n", w.Start, w.End, w.Count, w.Sum)
	}

	fmt.Printf("late=%d\n", a.LateCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
window[0,10) count=4 sum=20
window[10,20) count=2 sum=34
late=1
```

### Tests

The first test proves `Poll` withholds a window until both partitions have
advanced past its end, the second proves a genuine out-of-order arrival is
counted as late, and the third guards the edge case this design specifically
exists to get right: a single partition's own advancing event must never be
rejected as late against the watermark it is itself establishing.

Create `streamwin_test.go`:

```go
package streamwin

import "testing"

func TestPollEmitsOnlyClosedWindows(t *testing.T) {
	t.Parallel()

	a := New(10)
	a.Ingest(Event{Partition: "p1", Timestamp: 2, Value: 2})
	a.Ingest(Event{Partition: "p2", Timestamp: 1, Value: 1})

	// Neither partition has advanced past window [0,10)'s end yet.
	if got := a.Poll(); len(got) != 0 {
		t.Fatalf("Poll() = %v, want no windows emitted yet", got)
	}

	a.Ingest(Event{Partition: "p1", Timestamp: 15, Value: 15})
	a.Ingest(Event{Partition: "p2", Timestamp: 19, Value: 19})

	got := a.Poll()
	want := []WindowResult{{Start: 0, End: 10, Count: 2, Sum: 3}}
	if len(got) != len(want) {
		t.Fatalf("Poll() = %+v, want %+v", got, want)
	}
	if got[0] != want[0] {
		t.Fatalf("Poll()[0] = %+v, want %+v", got[0], want[0])
	}
}

func TestIngestCountsGenuineStragglersAsLate(t *testing.T) {
	t.Parallel()

	a := New(10)
	a.Ingest(Event{Partition: "p1", Timestamp: 8, Value: 8})
	a.Ingest(Event{Partition: "p2", Timestamp: 9, Value: 9})
	a.Ingest(Event{Partition: "p1", Timestamp: 15, Value: 15})
	a.Ingest(Event{Partition: "p2", Timestamp: 19, Value: 19}) // closes window [0,10)
	a.Poll()

	// A straggler for the already-closed window.
	a.Ingest(Event{Partition: "p2", Timestamp: 4, Value: 4})

	if got := a.LateCount(); got != 1 {
		t.Fatalf("LateCount() = %d, want 1", got)
	}
}

func TestIngestDoesNotRejectTheEventThatAdvancesItsOwnWindow(t *testing.T) {
	t.Parallel()

	a := New(10)
	a.Ingest(Event{Partition: "solo", Timestamp: 25, Value: 25})

	// A single-partition stream: the watermark equals this partition's own
	// max, so window [20,30) containing this very event must never be
	// treated as already late.
	if got := a.LateCount(); got != 0 {
		t.Fatalf("LateCount() = %d, want 0 (the event must not be late against its own advance)", got)
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

The aggregator is correct when `Poll` never emits a window until every
partition's high-water mark has passed that window's end, and `Ingest`
never counts an event as late unless some *other* partition's progress —
not this event's own — already closed its window. The bug this design
specifically avoids is using the watermark computed *after* updating the
incoming event's partition to decide that same event's lateness: that
ordering would make a single-partition stream's very first fast-forward
jump reject itself as its own straggler, since the watermark and the
event's window would advance in lockstep with nothing ever counting as
genuinely late.

## Resources

- [Akidau et al., "The Dataflow Model" (VLDB 2015)](https://research.google/pubs/pub43864/) — the watermark/window model this exercise's `Aggregator` implements a minimal version of.
- [Go Specification: For statements (range over map)](https://go.dev/ref/spec#For_statements)
- [Apache Flink: Event Time and Watermarks](https://nightlies.apache.org/flink/flink-docs-stable/docs/concepts/time/) — a production stream engine's take on the same mechanism.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-graceful-config-reload-dual-write.md](30-graceful-config-reload-dual-write.md) | Next: [32-gossip-protocol-vector-clocks.md](32-gossip-protocol-vector-clocks.md)
