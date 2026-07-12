# Exercise 1: In-Memory TTL Cache: comma-ok, delete, and sorted Keys under -race

An in-memory cache keyed by string with a per-entry TTL is the first map-backed
artifact most backends grow: a session store, a short-lived token cache, a
memoized lookup. This exercise builds it the production way — an `RWMutex`-guarded
`map[string]entry`, `Get` reporting presence through comma-ok, and `Keys`
returning live keys in sorted order so callers never see the map's randomized
iteration.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
ttlcache/                  independent module: example.com/ttlcache
  go.mod
  cache.go                 type Cache; New, Set, Get, Delete, Keys, Len (RWMutex-guarded map)
  cmd/
    demo/
      main.go              runnable demo: set, read, expire, delete, list keys
  cache_test.go            roundtrip, missing key, TTL expiry, delete, sorted Keys, zero-TTL, -race
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: a `Cache` over `map[string]entry` with `New`, `Set(key, value, ttl)` (ttl=0 means no expiry), `Get(key) (any, bool)` via comma-ok, `Delete`, `Keys` returning live keys sorted, and `Len`.
- Test: set/get roundtrip, missing key, TTL expiry, delete removes, delete-missing is a no-op, `Keys` sorted and excluding expired, zero-TTL never expires, and 100 concurrent `Set`s under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/01-ttl-cache/cmd/demo
cd go-solutions/06-collections-arrays-slices-and-maps/04-maps-creation-access-iteration/01-ttl-cache
```

### Why RWMutex, comma-ok, and a zero deadline

Three map facts shape this type. First, a map is not safe for concurrent
read/write and an unsynchronized concurrent write crashes the process with a
fatal `recover` cannot catch, so every access is guarded. Reads take `RLock`
(many readers may proceed together); writes take `Lock`. Second, `Get` must
distinguish "key absent" from "key present holding a zero/nil value", which is
exactly what comma-ok (`e, ok := c.entries[key]`) is for — a bare `c.entries[key]`
would silently treat a missing key as a stored nil. Third, TTL is encoded as an
`expiresAt time.Time`, and a TTL of `0` means "never expires"; the sentinel for
that is the zero `time.Time`, tested with `IsZero`. `Set` records
`time.Now().Add(ttl)` only when `ttl > 0`, leaving `expiresAt` as the zero value
otherwise.

Expiry is *lazy*: `Get` and `Keys` treat an entry whose `expiresAt` is in the
past as absent, but neither deletes it. `Len` therefore counts stored entries,
expired or not — reclaiming an expired-but-never-read entry would be the job of a
background sweeper, deliberately out of scope here. `Keys` collects only the live
keys into a slice and sorts it with `slices.Sort`, because ranging the map
directly would leak its randomized iteration order into the caller's output.

Create `cache.go`:

```go
package ttlcache

import (
	"slices"
	"sync"
	"time"
)

type entry struct {
	value     any
	expiresAt time.Time // zero means "never expires"
}

// Cache is a concurrency-safe string-keyed cache with per-entry TTL. It is
// guarded by an RWMutex: reads take RLock, writes take Lock.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry
}

// New returns an empty, ready-to-use Cache.
func New() *Cache {
	return &Cache{entries: make(map[string]entry)}
}

// Set stores value under key. A ttl of 0 means the entry never expires;
// a positive ttl expires it ttl from now.
func (c *Cache) Set(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	c.entries[key] = entry{value: value, expiresAt: exp}
}

// Get returns (value, true) if key is present and unexpired, else (nil, false).
// Missing and expired are both reported as not-found via the ok result.
func (c *Cache) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.value, true
}

// Delete removes key. It is a no-op if key is absent.
func (c *Cache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// Keys returns the live (non-expired) keys in sorted order.
func (c *Cache) Keys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.entries))
	for k, e := range c.entries {
		if !e.expiresAt.IsZero() && time.Now().After(e.expiresAt) {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// Len reports the number of entries still stored, expired or not.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
```

### The runnable demo

The demo stores two permanent sessions and one short-lived token, reads a hit and
a miss through comma-ok, sleeps past the token's TTL to watch lazy expiry, deletes
a session, and prints the sorted live keys.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/ttlcache"
)

func main() {
	c := ttlcache.New()
	c.Set("session:alice", "token-a", 0)
	c.Set("session:bob", "token-b", 0)
	c.Set("otp:alice", "123456", 30*time.Millisecond)

	if v, ok := c.Get("session:alice"); ok {
		fmt.Printf("hit session:alice = %v\n", v)
	}
	if _, ok := c.Get("session:carol"); !ok {
		fmt.Println("miss session:carol")
	}

	time.Sleep(60 * time.Millisecond) // past the OTP TTL
	if _, ok := c.Get("otp:alice"); !ok {
		fmt.Println("otp:alice expired")
	}

	c.Delete("session:bob")
	fmt.Printf("live keys: %v\n", c.Keys())
	fmt.Printf("stored entries: %d\n", c.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit session:alice = token-a
miss session:carol
otp:alice expired
live keys: [session:alice]
stored entries: 2
```

Note `Len` is `2`, not `1`: the expired `otp:alice` is still stored (lazy expiry),
and `Keys` filters it out while `Len` counts it. `session:alice` is the only live
key because `session:bob` was deleted.

### Tests

The tests pin every contract. `TestTTLExpires` is the core behavior test so a
future "always cache" regression is caught. `TestZeroTTLMeansNoExpiration` pins
the "ttl=0 never expires" contract from the other direction. `TestKeysSorted` and
`TestKeysExcludesExpired` prove `Keys` is both deterministic and expiry-aware.
`TestConcurrentSet` runs 100 goroutines writing 26 distinct keys and asserts
`Len <= 26` under `-race`, proving the mutex actually guards the map.

Create `cache_test.go`:

```go
package ttlcache

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestSetAndGet(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set("a", 1, 0)
	v, ok := c.Get("a")
	if !ok {
		t.Fatal("Get(a) should return ok")
	}
	if v.(int) != 1 {
		t.Fatalf("v = %v, want 1", v)
	}
}

func TestGetMissingKey(t *testing.T) {
	t.Parallel()

	c := New()
	if _, ok := c.Get("missing"); ok {
		t.Fatal("Get(missing) should return false")
	}
}

func TestTTLExpires(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set("a", 1, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get(a) should return false after TTL")
	}
}

func TestZeroTTLMeansNoExpiration(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set("a", 1, 0)
	time.Sleep(50 * time.Millisecond)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("zero-TTL entry must never expire")
	}
}

func TestDeleteRemovesKey(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set("a", 1, 0)
	c.Delete("a")
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get(a) should return false after Delete")
	}
}

func TestDeleteMissingIsNoop(t *testing.T) {
	t.Parallel()

	c := New()
	c.Delete("missing") // must not panic
}

func TestKeysSorted(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set("c", 1, 0)
	c.Set("a", 2, 0)
	c.Set("b", 3, 0)

	if got := c.Keys(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("Keys() = %v, want [a b c]", got)
	}
}

func TestKeysExcludesExpired(t *testing.T) {
	t.Parallel()

	c := New()
	c.Set("a", 1, 0)
	c.Set("b", 2, 10*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if got := c.Keys(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("Keys() = %v, want [a]", got)
	}
}

func TestConcurrentSet(t *testing.T) {
	t.Parallel()

	c := New()
	const n = 100
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Set(string(rune('a'+i%26)), i, 0)
		}(i)
	}
	wg.Wait()

	if got := c.Len(); got > 26 {
		t.Fatalf("Len() = %d, want <= 26", got)
	}
}

func ExampleCache() {
	c := New()
	c.Set("answer", 42, 0)
	v, ok := c.Get("answer")
	fmt.Println(v, ok)
	// Output: 42 true
}
```

## Review

The cache is correct when `Get` reports presence purely through comma-ok and the
entry's deadline: it returns `(nil, false)` exactly when the key is missing or
`time.Now()` is after a non-zero `expiresAt`, and `ttl=0` (zero `expiresAt`,
caught by `IsZero`) never expires. `Keys` collects live keys and sorts them, so
its output is deterministic no matter how the map iterates — the whole reason it
does not just range the map. `Len` counting an expired entry is correct: expiry
is lazy, and reclaiming the memory is a sweeper's job, not `Get`'s. The mistakes
to avoid are reading `c.entries[key]` without the `ok` (which conflates absent
with a stored nil) and ranging the map to build `Keys` (which leaks randomized
order). Run `go test -race` to confirm the `RWMutex` actually serializes the 100
concurrent `Set`s.

## Resources

- [Go Specification: Map types](https://go.dev/ref/spec#Map_types) — nil semantics, comma-ok, delete, len, comparability.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — the reader/writer lock guarding the map.
- [slices.Sort](https://pkg.go.dev/slices#Sort) — sorting the collected keys for deterministic output.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-fixed-window-rate-limiter.md](02-fixed-window-rate-limiter.md)
