# 5. Vector Clocks and Causality

Vector clocks are the minimal data structure that lets a distributed system distinguish "A caused B" from "A and B happened independently". Wall-clock time fails at this: two nodes can agree on the current time and still disagree about which event came first. Lamport timestamps give a total order but cannot tell you whether the order reflects actual causality or is an arbitrary tiebreak. Vector clocks capture the exact partial order that causality imposes, at the cost of O(n) space per event where n is the number of nodes.

```text
vectorclocks/
  go.mod
  clock.go
  clock_test.go
  store.go
  store_test.go
  cmd/demo/main.go
```

## Concepts

### The Happens-Before Relation

Leslie Lamport defined happens-before (->) in 1978. For events a and b:

- If a and b are on the same process and a executes before b, then a -> b.
- If a is "send message m" and b is "receive message m", then a -> b.
- If a -> c and c -> b, then a -> b (transitivity).

If neither a -> b nor b -> a holds, the events are concurrent (written a || b). Concurrency here means "no causal link exists", not "they happened at the same wall-clock instant".

### Vector Clock Mechanics

Assign every node an integer counter. A vector clock is a map from node ID to that node's counter. The rules:

- **Local event**: node i increments vc[i].
- **Send**: node i increments vc[i] and attaches vc to the message.
- **Receive**: node i sets vc[j] = max(vc[j], msg[j]) for all j, then increments vc[i].

The comparison that encodes the happens-before relation: vc(a) < vc(b) if and only if for every node j, vc(a)[j] <= vc(b)[j], and there exists at least one j where vc(a)[j] < vc(b)[j]. This is a partial order; not every pair of clocks is comparable.

### Concurrency as a Conflict Signal

Two vector clocks are concurrent when neither is less-than-or-equal-to the other. In a replicated key-value store this means two writes to the same key happened without either node seeing the other's write — a true conflict. The store must surface both versions (called siblings in Riak) and let the application decide: last-writer-wins, merge, or manual resolution.

Dynamo (Amazon) and Riak both use this model. Each write carries the client's current vector clock; the server appends its own node counter before storing. On read, if only one version exists and its clock dominates all others, that version is authoritative. If multiple versions are concurrent, the client receives all of them.

### Why Not Lamport Timestamps?

A Lamport timestamp is a single integer. It satisfies: if a -> b then L(a) < L(b). But the converse does not hold: L(a) < L(b) does not imply a -> b; a and b may be concurrent. Vector clocks satisfy the biconditional: vc(a) < vc(b) if and only if a -> b. That extra power is what makes conflict detection possible.

### Practical Limits

Vector clock size grows with the number of distinct node IDs ever seen. Systems that replace or rename nodes accumulate stale entries. Dynamo introduced a pruning scheme (bounded vector clocks) that discards the oldest entries when the clock exceeds a size limit, at the cost of occasionally losing concurrency information and treating concurrent events as causally ordered.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/05-vector-clocks/05-vector-clocks/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/05-vector-clocks/05-vector-clocks
```

This is a library, not a program. Verify with `go test`.

### Exercise 1: The Vector Clock Type

Create `clock.go`:

```go
// clock.go
package vectorclocks

import "fmt"

// Clock is an immutable-by-convention vector clock: callers must call Copy
// before mutating if they need to preserve the original.
type Clock map[string]uint64

// New returns an empty Clock.
func New() Clock {
	return make(Clock)
}

// Increment returns a new Clock with nodeID's counter incremented by one.
func (c Clock) Increment(nodeID string) Clock {
	out := c.Copy()
	out[nodeID]++
	return out
}

// Merge returns a new Clock whose value at each node is the max of c and other.
// This is the receive operation: call Merge then Increment.
func (c Clock) Merge(other Clock) Clock {
	out := c.Copy()
	for k, v := range other {
		if v > out[k] {
			out[k] = v
		}
	}
	return out
}

// Copy returns a deep copy.
func (c Clock) Copy() Clock {
	out := make(Clock, len(c))
	for k, v := range c {
		out[k] = v
	}
	return out
}

// HappensBefore reports whether c causally precedes other.
// c < other iff forall j: c[j] <= other[j] AND exists j: c[j] < other[j].
func (c Clock) HappensBefore(other Clock) bool {
	dominated := false
	for k, cv := range c {
		ov := other[k]
		if cv > ov {
			return false
		}
		if cv < ov {
			dominated = true
		}
	}
	// Check keys in other that are not in c (treat missing as 0).
	for k, ov := range other {
		if _, ok := c[k]; !ok && ov > 0 {
			dominated = true
		}
	}
	return dominated
}

// Equal reports whether c and other represent the same logical time.
func (c Clock) Equal(other Clock) bool {
	for k, cv := range c {
		if other[k] != cv {
			return false
		}
	}
	for k, ov := range other {
		if c[k] != ov {
			return false
		}
	}
	return true
}

// Concurrent reports whether c and other are causally unrelated.
func (c Clock) Concurrent(other Clock) bool {
	return !c.HappensBefore(other) && !other.HappensBefore(c) && !c.Equal(other)
}

// String returns a compact human-readable form, e.g. "{A:1 B:2}".
func (c Clock) String() string {
	if len(c) == 0 {
		return "{}"
	}
	// Stable output: not guaranteed order for map, so just format pairs.
	s := "{"
	first := true
	for k, v := range c {
		if !first {
			s += " "
		}
		s += fmt.Sprintf("%s:%d", k, v)
		first = false
	}
	return s + "}"
}
```

### Exercise 2: Test the Clock Contract

Create `clock_test.go`:

```go
// clock_test.go
package vectorclocks

import (
	"testing"
)

func TestNewClock(t *testing.T) {
	t.Parallel()

	c := New()
	if len(c) != 0 {
		t.Fatalf("New() len = %d, want 0", len(c))
	}
}

func TestIncrementDoesNotMutateOriginal(t *testing.T) {
	t.Parallel()

	c := New()
	c2 := c.Increment("A")
	if c["A"] != 0 {
		t.Fatalf("Increment mutated original: c[A] = %d", c["A"])
	}
	if c2["A"] != 1 {
		t.Fatalf("c2[A] = %d, want 1", c2["A"])
	}
}

func TestHappensBefore(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b Clock
		want bool
	}{
		{
			name: "empty clock does not happen before empty",
			a:    New(),
			b:    New(),
			want: false,
		},
		{
			name: "A:1 happens before A:2",
			a:    Clock{"A": 1},
			b:    Clock{"A": 2},
			want: true,
		},
		{
			name: "A:2 does not happen before A:1",
			a:    Clock{"A": 2},
			b:    Clock{"A": 1},
			want: false,
		},
		{
			name: "A:1 B:1 happens before A:2 B:2",
			a:    Clock{"A": 1, "B": 1},
			b:    Clock{"A": 2, "B": 2},
			want: true,
		},
		{
			name: "concurrent: A:1 B:0 vs A:0 B:1",
			a:    Clock{"A": 1, "B": 0},
			b:    Clock{"A": 0, "B": 1},
			want: false,
		},
		{
			name: "missing key treated as zero: A:1 happens before A:1 B:1",
			a:    Clock{"A": 1},
			b:    Clock{"A": 1, "B": 1},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.HappensBefore(tc.b); got != tc.want {
				t.Errorf("HappensBefore = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConcurrent(t *testing.T) {
	t.Parallel()

	a := Clock{"A": 1, "B": 0}
	b := Clock{"A": 0, "B": 1}
	if !a.Concurrent(b) {
		t.Errorf("expected %v || %v", a, b)
	}
	if b.Concurrent(a) != a.Concurrent(b) {
		t.Errorf("Concurrent must be symmetric")
	}
}

func TestMerge(t *testing.T) {
	t.Parallel()

	a := Clock{"A": 3, "B": 1}
	b := Clock{"A": 1, "B": 4, "C": 2}
	got := a.Merge(b)
	want := Clock{"A": 3, "B": 4, "C": 2}
	if !got.Equal(want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

func TestEqual(t *testing.T) {
	t.Parallel()

	c := Clock{"A": 1, "B": 2}
	if !c.Equal(Clock{"A": 1, "B": 2}) {
		t.Errorf("Equal returned false for identical clocks")
	}
	if c.Equal(Clock{"A": 1, "B": 3}) {
		t.Errorf("Equal returned true for different clocks")
	}
}

func ExampleClock_HappensBefore() {
	a := Clock{"A": 1, "B": 1}
	b := Clock{"A": 2, "B": 2}
	if a.HappensBefore(b) {
		_ = "a caused b"
	}
	// Output:
}
```

The `ExampleClock_HappensBefore` function has an empty `// Output:` comment because the function does not print anything; `go test` verifies it compiles and runs without panic.

### Exercise 3: Replicated Key-Value Store

Create `store.go`:

```go
// store.go
package vectorclocks

import "errors"

// ErrKeyNotFound is returned when Get is called on a key that has no versions.
var ErrKeyNotFound = errors.New("key not found")

// Version pairs a value with the vector clock of the write that created it.
type Version struct {
	Value string
	Clock Clock
}

// ReplicaNode is a single node in a replicated store.
// It is not safe for concurrent use from multiple goroutines.
type ReplicaNode struct {
	ID    string
	clock Clock
	store map[string][]Version // key -> concurrent versions (siblings)
}

// NodeID returns the node's identifier.
func (n *ReplicaNode) NodeID() string { return n.ID }

// NewNode constructs a ReplicaNode with the given ID.
func NewNode(id string) *ReplicaNode {
	return &ReplicaNode{
		ID:    id,
		clock: New(),
		store: make(map[string][]Version),
	}
}

// Put writes key=value, advancing this node's clock and pruning versions that
// are causally dominated by the new write.
func (n *ReplicaNode) Put(key, value string) {
	n.clock = n.clock.Increment(n.ID)
	newVer := Version{Value: value, Clock: n.clock.Copy()}

	existing := n.store[key]
	var survivors []Version
	for _, v := range existing {
		if !newVer.Clock.HappensBefore(v.Clock) && !newVer.Clock.Equal(v.Clock) {
			// v is not dominated by newVer and not equal — keep it only if
			// it is not dominated by newVer.
			if !v.Clock.HappensBefore(newVer.Clock) {
				survivors = append(survivors, v)
			}
		}
	}
	n.store[key] = append(survivors, newVer)
}

// Get returns all concurrent versions for key. Returns ErrKeyNotFound when
// no versions exist.
func (n *ReplicaNode) Get(key string) ([]Version, error) {
	vs, ok := n.store[key]
	if !ok || len(vs) == 0 {
		return nil, ErrKeyNotFound
	}
	out := make([]Version, len(vs))
	copy(out, vs)
	return out, nil
}

// Sync merges all of peer's versions into n. For each key, versions that are
// causally dominated are discarded; concurrent versions are kept as siblings.
func (n *ReplicaNode) Sync(peer *ReplicaNode) {
	// Advance our clock to reflect having seen peer's clock.
	n.clock = n.clock.Merge(peer.clock).Increment(n.ID)

	for key, peerVers := range peer.store {
		for _, pv := range peerVers {
			n.mergeVersion(key, pv)
		}
	}
}

func (n *ReplicaNode) mergeVersion(key string, incoming Version) {
	existing := n.store[key]
	dominated := false
	var survivors []Version
	for _, ev := range existing {
		if ev.Clock.HappensBefore(incoming.Clock) {
			// ev is dominated by incoming — discard ev.
			continue
		}
		if incoming.Clock.HappensBefore(ev.Clock) {
			// incoming is dominated — no need to add it.
			dominated = true
		}
		survivors = append(survivors, ev)
	}
	if !dominated {
		survivors = append(survivors, incoming)
	}
	n.store[key] = survivors
}
```

### Exercise 4: Test the Store and Conflict Detection

Create `store_test.go`:

```go
// store_test.go
package vectorclocks

import (
	"errors"
	"testing"
)

func TestPutAndGet(t *testing.T) {
	t.Parallel()

	n := NewNode("A")
	n.Put("x", "hello")
	vs, err := n.Get("x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(vs) != 1 || vs[0].Value != "hello" {
		t.Fatalf("Get = %v, want [{hello ...}]", vs)
	}
}

func TestGetKeyNotFound(t *testing.T) {
	t.Parallel()

	n := NewNode("A")
	_, err := n.Get("missing")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("err = %v, want ErrKeyNotFound", err)
	}
}

func TestCausalWriteCollapses(t *testing.T) {
	t.Parallel()

	// Two sequential writes on the same node: only the latest survives.
	n := NewNode("A")
	n.Put("x", "v1")
	n.Put("x", "v2")
	vs, err := n.Get("x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 version after causal write, got %d: %v", len(vs), vs)
	}
	if vs[0].Value != "v2" {
		t.Fatalf("value = %q, want v2", vs[0].Value)
	}
}

func TestConcurrentWritesProduceSiblings(t *testing.T) {
	t.Parallel()

	// Two nodes write to the same key without syncing first.
	// Neither write is aware of the other -> concurrent -> conflict.
	a := NewNode("A")
	b := NewNode("B")

	a.Put("x", "from-A")
	b.Put("x", "from-B")

	// Sync B's state into A.
	a.Sync(b)

	vs, err := a.Get("x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(vs) != 2 {
		t.Fatalf("expected 2 concurrent versions (siblings), got %d: %v", len(vs), vs)
	}
}

func TestSyncResolvesAfterCausalWrite(t *testing.T) {
	t.Parallel()

	// A writes, syncs to B, B writes. B's write is causally after A's.
	// After syncing back, only B's version should survive.
	a := NewNode("A")
	b := NewNode("B")

	a.Put("x", "from-A")
	b.Sync(a)
	b.Put("x", "from-B-after-sync")
	a.Sync(b)

	vs, err := a.Get("x")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("expected 1 version after causal resolution, got %d: %v", len(vs), vs)
	}
	if vs[0].Value != "from-B-after-sync" {
		t.Fatalf("value = %q, want from-B-after-sync", vs[0].Value)
	}
}

func TestLamportLimitation(t *testing.T) {
	t.Parallel()

	// Demonstrates that a higher Lamport integer does not imply causality.
	// Node A fires 10 local events; B fires 1. A's Lamport counter > B's,
	// but neither caused the other — they are concurrent.
	a := New()
	for i := 0; i < 10; i++ {
		a = a.Increment("A")
	}
	b := New().Increment("B")

	if !a.Concurrent(b) {
		t.Errorf("expected A and B to be concurrent; a=%v b=%v", a, b)
	}
	// A's A-component is larger, but that doesn't make A -> B.
	if a.HappensBefore(b) || b.HappensBefore(a) {
		t.Errorf("neither should happen-before the other")
	}
}

// Your turn: add TestThreeNodeConvergence.
// Create nodes A, B, and C. Have A and C each write to "k" without syncing.
// Sync both into B. Verify B has 2 siblings. Then have B write "resolved"
// (simulating a merge), sync back into A and C, and verify all three nodes
// agree on exactly 1 version with value "resolved".
```

### Exercise 5: Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"

	vc "example.com/vectorclocks"
)

func main() {
	fmt.Println("=== Clock basics ===")
	a := vc.New().Increment("A")
	b := vc.New().Increment("B")
	ab := a.Increment("A") // A does a second event

	fmt.Printf("a  = %v\n", a)
	fmt.Printf("b  = %v\n", b)
	fmt.Printf("ab = %v\n", ab)
	fmt.Printf("a happens-before ab: %v\n", a.HappensBefore(ab))
	fmt.Printf("a concurrent with b: %v\n", a.Concurrent(b))

	fmt.Println("\n=== Replicated store: concurrent writes produce siblings ===")
	nodeA := vc.NewNode("A")
	nodeB := vc.NewNode("B")

	nodeA.Put("color", "red")
	nodeB.Put("color", "blue")
	// Neither node has seen the other's write.

	nodeA.Sync(nodeB)
	versions, err := nodeA.Get("color")
	if err != nil {
		panic(err)
	}
	fmt.Printf("versions of 'color' after concurrent writes: %d sibling(s)\n", len(versions))
	for i, v := range versions {
		fmt.Printf("  [%d] value=%q clock=%v\n", i, v.Value, v.Clock)
	}

	fmt.Println("\n=== Causal write collapses siblings ===")
	nodeA.Put("color", "purple") // A resolves the conflict
	nodeB.Sync(nodeA)
	versions, err = nodeB.Get("color")
	if err != nil {
		panic(err)
	}
	fmt.Printf("versions of 'color' after resolution: %d\n", len(versions))
	if len(versions) == 1 {
		fmt.Printf("  resolved value: %q\n", versions[0].Value)
	}

	fmt.Println("\n=== ErrKeyNotFound ===")
	n := vc.NewNode("X")
	_, err = n.Get("nonexistent")
	if errors.Is(err, vc.ErrKeyNotFound) {
		fmt.Println("ErrKeyNotFound returned as expected")
	}
}
```

## Common Mistakes

### Mutating the Receiver Instead of Returning a New Clock

Wrong: methods like `Increment` and `Merge` modify the map in place, so the caller's original clock changes without any visible assignment.

What happens: code that stores the "before" and "after" clock to compare them finds both variables point to the same map. Tests that check immutability fail silently because the original and the copy are indistinguishable.

Fix: always return a new `Clock` from `Increment` and `Merge`, copying the map first. The lesson's `Copy()` helper makes this a one-liner.

### Treating Missing Keys as Infinity Instead of Zero

Wrong: when comparing two clocks where one lacks a key, the missing entry is treated as infinity, causing `HappensBefore` to return false for clocks that do happen-before.

What happens: nodes with a strict subset of keys never compare as less-than nodes with a superset, so every pair of events appears concurrent even when one clearly caused the other.

Fix: treat a missing key as counter value 0. The comparison `c[k]` on a Go map returns the zero value (0 for uint64) when k is absent, which is the correct default.

### Forgetting to Increment After Merge on Receive

Wrong: on receive, the node only merges the incoming clock without incrementing its own counter. The receive event is then invisible in the node's clock.

What happens: a message received on node B shows the sender's causal history but not the receive event itself. Later recipients cannot tell whether B processed the message.

Fix: after merging, always increment the receiving node's own counter: `clock = clock.Merge(msg.Clock).Increment(myID)`.

### Using a Single Lamport Counter for Conflict Detection

Wrong: replace the vector clock with a single integer and use `>` to decide which version is newer.

What happens: two concurrent writes have different integers (because one node's counter is higher), so the system picks one as "newer" and silently discards the other. There is no conflict signal.

Fix: use vector clocks. Only drop a version when its clock is strictly dominated by another. If neither dominates, keep both as siblings.

## Verification

From `~/go-exercises/vectorclocks`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. Add the `TestThreeNodeConvergence` test described in Exercise 4 before marking this lesson done.

## Summary

- Vector clocks assign one counter per node; each event increments the local counter.
- Send attaches the clock; receive merges (component-wise max) then increments.
- vc(a) < vc(b) if and only if a causally preceded b — the biconditional that Lamport timestamps cannot provide.
- Two clocks that are incomparable represent concurrent events — a true conflict in a replicated store.
- A replicated store keeps all concurrent versions (siblings) and surfaces them for application-level resolution.
- Lamport timestamps give a total order but lose concurrency information; they cannot detect whether two events are causally related or merely ordered by the tiebreaker.
- Vector clock size grows with the number of distinct node IDs; practical systems bound the size with pruning.

## What's Next

Next: [Raft Leader Election](../06-raft-leader-election/06-raft-leader-election.md).

## Resources

- [Time, Clocks, and the Ordering of Events in a Distributed System — Lamport 1978](https://lamport.azurewebsites.net/pubs/time-clocks.pdf)
- [Dynamo: Amazon's Highly Available Key-Value Store — DeCandia et al. 2007](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf)
- [Why Vector Clocks Are Hard — Basho 2010](https://riak.com/posts/technical/why-vector-clocks-are-hard/)
- [pkg.go.dev/maps — Go standard library map operations](https://pkg.go.dev/maps)
- [Go Specification: Map types](https://go.dev/ref/spec#Map_types)
