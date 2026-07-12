# Exercise 1: Slotted-Page Heap Layout

Every table in the engine ultimately becomes bytes in fixed-size pages, and the slotted page is the format that turns one 4096-byte block into a little arena of variable-length rows. This exercise builds that arena: a page that packs tuples from the bottom up, tracks them with a slot directory that grows from the top down, deletes by tombstone, and compacts to reclaim dead space without ever moving a row's external address. It is the bottom of the storage stack and the thing every later layer — heap files, the catalog's system tables, recovery — is built on.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
page.go              SlottedPage, Slot, Init, Insert, Read, Delete, Compact, FreeSpace, SlotCount
cmd/
  demo/
    main.go          insert rows, delete the middle one, compact, read the survivors
page_test.go         insert/read round-trips, tombstone-on-delete, fill-to-full, compaction reclaims space
```

- Files: `page.go`, `cmd/demo/main.go`, `page_test.go`.
- Implement: `SlottedPage` with `Init`, `Insert(tuple []byte) (int, error)`, `Read(slotIdx int) ([]byte, error)`, `Delete(slotIdx int) error`, `Compact()`, plus `FreeSpace`, `SlotCount`, and the `Slot` type.
- Test: `page_test.go` round-trips several tuples, asserts a deleted slot reads back as a tombstone, fills a page until `ErrPageFull`, and proves compaction recovers space while keeping live slot indices valid.
- Verify: `go test -run 'TestSlottedPage|ExampleSlottedPage' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/10-full-embedded-database/01-slotted-page-heap/cmd/demo && cd go-solutions/39-capstone-database-engine/10-full-embedded-database/01-slotted-page-heap
```

### Why a slot directory, and why tuples grow from the bottom

The hard requirement a heap page must satisfy is that a row's address stays valid even though rows have different sizes and come and go. If you stored tuples back-to-back and addressed a row by its byte offset, then deleting an earlier row and compacting would shift every later row and invalidate every address an index was holding. The slot directory breaks that coupling. Each row gets a fixed-size, four-byte directory entry — a two-byte offset and a two-byte length — and the row's external identity is its *slot index*, the position of that entry in the directory, not the byte offset of the data. The data can move; the entry's index does not. That indirection is the whole reason indexes can keep a tuple identifier across a vacuum.

The two regions grow toward each other from opposite ends, and that is a deliberate space-management trick. The header sits at the front (slot count, free-space pointer, page LSN), the slot directory grows downward right behind it, and the tuple data grows upward from the very end of the page. Free space is whatever is left in the middle. A new insert needs room for both a tuple *and* a four-byte slot entry, so the test for "does this fit" is `FreeSpace() >= len(tuple) + 4`, and the structural invariant the page must never break is that the directory's tail never crosses the free-space pointer: `12 + SlotCount()*4 <= freeSpacePtr`. Insert enforces it before writing a single byte.

Delete does not move anything. It writes a zero into the slot's length field, turning the entry into a tombstone, and leaves the tuple bytes where they are. This keeps every later slot index stable and makes delete O(1), at the cost of space that is now dead but not yet reclaimed. Reclamation is a separate, explicit step — `Compact` — which rebuilds the tuple area from scratch, copying only the live tuples to the tail of a fresh buffer and rewriting their offsets, while leaving tombstoned directory entries in place as zeroes so that slot indices, including those of the surviving rows, are unchanged. The page LSN in the header is read and written by the layer above (the WAL compares it during redo); the page itself only stores it.

Create `page.go`:

```go
package slottedpage

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Page layout constants. PageSize must match the disk manager's page size; all
// other constants are derived from it.
const (
	// PageSize is the unit of I/O between the buffer pool and the disk manager.
	PageSize = 4096
	// pageHeaderSize covers the slot count (2 bytes), free-space pointer (2 bytes),
	// and an 8-byte page LSN used by the WAL to detect already-applied records.
	pageHeaderSize = 12
	// slotEntrySize is the byte cost of one slot directory entry: offset (2) + length (2).
	slotEntrySize = 4
)

// Slot is a slot directory entry that describes where a single tuple is stored
// in the page. A slot whose Length is 0 is a tombstone (the tuple was deleted).
type Slot struct {
	Offset uint16
	Length uint16
}

// SlottedPage implements the classic slotted-page heap layout.
//
// Page layout (all integers are little-endian):
//
//	[0:2]                 slot count (uint16)
//	[2:4]                 free-space pointer (uint16) — first byte of tuple area
//	[4:12]                page LSN (uint64) — updated by WAL on every modification
//	[12 : 12+N*4]         slot directory (N * 4 bytes, grows toward the middle)
//	[12+N*4 : ptr]        free space
//	[ptr : PageSize]      tuple data (packed from the bottom, grows upward)
//
// Invariant: 12 + SlotCount()*4 <= FreeSpacePtr(). Violation means the page is
// overfull (a bug). Insert enforces this before writing.
type SlottedPage struct {
	data [PageSize]byte
}

// Sentinel errors for SlottedPage operations.
var (
	ErrPageFull    = errors.New("page is full")
	ErrInvalidSlot = errors.New("slot index out of range")
	ErrTombstone   = errors.New("slot has been deleted")
)

// Init resets the page to an empty state. Call this once after allocation before
// writing the first tuple.
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

// FreeSpace returns the number of bytes available for a new tuple plus its slot
// entry. A new Insert of an n-byte tuple requires FreeSpace() >= n + slotEntrySize.
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

// Insert copies tuple into the page and returns its slot index. Tuples are packed
// from the bottom of the page upward; slot directory entries grow downward from
// the header. The page is full when FreeSpace() < len(tuple)+4.
//
// The returned slot index is stable for the lifetime of the page: the caller uses
// (PageID, slotIndex) as the tuple's physical address.
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

// Read returns a copy of the tuple at slotIdx. Returns ErrTombstone if the slot
// was deleted and ErrInvalidSlot if the index is out of range.
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

// Delete marks slotIdx as a tombstone. The space is not immediately reclaimed;
// call Compact to reclaim space from all tombstones in the page.
func (p *SlottedPage) Delete(slotIdx int) error {
	if slotIdx < 0 || slotIdx >= p.SlotCount() {
		return fmt.Errorf("%w: index %d", ErrInvalidSlot, slotIdx)
	}
	s := p.getSlot(slotIdx)
	s.Length = 0
	p.setSlot(slotIdx, s)
	return nil
}

// Compact rebuilds the tuple area in-place, reclaiming space from tombstoned slots
// and resetting the free-space pointer. The slot directory entry count is unchanged
// so external addresses (PageID, slotIdx) remain valid; tombstoned entries retain
// zero offset and length after the operation.
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

The demo shows the full small life of a page: it inserts three rows, reports the free space the slot directory and tuple data leave between them, deletes the middle row, compacts to reclaim its bytes, and reads back the two survivors by their original slot indices to prove the addresses held across compaction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/slotted-page"
)

func main() {
	var p slottedpage.SlottedPage
	p.Init()

	rows := []string{"Alice,30", "Bob,25", "Carol,35"}
	indexes := make([]int, 0, len(rows))
	for _, row := range rows {
		idx, err := p.Insert([]byte(row))
		if err != nil {
			log.Fatalf("insert %q: %v", row, err)
		}
		indexes = append(indexes, idx)
	}
	fmt.Printf("inserted %d rows, free bytes remaining: %d\n", len(rows), p.FreeSpace())

	// Delete the middle row, compact, then verify the survivors by slot index.
	if err := p.Delete(indexes[1]); err != nil {
		log.Fatalf("delete: %v", err)
	}
	p.Compact()

	for _, idx := range []int{indexes[0], indexes[2]} {
		data, err := p.Read(idx)
		if err != nil {
			log.Fatalf("read slot %d: %v", idx, err)
		}
		fmt.Printf("  slot %d: %q\n", idx, data)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
inserted 3 rows, free bytes remaining: 4050
  slot 0: "Alice,30"
  slot 2: "Carol,35"
```

### Tests

The tests pin the four properties that define a correct page. Insert-then-read round-trips arbitrary byte payloads and confirms the first slot index is zero. Delete makes a subsequent read return `ErrTombstone`, not stale bytes. Filling with a fixed-size payload until `ErrPageFull` proves the free-space accounting bounds inserts. The compaction test is the load-bearing one: it deletes a middle row, compacts, asserts free space strictly grew, and then reads the surviving rows by their original indices to prove those addresses are still valid while the deleted slot still reads as a tombstone.

Create `page_test.go`:

```go
package slottedpage

import (
	"errors"
	"fmt"
	"testing"
)

func TestSlottedPageInit(t *testing.T) {
	t.Parallel()

	var p SlottedPage
	p.Init()
	if p.SlotCount() != 0 {
		t.Fatalf("SlotCount = %d, want 0", p.SlotCount())
	}
	want := PageSize - pageHeaderSize
	if got := p.FreeSpace(); got != want {
		t.Fatalf("FreeSpace = %d, want %d", got, want)
	}
}

func TestSlottedPageInsertAndRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []byte
	}{
		{"single byte", []byte{0x42}},
		{"short string", []byte("hello world")},
		{"binary data", []byte{0x00, 0xFF, 0xDE, 0xAD, 0xBE, 0xEF}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var p SlottedPage
			p.Init()
			idx, err := p.Insert(tc.input)
			if err != nil {
				t.Fatalf("Insert: %v", err)
			}
			if idx != 0 {
				t.Fatalf("first slot index = %d, want 0", idx)
			}
			got, err := p.Read(0)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if string(got) != string(tc.input) {
				t.Fatalf("Read = %q, want %q", got, tc.input)
			}
		})
	}
}

func TestSlottedPageMultipleInserts(t *testing.T) {
	t.Parallel()

	var p SlottedPage
	p.Init()
	tuples := []string{"first", "second", "third"}
	for i, s := range tuples {
		idx, err := p.Insert([]byte(s))
		if err != nil {
			t.Fatalf("Insert[%d]: %v", i, err)
		}
		if idx != i {
			t.Fatalf("Insert[%d] slot = %d, want %d", i, idx, i)
		}
	}
	if p.SlotCount() != len(tuples) {
		t.Fatalf("SlotCount = %d, want %d", p.SlotCount(), len(tuples))
	}
	for i, s := range tuples {
		got, err := p.Read(i)
		if err != nil {
			t.Fatalf("Read[%d]: %v", i, err)
		}
		if string(got) != s {
			t.Fatalf("Read[%d] = %q, want %q", i, got, s)
		}
	}
}

func TestSlottedPageDelete(t *testing.T) {
	t.Parallel()

	var p SlottedPage
	p.Init()
	idx, _ := p.Insert([]byte("to delete"))
	if err := p.Delete(idx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := p.Read(idx)
	if !errors.Is(err, ErrTombstone) {
		t.Fatalf("Read after Delete: err = %v, want ErrTombstone", err)
	}
}

func TestSlottedPageDeleteInvalidSlot(t *testing.T) {
	t.Parallel()

	var p SlottedPage
	p.Init()
	err := p.Delete(0)
	if !errors.Is(err, ErrInvalidSlot) {
		t.Fatalf("Delete(0) on empty page: err = %v, want ErrInvalidSlot", err)
	}
}

func TestSlottedPageReadOutOfRange(t *testing.T) {
	t.Parallel()

	var p SlottedPage
	p.Init()
	if _, err := p.Read(99); !errors.Is(err, ErrInvalidSlot) {
		t.Fatalf("Read(99) on empty page: err = %v, want ErrInvalidSlot", err)
	}
}

func TestSlottedPageFull(t *testing.T) {
	t.Parallel()

	var p SlottedPage
	p.Init()
	large := make([]byte, 512)
	var inserted int
	for {
		_, err := p.Insert(large)
		if errors.Is(err, ErrPageFull) {
			break
		}
		if err != nil {
			t.Fatalf("unexpected Insert error: %v", err)
		}
		inserted++
	}
	if inserted == 0 {
		t.Fatal("expected at least one successful insert before page full")
	}
}

func TestSlottedPageCompact(t *testing.T) {
	t.Parallel()

	var p SlottedPage
	p.Init()
	idx0, _ := p.Insert([]byte("keep me"))
	idx1, _ := p.Insert([]byte("delete me"))
	idx2, _ := p.Insert([]byte("keep me too"))

	freeBefore := p.FreeSpace()
	if err := p.Delete(idx1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	p.Compact()
	freeAfter := p.FreeSpace()
	if freeAfter <= freeBefore {
		t.Fatalf("Compact did not recover space: before=%d after=%d", freeBefore, freeAfter)
	}
	got0, err := p.Read(idx0)
	if err != nil || string(got0) != "keep me" {
		t.Fatalf("Read(idx0) after Compact: %v %q", err, got0)
	}
	got2, err := p.Read(idx2)
	if err != nil || string(got2) != "keep me too" {
		t.Fatalf("Read(idx2) after Compact: %v %q", err, got2)
	}
	_, err = p.Read(idx1)
	if !errors.Is(err, ErrTombstone) {
		t.Fatalf("Read(idx1) after Compact: err = %v, want ErrTombstone", err)
	}
}

func ExampleSlottedPage_Insert() {
	var p SlottedPage
	p.Init()
	idx, _ := p.Insert([]byte("hello world"))
	data, _ := p.Read(idx)
	fmt.Printf("slot=%d payload=%q\n", idx, data)
	// Output:
	// slot=0 payload="hello world"
}
```

## Review

A correct page keeps row addresses stable through every operation. Confirm an inserted tuple reads back byte-for-byte at slot zero, that a deleted slot reports `ErrTombstone` rather than returning stale bytes, and that an out-of-range index is `ErrInvalidSlot`. The compaction test is the one that matters most: free space must strictly increase after a delete-then-compact, the surviving rows must still be readable at their original slot indices, and the deleted slot must remain a tombstone — that combination is exactly what lets an index hold a tuple identifier across a vacuum. The fill-to-full test pins the free-space accounting: insert must refuse a tuple once `FreeSpace()` drops below `len(tuple)+4`, never overwriting the slot directory.

Common mistakes for this layout. Addressing a row by byte offset instead of slot index couples the address to compaction and breaks every external reference the moment you reclaim space. Reclaiming space inside Delete instead of deferring to Compact turns an O(1) delete into an O(page) move and shifts slot indices. Forgetting that an insert needs room for the tuple *and* its four-byte slot entry lets the directory grow into the tuple area and corrupts the page. Returning the page's internal slice from Read instead of a copy lets a caller mutate the page's bytes through the back door.

## Resources

- [Architecture of a Database System (Hellerstein, Stonebraker, Hamilton)](https://dsf.berkeley.edu/papers/fntdb07-architecture.pdf) — the authoritative survey; its storage-management section covers the slotted-page heap layout this exercise implements.
- [SQLite Database File Format](https://www.sqlite.org/fileformat2.html) — a production page format with a cell-pointer array and freeblocks, the real-world cousin of this slot directory.
- [CMU 15-445: Database Systems](https://15445.courses.cs.cmu.edu/) — the Storage lectures cover slotted pages, tuple identifiers, and tombstone-and-compact reclamation in depth.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-system-catalog.md](02-system-catalog.md)
