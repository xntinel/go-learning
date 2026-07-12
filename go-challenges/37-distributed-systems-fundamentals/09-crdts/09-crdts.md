# 9. CRDTs: Conflict-Free Replicated Data Types

Conflict-free Replicated Data Types (CRDTs) are data structures that can be
replicated across nodes, updated independently without coordination, and merged
deterministically. The merge function is commutative, associative, and
idempotent -- the three properties that form a join semilattice and guarantee
eventual convergence regardless of message ordering or network partitions.

The hard part is not the merge itself but understanding *why* the mathematical
properties are non-negotiable: any violation makes divergence possible, and no
amount of retry logic can recover from that. This lesson implements five
fundamental CRDTs, tests the semilattice laws directly, and builds a small
replicated shopping cart from them.

```text
crdt/
  go.mod
  crdt.go
  crdt_test.go
  cmd/demo/main.go
```

## Concepts

### The Join Semilattice

A CRDT's state space is a partially-ordered set (poset) with a least upper
bound (join) for every pair of elements. "Merge" is the join operation. Three
laws are required:

- Commutativity: `Merge(a, b) == Merge(b, a)` -- the order replicas are
  received does not matter.
- Associativity: `Merge(Merge(a, b), c) == Merge(a, Merge(b, c))` -- batching
  merges produces the same result.
- Idempotency: `Merge(a, a) == a` -- redelivering a state does not cause drift.

If any law fails, the system can diverge silently. No consensus protocol is
needed because the laws make the outcome deterministic.

### State-Based vs. Operation-Based

State-based CRDTs (CvRDTs) ship the entire local state and merge on receipt.
Operation-based CRDTs (CmRDTs) ship individual operations that must be
delivered exactly-once in causal order. State-based CRDTs are simpler to
implement and require only reliable broadcast (at-least-once); they are the
focus of this lesson.

### The Five Fundamental Types

| Type | Operations | Merge rule |
|---|---|---|
| G-Counter | Increment | Per-node max |
| PN-Counter | Increment, Decrement | Two G-Counters |
| G-Set | Add | Set union |
| OR-Set | Add, Remove | Add-wins via unique tags |
| LWW-Register | Write | Highest timestamp wins |

### OR-Set Add-Wins Semantics

Naive remove (delete the element) loses concurrent adds. OR-Set tags every add
with a unique token. Remove only deletes the *observed* tags at the time of
the remove. A concurrent add generates a fresh tag that is not deleted, so
the element remains present after merge. This is the canonical way to support
remove in a CRDT without coordination.

### LWW-Register Tie-Breaking

Two writes at the same wall-clock time produce a tie. Break it with the node
ID (lexicographic, or numeric) to make the outcome deterministic. Without a
tie-breaker, different replicas can choose different winners.

### Trade-offs

CRDTs give availability and partition tolerance at the cost of expressiveness.
You cannot express "decrement but never below zero" with a PN-Counter alone
because the constraint requires cross-replica coordination. Production systems
(Riak, Redis Cluster, Automerge) use CRDTs for counters, sets, and
last-writer-wins registers where monotonic or commutative semantics are natural.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/09-crdts/09-crdts/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/09-crdts/09-crdts
```

This is a library package verified with `go test`. There is no top-level
`main`; the demo lives in `cmd/demo`.

### Exercise 1: G-Counter and PN-Counter

Create `crdt.go`:

```go
// crdt.go
package crdt

import "fmt"

// GCounter is a grow-only counter. Each node tracks its own count; the
// global value is the sum of all node counts. Merge takes the per-node max.
type GCounter struct {
	counts map[string]int64
}

// NewGCounter returns an initialised GCounter for nodeID.
func NewGCounter(nodeID string) *GCounter {
	return &GCounter{counts: map[string]int64{nodeID: 0}}
}

// Increment increases the local node's count by delta. delta must be >= 1.
func (g *GCounter) Increment(nodeID string, delta int64) {
	if delta < 1 {
		return
	}
	g.counts[nodeID] += delta
}

// Value returns the global counter value (sum of all node counts).
func (g *GCounter) Value() int64 {
	var sum int64
	for _, v := range g.counts {
		sum += v
	}
	return sum
}

// Merge merges other into g in place (takes per-node max).
func (g *GCounter) Merge(other *GCounter) {
	for k, v := range other.counts {
		if v > g.counts[k] {
			g.counts[k] = v
		}
	}
}

// Clone returns a deep copy.
func (g *GCounter) Clone() *GCounter {
	c := &GCounter{counts: make(map[string]int64, len(g.counts))}
	for k, v := range g.counts {
		c.counts[k] = v
	}
	return c
}

// Equal reports whether g and other have identical state.
func (g *GCounter) Equal(other *GCounter) bool {
	if len(g.counts) != len(other.counts) {
		return false
	}
	for k, v := range g.counts {
		if other.counts[k] != v {
			return false
		}
	}
	return true
}

// NodeCount returns the stored count for nodeID (exported for cmd/demo).
func (g *GCounter) NodeCount(nodeID string) int64 {
	return g.counts[nodeID]
}

// PNCounter is a positive-negative counter built from two GCounters.
type PNCounter struct {
	inc *GCounter
	dec *GCounter
}

// NewPNCounter returns an initialised PNCounter.
func NewPNCounter() *PNCounter {
	return &PNCounter{
		inc: &GCounter{counts: make(map[string]int64)},
		dec: &GCounter{counts: make(map[string]int64)},
	}
}

// Increment increases the counter at nodeID by delta (delta >= 1).
func (p *PNCounter) Increment(nodeID string, delta int64) {
	if delta < 1 {
		return
	}
	p.inc.counts[nodeID] += delta
}

// Decrement decreases the counter at nodeID by delta (delta >= 1).
func (p *PNCounter) Decrement(nodeID string, delta int64) {
	if delta < 1 {
		return
	}
	p.dec.counts[nodeID] += delta
}

// Value returns inc.Value() - dec.Value().
func (p *PNCounter) Value() int64 {
	return p.inc.Value() - p.dec.Value()
}

// Merge merges other into p in place.
func (p *PNCounter) Merge(other *PNCounter) {
	p.inc.Merge(other.inc)
	p.dec.Merge(other.dec)
}

// Clone returns a deep copy.
func (p *PNCounter) Clone() *PNCounter {
	return &PNCounter{inc: p.inc.Clone(), dec: p.dec.Clone()}
}

// Equal reports whether p and other have identical state.
func (p *PNCounter) Equal(other *PNCounter) bool {
	return p.inc.Equal(other.inc) && p.dec.Equal(other.dec)
}

// GSet is a grow-only set. Elements can be added but never removed.
// Merge is set union.
type GSet struct {
	elems map[string]struct{}
}

// NewGSet returns an empty GSet.
func NewGSet() *GSet {
	return &GSet{elems: make(map[string]struct{})}
}

// Add inserts elem into the set.
func (s *GSet) Add(elem string) {
	s.elems[elem] = struct{}{}
}

// Contains reports whether elem is in the set.
func (s *GSet) Contains(elem string) bool {
	_, ok := s.elems[elem]
	return ok
}

// Size returns the number of elements in the set.
func (s *GSet) Size() int {
	return len(s.elems)
}

// Merge merges other into s in place (set union).
func (s *GSet) Merge(other *GSet) {
	for k := range other.elems {
		s.elems[k] = struct{}{}
	}
}

// Clone returns a deep copy.
func (s *GSet) Clone() *GSet {
	c := &GSet{elems: make(map[string]struct{}, len(s.elems))}
	for k := range s.elems {
		c.elems[k] = struct{}{}
	}
	return c
}

// Equal reports whether s and other have identical state.
func (s *GSet) Equal(other *GSet) bool {
	if len(s.elems) != len(other.elems) {
		return false
	}
	for k := range s.elems {
		if _, ok := other.elems[k]; !ok {
			return false
		}
	}
	return true
}

// ORSet is an observed-remove set with add-wins semantics.
// Each add is tagged with a unique token; remove deletes only the observed
// tags. A concurrent add with a fresh token survives remove.
type ORSet struct {
	// tags maps element -> set of add-tokens present for that element.
	tags map[string]map[string]struct{}
}

// NewORSet returns an empty ORSet.
func NewORSet() *ORSet {
	return &ORSet{tags: make(map[string]map[string]struct{})}
}

// Add inserts elem with a caller-supplied unique token (e.g. a UUID or
// a node+sequence string). The token must be globally unique.
func (o *ORSet) Add(elem, token string) {
	if o.tags[elem] == nil {
		o.tags[elem] = make(map[string]struct{})
	}
	o.tags[elem][token] = struct{}{}
}

// Remove deletes all currently-observed tags for elem. Concurrent adds with
// new tokens are unaffected (add-wins).
func (o *ORSet) Remove(elem string) {
	delete(o.tags, elem)
}

// Contains reports whether elem has at least one live tag.
func (o *ORSet) Contains(elem string) bool {
	return len(o.tags[elem]) > 0
}

// Merge merges other into o in place (union of tag sets per element).
func (o *ORSet) Merge(other *ORSet) {
	for elem, tokens := range other.tags {
		if o.tags[elem] == nil {
			o.tags[elem] = make(map[string]struct{})
		}
		for tok := range tokens {
			o.tags[elem][tok] = struct{}{}
		}
	}
}

// Clone returns a deep copy.
func (o *ORSet) Clone() *ORSet {
	c := &ORSet{tags: make(map[string]map[string]struct{}, len(o.tags))}
	for elem, tokens := range o.tags {
		ct := make(map[string]struct{}, len(tokens))
		for tok := range tokens {
			ct[tok] = struct{}{}
		}
		c.tags[elem] = ct
	}
	return c
}

// Equal reports whether o and other have identical state.
func (o *ORSet) Equal(other *ORSet) bool {
	if len(o.tags) != len(other.tags) {
		return false
	}
	for elem, tokens := range o.tags {
		ot, ok := other.tags[elem]
		if !ok || len(tokens) != len(ot) {
			return false
		}
		for tok := range tokens {
			if _, ok := ot[tok]; !ok {
				return false
			}
		}
	}
	return true
}

// LWWRegister is a last-writer-wins register. Merge keeps the entry with the
// highest (timestamp, nodeID) pair; nodeID breaks ties deterministically.
type LWWRegister struct {
	value     string
	timestamp int64  // Unix nanoseconds or any monotonically increasing int
	nodeID    string // tie-breaker
}

// NewLWWRegister returns a zero-valued register owned by nodeID.
func NewLWWRegister(nodeID string) *LWWRegister {
	return &LWWRegister{nodeID: nodeID}
}

// Write sets the register value with a caller-supplied timestamp.
// Use time.Now().UnixNano() for wall-clock writes.
func (r *LWWRegister) Write(value string, timestamp int64, nodeID string) {
	if timestamp > r.timestamp ||
		(timestamp == r.timestamp && nodeID > r.nodeID) {
		r.value = value
		r.timestamp = timestamp
		r.nodeID = nodeID
	}
}

// Value returns the current register value.
func (r *LWWRegister) Value() string {
	return r.value
}

// Timestamp returns the winning timestamp (exported for cmd/demo).
func (r *LWWRegister) Timestamp() int64 {
	return r.timestamp
}

// Merge merges other into r in place (highest-timestamp wins).
func (r *LWWRegister) Merge(other *LWWRegister) {
	r.Write(other.value, other.timestamp, other.nodeID)
}

// Clone returns a deep copy.
func (r *LWWRegister) Clone() *LWWRegister {
	return &LWWRegister{value: r.value, timestamp: r.timestamp, nodeID: r.nodeID}
}

// Equal reports whether r and other have identical state.
func (r *LWWRegister) Equal(other *LWWRegister) bool {
	return r.value == other.value &&
		r.timestamp == other.timestamp &&
		r.nodeID == other.nodeID
}

// Cart combines an ORSet (item presence) and a PNCounter (item quantity)
// into a simple replicated shopping cart.
type Cart struct {
	items     *ORSet
	qty       *PNCounter
	nodeID    string
	seqTokens int64
}

// NewCart returns a new Cart for nodeID.
func NewCart(nodeID string) *Cart {
	return &Cart{
		items:  NewORSet(),
		qty:    NewPNCounter(),
		nodeID: nodeID,
	}
}

// AddItem adds itemID with the given quantity.
func (c *Cart) AddItem(itemID string, quantity int64) {
	c.seqTokens++
	token := fmt.Sprintf("%s-%d", c.nodeID, c.seqTokens)
	c.items.Add(itemID, token)
	c.qty.Increment(c.nodeID, quantity)
}

// RemoveItem removes itemID.
func (c *Cart) RemoveItem(itemID string, quantity int64) {
	c.items.Remove(itemID)
	c.qty.Decrement(c.nodeID, quantity)
}

// HasItem reports whether itemID is in the cart.
func (c *Cart) HasItem(itemID string) bool {
	return c.items.Contains(itemID)
}

// Quantity returns the net quantity across all nodes.
func (c *Cart) Quantity() int64 {
	return c.qty.Value()
}

// Merge merges other into c in place.
func (c *Cart) Merge(other *Cart) {
	c.items.Merge(other.items)
	c.qty.Merge(other.qty)
}
```

### Exercise 2: Tests -- Semilattice Laws and Correctness

Create `crdt_test.go`:

```go
// crdt_test.go
package crdt

import (
	"fmt"
	"testing"
)

// --- GCounter tests ---

func TestGCounterIncrement(t *testing.T) {
	t.Parallel()

	g := NewGCounter("n1")
	g.Increment("n1", 3)
	g.Increment("n2", 2)
	if got := g.Value(); got != 5 {
		t.Fatalf("Value() = %d, want 5", got)
	}
}

func TestGCounterMergeCommutative(t *testing.T) {
	t.Parallel()

	a := &GCounter{counts: map[string]int64{"n1": 3, "n2": 1}}
	b := &GCounter{counts: map[string]int64{"n1": 1, "n2": 5}}

	ab := a.Clone()
	ab.Merge(b)

	ba := b.Clone()
	ba.Merge(a)

	if !ab.Equal(ba) {
		t.Fatalf("commutativity violated: ab=%v ba=%v", ab.counts, ba.counts)
	}
}

func TestGCounterMergeAssociative(t *testing.T) {
	t.Parallel()

	a := &GCounter{counts: map[string]int64{"n1": 1}}
	b := &GCounter{counts: map[string]int64{"n2": 2}}
	c := &GCounter{counts: map[string]int64{"n3": 3}}

	// (a merge b) merge c
	ab := a.Clone()
	ab.Merge(b)
	abc1 := ab.Clone()
	abc1.Merge(c)

	// a merge (b merge c)
	bc := b.Clone()
	bc.Merge(c)
	abc2 := a.Clone()
	abc2.Merge(bc)

	if !abc1.Equal(abc2) {
		t.Fatalf("associativity violated: %v vs %v", abc1.counts, abc2.counts)
	}
}

func TestGCounterMergeIdempotent(t *testing.T) {
	t.Parallel()

	a := &GCounter{counts: map[string]int64{"n1": 5, "n2": 3}}
	aa := a.Clone()
	aa.Merge(a)
	if !aa.Equal(a) {
		t.Fatalf("idempotency violated: %v vs %v", aa.counts, a.counts)
	}
}

// --- PNCounter tests ---

func TestPNCounterValue(t *testing.T) {
	t.Parallel()

	p := NewPNCounter()
	p.Increment("n1", 10)
	p.Decrement("n1", 3)
	p.Increment("n2", 2)
	if got := p.Value(); got != 9 {
		t.Fatalf("Value() = %d, want 9", got)
	}
}

func TestPNCounterMergeCommutative(t *testing.T) {
	t.Parallel()

	a := NewPNCounter()
	a.Increment("n1", 5)
	a.Decrement("n1", 1)

	b := NewPNCounter()
	b.Increment("n2", 3)
	b.Decrement("n2", 2)

	ab := a.Clone()
	ab.Merge(b)

	ba := b.Clone()
	ba.Merge(a)

	if ab.Value() != ba.Value() {
		t.Fatalf("commutativity violated: ab=%d ba=%d", ab.Value(), ba.Value())
	}
}

func TestPNCounterMergeIdempotent(t *testing.T) {
	t.Parallel()

	p := NewPNCounter()
	p.Increment("n1", 7)
	p.Decrement("n1", 2)

	pp := p.Clone()
	pp.Merge(p)

	if !pp.Equal(p) {
		t.Fatal("idempotency violated")
	}
}

// --- GSet tests ---

func TestGSetAddAndContains(t *testing.T) {
	t.Parallel()

	s := NewGSet()
	s.Add("apple")
	s.Add("banana")

	if !s.Contains("apple") || !s.Contains("banana") {
		t.Fatal("expected both elements present")
	}
	if s.Contains("cherry") {
		t.Fatal("cherry should not be present")
	}
}

func TestGSetMergeCommutative(t *testing.T) {
	t.Parallel()

	a := NewGSet()
	a.Add("x")
	b := NewGSet()
	b.Add("y")

	ab := a.Clone()
	ab.Merge(b)

	ba := b.Clone()
	ba.Merge(a)

	if !ab.Equal(ba) {
		t.Fatal("commutativity violated")
	}
}

func TestGSetMergeIdempotent(t *testing.T) {
	t.Parallel()

	s := NewGSet()
	s.Add("x")
	s.Add("y")

	ss := s.Clone()
	ss.Merge(s)
	if !ss.Equal(s) {
		t.Fatal("idempotency violated")
	}
}

// --- ORSet tests ---

func TestORSetAddAndContains(t *testing.T) {
	t.Parallel()

	o := NewORSet()
	o.Add("milk", "tok1")
	if !o.Contains("milk") {
		t.Fatal("expected milk present")
	}
	o.Remove("milk")
	if o.Contains("milk") {
		t.Fatal("expected milk removed")
	}
}

func TestORSetAddWinsOnConcurrentRemove(t *testing.T) {
	t.Parallel()

	// Node A adds "milk" with tok1.
	a := NewORSet()
	a.Add("milk", "tok1")

	// Node B starts from the same state, then removes "milk".
	b := a.Clone()
	b.Remove("milk")

	// Concurrently, Node A adds "milk" again with tok2 (new token).
	a.Add("milk", "tok2")

	// After merge, milk must be present (add-wins).
	a.Merge(b)
	if !a.Contains("milk") {
		t.Fatal("add-wins: milk should be present after concurrent add+remove")
	}
}

func TestORSetMergeCommutative(t *testing.T) {
	t.Parallel()

	a := NewORSet()
	a.Add("x", "t1")
	b := NewORSet()
	b.Add("y", "t2")

	ab := a.Clone()
	ab.Merge(b)

	ba := b.Clone()
	ba.Merge(a)

	if !ab.Equal(ba) {
		t.Fatal("commutativity violated")
	}
}

func TestORSetMergeIdempotent(t *testing.T) {
	t.Parallel()

	o := NewORSet()
	o.Add("x", "t1")
	oo := o.Clone()
	oo.Merge(o)
	if !oo.Equal(o) {
		t.Fatal("idempotency violated")
	}
}

// --- LWWRegister tests ---

func TestLWWRegisterWriteHigherTimestampWins(t *testing.T) {
	t.Parallel()

	r := NewLWWRegister("n1")
	r.Write("old", 100, "n1")
	r.Write("new", 200, "n1")
	if got := r.Value(); got != "new" {
		t.Fatalf("Value() = %q, want \"new\"", got)
	}
}

func TestLWWRegisterTieBreakByNodeID(t *testing.T) {
	t.Parallel()

	r := NewLWWRegister("n1")
	r.Write("from-n1", 100, "n1")
	r.Write("from-n2", 100, "n2") // same timestamp, n2 > n1 lexicographically
	if got := r.Value(); got != "from-n2" {
		t.Fatalf("Value() = %q, want \"from-n2\"", got)
	}
}

func TestLWWRegisterMergeCommutative(t *testing.T) {
	t.Parallel()

	a := NewLWWRegister("n1")
	a.Write("alice", 100, "n1")

	b := NewLWWRegister("n2")
	b.Write("bob", 200, "n2")

	ab := a.Clone()
	ab.Merge(b)

	ba := b.Clone()
	ba.Merge(a)

	if !ab.Equal(ba) {
		t.Fatalf("commutativity violated: ab=%q ba=%q", ab.Value(), ba.Value())
	}
}

func TestLWWRegisterMergeIdempotent(t *testing.T) {
	t.Parallel()

	r := NewLWWRegister("n1")
	r.Write("v", 42, "n1")
	rr := r.Clone()
	rr.Merge(r)
	if !rr.Equal(r) {
		t.Fatal("idempotency violated")
	}
}

// --- Cart (integration) tests ---

func TestCartAddItem(t *testing.T) {
	t.Parallel()

	cart := NewCart("node1")
	cart.AddItem("apple", 3)
	if !cart.HasItem("apple") {
		t.Fatal("expected apple in cart")
	}
	if got := cart.Quantity(); got != 3 {
		t.Fatalf("Quantity() = %d, want 3", got)
	}
}

func TestCartConvergesAfterPartition(t *testing.T) {
	t.Parallel()

	// Three nodes start from a common empty state.
	n1 := NewCart("n1")
	n2 := NewCart("n2")
	n3 := NewCart("n3")

	// Partition: n1 and n2 operate independently.
	n1.AddItem("milk", 2)
	n2.AddItem("bread", 1)
	n3.AddItem("eggs", 6)

	// Partition heals: all nodes merge.
	n1.Merge(n2)
	n1.Merge(n3)
	n2.Merge(n1)
	n2.Merge(n3)
	n3.Merge(n1)
	n3.Merge(n2)

	// All nodes must converge to the same item set.
	for _, item := range []string{"milk", "bread", "eggs"} {
		if !n1.HasItem(item) || !n2.HasItem(item) || !n3.HasItem(item) {
			t.Fatalf("convergence failure: item %q not present on all nodes", item)
		}
	}

	// Total quantity must agree across nodes.
	if n1.Quantity() != n2.Quantity() || n2.Quantity() != n3.Quantity() {
		t.Fatalf("quantity mismatch: n1=%d n2=%d n3=%d",
			n1.Quantity(), n2.Quantity(), n3.Quantity())
	}
}

// ExampleGCounter demonstrates basic GCounter usage.
func ExampleGCounter_Value() {
	g := &GCounter{counts: map[string]int64{}}
	g.Increment("node1", 3)
	g.Increment("node2", 2)
	fmt.Println(g.Value())
	// Output: 5
}

// ExampleORSet_Contains demonstrates add-wins semantics.
func ExampleORSet_Contains() {
	o := NewORSet()
	o.Add("milk", "tok-1")

	// Simulate a concurrent remove at another node.
	b := o.Clone()
	b.Remove("milk")

	// Concurrent add with a fresh token.
	o.Add("milk", "tok-2")

	// Merge: add-wins because tok-2 survived.
	o.Merge(b)
	fmt.Println(o.Contains("milk"))
	// Output: true
}
```

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/crdt"
)

func main() {
	// G-Counter: three nodes increment independently, then merge.
	g1 := crdt.NewGCounter("n1")
	g1.Increment("n1", 5)

	g2 := crdt.NewGCounter("n2")
	g2.Increment("n2", 3)

	g1.Merge(g2)
	fmt.Printf("G-Counter after merge: %d (n1=%d n2=%d)\n",
		g1.Value(), g1.NodeCount("n1"), g1.NodeCount("n2"))

	// LWW-Register: two nodes write at different timestamps.
	r1 := crdt.NewLWWRegister("n1")
	r1.Write("first", 100, "n1")

	r2 := crdt.NewLWWRegister("n2")
	r2.Write("second", 200, "n2")

	r1.Merge(r2)
	fmt.Printf("LWW-Register after merge: %q (ts=%d)\n",
		r1.Value(), r1.Timestamp())

	// Shopping cart: two nodes add items, partition heals.
	cart1 := crdt.NewCart("n1")
	cart2 := crdt.NewCart("n2")

	cart1.AddItem("milk", 2)
	cart2.AddItem("bread", 1)

	cart1.Merge(cart2)
	cart2.Merge(cart1)

	fmt.Printf("Cart n1 has milk: %v, bread: %v, qty: %d\n",
		cart1.HasItem("milk"), cart1.HasItem("bread"), cart1.Quantity())
	fmt.Printf("Cart n2 has milk: %v, bread: %v, qty: %d\n",
		cart2.HasItem("milk"), cart2.HasItem("bread"), cart2.Quantity())
}
```

## Common Mistakes

### Treating Merge as Append

Wrong: in a G-Counter, merge appends new node entries and sums duplicates.

What happens: the same increment is counted twice when a state is re-delivered
(idempotency fails) and the global value grows without bound.

Fix: merge takes the per-node maximum. Re-delivering an older state is a no-op
because `max(old, new) == new`.

### OR-Set Remove Deletes the Wrong Set

Wrong: `Remove` deletes the element from a global set of elements, ignoring
tags. Concurrent adds lose.

What happens: Node A adds "milk" with tok2 at the same time Node B removes
"milk". After merge, milk is gone even though the add happened after the
remove was issued.

Fix: `Remove` deletes only the *observed* tags (the ones present at remove
time). A concurrent add with tok2 was not observed, so tok2 survives.

### LWW-Register Without a Tie-Breaker

Wrong: when two writes land at the same timestamp, the register keeps
whichever it processed last, which differs per node.

What happens: replicas diverge on a tie even though they applied the same set
of operations. Commutativity is violated.

Fix: always include a deterministic tie-breaker (node ID, replica ID, or
a monotonic sequence number) so the same pair of writes always resolves to
the same winner regardless of processing order.

### Sharing Underlying Maps Between Clones

Wrong: `Clone` copies the outer struct but not the inner map, so both the
original and clone point to the same `map[string]int64`.

What happens: merging the clone mutates the original; tests pass only because
both references see the same state.

Fix: deep-copy every nested map or slice in `Clone`. The implementations above
use two-level copies for ORSet and explicit `make`+copy loops for GCounter.

## Verification

From `~/go-exercises/crdt`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five commands must pass. `go test -race` catches data races in tests that
exercise concurrent merge paths.

Your turn: add `TestGCounterIgnoresNonPositiveDelta` -- call
`g.Increment("n1", 0)` and `g.Increment("n1", -1)` and verify `g.Value()`
remains 0.

## Summary

- A CRDT's merge must be commutative, associative, and idempotent; violating
  any law allows replicas to diverge without a detectable error.
- G-Counter: per-node counts, merge = per-node max, value = sum.
- PN-Counter: two G-Counters (increments and decrements), value = diff.
- G-Set: grow-only set, merge = union.
- OR-Set: unique add-tokens, remove deletes observed tokens only; add-wins on
  concurrent add+remove.
- LWW-Register: highest (timestamp, nodeID) pair wins; tie-breaker is
  mandatory for determinism.
- CRDTs give availability and partition tolerance at the cost of
  expressiveness; constraints that require cross-replica coordination (e.g.
  "never below zero") need additional mechanisms.

## What's Next

Next: [Merkle Tree](../10-merkle-tree/10-merkle-tree.md).

## Resources

- [A Comprehensive Study of Convergent and Commutative Replicated Data Types (Shapiro et al., 2011)](https://hal.inria.fr/inria-00555588/document) -- the foundational survey; defines the semilattice properties formally.
- [CRDTs: Consistency without concurrency control (Preguica et al.)](https://arxiv.org/abs/0907.0929) -- an accessible formal treatment of state-based CRDTs.
- [Go maps in action](https://go.dev/blog/maps) -- how Go maps behave (zero values, iteration order, nil map panics); relevant to every CRDT implementation here.
- [pkg.go.dev/fmt](https://pkg.go.dev/fmt) -- Sprintf and Println signatures used in the Example functions.
- [Automerge](https://automerge.org/) -- a production CRDT library for JSON documents; useful for seeing how the concepts scale.
