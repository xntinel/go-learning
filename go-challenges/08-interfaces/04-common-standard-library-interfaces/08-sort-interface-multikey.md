# Exercise 8: sort.Interface for a Stable Multi-Key Report Sort

A report sort is rarely single-key: rows go by priority descending, then by
creation time ascending, then by id. This module implements that ordering as
`sort.Interface` with `sort.Stable`, contrasts it against the modern
`slices.SortStableFunc` with `cmp.Or`, and shows where `sort.Interface` still
earns its keep — including pairing with `sort.Search`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
reportsort/                 independent module: example.com/reportsort
  go.mod
  reportsort.go             byUrgency (sort.Interface); SortModern; FirstBelow (sort.Search)
  cmd/
    demo/
      main.go               sorts a fixture and prints the ordering
  reportsort_test.go        multi-key order, stability, sort.Search, parity with slices
```

- Files: `reportsort.go`, `cmd/demo/main.go`, `reportsort_test.go`.
- Implement: `Len`/`Less`/`Swap` on a `byUrgency` slice type ordering by (priority desc, createdAt asc, id asc); a `SortModern` using `slices.SortStableFunc` + `cmp.Or`; and `FirstBelow` using `sort.Search`.
- Test: the multi-key ordering on a fixed fixture; stability (equal keys keep input order) under `sort.Stable`; `sort.Search` finds an insertion index; the `sort.Interface` result matches `SortModern`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/04-common-standard-library-interfaces/08-sort-interface-multikey/cmd/demo
cd go-solutions/08-interfaces/04-common-standard-library-interfaces/08-sort-interface-multikey
```

### Two ways to say the same ordering

`sort.Interface` is three methods on the slice type. `Less(i, j)` encodes the full
ordering as a boolean: return `true` when element `i` must sort before `j`. For a
multi-key sort you compare key by key, returning as soon as a key differs:
priority *descending* means `a.Priority > b.Priority`; created-at *ascending* means
`a.CreatedAt.Before(b.CreatedAt)`; id ascending is the final tiebreak. Hand-writing
`Less` this way is error-prone — it is easy to get a comparison backwards or forget
the "only compare the next key when the previous is equal" structure.

The modern idiom makes the structure explicit. `slices.SortStableFunc` takes a
comparison returning negative/zero/positive, and `cmp.Or` returns the first
non-zero of a list — exactly "first key that breaks the tie". `cmp.Compare(b.Priority,
a.Priority)` flips the operands to get descending; `a.CreatedAt.Compare(b.CreatedAt)`
is ascending; `cmp.Compare(a.ID, b.ID)` is the final key. This is the default you
should reach for. `sort.Interface` still wins in three cases: a *named, reusable*
ordering attached to a type (`sort.Sort(byUrgency(rows))` reads well and can be
passed around); sorting *parallel slices* where `Swap` reorders several arrays at
once; and pairing with `sort.Search`.

`sort.Search` does binary search over an index range using a monotonic predicate
and returns the smallest index where it becomes true. Because the report is sorted
by priority *descending*, the predicate "priority below `p`" is
false,false,...,true,true — monotonic — so `FirstBelow` finds the boundary between
the high-priority head and the rest in O(log n), the exact primitive you need to
page or partition an already-sorted report.

Stability matters when the compared keys can tie. `sort.Stable` (and
`slices.SortStableFunc`) preserve the input order of elements that compare equal;
`sort.Sort` does not, so equal elements may reorder run to run. When two report
rows share the same (priority, createdAt, id) — a duplicated record from a merge —
stability keeps their original order, which the test pins.

Create `reportsort.go`:

```go
package reportsort

import (
	"cmp"
	"slices"
	"sort"
	"time"
)

// Row is a report line. Source is not part of the sort key; it distinguishes
// otherwise-equal rows so stability is observable.
type Row struct {
	ID        string
	Priority  int
	CreatedAt time.Time
	Source    string
}

// byUrgency orders rows by priority descending, then createdAt ascending, then
// id ascending. It is a named, reusable sort.Interface.
type byUrgency []Row

func (r byUrgency) Len() int      { return len(r) }
func (r byUrgency) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r byUrgency) Less(i, j int) bool {
	a, b := r[i], r[j]
	if a.Priority != b.Priority {
		return a.Priority > b.Priority // descending
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt) // ascending
	}
	return a.ID < b.ID
}

// SortStable sorts in place using the named sort.Interface, preserving the input
// order of rows that compare equal.
func SortStable(rows []Row) { sort.Stable(byUrgency(rows)) }

// SortModern expresses the identical ordering with the generic slices/cmp idiom.
func SortModern(rows []Row) {
	slices.SortStableFunc(rows, func(a, b Row) int {
		return cmp.Or(
			cmp.Compare(b.Priority, a.Priority), // priority descending
			a.CreatedAt.Compare(b.CreatedAt),    // createdAt ascending
			cmp.Compare(a.ID, b.ID),             // id ascending
		)
	})
}

// FirstBelow returns the index of the first row whose priority is below p, in a
// slice already sorted by byUrgency (priority descending). It uses sort.Search's
// binary search over the monotonic predicate.
func FirstBelow(rows []Row, p int) int {
	return sort.Search(len(rows), func(i int) bool {
		return rows[i].Priority < p
	})
}
```

### The runnable demo

The demo sorts a small fixture and prints each row so you can see priority
descending with created-at breaking ties among equal priorities.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/reportsort"
)

func day(n int) time.Time {
	return time.Date(2024, 1, n, 0, 0, 0, 0, time.UTC)
}

func main() {
	rows := []reportsort.Row{
		{ID: "r3", Priority: 1, CreatedAt: day(3)},
		{ID: "r1", Priority: 5, CreatedAt: day(1)},
		{ID: "r2", Priority: 5, CreatedAt: day(2)},
	}
	reportsort.SortStable(rows)

	for _, r := range rows {
		fmt.Printf("%s p=%d %s\n", r.ID, r.Priority, r.CreatedAt.Format("2006-01-02"))
	}

	fmt.Printf("first row below priority 5 is at index %d\n", reportsort.FirstBelow(rows, 5))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
r1 p=5 2024-01-01
r2 p=5 2024-01-02
r3 p=1 2024-01-03
```

Plus the search line:

```
first row below priority 5 is at index 2
```

### Tests

`TestMultiKeyOrder` sorts a fixed fixture and asserts the exact id order.
`TestStability` sorts rows with a duplicated composite key and asserts their
`Source` order is preserved. `TestSearchIndex` asserts `FirstBelow` returns the
boundary index. `TestModernParity` sorts two copies of a shuffled fixture — one
with the `sort.Interface`, one with `SortModern` — and asserts identical results.

Create `reportsort_test.go`:

```go
package reportsort

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func day(n int) time.Time {
	return time.Date(2024, 1, n, 0, 0, 0, 0, time.UTC)
}

func TestMultiKeyOrder(t *testing.T) {
	t.Parallel()

	rows := []Row{
		{ID: "c", Priority: 1, CreatedAt: day(1)},
		{ID: "a", Priority: 5, CreatedAt: day(2)},
		{ID: "b", Priority: 5, CreatedAt: day(1)},
		{ID: "d", Priority: 1, CreatedAt: day(1)},
	}
	SortStable(rows)

	var got []string
	for _, r := range rows {
		got = append(got, r.ID)
	}
	want := []string{"b", "a", "c", "d"} // p5(day1), p5(day2), then p1 by id
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestStability(t *testing.T) {
	t.Parallel()

	rows := []Row{
		{ID: "x", Priority: 3, CreatedAt: day(1), Source: "first"},
		{ID: "x", Priority: 3, CreatedAt: day(1), Source: "second"},
		{ID: "x", Priority: 3, CreatedAt: day(1), Source: "third"},
	}
	SortStable(rows)

	got := []string{rows[0].Source, rows[1].Source, rows[2].Source}
	want := []string{"first", "second", "third"}
	if !slices.Equal(got, want) {
		t.Fatalf("stable sort reordered ties: %v, want %v", got, want)
	}
}

func TestSearchIndex(t *testing.T) {
	t.Parallel()

	rows := []Row{
		{ID: "a", Priority: 9, CreatedAt: day(1)},
		{ID: "b", Priority: 5, CreatedAt: day(1)},
		{ID: "c", Priority: 5, CreatedAt: day(2)},
		{ID: "d", Priority: 1, CreatedAt: day(1)},
	}
	SortStable(rows) // priorities become 9, 5, 5, 1

	// Priority < 6 first matches index 1 (the first p5 row).
	if idx := FirstBelow(rows, 6); idx != 1 {
		t.Fatalf("FirstBelow(6) = %d, want 1", idx)
	}
	// Priority < 5 first matches index 3 (the lone p1 row).
	if idx := FirstBelow(rows, 5); idx != 3 {
		t.Fatalf("FirstBelow(5) = %d, want 3", idx)
	}
	// No row has priority < 1, so the boundary is len(rows).
	if idx := FirstBelow(rows, 1); idx != len(rows) {
		t.Fatalf("FirstBelow(1) = %d, want %d", idx, len(rows))
	}
}

func TestModernParity(t *testing.T) {
	t.Parallel()

	base := []Row{
		{ID: "e", Priority: 2, CreatedAt: day(3)},
		{ID: "a", Priority: 5, CreatedAt: day(1)},
		{ID: "d", Priority: 2, CreatedAt: day(1)},
		{ID: "b", Priority: 5, CreatedAt: day(2)},
		{ID: "c", Priority: 5, CreatedAt: day(1)},
	}
	viaInterface := slices.Clone(base)
	viaModern := slices.Clone(base)

	SortStable(viaInterface)
	SortModern(viaModern)

	if !slices.EqualFunc(viaInterface, viaModern, func(a, b Row) bool { return a.ID == b.ID }) {
		t.Fatalf("sort.Interface and SortModern disagree:\n interface=%v\n modern=%v", ids(viaInterface), ids(viaModern))
	}
}

func ids(rows []Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

func ExampleSortStable() {
	rows := []Row{
		{ID: "lo", Priority: 1, CreatedAt: day(1)},
		{ID: "hi", Priority: 9, CreatedAt: day(1)},
	}
	SortStable(rows)
	fmt.Println(rows[0].ID, rows[1].ID)
	// Output: hi lo
}
```

## Review

The sort is correct when the fixed fixture lands in exactly the multi-key order,
ties keep their input order under `sort.Stable`, and the hand-written
`sort.Interface` produces the same result as the generic `SortModern`. Reach for
`slices.SortStableFunc` with `cmp.Or` by default — it makes the tiebreak chain
explicit and hard to get backwards. Keep `sort.Interface` for a named reusable
ordering, parallel-slice sorts, and `sort.Search`, and remember `sort.Sort` is not
stable, so use `sort.Stable`/`slices.SortStableFunc` when input order must survive
ties. Run `go test -race`.

## Resources

- [sort.Interface](https://pkg.go.dev/sort#Interface) — `Len`/`Less`/`Swap`, `sort.Sort`, `sort.Stable`, `sort.Search`.
- [slices.SortStableFunc](https://pkg.go.dev/slices#SortStableFunc) — the modern stable sort with a comparison function.
- [cmp.Or](https://pkg.go.dev/cmp#Or) and [cmp.Compare](https://pkg.go.dev/cmp#Compare) — building a multi-key comparison.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-text-marshaler-config.md](07-text-marshaler-config.md) | Next: [09-valuer-scanner-db-enum.md](09-valuer-scanner-db-enum.md)
