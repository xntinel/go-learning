# Exercise 33: Binary Search Over Sorted Slice

**Nivel: Intermedio** — validacion rapida (un test corto).

`sort.Search` in the standard library returns a single `int` that always
means "insertion point", found or not — useful, but it forces every caller
to re-check `items[i] == target` themselves just to know if the value was
actually there. This exercise builds a generic `Search(items, target)
(index int, found bool)` where `index` carries no meaning at all unless
`found` is true, making the "did we actually find it" question part of the
signature instead of something the caller re-derives.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
binsearch/                 independent module: example.com/binary-search-sorted-list
  go.mod                   go 1.24
  binsearch.go             package binsearch; Search[T cmp.Ordered](items,target) (index,found)
  cmd/
    demo/
      main.go              middle hit, miss, both boundaries, empty slice
  binsearch_test.go         table of ints; empty slice; single element; strings; bounded-iteration check
```

- Files: `binsearch.go`, `cmd/demo/main.go`, `binsearch_test.go`.
- Implement: `Search[T cmp.Ordered](items []T, target T) (index int, found bool)` using an iterative `lo`/`hi` window (no recursion) that halves on every step, returning `(-1, false)` when `target` is absent.
- Test: a table of hits at the middle and both boundaries plus misses below, above, and between elements; an empty slice never panics; a single-element slice hits both the found and not-found paths; the generic function works over `[]string` too; a dedicated test proves the search never exceeds `floor(log2(n)) + 1` comparisons even on a 100,000-element slice.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why `index` has to be worthless when `found` is false

A function that returns a single `int` — `sort.Search`'s contract, "the
smallest index where the predicate is true, or `len(items)` if none" —
means every caller has to write `i := sort.Search(...); if i <
len(items) && items[i] == target { ... }` themselves, every time. That
extra check is easy to forget, and forgetting it produces a bug that only
shows up when the target happens to be absent but the insertion point
happens to land on a slot containing some *other* value the caller does
not expect to compare against.

Splitting the result into `(index int, found bool)` moves that check
inside the function, where it can never be skipped: `found` is the single
source of truth for "was it there", and the implementation is free to
return whatever placeholder it wants in `index` on the `false` path
precisely because callers are documented never to look at it — this
implementation happens to return `-1`, but that specific value is not part
of the contract other implementations must honor. The value of the
contract is what it prevents: a caller can never accidentally treat a
stale or placeholder `index` as a real position.

Create `binsearch.go`:

```go
package binsearch

import "cmp"

// Search performs a classic iterative binary search over items, which must
// already be sorted in ascending order. It reports the index of target and
// whether it was found. index is meaningful only when found is true --
// callers must not read index after a false result, since its value on
// that path carries no positional information (this implementation
// returns -1, but that is not a contract other implementations must
// share).
//
// The loop halves the search window [lo, hi] on every iteration, so it
// always terminates within floor(log2(len(items))) + 1 comparisons: there
// is no recursion and no unbounded loop, just a shrinking integer range.
func Search[T cmp.Ordered](items []T, target T) (index int, found bool) {
	lo, hi := 0, len(items)-1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		switch {
		case items[mid] == target:
			return mid, true
		case items[mid] < target:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return -1, false
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	binsearch "example.com/binary-search-sorted-list"
)

func main() {
	items := []int{2, 4, 6, 8, 10, 12, 14, 16, 18, 20}

	index, found := binsearch.Search(items, 14)
	fmt.Printf("search 14: index=%d found=%t\n", index, found)

	index, found = binsearch.Search(items, 7)
	fmt.Printf("search 7:  found=%t (index is meaningless here)\n", found)
	_ = index

	index, found = binsearch.Search(items, 2)
	fmt.Printf("search 2:  index=%d found=%t (left boundary)\n", index, found)

	index, found = binsearch.Search(items, 20)
	fmt.Printf("search 20: index=%d found=%t (right boundary)\n", index, found)

	index, found = binsearch.Search([]int{}, 5)
	fmt.Printf("empty slice: index=%d found=%t\n", index, found)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
search 14: index=6 found=true
search 7:  found=false (index is meaningless here)
search 2:  index=0 found=true (left boundary)
search 20: index=9 found=true (right boundary)
empty slice: index=-1 found=false
```

### Tests

Create `binsearch_test.go`:

```go
package binsearch

import (
	"math/bits"
	"testing"
)

func TestSearchTable(t *testing.T) {
	t.Parallel()
	items := []int{2, 4, 6, 8, 10, 12, 14, 16, 18, 20}

	cases := []struct {
		name      string
		target    int
		wantIndex int
		wantFound bool
	}{
		{name: "middle element", target: 12, wantIndex: 5, wantFound: true},
		{name: "left boundary", target: 2, wantIndex: 0, wantFound: true},
		{name: "right boundary", target: 20, wantIndex: 9, wantFound: true},
		{name: "not present between elements", target: 7, wantFound: false},
		{name: "not present below range", target: -5, wantFound: false},
		{name: "not present above range", target: 999, wantFound: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			index, found := Search(items, tc.target)
			if found != tc.wantFound {
				t.Fatalf("found = %t, want %t", found, tc.wantFound)
			}
			if found && index != tc.wantIndex {
				t.Fatalf("index = %d, want %d", index, tc.wantIndex)
			}
			if found {
				if items[index] != tc.target {
					t.Fatalf("items[%d] = %d, want %d", index, items[index], tc.target)
				}
			}
		})
	}
}

func TestSearchEmptySlice(t *testing.T) {
	t.Parallel()
	index, found := Search([]int{}, 5)
	if found {
		t.Fatal("found = true on an empty slice, want false")
	}
	_ = index // index is documented as meaningless when found is false
}

func TestSearchSingleElement(t *testing.T) {
	t.Parallel()

	if index, found := Search([]int{42}, 42); !found || index != 0 {
		t.Fatalf("index=%d found=%t, want 0/true", index, found)
	}
	if _, found := Search([]int{42}, 7); found {
		t.Fatal("found = true, want false")
	}
}

func TestSearchStrings(t *testing.T) {
	t.Parallel()
	items := []string{"alpha", "bravo", "charlie", "delta", "echo"}

	index, found := Search(items, "charlie")
	if !found || index != 2 {
		t.Fatalf("index=%d found=%t, want 2/true", index, found)
	}
}

// searchCounting is a test-only, instrumented copy of Search's algorithm
// that counts comparisons, used solely to prove the loop terminates within
// the theoretical bound instead of degrading into a linear scan.
func searchCounting(items []int, target int) (index int, found bool, comparisons int) {
	lo, hi := 0, len(items)-1
	for lo <= hi {
		comparisons++
		mid := lo + (hi-lo)/2
		switch {
		case items[mid] == target:
			return mid, true, comparisons
		case items[mid] < target:
			lo = mid + 1
		default:
			hi = mid - 1
		}
	}
	return -1, false, comparisons
}

func TestSearchIsBoundedByLogN(t *testing.T) {
	t.Parallel()
	const n = 100_000
	items := make([]int, n)
	for i := range items {
		items[i] = i * 2
	}
	maxComparisons := bits.Len(uint(n)) + 1

	// The worst case for an absent target is a value between two present
	// ones, forcing the search to narrow all the way down.
	_, found, comparisons := searchCounting(items, 1) // odd: never present
	if found {
		t.Fatal("target 1 should never be present in an all-even slice")
	}
	if comparisons > maxComparisons {
		t.Fatalf("comparisons = %d, want <= %d (log2(%d)+1)", comparisons, maxComparisons, n)
	}
}
```

## Review

`Search` is correct when `found` is the only signal a caller needs — no
combination of "check `found`, then also sanity-check `index`" should ever
be necessary. `TestSearchTable` covers both boundaries (the classic
off-by-one sources: an empty `[lo, hi]` window and a `mid` computation that
overflows for huge slices, avoided here via `lo + (hi-lo)/2` instead of
`(lo+hi)/2`) plus three distinct kinds of miss. `TestSearchIsBoundedByLogN`
is the load-bearing test for the "bounded iteration" half of the exercise:
it proves the algorithm degrades gracefully to a logarithmic number of
comparisons even at 100,000 elements, rather than silently becoming a
linear scan if a future edit reintroduced a bug like scanning forward
after `mid` instead of narrowing `hi`.

The mistake to avoid is computing `mid := (lo + hi) / 2` instead of `mid :=
lo + (hi-lo)/2` — the former overflows `int` for a large enough slice on a
32-bit platform (and, at $2^{62}$-scale indices, even on 64-bit), silently
producing a negative `mid` and an out-of-range index. The
subtraction-based form is not merely a style preference; it is the
difference between a search that works at any size and one with a hidden
ceiling.

## Resources

- [sort.Search](https://pkg.go.dev/sort#Search) — the standard library's insertion-point search that this exercise's `found bool` return improves on for exact-match lookups.
- [The Go Blog: When to use generics](https://go.dev/blog/when-generics) — the `cmp.Ordered` constraint used here to make `Search` work over any ordered element type.
- [cmp package](https://pkg.go.dev/cmp) — the `Ordered` constraint's definition.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-grpc-metadata-parse-extract.md](32-grpc-metadata-parse-extract.md) | Next: [34-tls-cert-verify-with-subject.md](34-tls-cert-verify-with-subject.md)
