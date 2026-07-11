# Exercise 29: DNS Resolution Cache — Defer Invalidation on Staleness

**Nivel: Intermedio** — validacion rapida (un test corto).

A DNS resolution cache has two independent reasons an entry it just wrote
should not be trusted: the entry's own TTL eventually elapses, and the
request that populated it might itself get cancelled before it finishes,
in which case the cache should not keep a value seeded by a request nobody
actually wanted. This module checks the first lazily, on the next lookup,
and the second with a deferred closure that inspects the request's context
right before `Resolve` returns.

## What you'll build

```text
dnscache/                    independent module: example.com/dnscache
  go.mod
  dnscache/dnscache.go         Cache (injected clock); Resolve (TTL + deferred cancel check)
  cmd/demo/main.go              cache hit within TTL; expiry; cancelled request
  dnscache/dnscache_test.go     cached within TTL; refetch after expiry; cancelled context invalidates
```

- Files: `dnscache/dnscache.go`, `cmd/demo/main.go`, `dnscache/dnscache_test.go`.
- Implement: a `Cache` with an injected `now func() time.Time` clock and a mutex-guarded map; `Resolve(ctx context.Context, host string, ttl time.Duration, lookup func() (string, error)) (addr string, err error)`, which returns a cached value if it has not exceeded its TTL (checked against the injected clock), otherwise calls `lookup`, caches the result, and defers a closure that invalidates the entry it just wrote if `ctx` is already done by the time `Resolve` returns.
- Test: a second `Resolve` within the TTL is served from cache without calling `lookup` again; advancing the injected clock past the TTL causes a third `Resolve` to call `lookup` again; a `Resolve` whose context was cancelled before it was even called leaves the cache empty for that host.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dnscache/dnscache ~/go-exercises/dnscache/cmd/demo
cd ~/go-exercises/dnscache
go mod init example.com/dnscache
go mod edit -go=1.24
```

### Two different kinds of staleness, two different checks

TTL expiry is checked the cheap way: lazily, the next time anyone asks for
that host, by comparing the injected clock against the stored expiry time.
There is no timer running in the background counting down — the entry just
sits there, and whether it still counts as fresh is decided at the moment
it is next read. That is also why the clock is injected rather than calling
`time.Now()` directly: a test can jump straight from "just cached" to
"31 seconds later" without an actual `time.Sleep`, making the expiry
assertion instant and exact.

Context cancellation is a different problem and needs a different
mechanism: it is not about how *old* the entry is, but about whether the
request that created it was ever supposed to succeed in the first place.
`Resolve` defers a closure, right after writing the entry, that does a
non-blocking `select` on `ctx.Done()`. If the context was already cancelled
by the time that closure runs — which, because it is deferred, is right
before `Resolve` actually returns to its caller — the entry gets deleted
immediately, before any other goroutine has a chance to read a value seeded
by a request that the caller had already given up on. On the normal,
non-cancelled path that same `select` falls through to `default` and does
nothing, leaving the entry to expire on its own schedule.

Create `dnscache/dnscache.go`:

```go
package dnscache

import (
	"context"
	"sync"
	"time"
)

// Cache is a bounded-TTL DNS resolution cache. The clock is injected so
// tests can advance time deterministically instead of sleeping.
type Cache struct {
	mu      sync.Mutex
	entries map[string]entry
	now     func() time.Time
}

type entry struct {
	addr    string
	expires time.Time
}

// New returns an empty Cache. If now is nil, time.Now is used.
func New(now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{entries: make(map[string]entry), now: now}
}

// Len reports how many entries are currently cached.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Resolve returns the cached address for host if it exists and has not
// exceeded its bounded TTL. Otherwise it calls lookup, caches the result for
// ttl, and returns it.
//
// The cache entry it just wrote is guarded by a deferred check: if ctx is
// already cancelled by the time Resolve is about to return, the entry is
// invalidated immediately rather than left behind looking fresh -- a
// cancelled request should not seed the cache for everyone else. On the
// normal (non-cancelled) path the entry simply survives until its TTL
// elapses, checked lazily the next time Resolve is called for that host.
func (c *Cache) Resolve(ctx context.Context, host string, ttl time.Duration, lookup func() (string, error)) (addr string, err error) {
	c.mu.Lock()
	if e, ok := c.entries[host]; ok && c.now().Before(e.expires) {
		c.mu.Unlock()
		return e.addr, nil
	}
	c.mu.Unlock()

	addr, err = lookup()
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.entries[host] = entry{addr: addr, expires: c.now().Add(ttl)}
	c.mu.Unlock()

	defer func() {
		select {
		case <-ctx.Done():
			c.invalidate(host)
		default:
		}
	}()

	return addr, nil
}

func (c *Cache) invalidate(host string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, host)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/dnscache/dnscache"
)

func main() {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	cache := dnscache.New(clock)

	lookups := 0
	lookup := func() (string, error) {
		lookups++
		return "93.184.216.34", nil
	}

	ctx := context.Background()

	addr, _ := cache.Resolve(ctx, "example.com", 30*time.Second, lookup)
	fmt.Println("first resolve:", addr, "lookups:", lookups)

	addr, _ = cache.Resolve(ctx, "example.com", 30*time.Second, lookup)
	fmt.Println("second resolve (cached):", addr, "lookups:", lookups)

	now = now.Add(31 * time.Second) // advance the injected clock past the TTL
	addr, _ = cache.Resolve(ctx, "example.com", 30*time.Second, lookup)
	fmt.Println("after TTL expiry:", addr, "lookups:", lookups)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = cache.Resolve(cancelledCtx, "canceled.example.com", 30*time.Second, lookup)
	fmt.Println("cache size after cancelled request:", cache.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first resolve: 93.184.216.34 lookups: 1
second resolve (cached): 93.184.216.34 lookups: 1
after TTL expiry: 93.184.216.34 lookups: 2
cache size after cancelled request: 1
```

### Tests

Create `dnscache/dnscache_test.go`:

```go
package dnscache

import (
	"context"
	"testing"
	"time"
)

func TestResolveCachesWithinTTL(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(func() time.Time { return now })

	lookups := 0
	lookup := func() (string, error) { lookups++; return "1.2.3.4", nil }

	ctx := context.Background()
	addr, err := c.Resolve(ctx, "a.test", 10*time.Second, lookup)
	if err != nil {
		t.Fatalf("Resolve() err = %v, want nil", err)
	}
	if addr != "1.2.3.4" {
		t.Fatalf("addr = %q, want %q", addr, "1.2.3.4")
	}

	addr, err = c.Resolve(ctx, "a.test", 10*time.Second, lookup)
	if err != nil {
		t.Fatalf("Resolve() err = %v, want nil", err)
	}
	if addr != "1.2.3.4" {
		t.Fatalf("addr = %q, want %q", addr, "1.2.3.4")
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want 1 (second call served from cache)", lookups)
	}
}

func TestResolveRefetchesAfterTTLExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(func() time.Time { return now })

	lookups := 0
	lookup := func() (string, error) { lookups++; return "1.2.3.4", nil }

	ctx := context.Background()
	if _, err := c.Resolve(ctx, "b.test", 5*time.Second, lookup); err != nil {
		t.Fatalf("Resolve() err = %v, want nil", err)
	}

	now = now.Add(6 * time.Second) // past the 5s TTL

	if _, err := c.Resolve(ctx, "b.test", 5*time.Second, lookup); err != nil {
		t.Fatalf("Resolve() err = %v, want nil", err)
	}

	if lookups != 2 {
		t.Fatalf("lookups = %d, want 2 (entry expired and was refetched)", lookups)
	}
}

func TestResolveInvalidatesEntryWhenContextAlreadyCancelled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(func() time.Time { return now })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Resolve is ever called

	addr, err := c.Resolve(ctx, "c.test", 30*time.Second, func() (string, error) {
		return "9.9.9.9", nil
	})
	if err != nil {
		t.Fatalf("Resolve() err = %v, want nil (lookup itself still succeeds)", err)
	}
	if addr != "9.9.9.9" {
		t.Fatalf("addr = %q, want %q", addr, "9.9.9.9")
	}

	if got := c.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0: entry seeded by a cancelled request must not survive", got)
	}
}
```

## Review

The cache is correct when a lookup within TTL never calls `lookup` a second
time, when advancing past the TTL causes exactly one refetch, and when a
request whose context is already cancelled by the time it returns leaves no
trace in the cache. The mistake this pattern exists to prevent is treating
"the request is cancelled" and "the entry is stale" as the same kind of
event: a real background timer or goroutine watching for TTL expiry would
be needless machinery for something a lazy comparison already handles for
free, while trying to fold cancellation into that same lazy check would
mean a cancelled request's bad value could sit in the cache, looking
perfectly fresh, until its TTL eventually (and separately) ran out.

## Resources

- [context package](https://pkg.go.dev/context)
- [time package](https://pkg.go.dev/time)
- [The Go Programming Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-rate-limiter-quota-release.md](28-rate-limiter-quota-release.md) | Next: [30-write-both-sinks-rollback-all.md](30-write-both-sinks-rollback-all.md)
