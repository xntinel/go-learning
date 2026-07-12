# 4. Write Barriers and GC Invariants

A concurrent garbage collector faces a fundamental problem: while it scans the object graph, your program keeps modifying it. Without a mechanism to track those modifications, the collector can miss a reachable object — a correctness bug that corrupts memory. Write barriers are compiler-inserted code fragments at every heap pointer write that notify the GC of graph mutations, maintaining the tri-color invariant. This lesson implements user-space models of the three barrier families so you can reason about what each one protects against and why Go's hybrid barrier is necessary.

```text
writebarrier/
  go.mod
  barrier.go
  barrier_test.go
  cmd/demo/main.go
```

## Concepts

### The Problem: Concurrent Mutation Breaks the Invariant

Recall the tri-color invariant: no black object may directly reference a white object. This holds trivially when the mutator is stopped. It can break when the mutator runs concurrently with the mark phase.

The classic lost-object scenario:

1. Object A (black) and object B (grey) are in the graph. B holds a reference to C (white).
2. Mutator does: `A.ref = C; B.ref = nil`. Now A (black) holds C (white). B no longer holds C.
3. When the marker finishes draining B (already in progress), it finds no outgoing references. It turns B black.
4. C is never greyed. C remains white. Sweep reclaims C. A now holds a dangling pointer.

The fix must shade (grey) at least one of the pointers involved in the write. There are two families.

### Dijkstra Insertion Barrier (shade-on-write)

On every pointer write `slot = new`, grey the new value before writing:

```
dijkstra_barrier(slot, new):
    grey(new)
    *slot = new
```

This prevents a black object from ever acquiring a direct reference to a white object, because the new value is greyed immediately. However, it does not protect against deletion: if you write `*slot = nil` and the old value was the only grey path to some object, that object becomes unreachable from the grey frontier. Dijkstra's barrier requires a stack re-scan at mark termination to find objects that became unreachable this way.

### Yuasa Deletion Barrier (shade-on-delete)

On every pointer write `slot = new`, grey the old value being overwritten:

```
yuasa_barrier(slot, new):
    grey(*slot)   // shade old
    *slot = new
```

This implements snapshot-at-the-beginning (SATB): every object that existed at the start of the mark phase is treated as live. This prevents losing an object when its only reference is overwritten. However, it can retain objects that became garbage during the mark phase — they are collected in the next cycle, not the current one.

### Go's Hybrid Barrier (Go 1.8+)

Go's barrier combines both:

```
hybrid_barrier(slot, new):
    grey(*slot)   // shade old  (Yuasa component)
    grey(new)     // shade new  (Dijkstra component)
    *slot = new
```

The Dijkstra component prevents the black-to-white write. The Yuasa component ensures that deleting the old reference does not lose a not-yet-scanned object. Together they eliminate the need to re-scan goroutine stacks at mark termination, which was the main source of mark-termination STW latency before Go 1.8.

Write barriers are only active during the mark phase. Outside of marking, pointer writes have zero overhead. The compiler inserts barrier calls at every heap pointer write; stack pointer writes do not use barriers because stacks are scanned precisely at mark initiation.

### Observing Write Barrier Cost

Write barriers add a small but measurable overhead to pointer-write-heavy code. You can observe this by running benchmarks with `GOGC=off` (barriers rarely active because GC never starts a mark phase) versus `GOGC=1` (barriers very frequently active). The difference is the write barrier overhead. In practice it is 1–5 ns per pointer write on modern hardware.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/35-runtime-garbage-collector/04-write-barriers/04-write-barriers/cmd/demo
cd go-solutions/35-runtime-garbage-collector/04-write-barriers/04-write-barriers
```

### Exercise 1: Object Graph and Barrier Implementations

Create `barrier.go`:

```go
package writebarrier

import (
	"fmt"
)

// Color is the tri-color mark state of an object.
type Color int

const (
	White Color = iota
	Grey
	Black
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

// Object is a heap node in the simulation.
type Object struct {
	ID    int
	Color Color
	Ref   *Object // single outgoing reference for clarity
}

// shade greys an object if it is currently white.
func shade(o *Object) {
	if o != nil && o.Color == White {
		o.Color = Grey
	}
}

// BarrierKind identifies which barrier implementation to use.
type BarrierKind int

const (
	NoBarrier       BarrierKind = iota // unsafe: no barrier
	DijkstraBarrier                    // shade-on-write (insertion barrier)
	YuasaBarrier                       // shade-on-delete (deletion/SATB barrier)
	HybridBarrier                      // shade old and new (Go's actual barrier)
)

// WriteRef simulates a pointer write with the specified barrier.
// slot is the address of the pointer field being updated; newVal is the value
// being written. The barrier fires only during the mark phase (markActive=true).
func WriteRef(slot **Object, newVal *Object, kind BarrierKind, markActive bool) {
	if !markActive {
		*slot = newVal
		return
	}
	switch kind {
	case NoBarrier:
		*slot = newVal

	case DijkstraBarrier:
		shade(newVal) // grey the new target
		*slot = newVal

	case YuasaBarrier:
		shade(*slot) // grey the old value before overwriting
		*slot = newVal

	case HybridBarrier:
		shade(*slot)  // Yuasa: grey old
		shade(newVal) // Dijkstra: grey new
		*slot = newVal
	}
}

// ScenarioResult records what happened to the target object in the lost-object
// scenario for each barrier kind.
type ScenarioResult struct {
	Kind     BarrierKind
	TargetID int  // ID of object C in the scenario
	Survived bool // true if C is black (alive) after marking
}

// RunLostObjectScenario simulates the classic lost-object scenario for the
// given barrier kind and returns whether the target object survived.
//
// Setup:
//
//	A (black) has no outgoing reference.
//	B (grey, in worklist) holds a reference to C (white).
//
// Mutation during marking (before B is scanned):
//
//	A.Ref = C  (black object acquires white object)
//	B.Ref = nil  (the only grey path to C is broken)
//
// If the barrier correctly shades C or B's old reference, C survives.
// Without a barrier, C remains white and is swept.
func RunLostObjectScenario(kind BarrierKind) ScenarioResult {
	A := &Object{ID: 0, Color: Black}
	B := &Object{ID: 1, Color: Grey}
	C := &Object{ID: 2, Color: White}
	B.Ref = C

	// Simulate the mutator writing A.Ref = C and B.Ref = nil during marking.
	WriteRef(&A.Ref, C, kind, true)   // A (black) acquires C (white)
	WriteRef(&B.Ref, nil, kind, true) // B's reference to C is deleted

	// Drain the remaining mark worklist: B is grey, scan it.
	// B.Ref is now nil (or shaded), so no new objects are greyed from B.
	if B.Color == Grey {
		// Scan B: grey all white referents (none if barrier deleted them).
		if B.Ref != nil && B.Ref.Color == White {
			B.Ref.Color = Grey
		}
		B.Color = Black
	}

	// If A is black and holds C, and C was shaded by the barrier, C is not white.
	// For DijkstraBarrier: C was greyed when A.Ref = C was written.
	// For YuasaBarrier: C was greyed when B.Ref = nil was written (old = C).
	// For HybridBarrier: both.
	// For NoBarrier: C remains white.

	// Simulate remaining mark work: drain any grey objects.
	for _, o := range []*Object{A, B, C} {
		if o.Color == Grey {
			if o.Ref != nil && o.Ref.Color == White {
				o.Ref.Color = Grey
			}
			o.Color = Black
		}
	}

	return ScenarioResult{
		Kind:     kind,
		TargetID: C.ID,
		Survived: C.Color == Black,
	}
}

// BarrierName returns a human-readable name for a BarrierKind.
func BarrierName(k BarrierKind) string {
	switch k {
	case NoBarrier:
		return "NoBarrier"
	case DijkstraBarrier:
		return "Dijkstra"
	case YuasaBarrier:
		return "Yuasa"
	case HybridBarrier:
		return "Hybrid"
	default:
		return fmt.Sprintf("unknown(%d)", k)
	}
}
```

### Exercise 2: Example Function

Append to `barrier.go`:

```go
// ExampleRunLostObjectScenario shows that every barrier except NoBarrier
// correctly saves the target object from premature collection.
func ExampleRunLostObjectScenario() {
	for _, k := range []BarrierKind{NoBarrier, DijkstraBarrier, YuasaBarrier, HybridBarrier} {
		r := RunLostObjectScenario(k)
		fmt.Printf("%s: survived=%v\n", BarrierName(k), r.Survived)
	}
	// Output:
	// NoBarrier: survived=false
	// Dijkstra: survived=true
	// Yuasa: survived=true
	// Hybrid: survived=true
}
```

### Exercise 3: Tests

Create `barrier_test.go`:

```go
package writebarrier

import (
	"testing"
)

func TestNoBarrierLosesObject(t *testing.T) {
	t.Parallel()

	r := RunLostObjectScenario(NoBarrier)
	if r.Survived {
		t.Errorf("NoBarrier: expected object %d to be lost (white), but it survived", r.TargetID)
	}
}

func TestDijkstraBarrierSavesObject(t *testing.T) {
	t.Parallel()

	r := RunLostObjectScenario(DijkstraBarrier)
	if !r.Survived {
		t.Errorf("DijkstraBarrier: object %d should survive (new value is shaded)", r.TargetID)
	}
}

func TestYuasaBarrierSavesObject(t *testing.T) {
	t.Parallel()

	r := RunLostObjectScenario(YuasaBarrier)
	if !r.Survived {
		t.Errorf("YuasaBarrier: object %d should survive (old value is shaded)", r.TargetID)
	}
}

func TestHybridBarrierSavesObject(t *testing.T) {
	t.Parallel()

	r := RunLostObjectScenario(HybridBarrier)
	if !r.Survived {
		t.Errorf("HybridBarrier: object %d should survive (both old and new are shaded)", r.TargetID)
	}
}

func TestWriteRefNoBarrierOutsideMark(t *testing.T) {
	t.Parallel()

	a := &Object{ID: 0, Color: White}
	b := &Object{ID: 1, Color: White}

	var slot *Object
	WriteRef(&slot, a, NoBarrier, false) // mark not active
	if slot != a {
		t.Errorf("slot should be a after write, got %v", slot)
	}

	// Colors must not change when mark is not active.
	if a.Color != White || b.Color != White {
		t.Error("colors changed outside mark phase")
	}
	_ = b
}

func TestWriteRefDijkstraGreysNewValue(t *testing.T) {
	t.Parallel()

	target := &Object{ID: 0, Color: White}
	var slot *Object

	WriteRef(&slot, target, DijkstraBarrier, true) // mark active
	if target.Color != Grey {
		t.Errorf("DijkstraBarrier: new value should be grey after write, got %s", target.Color)
	}
}

func TestWriteRefYuasaGreysOldValue(t *testing.T) {
	t.Parallel()

	old := &Object{ID: 0, Color: White}
	slot := old
	WriteRef(&slot, nil, YuasaBarrier, true) // mark active
	if old.Color != Grey {
		t.Errorf("YuasaBarrier: old value should be grey after overwrite, got %s", old.Color)
	}
}

func TestWriteRefHybridGreysOldAndNew(t *testing.T) {
	t.Parallel()

	old := &Object{ID: 0, Color: White}
	newObj := &Object{ID: 1, Color: White}
	slot := old

	WriteRef(&slot, newObj, HybridBarrier, true)
	if old.Color != Grey {
		t.Errorf("HybridBarrier: old value should be grey, got %s", old.Color)
	}
	if newObj.Color != Grey {
		t.Errorf("HybridBarrier: new value should be grey, got %s", newObj.Color)
	}
}

func TestShadeDoesNotRegressBlackToGrey(t *testing.T) {
	t.Parallel()

	o := &Object{ID: 0, Color: Black}
	var slot *Object
	// Hybrid barrier on a black object's reference: shading Black should be a no-op.
	WriteRef(&slot, o, HybridBarrier, true)
	if o.Color != Black {
		t.Errorf("shade should not change a black object to grey, got %s", o.Color)
	}
}

// Your turn: write TestAllBarriersExceptNoBarrierProtectObject that iterates
// over []BarrierKind{DijkstraBarrier, YuasaBarrier, HybridBarrier} and asserts
// RunLostObjectScenario returns Survived==true for each.
```

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/writebarrier"
)

func main() {
	fmt.Println("Write barrier simulation: lost-object scenario")
	fmt.Println()
	fmt.Println("Setup: A (black) has no ref; B (grey) -> C (white).")
	fmt.Println("Mutation: A.Ref = C; B.Ref = nil (during mark phase).")
	fmt.Println()

	barriers := []writebarrier.BarrierKind{
		writebarrier.NoBarrier,
		writebarrier.DijkstraBarrier,
		writebarrier.YuasaBarrier,
		writebarrier.HybridBarrier,
	}

	for _, k := range barriers {
		r := writebarrier.RunLostObjectScenario(k)
		status := "LOST (premature collection)"
		if r.Survived {
			status = "OK   (correctly retained)"
		}
		fmt.Printf("  %-14s  object %d: %s\n", writebarrier.BarrierName(k), r.TargetID, status)
	}

	fmt.Println()
	fmt.Println("Notes:")
	fmt.Println("  - Go uses the Hybrid barrier since Go 1.8.")
	fmt.Println("  - Barriers are only active during the mark phase.")
	fmt.Println("  - Stack writes do not use barriers; stacks are scanned at mark initiation.")
	fmt.Println("  - Use: go build -gcflags='-d=wb' to see barrier insertion decisions.")
}
```

## Common Mistakes

### Thinking Write Barriers Are Always Active

Wrong: assuming every pointer write in your program incurs write barrier overhead, even in steady state.

What happens: unnecessary concern about pointer-heavy data structures that are perfectly fine outside the mark phase.

Fix: write barriers are only active during the GC mark phase. The compiler generates a conditional check (testing a global flag) that is branch-predicted correctly in the common case (mark not active). Outside the mark phase the overhead is one branch prediction, not a full barrier.

### Confusing Stack and Heap Writes

Wrong: applying the barrier analysis to pointer writes on the stack (local variables, function arguments).

What happens: confused mental model. Stack objects are not managed by the heap allocator and do not participate in heap barriers.

Fix: write barriers apply to heap pointer writes. Stack frames are scanned precisely at the start of each mark phase (when the runtime knows the exact set of live pointers per goroutine). Stack writes between two mark phases are safe because the next mark will scan the stack again.

### Believing the Dijkstra Barrier Alone Is Sufficient in Go

Wrong: assuming the insertion barrier is all that is needed since it prevents black-to-white direct writes.

What happens: without the Yuasa component, the runtime must re-scan all goroutine stacks at mark termination to catch objects deleted from grey paths. Pre-Go-1.8, this re-scan was the dominant source of mark-termination STW latency.

Fix: Go's hybrid barrier adds the Yuasa deletion component, ensuring SATB semantics. This eliminates the need for stack re-scanning at mark termination, reducing STW from milliseconds to microseconds.

### Expecting Zero-Allocation Pointer Writes to Have Zero Barrier Cost

Wrong: assuming that because pointer writes in a benchmark show 0 allocs/op, they have zero GC-related cost.

What happens: the write barrier is not an allocation; it is extra instructions at the write site. It does not appear in alloc/op counts. It appears in CPU cycles.

Fix: measure with `GOGC=1` (barrier always active) vs `GOGC=off` (barrier rarely active). The difference in ns/op is the barrier overhead.

## Verification

From `~/go-exercises/writebarrier`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

To observe real write barrier insertion in Go code:

```bash
go build -gcflags='-d=wb' ./... 2>&1 | head -20
```

## Summary

- Write barriers are compiler-inserted code at heap pointer writes that notify the GC of graph mutations.
- The tri-color invariant (no black-to-white pointer) breaks without barriers in concurrent marking.
- Dijkstra insertion barrier: grey the new target on write (prevents black acquiring white).
- Yuasa deletion barrier: grey the old value on overwrite (SATB: prevents losing a not-yet-scanned object).
- Go uses a hybrid barrier since Go 1.8: grey both old and new, eliminating the need for stack re-scanning at mark termination.
- Barriers are only active during the mark phase; outside marking, pointer writes have near-zero overhead.

## What's Next

Next: [Observing GC with GODEBUG](../05-observing-gc-godebug/05-observing-gc-godebug.md).

## Resources

- [Proposal: Eliminate STW stack re-scanning](https://github.com/golang/proposal/blob/master/design/17503-eliminate-rescan.md) — the design document for Go's hybrid barrier
- [Go 1.8 Release Notes: GC improvements](https://go.dev/doc/go1.8#gc) — official announcement of the hybrid barrier
- [On-the-Fly Garbage Collection (Dijkstra et al., 1978)](https://dl.acm.org/doi/10.1145/359642.359655) — the original insertion barrier paper
- [Real-Time Garbage Collection on General-Purpose Machines (Yuasa, 1990)](https://dl.acm.org/doi/10.1145/155090.155099) — the deletion/SATB barrier paper
