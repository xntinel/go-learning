# Exercise 4: TTL cache with a FakeClock — expiry at the exact edge and a janitor sweep

An in-memory TTL cache backs sessions, rate-limit counters, and hot-key lookups
in nearly every backend. Its two time-dependent behaviors are expiry (a `Get`
after the TTL must miss) and eviction (expired entries must eventually free their
memory). Both hinge on a `<` vs `<=` decision at the exact expiry instant. This
exercise injects a `Clock`, pins the boundary at `TTL-1ns`/`TTL`/`TTL+1ns`, and
proves a `Sweep` janitor reclaims exactly the expired keys.

## What you'll build

```text
ttlcache/                      independent module: example.com/ttlcache
  go.mod
  cache.go                     Clock (Now); RealClock; FakeClock; Cache (Set, Get, Len, Sweep)
  cmd/
    demo/
      main.go                  set entries, advance a fake clock past TTL, sweep, print sizes
  cache_test.go                boundary expiry, sweep evicts exactly expired, -race
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: a concurrency-safe `Cache` (RWMutex) storing an expiry instant per key from an injected `Clock`; `Set(k,v,ttl)`, `Get(k) (V,bool)` with lazy expiry, `Len()`, and `Sweep()` that evicts expired keys.
Test: inject `FakeClock` — `Get` hits at `TTL-1ns`, misses at `TTL` and `TTL+1ns`; advancing past several TTLs and calling `Sweep` evicts exactly the expired keys and leaves live ones; `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/04-ttl-cache-expiry/cmd/demo
cd go-solutions/12-testing-ecosystem/16-testing-time-dependent-code/04-ttl-cache-expiry
```

### Lazy expiry vs. the janitor, and the boundary

There are two distinct notions here and they must not be conflated. *Lazy expiry*
is what `Get` does: it treats an entry as absent once the clock is no longer before
the stored `expires` instant, but it does not delete it. *Eviction* is what
`Sweep` does: it walks the map and deletes every entry whose deadline has passed,
freeing memory. `Len` counts *stored* entries — expired-but-not-yet-swept ones
included — because that is the honest size of the map until the janitor runs.
Keeping these separate is what lets a test assert both "a `Get` after TTL misses"
(lazy) and "memory is reclaimed only after `Sweep`" (eviction) independently.

Why inject a `Clock` here rather than reach for `synctest`? Because with an
injected clock the janitor is an explicit `Sweep()` the test calls at a chosen
instant, giving a fully deterministic eviction assertion with no goroutine at all.
(The next exercise takes the opposite approach — a real ticker under `synctest` —
so you see both.) The `Sweep` design also matches real caches where a background
goroutine calls the same sweep method on a ticker; testing the method directly
decouples the eviction logic from the scheduling.

The boundary is the whole game. An entry set with TTL `d` at time `t0` stores
`expires = t0.Add(d)`. `Get` returns it when `clock.Now().Before(expires)`. At
`t0+d-1ns` the clock is before `expires`, so `Get` hits. At `t0+d` the clock
equals `expires`, `Before` is false, so `Get` misses — expiry is *inclusive* of
the deadline instant. Pinning all three of `d-1ns`, `d`, `d+1ns` is what kills a
`<=` vs `<` off-by-one that "advance a while and check it's gone" would never
catch.

Create `cache.go`:

```go
package ttlcache

import (
	"sync"
	"time"
)

// Clock is the minimal time surface the cache reads.
type Clock interface {
	Now() time.Time
}

// RealClock forwards to the standard library.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a test clock advanced by hand, safe for concurrent use.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a concurrency-safe TTL map. Get expires lazily; Sweep evicts. Time is
// read through the injected Clock.
type Cache[K comparable, V any] struct {
	mu    sync.RWMutex
	clock Clock
	items map[K]entry[V]
}

func New[K comparable, V any](clock Clock) *Cache[K, V] {
	return &Cache[K, V]{clock: clock, items: make(map[K]entry[V])}
}

// Set stores value under key, expiring it ttl from the clock's current instant.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: c.clock.Now().Add(ttl)}
}

// Get returns the value if present and the clock is still before its expiry.
// Expiry is inclusive of the deadline instant: at exactly expires, Get misses.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.items[key]
	if !ok || !c.clock.Now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Len reports the number of stored entries, expired or not.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Sweep evicts every entry whose deadline has passed and returns the count
// removed. This is the janitor a background ticker would call.
func (c *Cache[K, V]) Sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock.Now()
	removed := 0
	for k, e := range c.items {
		if !now.Before(e.expires) {
			delete(c.items, k)
			removed++
		}
	}
	return removed
}
```

### The runnable demo

The demo stores two entries with different TTLs on a `FakeClock`, advances past
the shorter TTL, sweeps, and prints the sizes — showing the janitor reclaim
exactly the expired key.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	fc := ttlcache.NewFakeClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	c := ttlcache.New[string, string](fc)

	c.Set("short", "a", time.Minute)
	c.Set("long", "b", time.Hour)
	fmt.Printf("len after set: %d\n", c.Len())

	fc.Advance(2 * time.Minute) // past "short", before "long"
	_, shortOK := c.Get("short")
	_, longOK := c.Get("long")
	fmt.Printf("short live: %v, long live: %v\n", shortOK, longOK)

	fmt.Printf("swept: %d, len after sweep: %d\n", c.Sweep(), c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
len after set: 2
short live: false, long live: true
swept: 1, len after sweep: 1
```

### Tests

`TestExpiryBoundary` sets an entry and probes `Get` at exactly `TTL-1ns` (hit),
`TTL` (miss), and `TTL+1ns` (miss), pinning the inclusive-deadline contract.
`TestSweepEvictsExactlyExpired` stores three entries with staggered TTLs, advances
between two of them, sweeps, and asserts precisely the expired keys are gone and
the live one remains. `TestConcurrentAccess` exercises the RWMutex under `-race`.

Create `cache_test.go`:

```go
package ttlcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestExpiryBoundary(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	c := New[string, int](fc)
	c.Set("k", 7, time.Second)

	fc.Advance(time.Second - time.Nanosecond)
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Get at TTL-1ns = %d,%v; want 7,true", v, ok)
	}

	fc.Advance(time.Nanosecond) // exactly TTL
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get at exactly TTL still hit; expiry must be inclusive")
	}

	fc.Advance(time.Nanosecond) // TTL+1ns
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get past TTL still hit")
	}
}

func TestSweepEvictsExactlyExpired(t *testing.T) {
	t.Parallel()
	fc := NewFakeClock(epoch)
	c := New[string, int](fc)
	c.Set("a", 1, time.Minute)
	c.Set("b", 2, time.Minute)
	c.Set("live", 3, time.Hour)

	fc.Advance(2 * time.Minute) // past a and b, before live
	if got := c.Sweep(); got != 2 {
		t.Fatalf("Sweep evicted %d, want 2", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len after sweep = %d, want 1", got)
	}
	if v, ok := c.Get("live"); !ok || v != 3 {
		t.Fatalf("Get(live) = %d,%v; want 3,true", v, ok)
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()
	c := New[int, int](NewFakeClock(epoch))
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(i, i, time.Minute)
			c.Get(i)
			c.Len()
		}()
	}
	wg.Wait()
}

func ExampleCache_Get() {
	fc := NewFakeClock(epoch)
	c := New[string, string](fc)
	c.Set("session", "alice", time.Minute)

	v, ok := c.Get("session")
	fmt.Println(v, ok)

	fc.Advance(time.Minute)
	_, ok = c.Get("session")
	fmt.Println(ok)
	// Output:
	// alice true
	// false
}
```

## Review

The cache is correct when expiry is a pure function of the injected clock and the
stored deadline, and eviction removes exactly the entries a `Get` would already
treat as absent. The boundary test is the proof that matters: hit at `TTL-1ns`,
miss at `TTL` (inclusive), miss beyond. Conflating lazy expiry with eviction is
the classic bug — if `Get` deleted the entry itself you would lose the ability to
report an honest `Len` and to batch eviction, and if `Len` skipped expired entries
it would hide a memory leak until the process fell over. Keep `Get` read-only
(hence `RLock`), keep deletion in `Sweep` (hence `Lock`), and let `Len` count what
is really stored. Run `go test -race` to confirm the RWMutex guards concurrent
`Set`/`Get`/`Sweep`.

## Resources

- [`time.Time.Before`](https://pkg.go.dev/time#Time.Before) and [`time.Time.Add`](https://pkg.go.dev/time#Time.Add) — the deadline comparison at the heart of expiry.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — many readers, one writer, for a read-heavy cache.
- [Go generics tutorial](https://go.dev/doc/tutorial/generics) — the type parameters used by `Cache[K, V]`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-token-bucket-rate-limiter.md](03-token-bucket-rate-limiter.md) | Next: [05-synctest-ticker-worker.md](05-synctest-ticker-worker.md)
