# Exercise 6: Stable Multi-Key Ordering For Cursor Pagination (SortStableFunc)

A paginated API endpoint that orders results by a non-unique key (status,
priority, score) must keep equal-key rows in a deterministic sequence, or cursor
pagination breaks: a row can jump across a page boundary between requests and be
served twice or skipped. This module builds the ordering function the right way
with `slices.SortStableFunc`, contrasts it against the non-guarantee of
`slices.SortFunc`, and shows the total-order alternative using `cmp.Or`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pagesort/                      module example.com/pagesort
  go.mod                       go 1.24
  pagesort.go                  type Row; OrderStable (SortStableFunc), OrderTotal (SortFunc+cmp.Or)
  cmd/
    demo/
      main.go                  runnable demo: sort by priority, ties keep seq order
  pagesort_test.go             stable preserves tie order; cmp.Or total order; page boundary stable
```

- Files: `pagesort.go`, `cmd/demo/main.go`, `pagesort_test.go`.
- Implement: `OrderStable([]Row)` sorting by `Priority` with `SortStableFunc` so equal-priority rows keep insertion order; `OrderTotal([]Row)` sorting by `Priority` then `Seq` with `SortFunc` + `cmp.Or` for the same result via a total order.
- Test: many equal-priority rows keep their original `Seq` order under `OrderStable`; `OrderTotal` produces the identical ordering; a page-boundary slice is stable when the full set is re-sorted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/06-stable-multikey-pagination-sort/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/09-slices-package/06-stable-multikey-pagination-sort
go mod edit -go=1.24
```

### Why stability is a pagination correctness property

Cursor pagination serves results in pages by remembering a cursor (the last row
of the previous page) and asking for "the next N after this cursor". That only
works if the total ordering is deterministic and identical across requests. If the
sort orders by `Priority` alone and two rows share a priority, an unstable sort may
place them in either order — and worse, may place them differently on two separate
calls over the same data. A row that was the last of page 1 on the first request
can, on the next request, land as the first of page 2, so the client sees it
twice; or a row can slip from page 2's front to page 1's tail and be skipped
entirely. The bug is intermittent and data-dependent, the worst kind.

`slices.SortFunc` documents that it is NOT guaranteed stable: equal elements may be
reordered. `slices.SortStableFunc` guarantees equal elements retain their original
relative order. So `OrderStable` sorts by `Priority` with `SortStableFunc`, and
because the rows arrive in `Seq` order, equal-priority rows stay in `Seq` order —
deterministic across every call. This is the direct fix.

The alternative, often preferable because it does not depend on input order, is to
make the comparator a *total order*: compare `Priority`, and when equal, break the
tie on the unique `Seq`. `cmp.Or(a, b, ...)` returns the first non-zero of its
arguments, which expresses "primary key, then tiebreak" cleanly:
`cmp.Or(cmp.Compare(x.Priority, y.Priority), cmp.Compare(x.Seq, y.Seq))`. With a
total order no two elements are ever equal, so even the unstable `SortFunc`
produces a single deterministic ordering. `OrderTotal` uses exactly this and the
test proves it matches `OrderStable`.

Create `pagesort.go`:

```go
package pagesort

import (
	"cmp"
	"slices"
)

// Row is an API result row ordered by Priority; Seq is the unique insertion id
// used to keep equal-priority rows deterministic.
type Row struct {
	Seq      int
	Priority int
	Name     string
}

// OrderStable sorts by Priority only, relying on SortStableFunc to keep
// equal-priority rows in their original (Seq) order.
func OrderStable(rows []Row) {
	slices.SortStableFunc(rows, func(a, b Row) int {
		return cmp.Compare(a.Priority, b.Priority)
	})
}

// OrderTotal sorts by a total order (Priority, then Seq) so even the non-stable
// SortFunc is deterministic. cmp.Or returns the first non-zero comparison.
func OrderTotal(rows []Row) {
	slices.SortFunc(rows, func(a, b Row) int {
		return cmp.Or(
			cmp.Compare(a.Priority, b.Priority),
			cmp.Compare(a.Seq, b.Seq),
		)
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/pagesort"
)

func main() {
	rows := []pagesort.Row{
		{Seq: 0, Priority: 2, Name: "a"},
		{Seq: 1, Priority: 1, Name: "b"},
		{Seq: 2, Priority: 2, Name: "c"},
		{Seq: 3, Priority: 1, Name: "d"},
		{Seq: 4, Priority: 2, Name: "e"},
	}
	pagesort.OrderStable(rows)
	for _, r := range rows {
		fmt.Printf("prio=%d seq=%d name=%s\n", r.Priority, r.Seq, r.Name)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
prio=1 seq=1 name=b
prio=1 seq=3 name=d
prio=2 seq=0 name=a
prio=2 seq=2 name=c
prio=2 seq=4 name=e
```

Within each priority group the rows keep ascending `Seq` order — the property a
cursor depends on. Priority 1 holds seq 1 then 3; priority 2 holds seq 0, 2, 4.

### Tests

`TestStablePreservesTieOrder` builds many equal-priority rows and asserts their
`Seq` order is preserved. `TestTotalOrderMatchesStable` proves `OrderTotal`
produces the identical ordering. `TestPageBoundaryStable` slices the sorted result
into pages and re-sorts the full set, asserting the boundary row does not move.

Create `pagesort_test.go`:

```go
package pagesort

import (
	"slices"
	"testing"
)

func seqs(rows []Row) []int {
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.Seq
	}
	return out
}

func TestStablePreservesTieOrder(t *testing.T) {
	t.Parallel()

	// All same priority: stable sort must preserve the input Seq order exactly.
	rows := make([]Row, 8)
	for i := range rows {
		rows[i] = Row{Seq: i, Priority: 5}
	}
	OrderStable(rows)
	if !slices.Equal(seqs(rows), []int{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("stable tie order = %v, want 0..7", seqs(rows))
	}
}

func TestTotalOrderMatchesStable(t *testing.T) {
	t.Parallel()

	mk := func() []Row {
		return []Row{
			{Seq: 0, Priority: 2}, {Seq: 1, Priority: 1}, {Seq: 2, Priority: 2},
			{Seq: 3, Priority: 1}, {Seq: 4, Priority: 3}, {Seq: 5, Priority: 1},
		}
	}
	a, b := mk(), mk()
	OrderStable(a)
	OrderTotal(b)
	if !slices.Equal(seqs(a), seqs(b)) {
		t.Fatalf("OrderTotal %v != OrderStable %v", seqs(b), seqs(a))
	}
}

func TestPageBoundaryStable(t *testing.T) {
	t.Parallel()

	full := []Row{
		{Seq: 0, Priority: 1}, {Seq: 1, Priority: 1}, {Seq: 2, Priority: 1},
		{Seq: 3, Priority: 2}, {Seq: 4, Priority: 2},
	}
	OrderStable(full)
	pageSize := 2
	firstPageLast := full[pageSize-1] // last row of page 1

	// Re-sort a fresh copy of the same data: the page boundary row is identical.
	again := slices.Clone(full)
	OrderStable(again)
	if again[pageSize-1] != firstPageLast {
		t.Fatalf("page boundary moved: %+v vs %+v", again[pageSize-1], firstPageLast)
	}
}
```

## Review

The ordering is correct for pagination when it is deterministic and identical
across calls over the same data, which `SortStableFunc` guarantees for equal-key
rows and `SortFunc` does not. `OrderTotal` reaches the same guarantee by making the
comparator a total order with `cmp.Or` on the unique `Seq`, which is the more
robust choice because it does not depend on the input already being in `Seq` order.
The page-boundary test encodes the actual failure mode: if a boundary row could
move between two sorts of the same data, cursor pagination would double-serve or
skip it. Run `go test -race`; each test owns its rows.

## Resources

- [`slices.SortStableFunc`](https://pkg.go.dev/slices#SortStableFunc) — guaranteed stable sort.
- [`slices.SortFunc`](https://pkg.go.dev/slices#SortFunc) — documents the no-stability guarantee.
- [`cmp.Or`](https://pkg.go.dev/cmp#Or) — first non-zero, for composing a total order.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-batch-writer-chunk-iter.md](07-batch-writer-chunk-iter.md)
