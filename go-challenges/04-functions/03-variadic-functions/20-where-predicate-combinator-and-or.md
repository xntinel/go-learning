# Exercise 20: WHERE Predicate Combinator with AND/OR Logic

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A filter UI built from several independent fields — status, region, date
range — needs to become one SQL `WHERE` clause, and the fields combine with
both `AND` and `OR`, often nested (`status = ? AND (region = ? OR region =
?)`). Each field is a `Predicate`, and `And`/`Or` are variadic combinators
that accept any number of predicates, including other combinators, and
evaluate every one of them before deciding whether the whole thing failed —
because a form with three invalid fields should report all three at once.

## What you'll build

```text
wherebuild/                 independent module: example.com/wherebuild
  go.mod                    go 1.24
  wherebuild.go             package wherebuild; type Predicate func() (string, []any, error); Eq, And(preds ...Predicate), Or(preds ...Predicate)
  cmd/
    demo/
      main.go               runnable demo: a nested AND/OR filter, then one that fails on two fields
  wherebuild_test.go        table tests: AND, OR, nesting, aggregated failures, zero predicates
```

- Files: `wherebuild.go`, `cmd/demo/main.go`, `wherebuild_test.go`.
- Implement: `type Predicate func() (clause string, args []any, err error)`, `Eq(column, value string) Predicate`, and `And(preds ...Predicate) Predicate` / `Or(preds ...Predicate) Predicate` built on a shared unexported `combine`.
- Test: `And` of two valid predicates joins clauses with `AND` and concatenates args in order; `Or` joins with `OR`; a combinator nested inside another produces correctly parenthesized SQL; a combinator with two invalid predicates reports both failures, not just the first; zero predicates is an error.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/wherebuild/cmd/demo
cd ~/go-exercises/wherebuild
go mod init example.com/wherebuild
go mod edit -go=1.24
```

### `Predicate` as a variadic-friendly function type, and why `combine` never short-circuits

`Predicate` is `func() (string, []any, error)` — a value, not yet evaluated,
that produces its clause lazily. That laziness is what makes `And` and `Or`
composable: `Or(Eq("region", "eu-west"), Eq("region", "us-east"))` is itself
a `Predicate`, so it can be passed straight into `And(...)` as one more
element of its variadic list, and `combine` doesn't need to know or care
whether a given predicate is a leaf (`Eq`) or another combinator — it just
calls it and gets back a clause, args, and a possible error. That is what
lets a nested filter like `status AND (region OR region)` fall directly out
of ordinary function composition, without a separate tree data structure.

`combine`'s failure handling is the deliberate part: it loops over every
predicate and evaluates all of them, appending any error to an `errs` slice,
rather than returning on the first `err != nil`. Only after every predicate
has run does it check whether `errs` is non-empty and, if so, join them with
`errors.Join`. For a filter built from user-submitted form fields, failing
fast on the first bad field means a user fixes one typo, resubmits, and
immediately hits the *next* bad field — a frustrating one-error-at-a-time
loop. Evaluating everything and aggregating means one submission reports
every invalid field at once.

Create `wherebuild.go`:

```go
// wherebuild.go
package wherebuild

import (
	"errors"
	"fmt"
	"strings"
)

// Predicate produces a SQL WHERE fragment and its positional bind arguments
// when evaluated, or an error if the underlying filter value is invalid.
type Predicate func() (clause string, args []any, err error)

// Eq returns a Predicate for "column = ?". An empty value is treated as an
// invalid filter and reported as an error when the predicate is evaluated.
func Eq(column, value string) Predicate {
	return func() (string, []any, error) {
		if value == "" {
			return "", nil, fmt.Errorf("wherebuild: empty value for column %q", column)
		}
		return column + " = ?", []any{value}, nil
	}
}

// And combines any number of predicates into one clause joined by AND and
// wrapped in parentheses.
func And(preds ...Predicate) Predicate {
	return combine(" AND ", preds...)
}

// Or combines any number of predicates into one clause joined by OR and
// wrapped in parentheses.
func Or(preds ...Predicate) Predicate {
	return combine(" OR ", preds...)
}

// combine is the shared evaluator behind And and Or. It evaluates every
// predicate — it does not stop at the first failure — and if one or more
// fail, it joins all of their errors together with errors.Join, so a caller
// building a complex filter from user input sees every invalid field in a
// single pass instead of fixing them one at a time across repeated submits.
func combine(sep string, preds ...Predicate) Predicate {
	return func() (string, []any, error) {
		if len(preds) == 0 {
			return "", nil, fmt.Errorf("wherebuild: no predicates given")
		}

		clauses := make([]string, 0, len(preds))
		var args []any
		var errs []error

		for _, p := range preds {
			clause, a, err := p()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			clauses = append(clauses, clause)
			args = append(args, a...)
		}

		if len(errs) > 0 {
			return "", nil, errors.Join(errs...)
		}
		return "(" + strings.Join(clauses, sep) + ")", args, nil
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/wherebuild"
)

func main() {
	filter := wherebuild.And(
		wherebuild.Eq("status", "active"),
		wherebuild.Or(
			wherebuild.Eq("region", "eu-west"),
			wherebuild.Eq("region", "us-east"),
		),
	)

	clause, args, err := filter()
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(clause)
	fmt.Println(args)

	broken := wherebuild.And(
		wherebuild.Eq("status", ""),
		wherebuild.Eq("region", ""),
	)
	_, _, err = broken()
	fmt.Println("error:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
(status = ? AND (region = ? OR region = ?))
[active eu-west us-east]
error: wherebuild: empty value for column "status"
wherebuild: empty value for column "region"
```

### Tests

`TestAndAggregatesAllFailuresInsteadOfFailingFast` is the one that proves the
no-short-circuit contract: both `status` and `region` are given empty
values, and the test asserts the combined error mentions both column names,
not just the first one encountered.

Create `wherebuild_test.go`:

```go
// wherebuild_test.go
package wherebuild

import (
	"reflect"
	"strings"
	"testing"
)

func TestAndJoinsClausesAndArgs(t *testing.T) {
	t.Parallel()

	p := And(Eq("status", "active"), Eq("region", "eu-west"))
	clause, args, err := p()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "(status = ? AND region = ?)"; clause != want {
		t.Fatalf("clause = %q, want %q", clause, want)
	}
	if want := []any{"active", "eu-west"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestOrJoinsClauses(t *testing.T) {
	t.Parallel()

	p := Or(Eq("region", "eu-west"), Eq("region", "us-east"))
	clause, _, err := p()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "(region = ? OR region = ?)"; clause != want {
		t.Fatalf("clause = %q, want %q", clause, want)
	}
}

func TestNestedAndOr(t *testing.T) {
	t.Parallel()

	p := And(
		Eq("status", "active"),
		Or(Eq("region", "eu-west"), Eq("region", "us-east")),
	)
	clause, args, err := p()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "(status = ? AND (region = ? OR region = ?))"; clause != want {
		t.Fatalf("clause = %q, want %q", clause, want)
	}
	if want := []any{"active", "eu-west", "us-east"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

func TestAndAggregatesAllFailuresInsteadOfFailingFast(t *testing.T) {
	t.Parallel()

	p := And(Eq("status", ""), Eq("region", ""))
	_, _, err := p()
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"status"`) || !strings.Contains(msg, `"region"`) {
		t.Fatalf("error %q does not mention both invalid columns", msg)
	}
}

func TestCombineWithZeroPredicatesIsAnError(t *testing.T) {
	t.Parallel()

	if _, _, err := And()(); err == nil {
		t.Fatalf("And() with zero predicates: error = nil, want an error")
	}
	if _, _, err := Or()(); err == nil {
		t.Fatalf("Or() with zero predicates: error = nil, want an error")
	}
}
```

## Review

`And`/`Or` are correct when the clause is built from every predicate's output
joined by the right operator and wrapped in parentheses, arguments appear in
the same order the predicates were listed, nesting one combinator inside
another produces correctly nested parentheses, and — the property that
separates this from a naive fail-fast validator — a combinator with multiple
invalid predicates surfaces every failure in one `errors.Join`ed error rather
than only the first. The senior point is recognizing that "stop at the first
error" and "collect and report all errors" are two different, both valid,
error-handling strategies, and picking the aggregating one deliberately when
the caller is a human filling out a form who benefits from seeing every
problem at once. The mistake to avoid is an early `return` the moment a
predicate errors inside the loop — that silently reverts the combinator to
fail-fast and defeats the whole point of `errors.Join` at the end.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining multiple independent errors into one.
- [Go Spec: Passing arguments to `...` parameters](https://go.dev/ref/spec#Passing_arguments_to_..._parameters)
- [`database/sql`](https://pkg.go.dev/database/sql) — the driver-facing `query string, args ...any` shape this predicate tree ultimately feeds.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [19-query-param-encoder-pairs.md](19-query-param-encoder-pairs.md) | Next: [21-span-attribute-enricher-otel.md](21-span-attribute-enricher-otel.md)
