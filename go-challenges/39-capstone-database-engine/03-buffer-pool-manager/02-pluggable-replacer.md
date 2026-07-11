# Exercise 2: A Pluggable Replacer — CLOCK, LRU, and LRU-K

The buffer pool's eviction policy is really a strategy: which unpinned frame to reuse on a miss is a decision that should be swappable without touching the pool's correctness invariants. This exercise factors that decision behind one `Replacer` interface and gives it three implementations — CLOCK, exact LRU, and LRU-K — then proves the property that motivates LRU-K: resistance to sequential flooding. On the same access trace, plain LRU evicts a hot working-set page while LRU-2 evicts a one-touch scan page, which is exactly the difference that keeps a large scan from blowing the hot set out of cache.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
replacer.go            FrameID, Replacer, ClockReplacer, LRUReplacer, LRUKReplacer
                       (RecordAccess, SetEvictable, Evict, Size on each)
cmd/
  demo/
    main.go            run LRU and LRU-2 on one flooding trace, show divergent victims
replacer_test.go       evicts only evictable frames; LRU-K resists flooding; LRU-1 == LRU
```

- Files: `replacer.go`, `cmd/demo/main.go`, `replacer_test.go`.
- Implement: the `Replacer` interface and the `ClockReplacer`, `LRUReplacer`, and `LRUKReplacer` types, each with `RecordAccess`, `SetEvictable`, `Evict`, and `Size`, plus their constructors.
- Test: `replacer_test.go` checks that each policy evicts only evictable frames, that LRU-K resists sequential flooding where LRU does not, and that LRU-1 degenerates to exact LRU.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p pluggable-replacer/cmd/demo && cd pluggable-replacer
go mod init example.com/pluggable-replacer
```

### One interface, three policies, and why the abstraction holds

The pool performs the same three operations on its frames no matter which policy is in charge: it records an access on every pin and cache hit, it marks a frame evictable when its last pin is released, and it asks for a victim on a miss. Those three operations are exactly the `Replacer` interface — `RecordAccess`, `SetEvictable`, and `Evict`, with `Size` reporting the current candidate count. Because the pool's correctness invariant (a pinned frame is never evictable) lives in when it calls `SetEvictable`, not in the policy, all three implementations are interchangeable: swapping CLOCK for LRU-K changes the heuristic and nothing about safety.

`ClockReplacer` is the standalone form of the sweep the core already runs inline. It keeps a reference bit and a presence flag per frame and a rotating hand. `RecordAccess` sets the bit; `SetEvictable` toggles presence and maintains a live count; `Evict` runs the two-sweep algorithm, clearing bits on the first pass and evicting the first clear, present frame on the second. A hit costs one bit-write — no list surgery — which is why it scales under concurrency, and also why it stays flooding-vulnerable: a scan sets the bit on every cold page it touches.

`LRUReplacer` is exact LRU, the accuracy baseline. It stamps each access with a monotonic clock and evicts the evictable frame whose last stamp is oldest, breaking ties toward the smaller frame id for determinism. It is also the policy sequential flooding defeats: a one-time scan becomes the most-recently-used set and pushes the working set to the bottom of the recency order, so the next eviction discards a hot page the query is about to reuse.

`LRUKReplacer` implements LRU-K (O'Neil, O'Neil, Weikum, 1993). It keeps the last K access stamps per frame and evicts by backward K-distance: the gap between now and the K-th most recent access. A frame with fewer than K recorded accesses has an infinite K-distance and is evicted before any frame with a full history; ties among the infinite-distance frames break toward the earliest recorded access, which is classic LRU among the under-referenced pages. Because a one-touch sequential scan never reaches K references, LRU-K evicts scan pages first and preserves a repeatedly accessed working set. With K = 1 the history holds a single stamp and backward 1-distance is just recency, so LRU-1 is exactly LRU — a useful sanity check the test asserts directly.

Create `replacer.go`:

```go
package replacer

import "math"

// FrameID identifies a slot in the buffer pool. -1 means no frame.
type FrameID int

// InvalidFrameID is the sentinel returned when no frame is available.
const InvalidFrameID FrameID = -1

// Replacer decides which unpinned frame to evict next. The buffer pool records
// an access on every pin and a cache hit, and marks a frame evictable when its
// pin count drops to zero; Evict returns the frame the policy chooses to reuse.
// The three implementations below are interchangeable, which is the whole point
// of factoring eviction behind an interface: the pool's correctness invariants
// (pinned frames are never evictable) are independent of the replacement
// heuristic.
type Replacer interface {
	// RecordAccess registers a reference to fid (a pin or a cache hit).
	RecordAccess(fid FrameID)
	// SetEvictable marks fid evictable or not. Only evictable frames are
	// candidates for Evict.
	SetEvictable(fid FrameID, evictable bool)
	// Evict returns the victim frame and true, or InvalidFrameID and false if
	// no frame is currently evictable.
	Evict() (FrameID, bool)
	// Size returns the number of currently evictable frames.
	Size() int
}

// ClockReplacer approximates LRU with a single reference bit per frame and a
// sweeping hand. It is the policy the buffer pool's clockSweep implements inline;
// this standalone version satisfies Replacer so policies are interchangeable.
type ClockReplacer struct {
	ref       []bool
	present   []bool // frame is known to the replacer and currently evictable
	hand      int
	evictable int
}

// NewClockReplacer returns a ClockReplacer sized for numFrames frames.
func NewClockReplacer(numFrames int) *ClockReplacer {
	return &ClockReplacer{
		ref:     make([]bool, numFrames),
		present: make([]bool, numFrames),
	}
}

func (c *ClockReplacer) RecordAccess(fid FrameID) {
	if int(fid) < 0 || int(fid) >= len(c.ref) {
		return
	}
	c.ref[fid] = true
}

func (c *ClockReplacer) SetEvictable(fid FrameID, evictable bool) {
	if int(fid) < 0 || int(fid) >= len(c.present) {
		return
	}
	switch {
	case evictable && !c.present[fid]:
		c.present[fid] = true
		c.evictable++
	case !evictable && c.present[fid]:
		c.present[fid] = false
		c.evictable--
	}
}

func (c *ClockReplacer) Evict() (FrameID, bool) {
	n := len(c.ref)
	if n == 0 || c.evictable == 0 {
		return InvalidFrameID, false
	}
	// Two full sweeps: the first clears reference bits, the second evicts.
	for swept := 0; swept < 2*n; swept++ {
		idx := c.hand % n
		c.hand = (c.hand + 1) % n
		if !c.present[idx] {
			continue
		}
		if c.ref[idx] {
			c.ref[idx] = false
			continue
		}
		c.present[idx] = false
		c.evictable--
		return FrameID(idx), true
	}
	return InvalidFrameID, false
}

func (c *ClockReplacer) Size() int { return c.evictable }

// LRUReplacer evicts the evictable frame whose most recent access is oldest.
// It is exact LRU, the baseline that sequential flooding defeats: a one-time
// scan becomes the most-recently-used set and pushes the hot working set to the
// bottom of the recency order, so the next eviction discards a hot page.
type LRUReplacer struct {
	clock     uint64
	lastUsed  map[FrameID]uint64
	evictable map[FrameID]bool
}

func NewLRUReplacer() *LRUReplacer {
	return &LRUReplacer{
		lastUsed:  make(map[FrameID]uint64),
		evictable: make(map[FrameID]bool),
	}
}

func (l *LRUReplacer) RecordAccess(fid FrameID) {
	l.clock++
	l.lastUsed[fid] = l.clock
}

func (l *LRUReplacer) SetEvictable(fid FrameID, evictable bool) {
	if evictable {
		l.evictable[fid] = true
	} else {
		delete(l.evictable, fid)
	}
}

func (l *LRUReplacer) Evict() (FrameID, bool) {
	victim := InvalidFrameID
	var oldest uint64
	for fid := range l.evictable {
		t := l.lastUsed[fid]
		if victim == InvalidFrameID || t < oldest || (t == oldest && fid < victim) {
			oldest = t
			victim = fid
		}
	}
	if victim == InvalidFrameID {
		return InvalidFrameID, false
	}
	delete(l.evictable, victim)
	return victim, true
}

func (l *LRUReplacer) Size() int { return len(l.evictable) }

// LRUKReplacer implements the LRU-K policy (O'Neil, O'Neil, Weikum, 1993). It
// evicts the frame with the largest backward k-distance: the gap between now
// and its k-th most recent access. A frame referenced fewer than k times has an
// infinite k-distance and is evicted before any frame with a full history; ties
// among such frames break toward the earliest recorded access (classic LRU).
// Because a one-touch sequential scan never reaches k references, LRU-K evicts
// scan pages first and preserves a repeatedly accessed working set.
type LRUKReplacer struct {
	k         int
	clock     uint64
	history   map[FrameID][]uint64 // most recent access last; capped at k entries
	evictable map[FrameID]bool
}

func NewLRUKReplacer(k int) *LRUKReplacer {
	if k < 1 {
		k = 1
	}
	return &LRUKReplacer{
		k:         k,
		history:   make(map[FrameID][]uint64),
		evictable: make(map[FrameID]bool),
	}
}

func (r *LRUKReplacer) RecordAccess(fid FrameID) {
	r.clock++
	h := append(r.history[fid], r.clock)
	if len(h) > r.k {
		h = h[len(h)-r.k:]
	}
	r.history[fid] = h
}

func (r *LRUKReplacer) SetEvictable(fid FrameID, evictable bool) {
	if evictable {
		r.evictable[fid] = true
	} else {
		delete(r.evictable, fid)
	}
}

func (r *LRUKReplacer) Evict() (FrameID, bool) {
	victim := InvalidFrameID
	var bestDist uint64
	var bestEarliest uint64
	for fid := range r.evictable {
		h := r.history[fid]
		var dist uint64
		if len(h) < r.k {
			dist = math.MaxUint64 // infinite backward k-distance
		} else {
			dist = r.clock - h[0] // h[0] is the k-th most recent access
		}
		var earliest uint64
		if len(h) > 0 {
			earliest = h[0]
		}
		if victim == InvalidFrameID || dist > bestDist ||
			(dist == bestDist && earliest < bestEarliest) {
			victim = fid
			bestDist = dist
			bestEarliest = earliest
		}
	}
	if victim == InvalidFrameID {
		return InvalidFrameID, false
	}
	delete(r.evictable, victim)
	return victim, true
}

func (r *LRUKReplacer) Size() int { return len(r.evictable) }
```

### The runnable demo

The demo makes the flooding result concrete on one trace. A hot working set of frames 0 and 1 is referenced twice each; a sequential scan then touches frames 2, 3, and 4 once each. With every frame evictable, LRU picks frame 0 — a hot page — because the scan made 2, 3, 4 the most-recently-used and buried the working set. LRU-2 picks frame 2 — a cold scan page — because each scan frame has only one reference and therefore an infinite backward-2 distance, so they are evicted ahead of any twice-referenced page. Same trace, opposite decision: that gap is the entire argument for LRU-K.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pluggable-replacer"
)

func main() {
	// Hot working set {0,1} is touched twice; a scan touches {2,3,4} once each.
	seq := []replacer.FrameID{0, 0, 1, 1, 2, 3, 4}

	lru := replacer.NewLRUReplacer()
	lruk := replacer.NewLRUKReplacer(2)
	for _, fid := range seq {
		lru.RecordAccess(fid)
		lruk.RecordAccess(fid)
	}
	for _, fid := range []replacer.FrameID{0, 1, 2, 3, 4} {
		lru.SetEvictable(fid, true)
		lruk.SetEvictable(fid, true)
	}

	lruVictim, _ := lru.Evict()
	lrukVictim, _ := lruk.Evict()
	fmt.Printf("LRU evicts frame %d (a hot page — sequential flooding)\n", lruVictim)
	fmt.Printf("LRU-2 evicts frame %d (a cold scan page — flooding resisted)\n", lrukVictim)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
LRU evicts frame 0 (a hot page — sequential flooding)
LRU-2 evicts frame 2 (a cold scan page — flooding resisted)
```

### Tests

`TestReplacerEvictsOnlyEvictable` runs all three policies through a table: with nothing marked evictable, `Evict` must report no victim; after one frame is made evictable, `Size` is one and `Evict` returns exactly that frame and drops back to zero. `TestLRUKResistsSequentialFlooding` is the centerpiece: it feeds the flooding trace to both LRU and LRU-2 and asserts LRU evicts a hot frame while LRU-2 evicts a scan frame — the property in a single assertion. `TestLRUKDegeneratesToLRU` runs several traces through both `NewLRUReplacer()` and `NewLRUKReplacer(1)` and asserts identical victims, since backward 1-distance is just recency.

Create `replacer_test.go`:

```go
package replacer

import (
	"fmt"
	"testing"
)

// recordSeq applies a sequence of accesses to a replacer and marks every
// touched frame evictable, simulating a workload of pins that all unpin.
func recordSeq(r Replacer, seq []FrameID) {
	seen := make(map[FrameID]bool)
	for _, fid := range seq {
		r.RecordAccess(fid)
		if !seen[fid] {
			seen[fid] = true
			r.SetEvictable(fid, true)
		}
	}
}

func TestReplacerEvictsOnlyEvictable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		make func() Replacer
	}{
		{"clock", func() Replacer { return NewClockReplacer(4) }},
		{"lru", func() Replacer { return NewLRUReplacer() }},
		{"lru-2", func() Replacer { return NewLRUKReplacer(2) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.make()
			r.RecordAccess(0)
			r.RecordAccess(1)
			// Nothing is evictable yet: Evict must report no victim.
			if fid, ok := r.Evict(); ok {
				t.Fatalf("Evict returned frame %d with no evictable frames", fid)
			}
			r.SetEvictable(1, true)
			if got := r.Size(); got != 1 {
				t.Fatalf("Size = %d, want 1", got)
			}
			fid, ok := r.Evict()
			if !ok {
				t.Fatal("Evict found no victim despite one evictable frame")
			}
			if fid != 1 {
				t.Fatalf("evicted frame %d, want 1 (the only evictable one)", fid)
			}
			if got := r.Size(); got != 0 {
				t.Fatalf("Size = %d after evicting the only candidate, want 0", got)
			}
		})
	}
}

func TestLRUKResistsSequentialFlooding(t *testing.T) {
	t.Parallel()

	// Hot working set {0,1} is referenced twice; a sequential scan touches
	// {2,3,4} once each. All frames are evictable when eviction is requested.
	seq := []FrameID{0, 0, 1, 1, 2, 3, 4}
	hot := map[FrameID]bool{0: true, 1: true}

	lru := NewLRUReplacer()
	recordSeq(lru, seq)
	lruVictim, ok := lru.Evict()
	if !ok {
		t.Fatal("LRU: no victim")
	}

	lruk := NewLRUKReplacer(2)
	recordSeq(lruk, seq)
	lrukVictim, ok := lruk.Evict()
	if !ok {
		t.Fatal("LRU-K: no victim")
	}

	// Plain LRU evicts a hot page: the scan made {2,3,4} most-recently-used and
	// pushed the working set to the bottom of the recency order. This is
	// sequential flooding.
	if !hot[lruVictim] {
		t.Fatalf("LRU evicted frame %d; expected a hot frame (0 or 1) — flooding not reproduced", lruVictim)
	}
	// LRU-2 evicts a cold scan page first (it has only one reference, so an
	// infinite backward-2 distance), preserving the working set.
	if hot[lrukVictim] {
		t.Fatalf("LRU-K evicted hot frame %d; expected a scan frame (2, 3, or 4)", lrukVictim)
	}
}

func TestLRUKDegeneratesToLRU(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		seq  []FrameID
	}{
		{"distinct", []FrameID{0, 1, 2, 3}},
		{"with repeats", []FrameID{0, 1, 0, 2, 1, 3}},
		{"reverse", []FrameID{3, 2, 1, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lru := NewLRUReplacer()
			recordSeq(lru, tc.seq)
			lruVictim, ok := lru.Evict()
			if !ok {
				t.Fatal("LRU: no victim")
			}

			lru1 := NewLRUKReplacer(1)
			recordSeq(lru1, tc.seq)
			lru1Victim, ok := lru1.Evict()
			if !ok {
				t.Fatal("LRU-1: no victim")
			}

			// Backward 1-distance is just recency, so LRU-1 is exactly LRU.
			if lruVictim != lru1Victim {
				t.Fatalf("LRU evicted %d but LRU-1 evicted %d — should match", lruVictim, lru1Victim)
			}
		})
	}
}

func ExampleClockReplacer() {
	r := NewClockReplacer(3)
	r.RecordAccess(0)
	r.RecordAccess(1)
	r.SetEvictable(0, true)
	r.SetEvictable(1, true)
	// Both frames have their reference bit set, so the first sweep clears the
	// bits and the second sweep evicts in hand order, starting at frame 0.
	v, _ := r.Evict()
	fmt.Println("victim:", v)
	// Output: victim: 0
}
```

## Review

The abstraction is correct when all three policies obey the same contract and only the heuristic differs. Each must refuse to evict when nothing is marked evictable, must return exactly the marked frame when one is, and must keep `Size` in step with `SetEvictable` and `Evict`. The flooding test is the one that proves LRU-K earns its complexity: on the identical trace LRU surrenders a hot page while LRU-2 surrenders a scan page, and LRU-1 reproducing LRU exactly confirms LRU-K is a strict generalization rather than a different algorithm. All three are deterministic on these traces because each frame's last access falls at a distinct clock tick, so there are no ties to resolve by map-iteration order.

The mistakes to avoid are subtle. In `LRUKReplacer.Evict`, treating an under-referenced frame as merely "very old" instead of giving it an infinite distance breaks flooding resistance — a scan page with one recent reference would look hot. Capping history at the wrong end (keeping the oldest k stamps instead of the newest k) makes backward k-distance drift and corrupts the ordering. In `LRUReplacer`, omitting the tie-break by frame id reintroduces map-iteration nondeterminism that a test cannot pin. And in `ClockReplacer`, decrementing the evictable count on `Evict` but forgetting to clear the presence flag would let a victim be returned twice.

## Resources

- [The LRU-K Page Replacement Algorithm For Database Disk Buffering (O'Neil, O'Neil, Weikum, SIGMOD 1993)](https://dl.acm.org/doi/10.1145/170035.170081) — backward K-distance and flooding resistance, the basis for this exercise.
- [2Q: A Low Overhead High Performance Buffer Management Replacement Algorithm (Johnson, Shasha, VLDB 1994)](https://www.vldb.org/conf/1994/P439.PDF) — a two-queue approximation of LRU-K at O(1) cost.
- [CMU 15-445 Lecture 6: Buffer Pool Management](https://15445.courses.cs.cmu.edu/fall2024/slides/06-bufferpool.pdf) — the buffer-pool project that builds an LRU-K replacer behind exactly this interface.

---

Back to [01-buffer-pool-core.md](01-buffer-pool-core.md) | Next: [03-read-ahead-prefetch.md](03-read-ahead-prefetch.md)
