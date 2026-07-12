# 8. Generic Tree Structures

A binary search tree is a useful generics exercise because the implementation needs ordering, recursion, pointer mutation, and a concrete error contract. This lesson builds a small generic tree that stores unique ordered values.

## Concepts

### Ordering Is A Constraint, Not A Runtime Check

Tree insertion compares values with `<` and `>`, so the type parameter must be constrained to ordered types. Using `cmp.Ordered` gives the compiler enough information to allow comparisons and reject unordered types.

### Recursive Generic Types Are Ordinary Types

`node[T]` can point to `*node[T]` on the left and right. Each instantiation, such as `Tree[int]` or `Tree[string]`, has a consistent node type throughout the tree.

### Duplicate Policy Must Be Explicit

A search tree can count duplicates, store them on one side, or reject them. This lesson rejects duplicates with a sentinel error so callers know whether insertion changed the tree.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/20-generics/08-generic-tree-structures/08-generic-tree-structures/cmd/demo
cd go-solutions/20-generics/08-generic-tree-structures/08-generic-tree-structures
```

### Exercise 1: Build The Tree

Create `tree.go`:

```go
package generictree

import (
	"cmp"
	"errors"
	"fmt"
)

var ErrDuplicateValue = errors.New("value already exists")

type node[T cmp.Ordered] struct {
	value T
	left  *node[T]
	right *node[T]
}

type Tree[T cmp.Ordered] struct {
	root *node[T]
	size int
}

func (t *Tree[T]) Insert(value T) error {
	var inserted bool
	var err error
	t.root, inserted, err = insert(t.root, value)
	if err != nil {
		return fmt.Errorf("insert %v: %w", value, err)
	}
	if inserted {
		t.size++
	}
	return nil
}

func insert[T cmp.Ordered](n *node[T], value T) (*node[T], bool, error) {
	if n == nil {
		return &node[T]{value: value}, true, nil
	}
	if value < n.value {
		var inserted bool
		var err error
		n.left, inserted, err = insert(n.left, value)
		return n, inserted, err
	}
	if value > n.value {
		var inserted bool
		var err error
		n.right, inserted, err = insert(n.right, value)
		return n, inserted, err
	}
	return n, false, ErrDuplicateValue
}

func (t *Tree[T]) Contains(value T) bool {
	for n := t.root; n != nil; {
		if value < n.value {
			n = n.left
		} else if value > n.value {
			n = n.right
		} else {
			return true
		}
	}
	return false
}

func (t *Tree[T]) InOrder() []T {
	out := make([]T, 0, t.size)
	walk(t.root, &out)
	return out
}

func walk[T cmp.Ordered](n *node[T], out *[]T) {
	if n == nil {
		return
	}
	walk(n.left, out)
	*out = append(*out, n.value)
	walk(n.right, out)
}

func (t *Tree[T]) Size() int {
	return t.size
}
```

### Exercise 2: Add Tests And An Example

Create `tree_test.go`:

```go
package generictree

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestTreeInOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []int
		want []int
	}{
		{name: "empty", in: nil, want: []int{}},
		{name: "sorted", in: []int{5, 2, 8, 1}, want: []int{1, 2, 5, 8}},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var tree Tree[int]
			for _, value := range tt.in {
				if err := tree.Insert(value); err != nil {
					t.Fatal(err)
				}
			}
			got := tree.InOrder()
			if got == nil {
				got = []int{}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("InOrder() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestTreeRejectsDuplicates(t *testing.T) {
	t.Parallel()

	var tree Tree[string]
	if err := tree.Insert("go"); err != nil {
		t.Fatal(err)
	}
	if err := tree.Insert("go"); !errors.Is(err, ErrDuplicateValue) {
		t.Fatalf("err = %v, want ErrDuplicateValue", err)
	}
}

func TestContains(t *testing.T) {
	t.Parallel()

	var tree Tree[int]
	for _, value := range []int{3, 1, 4} {
		if err := tree.Insert(value); err != nil {
			t.Fatal(err)
		}
	}
	if !tree.Contains(4) || tree.Contains(9) {
		t.Fatalf("Contains returned wrong result")
	}
}

func ExampleTree_InOrder() {
	var tree Tree[int]
	for _, value := range []int{3, 1, 2} {
		_ = tree.Insert(value)
	}
	fmt.Println(tree.InOrder())
	// Output: [1 2 3]
}
```

### Exercise 3: Add A Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	generictree "example.com/verify"
)

func main() {
	var tree generictree.Tree[string]
	for _, value := range []string{"go", "rust", "c"} {
		if err := tree.Insert(value); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println(tree.InOrder())
}
```

## Common Mistakes

### Allowing Unordered Types

Wrong: `type Tree[T any]` and then trying to compare `T` values.

Fix: use `cmp.Ordered` because insertion and search require ordering.

### Incrementing Size Before Validation

Wrong: incrementing `size` before discovering a duplicate.

Fix: increment only after `insert` reports that it created a node.

### Hiding Duplicate Inserts

Wrong: silently ignoring a duplicate value.

Fix: return `ErrDuplicateValue` so the caller can distinguish no-op from success.

## Verification

Run this from `~/go-exercises/generictree`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

Add one more test that inserts `[]string{"b", "a", "c"}` and verifies the in-order traversal.

## Summary

- Generic recursive types work like non-generic recursive types.
- Ordered operations require an ordered constraint.
- Tree invariants must define what happens with duplicates.
- Tests should verify both traversal order and error contracts.

## What's Next

Next: [Generic Iterator Patterns](../09-generic-iterator-patterns/09-generic-iterator-patterns.md).

## Resources

- [cmp package](https://pkg.go.dev/cmp)
- [Go Specification: Type parameters](https://go.dev/ref/spec#Type_parameter_declarations)
- [Go Blog: When To Use Generics](https://go.dev/blog/when-generics)
