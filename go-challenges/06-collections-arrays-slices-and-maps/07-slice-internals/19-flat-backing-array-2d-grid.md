# Exercise 19: One Flat Backing Array Instead of N Row Slices

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A rate-limiting gateway that tracks request counts per shard and per
time bucket needs, at its core, a two-dimensional counter matrix. The
instinctive Go representation is `[][]int`: a slice of rows, each row its
own `make([]int, cols)`. It works, it reads naturally, and at any nontrivial
shard count or bucket count it is the wrong choice for exactly the reason
the standard library's own image types do not use it. `image.Gray` and
`image.RGBA` do not store `[][]uint8` either -- they store one flat `Pix
[]uint8` buffer plus a `Stride` field recording how many bytes separate the
start of one row from the next, and every pixel access is one multiplication
and one addition into that single array.

The difference is not stylistic. `[][]int` for an `R`-row matrix is `R+1`
separate heap allocations: one for the slice of row headers, and one for
each row's own backing array, scattered wherever the allocator happens to
place them. A flat `make([]int, rows*cols)` is one allocation, one
contiguous region a CPU cache line can span across, and one object for the
garbage collector to scan instead of `R+1`. None of this shows up in a unit
test that only checks values are stored and retrieved correctly -- both
representations pass that test identically. It shows up in an allocation
profile of a gateway processing shard counts in the thousands, and it shows
up as a measurable property this module pins directly rather than asserting
by hand.

This module builds `grid`, a fixed-size int matrix backed by one flat array,
with each row exposed as a bounded reslice of it rather than an independent
allocation. The N-row-slices version is not part of that API -- it lives
only in the test file, as the thing the allocation benchmark measures
against.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
grid/                    module example.com/grid
  go.mod                 go 1.24
  grid.go                Grid; New, At, Set, Row, Rows, Cols
  grid_test.go            cell table, row aliasing, capacity clip, the naive
                          N-row-slices allocation contrast, Example
```

- Files: `grid.go`, `grid_test.go`.
- Implement: `New(rows, cols int) (*Grid, error)` rejecting a negative dimension with `ErrInvalidDimensions` (zero is a valid, empty grid); `(*Grid).At(r, c int) (int, error)` and `(*Grid).Set(r, c int, v int) error` both returning `ErrOutOfRange` for coordinates outside the grid; `(*Grid).Row(r int) ([]int, error)` returning a three-index-clipped reslice of the flat backing array; `(*Grid).Rows()`/`Cols() int`.
- Test: the cell table (corners, an interior cell, four out-of-range variants); a zero-dimension grid accepted by `New` but rejected by `Row`; a write at the end of one row proven not to leak into the next; `Row`'s aliasing contract (writes through it visible via `At`); two consecutive rows proven to share one flat array; `append` on one row proven unable to spill into the next; a `newRowsNaive` contrast pinned with `testing.AllocsPerRun`, asserting the flat array allocates strictly fewer times than N independent row slices; and `Example` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Row-major arithmetic turns two indices into one flat offset

A flat matrix stores `rows*cols` elements in one array, row after row, and
recovers `(r, c)` with one formula: `data[r*cols + c]`. That is the entire
mechanism -- no per-row pointer, no per-row length, just a multiplication
and an addition into a single contiguous region. `image.Gray` names this
same offset `Stride` (bytes per row, since a pixel is a byte there) and
computes exactly the same `y*Stride + x` to find a pixel. It is not a
special trick this module invents; it is the standard layout for a
dense 2D array in any language without native multidimensional arrays,
and Go's `[]int` maps onto it with no extra bookkeeping beyond the row
width.

The alternative most people reach for first looks equally simple and pays
for that simplicity in allocations:

```go
func newRowsNaive(rows, cols int) [][]int {
    m := make([][]int, rows)
    for r := range m {
        m[r] = make([]int, cols) // one heap allocation per row
    }
    return m
}
```

Every row here is independent: its own `make` call, its own backing array,
its own entry in whatever the allocator's size classes decide. For `rows`
rows that is `rows + 1` total allocations against the flat version's one,
and it is `rows` separate objects the garbage collector has to visit on
every scan instead of one. Neither version is wrong in the sense of
producing incorrect values -- `newRowsNaive(r, c)[i][j] = x` and
`grid.Set(i, j, x)` both store `x` in the right logical cell. The flat
version is the one that scales, and the difference is exactly what
`testing.AllocsPerRun` is for: not a hand-counted number, but a property --
fewer allocations, asserted as a strict inequality, that holds regardless of
which Go release's specific size-class rounding is in effect.

Create `grid.go`:

```go
// Package grid implements a fixed-size rows-by-cols matrix of ints backed by
// a single flat []int array, the layout the standard library itself uses
// for image.Gray and image.RGBA (a Pix []uint8 buffer plus a Stride) instead
// of one []uint8 per scanline.
//
// A gateway's per-shard, per-time-bucket request-count matrix has the same
// shape problem: the naive representation is N independently allocated row
// slices, [][]int, which costs N heap objects, scatters rows across memory
// with poor cache locality, and gives the garbage collector N objects to
// scan instead of one. This package allocates the whole matrix as one
// make([]int, rows*cols) and exposes each row as a fixed-width reslice of
// it. See the package tests for a benchmark pinning the allocation-count
// property against the naive N-row-slices approach.
package grid

import (
	"errors"
	"fmt"
)

// ErrInvalidDimensions is returned by New for a negative rows or cols.
var ErrInvalidDimensions = errors.New("grid: rows and cols must not be negative")

// ErrOutOfRange is returned by At, Set, and Row for coordinates or a row
// index outside the grid's bounds.
var ErrOutOfRange = errors.New("grid: index out of range")

// Grid is a fixed-size rows x cols matrix of ints, backed by one flat
// []int array of length rows*cols in row-major order.
//
// Grid is not safe for concurrent use; a caller sharing one Grid across
// goroutines must guard it with its own lock.
type Grid struct {
	rows, cols int
	data       []int
}

// New returns a Grid of the given dimensions, with every cell initialized
// to zero. It returns ErrInvalidDimensions if rows or cols is negative;
// rows == 0 or cols == 0 is a valid, empty Grid.
func New(rows, cols int) (*Grid, error) {
	if rows < 0 || cols < 0 {
		return nil, fmt.Errorf("%w: got rows=%d cols=%d", ErrInvalidDimensions, rows, cols)
	}
	return &Grid{rows: rows, cols: cols, data: make([]int, rows*cols)}, nil
}

// Rows reports the number of rows.
func (g *Grid) Rows() int { return g.rows }

// Cols reports the number of columns.
func (g *Grid) Cols() int { return g.cols }

// At returns the value at (r, c). It returns ErrOutOfRange if r or c is
// outside the grid's bounds.
func (g *Grid) At(r, c int) (int, error) {
	i, err := g.index(r, c)
	if err != nil {
		return 0, err
	}
	return g.data[i], nil
}

// Set stores v at (r, c). It returns ErrOutOfRange if r or c is outside the
// grid's bounds.
func (g *Grid) Set(r, c int, v int) error {
	i, err := g.index(r, c)
	if err != nil {
		return err
	}
	g.data[i] = v
	return nil
}

// index converts a (row, col) pair to a flat offset into data, the same
// row-major arithmetic image.Gray uses with its Stride field.
func (g *Grid) index(r, c int) (int, error) {
	if r < 0 || r >= g.rows || c < 0 || c >= g.cols {
		return 0, fmt.Errorf("%w: (%d,%d) for a %dx%d grid", ErrOutOfRange, r, c, g.rows, g.cols)
	}
	return r*g.cols + c, nil
}

// Row returns row r as a slice view into the grid's single backing array.
// It returns ErrOutOfRange if r is outside [0, Rows()).
//
// The returned slice aliases this Grid's storage: writes through it are
// visible through At, and vice versa. It is capped to its own length via a
// three-index slice expression, so appending to it can never spill into
// row r+1 -- append is forced to allocate a fresh array instead of
// scribbling past the end of the row it was given.
func (g *Grid) Row(r int) ([]int, error) {
	if r < 0 || r >= g.rows {
		return nil, fmt.Errorf("%w: row %d for a %dx%d grid", ErrOutOfRange, r, g.rows, g.cols)
	}
	start := r * g.cols
	end := start + g.cols
	return g.data[start:end:end], nil
}
```

### Using it

Construct a `Grid` once its dimensions are known -- a shard count and a
bucket count decided at startup, for instance -- and use `Set`/`At` for
individual cell updates or `Row` when a caller genuinely needs a contiguous
view of one row, for example to hand a whole bucket's shard counts to
`sort.Ints` or to sum them with `slices.Reduce`-style code without an
intermediate copy. `Row`'s aliasing contract is exactly what makes that
useful and exactly what makes it dangerous if forgotten: writes through the
returned slice reach the grid's own storage, so a caller that wants an
independent snapshot must clone it explicitly.

`Grid` is not safe for concurrent use, stated on the type itself, the same
as any structure whose methods mutate shared state without their own
locking. `Example` is the runnable demonstration of this module: `go test`
executes it and compares its stdout against the `// Output:` comment below,
so the usage shown here cannot drift away from the code.

```go
func Example() {
	g, err := New(2, 3)
	if err != nil {
		panic(err)
	}
	for r := range g.Rows() {
		for c := range g.Cols() {
			if err := g.Set(r, c, r*10+c); err != nil {
				panic(err)
			}
		}
	}

	v, err := g.At(1, 2)
	if err != nil {
		panic(err)
	}
	fmt.Println("At(1,2) =", v)

	row0, err := g.Row(0)
	if err != nil {
		panic(err)
	}
	row1, err := g.Row(1)
	if err != nil {
		panic(err)
	}
	adjacent := (uintptr(basePointer(row1))-uintptr(basePointer(row0)))/unsafe.Sizeof(int(0)) == uintptr(g.Cols())
	fmt.Println("row 1 immediately follows row 0 in memory:", adjacent)

	// Output:
	// At(1,2) = 12
	// row 1 immediately follows row 0 in memory: true
}
```

### Tests

`TestAtAndSet` is the cell table: both corners, an interior cell, and four
out-of-range variants covering each side of both bounds.
`TestNewAllowsZeroDimensions` pins that a zero-row or zero-col grid is valid
construction but that `Row` still rejects any index into it.
`TestSetInOneRowDoesNotLeakIntoAnother` and
`TestRowClipsCapacitySoAppendCannotSpillIntoNextRow` are the two ways a flat
array could leak across a row boundary if the indexing or the clipping were
wrong; both are checked directly rather than assumed.
`TestRowsShareOneFlatBackingArray` uses `unsafe.SliceData` to prove two
rows are exactly `Cols()` ints apart in the same array, the same technique
`Example` uses for its own demonstration.

`TestFlatArrayAllocatesFewerTimesThanRowSlices` is the heart of the module.
`newRowsNaive` is unexported and unreachable from the package API; the test
measures both approaches with `testing.AllocsPerRun` and asserts only the
property `exact < naive`, never a specific count, because the runtime's
allocator size classes are not a contract this module should pin exactly.
Note this test does not call `t.Parallel`: `testing.AllocsPerRun` panics
when run from a parallel test, because a concurrent goroutine allocating in
the background would corrupt its measurement.

Create `grid_test.go`:

```go
package grid

import (
	"errors"
	"fmt"
	"testing"
	"unsafe"
)

// newRowsNaive builds a rows x cols matrix as N independent row slices, the
// way a grid is usually written the first time: one make call per row. Each
// row is its own heap allocation with its own backing array, unlike Grid's
// single flat array. It is never exported and never reachable from the
// package API; it exists only so the tests can pin the allocation-count
// difference numerically.
func newRowsNaive(rows, cols int) [][]int {
	m := make([][]int, rows)
	for r := range m {
		m[r] = make([]int, cols)
	}
	return m
}

func basePointer(s []int) unsafe.Pointer {
	return unsafe.Pointer(unsafe.SliceData(s))
}

func TestNewRejectsNegativeDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct{ rows, cols int }{
		{-1, 4}, {4, -1}, {-1, -1},
	}
	for _, tc := range tests {
		if _, err := New(tc.rows, tc.cols); !errors.Is(err, ErrInvalidDimensions) {
			t.Errorf("New(%d, %d) error = %v, want ErrInvalidDimensions", tc.rows, tc.cols, err)
		}
	}
}

func TestNewAllowsZeroDimensions(t *testing.T) {
	t.Parallel()

	g, err := New(0, 5)
	if err != nil {
		t.Fatalf("New(0, 5): %v", err)
	}
	if g.Rows() != 0 || g.Cols() != 5 {
		t.Fatalf("Rows()=%d Cols()=%d, want 0, 5", g.Rows(), g.Cols())
	}
	if _, err := g.Row(0); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("Row(0) on a zero-row grid: err = %v, want ErrOutOfRange", err)
	}
}

func TestAtAndSet(t *testing.T) {
	t.Parallel()

	g, err := New(3, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name    string
		r, c    int
		wantErr error
	}{
		{name: "top-left corner", r: 0, c: 0},
		{name: "bottom-right corner", r: 2, c: 3},
		{name: "interior cell", r: 1, c: 2},
		{name: "row out of range", r: 3, c: 0, wantErr: ErrOutOfRange},
		{name: "col out of range", r: 0, c: 4, wantErr: ErrOutOfRange},
		{name: "negative row", r: -1, c: 0, wantErr: ErrOutOfRange},
		{name: "negative col", r: 0, c: -1, wantErr: ErrOutOfRange},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := g.Set(tc.r, tc.c, 42)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Set(%d,%d) error = %v, want %v", tc.r, tc.c, err, tc.wantErr)
				}
				if _, err := g.At(tc.r, tc.c); !errors.Is(err, tc.wantErr) {
					t.Fatalf("At(%d,%d) error = %v, want %v", tc.r, tc.c, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%d,%d): %v", tc.r, tc.c, err)
			}
			got, err := g.At(tc.r, tc.c)
			if err != nil {
				t.Fatalf("At(%d,%d): %v", tc.r, tc.c, err)
			}
			if got != 42 {
				t.Fatalf("At(%d,%d) = %d, want 42", tc.r, tc.c, got)
			}
		})
	}
}

func TestSetInOneRowDoesNotLeakIntoAnother(t *testing.T) {
	t.Parallel()

	g, err := New(2, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := g.Set(0, 2, 99); err != nil {
		t.Fatalf("Set(0,2): %v", err)
	}
	got, err := g.At(1, 0)
	if err != nil {
		t.Fatalf("At(1,0): %v", err)
	}
	if got != 0 {
		t.Fatalf("At(1,0) = %d, want 0 (writing the end of row 0 must not touch row 1)", got)
	}
}

func TestRowAliasesGridStorage(t *testing.T) {
	t.Parallel()

	g, err := New(2, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	row, err := g.Row(1)
	if err != nil {
		t.Fatalf("Row(1): %v", err)
	}
	row[0] = 7

	got, err := g.At(1, 0)
	if err != nil {
		t.Fatalf("At(1,0): %v", err)
	}
	if got != 7 {
		t.Fatalf("At(1,0) = %d, want 7 (Row must alias the grid's storage)", got)
	}
}

func TestRowsShareOneFlatBackingArray(t *testing.T) {
	t.Parallel()

	g, err := New(3, 4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	row0, err := g.Row(0)
	if err != nil {
		t.Fatalf("Row(0): %v", err)
	}
	row1, err := g.Row(1)
	if err != nil {
		t.Fatalf("Row(1): %v", err)
	}

	// row1 must begin exactly Cols() ints after row0 begins: both are views
	// into the same array, laid out back to back, not two independent ones.
	gap := (uintptr(basePointer(row1)) - uintptr(basePointer(row0))) / unsafe.Sizeof(int(0))
	if gap != uintptr(g.Cols()) {
		t.Fatalf("gap between rows = %d ints, want %d (rows must share one flat array)", gap, g.Cols())
	}
}

func TestRowClipsCapacitySoAppendCannotSpillIntoNextRow(t *testing.T) {
	t.Parallel()

	g, err := New(2, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	row0, err := g.Row(0)
	if err != nil {
		t.Fatalf("Row(0): %v", err)
	}
	if cap(row0) != len(row0) {
		t.Fatalf("cap(row0) = %d, want %d (cap must equal len so append reallocates)", cap(row0), len(row0))
	}

	row0 = append(row0, 999) // would spill into row 1 if row0's cap exceeded its len
	got, err := g.At(1, 0)
	if err != nil {
		t.Fatalf("At(1,0): %v", err)
	}
	if got != 0 {
		t.Fatalf("At(1,0) = %d, want 0 (appending to row0 corrupted row 1)", got)
	}
	_ = row0
}

func TestRowOutOfRange(t *testing.T) {
	t.Parallel()

	g, err := New(2, 3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, r := range []int{-1, 2, 99} {
		if _, err := g.Row(r); !errors.Is(err, ErrOutOfRange) {
			t.Errorf("Row(%d) error = %v, want ErrOutOfRange", r, err)
		}
	}
}

// TestFlatArrayAllocatesFewerTimesThanRowSlices is the whole point of the
// module: it pins the allocation-count property between the flat-array
// Grid and the naive N-row-slices approach. The exact allocation counts are
// not asserted -- only that the flat array needs strictly fewer.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestFlatArrayAllocatesFewerTimesThanRowSlices(t *testing.T) {
	const rows, cols = 64, 64

	exact := testing.AllocsPerRun(100, func() {
		_, _ = New(rows, cols)
	})
	naive := testing.AllocsPerRun(100, func() {
		_ = newRowsNaive(rows, cols)
	})
	if !(exact < naive) {
		t.Fatalf("allocations: exact = %v, naive = %v; want exact < naive", exact, naive)
	}
}

// Example demonstrates building a grid, writing and reading cells, and that
// consecutive rows are adjacent views into one flat backing array.
func Example() {
	g, err := New(2, 3)
	if err != nil {
		panic(err)
	}
	for r := range g.Rows() {
		for c := range g.Cols() {
			if err := g.Set(r, c, r*10+c); err != nil {
				panic(err)
			}
		}
	}

	v, err := g.At(1, 2)
	if err != nil {
		panic(err)
	}
	fmt.Println("At(1,2) =", v)

	row0, err := g.Row(0)
	if err != nil {
		panic(err)
	}
	row1, err := g.Row(1)
	if err != nil {
		panic(err)
	}
	adjacent := (uintptr(basePointer(row1))-uintptr(basePointer(row0)))/unsafe.Sizeof(int(0)) == uintptr(g.Cols())
	fmt.Println("row 1 immediately follows row 0 in memory:", adjacent)

	// Output:
	// At(1,2) = 12
	// row 1 immediately follows row 0 in memory: true
}
```

## Review

`Grid` is correct when every cell maps to exactly one offset in a single
flat array via `r*cols + c`, and when `Row` hands out a bounded view of that
array rather than a copy -- both properties checked directly rather than
assumed: `TestSetInOneRowDoesNotLeakIntoAnother` for the indexing arithmetic,
`TestRowClipsCapacitySoAppendCannotSpillIntoNextRow` for the three-index
clip. The trap this module is built around is not a correctness bug at all
-- `[][]int` with N independent row allocations produces identical values to
the flat version under every functional test. It is an efficiency defect
that only an allocation profile or a benchmark like
`TestFlatArrayAllocatesFewerTimesThanRowSlices` surfaces, and that test
asserts a property, `exact < naive`, rather than a specific count, because
the runtime's own growth and size-class behavior is not part of the
contract. `New` validates its dimensions with `ErrInvalidDimensions`; `At`,
`Set`, and `Row` all report `ErrOutOfRange`, checkable with `errors.Is`;
`Grid` is explicitly not safe for concurrent use. `Example` is the
executable documentation: `go test` verifies its output. Run `go test
-count=1 -race ./...`.

## Resources

- [`image.Gray`](https://pkg.go.dev/image#Gray) — the standard library's own flat-buffer-plus-stride 2D layout this module's design mirrors.
- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions) — the three-index form used to clip each row's capacity to its length.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe used to pin the flat-array-versus-row-slices property, and its restriction against parallel tests.
- [`unsafe.SliceData`](https://pkg.go.dev/unsafe#SliceData) — the diagnostic used to prove two rows share one backing array.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-length-prefixed-frame-decoder.md](18-length-prefixed-frame-decoder.md) | Next: [../08-map-internals-and-iteration-order/00-concepts.md](../08-map-internals-and-iteration-order/00-concepts.md)
