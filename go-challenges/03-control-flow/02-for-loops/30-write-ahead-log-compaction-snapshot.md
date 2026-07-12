# Exercise 30: Write-Ahead Log Compaction with Snapshot Trigger

**Nivel: Intermedio** — validacion rapida (un test corto).

A key-value store's write-ahead log grows without bound if every restart
has to replay it from entry one, so production engines periodically
snapshot the state machine and let recovery start from there instead —
Redis's RDB-alongside-AOF and Raft's log compaction are both this same
idea. This module builds the counted replay loop that triggers a snapshot
every N entries, and the recovery path that proves the snapshot-plus-tail
replay produces exactly the same state as replaying the whole log from
scratch — the property that makes compaction safe to ship at all.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
wal/                           module example.com/wal
  go.mod                       go 1.24
  wal.go                       Entry; Snapshot; ApplyAll; ReplayWithSnapshots(entries, threshold); RecoverFrom(snapshot, entries)
  wal_test.go                    snapshot cadence, matches ApplyAll, threshold disables snapshots, recovery matches full replay, skips covered entries, empty snapshot
  cmd/demo/
    main.go                      five ledger entries snapshotted every two, then recovered from the last snapshot
```

- Files: `wal.go`, `wal_test.go`, `cmd/demo/main.go`.
- Implement: `ReplayWithSnapshots(entries []Entry, threshold int) (map[string]string, []Snapshot)` — a counted `for _, e := range entries` loop that applies each entry and takes a snapshot (resetting its counter) the instant a running count reaches `threshold`; `RecoverFrom(snapshot Snapshot, entries []Entry) map[string]string` — seed from the snapshot's state and replay only entries with `Seq > snapshot.UpToSeq`.
- Test: a snapshot is taken every `threshold` entries with the right `UpToSeq` and state; the final state always matches a plain `ApplyAll`; a non-positive threshold takes no snapshots; recovering from the last snapshot matches a full replay exactly; entries at or before `UpToSeq` are never re-applied during recovery; recovering from an empty snapshot replays everything.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why "exactly once" is a property of two functions agreeing, not one

Compaction is only safe if a process recovering from a snapshot ends up in
the *same* state as a process that replayed the entire log — otherwise
the whole point of snapshotting is undermined by subtle divergence after a
restart. That property cannot be verified by reading `ReplayWithSnapshots`
in isolation; it requires `RecoverFrom` to make a promise that mirrors
`ReplayWithSnapshots`'s own snapshot boundary exactly: an entry is either
*before or at* `UpToSeq`, in which case the snapshot already reflects it
and re-applying it during recovery would be redundant (though harmless
here, since every write's outcome is idempotent by key — the general risk
in a real log is a non-idempotent operation like "increment," where
re-applying a covered entry corrupts the state), or it is *after*
`UpToSeq`, in which case skipping it during recovery would silently lose a
write. `RecoverFrom`'s `if e.Seq <= snapshot.UpToSeq { continue }` guard is
the single line encoding that boundary, and `TestRecoverFromSnapshotMatchesFullReplayExactlyOnce`
is the test that actually proves the two functions agree, rather than
trusting that they were *written* to agree.

The counter reset inside `ReplayWithSnapshots` is the other detail worth
isolating: `count = 0` happens in the same branch as taking the snapshot,
never before it and never in a separate pass. That ordering is what makes
"every `threshold` entries" mean *threshold entries since the last
snapshot* rather than *threshold entries since the beginning of the log* —
the latter would take a snapshot only once, at entry `threshold`, and
never again, which defeats the entire purpose of periodic compaction.

Create `wal.go`:

```go
package wal

// Entry is one write-ahead-log record: assign Value to Key, tagged with a
// monotonically increasing Seq so a snapshot can record exactly how far it
// covers.
type Entry struct {
	Seq   int
	Key   string
	Value string
}

// Snapshot is a point-in-time copy of the state machine, tagged with the
// highest entry Seq it reflects. A recovering process can seed its state
// from a Snapshot and only replay entries whose Seq is greater than
// UpToSeq, instead of replaying the entire log from the beginning.
type Snapshot struct {
	UpToSeq int
	State   map[string]string
}

// ApplyAll replays every entry in order into a fresh state machine and
// returns the final state, with no snapshotting. It is the ground truth
// this package's snapshot-and-recover path must always agree with.
func ApplyAll(entries []Entry) map[string]string {
	state := make(map[string]string)
	for _, e := range entries {
		state[e.Key] = e.Value
	}
	return state
}

// ReplayWithSnapshots replays entries in order, applying each to the state
// machine, and takes a Snapshot every threshold entries. It returns the
// final state and every snapshot taken along the way.
//
// The loop is a counted pass over entries with an early snapshot trigger
// nested inside it: count resets to zero the instant a snapshot is taken,
// representing the log being truncated up to that point -- entries at or
// before a snapshot's UpToSeq never need to be replayed again after that
// snapshot exists. threshold values less than 1 disable snapshotting
// entirely (count can never reach a non-positive threshold), which is the
// deliberate behavior for callers that only want ApplyAll's plain replay.
func ReplayWithSnapshots(entries []Entry, threshold int) (map[string]string, []Snapshot) {
	state := make(map[string]string)
	var snapshots []Snapshot
	count := 0

	for _, e := range entries {
		state[e.Key] = e.Value
		count++

		if threshold > 0 && count >= threshold {
			snapshots = append(snapshots, Snapshot{UpToSeq: e.Seq, State: copyState(state)})
			count = 0
		}
	}

	return state, snapshots
}

// RecoverFrom simulates a restart from a snapshot: it seeds the state
// machine with snapshot.State, then replays only the entries whose Seq is
// greater than snapshot.UpToSeq, skipping every earlier entry because the
// snapshot already reflects it. This is the exactly-once guarantee across
// compaction: no entry the snapshot already covers is ever re-applied, and
// no entry after it is ever skipped.
func RecoverFrom(snapshot Snapshot, entries []Entry) map[string]string {
	state := copyState(snapshot.State)
	for _, e := range entries {
		if e.Seq <= snapshot.UpToSeq {
			continue // already reflected in the snapshot
		}
		state[e.Key] = e.Value
	}
	return state
}

func copyState(state map[string]string) map[string]string {
	cp := make(map[string]string, len(state))
	for k, v := range state {
		cp[k] = v
	}
	return cp
}
```

### The runnable demo

Five ledger entries are replayed with a snapshot every two entries, then
the demo recovers from the last snapshot and shows it matches the state
reached by the full replay.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/wal"
)

func main() {
	entries := []wal.Entry{
		{Seq: 1, Key: "balance:alice", Value: "100"},
		{Seq: 2, Key: "balance:bob", Value: "50"},
		{Seq: 3, Key: "balance:alice", Value: "80"},
		{Seq: 4, Key: "balance:carol", Value: "10"},
		{Seq: 5, Key: "balance:bob", Value: "65"},
	}

	final, snapshots := wal.ReplayWithSnapshots(entries, 2)
	fmt.Printf("final state: %v\n", final)
	fmt.Printf("snapshots taken: %d\n", len(snapshots))
	for i, s := range snapshots {
		fmt.Printf("  snapshot %d: upToSeq=%d state=%v\n", i, s.UpToSeq, s.State)
	}

	last := snapshots[len(snapshots)-1]
	recovered := wal.RecoverFrom(last, entries)
	fmt.Printf("recovered from last snapshot: %v\n", recovered)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
final state: map[balance:alice:80 balance:bob:65 balance:carol:10]
snapshots taken: 2
  snapshot 0: upToSeq=2 state=map[balance:alice:100 balance:bob:50]
  snapshot 1: upToSeq=4 state=map[balance:alice:80 balance:bob:50 balance:carol:10]
recovered from last snapshot: map[balance:alice:80 balance:bob:65 balance:carol:10]
```

### Tests

`TestReplayWithSnapshotsTakesSnapshotEveryThreshold` pins the exact
cadence and contents of each snapshot. `TestReplayWithSnapshotsMatchesApplyAll`
confirms snapshotting never changes the final state versus a plain
replay. `TestReplayWithSnapshotsThresholdBelowOneTakesNoSnapshots` covers
the disable case. `TestRecoverFromSnapshotMatchesFullReplayExactlyOnce`
is the load-bearing test — it is the one that actually proves compaction
is safe, by comparing `RecoverFrom` against `ApplyAll` on the same
entries. `TestRecoverFromSkipsEntriesAlreadyInSnapshot` isolates the
skip-boundary directly, and `TestRecoverFromEmptySnapshotReplaysEverything`
checks the degenerate case of recovering with no prior snapshot at all.

Create `wal_test.go`:

```go
package wal

import "testing"

func sampleEntries() []Entry {
	return []Entry{
		{Seq: 1, Key: "a", Value: "1"},
		{Seq: 2, Key: "b", Value: "2"},
		{Seq: 3, Key: "a", Value: "3"},
		{Seq: 4, Key: "c", Value: "4"},
		{Seq: 5, Key: "b", Value: "5"},
		{Seq: 6, Key: "a", Value: "6"},
		{Seq: 7, Key: "d", Value: "7"},
	}
}

func TestReplayWithSnapshotsTakesSnapshotEveryThreshold(t *testing.T) {
	t.Parallel()

	_, snapshots := ReplayWithSnapshots(sampleEntries(), 3)

	if len(snapshots) != 2 {
		t.Fatalf("len(snapshots) = %d, want 2", len(snapshots))
	}
	if snapshots[0].UpToSeq != 3 {
		t.Fatalf("snapshots[0].UpToSeq = %d, want 3", snapshots[0].UpToSeq)
	}
	if snapshots[1].UpToSeq != 6 {
		t.Fatalf("snapshots[1].UpToSeq = %d, want 6", snapshots[1].UpToSeq)
	}
	wantFirst := map[string]string{"a": "3", "b": "2"}
	if !mapsEqual(snapshots[0].State, wantFirst) {
		t.Fatalf("snapshots[0].State = %v, want %v", snapshots[0].State, wantFirst)
	}
}

func TestReplayWithSnapshotsMatchesApplyAll(t *testing.T) {
	t.Parallel()

	entries := sampleEntries()
	final, _ := ReplayWithSnapshots(entries, 3)
	want := ApplyAll(entries)

	if !mapsEqual(final, want) {
		t.Fatalf("final state = %v, want %v", final, want)
	}
}

func TestReplayWithSnapshotsThresholdBelowOneTakesNoSnapshots(t *testing.T) {
	t.Parallel()

	_, snapshots := ReplayWithSnapshots(sampleEntries(), 0)
	if snapshots != nil {
		t.Fatalf("snapshots = %v, want nil when threshold <= 0", snapshots)
	}
}

func TestRecoverFromSnapshotMatchesFullReplayExactlyOnce(t *testing.T) {
	t.Parallel()

	entries := sampleEntries()
	_, snapshots := ReplayWithSnapshots(entries, 3)
	lastSnapshot := snapshots[len(snapshots)-1]

	recovered := RecoverFrom(lastSnapshot, entries)
	want := ApplyAll(entries)

	if !mapsEqual(recovered, want) {
		t.Fatalf("RecoverFrom() = %v, want %v (must match a full replay)", recovered, want)
	}
}

func TestRecoverFromSkipsEntriesAlreadyInSnapshot(t *testing.T) {
	t.Parallel()

	snapshot := Snapshot{UpToSeq: 3, State: map[string]string{"a": "3", "b": "2"}}
	entries := []Entry{
		{Seq: 1, Key: "a", Value: "should-never-apply"},
		{Seq: 2, Key: "b", Value: "should-never-apply"},
		{Seq: 3, Key: "a", Value: "should-never-apply"},
		{Seq: 4, Key: "a", Value: "4"},
	}

	got := RecoverFrom(snapshot, entries)
	want := map[string]string{"a": "4", "b": "2"}
	if !mapsEqual(got, want) {
		t.Fatalf("RecoverFrom() = %v, want %v", got, want)
	}
}

func TestRecoverFromEmptySnapshotReplaysEverything(t *testing.T) {
	t.Parallel()

	entries := sampleEntries()
	got := RecoverFrom(Snapshot{UpToSeq: 0, State: nil}, entries)
	want := ApplyAll(entries)
	if !mapsEqual(got, want) {
		t.Fatalf("RecoverFrom() = %v, want %v", got, want)
	}
}

func mapsEqual(a, b map[string]string) bool {
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

## Review

`RecoverFrom` is correct only in conjunction with `ReplayWithSnapshots`:
the boundary condition `Seq <= UpToSeq` used to *skip* entries during
recovery must be the exact complement of "this entry was already applied
before the snapshot was taken" in `ReplayWithSnapshots`. The common
mistake this design avoids is an off-by-one at that seam — using `Seq <
UpToSeq` instead of `<=` during recovery would silently re-apply the
entry the snapshot was taken *at*, which happens to be harmless for
idempotent key assignment but would double-count a non-idempotent
operation in a real ledger. `TestRecoverFromSkipsEntriesAlreadyInSnapshot`
plants an entry exactly at `UpToSeq` with a value that must never be
applied, which is what would catch that boundary drifting by one. Run
`go test -count=1 ./...`.

## Resources

- [Redis persistence: RDB and AOF](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/) — a production system combining periodic snapshots with a replayable log.
- [The Log: What every software engineer should know about real-time data's unifying abstraction](https://engineering.linkedin.com/distributed-systems/log-what-every-software-engineer-should-know-about-real-time-datas-unifying) — the write-ahead-log and compaction model this module implements.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the counted replay loop with its nested snapshot trigger.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [29-watermark-stream-windowing-emission.md](29-watermark-stream-windowing-emission.md) | Next: [31-hierarchical-quota-token-cascade.md](31-hierarchical-quota-token-cascade.md)
