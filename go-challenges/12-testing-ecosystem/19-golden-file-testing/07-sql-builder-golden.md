# Exercise 7: Golden-Test Compiled SQL from a Query Builder

A repository layer builds SQL from a query builder, and a refactor must not
silently change the emitted query or the parameter order — that is how you ship
an injection-shaped bug or a mis-bound argument. You build a small builder and
golden its compiled SQL text, asserting the ordered args slice separately with
`cmp.Diff`.

This module imports `github.com/google/go-cmp`. It is otherwise fully
self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
sqlgold/                   independent module: example.com/sqlgold
  go.mod                   go 1.26; requires github.com/google/go-cmp
  builder.go               Select/Where/OrderBy/Limit -> Build() (sql, args)
  testdata/
    select_users.golden        compiled SQL for the base query
    select_users_region.golden compiled SQL with one added filter
  cmd/
    demo/
      main.go              builds a query and prints the SQL and args
  builder_test.go          SQL golden + cmp.Diff on the args slice
```

Files: `builder.go`, two `testdata/*.golden`, `cmd/demo/main.go`, `builder_test.go`.
Implement: a chainable `Query` with `Select`, `Where`, `OrderBy`, `Limit`, and `Build() (string, []any)` emitting `$1..$n` placeholders.
Test: golden the exact SQL text and assert the args slice with `cmp.Diff`; show that adding a filter is a clean one-line golden diff.
Verify: `go test -count=1 -race ./...`

Set up the module. It depends on go-cmp:

```bash
mkdir -p go-solutions/12-testing-ecosystem/19-golden-file-testing/07-sql-builder-golden/cmd/demo go-solutions/12-testing-ecosystem/19-golden-file-testing/07-sql-builder-golden/testdata
cd go-solutions/12-testing-ecosystem/19-golden-file-testing/07-sql-builder-golden
go get github.com/google/go-cmp/cmp@v0.7.0
```

### Why snapshot compiled SQL

A query builder is a code generator: its output is the SQL string sent to the
database and the ordered slice of bound arguments. Two things about that output
are safety-critical and invisible in the builder's call site. First, the
placeholder numbering (`$1`, `$2`, ...) must line up with the args slice, in
order — swap two `Where` calls and the values bind to the wrong columns, a bug
that no compiler catches and that a golden test catches instantly. Second, the
literal structure of the SQL is the thing an injection review reasons about; if a
refactor starts interpolating a value into the string instead of binding it, the
golden's shape changes and the reviewer sees it. Snapshotting the compiled SQL
turns "did this refactor change the query?" into a reviewable diff.

The contract here is deliberately byte-exact on the SQL text, because the exact
string is what goes over the wire — but canonicalized to a single line, so the
golden is stable and a change is a clean one-line diff. The args slice is
compared semantically with `cmp.Diff`, which prints a readable `-want +got` when
an argument is added, removed, or reordered. The two goldens for the same base
query — one with two filters, one with a third added — demonstrate the payoff:
adding a `Where` produces exactly one new `AND region = $3` clause and one new
arg, and nothing else moves. Note the trailing-newline policy: the builder emits
no newline, so the compare appends exactly one to match the committed file's
single trailing LF.

Create `builder.go`:

```go
package sqlgold

import (
	"fmt"
	"strings"
)

type cond struct {
	col string
	op  string
	val any
}

// Query is a chainable builder that compiles to parameterized SQL.
type Query struct {
	table   string
	cols    []string
	wheres  []cond
	orderBy string
	limit   int
}

// Select starts a query over table selecting cols.
func Select(table string, cols ...string) *Query {
	return &Query{table: table, cols: cols}
}

// Where appends a bound predicate; its value becomes the next $n placeholder.
func (q *Query) Where(col, op string, val any) *Query {
	q.wheres = append(q.wheres, cond{col: col, op: op, val: val})
	return q
}

// OrderBy sets the ORDER BY expression verbatim.
func (q *Query) OrderBy(expr string) *Query { q.orderBy = expr; return q }

// Limit sets a positive LIMIT.
func (q *Query) Limit(n int) *Query { q.limit = n; return q }

// Build compiles the query to a single-line SQL string and its ordered args.
// Placeholders are numbered $1..$n in Where-call order, matching args.
func (q *Query) Build() (string, []any) {
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(strings.Join(q.cols, ", "))
	b.WriteString(" FROM ")
	b.WriteString(q.table)
	var args []any
	for i, c := range q.wheres {
		if i == 0 {
			b.WriteString(" WHERE ")
		} else {
			b.WriteString(" AND ")
		}
		fmt.Fprintf(&b, "%s %s $%d", c.col, c.op, i+1)
		args = append(args, c.val)
	}
	if q.orderBy != "" {
		b.WriteString(" ORDER BY " + q.orderBy)
	}
	if q.limit > 0 {
		fmt.Fprintf(&b, " LIMIT %d", q.limit)
	}
	return b.String(), args
}
```

Now the committed SQL goldens.

Create `testdata/select_users.golden`:

```text
SELECT id, email FROM users WHERE org_id = $1 AND status = $2 ORDER BY created_at DESC LIMIT 20
```

Create `testdata/select_users_region.golden`:

```text
SELECT id, email FROM users WHERE org_id = $1 AND status = $2 AND region = $3 ORDER BY created_at DESC LIMIT 20
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sqlgold"
)

func main() {
	sql, args := sqlgold.Select("users", "id", "email").
		Where("org_id", "=", "org-7").
		Where("status", "=", "active").
		OrderBy("created_at DESC").
		Limit(20).
		Build()
	fmt.Println(sql)
	fmt.Printf("args: %v\n", args)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
SELECT id, email FROM users WHERE org_id = $1 AND status = $2 ORDER BY created_at DESC LIMIT 20
args: [org-7 active]
```

### Tests

Create `builder_test.go`:

```go
package sqlgold

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

var update = flag.Bool("update", false, "regenerate golden files in testdata/")

// goldenSQL compares one line of SQL (plus a single trailing newline) to a file.
func goldenSQL(t *testing.T, name, sql string) {
	t.Helper()
	got := []byte(sql + "\n")
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run: go test -update)", path, err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("SQL golden mismatch for %s (run: go test -update)\n--- got ---\n%s--- want ---\n%s", path, got, want)
	}
}

func TestBuildGolden(t *testing.T) {
	tests := []struct {
		name     string
		build    func() (string, []any)
		golden   string
		wantArgs []any
	}{
		{
			name: "base",
			build: func() (string, []any) {
				return Select("users", "id", "email").
					Where("org_id", "=", "org-7").
					Where("status", "=", "active").
					OrderBy("created_at DESC").
					Limit(20).
					Build()
			},
			golden:   "select_users.golden",
			wantArgs: []any{"org-7", "active"},
		},
		{
			name: "added_filter",
			build: func() (string, []any) {
				return Select("users", "id", "email").
					Where("org_id", "=", "org-7").
					Where("status", "=", "active").
					Where("region", "=", "amer").
					OrderBy("created_at DESC").
					Limit(20).
					Build()
			},
			golden:   "select_users_region.golden",
			wantArgs: []any{"org-7", "active", "amer"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := tc.build()
			goldenSQL(t, tc.golden, sql)
			if diff := cmp.Diff(tc.wantArgs, args); diff != "" {
				t.Errorf("args mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func ExampleQuery_Build() {
	sql, args := Select("accounts", "id").
		Where("email", "=", "a@example.com").
		Build()
	os.Stdout.WriteString(sql + "\n")
	os.Stdout.WriteString(args[0].(string) + "\n")
	// Output:
	// SELECT id FROM accounts WHERE email = $1
	// a@example.com
}
```

## Review

The suite is correct when the SQL golden pins the exact compiled string and the
`cmp.Diff` pins the args in order: together they catch the two silent failures a
builder refactor can introduce — a changed query shape and a mis-numbered or
reordered placeholder. The `added_filter` case is the reviewable-diff argument
made concrete: one extra `Where` yields exactly one new clause and one new arg,
so the golden diff a reviewer reads is a single line. Keep the SQL canonical (one
line, single spaces) so the golden is stable; a builder that emitted incidental
whitespace would churn the golden on every unrelated change. Regenerate with
`go test -update` only when you meant to change the query, then read the SQL diff
as carefully as you would read the query in a migration.

## Resources

- [strings.Builder](https://pkg.go.dev/strings#Builder) — efficient assembly of the SQL string.
- [go-cmp: cmp.Diff](https://pkg.go.dev/github.com/google/go-cmp/cmp#Diff) — the readable diff over the ordered args slice.
- [database/sql parameter placeholders](https://pkg.go.dev/database/sql#hdr-Query_placeholders) — why `$1..$n` binding, not interpolation, is the contract.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-table-driven-per-case-goldens.md](08-table-driven-per-case-goldens.md)
