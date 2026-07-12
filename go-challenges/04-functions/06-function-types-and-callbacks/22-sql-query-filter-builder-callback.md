# Exercise 22: SQL Query Filter Builder using Function Type Callbacks

**Nivel: Intermedio** — validacion rapida (un test corto).

A search endpoint lets a caller stack filters — status equals, amount at
least, region in a list — and the backend has to turn whichever subset the
caller sent into one parameterized `WHERE` clause with placeholders in the
right order and arguments to match. This module builds that as a
`QueryBuilder` driven by `FilterFunc` callbacks, one per filter kind.

## What you'll build

```text
sqlfilter/                  independent module: example.com/sql-query-filter-builder-callback
  go.mod                     go 1.24
  sqlfilter.go                 type QueryBuilder, type FilterFunc, func New, (QueryBuilder) Apply, Eq, Gte, Lte, In, (QueryBuilder) Build
  cmd/
    demo/
      main.go                  runnable demo: three filters composed, plus an empty IN
  sqlfilter_test.go            table test: each filter alone, composed in order, empty IN, and no filters at all
```

Files: `sqlfilter.go`, `cmd/demo/main.go`, `sqlfilter_test.go`.
Implement: `type FilterFunc func(q *QueryBuilder)`, `func New(table string) *QueryBuilder`, `func (q *QueryBuilder) Apply(filters ...FilterFunc) *QueryBuilder`, the filter constructors `Eq`, `Gte`, `Lte`, `In`, and `func (q *QueryBuilder) Build() (string, []any)`.
Test: `Eq` alone, `Gte`+`Lte` together, `In` with several values, `In` with zero values (must not produce invalid SQL), all four composed in one `Apply` call with the SQL and argument order both pinned, and `Build` with no filters applied at all.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/22-sql-query-filter-builder-callback/cmd/demo
cd go-solutions/04-functions/06-function-types-and-callbacks/22-sql-query-filter-builder-callback
go mod edit -go=1.24
```

### Why a filter is a callback that mutates the builder, not a struct the builder inspects

The tempting alternative is a `Filter` struct with a `Column`, an `Op`
enum, and a `Value`, which `Build` then switches over to render SQL. That
puts every operator `Build` will ever need to know about inside `Build`
itself, and a new operator means editing that switch. `FilterFunc func(q
*QueryBuilder)` inverts it: `Eq`, `Gte`, `Lte`, and `In` each know how to
append themselves — their own SQL fragment and their own argument, in that
order — and `Build` never branches on which filter it is looking at, only
walks whatever `wheres`/`args` are already there. `Apply` is what makes this
compose: it takes any number of `FilterFunc`s and runs them in the order
given, which is also the order their placeholders and arguments end up in —
critical, since a `database/sql` driver matches `?` placeholders to
arguments positionally, and a reordering bug there is a silent wrong-answer
bug, not a compile error.

Create `sqlfilter.go`:

```go
// Package sqlfilter builds parameterized SQL WHERE clauses by composing
// small FilterFunc callbacks against a QueryBuilder.
package sqlfilter

import (
	"fmt"
	"strings"
)

// QueryBuilder accumulates a table name, WHERE conditions, and their
// positional arguments, in the order filters are applied.
type QueryBuilder struct {
	table  string
	wheres []string
	args   []any
}

// FilterFunc appends one WHERE condition (and its arguments) to a
// QueryBuilder. Every filter constructor below returns a FilterFunc.
type FilterFunc func(q *QueryBuilder)

// New starts a QueryBuilder for the given table.
func New(table string) *QueryBuilder {
	return &QueryBuilder{table: table}
}

// Apply runs every filter against q, in order, and returns q for chaining.
func (q *QueryBuilder) Apply(filters ...FilterFunc) *QueryBuilder {
	for _, f := range filters {
		f(q)
	}
	return q
}

// Eq filters col = val.
func Eq(col string, val any) FilterFunc {
	return func(q *QueryBuilder) {
		q.wheres = append(q.wheres, col+" = ?")
		q.args = append(q.args, val)
	}
}

// Gte filters col >= val.
func Gte(col string, val any) FilterFunc {
	return func(q *QueryBuilder) {
		q.wheres = append(q.wheres, col+" >= ?")
		q.args = append(q.args, val)
	}
}

// Lte filters col <= val.
func Lte(col string, val any) FilterFunc {
	return func(q *QueryBuilder) {
		q.wheres = append(q.wheres, col+" <= ?")
		q.args = append(q.args, val)
	}
}

// In filters col IN (v1, v2, ...). An empty vals produces "1 = 0" (a
// WHERE clause that matches nothing), since "col IN ()" is not valid SQL
// and "no allowed values" should mean "no rows match," not "all rows."
func In(col string, vals []any) FilterFunc {
	return func(q *QueryBuilder) {
		if len(vals) == 0 {
			q.wheres = append(q.wheres, "1 = 0")
			return
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(vals)), ", ")
		q.wheres = append(q.wheres, fmt.Sprintf("%s IN (%s)", col, placeholders))
		q.args = append(q.args, vals...)
	}
}

// Build renders the accumulated filters into a parameterized SQL string
// and its positional argument slice, in the order filters were applied.
func (q *QueryBuilder) Build() (string, []any) {
	query := "SELECT * FROM " + q.table
	if len(q.wheres) > 0 {
		query += " WHERE " + strings.Join(q.wheres, " AND ")
	}
	return query, q.args
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sql-query-filter-builder-callback"
)

func main() {
	query, args := sqlfilter.New("orders").
		Apply(
			sqlfilter.Eq("status", "shipped"),
			sqlfilter.Gte("amount", 100),
			sqlfilter.In("region", []any{"us-east", "us-west"}),
		).
		Build()

	fmt.Println(query)
	fmt.Println(args)

	empty, emptyArgs := sqlfilter.New("orders").
		Apply(sqlfilter.In("region", nil)).
		Build()
	fmt.Println(empty)
	fmt.Println(emptyArgs)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
SELECT * FROM orders WHERE status = ? AND amount >= ? AND region IN (?, ?)
[shipped 100 us-east us-west]
SELECT * FROM orders WHERE 1 = 0
[]
```

### Tests

Create `sqlfilter_test.go`:

```go
package sqlfilter

import (
	"slices"
	"testing"
)

func TestEqAppendsEqualityClause(t *testing.T) {
	t.Parallel()
	query, args := New("users").Apply(Eq("id", 42)).Build()
	if query != "SELECT * FROM users WHERE id = ?" {
		t.Fatalf("query = %q", query)
	}
	if !slices.Equal(args, []any{42}) {
		t.Fatalf("args = %v, want [42]", args)
	}
}

func TestGteAndLteAppendRangeClauses(t *testing.T) {
	t.Parallel()
	query, args := New("orders").Apply(Gte("amount", 10), Lte("amount", 100)).Build()
	if query != "SELECT * FROM orders WHERE amount >= ? AND amount <= ?" {
		t.Fatalf("query = %q", query)
	}
	if !slices.Equal(args, []any{10, 100}) {
		t.Fatalf("args = %v, want [10 100]", args)
	}
}

func TestInAppendsPlaceholderList(t *testing.T) {
	t.Parallel()
	query, args := New("orders").Apply(In("region", []any{"us", "eu", "apac"})).Build()
	if query != "SELECT * FROM orders WHERE region IN (?, ?, ?)" {
		t.Fatalf("query = %q", query)
	}
	if !slices.Equal(args, []any{"us", "eu", "apac"}) {
		t.Fatalf("args = %v, want [us eu apac]", args)
	}
}

func TestInWithNoValuesMatchesNoRows(t *testing.T) {
	t.Parallel()
	query, args := New("orders").Apply(In("region", nil)).Build()
	if query != "SELECT * FROM orders WHERE 1 = 0" {
		t.Fatalf("query = %q, want a clause that matches nothing", query)
	}
	if len(args) != 0 {
		t.Fatalf("args = %v, want none", args)
	}
}

func TestApplyComposesFiltersInOrder(t *testing.T) {
	t.Parallel()
	query, args := New("orders").
		Apply(
			Eq("status", "shipped"),
			Gte("amount", 100),
			In("region", []any{"us-east", "us-west"}),
		).
		Build()

	wantQuery := "SELECT * FROM orders WHERE status = ? AND amount >= ? AND region IN (?, ?)"
	if query != wantQuery {
		t.Fatalf("query = %q, want %q", query, wantQuery)
	}
	wantArgs := []any{"shipped", 100, "us-east", "us-west"}
	if !slices.Equal(args, wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
}

func TestBuildWithNoFiltersHasNoWhereClause(t *testing.T) {
	t.Parallel()
	query, args := New("orders").Build()
	if query != "SELECT * FROM orders" {
		t.Fatalf("query = %q, want no WHERE clause", query)
	}
	if len(args) != 0 {
		t.Fatalf("args = %v, want none", args)
	}
}
```

## Review

`Build` does exactly one thing regardless of which filters were applied:
join `wheres` with `AND` and hand back `args` in the order they were
appended — every operator-specific decision already happened inside `Eq`,
`Gte`, `Lte`, or `In` when `Apply` called it. `TestApplyComposesFiltersInOrder`
is the test that would catch a real production bug: if `Eq`'s argument and
`In`'s arguments ever ended up out of sync with their placeholders'
left-to-right order, the query would still be syntactically valid SQL and
would still run — it would just silently filter on the wrong values. The
empty-`In` case matters for the same reason a different way: `col IN ()` is
a SQL syntax error on most engines, so `In` has to special-case zero values
into a clause that is both valid and semantically correct ("nothing
matches"), not fall through to `IN ()` and break the query at the database.

## Resources

- [database/sql](https://pkg.go.dev/database/sql)
- [Go Specification: Function types](https://go.dev/ref/spec#Function_types)
- [OWASP: SQL Injection Prevention (parameterized queries)](https://cheatsheetseries.owasp.org/cheatsheets/SQL_Injection_Prevention_Cheat_Sheet.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-media-type-codec-strategy.md](21-media-type-codec-strategy.md) | Next: [23-tenant-context-callback-extractor.md](23-tenant-context-callback-extractor.md)
