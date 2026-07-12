# Exercise 3: A Recursive Tree Push Iterator

A flat loop honors the yield protocol with a plain `return`. Recursion does not
-- a bare `return` unwinds one frame while the parent keeps walking, calling
`yield` after it already said stop. This exercise builds an in-order push
iterator over a binary search tree using the bool-threaded helper that makes
early `break` propagate correctly through every recursion frame, and contrasts it
with the trivial flat-loop case of a linked list.

This module is fully self-contained. It begins with its own `go mod init`,
defines every type it needs, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
tree.go              List.All (flat loop) and Tree.InOrder (bool-threaded recursion)
cmd/
  demo/
    main.go          full in-order walk, an early break, a linked-list walk
tree_test.go         sorted in-order output, early-break prefix, list early break
```

- Files: `tree.go`, `cmd/demo/main.go`, `tree_test.go`.
- Implement: `List[V].All() iter.Seq[V]` (flat loop) and a binary search tree
  `Tree[V cmp.Ordered]` with `Insert(v V)` and `InOrder() iter.Seq[V]` built on a
  recursive helper that returns `bool`.
- Test: `tree_test.go` checks the in-order walk is sorted, breaks early and
  asserts the exact prefix (which also proves no panic from a continued yield),
  and breaks early out of the list.
- Verify: `go test -run 'TestTree|TestList' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/03-tree-in-order-iterator/cmd/demo && cd go-solutions/25-iterators-and-modern-go/03-range-over-func-push-iterators/03-tree-in-order-iterator
```

### The easy case: a flat loop honors the protocol with return

A singly linked list iterates with one loop and no recursion, so the protocol is
the same `if !yield(v) { return }` from the first exercise. The `return` leaves
the whole iterator function, which is exactly what "stop" means here:

```go
for cur := l.head; cur != nil; cur = cur.next {
	if !yield(cur.value) {
		return
	}
}
```

Naming follows the standard library: a method that walks every element of a
collection is called `All` and returns `iter.Seq[V]`, the way `slices.All` and
`maps.All` do. The flat case is the baseline; the tree is where the protocol
needs more care.

### The hard case: why recursion needs a bool-threaded helper

An in-order traversal of a binary tree is naturally recursive: visit the left
subtree, yield the node, visit the right subtree. The tempting way to write it is
a helper that returns nothing and bails with a bare `return` when `yield` says
stop. That is a bug. A `return` inside the helper unwinds only the current call
frame -- it stops the work at one node, but the parent frame, which is mid-way
through its own "left, self, right" sequence, simply continues to the next step
and calls `yield` again. Calling `yield` after it returned `false` is the one
thing the protocol forbids, and the loop machinery the compiler generates panics
when it happens.

The correct pattern makes the helper return a bool meaning "keep going" and
checks it at every recursive call site:

```go
func (n *treeNode[V]) pushInOrder(yield func(V) bool) bool {
	if n == nil {
		return true
	}
	if !n.left.pushInOrder(yield) {
		return false
	}
	if !yield(n.value) {
		return false
	}
	return n.right.pushInOrder(yield)
}
```

Trace an early stop. Say the consumer `break`s on the value that lives deep in
the left subtree. `yield` returns `false`, so `pushInOrder` at that node returns
`false`. Its caller guarded the call with `if !n.left.pushInOrder(yield) { return
false }`, so it returns `false` too -- without touching its own `yield(n.value)`
or right subtree. That `false` races straight up through every parent frame, each
short-circuiting the same way, until the top-level `InOrder` closure returns. No
frame calls `yield` after the stop, so there is no panic and no wasted work. The
`nil` base case returns `true` because an empty subtree is not a reason to stop;
it just contributes nothing. Notice the helper is called on possibly-nil
receivers (`n.left.pushInOrder`) and handles `nil` itself, which keeps the call
sites free of nil checks.

Create `tree.go`:

```go
// Package treeiter builds push iterators over a linked list (flat loop) and a
// binary search tree (recursion that threads the yield-stop signal as a bool).
package treeiter

import (
	"cmp"
	"iter"
)

// List is a singly linked list whose All method iterates with a flat loop.
type List[V any] struct {
	head *listNode[V]
}

type listNode[V any] struct {
	value V
	next  *listNode[V]
}

// NewList builds a list preserving the order of values.
func NewList[V any](values ...V) *List[V] {
	var head *listNode[V]
	for i := len(values) - 1; i >= 0; i-- {
		head = &listNode[V]{value: values[i], next: head}
	}
	return &List[V]{head: head}
}

// All iterates every element in order. A flat loop honors the yield protocol
// with a plain return.
func (l *List[V]) All() iter.Seq[V] {
	return func(yield func(V) bool) {
		for cur := l.head; cur != nil; cur = cur.next {
			if !yield(cur.value) {
				return
			}
		}
	}
}

// Tree is a binary search tree of ordered values; duplicates are ignored.
type Tree[V cmp.Ordered] struct {
	root *treeNode[V]
}

type treeNode[V cmp.Ordered] struct {
	value       V
	left, right *treeNode[V]
}

// Insert adds v to the tree, keeping the binary-search-tree ordering.
func (t *Tree[V]) Insert(v V) {
	t.root = insert(t.root, v)
}

func insert[V cmp.Ordered](n *treeNode[V], v V) *treeNode[V] {
	if n == nil {
		return &treeNode[V]{value: v}
	}
	switch {
	case v < n.value:
		n.left = insert(n.left, v)
	case v > n.value:
		n.right = insert(n.right, v)
	}
	return n
}

// InOrder iterates the values in ascending (in-order) order. The traversal is
// recursive, so the stop signal is threaded back up as a bool by pushInOrder.
func (t *Tree[V]) InOrder() iter.Seq[V] {
	return func(yield func(V) bool) {
		t.root.pushInOrder(yield)
	}
}

// pushInOrder visits the subtree rooted at n in order and returns false the
// moment yield asks to stop, so the caller can short-circuit the rest of the
// traversal instead of calling yield again.
func (n *treeNode[V]) pushInOrder(yield func(V) bool) bool {
	if n == nil {
		return true
	}
	if !n.left.pushInOrder(yield) {
		return false
	}
	if !yield(n.value) {
		return false
	}
	return n.right.pushInOrder(yield)
}
```

### The runnable demo

The demo inserts nine integers in scrambled order and walks them in-order, which
a binary search tree yields sorted. Then it breaks after three values to show the
stop signal threading up out of the recursion cleanly. Finally it walks a linked
list to show the flat-loop iterator side by side.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/tree-iter"
)

func main() {
	t := &treeiter.Tree[int]{}
	for _, v := range []int{5, 3, 8, 1, 4, 7, 9, 2, 6} {
		t.Insert(v)
	}

	fmt.Print("in-order:")
	for v := range t.InOrder() {
		fmt.Printf(" %d", v)
	}
	fmt.Println()

	fmt.Print("first 3:")
	n := 0
	for v := range t.InOrder() {
		if n == 3 {
			break
		}
		fmt.Printf(" %d", v)
		n++
	}
	fmt.Println()

	list := treeiter.NewList("go", "rust", "zig")
	fmt.Print("list:")
	for s := range list.All() {
		fmt.Printf(" %s", s)
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-order: 1 2 3 4 5 6 7 8 9
first 3: 1 2 3
list: go rust zig
```

### Tests

`TestTreeInOrder` inserts scrambled values and asserts the walk comes out sorted,
which is the in-order property of a binary search tree. `TestTreeEarlyBreak` is
the load-bearing one: it breaks after three values and asserts the prefix is
exactly `[1 2 3]`. If the recursive helper used a bare `return` instead of
threading the bool, this case would not merely return the wrong prefix -- it would
panic when a parent frame called `yield` after the stop, so a clean pass proves
the propagation works. `TestListEarlyBreak` does the same for the flat-loop list.

Create `tree_test.go`:

```go
package treeiter

import (
	"slices"
	"testing"
)

func buildTree(values ...int) *Tree[int] {
	t := &Tree[int]{}
	for _, v := range values {
		t.Insert(v)
	}
	return t
}

func TestTreeInOrder(t *testing.T) {
	t.Parallel()

	tree := buildTree(5, 3, 8, 1, 4, 7, 9, 2, 6)
	var got []int
	for v := range tree.InOrder() {
		got = append(got, v)
	}
	if want := []int{1, 2, 3, 4, 5, 6, 7, 8, 9}; !slices.Equal(got, want) {
		t.Fatalf("in-order = %v, want %v", got, want)
	}
}

func TestTreeEarlyBreak(t *testing.T) {
	t.Parallel()

	tree := buildTree(5, 3, 8, 1, 4, 7, 9, 2, 6)
	var got []int
	for v := range tree.InOrder() {
		if len(got) == 3 {
			break
		}
		got = append(got, v)
	}
	if want := []int{1, 2, 3}; !slices.Equal(got, want) {
		t.Fatalf("early break = %v, want %v", got, want)
	}
}

func TestListAll(t *testing.T) {
	t.Parallel()

	var got []string
	for s := range NewList("a", "b", "c").All() {
		got = append(got, s)
	}
	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("list = %v, want %v", got, want)
	}
}

func TestListEarlyBreak(t *testing.T) {
	t.Parallel()

	var got []int
	for v := range NewList(10, 20, 30, 40).All() {
		if v == 30 {
			break
		}
		got = append(got, v)
	}
	if want := []int{10, 20}; !slices.Equal(got, want) {
		t.Fatalf("list early break = %v, want %v", got, want)
	}
}
```

## Review

The tree iterator is correct when an early `break` propagates out of the
recursion without a panic and yields the exact prefix the consumer asked for.
That property lives in the bool return of `pushInOrder` and the
`if !child.pushInOrder(yield) { return false }` guard at every call site:
`TestTreeEarlyBreak` collecting precisely `[1 2 3]` (and not panicking) is the
proof. Confirm the in-order walk of a binary search tree is sorted, that the
`nil` base case returns `true` so empty subtrees do not stop the traversal, and
that the flat-loop list honors the same protocol with a plain `return`. All four
tests passing under `go test -race ./...` establishes both shapes of the
protocol.

Common mistakes for this feature. The first and most important is writing the
recursive helper to return nothing and stop with a bare `return`; it unwinds one
frame, the parent keeps walking, and the next `yield` after the stop panics --
the bool-threaded helper is the fix. The second is forgetting the `nil` base
case, or returning `false` from it, which would stop the entire traversal the
first time a subtree is empty. The third is duplicating nil checks at the call
sites instead of letting the method handle a nil receiver, which clutters the
recursion and is easy to get inconsistent.

## Resources

- [`iter` package](https://pkg.go.dev/iter) -- `Seq[V]` and the documented yield
  contract the recursive helper must honor.
- [Go Blog: Range Over Function Types](https://go.dev/blog/range-functions) --
  how `break` and `panic` in the loop body turn into a `false` from `yield`.
- [`cmp` package](https://pkg.go.dev/cmp) -- the `cmp.Ordered` constraint that
  lets the binary search tree compare values with `<`.

---

Back to [02-seq2-key-value-iterators.md](02-seq2-key-value-iterators.md) | Next: [04-paginated-api-push-iterator.md](04-paginated-api-push-iterator.md)
