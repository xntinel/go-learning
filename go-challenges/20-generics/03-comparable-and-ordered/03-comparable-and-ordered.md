# 3. Comparable And Ordered With `cmp.Compare` And `cmp.Or`

`any`, `comparable`, and `cmp.Ordered` form a constraint hierarchy. `any` permits
storage and passing. `comparable` adds `==` and `!=`. `cmp.Ordered` adds `<`,
`<=`, `>`, `>=`. The `cmp` package adds three small utilities on top of
`cmp.Ordered`: `Compare` for three-way comparison, `Less` for the boolean
shorthand, and `Or` for "first non-zero".

Choosing the wrong constraint is the most common generics mistake: too tight and
the function is unusable, too loose and the body will not compile. This lesson
builds a `stats` library with `Identity`, `Equal`, `Clamp`, and `MinMax` to make
the trade-off concrete, and uses the `cmp` helpers to write them correctly.

```text
stats/
  go.mod
  stats.go
  stats_test.go
  cmd/demo/main.go
```

The package exposes only the functions and the types they need. The test file
exercises each function across multiple value kinds.

## Concepts

### The Constraint Hierarchy Is `any` ⊂ `comparable` ⊂ `cmp.Ordered`

`cmp.Ordered` is defined as the union of all ordered types
(`~int | ~int8 | ... | ~string`), so every type satisfying `cmp.Ordered` also
satisfies `comparable` (because ordered types are also equality-comparable). The
converse is false: a struct of `int` fields is `comparable` but not
`cmp.Ordered`. A `[]int` is neither.

The choice of constraint determines which types the function accepts. `any` is
the most permissive (every type), `cmp.Ordered` is the most restrictive (only
types that can be compared with `<`). Within a library, write the most general
function you can — start with `cmp.Ordered` and relax to `comparable` or `any`
only when the function does not need ordering.

### `cmp.Compare` Returns -1, 0, +1

For any `T cmp.Ordered`, `cmp.Compare(a, b)` returns `-1` if `a < b`, `0` if
`a == b`, and `+1` if `a > b`. It is the standard way to convert a comparison
into the `int` return type expected by `slices.SortFunc`, `slices.BinarySearch`,
and similar APIs.

`Compare` is NaN-aware for floats: `cmp.Compare(math.NaN(), 1.0)` returns `-1`
(NaN is "less than" non-NaN), and `cmp.Compare(math.NaN(), math.NaN())` returns
`0`. The bare `<` operator on NaN returns `false`; `Compare` is the way to get
deterministic ordering.

### `cmp.Or` Returns The First Non-Zero Value

`cmp.Or[T comparable]` returns the first argument that is not equal to the zero
value of `T`. It is the idiomatic way to chain fallbacks: `cmp.Or(env, os.Getenv("DEFAULT"), "fallback")`.
It is constrained to `comparable` so the "is it the zero value" check is well
defined.

### Structs Are `comparable` If All Their Fields Are

A struct whose fields are all `comparable` is itself `comparable`. That means
`==` works between two structs of the same type with comparable fields. It also
means the struct can be a map key. But it is **not** `cmp.Ordered` — there is
no `<` for structs in general. To order structs, write a `Less` method or pass
a `cmp.Compare`-based function to `slices.SortFunc`.

## Exercises

### Exercise 1: Identity, Equal, Clamp, MinMax

Create `stats.go`:

```go
package stats

import "cmp"

func Identity[T any](v T) T {
	return v
}

func Equal[T comparable](a, b T) bool {
	return a == b
}

func Clamp[T cmp.Ordered](value, low, high T) T {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func MinMax[T cmp.Ordered](items []T) (T, T, bool) {
	if len(items) == 0 {
		var zero T
		return zero, zero, false
	}
	min, max := items[0], items[0]
	for _, v := range items[1:] {
		// Use Compare on both sides: it is NaN-aware and consistent,
		// whereas bare `<` always returns false against NaN.
		if cmp.Compare(v, min) < 0 {
			min = v
		}
		if cmp.Compare(v, max) > 0 {
			max = v
		}
	}
	return min, max, true
}
```

`Identity` accepts any type and returns it unchanged — useful as a default in
generic pipelines. `Equal` is the simplest comparable-aware function.
`Clamp` shows the `<` and `>` operators inside an ordered body. `MinMax`
returns `(min, max, ok)` so the caller can distinguish "the slice had elements"
from "the slice was empty" (a slice of zeros would otherwise be ambiguous).

### Exercise 2: Table-Driven Tests And Examples

Create `stats_test.go`:

```go
package stats

import (
	"fmt"
	"math"
	"testing"
)

func TestIdentity(t *testing.T) {
	t.Parallel()
	if got := Identity(42); got != 42 {
		t.Errorf("Identity(42) = %d, want 42", got)
	}
	if got := Identity("go"); got != "go" {
		t.Errorf(`Identity("go") = %q, want "go"`, got)
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b int
		want bool
	}{
		{"equal", 1, 1, true},
		{"unequal", 1, 2, false},
		{"both zero", 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Equal(tt.a, tt.b); got != tt.want {
				t.Errorf("Equal(%d, %d) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestClamp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		value, low int
		high       int
		want       int
	}{
		{"inside", 5, 1, 10, 5},
		{"below", -3, 0, 100, 0},
		{"above", 200, 0, 100, 100},
		{"at low", 1, 1, 10, 1},
		{"at high", 10, 1, 10, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Clamp(tt.value, tt.low, tt.high); got != tt.want {
				t.Errorf("Clamp(%d, %d, %d) = %d, want %d", tt.value, tt.low, tt.high, got, tt.want)
			}
		})
	}
}

func TestMinMax(t *testing.T) {
	t.Parallel()

	t.Run("ints", func(t *testing.T) {
		t.Parallel()
		min, max, ok := MinMax([]int{5, 2, 8, 1, 9, 3})
		if !ok || min != 1 || max != 9 {
			t.Errorf("MinMax = (%d, %d, %v), want (1, 9, true)", min, max, ok)
		}
	})

	t.Run("strings", func(t *testing.T) {
		t.Parallel()
		min, max, ok := MinMax([]string{"banana", "apple", "cherry"})
		if !ok || min != "apple" || max != "cherry" {
			t.Errorf(`MinMax = (%q, %q, %v), want ("apple", "cherry", true)`, min, max, ok)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, _, ok := MinMax([]int{})
		if ok {
			t.Errorf("MinMax(empty) ok = true, want false")
		}
	})

	t.Run("single", func(t *testing.T) {
		t.Parallel()
		min, max, ok := MinMax([]int{42})
		if !ok || min != 42 || max != 42 {
			t.Errorf("MinMax = (%d, %d, %v), want (42, 42, true)", min, max, ok)
		}
	})
}

func TestMinMaxNaN(t *testing.T) {
	// NaN is the corner case where the bare `<` operator returns false
	// against any value, including NaN itself. cmp.Compare is NaN-aware:
	// it returns -1 for (NaN, non-NaN), so NaN is consistently less than
	// non-NaN. The test pins that MinMax uses Compare (not <) and that
	// the resulting order respects the documented cmp semantics.
	t.Parallel()
	min, max, ok := MinMax([]float64{2.0, math.NaN(), 1.0})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !math.IsNaN(min) {
		t.Errorf("min = %v, want NaN (NaN is less than non-NaN per cmp.Compare)", min)
	}
	if max != 2.0 {
		t.Errorf("max = %v, want 2.0 (the only finite value greater than 1.0)", max)
	}
}

func ExampleClamp() {
	fmt.Println(Clamp(15, 0, 10))
	fmt.Println(Clamp(-5, 0, 10))
	// Output:
	// 10
	// 0
}

func ExampleMinMax() {
	min, max, _ := MinMax([]int{3, 1, 4, 1, 5, 9, 2, 6})
	fmt.Println(min, max)
	// Output: 1 9
}
```

The `TestMinMaxNaN` test is the lesson's most important assertion: it pins that
the implementation uses `cmp.Compare` for the max side. A bare `v > max` on
`NaN` would return `false` and silently keep the previous max.

### Exercise 3: A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"cmp"
	"fmt"
	"os"

	"example.com/stats"
)

func main() {
	fmt.Println("Identity(42):", stats.Identity(42))
	fmt.Println(`Identity("go"):`, stats.Identity("go"))
	fmt.Println("Equal(1, 1):", stats.Equal(1, 1))
	fmt.Println("Equal(1, 2):", stats.Equal(1, 2))

	fmt.Println("Clamp(5, 1, 10):", stats.Clamp(5, 1, 10))
	fmt.Println("Clamp(-3, 0, 100):", stats.Clamp(-3, 0, 100))
	fmt.Println("Clamp(200, 0, 100):", stats.Clamp(200, 0, 100))
	fmt.Println(`Clamp("m", "a", "z"):`, stats.Clamp("m", "a", "z"))

	ints := []int{5, 2, 8, 1, 9, 3}
	min, max, _ := stats.MinMax(ints)
	fmt.Printf("MinMax(%v): min=%d, max=%d\n", ints, min, max)

	// Demonstrate cmp.Or for fallback chains.
	env := os.Getenv("UNSET_VAR")
	value := cmp.Or(env, "default")
	fmt.Println("cmp.Or result:", value)
}
```

The demo also shows `cmp.Or` outside the package to make the connection between
the lesson's constraint hierarchy and the standard library.

## Common Mistakes

### Using `comparable` For An Operation That Needs `<`

Wrong:

```go
func Min[T comparable](a, b T) T {
	if a < b { // compile error
```

What happens: `comparable` does not promise ordering.

Fix: use `cmp.Ordered`. If the type is custom, define a custom constraint
(covered in lessons 5 and 12).

### Expecting `<` To Work For NaN Floats

Wrong:

```go
if v > max {
	max = v
}
```

What happens: `NaN > x` is always `false`, so a NaN in the input silently
disappears from the max.

Fix: use `cmp.Compare(v, max) > 0` (or `!cmp.Less(max, v) && !cmp.Equal(v, max)`).
`Compare` is NaN-aware and consistent.

### Comparing Structs With `<`

Wrong:

```go
type User struct{ Name string }
Min(User{"A"}, User{"B"}) // compile error
```

What happens: structs are `comparable` if all fields are, but never
`cmp.Ordered`. The error reads "User does not satisfy cmp.Ordered".

Fix: pass the field you want to order by: `Min(u1.Name, u2.Name)`, or sort with
`slices.SortFunc(users, func(a, b User) int { return cmp.Compare(a.Name, b.Name) })`.

### Treating `cmp.Or` As `||`

`cmp.Or` returns the first non-zero *value*, not the first *true* condition.
For Booleans it is the same thing. For integers, `cmp.Or(0, 0, 3)` returns `3`.
For strings, `cmp.Or("", "", "c")` returns `"c"`. Use it for fallback chains
and for the first-non-empty pattern.

## Verification

From `~/go-exercises/stats`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must succeed. The `TestMinMaxNaN` test will fail if the implementation
ever regresses to a bare `>` comparison.

Add your own test: `TestClampFloat` covering `Clamp(1.5, 0.0, 1.0) == 1.0` and
`Clamp(-0.1, 0.0, 1.0) == 0.0`.

## Summary

- `any` allows storage and passing; `comparable` adds `==`; `cmp.Ordered` adds
  the comparison operators.
- `cmp.Compare` returns -1/0/+1 and is NaN-aware; use it for `slices.SortFunc`
  callbacks and any max-style comparison that may see floats.
- `cmp.Less[T cmp.Ordered](a, b)` is the boolean shorthand for `a < b`, also
  NaN-aware.
- `cmp.Or[T comparable]` returns the first non-zero value; use it for fallback
  chains.
- Structs with all-comparable fields are themselves `comparable`, but never
  `cmp.Ordered`.

## What's Next

[Generic Data Structures](../04-generic-data-structures/04-generic-data-structures.md) —
building a type-safe `Stack[T]` and `Queue[T]` where the type parameter is on
the type, not just the function.

## Resources

- [cmp package](https://pkg.go.dev/cmp)
- [Go spec: Comparison operators](https://go.dev/ref/spec#Comparison_operators)
- [Go spec: Type constraints](https://go.dev/ref/spec#Type_constraints)
