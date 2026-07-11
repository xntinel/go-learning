# 1. Tri-Color Mark and Sweep

Go's garbage collector runs concurrently with your application. The tri-color abstraction — white (unreached), grey (reached but not yet scanned), black (fully scanned) — is the mental model that keeps concurrent collection correct. Without it, you cannot reason about write barriers, GC pauses, or why certain pointer patterns affect collection latency. This lesson builds a user-space tri-color marker so you can trace the invariant from first principles.

```text
tricolor/
  go.mod
  marker.go
  marker_test.go
  cmd/demo/main.go
```

## Concepts

### The Three Colors

Every heap object is conceptually in one of three sets at any moment during the mark phase:

- White: not yet reached. After marking finishes, every white object is unreachable and eligible for collection.
- Grey: reached (a reference exists from a known-live object) but its own outgoing references have not yet been scanned.
- Black: fully scanned. All objects it directly references are at least grey.

The collector begins with all objects white, greys all root objects (globals, stack variables), then drains the grey set: pick any grey object, grey all its white referents, then colour it black. When the grey set is empty, marking is done.

### The Tri-Color Invariant

The invariant that makes concurrent marking safe: a black object must never hold a direct reference to a white object. If this invariant holds throughout the mark phase, no reachable object can be missed, because the only way a black object's referent can remain white is if the collector has not yet seen it — which contradicts the definition of black (fully scanned).

The danger arises when the mutator (your program) modifies the object graph during marking. If a mutator stores a pointer to a white object inside a black object and simultaneously breaks the only grey path to that white object, the invariant breaks and the white object will be swept even though it is still live.

### Marking as a Worklist Drain

The grey set is a worklist. In Go's runtime the worklist is a set of per-P work buffers called `gcWork`; logically it behaves like a queue. The algorithm is:

```
worklist = { root objects }
colour all roots grey
while worklist is not empty:
    obj = worklist.dequeue()
    for ref in obj.references:
        if ref is white:
            colour ref grey
            worklist.enqueue(ref)
    colour obj black
```

This is Dijkstra's original formulation. The runtime runs this concurrently across multiple goroutines, each draining a portion of the worklist.

### Sweep and Reclamation

After marking, the sweep phase reclaims white spans. In Go the sweeper is incremental and lazy: spans are swept on demand as the allocator needs memory, spread across allocation operations rather than in a single stop-the-world pass. The mark phase dominates the GC CPU budget; sweep is cheap.

### Why This Matters in Practice

Knowing the tri-color model helps you:
- Understand write barriers: they are precisely the mechanism that restores the invariant after a mutator pointer write.
- Predict what objects keep others alive unexpectedly (heap retention via long-lived black objects that hold references to otherwise-garbage subtrees).
- Read GC traces: "mark" phase duration, "assist" work, and STW pauses at phase boundaries all derive from how long the worklist drain takes.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/tricolor/cmd/demo
cd ~/go-exercises/tricolor
go mod init example.com/tricolor
```

This is a library verified by `go test`, not a `main` program.

### Exercise 1: Object Graph and Color Types

Create `marker.go`:

```go
package tricolor

import "fmt"

// Color represents the GC color of an object during the mark phase.
type Color int

const (
	White Color = iota // not yet reached
	Grey               // reached, referents not yet scanned
	Black              // fully scanned
)

func (c Color) String() string {
	switch c {
	case White:
		return "white"
	case Grey:
		return "grey"
	case Black:
		return "black"
	default:
		return "unknown"
	}
}

// Object is a node in the simulated heap graph.
type Object struct {
	ID    int
	Color Color
	Refs  []*Object
	Root  bool
}

// Graph holds a set of objects for a single mark-sweep simulation.
type Graph struct {
	Objects []*Object
}

// NewGraph creates a graph with n white objects.
func NewGraph(n int) *Graph {
	g := &Graph{Objects: make([]*Object, n)}
	for i := range g.Objects {
		g.Objects[i] = &Object{ID: i, Color: White}
	}
	return g
}

// AddEdge adds a reference from src to dst.
func (g *Graph) AddEdge(srcID, dstID int) {
	g.Objects[srcID].Refs = append(g.Objects[srcID].Refs, g.Objects[dstID])
}

// SetRoot marks an object as a GC root.
func (g *Graph) SetRoot(id int) {
	g.Objects[id].Root = true
}

// Reset returns all objects to white, preparing the graph for a fresh mark cycle.
func (g *Graph) Reset() {
	for _, o := range g.Objects {
		o.Color = White
	}
}

// LiveCount returns the number of objects that are black after a complete mark phase.
func (g *Graph) LiveCount() int {
	n := 0
	for _, o := range g.Objects {
		if o.Color == Black {
			n++
		}
	}
	return n
}

// GarbageCount returns the number of objects still white after a complete mark phase.
func (g *Graph) GarbageCount() int {
	n := 0
	for _, o := range g.Objects {
		if o.Color == White {
			n++
		}
	}
	return n
}

// SnapshotColors returns a map from object ID to its current color.
func (g *Graph) SnapshotColors() map[int]Color {
	m := make(map[int]Color, len(g.Objects))
	for _, o := range g.Objects {
		m[o.ID] = o.Color
	}
	return m
}

// Marker performs a tri-color mark phase over a Graph.
type Marker struct {
	graph    *Graph
	worklist []*Object
	Steps    []Step // record of each scan step for inspection
}

// Step records the state of one worklist drain iteration.
type Step struct {
	Scanned int           // ID of the object scanned in this step
	Colors  map[int]Color // snapshot of all colors after the scan
}

// NewMarker creates a Marker ready to run over g.
func NewMarker(g *Graph) *Marker {
	return &Marker{graph: g}
}

// Mark executes a complete tri-color mark phase and records each step.
// After Mark returns, all reachable objects are black and all unreachable
// objects remain white.
func (m *Marker) Mark() {
	m.Steps = nil
	m.worklist = m.worklist[:0]

	// Phase 1: grey all roots.
	for _, o := range m.graph.Objects {
		if o.Root {
			o.Color = Grey
			m.worklist = append(m.worklist, o)
		}
	}

	// Phase 2: drain the worklist.
	for len(m.worklist) > 0 {
		// Dequeue the first grey object.
		obj := m.worklist[0]
		m.worklist = m.worklist[1:]

		// Grey all white referents.
		for _, ref := range obj.Refs {
			if ref.Color == White {
				ref.Color = Grey
				m.worklist = append(m.worklist, ref)
			}
		}

		// Blacken the scanned object.
		obj.Color = Black

		m.Steps = append(m.Steps, Step{
			Scanned: obj.ID,
			Colors:  m.graph.SnapshotColors(),
		})
	}
}

// InvariantHolds reports whether the tri-color invariant holds over the current
// graph state: no black object directly references a white object.
func (g *Graph) InvariantHolds() bool {
	for _, o := range g.Objects {
		if o.Color != Black {
			continue
		}
		for _, ref := range o.Refs {
			if ref.Color == White {
				return false
			}
		}
	}
	return true
}

// SimulateInvariantViolation demonstrates what happens when a mutator
// moves a pointer from a grey object to a black object during marking,
// without a write barrier. It returns the ID of the object that is
// incorrectly treated as garbage as a result.
//
// The setup is: root(0) -> A(1) -> B(2). Marking starts: root and A become
// grey, then root is scanned (black). Before A is scanned, a mutator does:
//
//	root.Refs = [B]   (black object now points to white B)
//	A.Refs    = nil   (the only grey path to B is broken)
//
// When A is scanned next, it has no referents. B stays white and is swept
// even though root still points to it.
func SimulateInvariantViolation(g *Graph) (lostID int) {
	g.Reset()
	// Build root -> A -> B.
	g.Objects[0].Root = true
	g.AddEdge(0, 1) // root -> A
	g.AddEdge(1, 2) // A -> B

	// Grey the roots.
	for _, o := range g.Objects {
		if o.Root {
			o.Color = Grey
		}
	}

	worklist := []*Object{g.Objects[0]}

	// Scan root (object 0): grey A, blacken root.
	step := worklist[0]
	worklist = worklist[1:]
	for _, ref := range step.Refs {
		if ref.Color == White {
			ref.Color = Grey
			worklist = append(worklist, ref)
		}
	}
	step.Color = Black

	// --- Mutator runs without a write barrier ---
	// Moves B from A to root: root.Refs = [B], A.Refs = nil.
	g.Objects[0].Refs = []*Object{g.Objects[2]} // black root now holds white B
	g.Objects[1].Refs = nil                     // break the grey path to B

	// Drain remaining worklist: only A (object 1) remains.
	for len(worklist) > 0 {
		obj := worklist[0]
		worklist = worklist[1:]
		for _, ref := range obj.Refs {
			if ref.Color == White {
				ref.Color = Grey
				worklist = append(worklist, ref)
			}
		}
		obj.Color = Black
	}

	// B (object 2) is still white — invariant violated, B will be swept.
	for _, o := range g.Objects {
		if o.Color == White {
			return o.ID
		}
	}
	return -1
}

// PrintStep writes a human-readable description of a Step to stdout.
func PrintStep(s Step) {
	colors := make([]string, 0, len(s.Colors))
	for id := 0; id < len(s.Colors); id++ {
		colors = append(colors, fmt.Sprintf("obj%d=%s", id, s.Colors[id]))
	}
	fmt.Printf("scanned obj%d: %v\n", s.Scanned, colors)
}
```

The key types are `Graph` (the heap model), `Marker` (the worklist drainer), and the `InvariantHolds` predicate.

### Exercise 2: Example Function

Append to `marker.go`:

```go
// ExampleMarker_Mark shows the step-by-step mark phase on a small graph.
// 4 objects: root(0) -> 1 -> 3; root(0) -> 2; object 4 is unreachable.
func ExampleMarker_Mark() {
	g := NewGraph(5)
	g.SetRoot(0)
	g.AddEdge(0, 1)
	g.AddEdge(0, 2)
	g.AddEdge(1, 3)
	// object 4 has no incoming edges — unreachable

	m := NewMarker(g)
	m.Mark()

	fmt.Println("live:", g.LiveCount())
	fmt.Println("garbage:", g.GarbageCount())
	// Output:
	// live: 4
	// garbage: 1
}
```

### Exercise 3: Test the Marker

Create `marker_test.go`:

```go
package tricolor

import (
	"testing"
)

func TestMarkReachableObjectsAreBlack(t *testing.T) {
	t.Parallel()

	// Graph: root(0) -> 1 -> 3; root(0) -> 2; object 4 unreachable.
	g := NewGraph(5)
	g.SetRoot(0)
	g.AddEdge(0, 1)
	g.AddEdge(0, 2)
	g.AddEdge(1, 3)

	m := NewMarker(g)
	m.Mark()

	reachable := []int{0, 1, 2, 3}
	for _, id := range reachable {
		if g.Objects[id].Color != Black {
			t.Errorf("object %d should be black after marking, got %s", id, g.Objects[id].Color)
		}
	}
}

func TestMarkUnreachableObjectsRemainWhite(t *testing.T) {
	t.Parallel()

	g := NewGraph(5)
	g.SetRoot(0)
	g.AddEdge(0, 1)
	// objects 2, 3, 4 unreachable

	m := NewMarker(g)
	m.Mark()

	unreachable := []int{2, 3, 4}
	for _, id := range unreachable {
		if g.Objects[id].Color != White {
			t.Errorf("object %d should be white (garbage), got %s", id, g.Objects[id].Color)
		}
	}
}

func TestMarkEmptyRootSet(t *testing.T) {
	t.Parallel()

	g := NewGraph(4)
	// No roots: all objects should remain white.
	m := NewMarker(g)
	m.Mark()

	if g.LiveCount() != 0 {
		t.Errorf("live count = %d, want 0 (no roots)", g.LiveCount())
	}
	if g.GarbageCount() != 4 {
		t.Errorf("garbage count = %d, want 4", g.GarbageCount())
	}
}

func TestMarkCycleIsHandled(t *testing.T) {
	t.Parallel()

	// Cycle: root(0) -> 1 -> 2 -> 1 (cycle between 1 and 2).
	g := NewGraph(3)
	g.SetRoot(0)
	g.AddEdge(0, 1)
	g.AddEdge(1, 2)
	g.AddEdge(2, 1) // back edge

	m := NewMarker(g)
	m.Mark()

	for _, o := range g.Objects {
		if o.Color != Black {
			t.Errorf("object %d should be black, got %s", o.ID, o.Color)
		}
	}
}

func TestMarkMultipleRoots(t *testing.T) {
	t.Parallel()

	// root(0) -> 1; root(2) -> 3; object 4 unreachable.
	g := NewGraph(5)
	g.SetRoot(0)
	g.SetRoot(2)
	g.AddEdge(0, 1)
	g.AddEdge(2, 3)

	m := NewMarker(g)
	m.Mark()

	if g.LiveCount() != 4 {
		t.Errorf("live count = %d, want 4", g.LiveCount())
	}
	if g.GarbageCount() != 1 {
		t.Errorf("garbage count = %d, want 1 (object 4)", g.GarbageCount())
	}
}

func TestInvariantHoldsAfterCorrectMark(t *testing.T) {
	t.Parallel()

	g := NewGraph(6)
	g.SetRoot(0)
	g.AddEdge(0, 1)
	g.AddEdge(1, 2)
	g.AddEdge(0, 3)

	m := NewMarker(g)
	m.Mark()

	if !g.InvariantHolds() {
		t.Error("tri-color invariant violated after correct mark phase")
	}
}

func TestInvariantViolationDetectedWithoutBarrier(t *testing.T) {
	t.Parallel()

	// Use a graph with at least 3 objects for the violation demo.
	g := NewGraph(3)
	lostID := SimulateInvariantViolation(g)
	if lostID == -1 {
		t.Fatal("SimulateInvariantViolation: expected a lost object, found none")
	}
	// After the simulated violation, the invariant must be broken.
	if g.InvariantHolds() {
		t.Error("expected invariant to be violated after mutation without barrier")
	}
}

func TestMarkRecordsSteps(t *testing.T) {
	t.Parallel()

	// root(0) -> 1 -> 2.
	g := NewGraph(3)
	g.SetRoot(0)
	g.AddEdge(0, 1)
	g.AddEdge(1, 2)

	m := NewMarker(g)
	m.Mark()

	// Three objects are scanned: 0, 1, 2.
	if len(m.Steps) != 3 {
		t.Errorf("step count = %d, want 3", len(m.Steps))
	}
}

func TestGraphReset(t *testing.T) {
	t.Parallel()

	g := NewGraph(3)
	g.SetRoot(0)
	g.AddEdge(0, 1)

	m := NewMarker(g)
	m.Mark()

	g.Reset()
	for _, o := range g.Objects {
		if o.Color != White {
			t.Errorf("object %d should be white after reset, got %s", o.ID, o.Color)
		}
	}
}

// Your turn: write TestMarkLinearChain that builds a chain of 10 objects
// (0->1->2->...->9) with only 0 as root, runs Mark, and asserts all 10
// are black with GarbageCount()==0.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"example.com/tricolor"
	"fmt"
)

func main() {
	// Build an 8-object graph with two reachable subtrees and one isolated object.
	g := tricolor.NewGraph(8)
	g.SetRoot(0)
	g.SetRoot(3)
	g.AddEdge(0, 1)
	g.AddEdge(0, 2)
	g.AddEdge(1, 4)
	g.AddEdge(3, 5)
	g.AddEdge(5, 6)
	// object 7 is unreachable

	m := tricolor.NewMarker(g)
	m.Mark()

	fmt.Printf("Mark complete: %d live, %d garbage\n", g.LiveCount(), g.GarbageCount())
	fmt.Printf("Invariant holds: %v\n", g.InvariantHolds())
	fmt.Println()

	for i, step := range m.Steps {
		fmt.Printf("Step %d: ", i+1)
		tricolor.PrintStep(step)
	}

	// Demonstrate invariant violation.
	fmt.Println()
	fmt.Println("--- Invariant violation (no write barrier) ---")
	g2 := tricolor.NewGraph(3)
	lostID := tricolor.SimulateInvariantViolation(g2)
	fmt.Printf("Lost object ID: %d (swept while still reachable)\n", lostID)
	fmt.Printf("Invariant holds after violation: %v\n", g2.InvariantHolds())
}
```

Run with `go run ./cmd/demo` from the module root.

## Common Mistakes

### Confusing Grey and Black After Enqueue

Wrong: colour an object black when you enqueue it into the worklist (because "you found it").

What happens: the object's referents are never greyed. They remain white and are swept even though they are reachable.

Fix: grey on enqueue, blacken only after scanning all outgoing references. The two-step transition is the definition of the grey set.

### Ignoring Already-Grey Objects When Greying Referents

Wrong: grey a referent unconditionally, even if it is already grey or black, causing it to be scanned multiple times.

What happens: in user-space simulations this is just wasteful. In a real collector with concurrent mutation it can cause liveness bugs if grey-already objects are double-enqueued and one copy is scanned before a necessary write barrier fires.

Fix: only grey objects that are currently white, as in the `if ref.Color == White` guard in `Mark`.

### Breaking the Invariant by Mutating During Marking

Wrong: during a concurrent mark phase, store a pointer to a white object in a black object and simultaneously nil out the only grey path to the white object.

What happens: the collector has already scanned the black object and will not rescan it. The white object is never greyed and is swept at the end of the mark phase — a dangling pointer.

Fix: write barriers (lesson 04) shade the old and new pointer targets on every heap pointer write during the mark phase, restoring the invariant.

### Using `main` + `fmt.Println` as the Sole Verification

Wrong: verifying the marker by running `go run main.go` and reading the output by eye.

What happens: the test suite cannot catch regressions. If `Mark` stops greying some objects, the output changes but CI does not fail.

Fix: the tests in `marker_test.go` own the contract. Each property (reachable=black, unreachable=white, invariant holds, steps recorded) is an assertion that fails automatically on regression.

## Verification

From `~/go-exercises/tricolor`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `ExampleMarker_Mark` function is auto-verified by `go test` against its `// Output:` comment.

## Summary

- White = not yet reached; grey = reached, referents pending; black = fully scanned.
- The tri-color invariant (no black-to-white pointer) guarantees no reachable object is missed.
- Marking drains a worklist: grey roots, then grey-each-referent-and-blacken in a loop.
- The danger in concurrent GC: mutator pointer writes can break the invariant during marking.
- Write barriers (lesson 04) enforce the invariant by shading pointer targets on every heap write during the mark phase.
- Sweep reclaims all white objects; Go does this lazily as the allocator needs memory.

## What's Next

Next: [GC Phases](../02-gc-phases/02-gc-phases.md).

## Resources

- [Go GC Guide](https://tip.golang.org/doc/gc-guide) — the authoritative reference for Go's GC algorithm and tuning
- [Getting to Go: The Journey of Go's Garbage Collector](https://go.dev/blog/ismmkeynote) — Austin Clements' keynote tracing GC design decisions since Go 1.4
- [On-the-Fly Garbage Collection: An Exercise in Cooperation (Dijkstra et al., 1978)](https://dl.acm.org/doi/10.1145/359642.359655) — the original tri-color algorithm paper
- [Go runtime mgc.go source](https://github.com/golang/go/blob/master/src/runtime/mgc.go) — the actual mark phase implementation
