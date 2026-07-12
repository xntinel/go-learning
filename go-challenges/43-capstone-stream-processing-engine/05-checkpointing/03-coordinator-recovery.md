# Exercise 3: Coordinator and Recovery

The coordinator is the conductor of the checkpoint protocol: it assigns checkpoint IDs, injects barriers into the sources, collects an acknowledgement from every operator, and only then declares the checkpoint complete and prunes old ones. This module wires the coordinator to a stateful operator and closes the loop with recovery — rebuilding an operator from the latest completed checkpoint and proving the replay after a crash does not double-count.

This module is fully self-contained. It bundles the core types, a `StateBackend` interface with a filesystem implementation, the coordinator, and a counting operator, and ships its own demo and tests.

## What you'll build

```text
checkpoint.go          core types, StateBackend interface, FileStateBackend
coordinator.go         BarrierSink, Coordinator, TriggerCheckpoint, Acknowledge, Run
operator.go            CountingOperator, Snapshot, Restore, RestoreLatest
cmd/
  demo/
    main.go            process records, checkpoint, recover from the latest
checkpoint_test.go     trigger/ack, wait-for-all, snapshot/restore, exactly-once
```

- Files: `checkpoint.go`, `coordinator.go`, `operator.go`, `cmd/demo/main.go`, `checkpoint_test.go`.
- Implement: `Coordinator` with `TriggerCheckpoint`, `Acknowledge`, and `Run`; `CountingOperator` with `Process`, `Snapshot`, `Restore`; and the `RestoreLatest` recovery helper.
- Test: `checkpoint_test.go` proves a single-operator checkpoint finalizes, a multi-operator checkpoint waits for every acknowledgement, snapshot/restore round-trips at several sizes, and a crash-restart cycle yields exactly-once counts.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/05-checkpointing/03-coordinator-recovery/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/05-checkpointing/03-coordinator-recovery
go mod edit -go=1.26
```

### The lifecycle the coordinator enforces

`TriggerCheckpoint` is the start of a checkpoint. Under its lock the coordinator claims the next `CheckpointID`, builds a fresh acknowledgement set with one `false` entry per operator, and records it in the `pending` map; then, outside the lock, it injects a barrier carrying that ID into every source. Because the pending entry is created before any barrier is injected, an operator that snapshots and acknowledges almost instantly can never arrive "before" the coordinator is ready for it.

`Acknowledge` is the other half. It flips the operator's entry to `true`, then scans the whole acknowledgement set: if any operator is still outstanding it returns immediately and the checkpoint stays in-flight. Only when every operator has reported does it delete the pending entry, call `MarkComplete` on the backend, and prune. This all-or-nothing rule is the heart of consistency — a checkpoint marked complete after only the first acknowledgement would record some operators' state and not others', and recovery from it would resurrect an impossible global state. The coordinator keeps several checkpoints in `pending` at once, keyed by ID, because the barrier for checkpoint N+1 may be triggered before every operator has finished acknowledging N.

`Run` is the production driver: a ticker that calls `TriggerCheckpoint` every interval until its context is cancelled, at which point it returns `ctx.Err()` for the caller to treat as a clean shutdown.

Create `checkpoint.go`:
```go
// Package checkpoint coordinates Chandy-Lamport checkpoints across a stream
// pipeline: it triggers barriers, tracks per-operator acknowledgement, persists
// state, and recovers operators from the latest completed checkpoint.
package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sentinel errors. Every error returned by this package wraps one of these.
var (
	ErrNoCheckpoint       = errors.New("no completed checkpoint found")
	ErrCheckpointMismatch = errors.New("checkpoint ID mismatch")
	ErrStateNotFound      = errors.New("state not found")
)

// CheckpointID is a monotonically increasing identifier assigned by the Coordinator.
type CheckpointID uint64

// Barrier is a control message injected by the Coordinator into the stream.
type Barrier struct {
	ID        CheckpointID
	Timestamp time.Time
}

// Record is a data element flowing through the operator graph.
type Record struct {
	Data []byte
}

// StreamEvent carries either a Record or a Barrier. Exactly one field is non-nil.
type StreamEvent struct {
	Record  *Record
	Barrier *Barrier
}

// Snapshotable is implemented by stateful operators that checkpoint their state.
type Snapshotable interface {
	Snapshot() ([]byte, error)
	Restore([]byte) error
}

// StateBackend stores and retrieves serialized operator state. Implementations
// must be safe for concurrent use.
type StateBackend interface {
	SaveState(checkpointID CheckpointID, operatorID string, state []byte) error
	LoadState(checkpointID CheckpointID, operatorID string) ([]byte, error)
	LatestCheckpoint() (CheckpointID, error)
	MarkComplete(checkpointID CheckpointID) error
	Prune(keepN int) error
	Close() error
}

// FileStateBackend persists checkpoint state on the local filesystem using
// atomic write-to-temp-then-rename.
type FileStateBackend struct {
	mu  sync.Mutex
	dir string
}

// NewFileStateBackend creates a FileStateBackend rooted at dir.
func NewFileStateBackend(dir string) (*FileStateBackend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("checkpoint: create backend dir: %w", err)
	}
	return &FileStateBackend{dir: dir}, nil
}

func (b *FileStateBackend) ckptDir(id CheckpointID) string {
	return filepath.Join(b.dir, fmt.Sprintf("ckpt-%d", uint64(id)))
}

// SaveState atomically writes state via a same-directory temp file and rename.
func (b *FileStateBackend) SaveState(id CheckpointID, operatorID string, state []byte) error {
	dir := b.ckptDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("checkpoint: mkdir ckpt-%d: %w", id, err)
	}
	dst := filepath.Join(dir, operatorID+".state")
	tmp, err := os.CreateTemp(dir, ".tmp-"+operatorID+"-*")
	if err != nil {
		return fmt.Errorf("checkpoint: create temp for %s: %w", operatorID, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(state); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("checkpoint: write %s: %w", operatorID, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("checkpoint: close temp %s: %w", operatorID, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("checkpoint: rename for %s: %w", operatorID, err)
	}
	return nil
}

// LoadState reads state for operatorID at the given checkpoint.
func (b *FileStateBackend) LoadState(id CheckpointID, operatorID string) ([]byte, error) {
	path := filepath.Join(b.ckptDir(id), operatorID+".state")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("checkpoint: %w: operator=%s ckpt=%d", ErrStateNotFound, operatorID, id)
	}
	if err != nil {
		return nil, fmt.Errorf("checkpoint: load state %s: %w", operatorID, err)
	}
	return data, nil
}

// MarkComplete writes the completion marker for checkpoint id.
func (b *FileStateBackend) MarkComplete(id CheckpointID) error {
	dir := b.ckptDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("checkpoint: mkdir for complete ckpt-%d: %w", id, err)
	}
	f, err := os.Create(filepath.Join(dir, "completed"))
	if err != nil {
		return fmt.Errorf("checkpoint: mark complete ckpt-%d: %w", id, err)
	}
	return f.Close()
}

// LatestCheckpoint returns the highest completed checkpoint ID.
func (b *FileStateBackend) LatestCheckpoint() (CheckpointID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	completed, err := b.completedIDs()
	if err != nil {
		return 0, err
	}
	if len(completed) == 0 {
		return 0, fmt.Errorf("checkpoint: %w", ErrNoCheckpoint)
	}
	return CheckpointID(completed[len(completed)-1]), nil
}

// Prune removes completed checkpoints, keeping the most recent keepN.
func (b *FileStateBackend) Prune(keepN int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	completed, err := b.completedIDs()
	if err != nil {
		return err
	}
	if len(completed) <= keepN {
		return nil
	}
	for _, id := range completed[:len(completed)-keepN] {
		dir := filepath.Join(b.dir, fmt.Sprintf("ckpt-%d", id))
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("checkpoint: prune ckpt-%d: %w", id, err)
		}
	}
	return nil
}

func (b *FileStateBackend) completedIDs() ([]uint64, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, fmt.Errorf("checkpoint: read dir: %w", err)
	}
	var completed []uint64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "ckpt-") {
			continue
		}
		if _, err := os.Stat(filepath.Join(b.dir, e.Name(), "completed")); errors.Is(err, os.ErrNotExist) {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimPrefix(e.Name(), "ckpt-"), 10, 64)
		if err != nil {
			continue
		}
		completed = append(completed, n)
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i] < completed[j] })
	return completed, nil
}

// Close is a no-op for FileStateBackend.
func (b *FileStateBackend) Close() error { return nil }
```
The `FileStateBackend` here is the same atomic temp-then-rename store built in Exercise 1, bundled so this module stands alone. Its `completedIDs` helper is what makes `LatestCheckpoint` ignore an in-flight checkpoint during recovery.

Create `coordinator.go`:
```go
package checkpoint

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// BarrierSink accepts barriers injected by the Coordinator. Source operators
// implement this interface.
type BarrierSink interface {
	InjectBarrier(Barrier) error
}

// Coordinator triggers periodic checkpoints by injecting barriers into sources
// and finalizes each checkpoint once every operator acknowledges.
type Coordinator struct {
	mu        sync.Mutex
	backend   StateBackend
	sources   []BarrierSink
	operators []string
	nextID    CheckpointID
	pending   map[CheckpointID]map[string]bool // id -> operatorID -> acked
	interval  time.Duration
	keepN     int
}

// CoordinatorOption configures the Coordinator.
type CoordinatorOption func(*Coordinator)

// WithInterval sets the checkpoint trigger interval (default: 30s).
func WithInterval(d time.Duration) CoordinatorOption {
	return func(c *Coordinator) { c.interval = d }
}

// WithKeepN sets the number of completed checkpoints to retain (default: 3).
func WithKeepN(n int) CoordinatorOption {
	return func(c *Coordinator) { c.keepN = n }
}

// NewCoordinator creates a Coordinator. operators is the exhaustive list of
// operator IDs that will call Acknowledge for each checkpoint.
func NewCoordinator(backend StateBackend, sources []BarrierSink, operators []string, opts ...CoordinatorOption) *Coordinator {
	c := &Coordinator{
		backend:   backend,
		sources:   sources,
		operators: operators,
		nextID:    1,
		pending:   make(map[CheckpointID]map[string]bool),
		interval:  30 * time.Second,
		keepN:     3,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// TriggerCheckpoint assigns the next CheckpointID, records which operators must
// acknowledge, and injects a Barrier into all sources.
func (c *Coordinator) TriggerCheckpoint() (CheckpointID, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	acks := make(map[string]bool, len(c.operators))
	for _, op := range c.operators {
		acks[op] = false
	}
	c.pending[id] = acks
	c.mu.Unlock()

	b := Barrier{ID: id, Timestamp: time.Now().UTC()}
	for _, src := range c.sources {
		if err := src.InjectBarrier(b); err != nil {
			return 0, fmt.Errorf("coordinator: inject barrier: %w", err)
		}
	}
	return id, nil
}

// Acknowledge records that operatorID has completed its snapshot for checkpoint
// id. When every operator has acknowledged, the checkpoint is marked complete
// and old checkpoints are pruned.
func (c *Coordinator) Acknowledge(id CheckpointID, operatorID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	acks, ok := c.pending[id]
	if !ok {
		return fmt.Errorf("coordinator: %w: id=%d", ErrCheckpointMismatch, id)
	}
	acks[operatorID] = true
	for _, done := range acks {
		if !done {
			return nil // still waiting for other operators
		}
	}
	// All operators acknowledged: finalize.
	delete(c.pending, id)
	if err := c.backend.MarkComplete(id); err != nil {
		return fmt.Errorf("coordinator: mark complete ckpt-%d: %w", id, err)
	}
	if err := c.backend.Prune(c.keepN); err != nil {
		return fmt.Errorf("coordinator: prune: %w", err)
	}
	return nil
}

// Run triggers checkpoints at the configured interval until ctx is cancelled.
// A cancelled context returns ctx.Err(), which callers treat as normal shutdown.
func (c *Coordinator) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := c.TriggerCheckpoint(); err != nil {
				return err
			}
		}
	}
}
```
### The operator and the recovery path

`CountingOperator` is a minimal stateful operator: it counts records and, when a barrier arrives, snapshots its count as JSON, saves it under the barrier's checkpoint ID, and acknowledges the coordinator — the three steps every operator performs on a barrier. `Snapshot` and `Restore` are inverses: `Restore` *sets* the count to the saved value rather than replaying records, which is precisely what makes recovery exactly-once.

`RestoreLatest` is the recovery entry point. It asks the backend for the latest completed checkpoint, loads that operator's saved state, and restores it, returning the checkpoint ID it recovered from or a wrapped `ErrNoCheckpoint` on a fresh backend. After a crash the engine calls this for every operator, then replays the source records that arrived after the checkpoint; because the restored count already accounts for everything up to the checkpoint, the replayed records add on top rather than being counted twice.

Create `operator.go`:
```go
package checkpoint

import (
	"encoding/json"
	"fmt"
	"sync"
)

// CountingOperator counts records passing through it and checkpoints its count.
// It implements Snapshotable so it can save and restore across checkpoints.
type CountingOperator struct {
	mu      sync.Mutex
	id      string
	count   int64
	backend StateBackend
	coord   *Coordinator
}

// NewCountingOperator constructs a CountingOperator identified by id.
func NewCountingOperator(id string, backend StateBackend, coord *Coordinator) *CountingOperator {
	return &CountingOperator{id: id, backend: backend, coord: coord}
}

// ID returns the operator's unique identifier.
func (op *CountingOperator) ID() string { return op.id }

// Count returns the current record count.
func (op *CountingOperator) Count() int64 {
	op.mu.Lock()
	defer op.mu.Unlock()
	return op.count
}

// Process handles a StreamEvent. A Record increments the counter; a Barrier
// triggers Snapshot, SaveState, and Acknowledge on the coordinator.
func (op *CountingOperator) Process(ev StreamEvent) error {
	if ev.Record != nil {
		op.mu.Lock()
		op.count++
		op.mu.Unlock()
		return nil
	}
	if ev.Barrier != nil {
		return op.doCheckpoint(ev.Barrier.ID)
	}
	return nil
}

func (op *CountingOperator) doCheckpoint(id CheckpointID) error {
	state, err := op.Snapshot()
	if err != nil {
		return fmt.Errorf("operator %s: snapshot: %w", op.id, err)
	}
	if err := op.backend.SaveState(id, op.id, state); err != nil {
		return fmt.Errorf("operator %s: save state: %w", op.id, err)
	}
	if err := op.coord.Acknowledge(id, op.id); err != nil {
		return fmt.Errorf("operator %s: acknowledge: %w", op.id, err)
	}
	return nil
}

type countState struct {
	Count int64 `json:"count"`
}

// Snapshot serializes the operator's count as JSON.
func (op *CountingOperator) Snapshot() ([]byte, error) {
	op.mu.Lock()
	defer op.mu.Unlock()
	return json.Marshal(countState{Count: op.count})
}

// Restore rehydrates the operator's count from a previous Snapshot.
func (op *CountingOperator) Restore(data []byte) error {
	var s countState
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("operator %s: restore: %w", op.id, err)
	}
	op.mu.Lock()
	op.count = s.Count
	op.mu.Unlock()
	return nil
}

// RestoreLatest loads the latest completed checkpoint for operatorID from the
// backend and restores op from it. It returns the checkpoint ID that was
// restored, or a wrapped ErrNoCheckpoint if no completed checkpoint exists.
func RestoreLatest(backend StateBackend, op interface {
	ID() string
	Restore([]byte) error
}) (CheckpointID, error) {
	id, err := backend.LatestCheckpoint()
	if err != nil {
		return 0, err
	}
	data, err := backend.LoadState(id, op.ID())
	if err != nil {
		return 0, err
	}
	if err := op.Restore(data); err != nil {
		return 0, err
	}
	return id, nil
}
```
### The runnable demo

The demo processes ten records, triggers a checkpoint, and runs the barrier through the operator so it snapshots, saves, and acknowledges — which finalizes the checkpoint. It then builds a fresh operator and recovers it from the latest checkpoint, printing the restored count.

Create `cmd/demo/main.go`:
```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/coordinator-recovery"
)

// mockSource is a source operator that accepts injected barriers.
type mockSource struct {
	barriers []checkpoint.Barrier
}

func (s *mockSource) InjectBarrier(b checkpoint.Barrier) error {
	s.barriers = append(s.barriers, b)
	fmt.Printf("source: received barrier %d\n", b.ID)
	return nil
}

func main() {
	dir, err := os.MkdirTemp("", "demo-checkpoint-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	backend, err := checkpoint.NewFileStateBackend(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close()

	if _, err := backend.LatestCheckpoint(); err != nil {
		fmt.Println("backend: no checkpoint yet (expected)")
	}

	src := &mockSource{}
	const operatorID = "counter"
	coord := checkpoint.NewCoordinator(
		backend,
		[]checkpoint.BarrierSink{src},
		[]string{operatorID},
		checkpoint.WithKeepN(3),
	)
	op := checkpoint.NewCountingOperator(operatorID, backend, coord)

	for i := 0; i < 10; i++ {
		_ = op.Process(checkpoint.StreamEvent{Record: &checkpoint.Record{Data: []byte("record")}})
	}
	fmt.Printf("processed %d records\n", op.Count())

	id, err := coord.TriggerCheckpoint()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("coordinator: triggered checkpoint %d\n", id)

	// The barrier flows through the operator: snapshot, save, acknowledge.
	if err := op.Process(checkpoint.StreamEvent{Barrier: &checkpoint.Barrier{ID: id}}); err != nil {
		log.Fatal(err)
	}

	latest, err := backend.LatestCheckpoint()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("backend: checkpoint %d is complete\n", latest)

	// Recovery: rebuild a fresh operator from the latest completed checkpoint.
	recovered := checkpoint.NewCountingOperator(operatorID, backend, coord)
	restoredID, err := checkpoint.RestoreLatest(backend, recovered)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("recovered from checkpoint %d: count = %d\n", restoredID, recovered.Count())
}
```
Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
backend: no checkpoint yet (expected)
processed 10 records
source: received barrier 1
coordinator: triggered checkpoint 1
backend: checkpoint 1 is complete
recovered from checkpoint 1: count = 10
```

### Tests

`TestCoordinatorTriggerAndAcknowledge` checks a one-operator checkpoint finalizes and becomes the latest. `TestCoordinatorWaitsForAllOperators` proves a two-operator checkpoint is not complete after one acknowledgement and is after the second. `TestCoordinatorAckUnknownCheckpoint` covers the `ErrCheckpointMismatch` path. `TestCountingOperatorSnapshotRestore` round-trips state at three sizes. `TestProcessBarrierCheckpoints` runs the full operator path. `TestFullRecoveryExactlyOnce` is the headline: it checkpoints at count N, rebuilds from the checkpoint, replays M records, and asserts the count is N+M, never 2N+M.

Create `checkpoint_test.go`:
```go
package checkpoint

import (
	"errors"
	"sync"
	"testing"
)

type mockSource struct {
	mu       sync.Mutex
	barriers []Barrier
}

func (m *mockSource) InjectBarrier(b Barrier) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.barriers = append(m.barriers, b)
	return nil
}

func (m *mockSource) lastBarrier() (Barrier, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.barriers) == 0 {
		return Barrier{}, false
	}
	return m.barriers[len(m.barriers)-1], true
}

func newBackend(t *testing.T) *FileStateBackend {
	t.Helper()
	b, err := NewFileStateBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func TestCoordinatorTriggerAndAcknowledge(t *testing.T) {
	t.Parallel()
	backend := newBackend(t)
	src := &mockSource{}
	coord := NewCoordinator(backend, []BarrierSink{src}, []string{"counter"}, WithKeepN(3))

	id, err := coord.TriggerCheckpoint()
	if err != nil {
		t.Fatalf("TriggerCheckpoint: %v", err)
	}
	if id != 1 {
		t.Errorf("first checkpoint ID = %d, want 1", id)
	}
	if b, ok := src.lastBarrier(); !ok || b.ID != 1 {
		t.Errorf("source last barrier = %+v, want ID=1", b)
	}

	if err := coord.Acknowledge(1, "counter"); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	latest, err := backend.LatestCheckpoint()
	if err != nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	if latest != 1 {
		t.Errorf("LatestCheckpoint = %d, want 1", latest)
	}
}

func TestCoordinatorWaitsForAllOperators(t *testing.T) {
	t.Parallel()
	backend := newBackend(t)
	src := &mockSource{}
	coord := NewCoordinator(backend, []BarrierSink{src}, []string{"a", "b"})

	id, err := coord.TriggerCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := coord.Acknowledge(id, "a"); err != nil {
		t.Fatal(err)
	}
	// Only one of two operators acked: checkpoint must not be complete.
	if _, err := backend.LatestCheckpoint(); !errors.Is(err, ErrNoCheckpoint) {
		t.Fatalf("after one ack: err = %v, want ErrNoCheckpoint", err)
	}
	if err := coord.Acknowledge(id, "b"); err != nil {
		t.Fatal(err)
	}
	if latest, err := backend.LatestCheckpoint(); err != nil || latest != id {
		t.Fatalf("after both acks: latest=%d err=%v, want %d nil", latest, err, id)
	}
}

func TestCoordinatorAckUnknownCheckpoint(t *testing.T) {
	t.Parallel()
	backend := newBackend(t)
	coord := NewCoordinator(backend, []BarrierSink{&mockSource{}}, []string{"op"})
	if err := coord.Acknowledge(999, "op"); !errors.Is(err, ErrCheckpointMismatch) {
		t.Errorf("Ack unknown: err = %v, want ErrCheckpointMismatch", err)
	}
}

func TestCountingOperatorSnapshotRestore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		initial int64
	}{
		{"zero", 0},
		{"nonzero", 42},
		{"large", 1_000_000},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := newBackend(t)
			coord := NewCoordinator(backend, []BarrierSink{&mockSource{}}, []string{"op"})
			op := NewCountingOperator("op", backend, coord)
			for i := int64(0); i < tc.initial; i++ {
				_ = op.Process(StreamEvent{Record: &Record{Data: []byte("x")}})
			}
			state, err := op.Snapshot()
			if err != nil {
				t.Fatalf("Snapshot: %v", err)
			}
			op2 := NewCountingOperator("op2", backend, coord)
			if err := op2.Restore(state); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if op2.Count() != tc.initial {
				t.Errorf("after Restore: count = %d, want %d", op2.Count(), tc.initial)
			}
		})
	}
}

func TestProcessBarrierCheckpoints(t *testing.T) {
	t.Parallel()
	backend := newBackend(t)
	coord := NewCoordinator(backend, []BarrierSink{&mockSource{}}, []string{"counter"})
	op := NewCountingOperator("counter", backend, coord)

	for i := 0; i < 5; i++ {
		if err := op.Process(StreamEvent{Record: &Record{Data: []byte("r")}}); err != nil {
			t.Fatalf("Process record %d: %v", i, err)
		}
	}
	id, err := coord.TriggerCheckpoint()
	if err != nil {
		t.Fatalf("TriggerCheckpoint: %v", err)
	}
	if err := op.Process(StreamEvent{Barrier: &Barrier{ID: id}}); err != nil {
		t.Fatalf("Process barrier: %v", err)
	}
	if latest, err := backend.LatestCheckpoint(); err != nil || latest != id {
		t.Fatalf("latest=%d err=%v, want %d nil", latest, err, id)
	}
}

// TestFullRecoveryExactlyOnce models a crash-restart cycle. After a checkpoint
// at count N, a fresh operator restores from that checkpoint and processes M
// more records (the records replayed from the source after the crash). The
// recovered count must be N+M, never 2*N+M: restoring sets the count to the
// snapshot value instead of re-counting the pre-checkpoint records.
func TestFullRecoveryExactlyOnce(t *testing.T) {
	t.Parallel()
	const N, M = 7, 4
	backend := newBackend(t)
	coord := NewCoordinator(backend, []BarrierSink{&mockSource{}}, []string{"counter"})

	op := NewCountingOperator("counter", backend, coord)
	for i := 0; i < N; i++ {
		_ = op.Process(StreamEvent{Record: &Record{Data: []byte("r")}})
	}
	id, err := coord.TriggerCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := op.Process(StreamEvent{Barrier: &Barrier{ID: id}}); err != nil {
		t.Fatal(err)
	}

	// Crash: the operator state is lost. Rebuild from the latest checkpoint.
	recovered := NewCountingOperator("counter", backend, coord)
	restoredID, err := RestoreLatest(backend, recovered)
	if err != nil {
		t.Fatalf("RestoreLatest: %v", err)
	}
	if restoredID != id {
		t.Errorf("restored from checkpoint %d, want %d", restoredID, id)
	}
	if recovered.Count() != N {
		t.Fatalf("after restore: count = %d, want %d", recovered.Count(), N)
	}

	// Replay M records that arrived after the checkpoint.
	for i := 0; i < M; i++ {
		_ = recovered.Process(StreamEvent{Record: &Record{Data: []byte("r")}})
	}
	if got, want := recovered.Count(), int64(N+M); got != want {
		t.Errorf("recovered count = %d, want %d (no double-counting)", got, want)
	}
}

func TestRestoreLatestNoCheckpoint(t *testing.T) {
	t.Parallel()
	backend := newBackend(t)
	coord := NewCoordinator(backend, []BarrierSink{&mockSource{}}, []string{"op"})
	op := NewCountingOperator("op", backend, coord)
	if _, err := RestoreLatest(backend, op); !errors.Is(err, ErrNoCheckpoint) {
		t.Errorf("RestoreLatest on empty backend: err = %v, want ErrNoCheckpoint", err)
	}
}
```
## Review

The coordinator is correct when a checkpoint becomes durable exactly when the last operator acknowledges — not before, not never. The most common error is calling `MarkComplete` on the first acknowledgement instead of scanning the whole set; recovery from such a checkpoint mixes pre- and post-snapshot operator states. The second is recovering by replaying the pre-checkpoint records instead of loading the snapshot value: a counter restored at 7 that then sees 4 replayed records must end at 11, and `TestFullRecoveryExactlyOnce` fails loudly at 18 if `Restore` re-counts. The third is mutating coordinator state outside its lock; every method takes `c.mu`, and `go test -race` confirms there is no unsynchronised access between `TriggerCheckpoint` and a near-instant `Acknowledge`.

## Resources

- [Chandy and Lamport, "Distributed Snapshots: Determining Global States of Distributed Systems" (1985)](https://lamport.azurewebsites.net/pubs/chandy.pdf) — the original algorithm the coordinator implements.
- [Apache Flink: checkpointing](https://nightlies.apache.org/flink/flink-docs-stable/docs/dev/datastream/fault-tolerance/checkpointing/) — the coordinator, acknowledgements, and recovery in a production engine.
- [context.Context](https://pkg.go.dev/context#Context) — the cancellation used by `Run` for clean shutdown.

---

Back to [02-barrier-alignment.md](02-barrier-alignment.md) | Next: [04-durable-fsync-writes.md](04-durable-fsync-writes.md)
