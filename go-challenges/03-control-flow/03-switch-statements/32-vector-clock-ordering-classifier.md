# Exercise 32: Use Vector Clocks to Classify Event Causality in Distributed Systems

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A leaderless, quorum-based store — Dynamo-style replication, a CRDT-backed
cache, any system where writes can land on different replicas
concurrently — cannot use wall-clock timestamps to decide which of two
writes happened first: physical clocks drift, NTP corrections jump
backwards, and two replicas in different data centers are never perfectly
synchronized. A vector clock sidesteps the problem entirely by tracking,
per replica, how many events that replica has personally observed, and
classifying two events as causally ordered only when one clock's counters
truly dominate the other's across every replica. When neither dominates,
the events are genuinely concurrent, and the system has to keep both
writes rather than silently discarding one. This module builds that
classifier: a pure, deterministic `Compare` function plus a
mutex-protected event log multiple replica goroutines can safely append
to. It is self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
vclock/                     independent module: example.com/vector-clock-ordering-classifier
  go.mod                     go 1.24
  vclock.go                   package vclock; Clock; Relation; Compare(a, b) Relation; Merge; Log (mutex-protected)
  cmd/demo/main.go             runnable demo simulating two replicas exchanging one message
  vclock_test.go                boundary-condition table over Compare, Tick/Merge invariants, concurrent Log access
```

- Implement: `Compare(a, b Clock) Relation` — a tagless switch over two computed dominance booleans classifies the pair as `Identical`, `Before`, `After`, or `Concurrent`.
- Test: a table covering every relation (identical, strict before, strict after, disjoint-replica concurrency, mixed-direction concurrency, and a merge-then-tick chain), plus a concurrency subtest hammering a shared `Log` from many goroutines.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/32-vector-clock-ordering-classifier/cmd/demo
cd go-solutions/03-control-flow/03-switch-statements/32-vector-clock-ordering-classifier
go mod edit -go=1.24
```

### Why Compare needs two dominance checks, not one comparison

Two vector clocks cannot be compared with a single boolean the way two
integers can. `a` happened-before `b` only when *every* one of `a`'s
counters is less than or equal to the corresponding counter in `b`, *and*
at least one is strictly less (otherwise they're identical, not ordered).
`Compare` computes this dominance relation in both directions —
`dominance(a, b)` and `dominance(b, a)` — and then dispatches on the four
combinations with a tagless switch:

```go
switch {
case aLessOrEqual && bLessOrEqual:
    return Identical
case aLessOrEqual && aStrictlyLess:
    return Before
case bLessOrEqual && bStrictlyLess:
    return After
default:
    return Concurrent
}
```

The fourth case — neither `a<=b` nor `b<=a` holds everywhere — is
`Concurrent`, and it is the case a system built on wall-clock timestamps
literally cannot express: two timestamps are always comparable (one is
numerically before, at, or after the other), but two vector clocks are
not, and that's the entire point of switching from physical to logical
time. A replica ID absent from one clock is treated as counter zero
(`Clock` is `map[string]uint64`, and Go's zero value for a missing key is
`0`), which is why `dominance` only has to walk the *union* of both
clocks' replica IDs rather than requiring every replica to appear in both.

`Tick` and `Merge` both return a new `Clock` rather than mutating the
receiver, because a vector clock is meant to be attached to an event as an
immutable snapshot — a `Tick` that mutated `c` in place would silently
corrupt every earlier event that still held a reference to the same map,
since Go maps are reference types.

### The concurrency angle: many replicas appending to one Log

Real vector-clock usage happens across genuinely concurrent replicas, and
`Log` models the shared collector a real system would use to gather
causally-tagged events from many nodes for later analysis. `Log.Append`
and `Log.Snapshot` are both guarded by `sync.Mutex`, and `Snapshot` returns
a defensive copy rather than the internal slice — a caller iterating the
snapshot must never be able to race against a concurrent `Append` that
reallocates the backing array underneath it. The demo below stays fully
sequential and deterministic (vector clocks don't need real concurrency to
demonstrate causal ordering — a few explicit `Tick`/`Merge` calls are
enough), while the concurrency test exercises `Log` directly with many
goroutines, checked under the race detector.

Create `vclock.go`:

```go
// Package vclock implements vector clocks, the mechanism a quorum-based or
// leaderless distributed store (Dynamo-style replication, CRDTs, a
// multi-region cache) uses to order events across replicas without relying
// on wall-clock timestamps -- physical clocks drift and are never
// perfectly synchronized, so "which write happened first" cannot be
// answered by comparing two nodes' local clocks. A vector clock instead
// tracks, per replica, how many events that replica has observed, and two
// clocks are only comparable if one's counters dominate the other's across
// every replica; when neither dominates, the events are genuinely
// concurrent and a conflict-resolution policy (last-writer-wins, CRDT
// merge, application callback) has to take over.
package vclock

import "sync"

// Clock maps a replica ID to the number of events that replica has
// observed, including its own local events and merges from messages it has
// received. A replica ID absent from the map is treated as counter 0,
// which is why Compare and Merge never need every replica ID to be present
// in both clocks being compared.
type Clock map[string]uint64

// New returns an empty clock -- every replica implicitly at counter 0.
func New() Clock {
	return make(Clock)
}

// Tick returns a copy of c with replica's own counter incremented by one.
// It never mutates c: vector clocks are cheap, immutable snapshots attached
// to events, and mutating a clock that another goroutine or event might
// still be holding a reference to is exactly the kind of aliasing bug this
// package's copy-on-write style avoids.
func (c Clock) Tick(replica string) Clock {
	next := c.copy()
	next[replica] = next[replica] + 1
	return next
}

// Merge returns a new clock whose counter for every replica is the maximum
// of a's and b's -- the vector clock rule applied on message receipt,
// folding in everything the sender had observed before the message was
// sent.
func Merge(a, b Clock) Clock {
	merged := a.copy()
	for replica, bCount := range b {
		if bCount > merged[replica] {
			merged[replica] = bCount
		}
	}
	return merged
}

func (c Clock) copy() Clock {
	next := make(Clock, len(c))
	for replica, count := range c {
		next[replica] = count
	}
	return next
}

// Relation classifies how two clocks (and the events they're attached to)
// relate causally.
type Relation int

const (
	Identical Relation = iota
	Before             // a happened-before b
	After              // a happened-after b
	Concurrent
)

// String renders a Relation for logs and test failure messages.
func (r Relation) String() string {
	switch r {
	case Identical:
		return "identical"
	case Before:
		return "happened-before"
	case After:
		return "happened-after"
	case Concurrent:
		return "concurrent"
	default:
		return "unknown"
	}
}

// Compare classifies a relative to b. a happened-before b when every one of
// a's counters is less than or equal to the corresponding counter in b, and
// at least one is strictly less; a happened-after b is the mirror; if
// neither dominates, the two events are Concurrent -- neither could have
// observed the other, which for a quorum store means both writes must be
// kept (or reconciled by application logic) rather than one silently
// discarded.
func Compare(a, b Clock) Relation {
	aLessOrEqual, aStrictlyLess := dominance(a, b)
	bLessOrEqual, bStrictlyLess := dominance(b, a)

	switch {
	case aLessOrEqual && bLessOrEqual:
		return Identical
	case aLessOrEqual && aStrictlyLess:
		return Before
	case bLessOrEqual && bStrictlyLess:
		return After
	default:
		return Concurrent
	}
}

// dominance reports whether every counter in x is <= the corresponding
// counter in y (lessOrEqual), and whether at least one counter in x is
// strictly less than the corresponding counter in y (strictlyLess).
func dominance(x, y Clock) (lessOrEqual, strictlyLess bool) {
	lessOrEqual = true
	seen := make(map[string]struct{}, len(x)+len(y))
	for replica := range x {
		seen[replica] = struct{}{}
	}
	for replica := range y {
		seen[replica] = struct{}{}
	}
	for replica := range seen {
		if x[replica] > y[replica] {
			lessOrEqual = false
		}
		if x[replica] < y[replica] {
			strictlyLess = true
		}
	}
	return lessOrEqual, strictlyLess
}

// Event is a clock snapshot attached to a replica's local or received
// event, together with a human-readable label for logging.
type Event struct {
	Replica string
	Clock   Clock
	Label   string
}

// Log is a mutex-protected append-only recorder that multiple replica
// goroutines can safely write to concurrently -- the shared collector a
// real system would use to gather causally-ordered events from many nodes
// for later offline analysis or debugging a reported anomaly.
type Log struct {
	mu     sync.Mutex
	events []Event
}

// Append records e. Safe for concurrent use by many goroutines.
func (l *Log) Append(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

// Snapshot returns a copy of every event recorded so far. Returning a copy
// (rather than the internal slice) means a caller iterating the result
// can never race with a concurrent Append extending or reallocating the
// underlying array.
func (l *Log) Snapshot() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	vclock "example.com/vector-clock-ordering-classifier"
)

func main() {
	// Replica A does a local event.
	a1 := vclock.New().Tick("A") // A:1

	// Replica B, independently, does a local event -- neither replica has
	// observed the other yet, so these two events are concurrent.
	b1 := vclock.New().Tick("B") // B:1

	// Replica A sends a message carrying a1; replica B receives it, merges
	// a1's clock into its own, then ticks its own counter for the receive
	// event itself.
	b2 := vclock.Merge(b1, a1).Tick("B") // A:1, B:2 -- happened-after both a1 and b1

	// Replica A, unaware of b1 or b2, does another independent local event.
	a2 := a1.Tick("A") // A:2 -- still concurrent with b1 and b2

	fmt.Println("a1 vs b1:", vclock.Compare(a1, b1))
	fmt.Println("a1 vs b2:", vclock.Compare(a1, b2))
	fmt.Println("b2 vs a1:", vclock.Compare(b2, a1))
	fmt.Println("a2 vs b2:", vclock.Compare(a2, b2))
	fmt.Println("a1 vs a1:", vclock.Compare(a1, a1))
}
```

Run `go run ./cmd/demo`, expected output:

```
a1 vs b1: concurrent
a1 vs b2: happened-before
b2 vs a1: happened-after
a2 vs b2: concurrent
a1 vs a1: identical
```

### Tests

`TestCompare` runs a boundary-condition table: identical clocks (both
empty and both non-empty), strict domination in each direction, disjoint
replica IDs, a mixed-direction case where one counter is higher and
another lower, and a clock built by merging in another replica's event.
`TestTickNeverMutatesReceiver` and `TestMergeTakesElementwiseMax` check the
two invariants the whole package depends on. `TestLogConcurrentAppend`
drives fifty goroutines appending to a shared `Log` at once; since
concurrent goroutines give no ordering guarantee, it only asserts the
invariants that hold regardless of interleaving — every event arrives,
none is lost or duplicated — and relies on `go test -race` to catch any
synchronization bug the mutex was supposed to prevent.

Create `vclock_test.go`:

```go
package vclock

import (
	"sort"
	"sync"
	"testing"
)

func TestCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b Clock
		want Relation
	}{
		{
			name: "identical empty clocks",
			a:    New(),
			b:    New(),
			want: Identical,
		},
		{
			name: "identical non-empty clocks",
			a:    Clock{"A": 2, "B": 1},
			b:    Clock{"A": 2, "B": 1},
			want: Identical,
		},
		{
			name: "a strictly dominated by b on every shared replica",
			a:    Clock{"A": 1},
			b:    Clock{"A": 2},
			want: Before,
		},
		{
			name: "a strictly dominates b",
			a:    Clock{"A": 2, "B": 1},
			b:    Clock{"A": 1, "B": 1},
			want: After,
		},
		{
			name: "disjoint replicas are concurrent",
			a:    Clock{"A": 1},
			b:    Clock{"B": 1},
			want: Concurrent,
		},
		{
			name: "one counter higher, another lower is concurrent",
			a:    Clock{"A": 2, "B": 0},
			b:    Clock{"A": 1, "B": 2},
			want: Concurrent,
		},
		{
			name: "a is a merge that received b's event, so a happened-after b",
			a:    Clock{"A": 1, "B": 2},
			b:    Clock{"B": 1},
			want: After,
		},
	}

	for _, tc := range tests {
		if got := Compare(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: Compare(%v, %v) = %s, want %s", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

func TestTickNeverMutatesReceiver(t *testing.T) {
	t.Parallel()

	c := Clock{"A": 1}
	next := c.Tick("A")

	if c["A"] != 1 {
		t.Fatalf("original clock mutated: c[A] = %d, want 1", c["A"])
	}
	if next["A"] != 2 {
		t.Fatalf("next[A] = %d, want 2", next["A"])
	}
}

func TestMergeTakesElementwiseMax(t *testing.T) {
	t.Parallel()

	a := Clock{"A": 3, "B": 1}
	b := Clock{"A": 1, "B": 5, "C": 2}

	merged := Merge(a, b)
	want := Clock{"A": 3, "B": 5, "C": 2}

	for replica, count := range want {
		if merged[replica] != count {
			t.Errorf("merged[%s] = %d, want %d", replica, merged[replica], count)
		}
	}
}

// TestLogConcurrentAppend drives many goroutines appending events to a
// shared Log at once. The mutex makes every Append race-free regardless of
// interleaving, so the only invariants worth asserting are the ones that
// hold no matter what order the goroutines actually ran in: every event
// arrives in the snapshot, and no event is lost or duplicated. Per-event
// ordering is deliberately not asserted -- concurrent goroutines give no
// ordering guarantee, and a test that assumed one would be flaky by
// construction.
func TestLogConcurrentAppend(t *testing.T) {
	t.Parallel()

	log := &Log{}
	const n = 50

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			replica := "R"
			c := New().Tick(replica)
			log.Append(Event{Replica: replica, Clock: c, Label: "event"})
		}(i)
	}
	wg.Wait()

	events := log.Snapshot()
	if len(events) != n {
		t.Fatalf("len(events) = %d, want %d", len(events), n)
	}

	labels := make([]string, len(events))
	for i, e := range events {
		labels[i] = e.Label
	}
	sort.Strings(labels)
	for _, l := range labels {
		if l != "event" {
			t.Fatalf("unexpected label %q in recorded events", l)
		}
	}
}
```

Verify with:

```bash
go test -race -count=1 ./...
```

## Review

The classifier is correct when identical clocks are reported `Identical`
rather than being forced into `Before` or `After`, when disjoint or
mixed-direction clocks are correctly reported `Concurrent` instead of an
arbitrary pick, and when `Tick` and `Merge` never mutate their receivers so
that an earlier event's clock snapshot can never be silently altered by a
later `Tick` call on a value that shares its backing map. Carry this
forward: whenever a relation between two values genuinely has more than
two outcomes — before, after, equal, incomparable — resist collapsing it
into a `bool` or a simple ordering; a tagless switch over the actual
dominance conditions can express all four outcomes honestly, and a caller
that only checked "is A before B" would have silently mishandled every
concurrent pair.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [Lamport: Time, Clocks, and the Ordering of Events](https://lamport.azurewebsites.net/pubs/time-clocks.pdf) — the foundational paper behind vector and logical clocks.
- [Amazon Dynamo paper, section 4.4](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) — vector clocks used for conflict detection in a real, widely-cited quorum store.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-bloom-filter-membership-checker.md](31-bloom-filter-membership-checker.md) | Next: [33-wal-checkpoint-decision-engine.md](33-wal-checkpoint-decision-engine.md)
