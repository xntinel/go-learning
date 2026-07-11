# Exercise 3: Concurrent Load Harness and Race Gate

A cache that passes single-goroutine tests has proven nothing about the thing
it exists for. This exercise promotes load testing to a first-class artifact:
a runnable harness that hammers the cache from dozens of goroutines with
atomic hit/miss accounting, plus the two heavy concurrency tests whose real
gate is `go test -count=1 -race`.

## What you'll build

```text
cacheload/                       independent module: example.com/cacheload
  go.mod
  cache/
    cache.go                     sharded TTL cache + Cleanup + StartCleanup
    cache_test.go                TestConcurrentSetGet (40 goroutines, EXACT Size),
                                 TestConcurrentSetGetExpiring (TTLs racing a sweeper,
                                 invariant-based assertions), Example
  cmd/
    demo/
      main.go                    20 goroutines x 200 ops, atomic hit/miss counters,
                                 background cleanup every 50ms
```

- Files: `cache/cache.go`, `cache/cache_test.go`, `cmd/demo/main.go`.
- Implement: the self-contained cache (core + sweep) and a load harness with `sync.WaitGroup` fan-out and `sync/atomic` counters.
- Test: exact-Size assertion under 40-goroutine load; an expiring-entries variant that pins invariants, not exact counts.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`

Set up the module:

```bash
mkdir -p ~/go-exercises/cacheload/cache ~/go-exercises/cacheload/cmd/demo
cd ~/go-exercises/cacheload
go mod init example.com/cacheload
```

### The cache under test (self-contained copy)

Create `cache/cache.go`:

```go
package cache

import (
	"hash/fnv"
	"sync"
	"time"
)

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

func (e *entry[V]) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && now.After(e.expiresAt)
}

type shard[V any] struct {
	mu    sync.RWMutex
	items map[string]*entry[V]
}

// Cache is a lock-striped, TTL-aware map: lazy expiry on Get, periodic
// sweep for memory reclamation.
type Cache[V any] struct {
	shards    []*shard[V]
	numShards uint32
}

func New[V any](numShards int) *Cache[V] {
	if numShards < 1 {
		numShards = 1
	}
	shards := make([]*shard[V], numShards)
	for i := range shards {
		shards[i] = &shard[V]{items: make(map[string]*entry[V])}
	}
	return &Cache[V]{shards: shards, numShards: uint32(numShards)}
}

func (c *Cache[V]) shardFor(key string) *shard[V] {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return c.shards[h.Sum32()%c.numShards]
}

// Set stores value under key with the given TTL. A non-positive TTL
// means "no expiration".
func (c *Cache[V]) Set(key string, value V, ttl time.Duration) {
	s := c.shardFor(key)
	var expiresAt time.Time
	if ttl > 0 {
		expiresAt = time.Now().Add(ttl)
	}
	s.mu.Lock()
	s.items[key] = &entry[V]{value: value, expiresAt: expiresAt}
	s.mu.Unlock()
}

// Get returns the value for key; false if missing or expired.
func (c *Cache[V]) Get(key string) (V, bool) {
	s := c.shardFor(key)
	s.mu.RLock()
	e, ok := s.items[key]
	if !ok || e.expired(time.Now()) {
		s.mu.RUnlock()
		var zero V
		return zero, false
	}
	v := e.value
	s.mu.RUnlock()
	return v, true
}

// Delete removes the entry for key. It is a no-op if the key is absent.
func (c *Cache[V]) Delete(key string) {
	s := c.shardFor(key)
	s.mu.Lock()
	delete(s.items, key)
	s.mu.Unlock()
}

// Size returns the number of non-expired entries across all shards.
func (c *Cache[V]) Size() int {
	now := time.Now()
	total := 0
	for _, s := range c.shards {
		s.mu.RLock()
		for _, e := range s.items {
			if !e.expired(now) {
				total++
			}
		}
		s.mu.RUnlock()
	}
	return total
}

// Cleanup removes all expired entries, locking shards one at a time.
func (c *Cache[V]) Cleanup() {
	now := time.Now()
	for _, s := range c.shards {
		s.mu.Lock()
		for k, e := range s.items {
			if e.expired(now) {
				delete(s.items, k)
			}
		}
		s.mu.Unlock()
	}
}

// StartCleanup runs Cleanup every interval until stop is closed. The
// returned channel is closed when the sweep goroutine exits.
func (c *Cache[V]) StartCleanup(interval time.Duration, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c.Cleanup()
			case <-stop:
				return
			}
		}
	}()
	return done
}
```

### The harness: what to count, and how

The demo runs 20 goroutines, each doing 200 set-then-get cycles on its own
keys with a 100 ms TTL, while a sweeper fires every 50 ms. Hits and misses are
counted with `sync/atomic.Int64` — never plain `int` fields incremented from
multiple goroutines (a data race the detector flags immediately) and never a
mutex around the counters (needless contention added by the measuring
instrument itself; the observer should not perturb the observed). The fan-out
uses the standard shape: `wg.Add(1)` before each `go`, `defer wg.Done()`
inside, `wg.Wait()` after the loop. Under Go 1.22+ loop-variable scoping,
each iteration's `i` is a fresh variable — no `i := i` aliasing dance.

After the workers finish, the demo sleeps past the TTL, closes the sweeper
(waiting on its done channel), and prints the tallies. Every Get here happens
microseconds after its Set, well inside the 100 ms TTL, so a healthy run shows
hits=4000 misses=0 — and the final Size is 0 because everything has expired
and been swept. If you ever see misses climb here, something is wrong with
expiry math, not with the harness.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/cacheload/cache"
)

func main() {
	c := cache.New[string](16)
	stop := make(chan struct{})
	done := c.StartCleanup(50*time.Millisecond, stop)

	var wg sync.WaitGroup
	var hits, misses atomic.Int64

	const goroutines, ops = 20, 200
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range ops {
				key := fmt.Sprintf("k-%d-%d", i, j)
				c.Set(key, "v", 100*time.Millisecond)
				if _, ok := c.Get(key); ok {
					hits.Add(1)
				} else {
					misses.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	time.Sleep(200 * time.Millisecond) // let TTLs pass and the sweeper run
	close(stop)
	<-done

	fmt.Printf("hits=%d misses=%d size=%d\n", hits.Load(), misses.Load(), c.Size())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hits=4000 misses=0 size=0
```

### The two hard tests

`TestConcurrentSetGet` is the exact-count test, and it can afford to be exact
because nothing expires: 20 writers store `writers*ops` distinct keys with
one-hour TTLs while 20 readers race them, and the final `Size` must be
*exactly* `writers*ops`. Any lost update — a torn map write, a shard routing
two goroutines' writes into each other — shows up as a wrong count, and any
memory race shows up in the race detector.

`TestConcurrentSetGetExpiring` deliberately refuses to be exact. Thirty
goroutines write 30 ms TTLs while a 20 ms sweeper races them; whether a given
Get lands before or after its entry's expiry depends on scheduling, and a test
that asserts exact counts here flakes in CI forever. Instead it pins the hard
invariants: no panic, the race detector stays silent, and *some* gets succeed
(liveness — a cache where every Get misses is broken differently). This split
— exact where deterministic, invariant-based where timing-dependent — is the
discipline that keeps concurrency suites trustworthy.

Create `cache/cache_test.go`:

```go
package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentSetGet(t *testing.T) {
	t.Parallel()

	c := New[int](16)
	const writers, readers, ops = 20, 20, 500

	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func() {
			defer wg.Done()
			for j := range ops {
				c.Set(fmt.Sprintf("k-%d-%d", i, j), j, time.Hour)
			}
		}()
	}
	wg.Add(readers)
	for i := range readers {
		go func() {
			defer wg.Done()
			for j := range ops {
				_, _ = c.Get(fmt.Sprintf("k-%d-%d", i%writers, j))
			}
		}()
	}
	wg.Wait()

	if got := c.Size(); got != writers*ops {
		t.Fatalf("Size = %d, want %d (lost updates under load)", got, writers*ops)
	}
}

func TestConcurrentSetGetExpiring(t *testing.T) {
	t.Parallel()

	c := New[int](16)
	stop := make(chan struct{})
	done := c.StartCleanup(20*time.Millisecond, stop)
	defer func() {
		close(stop)
		<-done
	}()

	const goroutines, ops = 30, 200
	var wg sync.WaitGroup
	var hits atomic.Int64

	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			for j := range ops {
				key := fmt.Sprintf("k-%d-%d", i, j)
				c.Set(key, 1, 30*time.Millisecond)
				if v, ok := c.Get(key); ok && v == 1 {
					hits.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// Timing-dependent expiry is nondeterministic: some Gets may see
	// expired entries. The hard contract is liveness (some hits) and
	// safety (no panic, race detector silent) — never exact counts.
	if got := hits.Load(); got == 0 {
		t.Fatal("no successful gets; cache not working")
	}
}

func ExampleCache() {
	c := New[string](1)
	c.Set("k", "v", time.Hour)
	v, _ := c.Get("k")
	fmt.Println(v)
	// Output: v
}
```

Run the gate:

```bash
gofmt -l . && go vet ./... && go test -count=1 -race ./...
```

## Review

The hard gate here is `go test -count=1 -race`. Without `-race`, both tests
would pass against a cache with no locking at all most of the time; the race
detector is what turns "it happened to work" into "no data race occurred on
any execution it observed". `-count=1` matters too: cached test results would
happily replay yesterday's pass over today's regression.

Three details are worth re-reading. The counters are atomics because the
measuring instrument must not add contention or races of its own. The
expiring test's cleanup is `close(stop); <-done` in a defer — the lifecycle
discipline from exercise 2 applied inside a test, so the sweeper cannot leak
into other parallel tests. And the exact-Size assertion works only because
that test uses one-hour TTLs: determinism was *designed in* by removing the
timing dimension, not achieved by tuning sleeps. When a load test of yours
flakes, the fix is almost always to move an assertion from the exact column
to the invariant column, or to remove the timing dependence entirely.

## Resources

- [Data Race Detector](https://go.dev/doc/articles/race_detector) — what `-race` catches, its cost, and its limits.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64` and friends for race-free counters.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the Add-before-go rule the harness follows.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-background-sweeper-lifecycle.md](02-background-sweeper-lifecycle.md) | Next: [04-singleflight-loader.md](04-singleflight-loader.md)
