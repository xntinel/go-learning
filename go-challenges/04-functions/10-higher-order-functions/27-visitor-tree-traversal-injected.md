# Exercise 27: Tree Traversal with Injected Visitor Callback

**Nivel: Intermedio** — validacion rapida (un test corto).

A generic tree walk only needs to know one thing about its nodes: their
children. Everything else — what to do at each node, whether to descend
into a subtree, whether to stop early — belongs to the caller, not the
walker. `Walk` is the fixed algorithm; `Visitor` is the injected strategy
that decides, node by node, how the traversal proceeds.

## What you'll build

```text
treevisit/                   independent module: example.com/treevisit
  go.mod                     go 1.24
  treevisit.go                type Node, Action, Visitor; func Walk
  treevisit_test.go            full traversal, skip-children, stop, nil root
  cmd/demo/
    main.go                  walks a tree demonstrating prune and early stop
```

- Files: `treevisit.go`, `treevisit_test.go`, `cmd/demo/main.go`.
- Implement: `Node struct{ Value int; Children []*Node }`, `Action` (`Continue`, `SkipChildren`, `Stop`), `Visitor func(n *Node) Action`, and `Walk(root *Node, visit Visitor) (stopped bool)`.
- Test: a visitor that always continues visits every node in pre-order; `SkipChildren` prunes one subtree but the walk still visits that node's siblings; `Stop` halts the entire traversal immediately, skipping everything not yet visited; a nil root visits nothing and never calls the visitor.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Three actions instead of a boolean

A visitor callback that returns just `bool` can express "stop everything"
or "keep going," but not "skip only this subtree" — a real need for
filtering hidden directories out of a file-tree walk, or pruning inactive
branches out of an org-chart traversal, without aborting the whole walk.
`Action` gives the visitor three distinct answers instead of forcing that
distinction into a side channel: `Continue` descends into the node's
children before moving to its next sibling, `SkipChildren` skips the
descent but still lets siblings run, and `Stop` propagates all the way up
through every recursive call and ends the traversal on the spot. `walk`'s
recursion is what makes `Stop` propagate correctly — each recursive call
checks whether its child call returned `stopped == true` and, if so,
returns `true` immediately itself without visiting any further siblings.

Create `treevisit.go`:

```go
package treevisit

// Node is a node in an arbitrary tree: any number of children, visited in
// slice order.
type Node struct {
	Value    int
	Children []*Node
}

// Action is what a Visitor decides to do after looking at a node.
type Action int

const (
	// Continue descends into the node's children, then moves on.
	Continue Action = iota
	// SkipChildren does not descend into this node's children, but the
	// walk continues with the node's remaining siblings.
	SkipChildren
	// Stop halts the entire traversal immediately, unwinding without
	// visiting anything else.
	Stop
)

// Visitor is called once per node, in pre-order, and decides how the
// traversal proceeds from there.
type Visitor func(n *Node) Action

// Walk traverses root pre-order, calling visit at each node. It reports
// whether the traversal was halted early by a Stop action. A nil root
// visits nothing and reports false.
func Walk(root *Node, visit Visitor) (stopped bool) {
	if root == nil {
		return false
	}
	return walk(root, visit)
}

func walk(n *Node, visit Visitor) (stopped bool) {
	switch visit(n) {
	case Stop:
		return true
	case SkipChildren:
		return false
	default: // Continue
		for _, child := range n.Children {
			if walk(child, visit) {
				return true
			}
		}
		return false
	}
}
```

### The runnable demo

The demo tree has one node whose children are pruned (`SkipChildren`) and
one node that halts the whole walk (`Stop`) before its own sibling would
otherwise be visited.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/treevisit"
)

func main() {
	// root
	// ├── 10 (its children are pruned)
	// │   └── 11
	// ├── 999 (halts the whole walk)
	// └── 20
	//     └── 21
	root := &treevisit.Node{
		Value: 1,
		Children: []*treevisit.Node{
			{Value: 10, Children: []*treevisit.Node{{Value: 11}}},
			{Value: 999},
			{Value: 20, Children: []*treevisit.Node{{Value: 21}}},
		},
	}

	visit := func(n *treevisit.Node) treevisit.Action {
		fmt.Printf("visiting %d\n", n.Value)
		switch {
		case n.Value == 999:
			return treevisit.Stop
		case n.Value == 10:
			fmt.Println("  pruning its children")
			return treevisit.SkipChildren
		default:
			return treevisit.Continue
		}
	}

	stopped := treevisit.Walk(root, visit)
	fmt.Printf("stopped=%v\n", stopped)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
visiting 1
visiting 10
  pruning its children
visiting 999
stopped=true
```

Node `11` (child of `10`) is never visited because of the prune, and node
`20` (and its child `21`) are never visited because `999` stops the walk
before the traversal ever reaches them.

### Tests

`TestWalkFullTraversalVisitsAllNodesPreOrder` establishes the baseline
order every other test's assertions rely on. `TestWalkSkipChildrenPrunesSubtreeButContinuesSiblings`
is the case that distinguishes this from a boolean callback: the pruned
node's child is skipped, but its sibling is still visited afterward.
`TestWalkStopHaltsEntireTraversal` proves the halt actually propagates
through the recursion instead of only skipping the current node's own
children. `TestWalkNilRootVisitsNothing` is the guard clause: `visit`
must never be called at all for a nil tree.

Create `treevisit_test.go`:

```go
package treevisit

import (
	"reflect"
	"testing"
)

func TestWalkFullTraversalVisitsAllNodesPreOrder(t *testing.T) {
	t.Parallel()

	root := &Node{
		Value: 1,
		Children: []*Node{
			{Value: 2, Children: []*Node{{Value: 4}}},
			{Value: 3},
		},
	}

	var visited []int
	stopped := Walk(root, func(n *Node) Action {
		visited = append(visited, n.Value)
		return Continue
	})

	if stopped {
		t.Fatal("Walk() reported stopped=true, want false")
	}
	want := []int{1, 2, 4, 3}
	if !reflect.DeepEqual(visited, want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
}

func TestWalkSkipChildrenPrunesSubtreeButContinuesSiblings(t *testing.T) {
	t.Parallel()

	root := &Node{
		Value: 1,
		Children: []*Node{
			{Value: 2, Children: []*Node{{Value: 20}}},
			{Value: 3},
		},
	}

	var visited []int
	stopped := Walk(root, func(n *Node) Action {
		visited = append(visited, n.Value)
		if n.Value == 2 {
			return SkipChildren
		}
		return Continue
	})

	if stopped {
		t.Fatal("Walk() reported stopped=true, want false")
	}
	want := []int{1, 2, 3} // 20 is pruned, 3 (2's sibling) is still visited
	if !reflect.DeepEqual(visited, want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
}

func TestWalkStopHaltsEntireTraversal(t *testing.T) {
	t.Parallel()

	root := &Node{
		Value: 1,
		Children: []*Node{
			{Value: 2},
			{Value: 3, Children: []*Node{{Value: 30}}},
		},
	}

	var visited []int
	stopped := Walk(root, func(n *Node) Action {
		visited = append(visited, n.Value)
		if n.Value == 2 {
			return Stop
		}
		return Continue
	})

	if !stopped {
		t.Fatal("Walk() reported stopped=false, want true")
	}
	want := []int{1, 2} // 3 and 30 must never be visited
	if !reflect.DeepEqual(visited, want) {
		t.Fatalf("visited = %v, want %v", visited, want)
	}
}

func TestWalkNilRootVisitsNothing(t *testing.T) {
	t.Parallel()

	called := false
	stopped := Walk(nil, func(n *Node) Action {
		called = true
		return Continue
	})

	if stopped {
		t.Fatal("Walk(nil, ...) reported stopped=true, want false")
	}
	if called {
		t.Fatal("visit was called on a nil root")
	}
}
```

## Review

`Walk` is correct because `walk`'s recursive case does not just call
itself on every child — it checks each child call's returned `stopped`
and, on the first `true`, returns immediately without visiting any
remaining children or letting the caller's loop continue. Miss that check
and `Stop` degrades into "stop visiting this node's remaining children,"
a completely different and much weaker guarantee than "stop the entire
traversal." The nil-root guard belongs in `Walk`, not `walk`, precisely so
`walk`'s recursive calls never have to re-check for nil children — the
loop already only calls `walk` on `Children` entries, which are never nil
by construction of a well-formed tree in these tests.

## Resources

- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the `Visitor` strategy shape.
- [filepath.WalkDir](https://pkg.go.dev/path/filepath#WalkDir) — a standard-library tree walk with the same injected-callback-decides-how-to-proceed shape, including its own skip signal (`filepath.SkipDir`).
- [go/ast.Inspect](https://pkg.go.dev/go/ast#Inspect) — another stdlib visitor-callback traversal, over Go syntax trees instead of a generic `Node`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-request-dedup-time-window-singleflight.md](26-request-dedup-time-window-singleflight.md) | Next: [28-capacity-limiter-with-backpressure.md](28-capacity-limiter-with-backpressure.md)
