# Exercise 5: A Concurrent TTL Cache Whose Race the Detector Catches

An in-memory TTL cache in front of a slow dependency is a backend staple, and it
is also a classic data-race trap: a plain `map` read concurrently with a write is
undefined behavior. This module writes the cache first as the naive racy version
(for contrast), then the correct `sync.RWMutex` version, and drives it with a
`t.Parallel()` test of concurrent readers and writers so `-race` proves the fix.
Expiry itself is tested deterministically with an injected clock.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
ttlcache/                   independent module: example.com/ttlcache
  go.mod
  cache.go                  Cache[K,V] with RWMutex; New, Set, Get, DeleteExpired, Len
  cmd/
    demo/
      main.go               runnable demo: set, read, expire, sweep
  cache_test.go             concurrent -race test + deterministic expiry via clock
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: a generic `Cache[K comparable, V any]` guarded by `sync.RWMutex`, with
an injectable `now func() time.Time` for deterministic expiry tests, plus
`Set`, `Get`, `DeleteExpired`, and `Len`.
Test: a concurrent readers/writers test to run under `-race`, and an expiry test
that advances an injected clock instead of sleeping.
Verify: `go test -race -count=10 ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/14-parallel-tests/05-race-on-parallel-ttl-cache/cmd/demo
cd go-solutions/12-testing-ecosystem/14-parallel-tests/05-race-on-parallel-ttl-cache
```

### The race, made concrete

Go maps are not safe for concurrent use: one goroutine writing a map while another
reads it is a data race, and the runtime may even fatal with "concurrent map
read and map write". The naive cache below has exactly that bug — `Get` reads
`c.items` while `Set` writes it, with no synchronization:

```go
// NAIVE AND WRONG — do not assemble. Under go test -race the concurrent test
// reports a data race on c.items, and the runtime may fatal on concurrent
// map read/write.
type Cache struct {
	items map[string]entry
}

func (c *Cache) Set(k string, v entry) { c.items[k] = v }        // write
func (c *Cache) Get(k string) (entry, bool) { e, ok := c.items[k]; return e, ok } // read
```

Under `go test -race`, the parallel readers-and-writers test observes this race
and fails — which is the point of running with the detector. The fix is a
`sync.RWMutex`: `Set` and `DeleteExpired` take the write lock (`Lock`), while
`Get` and `Len` take the read lock (`RLock`), so many readers proceed together but
a writer is exclusive. (A `sync.Map` is an alternative for read-heavy,
write-rarely workloads with disjoint keys; for a general read-modify-write cache
with a sweep, an `RWMutex` over a plain map is simpler to reason about and is what
we build.)

### Why an injected clock for expiry, and a mutex for the race

Two different test problems need two different tools. Concurrency safety is tested
by *running many goroutines and turning on `-race`* — that is what surfaces the
map race. Expiry *correctness* is a timing property, and sleeping real seconds to
test it is slow and flaky; instead we make `now` an injectable field
(`func() time.Time`, defaulting to `time.Now`) so a test can advance a fake clock
to a chosen instant and assert an entry is present just before its deadline and
gone just after, deterministically. (Chapter 48's synctest lesson shows a third
way — a virtual clock with no injection — for `go 1.25`; here the injected-clock
pattern keeps the module buildable on any toolchain and keeps the focus on the
race.)

Expiry is lazy: `Get` treats an entry as absent once `now()` is no longer before
its `expires` instant but does not delete it; `DeleteExpired` is the explicit
sweep that reclaims memory. `Len` counts stored entries regardless of expiry.

Create `cache.go`:

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

// Cache is a concurrency-safe TTL cache. It is guarded by an RWMutex so many
// readers proceed together while writers are exclusive. now is injectable so
// expiry can be tested deterministically without sleeping.
type Cache[K comparable, V any] struct {
	mu    sync.RWMutex
	items map[K]entry[V]
	now   func() time.Time
}

// New returns an empty cache using the real wall clock.
func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{items: make(map[K]entry[V]), now: time.Now}
}

// WithClock overrides the clock; used by expiry tests to advance time exactly.
func (c *Cache[K, V]) WithClock(now func() time.Time) *Cache[K, V] {
	c.now = now
	return c
}

// Set stores value under key, expiring it ttl from the current clock.
func (c *Cache[K, V]) Set(key K, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: c.now().Add(ttl)}
}

// Get returns the value if present and unexpired.
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

// DeleteExpired removes every entry whose deadline has passed and returns the
// number removed.
func (c *Cache[K, V]) DeleteExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	removed := 0
	for k, e := range c.items {
		if !now.Before(e.expires) {
			delete(c.items, k)
			removed++
		}
	}
	return removed
}

// Len reports the number of entries still stored, expired or not.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}
```

### The runnable demo

The demo uses an injected clock so the output is deterministic (no real sleep):
it stores a session, reads it live, advances the clock past the TTL, sees it
absent, then sweeps.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	now := time.Unix(0, 0)
	c := ttlcache.New[string, string]().WithClock(func() time.Time { return now })

	c.Set("session", "alice", time.Minute)
	if v, ok := c.Get("session"); ok {
		fmt.Printf("live: %s\n", v)
	}

	now = now.Add(2 * time.Minute) // advance past TTL
	if _, ok := c.Get("session"); !ok {
		fmt.Println("expired: not returned")
	}

	fmt.Printf("swept: %d, len: %d\n", c.DeleteExpired(), c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
live: alice
expired: not returned
swept: 1, len: 0
```

### Tests

`TestConcurrentReadersWriters` is the race test: it fans out writer goroutines and
reader goroutines against one cache and waits for them. It asserts no crash and a
sane final `Len`; its real job is to give `-race` concurrent map accesses to
instrument. On the naive unlocked cache this test fails under `-race`; on the
`RWMutex` cache it passes. `TestExpiryBoundary` uses the injected clock to assert
the entry is live one nanosecond before expiry and gone at the deadline —
deterministic, no sleep. `TestDeleteExpired` checks the sweep count.

Create `cache_test.go`:

```go
package ttlcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestConcurrentReadersWriters(t *testing.T) {
	t.Parallel()

	c := New[int, int]()
	const workers = 50
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := range 100 {
				c.Set(j, i*j, time.Minute)
			}
		}()
		go func() {
			defer wg.Done()
			for j := range 100 {
				c.Get(j)
			}
		}()
	}
	wg.Wait()

	// Keys 0..99 were written with a one-minute TTL, so all survive.
	if got := c.Len(); got != 100 {
		t.Fatalf("Len = %d, want 100", got)
	}
}

func TestExpiryBoundary(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	c := New[string, int]().WithClock(func() time.Time { return now })
	c.Set("k", 7, time.Second)

	now = now.Add(time.Second - time.Nanosecond) // just before expiry
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Get before expiry = %d,%v; want 7,true", v, ok)
	}

	now = now.Add(time.Nanosecond) // exactly at expiry
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get at expiry: entry should be absent")
	}
}

func TestDeleteExpired(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	c := New[string, int]().WithClock(func() time.Time { return now })
	c.Set("short", 1, time.Second)
	c.Set("long", 2, time.Hour)

	now = now.Add(time.Minute)
	if removed := c.DeleteExpired(); removed != 1 {
		t.Fatalf("DeleteExpired = %d, want 1", removed)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len after sweep = %d, want 1", got)
	}
}

func Example() {
	now := time.Unix(0, 0)
	c := New[string, int]().WithClock(func() time.Time { return now })
	c.Set("answer", 42, time.Minute)
	v, ok := c.Get("answer")
	fmt.Println(v, ok)
	// Output: 42 true
}
```

## Review

The cache is safe when every access to `c.items` holds the matching lock — writes
under `Lock`, reads under `RLock` — so no `Get` can observe a `Set` mid-write.
The proof is `go test -race`: the naive unlocked version fails the concurrent test
(and may fatal on concurrent map read/write), while the `RWMutex` version passes.
Run it with `-count=10` to raise the detector's chance of observing any residual
race; a real fix survives repetition.

Keep the two concerns separate: `-race` proves *safety*, the injected clock proves
*expiry correctness*. Using a real `time.Sleep` to test expiry would make the test
slow and flaky and would not test safety any better. And remember expiry is lazy —
`Len` counting an unexpired entry after a `Set` is correct; reclaiming an
expired-but-unread entry is `DeleteExpired`'s job.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — read/write locking semantics.
- [`sync.Map`](https://pkg.go.dev/sync#Map) — the read-mostly alternative and when it fits.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — how `-race` finds map races.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-config-loader-setenv-vs-parallel.md](04-config-loader-setenv-vs-parallel.md) | Next: [06-bounded-parallelism-scarce-pool.md](06-bounded-parallelism-scarce-pool.md)
