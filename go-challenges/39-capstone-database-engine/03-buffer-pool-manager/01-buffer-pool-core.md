# Exercise 1: Buffer Pool Core

The buffer pool is the layer that lets a database touch far more data than fits in RAM without paying a disk read on every access. It maps on-disk pages to a fixed set of in-memory frames, hands callers a pinned, lock-protected view of a page, evicts the right victim when the pool is full, and writes dirty pages back through the WAL-before-page rule. This exercise builds that core end to end: the types and constructor, `FetchPage` with clock-sweep eviction, the `UnpinPage`/`FlushPage`/`FlushAll`/`NewPage`/`DeletePage` surface, the `PageGuard` that makes unpinning hard to forget, and a test suite that pins every invariant down — pinned frames survive eviction, dirty pages are written back, the WAL is forced before a page write, and concurrent fetch/unpin is race-free.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bufferpool.go            PageID, FrameID, LSN, Page, DiskManager, WALFlusher,
                         BufferPool, New, PageGuard, FetchPage, clockSweep,
                         writeBack, UnpinPage, FlushPage, FlushAll, NewPage, DeletePage
cmd/
  demo/
    main.go              allocate pages, re-read a cached page, FlushAll
bufferpool_test.go       cache hit, eviction, pinned-not-evicted, pool exhausted,
                         dirty write-back, WAL ordering, concurrent fetch/unpin under -race
```

- Files: `bufferpool.go`, `cmd/demo/main.go`, `bufferpool_test.go`.
- Implement: the types and sentinel errors, `New`, the `PageGuard` methods, and the `BufferPool` methods `FetchPage`, `UnpinPage`, `FlushPage`, `FlushAll`, `NewPage`, `DeletePage`, with `clockSweep` and `writeBack` as unexported helpers.
- Test: `bufferpool_test.go` exercises cache hits, eviction under pressure, the pinned-frame invariant, the pool-exhausted error, dirty write-back, WAL-before-page ordering, and a concurrent fetch/unpin stress loop.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p buffer-pool-core/cmd/demo && cd buffer-pool-core
go mod init example.com/buffer-pool-core
```

### The shape of the pool: frames, the page table, and the guard

A `BufferPool` is a fixed slice of `frame` structs plus the metadata that tracks them. The page table (`map[PageID]FrameID`) answers "is this page resident, and where?" in O(1). The free list is a stack of frame indices that hold no page yet, so a miss prefers a never-used frame over evicting a live one. The clock hand is the rotating cursor the eviction sweep advances. Every one of these fields is protected by a single pool-level `sync.Mutex`, because they are all small and mutated together on the hot path.

Each `frame` additionally owns a `sync.RWMutex` that guards only its page bytes. This is the second latch tier: the pool mutex protects metadata and is held briefly; the frame mutex protects contents and is held across the caller's possibly-long data access. Keeping them separate is what lets many goroutines read different frames at once without serializing on one global lock. The `frame` type is unexported — external code never sees a frame directly. It interacts only through a `*PageGuard`, which holds a pointer to the pinned frame and carries the `Unpin` method plus the frame-latch accessors. A guard is the buffer-pool analog of an open file handle: while you hold it the frame cannot be repurposed, and the `defer guard.Unpin(...)` you write on the line after the fetch is the analog of `defer f.Close()`.

The sentinel errors are created with `errors.New` so callers assert with `errors.Is` rather than string matching. `ErrPoolExhausted` means every frame is pinned and nothing can be evicted; `ErrNotPinned` and `ErrPagePinned` guard the unpin and delete contracts; `ErrInvalidPageID` rejects the reserved zero page id.

Create `bufferpool.go`:

```go
package bufferpool

import (
	"errors"
	"fmt"
	"sync"
)

// PageSize is the fixed size in bytes of one database page.
const PageSize = 4096

// PageID identifies a page on disk. Zero is reserved as InvalidPageID.
type PageID uint64

// FrameID identifies a slot in the buffer pool. -1 means no frame.
type FrameID int

// LSN is a Log Sequence Number that identifies a WAL record.
type LSN uint64

const (
	InvalidPageID  PageID  = 0
	InvalidFrameID FrameID = -1
)

// Sentinel errors — callers assert with errors.Is.
var (
	ErrPoolExhausted = errors.New("bufferpool: all frames are pinned")
	ErrPageNotFound  = errors.New("bufferpool: page not in pool")
	ErrNotPinned     = errors.New("bufferpool: page is not pinned")
	ErrPagePinned    = errors.New("bufferpool: page is still pinned")
	ErrInvalidPageID = errors.New("bufferpool: invalid page id")
)

// DiskManager abstracts page-level random I/O so the buffer pool can be
// tested without a real file system.
type DiskManager interface {
	ReadPage(pageID PageID, buf []byte) error
	WritePage(pageID PageID, buf []byte) error
	AllocatePage() (PageID, error)
	FreePage(pageID PageID) error
}

// WALFlusher ensures WAL records are durable up to a given LSN.
// Pass nil to New to disable the WAL-before-page check (useful in tests).
type WALFlusher interface {
	// FlushedLSN returns the LSN of the last WAL record written to durable storage.
	FlushedLSN() LSN
	// FlushUpTo ensures all WAL records up to and including lsn are durable.
	FlushUpTo(lsn LSN) error
}

// Page holds one page's worth of raw data.
type Page [PageSize]byte

// frame is one slot in the buffer pool. All fields are protected by the
// pool-level mutex except data, which is additionally protected by mu for
// callers that require concurrent read access to page contents.
type frame struct {
	mu       sync.RWMutex
	pageID   PageID
	data     Page
	pinCount int
	dirty    bool
	refBit   bool // clock-sweep second-chance bit
	pageLSN  LSN  // LSN of the last WAL record that modified this page
}

// BufferPool manages a fixed pool of in-memory page frames.
type BufferPool struct {
	mu        sync.Mutex
	frames    []frame
	pageTable map[PageID]FrameID
	freeList  []FrameID
	clockHand int
	dm        DiskManager
	wal       WALFlusher
}

// New returns a BufferPool with poolSize frames backed by dm.
// wal may be nil to disable WAL-before-page write enforcement.
func New(poolSize int, dm DiskManager, wal WALFlusher) (*BufferPool, error) {
	if poolSize < 1 {
		return nil, fmt.Errorf("bufferpool: pool size must be >= 1, got %d", poolSize)
	}
	if dm == nil {
		return nil, errors.New("bufferpool: DiskManager must not be nil")
	}
	free := make([]FrameID, poolSize)
	for i := range free {
		free[i] = FrameID(i)
	}
	return &BufferPool{
		frames:    make([]frame, poolSize),
		pageTable: make(map[PageID]FrameID, poolSize),
		freeList:  free,
		dm:        dm,
		wal:       wal,
	}, nil
}

// Size returns the number of frames in the pool.
func (bp *BufferPool) Size() int {
	return len(bp.frames)
}

// PageGuard holds a pinned frame. Call Unpin exactly once when done.
type PageGuard struct {
	bp     *BufferPool
	pageID PageID
	frame  *frame
}

// Data returns a pointer to the frame's page buffer.
// The caller must hold the frame lock (RLock/Lock) while reading or writing.
func (g *PageGuard) Data() *Page { return &g.frame.data }

// PageID returns the on-disk identifier of the held page.
func (g *PageGuard) PageID() PageID { return g.pageID }

// SetLSN records that lsn is the most recent WAL record affecting this page.
// Must be called after every modification covered by a WAL record.
func (g *PageGuard) SetLSN(lsn LSN) {
	g.bp.mu.Lock()
	g.frame.pageLSN = lsn
	g.bp.mu.Unlock()
}

// RLock, RUnlock, Lock, Unlock expose the per-frame mutex so callers can
// achieve concurrent read access without holding the pool lock.
func (g *PageGuard) RLock()   { g.frame.mu.RLock() }
func (g *PageGuard) RUnlock() { g.frame.mu.RUnlock() }
func (g *PageGuard) Lock()    { g.frame.mu.Lock() }
func (g *PageGuard) Unlock()  { g.frame.mu.Unlock() }

// Unpin decrements the pin count. Pass isDirty=true if the caller wrote to
// the page. Returns an error if the page is not in the pool or the pin count
// is already zero.
func (g *PageGuard) Unpin(isDirty bool) error {
	return g.bp.UnpinPage(g.pageID, isDirty)
}
```

### FetchPage and clock-sweep eviction

`FetchPage` is the hot path and the trickiest piece of concurrency in the pool. The cache-hit case is trivial: under the pool lock, look the page up, bump its pin count, set its reference bit, and return a guard. The cache-miss case is where the design earns its keep.

On a miss the method must read from disk, and a disk read must not happen with the pool mutex held — otherwise one slow read freezes every other goroutine. The sequence is: claim a victim frame via `clockSweep` while holding the lock, immediately set its pin count to 1 and mark its page id invalid (a "being loaded" sentinel), then release the lock and call `ReadPage` outside it. Pinning the frame before releasing the lock is the load-bearing move: a concurrent sweep skips any frame with a positive pin count, so no other goroutine can steal the frame mid-read.

After the read, the method re-acquires the lock and re-checks the page table. Two goroutines can miss for the same page at the same time; the second one to come back finds the page already loaded by the first, returns its own claimed frame to the free list, pins the existing frame, and hands back a guard to it. Without this re-check the same page could occupy two frames, breaking the page table's one-to-one invariant.

`clockSweep` prefers a free frame when one exists — no eviction needed — and otherwise runs the two-sweep clock algorithm: skip pinned frames, give a second chance to any frame whose reference bit is set (clearing it), and evict the first frame whose reference bit is already clear, writing it back first if dirty. If a dirty frame's write-back fails it is skipped rather than evicted, so a transient I/O error on one page does not abort the whole fetch. `writeBack` is the single choke point for the WAL-before-page rule: if the frame's pageLSN exceeds the WAL's flushed frontier, it forces the log forward before issuing the page write, then clears the dirty bit.

Add to `bufferpool.go`:

```go
// FetchPage pins pageID and returns a PageGuard. The caller must call
// guard.Unpin when finished. FetchPage is safe for concurrent use.
func (bp *BufferPool) FetchPage(pageID PageID) (*PageGuard, error) {
	if pageID == InvalidPageID {
		return nil, ErrInvalidPageID
	}

	bp.mu.Lock()

	// Cache hit: page already in pool.
	if fid, ok := bp.pageTable[pageID]; ok {
		f := &bp.frames[fid]
		f.pinCount++
		f.refBit = true
		bp.mu.Unlock()
		return &PageGuard{bp: bp, pageID: pageID, frame: f}, nil
	}

	// Cache miss: claim a victim frame. Pin it before releasing the lock so
	// no concurrent clockSweep can claim the same frame while we do I/O.
	fid, err := bp.clockSweep()
	if err != nil {
		bp.mu.Unlock()
		return nil, err
	}
	f := &bp.frames[fid]
	f.pinCount = 1
	f.pageID = InvalidPageID // sentinel: being loaded
	bp.mu.Unlock()

	// Disk read outside the pool lock. A slow read must not block other goroutines.
	if err := bp.dm.ReadPage(pageID, f.data[:]); err != nil {
		bp.mu.Lock()
		f.pinCount = 0
		bp.freeList = append(bp.freeList, fid)
		bp.mu.Unlock()
		return nil, fmt.Errorf("bufferpool: read page %d: %w", pageID, err)
	}

	bp.mu.Lock()
	// Re-check: a concurrent goroutine may have loaded the same page.
	if fid2, ok := bp.pageTable[pageID]; ok {
		f.pinCount = 0
		bp.freeList = append(bp.freeList, fid)
		f2 := &bp.frames[fid2]
		f2.pinCount++
		f2.refBit = true
		bp.mu.Unlock()
		return &PageGuard{bp: bp, pageID: pageID, frame: f2}, nil
	}
	f.pageID = pageID
	f.dirty = false
	f.refBit = true
	f.pageLSN = 0
	bp.pageTable[pageID] = fid
	bp.mu.Unlock()

	return &PageGuard{bp: bp, pageID: pageID, frame: f}, nil
}

// clockSweep finds a victim frame using the clock-sweep algorithm and returns
// its index after removing its old page table entry. Must be called with
// bp.mu held.
func (bp *BufferPool) clockSweep() (FrameID, error) {
	// Prefer a free frame — no eviction needed.
	if len(bp.freeList) > 0 {
		fid := bp.freeList[len(bp.freeList)-1]
		bp.freeList = bp.freeList[:len(bp.freeList)-1]
		return fid, nil
	}

	n := len(bp.frames)
	// Two full sweeps: sweep 1 clears reference bits; sweep 2 finds a victim.
	for swept := 0; swept < 2*n; swept++ {
		idx := bp.clockHand % n
		bp.clockHand = (bp.clockHand + 1) % n
		f := &bp.frames[idx]

		if f.pinCount > 0 {
			continue
		}
		if f.refBit {
			f.refBit = false
			continue
		}
		if f.dirty {
			if err := bp.writeBack(f); err != nil {
				// Cannot evict this frame right now; try the next one.
				continue
			}
		}
		delete(bp.pageTable, f.pageID)
		return FrameID(idx), nil
	}
	return InvalidFrameID, ErrPoolExhausted
}

// writeBack writes f's data to disk enforcing the WAL-before-page rule.
// Caller must hold bp.mu.
func (bp *BufferPool) writeBack(f *frame) error {
	if bp.wal != nil && f.pageLSN > 0 && bp.wal.FlushedLSN() < f.pageLSN {
		if err := bp.wal.FlushUpTo(f.pageLSN); err != nil {
			return fmt.Errorf("bufferpool: wal flush to lsn %d: %w", f.pageLSN, err)
		}
	}
	if err := bp.dm.WritePage(f.pageID, f.data[:]); err != nil {
		return fmt.Errorf("bufferpool: write page %d: %w", f.pageID, err)
	}
	f.dirty = false
	return nil
}
```

### Unpin, flush, allocate, and delete

The remaining surface manages the lifecycle of a pinned page. `UnpinPage` is the partner to every fetch: it decrements the pin count and, if the caller wrote to the page, sets the dirty bit. It returns `ErrPageNotFound` if the page is not resident and `ErrNotPinned` if the count is already zero, so a double unpin is caught rather than silently driving the count negative.

`FlushPage` writes one resident dirty page back through `writeBack`; a clean or absent page is a no-op. `FlushAll` walks every frame and joins per-page errors with `errors.Join`, so one failed page does not hide the others. `NewPage` allocates a fresh page id from the disk manager, loads a zero-filled frame for it, pins it, and marks it dirty so the empty page is persisted on first eviction; if no frame is available it rolls back the allocation. `DeletePage` returns a page to the disk free list and removes it from the pool, refusing with `ErrPagePinned` if anyone still holds it.

Add to `bufferpool.go`:

```go
// UnpinPage decrements the pin count for pageID. Pass isDirty=true if the
// caller modified the page. Returns ErrPageNotFound or ErrNotPinned on misuse.
func (bp *BufferPool) UnpinPage(pageID PageID, isDirty bool) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	fid, ok := bp.pageTable[pageID]
	if !ok {
		return fmt.Errorf("%w: pageID %d", ErrPageNotFound, pageID)
	}
	f := &bp.frames[fid]
	if f.pinCount == 0 {
		return fmt.Errorf("%w: pageID %d", ErrNotPinned, pageID)
	}
	f.pinCount--
	if isDirty {
		f.dirty = true
	}
	return nil
}

// FlushPage writes pageID's dirty frame to disk. If the page is not in the
// pool or is not dirty, FlushPage returns nil.
func (bp *BufferPool) FlushPage(pageID PageID) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	fid, ok := bp.pageTable[pageID]
	if !ok {
		return nil
	}
	f := &bp.frames[fid]
	if !f.dirty {
		return nil
	}
	return bp.writeBack(f)
}

// FlushAll flushes every dirty frame in the pool. Errors from individual
// pages are joined and returned together (errors.Join, Go 1.20+).
func (bp *BufferPool) FlushAll() error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	var errs []error
	for i := range bp.frames {
		f := &bp.frames[i]
		if f.dirty {
			if err := bp.writeBack(f); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// NewPage allocates a fresh page on disk, loads it into the pool, and returns
// its PageID and a PageGuard. The caller must call guard.Unpin when done.
func (bp *BufferPool) NewPage() (PageID, *PageGuard, error) {
	pageID, err := bp.dm.AllocatePage()
	if err != nil {
		return InvalidPageID, nil, fmt.Errorf("bufferpool: allocate page: %w", err)
	}

	bp.mu.Lock()
	fid, err := bp.clockSweep()
	if err != nil {
		bp.mu.Unlock()
		_ = bp.dm.FreePage(pageID) // roll back the allocation
		return InvalidPageID, nil, err
	}
	f := &bp.frames[fid]
	f.pageID = pageID
	f.data = Page{} // zero-fill
	f.pinCount = 1
	f.dirty = true // mark dirty so it is written on first eviction
	f.refBit = true
	f.pageLSN = 0
	bp.pageTable[pageID] = fid
	bp.mu.Unlock()

	return pageID, &PageGuard{bp: bp, pageID: pageID, frame: f}, nil
}

// DeletePage removes pageID from the pool and returns it to the disk free list.
// Returns ErrPagePinned if the page is currently pinned.
func (bp *BufferPool) DeletePage(pageID PageID) error {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	if fid, ok := bp.pageTable[pageID]; ok {
		f := &bp.frames[fid]
		if f.pinCount > 0 {
			return fmt.Errorf("%w: pageID %d", ErrPagePinned, pageID)
		}
		f.dirty = false
		f.pinCount = 0
		f.refBit = false
		delete(bp.pageTable, pageID)
		bp.freeList = append(bp.freeList, fid)
	}
	return bp.dm.FreePage(pageID)
}
```

### The runnable demo

The demo drives the exported API from a separate `package main`, which is the real test that the surface is usable from outside the package. It allocates three pages writing a distinct marker byte into each, re-reads page 1 as a cache hit to show the marker survived, then flushes everything. It uses a tiny in-memory `DiskManager`; the pool never knows the difference.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"sync"

	"example.com/buffer-pool-core"
)

// memDM is a minimal in-memory DiskManager for the demo.
type memDM struct {
	mu    sync.Mutex
	pages map[bufferpool.PageID]bufferpool.Page
	next  bufferpool.PageID
}

func newDM() *memDM {
	return &memDM{
		pages: make(map[bufferpool.PageID]bufferpool.Page),
		next:  1,
	}
}

func (m *memDM) ReadPage(id bufferpool.PageID, buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pages[id]
	copy(buf, p[:])
	return nil
}

func (m *memDM) WritePage(id bufferpool.PageID, buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p bufferpool.Page
	copy(p[:], buf)
	m.pages[id] = p
	return nil
}

func (m *memDM) AllocatePage() (bufferpool.PageID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	return id, nil
}

func (m *memDM) FreePage(bufferpool.PageID) error { return nil }

func main() {
	dm := newDM()
	bp, err := bufferpool.New(4, dm, nil)
	if err != nil {
		log.Fatalf("New: %v", err)
	}
	fmt.Printf("buffer pool: %d frames\n", bp.Size())

	// Allocate and write three pages.
	for i := 0; i < 3; i++ {
		pid, g, err := bp.NewPage()
		if err != nil {
			log.Fatalf("NewPage: %v", err)
		}
		g.Lock()
		g.Data()[0] = byte(i + 1)
		g.Unlock()
		fmt.Printf("  allocated page %d, wrote marker %#x\n", pid, byte(i+1))
		if err := g.Unpin(true); err != nil {
			log.Fatalf("Unpin page %d: %v", pid, err)
		}
	}

	// Read page 1 back (cache hit: still in pool).
	guard, err := bp.FetchPage(1)
	if err != nil {
		log.Fatalf("FetchPage(1): %v", err)
	}
	guard.RLock()
	marker := guard.Data()[0]
	guard.RUnlock()
	fmt.Printf("  re-read page 1: marker = %#x\n", marker)
	guard.Unpin(false)

	if err := bp.FlushAll(); err != nil {
		log.Fatalf("FlushAll: %v", err)
	}
	fmt.Println("flush complete")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buffer pool: 4 frames
  allocated page 1, wrote marker 0x1
  allocated page 2, wrote marker 0x2
  allocated page 3, wrote marker 0x3
  re-read page 1: marker = 0x1
flush complete
```

### Tests

The tests are the real verification — there is no behavior to eyeball here, only invariants to assert. They use an in-memory `DiskManager` with a `ReadDirect` escape hatch so a test can read what actually reached disk, and a controllable `stubWAL` that records how many times the log was forced. The cases cover constructor validation, cache hits keeping the pin count exact, eviction holding the pool at capacity, the pinned-frame-survives invariant, the pool-exhausted error, the unpin error contract, dirty write-back reaching disk, WAL-before-page ordering, new/delete, and a 50-goroutine fetch/unpin stress loop that must stay clean under `-race`.

Create `bufferpool_test.go`:

```go
package bufferpool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// memDiskManager is an in-memory DiskManager for tests. It is safe for
// concurrent use.
type memDiskManager struct {
	mu    sync.Mutex
	pages map[PageID]Page
	next  PageID
	freed []PageID
}

func newMemDM() *memDiskManager {
	return &memDiskManager{
		pages: make(map[PageID]Page),
		next:  1, // 0 is InvalidPageID
	}
}

func (m *memDiskManager) ReadPage(pageID PageID, buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.pages[pageID] // zero value if the page has never been written
	copy(buf, p[:])
	return nil
}

func (m *memDiskManager) WritePage(pageID PageID, buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p Page
	copy(p[:], buf)
	m.pages[pageID] = p
	return nil
}

func (m *memDiskManager) AllocatePage() (PageID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.freed) > 0 {
		id := m.freed[len(m.freed)-1]
		m.freed = m.freed[:len(m.freed)-1]
		return id, nil
	}
	id := m.next
	m.next++
	return id, nil
}

func (m *memDiskManager) FreePage(pageID PageID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pages, pageID)
	m.freed = append(m.freed, pageID)
	return nil
}

// ReadDirect reads the stored page from the memDiskManager directly,
// bypassing the buffer pool. Used to verify write-back.
func (m *memDiskManager) ReadDirect(pageID PageID) Page {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pages[pageID]
}

// stubWAL is a controllable WALFlusher for tests.
type stubWAL struct {
	mu         sync.Mutex
	flushed    LSN
	flushCalls int
}

func (w *stubWAL) FlushedLSN() LSN {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushed
}

func (w *stubWAL) FlushUpTo(lsn LSN) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushCalls++
	if lsn > w.flushed {
		w.flushed = lsn
	}
	return nil
}

// -- Tests --

func TestNewRejectsInvalidArgs(t *testing.T) {
	t.Parallel()

	dm := newMemDM()
	cases := []struct {
		name    string
		size    int
		dm      DiskManager
		wantErr bool
	}{
		{"zero size", 0, dm, true},
		{"negative size", -1, dm, true},
		{"nil dm", 4, nil, true},
		{"valid", 4, dm, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.size, tc.dm, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New(%d, dm=%v): err=%v, wantErr=%v", tc.size, tc.dm, err, tc.wantErr)
			}
		})
	}
}

func TestFetchPageCacheHit(t *testing.T) {
	t.Parallel()

	bp, _ := New(4, newMemDM(), nil)

	g1, err := bp.FetchPage(1)
	if err != nil {
		t.Fatalf("first FetchPage: %v", err)
	}
	g1.Unpin(false)

	g2, err := bp.FetchPage(1)
	if err != nil {
		t.Fatalf("second FetchPage: %v", err)
	}
	defer g2.Unpin(false)

	bp.mu.Lock()
	fid, ok := bp.pageTable[1]
	if !ok {
		bp.mu.Unlock()
		t.Fatal("page 1 not in pool")
	}
	pc := bp.frames[fid].pinCount
	bp.mu.Unlock()

	if pc != 1 {
		t.Fatalf("pinCount = %d after one fetch and one unpin, want 1", pc)
	}
}

func TestFetchPageInvalidID(t *testing.T) {
	t.Parallel()

	bp, _ := New(4, newMemDM(), nil)
	_, err := bp.FetchPage(InvalidPageID)
	if !errors.Is(err, ErrInvalidPageID) {
		t.Fatalf("err = %v, want ErrInvalidPageID", err)
	}
}

func TestEvictionUnderPressure(t *testing.T) {
	t.Parallel()

	// Pool size 3; access 6 different pages sequentially (all unpinned after use).
	// Eviction must occur after the pool is full.
	bp, _ := New(3, newMemDM(), nil)

	for id := PageID(1); id <= 6; id++ {
		g, err := bp.FetchPage(id)
		if err != nil {
			t.Fatalf("FetchPage(%d): %v", id, err)
		}
		g.Unpin(false)
	}

	bp.mu.Lock()
	inPool := len(bp.pageTable)
	bp.mu.Unlock()
	if inPool != 3 {
		t.Fatalf("pool has %d pages, want 3", inPool)
	}
}

func TestPinnedFrameNotEvicted(t *testing.T) {
	t.Parallel()

	// Pool size 2. Pin page 1, fill with pages 2 and 3. Page 1 must survive.
	bp, _ := New(2, newMemDM(), nil)

	pinned, err := bp.FetchPage(1)
	if err != nil {
		t.Fatalf("FetchPage(1): %v", err)
	}
	// Intentionally do NOT unpin page 1 yet.

	g2, _ := bp.FetchPage(2)
	g2.Unpin(false)

	// Pool has page 1 (pinned) and page 2 (refBit=1 after FetchPage).
	// First clockSweep sweep clears page 2's refBit; second evicts page 2.
	g3, err := bp.FetchPage(3)
	if err != nil {
		t.Fatalf("FetchPage(3): %v", err)
	}
	defer g3.Unpin(false)

	bp.mu.Lock()
	_, page1Present := bp.pageTable[1]
	bp.mu.Unlock()

	if !page1Present {
		t.Fatal("pinned page 1 was evicted — invariant violated")
	}

	pinned.Unpin(false)
}

func TestPoolExhaustedError(t *testing.T) {
	t.Parallel()

	bp, _ := New(2, newMemDM(), nil)
	g1, _ := bp.FetchPage(1)
	g2, _ := bp.FetchPage(2)

	_, err := bp.FetchPage(3)
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("err = %v, want ErrPoolExhausted", err)
	}

	g1.Unpin(false)
	g2.Unpin(false)
}

func TestUnpinErrors(t *testing.T) {
	t.Parallel()

	bp, _ := New(4, newMemDM(), nil)

	// Page not in pool.
	if err := bp.UnpinPage(99, false); !errors.Is(err, ErrPageNotFound) {
		t.Fatalf("not-in-pool: err = %v, want ErrPageNotFound", err)
	}

	g, _ := bp.FetchPage(1)
	g.Unpin(false)

	// Pin count already zero.
	if err := bp.UnpinPage(1, false); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("zero-pin: err = %v, want ErrNotPinned", err)
	}
}

func TestDirtyPageWriteBack(t *testing.T) {
	t.Parallel()

	dm := newMemDM()
	bp, _ := New(2, dm, nil)

	// Fetch and dirty page 1.
	g, _ := bp.FetchPage(1)
	g.Lock()
	g.Data()[0] = 0xAB
	g.Unlock()
	g.Unpin(true) // dirty=true

	// Fill pool: eviction of page 1 or 2 must trigger write-back.
	g2, _ := bp.FetchPage(2)
	g2.Unpin(false)
	g3, _ := bp.FetchPage(3)
	g3.Unpin(false)

	// Flush explicitly to ensure write-back if clock sweep chose page 2 first.
	bp.FlushPage(1)

	p := dm.ReadDirect(1)
	if p[0] != 0xAB {
		t.Fatalf("disk page 1 byte[0] = %#x, want 0xAB — dirty write-back failed", p[0])
	}
}

func TestFlushAllWritesDirtyPages(t *testing.T) {
	t.Parallel()

	dm := newMemDM()
	bp, _ := New(4, dm, nil)

	// Dirty all four pages; the pool holds exactly four frames so none is evicted.
	for id := PageID(1); id <= 4; id++ {
		g, err := bp.FetchPage(id)
		if err != nil {
			t.Fatalf("FetchPage(%d): %v", id, err)
		}
		g.Lock()
		g.Data()[0] = byte(id)
		g.Unlock()
		if err := g.Unpin(true); err != nil {
			t.Fatalf("Unpin(%d): %v", id, err)
		}
	}

	if err := bp.FlushAll(); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	for id := PageID(1); id <= 4; id++ {
		if got := dm.ReadDirect(id)[0]; got != byte(id) {
			t.Fatalf("disk page %d byte[0] = %#x, want %#x", id, got, byte(id))
		}
	}
}

func TestWALFlushOrderingEnforced(t *testing.T) {
	t.Parallel()

	dm := newMemDM()
	wal := &stubWAL{flushed: 0}
	bp, _ := New(4, dm, wal)

	// Fetch page 1 and set pageLSN=10. WAL flushedLSN is 0 < 10.
	g, _ := bp.FetchPage(1)
	g.SetLSN(10)
	g.Unpin(true) // dirty

	// FlushPage must call wal.FlushUpTo(10) before writing the page.
	if err := bp.FlushPage(1); err != nil {
		t.Fatalf("FlushPage: %v", err)
	}

	wal.mu.Lock()
	calls := wal.flushCalls
	flushed := wal.flushed
	wal.mu.Unlock()

	if calls == 0 {
		t.Fatal("WAL.FlushUpTo was not called before writing the dirty page — protocol violated")
	}
	if flushed < 10 {
		t.Fatalf("WAL flushedLSN = %d after flush, want >= 10", flushed)
	}
}

func TestNewPageAndDeletePage(t *testing.T) {
	t.Parallel()

	dm := newMemDM()
	bp, _ := New(4, dm, nil)

	pid, g, err := bp.NewPage()
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	if pid == InvalidPageID {
		t.Fatal("NewPage returned InvalidPageID")
	}

	g.Lock()
	g.Data()[0] = 0xFF
	g.Unlock()
	g.Unpin(true)

	if err := bp.DeletePage(pid); err != nil {
		t.Fatalf("DeletePage: %v", err)
	}

	bp.mu.Lock()
	_, inPool := bp.pageTable[pid]
	bp.mu.Unlock()
	if inPool {
		t.Fatal("deleted page still in pool")
	}
}

func TestDeletePinnedPageFails(t *testing.T) {
	t.Parallel()

	bp, _ := New(4, newMemDM(), nil)
	pid, g, _ := bp.NewPage()

	err := bp.DeletePage(pid)
	if !errors.Is(err, ErrPagePinned) {
		t.Fatalf("err = %v, want ErrPagePinned", err)
	}
	g.Unpin(false)
}

func TestConcurrentFetchUnpin(t *testing.T) {
	t.Parallel()

	dm := newMemDM()
	bp, _ := New(16, dm, nil)

	const goroutines = 50
	const iters = 100

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				// 50 goroutines competing for 20 pages in a 16-frame pool.
				pageID := PageID((id*iters+j)%20 + 1)
				g, err := bp.FetchPage(pageID)
				if err != nil {
					if errors.Is(err, ErrPoolExhausted) {
						continue // acceptable under high contention
					}
					t.Errorf("goroutine %d FetchPage(%d): %v", id, pageID, err)
					return
				}
				g.Unpin(false)
			}
		}(i)
	}
	wg.Wait()
}

func ExampleNew() {
	dm := newMemDM()
	bp, err := New(4, dm, nil)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("pool created with %d frames\n", bp.Size())
	// Output: pool created with 4 frames
}
```

## Review

The core is correct when every invariant holds at once. A cache hit must leave the pin count exact — one fetch then one unpin leaves it at one, not zero and not two — because the count reference-counts holders rather than flips a flag. Eviction must keep the pool at capacity and must never reclaim a pinned frame; the pinned-page test is the load-bearing assertion, since evicting a frame a caller still points into is the one bug the whole design exists to prevent. Dirty write-back must reach disk, and when a WAL is configured the log must be forced past a page's LSN before that page is written, which the ordering test checks by watching the stub's flush counter. The unpin contract must reject both an absent page and an already-zero pin count, so a double unpin surfaces as an error instead of corrupting eligibility. The whole surface, including the 50-goroutine stress loop, must run clean under `go test -race ./...`.

The common mistakes here are concrete. Holding the pool mutex across the cache-miss `ReadPage` serializes every goroutine on one slow disk read — the frame must be pinned inside the lock and the read done outside it, with the re-check on re-entry handling the two-goroutines-one-page case. Forgetting the re-check lets the same page occupy two frames and breaks the page table. Touching `guard.Data()` without the frame latch races concurrent readers and trips `-race`. And skipping `SetLSN` after a logged write leaves the pageLSN at zero, so write-back silently bypasses the WAL check and can put a page on disk ahead of its log record.

## Resources

- [CMU 15-445 Lecture 6: Buffer Pool Management](https://15445.courses.cs.cmu.edu/fall2024/slides/06-bufferpool.pdf) — pages, frames, pin counts, and the no-force/steal policies this core implements.
- [The Internals of PostgreSQL, Chapter 8: Buffer Manager](https://www.interdb.jp/pg/pgsql08.html) — clock sweep and the buffer descriptor layout in a production engine.
- [`sync` package](https://pkg.go.dev/sync) — `sync.Mutex` and `sync.RWMutex`, the two latch tiers the pool uses.
- [`errors` package](https://pkg.go.dev/errors) — `errors.New`, `errors.Is`, and `errors.Join`, used for the sentinel errors and `FlushAll`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-pluggable-replacer.md](02-pluggable-replacer.md)
