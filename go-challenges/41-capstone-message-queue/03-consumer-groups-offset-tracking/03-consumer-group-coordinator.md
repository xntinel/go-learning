# Exercise 3: The Consumer Group Coordinator

This is where the pieces become a system. The coordinator owns the membership, drives the rebalance state machine, tracks offsets and lag, and evicts consumers that stop heartbeating. It bundles its own assignors and offset store so it stands completely alone, and it uses a `Clock` interface so failure detection can be tested deterministically without a single `time.Sleep`.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs - including its own copy of the assignors and offset store - and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
consumergroup.go     ConsumerGroup coordinator; GroupMember; RebalanceListener;
                     RangeAssignor/RoundRobinAssignor; OffsetStore; Clock; generation
cmd/
  demo/
    main.go          3 consumers over 12 partitions, lag, a leave, a group snapshot
consumergroup_test.go  join/leave/rebalance, heartbeat expiry via fake clock, lag, listeners
```

- Files: `consumergroup.go`, `cmd/demo/main.go`, `consumergroup_test.go`.
- Implement: `ConsumerGroup` with `JoinGroup`, `LeaveGroup`, `Heartbeat`, `ExpireDeadMembers`, `CommitOffset`/`FetchOffset`, `SetLatestOffset`/`GetLag`, `GetGroupInfo`, `Generation`, plus the `RebalanceListener` hook and the `Clock` interface.
- Test: single and multi-consumer assignment, leave-triggered reassignment, unknown-consumer errors, heartbeat expiry with a fake clock, lag math, and listener callbacks.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p consumer-group/cmd/demo && cd consumer-group
go mod init example.com/consumergroup
```

### The coordinator's locking discipline and the rebalance state machine

The coordinator is a single struct guarded by one mutex, `g.mu`, with each `GroupMember` holding its own small mutex for its partition list. The discipline is strict and stated in the doc comment: `g.mu` is always acquired before a member's mutex, never the other way around, so the two locks have a fixed order and cannot deadlock against each other. Every membership change - join, leave, expiry - runs `rebalanceLocked` while `g.mu` is held, which is what makes a rebalance atomic with respect to other coordinator operations.

A rebalance walks the state machine `StatePreparing -> StateCompletingRebalance -> StateStable` and increments the generation counter first, before computing anything, so any commit that arrives tagged with the old generation can be recognized as stale. It then asks the configured assignor for a fresh `consumer -> partitions` map and, for each member, fires the `RebalanceListener`: `OnPartitionsRevoked` with the member's old partitions (only if it had any), then `OnPartitionsAssigned` with its new ones. The revoke-before-assign order is the contract that lets a consumer commit its final offsets for a partition before that partition is handed to someone else.

There is a deliberate design tension here worth naming. The listener callbacks run while `g.mu` is held. That is simple and correct for an in-process coordinator, but it imposes a rule on listeners: a listener must not call back into any `ConsumerGroup` method that also takes `g.mu`, or it self-deadlocks. The escape hatch is `CommitOffset`, which goes straight to the `OffsetStore` (a separate mutex) and never touches `g.mu` - so a listener committing offsets from inside `OnPartitionsRevoked`, the most important thing a listener does, is safe. A production coordinator would release the lock before invoking listeners and re-acquire it after; this implementation keeps the lock for clarity and documents the constraint instead.

Failure detection rides on the `Clock` interface. Each member records `lastHeartbeat`; `Heartbeat` refreshes it to `clock.Now()`; `ExpireDeadMembers` removes every member whose silence exceeds the session timeout and rebalances if it removed anyone. Because time comes from the injected clock, a test substitutes a fake whose clock only moves when the test says so - making expiry a deterministic, instant, race-clean assertion instead of a flaky sleep.

Create `consumergroup.go`:

```go
package consumergroup

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrUnknownConsumer is returned when an operation names a consumer that is not
// a member of the group.
var ErrUnknownConsumer = errors.New("unknown consumer")

// GroupState is the current phase of the rebalance state machine.
type GroupState int

const (
	StateStable              GroupState = iota // normal operation
	StatePreparing                             // membership change detected
	StateCompletingRebalance                   // new assignment being pushed
)

// Clock abstracts wall-clock time so tests can substitute a controllable fake.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Assignor computes a partition-to-consumer mapping for a rebalance.
type Assignor interface {
	Assign(partitions []int, consumers []string) map[string][]int
}

// RangeAssignor assigns contiguous ranges of sorted partitions to sorted
// consumers; the first (remainder) consumers each receive one extra partition.
type RangeAssignor struct{}

func (RangeAssignor) Assign(partitions []int, consumers []string) map[string][]int {
	ps := intsSorted(partitions)
	cs := strsSorted(consumers)
	result := make(map[string][]int, len(cs))
	if len(cs) == 0 {
		return result
	}
	base, rem := len(ps)/len(cs), len(ps)%len(cs)
	idx := 0
	for i, consumer := range cs {
		count := base
		if i < rem {
			count++
		}
		result[consumer] = append([]int(nil), ps[idx:idx+count]...)
		idx += count
	}
	return result
}

// RoundRobinAssignor distributes sorted partitions across sorted consumers in
// rotation, giving the most even distribution.
type RoundRobinAssignor struct{}

func (RoundRobinAssignor) Assign(partitions []int, consumers []string) map[string][]int {
	ps := intsSorted(partitions)
	cs := strsSorted(consumers)
	result := make(map[string][]int, len(cs))
	if len(cs) == 0 {
		return result
	}
	for i, p := range ps {
		result[cs[i%len(cs)]] = append(result[cs[i%len(cs)]], p)
	}
	return result
}

func intsSorted(s []int) []int {
	out := append([]int(nil), s...)
	sort.Ints(out)
	return out
}

func strsSorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

// OffsetStore is a thread-safe in-memory store for committed group offsets.
type OffsetStore struct {
	mu      sync.RWMutex
	offsets map[string]map[int]int64
}

// NewOffsetStore returns an empty, thread-safe offset store.
func NewOffsetStore() *OffsetStore {
	return &OffsetStore{offsets: make(map[string]map[int]int64)}
}

// Commit records the processed high-water mark for group on partition.
func (s *OffsetStore) Commit(group string, partition int, offset int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.offsets[group] == nil {
		s.offsets[group] = make(map[int]int64)
	}
	s.offsets[group][partition] = offset
}

// Fetch returns the last committed offset for group on partition, or -1.
func (s *OffsetStore) Fetch(group string, partition int) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if g := s.offsets[group]; g != nil {
		if off, ok := g[partition]; ok {
			return off
		}
	}
	return -1
}

// RebalanceListener is notified before partitions are revoked (so the consumer
// can commit pending offsets) and after new partitions are assigned (so state
// can be rebuilt).
type RebalanceListener interface {
	OnPartitionsRevoked(partitions []int)
	OnPartitionsAssigned(partitions []int)
}

// GroupMember represents one consumer within a consumer group.
type GroupMember struct {
	ID            string
	mu            sync.Mutex
	partitions    []int
	lastHeartbeat time.Time
}

// AssignedPartitions returns a copy of the partitions currently held by this
// member. Safe to call concurrently with rebalances.
func (m *GroupMember) AssignedPartitions() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.partitions...)
}

func (m *GroupMember) setPartitions(p []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.partitions = append([]int(nil), p...)
}

func (m *GroupMember) touchHeartbeat(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastHeartbeat = now
}

func (m *GroupMember) sinceHeartbeat(now time.Time) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return now.Sub(m.lastHeartbeat)
}

// GroupInfo is a point-in-time snapshot of a consumer group.
type GroupInfo struct {
	Name        string
	State       GroupState
	Generation  int
	Members     []string
	Assignments map[string][]int
	Lags        map[int]int64
}

// ConsumerGroupOption configures a ConsumerGroup at construction time.
type ConsumerGroupOption func(*ConsumerGroup)

// WithAssignor sets the partition assignment strategy. Default: RangeAssignor.
func WithAssignor(a Assignor) ConsumerGroupOption {
	return func(g *ConsumerGroup) { g.assignor = a }
}

// WithSessionTimeout sets the maximum silence before a consumer is considered
// dead. Default: 30s.
func WithSessionTimeout(d time.Duration) ConsumerGroupOption {
	return func(g *ConsumerGroup) { g.sessionTimeout = d }
}

// WithClock overrides the time source. Use a fake clock in tests.
func WithClock(c Clock) ConsumerGroupOption {
	return func(g *ConsumerGroup) { g.clock = c }
}

// ConsumerGroup coordinates partition assignment, offset tracking, and failure
// detection for a named group of consumers over a fixed set of partitions.
//
// Locking discipline: g.mu is always acquired before m.mu (GroupMember).
// Listener callbacks run while g.mu is held; a listener must not call back into
// any ConsumerGroup method that acquires g.mu, or it deadlocks.
type ConsumerGroup struct {
	mu             sync.Mutex
	name           string
	partitions     []int
	members        map[string]*GroupMember
	listeners      map[string]RebalanceListener
	assignment     map[string][]int
	generation     int
	state          GroupState
	assignor       Assignor
	offsets        *OffsetStore
	clock          Clock
	sessionTimeout time.Duration
	latestOffsets  map[int]int64
}

// NewConsumerGroup creates a coordinator for the named group over the given
// partitions. Pass functional options to override defaults.
func NewConsumerGroup(name string, partitions []int, store *OffsetStore, opts ...ConsumerGroupOption) *ConsumerGroup {
	g := &ConsumerGroup{
		name:           name,
		partitions:     append([]int(nil), partitions...),
		members:        make(map[string]*GroupMember),
		listeners:      make(map[string]RebalanceListener),
		assignment:     make(map[string][]int),
		assignor:       RangeAssignor{},
		offsets:        store,
		clock:          wallClock{},
		sessionTimeout: 30 * time.Second,
		latestOffsets:  make(map[int]int64),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

// JoinGroup adds a consumer and triggers a synchronous rebalance. The returned
// GroupMember reflects the post-rebalance assignment. Joining with an existing
// ID is idempotent.
func (g *ConsumerGroup) JoinGroup(consumerID string, listener RebalanceListener) (*GroupMember, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if m, ok := g.members[consumerID]; ok {
		return m, nil
	}
	m := &GroupMember{ID: consumerID, lastHeartbeat: g.clock.Now()}
	g.members[consumerID] = m
	if listener != nil {
		g.listeners[consumerID] = listener
	}
	g.rebalanceLocked()
	return m, nil
}

// LeaveGroup removes a consumer, invokes OnPartitionsRevoked, and triggers a
// rebalance so orphaned partitions are redistributed.
func (g *ConsumerGroup) LeaveGroup(consumerID string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	m, ok := g.members[consumerID]
	if !ok {
		return fmt.Errorf("consumergroup: %w: %s", ErrUnknownConsumer, consumerID)
	}
	if l := g.listeners[consumerID]; l != nil {
		l.OnPartitionsRevoked(m.AssignedPartitions())
	}
	delete(g.members, consumerID)
	delete(g.listeners, consumerID)
	g.rebalanceLocked()
	return nil
}

// Heartbeat records a liveness signal from the consumer. Call this on each
// consumer's heartbeat interval (typically every 3s).
func (g *ConsumerGroup) Heartbeat(consumerID string) error {
	g.mu.Lock()
	m, ok := g.members[consumerID]
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("consumergroup: %w: %s", ErrUnknownConsumer, consumerID)
	}
	m.touchHeartbeat(g.clock.Now())
	return nil
}

// ExpireDeadMembers removes consumers that have not heartbeated within the
// session timeout, then triggers a rebalance. Returns the removed IDs.
func (g *ConsumerGroup) ExpireDeadMembers() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	var expired []string
	for id, m := range g.members {
		if m.sinceHeartbeat(now) > g.sessionTimeout {
			if l := g.listeners[id]; l != nil {
				l.OnPartitionsRevoked(m.AssignedPartitions())
			}
			delete(g.members, id)
			delete(g.listeners, id)
			expired = append(expired, id)
		}
	}
	if len(expired) > 0 {
		g.rebalanceLocked()
	}
	sort.Strings(expired)
	return expired
}

// CommitOffset records that the group processed all messages up to and
// including offset on partition. It goes straight to the OffsetStore (a
// separate mutex), so it is safe to call from a rebalance listener.
func (g *ConsumerGroup) CommitOffset(partition int, offset int64) {
	g.offsets.Commit(g.name, partition, offset)
}

// FetchOffset returns the last committed offset for partition, or -1.
func (g *ConsumerGroup) FetchOffset(partition int) int64 {
	return g.offsets.Fetch(g.name, partition)
}

// SetLatestOffset records the highest offset written to partition. The
// difference between this and the committed offset is the lag.
func (g *ConsumerGroup) SetLatestOffset(partition int, offset int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.latestOffsets[partition] = offset
}

// GetLag returns the number of unconsumed messages per partition. A partition
// with no messages written reports lag 0.
func (g *ConsumerGroup) GetLag() map[int]int64 {
	g.mu.Lock()
	lats := make(map[int]int64, len(g.latestOffsets))
	for p, v := range g.latestOffsets {
		lats[p] = v
	}
	ps := append([]int(nil), g.partitions...)
	g.mu.Unlock()

	result := make(map[int]int64, len(ps))
	for _, p := range ps {
		latest, ok := lats[p]
		if !ok {
			result[p] = 0
			continue
		}
		lag := latest - g.offsets.Fetch(g.name, p)
		if lag < 0 {
			lag = 0
		}
		result[p] = lag
	}
	return result
}

// GetGroupInfo returns a point-in-time snapshot of the group.
func (g *ConsumerGroup) GetGroupInfo() GroupInfo {
	g.mu.Lock()
	members := make([]string, 0, len(g.members))
	for id := range g.members {
		members = append(members, id)
	}
	sort.Strings(members)
	assignments := make(map[string][]int, len(g.assignment))
	for k, v := range g.assignment {
		assignments[k] = append([]int(nil), v...)
	}
	gen, state := g.generation, g.state
	g.mu.Unlock()

	return GroupInfo{
		Name:        g.name,
		State:       state,
		Generation:  gen,
		Members:     members,
		Assignments: assignments,
		Lags:        g.GetLag(),
	}
}

// Generation returns the current rebalance generation counter. It increments on
// every rebalance (join, leave, or heartbeat expiry).
func (g *ConsumerGroup) Generation() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generation
}

// rebalanceLocked recalculates partition assignments and notifies listeners.
// Caller must hold g.mu.
func (g *ConsumerGroup) rebalanceLocked() {
	g.state = StatePreparing
	g.generation++
	consumers := make([]string, 0, len(g.members))
	for id := range g.members {
		consumers = append(consumers, id)
	}
	g.state = StateCompletingRebalance
	newAssignment := g.assignor.Assign(g.partitions, consumers)
	for id, m := range g.members {
		prev := m.AssignedPartitions()
		next := newAssignment[id]
		if l := g.listeners[id]; l != nil && len(prev) > 0 {
			l.OnPartitionsRevoked(prev)
		}
		m.setPartitions(next)
		if l := g.listeners[id]; l != nil {
			l.OnPartitionsAssigned(next)
		}
	}
	g.assignment = newAssignment
	g.state = StateStable
}
```

Trace one join. `JoinGroup` takes `g.mu`, returns the existing member if the ID is already present (idempotent), otherwise creates a member stamped with the current clock time, registers any listener, and calls `rebalanceLocked`. Inside the rebalance the generation bumps, the assignor recomputes, and each member's listener sees its revoke/assign pair. The returned `*GroupMember` already reflects the post-rebalance assignment, and `AssignedPartitions` hands back a copy so a caller can never mutate the coordinator's internal slice.

### The runnable demo

The demo stands up three consumers over twelve partitions, prints the Range assignment, writes a thousand messages per partition and commits half to show total lag, then removes one consumer and prints the redistributed assignment and a group snapshot. The generation counter climbs with every membership change.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	cg "example.com/consumergroup"
)

func main() {
	store := cg.NewOffsetStore()
	partitions := []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	g := cg.NewConsumerGroup("orders", partitions, store)

	m0, _ := g.JoinGroup("consumer-0", nil)
	m1, _ := g.JoinGroup("consumer-1", nil)
	m2, _ := g.JoinGroup("consumer-2", nil)

	fmt.Printf("Generation %d: 3 consumers, 12 partitions\n", g.Generation())
	fmt.Printf("  consumer-0: %v\n", m0.AssignedPartitions())
	fmt.Printf("  consumer-1: %v\n", m1.AssignedPartitions())
	fmt.Printf("  consumer-2: %v\n", m2.AssignedPartitions())

	// Write 1000 messages per partition, commit half.
	for _, p := range partitions {
		g.SetLatestOffset(p, 999)
		g.CommitOffset(p, 499)
	}
	var total int64
	for _, l := range g.GetLag() {
		total += l
	}
	fmt.Printf("\nTotal lag after committing 499/999: %d messages\n", total)

	// consumer-0 leaves; its partitions redistribute to the survivors.
	g.LeaveGroup("consumer-0")
	fmt.Printf("\nGeneration %d: consumer-0 left\n", g.Generation())
	fmt.Printf("  consumer-1: %v\n", m1.AssignedPartitions())
	fmt.Printf("  consumer-2: %v\n", m2.AssignedPartitions())

	info := g.GetGroupInfo()
	fmt.Printf("\nGroup %q generation %d state %d members %v\n",
		info.Name, info.Generation, info.State, info.Members)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Generation 3: 3 consumers, 12 partitions
  consumer-0: [0 1 2 3]
  consumer-1: [4 5 6 7]
  consumer-2: [8 9 10 11]

Total lag after committing 499/999: 6000 messages

Generation 4: consumer-0 left
  consumer-1: [0 1 2 3 4 5]
  consumer-2: [6 7 8 9 10 11]

Group "orders" generation 4 state 0 members [consumer-1 consumer-2]
```

Three joins take the generation to 3 (one rebalance each); the leave takes it to 4. Total lag is 6000 because twelve partitions each have 500 pending messages (offsets 500..999) after committing 499 of a latest of 999. State 0 is `StateStable` - the group has settled after the last rebalance.

### Tests

The tests exercise the whole lifecycle. Single-consumer join gives all partitions; a second join rebalances so the two together still cover every partition; a leave reassigns the orphans and bumps the generation. Unknown-consumer leave and heartbeat both return `ErrUnknownConsumer` via `errors.Is`. `TestHeartbeatExpiresDeadConsumer` is the deterministic centerpiece: it advances a fake clock past the session timeout, refreshes one member, and asserts the silent member - and only it - is expired. Lag, listener callbacks, and the RoundRobin option round out the coverage. Every assertion runs under `-race`.

Create `consumergroup_test.go`:

```go
package consumergroup

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a controllable time source for deterministic tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func TestJoinGroupSingleConsumerGetsAllPartitions(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0, 1, 2, 3}, NewOffsetStore())
	m, err := g.JoinGroup("c1", nil)
	if err != nil {
		t.Fatalf("JoinGroup: %v", err)
	}
	if got := m.AssignedPartitions(); len(got) != 4 {
		t.Fatalf("single consumer: expected 4 partitions, got %v", got)
	}
}

func TestJoinGroupSecondConsumerTriggersRebalance(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0, 1, 2, 3}, NewOffsetStore())
	m1, _ := g.JoinGroup("c1", nil)
	m2, _ := g.JoinGroup("c2", nil)
	if n := len(m1.AssignedPartitions()) + len(m2.AssignedPartitions()); n != 4 {
		t.Fatalf("after 2-consumer rebalance: total partitions = %d, want 4", n)
	}
}

func TestLeaveGroupReassignsOrphanedPartitions(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0, 1, 2, 3}, NewOffsetStore())
	g.JoinGroup("c1", nil)
	m2, _ := g.JoinGroup("c2", nil)
	gen1 := g.Generation()

	if err := g.LeaveGroup("c1"); err != nil {
		t.Fatalf("LeaveGroup: %v", err)
	}
	if g.Generation() <= gen1 {
		t.Fatalf("generation did not increment on leave: %d -> %d", gen1, g.Generation())
	}
	if p := m2.AssignedPartitions(); len(p) != 4 {
		t.Fatalf("c2 should hold all 4 partitions after c1 left, got %v", p)
	}
}

func TestLeaveGroupUnknownConsumerReturnsError(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0}, NewOffsetStore())
	if err := g.LeaveGroup("ghost"); !errors.Is(err, ErrUnknownConsumer) {
		t.Fatalf("LeaveGroup(unknown) err = %v, want ErrUnknownConsumer", err)
	}
}

func TestHeartbeatExpiresDeadConsumer(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	g := NewConsumerGroup("grp", []int{0, 1, 2, 3}, NewOffsetStore(),
		WithClock(clk),
		WithSessionTimeout(30*time.Second),
	)
	// Both members record lastHeartbeat = T+0 on join.
	g.JoinGroup("c1", nil)
	g.JoinGroup("c2", nil)

	// Advance past the session timeout, then refresh c2 only.
	clk.Advance(31 * time.Second)
	g.Heartbeat("c2") // c2.lastHeartbeat = T+31s; c1 still at T+0

	expired := g.ExpireDeadMembers() // now=T+31s; c1 silent 31s > 30s
	if len(expired) != 1 || expired[0] != "c1" {
		t.Fatalf("expired = %v, want [c1]", expired)
	}
}

func TestHeartbeatUnknownConsumerReturnsError(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0}, NewOffsetStore())
	if err := g.Heartbeat("ghost"); !errors.Is(err, ErrUnknownConsumer) {
		t.Fatalf("Heartbeat(unknown) err = %v, want ErrUnknownConsumer", err)
	}
}

func TestCommitAndFetchOffset(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0, 1}, NewOffsetStore())
	g.JoinGroup("c1", nil)

	g.CommitOffset(0, 99)
	if got := g.FetchOffset(0); got != 99 {
		t.Fatalf("FetchOffset(0) = %d, want 99", got)
	}
	if got := g.FetchOffset(1); got != -1 {
		t.Fatalf("FetchOffset(1) = %d, want -1 (uncommitted)", got)
	}
}

func TestConsumerLagCalculation(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0}, NewOffsetStore())
	g.JoinGroup("c1", nil)

	// latest=99, committed=-1 (sentinel) -> lag = 99-(-1) = 100 messages.
	g.SetLatestOffset(0, 99)
	if got := g.GetLag()[0]; got != 100 {
		t.Fatalf("lag before commit = %d, want 100", got)
	}
	g.CommitOffset(0, 49)
	if got := g.GetLag()[0]; got != 50 {
		t.Fatalf("lag after commit 49 = %d, want 50", got)
	}
	g.CommitOffset(0, 99)
	if got := g.GetLag()[0]; got != 0 {
		t.Fatalf("lag fully caught up = %d, want 0", got)
	}
}

func TestRebalanceListenerCallbacks(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var revoked, assigned []int
	l := &stubListener{
		onRevoked: func(p []int) {
			mu.Lock()
			revoked = append(revoked, p...)
			mu.Unlock()
		},
		onAssigned: func(p []int) {
			mu.Lock()
			assigned = append(assigned, p...)
			mu.Unlock()
		},
	}

	g := NewConsumerGroup("grp", []int{0, 1, 2, 3}, NewOffsetStore())
	g.JoinGroup("c1", l)
	mu.Lock()
	firstAssigned := len(assigned)
	mu.Unlock()
	if firstAssigned == 0 {
		t.Fatal("OnPartitionsAssigned not called on first join")
	}

	g.JoinGroup("c2", nil) // triggers rebalance; c1's partitions revoked then reassigned
	mu.Lock()
	defer mu.Unlock()
	if len(revoked) == 0 {
		t.Fatal("OnPartitionsRevoked not called on rebalance")
	}
	if len(assigned) <= firstAssigned {
		t.Fatal("OnPartitionsAssigned not called after rebalance")
	}
}

func TestRoundRobinAssignorOption(t *testing.T) {
	t.Parallel()

	g := NewConsumerGroup("grp", []int{0, 1, 2, 3, 4, 5}, NewOffsetStore(),
		WithAssignor(RoundRobinAssignor{}))
	m1, _ := g.JoinGroup("c0", nil)
	m2, _ := g.JoinGroup("c1", nil)
	// RoundRobin over {c0,c1}: c0 -> {0,2,4}, c1 -> {1,3,5}.
	if got := m1.AssignedPartitions(); len(got) != 3 || got[0] != 0 {
		t.Fatalf("c0 round-robin assignment = %v, want [0 2 4]", got)
	}
	if got := m2.AssignedPartitions(); len(got) != 3 || got[0] != 1 {
		t.Fatalf("c1 round-robin assignment = %v, want [1 3 5]", got)
	}
}

type stubListener struct {
	onRevoked  func([]int)
	onAssigned func([]int)
}

func (l *stubListener) OnPartitionsRevoked(p []int)  { l.onRevoked(p) }
func (l *stubListener) OnPartitionsAssigned(p []int) { l.onAssigned(p) }
```

## Review

The coordinator is correct when membership changes are atomic and observable: each join, leave, and expiry takes `g.mu`, increments the generation, recomputes the assignment, and fires revoke-then-assign on every affected listener. Confirm the fake-clock expiry test passes with no `time.Sleep` - that is the proof the `Clock` abstraction actually decouples the logic from wall time. Confirm `CommitOffset` reaches the store without touching `g.mu`, which is what makes committing from inside `OnPartitionsRevoked` safe rather than a deadlock.

Common mistakes for this feature. The first is inverting the lock order - taking a member's mutex before `g.mu` somewhere - which reintroduces the deadlock the discipline exists to prevent; the fixed order `g.mu` then member-mu must hold everywhere. The second is firing `OnPartitionsAssigned` before `OnPartitionsRevoked`, which denies a consumer its last chance to commit a partition before losing it and causes redelivery. The third is reading the generation once and caching it forever; it changes on every rebalance, and a commit tagged with a stale generation belongs to a superseded assignment.

## Resources

- [`sync` package](https://pkg.go.dev/sync) - the `Mutex` discipline the coordinator's lock ordering relies on.
- [KIP-62: Allow consumer to send heartbeats from a background thread](https://cwiki.apache.org/confluence/display/KAFKA/KIP-62%3A+Allow+consumer+to+send+heartbeats+from+a+background+thread) - why session timeout and heartbeat interval are separate knobs.
- [Kafka Consumer Group Protocol](https://kafka.apache.org/documentation/#impl_consumer) - the join/sync/heartbeat protocol this in-process coordinator models.

---

Back to [02-offset-tracking-and-lag.md](02-offset-tracking-and-lag.md) | Next: [04-rebalance-on-membership-change.md](04-rebalance-on-membership-change.md)
