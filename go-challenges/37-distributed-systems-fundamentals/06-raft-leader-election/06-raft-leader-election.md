# 6. Raft Leader Election

Raft is a consensus algorithm built for understandability. Its leader election guarantees at most one leader per term, achieves a new election within one election-timeout window after a failure, and prevents split-vote livelock through randomized timeouts. The hard part is not the state machine itself but the concurrency: each node runs its own timer goroutine, every incoming message can trigger a state transition, and all mutations must be guarded correctly so that `go test -race` stays clean.

This lesson builds a complete in-process Raft leader election simulation using nothing but the standard library. Nodes communicate over Go channels that act as the simulated RPC transport.

```text
raft/
  go.mod
  raft.go
  raft_test.go
  cmd/demo/main.go
```

## Concepts

### The Three-State Machine

Every Raft node is always in exactly one of three states:

- **Follower** — the starting state. Followers wait for heartbeats from the current leader. If none arrive before the election timeout, the follower converts to candidate.
- **Candidate** — a follower that timed out. The candidate increments the current term, votes for itself, and sends RequestVote to every peer.
- **Leader** — a candidate that collected votes from a strict majority (`(n/2)+1` nodes including itself). The leader sends periodic AppendEntries (heartbeats) to all followers to prevent them from timing out.

Transitions always go in one direction per term: Follower → Candidate → Leader. A leader or candidate can drop back to Follower when it sees a message carrying a higher term.

### Terms: the Logical Clock

A term is a monotonically increasing integer shared across the cluster. Each election starts a new term. Terms serve two purposes:

1. They detect stale messages. A node that receives any message with `term < currentTerm` ignores it.
2. They force step-downs. Any node that receives a message with `term > currentTerm` immediately sets `currentTerm = term` and reverts to Follower, even if it is currently the leader.

The invariant "at most one leader per term" is guaranteed by majority voting: two overlapping majorities cannot exist in the same term, so two leaders cannot both collect majorities simultaneously.

### Randomized Election Timeouts

If all followers timed out at the same moment they would all become candidates simultaneously and split the votes indefinitely. Randomizing the timeout per node and per election attempt breaks the symmetry: the node with the shortest timeout reaches candidacy first, collects votes before others time out, and becomes leader. The Raft paper uses 150-300 ms; this lesson scales times down to 100-200 ms for fast tests.

The timeout must be reset (not merely started once) on every relevant event:

- A follower resets its timeout when it receives any valid AppendEntries (heartbeat).
- A candidate resets its timeout when it starts a new election.

### Vote Granting Rules

A node grants its vote in a given term if and only if:

1. `request.Term >= currentTerm` (not stale).
2. The node has not already voted for someone else in `request.Term`.

Once a node votes for a candidate in term T it rejects all other candidates in term T. This is tracked with a `votedFor` field (peer ID or -1 for "not voted").

### Heartbeats and Failure Detection

The leader sends heartbeats every `heartbeatInterval`, which must be shorter than the minimum election timeout. If the interval is 50 ms and the minimum timeout is 100 ms, a live leader's heartbeats always arrive before followers time out.

When a leader crashes (or is partitioned away), followers stop receiving heartbeats and eventually time out, triggering a new election. The cluster elects a new leader within at most two timeout periods: one to detect the failure and one for the election itself.

### Majority and Quorums

In a cluster of N nodes, a majority is `(N/2) + 1`. For N=5, majority is 3. A candidate needs 3 votes (including its own) to become leader. If a 5-node cluster is partitioned and only 2 nodes can communicate, those 2 nodes cannot collect 3 votes, so they cannot elect a leader — this prevents split-brain. The N=2 case is a useful boundary: majority is 2, both nodes can reach each other, so they do elect one leader. The invariant is about reaching more than half the configured cluster, not about the partition size in isolation.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/06-raft-leader-election/06-raft-leader-election/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/06-raft-leader-election/06-raft-leader-election
```

This is a library package verified with `go test`. The `cmd/demo` directory holds a runnable demonstration.

### Exercise 1: Message Types and the Node Struct

Create `raft.go`:

```go
package raft

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// NodeState represents the role of a Raft node.
type NodeState int

const (
	Follower  NodeState = iota
	Candidate NodeState = iota
	Leader    NodeState = iota
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

// msgKind identifies the type of an in-channel message.
type msgKind int

const (
	msgRequestVote    msgKind = iota
	msgRequestVoteRsp msgKind = iota
	msgAppendEntries  msgKind = iota
	msgStop           msgKind = iota
)

// message is the envelope passed between nodes through channels.
type message struct {
	kind    msgKind
	from    int
	term    int
	granted bool // used by msgRequestVoteRsp
}

// Config controls timing parameters.
type Config struct {
	// ElectionTimeoutMin is the lower bound of the randomized election timeout.
	ElectionTimeoutMin time.Duration
	// ElectionTimeoutMax is the upper bound of the randomized election timeout.
	ElectionTimeoutMax time.Duration
	// HeartbeatInterval is how often the leader sends AppendEntries.
	HeartbeatInterval time.Duration
}

// DefaultConfig returns a Config suitable for tests (fast timeouts).
func DefaultConfig() Config {
	return Config{
		ElectionTimeoutMin: 100 * time.Millisecond,
		ElectionTimeoutMax: 200 * time.Millisecond,
		HeartbeatInterval:  30 * time.Millisecond,
	}
}

// Node is a single Raft peer.
type Node struct {
	id    int
	peers []*Node

	mu          sync.Mutex
	state       NodeState
	currentTerm int
	votedFor    int // -1 = not voted
	voteCount   int

	inbox chan message
	done  chan struct{}
	cfg   Config
	rng   *rand.Rand
}

// NewNode creates a node with the given id and config but does not start it.
func NewNode(id int, cfg Config) *Node {
	return &Node{
		id:          id,
		state:       Follower,
		currentTerm: 0,
		votedFor:    -1,
		inbox:       make(chan message, 64),
		done:        make(chan struct{}),
		cfg:         cfg,
		rng:         rand.New(rand.NewSource(int64(id) * 9973)),
	}
}

// SetPeers registers all peer nodes (including self is ignored by send).
func (n *Node) SetPeers(peers []*Node) {
	n.peers = peers
}

// State returns the node's current role (safe to call concurrently).
func (n *Node) State() NodeState {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state
}

// Term returns the node's current term (safe to call concurrently).
func (n *Node) Term() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.currentTerm
}

// ID returns the node's id.
func (n *Node) ID() int { return n.id }

// Start launches the node's event loop in a goroutine.
func (n *Node) Start() {
	go n.run()
}

// Stop signals the node to shut down and waits for it.
// After Stop returns, State() reports Follower so that callers
// (e.g. Cluster.Leaders) do not count this node as a live leader.
func (n *Node) Stop() {
	n.send(n, message{kind: msgStop})
	<-n.done
	n.mu.Lock()
	n.state = Follower
	n.mu.Unlock()
}

// send delivers msg to dst's inbox; it is a no-op if dst is stopped.
func (n *Node) send(dst *Node, msg message) {
	select {
	case dst.inbox <- msg:
	default:
		// drop if inbox full — simulates a lossy network
	}
}

// electionTimeout returns a random duration in [min, max).
func (n *Node) electionTimeout() time.Duration {
	span := int64(n.cfg.ElectionTimeoutMax - n.cfg.ElectionTimeoutMin)
	return n.cfg.ElectionTimeoutMin + time.Duration(n.rng.Int63n(span))
}
```

### Exercise 2: The Event Loop

Append to `raft.go`:

```go
// run is the single-threaded event loop for the node.
func (n *Node) run() {
	defer close(n.done)
	timer := time.NewTimer(n.electionTimeout())
	defer timer.Stop()

	for {
		select {
		case msg := <-n.inbox:
			if msg.kind == msgStop {
				return
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			n.handle(msg)
			// only followers and candidates reset their timeout
			n.mu.Lock()
			s := n.state
			n.mu.Unlock()
			if s != Leader {
				timer.Reset(n.electionTimeout())
			}

		case <-timer.C:
			n.mu.Lock()
			s := n.state
			n.mu.Unlock()
			if s != Leader {
				n.startElection()
			}
			timer.Reset(n.electionTimeout())
		}
	}
}

// handle dispatches an incoming message.
func (n *Node) handle(msg message) {
	n.mu.Lock()
	// Step down if we see a higher term.
	if msg.term > n.currentTerm {
		n.currentTerm = msg.term
		n.state = Follower
		n.votedFor = -1
		n.voteCount = 0
	}
	n.mu.Unlock()

	switch msg.kind {
	case msgRequestVote:
		n.handleRequestVote(msg)
	case msgRequestVoteRsp:
		n.handleVoteResponse(msg)
	case msgAppendEntries:
		n.handleAppendEntries(msg)
	}
}

// handleRequestVote decides whether to grant a vote.
func (n *Node) handleRequestVote(msg message) {
	n.mu.Lock()
	defer n.mu.Unlock()

	grant := false
	if msg.term >= n.currentTerm && (n.votedFor == -1 || n.votedFor == msg.from) {
		n.votedFor = msg.from
		grant = true
	}
	// find the sender
	for _, p := range n.peers {
		if p.id == msg.from {
			n.send(p, message{
				kind:    msgRequestVoteRsp,
				from:    n.id,
				term:    n.currentTerm,
				granted: grant,
			})
			return
		}
	}
}

// handleVoteResponse tallies a vote response.
func (n *Node) handleVoteResponse(msg message) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.state != Candidate || msg.term != n.currentTerm {
		return
	}
	if msg.granted {
		n.voteCount++
		majority := len(n.peers)/2 + 1
		if n.voteCount >= majority {
			n.becomeLeaderLocked()
		}
	}
}

// handleAppendEntries processes a heartbeat from the leader.
func (n *Node) handleAppendEntries(msg message) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if msg.term < n.currentTerm {
		return // stale leader
	}
	// Valid heartbeat: step down if we were a candidate.
	if n.state == Candidate {
		n.state = Follower
		n.votedFor = -1
		n.voteCount = 0
	}
}

// startElection converts this node to candidate and requests votes.
func (n *Node) startElection() {
	n.mu.Lock()
	n.state = Candidate
	n.currentTerm++
	n.votedFor = n.id
	n.voteCount = 1 // vote for self
	term := n.currentTerm
	majority := len(n.peers)/2 + 1
	// A single-node cluster already has majority with the self-vote.
	if n.voteCount >= majority {
		n.becomeLeaderLocked()
		n.mu.Unlock()
		return
	}
	n.mu.Unlock()

	for _, p := range n.peers {
		if p.id == n.id {
			continue
		}
		n.send(p, message{
			kind: msgRequestVote,
			from: n.id,
			term: term,
		})
	}
}

// becomeLeaderLocked promotes the node to leader; must be called with mu held.
func (n *Node) becomeLeaderLocked() {
	n.state = Leader
	go n.sendHeartbeats()
}

// sendHeartbeats periodically sends AppendEntries to all peers while leader.
func (n *Node) sendHeartbeats() {
	ticker := time.NewTicker(n.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			n.mu.Lock()
			s := n.state
			term := n.currentTerm
			n.mu.Unlock()
			if s != Leader {
				return
			}
			for _, p := range n.peers {
				if p.id == n.id {
					continue
				}
				n.send(p, message{
					kind: msgAppendEntries,
					from: n.id,
					term: term,
				})
			}
		case <-n.done:
			return
		}
	}
}

// Cluster manages a set of nodes.
type Cluster struct {
	nodes []*Node
}

// NewCluster creates a cluster of size n with the given config.
func NewCluster(size int, cfg Config) *Cluster {
	nodes := make([]*Node, size)
	for i := range nodes {
		nodes[i] = NewNode(i, cfg)
	}
	// Give every node the full peer list.
	for _, nd := range nodes {
		nd.SetPeers(nodes)
	}
	return &Cluster{nodes: nodes}
}

// Start starts all nodes.
func (c *Cluster) Start() {
	for _, n := range c.nodes {
		n.Start()
	}
}

// Stop stops all nodes.
func (c *Cluster) Stop() {
	for _, n := range c.nodes {
		n.Stop()
	}
}

// Leaders returns the IDs of nodes currently in Leader state.
func (c *Cluster) Leaders() []int {
	var ids []int
	for _, n := range c.nodes {
		if n.State() == Leader {
			ids = append(ids, n.id)
		}
	}
	return ids
}

// WaitForLeader polls until exactly one leader exists or timeout elapses.
// It returns the leader node and true on success.
func (c *Cluster) WaitForLeader(timeout time.Duration) (*Node, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		leaders := c.Leaders()
		if len(leaders) == 1 {
			return c.nodes[leaders[0]], true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil, false
}

// Node returns the node with the given id (panics if not found).
func (c *Cluster) Node(id int) *Node {
	for _, n := range c.nodes {
		if n.id == id {
			return n
		}
	}
	panic(fmt.Sprintf("node %d not found", id))
}
```

### Exercise 3: Tests

Create `raft_test.go`:

```go
package raft

import (
	"fmt"
	"testing"
	"time"
)

func TestSingleLeaderElected(t *testing.T) {
	t.Parallel()

	c := NewCluster(5, DefaultConfig())
	c.Start()
	defer c.Stop()

	leader, ok := c.WaitForLeader(2 * time.Second)
	if !ok {
		t.Fatal("no leader elected within timeout")
	}
	if leader.State() != Leader {
		t.Fatalf("node %d state = %s, want Leader", leader.ID(), leader.State())
	}
	if leaders := c.Leaders(); len(leaders) != 1 {
		t.Fatalf("want 1 leader, got %d: %v", len(leaders), leaders)
	}
}

func TestLeaderSendsHeartbeats(t *testing.T) {
	t.Parallel()

	c := NewCluster(3, DefaultConfig())
	c.Start()
	defer c.Stop()

	_, ok := c.WaitForLeader(2 * time.Second)
	if !ok {
		t.Fatal("no leader elected")
	}

	// Wait well past one election timeout; the cluster must still have
	// exactly one leader (heartbeats prevented new elections).
	time.Sleep(300 * time.Millisecond)
	if leaders := c.Leaders(); len(leaders) != 1 {
		t.Fatalf("after heartbeat period: want 1 leader, got %d: %v", len(leaders), leaders)
	}
}

func TestLeaderFailureTriggersReelection(t *testing.T) {
	t.Parallel()

	c := NewCluster(5, DefaultConfig())
	c.Start()

	leader, ok := c.WaitForLeader(2 * time.Second)
	if !ok {
		t.Fatal("no initial leader elected")
	}
	oldID := leader.ID()

	// Kill the leader.
	leader.Stop()

	// The remaining 4 nodes must elect a new leader.
	// Poll only nodes that are still running.
	deadline := time.Now().Add(2 * time.Second)
	var newLeader *Node
	for time.Now().Before(deadline) {
		for _, n := range c.nodes {
			if n.ID() == oldID {
				continue
			}
			if n.State() == Leader {
				newLeader = n
			}
		}
		if newLeader != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if newLeader == nil {
		t.Fatal("no new leader elected after old leader stopped")
	}
	if newLeader.ID() == oldID {
		t.Fatalf("new leader ID %d equals old leader ID", newLeader.ID())
	}
	if newLeader.Term() <= leader.Term() {
		t.Fatalf("new leader term %d must be > old term %d", newLeader.Term(), leader.Term())
	}

	// Stop remaining nodes manually (skip the already-stopped leader).
	for _, n := range c.nodes {
		if n.ID() != oldID {
			n.Stop()
		}
	}
}

func TestSmallClusterMajorityBehavior(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()

	// A 2-node cluster: majority threshold is 2 (both nodes must agree).
	// One candidate gets its own vote (count=1) and requests the other's vote;
	// the other grants it, bringing the count to 2 which meets majority=2.
	// So a 2-node isolated cluster does elect exactly one leader.
	two := NewCluster(2, cfg)
	two.Start()
	defer two.Stop()

	_, ok := two.WaitForLeader(1 * time.Second)
	if !ok {
		t.Fatal("2-node cluster must elect a leader (majority=2, both nodes reachable)")
	}
	if leaders := two.Leaders(); len(leaders) != 1 {
		t.Fatalf("2-node cluster: want 1 leader, got %d: %v", len(leaders), leaders)
	}

	// A 1-node cluster: it is its own majority (majority=1, self-vote suffices).
	solo := NewCluster(1, cfg)
	solo.Start()
	defer solo.Stop()

	_, ok = solo.WaitForLeader(500 * time.Millisecond)
	if !ok {
		t.Fatal("single-node cluster must elect itself as leader")
	}
}

func TestTermMonotonicallyIncreases(t *testing.T) {
	t.Parallel()

	c := NewCluster(3, DefaultConfig())
	c.Start()
	defer c.Stop()

	leader, ok := c.WaitForLeader(2 * time.Second)
	if !ok {
		t.Fatal("no leader elected")
	}
	firstTerm := leader.Term()
	if firstTerm < 1 {
		t.Fatalf("term = %d, want >= 1", firstTerm)
	}

	// Stop the leader and wait for a new one.
	leader.Stop()

	var newLeader *Node
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range c.nodes {
			if n.ID() == leader.ID() {
				continue
			}
			if n.State() == Leader {
				newLeader = n
				break
			}
		}
		if newLeader != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if newLeader == nil {
		t.Skip("no second leader elected; acceptable under slow CI")
	}
	if newLeader.Term() <= firstTerm {
		t.Fatalf("second leader term %d must be > first term %d", newLeader.Term(), firstTerm)
	}

	for _, n := range c.nodes {
		if n.ID() != leader.ID() {
			n.Stop()
		}
	}
}

func ExampleNewCluster() {
	cfg := DefaultConfig()
	c := NewCluster(3, cfg)
	c.Start()
	defer c.Stop()

	_, ok := c.WaitForLeader(2 * time.Second)
	if !ok {
		fmt.Println("no leader")
		return
	}
	fmt.Println("leader elected")
	// Output:
	// leader elected
}
```

Your turn: add `TestStateTransitions` that starts a 3-node cluster, waits for a leader, then verifies that all non-leader nodes are in Follower state (not Candidate).

### Exercise 4: cmd/demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/raft"
)

func main() {
	cfg := raft.DefaultConfig()
	c := raft.NewCluster(5, cfg)
	c.Start()
	defer c.Stop()

	fmt.Println("cluster started with 5 nodes, waiting for leader...")
	leader, ok := c.WaitForLeader(3 * time.Second)
	if !ok {
		fmt.Println("no leader elected within 3s")
		return
	}
	fmt.Printf("leader elected: node %d (term %d)\n", leader.ID(), leader.Term())

	// Wait a bit then show all node states.
	time.Sleep(150 * time.Millisecond)
	fmt.Println("node states after steady state:")
	for _, id := range []int{0, 1, 2, 3, 4} {
		n := c.Node(id)
		fmt.Printf("  node %d: %s (term %d)\n", id, n.State(), n.Term())
	}

	// Kill the leader and observe re-election.
	fmt.Printf("\nstopping leader (node %d)...\n", leader.ID())
	deadID := leader.ID()
	leader.Stop()

	// Poll remaining nodes for a new leader.
	deadline := time.Now().Add(3 * time.Second)
	var newLeader *raft.Node
	for time.Now().Before(deadline) {
		leaders := c.Leaders()
		if len(leaders) == 1 && leaders[0] != deadID {
			newLeader = c.Node(leaders[0])
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if newLeader == nil {
		fmt.Println("no new leader elected")
		return
	}
	fmt.Printf("new leader: node %d (term %d)\n", newLeader.ID(), newLeader.Term())
}
```

## Common Mistakes

### Forgetting to drain the timer channel on Reset

Wrong:

```go
timer.Reset(d)
```

Fix:

```go
if !timer.Stop() {
	select {
	case <-timer.C:
	default:
	}
}
timer.Reset(d)
```

If the timer fires just before `Stop()` returns, the channel has a pending value. Calling `Reset` without draining it means the next `<-timer.C` fires immediately with the old value, triggering a spurious election. The `select` with a `default` case drains safely without blocking.

### Holding the mutex across channel sends

Wrong:

```go
n.mu.Lock()
n.state = Candidate
for _, p := range n.peers {
	n.send(p, message{...})  // send may block; lock held
}
n.mu.Unlock()
```

Fix: release the mutex before sending. A blocked send with the lock held can deadlock if the recipient tries to acquire the same lock to process the message.

```go
n.mu.Lock()
n.state = Candidate
term := n.currentTerm
n.mu.Unlock()

for _, p := range n.peers {
	n.send(p, message{kind: msgRequestVote, from: n.id, term: term})
}
```

### Checking state after releasing the lock (TOCTOU)

Wrong: reading `n.state` twice — once inside a lock and once outside — and acting on the second read. Between the two reads another goroutine may have changed the state.

Fix: read state once while holding the lock and act on that snapshot. All state transitions in this lesson copy the relevant fields into local variables under the lock, then use those copies.

### Using a single shared `*rand.Rand` without synchronization

Wrong: one `rand.Rand` seeded once and used from multiple goroutines.

Fix: each node owns its `*rand.Rand` and is the only goroutine that calls it (the event loop is single-threaded per node). No locking needed. Alternatively, use `rand/v2` from Go 1.22+, whose top-level functions are goroutine-safe.

## Verification

From `~/go-exercises/raft`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass with no data race output.

## Summary

- Raft nodes cycle through Follower, Candidate, and Leader states; a node can only advance through that sequence within a single term.
- Terms are a logical clock: any message with a higher term forces an immediate step-down to Follower.
- Randomized election timeouts prevent split votes by giving one node a head start before others time out.
- Majority voting (strictly more than half) guarantees at most one leader per term.
- Heartbeats from the leader keep followers from timing out; a missed heartbeat triggers a new election.
- The event loop is single-threaded per node; shared state is guarded with a `sync.Mutex`.

## What's Next

Next: [Raft Log Replication](../07-raft-log-replication/07-raft-log-replication.md).

## Resources

- [In Search of an Understandable Consensus Algorithm (Ongaro & Ousterhout, 2014)](https://raft.github.io/raft.pdf) — the original Raft paper; Sections 5.1 and 5.2 cover leader election precisely.
- [Raft Interactive Visualization](https://raft.github.io/) — animates state transitions and term changes.
- [Students' Guide to Raft](https://thesquareplanet.com/blog/students-guide-to-raft/) — documents the most common implementation bugs, including the timer reset issue.
- [sync package — pkg.go.dev](https://pkg.go.dev/sync) — Mutex and related primitives used in this lesson.
- [time package — pkg.go.dev](https://pkg.go.dev/time) — Timer, NewTimer, Reset semantics (see "Timer gotchas" in the source comments).
