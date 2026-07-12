# Exercise 1: Atomic State Backend

The state backend is where every operator's snapshot lands, and its one non-negotiable property is that a crash mid-write must never leave a half-written snapshot behind. This module builds a filesystem-backed store that writes each snapshot atomically with the temp-then-rename pattern, tracks which checkpoints are complete, and prunes old ones.

This module is fully self-contained: it begins with its own `go mod init`, defines the `CheckpointID` and sentinel errors it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
store.go               CheckpointID, FileStateBackend, SaveState, LoadState,
                       LatestCheckpoint, MarkComplete, Prune
cmd/
  demo/
    main.go            save+complete three checkpoints, load latest, prune
store_test.go          round-trip, missing state, incomplete-ignored, prune, races
```

- Files: `store.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: `FileStateBackend` with `SaveState`, `LoadState`, `MarkComplete`, `LatestCheckpoint`, `Prune`, and `Close`, all safe for concurrent use.
- Test: `store_test.go` proves the save/load round-trip, the `ErrStateNotFound` and `ErrNoCheckpoint` paths, that an incomplete checkpoint is ignored by `LatestCheckpoint`, that `Prune` keeps the most recent N, and that concurrent saves are race-free.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### How an atomic write actually works

The backend lays state out as a directory per checkpoint: `<dir>/ckpt-<id>/<operatorID>.state` for the data and `<dir>/ckpt-<id>/completed` as a marker file that exists only once every operator has acknowledged. A checkpoint with no `completed` marker is in-flight and must be invisible to recovery, which is why `LatestCheckpoint` and `Prune` both filter on that marker rather than on the directory's mere existence.

`SaveState` never writes the destination file directly. It writes to a temporary file created with `os.CreateTemp(dir, ...)` in the *same* directory as the destination, closes it, then renames it onto the destination. On a POSIX filesystem the rename is atomic: a reader either sees the previous `.state` file or the new one, never a partially written file, and a crash at any instant leaves one whole file or the other. Creating the temp file in the destination directory is what makes the rename safe — a rename across two filesystems is not atomic and fails outright with `EXDEV`, so a temp file in the system temp directory would be a latent bug that only fires when `/tmp` is a separate mount.

`LatestCheckpoint` and `Prune` share a single `completedIDs` helper that scans the directory once, keeps only the `ckpt-N` directories carrying a `completed` marker, parses their numeric IDs, and returns them sorted. `LatestCheckpoint` returns the last element; `Prune` deletes everything except the final `keepN`. Routing both through one helper means there is exactly one definition of "a completed checkpoint" and the two methods can never disagree about it.

Create `store.go`:
```go
// Package store persists stream-processing checkpoint state on the local
// filesystem with atomic, crash-safe writes (write-to-temp then rename).
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Sentinel errors. Every error returned by this package wraps one of these,
// so callers branch on the class of failure with errors.Is.
var (
	ErrNoCheckpoint  = errors.New("no completed checkpoint found")
	ErrStateNotFound = errors.New("state not found")
)

// CheckpointID is a monotonically increasing identifier assigned by the engine.
type CheckpointID uint64

// FileStateBackend persists checkpoint state on the local filesystem.
//
// Directory layout:
//
//	<dir>/
//	  ckpt-1/
//	    counter.state    -- operator state (opaque bytes)
//	    completed        -- marker: checkpoint 1 is fully acknowledged
//	  ckpt-2/
//	    ...
type FileStateBackend struct {
	mu  sync.Mutex
	dir string
}

// NewFileStateBackend creates a FileStateBackend rooted at dir, creating the
// directory if it does not exist.
func NewFileStateBackend(dir string) (*FileStateBackend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: create backend dir: %w", err)
	}
	return &FileStateBackend{dir: dir}, nil
}

func (b *FileStateBackend) ckptDir(id CheckpointID) string {
	return filepath.Join(b.dir, fmt.Sprintf("ckpt-%d", uint64(id)))
}

// SaveState atomically writes state using write-to-temp-then-rename.
// The temp file is created in the same directory as the destination so that
// os.Rename stays on one filesystem (cross-device rename is not atomic).
func (b *FileStateBackend) SaveState(id CheckpointID, operatorID string, state []byte) error {
	dir := b.ckptDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir ckpt-%d: %w", id, err)
	}
	dst := filepath.Join(dir, operatorID+".state")
	tmp, err := os.CreateTemp(dir, ".tmp-"+operatorID+"-*")
	if err != nil {
		return fmt.Errorf("store: create temp for %s: %w", operatorID, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(state); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("store: write %s: %w", operatorID, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: close temp %s: %w", operatorID, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("store: rename for %s: %w", operatorID, err)
	}
	return nil
}

// LoadState reads state for operatorID at the given checkpoint.
// Returns a wrapped ErrStateNotFound if the state file does not exist.
func (b *FileStateBackend) LoadState(id CheckpointID, operatorID string) ([]byte, error) {
	path := filepath.Join(b.ckptDir(id), operatorID+".state")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("store: %w: operator=%s ckpt=%d", ErrStateNotFound, operatorID, id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: load state %s: %w", operatorID, err)
	}
	return data, nil
}

// MarkComplete writes the completion marker for checkpoint id.
func (b *FileStateBackend) MarkComplete(id CheckpointID) error {
	dir := b.ckptDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir for complete ckpt-%d: %w", id, err)
	}
	marker := filepath.Join(dir, "completed")
	f, err := os.Create(marker)
	if err != nil {
		return fmt.Errorf("store: mark complete ckpt-%d: %w", id, err)
	}
	return f.Close()
}

// LatestCheckpoint scans the directory for the highest completed checkpoint ID.
// Returns a wrapped ErrNoCheckpoint when no completed checkpoint exists.
func (b *FileStateBackend) LatestCheckpoint() (CheckpointID, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	completed, err := b.completedIDs()
	if err != nil {
		return 0, err
	}
	if len(completed) == 0 {
		return 0, fmt.Errorf("store: %w", ErrNoCheckpoint)
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
			return fmt.Errorf("store: prune ckpt-%d: %w", id, err)
		}
	}
	return nil
}

// completedIDs returns the sorted IDs of every completed checkpoint. Callers
// must hold b.mu.
func (b *FileStateBackend) completedIDs() ([]uint64, error) {
	entries, err := os.ReadDir(b.dir)
	if err != nil {
		return nil, fmt.Errorf("store: read dir: %w", err)
	}
	var completed []uint64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "ckpt-") {
			continue
		}
		marker := filepath.Join(b.dir, e.Name(), "completed")
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			continue // not yet complete
		}
		idStr := strings.TrimPrefix(e.Name(), "ckpt-")
		n, err := strconv.ParseUint(idStr, 10, 64)
		if err != nil {
			continue
		}
		completed = append(completed, n)
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i] < completed[j] })
	return completed, nil
}

// Close releases backend resources. It is a no-op for FileStateBackend.
func (b *FileStateBackend) Close() error { return nil }
```
### The runnable demo

The demo writes and finalizes three checkpoints, reads the latest one back, then prunes down to a single retained checkpoint and shows that the oldest is gone. It uses `errors.Is` against the package sentinels so the "no checkpoint yet" and "checkpoint 1 is gone" branches are exact, not string matches.

Create `cmd/demo/main.go`:
```go
package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"example.com/atomic-state-backend"
)

func main() {
	dir, err := os.MkdirTemp("", "demo-store-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	b, err := store.NewFileStateBackend(dir)
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	// A fresh backend has no completed checkpoint.
	if _, err := b.LatestCheckpoint(); errors.Is(err, store.ErrNoCheckpoint) {
		fmt.Println("fresh backend: no checkpoint yet")
	}

	// Write and finalize three checkpoints.
	for id := store.CheckpointID(1); id <= 3; id++ {
		payload := []byte(fmt.Sprintf(`{"count":%d}`, id*10))
		if err := b.SaveState(id, "counter", payload); err != nil {
			log.Fatal(err)
		}
		if err := b.MarkComplete(id); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("saved+completed checkpoint %d\n", id)
	}

	latest, err := b.LatestCheckpoint()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("latest checkpoint: %d\n", latest)

	data, err := b.LoadState(latest, "counter")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("loaded state of %d: %s\n", latest, data)

	// Retain only the most recent checkpoint.
	if err := b.Prune(1); err != nil {
		log.Fatal(err)
	}
	if _, err := b.LoadState(1, "counter"); errors.Is(err, store.ErrStateNotFound) {
		fmt.Println("after Prune(1): checkpoint 1 is gone")
	}
}
```
Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fresh backend: no checkpoint yet
saved+completed checkpoint 1
saved+completed checkpoint 2
saved+completed checkpoint 3
latest checkpoint: 3
loaded state of 3: {"count":30}
after Prune(1): checkpoint 1 is gone
```

### Tests

The suite pins every contract. `TestSaveLoadRoundTrip` checks bytes survive a write/read cycle; `TestLoadNotFound` and `TestLatestCheckpointEmpty` check the two sentinel errors; `TestLatestIgnoresIncomplete` saves a checkpoint without marking it complete and proves `LatestCheckpoint` skips it; `TestPruneKeepsMostRecent` retains the last two of five; and `TestConcurrentSaves` hammers `SaveState` from sixteen goroutines so `go test -race` can prove the atomic-write path has no data race.

Create `store_test.go`:
```go
package store

import (
	"errors"
	"sync"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	b, err := NewFileStateBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	want := []byte(`{"count":42}`)
	if err := b.SaveState(1, "counter", want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := b.LoadState(1, "counter")
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("LoadState = %q, want %q", got, want)
	}
}

func TestLoadNotFound(t *testing.T) {
	t.Parallel()
	b, err := NewFileStateBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := b.LoadState(99, "missing"); !errors.Is(err, ErrStateNotFound) {
		t.Errorf("LoadState missing: err = %v, want ErrStateNotFound", err)
	}
}

func TestLatestCheckpointEmpty(t *testing.T) {
	t.Parallel()
	b, err := NewFileStateBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := b.LatestCheckpoint(); !errors.Is(err, ErrNoCheckpoint) {
		t.Errorf("LatestCheckpoint empty: err = %v, want ErrNoCheckpoint", err)
	}
}

func TestLatestIgnoresIncomplete(t *testing.T) {
	t.Parallel()
	b, err := NewFileStateBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Checkpoint 1 is complete; 2 was saved but never marked complete.
	if err := b.SaveState(1, "op", []byte("s")); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkComplete(1); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveState(2, "op", []byte("s")); err != nil {
		t.Fatal(err)
	}

	latest, err := b.LatestCheckpoint()
	if err != nil {
		t.Fatalf("LatestCheckpoint: %v", err)
	}
	if latest != 1 {
		t.Errorf("LatestCheckpoint = %d, want 1 (2 is incomplete)", latest)
	}
}

func TestPruneKeepsMostRecent(t *testing.T) {
	t.Parallel()
	b, err := NewFileStateBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	for id := CheckpointID(1); id <= 5; id++ {
		if err := b.SaveState(id, "op", []byte("x")); err != nil {
			t.Fatalf("SaveState(%d): %v", id, err)
		}
		if err := b.MarkComplete(id); err != nil {
			t.Fatalf("MarkComplete(%d): %v", id, err)
		}
	}
	if err := b.Prune(2); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for id := CheckpointID(1); id <= 3; id++ {
		if _, err := b.LoadState(id, "op"); !errors.Is(err, ErrStateNotFound) {
			t.Errorf("after Prune: LoadState(%d) = %v, want ErrStateNotFound", id, err)
		}
	}
	for id := CheckpointID(4); id <= 5; id++ {
		if _, err := b.LoadState(id, "op"); err != nil {
			t.Errorf("after Prune: LoadState(%d) unexpected error: %v", id, err)
		}
	}
}

// TestConcurrentSaves exercises the atomic write path from many goroutines at
// once; -race must report no data race and every state must read back intact.
func TestConcurrentSaves(t *testing.T) {
	t.Parallel()
	b, err := NewFileStateBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			op := "op" + string(rune('a'+n))
			payload := []byte(op + "-state")
			if err := b.SaveState(1, op, payload); err != nil {
				t.Errorf("SaveState(%s): %v", op, err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < 16; i++ {
		op := "op" + string(rune('a'+i))
		got, err := b.LoadState(1, op)
		if err != nil {
			t.Fatalf("LoadState(%s): %v", op, err)
		}
		if want := op + "-state"; string(got) != want {
			t.Errorf("LoadState(%s) = %q, want %q", op, got, want)
		}
	}
}
```
## Review

The backend is correct when an in-flight checkpoint is invisible and a completed one is durable. The most common error is letting `LatestCheckpoint` return a checkpoint directory that has no `completed` marker — recovery would then load a half-snapshotted checkpoint. The second is creating the temp file with `os.CreateTemp("", ...)`: it lands in the system temp directory, the cross-filesystem rename fails with `EXDEV`, and every save errors out. The third is testing for a missing file with `os.IsNotExist` instead of `errors.Is(err, os.ErrNotExist)`; the latter unwraps the `%w` chain the backend builds and is the only form that keeps working once the error is wrapped. Running the suite under `go test -race` confirms the mutex around the directory scan and the temp-then-rename writes leave no unsynchronised access.

## Resources

- [pkg.go.dev/os — CreateTemp and Rename](https://pkg.go.dev/os#CreateTemp) — the standard-library calls behind an atomic write.
- [rename(2) manual page](https://man7.org/linux/man-pages/man2/rename.2.html) — the atomicity guarantee, and the `EXDEV` failure for a cross-filesystem rename.
- [errors.Is](https://pkg.go.dev/errors#Is) — the chain-aware identity check used for `ErrStateNotFound`, `ErrNoCheckpoint`, and `os.ErrNotExist`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-barrier-alignment.md](02-barrier-alignment.md)
