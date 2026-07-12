# Exercise 1: Sharded TTL Cache Core

Your service reads user profiles from Postgres on every request; the profiles
change rarely, and the database is doing 20k identical reads per second. The
fix is an in-process cache — and the moment more than one goroutine touches
it, that cache is a concurrency artifact. This exercise builds the core: a
generic, lock-striped, TTL-aware cache whose contract is pinned by tests.

## What you'll build

```text
shardedttl/                      independent module: example.com/shardedttl
  go.mod
  cache/
    cache.go                     entry[V] (expiresAt, expired), shard[V] (RWMutex + map),
                                 Cache[V]: New, shardFor (FNV-1a mod N),
                                 Set, Get, Delete, Size
    cache_test.go                contract tests: roundtrip, missing, expired, pre-expiry,
                                 delete idempotency, Size skips expired, overwrite replaces
                                 value AND TTL, shard distribution, ExampleCache
  cmd/
    demo/
      main.go                    set/get/delete/expiry walkthrough with real output
```

- Files: `cache/cache.go`, `cache/cache_test.go`, `cmd/demo/main.go`.
- Implement: `Cache[V]` with lock-striped shards, lazy TTL expiry on `Get`, idempotent `Delete`, and a `Size` that skips expired entries; non-positive TTL means "no expiration".
- Test: table of contract tests under `t.Parallel()`, including `TestSetOverwritesTTL` and `TestShardingDistributesKeys`, plus a testable Example.
- Verify: `go test -count=1 -race ./...`

### The design: shards, not one lock

A single mutex over one map serializes every operation; under 40 concurrent
goroutines your cache is a queue. Lock striping splits the keyspace into N
shards, each a `map[string]*entry[V]` guarded by its own `sync.RWMutex`.
Operations on different shards never contend. Shard selection must be fast and
stable: FNV-1a from `hash/fnv` hashes the key bytes, and the 32-bit sum modulo
the shard count picks the shard. The hash need not be cryptographic — only
cheap and well-distributed — and `fnv.New32a()` costs one small allocation per
call, which is acceptable here and eliminated by hand-inlining the FNV loop
when profiles say it matters.

Reads take `RLock`, so any number of concurrent `Get`s on one shard proceed
together; only `Set` and `Delete` take the exclusive `Lock`. This is the
RWMutex bargain, and it holds precisely because this `Get` does *no*
bookkeeping — no LRU move, no counter under the lock. Later exercises that add
recency tracking will have to give this up, deliberately.

### TTL is lazy here

Each entry stores an `expiresAt` (zero time means "never expires" — that is
what a non-positive TTL maps to). `Get` checks `expired(time.Now())` and
treats a dead entry as missing, returning the zero value of `V`. Nothing in
this module deletes expired entries; `Size` walks all shards and *counts only
live ones*, so callers see the truthful logical size even though dead entries
still occupy memory. Reclaiming that memory is a background sweeper's job —
exercise 2 — and keeping it out of this module keeps the core contract small
enough to pin completely with tests.

One subtlety in `Get`: the value is copied out *before* `RUnlock`, and the
early-return path unlocks explicitly. With a pointer-sized `V` the copy is
trivial; what matters is that no shared state is touched after the lock is
released.

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

// expired reports whether the entry's deadline has passed. A zero
// expiresAt means the entry never expires.
func (e *entry[V]) expired(now time.Time) bool {
	return !e.expiresAt.IsZero() && now.After(e.expiresAt)
}

type shard[V any] struct {
	mu    sync.RWMutex
	items map[string]*entry[V]
}

// Cache is a lock-striped, TTL-aware map. Keys hash to one of N shards,
// each guarded by its own RWMutex, so operations on different shards
// never contend.
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
// means "no expiration". A later Set fully replaces an earlier one,
// including its TTL.
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

// Get returns the value for key. The second result is false if the
// key is missing or expired; expired entries are treated as absent.
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
// Expired-but-unswept entries still occupy memory but are not counted.
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

### The demo

The demo walks the whole contract against the wall clock: a session that
survives its read, a delete, and an expiry observed after a real 20 ms sleep.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/shardedttl/cache"
)

func main() {
	c := cache.New[string](16)

	c.Set("session:alice", "token-a", time.Hour)
	c.Set("session:bob", "token-b", 10*time.Millisecond)
	c.Set("config:theme", "dark", 0) // non-positive TTL: never expires

	if v, ok := c.Get("session:alice"); ok {
		fmt.Printf("alice: %s\n", v)
	}
	fmt.Printf("size: %d\n", c.Size())

	c.Delete("session:alice")
	if _, ok := c.Get("session:alice"); !ok {
		fmt.Println("alice: deleted")
	}

	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("session:bob"); !ok {
		fmt.Println("bob: expired")
	}
	fmt.Printf("final size: %d\n", c.Size())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice: token-a
size: 3
alice: deleted
bob: expired
final size: 1
```

### Tests that pin the contract

Each test pins one clause of the contract. `TestSetOverwritesTTL` is the one
teams forget: a later `Set` must replace not just the value but the deadline —
an implementation that keeps the old `expiresAt` on overwrite serves values
past their intended lifetime. `TestShardingDistributesKeys` is a same-package
test on purpose: it reaches into the unexported shards (under `RLock`) and
proves every stored key landed in exactly one shard — the sum of shard map
lengths equals the number of keys, so no key was duplicated or lost by the
hash-mod routing.

Create `cache/cache_test.go`:

```go
package cache

import (
	"fmt"
	"testing"
	"time"
)

func TestSetGet(t *testing.T) {
	t.Parallel()

	c := New[string](4)
	c.Set("a", "1", time.Minute)
	v, ok := c.Get("a")
	if !ok || v != "1" {
		t.Fatalf("Get(a) = %q ok=%v, want 1 true", v, ok)
	}
}

func TestGetReturnsFalseForMissing(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	if _, ok := c.Get("missing"); ok {
		t.Fatal("Get(missing): ok=true, want false")
	}
}

func TestGetReturnsFalseForExpired(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Set("k", 1, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get(k) after expiry: ok=true, want false")
	}
}

func TestGetReturnsValueBeforeExpiry(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Set("k", 1, time.Hour)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("Get(k) before expiry: %d ok=%v", v, ok)
	}
}

func TestZeroTTLNeverExpires(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Set("k", 1, 0)
	time.Sleep(15 * time.Millisecond)
	if v, ok := c.Get("k"); !ok || v != 1 {
		t.Fatalf("Get(k) with ttl=0: %d ok=%v, want 1 true", v, ok)
	}
}

func TestDeleteRemovesEntry(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Set("k", 1, time.Hour)
	c.Delete("k")
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get(k) after Delete: ok=true")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Delete("missing") // must not panic
	c.Delete("missing") // twice is still fine
}

func TestSizeIgnoresExpired(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Set("live", 1, time.Hour)
	c.Set("dead", 2, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if got := c.Size(); got != 1 {
		t.Fatalf("Size = %d, want 1 (only live entry)", got)
	}
}

func TestSetOverwritesTTL(t *testing.T) {
	t.Parallel()

	c := New[int](4)
	c.Set("k", 1, time.Hour)
	c.Set("k", 2, 10*time.Millisecond) // replaces value AND deadline
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get(k): ok=true, want false (second Set's TTL must win)")
	}
}

func TestShardingDistributesKeys(t *testing.T) {
	t.Parallel()

	c := New[string](4)
	for i := range 100 {
		c.Set(fmt.Sprintf("k-%d", i), "v", time.Hour)
	}
	seen := 0
	for _, s := range c.shards {
		s.mu.RLock()
		seen += len(s.items)
		s.mu.RUnlock()
	}
	if seen != 100 {
		t.Fatalf("entries across shards = %d, want 100", seen)
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

The contract this module pins is small and exact: `Get` returns
`(zero, false)` iff the key is missing or expired; a non-positive TTL never
expires; `Delete` never panics; `Size` counts only live entries; a later `Set`
replaces both value and deadline; and every key routes to exactly one shard.
If any prose claim above surprised you, find the test that pins it — every
clause has one.

The mistakes to watch for are the ones the tests would catch. Keeping the old
`expiresAt` on overwrite fails `TestSetOverwritesTTL`. Forgetting the
`IsZero()` guard in `expired` makes ttl=0 entries instantly dead and fails
`TestZeroTTLNeverExpires`. Touching `e.value` after `RUnlock` is a data race
that `-race` flags once exercise 3's load harness hammers it. And note what
this `Get` does *not* do: no recency bookkeeping, no counters — that is what
keeps `RLock` legitimate, and later exercises pay real costs to change it.

## Resources

- [`sync` package — `RWMutex`](https://pkg.go.dev/sync#RWMutex) — reader/writer lock semantics and the writer-preference caveat.
- [`hash/fnv`](https://pkg.go.dev/hash/fnv) — FNV-1a, the conventional cheap shard-selection hash.
- [`time` package](https://pkg.go.dev/time) — `Time.After`, `Time.IsZero`, and why zero time is a natural "never" sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-background-sweeper-lifecycle.md](02-background-sweeper-lifecycle.md)
