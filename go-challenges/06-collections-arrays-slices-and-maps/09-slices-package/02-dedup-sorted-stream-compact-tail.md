# Exercise 2: Deduplicate A Sorted Event Stream And Free The Tail (Compact zeroing + GC)

An event-ingestion pipeline often receives an already-sorted batch of `*Event`
pointers (sorted by id at the source) and must collapse consecutive duplicates
before storing them. `slices.CompactFunc` is the right tool, and the senior point
is what happens to the retired pointers: `Compact` zeroes the freed tail, so the
duplicate `*Event` values become unreachable and collectable instead of being
pinned by the backing array. This module builds the dedup step and proves the
tail-zeroing contract directly.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
dedupstream/                   module example.com/dedupstream
  go.mod                       go 1.24
  dedup.go                     type Event; Dedup (IsSortedFunc guard, CompactFunc, Clip)
  cmd/
    demo/
      main.go                  runnable demo: dedup a sorted batch, show len shrink
  dedup_test.go                dedup correctness, consecutive-only, tail-zeroing, Clip
```

- Files: `dedup.go`, `cmd/demo/main.go`, `dedup_test.go`.
- Implement: `Dedup([]*Event) []*Event` that requires a slice sorted by id, collapses consecutive duplicate ids with `slices.CompactFunc`, and returns the shortened slice; plus `IsSortedByID` and a `Clip`-based `DedupClip`.
- Test: correct dedup on runs of duplicates, no-op on unique input, consecutive-only behavior on unsorted input, tail slots zeroed (nil) after compaction, and `Clip` capping capacity.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why Compact only works on sorted input, and what zeroing buys you

`slices.Compact` and `slices.CompactFunc` collapse *consecutive* runs of equal
elements into one. They do not de-duplicate globally: two equal elements with a
different element between them both survive. So the precondition for using
`CompactFunc` as a de-duplicator is that the slice is sorted on the same key you
compare — then all equals are adjacent. The stream here arrives sorted by id, so
a single `CompactFunc` pass keyed on id removes every duplicate. `Dedup` asserts
the precondition with `slices.IsSortedFunc` and refuses (returns an error via a
sentinel) rather than silently producing a half-deduplicated result, because a
violated sort invariant is exactly the kind of drift that turns into a
production bug.

The elements are `*Event`. When `CompactFunc` collapses a run of duplicates, it
shifts survivors down and then zeroes the slots between the new length and the
old length — for a pointer slice, it writes `nil` into those trailing slots. That
is not cosmetic: if those slots kept pointing at the retired `*Event` values, the
backing array would keep them reachable and the garbage collector could not
reclaim them, even though your logical slice is shorter. Zeroing drops the
references. The contract only holds if you keep the *returned* (shorter) slice as
the live header, so the zeroed slots live in the unreachable `[len:cap]` region.
The tail-zeroing test re-slices the result up to its capacity and asserts those
slots are nil — a direct, deterministic observation of the release.

`DedupClip` adds `slices.Clip` after compaction. Clip caps capacity at length, so
the freed tail (with its nil slots) is dropped from the slice's reach entirely and
a later `append` allocates instead of reusing that region. When the deduplicated
result is going to be stored long-term, clipping also lets the whole oversized
backing array be collected if the short slice is the only reference.

Create `dedup.go`:

```go
package dedupstream

import (
	"cmp"
	"errors"
	"slices"
)

// ErrNotSorted is returned when Dedup receives a slice that is not sorted by id,
// the precondition CompactFunc-based deduplication depends on.
var ErrNotSorted = errors.New("dedupstream: input not sorted by id")

// Event is an ingested event; the stream is keyed and sorted by ID.
type Event struct {
	ID      int
	Payload string
}

func byID(a, b *Event) int { return cmp.Compare(a.ID, b.ID) }

// IsSortedByID reports whether s is ascending by ID.
func IsSortedByID(s []*Event) bool {
	return slices.IsSortedFunc(s, byID)
}

// Dedup collapses consecutive duplicate-ID events in an already-sorted slice,
// reusing the backing array and zeroing the freed tail so retired *Event values
// become collectable. It returns ErrNotSorted if the precondition is violated.
func Dedup(s []*Event) ([]*Event, error) {
	if !IsSortedByID(s) {
		return nil, ErrNotSorted
	}
	return slices.CompactFunc(s, func(a, b *Event) bool {
		return a.ID == b.ID
	}), nil
}

// DedupClip is Dedup followed by Clip, capping capacity at the deduplicated
// length so a later append allocates and the oversized array can be released.
func DedupClip(s []*Event) ([]*Event, error) {
	out, err := Dedup(s)
	if err != nil {
		return nil, err
	}
	return slices.Clip(out), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dedupstream"
)

func main() {
	batch := []*dedupstream.Event{
		{ID: 1, Payload: "a"},
		{ID: 1, Payload: "a-dup"},
		{ID: 2, Payload: "b"},
		{ID: 2, Payload: "b-dup"},
		{ID: 2, Payload: "b-dup2"},
		{ID: 3, Payload: "c"},
	}
	fmt.Printf("in:  len=%d cap=%d\n", len(batch), cap(batch))

	out, err := dedupstream.Dedup(batch)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("out: len=%d\n", len(out))
	for _, e := range out {
		fmt.Printf("  id=%d payload=%s\n", e.ID, e.Payload)
	}

	// The freed tail was zeroed: slots past the new length are nil.
	full := batch[:cap(batch)]
	fmt.Printf("freed slot [%d]==nil: %v\n", len(out), full[len(out)] == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in:  len=6 cap=6
out: len=3
  id=1 payload=a
  id=2 payload=b
  id=3 payload=c
freed slot [3]==nil: true
```

Each id keeps its first event; the three trailing slots the duplicates occupied
are zeroed to nil, which is what lets the retired `*Event` values be collected.

### Tests

`TestDedupCollapsesRuns` covers multi-length duplicate runs; `TestDedupNoOpOnUnique`
proves unique input is unchanged; `TestDedupConsecutiveOnly` feeds an unsorted
slice and shows `Dedup` refuses with `ErrNotSorted` (and that a raw `CompactFunc`
on unsorted input keeps far-apart duplicates); `TestDedupZeroesTail` re-slices to
capacity and asserts the freed slots are nil; `TestDedupClipCapsCapacity` proves
`Clip` makes cap equal len.

Create `dedup_test.go`:

```go
package dedupstream

import (
	"errors"
	"slices"
	"testing"
)

func ids(s []*Event) []int {
	out := make([]int, len(s))
	for i, e := range s {
		out[i] = e.ID
	}
	return out
}

func TestDedupCollapsesRuns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   []int
		want []int
	}{
		{"single run", []int{1, 1, 1}, []int{1}},
		{"mixed runs", []int{1, 1, 2, 2, 2, 3}, []int{1, 2, 3}},
		{"no dupes", []int{1, 2, 3}, []int{1, 2, 3}},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := make([]*Event, len(tc.in))
			for i, id := range tc.in {
				s[i] = &Event{ID: id}
			}
			out, err := Dedup(s)
			if err != nil {
				t.Fatalf("Dedup returned error: %v", err)
			}
			if !slices.Equal(ids(out), tc.want) {
				t.Fatalf("Dedup ids = %v, want %v", ids(out), tc.want)
			}
		})
	}
}

func TestDedupNoOpOnUnique(t *testing.T) {
	t.Parallel()

	s := []*Event{{ID: 1}, {ID: 2}, {ID: 3}}
	out, err := Dedup(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("unique input changed length to %d", len(out))
	}
}

func TestDedupConsecutiveOnly(t *testing.T) {
	t.Parallel()

	// Unsorted: id 1 appears far apart. Dedup refuses the precondition.
	s := []*Event{{ID: 1}, {ID: 2}, {ID: 1}}
	if _, err := Dedup(s); !errors.Is(err, ErrNotSorted) {
		t.Fatalf("Dedup on unsorted input err = %v, want ErrNotSorted", err)
	}
	// Raw CompactFunc on the same unsorted slice keeps the far-apart duplicate,
	// proving Compact only collapses CONSECUTIVE equal runs.
	raw := slices.CompactFunc(slices.Clone(s), func(a, b *Event) bool { return a.ID == b.ID })
	if !slices.Equal(ids(raw), []int{1, 2, 1}) {
		t.Fatalf("CompactFunc on unsorted = %v, want [1 2 1]", ids(raw))
	}
}

func TestDedupZeroesTail(t *testing.T) {
	t.Parallel()

	s := []*Event{{ID: 1}, {ID: 1}, {ID: 2}, {ID: 2}}
	oldCap := cap(s)
	out, err := Dedup(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("len after dedup = %d, want 2", len(out))
	}
	// Re-slice the backing array to its full capacity and assert the freed
	// tail slots were zeroed to nil, releasing the retired *Event values.
	full := out[:oldCap]
	for i := len(out); i < oldCap; i++ {
		if full[i] != nil {
			t.Fatalf("freed slot [%d] = %v, want nil (not zeroed for GC)", i, full[i])
		}
	}
}

func TestDedupClipCapsCapacity(t *testing.T) {
	t.Parallel()

	s := []*Event{{ID: 1}, {ID: 1}, {ID: 2}}
	out, err := DedupClip(s)
	if err != nil {
		t.Fatal(err)
	}
	if cap(out) != len(out) {
		t.Fatalf("after Clip cap=%d len=%d, want equal", cap(out), len(out))
	}
}
```

## Review

The dedup is correct when it collapses exactly the consecutive duplicate-id runs
and leaves a slice whose ids are strictly increasing. The precondition guard is
what keeps it honest: `CompactFunc` on unsorted input produces a plausible but
half-deduplicated slice with no error, so `Dedup` checks `IsSortedFunc` first and
returns `ErrNotSorted`. The tail-zeroing test is the one that encodes the GC
contract — it re-slices past the new length and asserts nil, which fails if a
future change keeps a long header or copies into a fresh array without releasing
the old one. Reassigning the `CompactFunc` result is mandatory; drop the
assignment and you read into the zeroed tail. Run `go test -race` to confirm the
clone in the consecutive-only test isolates the two views.

## Resources

- [`slices.CompactFunc`](https://pkg.go.dev/slices#CompactFunc) — collapses consecutive equal runs and zeroes the freed tail.
- [`slices.Clip`](https://pkg.go.dev/slices#Clip) and [`slices.IsSortedFunc`](https://pkg.go.dev/slices#IsSortedFunc).
- [Go blog: the slices and maps packages](https://go.dev/blog/slices) — backing-array reuse and why the tail is zeroed.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-evict-expired-in-place-deletefunc.md](03-evict-expired-in-place-deletefunc.md)
