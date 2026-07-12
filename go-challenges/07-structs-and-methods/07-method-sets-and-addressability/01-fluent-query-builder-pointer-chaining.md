# Exercise 1: Fluent SQL Query Builder with Pointer-Receiver Chaining

A fluent builder is the canonical place a senior engineer meets the method-set
rules in production: every chainable method must return `*Builder`, not
`Builder`, or the chain mutates a copy and silently drops the clauses you added
earlier. This module builds that query builder, proves the chain contract with a
table-driven test, and pins validation at the terminal `Build` method.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
querybuilder/                  independent module: example.com/querybuilder
  go.mod                       module path + go directive
  builder.go                   type Builder; New, Where, OrderBy, Limit, Build (all on *Builder)
  cmd/
    demo/
      main.go                  runnable demo: chain clauses, render SQL
  builder_test.go              table-driven render tests + terminal-validation test
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: `New(table)` returning `*Builder`, chainable `Where`/`OrderBy`/`Limit` each returning `*Builder`, and a terminal `Build() (string, error)` that validates and renders.
- Test: empty query, single and multi `WHERE`, `ORDER BY`, `LIMIT`, the full chain, empty-table rejection, and a table-driven test pinning exact rendered SQL.
- Verify: `gofmt -l .`, `go vet ./...`, `go test -count=1 -race ./...`.

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/07-method-sets-and-addressability/01-fluent-query-builder-pointer-chaining/cmd/demo
cd go-solutions/07-structs-and-methods/07-method-sets-and-addressability/01-fluent-query-builder-pointer-chaining
```

### Why a chainable method must return *T

The builder accumulates state — a list of `WHERE` conditions, a list of
`ORDER BY` columns, a `LIMIT` — across several method calls that read as one
expression: `New("users").Where("active").OrderBy("name").Build()`. For that to
work, each call must mutate and then hand back the *same* builder so the next
call sees the accumulated state.

If `Where` were declared `func (b *Builder) Where(s string) Builder` (returning a
value), the return would be a copy of the builder. The next `.OrderBy` in the
chain would run against that copy; the copy's mutation would be discarded when
its temporary died; and the original builder would never see the `ORDER BY`. The
chain would compile and run and quietly produce the wrong SQL. Returning
`*Builder` keeps every call operating on one heap-allocated builder, so the
clauses accumulate. All the mutating methods take a pointer receiver for the same
reason: they append to slices and set fields, which must be visible to the next
call.

`Build` is the terminal method. It does not return `*Builder`; it validates
(rejecting an empty table with a sentinel error) and renders the SQL string. That
split — mutators return `*Builder` to keep chaining, the terminal returns the
result — is the shape of every fluent API in Go.

Create `builder.go`:

```go
package querybuilder

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNoTable is returned by Build when the builder has no table set.
var ErrNoTable = errors.New("querybuilder: table is required")

// Builder accumulates SQL SELECT clauses across chained calls. Every mutating
// method takes a pointer receiver and returns *Builder so the chain mutates one
// builder rather than a copy.
type Builder struct {
	table  string
	wheres []string
	orders []string
	limit  int
}

// New returns a *Builder for the given table. It returns a pointer because all
// the chainable methods are in the method set of *Builder only.
func New(table string) *Builder {
	return &Builder{table: table}
}

// Where adds a condition; multiple conditions are joined with AND.
func (b *Builder) Where(condition string) *Builder {
	b.wheres = append(b.wheres, condition)
	return b
}

// OrderBy adds an ORDER BY column; multiple columns render comma-separated.
func (b *Builder) OrderBy(column string) *Builder {
	b.orders = append(b.orders, column)
	return b
}

// Limit sets the LIMIT; a value <= 0 renders no LIMIT clause.
func (b *Builder) Limit(n int) *Builder {
	b.limit = n
	return b
}

// Build is the terminal method: it validates and renders the SQL. It wraps
// ErrNoTable with %w so callers can assert with errors.Is.
func (b *Builder) Build() (string, error) {
	if b.table == "" {
		return "", fmt.Errorf("build query: %w", ErrNoTable)
	}
	parts := []string{"SELECT * FROM " + b.table}
	if len(b.wheres) > 0 {
		parts = append(parts, "WHERE "+strings.Join(b.wheres, " AND "))
	}
	if len(b.orders) > 0 {
		parts = append(parts, "ORDER BY "+strings.Join(b.orders, ", "))
	}
	if b.limit > 0 {
		parts = append(parts, fmt.Sprintf("LIMIT %d", b.limit))
	}
	return strings.Join(parts, " "), nil
}
```

### The runnable demo

The demo builds a representative query and prints it, so you can watch the chain
accumulate clauses into one string.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/querybuilder"
)

func main() {
	q, err := querybuilder.New("users").
		Where("active = true").
		Where("age > 18").
		OrderBy("name").
		Limit(10).
		Build()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(q)

	if _, err := querybuilder.New("").Build(); err != nil {
		fmt.Println("empty table:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
SELECT * FROM users WHERE active = true AND age > 18 ORDER BY name LIMIT 10
empty table: build query: querybuilder: table is required
```

### Tests

The tests preserve every original case (empty query, single and multiple
`WHERE`, `ORDER BY`, `LIMIT`, the full chain, empty-table rejection) and add a
table-driven test asserting the exact rendered SQL for representative chains, plus
`TestBuilderRejectsEmptyTableAtBuild`, which pins validation at the terminal
method by asserting the wrapped sentinel with `errors.Is`.

Create `builder_test.go`:

```go
package querybuilder

import (
	"errors"
	"strings"
	"testing"
)

func TestBuilderRender(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func() (string, error)
		want  string
	}{
		{"empty", func() (string, error) { return New("users").Build() }, "SELECT * FROM users"},
		{"single where", func() (string, error) { return New("users").Where("active = true").Build() }, "SELECT * FROM users WHERE active = true"},
		{"multi where", func() (string, error) {
			return New("users").Where("active = true").Where("age > 18").Build()
		}, "SELECT * FROM users WHERE active = true AND age > 18"},
		{"order by", func() (string, error) {
			return New("users").OrderBy("name").OrderBy("age DESC").Build()
		}, "SELECT * FROM users ORDER BY name, age DESC"},
		{"limit", func() (string, error) { return New("users").Limit(10).Build() }, "SELECT * FROM users LIMIT 10"},
		{"full chain", func() (string, error) {
			return New("users").Where("active = true").OrderBy("name").Limit(10).Build()
		}, "SELECT * FROM users WHERE active = true ORDER BY name LIMIT 10"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.build()
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuilderContainsClauses(t *testing.T) {
	t.Parallel()
	q, err := New("users").
		Where("active = true").
		OrderBy("name").
		Limit(10).
		Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SELECT * FROM users", "WHERE active = true", "ORDER BY name", "LIMIT 10"} {
		if !strings.Contains(q, want) {
			t.Fatalf("missing %q in %q", want, q)
		}
	}
}

func TestBuilderRejectsEmptyTableAtBuild(t *testing.T) {
	t.Parallel()
	_, err := New("").Where("x = 1").OrderBy("y").Build()
	if !errors.Is(err, ErrNoTable) {
		t.Fatalf("Build() error = %v, want wrap of ErrNoTable", err)
	}
}
```

## Review

The builder is correct when the whole chain mutates a single builder: every
mutating method takes a pointer receiver and returns `*Builder`, so
`New("users").Where(...).OrderBy(...).Limit(...)` accumulates all clauses into
one object before `Build` renders them. The table-driven `TestBuilderRender`
proves the accumulation by asserting exact SQL; if any method returned `Builder`
by value instead of `*Builder`, the later clauses would vanish and those exact
strings would not match.

The trap to avoid is declaring a chainable method to return `Builder`. It
compiles, so the mistake survives review by eye; only a test that asserts the
full rendered chain catches it. `TestBuilderRejectsEmptyTableAtBuild` pins the
second contract, that validation lives at the terminal method and surfaces as a
wrapped `ErrNoTable` you can match with `errors.Is`, not a bare string. Run
`go vet` and `go test -race` to confirm.

## Resources

- [Go Language Specification: Method sets](https://go.dev/ref/spec#Method_sets) — why pointer methods live only in `*T`'s method set.
- [Go FAQ: Should I define methods on values or pointers?](https://go.dev/doc/faq#methods_on_values_or_pointers) — the guidance behind pointer receivers for mutating methods.
- [`strings.Join`](https://pkg.go.dev/strings#Join) — the exact clause-joining behavior used in `Build`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-repository-interface-pointer-method-set.md](02-repository-interface-pointer-method-set.md)
