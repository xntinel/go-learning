# 7. Concurrent B-Tree

## Concepts

A B-tree keeps sorted keys in nodes that hold many entries each, so the tree
stays shallow and lookups touch only a handful of nodes. Every node (except the
root) holds between `t-1` and `2t-1` keys, where `t` is the *minimum degree*;
all leaves sit at the same depth. Insertion uses the classic top-down
*proactive split*: while descending toward the target leaf, any full child is
split before you enter it, so a split never has to propagate back up past the
node you are holding.

This lesson builds a generic, in-memory B-tree and makes it safe for concurrent
use with a single `sync.RWMutex`. Reads (`Get`, `Ascend`, `Range`) take the
read lock, so any number of readers run in parallel; writes (`Insert`) take the
write lock exclusively. That gives correct, race-free behaviour with fully
parallel reads, which is the right baseline before reaching for finer-grained
schemes. Per-node lock coupling ("crabbing") and optimistic, version-validated
lock-free reads remove the single-writer bottleneck; they are described in the
extensions, but this lesson implements and verifies the reader-writer design so
the invariant — *every leaf at the same depth, occupancy within `[t-1, 2t-1]`* —
is easy to assert under `go test -race`.

Keys use the `cmp.Ordered` constraint, so the tree works for any ordered type.

Create `go.mod`:

```go
module example.com/cbtree

go 1.26
```

Create `btree.go`:

```go
// Package cbtree implements a generic, concurrent in-memory B-tree.
//
// The tree structure is guarded by a single reader-writer lock: readers run in
// parallel, writers take the tree exclusively. Each non-root node holds between
// t-1 and 2t-1 keys and all leaves are at the same depth.
package cbtree

import (
	"cmp"
	"sync"
)

type node[K cmp.Ordered, V any] struct {
	keys  []K
	vals  []V
	child []*node[K, V]
	leaf  bool
}

// Tree is a concurrent B-tree mapping keys of type K to values of type V.
type Tree[K cmp.Ordered, V any] struct {
	mu   sync.RWMutex
	root *node[K, V]
	t    int
	n    int
}

// New returns an empty tree with minimum degree t. Values of t below 2 are
// clamped to 2 (a 2-3-4 tree).
func New[K cmp.Ordered, V any](t int) *Tree[K, V] {
	if t < 2 {
		t = 2
	}
	return &Tree[K, V]{root: &node[K, V]{leaf: true}, t: t}
}

// Len reports the number of distinct keys stored.
func (tr *Tree[K, V]) Len() int {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	return tr.n
}

// Get returns the value stored for key and whether it was present.
func (tr *Tree[K, V]) Get(key K) (V, bool) {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	x := tr.root
	for x != nil {
		i := 0
		for i < len(x.keys) && key > x.keys[i] {
			i++
		}
		if i < len(x.keys) && key == x.keys[i] {
			return x.vals[i], true
		}
		if x.leaf {
			break
		}
		x = x.child[i]
	}
	var zero V
	return zero, false
}

// Insert adds key with val, overwriting any existing value for key.
func (tr *Tree[K, V]) Insert(key K, val V) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	r := tr.root
	if len(r.keys) == 2*tr.t-1 {
		s := &node[K, V]{child: []*node[K, V]{r}}
		tr.root = s
		tr.splitChild(s, 0)
		tr.insertNonFull(s, key, val)
		return
	}
	tr.insertNonFull(r, key, val)
}

// splitChild splits the full child x.child[i] around its median key, promoting
// that key into x.
func (tr *Tree[K, V]) splitChild(x *node[K, V], i int) {
	t := tr.t
	y := x.child[i]
	z := &node[K, V]{leaf: y.leaf}
	z.keys = append(z.keys, y.keys[t:]...)
	z.vals = append(z.vals, y.vals[t:]...)
	medKey, medVal := y.keys[t-1], y.vals[t-1]
	if !y.leaf {
		z.child = append(z.child, y.child[t:]...)
		y.child = y.child[:t]
	}
	y.keys = y.keys[:t-1]
	y.vals = y.vals[:t-1]

	x.keys = append(x.keys, medKey)
	copy(x.keys[i+1:], x.keys[i:])
	x.keys[i] = medKey
	x.vals = append(x.vals, medVal)
	copy(x.vals[i+1:], x.vals[i:])
	x.vals[i] = medVal
	x.child = append(x.child, nil)
	copy(x.child[i+2:], x.child[i+1:])
	x.child[i+1] = z
}

// insertNonFull inserts into a node guaranteed not to be full.
func (tr *Tree[K, V]) insertNonFull(x *node[K, V], key K, val V) {
	if x.leaf {
		pos := len(x.keys)
		for i := range x.keys {
			if key == x.keys[i] {
				x.vals[i] = val
				return
			}
			if key < x.keys[i] {
				pos = i
				break
			}
		}
		var zk K
		var zv V
		x.keys = append(x.keys, zk)
		copy(x.keys[pos+1:], x.keys[pos:])
		x.keys[pos] = key
		x.vals = append(x.vals, zv)
		copy(x.vals[pos+1:], x.vals[pos:])
		x.vals[pos] = val
		tr.n++
		return
	}

	i := 0
	for i < len(x.keys) && key > x.keys[i] {
		i++
	}
	if i < len(x.keys) && key == x.keys[i] {
		x.vals[i] = val
		return
	}
	if len(x.child[i].keys) == 2*tr.t-1 {
		tr.splitChild(x, i)
		if key > x.keys[i] {
			i++
		} else if key == x.keys[i] {
			x.vals[i] = val
			return
		}
	}
	tr.insertNonFull(x.child[i], key, val)
}

// Ascend calls fn for every key in ascending order until fn returns false.
func (tr *Tree[K, V]) Ascend(fn func(K, V) bool) {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	ascend(tr.root, fn)
}

func ascend[K cmp.Ordered, V any](x *node[K, V], fn func(K, V) bool) bool {
	if x == nil {
		return true
	}
	if x.leaf {
		for i := range x.keys {
			if !fn(x.keys[i], x.vals[i]) {
				return false
			}
		}
		return true
	}
	for i := range x.keys {
		if !ascend(x.child[i], fn) {
			return false
		}
		if !fn(x.keys[i], x.vals[i]) {
			return false
		}
	}
	return ascend(x.child[len(x.keys)], fn)
}

// Range calls fn for every key in [lo, hi) in ascending order, stopping early
// if fn returns false.
func (tr *Tree[K, V]) Range(lo, hi K, fn func(K, V) bool) {
	tr.mu.RLock()
	defer tr.mu.RUnlock()
	ascend(tr.root, func(k K, v V) bool {
		if k < lo {
			return true
		}
		if k >= hi {
			return false
		}
		return fn(k, v)
	})
}
```

Create `btree_test.go`:

```go
package cbtree

import (
	"cmp"
	"fmt"
	"math/rand"
	"sync"
	"testing"
)

// leafDepths records the depth of every leaf so the caller can verify they match.
func leafDepths[K cmp.Ordered, V any](x *node[K, V], d int, out *[]int) {
	if x.leaf {
		*out = append(*out, d)
		return
	}
	for _, c := range x.child {
		leafDepths(c, d+1, out)
	}
}

func checkInvariants[K cmp.Ordered, V any](t *testing.T, tr *Tree[K, V]) {
	t.Helper()
	var depths []int
	leafDepths(tr.root, 0, &depths)
	for i := 1; i < len(depths); i++ {
		if depths[i] != depths[0] {
			t.Fatalf("leaves at different depths: %v", depths)
		}
	}
	var walk func(x *node[K, V], root bool)
	walk = func(x *node[K, V], root bool) {
		if !root {
			if len(x.keys) < tr.t-1 || len(x.keys) > 2*tr.t-1 {
				t.Fatalf("occupancy %d out of [%d,%d]", len(x.keys), tr.t-1, 2*tr.t-1)
			}
		}
		if !x.leaf {
			if len(x.child) != len(x.keys)+1 {
				t.Fatalf("children %d != keys+1 %d", len(x.child), len(x.keys)+1)
			}
			for _, c := range x.child {
				walk(c, false)
			}
		}
	}
	walk(tr.root, true)
}

func TestInsertGet(t *testing.T) {
	tr := New[int, int](2)
	for _, k := range []int{5, 3, 8, 1, 4, 7, 9, 2, 6, 0} {
		tr.Insert(k, k*10)
	}
	if tr.Len() != 10 {
		t.Fatalf("Len = %d, want 10", tr.Len())
	}
	for k := 0; k < 10; k++ {
		if v, ok := tr.Get(k); !ok || v != k*10 {
			t.Fatalf("Get(%d) = %d,%v", k, v, ok)
		}
	}
	if _, ok := tr.Get(42); ok {
		t.Fatal("Get(42) should be absent")
	}
	checkInvariants(t, tr)
}

func TestOverwrite(t *testing.T) {
	tr := New[string, int](3)
	tr.Insert("a", 1)
	tr.Insert("a", 2)
	if v, _ := tr.Get("a"); v != 2 {
		t.Fatalf("overwrite failed: got %d", v)
	}
	if tr.Len() != 1 {
		t.Fatalf("Len = %d, want 1", tr.Len())
	}
}

func TestAscendSorted(t *testing.T) {
	tr := New[int, int](2)
	want := make([]int, 0, 200)
	for _, k := range rand.New(rand.NewSource(1)).Perm(200) {
		tr.Insert(k, k)
		want = append(want, k)
	}
	prev := -1
	count := 0
	tr.Ascend(func(k, v int) bool {
		if k <= prev {
			t.Fatalf("out of order: %d after %d", k, prev)
		}
		prev = k
		count++
		return true
	})
	if count != 200 {
		t.Fatalf("ascended %d keys, want 200", count)
	}
	checkInvariants(t, tr)
}

func TestRange(t *testing.T) {
	tr := New[int, int](2)
	for k := 0; k < 100; k++ {
		tr.Insert(k, k)
	}
	var got []int
	tr.Range(10, 15, func(k, v int) bool {
		got = append(got, k)
		return true
	})
	want := []int{10, 11, 12, 13, 14}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Range = %v, want %v", got, want)
	}
}

// TestConcurrent exercises parallel writers (disjoint key ranges) and readers
// under the race detector.
func TestConcurrent(t *testing.T) {
	tr := New[int, string](4)
	const writers = 8
	const perWriter = 500
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(base int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				k := base*perWriter + i
				tr.Insert(k, fmt.Sprintf("v%d", k))
			}
		}(w)
	}
	// Concurrent readers run while writers mutate the tree.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				tr.Get(i)
				tr.Ascend(func(int, string) bool { return true })
			}
		}()
	}
	wg.Wait()

	if tr.Len() != writers*perWriter {
		t.Fatalf("Len = %d, want %d", tr.Len(), writers*perWriter)
	}
	for k := 0; k < writers*perWriter; k++ {
		if v, ok := tr.Get(k); !ok || v != fmt.Sprintf("v%d", k) {
			t.Fatalf("Get(%d) = %q,%v", k, v, ok)
		}
	}
	checkInvariants(t, tr)
}

func ExampleTree_Ascend() {
	tr := New[int, string](2)
	tr.Insert(3, "c")
	tr.Insert(1, "a")
	tr.Insert(2, "b")
	tr.Insert(2, "B") // overwrite
	tr.Ascend(func(k int, v string) bool {
		fmt.Printf("%d=%s ", k, v)
		return true
	})
	fmt.Println()
	// Output:
	// 1=a 2=B 3=c
}
```

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strconv"
	"strings"

	"example.com/cbtree"
)

func main() {
	tr := cbtree.New[int, string](3)
	for i := 1; i <= 10; i++ {
		tr.Insert(i, "v"+strconv.Itoa(i))
	}
	fmt.Println("len:", tr.Len())

	got, ok := tr.Get(7)
	fmt.Printf("get 7 -> %q (%v)\n", got, ok)

	var ks []string
	tr.Range(3, 7, func(k int, v string) bool {
		ks = append(ks, strconv.Itoa(k))
		return true
	})
	fmt.Println("range [3,7):", strings.Join(ks, " "))
}
```

Run it with `go run ./cmd/demo`. Expected output:

```
len: 10
get 7 -> "v7" (true)
range [3,7): 3 4 5 6
```

## Verification

Run the checks before trusting the result:

```bash
gofmt -l .
go vet ./...
go test -race ./...
```

`go test -race` must report no data races: the reader-writer lock serializes
every structural change against concurrent readers, and `TestConcurrent` drives
eight writers and four readers at once. `checkInvariants` proves the structural
contract after the storm — all leaves at one depth, every non-root node within
`[t-1, 2t-1]` keys, and `len(children) == len(keys)+1` on every internal node.

## What's Next

Next: [Async/Await on Channels](../08-async-await-on-channels/08-async-await-on-channels.md).

## Resources

- "The Ubiquitous B-Tree", Douglas Comer (1979) - https://dl.acm.org/doi/10.1145/356770.356776
- "Efficient Locking for Concurrent Operations on B-Trees", Lehman and Yao (1981) - https://dl.acm.org/doi/10.1145/319628.319663
- Introduction to Algorithms (CLRS), Chapter 18 "B-Trees" - the proactive-split insert this lesson follows
- bbolt, a pure-Go B+ tree - https://github.com/etcd-io/bbolt
- google/btree, an in-memory B-tree for Go - https://github.com/google/btree
