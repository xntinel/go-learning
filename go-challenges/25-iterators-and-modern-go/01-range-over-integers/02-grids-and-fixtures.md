# Exercise 2: Grids and Test Fixtures

Where the integer range form pays off beyond a single counter is anything two-dimensional or repeated-with-structure: building a grid, an identity matrix, or a batch of deterministic test data. This exercise builds a `fixtures` package whose `Grid`, `Identity`, and `Users` functions all express their shape with `for r := range rows` and `for i := range n`, never a hand-managed index in a loop header.

This module is fully self-contained. It has its own `go mod init`, defines every type and function it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
fixtures.go          Grid, Identity, User, Users
cmd/
  demo/
    main.go          print a 2x3 grid, a 3x3 identity matrix, and three users
fixtures_test.go     pin grid contents and dimensions, identity, user fixtures, empty case
```

- Files: `fixtures.go`, `cmd/demo/main.go`, `fixtures_test.go`.
- Implement: `Grid(rows, cols int) [][]int`, `Identity(n int) [][]int`, the `User` struct, and `Users(n int) []User`.
- Test: pin the exact contents and dimensions of a grid, the identity matrix, the deterministic user fixtures, and the empty (`n == 0`) case.
- Verify: `go test -count=1 -race ./...`

### Why nested integer ranges read better than indexed loops

A two-dimensional structure is the place the three-clause loop costs the most, because it doubles every off-by-one risk: an outer `for r := 0; r < rows; r++` and an inner `for c := 0; c < cols; c++` give you two comparison operators, two increments, and two start values to keep consistent. Nesting `for r := range rows` inside which sits `for c := range cols` removes all of it. The loop headers carry only the bounds; the body carries only the meaning. `Grid` fills cell `[r][c]` with the sequential index `r*cols + c`, so the value at each cell makes the row-major layout visible: row 0 holds `0, 1, 2`, row 1 holds `3, 4, 5`, and the arithmetic `r*cols + c` is the standard flattening of a 2-D coordinate into a 1-D offset.

`Identity` shows that the inner dimension does not always need its own loop. The identity matrix is mostly zeros, and `make([]int, n)` already gives a zeroed row, so the only work per row is to set the single diagonal element `m[r][r] = 1`. One `for r := range n` does it; there is no inner loop because there is nothing to do off the diagonal. Recognizing when a default-zeroed allocation has already done the inner loop's work is a small but real efficiency habit.

`Users` is the pattern that shows up constantly in real test suites: build `n` rows of deterministic fixture data so a test can assert exact values without a random seed. `for i := range n` drives it, and every field is a pure function of `i`: the ID is `i + 1` (fixtures usually want 1-based IDs to match a database's identity column), the name is `fmt.Sprintf("user-%02d", i+1)` (zero-padded so they sort and read uniformly), and `Active` alternates with `i%2 == 0`. Because nothing here consults a clock or a random source, `Users(3)` returns the same three users on every run, which is exactly what makes the fixtures assertable.

Create `fixtures.go`:

```go
package fixtures

import "fmt"

// Grid builds a rows x cols matrix whose cell at [r][c] holds the sequential
// index r*cols + c. The outer loop ranges over the row count and the inner loop
// over the column count, so neither bound needs a manual counter.
func Grid(rows, cols int) [][]int {
	g := make([][]int, rows)
	for r := range rows {
		g[r] = make([]int, cols)
		for c := range cols {
			g[r][c] = r*cols + c
		}
	}
	return g
}

// Identity builds the n x n identity matrix: ones on the diagonal, zeros
// elsewhere. Each row is already zeroed by make, so the only per-row work is to
// set the single diagonal element.
func Identity(n int) [][]int {
	m := make([][]int, n)
	for r := range n {
		m[r] = make([]int, n)
		m[r][r] = 1
	}
	return m
}

// User is a deterministic test fixture.
type User struct {
	ID     int
	Name   string
	Active bool
}

// Users builds n deterministic user fixtures. The IDs are 1-based, the names are
// zero-padded, and every second user is inactive, so a test can assert on exact
// values without a random seed.
func Users(n int) []User {
	users := make([]User, n)
	for i := range n {
		users[i] = User{
			ID:     i + 1,
			Name:   fmt.Sprintf("user-%02d", i+1),
			Active: i%2 == 0,
		}
	}
	return users
}
```

`Grid` allocates the outer slice to `rows`, then allocates and fills each inner row of `cols` cells. `Identity` allocates each row but relies on the zero value for every cell except the diagonal. `Users` preallocates the result to exactly `n` and assigns by index (`users[i] = ...`) rather than appending, which it can do because the length is known up front.

### The runnable demo

The demo prints all three structures so their shapes are visible at once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fixtures"
)

func main() {
	for _, row := range fixtures.Grid(2, 3) {
		fmt.Println("grid row:", row)
	}
	for _, row := range fixtures.Identity(3) {
		fmt.Println("identity row:", row)
	}
	for _, u := range fixtures.Users(3) {
		fmt.Printf("user: %+v\n", u)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
grid row: [0 1 2]
grid row: [3 4 5]
identity row: [1 0 0]
identity row: [0 1 0]
identity row: [0 0 1]
user: {ID:1 Name:user-01 Active:true}
user: {ID:2 Name:user-02 Active:false}
user: {ID:3 Name:user-03 Active:true}
```

### Tests

The tests pin both the contents and the shape of what the builders produce. `TestGrid` checks the exact 2x3 matrix, proving the `r*cols + c` numbering. `TestGridDimensions` checks a 4x5 grid has four rows of five, using `for r := range g` to walk the result. `TestIdentity` pins the 3x3 identity. `TestUsers` asserts the deterministic fixtures field-for-field, and `TestUsersEmpty` proves `Users(0)` returns an empty slice rather than panicking.

Create `fixtures_test.go`:

```go
package fixtures

import (
	"reflect"
	"testing"
)

func TestGrid(t *testing.T) {
	t.Parallel()

	got := Grid(2, 3)
	want := [][]int{{0, 1, 2}, {3, 4, 5}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Grid(2, 3) = %v, want %v", got, want)
	}
}

func TestGridDimensions(t *testing.T) {
	t.Parallel()

	g := Grid(4, 5)
	if len(g) != 4 {
		t.Fatalf("rows = %d, want 4", len(g))
	}
	for r := range g {
		if len(g[r]) != 5 {
			t.Fatalf("row %d width = %d, want 5", r, len(g[r]))
		}
	}
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	got := Identity(3)
	want := [][]int{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Identity(3) = %v, want %v", got, want)
	}
}

func TestUsers(t *testing.T) {
	t.Parallel()

	got := Users(3)
	want := []User{
		{ID: 1, Name: "user-01", Active: true},
		{ID: 2, Name: "user-02", Active: false},
		{ID: 3, Name: "user-03", Active: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Users(3) = %v, want %v", got, want)
	}
}

func TestUsersEmpty(t *testing.T) {
	t.Parallel()

	if got := Users(0); len(got) != 0 {
		t.Fatalf("Users(0) = %v, want empty", got)
	}
}
```

## Review

The builders are correct when the loop shape matches the data shape. `Grid` nests `for c := range cols` inside `for r := range rows` and fills `r*cols + c`, the row-major flattening, with no index arithmetic in the loop headers. `Identity` uses a single range over `n` and leans on `make`'s zeroing for every cell but the diagonal. `Users` drives one `for i := range n` and derives every field from `i`, so the fixtures are deterministic and assertable without a random source.

The traps this code avoids: doubling the off-by-one surface by writing two three-clause loops for a 2-D walk; adding a redundant inner loop to zero cells that `make` already zeroed; and seeding fixtures from a clock or `math/rand` without a fixed seed, which makes the test data unassertable. The `-race` run with the content and dimension tests passing establishes that the grids, the identity matrix, and the deterministic users all hold.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the rules for ranging over an integer, used here in both the outer and inner loops.
- [Go 1.22 Release Notes](https://go.dev/doc/go1.22) — the release that added ranging over integers.
- [`fmt` package](https://pkg.go.dev/fmt) — `Sprintf` and the `%02d` and `%+v` verbs used to build and print the fixtures.

---

Back to [01-counter-package.md](01-counter-package.md) | Next: [03-closures-and-retries.md](03-closures-and-retries.md)
