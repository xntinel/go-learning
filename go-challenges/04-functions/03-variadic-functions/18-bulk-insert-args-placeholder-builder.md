# Exercise 18: Bulk Insert SQL Arguments and Placeholder Generator

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A bulk insert writes several rows in one round trip: `INSERT INTO users (id,
name) VALUES ($1,$2), ($3,$4)`. The number of rows is not known at compile
time, so the query builder takes any number of row tuples as a variadic
parameter, validates each one against the column list, and produces both the
placeholder-numbered SQL string and the flat argument slice a database driver
expects — all without ever executing anything, so the exercise stays
dependency-free.

## What you'll build

```text
sqlbulk/                   independent module: example.com/sqlbulk
  go.mod                   go 1.24
  sqlbulk.go               package sqlbulk; func BuildInsert(table string, columns []string, rows ...[]any) (string, []any, error)
  cmd/
    demo/
      main.go              runnable demo: a valid bulk insert, then a mismatched one
  sqlbulk_test.go          table tests: single/multi row, zero rows, zero columns, mismatched row length
```

- Files: `sqlbulk.go`, `cmd/demo/main.go`, `sqlbulk_test.go`.
- Implement: `BuildInsert(table string, columns []string, rows ...[]any) (query string, args []any, err error)`, numbering `$1..$n` placeholders across the whole statement and flattening every row's values into `args` in the same order.
- Test: one row and two rows produce correctly grouped and numbered placeholders; zero rows and zero columns are both errors; a row whose length does not match `columns` is an error that names the offending row index.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/18-bulk-insert-args-placeholder-builder/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/18-bulk-insert-args-placeholder-builder
go mod edit -go=1.24
```

### `rows ...[]any`, and why placeholders number across the whole statement

`rows ...[]any` is a variadic of slices, the same shape as exercise 15's
recipient merger, but here each element represents a fixed-width tuple that
must match `columns` exactly — this is not a free-form merge, it is a
positional contract. `BuildInsert` validates that contract per row before
writing anything: if row `i` has the wrong number of values, the function
returns immediately with an error naming that row, rather than emitting a
half-built, syntactically broken query string.

The placeholder counter (`placeholder := 1`) is *not* reset between rows.
Postgres-style bind parameters are numbered once across the entire statement
— row two's first value is `$3` when each row has two columns, not `$1`
again — because the driver later supplies one flat `args` slice and matches
each `$N` to `args[N-1]`. Building `query` and `args` in the same loop, in
the same row order, is what keeps the placeholder numbers and the argument
positions from silently drifting apart if someone edits one without the
other.

Create `sqlbulk.go`:

```go
// sqlbulk.go
package sqlbulk

import (
	"fmt"
	"strings"
)

// BuildInsert builds a Postgres-style parameterized multi-row INSERT
// statement plus its flattened argument slice from any number of value
// tuples. Each row must have exactly len(columns) values; row order is
// preserved and placeholders are numbered $1.. across the whole statement so
// the returned args line up positionally with the query.
func BuildInsert(table string, columns []string, rows ...[]any) (query string, args []any, err error) {
	if len(columns) == 0 {
		return "", nil, fmt.Errorf("sqlbulk: no columns given")
	}
	if len(rows) == 0 {
		return "", nil, fmt.Errorf("sqlbulk: no rows given")
	}

	args = make([]any, 0, len(rows)*len(columns))
	var valueGroups strings.Builder
	placeholder := 1

	for i, row := range rows {
		if len(row) != len(columns) {
			return "", nil, fmt.Errorf("sqlbulk: row %d has %d value(s), want %d", i, len(row), len(columns))
		}
		if i > 0 {
			valueGroups.WriteString(", ")
		}
		valueGroups.WriteByte('(')
		for j := range row {
			if j > 0 {
				valueGroups.WriteByte(',')
			}
			fmt.Fprintf(&valueGroups, "$%d", placeholder)
			placeholder++
		}
		valueGroups.WriteByte(')')
		args = append(args, row...)
	}

	query = fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", table, strings.Join(columns, ", "), valueGroups.String())
	return query, args, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/sqlbulk"
)

func main() {
	query, args, err := sqlbulk.BuildInsert(
		"users",
		[]string{"id", "name", "email"},
		[]any{1, "alice", "alice@example.com"},
		[]any{2, "bob", "bob@example.com"},
	)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(query)
	fmt.Println(args)

	_, _, err = sqlbulk.BuildInsert(
		"users",
		[]string{"id", "name", "email"},
		[]any{1, "alice"},
	)
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
INSERT INTO users (id, name, email) VALUES ($1,$2,$3), ($4,$5,$6)
[1 alice alice@example.com 2 bob bob@example.com]
error: sqlbulk: row 0 has 2 value(s), want 3
```

### Tests

`TestMismatchedRowErrorNamesTheOffendingRow` pins the exact error message,
because "which row is broken" is the one piece of information an operator
actually needs when a bulk insert of a thousand rows fails — a generic
"row length mismatch" without an index would send them scanning the whole
batch by hand.

Create `sqlbulk_test.go`:

```go
// sqlbulk_test.go
package sqlbulk

import (
	"reflect"
	"testing"
)

func TestBuildInsert(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		table     string
		columns   []string
		rows      [][]any
		wantQuery string
		wantArgs  []any
		wantErr   bool
	}{
		{
			name:      "single row",
			table:     "users",
			columns:   []string{"id", "name"},
			rows:      [][]any{{1, "alice"}},
			wantQuery: "INSERT INTO users (id, name) VALUES ($1,$2)",
			wantArgs:  []any{1, "alice"},
		},
		{
			name:      "two rows number placeholders across the whole statement",
			table:     "users",
			columns:   []string{"id", "name"},
			rows:      [][]any{{1, "alice"}, {2, "bob"}},
			wantQuery: "INSERT INTO users (id, name) VALUES ($1,$2), ($3,$4)",
			wantArgs:  []any{1, "alice", 2, "bob"},
		},
		{
			name:    "zero rows is an error",
			table:   "users",
			columns: []string{"id", "name"},
			rows:    nil,
			wantErr: true,
		},
		{
			name:    "zero columns is an error",
			table:   "users",
			columns: nil,
			rows:    [][]any{{1}},
			wantErr: true,
		},
		{
			name:    "mismatched row length is an error",
			table:   "users",
			columns: []string{"id", "name"},
			rows:    [][]any{{1, "alice"}, {2}},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotQuery, gotArgs, err := BuildInsert(tc.table, tc.columns, tc.rows...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BuildInsert(%v) error = nil, want an error", tc.rows)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildInsert(%v) unexpected error: %v", tc.rows, err)
			}
			if gotQuery != tc.wantQuery {
				t.Fatalf("BuildInsert query = %q, want %q", gotQuery, tc.wantQuery)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("BuildInsert args = %v, want %v", gotArgs, tc.wantArgs)
			}
		})
	}
}

func TestMismatchedRowErrorNamesTheOffendingRow(t *testing.T) {
	t.Parallel()

	_, _, err := BuildInsert("users", []string{"id", "name"}, []any{1, "alice"}, []any{2})
	if err == nil {
		t.Fatalf("expected an error for a short second row")
	}
	const want = "sqlbulk: row 1 has 1 value(s), want 2"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}
```

## Review

`BuildInsert` is correct when every row contributes exactly `len(columns)`
placeholders numbered contiguously across the whole statement, `args`
contains those values in the same flattened order the placeholders appear
in, and a malformed input — no rows, no columns, or a mis-sized row — is
rejected before any string is built rather than producing a query that would
fail confusingly at the database. The senior point is treating a row
mismatch as a hard, immediate error rather than silently padding or
truncating the row to fit; a bulk insert with silently dropped columns is a
data-corruption bug wearing a "helpful" disguise. The mistake to avoid is
resetting the placeholder counter per row — that produces a syntactically
valid-looking query whose `$1` appears twice, which most drivers will
either reject outright or, worse, bind to the wrong value.

## Resources

- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [`database/sql`](https://pkg.go.dev/database/sql) — the driver-facing shape (`query string, args ...any`) this builder's output is meant to feed.
- [`fmt.Fprintf`](https://pkg.go.dev/fmt#Fprintf) — writing formatted placeholders directly into a `strings.Builder`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-tenant-key-sharding-variadic.md](17-tenant-key-sharding-variadic.md) | Next: [19-query-param-encoder-pairs.md](19-query-param-encoder-pairs.md)
