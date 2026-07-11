# Exercise 33: Distributed Cache Double-Check Locking Allows Stale Data

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Double-checked locking exists to make the common case — a cache hit —
fast: check for a value under a cheap read lock first, and only pay
for the exclusive lock on the rare path, a miss. The subtlety that
makes this pattern easy to get wrong is exactly where the exclusive
lock's boundaries sit once you're on the miss path. It is tempting to
release the lock across the actual fetch from the source — a network
call, a database query — on the theory that holding an exclusive lock
across slow I/O blocks every *other* key's lookup too. That theory is
correct about the cost, and wrong about the consequence: releasing the
lock during the fetch reopens the exact race double-checked locking
was supposed to close, because nothing then stops two concurrent
misses from both fetching and both writing back, in whichever order
their fetches happen to finish — which is not necessarily the order
they started in. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
cache/                        independent module: example.com/cache-double-check-locking-race
  go.mod                       go 1.22
  cache.go                      Cache, New, Get
  cmd/
    demo/
      main.go                    runnable demo: 20 concurrent misses against a slow source
  cache_test.go                  concurrent-miss single-fetch guarantee, sequential cache-hit case
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: `Cache.Get(key string) (string, error)` using double-checked locking where the entire miss path -- re-check, fetch, write-back -- runs inside one exclusive-lock critical section.
- Test: a 50-goroutine burst of concurrent misses on the same key asserting the source is fetched exactly once and every caller sees the identical value; a sequential case confirming a second call is served from cache.
- Verify: `go test -count=1 -race ./...`.

```bash
mkdir -p ~/go-exercises/cache-double-check-locking-race/cmd/demo
cd ~/go-exercises/cache-double-check-locking-race
go mod init example.com/cache-double-check-locking-race
go mod edit -go=1.22
```

### Why releasing the lock during the fetch reopens the race

The version that looks like a reasonable optimization releases the
exclusive lock for the actual fetch, re-acquiring it only to commit
the result — minimizing how long other keys are blocked:

```go
// BUG: the lock is released across the fetch itself, so the recheck and
// the write-back are not part of one atomic miss resolution.
func (c *Cache) Get(key string) (string, error) {
	c.mu.RLock()
	if v, ok := c.data[key]; ok {
		c.mu.RUnlock()
		return v, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	if v, ok := c.data[key]; ok {
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock() // BUG: released before fetching

	v, err := c.source(key)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.data[key] = v // whichever goroutine's fetch finishes LAST wins
	c.mu.Unlock()
	return v, nil
}
```

Two concurrent callers missing on the same key can both pass the
re-check (nothing has been written yet, because neither has reached
the write-back), both release the lock, and both call `source(key)`
independently — exactly the redundant-fetch stampede double-checked
locking exists to prevent. Worse than the redundant work itself is
what happens if the *underlying value legitimately changes* between
the two fetches — a config value updated mid-flight, a price that just
changed — because nothing arbitrates which fetch's result is allowed
to win. Whichever goroutine's `c.data[key] = v` executes *last* is the
one that sticks, even if that goroutine's fetch started *first* and
its result is now the stale one. The cache ends up holding a value
that is provably older than one it briefly held moments earlier, with
no error and no signal that anything went wrong — the definition of
data quietly going stale under load.

The fix does not try to hold the lock across the network call safely
with some cleverer partial-release scheme; it accepts the simpler,
fully-correct answer the brief asks for: the recheck and the
write-back — the entire miss resolution — happen inside one
uninterrupted critical section.

```go
c.mu.Lock()
defer c.mu.Unlock()

if v, ok := c.data[key]; ok {
	return v, nil
}
v, err := c.source(key)
if err != nil {
	return "", err
}
c.data[key] = v
return v, nil
```

This does serialize every concurrent miss globally on one mutex,
which is a real, deliberate trade-off — a production system chasing
maximum miss-path throughput would reach for per-key locks or a
request-coalescing layer instead. But it eliminates the out-of-order
write correctness bug completely: by the time any second goroutine can
acquire the lock, the first goroutine's fetch-and-write is already
fully committed, so the second goroutine's re-check finds the key
present and never calls `source` at all.

Create `cache.go`:

```go
package cache

import "sync"

// Source fetches the authoritative value for key, e.g. from a database or
// a remote service.
type Source func(key string) (string, error)

// Cache is a read-through cache using double-checked locking: a fast
// read-locked check first, since a cache hit -- the common case -- should
// never contend with the exclusive lock at all; only on a miss is the
// exclusive lock acquired, and the whole miss path (re-check, fetch,
// write-back) happens inside that single critical section.
type Cache struct {
	mu     sync.RWMutex
	data   map[string]string
	source Source
}

// New creates a Cache backed by source.
func New(source Source) *Cache {
	return &Cache{data: make(map[string]string), source: source}
}

// Get returns the cached value for key, fetching from source on a miss.
// The entire miss path -- re-checking under the exclusive lock, fetching,
// and writing back -- happens inside one critical section, so concurrent
// misses for the same key can never both fetch and both write back an
// out-of-order result: whichever goroutine acquires the lock first
// resolves the miss completely before any other goroutine's re-check can
// even run.
func (c *Cache) Get(key string) (string, error) {
	c.mu.RLock()
	if v, ok := c.data[key]; ok {
		c.mu.RUnlock()
		return v, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	if v, ok := c.data[key]; ok {
		return v, nil
	}
	v, err := c.source(key)
	if err != nil {
		return "", err
	}
	c.data[key] = v
	return v, nil
}
```

### The runnable demo

Twenty goroutines all request the same tenant's config at once,
against a source slow enough (15ms) that a broken implementation would
have plenty of room to fetch redundantly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/cache-double-check-locking-race"
)

func main() {
	var fetches int64
	c := cache.New(func(key string) (string, error) {
		n := atomic.AddInt64(&fetches, 1)
		time.Sleep(15 * time.Millisecond) // simulates a slow backend fetch
		return fmt.Sprintf("value-for-%s-fetch-%d", key, n), nil
	})

	const callers = 20
	var wg sync.WaitGroup
	results := make([]string, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		i := i
		go func() {
			defer wg.Done()
			v, _ := c.Get("tenant-42-config")
			results[i] = v
		}()
	}
	wg.Wait()

	fmt.Println("source fetched", fetches, "time(s) for", callers, "concurrent misses")
	first := results[0]
	allSame := true
	for _, r := range results {
		if r != first {
			allSame = false
		}
	}
	fmt.Println("every caller received the same value:", allSame)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
source fetched 1 time(s) for 20 concurrent misses
every caller received the same value: true
```

### Tests

`TestGetFetchesSourceExactlyOnceUnderConcurrentMisses` is the
concurrency case: 50 goroutines released from a closed start barrier
all call `Get` for the same key against a deliberately slow source,
asserting the source is fetched exactly once and every single caller
receives the identical value — the precise guarantee a lock released
during the fetch would break. `TestGetCachesAcrossSequentialCalls`
pins the simple non-racy contract: a second sequential call for an
already-cached key is served without a second fetch.

Create `cache_test.go`:

```go
package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGetFetchesSourceExactlyOnceUnderConcurrentMisses is the concurrency
// case: 50 goroutines call Get for the same key at once against a slow
// source. Double-checked locking's entire purpose is that the source is
// fetched exactly once regardless of how many concurrent misses arrive --
// a lock that isn't held across the whole check-fetch-write path would
// let multiple goroutines each fetch and write back a different, possibly
// out-of-order, result.
func TestGetFetchesSourceExactlyOnceUnderConcurrentMisses(t *testing.T) {
	var fetches int64
	c := New(func(key string) (string, error) {
		n := atomic.AddInt64(&fetches, 1)
		time.Sleep(5 * time.Millisecond) // simulate a slow backend so misses overlap
		return fmt.Sprintf("value-for-%s-fetch-%d", key, n), nil
	})

	const callers = 50
	start := make(chan struct{})
	results := make([]string, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := range callers {
		go func() {
			defer wg.Done()
			<-start
			v, err := c.Get("tenant-42-config")
			if err != nil {
				t.Errorf("Get() error = %v, want nil", err)
			}
			results[i] = v
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&fetches); got != 1 {
		t.Fatalf("source fetched %d times for %d concurrent misses on the same key, want exactly 1", got, callers)
	}
	first := results[0]
	for i, v := range results {
		if v != first {
			t.Fatalf("results[%d] = %q, want every caller to see the same value %q (a stale out-of-order write leaked through)", i, v, first)
		}
	}
}

// TestGetCachesAcrossSequentialCalls pins the simple non-racy contract:
// once a key has been fetched, a later sequential Get for that key is
// served from the cache without a second fetch.
func TestGetCachesAcrossSequentialCalls(t *testing.T) {
	var fetches int64
	c := New(func(key string) (string, error) {
		atomic.AddInt64(&fetches, 1)
		return "value-for-" + key, nil
	})

	first, err := c.Get("k")
	if err != nil {
		t.Fatalf("first Get() error = %v, want nil", err)
	}
	second, err := c.Get("k")
	if err != nil {
		t.Fatalf("second Get() error = %v, want nil", err)
	}
	if first != second {
		t.Fatalf("first = %q, second = %q, want identical cached values", first, second)
	}
	if got := atomic.LoadInt64(&fetches); got != 1 {
		t.Fatalf("fetches = %d, want 1 (second Get should be a cache hit)", got)
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Get` is correct when, for any number of concurrent misses on the same
key, the source is fetched exactly once and every caller observes the
identical result — proven with a burst large enough (50 goroutines
against a deliberately slow source) that a lock released during the
fetch would visibly produce more than one fetch or a mismatched
result. The mistake this design avoids is optimizing the miss path by
narrowing the critical section around the slow part — the actual
fetch — without noticing that doing so un-atomically splits "check" and
"commit" back apart, which is exactly the race double-checked locking
exists to prevent in the first place. The trade-off this fix accepts
openly is coarser concurrency on the miss path: every concurrent miss,
even for different keys, serializes on one mutex. That is a real cost
worth naming in review, and a real production system might reach for
per-key locking or request coalescing (the same pattern the
`singleflight`-style exercise in this lesson set builds) to recover
miss-path parallelism — but never at the expense of the atomicity this
fix restores.

## Resources

- [Double-checked locking (Wikipedia)](https://en.wikipedia.org/wiki/Double-checked_locking) — the pattern, its purpose, and how narrowing the locked region incorrectly reintroduces races.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — `RLock` for the fast hit path, `Lock` for the full miss-resolution critical section.
- [Go Race Detector](https://go.dev/doc/articles/race_detector) — `-race` combined with a deliberately slow source to make the concurrent-miss window reproducible.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [32-database-conn-pool-multiplexing.md](32-database-conn-pool-multiplexing.md) | Next: [34-config-reload-concurrent-readers.md](34-config-reload-concurrent-readers.md)
