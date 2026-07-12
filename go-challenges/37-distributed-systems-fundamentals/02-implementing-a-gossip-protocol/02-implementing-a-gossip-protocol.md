# 2. Implementing a Gossip Protocol

Gossip protocols spread information through a cluster the way rumors spread through a crowd: each node periodically picks a random peer and exchanges state with it. No leader coordinates the exchange; no central registry holds the current view. The interesting difficulty is in the details: which state to send, how to resolve conflicts when two nodes update the same key, and how to bound the traffic while still guaranteeing convergence. This lesson builds a push-pull gossip engine from scratch and measures those properties.

```text
gossip/
  go.mod
  gossip.go
  gossip_test.go
  cmd/demo/main.go
```

## Concepts

### Epidemic Spread and Convergence

In push gossip each node sends its current state to a random peer. In pull gossip each node requests state from a random peer. Push-pull combines both directions in one round: node A sends a digest (key + version pairs) to node B; node B replies with every entry that A is missing or has at a lower version; then A sends the entries that B is missing. One push-pull round does roughly twice the work of a push round but reduces the number of rounds needed for full convergence, so total bandwidth is lower for large clusters.

Convergence time is O(log N) rounds for a cluster of N nodes because each round infects at least one new node, giving exponential growth of the informed set — the same analysis as epidemic modeling. Fanout (the number of peers contacted per round) multiplies the base of the exponent; a fanout of 3 converges in roughly log(N) / log(3) rounds.

### Version Vectors and Last-Writer-Wins

When two nodes update the same key concurrently, the protocol needs a rule to pick one value. Last-writer-wins with a Lamport timestamp (a monotonically increasing integer) is the simplest rule that is correct: the entry with the higher version number wins. The timestamp is not a wall clock because wall clocks on different machines are not synchronized to the precision needed; instead each node increments a local counter every time it writes a key.

A version of 0 means "I have never seen this key". A version of N > 0 means "this is the N-th write to this key (from my perspective)". During a push-pull exchange, node A sends `{key: version}` pairs; node B compares each version against its own copy and sends back the full entry only when its version is strictly greater.

### Failure Tolerance

Because gossip is decentralized, a node failure affects only the rounds in which that node would have been selected as a peer. With N nodes and fanout k, the probability that a failed node blocks propagation drops geometrically as the cluster grows. The cluster continues to converge as long as a connected subgraph of live nodes exists.

This lesson simulates failure by stopping a node's gossip goroutine. The remaining nodes still converge because each of them continues to contact random peers among the live set.

### The Push-Pull Exchange Protocol

```
Node A                          Node B
  |                               |
  |-- digest {key:ver, ...} ----> |
  |                               | compares each (key,ver) with local state
  | <-- missing/stale entries --- |
  |                               |
  | -- entries B is missing ----> |
  |                               |
```

The digest is cheap to send (keys and version numbers only, no values). The reply carries full entries only for the delta. Two passes complete the exchange in a single round-trip.

## Exercises

This is a library, not a program: there is no top-level `main`. Verify with `go test`.

### Exercise 1: Core Types

Create `gossip.go`:

```go
// gossip.go
package gossip

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// Entry is one versioned key-value record. Version 0 means "absent".
// Higher version wins on conflict (last-writer-wins).
type Entry struct {
	Value   string
	Version int64
}

// Node holds a key-value store and participates in gossip rounds.
// All exported methods are safe for concurrent use.
type Node struct {
	id      string
	mu      sync.RWMutex
	state   map[string]Entry
	clock   int64 // local Lamport counter; incremented on every local write
	stopped atomic.Bool
}

// NewNode creates a node with the given identifier.
func NewNode(id string) *Node {
	return &Node{
		id:    id,
		state: make(map[string]Entry),
	}
}

// ID returns the node identifier.
func (n *Node) ID() string { return n.id }

// Set writes key=value locally with a Lamport timestamp one higher than any
// version the node has already seen for that key.
func (n *Node) Set(key, value string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	ver := n.clock + 1
	if cur, ok := n.state[key]; ok && cur.Version >= ver {
		ver = cur.Version + 1
	}
	n.clock = ver
	n.state[key] = Entry{Value: value, Version: ver}
}

// Get returns the current value and version for key.
// Version is 0 and ok is false if the key is absent.
func (n *Node) Get(key string) (value string, version int64, ok bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	e, found := n.state[key]
	if !found {
		return "", 0, false
	}
	return e.Value, e.Version, true
}

// digest returns a map of key -> version (no values) for the push-pull exchange.
func (n *Node) digest() map[string]int64 {
	n.mu.RLock()
	defer n.mu.RUnlock()
	d := make(map[string]int64, len(n.state))
	for k, e := range n.state {
		d[k] = e.Version
	}
	return d
}

// merge applies incoming entries, keeping the higher-version entry for each key.
// It advances the local Lamport clock whenever a remote version is higher.
func (n *Node) merge(incoming map[string]Entry) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, remote := range incoming {
		local, ok := n.state[k]
		if !ok || remote.Version > local.Version {
			n.state[k] = remote
			if remote.Version > n.clock {
				n.clock = remote.Version
			}
		}
	}
}

// entriesNewerThan returns entries whose version is strictly greater than the
// version recorded in the digest d. Keys absent from d are included.
func (n *Node) entriesNewerThan(d map[string]int64) map[string]Entry {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make(map[string]Entry)
	for k, e := range n.state {
		if ver, seen := d[k]; !seen || e.Version > ver {
			out[k] = e
		}
	}
	return out
}

// exchangeWith performs one push-pull gossip round between n and peer.
// Both nodes end the round with the union of their states (higher version wins).
func (n *Node) exchangeWith(peer *Node) {
	// Phase 1: n sends its digest to peer.
	myDigest := n.digest()
	peerDigest := peer.digest()

	// Phase 2: each node computes what the other is missing or has stale.
	toSendToPeer := n.entriesNewerThan(peerDigest)
	toSendToN := peer.entriesNewerThan(myDigest)

	// Phase 3: apply deltas.
	n.merge(toSendToN)
	peer.merge(toSendToPeer)
}

// Stop prevents the node from being selected for future gossip rounds.
func (n *Node) Stop() { n.stopped.Store(true) }

// Alive reports whether the node is still participating.
func (n *Node) Alive() bool { return !n.stopped.Load() }

// Cluster manages a set of nodes and drives periodic gossip rounds.
type Cluster struct {
	mu    sync.RWMutex
	nodes map[string]*Node
}

// NewCluster creates an empty cluster.
func NewCluster() *Cluster {
	return &Cluster{nodes: make(map[string]*Node)}
}

// Add registers a node with the cluster.
func (c *Cluster) Add(n *Node) {
	c.mu.Lock()
	c.nodes[n.id] = n
	c.mu.Unlock()
}

// SetValue writes key=value on the node identified by nodeID.
func (c *Cluster) SetValue(nodeID, key, value string) error {
	c.mu.RLock()
	n, ok := c.nodes[nodeID]
	c.mu.RUnlock()
	if !ok {
		return fmt.Errorf("gossip: node %q not found", nodeID)
	}
	n.Set(key, value)
	return nil
}

// Converged returns true when every live node has the same value for key
// and that value equals want.
func (c *Cluster) Converged(key, want string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, n := range c.nodes {
		if !n.Alive() {
			continue
		}
		v, _, ok := n.Get(key)
		if !ok || v != want {
			return false
		}
	}
	return true
}

// liveNodes returns a snapshot of all live nodes.
func (c *Cluster) liveNodes() []*Node {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*Node, 0, len(c.nodes))
	for _, n := range c.nodes {
		if n.Alive() {
			out = append(out, n)
		}
	}
	return out
}

// RunRound executes one gossip round: each live node contacts fanout random
// peers and performs a push-pull exchange with each.
func (c *Cluster) RunRound(fanout int) {
	live := c.liveNodes()
	if len(live) < 2 {
		return
	}
	for _, node := range live {
		peers := randomPeers(live, node, fanout)
		for _, peer := range peers {
			node.exchangeWith(peer)
		}
	}
}

// MeasureConvergence sets key=value on the node with id seedID, then runs
// gossip rounds until all live nodes agree. It returns the number of rounds
// taken, or -1 if convergence was not reached within maxRounds.
func (c *Cluster) MeasureConvergence(seedID, key, value string, fanout, maxRounds int) int {
	if err := c.SetValue(seedID, key, value); err != nil {
		return -1
	}
	for round := 1; round <= maxRounds; round++ {
		if c.Converged(key, value) {
			return round
		}
		c.RunRound(fanout)
	}
	if c.Converged(key, value) {
		return maxRounds
	}
	return -1
}

// randomPeers selects up to n distinct live nodes from pool, excluding self.
func randomPeers(pool []*Node, self *Node, n int) []*Node {
	candidates := make([]*Node, 0, len(pool)-1)
	for _, p := range pool {
		if p != self {
			candidates = append(candidates, p)
		}
	}
	rand.Shuffle(len(candidates), func(i, j int) {
		candidates[i], candidates[j] = candidates[j], candidates[i]
	})
	if n > len(candidates) {
		n = len(candidates)
	}
	return candidates[:n]
}

// WaitConverged runs rounds at interval until key==want on all live nodes or
// the deadline is reached. It returns true on convergence.
func (c *Cluster) WaitConverged(key, want string, fanout int, interval, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Converged(key, want) {
			return true
		}
		c.RunRound(fanout)
		time.Sleep(interval)
	}
	return c.Converged(key, want)
}
```

The key design decisions:
- `digest()` sends only keys and versions; `entriesNewerThan()` computes the delta.
- `merge()` applies last-writer-wins and advances the local Lamport clock.
- `exchangeWith()` is symmetric: both nodes end the round with the union of their states.

### Exercise 2: Tests

Create `gossip_test.go`:

```go
// gossip_test.go
package gossip

import (
	"fmt"
	"testing"
)

// ExampleCluster_Converged shows the minimum usage: seed one node, run rounds,
// check convergence. The output is deterministic because the cluster has exactly
// two nodes and one round suffices.
func ExampleCluster_Converged() {
	c := NewCluster()
	a := NewNode("a")
	b := NewNode("b")
	c.Add(a)
	c.Add(b)

	a.Set("color", "blue")
	c.RunRound(1)

	v, _, _ := b.Get("color")
	fmt.Println(v)
	// Output: blue
}

func TestSetAndGet(t *testing.T) {
	t.Parallel()

	n := NewNode("n1")
	n.Set("k", "v")
	got, ver, ok := n.Get("k")
	if !ok {
		t.Fatal("key should be present")
	}
	if got != "v" {
		t.Fatalf("value = %q, want %q", got, "v")
	}
	if ver < 1 {
		t.Fatalf("version = %d, want >= 1", ver)
	}
}

func TestGetAbsentKey(t *testing.T) {
	t.Parallel()

	n := NewNode("n1")
	_, ver, ok := n.Get("missing")
	if ok {
		t.Fatal("absent key should return ok=false")
	}
	if ver != 0 {
		t.Fatalf("absent key version = %d, want 0", ver)
	}
}

func TestExchangeWithPropagatesState(t *testing.T) {
	t.Parallel()

	a := NewNode("a")
	b := NewNode("b")
	a.Set("x", "1")

	a.exchangeWith(b)

	v, _, ok := b.Get("x")
	if !ok || v != "1" {
		t.Fatalf("b.Get(x) = %q, %v; want 1, true", v, ok)
	}
}

func TestExchangeWithResolveConflictHigherVersionWins(t *testing.T) {
	t.Parallel()

	a := NewNode("a")
	b := NewNode("b")

	// a writes version 1; b writes version 2 (will win).
	a.Set("x", "from-a")
	b.Set("x", "from-b-v1")
	b.Set("x", "from-b-v2") // version 2 on b

	a.exchangeWith(b)

	va, _, _ := a.Get("x")
	vb, _, _ := b.Get("x")
	if va != "from-b-v2" {
		t.Fatalf("a.Get(x) = %q, want from-b-v2", va)
	}
	if vb != "from-b-v2" {
		t.Fatalf("b.Get(x) = %q, want from-b-v2", vb)
	}
}

func TestClusterConvergenceTwoNodes(t *testing.T) {
	t.Parallel()

	c := NewCluster()
	a := NewNode("a")
	b := NewNode("b")
	c.Add(a)
	c.Add(b)

	a.Set("env", "prod")
	c.RunRound(1)

	if !c.Converged("env", "prod") {
		t.Fatal("cluster should converge after one round with two nodes")
	}
}

func TestClusterConvergenceLogN(t *testing.T) {
	t.Parallel()

	const size = 20
	const fanout = 3
	const maxRounds = 10 // O(log_3(20)) ≈ 3, give ample margin

	c := NewCluster()
	nodes := make([]*Node, size)
	for i := range nodes {
		nodes[i] = NewNode(fmt.Sprintf("n%d", i))
		c.Add(nodes[i])
	}

	rounds := c.MeasureConvergence("n0", "region", "us-east-1", fanout, maxRounds)
	if rounds < 0 {
		t.Fatalf("cluster did not converge in %d rounds", maxRounds)
	}
	t.Logf("20-node cluster converged in %d round(s) with fanout %d", rounds, fanout)
}

func TestClusterConvergesWithFailedNode(t *testing.T) {
	t.Parallel()

	const size = 8
	c := NewCluster()
	nodes := make([]*Node, size)
	for i := range nodes {
		nodes[i] = NewNode(fmt.Sprintf("n%d", i))
		c.Add(nodes[i])
	}

	// Stop one node before gossiping.
	nodes[3].Stop()

	rounds := c.MeasureConvergence("n0", "status", "ok", 2, 20)
	if rounds < 0 {
		t.Fatal("cluster should converge even with a failed node")
	}
}

func TestMultipleKeysConverge(t *testing.T) {
	t.Parallel()

	const size = 10
	c := NewCluster()
	for i := 0; i < size; i++ {
		c.Add(NewNode(fmt.Sprintf("n%d", i)))
	}

	// Write three keys on different nodes concurrently (sequential here for
	// determinism; the merge logic is the same either way).
	_ = c.SetValue("n0", "color", "blue")
	_ = c.SetValue("n1", "shape", "circle")
	_ = c.SetValue("n2", "size", "large")

	for round := 0; round < 15; round++ {
		if c.Converged("color", "blue") &&
			c.Converged("shape", "circle") &&
			c.Converged("size", "large") {
			return
		}
		c.RunRound(3)
	}
	if !c.Converged("color", "blue") {
		t.Error("color did not converge")
	}
	if !c.Converged("shape", "circle") {
		t.Error("shape did not converge")
	}
	if !c.Converged("size", "large") {
		t.Error("size did not converge")
	}
}

func TestSetValueUnknownNode(t *testing.T) {
	t.Parallel()

	c := NewCluster()
	err := c.SetValue("ghost", "k", "v")
	if err == nil {
		t.Fatal("SetValue on unknown node should return an error")
	}
}

func TestHigherVersionOverwritesLower(t *testing.T) {
	t.Parallel()

	n := NewNode("n")
	n.Set("k", "v1")
	_, v1, _ := n.Get("k")
	n.Set("k", "v2")
	_, v2, _ := n.Get("k")
	if v2 <= v1 {
		t.Fatalf("second Set must produce a higher version: v1=%d v2=%d", v1, v2)
	}
	got, _, _ := n.Get("k")
	if got != "v2" {
		t.Fatalf("value = %q, want v2", got)
	}
}

func TestStopPreventsParticipation(t *testing.T) {
	t.Parallel()

	c := NewCluster()
	a := NewNode("a")
	b := NewNode("b")
	c.Add(a)
	c.Add(b)

	b.Stop()
	a.Set("flag", "yes")

	// Run many rounds; b is stopped so it should not receive updates.
	for i := 0; i < 10; i++ {
		c.RunRound(2)
	}

	_, _, ok := b.Get("flag")
	if ok {
		t.Fatal("stopped node should not receive gossip updates")
	}
}

// Your turn: add TestMeasureConvergenceReturnsNegativeOneOnTimeout that creates
// a 2-node cluster, sets a key on "n0", and then calls MeasureConvergence with
// maxRounds=0. Assert that the return value is -1.
```

The `ExampleCluster_Converged` function in the test file uses the `// Output:` comment and is automatically verified by `go test`. The test file is in `package gossip` (same package), so it can reach unexported methods like `exchangeWith`.

### Exercise 3: Command-Line Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"time"

	"example.com/gossip"
)

func main() {
	const (
		clusterSize = 16
		fanout      = 3
		maxRounds   = 20
	)

	c := gossip.NewCluster()
	for i := 0; i < clusterSize; i++ {
		c.Add(gossip.NewNode(fmt.Sprintf("n%d", i)))
	}

	// Seed one value on n0 and measure how many rounds until all nodes agree.
	rounds := c.MeasureConvergence("n0", "service", "auth", fanout, maxRounds)
	fmt.Printf("cluster=%d fanout=%d  convergence=%d rounds\n", clusterSize, fanout, rounds)

	// Now simulate a node failure: stop n7 and see if the rest still converge.
	c2 := gossip.NewCluster()
	for i := 0; i < clusterSize; i++ {
		n := gossip.NewNode(fmt.Sprintf("m%d", i))
		if i == 7 {
			n.Stop()
		}
		c2.Add(n)
	}
	rounds2 := c2.MeasureConvergence("m0", "region", "us-east-1", fanout, maxRounds)
	fmt.Printf("cluster=%d fanout=%d failed=1  convergence=%d rounds\n", clusterSize, fanout, rounds2)

	// Show WaitConverged with a short timeout.
	c3 := gossip.NewCluster()
	for i := 0; i < 4; i++ {
		c3.Add(gossip.NewNode(fmt.Sprintf("p%d", i)))
	}
	_ = c3.SetValue("p0", "mode", "dark")
	ok := c3.WaitConverged("mode", "dark", 2, time.Millisecond, 100*time.Millisecond)
	fmt.Printf("4-node WaitConverged: %v\n", ok)
}
```

## Common Mistakes

### Sending Full State Instead of a Digest

Wrong: in a push round, serializing the entire `state` map and sending it to the peer. For a node holding thousands of keys, this wastes bandwidth proportional to total state, not to the delta.

What happens: gossip traffic grows O(N * |state|) instead of O(N * |delta|), making it unusable for large clusters.

Fix: send a digest (`map[string]int64`) first; let the peer compute what it needs; send only the missing entries. The `digest()` and `entriesNewerThan()` methods in this lesson do exactly that.

### Using Wall-Clock Time as a Version

Wrong: `Version: time.Now().UnixNano()`. Two machines' clocks can differ by milliseconds or more, so a slower clock on a writer can produce a lower version number than an older value from a faster-clocked machine, causing the newer value to be overwritten.

What happens: writes are silently lost when nodes disagree on wall-clock time.

Fix: use a Lamport timestamp (a local counter incremented on every write). This lesson uses `n.clock`, which advances monotonically regardless of wall time.

### Race Conditions on the State Map

Wrong: reading `n.state` in one goroutine while another goroutine is writing to it without a lock. The Go race detector catches this immediately.

What happens: map concurrent read/write is a runtime panic in Go ("concurrent map read and map write").

Fix: hold `n.mu.RLock()` for reads and `n.mu.Lock()` for writes. The lesson's `Node` uses `sync.RWMutex` consistently. Run `go test -race` to confirm there are no data races.

### Stopping a Node but Still Selecting It as a Peer

Wrong: marking a node as failed but keeping it in the peer selection pool. The node will never respond, and every round that selects it wastes a fanout slot.

What happens: effective fanout drops below the configured value, slowing convergence.

Fix: filter stopped nodes before sampling peers. `liveNodes()` in this lesson returns only nodes where `Alive()` is true.

## Verification

From `~/go-exercises/gossip`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must complete without errors. `go test -race` is the primary verification; the race detector catches the most common correctness errors in concurrent Go code.

## Summary

- Gossip (epidemic) protocols disseminate state by having each node exchange with random peers; no central coordinator is required.
- Push-pull exchanges send a cheap digest first and transmit only the delta, reducing bandwidth while maximizing convergence speed.
- Convergence takes O(log N) rounds for a cluster of N nodes; fanout multiplies the base of the exponent.
- Lamport timestamps provide a conflict-resolution rule (higher version wins) that is correct without synchronized wall clocks.
- Node failures are tolerated automatically: the cluster converges as long as a connected subgraph of live nodes exists.
- The Go race detector (`go test -race`) is mandatory for verifying concurrent gossip code.

## What's Next

Next: [Leader Election: Bully Algorithm](../03-leader-election-bully-algorithm/03-leader-election-bully-algorithm.md).

## Resources

- [Epidemic Algorithms for Replicated Database Maintenance (Demers et al., 1987)](https://dl.acm.org/doi/10.1145/41840.41841) — the founding paper; defines push, pull, and push-pull modes and proves O(log N) convergence.
- [SWIM: Scalable Weakly-consistent Infection-style Membership Protocol (Das et al., 2002)](https://www.cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf) — extends gossip with failure detection; basis for Consul and Serf.
- [sync package — pkg.go.dev](https://pkg.go.dev/sync) — authoritative reference for `sync.RWMutex` and `sync/atomic`.
- [The Go Memory Model](https://go.dev/ref/mem) — defines the happens-before rules that make the locking strategy in this lesson correct.
- [Hashicorp Memberlist](https://github.com/hashicorp/memberlist) — production Go implementation of SWIM gossip; good reference for the delta-encoding and failure-detection patterns.
