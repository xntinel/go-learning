# Exercise 8: Generic Fold over a Category/Org Hierarchy

A reusable generic tree and a `Fold` that recursively aggregates a hierarchy —
summing headcount over an org chart, counting nodes, or taking a max over a nested
category tree. This separates traversal (walk the tree once) from aggregation (what
to accumulate), so one recursive walker serves every rollup a backend needs.

This module is fully self-contained: its own `go mod init`, the generic tree and
folds inline, its own demo and tests.

## What you'll build

```text
treefold/                  independent module: example.com/treefold
  go.mod                   go 1.26
  tree.go                  Node[T]; Fold[T, R]; Max[T cmp.Ordered]
  tree_test.go             sum, count, max, empty, single, visit-once
  cmd/
    demo/
      main.go              an org chart: total headcount and deepest report count
```

- Files: `tree.go`, `cmd/demo/main.go`, `tree_test.go`.
- Implement: a generic `Node[T any]` with `Value` and `Children`, a `Fold[T, R any](n *Node[T], init R, combine func(R, T) R) R` that recurses over the tree, and a `Max[T cmp.Ordered](n *Node[T]) T` built on `Fold`.
- Test: fold sum, count, and max over a fixed hierarchy with known totals; empty tree returns `init`; single node returns `combine(init, value)`; a counting combine proves each node is visited once.
- Verify: `go test -count=1 -race ./...`

### Separating traversal from aggregation with generics

An org chart, a category tree, a nested budget — they are all the same shape: a
node carrying a value, with a slice of child nodes. The rollups differ (sum
headcount, count nodes, find the deepest budget line) but the *traversal* is
identical every time: visit a node, then recurse into its children. Writing that
recursion once and parameterizing what it accumulates is what `Fold` does.

`Fold[T, R any]` takes the tree, an initial accumulator `init` of type `R`, and a
`combine func(R, T) R` that folds one node's value into the accumulator. Its body
is three lines: return `init` for a nil node (the base case), combine this node's
value into the accumulator, then fold each child in turn, threading the accumulator
through. The two type parameters are the point: `T` is the value type stored in the
tree, `R` is the accumulator type, and they need not be the same — a fold that
counts nodes has `R = int` regardless of `T`, and a fold that concatenates names
has `R = string`. This is the generic version of the classic list `reduce`,
generalized to a tree.

Because `combine` is supplied by the caller, one walker serves every aggregation.
Summing headcount is `Fold(root, 0, func(acc, n int) int { return acc + n })`.
Counting nodes is `Fold(root, 0, func(acc int, _ Emp) int { return acc + 1 })` —
note the value is ignored, so the count is independent of `T`. `Max` is a thin
wrapper that constrains `T` to `cmp.Ordered` and folds with a max-picking combine;
it starts from the zero value, which is correct for the non-negative quantities
(headcount, counts, sizes) these hierarchies hold — a caveat worth stating because
a tree of negative values would need the first element as the seed instead.

The recursion here is over data you own and whose depth is trusted (an org chart is
a handful of levels), so plain recursion is exactly right — no depth guard needed,
which is the contrast with the untrusted-input exercises.

Create `tree.go`:

```go
package tree

import "cmp"

// Node is a value with zero or more children — an org chart, category tree, etc.
type Node[T any] struct {
	Value    T
	Children []*Node[T]
}

// Fold aggregates the tree in a single recursive walk: it combines each node's
// value into an accumulator threaded through the whole traversal. R (the
// accumulator) is independent of T (the node value), so one Fold serves sum,
// count, concat, and more. A nil node returns init unchanged (the base case).
func Fold[T, R any](n *Node[T], init R, combine func(R, T) R) R {
	if n == nil {
		return init
	}
	acc := combine(init, n.Value)
	for _, child := range n.Children {
		acc = Fold(child, acc, combine)
	}
	return acc
}

// Max returns the largest value in the tree, or the zero value for an empty tree.
// It is a Fold whose accumulator picks the larger of the running max and each
// value; correct for non-negative quantities seeded from the zero value.
func Max[T cmp.Ordered](n *Node[T]) T {
	var zero T
	return Fold(n, zero, func(acc, v T) T {
		if v > acc {
			return v
		}
		return acc
	})
}
```

### The runnable demo

The demo builds a small org chart and reports total headcount (sum fold), the
number of positions (count fold), and the largest single team (max fold).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/treefold"
)

func main() {
	// Each node's value is the direct headcount at that position.
	org := &tree.Node[int]{
		Value: 1, // CEO
		Children: []*tree.Node[int]{
			{Value: 8, Children: []*tree.Node[int]{
				{Value: 20},
				{Value: 15},
			}},
			{Value: 5, Children: []*tree.Node[int]{
				{Value: 12},
			}},
		},
	}

	total := tree.Fold(org, 0, func(acc, headcount int) int { return acc + headcount })
	positions := tree.Fold(org, 0, func(acc, _ int) int { return acc + 1 })
	largest := tree.Max(org)

	fmt.Printf("total headcount: %d\n", total)
	fmt.Printf("positions: %d\n", positions)
	fmt.Printf("largest single team: %d\n", largest)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total headcount: 61
positions: 6
largest single team: 20
```

### Tests

`TestFoldSum` and `TestFoldCount` fold a fixed tree with known totals.
`TestMax` uses the `cmp.Ordered` fold on a skewed tree. `TestEmptyTree` confirms a
nil tree returns `init`. `TestSingleNode` confirms a leaf returns
`combine(init, value)`. `TestVisitsEachNodeOnce` uses a counting combine to prove
the walk touches every node exactly once.

Create `tree_test.go`:

```go
package tree

import (
	"fmt"
	"testing"
)

func sampleTree() *Node[int] {
	return &Node[int]{
		Value: 1,
		Children: []*Node[int]{
			{Value: 2, Children: []*Node[int]{
				{Value: 4},
				{Value: 5},
			}},
			{Value: 3},
		},
	}
}

func TestFoldSum(t *testing.T) {
	t.Parallel()

	got := Fold(sampleTree(), 0, func(acc, v int) int { return acc + v })
	if got != 15 {
		t.Fatalf("sum = %d, want 15 (1+2+3+4+5)", got)
	}
}

func TestFoldCount(t *testing.T) {
	t.Parallel()

	got := Fold(sampleTree(), 0, func(acc int, _ int) int { return acc + 1 })
	if got != 5 {
		t.Fatalf("count = %d, want 5", got)
	}
}

func TestMax(t *testing.T) {
	t.Parallel()

	// A left-skewed tree so the max is not at the root or a shallow node.
	skewed := &Node[int]{Value: 3, Children: []*Node[int]{
		{Value: 7, Children: []*Node[int]{
			{Value: 42, Children: []*Node[int]{
				{Value: 9},
			}},
		}},
	}}
	if got := Max(skewed); got != 42 {
		t.Fatalf("Max = %d, want 42", got)
	}
}

func TestEmptyTree(t *testing.T) {
	t.Parallel()

	got := Fold[int, int](nil, 99, func(acc, v int) int { return acc + v })
	if got != 99 {
		t.Fatalf("empty fold = %d, want init 99", got)
	}
	if m := Max[int](nil); m != 0 {
		t.Fatalf("Max(nil) = %d, want 0", m)
	}
}

func TestSingleNode(t *testing.T) {
	t.Parallel()

	leaf := &Node[int]{Value: 7}
	got := Fold(leaf, 10, func(acc, v int) int { return acc + v })
	if got != 17 {
		t.Fatalf("single-node fold = %d, want 17 (10+7)", got)
	}
}

func TestVisitsEachNodeOnce(t *testing.T) {
	t.Parallel()

	visited := make(map[int]int)
	count := Fold(sampleTree(), 0, func(acc, v int) int {
		visited[v]++
		return acc + 1
	})
	if count != 5 {
		t.Fatalf("count = %d, want 5", count)
	}
	for v, times := range visited {
		if times != 1 {
			t.Fatalf("value %d visited %d times, want 1", v, times)
		}
	}
}

func Example() {
	root := &Node[string]{
		Value: "root",
		Children: []*Node[string]{
			{Value: "child"},
		},
	}
	names := Fold(root, "", func(acc, v string) string {
		if acc == "" {
			return v
		}
		return acc + "," + v
	})
	fmt.Println(names)
	// Output: root,child
}
```

## Review

`Fold` is correct when it visits every node exactly once and threads the
accumulator through in a well-defined order — `TestVisitsEachNodeOnce` proves the
visit-once property with a counting combine, and the sum/count tests prove the
aggregation against known totals. The design win is that `R` is independent of `T`:
the same recursive walker computes an `int` sum, an `int` count, and a `string`
join without change, which is what "separate traversal from aggregation" buys you.
`Max` shows the `cmp.Ordered` constraint in action; its zero-value seed is correct
for the non-negative quantities these hierarchies hold, and the review-worthy
caveat is that a tree of negative values would need a first-element seed instead.
The recursion needs no depth guard because an org or category tree is data you own
with trusted, shallow depth — the deliberate contrast with the untrusted-input
walkers earlier in the lesson.

## Resources

- [cmp package (cmp.Ordered)](https://pkg.go.dev/cmp)
- [Go Specification: Type parameters](https://go.dev/ref/spec#Type_parameter_declarations)
- [Tutorial: Generics](https://go.dev/doc/tutorial/generics)
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-memoized-transitive-dependencies.md](07-memoized-transitive-dependencies.md) | Next: [09-no-tco-accumulator-vs-loop.md](09-no-tco-accumulator-vs-loop.md)
