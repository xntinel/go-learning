# Exercise 8: Accumulate Errors in a Builder and Fail Once at Build

When construction is genuinely multi-step, a fluent builder stages the settings
across chained setters and validates once at `Build()`. This exercise builds a
query builder whose setters record validation problems into an internal slice and
whose `Build()` returns `(Query, error)` via `errors.Join` — never a half-built
object — contrasting the builder's deferred, mutable staging with the immutable,
one-shot functional-options pattern from Exercise 3.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
querybuilder/                independent module: example.com/builder-accumulate-errors
  go.mod
  builder.go                 Query, Builder, Table/Where/Limit setters, Build, sentinels
  cmd/
    demo/
      main.go                a valid chain and a doubly-broken chain
  builder_test.go            valid build, each bad setter, aggregation, zero-on-failure, Build twice idempotent
```

- Files: `builder.go`, `cmd/demo/main.go`, `builder_test.go`.
- Implement: a fluent `Builder` whose setters append to an internal `errs` slice and whose `Build()` returns `errors.Join(errs...)` or the built `Query`.
- Test: a valid chain builds the expected value; each bad setter contributes its sentinel and `Build` aggregates via `errors.Is`; `Build` returns the zero `Query` plus error on any failure; and calling `Build` twice is idempotent (pinned by a test).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/querybuilder/cmd/demo
cd ~/go-exercises/querybuilder
go mod init example.com/builder-accumulate-errors
```

### Builder versus functional options

Both patterns solve "many optional settings", but they make opposite trade-offs.
Functional options are immutable and one-shot: each option is a pure function, the
set is order-independent, and the constructor validates the assembled result once
and hands back a finished value. A builder is mutable staging: chained setters
accumulate state on a `*Builder`, and validation is deferred to `Build()`. The
builder wins when construction is genuinely stepwise or when you want to collect
errors across setters and report them together at the end; options win when the
value is naturally constructed in one call.

The error discipline is the same as the config loader, relocated. Each setter
validates its own argument and, on failure, appends a wrapped sentinel to an
internal `errs` slice rather than returning — because a fluent setter returns
`*Builder` to keep the chain going, it has nowhere to return an error to. `Build()`
is the single collection point: if `errs` is non-empty it returns the zero `Query`
and `errors.Join(errs...)`; otherwise it constructs the `Query`. Returning the
zero value on failure is the non-negotiable part: `Build` must never hand back a
half-populated `Query` alongside an error, because a caller that mishandles the
error would then operate on a partially-valid object.

`Build` is idempotent by construction: it reads the staged state and the `errs`
slice without mutating them, so calling it twice returns equal results. That is a
deliberate contract, and the test pins it — the alternative (a `Build` that
consumes or resets the builder) is also valid but must be documented; silence is
the bug.

Create `builder.go`:

```go
package querybuilder

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrEmptyTable     = errors.New("table name is required")
	ErrEmptyCondition = errors.New("where condition must not be empty")
	ErrInvalidLimit   = errors.New("limit must not be negative")
)

// Query is a validated read query.
type Query struct {
	Table      string
	Conditions []string
	Limit      int
}

// Builder assembles a Query across chained setters, accumulating validation
// problems until Build reports them together.
type Builder struct {
	table      string
	conditions []string
	limit      int
	errs       []error
}

// NewBuilder returns an empty Builder.
func NewBuilder() *Builder {
	return &Builder{}
}

// Table sets the table name.
func (b *Builder) Table(name string) *Builder {
	name = strings.TrimSpace(name)
	if name == "" {
		b.errs = append(b.errs, ErrEmptyTable)
		return b
	}
	b.table = name
	return b
}

// Where adds a condition.
func (b *Builder) Where(cond string) *Builder {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		b.errs = append(b.errs, ErrEmptyCondition)
		return b
	}
	b.conditions = append(b.conditions, cond)
	return b
}

// Limit sets the row limit.
func (b *Builder) Limit(n int) *Builder {
	if n < 0 {
		b.errs = append(b.errs, fmt.Errorf("%w: %d", ErrInvalidLimit, n))
		return b
	}
	b.limit = n
	return b
}

// Build validates the staged state and returns the Query, or the zero Query and
// errors.Join of every problem. It does not mutate the builder, so calling it
// twice yields equal results.
func (b *Builder) Build() (Query, error) {
	var errs []error
	errs = append(errs, b.errs...)
	if b.table == "" && !hasSentinel(errs, ErrEmptyTable) {
		errs = append(errs, ErrEmptyTable)
	}
	if len(errs) > 0 {
		return Query{}, errors.Join(errs...)
	}
	conds := make([]string, len(b.conditions))
	copy(conds, b.conditions)
	return Query{Table: b.table, Conditions: conds, Limit: b.limit}, nil
}

func hasSentinel(errs []error, target error) bool {
	for _, e := range errs {
		if errors.Is(e, target) {
			return true
		}
	}
	return false
}
```

### The runnable demo

The demo builds one valid query and one with two bad setters, printing the
aggregated error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/builder-accumulate-errors"
)

func main() {
	q, err := querybuilder.NewBuilder().
		Table("users").
		Where("active = true").
		Limit(10).
		Build()
	if err != nil {
		fmt.Println("unexpected:", err)
		return
	}
	fmt.Printf("query: table=%s conds=%v limit=%d\n", q.Table, q.Conditions, q.Limit)

	_, err = querybuilder.NewBuilder().
		Table("").
		Limit(-5).
		Build()
	fmt.Println("errors:")
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
query: table=users conds=[active = true] limit=10
errors:
table name is required
limit must not be negative: -5
```

### Tests

`TestBuildTwiceIdempotent` pins the chosen contract: `Build` does not consume the
builder, so two calls return equal results. `TestZeroOnFailure` proves `Build`
never returns a half-built `Query` alongside an error.

Create `builder_test.go`:

```go
package querybuilder

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func TestValidBuild(t *testing.T) {
	t.Parallel()
	q, err := NewBuilder().Table("orders").Where("paid = true").Where("total > 0").Limit(25).Build()
	if err != nil {
		t.Fatal(err)
	}
	if q.Table != "orders" || q.Limit != 25 || !slices.Equal(q.Conditions, []string{"paid = true", "total > 0"}) {
		t.Fatalf("Query = %+v", q)
	}
}

func TestBadSetters(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		build func() (Query, error)
		want  error
	}{
		"empty table":     {func() (Query, error) { return NewBuilder().Table("").Build() }, ErrEmptyTable},
		"empty condition": {func() (Query, error) { return NewBuilder().Table("t").Where("  ").Build() }, ErrEmptyCondition},
		"negative limit":  {func() (Query, error) { return NewBuilder().Table("t").Limit(-1).Build() }, ErrInvalidLimit},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := tc.build()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAggregates(t *testing.T) {
	t.Parallel()
	_, err := NewBuilder().Table("").Where("").Limit(-3).Build()
	for _, want := range []error{ErrEmptyTable, ErrEmptyCondition, ErrInvalidLimit} {
		if !errors.Is(err, want) {
			t.Fatalf("joined err %v should include %v", err, want)
		}
	}
}

func TestZeroOnFailure(t *testing.T) {
	t.Parallel()
	q, err := NewBuilder().Table("t").Limit(-1).Build()
	if err == nil {
		t.Fatal("expected error")
	}
	if q.Table != "" || q.Limit != 0 || q.Conditions != nil {
		t.Fatalf("Build returned a half-built Query on failure: %+v", q)
	}
}

func TestBuildTwiceIdempotent(t *testing.T) {
	t.Parallel()
	b := NewBuilder().Table("t").Where("x = 1").Limit(5)
	q1, err1 := b.Build()
	q2, err2 := b.Build()
	if err1 != nil || err2 != nil {
		t.Fatalf("Build errors: %v, %v", err1, err2)
	}
	if q1.Table != q2.Table || q1.Limit != q2.Limit || !slices.Equal(q1.Conditions, q2.Conditions) {
		t.Fatalf("Build not idempotent: %+v vs %+v", q1, q2)
	}
}

func ExampleBuilder_Build() {
	q, _ := NewBuilder().Table("users").Limit(3).Build()
	fmt.Printf("%s limit %d\n", q.Table, q.Limit)
	// Output: users limit 3
}
```

## Review

The builder is correct when a valid chain builds the expected `Query`, each bad
setter contributes its sentinel and `Build` aggregates them, `Build` returns the
zero `Query` on any failure, and calling `Build` twice is idempotent. The contrast
with functional options is the lesson: the builder stages mutable state and defers
validation to `Build`, where options validate an immutable, order-independent set
once in the constructor. The non-negotiable is never returning a half-built object
alongside an error, and the documented contract for repeated `Build` calls — here,
idempotent — must be pinned by a test rather than left to chance.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — the aggregation `Build` performs.
- [Effective Go: Errors](https://go.dev/doc/effective_go#errors) — the sentinel-and-wrap conventions the setters use.
- [slices.Equal](https://pkg.go.dev/slices#Equal) — comparing the built conditions in tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-must-constructor-init-invariants.md](07-must-constructor-init-invariants.md) | Next: [09-normalize-endpoint-canonical.md](09-normalize-endpoint-canonical.md)
