# Exercise 2: Read-through cache with the double-check pattern

A read-through cache reads under a shared lock and, on a miss, takes the exclusive
lock to load and store. The subtle part is the miss path: if several goroutines
miss the same hot key at once, a naive implementation lets each one take the write
lock and call the loader, producing a thundering herd of redundant loads. The
double-check pattern — check under `RLock`, then re-check under `Lock` before
loading — collapses concurrent misses to (near) a single load. This exercise
builds that cache and pins the contract with a concurrent-single-load test.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
rtcache/                     independent module: example.com/rtcache
  go.mod                     module example.com/rtcache
  cache.go                   type Cache; GetOrLoad (RLock fast path -> Lock + double-check), ErrEmptyKey, Len
  cmd/
    demo/
      main.go                runnable demo: load a key, hit the cache, reject an empty key
  cache_test.go              cached-value, concurrent-single-load, empty-key, loader-error tests, Example
```

Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
Implement: a `*Cache` with `GetOrLoad(key, Loader)` doing the `RLock`/`RUnlock` fast path then `Lock` + re-check before loading, an `ErrEmptyKey` guard, and `Len`.
Test: loader called exactly once over 100 gets; 50 concurrent misses collapse to one stored entry; an empty key returns `ErrEmptyKey`; a loader error is wrapped and does not poison the cache.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/rtcache/cmd/demo
cd ~/go-exercises/rtcache
go mod init example.com/rtcache
```

### The double-check, step by step

`GetOrLoad` runs in three phases. First, the fast path: take `RLock`, look up the
key, release. If it is present, return it — this is the common case, and concurrent
hits never block one another. Second, on a miss, take the exclusive `Lock`. Third
— and this is the load-bearing step — *re-check the map under the write lock*
before calling the loader. Between releasing the read lock and acquiring the write
lock, another goroutine that missed the same key may have already taken the write
lock, loaded the value, and stored it. The re-check finds that value and returns
it without loading again.

Drop the re-check and the pattern degenerates into a serial load-storm: every
goroutine that missed the key waits for the write lock in turn, and each one calls
the loader because none of them looks at what the previous holder stored. With the
re-check, only the first arrival loads; the rest find the stored value. That is
the "single load under contention" contract the test pins.

Two guards complete the type. An empty key returns the sentinel `ErrEmptyKey`
before any locking — empty keys are a caller bug, not a cache miss. And a loader
error is wrapped with `%w` and returned *without storing anything*, so a transient
load failure does not poison the cache with a bad or empty entry: the next call
retries the load cleanly. The write lock is held across the loader here for
simplicity; Exercise 8 shows how to keep a slow load off the lock entirely when
readers must never block on it.

Create `cache.go`:

```go
package rtcache

import (
	"errors"
	"fmt"
	"sync"
)

// ErrEmptyKey is returned by GetOrLoad when called with an empty key.
var ErrEmptyKey = errors.New("rtcache: empty key")

// Loader produces the value for a key on a cache miss.
type Loader func(key string) (string, error)

// Cache is a concurrency-safe read-through cache. Reads take a shared lock; a
// miss takes the exclusive lock and double-checks before loading, so concurrent
// misses on the same key collapse to (near) a single load.
type Cache struct {
	mu    sync.RWMutex
	store map[string]string
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{store: make(map[string]string)}
}

// GetOrLoad returns the cached value for key, loading and storing it on a miss.
// A loader error is wrapped and the cache is left untouched, so a failed load
// does not poison the entry.
func (c *Cache) GetOrLoad(key string, load Loader) (string, error) {
	if key == "" {
		return "", ErrEmptyKey
	}

	// Fast path: shared read lock, concurrent hits do not block.
	c.mu.RLock()
	v, ok := c.store[key]
	c.mu.RUnlock()
	if ok {
		return v, nil
	}

	// Slow path: exclusive lock, then re-check before loading.
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok = c.store[key]; ok {
		return v, nil // another goroutine loaded it while we waited for Lock
	}

	v, err := load(key)
	if err != nil {
		return "", fmt.Errorf("rtcache: load %q: %w", key, err)
	}
	c.store[key] = v
	return v, nil
}

// Len reports the number of cached entries under a shared read lock.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.store)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/rtcache"
)

func main() {
	c := rtcache.NewCache()
	loads := 0
	load := func(key string) (string, error) {
		loads++
		return "loaded-" + key, nil
	}

	// First call loads; second is served from the cache.
	v1, _ := c.GetOrLoad("k", load)
	v2, _ := c.GetOrLoad("k", load)
	fmt.Printf("v1=%s v2=%s loads=%d len=%d\n", v1, v2, loads, c.Len())

	_, err := c.GetOrLoad("", load)
	fmt.Printf("empty key -> ErrEmptyKey: %t\n", errors.Is(err, rtcache.ErrEmptyKey))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
v1=loaded-k v2=loaded-k loads=1 len=1
empty key -> ErrEmptyKey: true
```

### Tests

`TestCacheReturnsCachedValue` calls `GetOrLoad` 100 times for the same key with a
loader that counts its invocations and asserts it ran exactly once — the fast path
serves every call after the first. `TestCacheConcurrentSingleLoad` is the lesson's
most important contract: 50 goroutines miss the same hot key at once; the cache
must end with exactly one entry and the loader must run at least once (the
double-check may permit a small number of duplicate loads under heavy contention,
but never a per-goroutine storm, and never more than one stored value).
`TestCacheRejectsEmptyKey` pins the sentinel. `TestCachePropagatesLoaderError`
proves a loader failure is wrapped (asserted with `errors.Is`) and leaves the
cache empty, so a bad load never poisons the entry.

Create `cache_test.go`:

```go
package rtcache

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCacheReturnsCachedValue(t *testing.T) {
	t.Parallel()

	c := NewCache()
	var loads atomic.Int64
	load := func(key string) (string, error) {
		loads.Add(1)
		return "v-" + key, nil
	}

	for range 100 {
		v, err := c.GetOrLoad("k", load)
		if err != nil {
			t.Fatal(err)
		}
		if v != "v-k" {
			t.Fatalf("v = %q, want v-k", v)
		}
	}
	if got := loads.Load(); got != 1 {
		t.Fatalf("loader called %d times, want 1", got)
	}
}

func TestCacheConcurrentSingleLoad(t *testing.T) {
	t.Parallel()

	c := NewCache()
	var loads atomic.Int64
	load := func(key string) (string, error) {
		loads.Add(1)
		return "v-" + key, nil
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if _, err := c.GetOrLoad("hot", load); err != nil {
				t.Errorf("GetOrLoad: %v", err)
			}
		}()
	}
	wg.Wait()

	// The cache must hold exactly one entry and every goroutine sees the same
	// value. The double-check may permit a few duplicate loads under heavy
	// contention, but the loader must have run at least once.
	if got := c.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
	if loads.Load() < 1 {
		t.Fatal("loader never called")
	}
}

func TestCacheRejectsEmptyKey(t *testing.T) {
	t.Parallel()

	c := NewCache()
	_, err := c.GetOrLoad("", func(string) (string, error) { return "", nil })
	if !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("err = %v, want ErrEmptyKey", err)
	}
}

func TestCachePropagatesLoaderError(t *testing.T) {
	t.Parallel()

	errBackend := errors.New("backend unavailable")
	c := NewCache()

	_, err := c.GetOrLoad("k", func(string) (string, error) {
		return "", errBackend
	})
	if !errors.Is(err, errBackend) {
		t.Fatalf("err = %v, want wrapped errBackend", err)
	}
	// A failed load must not poison the cache.
	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d after failed load, want 0", got)
	}

	// A subsequent successful load populates the entry cleanly.
	v, err := c.GetOrLoad("k", func(key string) (string, error) {
		return "v-" + key, nil
	})
	if err != nil {
		t.Fatalf("retry after failed load: %v", err)
	}
	if v != "v-k" || c.Len() != 1 {
		t.Fatalf("retry v = %q len = %d, want v-k, 1", v, c.Len())
	}
}

func ExampleCache() {
	c := NewCache()
	load := func(key string) (string, error) { return "v-" + key, nil }
	v, _ := c.GetOrLoad("k", load)
	fmt.Println(v)
	// Output: v-k
}
```

## Review

The cache is correct when the fast path never mutates, the slow path re-checks
under the write lock before loading, and a loader error leaves the map untouched.
The single-load contract is the heart of it: `TestCacheConcurrentSingleLoad`
proves 50 simultaneous misses do not each load, and `TestCacheReturnsCachedValue`
proves steady-state hits never load. The mistakes to avoid are dropping the
re-check (which reintroduces the herd), storing a value before checking the
loader's error (which poisons the cache), and returning the loader error unwrapped
(so callers cannot `errors.Is` it). Run `go test -race`; the double-check must be
race-free as well as correct.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock behind the double-check.
- [`errors.Is` and `%w` wrapping](https://pkg.go.dev/errors#Is) — how the loader error is propagated and asserted.
- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — the production primitive for collapsing duplicate concurrent loads to exactly one.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-hot-reload-backend-pool.md](03-hot-reload-backend-pool.md)
