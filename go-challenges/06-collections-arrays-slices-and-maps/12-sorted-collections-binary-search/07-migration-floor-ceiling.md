# Exercise 7: Schema-Migration Version Resolution (Floor and Ceiling Search)

A migration runner holds the sorted list of schema versions it ships. Catching a
database up to a target means finding the *floor* — the greatest available version
`<= target` — and stepping forward from there; validating that a requested version
is reachable means finding the *ceiling* — the least available version `>= target`.
Both are binary-search boundary queries, and both must handle "no such version"
(target below the minimum, or above the maximum). This exercise implements them
twice — once with `slices.BinarySearch`, once with `sort.Find` — and cross-checks
the two.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
migver/                      independent module: example.com/migver
  go.mod
  resolver.go                type Resolver; New, Floor, Ceiling (+ Find-based cross-checks)
  cmd/
    demo/
      main.go                resolve floor/ceiling for several targets
  resolver_test.go           exact match, between, below min, above max, cross-check sweep
```

Files: `resolver.go`, `cmd/demo/main.go`, `resolver_test.go`.
Implement: `Resolver` with `New([]int) *Resolver`, `Floor(target) (int, bool)`, `Ceiling(target) (int, bool)`, plus `floorFind`/`ceilingFind` using `sort.Find` for cross-checking.
Test: floor/ceiling of an exact match, of a value between versions, below the minimum, above the maximum, and a sweep asserting the `BinarySearch` and `sort.Find` implementations agree.
Verify: `go test -count=1 -race ./...`

### One search index answers both floor and ceiling

`slices.BinarySearch(versions, target)` returns `(pos, found)`. `pos` is the
lower bound: the first index whose version is `>= target`. From that single index
both boundary queries fall out:

- Ceiling: if `found`, the target itself is available, so the ceiling is `target`.
  Otherwise `pos` is the first version strictly greater than `target` — that *is*
  the ceiling — unless `pos == len(versions)`, meaning `target` is past the
  maximum and there is no ceiling.
- Floor: if `found`, the floor is `target`. Otherwise the floor is the version
  just below the insertion point, `versions[pos-1]` — unless `pos == 0`, meaning
  `target` is below the minimum and there is no floor.

This is the concepts file's point that `pos` is meaningful even when `found` is
false: the insertion point is exactly the boundary floor and ceiling need. The
two edge guards (`pos == 0` for floor, `pos == len` for ceiling) are where the
"no such version" answers come from, and they are the cases most code forgets.

To prove the boundary logic independently, the module reimplements the same
lookup with `sort.Find(n, cmp)`, which does not require a slice. Its `cmp` returns
positive over the prefix, zero at the match, negative over the suffix; passing
`cmp.Compare(target, versions[i])` gives exactly that shape (positive while
`target > versions[i]`, zero at equal, negative after), so `sort.Find` returns the
same `(pos, found)` as `slices.BinarySearch`. Note the sign is the *reverse* of a
`slices.BinarySearchFunc` comparator, which compares `element` to `target` — a
distinction the concepts file warns about. The test sweeps every target and
asserts the two implementations agree, so a sign or off-by-one slip in either one
is caught.

Create `resolver.go`:

```go
package migver

import (
	"cmp"
	"slices"
	"sort"
)

// Resolver answers floor/ceiling queries over a sorted, de-duplicated set of
// schema versions.
type Resolver struct {
	versions []int
}

// New sorts and de-duplicates the given versions.
func New(versions []int) *Resolver {
	vs := slices.Clone(versions)
	slices.Sort(vs)
	vs = slices.Compact(vs)
	return &Resolver{versions: vs}
}

// Floor returns the greatest available version <= target, or (0, false) if
// target is below the minimum available version.
func (r *Resolver) Floor(target int) (int, bool) {
	pos, found := slices.BinarySearch(r.versions, target)
	if found {
		return target, true
	}
	if pos == 0 {
		return 0, false
	}
	return r.versions[pos-1], true
}

// Ceiling returns the least available version >= target, or (0, false) if
// target is above the maximum available version.
func (r *Resolver) Ceiling(target int) (int, bool) {
	pos, found := slices.BinarySearch(r.versions, target)
	if found {
		return target, true
	}
	if pos == len(r.versions) {
		return 0, false
	}
	return r.versions[pos], true
}

// findPos reimplements the lower-bound search with sort.Find as an independent
// cross-check. cmp.Compare(target, versions[i]) is positive over the prefix,
// zero at a match, negative over the suffix -- the sign shape sort.Find wants.
func (r *Resolver) findPos(target int) (int, bool) {
	return sort.Find(len(r.versions), func(i int) int {
		return cmp.Compare(target, r.versions[i])
	})
}

// floorFind is Floor computed via sort.Find.
func (r *Resolver) floorFind(target int) (int, bool) {
	pos, found := r.findPos(target)
	if found {
		return target, true
	}
	if pos == 0 {
		return 0, false
	}
	return r.versions[pos-1], true
}

// ceilingFind is Ceiling computed via sort.Find.
func (r *Resolver) ceilingFind(target int) (int, bool) {
	pos, found := r.findPos(target)
	if found {
		return target, true
	}
	if pos == len(r.versions) {
		return 0, false
	}
	return r.versions[pos], true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/migver"
)

func main() {
	r := migver.New([]int{1, 3, 4, 7, 10})

	for _, target := range []int{0, 4, 5, 10, 11} {
		floor, fok := r.Floor(target)
		ceil, cok := r.Ceiling(target)
		fmt.Printf("target=%2d floor=%s ceiling=%s\n", target, show(floor, fok), show(ceil, cok))
	}
}

func show(v int, ok bool) string {
	if !ok {
		return "none"
	}
	return fmt.Sprintf("%d", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
target= 0 floor=none ceiling=1
target= 4 floor=4 ceiling=4
target= 5 floor=4 ceiling=7
target=10 floor=10 ceiling=10
target=11 floor=10 ceiling=none
```

### Tests

The table pins floor and ceiling for an exact match, a value between two versions,
below the minimum, and above the maximum. `TestCrossCheck` sweeps a wide target
range and asserts the `BinarySearch` and `sort.Find` implementations return
identical results — an independent proof of the boundary logic.

Create `resolver_test.go`:

```go
package migver

import (
	"fmt"
	"testing"
)

func TestFloorCeiling(t *testing.T) {
	t.Parallel()

	r := New([]int{1, 3, 4, 7, 10})
	cases := []struct {
		target    int
		floor     int
		floorOK   bool
		ceiling   int
		ceilingOK bool
	}{
		{target: 4, floor: 4, floorOK: true, ceiling: 4, ceilingOK: true},    // exact match
		{target: 5, floor: 4, floorOK: true, ceiling: 7, ceilingOK: true},    // between
		{target: 0, floor: 0, floorOK: false, ceiling: 1, ceilingOK: true},   // below min
		{target: 11, floor: 10, floorOK: true, ceiling: 0, ceilingOK: false}, // above max
		{target: 1, floor: 1, floorOK: true, ceiling: 1, ceilingOK: true},    // min exact
		{target: 10, floor: 10, floorOK: true, ceiling: 10, ceilingOK: true}, // max exact
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("target=%d", tc.target), func(t *testing.T) {
			t.Parallel()
			f, fok := r.Floor(tc.target)
			if f != tc.floor || fok != tc.floorOK {
				t.Errorf("Floor(%d) = %d,%v; want %d,%v", tc.target, f, fok, tc.floor, tc.floorOK)
			}
			c, cok := r.Ceiling(tc.target)
			if c != tc.ceiling || cok != tc.ceilingOK {
				t.Errorf("Ceiling(%d) = %d,%v; want %d,%v", tc.target, c, cok, tc.ceiling, tc.ceilingOK)
			}
		})
	}
}

func TestCrossCheck(t *testing.T) {
	t.Parallel()

	r := New([]int{2, 5, 5, 8, 13, 21}) // duplicate 5 collapses in New
	for target := -3; target <= 25; target++ {
		f1, fok1 := r.Floor(target)
		f2, fok2 := r.floorFind(target)
		if f1 != f2 || fok1 != fok2 {
			t.Fatalf("Floor(%d): BinarySearch=%d,%v Find=%d,%v", target, f1, fok1, f2, fok2)
		}
		c1, cok1 := r.Ceiling(target)
		c2, cok2 := r.ceilingFind(target)
		if c1 != c2 || cok1 != cok2 {
			t.Fatalf("Ceiling(%d): BinarySearch=%d,%v Find=%d,%v", target, c1, cok1, c2, cok2)
		}
	}
}

func TestNewDeduplicates(t *testing.T) {
	t.Parallel()

	r := New([]int{7, 3, 3, 1, 7})
	want := []int{1, 3, 7}
	if len(r.versions) != len(want) {
		t.Fatalf("versions = %v, want %v", r.versions, want)
	}
	for i := range want {
		if r.versions[i] != want[i] {
			t.Fatalf("versions = %v, want %v", r.versions, want)
		}
	}
}

func Example() {
	r := New([]int{1, 3, 4, 7, 10})
	floor, _ := r.Floor(5)
	ceil, _ := r.Ceiling(5)
	fmt.Println(floor, ceil)
	// Output: 4 7
}
```

## Review

Floor and ceiling are correct when they agree at an exact match (both return the
target), split around a value between versions (floor below, ceiling above), and
return not-found at the correct end (`pos == 0` for floor below the minimum,
`pos == len` for ceiling above the maximum). The cross-check sweep is the strongest
test: it would expose a sign error in either comparator or a missing edge guard,
because the two independent implementations would diverge on some target. Keep the
sign conventions straight — `sort.Find`'s `cmp` is positive on the prefix, which is
`cmp.Compare(target, versions[i])`, the reverse of a `BinarySearchFunc`
comparator. Run `go test -race`.

## Resources

- [`sort.Find`](https://pkg.go.dev/sort#Find) — the `(i, found)` closure search and its `cmp` sign contract.
- [`slices.BinarySearch`](https://pkg.go.dev/slices#BinarySearch) — the slice-backed `(pos, found)` search.
- [`cmp.Compare`](https://pkg.go.dev/cmp#Compare) — the three-way comparison feeding `sort.Find`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-leaderboard-ranked-insert.md](08-leaderboard-ranked-insert.md)
