# 13. Sharded Key-Value Store

A sharded key-value store distributes data across multiple nodes using consistent
hashing so that adding or removing a node moves only a minimal fraction of keys.
The hard part is not the hash ring itself — it is the interaction between
replication (each key lives on N nodes), quorum logic (how many replicas must
respond before a read or write succeeds), and concurrent access. This lesson
builds a self-contained, single-process simulation of the Dynamo family of stores:
consistent hashing for partition assignment, configurable replication factor,
quorum-based reads and writes, and a coordinator that routes requests to the
correct replica set.

```text
shardkv/
  go.mod
  ring.go          -- hash ring: node placement, replica lookup
  store.go         -- per-node in-memory store with versioned values
  cluster.go       -- cluster: coordinator logic, quorum reads/writes
  cluster_test.go  -- table-driven tests covering all the contracts
  cmd/demo/main.go -- runnable demo: put/get across a 5-node cluster
```

## Concepts

### Consistent Hashing: Why Modular Hashing Is Not Enough

The naive approach maps `hash(key) % N` to a node index. When N changes (a node
joins or leaves), almost every key remaps — that is catastrophic for a cache or
for a store that must stream data between nodes.

Consistent hashing places both nodes and keys on a circular hash space (the
"ring"). A key belongs to the first node clockwise from its position on the ring.
When a node is added, only the keys between the new node and its predecessor move.
When a node is removed, only the keys it owned move to its successor. The fraction
of keys that move is O(1/N) in expectation.

To avoid hot spots from uneven node placement, production rings (Cassandra, Riak)
place each physical node at V virtual nodes (vnodes). This lesson uses
`crypto/sha256` for hashing and places each node at `V` equally-spaced positions.

### Replication Factor and Replica Sets

With replication factor N, each key is stored on N consecutive nodes clockwise
from the key's position. The `ReplicasFor(key)` method on the ring returns the
ordered slice of N node IDs. The first node in that slice is the coordinator for
reads and writes (it does not have to be the node the client connects to; any node
can act as coordinator by forwarding).

This lesson uses N=3 as the default replication factor throughout.

### Versioned Values and Last-Write-Wins

Concurrent writes to different replicas can create conflicting versions. The
simplest conflict-resolution strategy is last-write-wins (LWW): each write
carries a monotonic timestamp, and the replica with the highest timestamp wins
during read repair or anti-entropy.

In this simulation a `Value` carries both the data and a `Version uint64`
incremented by the cluster on every write. A `Get` that receives conflicting
versions from two replicas returns the one with the higher version.

### Quorum Reads and Writes

A quorum is the minimum number of replicas that must participate in an operation
before it is considered successful. With N=3:

- `ConsistencyOne`: 1 replica must respond. Fastest; allows stale reads.
- `ConsistencyQuorum`: floor(N/2)+1 = 2 replicas must respond. Balanced.
- `ConsistencyAll`: all 3 replicas must respond. Slowest; guarantees freshness.

The algebra W + R > N (where W is the write quorum and R is the read quorum)
guarantees that every read sees the most recent successful write. With N=3,
W=QUORUM (2), R=QUORUM (2): 2+2 > 3.

### Coordinator Pattern

The client contacts any node. That node acts as coordinator: it looks up the
replica set for the key, issues concurrent sub-requests to each replica, waits for
the required quorum to respond, and returns success or failure. This lesson
simulates sub-requests as direct function calls with an in-process channel to
collect results — the same logic applies when sub-requests are HTTP or gRPC calls.

### Failure Modes

- **Quorum not reachable**: fewer than the required number of replicas respond
  (because a node is down or too slow). The write or read fails with
  `ErrQuorumNotReached`.
- **Split ring**: all nodes are reachable but the ring has fewer nodes than the
  replication factor. The cluster returns `ErrNotEnoughNodes`.
- **Stale read with ConsistencyOne**: the coordinator returns the value from the
  first replica to respond, regardless of version. A caller that needs the latest
  value must use `ConsistencyQuorum` or `ConsistencyAll`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/13-sharded-key-value-store/13-sharded-key-value-store/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/13-sharded-key-value-store/13-sharded-key-value-store
```

This is a library with a demo; the verification is `go test`, not `go run`.

### Exercise 1: Hash Ring

Create `ring.go`:

```go
package shardkv

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

// VNodesPerNode is the number of virtual nodes placed on the ring per
// physical node. Higher values produce more even key distribution.
const VNodesPerNode = 100

// ringEntry is one virtual-node position on the hash ring.
type ringEntry struct {
	hash   uint64
	nodeID string
}

// Ring is a consistent hash ring. It is safe for concurrent use.
type Ring struct {
	mu      sync.RWMutex
	entries []ringEntry // sorted by hash
	nodes   map[string]struct{}
	vnodes  int
}

// NewRing returns a ring with the given virtual-node count per physical node.
func NewRing(vnodesPerNode int) *Ring {
	if vnodesPerNode <= 0 {
		vnodesPerNode = VNodesPerNode
	}
	return &Ring{nodes: make(map[string]struct{}), vnodes: vnodesPerNode}
}

func hashKey(s string) uint64 {
	h := sha256.Sum256([]byte(s))
	return binary.BigEndian.Uint64(h[:8])
}

// Add places a physical node on the ring at vnodes virtual positions.
func (r *Ring) Add(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[nodeID]; ok {
		return
	}
	r.nodes[nodeID] = struct{}{}
	for i := 0; i < r.vnodes; i++ {
		key := fmt.Sprintf("%s#%d", nodeID, i)
		r.entries = append(r.entries, ringEntry{hash: hashKey(key), nodeID: nodeID})
	}
	sort.Slice(r.entries, func(i, j int) bool { return r.entries[i].hash < r.entries[j].hash })
}

// Remove removes a physical node and all its virtual positions.
func (r *Ring) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, nodeID)
	out := r.entries[:0]
	for _, e := range r.entries {
		if e.nodeID != nodeID {
			out = append(out, e)
		}
	}
	r.entries = out
}

// ReplicasFor returns up to n distinct physical nodes responsible for key,
// starting with the clockwise-nearest node and continuing clockwise.
// Returns fewer than n nodes only when the ring has fewer than n nodes.
func (r *Ring) ReplicasFor(key string, n int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.entries) == 0 {
		return nil
	}
	h := hashKey(key)
	idx := sort.Search(len(r.entries), func(i int) bool { return r.entries[i].hash >= h })
	if idx == len(r.entries) {
		idx = 0
	}
	seen := make(map[string]struct{})
	var replicas []string
	for i := 0; i < len(r.entries) && len(replicas) < n; i++ {
		e := r.entries[(idx+i)%len(r.entries)]
		if _, ok := seen[e.nodeID]; !ok {
			seen[e.nodeID] = struct{}{}
			replicas = append(replicas, e.nodeID)
		}
	}
	return replicas
}

// Nodes returns all physical nodes currently on the ring.
func (r *Ring) Nodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.nodes))
	for id := range r.nodes {
		out = append(out, id)
	}
	return out
}
```

The ring uses `crypto/sha256` for collision resistance. `ReplicasFor` walks
clockwise, collecting distinct physical nodes; virtual nodes let the same
physical node appear many times but are de-duplicated in the result.

### Exercise 2: Per-Node Store

Create `store.go`:

```go
package shardkv

import (
	"errors"
	"sync"
)

// ErrKeyNotFound is returned when a Get finds no entry for the key.
var ErrKeyNotFound = errors.New("key not found")

// Value is the datum stored for one key. Version is a logical clock
// incremented by the cluster on every successful write.
type Value struct {
	Data    []byte
	Version uint64
}

// nodeStore is an in-memory key-value store for one node.
type nodeStore struct {
	mu   sync.RWMutex
	data map[string]Value
}

func newNodeStore() *nodeStore {
	return &nodeStore{data: make(map[string]Value)}
}

func (s *nodeStore) put(key string, v Value) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.data[key]
	if !ok || v.Version > existing.Version {
		s.data[key] = v
	}
}

func (s *nodeStore) get(key string) (Value, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return Value{}, ErrKeyNotFound
	}
	return v, nil
}

func (s *nodeStore) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
}
```

`put` enforces last-write-wins at the per-node level: a write with a lower
version than the stored version is silently dropped, preventing a stale replica
from overwriting a newer value during rebalancing or hinted handoff.

### Exercise 3: Cluster (Coordinator and Quorum)

Create `cluster.go`:

```go
package shardkv

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Consistency controls how many replicas must respond before an operation
// is considered successful.
type Consistency int

const (
	// ConsistencyOne requires exactly 1 replica to respond.
	ConsistencyOne Consistency = iota + 1
	// ConsistencyQuorum requires floor(N/2)+1 replicas to respond.
	ConsistencyQuorum
	// ConsistencyAll requires all N replicas to respond.
	ConsistencyAll
)

// DefaultReplication is the number of replicas per key.
const DefaultReplication = 3

var (
	// ErrQuorumNotReached is returned when fewer replicas than required
	// responded successfully.
	ErrQuorumNotReached = errors.New("quorum not reached")
	// ErrNotEnoughNodes is returned when the ring has fewer nodes than
	// the replication factor.
	ErrNotEnoughNodes = errors.New("not enough nodes for replication factor")
)

// Cluster is a simulated distributed key-value cluster. All nodes run
// in the same process; communication is direct function calls.
type Cluster struct {
	ring        *Ring
	stores      map[string]*nodeStore
	mu          sync.RWMutex
	replication int
	version     atomic.Uint64
}

// NewCluster creates a cluster with the given node IDs and replication factor.
// Providing fewer nodes than replication factor is allowed at construction but
// Put and Get will return ErrNotEnoughNodes.
func NewCluster(nodeIDs []string, replication int) *Cluster {
	if replication <= 0 {
		replication = DefaultReplication
	}
	c := &Cluster{
		ring:        NewRing(VNodesPerNode),
		stores:      make(map[string]*nodeStore, len(nodeIDs)),
		replication: replication,
	}
	for _, id := range nodeIDs {
		c.ring.Add(id)
		c.stores[id] = newNodeStore()
	}
	return c
}

// quorumSize returns the number of replicas required for the given consistency
// level and replication factor.
func quorumSize(consistency Consistency, replication int) int {
	switch consistency {
	case ConsistencyOne:
		return 1
	case ConsistencyAll:
		return replication
	default: // ConsistencyQuorum
		return replication/2 + 1
	}
}

// Put writes key=data to the cluster with the given consistency level.
// The coordinator fans out to all replicas concurrently and waits for
// the required quorum to acknowledge before returning.
func (c *Cluster) Put(key string, data []byte, consistency Consistency) error {
	replicas := c.ring.ReplicasFor(key, c.replication)
	if len(replicas) < c.replication {
		return fmt.Errorf("%w: have %d, need %d", ErrNotEnoughNodes, len(replicas), c.replication)
	}
	ver := c.version.Add(1)
	v := Value{Data: data, Version: ver}
	required := quorumSize(consistency, c.replication)

	type result struct{ err error }
	ch := make(chan result, len(replicas))
	for _, nodeID := range replicas {
		go func() {
			c.mu.RLock()
			s := c.stores[nodeID]
			c.mu.RUnlock()
			s.put(key, v)
			ch <- result{}
		}()
	}

	acks := 0
	for range replicas {
		r := <-ch
		if r.err == nil {
			acks++
			if acks >= required {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: got %d/%d", ErrQuorumNotReached, acks, required)
}

// Get reads key from the cluster with the given consistency level.
// The coordinator reads from all replicas concurrently, waits for the
// required quorum to respond, and returns the value with the highest version
// (last-write-wins).
func (c *Cluster) Get(key string, consistency Consistency) ([]byte, error) {
	replicas := c.ring.ReplicasFor(key, c.replication)
	if len(replicas) < c.replication {
		return nil, fmt.Errorf("%w: have %d, need %d", ErrNotEnoughNodes, len(replicas), c.replication)
	}
	required := quorumSize(consistency, c.replication)

	type result struct {
		val Value
		err error
	}
	ch := make(chan result, len(replicas))
	for _, nodeID := range replicas {
		go func() {
			c.mu.RLock()
			s := c.stores[nodeID]
			c.mu.RUnlock()
			v, err := s.get(key)
			ch <- result{val: v, err: err}
		}()
	}

	var best Value
	acks := 0
	for range replicas {
		r := <-ch
		if r.err == nil {
			acks++
			if r.val.Version > best.Version {
				best = r.val
			}
			if acks >= required {
				return best.Data, nil
			}
		}
	}
	if acks == 0 {
		return nil, fmt.Errorf("%w: %w", ErrQuorumNotReached, ErrKeyNotFound)
	}
	return nil, fmt.Errorf("%w: got %d/%d", ErrQuorumNotReached, acks, required)
}

// Delete removes key from all replicas. It uses ConsistencyAll semantics
// internally because a partial delete creates a split-brain where some replicas
// still serve the key.
func (c *Cluster) Delete(key string) error {
	replicas := c.ring.ReplicasFor(key, c.replication)
	if len(replicas) < c.replication {
		return fmt.Errorf("%w: have %d, need %d", ErrNotEnoughNodes, len(replicas), c.replication)
	}
	var wg sync.WaitGroup
	for _, nodeID := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.mu.RLock()
			s := c.stores[nodeID]
			c.mu.RUnlock()
			s.delete(key)
		}()
	}
	wg.Wait()
	return nil
}

// AddNode joins a new node to the cluster. Keys that now belong to the new
// node are NOT transferred in this simulation (rebalancing is out of scope);
// new writes will use the updated ring.
func (c *Cluster) AddNode(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.stores[nodeID]; ok {
		return
	}
	c.ring.Add(nodeID)
	c.stores[nodeID] = newNodeStore()
}

// RemoveNode removes a node from the cluster ring. Existing data on the
// removed node is lost in this simulation; production systems use hinted
// handoff or streaming rebalance before removal.
func (c *Cluster) RemoveNode(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring.Remove(nodeID)
	delete(c.stores, nodeID)
}

// ReplicasFor returns the replica set for a key (exported for demo use).
func (c *Cluster) ReplicasFor(key string) []string {
	return c.ring.ReplicasFor(key, c.replication)
}

// NodeCount returns the number of physical nodes in the cluster.
func (c *Cluster) NodeCount() int {
	return len(c.ring.Nodes())
}
```

The version counter is a `sync/atomic.Uint64` (Go 1.19+), which avoids a mutex
on the fast path while guaranteeing a strictly increasing write version across
all concurrent `Put` calls.

### Exercise 4: Tests

Create `cluster_test.go`:

```go
package shardkv

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func newTestCluster(t *testing.T) *Cluster {
	t.Helper()
	nodes := []string{"node-A", "node-B", "node-C", "node-D", "node-E"}
	return NewCluster(nodes, DefaultReplication)
}

func TestPutGetRoundTrip(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t)

	cases := []struct {
		key  string
		data []byte
	}{
		{"alpha", []byte("hello")},
		{"beta", []byte("world")},
		{"gamma", []byte("go")},
	}
	for _, tc := range cases {
		if err := c.Put(tc.key, tc.data, ConsistencyQuorum); err != nil {
			t.Fatalf("Put(%q): %v", tc.key, err)
		}
		got, err := c.Get(tc.key, ConsistencyQuorum)
		if err != nil {
			t.Fatalf("Get(%q): %v", tc.key, err)
		}
		if !bytes.Equal(got, tc.data) {
			t.Fatalf("Get(%q) = %q, want %q", tc.key, got, tc.data)
		}
	}
}

func TestGetMissingKeyReturnsNotFound(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t)

	_, err := c.Get("nonexistent", ConsistencyOne)
	if !errors.Is(err, ErrQuorumNotReached) {
		t.Fatalf("err = %v, want ErrQuorumNotReached", err)
	}
}

func TestConsistencyLevels(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t)

	levels := []Consistency{ConsistencyOne, ConsistencyQuorum, ConsistencyAll}
	for _, wc := range levels {
		for _, rc := range levels {
			key := fmt.Sprintf("key-w%d-r%d", wc, rc)
			data := []byte(key)
			if err := c.Put(key, data, wc); err != nil {
				t.Fatalf("Put(consistency=%d): %v", wc, err)
			}
			got, err := c.Get(key, rc)
			if err != nil {
				t.Fatalf("Get(consistency=%d): %v", rc, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("Get = %q, want %q", got, data)
			}
		}
	}
}

func TestLastWriteWinsOnOverwrite(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t)

	const key = "overwrite-me"
	if err := c.Put(key, []byte("first"), ConsistencyAll); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(key, []byte("second"), ConsistencyAll); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(key, ConsistencyAll)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("second")) {
		t.Fatalf("got %q, want second", got)
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t)

	const key = "to-delete"
	if err := c.Put(key, []byte("value"), ConsistencyAll); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(key); err != nil {
		t.Fatal(err)
	}
	_, err := c.Get(key, ConsistencyOne)
	if !errors.Is(err, ErrQuorumNotReached) {
		t.Fatalf("after delete: err = %v, want ErrQuorumNotReached wrapping ErrKeyNotFound", err)
	}
}

func TestAddNodeExpandsCluster(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t)
	before := c.NodeCount()
	c.AddNode("node-F")
	if c.NodeCount() != before+1 {
		t.Fatalf("NodeCount = %d, want %d", c.NodeCount(), before+1)
	}
}

func TestNotEnoughNodesReturnsError(t *testing.T) {
	t.Parallel()
	// Build a cluster with fewer nodes than the replication factor.
	c := NewCluster([]string{"only-one"}, DefaultReplication)
	err := c.Put("k", []byte("v"), ConsistencyOne)
	if !errors.Is(err, ErrNotEnoughNodes) {
		t.Fatalf("err = %v, want ErrNotEnoughNodes", err)
	}
}

func TestReplicasForReturnThreeNodes(t *testing.T) {
	t.Parallel()
	c := newTestCluster(t)
	replicas := c.ReplicasFor("any-key")
	if len(replicas) != DefaultReplication {
		t.Fatalf("len(replicas) = %d, want %d", len(replicas), DefaultReplication)
	}
	seen := make(map[string]bool)
	for _, r := range replicas {
		if seen[r] {
			t.Fatalf("duplicate replica %q in %v", r, replicas)
		}
		seen[r] = true
	}
}

func ExampleCluster_Put() {
	c := NewCluster([]string{"n1", "n2", "n3"}, DefaultReplication)
	if err := c.Put("greeting", []byte("hello"), ConsistencyQuorum); err != nil {
		panic(err)
	}
	data, err := c.Get("greeting", ConsistencyQuorum)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s\n", data)
	// Output: hello
}
```

Your turn: add `TestRemoveNodeReducesCluster` that calls `c.RemoveNode("node-A")`
on a fresh five-node cluster and asserts `c.NodeCount() == 4`.

### Exercise 5: Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"bytes"
	"fmt"
	"log"

	"example.com/shardkv"
)

func main() {
	nodes := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	c := shardkv.NewCluster(nodes, shardkv.DefaultReplication)

	fmt.Printf("cluster: %d nodes, replication factor %d\n", c.NodeCount(), shardkv.DefaultReplication)

	// Write with quorum consistency.
	pairs := [][2]string{
		{"user:1", "alice"},
		{"user:2", "bob"},
		{"product:42", "widget"},
	}
	for _, p := range pairs {
		if err := c.Put(p[0], []byte(p[1]), shardkv.ConsistencyQuorum); err != nil {
			log.Fatalf("Put %q: %v", p[0], err)
		}
		replicas := c.ReplicasFor(p[0])
		fmt.Printf("put %-15s -> replicas: %v\n", p[0], replicas)
	}

	// Read with quorum consistency.
	for _, p := range pairs {
		got, err := c.Get(p[0], shardkv.ConsistencyQuorum)
		if err != nil {
			log.Fatalf("Get %q: %v", p[0], err)
		}
		status := "OK"
		if !bytes.Equal(got, []byte(p[1])) {
			status = "MISMATCH"
		}
		fmt.Printf("get %-15s = %q  %s\n", p[0], got, status)
	}

	// Overwrite and verify last-write-wins.
	if err := c.Put("user:1", []byte("alice-updated"), shardkv.ConsistencyAll); err != nil {
		log.Fatalf("overwrite: %v", err)
	}
	got, _ := c.Get("user:1", shardkv.ConsistencyAll)
	fmt.Printf("overwrite user:1 = %q\n", got)

	// Delete and verify.
	if err := c.Delete("product:42"); err != nil {
		log.Fatalf("delete: %v", err)
	}
	_, err := c.Get("product:42", shardkv.ConsistencyOne)
	if err != nil {
		fmt.Printf("delete product:42: confirmed (get returned: %v)\n", err)
	}

	// Add a node and verify the cluster grows.
	c.AddNode("zeta")
	fmt.Printf("after AddNode: %d nodes\n", c.NodeCount())
}
```

Run the demo with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Using a Hash Ring Without Virtual Nodes

Wrong: place each physical node at exactly one position on the ring.

What happens: with few nodes, the positions are likely clustered. One node ends
up owning the majority of the key space, creating a hot spot. Adding a node
relieves only that one hot node.

Fix: place each node at V virtual positions (this lesson uses
`VNodesPerNode = 100`). The positions spread evenly around the ring, so each
physical node owns roughly 1/N of the key space regardless of how many nodes
there are.

### Mutating the Replica Slice Returned by ReplicasFor

Wrong:

```go
replicas := ring.ReplicasFor(key, 3)
replicas = replicas[:2]        // "trim to quorum"
```

What happens: the slice shares backing array with the ring's internal state if
the ring returns a subslice. The trim silently modifies shared memory.

Fix: `ReplicasFor` always returns a fresh `[]string` allocated in the function.
Do not pass it back to ring internals. If you need a shorter slice, reslice the
return value — but do not pass the resliced value back anywhere.

### Ignoring the Version on Get Under ConsistencyOne

Wrong: calling `Get(key, ConsistencyOne)` and trusting the result to be
up-to-date after a recent `Put`.

What happens: `ConsistencyOne` returns as soon as the first replica responds
(any of the N replicas, whichever wins the goroutine race), without waiting for
the others. If that replica has not yet received the write (the write used
`ConsistencyQuorum` and only two of three replicas acknowledged), the read
returns stale data.

Fix: use `ConsistencyQuorum` or `ConsistencyAll` for reads that must see the
latest write. The lesson's `ExampleCluster_Put` uses `ConsistencyQuorum` for
both the write and the read for exactly this reason.

### Racing on Version Without an Atomic

Wrong:

```go
c.version++          // unprotected field increment
```

What happens: two concurrent `Put` calls can read the same version value, both
increment to the same result, and both writes carry the same version. Last-write-
wins is now undefined: two writes that should have distinct versions look like
the same write.

Fix: use `sync/atomic.Uint64.Add(1)` as this lesson does. The Add is guaranteed
to return a unique value to each caller even under heavy concurrency.

## Verification

From `~/go-exercises/shardkv`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is essential: the cluster's coordinator
fans out goroutines for every read and write, and any unguarded shared state
will surface as a data race under `-race`.

Run the demo to inspect the system end-to-end:

```bash
go run ./cmd/demo
```

## Summary

- Consistent hashing places nodes and keys on a circular ring; a key maps to the
  first node clockwise. Adding or removing a node remaps only O(1/N) keys.
- Virtual nodes (vnodes) per physical node smooth out the key distribution and
  prevent hot spots.
- Replication factor N means each key is stored on N consecutive nodes. The
  replica set is determined by walking the ring clockwise from the key's position.
- Quorum logic (W + R > N) guarantees that a read always overlaps with the most
  recent successful write.
- Last-write-wins with a monotonic version counter is the simplest conflict
  resolution; it is correct when clocks are not the source of the version.
- A coordinator fans out concurrent sub-requests and waits for the minimum quorum
  before returning, keeping tail latency low.

## What's Next

Next: [Chaos Testing Framework](../14-chaos-testing-framework/14-chaos-testing-framework.md).

## Resources

- [Dynamo: Amazon's Highly Available Key-value Store](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) — the foundational paper defining consistent hashing, quorum reads/writes, and hinted handoff
- [Go sync/atomic package](https://pkg.go.dev/sync/atomic) — `Uint64.Add` for lock-free version counters
- [Go crypto/sha256](https://pkg.go.dev/crypto/sha256) — hash function used for ring placement
- [Designing Data-Intensive Applications, Chapter 6 (Partitioning)](https://dataintensive.net/) — partitioning strategies, rebalancing, and request routing
- [Cassandra Architecture: Consistent Hashing](https://cassandra.apache.org/doc/latest/cassandra/architecture/dynamo.html) — production implementation of the Dynamo ring with vnodes
