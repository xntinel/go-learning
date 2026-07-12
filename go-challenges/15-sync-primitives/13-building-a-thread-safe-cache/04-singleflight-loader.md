# Exercise 4: GetOrLoad — Killing the Cache Stampede With Singleflight

The classic production incident: a hot key expires and five hundred concurrent
requests all miss at once, so five hundred identical queries hit the database
simultaneously — the cache stampede. This exercise wraps the sharded cache in
a `LoadingCache` whose `GetOrLoad` coalesces concurrent misses through
`golang.org/x/sync/singleflight`, so exactly one loader runs per key and every
waiter shares its result.

## What you'll build

```text
loadingcache/                    independent module: example.com/loadingcache
  go.mod                         requires golang.org/x/sync
  loadingcache/
    cache.go                     sharded TTL cache core (Set/Get/Delete/Size)
    loading.go                   LoadingCache[V]: singleflight.Group,
                                 GetOrLoad(ctx, key, ttl, loader), SharedLoads()
    loading_test.go              50 goroutines / 1 loader call, failing-loader
                                 semantics (error to all, nothing cached, retry works)
    example_test.go              testable Example
  cmd/
    demo/
      main.go                    8 concurrent requests, 1 database call
```

- Files: `loadingcache/cache.go`, `loadingcache/loading.go`, `loadingcache/loading_test.go`, `loadingcache/example_test.go`, `cmd/demo/main.go`.
- Implement: `GetOrLoad` with a cache fast path, singleflight coalescing with an in-flight double-check, error propagation to all waiters with nothing cached, and `Forget` after failures.
- Test: gate-channel loader proving exactly one call for 50 concurrent misses; failing loader returns the wrapped sentinel to every caller and the next call retries.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/13-building-a-thread-safe-cache/04-singleflight-loader/loadingcache go-solutions/15-sync-primitives/13-building-a-thread-safe-cache/04-singleflight-loader/cmd/demo
cd go-solutions/15-sync-primitives/13-building-a-thread-safe-cache/04-singleflight-loader
go get golang.org/x/sync
```

### The cache core (self-contained copy)

Create `loadingcache/cache.go`:

```go
package loadingcache

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

// Cache is a lock-striped, TTL-aware map with lazy expiry on Get.
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

### How singleflight coalesces, and where the sharp edges are

`singleflight.Group.Do(key, fn)` checks whether a call for `key` is already in
flight. If yes, the caller blocks and receives the in-flight call's result;
if no, it runs `fn` and becomes the flight everyone else joins. The third
return value, `shared`, reports whether the result was delivered to more than
one caller — `GetOrLoad` counts those in an atomic so tests and dashboards can
see coalescing actually happening.

Four decisions in `GetOrLoad` carry the production weight:

*The double-check inside the flight.* Between a caller's cache miss and its
`Do` call, a previous flight may have completed and populated the cache. The
first line inside `fn` re-checks the cache, so a late-joining caller costs a
map read, not a database query.

*Errors are returned, never cached.* If the loader fails, every waiter gets
the error (wrapped with `%w` so callers can `errors.Is` against their
sentinels) and the cache stays empty. Caching an error as if it were a value
pins one transient database blip onto every request until TTL.

*`Forget` after failure.* `Group` deduplicates against the in-flight call; on
an error, `Forget(key)` drops any memory of that call so the *next* request
starts a fresh load rather than risk attaching to a poisoned flight.
Forgetting to forget is how "the DB recovered but the service kept erroring"
incidents happen.

*The `any` boundary.* `singleflight.Group` predates generics: `Do` returns
`any`, so the wrapper type-asserts `v.(V)` on the way out. The generic
`LoadingCache[V]` confines that assertion to one line — callers never see it.
One caveat to know: all waiters share the *winner's* loader execution,
including its context. If the winner's request is cancelled mid-flight, every
waiter gets the cancellation error; per-caller context isolation requires
`DoChan` plus a detached load context, a refinement worth reaching for only
when cancellation storms are a real problem.

Create `loadingcache/loading.go`:

```go
package loadingcache

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// LoadingCache wraps Cache with stampede protection: concurrent misses
// for the same key run the loader exactly once and share its result.
type LoadingCache[V any] struct {
	cache      *Cache[V]
	group      singleflight.Group
	sharedLoad atomic.Int64
}

func NewLoading[V any](numShards int) *LoadingCache[V] {
	return &LoadingCache[V]{cache: New[V](numShards)}
}

// GetOrLoad returns the cached value for key, or runs loader to produce
// it. Concurrent calls for the same key are coalesced: one loader runs,
// everyone shares the result. Loader errors are returned to all waiters
// and are NOT cached; the flight is forgotten so the next call retries.
func (l *LoadingCache[V]) GetOrLoad(ctx context.Context, key string, ttl time.Duration, loader func(context.Context) (V, error)) (V, error) {
	if v, ok := l.cache.Get(key); ok {
		return v, nil
	}
	v, err, shared := l.group.Do(key, func() (any, error) {
		// Double-check: a flight that completed between our miss and
		// this call may already have populated the cache.
		if v, ok := l.cache.Get(key); ok {
			return v, nil
		}
		val, err := loader(ctx)
		if err != nil {
			return nil, err
		}
		l.cache.Set(key, val, ttl)
		return val, nil
	})
	if err != nil {
		l.group.Forget(key)
		var zero V
		return zero, fmt.Errorf("load %q: %w", key, err)
	}
	if shared {
		l.sharedLoad.Add(1)
	}
	return v.(V), nil
}

// SharedLoads reports how many callers received a result produced by
// another caller's flight — a direct measure of stampedes prevented.
func (l *LoadingCache[V]) SharedLoads() int64 {
	return l.sharedLoad.Load()
}

// Invalidate removes key so the next GetOrLoad reloads it.
func (l *LoadingCache[V]) Invalidate(key string) {
	l.cache.Delete(key)
}
```

### The demo

Eight goroutines request the same cold key while the "database" takes 50 ms to
answer. Without coalescing that is eight queries; with it, one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/loadingcache/loadingcache"
)

func main() {
	lc := loadingcache.NewLoading[string](8)
	var dbCalls atomic.Int64

	loader := func(ctx context.Context) (string, error) {
		dbCalls.Add(1)
		time.Sleep(50 * time.Millisecond) // the expensive query
		return "profile-of-user-42", nil
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := lc.GetOrLoad(context.Background(), "user:42", time.Minute, loader)
			if err != nil || v != "profile-of-user-42" {
				panic(fmt.Sprintf("unexpected result: %q %v", v, err))
			}
		}()
	}
	wg.Wait()

	fmt.Printf("concurrent requests: 8\n")
	fmt.Printf("database calls: %d\n", dbCalls.Load())
	fmt.Printf("waiters served by another caller's flight: %v\n", lc.SharedLoads() > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
concurrent requests: 8
database calls: 1
waiters served by another caller's flight: true
```

### Tests

The coalescing test uses a gated loader: the first call signals `started` and
then blocks on `gate`, guaranteeing the flight is still open while the other
49 goroutines arrive and join it. Only after a settling delay does the test
release the gate. The assertion `calls == 1` is exact and deterministic — the
gate removes the timing dimension. The failure test pins all three error
semantics at once: every caller sees the sentinel through `errors.Is` (proving
the `%w` wrap), nothing was cached, and the next call re-invokes the loader.

Create `loadingcache/loading_test.go`:

```go
package loadingcache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errUnavailable = errors.New("database unavailable")

func TestGetOrLoadCoalescesConcurrentMisses(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	started := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once

	loader := func(ctx context.Context) (string, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-gate
		return "db-value", nil
	}

	lc := NewLoading[string](8)
	const n = 50
	results := make([]string, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			results[i], errs[i] = lc.GetOrLoad(t.Context(), "hot", time.Minute, loader)
		}()
	}

	<-started                         // the flight is open
	time.Sleep(50 * time.Millisecond) // let the other goroutines pile on
	close(gate)                       // release the single loader
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("loader ran %d times, want exactly 1", got)
	}
	for i := range n {
		if errs[i] != nil || results[i] != "db-value" {
			t.Fatalf("caller %d: got %q, %v; want db-value, nil", i, results[i], errs[i])
		}
	}
	if lc.SharedLoads() == 0 {
		t.Fatal("no shared loads recorded; coalescing did not happen")
	}
}

func TestGetOrLoadDoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	lc := NewLoading[int](4)

	failing := func(ctx context.Context) (int, error) {
		calls.Add(1)
		return 0, errUnavailable
	}

	_, err := lc.GetOrLoad(t.Context(), "k", time.Minute, failing)
	if !errors.Is(err, errUnavailable) {
		t.Fatalf("err = %v, want wrapped errUnavailable", err)
	}
	if _, ok := lc.cache.Get("k"); ok {
		t.Fatal("error result was cached; errors must never be cached")
	}

	// The flight was forgotten: the next call retries the loader.
	v, err := lc.GetOrLoad(t.Context(), "k", time.Minute, func(ctx context.Context) (int, error) {
		calls.Add(1)
		return 7, nil
	})
	if err != nil || v != 7 {
		t.Fatalf("retry = %d, %v; want 7, nil", v, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("loader calls = %d, want 2 (fail, then retry)", got)
	}
}

func TestGetOrLoadErrorReachesAllWaiters(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	gate := make(chan struct{})
	var once sync.Once

	failing := func(ctx context.Context) (int, error) {
		once.Do(func() { close(started) })
		<-gate
		return 0, errUnavailable
	}

	lc := NewLoading[int](4)
	const n = 10
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			_, errs[i] = lc.GetOrLoad(t.Context(), "k", time.Minute, failing)
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(gate)
	wg.Wait()

	for i := range n {
		if !errors.Is(errs[i], errUnavailable) {
			t.Fatalf("waiter %d: err = %v, want wrapped errUnavailable", i, errs[i])
		}
	}
}

func TestGetOrLoadHitSkipsLoader(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	lc := NewLoading[string](4)
	loader := func(ctx context.Context) (string, error) {
		calls.Add(1)
		return "v", nil
	}

	for range 5 {
		if v, err := lc.GetOrLoad(t.Context(), "k", time.Minute, loader); err != nil || v != "v" {
			t.Fatalf("GetOrLoad = %q, %v", v, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("loader calls = %d, want 1 (subsequent calls are cache hits)", got)
	}
}
```

The testable Example lives in its own file to keep the main suite's imports
focused; `go test` verifies its output like any other test.

Create `loadingcache/example_test.go`:

```go
package loadingcache

import (
	"context"
	"fmt"
	"time"
)

func ExampleNewLoading() {
	lc := NewLoading[string](2)
	v, _ := lc.GetOrLoad(context.Background(), "greeting", time.Minute,
		func(ctx context.Context) (string, error) { return "hello", nil })
	fmt.Println(v)
	// Output: hello
}
```

Run the gate:

```bash
gofmt -l . && go vet ./... && go test -count=1 -race ./...
```

## Review

The contract worth memorizing: one loader per key per flight, errors to
everyone and cached for no one, `Forget` after failure so retries are real.
The double-check inside `Do` is not an optimization garnish — without it, a
caller that misses just as another flight completes runs a redundant database
query, and under sustained load "just as" happens constantly.

Two subtleties deserve a second look. First, the winner's context governs the
flight: a cancelled winner fails all waiters. That is usually acceptable and
occasionally an incident; know that `DoChan` plus a detached context is the
escape hatch. Second, coalescing is per-process — fifty service instances
still make fifty queries. Singleflight removes the multiplicative factor
*within* an instance; the fleet-scale factors are handled by TTL jitter
(exercise 6) and stale-while-revalidate (exercise 7). Confirm the whole story
with `go test -count=1 -race ./...`: the gated test proves exactly-once
loading under 50-way concurrency, deterministically.

## Resources

- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — `Group.Do`, `DoChan`, `Forget`, and the `Result` type.
- [`errors` package](https://pkg.go.dev/errors) — `Is` and the `%w` wrapping convention the error tests assert.
- [groupcache](https://github.com/golang/groupcache) — the original production home of the singleflight pattern.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-concurrent-load-harness.md](03-concurrent-load-harness.md) | Next: [05-lru-bounded-shards.md](05-lru-bounded-shards.md)
