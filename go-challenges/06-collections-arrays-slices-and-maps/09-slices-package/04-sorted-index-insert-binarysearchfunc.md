# Exercise 4: Maintain A Sorted Secondary Index With Ordered Insert (BinarySearchFunc + Insert)

A slice-backed secondary index — a leaderboard, a time-ordered log, a sorted set
of ids — earns its keep only if it stays sorted across insertions, so lookups can
binary-search in O(log n). This module builds that index: `BinarySearchFunc`
finds the insertion point, `slices.Insert` places the element there, and lookups
reuse the same comparator. The invariant to internalize is that `BinarySearch` is
silently wrong on an unsorted slice, so the index must never let itself drift.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
sortedindex/                   module example.com/sortedindex
  go.mod                       go 1.24
  index.go                     type Score, Index; Insert (BinarySearchFunc+Insert), Lookup
  cmd/
    demo/
      main.go                  runnable demo: insert shuffled, stay sorted, look up
  index_test.go                stays sorted after each insert; found/not-found positions; drift is silent
```

- Files: `index.go`, `cmd/demo/main.go`, `index_test.go`.
- Implement: an `Index` over `[]Score` keyed by name, with `Insert(Score)` doing an ordered insert via `BinarySearchFunc` + `slices.Insert`, and `Lookup(name) (Score, bool)`.
- Test: inserting a shuffled sequence keeps the slice `IsSortedFunc`-sorted after every step; `BinarySearchFunc` returns `(correctIndex, true)` for present keys and `(insertionPoint, false)` for absent keys including before-first and after-last; a documented negative test showing an unsorted slice yields a wrong-but-plausible index.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Ordered insert: search, then place

The maintenance loop is two calls. `slices.BinarySearchFunc(s, target, cmp)`
returns `(i, found)`: `i` is the position where `target` is or where it would go
to keep the slice sorted, and `found` says whether an element comparing equal to
`target` is already there. For an ordered insert you ignore `found` (or use it to
decide update-vs-insert) and call `slices.Insert(s, i, target)`, which shifts the
tail up by one and drops `target` at index `i`. `Insert` returns a new slice
header — it may have grown the backing array — so you must reassign. The result
is that after every insert the slice is still sorted, so the next
`BinarySearchFunc` is valid, so the invariant is self-sustaining.

`BinarySearchFunc`'s comparator has the shape `func(elem E, target T) int`: it is
called with a slice element first and the search target second, and returns the
sign of `elem` relative to `target`. Here elements and targets are both `Score`,
compared by name with `cmp.Compare`. The non-negotiable rule is that this is the
*same* ordering used to keep the slice sorted. If the index sorts by name but a
lookup searched by score, or sorted case-insensitively but searched
case-sensitively, `BinarySearchFunc` would report present keys as absent — a
found=false on data that is right there.

The silent-drift danger is the whole reason this is a senior topic. `BinarySearch`
never checks that its input is sorted; on an unsorted slice it returns a
plausible index with `found=false` (or a wrong index) and no error. The negative
test constructs an unsorted slice and shows the search failing to find a key that
is present — documented as the failure mode, not asserted as correct behavior —
so the lesson is felt: the only defense is to never let the slice drift, which is
what ordered insert guarantees.

Create `index.go`:

```go
package sortedindex

import (
	"cmp"
	"slices"
)

// Score is a leaderboard entry; the index is ordered by Name.
type Score struct {
	Name  string
	Value int
}

// byName is the single ordering used for BOTH inserts and lookups. Using any
// other order for a lookup than was used to sort makes BinarySearchFunc lie.
func byName(elem Score, target Score) int {
	return cmp.Compare(elem.Name, target.Name)
}

// Index is a slice-backed secondary index kept sorted by Name.
type Index struct {
	scores []Score
}

// Slice returns the ordered entries (for inspection and tests).
func (ix *Index) Slice() []Score { return ix.scores }

// Insert places s in sorted position, keeping the invariant. If a key equal to
// s already exists it is overwritten (an upsert), so the index has no duplicates.
func (ix *Index) Insert(s Score) {
	i, found := slices.BinarySearchFunc(ix.scores, s, byName)
	if found {
		ix.scores[i] = s
		return
	}
	ix.scores = slices.Insert(ix.scores, i, s)
}

// Lookup returns the entry for name if present, using the same ordering.
func (ix *Index) Lookup(name string) (Score, bool) {
	i, found := slices.BinarySearchFunc(ix.scores, Score{Name: name}, byName)
	if !found {
		return Score{}, false
	}
	return ix.scores[i], true
}

// IsSorted reports whether the internal slice is ordered by the search comparator.
func (ix *Index) IsSorted() bool {
	return slices.IsSortedFunc(ix.scores, byName)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sortedindex"
)

func main() {
	var ix sortedindex.Index
	for _, s := range []sortedindex.Score{
		{Name: "mallory", Value: 30},
		{Name: "alice", Value: 10},
		{Name: "carol", Value: 20},
		{Name: "bob", Value: 15},
	} {
		ix.Insert(s)
	}

	for _, s := range ix.Slice() {
		fmt.Printf("%s=%d\n", s.Name, s.Value)
	}
	fmt.Printf("sorted: %v\n", ix.IsSorted())

	if s, ok := ix.Lookup("carol"); ok {
		fmt.Printf("lookup carol -> %d\n", s.Value)
	}
	if _, ok := ix.Lookup("dave"); !ok {
		fmt.Println("lookup dave -> absent")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice=10
bob=15
carol=20
mallory=30
sorted: true
lookup carol -> 20
lookup dave -> absent
```

The names were inserted in shuffled order and come out sorted; both lookups
resolve correctly through the same comparator.

### Tests

`TestStaysSortedAfterEachInsert` inserts a shuffled sequence and asserts
`IsSortedFunc` holds after every single insert. `TestSearchPositions` pins the
`(index, found)` result for present keys, an absent key before the first, an
absent key between two, and an absent key after the last. `TestUnsortedIsSilentlyWrong`
documents the failure mode: a hand-built unsorted slice makes `BinarySearchFunc`
miss a key that is present.

Create `index_test.go`:

```go
package sortedindex

import (
	"slices"
	"testing"
)

func TestStaysSortedAfterEachInsert(t *testing.T) {
	t.Parallel()

	names := []string{"mallory", "alice", "carol", "bob", "erin", "dave"}
	var ix Index
	for _, n := range names {
		ix.Insert(Score{Name: n})
		if !ix.IsSorted() {
			t.Fatalf("index not sorted after inserting %q: %v", n, ix.Slice())
		}
	}
	if len(ix.Slice()) != len(names) {
		t.Fatalf("len = %d, want %d", len(ix.Slice()), len(names))
	}
}

func TestSearchPositions(t *testing.T) {
	t.Parallel()

	var ix Index
	for _, n := range []string{"bob", "carol", "erin"} {
		ix.Insert(Score{Name: n})
	}

	cases := []struct {
		name      string
		wantIndex int
		wantFound bool
	}{
		{"bob", 0, true},
		{"carol", 1, true},
		{"erin", 2, true},
		{"alice", 0, false}, // before first
		{"dave", 2, false},  // between carol and erin
		{"zoe", 3, false},   // after last
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			i, found := slices.BinarySearchFunc(ix.Slice(), Score{Name: tc.name}, byName)
			if i != tc.wantIndex || found != tc.wantFound {
				t.Fatalf("search(%q) = (%d,%v), want (%d,%v)", tc.name, i, found, tc.wantIndex, tc.wantFound)
			}
		})
	}
}

func TestUnsortedIsSilentlyWrong(t *testing.T) {
	t.Parallel()

	// Deliberately unsorted: BinarySearchFunc gives NO error, just a wrong answer.
	unsorted := []Score{{Name: "carol"}, {Name: "alice"}, {Name: "bob"}}
	_, found := slices.BinarySearchFunc(unsorted, Score{Name: "alice"}, byName)
	if found {
		t.Fatal("test premise broke: search unexpectedly found alice in unsorted slice")
	}
	// The same slice, sorted, finds alice. This is documentation of the failure
	// mode, not a claim that missing it is correct.
	slices.SortFunc(unsorted, byName)
	if _, ok := slices.BinarySearchFunc(unsorted, Score{Name: "alice"}, byName); !ok {
		t.Fatal("after sorting, alice should be found")
	}
}
```

## Review

The index is correct when it is `IsSortedFunc`-sorted after every mutation, which
is what makes each subsequent `BinarySearchFunc` valid. The two calls that
maintain it — search for the position, `Insert` at it — must share the exact
comparator that `Lookup` uses; a divergence there is the classic "sorted one way,
searched another" bug that reports present keys as absent. `Insert` returns a new
header, so reassigning it is mandatory. The unsorted negative test is the point of
the whole module: `BinarySearch` trusts you, so the invariant is your job. Run
`go test -race`; the search sub-tests read a shared immutable index in parallel,
which is safe.

## Resources

- [`slices.BinarySearchFunc`](https://pkg.go.dev/slices#BinarySearchFunc) — the (index, found) contract and the sorted-input precondition.
- [`slices.Insert`](https://pkg.go.dev/slices#Insert) — ordered placement returning a new header.
- [`slices.IsSortedFunc`](https://pkg.go.dev/slices#IsSortedFunc) — asserting the maintained invariant.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-reconcile-drift-equal-compare.md](05-reconcile-drift-equal-compare.md)
