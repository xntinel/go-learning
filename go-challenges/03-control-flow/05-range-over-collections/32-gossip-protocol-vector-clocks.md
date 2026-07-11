# Exercise 32: Gossip Protocol with Vector Clock Causal Ordering

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A peer-to-peer service that gossips state changes — a distributed cache
invalidation, a cluster membership update, a config change propagating
through a mesh — cannot rely on messages arriving in the order they were
sent, because gossip fans a message out through many independent paths with
independent latencies. A node that applies whatever arrives last, wall-clock
style, will sometimes overwrite a genuinely newer update with a genuinely
older one that simply took a slower path through the mesh. Vector clocks fix
this: each node tags its writes with a per-origin counter, and a receiver
compares that counter against what it already knows about the origin to
decide whether an incoming message is causally newer, a duplicate, or stale
— never by when it happened to arrive. This module ranges peer lists to
broadcast updates concurrently, encodes a full vector-clock snapshot into
every message, merges incoming clocks under lock, and demonstrates recovery
from exactly the kind of delivery reordering a real gossip fanout produces.
The module is fully self-contained: its own `go mod init`, no external
dependencies.

## What you'll build

```text
gossip/                     independent module: example.com/gossip-protocol-vector-clocks
  go.mod                    go 1.24
  gossip.go                 type Node; Update, Receive, Broadcast, ClockSnapshot
  cmd/
    demo/
      main.go               runnable demo: reordered delivery of two causally related updates
  gossip_test.go             table test: clock increment + reorder/duplicate rejection + transitive merge; concurrent Broadcast/Receive under -race
```

- Files: `gossip.go`, `cmd/demo/main.go`, `gossip_test.go`.
- Implement: `Node.Update`, `Node.Receive`, `Node.Broadcast`, and
  `Node.ClockSnapshot`, all synchronized under one `sync.Mutex` per node.
- Test: a clock-increment case, a table covering reordered and duplicate
  delivery, a transitive-merge case, a concurrent `Broadcast` case, and a
  concurrent `Receive` case under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gossip-protocol-vector-clocks/cmd/demo
cd ~/go-exercises/gossip-protocol-vector-clocks
go mod init example.com/gossip-protocol-vector-clocks
go mod edit -go=1.24
```

### One comparison, one range: rejecting staleness and merging knowledge

`Receive`'s entire causal-ordering guarantee rests on a single comparison —
`msg.Clock[msg.From] <= n.clock[msg.From]` — checked and acted on inside one
critical section. That one line does double duty: it rejects an exact
duplicate (equal counts, already applied) and it rejects a message that a
slower gossip path delivered *after* a causally newer update from the same
origin already arrived (a lower count than what this node has already
recorded). Both cases return `false` without touching state, which is
exactly the recovery from reordering the exercise is built around — a stale
message from origin A can arrive at any point after a newer message from A
was already applied, and it will always lose, regardless of how late it
shows up. Once a message clears that check, `Receive` ranges the *entire*
incoming `msg.Clock` — not just the sender's own entry — taking the
element-wise maximum against `n.clock`. That range is what makes vector
clocks propagate transitively through a mesh: a message from B that itself
merged knowledge from A carries A's counter along with it, so a node that
has never spoken to A directly still learns A's progress the moment it
hears from B.

The reason `Update` clones the clock into the message instead of handing
over `n.clock` directly is aliasing, not style. `Broadcast` ranges this
node's peers and starts one goroutine per peer to deliver the message
concurrently — a real gossip fanout does not wait for one peer before
telling the next — so by the time those goroutines run, this node may
already have taken its lock again for another `Update` and be mutating its
live clock map. If `Message.Clock` were that same live map instead of a
`clone()`, every peer goroutine ranging it inside `Receive` would be reading
a map another goroutine is concurrently writing: a data race `-race` will
catch instantly, and exactly the kind of bug that only shows up under real
concurrent load, not in a single-threaded demo run.

Create `gossip.go`:

```go
package gossip

import "sync"

// VectorClock maps a node ID to the number of updates that node has made
// (or, in a merged clock, the highest count this node has learned about for
// that origin, however it learned it).
type VectorClock map[string]uint64

// clone returns an independent copy so a message's clock snapshot can never
// alias — and therefore never race with — the live clock on the node that
// sent it.
func (vc VectorClock) clone() VectorClock {
	cp := make(VectorClock, len(vc))
	for id, count := range vc {
		cp[id] = count
	}
	return cp
}

// Message is one gossiped state change, carrying a full snapshot of the
// sender's vector clock at the moment of the write, not just the sender's
// own counter — that snapshot is what lets a receiving node learn
// transitively about updates from nodes it never talked to directly.
type Message struct {
	From  string
	Key   string
	Value string
	Clock VectorClock
}

// Node is one peer in a gossip mesh: a small key-value store plus the
// vector clock that orders its writes causally relative to every other node
// it has heard from, directly or transitively.
type Node struct {
	id    string
	mu    sync.Mutex
	clock VectorClock
	state map[string]string
	peers []*Node
}

// NewNode builds an empty Node identified by id.
func NewNode(id string) *Node {
	return &Node{
		id:    id,
		clock: make(VectorClock),
		state: make(map[string]string),
	}
}

// Connect registers peer as a gossip target for this node's Broadcast.
func (n *Node) Connect(peer *Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.peers = append(n.peers, peer)
}

// Update performs a local write, incrementing this node's own clock entry.
// The returned Message's Clock is a cloned snapshot, never the live map, so
// a later Update or Receive on this node cannot race with a peer ranging
// this message's clock after Broadcast hands it off to other goroutines.
func (n *Node) Update(key, value string) Message {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.clock[n.id]++
	n.state[key] = value

	return Message{
		From:  n.id,
		Key:   key,
		Value: value,
		Clock: n.clock.clone(),
	}
}

// Receive applies msg only if it is causally newer than what this node has
// already recorded for msg.From: msg.Clock[msg.From] must exceed this
// node's own count for that origin. That single comparison both
// deduplicates a redelivered message (equal counts) and discards a message
// that a gossip fanout delivered out of causal order (a lower count arriving
// after a higher one already applied), so every node converges on the same
// final state regardless of the order messages actually arrive in. The
// check and the state/clock mutation happen inside the same critical
// section, so two concurrent Receive calls for the same origin can never
// both observe the old count and both apply.
func (n *Node) Receive(msg Message) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	if msg.Clock[msg.From] <= n.clock[msg.From] {
		return false
	}

	n.state[msg.Key] = msg.Value
	for id, count := range msg.Clock {
		if count > n.clock[id] {
			n.clock[id] = count
		}
	}
	return true
}

// Broadcast ranges this node's peers and delivers msg to each of them
// concurrently, modeling a gossip fanout where the network gives no
// guarantee about delivery order across peers.
func (n *Node) Broadcast(msg Message) {
	n.mu.Lock()
	peers := make([]*Node, len(n.peers))
	copy(peers, n.peers)
	n.mu.Unlock()

	var wg sync.WaitGroup
	for _, peer := range peers {
		wg.Add(1)
		go func(peer *Node) {
			defer wg.Done()
			peer.Receive(msg)
		}(peer)
	}
	wg.Wait()
}

// Get returns the current value stored under key.
func (n *Node) Get(key string) (string, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.state[key]
	return v, ok
}

// ClockSnapshot returns a cloned copy of this node's current vector clock.
func (n *Node) ClockSnapshot() VectorClock {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.clock.clone()
}
```

### The runnable demo

The demo has node `A` make two updates to the same key, then delivers them
to node `C` in reverse order — the newer update first, the stale one
second — proving `C` ends up with the causally correct value either way.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/gossip-protocol-vector-clocks"
)

func main() {
	a := gossip.NewNode("A")
	c := gossip.NewNode("C")

	msg1 := a.Update("region", "us-east")
	msg2 := a.Update("region", "us-west")

	// Simulate a gossip network delivering the newer update before the
	// older one — a real fanout gives no ordering guarantee across peers.
	appliedNewer := c.Receive(msg2)
	appliedStale := c.Receive(msg1)

	val, _ := c.Get("region")
	fmt.Printf("applied_newer=%v applied_stale=%v region=%s\n", appliedNewer, appliedStale, val)
	fmt.Printf("A clock=%v\n", a.ClockSnapshot())
	fmt.Printf("C clock=%v\n", c.ClockSnapshot())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
applied_newer=true applied_stale=false region=us-west
A clock=map[A:2]
C clock=map[A:2]
```

### Tests

The reorder/duplicate table drives both failure modes `Receive` must reject
through the same code path, the transitive-merge test proves a node learns
about an origin it never spoke to directly, `TestBroadcastDeliversToAllPeersConcurrently`
proves the concurrent fanout reaches every peer, and
`TestConcurrentReceiveIsRaceFree` fires 50 updates from the same origin at
one node concurrently and must pass under `-race` with the highest count
winning regardless of goroutine scheduling.

Create `gossip_test.go`:

```go
package gossip

import (
	"sync"
	"testing"
)

func TestUpdateIncrementsOwnClock(t *testing.T) {
	t.Parallel()

	a := NewNode("A")
	msg1 := a.Update("k", "v1")
	msg2 := a.Update("k", "v2")

	if msg1.Clock["A"] != 1 {
		t.Fatalf("msg1.Clock[A] = %d, want 1", msg1.Clock["A"])
	}
	if msg2.Clock["A"] != 2 {
		t.Fatalf("msg2.Clock[A] = %d, want 2", msg2.Clock["A"])
	}
}

func TestReceiveRejectsStaleAndDuplicateMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		deliver func(c *Node, msg1, msg2 Message) (appliedFirst, appliedSecond bool)
	}{
		{
			name: "reordered delivery: newer then older",
			deliver: func(c *Node, msg1, msg2 Message) (bool, bool) {
				appliedNewer := c.Receive(msg2)
				appliedStale := c.Receive(msg1)
				return appliedNewer, appliedStale
			},
		},
		{
			name: "duplicate redelivery of the same message",
			deliver: func(c *Node, msg1, msg2 Message) (bool, bool) {
				first := c.Receive(msg1)
				duplicate := c.Receive(msg1)
				return first, duplicate
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := NewNode("A")
			msg1 := a.Update("region", "us-east")
			msg2 := a.Update("region", "us-west")

			c := NewNode("C")
			firstOK, secondOK := tc.deliver(c, msg1, msg2)

			if !firstOK {
				t.Fatalf("first delivery applied = false, want true")
			}
			if secondOK {
				t.Fatalf("second delivery applied = true, want false (stale or duplicate)")
			}
		})
	}
}

func TestReceiveMergesClockTransitively(t *testing.T) {
	t.Parallel()

	a := NewNode("A")
	b := NewNode("B")
	d := NewNode("D")

	msgFromA := a.Update("x", "1")
	// B learns about A's update.
	b.Receive(msgFromA)

	// B makes its own local update; its clock snapshot now carries
	// knowledge of both B's and A's counters.
	msgFromB := b.Update("y", "2")

	// D has never talked to A directly, only to B.
	d.Receive(msgFromB)

	clock := d.ClockSnapshot()
	if clock["B"] != 1 {
		t.Fatalf("D's clock[B] = %d, want 1", clock["B"])
	}
	if clock["A"] != 1 {
		t.Fatalf("D's clock[A] = %d, want 1 (learned transitively through B)", clock["A"])
	}
}

func TestBroadcastDeliversToAllPeersConcurrently(t *testing.T) {
	t.Parallel()

	a := NewNode("A")
	peers := []*Node{NewNode("B"), NewNode("C"), NewNode("D")}
	for _, p := range peers {
		a.Connect(p)
	}

	msg := a.Update("k", "v")
	a.Broadcast(msg)

	for _, p := range peers {
		v, ok := p.Get("k")
		if !ok || v != "v" {
			t.Fatalf("peer %v Get(k) = (%q, %v), want (\"v\", true)", p, v, ok)
		}
	}
}

func TestConcurrentReceiveIsRaceFree(t *testing.T) {
	t.Parallel()

	c := NewNode("C")
	a := NewNode("A")

	const updates = 50
	msgs := make([]Message, updates)
	for i := 0; i < updates; i++ {
		msgs[i] = a.Update("k", "v")
	}

	var wg sync.WaitGroup
	for _, msg := range msgs {
		wg.Add(1)
		go func(msg Message) {
			defer wg.Done()
			c.Receive(msg)
		}(msg)
	}
	wg.Wait()

	clock := c.ClockSnapshot()
	if clock["A"] != updates {
		t.Fatalf("C's clock[A] = %d, want %d (highest count wins regardless of arrival order)", clock["A"], updates)
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The mesh is correct when every node converges on the same final state for a
key no matter what order its gossip messages actually arrive in, and when a
message's clock snapshot is cloned so no peer goroutine can ever observe it
mutating mid-range. The bug this design specifically avoids is handing a
`Message` the live `n.clock` map instead of a clone: `Broadcast` fires off
one goroutine per peer and returns control to the caller, who is free to
call `Update` again immediately — if the just-sent message aliased the live
clock, that next `Update`'s `n.clock[n.id]++` would be a concurrent write
racing every peer goroutine's `range msg.Clock` inside `Receive`, a bug that
would not show up in a quick manual test but that `-race` catches on the
very first run of `TestConcurrentReceiveIsRaceFree`.

## Resources

- [Fidge/Mattern, the original vector clock papers (1988)](https://en.wikipedia.org/wiki/Vector_clock) — the causal-ordering mechanism this exercise's `VectorClock` implements.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [The Go Memory Model](https://go.dev/ref/mem) — why an aliased map shared across goroutines without synchronization is undefined behavior.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-watermark-stream-window-aggregation.md](31-watermark-stream-window-aggregation.md) | Next: [33-sliding-window-rate-limiter-log.md](33-sliding-window-rate-limiter-log.md)
