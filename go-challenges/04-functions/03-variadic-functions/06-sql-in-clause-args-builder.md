# Exercise 6: Dynamic SQL IN(...) Placeholder and Variadic Arg Builder

A repository fetching a batch of rows by id needs `WHERE id IN (?, ?, ?)` with one
placeholder per id and a matching `[]any` of values to splat into
`db.QueryContext(ctx, query, args...)`. You build `InClause(ids []int) (string,
[]any)` that produces exactly that — and you make it injection-safe and impossible
to desynchronize placeholder count from argument count.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
inclause/                  independent module: example.com/inclause
  go.mod                   go 1.25
  inclause.go              InClause(ids []int) (string, []any)
  cmd/
    demo/
      main.go              runnable demo: build clause + args, show the full query
  inclause_test.go         table tests + property: placeholder count == len(args)
```

- Files: `inclause.go`, `cmd/demo/main.go`, `inclause_test.go`.
- Implement: `InClause(ids []int) (string, []any)` returning `"(?, ?, ?)"` and the ids as `[]any`; an empty slice returns a form that cannot produce invalid SQL.
- Test: `InClause([]int{1,2,3})` gives `"(?, ?, ?)"` and args `[1 2 3]`; empty input returns the guarded sentinel with empty args; the placeholder count always equals `len(args)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/06-sql-in-clause-args-builder/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/06-sql-in-clause-args-builder
go mod edit -go=1.25
```

### Why placeholders, and why the count must equal len(args)

The reason `database/sql` separates the query string from the arguments is
injection safety. A value is bound to a `?` (or `$1`) placeholder out-of-band by
the driver, so a hostile id like `1); DROP TABLE orders; --` is treated as data,
never as SQL. Building the `IN` list by string-concatenating the values —
`"id IN (" + strings.Join(strIDs, ",") + ")"` — throws that protection away and
reintroduces both injection and type bugs. So `InClause` emits one `?` per id and
returns the ids as a `[]any` that the caller splats into `QueryContext`.

The non-negotiable invariant is that the placeholder count equals `len(args)`. If
they diverge, the driver rejects the statement at runtime with "sql: expected N
arguments, got M". `InClause` guarantees they cannot diverge by deriving both from
the same loop: each iteration appends one `?` to the builder and one value to the
args slice. The property test asserts this for many sizes.

The empty case needs care. `IN ()` is a syntax error in SQL, and an empty
placeholder list would produce exactly that. `InClause(nil)` therefore returns the
guarded sentinel `"(NULL)"` with an empty (nil) arg slice: `id IN (NULL)` is valid
SQL that matches no rows (because `id = NULL` is never true), which is the correct
semantics for "filter by an empty set". This turns a would-be runtime SQL error
into a well-defined empty result. A repository can also choose to short-circuit and
skip the query entirely when `len(ids) == 0`; returning valid, harmless SQL means
callers that do not short-circuit still stay safe.

The actual `db.QueryContext(ctx, "SELECT ... WHERE id IN "+clause, args...)` call
needs a live driver, so this module keeps `InClause` pure and shows the call shape
in prose; the tests and demo exercise the string-and-args construction directly.

Create `inclause.go`:

```go
// inclause.go
package inclause

import "strings"

// InClause builds a parameterized SQL IN list from a slice of ids. It returns the
// placeholder group (e.g. "(?, ?, ?)") and a matching []any of the ids to splat
// into db.QueryContext. For an empty slice it returns "(NULL)" and nil args, which
// is valid SQL that matches no rows. The number of placeholders always equals
// len(args).
func InClause(ids []int) (string, []any) {
	if len(ids) == 0 {
		return "(NULL)", nil
	}

	var b strings.Builder
	b.WriteByte('(')
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('?')
		args = append(args, id)
	}
	b.WriteByte(')')
	return b.String(), args
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/inclause"
)

func main() {
	clause, args := inclause.InClause([]int{10, 20, 30})
	fmt.Printf("SELECT id, total FROM orders WHERE id IN %s\n", clause)
	fmt.Printf("args: %v (len %d)\n", args, len(args))

	empty, emptyArgs := inclause.InClause(nil)
	fmt.Printf("empty clause: %s (args len %d)\n", empty, len(emptyArgs))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
SELECT id, total FROM orders WHERE id IN (?, ?, ?)
args: [10 20 30] (len 3)
empty clause: (NULL) (args len 0)
```

The real repository call is one line more — the clause is concatenated into the
query and the args are splatted:

```text
rows, err := db.QueryContext(ctx,
    "SELECT id, total FROM orders WHERE id IN "+clause, args...)
```

### Tests

`TestPlaceholderCountEqualsArgs` is the property test: across a range of sizes it
counts the `?` characters in the clause and asserts the count equals `len(args)`.
That invariant is what keeps the driver from rejecting the statement.

Create `inclause_test.go`:

```go
// inclause_test.go
package inclause

import (
	"fmt"
	"strings"
	"testing"
)

func TestInClause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ids        []int
		wantClause string
		wantArgs   []any
	}{
		{"three ids", []int{1, 2, 3}, "(?, ?, ?)", []any{1, 2, 3}},
		{"single id", []int{7}, "(?)", []any{7}},
		{"empty", nil, "(NULL)", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clause, args := InClause(tc.ids)
			if clause != tc.wantClause {
				t.Errorf("clause = %q, want %q", clause, tc.wantClause)
			}
			if fmt.Sprint(args) != fmt.Sprint(tc.wantArgs) {
				t.Errorf("args = %v, want %v", args, tc.wantArgs)
			}
		})
	}
}

func TestArgsPreserveOrder(t *testing.T) {
	t.Parallel()

	_, args := InClause([]int{30, 10, 20})
	want := []any{30, 10, 20}
	if fmt.Sprint(args) != fmt.Sprint(want) {
		t.Fatalf("args = %v, want %v (order must be preserved)", args, want)
	}
}

func TestPlaceholderCountEqualsArgs(t *testing.T) {
	t.Parallel()

	for n := 1; n <= 50; n++ {
		ids := make([]int, n)
		for i := range ids {
			ids[i] = i
		}
		clause, args := InClause(ids)
		if got := strings.Count(clause, "?"); got != len(args) {
			t.Fatalf("n=%d: %d placeholders but %d args", n, got, len(args))
		}
	}
}

func Example() {
	clause, args := InClause([]int{1, 2, 3})
	fmt.Println(clause, args)
	// Output: (?, ?, ?) [1 2 3]
}
```

## Review

`InClause` is correct when it emits exactly one `?` per id, returns the ids as an
ordered `[]any`, and turns the empty set into harmless `(NULL)` rather than the
`IN ()` syntax error. The one invariant that must never break is placeholder count
equals arg count, guaranteed here by deriving both from the same loop and asserted
by the property test. The senior discipline is to never interpolate values into
the query text — placeholders plus a splatted `...any` are the injection-safe
contract that `database/sql` is built around. Run `go test -race`.

## Resources

- [`database/sql`: `DB.QueryContext`](https://pkg.go.dev/database/sql#DB.QueryContext)
- [Go database tutorial: querying with parameters](https://go.dev/doc/database/querying)
- [`strings.Builder`](https://pkg.go.dev/strings#Builder)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-structured-log-attrs-helper.md](05-structured-log-attrs-helper.md) | Next: [07-http-middleware-chain.md](07-http-middleware-chain.md)
