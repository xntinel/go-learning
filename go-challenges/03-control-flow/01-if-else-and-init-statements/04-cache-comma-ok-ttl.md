# Exercise 4: TTL Cache Get: The Comma-Ok Idiom Under a Mutex

A per-instance read-through cache is one of the most common pieces of a backend
service, and its `Get` is the canonical comma-ok decision: fold the map lookup and
the expiry test into one scoped `if`. This module builds that cache with an injected
clock so expiry is deterministic in tests, and an `RWMutex` so it is safe under
concurrent request handlers.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
ttlcache/                   independent module: example.com/ttlcache
  go.mod                    go 1.26
  cache.go                  Cache[K,V]: New(clock), Get, Set, Len; comma-ok + expiry
  cmd/
    demo/
      main.go               set, read, advance clock, read again
  cache_test.go             hit/miss/expiry via injected clock; -race concurrency
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: `Cache[K comparable, V any]` with `New(now func() time.Time)`, `Get(key) (V, bool)`, `Set(key, val, ttl)`, `Len()`; `Get` uses the comma-ok read plus an inline expiry check and lazily deletes a stale entry.
- Test: hit before expiry, miss after expiry (advance an injected clock), miss on unknown key, overwrite via `Set`, lazy deletion shrinking `Len`; a concurrent `-race` test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ttlcache/cmd/demo
cd ~/go-exercises/ttlcache
go mod init example.com/ttlcache
```

## Get is the comma-ok decision, and it is lazy

The heart of the exercise is one line of control flow:

```go
if e, ok := m[key]; !ok || now.After(e.expiresAt) {
	return zero, false
}
```

Two conditions fold into one scoped `if`: the comma-ok read (`ok` is false for a
key that was never stored) and the expiry test (a stored entry whose deadline has
passed is treated as a miss). A caller cannot tell the two apart, and should not
have to — both mean "not usable, go fetch it". Because `e` and `ok` are scoped to
the `if`, they cannot leak into the rest of the method.

Expiry here is *lazy with cleanup*: when `Get` finds a stale entry it deletes it
before returning the miss, so a key that is read after expiry stops occupying memory.
That is why `Len` shrinks after an expired `Get` — the test asserts exactly this.
(Contrast a purely lazy cache that never deletes; that one needs a background
janitor to reclaim memory. Deleting on read is the simplest reclamation and is
correct as long as the delete happens inside the same critical section as the read.)

The clock is injected as `now func() time.Time` rather than calling `time.Now`
inline. That is the seam that makes expiry deterministic: the test advances a
variable and the cache "sees" time move, with no sleeping and no flakiness. In
production you pass `time.Now`.

Concurrency: `Set` and the mutating `Get` take the write lock (`Lock`), because both
can modify the map — `Get` deletes on expiry. `Len` takes the read lock (`RLock`).
The critical rule is that the comma-ok read and the possible delete are inside the
same `Lock`/`Unlock`, so two goroutines cannot both observe the stale entry and race
on the delete.

Create `cache.go`:

```go
package ttlcache

import (
	"sync"
	"time"
)

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

// Cache is a concurrency-safe TTL cache. It reads time through an injected clock
// so expiry is deterministic in tests.
type Cache[K comparable, V any] struct {
	mu    sync.RWMutex
	now   func() time.Time
	items map[K]entry[V]
}

// New returns a cache that reads the clock through now (pass time.Now in prod).
func New[K comparable, V any](now func() time.Time) *Cache[K, V] {
	return &Cache[K, V]{now: now, items: make(map[K]entry[V])}
}

// Set stores value under key, expiring it ttl from the current clock reading.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expiresAt: c.now().Add(ttl)}
}

// Get returns the live value for key, or (zero, false) if the key is absent or
// expired. A stale entry is deleted so its memory is reclaimed.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; !ok || c.now().After(e.expiresAt) {
		if ok {
			delete(c.items, key)
		}
		var zero V
		return zero, false
	}
	return c.items[key].value, true
}

// Len reports how many entries are stored right now.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
```

### The runnable demo

The demo uses a mutable clock variable so you can watch an entry expire without
sleeping: store a session for 30s, read it, jump the clock forward a minute, read
again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := ttlcache.New[string, string](func() time.Time { return clock })

	c.Set("session:alice", "token-abc", 30*time.Second)
	if v, ok := c.Get("session:alice"); ok {
		fmt.Printf("before expiry: %s (len=%d)\n", v, c.Len())
	}

	clock = clock.Add(time.Minute) // advance past the TTL
	if _, ok := c.Get("session:alice"); !ok {
		fmt.Printf("after expiry: miss (len=%d)\n", c.Len())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before expiry: token-abc (len=1)
after expiry: miss (len=0)
```

The first line reads `len=1` before any expiry. The second `Get` finds a stale
entry and deletes it inside its critical section before returning the miss, so by
the time the demo reads `Len` on the second line it is already 0 — proof the lazy
delete fired. If you see `len=1` there, the stale read is not reclaiming memory.

### Tests

The tests drive time through a pointer to a clock variable so each subtest controls
its own instant. They cover a hit before expiry, a miss after advancing past it,
a miss on an unknown key, an overwrite through `Set`, lazy deletion (asserting `Len`
shrinks after an expired `Get`), and a concurrent hammer under `-race`.

Create `cache_test.go`:

```go
package ttlcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// clock returns a now-func reading *t, so a test can advance time by writing *t.
func clock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestGetHitBeforeExpiry(t *testing.T) {
	t.Parallel()
	nowT := time.Unix(0, 0)
	c := New[string, int](clock(&nowT))
	c.Set("k", 7, time.Minute)
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Get(k) = %d,%v; want 7,true", v, ok)
	}
}

func TestGetMissAfterExpiry(t *testing.T) {
	t.Parallel()
	nowT := time.Unix(0, 0)
	c := New[string, int](clock(&nowT))
	c.Set("k", 7, time.Second)
	nowT = nowT.Add(2 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get(k) present after TTL expired")
	}
}

func TestGetMissUnknownKey(t *testing.T) {
	t.Parallel()
	nowT := time.Unix(0, 0)
	c := New[string, int](clock(&nowT))
	if _, ok := c.Get("nope"); ok {
		t.Fatal("Get(nope) present; want miss")
	}
}

func TestSetOverwrites(t *testing.T) {
	t.Parallel()
	nowT := time.Unix(0, 0)
	c := New[string, int](clock(&nowT))
	c.Set("k", 1, time.Minute)
	c.Set("k", 2, time.Minute)
	if v, _ := c.Get("k"); v != 2 {
		t.Fatalf("Get(k) = %d; want 2 after overwrite", v)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d; want 1", c.Len())
	}
}

func TestExpiredGetReclaimsMemory(t *testing.T) {
	t.Parallel()
	nowT := time.Unix(0, 0)
	c := New[string, int](clock(&nowT))
	c.Set("k", 1, time.Second)
	if c.Len() != 1 {
		t.Fatalf("Len = %d before expiry; want 1", c.Len())
	}
	nowT = nowT.Add(2 * time.Second)
	c.Get("k") // stale read deletes the entry
	if c.Len() != 0 {
		t.Fatalf("Len = %d after expired Get; want 0 (lazy delete)", c.Len())
	}
}

func TestConcurrentSetGet(t *testing.T) {
	t.Parallel()
	nowT := time.Unix(0, 0)
	c := New[int, int](clock(&nowT))
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(i%16, i, time.Minute)
			c.Get(i % 16)
		}()
	}
	wg.Wait()
}

func Example() {
	nowT := time.Unix(0, 0)
	c := New[string, int](func() time.Time { return nowT })
	c.Set("answer", 42, time.Minute)
	v, ok := c.Get("answer")
	fmt.Println(v, ok)
	// Output: 42 true
}
```

## Review

The cache is correct when `Get` returns `(zero, false)` exactly when the key is
absent or the clock is past its `expiresAt`, and when a stale read deletes the entry
inside the same critical section — so `Len` shrinks and no goroutine races on the
delete. The injected clock is what makes expiry deterministic: a test that sleeps to
force expiry is slow and flaky; advancing a variable is instant and exact. The
mistakes to avoid are calling `time.Now` inline (untestable), doing the comma-ok read
and the delete in separate lock scopes (a race), and treating an expired entry as a
hit. Run `go test -race` to confirm the mutex actually guards the map.

## Resources

- [Go maps in action: the comma-ok idiom](https://go.dev/blog/maps)
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex)
- [time.Time.After](https://pkg.go.dev/time#Time.After)
- [Effective Go: if](https://go.dev/doc/effective_go#if)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-env-config-loader.md](03-env-config-loader.md) | Next: [05-retry-classifier.md](05-retry-classifier.md)
