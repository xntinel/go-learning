# Exercise 6: Order Migrations with Postorder Recursive Topological Sort

A topological sort over a dependency graph, computed with recursive DFS that emits
each node in postorder and reverses the result, producing a valid apply order — or
an error if the graph has a cycle. This is exactly how you compute the order to
apply database migrations or bootstrap modules given a "must-run-before" map.

This module is fully self-contained: its own `go mod init`, the sort inline, its
own demo and tests.

## What you'll build

```text
toposort/                  independent module: example.com/toposort
  go.mod                   go 1.26
  migrate.go               TopoSort(map[string][]string) ([]string, error); ErrCycle
  migrate_test.go          linear-extension validity, cycle error, edge cases
  cmd/
    demo/
      main.go              order a small migration graph
```

- Files: `migrate.go`, `cmd/demo/main.go`, `migrate_test.go`.
- Implement: `TopoSort(deps map[string][]string) ([]string, error)` where `deps[u]` lists nodes that must be applied after `u` (edges `u -> v`), running recursive DFS that appends in postorder and reversing; a cycle returns `ErrCycle`.
- Test: assert every emitted order is a valid linear extension (for each edge `u -> v`, `u` precedes `v`), verified programmatically; a cycle returns an error; single-node and empty-graph cases; determinism via sorted traversal.
- Verify: `go test -count=1 -race ./...`

### The map convention and postorder-plus-reverse

Fix the edge direction first, because everything depends on it. Here
`deps[u] = [v, w]` means "`u` must be applied before `v` and `w`" — an edge `u -> v`
reads "`u` comes first". In migration terms, `deps["create_users"] =
["add_email_index"]` says the table must exist before the index that references it.

The algorithm is depth-first search that appends a node to the result *after*
visiting everything reachable from it (postorder), then reverses the whole list.
Why this works: in a DAG, a postorder finishes a node only once all its successors
have finished, so a successor is appended before its predecessor. That yields the
successors-first order; reversing it puts each node before its successors, which is
precisely the apply order where `u` precedes every `v` in `deps[u]`. The reverse is
the standard final step of DFS-based topological sort, and skipping it silently
produces the exact backwards order.

Two robustness details. First, the traversal must start from every node, including
nodes that appear only as a value (a leaf migration nobody depends on further), so
the sort collects the full node set — keys plus all values — before iterating.
Second, a cycle makes a valid order impossible, so the DFS carries the same
three-color state as the previous exercise: re-entering a gray (on-stack) node
means a back edge, and the sort returns `ErrCycle` instead of a meaningless order.
A migration graph with a cycle is a configuration bug you want surfaced at startup,
not a deadlock discovered in production.

Determinism comes from sorting the node set and each neighbor list, so the emitted
order is one fixed, canonical linear extension rather than a random valid one —
which lets tests pin an exact order in addition to checking the general
edge-ordering property.

Create `migrate.go`:

```go
package migrate

import (
	"errors"
	"fmt"
	"slices"
	"sort"
)

// ErrCycle is returned when the dependency graph cannot be linearized.
var ErrCycle = errors.New("dependency cycle")

const (
	white = iota
	gray
	black
)

// TopoSort returns an apply order in which every node precedes the nodes listed
// in its deps entry (deps[u] are nodes that must run after u). It runs postorder
// DFS and reverses the result. A cycle returns ErrCycle.
func TopoSort(deps map[string][]string) ([]string, error) {
	color := make(map[string]int)
	var order []string

	var dfs func(node string) error
	dfs = func(node string) error {
		color[node] = gray
		successors := append([]string(nil), deps[node]...)
		sort.Strings(successors)
		for _, next := range successors {
			switch color[next] {
			case gray:
				return fmt.Errorf("%s -> %s: %w", node, next, ErrCycle)
			case white:
				if err := dfs(next); err != nil {
					return err
				}
			}
		}
		color[node] = black
		order = append(order, node)
		return nil
	}

	for _, node := range allNodes(deps) {
		if color[node] == white {
			if err := dfs(node); err != nil {
				return nil, err
			}
		}
	}
	slices.Reverse(order)
	return order, nil
}

// allNodes returns every node in the graph (keys and values), sorted and deduped.
func allNodes(deps map[string][]string) []string {
	set := make(map[string]struct{})
	for node, succ := range deps {
		set[node] = struct{}{}
		for _, s := range succ {
			set[s] = struct{}{}
		}
	}
	nodes := make([]string, 0, len(set))
	for n := range set {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return nodes
}
```

### The runnable demo

The demo orders a small migration graph where the users table must precede its
index and the sessions table that references it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/toposort"
)

func main() {
	deps := map[string][]string{
		"create_users":    {"add_email_index", "create_sessions"},
		"add_email_index": {},
		"create_sessions": {"create_orders"},
		"create_orders":   {},
	}

	order, err := migrate.TopoSort(deps)
	if err != nil {
		panic(err)
	}
	for i, m := range order {
		fmt.Printf("%d. %s\n", i+1, m)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1. create_users
2. create_sessions
3. create_orders
4. add_email_index
```

### Tests

`TestValidLinearExtension` is the key test: rather than hardcoding one permutation,
it checks the general property that for every edge `u -> v` in the graph, `u`
appears before `v` in the output. `TestDeterministicOrder` pins the exact
canonical order the sorted traversal produces. `TestCycleReturnsError` confirms a
cyclic graph returns `ErrCycle`. `TestEdgeCases` covers a single node and an empty
graph.

Create `migrate_test.go`:

```go
package migrate

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func positions(order []string) map[string]int {
	pos := make(map[string]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	return pos
}

func TestValidLinearExtension(t *testing.T) {
	t.Parallel()

	deps := map[string][]string{
		"a": {"b", "c"},
		"b": {"d"},
		"c": {"d"},
		"d": {"e"},
		"e": {},
	}
	order, err := TopoSort(deps)
	if err != nil {
		t.Fatal(err)
	}
	pos := positions(order)
	if len(order) != 5 {
		t.Fatalf("order = %v, want 5 nodes", order)
	}
	for u, succ := range deps {
		for _, v := range succ {
			if pos[u] >= pos[v] {
				t.Fatalf("edge %s -> %s violated: %s at %d, %s at %d (order %v)",
					u, v, u, pos[u], v, pos[v], order)
			}
		}
	}
}

func TestDeterministicOrder(t *testing.T) {
	t.Parallel()

	deps := map[string][]string{
		"create_users":    {"add_index", "create_sessions"},
		"add_index":       {},
		"create_sessions": {},
	}
	order, err := TopoSort(deps)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"create_users", "create_sessions", "add_index"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestCycleReturnsError(t *testing.T) {
	t.Parallel()

	deps := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
	}
	_, err := TopoSort(deps)
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("err = %v, want ErrCycle", err)
	}
}

func TestEdgeCases(t *testing.T) {
	t.Parallel()

	single, err := TopoSort(map[string][]string{"only": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(single, []string{"only"}) {
		t.Fatalf("single = %v, want [only]", single)
	}

	empty, err := TopoSort(map[string][]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty = %v, want []", empty)
	}
}

func Example() {
	deps := map[string][]string{
		"schema": {"seed"},
		"seed":   {"verify"},
		"verify": {},
	}
	order, _ := TopoSort(deps)
	fmt.Println(order)
	// Output: [schema seed verify]
}
```

## Review

The sort is correct when its output is a valid linear extension of the graph:
`TestValidLinearExtension` checks the defining property directly — for every edge
`u -> v`, `u` precedes `v` — which is stronger than matching one hand-picked
permutation. The postorder-plus-reverse is the crux: appending in postorder yields
successors first, and the reverse turns that into the apply order; omit the reverse
and you get a clean-looking but exactly backwards sequence that would apply the
index before the table. `TestCycleReturnsError` confirms an unlinearizable graph
surfaces `ErrCycle` rather than emitting nonsense, which is the behavior you want
when a migration graph is misconfigured. Collecting nodes from both keys and values
ensures leaf migrations that nobody depends on still appear in the order.

## Resources

- [slices package (slices.Reverse)](https://pkg.go.dev/slices#Reverse)
- [sort package (sort.Strings)](https://pkg.go.dev/sort#Strings)
- [Topological sorting (DFS postorder)](https://en.wikipedia.org/wiki/Topological_sorting)
- [errors package (errors.Is)](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-dependency-cycle-detection-dfs.md](05-dependency-cycle-detection-dfs.md) | Next: [07-memoized-transitive-dependencies.md](07-memoized-transitive-dependencies.md)
