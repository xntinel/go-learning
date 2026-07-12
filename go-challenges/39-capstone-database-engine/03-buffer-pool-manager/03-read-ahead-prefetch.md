# Exercise 3: Read-Ahead Prefetch and a Hit-Rate Test

A sequential scan that fetches its pages one at a time pays a cache miss on every page: each `FetchPage` finds the page absent, evicts a victim, and reads from disk. Read-ahead removes those misses by warming the pool before the scan touches the pages — it fetches each page and immediately unpins it, so by the time the scan arrives every page is already resident and the scan is all cache hits. This exercise builds the prefetch path on top of a complete buffer pool and measures its effect directly: a counting disk manager proves the warm scan performs zero reads while the cold scan reads every page.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including the full buffer-pool baseline — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bufferpool.go            full buffer-pool baseline (PageID, BufferPool, New, FetchPage,
                         UnpinPage, NewPage, ...) plus Prefetch and PrefetchRange
cmd/
  demo/
    main.go              read-ahead a range, then scan it and report disk reads
prefetch_test.go         cold scan misses every page; warm scan hits every page;
                         prefetch into a full pinned pool returns ErrPoolExhausted
```

- Files: `bufferpool.go`, `cmd/demo/main.go`, `prefetch_test.go`.
- Implement: the buffer-pool baseline, then `Prefetch(ids ...PageID) error` and `PrefetchRange(start PageID, n int) error`.
- Test: `prefetch_test.go` measures scan-phase disk reads with and without read-ahead, asserts prefetch never writes a page, and asserts prefetch into a fully pinned pool returns `ErrPoolExhausted`.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/03-buffer-pool-manager/03-read-ahead-prefetch/cmd/demo && cd go-solutions/39-capstone-database-engine/03-buffer-pool-manager/03-read-ahead-prefetch
```

### Prefetch is a hint, not a reservation

The defining property of read-ahead is that it holds no pins. `Prefetch` fetches each page — paying the disk read once, up front — and then immediately unpins it, leaving the page resident but evictable. This matters because read-ahead is speculative: it warms pages the scan is likely to want, but if the pool is under pressure some of those pages may be evicted again before the scan reaches them. That is fine. A page evicted before the scan arrives simply pays its miss then, exactly as if no prefetch had happened. Prefetch can never make a scan slower or incorrect; at worst it does redundant work. This is why it must not hold pins: a prefetch that kept pages pinned would reserve frames the rest of the workload needs and could exhaust the pool for pages nobody has asked for yet.

The error contract follows from that stance. If a page cannot be loaded — most plausibly because the pool is fully pinned and nothing can be evicted — `Prefetch` stops and returns the wrapped error, which callers assert with `errors.Is` (for example against `ErrPoolExhausted`). Pages warmed before the failure stay cached; prefetch does not roll back its earlier work, because that work is already valid cache content. `PrefetchRange` is a thin convenience over `Prefetch` for the common sequential pattern of n consecutive page ids, and a non-positive count is a no-op. An invalid page id inside a batch is skipped rather than treated as an error, since a hint over a sparse id set should not abort on a gap.

The baseline below is the complete buffer pool from the core exercise, reproduced so this module stands alone. The two new methods are appended after it.

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

Now add the prefetch path.

Add to `bufferpool.go`:

```go
// Prefetch warms the pool by fetching then immediately unpinning each page in
// ids, so a later FetchPage finds them cached. Pages already resident cost no
// extra disk I/O. If a page cannot be loaded — for example the pool is fully
// pinned — Prefetch stops and returns the wrapped error (assertable with
// errors.Is, e.g. ErrPoolExhausted); pages warmed before the failure stay
// cached.
func (bp *BufferPool) Prefetch(ids ...PageID) error {
	for _, id := range ids {
		if id == InvalidPageID {
			continue
		}
		g, err := bp.FetchPage(id)
		if err != nil {
			return fmt.Errorf("bufferpool: prefetch page %d: %w", id, err)
		}
		if err := g.Unpin(false); err != nil {
			return fmt.Errorf("bufferpool: prefetch unpin page %d: %w", id, err)
		}
	}
	return nil
}

// PrefetchRange reads ahead n consecutive pages starting at start. It is a
// convenience wrapper over Prefetch for the common sequential-scan pattern.
func (bp *BufferPool) PrefetchRange(start PageID, n int) error {
	if n <= 0 {
		return nil
	}
	ids := make([]PageID, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, start+PageID(i))
	}
	return bp.Prefetch(ids...)
}
```

### The runnable demo

The demo wires a counting disk manager into a pool sized to hold the whole range, reads ahead n pages, then scans those n pages and reports the disk reads each phase cost. Read-ahead pays all n reads up front; the scan that follows is pure cache hits and reads nothing, which is the entire point of prefetch made visible in two numbers.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"sync"

	"example.com/read-ahead-prefetch"
)

// countingDM counts ReadPage calls so the demo can show the hit-rate effect.
type countingDM struct {
	mu    sync.Mutex
	pages map[bufferpool.PageID]bufferpool.Page
	next  bufferpool.PageID
	reads int
}

func newDM() *countingDM {
	return &countingDM{pages: make(map[bufferpool.PageID]bufferpool.Page), next: 1}
}

func (m *countingDM) ReadPage(id bufferpool.PageID, buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reads++
	p := m.pages[id]
	copy(buf, p[:])
	return nil
}

func (m *countingDM) WritePage(id bufferpool.PageID, buf []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p bufferpool.Page
	copy(p[:], buf)
	m.pages[id] = p
	return nil
}

func (m *countingDM) AllocatePage() (bufferpool.PageID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.next
	m.next++
	return id, nil
}

func (m *countingDM) FreePage(bufferpool.PageID) error { return nil }

func (m *countingDM) readCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reads
}

func main() {
	const n = 8
	dm := newDM()
	bp, err := bufferpool.New(n, dm, nil)
	if err != nil {
		log.Fatalf("New: %v", err)
	}

	if err := bp.PrefetchRange(1, n); err != nil {
		log.Fatalf("PrefetchRange: %v", err)
	}
	fmt.Printf("read-ahead of %d pages: %d disk reads\n", n, dm.readCount())

	before := dm.readCount()
	for id := bufferpool.PageID(1); id <= n; id++ {
		g, err := bp.FetchPage(id)
		if err != nil {
			log.Fatalf("FetchPage(%d): %v", id, err)
		}
		g.Unpin(false)
	}
	fmt.Printf("scan of %d pages after read-ahead: %d disk reads\n", n, dm.readCount()-before)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
read-ahead of 8 pages: 8 disk reads
scan of 8 pages after read-ahead: 0 disk reads
```

### Tests

`TestPrefetchImprovesHitRate` is table-driven and measures the scan phase in isolation: it snapshots the read counter after any prefetch and before the scan, so the delta is exactly the misses the scan itself incurs. Cold, every scanned page misses (n reads); warm, read-ahead already paid those reads, so the scan is all hits (0 reads). `TestPrefetchNeverWrites` asserts read-ahead performs no `WritePage` calls — it must never dirty or flush a page. `TestPrefetchPoolExhausted` pins both frames of a two-frame pool so nothing can be evicted, then asserts a prefetch returns `ErrPoolExhausted`.

Create `prefetch_test.go`:

```go
package bufferpool

import (
	"errors"
	"sync/atomic"
	"testing"
)

// countingDM is a DiskManager that counts ReadPage and WritePage calls so tests
// can measure cache hit rate and confirm prefetch never writes. Concurrency-safe.
type countingDM struct {
	readCount  atomic.Int64
	writeCount atomic.Int64
	next       atomic.Int64
}

func newCountingDM() *countingDM {
	d := &countingDM{}
	d.next.Store(1) // 0 is InvalidPageID
	return d
}

func (d *countingDM) ReadPage(pageID PageID, buf []byte) error {
	d.readCount.Add(1)
	return nil
}

func (d *countingDM) WritePage(pageID PageID, buf []byte) error {
	d.writeCount.Add(1)
	return nil
}

func (d *countingDM) AllocatePage() (PageID, error) {
	return PageID(d.next.Add(1) - 1), nil
}

func (d *countingDM) FreePage(PageID) error { return nil }

func (d *countingDM) reads() int64  { return d.readCount.Load() }
func (d *countingDM) writes() int64 { return d.writeCount.Load() }

func TestPrefetchImprovesHitRate(t *testing.T) {
	t.Parallel()

	const n = 8

	cases := []struct {
		name      string
		prefetch  bool
		wantReads int64 // disk reads during the scan phase only
	}{
		{"cold scan misses every page", false, n},
		{"warm scan after read-ahead hits every page", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dm := newCountingDM()
			bp, err := New(n, dm, nil)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if tc.prefetch {
				if err := bp.PrefetchRange(1, n); err != nil {
					t.Fatalf("PrefetchRange: %v", err)
				}
			}
			before := dm.reads()
			for id := PageID(1); id <= n; id++ {
				g, err := bp.FetchPage(id)
				if err != nil {
					t.Fatalf("FetchPage(%d): %v", id, err)
				}
				if err := g.Unpin(false); err != nil {
					t.Fatalf("Unpin(%d): %v", id, err)
				}
			}
			scanReads := dm.reads() - before
			if scanReads != tc.wantReads {
				t.Fatalf("scan-phase disk reads = %d, want %d", scanReads, tc.wantReads)
			}
		})
	}
}

func TestPrefetchNeverWrites(t *testing.T) {
	t.Parallel()

	dm := newCountingDM()
	bp, err := New(4, dm, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := bp.PrefetchRange(1, 4); err != nil {
		t.Fatalf("PrefetchRange: %v", err)
	}
	if w := dm.writes(); w != 0 {
		t.Fatalf("prefetch performed %d writes, want 0 — read-ahead must not dirty a page", w)
	}
}

func TestPrefetchPoolExhausted(t *testing.T) {
	t.Parallel()

	dm := newCountingDM()
	bp, err := New(2, dm, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Pin both frames so no eviction is possible.
	g1, err := bp.FetchPage(1)
	if err != nil {
		t.Fatalf("FetchPage(1): %v", err)
	}
	g2, err := bp.FetchPage(2)
	if err != nil {
		t.Fatalf("FetchPage(2): %v", err)
	}
	defer g1.Unpin(false)
	defer g2.Unpin(false)

	err = bp.Prefetch(3)
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("Prefetch into full pinned pool: err = %v, want ErrPoolExhausted", err)
	}
}
```

## Review

Prefetch is correct when it is purely a hint: it warms pages and holds nothing. The hit-rate test is the proof — measured in isolation, the warm scan reads zero pages because read-ahead already paid every read, while the cold scan reads all n. That the warm scan reads zero (not "fewer") is the strong claim, and it depends on the pool being large enough to hold the whole range; size the pool below the range and some warmed pages are evicted before the scan, which is correct behavior but a weaker number. Confirm prefetch issues no writes, and that into a fully pinned pool it surfaces `ErrPoolExhausted` through `errors.Is` rather than panicking or hanging.

The mistakes worth calling out: keeping pages pinned in `Prefetch` instead of unpinning them turns a hint into a reservation that can starve the real workload of frames and even deadlock a small pool. Rolling back already-warmed pages when a later one fails throws away valid cache content for no benefit. And treating an invalid page id in a batch as a hard error makes prefetch over a sparse id set brittle, when skipping the gap is the natural behavior for a speculative warm-up.

## Resources

- [PostgreSQL: effective_io_concurrency and read-ahead](https://www.postgresql.org/docs/current/runtime-config-resource.html) — how a production engine tunes prefetch depth for sequential and bitmap scans.
- [The Internals of PostgreSQL, Chapter 8: Buffer Manager](https://www.interdb.jp/pg/pgsql08.html) — ring buffers and how scans interact with the buffer pool.
- [`sync/atomic` package](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, the lock-free counter the hit-rate test uses to measure reads and writes.

---

Back to [02-pluggable-replacer.md](02-pluggable-replacer.md) | Next: [04-checkpoint-wal-ordering.md](04-checkpoint-wal-ordering.md)
