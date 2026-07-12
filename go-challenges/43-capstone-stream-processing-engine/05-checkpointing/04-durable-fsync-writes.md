# Exercise 4: Durable Writes with fsync

The temp-then-rename pattern is atomic, but atomic is not the same as durable: after a power loss the operating system may still hold both the file's data and the rename only in its page cache, and the "saved" checkpoint vanishes. This module hardens the write with the two `fsync` calls a crash-safe write actually requires — one on the data file and one on the parent directory.

This module is fully self-contained, with its own `go mod init`, demo, and tests, importing nothing from any other exercise.

## What you'll build

```text
durable.go             AtomicWriteFile, fsyncDir, DurableStore, Save, Load
cmd/
  demo/
    main.go            durable write, atomic overwrite, second operator
durable_test.go        round-trip, no temp leak, atomic overwrite, store save/load
```

- Files: `durable.go`, `cmd/demo/main.go`, `durable_test.go`.
- Implement: `AtomicWriteFile(dir, name string, data []byte) error` performing write, file-fsync, rename, dir-fsync; and a `DurableStore` that uses it.
- Test: `durable_test.go` proves the round-trip, that a successful write leaves no temp file behind, that concurrent overwrites are never observed torn, and the store's save/load and `ErrStateNotFound` paths.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/05-checkpointing/04-durable-fsync-writes/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/05-checkpointing/04-durable-fsync-writes
go mod edit -go=1.26
```

### The four steps, and why each fsync is load-bearing

`AtomicWriteFile` performs four ordered steps: write the data to a same-directory temp file; `fsync` the temp file; rename it onto the destination; `fsync` the parent directory. The two renames-and-fsyncs are not interchangeable, and dropping either fsync introduces a different, silent failure mode.

The file fsync (step 2) forces the temp file's *contents* to stable storage before the rename publishes its name. Without it, the rename — which is small metadata and may be flushed quickly — can reach disk while the data behind it has not, so after a crash the destination exists by name but reads back as zeros or garbage. The directory fsync (step 4) forces the *rename itself* to stable storage. A rename mutates the parent directory's list of entries; until that directory block is flushed, the new entry lives only in the page cache, and a crash can lose it entirely even though `os.Rename` returned nil. `fsyncDir` performs it by opening the directory read-only and calling `Sync`, which is the portable way to flush a directory's metadata.

The function is written so that any error after the temp file is created removes the orphan via a deferred cleanup keyed on the named return value `err`. That keeps a failed save from littering the checkpoint directory with `.tmp-*` files that a later directory scan would have to ignore.

Create `durable.go`:
```go
// Package durable writes checkpoint state so that it survives a power loss, not
// just a clean process exit. It pairs the atomic temp-then-rename pattern with
// the two fsync calls a crash-safe write requires: one on the data file and one
// on the parent directory.
package durable

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrStateNotFound is returned by Load when no state file exists.
var ErrStateNotFound = errors.New("state not found")

// AtomicWriteFile durably writes data to dir/name. The sequence is:
//
//  1. write data to a same-directory temp file;
//  2. fsync the temp file so its bytes reach stable storage;
//  3. rename the temp file onto the destination (atomic on POSIX);
//  4. fsync the parent directory so the rename itself is durable.
//
// Steps 2 and 4 are what separate "durable" from merely "atomic". A rename is
// atomic with respect to concurrent readers even without fsync, but after a
// power loss the kernel may have the rename in its page cache and not on disk;
// fsyncing the directory forces the new directory entry out. Likewise, without
// step 2 the file could exist by name yet contain zeros, because the rename
// reached disk before the data did.
func AtomicWriteFile(dir, name string, data []byte) (err error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("durable: mkdir %s: %w", dir, err)
	}
	dst := filepath.Join(dir, name)

	tmp, err := os.CreateTemp(dir, ".tmp-"+name+"-*")
	if err != nil {
		return fmt.Errorf("durable: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// On any failure after this point, remove the orphan temp file.
	defer func() {
		if err != nil {
			os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("durable: write temp: %w", err)
	}
	if err = tmp.Sync(); err != nil { // (2) flush file contents
		tmp.Close()
		return fmt.Errorf("durable: fsync temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("durable: close temp: %w", err)
	}
	if err = os.Rename(tmpName, dst); err != nil { // (3) atomic publish
		return fmt.Errorf("durable: rename: %w", err)
	}
	if err = fsyncDir(dir); err != nil { // (4) flush the directory entry
		return fmt.Errorf("durable: fsync dir: %w", err)
	}
	return nil
}

// fsyncDir flushes a directory's own metadata (its list of entries) to stable
// storage. It opens the directory read-only and calls Sync; that is the
// portable way to force a rename or create within it to become durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// DurableStore persists operator state under a checkpoint directory hierarchy,
// using AtomicWriteFile for every write.
type DurableStore struct {
	root string
}

// NewDurableStore roots a store at dir.
func NewDurableStore(dir string) (*DurableStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("durable: create store dir: %w", err)
	}
	return &DurableStore{root: dir}, nil
}

// Save durably writes state for operatorID at the given checkpoint.
func (s *DurableStore) Save(checkpointID uint64, operatorID string, state []byte) error {
	dir := filepath.Join(s.root, fmt.Sprintf("ckpt-%d", checkpointID))
	return AtomicWriteFile(dir, operatorID+".state", state)
}

// Load reads state for operatorID at the given checkpoint.
func (s *DurableStore) Load(checkpointID uint64, operatorID string) ([]byte, error) {
	path := filepath.Join(s.root, fmt.Sprintf("ckpt-%d", checkpointID), operatorID+".state")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("durable: %w: operator=%s ckpt=%d", ErrStateNotFound, operatorID, checkpointID)
	}
	if err != nil {
		return nil, fmt.Errorf("durable: load: %w", err)
	}
	return data, nil
}
```
### The runnable demo

The demo writes a value durably, reads it back, overwrites it atomically (a reader always sees the old value or the new one, never a mix), and writes a second operator's state into the same checkpoint directory.

Create `cmd/demo/main.go`:
```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/durable-fsync-writes"
)

func main() {
	dir, err := os.MkdirTemp("", "demo-durable-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := durable.NewDurableStore(dir)
	if err != nil {
		log.Fatal(err)
	}

	// First durable write.
	if err := store.Save(1, "counter", []byte(`{"count":10}`)); err != nil {
		log.Fatal(err)
	}
	data, err := store.Load(1, "counter")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("checkpoint 1 counter: %s\n", data)

	// Overwriting the same key is atomic: a reader sees old or new, never partial.
	if err := store.Save(1, "counter", []byte(`{"count":20}`)); err != nil {
		log.Fatal(err)
	}
	data, err = store.Load(1, "counter")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after overwrite: %s\n", data)

	// A second operator in the same checkpoint.
	if err := store.Save(1, "windows", []byte(`{"open":3}`)); err != nil {
		log.Fatal(err)
	}
	data, err = store.Load(1, "windows")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("checkpoint 1 windows: %s\n", data)
}
```
Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
checkpoint 1 counter: {"count":10}
after overwrite: {"count":20}
checkpoint 1 windows: {"open":3}
```

### Tests

`TestAtomicWriteRoundTrip` checks bytes survive a write/read. `TestAtomicWriteLeavesNoTempFiles` proves the only entry left after a successful write is the destination — the temp file is gone. `TestOverwriteIsAtomic` runs a writer alternating two fixed-width values against a concurrent reader and asserts the reader never sees a torn mix, which is the observable consequence of atomic rename. `TestDurableStoreSaveLoad` and `TestDurableStoreLoadMissing` cover the store wrapper and its `ErrStateNotFound` path.

Create `durable_test.go`:
```go
package durable

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAtomicWriteRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := []byte("durable-bytes")
	if err := AtomicWriteFile(dir, "x.state", want); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "x.state"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("read = %q, want %q", got, want)
	}
}

// TestAtomicWriteLeavesNoTempFiles checks that a successful write removes its
// temp file: only the destination must remain in the directory.
func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := AtomicWriteFile(dir, "x.state", []byte("v")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "x.state" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want [x.state]", names)
	}
}

// TestOverwriteIsAtomic overwrites a key repeatedly; every concurrent read must
// observe a fully-formed value (one of the written ones), never a partial mix.
func TestOverwriteIsAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valA := []byte("aaaaaaaaaaaaaaaa")
	valB := []byte("bbbbbbbbbbbbbbbb")
	if err := AtomicWriteFile(dir, "k.state", valA); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			v := valA
			if i%2 == 1 {
				v = valB
			}
			if err := AtomicWriteFile(dir, "k.state", v); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			got, err := os.ReadFile(filepath.Join(dir, "k.state"))
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if string(got) != string(valA) && string(got) != string(valB) {
				t.Errorf("torn read: %q", got)
				return
			}
		}
	}()
	wg.Wait()
}

func TestDurableStoreSaveLoad(t *testing.T) {
	t.Parallel()
	s, err := NewDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(7, "op", []byte("state-7")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(7, "op")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got) != "state-7" {
		t.Errorf("Load = %q, want %q", got, "state-7")
	}
}

func TestDurableStoreLoadMissing(t *testing.T) {
	t.Parallel()
	s, err := NewDurableStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(1, "absent"); !errors.Is(err, ErrStateNotFound) {
		t.Errorf("Load missing: err = %v, want ErrStateNotFound", err)
	}
}
```
## Review

The write is durable when both the data and the directory entry are on stable storage before the function returns. The most common error is treating atomic and durable as the same thing — temp-then-rename alone passes every test on a cleanly exiting process and still loses data on a real power cut, because the only thing that forces bytes out of the page cache is `fsync`. The second is fsyncing the file but not the parent directory: the rename then survives only as long as the cache does. The third is leaving orphan temp files on an error path; the deferred cleanup keyed on the named return makes a failed write leave the directory exactly as it found it. `go test -race` confirms the concurrent overwrite-and-read test sees no race and no torn value.

## Resources

- [fsync(2) manual page](https://man7.org/linux/man-pages/man2/fsync.2.html) — what fsync flushes, and the note that a file's directory entry needs its own fsync.
- [Dan Luu, "Files are hard"](https://danluu.com/file-consistency/) — a survey of how often the atomic-but-not-durable mistake causes real data loss.
- [pkg.go.dev/os — File.Sync](https://pkg.go.dev/os#File.Sync) — the Go call that issues fsync on a file or directory handle.

---

Back to [03-coordinator-recovery.md](03-coordinator-recovery.md) | Next: [05-pluggable-state-backends.md](05-pluggable-state-backends.md)
