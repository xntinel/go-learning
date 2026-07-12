# Exercise 17: A Flat Arena-Backed Matrix With Three-Index Row Slices

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A `[][]float64` built the obvious way — one `make([]float64, w)` per row —
does `h` separate heap allocations for an `h`-row matrix, each one a
candidate for a cache-unfriendly jump when you walk the matrix row by row,
and each one adding its own bookkeeping overhead to the garbage collector.
Numeric code that cares about throughput (a feature matrix for a model, an
adjacency matrix for a graph algorithm, a tile buffer for image processing)
instead allocates one flat arena — `make([]float64, w*h)` — and carves out
row views from it with three-index slice expressions. This gives you the
same `[][]float64` call-site ergonomics (`m.Row(r)[c]`) with one allocation
for the data instead of `h`, elements stored genuinely contiguously in row-
major order, and the same three-index safety property Exercise 15 used for
upload chunks: no row's `append` can spill into the next row.

This module builds the matrix as a package you can drop into numeric code:
`New` validates its dimensions and returns an error, `At` and `Set` reject
out-of-range indices instead of panicking, and `Row` returns a view whose
capacity is capped at its own length. The naive per-row construction, and
the naive two-index row view that would let `append` spill between rows, are
not part of that API. They live in the test file, where they belong, as the
things the tests prove wrong.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
arenamatrix/                  module example.com/arenamatrix
  go.mod                      go 1.24
  arenamatrix.go                Matrix, sentinel errors; New, Width, Height, At, Set, Row, Rows
  arenamatrix_test.go            bounds checks, contiguity, no overlap, append isolation, two contrasts,
                                 ExampleMatrix_Row
```

- Files: `arenamatrix.go`, `arenamatrix_test.go`.
- Implement: `Matrix` holding one flat `[]float64` arena of size `w*h` plus a `[][]float64` of row views built with `data[start:end:end]` in `New(w, h int) (*Matrix, error)`, rejecting non-positive dimensions with `ErrInvalidDimensions`; `At(r, c int) (float64, error)` and `Set(r, c int, v float64) error` rejecting out-of-range indices with `ErrIndexOutOfRange`; `Row(r int) []float64`, `Rows() [][]float64`, `Width()`, `Height()`.
- Test: bounds checks on `At`/`Set`; every row's first element sits at the exact offset `r*w` into the shared arena, proving contiguity; writing through one row is invisible through every other row; appending onto a row cannot change the next row's first element; a `buildNaivePerRow` contrast proving the per-row loop allocates more than the arena; a `rowNaiveTwoIndex` contrast proving a two-index row view lets `append` spill into the next row while the real `Row` does not; and `ExampleMatrix_Row` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### One allocation for the data, three-index slices for the rows

The naive `[][]float64` construction — a loop doing `rows[r] = make([]float64,
w)` — allocates the row index slice once and then allocates each row's
backing array separately, `h` times. Every one of those `h` allocations is
independent: nothing guarantees row `r` and row `r+1` end up adjacent in
memory, so a loop that walks the whole matrix row by row, column by column,
jumps between `h` unrelated heap objects instead of streaming through one
contiguous block — the exact access pattern that defeats CPU cache
prefetching in numeric code.

The arena fixes both problems in one move. `data := make([]float64, w*h)`
is a single allocation for every element in the matrix, laid out row-major
by construction: element `(r, c)` lives at `data[r*w+c]`. Building
`rows[r] = data[start:end:end]` for `start, end := r*w, r*w+w` does not
allocate at all — a slice expression is just a new three-word header over
existing memory — so the whole `[][]float64` costs one allocation for `data`
plus one allocation for the `rows` index slice itself, a constant total no
matter how large `h` gets. The three-index form matters here for the same
reason it did for upload chunks: a plain two-index `data[start:end]` would
give a row a capacity that runs to the end of the *entire arena*, not to the
end of that row, so a caller who appends onto one row would silently start
overwriting the next row's values. `data[start:end:end]` caps the row's
capacity at its own length, so the first append past that length is forced
to allocate a fresh array instead of reaching into the next row's memory.

Create `arenamatrix.go`:

```go
// Package arenamatrix implements a dense matrix backed by a single flat
// allocation. Every row is a three-index slice into that shared arena, so
// the whole matrix costs a constant number of allocations regardless of its
// row count, and no row's append can spill into its neighbor.
package arenamatrix

import (
	"errors"
	"fmt"
)

// ErrInvalidDimensions means New was called with a non-positive width or
// height.
var ErrInvalidDimensions = errors.New("arenamatrix: width and height must be positive")

// ErrIndexOutOfRange means At or Set was called with a row or column outside
// the matrix's bounds.
var ErrIndexOutOfRange = errors.New("arenamatrix: index out of range")

// Matrix is a dense w-by-h matrix of float64 backed by a single flat
// allocation. All w*h elements live in one contiguous []float64 arena; each
// row is a three-index slice into that arena (data[start:end:end]), so every
// row's capacity equals its length and rows never overlap.
//
// A Matrix is not safe for concurrent use. Concurrent Set calls, or a Set
// racing a Row/At/Rows read, must be synchronized by the caller.
type Matrix struct {
	data []float64
	w, h int
	rows [][]float64
}

// New allocates a w-by-h matrix. The element storage is a single
// make([]float64, w*h) arena -- never a separate make per row -- and each
// row view is carved out of it with a three-index expression, so New does a
// constant number of allocations no matter how many rows h is. New returns
// ErrInvalidDimensions if w or h is not positive.
func New(w, h int) (*Matrix, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("%w: got w=%d h=%d", ErrInvalidDimensions, w, h)
	}
	data := make([]float64, w*h)
	rows := make([][]float64, h)
	for r := 0; r < h; r++ {
		start := r * w
		end := start + w
		rows[r] = data[start:end:end]
	}
	return &Matrix{data: data, w: w, h: h, rows: rows}, nil
}

// Width reports the number of columns.
func (m *Matrix) Width() int { return m.w }

// Height reports the number of rows.
func (m *Matrix) Height() int { return m.h }

// At returns the element at row r, column c. It returns ErrIndexOutOfRange
// if either index is outside the matrix's bounds.
func (m *Matrix) At(r, c int) (float64, error) {
	if r < 0 || r >= m.h || c < 0 || c >= m.w {
		return 0, fmt.Errorf("%w: At(%d,%d) in a %dx%d matrix", ErrIndexOutOfRange, r, c, m.w, m.h)
	}
	return m.rows[r][c], nil
}

// Set writes v into row r, column c. It returns ErrIndexOutOfRange if either
// index is outside the matrix's bounds, leaving the matrix unchanged.
func (m *Matrix) Set(r, c int, v float64) error {
	if r < 0 || r >= m.h || c < 0 || c >= m.w {
		return fmt.Errorf("%w: Set(%d,%d) in a %dx%d matrix", ErrIndexOutOfRange, r, c, m.w, m.h)
	}
	m.rows[r][c] = v
	return nil
}

// Row returns the r-th row as a three-index slice: cap(row) == len(row) ==
// Width(), so a caller that appends to the returned row cannot spill into
// row r+1's storage -- the append is forced to allocate its own array the
// moment it exceeds the row's length. The returned slice aliases the
// matrix's internal arena for reads and in-bounds writes; grow it with
// append only if isolation from the matrix is what you want. It returns nil
// if r is outside the matrix's bounds.
func (m *Matrix) Row(r int) []float64 {
	if r < 0 || r >= m.h {
		return nil
	}
	return m.rows[r]
}

// Rows returns the full set of row views, in row order. The returned slice
// of slices, and each row within it, aliases the matrix's internal arena.
func (m *Matrix) Rows() [][]float64 { return m.rows }
```

### Using it

Construct a `Matrix` once with `New(w, h)` and use `Set`/`At` for
element-level access or `Row`/`Rows` when you need a view to hand to code
that expects a `[]float64` or `[][]float64` — a BLAS-style routine, a JSON
encoder, a row-wise reducer. `New` validates the dimensions for you, and
`At`/`Set` return `ErrIndexOutOfRange` instead of panicking on a bad index,
so a caller driven by untrusted input (a deserialized row/column pair) can
handle the failure instead of crashing the process.

The aliasing contract is explicit on `Row`: the returned slice's capacity is
capped at its own length, so a caller that appends past it is forced into a
fresh allocation rather than silently corrupting the next row — the module's
`TestAppendToRowDoesNotSpillIntoNextRow` and the sharper
`TestNaiveTwoIndexRowLetsAppendSpillIntoTheNextRow` both pin that directly.
The module has no `main.go`, because a matrix type is a library, not a tool.
Its executable demonstration is `ExampleMatrix_Row`: `go test` runs it and
compares its standard output against the `// Output:` comment, so the usage
shown below cannot drift away from the code.

```go
func ExampleMatrix_Row() {
	m, err := New(4, 3)
	if err != nil {
		panic(err)
	}
	n := 0
	for r := 0; r < m.Height(); r++ {
		for c := 0; c < m.Width(); c++ {
			_ = m.Set(r, c, float64(n))
			n++
		}
	}

	for r := 0; r < m.Height(); r++ {
		row := m.Row(r)
		fmt.Printf("row %d: %v (len=%d cap=%d)\n", r, row, len(row), cap(row))
	}

	before, _ := m.At(1, 0)
	grown := append(m.Row(0), 999)
	after, _ := m.At(1, 0)
	fmt.Printf("row 1 first element before=%v after=%v unchanged=%v\n", before, after, before == after)
	fmt.Printf("grown row 0: %v\n", grown)

	// Output:
	// row 0: [0 1 2 3] (len=4 cap=4)
	// row 1: [4 5 6 7] (len=4 cap=4)
	// row 2: [8 9 10 11] (len=4 cap=4)
	// row 1 first element before=4 after=4 unchanged=true
	// grown row 0: [0 1 2 3 999]
}
```

Every row prints `cap == len`, confirming the three-index split. The values
run sequentially across the whole matrix (`0..11`) precisely because `Set`
writes into one shared, row-major arena rather than into per-row arrays that
happen to hold the right numbers in some unrelated order. Appending past row
0 leaves row 1's first element (`4`) untouched.

### Tests

`TestNewRejectsNonPositiveDimensions` and `TestAtSetRejectOutOfRange` cover
the constructor and accessor validation. `TestRowsAreContiguousInArena`
checks, with `unsafe.Pointer` address comparisons, that row `r`'s first
element sits at exactly `w*r` elements into the shared arena — direct proof
of row-major contiguity, not just correct values. `TestRowsDoNotOverlap`
writes through every row and confirms no write is visible through any other
row. `TestAppendToRowDoesNotSpillIntoNextRow` appends onto row 0 and asserts
row 1's data is unchanged.

`TestNaivePerRowAllocatesMoreThanTheArena` contrasts the arena against the
unexported `buildNaivePerRow` helper: it asserts the property `arena <
naive`, never a specific count, since the exact allocation counts a runtime
produces are not a documented contract.
`TestNaiveTwoIndexRowLetsAppendSpillIntoTheNextRow` is the sharpest test in
the module: it appends onto the unexported `rowNaiveTwoIndex` helper's
two-index view and shows the write lands inside row 1's storage, then
repeats the same append against the real, three-index `Row` and shows it
does not — the three-index expression earning its keep in one direct,
side-by-side comparison.

Create `arenamatrix_test.go`:

```go
package arenamatrix

import (
	"errors"
	"fmt"
	"testing"
	"unsafe"
)

func TestNewRejectsNonPositiveDimensions(t *testing.T) {
	t.Parallel()

	tests := []struct{ w, h int }{
		{0, 3}, {3, 0}, {-1, 3}, {3, -1},
	}
	for _, tc := range tests {
		if _, err := New(tc.w, tc.h); !errors.Is(err, ErrInvalidDimensions) {
			t.Errorf("New(%d,%d) error = %v, want ErrInvalidDimensions", tc.w, tc.h, err)
		}
	}
}

func TestAtSetRejectOutOfRange(t *testing.T) {
	t.Parallel()

	m, err := New(3, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct{ r, c int }{
		{-1, 0}, {0, -1}, {2, 0}, {0, 3}, {2, 3},
	}
	for _, tc := range tests {
		if _, err := m.At(tc.r, tc.c); !errors.Is(err, ErrIndexOutOfRange) {
			t.Errorf("At(%d,%d) error = %v, want ErrIndexOutOfRange", tc.r, tc.c, err)
		}
		if err := m.Set(tc.r, tc.c, 1); !errors.Is(err, ErrIndexOutOfRange) {
			t.Errorf("Set(%d,%d) error = %v, want ErrIndexOutOfRange", tc.r, tc.c, err)
		}
	}
}

func TestRowsAreContiguousInArena(t *testing.T) {
	t.Parallel()

	const w, h = 5, 4
	m, err := New(w, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n := 0
	for r := 0; r < h; r++ {
		for c := 0; c < w; c++ {
			if err := m.Set(r, c, float64(n)); err != nil {
				t.Fatalf("Set(%d,%d): %v", r, c, err)
			}
			n++
		}
	}

	// Row r's first element must sit exactly w*r elements into the shared
	// arena: proof that rows are laid out contiguously and in order, not
	// scattered across separate allocations.
	for r := 0; r < h; r++ {
		gotAddr := unsafe.Pointer(&m.Row(r)[0])
		wantAddr := unsafe.Pointer(&m.data[r*w])
		if gotAddr != wantAddr {
			t.Errorf("row %d starts at %p, want %p (offset %d into the arena)", r, gotAddr, wantAddr, r*w)
		}
	}
}

func TestRowsDoNotOverlap(t *testing.T) {
	t.Parallel()

	const w, h = 3, 3
	m, err := New(w, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for r := 0; r < h; r++ {
		for c := 0; c < w; c++ {
			_ = m.Set(r, c, float64(r*10+c))
		}
	}

	// Writing through one row must not be visible through any other row.
	_ = m.Set(1, 0, 777)
	for r := 0; r < h; r++ {
		for c := 0; c < w; c++ {
			if r == 1 && c == 0 {
				continue
			}
			want := float64(r*10 + c)
			got, err := m.At(r, c)
			if err != nil {
				t.Fatalf("At(%d,%d): %v", r, c, err)
			}
			if got != want {
				t.Errorf("At(%d,%d) = %v, want %v (a write to row 1 leaked into another row)", r, c, got, want)
			}
		}
	}
	got, _ := m.At(1, 0)
	if got != 777 {
		t.Fatalf("At(1,0) = %v, want 777", got)
	}
}

func TestAppendToRowDoesNotSpillIntoNextRow(t *testing.T) {
	t.Parallel()

	const w, h = 4, 3
	m, err := New(w, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for r := 0; r < h; r++ {
		for c := 0; c < w; c++ {
			_ = m.Set(r, c, float64(r*10+c))
		}
	}

	row0 := m.Row(0)
	if cap(row0) != w {
		t.Fatalf("cap(Row(0)) = %d, want %d (three-index split must cap at row length)", cap(row0), w)
	}

	before, _ := m.At(1, 0)
	grown := append(row0, 999)
	after, _ := m.At(1, 0)

	if before != after {
		t.Errorf("row 1, col 0 changed from %v to %v after appending to row 0", before, after)
	}
	if len(grown) != w+1 || grown[w] != 999 {
		t.Errorf("grown row 0 = %v, want length %d with 999 appended", grown, w+1)
	}
}

// buildNaivePerRow is the construction every hand-rolled [][]float64 starts
// life as: one make call for the row index, then one more make call per row.
// It is unexported and lives only here, never in arenamatrix.go, because it
// throws away the two properties the arena exists to guarantee: it costs h
// separate heap allocations instead of one, and nothing places those h
// allocations next to each other in memory, so a row-major walk jumps
// between unrelated objects instead of streaming through one block.
func buildNaivePerRow(w, h int) [][]float64 {
	rows := make([][]float64, h)
	for r := 0; r < h; r++ {
		rows[r] = make([]float64, w)
	}
	return rows
}

// rowNaiveTwoIndex reproduces the row view arenamatrix.Row deliberately does
// not: a plain two-index slice into the shared arena, whose capacity runs to
// the end of the *entire* arena rather than stopping at the row's own
// length. It is unexported and lives only here to demonstrate, by contrast,
// exactly the spill that Row's three-index expression prevents.
func rowNaiveTwoIndex(m *Matrix, r int) []float64 {
	start := r * m.w
	end := start + m.w
	return m.data[start:end]
}

// TestNaivePerRowAllocatesMoreThanTheArena shows the allocation cost of the
// naive construction. The exact count the arena performs is not asserted --
// only that it stays strictly below the naive per-row loop's, which grows
// with h. This test does not call t.Parallel: testing.AllocsPerRun panics
// when run from a parallel test, because a concurrent goroutine allocating
// in the background would corrupt the measurement.
func TestNaivePerRowAllocatesMoreThanTheArena(t *testing.T) {
	const w, h = 32, 32

	var arena *Matrix
	arenaAllocs := testing.AllocsPerRun(20, func() {
		arena, _ = New(w, h)
	})
	var naive [][]float64
	naiveAllocs := testing.AllocsPerRun(20, func() {
		naive = buildNaivePerRow(w, h)
	})

	if !(arenaAllocs < naiveAllocs) {
		t.Fatalf("allocations: arena = %v, naive = %v; want arena < naive", arenaAllocs, naiveAllocs)
	}
	_ = arena
	_ = naive
}

// TestNaiveTwoIndexRowLetsAppendSpillIntoTheNextRow is the sharpest contrast
// in the module: it appends onto the naive two-index row view and shows the
// write lands inside row r+1's storage, then appends onto the real
// arenamatrix.Row view over the identical data and shows it does not.
func TestNaiveTwoIndexRowLetsAppendSpillIntoTheNextRow(t *testing.T) {
	t.Parallel()

	const w, h = 4, 3
	m, err := New(w, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for r := 0; r < h; r++ {
		for c := 0; c < w; c++ {
			_ = m.Set(r, c, float64(r*10+c))
		}
	}

	naiveRow0 := rowNaiveTwoIndex(m, 0)
	if cap(naiveRow0) == w {
		t.Fatalf("cap(naive row 0) = %d, want > %d (a two-index slice's capacity runs to the end of the arena)", cap(naiveRow0), w)
	}
	_ = append(naiveRow0, 999)
	spilled, _ := m.At(1, 0)
	if spilled != 999 {
		t.Fatalf("row 1, col 0 = %v, want 999 (appending onto the naive two-index row must spill into row 1)", spilled)
	}

	// Rebuild a clean matrix and repeat with the real, three-index Row.
	m2, err := New(w, h)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for r := 0; r < h; r++ {
		for c := 0; c < w; c++ {
			_ = m2.Set(r, c, float64(r*10+c))
		}
	}
	before, _ := m2.At(1, 0)
	_ = append(m2.Row(0), 999)
	after, _ := m2.At(1, 0)
	if before != after {
		t.Fatalf("row 1, col 0 changed from %v to %v after appending to the real Row(0); the three-index split must prevent this", before, after)
	}
}

// ExampleMatrix_Row is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleMatrix_Row() {
	m, err := New(4, 3)
	if err != nil {
		panic(err)
	}
	n := 0
	for r := 0; r < m.Height(); r++ {
		for c := 0; c < m.Width(); c++ {
			_ = m.Set(r, c, float64(n))
			n++
		}
	}

	for r := 0; r < m.Height(); r++ {
		row := m.Row(r)
		fmt.Printf("row %d: %v (len=%d cap=%d)\n", r, row, len(row), cap(row))
	}

	// Appending to row 0 must not spill into row 1, because row 0's
	// capacity was capped at its length by the three-index split.
	before, _ := m.At(1, 0)
	grown := append(m.Row(0), 999)
	after, _ := m.At(1, 0)
	fmt.Printf("row 1 first element before=%v after=%v unchanged=%v\n", before, after, before == after)
	fmt.Printf("grown row 0: %v\n", grown)

	// Output:
	// row 0: [0 1 2 3] (len=4 cap=4)
	// row 1: [4 5 6 7] (len=4 cap=4)
	// row 2: [8 9 10 11] (len=4 cap=4)
	// row 1 first element before=4 after=4 unchanged=true
	// grown row 0: [0 1 2 3 999]
}
```

## Review

The arena is correct when it costs a constant, small number of allocations
regardless of `h` — `TestNaivePerRowAllocatesMoreThanTheArena` pins the
property directly against the naive per-row loop — and when every row is
both genuinely contiguous with its neighbors (`TestRowsAreContiguousInArena`)
and fully isolated from them for both plain writes (`TestRowsDoNotOverlap`)
and `append` (`TestAppendToRowDoesNotSpillIntoNextRow`,
`TestNaiveTwoIndexRowLetsAppendSpillIntoTheNextRow`). The two append-isolation
tests matter for different reasons: the first confirms `Row` behaves
correctly on its own, and the second proves it head-to-head against the
naive two-index alternative that a first attempt at this type would very
plausibly write. `New` and the accessors reject invalid input with
`ErrInvalidDimensions` and `ErrIndexOutOfRange`, both checkable with
`errors.Is`. `AllocsPerRun` cannot run inside a parallel test, so that one
test is serial; the rest use `t.Parallel()`. Run
`go test -count=1 -race ./...` to confirm.

## Resources

- [Slice expressions — the Go spec](https://go.dev/ref/spec#Slice_expressions) — the three-index form used for every row.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measures the allocation count `New` does per call, contrasted against the naive per-row loop.
- [`unsafe.Pointer`](https://pkg.go.dev/unsafe#Pointer) — used by the tests to prove row addresses land at the expected arena offsets.
- [Go blog: Arrays, slices (and strings): The mechanics of 'append'](https://go.dev/blog/slices) — background on slice headers and capacity.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-bounded-queue-drop-oldest-vs-reject.md](16-bounded-queue-drop-oldest-vs-reject.md) | Next: [18-copy-on-write-snapshot-publisher.md](18-copy-on-write-snapshot-publisher.md)
