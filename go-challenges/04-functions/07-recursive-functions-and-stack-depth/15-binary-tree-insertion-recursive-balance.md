# Exercise 15: Recursive BST Insertion with Self-Balancing

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A binary search tree used as an in-memory index only stays fast if it stays
shallow. Insert keys in sorted order into a plain BST and every insertion
degenerates it into a linked list — lookups go from O(log n) to O(n). An
AVL tree fixes this by rebalancing after every insertion: the recursive
`Insert` call unwinds back up the path it just walked down, and at each
ancestor it checks whether that node's subtree tipped over and, if so,
rotates it back upright before returning.

This module is fully self-contained: its own `go mod init`, the tree inline,
its own demo and tests.

## What you'll build

```text
avltree/                    independent module: example.com/avltree
  go.mod                     go 1.24
  avltree.go                 type Node; func Insert; func InOrder; func Height
  avltree_test.go            BST order, duplicates, balance bound, log-height, full-tree check
  cmd/
    demo/
      main.go                inserts 1..7 in sorted order, prints the balanced result
```

- Files: `avltree.go`, `cmd/demo/main.go`, `avltree_test.go`.
- Implement: `type Node struct { Key int; Left, Right *Node; Height int }` and
  `func Insert(root *Node, key int) *Node` that inserts recursively and
  rebalances every ancestor on the way back out.
- Test: in-order traversal stays sorted after mixed inserts; a duplicate key
  is a no-op; the balance factor at every node stays in `[-1, 1]`; 1000
  ascending inserts (the worst case for a plain BST) keep the tree at
  logarithmic height; a full-tree walk confirms every node, not just the
  root, is balanced.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/avltree/cmd/demo
cd ~/go-exercises/avltree
go mod init example.com/avltree
go mod edit -go=1.24
```

### Why the rebalancing happens on the way *out* of the recursion

`Insert` walks down to find the right spot for a new key exactly like a
plain BST insert: compare, recurse left or right, until it hits a `nil`
child and creates a new leaf there. The difference is what happens next.
Each recursive call is a stack frame holding one ancestor of the new node,
and when the base case returns, those frames unwind one at a time, each one
resuming exactly where it left off — with `root` still bound to that
ancestor. That is the only property this exercise depends on: the unwind
gives you, for free, a walk back up the insertion path in reverse order,
without needing parent pointers or a second pass.

At each frame on the way back out, `updateHeight` recomputes that node's
height from its (already-correct, because they were fixed first) children,
and `rebalance` checks the resulting balance factor. If a subtree tipped by
more than one, a rotation restores it — and because children are fixed
before their parent is even examined, the rotation at any given node can
assume both of its children are already valid AVL subtrees. Fix the leaf's
immediate parent first, then its grandparent, then the root: never the
other order. That bottom-up guarantee is what recursion's call-stack unwind
gives you automatically; get the order backward and a rotation could act on
a subtree that is not yet balanced, and the invariant breaks.

Create `avltree.go`:

```go
package avltree

// Node is one node of an AVL-balanced binary search tree. Height is the
// height of the subtree rooted at this node (a leaf has height 1), kept
// current by every recursive Insert so balance factors can be computed in
// O(1) instead of re-walking the subtree.
type Node struct {
	Key         int
	Left, Right *Node
	Height      int
}

// Insert returns the root of the tree that results from inserting key into
// the tree rooted at root, rebalancing every ancestor of the insertion point
// so the AVL invariant (balance factor in [-1, 1] at every node) holds
// afterward. Duplicate keys are ignored: the tree is unchanged.
func Insert(root *Node, key int) *Node {
	if root == nil {
		return &Node{Key: key, Height: 1}
	}
	switch {
	case key < root.Key:
		root.Left = Insert(root.Left, key)
	case key > root.Key:
		root.Right = Insert(root.Right, key)
	default:
		return root
	}

	updateHeight(root)
	return rebalance(root)
}

// rebalance inspects root's balance factor and applies the one rotation (or
// rotation pair) that restores the AVL invariant, assuming both children were
// already balanced before this call — true because Insert rebalances
// bottom-up, one ancestor at a time, on its way back out of the recursion.
func rebalance(root *Node) *Node {
	balance := balanceFactor(root)

	if balance > 1 {
		if balanceFactor(root.Left) < 0 {
			root.Left = rotateLeft(root.Left)
		}
		return rotateRight(root)
	}
	if balance < -1 {
		if balanceFactor(root.Right) > 0 {
			root.Right = rotateRight(root.Right)
		}
		return rotateLeft(root)
	}
	return root
}

func rotateRight(y *Node) *Node {
	x := y.Left
	t2 := x.Right

	x.Right = y
	y.Left = t2

	updateHeight(y)
	updateHeight(x)
	return x
}

func rotateLeft(x *Node) *Node {
	y := x.Right
	t2 := y.Left

	y.Left = x
	x.Right = t2

	updateHeight(x)
	updateHeight(y)
	return y
}

func height(n *Node) int {
	if n == nil {
		return 0
	}
	return n.Height
}

func updateHeight(n *Node) {
	l, r := height(n.Left), height(n.Right)
	if l > r {
		n.Height = l + 1
	} else {
		n.Height = r + 1
	}
}

// balanceFactor is the height of the left subtree minus the height of the
// right subtree. A correctly balanced AVL node keeps this in [-1, 1].
func balanceFactor(n *Node) int {
	if n == nil {
		return 0
	}
	return height(n.Left) - height(n.Right)
}

// InOrder returns the tree's keys in sorted order, the standard way to check
// the BST property survived every rotation.
func InOrder(root *Node) []int {
	if root == nil {
		return nil
	}
	var out []int
	out = append(out, InOrder(root.Left)...)
	out = append(out, root.Key)
	out = append(out, InOrder(root.Right)...)
	return out
}

// Height reports the height of the tree rooted at root (0 for an empty tree).
func Height(root *Node) int {
	return height(root)
}
```

### The runnable demo

The demo inserts `1..7` in ascending order — the input that turns a plain
BST into a 7-deep linked list — and shows the AVL tree stays flat instead.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/avltree"
)

func main() {
	var root *avltree.Node
	for _, key := range []int{1, 2, 3, 4, 5, 6, 7} {
		root = avltree.Insert(root, key)
	}

	fmt.Printf("in-order: %v\n", avltree.InOrder(root))
	fmt.Printf("height: %d\n", avltree.Height(root))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-order: [1 2 3 4 5 6 7]
height: 3
```

### Tests

`TestInsertMaintainsBSTOrder` inserts a mixed sequence and checks the
in-order traversal is sorted. `TestInsertDuplicateIsNoOp` confirms
re-inserting an existing key changes nothing. `TestInsertKeepsBalanceFactorBounded`
inserts 1000 keys one at a time and checks the root's balance factor after
every single insert, not just at the end. `TestInsertAscendingSequenceStaysLogHeight`
is the point of the exercise: 1000 ascending inserts — the adversarial case
for a plain BST — must keep the tree at logarithmic height, not linear.
`TestInsertAllAncestorsBalanced` walks the entire tree, not just the root,
confirming rebalancing reaches every node on the path, not only the one
closest to the insertion.

Create `avltree_test.go`:

```go
package avltree

import (
	"reflect"
	"testing"
)

func TestInsertMaintainsBSTOrder(t *testing.T) {
	t.Parallel()

	var root *Node
	for _, key := range []int{5, 3, 8, 1, 4, 7, 9, 2, 6, 0} {
		root = Insert(root, key)
	}

	got := InOrder(root)
	want := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("InOrder = %v, want %v", got, want)
	}
}

func TestInsertDuplicateIsNoOp(t *testing.T) {
	t.Parallel()

	var root *Node
	for _, key := range []int{1, 2, 3} {
		root = Insert(root, key)
	}
	before := InOrder(root)

	root = Insert(root, 2)
	after := InOrder(root)

	if !reflect.DeepEqual(before, after) {
		t.Fatalf("duplicate insert changed tree: before %v, after %v", before, after)
	}
}

func TestInsertKeepsBalanceFactorBounded(t *testing.T) {
	t.Parallel()

	var root *Node
	for key := 0; key < 1000; key++ {
		root = Insert(root, key)
		if bf := balanceFactor(root); bf < -1 || bf > 1 {
			t.Fatalf("after inserting %d, root balance factor = %d, want in [-1,1]", key, bf)
		}
	}
}

func TestInsertAscendingSequenceStaysLogHeight(t *testing.T) {
	t.Parallel()

	// Inserting 0..999 in ascending order into a plain BST produces a
	// 1000-deep chain. An AVL tree must keep the height close to log2(1000).
	var root *Node
	for key := 0; key < 1000; key++ {
		root = Insert(root, key)
	}

	h := Height(root)
	if h > 15 {
		t.Fatalf("height = %d after 1000 ascending inserts, want <= 15 (log2 bound)", h)
	}
}

func TestInsertAllAncestorsBalanced(t *testing.T) {
	t.Parallel()

	var root *Node
	for _, key := range []int{10, 20, 30, 40, 50, 25} {
		root = Insert(root, key)
	}

	var check func(n *Node)
	check = func(n *Node) {
		if n == nil {
			return
		}
		if bf := balanceFactor(n); bf < -1 || bf > 1 {
			t.Fatalf("node %d has balance factor %d, want in [-1,1]", n.Key, bf)
		}
		check(n.Left)
		check(n.Right)
	}
	check(root)
}
```

Run it: `go test -count=1 ./...`

## Review

`Insert` is correct when it keeps two things true after every call: the
result is still a valid BST (`InOrder` is sorted) and every node's balance
factor is in `[-1, 1]` (checked at the root continuously, and at every node
in the final tree). `TestInsertAscendingSequenceStaysLogHeight` is the test
that would fail hardest on a plain, unbalanced BST — it is the entire reason
this exercise exists. The mistake this targets is rebalancing only the
insertion point rather than every ancestor on the path back to the root: a
rotation two levels up can still be needed even when the immediate parent
looks fine, and skipping it leaves a subtree silently out of the `[-1, 1]`
bound, which only a full-tree walk like `TestInsertAllAncestorsBalanced`
would catch.

## Resources

- [Go Specification: Recursive types](https://go.dev/ref/spec#Type_declarations)
- [AVL tree (rotations and balance factor)](https://en.wikipedia.org/wiki/AVL_tree)
- [reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-permission-inheritance-ancestor-walk.md](14-permission-inheritance-ancestor-walk.md) | Next: [16-xml-element-streaming-depth-validation.md](16-xml-element-streaming-depth-validation.md)
