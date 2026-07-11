# Exercise 18: Recursive Shortest-Path Finder with Explicit Stack

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Finding the shortest path through a weighted dependency graph (service call
routing, cheapest build order, cost-optimal migration ordering) is naturally
recursive: order the nodes so every edge points forward, then walk them
computing the best distance so far. Written as literal recursive calls, that
walk puts one goroutine-stack frame on hold per node on the deepest chain in
the graph — fine for a small graph, a real risk for a large generated one
(a build graph with thousands of steps, an org chart imported from a
spreadsheet). This exercise builds the same depth-first walk, but with the
call stack replaced by an explicit, heap-allocated stack of frames — the
standard technique for keeping a recursive algorithm's *shape* while
removing its dependency on the goroutine stack's fixed size.

This module is fully self-contained: its own `go mod init`, the algorithm
inline, its own demo and tests.

## What you'll build

```text
shortestpath/                 independent module: example.com/shortestpath
  go.mod                        go 1.24
  shortestpath.go                type Graph; func ShortestPath
  shortestpath_test.go            cheaper route, same start/end, unreachable, cycle, self-loop, 20000-deep chain
  cmd/
    demo/
      main.go                     picks the cheaper of two routes through a small service graph
```

- Files: `shortestpath.go`, `cmd/demo/main.go`, `shortestpath_test.go`.
- Implement: `type Graph map[string]map[string]int` and
  `func ShortestPath(g Graph, start, end string) ([]string, int, error)`,
  built on an explicit-stack topological sort that detects cycles the same
  way three-color recursive DFS would, but with a stack of `frame` values
  instead of function calls.
- Test: a graph with two routes picks the cheaper one; `start == end` is the
  trivial zero-weight path; an unreachable end returns `ErrUnreachable`; a
  cycle (including a self-loop) returns `ErrCycle`; a chain 20000 nodes deep
  completes without a stack-related failure.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shortestpath/cmd/demo
cd ~/go-exercises/shortestpath
go mod init example.com/shortestpath
go mod edit -go=1.24
```

### Turning recursive calls into an explicit stack of frames

A recursive depth-first search processes a node by recursing into its first
unvisited neighbor, then its second, and so on, only finishing the node
(marking it done) after every neighbor has been handled — the "finish"
step happens on the way back out of the recursion, same as the AVL
rebalancing and cycle detection exercises elsewhere in this lesson. The
information that makes this work is, implicitly, on the call stack: which
node is "in progress," and how far through its neighbor list it has gotten.

The explicit-stack version keeps that same two pieces of information, but
in a struct instead of a stack frame: a `frame{node, neighbors, neighborIdx}`
records which node is in progress and an index into its already-sorted
neighbor list. The outer loop is a `for len(stack) > 0` instead of a
function call: at each step, look at the top frame; if it has an unvisited
neighbor left, either follow it (color it gray, push a new frame) or, if
that neighbor is already gray, report `ErrCycle` — a back edge to a node
still being explored, the same signal recursive DFS uses. If the top
frame's neighbor list is exhausted, that node is finished: color it black,
append it to the finish order, and pop the frame. This is exactly what
recursive DFS does, one call at a time; the only difference is the stack of
in-progress nodes is a `[]*frame` the function owns and grows on the heap,
rather than the goroutine's own call stack, which has a size the program
does not control directly.

Once nodes are ordered so every edge points from earlier to later (the
reverse of the DFS finish order, a standard fact about topological sort),
computing shortest distances is a single forward pass: process nodes in
that order, and for each one already known to be reachable, relax its
outgoing edges into a running `dist` map.

Create `shortestpath.go`:

```go
// Package shortestpath computes shortest paths in a weighted directed
// acyclic graph. The traversal that would naturally be written as recursive
// depth-first search is instead implemented with an explicit stack of
// frames, so a long dependency chain cannot exhaust the goroutine stack, and
// the same pass that would detect a cycle in recursive DFS (a back edge to
// a node still "on the stack") detects it here too, using an explicit color
// map instead of the call stack's implicit state.
package shortestpath

import (
	"errors"
	"fmt"
	"sort"
)

// ErrCycle is returned when the graph is not a DAG: a path from some node
// leads back to a node that is still being explored.
var ErrCycle = errors.New("shortestpath: graph contains a cycle")

// ErrUnreachable is returned when end cannot be reached from start.
var ErrUnreachable = errors.New("shortestpath: end is unreachable from start")

// Graph maps a node to its outgoing edges and their weights.
type Graph map[string]map[string]int

const (
	white = iota
	gray
	black
)

// frame is one level of the explicit DFS stack: the node being explored and
// how far through its (sorted) neighbor list the exploration has gotten.
// Keeping this in a slice-backed stack instead of recursive calls means the
// traversal's memory use is exactly one frame per node on the current path,
// heap-allocated, rather than one goroutine-stack frame per node — the two
// behave the same for detecting cycles and computing finish order, but the
// explicit version is not subject to a fixed maximum recursion depth.
type frame struct {
	node        string
	neighbors   []string
	neighborIdx int
}

// ShortestPath returns the shortest path (by summed edge weight) from start
// to end in g, along with its total weight. g must be acyclic; ErrCycle is
// returned otherwise. ErrUnreachable is returned if end cannot be reached.
func ShortestPath(g Graph, start, end string) ([]string, int, error) {
	topo, err := topologicalOrder(g)
	if err != nil {
		return nil, 0, err
	}

	dist := map[string]int{start: 0}
	prev := map[string]string{}
	reached := false

	for _, node := range topo {
		if node == start {
			reached = true
		}
		if !reached {
			continue
		}
		d, ok := dist[node]
		if !ok {
			continue
		}
		for _, next := range sortedKeys(g[node]) {
			candidate := d + g[node][next]
			if cur, ok := dist[next]; !ok || candidate < cur {
				dist[next] = candidate
				prev[next] = node
			}
		}
	}

	total, ok := dist[end]
	if !ok {
		return nil, 0, fmt.Errorf("%s: %w", end, ErrUnreachable)
	}

	path := []string{end}
	for cur := end; cur != start; {
		p, ok := prev[cur]
		if !ok {
			break
		}
		path = append(path, p)
		cur = p
	}
	reverseStrings(path)
	return path, total, nil
}

// topologicalOrder returns g's nodes in topological order (every edge points
// from an earlier node to a later one) using an explicit-stack depth-first
// search, or ErrCycle if g is not a DAG. Coloring plays the same role three
// colors play in a fully recursive DFS cycle detector: white is unvisited,
// gray is "on the current path," black is fully finished. Here that state
// lives in a map instead of being implicit in which recursive calls are
// still on the goroutine's call stack.
func topologicalOrder(g Graph) ([]string, error) {
	color := make(map[string]int, len(g))
	var finishOrder []string

	for _, start := range sortedNodes(g) {
		if color[start] != white {
			continue
		}

		color[start] = gray
		stack := []*frame{{node: start, neighbors: sortedKeys(g[start])}}

		for len(stack) > 0 {
			top := stack[len(stack)-1]
			if top.neighborIdx >= len(top.neighbors) {
				color[top.node] = black
				finishOrder = append(finishOrder, top.node)
				stack = stack[:len(stack)-1]
				continue
			}

			next := top.neighbors[top.neighborIdx]
			top.neighborIdx++

			switch color[next] {
			case gray:
				return nil, fmt.Errorf("%s: %w", next, ErrCycle)
			case white:
				color[next] = gray
				stack = append(stack, &frame{node: next, neighbors: sortedKeys(g[next])})
			}
		}
	}

	reverseStrings(finishOrder)
	return finishOrder, nil
}

func sortedNodes(g Graph) []string {
	nodes := make([]string, 0, len(g))
	for n := range g {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	return nodes
}

func sortedKeys(edges map[string]int) []string {
	keys := make([]string, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func reverseStrings(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
```

### The runnable demo

The demo has two routes from `gateway` to `billing` — through `auth`
(weight 1+2=3) and through `cache` (weight 4+1=5) — and confirms the
cheaper one is chosen.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/shortestpath"
)

func main() {
	g := shortestpath.Graph{
		"gateway": {"auth": 1, "cache": 4},
		"auth":    {"billing": 2},
		"cache":   {"billing": 1},
		"billing": {},
	}

	path, total, err := shortestpath.ShortestPath(g, "gateway", "billing")
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("path: %s\n", strings.Join(path, " -> "))
	fmt.Printf("total weight: %d\n", total)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
path: gateway -> auth -> billing
total weight: 3
```

### Tests

`TestShortestPathPicksCheaperRoute` confirms the algorithm's whole point:
given two routes, it picks the lower-weight one, not just any path.
`TestShortestPathSameStartAndEnd` checks the trivial zero-weight case.
`TestShortestPathUnreachable` and the two cycle tests
(`TestShortestPathDetectsCycle`, `TestShortestPathDetectsSelfLoop`) check
the two ways a graph can fail to yield an answer. `TestShortestPathDeepChainDoesNotOverflowStack`
is the test that justifies the whole exercise: a 20000-node chain, which a
naive recursive implementation risks handling badly on a constrained stack,
must complete cleanly with the explicit-stack version.

Create `shortestpath_test.go`:

```go
package shortestpath

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestShortestPathPicksCheaperRoute(t *testing.T) {
	t.Parallel()

	g := Graph{
		"gateway": {"auth": 1, "cache": 4},
		"auth":    {"billing": 2},
		"cache":   {"billing": 1},
		"billing": {},
	}

	path, total, err := ShortestPath(g, "gateway", "billing")
	if err != nil {
		t.Fatalf("ShortestPath() error = %v", err)
	}
	wantPath := []string{"gateway", "auth", "billing"}
	if !reflect.DeepEqual(path, wantPath) {
		t.Errorf("path = %v, want %v", path, wantPath)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
}

func TestShortestPathSameStartAndEnd(t *testing.T) {
	t.Parallel()

	g := Graph{"a": {"b": 5}, "b": {}}
	path, total, err := ShortestPath(g, "a", "a")
	if err != nil {
		t.Fatalf("ShortestPath() error = %v", err)
	}
	if !reflect.DeepEqual(path, []string{"a"}) || total != 0 {
		t.Errorf("path = %v, total = %d, want [a], 0", path, total)
	}
}

func TestShortestPathUnreachable(t *testing.T) {
	t.Parallel()

	g := Graph{"a": {"b": 1}, "b": {}, "isolated": {}}
	_, _, err := ShortestPath(g, "a", "isolated")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("ShortestPath() error = %v, want %v", err, ErrUnreachable)
	}
}

func TestShortestPathDetectsCycle(t *testing.T) {
	t.Parallel()

	g := Graph{
		"a": {"b": 1},
		"b": {"c": 1},
		"c": {"a": 1},
	}
	_, _, err := ShortestPath(g, "a", "c")
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("ShortestPath() error = %v, want %v", err, ErrCycle)
	}
}

func TestShortestPathDetectsSelfLoop(t *testing.T) {
	t.Parallel()

	g := Graph{"a": {"a": 1}}
	_, _, err := ShortestPath(g, "a", "a")
	if !errors.Is(err, ErrCycle) {
		t.Fatalf("ShortestPath() error = %v, want %v", err, ErrCycle)
	}
}

func TestShortestPathDeepChainDoesNotOverflowStack(t *testing.T) {
	t.Parallel()

	// A chain 20000 nodes long: n0 -> n1 -> ... -> n19999. A recursive DFS
	// this deep risks a stack-growth failure on some configurations; the
	// explicit-stack version just grows a heap-allocated slice.
	const n = 20000
	g := make(Graph, n)
	for i := 0; i < n-1; i++ {
		g[fmt.Sprintf("n%d", i)] = map[string]int{fmt.Sprintf("n%d", i+1): 1}
	}
	g[fmt.Sprintf("n%d", n-1)] = map[string]int{}

	path, total, err := ShortestPath(g, "n0", fmt.Sprintf("n%d", n-1))
	if err != nil {
		t.Fatalf("ShortestPath() error = %v", err)
	}
	if len(path) != n {
		t.Fatalf("path length = %d, want %d", len(path), n)
	}
	if total != n-1 {
		t.Fatalf("total = %d, want %d", total, n-1)
	}
}
```

Run it: `go test -count=1 ./...`

## Review

`ShortestPath` is correct when it always returns the minimum-weight path
(`TestShortestPathPicksCheaperRoute` is the direct check), correctly
signals the two distinct failure modes — unreachable versus cyclic — with
different sentinel errors, and does all of this without recursive function
calls whose depth tracks the graph's longest chain. The mistake this
exercise targets is assuming "convert recursion to an explicit stack" is
purely a memory optimization with no behavioral risk: it is easy to get the
push/pop bookkeeping subtly wrong (finishing a node before all its
neighbors are visited, or comparing colors at the wrong point) and silently
break cycle detection while everything still compiles.
`TestShortestPathDeepChainDoesNotOverflowStack` is the test that
specifically justifies not using plain recursion here.

## Resources

- [Go Specification: Slices](https://go.dev/ref/spec#Slice_types)
- [Topological sorting (DFS finish-order method)](https://en.wikipedia.org/wiki/Topological_sorting)
- [sort package (sort.Strings)](https://pkg.go.dev/sort#Strings)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-regex-backtracking-memoization-table.md](17-regex-backtracking-memoization-table.md) | Next: [19-even-odd-mutual-recursion-depth-guard.md](19-even-odd-mutual-recursion-depth-guard.md)
