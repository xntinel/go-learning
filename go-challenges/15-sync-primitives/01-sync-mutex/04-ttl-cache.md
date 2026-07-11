# Exercise 4: In-memory cache with per-key TTL

The hot cache in front of a database or a remote config service is a mutex, a
map, and an expiry timestamp per entry. This module builds that cache as a
generic `Cache[V any]`: `Set` with a TTL, `Get` that treats an expired entry as
absent (lazy expiry), and a `DeleteExpired` sweep that reclaims stale memory. It
also shows the clean way to test time-dependent code without flaky real sleeps —
an injectable clock.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
ttlcache/                    independent module: example.com/ttlcache
  go.mod                     go 1.26
  cache.go                   type Cache[V any]; New, Set, Get, DeleteExpired, Len
  cmd/
    demo/
      main.go                runnable demo: set, read, sleep past TTL, read again
  cache_test.go              round-trip, injected-clock expiry, DeleteExpired, -race
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a generic `Cache[V any]` (mutex + `map[string]entry[V]`) with `Set(key, v, ttl)`, `Get(key) (V, bool)` (lazy expiry), `DeleteExpired() int`, and `Len() int`, reading the clock through an injectable `now func() time.Time`.
- Test: Set/Get round-trip; expiry via an injected clock asserting `Get` returns `ok==false` past the deadline; `DeleteExpired` removes only stale keys; concurrent Set/Get under `-race`.
- Verify: `go test -count=1 -race ./...`

```bash
mkdir -p ~/go-exercises/ttlcache/cmd/demo
cd ~/go-exercises/ttlcache
go mod init example.com/ttlcache
```

### Lazy expiry, and an injectable clock for honest tests

The cache stores each value with an `expires` instant computed at `Set` time as
`now().Add(ttl)`. `Get` is lazy: it treats an entry as absent once the clock is
no longer before `expires`, but it does not delete it there. Lazy expiry keeps
the read path a single map lookup and a time comparison — no allocation, no
write — which is what you want on the hot path. The cost is that an
expired-but-never-read entry keeps holding memory until something reclaims it,
which is `DeleteExpired`'s job: a sweep, run on a ticker in production, that
deletes every entry whose deadline has passed. `Len` counts stored entries,
expired or not, so it reflects memory held rather than live entries — a useful
distinction when you are deciding whether the sweeper is keeping up.

Every method touches the map under one mutex; a Go map races and crashes under
concurrent access otherwise. The critical sections are tiny: a lookup or a store
plus a time comparison, nothing slow.

The design choice worth calling out is the `now func() time.Time` field.
Production uses `time.Now`; tests inject a fake clock they can advance by hand.
This is what makes the expiry test deterministic and instant instead of a real
`time.Sleep` that is both slow and flaky (a 10 ms TTL tested with an 11 ms sleep
occasionally fails when the machine is busy). The injected clock is guarded by
its own mutex in the test because `Advance` and `Now` are called from different
goroutines under `-race`. (A newer alternative, `testing/synctest`, virtualizes
`time` itself so you need no clock field at all; it is covered in chapter 48.
Here, clock injection keeps the artifact buildable on any toolchain and makes the
seam explicit.)

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

// Cache is a concurrency-safe map whose entries expire after a per-key TTL.
// Expiry is lazy: Get ignores an expired entry, DeleteExpired reclaims it.
type Cache[V any] struct {
	mu    sync.Mutex
	items map[string]entry[V]
	now   func() time.Time
}

// New returns a Cache that reads the wall clock via time.Now.
func New[V any]() *Cache[V] {
	return newWithClock[V](time.Now)
}

// newWithClock returns a Cache reading time through now, for deterministic tests.
func newWithClock[V any](now func() time.Time) *Cache[V] {
	return &Cache[V]{items: make(map[string]entry[V]), now: now}
}

// Set stores value under key, expiring it ttl from now.
func (c *Cache[V]) Set(key string, value V, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expires: c.now().Add(ttl)}
}

// Get returns the value if present and unexpired; an expired or missing key
// reports (zero, false).
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok || !c.now().Before(e.expires) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// DeleteExpired removes every entry whose deadline has passed and returns the
// number removed. Run it periodically to reclaim lazily-expired memory.
func (c *Cache[V]) DeleteExpired() int {
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

// Len reports the number of stored entries, expired or not.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
```

### The runnable demo

The demo uses a real short sleep so you can watch an actual eviction: it caches a
session for 30 ms, reads it, sleeps 60 ms, and reads again to see it gone.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	c := ttlcache.New[string]()
	c.Set("session", "alice", 30*time.Millisecond)

	if v, ok := c.Get("session"); ok {
		fmt.Printf("before expiry: %s\n", v)
	}

	time.Sleep(60 * time.Millisecond)

	if _, ok := c.Get("session"); !ok {
		fmt.Println("after expiry: evicted")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before expiry: alice
after expiry: evicted
```

### Tests

`TestRoundTrip` checks a fresh `Set`/`Get`. `TestExpiryWithInjectedClock`
advances a fake clock past the deadline and asserts `Get` reports absence with no
real sleep, then that the boundary is exact — one nanosecond before expiry the
entry is still live. `TestDeleteExpired` proves the sweep removes only stale
keys. `TestConcurrentSetGet` runs the real-clock cache under `-race`.

Create `cache_test.go`:

```go
package ttlcache

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeClock is a hand-advanced clock, safe for concurrent Now/Advance.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	c := New[int]()
	c.Set("a", 42, time.Minute)
	if v, ok := c.Get("a"); !ok || v != 42 {
		t.Fatalf("Get(a) = %d,%v; want 42,true", v, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("Get(missing) reported ok=true")
	}
}

func TestExpiryWithInjectedClock(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newWithClock[int](clk.Now)
	c.Set("a", 1, time.Second)

	clk.Advance(time.Second - time.Nanosecond)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("entry expired one nanosecond early")
	}

	clk.Advance(time.Nanosecond) // now exactly at the deadline
	if _, ok := c.Get("a"); ok {
		t.Fatal("entry still present at its expiry instant")
	}
}

func TestDeleteExpired(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Unix(0, 0)}
	c := newWithClock[int](clk.Now)
	c.Set("stale", 1, time.Second)
	c.Set("fresh", 2, time.Hour)

	clk.Advance(2 * time.Second)
	if got := c.DeleteExpired(); got != 1 {
		t.Fatalf("DeleteExpired() removed %d, want 1", got)
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d after sweep, want 1", got)
	}
	if _, ok := c.Get("fresh"); !ok {
		t.Fatal("sweep removed the fresh key")
	}
}

func TestConcurrentSetGet(t *testing.T) {
	t.Parallel()
	c := New[int]()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%8)
			c.Set(key, i, time.Minute)
			c.Get(key)
		}()
	}
	wg.Wait()
}

func Example() {
	c := New[string]()
	c.Set("region", "eu-west-1", time.Minute)
	v, ok := c.Get("region")
	fmt.Println(v, ok)
	// Output: eu-west-1 true
}
```

## Review

The cache is correct when expiry is a pure function of the injected clock and the
stored deadline: `Get` returns `(zero, false)` exactly when the key is missing or
`now()` is not before `expires`, and nothing else moves the boundary. The
injected-clock test pins that boundary to the nanosecond, which a real
`time.Sleep` can never do reliably. `DeleteExpired` must remove stale keys only,
never a live one.

The traps are two. First, do not reach for a real sleep in the expiry test — it
is slow and flaky; inject the clock. Second, remember `Len` counting an expired
entry is correct, not a bug: lazy expiry defers reclamation to the sweep, and
conflating "stored" with "live" leads to a wrong assertion. Run `go test -race`
to confirm the mutex guards the map under concurrent `Set`/`Get`.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock guarding the map.
- [`time` package](https://pkg.go.dev/time) — `time.Now`, `Time.Add`, `Time.Before`, `Duration`.
- [Type parameters (generics)](https://go.dev/blog/intro-generics) — the `Cache[V any]` shape.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the alternative that virtualizes time, removing the clock field (chapter 48).

---

Back to [03-race-contention-test.md](03-race-contention-test.md) | Next: [05-token-bucket-limiter.md](05-token-bucket-limiter.md)
