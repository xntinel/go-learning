# Exercise 5: Balanced Delete with Borrow and Merge

The core delete leaves nodes underfull; this exercise restores the half-full invariant. After removing a key, if a non-root node drops below the minimum, it is repaired with the standard redistribute-then-merge policy: borrow one entry from a sibling that has more than the minimum, or, when both siblings are minimal, merge with one of them and pull the parent's separator down. A merge shrinks the parent, so the repair recurses upward; when the root internal node loses its last separator, its single child becomes the new root and the tree's height drops by one. This is the upward-cascading, height-changing repair that makes delete the harder half of a B+Tree.

This module is fully self-contained. It carries its own page layout, store, insert, search, and range-scan paths, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
btree.go             page layout + MemStore + Insert + Search + RangeScan (baseline)
delete.go            DeleteBalanced, rebalance, borrowFromLeft, borrowFromRight,
                     mergeNodes, childIndex
cmd/
  demo/
    main.go          insert 50 keys, delete 30, scan the survivors
delete_test.go       full-tree invariant check across random deletes, parity delete
```

- Files: `btree.go`, `delete.go`, `cmd/demo/main.go`, `delete_test.go`.
- Implement: `DeleteBalanced`, `rebalance`, `borrowFromLeft`, `borrowFromRight`, `mergeNodes`, `childIndex`.
- Test: `delete_test.go` deletes every key in random order while periodically asserting all B+Tree invariants, and deletes by parity while confirming survivors remain findable.
- Verify: `go test -run 'TestDeleteBalanced|ExampleTree' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/39-capstone-database-engine/02-btree-index/05-delete-balanced-borrow-and-merge/cmd/demo && cd go-solutions/39-capstone-database-engine/02-btree-index/05-delete-balanced-borrow-and-merge
```

### The baseline

This module needs a tree it can build, search, and scan, plus one extra helper, `cloneKey`, that the rebalance code uses when it copies a key into a separator slot. That baseline is the insert/search/range-scan tree from the earlier exercises, duplicated here so the module stands alone. The new work — everything that restores the invariant — lives in its own `delete.go`.

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

### Rebalance: the decision tree

After `DeleteBalanced` removes a key and writes the leaf, it calls `rebalance` on that node. `rebalance` is the whole repair, and it reads as a small decision tree.

First, the root is exempt from the minimum-fill rule: a root may legally hold a single key, and the only structural action it needs is height collapse. If the root is an internal node that has lost its last separator (`len(keys) == 0`), its single remaining child becomes the new root and the tree shrinks by one level. Otherwise the root needs nothing.

For a non-root node, if it still has at least `min` entries (`leafCap/2` for leaves, `innerCap/2` for internal nodes) the invariant already holds and `rebalance` returns. If it is underfull, the repair consults a sibling. It prefers to borrow: if the left sibling exists and has more than `min`, borrow from it; otherwise if the right sibling has more than `min`, borrow from it. Borrowing — redistribution — touches only the node, one sibling, and the parent, and crucially does not change the parent's key count, so it never cascades. Only when *no* sibling can spare an entry does it fall back to a merge, combining the node with a sibling. A merge deletes a separator from the parent, so it then recurses by calling `rebalance` on the parent, and this is the path that can climb all the way to the root and reduce height.

The leaf/internal asymmetry from splits reappears in borrowing. At a leaf, redistribution simply *copies* the new boundary key into the parent separator, because the separator equals the smallest key of the right subtree. At an internal node, redistribution *rotates*: the parent separator descends into the underfull node and a sibling key ascends to take its place, because an internal separator is a router, not a stored value, and the moved child pointer must travel with it.

Create `delete.go`:

```go
package btree

import (
	"bytes"
	"fmt"
)

// DeleteBalanced removes key and restores the half-full invariant by borrowing
// from a sibling (redistribution) or merging when no sibling can spare an entry.
// It returns (true, nil) on success and (false, nil) when the key is absent.
func (t *Tree) DeleteBalanced(key []byte) (bool, error) {
	if len(key) > MaxKeySize {
		return false, fmt.Errorf("%w: len=%d", ErrKeyTooLong, len(key))
	}
	leafID, err := t.leafFor(key)
	if err != nil {
		return false, err
	}
	leaf, err := t.readNode(leafID)
	if err != nil {
		return false, err
	}
	pos := lowerBound(leaf.keys, key)
	if pos >= len(leaf.keys) || !bytes.Equal(leaf.keys[pos], key) {
		return false, nil
	}
	leaf.keys = append(leaf.keys[:pos], leaf.keys[pos+1:]...)
	leaf.vals = append(leaf.vals[:pos], leaf.vals[pos+1:]...)
	if err := t.writeNode(leafID, leaf); err != nil {
		return false, err
	}
	if err := t.rebalance(leafID, leaf); err != nil {
		return false, err
	}
	return true, nil
}

// rebalance repairs node id if it dropped below the minimum fill. The node has
// already been written; rebalance may rewrite it, a sibling, and the parent,
// and may recurse onto the parent after a merge.
func (t *Tree) rebalance(id uint64, n *node) error {
	if id == t.rootID {
		// The root is exempt from the minimum-fill rule. If it is an internal
		// node that lost its last separator, its single child becomes the new
		// root and the tree height shrinks by one.
		if n.kind == kindInternal && len(n.keys) == 0 {
			child := n.children[0]
			if err := t.setParent(child, 0); err != nil {
				return err
			}
			t.rootID = child
		}
		return nil
	}
	min := leafCap / 2
	if n.kind == kindInternal {
		min = innerCap / 2
	}
	if len(n.keys) >= min {
		return nil
	}
	parentID := n.parent
	parent, err := t.readNode(parentID)
	if err != nil {
		return err
	}
	ci := childIndex(parent, id)
	if ci < 0 {
		return fmt.Errorf("btree: node %d not a child of parent %d", id, parentID)
	}
	if ci > 0 {
		leftID := parent.children[ci-1]
		left, err := t.readNode(leftID)
		if err != nil {
			return err
		}
		if len(left.keys) > min {
			return t.borrowFromLeft(parentID, parent, ci, leftID, left, id, n)
		}
	}
	if ci < len(parent.children)-1 {
		rightID := parent.children[ci+1]
		right, err := t.readNode(rightID)
		if err != nil {
			return err
		}
		if len(right.keys) > min {
			return t.borrowFromRight(parentID, parent, ci, rightID, right, id, n)
		}
	}
	// No sibling can spare an entry: merge with one of them.
	if ci > 0 {
		leftID := parent.children[ci-1]
		left, err := t.readNode(leftID)
		if err != nil {
			return err
		}
		return t.mergeNodes(parentID, parent, ci-1, leftID, left, id, n)
	}
	rightID := parent.children[ci+1]
	right, err := t.readNode(rightID)
	if err != nil {
		return err
	}
	return t.mergeNodes(parentID, parent, ci, id, n, rightID, right)
}

// childIndex returns the index of childID within parent.children, or -1.
func childIndex(parent *node, childID uint64) int {
	for i, c := range parent.children {
		if c == childID {
			return i
		}
	}
	return -1
}

// borrowFromLeft moves the last entry of the left sibling into the front of n
// and updates the separator parent.keys[ci-1].
func (t *Tree) borrowFromLeft(parentID uint64, parent *node, ci int, leftID uint64, left *node, id uint64, n *node) error {
	if n.kind == kindLeaf {
		lastK := left.keys[len(left.keys)-1]
		lastV := left.vals[len(left.vals)-1]
		left.keys = left.keys[:len(left.keys)-1]
		left.vals = left.vals[:len(left.vals)-1]
		n.keys = append([][]byte{lastK}, n.keys...)
		n.vals = append([]RecordID{lastV}, n.vals...)
		parent.keys[ci-1] = cloneKey(n.keys[0])
	} else {
		// Rotate: the parent separator descends into n; the sibling's last key
		// ascends to become the new separator.
		sepK := parent.keys[ci-1]
		movedChild := left.children[len(left.children)-1]
		lastK := left.keys[len(left.keys)-1]
		left.keys = left.keys[:len(left.keys)-1]
		left.children = left.children[:len(left.children)-1]
		n.keys = append([][]byte{sepK}, n.keys...)
		n.children = append([]uint64{movedChild}, n.children...)
		parent.keys[ci-1] = cloneKey(lastK)
		if err := t.setParent(movedChild, id); err != nil {
			return err
		}
	}
	if err := t.writeNode(leftID, left); err != nil {
		return err
	}
	if err := t.writeNode(id, n); err != nil {
		return err
	}
	return t.writeNode(parentID, parent)
}

// borrowFromRight moves the first entry of the right sibling onto the end of n
// and updates the separator parent.keys[ci].
func (t *Tree) borrowFromRight(parentID uint64, parent *node, ci int, rightID uint64, right *node, id uint64, n *node) error {
	if n.kind == kindLeaf {
		firstK := right.keys[0]
		firstV := right.vals[0]
		right.keys = right.keys[1:]
		right.vals = right.vals[1:]
		n.keys = append(n.keys, firstK)
		n.vals = append(n.vals, firstV)
		parent.keys[ci] = cloneKey(right.keys[0])
	} else {
		sepK := parent.keys[ci]
		movedChild := right.children[0]
		firstK := right.keys[0]
		right.keys = right.keys[1:]
		right.children = right.children[1:]
		n.keys = append(n.keys, sepK)
		n.children = append(n.children, movedChild)
		parent.keys[ci] = cloneKey(firstK)
		if err := t.setParent(movedChild, id); err != nil {
			return err
		}
	}
	if err := t.writeNode(rightID, right); err != nil {
		return err
	}
	if err := t.writeNode(id, n); err != nil {
		return err
	}
	return t.writeNode(parentID, parent)
}

// mergeNodes merges right into left, dropping the separator parent.keys[sep] and
// the right child pointer. For leaves the sibling link is repaired. For internal
// nodes the separator descends between the two halves. The parent then loses an
// entry, so mergeNodes recurses to rebalance it.
func (t *Tree) mergeNodes(parentID uint64, parent *node, sep int, leftID uint64, left *node, rightID uint64, right *node) error {
	if left.kind == kindLeaf {
		left.keys = append(left.keys, right.keys...)
		left.vals = append(left.vals, right.vals...)
		left.nextLeaf = right.nextLeaf
	} else {
		left.keys = append(left.keys, parent.keys[sep])
		left.keys = append(left.keys, right.keys...)
		left.children = append(left.children, right.children...)
		for _, cid := range right.children {
			if err := t.setParent(cid, leftID); err != nil {
				return err
			}
		}
	}
	if err := t.writeNode(leftID, left); err != nil {
		return err
	}
	// rightID is now unreferenced; a real page allocator would free it here.
	_ = rightID
	parent.keys = append(parent.keys[:sep], parent.keys[sep+1:]...)
	parent.children = append(parent.children[:sep+1], parent.children[sep+2:]...)
	if err := t.writeNode(parentID, parent); err != nil {
		return err
	}
	return t.rebalance(parentID, parent)
}
```

The merge bound is what makes this safe. An underfull node holds exactly `min-1` entries (the invariant held before the delete), and a minimal sibling holds `min`, so a leaf merge produces `2*min-1` entries and an internal merge `2*min` keys — both at or below capacity for the sizes in this lesson. Redistribution touches three nodes and never cascades because the parent's key count is unchanged; merge cascades upward and is the only path that lowers the tree's height.

### The runnable demo

The demo inserts fifty keys, deletes the first thirty with `DeleteBalanced`, and scans the survivors to show the count is exactly twenty. The structural repair — borrows, merges, possibly a height drop — happens silently; what the demo verifies is that the tree stays a correct, scannable index throughout.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/delete-balanced-borrow-and-merge"
)

func main() {
	store := btree.NewMemStore()
	tree, err := btree.Open(store, 0)
	if err != nil {
		log.Fatal(err)
	}

	const n = 50
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("%03d", i))
		if err := tree.Insert(key, btree.MakeRecordID(uint64(i), 0)); err != nil {
			log.Fatalf("insert %d: %v", i, err)
		}
	}
	fmt.Printf("inserted %d keys\n", n)

	const d = 30
	for i := 0; i < d; i++ {
		key := []byte(fmt.Sprintf("%03d", i))
		if _, err := tree.DeleteBalanced(key); err != nil {
			log.Fatalf("delete %d: %v", i, err)
		}
	}
	fmt.Printf("deleted %d keys\n", d)

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
	fmt.Printf("remaining: %d\n", count)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
inserted 50 keys
deleted 30 keys
remaining: 20
```

### Tests

The centerpiece is `checkInvariants`, a full-tree walk that fails if any B+Tree invariant is violated: all leaves at the same depth, keys strictly sorted within a node, every key inside its separator bounds, and every non-root node at least half full. `TestDeleteBalancedRestoresInvariant` inserts four hundred keys and deletes all of them in random order, re-checking invariants periodically and asserting the final tree is empty — the strongest possible stress on borrow, merge, and the cascading root collapse. `TestDeleteBalancedRemainingFindable` deletes by parity and confirms the survivors are still findable and the structure still holds. `TestDeleteBalancedErrors` pins the oversized-key error.

Create `delete_test.go`:

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
			if !isRoot && len(n.keys) < leafCap/2 {
				t.Fatalf("leaf %d underfull: %d keys < min %d", id, len(n.keys), leafCap/2)
			}
			return
		}
		if len(n.children) != len(n.keys)+1 {
			t.Fatalf("inner %d: %d children, want %d", id, len(n.children), len(n.keys)+1)
		}
		if isRoot {
			if len(n.keys) < 1 {
				t.Fatalf("root inner %d has no separators", id)
			}
		} else if len(n.keys) < innerCap/2 {
			t.Fatalf("inner %d underfull: %d keys < min %d", id, len(n.keys), innerCap/2)
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

func TestDeleteBalancedErrors(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	_, err := tr.DeleteBalanced(make([]byte, MaxKeySize+1))
	if !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("err = %v, want ErrKeyTooLong", err)
	}
}

func TestDeleteBalancedRestoresInvariant(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	const n = 400
	keys := make([][]byte, n)
	for i := 0; i < n; i++ {
		keys[i] = []byte(fmt.Sprintf("%05d", i))
		if err := tr.Insert(keys[i], MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	checkInvariants(t, tr)
	rng := rand.New(rand.NewSource(1))
	for step, idx := range rng.Perm(n) {
		k := keys[idx]
		ok, err := tr.DeleteBalanced(k)
		if err != nil {
			t.Fatalf("DeleteBalanced(%q): %v", k, err)
		}
		if !ok {
			t.Fatalf("DeleteBalanced(%q): reported missing", k)
		}
		if _, found, _ := tr.Search(k); found {
			t.Fatalf("key %q still present after delete", k)
		}
		if step%29 == 0 {
			checkInvariants(t, tr)
		}
	}
	checkInvariants(t, tr)
	iter, err := tr.RangeScan([]byte(""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := iter.Next(); ok {
		t.Fatal("tree should be empty after deleting every key")
	}
}

func TestDeleteBalancedRemainingFindable(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	const n = 200
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("%05d", i))
		if err := tr.Insert(key, MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i += 2 {
		key := []byte(fmt.Sprintf("%05d", i))
		ok, err := tr.DeleteBalanced(key)
		if err != nil || !ok {
			t.Fatalf("DeleteBalanced(%q): ok=%v err=%v", key, ok, err)
		}
	}
	checkInvariants(t, tr)
	for i := 1; i < n; i += 2 {
		key := []byte(fmt.Sprintf("%05d", i))
		if _, ok, _ := tr.Search(key); !ok {
			t.Fatalf("odd key %q should still exist", key)
		}
	}
	for i := 0; i < n; i += 2 {
		key := []byte(fmt.Sprintf("%05d", i))
		if _, ok, _ := tr.Search(key); ok {
			t.Fatalf("even key %q should be gone", key)
		}
	}
}

func ExampleTree_DeleteBalanced() {
	tr, _ := Open(NewMemStore(), 0)
	for _, k := range []string{"a", "b", "c"} {
		_ = tr.Insert([]byte(k), MakeRecordID(1, 0))
	}
	ok, _ := tr.DeleteBalanced([]byte("b"))
	_, found, _ := tr.Search([]byte("b"))
	fmt.Printf("deleted=%v found=%v\n", ok, found)
	// Output: deleted=true found=false
}
```

## Review

The rebalance is correct when the invariant survives an arbitrary delete sequence: deleting all four hundred keys in random order must end with an empty tree and never trip `checkInvariants` along the way. That single test exercises every branch — leaf and internal borrows from both sides, leaf and internal merges, and the root collapse that drops height. Confirm a borrow leaves the parent's key count unchanged (no cascade) while a merge reduces it by one and recurses, and that the root, alone among nodes, is allowed to fall below the minimum.

The pitfalls cluster around the internal-node rotation and the merge bookkeeping. Treating an internal borrow like a leaf borrow — copying a key up instead of rotating the separator down and a sibling key up — corrupts routing, because an internal separator is a router and the moved child pointer must travel with it. Forgetting to re-parent a moved or merged child leaves a stale parent pointer that the next rebalance trusts and mis-navigates. In `mergeNodes`, the parent loses both a key at `sep` and a child at `sep+1`; deleting the wrong child index detaches the surviving half. And the root collapse must re-parent the surviving child to 0 and update `rootID`, or the tree keeps a phantom level. The `checkInvariants` walk is the cheapest way to catch all of these at once.

## Resources

- [CMU 15-445/645 Fall 2024, B+Tree Indexes](https://15445.courses.cs.cmu.edu/fall2024/slides/08-indexes1.pdf) — redistribution versus merge, the half-full minimum, and merge-driven height reduction.
- [PostgreSQL nbtree README](https://github.com/postgres/postgres/blob/master/src/backend/access/nbtree/README) — how a production engine decides when to merge versus leave a page underfull.
- [BoltDB (bbolt): a production B+Tree in Go](https://github.com/etcd-io/bbolt) — its `rebalance` and `spill` paths are a real-world counterpart to this exercise's merge and split.

---

Back to [04-delete.md](04-delete.md) | Next: [06-bulk-loading.md](06-bulk-loading.md)
