# Exercise 16: Compact Kafka-Style Log Segments With slices.Delete

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Kafka retains a partition's log as a sequence of immutable segment files, and
once a topic's retention checkpoint advances past a segment's base offset,
the broker deletes that segment outright. A Go model of the in-memory
segment list reaches for the obvious translation: `entries = entries[cutoff:]`.
It compiles, it "removes" the expired segments from the visible slice, and it
passes every test that only checks `len(entries)` afterward. It is also
exactly the bug this module exists to catch.

Re-slicing moves the slice header's start pointer forward; it does not touch
a single byte of the discarded elements. They are still sitting, fully
intact, in the same backing array the new (shorter) slice still points into
-- and Go's garbage collector tracks liveness per backing array, not per
element, so as long as anything references any position in that array, the
whole array stays reachable. A log that compacts this way never actually
frees the segments it just "deleted": their data, and anything those
segments point to, is pinned in memory for as long as the array survives.
`slices.Delete` takes the opposite approach: it shifts survivors down over
the discarded prefix, physically overwriting it, and zeroes what is left
over at the tail. The discarded data is gone, not merely out of view.

This module builds `segmentlog`, a package modeling one partition's log:
a `Log` that keeps its segments sorted by offset, and a `Compact` method
that locates the retention cutoff with a binary search and removes the
expired prefix with a single `slices.Delete` call -- while never deleting
the segment currently being appended to, the same guarantee a real broker
gives its active segment.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
segmentlog/              module example.com/segmentlog
  go.mod                 go 1.24
  segmentlog.go           Segment, Log; New, (*Log).Compact, (*Log).Segments
  segmentlog_test.go      sorted-order validation, compact table, the
                          reslice-vs-Delete contrast, aliasing, ExampleLog_Compact
```

- Files: `segmentlog.go`, `segmentlog_test.go`.
- Implement: `New(segs []Segment) (*Log, error)` rejecting segments not sorted in strictly ascending order by `Offset` with `ErrUnsorted`; `(*Log).Compact(uptoOffset int64) (int, error)` that locates the cutoff with `slices.BinarySearchFunc`, clamps it so the active (newest) segment is never removed, and deletes the expired prefix with `slices.Delete`; `(*Log).Segments() []Segment`.
- Test: `New` rejecting descending, duplicate, and dipping offsets; a `Compact` table covering a checkpoint before every segment, between segments, exactly on a segment boundary, past every segment (active segment retained), a single-segment log, and an empty log; the reslice-vs-`Delete` contrast that pins the discarded data actually being overwritten; that `Segments` aliases internal state; and `ExampleLog_Compact` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/16-kafka-segment-prefix-compaction-delete
cd go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/16-kafka-segment-prefix-compaction-delete
go mod edit -go=1.24
```

### Delete shifts and overwrites; a reslice only moves a pointer

`slices.Delete(s, i, j)` removes `s[i:j]` by copying every element after `j`
down to start at index `i`, then zeroing the now-unused tail between the new
(shorter) length and the old one. For a prefix delete -- `i` is `0` -- that
copy step overwrites the discarded elements with the surviving ones: their
old contents do not linger anywhere in the backing array. Contrast that with
the naive prefix drop:

```go
// The naive translation of "drop everything before cutoff":
entries = entries[cutoff:]
```

This never runs a single write. It returns a new slice header whose start
pointer is `cutoff` positions further into the *same* backing array, and the
elements at indices `0` through `cutoff-1` are untouched -- still holding
their original field values, string headers included. If anything else
still references that array (or even if nothing does, but the array hasn't
been superseded by a fresh allocation from a later `append`), that memory is
not reclaimed. For a partition log that compacts on every retention sweep
but rarely triggers a full reallocation, the backing array only ever grows
to its historical peak size and never shrinks back down, no matter how
aggressively segments are compacted away.

`Log.Compact` locates the cutoff with `slices.BinarySearchFunc`, searching
for the first segment whose `Offset` is not less than `uptoOffset` -- segments
before that index are fully expired. It then clamps the cutoff to
`len(l.segments)-1` so the newest segment always survives, even when the
checkpoint has advanced past every offset in the log; a live partition never
deletes the segment it is still appending to. The actual removal is one
`slices.Delete` call.

Create `segmentlog.go`:

```go
// Package segmentlog models a Kafka-style partition log as an ordered list
// of append-only segments and compacts expired segments off the front with
// slices.Delete, which shifts survivors down in place and overwrites the
// discarded prefix instead of merely hiding it behind a moved slice header.
package segmentlog

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

// ErrUnsorted is returned by New when the supplied segments are not sorted
// in strictly ascending order by Offset, the invariant Compact relies on to
// binary-search for the cutoff index.
var ErrUnsorted = errors.New("segmentlog: segments must be sorted ascending by offset")

// Segment is one contiguous run of a partition's log, addressed by the
// offset of its first record.
type Segment struct {
	Offset int64
	Data   string
}

// Log is an ordered, in-memory model of a partition's log segments, oldest
// first. A Log is not safe for concurrent use; a caller that shares one
// across goroutines must synchronize access with its own lock.
type Log struct {
	segments []Segment
}

// New returns a Log over segs, which must be sorted in strictly ascending
// order by Offset. New clones segs, so later mutation of the caller's slice
// has no effect on the Log, and an empty or nil segs is a valid, empty Log.
func New(segs []Segment) (*Log, error) {
	for i := 1; i < len(segs); i++ {
		if segs[i].Offset <= segs[i-1].Offset {
			return nil, fmt.Errorf("%w: segment %d offset %d <= segment %d offset %d",
				ErrUnsorted, i, segs[i].Offset, i-1, segs[i-1].Offset)
		}
	}
	return &Log{segments: slices.Clone(segs)}, nil
}

// Compact deletes every segment whose Offset is strictly less than
// uptoOffset, except that it always retains at least the newest (active)
// segment: a live partition never deletes the segment it is currently
// appending to, no matter how far the retention checkpoint has advanced. It
// returns the number of segments removed; on a Log with no segments it
// returns (0, nil).
//
// Compact locates the cutoff with a binary search over the sorted offsets
// and removes the prefix with slices.Delete, which shifts the surviving
// segments down to the front of the backing array -- overwriting the
// discarded ones in place -- and then zeroes the leftover tail. It never
// allocates a new backing array.
func (l *Log) Compact(uptoOffset int64) (int, error) {
	if len(l.segments) == 0 {
		return 0, nil
	}
	cutoff, _ := slices.BinarySearchFunc(l.segments, uptoOffset, func(s Segment, upto int64) int {
		return cmp.Compare(s.Offset, upto)
	})
	cutoff = min(cutoff, len(l.segments)-1) // never delete the active segment
	if cutoff == 0 {
		return 0, nil
	}
	l.segments = slices.Delete(l.segments, 0, cutoff)
	return cutoff, nil
}

// Segments returns the Log's current segments, oldest first. The returned
// slice aliases the Log's internal state: the caller must not mutate it,
// and it is invalidated by the next call to Compact, which shifts and
// zeroes the same backing array in place. Call slices.Clone to retain an
// independent copy across a mutating call.
func (l *Log) Segments() []Segment {
	return l.segments
}
```

### Using it

Construct a `Log` once from the segments read off disk at startup, then call
`Compact` on the interval your retention policy uses (a timer, or a hook off
the last committed checkpoint). `New` guarantees the segments are sorted, and
`Compact` preserves that invariant across every call, so a caller never has
to re-sort. `Segments` hands back a view into the `Log`'s own storage --
cheap, but only valid until the next `Compact`, exactly as documented on
both methods.

The module has no `main.go`; a log's compaction step is a package a service
imports, not a standalone binary. Its executable demonstration is
`ExampleLog_Compact`: `go test` runs it and compares its stdout against the
`// Output:` comment, so the usage shown below cannot drift from the code.

### Tests

`TestNewRejectsUnsortedSegments` covers a descending pair, duplicate
offsets, and a slice that starts sorted and then dips -- every shape of
violated invariant `Compact`'s binary search depends on. `TestCompact` is
the table: a checkpoint before every segment, between two, landing exactly
on a segment's offset (which must be kept, not deleted, since retention is
strictly-less-than), past every segment (proving the active segment
survives), a single-segment log, and an empty log.

`TestPrefixDeleteErasesDiscardedDataNaiveResliceLeavesItIntact` is the
module's core: it keeps a second reference to the same backing array a
naive `entries[cutoff:]` reslice produces, and shows the discarded elements
are still fully readable through it -- proof the reslice never touched them.
It then runs the same input through `Log.Compact` and shows the discarded
segments' data does not survive anywhere in the backing array, including its
freed tail, which `slices.Delete` zeroes. `TestSegmentsAliasesInternalState`
pins that `Segments()` is a live view, not a copy.

Create `segmentlog_test.go`:

```go
package segmentlog

import (
	"errors"
	"fmt"
	"testing"
)

func mkSegments(n int) []Segment {
	s := make([]Segment, n)
	for i := range s {
		s[i] = Segment{Offset: int64(i * 100), Data: fmt.Sprintf("seg-%d", i)}
	}
	return s
}

// compactByReslice is the naive translation of "drop the compacted prefix":
// entries = entries[cutoff:]. It moves the slice header forward but never
// touches the discarded elements or the backing array they live in, so the
// whole discarded prefix stays exactly as it was -- reachable through any
// other alias of the same array, and still part of the single allocation
// that the live slice points into. It is unexported, unreachable from the
// package API, and exists only so the tests can pin what it gets wrong.
func compactByReslice(entries []Segment, cutoff int) []Segment {
	return entries[cutoff:]
}

func TestNewRejectsUnsortedSegments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		segs []Segment
	}{
		{"descending pair", []Segment{{Offset: 10}, {Offset: 5}}},
		{"duplicate offsets", []Segment{{Offset: 10}, {Offset: 10}}},
		{"sorted then a dip", []Segment{{Offset: 1}, {Offset: 2}, {Offset: 0}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.segs); !errors.Is(err, ErrUnsorted) {
				t.Fatalf("New(%v) err = %v, want ErrUnsorted", tc.segs, err)
			}
		})
	}
}

func TestNewAcceptsEmptyAndNil(t *testing.T) {
	t.Parallel()

	for _, segs := range [][]Segment{nil, {}} {
		l, err := New(segs)
		if err != nil {
			t.Fatalf("New(%v): %v", segs, err)
		}
		if len(l.Segments()) != 0 {
			t.Fatalf("Segments() = %v, want empty", l.Segments())
		}
	}
}

func TestCompact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		offsets     []int64
		uptoOffset  int64
		wantRemoved int
		wantOffsets []int64
	}{
		{"checkpoint before every segment", []int64{10, 20, 30, 40}, 5, 0, []int64{10, 20, 30, 40}},
		{"checkpoint between segments", []int64{10, 20, 30, 40}, 25, 2, []int64{30, 40}},
		{"checkpoint exactly on a segment boundary keeps it", []int64{10, 20, 30, 40}, 20, 1, []int64{20, 30, 40}},
		{"checkpoint past every segment keeps the active one", []int64{10, 20, 30, 40}, 10000, 3, []int64{40}},
		{"single segment is always active", []int64{10}, 10000, 0, []int64{10}},
		{"empty log", nil, 999, 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			segs := make([]Segment, len(tc.offsets))
			for i, off := range tc.offsets {
				segs[i] = Segment{Offset: off, Data: fmt.Sprintf("d%d", off)}
			}
			l, err := New(segs)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			removed, err := l.Compact(tc.uptoOffset)
			if err != nil {
				t.Fatalf("Compact: %v", err)
			}
			if removed != tc.wantRemoved {
				t.Fatalf("removed = %d, want %d", removed, tc.wantRemoved)
			}
			got := l.Segments()
			if len(got) != len(tc.wantOffsets) {
				t.Fatalf("len(Segments()) = %d, want %d: %+v", len(got), len(tc.wantOffsets), got)
			}
			for i, want := range tc.wantOffsets {
				if got[i].Offset != want {
					t.Errorf("Segments()[%d].Offset = %d, want %d", i, got[i].Offset, want)
				}
			}
		})
	}
}

// TestPrefixDeleteErasesDiscardedDataNaiveResliceLeavesItIntact is the heart
// of the module: it shows the observable difference between the naive
// "entries = entries[cutoff:]" translation and Log.Compact's use of
// slices.Delete.
//
// The naive reslice only moves the slice header forward. It never writes to
// the discarded elements, so they remain fully intact and readable through
// any other reference to the same backing array -- exactly the condition
// that keeps that memory (and anything the discarded elements point to)
// reachable and pinned, instead of collectable.
//
// slices.Delete instead shifts the surviving elements down over the
// discarded ones, physically overwriting their contents, and zeroes the
// leftover tail. The discarded data is gone from the backing array, not
// merely out of view.
func TestPrefixDeleteErasesDiscardedDataNaiveResliceLeavesItIntact(t *testing.T) {
	t.Parallel()

	// Naive path: entries[cutoff:]. Keep a second reference, full, to the
	// same backing array so the discarded prefix stays observable.
	full := mkSegments(6)
	naive := compactByReslice(full, 3)

	if len(naive) != 3 || naive[0].Data != "seg-3" {
		t.Fatalf("compactByReslice = %+v, want segments 3..5", naive)
	}
	if cap(naive) != cap(full)-3 {
		t.Fatalf("naive reslice cap = %d, want %d: it must share the same backing array, never truncate it", cap(naive), cap(full)-3)
	}
	for i, want := range []string{"seg-0", "seg-1", "seg-2"} {
		if full[i].Data != want {
			t.Fatalf("full[%d].Data = %q, want %q: naive reslice must never touch discarded elements", i, full[i].Data, want)
		}
	}

	// Correct path: Log.Compact via slices.Delete.
	l, err := New(mkSegments(6))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	removed, err := l.Compact(300)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed = %d, want 3", removed)
	}

	survivors := l.Segments()
	raw := survivors[:cap(survivors)]
	for i, want := range []string{"seg-3", "seg-4", "seg-5"} {
		if raw[i].Data != want {
			t.Fatalf("raw[%d].Data = %q, want %q", i, raw[i].Data, want)
		}
	}
	for i := len(survivors); i < cap(survivors); i++ {
		if raw[i] != (Segment{}) {
			t.Fatalf("freed slot [%d] = %+v, want the zero value", i, raw[i])
		}
	}
	for _, discarded := range []string{"seg-0", "seg-1", "seg-2"} {
		for _, s := range raw {
			if s.Data == discarded {
				t.Fatalf("discarded segment %q is still present in the backing array after Compact", discarded)
			}
		}
	}
}

func TestSegmentsAliasesInternalState(t *testing.T) {
	t.Parallel()

	l, err := New(mkSegments(3))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := l.Segments()
	if len(got) != 3 {
		t.Fatalf("len(Segments()) = %d, want 3", len(got))
	}
	if _, err := l.Compact(200); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	// got's own length header is unaffected (Go slices are value headers),
	// but the array it points into has been shifted and zeroed by Compact.
	if got[0].Data != "seg-2" {
		t.Fatalf("got[0].Data = %q after Compact, want %q: Segments() aliases the backing array", got[0].Data, "seg-2")
	}
}

// ExampleLog_Compact is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleLog_Compact() {
	l, err := New([]Segment{
		{Offset: 0, Data: "seg-0"},
		{Offset: 100, Data: "seg-1"},
		{Offset: 200, Data: "seg-2"},
		{Offset: 300, Data: "seg-3"},
	})
	if err != nil {
		panic(err)
	}

	removed, err := l.Compact(150)
	if err != nil {
		panic(err)
	}
	fmt.Printf("compact(150): removed=%d remaining=%d\n", removed, len(l.Segments()))
	for _, s := range l.Segments() {
		fmt.Printf("  offset=%d data=%s\n", s.Offset, s.Data)
	}

	// A checkpoint past every segment's offset still retains the newest
	// (active) segment: a live log never deletes what it is appending to.
	removed, err = l.Compact(10000)
	if err != nil {
		panic(err)
	}
	fmt.Printf("compact(10000): removed=%d remaining=%d\n", removed, len(l.Segments()))
	for _, s := range l.Segments() {
		fmt.Printf("  offset=%d data=%s\n", s.Offset, s.Data)
	}

	// Output:
	// compact(150): removed=2 remaining=2
	//   offset=200 data=seg-2
	//   offset=300 data=seg-3
	// compact(10000): removed=1 remaining=1
	//   offset=300 data=seg-3
}
```

## Review

`Log` is correct when `Compact` leaves exactly the segments whose offset is
at or after the checkpoint, plus always the newest one, and when the
segments it discarded no longer occupy any live memory. `slices.Delete` is
what buys the second property: it shifts survivors down over the discarded
prefix and zeroes the tail, so nothing keeps the old data reachable. The
trap it replaces -- `entries = entries[cutoff:]` -- only moves a pointer
forward inside the same backing array; the discarded elements are still
sitting there, still readable through any other alias, and the array itself
never shrinks. `New` enforces the sorted invariant `Compact`'s binary search
depends on with `ErrUnsorted`, checkable via `errors.Is`. `Segments`
deliberately aliases the `Log`'s own storage rather than copying, which is
cheap but means the caller must treat it as invalidated by the next
`Compact`. Run `go test -count=1 -race ./...` to confirm the sort
validation, the compact table across every boundary, the reslice-versus-
`Delete` contrast, and the aliasing behavior.

## Resources

- [`slices.Delete`](https://pkg.go.dev/slices#Delete) — the shift-and-zero-tail contract this module relies on.
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — locating the cutoff index over a custom-keyed sorted slice.
- [Kafka: Log Compaction and Retention](https://kafka.apache.org/documentation/#compaction) — the real system this module's domain is drawn from.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — idiomatic slice removal patterns and their aliasing behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-wal-replay-backward-iteration.md](15-wal-replay-backward-iteration.md) | Next: [17-ssh-hostkey-pinset-contains.md](17-ssh-hostkey-pinset-contains.md)
