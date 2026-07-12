# Exercise 15: Clip-Guarded Memtable Compaction

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every LSM-tree storage engine -- LevelDB, RocksDB, and the memtable stage
underneath BoltDB-adjacent designs -- keeps writes in an in-memory,
append-only batch before periodically compacting it: dropping tombstoned
keys and handing the survivors off as an immutable segment to be serialized
to disk. The natural, allocation-light way to write that compaction step is
to filter in place, reusing the batch's own backing array instead of paying
for a fresh one on every flush cycle. That reuse is exactly where the bug
lives: the compacted segment you just handed off and the memtable's own
next writes still point at the same backing array, and whichever one
appends next -- the caller adding a trailer before serializing, or the
memtable resuming inserts -- silently overwrites the other's data.

This is a narrower, sharper case of the general slice-pinning problem this
lesson covers. It is not about a consumer growing into a producer's data (a
downstream reader misbehaving), and it is not about retaining a slice past
its documented lifetime (a producer misbehaving). It is about two *legitimate*
appends to the *same array*, both correct in isolation, colliding because
neither one's owner clipped its capacity first. `slices.Clip` is the
one-line fix, and understanding exactly what it does and does not
guarantee -- it trims capacity to length; it does not copy, and it does not
sever the read pin on the array's current content -- is what keeps a senior
engineer from reaching for it as a substitute for `slices.Clone` in the
wrong situation.

This module builds a `Memtable` that compacts correctly -- always clipping
the segment it returns -- and a `Segment` that is safe to extend with
`AppendEntry` once it has been. The uncapped version that collides is not
part of that API; it lives in the test file, isolated as the thing the
tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
memtable/                module example.com/memtable
  go.mod                 go 1.24
  memtable.go            Entry, Memtable, Segment; NewMemtable, Put, Delete, Compact, AppendEntry
  memtable_test.go        tombstone table, the buggy-compact collision, capacity edges, ExampleMemtable
```

- Files: `memtable.go`, `memtable_test.go`.
- Implement: `NewMemtable(maxEntries int) (*Memtable, error)` rejecting a non-positive size with `ErrInvalidCapacity`; `(*Memtable).Put(key string, value []byte) error` and `(*Memtable).Delete(key string) error`, both returning `ErrMemtableFull` once the memtable holds `maxEntries` records; `(*Memtable).Compact() Segment`, which filters tombstones in place and returns the survivors with `slices.Clip` applied; `(Segment).AppendEntry(e Entry) Segment`, which allocates a fresh array because the segment it extends is always clipped.
- Test: a tombstone-filtering table (empty, no tombstones, all tombstones, mixed); the collision `compactBuggy` reproduces when a returned segment is not clipped, contrasted against `Compact` where the same sequence never collides; `NewMemtable` rejecting non-positive capacity; `Put`/`Delete` rejecting writes once full; `ExampleMemtable` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Clip trims capacity; it does not copy, and it does not detach

`slices.Clip(s)` returns `s[:len(s):len(s)]`: same backing array, same
visible content, capacity forced down to length. It costs nothing --
no allocation, no copy -- which is exactly why it is tempting to reach for
it whenever a boundary problem involving `append` shows up. But its
guarantee is narrow: it changes what the *next* `append` on that value does
(reallocate, because there is no spare room), and it changes nothing else.
The array underneath is exactly the one it was before; anyone who already
holds a header into that array, including the memtable that produced the
segment, still reads the same bytes.

That narrow guarantee is precisely what `Compact` needs. `Compact` filters
tombstones out of `m.entries` in place -- `kept := m.entries[:0]` followed
by appending only the live entries back into the same array -- because
paying for a fresh array on every flush cycle, most of which touch a small
minority of tombstoned keys, is wasted work. After filtering, `m.entries`
and the segment about to be returned share one array: `m.entries` has
whatever spare capacity was already there from ordinary growth, and the
segment, if returned unclipped, has that same spare capacity. The memtable
resumes writing into that shared space on its very next `Put`. A caller
that appends a trailer entry to the segment before serializing it -- an
entirely ordinary thing to do -- writes into the identical space. One of
the two writes wins; the other vanishes without an error, a panic, or a
race-detector complaint, because both operations are sequential, single-
threaded appends to a slice with room to spare:

```go
// memtable.go -- the bug, if Compact forgot to clip.
func (m *Memtable) Compact() Segment {
    kept := m.entries[:0]
    for _, e := range m.entries {
        if !e.Tombstone {
            kept = append(kept, e)
        }
    }
    m.entries = kept
    return Segment{entries: kept} // shares m's spare capacity -- not clipped
}
```

Clipping `kept` before returning it changes nothing about what `Compact`
just computed; it only removes the spare room a future append could land
in. The segment's own next `AppendEntry` then reallocates -- a fresh array,
independent of the memtable -- and the memtable's own next `Put` or
`Delete` reallocates too, the moment it needs the first byte beyond the
segment's clipped length. Neither write can reach the other's array again.
This bound is deliberately narrow in time: it holds for the one compaction
cycle during which the segment is expected to be serialized and discarded.
A design that must keep segments alive across several compaction cycles
needs `slices.Clone` instead, at the cost of one allocation per cycle --
that is the tradeoff the concept notes make explicit: reach for Clip when
you own the array and only need to bound its next growth; reach for Clone
when you must not pin, or share, the source at all.

Create `memtable.go`:

```go
// Package memtable models the memtable stage of an LSM-tree storage engine,
// as in LevelDB, RocksDB, and BoltDB-adjacent designs: writes accumulate in
// an in-memory, append-only batch before being periodically compacted (to
// drop tombstoned keys) and handed off as an immutable Segment for a caller
// to serialize to disk.
//
// It exists to get one detail right that a hand-rolled compactor routinely
// gets wrong: Compact filters tombstones in place, reusing the memtable's
// backing array to avoid an allocation on every flush cycle, and then must
// clip the surviving slice before returning it. Without that clip, the
// returned Segment and the memtable's own next writes still share spare
// capacity in the same backing array, and whichever one appends next
// silently overwrites the other's data. See the package tests for that
// collision reproduced without the clip.
package memtable

import (
	"errors"
	"fmt"
	"slices"
)

// Sentinel errors returned by NewMemtable and Put. Callers should test for
// them with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidCapacity means the configured max entry count was not positive.
	ErrInvalidCapacity = errors.New("memtable: max entries must be positive")
	// ErrMemtableFull means Put was called after the memtable reached its
	// configured maximum entry count.
	ErrMemtableFull = errors.New("memtable: at capacity, flush required")
)

// Entry is one key/value record in a Memtable, or a tombstone marking a
// deleted key.
type Entry struct {
	Key       string
	Value     []byte
	Tombstone bool
}

// Memtable is an in-memory, append-only batch of Entry records. Put and
// Delete both append; Compact drops tombstones in place and returns the
// survivors as a Segment.
//
// Memtable is not safe for concurrent use; a real storage engine routes all
// writes to the active memtable through a single owning goroutine.
type Memtable struct {
	entries []Entry
	max     int
}

// NewMemtable returns an empty Memtable that refuses Put and Delete once it
// holds maxEntries records. It returns ErrInvalidCapacity if maxEntries is
// not positive.
func NewMemtable(maxEntries int) (*Memtable, error) {
	if maxEntries <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, maxEntries)
	}
	return &Memtable{max: maxEntries}, nil
}

// Len reports the number of entries currently held, tombstones included.
func (m *Memtable) Len() int { return len(m.entries) }

// Put appends a live entry for key. It returns ErrMemtableFull if the
// memtable is already at its configured maximum entry count.
func (m *Memtable) Put(key string, value []byte) error {
	if len(m.entries) >= m.max {
		return fmt.Errorf("%w: max %d", ErrMemtableFull, m.max)
	}
	m.entries = append(m.entries, Entry{Key: key, Value: value})
	return nil
}

// Delete appends a tombstone for key. The prior value, if any, is not
// physically removed until the next Compact. It returns ErrMemtableFull
// under the same condition as Put.
func (m *Memtable) Delete(key string) error {
	if len(m.entries) >= m.max {
		return fmt.Errorf("%w: max %d", ErrMemtableFull, m.max)
	}
	m.entries = append(m.entries, Entry{Key: key, Tombstone: true})
	return nil
}

// Compact drops every tombstoned entry, filtering in place to reuse the
// memtable's backing array rather than allocating a new one on every flush
// cycle. The memtable keeps operating on that same array afterward, at the
// shorter, compacted length -- Put and Delete simply resume appending past
// it.
//
// Compact judges each entry individually by its own Tombstone flag; it does
// not deduplicate repeated writes to the same key. A full LSM engine also
// keeps only the newest entry per key, which needs bookkeeping orthogonal
// to the array-capacity lesson this type demonstrates.
//
// The returned Segment's capacity is clipped to its length (slices.Clip),
// which is what makes it safe to keep and extend: the memtable's own next
// append lands past the segment's clipped capacity in a region the segment
// cannot see, and the segment's own next append (AppendEntry, e.g. to add a
// trailer before serializing) reallocates instead of writing into the
// memtable's live storage. This bound holds for exactly one compaction
// cycle: a Segment is meant to be serialized and discarded before the next
// Compact runs, not held across several. A design that must keep segments
// alive across many compactions should have Compact return a
// slices.Clone-d copy instead, at the cost of one allocation per cycle.
func (m *Memtable) Compact() Segment {
	kept := m.entries[:0]
	for _, e := range m.entries {
		if !e.Tombstone {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	return Segment{entries: slices.Clip(kept)}
}

// Segment is an immutable, flushed batch of live entries produced by
// Compact. The zero Segment is empty.
type Segment struct {
	entries []Entry
}

// Entries returns the segment's records. The caller must not mutate the
// returned slice's elements in place; use AppendEntry to extend the
// segment.
func (s Segment) Entries() []Entry { return s.entries }

// Len reports the number of entries in the segment.
func (s Segment) Len() int { return len(s.entries) }

// AppendEntry returns a new Segment with e appended after s's existing
// entries. Because Compact clips every Segment's capacity to its length,
// this always allocates a fresh backing array: it never aliases s, and it
// never touches memory the Memtable that produced s might be writing into.
func (s Segment) AppendEntry(e Entry) Segment {
	return Segment{entries: append(s.entries, e)}
}
```

### Using it

A `Memtable` is the write path of a toy storage engine: construct it with
the batch size that triggers a flush, `Put` and `Delete` into it, and call
`Compact` when it is time to serialize. The value it returns, `Segment`, is
what you hand to a serializer -- and what makes it safe to extend with a
format trailer via `AppendEntry` before writing it out, even though the
`Memtable` that produced it keeps running and accepting new writes in the
same call.

`ExamplePutDelete`, shown below in its test-file section, is the runnable
demonstration of this module: `go test` runs it and compares its standard
output against the `// Output:` comment, so the usage shown there cannot
drift away from the code. The one aliasing contract worth internalizing
before using this package elsewhere: `Segment.Entries()` returns a view,
not a defensive copy -- `Compact` clips its capacity but does not clone its
content, so mutating an element of the returned slice in place (as opposed
to appending to it) is not guarded against, and is the caller's
responsibility to avoid.

### Tests

`TestBuggyCompactCorruptsSegmentOnConcurrentWrites` is the module's center
of gravity. `compactBuggy` is unexported and unreachable from the package
API; it is `Compact` with the clip deleted. The test builds a memtable with
one tombstoned key so that, after filtering, the kept slice is strictly
shorter than the array's capacity -- guaranteeing the collision reproduces
regardless of exactly how much spare capacity `append`'s growth curve left
behind, since any positive margin is enough. It appends a trailer to the
unclipped segment, then performs the memtable's next write, and asserts
that the trailer slot silently became the new entry instead of an error.
`TestCompactClipsSoAppendEntryNeverCollidesWithMemtable` runs the identical
sequence through the real `Compact` and asserts the opposite: the trailer
survives, and the memtable's own length is unaffected by what the segment
does. If a future edit ever drops the clip from `Compact`, this pair of
tests fails at exactly the line that matters, instead of in a corrupted
on-disk segment.

`TestCompactDropsTombstones` is the ordinary table: empty input, no
tombstones, all tombstones, and a mixed batch, asserting both the count of
survivors and that none of them carry `Tombstone == true`.
`TestNewMemtableRejectsNonPositiveCapacity` and
`TestPutAndDeleteRejectWhenFull` cover the two edges the constructor and
the writers must handle: a non-positive configured size, and a memtable
that has reached it. `Memtable` is not safe for concurrent use, so there is
no concurrency test here; the borde coverage is the capacity and tombstone
edges above.

Create `memtable_test.go`:

```go
package memtable

import (
	"errors"
	"fmt"
	"testing"
)

// compactBuggy is Compact with the fix removed: it filters tombstones in
// place exactly like Compact, but returns the survivors without clipping
// their capacity. It is never exported and never reachable from the
// package API; it exists only so the tests can pin the collision it
// causes.
func compactBuggy(m *Memtable) Segment {
	kept := m.entries[:0]
	for _, e := range m.entries {
		if !e.Tombstone {
			kept = append(kept, e)
		}
	}
	m.entries = kept
	return Segment{entries: kept} // BUG: no slices.Clip
}

func fill3with1Tombstone(t *testing.T, m *Memtable) {
	t.Helper()
	if err := m.Put("a", []byte("1")); err != nil {
		t.Fatalf("Put a: %v", err)
	}
	if err := m.Put("b", []byte("2")); err != nil {
		t.Fatalf("Put b: %v", err)
	}
	if err := m.Put("c", []byte("3")); err != nil {
		t.Fatalf("Put c: %v", err)
	}
	if err := m.Delete("b"); err != nil {
		t.Fatalf("Delete b: %v", err)
	}
}

// TestBuggyCompactCorruptsSegmentOnConcurrentWrites is the heart of the
// module. compactBuggy hands out a segment that still shares spare
// capacity with the memtable's own backing array. Appending a trailer to
// that segment writes into the shared array; the memtable's very next
// write lands in the identical slot and silently clobbers the trailer. If
// a future edit reintroduces this by dropping the clip from Compact, this
// test fails here instead of in a corrupted on-disk segment.
func TestBuggyCompactCorruptsSegmentOnConcurrentWrites(t *testing.T) {
	t.Parallel()

	m, err := NewMemtable(10)
	if err != nil {
		t.Fatalf("NewMemtable: %v", err)
	}
	fill3with1Tombstone(t, m)

	seg := compactBuggy(m)
	if seg.Len() != 3 {
		t.Fatalf("seg.Len() = %d, want 3", seg.Len())
	}

	seg2 := seg.AppendEntry(Entry{Key: "trailer", Value: []byte("EOF")})
	if err := m.Put("d", []byte("4")); err != nil {
		t.Fatalf("Put d: %v", err)
	}

	got := seg2.Entries()[3].Key
	if got != "d" {
		t.Fatalf("expected the unclipped segment's trailer slot to be clobbered by the memtable's next write, got key %q", got)
	}
}

// TestCompactClipsSoAppendEntryNeverCollidesWithMemtable is the contrast:
// the real Compact clips the returned segment's capacity, so the same
// sequence of operations never collides.
func TestCompactClipsSoAppendEntryNeverCollidesWithMemtable(t *testing.T) {
	t.Parallel()

	m, err := NewMemtable(10)
	if err != nil {
		t.Fatalf("NewMemtable: %v", err)
	}
	fill3with1Tombstone(t, m)

	seg := m.Compact()
	if seg.Len() != 3 {
		t.Fatalf("seg.Len() = %d, want 3", seg.Len())
	}

	seg2 := seg.AppendEntry(Entry{Key: "trailer", Value: []byte("EOF")})
	if err := m.Put("d", []byte("4")); err != nil {
		t.Fatalf("Put d: %v", err)
	}

	got := seg2.Entries()[3].Key
	if got != "trailer" {
		t.Fatalf("clipped segment's trailer entry was overwritten: got %q, want %q", got, "trailer")
	}
	if m.Len() != 4 {
		t.Fatalf("m.Len() = %d, want 4 (3 kept + 1 new)", m.Len())
	}
}

func TestCompactDropsTombstones(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		build   func(t *testing.T, m *Memtable)
		wantLen int
	}{
		{
			name:    "empty memtable",
			build:   func(t *testing.T, m *Memtable) {},
			wantLen: 0,
		},
		{
			name: "no tombstones",
			build: func(t *testing.T, m *Memtable) {
				must(t, m.Put("x", nil))
				must(t, m.Put("y", nil))
			},
			wantLen: 2,
		},
		{
			name: "all tombstones",
			build: func(t *testing.T, m *Memtable) {
				must(t, m.Delete("x"))
				must(t, m.Delete("y"))
			},
			wantLen: 0,
		},
		{
			name:    "mixed",
			build:   fillMixed,
			wantLen: 4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m, err := NewMemtable(20)
			if err != nil {
				t.Fatalf("NewMemtable: %v", err)
			}
			tc.build(t, m)
			seg := m.Compact()
			if seg.Len() != tc.wantLen {
				t.Fatalf("seg.Len() = %d, want %d", seg.Len(), tc.wantLen)
			}
			for _, e := range seg.Entries() {
				if e.Tombstone {
					t.Errorf("segment retained a tombstone for key %q", e.Key)
				}
			}
		})
	}
}

func fillMixed(t *testing.T, m *Memtable) {
	t.Helper()
	must(t, m.Put("a", nil))
	must(t, m.Delete("z"))
	must(t, m.Put("b", nil))
	must(t, m.Put("c", nil))
	must(t, m.Delete("a"))
	must(t, m.Put("a", []byte("v2")))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewMemtableRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()

	for _, max := range []int{0, -1} {
		if _, err := NewMemtable(max); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("NewMemtable(%d) error = %v, want ErrInvalidCapacity", max, err)
		}
	}
}

func TestPutAndDeleteRejectWhenFull(t *testing.T) {
	t.Parallel()

	m, err := NewMemtable(2)
	if err != nil {
		t.Fatalf("NewMemtable: %v", err)
	}
	must(t, m.Put("a", nil))
	must(t, m.Put("b", nil))

	if err := m.Put("c", nil); !errors.Is(err, ErrMemtableFull) {
		t.Errorf("Put at capacity: err = %v, want ErrMemtableFull", err)
	}
	if err := m.Delete("a"); !errors.Is(err, ErrMemtableFull) {
		t.Errorf("Delete at capacity: err = %v, want ErrMemtableFull", err)
	}
}

// ExampleMemtable is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleMemtable() {
	m, err := NewMemtable(10)
	if err != nil {
		panic(err)
	}
	_ = m.Put("user:1", []byte("alice"))
	_ = m.Put("user:2", []byte("bob"))
	_ = m.Delete("user:2")
	_ = m.Put("user:3", []byte("carol"))

	seg := m.Compact()
	fmt.Printf("segment: %d live entries, memtable resumed at len=%d\n", seg.Len(), m.Len())

	final := seg.AppendEntry(Entry{Key: "__trailer__", Value: []byte("EOF")})
	_ = m.Put("user:4", []byte("dave"))

	fmt.Printf("segment trailer intact: %q\n", final.Entries()[seg.Len()].Key)
	fmt.Printf("memtable now has %d live entries\n", m.Len())

	// Output:
	// segment: 3 live entries, memtable resumed at len=3
	// segment trailer intact: "__trailer__"
	// memtable now has 4 live entries
}
```

## Review

`Compact` is correct when the segment it returns can be extended without
ever colliding with the memtable that produced it -- and that is true
exactly because it clips the segment's capacity before returning it, never
because it copies the segment's content into a new array. Clip and Clone
solve different problems: Clip bounds what a value you already own can grow
into next; Clone severs a pin on the array itself. Using Clip where the
segment needed to survive many compaction cycles would be a mistake in the
other direction -- the clip only protects against the one collision this
module isolates, not against a later `Compact` reusing the same array from
index zero. `NewMemtable` rejects a non-positive capacity with
`ErrInvalidCapacity`, and `Put`/`Delete` reject writes past capacity with
`ErrMemtableFull`, both checkable with `errors.Is`. The pair of contrast
tests is what proves the mechanism: `compactBuggy`, confined to the test
file and never exported, reproduces the exact silent overwrite that an
unclipped `Compact` would ship to production; the real `Compact` runs the
identical sequence and never collides. Run `go test -count=1 -race ./...`.

## Resources

- [`slices.Clip`](https://pkg.go.dev/slices#Clip) — trims capacity to length without allocating; the exact operation this module hinges on.
- [`slices.Clone`](https://pkg.go.dev/slices#Clone) — the allocation-costing alternative, needed when a value must not share an array with its source at all.
- [Go Spec: Appending and copying slices](https://go.dev/ref/spec#Appending_and_copying_slices) — why `append` writes into spare capacity when there is any, and reallocates when there is none.
- [LevelDB: Implementation notes on memtables and SSTables](https://github.com/google/leveldb/blob/main/doc/impl.md) — the real system whose write path this module's `Memtable`/`Segment` pair sketches.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-ndjson-batch-redactor-clear-reuse.md](14-ndjson-batch-redactor-clear-reuse.md) | Next: [16-connpool-element-pointer-pin.md](16-connpool-element-pointer-pin.md)
