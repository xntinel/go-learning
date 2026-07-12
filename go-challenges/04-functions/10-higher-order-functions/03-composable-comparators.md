# Exercise 3: Building Multi-Key Comparators for slices.SortFunc

Result ordering in a repository layer is rarely single-key: "orders by status, then
by amount descending, then by createdAt". You build that as a fold of single-key
comparators, not a nested if-ladder, and hand the composed function straight to
`slices.SortFunc`.

## What you'll build

```text
ordersort/                   independent module: example.com/ordersort
  go.mod                     go 1.25
  sort.go                    type Order; By, Reverse, byStatus/byAmount/byCreatedAt comparators
  sort_test.go               tie-break ordering, reverse, stability, cmp.Or short-circuit
  cmd/demo/
    main.go                  sorts a fixture slice by the composed comparator
```

- Files: `sort.go`, `sort_test.go`, `cmd/demo/main.go`.
- Implement: `By[T any](less ...func(a, b T) int) func(a, b T) int` folding comparators with `cmp.Or`, a `Reverse` wrapper, and three field comparators over `Order`.
- Test: full tie-break ordering equals a hand-written expected slice, descending via `Reverse`, `SortStableFunc` preserves equal-element order, and `cmp.Or` short-circuits on the first non-zero.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/03-composable-comparators/cmd/demo
cd go-solutions/04-functions/10-higher-order-functions/03-composable-comparators
go mod edit -go=1.25
```

### The fold, and why cmp.Or is exactly right

A comparator returns a negative number when `a` sorts before `b`, positive when
after, and zero on a tie. A multi-key sort evaluates the primary comparator; on a
tie it consults the secondary; on another tie the tertiary. That is precisely
`cmp.Or`'s contract: it returns its first non-zero argument. So `By(cmpStatus,
cmpAmountDesc, cmpCreatedAt)` returns a comparator that, for a given `a` and `b`,
computes `cmp.Or(cmpStatus(a,b), cmpAmountDesc(a,b), cmpCreatedAt(a,b))`. The first
comparator that returns non-zero decides the order; the rest are ignored. This
reads top to bottom, and the tie-break precedence is the argument order — no nested
`if/else` to get subtly wrong.

There is a real efficiency subtlety. `cmp.Or(vals...)` is a variadic call: every
argument is evaluated before `cmp.Or` runs, so `By` does *not* short-circuit the
evaluation of later comparators — it short-circuits which *result* wins. For pure
field comparisons that is free. If a comparator were expensive (a computed score, a
DB lookup) you would fold with explicit `if` returns instead so the expensive one
never runs on a decided pair. Know which you have.

Descending is not a special comparator; it is any comparator with its result
negated. `Reverse(less)` returns `func(a, b T) int { return -less(a, b) }`, wrapping
"amount ascending" into "amount descending". One wrapper, reusable over every field.

Create `sort.go`:

```go
package ordersort

import (
	"cmp"
	"time"
)

// Order is a domain record with several sortable fields.
type Order struct {
	ID        string
	Status    string // "pending" < "paid" < "shipped" lexically here
	Amount    int64  // cents
	CreatedAt time.Time
}

// By folds single-key comparators into one tie-breaking comparator: the first
// comparator that returns non-zero decides, matching thenBy semantics.
func By[T any](less ...func(a, b T) int) func(a, b T) int {
	return func(a, b T) int {
		for _, f := range less {
			if r := f(a, b); r != 0 {
				return r
			}
		}
		return 0
	}
}

// Reverse flips a comparator, turning ascending into descending.
func Reverse[T any](less func(a, b T) int) func(a, b T) int {
	return func(a, b T) int { return -less(a, b) }
}

// ByStatus compares orders by Status ascending (lexical).
func ByStatus(a, b Order) int { return cmp.Compare(a.Status, b.Status) }

// ByAmount compares orders by Amount ascending.
func ByAmount(a, b Order) int { return cmp.Compare(a.Amount, b.Amount) }

// ByCreatedAt compares orders by CreatedAt ascending.
func ByCreatedAt(a, b Order) int { return a.CreatedAt.Compare(b.CreatedAt) }
```

The `By` helper folds with an explicit loop rather than `cmp.Or` so it short-circuits
comparator *evaluation*, which is the safer default for a general helper. The test
also demonstrates `cmp.Or` directly so you see the variadic form the concepts
described; both express the same ordering.

### The runnable demo

The demo sorts a small fixture by status ascending, then amount descending, then
createdAt ascending, and prints the result so you can read the tie-breaks. Two
orders share the `paid` status, so the amount-descending key breaks their tie.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"slices"
	"time"

	"example.com/ordersort"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	orders := []ordersort.Order{
		{ID: "o1", Status: "paid", Amount: 500, CreatedAt: base},
		{ID: "o2", Status: "pending", Amount: 900, CreatedAt: base},
		{ID: "o3", Status: "paid", Amount: 1500, CreatedAt: base.Add(time.Hour)},
		{ID: "o4", Status: "paid", Amount: 1500, CreatedAt: base},
	}

	cmpFunc := ordersort.By(
		ordersort.ByStatus,
		ordersort.Reverse(ordersort.ByAmount),
		ordersort.ByCreatedAt,
	)
	slices.SortFunc(orders, cmpFunc)

	for _, o := range orders {
		fmt.Printf("%s status=%s amount=%d created=%s\n",
			o.ID, o.Status, o.Amount, o.CreatedAt.Format("15:04"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
o4 status=paid amount=1500 created=00:00
o3 status=paid amount=1500 created=01:00
o1 status=paid amount=500 created=00:00
o2 status=pending amount=900 created=00:00
```

Trace the tie-breaks. Status is primary, so the three `paid` orders group ahead of
`pending` o2. Within `paid`, amount descending puts the two 1500s (o3, o4) ahead of
the 500 (o1). o3 and o4 tie on both status and amount, so the tertiary createdAt
key decides: o4 (00:00) before o3 (01:00). The final order is o4, o3, o1, o2 — and
the test below asserts exactly that, so the demo and the test share one source of
truth.

### Tests

The fixture is built with a deliberate tie on the primary key (three `paid`
orders) so the secondary comparator is exercised, and a further tie on amount (o3,
o4 both 1500) so the tertiary createdAt comparator runs. The stability test uses
`SortStableFunc` and asserts that two fully-equal elements keep their input order.
The `cmp.Or` test proves the primary decision wins even when the secondary would
flip it.

Create `sort_test.go`:

```go
package ordersort

import (
	"cmp"
	"slices"
	"testing"
	"time"
)

func ids(orders []Order) []string {
	out := make([]string, len(orders))
	for i, o := range orders {
		out[i] = o.ID
	}
	return out
}

func TestByTieBreaks(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	orders := []Order{
		{ID: "o1", Status: "paid", Amount: 500, CreatedAt: base},
		{ID: "o2", Status: "pending", Amount: 900, CreatedAt: base},
		{ID: "o3", Status: "paid", Amount: 1500, CreatedAt: base.Add(time.Hour)},
		{ID: "o4", Status: "paid", Amount: 1500, CreatedAt: base},
	}

	slices.SortFunc(orders, By(
		ByStatus,
		Reverse(ByAmount),
		ByCreatedAt,
	))

	want := []string{"o4", "o3", "o1", "o2"}
	if got := ids(orders); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestReverseDescending(t *testing.T) {
	t.Parallel()

	orders := []Order{
		{ID: "a", Amount: 100},
		{ID: "b", Amount: 300},
		{ID: "c", Amount: 200},
	}
	slices.SortFunc(orders, Reverse(ByAmount))

	want := []string{"b", "c", "a"}
	if got := ids(orders); !slices.Equal(got, want) {
		t.Fatalf("descending order = %v, want %v", got, want)
	}
}

func TestSortStableFuncPreservesEqualOrder(t *testing.T) {
	t.Parallel()

	// All equal on the only sort key (Status), so stable sort must keep input order.
	orders := []Order{
		{ID: "first", Status: "paid"},
		{ID: "second", Status: "paid"},
		{ID: "third", Status: "paid"},
	}
	slices.SortStableFunc(orders, ByStatus)

	want := []string{"first", "second", "third"}
	if got := ids(orders); !slices.Equal(got, want) {
		t.Fatalf("stable order = %v, want %v", got, want)
	}
}

func TestCmpOrShortCircuitsOnPrimary(t *testing.T) {
	t.Parallel()

	a := Order{Status: "paid", Amount: 100}
	b := Order{Status: "pending", Amount: 999999}

	// Primary (status) already decides paid < pending; the secondary amount key
	// would put a AFTER b, but cmp.Or returns the first non-zero result.
	got := cmp.Or(ByStatus(a, b), ByAmount(a, b))
	if got >= 0 {
		t.Fatalf("cmp.Or = %d, want negative (primary status must decide)", got)
	}
}
```

## Review

The composed comparator is correct when the fixture with deliberate ties sorts to
the exact hand-written slice: `o4, o3, o1, o2` for status-asc, amount-desc,
createdAt-asc. The two `paid`+1500 orders (o3, o4) tie on the first two keys, so
the createdAt key decides o4 (00:00) before o3 (01:00); o1 (500) follows because
amount descending puts it last among `paid`; o2 (`pending`) sorts last on status.
`Reverse` is a one-line wrapper that turns any ascending comparator descending.
Use `SortStableFunc` when equal elements must keep their input order — plain
`SortFunc` is not stable and may reorder ties. And remember `cmp.Or` decides by the
first non-zero, which is what lets the primary key override a secondary that would
disagree.

## Resources

- [slices package](https://pkg.go.dev/slices) — `SortFunc`, `SortStableFunc`, `Equal`.
- [cmp package](https://pkg.go.dev/cmp) — `Compare`, `Or`, and their zero/non-zero contract.
- [time.Time.Compare](https://pkg.go.dev/time#Time.Compare) — the correct way to order timestamps.

---

Back to [02-http-middleware-chain.md](02-http-middleware-chain.md) | Next: [04-validation-pipeline.md](04-validation-pipeline.md)
