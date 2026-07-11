# Exercise 4: Delete

Removing a key is the start of the hardest half of a B+Tree. This exercise builds the core delete: descend to the leaf, splice the key out of its sorted slices, and write the page back. That is genuinely all a correct point-delete requires for a single tree instance — but it deliberately stops short of rebalancing, so a node can fall below the half-full minimum. Seeing exactly where the invariant breaks here is what motivates the borrow-and-merge machinery in the next exercise.

This module is fully self-contained. It carries its own page layout, store, insert, and search paths, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
btree.go             page layout + MemStore + Insert + Search (baseline), Delete
cmd/
  demo/
    main.go          insert words, delete one, confirm it is gone and others remain
btree_test.go        delete present key, delete missing key, key-too-long, bulk delete
```

- Files: `btree.go`, `cmd/demo/main.go`, `btree_test.go`.
- Implement: `Delete`.
- Test: `btree_test.go` deletes a present key, reports a missing key as not deleted, rejects an oversized key, and deletes half of two hundred keys then confirms the survivors are still found.
- Verify: `go test -run 'TestDelete' -race ./...`

Set up the module:

```bash
mkdir -p delete/cmd/demo && cd delete
go mod init example.com/delete
```

### Why delete is the asymmetric, harder operation

An insert only ever makes a node fuller, and its single failure mode — overflow — is repaired by a split that propagates strictly upward and monotonically. Delete has none of that comfort. Removing a key can drop a node *below* the minimum fill, and restoring the invariant is not a local operation: it must consult an adjacent sibling, choose between two different repairs (borrow or merge), and possibly cascade a merge all the way to the root, even shrinking the tree's height. That is a lot of machinery, and a great deal of production code defers it: SQLite, for one, does not aggressively rebalance on delete; it tolerates underfull pages and reuses their free space later.

So this exercise builds the part that is unambiguous and always correct on its own — the removal itself — and is honest that the result may be underfull. `Delete` descends with `leafFor`, finds the key's position with `lowerBound`, and confirms an exact match with `bytes.Equal`. A missing key is reported as `(false, nil)`: not an error, just nothing to do. On a match it removes the entry from both parallel slices with the standard `append(s[:pos], s[pos+1:]...)` splice and writes the leaf back. The leaf may now hold fewer than `leafCap/2` entries, which violates the half-full invariant — that is expected here and is exactly what the next exercise repairs.

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

### Delete itself

The new code is small and that is the point: a correct point-delete is just a guarded splice. The guard returns `(false, nil)` when the key is absent, distinguishing "nothing to delete" from an error. The `append(leaf.keys[:pos], leaf.keys[pos+1:]...)` idiom removes the entry in place by sliding everything after `pos` left by one; the same is done to the parallel `vals` slice so the two stay aligned.

Append to `btree.go`:

```go
// Delete removes key from the tree. Returns (true, nil) on success,
// (false, nil) when the key does not exist. The leaf may fall below the
// half-full minimum; this core delete does not rebalance.
func (t *Tree) Delete(key []byte) (bool, error) {
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
	return true, t.writeNode(leafID, leaf)
}
```

### The runnable demo

The demo inserts six keys, deletes one, and confirms through `Search` that the deleted key is gone while an untouched key survives. This is the entire user-visible contract of a point-delete; the structural underfill it may leave behind is invisible to a lookup, which is precisely why it can be deferred.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/delete"
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
	fmt.Printf("inserted %d keys\n", len(words))

	ok, err := tree.Delete([]byte("apple"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("deleted apple: %v\n", ok)

	_, found, _ := tree.Search([]byte("apple"))
	fmt.Printf("apple after delete: found=%v\n", found)

	_, stillThere, _ := tree.Search([]byte("date"))
	fmt.Printf("date still present: found=%v\n", stillThere)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
inserted 6 keys
deleted apple: true
apple after delete: found=false
date still present: found=true
```

### Tests

`TestDelete` removes a present key and confirms it is gone while neighbours remain. `TestDeleteMissingKey` checks the absent-key signal is `(false, nil)`. `TestDeleteKeyTooLong` pins the oversized-key error. `TestDeleteHalfRemainingFindable` inserts two hundred keys, deletes every even one, and confirms the odd keys are all still found and the even ones all return not-found — exercising deletes that span multiple leaves while leaving the tree searchable.

Create `btree_test.go`:

```go
package btree

import (
	"errors"
	"fmt"
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

func TestDelete(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	for i, k := range []string{"x", "y", "z"} {
		if err := tr.Insert([]byte(k), MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	ok, err := tr.Delete([]byte("y"))
	if err != nil || !ok {
		t.Fatalf("Delete: ok=%v err=%v", ok, err)
	}
	if _, found, _ := tr.Search([]byte("y")); found {
		t.Fatal("deleted key should not be found")
	}
	for _, k := range []string{"x", "z"} {
		if _, found, _ := tr.Search([]byte(k)); !found {
			t.Fatalf("%q should still exist after deleting y", k)
		}
	}
}

func TestDeleteMissingKey(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	ok, err := tr.Delete([]byte("absent"))
	if err != nil || ok {
		t.Fatalf("Delete(missing): ok=%v err=%v", ok, err)
	}
}

func TestDeleteKeyTooLong(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	_, err := tr.Delete(make([]byte, MaxKeySize+1))
	if !errors.Is(err, ErrKeyTooLong) {
		t.Fatalf("err = %v, want ErrKeyTooLong", err)
	}
}

func TestDeleteHalfRemainingFindable(t *testing.T) {
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
		ok, err := tr.Delete(key)
		if err != nil || !ok {
			t.Fatalf("Delete(%q): ok=%v err=%v", key, ok, err)
		}
	}
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

func ExampleTree_Delete() {
	tr, _ := Open(NewMemStore(), 0)
	for _, k := range []string{"a", "b", "c"} {
		_ = tr.Insert([]byte(k), MakeRecordID(1, 0))
	}
	ok, _ := tr.Delete([]byte("b"))
	_, found, _ := tr.Search([]byte("b"))
	fmt.Printf("deleted=%v found=%v\n", ok, found)
	// Output: deleted=true found=false
}
```

## Review

This core delete is correct when every removed key becomes unfindable, every untouched key stays findable, and a missing key is reported as `(false, nil)` rather than an error. Confirm the bulk test: after deleting every even key among two hundred, all odd keys must still be found and all even keys must be gone, which proves the splice keeps the keys and values aligned across many leaves. What this delete deliberately does *not* guarantee is the half-full invariant — leaves can drop below `leafCap/2` entries, and that is the gap the next exercise closes.

The pitfalls are the splice and the signal. Forgetting to apply the identical splice to the `vals` slice leaves keys and values misaligned, so a later search returns the wrong `RecordID` for a shifted key — a silent corruption that no test of mere presence will catch, which is why the bulk test checks the survivors are findable rather than just counting them. Returning an error for a missing key, instead of `(false, nil)`, conflates "not there" with "I/O failed" and breaks idempotent callers that delete defensively. And comparing the candidate key with `==` rather than `bytes.Equal` does not compile for slices, so the match check must use `bytes.Equal`.

## Resources

- [`bytes`](https://pkg.go.dev/bytes) — `bytes.Equal` for the exact-match guard and `bytes.Compare` in the descent.
- [CMU 15-445/645 Fall 2024, B+Tree Indexes](https://15445.courses.cs.cmu.edu/fall2024/slides/08-indexes1.pdf) — why deletes underflow and why many systems defer the rebalance.
- [PostgreSQL nbtree README](https://github.com/postgres/postgres/blob/master/src/backend/access/nbtree/README) — how a production B+Tree handles deletions and reclaims space lazily rather than merging eagerly.

---

Back to [03-search-and-range-scan.md](03-search-and-range-scan.md) | Next: [05-delete-balanced-borrow-and-merge.md](05-delete-balanced-borrow-and-merge.md)
