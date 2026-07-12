# Exercise 5: Pluggable State Backends

Operators should not care whether their state lives in memory, on a local disk, or in object storage. Hiding the store behind a `StateBackend` interface lets the same pipeline run against a fast in-memory backend in tests and a durable file backend in production. This module defines that interface, implements it twice, and proves the two implementations are interchangeable with a single conformance suite run against both.

This module is fully self-contained, with its own `go mod init`, demo, and tests.

## What you'll build

```text
backend.go             CheckpointID, StateBackend interface, sentinel errors
memory.go              MemoryStateBackend (map-backed, byte-isolated)
file.go                FileStateBackend (atomic temp-then-rename)
cmd/
  demo/
    main.go            same workflow run against memory and file backends
backend_test.go        one conformance suite over both backends, byte isolation
```

- Files: `backend.go`, `memory.go`, `file.go`, `cmd/demo/main.go`, `backend_test.go`.
- Implement: the `StateBackend` interface and two implementations — `MemoryStateBackend` and `FileStateBackend` — each safe for concurrent use.
- Test: `backend_test.go` runs one behavioural suite against every backend through the interface, plus a byte-isolation test specific to the in-memory backend.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/05-checkpointing/05-pluggable-state-backends/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/05-checkpointing/05-pluggable-state-backends
go mod edit -go=1.26
```

### One interface, two implementations, one test suite

The `StateBackend` interface is the seam: `SaveState`, `LoadState`, `MarkComplete`, `LatestCheckpoint`, `Close`. Any code written against it — the coordinator, the operators, the recovery path — works with any implementation without change. The two implementations make very different trade-offs. `MemoryStateBackend` keeps state in nested maps; a full checkpoint round-trip costs a map lookup and nothing touches the disk, which is exactly what a unit test wants and exactly what production cannot use, because the state evaporates on restart. `FileStateBackend` is the durable counterpart, writing each snapshot with the same atomic temp-then-rename pattern as the earlier exercises.

The in-memory backend has one obligation the file backend gets for free: byte isolation. `SaveState` must store a *copy* of the state slice, and `LoadState` must return a *copy*. If it kept the caller's slice, a later mutation of that slice would silently rewrite the stored snapshot; the file backend cannot have this bug because it serializes through the filesystem, so the in-memory one must copy explicitly to match the contract.

The discipline that keeps the two honest is `TestConformance`: a table of backend factories and a set of sub-tests run against each. Because the sub-tests only ever touch the interface, a behavioural difference between the backends — a different error on a missing key, say, or counting an incomplete checkpoint as latest — fails the suite for one backend and not the other. Two compile-time assertions (`var _ StateBackend = (*MemoryStateBackend)(nil)`) catch an interface drift the moment a method signature changes.

Create `backend.go`:
```go
// Package backend defines a StateBackend abstraction for checkpoint storage and
// ships two interchangeable implementations: an in-memory backend for tests and
// low-latency development, and a file backend for durable production storage.
package backend

import (
	"errors"
)

// Sentinel errors shared by every StateBackend implementation.
var (
	ErrNoCheckpoint  = errors.New("no completed checkpoint found")
	ErrStateNotFound = errors.New("state not found")
)

// CheckpointID is a monotonically increasing checkpoint identifier.
type CheckpointID uint64

// StateBackend stores and retrieves serialized operator state. Every method is
// safe for concurrent use. Decoupling operators from a concrete store lets a
// pipeline run against an in-memory backend in tests and a file (or remote)
// backend in production without touching operator code.
type StateBackend interface {
	// SaveState writes serialized state for operatorID at checkpoint id.
	SaveState(id CheckpointID, operatorID string, state []byte) error
	// LoadState retrieves state, wrapping ErrStateNotFound when absent.
	LoadState(id CheckpointID, operatorID string) ([]byte, error)
	// MarkComplete records that checkpoint id is fully acknowledged.
	MarkComplete(id CheckpointID) error
	// LatestCheckpoint returns the highest completed checkpoint, wrapping
	// ErrNoCheckpoint when none is complete.
	LatestCheckpoint() (CheckpointID, error)
	// Close releases backend resources.
	Close() error
}
```
Create `memory.go`:
```go
package backend

import (
	"fmt"
	"sync"
)

// MemoryStateBackend keeps checkpoint state in process memory. It is a drop-in
// StateBackend for tests and development: there is no disk I/O, so a full
// checkpoint round-trip costs a map lookup. State does not survive a restart,
// which is exactly what a unit test wants and exactly what production does not.
type MemoryStateBackend struct {
	mu        sync.Mutex
	state     map[CheckpointID]map[string][]byte
	completed map[CheckpointID]bool
}

// NewMemoryStateBackend creates an empty in-memory backend.
func NewMemoryStateBackend() *MemoryStateBackend {
	return &MemoryStateBackend{
		state:     make(map[CheckpointID]map[string][]byte),
		completed: make(map[CheckpointID]bool),
	}
}

// SaveState stores a private copy of state so later mutation of the caller's
// slice cannot corrupt the stored snapshot.
func (m *MemoryStateBackend) SaveState(id CheckpointID, operatorID string, state []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ops, ok := m.state[id]
	if !ok {
		ops = make(map[string][]byte)
		m.state[id] = ops
	}
	cp := make([]byte, len(state))
	copy(cp, state)
	ops[operatorID] = cp
	return nil
}

// LoadState returns a private copy of the stored state.
func (m *MemoryStateBackend) LoadState(id CheckpointID, operatorID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ops, ok := m.state[id]
	if !ok {
		return nil, fmt.Errorf("memory: %w: operator=%s ckpt=%d", ErrStateNotFound, operatorID, id)
	}
	data, ok := ops[operatorID]
	if !ok {
		return nil, fmt.Errorf("memory: %w: operator=%s ckpt=%d", ErrStateNotFound, operatorID, id)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

// MarkComplete flags checkpoint id as fully acknowledged.
func (m *MemoryStateBackend) MarkComplete(id CheckpointID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed[id] = true
	return nil
}

// LatestCheckpoint returns the highest completed checkpoint ID.
func (m *MemoryStateBackend) LatestCheckpoint() (CheckpointID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest CheckpointID
	found := false
	for id, done := range m.completed {
		if !done {
			continue
		}
		if !found || id > latest {
			latest, found = id, true
		}
	}
	if !found {
		return 0, fmt.Errorf("memory: %w", ErrNoCheckpoint)
	}
	return latest, nil
}

// Close releases resources; nothing to do for an in-memory backend.
func (m *MemoryStateBackend) Close() error { return nil }
```
Create `file.go`:
```go
package backend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FileStateBackend persists checkpoint state on the local filesystem using
// atomic write-to-temp-then-rename. It is the durable counterpart to
// MemoryStateBackend and satisfies the same StateBackend interface.
type FileStateBackend struct {
	mu  sync.Mutex
	dir string
}

// NewFileStateBackend roots a file backend at dir.
func NewFileStateBackend(dir string) (*FileStateBackend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("file: create dir: %w", err)
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
		return fmt.Errorf("file: mkdir ckpt-%d: %w", id, err)
	}
	dst := filepath.Join(dir, operatorID+".state")
	tmp, err := os.CreateTemp(dir, ".tmp-"+operatorID+"-*")
	if err != nil {
		return fmt.Errorf("file: temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(state); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("file: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("file: close: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("file: rename: %w", err)
	}
	return nil
}

// LoadState reads state for operatorID at the given checkpoint.
func (b *FileStateBackend) LoadState(id CheckpointID, operatorID string) ([]byte, error) {
	path := filepath.Join(b.ckptDir(id), operatorID+".state")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("file: %w: operator=%s ckpt=%d", ErrStateNotFound, operatorID, id)
	}
	if err != nil {
		return nil, fmt.Errorf("file: load: %w", err)
	}
	return data, nil
}

// MarkComplete writes the completion marker for checkpoint id.
func (b *FileStateBackend) MarkComplete(id CheckpointID) error {
	dir := b.ckptDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("file: mkdir complete ckpt-%d: %w", id, err)
	}
	f, err := os.Create(filepath.Join(dir, "completed"))
	if err != nil {
		return fmt.Errorf("file: mark complete ckpt-%d: %w", id, err)
	}
	return f.Close()
}

// LatestCheckpoint scans the directory for the highest completed checkpoint ID.
func (b *FileStateBackend) LatestCheckpoint() (CheckpointID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return 0, fmt.Errorf("file: read dir: %w", err)
	}
	var latest CheckpointID
	found := false
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
		if id := CheckpointID(n); !found || id > latest {
			latest, found = id, true
		}
	}
	if !found {
		return 0, fmt.Errorf("file: %w", ErrNoCheckpoint)
	}
	return latest, nil
}

// Close is a no-op for the file backend.
func (b *FileStateBackend) Close() error { return nil }
```
### The runnable demo

The demo's `run` function drives a checkpoint workflow — save two checkpoints, finalize them, read the latest back — against any `StateBackend`. Its body never names a concrete type, so calling it once with a memory backend and once with a file backend proves the abstraction holds: identical output from two completely different stores.

Create `cmd/demo/main.go`:
```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/pluggable-state-backends"
)

// run drives the same checkpoint workflow against any StateBackend. The body
// never names a concrete type: the abstraction is what lets one pipeline run on
// memory in a test and on the filesystem in production.
func run(label string, b backend.StateBackend) {
	defer b.Close()
	for id := backend.CheckpointID(1); id <= 2; id++ {
		_ = b.SaveState(id, "counter", []byte(fmt.Sprintf(`{"count":%d}`, id*5)))
		_ = b.MarkComplete(id)
	}
	latest, err := b.LatestCheckpoint()
	if err != nil {
		log.Fatal(err)
	}
	data, err := b.LoadState(latest, "counter")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s: latest=%d state=%s\n", label, latest, data)
}

func main() {
	run("memory", backend.NewMemoryStateBackend())

	dir, err := os.MkdirTemp("", "demo-backend-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fb, err := backend.NewFileStateBackend(dir)
	if err != nil {
		log.Fatal(err)
	}
	run("file", fb)
}
```
Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
memory: latest=2 state={"count":10}
file: latest=2 state={"count":10}
```

### Tests

`TestConformance` is the centrepiece: a factory table builds a fresh memory backend and a fresh file backend, and four sub-tests — save/load, missing-key, empty-latest, and incomplete-ignored — run against each through the interface. `TestMemoryIsolatesStoredBytes` mutates the caller's slice after `SaveState` and asserts the stored snapshot is unchanged, pinning the copy contract the file backend satisfies for free.

Create `backend_test.go`:
```go
package backend

import (
	"errors"
	"testing"
)

// backendFactory builds a fresh StateBackend for one subtest.
type backendFactory struct {
	name string
	make func(t *testing.T) StateBackend
}

func factories(t *testing.T) []backendFactory {
	return []backendFactory{
		{"memory", func(t *testing.T) StateBackend { return NewMemoryStateBackend() }},
		{"file", func(t *testing.T) StateBackend {
			b, err := NewFileStateBackend(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
	}
}

// TestConformance runs one behavioural suite against every implementation, so
// memory and file backends are proven interchangeable.
func TestConformance(t *testing.T) {
	t.Parallel()
	for _, f := range factories(t) {
		f := f
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			t.Run("SaveLoad", func(t *testing.T) {
				b := f.make(t)
				defer b.Close()
				if err := b.SaveState(1, "op", []byte("hello")); err != nil {
					t.Fatalf("SaveState: %v", err)
				}
				got, err := b.LoadState(1, "op")
				if err != nil {
					t.Fatalf("LoadState: %v", err)
				}
				if string(got) != "hello" {
					t.Errorf("LoadState = %q, want %q", got, "hello")
				}
			})

			t.Run("LoadMissing", func(t *testing.T) {
				b := f.make(t)
				defer b.Close()
				if _, err := b.LoadState(9, "absent"); !errors.Is(err, ErrStateNotFound) {
					t.Errorf("LoadState missing: err = %v, want ErrStateNotFound", err)
				}
			})

			t.Run("LatestEmpty", func(t *testing.T) {
				b := f.make(t)
				defer b.Close()
				if _, err := b.LatestCheckpoint(); !errors.Is(err, ErrNoCheckpoint) {
					t.Errorf("LatestCheckpoint empty: err = %v, want ErrNoCheckpoint", err)
				}
			})

			t.Run("LatestIgnoresIncomplete", func(t *testing.T) {
				b := f.make(t)
				defer b.Close()
				if err := b.SaveState(1, "op", []byte("s")); err != nil {
					t.Fatal(err)
				}
				if err := b.MarkComplete(1); err != nil {
					t.Fatal(err)
				}
				if err := b.SaveState(2, "op", []byte("s")); err != nil {
					t.Fatal(err) // saved but not completed
				}
				latest, err := b.LatestCheckpoint()
				if err != nil {
					t.Fatalf("LatestCheckpoint: %v", err)
				}
				if latest != 1 {
					t.Errorf("LatestCheckpoint = %d, want 1", latest)
				}
			})
		})
	}
}

// TestMemoryIsolatesStoredBytes proves the in-memory backend copies state, so a
// caller mutating its slice after SaveState cannot corrupt the snapshot.
func TestMemoryIsolatesStoredBytes(t *testing.T) {
	t.Parallel()
	b := NewMemoryStateBackend()
	buf := []byte("original")
	if err := b.SaveState(1, "op", buf); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X' // mutate after save
	got, err := b.LoadState(1, "op")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Errorf("LoadState = %q, want %q (stored bytes must be isolated)", got, "original")
	}
}

// Compile-time assertions that both types satisfy the interface.
var (
	_ StateBackend = (*MemoryStateBackend)(nil)
	_ StateBackend = (*FileStateBackend)(nil)
)
```
## Review

The abstraction is correct when every implementation passes the same suite, so a caller can swap backends with one line and trust the behaviour. The most common error is letting an implementation diverge in a way no shared test would catch — a different sentinel for a missing key, or `LatestCheckpoint` counting an unfinalized checkpoint — which is exactly what `TestConformance` exists to prevent by routing every check through the interface. The second is the in-memory backend storing the caller's slice instead of a copy, an aliasing bug that the file backend cannot reproduce and that `TestMemoryIsolatesStoredBytes` pins. The compile-time `var _ StateBackend` assertions turn an accidental signature change into a build error rather than a runtime surprise.

## Resources

- [Effective Go: interfaces](https://go.dev/doc/effective_go#interfaces) — defining behaviour as an interface and satisfying it implicitly.
- [Go blog: errors are values](https://go.dev/blog/errors-are-values) — why shared sentinel errors keep multiple implementations consistent.
- [Apache Flink: state backends](https://nightlies.apache.org/flink/flink-docs-stable/docs/ops/state/state_backends/) — the same pluggable-backend idea (heap vs RocksDB) in a production engine.

---

Back to [04-durable-fsync-writes.md](04-durable-fsync-writes.md) | Next: [06-incremental-snapshots.md](06-incremental-snapshots.md)
