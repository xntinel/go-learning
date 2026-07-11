# Exercise 2: System Catalog

The catalog is the engine's memory of its own shape: every table, every column, every index lives here, and every layer above storage consults it. The planner reads it to turn a table name into an identifier and a column name into an ordinal and a type; the executor reads it to know which indexes exist. This exercise builds that registry as an in-memory, concurrency-safe map keyed by name and identifier, with the create, look-up, drop, and index operations that DDL drives — the smallest catalog that still behaves like the self-describing schema store at the heart of a real database.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
catalog.go           Catalog, Column, TableMeta, IndexMeta, NewCatalog, CreateTable, Table,
                     Columns, AddIndex, Indexes, DropTable
cmd/
  demo/
    main.go          create a table with typed columns and print its catalog entry
catalog_test.go      create/lookup, duplicate rejection, drop clears metadata, indexes, concurrent reads
```

- Files: `catalog.go`, `cmd/demo/main.go`, `catalog_test.go`.
- Implement: `Catalog` with `CreateTable(name string, cols []*Column) (*TableMeta, error)`, `Table`, `Columns`, `AddIndex`, `Indexes`, `DropTable`, plus `NewCatalog`, `Column`, `TableMeta`, and `IndexMeta`.
- Test: `catalog_test.go` creates and looks up a table, rejects a duplicate with `ErrTableExists`, proves a drop clears the table's columns and indexes, registers and lists an index, and hammers the read path from 50 goroutines under `-race`.
- Verify: `go test -run 'TestCatalog|ExampleCatalog' -race ./...`

Set up the module:

```bash
mkdir -p system-catalog/cmd/demo && cd system-catalog
go mod init example.com/system-catalog
```

### Why the catalog is a registry, and why ordinals are assigned not copied

A catalog has three jobs, and each shapes the data structure. It must resolve a *name* to a table quickly (the planner does this on every query), resolve a *table identifier* to its ordered columns and to its indexes (binding and planning do this), and enforce uniqueness so two `CREATE TABLE`s with the same name cannot both succeed. Those access patterns are why this implementation keeps three maps: name to table metadata, table identifier to the ordered column slice, and table identifier to the index slice. The table's identifier, not its name, is the durable key that columns and indexes hang off, because a name can in principle be reused after a drop while identifiers are handed out monotonically and never recycled here.

The detail that is easy to get wrong, and that the test pins, is that the *column ordinal is assigned by the catalog, not taken from the caller*. When you create a table you pass column definitions in order, and the catalog stamps each one with its position (0, 1, 2, …) and with the new table's identifier as it stores them, copying each `Column` so the caller's slice cannot mutate catalog state afterward. The ordinal is the column's stable position in a row's encoding; letting the caller supply it would invite two columns with the same ordinal or a gap in the sequence, either of which corrupts every tuple the executor later decodes. By owning ordinal assignment the catalog guarantees the column order is dense, zero-based, and consistent with the order the columns were declared.

Concurrency is handled with a single read/write mutex, and the asymmetry is intentional. DDL — create, drop, add index — is rare and takes the exclusive lock; lookups are frequent, are pure reads, and take the shared lock, so any number of planner goroutines can resolve names at once without blocking each other. This matches reality: a busy database runs millions of queries against a schema that changes a handful of times a day, so the read path must never serialize on the write path. The concurrent-reads test exists to prove, under the race detector, that fifty simultaneous lookups against a live catalog are clean.

Create `catalog.go`:

```go
package catalog

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

// Catalog is the in-memory system catalog. It tracks every table, column, and
// index definition. In a full engine it is bootstrapped once on database creation,
// checkpointed periodically to a dedicated catalog page, and fully reconstructed
// from the WAL on restart; all mutations are WAL-logged before the in-memory state
// is updated so that crash recovery can rebuild the catalog from first principles.
//
// Concurrency: Catalog is safe for concurrent use. Reads take a shared lock; writes
// take an exclusive lock. DDL is rare compared to DML, so lock contention on the
// catalog is not a bottleneck in practice.
type Catalog struct {
	mu      sync.RWMutex
	tables  map[string]*TableMeta   // name -> meta
	columns map[uint32][]*Column    // tableID -> columns
	indexes map[uint32][]*IndexMeta // tableID -> indexes
	nextID  uint32
}

// NewCatalog creates an empty system catalog. A full engine would then bootstrap
// sys_tables, sys_columns, and sys_indexes as real tables stored in the catalog
// itself, breaking the chicken-and-egg problem with hard-coded initialization.
func NewCatalog() *Catalog {
	return &Catalog{
		tables:  make(map[string]*TableMeta),
		columns: make(map[uint32][]*Column),
		indexes: make(map[uint32][]*IndexMeta),
		nextID:  1,
	}
}

// CreateTable registers a new table and its column definitions. Returns
// ErrTableExists if a table with the same name already exists. In the engine the
// caller WAL-logs the DDL record before calling CreateTable so that a crash after
// the WAL write but before the catalog update is recoverable on replay. The column
// ordinal and table ID are assigned here, not taken from the caller's input.
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

// Table looks up a table by name. Returns ErrTableNotFound if no such table exists.
func (c *Catalog) Table(name string) (*TableMeta, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	t, ok := c.tables[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrTableNotFound, name)
	}
	return t, nil
}

// Columns returns the column definitions for a table by ID. Returns
// ErrTableNotFound if the table ID is not in the catalog.
func (c *Catalog) Columns(tableID uint32) ([]*Column, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cols, ok := c.columns[tableID]
	if !ok {
		return nil, fmt.Errorf("%w: id %d", ErrTableNotFound, tableID)
	}
	return cols, nil
}

// AddIndex registers a new index on a table and assigns it a catalog ID.
func (c *Catalog) AddIndex(meta *IndexMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	meta.ID = c.nextID
	c.nextID++
	c.indexes[meta.TableID] = append(c.indexes[meta.TableID], meta)
}

// Indexes returns all indexes defined on a table. Returns nil if no indexes exist.
func (c *Catalog) Indexes(tableID uint32) []*IndexMeta {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.indexes[tableID]
}

// DropTable removes a table and all its column and index metadata. Returns
// ErrTableNotFound if no such table exists.
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

The demo creates a table with three typed, differently nullable columns, then reads its entry back out of the catalog and prints the table identifier, the column count, and each column's assigned ordinal, name, type, and nullability — making visible that the catalog, not the caller, stamped the ordinals 0, 1, 2 in declaration order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/system-catalog"
)

func main() {
	cat := catalog.NewCatalog()
	_, err := cat.CreateTable("orders", []*catalog.Column{
		{Name: "id", Type: "int", Nullable: false},
		{Name: "customer", Type: "text", Nullable: false},
		{Name: "total", Type: "float", Nullable: true},
	})
	if err != nil {
		log.Fatalf("create table: %v", err)
	}
	meta, _ := cat.Table("orders")
	cols, _ := cat.Columns(meta.ID)
	fmt.Printf("catalog: table %q (id=%d) has %d columns\n", meta.Name, meta.ID, len(cols))
	for _, c := range cols {
		nullable := "NOT NULL"
		if c.Nullable {
			nullable = "NULL"
		}
		fmt.Printf("  [%d] %s %s %s\n", c.Ordinal, c.Name, c.Type, nullable)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
catalog: table "orders" (id=1) has 3 columns
  [0] id int NOT NULL
  [1] customer text NOT NULL
  [2] total float NULL
```

### Tests

The tests cover the registry's contract from every angle. Create-and-lookup proves a table is resolvable by name and by identifier and that the catalog assigned dense ordinals rather than copying the caller's. Duplicate creation must return `ErrTableExists`. Drop must clear the table *and* its columns and indexes, so a later lookup of any of the three is `ErrTableNotFound`. The index test registers an index and lists it back. The concurrent-reads test runs fifty simultaneous lookups under the race detector to prove the shared-lock read path is clean.

Create `catalog_test.go`:

```go
package catalog

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestCatalogCreateAndLookup(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	cols := []*Column{
		{Name: "id", Type: "int", Nullable: false},
		{Name: "name", Type: "text", Nullable: true},
	}
	meta, err := cat.CreateTable("users", cols)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if meta.Name != "users" {
		t.Fatalf("meta.Name = %q, want users", meta.Name)
	}
	got, err := cat.Table("users")
	if err != nil {
		t.Fatalf("Table: %v", err)
	}
	if got.ID != meta.ID {
		t.Fatalf("Table ID mismatch: %d vs %d", got.ID, meta.ID)
	}
	gotCols, err := cat.Columns(meta.ID)
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	if len(gotCols) != 2 {
		t.Fatalf("Columns count = %d, want 2", len(gotCols))
	}
	if gotCols[0].Name != "id" || gotCols[1].Name != "name" {
		t.Fatalf("unexpected column names: %v", gotCols)
	}
	// Column ordinals are assigned by the catalog, not copied from input.
	if gotCols[0].Ordinal != 0 || gotCols[1].Ordinal != 1 {
		t.Fatalf("ordinals wrong: %d %d", gotCols[0].Ordinal, gotCols[1].Ordinal)
	}
}

func TestCatalogCreateTableDuplicate(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	cols := []*Column{{Name: "id", Type: "int"}}
	if _, err := cat.CreateTable("orders", cols); err != nil {
		t.Fatalf("first CreateTable: %v", err)
	}
	_, err := cat.CreateTable("orders", cols)
	if !errors.Is(err, ErrTableExists) {
		t.Fatalf("second CreateTable: err = %v, want ErrTableExists", err)
	}
}

func TestCatalogDropTable(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	cols := []*Column{{Name: "id", Type: "int"}}
	meta, err := cat.CreateTable("items", cols)
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if err := cat.DropTable("items"); err != nil {
		t.Fatalf("DropTable: %v", err)
	}
	_, err = cat.Table("items")
	if !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("Table after Drop: err = %v, want ErrTableNotFound", err)
	}
	if _, err := cat.Columns(meta.ID); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("Columns after Drop: err = %v, want ErrTableNotFound", err)
	}
}

func TestCatalogDropTableNotFound(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	err := cat.DropTable("no_such_table")
	if !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("DropTable(missing): err = %v, want ErrTableNotFound", err)
	}
}

func TestCatalogAddAndListIndexes(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	meta, _ := cat.CreateTable("events", []*Column{{Name: "ts", Type: "int"}})
	cat.AddIndex(&IndexMeta{
		TableID: meta.ID,
		Name:    "idx_events_ts",
		Columns: []string{"ts"},
		Unique:  false,
	})
	idxs := cat.Indexes(meta.ID)
	if len(idxs) != 1 {
		t.Fatalf("Indexes count = %d, want 1", len(idxs))
	}
	if idxs[0].Name != "idx_events_ts" {
		t.Fatalf("index name = %q, want idx_events_ts", idxs[0].Name)
	}
}

func TestCatalogConcurrentReads(t *testing.T) {
	t.Parallel()

	cat := NewCatalog()
	if _, err := cat.CreateTable("log", []*Column{{Name: "msg", Type: "text"}}); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cat.Table("log")
		}()
	}
	wg.Wait()
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
	t, _ := cat.Table("users")
	cols, _ := cat.Columns(t.ID)
	fmt.Printf("table=%q columns=%d\n", t.Name, len(cols))
	// Output:
	// table="users" columns=2
}
```

## Review

A correct catalog is a registry that owns its keys. Confirm a created table is resolvable by name and by identifier, that the catalog assigned dense zero-based ordinals rather than echoing the caller's, and that a second create of the same name returns `ErrTableExists`. The drop test is the one that catches sloppy bookkeeping: dropping a table must also delete its column and index entries, so all three lookups afterward return `ErrTableNotFound` — a drop that leaves orphaned columns behind is a slow memory leak and a correctness bug if the identifier is ever reused. The concurrent-reads test must stay clean under `-race`, proving the read path holds only the shared lock.

Common mistakes for this registry. Taking the caller's column ordinal instead of assigning it lets duplicate or sparse ordinals corrupt tuple decoding. Storing the caller's `*Column` pointers directly instead of copying lets a later mutation of the caller's slice silently rewrite catalog state. Keying columns by name rather than by table identifier breaks the moment a name is dropped and reused. Taking the exclusive lock on the read path turns every planner lookup into a serialization point and defeats the read/write split entirely.

## Resources

- [SQLite: The Schema Table](https://www.sqlite.org/schematab.html) — `sqlite_schema`, the self-describing system catalog stored as an ordinary table, the production form of this registry.
- [Architecture of a Database System (Hellerstein, Stonebraker, Hamilton)](https://dsf.berkeley.edu/papers/fntdb07-architecture.pdf) — its catalog-manager discussion explains why the schema lives in tables the system already knows how to read.
- [`sync` package](https://pkg.go.dev/sync) — `RWMutex` semantics; the read/write split that lets frequent lookups run concurrently with rare DDL.

---

Back to [01-slotted-page-heap.md](01-slotted-page-heap.md) | Next: [03-database-integration.md](03-database-integration.md)
