# Exercise 1: A Sorted-Slice Index of Entity IDs (Add/Has/Range/Size)

Many repositories keep a small in-memory index of the primary keys they hold —
to answer "do we have this ID" without a round trip, and to scan a contiguous ID
window. A sorted `[]int` is the leanest structure that does both. This exercise
builds that index: `Add` inserts in sorted position with set-style dedup, `Has`
is O(log n) membership, `Range(lo, hi)` returns a fresh copy of the half-open
`[lo, hi)` window, and `Size` reports cardinality.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
entityindex/                 independent module: example.com/entityindex
  go.mod
  index.go                   type Index; New, Add, Has, Range, Size
  cmd/
    demo/
      main.go                shuffled inserts, membership, a range scan
  index_test.go              sorted invariant, dedup, membership, range, copy proof
```

Files: `index.go`, `cmd/demo/main.go`, `index_test.go`.
Implement: `Index` over a sorted `[]int` with `New`, `Add(id)`, `Has(id) bool`, `Range(lo, hi) []int`, `Size() int`.
Test: sorted invariant after shuffled inserts, duplicate rejection, membership true/false, range-all-in-range, empty range, empty table, single-element range, and a proof that `Range` returns a copy.
Verify: `go test -count=1 -race ./...`

### Why insertion is O(n) and Range must copy

`Add` finds the insertion point with `sort.SearchInts`, which returns the first
index `i` with `data[i] >= id`. If the element at `i` already equals `id`, the
index is a set and the insert is a no-op — that is the dedup. Otherwise we open a
one-slot gap at `i` by appending a zero and sliding the tail up with `copy`, then
drop `id` into the gap. The search is O(log n); the `copy` shift is O(n) and
dominates. That is the price of keeping order: you pay a linear shift on every
insert to buy O(log n) membership and cheap range scans forever after.

`Range(lo, hi)` is two boundary searches. `sort.SearchInts(data, lo)` is the
lower bound: the first index whose value is `>= lo`. The upper bound is the first
index whose value is `>= hi`, which we express with `sort.Search` and the
predicate `data[idx] >= hi`. The window is `data[i:j]`; because the interval is
half-open, `hi` is excluded and the count is exactly `j - i`. When `i >= j` the
window is empty and we return an empty slice.

The single most important detail is that `Range` returns a *copy*, not
`data[i:j]`. Returning the sub-slice would alias the index's backing array: the
caller could write through the window and corrupt the index, and a later `Add`
that shifts the tail could overwrite the caller's view mid-flight. We sever the
aliasing with `append([]int(nil), data[i:j]...)`, which allocates a fresh backing
array the caller owns outright.

Create `index.go`:

```go
package entityindex

import "sort"

// Index is an in-memory set of entity IDs kept sorted so membership is
// O(log n) and contiguous ID windows can be scanned. It is a set: Add ignores
// an ID that is already present.
type Index struct {
	data []int
}

// New returns an empty index.
func New() *Index {
	return &Index{}
}

// Add inserts id in sorted position. If id is already present it is a no-op,
// so the index behaves as a set. Insertion is O(n): O(log n) to locate the
// position, O(n) to shift the tail.
func (x *Index) Add(id int) {
	i := sort.SearchInts(x.data, id)
	if i < len(x.data) && x.data[i] == id {
		return
	}
	x.data = append(x.data, 0)
	copy(x.data[i+1:], x.data[i:])
	x.data[i] = id
}

// Has reports whether id is in the index in O(log n).
func (x *Index) Has(id int) bool {
	i := sort.SearchInts(x.data, id)
	return i < len(x.data) && x.data[i] == id
}

// Range returns a fresh copy of every ID in the half-open interval [lo, hi).
// The result never aliases the index's storage.
func (x *Index) Range(lo, hi int) []int {
	i := sort.SearchInts(x.data, lo)
	j := sort.Search(len(x.data), func(idx int) bool {
		return x.data[idx] >= hi
	})
	if i >= j {
		return []int{}
	}
	return append([]int(nil), x.data[i:j]...)
}

// Size reports the number of IDs in the index.
func (x *Index) Size() int {
	return len(x.data)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/entityindex"
)

func main() {
	x := entityindex.New()
	for _, id := range []int{5, 3, 8, 1, 4, 7, 2, 6} {
		x.Add(id)
	}
	x.Add(5) // duplicate: ignored

	fmt.Println("size:", x.Size())
	fmt.Println("has 4:", x.Has(4))
	fmt.Println("has 9:", x.Has(9))
	fmt.Println("range [3,7):", x.Range(3, 7))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
size: 8
has 4: true
has 9: false
range [3,7): [3 4 5 6]
```

### Tests

The suite pins each part of the contract. `TestAddKeepsIndexSorted` feeds a
shuffled sequence and asserts the backing slice comes out sorted.
`TestAddRejectsDuplicates` proves set semantics. The membership tests cover
present and missing. `TestRangeReturnsAllInRange`, `TestRangeEmptyForNoMatches`,
`TestRangeEmptyForEmptyTable`, and `TestRangeWithSingleElement` pin the half-open
window. `TestRangeReturnsCopy` mutates the returned slice and asserts the index
is unchanged — the aliasing proof.

Create `index_test.go`:

```go
package entityindex

import (
	"fmt"
	"reflect"
	"testing"
)

func TestAddKeepsIndexSorted(t *testing.T) {
	t.Parallel()

	x := New()
	for _, id := range []int{5, 3, 8, 1, 4, 7, 2, 6} {
		x.Add(id)
	}
	want := []int{1, 2, 3, 4, 5, 6, 7, 8}
	if !reflect.DeepEqual(x.data, want) {
		t.Fatalf("data = %v, want %v", x.data, want)
	}
}

func TestAddRejectsDuplicates(t *testing.T) {
	t.Parallel()

	x := New()
	x.Add(5)
	x.Add(3)
	x.Add(5)
	if x.Size() != 2 {
		t.Fatalf("Size = %d, want 2", x.Size())
	}
}

func TestHasReturnsTrueForPresent(t *testing.T) {
	t.Parallel()

	x := New()
	x.Add(5)
	if !x.Has(5) {
		t.Fatal("Has(5) should be true")
	}
}

func TestHasReturnsFalseForMissing(t *testing.T) {
	t.Parallel()

	x := New()
	if x.Has(5) {
		t.Fatal("Has(5) on empty index should be false")
	}
}

func TestRangeReturnsAllInRange(t *testing.T) {
	t.Parallel()

	x := New()
	for _, id := range []int{1, 3, 5, 7, 9} {
		x.Add(id)
	}
	got := x.Range(3, 8)
	want := []int{3, 5, 7}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Range(3, 8) = %v, want %v", got, want)
	}
}

func TestRangeEmptyForNoMatches(t *testing.T) {
	t.Parallel()

	x := New()
	for _, id := range []int{1, 3, 5} {
		x.Add(id)
	}
	got := x.Range(10, 20)
	if len(got) != 0 {
		t.Fatalf("Range(10, 20) = %v, want empty", got)
	}
}

func TestRangeEmptyForEmptyTable(t *testing.T) {
	t.Parallel()

	got := New().Range(0, 100)
	if len(got) != 0 {
		t.Fatalf("Range(0, 100) on empty index = %v, want empty", got)
	}
}

func TestRangeWithSingleElement(t *testing.T) {
	t.Parallel()

	x := New()
	for _, id := range []int{1, 5, 9} {
		x.Add(id)
	}
	got := x.Range(5, 6)
	want := []int{5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Range(5, 6) = %v, want %v", got, want)
	}
}

func TestRangeReturnsCopy(t *testing.T) {
	t.Parallel()

	x := New()
	for _, id := range []int{1, 2, 3, 4, 5} {
		x.Add(id)
	}
	got := x.Range(1, 6)
	got[0] = 999 // mutate the returned window

	if x.Has(999) {
		t.Fatal("mutating Range result corrupted the index: 999 leaked in")
	}
	if !x.Has(1) {
		t.Fatal("mutating Range result destroyed the original element 1")
	}
}

func Example() {
	x := New()
	for _, id := range []int{30, 10, 20} {
		x.Add(id)
	}
	fmt.Println(x.Range(10, 30))
	// Output: [10 20]
}
```

## Review

The index is correct when three properties hold together. The backing slice is
sorted after any sequence of `Add`s — check with the shuffled-insert test. `Add`
is idempotent per ID, so `Size` counts distinct IDs — the duplicate test proves
it. And `Range(lo, hi)` returns exactly the IDs in `[lo, hi)` as an
independently-owned slice — the single-element test pins the half-open boundary,
and the copy test proves no aliasing. The most common regressions are switching
the upper-bound predicate to `> hi` (which wrongly includes `hi`) and returning
`data[i:j]` directly (which leaks internal storage). Run `go test -race` to
confirm the whole suite.

## Resources

- [`sort` package](https://pkg.go.dev/sort) — `sort.Search`, `sort.SearchInts` and the monotone-predicate contract.
- [`slices` package](https://pkg.go.dev/slices) — the generic successors used in later exercises.
- [Go Wiki: SliceTricks](https://go.dev/wiki/SliceTricks) — the insert and copy idioms behind `Add` and `Range`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-generic-sorted-set.md](02-generic-sorted-set.md)
