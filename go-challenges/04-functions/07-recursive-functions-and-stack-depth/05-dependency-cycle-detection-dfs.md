# Exercise 5: Detect Circular Dependencies with Three-Color Recursive DFS

A cycle detector over a dependency graph, using recursive depth-first search with
white/gray/black coloring to find a back edge and reconstruct the cycle path. On
a backend this catches circular service dependencies, circular imports, or
self-referential migration prerequisites before they deadlock startup.

This module is fully self-contained: its own `go mod init`, the detector inline,
its own demo and tests.

## What you'll build

```text
cycledetect/               independent module: example.com/cycledetect
  go.mod                   go 1.26
  graph.go                 DetectCycle(map[string][]string) ([]string, bool)
  graph_test.go            acyclic, simple cycle, self-loop, disconnected, path validity
  cmd/
    demo/
      main.go              a service graph with a cycle, printed
```

- Files: `graph.go`, `cmd/demo/main.go`, `graph_test.go`.
- Implement: `DetectCycle(graph map[string][]string) ([]string, bool)` using recursive DFS with three-color state; on a cycle, return the closed path (nodes plus the repeated start) and `true`.
- Test: an acyclic DAG returns `false`; `A->B->C->A` returns the cycle; a self-loop `A->A` is detected; disconnected components are each checked; the reported cycle is a real closed path; determinism via sorted iteration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/cycledetect/cmd/demo
cd ~/go-exercises/cycledetect
go mod init example.com/cycledetect
```

### Why three colors, not one set

Recursion over a graph must terminate, and a graph — unlike a tree — can have
cycles, so plain recursion loops forever. A single `visited` set is enough to make
it terminate: never re-enter a node. But a plain visited set cannot tell you *why*
you reached a node again, and that distinction is the entire job of cycle
detection.

Three colors carry the missing information. White is unvisited. Gray means the
node is on the *current* DFS path — you entered it and have not yet finished
exploring its descendants. Black means fully explored — you entered it, finished
all its descendants, and left. When DFS follows an edge to a gray node, that node
is still on your own call stack: you have found a back edge, which is exactly a
cycle. When DFS follows an edge to a black node, it is merely a node you already
finished by another route — a diamond, not a cycle. A visited-set-only traversal
collapses gray and black into one state and so cannot distinguish these, which is
why cycle detection specifically needs the third color.

To reconstruct the actual cycle, keep an explicit slice of the current path
(the gray nodes, in order). When you hit a gray neighbor, slice the path from that
node's first occurrence to the end and append the node again to close the loop —
`[A B C A]` for `A->B->C->A`. That closed form is directly verifiable: every
consecutive pair is a real edge, and the last node equals the first, so a test can
confirm the reported cycle is genuine rather than trusting the boolean.

Determinism matters because the graph is a map, whose iteration order is
randomized. Iterate the start nodes in sorted order, and sort each node's
neighbors before recursing, so the detected cycle is stable run to run and tests
can assert an exact path.

Create `graph.go`:

```go
package graph

import "sort"

const (
	white = iota
	gray
	black
)

// DetectCycle reports whether graph contains a directed cycle. When it does, the
// first slice is the cycle as a closed path (its start node repeated at the end),
// e.g. [A B C A] for A->B->C->A. Iteration is sorted, so the result is
// deterministic.
func DetectCycle(graph map[string][]string) ([]string, bool) {
	color := make(map[string]int, len(graph))
	var path []string
	var cycle []string

	var dfs func(node string) bool
	dfs = func(node string) bool {
		color[node] = gray
		path = append(path, node)

		neighbors := append([]string(nil), graph[node]...)
		sort.Strings(neighbors)
		for _, next := range neighbors {
			switch color[next] {
			case gray:
				cycle = closeCycle(path, next)
				return true
			case white:
				if dfs(next) {
					return true
				}
			}
		}

		color[node] = black
		path = path[:len(path)-1]
		return false
	}

	for _, node := range sortedKeys(graph) {
		if color[node] == white {
			if dfs(node) {
				return cycle, true
			}
		}
	}
	return nil, false
}

// closeCycle returns path from start's first occurrence to the end, with start
// appended to close the loop.
func closeCycle(path []string, start string) []string {
	idx := 0
	for i, n := range path {
		if n == start {
			idx = i
			break
		}
	}
	out := append([]string(nil), path[idx:]...)
	return append(out, start)
}

func sortedKeys(graph map[string][]string) []string {
	keys := make([]string, 0, len(graph))
	for k := range graph {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
```

### The runnable demo

The demo builds a service dependency graph with a deliberate cycle
(`billing -> accounts -> billing`) and prints the detected loop.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/cycledetect"
)

func main() {
	services := map[string][]string{
		"api":      {"accounts", "billing"},
		"accounts": {"billing"},
		"billing":  {"accounts"},
		"audit":    {},
	}

	if cycle, found := graph.DetectCycle(services); found {
		fmt.Printf("cycle detected: %s\n", strings.Join(cycle, " -> "))
	} else {
		fmt.Println("no cycle")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cycle detected: accounts -> billing -> accounts
```

### Tests

`TestAcyclic` confirms a DAG reports no cycle. `TestSimpleCycle` checks the exact
path for `A->B->C->A`. `TestSelfLoop` verifies `A->A` is caught. `TestDisconnected`
puts a cycle in one component and a DAG in another and confirms detection.
`TestReportedCycleIsRealPath` is the strongest: it does not hardcode the expected
nodes but verifies every consecutive pair in the returned cycle is a real edge and
the path is closed.

Create `graph_test.go`:

```go
package graph

import (
	"fmt"
	"reflect"
	"testing"
)

func TestAcyclic(t *testing.T) {
	t.Parallel()

	g := map[string][]string{
		"a": {"b", "c"},
		"b": {"d"},
		"c": {"d"},
		"d": {},
	}
	if cycle, found := DetectCycle(g); found {
		t.Fatalf("acyclic graph reported cycle %v", cycle)
	}
}

func TestSimpleCycle(t *testing.T) {
	t.Parallel()

	g := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
	}
	cycle, found := DetectCycle(g)
	if !found {
		t.Fatal("expected a cycle")
	}
	want := []string{"a", "b", "c", "a"}
	if !reflect.DeepEqual(cycle, want) {
		t.Fatalf("cycle = %v, want %v", cycle, want)
	}
}

func TestSelfLoop(t *testing.T) {
	t.Parallel()

	g := map[string][]string{"a": {"a"}}
	cycle, found := DetectCycle(g)
	if !found {
		t.Fatal("expected self-loop to be detected")
	}
	want := []string{"a", "a"}
	if !reflect.DeepEqual(cycle, want) {
		t.Fatalf("cycle = %v, want %v", cycle, want)
	}
}

func TestDisconnected(t *testing.T) {
	t.Parallel()

	g := map[string][]string{
		"x": {"y"},
		"y": {},
		"p": {"q"},
		"q": {"p"},
	}
	if _, found := DetectCycle(g); !found {
		t.Fatal("expected cycle in the p-q component")
	}
}

func TestReportedCycleIsRealPath(t *testing.T) {
	t.Parallel()

	g := map[string][]string{
		"a": {"b"},
		"b": {"c", "d"},
		"c": {"a"},
		"d": {},
	}
	cycle, found := DetectCycle(g)
	if !found {
		t.Fatal("expected a cycle")
	}
	if len(cycle) < 2 {
		t.Fatalf("cycle too short: %v", cycle)
	}
	if cycle[0] != cycle[len(cycle)-1] {
		t.Fatalf("cycle is not closed: %v", cycle)
	}
	for i := 0; i < len(cycle)-1; i++ {
		from, to := cycle[i], cycle[i+1]
		if !hasEdge(g, from, to) {
			t.Fatalf("no edge %s -> %s in reported cycle %v", from, to, cycle)
		}
	}
}

func hasEdge(g map[string][]string, from, to string) bool {
	for _, n := range g[from] {
		if n == to {
			return true
		}
	}
	return false
}

func Example() {
	g := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
	}
	cycle, found := DetectCycle(g)
	fmt.Println(found, cycle)
	// Output: true [a b c a]
}
```

## Review

The detector is correct when it distinguishes the two ways DFS re-encounters a
node: a gray neighbor is a back edge (a cycle), a black neighbor is a finished
node (a diamond, not a cycle). `TestAcyclic` proves the diamond case does not raise
a false positive, and `TestSimpleCycle`/`TestSelfLoop` prove real cycles are
caught with the exact closed path. `TestReportedCycleIsRealPath` is the honest
check: it validates that every edge in the reported cycle exists and the path
closes, rather than trusting a hardcoded expectation. The mistake this exercise
targets is using a single visited set — it terminates the recursion but cannot
tell a back edge from a cross edge, so it cannot detect the cycle it was meant to
find. Sorted iteration keeps the reported cycle deterministic and the tests stable.

## Resources

- [sort package (sort.Strings)](https://pkg.go.dev/sort#Strings)
- [Depth-first search and back edges (CLRS overview)](https://en.wikipedia.org/wiki/Depth-first_search)
- [Go maps in action (iteration order is randomized)](https://go.dev/blog/maps)
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-recursive-descent-filter-parser.md](04-recursive-descent-filter-parser.md) | Next: [06-topological-sort-migrations.md](06-topological-sort-migrations.md)
