# Exercise 9: A Binary Tree With Nil-Safe *node Pointer Methods

A method with a pointer receiver can be called on a nil pointer, as long as the
method does not dereference a nil field. This exercise builds a binary search tree
whose `Count`, `Contains`, and `Insert` methods are defined on `*node` and treat a
nil receiver as the empty tree — so callers recurse into `left`/`right` without ever
nil-checking first, and the base case falls out of the receiver itself.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
niltree/                  independent module: example.com/niltree
  go.mod
  tree.go                 node{val,left,right}; Insert/Count/Contains on *node, nil-safe
  cmd/
    demo/
      main.go             build a tree, print count / contains / in-order
  tree_test.go            nil receiver returns 0/false; insert on nil; ordering; no panics
```

Files: `tree.go`, `cmd/demo/main.go`, `tree_test.go`.
Implement: a `node` with `val int` and `left, right *node`; methods `Insert(v int) *node`, `Count() int`, `Contains(v int) bool`, and `InOrder() []int`, each safe to call on a nil `*node`.
Test: `Count`/`Contains`/`InOrder` on a nil `*node` return `0`/`false`/empty without panicking; `Insert` on nil returns a fresh single-node tree; a built tree reports correct `Count`, `Contains` for present/absent values, and in-order (sorted) traversal.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/niltree/cmd/demo
cd ~/go-exercises/niltree
go mod init example.com/niltree
```

### A typed nil is a valid receiver

In Go, a method call `n.Count()` is sugar for `(*node).Count(n)`, passing the pointer
`n` as the receiver argument. That works even when `n` is nil — a nil `*node` is a
perfectly valid value to pass. What is *not* allowed is dereferencing the nil: writing
`n.val` when `n` is nil panics. So a method on `*node` can safely handle a nil receiver
by checking `if n == nil` *first* and returning a base-case answer before touching any
field. That is the whole technique, and it makes recursive tree code dramatically
cleaner: instead of every caller writing `if n.left != nil { total += n.left.Count() }`,
the method itself absorbs the nil case, so `n.left.Count()` on a nil `left` just
returns 0. The recursion's base case lives in the receiver, not in a scattered set of
nil-checks at every call site.

`Count` returns 0 for a nil tree, else `1 + left.Count() + right.Count()`. `Contains`
returns false for a nil tree, else compares and descends left or right. `InOrder`
returns nil (an empty slice) for a nil tree, else the left subtree, then this value,
then the right subtree — which for a BST yields sorted order. `Insert` is the one that
*returns* a `*node`: inserting into a nil tree can't mutate anything (there is no node
to mutate), so it returns a new node; inserting into a non-nil tree descends and
reassigns the child pointer to the (possibly new) subtree root. The
`n.left = n.left.Insert(v)` idiom is why `Insert` returns the subtree root: it lets the
nil case create a node and the non-nil case return the unchanged root, uniformly.

Create `tree.go`:

```go
package niltree

// node is a binary-search-tree node. All methods are defined on *node and are safe
// to call on a nil receiver, which represents the empty tree.
type node struct {
	val         int
	left, right *node
}

// Insert returns the root of the subtree with v inserted. On a nil receiver it
// creates the node; otherwise it descends and reassigns the child pointer.
func (n *node) Insert(v int) *node {
	if n == nil {
		return &node{val: v}
	}
	switch {
	case v < n.val:
		n.left = n.left.Insert(v)
	case v > n.val:
		n.right = n.right.Insert(v)
	}
	// v == n.val: already present, no duplicate inserted.
	return n
}

// Count returns the number of nodes. A nil receiver (empty tree) returns 0.
func (n *node) Count() int {
	if n == nil {
		return 0
	}
	return 1 + n.left.Count() + n.right.Count()
}

// Contains reports whether v is in the tree. A nil receiver returns false.
func (n *node) Contains(v int) bool {
	if n == nil {
		return false
	}
	switch {
	case v < n.val:
		return n.left.Contains(v)
	case v > n.val:
		return n.right.Contains(v)
	default:
		return true
	}
}

// InOrder returns the values in ascending order. A nil receiver returns nil.
func (n *node) InOrder() []int {
	if n == nil {
		return nil
	}
	out := n.left.InOrder()
	out = append(out, n.val)
	out = append(out, n.right.InOrder()...)
	return out
}

// Tree wraps a root *node so callers have a value to start from. The zero Tree is
// an empty, usable tree because its nil root is a valid receiver.
type Tree struct {
	root *node
}

func (t *Tree) Insert(v int)        { t.root = t.root.Insert(v) }
func (t *Tree) Count() int          { return t.root.Count() }
func (t *Tree) Contains(v int) bool { return t.root.Contains(v) }
func (t *Tree) InOrder() []int      { return t.root.InOrder() }
```

### The runnable demo

The demo inserts a handful of values into a zero `Tree` (whose nil root is already a
valid empty tree), then prints the count, a couple of membership checks, and the
sorted in-order traversal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/niltree"
)

func main() {
	var t niltree.Tree // zero value: empty but usable
	for _, v := range []int{5, 3, 8, 1, 4, 7, 9, 3} {
		t.Insert(v) // duplicate 3 is ignored
	}

	fmt.Printf("count=%d\n", t.Count())
	fmt.Printf("contains 7=%v\n", t.Contains(7))
	fmt.Printf("contains 6=%v\n", t.Contains(6))
	fmt.Printf("in-order=%v\n", t.InOrder())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
count=7
contains 7=true
contains 6=false
in-order=[1 3 4 5 7 8 9]
```

### Tests

The tests hit every method on a nil receiver to prove none panic and each returns the
empty-tree answer, then build a tree and check `Count`, `Contains` for present and
absent values, and that `InOrder` yields sorted output. A duplicate-insert test
confirms the tree does not grow on a repeated value.

Create `tree_test.go`:

```go
package niltree

import (
	"slices"
	"testing"
)

func TestNilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var n *node // nil

	if got := n.Count(); got != 0 {
		t.Fatalf("nil Count = %d, want 0", got)
	}
	if n.Contains(5) {
		t.Fatal("nil Contains must be false")
	}
	if got := n.InOrder(); got != nil {
		t.Fatalf("nil InOrder = %v, want nil", got)
	}
}

func TestInsertOnNilCreatesRoot(t *testing.T) {
	t.Parallel()
	var n *node
	n = n.Insert(42)
	if n == nil {
		t.Fatal("Insert on nil must return a new node")
	}
	if n.Count() != 1 || !n.Contains(42) {
		t.Fatalf("new tree: count=%d contains42=%v", n.Count(), n.Contains(42))
	}
}

func TestBuiltTree(t *testing.T) {
	t.Parallel()
	var tr Tree
	for _, v := range []int{5, 3, 8, 1, 4, 7, 9} {
		tr.Insert(v)
	}

	if tr.Count() != 7 {
		t.Fatalf("Count = %d, want 7", tr.Count())
	}
	for _, v := range []int{1, 3, 4, 5, 7, 8, 9} {
		if !tr.Contains(v) {
			t.Fatalf("Contains(%d) = false, want true", v)
		}
	}
	for _, v := range []int{0, 2, 6, 10} {
		if tr.Contains(v) {
			t.Fatalf("Contains(%d) = true, want false", v)
		}
	}
	got := tr.InOrder()
	want := []int{1, 3, 4, 5, 7, 8, 9}
	if !slices.Equal(got, want) {
		t.Fatalf("InOrder = %v, want %v", got, want)
	}
}

func TestDuplicateInsertDoesNotGrow(t *testing.T) {
	t.Parallel()
	var tr Tree
	tr.Insert(5)
	tr.Insert(5)
	tr.Insert(5)
	if tr.Count() != 1 {
		t.Fatalf("Count = %d after duplicate inserts, want 1", tr.Count())
	}
}

func TestZeroTreeIsUsable(t *testing.T) {
	t.Parallel()
	var tr Tree // never initialized
	if tr.Count() != 0 || tr.Contains(1) {
		t.Fatal("zero Tree must behave as an empty tree without panicking")
	}
}
```

## Review

The tree is correct when every method on a nil `*node` returns the empty-tree answer
(`0`, `false`, `nil`) without panicking, when `Insert` on nil returns a fresh node,
and when a built tree reports the right count, membership, and sorted in-order
traversal. The nil-receiver tests are the ones that prove the technique: if any method
dereferenced a field before its `if n == nil` guard, calling it on an empty subtree
would panic — which is exactly what happens on a real tree the moment recursion reaches
a leaf's nil child.

The mistakes: forgetting the `if n == nil` guard and dereferencing, so the first leaf
descent panics; writing `Insert` with a void return and trying to mutate a nil
receiver in place (there is no node to mutate — `Insert` must *return* the new
subtree root); and scattering nil-checks at every call site instead of absorbing the
base case into the receiver, which is verbose and easy to get wrong. The `Tree`
wrapper exists so callers hold an addressable value whose zero form is already a valid
empty tree.

## Resources

- [A Tour of Go: Methods and pointer indirection (nil receivers)](https://go.dev/tour/methods/12) — a nil pointer is a valid, useful receiver you can guard against inside the method.
- [Go Specification: Method values and calls](https://go.dev/ref/spec#Calls) — how a method call passes the receiver as an argument.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — self-referential `*node` fields.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../06-pointer-receivers-and-interfaces/00-concepts.md](../06-pointer-receivers-and-interfaces/00-concepts.md)
