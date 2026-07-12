# 8. Raft Snapshots

Log compaction is the part of Raft that separates simulated toy implementations from ones that can run for weeks. Without it, the log grows without bound: every command ever applied occupies memory and disk forever. The hard parts are (1) adjusting every index-based operation after truncation — the log no longer starts at index 0 — and (2) the InstallSnapshot RPC, which replaces incremental replication when a follower has fallen so far behind that the leader has already discarded the entries it needs.

```text
raftsnapshot/
  go.mod
  raft.go
  raft_test.go
  cmd/demo/main.go
```

## Concepts

### Why Snapshots Exist

A Raft log is append-only. Every `AppendEntries` call adds entries; committed entries are applied to the state machine but never erased from the log. After weeks of operation the log dominates memory, start-up replay time grows linearly, and peer catch-up requires replaying the entire history. Snapshots break the log at a chosen point: the state machine at index `I` is serialized, and all entries at index <= `I` are discarded.

The Raft paper (Section 7) refers to this specific style as "snapshotting". Alternative compaction approaches (log cleaning, LSM-style tiered compaction) exist but are outside the Raft spec.

### The Snapshot Metadata

A snapshot is a triple:

- `LastIncludedIndex` — the index of the last log entry reflected in the snapshot.
- `LastIncludedTerm` — the term of that entry. Both fields are needed because term information is used by the consistency check in `AppendEntries`.
- `Data` — the serialized state machine (a byte slice; the Raft layer does not interpret it).

After installing a snapshot the node must remember `(LastIncludedIndex, LastIncludedTerm)` even though those log entries no longer exist, because they anchor all future index arithmetic.

### Index Arithmetic After Truncation

Before compaction the log is zero-based: `log[i]` holds the entry at Raft index `i`. After truncation, if `LastIncludedIndex` is `S`, then `log[0]` holds the entry at Raft index `S+1`. Converting between a Raft index and a slice index is the most common source of bugs:

```
sliceIndex = raftIndex - lastIncludedIndex - 1
```

This must be applied to every place that reads or writes the log by index: `AppendEntries` consistency check, `commitIndex` advance, `nextIndex`/`matchIndex` updates.

### InstallSnapshot RPC

When the leader computes `nextIndex[peer]` and finds it points to an entry that has already been discarded (i.e. `nextIndex[peer] <= lastIncludedIndex`), it cannot send `AppendEntries`. Instead it sends the full snapshot via `InstallSnapshot`. The follower:

1. Ignores the RPC if `Term < currentTerm` (stale leader).
2. If `LastIncludedIndex <= commitIndex`, ignores the RPC (already at least as up-to-date).
3. Discards its entire log, replaces the state machine, and updates `lastIncludedIndex`/`lastIncludedTerm`.
4. Advances `commitIndex` and `lastApplied` to `LastIncludedIndex`.

The simplified `Node` in these exercises omits step 1 (the term check) because it does not track `currentTerm`; only the index/stale check in step 2 is implemented in `HandleInstallSnapshot`.

### Concurrent Snapshotting

Serializing a large state machine can take milliseconds. Blocking the Raft event loop during serialization would delay heartbeats and cause spurious leader elections. The correct approach is to copy the state machine (or use a copy-on-write structure) and serialize it in a background goroutine. Log truncation happens only after serialization completes; until then, the old log entries must stay in memory.

### Snapshot Policy

A simple policy: trigger a snapshot when `len(log) > maxLogSize`. Production systems (etcd, CockroachDB) use a byte-size threshold on the serialized log, not entry count, because entries vary in size. Either way, the policy is a tunable separate from the Raft core.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/37-distributed-systems-fundamentals/08-raft-snapshots/08-raft-snapshots/cmd/demo
cd go-solutions/37-distributed-systems-fundamentals/08-raft-snapshots/08-raft-snapshots
```

### Exercise 1: Core Types and Log With Offset

Create `raft.go`:

```go
package raftsnapshot

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors for index arithmetic checks.
var (
	ErrIndexTooOld = errors.New("raft: index is before snapshot")
	ErrIndexOOB    = errors.New("raft: index out of range")
)

// Entry is a single Raft log entry.
type Entry struct {
	Index   int
	Term    int
	Command []byte
}

// Snapshot holds a compacted state machine image.
type Snapshot struct {
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

// InstallSnapshotArgs is sent by the leader when a follower is too far behind.
type InstallSnapshotArgs struct {
	Term              int
	LeaderID          int
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

// InstallSnapshotReply is returned by the follower.
type InstallSnapshotReply struct {
	Term    int
	Success bool
}

// StateMachine is a simple key-value store used in exercises.
type StateMachine struct {
	mu   sync.Mutex
	data map[string]string
}

// Apply applies a serialized command of the form "SET key value" or "DEL key".
func (sm *StateMachine) Apply(cmd []byte) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if sm.data == nil {
		sm.data = make(map[string]string)
	}
	var op, key, val string
	fmt.Sscanf(string(cmd), "%s %s %s", &op, &key, &val)
	switch op {
	case "SET":
		sm.data[key] = val
	case "DEL":
		delete(sm.data, key)
	}
}

// Get returns the value for key or "".
func (sm *StateMachine) Get(key string) string {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.data[key]
}

// Serialize encodes the state machine to bytes using gob.
func (sm *StateMachine) Serialize() ([]byte, error) {
	sm.mu.Lock()
	snapshot := make(map[string]string, len(sm.data))
	for k, v := range sm.data {
		snapshot[k] = v
	}
	sm.mu.Unlock()

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(snapshot); err != nil {
		return nil, fmt.Errorf("raftsnapshot: serialize: %w", err)
	}
	return buf.Bytes(), nil
}

// Restore replaces the state machine state from serialized bytes.
func (sm *StateMachine) Restore(data []byte) error {
	var m map[string]string
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&m); err != nil {
		return fmt.Errorf("raftsnapshot: restore: %w", err)
	}
	sm.mu.Lock()
	sm.data = m
	sm.mu.Unlock()
	return nil
}

// RaftLog is the compaction-aware log. After a snapshot at index S, entries
// at Raft indexes <= S are gone; entries at Raft indexes > S live in entries[].
type RaftLog struct {
	mu        sync.Mutex
	entries   []Entry // entries[0] is at Raft index (lastSnapshotIndex+1)
	snapIndex int     // LastIncludedIndex of the installed snapshot
	snapTerm  int     // LastIncludedTerm of the installed snapshot
}

// sliceIdx converts a Raft index to a slice index.
// Returns ErrIndexTooOld if the entry has been compacted, ErrIndexOOB if past the end.
func (l *RaftLog) sliceIdx(raftIdx int) (int, error) {
	if raftIdx <= l.snapIndex {
		return 0, fmt.Errorf("%w: raft=%d snap=%d", ErrIndexTooOld, raftIdx, l.snapIndex)
	}
	si := raftIdx - l.snapIndex - 1
	if si >= len(l.entries) {
		return 0, fmt.Errorf("%w: raft=%d len=%d", ErrIndexOOB, raftIdx, len(l.entries))
	}
	return si, nil
}

// LastIndex returns the Raft index of the last entry, or snapIndex if the log is empty.
func (l *RaftLog) LastIndex() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) == 0 {
		return l.snapIndex
	}
	return l.entries[len(l.entries)-1].Index
}

// LastTerm returns the term of the last entry, or snapTerm if the log is empty.
func (l *RaftLog) LastTerm() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) == 0 {
		return l.snapTerm
	}
	return l.entries[len(l.entries)-1].Term
}

// Append adds entries at the end of the log.
func (l *RaftLog) Append(entries ...Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, entries...)
}

// At returns the entry at the given Raft index.
func (l *RaftLog) At(raftIdx int) (Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	si, err := l.sliceIdx(raftIdx)
	if err != nil {
		return Entry{}, err
	}
	return l.entries[si], nil
}

// TruncateSuffix removes all entries at Raft index >= raftIdx.
func (l *RaftLog) TruncateSuffix(raftIdx int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	si := raftIdx - l.snapIndex - 1
	if si < 0 {
		si = 0
	}
	if si < len(l.entries) {
		l.entries = l.entries[:si]
	}
}

// Len returns the number of entries currently held in memory.
func (l *RaftLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Compact discards all entries at Raft index <= idx and records the snapshot metadata.
// It returns ErrIndexOOB if idx is beyond the current last entry.
func (l *RaftLog) Compact(idx, term int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if idx <= l.snapIndex {
		return nil // already compacted past this point
	}
	si := idx - l.snapIndex - 1
	if si >= len(l.entries) {
		return fmt.Errorf("%w: compact idx=%d", ErrIndexOOB, idx)
	}
	l.entries = append([]Entry(nil), l.entries[si+1:]...)
	l.snapIndex = idx
	l.snapTerm = term
	return nil
}

// SnapshotMeta returns the (lastIncludedIndex, lastIncludedTerm) pair.
func (l *RaftLog) SnapshotMeta() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapIndex, l.snapTerm
}

// InstallSnapshot replaces log state with the snapshot metadata, discarding
// all in-memory entries that are covered by or conflict with the snapshot.
func (l *RaftLog) InstallSnapshot(snap Snapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// If the snapshot covers part of our log, keep the tail.
	si := snap.LastIncludedIndex - l.snapIndex - 1
	if si >= 0 && si < len(l.entries) &&
		l.entries[si].Index == snap.LastIncludedIndex &&
		l.entries[si].Term == snap.LastIncludedTerm {
		// Keep entries after the snapshot.
		l.entries = append([]Entry(nil), l.entries[si+1:]...)
	} else {
		// Discard everything; the snapshot supersedes the log.
		l.entries = nil
	}
	l.snapIndex = snap.LastIncludedIndex
	l.snapTerm = snap.LastIncludedTerm
}
```

### Exercise 2: Snapshot Creation and Log Compaction

Append to `raft.go`:

```go
// Node is a simplified Raft node that demonstrates snapshot mechanics.
// It does not implement full consensus; it focuses on the compaction path.
type Node struct {
	id          int
	mu          sync.Mutex
	log         RaftLog
	sm          StateMachine
	commitIndex int
	lastApplied int
	snapshot    *Snapshot // most recent snapshot, nil if none
	maxLogSize  int       // trigger snapshot when log grows past this
}

// NewNode creates a Node with the given ID and snapshot threshold.
func NewNode(id, maxLogSize int) *Node {
	return &Node{id: id, maxLogSize: maxLogSize}
}

// Submit appends a command to the log and applies it to the state machine.
// In a real Raft node, Apply would happen only after commitment; here we
// apply immediately to keep the demo self-contained.
func (n *Node) Submit(term int, cmd []byte) {
	n.mu.Lock()
	idx := n.log.LastIndex() + 1
	n.log.Append(Entry{Index: idx, Term: term, Command: cmd})
	n.commitIndex = idx
	n.mu.Unlock()

	n.sm.Apply(cmd)

	n.mu.Lock()
	n.lastApplied = idx
	size := n.log.Len()
	n.mu.Unlock()

	if size > n.maxLogSize {
		_ = n.TakeSnapshot()
	}
}

// TakeSnapshot serializes the state machine and compacts the log up to commitIndex.
// The serialization happens while the node lock is not held (simulating a
// background goroutine), then the log is compacted under the lock.
func (n *Node) TakeSnapshot() error {
	n.mu.Lock()
	idx := n.commitIndex
	n.mu.Unlock()

	entry, err := n.log.At(idx)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}

	// Serialize outside the lock — this is the slow step.
	data, err := n.sm.Serialize()
	if err != nil {
		return err
	}

	snap := &Snapshot{
		LastIncludedIndex: idx,
		LastIncludedTerm:  entry.Term,
		Data:              data,
	}

	// Compact the log under the lock.
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.log.Compact(snap.LastIncludedIndex, snap.LastIncludedTerm); err != nil {
		return err
	}
	n.snapshot = snap
	return nil
}

// Snapshot returns the most recent snapshot, or nil.
func (n *Node) Snapshot() *Snapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.snapshot
}

// LogLen returns the number of entries currently in memory.
func (n *Node) LogLen() int {
	return n.log.Len()
}

// HandleInstallSnapshot applies a snapshot received from the leader.
func (n *Node) HandleInstallSnapshot(args InstallSnapshotArgs) (InstallSnapshotReply, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	reply := InstallSnapshotReply{}

	// Stale snapshot: ignore.
	if args.LastIncludedIndex <= n.commitIndex {
		reply.Success = false
		return reply, nil
	}

	snap := Snapshot{
		LastIncludedIndex: args.LastIncludedIndex,
		LastIncludedTerm:  args.LastIncludedTerm,
		Data:              args.Data,
	}
	n.log.InstallSnapshot(snap)
	n.snapshot = &snap
	n.commitIndex = args.LastIncludedIndex
	n.lastApplied = args.LastIncludedIndex

	// Restore state machine outside the struct lock would be better in production;
	// here we restore under the node lock for simplicity.
	if err := n.sm.Restore(args.Data); err != nil {
		return reply, fmt.Errorf("install snapshot: %w", err)
	}

	reply.Success = true
	return reply, nil
}

// Get reads a key from the state machine.
func (n *Node) Get(key string) string {
	return n.sm.Get(key)
}

// CommitIndex returns the node's commit index.
func (n *Node) CommitIndex() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.commitIndex
}
```

### Exercise 3: Tests

Create `raft_test.go`:

```go
package raftsnapshot

import (
	"errors"
	"fmt"
	"testing"
)

func TestRaftLogIndexArithmetic(t *testing.T) {
	t.Parallel()

	var l RaftLog
	for i := 1; i <= 5; i++ {
		l.Append(Entry{Index: i, Term: 1})
	}

	// Compact through index 3.
	if err := l.Compact(3, 1); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	if l.LastIndex() != 5 {
		t.Fatalf("LastIndex = %d, want 5", l.LastIndex())
	}
	if l.Len() != 2 {
		t.Fatalf("Len = %d, want 2 (entries 4 and 5)", l.Len())
	}

	// Entry at index 4 must still be accessible.
	e, err := l.At(4)
	if err != nil {
		t.Fatalf("At(4): %v", err)
	}
	if e.Index != 4 {
		t.Fatalf("entry index = %d, want 4", e.Index)
	}

	// Entry at index 3 (compacted) must return ErrIndexTooOld.
	_, err = l.At(3)
	if !errors.Is(err, ErrIndexTooOld) {
		t.Fatalf("At(3): err = %v, want ErrIndexTooOld", err)
	}
}

func TestRaftLogCompactIdempotent(t *testing.T) {
	t.Parallel()

	var l RaftLog
	for i := 1; i <= 4; i++ {
		l.Append(Entry{Index: i, Term: 1})
	}
	if err := l.Compact(2, 1); err != nil {
		t.Fatal(err)
	}
	// Compacting again at the same index is a no-op (not an error).
	if err := l.Compact(2, 1); err != nil {
		t.Fatalf("second Compact at same index: %v", err)
	}
}

func TestTakeSnapshotReducesLog(t *testing.T) {
	t.Parallel()

	// maxLogSize=2 so snapshot triggers after the third entry.
	n := NewNode(1, 2)
	n.Submit(1, []byte("SET a 1"))
	n.Submit(1, []byte("SET b 2"))
	n.Submit(1, []byte("SET c 3"))

	// Log should be compacted.
	if n.LogLen() == 3 {
		t.Fatal("log was not compacted after exceeding maxLogSize")
	}
	snap := n.Snapshot()
	if snap == nil {
		t.Fatal("snapshot is nil after compaction")
	}
	if snap.LastIncludedIndex < 1 {
		t.Fatalf("snapshot.LastIncludedIndex = %d, want >= 1", snap.LastIncludedIndex)
	}
}

func TestInstallSnapshotRestoresState(t *testing.T) {
	t.Parallel()

	// Build a leader node with some state.
	leader := NewNode(1, 100)
	leader.Submit(1, []byte("SET x 10"))
	leader.Submit(1, []byte("SET y 20"))
	if err := leader.TakeSnapshot(); err != nil {
		t.Fatalf("leader TakeSnapshot: %v", err)
	}
	snap := leader.Snapshot()
	if snap == nil {
		t.Fatal("leader snapshot is nil")
	}

	// A follower that missed all entries installs the snapshot.
	follower := NewNode(2, 100)
	args := InstallSnapshotArgs{
		Term:              1,
		LeaderID:          1,
		LastIncludedIndex: snap.LastIncludedIndex,
		LastIncludedTerm:  snap.LastIncludedTerm,
		Data:              snap.Data,
	}
	reply, err := follower.HandleInstallSnapshot(args)
	if err != nil {
		t.Fatalf("HandleInstallSnapshot: %v", err)
	}
	if !reply.Success {
		t.Fatal("HandleInstallSnapshot returned Success=false")
	}
	if got := follower.Get("x"); got != "10" {
		t.Fatalf("follower.Get(x) = %q, want 10", got)
	}
	if got := follower.Get("y"); got != "20" {
		t.Fatalf("follower.Get(y) = %q, want 20", got)
	}
	if follower.CommitIndex() != snap.LastIncludedIndex {
		t.Fatalf("follower commitIndex = %d, want %d", follower.CommitIndex(), snap.LastIncludedIndex)
	}
}

func TestInstallSnapshotIgnoresStale(t *testing.T) {
	t.Parallel()

	n := NewNode(1, 100)
	n.Submit(1, []byte("SET a 1"))
	n.Submit(1, []byte("SET a 2"))
	// commitIndex is now 2.

	// A snapshot at index 1 is stale; it must be ignored.
	args := InstallSnapshotArgs{
		Term:              1,
		LeaderID:          0,
		LastIncludedIndex: 1,
		LastIncludedTerm:  1,
		Data:              []byte{},
	}
	reply, err := n.HandleInstallSnapshot(args)
	if err != nil {
		t.Fatalf("HandleInstallSnapshot: %v", err)
	}
	if reply.Success {
		t.Fatal("stale snapshot should be rejected (Success=false)")
	}
}

func TestStateMachineSerializeRestore(t *testing.T) {
	t.Parallel()

	var sm StateMachine
	sm.Apply([]byte("SET foo bar"))
	sm.Apply([]byte("SET baz qux"))

	data, err := sm.Serialize()
	if err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	var sm2 StateMachine
	if err := sm2.Restore(data); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := sm2.Get("foo"); got != "bar" {
		t.Fatalf("sm2.Get(foo) = %q, want bar", got)
	}
	if got := sm2.Get("baz"); got != "qux" {
		t.Fatalf("sm2.Get(baz) = %q, want qux", got)
	}
}

func TestRaftLogTruncateSuffix(t *testing.T) {
	t.Parallel()

	var l RaftLog
	for i := 1; i <= 5; i++ {
		l.Append(Entry{Index: i, Term: 1})
	}
	l.TruncateSuffix(4) // remove entries at Raft indexes 4 and 5
	if l.LastIndex() != 3 {
		t.Fatalf("LastIndex = %d, want 3", l.LastIndex())
	}
}

func ExampleNode_Get() {
	n := NewNode(1, 100)
	n.Submit(1, []byte("SET greeting hello"))
	fmt.Println(n.Get("greeting"))
	// Output: hello
}
```

Your turn: write `TestRaftLogAtAfterInstallSnapshot` that creates a log with 5 entries, installs a snapshot covering indexes 1-3, then verifies that `At(2)` returns `ErrIndexTooOld` and `At(5)` succeeds.

### Exercise 4: Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"log"

	"example.com/raftsnapshot"
)

func main() {
	const maxLog = 3
	n := raftsnapshot.NewNode(1, maxLog)

	cmds := []string{
		"SET city tokyo",
		"SET lang go",
		"SET year 2026",
		"SET tier prod",
		"SET region asia",
	}
	for i, cmd := range cmds {
		n.Submit(1, []byte(cmd))
		fmt.Printf("submitted %d: %s  logLen=%d commitIndex=%d\n",
			i+1, cmd, n.LogLen(), n.CommitIndex())
	}

	snap := n.Snapshot()
	if snap == nil {
		log.Fatal("expected a snapshot after submitting more than maxLog entries")
	}
	fmt.Printf("\nsnapshot taken: lastIndex=%d\n", snap.LastIncludedIndex)

	// Simulate a follower that missed all entries.
	follower := raftsnapshot.NewNode(2, maxLog)
	args := raftsnapshot.InstallSnapshotArgs{
		Term:              1,
		LeaderID:          1,
		LastIncludedIndex: snap.LastIncludedIndex,
		LastIncludedTerm:  snap.LastIncludedTerm,
		Data:              snap.Data,
	}
	reply, err := follower.HandleInstallSnapshot(args)
	if err != nil {
		log.Fatalf("InstallSnapshot: %v", err)
	}
	fmt.Printf("\nfollower installed snapshot: success=%v commitIndex=%d\n",
		reply.Success, follower.CommitIndex())
	fmt.Printf("follower city=%s lang=%s\n",
		follower.Get("city"), follower.Get("lang"))
}
```

## Common Mistakes

### Off-by-one in Slice Index Conversion

Wrong: `si := raftIdx - lastSnapshotIndex` — misses the `-1` because entry `lastSnapshotIndex+1` is at slice index 0, not slice index 1.

What happens: reads the wrong entry silently, or panics with an out-of-bounds access.

Fix: `si := raftIdx - lastSnapshotIndex - 1`. This is the formula in `sliceIdx`; write it once and call it everywhere rather than repeating the arithmetic inline.

### Not Preserving SnapshotMeta After Compaction

Wrong: zeroing `snapIndex` and `snapTerm` when the log is compacted, thinking "the old metadata is gone now".

What happens: `LastIndex()` and `LastTerm()` return 0 when the in-memory log is empty, breaking the `AppendEntries` consistency check on the very next heartbeat.

Fix: update `snapIndex`/`snapTerm` to the compacted point and never reset them back to zero. The `RaftLog` struct keeps these fields separate from `entries`.

### Blocking the Raft Loop During Serialization

Wrong: calling `sm.Serialize()` while holding the Raft node lock, then truncating the log.

What happens: heartbeat processing stalls for the entire duration of serialization, peers time out, and a new leader election fires.

Fix: copy the state machine data under the lock, release the lock, serialize the copy, then re-acquire the lock to do the truncation. `TakeSnapshot` in Exercise 2 demonstrates this pattern.

### Accepting a Stale InstallSnapshot

Wrong: applying a snapshot without checking whether `LastIncludedIndex <= commitIndex`.

What happens: the follower regresses, losing state it had already applied.

Fix: check `args.LastIncludedIndex <= n.commitIndex` and return `Success=false` before touching any state. `HandleInstallSnapshot` in Exercise 2 performs this check first.

### Forgetting to Advance lastApplied After InstallSnapshot

Wrong: updating `commitIndex` to `LastIncludedIndex` but leaving `lastApplied` at its old value.

What happens: the apply loop re-applies every entry from `lastApplied` to `commitIndex`, replaying entries that the snapshot already covers — or trying to read from compacted log positions.

Fix: set both `commitIndex` and `lastApplied` to `LastIncludedIndex` when installing a snapshot.

## Verification

From `~/go-exercises/raftsnapshot`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass. `gofmt -l` must produce no output. `go test -race` must show no data races. `go run ./cmd/demo` should print snapshot and follower recovery lines without error.

## Summary

- Snapshots compact the Raft log by serializing the state machine and discarding entries at or below the snapshot index.
- Every log access must convert a Raft index to a slice index with `raftIdx - snapIndex - 1`.
- The snapshot stores `(LastIncludedIndex, LastIncludedTerm)` to anchor future index arithmetic and the `AppendEntries` consistency check.
- InstallSnapshot transfers state to a follower too far behind for incremental replication; the follower discards its log and restores the state machine.
- Stale snapshots (`LastIncludedIndex <= commitIndex`) must be ignored to prevent state regression.
- Both `commitIndex` and `lastApplied` must advance to `LastIncludedIndex` after installing a snapshot.
- Serialization must happen outside the Raft event loop to avoid blocking heartbeats.

## What's Next

Next: [CRDTs: Conflict-Free Replicated Data Types](../09-crdts/09-crdts.md).

## Resources

- [Raft paper, Section 7: Log Compaction](https://raft.github.io/raft.pdf) — the primary specification for snapshots and InstallSnapshot RPC.
- [Students' Guide to Raft: Snapshots](https://thesquareplanet.com/blog/students-guide-to-raft/#an-aside-on-snapshots) — practical notes on index arithmetic traps.
- [etcd/raft log.go](https://github.com/etcd-io/raft/blob/main/log.go) — production reference implementation of `unstable` and log offset management.
- [encoding/gob](https://pkg.go.dev/encoding/gob) — the Go package used for state machine serialization in these exercises.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the concurrency primitive that protects log and state machine state.
