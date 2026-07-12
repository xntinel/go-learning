# Exercise 1: Cache-Aside Reads with TTL and Jittered Expiry

This builds the resilient default read path: a generic `GetOrLoad[T]` that checks
Redis, loads from an injected origin on a miss, writes the value back with a
jittered TTL, caches not-found results negatively, and — the part that matters
under an incident — falls through to the origin instead of erroring when Redis is
down.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests that run against an in-process Redis (`miniredis`) so nothing touches a
network.

## What you'll build

```text
cacheaside/                   independent module: example.com/cacheaside
  go.mod                      go 1.26; requires go-redis and miniredis
  cache.go                    Cache, Config, Loader[T]; GetOrLoad[T] (hit/miss/fail-open)
  cmd/
    demo/
      main.go                 cold/warm/expiry/negative lookups against miniredis
  cache_test.go               miss-then-hit, expiry reload, negative cache, fail-open, Example
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: `GetOrLoad[T](ctx, *Cache, key, Loader[T]) (T, bool, error)` with `redis.Nil` miss detection, an `envelope` that distinguishes a cached value from a cached not-found, jittered positive TTL, negative caching, and a fail-open path on any non-`redis.Nil` error.
- Test: first call misses and loads once; second call hits and does not; `FastForward` past the TTL forces a reload; a not-found is cached so the repeat does not re-hit the origin; closing Redis still returns the origin value.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get github.com/redis/go-redis/v9@latest
go get github.com/alicebob/miniredis/v2@latest
```

### The three branches of a read, and why the envelope exists

The whole correctness of cache-aside lives in one `switch`. A `Get` against Redis
has exactly three outcomes and each demands different behavior:

- `err == nil`: a hit. Decode the stored bytes and return.
- `errors.Is(err, redis.Nil)`: a miss. The key is genuinely absent; load from the
  origin and populate the cache.
- any other error: Redis is unreachable or slow. Do **not** treat this as a miss
  that gets cached, and do **not** surface it to the caller. Fall through to the
  origin and return its result *without* caching — the fail-open path that keeps a
  Redis outage a latency event instead of an outage.

The second subtlety is negative caching. A `Loader` reports three things:
`(value, true, nil)` when the origin has the value, `(zero, false, nil)` when the
origin definitively has no such key, and `(_, _, err)` for a real failure. If we
cached a not-found by simply storing a zero value, a later read could not tell "we
cached that this key is absent" from "we cached a real zero value." So the wire
format is a small `envelope{Found bool, Data json.RawMessage}`: a negative entry
is `{Found:false}`, a positive entry carries the JSON of the value in `Data`. That
one bool is what lets a cached not-found be absorbed correctly on the next read.

Third, the TTL. A positive entry expires after `base + rand.N(jitter)` using
`math/rand/v2` — the jitter decorrelates a batch of keys written together so they
do not all expire in the same instant and stampede the origin. A negative entry
gets a shorter, fixed TTL so a key that later gets created does not stay invisible
for long. Note `rand.N` takes and returns the argument's type, and `time.Duration`
is an integer type, so `rand.N(c.cfg.Jitter)` returns a `time.Duration` in
`[0, Jitter)` directly.

Finally, every cache *write* is best-effort: if the `Set` fails, the caller still
has the value it just loaded, so a write error is swallowed rather than propagated.
The cache is not authoritative; a failed populate is a missed optimization, not a
failed request.

Create `cache.go`:

```go
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
)

// Loader fetches the authoritative value for a key. It returns (value, true, nil)
// when the origin has the value, (zero, false, nil) when the origin definitively
// has no such key (a not-found, cached negatively), and a non-nil error only for
// a genuine failure (which is never cached).
type Loader[T any] func(ctx context.Context) (T, bool, error)

// Config controls the cache-aside behavior.
type Config struct {
	// Namespace is prepended to every key. Bump it (v1 -> v2) when the serialized
	// shape changes, so a format change becomes a clean miss, not a decode error.
	Namespace string
	// TTL is the base lifetime of a positive entry.
	TTL time.Duration
	// Jitter is the maximum random amount added to TTL to decorrelate expiry.
	Jitter time.Duration
	// NegativeTTL is the (short) lifetime of a cached not-found.
	NegativeTTL time.Duration
}

// Cache is a cache-aside wrapper around a go-redis client.
type Cache struct {
	rdb *redis.Client
	cfg Config
}

// New builds a cache over an existing, pooled go-redis client.
func New(rdb *redis.Client, cfg Config) *Cache {
	return &Cache{rdb: rdb, cfg: cfg}
}

// envelope is what actually crosses the wire. Found distinguishes a cached value
// from a cached not-found, so a negative entry never decodes as a real zero value.
type envelope struct {
	Found bool            `json:"found"`
	Data  json.RawMessage `json:"data,omitempty"`
}

func (c *Cache) key(k string) string {
	return c.cfg.Namespace + ":" + k
}

// positiveTTL returns the base TTL plus a random jitter in [0, Jitter).
func (c *Cache) positiveTTL() time.Duration {
	if c.cfg.Jitter <= 0 {
		return c.cfg.TTL
	}
	return c.cfg.TTL + rand.N(c.cfg.Jitter)
}

// GetOrLoad returns the cached value for key, loading and caching it on a miss.
// It degrades gracefully: a cache miss, a not-found, and a Redis outage all fall
// through to the origin instead of surfacing as an error to the caller.
func GetOrLoad[T any](ctx context.Context, c *Cache, key string, load Loader[T]) (T, bool, error) {
	var zero T
	full := c.key(key)

	raw, err := c.rdb.Get(ctx, full).Bytes()
	switch {
	case err == nil:
		// Hit: positive or negative.
		var env envelope
		if json.Unmarshal(raw, &env) != nil {
			// Corrupt or old-shape bytes: treat as a miss and reload.
			return loadAndCache(ctx, c, full, load)
		}
		if !env.Found {
			return zero, false, nil
		}
		var v T
		if json.Unmarshal(env.Data, &v) != nil {
			return loadAndCache(ctx, c, full, load)
		}
		return v, true, nil
	case errors.Is(err, redis.Nil):
		// Genuine miss: load from origin and populate.
		return loadAndCache(ctx, c, full, load)
	default:
		// Redis is unreachable: fail open to the origin, do not cache.
		return load(ctx)
	}
}

func loadAndCache[T any](ctx context.Context, c *Cache, full string, load Loader[T]) (T, bool, error) {
	var zero T
	v, found, err := load(ctx)
	if err != nil {
		return zero, false, err // never cache an error
	}
	if !found {
		c.store(ctx, full, envelope{Found: false}, c.cfg.NegativeTTL)
		return zero, false, nil
	}
	if data, merr := json.Marshal(v); merr == nil {
		c.store(ctx, full, envelope{Found: true, Data: data}, c.positiveTTL())
	}
	return v, true, nil
}

// store best-effort writes an envelope; a write failure is swallowed because the
// caller already has the value and the cache is not authoritative.
func (c *Cache) store(ctx context.Context, full string, env envelope, ttl time.Duration) {
	b, err := json.Marshal(env)
	if err != nil {
		return
	}
	_ = c.rdb.Set(ctx, full, b, ttl).Err()
}
```

### The runnable demo

The demo runs against an in-process `miniredis` so it needs no server, and it uses
`FastForward` to jump past the TTL deterministically. It threads a call counter
through the origin so you can watch the cache do its job: the cold lookup loads,
the warm lookup does not, the post-expiry lookup loads again, and a missing key is
loaded once then absorbed by the negative cache.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/cacheaside"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type user struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func main() {
	s, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer s.Close()

	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer rdb.Close()

	c := cache.New(rdb, cache.Config{
		Namespace:   "app:v1:user",
		TTL:         30 * time.Second,
		Jitter:      10 * time.Second,
		NegativeTTL: 5 * time.Second,
	})

	db := map[string]user{"1": {ID: "1", Name: "alice"}}
	calls := 0
	load := func(id string) cache.Loader[user] {
		return func(ctx context.Context) (user, bool, error) {
			calls++
			u, ok := db[id]
			return u, ok, nil
		}
	}

	ctx := context.Background()

	u, _, _ := cache.GetOrLoad(ctx, c, "1", load("1"))
	fmt.Printf("cold  lookup:  name=%s origin_calls=%d\n", u.Name, calls)

	u, _, _ = cache.GetOrLoad(ctx, c, "1", load("1"))
	fmt.Printf("warm  lookup:  name=%s origin_calls=%d\n", u.Name, calls)

	s.FastForward(41 * time.Second) // past TTL(30) + max Jitter(<10)
	u, _, _ = cache.GetOrLoad(ctx, c, "1", load("1"))
	fmt.Printf("after expiry:  name=%s origin_calls=%d\n", u.Name, calls)

	_, found, _ := cache.GetOrLoad(ctx, c, "404", load("404"))
	fmt.Printf("missing key:   found=%v origin_calls=%d\n", found, calls)

	_, found, _ = cache.GetOrLoad(ctx, c, "404", load("404"))
	fmt.Printf("missing again: found=%v origin_calls=%d\n", found, calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cold  lookup:  name=alice origin_calls=1
warm  lookup:  name=alice origin_calls=1
after expiry:  name=alice origin_calls=2
missing key:   found=false origin_calls=3
missing again: found=false origin_calls=3
```

### Tests

The tests use `miniredis.RunT(t)`, which starts an in-process Redis and registers
its own `t.Cleanup`, so each test is deterministic and offline. `FastForward`
advances miniredis's clock to force expiry without sleeping. `TestGetOrLoadFailsOpen`
closes the server before the read to prove the wrapper returns the origin value
rather than an error when Redis is down.

Create `cache_test.go`:

```go
package cache

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type product struct {
	SKU   string `json:"sku"`
	Price int    `json:"price"`
}

func newTestCache(t *testing.T) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { rdb.Close() })
	c := New(rdb, Config{
		Namespace:   "t:v1",
		TTL:         time.Minute,
		Jitter:      10 * time.Second,
		NegativeTTL: 30 * time.Second,
	})
	return c, s
}

func TestGetOrLoadMissThenHit(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	var calls int
	load := func(context.Context) (product, bool, error) {
		calls++
		return product{SKU: "abc", Price: 100}, true, nil
	}
	ctx := context.Background()

	p, found, err := GetOrLoad(ctx, c, "abc", load)
	if err != nil || !found || p.Price != 100 {
		t.Fatalf("first: p=%+v found=%v err=%v", p, found, err)
	}
	if calls != 1 {
		t.Fatalf("origin calls after miss = %d, want 1", calls)
	}

	p, found, err = GetOrLoad(ctx, c, "abc", load)
	if err != nil || !found || p.Price != 100 {
		t.Fatalf("second: p=%+v found=%v err=%v", p, found, err)
	}
	if calls != 1 {
		t.Fatalf("origin calls after hit = %d, want 1 (served from cache)", calls)
	}
}

func TestGetOrLoadExpiryReloads(t *testing.T) {
	t.Parallel()
	c, s := newTestCache(t)
	var calls int
	load := func(context.Context) (product, bool, error) {
		calls++
		return product{SKU: "abc", Price: calls * 10}, true, nil
	}
	ctx := context.Background()

	GetOrLoad(ctx, c, "abc", load)
	GetOrLoad(ctx, c, "abc", load) // hit
	if calls != 1 {
		t.Fatalf("calls before expiry = %d, want 1", calls)
	}

	s.FastForward(71 * time.Second) // past TTL(60) + max Jitter(<10)
	p, _, _ := GetOrLoad(ctx, c, "abc", load)
	if calls != 2 {
		t.Fatalf("calls after expiry = %d, want 2", calls)
	}
	if p.Price != 20 {
		t.Fatalf("reloaded price = %d, want 20", p.Price)
	}
}

func TestGetOrLoadNegativeCaching(t *testing.T) {
	t.Parallel()
	c, _ := newTestCache(t)
	var calls int
	load := func(context.Context) (product, bool, error) {
		calls++
		return product{}, false, nil // origin says not found
	}
	ctx := context.Background()

	_, found, err := GetOrLoad(ctx, c, "missing", load)
	if err != nil || found {
		t.Fatalf("first missing: found=%v err=%v", found, err)
	}
	_, found, _ = GetOrLoad(ctx, c, "missing", load)
	if found {
		t.Fatal("second lookup should still report not found")
	}
	if calls != 1 {
		t.Fatalf("origin calls = %d, want 1 (negative cache absorbed the repeat)", calls)
	}
}

func TestGetOrLoadFailsOpen(t *testing.T) {
	t.Parallel()
	c, s := newTestCache(t)
	s.Close() // Redis is now unreachable
	var calls int
	load := func(context.Context) (product, bool, error) {
		calls++
		return product{SKU: "abc", Price: 42}, true, nil
	}

	p, found, err := GetOrLoad(context.Background(), c, "abc", load)
	if err != nil {
		t.Fatalf("fail-open must not surface an error, got %v", err)
	}
	if !found || p.Price != 42 {
		t.Fatalf("fail-open value = %+v found=%v, want price 42", p, found)
	}
	if calls != 1 {
		t.Fatalf("origin calls = %d, want 1 (loaded from origin)", calls)
	}
}

func Example() {
	s, _ := miniredis.Run()
	defer s.Close()
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer rdb.Close()
	c := New(rdb, Config{Namespace: "ex:v1", TTL: time.Minute, NegativeTTL: time.Minute})

	calls := 0
	load := func(context.Context) (string, bool, error) {
		calls++
		return "value", true, nil
	}
	ctx := context.Background()
	GetOrLoad(ctx, c, "k", load)
	v, _, _ := GetOrLoad(ctx, c, "k", load)
	fmt.Printf("%s calls=%d\n", v, calls)
	// Output: value calls=1
}
```

## Review

The read path is correct when it branches on exactly three cases and each does the
right thing: a hit decodes and returns, a `redis.Nil` miss loads and populates, and
any other error falls open to the origin uncached. `TestGetOrLoadFailsOpen` is the
one that proves the last property is real — if you accidentally treat a closed
connection as a miss, the test still passes for the value but you have masked an
outage; if you surface the error, the test fails. The negative-cache test proves
the `envelope.Found` bool is doing its job: a not-found is stored and the repeat
lookup is absorbed, so `calls` stays at 1.

The mistakes to avoid: do not conflate `redis.Nil` with a real error (branch with
`errors.Is`); do not cache the result of a failed load (return the error, let the
next request retry); do not give every key a fixed TTL (the jitter is what
prevents a synchronized expiry avalanche); and do not store a bare zero value for a
not-found — you could not distinguish it from a cached real zero later. Run `go
test -race` to confirm nothing shares mutable state across the concurrent callers.

## Resources

- [`github.com/redis/go-redis/v9`](https://pkg.go.dev/github.com/redis/go-redis/v9) — `NewClient`, `Options`, `Get`/`Set`, `StringCmd.Bytes`, and the `redis.Nil` sentinel.
- [Cache-aside with Go (Redis docs)](https://redis.io/docs/latest/develop/use-cases/cache-aside/go/) — the lazy-loading pattern and why the application owns the load.
- [`github.com/alicebob/miniredis/v2`](https://pkg.go.dev/github.com/alicebob/miniredis/v2) — `RunT`, `Addr`, `FastForward`, and `Close` for deterministic, offline cache tests.
- [`math/rand/v2`](https://pkg.go.dev/math/rand/v2) — `rand.N` for TTL jitter without a global seed.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-write-through-and-invalidation.md](02-write-through-and-invalidation.md)
