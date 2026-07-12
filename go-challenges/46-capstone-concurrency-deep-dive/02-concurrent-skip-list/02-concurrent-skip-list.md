# 2. Concurrent Skip List

A skip list layers a probabilistic tower structure over a sorted linked list to achieve O(log n) expected time for search, insert, and delete. This lesson builds a generic concurrent variant that separates reads from writes: reads are lock-free, using only `atomic.Pointer[T]` loads; writes use fine-grained per-node locking with optimistic validation and lazy deletion. Unlike `sync.Map`, the result supports ordered key-value storage and range scans — properties no hash map can provide at any concurrency level.

```text
skiplist/
  go.mod
  skiplist.go
  skiplist_test.go
  cmd/demo/main.go
```

## Concepts

### Tower Structure and Probabilistic Heights

A skip list with n elements maintains up to L levels of linked lists. Level 0 is the base, containing every element in sorted order. Each successive level contains roughly 1/p of the nodes from the level below, acting as an express lane. With p = 1/4 (the value used here), the expected number of levels is O(log₄ n), which gives O(log n) expected traversal cost — the same asymptotic bound as a balanced tree, without the rebalancing.

Node height is chosen at insertion by Bernoulli trials:

```go
func (sl *SkipList[K, V]) randomLevel() int {
	level := 1
	sl.mu.Lock()
	for level < maxLevel && sl.rng.Int32N(invProb) == 0 {
		level++
	}
	sl.mu.Unlock()
	return level
}
```

`math/rand/v2.Rand` is not goroutine-safe; a single mutex guards the shared source. An alternative is a per-goroutine source stored in a `sync.Pool`, but that complicates `New` and is only worth the extra complexity at very high insertion rates.

### Lock-Free Reads with atomic.Pointer

Go 1.19 introduced `atomic.Pointer[T]`, a typed wrapper over the underlying unsafe pointer atomics. All forward pointers in the tower use it:

```go
type node[K cmp.Ordered, V any] struct {
	key      K
	val      atomic.Pointer[V]
	marked   atomic.Bool
	mu       sync.Mutex
	topLevel int
	next     [maxLevel]atomic.Pointer[node[K, V]]
}
```

`Search` loads each pointer with `.Load()`. This issues a sequentially consistent load. No mutex is acquired. The property that makes this safe is that `Insert` sets the new node's own forward pointers before publishing the node by storing it into a predecessor's `next` field. Go's memory model (go.dev/ref/mem) guarantees that any goroutine that loads the predecessor's pointer and sees the new node will also see the new node's already-written forward pointers — the `atomic.Store` on the predecessor's `next` synchronizes-with the corresponding `atomic.Load`.

### Fine-Grained Locking and Optimistic Validation

`Insert` and `Delete` use a two-phase protocol:

1. Optimistic traversal: call `findPreds` to locate predecessor and successor nodes at each level, without holding any lock.
2. Validation under lock: acquire the relevant predecessor nodes' mutexes, then re-check that each predecessor is not marked and still points to the expected successor.

If validation fails — because a concurrent insert changed a predecessor, or a predecessor was concurrently deleted — all acquired locks are released and the operation retries from the traversal step.

Deadlock prevention requires that the same node is never locked twice. The head sentinel appears as a predecessor at every level, and `sync.Mutex` in Go is not reentrant. `acquireLocks` deduplicates the predecessor set before acquiring any lock.

### Lazy Deletion

`Delete` proceeds in two phases:

1. Logical deletion: call `marked.CompareAndSwap(false, true)`. Exactly one goroutine among all concurrent deleters wins this CAS. The winner "owns" the deletion; all others return `false`.
2. Physical unlinking: for each level from `topLevel` down to 0, find the predecessor at that level, lock it, validate it still points to the victim, and update its pointer to skip the victim. If the predecessor was concurrently deleted or moved, retry.

A logically deleted node is invisible to `Search` and `Range` immediately after the CAS, even before the physical unlinking completes. This is the "lazy" property: readers do not need locks, and physical cleanup is deferred.

### Ordered Constraint via cmp.Ordered

`cmp.Ordered` (introduced in Go 1.21) is a type constraint satisfied by all integer, float, and string types. Pairing it with `cmp.Compare` gives a three-way comparison that handles the full `cmp.Ordered` type set:

```go
type SkipList[K cmp.Ordered, V any] struct { ... }
```

This eliminates a comparison callback while keeping the implementation generic over any ordered key type.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/46-capstone-concurrency-deep-dive/02-concurrent-skip-list/02-concurrent-skip-list/cmd/demo
cd go-solutions/46-capstone-concurrency-deep-dive/02-concurrent-skip-list/02-concurrent-skip-list
```

This is a library. Verification uses `go test`, not `go run`.

### Exercise 1: Node Type and Skip List Structure

Create `skiplist.go`. This file holds all types, constants, and unexported helpers:

```go
// Package skiplist provides a concurrent ordered map backed by a probabilistic
// skip list. Reads are lock-free via sync/atomic. Writes use fine-grained
// per-node locking with optimistic validation and lazy deletion.
package skiplist

import (
	"cmp"
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

const (
	maxLevel = 32
	invProb  = 4 // each additional level is included with probability 1/invProb
)

// node is one element in the tower structure.
type node[K cmp.Ordered, V any] struct {
	key      K
	val      atomic.Pointer[V] // value; replaced atomically on duplicate insert
	marked   atomic.Bool       // true = logically deleted
	mu       sync.Mutex        // serialises predecessor-pointer updates for this node
	topLevel int               // highest level index used by this node (0-based)
	next     [maxLevel]atomic.Pointer[node[K, V]]
}

// SkipList is a concurrent ordered map.
//
// Search is lock-free: it reads only atomic pointers.
// Insert and Delete acquire fine-grained per-node locks with optimistic
// validation and retry on conflict.
// Delete uses lazy deletion: the node is first marked then physically unlinked.
type SkipList[K cmp.Ordered, V any] struct {
	head   *node[K, V] // sentinel; key is zero value of K; never compared
	length atomic.Int64
	height atomic.Int32 // current maximum tower height in use
	mu     sync.Mutex   // guards rng
	rng    *rand.Rand
}

// New returns an empty, ready-to-use SkipList.
func New[K cmp.Ordered, V any]() *SkipList[K, V] {
	sl := &SkipList[K, V]{
		head: &node[K, V]{topLevel: maxLevel - 1},
		rng:  rand.New(rand.NewPCG(0, 0)),
	}
	sl.height.Store(1)
	return sl
}

// Len returns the approximate number of elements.
// The count may lag by up to one under concurrent inserts or deletes.
func (sl *SkipList[K, V]) Len() int64 { return sl.length.Load() }

// Height returns the current maximum tower height in use (at least 1).
func (sl *SkipList[K, V]) Height() int { return int(sl.height.Load()) }

// randomLevel returns a new random tower height.
// Each successive level is included with probability 1/invProb.
func (sl *SkipList[K, V]) randomLevel() int {
	level := 1
	sl.mu.Lock()
	for level < maxLevel && sl.rng.Int32N(invProb) == 0 {
		level++
	}
	sl.mu.Unlock()
	return level
}
```

The head sentinel's key is the zero value of K and is never accessed during comparisons. The sentinel always acts as the predecessor at every level for the first real element. Setting `head.topLevel = maxLevel - 1` ensures `findPreds` always has a valid starting predecessor at every level.

### Exercise 2: findPreds and Lock-Free Search

Add `findPreds` and `Search` to `skiplist.go`:

```go
// findPreds traverses the list from the top level down to 0.
// After the call:
//
//	preds[i].key < target  (or preds[i] is the head sentinel)
//	succs[i] = preds[i].next[i]   (first node at level i with key >= target, or nil)
//
// found is true when succs[0].key == target and succs[0] is not marked.
func (sl *SkipList[K, V]) findPreds(target K) (preds, succs [maxLevel]*node[K, V], found bool) {
	pred := sl.head
	for lv := maxLevel - 1; lv >= 0; lv-- {
		curr := pred.next[lv].Load()
		for curr != nil && cmp.Compare(curr.key, target) < 0 {
			pred = curr
			curr = pred.next[lv].Load()
		}
		preds[lv] = pred
		succs[lv] = curr
	}
	s := succs[0]
	found = s != nil && cmp.Compare(s.key, target) == 0 && !s.marked.Load()
	return
}

// Search returns the value for key, or (zero, false) if not found.
// Search is lock-free: it issues only atomic loads, no mutex acquisitions.
func (sl *SkipList[K, V]) Search(key K) (V, bool) {
	pred := sl.head
	var zero V
	for lv := maxLevel - 1; lv >= 0; lv-- {
		curr := pred.next[lv].Load()
		for curr != nil && cmp.Compare(curr.key, key) < 0 {
			pred = curr
			curr = pred.next[lv].Load()
		}
		if curr != nil && cmp.Compare(curr.key, key) == 0 {
			if curr.marked.Load() {
				return zero, false
			}
			vp := curr.val.Load()
			if vp == nil {
				return zero, false
			}
			return *vp, true
		}
	}
	return zero, false
}
```

`findPreds` does not skip over logically deleted nodes. That is deliberate: the function is also used by `Insert` and `Delete`, which validate under lock whether the predecessor still points to the expected successor. Stale predecessor information is detected during validation and triggers a retry.

`Search` is a separate lock-free traversal. It checks `curr.marked.Load()` explicitly: a node that has been logically deleted but not yet physically unlinked is not returned.

### Exercise 3: Insert with Optimistic Validation

Add `Insert` and the locking helpers to `skiplist.go`:

```go
// Insert inserts key with value. If key already exists and is not deleted,
// the stored value is replaced atomically without acquiring any lock.
func (sl *SkipList[K, V]) Insert(key K, value V) {
	topLevel := sl.randomLevel()
	for {
		preds, succs, found := sl.findPreds(key)
		if found {
			// Existing live node: update value without a lock.
			succs[0].val.Store(&value)
			return
		}

		// Build the new node and pre-set its forward pointers before
		// publishing: any Search that sees the node will also see these.
		n := &node[K, V]{key: key, topLevel: topLevel - 1}
		n.val.Store(&value)
		for lv := 0; lv < topLevel; lv++ {
			n.next[lv].Store(succs[lv])
		}

		// Acquire locks on the unique set of predecessors and validate.
		locked, valid := acquireLocks[K, V](preds[:topLevel], succs[:topLevel])
		if !valid {
			releaseLocks[K, V](locked)
			continue // retry from findPreds
		}

		// Link n into each level; predecessors already hold their locks.
		for lv := 0; lv < topLevel; lv++ {
			preds[lv].next[lv].Store(n)
		}
		releaseLocks[K, V](locked)

		sl.length.Add(1)
		// Raise height if topLevel exceeds the current recorded maximum.
		for {
			h := sl.height.Load()
			if int32(topLevel) <= h || sl.height.CompareAndSwap(h, int32(topLevel)) {
				break
			}
		}
		return
	}
}

// acquireLocks locks the unique nodes in preds and validates that:
//   - no pred is marked, and
//   - preds[i].next[i] == succs[i].
//
// It returns the set of locked nodes and whether validation passed.
// The caller must call releaseLocks regardless of the result.
func acquireLocks[K cmp.Ordered, V any](preds, succs []*node[K, V]) ([]*node[K, V], bool) {
	seen := make(map[*node[K, V]]struct{}, len(preds))
	locked := make([]*node[K, V], 0, len(preds))
	for i, pred := range preds {
		if _, ok := seen[pred]; !ok {
			pred.mu.Lock()
			seen[pred] = struct{}{}
			locked = append(locked, pred)
		}
		if pred.marked.Load() || pred.next[i].Load() != succs[i] {
			return locked, false
		}
	}
	return locked, true
}

// releaseLocks unlocks every node in locked.
func releaseLocks[K cmp.Ordered, V any](locked []*node[K, V]) {
	for _, n := range locked {
		n.mu.Unlock()
	}
}
```

The key ordering guarantee: `n.next[lv]` are stored before the node is linked into any predecessor. When a concurrent `Search` loads a predecessor's pointer and observes the new node, the happens-before relationship of the `atomic.Store` / `atomic.Load` pair guarantees it also sees the node's already-written forward pointers.

The value update path (`found == true`) uses `val.Store` without any lock. This is safe because `val` is an `atomic.Pointer[V]`: concurrent loads in `Search` will see either the old or new value, but never a partial write.

### Exercise 4: Lazy Delete and Range Scan

Add `Delete` and `Range` to `skiplist.go`:

```go
// Delete removes key. Returns true if key was present and has been logically
// deleted; false if it was absent or already being deleted by another goroutine.
// Physical unlinking completes before Delete returns.
func (sl *SkipList[K, V]) Delete(key K) bool {
	_, succs, found := sl.findPreds(key)
	if !found {
		return false
	}
	victim := succs[0]

	// Logical deletion: exactly one goroutine wins the CAS.
	if !victim.marked.CompareAndSwap(false, true) {
		return false
	}
	sl.length.Add(-1)

	// Physical unlinking: walk each level from the victim's top down to 0.
	for lv := victim.topLevel; lv >= 0; lv-- {
		for {
			// Find the predecessor at this level with key < victim.key.
			pred := sl.head
			curr := pred.next[lv].Load()
			for curr != nil && curr != victim && cmp.Compare(curr.key, victim.key) < 0 {
				pred = curr
				curr = pred.next[lv].Load()
			}
			if curr != victim {
				break // already unlinked at this level by another goroutine
			}
			pred.mu.Lock()
			if pred.marked.Load() || pred.next[lv].Load() != victim {
				// pred itself was deleted or has already moved past victim; retry.
				pred.mu.Unlock()
				continue
			}
			pred.next[lv].Store(victim.next[lv].Load())
			pred.mu.Unlock()
			break
		}
	}
	return true
}

// Range calls fn(key, value) for every key in [start, end] in ascending order.
// It uses the upper levels to fast-forward to start, then scans level 0.
// Logically deleted nodes (marked) are skipped. Insertions that occur ahead of
// the current scan position may or may not be observed.
func (sl *SkipList[K, V]) Range(start, end K, fn func(K, V) bool) {
	// Use upper levels to skip quickly to the start boundary.
	pred := sl.head
	for lv := maxLevel - 1; lv >= 0; lv-- {
		curr := pred.next[lv].Load()
		for curr != nil && cmp.Compare(curr.key, start) < 0 {
			pred = curr
			curr = pred.next[lv].Load()
		}
	}
	// Level-0 scan.
	curr := pred.next[0].Load()
	for curr != nil && cmp.Compare(curr.key, end) <= 0 {
		if !curr.marked.Load() {
			vp := curr.val.Load()
			if vp != nil {
				if !fn(curr.key, *vp) {
					return
				}
			}
		}
		curr = curr.next[0].Load()
	}
}
```

`Range` is a single forward traversal at level 0 with an upper-level fast-forward to the start boundary. It is not a snapshot: a concurrent insert that places a key before the iterator's current position will be missed. A concurrent delete that marks a node the iterator is about to visit causes the iterator to skip that node. Both are documented, consistent behaviors.

The fast-forward via upper levels uses the same atomic loads as `Search`. No locks are acquired anywhere in `Range`.

### Exercise 5: Test the Contract

Create `skiplist_test.go` as an external test package (`package skiplist_test`):

```go
package skiplist_test

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"example.com/skiplist"
)

func TestSearchMiss(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[string, int]()
	if _, ok := sl.Search("absent"); ok {
		t.Fatal("Search on empty list should return false")
	}
}

func TestInsertAndSearch(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[string, int]()
	sl.Insert("alpha", 1)
	sl.Insert("beta", 2)
	sl.Insert("gamma", 3)

	cases := []struct {
		key  string
		want int
	}{
		{"alpha", 1},
		{"beta", 2},
		{"gamma", 3},
	}
	for _, tc := range cases {
		got, ok := sl.Search(tc.key)
		if !ok || got != tc.want {
			t.Errorf("Search(%q) = (%d, %v), want (%d, true)", tc.key, got, ok, tc.want)
		}
	}
}

func TestInsertDuplicateUpdatesValue(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, string]()
	sl.Insert(1, "first")
	sl.Insert(1, "second")

	got, ok := sl.Search(1)
	if !ok || got != "second" {
		t.Fatalf("Search(1) = (%q, %v), want (\"second\", true)", got, ok)
	}
	if sl.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 after duplicate insert", sl.Len())
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, int]()
	for i := 0; i < 10; i++ {
		sl.Insert(i, i*10)
	}

	if !sl.Delete(5) {
		t.Fatal("Delete(5) should return true")
	}
	if _, ok := sl.Search(5); ok {
		t.Fatal("Search(5) should return false after deletion")
	}
	if sl.Len() != 9 {
		t.Fatalf("Len() = %d, want 9 after one deletion", sl.Len())
	}
}

func TestDeleteAbsentReturnsFalse(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, int]()
	sl.Insert(1, 10)
	if sl.Delete(99) {
		t.Fatal("Delete of absent key should return false")
	}
}

func TestDeleteIdempotent(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, int]()
	sl.Insert(42, 99)
	if !sl.Delete(42) {
		t.Fatal("first Delete should return true")
	}
	if sl.Delete(42) {
		t.Fatal("second Delete should return false")
	}
}

func TestRangeReturnsSortedSubset(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, int]()
	for _, k := range []int{10, 20, 30, 40, 50} {
		sl.Insert(k, k)
	}

	var got []int
	sl.Range(15, 45, func(k, v int) bool {
		got = append(got, k)
		return true
	})
	want := []int{20, 30, 40}
	if len(got) != len(want) {
		t.Fatalf("Range result = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Range[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestRangeExcludesDeletedNodes(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, int]()
	for _, k := range []int{1, 2, 3, 4, 5} {
		sl.Insert(k, k*10)
	}
	sl.Delete(3)

	var got []int
	sl.Range(1, 5, func(k, _ int) bool {
		got = append(got, k)
		return true
	})
	for _, k := range got {
		if k == 3 {
			t.Fatal("deleted key 3 must not appear in Range")
		}
	}
}

func TestRangeEarlyStop(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, int]()
	for i := 0; i < 10; i++ {
		sl.Insert(i, i)
	}
	count := 0
	sl.Range(0, 9, func(k, v int) bool {
		count++
		return count < 3
	})
	if count != 3 {
		t.Fatalf("Range stopped after %d calls, want 3", count)
	}
}

func TestLenTracksInsertDelete(t *testing.T) {
	t.Parallel()
	sl := skiplist.New[int, int]()
	if sl.Len() != 0 {
		t.Fatalf("Len() = %d, want 0", sl.Len())
	}
	sl.Insert(1, 10)
	sl.Insert(2, 20)
	if sl.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", sl.Len())
	}
	sl.Delete(1)
	if sl.Len() != 1 {
		t.Fatalf("Len() = %d, want 1 after delete", sl.Len())
	}
}

// TestConcurrentInsertSearch tests that 64 goroutines inserting and reading
// concurrently do not trigger the race detector.
func TestConcurrentInsertSearch(t *testing.T) {
	t.Parallel()

	const goroutines = 64
	const keysPerGoroutine = 500

	sl := skiplist.New[int, int]()
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			base := g * keysPerGoroutine
			for i := 0; i < keysPerGoroutine; i++ {
				sl.Insert(base+i, base+i)
			}
			for i := 0; i < keysPerGoroutine; i++ {
				sl.Search(base + i)
			}
		}()
	}
	wg.Wait()

	total := int64(goroutines * keysPerGoroutine)
	if got := sl.Len(); got != total {
		t.Fatalf("Len() = %d, want %d", got, total)
	}
}

// TestConcurrentMix tests mixed inserts, deletes, and searches for races.
func TestConcurrentMix(t *testing.T) {
	t.Parallel()

	const n = 1000
	sl := skiplist.New[int, int]()
	for i := 0; i < n; i++ {
		sl.Insert(i, i)
	}

	var wg sync.WaitGroup
	for _, role := range []string{"insert", "delete", "search"} {
		role := role
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(0, 0))
			for i := 0; i < n; i++ {
				k := int(rng.Int32N(n))
				switch role {
				case "insert":
					sl.Insert(k, k*2)
				case "delete":
					sl.Delete(k)
				case "search":
					sl.Search(k)
				}
			}
		}()
	}
	wg.Wait()
}

// TestRangeSortedUnderConcurrentInsert verifies that Range always returns
// keys in ascending order even when inserts occur concurrently.
func TestRangeSortedUnderConcurrentInsert(t *testing.T) {
	t.Parallel()

	sl := skiplist.New[int, int]()
	for i := 0; i < 1000; i++ {
		sl.Insert(i, i)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewPCG(1, 0))
		for {
			select {
			case <-stop:
				return
			default:
				sl.Insert(int(rng.Int32N(2000)), 0)
			}
		}
	}()

	for iter := 0; iter < 20; iter++ {
		prev := -1
		sl.Range(0, 999, func(k, _ int) bool {
			if k <= prev {
				t.Errorf("Range not sorted: got %d after %d", k, prev)
				return false
			}
			prev = k
			return true
		})
	}

	close(stop)
	wg.Wait()
}

func ExampleNew() {
	sl := skiplist.New[string, int]()
	sl.Insert("apple", 1)
	sl.Insert("cherry", 3)
	sl.Insert("banana", 2)

	v, ok := sl.Search("banana")
	fmt.Printf("banana: %d found=%v\n", v, ok)

	sl.Delete("cherry")
	_, ok = sl.Search("cherry")
	fmt.Printf("cherry after delete: found=%v\n", ok)

	// Range returns keys in sorted order.
	sl.Range("apple", "cherry", func(k string, _ int) bool {
		fmt.Println(k)
		return true
	})
	// Output:
	// banana: 2 found=true
	// cherry after delete: found=false
	// apple
	// banana
}
```

Your turn: add `TestYourTurn` that inserts keys 0–99, deletes every even key, then calls `Range(0, 99)` and asserts that no even key appears in the result. The test must fail if `Delete` is replaced with a no-op.

### Exercise 6: Demo Entry Point

Create `cmd/demo/main.go`. This file is `package main` and can only touch exported API:

```go
// Command demo exercises the skiplist package with a small integer-keyed map.
package main

import (
	"fmt"

	"example.com/skiplist"
)

func main() {
	sl := skiplist.New[int, string]()

	words := []struct {
		k int
		v string
	}{
		{5, "five"}, {2, "two"}, {8, "eight"},
		{1, "one"}, {4, "four"}, {7, "seven"},
		{3, "three"}, {6, "six"}, {9, "nine"},
	}
	for _, w := range words {
		sl.Insert(w.k, w.v)
	}

	fmt.Printf("Len=%d Height=%d\n", sl.Len(), sl.Height())

	v, ok := sl.Search(4)
	fmt.Printf("Search(4) = %q, found=%v\n", v, ok)

	sl.Insert(4, "FOUR")
	v, _ = sl.Search(4)
	fmt.Printf("After update, Search(4) = %q\n", v)

	fmt.Print("Range [3,7]: ")
	sl.Range(3, 7, func(k int, val string) bool {
		fmt.Printf("%d=%s ", k, val)
		return true
	})
	fmt.Println()

	sl.Delete(4)
	_, ok = sl.Search(4)
	fmt.Printf("After Delete(4), Search(4) found=%v Len=%d\n", ok, sl.Len())
}
```

Run with: `go run ./cmd/demo`

## Common Mistakes

### Locking the Same Mutex Twice

Wrong: passing `preds[:topLevel]` to a lock helper that locks each element unconditionally. The head sentinel appears as a predecessor at every level; locking it twice deadlocks because `sync.Mutex` is not reentrant in Go.

Fix: deduplicate predecessors before locking. `acquireLocks` uses a map to track which nodes have already been locked. Only the first occurrence of each node in the slice triggers `mu.Lock()`.

### Storing a Pointer to a Local Variable Incorrectly

Wrong:

```go
func (sl *SkipList[K, V]) Insert(key K, value V) {
	// ...
	n.val.Store(&value) // value is the function parameter
	// ...
}
```

This stores a pointer to the function's stack frame copy of `value`. After `Insert` returns, that memory may be reused. In practice, Go's escape analysis promotes `value` to the heap when its address is taken, so this compiles and runs correctly — but it relies on the compiler's escape analysis, not on explicit heap allocation.

Fix: the code as written is safe because the compiler detects that `&value` escapes to the heap. But if you are uncertain, write `v := value; n.val.Store(&v)` to make the intent explicit. The same applies in the duplicate-insert update path.

### Skipping the Validation Retry

Wrong: acquiring locks in `Insert` but returning an error instead of retrying when validation fails:

```go
locked, valid := acquireLocks[K, V](preds[:topLevel], succs[:topLevel])
if !valid {
	releaseLocks[K, V](locked)
	return // BUG: the insert is silently dropped
}
```

Fix: the outer `for {}` loop exists precisely to retry. If validation fails, release locks and restart from `findPreds`. Dropping the insert on conflict produces a skip list that silently loses data under concurrent writes.

### Reading Range Results After Returning True From a Deleted Node

Wrong: assuming that a node that `Range` has already called `fn` for is still in the list at the time `fn` returns. A concurrent `Delete` can logically mark the node between the `Range` iterator's `marked.Load()` check and the `fn` call.

Fix: `Range` does check `marked.Load()` before calling `fn`. There is no TOCTOU here: if the node was not marked when `Range` read it, the iterator delivers a valid snapshot value. The key may be concurrently deleted before `fn` returns, but `fn` received a consistent (key, value) pair at the time of the read. This is the documented behavior: `Range` is not a transactional snapshot.

## Verification

From `~/go-exercises/skiplist`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
go run ./cmd/demo
```

All five must pass without errors. `go test -race` is required — it is the only tool that catches data races in the concurrent tests. A passing test suite without `-race` is insufficient.

The concurrent tests (`TestConcurrentInsertSearch`, `TestConcurrentMix`, `TestRangeSortedUnderConcurrentInsert`) do not assert specific final values for the mixed-operation tests, because the final state of the list depends on scheduling. What they do assert is: no race detector reports, correct Len() after non-overlapping inserts, and sorted order of Range even under concurrent modification.

## Summary

- A skip list achieves O(log n) expected search, insert, and delete by maintaining multiple levels of sorted linked lists with probabilistically generated node heights.
- Lock-free reads use `atomic.Pointer[T].Load()`. Go's memory model guarantees that a reader seeing a newly inserted node also sees its pre-set forward pointers, because the predecessor's `atomic.Store` synchronizes-with the reader's `atomic.Load`.
- Fine-grained writes lock only the affected predecessor nodes. The `acquireLocks` helper deduplicates the predecessor set to prevent re-locking the same mutex (which would deadlock, since `sync.Mutex` is not reentrant).
- Lazy deletion separates logical deletion (a CAS on `marked`) from physical unlinking. Exactly one goroutine per key wins the CAS; `Search` and `Range` observe the deletion immediately via the `marked` flag, before physical unlinking completes.
- `cmp.Ordered` and `cmp.Compare` (Go 1.21+) provide a clean three-way comparison over all ordered types without a callback parameter.

## What's Next

Next: [Hazard Pointer Memory Reclamation](../03-hazard-pointer-reclamation/03-hazard-pointer-reclamation.md).

## Resources

- Go memory model: go.dev/ref/mem — the authoritative source for atomic happens-before guarantees used throughout this lesson.
- `sync/atomic` package: pkg.go.dev/sync/atomic — `atomic.Pointer[T]`, `atomic.Bool`, `atomic.Int64` types used by the node and SkipList structs.
- `cmp` package: pkg.go.dev/cmp — `cmp.Ordered` constraint and `cmp.Compare` three-way comparison.
- Herlihy, M. & Shavit, N. "The Art of Multiprocessor Programming" (2012), Chapter 14: SkipLists — the lazy skip list algorithm this implementation is based on.
- Pugh, W. "Skip Lists: A Probabilistic Alternative to Balanced Trees" (1990), Communications of the ACM — the original probabilistic analysis of expected height and traversal cost.
