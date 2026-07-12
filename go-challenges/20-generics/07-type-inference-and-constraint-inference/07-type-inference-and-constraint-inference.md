# 7. Type Inference and Constraint Inference

Type inference is what keeps generic Go readable. This lesson builds a small transformation package where the compiler infers most type arguments from ordinary function arguments and from constraints such as `S ~[]E`.

## Concepts

### Function Arguments Drive Inference

In `Map([]int{1}, strconv.Itoa)`, Go infers the source element type from the slice and the destination type from the callback. You only write explicit type arguments when a type parameter cannot be learned from the arguments.

### Constraint Inference Decomposes Types

A constraint such as `S ~[]E` tells the compiler that `S` is a slice type whose element type is `E`. If the argument is `IDs`, where `type IDs []int`, Go can infer both `S = IDs` and `E = int`.

### Return-Only Type Parameters Are A Smell

If a type parameter appears only in the return type, callers often have to provide it explicitly. Prefer APIs where the important types are present in the inputs or in a function argument.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/20-generics/07-type-inference-and-constraint-inference/07-type-inference-and-constraint-inference/cmd/demo
cd go-solutions/20-generics/07-type-inference-and-constraint-inference/07-type-inference-and-constraint-inference
```

### Exercise 1: Build Inference-Friendly Helpers

Create `transform.go`:

```go
package inference

import (
	"errors"
	"fmt"
)

var ErrEmptySlice = errors.New("slice must not be empty")

func Map[S, D any](src []S, fn func(S) D) []D {
	out := make([]D, len(src))
	for i, value := range src {
		out[i] = fn(value)
	}
	return out
}

func Clone[S ~[]E, E any](src S) S {
	out := make(S, len(src))
	copy(out, src)
	return out
}

func First[S ~[]E, E any](src S) (E, error) {
	var zero E
	if len(src) == 0 {
		return zero, fmt.Errorf("first: %w", ErrEmptySlice)
	}
	return src[0], nil
}
```

### Exercise 2: Add Tests And An Example

Create `transform_test.go`:

```go
package inference

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
)

type IDs []int

func TestMapInfersTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []int
		want []string
	}{
		{name: "empty", in: nil, want: []string{}},
		{name: "numbers", in: []int{1, 2}, want: []string{"#1", "#2"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Map(tt.in, func(n int) string { return "#" + strconv.Itoa(n) })
			if got == nil {
				got = []string{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Map() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestClonePreservesNamedSliceType(t *testing.T) {
	t.Parallel()

	in := IDs{1, 2, 3}
	got := Clone(in)
	if reflect.TypeOf(got) != reflect.TypeOf(in) {
		t.Fatalf("Clone() type = %T, want %T", got, in)
	}
	got[0] = 99
	if in[0] == 99 {
		t.Fatal("Clone() should not alias the input backing array")
	}
}

func TestFirstRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := First(IDs{})
	if !errors.Is(err, ErrEmptySlice) {
		t.Fatalf("err = %v, want ErrEmptySlice", err)
	}
}

func ExampleFirst() {
	first, _ := First(IDs{7, 8})
	fmt.Println(first)
	// Output: 7
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strconv"

	inference "example.com/verify"
)

func main() {
	labels := inference.Map([]int{1, 2, 3}, func(n int) string { return "item-" + strconv.Itoa(n) })
	fmt.Println(labels)
}
```

## Common Mistakes

### Making Callers Write Avoidable Type Arguments

Wrong: designing helpers that require `Map[int, string]` at every call.

Fix: put type parameters in arguments so the compiler can infer them.

### Losing Named Slice Types

Wrong: `func Clone[E any](src []E) []E` when callers use named slice types.

Fix: use `func Clone[S ~[]E, E any](src S) S` to preserve `S`.

### Ignoring Empty Inputs

Wrong: returning the zero element from `First(nil)` without saying why.

Fix: return a wrapped sentinel error and test it with `errors.Is`.

## Verification

Run this from `~/go-exercises/inference`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test proving `First([]string{"go"})` infers `E = string` without explicit type arguments.

## Summary

- Go infers type arguments from function arguments and callback signatures.
- Constraints such as `S ~[]E` let the compiler infer related type parameters.
- Named slice types can be preserved with a separate slice type parameter.
- Return-only type parameters usually make APIs harder to call.

## What's Next

Next: [Generic Tree Structures](../08-generic-tree-structures/08-generic-tree-structures.md).

## Resources

- [Go Blog: Deconstructing Type Parameters](https://go.dev/blog/deconstructing-type-parameters)
- [Go Specification: Type inference](https://go.dev/ref/spec#Type_inference)
- [Go Blog: An Introduction To Generics](https://go.dev/blog/intro-generics)
