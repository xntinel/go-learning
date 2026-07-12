# Exercise 8: Cache Layer — Deferred Unlock vs. Minimal Lock Hold Time

`defer mu.Unlock()` is correct-by-default, but its scope is the *whole function*,
so a naive read-through cache that defers the unlock at the top holds the lock
across a slow backend load — serializing every unrelated key lookup behind one
slow miss. This exercise refactors to small critical-section helpers whose
function boundary bounds the lock, doing the slow load *outside* any lock, and
proves with a timing test that distinct-key loads now proceed concurrently.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
rtcache/                     independent module: example.com/rtcache
  go.mod                     module example.com/rtcache
  rtcache.go                 Cache (RWMutex), lookup/store helpers, Get (load outside lock)
  cmd/
    demo/
      main.go                runnable demo: warm and hit the cache
  rtcache_test.go            -race concurrency; distinct-key loads run concurrently (timing)
```

- Files: `rtcache.go`, `cmd/demo/main.go`, `rtcache_test.go`.
- Implement: a `Cache` over a `map[string]string` guarded by a `sync.RWMutex`, with `lookup` (`RLock`) and `store` (`Lock`) helpers each deferring their unlock, and `Get(key)` that checks `lookup`, runs the slow `load` *outside* any lock on a miss, then `store`s the result.
- Test: `-race` concurrency over many goroutines; a timing test where a slow loader for distinct keys runs concurrently (total time near one load, not N loads).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/07-defer-semantics-and-ordering/08-mutex-unlock-scope/cmd/demo
cd go-solutions/03-control-flow/07-defer-semantics-and-ordering/08-mutex-unlock-scope
```

### Why the deferred unlock scope is the whole problem

Here is the naive method, and it is not obviously wrong:

```go
// Wrong: the lock is held across the slow load.
func (c *Cache) Get(key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[key]; ok {
		return v, nil
	}
	v, err := c.load(key) // slow I/O, and the lock is STILL held
	if err != nil {
		return "", err
	}
	c.m[key] = v
	return v, nil
}
```

`defer c.mu.Unlock()` releases the lock — correctly — on every return path. But its
scope is the entire function body, so the lock is held while `c.load(key)` does its
slow backend call. Every other goroutine calling `Get` for *any* key blocks on
that mutex until the one slow load finishes. A cache that was supposed to reduce
load now serializes the whole service behind its slowest miss.

The fix is not to drop `defer` — it is to shrink the function whose boundary the
lock lives in. Extract two tiny helpers: `lookup` takes the read lock, reads the
map, and returns (its `defer RUnlock` scopes the read lock to just that read);
`store` takes the write lock, writes, and returns. `Get` calls `lookup`, and on a
miss runs `c.load(key)` in its *own* frame with no lock held, then calls `store`.
Each helper holds its lock for a few nanoseconds; the slow load holds nothing. Two
goroutines missing on *different* keys now load concurrently.

The `sync.RWMutex` lets many `lookup`s proceed in parallel (read lock) while only
`store` needs exclusive access. The trade-off to acknowledge: because the load
runs outside the lock, two goroutines racing on the *same* missing key can both
load it (a duplicated backend call), and the last `store` wins. That is the
standard read-through cache trade-off — brief duplicate work in exchange for not
serializing the whole map — and single-flight de-duplication is a separate concern
layered on top, not a reason to hold the map lock across I/O.

Create `rtcache.go`:

```go
package rtcache

import "sync"

// Cache is a read-through cache. The map is guarded by an RWMutex; the slow load
// runs OUTSIDE any lock so distinct keys load concurrently.
type Cache struct {
	mu   sync.RWMutex
	m    map[string]string
	load func(key string) (string, error)
}

// New builds a cache whose misses are served by load.
func New(load func(key string) (string, error)) *Cache {
	return &Cache{m: make(map[string]string), load: load}
}

// lookup is a critical section: its defer scopes the read lock to just the map
// read, holding it for nanoseconds rather than across a load.
func (c *Cache) lookup(key string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[key]
	return v, ok
}

// store is the write critical section, likewise bounded by its own scope.
func (c *Cache) store(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = value
}

// Get returns a cached value or loads it. On a miss the load runs with NO lock
// held, so a slow load for one key does not block lookups for other keys.
func (c *Cache) Get(key string) (string, error) {
	if v, ok := c.lookup(key); ok {
		return v, nil
	}
	v, err := c.load(key)
	if err != nil {
		return "", err
	}
	c.store(key, v)
	return v, nil
}
```

### The runnable demo

The demo builds a cache whose loader tags each value, fetches two keys (misses),
then re-fetches one (a hit), and prints the results.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/rtcache"
)

func main() {
	c := rtcache.New(func(key string) (string, error) {
		return "loaded:" + key, nil
	})

	a, _ := c.Get("alpha")  // miss -> load
	b, _ := c.Get("beta")   // miss -> load
	a2, _ := c.Get("alpha") // hit -> cached

	fmt.Println(a)
	fmt.Println(b)
	fmt.Println(a2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loaded:alpha
loaded:beta
loaded:alpha
```

### Tests

`TestConcurrentGetsAreRaceFree` fires many goroutines through the cache under
`-race`. `TestDistinctKeysLoadConcurrently` is the point: a loader that sleeps a
fixed delay is asked for N distinct keys in parallel; if the lock were held across
the load, total time would be about N delays, but with the load outside the lock it
is about one delay. The test asserts the elapsed time is well under the serialized
bound.

Create `rtcache_test.go`:

```go
package rtcache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetLoadsAndCaches(t *testing.T) {
	t.Parallel()

	var loads atomic.Int32
	c := New(func(key string) (string, error) {
		loads.Add(1)
		return "v:" + key, nil
	})

	for range 3 {
		if v, err := c.Get("k"); err != nil || v != "v:k" {
			t.Fatalf("Get(k) = %q, %v", v, err)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("load called %d times for one key, want 1", got)
	}
}

func TestConcurrentGetsAreRaceFree(t *testing.T) {
	t.Parallel()

	c := New(func(key string) (string, error) {
		return "v:" + key, nil
	})

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%16)
			if _, err := c.Get(key); err != nil {
				t.Errorf("Get(%s) = %v", key, err)
			}
		}()
	}
	wg.Wait()
}

func TestDistinctKeysLoadConcurrently(t *testing.T) {
	t.Parallel()

	const (
		delay = 30 * time.Millisecond
		keys  = 8
	)
	c := New(func(key string) (string, error) {
		time.Sleep(delay) // simulate a slow backend
		return "v:" + key, nil
	})

	start := time.Now()
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Get(fmt.Sprintf("k%d", i))
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Serialized (lock held across load) would be ~keys*delay. Concurrent loads
	// finish in about one delay; allow generous slack for scheduling.
	if elapsed >= time.Duration(keys)*delay/2 {
		t.Fatalf("distinct-key loads took %v; expected concurrency (well under %v)",
			elapsed, time.Duration(keys)*delay/2)
	}
}
```

## Review

The cache is correct when the map access is race-free and — the performance
property that motivates the whole design — when a slow load for one key does not
block lookups for others. `TestDistinctKeysLoadConcurrently` proves the latter by
timing: eight 30ms loads finish in roughly 30ms because none holds the map lock
during I/O. The lesson is not "avoid `defer mu.Unlock()`" — it is the best default —
but "remember its scope is the function, so bound the critical section by
extracting a helper when slow work would otherwise sit inside it." The `RWMutex`
split (many concurrent `lookup`s, exclusive `store`) is the natural fit for a
read-heavy cache; `sync.Map` is an alternative for very high read concurrency, but
the explicit-lock version keeps the critical-section boundary visible, which is the
teaching point here.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — `RLock`/`RUnlock` for the read critical section, `Lock` for writes.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the acquire-then-defer-release baseline.
- [`sync.Map`](https://pkg.go.dev/sync#Map) — the alternative for read-mostly concurrent maps.
- [Go Code Review Comments: synchronize access to shared mutable state](https://go.dev/wiki/CodeReviewComments) — locking guidance.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-http-body-close-drain.md](07-http-body-close-drain.md) | Next: [09-graceful-shutdown-defer-stack.md](09-graceful-shutdown-defer-stack.md)
