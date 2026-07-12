# Exercise 6: TTL Cache with Clock Injection and Eviction Hook

A TTL cache expires entries based on the clock and evicts the oldest when full —
two behaviors that are painful to test if the cache reads `time.Now()` and hides
its evictions. This module injects both through options: `WithClock` makes expiry
a pure function of a clock the test advances by hand, and `WithOnEvict` exposes
every eviction as an observable hook. It also shows generic option types.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
ttlcache/                        independent module: example.com/ttlcache
  go.mod                         go 1.26
  cache.go                       Cache[K,V], Reason, Option[K,V], NewCache,
                                 WithTTL, WithMaxEntries, WithClock, WithOnEvict, Get, Set, Len
  cmd/
    demo/
      main.go                    drives a manual clock to show capacity and expiry eviction
  cache_test.go                  fake clock advances past TTL; capacity eviction; -race concurrency
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: `NewCache[K,V](opts...) (*Cache[K,V], error)` whose `Get`/`Set`/`Len` respect TTL through an injected clock, evict the oldest entry at capacity, and report every eviction to an injected hook with a `Reason`.
- Test: advance a fake clock past the TTL and assert `Get` misses with an `Expired` eviction; fill to capacity and assert the oldest is evicted with a `Capacity` reason; run a `-race` concurrency check.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/06-clock-injection-cache-options/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/06-clock-injection-cache-options
```

### Injecting a clock and a hook, both generic

Two collaborators are injected here. `WithClock(func() time.Time)` replaces the
call to `time.Now()`, so a test can freeze the clock and advance it by
reassigning a captured variable — a two-minute TTL is checked in zero real time.
`WithOnEvict(func(K, V, Reason))` turns an internal event (an entry being removed)
into a hook the caller can observe for metrics, logging, or cleanup of the evicted
value. Both default to harmless behavior: the clock defaults to `time.Now` and the
hook defaults to nil (no-op). Production passes neither; tests pass both.

Because the cache is generic over its key and value types, the options are generic
too: `type Option[K comparable, V any] func(*Cache[K, V]) error`. That is more
verbose at the call site — `WithTTL[string, int](...)` — but it is the price of a
reusable generic type whose options still validate and still inject. It
demonstrates that the options pattern composes cleanly with generics.

### Firing the hook outside the lock

One subtle correctness point: the eviction hook must not be called while the
cache's mutex is held. If it were, a hook that called back into the cache (say, to
record a metric that itself touches the cache) would deadlock. So `Get` and `Set`
remove the entry under the lock, release the lock, and only then fire the hook.
This is a small discipline that keeps an injected callback from turning into a
self-deadlock — a real hazard whenever you invoke caller-supplied code from inside
a critical section.

Create `cache.go`:

```go
package ttlcache

import (
	"fmt"
	"sync"
	"time"
)

// Reason explains why an entry was evicted.
type Reason int

const (
	// Expired means the entry's TTL elapsed.
	Expired Reason = iota
	// Capacity means the entry was the oldest and the cache was full.
	Capacity
)

func (r Reason) String() string {
	switch r {
	case Expired:
		return "expired"
	case Capacity:
		return "capacity"
	default:
		return "unknown"
	}
}

type entry[V any] struct {
	value   V
	expires time.Time
}

// Cache is a concurrency-safe TTL cache with oldest-first capacity eviction.
type Cache[K comparable, V any] struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	onEvict    func(K, V, Reason)
	items      map[K]entry[V]
	order      []K
}

// Option configures a Cache and may reject invalid input.
type Option[K comparable, V any] func(*Cache[K, V]) error

// NewCache builds a Cache, seeding defaults and applying opts.
func NewCache[K comparable, V any](opts ...Option[K, V]) (*Cache[K, V], error) {
	c := &Cache[K, V]{
		ttl:        time.Minute,
		maxEntries: 1024,
		now:        time.Now,
		items:      make(map[K]entry[V]),
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Set stores value under key, expiring it ttl from the injected clock's now.
// If the cache is full and key is new, the oldest entry is evicted.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	var (
		evKey    K
		evVal    V
		didEvict bool
	)
	if _, ok := c.items[key]; !ok {
		if len(c.items) >= c.maxEntries {
			oldest := c.order[0]
			evKey, evVal, didEvict = oldest, c.items[oldest].value, true
			c.removeLocked(oldest)
		}
		c.order = append(c.order, key)
	}
	c.items[key] = entry[V]{value: value, expires: c.now().Add(c.ttl)}
	c.mu.Unlock()

	if didEvict {
		c.fireEvict(evKey, evVal, Capacity)
	}
}

// Get returns the value if present and unexpired. An expired entry is removed
// and reported to the eviction hook.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	e, ok := c.items[key]
	if !ok {
		c.mu.Unlock()
		var zero V
		return zero, false
	}
	if !c.now().Before(e.expires) {
		c.removeLocked(key)
		c.mu.Unlock()
		c.fireEvict(key, e.value, Expired)
		var zero V
		return zero, false
	}
	c.mu.Unlock()
	return e.value, true
}

// Len reports the number of entries still stored.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

func (c *Cache[K, V]) removeLocked(key K) {
	delete(c.items, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

func (c *Cache[K, V]) fireEvict(key K, value V, reason Reason) {
	if c.onEvict != nil {
		c.onEvict(key, value, reason)
	}
}

// WithTTL sets the entry lifetime (> 0).
func WithTTL[K comparable, V any](ttl time.Duration) Option[K, V] {
	return func(c *Cache[K, V]) error {
		if ttl <= 0 {
			return fmt.Errorf("ttl must be positive, got %s", ttl)
		}
		c.ttl = ttl
		return nil
	}
}

// WithMaxEntries caps the number of stored entries (> 0).
func WithMaxEntries[K comparable, V any](n int) Option[K, V] {
	return func(c *Cache[K, V]) error {
		if n <= 0 {
			return fmt.Errorf("maxEntries must be positive, got %d", n)
		}
		c.maxEntries = n
		return nil
	}
}

// WithClock injects the clock used for expiry (nil rejected).
func WithClock[K comparable, V any](now func() time.Time) Option[K, V] {
	return func(c *Cache[K, V]) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		c.now = now
		return nil
	}
}

// WithOnEvict injects the eviction hook (nil rejected).
func WithOnEvict[K comparable, V any](fn func(K, V, Reason)) Option[K, V] {
	return func(c *Cache[K, V]) error {
		if fn == nil {
			return fmt.Errorf("onEvict is nil")
		}
		c.onEvict = fn
		return nil
	}
}
```

### The runnable demo

The demo drives a manual clock so eviction is deterministic: it fills a
two-entry cache, adds a third to force a capacity eviction, then advances the
clock past the TTL and reads an entry to force an expiry eviction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	c, err := ttlcache.NewCache[string, string](
		ttlcache.WithTTL[string, string](time.Minute),
		ttlcache.WithMaxEntries[string, string](2),
		ttlcache.WithClock[string, string](clock),
		ttlcache.WithOnEvict[string, string](func(k, v string, r ttlcache.Reason) {
			fmt.Printf("evicted %s (%s)\n", k, r)
		}),
	)
	if err != nil {
		panic(err)
	}

	c.Set("s1", "alice")
	c.Set("s2", "bob")
	c.Set("s3", "carol") // s1 is oldest, evicted for capacity

	current = current.Add(2 * time.Minute) // past the TTL
	_, ok := c.Get("s2")                   // expired, evicted on read
	fmt.Printf("s2 present after TTL: %t\n", ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
evicted s1 (capacity)
evicted s2 (expired)
s2 present after TTL: false
```

### Tests

`TestExpiryWithFakeClock` sets an entry, advances the fake clock past the TTL, and
asserts `Get` misses and the hook fired with `Expired`. `TestCapacityEviction`
fills the cache and inserts one more, asserting the oldest was evicted with
`Capacity`. `TestConcurrentAccess` runs `-race` over concurrent `Set`/`Get`.

Create `cache_test.go`:

```go
package ttlcache

import (
	"sync"
	"testing"
	"time"
)

func TestExpiryWithFakeClock(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	var evicted []string
	c, err := NewCache[string, int](
		WithTTL[string, int](time.Minute),
		WithClock[string, int](func() time.Time { return current }),
		WithOnEvict[string, int](func(k string, v int, r Reason) {
			evicted = append(evicted, k+":"+r.String())
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	c.Set("a", 1)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("Get(a) missed right after Set")
	}

	current = base.Add(2 * time.Minute) // past TTL
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get(a) still present after TTL")
	}

	if len(evicted) != 1 || evicted[0] != "a:expired" {
		t.Fatalf("evicted = %v, want [a:expired]", evicted)
	}
}

func TestCapacityEviction(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

	var evicted []string
	c, err := NewCache[string, int](
		WithMaxEntries[string, int](2),
		WithClock[string, int](func() time.Time { return base }),
		WithOnEvict[string, int](func(k string, v int, r Reason) {
			evicted = append(evicted, k+":"+r.String())
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	c.Set("a", 1)
	c.Set("b", 2)
	c.Set("c", 3) // a is oldest, evicted

	if len(evicted) != 1 || evicted[0] != "a:capacity" {
		t.Fatalf("evicted = %v, want [a:capacity]", evicted)
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("a should have been evicted")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	t.Parallel()

	c, err := NewCache[int, int]()
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(i, i)
			c.Get(i)
		}()
	}
	wg.Wait()
}
```

## Review

The cache is correct when expiry is a pure function of the injected clock and the
stored deadline, and when every removal — expiry or capacity — reaches the hook
exactly once. `TestExpiryWithFakeClock` proves the first by advancing a frozen
clock rather than sleeping, and `TestCapacityEviction` proves oldest-first
eviction and the `Capacity` reason. The design point worth keeping is firing the
hook after releasing the mutex: a caller-supplied callback invoked under the lock
is a latent deadlock, and moving it outside is what makes `WithOnEvict` safe to
hand arbitrary code. The `-race` test confirms the mutex actually guards the map
and order slice under concurrent access.

## Resources

- [Go generics: type parameters](https://go.dev/doc/tutorial/generics)
- [time package](https://pkg.go.dev/time)
- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [Effective Go: concurrency](https://go.dev/doc/effective_go#concurrency)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-retrier-backoff-options.md](05-retrier-backoff-options.md) | Next: [07-generic-options-errors-join.md](07-generic-options-errors-join.md)
