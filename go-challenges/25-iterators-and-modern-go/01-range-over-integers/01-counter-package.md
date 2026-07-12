# Exercise 1: A Counter Package

The integer range form earns its keep first in the most basic place a count appears: a small library that produces sequences and repeats work a fixed number of times. This exercise builds a `counter` package with `Count`, `Repeat`, `Squares`, and `TimesTable`, each using `for i := range n` or `for range n`, and each validating a negative count at the API boundary instead of leaning on the loop's silent tolerance of negatives.

This module is fully self-contained. It has its own `go mod init`, defines every function it needs, and ships its own demo, table tests, and a testable example. Nothing here imports another exercise.

## What you'll build

```text
counter.go           Count, Repeat, Squares, TimesTable, ErrNegativeCount
cmd/
  demo/
    main.go          run each function and show a rejected negative count
counter_test.go      table tests for Count, negative-input rejection, derived sequences
example_test.go      ExampleCount (a compiled, output-checked doc example)
```

- Files: `counter.go`, `cmd/demo/main.go`, `counter_test.go`, `example_test.go`.
- Implement: `Count(n int) ([]int, error)`, `Repeat(s string, n int) (string, error)`, `Squares(n int) ([]int, error)`, `TimesTable(row, through int) ([]string, error)`, and the sentinel `ErrNegativeCount`.
- Test: round-trip `Count`, assert every function rejects a negative count with `ErrNegativeCount`, and pin the derived sequences.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/01-range-over-integers/01-counter-package/cmd/demo && cd go-solutions/25-iterators-and-modern-go/01-range-over-integers/01-counter-package
```

### Why each function looks the way it does

The package draws a clean line between the two integer-range forms. `Count` and `Squares` need the iteration number, so they use `for i := range n` and read `i` in the body. `Repeat` does not care which iteration it is on — it appends the same string every time — so it uses `for range n`, which states "do this n times" without introducing an index that `go vet` and the reader would otherwise have to justify. Choosing the form by whether the body reads the counter is the single most useful habit this lesson teaches.

The validation is the other half of the design. Ranging over a negative `int` produces zero iterations with no error, which is convenient inside a private helper but wrong at a public boundary: a function that returns an empty slice for `Count(-1)` cannot tell a caller's bug from a request for zero items. So every exported function checks `n < 0` first and returns a wrapped `ErrNegativeCount`. Wrapping with `%w` (rather than returning the sentinel directly) preserves the offending value in the message while still letting callers match the category with `errors.Is(err, ErrNegativeCount)`. A valid zero count is allowed: `Count(0)` returns an empty, non-nil slice and no error, which is a genuine "zero items" result distinct from the error case.

`TimesTable` is the one place an off-by-one would naturally hide, and it shows the zero-based iterator handling a one-based-feeling domain. A table "through 3" should include rows `0, 1, 2, 3` — four lines — so the loop ranges over `through + 1`, not `through`. The iterator stays zero-based; the `+1` lives in the bound expression where the inclusive upper limit belongs.

Create `counter.go`:

```go
package counter

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNegativeCount is returned by every function in this package when it is
// asked to produce a negative number of items. Callers match it with
// errors.Is, which works because the functions wrap it with %w.
var ErrNegativeCount = errors.New("count must not be negative")

// Count returns the slice [0, 1, ..., n-1]. A zero count yields an empty,
// non-nil slice; a negative count is rejected.
func Count(n int) ([]int, error) {
	if n < 0 {
		return nil, fmt.Errorf("count %d: %w", n, ErrNegativeCount)
	}

	values := make([]int, 0, n)
	for i := range n {
		values = append(values, i)
	}
	return values, nil
}

// Repeat concatenates s with itself n times. It uses `for range n` because the
// iteration number is irrelevant: only the count of repetitions matters.
func Repeat(s string, n int) (string, error) {
	if n < 0 {
		return "", fmt.Errorf("repeat %d: %w", n, ErrNegativeCount)
	}

	var b strings.Builder
	for range n {
		b.WriteString(s)
	}
	return b.String(), nil
}

// Squares returns [0, 1, 4, 9, ..., (n-1)^2].
func Squares(n int) ([]int, error) {
	if n < 0 {
		return nil, fmt.Errorf("squares %d: %w", n, ErrNegativeCount)
	}

	values := make([]int, 0, n)
	for i := range n {
		values = append(values, i*i)
	}
	return values, nil
}

// TimesTable returns the rows "i x row = i*row" for i in [0, through]. The loop
// ranges over `through + 1` because the upper bound is inclusive.
func TimesTable(row, through int) ([]string, error) {
	if through < 0 {
		return nil, fmt.Errorf("times table through %d: %w", through, ErrNegativeCount)
	}

	lines := make([]string, 0, through+1)
	for i := range through + 1 {
		lines = append(lines, fmt.Sprintf("%d x %d = %d", i, row, i*row))
	}
	return lines, nil
}
```

`Count` builds its slice with a preallocated capacity of `n` and appends `i` each pass; `Squares` is identical but appends `i*i`. `Repeat` ignores the iterator entirely and writes into a `strings.Builder`, which avoids the quadratic copying of repeated string concatenation. `TimesTable` is the only function whose bound is an expression rather than a bare variable, and the `+ 1` there is the inclusive-upper-bound adjustment, kept out of the loop body.

### The runnable demo

The demo exercises every function and ends by showing that a negative count is rejected rather than silently treated as zero.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/counter"
)

func main() {
	squares, err := counter.Squares(6)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("squares:", squares)

	banner, err := counter.Repeat("Go! ", 3)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print("banner: ", banner, "\n")

	table, err := counter.TimesTable(7, 4)
	if err != nil {
		log.Fatal(err)
	}
	for _, line := range table {
		fmt.Println(line)
	}

	if _, err := counter.Count(-1); err != nil {
		fmt.Println("rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
squares: [0 1 4 9 16 25]
banner: Go! Go! Go! 
0 x 7 = 0
1 x 7 = 7
2 x 7 = 14
3 x 7 = 21
4 x 7 = 28
rejected: count -1: count must not be negative
```

### Tests

The tests pin three properties. `TestCount` round-trips the zero and five cases and proves the zero case is a valid empty result, not an error. `TestNegativeInputs` drives every exported function through a `-1` and asserts each one returns `ErrNegativeCount` via `errors.Is`, which is the contract the `%w` wrapping exists to support. `TestSquaresAndTable` pins the derived sequences, including the inclusive upper bound of `TimesTable`. `TestRepeatZero` proves the documented "zero is valid" behavior for the repetition path.

Create `counter_test.go`:

```go
package counter

import (
	"errors"
	"reflect"
	"testing"
)

func TestCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want []int
	}{
		{name: "zero", n: 0, want: []int{}},
		{name: "five", n: 5, want: []int{0, 1, 2, 3, 4}},
	}

	for _, tt := range tests {
		got, err := Count(tt.n)
		if err != nil {
			t.Fatalf("Count(%d) error = %v", tt.n, err)
		}
		if !reflect.DeepEqual(got, tt.want) {
			t.Fatalf("Count(%d) = %v, want %v", tt.n, got, tt.want)
		}
	}
}

func TestNegativeInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func() error
	}{
		{name: "count", call: func() error { _, err := Count(-1); return err }},
		{name: "repeat", call: func() error { _, err := Repeat("go", -1); return err }},
		{name: "squares", call: func() error { _, err := Squares(-1); return err }},
		{name: "table", call: func() error { _, err := TimesTable(7, -1); return err }},
	}

	for _, tt := range tests {
		if err := tt.call(); !errors.Is(err, ErrNegativeCount) {
			t.Fatalf("%s error = %v, want ErrNegativeCount", tt.name, err)
		}
	}
}

func TestRepeatZero(t *testing.T) {
	t.Parallel()

	got, err := Repeat("x", 0)
	if err != nil {
		t.Fatalf("Repeat zero error = %v", err)
	}
	if got != "" {
		t.Fatalf("Repeat(x, 0) = %q, want empty string", got)
	}
}

func TestSquaresAndTable(t *testing.T) {
	t.Parallel()

	squares, err := Squares(6)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(squares, []int{0, 1, 4, 9, 16, 25}) {
		t.Fatalf("Squares(6) = %v", squares)
	}

	table, err := TimesTable(7, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"0 x 7 = 0", "1 x 7 = 7", "2 x 7 = 14", "3 x 7 = 21"}
	if !reflect.DeepEqual(table, want) {
		t.Fatalf("TimesTable(7, 3) = %v, want %v", table, want)
	}
}
```

A testable example doubles as documentation: `go test` runs it and checks its output against the `// Output:` comment, so the example can never drift from the code.

Create `example_test.go`:

```go
package counter

import "fmt"

func ExampleCount() {
	values, _ := Count(5)
	fmt.Println(values)
	// Output: [0 1 2 3 4]
}
```

## Review

The package is correct when the two range forms are chosen by intent and the boundary is validated. `Count` and `Squares` read the iterator, so they use `for i := range n`; `Repeat` ignores it, so it uses `for range n`. Every exported function rejects `n < 0` with a `%w`-wrapped `ErrNegativeCount` before the loop, so `errors.Is` works and a negative count is never confused with a legitimate empty result. `TimesTable` ranges over `through + 1` to make the upper bound inclusive while keeping the iterator zero-based.

The common traps this code is built to avoid: expecting `for i := range 5` to yield one-based values (it yields `0..4`; use `i+1` for a one-based domain); letting a negative count silently mean "do nothing" instead of validating it; and keeping a three-clause loop in `Repeat` where the index is never read. The `-race` run with the table tests and the output-checked example passing together establish that the sequences, the inclusive bound, and the error contract all hold.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the normative rules for ranging over an integer, including the zero-based values and the integer-type requirement.
- [Go 1.22 Release Notes](https://go.dev/doc/go1.22) — the release that introduced ranging over integers.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` and the `%w` wrapping the sentinel-error contract relies on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-grids-and-fixtures.md](02-grids-and-fixtures.md)
