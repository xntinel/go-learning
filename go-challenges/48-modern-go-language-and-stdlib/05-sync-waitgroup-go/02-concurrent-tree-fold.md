# Exercise 2: Concurrent Tree Fold (Nested Go)

A flat slice is the easy case for fan-out; the more interesting one is a recursive
structure — a directory tree, a DOM, a dependency graph — where you do not know
the shape of the work up front. `SumTree` folds such a structure by having each
node's task spawn a task *per child on the same `WaitGroup`*, the nested-`Go`
pattern that turns a recursion into a concurrent traversal with a single top-level
`Wait`.

This module is fully self-contained. It begins with its own `go mod init`, defines
the `Node` type and `SumTree`, and ships its own demo and tests. Nothing here
imports any other exercise.

## What you'll build

```text
tree.go              type Node{Value, Children}; SumTree(*Node) int64
cmd/
  demo/
    main.go          sum a small tree concurrently and print the total
tree_test.go         a known-total tree, a nil tree, and a wide tree under -race
```

- Files: `tree.go`, `cmd/demo/main.go`, `tree_test.go`.
- Implement: `Node` (an integer value plus child pointers) and `SumTree(root *Node) int64`.
- Test: `tree_test.go` asserts a hand-summed tree, the `nil` tree returning `0`, and a wide tree summing correctly under `-race`.
- Verify: `go vet ./... && go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/48-modern-go-language-and-stdlib/05-sync-waitgroup-go/02-concurrent-tree-fold/cmd/demo && cd go-solutions/48-modern-go-language-and-stdlib/05-sync-waitgroup-go/02-concurrent-tree-fold
go mod edit -go=1.25
```

### Why nested Go works, and the deadlock it must avoid

The mechanism is a single rule from the `WaitGroup` documentation: a goroutine
started by `Go` may itself call `Go` on the same group, as long as the group is
non-empty at the time. It always is here — when a node's task calls `wg.Go` for a
child, that node's own task is still running and still counted, so the counter
never reaches zero between a parent spawning and its child being registered. This
is what lets `visit` recurse concurrently: `SumTree` seeds the group with one task
for the root, and from then on every visited node spawns one task per child on the
*same* `wg`. The single `wg.Wait()` at the top therefore unblocks only after the
last leaf's task has returned — the whole tree has been visited.

Two design choices make the fold both correct and safe. The total accumulates
through an `atomic.Int64`: many tasks call `total.Add` concurrently, and the
atomic is what makes those increments race-free without a mutex. Reading it with
`total.Load()` after `Wait` is safe by the same happens-before edge `Map` relies
on — every task's `Add` happens before its `Done`, which happens before `Wait`
returns — so the final `Load` observes every contribution.

The choice that is easy to get wrong is the *absence* of a semaphore. It is
tempting to bound this fan-out the way `Map` bounds its own, but combining nested
`Go` with a semaphore that a parent holds while waiting for its children is a
genuine deadlock, not a slowdown. Picture each task acquiring a slot on entry and
holding it until it (and its descendants) finish: with fewer slots than the
breadth of the tree, every slot ends up held by a parent blocked waiting for a
child, and no child can acquire the slot it needs because no parent will release
one first. That is the classic hold-and-wait cycle and it hangs the program.
`SumTree` sidesteps it structurally: `visit` adds its value, spawns its children's
tasks, and returns *immediately* — it never blocks holding a resource while a
child needs it — so the fan-out is unbounded but deadlock-free. If you truly need
to cap concurrency on a tree, you bound the *work itself* (a worker pool draining
a queue of nodes), not the recursion, precisely so a parent never waits while
holding a slot.

Create `tree.go`. Each node spawns a task per child on the *same* `WaitGroup` —
nested `Go` — and contributions accumulate through an atomic:

```go
package parallel

import (
	"sync"
	"sync/atomic"
)

// Node is a node in an integer tree.
type Node struct {
	Value    int
	Children []*Node
}

// SumTree sums every node's Value concurrently. Each node's task spawns one task
// per child on the same WaitGroup (a goroutine started by Go may itself call
// Go), and Wait returns only after the whole tree has been visited.
func SumTree(root *Node) int64 {
	var total atomic.Int64
	var wg sync.WaitGroup

	var visit func(n *Node)
	visit = func(n *Node) {
		if n == nil {
			return
		}
		total.Add(int64(n.Value))
		for _, child := range n.Children {
			wg.Go(func() { visit(child) })
		}
	}

	wg.Go(func() { visit(root) })
	wg.Wait()
	return total.Load()
}
```

The `nil` guard at the top of `visit` is what lets the root case and any `nil`
child pointer be handled by the same code path: `SumTree(nil)` spawns one task
that returns at once, `Wait` unblocks, and the zero-valued `total` is loaded as
`0`.

### The runnable demo

The demo sums a small hand-built tree so the total is checkable by eye, and a
`nil` tree to show the empty case.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/parallel"
)

func main() {
	// 5 + 3 + 1 + 4 + 2 + 6 = 21
	root := &parallel.Node{Value: 5, Children: []*parallel.Node{
		{Value: 3, Children: []*parallel.Node{{Value: 1}, {Value: 4}}},
		{Value: 2, Children: []*parallel.Node{{Value: 6}}},
	}}

	fmt.Println("sum =", parallel.SumTree(root))
	fmt.Println("empty =", parallel.SumTree(nil))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
sum = 21
empty = 0
```

### Tests

The tests cover the fold's contract and exercise the nested fan-out hard enough
for `-race` to mean something. The first sums a small tree whose total is computed
by hand and also asserts `SumTree(nil)` is `0`. The second builds a wide tree —
many children under the root, each with grandchildren — so that many tasks run the
`atomic.Add` at once; summing it to a value computed independently, under `-race`,
is what proves the nested `Go` and the atomic accumulation never collide.

Create `tree_test.go`:

```go
package parallel

import "testing"

func TestSumTree(t *testing.T) {
	t.Parallel()

	root := &Node{Value: 1, Children: []*Node{
		{Value: 2, Children: []*Node{{Value: 4}, {Value: 5}}},
		{Value: 3},
	}}
	if got := SumTree(root); got != 15 {
		t.Fatalf("SumTree = %d, want 15", got)
	}
	if got := SumTree(nil); got != 0 {
		t.Fatalf("SumTree(nil) = %d, want 0", got)
	}
}

func TestSumTreeWide(t *testing.T) {
	t.Parallel()

	// A wide, two-level tree: root + 50 children, each with 10 grandchildren,
	// every node carrying Value 1. Total = 1 + 50 + 50*10 = 551.
	const (
		children      = 50
		grandchildren = 10
	)
	root := &Node{Value: 1}
	for i := 0; i < children; i++ {
		c := &Node{Value: 1}
		for j := 0; j < grandchildren; j++ {
			c.Children = append(c.Children, &Node{Value: 1})
		}
		root.Children = append(root.Children, c)
	}

	want := int64(1 + children + children*grandchildren)
	if got := SumTree(root); got != want {
		t.Fatalf("SumTree(wide) = %d, want %d", got, want)
	}
}
```

## Review

`SumTree` is correct when the nested fan-out and the accumulation agree. The
fan-out is sound because nested `Go` is registered while the parent task is still
counted, so the group is never spuriously empty and the single `Wait` covers the
entire tree; the wide-tree test, which fans out hundreds of tasks, is the check
that nothing is dropped. The accumulation is sound because every contribution goes
through `atomic.Int64.Add` and is read with `Load` only after `Wait`, so the
final total reflects all of them — and `-race` confirms the concurrent `Add`s do
not collide.

Common mistakes for this feature. The first is trying to bound the recursion with
a semaphore a parent holds while waiting for its children, which deadlocks by
hold-and-wait — the fan-out here is intentionally unbounded for that reason, and a
real bound belongs on a worker pool draining a node queue, not on the recursion.
The second is accumulating into a plain `int64` instead of an `atomic.Int64`; the
concurrent increments then race and `-race` flags it immediately. The third is
reading the total without the `Wait` barrier — load it before `Wait` returns and
you observe a partial sum, because the happens-before edge that publishes every
`Add` is exactly `Wait`.

## Resources

- [`sync.WaitGroup.Go`](https://pkg.go.dev/sync#WaitGroup.Go) — the method whose nested use drives the traversal; the docs note a `Go`-started goroutine may itself call `Go`.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` and its `Add`/`Load`, the lock-free accumulator the fold uses.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before guarantee that makes reading `total` after `Wait` see every concurrent `Add`.

---

Back to [01-bounded-parallel-map.md](01-bounded-parallel-map.md) | Next: [00-concepts.md](00-concepts.md)
