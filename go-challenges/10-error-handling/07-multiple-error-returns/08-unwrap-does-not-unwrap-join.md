# Exercise 8: The Trap — errors.Unwrap Does Not Traverse A Join

Reaching for `errors.Unwrap` to enumerate a joined error's members returns nothing,
and the reason bites people in review. This exercise proves the trap
(`errors.Unwrap(errors.Join(a, b)) == nil`) and then builds the traversal you must
write by hand: `Flatten`, which type-asserts `interface{ Unwrap() []error }` and
recurses to collect the leaf errors — the exact thing you need to map each member
to its own API error code.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
flatten/                   independent module: example.com/flatten
  go.mod                   go 1.26
  flatten.go               Flatten(err) []error via interface{ Unwrap() []error }
  cmd/
    demo/
      main.go              shows errors.Unwrap(join) == nil, then Flatten
  flatten_test.go          nested join -> ordered leaves; single; nil; Unwrap trap
```

- Files: `flatten.go`, `cmd/demo/main.go`, `flatten_test.go`.
- Implement: `Flatten(err error) []error` that returns the leaf errors of a (possibly nested) join, recursing through `Unwrap() []error`.
- Test: `errors.Unwrap(errors.Join(a, b))` is nil (the trap); `Flatten(Join(a, Join(b, c)))` is exactly `[a, b, c]` in order; `Flatten` on a single non-aggregate error is `[err]`; `Flatten(nil)` is empty.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/07-multiple-error-returns/08-unwrap-does-not-unwrap-join/cmd/demo
cd go-solutions/10-error-handling/07-multiple-error-returns/08-unwrap-does-not-unwrap-join
```

### Why errors.Unwrap comes up empty

`errors.Unwrap(err)` invokes **only** a method of the form `Unwrap() error`. A
joined error does not have that method — it has `Unwrap() []error`. So
`errors.Unwrap(errors.Join(a, b))` returns nil, and a loop built on repeated
`errors.Unwrap` calls to walk a join sees nothing and exits immediately. This is
easy to write and wrong, because the sibling functions `errors.Is` and `errors.As`
*do* know about both unwrap methods, so people assume `errors.Unwrap` does too. It
does not, by documented design.

When you need the individual members — not a boolean "does the tree contain X" that
`errors.Is` answers, but the actual list, to map each leaf to an HTTP status or a
metrics label — you traverse the tree yourself. `Flatten` does it: if the error
exposes `Unwrap() []error`, recurse into each child and concatenate; otherwise the
error is a leaf, so return it as a one-element slice. Recursion handles nesting: a
`Join(a, Join(b, c))` flattens to `[a, b, c]` because the inner join is itself an
aggregate the recursion descends into. Order is preserved because we append in
slice order at each level.

One deliberate scope choice: `Flatten` treats a `fmt.Errorf("%w")`-wrapped error as
a leaf, because that wrapper exposes `Unwrap() error`, not the slice form. Flatten's
job is to split *aggregates* into their members; peeling single-wrap chains is
`errors.Unwrap`'s job. Keeping the two concerns separate is what makes each simple.

Create `flatten.go`:

```go
package flatten

// Flatten returns the leaf errors of a possibly-nested aggregate. It descends
// through any error exposing Unwrap() []error (as errors.Join does) and returns
// non-aggregate errors as single-element results. Flatten(nil) returns nil.
//
// This is the traversal errors.Unwrap cannot do: errors.Unwrap only calls
// Unwrap() error and returns nil for a join.
func Flatten(err error) []error {
	if err == nil {
		return nil
	}
	if agg, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, child := range agg.Unwrap() {
			out = append(out, Flatten(child)...)
		}
		return out
	}
	return []error{err}
}
```

### The runnable demo

The demo shows the trap directly — `errors.Unwrap` on a join is nil — then flattens
a nested join and prints each leaf, the traversal you actually needed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/flatten"
)

func main() {
	a := errors.New("a")
	b := errors.New("b")
	c := errors.New("c")

	joined := errors.Join(a, errors.Join(b, c))

	fmt.Println("errors.Unwrap(join) == nil:", errors.Unwrap(joined) == nil)

	fmt.Println("flattened leaves:")
	for _, leaf := range flatten.Flatten(joined) {
		fmt.Println(" -", leaf)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
errors.Unwrap(join) == nil: true
flattened leaves:
 - a
 - b
 - c
```

### Tests

`TestUnwrapDoesNotTraverseJoin` pins the trap: `errors.Unwrap(errors.Join(a, b))`
is nil. `TestFlattenNested` asserts a two-level join flattens to its three leaves
in order — the identity comparison works because these are the same sentinel values
that went in (no wrapping). `TestFlattenSingle` asserts a lone error flattens to
itself. `TestFlattenNil` asserts a nil error flattens to an empty slice.

Create `flatten_test.go`:

```go
package flatten

import (
	"errors"
	"testing"
)

func TestUnwrapDoesNotTraverseJoin(t *testing.T) {
	t.Parallel()

	a := errors.New("a")
	b := errors.New("b")
	if got := errors.Unwrap(errors.Join(a, b)); got != nil {
		t.Fatalf("errors.Unwrap(Join) = %v, want nil", got)
	}
}

func TestFlattenNested(t *testing.T) {
	t.Parallel()

	a := errors.New("a")
	b := errors.New("b")
	c := errors.New("c")

	got := Flatten(errors.Join(a, errors.Join(b, c)))
	want := []error{a, b, c}
	if len(got) != len(want) {
		t.Fatalf("Flatten len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Flatten[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestFlattenSingle(t *testing.T) {
	t.Parallel()

	e := errors.New("solo")
	got := Flatten(e)
	if len(got) != 1 || got[0] != e {
		t.Fatalf("Flatten(single) = %v, want [%v]", got, e)
	}
}

func TestFlattenNil(t *testing.T) {
	t.Parallel()

	if got := Flatten(nil); len(got) != 0 {
		t.Fatalf("Flatten(nil) = %v, want empty", got)
	}
}
```

## Review

The trap test is the whole point: `errors.Unwrap` returns nil for a join because it
only calls `Unwrap() error`. `Flatten` is correct when it returns every leaf of a
nested aggregate in input order, returns a single non-aggregate error as a one-
element slice, and returns empty for nil. Use `errors.Is`/`errors.As` when you only
need to *ask* whether the tree contains something; write `Flatten` when you need the
*list* to act on each member — mapping each to a status code, emitting a metric per
failure, or building a structured error array. Run with `-race`.

## Resources

- [errors.Unwrap](https://pkg.go.dev/errors#Unwrap) — "Unwrap only calls a method of the form Unwrap() error ... it does not unwrap errors returned by Join."
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — `Unwrap() []error` and the tree walk that `Is`/`As` (but not `Unwrap`) perform.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-custom-multierror-unwrap-slice.md](07-custom-multierror-unwrap-slice.md) | Next: [09-concurrent-collect-vs-errgroup.md](09-concurrent-collect-vs-errgroup.md)
