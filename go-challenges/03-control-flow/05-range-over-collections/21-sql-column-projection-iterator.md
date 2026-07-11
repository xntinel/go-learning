# Exercise 21: Lazy Column Projection from a Row Iterator

**Nivel: Intermedio** — validacion rapida (un test corto).

A `*sql.Rows` cursor yields every column a query selected, but a caller —
say, an API handler serializing a response — often needs only a subset, and
must never leak sensitive columns like a password hash into that response by
accident. This module wraps a row source with an `iter.Seq` that yields only
the requested columns, projecting lazily row by row instead of scanning the
whole result set into memory first and filtering afterward. The module is
fully self-contained: its own `go mod init`, no external dependencies.

## What you'll build

```text
projection/                 independent module: example.com/sql-column-projection-iterator
  go.mod                    go 1.24
  projection.go             type Row; FromRows(rows) iter.Seq[Row]; Project(source, columns) iter.Seq[Row]
  cmd/
    demo/
      main.go               runnable demo: project two of three columns
  projection_test.go        table test: subset projection + missing column + early break
```

- Files: `projection.go`, `cmd/demo/main.go`, `projection_test.go`.
- Implement: `FromRows(rows []Row) iter.Seq[Row]` and
  `Project(source iter.Seq[Row], columns []string) iter.Seq[Row]`.
- Test: one table asserting only requested columns survive and a missing
  column is simply omitted, plus a test proving `break` stops the chained
  iterator early.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/sql-column-projection-iterator/cmd/demo
cd ~/go-exercises/sql-column-projection-iterator
go mod init example.com/sql-column-projection-iterator
go mod edit -go=1.24
```

### Chaining iterators without ever holding the full result set

`Project` takes an `iter.Seq[Row]` and returns another `iter.Seq[Row]` — it
does not take a slice of rows and does not return one either. That is the
whole point of the exercise: `for row := range source` inside `Project`'s
body only pulls one row at a time from whatever produced `source` (here,
`FromRows` wrapping a slice, but in production a real `*sql.Rows` cursor),
projects it, and immediately forwards it via `yield` before asking for the
next one. No intermediate `[]Row` of the full result set, and no
intermediate `[]Row` of the projected result either — the whole pipeline is
one row in flight at a time, however many the underlying source actually
has. This is the shape you would build `Filter`, `Map`, or `Take` adapters
in as well: an `iter.Seq` wrapping another `iter.Seq`, composing without
allocating a buffer at each stage.

The other subtlety is what happens to a requested column that a particular
row does not have. `Project` uses the comma-ok form, `row[col]`, and only
writes into `projected` when the column is actually present — it does not
write a zero value for a missing key. That preserves a real distinction a
SQL caller cares about: a `NULL` column that was selected still has a key in
the map (with a nil or zero value depending on how it was scanned), while a
column that was never selected — or that this particular heterogeneous row
simply lacks — has no key at all. Collapsing those two cases into "the map
has a zero value either way" would make `_, ok := row["email"]` lie about
whether the column was ever selected.

Create `projection.go`:

```go
package projection

import "iter"

// Row is one full result-set row, column name to value.
type Row map[string]any

// FromRows adapts a plain slice of Row into an iter.Seq[Row], standing in
// for a real database cursor (e.g. *sql.Rows) in this exercise.
func FromRows(rows []Row) iter.Seq[Row] {
	return func(yield func(Row) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
		}
	}
}

// Project returns a lazy iter.Seq that, for each row pulled from source,
// yields a new Row containing only the requested columns. It never
// materializes the underlying result set and never allocates space for a
// column nobody asked for; a requested column absent from a given row is
// simply omitted from that row's projection rather than added as a zero
// value, so callers can distinguish "column not selected" from "column
// selected but NULL" by checking the second return of a map read.
func Project(source iter.Seq[Row], columns []string) iter.Seq[Row] {
	return func(yield func(Row) bool) {
		for row := range source {
			projected := make(Row, len(columns))
			for _, col := range columns {
				if v, ok := row[col]; ok {
					projected[col] = v
				}
			}
			if !yield(projected) {
				return
			}
		}
	}
}
```

### The runnable demo

The demo builds two rows with three columns each and projects down to only
`id` and `email`, dropping `password_hash` from every row that passes
through.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sql-column-projection-iterator"
)

func main() {
	rows := []projection.Row{
		{"id": 1, "email": "a@example.com", "password_hash": "x1"},
		{"id": 2, "email": "b@example.com", "password_hash": "x2"},
	}

	source := projection.FromRows(rows)
	projected := projection.Project(source, []string{"id", "email"})

	for row := range projected {
		fmt.Printf("id=%v email=%v cols=%d\n", row["id"], row["email"], len(row))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
id=1 email=a@example.com cols=2
id=2 email=b@example.com cols=2
```

Each projected row has exactly 2 columns — `password_hash` never appears in
the output, though it was present in the source.

### Tests

The table asserts requested columns survive projection while others are
dropped, and that a row missing a requested column simply omits it rather
than adding a zero value; a second test proves a `break` in the caller's
range loop actually stops `Project` (and, transitively, `FromRows`) from
producing further rows.

Create `projection_test.go`:

```go
package projection

import (
	"reflect"
	"slices"
	"testing"
)

func TestProject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rows    []Row
		columns []string
		want    []Row
	}{
		{
			name:    "empty source",
			rows:    []Row{},
			columns: []string{"id"},
			want:    nil,
		},
		{
			name: "requested columns kept, others dropped, missing column omitted",
			rows: []Row{
				{"id": 1, "email": "a@example.com", "password_hash": "x1"},
				{"id": 2, "password_hash": "x2"}, // no email column on this row
			},
			columns: []string{"id", "email"},
			want: []Row{
				{"id": 1, "email": "a@example.com"},
				{"id": 2},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Collect(Project(FromRows(tc.rows), tc.columns))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Project() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProjectStopsOnBreak(t *testing.T) {
	t.Parallel()

	rows := []Row{
		{"id": 1}, {"id": 2}, {"id": 3},
	}

	seen := 0
	for range Project(FromRows(rows), []string{"id"}) {
		seen++
		if seen == 1 {
			break
		}
	}
	if seen != 1 {
		t.Fatalf("seen = %d, want 1 (break should stop the iterator)", seen)
	}
}
```

Run it:

```bash
go test -count=1 ./...
```

## Review

`Project` is correct when every yielded row contains exactly the requested
columns that were present on the source row — no extra columns, and no
zero-valued placeholders for columns that were absent — and when it stops
pulling from `source` the moment the caller's loop stops consuming. The
`TestProjectStopsOnBreak` case exists because it is easy to write a
range-over-func adapter that forwards `yield`'s return value incorrectly (or
not at all); ignoring it here would mean a caller's `break` on the first row
still silently drained all three rows out of the underlying source.

## Resources

- [package iter](https://pkg.go.dev/iter) — the `Seq` iterator type and range-over-func semantics.
- [package slices (Collect)](https://pkg.go.dev/slices#Collect)
- [database/sql: Rows](https://pkg.go.dev/database/sql#Rows) — the real cursor this exercise's `Row`/`FromRows` stand in for.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [20-topic-subscription-dispatcher.md](20-topic-subscription-dispatcher.md) | Next: [22-connection-pool-lru-eviction.md](22-connection-pool-lru-eviction.md)
