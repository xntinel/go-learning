# Exercise 4: Idempotent Upsert Sink

When the destination is a key-value store, exactly-once delivery stops requiring two-phase commit and becomes a property of the *write* instead. A keyed upsert is idempotent: re-applying it leaves the same state. This exercise builds an upsert sink with a version guard, so at-least-once delivery — duplicates and reordering and all — produces exactly-once state, and a flush-on-checkpoint snapshot materializes that deduplicated state to disk.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sink.go                Record (with Version), UpsertSink, Write, Get, Metrics
snapshot.go            Snapshot: atomic, sorted, deduplicated materialized view
cmd/
  demo/
    main.go            replay duplicates and a stale write, snapshot the result
sink_test.go           idempotency, last-write-wins, stale rejection, concurrency
```

- Files: `sink.go`, `snapshot.go`, `cmd/demo/main.go`, `sink_test.go`.
- Implement: `UpsertSink` with `Write`, `Get`, `Len`, and `Snapshot`, plus the version-carrying `Record` and the classification `Metrics`.
- Test: `sink_test.go` proves applying a batch twice is idempotent, that a higher version wins and a lower one is rejected, that concurrent duplicate delivery yields exactly-once state, and that the snapshot is deduplicated.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p idempotent-upsert-sink/cmd/demo && cd idempotent-upsert-sink
go mod init example.com/idempotent-upsert-sink
go mod edit -go=1.26
```

### Why a version guard, not just an upsert

A plain upsert — store `value` at `key`, overwriting whatever is there — is already idempotent against *duplicates*: delivering the same record twice writes the same value twice and leaves one row. That alone converts at-least-once delivery into exactly-once state for the simple case. But at-least-once delivery does not only duplicate; it also *reorders*. A retry of an early message can arrive after a later message for the same key has already landed. A blind upsert would let that stale, late re-delivery clobber the fresh value, silently rolling a key backward.

The version guard fixes this. Every record carries a monotonically increasing per-key `Version`, and `Write` installs an incoming record only when its version is greater than or equal to the stored one. The three cases are explicit in the code and each bumps a different metric: a strictly higher version (or an absent key) is `Applied`; an equal version is a `Duplicate` and changes nothing; a lower version is `Stale` and is rejected. This is last-writer-wins-by-version, and it is what makes the sink correct under the two failure modes at-least-once actually produces — not just duplication, but reordering too.

The store copies the value bytes on the way in (`Get` copies them on the way out), so a caller that reuses its record buffer cannot mutate a stored value, and a caller that mutates a returned slice cannot corrupt the store. The whole thing is guarded by one mutex, which is what lets the concurrency test hammer it from many goroutines and still get deterministic state.

Create `sink.go`:

```go
// Package sink provides an idempotent upsert connector for a stream processing
// pipeline. It converts at-least-once delivery into exactly-once *state* by
// applying keyed, version-guarded upserts whose re-application is a no-op.
package sink

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrEmptyKey is returned when a record carries no key. An upsert sink keys
// every write by Record.Key, so an empty key has no destination row.
var ErrEmptyKey = errors.New("sink: record key must not be empty")

// Record is one keyed update. Key identifies the destination row (the primary
// key). Version is a monotonically increasing per-key revision number used to
// resolve duplicates and reordering: a higher version wins, an equal version is
// a duplicate, a lower version is stale.
type Record struct {
	Key     []byte
	Value   []byte
	Version uint64
}

// Metrics counts how each delivered record was classified.
type Metrics struct {
	Applied    atomic.Int64 // installed a new or newer version
	Duplicates atomic.Int64 // exact re-delivery of the version already stored
	Stale      atomic.Int64 // an older version than the one already stored
	Snapshots  atomic.Int64 // materialized-view snapshots written
}

// entry is the stored value plus the version that produced it.
type entry struct {
	value   []byte
	version uint64
}

// UpsertSink is an in-memory key-value sink with last-writer-wins-by-version
// semantics. Re-delivering a record is harmless: the same (key, version) pair
// leaves the store unchanged, so the sink is idempotent under at-least-once
// delivery and tolerant of reordering.
type UpsertSink struct {
	mu      sync.Mutex
	store   map[string]entry
	metrics Metrics
}

// NewUpsertSink constructs an empty UpsertSink.
func NewUpsertSink() *UpsertSink {
	return &UpsertSink{store: make(map[string]entry)}
}

// Metrics returns the live classification counters.
func (s *UpsertSink) Metrics() *Metrics { return &s.metrics }

// Write applies each record as a version-guarded upsert. It is safe to call
// concurrently and safe to call with records that were already delivered.
func (s *UpsertSink) Write(records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		if len(r.Key) == 0 {
			return ErrEmptyKey
		}
		k := string(r.Key)
		cur, ok := s.store[k]
		switch {
		case !ok || r.Version > cur.version:
			// New row, or a strictly newer version: install it.
			v := make([]byte, len(r.Value))
			copy(v, r.Value)
			s.store[k] = entry{value: v, version: r.Version}
			s.metrics.Applied.Add(1)
		case r.Version == cur.version:
			// Exact re-delivery: the store already reflects this write.
			s.metrics.Duplicates.Add(1)
		default:
			// r.Version < cur.version: a stale, out-of-order re-delivery.
			s.metrics.Stale.Add(1)
		}
	}
	return nil
}

// Get returns the current value for key and whether it is present.
func (s *UpsertSink) Get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store[key]
	if !ok {
		return nil, false
	}
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, true
}

// Len returns the number of distinct keys currently stored.
func (s *UpsertSink) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.store)
}
```

### Flush-on-checkpoint: the snapshot as exactly-once state

Idempotent writes give exactly-once state in memory, but a stream engine needs that state to survive a restart. `Snapshot` is the flush-on-checkpoint operation: it writes the current deduplicated store to a file as newline-delimited JSON, sorted by key for determinism. Because the store already holds exactly one entry per key, the snapshot has exactly one row per key no matter how many duplicate deliveries produced it.

`Snapshot` writes to a `.tmp` file, fsyncs it, and atomically renames it into place — the same stage-and-promote pattern as the file sink's commit. A reader of the snapshot path therefore never observes a half-written materialized view: it sees the previous complete snapshot or the new complete one. That atomicity is what lets the snapshot double as a recovery point — on restart the engine can load the last snapshot and resume, confident it is a consistent view of the deduplicated state.

Create `snapshot.go`:

```go
package sink

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// snapshotRow is the on-disk form of one materialized-view entry.
type snapshotRow struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version uint64 `json:"version"`
}

// Snapshot writes the deduplicated materialized view to path as newline-
// delimited JSON, sorted by key for determinism. It writes to a temp file and
// renames it into place so a reader never observes a half-written snapshot.
// This is the flush-on-checkpoint operation: the snapshot is the exactly-once
// state that survives a restart.
func (s *UpsertSink) Snapshot(path string) error {
	s.mu.Lock()
	rows := make([]snapshotRow, 0, len(s.store))
	for k, e := range s.store {
		rows = append(rows, snapshotRow{Key: k, Value: string(e.value), Version: e.version})
	}
	s.mu.Unlock()

	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("snapshot: create: %w", err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("snapshot: encode: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("snapshot: flush: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("snapshot: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("snapshot: close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("snapshot: rename: %w", err)
	}
	s.metrics.Snapshots.Add(1)
	return nil
}
```

### The runnable demo

The demo plays a deliberately messy at-least-once delivery stream: two keys, with one duplicate, one update to a higher version, one stale re-delivery of the old version, and another duplicate. Six deliveries collapse to two distinct keys, the snapshot has two rows, and `user:1` ends at its updated value despite the stale re-delivery arriving afterward.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"example.com/idempotent-upsert-sink"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run() error {
	s := sink.NewUpsertSink()

	// An at-least-once source re-delivers some records and reorders others.
	deliveries := []sink.Record{
		{Key: []byte("user:1"), Value: []byte("alice"), Version: 1},
		{Key: []byte("user:2"), Value: []byte("bob"), Version: 1},
		{Key: []byte("user:1"), Value: []byte("alice"), Version: 1},  // duplicate
		{Key: []byte("user:1"), Value: []byte("alice2"), Version: 2}, // update
		{Key: []byte("user:1"), Value: []byte("alice"), Version: 1},  // stale re-delivery
		{Key: []byte("user:2"), Value: []byte("bob"), Version: 1},    // duplicate
	}
	if err := s.Write(deliveries); err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "upsert-demo")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "view.ndjson")
	if err := s.Snapshot(path); err != nil {
		return err
	}
	data, _ := os.ReadFile(path)

	m := s.Metrics()
	fmt.Printf("delivered=%d distinct keys=%d snapshot rows=%d\n",
		len(deliveries), s.Len(), bytes.Count(data, []byte("\n")))
	fmt.Printf("applied=%d duplicates=%d stale=%d\n",
		m.Applied.Load(), m.Duplicates.Load(), m.Stale.Load())
	v1, _ := s.Get("user:1")
	fmt.Printf("user:1 = %s\n", v1)
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered=6 distinct keys=2 snapshot rows=2
applied=3 duplicates=2 stale=1
user:1 = alice2
```

### Tests

`TestUpsertIsIdempotent` is the core property: applying the same batch twice leaves the store identical and classifies the second pass as duplicates, not new writes. `TestUpsertLastWriteWins` and `TestUpsertRejectsStaleVersion` cover the two version-ordering directions. `TestConcurrentDuplicateDeliveryIsExactlyOnce` is the exactly-once proof: eight goroutines each deliver the same fifty-record batch — as if every partition retried every record — and the test asserts the final state equals a single application, with `Applied` equal to the distinct key count. `TestSnapshotDeduplicates` confirms the materialized view has one row per key regardless of duplicate deliveries.

Create `sink_test.go`:

```go
package sink

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestUpsertIsIdempotent is the core property: applying the same batch twice
// leaves the store identical and classifies the second application as
// duplicates, not new writes. This is what makes at-least-once delivery safe.
func TestUpsertIsIdempotent(t *testing.T) {
	t.Parallel()

	s := NewUpsertSink()
	batch := []Record{
		{Key: []byte("a"), Value: []byte("1"), Version: 1},
		{Key: []byte("b"), Value: []byte("2"), Version: 1},
	}
	if err := s.Write(batch); err != nil {
		t.Fatal(err)
	}
	// Re-deliver the identical batch (a duplicate at-least-once delivery).
	if err := s.Write(batch); err != nil {
		t.Fatal(err)
	}

	if got := s.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	if got := s.metrics.Applied.Load(); got != 2 {
		t.Fatalf("Applied = %d, want 2", got)
	}
	if got := s.metrics.Duplicates.Load(); got != 2 {
		t.Fatalf("Duplicates = %d, want 2", got)
	}
	if v, _ := s.Get("a"); string(v) != "1" {
		t.Fatalf("a = %q, want 1", v)
	}
}

// TestUpsertLastWriteWins verifies a strictly newer version replaces the value.
func TestUpsertLastWriteWins(t *testing.T) {
	t.Parallel()

	s := NewUpsertSink()
	_ = s.Write([]Record{{Key: []byte("a"), Value: []byte("old"), Version: 1}})
	_ = s.Write([]Record{{Key: []byte("a"), Value: []byte("new"), Version: 2}})

	if v, _ := s.Get("a"); string(v) != "new" {
		t.Fatalf("a = %q, want new", v)
	}
	if got := s.metrics.Applied.Load(); got != 2 {
		t.Fatalf("Applied = %d, want 2", got)
	}
}

// TestUpsertRejectsStaleVersion verifies an out-of-order re-delivery of an
// older version does not clobber a newer stored value.
func TestUpsertRejectsStaleVersion(t *testing.T) {
	t.Parallel()

	s := NewUpsertSink()
	_ = s.Write([]Record{{Key: []byte("a"), Value: []byte("v5"), Version: 5}})
	// A delayed duplicate of an older version arrives late.
	_ = s.Write([]Record{{Key: []byte("a"), Value: []byte("v3"), Version: 3}})

	if v, _ := s.Get("a"); string(v) != "v5" {
		t.Fatalf("a = %q, want v5 (stale write must not win)", v)
	}
	if got := s.metrics.Stale.Load(); got != 1 {
		t.Fatalf("Stale = %d, want 1", got)
	}
}

func TestUpsertRejectsEmptyKey(t *testing.T) {
	t.Parallel()

	s := NewUpsertSink()
	err := s.Write([]Record{{Value: []byte("x"), Version: 1}})
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("err = %v, want ErrEmptyKey", err)
	}
}

// TestConcurrentDuplicateDeliveryIsExactlyOnce hammers the sink from many
// goroutines, each delivering the SAME set of records (as if every partition
// retried every record). The final state must equal a single application and
// the number of applied writes must equal the number of distinct keys.
func TestConcurrentDuplicateDeliveryIsExactlyOnce(t *testing.T) {
	t.Parallel()

	const keys = 50
	const deliverers = 8

	batch := make([]Record, keys)
	for i := range batch {
		batch[i] = Record{
			Key:     []byte(fmt.Sprintf("k%d", i)),
			Value:   []byte(fmt.Sprintf("v%d", i)),
			Version: 1,
		}
	}

	s := NewUpsertSink()
	var wg sync.WaitGroup
	for d := 0; d < deliverers; d++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Write(batch); err != nil {
				t.Errorf("Write: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := s.Len(); got != keys {
		t.Fatalf("Len = %d, want %d", got, keys)
	}
	if got := s.metrics.Applied.Load(); got != int64(keys) {
		t.Fatalf("Applied = %d, want %d (state must be exactly-once)", got, keys)
	}
	for i := 0; i < keys; i++ {
		want := fmt.Sprintf("v%d", i)
		if v, _ := s.Get(fmt.Sprintf("k%d", i)); string(v) != want {
			t.Fatalf("k%d = %q, want %q", i, v, want)
		}
	}
}

// TestSnapshotDeduplicates verifies the flush-on-checkpoint snapshot contains
// exactly one row per key, regardless of how many duplicate deliveries occurred.
func TestSnapshotDeduplicates(t *testing.T) {
	t.Parallel()

	s := NewUpsertSink()
	// Deliver the same two keys three times, with the last delivery newer.
	_ = s.Write([]Record{{Key: []byte("a"), Value: []byte("a1"), Version: 1}})
	_ = s.Write([]Record{{Key: []byte("a"), Value: []byte("a1"), Version: 1}})
	_ = s.Write([]Record{{Key: []byte("a"), Value: []byte("a2"), Version: 2}})
	_ = s.Write([]Record{{Key: []byte("b"), Value: []byte("b1"), Version: 1}})

	path := filepath.Join(t.TempDir(), "view.ndjson")
	if err := s.Snapshot(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Count(data, []byte("\n"))
	if lines != 2 {
		t.Fatalf("snapshot has %d rows, want 2:\n%s", lines, data)
	}
}
```

## Review

The sink is correct when re-delivery and reordering both leave the state a deterministic function of the highest-versioned write per key. Confirm the three version cases are handled distinctly — higher applies, equal is a no-op duplicate, lower is rejected — because collapsing equal-version duplicates into the apply path would over-count, and accepting lower versions would corrupt state under reordering. Confirm values are copied in and out so buffer reuse cannot alias a stored value. The classic mistakes are the blind upsert with no version guard (a late stale re-delivery silently rolls a key back), forgetting to copy the value bytes (a reused source buffer mutates the store), and a non-atomic snapshot (a reader catches a half-written file). The concurrency test passing under `go test -race ./...` is the proof that duplicate delivery yields exactly-once state.

## Resources

- [`sync/atomic` Int64](https://pkg.go.dev/sync/atomic#Int64) — the lock-free counters the metrics use.
- [Kafka: Idempotent and transactional producers](https://kafka.apache.org/documentation/#semantics) — how keyed dedup underpins exactly-once in a production streaming system.
- [Designing Data-Intensive Applications, ch. 11 (idempotence)](https://dataintensive.net/) — why idempotent operations turn at-least-once delivery into exactly-once effect.
- [`os.Rename`](https://pkg.go.dev/os#Rename) — the atomic rename that makes the snapshot crash-safe.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-http-sink-batching-idempotency.md](03-http-sink-batching-idempotency.md) | Next: [05-dead-letter-sink.md](05-dead-letter-sink.md)
