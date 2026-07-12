# Exercise 2: Consumer Group Coordinator

A consumer group is how many consumers share a topic's partitions without stepping on each other: each partition is owned by exactly one member, and each member commits how far it has read. This exercise builds the coordinator that tracks membership, computes a deterministic range assignment, rebalances on every join and leave, and stores committed offsets.

This module is fully self-contained: its own `go mod init`, all types defined inline, its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
coordinator.go        Coordinator, Join, Leave, Assignment, Commit/FetchOffset
cmd/
  demo/
    main.go           two consumers join, get disjoint partitions, commit offsets
coordinator_test.go   coverage with no overlap, rebalance on join, offset commit
```

- Files: `coordinator.go`, `cmd/demo/main.go`, `coordinator_test.go`.
- Implement: `Coordinator` with `Join(group, consumer string, numPartitions int) ([]int, error)`, `Leave`, `Assignment`, `Heartbeat`, `CommitOffset`, and `FetchOffset`.
- Test: range assignment covers all partitions with no overlap, every join triggers a rebalance, committed offsets round-trip, and an uncommitted offset reads as `-1`.
- Verify: `go test -race ./...`

### Why a deterministic range assignment and a rebalance on every change

The coordinator answers two questions: who owns which partitions, and how far has each group read. Both must be reproducible.

Ownership uses a range strategy. Sort the member IDs — sorting is what makes the result identical on every machine regardless of join order — then divide the partitions as evenly as possible. With `n` partitions and `m` members, each member gets `n/m` partitions, and the first `n%m` members (in sorted order) get one extra. For `n=5, m=2` that is member 0 owning partitions `{0,1,2}` and member 1 owning `{3,4}`: full coverage, no overlap. A member's index `i` in the sorted list gives its block: `base = i*(n/m) + min(i, n%m)`, and its count is `n/m` plus one when `i < n%m`.

The recomputation rule is just as important as the formula. Every join and every non-emptying leave recomputes the assignment for *all* current members — a rebalance — so the invariant "the union of assignments covers every partition exactly once" holds after membership settles, not just after the first join. A coordinator that assigned partitions only to the joining member and left the others stale would hand out overlapping ownership the moment a second consumer arrived. Real brokers wrap this in a generation protocol with revocation rounds; the essential primitive is the deterministic formula plus rebalance-on-change.

Offsets are decoupled from ownership. A consumer commits the offset of the last record it processed; `FetchOffset` returns it, or `-1` when nothing has been committed yet, which the consumer reads as "start from the beginning." This is the at-least-once contract: if a consumer crashes after processing but before committing, the next consumer re-reads from the last committed offset and reprocesses the tail.

Create `coordinator.go`:

```go
package coordinator

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrGroupNotFound is returned for operations on a group that does not exist.
var ErrGroupNotFound = errors.New("coordinator: group not found")

type topicPartition struct {
	Topic     string
	Partition int
}

// member tracks one consumer's assignment and liveness within a group.
type member struct {
	id         string
	partitions []int
	lastSeen   time.Time
}

// group holds membership and committed offsets for one consumer group.
type group struct {
	mu      sync.Mutex
	members map[string]*member
	offsets map[topicPartition]int64
}

// Coordinator manages all consumer groups.
type Coordinator struct {
	mu     sync.RWMutex
	groups map[string]*group
}

// NewCoordinator returns an empty coordinator.
func NewCoordinator() *Coordinator {
	return &Coordinator{groups: make(map[string]*group)}
}

func (c *Coordinator) getOrCreate(id string) *group {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.groups[id]
	if !ok {
		g = &group{members: make(map[string]*member), offsets: make(map[topicPartition]int64)}
		c.groups[id] = g
	}
	return g
}

func (c *Coordinator) lookup(id string) (*group, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	g, ok := c.groups[id]
	return g, ok
}

// rebalance recomputes the range assignment for every member. Caller holds g.mu.
func (g *group) rebalance(numPartitions int) {
	ids := make([]string, 0, len(g.members))
	for id := range g.members {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	m := len(ids)
	for i, id := range ids {
		count := numPartitions / m
		base := i*count + min(i, numPartitions%m)
		if i < numPartitions%m {
			count++
		}
		assigned := make([]int, 0, count)
		for p := base; p < base+count; p++ {
			assigned = append(assigned, p)
		}
		g.members[id].partitions = assigned
	}
}

// Join registers consumerID in groupID and returns its partition assignment.
// Every join rebalances the whole group.
func (c *Coordinator) Join(groupID, consumerID string, numPartitions int) ([]int, error) {
	if groupID == "" || consumerID == "" {
		return nil, fmt.Errorf("coordinator: join requires non-empty groupID and consumerID")
	}
	if numPartitions < 1 {
		return nil, fmt.Errorf("coordinator: numPartitions must be >= 1, got %d", numPartitions)
	}
	g := c.getOrCreate(groupID)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.members[consumerID] = &member{id: consumerID, lastSeen: time.Now()}
	g.rebalance(numPartitions)
	return append([]int(nil), g.members[consumerID].partitions...), nil
}

// Leave removes consumerID and rebalances the remaining members.
func (c *Coordinator) Leave(groupID, consumerID string, numPartitions int) error {
	g, ok := c.lookup(groupID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrGroupNotFound, groupID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.members, consumerID)
	if len(g.members) > 0 {
		g.rebalance(numPartitions)
	}
	return nil
}

// Assignment returns the partitions currently owned by consumerID.
func (c *Coordinator) Assignment(groupID, consumerID string) ([]int, error) {
	g, ok := c.lookup(groupID)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrGroupNotFound, groupID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	mem, ok := g.members[consumerID]
	if !ok {
		return nil, fmt.Errorf("coordinator: %s: %q is not a member", groupID, consumerID)
	}
	return append([]int(nil), mem.partitions...), nil
}

// Heartbeat refreshes a consumer's liveness timestamp.
func (c *Coordinator) Heartbeat(groupID, consumerID string) error {
	g, ok := c.lookup(groupID)
	if !ok {
		return fmt.Errorf("%w: %s", ErrGroupNotFound, groupID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if mem, ok := g.members[consumerID]; ok {
		mem.lastSeen = time.Now()
	}
	return nil
}

// CommitOffset stores offset for (groupID, topic, partition).
func (c *Coordinator) CommitOffset(groupID, topic string, partition int, offset int64) error {
	g := c.getOrCreate(groupID)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.offsets[topicPartition{topic, partition}] = offset
	return nil
}

// FetchOffset returns the committed offset, or -1 if none has been committed.
func (c *Coordinator) FetchOffset(groupID, topic string, partition int) (int64, error) {
	g, ok := c.lookup(groupID)
	if !ok {
		return -1, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	off, ok := g.offsets[topicPartition{topic, partition}]
	if !ok {
		return -1, nil
	}
	return off, nil
}
```

`Join` is the heart of the module. It records the member, calls `rebalance`, which recomputes *every* member's block from the sorted ID list, and returns a copy of the joining member's slice so a caller cannot mutate the coordinator's internal state. Because `rebalance` runs on every join, calling `Assignment` for an earlier member after a later one joins returns the new, smaller block — the rebalance already shifted ownership. `CommitOffset` and `FetchOffset` operate on a separate map and never touch membership, which is why a consumer can commit progress even while the group is rebalancing.

### The runnable demo

The demo has two consumers join a four-partition topic, prints each one's assignment (a copy, so the order is the sorted, deterministic block), commits an offset, and reads it back. Output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/consumer-group"
)

func main() {
	c := coordinator.NewCoordinator()
	const numPartitions = 4

	a, err := c.Join("billing", "consumer-A", numPartitions)
	if err != nil {
		fmt.Println("join A:", err)
		return
	}
	fmt.Printf("after A joins: A owns %v\n", a)

	if _, err := c.Join("billing", "consumer-B", numPartitions); err != nil {
		fmt.Println("join B:", err)
		return
	}

	// The join above rebalanced the group; re-read both assignments.
	a, _ = c.Assignment("billing", "consumer-A")
	b, _ := c.Assignment("billing", "consumer-B")
	fmt.Printf("after B joins: A owns %v, B owns %v\n", a, b)

	if err := c.CommitOffset("billing", "orders", 0, 41); err != nil {
		fmt.Println("commit:", err)
		return
	}
	off, _ := c.FetchOffset("billing", "orders", 0)
	fmt.Printf("committed offset for orders/0: %d\n", off)

	missing, _ := c.FetchOffset("billing", "orders", 3)
	fmt.Printf("uncommitted offset for orders/3: %d\n", missing)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after A joins: A owns [0 1 2 3]
after B joins: A owns [0 1], B owns [2 3]
committed offset for orders/0: 41
uncommitted offset for orders/3: -1
```

### Tests

The tests pin the assignment invariants across several partition counts, prove that a join rebalances existing members, and round-trip committed offsets including the `-1` sentinel for an uncommitted partition.

Create `coordinator_test.go`:

```go
package coordinator

import (
	"fmt"
	"testing"
)

func TestRangeAssignmentCoversWithoutOverlap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		members    int
		partitions int
	}{
		{1, 4}, {2, 4}, {3, 4}, {2, 5}, {3, 7}, {4, 4}, {5, 3},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("m%d_p%d", tc.members, tc.partitions), func(t *testing.T) {
			t.Parallel()
			c := NewCoordinator()
			for i := 0; i < tc.members; i++ {
				if _, err := c.Join("g", fmt.Sprintf("c%02d", i), tc.partitions); err != nil {
					t.Fatal(err)
				}
			}
			seen := make(map[int]int)
			for i := 0; i < tc.members; i++ {
				parts, err := c.Assignment("g", fmt.Sprintf("c%02d", i))
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range parts {
					seen[p]++
				}
			}
			for p := 0; p < tc.partitions; p++ {
				if seen[p] != 1 {
					t.Fatalf("partition %d assigned %d times, want exactly 1", p, seen[p])
				}
			}
			if len(seen) != tc.partitions {
				t.Fatalf("covered %d partitions, want %d", len(seen), tc.partitions)
			}
		})
	}
}

func TestJoinRebalancesExistingMembers(t *testing.T) {
	t.Parallel()
	c := NewCoordinator()

	a, err := c.Join("g", "consumer-A", 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 4 {
		t.Fatalf("solo member owns %v, want all 4 partitions", a)
	}

	if _, err := c.Join("g", "consumer-B", 4); err != nil {
		t.Fatal(err)
	}
	a, _ = c.Assignment("g", "consumer-A")
	b, _ := c.Assignment("g", "consumer-B")
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("after B joins: A=%v B=%v, want 2 each", a, b)
	}
}

func TestLeaveRebalances(t *testing.T) {
	t.Parallel()
	c := NewCoordinator()
	for _, id := range []string{"a", "b", "c"} {
		if _, err := c.Join("g", id, 6); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Leave("g", "b", 6); err != nil {
		t.Fatal(err)
	}
	seen := make(map[int]int)
	for _, id := range []string{"a", "c"} {
		parts, err := c.Assignment("g", id)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range parts {
			seen[p]++
		}
	}
	for p := 0; p < 6; p++ {
		if seen[p] != 1 {
			t.Fatalf("after leave, partition %d seen %d times, want 1", p, seen[p])
		}
	}
}

func TestOffsetCommitFetch(t *testing.T) {
	t.Parallel()
	c := NewCoordinator()

	if off, _ := c.FetchOffset("g", "t", 0); off != -1 {
		t.Fatalf("initial offset = %d, want -1", off)
	}
	if err := c.CommitOffset("g", "t", 0, 42); err != nil {
		t.Fatal(err)
	}
	if off, _ := c.FetchOffset("g", "t", 0); off != 42 {
		t.Fatalf("committed offset = %d, want 42", off)
	}
	if err := c.CommitOffset("g", "t", 0, 100); err != nil {
		t.Fatal(err)
	}
	if off, _ := c.FetchOffset("g", "t", 0); off != 100 {
		t.Fatalf("re-committed offset = %d, want 100", off)
	}
}

func TestFetchOffsetUnknownGroup(t *testing.T) {
	t.Parallel()
	c := NewCoordinator()
	if off, _ := c.FetchOffset("nope", "t", 0); off != -1 {
		t.Fatalf("unknown group offset = %d, want -1", off)
	}
}
```

## Review

The coordinator is correct when the assignment is deterministic and the rebalance runs on every membership change. `TestRangeAssignmentCoversWithoutOverlap` is the core proof: across several member-and-partition combinations it asserts every partition is owned exactly once and all partitions are covered, which is the property the `base = i*(n/m) + min(i, n%m)` formula and the sorted-ID order exist to guarantee. `TestJoinRebalancesExistingMembers` catches the most common mistake — assigning partitions only to the joining member — by asserting an earlier member's block shrinks when a second consumer arrives. Offsets live in their own map: `FetchOffset` returns `-1` for anything never committed (the "start from the beginning" sentinel) and the latest value otherwise, and both an unknown group and an unknown partition collapse to that same sentinel so a consumer's bootstrap path has one rule. Returning copies of the partition slice from `Join` and `Assignment` keeps a caller from mutating the coordinator's internal state, which the race detector would otherwise surface under concurrent access.

## Resources

- [Apache Kafka: Consumer Groups and rebalancing](https://kafka.apache.org/documentation/#intro_consumers) — the model this coordinator implements in miniature, including the at-least-once commit contract.
- [`sort.Strings`](https://pkg.go.dev/sort#Strings) — the deterministic member ordering that makes range assignment reproducible.
- [Go 1.21 `min` built-in](https://pkg.go.dev/builtin#min) — the two-argument minimum used in the range block formula.

---

Back to [01-partition-log.md](01-partition-log.md) | Next: [03-broker-orchestration.md](03-broker-orchestration.md)
