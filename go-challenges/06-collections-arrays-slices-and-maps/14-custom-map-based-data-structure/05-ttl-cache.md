# Exercise 5: TTL cache with lazy and active expiry

Auth-token introspection is expensive — a round trip to an identity provider — so
you cache the result for a short window. But a cached authorization must *expire*:
a revoked token should stop being honored quickly. This module builds a
time-to-live cache that expires entries two ways at once — lazily on read, and
actively via a background janitor — and it does so with an injected clock so the
expiry logic is tested deterministically, with no `time.Sleep` in the assertions.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
ttlcache/                  independent module: example.com/ttlcache
  go.mod
  ttlcache.go              type TTLCache[K,V]; New, Set, Get, Len, Sweep, StartJanitor
  cmd/
    demo/
      main.go              introspection cache driven by a manual clock
  ttlcache_test.go         lazy/active expiry with a fake clock, janitor lifecycle
```

- Files: `ttlcache.go`, `cmd/demo/main.go`, `ttlcache_test.go`.
- Implement: `TTLCache[K comparable, V any]` holding `map[K]entry{value, deadline}` under a `sync.RWMutex`; `Get` treats an expired entry as a miss (lazy), `Sweep` deletes expired entries (active), and `StartJanitor(ctx, interval)` runs `Sweep` on a ticker until the context is cancelled.
- Test: with an injected clock, hit before deadline and miss after; `Sweep` reclaims keys; the janitor reclaims keys and its goroutine stops when the context is cancelled (verified via a done channel).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/ttlcache/cmd/demo
cd ~/go-exercises/ttlcache
go mod init example.com/ttlcache
```

### Lazy plus active, and why the clock is injected

Expiry has two modes and a real cache uses both. **Lazy** eviction is enforced on
read: `Get` compares the entry's deadline against "now" and reports a miss if the
entry has expired — but it leaves the entry in the map. That is cheap and always
correct, but a token that is written once and never read again lingers forever,
holding its memory. **Active** eviction is a background janitor: a goroutine on a
ticker calls `Sweep`, which walks the map and deletes every expired entry,
reclaiming that memory. Lazy keeps reads correct; active keeps memory bounded.

Two design points carry the module. First, the janitor must be *stoppable*. It is
driven by a `context.Context`; its `select` watches both the ticker and
`ctx.Done()`, and `StartJanitor` returns a `done` channel that closes when the
goroutine exits, so a caller (or a test) can prove the goroutine actually stopped
rather than leaked. A janitor with no shutdown path is a goroutine leak on every
cache instance.

Second, the clock is injected. Every deadline comparison goes through a `now
func() time.Time` field, defaulting to `time.Now` in production but overridable in
a test. This is what makes the expiry logic deterministic: a test advances a fake
clock instantly and asserts the boundary exactly, with no real sleeping and no
scheduler slack to flake on. (In production, `time.Now` carries a monotonic
reading, so the deadline math stays correct even if the wall clock is stepped by
NTP — injecting the clock does not sacrifice that, since production still uses
`time.Now`.)

Create `ttlcache.go`:

```go
package ttlcache

import (
	"context"
	"sync"
	"time"
)

type entry[V any] struct {
	value    V
	deadline time.Time
}

// TTLCache is a concurrency-safe cache whose entries expire after a fixed TTL.
// Expiry is lazy on Get and active via Sweep/StartJanitor. The clock is read
// through now, which tests override for deterministic expiry.
type TTLCache[K comparable, V any] struct {
	mu    sync.RWMutex
	items map[K]entry[V]
	ttl   time.Duration
	now   func() time.Time
}

// Option configures a TTLCache.
type Option[K comparable, V any] func(*TTLCache[K, V])

// WithClock overrides the clock (default time.Now) for deterministic tests.
func WithClock[K comparable, V any](now func() time.Time) Option[K, V] {
	return func(c *TTLCache[K, V]) { c.now = now }
}

// New returns a cache whose entries live for ttl.
func New[K comparable, V any](ttl time.Duration, opts ...Option[K, V]) *TTLCache[K, V] {
	c := &TTLCache[K, V]{
		items: make(map[K]entry[V]),
		ttl:   ttl,
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Set stores value under key, expiring it ttl from now.
func (c *TTLCache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, deadline: c.now().Add(c.ttl)}
}

// Get returns the value if present and unexpired. An expired entry reports a
// miss (lazy eviction) but is not deleted here.
func (c *TTLCache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	e, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || !c.now().Before(e.deadline) {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Len reports the number of entries still stored, expired or not.
func (c *TTLCache[K, V]) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Sweep deletes every expired entry and returns how many it removed (active
// eviction).
func (c *TTLCache[K, V]) Sweep() int {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	removed := 0
	for k, e := range c.items {
		if !now.Before(e.deadline) {
			delete(c.items, k)
			removed++
		}
	}
	return removed
}

// StartJanitor runs Sweep every interval in a background goroutine until ctx is
// cancelled. The returned channel closes when the goroutine has exited.
func (c *TTLCache[K, V]) StartJanitor(ctx context.Context, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.Sweep()
			}
		}
	}()
	return done
}
```

### The runnable demo

The demo drives expiry with a manual clock so it is deterministic (no sleeping):
it caches an introspection result, reads it back, advances past the TTL to show
the lazy miss, then calls `Sweep` to show the memory being reclaimed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	current := time.Unix(0, 0)
	clock := func() time.Time { return current }

	c := ttlcache.New[string, string](30*time.Second, ttlcache.WithClock[string, string](clock))
	c.Set("token-abc", "user-42")

	v, ok := c.Get("token-abc")
	fmt.Printf("t=0s   introspect=%q ok=%v\n", v, ok)

	current = current.Add(35 * time.Second) // past the 30s TTL
	_, ok = c.Get("token-abc")
	fmt.Printf("t=35s  cached=%v\n", ok)

	fmt.Printf("entries before sweep: %d\n", c.Len())
	c.Sweep()
	fmt.Printf("entries after sweep:  %d\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
t=0s   introspect="user-42" ok=true
t=35s  cached=false
entries before sweep: 1
entries after sweep:  0
```

### Tests

The expiry tests use an injected fake clock (an atomic nanosecond counter, so the
janitor goroutine can read it while the test advances it under `-race`), which
makes them deterministic with no real sleeping. The janitor tests use a real short
ticker and prove two things: the janitor reclaims expired keys, and its goroutine
stops when the context is cancelled — the done channel closing is the proof it did
not leak.

Create `ttlcache_test.go`:

```go
package ttlcache

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a controllable clock safe for concurrent reads and advances.
type fakeClock struct{ ns atomic.Int64 }

func (f *fakeClock) Now() time.Time { return time.Unix(0, f.ns.Load()) }

func (f *fakeClock) Advance(d time.Duration) { f.ns.Add(int64(d)) }

func TestHitBeforeDeadline(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	c := New[string, int](time.Second, WithClock[string, int](clk.Now))
	c.Set("k", 7)
	clk.Advance(500 * time.Millisecond) // still before the 1s deadline
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Get(k) = %d,%v before deadline; want 7,true", v, ok)
	}
}

func TestMissAfterDeadline(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	c := New[string, int](time.Second, WithClock[string, int](clk.Now))
	c.Set("k", 7)
	clk.Advance(2 * time.Second) // past the deadline
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get(k) still present after TTL expired")
	}
}

func TestSweepReclaimsMemory(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	c := New[string, int](time.Second, WithClock[string, int](clk.Now))
	c.Set("a", 1)
	c.Set("b", 2)
	clk.Advance(2 * time.Second)
	if removed := c.Sweep(); removed != 2 {
		t.Fatalf("Sweep removed %d, want 2", removed)
	}
	if c.Len() != 0 {
		t.Fatalf("Len after sweep = %d, want 0", c.Len())
	}
}

func TestJanitorReclaimsExpiredKeys(t *testing.T) {
	t.Parallel()

	clk := &fakeClock{}
	c := New[string, int](time.Second, WithClock[string, int](clk.Now))
	c.Set("k", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := c.StartJanitor(ctx, time.Millisecond)

	clk.Advance(2 * time.Second) // entry is now expired

	deadline := time.Now().Add(2 * time.Second)
	for c.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("janitor did not reclaim the expired key in time")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("janitor did not stop after cancel")
	}
}

func TestJanitorStopsOnCancel(t *testing.T) {
	t.Parallel()

	c := New[string, int](time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartJanitor(ctx, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("janitor goroutine leaked: done never closed")
	}
}

func Example() {
	clk := &fakeClock{}
	c := New[string, string](time.Minute, WithClock[string, string](clk.Now))
	c.Set("session", "alice")
	v, ok := c.Get("session")
	fmt.Println(v, ok)
	// Output: alice true
}
```

## Review

The cache is correct when `Get` returns a miss exactly when the key is absent or
`now()` is not before its deadline (lazy), and `Sweep` deletes precisely the
expired entries (active). The two mistakes this module targets are shipping a
janitor with no shutdown path — a goroutine leak that `TestJanitorStopsOnCancel`
would catch — and comparing deadlines against a clock the test cannot control,
which forces real sleeps and flaky assertions; injecting `now` removes both. Note
that lazy expiry means `Len` can count an entry that `Get` already treats as
absent; reclaiming that memory is `Sweep`'s job, not `Get`'s. Run
`go test -count=1 -race ./...` to confirm the janitor and readers do not race on
the map.

## Resources

- [`time` package](https://pkg.go.dev/time) — `time.Now`, `time.Time.Add`, `time.Time.Before`, `time.NewTicker`, and the monotonic clock note.
- [`context` package](https://pkg.go.dev/context) — `context.WithCancel` and `Context.Done` for the janitor's shutdown.
- [`sync` package](https://pkg.go.dev/sync) — `RWMutex` semantics for a read-mostly cache.
- [`sync/atomic` package](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, used for the race-free fake clock.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-lru-cache.md](04-lru-cache.md) | Next: [06-sharded-map.md](06-sharded-map.md)
