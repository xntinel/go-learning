# Exercise 9: Build a Parameterized SQL IN Clause Without Injection

A repository method that queries `WHERE id IN (...)` with a variable number of
ids needs a placeholder list of the right length — `($1,$2,$3)` for
PostgreSQL or `(?,?,?)` for MySQL. The one rule that makes this safe is that the
`strings` package builds only the placeholder *skeleton*; the values never touch
the query text and flow through the driver's argument binding instead.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
sqlin/                          independent module: example.com/sqlin
  go.mod                        go 1.26
  sqlin.go                      InClause ($N, Builder) + InClauseQ (?, Repeat/Join)
  sqlin_test.go                 table test + "only placeholders" guard + benchmark
  cmd/
    demo/
      main.go                   runnable demo showing values passed separately
```

Files: `sqlin.go`, `sqlin_test.go`, `cmd/demo/main.go`.
Implement: `InClause(startIndex, n int) (string, error)` for `$`-style and
`InClauseQ(n int) (string, error)` for `?`-style placeholders.
Test: n=1, n=3, n=0 (error), a `startIndex` offset, and a guard asserting the
output contains only placeholders and separators; a benchmark contrasting
`Builder` with `+`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/05-strings-package/09-sql-in-placeholder-builder/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/05-strings-package/09-sql-in-placeholder-builder
```

### Skeleton in the query, values in the args

The security invariant is absolute: user values are never formatted into SQL
text. `InClause` emits `($1,$2,$3)` and the caller passes the three ids as
`db.Query(query, ids...)`; the driver binds them as parameters, so a value like
`1); DROP TABLE users;--` is data, not syntax. The moment you `fmt.Sprintf` a
value into the query string, you have an injection vector. This module builds
only the fixed part.

Two builders show two `strings` primitives. `InClause` uses `strings.Builder`
with `Grow` to pre-size the buffer, then writes `$`, the number
(`strconv.Itoa`), and `,` in a loop — one allocation for the final string, and
`startIndex` lets the placeholders start at `$5` when earlier parameters already
occupy `$1..$4`. `InClauseQ` uses `strings.Repeat`/`strings.Join` for the
`?`-style list, which is the terse way to make N identical placeholders. Both
guard `n <= 0`: an empty `IN ()` is a SQL syntax error in most engines, so the
caller must special-case an empty id set (usually by skipping the query and
returning no rows). Returning `ErrNoArgs` forces that decision rather than
emitting `()`.

Create `sqlin.go`:

```go
package sqlin

import (
	"fmt"
	"strconv"
	"strings"
)

// ErrNoArgs means n <= 0: an empty IN () list is invalid SQL, so the caller must
// guard the empty case (skip the query) rather than build "()".
var ErrNoArgs = fmt.Errorf("sqlin: IN clause needs at least one placeholder")

// ErrBadStart means startIndex < 1: PostgreSQL placeholders are 1-based.
var ErrBadStart = fmt.Errorf("sqlin: startIndex must be >= 1")

// InClause builds a PostgreSQL-style placeholder list "($k,$k+1,...,$k+n-1)"
// where k is startIndex. It never interpolates values; args go to the driver.
func InClause(startIndex, n int) (string, error) {
	if n <= 0 {
		return "", ErrNoArgs
	}
	if startIndex < 1 {
		return "", ErrBadStart
	}
	var b strings.Builder
	b.Grow(2 + n*5) // '(' ')' plus roomy per-placeholder estimate
	b.WriteByte('(')
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(startIndex + i))
	}
	b.WriteByte(')')
	return b.String(), nil
}

// InClauseQ builds a MySQL-style placeholder list "(?,?,...,?)" with n markers.
func InClauseQ(n int) (string, error) {
	if n <= 0 {
		return "", ErrNoArgs
	}
	markers := make([]string, n)
	for i := range markers {
		markers[i] = "?"
	}
	return "(" + strings.Join(markers, ",") + ")", nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sqlin"
)

func main() {
	ids := []int{101, 102, 103}

	clause, err := sqlin.InClause(1, len(ids))
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	query := "SELECT name FROM users WHERE id IN " + clause

	fmt.Println("query:", query)
	fmt.Println("args: ", ids) // values go to db.Query(query, args...), never into the text

	q, _ := sqlin.InClauseQ(len(ids))
	fmt.Println("mysql:", "SELECT name FROM users WHERE id IN "+q)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
query: SELECT name FROM users WHERE id IN ($1,$2,$3)
args:  [101 102 103]
mysql: SELECT name FROM users WHERE id IN (?,?,?)
```

### Tests

Create `sqlin_test.go`:

```go
package sqlin

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestInClause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		startIndex int
		n          int
		want       string
		wantErr    error
	}{
		{name: "one", startIndex: 1, n: 1, want: "($1)"},
		{name: "three", startIndex: 1, n: 3, want: "($1,$2,$3)"},
		{name: "offset start", startIndex: 5, n: 2, want: "($5,$6)"},
		{name: "empty is error", startIndex: 1, n: 0, wantErr: ErrNoArgs},
		{name: "bad start", startIndex: 0, n: 1, wantErr: ErrBadStart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := InClause(tc.startIndex, tc.n)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("InClause(%d,%d) err = %v, want %v", tc.startIndex, tc.n, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("InClause(%d,%d) unexpected err: %v", tc.startIndex, tc.n, err)
			}
			if got != tc.want {
				t.Fatalf("InClause(%d,%d) = %q, want %q", tc.startIndex, tc.n, got, tc.want)
			}
		})
	}
}

func TestInClauseQ(t *testing.T) {
	t.Parallel()

	got, err := InClauseQ(3)
	if err != nil {
		t.Fatalf("InClauseQ(3): %v", err)
	}
	if got != "(?,?,?)" {
		t.Fatalf("InClauseQ(3) = %q, want %q", got, "(?,?,?)")
	}
	if _, err := InClauseQ(0); !errors.Is(err, ErrNoArgs) {
		t.Fatalf("InClauseQ(0) err = %v, want ErrNoArgs", err)
	}
}

// TestOnlyPlaceholders guards against accidental interpolation: the output must
// contain nothing but '(', ')', ',', '$', and digits.
func TestOnlyPlaceholders(t *testing.T) {
	t.Parallel()

	got, err := InClause(1, 10)
	if err != nil {
		t.Fatalf("InClause: %v", err)
	}
	const allowed = "()$,0123456789"
	for _, r := range got {
		if !strings.ContainsRune(allowed, r) {
			t.Fatalf("InClause produced unexpected rune %q in %q", r, got)
		}
	}
}

func BenchmarkInClauseBuilder(b *testing.B) {
	for range b.N {
		_, _ = InClause(1, 50)
	}
}

// BenchmarkInClausePlus contrasts naive '+' concatenation, which reallocates on
// every append, with the Builder version above.
func BenchmarkInClausePlus(b *testing.B) {
	for range b.N {
		s := "("
		for i := 1; i <= 50; i++ {
			if i > 1 {
				s += ","
			}
			s += "$" + fmt.Sprint(i)
		}
		s += ")"
		_ = s
	}
}

func ExampleInClause() {
	clause, _ := InClause(1, 3)
	fmt.Println("id IN " + clause)
	// Output: id IN ($1,$2,$3)
}
```

## Review

The builder is correct when the placeholder count and numbering are exact
(`$5,$6` for a start offset of 5) and when `n <= 0` is an error, forcing the
caller to guard the empty-set case rather than emit invalid `IN ()`.
`TestOnlyPlaceholders` is the security guard: it proves the output is nothing but
placeholders and separators, so no value could have leaked into the query text.
The `Builder` benchmark against `+` concatenation shows why `Grow` plus one
`String()` beats repeated reallocation. The rule to carry away: `strings` builds
the skeleton; the values ride the driver's parameter binding. Confirm with
`go test -race` and `go test -bench=.`.

## Resources

- [strings.Builder](https://pkg.go.dev/strings#Builder) and [strings.Repeat](https://pkg.go.dev/strings#Repeat).
- [database/sql query parameters](https://pkg.go.dev/database/sql#DB.Query) — how args bind, separate from the query text.
- [Go database/sql tutorial: avoiding SQL injection](https://go.dev/doc/database/sql-injection) — why placeholders, not concatenation.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-case-insensitive-header-lookup.md](08-case-insensitive-header-lookup.md) | Next: [../06-string-formatting-with-fmt/00-concepts.md](../06-string-formatting-with-fmt/00-concepts.md)
