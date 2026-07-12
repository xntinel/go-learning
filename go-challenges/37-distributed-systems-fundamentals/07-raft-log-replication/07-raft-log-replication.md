# 7. Raft Log Replication

Raft log replication is the mechanism by which a leader serializes client commands into a distributed log that every server in the cluster eventually applies in the same order. The hard part is not the happy path; it is the invariant work: the Log Matching Property, the commitment rule that prohibits committing entries from previous terms directly, and the back-fill protocol that repairs follower logs after a leader change. This lesson builds a self-contained, testable model of those mechanics in pure Go, without network I/O, so the invariants are observable in fast in-process tests.

```text
raftlog/
  go.mod
  raftlog.go
  raftlog_test.go
  cmd/demo/main.go
```

## Concepts

### Log Entries and the Log Matching Property

Every command a client sends to the leader is wrapped in a `LogEntry` that carries three fields: the Raft term in which it was created, its one-based position in the log (index), and the opaque command bytes.

The Log Matching Property states:

1. If two entries in different logs have the same index and the same term, they store the same command.
2. If two entries in different logs have the same index and the same term, all preceding entries are identical.

AppendEntries enforces this by requiring the leader to send the index and term of the entry immediately before the new ones (`prevLogIndex`, `prevLogTerm`). A follower rejects the RPC if its log does not match at that position. On rejection the leader decrements `nextIndex[follower]` and retries with an earlier prefix, until the follower accepts.

### The Commitment Rule

An entry is committed once a majority of servers have written it durably. Only entries from the leader's *current* term are committed by counting replicas. Entries from previous terms are committed *indirectly*: when a current-term entry at a higher index is committed, all preceding entries — including those from earlier terms — become committed as a side effect. This prevents the "stale commit" anomaly described in §5.4.2 of the Raft paper.

### nextIndex and matchIndex

For each follower the leader tracks two indices:

- `nextIndex[i]`: the next log index to send to follower i. Starts at leader's last log index + 1. Decremented on rejection.
- `matchIndex[i]`: the highest log index known to be replicated on follower i. Starts at 0. Advances on successful AppendEntries.

The leader advances `commitIndex` to the highest index N such that a majority of servers have `matchIndex[i] >= N` and `log[N].Term == currentTerm`.

### The Apply Loop

Committed entries do not auto-apply; a separate apply loop (goroutine or explicit call) advances `lastApplied` from its current value to `commitIndex`, feeding each entry into a state machine in index order. The state machine in this lesson is a key-value store: commands are `SET key value` or `GET key`.

### Log Conflicts After a Leader Change

A new leader's log may diverge from a follower's log: the follower may have entries the leader does not, or the leader may have entries at indices the follower has with different terms. AppendEntries truncates conflicting entries on the follower before appending the leader's entries. The leader never truncates its own log.

## Exercises

This is a library. You verify it with `go test`, not by running a program.

### Exercise 1: LogEntry and the Log Type

Create `raftlog.go`:

```go
// raftlog.go
package raftlog

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Sentinel errors for callers that use errors.Is.
var (
	ErrNotLeader       = errors.New("raftlog: not the leader")
	ErrIndexOutOfRange = errors.New("raftlog: index out of range")
	ErrConflict        = errors.New("raftlog: log conflict")
	ErrBadCommand      = errors.New("raftlog: unrecognised command")
)

// LogEntry is one record in the replicated log.
type LogEntry struct {
	Term    int
	Index   int    // 1-based
	Command string // "SET key value" | "GET key" | "NOP"
}

// AppendArgs is the payload for an AppendEntries RPC (simplified: single leader,
// in-process calls replace network I/O).
type AppendArgs struct {
	LeaderTerm   int
	PrevLogIndex int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

// AppendResult is the response.
type AppendResult struct {
	Term    int
	Success bool
	// ConflictIndex is set on rejection: the first index of the conflicting term,
	// so the leader can skip the whole term in one step.
	ConflictIndex int
	ConflictTerm  int
}

// KVStore is the state machine: a key-value store driven by committed log entries.
type KVStore struct {
	mu   sync.Mutex
	data map[string]string
}

func newKVStore() *KVStore { return &KVStore{data: make(map[string]string)} }

// Apply executes one committed command against the store.
// Returns the value (for GET) or "" (for SET/NOP).
func (kv *KVStore) Apply(cmd string) (string, error) {
	parts := strings.Fields(cmd)
	if len(parts) == 0 {
		return "", nil
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	switch strings.ToUpper(parts[0]) {
	case "NOP":
		return "", nil
	case "SET":
		if len(parts) < 3 {
			return "", fmt.Errorf("%w: SET needs key and value", ErrBadCommand)
		}
		kv.data[parts[1]] = parts[2]
		return "", nil
	case "GET":
		if len(parts) < 2 {
			return "", fmt.Errorf("%w: GET needs key", ErrBadCommand)
		}
		return kv.data[parts[1]], nil
	default:
		return "", fmt.Errorf("%w: %q", ErrBadCommand, parts[0])
	}
}

// Get returns the value for key without applying a log entry.
func (kv *KVStore) Get(key string) string {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.data[key]
}

// RaftLog models the replicated log of one Raft server.
type RaftLog struct {
	mu          sync.Mutex
	term        int
	entries     []LogEntry // entries[0] is a sentinel (index 0, term 0)
	commitIndex int
	lastApplied int
	store       *KVStore
}

// New returns a RaftLog for the given Raft term, pre-seeded with a sentinel
// entry at index 0 so that all real entries start at index 1.
func New(term int) *RaftLog {
	return &RaftLog{
		term:    term,
		entries: []LogEntry{{Term: 0, Index: 0, Command: ""}},
		store:   newKVStore(),
	}
}

// CurrentTerm returns the server's current Raft term.
func (r *RaftLog) CurrentTerm() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.term
}

// LastIndex returns the index of the last entry (0 if the log holds only the sentinel).
func (r *RaftLog) LastIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastIndex()
}

func (r *RaftLog) lastIndex() int { return len(r.entries) - 1 }

// LastTerm returns the term of the last real entry (0 if empty).
func (r *RaftLog) LastTerm() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.entries) == 1 {
		return 0
	}
	return r.entries[len(r.entries)-1].Term
}

// CommitIndex returns the highest committed index.
func (r *RaftLog) CommitIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commitIndex
}

// entry returns the entry at index i (1-based). Caller must hold r.mu.
func (r *RaftLog) entry(i int) (LogEntry, error) {
	if i < 0 || i >= len(r.entries) {
		return LogEntry{}, fmt.Errorf("%w: %d", ErrIndexOutOfRange, i)
	}
	return r.entries[i], nil
}

// Append adds a command to the log as the leader. Term is the leader's current term.
func (r *RaftLog) Append(term int, command string) LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.entries)
	e := LogEntry{Term: term, Index: idx, Command: command}
	r.entries = append(r.entries, e)
	return e
}

// AppendEntries is the follower's handler for the AppendEntries RPC.
func (r *RaftLog) AppendEntries(args AppendArgs) AppendResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := AppendResult{Term: r.term}

	// Stale leader.
	if args.LeaderTerm < r.term {
		return result
	}
	if args.LeaderTerm > r.term {
		r.term = args.LeaderTerm
		result.Term = r.term
	}

	// Log consistency check.
	if args.PrevLogIndex > 0 {
		if args.PrevLogIndex >= len(r.entries) {
			// Follower log is too short.
			result.ConflictIndex = len(r.entries)
			result.ConflictTerm = -1
			return result
		}
		prevEntry := r.entries[args.PrevLogIndex]
		if prevEntry.Term != args.PrevLogTerm {
			// Find the first index of the conflicting term for fast roll-back.
			ct := prevEntry.Term
			ci := args.PrevLogIndex
			for ci > 1 && r.entries[ci-1].Term == ct {
				ci--
			}
			result.ConflictTerm = ct
			result.ConflictIndex = ci
			return result
		}
	}

	// Append new entries, truncating any conflict.
	for i, e := range args.Entries {
		pos := args.PrevLogIndex + 1 + i
		if pos < len(r.entries) {
			if r.entries[pos].Term != e.Term {
				// Truncate from here.
				r.entries = r.entries[:pos]
			} else {
				continue
			}
		}
		r.entries = append(r.entries, e)
	}

	// Advance commitIndex.
	if args.LeaderCommit > r.commitIndex {
		last := r.lastIndex()
		if args.LeaderCommit < last {
			r.commitIndex = args.LeaderCommit
		} else {
			r.commitIndex = last
		}
	}

	result.Success = true
	return result
}

// AdvanceCommit is called by the leader to advance commitIndex to newCommit.
// The leader only advances when a majority have replicated the entry AND the
// entry belongs to the current term.
func (r *RaftLog) AdvanceCommit(newCommit int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if newCommit <= r.commitIndex {
		return nil
	}
	if newCommit >= len(r.entries) {
		return fmt.Errorf("%w: commit=%d last=%d", ErrIndexOutOfRange, newCommit, r.lastIndex())
	}
	r.commitIndex = newCommit
	return nil
}

// ApplyCommitted applies all entries between lastApplied and commitIndex to the
// state machine in order. It is safe to call repeatedly.
func (r *RaftLog) ApplyCommitted() error {
	r.mu.Lock()
	from := r.lastApplied + 1
	to := r.commitIndex
	r.mu.Unlock()

	for i := from; i <= to; i++ {
		r.mu.Lock()
		e, err := r.entry(i)
		r.mu.Unlock()
		if err != nil {
			return err
		}
		if _, err := r.store.Apply(e.Command); err != nil {
			return err
		}
		r.mu.Lock()
		r.lastApplied = i
		r.mu.Unlock()
	}
	return nil
}

// StoreGet returns the current value for key in the state machine.
func (r *RaftLog) StoreGet(key string) string { return r.store.Get(key) }

// majority returns the commit quorum size for a cluster of n servers.
func majority(n int) int { return n/2 + 1 }

// LeaderCommitIndex computes the highest index N that a majority of matchIndex
// values are >= N, AND entries[N].Term == currentTerm. Returns the new
// commitIndex (unchanged if no advance is possible).
func LeaderCommitIndex(current, currentTerm int, log []LogEntry, matchIndex []int) int {
	best := current
	for n := len(log) - 1; n > current; n-- {
		if log[n].Term != currentTerm {
			break
		}
		count := 1 // count the leader itself
		for _, m := range matchIndex {
			if m >= n {
				count++
			}
		}
		if count >= majority(len(matchIndex)+1) {
			best = n
			break
		}
	}
	return best
}

// Entries returns a copy of log entries from startIndex to end (inclusive).
func (r *RaftLog) Entries(startIndex int) ([]LogEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if startIndex < 0 || startIndex >= len(r.entries) {
		return nil, fmt.Errorf("%w: %d", ErrIndexOutOfRange, startIndex)
	}
	cp := make([]LogEntry, len(r.entries)-startIndex)
	copy(cp, r.entries[startIndex:])
	return cp, nil
}

// PrevOf returns the index and term immediately before startIndex.
func (r *RaftLog) PrevOf(startIndex int) (prevIndex, prevTerm int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := startIndex - 1
	if prev < 0 || prev >= len(r.entries) {
		return 0, 0, fmt.Errorf("%w: %d", ErrIndexOutOfRange, prev)
	}
	return prev, r.entries[prev].Term, nil
}

// TermOf returns the term of the entry at index.
func (r *RaftLog) TermOf(index int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if index < 0 || index >= len(r.entries) {
		return 0, fmt.Errorf("%w: %d", ErrIndexOutOfRange, index)
	}
	return r.entries[index].Term, nil
}

// Len returns the number of entries including the sentinel.
func (r *RaftLog) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// FormatIndex formats index as a zero-padded string for display.
func FormatIndex(idx int) string { return strconv.Itoa(idx) }
```

The sentinel entry at index 0 means every real entry lives at index >= 1 and `prevLogIndex = 0, prevLogTerm = 0` always matches an empty log.

### Exercise 2: AppendEntries on a Follower

The follower calls `AppendEntries`. It either accepts and appends entries, or rejects and supplies `ConflictIndex`/`ConflictTerm` so the leader can roll back by a whole term at a time (the optimisation from §5.3 of the Raft paper).

Try it: build a leader that appends three entries, then send them to a fresh follower:

```go
// cmd/demo/main.go
package main

import (
	"fmt"

	"example.com/raftlog"
)

func main() {
	const clusterTerm = 2

	// Leader log: three entries.
	leader := raftlog.New(clusterTerm)
	e1 := leader.Append(clusterTerm, "SET x 10")
	leader.Append(clusterTerm, "SET y 20")
	e3 := leader.Append(clusterTerm, "SET z 30")

	// Follower starts empty.
	follower := raftlog.New(clusterTerm)

	// Send all three entries in one AppendEntries RPC.
	pi, pt, _ := leader.PrevOf(e1.Index)
	entries, _ := leader.Entries(e1.Index)
	result := follower.AppendEntries(raftlog.AppendArgs{
		LeaderTerm:   clusterTerm,
		PrevLogIndex: pi,
		PrevLogTerm:  pt,
		Entries:      entries,
		LeaderCommit: e3.Index,
	})

	fmt.Printf("AppendEntries success=%v follower len=%d commitIndex=%d\n",
		result.Success, follower.Len(), follower.CommitIndex())

	if err := follower.ApplyCommitted(); err != nil {
		fmt.Println("apply error:", err)
		return
	}
	fmt.Println("follower x =", follower.StoreGet("x"))
	fmt.Println("follower y =", follower.StoreGet("y"))
	fmt.Println("follower z =", follower.StoreGet("z"))
}
```

Run with:

```bash
go run ./cmd/demo
```

### Exercise 3: Tests

Create `raftlog_test.go`:

```go
// raftlog_test.go
package raftlog

import (
	"errors"
	"fmt"
	"testing"
)

func TestAppendAndLen(t *testing.T) {
	t.Parallel()

	r := New(1)
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (sentinel only)", r.Len())
	}
	r.Append(1, "SET a 1")
	r.Append(1, "SET b 2")
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3", r.Len())
	}
}

func TestLogMatchingProperty(t *testing.T) {
	t.Parallel()

	// Leader writes two entries.
	leader := New(1)
	e1 := leader.Append(1, "SET a 1")
	e2 := leader.Append(1, "SET b 2")

	follower := New(1)
	pi, pt, _ := leader.PrevOf(e1.Index)
	entries, _ := leader.Entries(e1.Index)
	result := follower.AppendEntries(AppendArgs{
		LeaderTerm:   1,
		PrevLogIndex: pi,
		PrevLogTerm:  pt,
		Entries:      entries,
		LeaderCommit: e2.Index,
	})
	if !result.Success {
		t.Fatalf("AppendEntries rejected: %+v", result)
	}
	if follower.Len() != leader.Len() {
		t.Fatalf("follower len=%d, leader len=%d", follower.Len(), leader.Len())
	}
	if follower.CommitIndex() != e2.Index {
		t.Fatalf("commitIndex=%d, want %d", follower.CommitIndex(), e2.Index)
	}
}

func TestFollowerRejectsStaleLeader(t *testing.T) {
	t.Parallel()

	follower := New(3)
	result := follower.AppendEntries(AppendArgs{LeaderTerm: 2})
	if result.Success {
		t.Fatal("follower accepted stale leader term")
	}
}

func TestFollowerRejectsPrevLogMismatch(t *testing.T) {
	t.Parallel()

	// Follower has one entry from term 1.
	follower := New(1)
	follower.Append(1, "SET a 1")

	// Leader tries to append at index 2 claiming prevLogTerm=2 (mismatch).
	result := follower.AppendEntries(AppendArgs{
		LeaderTerm:   2,
		PrevLogIndex: 1,
		PrevLogTerm:  2,
		Entries:      []LogEntry{{Term: 2, Index: 2, Command: "SET b 2"}},
	})
	if result.Success {
		t.Fatal("follower accepted despite prevLogTerm mismatch")
	}
	if result.ConflictTerm == 0 && result.ConflictIndex == 0 {
		t.Fatal("rejection should supply ConflictTerm or ConflictIndex")
	}
}

func TestConflictingEntriesTruncated(t *testing.T) {
	t.Parallel()

	// Follower has stale entries from an old leader (term 1).
	follower := New(2)
	follower.Append(1, "SET a 1")
	follower.Append(1, "SET a 2") // stale, will be overwritten

	// New leader (term 2) sends its version of index 2.
	result := follower.AppendEntries(AppendArgs{
		LeaderTerm:   2,
		PrevLogIndex: 1,
		PrevLogTerm:  1,
		Entries:      []LogEntry{{Term: 2, Index: 2, Command: "SET a CORRECT"}},
		LeaderCommit: 2,
	})
	if !result.Success {
		t.Fatalf("expected success: %+v", result)
	}
	// Follower must have exactly 3 slots (sentinel + 2 real).
	if follower.Len() != 3 {
		t.Fatalf("follower len=%d, want 3", follower.Len())
	}
	// The applied entry should be the leader's version.
	if err := follower.ApplyCommitted(); err != nil {
		t.Fatal(err)
	}
	if got := follower.StoreGet("a"); got != "CORRECT" {
		t.Fatalf("store a=%q, want CORRECT", got)
	}
}

func TestCommitRequiresCurrentTermEntry(t *testing.T) {
	t.Parallel()

	// Log: one entry from term 1, one entry from term 2.
	log := []LogEntry{
		{Term: 0, Index: 0}, // sentinel
		{Term: 1, Index: 1, Command: "SET a 1"},
		{Term: 2, Index: 2, Command: "SET b 2"},
	}
	matchIndex := []int{2, 2} // two followers each have both entries

	// Current term is 2. Commit can advance to 2.
	newCommit := LeaderCommitIndex(0, 2, log, matchIndex)
	if newCommit != 2 {
		t.Fatalf("commitIndex=%d, want 2", newCommit)
	}

	// If current term were 3 but no term-3 entry exists, commit stays.
	newCommit = LeaderCommitIndex(0, 3, log, matchIndex)
	if newCommit != 0 {
		t.Fatalf("commitIndex=%d, want 0 (no term-3 entry)", newCommit)
	}
}

func TestMajorityReplicatedBeforeCommit(t *testing.T) {
	t.Parallel()

	// Five-node cluster; leader + 4 followers.
	log := []LogEntry{
		{Term: 0, Index: 0},
		{Term: 3, Index: 1, Command: "SET x 9"},
		{Term: 3, Index: 2, Command: "SET y 8"},
	}
	// Three of the four followers have index 2; majority(5)=3, leader+3=4 >= 3.
	matchIndex := []int{2, 2, 2, 0} // follower match indices
	newCommit := LeaderCommitIndex(0, 3, log, matchIndex)
	// leader(1) + 3 followers have index 2 -> count=4 >= majority(5)=3.
	if newCommit != 2 {
		t.Fatalf("commitIndex=%d, want 2", newCommit)
	}

	// Only one follower has index 2; majority is not reached for index 2.
	matchIndex2 := []int{1, 1, 1, 0}
	newCommit2 := LeaderCommitIndex(0, 3, log, matchIndex2)
	// leader(1)+0 have index2 -> count=1 < 3; check index1:
	// leader(1)+3 have index1 -> count=4 >= 3; commit at 1.
	if newCommit2 != 1 {
		t.Fatalf("commitIndex=%d, want 1", newCommit2)
	}
}

func TestApplyCommittedUpdatesStore(t *testing.T) {
	t.Parallel()

	r := New(1)
	r.Append(1, "SET k 42")
	r.Append(1, "SET m 7")
	if err := r.AdvanceCommit(2); err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyCommitted(); err != nil {
		t.Fatal(err)
	}
	if got := r.StoreGet("k"); got != "42" {
		t.Fatalf("k=%q, want 42", got)
	}
	if got := r.StoreGet("m"); got != "7" {
		t.Fatalf("m=%q, want 7", got)
	}
}

func TestAdvanceCommitOutOfRange(t *testing.T) {
	t.Parallel()

	r := New(1)
	err := r.AdvanceCommit(5)
	if !errors.Is(err, ErrIndexOutOfRange) {
		t.Fatalf("err=%v, want ErrIndexOutOfRange", err)
	}
}

func TestFormatIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{42, "42"},
	}
	for _, tc := range cases {
		if got := FormatIndex(tc.in); got != tc.want {
			t.Errorf("FormatIndex(%d)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func ExampleNew() {
	r := New(2)
	e := r.Append(2, "SET x 1")
	fmt.Printf("entry index=%d term=%d command=%q\n", e.Index, e.Term, e.Command)
	// Output:
	// entry index=1 term=2 command="SET x 1"
}

func ExampleRaftLog_AppendEntries() {
	leader := New(1)
	e1 := leader.Append(1, "SET a 10")
	follower := New(1)
	pi, pt, _ := leader.PrevOf(e1.Index)
	entries, _ := leader.Entries(e1.Index)
	result := follower.AppendEntries(AppendArgs{
		LeaderTerm:   1,
		PrevLogIndex: pi,
		PrevLogTerm:  pt,
		Entries:      entries,
		LeaderCommit: e1.Index,
	})
	fmt.Printf("success=%v follower-len=%d\n", result.Success, follower.Len())
	// Output:
	// success=true follower-len=2
}

// Your turn: add TestKVStoreBadCommand that calls r.store.Apply("INVALID cmd")
// and asserts errors.Is(err, ErrBadCommand). Access store via the unexported
// field (same package tests allow this).
```

`ExampleNew` and `ExampleRaftLog_AppendEntries` are auto-verified by `go test`; they fail if the output changes.

## Common Mistakes

### Committing Previous-Term Entries Directly

Wrong: the leader counts replicas for an entry from an earlier term and advances `commitIndex` when it reaches majority. The Raft paper §5.4.2 shows this can cause committed entries to be overwritten after a leader change.

Fix: `LeaderCommitIndex` checks `log[n].Term == currentTerm` before counting. Entries from earlier terms commit only indirectly when a current-term entry at a higher index is committed.

### Forgetting to Truncate Conflicting Entries

Wrong: on AppendEntries the follower appends new entries without checking whether it already has entries at those positions with a different term. The follower ends up with a hybrid log.

Fix: `AppendEntries` compares the term of the existing entry at each position before appending. On a term mismatch, the follower truncates from that position and replaces with the leader's entries.

### Applying Entries Above commitIndex

Wrong: `lastApplied` advances ahead of `commitIndex` — the state machine sees commands before they are safe to commit (not yet replicated on a majority).

Fix: `ApplyCommitted` reads `commitIndex` under the lock, then applies only entries `lastApplied+1 .. commitIndex`. It never reads `lastApplied` and applies past `commitIndex` in the same unlocked window.

### Advancing commitIndex Without a Majority

Wrong: the leader commits an entry as soon as one follower acknowledges it.

Fix: `LeaderCommitIndex` counts how many servers (leader + followers) have `matchIndex >= n` and requires the count to reach `majority(clusterSize)`. The count always includes the leader (which already has the entry).

### PrevLogIndex Off by One

Wrong: the first real entry is at index 1; `prevLogIndex = 1` when sending that entry would check slot 1, not slot 0.

Fix: `PrevOf(startIndex)` returns `startIndex - 1`. For the first real entry (`startIndex = 1`), `prevLogIndex = 0`, which refers to the sentinel. The sentinel's term is 0, and an empty follower also returns term 0 for index 0, so the check passes.

## Verification

From `~/go-exercises/raftlog`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `go test` output is the verification — no eyeballed program output.

Run the demo to see a live replication sequence:

```bash
go run ./cmd/demo
```

## Summary

- A `LogEntry` carries `Term`, `Index`, and `Command`; the sentinel at index 0 lets `prevLogIndex = 0, prevLogTerm = 0` match any empty log.
- AppendEntries rejects when the follower's log does not match at `prevLogIndex`/`prevLogTerm`; on rejection the leader decrements `nextIndex` (or skips a whole term with `ConflictIndex`).
- Conflicting entries are truncated from the follower before new entries are appended.
- The leader only advances `commitIndex` to an entry whose term equals the current term; earlier-term entries commit indirectly.
- `ApplyCommitted` drives the state machine from `lastApplied + 1` to `commitIndex` in index order.
- `majority(n) = n/2 + 1`; the leader always counts itself when computing the replication quorum.

## What's Next

Next: [Raft Snapshots](../08-raft-snapshots/08-raft-snapshots.md).

## Resources

- [In Search of an Understandable Consensus Algorithm (Ongaro & Ousterhout, 2014)](https://raft.github.io/raft.pdf) -- Sections 5.3 and 5.4 define AppendEntries, Log Matching, and the commitment rule.
- [Students' Guide to Raft (Waldo, 2016)](https://thesquareplanet.com/blog/students-guide-to-raft/) -- documents the exact log conflict bugs students hit in the MIT 6.824 labs.
- [etcd/raft log.go](https://github.com/etcd-io/raft/blob/main/log.go) -- production implementation; read `unstable`, `storage`, and `maybeCommit` for contrast.
- [pkg.go.dev/sync](https://pkg.go.dev/sync) -- `sync.Mutex` used for the log and store; note the "must not be copied after first use" invariant.
- [Go Specification: goroutines and the memory model](https://go.dev/ref/mem) -- the formal basis for why the lock around `commitIndex`/`lastApplied` is required.
