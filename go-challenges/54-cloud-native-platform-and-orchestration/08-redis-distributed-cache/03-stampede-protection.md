# Exercise 3: Stampede Protection with singleflight and a SetNX Lock

This is the code that gets written after the first production incident where a
single expired key took down the database. When a hot key expires under
concurrency, every request misses at once and recomputes it simultaneously —
amplifying exactly the load the cache existed to remove. The fix has two layers,
because concurrency has two scopes: `singleflight` collapses duplicate loads within
one process, and a Redis `SetNX` lock coordinates across replicas so cluster-wide
only one loader recomputes.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests against an in-process `miniredis`.

## What you'll build

```text
stampede/                     independent module: example.com/stampede
  go.mod                      go 1.26; requires go-redis, miniredis, x/sync
  cache.go                    Cache{GetOrLoad}; singleflight + SetNX lock + EVAL release
  cmd/
    demo/
      main.go                 100 concurrent callers, one origin hit
  cache_test.go               collapse, lock exclusivity + TTL, compare-and-delete, no-poison
```

- Files: `cache.go`, `cmd/demo/main.go`, `cache_test.go`.
- Implement: `GetOrLoad` with a fast-path cache read; on a miss, a `singleflight.Group.Do` that collapses in-process callers; inside it, a `SetNX` lock with a unique owner token and TTL; the winner loads the origin, caches with a jittered TTL, and releases via an `EVAL` compare-and-delete; losers back off and re-read.
- Test: N concurrent callers produce exactly one origin call and all get the same value with `shared==true`; `SetNX` grants the lock to exactly one owner and the lock carries a TTL; the compare-and-delete refuses a wrong token; an origin error neither populates the cache nor leaves the lock held.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/08-redis-distributed-cache/03-stampede-protection/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/08-redis-distributed-cache/03-stampede-protection
go get github.com/redis/go-redis/v9@latest
go get github.com/alicebob/miniredis/v2@latest
go get golang.org/x/sync/singleflight@latest
```

### Two layers, because there are two scopes of concurrency

Start with the fast path: `GetOrLoad` does a plain `Get`. A hit returns
immediately with no coordination — the common case must stay cheap. A `redis.Nil`
miss drops into the coordinated load; any other error fails open to the origin.

The first coordination layer is `singleflight`. When a hot key expires, dozens of
goroutines in one process miss at the same instant. `sf.Do(key, fn)` runs `fn`
once for a given key while it is in flight and hands the single result to every
caller that joined; the returned `shared` bool reports that the result was fanned
out to more than one caller. That collapses N in-process misses into one origin
call. But `singleflight` is process-local: ten pods each collapse internally to
one call, so the origin still sees ten. That is the exact reason the second layer
exists.

The second layer is a Redis lock, acquired inside the singleflight function so only
the one in-process winner contends for it. `SetNX(lockKey, token, lockTTL)` is
`SET lockKey token EX ttl NX` in one command: it succeeds for exactly one caller
cluster-wide. Two correctness properties are non-negotiable. The lock **must** have
a TTL, so a holder that crashes cannot deadlock the key forever — that is the
`lockTTL`. And it **must** be released with a compare-and-delete keyed on a unique
owner token, never a plain `DEL`: if your load outran the lock TTL, the lock may
already belong to a different owner, and a blind delete would free their lock. The
compare-and-delete has to be atomic, so it is a Lua script run with `EVAL`:

```
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end
```

The lock winner loads the origin, and on success caches the value (with a jittered
TTL) and releases the lock. On an origin error it does **not** cache anything —
poisoning the cache with a failed load would serve the error to every later reader
— and it releases the lock (the `defer`), so the next request retries cleanly. A
lock *loser* means another replica is already loading; it backs off for a bounded
number of short retries, re-reading the cache each time, and returns the value the
winner wrote. If the winner is slow or died and the retries are exhausted, the
loser falls back to a direct origin load rather than blocking the caller forever.

Note the token uses `crypto/rand` (an unguessable owner id) while the TTL jitter
uses `math/rand/v2`; they are aliased `crand` and `mrand` so both can be imported.

Create `cache.go`:

```go
package stampede

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	mrand "math/rand/v2"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Origin loads the serialized value for a key from the authoritative store.
type Origin func(ctx context.Context, key string) ([]byte, error)

// Config controls TTLs and the lock-loser back-off.
type Config struct {
	TTL        time.Duration // base lifetime of a cached value
	Jitter     time.Duration // max random amount added to TTL
	LockTTL    time.Duration // lifetime of the recompute lock (bounds a crash)
	MaxRetries int           // how many times a lock loser re-reads before falling back
	RetryWait  time.Duration // sleep between loser re-reads
}

// Cache is a stampede-safe read-through cache: singleflight collapses in-process
// duplicates and a SetNX lock coordinates recomputes across replicas.
type Cache struct {
	rdb    *redis.Client
	origin Origin
	cfg    Config
	sf     singleflight.Group
}

func New(rdb *redis.Client, origin Origin, cfg Config) *Cache {
	return &Cache{rdb: rdb, origin: origin, cfg: cfg}
}

// releaseScript deletes the lock only if it still holds our token, so we never
// delete a lock a different owner acquired after ours expired.
const releaseScript = `if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
else
	return 0
end`

func lockKey(key string) string { return key + ":lock" }

func newToken() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Cache) positiveTTL() time.Duration {
	if c.cfg.Jitter <= 0 {
		return c.cfg.TTL
	}
	return c.cfg.TTL + mrand.N(c.cfg.Jitter)
}

func (c *Cache) release(ctx context.Context, key, token string) {
	_ = c.rdb.Eval(ctx, releaseScript, []string{lockKey(key)}, token).Err()
}

// GetOrLoad returns the cached bytes for key. On a miss it collapses concurrent
// in-process callers with singleflight and coordinates across replicas with a
// SetNX lock, so cluster-wide only one loader recomputes a hot key. The bool
// reports whether singleflight fanned the result out to more than one caller.
func (c *Cache) GetOrLoad(ctx context.Context, key string) ([]byte, bool, error) {
	// Fast path: a plain cache read, no coordination.
	b, err := c.rdb.Get(ctx, key).Bytes()
	switch {
	case err == nil:
		return b, false, nil
	case errors.Is(err, redis.Nil):
		// miss: fall through to the coordinated load
	default:
		// Redis unreachable: fail open to the origin, do not cache.
		v, oerr := c.origin(ctx, key)
		return v, false, oerr
	}

	v, err, shared := c.sf.Do(key, func() (any, error) {
		return c.coordinatedLoad(ctx, key)
	})
	if err != nil {
		return nil, shared, err
	}
	return v.([]byte), shared, nil
}

func (c *Cache) coordinatedLoad(ctx context.Context, key string) ([]byte, error) {
	// Another in-process batch may have populated the key already.
	if b, err := c.rdb.Get(ctx, key).Bytes(); err == nil {
		return b, nil
	}

	token := newToken()
	got, err := c.rdb.SetNX(ctx, lockKey(key), token, c.cfg.LockTTL).Result()
	if err != nil {
		// Redis lock op failed: degrade to a direct origin load, do not cache.
		return c.origin(ctx, key)
	}

	if got {
		// We are the single cluster-wide loader.
		defer c.release(ctx, key, token)
		v, oerr := c.origin(ctx, key)
		if oerr != nil {
			return nil, oerr // do not poison the cache; the defer frees the lock
		}
		_ = c.rdb.Set(ctx, key, v, c.positiveTTL()).Err()
		return v, nil
	}

	// We lost the lock: another replica is loading. Back off and re-read.
	for range c.cfg.MaxRetries {
		time.Sleep(c.cfg.RetryWait)
		if b, err := c.rdb.Get(ctx, key).Bytes(); err == nil {
			return b, nil
		}
	}
	// The winner is slow or died: fall back to a direct origin load (uncached)
	// rather than blocking the caller indefinitely.
	return c.origin(ctx, key)
}
```

### The runnable demo

The demo fires 100 goroutines at one cold key, all released from a barrier at once,
against an origin that sleeps 50 ms to widen the collapse window. Despite 100
concurrent misses, `singleflight` collapses them to a single origin call — which is
the whole point.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/stampede"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func main() {
	s, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer s.Close()

	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer rdb.Close()

	var calls atomic.Int64
	c := stampede.New(rdb, func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // slow origin: widen the collapse window
		return []byte("rendered-page"), nil
	}, stampede.Config{
		TTL:        time.Minute,
		Jitter:     10 * time.Second,
		LockTTL:    5 * time.Second,
		MaxRetries: 50,
		RetryWait:  5 * time.Millisecond,
	})

	const n = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c.GetOrLoad(context.Background(), "homepage")
		}()
	}
	close(start)
	wg.Wait()

	fmt.Printf("callers=%d origin_hits=%d\n", n, calls.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
callers=100 origin_hits=1
```

### Tests

`TestSingleflightCollapse` launches 20 goroutines at one cold key from a barrier;
the origin increments an `atomic.Int64` and sleeps, so all callers join the same
in-flight call and the origin runs exactly once, every caller getting the same
value with `shared==true`. `TestSetNXLock...` drives the lock primitives directly:
`SetNX` grants to one owner, the lock carries a TTL (`s.TTL`), the compare-and-delete
refuses a wrong token, and accepts the right one. `TestLoserReadsPopulatedValue`
seeds a lock held by another owner with the key still unset, so `GetOrLoad` is forced
down the lock-loser branch of `coordinatedLoad`; a goroutine publishes the value mid
back-off, proving the loser re-reads and returns the winner's value without touching
the origin. `TestLoserFallsBackToOriginWhenRetriesExhausted` holds the lock but never
publishes, so the retries drain and the loser falls back to a single uncached origin
load, leaving the winner's lock intact. `TestOriginError...` proves a failed load
neither caches nor leaves the lock held.

Create `cache_test.go`:

```go
package stampede

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newCache(t *testing.T, origin Origin) (*Cache, *miniredis.Miniredis) {
	t.Helper()
	s := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { rdb.Close() })
	c := New(rdb, origin, Config{
		TTL:        time.Minute,
		Jitter:     10 * time.Second,
		LockTTL:    5 * time.Second,
		MaxRetries: 50,
		RetryWait:  5 * time.Millisecond,
	})
	return c, s
}

func TestSingleflightCollapse(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	origin := func(ctx context.Context, key string) ([]byte, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond) // widen the collapse window
		return []byte("payload"), nil
	}
	c, _ := newCache(t, origin)

	const n = 20
	type result struct {
		val    string
		shared bool
		err    error
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b, shared, err := c.GetOrLoad(context.Background(), "hot")
			results[i] = result{val: string(b), shared: shared, err: err}
		}()
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("origin calls = %d, want 1 (singleflight must collapse)", got)
	}
	shared := 0
	for i := range n {
		if results[i].err != nil {
			t.Fatalf("caller %d error: %v", i, results[i].err)
		}
		if results[i].val != "payload" {
			t.Fatalf("caller %d value = %q, want payload", i, results[i].val)
		}
		if results[i].shared {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("expected shared==true for coalesced callers")
	}
}

func TestSetNXLockExclusiveWithTTL(t *testing.T) {
	t.Parallel()
	c, s := newCache(t, func(context.Context, string) ([]byte, error) {
		return []byte("x"), nil
	})
	ctx := context.Background()
	token := newToken()

	ok, err := c.rdb.SetNX(ctx, lockKey("k"), token, c.cfg.LockTTL).Result()
	if err != nil || !ok {
		t.Fatalf("first SetNX = %v,%v; want true,nil", ok, err)
	}
	ok2, _ := c.rdb.SetNX(ctx, lockKey("k"), newToken(), c.cfg.LockTTL).Result()
	if ok2 {
		t.Fatal("second SetNX granted the lock; want it denied")
	}
	if ttl := s.TTL(lockKey("k")); ttl <= 0 {
		t.Fatalf("lock TTL = %s, want > 0 so a crashed holder cannot deadlock", ttl)
	}

	c.release(ctx, "k", "not-the-token")
	if !s.Exists(lockKey("k")) {
		t.Fatal("release with a wrong token deleted the lock")
	}
	c.release(ctx, "k", token)
	if s.Exists(lockKey("k")) {
		t.Fatal("release with the correct token did not delete the lock")
	}
}

func TestLoserReadsPopulatedValue(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	origin := func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("origin-fallback"), nil
	}
	c, _ := newCache(t, origin)
	ctx := context.Background()

	// Simulate a winner on another replica that holds the lock but has NOT written
	// the value yet, so key "k" is still a miss on entry. GetOrLoad must take the
	// lock-loser branch: the fast path misses, coordinatedLoad's re-read misses,
	// SetNX is denied, and the caller backs off and re-reads.
	if ok, _ := c.rdb.SetNX(ctx, lockKey("k"), newToken(), c.cfg.LockTTL).Result(); !ok {
		t.Fatal("could not simulate the winner's lock")
	}

	// The winner publishes the value mid back-off, so a later re-read inside the
	// loser's retry loop observes it.
	go func() {
		time.Sleep(15 * time.Millisecond)
		_ = c.rdb.Set(ctx, "k", []byte("fresh"), time.Minute).Err()
	}()

	b, _, err := c.GetOrLoad(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "fresh" {
		t.Fatalf("loser value = %q, want fresh (read after the winner published)", b)
	}
	if calls.Load() != 0 {
		t.Fatalf("origin calls = %d, want 0 (loser served the winner's value)", calls.Load())
	}
}

func TestLoserFallsBackToOriginWhenRetriesExhausted(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	origin := func(context.Context, string) ([]byte, error) {
		calls.Add(1)
		return []byte("direct"), nil
	}
	s := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	t.Cleanup(func() { rdb.Close() })
	c := New(rdb, origin, Config{
		TTL:        time.Minute,
		LockTTL:    5 * time.Second,
		MaxRetries: 3,
		RetryWait:  time.Millisecond,
	})
	ctx := context.Background()

	// A winner on another replica holds the lock but never publishes the value, so
	// every loser re-read misses and the bounded retries drain.
	if ok, _ := c.rdb.SetNX(ctx, lockKey("k"), newToken(), c.cfg.LockTTL).Result(); !ok {
		t.Fatal("could not simulate the winner's lock")
	}

	b, _, err := c.GetOrLoad(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "direct" {
		t.Fatalf("fallback value = %q, want direct", b)
	}
	if calls.Load() != 1 {
		t.Fatalf("origin calls = %d, want 1 (exhausted loser falls back to origin)", calls.Load())
	}
	// The exhausted-loser fallback is uncached and must not disturb the winner.
	if s.Exists("k") {
		t.Fatal("exhausted-loser fallback must not populate the cache")
	}
	if !s.Exists(lockKey("k")) {
		t.Fatal("loser must not release the winner's lock")
	}
}

func TestOriginErrorNotCachedAndLockReleased(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("origin down")
	origin := func(context.Context, string) ([]byte, error) {
		return nil, wantErr
	}
	c, s := newCache(t, origin)
	ctx := context.Background()

	_, _, err := c.GetOrLoad(ctx, "k")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if s.Exists("k") {
		t.Fatal("origin error must not populate the cache")
	}
	if s.Exists(lockKey("k")) {
		t.Fatal("lock must be released after an origin error")
	}
}

func Example() {
	s, _ := miniredis.Run()
	defer s.Close()
	rdb := redis.NewClient(&redis.Options{Addr: s.Addr()})
	defer rdb.Close()

	calls := 0
	c := New(rdb, func(context.Context, string) ([]byte, error) {
		calls++
		return []byte("v"), nil
	}, Config{TTL: time.Minute, LockTTL: time.Second, MaxRetries: 10, RetryWait: time.Millisecond})

	ctx := context.Background()
	c.GetOrLoad(ctx, "k")
	b, _, _ := c.GetOrLoad(ctx, "k")
	fmt.Printf("%s origin_calls=%d\n", b, calls)
	// Output: v origin_calls=1
}
```

## Review

The cache is stampede-safe when both layers hold. `TestSingleflightCollapse` proves
the in-process layer: 20 concurrent misses produce exactly one origin call and a
fanned-out result (`shared==true`). If that test sees more than one call, the
origin was too fast for the goroutines to coalesce, or the singleflight key is
wrong. The lock test proves the cross-replica layer's correctness: exactly one
`SetNX` winner, a TTL on the lock so a crash cannot deadlock, and a
compare-and-delete that refuses a foreign token — the property that keeps you from
deleting another owner's lock.

The mistakes to avoid: do not rely on `singleflight` alone and call the stampede
solved — it only coalesces within one process, which is why the `SetNX` lock is
layered on top. Do not acquire a lock without a TTL, and never release it with a
plain `DEL` — use the `EVAL` compare-and-delete. Do not cache a failed load;
`TestOriginErrorNotCachedAndLockReleased` fails if you populate the cache on error
or leave the lock held. This is a lightweight lock for coalescing recomputes, not a
general-purpose mutex — the next lesson (redsync/Redlock) raises the bar for locks
you actually depend on for mutual exclusion. Run `go test -race` to confirm the
concurrent path is free of data races.

## Resources

- [`golang.org/x/sync/singleflight`](https://pkg.go.dev/golang.org/x/sync/singleflight) — `Group.Do`, the `shared` return, and `Group.Forget`.
- [`github.com/redis/go-redis/v9`](https://pkg.go.dev/github.com/redis/go-redis/v9) — `SetNX`, `BoolCmd.Result`, and `Eval` for the compare-and-delete release.
- [Distributed Locks with Redis](https://redis.io/docs/latest/develop/use-cases/patterns/distributed-locks/) — why a lock needs a TTL and a token, and the limits of a single-instance lock.
- [`github.com/alicebob/miniredis/v2`](https://pkg.go.dev/github.com/alicebob/miniredis/v2) — `TTL`, `Exists`, and `Set` for asserting lock and cache state offline.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-write-through-and-invalidation.md](02-write-through-and-invalidation.md) | Next: [../09-redis-distributed-locks-redsync/00-concepts.md](../09-redis-distributed-locks-redsync/00-concepts.md)
