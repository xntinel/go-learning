# Exercise 33: Gossip protocol for eventual consistency across replicas

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A gossip (epidemic) protocol spreads information across a cluster of
replicas without any central coordinator: each replica periodically
exchanges its known state with a peer, and information that started on one
node eventually reaches every node through repeated pairwise merges — the
same mechanism that keeps Cassandra's ring metadata, Consul's membership
list, and Amazon's original Dynamo paper's replica state all eventually
consistent. A replica that keeps gossiping forever wastes bandwidth and CPU
once it has genuinely learned everything its neighbors know; recognizing
that quiescence and stopping is as important as the propagation itself.
This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
gossip/                     independent module: example.com/gossip
  go.mod                     go 1.24
  gossip.go                    Update, Replica; NewCluster, Set, Snapshot, Gossip
  cmd/
    demo/
      main.go                runnable demo: 5 replicas, each with distinct info, gossiping to full agreement
  gossip_test.go               table-style tests: a peerless replica, a bidirectional merge, two replicas converging, and a full 5-replica cluster reaching eventual consistency concurrently
```

- Files: `gossip.go`, `cmd/demo/main.go`, `gossip_test.go`.
- Implement: `Replica.Gossip(maxRounds int) (rounds int, converged bool)`, round-robin gossiping with each peer and stopping once a full pass through every peer produces no change; `merge(a, b *Replica) bool`, a deadlock-safe bidirectional state exchange.
- Test: a replica with no peers, a merge copying newer versions in both directions, two replicas converging when run concurrently, and five replicas — each originating distinct information, one of them a conflicting newer version — reaching identical final state after gossiping concurrently.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why break needs the label here but continue does not

`Gossip` rotates through a replica's peers round-robin — a deterministic
stand-in for random peer selection, so a single replica's own schedule is
reproducible even though the cluster's overall convergence timing depends
on how goroutines interleave. Each round's outcome is classified in a
`switch`, and that placement reproduces the exact asymmetry from the MVCC
exercise: `break` is aware of an enclosing `switch` and, left bare, leaves
only the `switch`; `continue` is not, and reaches the enclosing `for`
directly even from inside a `switch` case.

The "changed" branch uses a plain, unlabeled `continue` — it already
advances the loop correctly, no label needed. But the "quiesced" branch —
a full pass through every peer with nothing new to report — has actually
found the stopping condition, and a bare `break` there would only leave the
`switch`: the `for` would carry on gossiping for the full `maxRounds`
regardless, burning rounds a replica that has already learned everything
its neighbors know no longer needs. `break gossip`, naming the `for`, is
what actually stops the round loop the instant quiescence is detected.

`merge` is the part that has to be safe under real concurrency: many
replicas gossip in their own goroutines at once, and any two of them might
try to exchange state with each other from both directions simultaneously.
Always locking the lower-ID replica first, regardless of which side
initiated the call, is what makes that safe — two goroutines racing to
merge the same pair can never acquire the two mutexes in opposite order,
which is exactly what a deadlock requires.

Create `gossip.go`:

```go
package gossip

import "sync"

// Update is one key's value paired with a version used to resolve
// conflicting merges: the higher version always wins, exactly like a
// last-writer-wins register in a real eventually-consistent store.
type Update struct {
	Value   string
	Version int64
}

// Replica is one node in a gossiping cluster. Its own mutex guards its
// state; merging two replicas always locks the lower-ID one first, so
// concurrent, overlapping merges across many replicas can never deadlock.
type Replica struct {
	ID    int
	Peers []*Replica

	mu    sync.Mutex
	state map[string]Update
}

// NewReplica returns an empty replica with the given ID. Peers must be
// wired up (directly or via NewCluster) before gossiping.
func NewReplica(id int) *Replica {
	return &Replica{ID: id, state: make(map[string]Update)}
}

// NewCluster builds n fully-connected replicas: every replica's Peers lists
// every other replica in the cluster.
func NewCluster(n int) []*Replica {
	replicas := make([]*Replica, n)
	for i := range replicas {
		replicas[i] = NewReplica(i)
	}
	for _, r := range replicas {
		for _, other := range replicas {
			if other.ID != r.ID {
				r.Peers = append(r.Peers, other)
			}
		}
	}
	return replicas
}

// Set applies a local write directly to this replica -- the origin of new
// information a gossip round eventually spreads to every other replica.
func (r *Replica) Set(key, value string, version int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.state[key]; !ok || version > cur.Version {
		r.state[key] = Update{Value: value, Version: version}
	}
}

// Snapshot returns a copy of this replica's current state, for inspection
// or comparison in tests.
func (r *Replica) Snapshot() map[string]Update {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Update, len(r.state))
	for k, v := range r.state {
		out[k] = v
	}
	return out
}

// merge exchanges state between a and b: every key where one side's
// version is strictly newer is copied to the other side. It always locks
// the lower-ID replica first, regardless of which side initiated the
// exchange, so two replicas gossiping with each other from both directions
// at the same time can never deadlock. It reports whether EITHER side's
// state changed as a result.
func merge(a, b *Replica) bool {
	first, second := a, b
	if second.ID < first.ID {
		first, second = second, first
	}
	first.mu.Lock()
	defer first.mu.Unlock()
	second.mu.Lock()
	defer second.mu.Unlock()

	changed := false
	for k, v := range first.state {
		if cur, ok := second.state[k]; !ok || v.Version > cur.Version {
			second.state[k] = v
			changed = true
		}
	}
	for k, v := range second.state {
		if cur, ok := first.state[k]; !ok || v.Version > cur.Version {
			first.state[k] = v
			changed = true
		}
	}
	return changed
}

// Gossip runs replica's own gossip loop against maxRounds as a hard cap:
// each round it exchanges state with the next peer in round-robin order (a
// deterministic stand-in for random peer selection, so a single replica's
// own propagation schedule is reproducible even though the CLUSTER's
// overall convergence timing depends on how goroutines interleave). A
// round that produces a change resets the quiet streak; a full pass
// through every peer with NO change at all means this replica has learned
// everything its neighbors currently know, and continuing to gossip is
// pure overhead. The labeled break is what actually stops the round loop
// the instant that quiescence is detected.
func (r *Replica) Gossip(maxRounds int) (rounds int, converged bool) {
	if len(r.Peers) == 0 {
		return 0, true
	}

	quiet := 0
gossip:
	for round := range maxRounds {
		peer := r.Peers[round%len(r.Peers)]
		changed := merge(r, peer)

		switch {
		case changed:
			// New information arrived: the quiet streak resets. A bare
			// continue here is fine -- continue is not switch-aware, so it
			// already reaches the enclosing for directly.
			quiet = 0
			continue
		case quiet+1 >= len(r.Peers):
			// A full pass through every peer with nothing new: this
			// replica has quiesced. A bare break here would only leave the
			// switch (break IS switch-aware, unlike continue), and the
			// loop would carry on gossiping for maxRounds regardless,
			// burning rounds a converged replica no longer needs.
			rounds = round + 1
			converged = true
			break gossip
		default:
			quiet++
		}
	}
	return rounds, converged
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"

	"example.com/gossip"
)

func main() {
	replicas := gossip.NewCluster(5)

	// Each replica originates a different piece of information; nothing
	// else knows about any of it yet.
	replicas[0].Set("region", "us-east", 1)
	replicas[1].Set("tier", "gold", 1)
	replicas[2].Set("owner", "team-payments", 1)
	replicas[3].Set("region", "us-west", 2) // a newer write to a key replica 0 already set
	replicas[4].Set("status", "active", 1)

	var wg sync.WaitGroup
	convergedCount := 0
	var mu sync.Mutex
	for _, r := range replicas {
		wg.Add(1)
		go func(r *gossip.Replica) {
			defer wg.Done()
			_, converged := r.Gossip(50)
			if converged {
				mu.Lock()
				convergedCount++
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()

	fmt.Println("replicas that converged:", convergedCount, "/", len(replicas))

	allMatch := true
	first := replicas[0].Snapshot()
	for _, r := range replicas[1:] {
		if !snapshotsEqual(first, r.Snapshot()) {
			allMatch = false
		}
	}
	fmt.Println("every replica agrees:", allMatch)

	keys := make([]string, 0, len(first))
	for k := range first {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %s = %q (v%d)\n", k, first[k].Value, first[k].Version)
	}
}

func snapshotsEqual(a, b map[string]gossip.Update) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
replicas that converged: 5 / 5
every replica agrees: true
  owner = "team-payments" (v1)
  region = "us-west" (v2)
  status = "active" (v1)
  tier = "gold" (v1)
```

Five pieces of information, each originating on a different replica —
including a conflicting, newer version of `"region"` that replica 3 wrote
after replica 0 — end up identical everywhere. `"region"` settles on
`"us-west"` (version 2), never `"us-east"` (version 1), because `merge`
always keeps the higher version. The exact number of rounds each replica
takes is not printed because it depends on goroutine scheduling; whether
every replica reaches full agreement does not.

### Tests

`TestGossipSingleReplicaHasNoPeers` and `TestMergeCopiesNewerVersionsBothWays`
cover the boundaries and the merge primitive directly.
`TestGossipConvergesTwoReplicas` runs the smallest real concurrent case.
`TestGossipClusterReachesEventualConsistency` is the core case: five
replicas, five distinct origins of information (one a version conflict),
gossiping concurrently, asserting every replica's final snapshot is
identical and correct — an assertion about the *outcome*, never about
timing or interleaving, so it never flakes.

Create `gossip_test.go`:

```go
package gossip

import (
	"sync"
	"testing"
)

func TestGossipSingleReplicaHasNoPeers(t *testing.T) {
	t.Parallel()

	r := NewReplica(0)
	rounds, converged := r.Gossip(10)
	if !converged || rounds != 0 {
		t.Fatalf("Gossip() = (%d, %v), want (0, true) for a peerless replica", rounds, converged)
	}
}

func TestMergeCopiesNewerVersionsBothWays(t *testing.T) {
	t.Parallel()

	a := NewReplica(0)
	b := NewReplica(1)
	a.Set("x", "from-a", 1)
	b.Set("y", "from-b", 1)
	b.Set("x", "from-b-newer", 5) // b has a newer version of a's key too

	changed := merge(a, b)
	if !changed {
		t.Fatal("merge should report a change when either side has something new")
	}

	snapA, snapB := a.Snapshot(), b.Snapshot()
	if snapA["y"] != (Update{Value: "from-b", Version: 1}) {
		t.Fatalf("a did not learn y from b: %v", snapA)
	}
	if snapA["x"] != (Update{Value: "from-b-newer", Version: 5}) {
		t.Fatalf("a's x should have been overwritten by b's newer version: %v", snapA)
	}
	if snapB["y"] != (Update{Value: "from-b", Version: 1}) {
		t.Fatalf("b's own y should be unchanged: %v", snapB)
	}

	// A second merge with nothing new on either side reports no change.
	if merge(a, b) {
		t.Fatal("a second merge with identical state should report no change")
	}
}

func TestGossipConvergesTwoReplicas(t *testing.T) {
	t.Parallel()

	replicas := NewCluster(2)
	replicas[0].Set("k", "v0", 1)
	replicas[1].Set("k2", "v1", 1)

	var wg sync.WaitGroup
	for _, r := range replicas {
		wg.Add(1)
		go func(r *Replica) {
			defer wg.Done()
			_, converged := r.Gossip(20)
			if !converged {
				t.Errorf("replica %d did not converge within 20 rounds", r.ID)
			}
		}(r)
	}
	wg.Wait()

	if !snapshotsEqual(replicas[0].Snapshot(), replicas[1].Snapshot()) {
		t.Fatalf("replicas disagree after convergence: %v vs %v", replicas[0].Snapshot(), replicas[1].Snapshot())
	}
}

// TestGossipClusterReachesEventualConsistency is the core concurrency case:
// five replicas each originate a distinct piece of information (one of them
// a conflicting, newer version of a key another replica also set), all five
// gossip concurrently in their own goroutines, and every replica must end up
// with the identical final state -- proving the information from every
// origin propagated to every replica regardless of the order goroutines
// happened to run in.
func TestGossipClusterReachesEventualConsistency(t *testing.T) {
	replicas := NewCluster(5)
	replicas[0].Set("region", "us-east", 1)
	replicas[1].Set("tier", "gold", 1)
	replicas[2].Set("owner", "team-payments", 1)
	replicas[3].Set("region", "us-west", 2) // newer version of a key replica 0 also set
	replicas[4].Set("status", "active", 1)

	var wg sync.WaitGroup
	for _, r := range replicas {
		wg.Add(1)
		go func(r *Replica) {
			defer wg.Done()
			_, converged := r.Gossip(50)
			if !converged {
				t.Errorf("replica %d did not converge within 50 rounds", r.ID)
			}
		}(r)
	}
	wg.Wait()

	want := map[string]Update{
		"region": {Value: "us-west", Version: 2},
		"tier":   {Value: "gold", Version: 1},
		"owner":  {Value: "team-payments", Version: 1},
		"status": {Value: "active", Version: 1},
	}
	for _, r := range replicas {
		got := r.Snapshot()
		if !snapshotsEqual(got, want) {
			t.Fatalf("replica %d final state = %v, want %v", r.ID, got, want)
		}
	}
}

func snapshotsEqual(a, b map[string]Update) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

The gossip loop is correct when every replica in the cluster ends up with
the identical final state, and when a version conflict is always resolved
by keeping the higher version regardless of which replica happened to
merge first — `TestGossipClusterReachesEventualConsistency` checks both,
against an outcome, not an interleaving. The bug this exercise guards
against is a bare `break` in the quiescence branch: it would leave only the
`switch`, and a replica that had already learned everything would keep
gossiping for the full `maxRounds` regardless, which does not break
correctness but does waste real work in a production cluster running this
loop continuously. The deadlock-safety argument is the other half worth
re-reading: locking the lower-ID replica first in `merge`, unconditionally,
is what lets `-race` (and, more importantly, production) stay clean when
many replicas are gossiping with each other in every direction at once.

## Resources

- [Epidemic Algorithms for Replicated Database Maintenance (Demers et al., 1987)](https://dl.acm.org/doi/10.1145/41840.41841) — the original gossip protocol paper.
- [Amazon Dynamo paper, §4.8 Gossip-Based Membership Protocol](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) — gossip used for cluster membership in a real production system.
- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — `break` is aware of an enclosing `switch` or `select`; `continue` is not.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-mvcc-snapshot-isolation-reads.md](32-mvcc-snapshot-isolation-reads.md) | Next: [34-two-phase-commit-coordinator.md](34-two-phase-commit-coordinator.md)
