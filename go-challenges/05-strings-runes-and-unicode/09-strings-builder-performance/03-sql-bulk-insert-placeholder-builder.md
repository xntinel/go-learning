# Exercise 3: Build Parameterized Bulk-INSERT Placeholders in a Repository Layer

A repository that inserts many rows in one round trip needs to build a `VALUES`
clause with a numbered placeholder per column — `($1,$2),($3,$4),...` — and a
matching flat args slice, without ever putting a value into the SQL text. This
exercise builds that helper with `strings.Builder` and `strconv.AppendInt`, and its
whole discipline is that the builder assembles TEXT while values stay parameterized.

This module is self-contained.

## What you'll build

```text
bulkinsert/                  independent module: example.com/bulkinsert
  go.mod
  bulkinsert.go              BuildBulkInsert -> (query, args, error); sentinel errors
  cmd/
    demo/
      main.go                builds an INSERT for three rows, prints query + args
  bulkinsert_test.go         exact-placeholder tests, arg-flattening, error cases
```

Files: `bulkinsert.go`, `cmd/demo/main.go`, `bulkinsert_test.go`.
Implement: `BuildBulkInsert(table string, columns []string, rows [][]any) (string, []any, error)` returning `INSERT INTO t (c1,c2) VALUES ($1,$2),($3,$4)` plus the flattened args.
Test: exact placeholder string for 1x1, 2x3, and 0-row inputs; `len(args) == rows*cols`; error on empty rowset and ragged rows.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/03-sql-bulk-insert-placeholder-builder/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/09-strings-builder-performance/03-sql-bulk-insert-placeholder-builder
```

### The one rule: text is built, values are parameterized

SQL injection happens when a value ends up inside the query text. The entire point of
placeholders is that the driver sends the query text and the argument values
separately, and the database never parses a value as SQL. So this helper builds only
the *skeleton* — table name, column list, and a grid of `$N` placeholders — and
returns the values untouched in a `[]any` in exactly the order the placeholders
reference them. A caller does `db.Exec(query, args...)`; the values never touch the
Builder. If you ever find yourself writing a value into the Builder, you have
reintroduced the injection you were avoiding.

PostgreSQL numbers placeholders `$1, $2, ...` globally across the whole statement,
not per row, so the counter increments once per column across all rows: row 0 uses
`$1..$M`, row 1 uses `$(M+1)..$(2M)`, and so on. We keep a running `n` starting at 1.
To number without allocating a string per placeholder, we format the integer with
`strconv.AppendInt` into a tiny reused scratch slice and `Write` those bytes into the
Builder — no `fmt.Sprintf` per placeholder, which would allocate and box.

The failure modes a repository must reject rather than emit malformed SQL: an empty
rowset (there is nothing to insert, and `VALUES` with no tuples is a syntax error) and
a ragged row whose length does not match the column count (the placeholders and args
would desynchronize). Both return a wrapped sentinel error so callers can branch on
them with `errors.Is`.

Create `bulkinsert.go`:

```go
package bulkinsert

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNoRows is returned when there are no rows to insert.
var ErrNoRows = errors.New("bulkinsert: no rows")

// ErrRaggedRow is returned when a row's length does not match the column count.
var ErrRaggedRow = errors.New("bulkinsert: row length does not match column count")

// BuildBulkInsert builds a parameterized multi-row INSERT. It returns the query
// text with $N placeholders and a flat args slice in placeholder order; values
// are never written into the query text.
func BuildBulkInsert(table string, columns []string, rows [][]any) (string, []any, error) {
	if len(rows) == 0 {
		return "", nil, ErrNoRows
	}
	cols := len(columns)

	var b strings.Builder
	// Rough pre-size: fixed preamble plus ~6 bytes per placeholder.
	b.Grow(len(table) + cols*12 + len(rows)*cols*6 + 32)
	b.WriteString("INSERT INTO ")
	b.WriteString(table)
	b.WriteString(" (")
	b.WriteString(strings.Join(columns, ","))
	b.WriteString(") VALUES ")

	args := make([]any, 0, len(rows)*cols)
	scratch := make([]byte, 0, 8)
	n := 1
	for i, row := range rows {
		if len(row) != cols {
			return "", nil, fmt.Errorf("%w: row %d has %d values, want %d", ErrRaggedRow, i, len(row), cols)
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('(')
		for j := range row {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('$')
			scratch = strconv.AppendInt(scratch[:0], int64(n), 10)
			b.Write(scratch)
			n++
		}
		b.WriteByte(')')
		args = append(args, row...)
	}
	return b.String(), args, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/bulkinsert"
)

func main() {
	rows := [][]any{
		{"alice", "alice@example.com"},
		{"bob", "bob@example.com"},
		{"carol", "carol@example.com"},
	}
	query, args, err := bulkinsert.BuildBulkInsert("users", []string{"name", "email"}, rows)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(query)
	fmt.Println(args)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INSERT INTO users (name,email) VALUES ($1,$2),($3,$4),($5,$6)
[alice alice@example.com bob bob@example.com carol carol@example.com]
```

### Tests

The tests pin the exact placeholder text for a 1x1 and a 2x3 grid, confirm the args
are flattened in placeholder order and count `rows*cols`, and assert the two error
paths with `errors.Is`.

Create `bulkinsert_test.go`:

```go
package bulkinsert

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestBuildBulkInsertShapes(t *testing.T) {
	t.Parallel()

	t.Run("1x1", func(t *testing.T) {
		t.Parallel()
		q, args, err := BuildBulkInsert("t", []string{"c"}, [][]any{{1}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		const want = "INSERT INTO t (c) VALUES ($1)"
		if q != want {
			t.Fatalf("query = %q, want %q", q, want)
		}
		if len(args) != 1 {
			t.Fatalf("len(args) = %d, want 1", len(args))
		}
	})

	t.Run("2x3", func(t *testing.T) {
		t.Parallel()
		rows := [][]any{{1, 2, 3}, {4, 5, 6}}
		q, args, err := BuildBulkInsert("t", []string{"a", "b", "c"}, rows)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		const want = "INSERT INTO t (a,b,c) VALUES ($1,$2,$3),($4,$5,$6)"
		if q != want {
			t.Fatalf("query = %q, want %q", q, want)
		}
		if len(args) != 6 {
			t.Fatalf("len(args) = %d, want 6", len(args))
		}
		if !reflect.DeepEqual(args, []any{1, 2, 3, 4, 5, 6}) {
			t.Fatalf("args = %v, want [1 2 3 4 5 6]", args)
		}
	})
}

func TestBuildBulkInsertErrors(t *testing.T) {
	t.Parallel()

	if _, _, err := BuildBulkInsert("t", []string{"c"}, nil); !errors.Is(err, ErrNoRows) {
		t.Fatalf("empty rows: got %v, want ErrNoRows", err)
	}

	ragged := [][]any{{1, 2}, {3}}
	if _, _, err := BuildBulkInsert("t", []string{"a", "b"}, ragged); !errors.Is(err, ErrRaggedRow) {
		t.Fatalf("ragged rows: got %v, want ErrRaggedRow", err)
	}
}

func ExampleBuildBulkInsert() {
	q, args, _ := BuildBulkInsert("users", []string{"name"}, [][]any{{"alice"}, {"bob"}})
	fmt.Println(q)
	fmt.Println(args)
	// Output:
	// INSERT INTO users (name) VALUES ($1),($2)
	// [alice bob]
}
```

## Review

The helper is correct when the placeholder numbers run `$1..$(rows*cols)` in row-major
order, the args slice mirrors that order and length, and the two malformed inputs —
empty rowset and ragged row — return wrapped sentinels rather than emitting broken
SQL. The security-critical invariant is that no element of any `row` ever reaches the
Builder; grep the code and the only things written are the table name, the column
names you control, punctuation, and `$N`. Reusing the `scratch` slice with
`AppendInt(scratch[:0], ...)` is what keeps placeholder numbering allocation-free
instead of one `strconv.Itoa` string per placeholder. Column names are assumed to be
developer-controlled identifiers; never build them from user input.

## Resources

- [strconv.AppendInt](https://pkg.go.dev/strconv#AppendInt) — format an int into a byte slice.
- [database/sql: query parameters](https://pkg.go.dev/database/sql#DB.Query) — why placeholders keep values out of SQL text.
- [strings.Builder](https://pkg.go.dev/strings#Builder) — the buffer assembling the query text.

---

Prev: [02-benchmark-assemblers-with-benchmem.md](02-benchmark-assemblers-with-benchmem.md) | Back to [00-concepts.md](00-concepts.md) | Next: [04-csv-export-row-encoder.md](04-csv-export-row-encoder.md)
