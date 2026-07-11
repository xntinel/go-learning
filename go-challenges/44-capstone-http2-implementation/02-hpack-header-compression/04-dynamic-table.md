# Exercise 4: The Dynamic Table — Eviction And Size Updates

The dynamic table is the stateful heart of HPACK: a connection-scoped FIFO of header entries, bounded by an accounted size, that both peers must evolve in lockstep. This module implements it directly — the `name + value + 32` size accounting, eviction of the oldest entries when a newcomer would overflow, the rule that an oversized entry empties the table, and the size-update directive that flushes it.

This module is fully self-contained: its own `go mod init`, no external dependencies, its own demo and tests.

## What you'll build

```text
dyntable/
  go.mod
  table.go               DynamicTable: Add, At, SetMaxSize, Size, eviction
  table_test.go          entry size, indexing, eviction, size update, oversize
  cmd/demo/main.go        fill a 256-byte table and watch eviction kick in
```

- Files: `table.go`, `table_test.go`, `cmd/demo/main.go`.
- Implement: `DynamicTable` with `Add(name, value)`, `At(i) (Entry, error)`, `SetMaxSize(n)`, and the `Size`/`MaxSize`/`Len`/`Inserts`/`Evictions` accessors.
- Test: entry size is `name+value+32`; the newest entry is dynamic index 1; filling past the bound evicts oldest-first; a size update to 0 empties the table; an oversized entry clears the table and stores nothing.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p dyntable/cmd/demo && cd dyntable
go mod init example.com/dyntable
go mod edit -go=1.26
```

### Accounting, indexing, eviction, and the size update

An entry's accounted size is `len(name) + len(value) + 32` (RFC 7541 section 4.1); the constant keeps a table of many tiny headers from exhausting memory. `:authority: www.example.com` therefore costs `10 + 15 + 32 = 57` bytes, a number the tests pin directly because it is the first dynamic entry in RFC 7541's own examples. Entries are held newest-first, so HPACK dynamic index 1 is the most recent insertion and the largest index is the oldest — `At` translates that 1-based index and reports an error outside the range.

`Add` evicts before it inserts: while the newcomer would push the total over the maximum, it drops the oldest entry, until the newcomer fits or the table is empty. RFC 7541 section 4.4 adds the boundary case that catches naive implementations — an entry whose own size exceeds the maximum is never stored, and the attempt clears the table entirely. The code expresses this by evicting in the loop and then, if the entry still does not fit, returning without inserting, leaving an empty table.

`SetMaxSize` is the size-update directive (RFC 7541 section 4.2). Lowering the bound evicts oldest-first until the table fits; a bound of 0 empties it outright, which is exactly how a peer flushes the table without dropping the connection. Counters for total inserts and evictions make the eviction behaviour observable to a test and to production metrics.

Create `table.go`:

```go
package dyntable

import "errors"

// entryOverhead is the per-entry accounting cost in bytes that RFC 7541
// section 4.1 adds on top of the name and value octet lengths.
const entryOverhead = 32

// ErrNoEntry is returned by At for an index outside the table.
var ErrNoEntry = errors.New("dyntable: no entry at index")

// Entry is one name/value pair stored in the dynamic table.
type Entry struct {
	Name  string
	Value string
}

// Size is the entry's accounted size: name + value octets + 32 (RFC 7541 4.1).
func (e Entry) Size() uint32 {
	return uint32(len(e.Name) + len(e.Value) + entryOverhead)
}

// DynamicTable is the connection-scoped HPACK dynamic table: a FIFO of entries
// bounded by a maximum accounted size. The newest entry has HPACK dynamic index
// 1; inserting a new entry evicts the oldest entries until it fits.
type DynamicTable struct {
	entries   []Entry // entries[0] is the newest
	size      uint32
	maxSize   uint32
	inserts   uint64
	evictions uint64
}

// New returns a dynamic table with the given maximum accounted size.
func New(maxSize uint32) *DynamicTable {
	return &DynamicTable{maxSize: maxSize}
}

// Size returns the current accounted size in bytes.
func (t *DynamicTable) Size() uint32 { return t.size }

// MaxSize returns the current size bound.
func (t *DynamicTable) MaxSize() uint32 { return t.maxSize }

// Len returns the number of entries currently held.
func (t *DynamicTable) Len() int { return len(t.entries) }

// Inserts returns the total number of entries ever added.
func (t *DynamicTable) Inserts() uint64 { return t.inserts }

// Evictions returns the total number of entries ever evicted.
func (t *DynamicTable) Evictions() uint64 { return t.evictions }

// Add inserts name/value as the newest entry, evicting the oldest entries first
// until it fits. An entry larger than the whole table evicts everything and is
// not stored, exactly as RFC 7541 section 4.4 specifies.
func (t *DynamicTable) Add(name, value string) {
	e := Entry{name, value}
	es := e.Size()
	for t.size+es > t.maxSize && len(t.entries) > 0 {
		t.evictOldest()
	}
	if es > t.maxSize {
		return
	}
	t.entries = append([]Entry{e}, t.entries...)
	t.size += es
	t.inserts++
}

func (t *DynamicTable) evictOldest() {
	last := len(t.entries) - 1
	t.size -= t.entries[last].Size()
	t.entries = t.entries[:last]
	t.evictions++
}

// At returns the entry at HPACK dynamic index i, where 1 is the newest entry
// and Len() is the oldest.
func (t *DynamicTable) At(i int) (Entry, error) {
	if i < 1 || i > len(t.entries) {
		return Entry{}, ErrNoEntry
	}
	return t.entries[i-1], nil
}

// SetMaxSize applies a dynamic table size update (RFC 7541 section 4.2),
// evicting the oldest entries until the table fits the new bound. A bound of 0
// empties the table.
func (t *DynamicTable) SetMaxSize(n uint32) {
	t.maxSize = n
	for t.size > t.maxSize && len(t.entries) > 0 {
		t.evictOldest()
	}
}
```


### The runnable demo

The demo creates a 256-byte table and adds eight `x-id` entries, each costing `4 + 8 + 32 = 44` bytes, so five fit (220 bytes) and every further insert evicts exactly one. It prints the running length, size, and eviction count, then identifies the newest and oldest surviving entries, then issues a size update to 0 to flush everything.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/dyntable"
)

func main() {
	t := dyntable.New(256)
	fmt.Printf("max size %d bytes (each x-id entry costs 4+8+32 = 44 bytes)\n", t.MaxSize())

	for i := 0; i < 8; i++ {
		t.Add("x-id", fmt.Sprintf("value-%02d", i))
		fmt.Printf("  add value-%02d -> len=%d size=%d evictions=%d\n",
			i, t.Len(), t.Size(), t.Evictions())
	}

	newest, _ := t.At(1)
	oldest, _ := t.At(t.Len())
	fmt.Printf("newest (index 1) = %s, oldest (index %d) = %s\n",
		newest.Value, t.Len(), oldest.Value)

	t.SetMaxSize(0)
	fmt.Printf("after size-update to 0: len=%d size=%d\n", t.Len(), t.Size())
}
```


Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
max size 256 bytes (each x-id entry costs 4+8+32 = 44 bytes)
  add value-00 -> len=1 size=44 evictions=0
  add value-01 -> len=2 size=88 evictions=0
  add value-02 -> len=3 size=132 evictions=0
  add value-03 -> len=4 size=176 evictions=0
  add value-04 -> len=5 size=220 evictions=0
  add value-05 -> len=5 size=220 evictions=1
  add value-06 -> len=5 size=220 evictions=2
  add value-07 -> len=5 size=220 evictions=3
newest (index 1) = value-07, oldest (index 5) = value-03
after size-update to 0: len=0 size=0
```

The size holds steady at 220 once five entries are present: each new insert evicts the oldest, so length stays at 5 while the eviction counter climbs.

### Tests

`TestEntrySize` pins the `57`-byte `:authority` entry from the RFC examples. `TestAddAndIndex` checks the size accumulates (`57`, then `110`) and that newest-first indexing puts the latest entry at index 1. `TestEvictionUnderLimit` fills a 256-byte table with ten 44-byte entries and asserts exactly five survive, the right ones, with five evictions. `TestSizeUpdateEvicts` flushes via a zero size update, and `TestOversizedEntryClears` pins the section 4.4 rule that an entry larger than the table empties it and stores nothing.

Create `table_test.go`:

```go
package dyntable

import "testing"

func TestEntrySize(t *testing.T) {
	t.Parallel()
	// RFC 7541 4.1: size = name octets + value octets + 32.
	e := Entry{Name: ":authority", Value: "www.example.com"}
	if got := e.Size(); got != 57 {
		t.Errorf("Size() = %d, want 57", got)
	}
}

func TestAddAndIndex(t *testing.T) {
	t.Parallel()
	tab := New(4096)
	tab.Add(":authority", "www.example.com") // 57
	if tab.Size() != 57 {
		t.Fatalf("size = %d, want 57", tab.Size())
	}
	tab.Add("cache-control", "no-cache") // 53
	if tab.Size() != 110 {
		t.Fatalf("size = %d, want 110", tab.Size())
	}
	// Newest entry is dynamic index 1; oldest is the highest index.
	if e, _ := tab.At(1); e.Name != "cache-control" {
		t.Errorf("At(1).Name = %q, want cache-control", e.Name)
	}
	if e, _ := tab.At(2); e.Name != ":authority" {
		t.Errorf("At(2).Name = %q, want :authority", e.Name)
	}
	if _, err := tab.At(3); err == nil {
		t.Error("At(3) should fail")
	}
}

func TestEvictionUnderLimit(t *testing.T) {
	t.Parallel()
	tab := New(256) // each x-id/value-NN entry costs 4+8+32 = 44
	for i := 0; i < 10; i++ {
		tab.Add("x-id", padNum(i))
	}
	if tab.Len() != 5 {
		t.Errorf("Len() = %d, want 5", tab.Len())
	}
	if tab.Size() != 220 {
		t.Errorf("Size() = %d, want 220", tab.Size())
	}
	if e, _ := tab.At(1); e.Value != "value-09" {
		t.Errorf("newest = %q, want value-09", e.Value)
	}
	if e, _ := tab.At(5); e.Value != "value-05" {
		t.Errorf("oldest retained = %q, want value-05", e.Value)
	}
	if tab.Evictions() != 5 {
		t.Errorf("Evictions() = %d, want 5", tab.Evictions())
	}
}

func TestSizeUpdateEvicts(t *testing.T) {
	t.Parallel()
	tab := New(4096)
	tab.Add("a", "1")
	tab.Add("b", "2")
	tab.SetMaxSize(0) // size update to zero empties the table
	if tab.Len() != 0 || tab.Size() != 0 {
		t.Fatalf("after SetMaxSize(0): len=%d size=%d, want 0/0", tab.Len(), tab.Size())
	}
}

func TestOversizedEntryClears(t *testing.T) {
	t.Parallel()
	tab := New(40) // smaller than any 32+name+value entry below
	tab.Add("x", "y")
	tab.Add("a-much-longer-name", "with-a-long-value")
	// RFC 7541 4.4: an entry larger than the table empties it and is not stored.
	if tab.Len() != 0 || tab.Size() != 0 {
		t.Fatalf("oversized add: len=%d size=%d, want 0/0", tab.Len(), tab.Size())
	}
}

func padNum(i int) string {
	const digits = "0123456789"
	return "value-" + string([]byte{digits[i/10], digits[i%10]})
}
```


## Review

The table is correct when accounting, eviction order, and the oversize rule all hold. Confirm the size constant is 32 and is applied per entry, confirm eviction removes the oldest (highest-index) entries first, and confirm an entry larger than the maximum leaves the table empty rather than storing a partial or oversized row. The mistakes that bite are off-by-one indexing (HPACK indices are 1-based and newest-first), forgetting the 32-byte overhead so the table holds more than the spec allows, and storing an oversized entry instead of clearing the table per section 4.4. Because both peers run this same logic, any divergence here is a silent, connection-killing desynchronization.

## Resources

- [RFC 7541 section 4 - Dynamic Table Management](https://httpwg.org/specs/rfc7541.html#dynamic.table.management) — entry size accounting, the maximum, eviction, and the oversize rule.
- [RFC 7541 section 4.2 - Maximum Table Size](https://httpwg.org/specs/rfc7541.html#maximum.table.size) — the dynamic table size update directive and SETTINGS_HEADER_TABLE_SIZE.
- [RFC 7541 Appendix C.3 - Request Examples with Indexing](https://httpwg.org/specs/rfc7541.html#request.examples.huffman) — the worked dynamic-table evolution, including the 57-byte first entry.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../03-stream-multiplexing/00-concepts.md](../03-stream-multiplexing/00-concepts.md)
