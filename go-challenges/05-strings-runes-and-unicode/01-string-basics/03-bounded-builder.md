# Exercise 3: Assemble a bulk SQL VALUES clause with strings.Builder

Building one big string out of many small pieces in a loop is the most common
place a Go backend accidentally goes quadratic. This module builds a bulk-insert
`VALUES` clause the right way — one pre-sized `strings.Builder` — and encodes the
O(n) versus O(n^2) lesson as an executable allocation assertion.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
bulkinsert/                 independent module: example.com/bulkinsert
  go.mod                    go 1.26
  bulkinsert.go             BuildBulkInsert with a Grow-sized strings.Builder
  cmd/
    demo/
      main.go               render an INSERT for three rows
  bulkinsert_test.go        correctness table + AllocsPerRun bound
```

Files: `bulkinsert.go`, `cmd/demo/main.go`, `bulkinsert_test.go`.
Implement: `BuildBulkInsert(table string, rows [][]string) string`.
Test: 0/1/N rows, values needing quote-escaping, and an `AllocsPerRun` assertion
that the pre-sized builder stays at a small constant allocation count.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/01-string-basics/03-bounded-builder/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/01-string-basics/03-bounded-builder
```

## Why += is a latency bug and Builder is the fix

The naive version is seductive:

```go
// Wrong: quadratic. Each += allocates a new string and copies everything so far.
out := "INSERT INTO " + table + " VALUES "
for i, row := range rows {
	if i > 0 {
		out += ","
	}
	out += "(" + strings.Join(row, ",") + ")"
}
```

A Go string is immutable, so `out += piece` cannot append in place — it allocates
a fresh string of length `len(out)+len(piece)` and copies the old contents into
it. Over `n` rows the total bytes copied grow as 1+2+3+...+n, i.e. O(n^2), and
every step is a separate heap allocation feeding the garbage collector. For a
bulk insert of thousands of rows this turns a microsecond of work into a
measurable latency spike and a GC churn source. It passes every unit test and
only hurts under load, which is exactly the kind of regression that is hard to
spot in review.

`strings.Builder` fixes both problems. It accumulates into a single `[]byte` that
grows amortized like `append`, and `String()` returns the accumulated bytes with
*no* final copy (it reinterprets the buffer as a string). Calling `Grow(n)` once
up front, with an estimate of the final size, pre-allocates the buffer so there
are zero intermediate reallocations — the whole build becomes one allocation and
one O(n) pass. Here the estimate is a small per-row overhead times the row count
plus the summed value lengths; over-estimating slightly is fine and cheaper than
re-growing.

Value escaping matters for correctness: a single quote inside a value must be
doubled (`O'Brien` becomes `'O''Brien'`) or the SQL is malformed (and in real
code, injectable). `strings.ReplaceAll(v, "'", "''")` does the doubling. This
exercise builds a literal string to teach the builder mechanics; in production you
would still prefer parameterized placeholders, but bulk COPY/format paths that
must emit literal SQL use exactly this escaping.

Create `bulkinsert.go`:

```go
package bulkinsert

import "strings"

// BuildBulkInsert renders "INSERT INTO <table> VALUES (..),(..)" for the given
// rows. Each value is single-quoted and internal single quotes are doubled. An
// empty rows slice yields "" (nothing to insert).
func BuildBulkInsert(table string, rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}

	// Estimate the final size so the builder allocates exactly once.
	size := len("INSERT INTO ") + len(table) + len(" VALUES ")
	for _, row := range rows {
		size += 3 // "(",")" plus a separating comma
		for _, v := range row {
			size += len(v) + 3 // two quotes plus a separating comma, with slack
		}
	}

	var b strings.Builder
	b.Grow(size)

	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" VALUES ")

	for i, row := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for j, v := range row {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('\'')
			b.WriteString(strings.ReplaceAll(v, "'", "''"))
			b.WriteByte('\'')
		}
		b.WriteByte(')')
	}

	return b.String()
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/bulkinsert"
)

func main() {
	rows := [][]string{
		{"1", "alice"},
		{"2", "O'Brien"},
		{"3", "carol"},
	}
	fmt.Println(bulkinsert.BuildBulkInsert("users", rows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INSERT INTO users VALUES ('1','alice'),('2','O''Brien'),('3','carol')
```

## Tests

The correctness table covers zero, one, and many rows plus quote-escaping. The
allocation test is the lesson made executable: `testing.AllocsPerRun` reports the
average number of heap allocations per call, and a `Grow`-sized builder building
1000 rows must stay at a small constant — not grow with the row count. Note the
inner `strings.ReplaceAll` allocates only when a value actually contains a quote,
so the alloc-bound test uses quote-free values to isolate the builder's own
behavior.

Create `bulkinsert_test.go`:

```go
package bulkinsert

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildBulkInsert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		table string
		rows  [][]string
		want  string
	}{
		{"empty", "users", nil, ""},
		{"one row", "users", [][]string{{"1", "alice"}}, "INSERT INTO users VALUES ('1','alice')"},
		{
			"many rows",
			"users",
			[][]string{{"1", "alice"}, {"2", "bob"}},
			"INSERT INTO users VALUES ('1','alice'),('2','bob')",
		},
		{
			"quote escaping",
			"users",
			[][]string{{"1", "O'Brien"}},
			"INSERT INTO users VALUES ('1','O''Brien')",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := BuildBulkInsert(tc.table, tc.rows)
			if got != tc.want {
				t.Fatalf("BuildBulkInsert = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildBulkInsertAllocations(t *testing.T) {
	// No t.Parallel(): testing.AllocsPerRun must not run during a parallel test.
	rows := make([][]string, 1000)
	for i := range rows {
		rows[i] = []string{"1", "alice"}
	}

	avg := testing.AllocsPerRun(100, func() {
		_ = BuildBulkInsert("users", rows)
	})

	// A pre-sized builder is a small constant number of allocations regardless
	// of row count; quadratic += would be thousands.
	if avg > 3 {
		t.Fatalf("BuildBulkInsert allocated %.0f times for 1000 rows; want a small constant (Grow not effective)", avg)
	}
}

func ExampleBuildBulkInsert() {
	out := BuildBulkInsert("t", [][]string{{"a"}, {"b"}})
	fmt.Println(strings.HasPrefix(out, "INSERT INTO t VALUES "))
	fmt.Println(out)
	// Output:
	// true
	// INSERT INTO t VALUES ('a'),('b')
}
```

## Review

The builder is correct when the output is well-formed SQL for zero, one, and many
rows, and when internal single quotes are doubled so a value like `O'Brien` cannot
break the statement. The allocation test is the real teaching artifact: it asserts
the pre-sized `strings.Builder` builds a thousand rows in a small constant number
of allocations, which the `+=` version cannot do. Keep the `Grow` estimate at or
above the true size so no intermediate re-grow happens, and never copy the
`Builder` after writing to it — pass a `*strings.Builder` if you factor a helper
out. Run `go test -race` to confirm the function holds no shared state across the
parallel subtests.

## Resources

- [strings.Builder (pkg.go.dev)](https://pkg.go.dev/strings#Builder)
- [strings.Builder.Grow (pkg.go.dev)](https://pkg.go.dev/strings#Builder.Grow)
- [testing.AllocsPerRun (pkg.go.dev)](https://pkg.go.dev/testing#AllocsPerRun)
- [The Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-header-and-cut.md](02-header-and-cut.md) | Next: [04-case-insensitive-lookup.md](04-case-insensitive-lookup.md)
