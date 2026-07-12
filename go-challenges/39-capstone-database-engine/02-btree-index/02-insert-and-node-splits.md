# Exercise 2: Insert with Leaf and Internal Splits

A B+Tree grows by inserting into a leaf and, when that leaf overflows, splitting it and propagating a separator key upward. This single mechanism — split-and-propagate — is the entire growth story, and getting the leaf/internal asymmetry right (copy-up versus push-up) is the most important correctness decision in the whole structure. This exercise builds `Insert` on top of a page-backed tree, including the leaf split, the internal split, and the special case that creates a new root and raises the tree's height.

This module is fully self-contained. It carries its own copy of the page layout and store from the previous exercise, defines the `Tree`, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
btree.go             page layout + MemStore (baseline), Tree, Open, leafFor,
                     Insert, splitLeaf, insertIntoParent, splitInner, setParent
cmd/
  demo/
    main.go          insert enough keys to split the root, prove a duplicate is rejected
btree_test.go        leaf-chain order after splits, root split, duplicate, key-too-long
```

- Files: `btree.go`, `cmd/demo/main.go`, `btree_test.go`.
- Implement: `Tree`, `Open`, `RootID`, `leafFor`, `Insert`, `splitLeaf`, `insertIntoParent`, `splitInner`, `setParent`, plus the helpers `lowerBound`, `cloneKeys`, `cloneVals`.
- Test: `btree_test.go` inserts past the leaf capacity and walks the leaf sibling chain to confirm sorted order and exact count, checks that the root becomes internal after a split, and that duplicate and oversized keys are rejected.
- Verify: `go test -run 'TestInsert' -race ./...`

### The baseline: pages, store, and descent

Insert needs the page layout, the in-memory store, and a way to find the leaf a key belongs in. That baseline is the page-encoding work from the previous exercise, duplicated here so this module stands alone. The one new piece of descent logic is `leafFor`: starting at the root, it reads each node, and at an internal node runs a binary search for the smallest index `i` where `keys[i] > key`, then follows `children[i]`. That index is exactly the child whose subtree must contain the key under the routing invariant `keys[i-1] <= key < keys[i]`. It stops when it reaches a leaf and returns that leaf's page ID.

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

// node is the decoded in-memory representation of one page.
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

// Tree is a page-backed B+Tree. It is not safe for concurrent use; callers
// that need concurrent access must wrap with sync.RWMutex.
type Tree struct {
	store  PageStore
	rootID uint64
}

// Open creates or re-opens a B+Tree backed by store. Pass rootPageID = 0 to
// create a new tree. Pass the value returned by a previous call to RootID to
// re-open an existing tree.
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

// RootID returns the current root page ID. This value changes after a root
// split; persist it when checkpointing the tree.
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
		// Find the smallest i where keys[i] > key; follow children[i].
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
```

### Insert, and the copy-up versus push-up asymmetry

`Insert` descends to the right leaf, rejects a duplicate, and splices the new `(key, value)` into the leaf's sorted slices at the position `lowerBound` returns. If the leaf still fits within `leafCap` it is written back and the work is done. Only on overflow does it call `splitLeaf`.

A leaf split allocates a right sibling, moves the upper half of the entries into it, repairs the sibling chain so `left.nextLeaf` points at the new right leaf and the right leaf inherits the old `nextLeaf`, and then *copies* the right leaf's smallest key up into the parent as a separator. The word "copies" is load-bearing: the key still physically lives in the right leaf, because a search for that key must descend to and find it among the leaf's entries. This is copy-up.

An internal split is different in exactly one way and it is the way that trips people up. `splitInner` takes the middle key, moves the keys and children above it into a new right node, and then *pushes* the middle key up — it is removed from both halves and given only to the parent. An internal node is a pure router; the pushed key is the boundary between the two child subtrees and must appear in neither of them, or routing would see it twice. This is push-up. After moving children to the new right node, their `parent` pointers are repaired with `setParent`.

`insertIntoParent` is the shared upward step. If the node that split was the root (parent ID 0), it allocates a new internal root with the two halves as children and re-parents them; this is the only operation that raises tree height. Otherwise it splices the separator and the new right child into the parent — the right child at `pos+1`, never `pos` — and, if the parent now overflows, recurses through `splitInner`.

Append to `btree.go`:

```go
// Insert adds key -> value. Returns ErrDuplicate if the key already exists.
// Returns ErrKeyTooLong if len(key) > MaxKeySize.
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
	// Shift keys and vals to make room at pos.
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
	// Copy-up: right.keys[0] becomes the separator in the parent.
	return t.insertIntoParent(leafID, right.keys[0], rightID, leaf.parent)
}

func (t *Tree) insertIntoParent(leftID uint64, key []byte, rightID uint64, parentID uint64) error {
	if parentID == 0 {
		// The node being split was the root; create a new root above it.
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
	// Push-up: the middle key is removed from both halves and moved to parent.
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

// lowerBound returns the smallest index i such that keys[i] >= key.
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

### The runnable demo

The demo makes a split observable through the exported surface alone. It records the root page ID, inserts twenty keys — more than the leaf capacity of fifteen, so the leaf overflows and the root is replaced by a new internal node — and reports that the root ID changed, which can only happen on a root split. It then attempts to re-insert an existing key and shows the duplicate is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"log"

	"example.com/insert-and-node-splits"
)

func main() {
	store := btree.NewMemStore()
	tree, err := btree.Open(store, 0)
	if err != nil {
		log.Fatal(err)
	}

	before := tree.RootID()
	const n = 20
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key%04d", i))
		if err := tree.Insert(key, btree.MakeRecordID(uint64(i), 0)); err != nil {
			log.Fatalf("insert %d: %v", i, err)
		}
	}
	after := tree.RootID()

	fmt.Printf("inserted %d keys\n", n)
	fmt.Printf("root split occurred: %v\n", before != after)

	dupErr := tree.Insert([]byte("key0000"), btree.MakeRecordID(0, 0))
	fmt.Printf("duplicate rejected: %v\n", errors.Is(dupErr, btree.ErrDuplicate))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
inserted 20 keys
root split occurred: true
duplicate rejected: true
```

### Tests

`TestInsertLeafChain` inserts more keys than one leaf holds, walks the leaf sibling chain from the leftmost leaf, and asserts the keys come out strictly sorted and complete — the property a correct split sequence must preserve. `TestInsertRandomOrderChain` repeats that with two hundred keys inserted in random order, forcing internal splits as well. `TestInsertRootSplit` confirms the root is a leaf with one key and becomes an internal node once a split propagates. `TestInsertDuplicate` and `TestInsertKeyTooLong` pin the two error paths.

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

// chainKeys walks the leaf sibling chain from the leftmost leaf and returns
// every key in leaf order.
func chainKeys(t *testing.T, tr *Tree) [][]byte {
	t.Helper()
	id, err := tr.leafFor([]byte{})
	if err != nil {
		t.Fatalf("leafFor: %v", err)
	}
	var out [][]byte
	for {
		n, err := tr.readNode(id)
		if err != nil {
			t.Fatalf("readNode: %v", err)
		}
		out = append(out, n.keys...)
		if n.nextLeaf == 0 {
			return out
		}
		id = n.nextLeaf
	}
}

func TestInsertLeafChain(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	n := leafCap + 5
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key%04d", i))
		if err := tr.Insert(key, MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	keys := chainKeys(t, tr)
	if len(keys) != n {
		t.Fatalf("chain has %d keys, want %d", len(keys), n)
	}
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Fatalf("leaf chain out of order at %d: %q >= %q", i, keys[i-1], keys[i])
		}
	}
}

func TestInsertRandomOrderChain(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	const n = 200
	for _, i := range rand.Perm(n) {
		key := []byte(fmt.Sprintf("%05d", i))
		if err := tr.Insert(key, MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	keys := chainKeys(t, tr)
	if len(keys) != n {
		t.Fatalf("chain has %d keys, want %d", len(keys), n)
	}
	for i := 1; i < len(keys); i++ {
		if bytes.Compare(keys[i-1], keys[i]) >= 0 {
			t.Fatalf("out of order at %d: %q >= %q", i, keys[i-1], keys[i])
		}
	}
}

func TestInsertRootSplit(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	root, _ := tr.readNode(tr.rootID)
	if root.kind != kindLeaf {
		t.Fatalf("fresh root should be a leaf")
	}
	for i := 0; i <= leafCap; i++ {
		key := []byte(fmt.Sprintf("key%04d", i))
		if err := tr.Insert(key, MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	root, _ = tr.readNode(tr.rootID)
	if root.kind != kindInternal {
		t.Fatalf("root should be internal after a split, got kind %d", root.kind)
	}
	if len(root.children) != len(root.keys)+1 {
		t.Fatalf("internal root: %d children, want %d", len(root.children), len(root.keys)+1)
	}
}

func TestInsertDuplicate(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	rid := MakeRecordID(1, 0)
	if err := tr.Insert([]byte("dup"), rid); err != nil {
		t.Fatal(err)
	}
	if err := tr.Insert([]byte("dup"), rid); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

func TestInsertKeyTooLong(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	err := tr.Insert(make([]byte, MaxKeySize+1), MakeRecordID(1, 0))
	if !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("err = %v, want ErrKeyTooLong", err)
	}
}
```

## Review

Insert is correct when the leaf chain it produces is always fully sorted and complete, no matter the insertion order, and when a split raises height by exactly one level via a new internal root. The single most important thing to get right is the copy-up versus push-up asymmetry: a leaf split keeps the separator in the right leaf and copies it up, while an internal split removes the pushed key from both halves and gives it only to the parent. Confirm via the chain walk that every key survives across many splits, that the root flips from leaf to internal at the right moment, and that duplicate and oversized keys are rejected before any page is written.

The classic pitfalls all live in the splice. The new right child after an internal insert belongs at `children[pos+1]`, not `children[pos]`; writing it to `pos` overwrites the existing left child and orphans a subtree. Treating an internal split as a copy-up duplicates the boundary key into a child and makes routing visit it twice. Forgetting to re-parent the children moved into a new right node leaves them pointing at their old parent, which only surfaces later when a rebalance or a re-descent trusts the stale pointer. Finally, comparing keys with `==` does not compile for byte slices and `bytes.Equal` is required — the duplicate check depends on it.

## Resources

- [`bytes`](https://pkg.go.dev/bytes) — `bytes.Compare` and `bytes.Equal`, the key comparator and equality test used in descent and the duplicate check.
- [CMU 15-445/645 Fall 2024, B+Tree Indexes](https://15445.courses.cs.cmu.edu/fall2024/slides/08-indexes1.pdf) — leaf versus internal splits, copy-up versus push-up, and how a root split raises height.
- [PostgreSQL nbtree README](https://github.com/postgres/postgres/blob/master/src/backend/access/nbtree/README) — how a production B+Tree chooses split points and propagates separators upward.

---

Back to [01-page-encoding-and-store.md](01-page-encoding-and-store.md) | Next: [03-search-and-range-scan.md](03-search-and-range-scan.md)
