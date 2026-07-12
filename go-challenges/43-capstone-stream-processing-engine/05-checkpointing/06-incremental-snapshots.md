# Exercise 6: Incremental Snapshots

Operator state can grow to gigabytes, and rewriting all of it on every checkpoint when only a handful of keys changed is pure waste. An incremental checkpoint persists only the keys that changed since the previous one and writes a full snapshot only periodically; recovery rebuilds state by loading the latest full snapshot and replaying the deltas forward. This module builds that scheme over a keyed counter with a crash-restore round-trip.

This module is fully self-contained, with its own `go mod init`, demo, and tests.

## What you'll build

```text
incremental.go         CheckpointID, ChunkStore, KeyedCounter, Checkpoint, RestoreAt
cmd/
  demo/
    main.go            a full snapshot, two deltas, recover by replay
incremental_test.go    cadence, delta carries only changed keys, restore round-trip
```

- Files: `incremental.go`, `cmd/demo/main.go`, `incremental_test.go`.
- Implement: `KeyedCounter` with `Add`, `Get`, and `Checkpoint` (full or delta), plus `RestoreAt` which rebuilds state as of any checkpoint by replaying from the latest full snapshot.
- Test: `incremental_test.go` proves the full-snapshot cadence, that a delta carries only changed keys, a crash-restore round-trip, historical recovery at an earlier checkpoint, and the no-snapshot error.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Deltas, full snapshots, and replay

Each checkpoint persists one `chunk`. A full chunk holds the entire keyed state; a delta chunk holds only the keys whose value changed since the previous checkpoint. The operator tracks a `dirty` set — every key touched by `Add` since the last checkpoint — and a `sinceFull` counter that decides, on each checkpoint, whether a full snapshot is due. The first checkpoint is always full (there is nothing to be incremental against), and thereafter a full snapshot is forced every `fullEvery` checkpoints; the rest are deltas of the dirty set, after which `dirty` is cleared.

Recovery is the interesting half. `RestoreAt(target)` cannot simply load the chunk at `target`, because that chunk is usually a delta holding only a few keys. Instead it scans for the most recent full snapshot at or before `target`, loads that as the base state, then replays every chunk from there up to `target` in increasing ID order, applying each delta by *setting* the keys it carries. Because a delta records the absolute value of a changed key (not an increment), a later delta naturally overrides an earlier one, and the replayed result is exactly the state the operator had at `target`. If no full snapshot exists at or before the target, recovery is impossible and `RestoreAt` returns a wrapped `ErrNoCheckpoint`.

The periodic full snapshot is what bounds recovery cost. With `fullEvery = N`, recovery never replays more than N-1 deltas, and a single corrupt early delta can only cost the deltas since the last full snapshot rather than the entire history of the job. Choosing `fullEvery` trades write amplification (full snapshots are large) against recovery latency (more deltas to replay) — the same dial Flink's incremental RocksDB checkpoints expose.

Create `incremental.go`:
```go
// Package incremental implements incremental (delta) checkpoints for a keyed
// state operator. Most checkpoints persist only the keys that changed since the
// previous checkpoint; a periodic full snapshot bounds how many deltas a
// recovery must replay.
package incremental

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ErrNoCheckpoint is returned when a recovery target has no full snapshot to
// rebuild from.
var ErrNoCheckpoint = errors.New("no full snapshot at or before target")

// CheckpointID is a monotonically increasing checkpoint identifier.
type CheckpointID uint64

// chunk is what a single checkpoint persists. A full chunk holds the entire
// keyed state; a delta chunk holds only the keys whose value changed since the
// previous checkpoint.
type chunk struct {
	Full    bool             `json:"full"`
	Entries map[string]int64 `json:"entries"`
}

// ChunkStore persists one chunk per checkpoint as a JSON file written atomically.
type ChunkStore struct {
	dir string
}

// NewChunkStore roots a store at dir.
func NewChunkStore(dir string) (*ChunkStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("incremental: create dir: %w", err)
	}
	return &ChunkStore{dir: dir}, nil
}

func (s *ChunkStore) path(id CheckpointID) string {
	return filepath.Join(s.dir, fmt.Sprintf("ckpt-%d.json", uint64(id)))
}

func (s *ChunkStore) put(id CheckpointID, c chunk) error {
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("incremental: marshal ckpt-%d: %w", id, err)
	}
	tmp, err := os.CreateTemp(s.dir, fmt.Sprintf(".tmp-%d-*", uint64(id)))
	if err != nil {
		return fmt.Errorf("incremental: temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("incremental: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("incremental: close: %w", err)
	}
	if err := os.Rename(tmpName, s.path(id)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("incremental: rename: %w", err)
	}
	return nil
}

func (s *ChunkStore) get(id CheckpointID) (chunk, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return chunk{}, fmt.Errorf("incremental: read ckpt-%d: %w", id, err)
	}
	var c chunk
	if err := json.Unmarshal(data, &c); err != nil {
		return chunk{}, fmt.Errorf("incremental: unmarshal ckpt-%d: %w", id, err)
	}
	return c, nil
}

// completedIDs returns the sorted IDs of every persisted chunk.
func (s *ChunkStore) completedIDs() ([]CheckpointID, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("incremental: read dir: %w", err)
	}
	var ids []CheckpointID
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "ckpt-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(name, "ckpt-"), ".json")
		n, err := strconv.ParseUint(mid, 10, 64)
		if err != nil {
			continue
		}
		ids = append(ids, CheckpointID(n))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

// KeyedCounter is a keyed-state operator: a map from key to a running count.
// It tracks which keys changed since the last checkpoint so it can persist a
// delta, and forces a full snapshot every fullEvery checkpoints.
type KeyedCounter struct {
	state     map[string]int64
	dirty     map[string]struct{}
	fullEvery int
	sinceFull int // checkpoints written since the last full snapshot
}

// NewKeyedCounter creates a counter that writes a full snapshot on its first
// checkpoint and then every fullEvery checkpoints (fullEvery >= 1).
func NewKeyedCounter(fullEvery int) *KeyedCounter {
	if fullEvery < 1 {
		fullEvery = 1
	}
	return &KeyedCounter{
		state:     make(map[string]int64),
		dirty:     make(map[string]struct{}),
		fullEvery: fullEvery,
	}
}

// Add increments key by delta and marks it dirty for the next checkpoint.
func (k *KeyedCounter) Add(key string, delta int64) {
	k.state[key] += delta
	k.dirty[key] = struct{}{}
}

// Get returns the current value of key.
func (k *KeyedCounter) Get(key string) int64 { return k.state[key] }

// Checkpoint persists the operator's state at id. It writes a full snapshot
// when one is due (first checkpoint, or fullEvery reached), otherwise a delta
// containing only the keys changed since the previous checkpoint. It returns
// true if the chunk written was a full snapshot.
func (k *KeyedCounter) Checkpoint(store *ChunkStore, id CheckpointID) (bool, error) {
	full := k.sinceFull == 0 || k.sinceFull >= k.fullEvery

	entries := make(map[string]int64)
	if full {
		for key, v := range k.state {
			entries[key] = v
		}
	} else {
		for key := range k.dirty {
			entries[key] = k.state[key]
		}
	}
	if err := store.put(id, chunk{Full: full, Entries: entries}); err != nil {
		return false, err
	}

	k.dirty = make(map[string]struct{})
	if full {
		k.sinceFull = 1
	} else {
		k.sinceFull++
	}
	return full, nil
}

// RestoreAt rebuilds a KeyedCounter as of checkpoint target. It walks back to
// the most recent full snapshot at or before target, loads it as the base, then
// replays each delta forward up to target. A delta sets the absolute value of
// every key it carries, so a later delta overrides an earlier one.
func RestoreAt(store *ChunkStore, target CheckpointID, fullEvery int) (*KeyedCounter, error) {
	ids, err := store.completedIDs()
	if err != nil {
		return nil, err
	}

	// Find the latest full snapshot at or before target.
	base := CheckpointID(0)
	found := false
	for _, id := range ids {
		if id > target {
			break
		}
		c, err := store.get(id)
		if err != nil {
			return nil, err
		}
		if c.Full {
			base, found = id, true
		}
	}
	if !found {
		return nil, fmt.Errorf("incremental: %w: target=%d", ErrNoCheckpoint, target)
	}

	k := NewKeyedCounter(fullEvery)
	for _, id := range ids {
		if id < base || id > target {
			continue
		}
		c, err := store.get(id)
		if err != nil {
			return nil, err
		}
		for key, v := range c.Entries {
			k.state[key] = v
		}
	}
	return k, nil
}
```
### The runnable demo

With `fullEvery = 3`, the demo writes a full snapshot at checkpoint 1, then deltas at 2 and 3 (only the changed keys each time), then recovers the operator as of checkpoint 3 by replaying the full snapshot followed by the two deltas.

Create `cmd/demo/main.go`:
```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/incremental-snapshots"
)

func main() {
	dir, err := os.MkdirTemp("", "demo-incremental-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := incremental.NewChunkStore(dir)
	if err != nil {
		log.Fatal(err)
	}

	const fullEvery = 3
	k := incremental.NewKeyedCounter(fullEvery)

	// Checkpoint 1: first checkpoint is always a full snapshot.
	k.Add("a", 1)
	k.Add("b", 1)
	report(k, store, 1)

	// Checkpoint 2: only "a" changed -> delta of one key.
	k.Add("a", 1)
	report(k, store, 2)

	// Checkpoint 3: only "c" changed -> delta of one key.
	k.Add("c", 5)
	report(k, store, 3)

	// Recover the operator as of the latest checkpoint by replaying the full
	// snapshot at checkpoint 1 followed by the deltas at 2 and 3.
	restored, err := incremental.RestoreAt(store, 3, fullEvery)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("restored at 3: a=%d b=%d c=%d\n",
		restored.Get("a"), restored.Get("b"), restored.Get("c"))
}

func report(k *incremental.KeyedCounter, store *incremental.ChunkStore, id incremental.CheckpointID) {
	full, err := k.Checkpoint(store, id)
	if err != nil {
		log.Fatal(err)
	}
	kind := "delta"
	if full {
		kind = "full snapshot"
	}
	fmt.Printf("checkpoint %d: %s\n", id, kind)
}
```
Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
checkpoint 1: full snapshot
checkpoint 2: delta
checkpoint 3: delta
restored at 3: a=2 b=1 c=5
```

### Tests

`TestFullSnapshotCadence` pins which checkpoints are full for `fullEvery=3` (1 and 4). `TestDeltaCarriesOnlyChangedKeys` reads the persisted chunks and proves the delta after a full snapshot carries exactly the one key that changed. `TestRestoreLatestRoundTrip` simulates a restart: it rebuilds a brand-new counter from disk and asserts it equals the live state across full and delta boundaries. `TestRestoreHistorical` recovers at an earlier checkpoint and sees the historical value, not the latest. `TestRestoreNoSnapshot` covers the `ErrNoCheckpoint` path.

Create `incremental_test.go`:
```go
package incremental

import (
	"errors"
	"testing"
)

func newStore(t *testing.T) *ChunkStore {
	t.Helper()
	s, err := NewChunkStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestFirstCheckpointIsFull and the delta cadence: with fullEvery=3 the full
// snapshots land at checkpoints 1 and 4, deltas in between.
func TestFullSnapshotCadence(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	k := NewKeyedCounter(3)

	wantFull := map[CheckpointID]bool{1: true, 2: false, 3: false, 4: true, 5: false}
	for id := CheckpointID(1); id <= 5; id++ {
		k.Add("a", 1)
		full, err := k.Checkpoint(s, id)
		if err != nil {
			t.Fatalf("Checkpoint(%d): %v", id, err)
		}
		if full != wantFull[id] {
			t.Errorf("checkpoint %d: full = %v, want %v", id, full, wantFull[id])
		}
	}
}

// TestDeltaCarriesOnlyChangedKeys verifies a delta persists exactly the keys
// touched since the previous checkpoint, not the whole state.
func TestDeltaCarriesOnlyChangedKeys(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	k := NewKeyedCounter(100) // never auto-full after the first

	k.Add("a", 1)
	k.Add("b", 1)
	if _, err := k.Checkpoint(s, 1); err != nil { // full: a,b
		t.Fatal(err)
	}
	k.Add("a", 1)                                 // only a changes
	if _, err := k.Checkpoint(s, 2); err != nil { // delta: a
		t.Fatal(err)
	}

	full, err := s.get(1)
	if err != nil {
		t.Fatal(err)
	}
	if !full.Full || len(full.Entries) != 2 {
		t.Errorf("chunk 1 = %+v, want full with 2 entries", full)
	}
	delta, err := s.get(2)
	if err != nil {
		t.Fatal(err)
	}
	if delta.Full || len(delta.Entries) != 1 || delta.Entries["a"] != 2 {
		t.Errorf("chunk 2 = %+v, want delta {a:2}", delta)
	}
}

// TestRestoreLatestRoundTrip simulates a restart: a brand-new counter is rebuilt
// from disk and must equal the live operator's state.
func TestRestoreLatestRoundTrip(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	k := NewKeyedCounter(2)

	k.Add("x", 10)
	k.Add("y", 1)
	mustCheckpoint(t, k, s, 1) // full
	k.Add("x", 5)
	mustCheckpoint(t, k, s, 2) // delta x=15
	k.Add("z", 7)
	mustCheckpoint(t, k, s, 3) // full (fullEvery=2 reached)
	k.Add("y", 1)
	mustCheckpoint(t, k, s, 4) // delta y=2

	restored, err := RestoreAt(s, 4, 2)
	if err != nil {
		t.Fatalf("RestoreAt: %v", err)
	}
	for key, want := range map[string]int64{"x": 15, "y": 2, "z": 7} {
		if got := restored.Get(key); got != want {
			t.Errorf("restored %q = %d, want %d", key, got, want)
		}
	}
}

// TestRestoreHistorical rebuilds state as of an earlier checkpoint and must see
// the historical value, not the latest.
func TestRestoreHistorical(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	k := NewKeyedCounter(100)

	k.Add("n", 1)
	mustCheckpoint(t, k, s, 1)
	k.Add("n", 1)
	mustCheckpoint(t, k, s, 2)
	k.Add("n", 1)
	mustCheckpoint(t, k, s, 3)

	at2, err := RestoreAt(s, 2, 100)
	if err != nil {
		t.Fatalf("RestoreAt(2): %v", err)
	}
	if got := at2.Get("n"); got != 2 {
		t.Errorf("restored at 2: n = %d, want 2", got)
	}
	at3, err := RestoreAt(s, 3, 100)
	if err != nil {
		t.Fatalf("RestoreAt(3): %v", err)
	}
	if got := at3.Get("n"); got != 3 {
		t.Errorf("restored at 3: n = %d, want 3", got)
	}
}

func TestRestoreNoSnapshot(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	if _, err := RestoreAt(s, 0, 1); !errors.Is(err, ErrNoCheckpoint) {
		t.Errorf("RestoreAt empty: err = %v, want ErrNoCheckpoint", err)
	}
}

func mustCheckpoint(t *testing.T, k *KeyedCounter, s *ChunkStore, id CheckpointID) {
	t.Helper()
	if _, err := k.Checkpoint(s, id); err != nil {
		t.Fatalf("Checkpoint(%d): %v", id, err)
	}
}
```
## Review

Incremental checkpointing is correct when recovery at any target reconstructs exactly the state the operator had at that checkpoint. The most common error is omitting the periodic full snapshot, which forces recovery to replay every delta since the job began and makes a single missing early delta unrecoverable. The second is recording increments in a delta instead of absolute values: replaying then double-applies a key that changed in two deltas, so the delta must store the current value and recovery must *set* rather than add. The third is forgetting to clear the dirty set after a checkpoint, which makes every subsequent delta carry stale keys and quietly defeats the whole point of being incremental. The round-trip and historical tests, run under `go test -race`, confirm replay rebuilds the correct state from disk.

## Resources

- [Apache Flink: incremental checkpoints](https://flink.apache.org/2018/01/30/managing-large-state-in-apache-flink-an-intro-to-incremental-checkpointing/) — the production design this module mirrors: full snapshots plus deltas.
- [RocksDB checkpoints](https://github.com/facebook/rocksdb/wiki/Checkpoints) — the immutable-file mechanism Flink's incremental state backend builds on.
- [pkg.go.dev/encoding/json](https://pkg.go.dev/encoding/json) — the serialization used for each chunk.

---

Back to [05-pluggable-state-backends.md](05-pluggable-state-backends.md) | Next: [../06-parallel-execution/00-concepts.md](../06-parallel-execution/00-concepts.md)
