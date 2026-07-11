# Exercise 6: Transactional Coordinator Sink

The file sink in exercise 1 had `PrepareCommit` and `Commit`, but it left the hard question unanswered: who decides *when* to commit, what happens to a transaction whose checkpoint never completes, and how does a sink finish committing after a crash mid-protocol? This exercise builds the coordinator that answers all three — a generic two-phase-commit sink modeled on Flink's `TwoPhaseCommitSinkFunction`, with per-checkpoint transactions, idempotent commits, abort, and crash recovery.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
coordinator.go         TxSink, Write, PreCommit, Commit, Abort, Pending, Metrics
recovery.go            Recover, CommittedRecords (read-back of durable output)
cmd/
  demo/
    main.go            run two checkpoints, crash mid-protocol, recover, commit
coordinator_test.go    idempotent commit, cover-earlier, crash recovery, abort
```

- Files: `coordinator.go`, `recovery.go`, `cmd/demo/main.go`, `coordinator_test.go`.
- Implement: `TxSink` with `Write`, `PreCommit`, `Commit`, `Abort`, and `Pending`; the package functions `Recover` and `CommittedRecords`; and the `Record` and `Metrics` types.
- Test: `coordinator_test.go` proves commit is idempotent, that a later checkpoint commits earlier staged ones, that a crash between prepare and commit recovers and commits exactly once, that an open transaction is not durable, and that abort discards.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p transactional-coordinator-sink/cmd/demo && cd transactional-coordinator-sink
go mod init example.com/transactional-coordinator-sink
go mod edit -go=1.26
```

### The transaction lifecycle, tied to checkpoint barriers

A transaction is a set of records destined to become durable together. The coordinator keeps exactly one *open* transaction at a time, holding the records written since the last checkpoint barrier. The lifecycle maps one-to-one onto Flink's `TwoPhaseCommitSinkFunction`:

- `Write` appends records to the current open transaction. They live only in memory — nothing on disk, nothing committed.
- `PreCommit(checkpointID)` is `snapshotState`: it flushes the open transaction to a staging file keyed by the checkpoint id, fsyncs it (so phase one is durable), records it in the `pending` map, and opens a fresh transaction for subsequent writes. This runs when a checkpoint barrier reaches the sink.
- `Commit(checkpointID)` is `notifyCheckpointComplete`: when the checkpoint coordinator confirms the checkpoint is globally complete, this renames the staging file to its committed name, making the records visible.
- `Abort(checkpointID)` discards a precommitted transaction whose checkpoint failed, deleting the staging file so its records are never committed.

The split between in-memory open transactions and on-disk staged transactions is the crux. Anything still in the open transaction at the moment of a crash was never made durable, so it is simply lost — and that is *correct*, because the engine has not checkpointed it yet and will replay it from the last good checkpoint. Anything staged is recoverable. There is no in-between.

Create `coordinator.go`:

```go
// Package sink provides a transactional sink coordinated with checkpoint
// barriers: the two-phase-commit protocol that gives a stream pipeline
// exactly-once output even across process crashes.
package sink

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
)

// ErrEmptyDir is returned when the staging directory path is empty.
var ErrEmptyDir = errors.New("sink: staging directory must not be empty")

// Record is the unit of data flowing through the pipeline.
type Record struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

// Metrics counts coordinator activity.
type Metrics struct {
	Committed atomic.Int64 // transactions renamed to their committed name
	Aborted   atomic.Int64 // transactions discarded before commit
	Recovered atomic.Int64 // precommitted transactions found at startup
}

// txn is one transaction: a set of records destined to become durable together.
// Before PreCommit it lives only in memory (open). PreCommit flushes it to a
// staging file and fsyncs it; Commit renames the staging file to its final
// committed name. The rename is atomic, so a record is either fully committed
// or not at all.
type txn struct {
	records []Record
}

// TxSink is a two-phase-commit sink that coordinates with checkpoint barriers.
//
// The lifecycle mirrors Flink's TwoPhaseCommitSinkFunction:
//
//	Write           -> append to the current open transaction (in memory)
//	PreCommit(id)   -> snapshotState: flush+fsync the open transaction to a
//	                   staging file keyed by checkpoint id, then open a fresh one
//	Commit(id)      -> notifyCheckpointComplete: atomically rename the staging
//	                   files of every checkpoint <= id to their committed name
//	Abort(id)       -> discard a precommitted transaction whose checkpoint failed
//
// Commit is idempotent: a duplicate notification, or a commit replayed after a
// crash, renames a file that is already renamed and is a no-op. That idempotence
// is what makes the output exactly-once.
type TxSink struct {
	dir     string
	mu      sync.Mutex
	cur     *txn
	pending map[uint64]*txn // checkpoint id -> precommitted transaction
	metrics Metrics
}

// New constructs a TxSink whose staging and committed files live in dir.
func New(dir string) (*TxSink, error) {
	if dir == "" {
		return nil, ErrEmptyDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("tx sink: mkdir: %w", err)
	}
	return &TxSink{
		dir:     dir,
		cur:     &txn{},
		pending: make(map[uint64]*txn),
	}, nil
}

// Metrics returns the live coordinator counters.
func (s *TxSink) Metrics() *Metrics { return &s.metrics }

func (s *TxSink) stagingPath(id uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("txn-%020d.staging", id))
}

func (s *TxSink) committedPath(id uint64) string {
	return filepath.Join(s.dir, fmt.Sprintf("txn-%020d.committed", id))
}

// Write appends records to the current open transaction. They become durable
// only when a later PreCommit flushes the transaction.
func (s *TxSink) Write(_ context.Context, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur.records = append(s.cur.records, records...)
	return nil
}

// PreCommit ends the current transaction and stages it durably under the given
// checkpoint id, then opens a fresh transaction for subsequent writes. This is
// the first phase of two-phase commit: the data is on disk and fsynced, but not
// yet at its committed name.
func (s *TxSink) PreCommit(_ context.Context, checkpointID uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t := s.cur
	s.cur = &txn{}

	if err := s.stageLocked(checkpointID, t.records); err != nil {
		return err
	}
	s.pending[checkpointID] = t
	return nil
}

// stageLocked writes records to the staging file for checkpointID and fsyncs
// it. Callers must hold s.mu.
func (s *TxSink) stageLocked(checkpointID uint64, records []Record) error {
	path := s.stagingPath(checkpointID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("tx sink prepare: create: %w", err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for _, r := range records {
		if err := enc.Encode(r); err != nil {
			f.Close()
			os.Remove(path)
			return fmt.Errorf("tx sink prepare: encode: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("tx sink prepare: flush: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("tx sink prepare: sync: %w", err)
	}
	return f.Close()
}

// Commit is notifyCheckpointComplete: it commits every precommitted transaction
// whose checkpoint id is <= checkpointID. A completed checkpoint N implies all
// earlier checkpoints are complete too, so any of their transactions that were
// staged but never committed (for example because a crash dropped the
// notification) are committed now. Commit is idempotent.
func (s *TxSink) Commit(_ context.Context, checkpointID uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	ids := make([]uint64, 0, len(s.pending))
	for id := range s.pending {
		if id <= checkpointID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		if err := s.commitOneLocked(id); err != nil {
			return err
		}
		delete(s.pending, id)
	}
	return nil
}

// commitOneLocked renames one staging file to its committed name. If the
// staging file is already gone but the committed file exists, the transaction
// was committed by an earlier call and this is a no-op. Callers must hold s.mu.
func (s *TxSink) commitOneLocked(id uint64) error {
	staging := s.stagingPath(id)
	committed := s.committedPath(id)
	if err := os.Rename(staging, committed); err != nil {
		// If the staging file is already gone but the committed file exists,
		// this transaction was committed by an earlier call: a no-op, not an
		// error. That is what makes Commit idempotent.
		if os.IsNotExist(err) {
			if _, statErr := os.Stat(committed); statErr == nil {
				return nil
			}
		}
		return fmt.Errorf("tx sink commit %d: %w", id, err)
	}
	s.metrics.Committed.Add(1)
	return nil
}

// Abort discards a precommitted transaction whose checkpoint did not complete.
// It deletes the staging file so the records are never committed.
func (s *TxSink) Abort(_ context.Context, checkpointID uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[checkpointID]; !ok {
		return nil
	}
	delete(s.pending, checkpointID)
	if err := os.Remove(s.stagingPath(checkpointID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("tx sink abort %d: %w", checkpointID, err)
	}
	s.metrics.Aborted.Add(1)
	return nil
}

// Pending returns the checkpoint ids of transactions that are staged but not
// yet committed, sorted ascending.
func (s *TxSink) Pending() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]uint64, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}
```

Two design choices make `Commit` exactly-once. First, it commits every pending transaction with id *less than or equal to* the given checkpoint, not just the exact match. A globally-complete checkpoint N implies every earlier checkpoint is complete too, so any earlier transaction whose `notifyCheckpointComplete` was dropped — by a crash, a lost message, a reordering — is committed now by the next notification. The engine never has to deliver every notification reliably; a later one repairs an earlier miss. Second, the commit itself is an atomic, idempotent `os.Rename`: committing a transaction whose staging file is already renamed away (because it was committed before) finds the file gone, confirms the committed file exists, and returns a no-op instead of an error. A duplicate notification, or a commit replayed after recovery, therefore cannot double-commit.

### Recovery: reloading staged transactions after a crash

`Recover` reopens the sink against an existing directory and rebuilds the pending set from the staging files left on disk. Each `txn-<id>.staging` file is a transaction that completed phase one but may not have reached phase two before the crash. `Recover` loads them all into `pending` so the engine can finish the protocol: it calls `Commit` for the checkpoints that did complete (committing those staged transactions) and `Abort` for the ones that did not. Committed files — already renamed — are left untouched, because they are already visible and re-committing them is unnecessary.

The asymmetry between staging and open transactions is what keeps recovery exactly-once. An open transaction's records were never written to disk, so `Recover` finds nothing for them and the engine replays them — no duplication, because they were never committed. A staged transaction's records *are* on disk under a staging name, so `Recover` can re-commit them — no loss, and no duplication either, because the commit is an idempotent rename. `CommittedRecords` reads back every committed transaction in checkpoint order and is the ground-truth view used to assert exactly-once: the same records, in order, exactly once, regardless of how many crashes and duplicate notifications the protocol survived.

Create `recovery.go`:

```go
package sink

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Recover reopens the sink against an existing directory after a crash. It
// reloads every staging file (a precommitted-but-uncommitted transaction) into
// the pending set so the engine can finish committing the checkpoints that
// completed and abort the ones that did not. Committed files are left untouched.
//
// Records from a transaction that was still open (never precommitted) at the
// time of the crash are not on disk at all, so they cannot be recovered here;
// the engine replays them from the last checkpoint. This is what keeps recovery
// exactly-once rather than at-least-once: nothing half-written is ever
// committed, and nothing committed is ever committed twice.
func Recover(dir string) (*TxSink, error) {
	s, err := New(dir)
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "txn-*.staging"))
	if err != nil {
		return nil, fmt.Errorf("tx sink recover: glob: %w", err)
	}
	for _, m := range matches {
		id, ok := parseTxnID(m)
		if !ok {
			continue
		}
		recs, err := readRecords(m)
		if err != nil {
			return nil, fmt.Errorf("tx sink recover: read %s: %w", m, err)
		}
		s.pending[id] = &txn{records: recs}
		s.metrics.Recovered.Add(1)
	}
	return s, nil
}

// parseTxnID extracts the checkpoint id from a "txn-<id>.staging" or
// "txn-<id>.committed" filename.
func parseTxnID(path string) (uint64, bool) {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, ".staging")
	base = strings.TrimSuffix(base, ".committed")
	base = strings.TrimPrefix(base, "txn-")
	n, err := strconv.ParseUint(base, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// CommittedRecords reads back every committed transaction in dir, in checkpoint
// order, and returns the flattened record stream. It is the ground-truth view
// of what the sink has durably emitted, used to assert exactly-once delivery.
func CommittedRecords(dir string) ([]Record, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "txn-*.committed"))
	if err != nil {
		return nil, err
	}
	type idPath struct {
		id   uint64
		path string
	}
	var files []idPath
	for _, m := range matches {
		if id, ok := parseTxnID(m); ok {
			files = append(files, idPath{id, m})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].id < files[j].id })

	var out []Record
	for _, f := range files {
		recs, err := readRecords(f.path)
		if err != nil {
			return nil, err
		}
		out = append(out, recs...)
	}
	return out, nil
}
```

### The runnable demo

The demo runs a realistic failure timeline. Run A commits checkpoint 1 cleanly, then pre-commits checkpoint 2 and crashes before committing it — and also writes an open-transaction record (`c3-lost`) that was never staged. Run B recovers: it finds checkpoint 2 staged-but-uncommitted, commits it (a duplicate `Commit(2)` proves idempotence), and reads back the durable output. The committed stream is exactly `c1-a, c1-b, c2-a` — the open-transaction `c3-lost` is correctly absent, and nothing is committed twice.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"example.com/transactional-coordinator-sink"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "txsink-demo")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx := context.Background()

	// --- Run A: process two checkpoints, then crash mid-protocol. ---
	a, err := sink.New(dir)
	if err != nil {
		return err
	}
	_ = a.Write(ctx, []sink.Record{{Key: []byte("c1-a")}, {Key: []byte("c1-b")}})
	_ = a.PreCommit(ctx, 1)
	_ = a.Commit(ctx, 1) // checkpoint 1 completes cleanly

	_ = a.Write(ctx, []sink.Record{{Key: []byte("c2-a")}})
	_ = a.PreCommit(ctx, 2) // staged durably...
	// ...crash here: the notifyCheckpointComplete for 2 never runs.
	_ = a.Write(ctx, []sink.Record{{Key: []byte("c3-lost")}}) // open txn, not durable

	fmt.Printf("run A: committed=%d pending=%v\n",
		a.Metrics().Committed.Load(), a.Pending())

	// --- Run B: recover and finish the protocol. ---
	b, err := sink.Recover(dir)
	if err != nil {
		return err
	}
	fmt.Printf("run B: recovered pending=%v\n", b.Pending())
	if err := b.Commit(ctx, 2); err != nil { // checkpoint 2 had completed
		return err
	}
	if err := b.Commit(ctx, 2); err != nil { // duplicate notification: no-op
		return err
	}

	recs, err := sink.CommittedRecords(dir)
	if err != nil {
		return err
	}
	fmt.Printf("durably committed records: %d\n", len(recs))
	for _, r := range recs {
		fmt.Printf("  %s\n", r.Key)
	}
	fmt.Printf("run B: recovered=%d\n", b.Metrics().Recovered.Load())
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
run A: committed=1 pending=[2]
run B: recovered pending=[2]
durably committed records: 3
  c1-a
  c1-b
  c2-a
run B: recovered=1
```

### Tests

`TestPreCommitThenCommit` walks the happy path and confirms the staged transaction becomes committed. `TestCommitIsIdempotent` commits the same checkpoint twice and a later empty checkpoint, asserting exactly one record is committed. `TestCommitCoversEarlierCheckpoints` stages checkpoints 1 and 2 and commits only via `Commit(2)`, proving both land. `TestCrashRecoveryCommitsPending` stages a checkpoint with one sink instance, recovers with a second, commits, and re-commits — asserting the records appear exactly once. `TestOpenTransactionNotDurable` proves an un-precommitted write recovers to nothing. `TestAbortDiscardsTransaction` proves an aborted transaction is never committed. `TestConcurrentWriteAndCheckpoint` interleaves writes, pre-commits, and commits from separate goroutines and asserts every record lands in exactly one committed transaction under `-race`.

Create `coordinator_test.go`:

```go
package sink

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func ctx() context.Context { return context.Background() }

func TestRejectsEmptyDir(t *testing.T) {
	t.Parallel()
	if _, err := New(""); !errors.Is(err, ErrEmptyDir) {
		t.Fatalf("err = %v, want ErrEmptyDir", err)
	}
}

func TestPreCommitThenCommit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx(), []Record{{Key: []byte("a")}, {Key: []byte("b")}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PreCommit(ctx(), 1); err != nil {
		t.Fatal(err)
	}
	if got := s.Pending(); len(got) != 1 || got[0] != 1 {
		t.Fatalf("Pending = %v, want [1]", got)
	}
	if err := s.Commit(ctx(), 1); err != nil {
		t.Fatal(err)
	}
	if got := s.Pending(); len(got) != 0 {
		t.Fatalf("Pending = %v, want empty after commit", got)
	}

	recs, err := CommittedRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("committed %d records, want 2", len(recs))
	}
}

// TestCommitIsIdempotent is a core exactly-once property: committing the same
// checkpoint twice (a duplicate notifyCheckpointComplete, or a replay after a
// crash) commits the records exactly once.
func TestCommitIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, _ := New(dir)
	_ = s.Write(ctx(), []Record{{Key: []byte("a")}})
	_ = s.PreCommit(ctx(), 7)

	if err := s.Commit(ctx(), 7); err != nil {
		t.Fatal(err)
	}
	// Duplicate notification for the same checkpoint.
	if err := s.Commit(ctx(), 7); err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if err := s.Commit(ctx(), 99); err != nil { // later checkpoint, nothing pending
		t.Fatalf("Commit(99): %v", err)
	}

	recs, _ := CommittedRecords(dir)
	if len(recs) != 1 {
		t.Fatalf("committed %d records, want exactly 1", len(recs))
	}
	if got := s.metrics.Committed.Load(); got != 1 {
		t.Fatalf("Committed = %d, want 1", got)
	}
}

// TestCommitCoversEarlierCheckpoints verifies notifyCheckpointComplete(N)
// commits every staged checkpoint <= N, so a missed earlier notification is
// repaired by a later one.
func TestCommitCoversEarlierCheckpoints(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, _ := New(dir)
	_ = s.Write(ctx(), []Record{{Key: []byte("c1")}})
	_ = s.PreCommit(ctx(), 1)
	_ = s.Write(ctx(), []Record{{Key: []byte("c2")}})
	_ = s.PreCommit(ctx(), 2)

	// Only the notification for checkpoint 2 arrives.
	if err := s.Commit(ctx(), 2); err != nil {
		t.Fatal(err)
	}
	recs, _ := CommittedRecords(dir)
	if len(recs) != 2 {
		t.Fatalf("committed %d records, want 2 (both checkpoints)", len(recs))
	}
	if string(recs[0].Key) != "c1" || string(recs[1].Key) != "c2" {
		t.Fatalf("committed order = %q,%q, want c1,c2", recs[0].Key, recs[1].Key)
	}
}

// TestCrashRecoveryCommitsPending simulates a crash between PreCommit and
// Commit: a fresh sink recovers the staged transaction and commits it exactly
// once, even if the recovered commit is then replayed.
func TestCrashRecoveryCommitsPending(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Run A: write and precommit checkpoint 5, then "crash" (no Commit).
	a, _ := New(dir)
	_ = a.Write(ctx(), []Record{{Key: []byte("x")}, {Key: []byte("y")}})
	_ = a.PreCommit(ctx(), 5)

	// Run B: recover. The precommitted transaction must reappear as pending.
	b, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Pending(); len(got) != 1 || got[0] != 5 {
		t.Fatalf("recovered Pending = %v, want [5]", got)
	}
	if got := b.metrics.Recovered.Load(); got != 1 {
		t.Fatalf("Recovered = %d, want 1", got)
	}

	// The engine reports checkpoint 5 completed: commit it.
	if err := b.Commit(ctx(), 5); err != nil {
		t.Fatal(err)
	}
	// Duplicate notification after recovery.
	if err := b.Commit(ctx(), 5); err != nil {
		t.Fatal(err)
	}

	recs, _ := CommittedRecords(dir)
	if len(recs) != 2 {
		t.Fatalf("committed %d records, want exactly 2", len(recs))
	}
}

// TestOpenTransactionNotDurable verifies records in an open transaction (never
// precommitted) are not committed after a crash; recovery finds nothing.
func TestOpenTransactionNotDurable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	a, _ := New(dir)
	_ = a.Write(ctx(), []Record{{Key: []byte("ghost")}}) // never precommitted

	b, err := Recover(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Pending(); len(got) != 0 {
		t.Fatalf("recovered Pending = %v, want empty (open txn is not durable)", got)
	}
	recs, _ := CommittedRecords(dir)
	if len(recs) != 0 {
		t.Fatalf("committed %d records, want 0", len(recs))
	}
}

// TestAbortDiscardsTransaction verifies a precommitted transaction whose
// checkpoint failed is discarded and never committed.
func TestAbortDiscardsTransaction(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, _ := New(dir)
	_ = s.Write(ctx(), []Record{{Key: []byte("a")}})
	_ = s.PreCommit(ctx(), 3)

	if err := s.Abort(ctx(), 3); err != nil {
		t.Fatal(err)
	}
	if got := s.Pending(); len(got) != 0 {
		t.Fatalf("Pending = %v, want empty after abort", got)
	}
	// Committing the same id now is a no-op: there is nothing to commit.
	if err := s.Commit(ctx(), 3); err != nil {
		t.Fatal(err)
	}
	recs, _ := CommittedRecords(dir)
	if len(recs) != 0 {
		t.Fatalf("committed %d records, want 0 after abort", len(recs))
	}
}

// TestConcurrentWriteAndCheckpoint interleaves writes with checkpoint barriers
// from separate goroutines and asserts every record lands in exactly one
// committed transaction with no races and no loss.
func TestConcurrentWriteAndCheckpoint(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, _ := New(dir)

	const checkpoints = 20
	const perCheckpoint = 10

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for cp := uint64(1); cp <= checkpoints; cp++ {
			for i := 0; i < perCheckpoint; i++ {
				_ = s.Write(ctx(), []Record{{Key: []byte(fmt.Sprintf("cp%d-r%d", cp, i))}})
			}
			if err := s.PreCommit(ctx(), cp); err != nil {
				t.Errorf("PreCommit(%d): %v", cp, err)
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for cp := uint64(1); cp <= checkpoints; cp++ {
			if err := s.Commit(ctx(), cp); err != nil {
				t.Errorf("Commit(%d): %v", cp, err)
			}
		}
	}()
	wg.Wait()

	// Commit any checkpoints the committer raced past before they were staged.
	if err := s.Commit(ctx(), checkpoints); err != nil {
		t.Fatal(err)
	}

	recs, err := CommittedRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := checkpoints * perCheckpoint
	if len(recs) != want {
		t.Fatalf("committed %d records, want %d (exactly once, no loss)", len(recs), want)
	}
	seen := make(map[string]bool, len(recs))
	for _, r := range recs {
		if seen[string(r.Key)] {
			t.Fatalf("record %q committed more than once", r.Key)
		}
		seen[string(r.Key)] = true
	}
}
```

## Review

The sink is correct when no crash, dropped notification, or duplicate notification can make the committed output anything other than each checkpoint's records, in order, exactly once. Confirm `PreCommit` fsyncs the staging file before recording it pending, so phase one is genuinely durable. Confirm `Commit` covers all checkpoints `<= id` so a missed notification is repaired by a later one, and that a re-commit of an already-renamed transaction is a no-op rather than an error. Confirm `Recover` reloads only staging files (open transactions are gone, by design) so replayed records are never double-committed. The classic mistakes are committing only the exact checkpoint id (a dropped notification then strands a staged transaction forever), making the commit non-idempotent so a duplicate notification errors or double-commits, and persisting open-transaction records to disk where recovery would wrongly resurrect them. The full suite passing under `go test -race ./...` — especially the crash-recovery and concurrency tests — establishes exactly-once.

## Resources

- [Flink: TwoPhaseCommitSinkFunction](https://flink.apache.org/2018/02/28/an-overview-of-end-to-end-exactly-once-processing-in-apache-flink/) — the end-to-end exactly-once design this exercise implements.
- [Flink checkpointing & exactly-once](https://nightlies.apache.org/flink/flink-docs-stable/docs/concepts/stateful-stream-processing/) — how checkpoint barriers drive the commit protocol.
- [`os.Rename`](https://pkg.go.dev/os#Rename) — the atomic, idempotent rename that makes the commit phase exactly-once.
- [`os.File.Sync`](https://pkg.go.dev/os#File.Sync) — the fsync that makes a precommitted transaction survive a crash.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-dead-letter-sink.md](05-dead-letter-sink.md) | Next: [../08-full-stream-engine/00-concepts.md](../08-full-stream-engine/00-concepts.md)
