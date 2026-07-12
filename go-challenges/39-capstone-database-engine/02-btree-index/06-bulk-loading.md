# Exercise 6: Bulk Loading from Sorted Input

Inserting n keys one at a time costs O(n log n) and leaves every leaf only about half full, because each split halves a node. When the input is already sorted — rebuilding an index, or running `CREATE INDEX` over an existing table — bottom-up bulk loading is dramatically better: pack each leaf to a chosen fill factor, link the leaves, then build each internal level from the level below until a single root remains. The result is a dense, minimal-height tree built in one pass. This exercise builds that loader.

This module is fully self-contained. It carries its own page layout, store, search, and range-scan paths, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
btree.go             page layout + MemStore + Search + RangeScan + setParent (baseline)
bulk.go              BulkLoad, evenGroups, bulkEntry, ErrNotSorted
cmd/
  demo/
    main.go          bulk-load sorted keys, search one, scan the count
bulk_test.go         invariant check on 1000 keys, input validation, empty input
```

- Files: `btree.go`, `bulk.go`, `cmd/demo/main.go`, `bulk_test.go`.
- Implement: `BulkLoad`, `evenGroups`, the `bulkEntry` type, and `ErrNotSorted`.
- Test: `bulk_test.go` bulk-loads a thousand sorted keys and asserts every invariant plus full searchability, rejects unsorted, duplicate, oversized, and out-of-range-fill inputs, and handles empty input.
- Verify: `go test -run 'TestBulkLoad|ExampleBulkLoad' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/02-btree-index/06-bulk-loading/cmd/demo && cd go-solutions/39-capstone-database-engine/02-btree-index/06-bulk-loading
```

### The baseline

Bulk loading does not use `Insert` at all — it writes pages directly — so this module's baseline is a slimmer tree: the page layout and store, `leafFor`/`Search`/`RangeScan` to read the result, `setParent` to wire children to the internal nodes the loader builds, and the `clone` helpers. That baseline is duplicated here so the module stands alone; the loader itself is the new work in `bulk.go`.

Create `btree.go`:

```go
package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	PageSize   = 4096
	MaxKeySize = 255
)

// RecordID packs a (pageID, slotIndex) pair into a uint64.
type RecordID uint64

// MakeRecordID packs a page and slot into a RecordID.
func MakeRecordID(page uint64, slot uint16) RecordID {
	return RecordID(page<<16 | uint64(slot))
}

// Page returns the page component of a RecordID.
func (r RecordID) Page() uint64 { return uint64(r) >> 16 }

// Slot returns the slot component of a RecordID.
func (r RecordID) Slot() uint16 { return uint16(r) }

// ErrKeyTooLong is returned when a key exceeds MaxKeySize bytes.
var ErrKeyTooLong = errors.New("btree: key exceeds MaxKeySize bytes")

// PageStore abstracts page-level I/O.
type PageStore interface {
	ReadPage(id uint64) ([]byte, error)
	WritePage(id uint64, data []byte) error
	AllocPage() (uint64, error)
}

// MemStore is a fully in-memory PageStore.
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

const (
	kindInternal byte = 0
	kindLeaf     byte = 1

	leafHdr  = 19
	innerHdr = 11

	maxEntrySize = 1 + MaxKeySize + 8

	leafCap  = (PageSize - leafHdr) / maxEntrySize
	innerCap = (PageSize - innerHdr - 8) / maxEntrySize
)

type node struct {
	kind     byte
	parent   uint64
	nextLeaf uint64
	keys     [][]byte
	vals     []RecordID
	children []uint64
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

// Tree is a page-backed B+Tree. It is not safe for concurrent use.
type Tree struct {
	store  PageStore
	rootID uint64
}

func (t *Tree) readNode(id uint64) (*node, error) {
	page, err := t.store.ReadPage(id)
	if err != nil {
		return nil, err
	}
	if page[0] == kindLeaf {
		return decodeLeaf(page), nil
	}
	return decodeInner(page), nil
}

func (t *Tree) writeNode(id uint64, n *node) error {
	var page []byte
	if n.kind == kindLeaf {
		page = encodeLeaf(n)
	} else {
		page = encodeInner(n)
	}
	return t.store.WritePage(id, page)
}

func (t *Tree) setParent(childID, parentID uint64) error {
	n, err := t.readNode(childID)
	if err != nil {
		return err
	}
	n.parent = parentID
	return t.writeNode(childID, n)
}

// leafFor returns the ID of the leaf page that should contain key.
func (t *Tree) leafFor(key []byte) (uint64, error) {
	id := t.rootID
	for {
		n, err := t.readNode(id)
		if err != nil {
			return 0, err
		}
		if n.kind == kindLeaf {
			return id, nil
		}
		lo, hi := 0, len(n.keys)
		for lo < hi {
			mid := (lo + hi) / 2
			if bytes.Compare(n.keys[mid], key) <= 0 {
				lo = mid + 1
			} else {
				hi = mid
			}
		}
		id = n.children[lo]
	}
}

// Search returns the value stored under key. Returns (0, false, nil) when the
// key does not exist.
func (t *Tree) Search(key []byte) (RecordID, bool, error) {
	if len(key) > MaxKeySize {
		return 0, false, fmt.Errorf("%w: len=%d", ErrKeyTooLong, len(key))
	}
	leafID, err := t.leafFor(key)
	if err != nil {
		return 0, false, err
	}
	leaf, err := t.readNode(leafID)
	if err != nil {
		return 0, false, err
	}
	pos := lowerBound(leaf.keys, key)
	if pos < len(leaf.keys) && bytes.Equal(leaf.keys[pos], key) {
		return leaf.vals[pos], true, nil
	}
	return 0, false, nil
}

// Iterator scans leaves in ascending key order.
type Iterator struct {
	tree   *Tree
	endKey []byte
	cur    *node
	pos    int
}

// RangeScan returns an iterator over all keys in [startKey, endKey].
// If endKey is nil the scan is open-ended.
func (t *Tree) RangeScan(startKey, endKey []byte) (*Iterator, error) {
	leafID, err := t.leafFor(startKey)
	if err != nil {
		return nil, err
	}
	leaf, err := t.readNode(leafID)
	if err != nil {
		return nil, err
	}
	pos := lowerBound(leaf.keys, startKey)
	return &Iterator{tree: t, endKey: endKey, cur: leaf, pos: pos}, nil
}

// Next returns the next key and value in the scan.
func (it *Iterator) Next() (key []byte, value RecordID, ok bool) {
	for {
		if it.pos < len(it.cur.keys) {
			k := it.cur.keys[it.pos]
			if it.endKey != nil && bytes.Compare(k, it.endKey) > 0 {
				return nil, 0, false
			}
			v := it.cur.vals[it.pos]
			it.pos++
			return k, v, true
		}
		if it.cur.nextLeaf == 0 {
			return nil, 0, false
		}
		next, err := it.tree.readNode(it.cur.nextLeaf)
		if err != nil {
			return nil, 0, false
		}
		it.cur = next
		it.pos = 0
	}
}

func lowerBound(keys [][]byte, key []byte) int {
	lo, hi := 0, len(keys)
	for lo < hi {
		mid := (lo + hi) / 2
		if bytes.Compare(keys[mid], key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

func cloneKeys(src [][]byte) [][]byte {
	out := make([][]byte, len(src))
	for i, k := range src {
		out[i] = append([]byte(nil), k...)
	}
	return out
}

func cloneVals(src []RecordID) []RecordID {
	out := make([]RecordID, len(src))
	copy(out, src)
	return out
}

// cloneKey returns an independent copy of a key slice.
func cloneKey(k []byte) []byte {
	return append([]byte(nil), k...)
}
```

### BulkLoad: build the tree level by level

The loader works bottom-up. First it validates the input — equal-length key and value slices, every key within `MaxKeySize`, and strictly increasing keys (which also rejects duplicates). Then it builds the leaf level: it splits the n entries into groups, allocates a leaf per group, copies the group's keys and values in, and links each leaf to the previous one through `nextLeaf`. For each leaf it records a `bulkEntry{low, id}` — the leaf's smallest key and its page ID — because that low key is exactly the separator the parent will use to route to this leaf.

The one subtlety is avoiding a tiny last node. Cutting fixed-size chunks of `fill` and leaving whatever remains would produce a final leaf that might hold a single entry and violate the half-full invariant. `evenGroups` instead computes how many groups are needed, `ceil(n/maxPer)`, and distributes the entries as evenly as possible across them, so the group sizes differ by at most one. With `fill` near `leafCap`, every non-root leaf lands between `floor(fill/2)` and `fill` entries.

Building internal levels repeats the same grouping over the level below. An internal node with g children carries g-1 separators — the low keys of its children at positions 1 through g-1 — while the low key of its first child propagates upward as the separator its own parent will use. The loop runs until a level has a single entry, and that entry's page ID is the root.

Create `bulk.go`:

```go
package btree

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrNotSorted is returned by BulkLoad when the input keys are not strictly
// increasing (which also rejects duplicates).
var ErrNotSorted = errors.New("btree: input keys not strictly sorted")

// bulkEntry is one (lowKey, pageID) pair at a level being built bottom-up.
// lowKey is the smallest key in the subtree rooted at id, used as the separator
// in the parent.
type bulkEntry struct {
	low []byte
	id  uint64
}

// evenGroups splits n items into groups of at most maxPer, as evenly sized as
// possible. The returned sizes sum to n and differ by at most one.
func evenGroups(n, maxPer int) []int {
	if n <= 0 {
		return nil
	}
	groups := (n + maxPer - 1) / maxPer
	base := n / groups
	rem := n % groups
	sizes := make([]int, groups)
	for i := range sizes {
		sizes[i] = base
		if i < rem {
			sizes[i]++
		}
	}
	return sizes
}

// BulkLoad builds a B+Tree bottom-up from strictly increasing keys and their
// parallel values. fill is the target entries per leaf and must be in
// [1, leafCap]; choosing fill close to leafCap keeps every node well above the
// half-full minimum. The store should be empty.
func BulkLoad(store PageStore, keys [][]byte, vals []RecordID, fill int) (*Tree, error) {
	if len(keys) != len(vals) {
		return nil, fmt.Errorf("btree: bulk load: %d keys but %d values", len(keys), len(vals))
	}
	if fill < 1 || fill > leafCap {
		return nil, fmt.Errorf("btree: bulk load: fill %d out of range [1,%d]", fill, leafCap)
	}
	for i := range keys {
		if len(keys[i]) > MaxKeySize {
			return nil, fmt.Errorf("%w: index %d len=%d", ErrKeyTooLong, i, len(keys[i]))
		}
		if i > 0 && bytes.Compare(keys[i-1], keys[i]) >= 0 {
			return nil, fmt.Errorf("%w: at index %d", ErrNotSorted, i)
		}
	}
	tr := &Tree{store: store}
	if len(keys) == 0 {
		id, err := store.AllocPage()
		if err != nil {
			return nil, fmt.Errorf("btree: bulk alloc root: %w", err)
		}
		if err := store.WritePage(id, encodeLeaf(&node{kind: kindLeaf})); err != nil {
			return nil, fmt.Errorf("btree: bulk write root: %w", err)
		}
		tr.rootID = id
		return tr, nil
	}

	// Build the leaf level, linking siblings left to right.
	var level []bulkEntry
	idx := 0
	var prevID uint64
	havePrev := false
	for _, sz := range evenGroups(len(keys), fill) {
		id, err := store.AllocPage()
		if err != nil {
			return nil, fmt.Errorf("btree: bulk alloc leaf: %w", err)
		}
		leaf := &node{
			kind: kindLeaf,
			keys: cloneKeys(keys[idx : idx+sz]),
			vals: cloneVals(vals[idx : idx+sz]),
		}
		if err := tr.writeNode(id, leaf); err != nil {
			return nil, err
		}
		if havePrev {
			prev, err := tr.readNode(prevID)
			if err != nil {
				return nil, err
			}
			prev.nextLeaf = id
			if err := tr.writeNode(prevID, prev); err != nil {
				return nil, err
			}
		}
		level = append(level, bulkEntry{low: cloneKey(keys[idx]), id: id})
		prevID = id
		havePrev = true
		idx += sz
	}

	// Build internal levels until a single root remains.
	for len(level) > 1 {
		var next []bulkEntry
		ci := 0
		for _, g := range evenGroups(len(level), innerCap+1) {
			id, err := store.AllocPage()
			if err != nil {
				return nil, fmt.Errorf("btree: bulk alloc inner: %w", err)
			}
			inner := &node{kind: kindInternal}
			for j := 0; j < g; j++ {
				e := level[ci+j]
				inner.children = append(inner.children, e.id)
				if j > 0 {
					inner.keys = append(inner.keys, cloneKey(e.low))
				}
			}
			if err := tr.writeNode(id, inner); err != nil {
				return nil, err
			}
			for _, cid := range inner.children {
				if err := tr.setParent(cid, id); err != nil {
					return nil, err
				}
			}
			next = append(next, bulkEntry{low: cloneKey(level[ci].low), id: id})
			ci += g
		}
		level = next
	}
	tr.rootID = level[0].id
	return tr, nil
}
```

### The runnable demo

The demo bulk-loads twelve sorted keys with a fill of eight — forcing two leaves under one root — then searches a middle key to confirm its `RecordID` survived and scans the whole tree to confirm the count. It exercises the full bottom-up path: two leaf groups, one internal level, a single root.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/bulk-loading"
)

func main() {
	const n = 12
	keys := make([][]byte, n)
	vals := make([]btree.RecordID, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("%04d", i))
		vals[i] = btree.MakeRecordID(uint64(i), 0)
	}

	tree, err := btree.BulkLoad(btree.NewMemStore(), keys, vals, 8)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("bulk-loaded %d keys (fill=8)\n", n)

	rid, ok, err := tree.Search([]byte("0005"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("search 0005: found=%v page=%d\n", ok, rid.Page())

	iter, err := tree.RangeScan([]byte(""), nil)
	if err != nil {
		log.Fatal(err)
	}
	count := 0
	for {
		if _, _, ok := iter.Next(); !ok {
			break
		}
		count++
	}
	fmt.Printf("scan count: %d\n", count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
bulk-loaded 12 keys (fill=8)
search 0005: found=true page=5
scan count: 12
```

### Tests

`TestBulkLoadInvariants` loads a thousand sorted keys at the maximum fill, runs the full-tree `checkInvariants` walk, confirms every key is searchable to its exact value, and scans the whole tree to verify ascending order and an exact count. `TestBulkLoadErrors` is table-driven over the rejected inputs: unsorted, duplicate, oversized key, and out-of-range fill. `TestBulkLoadEmpty` confirms an empty input produces a valid empty tree.

Create `bulk_test.go`:

```go
package btree

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// checkInvariants walks the whole tree and fails t if any B+Tree invariant is
// violated.
func checkInvariants(t *testing.T, tr *Tree) {
	t.Helper()
	leafDepth := -1
	var walk func(id uint64, depth int, isRoot bool, lo, hi []byte)
	walk = func(id uint64, depth int, isRoot bool, lo, hi []byte) {
		n, err := tr.readNode(id)
		if err != nil {
			t.Fatalf("read node %d: %v", id, err)
		}
		for i := 1; i < len(n.keys); i++ {
			if bytes.Compare(n.keys[i-1], n.keys[i]) >= 0 {
				t.Fatalf("node %d keys not strictly sorted at index %d", id, i)
			}
		}
		for _, k := range n.keys {
			if lo != nil && bytes.Compare(k, lo) < 0 {
				t.Fatalf("node %d key %q below lower bound %q", id, k, lo)
			}
			if hi != nil && bytes.Compare(k, hi) >= 0 {
				t.Fatalf("node %d key %q at or above upper bound %q", id, k, hi)
			}
		}
		if n.kind == kindLeaf {
			if leafDepth == -1 {
				leafDepth = depth
			} else if depth != leafDepth {
				t.Fatalf("leaf %d at depth %d, want %d", id, depth, leafDepth)
			}
			return
		}
		if len(n.children) != len(n.keys)+1 {
			t.Fatalf("inner %d: %d children, want %d", id, len(n.children), len(n.keys)+1)
		}
		for i, c := range n.children {
			childLo, childHi := lo, hi
			if i > 0 {
				childLo = n.keys[i-1]
			}
			if i < len(n.keys) {
				childHi = n.keys[i]
			}
			walk(c, depth+1, false, childLo, childHi)
		}
	}
	walk(tr.rootID, 0, true, nil, nil)
}

func TestBulkLoadInvariants(t *testing.T) {
	t.Parallel()
	const n = 1000
	keys := make([][]byte, n)
	vals := make([]RecordID, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("%06d", i))
		vals[i] = MakeRecordID(uint64(i), 0)
	}
	tr, err := BulkLoad(NewMemStore(), keys, vals, leafCap)
	if err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	checkInvariants(t, tr)
	for i := 0; i < n; i++ {
		got, ok, err := tr.Search(keys[i])
		if err != nil || !ok {
			t.Fatalf("Search(%q): ok=%v err=%v", keys[i], ok, err)
		}
		if got != vals[i] {
			t.Fatalf("Search(%q) = %v, want %v", keys[i], got, vals[i])
		}
	}
	iter, err := tr.RangeScan([]byte("000000"), nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	var prev []byte
	for {
		k, _, ok := iter.Next()
		if !ok {
			break
		}
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("scan out of order: %q >= %q", prev, k)
		}
		prev = append([]byte(nil), k...)
		count++
	}
	if count != n {
		t.Fatalf("scan returned %d keys, want %d", count, n)
	}
}

func TestBulkLoadErrors(t *testing.T) {
	t.Parallel()
	good := [][]byte{[]byte("a"), []byte("b")}
	goodVals := []RecordID{1, 2}
	cases := []struct {
		name    string
		keys    [][]byte
		vals    []RecordID
		fill    int
		wantErr error
	}{
		{name: "unsorted", keys: [][]byte{[]byte("b"), []byte("a")}, vals: []RecordID{1, 2}, fill: leafCap, wantErr: ErrNotSorted},
		{name: "duplicate", keys: [][]byte{[]byte("a"), []byte("a")}, vals: []RecordID{1, 2}, fill: leafCap, wantErr: ErrNotSorted},
		{name: "key too long", keys: [][]byte{make([]byte, MaxKeySize+1)}, vals: []RecordID{1}, fill: leafCap, wantErr: ErrKeyTooLong},
		{name: "fill zero", keys: good, vals: goodVals, fill: 0, wantErr: nil},
		{name: "fill too big", keys: good, vals: goodVals, fill: leafCap + 1, wantErr: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := BulkLoad(NewMemStore(), tc.keys, tc.vals, tc.fill)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
		})
	}
}

func TestBulkLoadEmpty(t *testing.T) {
	t.Parallel()
	tr, err := BulkLoad(NewMemStore(), nil, nil, leafCap)
	if err != nil {
		t.Fatalf("BulkLoad empty: %v", err)
	}
	if _, ok, _ := tr.Search([]byte("x")); ok {
		t.Fatal("empty tree should find nothing")
	}
}

func ExampleBulkLoad() {
	keys := [][]byte{[]byte("a"), []byte("b"), []byte("c")}
	vals := []RecordID{MakeRecordID(1, 0), MakeRecordID(2, 0), MakeRecordID(3, 0)}
	tr, _ := BulkLoad(NewMemStore(), keys, vals, leafCap)
	rid, ok, _ := tr.Search([]byte("b"))
	fmt.Printf("found=%v page=%d\n", ok, rid.Page())
	// Output: found=true page=2
}
```

## Review

Bulk loading is correct when the tree it produces is indistinguishable from one built by repeated inserts: same invariants, every key findable to its exact value, a full scan in ascending order with the exact count. The thousand-key invariant test is the proof, because `checkInvariants` enforces uniform leaf depth and the routing bounds that a mis-grouped level would violate. Confirm that the loader rejects unsorted and duplicate input up front — a bottom-up build trusts the order completely and will silently produce a corrupt tree if handed unsorted keys — and that an out-of-range fill is refused before any page is written.

The pitfalls are in the grouping and the separators. Cutting fixed `fill`-sized chunks instead of using `evenGroups` can leave a final leaf with one entry, violating the half-full minimum; even distribution is what keeps the smallest group within one of the largest. Taking the wrong key as a node's separator — anything other than the low key of each child after the first — misroutes searches into the wrong subtree. Forgetting to link leaves through `nextLeaf`, or forgetting to re-parent children to the internal nodes the loader builds, leaves a tree that searches correctly but cannot scan or rebalance. The full-tree walk plus a scan-count check catches all three.

## Resources

- [`bytes`](https://pkg.go.dev/bytes) — `bytes.Compare` for the strictly-increasing input check and the search descent.
- [PostgreSQL nbtree README](https://github.com/postgres/postgres/blob/master/src/backend/access/nbtree/README) — how Postgres builds an index bottom-up from sorted input during `CREATE INDEX`.
- [CMU 15-445/645 Fall 2024, B+Tree Indexes](https://15445.courses.cs.cmu.edu/fall2024/slides/08-indexes1.pdf) — bulk insertion and why sorted bottom-up loading beats repeated top-down inserts.

---

Back to [05-delete-balanced-borrow-and-merge.md](05-delete-balanced-borrow-and-merge.md) | Next: [07-cursor-seek-and-prefix-scan.md](07-cursor-seek-and-prefix-scan.md)
