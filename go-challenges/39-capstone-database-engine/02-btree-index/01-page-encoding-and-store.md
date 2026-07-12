# Exercise 1: Page Encoding and the Page Store

Every durability and lookup guarantee a B+Tree makes rests on one humble object: the on-disk page. A node is not a Go struct that happens to live in memory; it is exactly 4096 bytes of bytes that can be written to disk, read back after a crash, and decoded without parsing text. This exercise builds that substrate: the fixed binary layout for leaf and internal pages, the `RecordID` that a leaf entry points at, and the `PageStore` abstraction with an in-memory implementation that every later exercise reads and writes through.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
btree.go             PageSize, MaxKeySize, RecordID, MakeRecordID, PageStore,
                     MemStore, node, encodeLeaf/decodeLeaf, encodeInner/decodeInner
cmd/
  demo/
    main.go          allocate pages, pack a RecordID, round-trip a raw page
btree_test.go        RecordID bit-packing + leaf and internal page round-trips
```

- Files: `btree.go`, `cmd/demo/main.go`, `btree_test.go`.
- Implement: `RecordID` with `MakeRecordID`, `Page`, `Slot`; the `PageStore` interface; `MemStore` with `ReadPage`, `WritePage`, `AllocPage`; the `node` type; and `encodeLeaf`/`decodeLeaf`, `encodeInner`/`decodeInner`.
- Test: `btree_test.go` packs and unpacks `RecordID` values, and round-trips both a leaf node and an internal node through encode then decode.
- Verify: `go test -run 'TestRecordID|TestPage' -race ./...`

### Why a fixed binary layout, and what a RecordID is

The tree is a graph of pages addressed by integer page IDs, not Go pointers. A node holds child *page IDs*, not `*node` references, because the whole point is that a node can outlive the process: it is written to a page store, evicted from memory, and read back by ID. That single decision — address by ID, encode to bytes — is what makes the structure persistable, and it forces a concrete binary format rather than relying on Go's in-memory representation.

A leaf entry's value is a `RecordID`: a uint64 that packs a (pageID, slotIndex) pair, the upper 48 bits being the heap page that physically holds the row and the lower 16 bits being the slot within that page. Packing the pair into one integer keeps every leaf value field a fixed 8 bytes, which keeps the entry layout simple and the capacity arithmetic predictable. `MakeRecordID` shifts the page left 16 and ORs in the slot; `Page` shifts back right 16 and `Slot` truncates to the low 16 bits.

The page layouts are deliberately asymmetric because leaves and internal nodes carry different things. A leaf carries a `nextLeaf` pointer (so range scans can walk the sibling chain) and `(key, value)` entries; an internal node carries one more child pointer than it has keys and no values at all. Both begin with a one-byte node-type tag so the reader can decide which decoder to run before interpreting the rest:

```text
leaf:     [type=1][parentID u64][keyCount u16][nextLeaf u64] then [keyLen u8][key][value u64]...
internal: [type=0][parentID u64][keyCount u16][child0 u64]   then [keyLen u8][key][child u64]...
```

`encodeLeaf` and `decodeLeaf` are mirror images, and so are the internal pair. Encoding walks a cursor `off` forward, writing the one-byte key length, then the key bytes, then the 8-byte value or child pointer; decoding walks the same cursor, reading the same fields in the same order. Because the key length is a single byte, keys are capped at 255 bytes (`MaxKeySize`), which is enforced by the insert path in later exercises. The decoder copies each key into a fresh slice rather than aliasing the page buffer, so a decoded node owns its keys and the caller can reuse or discard the page bytes.

Create `btree.go`:

```go
package btree

import (
	"encoding/binary"
	"fmt"
)

const (
	PageSize   = 4096
	MaxKeySize = 255
)

// RecordID packs a (pageID, slotIndex) pair into a uint64.
// Upper 48 bits: page ID. Lower 16 bits: slot index.
type RecordID uint64

// MakeRecordID packs a page and slot into a RecordID.
func MakeRecordID(page uint64, slot uint16) RecordID {
	return RecordID(page<<16 | uint64(slot))
}

// Page returns the page component of a RecordID.
func (r RecordID) Page() uint64 { return uint64(r) >> 16 }

// Slot returns the slot component of a RecordID.
func (r RecordID) Slot() uint16 { return uint16(r) }

// PageStore abstracts page-level I/O. Callers choose the backing (in-memory,
// file, or buffer-pool manager) by implementing this interface.
type PageStore interface {
	ReadPage(id uint64) ([]byte, error)
	WritePage(id uint64, data []byte) error
	AllocPage() (uint64, error)
}

// MemStore is a fully in-memory PageStore. It is the only store used in tests
// so that tests are hermetic and require no disk I/O.
type MemStore struct {
	pages [][]byte
}

// NewMemStore returns an empty MemStore.
func NewMemStore() *MemStore { return &MemStore{} }

func (m *MemStore) ReadPage(id uint64) ([]byte, error) {
	if id >= uint64(len(m.pages)) {
		return nil, fmt.Errorf("btree: page %d not allocated", id)
	}
	out := make([]byte, PageSize)
	copy(out, m.pages[id])
	return out, nil
}

func (m *MemStore) WritePage(id uint64, data []byte) error {
	if id >= uint64(len(m.pages)) {
		return fmt.Errorf("btree: page %d not allocated", id)
	}
	copy(m.pages[id], data)
	return nil
}

func (m *MemStore) AllocPage() (uint64, error) {
	id := uint64(len(m.pages))
	m.pages = append(m.pages, make([]byte, PageSize))
	return id, nil
}

// -----------------------------------------------------------------------
// Page encoding / decoding
// -----------------------------------------------------------------------

const (
	kindInternal byte = 0
	kindLeaf     byte = 1

	leafHdr  = 19 // 1 type + 8 parent + 2 count + 8 nextLeaf
	innerHdr = 11 // 1 type + 8 parent + 2 count

	// Conservative per-entry size used only to compute node capacity.
	// Actual entries with shorter keys are smaller.
	maxEntrySize = 1 + MaxKeySize + 8 // keyLen + key + value/childPtr

	// Minimum number of entries that fit on a leaf or inner page.
	leafCap  = (PageSize - leafHdr) / maxEntrySize      // 15
	innerCap = (PageSize - innerHdr - 8) / maxEntrySize // 15
)

// node is the decoded in-memory representation of one page.
type node struct {
	kind     byte
	parent   uint64
	nextLeaf uint64     // leaf only; 0 = no right sibling
	keys     [][]byte   // sorted
	vals     []RecordID // leaf: parallel to keys
	children []uint64   // inner: len = len(keys)+1
}

func decodeLeaf(page []byte) *node {
	n := &node{kind: kindLeaf}
	n.parent = binary.BigEndian.Uint64(page[1:9])
	count := int(binary.BigEndian.Uint16(page[9:11]))
	n.nextLeaf = binary.BigEndian.Uint64(page[11:19])
	off := leafHdr
	for i := 0; i < count; i++ {
		klen := int(page[off])
		off++
		k := make([]byte, klen)
		copy(k, page[off:off+klen])
		off += klen
		v := binary.BigEndian.Uint64(page[off : off+8])
		off += 8
		n.keys = append(n.keys, k)
		n.vals = append(n.vals, RecordID(v))
	}
	return n
}

func encodeLeaf(n *node) []byte {
	page := make([]byte, PageSize)
	page[0] = kindLeaf
	binary.BigEndian.PutUint64(page[1:9], n.parent)
	binary.BigEndian.PutUint16(page[9:11], uint16(len(n.keys)))
	binary.BigEndian.PutUint64(page[11:19], n.nextLeaf)
	off := leafHdr
	for i, k := range n.keys {
		page[off] = byte(len(k))
		off++
		copy(page[off:], k)
		off += len(k)
		binary.BigEndian.PutUint64(page[off:off+8], uint64(n.vals[i]))
		off += 8
	}
	return page
}

func decodeInner(page []byte) *node {
	n := &node{kind: kindInternal}
	n.parent = binary.BigEndian.Uint64(page[1:9])
	count := int(binary.BigEndian.Uint16(page[9:11]))
	off := innerHdr
	c0 := binary.BigEndian.Uint64(page[off : off+8])
	off += 8
	n.children = append(n.children, c0)
	for i := 0; i < count; i++ {
		klen := int(page[off])
		off++
		k := make([]byte, klen)
		copy(k, page[off:off+klen])
		off += klen
		c := binary.BigEndian.Uint64(page[off : off+8])
		off += 8
		n.keys = append(n.keys, k)
		n.children = append(n.children, c)
	}
	return n
}

func encodeInner(n *node) []byte {
	page := make([]byte, PageSize)
	page[0] = kindInternal
	binary.BigEndian.PutUint64(page[1:9], n.parent)
	binary.BigEndian.PutUint16(page[9:11], uint16(len(n.keys)))
	off := innerHdr
	binary.BigEndian.PutUint64(page[off:off+8], n.children[0])
	off += 8
	for i, k := range n.keys {
		page[off] = byte(len(k))
		off++
		copy(page[off:], k)
		off += len(k)
		binary.BigEndian.PutUint64(page[off:off+8], n.children[i+1])
		off += 8
	}
	return page
}
```

The `leafCap` and `innerCap` constants are computed from the conservative `maxEntrySize` (a worst-case 255-byte key), so both come out to 15. That is the *minimum* a page guarantees; with short keys a real page holds far more. The small number is intentional for a teaching tree — it lets a test of a few hundred keys exercise multiple levels of splits and merges instead of needing tens of thousands.

### The runnable demo

A test proves the encoding mirrors itself in the abstract; the demo makes the store concrete. It allocates two pages and shows their sequential IDs, packs and unpacks a `RecordID`, then writes a raw page and reads it back to confirm the store hands back an independent 4096-byte copy with the type byte intact. The encode/decode functions are package-private, so the round-trip of a real node is proven in the test; the demo exercises the exported surface a caller actually touches.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/page-encoding-and-store"
)

func main() {
	store := btree.NewMemStore()
	a, _ := store.AllocPage()
	b, _ := store.AllocPage()
	fmt.Printf("allocated pages: %d %d\n", a, b)

	rid := btree.MakeRecordID(42, 7)
	fmt.Printf("recordid page=%d slot=%d\n", rid.Page(), rid.Slot())

	page := make([]byte, btree.PageSize)
	page[0] = 1 // a leaf-type marker byte
	if err := store.WritePage(a, page); err != nil {
		fmt.Println("write error:", err)
		return
	}
	got, err := store.ReadPage(a)
	if err != nil {
		fmt.Println("read error:", err)
		return
	}
	fmt.Printf("page %d size=%d type=%d\n", a, len(got), got[0])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
allocated pages: 0 1
recordid page=42 slot=7
page 0 size=4096 type=1
```

### Tests

`TestRecordIDRoundTrip` checks that packing then unpacking recovers the original page and slot across the full range, including the largest 48-bit page and 16-bit slot. `TestPageRoundTripLeaf` and `TestPageRoundTripInner` build a node, encode it to a page, decode it back, and assert every field — parent, sibling pointer, keys, and values or children — survives unchanged. These are the properties the rest of the tree depends on without ever re-checking.

Create `btree_test.go`:

```go
package btree

import (
	"bytes"
	"testing"
)

func TestRecordIDRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		page uint64
		slot uint16
	}{
		{0, 0},
		{1, 0},
		{1, 42},
		{0x0000FFFFFFFFFFFF, 0xFFFF},
	}
	for _, tc := range cases {
		rid := MakeRecordID(tc.page, tc.slot)
		if got := rid.Page(); got != tc.page {
			t.Errorf("Page: got %d want %d", got, tc.page)
		}
		if got := rid.Slot(); got != tc.slot {
			t.Errorf("Slot: got %d want %d", got, tc.slot)
		}
	}
}

func TestPageRoundTripLeaf(t *testing.T) {
	t.Parallel()
	orig := &node{
		kind:     kindLeaf,
		parent:   7,
		nextLeaf: 42,
		keys:     [][]byte{[]byte("hello"), []byte("world")},
		vals:     []RecordID{MakeRecordID(1, 0), MakeRecordID(2, 1)},
	}
	got := decodeLeaf(encodeLeaf(orig))

	if got.parent != orig.parent || got.nextLeaf != orig.nextLeaf {
		t.Fatalf("header mismatch: got parent=%d next=%d", got.parent, got.nextLeaf)
	}
	if len(got.keys) != len(orig.keys) {
		t.Fatalf("key count mismatch: got %d want %d", len(got.keys), len(orig.keys))
	}
	for i := range orig.keys {
		if !bytes.Equal(got.keys[i], orig.keys[i]) {
			t.Errorf("keys[%d]: got %q want %q", i, got.keys[i], orig.keys[i])
		}
		if got.vals[i] != orig.vals[i] {
			t.Errorf("vals[%d]: got %v want %v", i, got.vals[i], orig.vals[i])
		}
	}
}

func TestPageRoundTripInner(t *testing.T) {
	t.Parallel()
	orig := &node{
		kind:     kindInternal,
		parent:   3,
		keys:     [][]byte{[]byte("m"), []byte("t")},
		children: []uint64{10, 20, 30},
	}
	got := decodeInner(encodeInner(orig))

	if got.parent != orig.parent {
		t.Fatalf("parent mismatch: got %d want %d", got.parent, orig.parent)
	}
	if len(got.children) != len(orig.children) {
		t.Fatalf("child count: got %d want %d", len(got.children), len(orig.children))
	}
	for i := range orig.children {
		if got.children[i] != orig.children[i] {
			t.Errorf("children[%d]: got %d want %d", i, got.children[i], orig.children[i])
		}
	}
	for i := range orig.keys {
		if !bytes.Equal(got.keys[i], orig.keys[i]) {
			t.Errorf("keys[%d]: got %q want %q", i, got.keys[i], orig.keys[i])
		}
	}
}
```

## Review

The encoding is sound when each decoder is the exact mirror of its encoder: both walk the same `off` cursor through one-byte length, key bytes, and an 8-byte value or child pointer, in the same order. Confirm that an internal node always carries exactly one more child than it has keys — the `child0` pointer written before the loop is what makes `len(children) == len(keys)+1` — and that a decoded node owns its keys, since each is copied into a fresh slice rather than aliased into the page buffer. The two round-trip tests plus the `RecordID` packing test passing under `go test -race ./...` establish that pages survive a write/read cycle byte-for-byte.

The pitfalls here are all off-by-one or aliasing errors. Forgetting to write `child0` before the key/child loop, or reading it back, shifts every child pointer by one and silently mis-routes searches. Aliasing a key with `n.keys = append(n.keys, page[off:off+klen])` instead of copying it into a fresh slice leaves the node pointing into a buffer the store may overwrite, which corrupts keys the moment the page is reused. And confusing big-endian with little-endian between encode and decode breaks ordering for multi-byte integers without any obvious crash. Keeping encode and decode adjacent in the file and reading them as a pair is the cheapest defense.

## Resources

- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — fixed-width big-endian encoding of the page header fields and entry pointers.
- [`bytes`](https://pkg.go.dev/bytes) — `bytes.Equal`, used to compare decoded keys against the originals.
- [CMU 15-445/645 Fall 2024, B+Tree Indexes](https://15445.courses.cs.cmu.edu/fall2024/slides/08-indexes1.pdf) — node layout, slotted pages, and why on-disk indexes address nodes by page ID.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-insert-and-node-splits.md](02-insert-and-node-splits.md)
