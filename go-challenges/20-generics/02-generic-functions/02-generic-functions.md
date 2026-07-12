# 2. Generic Functions In A Collection Utility Library

Without generics, the most basic utilities have to be hand-rewritten per type:
`MinInt`, `MinFloat64`, `MinString`. The `cmp.Ordered` constraint from the standard
library exposes the comparison operators `<`, `<=`, `>`, `>=` to a type parameter,
so a single generic `Min[T cmp.Ordered]` covers all of them. A separate
`comparable` constraint covers `==` and `!=` for membership checks.

This lesson builds a `collection` package with `Min`, `Max`, `Contains`, `Index`,
`Filter`, and `Map` — the standard toolbox that every Go program reaches for. The
hard part is choosing the right constraint for each function: too tight and the
function is unusable; too loose and the body will not compile.

```text
collection/
  go.mod
  collection.go
  collection_test.go
  cmd/demo/main.go
```

The package is a library. The verification comes from a real `*_test.go` with
table-driven tests, plus an `Example` function and a `cmd/demo` program.

## Concepts

### `comparable` And `cmp.Ordered` Are The Two Real Constraints To Know

`comparable` is the interface that allows `==` and `!=`. The compiler implements it
implicitly: every type made of comparable fields is comparable, and slices, maps,
and functions are not. Use it for membership tests and map keys.

`cmp.Ordered` is the standard-library interface for all types that support `<`,
`<=`, `>`, `>=`. It is defined as the union
`~int | ~int8 | ... | ~float64 | ~string` (the `~` permits named types with
those underlying types). Use it for `Min`, `Max`, `Clamp`, and any comparison
beyond equality.

### The Body Of A Generic Function Only Sees What The Constraint Promises

Inside `Min[T cmp.Ordered]`, the expressions `a < b` and `a > b` are legal because
the constraint promises ordering. Inside `Contains[T comparable]`, the expression
`v == target` is legal because the constraint promises equality. The compiler
will reject any other operation on `T`, and that is the whole point: the function
body cannot assume something the constraint did not promise.

### A Generic Function Is Compiled Per Instantiation

`Min[int]` and `Min[string]` are two separate compiled functions. There is no
boxing of `T` into an `interface{}`, no reflection, no runtime type assertion. The
cost is binary size (one copy per type used); the benefit is type safety plus
performance comparable to hand-rolled type-specific code.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/20-generics/02-generic-functions/02-generic-functions/cmd/demo
cd go-solutions/20-generics/02-generic-functions/02-generic-functions
```

### Exercise 1: The Min, Max, Contains, And Index Functions

Create `collection.go`:

```go
package collection

import "cmp"

func Min[T cmp.Ordered](a, b T) T {
	if a < b {
		return a
	}
	return b
}

func Max[T cmp.Ordered](a, b T) T {
	if a > b {
		return a
	}
	return b
}

func Contains[T comparable](items []T, target T) bool {
	for _, v := range items {
		if v == target {
			return true
		}
	}
	return false
}

func Index[T comparable](items []T, target T) int {
	for i, v := range items {
		if v == target {
			return i
		}
	}
	return -1
}
```

`Min`/`Max` take `cmp.Ordered` because they need ordering; `Contains`/`Index` take
`comparable` because they only need equality. Slices of slices or slices of maps
are not `comparable`, so `Contains[[]int, ...]` would not compile — that is
correct, not a bug.

### Exercise 2: Filter And Map

Append to `collection.go`:

```go
func Filter[T any](items []T, pred func(T) bool) []T {
	out := make([]T, 0, len(items))
	for _, v := range items {
		if pred(v) {
			out = append(out, v)
		}
	}
	return out
}

func Map[T, U any](items []T, fn func(T) U) []U {
	out := make([]U, len(items))
	for i, v := range items {
		out[i] = fn(v)
	}
	return out
}
```

`Filter` and `Map` use `any` because they only store and pass values. `Map` is
two-parameter: the source type `T` and the destination type `U` are independent,
so `Map([]string{...}, strconv.Atoi)` would not even need explicit arguments
because each is inferred from the inputs.

### Exercise 3: Table-Driven Tests

Create `collection_test.go`:

```go
package collection

import (
	"fmt"
	"strconv"
	"testing"
)

func TestMin(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int
		want int
	}{
		{"a smaller", 1, 2, 1},
		{"b smaller", 5, 2, 2},
		{"equal", 3, 3, 3},
		{"negatives", -10, -1, -10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Min(tt.a, tt.b); got != tt.want {
				t.Errorf("Min(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestMinStrings(t *testing.T) {
	t.Parallel()
	if got := Min("apple", "banana"); got != "apple" {
		t.Errorf(`Min("apple","banana") = %q, want "apple"`, got)
	}
}

func TestMax(t *testing.T) {
	t.Parallel()
	if got := Max(3, 7); got != 7 {
		t.Errorf("Max(3,7) = %d, want 7", got)
	}
	if got := Max(3.14, 2.71); got != 3.14 {
		t.Errorf("Max(3.14, 2.71) = %v, want 3.14", got)
	}
}

func TestContains(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		items  []int
		target int
		want   bool
	}{
		{"present", []int{1, 2, 3, 4, 5}, 3, true},
		{"absent", []int{1, 2, 3, 4, 5}, 9, false},
		{"empty", []int{}, 1, false},
		{"first", []int{7, 8, 9}, 7, true},
		{"last", []int{7, 8, 9}, 9, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Contains(tt.items, tt.target); got != tt.want {
				t.Errorf("Contains(%v, %d) = %v, want %v", tt.items, tt.target, got, tt.want)
			}
		})
	}
}

func TestIndex(t *testing.T) {
	t.Parallel()
	words := []string{"go", "rust", "python"}
	if got := Index(words, "rust"); got != 1 {
		t.Errorf(`Index(words, "rust") = %d, want 1`, got)
	}
	if got := Index(words, "java"); got != -1 {
		t.Errorf(`Index(words, "java") = %d, want -1`, got)
	}
}

func TestFilter(t *testing.T) {
	t.Parallel()
	nums := []int{1, 2, 3, 4, 5, 6}
	evens := Filter(nums, func(n int) bool { return n%2 == 0 })
	want := []int{2, 4, 6}
	if len(evens) != len(want) {
		t.Fatalf("len(evens) = %d, want %d", len(evens), len(want))
	}
	for i := range evens {
		if evens[i] != want[i] {
			t.Errorf("evens[%d] = %d, want %d", i, evens[i], want[i])
		}
	}
}

func TestMap(t *testing.T) {
	t.Parallel()
	nums := []int{1, 2, 3}
	asStr := Map(nums, strconv.Itoa)
	want := []string{"1", "2", "3"}
	if len(asStr) != len(want) {
		t.Fatalf("len(asStr) = %d, want %d", len(asStr), len(want))
	}
	for i := range asStr {
		if asStr[i] != want[i] {
			t.Errorf("asStr[%d] = %q, want %q", i, asStr[i], want[i])
		}
	}
}

func ExampleMin() {
	fmt.Println(Min(3, 7))
	// Output: 3
}

func ExampleFilter() {
	evens := Filter([]int{1, 2, 3, 4, 5}, func(n int) bool { return n%2 == 0 })
	fmt.Println(evens)
	// Output: [2 4]
}
```

The tests are table-driven (`TestMin`, `TestContains`) where multiple cases add
real coverage value, and direct assertions where a single case is enough
(`TestMax`, `TestIndex`). The two `Example` functions are auto-verified by
`go test`.

### Exercise 4: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strconv"

	"example.com/collection"
)

func main() {
	fmt.Println("Min(3, 7):", collection.Min(3, 7))
	fmt.Println("Max(3, 7):", collection.Max(3, 7))
	fmt.Println(`Min("apple","banana"):`, collection.Min("apple", "banana"))
	fmt.Println(`Max("apple","banana"):`, collection.Max("apple", "banana"))

	nums := []int{1, 2, 3, 4, 5}
	fmt.Println("Contains(nums,3):", collection.Contains(nums, 3))
	fmt.Println("Contains(nums,9):", collection.Contains(nums, 9))

	words := []string{"go", "rust", "python"}
	fmt.Println(`Index(words,"rust"):`, collection.Index(words, "rust"))
	fmt.Println(`Index(words,"java"):`, collection.Index(words, "java"))

	evens := collection.Filter(nums, func(n int) bool { return n%2 == 0 })
	fmt.Println("Evens:", evens)

	doubled := collection.Map(nums, func(n int) int { return n * 2 })
	fmt.Println("Doubled:", doubled)

	asStr := collection.Map(nums, strconv.Itoa)
	fmt.Println("As strings:", asStr)
}
```

## Common Mistakes

### Using `any` When You Need `comparable`

Wrong:

```go
func Contains[T any](items []T, target T) bool {
	for _, v := range items {
		if v == target { // compile error
```

What happens: `any` does not promise `==`. The error reads "operator == not
defined on T".

Fix: change the constraint to `comparable`.

### Using `comparable` When You Need `cmp.Ordered`

Wrong:

```go
func Min[T comparable](a, b T) T {
	if a < b { // compile error
```

What happens: `comparable` does not promise `<`.

Fix: change the constraint to `cmp.Ordered`.

### Returning `nil` For A Generic Zero Value

Wrong:

```go
func First[T any](items []T) T {
	return nil // compile error: nil is not a valid T
}
```

What happens: `nil` is not a valid value of an arbitrary `T`. For pointer-like
types it would be; for `int` it would not. The compiler refuses.

Fix: declare the zero value with the type parameter: `var zero T; return zero`.

### Mixing `Min` With A Slice

`Min` takes two values, not a slice. For a slice, use the `slices.Min` function
from the standard library, or write `Min(items[0], items[1])` after a length
check. This lesson covers the two-argument form because the slice form is
covered in lesson 4.

## Verification

From `~/go-exercises/collection`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five commands must succeed. The `// Output:` lines on the Examples will
fail the test if they are out of date.

Add your own test: `TestMaxFloat` covering `Max(2.5, 1.5) == 2.5` and
`Max(-0.1, -0.2) == -0.1`, then a `TestMinEmptyString` for `Min("", "a")`.

## Summary

- `comparable` allows `==` and `!=`; use it for membership and equality checks.
- `cmp.Ordered` allows `<`, `<=`, `>`, `>=`; use it for `Min`, `Max`, `Clamp`.
- `any` allows storage and passing; use it when the function does not operate
  on the value at all.
- The body of a generic function may only use the operations promised by the
  constraint; the compiler enforces this.
- A generic function is compiled per instantiation, so there is no boxing or
  runtime type assertion overhead.
- The zero value of a type parameter is `var zero T`.

## What's Next

[Comparable and Ordered](../03-comparable-and-ordered/03-comparable-and-ordered.md) —
`cmp.Compare` for three-way comparison, `cmp.Or` for fallbacks, and the rule
that structs are `comparable` if and only if all their fields are.

## Resources

- [cmp package](https://pkg.go.dev/cmp)
- [slices package](https://pkg.go.dev/slices) — generic slice utilities in the
  standard library
- [maps package](https://pkg.go.dev/maps) — generic map utilities
- [Go spec: Type constraints](https://go.dev/ref/spec#Type_constraints)
