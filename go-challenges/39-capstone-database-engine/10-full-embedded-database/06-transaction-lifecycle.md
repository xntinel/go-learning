# Exercise 6: Transaction Lifecycle Through a Store API

A transaction's defining promise is isolation: the writes it makes are its own until it commits, visible to itself but to no one else, and they either all become durable at commit or all vanish at rollback. This exercise demonstrates that contract end to end with `TxnStore`, a minimal transactional layer over a heap file. Each open transaction buffers its inserts privately; a select inside the transaction sees them (read-your-own-writes), a rollback discards them, and a commit flushes them into the shared heap where later readers finally see them. It is the smallest model that still proves the BEGIN to COMMIT-or-ROLLBACK lifecycle the executor and a full MVCC manager provide with per-row versions.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including its own heap file and slotted page — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
txn.go               TxnStore, TxID, NewTxnStore, Begin, Insert, Select, Commit, Rollback, CommittedCount
heapfile.go          HeapFile, TID, PageID (the committed-row storage)
page.go              SlottedPage (per-page storage)
cmd/
  demo/
    main.go           begin, read-own-write, rollback; then begin, commit, observe visibility
txn_test.go           rollback is invisible, commit is visible, unknown-txn errors
```

- Files: `txn.go`, `heapfile.go`, `page.go`, `cmd/demo/main.go`, `txn_test.go`.
- Implement: `TxnStore` with `Begin() TxID`, `Insert(tx TxID, row []byte) error`, `Select(tx TxID) ([][]byte, error)`, `Commit`, `Rollback`, and `CommittedCount`, plus `NewTxnStore`.
- Test: `txn_test.go` proves a transaction sees its own pending insert, a rollback leaves nothing visible to a later reader, a commit is visible to a later reader, and every operation on an unknown transaction returns `ErrNoTxn`.
- Verify: `go test -run 'TestTxn|ExampleTxnStore' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/10-full-embedded-database/06-transaction-lifecycle/cmd/demo && cd go-solutions/39-capstone-database-engine/10-full-embedded-database/06-transaction-lifecycle
```

### Why writes are private until commit, and what read-your-own-writes means

The contract this store models has three observable rules, and each maps to one line of the implementation. A transaction's inserts are buffered in a per-transaction slice and not touched into the shared heap until commit — that is what makes them private. A select issued *inside* the transaction returns the committed rows followed by that transaction's own pending rows — that is read-your-own-writes, the rule that a transaction always sees the effect of its own statements even before it commits, which is what lets a single transaction insert a row and then update it. A rollback deletes the buffer without ever touching the heap, so the rows simply cease to exist; a commit walks the buffer and inserts each row into the heap, after which a *new* transaction's select sees them. The pivotal asymmetry is that visibility to others is gated on commit while visibility to self is immediate.

The buffering is also why every entry point first looks the transaction up and returns `ErrNoTxn` if it is absent. A transaction identifier that is not in the pending map is either never-begun or already-finished (committed or rolled back), and in both cases there is no buffer to read from or write to, so the operation is a programming error the API must reject rather than silently create state for. Insert, Select, Commit, and Rollback all share this guard, and the test exercises all four against an unknown identifier to prove none of them quietly succeeds.

This is deliberately a teaching model, not full MVCC: there is a single writer's buffer at a time and no snapshot versioning, so it cannot show two concurrent transactions each reading a consistent snapshot while the other writes. What it does show, exactly and testably, is the lifecycle every richer scheme is built on — begin, write, read-your-own-write, then either commit to publish or roll back to discard. Defensive copying appears twice and both copies matter: Insert copies the caller's row so a later mutation of the caller's slice cannot rewrite buffered data, and Select copies each heap tuple so a reader cannot mutate committed storage through the returned slices.

Create `txn.go`:

```go
package txnstore

import (
	"errors"
	"fmt"
)

// TxID identifies an open transaction.
type TxID uint64

// ErrNoTxn is returned when an operation names a transaction that is not open.
var ErrNoTxn = errors.New("transaction not open")

// TxnStore is a minimal transactional row store layered over a HeapFile. Each open
// transaction buffers its own inserts; a Select issued inside the transaction sees
// those uncommitted rows (read-your-own-writes), but they become visible to other
// readers only after Commit. Rollback discards the buffer. This is the isolation
// contract an executor relies on: a transaction's writes are private until commit.
//
// This is a teaching model, not full MVCC: there is a single writer's buffer at a
// time and no snapshot versioning. It demonstrates the BEGIN -> write ->
// read-own-write -> COMMIT/ROLLBACK lifecycle that a real TxManager implements with
// per-row versions.
type TxnStore struct {
	heap    *HeapFile
	nextTx  TxID
	pending map[TxID][][]byte
}

// NewTxnStore returns an empty transactional store.
func NewTxnStore() *TxnStore {
	return &TxnStore{
		heap:    NewHeapFile(),
		nextTx:  1,
		pending: make(map[TxID][][]byte),
	}
}

// Begin opens a transaction and returns its ID.
func (s *TxnStore) Begin() TxID {
	id := s.nextTx
	s.nextTx++
	s.pending[id] = nil
	return id
}

// Insert buffers a row in the transaction. The row is invisible to other
// transactions until Commit.
func (s *TxnStore) Insert(tx TxID, row []byte) error {
	buf, ok := s.pending[tx]
	if !ok {
		return fmt.Errorf("%w: tx %d", ErrNoTxn, tx)
	}
	cp := make([]byte, len(row))
	copy(cp, row)
	s.pending[tx] = append(buf, cp)
	return nil
}

// Select returns every row visible to tx: all committed rows followed by this
// transaction's own pending rows (read-your-own-writes).
func (s *TxnStore) Select(tx TxID) ([][]byte, error) {
	buf, ok := s.pending[tx]
	if !ok {
		return nil, fmt.Errorf("%w: tx %d", ErrNoTxn, tx)
	}
	var out [][]byte
	err := s.heap.Scan(func(_ TID, tuple []byte) error {
		cp := make([]byte, len(tuple))
		copy(cp, tuple)
		out = append(out, cp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return append(out, buf...), nil
}

// Commit flushes the transaction's pending rows into the committed heap and closes
// the transaction.
func (s *TxnStore) Commit(tx TxID) error {
	buf, ok := s.pending[tx]
	if !ok {
		return fmt.Errorf("%w: tx %d", ErrNoTxn, tx)
	}
	for _, row := range buf {
		if _, err := s.heap.Insert(row); err != nil {
			return err
		}
	}
	delete(s.pending, tx)
	return nil
}

// Rollback discards the transaction's pending rows and closes it.
func (s *TxnStore) Rollback(tx TxID) error {
	if _, ok := s.pending[tx]; !ok {
		return fmt.Errorf("%w: tx %d", ErrNoTxn, tx)
	}
	delete(s.pending, tx)
	return nil
}

// CommittedCount returns the number of committed rows visible to a fresh reader.
func (s *TxnStore) CommittedCount() int {
	n := 0
	_ = s.heap.Scan(func(TID, []byte) error {
		n++
		return nil
	})
	return n
}
```

The committed rows live in a heap file, the same access-method abstraction from the heap-file exercise, carried here so this module stands alone.

Create `heapfile.go`:

```go
package txnstore

import (
	"errors"
	"fmt"
)

// PageID is the index of a page within the heap file.
type PageID uint32

// TID is the physical address of a tuple: which page, and which slot inside it.
type TID struct {
	Page PageID
	Slot int
}

// ErrTupleNotFound is returned when a TID does not name a live tuple.
var ErrTupleNotFound = errors.New("tuple not found")

// HeapFile is an ordered collection of slotted pages that grows by appending a tail
// page when no existing page can fit a tuple.
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
// none fits, and returns the tuple's stable TID.
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

// Scan calls fn for every live (non-tombstoned) tuple in TID order.
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
```

The per-page storage is the slotted page from the storage exercise, trimmed to what the heap file uses here.

Create `page.go`:

```go
package txnstore

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
```

### The runnable demo

The demo walks the lifecycle twice. First it begins a transaction, inserts a row, reads its own write back (one row visible), then rolls back and shows the committed store is still empty. Then it begins a second transaction, inserts and commits, and shows a fresh reader now sees one committed row — visibility gated exactly on commit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/txn-store"
)

func main() {
	s := txnstore.NewTxnStore()

	// A transaction sees its own write, then rolls it back: nothing is committed.
	tx := s.Begin()
	if err := s.Insert(tx, []byte("alice")); err != nil {
		log.Fatal(err)
	}
	own, err := s.Select(tx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("in-txn rows=%d\n", len(own))
	if err := s.Rollback(tx); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after rollback, committed rows=%d\n", s.CommittedCount())

	// A second transaction commits: a fresh reader now sees the row.
	tx2 := s.Begin()
	if err := s.Insert(tx2, []byte("bob")); err != nil {
		log.Fatal(err)
	}
	if err := s.Commit(tx2); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("after commit, committed rows=%d\n", s.CommittedCount())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-txn rows=1
after rollback, committed rows=0
after commit, committed rows=1
```

### Tests

The tests pin the visibility contract from both directions. The rollback test inserts inside a transaction, confirms read-your-own-writes sees the row, rolls back, and proves a *new* transaction sees nothing and the committed count is zero. The commit test inserts, commits, and proves a new transaction sees the row. The unknown-transaction test runs all four operations against an identifier that was never begun and asserts each returns `ErrNoTxn`.

Create `txn_test.go`:

```go
package txnstore

import (
	"errors"
	"fmt"
	"testing"
)

func TestTxnRollbackIsInvisible(t *testing.T) {
	t.Parallel()

	s := NewTxnStore()
	tx := s.Begin()
	if err := s.Insert(tx, []byte("scratch")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Read-your-own-write: the open transaction sees its pending row.
	own, err := s.Select(tx)
	if err != nil {
		t.Fatalf("Select in txn: %v", err)
	}
	if len(own) != 1 {
		t.Fatalf("in-txn Select = %d rows, want 1", len(own))
	}
	if err := s.Rollback(tx); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// A new transaction must not see the rolled-back row.
	tx2 := s.Begin()
	after, err := s.Select(tx2)
	if err != nil {
		t.Fatalf("Select after rollback: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("post-rollback Select = %d rows, want 0", len(after))
	}
	if s.CommittedCount() != 0 {
		t.Fatalf("CommittedCount = %d, want 0", s.CommittedCount())
	}
}

func TestTxnCommitIsVisible(t *testing.T) {
	t.Parallel()

	s := NewTxnStore()
	tx := s.Begin()
	if err := s.Insert(tx, []byte("kept")); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.Commit(tx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	tx2 := s.Begin()
	rows, err := s.Select(tx2)
	if err != nil {
		t.Fatalf("Select after commit: %v", err)
	}
	if len(rows) != 1 || string(rows[0]) != "kept" {
		t.Fatalf("post-commit Select = %v, want [kept]", rows)
	}
}

func TestTxnUnknownTransaction(t *testing.T) {
	t.Parallel()

	s := NewTxnStore()
	tests := []struct {
		name string
		op   func() error
	}{
		{"insert", func() error { return s.Insert(999, []byte("x")) }},
		{"select", func() error { _, err := s.Select(999); return err }},
		{"commit", func() error { return s.Commit(999) }},
		{"rollback", func() error { return s.Rollback(999) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.op(); !errors.Is(err, ErrNoTxn) {
				t.Fatalf("%s on unknown txn: err = %v, want ErrNoTxn", tc.name, err)
			}
		})
	}
}

func ExampleTxnStore() {
	s := NewTxnStore()
	tx := s.Begin()
	_ = s.Insert(tx, []byte("alice"))
	own, _ := s.Select(tx)
	fmt.Printf("in-txn rows=%d\n", len(own))
	_ = s.Rollback(tx)
	fmt.Printf("committed rows=%d\n", s.CommittedCount())
	// Output:
	// in-txn rows=1
	// committed rows=0
}
```

## Review

The store is correct when visibility flips exactly at commit. Confirm read-your-own-writes — an open transaction sees its pending insert — and that a rollback leaves a later reader seeing nothing and a committed count of zero, while a commit makes the row visible to a fresh transaction. Confirm that all four operations reject an unknown transaction with `ErrNoTxn`, so a finished or never-begun identifier cannot silently create or read state. The defensive copies on insert and select are part of correctness, not decoration: without them a caller's later mutation could rewrite buffered or committed bytes.

Common mistakes for this lifecycle. Writing inserts straight into the heap instead of a per-transaction buffer destroys isolation — other readers see uncommitted rows. Omitting the transaction's own pending rows from its select breaks read-your-own-writes, so a transaction cannot see the effect of its own statements. Forgetting to delete the buffer on commit or rollback leaks memory and lets a reused identifier read stale rows. Returning the heap's internal slices from select lets a reader corrupt committed storage.

## Resources

- [Architecture of a Database System (Hellerstein, Stonebraker, Hamilton)](https://dsf.berkeley.edu/papers/fntdb07-architecture.pdf) — its transactions section covers isolation, visibility, and the commit/abort lifecycle this store models.
- [CMU 15-445: Database Systems](https://15445.courses.cs.cmu.edu/) — the Concurrency Control and MVCC lectures derive read-your-own-writes and snapshot visibility from first principles.
- [SQLite: Isolation in SQLite](https://www.sqlite.org/isolation.html) — how a production embedded engine defines what a transaction sees of its own and others' writes.

---

Back to [05-heap-file-pages.md](05-heap-file-pages.md) | Back to [00-concepts.md](00-concepts.md)
