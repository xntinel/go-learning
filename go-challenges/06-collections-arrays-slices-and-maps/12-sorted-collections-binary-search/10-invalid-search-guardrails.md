# Exercise 10: Failure Modes — Searching an Unsorted Slice and NaN/Comparator Traps

Binary search does not fail loudly when its precondition is violated; it lies
quietly. This exercise turns the lesson's failure modes into an executable
contract: a constructor that validates its input is sorted and NaN-free and
rejects it otherwise, plus tests that *demonstrate* the silent lies — a search on
an unsorted slice reporting a present element as absent, and a comparator
inconsistent with the sort order missing every element.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
searchguard/                 independent module: example.com/searchguard
  go.mod
  guard.go                   type SortedFloats; New (validates), Contains
  cmd/
    demo/
      main.go                accept a valid set, reject unsorted and NaN
  guard_test.go              constructor guards + demonstrations of the silent lies
```

Files: `guard.go`, `cmd/demo/main.go`, `guard_test.go`.
Implement: `SortedFloats` with `New([]float64) (*SortedFloats, error)` validating sortedness and rejecting NaN, and `Contains(float64) bool`.
Test: `New` rejects unsorted input and NaN; a raw `slices.BinarySearch` on an unsorted slice reports a present element as not-found; a comparator inconsistent with the sort order misses a present element.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/10-invalid-search-guardrails/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/12-sorted-collections-binary-search/10-invalid-search-guardrails
```

### Validate at the boundary, because the search never will

The whole point of this module is that `slices.BinarySearch` and
`slices.BinarySearchFunc` assume two things they cannot check: the slice is
sorted, and the comparator is a total order consistent with that sort. Violate
either and you get a wrong index or a false "not found" for a present element —
no panic, no error. The defense is to validate at the *constructor*, the one
place you can, and refuse a bad slice before any query can be mis-answered.

`New` does two checks. First it rejects NaN: a `float64` slice containing NaN
cannot be totally ordered because NaN compares unequal to everything including
itself, so both sorting and searching become undefined. Rejecting NaN at the
boundary keeps the order total. Second it checks `slices.IsSortedFunc(vals,
cmp.Compare)` and refuses an unsorted slice. Both failures return wrapped
sentinel errors so callers can distinguish them with `errors.Is`.

`Contains` then searches with `slices.BinarySearchFunc(s, v, cmp.Compare)` — safe,
because the invariant was enforced on construction.

The tests are where the failure modes become executable. `TestUnsortedSilentLie`
bypasses the constructor, calls `slices.BinarySearch` directly on the unsorted
slice `[5, 1, 3, 2, 4]`, searches for `5` — which is present at index 0 — and
asserts the search reports `found == false`. That is the silent lie, pinned as a
fact: the value is in the slice, and binary search says it is not, purely because
the slice is not sorted. `TestReversedComparatorMiss` does the same for the
comparator hazard: a slice correctly sorted ascending, searched with a reversed
comparator (`cmp.Compare(target, element)` instead of `cmp.Compare(element,
target)`), misses `3` even though it is present. These two tests are the reason
`New` exists.

Create `guard.go`:

```go
package searchguard

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
)

// ErrUnsorted is returned when New is given an unsorted slice.
var ErrUnsorted = errors.New("searchguard: input not sorted")

// ErrNaN is returned when New is given a slice containing NaN.
var ErrNaN = errors.New("searchguard: NaN is not orderable")

// SortedFloats is a validated sorted set of float64 values safe to binary-search.
type SortedFloats struct {
	data []float64
}

// New validates that vals contains no NaN and is sorted ascending, then stores a
// clone. It refuses invalid input rather than let a later search lie.
func New(vals []float64) (*SortedFloats, error) {
	for i, v := range vals {
		if math.IsNaN(v) {
			return nil, fmt.Errorf("%w: index %d", ErrNaN, i)
		}
	}
	if !slices.IsSortedFunc(vals, cmp.Compare) {
		return nil, ErrUnsorted
	}
	return &SortedFloats{data: slices.Clone(vals)}, nil
}

// Contains reports membership via a safe binary search over the validated slice.
func (s *SortedFloats) Contains(v float64) bool {
	_, found := slices.BinarySearchFunc(s.data, v, cmp.Compare)
	return found
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"math"

	"example.com/searchguard"
)

func main() {
	ok, err := searchguard.New([]float64{1.5, 2.0, 3.25, 10.0})
	fmt.Printf("valid set: err=%v contains(3.25)=%v\n", err, ok.Contains(3.25))

	_, err = searchguard.New([]float64{3, 1, 2})
	fmt.Printf("unsorted:  errors.Is(ErrUnsorted)=%v\n", errors.Is(err, searchguard.ErrUnsorted))

	_, err = searchguard.New([]float64{1, math.NaN(), 3})
	fmt.Printf("with NaN:  errors.Is(ErrNaN)=%v\n", errors.Is(err, searchguard.ErrNaN))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid set: err=<nil> contains(3.25)=true
unsorted:  errors.Is(ErrUnsorted)=true
with NaN:  errors.Is(ErrNaN)=true
```

### Tests

The constructor tests pin the two guards. The two "silent lie" tests are the core
of the lesson: they call the raw `slices` functions on invalid inputs and assert
the wrong answer, documenting exactly what the constructor is protecting against.

Create `guard_test.go`:

```go
package searchguard

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"
)

func TestNewRejectsUnsorted(t *testing.T) {
	t.Parallel()

	if _, err := New([]float64{3, 1, 2}); !errors.Is(err, ErrUnsorted) {
		t.Fatalf("New(unsorted): err = %v, want ErrUnsorted", err)
	}
}

func TestNewRejectsNaN(t *testing.T) {
	t.Parallel()

	if _, err := New([]float64{1, math.NaN(), 3}); !errors.Is(err, ErrNaN) {
		t.Fatalf("New(NaN): err = %v, want ErrNaN", err)
	}
}

func TestValidLookup(t *testing.T) {
	t.Parallel()

	s, err := New([]float64{1.5, 2.0, 3.25, 10.0})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.Contains(3.25) {
		t.Fatal("Contains(3.25) should be true")
	}
	if s.Contains(4.0) {
		t.Fatal("Contains(4.0) should be false")
	}
}

// TestUnsortedSilentLie documents the failure the constructor prevents: binary
// search on an unsorted slice reports a present element as absent.
func TestUnsortedSilentLie(t *testing.T) {
	t.Parallel()

	unsorted := []float64{5, 1, 3, 2, 4} // 5 is present, at index 0
	_, found := slices.BinarySearch(unsorted, 5)
	if found {
		t.Fatal("expected the unsorted search to LIE (report 5 as not found)")
	}
	// Prove 5 really is in the slice, so the miss is a lie, not a true absence.
	if !slices.Contains(unsorted, 5) {
		t.Fatal("test setup wrong: 5 should be in the slice")
	}
}

// TestReversedComparatorMiss documents the other failure: a comparator
// inconsistent with the sort order misses a present element.
func TestReversedComparatorMiss(t *testing.T) {
	t.Parallel()

	asc := []float64{1, 2, 3, 4, 5} // correctly sorted ascending
	reversed := func(element, target float64) int {
		return cmp.Compare(target, element) // wrong: sign is inverted
	}
	_, found := slices.BinarySearchFunc(asc, 3, reversed)
	if found {
		t.Fatal("expected the reversed comparator to MISS a present element")
	}
	// The correct comparator finds it.
	if _, ok := slices.BinarySearchFunc(asc, 3, cmp.Compare); !ok {
		t.Fatal("the correct comparator should find 3")
	}
}

func Example() {
	s, _ := New([]float64{1.5, 2.0, 3.25, 10.0})
	fmt.Println(s.Contains(2.0), s.Contains(2.5))
	// Output: true false
}
```

## Review

The module is correct when `New` accepts only sorted, NaN-free slices and the two
demonstration tests keep failing loudly for the right reason. Those tests are
unusual — they assert a *wrong* answer — because their job is to document the
silent lies that motivate the guard: an unsorted search reporting a present value
as absent, and a reversed comparator missing everything. The practical rule they
encode is the concepts file's first point: never binary-search a slice whose
sortedness and comparator you have not established, and prefer a validating
constructor over trusting the caller. Note the sign trap in `reversed`: a
`BinarySearchFunc` comparator must compare `element` to `target`, and inverting it
is a total-order violation the search cannot detect. Run `go test -race`.

## Resources

- [`slices.IsSortedFunc`](https://pkg.go.dev/slices#IsSortedFunc) — the constructor guard.
- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the comparator contract that a reversed sign violates.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — total ordering of ordered types, including how NaN is handled.

---

Back to [00-concepts.md](00-concepts.md) | Next: [11-dns-rrset-equal-range.md](11-dns-rrset-equal-range.md)
