# 3. Anti-Entropy with Merkle Trees

Anti-entropy is the background mechanism that keeps replicas consistent after
network partitions, node restarts, and missed writes. Merkle trees make it
efficient: two replicas identify exactly which key ranges diverge by exchanging
O(differing_segments * depth) hash comparisons instead of scanning every key.
This lesson builds the complete Merkle tree and a local simulation of the sync
protocol using only the Go standard library.

The hard parts are three: making leaf hashes deterministic regardless of
key-iteration order; flushing only the affected ancestor chain after incremental
updates; and traversing the tree top-down so that subtrees with matching roots
are pruned without descending into their leaves.

```text
merkle/
  go.mod
  merkle.go
  store.go
  sync.go
  merkle_test.go
  cmd/demo/main.go
```

## Concepts

### A Merkle Tree as a Dataset Fingerprint

A Merkle tree assigns a cryptographic hash to every node in a binary tree.
Leaves cover equal partitions of the hash space derived from key names; each
leaf hash summarises all (key, value, timestamp) triples whose hashed key falls
in that partition. Each internal node stores `SHA-256(left_hash || right_hash)`.
The root is a single 32-byte fingerprint of the entire dataset.

Two replicas with identical roots are identical on every key in the covered
range — SHA-256 collision resistance makes this a practical guarantee. Two
replicas with differing roots locate the divergent leaf segments by recursive
descent: compare root hashes; if they agree, stop; otherwise descend into both
children and repeat. Subtrees whose hashes agree are pruned without inspection.
The traversal visits at most `differing_segments * depth` nodes.

Dynamo (DeCandia et al., SOSP 2007, §4.7) and Apache Cassandra both use exactly
this design. Cassandra's repair tracks Merkle trees per token range and has used
a default depth of 15 (32,768 leaf segments) since version 2.1.

### Array-Based Binary Heap Layout

Storing the tree in a flat slice avoids per-node heap allocation and is
cache-friendly:

- Root at index 0.
- Node `i` has left child at `2*i + 1`, right child at `2*i + 2`.
- Parent of node `i` is `(i - 1) / 2`.
- For depth `d`: leaves occupy indices `[2^d - 1, 2^(d+1) - 2]`.
- Total node count: `2^(d+1) - 1`.
- Leaf segment `l` is at array index `(2^d - 1) + l`.

Depth 15 yields a 65,535-element slice of `[32]byte` values (approximately
2 MB), well within a single cache-coherent region on modern hardware.

### Key-to-Segment Mapping

A key maps to a leaf segment by hashing the key string with SHA-256, reading
the first 8 bytes as a big-endian uint64, and right-shifting to retain the top
`d` bits. For depth 15 the result is in `[0, 32767]`. The mapping is a pure
function of the key; replicas use the same segment for the same key with no
coordination required.

### Deterministic Leaf Hashing

Two stores holding the same keys must produce identical leaf hashes. Because
keys within a segment are stored in a hash map, their iteration order is
non-deterministic. Sorting entries by key before hashing removes the ordering
dependency. Any total order works; lexicographic key order is the simplest.
Skipping the sort is the most common source of phantom divergences — two stores
appear to disagree on every anti-entropy round even though their data is
identical.

### Incremental Updates and Batched Flushing

When a single key is written, only its leaf segment's hash changes. Ancestors
up to the root also change, but the rest of the tree is untouched. Tracking a
dirty set of leaf segment indices and recomputing affected ancestors in one
bottom-up pass during `Flush` costs O(depth) hash computations per write. When
multiple dirty leaves share ancestors (common in high-write workloads), Flush
recomputes each ancestor exactly once regardless of how many dirty leaves
contributed it: collect all affected ancestor indices first, then sweep level
by level from deepest to shallowest.

### Concurrency Boundary

The tree supports concurrent reads and single-writer updates. `UpdateLeaf` and
`Flush` acquire a write lock; `Root` and `NodeAt` acquire a read lock. A store
that takes foreground write traffic while running background anti-entropy should
take a snapshot of the relevant segment entries under the store's own lock,
release the store lock, then call `UpdateLeaf` and `Flush` on the tree. This
keeps the tree lock held only for the duration of the hash computation, not for
the duration of data access.

## Exercises

This is a library, not a program. The core package is `package merkle`; the
runnable demo is in `cmd/demo`.

### Exercise 1: The Merkle Tree

Create `merkle.go`:

```go
package merkle

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"sync"
)

// DefaultDepth is 15, yielding 32,768 leaf segments.
const DefaultDepth = 15

// hash32 is a SHA-256 digest (32 bytes).
type hash32 [32]byte

// Tree is an array-based binary Merkle tree whose leaves cover equal slices of
// the uint64 key-hash space. Node i has children at 2*i+1 and 2*i+2.
// All exported methods are safe for concurrent use.
type Tree struct {
	mu     sync.RWMutex
	depth  int
	nodes  []hash32
	dirty  map[int]struct{} // pending leaf segment indices
	leaves int              // 2^depth
}

// Entry is a (key, value, timestamp) triple stored in the key-value layer.
type Entry struct {
	Key       string
	Value     []byte
	Timestamp uint64
}

// New returns a Merkle tree of the given depth (1..20).
// Depth d yields 2^d leaf segments and 2^(d+1)-1 total nodes.
func New(depth int) *Tree {
	if depth < 1 || depth > 20 {
		panic("merkle: depth must be 1..20")
	}
	leaves := 1 << depth
	return &Tree{
		depth:  depth,
		nodes:  make([]hash32, 2*leaves-1),
		dirty:  make(map[int]struct{}),
		leaves: leaves,
	}
}

// Depth returns the tree depth.
func (t *Tree) Depth() int { return t.depth }

// Leaves returns the number of leaf segments (2^depth).
func (t *Tree) Leaves() int { return t.leaves }

// SegmentOf returns the leaf segment index for key, in [0, Leaves()).
func (t *Tree) SegmentOf(key string) int {
	h := sha256.Sum256([]byte(key))
	prefix := binary.BigEndian.Uint64(h[:8])
	return int(prefix >> (64 - t.depth))
}

// leafIdx converts leaf segment l to its index in the nodes slice.
func (t *Tree) leafIdx(l int) int { return t.leaves - 1 + l }

// UpdateLeaf recomputes the hash for leaf segment l from entries and marks the
// leaf dirty. Call Flush to propagate changes up to the root.
// Entries are sorted by key internally; callers need not pre-sort.
func (t *Tree) UpdateLeaf(l int, entries []Entry) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nodes[t.leafIdx(l)] = leafHash(entries)
	t.dirty[l] = struct{}{}
}

// leafHash computes SHA-256 over the sorted (key, value, timestamp) triples.
func leafHash(entries []Entry) hash32 {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Key < sorted[j].Key
	})
	h := sha256.New()
	var buf [8]byte
	for _, e := range sorted {
		h.Write([]byte(e.Key))
		h.Write(e.Value)
		binary.BigEndian.PutUint64(buf[:], e.Timestamp)
		h.Write(buf[:])
	}
	var out hash32
	copy(out[:], h.Sum(nil))
	return out
}

func internalHash(left, right hash32) hash32 {
	h := sha256.New()
	h.Write(left[:])
	h.Write(right[:])
	var out hash32
	copy(out[:], h.Sum(nil))
	return out
}

// Flush propagates all dirty leaves to the root by recomputing the affected
// ancestor nodes in bottom-up order. It clears the dirty set.
// Cost is O(dirty_leaves * depth) hash computations.
func (t *Tree) Flush() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.dirty) == 0 {
		return
	}

	// Collect all internal ancestor node indices of dirty leaves.
	affected := make(map[int]struct{})
	for l := range t.dirty {
		idx := t.leafIdx(l)
		for idx > 0 {
			parent := (idx - 1) / 2
			affected[parent] = struct{}{}
			idx = parent
		}
	}
	t.dirty = make(map[int]struct{})

	// Recompute affected internal nodes level by level, deepest first
	// (level depth-1 down to 0), so children are up to date before their
	// parent is recomputed.
	for level := t.depth - 1; level >= 0; level-- {
		start := (1 << level) - 1
		end := (1 << (level + 1)) - 1
		for i := start; i < end; i++ {
			if _, ok := affected[i]; ok {
				t.nodes[i] = internalHash(t.nodes[2*i+1], t.nodes[2*i+2])
			}
		}
	}
}

// Root returns the current root hash. If dirty leaves exist and Flush has not
// been called, the root reflects the state at the previous Flush.
func (t *Tree) Root() hash32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodes[0]
}

// RootHex returns the root hash as a 64-character lower-case hex string.
func (t *Tree) RootHex() string {
	r := t.Root()
	return hex.EncodeToString(r[:])
}

// NodeAt returns the hash of node index i (i=0 is the root).
func (t *Tree) NodeAt(i int) hash32 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.nodes[i]
}
```

The zero value of `hash32` represents an empty leaf or an unflushed internal
node. A freshly allocated tree has all-zero nodes because `make` zero-initialises
the slice.

### Exercise 2: In-Memory Store with a Maintained Tree

Create `store.go`. `MemStore` holds key-value entries, maps each key to its
leaf segment, and keeps the Merkle tree current on every write.

```go
package merkle

import (
	"sort"
	"sync"
)

// Store is the interface the Syncer uses to exchange data between replicas.
type Store interface {
	// SegmentEntries returns a snapshot of all entries in leaf segment seg.
	SegmentEntries(seg int) []Entry
	// Put applies e using last-write-wins: if the store already holds e.Key
	// with a timestamp >= e.Timestamp, the call is a no-op.
	Put(e Entry)
	// Tree returns the current Merkle tree (after all Puts have been flushed).
	Tree() *Tree
}

// MemStore is a thread-safe in-memory Store backed by a Merkle tree.
type MemStore struct {
	mu      sync.Mutex
	tree    *Tree
	data    map[string]Entry
	segKeys map[int]map[string]struct{}
}

// NewMemStore returns a MemStore using a Merkle tree of the given depth.
func NewMemStore(depth int) *MemStore {
	return &MemStore{
		tree:    New(depth),
		data:    make(map[string]Entry),
		segKeys: make(map[int]map[string]struct{}),
	}
}

// Put applies e using last-write-wins and updates the Merkle tree leaf.
func (s *MemStore) Put(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.data[e.Key]; ok && existing.Timestamp >= e.Timestamp {
		return
	}
	s.data[e.Key] = e

	seg := s.tree.SegmentOf(e.Key)
	if s.segKeys[seg] == nil {
		s.segKeys[seg] = make(map[string]struct{})
	}
	s.segKeys[seg][e.Key] = struct{}{}

	entries := s.snapshotSegment(seg)
	s.tree.UpdateLeaf(seg, entries)
	s.tree.Flush()
}

// SegmentEntries returns a sorted snapshot of entries in leaf segment seg.
func (s *MemStore) SegmentEntries(seg int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotSegment(seg)
}

// snapshotSegment builds a sorted entry slice for seg; caller must hold s.mu.
func (s *MemStore) snapshotSegment(seg int) []Entry {
	keys := s.segKeys[seg]
	entries := make([]Entry, 0, len(keys))
	for k := range keys {
		entries = append(entries, s.data[k])
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Key < entries[j].Key
	})
	return entries
}

// Tree returns the underlying Merkle tree.
func (s *MemStore) Tree() *Tree { return s.tree }
```

`Put` calls both `UpdateLeaf` and `Flush` on every write, which is correct for
a store that needs an immediately consistent tree. A high-throughput store would
batch multiple writes and flush once before each anti-entropy round to amortise
the cost.

### Exercise 3: Anti-Entropy Sync Protocol and Tests

Create `sync.go`:

```go
package merkle

// Stats records metrics for one anti-entropy synchronisation round.
type Stats struct {
	SegmentsCompared  int
	SegmentsDiffering int
	KeysTransferred   int
	BytesTransferred  int
}

// DiffSegments compares the local tree with remote and returns the leaf segment
// indices where they disagree. Both trees must have the same depth.
//
// The comparison is top-down: subtrees whose root hashes agree are pruned
// without descending, so the number of comparisons is bounded by
// differing_segments * depth, not by total_leaves.
func (t *Tree) DiffSegments(remote *Tree, stats *Stats) []int {
	if t.depth != remote.depth {
		panic("merkle: DiffSegments requires trees of equal depth")
	}
	var diff []int
	t.mu.RLock()
	remote.mu.RLock()
	defer t.mu.RUnlock()
	defer remote.mu.RUnlock()
	diffDescend(t.nodes, remote.nodes, 0, t.leaves, &diff, stats)
	return diff
}

// diffDescend recursively compares node index i in trees a and b.
func diffDescend(a, b []hash32, i, leaves int, diff *[]int, stats *Stats) {
	if stats != nil {
		stats.SegmentsCompared++
	}
	if a[i] == b[i] {
		return // subtree agrees; prune
	}
	if i >= leaves-1 {
		// leaf node: segment index = i - (leaves - 1)
		*diff = append(*diff, i-(leaves-1))
		if stats != nil {
			stats.SegmentsDiffering++
		}
		return
	}
	diffDescend(a, b, 2*i+1, leaves, diff, stats)
	diffDescend(a, b, 2*i+2, leaves, diff, stats)
}

// Syncer copies entries from a source Store to a destination Store for each
// leaf segment where their Merkle trees disagree.
type Syncer struct{}

// Sync runs one anti-entropy round from src to dst. It updates stats with the
// number of segments compared, segments differing, keys transferred, and bytes
// transferred.
func (Syncer) Sync(dst, src Store, stats *Stats) {
	diff := dst.Tree().DiffSegments(src.Tree(), stats)
	for _, seg := range diff {
		for _, e := range src.SegmentEntries(seg) {
			stats.KeysTransferred++
			stats.BytesTransferred += len(e.Key) + len(e.Value) + 8
			dst.Put(e)
		}
	}
}
```

Create `merkle_test.go`:

```go
package merkle

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

// ExampleTree_RootHex shows that a freshly allocated tree has an all-zero root.
func ExampleTree_RootHex() {
	t := New(4)
	fmt.Println(t.RootHex())
	// Output: 0000000000000000000000000000000000000000000000000000000000000000
}

func TestNewPanicsOnBadDepth(t *testing.T) {
	t.Parallel()
	for _, d := range []int{0, 21} {
		d := d
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("depth %d: expected panic, got none", d)
				}
			}()
			New(d)
		}()
	}
}

func TestFreshTreeHasZeroRoot(t *testing.T) {
	t.Parallel()
	tree := New(4)
	var empty hash32
	if tree.Root() != empty {
		t.Fatalf("fresh tree root should be zero, got %x", tree.Root())
	}
}

func TestUpdateLeafChangesRoot(t *testing.T) {
	t.Parallel()
	tree := New(4)
	entries := []Entry{{Key: "hello", Value: []byte("world"), Timestamp: 1}}
	seg := tree.SegmentOf("hello")
	tree.UpdateLeaf(seg, entries)
	tree.Flush()

	var empty hash32
	if tree.Root() == empty {
		t.Fatal("root should change after UpdateLeaf+Flush")
	}
}

func TestSameDataSameRoot(t *testing.T) {
	t.Parallel()
	sa := NewMemStore(4)
	sb := NewMemStore(4)
	entries := []Entry{
		{Key: "k1", Value: []byte("v1"), Timestamp: 1},
		{Key: "k2", Value: []byte("v2"), Timestamp: 2},
	}
	for _, e := range entries {
		sa.Put(e)
		sb.Put(e)
	}
	if sa.Tree().Root() != sb.Tree().Root() {
		t.Fatalf("identical data must produce identical roots:\n  a=%s\n  b=%s",
			sa.Tree().RootHex(), sb.Tree().RootHex())
	}
}

func TestDiffSegmentsEmptyTrees(t *testing.T) {
	t.Parallel()
	a := New(4)
	b := New(4)
	var stats Stats
	diff := a.DiffSegments(b, &stats)
	if len(diff) != 0 {
		t.Fatalf("identical empty trees should produce no diff, got %v", diff)
	}
	if stats.SegmentsDiffering != 0 {
		t.Fatalf("SegmentsDiffering = %d, want 0", stats.SegmentsDiffering)
	}
}

func TestDiffSegmentsOneDivergentLeaf(t *testing.T) {
	t.Parallel()
	a := New(4)
	b := New(4)

	entries := []Entry{{Key: "only-in-a", Value: []byte("x"), Timestamp: 1}}
	seg := a.SegmentOf("only-in-a")
	a.UpdateLeaf(seg, entries)
	a.Flush()

	var stats Stats
	diff := a.DiffSegments(b, &stats)
	if len(diff) != 1 || diff[0] != seg {
		t.Fatalf("DiffSegments = %v, want [%d]", diff, seg)
	}
	if stats.SegmentsDiffering != 1 {
		t.Fatalf("SegmentsDiffering = %d, want 1", stats.SegmentsDiffering)
	}
}

func TestDiffSegmentsPanicsOnDepthMismatch(t *testing.T) {
	t.Parallel()
	a := New(4)
	b := New(5)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for depth mismatch")
		}
	}()
	a.DiffSegments(b, nil)
}

func TestSyncTransfersOnlyDivergentSegments(t *testing.T) {
	t.Parallel()
	src := NewMemStore(4)
	dst := NewMemStore(4)

	keys := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	for _, k := range keys {
		src.Put(Entry{Key: k, Value: []byte(k + "_val"), Timestamp: 1})
	}

	var stats Stats
	var s Syncer
	s.Sync(dst, src, &stats)

	if dst.Tree().Root() != src.Tree().Root() {
		t.Fatalf("roots differ after sync:\n  dst=%s\n  src=%s",
			dst.Tree().RootHex(), src.Tree().RootHex())
	}
	if stats.KeysTransferred != len(keys) {
		t.Fatalf("KeysTransferred = %d, want %d", stats.KeysTransferred, len(keys))
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	t.Parallel()
	src := NewMemStore(4)
	dst := NewMemStore(4)

	src.Put(Entry{Key: "k", Value: []byte("v"), Timestamp: 1})

	var s Syncer
	var stats1, stats2 Stats
	s.Sync(dst, src, &stats1)
	s.Sync(dst, src, &stats2)

	if stats2.KeysTransferred != 0 {
		t.Fatalf("second sync should transfer 0 keys, got %d", stats2.KeysTransferred)
	}
	if stats2.SegmentsDiffering != 0 {
		t.Fatalf("second sync should find 0 differing segments, got %d", stats2.SegmentsDiffering)
	}
}

func TestLastWriteWins(t *testing.T) {
	t.Parallel()
	s := NewMemStore(4)
	s.Put(Entry{Key: "k", Value: []byte("old"), Timestamp: 1})
	s.Put(Entry{Key: "k", Value: []byte("new"), Timestamp: 2})
	// An older write must not overwrite the newer one.
	s.Put(Entry{Key: "k", Value: []byte("stale"), Timestamp: 1})

	seg := s.Tree().SegmentOf("k")
	entries := s.SegmentEntries(seg)
	if len(entries) != 1 || string(entries[0].Value) != "new" {
		t.Fatalf("last-write-wins failed: got %v", entries)
	}
}

func TestLeafHashDeterminism(t *testing.T) {
	t.Parallel()
	entries := []Entry{
		{Key: "z", Value: []byte("1"), Timestamp: 10},
		{Key: "a", Value: []byte("2"), Timestamp: 20},
	}
	reversed := []Entry{entries[1], entries[0]}
	h1 := leafHash(entries)
	h2 := leafHash(reversed)
	if h1 != h2 {
		t.Fatal("leafHash must be order-independent")
	}
}

func TestInternalHashCombinesChildren(t *testing.T) {
	t.Parallel()
	left := sha256.Sum256([]byte("left"))
	right := sha256.Sum256([]byte("right"))
	var l, r hash32
	copy(l[:], left[:])
	copy(r[:], right[:])

	got := internalHash(l, r)

	h := sha256.New()
	h.Write(left[:])
	h.Write(right[:])
	var want hash32
	copy(want[:], h.Sum(nil))
	if got != want {
		t.Fatalf("internalHash mismatch: got %x, want %x", got, want)
	}
}

func TestSegmentComparisonsAreBoundedByDepth(t *testing.T) {
	t.Parallel()
	const depth = 6
	a := New(depth)
	b := New(depth)

	// One key in segment 0, present only in a.
	a.UpdateLeaf(0, []Entry{{Key: "x", Value: []byte("y"), Timestamp: 1}})
	a.Flush()

	var stats Stats
	a.DiffSegments(b, &stats)

	// One differing leaf at depth 6: the path to the leaf is depth+1 nodes;
	// each level also checks the agreeing sibling (depth siblings total).
	// Total = 2*depth+1 = 13 comparisons.
	maxComparisons := 2*depth + 1
	if stats.SegmentsCompared > maxComparisons {
		t.Fatalf("SegmentsCompared = %d for one differing leaf at depth %d, want <= %d",
			stats.SegmentsCompared, depth, maxComparisons)
	}
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/merkle"
)

func main() {
	const depth = 6 // 64 leaf segments

	src := merkle.NewMemStore(depth)
	dst := merkle.NewMemStore(depth)

	// Populate src with 10 entries.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("key-%02d", i)
		val := fmt.Sprintf("value-%02d", i)
		src.Put(merkle.Entry{
			Key:       key,
			Value:     []byte(val),
			Timestamp: uint64(i + 1),
		})
	}

	fmt.Printf("src root: %s\n", src.Tree().RootHex())
	fmt.Printf("dst root: %s\n", dst.Tree().RootHex())
	fmt.Println("roots match before sync:", src.Tree().Root() == dst.Tree().Root())

	var stats merkle.Stats
	var s merkle.Syncer
	s.Sync(dst, src, &stats)

	fmt.Printf("\n--- after sync ---\n")
	fmt.Printf("segments compared:  %d\n", stats.SegmentsCompared)
	fmt.Printf("segments differing: %d\n", stats.SegmentsDiffering)
	fmt.Printf("keys transferred:   %d\n", stats.KeysTransferred)
	fmt.Printf("bytes transferred:  %d\n", stats.BytesTransferred)
	fmt.Println("roots match after sync:", src.Tree().Root() == dst.Tree().Root())

	if src.Tree().Root() != dst.Tree().Root() {
		log.Fatal("roots differ after sync")
	}
}
```

## Common Mistakes

### Not Sorting Entries Before Hashing

Wrong: iterating directly over a map to build the entry slice for `leafHash`.
Two stores with identical data but different insertion orders produce different
leaf hashes and appear to disagree on every anti-entropy round even though their
data is identical.

Fix: sort entries by key before hashing. `leafHash` does this internally via
`sort.Slice`. If you compute a leaf hash outside of `leafHash`, you must sort
yourself.

### Forgetting to Call Flush Before Comparing Roots

Wrong: calling `UpdateLeaf` on several leaves and then calling `DiffSegments`
or `Root()` without `Flush`. The root still reflects the state at the previous
Flush; the comparison finds phantom differences or misses real ones.

Fix: call `Flush` after all `UpdateLeaf` calls and before any root comparison.
`MemStore.Put` calls both `UpdateLeaf` and `Flush` on every write. A
high-throughput store batches writes and calls `Flush` once before each
anti-entropy round.

### Acquiring Both Tree Locks in Inconsistent Order

Wrong: goroutine A holds `treeA.mu` and blocks waiting for `treeB.mu`;
goroutine B holds `treeB.mu` and blocks waiting for `treeA.mu`. Classic
deadlock.

Fix: `DiffSegments` acquires both read locks before the traversal and holds
them throughout. Callers must not hold either lock when calling `DiffSegments`.
`MemStore` never holds `tree.mu` directly; it routes through `UpdateLeaf` and
`Flush`, which manage their own locks.

### Using Different Tree Depths on Different Replicas

Wrong: replica A uses depth 15 and replica B uses depth 10. The segment-to-key
mapping differs, so a key that falls in segment 5000 on A falls in a different
segment on B. `DiffSegments` panics because the node arrays have different
lengths.

Fix: all replicas must use the same depth. Treat the depth as part of the
cluster configuration, chosen at startup and never changed.

### Recomputing Every Ancestor Immediately Per Dirty Leaf

Wrong: for each dirty leaf, immediately walk up the tree and recompute each
ancestor before processing the next leaf. When two dirty leaves share ancestors
(for example both are in the left subtree), the shared ancestors are recomputed
twice: once for the first leaf, and again for the second.

Fix: collect all affected ancestor indices into a set first, then sweep level
by level from deepest to shallowest. Each ancestor is recomputed exactly once.
`Flush` implements this pattern.

## Verification

From `~/go-exercises/merkle`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass without errors. `go test -race` verifies that the
`sync.RWMutex` boundaries in `Tree` and the `sync.Mutex` in `MemStore` prevent
data races under concurrent access.

Your turn: add `TestSyncWithPartialOverlap`. Create two `MemStore` instances at
depth 4. Give each a distinct set of 5 keys (no overlap), plus 2 keys in common
with the same timestamps. Run `Sync` from A to B, then from B to A. After both
rounds verify that A and B have equal roots and that the combined
`KeysTransferred` across both rounds is exactly 10 (the 5 unique keys from each
side). The 2 shared keys must not be transferred in either direction.

## Summary

- A Merkle tree assigns a hash fingerprint to each leaf (key range) and
  propagates changes to the root in O(depth) hash computations per write.
- Two replicas locate divergent key ranges by traversing the tree top-down;
  subtrees with matching hashes are pruned, bounding the comparison cost to
  differing_segments * depth node checks.
- Leaf hashes must be computed over entries sorted by key; any non-determinism
  in ordering causes phantom divergences that force unnecessary data transfer.
- `Flush` propagates dirty leaves to the root; call it after all `UpdateLeaf`
  calls and before comparing roots.
- The array-based binary heap layout (children at 2*i+1 and 2*i+2) avoids
  per-node allocation and enables level-by-level bottom-up recomputation.

## What's Next

Next: [Hinted Handoff](../04-hinted-handoff/04-hinted-handoff.md).

## Resources

- DeCandia et al., "Dynamo: Amazon's Highly Available Key-Value Store" (SOSP 2007), §4.7 — original Merkle tree anti-entropy design: https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf
- Apache Cassandra repair documentation: https://cassandra.apache.org/doc/latest/cassandra/managing/operating/repair.html
- Go `crypto/sha256` package: https://pkg.go.dev/crypto/sha256
- Go `sync` package (RWMutex): https://pkg.go.dev/sync#RWMutex
- Ralph Merkle, "A Digital Signature Based on a Conventional Encryption Function" (CRYPTO 1987) — the original tree construction
