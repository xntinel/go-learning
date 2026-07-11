# Exercise 27: Write-ahead log compaction with checkpoint barriers

**Nivel: Intermedio** — validacion rapida (un test corto).

A storage engine's write-ahead log grows without bound unless something
periodically folds committed entries into a snapshot and truncates the log
behind them. That "something" is compaction, and the one rule it can never
break is: never trust a truncation point you cannot verify. The writer
periodically emits a checkpoint barrier declaring "everything up to sequence
N is durable" — and if a checkpoint's declared sequence ever disagrees with
what compaction itself just computed as the last-applied entry, the log has
drifted from what compaction believes, and continuing to compact past that
point risks silently losing or duplicating a mutation on the next pass. This
module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
walcompact/                 independent module: example.com/walcompact
  go.mod                     go 1.24
  walcompact.go                Entry, Segment, Compact
  cmd/
    demo/
      main.go                runnable demo: two clean segments, one with a stale checkpoint
  walcompact_test.go           table test: no segments, all checkpoints match, a mismatch halts everything, a segment with no checkpoint at all
```

- Files: `walcompact.go`, `cmd/demo/main.go`, `walcompact_test.go`.
- Implement: `Compact(segments []Segment) (snapshot map[string]string, appliedThrough int64, haltedAt string, halted bool)`, applying every `Put` into a snapshot and advancing the safe truncation point only when a `Checkpoint` entry's declared sequence matches the last entry actually applied.
- Test: no segments, every checkpoint agreeing with what was applied, a checkpoint whose declared sequence disagrees (halting compaction at that segment), and a segment with a `Put` but no checkpoint at all (truncation never advances past it).
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/walcompact/cmd/demo
cd ~/go-exercises/walcompact
go mod init example.com/walcompact
go mod edit -go=1.24
```

### Why a checkpoint mismatch needs a labeled break, not a bare one

`Compact` walks segments in the outer loop and each segment's entries in the
inner loop, threading one variable — `lastSeq` — across every entry seen so
far, regardless of which segment it came from. A `Put` updates the snapshot
and advances `lastSeq`. A `Checkpoint` is the writer's own claim about where
`lastSeq` should be at that point in the log; when it agrees, compaction
records that sequence as safe to truncate up to. When it disagrees, the
checkpoint's bookkeeping and compaction's own computed state have diverged —
which can only mean the segment was truncated, corrupted, or the checkpoint
was written by a writer that was not actually caught up yet.

The disagreement is discovered inside the per-entry loop, one level below
the per-segment loop that has to stop. A bare `break` there would leave only
the current segment's remaining entries unprocessed — the outer loop would
then move on to the *next* segment and keep trusting its checkpoints, built
on top of a resume point that compaction already knows is unreliable. That
is worse than doing nothing: it would report a false "safe to truncate"
point built on unverified ground. `break segments`, fired from inside the
per-entry loop, stops the entire compaction pass the instant the first
checkpoint fails to verify, and reports which segment it happened in so an
operator can go inspect that segment by hand.

Create `walcompact.go`:

```go
package walcompact

// EntryKind distinguishes a data mutation from a checkpoint barrier.
type EntryKind int

const (
	Put EntryKind = iota
	Checkpoint
)

// Entry is one WAL record. Seq is the WAL's monotonically increasing
// sequence number, assigned at write time regardless of entry kind.
type Entry struct {
	Seq   int64
	Kind  EntryKind
	Key   string
	Value string
}

// Segment is one WAL file: an ordered slice of entries with a name for
// diagnostics (in production, the segment's filename).
type Segment struct {
	Name    string
	Entries []Entry
}

// Compact applies every Put entry across segments, in order, into a
// snapshot, and returns the highest sequence number known to be SAFE to
// truncate up to. A Checkpoint entry is a barrier the writer inserted,
// asserting "every entry up to seq N has been durably applied" -- the log
// can be truncated up to that point ONLY if the checkpoint's declared seq
// matches what compaction itself just computed as the last-applied seq. A
// mismatch means the segment was truncated, corrupted, or the checkpoint
// was written out of order: the resume point is now AMBIGUOUS, and trusting
// anything further risks silently skipping or double-applying a mutation on
// the next compaction pass. Compaction halts immediately and reports where.
func Compact(segments []Segment) (snapshot map[string]string, appliedThrough int64, haltedAt string, halted bool) {
	snapshot = make(map[string]string)
	var lastSeq int64

segments:
	for _, seg := range segments {
		for _, e := range seg.Entries {
			switch e.Kind {
			case Put:
				snapshot[e.Key] = e.Value
				lastSeq = e.Seq
			case Checkpoint:
				if e.Seq != lastSeq {
					// The barrier's own bookkeeping disagrees with what we
					// just applied. A bare break here would leave only the
					// entries loop for THIS segment -- the outer segments
					// loop would move on and keep trusting checkpoints in
					// later segments built on top of a resume point that is
					// already unreliable. break segments stops the whole
					// compaction pass at the first sign of drift.
					haltedAt = seg.Name
					halted = true
					break segments
				}
				appliedThrough = lastSeq
			}
		}
	}

	return snapshot, appliedThrough, haltedAt, halted
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/walcompact"
)

func main() {
	segments := []walcompact.Segment{
		{Name: "seg-0", Entries: []walcompact.Entry{
			{Seq: 1, Kind: walcompact.Put, Key: "a", Value: "1"},
			{Seq: 2, Kind: walcompact.Put, Key: "b", Value: "2"},
			{Seq: 2, Kind: walcompact.Checkpoint},
		}},
		{Name: "seg-1", Entries: []walcompact.Entry{
			{Seq: 3, Kind: walcompact.Put, Key: "c", Value: "3"},
			{Seq: 3, Kind: walcompact.Checkpoint},
		}},
		{Name: "seg-2", Entries: []walcompact.Entry{
			{Seq: 5, Kind: walcompact.Put, Key: "d", Value: "4"},
			{Seq: 4, Kind: walcompact.Checkpoint}, // declares seq 4, but 5 was already applied: drift
		}},
	}

	snapshot, appliedThrough, haltedAt, halted := walcompact.Compact(segments)
	fmt.Println("snapshot:", snapshot)
	fmt.Println("applied through seq:", appliedThrough)
	fmt.Println("halted:", halted, "at", haltedAt)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
snapshot: map[a:1 b:2 c:3 d:4]
applied through seq: 3
halted: true at seg-2
```

`seg-2`'s `Put` for key `d` is applied to the snapshot before the checkpoint
after it is even inspected — but that checkpoint claims sequence 4 was the
last durable entry, when compaction has already applied sequence 5. That
disagreement halts compaction right there, so `appliedThrough` stays at 3
(the last segment whose checkpoint actually verified), even though `d` is
sitting in the in-memory snapshot.

### Tests

`TestCompact` covers no segments at all, every checkpoint agreeing with the
sequence compaction computed, a checkpoint mismatch halting the entire scan
(with the segment after it never even inspected), and a segment whose `Put`
is never followed by a checkpoint at all, proving truncation never advances
past an unverified point.

Create `walcompact_test.go`:

```go
package walcompact

import "testing"

func TestCompact(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		segments           []Segment
		wantSnapshot       map[string]string
		wantAppliedThrough int64
		wantHalted         bool
		wantHaltedAt       string
	}{
		"no segments": {
			segments:     nil,
			wantSnapshot: map[string]string{},
		},
		"clean checkpoints all match and truncation advances each time": {
			segments: []Segment{
				{Name: "seg-0", Entries: []Entry{
					{Seq: 1, Kind: Put, Key: "a", Value: "1"},
					{Seq: 1, Kind: Checkpoint},
				}},
				{Name: "seg-1", Entries: []Entry{
					{Seq: 2, Kind: Put, Key: "b", Value: "2"},
					{Seq: 2, Kind: Checkpoint},
				}},
			},
			wantSnapshot:       map[string]string{"a": "1", "b": "2"},
			wantAppliedThrough: 2,
		},
		"a mismatched checkpoint halts the entire compaction": {
			segments: []Segment{
				{Name: "seg-0", Entries: []Entry{
					{Seq: 1, Kind: Put, Key: "a", Value: "1"},
					{Seq: 1, Kind: Checkpoint},
				}},
				{Name: "seg-1", Entries: []Entry{
					{Seq: 3, Kind: Put, Key: "b", Value: "2"},
					{Seq: 2, Kind: Checkpoint}, // stale: last applied seq is 3, not 2
				}},
				{Name: "seg-2", Entries: []Entry{
					{Seq: 4, Kind: Put, Key: "c", Value: "3"},
					{Seq: 4, Kind: Checkpoint},
				}},
			},
			// b IS applied to the snapshot (its Put ran before the barrier
			// mismatch was discovered), but seg-2 is never even visited, so
			// c never appears, and the safe truncation point stays at 1.
			wantSnapshot:       map[string]string{"a": "1", "b": "2"},
			wantAppliedThrough: 1,
			wantHalted:         true,
			wantHaltedAt:       "seg-1",
		},
		"a segment with no checkpoint at all never advances truncation": {
			segments: []Segment{
				{Name: "seg-0", Entries: []Entry{
					{Seq: 1, Kind: Put, Key: "a", Value: "1"},
				}},
			},
			wantSnapshot:       map[string]string{"a": "1"},
			wantAppliedThrough: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			snapshot, appliedThrough, haltedAt, halted := Compact(tc.segments)
			if len(snapshot) != len(tc.wantSnapshot) {
				t.Fatalf("snapshot = %v, want %v", snapshot, tc.wantSnapshot)
			}
			for k, v := range tc.wantSnapshot {
				if snapshot[k] != v {
					t.Fatalf("snapshot[%q] = %q, want %q", k, snapshot[k], v)
				}
			}
			if appliedThrough != tc.wantAppliedThrough {
				t.Fatalf("appliedThrough = %d, want %d", appliedThrough, tc.wantAppliedThrough)
			}
			if halted != tc.wantHalted || haltedAt != tc.wantHaltedAt {
				t.Fatalf("halted,haltedAt = %v,%q want %v,%q", halted, haltedAt, tc.wantHalted, tc.wantHaltedAt)
			}
		})
	}
}
```

Verify:

```bash
go test -count=1 ./...
```

## Review

Compaction is correct when a checkpoint mismatch stops the *entire* pass,
not just the segment it was found in, and when the safe truncation point
never advances past the last checkpoint that actually verified — the
"mismatched checkpoint halts the entire compaction" test is the one to
study, since `seg-2`'s entries are never even inspected, and its `Put`
therefore never reaches the snapshot at all. The bug this exercise guards
against is a `break` that only reaches the per-entry loop: the scan would
resume at the next segment, silently building on a resume point that was
already known to be unreliable. The "no checkpoint at all" test pins the
other half of the contract: applying a `Put` to the in-memory snapshot and
advancing the *safe truncation point* are two different guarantees, and the
second one only ever moves on a verified barrier.

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` can leave any number of enclosing loops at once.
- [PostgreSQL WAL internals](https://www.postgresql.org/docs/current/wal-internals.html) — checkpoints and the truncation guarantee they provide in a real WAL implementation.
- [etcd/raft: log compaction](https://pkg.go.dev/go.etcd.io/etcd/raft/v3#hdr-Usage) — compaction and snapshotting in a production consensus log.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [26-consistent-hash-shard-routing.md](26-consistent-hash-shard-routing.md) | Next: [28-bloom-filter-membership-dedup.md](28-bloom-filter-membership-dedup.md)
