# Exercise 22: Cache Hit Detection With Entry Age

**Nivel: Intermedio** — validacion rapida (un test corto).

A cache lookup that only returns `(value, ok)` cannot tell a caller
*how stale* a hit is — and TTL-aware eviction needs exactly that number to
decide whether a hit still counts as fresh. This exercise builds a generic
`Cache[T].Get(key, ttl) (value T, age time.Duration, hit bool)` that
reports an entry's age on every lookup and lazily evicts it the moment
that age exceeds the caller-supplied TTL, all under an injected clock so
expiry is deterministic in tests.

This module is fully self-contained: its own `go mod init`, all code
inline, one quick test file.

## What you'll build

```text
agedcache/                   independent module: example.com/cache-lookup-with-age
  go.mod                     go 1.24
  agedcache.go                package agedcache; generic Cache[T]; Set(key,value); Get(key,ttl) (value,age,hit)
  cmd/
    demo/
      main.go                  fresh hit, aged hit, expiry-triggered eviction, missing key
  agedcache_test.go            hit/age/eviction table plus a short concurrent Set/Get race check
```

- Files: `agedcache.go`, `cmd/demo/main.go`, `agedcache_test.go`.
- Implement: `(*Cache[T]).Get(key string, ttl time.Duration) (value T, age time.Duration, hit bool)`, computing age from an injected clock and evicting the entry (not just reporting a miss) the moment `age > ttl`.
- Test: a fresh entry is a hit with the expected age; the same entry past its TTL is a miss whose age is still reported; the stale entry is genuinely gone afterward, not just treated as stale on that one call.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/22-cache-lookup-with-age/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/22-cache-lookup-with-age
go mod edit -go=1.24
```

### Age is not the same question as hit

`(value, hit)` answers "is this key here". `(value, age, hit)` answers a
strictly more useful question for anything TTL-based: "is this key here,
and for how long has it been sitting". A cache with a single fixed TTL
could get away with just `hit`, checked internally — but a cache shared
across callers who each apply their *own* freshness threshold (one caller
tolerates a 5-minute-old price quote, another needs it under 10 seconds)
needs the raw age exposed so each caller's `ttl` argument decides the
outcome independently, off the same stored entry.

`Get` also has to evict, not just report, an expired entry — and the
check (is it stale) and the act (delete it) must happen inside the same
locked section, or a concurrent `Get` from another goroutine could observe
the entry mid-eviction:

```go
age = c.now().Sub(e.storedAt)
if age > ttl {
	delete(c.entries, key)
	var zero T
	return zero, age, false
}
```

Because `Get` and `Set` share one `sync.Mutex` held for their entire
bodies, that delete and every other read or write of `c.entries` are
serialized — the exact discipline `go vet`'s race detector and this
lesson's concurrency exercises have been reinforcing throughout.

Create `agedcache.go`:

```go
package agedcache

import (
	"sync"
	"time"
)

type entry[T any] struct {
	value    T
	storedAt time.Time
}

// Cache is a generic, TTL-aware cache with lazy expiry: entries are only
// checked (and evicted, if stale) on Get, there is no background sweeper.
// It is safe for concurrent use.
type Cache[T any] struct {
	mu      sync.Mutex
	entries map[string]entry[T]
	now     func() time.Time
}

// NewCache builds a Cache using now as the injectable clock (pass
// time.Now in production, a fixed func in tests).
func NewCache[T any](now func() time.Time) *Cache[T] {
	return &Cache[T]{entries: make(map[string]entry[T]), now: now}
}

// Set stores value under key, timestamped with the cache's clock.
func (c *Cache[T]) Set(key string, value T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = entry[T]{value: value, storedAt: c.now()}
}

// Get looks up key and reports its age alongside whether it counts as a
// hit under ttl. An entry older than ttl is a miss and is evicted as part
// of the same locked check — the check (is it stale) and the act (delete
// it) happen atomically, so a concurrent Get never observes a half-evicted
// entry. A wholly absent key is also a miss, with age reported as zero.
func (c *Cache[T]) Get(key string, ttl time.Duration) (value T, age time.Duration, hit bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[key]
	if !ok {
		var zero T
		return zero, 0, false
	}

	age = c.now().Sub(e.storedAt)
	if age > ttl {
		delete(c.entries, key)
		var zero T
		return zero, age, false
	}
	return e.value, age, true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/cache-lookup-with-age"
)

func main() {
	clockNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return clockNow }

	cache := agedcache.NewCache[string](clock)
	cache.Set("greeting", "hello")

	ttl := 5 * time.Second

	value, age, hit := cache.Get("greeting", ttl)
	fmt.Printf("immediately: value=%q age=%s hit=%t\n", value, age, hit)

	clockNow = clockNow.Add(3 * time.Second)
	value, age, hit = cache.Get("greeting", ttl)
	fmt.Printf("after 3s:    value=%q age=%s hit=%t\n", value, age, hit)

	clockNow = clockNow.Add(3 * time.Second) // total 6s elapsed, past the 5s ttl
	value, age, hit = cache.Get("greeting", ttl)
	fmt.Printf("after 6s:    value=%q age=%s hit=%t (evicted, stale)\n", value, age, hit)

	_, _, hit = cache.Get("missing-key", ttl)
	fmt.Printf("missing key: hit=%t\n", hit)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
immediately: value="hello" age=0s hit=true
after 3s:    value="hello" age=3s hit=true
after 6s:    value="" age=6s hit=false (evicted, stale)
missing key: hit=false
```

### Tests

Create `agedcache_test.go`:

```go
package agedcache

import (
	"sync"
	"testing"
	"time"
)

func TestGetHitAgeAndEviction(t *testing.T) {
	t.Parallel()

	clockNow := time.Unix(1000, 0)
	clock := func() time.Time { return clockNow }
	cache := NewCache[string](clock)
	cache.Set("key", "value")

	ttl := 5 * time.Second

	value, age, hit := cache.Get("key", ttl)
	if !hit || value != "value" || age != 0 {
		t.Fatalf("immediate Get = (%q, %s, %t), want (value, 0s, true)", value, age, hit)
	}

	clockNow = clockNow.Add(3 * time.Second)
	value, age, hit = cache.Get("key", ttl)
	if !hit || value != "value" || age != 3*time.Second {
		t.Fatalf("Get at 3s = (%q, %s, %t), want (value, 3s, true)", value, age, hit)
	}

	clockNow = clockNow.Add(3 * time.Second) // 6s total, past the 5s ttl
	value, age, hit = cache.Get("key", ttl)
	if hit {
		t.Fatalf("Get at 6s: hit = true, want false (stale)")
	}
	if age != 6*time.Second {
		t.Fatalf("Get at 6s: age = %s, want 6s even on a miss", age)
	}

	// The stale entry must have been evicted, not merely reported as a miss.
	_, _, hitAfterEviction := cache.Get("key", ttl)
	if hitAfterEviction {
		t.Fatal("stale entry was not evicted")
	}

	_, _, hit = cache.Get("never-set", ttl)
	if hit {
		t.Fatal("hit = true for a key that was never Set")
	}
}

func TestConcurrentSetAndGetIsRaceFree(t *testing.T) {
	t.Parallel()
	cache := NewCache[int](func() time.Time { return time.Unix(0, 0) })

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cache.Set("shared", i)
			cache.Get("shared", time.Second)
		}(i)
	}
	wg.Wait()

	if _, _, hit := cache.Get("shared", time.Second); !hit {
		t.Fatal("expected a hit after concurrent writers finished")
	}
}
```

## Review

`Get` is correct when age is reported honestly on both hits and misses
(including the miss it just caused by evicting a stale entry), and when a
stale entry, once evicted, stays gone rather than reappearing on the next
lookup with a fresh timestamp. `TestGetHitAgeAndEviction`'s final assertion
— calling `Get` a second time after the entry expired — is the one that
would catch a `Get` that reports `hit == false` for a stale entry without
actually deleting it from the map, a bug invisible from a single call.

The mistake to avoid is computing age from a stored *duration until
expiry* rather than a stored *timestamp*: if `Set` stored `ttl` itself
(baking a specific caller's freshness threshold into the entry), a second
caller passing a different `ttl` to `Get` could never see a different
answer, defeating the entire point of separating "how old is this" from
"is that too old for you".

## Resources

- [Go spec: type parameters](https://go.dev/ref/spec#Type_parameters) — the generic `Cache[T any]` declaration.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the check-then-act eviction inside `Get`.
- [time.Time.Sub](https://pkg.go.dev/time#Time.Sub) — computing `age` as a `time.Duration` from two `time.Time` values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-protobuf-field-extract-typed.md](21-protobuf-field-extract-typed.md) | Next: [23-webhook-validator-collect-errors.md](23-webhook-validator-collect-errors.md)
