# Exercise 1: The Weak-Value Cache

A cache of large, shareable values wants to share a value while it is in use and let it be freed the moment nothing else holds it. This exercise builds that cache out of one idea: store `map[K]weak.Pointer[V]` instead of `map[K]*V`, so an entry never keeps its value alive, and let a `runtime.AddCleanup` prune the dead slot afterward.

This module is fully self-contained. It begins with its own `go mod init`, defines the cache type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
weakcache.go         Cache[K,V] (map[K]weak.Pointer[V]); New, Get, Len, deleteStale
cmd/
  demo/
    main.go          share a value while alive, drop it, GC, watch the reload
weakcache_test.go    sharing, reload-after-collection, cleanup pruning, identity,
                     two coexisting keys, and concurrent Get under -race
```

- Files: `weakcache.go`, `cmd/demo/main.go`, `weakcache_test.go`.
- Implement: `Cache[K, V]` with `New[K, V]()`, `(*Cache).Get(key, load)`, `(*Cache).Len()`, and the internal `deleteStale`.
- Test: `weakcache_test.go` proves a value is shared while alive, reloaded after collection, that the stale entry is pruned, that weak identity holds, that two keys coexist, and that concurrent `Get` is race-free.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p weak-value-cache/cmd/demo && cd weak-value-cache
go mod init example.com/weak-value-cache
go mod edit -go=1.26
```

### Why a weak map, and the honesty it demands of the reader

A normal `map[K]*V` cache leaks by construction: the map's `*V` is a strong reference, so every value it has ever loaded stays reachable and the GC can never reclaim it, even long after the rest of the program has dropped the value. Storing `map[K]weak.Pointer[V]` removes exactly that pin. `weak.Make(v)` records the value without retaining it, so once the last *strong* reference outside the cache goes away, the value is collectible even though the cache still has an entry for it.

That power costs the reader one piece of honesty. `wp.Value()` returns the live `*V` *or* `nil` if the value was already collected, so `Get` must treat a `nil` as a miss and reload rather than handing back a dead pointer. This produces a clean split of responsibilities that runs through the whole topic: the weak pointer provides *correctness* — a collected entry is never mistaken for a live value — and the cleanup registered here provides only *memory hygiene*, eventually deleting the empty slot. The cache is still correct if the cleanup never runs; it would just accumulate empty slots.

Walk the three states `Get` handles. A live hit: the key is present and `wp.Value()` is non-nil, so the same `*V` is returned and a second caller asking for the same key observes the same pointer. A stale hit: the key is present but `wp.Value()` is `nil` because the value was collected before the pruning cleanup ran — `Get` falls through and reloads. A miss: the key is absent, so `Get` loads, wraps the new value with `weak.Make`, and registers the pruning cleanup. The cleanup's `arg` is the `key`, never the value, because passing the value would keep it alive forever and defeat the entire design (and `AddCleanup` panics if `arg == ptr`).

The pruning cleanup, `deleteStale`, has one subtlety: it deletes the entry only if it is *still* stale. Between the value's collection and the cleanup actually running, a concurrent `Get` may have already reloaded the key and stored a fresh, live weak pointer; deleting unconditionally would throw that live entry away. So `deleteStale` re-checks `wp.Value() == nil` under the lock before deleting.

Create `weakcache.go`:

```go
package weakcache

import (
	"runtime"
	"sync"
	"weak"
)

// Cache returns shared *V values keyed by K. It holds values weakly, so a value
// the rest of the program no longer references is reclaimed by the GC and its
// map entry is removed by a cleanup. The cache itself never keeps a value alive.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]weak.Pointer[V]
}

func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]weak.Pointer[V])}
}

// Get returns the shared value for key, creating it with load if no live value
// exists. While at least one caller keeps the returned *V alive, later callers
// asking for the same key observe the same pointer.
func (c *Cache[K, V]) Get(key K, load func() V) *V {
	c.mu.Lock()
	defer c.mu.Unlock()

	if wp, ok := c.items[key]; ok {
		if v := wp.Value(); v != nil {
			return v
		}
		// Stale entry: the value was collected. Fall through and reload.
	}

	v := new(V)
	*v = load()
	c.items[key] = weak.Make(v)
	runtime.AddCleanup(v, c.deleteStale, key)
	return v
}

// deleteStale removes key's entry, but only if it is still stale. A concurrent
// Get may have already replaced it with a fresh, live value.
func (c *Cache[K, V]) deleteStale(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wp, ok := c.items[key]; ok && wp.Value() == nil {
		delete(c.items, key)
	}
}

// Len reports the number of map entries, including any not-yet-pruned stale ones.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
```

### The runnable demo

A test proves a property in the abstract; a demo makes it concrete. This one loads a one-megabyte value under a key, asks for the same key again to show the value is shared (and `load` did not run a second time), then drops both strong references, forces a GC, and asks once more to show the value was collected and had to be reloaded.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"runtime"

	weakcache "example.com/weak-value-cache"
)

func main() {
	c := weakcache.New[string, []byte]()
	loads := 0
	load := func() []byte { loads++; return make([]byte, 1<<20) }

	a := c.Get("image", load)
	b := c.Get("image", load)
	fmt.Printf("shared while alive: %t, loads=%d\n", a == b, loads)

	a, b = nil, nil
	runtime.GC()

	c.Get("image", load) // the old value was collected, so this reloads
	fmt.Printf("loads after collection: %d\n", loads)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shared while alive: true, loads=1
loads after collection: 2
```

### Tests

Garbage collection is asynchronous, which makes "the value was collected" look hard to test — but it is testable if you assert on a *deterministic consequence* rather than on timing. After dropping the only strong reference, the tests call `runtime.GC()` and then observe something that must now be true: `load` runs a second time, or — by polling `runtime.GC()` with a timeout — the cleanup eventually prunes the map entry. Every collection test uses a non-tiny, pointer-containing `[]byte` value, never a bare `int`, because the runtime may batch tiny pointer-free objects so they never become individually collectible. `TestConcurrentGet` runs under `-race`.

Create `weakcache_test.go`:

```go
package weakcache

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"
)

func TestSharedWhileAlive(t *testing.T) {
	t.Parallel()

	c := New[string, int]()
	loads := 0
	load := func() int { loads++; return 42 }

	a := c.Get("k", load)
	b := c.Get("k", load)

	if a != b {
		t.Fatal("same key must return the same pointer while alive")
	}
	if loads != 1 {
		t.Fatalf("load ran %d times, want 1 (second Get reused the live value)", loads)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

func TestReloadsAfterCollection(t *testing.T) {
	t.Parallel()

	// Use a non-tiny, pointer-containing value ([]byte). The runtime may batch
	// tiny pointer-free objects (such as a bare int) so they never become
	// individually collectible, which would make weak-nil-ing and cleanups
	// unreliable -- the docs warn about exactly this for weak.Pointer/AddCleanup.
	c := New[string, []byte]()
	loads := 0
	load := func() []byte { loads++; return make([]byte, 1024) }

	p := c.Get("k", load)
	if len(*p) != 1024 {
		t.Fatalf("len(*p) = %d, want 1024", len(*p))
	}
	p = nil // drop the only strong reference

	runtime.GC()

	if c.Get("k", load); loads != 2 {
		t.Fatalf("load ran %d times, want 2 (value was collected and reloaded)", loads)
	}
}

func TestCleanupPrunesStaleEntry(t *testing.T) {
	t.Parallel()

	c := New[string, []byte]()
	p := c.Get("k", func() []byte { return make([]byte, 1024) })
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	runtime.KeepAlive(p)
	p = nil

	deadline := time.Now().Add(2 * time.Second)
	for c.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("cleanup did not prune the stale entry; Len = %d", c.Len())
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

func TestTwoKeysCoexist(t *testing.T) {
	t.Parallel()

	c := New[string, []byte]()
	a := c.Get("a", func() []byte { return make([]byte, 1024) })
	b := c.Get("b", func() []byte { return make([]byte, 1024) })
	if a == nil || b == nil {
		t.Fatal("both keys must load a value")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

func TestWeakPointerIdentity(t *testing.T) {
	t.Parallel()

	x := new(int)
	y := new(int)
	if weak.Make(x) != weak.Make(x) {
		t.Fatal("weak pointers to the same object must be equal")
	}
	if weak.Make(x) == weak.Make(y) {
		t.Fatal("weak pointers to different objects must differ")
	}
	if weak.Make(x).Value() != x {
		t.Fatal("Value() must return the original pointer while alive")
	}
	runtime.KeepAlive(x)
	runtime.KeepAlive(y)
}

func TestConcurrentGet(t *testing.T) {
	t.Parallel()

	c := New[string, int]()
	var wg sync.WaitGroup
	got := make([]*int, 50)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = c.Get("k", func() int { return 1 })
		}()
	}
	wg.Wait()

	for i, p := range got {
		if p == nil || *p != 1 {
			t.Fatalf("got[%d] = %v", i, p)
		}
	}
	runtime.KeepAlive(got)
}

func Example() {
	c := New[string, int]()
	loads := 0
	load := func() int { loads++; return 99 }

	a := c.Get("k", load)
	b := c.Get("k", load) // reuses the live value; load does not run again

	fmt.Println(a == b, *a, loads)

	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
	// Output:
	// true 99 1
}
```

## Review

The cache is correct when `Get` treats the three states honestly: a live `Value()` is returned and shared, a `nil` `Value()` (a stale entry) falls through to a reload, and a miss loads and registers the pruning cleanup. Confirm the cleanup's `arg` is the `key` and never the value — passing the value would pin it alive forever and `AddCleanup` would panic on `arg == ptr` — and that `deleteStale` re-checks `wp.Value() == nil` under the lock so a key a concurrent `Get` has just reloaded is not deleted out from under it. The collection tests make the asynchronous GC observable by asserting a deterministic consequence after `runtime.GC()` (a second `load`, or a pruned entry under a polling deadline), and they all use `[]byte` values so a dropped reference is really collectible.

Common mistakes for this feature. The first is using a strong `map[K]*V` and wondering why memory only grows: the map's pointer is the leak. The second is trusting a weak hit without a `nil` check — `Value()` can return `nil` for a collected value, so a missing check hands back a dead pointer. The third is making the object reachable from its own cleanup by capturing it or passing it as `arg`, which keeps it alive forever so the cleanup never fires. The fourth is testing collection with a tiny pointer-free value such as a bare `int`, which the runtime may never collect individually, making the test flake; use a kilobyte `[]byte`.

## Resources

- [`weak` package](https://pkg.go.dev/weak) — `weak.Make`, `weak.Pointer[T]`, and `Value()`, including the note that tiny objects may never be reclaimed.
- [`runtime.AddCleanup`](https://pkg.go.dev/runtime#AddCleanup) — the cleanup registration the cache uses to prune dead entries, and its reachability rules.
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — the release that introduced the `weak` package and `runtime.AddCleanup`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-cleanup-backed-resource.md](02-cleanup-backed-resource.md)
