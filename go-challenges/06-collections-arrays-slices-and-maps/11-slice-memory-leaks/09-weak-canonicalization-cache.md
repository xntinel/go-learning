# Exercise 9: A Canonicalizing Cache That Does Not Keep Large Values Alive

A service that repeatedly parses the same expensive objects — compiled configs,
compiled regexes, decoded schemas — wants to share one parsed instance per key
while it is in use, and let it be freed once nothing uses it. A plain
`map[string]*T` cache cannot: its pointer is a strong reference, so every value it
ever parsed stays alive forever. This module builds the cache out of
`weak.Pointer[T]` (Go 1.24), so an entry never keeps its value alive, with
`runtime.AddCleanup` pruning the dead key afterward — and contrasts it with the
strong-map cache that leaks.

## What you'll build

```text
canoncache/                  independent module: example.com/canoncache
  go.mod                     go 1.24
  canoncache.go              type Compiled, Cache (weak), StrongCache; New, Get, Len
  cmd/
    demo/
      main.go                canonicalize a key: two Gets return the same instance
  canoncache_test.go         shared-while-alive, reloads-after-GC, cleanup-evicts, strong-leaks; -race
```

Files: `canoncache.go`, `cmd/demo/main.go`, `canoncache_test.go`.
Implement: `Cache[K,V]` storing `map[K]weak.Pointer[V]`, `Get` returning a shared `*V`, `runtime.AddCleanup` evicting the dead key; a `StrongCache` for contrast.
Test: assert a live value is shared, a collected value is reloaded and its key evicted, and the strong cache pins its value across GC.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

## Weak pointers plus a cleanup

The cache stores `map[K]weak.Pointer[V]`. `weak.Make(v)` records a reference to the
parsed value without keeping it alive, so once the last *strong* reference outside
the cache is dropped, the value becomes collectible even though the cache still has
an entry for it. That is exactly the property a strong `map[K]*V` lacks — its
pointer pins the value forever.

The weak reference costs the reader one honest check. `wp.Value()` returns the live
`*V` or `nil` if the value was already collected, so `Get` handles three states.
A live hit: the key is present and `Value()` is non-`nil`, so the same `*V` is
returned and two callers asking for the same key while it is alive observe the same
instance — the canonicalization guarantee. A stale hit: the key is present but
`Value()` is `nil` because the value was collected before the pruning cleanup ran,
so `Get` falls through and reloads. A miss: the key is absent, so `Get` loads,
wraps the value with `weak.Make`, and registers the cleanup.

`runtime.AddCleanup(v, c.evict, key)` does the memory hygiene the weak pointer
cannot: when `v` is reclaimed, it deletes the now-dead key so the map does not fill
with empty slots. Two rules make it correct. The `arg` is the `key`, never the
value — passing the value would keep it alive forever and defeat the whole design
(and `AddCleanup` panics if `arg == ptr`). And `evict` re-checks `Value() == nil`
under the lock before deleting, because between the value's collection and the
cleanup running a concurrent `Get` may have reloaded the key with a fresh, live
weak pointer that must not be thrown away.

The division of labor to carry away: the weak pointer provides *correctness* — a
collected entry is never mistaken for a live value — and the cleanup provides only
*memory hygiene*, eventually removing empty slots. The cache is still correct if
the cleanup never runs; it would just accumulate empty entries. This is not a
lifetime guarantee: if a value *must* stay alive, hold a strong reference.

The cached `Compiled` carries a kilobyte `Data` field so it is a non-tiny,
pointer-containing allocation — the runtime may batch tiny pointer-free objects so
they never become individually collectible, which would make the GC tests flake.

Create `canoncache.go`:

```go
// Package canoncache canonicalizes expensive parsed values with a weak-pointer
// cache that never extends their lifetime.
package canoncache

import (
	"runtime"
	"sync"
	"weak"
)

// Compiled stands in for an expensive parsed object (a compiled config or regex).
// The Data field makes it a non-tiny, pointer-containing allocation.
type Compiled struct {
	Name string
	Data []byte
}

// Cache canonicalizes values by key without keeping them alive.
type Cache[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]weak.Pointer[V]
	load  func(K) *V
}

// New returns a Cache that parses missing keys with load.
func New[K comparable, V any](load func(K) *V) *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]weak.Pointer[V]), load: load}
}

// Get returns the canonical *V for key, parsing it if absent or already collected.
func (c *Cache[K, V]) Get(key K) *V {
	c.mu.Lock()
	defer c.mu.Unlock()

	if wp, ok := c.items[key]; ok {
		if v := wp.Value(); v != nil {
			return v // live hit: shared instance
		}
	}

	v := c.load(key) // miss or stale: parse
	c.items[key] = weak.Make(v)
	runtime.AddCleanup(v, c.evict, key) // arg is key, never v
	return v
}

// evict deletes key once its value has been collected, unless a concurrent Get has
// already reloaded a live value under it.
func (c *Cache[K, V]) evict(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wp, ok := c.items[key]; ok && wp.Value() == nil {
		delete(c.items, key)
	}
}

// Len reports the number of entries (live or not-yet-pruned).
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// StrongCache is the leaky contrast: it stores *V directly, so every value it ever
// loads stays reachable forever.
type StrongCache[K comparable, V any] struct {
	mu    sync.Mutex
	items map[K]*V
	load  func(K) *V
}

// NewStrong returns a StrongCache backed by a plain map[K]*V.
func NewStrong[K comparable, V any](load func(K) *V) *StrongCache[K, V] {
	return &StrongCache[K, V]{items: make(map[K]*V), load: load}
}

// Get returns the *V for key, loading and pinning it forever if absent.
func (c *StrongCache[K, V]) Get(key K) *V {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.items[key]; ok {
		return v
	}
	v := c.load(key)
	c.items[key] = v
	return v
}

// Len reports the number of entries.
func (c *StrongCache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
```

## The runnable demo

The demo compiles a config once and shows that a second `Get` for the same key
returns the identical instance (canonicalization) without recompiling, while a
different key compiles separately.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/canoncache"
)

func main() {
	compiles := 0
	c := canoncache.New(func(name string) *canoncache.Compiled {
		compiles++
		return &canoncache.Compiled{Name: name, Data: make([]byte, 1024)}
	})

	a := c.Get("routes.yaml")
	b := c.Get("routes.yaml") // same key, still alive: no recompile
	d := c.Get("limits.yaml")

	fmt.Println("same instance for same key:", a == b)
	fmt.Println("different instance for different key:", a != d)
	fmt.Println("compiles:", compiles)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
same instance for same key: true
different instance for different key: true
compiles: 2
```

## Tests

Garbage collection is asynchronous, so the collection tests assert on a
deterministic consequence after `runtime.GC()`: the value is reloaded, or the key
is pruned under a bounded polling deadline. Every value is a kilobyte `Compiled`
so a dropped reference is genuinely collectible. The strong-cache test proves the
leak by keeping a weak witness on a value the strong cache holds and showing it
survives GC.

Create `canoncache_test.go`:

```go
package canoncache

import (
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"
)

func newCompiled(name string) *Compiled {
	return &Compiled{Name: name, Data: make([]byte, 1024)}
}

func TestSharedWhileAlive(t *testing.T) {
	t.Parallel()

	loads := 0
	c := New(func(k string) *Compiled { loads++; return newCompiled(k) })

	a := c.Get("k")
	b := c.Get("k")
	if a != b {
		t.Fatal("same key while alive must return the same instance")
	}
	if loads != 1 {
		t.Fatalf("load ran %d times, want 1", loads)
	}
	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

func TestReloadsAfterCollection(t *testing.T) {
	loads := 0
	c := New(func(k string) *Compiled { loads++; return newCompiled(k) })

	p := c.Get("k")
	if p.Name != "k" {
		t.Fatalf("Name = %q, want k", p.Name)
	}
	p = nil // drop the only strong reference

	runtime.GC()

	c.Get("k")
	if loads != 2 {
		t.Fatalf("load ran %d times, want 2 (value was collected and reloaded)", loads)
	}
}

func TestCleanupEvictsKey(t *testing.T) {
	c := New(func(k string) *Compiled { return newCompiled(k) })

	p := c.Get("k")
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1", c.Len())
	}
	runtime.KeepAlive(p)
	p = nil

	deadline := time.Now().Add(2 * time.Second)
	for c.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("cleanup did not prune the dead key; Len = %d", c.Len())
		}
		runtime.GC()
		time.Sleep(time.Millisecond)
	}
}

func TestStrongCacheLeaks(t *testing.T) {
	c := NewStrong(func(k string) *Compiled { return newCompiled(k) })

	p := c.Get("k")
	wp := weak.Make(p)
	p = nil // drop our reference; the strong cache still holds one

	runtime.GC()
	runtime.GC()
	if wp.Value() == nil {
		t.Fatal("strong cache value was collected; it should be pinned forever")
	}
	runtime.KeepAlive(c)
}

func TestConcurrentGet(t *testing.T) {
	t.Parallel()

	c := New(func(k string) *Compiled { return newCompiled(k) })
	var wg sync.WaitGroup
	got := make([]*Compiled, 50)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = c.Get("k")
		}()
	}
	wg.Wait()
	for i := range got {
		runtime.KeepAlive(got[i])
	}
}
```

## Review

The cache is correct when `Get` treats the three states honestly: a live `Value()`
is returned and shared, a `nil` `Value()` falls through to a reload, and a miss
loads and registers the cleanup. `TestReloadsAfterCollection` and
`TestCleanupEvictsKey` make the asynchronous GC observable by asserting a
deterministic consequence after `runtime.GC()`, and `TestStrongCacheLeaks` shows
the plain `map[K]*V` pinning its value forever. The mistakes this module exists to
prevent are storing `map[K]*V` when you meant "cache while in use" (a permanent
leak), passing the value as the cleanup `arg` (which pins it and panics on
`arg == ptr`), and reaching for `runtime.SetFinalizer` — prefer `AddCleanup`, which
cannot resurrect the object and works on cycles. Run `go test -race` to confirm the
map is safe under concurrent `Get`.

## Resources

- [`weak` package](https://pkg.go.dev/weak) — `weak.Make`, `weak.Pointer[T]`, and `Value()`, with the note that tiny objects may never be reclaimed.
- [`runtime.AddCleanup`](https://pkg.go.dev/runtime#AddCleanup) — the cleanup registration and its reachability rules.
- [Go 1.24 release notes](https://go.dev/doc/go1.24) — the release that introduced the `weak` package and `runtime.AddCleanup`.

---

Back to [08-three-index-frame-window.md](08-three-index-frame-window.md) | Next: [10-audit-hook-closure-buffer-pin.md](10-audit-hook-closure-buffer-pin.md)
