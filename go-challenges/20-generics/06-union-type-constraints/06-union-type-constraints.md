# 6. Union Type Constraints

Union constraints describe the exact set of types that may use operators such as `+`, `<`, and conversion to `float64`. This lesson builds numeric helpers that accept built-in numeric types and named aliases with matching underlying types.

## Concepts

### A Union Is A Type Set

Inside a constraint interface, `~int | ~int64 | ~float64` means a type argument may be any type whose underlying type is one of those terms. The `|` operator is not a runtime branch; it is a compile-time description of allowed types.

### The Tilde Includes Named Types

Without `~`, `type Cents int64` does not satisfy `int64`. With `~int64`, it does. This is what makes reusable numeric helpers work with domain types instead of forcing callers back to raw primitives.

### Constraints Should Be No Wider Than The Operation Needs

If a function only adds values, a numeric constraint is enough. If it also orders values, the constraint must include only types that support ordering. Precise constraints make invalid calls fail at compile time.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/20-generics/06-union-type-constraints/06-union-type-constraints/cmd/demo
cd go-solutions/20-generics/06-union-type-constraints/06-union-type-constraints
```

### Exercise 1: Build Numeric Constraints

Create `numbers.go`:

```go
package unionconstraints

import (
	"errors"
	"fmt"
)

var ErrEmptyValues = errors.New("values must not be empty")

type Integer interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 | ~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64
}

type Float interface {
	~float32 | ~float64
}

type Number interface {
	Integer | Float
}

func Sum[T Number](values []T) T {
	var total T
	for _, value := range values {
		total += value
	}
	return total
}

func Average[T Number](values []T) (float64, error) {
	if len(values) == 0 {
		return 0, fmt.Errorf("average: %w", ErrEmptyValues)
	}
	return float64(Sum(values)) / float64(len(values)), nil
}

func Clamp[T Number](value, min, max T) (T, error) {
	if min > max {
		return value, fmt.Errorf("clamp range: %w", ErrEmptyValues)
	}
	if value < min {
		return min, nil
	}
	if value > max {
		return max, nil
	}
	return value, nil
}
```

### Exercise 2: Add Tests And An Example

Create `numbers_test.go`:

```go
package unionconstraints

import (
	"errors"
	"fmt"
	"testing"
)

type Cents int64

func TestSumWithNamedTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []Cents
		want Cents
	}{
		{name: "empty", in: nil, want: 0},
		{name: "cents", in: []Cents{100, 250, 50}, want: 400},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Sum(tt.in); got != tt.want {
				t.Fatalf("Sum() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAverageRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := Average([]int(nil))
	if !errors.Is(err, ErrEmptyValues) {
		t.Fatalf("err = %v, want ErrEmptyValues", err)
	}
}

func TestClamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value int
		min   int
		max   int
		want  int
		err   error
	}{
		{name: "inside", value: 5, min: 1, max: 10, want: 5},
		{name: "low", value: -1, min: 1, max: 10, want: 1},
		{name: "bad range", value: 5, min: 10, max: 1, want: 5, err: ErrEmptyValues},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Clamp(tt.value, tt.min, tt.max)
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("Clamp() = %v, want %v", got, tt.want)
			}
		})
	}
}

func ExampleAverage() {
	avg, _ := Average([]Cents{100, 200, 300})
	fmt.Printf("%.0f\n", avg)
	// Output: 200
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	unionconstraints "example.com/verify"
)

func main() {
	avg, err := unionconstraints.Average([]float64{10, 20, 30})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("average %.0f\n", avg)
}
```

## Common Mistakes

### Omitting `~` For Domain Types

Wrong: `type Integer interface { int64 }`, then expecting `type Cents int64` to work.

Fix: use `~int64` when named types with that underlying type should be accepted.

### Using `any` And Hoping Operators Work

Wrong: `func Sum[T any](values []T) T { total += value }`.

Fix: constrain `T` to a type set where `+` is defined.

### Returning Zero For Invalid Input Without An Error

Wrong: `Average(nil)` returns `0`, which is indistinguishable from a real average.

Fix: return a sentinel error and test it with `errors.Is`.

## Verification

Run this from `~/go-exercises/unionconstraints`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test using a named `type Score float64` to prove the `~float64` term accepts named float types.

## Summary

- Union constraints describe compile-time type sets.
- The `~` term includes named types with the same underlying type.
- Operators are available only when every type in the set supports them.
- Invalid runtime input still needs normal Go error handling.

## What's Next

Next: [Type Inference and Constraint Inference](../07-type-inference-and-constraint-inference/07-type-inference-and-constraint-inference.md).

## Resources

- [Go Specification: General interfaces](https://go.dev/ref/spec#General_interfaces)
- [Go Blog: Deconstructing Type Parameters](https://go.dev/blog/deconstructing-type-parameters)
- [Go Blog: An Introduction To Generics](https://go.dev/blog/intro-generics)
