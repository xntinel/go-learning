# Exercise 31: Reconstruct Event Causality from Vector Clock Metadata

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A distributed system with no shared clock — nodes in different regions,
replicas that can drift, clients that submit writes through whichever
partition answers first — cannot order events by wall-clock timestamp
alone, because clock skew makes "earlier timestamp" and "actually happened
first" two different things. A vector clock sidesteps that by having each
node track a counter per node it has heard from, so that comparing two
clocks reveals genuine causality — whether one event's clock is
provably derived from the other's — without any node's wall clock
being trustworthy at all. The hard part in production is not computing the
comparison; it is reconstructing a coherent picture from events that
arrive out of order over the network, while also catching the one failure
mode vector clocks cannot self-correct: a node whose own counter goes
backward, which can only mean a duplicate delivery or a corrupted clock,
never ordinary concurrent activity elsewhere in the cluster. This module is
fully self-contained: its own `go mod init`, all code inline, its own demo
and tests.

## What you'll build

```text
vector-clock-causal-ordering/   independent module: example.com/vector-clock-causal-ordering
  go.mod                        go 1.24
  vclock.go                     Compare(a, b VectorClock) any; (*Processor).Ingest(evt Event) any
  cmd/
    demo/
      main.go                   ingests a root event, a caused event, a concurrent one, and a duplicate
  vclock_test.go                  table of Compare cases, an Ingest sequence, and a concurrency test
```

- Files: `vclock.go`, `cmd/demo/main.go`, `vclock_test.go`.
- Implement: `Compare(a, b VectorClock) any` returning `Before`, `After`,
  or `Concurrent`; `(*Processor).Ingest(evt Event) any` additionally
  returning `Root` for the first event ever ingested and `Anomaly` when an
  event's own node reports a self-counter that failed to strictly
  increase.
- Test: strict precedence in both directions, incomparable clocks,
  identical clocks, disjoint node sets, an `Ingest` sequence walking
  `Root` → `Before` → `Concurrent` → `Anomaly`, and many goroutines from
  distinct nodes ingesting concurrently with no false anomaly and no data
  race.

Set up the module:

```bash
go mod edit -go=1.24
```

`Compare` is deliberately built from two booleans rather than a per-node
subtraction: `aHasGreater` and `bHasGreater` each ask only "does *any*
component of this clock exceed the corresponding component of the other,"
over the union of both clocks' node sets, treating a missing node as
counter zero. That framing is what makes the standard vector clock partial
order fall out almost for free — `a` happened-before `b` exactly when `b`
has a greater component somewhere and `a` does not have one anywhere,
`a` happened-after `b` is the mirror image, and anything else (including
two identical clocks, which trivially satisfy neither condition) is
`Concurrent`, because no causal relationship can be derived from the
clocks alone. `Processor.Ingest` layers a second, independent check ahead
of `Compare`: before ever comparing clocks, it checks whether `evt`'s own
node reported a self-counter that failed to strictly increase from the
last self-counter that node reported. A correctly functioning vector clock
implementation increments a node's own component by exactly one for every
event that node produces, so a non-increasing self-counter is never
ordinary concurrent activity — it can only be a duplicate delivery, a
replayed event, or a clock that was reset, which is why it short-circuits
into `Anomaly` before `Compare` ever runs on data that cannot be trusted.

Create `vclock.go`:

```go
package vclock

import "sync"

// VectorClock maps each node's identifier to the number of events that node
// has produced, counted from that node's own point of view. A component
// missing from the map is treated as zero, so two clocks with different
// node sets still compare correctly against the union of both.
type VectorClock map[string]uint64

// Event is one message carrying the vector clock its producer stamped it
// with at send time.
type Event struct {
	Node  string
	Clock VectorClock
}

// Before means the first clock happened-before the second: every component
// of the first is less than or equal to the corresponding component of the
// second, with at least one strictly less.
type Before struct{}

// After means the second clock happened-before the first — the mirror
// image of Before, returned when comparing (a, b) but b caused a.
type After struct{}

// Concurrent means neither clock's components dominate the other's: the
// two events happened independently, with no causal relationship
// recoverable from the clocks alone. Two identical clocks are also reported
// as Concurrent, since neither strictly dominates the other.
type Concurrent struct{}

// Root marks the first event a Processor has ever ingested, which has
// nothing prior in its history to compare against.
type Root struct{}

// Anomaly means evt's own node reported a self-counter that did not
// strictly increase from the last self-counter that node reported. A
// correctly functioning vector clock implementation increments a node's own
// component by exactly one for every event that node produces, so a
// non-increasing self-counter can only come from a duplicate delivery, a
// replayed event, or a clock that was reset — never from ordinary
// concurrent activity elsewhere in the cluster, which is why it is reported
// distinctly from Concurrent rather than folded into it.
type Anomaly struct {
	Node     string
	Observed uint64
	Expected uint64
}

// Compare classifies the causal relationship between two vector clocks
// using the standard component-wise partial order over the union of both
// clocks' node sets.
func Compare(a, b VectorClock) any {
	aHasGreater, bHasGreater := false, false

	nodes := make(map[string]struct{}, len(a)+len(b))
	for n := range a {
		nodes[n] = struct{}{}
	}
	for n := range b {
		nodes[n] = struct{}{}
	}
	for n := range nodes {
		switch {
		case a[n] > b[n]:
			aHasGreater = true
		case b[n] > a[n]:
			bHasGreater = true
		}
	}

	switch {
	case aHasGreater && !bHasGreater:
		return After{} // a strictly dominates b: a happened after b
	case bHasGreater && !aHasGreater:
		return Before{} // b strictly dominates a: a happened before b
	default:
		return Concurrent{} // equal clocks, or neither dominates (incomparable)
	}
}

// Processor reconstructs causal order from a stream of events that may
// arrive out of order, using only the vector clock each event carries.
type Processor struct {
	mu        sync.Mutex
	lastSelf  map[string]uint64
	lastEvent *Event
}

// NewProcessor returns a Processor ready to ingest events.
func NewProcessor() *Processor {
	return &Processor{lastSelf: make(map[string]uint64)}
}

// Ingest classifies evt's relationship to whatever was ingested most
// recently, then records it into history. Before comparing, it first
// checks evt's own node self-counter against the last self-counter that
// node reported: a self-counter that failed to strictly increase is an
// Anomaly and short-circuits before ever reaching Compare, since a
// corrupted clock cannot be trusted to classify causality correctly. The
// self-counter check and the history update happen in the same lock, so
// concurrent ingestion of well-formed events from many nodes never races on
// either.
func (p *Processor) Ingest(evt Event) any {
	p.mu.Lock()
	defer p.mu.Unlock()

	self := evt.Clock[evt.Node]
	if last, ok := p.lastSelf[evt.Node]; ok && self <= last {
		return Anomaly{Node: evt.Node, Observed: self, Expected: last + 1}
	}
	p.lastSelf[evt.Node] = self

	if p.lastEvent == nil {
		e := evt
		p.lastEvent = &e
		return Root{}
	}

	rel := Compare(p.lastEvent.Clock, evt.Clock)
	e := evt
	p.lastEvent = &e
	return rel
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/vector-clock-causal-ordering"
)

func main() {
	p := vclock.NewProcessor()

	events := []vclock.Event{
		{Node: "A", Clock: vclock.VectorClock{"A": 1}},
		{Node: "B", Clock: vclock.VectorClock{"A": 1, "B": 1}},
		{Node: "A", Clock: vclock.VectorClock{"A": 2}},
		{Node: "A", Clock: vclock.VectorClock{"A": 2}}, // duplicate delivery of the same event
	}

	for i, evt := range events {
		result := p.Ingest(evt)
		switch r := result.(type) {
		case vclock.Root:
			fmt.Printf("event %d (node %s): root of history\n", i, evt.Node)
		case vclock.Before:
			fmt.Printf("event %d (node %s): causally after the previous event\n", i, evt.Node)
		case vclock.After:
			fmt.Printf("event %d (node %s): causally before the previous event\n", i, evt.Node)
		case vclock.Concurrent:
			fmt.Printf("event %d (node %s): concurrent with the previous event\n", i, evt.Node)
		case vclock.Anomaly:
			fmt.Printf("event %d (node %s): anomaly, observed self-counter %d, expected %d\n", i, evt.Node, r.Observed, r.Expected)
		}
	}
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
event 0 (node A): root of history
event 1 (node B): causally after the previous event
event 2 (node A): concurrent with the previous event
event 3 (node A): anomaly, observed self-counter 2, expected 3
```

Event 1's clock `{A:1, B:1}` strictly dominates event 0's clock `{A:1}` (B's
component is greater, A's is tied), so it is causally after it — printed
here as "the previous event happened before this one." Event 2's clock
`{A:2}` never observed B's update, so its clock is incomparable with event
1's `{A:1, B:1}`: node A has a greater component (2 > 1) but node B has a
lesser one (0 < 1), which is exactly the signature of concurrency. Event 3
repeats node A's self-counter of 2, which already appeared in event 2, so
it is flagged as an anomaly rather than compared at all.

### Tests

Create `vclock_test.go`:

```go
package vclock

import (
	"fmt"
	"sync"
	"testing"
)

func TestCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b VectorClock
		want any
	}{
		{
			name: "a strictly precedes b",
			a:    VectorClock{"n1": 1},
			b:    VectorClock{"n1": 2},
			want: Before{},
		},
		{
			name: "b strictly precedes a",
			a:    VectorClock{"n1": 3, "n2": 1},
			b:    VectorClock{"n1": 2, "n2": 1},
			want: After{},
		},
		{
			name: "neither dominates: concurrent",
			a:    VectorClock{"n1": 2, "n2": 0},
			b:    VectorClock{"n1": 1, "n2": 1},
			want: Concurrent{},
		},
		{
			name: "identical clocks are concurrent",
			a:    VectorClock{"n1": 1, "n2": 1},
			b:    VectorClock{"n1": 1, "n2": 1},
			want: Concurrent{},
		},
		{
			name: "disjoint node sets treat the missing component as zero",
			a:    VectorClock{"n1": 1},
			b:    VectorClock{"n1": 1, "n2": 1},
			want: Before{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Compare(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("Compare(%v, %v) = %#v, want %#v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestProcessorIngest(t *testing.T) {
	t.Parallel()
	p := NewProcessor()

	if _, ok := p.Ingest(Event{Node: "A", Clock: VectorClock{"A": 1}}).(Root); !ok {
		t.Fatal("first event should be Root")
	}
	if _, ok := p.Ingest(Event{Node: "B", Clock: VectorClock{"A": 1, "B": 1}}).(Before); !ok {
		t.Fatal("second event should be Before relative to the first")
	}
	if _, ok := p.Ingest(Event{Node: "A", Clock: VectorClock{"A": 2}}).(Concurrent); !ok {
		t.Fatal("third event, which never observed B's update, should be Concurrent")
	}

	result := p.Ingest(Event{Node: "A", Clock: VectorClock{"A": 2}})
	anomaly, ok := result.(Anomaly)
	if !ok {
		t.Fatalf("duplicate delivery should be Anomaly, got %#v", result)
	}
	if anomaly.Observed != 2 || anomaly.Expected != 3 {
		t.Fatalf("Anomaly = %+v, want Observed=2 Expected=3", anomaly)
	}
}

// TestProcessorConcurrentIngestFromDistinctNodes fires many goroutines,
// each posting its own node's strictly increasing self-counter sequence
// concurrently. No goroutine should ever see a false Anomaly, and the race
// detector must find no data race on the shared lastSelf map or history
// pointer — the property the mutex inside Ingest exists to guarantee.
func TestProcessorConcurrentIngestFromDistinctNodes(t *testing.T) {
	p := NewProcessor()
	const nodes = 8
	const eventsPerNode = 20

	var wg sync.WaitGroup
	anomalies := make([][]Anomaly, nodes)
	for n := 0; n < nodes; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			node := fmt.Sprintf("node-%d", n)
			for i := uint64(1); i <= eventsPerNode; i++ {
				result := p.Ingest(Event{Node: node, Clock: VectorClock{node: i}})
				if a, ok := result.(Anomaly); ok {
					anomalies[n] = append(anomalies[n], a)
				}
			}
		}(n)
	}
	wg.Wait()

	for n, as := range anomalies {
		if len(as) != 0 {
			t.Fatalf("node %d reported false anomalies: %+v", n, as)
		}
	}
}
```

Verify: `go test -race -count=1 ./...`

## Review

`Compare` is correct because it is built entirely from "does any component
exceed the other's" rather than accumulating a running difference, which
is what makes identical clocks and incomparable clocks both fall cleanly
into `Concurrent` without special-casing either: neither condition for
`Before` or `After` is satisfied in either case, so `default` catches both
correctly. `Processor.Ingest` is correct because the self-counter anomaly
check runs strictly before `Compare` is ever called, which matters because
a corrupted or replayed clock is not merely "different" from what
`Compare` expects — it actively violates the assumption that a node's own
component only ever increases, and comparing against it would produce a
relation that looks plausible but means nothing. The concurrency test is
the part doing the real verification here: a version of `Ingest` that
checked `lastSelf` and then updated `lastEvent` in two separate lock
acquisitions, instead of one, would pass every single-threaded test in this
file while still losing updates or reporting phantom anomalies under real
concurrent ingestion from multiple nodes — exactly the bug `-race` exists
to catch and a sequential test cannot.

## Resources

- [Fidge, C. (1988). Timestamps in Message-Passing Systems That Preserve the Partial Ordering](https://zoo.cs.yale.edu/classes/cs426/2012/lab/bib/fidge88timestamps.pdf)
- [Mattern, F. (1989). Virtual Time and Global States of Distributed Systems](https://www.vs.inf.ethz.ch/publ/papers/VirtTime.pdf)
- [Go Specification: Type switches](https://go.dev/ref/spec#Type_switches)
- [Go: Data Race Detector](https://go.dev/doc/articles/race_detector)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [30-graceful-config-reload-dispatcher.md](30-graceful-config-reload-dispatcher.md) | Next: [32-request-coalescing-singleflight.md](32-request-coalescing-singleflight.md)
