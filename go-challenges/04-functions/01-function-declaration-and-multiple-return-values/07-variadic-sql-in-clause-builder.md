# Exercise 7: A Variadic IN-Clause Placeholder Builder

Building a parameterized `col IN ($1,$2,...)` fragment from a slice of ids is a
daily backend chore, and it hides a real trap: zero ids must never produce
`col IN ()`, which is a SQL syntax error. This exercise builds
`InClause(column string, args ...any) (string, []any)` — a variadic function that
returns the fragment and the matching bind slice, handling the empty case with an
always-false predicate.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
inclause/                  independent module: example.com/inclause
  go.mod                   go 1.25
  inclause.go              InClause(col string, args ...any) (string, []any)
  cmd/
    demo/
      main.go              one, three, and zero args; a spread of a []any
  inclause_test.go         placeholder text and bind slice for 1/3/0 args; spread test; -race
```

- Files: `inclause.go`, `cmd/demo/main.go`, `inclause_test.go`.
- Implement: `InClause` with a variadic `args ...any` parameter, building `col IN ($1,$2,...,$N)` with `$N` placeholders and returning the args as `[]any`; zero args returns an always-false predicate (`1=0`) and an empty slice, never `IN ()`.
- Test: one arg yields `col IN ($1)` and a 1-element slice; three args yield `col IN ($1,$2,$3)` and a 3-element slice preserving order; zero args yields the always-false predicate; a spread test confirms `ids...` expands into the variadic sink.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/07-variadic-sql-in-clause-builder/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/07-variadic-sql-in-clause-builder
go mod edit -go=1.25
```

### Variadic is a slice, and the empty case is a hazard

Inside `InClause`, `args` is a plain `[]any` — the variadic `...any` is just
call-site sugar for "the caller may pass zero or more arguments, or spread a
slice". A caller writes `InClause("id", 1, 2, 3)` to pass three values, or
`InClause("id", ids...)` to spread an existing `[]any`. The `...` at the call site
is load-bearing: `InClause("id", ids)` without it passes the whole slice as a
*single* `any` argument, which is almost always a bug (you would get `IN ($1)`
binding one value that is itself a slice).

The failure mode this exercise centers on is `len(args) == 0`. The naive builder
joins `len(args)` placeholders with commas, which for zero args yields
`col IN ()` — and `IN ()` is a syntax error in PostgreSQL, MySQL, and SQLite
alike. A query that works in every test with a non-empty list explodes in
production the first time the id list is empty. The correct handling is to return
a predicate that is always false and binds nothing: `1=0`. `WHERE 1=0` is valid
SQL that matches no rows, which is exactly the semantics of "id IN (empty set)".
So `InClause` special-cases zero args before it ever builds a placeholder.

Placeholders here use the PostgreSQL `$N` style (1-indexed), built with
`strconv.Itoa`. The returned `[]any` preserves argument order so it lines up with
`$1..$N` when passed to the driver as `db.Query(sql, args...)`.

Create `inclause.go`:

```go
package inclause

import (
	"strconv"
	"strings"
)

// InClause builds a parameterized predicate for "column IN (...)" using $N
// placeholders, returning the fragment and the bind arguments in order.
//
// With no args it returns an always-false predicate ("1=0") and an empty slice,
// never "column IN ()", which is a SQL syntax error.
func InClause(column string, args ...any) (string, []any) {
	if len(args) == 0 {
		return "1=0", nil
	}

	var b strings.Builder
	b.WriteString(column)
	b.WriteString(" IN (")
	for i := range args {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(i + 1))
	}
	b.WriteByte(')')

	// Copy into a fresh []any so the caller cannot alias the variadic backing
	// array, and the result is independent of how it was called.
	binds := make([]any, len(args))
	copy(binds, args)
	return b.String(), binds
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/inclause"
)

func main() {
	frag, binds := inclause.InClause("id", 1, 2, 3)
	fmt.Printf("%s  binds=%v\n", frag, binds)

	frag, binds = inclause.InClause("id", 42)
	fmt.Printf("%s  binds=%v\n", frag, binds)

	frag, binds = inclause.InClause("id")
	fmt.Printf("%s  binds=%v\n", frag, binds)

	// Spread an existing slice into the variadic parameter with ids...
	ids := []any{"a", "b"}
	frag, binds = inclause.InClause("sku", ids...)
	fmt.Printf("%s  binds=%v\n", frag, binds)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id IN ($1,$2,$3)  binds=[1 2 3]
id IN ($1)  binds=[42]
1=0  binds=[]
sku IN ($1,$2)  binds=[a b]
```

Note the zero-arg line prints `binds=[]` because `fmt` renders a nil slice of
`[]any` as `[]`.

### Tests

Create `inclause_test.go`:

```go
package inclause

import (
	"testing"
)

func TestInClause(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		column    string
		args      []any
		wantFrag  string
		wantBinds int
	}{
		{"one arg", "id", []any{42}, "id IN ($1)", 1},
		{"three args", "id", []any{1, 2, 3}, "id IN ($1,$2,$3)", 3},
		{"zero args", "id", nil, "1=0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			frag, binds := InClause(tc.column, tc.args...)
			if frag != tc.wantFrag {
				t.Fatalf("fragment = %q, want %q", frag, tc.wantFrag)
			}
			if len(binds) != tc.wantBinds {
				t.Fatalf("len(binds) = %d, want %d", len(binds), tc.wantBinds)
			}
		})
	}
}

func TestInClauseNeverEmptyParens(t *testing.T) {
	t.Parallel()
	frag, binds := InClause("id")
	if frag == "id IN ()" {
		t.Fatal("emitted 'id IN ()', which is a SQL syntax error")
	}
	if frag != "1=0" {
		t.Fatalf("fragment = %q, want always-false 1=0", frag)
	}
	if len(binds) != 0 {
		t.Fatalf("len(binds) = %d, want 0", len(binds))
	}
}

func TestInClausePreservesOrder(t *testing.T) {
	t.Parallel()
	_, binds := InClause("id", "x", "y", "z")
	if len(binds) != 3 || binds[0] != "x" || binds[1] != "y" || binds[2] != "z" {
		t.Fatalf("binds = %v, want [x y z] in order", binds)
	}
}

func TestInClauseSpread(t *testing.T) {
	t.Parallel()
	ids := []any{10, 20}
	// Spreading ids... must expand into the variadic sink as two args.
	frag, binds := InClause("id", ids...)
	if frag != "id IN ($1,$2)" {
		t.Fatalf("fragment = %q, want id IN ($1,$2)", frag)
	}
	if len(binds) != 2 {
		t.Fatalf("len(binds) = %d, want 2 after spread", len(binds))
	}
}
```

## Review

`InClause` is correct when N args produce `col IN ($1,...,$N)` with an N-element
bind slice in order, and zero args produce `1=0` with an empty slice.
`TestInClauseNeverEmptyParens` is the reason this exercise exists: it proves the
empty case does not emit `IN ()`, the bug that passes every test with a populated
list and then takes down the query the day the list is empty.

The mistakes cluster around the variadic call site. Forgetting the `...` when
spreading a slice (`InClause("id", ids)`) passes the slice as one argument and
binds a slice-of-values as a single parameter — `TestInClauseSpread` demonstrates
the correct spread. And copying the args into a fresh `[]any` rather than
returning the variadic slice directly avoids the caller aliasing the backing array
of a literal call. Run `go test -race`; the function is pure, so it is trivially
concurrency-safe, which the table subtests exercise in parallel.

## Resources

- [Go Spec: Passing arguments to ... parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters) — the variadic and spread rules.
- [strings.Builder](https://pkg.go.dev/strings#Builder) — efficient fragment assembly without repeated allocation.
- [database/sql: query with placeholders](https://pkg.go.dev/database/sql#DB.Query) — why the `[]any` is passed as `args...` to the driver.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-constructor-returns-cleanup-func.md](06-constructor-returns-cleanup-func.md) | Next: [08-generic-retry-wrapper.md](08-generic-retry-wrapper.md)
