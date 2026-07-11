# 5. Interface Constraints with Methods

Method constraints let generic code call behavior on a type parameter without falling back to `any`. This lesson builds a small formatting package where each item supplies a stable label, and the generic functions preserve concrete types while using that method.

## Concepts

### Constraints Can Require Methods

An ordinary interface such as `interface { Label() string }` can be used as a constraint. Inside `func Labels[T Labeled](items []T) []string`, the compiler lets the function call `item.Label()` because every possible `T` in the type set has that method.

### Method Constraints Keep The Element Type

An interface-typed slice, such as `[]fmt.Stringer`, stores interface values. A generic function with `T Labeled` accepts `[]Product`, `[]Color`, or any other concrete slice without converting each element into an interface value first. The result is still checked at compile time.

### Validation Still Belongs At Package Boundaries

The generic algorithm can require the method, but constructors should still validate domain values. The tests below use sentinel errors wrapped with `%w` so callers can use `errors.Is` instead of comparing strings.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/methodconstraints/cmd/demo
cd ~/go-exercises/methodconstraints
go mod init example.com/verify
```

### Exercise 1: Build The Generic Label Functions

Create `labels.go`:

```go
package methodconstraints

import (
	"errors"
	"fmt"
	"strings"
)

var ErrEmptyLabel = errors.New("label must not be empty")

type Labeled interface {
	Label() string
}

type Product struct {
	SKU  string
	Name string
}

func NewProduct(sku, name string) (Product, error) {
	sku = strings.TrimSpace(sku)
	name = strings.TrimSpace(name)
	if sku == "" || name == "" {
		return Product{}, fmt.Errorf("product: %w", ErrEmptyLabel)
	}
	return Product{SKU: sku, Name: name}, nil
}

func (p Product) Label() string {
	return p.SKU + ":" + p.Name
}

type Color struct {
	R uint8
	G uint8
	B uint8
}

func (c Color) Label() string {
	return fmt.Sprintf("rgb(%d,%d,%d)", c.R, c.G, c.B)
}

func Labels[T Labeled](items []T) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.Label()
	}
	return out
}

func JoinLabels[T Labeled](items []T, sep string) string {
	return strings.Join(Labels(items), sep)
}
```

### Exercise 2: Add Tests And An Example

Create `labels_test.go`:

```go
package methodconstraints

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestLabels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []Color
		want []string
	}{
		{name: "empty", in: nil, want: []string{}},
		{name: "colors", in: []Color{{R: 255}, {G: 128, B: 64}}, want: []string{"rgb(255,0,0)", "rgb(0,128,64)"}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := Labels(tt.in)
			if tt.want == nil {
				tt.want = []string{}
			}
			if got == nil {
				got = []string{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Labels() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNewProductValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sku  string
		item string
	}{
		{name: "missing sku", sku: "", item: "Keyboard"},
		{name: "missing name", sku: "K1", item: ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewProduct(tt.sku, tt.item)
			if !errors.Is(err, ErrEmptyLabel) {
				t.Fatalf("err = %v, want ErrEmptyLabel", err)
			}
		})
	}
}

func ExampleJoinLabels() {
	items := []Color{{R: 255}, {G: 255}}
	fmt.Println(JoinLabels(items, " | "))
	// Output: rgb(255,0,0) | rgb(0,255,0)
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	methodconstraints "example.com/verify"
)

func main() {
	keyboard, err := methodconstraints.NewProduct("K1", "Keyboard")
	if err != nil {
		log.Fatal(err)
	}
	mouse, err := methodconstraints.NewProduct("M1", "Mouse")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(methodconstraints.JoinLabels([]methodconstraints.Product{keyboard, mouse}, ", "))
}
```

## Common Mistakes

### Accepting `[]Labeled` Instead Of `[]T`

Wrong: `func Labels(items []Labeled) []string`. A `[]Product` cannot be passed directly to `[]Labeled` because slices are not covariant.

Fix: use `func Labels[T Labeled](items []T) []string` so the caller keeps the concrete slice type.

### Forgetting That The Constraint Is Compile-Time Only

Wrong: adding reflection checks inside `Labels` to see whether `Label` exists.

Fix: let the constraint do that work. Code that calls `Labels([]int{1})` does not compile.

### Comparing Error Strings

Wrong: `err.Error() == "label must not be empty"`.

Fix: wrap `ErrEmptyLabel` with `%w` and assert `errors.Is(err, ErrEmptyLabel)`.

## Verification

Run this from `~/go-exercises/methodconstraints`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that builds two `Product` values and verifies `JoinLabels` returns the labels in input order.

## Summary

- Interfaces with methods can be used directly as generic constraints.
- A method constraint lets generic code call that method on `T`.
- Generic functions avoid forcing callers to convert `[]Concrete` into `[]Interface`.
- Domain constructors still validate values and return sentinel errors.

## What's Next

Next: [Union Type Constraints](../06-union-type-constraints/06-union-type-constraints.md).

## Resources

- [Go Specification: Interface types](https://go.dev/ref/spec#Interface_types)
- [Go Blog: An Introduction To Generics](https://go.dev/blog/intro-generics)
- [Go Blog: When To Use Generics](https://go.dev/blog/when-generics)
