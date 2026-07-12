# Exercise 19: Service Deploy Order from an Adjacency-List Map

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A microservice deploy pipeline that knows "this service depends on these
others" -- the API needs the database migrated first, the worker needs the
queue provisioned, everything needs the network layer up -- has to turn that
dependency graph into a linear deploy order before it can act. The natural
representation for the graph is exactly the shape Go hands you for free: a
`map[string][]string` from a service name to the names it depends on. The
hard part is not building that map, it is walking it safely: a genuine
misconfiguration (service A depends on B, B depends on C, C depends back on
A) makes no valid deploy order exist at all, and a pipeline that does not
notice will either deadlock waiting for a service that can never finish, or
worse, deploy something before a dependency it silently needed.

This module builds a real command-line tool around that walk: it reads
`name: dep1, dep2` lines from stdin -- the same adjacency-list shape a
Terraform module graph or a Helm chart's dependency block already uses --
and computes the order with an iterative depth-first search using the
classical three-color marking scheme to detect a cycle the instant the
traversal walks back onto one of its own ancestors. It reports the exact
cycle path when it finds one -- not just "there is a cycle somewhere," but
which services are in it -- and exits 1 so a CI pipeline can gate on it.
Because map range order is randomized, the traversal sorts each service's
dependency list before visiting it, so the same graph always produces the
same deploy order and, when a cycle exists, always reports the same one.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
deployorder/            module example.com/deployorder
  go.mod                go 1.24
  depgraph.go           package main — ParseGraph(r) (map[string][]string, error); CycleError;
                         DeployOrder(graph) ([]string, error)
  depgraph_test.go      package main — ParseGraph table, DeployOrder table (chain, diamond,
                         self-loop, cycle, disconnected, empty), property + determinism checks,
                         run() end to end
  main.go               package main — -check flag, exit codes
```

- Files: `depgraph.go`, `depgraph_test.go`, `main.go`.
- Implement: `ParseGraph(r io.Reader) (map[string][]string, error)`, reading `"name: dep1, dep2"` lines (blank/`#` lines skipped, `"name:"` means no dependencies), rejecting a colon-less line with `ErrMalformedLine`, an empty name with `ErrEmptyService`, and a name defined twice with `ErrDuplicateService`, all wrapped with `%w` and the line number; `DeployOrder(graph map[string][]string) ([]string, error)`, an iterative DFS with an explicit stack of frames and a three-color (`white`/`gray`/`black`) visit map, returning services in dependency-before-dependent order or a `*CycleError` (with a `Path []string` field) the instant the traversal walks into a `gray` node; every dependency list and the outer set of starting services are sorted before use, so both the order and which cycle gets reported are deterministic.
- Tool: `deployorder` reads the graph from stdin and prints the deploy order to stdout, one service per line. `-check` validates the graph is acyclic without printing the order. Exit 0 on success, exit 2 for a malformed line, an empty or duplicate service name, or an unknown flag (all usage errors), exit 1 for a discovered cycle -- the input parsed fine, but the graph itself has no valid order, which is a runtime failure rather than a usage mistake.
- Test: a table covering a linear chain, a diamond (shared dependency reached two ways), a self-loop, a two-node cycle, disconnected components, and the empty graph; a property test confirming every dependency's position in the output precedes its dependent's and that repeated calls agree exactly; `run` end to end over a `strings.Reader` and a `bytes.Buffer`, including `-check` and the cycle/usage exit-code split.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why three colors, and why the order comes out right without a reversal step

A plain "visited/unvisited" boolean is not enough to detect a cycle during a
DFS: it cannot distinguish between "I have already fully finished processing
this node on some other path" (completely fine, and expected in any graph
with a shared dependency -- a diamond) from "this node is one of my own
ancestors on the *current* path" (a cycle). Three colors make that
distinction explicit. `white` means unvisited. `gray` means the node is on
the stack of the DFS branch currently in progress -- an ancestor of whatever
node the traversal is looking at right now. `black` means the node and
everything it depends on has been fully processed. The only condition that
signals a cycle is the traversal following an edge from the current node
into a node that is *already gray* -- that can only happen if the current
path has looped back onto one of its own ancestors. An edge into a `black`
node is completely legitimate: it means two different services both depend
on some already-finished dependency, which is exactly what a diamond shape
looks like, not a cycle.

The traversal is written as an explicit stack of frames rather than a
recursive function precisely because a hand-written recursive DFS ties its
call-stack depth to the graph's longest dependency chain -- fine for a
five-service demo, a real liability for a service mesh with hundreds of
services and long transitive chains. Each `frame` on the stack tracks which
node it is visiting and how far through that node's *sorted* dependency
list it has advanced; when a frame runs out of dependencies to visit, its
node turns `black` and gets appended to the output.

That append-on-finish detail is what makes the output order correct without
any reversal step, which trips people up if they remember the "reverse
post-order" recipe for topological sort: that recipe applies when edges
point from a task to what must run *after* it. Here the edges point the
other way -- `graph[svc]` lists what `svc` *depends on*, what must finish
*before* `svc` can start -- so a dependency is guaranteed to turn `black`
(and get appended) strictly before the service that depends on it does.
Appending in finish order, left to right, already produces
dependency-before-dependent order; no reversal needed. Both the sorted
dependency lists inside each frame and the sorted starting-node list make
the whole traversal, including which cycle gets reported first when more
than one exists, the same on every run.

Create `depgraph.go`:

```go
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// Sentinel parse errors returned by ParseGraph, checkable with errors.Is.
var (
	ErrMalformedLine    = errors.New("deployorder: malformed line, want NAME: dep1, dep2")
	ErrEmptyService     = errors.New("deployorder: empty service name")
	ErrDuplicateService = errors.New("deployorder: service defined more than once")
)

// ParseGraph reads "name: dep1, dep2, ..." lines from r into an adjacency
// map ("name:" means no dependencies). A dependency that never appears as
// its own line is still a valid leaf node. Errors wrap %w and the 1-based
// line number that failed.
func ParseGraph(r io.Reader) (map[string][]string, error) {
	graph := make(map[string][]string)
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, depsPart, ok := strings.Cut(text, ":")
		if !ok {
			return nil, fmt.Errorf("line %d: %w: %q", line, ErrMalformedLine, text)
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("line %d: %w", line, ErrEmptyService)
		}
		if _, exists := graph[name]; exists {
			return nil, fmt.Errorf("line %d: %w: %q", line, ErrDuplicateService, name)
		}
		var deps []string
		for _, d := range strings.Split(depsPart, ",") {
			if d = strings.TrimSpace(d); d != "" {
				deps = append(deps, d)
			}
		}
		graph[name] = deps
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("deployorder: reading input: %w", err)
	}
	return graph, nil
}

// color is a node's state in the three-color DFS: white unvisited, gray an
// ancestor on the current path, black fully processed.
type color int

const (
	white color = iota
	gray
	black
)

// CycleError reports a dependency cycle found while computing a deploy
// order. Path lists the cycle's nodes in traversal order, starting and
// ending at the same node.
type CycleError struct {
	Path []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("deployorder: cycle detected: %s", strings.Join(e.Path, " -> "))
}

// DeployOrder computes a deterministic deploy order for the services in
// graph, where graph[svc] lists what svc depends on (must deploy first).
// It returns services in dependency-before-dependent order, or a
// *CycleError if graph is not a DAG. See visit for the traversal itself.
func DeployOrder(graph map[string][]string) ([]string, error) {
	colors := make(map[string]color, len(graph))
	var order []string
	for _, start := range allNodes(graph) {
		if colors[start] != white {
			continue
		}
		if err := visit(graph, colors, &order, start); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// frame is one entry on the explicit DFS stack.
type frame struct {
	node string
	deps []string
	next int
}

// visit runs an iterative DFS from start; path tracks the current gray
// chain so a found cycle reports its full loop.
func visit(graph map[string][]string, colors map[string]color, order *[]string, start string) error {
	stack := []frame{{node: start, deps: sortedDeps(graph, start)}}
	colors[start] = gray
	path := []string{start}
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		if top.next >= len(top.deps) {
			colors[top.node] = black
			*order = append(*order, top.node)
			stack = stack[:len(stack)-1]
			path = path[:len(path)-1]
			continue
		}
		dep := top.deps[top.next]
		top.next++
		switch colors[dep] {
		case white:
			colors[dep] = gray
			path = append(path, dep)
			stack = append(stack, frame{node: dep, deps: sortedDeps(graph, dep)})
		case gray:
			idx := slices.Index(path, dep)
			return &CycleError{Path: append(slices.Clone(path[idx:]), dep)}
		case black:
			// Already fully processed via another path: a diamond, not a cycle.
		}
	}
	return nil
}

// allNodes returns every service name in graph, key or dependency, sorted
// so traversal order (and which cycle is found first) is deterministic.
func allNodes(graph map[string][]string) []string {
	set := make(map[string]struct{}, len(graph))
	for node, deps := range graph {
		set[node] = struct{}{}
		for _, d := range deps {
			set[d] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// sortedDeps returns a sorted copy of graph[node]'s dependency list.
func sortedDeps(graph map[string][]string, node string) []string {
	deps := slices.Clone(graph[node])
	slices.Sort(deps)
	return deps
}
```

### The tool

`deployorder` reads the whole graph from stdin: a deploy pipeline pipes in
whatever generated the dependency list (a Terraform graph export, a Helm
chart's dependency block, a hand-maintained file in the repo) and reads the
order or the cycle back off stdout/stderr. `run` takes the flag arguments,
an `io.Reader` for stdin, and an `io.Writer` for stdout, so it is testable
with a `strings.Reader` and a `bytes.Buffer` without a real process.
`flag.NewFlagSet` with `flag.ContinueOnError` lets `run` return the parse
error instead of the package-level flag set calling `os.Exit`. The exit
code split is the one genuinely interesting design decision here: a
malformed line, an empty or duplicate name, or an unknown flag are all
usage errors -- the caller wrote something wrong and exit code 2 says so --
but a discovered cycle is different in kind. The input was syntactically
fine; the graph it describes simply has no valid deploy order. That is a
runtime failure, not a usage mistake, so it gets exit code 1 instead, and
`*CycleError` deliberately does not wrap `errUsage` so the two paths in
`main` stay distinct. `-check` exists for a CI step that only wants the
exit code, not the full order, printed on every invocation.

Create `main.go`:

```go
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// errUsage marks a failure fixable by changing the input or the command
// line. main maps it to exit code 2. A discovered cycle is not a usage
// error -- the input parsed fine, the graph itself has no valid order --
// so it maps to exit code 1 instead.
var errUsage = errors.New("usage")

// run parses a dependency graph from stdin and prints its deterministic
// deploy order to stdout, one service per line, or returns the
// *CycleError DeployOrder found. With -check it validates without
// printing the order. run never touches os.Stdin/os.Stdout/os.Exit, so it
// is testable with a strings.Reader and a bytes.Buffer.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("deployorder", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "only validate the graph is acyclic; do not print the order")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	graph, err := ParseGraph(stdin)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	order, err := DeployOrder(graph)
	if err != nil {
		return err // *CycleError: a runtime failure, not a usage error.
	}
	if *check {
		fmt.Fprintln(stdout, "ok: no cycle")
		return nil
	}
	for _, svc := range order {
		fmt.Fprintln(stdout, svc)
	}
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: deployorder [-check] < graph.txt")
		fmt.Fprintln(os.Stderr, "reads \"name: dep1, dep2\" lines from stdin, prints a deploy order or a cycle.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "deployorder:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf 'api: database, cache\ndatabase: network\ncache: network\nworker: database, queue\nqueue: network\nnetwork:\n' | go run .
printf 'api: database\ndatabase: migration-runner\nmigration-runner: api\n' | go run .
printf 'api: database\ndatabase:\n' | go run . -check
```

Expected output:

```text
network
cache
database
api
queue
worker
deployorder: deployorder: cycle detected: api -> database -> migration-runner -> api
ok: no cycle
```

The first command's `network` has no dependencies, so it finishes first;
`api` and `queue` both wait on services that themselves wait on `network`,
and `worker` -- the service with the deepest transitive dependency chain
here -- finishes last. The second command's cycle is reported exactly as
`api -> database -> migration-runner -> api`, the full loop, prefixed twice
because `main` prepends `deployorder:` to whatever `run` returned, and
`run` already wrapped `CycleError.Error()`'s own `deployorder:` prefix --
and it exits 1, not 2, because the input parsed correctly. The third
command's `-check` confirms the graph is acyclic without printing the order.

### Tests

`TestParseGraph` is the table over the input shapes a dependency file can
take: basic edges, comments and whitespace, a malformed line, an empty
name, a duplicate definition, and empty input. `TestDeployOrder` is the
table over the graph shapes that matter: a linear chain, a diamond (two
services sharing one dependency), a self-loop, a two-node cycle,
disconnected components -- the case that proves `DeployOrder` restarts the
traversal for every unvisited node, not just the first one reached -- and
the empty graph. `TestDeployOrderPropertiesHold` checks the actual
contract independent of one specific valid order: every dependency's
position precedes its dependent's, and ten repeated calls on the same
graph produce byte-for-byte the same order, which only holds because both
the dependency lists and the starting-node list are sorted before use.
`TestRun` drives the whole tool end to end: a valid graph, `-check`, a
cycle (asserted to *not* wrap `errUsage`), a malformed line, and an unknown
flag (both asserted to wrap it), and empty input.

Create `depgraph_test.go`:

```go
package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParseGraph(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   string
		want    map[string][]string
		wantErr error
	}{
		{"basic edges", "api: database, cache\ndatabase:\ncache:\n",
			map[string][]string{"api": {"database", "cache"}, "database": nil, "cache": nil}, nil},
		{"blank lines, comments, whitespace trimmed", "# top\napi :  database \n\n  \n# another\ndatabase:\n",
			map[string][]string{"api": {"database"}, "database": nil}, nil},
		{"malformed line has no colon", "not-a-line\n", nil, ErrMalformedLine},
		{"empty service name", ": database\n", nil, ErrEmptyService},
		{"duplicate service definition", "api: database\napi: cache\n", nil, ErrDuplicateService},
		{"empty input", "", map[string][]string{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseGraph(strings.NewReader(tc.input))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ParseGraph() error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGraph(): unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("ParseGraph() = %v, want %v", got, tc.want)
			}
			for name, deps := range tc.want {
				if !slices.Equal(got[name], deps) {
					t.Errorf("ParseGraph()[%q] = %v, want %v", name, got[name], deps)
				}
			}
		})
	}
}

// TestDeployOrder covers linear, diamond, self-loop, cycle, disconnected, and empty shapes.
func TestDeployOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		graph     map[string][]string
		wantOrder []string
		wantCycle []string // nil means no cycle expected
	}{
		{"linear chain", map[string][]string{"a": {"b"}, "b": {"c"}, "c": {}}, []string{"c", "b", "a"}, nil},
		{"diamond", map[string][]string{"d": {"b", "c"}, "b": {"a"}, "c": {"a"}, "a": {}}, []string{"a", "b", "c", "d"}, nil},
		{"self loop", map[string][]string{"x": {"x"}}, nil, []string{"x", "x"}},
		{"two node cycle", map[string][]string{"a": {"b"}, "b": {"a"}}, nil, []string{"a", "b", "a"}},
		{"disconnected components", map[string][]string{"a": {"b"}, "b": {}, "x": {"y"}, "y": {}}, []string{"b", "a", "y", "x"}, nil},
		{"empty graph", map[string][]string{}, []string{}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DeployOrder(tc.graph)
			if tc.wantCycle != nil {
				var cycleErr *CycleError
				if !errors.As(err, &cycleErr) {
					t.Fatalf("DeployOrder() error = %v, want *CycleError", err)
				}
				if !slices.Equal(cycleErr.Path, tc.wantCycle) {
					t.Fatalf("cycle path = %v, want %v", cycleErr.Path, tc.wantCycle)
				}
				return
			}
			if err != nil {
				t.Fatalf("DeployOrder() error = %v, want nil", err)
			}
			if !slices.Equal(got, tc.wantOrder) {
				t.Fatalf("DeployOrder() = %v, want %v", got, tc.wantOrder)
			}
		})
	}
}

// TestDeployOrderPropertiesHold checks every dependency precedes its
// dependent, and that repeated calls on the same graph agree exactly.
func TestDeployOrderPropertiesHold(t *testing.T) {
	t.Parallel()
	graph := map[string][]string{
		"api": {"database", "cache"}, "database": {"network"}, "cache": {"network"},
		"worker": {"database", "queue"}, "queue": {"network"}, "network": {},
	}
	first, err := DeployOrder(graph)
	if err != nil {
		t.Fatalf("DeployOrder() error = %v, want nil", err)
	}
	position := make(map[string]int, len(first))
	for i, node := range first {
		position[node] = i
	}
	for node, deps := range graph {
		for _, dep := range deps {
			if position[dep] >= position[node] {
				t.Fatalf("dependency %q does not precede %q in %v", dep, node, first)
			}
		}
	}
	for i := 0; i < 10; i++ {
		got, err := DeployOrder(graph)
		if err != nil || !slices.Equal(got, first) {
			t.Fatalf("run %d: DeployOrder() = %v, %v, want %v, nil", i, got, err, first)
		}
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		stdin     string
		want      string
		wantErr   bool
		wantUsage bool
	}{
		{name: "valid graph prints deploy order",
			stdin: "api: database, cache\ndatabase: network\ncache: network\nnetwork:\n", want: "network\ncache\ndatabase\napi\n"},
		{name: "check flag validates without printing the order",
			args: []string{"-check"}, stdin: "api: database\ndatabase:\n", want: "ok: no cycle\n"},
		{name: "cycle is a non-usage error", stdin: "a: b\nb: a\n", wantErr: true},
		{name: "malformed line is a usage error", stdin: "not-a-line\n", wantErr: true, wantUsage: true},
		{name: "unknown flag is a usage error", args: []string{"-bogus"}, wantErr: true, wantUsage: true},
		{name: "empty input prints nothing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout bytes.Buffer
			err := run(tc.args, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run(%v): want error, got nil", tc.args)
				}
				if tc.wantUsage != errors.Is(err, errUsage) {
					t.Fatalf("run(%v) error = %v, errors.Is(errUsage) = %v, want %v",
						tc.args, err, errors.Is(err, errUsage), tc.wantUsage)
				}
				return
			}
			if err != nil {
				t.Fatalf("run(%v): %v", tc.args, err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run(%v) stdout = %q, want %q", tc.args, stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

`DeployOrder` is correct exactly when every dependency appears before its
dependent in the output for a valid DAG, and when it reports a `*CycleError`
with the actual loop for anything that is not one --
`TestDeployOrderPropertiesHold` checks that property directly rather than
pinning one specific valid order, since a DAG generally has more than one.
The table's self-loop and two-node cycle cases are the sharpest tests of
the three-color logic itself: a two-color (visited/unvisited) implementation
would either miss the cycle entirely or misreport a diamond as one, and
these cases catch either mistake. The same test's ten repeated calls exist
because sorting is easy to forget in exactly one of the two places it is
needed -- the per-node dependency list and the outer starting-node list --
and forgetting either one reintroduces the same map-range nondeterminism
this whole lesson is about. `deployorder` maps every input mistake to exit
code 2 and reserves exit code 1 specifically for a cycle: the graph parsed,
it just has no valid order, which is a different class of failure a caller
handles differently (fix the file vs. fix the actual dependency tangle).
Run `go test -count=1 -race ./...` before trusting any change to the
traversal.

## Resources

- [container/heap package](https://pkg.go.dev/container/heap) — a related graph/ordering primitive, useful context for why an explicit stack (not recursion) is the standard shape for iterative traversals in Go.
- [maps.Keys](https://pkg.go.dev/maps#Keys) and [slices.Sorted](https://pkg.go.dev/slices#Sorted) — collect and pin the deterministic starting-node order `allNodes` relies on.
- [errors.As](https://pkg.go.dev/errors#As) — how the tests extract the `*CycleError`'s `Path` field from the returned error.
- [Topological sorting (Wikipedia)](https://en.wikipedia.org/wiki/Topological_sorting) — the DFS-based algorithm this module implements, including why cycle detection falls directly out of the three-color scheme.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-bidirectional-index-consistency.md](18-bidirectional-index-consistency.md) | Next: [20-approximate-lru-random-sampling.md](20-approximate-lru-random-sampling.md)
