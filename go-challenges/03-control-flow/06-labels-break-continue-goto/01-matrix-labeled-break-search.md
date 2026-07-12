# Exercise 1: Scan a 2D grid index with a labeled break (first-match search)

A backend that serves spatial or tile lookups keeps a row-major grid in a flat
slice and answers "the first cell matching this predicate, and where is it." The
first-match search is the textbook place a labeled `break` is the right answer:
one decision leaves both the row loop and the column loop.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
tileindex/                 independent module: example.com/tileindex
  go.mod                   go 1.24
  tileindex.go             Grid[T comparable]; New, At, Set, FindFirst, ForEachNonZero
  cmd/
    demo/
      main.go              runnable demo: first-match search + visit non-zero tiles
  tileindex_test.go        table tests: first match, no match, non-square, bounds, iterate
```

- Files: `tileindex.go`, `cmd/demo/main.go`, `tileindex_test.go`.
- Implement: a generic `Grid[T comparable]` with `At`/`Set` bounds checks, `FindFirst` using a labeled `break` to leave both loops on the first predicate hit, and `ForEachNonZero` using a plain `continue` for independent per-cell iteration.
- Test: first value matching a predicate in a 3x3 returns with correct `(row,col)`; no-match returns `(zero,false,-1,-1)`; a non-square grid proves an inner-only break would return the wrong match; `ForEachNonZero` visits exactly the non-zero cells; `At`/`Set` reject out-of-range indices.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the labeled break is load-bearing here

`FindFirst` scans row-major: row 0 left to right, then row 1, and so on. "First
match" means the earliest cell in that traversal order. The moment a cell
satisfies the predicate the search is over — there is nothing left to look at, and
continuing would be wrong. But the hit is discovered inside the inner (column)
loop, and a bare `break` there leaves only the inner loop. The outer loop would
resume at the next row and keep scanning, and in a first-match search a later
match would overwrite the one you already found. The result would silently become
the *last* row containing a match, not the first.

A labeled `break search` leaves both loops in a single statement, exactly when the
match is found, so the first hit wins. The non-square test below pins this: it
places a match in an early row and another in a later row, and asserts the early
one is returned. An inner-only break would return the later one and fail the test.

`ForEachNonZero` is the opposite case and uses a plain `continue`: each cell is
independent, so skipping a zero cell means "this one is not interesting, move on,"
never "we are done." That is precisely what a plain `continue` says. `At` is the
real bounds check — it returns `(zero, false)` for any out-of-range index — so the
loops can call it and skip cells that are out of range without a separate guard.

Create `tileindex.go`:

```go
package tileindex

// Grid is a row-major 2D index backed by a flat slice: cell (row, col) lives at
// Data[row*Cols+col]. It is the shape a backend serves spatial or tile lookups
// against.
type Grid[T comparable] struct {
	Rows int
	Cols int
	Data []T
}

// New allocates a rows-by-cols grid with the zero value of T in every cell.
func New[T comparable](rows, cols int) *Grid[T] {
	return &Grid[T]{Rows: rows, Cols: cols, Data: make([]T, rows*cols)}
}

// At returns the value at (row, col) and true, or the zero value and false if
// the index is out of range. It is the single real bounds check.
func (g *Grid[T]) At(row, col int) (T, bool) {
	var zero T
	if row < 0 || row >= g.Rows || col < 0 || col >= g.Cols {
		return zero, false
	}
	return g.Data[row*g.Cols+col], true
}

// Set writes value at (row, col) and reports whether the index was in range.
func (g *Grid[T]) Set(row, col int, value T) bool {
	if row < 0 || row >= g.Rows || col < 0 || col >= g.Cols {
		return false
	}
	g.Data[row*g.Cols+col] = value
	return true
}

// FindFirst returns the first cell (in row-major order) whose value satisfies
// pred, along with its (row, col). If no cell matches it returns (zero, false,
// -1, -1). The labeled break leaves BOTH loops on the first hit, so the earliest
// match wins.
func (g *Grid[T]) FindFirst(pred func(T) bool) (value T, ok bool, row, col int) {
	row, col = -1, -1
search:
	for r := range g.Rows {
		for c := range g.Cols {
			v, in := g.At(r, c)
			if !in {
				continue
			}
			if pred(v) {
				value, ok, row, col = v, true, r, c
				break search
			}
		}
	}
	return value, ok, row, col
}

// ForEachNonZero calls fn for every cell whose value is not the given zero,
// in row-major order. The plain continue skips uninteresting cells without
// ending the scan.
func (g *Grid[T]) ForEachNonZero(zero T, fn func(value T, row, col int)) {
	for r := range g.Rows {
		for c := range g.Cols {
			v, in := g.At(r, c)
			if !in || v == zero {
				continue
			}
			fn(v, r, c)
		}
	}
}
```

### The runnable demo

The demo builds a 3x3 tile grid of heights, finds the first tile at least five
high, then walks every occupied (non-zero) tile in traversal order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tileindex"
)

func main() {
	g := tileindex.New[int](3, 3)
	copy(g.Data, []int{
		0, 0, 5,
		0, 7, 0,
		3, 0, 0,
	})

	if v, ok, r, c := g.FindFirst(func(h int) bool { return h >= 5 }); ok {
		fmt.Printf("first tall tile: height=%d at (%d,%d)\n", v, r, c)
	}

	fmt.Println("occupied tiles:")
	g.ForEachNonZero(0, func(v, r, c int) {
		fmt.Printf("  (%d,%d)=%d\n", r, c, v)
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first tall tile: height=5 at (0,2)
occupied tiles:
  (0,2)=5
  (1,1)=7
  (2,0)=3
```

### Tests

`TestFindFirstReturnsFirstMatch` proves the labeled break returns the earliest
match in a 3x3. `TestFindFirstNonSquareEarlyRow` is the real proof: in a 3x2 grid
a match sits in row 0 and another in row 2, and the test asserts row 0 wins — an
inner-only break would let row 2 overwrite it. `TestForEachNonZeroVisitsNonZero`
proves the plain `continue` does not short-circuit the outer loop, and the bounds
tests pin `At`/`Set`.

Create `tileindex_test.go`:

```go
package tileindex

import (
	"fmt"
	"testing"
)

func TestFindFirstReturnsFirstMatch(t *testing.T) {
	t.Parallel()

	g := New[int](3, 3)
	copy(g.Data, []int{
		1, 2, 3,
		4, 5, 6,
		7, 8, 9,
	})

	v, ok, row, col := g.FindFirst(func(x int) bool { return x > 5 })
	if !ok {
		t.Fatal("expected a match")
	}
	if v != 6 || row != 1 || col != 2 {
		t.Fatalf("got (%d,%d,%d), want (6,1,2)", v, row, col)
	}
}

func TestFindFirstNonSquareEarlyRow(t *testing.T) {
	t.Parallel()

	// 3 rows, 2 cols. A match sits at (0,1) and another at (2,0). An inner-only
	// break would let the row-2 match overwrite the row-0 one; the labeled break
	// stops at (0,1).
	g := New[int](3, 2)
	copy(g.Data, []int{
		0, 9,
		0, 0,
		9, 0,
	})

	v, ok, row, col := g.FindFirst(func(x int) bool { return x == 9 })
	if !ok {
		t.Fatal("expected a match")
	}
	if v != 9 || row != 0 || col != 1 {
		t.Fatalf("got (%d,%d,%d), want (9,0,1)", v, row, col)
	}
}

func TestFindFirstNoMatch(t *testing.T) {
	t.Parallel()

	g := New[int](2, 2)
	copy(g.Data, []int{1, 2, 3, 4})

	v, ok, row, col := g.FindFirst(func(x int) bool { return x > 100 })
	if ok {
		t.Fatalf("expected no match, got value %d", v)
	}
	if row != -1 || col != -1 {
		t.Fatalf("got (%d,%d), want (-1,-1)", row, col)
	}
}

func TestForEachNonZeroVisitsNonZero(t *testing.T) {
	t.Parallel()

	g := New[int](2, 3)
	copy(g.Data, []int{
		0, 1, 0,
		2, 0, 3,
	})

	seen := map[[2]int]int{}
	g.ForEachNonZero(0, func(v, r, c int) {
		seen[[2]int{r, c}] = v
	})

	want := map[[2]int]int{
		{0, 1}: 1,
		{1, 0}: 2,
		{1, 2}: 3,
	}
	if len(seen) != len(want) {
		t.Fatalf("seen = %v, want %v", seen, want)
	}
	for k, v := range want {
		if seen[k] != v {
			t.Fatalf("seen[%v] = %d, want %d", k, seen[k], v)
		}
	}
}

func TestBoundsCheck(t *testing.T) {
	t.Parallel()

	g := New[int](2, 2)
	if _, ok := g.At(-1, 0); ok {
		t.Error("At(-1,0) should be out of range")
	}
	if _, ok := g.At(0, 5); ok {
		t.Error("At(0,5) should be out of range")
	}
	if g.Set(0, 5, 1) {
		t.Error("Set(0,5) beyond width should return false")
	}
	if !g.Set(1, 1, 7) {
		t.Error("Set(1,1) in range should return true")
	}
	if v, ok := g.At(1, 1); !ok || v != 7 {
		t.Errorf("At(1,1) = %d,%v; want 7,true", v, ok)
	}
}

func ExampleGrid_FindFirst() {
	g := New[int](2, 2)
	copy(g.Data, []int{1, 2, 3, 4})
	v, ok, r, c := g.FindFirst(func(x int) bool { return x%2 == 0 })
	fmt.Println(v, ok, r, c)
	// Output: 2 true 0 1
}
```

## Review

The search is correct when `FindFirst` returns the row-major-earliest cell
satisfying the predicate, and `(zero, false, -1, -1)` when none does. The proof
that the labeled break is doing its job is `TestFindFirstNonSquareEarlyRow`: with
matches in two different rows, only a `break search` that leaves both loops
returns the earlier one. The classic way to break this is a bare `break` in the
inner loop plus no outer guard, which turns the search into "last row with a
match wins." `ForEachNonZero`'s plain `continue` is the opposite: each cell is
independent, so skipping one never ends the walk. Run `go test -race` to confirm,
and remember `At` is the only bounds check — the loops rely on it to skip
out-of-range cells rather than duplicating the guard.

## Resources

- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the range clause and `for range n`.
- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — what a labeled `break` targets.
- [Go Specification: Type parameters](https://go.dev/ref/spec#Type_parameter_declarations) — the `comparable` constraint used by `Grid[T]`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-event-loop-break-in-select.md](02-event-loop-break-in-select.md)
