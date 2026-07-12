# Exercise 5: Read-Through Cache Without RWMutex Self-Deadlock

A read-through cache guarded by `sync.RWMutex` invites a specific self-deadlock: hold the
read lock, miss, then take the write lock to fill — on the same goroutine. `RWMutex` has no
lock upgrade, so that deadlocks. This exercise builds the cache the correct way with the
double-checked (check-lock-recheck) pattern, plus single-flight so a thundering herd does not
all recompute the same value.

This module is fully self-contained: its own `go mod init`, all types inline, its own demo
and tests.

## What you'll build

```text
cache/                     independent module: example.com/cache
  go.mod                   go 1.25
  cache.go                 Cache[K,V]; Get with RLock -> drop -> Lock -> recheck -> load
  cmd/
    demo/
      main.go              concurrent Gets of a cold key; loader runs once
  cache_test.go            bounded-load-count concurrency test + hit/miss unit tests
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a generic `Cache[K comparable, V any]` whose `Get` reads under `RLock`, and on a miss drops the read lock, takes the write `Lock`, re-checks, and only then calls the loader — never holding `RLock` while taking `Lock`.
- Test: a concurrent `Get` of a cold key with a slow loader that counts invocations, asserting the value is loaded at most once (single-flight) and no goroutine deadlocks; unit tests for hit, miss, and loader error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/14-deadlock-detection-and-prevention/05-rwmutex-readthrough-cache/cmd/demo
cd go-solutions/13-goroutines-and-channels/14-deadlock-detection-and-prevention/05-rwmutex-readthrough-cache
go mod edit -go=1.25
```

### Why RLock-then-Lock deadlocks

`sync.RWMutex` lets many goroutines hold the read lock concurrently, or one goroutine hold the
write lock exclusively. The write `Lock` waits until *all* readers have released their read
locks. Now picture the tempting-but-broken read-through cache:

```go
c.mu.RLock()
v, ok := c.m[key]
if !ok {
	c.mu.Lock() // DEADLOCK: waits for readers to release, but this goroutine is a reader
	...
}
```

The goroutine holds `RLock` and then calls `Lock`. `Lock` blocks until every reader releases —
including this very goroutine, which is holding a read lock and is now blocked in `Lock` and so
will never release it. It waits on itself. `RWMutex` does not support upgrading a read lock to a
write lock; there is no atomic "I already read, now let me write." The result is a self-deadlock
that hangs exactly the one goroutine, invisibly to the runtime.

The correct pattern is **double-checked locking**: take `RLock`, read; on a hit return under the
read lock; on a miss, *release* the read lock, take the write `Lock`, then **re-check** the map,
because between dropping `RLock` and acquiring `Lock` another goroutine may have filled the entry.
Only if it is still missing do you call the loader and store the result. The re-check is not
optional — without it, N goroutines that all miss will all take the write lock in turn and all
run the loader, which is both wasteful and, if the loader has side effects, wrong. The re-check
turns the herd into single-flight: the first writer fills the entry, and the rest find it on their
re-check and skip the loader.

One deliberate design note: the loader runs *while the write lock is held*. That serializes
concurrent misses for different keys too, which is a simplification — a production cache often
uses a per-key `singleflight.Group` so a slow load of key A does not block a load of key B. Here
we keep one lock to keep the RWMutex lesson focused; the exercise's contract is "the loader for a
given key runs at most once under contention," which the write lock plus re-check guarantees.

Create `cache.go`:

```go
package cache

import "sync"

// Loader computes the value for a key on a cache miss.
type Loader[K comparable, V any] func(key K) (V, error)

// Cache is a concurrency-safe read-through cache. Reads take a shared RLock;
// a miss drops it and takes the exclusive Lock, re-checks, then loads once.
type Cache[K comparable, V any] struct {
	mu   sync.RWMutex
	m    map[K]V
	load Loader[K, V]
}

// New returns a cache that fills misses with load.
func New[K comparable, V any](load Loader[K, V]) *Cache[K, V] {
	return &Cache[K, V]{m: make(map[K]V), load: load}
}

// Get returns the cached value for key, loading it on a miss. It never holds
// the read lock while taking the write lock, so it cannot self-deadlock, and it
// re-checks under the write lock so the loader runs at most once per key under
// concurrent misses.
func (c *Cache[K, V]) Get(key K) (V, error) {
	c.mu.RLock()
	v, ok := c.m[key]
	c.mu.RUnlock()
	if ok {
		return v, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check: another goroutine may have filled it between RUnlock and Lock.
	if v, ok := c.m[key]; ok {
		return v, nil
	}
	v, err := c.load(key)
	if err != nil {
		var zero V
		return zero, err
	}
	c.m[key] = v
	return v, nil
}

// Len reports the number of cached entries.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
```

### The runnable demo

The demo fires many concurrent `Get`s of one cold key with a loader that sleeps and counts its
calls, showing the loader runs exactly once despite the herd.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/cache"
)

func main() {
	var calls atomic.Int64
	c := cache.New(func(key string) (int, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond) // slow load
		return len(key), nil
	})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Get("hello")
		}()
	}
	wg.Wait()

	fmt.Printf("loader calls: %d\n", calls.Load())
	v, _ := c.Get("hello")
	fmt.Printf("value: %d\n", v)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
loader calls: 1
value: 5
```

### Tests

`TestSingleFlightUnderHerd` launches 100 concurrent `Get`s of a cold key with a counting slow
loader and asserts the loader ran exactly once — the single-flight property that the re-check
provides. It also implicitly proves no deadlock: if `Get` used the broken RLock-then-Lock, the
first miss would hang and the `WaitGroup` would never complete (caught by the test timeout).
`TestNoUpgradeDeadlock` documents, in a comment tied to the code, why the broken pattern
deadlocks. Unit tests cover hit, miss, and loader error.

Create `cache_test.go`:

```go
package cache

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetHitAndMiss(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	c := New(func(k string) (int, error) {
		calls.Add(1)
		return len(k), nil
	})

	v, err := c.Get("abc")
	if err != nil || v != 3 {
		t.Fatalf("miss: got %d,%v want 3,nil", v, err)
	}
	v, err = c.Get("abc") // hit: loader not called again
	if err != nil || v != 3 {
		t.Fatalf("hit: got %d,%v want 3,nil", v, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("loader called %d times, want 1", calls.Load())
	}
}

func TestGetLoaderError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	c := New(func(k string) (int, error) { return 0, wantErr })
	if _, err := c.Get("x"); !errors.Is(err, wantErr) {
		t.Fatalf("Get err = %v, want %v", err, wantErr)
	}
	if c.Len() != 0 {
		t.Fatalf("failed load cached an entry: Len = %d", c.Len())
	}
}

// TestSingleFlightUnderHerd proves the re-check pattern (RLock; on miss drop it,
// take Lock, re-check, load) both avoids the RWMutex upgrade deadlock and runs
// the loader at most once under a herd. The broken alternative — holding RLock
// while calling Lock on the same RWMutex — would self-deadlock here, hanging the
// WaitGroup until the test's timeout.
func TestSingleFlightUnderHerd(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	c := New(func(k string) (int, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return len(k), nil
	})

	const n = 100
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := c.Get("hello")
			if err != nil || v != 5 {
				t.Errorf("Get = %d,%v, want 5,nil", v, err)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("loader called %d times under herd, want 1 (single-flight broken)", got)
	}
}
```

## Review

The cache is correct when `Get` never holds the read lock while acquiring the write lock, and
re-checks under the write lock before loading. The read-then-drop-then-write sequence is what
makes it self-deadlock-free; the re-check is what makes it single-flight. `TestSingleFlightUnderHerd`
asserts both at once: a loader-call count of exactly 1 under 100 concurrent misses is only
achievable if no goroutine deadlocked (all 100 completed) and the re-check collapsed the herd.

The mistake this exercise exists to prevent is RLock-then-Lock expecting an upgrade — there is
none, and it hangs the goroutine. The second is skipping the re-check, which turns the herd back
into N redundant loads. Note the trade-off made explicit above: loading under the write lock
serializes misses across keys; a per-key `singleflight.Group` removes that at the cost of more
machinery. Run `-race` to confirm the map is fully guarded on both the read and write paths.

## Resources

- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — reader/writer locking and the absence of upgrade.
- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — per-key request coalescing for a production read-through cache.
- [The Go Memory Model](https://go.dev/ref/mem) — the happens-before a write lock establishes for cached reads.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-nonreentrant-mutex-selfdeadlock.md](06-nonreentrant-mutex-selfdeadlock.md)
