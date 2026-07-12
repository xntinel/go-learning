# 10. Memory-Mapped Data Store with unsafe

`mmap` maps a file (or anonymous memory) directly into the process's virtual address space. After a single `mmap` syscall, every read from the mapped region is served by the OS page cache with zero `read` syscalls. Go's `unsafe.String` gives zero-copy string views into that region, and `unsafe.Pointer` lets you hand the mapping's base address to syscalls like `msync`. This is the architecture of LMDB, bbolt, and SQLite's WAL mode: zero-syscall reads, the OS handles page faults, and `msync` provides the durability guarantee.

The lesson is not trivial to set up: you must call `syscall.Mmap`, manage file growth with `ftruncate` and remap, handle alignment, and reason carefully about what happens to existing `unsafe.Pointer` values when a remap invalidates the mapping.

```text
mmapstore/
  go.mod
  mmapstore.go
  mmapstore_test.go
  cmd/demo/main.go
```

## Concepts

### How mmap Works

`mmap(fd, offset, size, prot, flags)` registers a virtual address range with the kernel. Accessing a page in that range for the first time triggers a page fault; the kernel reads the page from the file and maps it into physical memory. Subsequent accesses hit the TLB and require no syscall. `msync(addr, size, flags)` flushes dirty pages to disk. `munmap` releases the virtual address range.

In Go:

```go
mapped, err := syscall.Mmap(
	int(f.Fd()),
	0,
	size,
	syscall.PROT_READ|syscall.PROT_WRITE,
	syscall.MAP_SHARED,
)
```

`mapped` is a `[]byte` whose backing array is the mapped region. Every `mapped[i]` is a byte in the file.

### Pointer Arithmetic via unsafe.Pointer

An alternative way to read a `uint32` from offset `off` in the mapped region is via an unsafe pointer cast:

```go
val := *(*uint32)(unsafe.Pointer(&mapped[off]))
```

Writing:

```go
*(*uint32)(unsafe.Pointer(&mapped[off])) = newVal
```

Reading a `uint64` at offset `off`:

```go
val := *(*uint64)(unsafe.Pointer(&mapped[off]))
```

Note: these casts require `off` to be aligned to `unsafe.Alignof(uint64(0))` (8 bytes on 64-bit platforms) to avoid undefined behavior on architectures that forbid misaligned loads. The implementation in this lesson deliberately avoids these casts for numeric fields: it uses `binary.LittleEndian.Uint32` / `binary.LittleEndian.Uint64` instead, which have no alignment constraint and are portable across endianness. The unsafe package is reserved here for `unsafe.String` (zero-copy string views) and `unsafe.Pointer(&s.mapped[0])` (passing the mapping base address to the `msync` syscall).

### The Remap Trap

When the data region fills up, you must call `ftruncate` to grow the file and remap. After `syscall.Munmap(old)` and `syscall.Mmap(...)`, any pointer derived from the old mapping — every `unsafe.Pointer` you stashed — is invalid. The store must invalidate all derived pointers before the remap and recompute them from the new mapping afterward. The design below avoids stashing pointers: every accessor computes the offset from `s.mapped` at call time.

### File Format

```text
Header (64 bytes, at offset 0):
  [0:4]   magic       uint32  = 0x4D4D4150 ("MMAP")
  [4:8]   version     uint32  = 1
  [8:16]  entryCount  uint64
  [16:24] dataOffset  uint64  (byte offset where KV data begins)
  [24:32] dataSize    uint64  (bytes currently used in the data region)
  [32:64] reserved    [32]byte

Index (after header, fixed size = maxEntries * 16 bytes):
  Each IndexEntry:
    [0:4]  keyOffset   uint32  (relative to start of mapped region)
    [4:8]  keyLen      uint32
    [8:12] valOffset   uint32  (relative to start of mapped region)
    [12:16] valLen     uint32

Data region (after index):
  Raw bytes for keys and values, packed sequentially.
```

`dataOffset` = 64 + maxEntries*16 (fixed at open time). `dataSize` grows with each Put.

### Concurrency Model

Reads acquire a read lock; they compute offsets from `s.mapped` directly. Writes acquire a write lock; they append to the data region and update the index entry atomically (write data first, then update index, then increment count). A remap happens under the write lock, so readers can never observe a partially remapped state.

## Exercises

### Exercise 1: Implement the Store

Create `mmapstore.go`:

```go
package mmapstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

const (
	magic      uint32 = 0x4D4D4150 // "MMAP"
	version    uint32 = 1
	headerSize        = 64
	entrySize         = 16 // 4 fields * 4 bytes each
)

// ErrNotFound is returned by Get when the key does not exist.
var ErrNotFound = errors.New("mmapstore: key not found")

// ErrStoreFull is returned by Put when the index is at capacity.
var ErrStoreFull = errors.New("mmapstore: index is full")

// ErrClosed is returned by any method called after Close.
var ErrClosed = errors.New("mmapstore: store is closed")

// MMapStore is a persistent key-value store backed by a memory-mapped file.
// It supports concurrent reads and a single writer via sync.RWMutex.
// The store is NOT crash-safe; call Sync before relying on persistence.
//
// Returned strings from Get point into the mapped region and are valid only
// while the store is open. Copy them if you need them to outlive the store.
type MMapStore struct {
	mu         sync.RWMutex
	f          *os.File
	mapped     []byte
	maxEntries int
	closed     bool
}

// Open opens (or creates) the store at path. maxEntries sets the index capacity;
// initialSize sets the initial data region size in bytes.
func Open(path string, maxEntries, initialSize int) (*MMapStore, error) {
	if maxEntries <= 0 || initialSize <= 0 {
		return nil, fmt.Errorf("mmapstore: maxEntries and initialSize must be positive")
	}

	indexSize := maxEntries * entrySize
	minFileSize := headerSize + indexSize + initialSize

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mmapstore: open file: %w", err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmapstore: stat: %w", err)
	}

	fileSize := int(fi.Size())
	if fileSize < minFileSize {
		if err := f.Truncate(int64(minFileSize)); err != nil {
			f.Close()
			return nil, fmt.Errorf("mmapstore: truncate: %w", err)
		}
		fileSize = minFileSize
	}

	mapped, err := syscall.Mmap(
		int(f.Fd()), 0, fileSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED,
	)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmapstore: mmap: %w", err)
	}

	s := &MMapStore{f: f, mapped: mapped, maxEntries: maxEntries}

	// Initialize header if this is a new file.
	existingMagic := binary.LittleEndian.Uint32(mapped[0:4])
	if existingMagic != magic {
		binary.LittleEndian.PutUint32(mapped[0:4], magic)
		binary.LittleEndian.PutUint32(mapped[4:8], version)
		binary.LittleEndian.PutUint64(mapped[8:16], 0)
		dataOff := uint64(headerSize + indexSize)
		binary.LittleEndian.PutUint64(mapped[16:24], dataOff)
		binary.LittleEndian.PutUint64(mapped[24:32], 0)
	}

	return s, nil
}

func (s *MMapStore) checkClosed() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *MMapStore) entryCount() int {
	return int(binary.LittleEndian.Uint64(s.mapped[8:16]))
}

func (s *MMapStore) dataOffset() int {
	return int(binary.LittleEndian.Uint64(s.mapped[16:24]))
}

func (s *MMapStore) dataSize() int {
	return int(binary.LittleEndian.Uint64(s.mapped[24:32]))
}

func (s *MMapStore) indexEntryOffset(i int) int {
	return headerSize + i*entrySize
}

func (s *MMapStore) readEntry(i int) (keyOff, keyLen, valOff, valLen uint32) {
	off := s.indexEntryOffset(i)
	keyOff = binary.LittleEndian.Uint32(s.mapped[off : off+4])
	keyLen = binary.LittleEndian.Uint32(s.mapped[off+4 : off+8])
	valOff = binary.LittleEndian.Uint32(s.mapped[off+8 : off+12])
	valLen = binary.LittleEndian.Uint32(s.mapped[off+12 : off+16])
	return
}

func (s *MMapStore) writeEntry(i int, keyOff, keyLen, valOff, valLen uint32) {
	off := s.indexEntryOffset(i)
	binary.LittleEndian.PutUint32(s.mapped[off:off+4], keyOff)
	binary.LittleEndian.PutUint32(s.mapped[off+4:off+8], keyLen)
	binary.LittleEndian.PutUint32(s.mapped[off+8:off+12], valOff)
	binary.LittleEndian.PutUint32(s.mapped[off+12:off+16], valLen)
}

// Get returns the value for key. The returned string points into the mapped
// region; copy it if it needs to outlive the store.
func (s *MMapStore) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err := s.checkClosed(); err != nil {
		return "", err
	}

	count := s.entryCount()
	for i := 0; i < count; i++ {
		kOff, kLen, vOff, vLen := s.readEntry(i)
		if int(kLen) != len(key) {
			continue
		}
		storedKey := unsafe.String(&s.mapped[kOff], int(kLen))
		if storedKey == key {
			val := unsafe.String(&s.mapped[vOff], int(vLen))
			return val, nil
		}
	}
	return "", ErrNotFound
}

// Put inserts or updates key with value. Grows the mapped region if needed.
func (s *MMapStore) Put(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkClosed(); err != nil {
		return err
	}

	// Check for an existing key to update.
	count := s.entryCount()
	for i := 0; i < count; i++ {
		kOff, kLen, _, _ := s.readEntry(i)
		if int(kLen) != len(key) {
			continue
		}
		storedKey := unsafe.String(&s.mapped[kOff], int(kLen))
		if storedKey == key {
			// Append the new value; update the index entry.
			vOff, err := s.appendData(value)
			if err != nil {
				return err
			}
			existing := s.indexEntryOffset(i)
			binary.LittleEndian.PutUint32(s.mapped[existing+8:existing+12], uint32(vOff))
			binary.LittleEndian.PutUint32(s.mapped[existing+12:existing+16], uint32(len(value)))
			return nil
		}
	}

	if count >= s.maxEntries {
		return ErrStoreFull
	}

	// New key: append key then value.
	kOff, err := s.appendData(key)
	if err != nil {
		return err
	}
	vOff, err := s.appendData(value)
	if err != nil {
		return err
	}

	s.writeEntry(count, uint32(kOff), uint32(len(key)), uint32(vOff), uint32(len(value)))
	binary.LittleEndian.PutUint64(s.mapped[8:16], uint64(count+1))
	return nil
}

// appendData appends data to the data region, growing the file if needed.
// Returns the absolute offset into mapped where the data was written.
func (s *MMapStore) appendData(data string) (int, error) {
	dOff := s.dataOffset()
	dSize := s.dataSize()
	writeAt := dOff + dSize
	needed := writeAt + len(data)

	if needed > len(s.mapped) {
		newSize := len(s.mapped) * 2
		for newSize < needed {
			newSize *= 2
		}
		if err := s.remap(newSize); err != nil {
			return 0, err
		}
	}

	copy(s.mapped[writeAt:], data)
	binary.LittleEndian.PutUint64(s.mapped[24:32], uint64(dSize+len(data)))
	return writeAt, nil
}

// remap unmaps the current region, grows the file, and remaps it.
// Must be called under the write lock.
func (s *MMapStore) remap(newSize int) error {
	if err := syscall.Munmap(s.mapped); err != nil {
		return fmt.Errorf("mmapstore: munmap: %w", err)
	}
	s.mapped = nil

	if err := s.f.Truncate(int64(newSize)); err != nil {
		return fmt.Errorf("mmapstore: truncate for remap: %w", err)
	}

	mapped, err := syscall.Mmap(
		int(s.f.Fd()), 0, newSize,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED,
	)
	if err != nil {
		return fmt.Errorf("mmapstore: mmap after remap: %w", err)
	}
	s.mapped = mapped
	return nil
}

// Delete marks the first index entry matching key as empty (keyLen = 0).
// It does not compact the data region.
func (s *MMapStore) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkClosed(); err != nil {
		return err
	}

	count := s.entryCount()
	for i := 0; i < count; i++ {
		kOff, kLen, vOff, vLen := s.readEntry(i)
		if int(kLen) != len(key) {
			continue
		}
		storedKey := unsafe.String(&s.mapped[kOff], int(kLen))
		if storedKey == key {
			// Zero out the key length; the entry is now invisible to Get.
			s.writeEntry(i, kOff, 0, vOff, vLen)
			return nil
		}
	}
	return ErrNotFound
}

// Len returns the number of non-deleted entries.
func (s *MMapStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := s.entryCount()
	live := 0
	for i := 0; i < count; i++ {
		_, kLen, _, _ := s.readEntry(i)
		if kLen > 0 {
			live++
		}
	}
	return live
}

// Range calls fn for each live key-value pair. If fn returns false, iteration stops.
// The key and value strings point into the mapped region.
func (s *MMapStore) Range(fn func(key, value string) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	count := s.entryCount()
	for i := 0; i < count; i++ {
		kOff, kLen, vOff, vLen := s.readEntry(i)
		if kLen == 0 {
			continue
		}
		k := unsafe.String(&s.mapped[kOff], int(kLen))
		v := unsafe.String(&s.mapped[vOff], int(vLen))
		if !fn(k, v) {
			return
		}
	}
}

// Sync flushes dirty pages to disk via msync.
func (s *MMapStore) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.checkClosed(); err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_MSYNC,
		uintptr(unsafe.Pointer(&s.mapped[0])),
		uintptr(len(s.mapped)),
		uintptr(syscall.MS_SYNC),
	)
	if errno != 0 {
		return fmt.Errorf("mmapstore: msync: %w", errno)
	}
	return nil
}

// Close flushes and unmaps the region, then closes the file.
func (s *MMapStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	var errs []error
	if s.mapped != nil {
		if err := syscall.Munmap(s.mapped); err != nil {
			errs = append(errs, fmt.Errorf("munmap: %w", err))
		}
		s.mapped = nil
	}
	if err := s.f.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close fd: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("mmapstore: close: %v", errs)
	}
	return nil
}
```

### Exercise 2: Tests

Create `mmapstore_test.go`:

```go
package mmapstore

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *MMapStore {
	t.Helper()
	path := t.TempDir() + "/test.mmap"
	s, err := Open(path, 64, 4096)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPutGet(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	if err := s.Put("hello", "world"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("hello")
	if err != nil {
		t.Fatal(err)
	}
	if got != "world" {
		t.Fatalf("Get = %q, want world", got)
	}
}

func TestGetMissing(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	_, err := s.Get("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	if err := s.Put("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("k", "v2"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v2" {
		t.Fatalf("Get after update = %q, want v2", got)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	if err := s.Put("del", "gone"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("del"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("del"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
}

func TestDeleteMissing(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	if err := s.Delete("nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestLen(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	for i := 0; i < 5; i++ {
		if err := s.Put(fmt.Sprintf("k%d", i), "v"); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.Len(); got != 5 {
		t.Fatalf("Len = %d, want 5", got)
	}
	if err := s.Delete("k2"); err != nil {
		t.Fatal(err)
	}
	if got := s.Len(); got != 4 {
		t.Fatalf("Len after delete = %d, want 4", got)
	}
}

func TestPersistence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := dir + "/persist.mmap"

	s, err := Open(path, 64, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("persist", "data"); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, 64, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.Get("persist")
	if err != nil {
		t.Fatal(err)
	}
	if got != "data" {
		t.Fatalf("after reopen Get = %q, want data", got)
	}
}

func TestGrowth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Start with a very small data region to force remap.
	s, err := Open(dir+"/grow.mmap", 128, 32)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Write enough data to exceed the initial 32-byte data region.
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key%d", i)
		val := fmt.Sprintf("value%d-%s", i, "padding-to-grow-the-file")
		if err := s.Put(key, val); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	// Verify all entries are readable after growth.
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key%d", i)
		want := fmt.Sprintf("value%d-%s", i, "padding-to-grow-the-file")
		got, err := s.Get(key)
		if err != nil {
			t.Fatalf("Get(%q): %v", key, err)
		}
		if got != want {
			t.Fatalf("Get(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestConcurrentReadersWriter(t *testing.T) {
	t.Parallel()
	s := openTemp(t)

	// Pre-populate.
	for i := 0; i < 10; i++ {
		if err := s.Put(fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	// 20 concurrent readers.
	for r := 0; r < 20; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				key := fmt.Sprintf("k%d", i%10)
				_, _ = s.Get(key)
			}
		}(r)
	}
	// 1 concurrent writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_ = s.Put(fmt.Sprintf("w%d", i), "written")
		}
	}()

	wg.Wait()
}

func TestClosedStore(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/closed.mmap"
	s, err := Open(path, 64, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// All operations on a closed store must return ErrClosed.
	if _, err := s.Get("k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get on closed: err = %v, want ErrClosed", err)
	}
	if err := s.Put("k", "v"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put on closed: err = %v, want ErrClosed", err)
	}
	if err := s.Delete("k"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Delete on closed: err = %v, want ErrClosed", err)
	}
	// Double Close must not panic.
	if err := s.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func BenchmarkGet(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir+"/bench.mmap", 64, 65536)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	if err := s.Put("benchkey", "benchvalue"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Get("benchkey")
	}
}

func BenchmarkPut(b *testing.B) {
	dir := b.TempDir()
	s, err := Open(dir+"/bench.mmap", 8192, 1<<20)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("k%d", i)
		_ = s.Put(key, "value")
	}
}
```

Your turn: add `TestRange` that puts three entries, calls `Range` to collect all keys, and asserts all three keys appear in the result.

### Exercise 3: Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"example.com/mmapstore"
)

func main() {
	dir, err := os.MkdirTemp("", "mmapstore-demo-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "MkdirTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "demo.mmap")
	s, err := mmapstore.Open(path, 64, 4096)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Open: %v\n", err)
		os.Exit(1)
	}

	_ = s.Put("language", "Go")
	_ = s.Put("topic", "unsafe")
	_ = s.Put("version", "1.26")

	fmt.Printf("Len: %d\n", s.Len())

	s.Range(func(k, v string) bool {
		fmt.Printf("  %s = %s\n", k, v)
		return true
	})

	_ = s.Sync()
	_ = s.Close()

	// Reopen and verify persistence.
	s2, _ := mmapstore.Open(path, 64, 4096)
	defer s2.Close()
	val, _ := s2.Get("language")
	fmt.Printf("After reopen: language = %q\n", val)
}
```

Run with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Dereferencing a Pointer After Munmap

Wrong:

```go
ptr := (*uint32)(unsafe.Pointer(&s.mapped[0]))
s.remap(newSize) // s.mapped is replaced; old backing is gone
fmt.Println(*ptr) // reads freed (or remapped) memory: undefined behavior / SIGSEGV
```

Fix: never stash `unsafe.Pointer` values derived from `s.mapped`. Recompute them from the current `s.mapped` at each call site. The implementation above follows this rule: every accessor reads `s.mapped[offset]` at call time rather than caching a pointer.

### Not Holding the Write Lock During Remap

Wrong:

```go
// Write goroutine calls remap without holding the lock.
// A concurrent reader holds the read lock and dereferences the old mapping
// while the write goroutine unmaps it.
```

Fix: remap must happen under the write lock. `appendData` is called only from `Put`, which holds `s.mu.Lock()`.

### Forgetting to Call Sync Before Close

Wrong:

```go
s.Put("k", "v")
s.Close() // pages may still be in the OS page cache; a crash now loses data
```

Fix: call `s.Sync()` before `s.Close()` when durability matters. Document this requirement clearly; it is not crash-safe by default (WAL or copy-on-write would be needed for that).

### Linear Index Scan in Production

The implementation uses a linear scan for `Get` and `Delete`. This is correct for the exercise, but for a store with thousands of entries, a B-tree or hash index is needed. bbolt uses a B+ tree. LMDB uses a copy-on-write B-tree. Comment this limitation rather than silently accepting O(n) behavior.

## Verification

From `~/go-exercises/mmapstore`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go test -bench=Benchmark -benchmem ./...
```

All five must pass with the race detector clean. The `TestPersistence` test proves data survives close and reopen. The `TestGrowth` test proves remap works transparently. The `BenchmarkGet` test should show `0 allocs/op` because `Get` returns a string backed by the mapped region.

Note: `Sync` uses `syscall.SYS_MSYNC` directly because the `syscall.Mmap` API does not expose a corresponding `Msync` wrapper on all platforms. On Linux and macOS this is available and stable.

## Summary

- `syscall.Mmap` returns a `[]byte` backed by a file; every access to this slice is a direct memory read or write with no `read`/`write` syscalls.
- `unsafe.String(&mapped[off], n)` creates a zero-copy string view into the mapped region. The string is valid only while the mapping is alive.
- After `syscall.Munmap`, all pointers derived from the old mapping are invalid. Never cache them across a remap.
- A remap must happen under the write lock to prevent concurrent readers from dereferencing the unmapped region.
- `msync` flushes dirty pages to disk. The store is not crash-safe without additional WAL or copy-on-write logic.
- This architecture (mmap + linear index) is the conceptual base for LMDB and bbolt, which replace the linear scan with a B+ tree.

## What's Next

Next: [Exercise 29.1: go generate Basics](../../29-code-generation-and-build-system/01-go-generate-basics/01-go-generate-basics.md).

## Resources

- [syscall.Mmap](https://pkg.go.dev/syscall#Mmap)
- [LMDB architecture](http://www.lmdb.tech/doc/) — the gold standard for mmap-backed key-value stores
- [bbolt (etcd)](https://github.com/etcd-io/bbolt) — Go's mmap-backed B+ tree database
- [unsafe.String](https://pkg.go.dev/unsafe#String) — zero-copy string from a mapped region
- [mmap(2) man page](https://man7.org/linux/man-pages/man2/mmap.2.html)
