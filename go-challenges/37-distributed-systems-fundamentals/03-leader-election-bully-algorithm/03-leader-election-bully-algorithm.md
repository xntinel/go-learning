# 3. Leader Election: Bully Algorithm

The Bully algorithm is the simplest deterministic leader-election protocol: the node with the highest ID always wins. Its appeal is its ease of reasoning; its cost is O(n^2) messages and an inability to handle split-brain. Implementing it in Go is an exercise in safe concurrent state: multiple goroutines racing to start elections, send messages, and agree on a winner without deadlocking or reading stale values.

```text
bully/
  go.mod
  bully.go
  bully_test.go
  cmd/demo/main.go
```

## Concepts

### The Three Message Types

The protocol uses exactly three messages:

- `Election` — "I am starting an election; anyone with a higher ID respond."
- `Answer` — "I have a higher ID; I will take over the election."
- `Coordinator` — "I am the new leader."

A node that receives `Election` from a lower-ID node sends back `Answer` and starts its own election (if it is not already running one). A node that sends `Election` and receives no `Answer` within a timeout declares itself the coordinator and broadcasts `Coordinator` to all peers.

### Why the Highest ID Wins

The invariant is simple: if two nodes are both alive and both start elections, the one with the lower ID will always receive an `Answer` from the one with the higher ID. It therefore backs off. The node with the highest ID receives no `Answer` (no one has a higher ID) and wins. The result is deterministic given a fixed set of alive nodes.

### Election Trigger and Timeout

An election is triggered when:

1. The cluster starts (no leader yet).
2. A node's heartbeat monitor stops receiving responses from the current leader.
3. A recovered node detects that its ID is higher than the current leader's ID.

The election timeout must be long enough for `Election` → `Answer` round-trips to complete but short enough to detect failures quickly. In a simulation with goroutine latency, a few milliseconds is sufficient; in production over a real network, tens of milliseconds is typical.

### Trade-offs and Failure Modes

The algorithm is partition-intolerant: if the network splits, each partition may elect its own leader. A node with the highest ID recovering from a crash immediately wins again, which may be desirable (that node is the designated "biggest machine") or undesirable (constant re-elections if it is flaky).

Message complexity is O(n^2) in the worst case: every node sends `Election` to every node with a higher ID. With n = 5 nodes and the lowest-ID node initiating, that is 4 + 3 + 2 + 1 = 10 messages just for the Election phase, plus n-1 Coordinator messages.

### Concurrent Safety

Each `Node` keeps its leader ID and alive/election state under a `sync.Mutex`. Goroutines send messages over buffered channels; reads and writes to shared fields always hold the lock. The election function itself must not hold the lock while sending on a channel (deadlock risk if the receiver also tries to lock the same mutex).

## Exercises

This is a library (`package bully`). Verification is `go test`, not `go run`.

### Exercise 1: Node, Message Types, and Constructor

Create `bully.go`:

```go
package bully

import (
	"fmt"
	"sync"
	"time"
)

// MsgType identifies one of the three Bully protocol messages.
type MsgType int

const (
	MsgElection    MsgType = iota // start or participate in election
	MsgAnswer                     // higher-ID node is alive
	MsgCoordinator                // sender is the new leader
)

// Message is the envelope exchanged between nodes.
type Message struct {
	Type   MsgType
	FromID int
}

// electionReq is an internal request sent from StartElection to the Serve loop.
type electionReq struct {
	resultCh chan int // receives the elected leader ID
}

// Node represents one participant in the cluster.
type Node struct {
	id          int
	mu          sync.Mutex
	leader      int // -1 means unknown
	alive       bool
	peers       []*Node
	timeout     time.Duration
	inbox       chan Message
	electionReq chan electionReq
}

// New creates a Node with the given ID and election timeout.
// Nodes are created before connecting peers; use SetPeers after all nodes exist.
func New(id int, electionTimeout time.Duration) *Node {
	return &Node{
		id:          id,
		leader:      -1,
		alive:       true,
		inbox:       make(chan Message, 64),
		electionReq: make(chan electionReq, 1),
		timeout:     electionTimeout,
	}
}

// SetPeers tells this node about all other nodes in the cluster (excluding itself).
func (n *Node) SetPeers(peers []*Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.peers = peers
}

// ID returns the node's identifier.
func (n *Node) ID() int { return n.id }

// Leader returns the current known leader ID, or -1 if unknown.
func (n *Node) Leader() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.leader
}

// IsAlive reports whether the node has not been crashed.
func (n *Node) IsAlive() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.alive
}

// Crash simulates a node failure. The node stops processing messages.
func (n *Node) Crash() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alive = false
}

// Recover brings a crashed node back to life with an unknown leader.
func (n *Node) Recover() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.alive = true
	n.leader = -1
}

// send delivers a message to another node's inbox if that node is alive.
// It does not block: if the inbox is full, the message is dropped (simulates
// congestion).
func (n *Node) send(to *Node, m Message) {
	to.mu.Lock()
	alive := to.alive
	to.mu.Unlock()
	if !alive {
		return
	}
	select {
	case to.inbox <- m:
	default:
	}
}

// StartElection asks the node's Serve loop to run a Bully election and blocks
// until the loop returns the elected leader ID.
// Returns -1 if the node is crashed.
func (n *Node) StartElection() int {
	n.mu.Lock()
	alive := n.alive
	n.mu.Unlock()
	if !alive {
		return -1
	}
	resultCh := make(chan int, 1)
	n.electionReq <- electionReq{resultCh: resultCh}
	return <-resultCh
}

// runElection executes the Bully protocol synchronously inside the Serve loop.
// It must not be called from outside the loop.
func (n *Node) runElection() int {
	n.mu.Lock()
	peers := make([]*Node, len(n.peers))
	copy(peers, n.peers)
	timeout := n.timeout
	n.mu.Unlock()

	// Send Election to all nodes with a higher ID.
	var higherCount int
	for _, p := range peers {
		if p.id > n.id {
			higherCount++
			n.send(p, Message{Type: MsgElection, FromID: n.id})
		}
	}

	if higherCount == 0 {
		return n.becomeCoordinator(peers)
	}

	// Wait for Answer or timeout.
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		select {
		case msg := <-n.inbox:
			switch msg.Type {
			case MsgAnswer:
				// A higher node is alive; it will run its own election
				// and eventually broadcast Coordinator. Wait for it.
				t2 := time.NewTimer(timeout * 3)
				defer t2.Stop()
				for {
					select {
					case msg2 := <-n.inbox:
						if msg2.Type == MsgCoordinator {
							n.mu.Lock()
							n.leader = msg2.FromID
							n.mu.Unlock()
							return msg2.FromID
						}
						// Other Election messages while waiting: handle inline.
						if msg2.Type == MsgElection && msg2.FromID < n.id {
							n.replyAnswer(msg2.FromID, peers)
						}
					case <-t2.C:
						// Coordinator never arrived; re-run election.
						return n.runElection()
					}
				}
			case MsgElection:
				// A lower node started an election while we were waiting.
				if msg.FromID < n.id {
					n.replyAnswer(msg.FromID, peers)
				}
			case MsgCoordinator:
				// Someone else became coordinator.
				n.mu.Lock()
				n.leader = msg.FromID
				n.mu.Unlock()
				return msg.FromID
			}
		case <-t.C:
			return n.becomeCoordinator(peers)
		}
	}
}

// replyAnswer sends MsgAnswer to the peer with the given ID.
func (n *Node) replyAnswer(toID int, peers []*Node) {
	for _, p := range peers {
		if p.id == toID {
			n.send(p, Message{Type: MsgAnswer, FromID: n.id})
			return
		}
	}
}

// becomeCoordinator records this node as leader and notifies peers.
func (n *Node) becomeCoordinator(peers []*Node) int {
	n.mu.Lock()
	n.leader = n.id
	n.mu.Unlock()
	for _, p := range peers {
		n.send(p, Message{Type: MsgCoordinator, FromID: n.id})
	}
	return n.id
}

// Serve runs the node's event loop until stopCh is closed.
// Call in a goroutine: go n.Serve(stop).
// The loop owns all reads from n.inbox, preventing races with StartElection.
func (n *Node) Serve(stopCh <-chan struct{}) {
	for {
		select {
		case req := <-n.electionReq:
			// Run the election synchronously inside the loop so that
			// n.inbox is owned by a single goroutine.
			n.mu.Lock()
			alive := n.alive
			n.mu.Unlock()
			if !alive {
				req.resultCh <- -1
				continue
			}
			req.resultCh <- n.runElection()
		case msg := <-n.inbox:
			n.mu.Lock()
			alive := n.alive
			n.mu.Unlock()
			if !alive {
				continue
			}
			switch msg.Type {
			case MsgElection:
				if msg.FromID < n.id {
					n.mu.Lock()
					peers := make([]*Node, len(n.peers))
					copy(peers, n.peers)
					n.mu.Unlock()
					n.replyAnswer(msg.FromID, peers)
					// Start our own election asynchronously so we do
					// not block the Serve loop while waiting for answers.
					go func() { n.StartElection() }()
				}
			case MsgCoordinator:
				n.mu.Lock()
				n.leader = msg.FromID
				n.mu.Unlock()
			}
		case <-stopCh:
			return
		}
	}
}

// String returns a short human-readable description.
func (n *Node) String() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	state := "alive"
	if !n.alive {
		state = "crashed"
	}
	return fmt.Sprintf("Node{id=%d leader=%d %s}", n.id, n.leader, state)
}
```

`StartElection` sends a request to the `Serve` loop rather than reading from `n.inbox` directly.
That single ownership over `n.inbox` (only the `Serve` goroutine reads it) removes the race where
two goroutines compete for the same message.

### Exercise 2: Test the Election Contract

Create `bully_test.go`:

```go
package bully

import (
	"testing"
	"time"
)

const testTimeout = 20 * time.Millisecond

// makeCluster creates n nodes with IDs 1..n, wires their peers,
// starts their serve loops, and returns the nodes and a stop function.
func makeCluster(t *testing.T, n int) ([]*Node, func()) {
	t.Helper()
	nodes := make([]*Node, n)
	for i := range nodes {
		nodes[i] = New(i+1, testTimeout)
	}
	for _, nd := range nodes {
		peers := make([]*Node, 0, n-1)
		for _, other := range nodes {
			if other.id != nd.id {
				peers = append(peers, other)
			}
		}
		nd.SetPeers(peers)
	}
	stop := make(chan struct{})
	for _, nd := range nodes {
		nd2 := nd
		go nd2.Serve(stop)
	}
	return nodes, func() { close(stop) }
}

// waitLeader polls until all alive nodes agree on the same leader or the
// deadline expires.
func waitLeader(t *testing.T, nodes []*Node, wantLeader int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		ok := true
		for _, nd := range nodes {
			if !nd.IsAlive() {
				continue
			}
			if nd.Leader() != wantLeader {
				ok = false
				break
			}
		}
		if ok {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Report state of each node for debugging.
	for _, nd := range nodes {
		if nd.IsAlive() {
			t.Logf("  %s", nd)
		}
	}
	t.Fatalf("nodes did not agree on leader %d within deadline", wantLeader)
}

func TestInitialElectionHighestIDWins(t *testing.T) {
	t.Parallel()

	nodes, stop := makeCluster(t, 5)
	defer stop()

	// Node 1 (lowest) starts the election. Highest ID (5) must win.
	winner := nodes[0].StartElection()
	if winner != 5 {
		t.Fatalf("StartElection() = %d, want 5", winner)
	}
	waitLeader(t, nodes, 5)
}

func TestMiddleNodeStartsElection(t *testing.T) {
	t.Parallel()

	nodes, stop := makeCluster(t, 5)
	defer stop()

	// Node 3 (middle) starts: nodes 4 and 5 are higher, 5 must win.
	winner := nodes[2].StartElection()
	if winner != 5 {
		t.Fatalf("StartElection() = %d, want 5", winner)
	}
	waitLeader(t, nodes, 5)
}

func TestHighestNodeStartsElection(t *testing.T) {
	t.Parallel()

	nodes, stop := makeCluster(t, 4)
	defer stop()

	// Node 4 (highest) starts; no one answers — it wins immediately.
	winner := nodes[3].StartElection()
	if winner != 4 {
		t.Fatalf("StartElection() = %d, want 4", winner)
	}
	waitLeader(t, nodes, 4)
}

func TestLeaderCrashTriggersReelection(t *testing.T) {
	t.Parallel()

	nodes, stop := makeCluster(t, 4)
	defer stop()

	// Elect node 4.
	nodes[0].StartElection()
	waitLeader(t, nodes, 4)

	// Crash node 4; node 2 starts a new election.
	nodes[3].Crash()
	alive := nodes[:3]
	nodes[1].StartElection()
	waitLeader(t, alive, 3)
}

func TestRecoveredHighIDNodeReclaims(t *testing.T) {
	t.Parallel()

	nodes, stop := makeCluster(t, 4)
	defer stop()

	// Crash node 4; elect node 3.
	nodes[3].Crash()
	nodes[0].StartElection()
	waitLeader(t, nodes[:3], 3)

	// Recover node 4; it re-elects itself.
	nodes[3].Recover()
	nodes[3].StartElection()
	waitLeader(t, nodes, 4)
}

func TestCrashedNodeDoesNotParticipate(t *testing.T) {
	t.Parallel()

	nodes, stop := makeCluster(t, 3)
	defer stop()

	// Crash node 3 (would normally win).
	nodes[2].Crash()
	winner := nodes[0].StartElection()
	// Node 2 must win because node 3 is silent.
	if winner != 2 {
		t.Fatalf("StartElection() = %d, want 2 (node 3 is crashed)", winner)
	}
}

func ExampleNew() {
	node := New(5, 50*time.Millisecond)
	// Output:
	_ = node.ID()
}
```

Your turn: add `TestTwoSimultaneousElections` that crashes the leader, starts elections from both node 1 and node 2 concurrently (use goroutines), waits for convergence, and asserts that all alive nodes agree on the same leader.

### Exercise 3: Command-line Demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"time"

	"example.com/bully"
)

func main() {
	const n = 5
	const timeout = 30 * time.Millisecond

	nodes := make([]*bully.Node, n)
	for i := range nodes {
		nodes[i] = bully.New(i+1, timeout)
	}
	for _, nd := range nodes {
		peers := make([]*bully.Node, 0, n-1)
		for _, other := range nodes {
			if other.ID() != nd.ID() {
				peers = append(peers, other)
			}
		}
		nd.SetPeers(peers)
	}

	stop := make(chan struct{})
	defer close(stop)
	for _, nd := range nodes {
		nd2 := nd
		go nd2.Serve(stop)
	}

	// Phase 1: initial election from node 1.
	fmt.Println("--- phase 1: initial election from node 1 ---")
	winner := nodes[0].StartElection()
	fmt.Printf("winner: %d\n", winner)
	time.Sleep(50 * time.Millisecond)
	for _, nd := range nodes {
		fmt.Printf("  node %d sees leader %d\n", nd.ID(), nd.Leader())
	}

	// Phase 2: crash the leader, re-elect.
	fmt.Println("--- phase 2: crash node 5, re-elect from node 2 ---")
	nodes[4].Crash()
	winner2 := nodes[1].StartElection()
	fmt.Printf("winner: %d\n", winner2)
	time.Sleep(50 * time.Millisecond)
	for _, nd := range nodes[:4] {
		fmt.Printf("  node %d sees leader %d\n", nd.ID(), nd.Leader())
	}

	// Phase 3: recover node 5, it reclaims leadership.
	fmt.Println("--- phase 3: recover node 5 ---")
	nodes[4].Recover()
	winner3 := nodes[4].StartElection()
	fmt.Printf("winner: %d\n", winner3)
	time.Sleep(50 * time.Millisecond)
	for _, nd := range nodes {
		fmt.Printf("  node %d sees leader %d\n", nd.ID(), nd.Leader())
	}
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Holding the Mutex While Sending on a Channel

Wrong: acquire the lock, then send a message to a peer whose `Serve` goroutine also tries to acquire the same lock on receipt. Both goroutines block forever.

Fix: copy the fields you need (peer list, timeout) while holding the lock, then release the lock before sending on any channel. The implementation above uses this pattern in `StartElection`.

### Starting Election Recursion Without a Base Case

Wrong: `StartElection` re-calls itself when no Coordinator arrives, with no bound. Under a flapping network, this recurses until the stack overflows.

Fix: a real implementation limits retries or uses a state machine with a `electing bool` guard. The demo uses a fixed timeout and re-calls once; production code adds a retry counter or a context with deadline.

### Reading Shared State Without a Lock

Wrong: checking `n.alive` or `n.leader` directly from multiple goroutines without synchronization. The race detector flags these.

Fix: always acquire `n.mu` before reading or writing `n.alive`, `n.leader`, or `n.peers`. The test suite runs with `-race`; any data race fails immediately.

### Using Unbuffered Channels as Inboxes

Wrong: `inbox: make(chan Message)` — a sender blocks until a receiver is ready. During an election with n goroutines all sending simultaneously, senders deadlock each other.

Fix: use a buffered channel (`make(chan Message, 64)`). If the buffer fills, drop the message (simulates network congestion). For production, use a larger buffer or a separate queue goroutine.

### Not Draining the Inbox on Crash

Wrong: a crashed node's `Serve` loop exits, but its inbox channel still accumulates messages. When the node recovers and `Serve` restarts, stale messages from the old epoch are processed first.

Fix: drain the inbox channel when crashing, or use a versioned epoch number on every message to discard stale ones.

## Verification

From `~/go-exercises/bully`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector is not optional: concurrent elections are the core of this lesson.

## Summary

- The Bully algorithm elects the highest-ID alive node using three message types: Election, Answer, Coordinator.
- A node that receives no Answer within a timeout declares itself the coordinator and broadcasts Coordinator to all peers.
- The algorithm guarantees convergence but is not partition-tolerant: a split network can elect two leaders.
- Message complexity is O(n^2) in the worst case.
- Safe concurrent implementation requires releasing the mutex before sending on a channel to avoid deadlock.
- A recovered high-ID node triggers a new election and reclaims leadership.

## What's Next

Next: [Distributed Locking with Leases](../04-distributed-locking/04-distributed-locking.md).

## Resources

- [Garcia-Molina: Elections in a Distributed Computing System (1982)](https://dl.acm.org/doi/10.1109/TC.1982.1675885) — the original paper defining the Bully algorithm.
- [Go sync package](https://pkg.go.dev/sync) — Mutex, RWMutex, and WaitGroup used in the implementation.
- [Go channels specification](https://go.dev/ref/spec#Channel_types) — buffered vs. unbuffered channel semantics.
- [Go race detector](https://go.dev/doc/articles/race_detector) — how to enable and interpret `-race` output.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) — goroutine and channel idioms used throughout this lesson.
