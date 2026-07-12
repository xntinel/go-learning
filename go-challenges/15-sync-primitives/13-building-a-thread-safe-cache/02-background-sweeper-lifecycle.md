# Exercise 2: Background Sweeper With a Clean Lifecycle

Lazy expiry keeps `Get` correct, but an entry that expires and is never read
again occupies memory forever — so every TTL cache ships a background sweeper.
A sweeper is a goroutine, and a goroutine you start is a goroutine you must be
able to stop *and prove stopped*: this exercise builds the sweep plus both
lifecycle variants (stop channel and context), each returning a done channel
that shutdown code can wait on.

## What you'll build

```text
cachesweeper/                    independent module: example.com/cachesweeper
  go.mod
  cache/
    cache.go                     sharded TTL cache core (Set/Get/Delete/Size)
    sweep.go                     Cleanup (one shard at a time),
                                 StartCleanup(interval, stop) <-chan struct{},
                                 StartCleanupCtx(ctx, interval) <-chan struct{}
    sweep_test.go                manual-sweep test, lifecycle tests that WAIT for
                                 goroutine exit, concurrent-Cleanup race test
  cmd/
    demo/
      main.go                    sweeper reclaims expired entries, then clean shutdown
```

- Files: `cache/cache.go`, `cache/sweep.go`, `cache/sweep_test.go`, `cmd/demo/main.go`.
- Implement: `Cleanup` sweeping shards one at a time, `StartCleanup` (stop channel) and `StartCleanupCtx` (context), both returning a done channel closed when the goroutine exits.
- Test: manual sweep shrinks `Size`; cancel/close then receive on done with a timeout to prove the goroutine exited; concurrent `Cleanup` calls under `-race`.
- Verify: `go test -count=1 -race ./...`

### The core cache (self-contained copy)

This module is independent, so it carries its own copy of the sharded core
from exercise 1.

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

// Cache is a lock-striped, TTL-aware map with lazy expiry on Get and a
// background sweep for memory reclamation (see sweep.go).
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
```

### The sweep: one shard at a time, always

`Cleanup` iterates shards in a fixed order and, for each, acquires the lock,
deletes expired entries, and *releases before touching the next shard*. This
acquire-release loop is the load-bearing decision. Holding all shard locks for
the duration of the sweep would block every foreground operation on every
shard — a stop-the-world pause whose length grows with cache size. Nesting one
shard's lock inside another's would additionally create a lock-ordering hazard
against any future code path that also takes two shard locks. With the loop,
the sweeper contends with at most one shard's traffic at any instant, and a
concurrent user-initiated `Cleanup` is safe: both sweepers serialize per shard
through the same mutex, and deleting an already-deleted key is a no-op.

### Lifecycle: stop, and *observe* stopped

`StartCleanup(interval, stop)` keeps the classic stop-channel API: the caller
closes `stop` and the goroutine returns. The upgrade over the naive version is
the return value — a `done` channel the goroutine closes on exit (via `defer`,
so it closes even if a future edit adds a panic path). Without it, shutdown
code cannot distinguish "I asked the sweeper to stop" from "the sweeper has
stopped": a test that closes `stop` and immediately finishes leaks a goroutine
that is still draining its ticker, and a service that closes `stop` during
graceful shutdown may tear down state the sweeper is still touching.

`StartCleanupCtx(ctx, interval)` is the same loop driven by
`ctx.Done()` — the variant you want when the sweeper's lifetime is scoped to a
service's run context, so cancellation propagates from one place. Both
variants `defer t.Stop()` on the ticker; an unstopped ticker holds a runtime
timer alive until garbage collection, and in a long-lived process that is a
slow leak of timer work.

Create `cache/sweep.go`:

```go
package cache

import (
	"context"
	"time"
)

// Cleanup removes all expired entries. It locks shards one at a time —
// never all at once — so foreground traffic on other shards is never
// blocked. Safe to call concurrently with any other operation,
// including another Cleanup.
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
// returned channel is closed when the sweep goroutine has exited, so
// shutdown code can wait for it deterministically.
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

// StartCleanupCtx runs Cleanup every interval until ctx is cancelled.
// The returned channel is closed when the sweep goroutine has exited.
func (c *Cache[V]) StartCleanupCtx(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				c.Cleanup()
			case <-ctx.Done():
				return
			}
		}
	}()
	return done
}
```

### The demo

The demo stores twenty short-lived entries and two long-lived ones, lets the
sweeper run against the wall clock, and then shuts it down *and waits for the
done channel* — the pattern every service's shutdown hook should copy.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/cachesweeper/cache"
)

func main() {
	c := cache.New[int](8)
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartCleanupCtx(ctx, 20*time.Millisecond)

	for i := range 20 {
		c.Set(fmt.Sprintf("ephemeral-%d", i), i, 30*time.Millisecond)
	}
	c.Set("pinned-a", 1, time.Hour)
	c.Set("pinned-b", 2, 0) // never expires

	fmt.Printf("size before expiry: %d\n", c.Size())

	time.Sleep(100 * time.Millisecond) // TTLs pass; sweeper reclaims

	fmt.Printf("size after sweep: %d\n", c.Size())

	cancel()
	<-done // observe the goroutine exit, don't just hope
	fmt.Println("sweeper exited cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
size before expiry: 22
size after sweep: 2
sweeper exited cleanly
```

### Tests

The lifecycle tests are the point of this module. Each one cancels (or closes
`stop`) and then *receives on the done channel with a timeout*: if the
goroutine leaks, the test fails in 2 seconds with a precise message instead of
passing silently and tripping a leak detector three packages later. The
context test builds its cancelable context from `t.Context()` (Go 1.24), so
even a buggy sweeper that ignores the explicit cancel would be cancelled at
test end rather than outliving the test binary. `TestConcurrentCleanupSafe`
runs two sweepers plus a writer under `-race` to prove the "safe to call
concurrently" claim in the doc comment.

Create `cache/sweep_test.go`:

```go
package cache

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCleanupRemovesExpired(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Set("a", 1, 10*time.Millisecond)
	c.Set("b", 2, time.Hour)
	time.Sleep(20 * time.Millisecond)
	c.Cleanup()
	if got := c.Size(); got != 1 {
		t.Fatalf("Size after Cleanup = %d, want 1", got)
	}
	// Cleanup must not touch live entries.
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("Get(b) after Cleanup = %d,%v, want 2,true", v, ok)
	}
}

func TestStartCleanupStopsOnChannelClose(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	stop := make(chan struct{})
	done := c.StartCleanup(10*time.Millisecond, stop)

	c.Set("k", 1, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if got := c.Size(); got != 0 {
		t.Fatalf("Size = %d, want 0 (sweeper should have reclaimed k)", got)
	}

	close(stop)
	select {
	case <-done:
		// goroutine exited; no leak
	case <-time.After(2 * time.Second):
		t.Fatal("sweep goroutine did not exit after close(stop)")
	}
}

func TestStartCleanupCtxStopsOnCancel(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	ctx, cancel := context.WithCancel(t.Context())
	done := c.StartCleanupCtx(ctx, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweep goroutine did not exit after cancel")
	}
}

func TestConcurrentCleanupSafe(t *testing.T) {
	t.Parallel()

	c := New[int](8)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 50 {
			c.Cleanup()
		}
	}()
	go func() {
		defer wg.Done()
		for range 50 {
			c.Cleanup()
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 200 {
			c.Set(fmt.Sprintf("k-%d", i), i, time.Millisecond)
		}
	}()
	wg.Wait()
}

func ExampleCache_Cleanup() {
	c := New[string](2)
	c.Set("k", "v", time.Hour)
	c.Cleanup() // nothing expired; entry survives
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

The sweep is correct when three things hold: expired entries disappear, live
entries survive, and at no instant does the sweeper hold more than one shard
lock. The first two are pinned by `TestCleanupRemovesExpired`; the third is a
structural property of the acquire-release loop — if you ever find yourself
collecting locks into a slice to release later, you have reintroduced the
stop-the-world sweep.

Lifecycle is where the production bugs live. A `StartCleanup` that returns
nothing forces callers into sleep-and-hope shutdown; the done channel makes
"stopped" an observable event, and `defer close(done)` guarantees it fires on
every exit path. Forgetting `t.Stop()` on the ticker leaks a runtime timer per
cache instance. And note the asymmetry the tests encode: closing `stop` (or
cancelling ctx) is a *request*; only receiving on `done` is *confirmation*.
Confirm with `go test -count=1 -race ./...` — the concurrent-Cleanup test
plus the race detector proves the per-shard serialization claim rather than
asserting it.

## Resources

- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) — ticker semantics and why `Stop` matters.
- [`context` package](https://pkg.go.dev/context) — `WithCancel`, `Done`, and context-scoped goroutine lifetimes.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — the done-channel and explicit-cancellation patterns this module applies.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-sharded-ttl-cache-core.md](01-sharded-ttl-cache-core.md) | Next: [03-concurrent-load-harness.md](03-concurrent-load-harness.md)
