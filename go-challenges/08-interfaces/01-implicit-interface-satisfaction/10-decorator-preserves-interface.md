# Exercise 10: A Metrics/Caching Decorator That Still Satisfies Store

A decorator holds an interface value and also satisfies that same interface,
adding behavior transparently. Because satisfaction is implicit, you can stack
decorators — `metrics(cache(memory))` — and the base store never learns it is
wrapped. This module builds a read-through `CachingStore` and a counting
`MetricsStore`, both of which satisfy `Store`, and proves a cache hit avoids a
second delegate call.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
decorate/                     independent module: example.com/decorate
  go.mod                      go 1.26
  store.go                    Store interface (Get/Set); MemoryStore base; ErrNotFound
  cache.go                    CachingStore decorator (read-through cache + atomic counters)
  metrics.go                  MetricsStore decorator (atomic call counters)
  cmd/
    demo/
      main.go                 runnable demo: stack metrics(cache(memory))
  store_test.go               cache hit avoids delegate; counters; stacking; Store guards
```

- Files: `store.go`, `cache.go`, `metrics.go`, `cmd/demo/main.go`, `store_test.go`.
- Implement: a narrow `Store` (`Get`/`Set`), a `MemoryStore` base, a `CachingStore` that adds a read-through cache and hit/miss counters, and a `MetricsStore` that counts calls — both decorators satisfy `Store`.
- Test: a counting spy `Store` proves a cache hit skips the delegate `Get`; counters increment; two decorators stack in either order with correct results; compile-time `Store` guards.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/01-implicit-interface-satisfaction/10-decorator-preserves-interface/cmd/demo
cd go-solutions/08-interfaces/01-implicit-interface-satisfaction/10-decorator-preserves-interface
```

### Why decoration is clean only over a narrow interface

A decorator is a `Store` that wraps a `Store`. `CachingStore` holds a delegate
`Store` and adds a read-through cache: on `Get`, it returns a cached value if
present (a hit) or asks the delegate and caches the result (a miss); on `Set`, it
writes through to the delegate and updates its cache. `MetricsStore` holds a
delegate and counts every call before forwarding. Both satisfy `Store`, so either
can wrap either, and both can wrap a `MemoryStore`. `metrics(cache(memory))`
type-checks because each layer *is* a `Store` and *accepts* a `Store`, and the
`MemoryStore` at the bottom never knows it is decorated — that transparency is the
gift of implicit satisfaction.

The reason decoration is clean here is that `Store` is narrow: two methods. Each
decorator forwards two methods and adds its behavior. Over a twelve-method
interface, every decorator would be ten lines of pure pass-through boilerplate
plus the two it cares about — the width tax the lesson keeps warning about. Narrow
interfaces are what make the decorator pattern ergonomic rather than tedious.

Counters use `sync/atomic.Int64` so they are correct under concurrent access
without a mutex; the cache map is guarded by a `sync.RWMutex`. The compile-time
guard `var _ Store = (*CachingStore)(nil)` proves the decorator still satisfies the
interface it decorates — if a signature drifted, the wrapper would stop being a
drop-in replacement, and the guard catches it.

Create `store.go`:

```go
package decorate

import (
	"errors"
	"sync"
)

// ErrNotFound is returned by Get when the key is absent.
var ErrNotFound = errors.New("key not found")

// Store is the narrow interface every layer satisfies.
type Store interface {
	Get(key string) (string, error)
	Set(key, value string) error
}

// MemoryStore is the base implementation.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]string)}
}

func (m *MemoryStore) Get(key string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *MemoryStore) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = value
	return nil
}

var _ Store = (*MemoryStore)(nil)
```

Create `cache.go`:

```go
package decorate

import (
	"sync"
	"sync/atomic"
)

// CachingStore decorates a Store with a read-through cache and hit/miss
// counters. It satisfies Store, so it is a drop-in replacement for the store it
// wraps.
type CachingStore struct {
	delegate Store
	mu       sync.RWMutex
	cache    map[string]string
	hits     atomic.Int64
	misses   atomic.Int64
}

func NewCachingStore(delegate Store) *CachingStore {
	return &CachingStore{delegate: delegate, cache: make(map[string]string)}
}

// Get returns a cached value on a hit; on a miss it consults the delegate and
// caches the result. A hit does not touch the delegate.
func (c *CachingStore) Get(key string) (string, error) {
	c.mu.RLock()
	v, ok := c.cache[key]
	c.mu.RUnlock()
	if ok {
		c.hits.Add(1)
		return v, nil
	}

	c.misses.Add(1)
	v, err := c.delegate.Get(key)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.cache[key] = v
	c.mu.Unlock()
	return v, nil
}

// Set writes through to the delegate and updates the cache.
func (c *CachingStore) Set(key, value string) error {
	if err := c.delegate.Set(key, value); err != nil {
		return err
	}
	c.mu.Lock()
	c.cache[key] = value
	c.mu.Unlock()
	return nil
}

// Hits and Misses expose the counters.
func (c *CachingStore) Hits() int64   { return c.hits.Load() }
func (c *CachingStore) Misses() int64 { return c.misses.Load() }

var _ Store = (*CachingStore)(nil)
```

Create `metrics.go`:

```go
package decorate

import "sync/atomic"

// MetricsStore decorates a Store, counting Get and Set calls. It satisfies Store.
type MetricsStore struct {
	delegate Store
	gets     atomic.Int64
	sets     atomic.Int64
}

func NewMetricsStore(delegate Store) *MetricsStore {
	return &MetricsStore{delegate: delegate}
}

func (m *MetricsStore) Get(key string) (string, error) {
	m.gets.Add(1)
	return m.delegate.Get(key)
}

func (m *MetricsStore) Set(key, value string) error {
	m.sets.Add(1)
	return m.delegate.Set(key, value)
}

func (m *MetricsStore) Gets() int64 { return m.gets.Load() }
func (m *MetricsStore) Sets() int64 { return m.sets.Load() }

var _ Store = (*MetricsStore)(nil)
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/decorate"
)

func main() {
	// Stack: metrics wraps cache wraps memory. Each layer satisfies Store.
	base := decorate.NewMemoryStore()
	_ = base.Set("k", "v") // seed the base directly, so the cache is cold

	cache := decorate.NewCachingStore(base)
	metrics := decorate.NewMetricsStore(cache)

	var store decorate.Store = metrics // used through the narrow interface

	_, _ = store.Get("k") // miss -> consults delegate, fills cache
	_, _ = store.Get("k") // hit  -> served from cache

	v, _ := store.Get("k") // hit
	fmt.Printf("value: %s\n", v)
	fmt.Printf("metrics gets=%d sets=%d\n", metrics.Gets(), metrics.Sets())
	fmt.Printf("cache hits=%d misses=%d\n", cache.Hits(), cache.Misses())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
value: v
metrics gets=3 sets=0
cache hits=2 misses=1
```

### Tests

`countingStore` is a spy `Store` that records how many times its `Get` was called.
`TestCacheHitSkipsDelegate` sets a key, reads it twice, and asserts the delegate
`Get` ran once — proving the second read was a cache hit. `TestStackingOrder`
builds the decorators in both orders and asserts correct results either way.

Create `store_test.go`:

```go
package decorate

import (
	"sync"
	"sync/atomic"
	"testing"
)

// countingStore is a spy Store that counts delegate Get calls.
type countingStore struct {
	mu   sync.Mutex
	data map[string]string
	getN atomic.Int64
}

func newCountingStore() *countingStore {
	return &countingStore{data: make(map[string]string)}
}

func (s *countingStore) Get(key string) (string, error) {
	s.getN.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (s *countingStore) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[key] = value
	return nil
}

func TestCacheHitSkipsDelegate(t *testing.T) {
	t.Parallel()

	spy := newCountingStore()
	cache := NewCachingStore(spy)

	// Seed the delegate directly so the cache does not yet know the key; the
	// first cache.Get is then a real miss that consults the delegate.
	if err := spy.Set("k", "v"); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if v, err := cache.Get("k"); err != nil || v != "v" {
			t.Fatalf("Get = %q,%v, want v,nil", v, err)
		}
	}

	// First Get was a miss (one delegate call); the next two were cache hits.
	if got := spy.getN.Load(); got != 1 {
		t.Fatalf("delegate Get calls = %d, want 1 (cache should absorb the rest)", got)
	}
	if cache.Hits() != 2 || cache.Misses() != 1 {
		t.Fatalf("hits=%d misses=%d, want 2 and 1", cache.Hits(), cache.Misses())
	}
}

func TestStackingOrder(t *testing.T) {
	t.Parallel()

	// metrics(cache(memory))
	a := NewMetricsStore(NewCachingStore(NewMemoryStore()))
	// cache(metrics(memory))
	b := NewCachingStore(NewMetricsStore(NewMemoryStore()))

	for _, s := range []Store{a, b} {
		if err := s.Set("x", "1"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.Get("x"); err != nil || v != "1" {
			t.Fatalf("Get = %q,%v, want 1,nil (order-independent)", v, err)
		}
	}
}

func TestDecoratorsSatisfyStore(t *testing.T) {
	t.Parallel()

	var _ Store = (*CachingStore)(nil)
	var _ Store = (*MetricsStore)(nil)
}

func TestConcurrentGetIsRaceFree(t *testing.T) {
	t.Parallel()

	cache := NewCachingStore(NewMemoryStore())
	_ = cache.Set("k", "v")

	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cache.Get("k")
		}()
	}
	wg.Wait()

	if cache.Hits()+cache.Misses() != 200 {
		t.Fatalf("hits+misses = %d, want 200", cache.Hits()+cache.Misses())
	}
}
```

## Review

The decorator is correct when it satisfies `Store`, forwards to its delegate, and
adds its behavior transparently — a cache hit must not touch the delegate, which
`TestCacheHitSkipsDelegate` proves with a spy that counts delegate `Get` calls.
Because every layer is a `Store` and accepts a `Store`, decorators stack in any
order (`TestStackingOrder`) and the base never learns it is wrapped. Counters use
`atomic.Int64` so they are correct under the concurrent `Get` storm without a lock,
while the cache map keeps its `RWMutex`. The lesson the pattern teaches: decoration
is ergonomic only over a narrow interface — two forwarded methods, not twelve. The
common mistake is a decorator that drifts from the interface after a signature
change; the `var _ Store` guard on each wrapper turns that into a compile error.
Run `go test -race` to confirm the counters and cache are race-free.

## Resources

- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — lock-free counters for decorators.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — composing behavior by wrapping interfaces.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — guarding the decorator's cache map.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../02-empty-interface-and-any/00-concepts.md](../02-empty-interface-and-any/00-concepts.md)
