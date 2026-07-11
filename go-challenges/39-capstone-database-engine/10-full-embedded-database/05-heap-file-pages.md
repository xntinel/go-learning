# Exercise 5: Heap File over Many Pages

A single page holds at most 4096 bytes; a real table spans thousands of pages. This exercise builds the `HeapFile`: an ordered collection of slotted pages that grows by appending a tail page whenever no existing page can fit a tuple, and that addresses every row by a `TID` — the (page, slot) physical identifier the access methods and indexes hand around. It is the access-method floor of the stack, the abstraction that turns a pile of pages into a table you can scan, and it inherits the slotted page's most important promise: a tuple identifier stays valid across compaction.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including its own slotted page — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
heapfile.go          HeapFile, TID, PageID, NewHeapFile, PageCount, Insert, Read, Delete, Scan, Compact
page.go              SlottedPage (the per-page storage the heap file tiles together)
cmd/
  demo/
    main.go          insert rows, delete the middle, compact, scan the survivors
heapfile_test.go     scan count == inserts-deletes, TID stable across compaction, growth, invariant
```

- Files: `heapfile.go`, `page.go`, `cmd/demo/main.go`, `heapfile_test.go`.
- Implement: `HeapFile` with `Insert(tuple []byte) (TID, error)`, `Read`, `Delete`, `Scan(fn func(TID, []byte) error) error`, `Compact`, `PageCount`, plus `NewHeapFile` and the `TID` type.
- Test: `heapfile_test.go` proves a scan yields exactly inserts minus deletes, a `TID` survives compaction, the file grows a second page under load, an out-of-range `TID` is `ErrTupleNotFound`, and the two-pointer page invariant holds after every operation.
- Verify: `go test -run 'TestHeapFile|ExampleHeapFile' -race ./...`

Set up the module:

```bash
mkdir -p heap-file/cmd/demo && cd heap-file
go mod init example.com/heap-file
```

### Why a TID, and why the heap grows at the tail

The heap file's job is to make many fixed-size pages look like one growable table while keeping each row's address stable forever. The address it hands out is the `TID` — a page index plus a slot index inside that page — and the entire design serves keeping that pair valid for the life of the tuple. Insert walks the existing pages looking for the first one with room for the tuple plus its four-byte slot entry; if it finds one, the row lands there and its `TID` names that page and the slot the page assigned. If no page fits, the heap appends a fresh tail page and places the row there, and `PageCount` grows by one. This first-fit-then-append policy is the simplest allocation that never fragments a tuple across pages, which matters because a tuple must live entirely within one page for the slotted layout to address it with a single offset.

Compaction is where the `TID` promise is paid off. When `HeapFile.Compact` runs, it compacts every page in place, reclaiming the space of tombstoned tuples — but because each page's compaction preserves its slot indices, a live tuple's `TID` is exactly as valid after the compaction as before. A reader holding `TID{Page: 0, Slot: 3}` still reads the same row, even though that row's bytes may have moved within page 0. This is the property an index depends on: it stores `TID`s and must be able to follow them after a vacuum without being rebuilt. The exercise pins it directly — insert two rows, delete one, compact, and prove the survivor's `TID` still reads the right bytes while the deleted one still reads as a tombstone.

`Scan` is the sequential access method built on top: it walks pages in order and, within each, walks slots in order, skipping tombstones and yielding every live tuple with its `TID`. Returning the `TID` alongside the bytes is what lets a caller delete or update a row it just scanned. The invariant the tests assert on every page after every operation — `12 + SlotCount()*4 <= freeSpacePtr` — is the structural guarantee that the slot directory and the tuple area never overlapped; if it ever fails, a page is corrupt regardless of what the data looks like.

Create `heapfile.go`:

```go
package heapfile

import (
	"errors"
	"fmt"
)

// PageID is the index of a page within the heap file.
type PageID uint32

// TID is the physical address of a tuple: which page, and which slot inside it. It
// is stable for the lifetime of the tuple, including across page compaction, because
// Compact preserves slot indices.
type TID struct {
	Page PageID
	Slot int
}

// ErrTupleNotFound is returned when a TID does not name a live tuple.
var ErrTupleNotFound = errors.New("tuple not found")

// HeapFile is an ordered collection of slotted pages. It grows by appending a new
// tail page when no existing page can fit a tuple. It is the storage layer an access
// method (sequential scan, or a heap-backed index) sits on top of: callers address
// rows by TID and never see the page boundaries directly.
type HeapFile struct {
	pages []*SlottedPage
}

// NewHeapFile returns a heap file containing a single empty page.
func NewHeapFile() *HeapFile {
	var p SlottedPage
	p.Init()
	return &HeapFile{pages: []*SlottedPage{&p}}
}

// PageCount returns the number of pages currently backing the heap file.
func (h *HeapFile) PageCount() int {
	return len(h.pages)
}

// Insert places tuple in the first page with room, allocating a new tail page when
// none fits, and returns the tuple's stable TID. A tuple too large for an empty page
// returns ErrPageFull.
func (h *HeapFile) Insert(tuple []byte) (TID, error) {
	needed := len(tuple) + slotEntrySize
	for i, p := range h.pages {
		if p.FreeSpace() >= needed {
			slot, err := p.Insert(tuple)
			if err != nil {
				return TID{}, err
			}
			return TID{Page: PageID(i), Slot: slot}, nil
		}
	}
	var p SlottedPage
	p.Init()
	if p.FreeSpace() < needed {
		return TID{}, ErrPageFull
	}
	slot, err := p.Insert(tuple)
	if err != nil {
		return TID{}, err
	}
	h.pages = append(h.pages, &p)
	return TID{Page: PageID(len(h.pages) - 1), Slot: slot}, nil
}

// Read returns a copy of the tuple at tid.
func (h *HeapFile) Read(tid TID) ([]byte, error) {
	if int(tid.Page) >= len(h.pages) {
		return nil, fmt.Errorf("%w: page %d", ErrTupleNotFound, tid.Page)
	}
	return h.pages[tid.Page].Read(tid.Slot)
}

// Delete tombstones the tuple at tid. The space is reclaimed only by Compact.
func (h *HeapFile) Delete(tid TID) error {
	if int(tid.Page) >= len(h.pages) {
		return fmt.Errorf("%w: page %d", ErrTupleNotFound, tid.Page)
	}
	return h.pages[tid.Page].Delete(tid.Slot)
}

// Scan calls fn for every live (non-tombstoned) tuple in TID order. Iteration stops
// and returns the first error fn returns.
func (h *HeapFile) Scan(fn func(tid TID, tuple []byte) error) error {
	for pi, p := range h.pages {
		for s := 0; s < p.SlotCount(); s++ {
			tuple, err := p.Read(s)
			if errors.Is(err, ErrTombstone) {
				continue
			}
			if err != nil {
				return err
			}
			if err := fn(TID{Page: PageID(pi), Slot: s}, tuple); err != nil {
				return err
			}
		}
	}
	return nil
}

// Compact compacts every page in place, reclaiming tombstoned space. TIDs of live
// tuples remain valid because slot indices are preserved.
func (h *HeapFile) Compact() {
	for _, p := range h.pages {
		p.Compact()
	}
}
```

The per-page storage is the full slotted page from the storage exercise, including `Delete` and `Compact`, carried here so the heap file stands alone.

Create `page.go`:

```go
package heapfile

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Page layout constants.
const (
	// PageSize is the unit of I/O between the buffer pool and the disk manager.
	PageSize = 4096
	// pageHeaderSize covers the slot count, free-space pointer, and 8-byte page LSN.
	pageHeaderSize = 12
	// slotEntrySize is the byte cost of one slot directory entry: offset (2) + length (2).
	slotEntrySize = 4
)

// Slot is a slot directory entry. A slot whose Length is 0 is a tombstone.
type Slot struct {
	Offset uint16
	Length uint16
}

// SlottedPage implements the classic slotted-page heap layout.
type SlottedPage struct {
	data [PageSize]byte
}

// Sentinel errors for SlottedPage operations.
var (
	ErrPageFull    = errors.New("page is full")
	ErrInvalidSlot = errors.New("slot index out of range")
	ErrTombstone   = errors.New("slot has been deleted")
)

// Init resets the page to an empty state.
func (p *SlottedPage) Init() {
	for i := range p.data {
		p.data[i] = 0
	}
	p.setFreeSpacePtr(PageSize)
}

// SlotCount returns the number of slot directory entries, including tombstones.
func (p *SlottedPage) SlotCount() int {
	return int(binary.LittleEndian.Uint16(p.data[0:2]))
}

// FreeSpace returns the bytes available for a new tuple plus its slot entry.
func (p *SlottedPage) FreeSpace() int {
	directoryEnd := pageHeaderSize + p.SlotCount()*slotEntrySize
	tupleStart := int(p.freeSpacePtr())
	available := tupleStart - directoryEnd
	if available < 0 {
		return 0
	}
	return available
}

func (p *SlottedPage) freeSpacePtr() uint16 {
	return binary.LittleEndian.Uint16(p.data[2:4])
}

func (p *SlottedPage) setSlotCount(n int) {
	binary.LittleEndian.PutUint16(p.data[0:2], uint16(n))
}

func (p *SlottedPage) setFreeSpacePtr(ptr int) {
	binary.LittleEndian.PutUint16(p.data[2:4], uint16(ptr))
}

func (p *SlottedPage) getSlot(idx int) Slot {
	base := pageHeaderSize + idx*slotEntrySize
	return Slot{
		Offset: binary.LittleEndian.Uint16(p.data[base : base+2]),
		Length: binary.LittleEndian.Uint16(p.data[base+2 : base+4]),
	}
}

func (p *SlottedPage) setSlot(idx int, s Slot) {
	base := pageHeaderSize + idx*slotEntrySize
	binary.LittleEndian.PutUint16(p.data[base:base+2], s.Offset)
	binary.LittleEndian.PutUint16(p.data[base+2:base+4], s.Length)
}

// Insert copies tuple into the page and returns its stable slot index.
func (p *SlottedPage) Insert(tuple []byte) (int, error) {
	needed := len(tuple) + slotEntrySize
	if p.FreeSpace() < needed {
		return 0, ErrPageFull
	}
	newPtr := int(p.freeSpacePtr()) - len(tuple)
	copy(p.data[newPtr:], tuple)
	p.setFreeSpacePtr(newPtr)
	idx := p.SlotCount()
	p.setSlot(idx, Slot{Offset: uint16(newPtr), Length: uint16(len(tuple))})
	p.setSlotCount(idx + 1)
	return idx, nil
}

// Read returns a copy of the tuple at slotIdx.
func (p *SlottedPage) Read(slotIdx int) ([]byte, error) {
	if slotIdx < 0 || slotIdx >= p.SlotCount() {
		return nil, fmt.Errorf("%w: index %d", ErrInvalidSlot, slotIdx)
	}
	s := p.getSlot(slotIdx)
	if s.Length == 0 {
		return nil, ErrTombstone
	}
	out := make([]byte, s.Length)
	copy(out, p.data[s.Offset:int(s.Offset)+int(s.Length)])
	return out, nil
}

// Delete marks slotIdx as a tombstone.
func (p *SlottedPage) Delete(slotIdx int) error {
	if slotIdx < 0 || slotIdx >= p.SlotCount() {
		return fmt.Errorf("%w: index %d", ErrInvalidSlot, slotIdx)
	}
	s := p.getSlot(slotIdx)
	s.Length = 0
	p.setSlot(slotIdx, s)
	return nil
}

// Compact rebuilds the tuple area in-place, reclaiming tombstoned space while
// preserving slot indices so external TIDs remain valid.
func (p *SlottedPage) Compact() {
	var newData [PageSize]byte
	ptr := PageSize
	n := p.SlotCount()
	slots := make([]Slot, n)
	for i := 0; i < n; i++ {
		s := p.getSlot(i)
		if s.Length == 0 {
			continue // tombstone: leave slot as zero
		}
		ptr -= int(s.Length)
		copy(newData[ptr:], p.data[s.Offset:int(s.Offset)+int(s.Length)])
		slots[i] = Slot{Offset: uint16(ptr), Length: s.Length}
	}
	copy(newData[0:pageHeaderSize], p.data[0:pageHeaderSize])
	p.data = newData
	for i, s := range slots {
		p.setSlot(i, s)
	}
	p.setFreeSpacePtr(ptr)
}
```

### The runnable demo

The demo inserts three short rows, deletes the middle one, compacts, and scans the survivors — showing the heap file as a single logical table whose live set is exactly the inserts minus the deletes, with a stable page count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/heap-file"
)

func main() {
	h := heapfile.NewHeapFile()
	if _, err := h.Insert([]byte("a")); err != nil {
		log.Fatal(err)
	}
	mid, err := h.Insert([]byte("b"))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := h.Insert([]byte("c")); err != nil {
		log.Fatal(err)
	}
	if err := h.Delete(mid); err != nil {
		log.Fatal(err)
	}
	h.Compact()

	var live []string
	_ = h.Scan(func(_ heapfile.TID, tuple []byte) error {
		live = append(live, string(tuple))
		return nil
	})
	fmt.Println("live tuples:", live)
	fmt.Println("pages:", h.PageCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
live tuples: [a c]
pages: 1
```

### Tests

The tests pin three invariants and one error. A table-driven case inserts and deletes in several ratios and asserts the live scan count equals inserts minus deletes, checking the two-pointer page invariant after each run. A second test deletes one of two rows, compacts, and proves the survivor's `TID` still reads the right bytes while the deleted `TID` reads as a tombstone. A growth test inserts ten 1024-byte rows and asserts the file grew to at least two pages while still scanning all ten. An out-of-range `TID` must return `ErrTupleNotFound`.

Create `heapfile_test.go`:

```go
package heapfile

import (
	"errors"
	"fmt"
	"testing"
)

// liveCount counts the tuples a Scan yields.
func liveCount(t *testing.T, h *HeapFile) int {
	t.Helper()
	n := 0
	if err := h.Scan(func(TID, []byte) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return n
}

// assertTwoPointerInvariant checks 12 + SlotCount*4 <= freeSpacePtr on every page.
func assertTwoPointerInvariant(t *testing.T, h *HeapFile) {
	t.Helper()
	for i, p := range h.pages {
		dirEnd := pageHeaderSize + p.SlotCount()*slotEntrySize
		if dirEnd > int(p.freeSpacePtr()) {
			t.Fatalf("page %d violates two-pointer invariant: dirEnd=%d freeSpacePtr=%d",
				i, dirEnd, p.freeSpacePtr())
		}
	}
}

func TestHeapFileScanCountMatchesInserts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		inserts int
		deletes int
	}{
		{"no deletes", 5, 0},
		{"some deletes", 8, 3},
		{"all deleted", 4, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := NewHeapFile()
			tids := make([]TID, 0, tc.inserts)
			for i := 0; i < tc.inserts; i++ {
				tid, err := h.Insert([]byte(fmt.Sprintf("row-%d", i)))
				if err != nil {
					t.Fatalf("Insert[%d]: %v", i, err)
				}
				tids = append(tids, tid)
			}
			for i := 0; i < tc.deletes; i++ {
				if err := h.Delete(tids[i]); err != nil {
					t.Fatalf("Delete[%d]: %v", i, err)
				}
			}
			want := tc.inserts - tc.deletes
			if got := liveCount(t, h); got != want {
				t.Fatalf("live count = %d, want %d", got, want)
			}
			assertTwoPointerInvariant(t, h)
		})
	}
}

func TestHeapFileTIDStableAcrossCompaction(t *testing.T) {
	t.Parallel()

	h := NewHeapFile()
	keep, err := h.Insert([]byte("survivor"))
	if err != nil {
		t.Fatalf("Insert keep: %v", err)
	}
	gone, err := h.Insert([]byte("doomed"))
	if err != nil {
		t.Fatalf("Insert gone: %v", err)
	}
	if err := h.Delete(gone); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	h.Compact()

	got, err := h.Read(keep)
	if err != nil {
		t.Fatalf("Read(keep) after Compact: %v", err)
	}
	if string(got) != "survivor" {
		t.Fatalf("Read(keep) = %q, want survivor", got)
	}
	if _, err := h.Read(gone); !errors.Is(err, ErrTombstone) {
		t.Fatalf("Read(gone) after Compact: err = %v, want ErrTombstone", err)
	}
	assertTwoPointerInvariant(t, h)
}

func TestHeapFileGrowsToNewPage(t *testing.T) {
	t.Parallel()

	h := NewHeapFile()
	big := make([]byte, 1024)
	for i := 0; i < 10; i++ {
		if _, err := h.Insert(big); err != nil {
			t.Fatalf("Insert[%d]: %v", i, err)
		}
	}
	if h.PageCount() < 2 {
		t.Fatalf("PageCount = %d, want >= 2 (heap should have grown)", h.PageCount())
	}
	if got := liveCount(t, h); got != 10 {
		t.Fatalf("live count = %d, want 10", got)
	}
}

func TestHeapFileReadOutOfRange(t *testing.T) {
	t.Parallel()

	h := NewHeapFile()
	if _, err := h.Read(TID{Page: 99, Slot: 0}); !errors.Is(err, ErrTupleNotFound) {
		t.Fatalf("Read(missing page): err = %v, want ErrTupleNotFound", err)
	}
}

func ExampleHeapFile_Scan() {
	h := NewHeapFile()
	_, _ = h.Insert([]byte("a"))
	mid, _ := h.Insert([]byte("b"))
	_, _ = h.Insert([]byte("c"))
	_ = h.Delete(mid)
	h.Compact()
	var live []string
	_ = h.Scan(func(_ TID, tuple []byte) error {
		live = append(live, string(tuple))
		return nil
	})
	fmt.Println(live)
	// Output:
	// [a c]
}
```

## Review

The heap file is correct when a `TID` is a permanent address. Confirm that a scan yields exactly inserts minus deletes across every ratio, that the file appends a tail page rather than overflowing when a page fills, and — the decisive test — that a live tuple's `TID` still reads the right bytes after a compaction that reclaimed a neighbor's space, while the deleted `TID` stays a tombstone. The two-pointer invariant must hold on every page after every operation; a violation means the slot directory grew into the tuple area and the page is corrupt no matter how the data prints.

Common mistakes for this layer. Addressing rows by a global running index instead of (page, slot) breaks the moment the heap grows a second page. Splitting a tuple across two pages to use up trailing free space makes it un-addressable by a single slot offset. Forgetting to skip tombstones in `Scan` resurrects deleted rows. Rebuilding the heap on compaction instead of compacting pages in place invalidates every outstanding `TID` and forces every index to be rebuilt.

## Resources

- [Architecture of a Database System (Hellerstein, Stonebraker, Hamilton)](https://dsf.berkeley.edu/papers/fntdb07-architecture.pdf) — its access-methods section covers heap files, tuple identifiers, and sequential scan over many pages.
- [CMU 15-445: Database Systems](https://15445.courses.cs.cmu.edu/) — the Storage and Access Methods lectures cover heap-file organization and record identifiers in detail.
- [BoltDB source (etcd-io/bbolt)](https://github.com/etcd-io/bbolt) — a production embedded database in Go whose page-and-pointer design is a useful reference for tiling fixed-size pages into a larger structure.

---

Back to [04-crash-recovery-redo-log.md](04-crash-recovery-redo-log.md) | Next: [06-transaction-lifecycle.md](06-transaction-lifecycle.md)
