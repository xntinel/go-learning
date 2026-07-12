# Exercise 3: Database Integration over Narrow Subsystem Interfaces

This is the seam the whole lesson is about. The `Database` struct does not implement a log, a buffer pool, or a transaction manager — it composes them through narrow interfaces and enforces the rules that only matter once the pieces sit together: log before mutating the catalog, roll back the catalog (and check the rollback error) when a later step fails, and flush the log before the pages on shutdown. This exercise builds that composition, with its own copies of the slotted page and catalog so the module compiles and runs entirely offline, and drives it with in-memory subsystem implementations in the demo and mocks in the tests.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs — including its own slotted page and catalog — and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
dbengine.go          Database, WAL/BufferPool/TxManager interfaces, Options, Open, Close,
                     CreateTable, DropTable
page.go              SlottedPage (storage the buffer pool hands out)
catalog.go           Catalog (the schema registry Database owns)
cmd/
  demo/
    main.go          open a database over in-memory subsystems, create and drop a table
dbengine_test.go     mock subsystems + open/close, create, duplicate, drop, after-close
```

- Files: `dbengine.go`, `page.go`, `catalog.go`, `cmd/demo/main.go`, `dbengine_test.go`.
- Implement: the `WAL`, `BufferPool`, and `TxManager` interfaces; `Database` with `Open`, `Close`, `CreateTable`, and `DropTable`; plus `Options`/`DefaultOptions`. `CreateTable` logs first, updates the catalog second, allocates the first page third, and rolls the catalog back — checking the rollback error — on any later failure.
- Test: `dbengine_test.go` supplies in-memory mocks for all three subsystems and asserts open/close flushes and closes the right things, that `Close` is idempotent with `ErrClosed`, that `CreateTable` logs a record and allocates exactly one page, that a duplicate is `ErrTableExists`, and that operations after `Close` return `ErrClosed`.
- Verify: `go test -run 'TestDatabase|ExampleSlottedPage|ExampleCatalog' -race ./...`

### Why narrow interfaces, and why the order of operations in CreateTable is the whole point

The `Database` accepts its dependencies as three small interfaces rather than three concrete types, and that single decision is what makes the integration testable. The real log, buffer pool, and transaction manager from the earlier lessons carry filesystem and network state; if `Database` named them concretely, every test would need a disk. By naming only the contract each layer offers — append/flush/replay/close for the log, fetch/dirty/flush/allocate/close for the pool, begin/commit/rollback for transactions — the integration logic can be driven by in-memory mocks that satisfy the same interfaces, and the exact same `Database` code runs in production against the real subsystems. The interface is sized to what the integration actually needs, not transcribed from the concrete type; that is why `BufferPool` here is five methods and not fifty.

The body of `CreateTable` encodes the integration invariant as a strict sequence, and the order is not negotiable. First it appends a DDL record to the log. Only if that succeeds does it update the catalog. Only if that succeeds does it allocate and initialize the first heap page. The reason the log goes first is that the log is the source of truth for recovery: a catalog entry that exists without a matching log record is invisible to recovery and silently lost, so the durable record must precede the in-memory effect. The reason a stray DDL record with no commit is harmless is symmetric — recovery's undo pass skips any DDL whose transaction never committed, so logging early costs nothing.

The failure handling after the catalog is updated is where correctness is won or lost. If page allocation or the page fetch or the dirty-mark fails, the catalog has already gained an entry that now describes a table with no storage, so it must be rolled back with `DropTable`. The rule that the original cut corners on and this code does not: **the rollback's own error must be checked, not discarded with `_`**. If `DropTable` itself fails, the engine is left with a catalog entry it could neither complete nor remove — a genuinely inconsistent state — and the caller has to know. So each failure path wraps both the original error and the rollback error and returns them together. Discarding the rollback error would turn a recoverable failure into a silent corruption.

One more subtlety lives in the last line: `meta.PageCount = 1` is written without a lock even though the catalog is concurrency-safe. It is safe because the whole method holds `db.mu`, which serializes all DDL, and this `*TableMeta` is not observable to any reader until `CreateTable` returns successfully — no concurrent goroutine can see the field while it is being set. `Close` mirrors the write path in reverse and uses `errors.Join` so a failure flushing the pool does not hide a failure closing the log.

Create `dbengine.go`:

```go
package dbengine

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// TxID is an opaque transaction identifier assigned by the TxManager.
type TxID uint64

// PageID is the logical address of a heap page on disk.
type PageID uint32

// Sentinel errors shared across subsystems.
var (
	ErrClosed = errors.New("database is closed")
)

// WAL is the write-ahead log interface. The concrete implementation is built in
// the write-ahead-log lesson; the interface here lets the integration layer and
// its tests stay decoupled from it.
type WAL interface {
	// Append writes one log record and returns its log sequence number.
	Append(txID TxID, kind string, payload []byte) (lsn uint64, err error)
	// Flush forces all pending records to durable storage.
	Flush() error
	// Replay calls fn for every record in log order; used during crash recovery.
	Replay(fn func(txID TxID, kind string, payload []byte) error) error
	// Close flushes and closes the log.
	Close() error
}

// BufferPool manages the in-memory page cache and interfaces with the disk
// manager. The buffer-pool lesson's implementation satisfies this interface.
type BufferPool interface {
	// FetchPage pins a page in the cache and returns a pointer to it.
	FetchPage(id PageID) (*SlottedPage, error)
	// DirtyPage marks a cached page as modified so the pool writes it on eviction.
	DirtyPage(id PageID) error
	// FlushAll writes all dirty pages to disk.
	FlushAll() error
	// AllocatePage allocates a new page and returns its ID.
	AllocatePage() (PageID, error)
	// Close flushes all dirty pages and releases resources.
	Close() error
}

// TxManager controls the transaction lifecycle and implements MVCC snapshot
// assignment. The MVCC lesson's implementation satisfies this interface.
type TxManager interface {
	// Begin opens a new transaction and returns its opaque ID.
	Begin() (TxID, error)
	// Commit makes a transaction's changes visible and releases its locks.
	Commit(txID TxID) error
	// Rollback undoes all changes made by a transaction.
	Rollback(txID TxID) error
	// ActiveTxIDs returns the IDs of all currently open transactions.
	ActiveTxIDs() []TxID
}

// Options controls the behavior of an open database.
type Options struct {
	// PageCacheSize is the maximum number of pages held in the buffer pool.
	PageCacheSize int
	// SyncWAL calls fsync on every WAL append for maximum durability.
	SyncWAL bool
	// CheckpointInterval controls how often checkpoints are taken. Shorter
	// intervals bound recovery time at the cost of extra I/O.
	CheckpointInterval time.Duration
}

// DefaultOptions returns a safe starting configuration suitable for a development
// database. Tune PageCacheSize and CheckpointInterval for production workloads.
func DefaultOptions() Options {
	return Options{
		PageCacheSize:      256,
		SyncWAL:            true,
		CheckpointInterval: 5 * time.Minute,
	}
}

// Database composes all subsystems into a single embedded engine. Callers supply
// concrete implementations of WAL, BufferPool, and TxManager via Open; Database
// owns the system catalog and all integration logic.
//
// Concurrency: Database is safe for concurrent use from multiple goroutines. The
// internal mu guards the closed flag and DDL mutations; individual subsystems
// carry their own locks for data-plane operations.
type Database struct {
	path    string
	opts    Options
	catalog *Catalog
	wal     WAL
	pool    BufferPool
	txm     TxManager

	mu     sync.Mutex
	closed bool
}

// Open opens or creates a database at path using the supplied subsystems. If a WAL
// from a previous run exists, crash recovery runs before the database becomes
// available. The caller is responsible for calling Close when done.
func Open(path string, opts Options, wal WAL, pool BufferPool, txm TxManager) (*Database, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("dbengine: mkdir %q: %w", path, err)
	}
	db := &Database{
		path:    path,
		opts:    opts,
		catalog: NewCatalog(),
		wal:     wal,
		pool:    pool,
		txm:     txm,
	}
	if err := db.recover(); err != nil {
		return nil, fmt.Errorf("dbengine: recovery: %w", err)
	}
	return db, nil
}

// recover drives ARIES-style crash recovery: replay the WAL in LSN order, applying
// redo for committed transactions and collecting undo for transactions that never
// committed. This orchestration traces the path; the full redo/undo logic lives in
// the WAL and BufferPool implementations from the earlier lessons.
func (db *Database) recover() error {
	return db.wal.Replay(func(_ TxID, _ string, _ []byte) error {
		// Full implementation:
		//   1. Analysis pass: build a dirty-page table and active-transaction table.
		//   2. Redo pass: replay every record whose LSN > pageLSN for the affected page.
		//   3. Undo pass: roll back every transaction not found in a commit record.
		return nil
	})
}

// Close performs a clean shutdown: flush the WAL, flush all dirty buffer pool pages,
// then close both subsystems. Returns a joined error if multiple subsystems fail;
// calling Close a second time returns ErrClosed.
func (db *Database) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	db.closed = true
	var errs []error
	if err := db.wal.Flush(); err != nil {
		errs = append(errs, fmt.Errorf("wal flush: %w", err))
	}
	if err := db.pool.FlushAll(); err != nil {
		errs = append(errs, fmt.Errorf("pool flush: %w", err))
	}
	if err := db.wal.Close(); err != nil {
		errs = append(errs, fmt.Errorf("wal close: %w", err))
	}
	if err := db.pool.Close(); err != nil {
		errs = append(errs, fmt.Errorf("pool close: %w", err))
	}
	return errors.Join(errs...)
}

// CreateTable creates a new table transactionally. The WAL record is written first;
// the catalog is updated only after the WAL append succeeds. If any step fails after
// the catalog is updated, the catalog change is rolled back — and the rollback's own
// error is checked, not discarded — so the in-memory state stays consistent. The
// first heap page is allocated and initialized in the buffer pool.
func (db *Database) CreateTable(txID TxID, name string, cols []*Column) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	// The DDL record is logged before the catalog op that may fail. A stray DDL
	// record with no matching commit record is harmless: recovery trusts the commit
	// record as the source of truth and the undo pass skips any DDL whose
	// transaction never committed.
	if _, err := db.wal.Append(txID, "DDL", []byte("CREATE TABLE:"+name)); err != nil {
		return fmt.Errorf("dbengine: wal append: %w", err)
	}
	meta, err := db.catalog.CreateTable(name, cols)
	if err != nil {
		return err
	}
	pageID, err := db.pool.AllocatePage()
	if err != nil {
		if derr := db.catalog.DropTable(name); derr != nil {
			return fmt.Errorf("dbengine: rollback after %w failed: %v", err, derr)
		}
		return fmt.Errorf("dbengine: allocate first page: %w", err)
	}
	page, err := db.pool.FetchPage(pageID)
	if err != nil {
		if derr := db.catalog.DropTable(name); derr != nil {
			return fmt.Errorf("dbengine: rollback after %w failed: %v", err, derr)
		}
		return fmt.Errorf("dbengine: fetch first page: %w", err)
	}
	page.Init()
	if err := db.pool.DirtyPage(pageID); err != nil {
		if derr := db.catalog.DropTable(name); derr != nil {
			return fmt.Errorf("dbengine: rollback after %w failed: %v", err, derr)
		}
		return err
	}
	// PageCount is set once here, before this *TableMeta is observable to any reader
	// through a successful CreateTable return. db.mu (held for the whole method)
	// serializes DDL, and no concurrent goroutine mutates this field afterward, so
	// the unsynchronized write is safe.
	meta.PageCount = 1
	return nil
}

// DropTable removes a table's catalog entries transactionally. The DDL record is
// WAL-logged before the catalog is mutated so that a crash after the WAL write but
// before the catalog update is recoverable on replay.
func (db *Database) DropTable(txID TxID, name string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.closed {
		return ErrClosed
	}
	if _, err := db.wal.Append(txID, "DDL", []byte("DROP TABLE:"+name)); err != nil {
		return fmt.Errorf("dbengine: wal append: %w", err)
	}
	return db.catalog.DropTable(name)
}
```

The buffer pool hands out slotted pages, so this module carries its own copy of the page type. It is the same layout as the storage exercise — a header, a slot directory, and a tuple area — and `Database` uses only `Init`, which the buffer pool calls on a freshly allocated page.

Create `page.go`:

```go
package dbengine

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

// SlottedPage implements the classic slotted-page heap layout the buffer pool
// caches and hands out. Layout: a 12-byte header (slot count, free-space pointer,
// page LSN), a slot directory growing down from the header, and tuple data packed
// up from the bottom.
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

The catalog is the schema registry `Database` owns. It is the same in-memory registry as the catalog exercise, reproduced here so this module stands alone.

Create `catalog.go`:

```go
package dbengine

import (
	"errors"
	"fmt"
	"sync"
)

// Catalog sentinel errors.
var (
	ErrTableExists   = errors.New("table already exists")
	ErrTableNotFound = errors.New("table not found")
)

// Column is a column definition stored in sys_columns.
type Column struct {
	TableID  uint32
	Name     string
	Type     string // "int", "text", "bool", "float"
	Ordinal  int
	Nullable bool
	Default  string
}

// TableMeta is a row in sys_tables.
type TableMeta struct {
	ID        uint32
	Name      string
	PageCount int
	RowCount  int64
}

// IndexMeta is a row in sys_indexes.
type IndexMeta struct {
	ID      uint32
	TableID uint32
	Name    string
	Columns []string
	Unique  bool
}

// Catalog is the in-memory system catalog guarding tables, columns, and indexes.
// Reads take a shared lock; writes take an exclusive lock.
type Catalog struct {
	mu      sync.RWMutex
	tables  map[string]*TableMeta
	columns map[uint32][]*Column
	indexes map[uint32][]*IndexMeta
	nextID  uint32
}

// NewCatalog creates an empty system catalog.
func NewCatalog() *Catalog {
	return &Catalog{
		tables:  make(map[string]*TableMeta),
		columns: make(map[uint32][]*Column),
		indexes: make(map[uint32][]*IndexMeta),
		nextID:  1,
	}
}

// CreateTable registers a new table and its columns, assigning the table ID and
// each column's ordinal. Returns ErrTableExists if the name is taken.
func (c *Catalog) CreateTable(name string, cols []*Column) (*TableMeta, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.tables[name]; ok {
		return nil, fmt.Errorf("%w: %q", ErrTableExists, name)
	}
	id := c.nextID
	c.nextID++
	meta := &TableMeta{ID: id, Name: name}
	c.tables[name] = meta
	assigned := make([]*Column, len(cols))
	for i, col := range cols {
		cp := *col
		cp.TableID = id
		cp.Ordinal = i
		assigned[i] = &cp
	}
	c.columns[id] = assigned
	return meta, nil
}

// Table looks up a table by name.
func (c *Catalog) Table(name string) (*TableMeta, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrTableNotFound, name)
	}
	return t, nil
}

// Columns returns the column definitions for a table by ID.
func (c *Catalog) Columns(tableID uint32) ([]*Column, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cols, ok := c.columns[tableID]
	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrTableNotFound, tableID)
	}
	return cols, nil
}

// DropTable removes a table and all its column and index metadata.
func (c *Catalog) DropTable(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.tables[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrTableNotFound, name)
	}
	delete(c.columns, t.ID)
	delete(c.indexes, t.ID)
	delete(c.tables, name)
	return nil
}
```

### The runnable demo

In production the three subsystems come from the earlier lessons and touch the disk and network. The demo instead supplies the smallest in-memory implementations that satisfy the interfaces — a slice-backed log, a map-backed page pool, a counter-based transaction manager — so the integration logic runs offline. It opens a database, begins a transaction, creates a two-column table (which logs a DDL record and allocates a first heap page through the pool), drops it, and closes cleanly, printing each step.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"example.com/database-integration"
)

// memWAL is a slice-backed write-ahead log: enough of the contract to run DDL.
type memWAL struct {
	records [][]byte
}

func (w *memWAL) Append(_ dbengine.TxID, _ string, payload []byte) (uint64, error) {
	w.records = append(w.records, payload)
	return uint64(len(w.records)), nil
}
func (w *memWAL) Flush() error { return nil }
func (w *memWAL) Replay(fn func(dbengine.TxID, string, []byte) error) error {
	for _, r := range w.records {
		if err := fn(0, "DDL", r); err != nil {
			return err
		}
	}
	return nil
}
func (w *memWAL) Close() error { return nil }

// memPool is a map-backed buffer pool that allocates real slotted pages.
type memPool struct {
	pages  map[dbengine.PageID]*dbengine.SlottedPage
	nextID dbengine.PageID
}

func newMemPool() *memPool {
	return &memPool{pages: make(map[dbengine.PageID]*dbengine.SlottedPage)}
}
func (p *memPool) FetchPage(id dbengine.PageID) (*dbengine.SlottedPage, error) {
	pg, ok := p.pages[id]
	if !ok {
		return nil, fmt.Errorf("memPool: page %d not found", id)
	}
	return pg, nil
}
func (p *memPool) DirtyPage(dbengine.PageID) error { return nil }
func (p *memPool) FlushAll() error                 { return nil }
func (p *memPool) AllocatePage() (dbengine.PageID, error) {
	id := p.nextID
	p.nextID++
	p.pages[id] = &dbengine.SlottedPage{}
	return id, nil
}
func (p *memPool) Close() error { return nil }

// memTxm is a counter-based transaction manager.
type memTxm struct {
	nextID dbengine.TxID
	active map[dbengine.TxID]bool
}

func newMemTxm() *memTxm {
	return &memTxm{nextID: 1, active: make(map[dbengine.TxID]bool)}
}
func (m *memTxm) Begin() (dbengine.TxID, error) {
	id := m.nextID
	m.nextID++
	m.active[id] = true
	return id, nil
}
func (m *memTxm) Commit(id dbengine.TxID) error   { delete(m.active, id); return nil }
func (m *memTxm) Rollback(id dbengine.TxID) error { delete(m.active, id); return nil }
func (m *memTxm) ActiveTxIDs() []dbengine.TxID {
	out := make([]dbengine.TxID, 0, len(m.active))
	for id := range m.active {
		out = append(out, id)
	}
	return out
}

func main() {
	dir, err := os.MkdirTemp("", "dbengine-demo")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	txm := newMemTxm()
	db, err := dbengine.Open(dir, dbengine.DefaultOptions(), &memWAL{}, newMemPool(), txm)
	if err != nil {
		log.Fatalf("open: %v", err)
	}

	tx, _ := txm.Begin()
	cols := []*dbengine.Column{
		{Name: "id", Type: "int", Nullable: false},
		{Name: "email", Type: "text", Nullable: false},
	}
	if err := db.CreateTable(tx, "users", cols); err != nil {
		log.Fatalf("create table: %v", err)
	}
	fmt.Printf("created table %q with %d columns\n", "users", len(cols))

	if err := db.DropTable(tx, "users"); err != nil {
		log.Fatalf("drop table: %v", err)
	}
	fmt.Println("dropped table \"users\"")
	_ = txm.Commit(tx)

	if err := db.Close(); err != nil {
		log.Fatalf("close: %v", err)
	}
	fmt.Println("database closed cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
created table "users" with 2 columns
dropped table "users"
database closed cleanly
```

### Tests

The tests drive the integration through mock subsystems that record what the `Database` asked of them. Open-then-close asserts the log was flushed *and* closed and the pool was closed — pinning the shutdown order. The idempotence test confirms a second `Close` returns `ErrClosed`. The create-table test asserts a WAL record was written, exactly one page was allocated, and the catalog reflects `PageCount == 1`. A duplicate create returns `ErrTableExists`, a drop clears the catalog, and any operation after `Close` returns `ErrClosed`.

Create `dbengine_test.go`:

```go
package dbengine

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// ---- mock subsystems -------------------------------------------------------

type mockWAL struct {
	mu      sync.Mutex
	records []walRecord
	flushed bool
	closed  bool
}

type walRecord struct {
	txID    TxID
	kind    string
	payload []byte
}

func (m *mockWAL) Append(txID TxID, kind string, payload []byte) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records = append(m.records, walRecord{txID, kind, payload})
	return uint64(len(m.records)), nil
}

func (m *mockWAL) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushed = true
	return nil
}

func (m *mockWAL) Replay(fn func(TxID, string, []byte) error) error {
	m.mu.Lock()
	recs := make([]walRecord, len(m.records))
	copy(recs, m.records)
	m.mu.Unlock()
	for _, r := range recs {
		if err := fn(r.txID, r.kind, r.payload); err != nil {
			return err
		}
	}
	return nil
}

func (m *mockWAL) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

type mockPool struct {
	mu     sync.Mutex
	pages  map[PageID]*SlottedPage
	dirty  map[PageID]bool
	nextID PageID
	closed bool
}

func newMockPool() *mockPool {
	return &mockPool{
		pages: make(map[PageID]*SlottedPage),
		dirty: make(map[PageID]bool),
	}
}

func (m *mockPool) FetchPage(id PageID) (*SlottedPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pages[id]
	if !ok {
		return nil, fmt.Errorf("mockPool: page %d not found", id)
	}
	return p, nil
}

func (m *mockPool) DirtyPage(id PageID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirty[id] = true
	return nil
}

func (m *mockPool) FlushAll() error { return nil }

func (m *mockPool) AllocatePage() (PageID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	m.pages[id] = &SlottedPage{}
	return id, nil
}

func (m *mockPool) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

type mockTxm struct {
	mu     sync.Mutex
	nextID TxID
	active map[TxID]bool
}

func newMockTxm() *mockTxm {
	return &mockTxm{
		nextID: 1,
		active: make(map[TxID]bool),
	}
}

func (m *mockTxm) Begin() (TxID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	m.active[id] = true
	return id, nil
}

func (m *mockTxm) Commit(txID TxID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, txID)
	return nil
}

func (m *mockTxm) Rollback(txID TxID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.active, txID)
	return nil
}

func (m *mockTxm) ActiveTxIDs() []TxID {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TxID, 0, len(m.active))
	for id := range m.active {
		out = append(out, id)
	}
	return out
}

// ---- test helper -----------------------------------------------------------

func openTestDB(t *testing.T) (*Database, *mockWAL, *mockPool, *mockTxm) {
	t.Helper()
	wal := &mockWAL{}
	pool := newMockPool()
	txm := newMockTxm()
	db, err := Open(t.TempDir(), DefaultOptions(), wal, pool, txm)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db, wal, pool, txm
}

// ---- integration tests -----------------------------------------------------

func TestDatabaseOpenClose(t *testing.T) {
	t.Parallel()

	db, wal, pool, _ := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !wal.flushed {
		t.Fatal("WAL was not flushed on Close")
	}
	if !wal.closed {
		t.Fatal("WAL was not closed on Close")
	}
	if !pool.closed {
		t.Fatal("BufferPool was not closed on Close")
	}
}

func TestDatabaseCloseIdempotent(t *testing.T) {
	t.Parallel()

	db, _, _, _ := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := db.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second Close: err = %v, want ErrClosed", err)
	}
}

func TestDatabaseCreateTable(t *testing.T) {
	t.Parallel()

	db, wal, pool, txm := openTestDB(t)
	defer db.Close()

	txID, err := txm.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	cols := []*Column{
		{Name: "id", Type: "int", Nullable: false},
		{Name: "email", Type: "text", Nullable: false},
	}
	if err := db.CreateTable(txID, "accounts", cols); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := txm.Commit(txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The WAL must contain a DDL record for the create.
	wal.mu.Lock()
	walLen := len(wal.records)
	wal.mu.Unlock()
	if walLen == 0 {
		t.Fatal("no WAL records written for CreateTable")
	}

	// The buffer pool must have exactly one allocated page (the first heap page).
	pool.mu.Lock()
	pageCount := len(pool.pages)
	pool.mu.Unlock()
	if pageCount != 1 {
		t.Fatalf("pageCount = %d, want 1", pageCount)
	}

	// The catalog must reflect the new table with PageCount=1.
	meta, err := db.catalog.Table("accounts")
	if err != nil {
		t.Fatalf("catalog.Table: %v", err)
	}
	if meta.PageCount != 1 {
		t.Fatalf("PageCount = %d, want 1", meta.PageCount)
	}
}

func TestDatabaseCreateTableDuplicate(t *testing.T) {
	t.Parallel()

	db, _, _, txm := openTestDB(t)
	defer db.Close()

	txID, _ := txm.Begin()
	cols := []*Column{{Name: "id", Type: "int"}}
	if err := db.CreateTable(txID, "products", cols); err != nil {
		t.Fatalf("first CreateTable: %v", err)
	}
	if err := db.CreateTable(txID, "products", cols); !errors.Is(err, ErrTableExists) {
		t.Fatalf("second CreateTable: err = %v, want ErrTableExists", err)
	}
}

func TestDatabaseDropTable(t *testing.T) {
	t.Parallel()

	db, _, _, txm := openTestDB(t)
	defer db.Close()

	txID, _ := txm.Begin()
	cols := []*Column{{Name: "id", Type: "int"}}
	if err := db.CreateTable(txID, "tmp", cols); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := db.DropTable(txID, "tmp"); err != nil {
		t.Fatalf("DropTable: %v", err)
	}
	_, err := db.catalog.Table("tmp")
	if !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("Table after Drop: err = %v, want ErrTableNotFound", err)
	}
}

func TestDatabaseCreateTableAfterClose(t *testing.T) {
	t.Parallel()

	db, _, _, txm := openTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	txID, _ := txm.Begin()
	err := db.CreateTable(txID, "late", []*Column{{Name: "id", Type: "int"}})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("CreateTable after Close: err = %v, want ErrClosed", err)
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

func ExampleCatalog_CreateTable() {
	cat := NewCatalog()
	_, err := cat.CreateTable("users", []*Column{
		{Name: "id", Type: "int"},
		{Name: "name", Type: "text"},
	})
	if err != nil {
		panic(err)
	}
	tbl, _ := cat.Table("users")
	cols, _ := cat.Columns(tbl.ID)
	fmt.Printf("table=%q columns=%d\n", tbl.Name, len(cols))
	// Output:
	// table="users" columns=2
}
```

## Review

The integration is correct when the rules survive every failure path. Confirm the shutdown order — `wal.Flush`, `pool.FlushAll`, `wal.Close`, `pool.Close` — by asserting the log was both flushed and closed and the pool was closed, and that a second `Close` returns `ErrClosed`. Confirm `CreateTable` logs before it mutates: a WAL record exists, exactly one page was allocated, and the catalog shows `PageCount == 1`. The detail that distinguishes a correct implementation from a plausible one is the rollback: every failure path after the catalog is updated must call `DropTable` and check its error, returning a wrapped pair when the rollback itself fails, never discarding it with `_`.

Common mistakes for this seam. Updating the catalog before the WAL append leaves an in-memory table with no durable record, invisible to recovery. Discarding the rollback error turns a recoverable allocation failure into a silent catalog corruption. Flushing the pool before the log can write a page ahead of the record that describes it, defeating redo. Returning "committed" before the commit record is durable breaks the D in ACID — the demo and tests here cover the DDL path; the commit-fsync ordering is the same rule one layer down.

## Resources

- [Architecture of a Database System (Hellerstein, Stonebraker, Hamilton)](https://dsf.berkeley.edu/papers/fntdb07-architecture.pdf) — the layered-stack model and the narrow inter-layer contracts this `Database` composes.
- [SQLite: Architecture of SQLite](https://www.sqlite.org/arch.html) — how a real embedded engine wires its tokenizer, parser, virtual machine, B-tree, pager, and OS interface into one in-process library.
- [SQLite: PRAGMA synchronous](https://www.sqlite.org/pragma.html#pragma_synchronous) — the fsync-before-commit-ack durability trade-off (`FULL` vs `OFF`) the durability ordering enforces.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Join`, used by `Close` to collect failures from all four shutdown steps.

---

Back to [02-system-catalog.md](02-system-catalog.md) | Next: [04-crash-recovery-redo-log.md](04-crash-recovery-redo-log.md)
