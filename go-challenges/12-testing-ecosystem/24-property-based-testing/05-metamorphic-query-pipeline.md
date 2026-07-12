# Exercise 5: Metamorphic Properties of a Sort/Filter/Paginate Query Pipeline

Every read endpoint has the same shape: take a slice of records, filter by some
predicate, sort by one or more keys, then cut a page with offset and limit. There
is no oracle for "the right output" of the whole pipeline — the right output *is*
whatever the pipeline computes. When you cannot check a single input against a
known answer, you check *metamorphic* relations: known relationships between the
outputs of *related* inputs that must hold for any correct implementation. This
exercise builds the pipeline and asserts four metamorphic and invariant properties
with `pgregory.net/rapid`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
querypipe/                  independent module: example.com/querypipe
  go.mod                    go 1.26, requires pgregory.net/rapid
  querypipe.go              type Record; Sort, Filter, Page; ByScoreThenID comparator
  cmd/
    demo/
      main.go               runnable demo: filter, sort, and page a small dataset
  querypipe_test.go         rapid metamorphic + invariant properties
```

Files: `querypipe.go`, `cmd/demo/main.go`, `querypipe_test.go`.
Implement: `Sort` (stable, `cmp`-based total order), `Filter` (predicate), and `Page` (offset/limit) over a `Record` slice.
Test: rapid properties — sort is idempotent; filter and sort commute; concatenating all pages reconstructs the sorted-filtered result exactly; every page is sorted and no longer than the limit.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### Four properties for a pipeline with no oracle

The comparator establishes a *total* order — score descending, then ID ascending,
then name ascending — so no two distinct records compare equal. That matters: with
a total order, `slices.SortStableFunc` produces a unique result independent of input
order, which is what makes the metamorphic relations clean. `Sort` clones its input
(never mutating the caller's slice), `Filter` keeps the records matching a
predicate in their original relative order, and `Page` returns the half-open
`[offset, offset+limit)` window, clamped to the slice bounds.

Property 1 — idempotence of sort: `Sort(Sort(x))` equals `Sort(x)`. Sorting an
already-sorted slice must not reorder it. A comparator that is not a consistent
total order (returns inconsistent signs) breaks this immediately.

Property 2 — filter and sort commute: `Sort(Filter(x))` equals `Filter(Sort(x))`.
Filtering removes records without reordering; sorting orders by a key independent
of position. Because the order is total, both routes land on the same sequence:
the matching records, in sorted order. This is the metamorphic heart of the
exercise — you never compute the "expected" page, you only assert that two ways of
producing it agree.

Property 3 — pagination reconstructs the whole: stepping `offset` by `limit`
across the entire sorted-filtered result and concatenating the pages yields exactly
that result — every record once, in order, no gaps, no duplicates. This is the
property an off-by-one page boundary (`<=` versus `<`, `offset+limit` versus
`offset+limit-1`) violates, and rapid shrinks such a bug to a tiny record set and a
small limit.

Property 4 — output invariant: any page produced by the pipeline is itself sorted
per the comparator and never longer than the limit. `slices.IsSortedFunc` checks
the ordering directly.

Create `querypipe.go`:

```go
package querypipe

import (
	"cmp"
	"slices"
)

// Record is a row returned by a list endpoint.
type Record struct {
	ID     int
	Name   string
	Score  int
	Active bool
}

// ByScoreThenID is a total order: score descending, then ID ascending, then name
// ascending. Distinct records never compare equal, so the sort result is unique.
func ByScoreThenID(a, b Record) int {
	return cmp.Or(
		cmp.Compare(b.Score, a.Score),
		cmp.Compare(a.ID, b.ID),
		cmp.Compare(a.Name, b.Name),
	)
}

// Sort returns a sorted copy of recs, leaving the input untouched.
func Sort(recs []Record) []Record {
	out := slices.Clone(recs)
	slices.SortStableFunc(out, ByScoreThenID)
	return out
}

// Filter returns the records satisfying pred, in their original relative order.
func Filter(recs []Record, pred func(Record) bool) []Record {
	out := make([]Record, 0, len(recs))
	for _, r := range recs {
		if pred(r) {
			out = append(out, r)
		}
	}
	return out
}

// Page returns the half-open window [offset, offset+limit) of recs, clamped to the
// slice bounds. offset<0 is treated as 0; limit<=0 yields an empty page.
func Page(recs []Record, offset, limit int) []Record {
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || offset >= len(recs) {
		return nil
	}
	end := offset + limit
	if end > len(recs) {
		end = len(recs)
	}
	return recs[offset:end]
}
```

### The runnable demo

The demo runs the full pipeline over a small dataset: keep active records, sort
them, and take the first page of two.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/querypipe"
)

func main() {
	recs := []querypipe.Record{
		{ID: 1, Name: "alpha", Score: 30, Active: true},
		{ID: 2, Name: "bravo", Score: 50, Active: false},
		{ID: 3, Name: "charlie", Score: 50, Active: true},
		{ID: 4, Name: "delta", Score: 40, Active: true},
	}
	active := querypipe.Filter(recs, func(r querypipe.Record) bool { return r.Active })
	sorted := querypipe.Sort(active)
	page := querypipe.Page(sorted, 0, 2)
	for _, r := range page {
		fmt.Printf("%d %s score=%d\n", r.ID, r.Name, r.Score)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
3 charlie score=50
4 delta score=40
```

### The property tests

The record generator draws bounded integers and short names so datasets stay small
enough to shrink to a readable counterexample, and it deliberately allows duplicate
scores so that ordering ties actually occur and the total-order tiebreak is
exercised. The pagination property loops the page window across the whole result;
the invariant property checks each page independently.

Create `querypipe_test.go`:

```go
package querypipe

import (
	"fmt"
	"slices"
	"testing"

	"pgregory.net/rapid"
)

func genRecord() *rapid.Generator[Record] {
	return rapid.Custom(func(t *rapid.T) Record {
		return Record{
			ID:     rapid.IntRange(0, 20).Draw(t, "id"),
			Name:   rapid.StringN(0, 4, -1).Draw(t, "name"),
			Score:  rapid.IntRange(0, 5).Draw(t, "score"),
			Active: rapid.Bool().Draw(t, "active"),
		}
	})
}

func genRecords() *rapid.Generator[[]Record] {
	return rapid.SliceOf(genRecord())
}

func isActive(r Record) bool { return r.Active }

func TestSortIdempotent(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		recs := genRecords().Draw(t, "recs")
		once := Sort(recs)
		twice := Sort(once)
		if !slices.Equal(once, twice) {
			t.Fatalf("Sort not idempotent:\n once %+v\n twice %+v", once, twice)
		}
	})
}

func TestFilterSortCommute(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		recs := genRecords().Draw(t, "recs")
		a := Sort(Filter(recs, isActive))
		b := Filter(Sort(recs), isActive)
		if !slices.Equal(a, b) {
			t.Fatalf("filter and sort do not commute:\n Sort(Filter)=%+v\n Filter(Sort)=%+v", a, b)
		}
	})
}

func TestPagesReconstruct(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		recs := genRecords().Draw(t, "recs")
		limit := rapid.IntRange(1, 5).Draw(t, "limit")
		full := Sort(Filter(recs, isActive))

		var reassembled []Record
		for offset := 0; offset < len(full); offset += limit {
			reassembled = append(reassembled, Page(full, offset, limit)...)
		}
		if !slices.Equal(reassembled, full) {
			t.Fatalf("pages did not reconstruct the result:\n got  %+v\n want %+v", reassembled, full)
		}
	})
}

func TestPageIsSortedAndBounded(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		recs := genRecords().Draw(t, "recs")
		limit := rapid.IntRange(1, 5).Draw(t, "limit")
		offset := rapid.IntRange(0, 30).Draw(t, "offset")
		page := Page(Sort(Filter(recs, isActive)), offset, limit)

		if len(page) > limit {
			t.Fatalf("page length %d exceeds limit %d", len(page), limit)
		}
		if !slices.IsSortedFunc(page, ByScoreThenID) {
			t.Fatalf("page is not sorted: %+v", page)
		}
	})
}

func ExampleSort() {
	out := Sort([]Record{
		{ID: 2, Score: 10},
		{ID: 1, Score: 10},
	})
	fmt.Println(out[0].ID, out[1].ID)
	// Output: 1 2
}
```

## Review

The pipeline is correct when sort is idempotent, filter and sort commute, the pages
tile the sorted-filtered result exactly, and every page is sorted and bounded. These
four properties pin the whole pipeline without ever computing an "expected" output,
which is the entire point of metamorphic testing: you assert relationships between
related runs, not answers for single runs. The total-order comparator is what makes
the relations clean — with ties broken to a unique order, both routes through
`Sort(Filter)` and `Filter(Sort)` land on the same sequence.

The mistakes to avoid are subtle. First, do not mutate the caller's slice in `Sort`
or `Filter` — `Sort` clones, because a metamorphic test runs the same input through
two pipelines and an in-place sort would corrupt the second run. Second, do not use
a non-total comparator (say, score only) and then rely on the commute property: with
ties, a merely-stable sort still commutes, but any accidental instability makes the
two routes diverge and the property fails on a real ordering ambiguity — a total
order removes the ambiguity entirely. Third, watch the page-boundary arithmetic: the
reconstruct property is precisely the one an off-by-one (`end := offset+limit-1`, or
`offset >= len` versus `>`) violates, and it will shrink to a two-record set that
makes the bug obvious. Run `go test -race`.

## Resources

- [`slices` package](https://pkg.go.dev/slices) — `SortStableFunc`, `IsSortedFunc`, `Clone`, and `Equal`.
- [`cmp` package](https://pkg.go.dev/cmp) — `cmp.Compare` and `cmp.Or` for building a total-order comparator.
- [Metamorphic testing](https://www.hillelwayne.com/post/metamorphic-testing/) — the pattern of asserting relations between related inputs when there is no single-input oracle.

---

Back to [04-differential-oracle-parser.md](04-differential-oracle-parser.md) | Next: [06-stateful-lru-cache-model.md](06-stateful-lru-cache-model.md)
