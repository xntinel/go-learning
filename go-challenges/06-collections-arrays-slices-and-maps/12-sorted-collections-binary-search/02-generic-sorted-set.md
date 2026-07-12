# Exercise 2: A Generic Ordered Set with slices.BinarySearch and slices.Insert

The int index from Exercise 1 hard-codes `int` and reaches for the old
`sort.Search` API. Real code holds tenant IDs (strings), account numbers, region
codes — anything ordered. This exercise generalizes the index into a
`Set[T cmp.Ordered]` built entirely on the modern generic API:
`slices.BinarySearch` to locate-and-detect in one call, `slices.Insert` to splice
in the new element, `slices.Clone` for the defensive copy.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
orderedset/                  independent module: example.com/orderedset
  go.mod
  set.go                     type Set[T cmp.Ordered]; New, Add, Contains, Items, Len
  cmd/
    demo/
      main.go                a string set of tenant IDs, dedup and membership
  set_test.go                table-driven for string and int, clone proof, permutation property
```

Files: `set.go`, `cmd/demo/main.go`, `set_test.go`.
Implement: `Set[T cmp.Ordered]` with `New`, `Add(v) bool`, `Contains(v) bool`, `Items() []T`, `Len() int`.
Test: sorted invariant via `slices.IsSorted`, dedup, membership, `Items` is a clone, and a shuffled-permutation property test.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/02-generic-sorted-set/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/02-generic-sorted-set
```

### The (pos, found) result is the whole point

`slices.BinarySearch(s, target)` returns `(pos, found)`. `pos` is the insertion
point — the index where `target` belongs to keep `s` sorted — and `found` reports
whether `s[pos] == target`. That one call gives `Add` everything it needs: if
`found`, the element is already in the set and `Add` returns false without
touching the slice (set dedup); otherwise `pos` is exactly where to splice the
new element, and `slices.Insert(s, pos, v)` shifts the tail up and drops it in.

This is the semantic difference the exercise teaches. The older
`sort.Search(len(s), func(i int) bool { return s[i] >= target })` returns only
the insertion index; to know whether the element is *present* you then write a
second line, `i < len(s) && s[i] == target`. `slices.BinarySearch` folds that
second check into the `found` return. Fewer moving parts, and no chance of
getting the bounds check wrong.

`Contains` is then just the `found` bit, discarding `pos`. `Items` returns
`slices.Clone(s)` so a caller iterating the set cannot mutate the set's storage —
the same aliasing discipline as Exercise 1, now expressed with the generic
`slices.Clone`.

Because `T` is constrained to `cmp.Ordered`, the set works for any ordered type
with no per-type code: `Set[string]`, `Set[int]`, `Set[uint64]` all instantiate
from the same source.

Create `set.go`:

```go
package orderedset

import (
	"cmp"
	"slices"
)

// Set is a sorted set of ordered values. It stays sorted on every Add, so
// Contains is O(log n) and Items yields elements in order.
type Set[T cmp.Ordered] struct {
	items []T
}

// New returns an empty set.
func New[T cmp.Ordered]() *Set[T] {
	return &Set[T]{}
}

// Add inserts v and reports true if it was newly added. If v is already
// present, Add is a no-op and returns false. Insertion is O(n) (the shift).
func (s *Set[T]) Add(v T) bool {
	pos, found := slices.BinarySearch(s.items, v)
	if found {
		return false
	}
	s.items = slices.Insert(s.items, pos, v)
	return true
}

// Contains reports membership in O(log n).
func (s *Set[T]) Contains(v T) bool {
	_, found := slices.BinarySearch(s.items, v)
	return found
}

// Items returns a sorted clone of the elements. Mutating it does not affect
// the set.
func (s *Set[T]) Items() []T {
	return slices.Clone(s.items)
}

// Len reports the number of distinct elements.
func (s *Set[T]) Len() int {
	return len(s.items)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/orderedset"
)

func main() {
	tenants := orderedset.New[string]()
	for _, id := range []string{"acme", "globex", "acme", "initech", "hooli"} {
		added := tenants.Add(id)
		fmt.Printf("Add(%q) -> %v\n", id, added)
	}

	fmt.Println("items:", tenants.Items())
	fmt.Println("contains globex:", tenants.Contains("globex"))
	fmt.Println("contains umbrella:", tenants.Contains("umbrella"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Add("acme") -> true
Add("globex") -> true
Add("acme") -> false
Add("initech") -> true
Add("hooli") -> true
items: [acme globex hooli initech]
contains globex: true
contains umbrella: false
```

### Tests

The suite instantiates `T` as both `string` and `int` from one table.
`slices.IsSorted` checks the invariant after every insert path. The clone test
mutates `Items()` and asserts the set is intact. The permutation property test
feeds a shuffled sequence with duplicates and asserts the output equals the
sorted, de-duplicated set — a strong statement that `Add` order never matters.

Create `set_test.go`:

```go
package orderedset

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

func TestAddStringSet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []string
		want  []string
	}{
		{"dedup", []string{"b", "a", "b", "c", "a"}, []string{"a", "b", "c"}},
		{"single", []string{"x"}, []string{"x"}},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := New[string]()
			for _, v := range tc.input {
				s.Add(v)
			}
			if !slices.IsSorted(s.items) {
				t.Fatalf("items not sorted: %v", s.items)
			}
			if !slices.Equal(s.Items(), tc.want) {
				t.Fatalf("Items() = %v, want %v", s.Items(), tc.want)
			}
		})
	}
}

func TestAddIntSet(t *testing.T) {
	t.Parallel()

	s := New[int]()
	for _, v := range []int{5, 1, 3, 1, 5, 2} {
		s.Add(v)
	}
	if !slices.IsSorted(s.items) {
		t.Fatalf("items not sorted: %v", s.items)
	}
	want := []int{1, 2, 3, 5}
	if !slices.Equal(s.Items(), want) {
		t.Fatalf("Items() = %v, want %v", s.Items(), want)
	}
}

func TestAddReturnsWhetherNew(t *testing.T) {
	t.Parallel()

	s := New[int]()
	if !s.Add(7) {
		t.Fatal("first Add(7) should return true")
	}
	if s.Add(7) {
		t.Fatal("second Add(7) should return false")
	}
}

func TestContains(t *testing.T) {
	t.Parallel()

	s := New[int]()
	for _, v := range []int{2, 4, 6} {
		s.Add(v)
	}
	if !s.Contains(4) {
		t.Fatal("Contains(4) should be true")
	}
	if s.Contains(5) {
		t.Fatal("Contains(5) should be false")
	}
}

func TestItemsIsClone(t *testing.T) {
	t.Parallel()

	s := New[int]()
	for _, v := range []int{1, 2, 3} {
		s.Add(v)
	}
	got := s.Items()
	got[0] = 999

	if s.Contains(999) {
		t.Fatal("mutating Items() result corrupted the set")
	}
	if !s.Contains(1) {
		t.Fatal("mutating Items() result destroyed element 1")
	}
}

func TestPermutationInvariance(t *testing.T) {
	t.Parallel()

	base := []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	want := slices.Clone(base)

	perm := slices.Clone(base)
	perm = append(perm, base...) // duplicates too
	rand.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })

	s := New[int]()
	for _, v := range perm {
		s.Add(v)
	}
	if !slices.Equal(s.Items(), want) {
		t.Fatalf("shuffled inserts gave %v, want %v", s.Items(), want)
	}
}

func Example() {
	s := New[string]()
	s.Add("gamma")
	s.Add("alpha")
	s.Add("beta")
	s.Add("alpha")
	fmt.Println(s.Items())
	// Output: [alpha beta gamma]
}
```

## Review

The set is correct when it stays sorted and distinct under any insertion order,
and when `Items` cannot be used to reach inside it. The permutation test is the
strongest single check: if `Add` ever computed the wrong insertion point or
mishandled a duplicate, a shuffled input would produce an out-of-order or
duplicated result. The two API points to keep straight: `slices.BinarySearch`
returns `(pos, found)` — use `found` for membership and `pos` for the insert —
and `slices.Insert` returns the new slice, so you must assign it back to
`s.items`. Dropping that assignment is the classic silent bug; the sorted-invariant
assertions would catch it. Run `go test -race`.

## Resources

- [`slices.BinarySearch`](https://pkg.go.dev/slices#BinarySearch) — the `(pos, found)` contract.
- [`slices.Insert`](https://pkg.go.dev/slices#Insert) — shift-and-splice, returns the new slice.
- [`cmp` package](https://pkg.go.dev/cmp) — the `Ordered` constraint.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-timeseries-window-query.md](03-timeseries-window-query.md)
