# Exercise 4: A Composable, Injection-Safe Report Query Builder

A backend reporting service rarely runs one fixed query. It runs a family of them: the same table, filtered, sorted, and paginated differently for each report a caller asks for. That is exactly a builder's job — accumulate composable filters, sort keys, and a page window, then emit a finished query. The hard requirement is safety: every value a caller supplies must travel as a bound parameter, never as text spliced into SQL, or the service ships an injection hole. This exercise builds a `report.Builder` that produces an immutable `QueryPlan` — a parameterized SQL string plus its ordered argument list — and rejects anything that cannot be made safe.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
query.go             Op, Dir, QueryPlan, Builder, New, Select, Where, OrderBy, Limit, Offset, Build
cmd/
  demo/
    main.go          compose a paginated report, then print two error cases
query_test.go        composition, the parameter/identifier split, every validator, reuse
```

- Files: `query.go`, `cmd/demo/main.go`, `query_test.go`.
- Implement: `Builder` with composable setters (`Select`, `Where`, `OrderBy`, `Limit`, `Offset`) and `Build() (QueryPlan, error)`, over the `Op` and `Dir` enums and the immutable `QueryPlan` product.
- Test: `query_test.go` pins the composed SQL and its arguments, proves caller values land in `Args` and never in the text, exercises every validator via its sentinel, confirms `errors.Join` aggregation, and confirms a built plan is independent of later setter calls.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/02-builder-pattern/04-query-report-builder/cmd/demo && cd go-solutions/24-design-patterns-in-go/02-builder-pattern/04-query-report-builder
```

### The one rule: values are parameters, identifiers are allow-listed

SQL injection has exactly one cause — a caller-controlled string becomes part of the query *text* instead of a query *value*. The defense is equally precise, and it splits along a line the SQL grammar itself draws. A *value* (the `42` in `total = 42`, the `'paid'` in `status = 'paid'`) can always be replaced by a placeholder — `total = $1` — and sent to the driver out-of-band, where it can never be parsed as SQL. An *identifier* (a table or column name) cannot: no database lets you bind a column name to a placeholder, because the name has to be known before the query is planned. So identifiers must be made safe a different way — by checking them against an allow-list of names the service already trusts, and only then placing them into the text verbatim.

This builder encodes that split as its entire design. `New(table, allowedColumns...)` takes the set of columns the caller is permitted to name; every column that later appears in `Select`, `Where`, or `OrderBy` is checked against that set, and an unknown column is a recorded error, not a string that reaches the SQL. The table name and each allowed column are additionally screened by `safeIdent`, a conservative `[A-Za-z_][A-Za-z0-9_]*` check, so even the trusted set cannot contain something like `total; DROP TABLE orders`. Values, by contrast, are never inspected for content at all: a caller may filter on the literal string `Robert'); DROP TABLE users;--` and the builder will happily bind it as `$1`, because as a bound argument it is inert. The test `TestBuild_ValuesNeverAppearInText` pins precisely that — the malicious payload appears in `Args`, never in `SQL`.

The operator is the third channel an attacker might reach for, so it is closed the same way as identifiers: `Op` is a named string type whose only legal values are the package constants, and `Where` rejects any `Op` not in `knownOps`. A caller cannot pass `Op("OR 1=1")` and have it concatenated; it is an `ErrUnknownOp`. Between allow-listed identifiers, a closed operator set, and parameterized values, there is no path left by which caller input becomes executable SQL.

### Why setters record and Build decides

Like the fluent builder earlier in this lesson, the setters here never fail fast. Each one validates its argument, and on a problem appends a sentinel-wrapped error to an internal slice and returns the same `*Builder` so calls keep chaining. Deferring judgment lets `Build` report every problem in one `errors.Join` rather than forcing the caller through a fix-one-rebuild loop. `Build` copies the accumulated errors into a *fresh local slice* before deciding, so a failed build never sticks to the builder — a detail the reuse test guards, exactly as in Exercise 1.

`Build` itself is pure assembly once the inputs are known to be valid. With no `Select`, it projects every allowed column in sorted order, so the SQL is deterministic and a test can pin it. Filters are AND-combined in the order they were added; an `IN` filter expands to one placeholder per value (`status IN ($1, $2)`), while every other operator takes a single placeholder. Sort keys render left to right. `LIMIT` and `OFFSET` are the one place a value is inlined rather than bound — but they are validated non-negative Go `int`s, not caller strings, so there is nothing to inject; an `int` has no SQL syntax. The result is a `QueryPlan` whose `SQL` and `Args` are returned by value, independent of the builder, so a later setter call cannot mutate a plan already handed out.

Create `query.go`:

```go
package report

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrNoTable        = errors.New("table is required")
	ErrUnknownColumn  = errors.New("column is not in the allowed set")
	ErrBadIdentifier  = errors.New("identifier is not a safe SQL name")
	ErrUnknownOp      = errors.New("unknown comparison operator")
	ErrArity          = errors.New("wrong number of values for operator")
	ErrEmptyIn        = errors.New("IN requires at least one value")
	ErrNegativeLimit  = errors.New("limit must not be negative")
	ErrNegativeOffset = errors.New("offset must not be negative")
)

// Op is a comparison operator. Only the constants below are accepted; any other
// value is rejected by Build, so an operator can never be smuggled into the SQL.
type Op string

const (
	Eq   Op = "="
	Ne   Op = "<>"
	Lt   Op = "<"
	Lte  Op = "<="
	Gt   Op = ">"
	Gte  Op = ">="
	In   Op = "IN"
	Like Op = "LIKE"
)

var knownOps = map[Op]bool{
	Eq: true, Ne: true, Lt: true, Lte: true, Gt: true, Gte: true, In: true, Like: true,
}

// Dir is a sort direction.
type Dir string

const (
	Asc  Dir = "ASC"
	Desc Dir = "DESC"
)

type filter struct {
	column string
	op     Op
	values []any
}

type sortKey struct {
	column string
	dir    Dir
}

// QueryPlan is the immutable product: a parameterized SQL string and the
// ordered argument list that fills its placeholders. The text never contains a
// caller-supplied value, only $N placeholders, so the plan is injection-safe by
// construction.
type QueryPlan struct {
	SQL  string
	Args []any
}

// Builder accumulates a SELECT over one table. Setters record problems instead
// of failing fast; Build runs the cross-field checks and reports everything
// together. It is mutable and not safe for concurrent use.
type Builder struct {
	table   string
	allowed map[string]bool
	columns []string
	filters []filter
	sorts   []sortKey
	limit   int
	offset  int
	hasLim  bool
	hasOff  bool
	errs    []error
}

// safeIdent matches a conservative SQL identifier: a letter or underscore
// followed by letters, digits, or underscores. It is the gate that lets a
// column name be placed into the SQL text verbatim, because a placeholder
// cannot stand in for an identifier.
func safeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// New starts a plan over table, with an allow-list of the columns that may be
// referenced. Any column named in Select, Where, or OrderBy must appear here;
// that allow-list is what makes identifiers safe to interpolate.
func New(table string, allowedColumns ...string) *Builder {
	b := &Builder{
		table:   table,
		allowed: make(map[string]bool, len(allowedColumns)),
		limit:   0,
		offset:  0,
	}
	if table == "" {
		b.errs = append(b.errs, ErrNoTable)
	} else if !safeIdent(table) {
		b.errs = append(b.errs, fmt.Errorf("%w: %q", ErrBadIdentifier, table))
	}
	for _, c := range allowedColumns {
		if !safeIdent(c) {
			b.errs = append(b.errs, fmt.Errorf("%w: %q", ErrBadIdentifier, c))
			continue
		}
		b.allowed[c] = true
	}
	return b
}

func (b *Builder) check(column string) bool {
	if !b.allowed[column] {
		b.errs = append(b.errs, fmt.Errorf("%w: %q", ErrUnknownColumn, column))
		return false
	}
	return true
}

// Select restricts the projection to the named columns. With no Select the plan
// projects every allowed column, sorted, so the SQL is deterministic.
func (b *Builder) Select(columns ...string) *Builder {
	for _, c := range columns {
		if b.check(c) {
			b.columns = append(b.columns, c)
		}
	}
	return b
}

// Where adds one filter, AND-combined with the others. The values become
// placeholders, never text.
func (b *Builder) Where(column string, op Op, values ...any) *Builder {
	if !knownOps[op] {
		b.errs = append(b.errs, fmt.Errorf("%w: %q", ErrUnknownOp, op))
		return b
	}
	if !b.check(column) {
		return b
	}
	switch op {
	case In:
		if len(values) == 0 {
			b.errs = append(b.errs, fmt.Errorf("%w: %s", ErrEmptyIn, column))
			return b
		}
	default:
		if len(values) != 1 {
			b.errs = append(b.errs, fmt.Errorf("%w: %s %s wants 1 value, got %d", ErrArity, column, op, len(values)))
			return b
		}
	}
	b.filters = append(b.filters, filter{column: column, op: op, values: values})
	return b
}

// OrderBy appends a sort key. Multiple calls compose left to right.
func (b *Builder) OrderBy(column string, dir Dir) *Builder {
	if dir != Asc && dir != Desc {
		dir = Asc
	}
	if b.check(column) {
		b.sorts = append(b.sorts, sortKey{column: column, dir: dir})
	}
	return b
}

// Limit caps the row count. A negative limit is recorded as an error.
func (b *Builder) Limit(n int) *Builder {
	if n < 0 {
		b.errs = append(b.errs, fmt.Errorf("%w: %d", ErrNegativeLimit, n))
		return b
	}
	b.limit, b.hasLim = n, true
	return b
}

// Offset skips rows. A negative offset is recorded as an error.
func (b *Builder) Offset(n int) *Builder {
	if n < 0 {
		b.errs = append(b.errs, fmt.Errorf("%w: %d", ErrNegativeOffset, n))
		return b
	}
	b.offset, b.hasOff = n, true
	return b
}

// Build validates the accumulated query and renders the plan. The setter errors
// are copied into a fresh local slice so a failed Build never sticks to the
// builder and poisons a later, valid Build.
func (b *Builder) Build() (QueryPlan, error) {
	errs := append([]error(nil), b.errs...)
	if len(errs) > 0 {
		return QueryPlan{}, fmt.Errorf("report: %w", errors.Join(errs...))
	}

	cols := b.columns
	if len(cols) == 0 {
		cols = make([]string, 0, len(b.allowed))
		for c := range b.allowed {
			cols = append(cols, c)
		}
		sort.Strings(cols)
	}

	var sb strings.Builder
	args := make([]any, 0, len(b.filters))
	n := 0
	placeholder := func() string {
		n++
		return "$" + strconv.Itoa(n)
	}

	sb.WriteString("SELECT ")
	sb.WriteString(strings.Join(cols, ", "))
	sb.WriteString(" FROM ")
	sb.WriteString(b.table)

	if len(b.filters) > 0 {
		sb.WriteString(" WHERE ")
		for i, f := range b.filters {
			if i > 0 {
				sb.WriteString(" AND ")
			}
			switch f.op {
			case In:
				ph := make([]string, len(f.values))
				for j, v := range f.values {
					ph[j] = placeholder()
					args = append(args, v)
				}
				sb.WriteString(f.column)
				sb.WriteString(" IN (")
				sb.WriteString(strings.Join(ph, ", "))
				sb.WriteString(")")
			default:
				sb.WriteString(f.column)
				sb.WriteString(" ")
				sb.WriteString(string(f.op))
				sb.WriteString(" ")
				sb.WriteString(placeholder())
				args = append(args, f.values[0])
			}
		}
	}

	if len(b.sorts) > 0 {
		sb.WriteString(" ORDER BY ")
		for i, s := range b.sorts {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(s.column)
			sb.WriteString(" ")
			sb.WriteString(string(s.dir))
		}
	}

	// Limit and offset are validated non-negative Go ints, never caller text,
	// so they are safe to inline; only opaque values ever become placeholders.
	if b.hasLim {
		sb.WriteString(" LIMIT ")
		sb.WriteString(strconv.Itoa(b.limit))
	}
	if b.hasOff {
		sb.WriteString(" OFFSET ")
		sb.WriteString(strconv.Itoa(b.offset))
	}

	return QueryPlan{SQL: sb.String(), Args: args}, nil
}
```

The placeholder counter is a closure so the numbering stays correct no matter how `IN` expansion interleaves with single-value filters: each call to `placeholder()` increments `n` once and emits `$n`, so a query with an `IN` of two values followed by an `Eq` produces `$1, $2` then `$3`, and `Args` lines up index-for-index.

### The runnable demo

The demo composes one realistic report — projection, three AND-ed filters (one of them an `IN`), two sort keys, and a page window — and prints its SQL and arguments. Then it shows two failures: an injection attempt that is blocked because the column is not on the allow-list, and a chain whose three independent faults aggregate into one error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/report-builder"
)

func main() {
	// A composed reporting query: projection, three AND-ed filters, two sort
	// keys, and a page window. Every caller value lands in Args, never the text.
	plan, err := report.New("orders", "id", "customer_id", "status", "total", "created_at").
		Select("id", "customer_id", "total").
		Where("status", report.In, "paid", "shipped").
		Where("total", report.Gte, 100).
		Where("customer_id", report.Eq, 42).
		OrderBy("created_at", report.Desc).
		OrderBy("id", report.Asc).
		Limit(20).
		Offset(40).
		Build()
	if err != nil {
		log.Fatalf("build: %v", err)
	}
	fmt.Println(plan.SQL)
	fmt.Printf("args=%v\n", plan.Args)

	fmt.Println("--- error cases ---")

	// An unknown column is the injection attempt: it is not on the allow-list.
	if _, err := report.New("orders", "id", "status").
		Where("total; DROP TABLE orders", report.Eq, 1).
		Build(); err != nil {
		fmt.Printf("injection blocked: %v\n", err)
	}

	// Several independent faults aggregate into one error.
	if _, err := report.New("orders", "id").
		Where("id", report.In).
		OrderBy("missing", report.Asc).
		Limit(-5).
		Build(); err != nil {
		fmt.Printf("aggregated: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
SELECT id, customer_id, total FROM orders WHERE status IN ($1, $2) AND total >= $3 AND customer_id = $4 ORDER BY created_at DESC, id ASC LIMIT 20 OFFSET 40
args=[paid shipped 100 42]
--- error cases ---
injection blocked: report: column is not in the allowed set: "total; DROP TABLE orders"
aggregated: report: IN requires at least one value: id
column is not in the allowed set: "missing"
limit must not be negative: -5
```

The composed query reads as written: four placeholders for the four bound values, two sort keys in order, and an inlined page window. The aggregated case prints three lines because `errors.Join` separates its causes with newlines — the empty `IN`, the unknown sort column, and the negative limit were each recorded by a different setter, then reported together.

### Tests

The suite pins every property the builder claims. `TestBuild_ComposesFullQuery` fixes the exact SQL and the exact `Args` for a composed query, so a regression in placeholder numbering or clause order fails loudly. `TestBuild_DefaultProjectionIsSortedAllowedColumns` pins the no-`Select` default. `TestBuild_ValuesNeverAppearInText` is the security test: a SQL-shaped payload must end up in `Args`, never in `SQL`. One test per validator asserts the matching sentinel via `errors.Is`, including the two injection vectors — an unknown column and an unknown operator. The aggregation test confirms three faults stay individually reachable. The reuse test guards the local-slice copy, and the independence test confirms a built plan does not change when the builder is mutated afterward.

Create `query_test.go`:

```go
package report

import (
	"errors"
	"testing"
)

func TestBuild_ComposesFullQuery(t *testing.T) {
	t.Parallel()

	plan, err := New("orders", "id", "customer_id", "status", "total", "created_at").
		Select("id", "total").
		Where("status", In, "paid", "shipped").
		Where("total", Gte, 100).
		OrderBy("created_at", Desc).
		Limit(20).
		Offset(40).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "SELECT id, total FROM orders WHERE status IN ($1, $2) AND total >= $3 ORDER BY created_at DESC LIMIT 20 OFFSET 40"
	if plan.SQL != want {
		t.Errorf("SQL =\n  %q\nwant\n  %q", plan.SQL, want)
	}
	if len(plan.Args) != 3 {
		t.Fatalf("len(Args) = %d, want 3", len(plan.Args))
	}
	if plan.Args[0] != "paid" || plan.Args[1] != "shipped" || plan.Args[2] != 100 {
		t.Errorf("Args = %v, want [paid shipped 100]", plan.Args)
	}
}

func TestBuild_DefaultProjectionIsSortedAllowedColumns(t *testing.T) {
	t.Parallel()

	plan, err := New("orders", "total", "id", "status").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "SELECT id, status, total FROM orders"
	if plan.SQL != want {
		t.Errorf("SQL = %q, want %q", plan.SQL, want)
	}
}

func TestBuild_ValuesNeverAppearInText(t *testing.T) {
	t.Parallel()

	plan, err := New("users", "id", "name").
		Where("name", Eq, "Robert'); DROP TABLE users;--").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := "SELECT id, name FROM users WHERE name = $1"
	if plan.SQL != want {
		t.Errorf("SQL = %q, want %q", plan.SQL, want)
	}
	if plan.Args[0] != "Robert'); DROP TABLE users;--" {
		t.Errorf("payload must travel as an argument, got %v", plan.Args[0])
	}
}

func TestBuild_RejectsUnknownColumn(t *testing.T) {
	t.Parallel()

	_, err := New("orders", "id").Where("secret", Eq, 1).Build()
	if !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("err = %v, want ErrUnknownColumn", err)
	}
}

func TestBuild_RejectsUnsafeIdentifier(t *testing.T) {
	t.Parallel()

	_, err := New("orders; DROP TABLE x", "id").Build()
	if !errors.Is(err, ErrBadIdentifier) {
		t.Fatalf("err = %v, want ErrBadIdentifier", err)
	}
}

func TestBuild_RejectsUnknownOperator(t *testing.T) {
	t.Parallel()

	_, err := New("orders", "id").Where("id", Op("OR 1=1"), 1).Build()
	if !errors.Is(err, ErrUnknownOp) {
		t.Fatalf("err = %v, want ErrUnknownOp", err)
	}
}

func TestBuild_RejectsEmptyIn(t *testing.T) {
	t.Parallel()

	_, err := New("orders", "status").Where("status", In).Build()
	if !errors.Is(err, ErrEmptyIn) {
		t.Fatalf("err = %v, want ErrEmptyIn", err)
	}
}

func TestBuild_RejectsWrongArity(t *testing.T) {
	t.Parallel()

	_, err := New("orders", "total").Where("total", Gt, 1, 2).Build()
	if !errors.Is(err, ErrArity) {
		t.Fatalf("err = %v, want ErrArity", err)
	}
}

func TestBuild_RejectsNegativeLimitAndOffset(t *testing.T) {
	t.Parallel()

	if _, err := New("orders", "id").Limit(-1).Build(); !errors.Is(err, ErrNegativeLimit) {
		t.Fatalf("limit err = %v, want ErrNegativeLimit", err)
	}
	if _, err := New("orders", "id").Offset(-1).Build(); !errors.Is(err, ErrNegativeOffset) {
		t.Fatalf("offset err = %v, want ErrNegativeOffset", err)
	}
}

func TestBuild_AggregatesMultipleErrors(t *testing.T) {
	t.Parallel()

	_, err := New("orders", "id").
		Where("id", In).
		OrderBy("missing", Asc).
		Limit(-5).
		Build()
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	for _, want := range []error{ErrEmptyIn, ErrUnknownColumn, ErrNegativeLimit} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v: got %v", want, err)
		}
	}
}

func TestBuild_IsReusableAndDoesNotLeakBuildErrors(t *testing.T) {
	t.Parallel()

	b := New("orders", "id", "status")
	b.Where("nope", Eq, 1) // records ErrUnknownColumn
	if _, err := b.Build(); !errors.Is(err, ErrUnknownColumn) {
		t.Fatalf("first Build: want ErrUnknownColumn, got %v", err)
	}
	// A second builder with valid input must build cleanly; the first builder's
	// errors must not be global state.
	plan, err := New("orders", "id", "status").Where("status", Eq, "paid").Build()
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}
	if plan.SQL != "SELECT id, status FROM orders WHERE status = $1" {
		t.Errorf("SQL = %q", plan.SQL)
	}
}

func TestBuild_FiltersAndPlanAreIndependentOfBuilder(t *testing.T) {
	t.Parallel()

	b := New("orders", "id", "status").Where("status", Eq, "paid")
	plan, err := b.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Mutating the builder after Build must not change an already-built plan.
	b.Where("id", Eq, 7)
	if len(plan.Args) != 1 {
		t.Errorf("built plan changed after later setter: Args = %v", plan.Args)
	}
}
```

## Review

The builder is correct when no caller value ever reaches the SQL text and no unallowed identifier ever does either. Confirm the split holds in both directions: `TestBuild_ValuesNeverAppearInText` proves a SQL-shaped value lands in `Args`, and `TestBuild_RejectsUnknownColumn` plus `TestBuild_RejectsUnknownOperator` prove the two identifier-and-operator channels are closed. The placeholder numbering is the other property a regression most easily breaks; `TestBuild_ComposesFullQuery` pins the exact `$1..$4` sequence against a composed query, so an off-by-one in the closure fails it.

Common mistakes for this builder. The first is inlining a value "just this once" — interpolating a string filter directly because it looked harmless — which is the whole vulnerability; route every value through a placeholder without exception. The second is forgetting that an identifier *cannot* be parameterized and reaching for a placeholder where a column name belongs; identifiers are made safe by the allow-list and `safeIdent`, never by binding. The third is letting a failed `Build` poison the builder by appending build-time errors to `b.errs`; keep them in a fresh local slice so the builder stays reusable, which `TestBuild_IsReusableAndDoesNotLeakBuildErrors` guards. Run `go test -race -count=1 ./...` to confirm the composition, the security split, and the reuse contract all hold; the builder is not safe to share across goroutines.

## Resources

- [OWASP SQL Injection Prevention Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/SQL_Injection_Prevention_Cheat_Sheet.html) — the canonical statement of the split this builder encodes: parameterize values, allow-list identifiers that cannot be parameterized.
- [Go: Avoiding SQL injection risk](https://go.dev/doc/database/sql-injection) — why `database/sql` placeholders are safe and string concatenation is not.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard-library function that combines several errors into one that still answers `errors.Is`.
- [Builder in Go (Refactoring Guru)](https://refactoring.guru/design-patterns/builder/go/example) — the Builder pattern with a worked Go example.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-immutable-builder-and-director.md](03-immutable-builder-and-director.md) | Next: [05-deployment-spec-builder.md](05-deployment-spec-builder.md)
