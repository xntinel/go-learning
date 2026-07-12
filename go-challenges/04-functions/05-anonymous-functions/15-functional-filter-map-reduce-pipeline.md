# Exercise 15: A Filter/Map/Reduce Pipeline Built from Anonymous Functions

**Nivel: Intermedio** — validacion rapida (un test corto).

Turning a slice of orders into "total revenue from the ones that shipped" is
a filter, a projection, and a fold — three small, single-purpose steps that
read better composed than as one hand-written loop. This module builds
generic `Filter`, `MapTo`, and `Reduce` helpers and a one-line report
function that passes each of them a small anonymous function describing just
its own step.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
pipeline/                     module example.com/pipeline
  go.mod
  pipeline.go                  Filter, MapTo, Reduce (generic); Order; ShippedRevenue
  pipeline_test.go             each helper in isolation, ShippedRevenue on mixed statuses, empty input
```

- Files: `pipeline.go`, `pipeline_test.go`.
- Implement: `Filter[T](in []T, keep func(T) bool) []T`; `MapTo[T, U](in []T, fn func(T) U) []U`; `Reduce[T, U](in []T, init U, fn func(acc U, cur T) U) U`; `Order` and `ShippedRevenue(orders) float64` composed from the three, each step supplied as an anonymous function literal.
- Test: `Filter` keeps only matching elements in order; `MapTo` transforms every element; `Reduce` folds left to right; `ShippedRevenue` counts only shipped orders and returns 0 for empty input.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/15-functional-filter-map-reduce-pipeline
cd go-solutions/04-functions/05-anonymous-functions/15-functional-filter-map-reduce-pipeline
go mod edit -go=1.24
```

### Three generic helpers, three literals at the call site

`Filter`, `MapTo`, and `Reduce` know nothing about orders — they are generic
over the element type and take the actual logic as a parameter. `ShippedRevenue`
is where the domain knowledge lives, and it lives entirely inside three tiny
anonymous functions passed inline: "keep it if shipped," "project it to its
total," "add it to the running sum." None of those three literals is worth
naming and exporting on its own; writing them inline at the call site is
exactly the case an anonymous function is for. A hand-written loop doing all
three things at once would work too, but it would need a new loop for every
new report instead of recombining the same three verbs.

Create `pipeline.go`:

```go
package pipeline

// Filter returns the elements of in for which keep reports true, preserving
// order. keep is typically a small anonymous predicate supplied at the call
// site.
func Filter[T any](in []T, keep func(T) bool) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if keep(v) {
			out = append(out, v)
		}
	}
	return out
}

// MapTo transforms each element of in with fn, in order.
func MapTo[T, U any](in []T, fn func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = fn(v)
	}
	return out
}

// Reduce folds in into a single accumulator, starting from init and applying
// fn left to right.
func Reduce[T, U any](in []T, init U, fn func(acc U, cur T) U) U {
	acc := init
	for _, v := range in {
		acc = fn(acc, v)
	}
	return acc
}

// Order is a single customer order.
type Order struct {
	ID     string
	Status string
	Total  float64
}

// ShippedRevenue composes Filter, MapTo, and Reduce with three anonymous
// function literals to answer one business question: total revenue from
// orders that have actually shipped. Each literal is small and local to its
// call, which is the point of passing behavior as a function value instead
// of hand-writing a bespoke loop for every report.
func ShippedRevenue(orders []Order) float64 {
	shipped := Filter(orders, func(o Order) bool {
		return o.Status == "shipped"
	})
	totals := MapTo(shipped, func(o Order) float64 {
		return o.Total
	})
	return Reduce(totals, 0.0, func(acc, cur float64) float64 {
		return acc + cur
	})
}
```

### Tests

`TestFilterKeepsMatchingElementsInOrder`, `TestMapToTransformsEachElement`,
and `TestReduceFoldsLeftToRight` exercise the three generic helpers directly
on plain `int`/`string` slices. `TestShippedRevenueOnlyCountsShippedOrders`
mixes shipped, pending, and canceled orders and checks only the shipped
totals are summed. `TestShippedRevenueOnEmptyInput` checks the zero-value
result on `nil`.

Create `pipeline_test.go`:

```go
package pipeline

import (
	"slices"
	"testing"
)

func TestFilterKeepsMatchingElementsInOrder(t *testing.T) {
	t.Parallel()
	in := []int{1, 2, 3, 4, 5, 6}
	got := Filter(in, func(n int) bool { return n%2 == 0 })
	want := []int{2, 4, 6}
	if !slices.Equal(got, want) {
		t.Fatalf("Filter(evens) = %v, want %v", got, want)
	}
}

func TestMapToTransformsEachElement(t *testing.T) {
	t.Parallel()
	in := []int{1, 2, 3}
	got := MapTo(in, func(n int) int { return n * n })
	want := []int{1, 4, 9}
	if !slices.Equal(got, want) {
		t.Fatalf("MapTo(square) = %v, want %v", got, want)
	}
}

func TestReduceFoldsLeftToRight(t *testing.T) {
	t.Parallel()
	in := []string{"a", "b", "c"}
	got := Reduce(in, "", func(acc, cur string) string { return acc + cur })
	if got != "abc" {
		t.Fatalf("Reduce(concat) = %q, want %q", got, "abc")
	}
}

func TestShippedRevenueOnlyCountsShippedOrders(t *testing.T) {
	t.Parallel()
	orders := []Order{
		{ID: "o1", Status: "shipped", Total: 100},
		{ID: "o2", Status: "pending", Total: 999},
		{ID: "o3", Status: "shipped", Total: 50},
		{ID: "o4", Status: "canceled", Total: 500},
	}
	if got, want := ShippedRevenue(orders), 150.0; got != want {
		t.Fatalf("ShippedRevenue() = %v, want %v", got, want)
	}
}

func TestShippedRevenueOnEmptyInput(t *testing.T) {
	t.Parallel()
	if got := ShippedRevenue(nil); got != 0 {
		t.Fatalf("ShippedRevenue(nil) = %v, want 0", got)
	}
}
```

## Review

`Filter`, `MapTo`, and `Reduce` carry no domain knowledge at all — every
decision that makes `ShippedRevenue` mean what it means lives in the three
anonymous functions passed to them. That separation is what makes the
generic helpers reusable for a completely different report (count canceled
orders, average order value) without touching `pipeline.go` itself: only the
literals at the new call site change.

## Resources

- [Type parameters](https://go.dev/blog/intro-generics)
- [slices package](https://pkg.go.dev/slices)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-strategy-selection-pricing.md](14-strategy-selection-pricing.md) | Next: [16-background-workers-heartbeat.md](16-background-workers-heartbeat.md)
