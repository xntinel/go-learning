# Exercise 7: Memoized Recursion for Transitive Dependency Closure

The full transitive closure of a node in a dependency graph — "everything this
service actually pulls in" — computed with recursion plus a memo cache so shared
subgraphs are walked once, turning exponential re-walks into linear work. This is
the core of blast-radius and "what does deploying X actually touch" analysis.

This module is fully self-contained: its own `go mod init`, the closure inline,
its own demo and tests.

## What you'll build

```text
transitive/                independent module: example.com/transitive
  go.mod                   go 1.26
  deps.go                  TransitiveDeps; TransitiveDepsWithVisits; ErrUnknownNode
  deps_test.go             diamond dedup, visit-once counter, unknown root, cycle-safe
  cmd/
    demo/
      main.go              closure of a service, with the visit-count saving
```

- Files: `deps.go`, `cmd/demo/main.go`, `deps_test.go`.
- Implement: `TransitiveDeps(graph map[string][]string, root string) ([]string, error)` computing the sorted, deduped transitive closure with a `memo map[string][]string`; expose `TransitiveDepsWithVisits` returning per-node compute counts; unknown root returns `ErrUnknownNode`.
- Test: a diamond graph returns the shared node once; a counter proves each node is computed once; an unknown root errors; a cyclic graph terminates.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/07-memoized-transitive-dependencies/cmd/demo
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/07-memoized-transitive-dependencies
```

### Why memoization is not optional here

The transitive closure of a node is the set of all nodes reachable from it. The
naive recursion is obvious: a node's closure is the union of its direct
dependencies and each dependency's closure. The problem is shared subgraphs. In a
diamond — `A` depends on `B` and `C`, both of which depend on `D` — a naive walk
computes `D`'s closure once for the path through `B` and again for the path through
`C`. Stack a few diamonds and the number of recomputations doubles at each level:
the work becomes exponential in the graph's depth even though the graph is small.

Memoization fixes this by caching each node's computed closure in a map. The first
time the recursion needs `D`'s closure it computes it; every later request returns
the cached slice. Each node is computed once, so total work is linear in the number
of edges. The cache also makes recursion over a *cyclic* graph terminate: a
"currently computing" guard short-circuits a revisit instead of recursing forever,
so a graph with a back edge yields a finite closure rather than a stack overflow.

To make the "computed once" claim testable rather than merely asserted,
`TransitiveDepsWithVisits` returns a `map[string]int` counting how many times each
node's closure was actually computed (cache misses). A test can then assert every
node's count is exactly one, which is the empirical proof that the memo works —
the difference between a correct-but-exponential implementation and a correct
linear one is invisible in the returned closure but glaring in the visit counts.

The public `TransitiveDeps` wraps the counted version and discards the counts. The
closure excludes the root itself (you asked what `root` pulls in, not `root`), is
deduped, and is sorted for a deterministic result. An unknown root — one that is
not a key in the graph — returns `ErrUnknownNode`, because "closure of a node that
does not exist" is a caller bug, not an empty answer.

Create `deps.go`:

```go
package deps

import (
	"errors"
	"fmt"
	"slices"
)

// ErrUnknownNode is returned when the requested root is not present in the graph.
var ErrUnknownNode = errors.New("unknown node")

// TransitiveDeps returns the sorted, deduplicated set of all nodes reachable from
// root (excluding root itself). Shared subgraphs are computed once via a memo.
func TransitiveDeps(graph map[string][]string, root string) ([]string, error) {
	out, _, err := TransitiveDepsWithVisits(graph, root)
	return out, err
}

// TransitiveDepsWithVisits is TransitiveDeps plus a per-node count of how many
// times each node's closure was computed. With memoization every count is 1,
// which the tests assert to prove the cache eliminates repeated work.
func TransitiveDepsWithVisits(graph map[string][]string, root string) ([]string, map[string]int, error) {
	if _, ok := graph[root]; !ok {
		return nil, nil, fmt.Errorf("%q: %w", root, ErrUnknownNode)
	}

	memo := make(map[string][]string)
	visits := make(map[string]int)
	inProgress := make(map[string]bool)

	var closure func(node string) []string
	closure = func(node string) []string {
		if cached, ok := memo[node]; ok {
			return cached
		}
		if inProgress[node] {
			return nil // cycle guard: break the back edge
		}
		inProgress[node] = true
		visits[node]++

		set := make(map[string]struct{})
		for _, dep := range graph[node] {
			set[dep] = struct{}{}
			for _, t := range closure(dep) {
				set[t] = struct{}{}
			}
		}

		result := make([]string, 0, len(set))
		for n := range set {
			result = append(result, n)
		}
		slices.Sort(result)

		memo[node] = result
		inProgress[node] = false
		return result
	}

	return closure(root), visits, nil
}
```

### The runnable demo

The demo computes the closure of a service whose dependencies form a diamond, and
reports how many nodes were computed exactly once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/transitive"
)

func main() {
	graph := map[string][]string{
		"web":    {"auth", "orders"},
		"auth":   {"cache", "db"},
		"orders": {"cache", "db"},
		"cache":  {"db"},
		"db":     {},
	}

	closure, visits, err := deps.TransitiveDepsWithVisits(graph, "web")
	if err != nil {
		panic(err)
	}
	fmt.Printf("web pulls in: %v\n", closure)
	fmt.Printf("nodes computed once each: %v\n", allOne(visits))
}

func allOne(visits map[string]int) bool {
	for _, c := range visits {
		if c != 1 {
			return false
		}
	}
	return true
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
web pulls in: [auth cache db orders]
nodes computed once each: true
```

### Tests

`TestDiamondDedup` proves the shared node `D` appears once, not twice.
`TestVisitsEachNodeOnce` uses the visit counter to prove memoization: every node
computed exactly once. `TestUnknownRoot` checks the sentinel. `TestCycleSafe`
feeds a cyclic graph and asserts the closure is finite (the recursion terminates)
rather than overflowing.

Create `deps_test.go`:

```go
package deps

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestDiamondDedup(t *testing.T) {
	t.Parallel()

	graph := map[string][]string{
		"a": {"b", "c"},
		"b": {"d"},
		"c": {"d"},
		"d": {},
	}
	got, err := TransitiveDeps(graph, "a")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"b", "c", "d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closure = %v, want %v", got, want)
	}
}

func TestVisitsEachNodeOnce(t *testing.T) {
	t.Parallel()

	// A deep diamond stack: without memo, node z would be recomputed many times.
	graph := map[string][]string{
		"a": {"b", "c"},
		"b": {"d", "e"},
		"c": {"d", "e"},
		"d": {"z"},
		"e": {"z"},
		"z": {},
	}
	_, visits, err := TransitiveDepsWithVisits(graph, "a")
	if err != nil {
		t.Fatal(err)
	}
	for node, count := range visits {
		if count != 1 {
			t.Fatalf("node %s computed %d times, want 1", node, count)
		}
	}
	if len(visits) != 6 {
		t.Fatalf("visited %d distinct nodes, want 6", len(visits))
	}
}

func TestUnknownRoot(t *testing.T) {
	t.Parallel()

	_, err := TransitiveDeps(map[string][]string{"a": {}}, "missing")
	if !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("err = %v, want ErrUnknownNode", err)
	}
}

func TestCycleSafe(t *testing.T) {
	t.Parallel()

	graph := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"}, // back edge
	}
	got, err := TransitiveDeps(graph, "a")
	if err != nil {
		t.Fatal(err)
	}
	// The closure is finite; a and its reachable set are b, c (and a via the cycle).
	want := []string{"a", "b", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closure = %v, want %v", got, want)
	}
}

func Example() {
	graph := map[string][]string{
		"api":   {"store"},
		"store": {"disk"},
		"disk":  {},
	}
	closure, _ := TransitiveDeps(graph, "api")
	fmt.Println(closure)
	// Output: [disk store]
}
```

## Review

The closure is correct when a shared node appears exactly once regardless of how
many paths reach it — `TestDiamondDedup` proves the result set, but
`TestVisitsEachNodeOnce` proves the thing that actually matters: each node is
*computed* once, which is the difference between the linear implementation and a
correct-but-exponential one that returns the same answer while doing far more work.
That is why the visit counter is exposed and asserted rather than trusted. The
cycle guard makes the recursion safe on cyclic input by short-circuiting a revisit
instead of recursing forever; `TestCycleSafe` confirms a graph with a back edge
yields a finite closure. The mistake this exercise targets is recomputing shared
subgraphs — invisible in the output, fatal to performance at scale.

## Resources

- [slices package (slices.Sort)](https://pkg.go.dev/slices#Sort)
- [Memoization (dynamic programming overview)](https://en.wikipedia.org/wiki/Memoization)
- [errors package (errors.Is)](https://pkg.go.dev/errors#Is)
- [Go maps (make, iteration)](https://go.dev/blog/maps)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-topological-sort-migrations.md](06-topological-sort-migrations.md) | Next: [08-generic-tree-fold.md](08-generic-tree-fold.md)
