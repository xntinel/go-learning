# Exercise 3: Search and Range Scan

An index that can only be built is useless; the payoff of all the splitting machinery is fast retrieval. This exercise adds the two read paths a B+Tree exists to provide: a point lookup (`Search`) that descends to one leaf and binary-searches it, and a range scan (`RangeScan` plus an `Iterator`) that finds the starting leaf once and then walks the leaf sibling chain in sorted order without ever re-touching an internal node. The linked-leaf range scan is the reason engines prefer a B+Tree over a plain B-Tree.

This module is fully self-contained. It carries its own copy of the page layout, store, and insert path, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
btree.go             page layout + MemStore + Insert/splits (baseline),
                     Search, Iterator, RangeScan, Next
cmd/
  demo/
    main.go          insert words, search one, range-scan a sub-interval
btree_test.go        point lookup, missing key, bounded + unbounded scans, 1000-key sort
```

- Files: `btree.go`, `cmd/demo/main.go`, `btree_test.go`.
- Implement: `Search`, the `Iterator` type, `RangeScan`, and `(*Iterator).Next`.
- Test: `btree_test.go` looks up present and absent keys, scans a bounded interval and an open-ended one, and inserts a thousand keys in random order to confirm a full scan returns them sorted with no gaps.
- Verify: `go test -run 'TestInsertAndSearch|TestSearch|TestRange|TestInsertSorted|ExampleTree' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/02-btree-index/03-search-and-range-scan/cmd/demo && cd go-solutions/39-capstone-database-engine/02-btree-index/03-search-and-range-scan
```

### The baseline: a tree you can already fill

Search and range scan need a populated tree, so this module carries the full page layout, store, descent, and insert path from the previous exercise. That baseline is duplicated here so the module stands alone; the new work is everything after `leafFor` plus the two read methods. If the insert and split logic is unfamiliar, the previous exercise is where it is explained in depth; here it is the foundation the read paths build on.

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

// Sentinel errors returned by Tree methods.
var (
	ErrKeyTooLong = errors.New("btree: key exceeds MaxKeySize bytes")
	ErrDuplicate  = errors.New("btree: duplicate key")
)

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

// Open creates or re-opens a B+Tree backed by store. Pass rootPageID = 0 to
// create a new tree.
func Open(store PageStore, rootPageID uint64) (*Tree, error) {
	if rootPageID != 0 {
		return &Tree{store: store, rootID: rootPageID}, nil
	}
	id, err := store.AllocPage()
	if err != nil {
		return nil, fmt.Errorf("btree: init root: %w", err)
	}
	root := &node{kind: kindLeaf}
	if err := store.WritePage(id, encodeLeaf(root)); err != nil {
		return nil, fmt.Errorf("btree: write root: %w", err)
	}
	return &Tree{store: store, rootID: id}, nil
}

// RootID returns the current root page ID.
func (t *Tree) RootID() uint64 { return t.rootID }

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

// Insert adds key -> value. Returns ErrDuplicate if the key already exists.
func (t *Tree) Insert(key []byte, value RecordID) error {
	if len(key) > MaxKeySize {
		return fmt.Errorf("%w: len=%d", ErrKeyTooLong, len(key))
	}
	leafID, err := t.leafFor(key)
	if err != nil {
		return err
	}
	leaf, err := t.readNode(leafID)
	if err != nil {
		return err
	}
	pos := lowerBound(leaf.keys, key)
	if pos < len(leaf.keys) && bytes.Equal(leaf.keys[pos], key) {
		return fmt.Errorf("%w: key=%q", ErrDuplicate, key)
	}
	leaf.keys = append(leaf.keys, nil)
	leaf.vals = append(leaf.vals, 0)
	copy(leaf.keys[pos+1:], leaf.keys[pos:])
	copy(leaf.vals[pos+1:], leaf.vals[pos:])
	leaf.keys[pos] = key
	leaf.vals[pos] = value

	if len(leaf.keys) <= leafCap {
		return t.writeNode(leafID, leaf)
	}
	return t.splitLeaf(leafID, leaf)
}

func (t *Tree) splitLeaf(leafID uint64, leaf *node) error {
	mid := len(leaf.keys) / 2
	right := &node{
		kind:     kindLeaf,
		parent:   leaf.parent,
		nextLeaf: leaf.nextLeaf,
		keys:     cloneKeys(leaf.keys[mid:]),
		vals:     cloneVals(leaf.vals[mid:]),
	}
	rightID, err := t.store.AllocPage()
	if err != nil {
		return fmt.Errorf("btree: alloc right leaf: %w", err)
	}
	leaf.keys = leaf.keys[:mid]
	leaf.vals = leaf.vals[:mid]
	leaf.nextLeaf = rightID

	if err := t.writeNode(leafID, leaf); err != nil {
		return err
	}
	if err := t.writeNode(rightID, right); err != nil {
		return err
	}
	return t.insertIntoParent(leafID, right.keys[0], rightID, leaf.parent)
}

func (t *Tree) insertIntoParent(leftID uint64, key []byte, rightID uint64, parentID uint64) error {
	if parentID == 0 {
		newRoot := &node{
			kind:     kindInternal,
			keys:     [][]byte{key},
			children: []uint64{leftID, rightID},
		}
		rootID, err := t.store.AllocPage()
		if err != nil {
			return fmt.Errorf("btree: alloc new root: %w", err)
		}
		if err := t.writeNode(rootID, newRoot); err != nil {
			return err
		}
		if err := t.setParent(leftID, rootID); err != nil {
			return err
		}
		if err := t.setParent(rightID, rootID); err != nil {
			return err
		}
		t.rootID = rootID
		return nil
	}
	parent, err := t.readNode(parentID)
	if err != nil {
		return err
	}
	pos := lowerBound(parent.keys, key)
	parent.keys = append(parent.keys, nil)
	parent.children = append(parent.children, 0)
	copy(parent.keys[pos+1:], parent.keys[pos:])
	copy(parent.children[pos+2:], parent.children[pos+1:])
	parent.keys[pos] = key
	parent.children[pos+1] = rightID

	if err := t.setParent(rightID, parentID); err != nil {
		return err
	}
	if len(parent.keys) <= innerCap {
		return t.writeNode(parentID, parent)
	}
	return t.splitInner(parentID, parent)
}

func (t *Tree) splitInner(nodeID uint64, n *node) error {
	mid := len(n.keys) / 2
	pushKey := n.keys[mid]
	right := &node{
		kind:     kindInternal,
		parent:   n.parent,
		keys:     cloneKeys(n.keys[mid+1:]),
		children: append([]uint64(nil), n.children[mid+1:]...),
	}
	n.keys = n.keys[:mid]
	n.children = n.children[:mid+1]

	rightID, err := t.store.AllocPage()
	if err != nil {
		return fmt.Errorf("btree: alloc right inner: %w", err)
	}
	for _, cid := range right.children {
		if err := t.setParent(cid, rightID); err != nil {
			return err
		}
	}
	if err := t.writeNode(nodeID, n); err != nil {
		return err
	}
	if err := t.writeNode(rightID, right); err != nil {
		return err
	}
	return t.insertIntoParent(nodeID, pushKey, rightID, n.parent)
}

func (t *Tree) setParent(childID, parentID uint64) error {
	n, err := t.readNode(childID)
	if err != nil {
		return err
	}
	n.parent = parentID
	return t.writeNode(childID, n)
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
```

### Search: descend once, binary-search the leaf

`Search` is the simplest possible use of the structure: `leafFor` descends to the one leaf that can hold the key, `lowerBound` binary-searches the leaf's sorted keys for the first index `>= key`, and a single `bytes.Equal` confirms an exact match. It returns three values — the value, an `ok` flag, and an error — and the two negative signals are deliberately distinct. A missing key is `(0, false, nil)`, an ordinary not-found that callers branch on; an I/O failure is a non-nil error. Collapsing the two would force callers to treat a corrupt page the same as an absent key.

Range scan is where the leaf links earn their place. `RangeScan` calls `leafFor(startKey)` to find the leaf where the range begins and `lowerBound` to find the offset within it, then hands back an `Iterator` positioned there. `Next` returns the entry at the cursor and advances; when it runs off the end of a leaf it follows `nextLeaf` to the sibling and continues at offset zero, with no descent through internal nodes. The `endKey` bound is checked on each returned key, so an inclusive `[start, end]` scan stops the moment a key exceeds `end`. A nil `endKey` means open-ended, running to the last key in the tree. This is the whole reason a B+Tree beats a B-Tree for ranges: after one O(log n) descent, every subsequent key costs one pointer-follow.

Append to `btree.go`:

```go
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

// Iterator scans leaves in ascending key order. Call Next until ok is false.
// Modifying the tree during iteration produces undefined results.
type Iterator struct {
	tree   *Tree
	endKey []byte // nil = unbounded
	cur    *node
	pos    int
}

// RangeScan returns an iterator over all keys in [startKey, endKey].
// If endKey is nil the scan is open-ended (continues to the last key).
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
```

### The runnable demo

The demo inserts six fruit names in scrambled order, searches one by name and prints the page from its `RecordID`, then range-scans the closed interval `[banana, date]` to show the scan emits keys in sorted order regardless of insertion order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/search-and-range-scan"
)

func main() {
	store := btree.NewMemStore()
	tree, err := btree.Open(store, 0)
	if err != nil {
		log.Fatal(err)
	}

	words := []string{"fig", "apple", "date", "banana", "elderberry", "cherry"}
	for i, w := range words {
		if err := tree.Insert([]byte(w), btree.MakeRecordID(uint64(i+1), 0)); err != nil {
			log.Fatalf("insert %q: %v", w, err)
		}
	}

	rid, ok, err := tree.Search([]byte("banana"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("search banana: found=%v page=%d\n", ok, rid.Page())

	iter, err := tree.RangeScan([]byte("banana"), []byte("date"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print("range [banana, date]:")
	for {
		k, _, ok := iter.Next()
		if !ok {
			break
		}
		fmt.Printf(" %s", k)
	}
	fmt.Println()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
search banana: found=true page=4
range [banana, date]: banana cherry date
```

### Tests

`TestInsertAndSearch` round-trips a handful of keys through insert and lookup. `TestSearchMissingKey` and `TestSearchKeyTooLong` pin the absent-key and oversized-key signals. `TestLeafSplitSearchable` inserts past the leaf capacity and confirms every key is still found after splits. `TestRangeScan` and `TestRangeScanUnbounded` check the bounded and open-ended scans. `TestInsertSorted1000` is the stress case: a thousand keys inserted in random order, then a full scan that must return exactly a thousand keys in strictly ascending order with no gaps.

Create `btree_test.go`:

```go
package btree

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

func openTree(t *testing.T) *Tree {
	t.Helper()
	tr, err := Open(NewMemStore(), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return tr
}

func TestInsertAndSearch(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	pairs := []struct {
		key string
		rid RecordID
	}{
		{"apple", MakeRecordID(1, 0)},
		{"banana", MakeRecordID(2, 0)},
		{"cherry", MakeRecordID(3, 0)},
	}
	for _, p := range pairs {
		if err := tr.Insert([]byte(p.key), p.rid); err != nil {
			t.Fatalf("Insert(%q): %v", p.key, err)
		}
	}
	for _, p := range pairs {
		got, ok, err := tr.Search([]byte(p.key))
		if err != nil {
			t.Fatalf("Search(%q): %v", p.key, err)
		}
		if !ok {
			t.Fatalf("Search(%q): not found", p.key)
		}
		if got != p.rid {
			t.Fatalf("Search(%q) = %v, want %v", p.key, got, p.rid)
		}
	}
}

func TestSearchMissingKey(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	_ = tr.Insert([]byte("a"), MakeRecordID(1, 0))
	_, ok, err := tr.Search([]byte("z"))
	if err != nil || ok {
		t.Fatalf("Search(missing): ok=%v err=%v", ok, err)
	}
}

func TestSearchKeyTooLong(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	_, _, err := tr.Search(make([]byte, MaxKeySize+1))
	if !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("err = %v, want ErrKeyTooLong", err)
	}
}

func TestLeafSplitSearchable(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	n := leafCap + 5
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key%04d", i))
		if err := tr.Insert(key, MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key%04d", i))
		if _, ok, err := tr.Search(key); err != nil || !ok {
			t.Fatalf("Search %d after split: ok=%v err=%v", i, ok, err)
		}
	}
}

func TestRangeScan(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	for i, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		if err := tr.Insert([]byte(k), MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	iter, err := tr.RangeScan([]byte("c"), []byte("e"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"c", "d", "e"}
	for i, w := range want {
		k, _, ok := iter.Next()
		if !ok {
			t.Fatalf("Next() stopped early at i=%d", i)
		}
		if string(k) != w {
			t.Fatalf("Next()[%d] = %q, want %q", i, k, w)
		}
	}
	if _, _, ok := iter.Next(); ok {
		t.Fatal("iterator should be exhausted after endKey")
	}
}

func TestRangeScanUnbounded(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	for i, k := range []string{"a", "b", "c"} {
		if err := tr.Insert([]byte(k), MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	iter, err := tr.RangeScan([]byte("b"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for {
		k, _, ok := iter.Next()
		if !ok {
			break
		}
		got = append(got, string(k))
	}
	if len(got) != 2 || got[0] != "b" || got[1] != "c" {
		t.Fatalf("unbounded scan = %v, want [b c]", got)
	}
}

func TestInsertSorted1000(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	const n = 1000
	for _, i := range rand.Perm(n) {
		key := []byte(fmt.Sprintf("%08d", i))
		if err := tr.Insert(key, MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	iter, err := tr.RangeScan([]byte("00000000"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var prev []byte
	count := 0
	for {
		k, _, ok := iter.Next()
		if !ok {
			break
		}
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			t.Fatalf("out of order: %q >= %q", prev, k)
		}
		prev = append([]byte(nil), k...)
		count++
	}
	if count != n {
		t.Fatalf("scan returned %d keys, want %d", count, n)
	}
}

func ExampleTree_Search() {
	store := NewMemStore()
	tree, _ := Open(store, 0)
	_ = tree.Insert([]byte("go"), MakeRecordID(10, 2))

	rid, ok, _ := tree.Search([]byte("go"))
	fmt.Printf("found=%v page=%d slot=%d\n", ok, rid.Page(), rid.Slot())
	// Output: found=true page=10 slot=2
}
```

## Review

Search and range scan are correct when a lookup finds every inserted key after arbitrary splits, an absent key returns `(0, false, nil)` rather than an error, and a full scan of a randomly built tree returns every key in strictly ascending order with no duplicates or gaps. The decisive property the thousand-key test exercises is that the leaf chain stays globally sorted across every split, because the scan trusts it completely: it descends once and then never consults an internal node again. Confirm that a bounded scan stops at the first key past `endKey` and that an unbounded scan runs to the last leaf.

The pitfalls are subtle because the read paths look trivial. The first is conflating a missing key with an error: a caller that treats any non-`ok` result as failure will choke on perfectly normal absent keys, while one that ignores the error will read past a corrupt page. The second is forgetting the inclusive bound check in `Next`, which turns `[c, e]` into an open-ended scan that walks to the end of the tree. The third is re-descending from the root on every `Next` instead of following `nextLeaf` — it still returns correct keys but throws away the entire performance argument for the linked-leaf design, turning an O(1)-per-step scan back into O(log n) per step.

## Resources

- [`bytes`](https://pkg.go.dev/bytes) — `bytes.Compare` for the range bound and `bytes.Equal` for the exact-match check in `Search`.
- [CMU 15-445/645 Fall 2024, B+Tree Indexes](https://15445.courses.cs.cmu.edu/fall2024/slides/08-indexes1.pdf) — point lookup versus sequential scan and why leaf-linked nodes make range queries cheap.
- [BoltDB (bbolt): a production B+Tree in Go](https://github.com/etcd-io/bbolt) — a real embedded engine whose cursor walks leaf pages exactly as this iterator does.

---

Back to [02-insert-and-node-splits.md](02-insert-and-node-splits.md) | Next: [04-delete.md](04-delete.md)
