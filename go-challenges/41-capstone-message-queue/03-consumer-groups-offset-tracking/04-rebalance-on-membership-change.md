# Exercise 4: Rebalance as a Per-Member Delta

A rebalance is not a global event a consumer observes from the outside; it is a precise instruction each consumer receives: stop owning these partitions, start owning those. This exercise builds a coordinator that computes that minimal delta on every membership change - which partitions each member revokes and which it gains - and contrasts the cooperative (incremental) view with the eager "revoke everything and recompute" view.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
rebalance.go         AssignFunc (RangeAssign, RoundRobinAssign); Delta; Coordinator (Join/Leave)
cmd/
  demo/
    main.go          c0,c1,c2 join then c1 leaves; print each rebalance's revoke/assign delta
rebalance_test.go    first-join, second-join split, leave hand-off, single-owner invariant
```

- Files: `rebalance.go`, `cmd/demo/main.go`, `rebalance_test.go`.
- Implement: `Coordinator` with `Join` and `Leave` returning a `Delta`, plus `RangeAssign` and `RoundRobinAssign`.
- Test: the first join assigns everything and revokes nothing, the second join moves exactly half off the first consumer, a leave hands partitions to the survivor, and every partition is always owned by exactly one consumer.
- Verify: `go test -race ./...`

### Why a consumer needs the delta, not the full assignment

When membership changes, the coordinator could just hand every consumer its new partition list and let each figure out what changed. That is what an eager rebalance does, and it is wasteful: a consumer that keeps partition 3 across the rebalance has no way to know it kept it, so it conservatively stops, commits, and restarts every partition - including the ones that never moved. The fix is to compute the change explicitly. A `Delta` carries, per consumer, a `Revoked` set (partitions to stop owning) and an `Assigned` set (partitions to begin owning). A consumer that appears in neither does nothing; a consumer that only keeps its partitions sees an empty delta and never pauses.

The order the consumer acts on a delta is fixed and load-bearing: revoke first, then assign. The revoked partitions are about to be handed to another consumer, which will resume from the committed offset, so the losing consumer must commit its final offsets for those partitions before it lets them go. Only then is it safe for the gaining consumer to start. This is the same revoke-before-assign contract the coordinator in the previous exercise enforces through its listener, expressed here as data the consumer can inspect.

Computing the delta is a set difference per consumer. For each consumer present in either the old or the new assignment, `Revoked` is "old minus new" and `Assigned` is "new minus old". A departed consumer appears only in the old assignment, so all its partitions land in its `Revoked` set (and it is dropped from the result); a brand-new consumer appears only in the new assignment, so everything it gets is `Assigned`. The generation counter rides along on every delta so a consumer can tag its commits and the coordinator can reject a commit from a superseded generation.

Create `rebalance.go`:

```go
package rebalance

import "sort"

// AssignFunc maps a sorted partition set across a set of consumers, returning
// consumer -> partitions. Two strategies are provided: RangeAssign and
// RoundRobinAssign.
type AssignFunc func(partitions []int, consumers []string) map[string][]int

// RangeAssign gives each sorted consumer a contiguous block of sorted
// partitions; the first (remainder) consumers each take one extra.
func RangeAssign(partitions []int, consumers []string) map[string][]int {
	ps := sortedInts(partitions)
	cs := sortedStrs(consumers)
	out := make(map[string][]int, len(cs))
	if len(cs) == 0 {
		return out
	}
	base, rem := len(ps)/len(cs), len(ps)%len(cs)
	idx := 0
	for i, c := range cs {
		n := base
		if i < rem {
			n++
		}
		out[c] = append([]int(nil), ps[idx:idx+n]...)
		idx += n
	}
	return out
}

// RoundRobinAssign deals sorted partitions to sorted consumers in rotation.
func RoundRobinAssign(partitions []int, consumers []string) map[string][]int {
	ps := sortedInts(partitions)
	cs := sortedStrs(consumers)
	out := make(map[string][]int, len(cs))
	if len(cs) == 0 {
		return out
	}
	for i, p := range ps {
		out[cs[i%len(cs)]] = append(out[cs[i%len(cs)]], p)
	}
	return out
}

// Delta is the per-consumer change a rebalance produces. For each consumer it
// lists the partitions it must stop owning (Revoked) and the partitions it must
// begin owning (Assigned). A consumer acting on a Delta commits offsets for its
// Revoked partitions before it starts fetching its Assigned ones. Generation is
// the new generation counter; a consumer tags its commits with it so a stale
// commit from a superseded generation can be rejected.
type Delta struct {
	Generation int
	Revoked    map[string][]int
	Assigned   map[string][]int
}

// Coordinator tracks the current assignment for a fixed partition set and
// recomputes it whenever membership changes, returning the minimal per-consumer
// Delta each change produces. It is single-goroutine: callers serialize Join
// and Leave (membership changes are not concurrent in this model).
type Coordinator struct {
	partitions []int
	assign     AssignFunc
	members    map[string]bool
	current    map[string][]int
	generation int
}

// New returns a Coordinator over partitions using the given assignment strategy.
func New(partitions []int, assign AssignFunc) *Coordinator {
	return &Coordinator{
		partitions: append([]int(nil), partitions...),
		assign:     assign,
		members:    make(map[string]bool),
		current:    make(map[string][]int),
	}
}

// Join adds a consumer and returns the Delta the resulting rebalance produces.
// Joining an existing member is a no-op that still bumps the generation.
func (c *Coordinator) Join(id string) Delta {
	c.members[id] = true
	return c.rebalance()
}

// Leave removes a consumer and returns the Delta. The departing consumer's
// partitions appear as Assigned on the survivors that pick them up; the
// departing consumer itself is dropped from the assignment.
func (c *Coordinator) Leave(id string) Delta {
	delete(c.members, id)
	return c.rebalance()
}

// Assignment returns a copy of the current consumer -> partitions mapping.
func (c *Coordinator) Assignment() map[string][]int {
	out := make(map[string][]int, len(c.current))
	for k, v := range c.current {
		out[k] = append([]int(nil), v...)
	}
	return out
}

// Generation returns the current generation counter.
func (c *Coordinator) Generation() int { return c.generation }

func (c *Coordinator) rebalance() Delta {
	c.generation++
	consumers := make([]string, 0, len(c.members))
	for id := range c.members {
		consumers = append(consumers, id)
	}
	next := c.assign(c.partitions, consumers)
	for id := range next {
		sort.Ints(next[id])
	}

	d := Delta{
		Generation: c.generation,
		Revoked:    make(map[string][]int),
		Assigned:   make(map[string][]int),
	}
	// Every consumer that appears in either the old or new assignment.
	all := make(map[string]bool)
	for id := range c.current {
		all[id] = true
	}
	for id := range next {
		all[id] = true
	}
	for id := range all {
		oldSet := toSet(c.current[id])
		newSet := toSet(next[id])
		for _, p := range c.current[id] {
			if !newSet[p] {
				d.Revoked[id] = append(d.Revoked[id], p)
			}
		}
		for _, p := range next[id] {
			if !oldSet[p] {
				d.Assigned[id] = append(d.Assigned[id], p)
			}
		}
		sort.Ints(d.Revoked[id])
		sort.Ints(d.Assigned[id])
		if len(d.Revoked[id]) == 0 {
			delete(d.Revoked, id)
		}
		if len(d.Assigned[id]) == 0 {
			delete(d.Assigned, id)
		}
	}
	c.current = next
	return d
}

func toSet(s []int) map[int]bool {
	m := make(map[int]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

func sortedInts(s []int) []int {
	out := append([]int(nil), s...)
	sort.Ints(out)
	return out
}

func sortedStrs(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}
```

The `rebalance` method is the core. It bumps the generation, recomputes the full assignment with the configured `AssignFunc`, then diffs old against new for every consumer that appears in either. The two `delete` calls at the end prune empty `Revoked`/`Assigned` entries so the delta only mentions consumers that actually changed - which is exactly what makes a no-op rebalance produce an empty delta rather than a map full of empty slices. Note that `Coordinator` is single-goroutine by contract: membership changes are serialized by the caller, because a rebalance is a globally ordered event, not something two members trigger concurrently.

### The runnable demo

The demo runs four membership changes - three joins then a leave - over six partitions with the Range strategy, and prints the delta each one produces. Watch how a join only moves partitions off the consumers that had too many, and how a leave hands the departed consumer's partitions to the survivors.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"

	"example.com/rebalance"
)

func printDelta(label string, d rebalance.Delta) {
	fmt.Printf("%s (generation %d)\n", label, d.Generation)
	if len(d.Revoked) == 0 && len(d.Assigned) == 0 {
		fmt.Println("  no partitions moved")
		return
	}
	for _, id := range sortedKeys(d.Revoked) {
		fmt.Printf("  %s revoke  %v\n", id, d.Revoked[id])
	}
	for _, id := range sortedKeys(d.Assigned) {
		fmt.Printf("  %s assign  %v\n", id, d.Assigned[id])
	}
}

func sortedKeys(m map[string][]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func main() {
	c := rebalance.New([]int{0, 1, 2, 3, 4, 5}, rebalance.RangeAssign)

	printDelta("c0 joins", c.Join("c0"))
	printDelta("c1 joins", c.Join("c1"))
	printDelta("c2 joins", c.Join("c2"))
	printDelta("c1 leaves", c.Leave("c1"))

	fmt.Println("\nFinal assignment:")
	a := c.Assignment()
	for _, id := range sortedKeys(a) {
		fmt.Printf("  %s: %v\n", id, a[id])
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
c0 joins (generation 1)
  c0 assign  [0 1 2 3 4 5]
c1 joins (generation 2)
  c0 revoke  [3 4 5]
  c1 assign  [3 4 5]
c2 joins (generation 3)
  c0 revoke  [2]
  c1 revoke  [4 5]
  c1 assign  [2]
  c2 assign  [4 5]
c1 leaves (generation 4)
  c1 revoke  [2 3]
  c0 assign  [2]
  c2 assign  [3]
```

The first join is pure assignment - nothing to revoke. The second join moves exactly the three partitions c0 has in excess. The third join is the interesting one: c0 gives up one partition, c1 gives up two and gains one, and c2 starts fresh with two - the delta names only the partitions that actually move, which is the whole point of computing it. When c1 leaves, its two partitions are split between the survivors rather than triggering a full reshuffle.

### Tests

The tests pin the delta's contract. The first join revokes nothing and assigns everything. The second join over four partitions moves exactly partitions 2 and 3 off c0 and onto c1 - a precise assertion, not a count. A leave hands the departed consumer's partitions to the survivor and leaves the final assignment whole. The invariant test runs a join/join/join/leave/join sequence under both strategies and asserts every partition is owned by exactly one consumer at the end, and a separate test asserts a consumer never both revokes and assigns the same partition in one delta. A conservation test confirms the partition set is never lost or duplicated across a long sequence of changes.

Create `rebalance_test.go`:

```go
package rebalance

import (
	"sort"
	"testing"
)

func TestFirstJoinAssignsEverythingToOneConsumer(t *testing.T) {
	t.Parallel()

	c := New([]int{0, 1, 2, 3}, RangeAssign)
	d := c.Join("c0")
	if len(d.Revoked) != 0 {
		t.Fatalf("first join should revoke nothing, got %v", d.Revoked)
	}
	if got := d.Assigned["c0"]; len(got) != 4 {
		t.Fatalf("first join should assign all 4 partitions to c0, got %v", got)
	}
	if d.Generation != 1 {
		t.Fatalf("first join generation = %d, want 1", d.Generation)
	}
}

func TestSecondJoinMovesHalfOff(t *testing.T) {
	t.Parallel()

	c := New([]int{0, 1, 2, 3}, RangeAssign)
	c.Join("c0")
	d := c.Join("c1")

	// Range over {c0,c1}: c0 -> {0,1}, c1 -> {2,3}. c0 had {0,1,2,3}, so it
	// revokes {2,3}; c1 gains {2,3}. Nothing else moves.
	if got := d.Revoked["c0"]; !equal(got, []int{2, 3}) {
		t.Fatalf("c0 revoked = %v, want [2 3]", got)
	}
	if got := d.Assigned["c1"]; !equal(got, []int{2, 3}) {
		t.Fatalf("c1 assigned = %v, want [2 3]", got)
	}
	if _, ok := d.Assigned["c0"]; ok {
		t.Fatalf("c0 should gain nothing on this rebalance, got %v", d.Assigned["c0"])
	}
}

func TestLeaveHandsPartitionsToSurvivor(t *testing.T) {
	t.Parallel()

	c := New([]int{0, 1, 2, 3}, RangeAssign)
	c.Join("c0")
	c.Join("c1")
	d := c.Leave("c1")

	// c1 held {2,3}; after it leaves c0 owns everything and gains {2,3}.
	if got := d.Assigned["c0"]; !equal(got, []int{2, 3}) {
		t.Fatalf("c0 assigned on leave = %v, want [2 3]", got)
	}
	if a := c.Assignment(); !equal(a["c0"], []int{0, 1, 2, 3}) {
		t.Fatalf("c0 final assignment = %v, want [0 1 2 3]", a["c0"])
	}
}

func TestEveryPartitionOwnedByExactlyOneConsumer(t *testing.T) {
	t.Parallel()

	for _, fn := range []AssignFunc{RangeAssign, RoundRobinAssign} {
		c := New([]int{0, 1, 2, 3, 4, 5, 6, 7}, fn)
		c.Join("a")
		c.Join("b")
		c.Join("c")
		c.Leave("b")
		c.Join("d")

		owner := make(map[int]string)
		for id, parts := range c.Assignment() {
			for _, p := range parts {
				if prev, ok := owner[p]; ok {
					t.Fatalf("partition %d owned by both %s and %s", p, prev, id)
				}
				owner[p] = id
			}
		}
		if len(owner) != 8 {
			t.Fatalf("expected all 8 partitions owned, got %d", len(owner))
		}
	}
}

func TestRevokedAndAssignedAreDisjointPerConsumer(t *testing.T) {
	t.Parallel()

	c := New([]int{0, 1, 2, 3, 4, 5}, RoundRobinAssign)
	c.Join("c0")
	c.Join("c1")
	d := c.Join("c2")

	// A consumer never both revokes and assigns the same partition in one delta.
	for id, rev := range d.Revoked {
		asn := toSet(d.Assigned[id])
		for _, p := range rev {
			if asn[p] {
				t.Fatalf("%s both revokes and assigns partition %d", id, p)
			}
		}
	}
}

func TestDeltaConservesPartitions(t *testing.T) {
	t.Parallel()

	// Sum of partitions across the assignment is always the full set after any
	// membership change.
	c := New([]int{0, 1, 2, 3, 4}, RangeAssign)
	c.Join("c0")
	c.Join("c1")
	c.Leave("c0")
	c.Join("c2")
	c.Join("c3")

	var all []int
	for _, parts := range c.Assignment() {
		all = append(all, parts...)
	}
	sort.Ints(all)
	if !equal(all, []int{0, 1, 2, 3, 4}) {
		t.Fatalf("partitions not conserved: got %v, want [0 1 2 3 4]", all)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

## Review

The delta is correct when it names only what changed: `Revoked` is old-minus-new and `Assigned` is new-minus-old per consumer, empty entries are pruned, and a no-op rebalance yields an empty delta. The load-bearing property is that across any sequence of joins and leaves the partition set is conserved and every partition has exactly one owner - the single-owner invariant test is what guarantees no partition is dropped or double-assigned during a hand-off. Confirm the revoke-before-assign discipline is expressed in the data: a consumer's `Revoked` and `Assigned` sets for one delta are disjoint, so acting on revoke first and assign second is always well-defined.

Common mistakes for this feature. The first is handing consumers their full new assignment instead of the delta, which forces every consumer - even those that kept all their partitions - to stop and restart, the exact cost cooperative rebalancing exists to avoid. The second is computing the delta but acting on assign before revoke, which lets two consumers believe they own the same partition during the hand-off window and causes double processing. The third is forgetting to prune empty entries, so a no-op rebalance produces a delta that looks like everyone changed when no one did.

## Resources

- [KIP-429: Kafka Consumer Incremental Rebalance Protocol](https://cwiki.apache.org/confluence/display/KAFKA/KIP-429%3A+Kafka+Consumer+Incremental+Rebalance+Protocol) - the design that replaced eager rebalancing with the cooperative, delta-based protocol this exercise models.
- [Kafka Consumer Group Protocol](https://kafka.apache.org/documentation/#impl_consumer) - the join/sync round that produces an assignment a consumer must diff against its current one.
- [`sort` package](https://pkg.go.dev/sort) - `sort.Ints`, used to make every delta deterministic.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-delivery-semantics.md](05-delivery-semantics.md)
