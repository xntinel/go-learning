# Exercise 7: Delete From the Map But It Never Frees — The Ordering-Slice Leak

A TTL cache holding `*Entry` in a `map[string]*Entry` plus an eviction-order slice
has a subtle leak: `delete(byID, id)` alone does not free the `Entry` when the same
pointer is still reachable from the order slice. This module proves the leak with a
finalizer and builds an eviction that removes from *both* structures so the GC can
actually reclaim.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
evictcache/                   independent module: example.com/evictcache
  go.mod                      go 1.24
  cache.go                    Cache{byID map[string]*Entry, order []*Entry}; Put, evictBuggy, Evict
  cache_test.go               both structures cleaned; finalizer proves correct eviction frees, buggy retains
  cmd/demo/main.go            runnable demo of churn: put N, evict half, report lengths
```

Files: `cache.go`, `cache_test.go`, `cmd/demo/main.go`.
Implement: a `Cache` over `map[string]*Entry` and `[]*Entry`; a buggy `evictBuggy`
that only deletes from the map; a correct `Evict` that also
`slices.DeleteFunc`s the order slice (which zeroes the vacated tail for you).
Test: after `Evict`, `len(byID) == len(order)` and evicted ids are absent from
both; a finalizer proves the correctly-evicted `*Entry` is collected, while the
buggy path keeps it reachable from the order slice.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/evictcache/cmd/demo
cd ~/go-exercises/evictcache
go mod init example.com/evictcache
```

### Why delete is not enough, and how to prove it

The garbage collector frees an object only when it is unreachable from *every* live
root. A cache that stores each `*Entry` in two places — the `byID` map for lookup
and the `order` slice for eviction policy — has two references to each object.
`delete(byID, id)` removes one. If the `order` slice still holds the same pointer,
the `Entry` remains reachable and is never collected. Under sustained
insert/evict churn the `order` slice keeps every "evicted" pointer, growing without
bound: a leak that no map-length metric reveals, because `len(byID)` looks healthy
while the retained objects pile up behind the slice.

Correct eviction removes the key from the map *and* the pointer from the order
slice. `slices.DeleteFunc(order, pred)` compacts out the matching elements and
returns the shortened slice. A subtle detail matters here: compaction slides the
survivors down and leaves the old pointer values sitting in the now-unused tail
slots of the backing array — and those slots would themselves keep the objects
reachable. Since Go 1.22, `slices.Delete`/`DeleteFunc` zero that vacated tail for
you precisely to prevent this leak, so after `DeleteFunc` no stale slot holds a
reference and the `Entry` is unreachable and collectable. (Before 1.22 you had to
nil the tail by hand; the modern function does it, and knowing it does is the
point — a hand-rolled `s = s[:len(s)-1]` compaction that forgets to nil the dropped
slot reintroduces exactly this leak.)

Proving collection deterministically is the interesting part. Raw `HeapAlloc`
readings are noisy and flaky. Instead attach a `runtime.SetFinalizer` to the
`Entry`: the finalizer runs (asynchronously, after a GC) once the object becomes
unreachable. The test drops its own references, calls `runtime.GC()` in a bounded
loop, and waits on a channel the finalizer closes. If correct `Evict` made the
object unreachable, the finalizer fires and the channel closes; under the buggy
path the order slice still references it, so it never fires and the test asserts the
object is still structurally reachable in the slice.

Create `cache.go`:

```go
package evictcache

import "slices"

type Entry struct {
	Key   string
	Value string
}

// Cache stores each *Entry in a map for lookup and a slice for eviction order.
// A correct eviction must remove the pointer from BOTH.
type Cache struct {
	byID  map[string]*Entry
	order []*Entry
}

func NewCache() *Cache {
	return &Cache{byID: make(map[string]*Entry)}
}

// Put inserts a new entry and returns the stored pointer.
func (c *Cache) Put(key, value string) *Entry {
	e := &Entry{Key: key, Value: value}
	c.byID[key] = e
	c.order = append(c.order, e)
	return e
}

func (c *Cache) Len() int { return len(c.byID) }

func (c *Cache) OrderLen() int { return len(c.order) }

// evictBuggy removes the key from the map only. The *Entry stays reachable from
// the order slice, so it is never collected: an unbounded leak under churn.
func (c *Cache) evictBuggy(key string) {
	delete(c.byID, key)
}

// Evict removes the entry from both structures so the GC can reclaim the
// *Entry. slices.DeleteFunc (Go 1.22+) zeroes the vacated tail slots itself, so
// no stale pointer lingers in the backing array.
func (c *Cache) Evict(key string) {
	delete(c.byID, key)
	c.order = slices.DeleteFunc(c.order, func(e *Entry) bool {
		return e.Key == key
	})
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/evictcache"
)

func main() {
	c := evictcache.NewCache()
	for i := range 6 {
		c.Put(fmt.Sprintf("k%d", i), "v")
	}
	// Evict the even keys.
	for i := 0; i < 6; i += 2 {
		c.Evict(fmt.Sprintf("k%d", i))
	}
	fmt.Printf("map=%d order=%d (must match)\n", c.Len(), c.OrderLen())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
map=3 order=3 (must match)
```

### Tests

`TestEvictRemovesFromBothStructures` inserts N, evicts half, and asserts
`len(byID) == len(order)` with the evicted ids absent from both.
`TestCorrectEvictionAllowsCollection` attaches a finalizer to an entry, calls
`Evict`, and waits for the finalizer to fire — proof the object became
unreachable. `TestBuggyEvictionRetainsInOrderSlice` shows the buggy path leaves the
pointer structurally reachable in the order slice.

Create `cache_test.go`:

```go
package evictcache

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestEvictRemovesFromBothStructures(t *testing.T) {
	t.Parallel()

	c := NewCache()
	for i := range 10 {
		c.Put(fmt.Sprintf("k%d", i), "v")
	}
	for i := 0; i < 10; i += 2 { // evict evens
		c.Evict(fmt.Sprintf("k%d", i))
	}

	if c.Len() != 5 || c.OrderLen() != 5 {
		t.Fatalf("map=%d order=%d, want 5 and 5", c.Len(), c.OrderLen())
	}
	for i := 0; i < 10; i += 2 {
		key := fmt.Sprintf("k%d", i)
		if _, ok := c.byID[key]; ok {
			t.Fatalf("%s still in map", key)
		}
		for _, e := range c.order {
			if e != nil && e.Key == key {
				t.Fatalf("%s still in order slice", key)
			}
		}
	}
}

func TestCorrectEvictionAllowsCollection(t *testing.T) {
	// Not parallel: exercises the GC directly.
	c := NewCache()
	collected := make(chan struct{})

	// Create the entry in a nested scope so no stack reference lingers.
	func() {
		e := c.Put("victim", "v")
		runtime.SetFinalizer(e, func(*Entry) { close(collected) })
	}()

	c.Evict("victim") // removes from map AND order slice

	for range 100 {
		runtime.GC()
		select {
		case <-collected:
			return // finalizer fired: the entry was reclaimed
		default:
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("evicted entry was never collected: it is still reachable (leak)")
}

func TestBuggyEvictionRetainsInOrderSlice(t *testing.T) {
	t.Parallel()

	c := NewCache()
	e := c.Put("victim", "v")
	c.evictBuggy("victim") // deletes from map only

	if _, ok := c.byID["victim"]; ok {
		t.Fatal("buggy evict should still remove the map entry")
	}
	// The pointer is still reachable from the order slice: the leak.
	found := false
	for _, o := range c.order {
		if o == e {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the buggy path to leave the *Entry reachable in the order slice")
	}
}
```

## Review

The leak is invisible to the obvious metric: `len(byID)` drops on every
`delete`, so the map looks healthy while the `order` slice silently retains every
evicted pointer. `TestEvictRemovesFromBothStructures` enforces the real invariant —
the two structures stay the same length and no evicted id survives in either.
`TestCorrectEvictionAllowsCollection` is the strongest proof available: a finalizer
fires only when the object is unreachable, so its firing after `Evict` means the
memory can actually be reclaimed; `TestBuggyEvictionRetainsInOrderSlice` shows the
buggy path leaving the pointer alive in the slice. The general rule: a pointer is
freed only when unreachable from *all* structures; `slices.DeleteFunc` zeroes its
vacated tail for you (Go 1.22+), but a hand-rolled compaction that forgets to would
reintroduce the leak. Removing from one index is not eviction.

## Resources

- [`slices.DeleteFunc`](https://pkg.go.dev/slices#DeleteFunc) — compacts matching elements out; note the tail must be cleared to drop references.
- [`runtime.SetFinalizer`](https://pkg.go.dev/runtime#SetFinalizer) — runs after the object becomes unreachable; used here to prove collection.
- [Go Blog: Getting to Go — the GC](https://go.dev/blog/ismmkeynote) — reachability is what keeps an object alive.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-sync-map-pointer-registry.md](08-sync-map-pointer-registry.md)
