# Exercise 6: An LRU Cache Built From *node Doubly Linked List

An LRU cache is the canonical pointer-linked data structure: a hash map from key to
node for O(1) lookup, plus a doubly linked list of those same nodes for O(1)
recency ordering. This exercise builds it by hand from `*node` fields, with sentinel
head/tail nodes, and gets the bookkeeping right — including nulling an evicted node's
links so it does not leak or dangle.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
lrucache/                 independent module: example.com/lrucache
  go.mod
  lru.go                  node{prev,next *node}; LRUCache; Get (promote); Put (evict)
  cmd/
    demo/
      main.go             fill past capacity, show LRU eviction order
  lru_test.go             eviction order, promotion, update-in-place, links cleared, -race
```

Files: `lru.go`, `cmd/demo/main.go`, `lru_test.go`.
Implement: a `node` with `key`, `val`, `prev`, `next *node`; an `LRUCache` with a map and sentinel head/tail; `Get(key) (V, bool)` that moves the node to front; `Put(key, val)` that inserts or updates and evicts the tail when over capacity; mutex-guarded.
Test: capacity eviction removes the least-recently-used; `Get` promotes; `Put` on an existing key updates without growing; evicted node's links are nulled; `len(map) == size` invariant; churn under `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/lrucache/cmd/demo
cd ~/go-exercises/lrucache
go mod init example.com/lrucache
```

### Sentinels, wiring, and why you must null the links on eviction

The list is doubly linked so that any node can be unlinked in O(1) given only a
pointer to it — you need both `prev` and `next` to splice it out. Two *sentinel*
nodes, `head` and `tail`, are permanent dummies that bracket the list; the
most-recently-used real node sits at `head.next` and the least-recently-used at
`tail.prev`. Sentinels remove all the nil-edge special cases: inserting at the front
is always "between `head` and `head.next`", and the eviction victim is always
`tail.prev`, with no "is the list empty?" branch in the splice code.

`Get` looks the node up in the map (O(1)); on a hit it unlinks the node from its
current position and re-inserts it right after `head`, marking it most-recently-used.
`Put` on an existing key updates the value and promotes; on a new key it inserts at
the front and, if the cache is now over capacity, evicts `tail.prev`. Eviction is two
steps: remove the victim from the map (`delete(m, victim.key)` — this is why the node
stores its own `key`) and unlink it from the list.

The step everyone forgets is nulling the evicted node's `prev` and `next`. After
`remove(victim)` splices it out, the victim still holds pointers into the live list.
If anything retains a reference to that victim (a stale caller, a bug, the GC's
inspection), those links keep the neighbors reachable — a leak — or a later stray
traversal through the victim follows a pointer into a structure it no longer belongs
to. Setting `victim.prev = nil; victim.next = nil` severs it cleanly. The test asserts
this directly.

Create `lru.go`:

```go
package lrucache

import "sync"

type node[K comparable, V any] struct {
	key        K
	val        V
	prev, next *node[K, V]
}

// LRUCache is a fixed-capacity cache backed by a map for O(1) lookup and a doubly
// linked list (with sentinel head/tail) for O(1) recency ordering.
type LRUCache[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	items    map[K]*node[K, V]
	head     *node[K, V] // sentinel: head.next is most-recently-used
	tail     *node[K, V] // sentinel: tail.prev is least-recently-used
}

func New[K comparable, V any](capacity int) *LRUCache[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	head := &node[K, V]{}
	tail := &node[K, V]{}
	head.next = tail
	tail.prev = head
	return &LRUCache[K, V]{
		capacity: capacity,
		items:    make(map[K]*node[K, V]),
		head:     head,
		tail:     tail,
	}
}

// insertFront links n in just after the head sentinel (most-recently-used).
func (c *LRUCache[K, V]) insertFront(n *node[K, V]) {
	n.prev = c.head
	n.next = c.head.next
	c.head.next.prev = n
	c.head.next = n
}

// remove splices n out of the list and nulls its own links so it cannot dangle.
func (c *LRUCache[K, V]) remove(n *node[K, V]) {
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev = nil
	n.next = nil
}

// Get returns the value and promotes the node to most-recently-used.
func (c *LRUCache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.remove(n)
	c.insertFront(n)
	return n.val, true
}

// Put inserts or updates key, promoting it, and evicts the LRU node if over cap.
func (c *LRUCache[K, V]) Put(key K, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n, ok := c.items[key]; ok {
		n.val = val
		c.remove(n)
		c.insertFront(n)
		return
	}
	n := &node[K, V]{key: key, val: val}
	c.items[key] = n
	c.insertFront(n)
	if len(c.items) > c.capacity {
		c.evictLRU()
	}
}

// evictLRU removes the least-recently-used node (tail.prev) from list and map.
func (c *LRUCache[K, V]) evictLRU() {
	lru := c.tail.prev
	if lru == c.head {
		return // empty
	}
	c.remove(lru)
	delete(c.items, lru.key)
}

// Len reports the number of live entries.
func (c *LRUCache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
```

### The runnable demo

The demo builds a capacity-2 cache, inserts three keys (evicting the oldest), then
promotes one and inserts a fourth to show which key survives.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lrucache"
)

func main() {
	c := lrucache.New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3) // capacity 2: evicts "a" (least recently used)

	if _, ok := c.Get("a"); !ok {
		fmt.Println("a evicted")
	}

	c.Get("b")    // promote b to most-recently-used
	c.Put("d", 4) // evicts "c", not "b"

	if _, ok := c.Get("c"); !ok {
		fmt.Println("c evicted")
	}
	if v, ok := c.Get("b"); ok {
		fmt.Printf("b survived: %d\n", v)
	}
	fmt.Printf("len=%d\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
a evicted
c evicted
b survived: 2
len=2
```

### Tests

The tests pin every invariant: eviction removes the least-recently-used key, `Get`
promotes so the promoted key survives a later eviction, `Put` on an existing key
updates the value without growing the cache, the size never exceeds capacity, and an
evicted node's `prev`/`next` are nulled. A churn test hammers the cache from many
goroutines under `-race`.

Create `lru_test.go`:

```go
package lrucache

import (
	"sync"
	"testing"
)

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3) // evicts "a"

	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("b = %d,%v; want 2,true", v, ok)
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Fatalf("c = %d,%v; want 3,true", v, ok)
	}
}

func TestGetPromotes(t *testing.T) {
	t.Parallel()
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Get("a")    // promote a; b is now LRU
	c.Put("c", 3) // evicts b, not a

	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted after a was promoted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should have survived (it was promoted)")
	}
}

func TestPutUpdatesInPlace(t *testing.T) {
	t.Parallel()
	c := New[string, int](2)
	c.Put("a", 1)
	c.Put("a", 100) // update, not insert
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (update must not grow)", c.Len())
	}
	if v, _ := c.Get("a"); v != 100 {
		t.Fatalf("a = %d, want 100", v)
	}
}

func TestSizeNeverExceedsCapacity(t *testing.T) {
	t.Parallel()
	c := New[int, int](3)
	for i := range 100 {
		c.Put(i, i)
		if c.Len() > 3 {
			t.Fatalf("Len = %d exceeded capacity 3", c.Len())
		}
	}
	if c.Len() != 3 {
		t.Fatalf("final Len = %d, want 3", c.Len())
	}
}

func TestEvictedNodeLinksCleared(t *testing.T) {
	t.Parallel()
	c := New[string, int](1)
	c.Put("a", 1)
	// Grab the node before it is evicted.
	victim := c.items["a"]
	c.Put("b", 2) // evicts "a"

	if victim.prev != nil || victim.next != nil {
		t.Fatal("evicted node must have prev/next nulled to avoid dangling links")
	}
	if _, ok := c.items["a"]; ok {
		t.Fatal("evicted key must be removed from the map")
	}
}

func TestConcurrentChurn(t *testing.T) {
	t.Parallel()
	c := New[int, int](16)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 200 {
				k := (g*200 + i) % 32
				c.Put(k, i)
				c.Get(k)
			}
		}(g)
	}
	wg.Wait()
	if c.Len() > 16 {
		t.Fatalf("Len = %d exceeded capacity under churn", c.Len())
	}
}
```

## Review

The cache is correct when the least-recently-used key is the one evicted, `Get` and
`Put` both promote to most-recently-used, an update does not grow the cache, and the
size invariant `len(map) <= capacity` always holds. The link-clearing test is the one
that distinguishes a correct pointer-structure implementation from a merely
functional one: forgetting to null the evicted node's `prev`/`next` passes every
behavioral test while quietly leaking.

The mistakes: dropping a node from the map but not the list (or vice versa), which
desynchronizes the two views; forgetting sentinel head/tail and drowning in nil-edge
branches; storing the value but not the `key` in the node, so eviction cannot
`delete` from the map; and copying the cache by value, which copies the embedded
mutex and breaks locking — always pass `*LRUCache`. Run `go test -race` to confirm the
mutex guards both the map and the list under churn.

## Resources

- [`container/list`](https://pkg.go.dev/container/list) — the stdlib doubly linked list; this exercise hand-rolls the same idea to expose the pointer wiring.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — self-referential struct fields (`*node`).
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock guarding the cache; note copylocks forbids copying it.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-request-builder-pointer-receiver.md](07-request-builder-pointer-receiver.md)
