# Exercise 4: Bounded LRU cache in front of a slow repository

Put a cache in front of a slow datastore and you must answer one question: what
gets evicted when it fills up? A least-recently-used cache evicts whatever has
gone longest untouched, which matches the access pattern of most read paths. The
classic implementation pairs a map (O(1) lookup) with a doubly linked list (O(1)
recency reordering and tail eviction) so that both operations stay constant time.
This module builds exactly that, with an eviction callback for observability.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
lru/                       independent module: example.com/lru
  go.mod
  lru.go                   type LRU[K,V]; New, Get, Put, Len, eviction callback
  cmd/
    demo/
      main.go              read-through cache over a "slow" loader
  lru_test.go              eviction order, recency, capacity, update-in-place, callback
```

- Files: `lru.go`, `cmd/demo/main.go`, `lru_test.go`.
- Implement: `LRU[K comparable, V any]` using `map[K]*list.Element` plus a `container/list`; `Get` moves the node to front, `Put` inserts/updates and evicts the back node past capacity, with an `onEvict(K, V)` hook.
- Test: least-recently-used leaves first, `Get` updates recency, capacity is never exceeded, update-in-place does not grow size, and the eviction callback fires with the evicted key/value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/lru/cmd/demo
cd ~/go-exercises/lru
go mod init example.com/lru
```

### Two structures pointing at the same node

An LRU needs two O(1) operations that pull in opposite directions: find a value by
key, and reorder by recency. No single structure gives both cheaply, so you use
two and wire them together. The map is `map[K]*list.Element`: the key finds the
*list node*, not the value directly. The `container/list` doubly linked list holds
the actual entries in recency order — front is most-recently-used, back is least.
Each list node's `Value` carries a `*pair[K, V]` (the key and value together); the
key is stored in the node so that when you evict the back node you know which map
entry to delete.

The two operations then fall out. `Get(key)`: look up the node in the map; if
present, `MoveToFront` it (this is the step people forget — without it recency
never updates and eviction degenerates to random) and return its value. `Put`:
if the key exists, move its node to front and overwrite the value *in place* (no
new node, so size does not grow); otherwise `PushFront` a new node, record it in
the map, and if the list is now over capacity, `Remove` the `Back` node, delete
its key from the map, and fire the eviction callback. The callback is the
observability hook — in production you would increment an eviction counter or log
the key so you can see whether the cache is thrashing.

One deliberate scope choice: this LRU is not safe for concurrent use — it has no
lock. That keeps the recency logic in focus. To use it from multiple goroutines
you wrap every method in a `sync.Mutex` (a plain `Mutex`, not `RWMutex`, because
even `Get` mutates the list via `MoveToFront`), or you compose it behind the
sharded or TTL patterns from the sibling exercises.

Create `lru.go`:

```go
package lru

import "container/list"

// pair is what each list node carries: the key (so eviction can find the map
// entry) and the value.
type pair[K comparable, V any] struct {
	key   K
	value V
}

// LRU is a bounded least-recently-used cache. It is not safe for concurrent use;
// guard it with a sync.Mutex if shared across goroutines.
//
// ll orders entries by recency (front = most-recently-used, back = least);
// items maps each key to its node in ll; onEvict is an optional eviction hook.
type LRU[K comparable, V any] struct {
	cap     int
	ll      *list.List
	items   map[K]*list.Element
	onEvict func(K, V)
}

// New returns an LRU holding at most capacity entries. capacity < 1 is clamped
// to 1. onEvict may be nil.
func New[K comparable, V any](capacity int, onEvict func(K, V)) *LRU[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	return &LRU[K, V]{
		cap:     capacity,
		ll:      list.New(),
		items:   make(map[K]*list.Element, capacity),
		onEvict: onEvict,
	}
}

// Get returns the value for key and marks it most-recently-used.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*pair[K, V]).value, true
	}
	var zero V
	return zero, false
}

// Put inserts or updates key. On overflow it evicts the least-recently-used
// entry and invokes onEvict.
func (c *LRU[K, V]) Put(key K, value V) {
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*pair[K, V]).value = value // update in place: size unchanged
		return
	}
	el := c.ll.PushFront(&pair[K, V]{key: key, value: value})
	c.items[key] = el
	if c.ll.Len() > c.cap {
		c.evictOldest()
	}
}

// Len reports the number of entries currently held.
func (c *LRU[K, V]) Len() int { return c.ll.Len() }

func (c *LRU[K, V]) evictOldest() {
	el := c.ll.Back()
	if el == nil {
		return
	}
	c.ll.Remove(el)
	p := el.Value.(*pair[K, V])
	delete(c.items, p.key)
	if c.onEvict != nil {
		c.onEvict(p.key, p.value)
	}
}
```

### The runnable demo

The demo is a read-through cache over a "slow" loader: on a miss it loads (and
counts the load), on a hit it serves from cache. Capacity is 2, so the third
distinct key evicts the least-recently-used one — and because `alpha` was read
just before, `beta` is the victim.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/lru"
)

func main() {
	loads := 0
	c := lru.New[string, int](2, func(k string, v int) {
		fmt.Printf("evicted %s=%d\n", k, v)
	})

	load := func(key string) int {
		if v, ok := c.Get(key); ok {
			return v
		}
		loads++
		v := len(key) // pretend this came from a slow database
		c.Put(key, v)
		return v
	}

	load("alpha") // miss
	load("beta")  // miss
	load("alpha") // hit, refreshes alpha's recency
	load("gamma") // miss, evicts least-recently-used (beta)

	_, betaOK := c.Get("beta")
	_, alphaOK := c.Get("alpha")
	fmt.Printf("loads: %d\n", loads)
	fmt.Printf("beta cached: %v\n", betaOK)
	fmt.Printf("alpha cached: %v\n", alphaOK)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
evicted beta=4
loads: 3
beta cached: false
alpha cached: true
```

### Tests

Create `lru_test.go`:

```go
package lru

import (
	"fmt"
	"testing"
)

func TestEvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()

	c := New[string, int](2, nil)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Put("c", 3) // over capacity: evicts a (least recently used)

	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if _, ok := c.Get("b"); !ok {
		t.Fatal("b should still be present")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should be present")
	}
}

func TestGetUpdatesRecency(t *testing.T) {
	t.Parallel()

	c := New[string, int](2, nil)
	c.Put("a", 1)
	c.Put("b", 2)
	c.Get("a")    // a becomes most-recently-used
	c.Put("c", 3) // should evict b, not a

	if _, ok := c.Get("a"); !ok {
		t.Fatal("a was touched and must survive eviction")
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("b was least-recently-used and must be evicted")
	}
}

func TestCapacityNeverExceeded(t *testing.T) {
	t.Parallel()

	c := New[int, int](3, nil)
	for i := range 100 {
		c.Put(i, i)
		if c.Len() > 3 {
			t.Fatalf("Len = %d after Put(%d), want <= 3", c.Len(), i)
		}
	}
}

func TestUpdateInPlaceDoesNotGrow(t *testing.T) {
	t.Parallel()

	c := New[string, int](2, nil)
	c.Put("a", 1)
	c.Put("a", 2) // update, not insert

	if c.Len() != 1 {
		t.Fatalf("Len = %d after updating the same key, want 1", c.Len())
	}
	if v, _ := c.Get("a"); v != 2 {
		t.Fatalf("Get(a) = %d, want 2 (value should be updated)", v)
	}
}

func TestEvictionCallbackFires(t *testing.T) {
	t.Parallel()

	var gotKey string
	var gotVal int
	c := New[string, int](1, func(k string, v int) {
		gotKey, gotVal = k, v
	})
	c.Put("a", 1)
	c.Put("b", 2) // evicts a; callback must fire with (a, 1)

	if gotKey != "a" || gotVal != 1 {
		t.Fatalf("onEvict got (%q,%d), want (a,1)", gotKey, gotVal)
	}
}

func Example() {
	c := New[string, int](2, nil)
	c.Put("x", 10)
	c.Put("y", 20)
	v, ok := c.Get("x")
	fmt.Println(v, ok)
	// Output: 10 true
}
```

## Review

The cache is correct when three invariants hold: `Len()` never exceeds capacity,
the least-recently-*used* entry is always the one evicted (which requires `Get`
and `Put` to `MoveToFront` on every access), and updating an existing key changes
the value without growing the size. The mistakes to avoid are forgetting the
`MoveToFront` in `Get` (recency stops updating and eviction becomes effectively
random — `TestGetUpdatesRecency` catches it) and evicting from the wrong end of
the list (evict `Back`, the least-recently-used, not `Front`). Remember the map
value is the `*list.Element`, so the map and the list always point at the same
node; the key lives inside the node so eviction can delete the right map entry.
Run `go test -count=1 -race ./...`.

## Resources

- [`container/list` package](https://pkg.go.dev/container/list) — `New`, `PushFront`, `MoveToFront`, `Remove`, `Back`, and `Element.Value`.
- [Go blog: the `sync` package and caching patterns](https://go.dev/blog/maps) — background on maps as caches and why bounds matter.
- [`groupcache` LRU](https://github.com/golang/groupcache/blob/master/lru/lru.go) — a real-world map-plus-list LRU in the standard toolchain's ecosystem.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-generic-set.md](03-generic-set.md) | Next: [05-ttl-cache.md](05-ttl-cache.md)
