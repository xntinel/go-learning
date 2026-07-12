# Exercise 2: Preallocating a Repository Result Slice From a Known Row Count

Every repository layer that scans rows into a slice faces the same choice: start
from `nil` and let `append` grow the slice by repeated reallocation, or reserve
the capacity up front from a known bound and let every append land in place. When
the query carries a `LIMIT` or the driver reports a row count, the second option
turns O(log n) allocations plus O(n) copying into a single allocation — a real,
measurable win on hot read paths.

This module is self-contained: its own module, demo, and tests.

## What you'll build

```text
repo/                      independent module: example.com/repo
  go.mod                   go 1.26
  repo.go                  Row; ScanAll (preallocated); scanNaive (contrast)
  cmd/
    demo/
      main.go              scan a fixed row set, print len and cap
  repo_test.go             correctness + AllocsPerRun single-alloc proof, Example
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: `ScanAll(src RowSource, expected int) []Row` using `make([]Row, 0, expected)`, and a naive `scanNaive` starting from `nil` for contrast.
Test: assert both return the same rows; use `testing.AllocsPerRun` to prove the preallocated path does one backing-array allocation while the naive path does several; assert final `len` and `cap >= expected`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why the preallocated path allocates once

A driver that knows how many rows it will return — because the query said
`LIMIT 500`, or because `SELECT count(*)` ran first, or because the result set
reports its size — hands you a bound. `make([]Row, 0, expected)` uses that bound to
allocate the backing array a single time, with length zero and capacity
`expected`. Every subsequent `append` finds `len < cap`, writes in place, and
never reallocates. The array is allocated exactly once, regardless of how many
rows you append (up to `expected`).

The naive path — `var rows []Row` then `append` in the loop — starts with a nil
backing array. The first append allocates a small array; when it fills, `append`
allocates a larger one and copies everything over; that repeats as the slice
doubles. For `n` rows you pay O(log n) allocations and, because each doubling
copies the whole prefix, O(n) total element copies. On a path that runs on every
request, that is wasted CPU and GC pressure for no benefit, since the size was
knowable.

The correctness of both paths is identical — same rows, same order. The only
difference is allocation behavior, which is exactly what the test measures. Note
that `expected` is a *hint*: if the real row count exceeds it, `append` still
grows correctly past `expected` (just with a reallocation at the boundary); if it
undershoots, you have slightly over-reserved. Sizing from a real bound makes the
common case allocation-free without making the uncommon case wrong.

Create `repo.go`:

```go
package repo

// Row is one scanned record.
type Row struct {
	ID   int
	Name string
}

// RowSource yields rows one at a time, like a *sql.Rows cursor. Next reports
// whether a row was produced.
type RowSource interface {
	Next() (Row, bool)
}

// ScanAll collects every row from src into a slice whose capacity is reserved
// from expected (e.g. a LIMIT or a count), so append fills in place and the
// backing array is allocated exactly once on the common path.
func ScanAll(src RowSource, expected int) []Row {
	if expected < 0 {
		expected = 0
	}
	rows := make([]Row, 0, expected)
	for {
		r, ok := src.Next()
		if !ok {
			return rows
		}
		rows = append(rows, r)
	}
}

// scanNaive is the un-preallocated contrast: it grows from nil, reallocating
// and copying as capacity is exhausted. Kept unexported to document the anti-
// pattern the test measures against.
func scanNaive(src RowSource) []Row {
	var rows []Row
	for {
		r, ok := src.Next()
		if !ok {
			return rows
		}
		rows = append(rows, r)
	}
}
```

### The runnable demo

The demo scans a fixed in-memory source of five rows with an accurate `expected`
of five, then prints the resulting length and capacity to show the reservation
held.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

// sliceSource is a trivial RowSource backed by a slice.
type sliceSource struct {
	rows []repo.Row
	i    int
}

func (s *sliceSource) Next() (repo.Row, bool) {
	if s.i >= len(s.rows) {
		return repo.Row{}, false
	}
	r := s.rows[s.i]
	s.i++
	return r, true
}

func main() {
	src := &sliceSource{rows: []repo.Row{
		{ID: 1, Name: "alice"},
		{ID: 2, Name: "bob"},
		{ID: 3, Name: "carol"},
		{ID: 4, Name: "dave"},
		{ID: 5, Name: "erin"},
	}}
	rows := repo.ScanAll(src, 5)
	fmt.Printf("scanned %d rows, cap=%d\n", len(rows), cap(rows))
	fmt.Printf("first=%s last=%s\n", rows[0].Name, rows[len(rows)-1].Name)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
scanned 5 rows, cap=5
first=alice last=erin
```

### Tests

`TestScanAllCorrect` checks the assembled slice equals the input and that
`cap >= expected`. `TestScanAllMatchesNaive` proves both paths produce identical
results, so preallocation is purely an optimization. `TestSingleAllocation` is the
core proof: `testing.AllocsPerRun` shows the preallocated path does one allocation
while the naive path does several. The result is assigned to a package-level sink
so the compiler cannot elide the allocation being measured.

Create `repo_test.go`:

```go
package repo

import (
	"fmt"
	"slices"
	"testing"
)

// countingSource yields a fixed number of generated rows and is re-usable via
// reset, so AllocsPerRun can run it repeatedly.
type countingSource struct {
	n int
	i int
}

func (s *countingSource) Next() (Row, bool) {
	if s.i >= s.n {
		return Row{}, false
	}
	r := Row{ID: s.i, Name: "row"}
	s.i++
	return r, true
}

func (s *countingSource) reset() { s.i = 0 }

var sink []Row

func TestScanAllCorrect(t *testing.T) {
	t.Parallel()
	src := &countingSource{n: 4}
	rows := ScanAll(src, 4)
	if len(rows) != 4 {
		t.Fatalf("len = %d, want 4", len(rows))
	}
	if cap(rows) < 4 {
		t.Fatalf("cap = %d, want >= 4", cap(rows))
	}
	want := []Row{{0, "row"}, {1, "row"}, {2, "row"}, {3, "row"}}
	if !slices.Equal(rows, want) {
		t.Fatalf("rows = %v, want %v", rows, want)
	}
}

func TestScanAllMatchesNaive(t *testing.T) {
	t.Parallel()
	a := ScanAll(&countingSource{n: 7}, 7)
	b := scanNaive(&countingSource{n: 7})
	if !slices.Equal(a, b) {
		t.Fatalf("ScanAll = %v, scanNaive = %v", a, b)
	}
}

func TestSingleAllocation(t *testing.T) {
	const n = 1000
	pre := &countingSource{n: n}
	prealloc := testing.AllocsPerRun(50, func() {
		pre.reset()
		sink = ScanAll(pre, n)
	})
	nai := &countingSource{n: n}
	naive := testing.AllocsPerRun(50, func() {
		nai.reset()
		sink = scanNaive(nai)
	})
	if prealloc > 1 {
		t.Errorf("preallocated path did %v allocations, want 1", prealloc)
	}
	if naive <= prealloc {
		t.Errorf("naive=%v not greater than prealloc=%v; growth should cost more", naive, prealloc)
	}
	t.Logf("prealloc=%v naive=%v", prealloc, naive)
}

func ExampleScanAll() {
	src := &countingSource{n: 3}
	rows := ScanAll(src, 3)
	fmt.Println(len(rows), cap(rows))
	// Output: 3 3
}
```

## Review

The preallocated `ScanAll` is correct when it returns the same rows in the same
order as the naive version and, on the common path where `expected` matches the
row count, allocates the backing array exactly once. `TestSingleAllocation`
encodes both halves: `prealloc <= 1` and `naive > prealloc`. The assertion is
written as an inequality rather than an exact count because the naive path's
allocation number depends on the growth factor, which is an implementation detail;
what is stable and meaningful is that preallocation collapses it to one. The
common failure here is sizing from something that is not actually a bound — an
optimistic guess that is usually too small still reallocates, so tie `expected` to
a real `LIMIT` or count. Run with `-race` to confirm nothing is shared.

## Resources

- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun)
- [Go blog: Go Slices: usage and internals](https://go.dev/blog/slices-intro)
- [Effective Go: Slices](https://go.dev/doc/effective_go#slices)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-ring-buffer-fixed-capacity.md](01-ring-buffer-fixed-capacity.md) | Next: [03-event-batcher-reuse-backing-array.md](03-event-batcher-reuse-backing-array.md)
