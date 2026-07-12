# Exercise 26: Compute Reachable States Using DFS on State Graph

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A workflow state machine is a directed graph: states are nodes, allowed
transitions are edges. Two questions matter before that machine ever runs
in production. First, is every state you defined actually reachable from
the start state, or did an edit leave dead configuration behind — a state
nothing transitions into anymore? Second, and more dangerous, can a run
that reaches some state ever escape it again, or has an edit accidentally
created a **self-deadlock**: a state, or a cycle of states, from which no
terminal (accepting) state is ever reachable, so a run that lands there is
stuck forever. Both questions are graph reachability, and recursive
depth-first search answers both — forward from the start state for the
first, backward from every terminal state for the second.

This module is fully self-contained: its own `go mod init`, the analysis
inline, its own demo and tests.

## What you'll build

```text
statereach/                   independent module: example.com/statereach
  go.mod                        go 1.24
  statereach.go                  type Graph; Reachable, UnreachableStates, DeadlockStates (recursive DFS)
  statereach_test.go              reachable set, orphan state, self-loop trap, clean graph, mutual-cycle trap, terminal never flagged
  cmd/
    demo/
      main.go                     workflow with an orphan state and a self-loop trap, prints all three analyses
```

- Files: `statereach.go`, `cmd/demo/main.go`, `statereach_test.go`.
- Implement: `Graph{Transitions map[string][]string; Terminal map[string]bool}`, `Reachable(g *Graph, start string) map[string]bool`, `UnreachableStates(g *Graph, start string) []string`, and `DeadlockStates(g *Graph, start string) []string`, each driven by a recursive inner `visit` closure.
- Test: every connected state reached; an orphan state flagged unreachable; a self-loop trap flagged as a deadlock; a clean linear graph with no deadlocks; a mutual two-state cycle with no exit, all flagged; a terminal state with no outgoing transitions never flagged as a deadlock.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/07-recursive-functions-and-stack-depth/26-state-machine-reachability-analysis/cmd/demo
cd go-solutions/04-functions/07-recursive-functions-and-stack-depth/26-state-machine-reachability-analysis
go mod edit -go=1.24
```

### Two DFS passes: forward for reachability, backward for deadlocks

`Reachable` is the straightforward recursive DFS: a named closure `visit`
marks the current state, then calls itself on every state the current one
transitions to, skipping anything already marked. The base case is a state
whose every transition target has already been visited (or which has none)
— the recursion simply stops descending. `UnreachableStates` is not a
second traversal; it takes the complement of `Reachable`'s result against
every state name that appears anywhere in the graph (as a source, a
target, or a declared terminal), so an edit that leaves a state defined but
disconnected is caught even though nothing ever visits it.

`DeadlockStates` answers a harder question, and it is tempting to answer it
with a single forward DFS that tracks "have I seen a terminal state yet"
per branch — but that conflates a node's own reachability to a terminal
with whichever branch of the search happened to visit it first, and it
gets cycles wrong in exactly the case that matters (a cycle that loops
back on itself before ever trying the branch that *would* reach a
terminal). The robust construction instead builds the **reversed**
transition graph and runs a second recursive DFS backward, starting from
every terminal state at once: `canReachTerminalSet` marks a state the
instant the backward walk touches it, meaning some forward path from that
state reaches a terminal. Any state that `Reachable` says a run can enter,
that is not itself terminal, and that the backward walk never touches, is
a genuine trap — every forward path out of it either dead-ends or cycles
forever among other such states.

Create `statereach.go`:

```go
// Package statereach analyzes a workflow state machine's transition graph:
// which states a run can actually reach from its start state, which defined
// states it can never reach at all, and which reachable states are
// self-deadlocks -- states a run can enter but from which no terminal state
// is ever reachable again. All three answers come from recursive
// depth-first traversal, forward over the transition graph and backward
// over its reverse.
package statereach

import "sort"

// Graph is a directed state-transition graph for a workflow state machine.
// Transitions maps a state to the states it can move to. Terminal marks the
// states considered a valid, accepting end of the workflow (a workflow may
// have more than one, e.g. both "completed" and "rejected").
type Graph struct {
	Transitions map[string][]string
	Terminal    map[string]bool
}

// Reachable returns every state reachable from start (start itself
// included), found by a recursive depth-first walk of the transition graph.
func Reachable(g *Graph, start string) map[string]bool {
	visited := make(map[string]bool)
	var visit func(state string)
	visit = func(state string) {
		if visited[state] {
			return
		}
		visited[state] = true
		for _, next := range g.Transitions[state] {
			visit(next)
		}
	}
	visit(start)
	return visited
}

// UnreachableStates returns, sorted, every state named anywhere in g (as a
// transition source, a transition target, or a terminal state) that
// Reachable(g, start) never visits. These are states a run starting at
// start can never enter -- typically dead configuration left behind by a
// workflow edit.
func UnreachableStates(g *Graph, start string) []string {
	reachable := Reachable(g, start)

	all := make(map[string]bool)
	for state, nexts := range g.Transitions {
		all[state] = true
		for _, n := range nexts {
			all[n] = true
		}
	}
	for state := range g.Terminal {
		all[state] = true
	}

	var out []string
	for state := range all {
		if !reachable[state] {
			out = append(out, state)
		}
	}
	sort.Strings(out)
	return out
}

// canReachTerminalSet recursively walks the reversed transition graph
// backward from every terminal state, marking each state it touches as
// able to reach a terminal state.
func canReachTerminalSet(g *Graph) map[string]bool {
	reverse := make(map[string][]string)
	for state, nexts := range g.Transitions {
		for _, n := range nexts {
			reverse[n] = append(reverse[n], state)
		}
	}

	canReach := make(map[string]bool)
	var visit func(state string)
	visit = func(state string) {
		if canReach[state] {
			return
		}
		canReach[state] = true
		for _, prev := range reverse[state] {
			visit(prev)
		}
	}
	for terminal := range g.Terminal {
		visit(terminal)
	}
	return canReach
}

// DeadlockStates returns, sorted, every state reachable from start that is
// not itself terminal and cannot reach any terminal state -- a
// self-deadlock the workflow can enter and never leave (a run stuck
// looping among non-terminal states forever). Detection recurses backward
// from every terminal state over the reversed graph; any reachable,
// non-terminal state that backward walk never touches is a trap.
func DeadlockStates(g *Graph, start string) []string {
	reachable := Reachable(g, start)
	canReach := canReachTerminalSet(g)

	var out []string
	for state := range reachable {
		if g.Terminal[state] {
			continue
		}
		if !canReach[state] {
			out = append(out, state)
		}
	}
	sort.Strings(out)
	return out
}
```

### The runnable demo

The demo graph mirrors a document-approval workflow with two problems
planted deliberately: `archived_legacy` is defined but nothing transitions
into it (unreachable), and `needs_review` transitions only to itself, so
any run that lands there never reaches `completed` or `rejected`
(deadlock).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/statereach"
)

func main() {
	g := &statereach.Graph{
		Transitions: map[string][]string{
			"created":         {"validating"},
			"validating":      {"approved", "rejected", "needs_review"},
			"needs_review":    {"needs_review"}, // trap: no way out once entered
			"approved":        {"completed"},
			"rejected":        {},
			"completed":       {},
			"archived_legacy": {"archived_legacy"}, // orphan, unreachable from "created"
		},
		Terminal: map[string]bool{"completed": true, "rejected": true},
	}

	reachable := statereach.Reachable(g, "created")
	var reachedList []string
	for state := range reachable {
		reachedList = append(reachedList, state)
	}
	sort.Strings(reachedList)
	fmt.Println("reachable:", reachedList)

	fmt.Println("unreachable:", statereach.UnreachableStates(g, "created"))
	fmt.Println("deadlocks:", statereach.DeadlockStates(g, "created"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
reachable: [approved completed created needs_review rejected validating]
unreachable: [archived_legacy]
deadlocks: [needs_review]
```

### Tests

`TestReachableVisitsAllConnectedStates` and `TestUnreachableStatesFindsOrphan`
check the forward pass against the demo graph. `TestDeadlockStatesDetectsSelfLoopTrap`
pins the demo's `needs_review` trap. `TestDeadlockStatesEmptyWhenAllPathsTerminate`
is the negative case: a clean linear graph must report no deadlocks.
`TestDeadlockStatesDetectsMutualCycleTrap` is the harder case a
forward-only, single-pass detector tends to get wrong: two states looping
on each other, with the state feeding into them also unable to escape, all
three must be reported. `TestDeadlockStatesNeverFlagsTerminalItself` guards
the edge every implementation must not get backwards: a terminal state
naturally has no outgoing transitions, and that alone must never make it a
deadlock.

Create `statereach_test.go`:

```go
package statereach

import (
	"reflect"
	"testing"
)

func demoGraph() *Graph {
	return &Graph{
		Transitions: map[string][]string{
			"created":         {"validating"},
			"validating":      {"approved", "rejected", "needs_review"},
			"needs_review":    {"needs_review"},
			"approved":        {"completed"},
			"rejected":        {},
			"completed":       {},
			"archived_legacy": {"archived_legacy"},
		},
		Terminal: map[string]bool{"completed": true, "rejected": true},
	}
}

func TestReachableVisitsAllConnectedStates(t *testing.T) {
	t.Parallel()

	got := Reachable(demoGraph(), "created")
	want := []string{"created", "validating", "approved", "rejected", "needs_review", "completed"}
	for _, s := range want {
		if !got[s] {
			t.Errorf("Reachable missing %q", s)
		}
	}
	if got["archived_legacy"] {
		t.Error("Reachable should not include archived_legacy (orphan, not connected to created)")
	}
}

func TestUnreachableStatesFindsOrphan(t *testing.T) {
	t.Parallel()

	got := UnreachableStates(demoGraph(), "created")
	want := []string{"archived_legacy"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UnreachableStates() = %v, want %v", got, want)
	}
}

func TestDeadlockStatesDetectsSelfLoopTrap(t *testing.T) {
	t.Parallel()

	got := DeadlockStates(demoGraph(), "created")
	want := []string{"needs_review"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeadlockStates() = %v, want %v", got, want)
	}
}

func TestDeadlockStatesEmptyWhenAllPathsTerminate(t *testing.T) {
	t.Parallel()

	g := &Graph{
		Transitions: map[string][]string{
			"start":  {"middle"},
			"middle": {"done"},
			"done":   {},
		},
		Terminal: map[string]bool{"done": true},
	}
	got := DeadlockStates(g, "start")
	if len(got) != 0 {
		t.Fatalf("DeadlockStates() = %v, want empty", got)
	}
}

func TestDeadlockStatesDetectsMutualCycleTrap(t *testing.T) {
	t.Parallel()

	// "start" itself feeds straight into the a<->b cycle with no route to
	// "done", so all three of start, a, and b are unrecoverable traps.
	g := &Graph{
		Transitions: map[string][]string{
			"start": {"a"},
			"a":     {"b"},
			"b":     {"a"},
			"done":  {},
		},
		Terminal: map[string]bool{"done": true},
	}
	got := DeadlockStates(g, "start")
	want := []string{"a", "b", "start"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DeadlockStates() = %v, want %v", got, want)
	}
}

func TestDeadlockStatesNeverFlagsTerminalItself(t *testing.T) {
	t.Parallel()

	g := &Graph{
		Transitions: map[string][]string{
			"start": {"end"},
			"end":   {},
		},
		Terminal: map[string]bool{"end": true},
	}
	got := DeadlockStates(g, "start")
	if len(got) != 0 {
		t.Fatalf("DeadlockStates() = %v, want empty (terminal has no outgoing transitions but is not a deadlock)", got)
	}
}
```

## Review

The analysis is correct when `Reachable` matches a run's true set of
possible states, `UnreachableStates` flags exactly the configuration
nothing points to, and `DeadlockStates` flags exactly the reachable,
non-terminal states with no forward path to any terminal — no more, no
less. `TestDeadlockStatesDetectsMutualCycleTrap` is the test that would
fail on the tempting-but-wrong design of computing "can reach terminal" as
a single forward DFS with an in-progress/on-stack flag treated as a
conclusive "no": that design can under- or over-report depending on branch
order, because a state's true answer depends on the whole graph, not on
which of its neighbors got explored first. Building the reversed graph and
recursing backward from every terminal state sidesteps that entirely — a
state either has some path forward to a terminal, discoverable by walking
backward from that terminal, or it does not, independent of traversal
order. `TestDeadlockStatesNeverFlagsTerminalItself` guards the adjacent
mistake of forgetting that a terminal's own missing outgoing edges are by
definition not a trap.

## Resources

- [Wikipedia: Reachability (graph theory)](https://en.wikipedia.org/wiki/Reachability)
- [Wikipedia: Depth-first search](https://en.wikipedia.org/wiki/Depth-first_search)
- [Go Specification: Function declarations](https://go.dev/ref/spec#Function_declarations)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [25-jwt-claim-nesting-depth-validation.md](25-jwt-claim-nesting-depth-validation.md) | Next: [27-distributed-trace-span-aggregation.md](27-distributed-trace-span-aggregation.md)
