# Exercise 1: Partition Assignment Strategies

A consumer group's first job is to decide who reads what. This exercise builds the three classic partition assignors - Range, RoundRobin, and Sticky - as pure functions from `(partitions, consumers)` to an assignment map, plus a `Moved` helper that measures the one property that separates a good rebalance from a bad one: how many already-owned partitions changed hands.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
assignment.go        Assignor interface; RangeAssignor, RoundRobinAssignor, StickyAssignor; Moved
cmd/
  demo/
    main.go          assign 6 partitions 3 ways, then drop a consumer and compare movement
assignment_test.go   even/remainder/single-consumer splits, sticky preservation, movement bounds
```

- Files: `assignment.go`, `cmd/demo/main.go`, `assignment_test.go`.
- Implement: `Assignor` and the three concrete assignors, plus `Moved(prev, next)`.
- Test: even and remainder splits, the single-consumer case, sticky preservation, and that sticky moves strictly fewer partitions than an eager recompute.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p partition-assignment/cmd/demo && cd partition-assignment
go mod init example.com/assignment
```

### Why an assignor is a pure function, and what "moved" means

An assignor takes the current set of partitions and the current set of consumers and returns a map from consumer to the partitions it should own. Making it a pure function - no state, no I/O - is what makes it testable and what lets a coordinator swap strategies by configuration. The previous assignment is passed in as a third argument so a sticky assignor can consult it; the two stateless assignors ignore it. Every assignor sorts its inputs first, so its output is deterministic regardless of map iteration order - a property the tests depend on.

Range walks the sorted consumers and hands each a contiguous slice of the sorted partitions. With `N` partitions and `M` consumers, the block size is `N/M`, and the first `N mod M` consumers each take one extra partition to absorb the remainder. RoundRobin instead deals partitions out one at a time, `partition i` to `consumer i mod M`, which spreads any remainder evenly instead of piling it on the first consumer. Both are "eager": they compute the assignment from scratch and ignore whatever came before.

Sticky is different in kind. It starts from the previous assignment, keeps every partition that is still valid and still owned by a consumer that is still present, then collects the leftover partitions - the ones orphaned by a departed consumer, or freshly added - and hands each to the currently least-loaded survivor. The result is balanced and, crucially, barely changed from the input.

`Moved` is how you prove that. It builds the `partition -> owner` map for both the previous and the next assignment and counts the partitions that were owned by someone before and are owned by a different consumer (or no one) after. Partitions that are new in `next` - present now, unowned before - are first assignment, not movement, and are not counted. This is the metric that matters operationally: every moved partition forces its new owner to rebuild per-partition state from the committed offset, so an assignor that scores low on `Moved` is an assignor that keeps caches warm.

Create `assignment.go`:

```go
package assignment

import "sort"

// Assignor computes a partition-to-consumer mapping for a rebalance.
// prev is the assignment from the previous generation; sticky assignors use it
// to minimize partition movement. Implementations must not modify prev.
type Assignor interface {
	Assign(partitions []int, consumers []string, prev map[string][]int) map[string][]int
}

// RangeAssignor assigns contiguous ranges of sorted partitions to sorted
// consumers. When the count is not evenly divisible, the first (remainder)
// consumers each receive one extra partition.
type RangeAssignor struct{}

func (RangeAssignor) Assign(partitions []int, consumers []string, _ map[string][]int) map[string][]int {
	ps := intsSorted(partitions)
	cs := strsSorted(consumers)
	result := make(map[string][]int, len(cs))
	if len(cs) == 0 {
		return result
	}
	n, c := len(ps), len(cs)
	base, rem := n/c, n%c
	idx := 0
	for i, consumer := range cs {
		count := base
		if i < rem {
			count++
		}
		result[consumer] = append([]int(nil), ps[idx:idx+count]...)
		idx += count
	}
	return result
}

// RoundRobinAssignor distributes sorted partitions across sorted consumers in
// rotation. Each consumer receives floor(N/M) or ceil(N/M) partitions.
type RoundRobinAssignor struct{}

func (RoundRobinAssignor) Assign(partitions []int, consumers []string, _ map[string][]int) map[string][]int {
	ps := intsSorted(partitions)
	cs := strsSorted(consumers)
	result := make(map[string][]int, len(cs))
	if len(cs) == 0 {
		return result
	}
	for i, p := range ps {
		c := cs[i%len(cs)]
		result[c] = append(result[c], p)
	}
	return result
}

// StickyAssignor keeps the previous assignment for surviving consumers and only
// redistributes partitions orphaned by departed consumers. This minimizes the
// number of partitions that move on rebalance.
type StickyAssignor struct{}

func (StickyAssignor) Assign(partitions []int, consumers []string, prev map[string][]int) map[string][]int {
	pset := make(map[int]bool, len(partitions))
	for _, p := range partitions {
		pset[p] = true
	}
	cset := make(map[string]bool, len(consumers))
	for _, c := range consumers {
		cset[c] = true
	}

	result := make(map[string][]int, len(consumers))
	claimed := make(map[int]bool, len(partitions))

	// Carry over previous assignments for consumers that are still alive.
	for consumer, parts := range prev {
		if !cset[consumer] {
			continue
		}
		for _, p := range parts {
			if pset[p] && !claimed[p] {
				result[consumer] = append(result[consumer], p)
				claimed[p] = true
			}
		}
	}

	// Collect partitions not yet claimed (orphaned by departed consumers or
	// newly added) and hand each to the least-loaded survivor.
	var orphans []int
	for _, p := range partitions {
		if !claimed[p] {
			orphans = append(orphans, p)
		}
	}
	sort.Ints(orphans)
	cs := strsSorted(consumers)
	for _, p := range orphans {
		if len(cs) == 0 {
			break
		}
		pick := leastLoaded(cs, result)
		result[pick] = append(result[pick], p)
	}
	for c := range result {
		sort.Ints(result[c])
	}
	return result
}

func leastLoaded(consumers []string, assignment map[string][]int) string {
	best := consumers[0]
	for _, c := range consumers[1:] {
		if len(assignment[c]) < len(assignment[best]) {
			best = c
		}
	}
	return best
}

// Moved counts how many already-owned partitions changed hands between prev and
// next. A partition that was owned by some consumer in prev counts as moved if
// next assigns it to a different consumer (or to none). Partitions that are new
// in next (unowned in prev) are not movement; they are first assignment.
func Moved(prev, next map[string][]int) int {
	owner := func(m map[string][]int) map[int]string {
		out := make(map[int]string)
		for c, parts := range m {
			for _, p := range parts {
				out[p] = c
			}
		}
		return out
	}
	po, no := owner(prev), owner(next)
	moved := 0
	for p, c := range po {
		if no[p] != c {
			moved++
		}
	}
	return moved
}

func intsSorted(s []int) []int {
	out := append([]int(nil), s...)
	sort.Ints(out)
	return out
}

func strsSorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
```

Read `StickyAssignor.Assign` as two passes. The first pass carries forward: for every consumer in the previous assignment that is still alive, it keeps that consumer's partitions that are still valid and not already claimed. The second pass repairs: it gathers every partition no surviving consumer kept, sorts them for determinism, and gives each to the least-loaded survivor so the leftover load spreads evenly. `Moved` then quantifies the win - it counts only partitions that were owned before and changed owner, so a brand-new partition does not inflate the score.

### The runnable demo

The demo makes the movement difference concrete. It assigns six partitions across three consumers under Range and RoundRobin, then drops one consumer and compares two ways of repairing the assignment: an eager RoundRobin recompute versus a sticky rebalance. The `Moved` counts show why sticky exists.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/assignment"
)

func main() {
	partitions := []int{0, 1, 2, 3, 4, 5}
	consumers := []string{"c0", "c1", "c2"}

	rng := assignment.RangeAssignor{}.Assign(partitions, consumers, nil)
	rr := assignment.RoundRobinAssignor{}.Assign(partitions, consumers, nil)

	fmt.Println("6 partitions, 3 consumers")
	fmt.Println("Range:")
	for _, c := range consumers {
		fmt.Printf("  %s: %v\n", c, rng[c])
	}
	fmt.Println("RoundRobin:")
	for _, c := range consumers {
		fmt.Printf("  %s: %v\n", c, rr[c])
	}

	// c2 leaves. Compare how many partitions move under an eager recompute
	// (RoundRobin from scratch) versus a sticky rebalance.
	survivors := []string{"c0", "c1"}
	eager := assignment.RoundRobinAssignor{}.Assign(partitions, survivors, nil)
	sticky := assignment.StickyAssignor{}.Assign(partitions, survivors, rr)

	fmt.Println("\nc2 leaves; redistribute its partitions")
	fmt.Printf("  eager RoundRobin moved %d partitions\n", assignment.Moved(rr, eager))
	fmt.Printf("  sticky moved %d partitions\n", assignment.Moved(rr, sticky))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
6 partitions, 3 consumers
Range:
  c0: [0 1]
  c1: [2 3]
  c2: [4 5]
RoundRobin:
  c0: [0 3]
  c1: [1 4]
  c2: [2 5]

c2 leaves; redistribute its partitions
  eager RoundRobin moved 4 partitions
  sticky moved 2 partitions
```

The eager recompute moves four of six partitions even though only one consumer left; sticky moves exactly the two that consumer owned. That four-versus-two gap is the whole argument for sticky assignment, and it widens as the partition count grows.

### Tests

The tests pin each assignor's contract. The Range tests cover an even split and the remainder case where the first consumer takes the extra; the single-consumer test (the "everything to one owner" base case) confirms a lone consumer owns all partitions. The RoundRobin test pins the interleaving. The Sticky tests assert that survivors keep their partitions and that `Moved` is exactly the count of orphans - and a head-to-head test asserts sticky moves strictly fewer partitions than an eager recompute over twelve partitions. The empty-consumers test confirms every assignor returns an empty map rather than panicking on a zero divisor.

Create `assignment_test.go`:

```go
package assignment

import (
	"fmt"
	"testing"
)

func TestRangeAssignorEvenSplit(t *testing.T) {
	t.Parallel()

	got := RangeAssignor{}.Assign([]int{0, 1, 2, 3, 4, 5}, []string{"c0", "c1", "c2"}, nil)
	want := map[string][]int{"c0": {0, 1}, "c1": {2, 3}, "c2": {4, 5}}
	if !assignmentsEqual(got, want) {
		t.Fatalf("RangeAssignor even split: got %v, want %v", got, want)
	}
}

func TestRangeAssignorRemainder(t *testing.T) {
	t.Parallel()

	// 10 partitions / 3 consumers: first gets floor(10/3)+1=4, others get 3.
	got := RangeAssignor{}.Assign(
		[]int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		[]string{"c0", "c1", "c2"},
		nil,
	)
	want := map[string][]int{"c0": {0, 1, 2, 3}, "c1": {4, 5, 6}, "c2": {7, 8, 9}}
	if !assignmentsEqual(got, want) {
		t.Fatalf("RangeAssignor remainder: got %v, want %v", got, want)
	}
}

func TestRangeAssignorSingleConsumer(t *testing.T) {
	t.Parallel()

	parts := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	got := RangeAssignor{}.Assign(parts, []string{"only"}, nil)
	if len(got["only"]) != 12 {
		t.Fatalf("single consumer should own all 12 partitions, got %v", got["only"])
	}
}

func TestRoundRobinAssignor(t *testing.T) {
	t.Parallel()

	got := RoundRobinAssignor{}.Assign([]int{0, 1, 2, 3, 4, 5}, []string{"c0", "c1", "c2"}, nil)
	want := map[string][]int{"c0": {0, 3}, "c1": {1, 4}, "c2": {2, 5}}
	if !assignmentsEqual(got, want) {
		t.Fatalf("RoundRobinAssignor: got %v, want %v", got, want)
	}
}

func TestStickyAssignorPreservesRetainedPartitions(t *testing.T) {
	t.Parallel()

	prev := map[string][]int{"c0": {0, 1}, "c1": {2, 3}, "c2": {4, 5}}
	// c2 departs; c0 and c1 must keep their previous partitions.
	got := StickyAssignor{}.Assign([]int{0, 1, 2, 3, 4, 5}, []string{"c0", "c1"}, prev)
	if !containsAll(got["c0"], []int{0, 1}) || !containsAll(got["c1"], []int{2, 3}) {
		t.Fatalf("Sticky: original partitions not preserved, got %v", got)
	}
	if !allCovered(got, 6) {
		t.Fatalf("Sticky: not all 6 partitions covered: %v", got)
	}
}

func TestStickyAssignorMinimizesMovement(t *testing.T) {
	t.Parallel()

	prev := map[string][]int{"c0": {0, 1}, "c1": {2, 3}, "c2": {4, 5}}
	got := StickyAssignor{}.Assign([]int{0, 1, 2, 3, 4, 5}, []string{"c0", "c1"}, prev)

	// The two surviving consumers keep all their partitions; only c2's two
	// orphans move. Moved counts already-owned partitions that changed hands.
	if m := Moved(prev, got); m != 2 {
		t.Fatalf("Sticky moved %d partitions, want exactly 2 (only c2's orphans)", m)
	}
}

func TestStickyMovesLessThanEagerRoundRobin(t *testing.T) {
	t.Parallel()

	parts := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	prev := RoundRobinAssignor{}.Assign(parts, []string{"c0", "c1", "c2"}, nil)

	eager := RoundRobinAssignor{}.Assign(parts, []string{"c0", "c1"}, nil)
	sticky := StickyAssignor{}.Assign(parts, []string{"c0", "c1"}, prev)

	me, ms := Moved(prev, eager), Moved(prev, sticky)
	if ms >= me {
		t.Fatalf("sticky moved %d, eager moved %d; sticky must move strictly fewer", ms, me)
	}
	if !allCovered(sticky, 12) || !allCovered(eager, 12) {
		t.Fatalf("not all partitions covered: sticky=%v eager=%v", sticky, eager)
	}
}

func TestEmptyConsumersYieldsEmptyAssignment(t *testing.T) {
	t.Parallel()

	for _, a := range []Assignor{RangeAssignor{}, RoundRobinAssignor{}, StickyAssignor{}} {
		got := a.Assign([]int{0, 1, 2}, nil, nil)
		if len(got) != 0 {
			t.Fatalf("%T: empty consumers should yield empty assignment, got %v", a, got)
		}
	}
}

func ExampleRangeAssignor() {
	got := RangeAssignor{}.Assign([]int{0, 1, 2, 3, 4, 5}, []string{"c0", "c1", "c2"}, nil)
	fmt.Println(got["c0"])
	fmt.Println(got["c1"])
	fmt.Println(got["c2"])
	// Output:
	// [0 1]
	// [2 3]
	// [4 5]
}

func ExampleRoundRobinAssignor() {
	got := RoundRobinAssignor{}.Assign([]int{0, 1, 2, 3, 4, 5}, []string{"c0", "c1", "c2"}, nil)
	fmt.Println(got["c0"])
	fmt.Println(got["c1"])
	fmt.Println(got["c2"])
	// Output:
	// [0 3]
	// [1 4]
	// [2 5]
}

// --- test helpers ---

func assignmentsEqual(a, b map[string][]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		sa, sb := intsSorted(va), intsSorted(vb)
		for i := range sa {
			if sa[i] != sb[i] {
				return false
			}
		}
	}
	return true
}

func allCovered(assignment map[string][]int, total int) bool {
	seen := make(map[int]bool, total)
	for _, parts := range assignment {
		for _, p := range parts {
			seen[p] = true
		}
	}
	return len(seen) == total
}

func contains(slice []int, v int) bool {
	for _, x := range slice {
		if x == v {
			return true
		}
	}
	return false
}

func containsAll(slice, sub []int) bool {
	for _, v := range sub {
		if !contains(slice, v) {
			return false
		}
	}
	return true
}
```

## Review

The assignors are correct when their outputs are deterministic and their contracts hold: Range gives contiguous blocks with the remainder on the first consumers, RoundRobin interleaves evenly, and Sticky preserves every survivor's partitions while redistributing only orphans. The single most important property is the one `Moved` measures - sticky changes the ownership of strictly fewer partitions than an eager recompute - because each moved partition is a per-partition state rebuild for its new owner. Confirm the empty-consumer guard: dividing by `len(consumers)` without checking for zero panics, so each assignor returns early on an empty consumer set.

Common mistakes for this feature. The first is forgetting to sort inputs, which makes the assignment depend on Go's randomized map iteration order and produces a different (but still "valid") result on every run, breaking any test that pins exact output. The second is measuring an assignor only by balance and concluding RoundRobin is fine - balance is necessary but not sufficient, and the movement metric is what reveals that an eager strategy reshuffles state needlessly. The third is counting newly added partitions as "moved" in `Moved`; first assignment is not movement, so only partitions owned in `prev` count.

## Resources

- [`sort` package](https://pkg.go.dev/sort) - `sort.Ints` and `sort.Strings`, used to make every assignment deterministic.
- [KIP-54: Sticky Partition Assignment Strategy](https://cwiki.apache.org/confluence/display/KAFKA/KIP-54+-+Sticky+Partition+Assignment+Strategy) - the motivation and algorithm for minimizing partition movement on rebalance.
- [Kafka Consumer Group Protocol](https://kafka.apache.org/documentation/#impl_consumer) - how a production broker assigns partitions across a group.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-offset-tracking-and-lag.md](02-offset-tracking-and-lag.md)
