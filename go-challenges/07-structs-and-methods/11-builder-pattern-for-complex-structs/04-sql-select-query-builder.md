# Exercise 4: Parameterized SQL SELECT Builder (No Injection)

Assembling SQL by hand is where builders earn their keep and where a careless one
opens a hole. This module builds a SELECT query builder with one hard invariant:
user values never enter the generated SQL text — they go into a parallel args slice
addressed by placeholders, so the output is safe to hand to `database/sql`'s
`QueryContext`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
sqlbuild/                   independent module: example.com/sqlbuild
  go.mod                    go 1.26
  builder.go                package sqlbuild: Select, Where, OrderBy, Limit, Build
  cmd/
    demo/
      main.go               runnable demo: a multi-predicate query with args
  builder_test.go           golden-string tests, args count, no-table negative
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: a builder whose `Where(col, val)` appends an AND-joined predicate plus a numbered placeholder (`$1`, `$2`, ...) and pushes `val` onto an `[]any` args slice; `OrderBy(col)` and `Limit(n)` append their clauses; `Build()` returns `(sql string, args []any, err error)`, assembling text with `strings.Builder` and never concatenating a user value into the text.
- Test: golden SQL for one predicate, three predicates in AND order, with ORDER BY and LIMIT; `Build` with no table returns `ErrNoTable`; the placeholder count equals `len(args)`; no user value string ever appears in the SQL text.
- Verify: `go test -count=1 -race ./...`

### The security invariant, structurally

The builder holds two parallel accumulators: a `strings.Builder` for the SQL text
and an `[]any` for the values. `Where("email", v)` writes the *column name* and a
placeholder into the text (`email = $1`) and pushes `v` into args. The value `v`
never touches the text — not even for "just this one trusted column". That is the
whole defense: `database/sql` sends the statement and the args over separate
channels, so a value that happens to contain `'; DROP TABLE users; --` is bound as
a literal string parameter, never parsed as SQL. Placeholder numbering is
sequential and driven by `len(args)+1` at the moment a predicate is added, so the
text and the slice stay in lock-step. Column and table *names* are structural, not
data, and are written into the text directly; if you needed dynamic column names
from untrusted input you would validate them against an allowlist, never
placeholder them (placeholders bind values, not identifiers).

`strings.Builder` is the right assembler because it avoids the quadratic cost of
`+=` string concatenation: it appends into a growing byte buffer and materializes
the string once at `String()`. `fmt.Fprintf(&b, ...)` works directly against it
because `*strings.Builder` implements `io.Writer`.

Create `builder.go`:

```go
package sqlbuild

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrNoTable is returned by Build when Select was never given a table.
var ErrNoTable = errors.New("no table specified")

// ErrNoColumns is returned by Build when no columns were selected.
var ErrNoColumns = errors.New("no columns specified")

// Builder assembles a parameterized SELECT. User values go into args, never into
// the SQL text.
type Builder struct {
	table   string
	columns []string
	wheres  []string
	args    []any
	orderBy string
	limit   int
	hasLim  bool
}

// Select starts a query for the given columns.
func Select(columns ...string) *Builder {
	return &Builder{columns: columns}
}

// From sets the table.
func (b *Builder) From(table string) *Builder {
	b.table = table
	return b
}

// Where appends "col = $N AND ..." to the predicate and binds val as $N.
func (b *Builder) Where(col string, val any) *Builder {
	b.args = append(b.args, val)
	b.wheres = append(b.wheres, fmt.Sprintf("%s = $%d", col, len(b.args)))
	return b
}

// OrderBy sets the ORDER BY column.
func (b *Builder) OrderBy(col string) *Builder {
	b.orderBy = col
	return b
}

// Limit sets a row limit.
func (b *Builder) Limit(n int) *Builder {
	b.limit = n
	b.hasLim = true
	return b
}

// Build assembles the SQL text and returns it with the parallel args slice.
func (b *Builder) Build() (string, []any, error) {
	var errs []error
	if b.table == "" {
		errs = append(errs, ErrNoTable)
	}
	if len(b.columns) == 0 {
		errs = append(errs, ErrNoColumns)
	}
	if err := errors.Join(errs...); err != nil {
		return "", nil, err
	}

	var sb strings.Builder
	sb.Grow(64)
	sb.WriteString("SELECT ")
	sb.WriteString(strings.Join(b.columns, ", "))
	sb.WriteString(" FROM ")
	sb.WriteString(b.table)
	if len(b.wheres) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(b.wheres, " AND "))
	}
	if b.orderBy != "" {
		sb.WriteString(" ORDER BY ")
		sb.WriteString(b.orderBy)
	}
	if b.hasLim {
		sb.WriteString(" LIMIT ")
		sb.WriteString(strconv.Itoa(b.limit))
	}

	// Return a copy of args so the caller owns it.
	args := make([]any, len(b.args))
	copy(args, b.args)
	return sb.String(), args, nil
}
```

### The runnable demo

The demo builds a realistic filtered query and prints the SQL and its args
separately, exactly as you would pass them to a driver.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sqlbuild"
)

func main() {
	sql, args, err := sqlbuild.
		Select("id", "email", "status").
		From("users").
		Where("status", "active").
		Where("org_id", 42).
		OrderBy("created_at").
		Limit(10).
		Build()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(sql)
	fmt.Printf("args: %v\n", args)

	if _, _, err := sqlbuild.Select("id").Build(); err != nil {
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
SELECT id, email, status FROM users WHERE status = $1 AND org_id = $2 ORDER BY created_at LIMIT 10
args: [active 42]
```

The rejected build prints its error on a fourth line:

```
rejected: no table specified
```

### Tests

The golden-string tests pin the exact generated SQL for one and three predicates,
with ORDER BY and LIMIT. The security test asserts a value containing SQL never
appears in the text — only its placeholder does — and that the placeholder count
equals `len(args)`.

Create `builder_test.go`:

```go
package sqlbuild

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBuildOnePredicate(t *testing.T) {
	t.Parallel()

	sql, args, err := Select("id", "name").From("users").Where("id", 7).Build()
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT id, name FROM users WHERE id = $1"
	if sql != want {
		t.Fatalf("sql = %q, want %q", sql, want)
	}
	if len(args) != 1 || args[0] != 7 {
		t.Fatalf("args = %v, want [7]", args)
	}
}

func TestBuildThreePredicatesInOrder(t *testing.T) {
	t.Parallel()

	sql, args, err := Select("id").
		From("orders").
		Where("status", "paid").
		Where("org_id", 3).
		Where("region", "eu").
		OrderBy("created_at").
		Limit(50).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	want := "SELECT id FROM orders WHERE status = $1 AND org_id = $2 AND region = $3 ORDER BY created_at LIMIT 50"
	if sql != want {
		t.Fatalf("sql = %q\nwant %q", sql, want)
	}
	if len(args) != 3 {
		t.Fatalf("len(args) = %d, want 3", len(args))
	}
}

func TestPlaceholderCountEqualsArgs(t *testing.T) {
	t.Parallel()

	sql, args, err := Select("id").
		From("t").
		Where("a", 1).
		Where("b", 2).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(sql, "$"); got != len(args) {
		t.Fatalf("placeholders = %d, args = %d; must be equal", got, len(args))
	}
}

func TestUserValueNeverInText(t *testing.T) {
	t.Parallel()

	evil := "'; DROP TABLE users; --"
	sql, args, err := Select("id").From("t").Where("name", evil).Build()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, evil) {
		t.Fatalf("user value leaked into SQL text: %q", sql)
	}
	if strings.Contains(sql, "DROP") {
		t.Fatalf("SQL fragment from a value appeared in text: %q", sql)
	}
	if len(args) != 1 || args[0] != evil {
		t.Fatalf("value should be bound as an arg, got %v", args)
	}
}

func TestBuildRejectsNoTable(t *testing.T) {
	t.Parallel()

	if _, _, err := Select("id").Build(); !errors.Is(err, ErrNoTable) {
		t.Fatalf("err = %v, want ErrNoTable", err)
	}
}

func TestBuildRejectsNoColumns(t *testing.T) {
	t.Parallel()

	if _, _, err := Select().From("t").Build(); !errors.Is(err, ErrNoColumns) {
		t.Fatalf("err = %v, want ErrNoColumns", err)
	}
}

func ExampleBuilder_Build() {
	sql, args, _ := Select("id").From("users").Where("id", 1).Build()
	fmt.Println(sql)
	fmt.Println(args)
	// Output:
	// SELECT id FROM users WHERE id = $1
	// [1]
}
```

## Review

The builder is correct when the invariant holds structurally: `TestUserValueNeverInText`
feeds a value containing a `DROP TABLE` payload and proves neither the value nor any
fragment of it appears in the SQL text — it is bound as `args[0]` instead. The
golden-string tests pin the exact text so a formatting regression (a missing space,
a wrong join) fails loudly, and `TestPlaceholderCountEqualsArgs` guards the
lock-step between placeholders and args that a driver relies on. `Build` also
aggregates its structural errors (`ErrNoTable`, `ErrNoColumns`) with `errors.Join`,
each matchable with `errors.Is`. The mistake to never make: concatenating a value
into the text "just this once" — that is precisely the injection this design
prevents. Run `go test -race` to confirm.

## Resources

- [strings.Builder](https://pkg.go.dev/strings#Builder) — efficient text assembly with `WriteString`, `Grow`, and `String`.
- [database/sql: Avoiding SQL injection](https://go.dev/doc/database/sql-injection) — why values must be placeholders, not concatenated text.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating the structural validation errors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-staged-typestate-builder.md](05-staged-typestate-builder.md)
