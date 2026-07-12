# Exercise 4: Checkpoint — Flush All Dirty Pages with WAL Ordering

A checkpoint forces every dirty page in the pool to disk so crash recovery has a known point to start from: everything dirtied before the checkpoint is now durable, so redo need not replay it. The subtle requirement is that a checkpoint cannot just blast pages to disk — under a steal/no-force engine each page write must still obey the WAL-before-page rule, forcing the log past that page's LSN first. This exercise builds a `Checkpoint` method on top of a complete buffer pool and a test that proves both invariants at once: no dirty frame survives the checkpoint, and at the instant of every page write the WAL had already been flushed to at least that page's LSN.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including the full buffer-pool baseline — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bufferpool.go            full buffer-pool baseline (BufferPool, New, FetchPage,
                         NewPage, writeBack, ...) plus Checkpoint
cmd/
  demo/
    main.go              dirty pages with LSNs, checkpoint, show the forced WAL frontier
checkpoint_test.go       checkpoint flushes all dirty pages obeying WAL order;
                         a second checkpoint of a clean pool is a no-op
```

- Files: `bufferpool.go`, `cmd/demo/main.go`, `checkpoint_test.go`.
- Implement: the buffer-pool baseline, then `Checkpoint() (LSN, error)`.
- Test: `checkpoint_test.go` dirties several pages with increasing LSNs against an unflushed WAL, checkpoints, and asserts no frame stays dirty and every recorded page write saw a flushed frontier at least its LSN; a second checkpoint returns LSN 0 and writes nothing.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/03-buffer-pool-manager/04-checkpoint-wal-ordering/cmd/demo && cd go-solutions/39-capstone-database-engine/03-buffer-pool-manager/04-checkpoint-wal-ordering
```

### Checkpoint reuses the one write-back choke point

The whole correctness argument for `Checkpoint` rests on a design decision made earlier: every page that leaves the pool for disk goes through `writeBack`, and `writeBack` is the single place the WAL-before-page check lives. So `Checkpoint` does not re-implement durability ordering — it walks the frames, and for each dirty one it calls `writeBack`, which forces the log to that page's LSN before issuing the write if the log is behind. Because the check is centralized, it is impossible for a checkpoint to write a page ahead of its log record; the ordering is structural, not something the checkpoint code has to remember.

`Checkpoint` returns the checkpoint LSN: the highest pageLSN among the pages it wrote, which is the point up to which all in-memory updates are now durable. It captures each page's LSN before calling `writeBack`, because `writeBack` clears the dirty bit but leaves the pageLSN in place — reading it after would still work, but capturing it first keeps the intent explicit. Per-frame write errors are collected and joined with `errors.Join`; on any error the method returns a zero LSN and the joined error rather than a half-meaningful checkpoint LSN. On success no frame in the pool is dirty, which is the property recovery depends on: the checkpoint LSN is a true high-water mark for durability.

The baseline below is the complete buffer pool from the core exercise, reproduced so this module stands alone. `Checkpoint` is appended after it.

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

// DiskManager abstracts page-level random I/O.
type DiskManager interface {
	ReadPage(pageID PageID, buf []byte) error
	WritePage(pageID PageID, buf []byte) error
	AllocatePage() (PageID, error)
	FreePage(pageID PageID) error
}

// WALFlusher ensures WAL records are durable up to a given LSN.
type WALFlusher interface {
	FlushedLSN() LSN
	FlushUpTo(lsn LSN) error
}

// Page holds one page's worth of raw data.
type Page [PageSize]byte

type frame struct {
	mu       sync.RWMutex
	pageID   PageID
	data     Page
	pinCount int
	dirty    bool
	refBit   bool
	pageLSN  LSN
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
func (bp *BufferPool) Size() int { return len(bp.frames) }

// PageGuard holds a pinned frame. Call Unpin exactly once when done.
type PageGuard struct {
	bp     *BufferPool
	pageID PageID
	frame  *frame
}

func (g *PageGuard) Data() *Page    { return &g.frame.data }
func (g *PageGuard) PageID() PageID { return g.pageID }

func (g *PageGuard) SetLSN(lsn LSN) {
	g.bp.mu.Lock()
	g.frame.pageLSN = lsn
	g.bp.mu.Unlock()
}

func (g *PageGuard) RLock()   { g.frame.mu.RLock() }
func (g *PageGuard) RUnlock() { g.frame.mu.RUnlock() }
func (g *PageGuard) Lock()    { g.frame.mu.Lock() }
func (g *PageGuard) Unlock()  { g.frame.mu.Unlock() }

func (g *PageGuard) Unpin(isDirty bool) error {
	return g.bp.UnpinPage(g.pageID, isDirty)
}

// FetchPage pins pageID and returns a PageGuard. Safe for concurrent use.
func (bp *BufferPool) FetchPage(pageID PageID) (*PageGuard, error) {
	if pageID == InvalidPageID {
		return nil, ErrInvalidPageID
	}

	bp.mu.Lock()

	if fid, ok := bp.pageTable[pageID]; ok {
		f := &bp.frames[fid]
		f.pinCount++
		f.refBit = true
		bp.mu.Unlock()
		return &PageGuard{bp: bp, pageID: pageID, frame: f}, nil
	}

	fid, err := bp.clockSweep()
	if err != nil {
		bp.mu.Unlock()
		return nil, err
	}
	f := &bp.frames[fid]
	f.pinCount = 1
	f.pageID = InvalidPageID
	bp.mu.Unlock()

	if err := bp.dm.ReadPage(pageID, f.data[:]); err != nil {
		bp.mu.Lock()
		f.pinCount = 0
		bp.freeList = append(bp.freeList, fid)
		bp.mu.Unlock()
		return nil, fmt.Errorf("bufferpool: read page %d: %w", pageID, err)
	}

	bp.mu.Lock()
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

func (bp *BufferPool) clockSweep() (FrameID, error) {
	if len(bp.freeList) > 0 {
		fid := bp.freeList[len(bp.freeList)-1]
		bp.freeList = bp.freeList[:len(bp.freeList)-1]
		return fid, nil
	}

	n := len(bp.frames)
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

// UnpinPage decrements the pin count for pageID.
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

// FlushPage writes pageID's dirty frame to disk.
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

// FlushAll flushes every dirty frame in the pool.
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

// NewPage allocates a fresh page and loads it pinned into the pool.
func (bp *BufferPool) NewPage() (PageID, *PageGuard, error) {
	pageID, err := bp.dm.AllocatePage()
	if err != nil {
		return InvalidPageID, nil, fmt.Errorf("bufferpool: allocate page: %w", err)
	}

	bp.mu.Lock()
	fid, err := bp.clockSweep()
	if err != nil {
		bp.mu.Unlock()
		_ = bp.dm.FreePage(pageID)
		return InvalidPageID, nil, err
	}
	f := &bp.frames[fid]
	f.pageID = pageID
	f.data = Page{}
	f.pinCount = 1
	f.dirty = true
	f.refBit = true
	f.pageLSN = 0
	bp.pageTable[pageID] = fid
	bp.mu.Unlock()

	return pageID, &PageGuard{bp: bp, pageID: pageID, frame: f}, nil
}

// DeletePage removes pageID from the pool and returns it to the disk free list.
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

Now add the checkpoint.

Add to `bufferpool.go`:

```go
// Checkpoint flushes every dirty frame to disk, enforcing the WAL-before-page
// rule for each (see writeBack), and returns the highest pageLSN written — the
// checkpoint LSN, the point up to which all in-memory updates are now durable.
// On success no frame in the pool is dirty. Per-frame write errors are joined;
// on any error Checkpoint returns a zero LSN and the joined error.
func (bp *BufferPool) Checkpoint() (LSN, error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()

	var checkpointLSN LSN
	var errs []error
	for i := range bp.frames {
		f := &bp.frames[i]
		if !f.dirty {
			continue
		}
		lsn := f.pageLSN
		if err := bp.writeBack(f); err != nil {
			errs = append(errs, err)
			continue
		}
		if lsn > checkpointLSN {
			checkpointLSN = lsn
		}
	}
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}
	return checkpointLSN, nil
}
```

### The runnable demo

The demo allocates three pages, writes a marker into each, and tags each with an increasing LSN against a WAL whose flushed frontier starts at zero. Before the checkpoint the WAL is unflushed; the checkpoint write-backs every dirty page, and because each write goes through `writeBack` the log is forced forward to the highest LSN it wrote. The output shows the frontier moving from 0 to the checkpoint LSN — the WAL-before-page rule advancing the log exactly as far as the pages it persisted.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"sync"

	"example.com/checkpoint-wal-ordering"
)

type memDM struct {
	mu    sync.Mutex
	pages map[bufferpool.PageID]bufferpool.Page
	next  bufferpool.PageID
}

func newDM() *memDM {
	return &memDM{pages: make(map[bufferpool.PageID]bufferpool.Page), next: 1}
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

// demoWAL is a minimal WALFlusher that tracks a flushed frontier.
type demoWAL struct {
	mu      sync.Mutex
	flushed bufferpool.LSN
}

func (w *demoWAL) FlushedLSN() bufferpool.LSN {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushed
}

func (w *demoWAL) FlushUpTo(lsn bufferpool.LSN) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if lsn > w.flushed {
		w.flushed = lsn
	}
	return nil
}

func main() {
	dm := newDM()
	wal := &demoWAL{}
	bp, err := bufferpool.New(4, dm, wal)
	if err != nil {
		log.Fatalf("New: %v", err)
	}

	for i := 0; i < 3; i++ {
		_, g, err := bp.NewPage()
		if err != nil {
			log.Fatalf("NewPage: %v", err)
		}
		g.Lock()
		g.Data()[0] = byte(i + 1)
		g.Unlock()
		g.SetLSN(bufferpool.LSN(10 * (i + 1))) // 10, 20, 30
		if err := g.Unpin(true); err != nil {
			log.Fatalf("Unpin: %v", err)
		}
	}

	fmt.Printf("wal flushed before checkpoint: %d\n", wal.FlushedLSN())
	cpLSN, err := bp.Checkpoint()
	if err != nil {
		log.Fatalf("Checkpoint: %v", err)
	}
	fmt.Printf("checkpoint LSN: %d\n", cpLSN)
	fmt.Printf("wal flushed after checkpoint: %d\n", wal.FlushedLSN())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
wal flushed before checkpoint: 0
checkpoint LSN: 30
wal flushed after checkpoint: 30
```

### Tests

`TestCheckpointFlushesAllDirtyPages` is the centerpiece. It uses a recording disk manager that captures `wal.FlushedLSN()` at the instant of every `WritePage`, so the test can prove the ordering after the fact. It dirties four pages with strictly increasing LSNs against an unflushed WAL — so every write-back must force the log first — then checkpoints and asserts three things: the returned LSN is the max dirty pageLSN, no frame stays dirty, and every recorded write saw a flushed frontier at least its page's LSN. `TestCheckpointIsIdempotent` checkpoints a clean pool a second time and asserts it returns LSN 0 with no error and issues zero additional writes.

Create `checkpoint_test.go`:

```go
package bufferpool

import (
	"fmt"
	"sync"
	"testing"
)

// stubWAL is a controllable WALFlusher for tests.
type stubWAL struct {
	mu      sync.Mutex
	flushed LSN
}

func (w *stubWAL) FlushedLSN() LSN {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushed
}

func (w *stubWAL) FlushUpTo(lsn LSN) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if lsn > w.flushed {
		w.flushed = lsn
	}
	return nil
}

// walRecordingDM records, for every WritePage, the WAL flushedLSN observed at
// the moment of the write. It lets a test prove the WAL-before-page ordering:
// at each page write the WAL must already be durable up to that page's LSN.
type walRecordingDM struct {
	mu     sync.Mutex
	pages  map[PageID]Page
	next   PageID
	wal    *stubWAL
	writes []pageWrite
}

type pageWrite struct {
	pageID      PageID
	flushedSeen LSN
}

func newWALRecordingDM(wal *stubWAL) *walRecordingDM {
	return &walRecordingDM{
		pages: make(map[PageID]Page),
		next:  1,
		wal:   wal,
	}
}

func (d *walRecordingDM) ReadPage(pageID PageID, buf []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.pages[pageID]
	copy(buf, p[:])
	return nil
}

func (d *walRecordingDM) WritePage(pageID PageID, buf []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var p Page
	copy(p[:], buf)
	d.pages[pageID] = p
	d.writes = append(d.writes, pageWrite{pageID: pageID, flushedSeen: d.wal.FlushedLSN()})
	return nil
}

func (d *walRecordingDM) AllocatePage() (PageID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := d.next
	d.next++
	return id, nil
}

func (d *walRecordingDM) FreePage(PageID) error { return nil }

func TestCheckpointFlushesAllDirtyPages(t *testing.T) {
	t.Parallel()

	wal := &stubWAL{flushed: 0}
	dm := newWALRecordingDM(wal)
	bp, err := New(4, dm, wal)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Dirty four pages with strictly increasing LSNs. The WAL is unflushed (0),
	// so each write-back must force the log forward first.
	type dirtied struct {
		pageID PageID
		lsn    LSN
	}
	var pages []dirtied
	for i := 0; i < 4; i++ {
		pid, g, err := bp.NewPage()
		if err != nil {
			t.Fatalf("NewPage: %v", err)
		}
		lsn := LSN(10 * (i + 1)) // 10, 20, 30, 40
		g.Lock()
		g.Data()[0] = byte(i + 1)
		g.Unlock()
		g.SetLSN(lsn)
		if err := g.Unpin(true); err != nil {
			t.Fatalf("Unpin: %v", err)
		}
		pages = append(pages, dirtied{pid, lsn})
	}

	cpLSN, err := bp.Checkpoint()
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if cpLSN != 40 {
		t.Fatalf("checkpoint LSN = %d, want 40 (max dirty pageLSN)", cpLSN)
	}

	// 1. No dirty frame survives the checkpoint.
	bp.mu.Lock()
	for i := range bp.frames {
		if bp.frames[i].dirty {
			bp.mu.Unlock()
			t.Fatalf("frame %d still dirty after Checkpoint", i)
		}
	}
	bp.mu.Unlock()

	// 2. WAL-before-page ordering: every page write observed a flushedLSN at
	// least as large as that page's pageLSN.
	wantLSN := make(map[PageID]LSN, len(pages))
	for _, p := range pages {
		wantLSN[p.pageID] = p.lsn
	}
	dm.mu.Lock()
	writes := append([]pageWrite(nil), dm.writes...)
	dm.mu.Unlock()
	if len(writes) != len(pages) {
		t.Fatalf("recorded %d page writes, want %d", len(writes), len(pages))
	}
	for _, w := range writes {
		if w.flushedSeen < wantLSN[w.pageID] {
			t.Fatalf("page %d written while WAL flushed only to %d (need >= %d) — WAL-before-page violated",
				w.pageID, w.flushedSeen, wantLSN[w.pageID])
		}
	}
}

func TestCheckpointIsIdempotent(t *testing.T) {
	t.Parallel()

	wal := &stubWAL{}
	dm := newWALRecordingDM(wal)
	bp, err := New(4, dm, wal)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, g, err := bp.NewPage()
	if err != nil {
		t.Fatalf("NewPage: %v", err)
	}
	g.Lock()
	g.Data()[0] = 1
	g.Unlock()
	g.SetLSN(15)
	if err := g.Unpin(true); err != nil {
		t.Fatalf("Unpin: %v", err)
	}

	if _, err := bp.Checkpoint(); err != nil {
		t.Fatalf("first Checkpoint: %v", err)
	}
	dm.mu.Lock()
	afterFirst := len(dm.writes)
	dm.mu.Unlock()

	// A clean pool needs no I/O: zero LSN, no error, no additional writes.
	cp2, err := bp.Checkpoint()
	if err != nil {
		t.Fatalf("second Checkpoint: %v", err)
	}
	if cp2 != 0 {
		t.Fatalf("second checkpoint LSN = %d, want 0 (clean pool)", cp2)
	}
	dm.mu.Lock()
	afterSecond := len(dm.writes)
	dm.mu.Unlock()
	if afterSecond != afterFirst {
		t.Fatalf("second Checkpoint issued %d extra writes, want 0", afterSecond-afterFirst)
	}
}

func ExampleBufferPool_Checkpoint() {
	wal := &stubWAL{}
	dm := newWALRecordingDM(wal)
	bp, _ := New(4, dm, wal)

	_, g, _ := bp.NewPage()
	g.Lock()
	g.Data()[0] = 1
	g.Unlock()
	g.SetLSN(25)
	g.Unpin(true)

	cpLSN, _ := bp.Checkpoint()
	fmt.Println("checkpoint LSN:", cpLSN)
	fmt.Println("wal flushed:", wal.FlushedLSN())
	// Output:
	// checkpoint LSN: 25
	// wal flushed: 25
}
```

## Review

The checkpoint is correct when both invariants hold together. After it returns successfully, no frame is dirty — recovery can trust that everything up to the checkpoint LSN reached disk — and the returned LSN is the true maximum among the pages it wrote. The ordering invariant is the one that takes work to prove: the recording disk manager captures the WAL frontier at the exact moment of each write, so the test can show every page reached disk only after the log was forced past its LSN. Because the WAL started unflushed, a checkpoint that skipped the `writeBack` choke point would be caught immediately — some write would record a frontier of zero against a page LSN of forty. The idempotence case confirms the cheap path: a clean pool checkpoints to LSN 0 and issues no I/O.

The mistakes here are about where durability ordering lives. Writing pages directly with `dm.WritePage` inside `Checkpoint`, bypassing `writeBack`, drops the WAL check and can place a page on disk ahead of its log record — the exact crash-unrecoverable state the protocol exists to prevent. Reading each page's LSN after `writeBack` instead of before is harmless here because `writeBack` leaves the pageLSN intact, but capturing it first keeps the code honest if that ever changes. And returning a non-zero LSN on partial failure would hand recovery a high-water mark that some pages never actually reached, so the joined-error path must return zero.

## Resources

- [ARIES: A Transaction Recovery Method (Mohan et al., 1992)](https://dl.acm.org/doi/10.1145/128765.128770) — the foundational WAL-before-page protocol and the role of checkpoints in bounding redo.
- [CMU 15-445 Lecture 21: Database Recovery](https://15445.courses.cs.cmu.edu/fall2024/slides/21-recovery.pdf) — sharp versus fuzzy checkpoints and the redo point.
- [PostgreSQL: WAL Configuration (checkpoints)](https://www.postgresql.org/docs/current/wal-configuration.html) — how a production engine schedules checkpoints and what they flush.

---

Back to [03-read-ahead-prefetch.md](03-read-ahead-prefetch.md) | Next: [SQL Lexer and Tokenizer](../04-sql-lexer-tokenizer/00-concepts.md)
