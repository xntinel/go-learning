# Exercise 8: Merge Paginated Shard Results In A Fan-In (Concat + Sorted / Values)

A query that fans out across shards (partitions, replicas, regions) returns one
result slice per shard, and the aggregator must merge them into a single
globally-ordered, de-duplicated view. This module builds that fan-in merge:
`slices.Concat` joins the shard outputs into one slice without aliasing any of
them, then `slices.SortedFunc` over a `slices.Values` iterator produces the ordered
result, and `CompactFunc` drops cross-shard duplicates.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
shardmerge/                    module example.com/shardmerge
  go.mod                       go 1.24
  merge.go                     type Hit; Merge (Concat + SortedFunc + CompactFunc)
  cmd/
    demo/
      main.go                  runnable demo: merge three shard result sets
  merge_test.go                all elements sorted, empties, de-dup overlap, no aliasing, SortedFunc==reference
```

- Files: `merge.go`, `cmd/demo/main.go`, `merge_test.go`.
- Implement: `Merge(shards ...[]Hit) []Hit` using `slices.Concat` to join shards, `slices.SortedFunc` over `slices.Values` to order by id, and `slices.CompactFunc` to de-duplicate equal ids.
- Test: several shards yield all elements sorted; empty and all-empty produce nil per `Concat` semantics; overlapping ids across shards are de-duplicated; `Concat` does not alias any input shard; `SortedFunc` output equals a `SortFunc`+sort reference.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/08-shard-fanin-merge-concat-sorted/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/08-shard-fanin-merge-concat-sorted
go mod edit -go=1.24
```

### Concat allocates fresh; SortedFunc orders an iterator

`slices.Concat(s1, s2, ...)` returns a new slice that is the concatenation of its
arguments. Two properties make it the right join for a fan-in. First, it always
allocates a fresh backing array (it sizes the total, then appends each shard into
it), so the result does not alias any input shard — mutating the merged slice
leaves every shard intact. This matters when shards are reused (a retry, a cache)
after the merge. Second, `Concat` returns nil when the total length is zero: an
all-empty fan-in produces a nil slice, not a non-nil empty one. The test pins both.

Once concatenated, the merge orders by id. `slices.Values(s)` turns the joined
slice into an `iter.Seq[Hit]`, and `slices.SortedFunc(seq, cmp)` collects that
iterator into a new sorted slice in one call. It is equivalent to
`SortFunc(Clone(s), cmp)` but reads as a pipeline: values in, sorted slice out. The
test asserts the two are identical so the iterator form is not hiding a difference.

De-duplication comes last. Because the slice is now sorted by id, equal ids are
adjacent, so a single `slices.CompactFunc` keyed on id collapses cross-shard
duplicates (the same hit returned by two overlapping shards). The order is
deliberate: concat, then sort, then compact — compacting before sorting would only
catch duplicates that happened to be adjacent by accident.

Create `merge.go`:

```go
package shardmerge

import (
	"cmp"
	"slices"
)

// Hit is one search/query result keyed by ID.
type Hit struct {
	ID    int
	Shard string
}

// Merge joins per-shard results into one globally id-ordered, de-duplicated
// slice. Concat allocates a fresh array (no shard is aliased); SortedFunc orders
// the joined values; CompactFunc drops equal-id duplicates from overlapping
// shards. An all-empty fan-in returns nil, per Concat.
func Merge(shards ...[]Hit) []Hit {
	joined := slices.Concat(shards...)
	byID := func(a, b Hit) int { return cmp.Compare(a.ID, b.ID) }
	sorted := slices.SortedFunc(slices.Values(joined), byID)
	return slices.CompactFunc(sorted, func(a, b Hit) bool { return a.ID == b.ID })
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/shardmerge"
)

func main() {
	shardA := []shardmerge.Hit{{ID: 5, Shard: "A"}, {ID: 1, Shard: "A"}}
	shardB := []shardmerge.Hit{{ID: 3, Shard: "B"}, {ID: 5, Shard: "B"}} // 5 overlaps A
	shardC := []shardmerge.Hit{{ID: 2, Shard: "C"}}

	merged := shardmerge.Merge(shardA, shardB, shardC)
	for _, h := range merged {
		fmt.Printf("id=%d from=%s\n", h.ID, h.Shard)
	}
	fmt.Printf("empty merge is nil: %v\n", shardmerge.Merge() == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=1 from=A
id=2 from=C
id=3 from=B
id=5 from=A
empty merge is nil: true
```

Ids 1, 2, 3, 5 come out sorted; the duplicate id 5 from shards A and B collapses to
one; an empty fan-in is nil. Which of the two equal-id hits survives (its `Shard`
label) is not guaranteed by `SortedFunc` — the function only contracts the id
ordering, not the relative order of elements that compare equal — so the tests
assert on ids alone and never on the surviving `Shard`. If your merge must keep a
specific record among duplicates (say, the freshest, or a preferred shard), encode
that as a tie-breaker in the comparator so the desired element sorts first, rather
than relying on the sort landing a particular one adjacent.

### Tests

`TestMergeSortsAll` merges several shards and asserts the ids are sorted and
complete. `TestMergeEmpties` covers empty and all-empty inputs, pinning the nil
result. `TestMergeDeduplicates` proves overlapping ids collapse. `TestConcatNoAlias`
mutates the merged slice and asserts the shards are untouched.
`TestSortedFuncMatchesReference` proves `SortedFunc` equals a `SortFunc` reference.

Create `merge_test.go`:

```go
package shardmerge

import (
	"cmp"
	"slices"
	"testing"
)

func mergedIDs(hits []Hit) []int {
	out := make([]int, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestMergeSortsAll(t *testing.T) {
	t.Parallel()

	got := Merge(
		[]Hit{{ID: 9}, {ID: 2}},
		[]Hit{{ID: 4}},
		[]Hit{{ID: 7}, {ID: 1}},
	)
	if !slices.Equal(mergedIDs(got), []int{1, 2, 4, 7, 9}) {
		t.Fatalf("merged ids = %v, want [1 2 4 7 9]", mergedIDs(got))
	}
}

func TestMergeEmpties(t *testing.T) {
	t.Parallel()

	if got := Merge(); got != nil {
		t.Fatalf("Merge() = %v, want nil", got)
	}
	if got := Merge(nil, []Hit{}, nil); got != nil {
		t.Fatalf("Merge(all empty) = %v, want nil", got)
	}
	// One non-empty shard among empties still merges.
	got := Merge(nil, []Hit{{ID: 3}}, nil)
	if !slices.Equal(mergedIDs(got), []int{3}) {
		t.Fatalf("Merge with one shard = %v, want [3]", mergedIDs(got))
	}
}

func TestMergeDeduplicates(t *testing.T) {
	t.Parallel()

	got := Merge(
		[]Hit{{ID: 1, Shard: "A"}, {ID: 2, Shard: "A"}},
		[]Hit{{ID: 2, Shard: "B"}, {ID: 3, Shard: "B"}},
	)
	if !slices.Equal(mergedIDs(got), []int{1, 2, 3}) {
		t.Fatalf("de-duplicated ids = %v, want [1 2 3]", mergedIDs(got))
	}
}

func TestConcatNoAlias(t *testing.T) {
	t.Parallel()

	shard := []Hit{{ID: 1, Shard: "A"}}
	merged := Merge(shard, []Hit{{ID: 2, Shard: "B"}})
	// Mutating the merged result must not reach back into the shard.
	merged[0] = Hit{ID: 999, Shard: "X"}
	if shard[0].ID != 1 || shard[0].Shard != "A" {
		t.Fatalf("shard aliased by Merge: %+v", shard[0])
	}
}

func TestSortedFuncMatchesReference(t *testing.T) {
	t.Parallel()

	joined := slices.Concat(
		[]Hit{{ID: 3}, {ID: 1}},
		[]Hit{{ID: 2}},
	)
	byID := func(a, b Hit) int { return cmp.Compare(a.ID, b.ID) }

	fromIter := slices.SortedFunc(slices.Values(joined), byID)
	reference := slices.Clone(joined)
	slices.SortFunc(reference, byID)

	if !slices.Equal(mergedIDs(fromIter), mergedIDs(reference)) {
		t.Fatalf("SortedFunc %v != SortFunc reference %v", mergedIDs(fromIter), mergedIDs(reference))
	}
}
```

## Review

The merge is correct when the result contains every distinct id across all shards,
sorted, with cross-shard duplicates collapsed, and when it shares no backing array
with any shard. `Concat`'s fresh allocation is what guarantees the no-alias
property — the test mutates the merged slice and checks the shard is intact. The
nil-on-empty behavior is `Concat`'s documented semantics, worth pinning because a
caller that marshals the result cares whether it gets `null` or `[]`. The
concat-sort-compact order is load-bearing: compacting before sorting would miss
non-adjacent duplicates. Run `go test -race`; the merge is pure over its inputs, so
the parallel cases do not share state.

## Resources

- [`slices.Concat`](https://pkg.go.dev/slices#Concat) — fresh-array join, nil on empty.
- [`slices.SortedFunc`](https://pkg.go.dev/slices#SortedFunc) and [`slices.Values`](https://pkg.go.dev/slices#Values) — iterator-to-sorted-slice.
- [Go blog: the slices and maps packages](https://go.dev/blog/slices) — Concat and iterator helpers.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-safe-subslice-clip-grow.md](09-safe-subslice-clip-grow.md)
