# Exercise 3: A Concurrency-Safe TTL Cache — Named vs Embedded Mutex

A TTL cache is a staple of every backend: a concurrency-safe map whose entries
expire. This exercise builds one guarded by an *unexported named* `sync.RWMutex`
field, and contrasts it deliberately with the anti-pattern of *embedding*
`sync.Mutex`, which promotes `Lock`/`Unlock` onto the cache's public API and lets
any caller corrupt its invariants.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ttlcache/                   independent module: example.com/ttlcache
  go.mod                    module example.com/ttlcache
  ttlcache.go               Cache[K,V] with an unexported RWMutex; Get, Set, Delete, Len
  cmd/
    demo/
      main.go               set, read, delete; print hits and misses
  ttlcache_test.go          TTL expiry via injected clock, -race concurrency, no-promoted-Lock
```

Files: `ttlcache.go`, `cmd/demo/main.go`, `ttlcache_test.go`.
Implement: `Cache[K comparable, V any]` with `New`, `Set(key, value, ttl)`,
`Get(key) (V, bool)`, `Delete(key)`, and `Len()`, all guarded by an *unexported*
`sync.RWMutex` field.
Test: `Get`/`Set`/`Delete` behavior and TTL expiry driven by an injectable clock;
a `-race` test with many concurrent readers and writers; a reflection test proving
the exported `Cache` type does *not* expose a promoted `Lock` method.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ttlcache/cmd/demo
cd ~/go-exercises/ttlcache
go mod init example.com/ttlcache
```

### Why the mutex is a named field, not embedded

It is tempting to embed the lock:

```text
type Cache struct {
	sync.RWMutex        // embedded: promotes Lock, Unlock, RLock, RUnlock
	items map[string]entry
}
```

That compiles, and inside the package it reads slightly shorter (`c.Lock()`
instead of `c.mu.Lock()`). But `Cache` is *exported*, and embedding promotes the
lock's exported methods onto `Cache`'s public API. Now any code in any package can
call `cache.Lock()` and `cache.Unlock()` — and hold the lock across an arbitrary
call, unlock it from the wrong goroutine, or deadlock the cache — with no way for
the cache to stop them. The lock is supposed to be an implementation detail that
enforces the cache's invariants; promoting it hands that control to strangers.

The rule: on an exported type, make the mutex an *unexported named field*
(`mu sync.RWMutex`). It stays private, callers cannot touch it, and the reflection
test below pins that `Cache` exposes no `Lock` method. (Embedding a lock is only
acceptable on an *unexported* type, where the promoted methods are invisible
outside the package anyway.) Using `RWMutex` rather than `Mutex` lets concurrent
`Get`s share a read lock while `Set`/`Delete` take the exclusive write lock — the
right trade-off for a read-heavy cache.

### Why the clock is injectable here

Unlike a `synctest`-based lesson, this module tests TTL expiry the portable way:
the cache reads the time through an unexported `now func() time.Time` field that
defaults to `time.Now`. Production uses the real clock; the test swaps in a
controllable one and advances it by hand, so expiry is asserted deterministically
with no sleeping and no flakiness. Expiry is *lazy*: `Get` reports an entry as
absent once the clock is no longer before its deadline, but does not delete it —
`Len` counts stored entries whether or not they have expired. Keeping the field
unexported means only same-package tests can reach it; the public API stays clean.

Create `ttlcache.go`:

```go
package ttlcache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a concurrency-safe TTL map. The lock is an UNEXPORTED NAMED field, so
// Lock/Unlock are not promoted onto the exported Cache's public API.
type Cache[K comparable, V any] struct {
	mu    sync.RWMutex
	items map[K]entry[V]
	now   func() time.Time
}

// New returns an empty cache reading the real wall clock.
func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]entry[V]), now: time.Now}
}

// Set stores value under key, expiring it ttl from now.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: c.now().Add(ttl)}
}

// Get returns the value if present and unexpired. Expiry is lazy.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok || !c.now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Delete removes key if present.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// Len reports the number of stored entries, expired or not.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
```

### The runnable demo

The demo stores a session token, reads it back, deletes it, and reads again to
show the miss — no timing involved, so the output is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	c := ttlcache.New[string, string]()
	c.Set("token", "alice", time.Minute)

	if v, ok := c.Get("token"); ok {
		fmt.Printf("hit: %s (len=%d)\n", v, c.Len())
	}

	c.Delete("token")

	if _, ok := c.Get("token"); !ok {
		fmt.Printf("miss after delete (len=%d)\n", c.Len())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit: alice (len=1)
miss after delete (len=0)
```

### Tests

`TestExpiryWithInjectedClock` sets an entry with a one-minute TTL, advances the
injected clock past it, and asserts the entry is gone — deterministic, no sleep.
`TestConcurrentAccess` hammers the cache from many goroutines under `-race` to
prove the lock actually guards the map. `TestNoPromotedLockMethod` uses reflection
to assert that `Cache` exposes no `Lock` method, documenting that the lock is a
private field rather than an embedded one.

Create `ttlcache_test.go`:

```go
package ttlcache

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestSetGetDelete(t *testing.T) {
	t.Parallel()

	c := New[string, int]()
	c.Set("a", 1, time.Minute)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %d,%v; want 1,true", v, ok)
	}
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get(a) still present after Delete")
	}
}

func TestExpiryWithInjectedClock(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC)
	c := New[string, int]()
	c.now = func() time.Time { return now } // same-package access to the hook

	c.Set("a", 1, time.Minute)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("entry missing immediately after Set")
	}

	now = now.Add(2 * time.Minute) // advance past the TTL
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry still present after TTL expired")
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d; expiry is lazy, want 1", c.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	c := New[int, int]()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(i%16, i, time.Minute)
			c.Get(i % 16)
			if i%3 == 0 {
				c.Delete(i % 16)
			}
		}()
	}
	wg.Wait()
}

func TestNoPromotedLockMethod(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(New[string, int]())
	if _, ok := typ.MethodByName("Lock"); ok {
		t.Fatal("Cache must not expose a promoted Lock method; the mutex must be an unexported field")
	}
}
```

## Review

The cache is correct when the exported API is exactly `New`, `Set`, `Get`,
`Delete`, `Len` — no `Lock`/`Unlock` leaking out — and when expiry is a pure
function of the injected clock and the stored deadline. The central lesson is the
one the reflection test pins: on an exported type, embed nothing you would not
want on the public API, and a mutex is top of that list. Other mistakes to avoid:
taking the write lock in `Get` (a read lock lets concurrent reads proceed);
forgetting that `Len` counts expired entries (lazy expiry is deliberate); and
reaching for `time.Now()` directly in a way the test cannot control. Run
`go test -race` — the concurrency test is the proof the lock does its job.

## Resources

- [sync: RWMutex](https://pkg.go.dev/sync#RWMutex) — read/write locking semantics.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields and method promotion (why embedding a mutex promotes its methods).
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — when embedding helps and when a named field is clearer.

---

Prev: [02-responsewriter-status-capture.md](02-responsewriter-status-capture.md) | Back to [00-concepts.md](00-concepts.md) | Next: [04-base-repository-embedding.md](04-base-repository-embedding.md)
