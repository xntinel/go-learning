# Exercise 7: Leaf-Linked Cursor with Seek and Prefix Scan

A range iterator that starts at a fixed interval is useful, but a database needs something more general: a reusable cursor that can be positioned *anywhere* with `Seek(target)` — landing on the first key `>= target` — and then advanced with `Next`, walking across leaf boundaries through the sibling pointers without ever re-descending the tree. On top of that primitive, `PrefixScan` answers `LIKE 'abc%'` and composite-index lookups by returning every key sharing a byte prefix. This exercise builds both.

This module is fully self-contained. It carries its own page layout, store, and insert path, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
btree.go             page layout + MemStore + Insert/splits (baseline)
cursor.go            Cursor, NewCursor, Seek, Next, PrefixScan
cmd/
  demo/
    main.go          prefix-scan a set of words, seek to a key
cursor_test.go       seek + walk across leaves, seek past end, prefix-scan table
```

- Files: `btree.go`, `cursor.go`, `cmd/demo/main.go`, `cursor_test.go`.
- Implement: the `Cursor` type, `NewCursor`, `(*Cursor).Seek`, `(*Cursor).Next`, and `(*Tree).PrefixScan`.
- Test: `cursor_test.go` seeks into a 300-key tree and walks to the end across leaf boundaries, seeks past the last key, and prefix-scans a table of overlapping words.
- Verify: `go test -run 'TestCursor|TestPrefixScan|ExampleTree' -race ./...`

Set up the module:

```bash
mkdir -p cursor-seek-and-prefix-scan/cmd/demo && cd cursor-seek-and-prefix-scan
go mod init example.com/cursor-seek-and-prefix-scan
```

### The baseline

The cursor needs a populated tree to walk, so this module carries the page layout, store, descent, and insert path, duplicated here so it stands alone. The new work is the `Cursor` in `cursor.go`. The cursor reaches into the tree's internals — `leafFor`, `readNode`, `lowerBound`, and the leaf `nextLeaf` chain — which is why it lives in the same package rather than on top of the public API.

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

// cloneKey returns an independent copy of a key slice.
func cloneKey(k []byte) []byte {
	return append([]byte(nil), k...)
}
```

### Seek, Next, and prefix scan

`Seek(target)` descends once with `leafFor` to the leaf that could hold `target`, then `lowerBound` gives the offset of the first key `>= target` within it. There is one edge case the loop after it handles: `target` may sort *after* every key on the landing leaf, which happens when `target` falls in the gap between this leaf's last key and the next leaf's first. In that case the cursor follows `nextLeaf` to the next non-empty leaf and starts at offset zero; if the chain runs out, the cursor is exhausted and `Seek` returns `false`.

`Next` returns the key/value at the cursor and advances by one. When it reaches the end of a leaf it follows `nextLeaf` to the sibling — the same single-pointer hop the range iterator uses — so a full walk after a `Seek` costs one descent plus one pointer-follow per leaf boundary, never another root-to-leaf traversal. A nil `cur` means exhausted.

`PrefixScan` is the payoff. It seeks to `prefix` itself (the first key `>= prefix` is the first candidate that could carry it) and then walks forward, collecting every key while `bytes.HasPrefix` holds and stopping at the first key that fails. That early stop is correct precisely because the keys are globally sorted: every key carrying a given prefix forms one contiguous run starting at the first key `>= prefix`, so the first key that loses the prefix is past the entire run. Collected keys are cloned so the caller owns them independently of the page buffers the cursor read through.

Create `cursor.go`:

```go
package btree

import "bytes"

// Cursor is a forward-only leaf cursor positioned by Seek and advanced by Next.
// It reads through the leaf sibling chain and is not safe for concurrent use.
type Cursor struct {
	tree *Tree
	cur  *node
	pos  int
}

// NewCursor returns an unpositioned cursor; call Seek before Next.
func (t *Tree) NewCursor() *Cursor { return &Cursor{tree: t} }

// Seek positions the cursor at the first key >= target. It returns ok=false
// (and leaves the cursor exhausted) when no such key exists.
func (c *Cursor) Seek(target []byte) (ok bool, err error) {
	leafID, err := c.tree.leafFor(target)
	if err != nil {
		return false, err
	}
	leaf, err := c.tree.readNode(leafID)
	if err != nil {
		return false, err
	}
	c.cur = leaf
	c.pos = lowerBound(leaf.keys, target)
	// target may sort after every key on this leaf; skip to the next non-empty
	// leaf via the sibling chain.
	for c.pos >= len(c.cur.keys) {
		if c.cur.nextLeaf == 0 {
			c.cur = nil
			return false, nil
		}
		nxt, err := c.tree.readNode(c.cur.nextLeaf)
		if err != nil {
			return false, err
		}
		c.cur = nxt
		c.pos = 0
	}
	return true, nil
}

// Next returns the current key/value and advances the cursor. ok is false when
// the cursor is exhausted.
func (c *Cursor) Next() (key []byte, value RecordID, ok bool, err error) {
	if c.cur == nil {
		return nil, 0, false, nil
	}
	for c.pos >= len(c.cur.keys) {
		if c.cur.nextLeaf == 0 {
			c.cur = nil
			return nil, 0, false, nil
		}
		nxt, e := c.tree.readNode(c.cur.nextLeaf)
		if e != nil {
			return nil, 0, false, e
		}
		c.cur = nxt
		c.pos = 0
	}
	k := c.cur.keys[c.pos]
	v := c.cur.vals[c.pos]
	c.pos++
	return k, v, true, nil
}

// PrefixScan returns, in order, all keys that start with prefix together with
// their values. It Seeks to the first candidate and walks the sibling chain,
// stopping at the first key that no longer carries the prefix.
func (t *Tree) PrefixScan(prefix []byte) (keys [][]byte, vals []RecordID, err error) {
	c := t.NewCursor()
	ok, err := c.Seek(prefix)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, nil
	}
	for {
		k, v, more, err := c.Next()
		if err != nil {
			return nil, nil, err
		}
		if !more || !bytes.HasPrefix(k, prefix) {
			break
		}
		keys = append(keys, cloneKey(k))
		vals = append(vals, v)
	}
	return keys, vals, nil
}
```

### The runnable demo

The demo inserts six words, prefix-scans `"ap"` to show the three words that share it come back in sorted order, then seeks to `"band"` and reads the first key at or after it. Both operations descend once and then read leaves; neither re-enters an internal node.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/cursor-seek-and-prefix-scan"
)

func main() {
	store := btree.NewMemStore()
	tree, err := btree.Open(store, 0)
	if err != nil {
		log.Fatal(err)
	}

	words := []string{"apple", "apply", "apt", "banana", "band", "car"}
	for i, w := range words {
		if err := tree.Insert([]byte(w), btree.MakeRecordID(uint64(i), 0)); err != nil {
			log.Fatalf("insert %q: %v", w, err)
		}
	}

	keys, _, err := tree.PrefixScan([]byte("ap"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print("prefix ap:")
	for _, k := range keys {
		fmt.Printf(" %s", k)
	}
	fmt.Println()

	c := tree.NewCursor()
	ok, err := c.Seek([]byte("band"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("seek band: ok=%v\n", ok)
	if ok {
		k, _, _, _ := c.Next()
		fmt.Printf("first key >= band: %s\n", k)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
prefix ap: apple apply apt
seek band: ok=true
first key >= band: band
```

### Tests

`TestCursorSeekAndNext` builds a 300-key tree, seeks into the middle, and walks to the end, asserting every key comes out in order across the leaf boundaries the splits created. `TestCursorSeekPastEnd` seeks past the last key and asserts the cursor reports exhausted. `TestPrefixScan` is table-driven over overlapping words — `"app"`, single-character `"a"`, `"ban"`, a full-key prefix, and a no-match prefix — checking each returns exactly the contiguous run that carries it.

Create `cursor_test.go`:

```go
package btree

import (
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

func TestCursorSeekAndNext(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	const n = 300
	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("k%05d", i))
		if err := tr.Insert(key, MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
	}
	c := tr.NewCursor()
	ok, err := c.Seek([]byte("k00100"))
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if !ok {
		t.Fatal("Seek(k00100) found nothing")
	}
	for i := 100; i < n; i++ {
		k, _, more, err := c.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !more {
			t.Fatalf("cursor exhausted early at i=%d", i)
		}
		want := fmt.Sprintf("k%05d", i)
		if string(k) != want {
			t.Fatalf("Next = %q, want %q", k, want)
		}
	}
	if _, _, more, _ := c.Next(); more {
		t.Fatal("cursor should be exhausted after the last key")
	}
}

func TestCursorSeekPastEnd(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	for _, k := range []string{"a", "b", "c"} {
		if err := tr.Insert([]byte(k), MakeRecordID(1, 0)); err != nil {
			t.Fatal(err)
		}
	}
	c := tr.NewCursor()
	ok, err := c.Seek([]byte("z"))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("Seek(z) past the last key should return false")
	}
}

func TestPrefixScan(t *testing.T) {
	t.Parallel()
	tr := openTree(t)
	words := []string{"app", "apple", "apply", "apt", "band", "bandana", "banana", "car"}
	for i, w := range words {
		if err := tr.Insert([]byte(w), MakeRecordID(uint64(i), 0)); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		prefix string
		want   []string
	}{
		{"app prefix", "app", []string{"app", "apple", "apply"}},
		{"single char a", "a", []string{"app", "apple", "apply", "apt"}},
		{"ban prefix", "ban", []string{"banana", "band", "bandana"}},
		{"full key", "car", []string{"car"}},
		{"no match", "z", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			keys, _, err := tr.PrefixScan([]byte(tc.prefix))
			if err != nil {
				t.Fatalf("PrefixScan(%q): %v", tc.prefix, err)
			}
			if len(keys) != len(tc.want) {
				t.Fatalf("PrefixScan(%q) returned %d keys, want %d", tc.prefix, len(keys), len(tc.want))
			}
			for i, w := range tc.want {
				if string(keys[i]) != w {
					t.Fatalf("PrefixScan(%q)[%d] = %q, want %q", tc.prefix, i, keys[i], w)
				}
			}
		})
	}
}

func ExampleTree_PrefixScan() {
	tr, _ := Open(NewMemStore(), 0)
	for i, w := range []string{"apple", "apply", "apt", "banana"} {
		_ = tr.Insert([]byte(w), MakeRecordID(uint64(i), 0))
	}
	keys, _, _ := tr.PrefixScan([]byte("app"))
	for _, k := range keys {
		fmt.Printf("%s ", k)
	}
	fmt.Println()
	// Output: apple apply
}
```

## Review

The cursor is correct when `Seek` lands on the first key `>= target` for every target — including one that falls in the gap between two leaves, which forces the skip-to-next-leaf loop — and when a walk after `Seek` returns every subsequent key in order across leaf boundaries. The 300-key walk proves both, because a mid-tree seek followed by reading to the end can only succeed if the sibling chain is intact and the cursor never re-descends. Confirm that `PrefixScan` stops at the first non-matching key rather than scanning to the end, and that a no-match prefix returns nil cleanly.

The pitfalls are the seek edge case and the early stop. Omitting the skip-to-next-leaf loop in `Seek` makes a target that sorts after a leaf's last key return that leaf at an out-of-range offset, so the first `Next` either skips a key or mis-reports exhaustion. Continuing a prefix scan past the first non-matching key, instead of breaking, either returns keys that do not carry the prefix or, worse, walks the rest of the tree needlessly — the early stop is what makes the scan cost proportional to the match count, not the tree size. Aliasing collected keys into the page buffers instead of cloning them lets a later read overwrite results the caller still holds. And re-descending from the root on each `Next` would return correct keys while discarding the entire reason the leaves are linked.

## Resources

- [`bytes`](https://pkg.go.dev/bytes) — `bytes.HasPrefix` for the prefix test and `bytes.Compare` in the seek descent.
- [BoltDB (bbolt): a production B+Tree in Go](https://github.com/etcd-io/bbolt) — its `Cursor` with `Seek`, `Next`, and prefix iteration is the real-world analogue of this exercise.
- [CMU 15-445/645 Fall 2024, B+Tree Indexes](https://15445.courses.cs.cmu.edu/fall2024/slides/08-indexes1.pdf) — sequential leaf scans and how a sorted index answers prefix and range predicates.

---

Back to [06-bulk-loading.md](06-bulk-loading.md) | Next: [../03-buffer-pool-manager/00-concepts.md](../03-buffer-pool-manager/00-concepts.md)
