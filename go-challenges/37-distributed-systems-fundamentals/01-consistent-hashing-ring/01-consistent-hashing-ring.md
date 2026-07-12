# 1. Consistent Hashing Ring

Naive partitioning with `hash(key) % N` moves nearly every key when a node is added or removed. Consistent hashing solves this by placing both keys and nodes on a fixed-size ring so that a topology change only disturbs the keys in a node's immediate neighborhood. Virtual nodes give each physical node multiple ring positions, which smooths out the load distribution without coupling it to the number of physical nodes. This is the core mechanism behind DynamoDB, Cassandra, and Memcached.

The hard part is not the idea but the details: maintaining a sorted position list, binary-searching it correctly with wrap-around, removing all virtual positions for a node without corrupting the ring, and collecting distinct physical nodes for replication.

```text
ring/
  go.mod
  ring.go
  ring_test.go
  cmd/demo/main.go
```

## Concepts

### The Ring Model

A consistent hash ring is a fixed address space — conventionally `[0, 2^32)` for a 32-bit hash or `[0, 2^64)` for 64-bit. Both keys and nodes are hashed into this space. A key's owner is the first node encountered when walking clockwise from the key's hash position. When a new node is added it takes ownership of the segment between itself and its predecessor; when a node is removed its successor absorbs its segment. In either case only one contiguous segment moves, so the fraction of disturbed keys is `1 / N` on average.

```
ring positions (clockwise):
  ... A ... B ... key ... C ...
          ^^^
      key maps to C (first node clockwise after key)
```

### Virtual Nodes

With a small number of physical nodes the ring positions cluster, causing highly uneven load: one node may own 60% of the ring while another owns 5%. Virtual nodes fix this by mapping each physical node to `V` distinct ring positions. A key still maps to the first clockwise position; that position resolves back to its physical node. With `V = 150` the standard deviation of load across nodes drops from O(1/sqrt(N)) to roughly 10% of the mean even with five nodes.

The trade-off: larger `V` improves distribution but increases the memory cost of the sorted position list (O(N * V) integers and strings) and the cost of a removal (must filter all V positions for that node).

### Binary Search With Wrap-Around

`sort.Search` returns the first index `i` in `[0, n)` where `f(i)` is true. For the ring:

```go
i := sort.Search(len(r.positions), func(i int) bool {
	return r.positions[i] >= h
})
if i == len(r.positions) {
	i = 0  // wrap: key falls past the last position, use the first
}
```

If the key hash equals a position exactly, `sort.Search` finds it directly. If it falls between two positions, the search returns the next one clockwise. If it falls past the last position, the wrap sends it to index 0.

### Replication: Collecting Distinct Physical Nodes

`GetNodes(key, count)` starts at the owner and walks clockwise, skipping virtual-node positions that resolve to a physical node already collected. It stops when `count` distinct physical nodes are found or the ring is exhausted. The walk must handle wrap-around correctly: start at index `i`, and use `(i + offset) % len(positions)` to step through the ring.

### Failure Modes

- Empty ring: `GetNode` on an empty ring returns `""`. Callers must check.
- `count > len(physical nodes)`: `GetNodes` returns fewer than `count` nodes; it does not loop forever because the seen-set bounds the walk.
- Concurrent reads and writes: the ring stores a sorted slice that is mutated on `AddNode`/`RemoveNode`. Any concurrent access needs external synchronization or a `sync.RWMutex`.

## Exercises

This is a library, not a program: there is no `main` at the package level. Verification uses `go test`.

### Exercise 1: The Ring Type and Constructor

Create `ring.go`:

```go
// ring.go
package ring

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

// Ring is a consistent hash ring. The zero value is not usable; use New.
type Ring struct {
	mu           sync.RWMutex
	virtualNodes int
	positions    []uint32          // sorted ring positions
	nodeOf       map[uint32]string // position -> physical node name
}

// New returns a Ring with the given number of virtual nodes per physical node.
// virtualNodes must be at least 1; values between 100 and 200 give good
// distribution with typical cluster sizes.
func New(virtualNodes int) *Ring {
	if virtualNodes < 1 {
		virtualNodes = 1
	}
	return &Ring{
		virtualNodes: virtualNodes,
		nodeOf:       make(map[uint32]string),
	}
}

// hash returns a uint32 position for the given key string.
func hash(key string) uint32 {
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint32(sum[:4])
}

// virtualKey returns the ring key for virtual node index i of a physical node.
func virtualKey(node string, i int) string {
	return fmt.Sprintf("%s#%d", node, i)
}
```

The `sync.RWMutex` lets multiple goroutines call `GetNode` concurrently while `AddNode` and `RemoveNode` take an exclusive write lock.

### Exercise 2: Add, Remove, and Lookup

Append to `ring.go`:

```go
// AddNode places the node's virtual positions onto the ring.
// If the node is already present its positions are replaced.
func (r *Ring) AddNode(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeNodeLocked(node)
	for i := range r.virtualNodes {
		pos := hash(virtualKey(node, i))
		r.positions = append(r.positions, pos)
		r.nodeOf[pos] = node
	}
	sort.Slice(r.positions, func(a, b int) bool {
		return r.positions[a] < r.positions[b]
	})
}

// RemoveNode removes all virtual positions of the named node from the ring.
func (r *Ring) RemoveNode(node string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removeNodeLocked(node)
}

// removeNodeLocked removes virtual positions for node. Must be called with r.mu held.
func (r *Ring) removeNodeLocked(node string) {
	kept := r.positions[:0]
	for _, pos := range r.positions {
		if r.nodeOf[pos] == node {
			delete(r.nodeOf, pos)
		} else {
			kept = append(kept, pos)
		}
	}
	r.positions = kept
}

// GetNode returns the physical node responsible for the given key.
// Returns "" if the ring is empty.
func (r *Ring) GetNode(key string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.positions) == 0 {
		return ""
	}
	h := hash(key)
	i := sort.Search(len(r.positions), func(i int) bool {
		return r.positions[i] >= h
	})
	if i == len(r.positions) {
		i = 0
	}
	return r.nodeOf[r.positions[i]]
}

// GetNodes returns up to count distinct physical nodes responsible for key,
// walking clockwise from the key's position. Used for replication.
func (r *Ring) GetNodes(key string, count int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.positions) == 0 || count <= 0 {
		return nil
	}
	h := hash(key)
	start := sort.Search(len(r.positions), func(i int) bool {
		return r.positions[i] >= h
	})
	if start == len(r.positions) {
		start = 0
	}
	seen := make(map[string]bool)
	var result []string
	for offset := range len(r.positions) {
		idx := (start + offset) % len(r.positions)
		node := r.nodeOf[r.positions[idx]]
		if !seen[node] {
			seen[node] = true
			result = append(result, node)
			if len(result) == count {
				break
			}
		}
	}
	return result
}

// Len returns the number of virtual positions on the ring.
func (r *Ring) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.positions)
}
```

### Exercise 3: Test the Ring Contract

Create `ring_test.go`:

```go
// ring_test.go
package ring

import (
	"fmt"
	"slices"
	"testing"
)

func TestEmptyRingReturnsEmpty(t *testing.T) {
	t.Parallel()
	r := New(10)
	if got := r.GetNode("anykey"); got != "" {
		t.Fatalf("empty ring: GetNode = %q, want empty string", got)
	}
}

func TestAddNodeAndGetNode(t *testing.T) {
	t.Parallel()
	r := New(50)
	r.AddNode("nodeA")
	got := r.GetNode("hello")
	if got != "nodeA" {
		t.Fatalf("GetNode = %q, want nodeA", got)
	}
}

func TestGetNodeIsDeterministic(t *testing.T) {
	t.Parallel()
	r := New(100)
	r.AddNode("a")
	r.AddNode("b")
	r.AddNode("c")
	first := r.GetNode("mykey")
	for range 10 {
		if got := r.GetNode("mykey"); got != first {
			t.Fatalf("GetNode not deterministic: %q then %q", first, got)
		}
	}
}

func TestRemoveNodeRedistributes(t *testing.T) {
	t.Parallel()
	r := New(150)
	nodes := []string{"a", "b", "c"}
	for _, n := range nodes {
		r.AddNode(n)
	}
	// Collect owners for 1000 keys before removal.
	keys := make([]string, 1000)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}
	before := make(map[string]string, len(keys))
	for _, k := range keys {
		before[k] = r.GetNode(k)
	}
	r.RemoveNode("b")
	// After removing "b", every key that was on "b" must now be on "a" or "c".
	// Keys on "a" or "c" must not move.
	for _, k := range keys {
		after := r.GetNode(k)
		if before[k] != "b" && after != before[k] {
			t.Fatalf("key %q moved from %q to %q without its node being removed", k, before[k], after)
		}
		if after == "b" {
			t.Fatalf("key %q still maps to removed node b", k)
		}
	}
}

func TestGetNodesReturnsDistinctPhysicalNodes(t *testing.T) {
	t.Parallel()
	r := New(100)
	for _, n := range []string{"a", "b", "c", "d"} {
		r.AddNode(n)
	}
	nodes := r.GetNodes("somekey", 3)
	if len(nodes) != 3 {
		t.Fatalf("GetNodes returned %d nodes, want 3", len(nodes))
	}
	seen := make(map[string]bool)
	for _, n := range nodes {
		if seen[n] {
			t.Fatalf("GetNodes returned duplicate node %q: %v", n, nodes)
		}
		seen[n] = true
	}
}

func TestGetNodesCountExceedsPhysicalNodes(t *testing.T) {
	t.Parallel()
	r := New(50)
	r.AddNode("x")
	r.AddNode("y")
	nodes := r.GetNodes("key", 10)
	// Should return at most 2 (the actual number of physical nodes), not loop forever.
	if len(nodes) > 2 {
		t.Fatalf("GetNodes returned %d nodes, want at most 2", len(nodes))
	}
}

func TestAddNodeIsIdempotent(t *testing.T) {
	t.Parallel()
	r := New(50)
	r.AddNode("a")
	lenBefore := r.Len()
	r.AddNode("a")
	if r.Len() != lenBefore {
		t.Fatalf("second AddNode(\"a\") changed Len from %d to %d", lenBefore, r.Len())
	}
}

func TestDistributionWithVirtualNodes(t *testing.T) {
	t.Parallel()
	const numKeys = 10_000
	r := New(150)
	nodeNames := []string{"n1", "n2", "n3", "n4", "n5"}
	for _, n := range nodeNames {
		r.AddNode(n)
	}
	counts := make(map[string]int, len(nodeNames))
	for i := range numKeys {
		key := fmt.Sprintf("key-%d", i)
		counts[r.GetNode(key)]++
	}
	// Each node should own between 10% and 30% of keys (ideal is 20%).
	for _, n := range nodeNames {
		frac := float64(counts[n]) / numKeys
		if frac < 0.10 || frac > 0.30 {
			t.Errorf("node %q owns %.1f%% of keys (want 10%%–30%%)", n, frac*100)
		}
	}
}

func TestKeyMovementOnAddNode(t *testing.T) {
	t.Parallel()
	const numKeys = 10_000
	r := New(150)
	initial := []string{"n1", "n2", "n3", "n4", "n5"}
	for _, n := range initial {
		r.AddNode(n)
	}
	keys := make([]string, numKeys)
	before := make(map[string]string, numKeys)
	for i := range numKeys {
		keys[i] = fmt.Sprintf("key-%d", i)
		before[keys[i]] = r.GetNode(keys[i])
	}
	r.AddNode("n6")
	moved := 0
	for _, k := range keys {
		if r.GetNode(k) != before[k] {
			moved++
		}
	}
	// Expect ~1/6 of keys to move (within factor of 3 for statistical tolerance).
	lo, hi := numKeys/18, numKeys*3/6
	if moved < lo || moved > hi {
		t.Errorf("added 1 node to 5: %d/%d keys moved (want %d–%d)", moved, numKeys, lo, hi)
	}
}

func TestVirtualNodeCount(t *testing.T) {
	t.Parallel()
	r := New(10)
	r.AddNode("a")
	r.AddNode("b")
	if got := r.Len(); got != 20 {
		t.Fatalf("Len = %d, want 20 (2 nodes * 10 virtual)", got)
	}
	r.RemoveNode("a")
	if got := r.Len(); got != 10 {
		t.Fatalf("Len after remove = %d, want 10", got)
	}
}

func ExampleRing_GetNodes() {
	r := New(100)
	r.AddNode("cache-1")
	r.AddNode("cache-2")
	r.AddNode("cache-3")
	nodes := r.GetNodes("user:42", 2)
	// Nodes are deterministic for a given key and ring state.
	if len(nodes) == 2 && !slices.Contains(nodes, "") {
		fmt.Println("ok: 2 distinct nodes returned")
	}
	// Output: ok: 2 distinct nodes returned
}
```

Your turn: add `TestGetNodeReturnsEmptyAfterAllNodesRemoved` — add two nodes, remove both, then call `GetNode("k")` and assert the result is `""`.

### Exercise 4: Runnable Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"math/rand/v2"

	"example.com/ring"
)

func main() {
	const (
		numKeys      = 20_000
		virtualNodes = 150
	)

	r := ring.New(virtualNodes)
	nodes := []string{"cache-1", "cache-2", "cache-3", "cache-4", "cache-5"}
	for _, n := range nodes {
		r.AddNode(n)
	}

	// Distribution across 5 nodes.
	counts := make(map[string]int, len(nodes))
	keys := make([]string, numKeys)
	for i := range numKeys {
		keys[i] = fmt.Sprintf("key-%d", rand.N(1_000_000))
		counts[r.GetNode(keys[i])]++
	}
	fmt.Println("=== Distribution (5 nodes, 20 000 keys) ===")
	for _, n := range nodes {
		bar := ""
		for range counts[n] / 200 {
			bar += "#"
		}
		fmt.Printf("  %-10s %5d  %s\n", n, counts[n], bar)
	}

	// Key movement when adding a 6th node.
	before := make(map[string]string, numKeys)
	for _, k := range keys {
		before[k] = r.GetNode(k)
	}
	r.AddNode("cache-6")
	moved := 0
	for _, k := range keys {
		if r.GetNode(k) != before[k] {
			moved++
		}
	}
	fmt.Printf("\n=== After adding cache-6 ===\n")
	fmt.Printf("  Keys moved: %d/%d (%.1f%%)\n", moved, numKeys, float64(moved)/numKeys*100)
	fmt.Printf("  Expected ~%.1f%% (1/6)\n", 100.0/6)

	// Replication: first 3 replicas for one key.
	fmt.Println("\n=== Replication for key \"user:42\" ===")
	replicas := r.GetNodes("user:42", 3)
	for i, n := range replicas {
		fmt.Printf("  replica %d: %s\n", i+1, n)
	}
}
```

Run it with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Off-by-one in the wrap-around

Wrong: `i := sort.Search(...); return r.nodeOf[r.positions[i]]` — if `i == len(r.positions)`, this panics with an index out of range.

Fix: always check `if i == len(r.positions) { i = 0 }` before indexing. The key hash is larger than every position on the ring, so it wraps to the first position.

### Leaving stale positions after `AddNode` on an existing node

Wrong: calling `AddNode("a")` twice appends another `V` positions without removing the first `V`. The ring then has `2V` positions for node `a`, doubling its load.

Fix: call `removeNodeLocked` at the start of `AddNode` so the node always starts fresh.

### Sharing the backing array on `removeNodeLocked`

Wrong: `kept := r.positions` creates a slice header that shares the backing array. The in-place filter `kept = append(kept, pos)` will corrupt `r.positions` when `kept` is shorter.

Fix: `kept := r.positions[:0]` — this reuses the backing array intentionally for the filter, which is correct because `kept` always writes at or before the read cursor.

### `GetNodes` looping forever when count > physical nodes

Wrong: `for { walk... }` without a bound — the walk can revisit virtual positions indefinitely.

Fix: iterate at most `len(r.positions)` steps; the `seen` map guarantees at most `len(physicalNodes)` results, and the bounded loop ensures termination.

### Space-indented Go code

Wrong: using 4 spaces for indentation inside fenced blocks. `gofmt` requires tabs and `gofmt -l` will flag the file.

Fix: use a tab character for every indent level. Every fenced block in this lesson uses tabs.

## Verification

From `~/go-exercises/ring`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Add `TestGetNodeReturnsEmptyAfterAllNodesRemoved` before running `go test`.

## Summary

- Consistent hashing maps keys and nodes onto a fixed ring so that adding or removing a node only disturbs the keys in its immediate neighborhood (~1/N fraction).
- Virtual nodes give each physical node V ring positions, smoothing distribution from O(1/sqrt(N)) variance to roughly 10% of the mean at V = 150.
- Binary search with `sort.Search` plus a wrap-around check finds the owning node in O(log(N*V)) time.
- `GetNodes` collects distinct physical nodes for replication by walking clockwise and skipping already-seen physical nodes.
- A `sync.RWMutex` allows concurrent `GetNode` reads while serializing writes; without it the sorted slice is a data race.

## What's Next

Next: [Implementing a Gossip Protocol](../02-implementing-a-gossip-protocol/02-implementing-a-gossip-protocol.md).

## Resources

- [sort.Search documentation](https://pkg.go.dev/sort#Search) — the binary search primitive used for ring lookup
- [crypto/sha256](https://pkg.go.dev/crypto/sha256) — hash function used for node and key positions
- [Consistent Hashing and Random Trees (Karger et al., 1997)](https://dl.acm.org/doi/10.1145/258533.258660) — original paper
- [Dynamo: Amazon's Highly Available Key-Value Store (DeCandia et al., SOSP 2007)](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) — production use of consistent hashing with virtual nodes
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — concurrent read, exclusive write pattern used in the ring
