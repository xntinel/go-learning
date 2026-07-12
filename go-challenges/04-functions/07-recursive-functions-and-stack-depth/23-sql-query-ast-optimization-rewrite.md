# Exercise 23: Rewrite Recursive Query AST for Cost-Based Optimization

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A query planner represents a filter as a tree — `AND`s and `OR`s combining
column comparisons, some of which are really references to a named view
whose own definition is another filter tree. Optimizing that tree before
execution is a recursive rewrite: flatten `AND(AND(a, b), c)` into
`AND(a, b, c)` so the executor sees one flat conjunction instead of a
needlessly nested one, drop a redundant `true` filter, and inline every view
reference by substituting the view's own (recursively optimized) definition
in its place. That last step is where a real hazard lives: a view whose
definition transitively refers back to itself is a modeling bug that must
be caught, not one that quietly recurses forever while a query planner hangs.

This module is fully self-contained: its own `go mod init`, the optimizer
inline, its own demo and tests.

## What you'll build

```text
queryopt/                     independent module: example.com/queryopt
  go.mod                        go 1.24
  queryopt.go                    type Expr; func Optimize
  queryopt_test.go                flatten, drop true, all-true collapse, view inline, diamond, cycle, missing view
  cmd/
    demo/
      main.go                     optimizes a filter that mixes AND/OR, a view reference, and a redundant true
```

- Files: `queryopt.go`, `cmd/demo/main.go`, `queryopt_test.go`.
- Implement: `type Expr struct { Op, Value string; Children []*Expr }` and
  `func Optimize(views map[string]*Expr, root *Expr) (*Expr, error)`,
  recursively flattening same-operator `AND`/`OR` nodes, dropping redundant
  `true` operands under `AND`, and inlining `"view"` nodes while tracking a
  `visiting` set to catch circular view definitions.
- Test: nested same-operator nodes flatten; a redundant `true` is dropped;
  an all-`true` conjunction collapses to `true`; a view reference inlines
  correctly; two branches referencing the *same* view succeed (a diamond,
  not a cycle); a genuinely circular view chain returns `ErrCircularView`;
  a missing view returns `ErrViewNotFound`.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/23-sql-query-ast-optimization-rewrite/cmd/demo
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/23-sql-query-ast-optimization-rewrite
go mod edit -go=1.24
```

### A diamond of view references must succeed; a cycle must not

Inlining a `"view"` node means recursively optimizing the view's own
definition and substituting the result. If two different branches of the
same query both reference the same view — `AND(view("base"), view("base"))`
— that is completely legitimate: the view gets resolved twice, once per
reference, and both resolutions succeed independently. It is not a cycle,
because neither reference is *still in progress* while the other runs; they
just happen to name the same thing.

The `visiting` map exists to tell that apart from an actual cycle
(`a -> view b -> view a`), the same way three-color DFS elsewhere in this
lesson tells a diamond apart from a back edge. Before resolving a view, the
optimizer marks that view's name in `visiting`; if resolving its definition
recursively reaches a `"view"` node naming something already in `visiting`,
that reference is *still being resolved* by an ancestor call — a genuine
cycle, reported as `ErrCircularView`. Critically, the mark is removed with
`delete(visiting, name)` as soon as that view's resolution finishes, before
control returns to the caller. That removal is what makes the second,
independent reference to `"base"` in the diamond case succeed: by the time
the second `view("base")` node is reached, the first one's resolution has
already completed and unmarked itself, so `visiting["base"]` is false again.
Skip the `delete` and every view could only ever be referenced once in an
entire query — a false positive that would reject perfectly ordinary,
non-circular queries.

Create `queryopt.go`:

```go
// Package queryopt rewrites a recursive query expression tree into an
// optimized form: nested AND/OR nodes of the same operator are flattened
// (associativity), redundant "true" filters are dropped, and references to
// named views are recursively inlined — while detecting circular view
// definitions, which would otherwise send the inliner into infinite
// recursion.
package queryopt

import (
	"errors"
	"fmt"
)

// ErrCircularView is returned when a view's definition refers back to
// itself, directly or through other views.
var ErrCircularView = errors.New("queryopt: circular view reference")

// ErrViewNotFound is returned when a "view" node names a view that is not in
// the provided view table.
var ErrViewNotFound = errors.New("queryopt: view not found")

// Expr is one node of a query filter expression tree.
//
//	Op == "true"        a literal always-true predicate, no children
//	Op == "eq"           a column=literal predicate; Value holds "col=literal"
//	Op == "and", "or"    a boolean combination of Children
//	Op == "view"         a reference to a named view's filter; Value is the view name
type Expr struct {
	Op       string
	Value    string
	Children []*Expr
}

// Optimize resolves every "view" node in root by recursively inlining the
// referenced view's expression from views, then flattens nested AND/OR
// nodes of the same operator and drops redundant "true" operands under AND.
// It returns ErrCircularView if a view (directly or transitively) refers to
// itself, and ErrViewNotFound if a referenced view is missing.
func Optimize(views map[string]*Expr, root *Expr) (*Expr, error) {
	return optimize(views, root, map[string]bool{})
}

func optimize(views map[string]*Expr, e *Expr, visiting map[string]bool) (*Expr, error) {
	switch e.Op {
	case "true", "eq":
		return e, nil

	case "view":
		if visiting[e.Value] {
			return nil, fmt.Errorf("%s: %w", e.Value, ErrCircularView)
		}
		def, ok := views[e.Value]
		if !ok {
			return nil, fmt.Errorf("%s: %w", e.Value, ErrViewNotFound)
		}
		// Mark this view "on the current inlining path," resolve its
		// definition, then unmark it. Unmarking (rather than leaving it
		// marked forever) is what allows a diamond — two different
		// branches both referencing the same view — to inline
		// successfully instead of being mistaken for a cycle: only a view
		// that refers to itself while its own resolution is still in
		// progress is a genuine cycle.
		visiting[e.Value] = true
		resolved, err := optimize(views, def, visiting)
		delete(visiting, e.Value)
		if err != nil {
			return nil, err
		}
		return resolved, nil

	case "and", "or":
		var flat []*Expr
		for _, child := range e.Children {
			rc, err := optimize(views, child, visiting)
			if err != nil {
				return nil, err
			}
			if e.Op == "and" && rc.Op == "true" {
				continue // "true" is a no-op operand under AND
			}
			if rc.Op == e.Op {
				flat = append(flat, rc.Children...) // associativity: absorb same-operator children
			} else {
				flat = append(flat, rc)
			}
		}
		switch len(flat) {
		case 0:
			return &Expr{Op: "true"}, nil
		case 1:
			return flat[0], nil
		default:
			return &Expr{Op: e.Op, Children: flat}, nil
		}

	default:
		return nil, fmt.Errorf("queryopt: unknown operator %q", e.Op)
	}
}
```

### The runnable demo

The demo optimizes `AND(status=active, OR(view(recent_orders), region=us), true)`,
where `recent_orders` is a view defined as
`AND(created>2024, created<2025)`. Optimization inlines the view and drops
the redundant `true`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/queryopt"
)

func render(e *queryopt.Expr) string {
	switch e.Op {
	case "true":
		return "true"
	case "eq":
		return "eq(" + e.Value + ")"
	case "view":
		return "view(" + e.Value + ")"
	default:
		parts := make([]string, len(e.Children))
		for i, c := range e.Children {
			parts[i] = render(c)
		}
		return e.Op + "(" + strings.Join(parts, " ") + ")"
	}
}

func main() {
	views := map[string]*queryopt.Expr{
		"recent_orders": {
			Op: "and",
			Children: []*queryopt.Expr{
				{Op: "eq", Value: "created>2024"},
				{Op: "eq", Value: "created<2025"},
			},
		},
	}

	root := &queryopt.Expr{
		Op: "and",
		Children: []*queryopt.Expr{
			{Op: "eq", Value: "status=active"},
			{
				Op: "or",
				Children: []*queryopt.Expr{
					{Op: "view", Value: "recent_orders"},
					{Op: "eq", Value: "region=us"},
				},
			},
			{Op: "true"},
		},
	}

	optimized, err := queryopt.Optimize(views, root)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Println(render(optimized))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
and(eq(status=active) or(and(eq(created>2024) eq(created<2025)) eq(region=us)))
```

### Tests

`TestOptimizeFlattensNestedSameOperator` and `TestOptimizeDropsRedundantTrueUnderAnd`
check the two basic rewrites. `TestOptimizeAllTrueCollapsesToTrue` is the
edge of the drop-true rule — every operand disappearing must still leave a
valid expression, not an empty one. `TestOptimizeInlinesViewReference`
checks substitution. `TestOptimizeDiamondViewReferenceIsNotACycle` is the
test that would fail if `visiting` were never unmarked: two independent
references to the same view must both succeed.
`TestOptimizeDetectsCircularViewReference` checks the actual failure case,
and `TestOptimizeMissingViewReturnsError` covers a dangling reference.

Create `queryopt_test.go`:

```go
package queryopt

import (
	"errors"
	"reflect"
	"testing"
)

func TestOptimizeFlattensNestedSameOperator(t *testing.T) {
	t.Parallel()

	root := &Expr{
		Op: "and",
		Children: []*Expr{
			{Op: "and", Children: []*Expr{
				{Op: "eq", Value: "a=1"},
				{Op: "eq", Value: "b=2"},
			}},
			{Op: "eq", Value: "c=3"},
		},
	}

	got, err := Optimize(nil, root)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	want := &Expr{Op: "and", Children: []*Expr{
		{Op: "eq", Value: "a=1"},
		{Op: "eq", Value: "b=2"},
		{Op: "eq", Value: "c=3"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Optimize() = %#v, want %#v", got, want)
	}
}

func TestOptimizeDropsRedundantTrueUnderAnd(t *testing.T) {
	t.Parallel()

	root := &Expr{Op: "and", Children: []*Expr{
		{Op: "eq", Value: "a=1"},
		{Op: "true"},
	}}

	got, err := Optimize(nil, root)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	want := &Expr{Op: "eq", Value: "a=1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Optimize() = %#v, want %#v", got, want)
	}
}

func TestOptimizeAllTrueCollapsesToTrue(t *testing.T) {
	t.Parallel()

	root := &Expr{Op: "and", Children: []*Expr{{Op: "true"}, {Op: "true"}}}
	got, err := Optimize(nil, root)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	if got.Op != "true" {
		t.Fatalf("Optimize() = %#v, want true", got)
	}
}

func TestOptimizeInlinesViewReference(t *testing.T) {
	t.Parallel()

	views := map[string]*Expr{
		"active_users": {Op: "eq", Value: "status=active"},
	}
	root := &Expr{Op: "view", Value: "active_users"}

	got, err := Optimize(views, root)
	if err != nil {
		t.Fatalf("Optimize() error = %v", err)
	}
	want := &Expr{Op: "eq", Value: "status=active"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Optimize() = %#v, want %#v", got, want)
	}
}

func TestOptimizeDiamondViewReferenceIsNotACycle(t *testing.T) {
	t.Parallel()

	// Two branches both reference the same view. This must succeed: it is
	// a diamond, not a cycle, because neither reference is still "in
	// progress" while the other resolves.
	views := map[string]*Expr{
		"base": {Op: "eq", Value: "tenant=acme"},
	}
	root := &Expr{Op: "and", Children: []*Expr{
		{Op: "view", Value: "base"},
		{Op: "view", Value: "base"},
	}}

	got, err := Optimize(views, root)
	if err != nil {
		t.Fatalf("Optimize() error = %v, want nil (diamond, not a cycle)", err)
	}
	want := &Expr{Op: "and", Children: []*Expr{
		{Op: "eq", Value: "tenant=acme"},
		{Op: "eq", Value: "tenant=acme"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Optimize() = %#v, want %#v", got, want)
	}
}

func TestOptimizeDetectsCircularViewReference(t *testing.T) {
	t.Parallel()

	views := map[string]*Expr{
		"a": {Op: "view", Value: "b"},
		"b": {Op: "view", Value: "a"},
	}
	root := &Expr{Op: "view", Value: "a"}

	_, err := Optimize(views, root)
	if !errors.Is(err, ErrCircularView) {
		t.Fatalf("Optimize() error = %v, want %v", err, ErrCircularView)
	}
}

func TestOptimizeMissingViewReturnsError(t *testing.T) {
	t.Parallel()

	root := &Expr{Op: "view", Value: "does-not-exist"}
	_, err := Optimize(map[string]*Expr{}, root)
	if !errors.Is(err, ErrViewNotFound) {
		t.Fatalf("Optimize() error = %v, want %v", err, ErrViewNotFound)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`Optimize` is correct when the rewritten tree is both flatter (no
same-operator nesting, no redundant `true`) and semantically identical to
the original, and when it distinguishes a legitimate repeated view
reference from a real cycle. The mistake this exercise targets is treating
`visiting` as a permanent "already resolved" set instead of a scoped "on
the current path" set — forgetting the `delete` call after a view resolves.
That version would still catch every real cycle, so it is easy to ship
without noticing anything is wrong, right up until a query with the same
view referenced from two different branches (an entirely ordinary shape,
not a modeling mistake) gets rejected as if it were circular.
`TestOptimizeDiamondViewReferenceIsNotACycle` is the test written
specifically to catch that regression.

## Resources

- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types)
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual)
- [PostgreSQL: Rule system and view rewriting (conceptual background)](https://www.postgresql.org/docs/current/rules-views.html)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-raft-log-prefix-matching-memoized.md](22-raft-log-prefix-matching-memoized.md) | Next: [24-semver-dependency-resolution-backtracking.md](24-semver-dependency-resolution-backtracking.md)
