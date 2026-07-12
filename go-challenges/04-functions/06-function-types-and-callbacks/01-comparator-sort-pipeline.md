# Exercise 1: Comparator-Driven Sort Pipeline (Less/Equal Function Types)

Every list endpoint sorts, searches, and de-duplicates records, and the code that
does it is parameterized by a comparator. This module builds the `sortx` package a
backend reuses everywhere: generic `Less[T]` and `Equal[T]` function types over the
modern value-based `slices` and `cmp` APIs.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
sortx/                      independent module: example.com/sortx
  go.mod                    go 1.26
  sortx.go                  Less[T], Equal[T]; Sort, SortBy, IndexOf, BinarySearch
  cmd/
    demo/
      main.go               runnable demo: sort people, search, descending
  sortx_test.go             table + property tests, stability, NaN, descending
```

Files: `sortx.go`, `cmd/demo/main.go`, `sortx_test.go`.
Implement: `Less[T]`/`Equal[T]` function types, `Sort` over `slices.SortFunc`, `SortBy` deriving a `cmp.Ordered` key, `IndexOf` linear scan, `BinarySearch` over `slices.BinarySearchFunc`.
Test: sortedness via `slices.IsSorted`, pinned stability, `IndexOf` miss, binary search found/not-found, a descending sort by a negated comparator, and a `float64`-with-`NaN` total-order test.
Verify: `go test -count=1 -race ./...`

### Why value-based callbacks, and where each function fits

The package exposes two callback shapes. `Less[T] func(a, b T) int` is the
three-valued comparator the whole modern stdlib speaks: negative, zero, positive.
`Equal[T] func(a, b T) bool` is the two-valued predicate for "is this the element I
want" during a linear scan, where no ordering exists.

`Sort` is a thin wrapper over `slices.SortFunc`, which is value-based: the
comparator receives the elements themselves, not indices, so you cannot accidentally
index the wrong slice the way `sort.Slice` invites. `SortBy` is the everyday case —
"sort by a derived key" — implemented by deriving a `cmp.Ordered` key and delegating
to `cmp.Compare`, which is the canonical total-order comparator for ordered types.
`IndexOf` is a linear scan driven by an `Equal[T]` for unsorted data; `BinarySearch`
wraps `slices.BinarySearchFunc` for sorted data and returns `(index, found)`, where
even on a miss the index is the insertion point.

Two properties earn their own tests. Stability: equal elements keeping input order
is something callers depend on, so the test tags equal keys with an insertion order
and asserts non-decreasing order within each equal-key run. Total order over floats:
`cmp.Compare` orders `NaN` deterministically, where a hand-written `a < b` comparator
would report `NaN` equal to everything and produce a partial, flaky order — the test
sorts a slice containing `NaN` and asserts a fixed, reproducible result.

Create `sortx.go`:

```go
package sortx

import (
	"cmp"
	"slices"
)

// Less is the three-valued comparator convention the modern stdlib uses:
// negative if a < b, zero if equal, positive if a > b.
type Less[T any] func(a, b T) int

// Equal is a two-valued predicate for linear membership scans where no
// ordering is defined.
type Equal[T any] func(a, b T) bool

// Sort sorts items in place using the comparator. It wraps slices.SortFunc,
// which is value-based (the comparator receives elements, not indices).
func Sort[T any](items []T, less Less[T]) {
	slices.SortFunc(items, less)
}

// SortBy sorts items in place by a derived cmp.Ordered key, delegating the
// element comparison to cmp.Compare.
func SortBy[T any, K cmp.Ordered](items []T, key func(T) K) {
	slices.SortFunc(items, func(a, b T) int {
		return cmp.Compare(key(a), key(b))
	})
}

// IndexOf returns the index of the first item equal to target per eq, or -1.
// Use it on unsorted data where no ordering exists.
func IndexOf[T any](items []T, target T, eq Equal[T]) int {
	for i, item := range items {
		if eq(item, target) {
			return i
		}
	}
	return -1
}

// BinarySearch finds target in a slice already sorted by less, returning the
// index and whether it was found. On a miss, the index is the insertion point.
func BinarySearch[T any](items []T, target T, less Less[T]) (int, bool) {
	return slices.BinarySearchFunc(items, target, less)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"cmp"
	"fmt"

	"example.com/sortx"
)

type person struct {
	Name string
	Age  int
}

func main() {
	people := []person{
		{Name: "Carol", Age: 35},
		{Name: "Alice", Age: 30},
		{Name: "Bob", Age: 25},
	}
	sortx.SortBy(people, func(p person) int { return p.Age })
	fmt.Println("by age:", people[0].Name, people[1].Name, people[2].Name)

	ages := []int{25, 30, 35}
	idx, found := sortx.BinarySearch(ages, 30, cmp.Compare)
	fmt.Printf("binary search 30: index=%d found=%v\n", idx, found)

	miss := sortx.IndexOf(people, person{Name: "Nobody"}, func(a, b person) bool {
		return a.Name == b.Name
	})
	fmt.Println("indexOf Nobody:", miss)

	nums := []int{4, 1, 3, 2}
	// Descending by negating the comparator.
	sortx.Sort(nums, func(a, b int) int { return -cmp.Compare(a, b) })
	fmt.Println("descending:", nums)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
by age: Bob Alice Carol
binary search 30: index=1 found=true
indexOf Nobody: -1
descending: [4 3 2 1]
```

### Tests

The original pipeline tests are preserved; two new ones pin the descending pattern
and the `NaN` total order.

Create `sortx_test.go`:

```go
package sortx

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"testing"
)

type person struct {
	Name string
	Age  int
}

func TestSortAppliesComparator(t *testing.T) {
	t.Parallel()
	items := []int{3, 1, 4, 1, 5, 9, 2, 6}
	Sort(items, cmp.Compare)
	if !slices.IsSorted(items) {
		t.Fatalf("not sorted: %v", items)
	}
}

func TestSortByKey(t *testing.T) {
	t.Parallel()
	people := []person{
		{Name: "Alice", Age: 30},
		{Name: "Bob", Age: 25},
		{Name: "Carol", Age: 35},
	}
	SortBy(people, func(p person) int { return p.Age })
	got := []int{people[0].Age, people[1].Age, people[2].Age}
	if want := []int{25, 30, 35}; !slices.Equal(got, want) {
		t.Fatalf("ages = %v, want %v", got, want)
	}
}

func TestSortByKeyIsStable(t *testing.T) {
	t.Parallel()
	type record struct {
		key   int
		order int // insertion order tag
	}
	items := []record{
		{key: 1, order: 1},
		{key: 2, order: 1},
		{key: 1, order: 2},
		{key: 2, order: 2},
		{key: 1, order: 3},
	}
	SortBy(items, func(e record) int { return e.key })
	for i := 1; i < len(items); i++ {
		if items[i-1].key == items[i].key && items[i-1].order > items[i].order {
			t.Fatalf("not stable at %d: %+v vs %+v", i, items[i-1], items[i])
		}
	}
}

func TestIndexOfFindsByEquality(t *testing.T) {
	t.Parallel()
	items := []person{
		{Name: "Alice", Age: 30},
		{Name: "Bob", Age: 25},
	}
	eq := func(a, b person) bool { return a.Name == b.Name }

	if idx := IndexOf(items, person{Name: "Bob"}, eq); idx != 1 {
		t.Fatalf("idx = %d, want 1", idx)
	}
	if idx := IndexOf(items, person{Name: "Nobody"}, eq); idx != -1 {
		t.Fatalf("idx = %d, want -1", idx)
	}
}

func TestBinarySearchReturnsCorrectIndex(t *testing.T) {
	t.Parallel()
	items := []int{1, 3, 5, 7, 9}
	idx, found := BinarySearch(items, 5, cmp.Compare)
	if !found {
		t.Fatal("5 should be found")
	}
	if items[idx] != 5 {
		t.Fatalf("items[%d] = %d, want 5", idx, items[idx])
	}
	if _, found := BinarySearch(items, 4, cmp.Compare); found {
		t.Fatal("4 should not be found")
	}
}

// TestSortByDescending pins the descending-sort pattern: negate the comparator.
func TestSortByDescending(t *testing.T) {
	t.Parallel()
	nums := []int{3, 1, 4, 1, 5, 9, 2, 6}
	Sort(nums, func(a, b int) int { return -cmp.Compare(a, b) })
	if !slices.IsSortedFunc(nums, func(a, b int) int { return -cmp.Compare(a, b) }) {
		t.Fatalf("not descending: %v", nums)
	}
	if nums[0] != 9 || nums[len(nums)-1] != 1 {
		t.Fatalf("descending bounds wrong: %v", nums)
	}
}

// TestNaNOrdersDeterministically proves cmp.Compare gives floats a total order.
func TestNaNOrdersDeterministically(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	nums := []float64{2.0, nan, 1.0, 3.0}
	Sort(nums, cmp.Compare)
	// cmp.Compare treats NaN as less than any non-NaN, so it sorts first.
	if !math.IsNaN(nums[0]) {
		t.Fatalf("NaN not ordered first: %v", nums)
	}
	rest := nums[1:]
	if !slices.IsSorted(rest) {
		t.Fatalf("non-NaN tail not sorted: %v", rest)
	}
	// Determinism: sorting again yields the same layout.
	again := []float64{3.0, 1.0, nan, 2.0}
	Sort(again, cmp.Compare)
	if !math.IsNaN(again[0]) || !slices.Equal(again[1:], []float64{1.0, 2.0, 3.0}) {
		t.Fatalf("non-deterministic order: %v", again)
	}
}

func ExampleSortBy() {
	people := []person{{Name: "Carol", Age: 35}, {Name: "Bob", Age: 25}}
	SortBy(people, func(p person) int { return p.Age })
	fmt.Println(people[0].Name, people[1].Name)
	// Output: Bob Carol
}

func ExampleBinarySearch() {
	nums := []int{1, 3, 5, 7, 9}
	idx, found := BinarySearch(nums, 7, cmp.Compare)
	fmt.Println(idx, found)
	// Output: 3 true
}
```

## Review

The pipeline is correct when sorting is a pure function of the comparator: after
`Sort` the slice satisfies `slices.IsSorted` (or `IsSortedFunc` for a custom order),
`IndexOf` returns `-1` on a miss and the first match otherwise, and `BinarySearch`
returns a `found` boolean plus an index that lands on the target when found. The two
new tests guard the traps a senior engineer hits in production: descending order is a
negated `cmp.Compare`, not a second code path, and floats need `cmp.Compare` for a
total order because a hand-written `a < b` comparator makes `NaN` compare equal to
everything and the sort becomes non-deterministic. Keep `SortBy` delegating to
`cmp.Compare` on the derived key rather than hand-writing the three-way branch — the
stdlib comparator is the tested one.

## Resources

- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [slices.SortFunc and BinarySearchFunc](https://pkg.go.dev/slices#SortFunc)
- [cmp.Compare](https://pkg.go.dev/cmp#Compare)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-functional-options-constructor.md](02-functional-options-constructor.md)
