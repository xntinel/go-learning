# 21. Anti-Entropy Protocol

Anti-entropy protocols are the mechanism by which eventually consistent systems heal divergence between replicas. The hard part is not detecting that replicas differ — it is doing so efficiently when each node holds millions of keys. A naive full scan would transfer the entire dataset on every repair cycle. The canonical solution, used by Dynamo and Cassandra, is a Merkle tree: build a hash tree over key ranges, exchange only the tree roots, and recurse only into subtrees whose hashes differ. This lesson implements that idea in a self-contained Go package, with both passive read-repair and a synchronous active repair primitive that serves as the building block for a production background-repair loop.

```text
antientropy/
  go.mod
  antientropy.go
  antientropy_test.go
  cmd/demo/main.go
```

The package exposes a `Store` that holds string key-value pairs, computes a Merkle tree over its contents, can compare trees with a peer to identify divergent key ranges, and repairs those ranges.

## Concepts

### Why Replicas Diverge

In a system that tolerates network partitions or node failures, a write that succeeds on the primary may never reach a replica. When the partition heals, the replica is stale. Anti-entropy is the background process that detects and corrects this staleness, independent of read or write traffic.

The two strategies are complementary:

- Read repair (passive): on every quorum read, compare values returned by each replica. If they differ, send the newest value (determined by timestamp) back to the stale replica. This is proportional to read traffic: heavily-read keys converge quickly; cold keys may stay stale for a long time.
- Active repair: a background goroutine periodically selects a peer, compares state, and pushes missing or stale keys. This covers cold keys that read repair would miss. In a real deployment this is wired to a `time.Ticker`-driven loop; this lesson implements `Repair`, the one-shot comparison primitive that such a loop calls on each tick.

### Merkle Trees For Efficient Divergence Detection

A Merkle tree is a binary tree where each leaf holds the hash of one data item (or a range of items), and each internal node holds the hash of its children. To check whether two stores agree on a key range, the nodes exchange only their tree roots (one hash). If the roots match, the stores agree on every key in that range and no further communication is needed. If they differ, the nodes descend into left and right subtrees, halving the search space at each level. In the worst case (all keys differ) this still requires O(N) comparisons, but the common case (few differences) requires only O(D log N) comparisons, where D is the number of differing key ranges.

Building the tree: partition the key space into fixed-size buckets (here, by hashing each key with SHA-256 and taking the result modulo the bucket count). Hash each bucket's contents (sorted key-value pairs). Build a binary tree over the bucket hashes. The tree depth is O(log B) where B is the number of buckets.

### Version Comparison

When two replicas disagree on a key, we need to know which value is authoritative. This lesson uses a Lamport timestamp (a monotonically increasing integer): each write increments the node's local clock, and the higher version wins. For production systems, vector clocks or hybrid logical clocks provide finer-grained causality tracking, but a Lamport timestamp is sufficient to demonstrate the protocol.

### Repair Throttling

Background repair competes with foreground reads and writes for CPU and network. Without throttling, a node that falls far behind can trigger a repair storm that degrades foreground latency. The standard production approach is a token-bucket rate limiter that caps the number of keys repaired per second, typically wired to a `time.Ticker`-driven goroutine that calls `Repair` on each tick and sleeps or drops ticks when the repair backlog is large. This lesson implements `Repair` as a synchronous one-shot primitive; integrating it into a throttled background loop is left as a production deployment concern.

### Idempotency

Anti-entropy must be safe to run repeatedly. Running repair twice on two already-consistent replicas should produce no mutations and no side effects. This is naturally guaranteed when the repair condition is "remote version > local version": if both nodes already agree on the latest version, neither side sends anything.

## Exercises

This is a library, not a program: the core package is verified with `go test`.

### Exercise 1: The Data Store With Versioned Entries

Create `antientropy.go`:

```go
package antientropy

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrKeyNotFound is returned when a Get targets a key absent from the store.
var ErrKeyNotFound = errors.New("key not found")

// entry holds a value and a Lamport timestamp.
type entry struct {
	value   string
	version uint64
}

// Store is a versioned key-value store that maintains a Merkle tree over its
// contents. All exported methods are safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	data    map[string]entry
	clock   uint64
	buckets int // number of Merkle leaf buckets
}

// New returns a Store partitioned into the given number of Merkle buckets.
// buckets must be a power of two and at least 1.
func New(buckets int) (*Store, error) {
	if buckets < 1 {
		return nil, fmt.Errorf("antientropy: %w: got %d", ErrInvalidBuckets, buckets)
	}
	// Round up to the next power of two.
	b := 1
	for b < buckets {
		b <<= 1
	}
	return &Store{
		data:    make(map[string]entry),
		buckets: b,
	}, nil
}

// ErrInvalidBuckets is returned when New receives a non-positive bucket count.
var ErrInvalidBuckets = errors.New("bucket count must be at least 1")

// Put writes key=value with a new Lamport timestamp. If an entry already exists
// with a higher version, the write is silently dropped (last-write-wins by
// version, not wall clock).
func (s *Store) Put(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock++
	if e, ok := s.data[key]; ok && e.version >= s.clock {
		return
	}
	s.data[key] = entry{value: value, version: s.clock}
}

// putVersioned writes key=value only if newVersion is strictly greater than the
// existing version. It does not advance the local clock; it is used by repair
// to apply remote values without disrupting the local Lamport sequence.
func (s *Store) putVersioned(key, value string, newVersion uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.data[key]; ok && e.version >= newVersion {
		return false
	}
	s.data[key] = entry{value: value, version: newVersion}
	return true
}

// Get returns the value for key, or ErrKeyNotFound.
func (s *Store) Get(key string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.data[key]
	if !ok {
		return "", fmt.Errorf("antientropy: %w: %s", ErrKeyNotFound, key)
	}
	return e.value, nil
}

// Keys returns all keys in sorted order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.data))
	for k := range s.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Size returns the number of keys in the store.
func (s *Store) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data)
}

// bucketIndex assigns key to a bucket by hashing it.
func (s *Store) bucketIndex(key string) int {
	h := sha256.Sum256([]byte(key))
	n := binary.BigEndian.Uint64(h[:8])
	return int(n % uint64(s.buckets))
}
```

`putVersioned` is unexported; it is used by the repair path and is tested through the package-level `Repair` function below. `bucketIndex` maps each key to a Merkle leaf deterministically.

### Exercise 2: The Merkle Tree

Append to `antientropy.go`:

```go
// MerkleTree is the hash tree built over the store's key-value contents.
// The tree has s.buckets leaves; internal nodes are the SHA-256 of their
// children's hashes concatenated.
type MerkleTree struct {
	nodes   [][]byte // level-order: index 0 is root
	depth   int
	buckets int
}

// BuildMerkleTree computes the Merkle tree for the store's current contents.
// The tree is a snapshot; it is not updated when the store changes.
func (s *Store) BuildMerkleTree() *MerkleTree {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Group keys by bucket.
	bucketKeys := make([][]string, s.buckets)
	for k := range s.data {
		b := s.bucketIndex(k)
		bucketKeys[b] = append(bucketKeys[b], k)
	}

	// Hash each bucket: SHA-256 over sorted "key\0value\0version" tuples.
	leaves := make([][]byte, s.buckets)
	for i, keys := range bucketKeys {
		sort.Strings(keys)
		h := sha256.New()
		for _, k := range keys {
			e := s.data[k]
			fmt.Fprintf(h, "%s\x00%s\x00%d\x00", k, e.value, e.version)
		}
		leaves[i] = h.Sum(nil)
	}

	// Build the binary tree bottom-up.
	// Total nodes in a complete binary tree with b leaves: 2*b - 1.
	total := 2*s.buckets - 1
	nodes := make([][]byte, total)

	// Leaves occupy indices [total-s.buckets, total).
	leafStart := total - s.buckets
	copy(nodes[leafStart:], leaves)

	// Internal nodes: parent i has children 2i+1 and 2i+2.
	for i := leafStart - 1; i >= 0; i-- {
		l, r := nodes[2*i+1], nodes[2*i+2]
		combined := append(l, r...) //nolint:gocritic
		h := sha256.Sum256(combined)
		nodes[i] = h[:]
	}

	depth := 0
	for n := s.buckets; n > 1; n >>= 1 {
		depth++
	}

	return &MerkleTree{nodes: nodes, depth: depth, buckets: s.buckets}
}

// Root returns the root hash of the Merkle tree.
func (t *MerkleTree) Root() []byte {
	if len(t.nodes) == 0 {
		return nil
	}
	return t.nodes[0]
}

// DifferingBuckets compares two trees and returns the indices of leaf buckets
// whose hashes differ. It uses the tree structure to skip identical subtrees.
func DifferingBuckets(a, b *MerkleTree) []int {
	if a.buckets != b.buckets {
		// Cannot compare trees of different shape.
		var all []int
		for i := range a.buckets {
			all = append(all, i)
		}
		return all
	}
	var diff []int
	diffBucketsRecurse(a, b, 0, 0, a.buckets, &diff)
	return diff
}

// diffBucketsRecurse descends the tree, skipping identical subtrees.
// nodeIdx is the current node index (level-order). lo..hi is the bucket range
// covered by this subtree.
func diffBucketsRecurse(a, b *MerkleTree, nodeIdx, lo, hi int, diff *[]int) {
	if nodeIdx >= len(a.nodes) || nodeIdx >= len(b.nodes) {
		return
	}
	ah, bh := a.nodes[nodeIdx], b.nodes[nodeIdx]
	if string(ah) == string(bh) {
		return // subtree is identical; skip
	}
	if hi-lo == 1 {
		// This is a leaf: the bucket at index lo differs.
		*diff = append(*diff, lo)
		return
	}
	mid := (lo + hi) / 2
	left := 2*nodeIdx + 1
	right := 2*nodeIdx + 2
	diffBucketsRecurse(a, b, left, lo, mid, diff)
	diffBucketsRecurse(a, b, right, mid, hi, diff)
}
```

The tree comparison recurses only into subtrees whose hashes differ, which is the key efficiency gain over a full key scan.

### Exercise 3: Read Repair And Active Repair

Append to `antientropy.go`:

```go
// RemoteEntry is a key, value, and version exported by a peer for repair.
type RemoteEntry struct {
	Key     string
	Value   string
	Version uint64
}

// EntriesInBucket returns all entries in the given bucket index for use by a
// repair peer. Exported so cmd/demo can inspect state.
func (s *Store) EntriesInBucket(bucket int) []RemoteEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []RemoteEntry
	for k, e := range s.data {
		if s.bucketIndex(k) == bucket {
			out = append(out, RemoteEntry{Key: k, Value: e.value, Version: e.version})
		}
	}
	return out
}

// ReadRepair compares a local value with a value returned by a peer for the
// same key. If the peer's version is higher, the local store is updated.
// If the local version is higher, the returned RemoteEntry describes what the
// peer should receive (the caller is responsible for sending it back).
// Returns (updated, localEntry) where updated is true if the local store
// was changed.
func (s *Store) ReadRepair(key, peerValue string, peerVersion uint64) (updated bool, authoritative RemoteEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	local, ok := s.data[key]
	if !ok || peerVersion > local.version {
		s.data[key] = entry{value: peerValue, version: peerVersion}
		return true, RemoteEntry{}
	}
	// Local is at least as recent; return it so the caller can push it to the peer.
	return false, RemoteEntry{Key: key, Value: local.value, Version: local.version}
}

// Repair performs active anti-entropy between s (local) and peer.
// It compares Merkle trees to find differing buckets, then for each differing
// bucket exchanges entries and applies the ones with higher versions.
// Returns the number of keys updated on the local side and the number of
// RemoteEntry values the caller should send back to the peer.
func Repair(local, peer *Store) (localUpdates int, peerUpdates []RemoteEntry) {
	la := local.BuildMerkleTree()
	pb := peer.BuildMerkleTree()

	diffBuckets := DifferingBuckets(la, pb)
	for _, bucket := range diffBuckets {
		localEntries := local.EntriesInBucket(bucket)
		peerEntries := peer.EntriesInBucket(bucket)

		// Index local entries.
		localMap := make(map[string]RemoteEntry, len(localEntries))
		for _, e := range localEntries {
			localMap[e.Key] = e
		}

		// Apply peer entries that are newer.
		for _, pe := range peerEntries {
			if le, ok := localMap[pe.Key]; !ok || pe.Version > le.Version {
				if local.putVersioned(pe.Key, pe.Value, pe.Version) {
					localUpdates++
				}
			}
		}

		// Collect local entries that are newer than the peer's.
		peerMap := make(map[string]RemoteEntry, len(peerEntries))
		for _, e := range peerEntries {
			peerMap[e.Key] = e
		}
		for _, le := range localEntries {
			if pe, ok := peerMap[le.Key]; !ok || le.Version > pe.Version {
				peerUpdates = append(peerUpdates, le)
			}
		}
	}
	return localUpdates, peerUpdates
}
```

`Repair` is symmetric: it updates the local store and returns a list of entries for the caller to push to the peer. In a real system the peer-update push would be a network call; here the test applies it directly.

### Exercise 4: Test The Contract

Create `antientropy_test.go`:

```go
package antientropy

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewRejectsBadBuckets(t *testing.T) {
	t.Parallel()

	_, err := New(0)
	if !errors.Is(err, ErrInvalidBuckets) {
		t.Fatalf("New(0) err = %v, want ErrInvalidBuckets", err)
	}
}

func TestGetMissingKey(t *testing.T) {
	t.Parallel()

	s, _ := New(4)
	_, err := s.Get("missing")
	if !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("Get missing err = %v, want ErrKeyNotFound", err)
	}
}

func TestPutAndGet(t *testing.T) {
	t.Parallel()

	s, _ := New(4)
	s.Put("k", "v")
	got, err := s.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "v" {
		t.Fatalf("Get = %q, want %q", got, "v")
	}
}

func TestMerkleTreeRootChangesOnWrite(t *testing.T) {
	t.Parallel()

	s, _ := New(4)
	before := s.BuildMerkleTree()
	s.Put("key1", "val1")
	after := s.BuildMerkleTree()

	if string(before.Root()) == string(after.Root()) {
		t.Fatal("Merkle root should change after a write")
	}
}

func TestDifferingBucketsEmptyStores(t *testing.T) {
	t.Parallel()

	a, _ := New(4)
	b, _ := New(4)
	ta := a.BuildMerkleTree()
	tb := b.BuildMerkleTree()

	if diff := DifferingBuckets(ta, tb); len(diff) != 0 {
		t.Fatalf("two empty stores should have no differing buckets; got %v", diff)
	}
}

func TestDifferingBucketsDetectsChange(t *testing.T) {
	t.Parallel()

	a, _ := New(4)
	b, _ := New(4)

	a.Put("only-in-a", "value")

	ta := a.BuildMerkleTree()
	tb := b.BuildMerkleTree()

	diff := DifferingBuckets(ta, tb)
	if len(diff) == 0 {
		t.Fatal("expected at least one differing bucket")
	}
}

func TestRepairConvergesReplicas(t *testing.T) {
	t.Parallel()

	local, _ := New(8)
	peer, _ := New(8)

	// local has keys peer is missing
	local.Put("a", "1")
	local.Put("b", "2")

	// peer has keys local is missing
	peer.Put("c", "3")
	peer.Put("d", "4")

	localUpdates, peerUpdates := Repair(local, peer)

	// Apply peerUpdates back to the peer (simulating the network push).
	for _, e := range peerUpdates {
		peer.putVersioned(e.Key, e.Value, e.Version)
	}

	if localUpdates == 0 && len(peerUpdates) == 0 {
		t.Fatal("Repair should have found differences")
	}

	// After repair, both stores should agree on all four keys.
	for _, k := range []string{"a", "b", "c", "d"} {
		lv, lerr := local.Get(k)
		pv, perr := peer.Get(k)
		if lerr != nil || perr != nil {
			t.Errorf("key %q: local err=%v peer err=%v", k, lerr, perr)
			continue
		}
		if lv != pv {
			t.Errorf("key %q: local=%q peer=%q (should agree)", k, lv, pv)
		}
	}
}

func TestRepairIsIdempotent(t *testing.T) {
	t.Parallel()

	local, _ := New(4)
	peer, _ := New(4)

	local.Put("x", "hello")
	// Fully sync peer.
	_, pu := Repair(local, peer)
	for _, e := range pu {
		peer.putVersioned(e.Key, e.Value, e.Version)
	}

	// Second repair should find no differences.
	lu2, pu2 := Repair(local, peer)
	if lu2 != 0 || len(pu2) != 0 {
		t.Fatalf("second Repair should be a no-op; got localUpdates=%d peerUpdates=%d", lu2, len(pu2))
	}
}

func TestReadRepairAppliesNewerPeerValue(t *testing.T) {
	t.Parallel()

	s, _ := New(4)
	s.Put("k", "old")

	// Simulate a peer that has a newer version.
	updated, _ := s.ReadRepair("k", "new", 999)
	if !updated {
		t.Fatal("ReadRepair should update local store when peer version is higher")
	}
	got, _ := s.Get("k")
	if got != "new" {
		t.Fatalf("Get after ReadRepair = %q, want %q", got, "new")
	}
}

func TestReadRepairReturnsLocalWhenNewer(t *testing.T) {
	t.Parallel()

	s, _ := New(4)
	s.Put("k", "current")

	updated, authoritative := s.ReadRepair("k", "stale", 1)
	if updated {
		t.Fatal("ReadRepair should not update store when local is newer")
	}
	if authoritative.Value != "current" {
		t.Fatalf("authoritative value = %q, want %q", authoritative.Value, "current")
	}
}

func TestRepairHigherVersionWins(t *testing.T) {
	t.Parallel()

	local, _ := New(4)
	peer, _ := New(4)

	// Both have the key but with different values and versions.
	local.putVersioned("shared", "local-value", 5)
	peer.putVersioned("shared", "peer-value", 10)

	lu, _ := Repair(local, peer)
	if lu != 1 {
		t.Fatalf("local should have been updated once; got %d", lu)
	}
	got, _ := local.Get("shared")
	if got != "peer-value" {
		t.Fatalf("after repair local[shared] = %q, want %q", got, "peer-value")
	}
}

func TestPutVersionedDropsOlderVersion(t *testing.T) {
	t.Parallel()

	s, _ := New(4)
	s.putVersioned("k", "new", 10)
	applied := s.putVersioned("k", "old", 5)
	if applied {
		t.Fatal("putVersioned should reject an older version")
	}
	got, _ := s.Get("k")
	if got != "new" {
		t.Fatalf("Get = %q, want %q", got, "new")
	}
}

func ExampleRepair() {
	local, _ := New(4)
	peer, _ := New(4)

	local.Put("greeting", "hello")

	_, peerUpdates := Repair(local, peer)
	for _, e := range peerUpdates {
		peer.putVersioned(e.Key, e.Value, e.Version)
	}

	v, _ := peer.Get("greeting")
	fmt.Println(v)
	// Output: hello
}
```

Your turn: add `TestStoreSize` that creates a store, puts three distinct keys, and asserts `s.Size() == 3`. Then add a fourth Put to a key that already exists and confirm `s.Size()` is still 3.

### Exercise 5: The Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/antientropy"
)

func main() {
	local, err := antientropy.New(8)
	if err != nil {
		log.Fatal(err)
	}
	peer, err := antientropy.New(8)
	if err != nil {
		log.Fatal(err)
	}

	// Write some keys to local only, simulating missed writes on the peer.
	for _, kv := range [][2]string{
		{"alpha", "1"},
		{"beta", "2"},
		{"gamma", "3"},
	} {
		local.Put(kv[0], kv[1])
	}

	// Write a different key to peer only.
	peer.Put("delta", "4")

	fmt.Printf("before repair: local=%d keys, peer=%d keys\n", local.Size(), peer.Size())

	la := local.BuildMerkleTree()
	pa := peer.BuildMerkleTree()
	diff := antientropy.DifferingBuckets(la, pa)
	fmt.Printf("differing buckets: %d\n", len(diff))

	localUpdates, peerUpdates := antientropy.Repair(local, peer)

	// In a real system, peerUpdates would be sent over the network to the peer.
	// For this demo we print the count; the peer's update path is exercised in
	// the test suite via putVersioned.
	fmt.Printf("local updates applied: %d\n", localUpdates)
	fmt.Printf("entries to push to peer: %d\n", len(peerUpdates))

	fmt.Printf("after repair: local=%d keys\n", local.Size())

	v, err := local.Get("delta")
	if err != nil {
		log.Fatalf("local missing delta after repair: %v", err)
	}
	fmt.Printf("local[delta]=%s\n", v)
}
```

## Common Mistakes

### Hashing Without Sorting Keys First

Wrong: iterating `map[string]entry` in Go's non-deterministic order before hashing. Two nodes with identical data produce different hashes, so the tree always shows differences.

Fix: sort the keys in each bucket before feeding them to the hash. The `BuildMerkleTree` implementation above sorts with `sort.Strings(keys)` before calling `fmt.Fprintf`.

### Advancing The Clock During Repair

Wrong: calling `Put` to apply a remote entry during repair. `Put` increments the Lamport clock, which may overwrite the remote version number with a locally-generated one. On the next repair cycle, the local clock-derived version may be higher than the authoritative remote version, permanently hiding the correct value.

Fix: use a separate unexported `putVersioned` that applies the remote version number without touching the local clock. Repair must preserve provenance.

### Comparing Trees Of Different Bucket Counts

Wrong: building a tree with 4 buckets on one node and 16 on another, then calling `DifferingBuckets`. The level-order indexing does not align, producing incorrect diff results.

Fix: all nodes in a cluster must use the same bucket count, agreed upon at cluster configuration time. `DifferingBuckets` above falls back to returning all buckets when the counts differ.

### Not Applying Both Sides Of The Repair

Wrong: calling `Repair(local, peer)` but only applying `localUpdates` and discarding `peerUpdates`. The peer never learns about keys local has and peer is missing.

Fix: apply `peerUpdates` to the peer after the call. In a networked system this is a push RPC. In the test suite the return value is applied directly.

### Using Wall Clock Instead Of Lamport Timestamp

Wrong: using `time.Now().UnixNano()` as the version. Clock skew between nodes means a node whose wall clock runs ahead can win a conflict even when it holds the staler value.

Fix: use a Lamport timestamp or a vector clock. Lamport timestamps are monotonically increasing per node and are compared by value, not by correspondence with real time.

## Verification

From `~/go-exercises/antientropy`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must produce no output (or, for `go test`, only the pass line). The `ExampleRepair` function is verified automatically by `go test` against its `// Output:` comment.

## Summary

- Anti-entropy protocols repair divergence between replicas without requiring a full data transfer on every cycle.
- A Merkle tree over key ranges reduces the comparison cost from O(N) to O(D log N) where D is the number of differing buckets.
- Read repair (passive) fixes stale values proportional to read traffic; active repair covers cold keys on a background schedule.
- Lamport timestamps provide a tie-breaking rule without wall-clock synchronization; the higher version wins.
- Repair must preserve version provenance: applying remote entries must not overwrite their version numbers with locally-generated ones.
- Repair idempotency (running repair twice on already-consistent stores produces no mutations) is a design invariant, not an accident.

## What's Next

Next: [Failure Detector: Phi Accrual](../22-failure-detector-phi-accrual/22-failure-detector-phi-accrual.md).

## Resources

- [Amazon Dynamo (SOSP 2007), Section 4.7: Merkle Trees](https://www.allthingsdistributed.com/files/amazon-dynamo-sosp2007.pdf) -- the original production description of Merkle-tree anti-entropy
- [crypto/sha256 package](https://pkg.go.dev/crypto/sha256) -- SHA-256 hash function used for Merkle node hashes
- [sync package](https://pkg.go.dev/sync) -- RWMutex for concurrent store access
- [Epidemic Algorithms for Replicated Database Maintenance (Demers et al., 1987)](https://dl.acm.org/doi/10.1145/41840.41841) -- foundational anti-entropy and gossip paper
- [Apache Cassandra repair documentation](https://cassandra.apache.org/doc/latest/cassandra/operating/repair.html) -- production Merkle-tree repair implementation
