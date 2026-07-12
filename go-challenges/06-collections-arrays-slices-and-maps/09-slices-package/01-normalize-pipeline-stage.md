# Exercise 1: Normalize A Records Pipeline Stage (Clone + SortFunc + CompactFunc + Reverse)

A normalization stage sits between an ingest source and a store: it takes a batch
of records the caller still owns, produces a canonical ordering, collapses
same-key duplicates, and returns an independent copy so nothing downstream can
mutate the caller's input. This module builds exactly that stage out of
`slices.Clone`, `slices.SortFunc`, `slices.CompactFunc`, and `slices.Reverse`,
and proves the input-not-modified contract that makes the stage safe to drop into
a pipeline.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
normalize/                     module example.com/normalize
  go.mod                       go 1.24
  pipeline.go                  type Record; Process (Clone, SortFunc, CompactFunc, Reverse)
  cmd/
    demo/
      main.go                  runnable demo: normalize a small batch
  pipeline_test.go             sort/compact/tie-break/empty/independent-copy + BinarySearch consistency
```

- Files: `pipeline.go`, `cmd/demo/main.go`, `pipeline_test.go`.
- Implement: `Process([]Record) []Record` that clones the input, sorts by key (case-insensitive, then by Value as tiebreak), compacts consecutive same-key records, and reverses the result.
- Test: sort-by-key-case-insensitive, compact-consecutive-duplicates, tie-break-by-value, empty input, input-not-modified, and a BinarySearch-consistency test proving the sort order is compatible with a sorted-slice search.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/01-normalize-pipeline-stage/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/01-normalize-pipeline-stage
go mod edit -go=1.24
```

### Why clone first, and why the comparator is a total order

The stage's headline contract is that it does not touch the caller's slice.
`slices.SortFunc`, `slices.CompactFunc`, and `slices.Reverse` all operate on the
backing array in place — sort reorders it, compact shifts survivors down and
zeroes the tail, reverse swaps ends. If `Process` ran those on the argument
directly, the caller's `records` would come back reordered and truncated, an
action-at-a-distance bug that is miserable to trace. `slices.Clone` gives an
independent backing array up front, so every in-place operation lands on the copy
and the caller's slice is untouched. The final test pins this by mutating the
returned slice and asserting the input is unchanged.

The comparator sorts by key case-insensitively, and when two keys are equal
(ignoring case) it breaks the tie by `Value`. That tiebreak turns the comparator
into a *total order*: no two distinct records with the same key and different
values compare equal, so the ordering is deterministic. `cmp.Compare` supplies
the sign for each key; the pattern of "compare primary, and if zero compare
secondary" is the manual form of `cmp.Or` (a later module uses `cmp.Or` for the
same shape).

`CompactFunc` then collapses *consecutive* records the equality function calls
equal — here, same key ignoring case. Because the slice was just sorted by key,
all same-key records are adjacent, so a single `CompactFunc` pass de-duplicates
by key globally. The record kept from each run is the first one, which — thanks
to the value tiebreak in the sort — is the one with the smallest `Value`. Finally
`Reverse` flips the ascending key order into descending. `CompactFunc` returns a
shortened slice header, so its result is reassigned to `out`; `Reverse` mutates
in place and returns nothing.

Create `pipeline.go`:

```go
package pipeline

import (
	"cmp"
	"slices"
	"strings"
)

// Record is one ingested item keyed by a case-insensitive Key.
type Record struct {
	Key   string
	Value int
}

// Process normalizes a batch without mutating the caller's slice: it clones the
// input, sorts by key (case-insensitive) then by Value, collapses consecutive
// same-key records keeping the smallest Value, and reverses into descending key
// order. The returned slice has an independent backing array.
func Process(records []Record) []Record {
	out := slices.Clone(records)
	slices.SortFunc(out, func(a, b Record) int {
		if c := cmp.Compare(strings.ToLower(a.Key), strings.ToLower(b.Key)); c != 0 {
			return c
		}
		return cmp.Compare(a.Value, b.Value)
	})
	out = slices.CompactFunc(out, func(a, b Record) bool {
		return strings.EqualFold(a.Key, b.Key)
	})
	slices.Reverse(out)
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/normalize"
)

func main() {
	in := []pipeline.Record{
		{Key: "banana", Value: 1},
		{Key: "Apple", Value: 9},
		{Key: "apple", Value: 2},
		{Key: "cherry", Value: 3},
	}
	out := pipeline.Process(in)
	for _, r := range out {
		fmt.Printf("%s=%d\n", r.Key, r.Value)
	}
	fmt.Printf("input[0] still %s\n", in[0].Key)
}
```

Note the demo's package alias: the module is `example.com/normalize` but the
package inside is `pipeline`, so the import path ends in `normalize` while the
identifier is `pipeline`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cherry=3
banana=1
apple=2
input[0] still banana
```

The two `apple`/`Apple` records collapse to the one with the smaller Value (2),
keyed `apple` because that was first after the ascending sort; the order is
descending by key, so `cherry`, `banana`, `apple`. The input's first element is
still `banana`, proving `Process` did not reorder the caller's slice.

### Tests

The five original cases are preserved and one is added. `TestProcessSortsByKeyCaseInsensitive`
proves ordering ignores case; `TestProcessCompactsConsecutiveDuplicates` is the
core case (sort, compact ignoring case, reverse); `TestProcessSortsTieByValue`
pins the value tiebreak; `TestProcessHandlesEmptyInput` covers nil; and
`TestProcessReturnsIndependentCopy` proves the caller's slice is untouched. The
added `TestProcessOrderIsBinarySearchConsistent` proves the emitted order (after
re-sorting ascending) is compatible with `slices.BinarySearchFunc` using the same
case-insensitive key comparator — the invariant that a downstream index build
relies on.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"cmp"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProcessSortsByKeyCaseInsensitive(t *testing.T) {
	t.Parallel()

	in := []Record{
		{Key: "banana", Value: 1},
		{Key: "Apple", Value: 2},
		{Key: "cherry", Value: 3},
	}
	got := Process(in)
	want := []Record{
		{Key: "cherry", Value: 3},
		{Key: "banana", Value: 1},
		{Key: "Apple", Value: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Process() = %+v, want %+v", got, want)
	}
}

func TestProcessCompactsConsecutiveDuplicates(t *testing.T) {
	t.Parallel()

	in := []Record{
		{Key: "a", Value: 1},
		{Key: "a", Value: 2},
		{Key: "b", Value: 3},
		{Key: "B", Value: 4},
	}
	got := Process(in)
	want := []Record{
		{Key: "b", Value: 3},
		{Key: "a", Value: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Process() = %+v, want %+v", got, want)
	}
}

func TestProcessSortsTieByValue(t *testing.T) {
	t.Parallel()

	in := []Record{
		{Key: "a", Value: 3},
		{Key: "a", Value: 1},
		{Key: "a", Value: 2},
	}
	got := Process(in)
	want := []Record{{Key: "a", Value: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Process() = %+v, want %+v", got, want)
	}
}

func TestProcessHandlesEmptyInput(t *testing.T) {
	t.Parallel()

	got := Process(nil)
	if len(got) != 0 {
		t.Fatalf("Process(nil) = %+v, want empty", got)
	}
}

func TestProcessReturnsIndependentCopy(t *testing.T) {
	t.Parallel()

	in := []Record{{Key: "z", Value: 1}, {Key: "a", Value: 2}}
	got := Process(in)
	got[0].Key = "modified"
	if in[0].Key != "z" {
		t.Fatalf("input was modified: %+v", in)
	}
}

func TestProcessOrderIsBinarySearchConsistent(t *testing.T) {
	t.Parallel()

	byKey := func(a, b Record) int {
		return cmp.Compare(strings.ToLower(a.Key), strings.ToLower(b.Key))
	}
	in := []Record{
		{Key: "banana", Value: 1},
		{Key: "Apple", Value: 2},
		{Key: "cherry", Value: 3},
	}
	// Process emits descending key order; re-sort ascending with the SAME key
	// comparator used inside Process, then binary-search must find every key.
	asc := Process(in)
	slices.SortFunc(asc, byKey)
	if !slices.IsSortedFunc(asc, byKey) {
		t.Fatalf("re-sorted slice is not sorted by the search comparator: %+v", asc)
	}
	for _, want := range []string{"apple", "banana", "cherry"} {
		i, found := slices.BinarySearchFunc(asc, Record{Key: want}, byKey)
		if !found {
			t.Fatalf("BinarySearchFunc(%q) not found; order incompatible with sort", want)
		}
		if !strings.EqualFold(asc[i].Key, want) {
			t.Fatalf("BinarySearchFunc(%q) = index %d holding %q", want, i, asc[i].Key)
		}
	}
	if _, found := slices.BinarySearchFunc(asc, Record{Key: "durian"}, byKey); found {
		t.Fatal("BinarySearchFunc reported absent key durian as found")
	}
}
```

## Review

`Process` is correct when its output is a pure function of the input contents and
never a function of the input's identity: the caller's slice must read the same
before and after. The clone is what guarantees that, and `TestProcessReturnsIndependentCopy`
is the test that would catch a regression where someone "optimized away" the
clone. The compaction keeping the smallest `Value` per key is a consequence of
the value tiebreak in the sort, not a separate step — remove the tiebreak and the
surviving record per run becomes non-deterministic. The BinarySearch-consistency
test encodes the real reason ordering matters downstream: if the sort order and a
later search's comparator disagree, present keys go missing with no error. Run
`go test -race` to confirm nothing shares state across the parallel cases.

## Resources

- [`slices` package](https://pkg.go.dev/slices) — `Clone`, `SortFunc`, `CompactFunc`, `Reverse`, `BinarySearchFunc`, `IsSortedFunc`.
- [`cmp` package](https://pkg.go.dev/cmp) — `Compare` and the total-order contract.
- [Go blog: the slices and maps packages](https://go.dev/blog/slices) — why the package exists and how it treats backing arrays.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-dedup-sorted-stream-compact-tail.md](02-dedup-sorted-stream-compact-tail.md)
