# Exercise 27: Vector Clock Updates in Distributed Ledger

**Nivel: Intermedio** — validacion rapida (un test corto).

A distributed ledger replicated across several nodes cannot use wall-clock
timestamps to decide which write happened "first," because clocks on
different machines drift and NTP skew alone can reorder events that
actually happened in the opposite sequence. A vector clock sidesteps the
problem entirely: each node tracks a counter per actor, and integrating a
peer's clock is a loop over that peer's entries taking the element-wise
maximum. This module builds the merge, the happens-before comparison, and a
small ledger that threads vector clocks through a chain of writes exactly
the way gossiped events do in production.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
vclock/                        module example.com/vclock
  go.mod                       go 1.24
  vclock.go                    Clock; Merge; HappensBefore; Concurrent; Ledger; (*Ledger).Record
  vclock_test.go                 record table, deterministic counters, gossiped causality chain, concurrent events, equal clocks, merge
  cmd/demo/
    main.go                      two replicas writing, gossiping, and writing again
```

- Files: `vclock.go`, `vclock_test.go`, `cmd/demo/main.go`.
- Implement: `Merge(a, b Clock) Clock` — two `for range` passes folding the element-wise maximum of every actor's counter; `HappensBefore(a, b Clock) bool`; `Concurrent(a, b Clock) bool`; `(*Ledger).Record(actor string, received ...Clock) Clock` — merge in every received clock, then increment `actor`'s own counter.
- Test: `Record` with no peers, one peer, and several peers; counters advance deterministically across repeated calls; a two-replica gossip chain proves transitivity of happens-before; two independently-produced clocks are concurrent; identical clocks are neither ordered nor concurrent; `Merge` takes the element-wise maximum over the union of actors.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why merge is a loop over entries, not a single comparison

A vector clock is only useful because it turns "did this event already know
about that one" into pure arithmetic instead of a coordination protocol.
`Merge` is the operation that makes that arithmetic honest: when a node
receives a peer's clock, it cannot just replace its own with the peer's (it
would forget its own history) or keep its own unchanged (it would ignore
what the peer just taught it). The correct move is element-wise maximum
across the *union* of every actor either clock has ever seen, and that is
inherently a loop over map entries — there is no closed-form single
comparison that does it. The two-pass shape (copy `a` in, then take the max
against each entry of `b`) is deliberate: it means an actor present only in
`a` or only in `b` still ends up in the merged result with its original
counter, which is exactly what "union" requires.

`HappensBefore` earns its second loop for the same reason `Merge` needs
two passes: after confirming every actor common to both clocks (and every
actor in `a`) satisfies `a[actor] <= b[actor]`, the comparison is not
finished until it also accounts for actors that exist in `b` but never
appeared in `a` at all — a brand-new actor with any positive counter in `b`
is by itself a fact `a` could not have known, which forces `strictlyLess`
to `true` even if every shared actor happened to tie. Skipping that second
loop is the subtle bug: a clock `{a: 2}` would wrongly compare equal-to
`{a: 2, c: 1}` instead of causally preceding it, because nothing in `a`'s
own entries reveals that `b` knows about actor `c` at all.

Create `vclock.go`:

```go
package vclock

import "maps"

// Clock maps each actor (a node, a replica, a client session) to the
// highest event counter that actor has produced. It is the standard vector
// clock used to detect causality in a distributed ledger without relying on
// wall-clock timestamps, which cannot be trusted to agree across nodes.
type Clock map[string]uint64

// Copy returns an independent copy so callers can hand out a Clock without
// letting the receiver mutate the sender's history.
func (c Clock) Copy() Clock {
	cp := make(Clock, len(c))
	for actor, v := range c {
		cp[actor] = v
	}
	return cp
}

// Increment returns a copy of c with actor's own counter advanced by one,
// representing actor producing a new local event.
func (c Clock) Increment(actor string) Clock {
	next := c.Copy()
	next[actor] = next[actor] + 1
	return next
}

// Merge folds two clocks together by taking the element-wise maximum of
// every actor's counter across both -- the operation a node performs when it
// receives a peer's clock and needs to integrate the peer's causal history
// into its own. The loop runs once over each input clock's entries, so its
// cost is proportional to the number of distinct actors, not to the number
// of events any of them produced.
func Merge(a, b Clock) Clock {
	merged := make(Clock, len(a)+len(b))
	for actor, v := range a {
		merged[actor] = v
	}
	for actor, v := range b {
		if v > merged[actor] {
			merged[actor] = v
		}
	}
	return merged
}

// HappensBefore reports whether a causally precedes b: every actor's counter
// in a is less than or equal to the corresponding counter in b, and at least
// one counter is strictly less. Two clocks where neither happens-before the
// other are concurrent -- they represent events neither node could have
// known about when it produced its own.
func HappensBefore(a, b Clock) bool {
	strictlyLess := false
	for actor, av := range a {
		if av > b[actor] {
			return false
		}
		if av < b[actor] {
			strictlyLess = true
		}
	}
	for actor, bv := range b {
		if _, ok := a[actor]; !ok && bv > 0 {
			strictlyLess = true
		}
	}
	return strictlyLess
}

// Concurrent reports whether a and b represent a genuine conflict: neither
// happens-before the other, and they are not simply the same clock (an
// identical pair is a duplicate observation, not a conflict that needs
// application-level resolution).
func Concurrent(a, b Clock) bool {
	if maps.Equal(a, b) {
		return false
	}
	return !HappensBefore(a, b) && !HappensBefore(b, a)
}

// Ledger tracks the latest known vector clock produced by each actor and
// integrates incoming events from peers.
type Ledger struct {
	clocks map[string]Clock
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{clocks: make(map[string]Clock)}
}

// Record integrates a new local event for actor: it merges in every remote
// clock the actor received alongside the event (for example, clocks attached
// to gossip messages from peers), then advances actor's own counter. The
// resulting clock is what the caller attaches to the event before
// broadcasting it onward, so causality keeps propagating through the system.
func (l *Ledger) Record(actor string, received ...Clock) Clock {
	merged := l.clocks[actor]
	for _, r := range received {
		merged = Merge(merged, r)
	}
	merged = merged.Increment(actor)
	l.clocks[actor] = merged
	return merged
}

// Clock returns actor's current clock, or nil if actor has never recorded an
// event.
func (l *Ledger) Clock(actor string) Clock {
	return l.clocks[actor]
}
```

### The runnable demo

Two replicas each write locally, then `replica-b` receives `replica-a`'s
clock and writes again, and finally `replica-a` receives `replica-b`'s
clock and writes again -- the classic gossip-and-write cycle. The demo
prints the resulting clocks (map values print with sorted keys, so the
output is stable) and the three causality checks that matter.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/vclock"
)

func main() {
	ledger := vclock.NewLedger()

	a1 := ledger.Record("replica-a")
	fmt.Printf("replica-a writes locally:        %v\n", a1)

	b1 := ledger.Record("replica-b")
	fmt.Printf("replica-b writes locally:        %v\n", b1)

	b2 := ledger.Record("replica-b", a1)
	fmt.Printf("replica-b receives replica-a's write, then writes again: %v\n", b2)

	a2 := ledger.Record("replica-a", b2)
	fmt.Printf("replica-a receives replica-b's write, then writes again: %v\n", a2)

	fmt.Printf("\nHappensBefore(a1, b2) = %v (b2 saw a1's write)\n", vclock.HappensBefore(a1, b2))
	fmt.Printf("Concurrent(a1, b1)    = %v (neither saw the other's first write)\n", vclock.Concurrent(a1, b1))
	fmt.Printf("HappensBefore(b2, a2) = %v (a2 saw b2's write)\n", vclock.HappensBefore(b2, a2))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
replica-a writes locally:        map[replica-a:1]
replica-b writes locally:        map[replica-b:1]
replica-b receives replica-a's write, then writes again: map[replica-a:1 replica-b:2]
replica-a receives replica-b's write, then writes again: map[replica-a:2 replica-b:2]

HappensBefore(a1, b2) = true (b2 saw a1's write)
Concurrent(a1, b1)    = true (neither saw the other's first write)
HappensBefore(b2, a2) = true (a2 saw b2's write)
```

### Tests

`TestLedgerRecordAdvancesOwnCounterOnly` is a table over zero, one, and
several received clocks. `TestLedgerRecordIsDeterministicAcrossCalls`
confirms repeated calls advance the counter by exactly one each time, with
no reliance on timing. `TestCausalityAcrossGossipedClocks` builds the same
two-replica gossip chain as the demo and checks transitivity end to end;
`TestConcurrentEventsNeitherHappensBefore` and
`TestHappensBeforeIsFalseForIdenticalClocks` cover the two edge cases that
a naive happens-before check gets wrong; `TestMergeTakesElementWiseMaximum`
checks the union behavior directly.

Create `vclock_test.go`:

```go
package vclock

import (
	"maps"
	"testing"
)

func TestLedgerRecordAdvancesOwnCounterOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		received []Clock
		want     Clock
	}{
		{
			name:     "first event with no peers",
			received: nil,
			want:     Clock{"a": 1},
		},
		{
			name:     "merges a single peer clock then increments",
			received: []Clock{{"a": 0, "b": 3}},
			want:     Clock{"a": 1, "b": 3},
		},
		{
			name:     "merges multiple peer clocks then increments",
			received: []Clock{{"b": 2}, {"c": 5}, {"b": 1}},
			want:     Clock{"a": 1, "b": 2, "c": 5},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := NewLedger()
			got := l.Record("a", tc.received...)
			if !maps.Equal(got, tc.want) {
				t.Fatalf("Record() = %v, want %v", got, tc.want)
			}
			if !maps.Equal(l.Clock("a"), tc.want) {
				t.Fatalf("Clock(a) = %v, want %v", l.Clock("a"), tc.want)
			}
		})
	}
}

func TestLedgerRecordIsDeterministicAcrossCalls(t *testing.T) {
	t.Parallel()

	l := NewLedger()
	first := l.Record("a")
	second := l.Record("a")
	third := l.Record("a")

	if first["a"] != 1 || second["a"] != 2 || third["a"] != 3 {
		t.Fatalf("counters = %d, %d, %d; want 1, 2, 3", first["a"], second["a"], third["a"])
	}
}

func TestCausalityAcrossGossipedClocks(t *testing.T) {
	t.Parallel()

	ledger := NewLedger()
	eventA1 := ledger.Record("a")          // a:1
	ledger.Record("b", eventA1)            // b's first local event, with a:1 merged in: {a:1, b:1}
	eventB2 := ledger.Record("b", eventA1) // b integrates a's clock again: {a:1, b:2}
	eventA2 := ledger.Record("a", eventB2) // a integrates b's clock: {a:2, b:2}

	if !HappensBefore(eventA1, eventB2) {
		t.Fatalf("HappensBefore(eventA1, eventB2) = false, want true (b2 saw a1)")
	}
	if !HappensBefore(eventB2, eventA2) {
		t.Fatalf("HappensBefore(eventB2, eventA2) = false, want true (a2 saw b2)")
	}
	if !HappensBefore(eventA1, eventA2) {
		t.Fatalf("HappensBefore(eventA1, eventA2) = false, want true (transitivity)")
	}
	if HappensBefore(eventA2, eventA1) {
		t.Fatal("HappensBefore(eventA2, eventA1) = true, want false")
	}
}

func TestConcurrentEventsNeitherHappensBefore(t *testing.T) {
	t.Parallel()

	a := Clock{"a": 1}
	b := Clock{"b": 1}

	if !Concurrent(a, b) {
		t.Fatal("Concurrent(a, b) = false, want true")
	}
	if HappensBefore(a, b) || HappensBefore(b, a) {
		t.Fatal("independently-produced clocks must not happen-before each other")
	}
}

func TestHappensBeforeIsFalseForIdenticalClocks(t *testing.T) {
	t.Parallel()

	a := Clock{"a": 2, "b": 3}
	b := Clock{"a": 2, "b": 3}

	if HappensBefore(a, b) {
		t.Fatal("HappensBefore(a, b) = true, want false for identical clocks")
	}
	if Concurrent(a, b) {
		t.Fatal("Concurrent(a, b) = true, want false for identical clocks (they are equal, not concurrent)")
	}
}

func TestMergeTakesElementWiseMaximum(t *testing.T) {
	t.Parallel()

	a := Clock{"a": 5, "b": 1}
	b := Clock{"a": 2, "b": 7, "c": 3}

	got := Merge(a, b)
	want := Clock{"a": 5, "b": 7, "c": 3}
	if !maps.Equal(got, want) {
		t.Fatalf("Merge() = %v, want %v", got, want)
	}
}
```

## Review

`Merge` and `HappensBefore` are correct when they treat the union of
actors across both clocks as the domain of comparison, not just the actors
one side happens to know about -- and both functions need two passes for
exactly that reason. The common mistake this design avoids is writing
`HappensBefore` with a single loop over `a`'s entries and calling it done:
that version passes every test where `b` is a strict superset of `a`'s
actors with larger counters, but silently mis-ranks a clock that gained a
brand-new actor no differently from one that gained nothing, because a new
actor with a positive counter never appears while ranging only over `a`.
Run `go test -count=1 ./...`.

## Resources

- [Time, Clocks, and the Ordering of Events in a Distributed System (Lamport, 1978)](https://lamport.azurewebsites.net/pubs/time-clocks.pdf) — the causality model vector clocks generalize.
- [Vector clock (Wikipedia)](https://en.wikipedia.org/wiki/Vector_clock) — the merge and happens-before rules this module implements.
- [maps package](https://pkg.go.dev/maps) — `maps.Equal`, used to detect the identical-clock edge case.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-consistent-hash-ring-shard-lookup.md](26-consistent-hash-ring-shard-lookup.md) | Next: [28-bloom-filter-cardinality-sketch.md](28-bloom-filter-cardinality-sketch.md)
