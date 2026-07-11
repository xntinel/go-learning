# Exercise 34: DNS Service Discovery with TTL-Aware Endpoint Reloading

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A client-side service discovery cache that treats every cached endpoint list
as either "valid" or "gone" is both slower and less resilient than it needs
to be: refusing to serve an entry the instant its TTL expires means every
request that lands in that instant blocks on a synchronous re-resolution,
and a resolver outage turns a momentary blip into every caller stalling at
once. Real DNS-backed discovery clients instead serve a stale entry
immediately while refreshing it in the background, and only fall back to a
blocking fetch once the entry is old enough that serving it at all would be
unsafe. This module manages a per-service endpoint cache with independent
TTLs, ranges every cached entry to find and revalidate the ones that have
gone stale, deduplicates concurrent revalidation attempts for the same
service, and falls back to a synchronous fetch once an entry crosses a hard
max-age. The module is fully self-contained: its own `go mod init`, no
external dependencies.

## What you'll build

```text
discovery/                  independent module: example.com/dns-service-discovery-ttl-cache
  go.mod                    go 1.24
  discovery.go              type Cache; Resolve, SweepStale
  cmd/
    demo/
      main.go               runnable demo: fresh resolve, stale sweep, expired synchronous refetch
  discovery_test.go          table-style tests: fresh/stale/expired paths + dedup under -race
```

- Files: `discovery.go`, `cmd/demo/main.go`, `discovery_test.go`.
- Implement: `Cache.Resolve` and `Cache.SweepStale`, both built on a shared
  `sync.Mutex` and a deduplicated background-revalidation helper.
- Test: a fresh-entry case, a stale-serves-then-revalidates case, an
  expired-synchronous-fetch case, a dedup-against-in-flight case, and a
  concurrent-`Resolve` dedup case under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dns-service-discovery-ttl-cache/cmd/demo
cd ~/go-exercises/dns-service-discovery-ttl-cache
go mod init example.com/dns-service-discovery-ttl-cache
go mod edit -go=1.24
```

### Ranging every cached entry to find the ones worth revalidating

`SweepStale` is the janitor half of this cache's staleness policy: it ranges
every entry currently in `c.entries` and, for each one that is stale but
not yet past `maxAge`, calls the same `triggerRevalidateLocked` helper that
`Resolve` calls inline when it notices a stale hit. Sharing that helper
matters — a service nobody has queried in a while would otherwise sit stale
forever, since `Resolve`'s inline check only fires when a caller actually
asks for that exact service, and a periodic sweep is what catches the case
where the *next* caller for a quiet service is the one who ends up paying
for a synchronous fetch instead of getting served immediately from a
slightly-stale cache. The range explicitly skips any entry that has crossed
`maxAge`: a sweep has no business trying to background-refresh data the
cache has already decided is too old to trust at all, and doing so would
race a caller's own synchronous refetch of that same expired entry with no
benefit.

`triggerRevalidateLocked`'s dedup check — `if c.revalidating[service] { return false }` —
runs inside the same lock the range in `SweepStale` and the inline check in
`Resolve` both hold, which is what prevents a stampede: if ten concurrent
`Resolve` calls for the same stale service all reach this check before any
of them finishes, only the first to acquire the lock sees `revalidating[service] == false`,
flips it to `true`, and starts the one background fetch; every other
caller, whether it got there through `Resolve` or a concurrent `SweepStale`,
sees the flag already set and does nothing further — they still return the
stale data they already have, they just do not pile on a second redundant
fetch. Marking `revalidating[service] = true` and launching the goroutine
happen atomically with the check, so there is no window between "decide to
revalidate" and "record that a revalidation is in flight" for a second
caller to slip through.

Create `discovery.go`:

```go
package discovery

import (
	"sort"
	"sync"
	"time"
)

// Endpoint is one resolved network address for a service.
type Endpoint struct {
	Addr string
}

// Resolver performs the actual (slow, network-bound) service lookup — a DNS
// SRV query, a service-mesh control-plane call — and returns the resolved
// endpoints along with how long they may be trusted before going stale.
type Resolver func(service string) (endpoints []Endpoint, ttl time.Duration, err error)

// entry is one cached resolution.
type entry struct {
	endpoints []Endpoint
	fetchedAt time.Time
	ttl       time.Duration
}

func (e *entry) stale(now time.Time) bool {
	return now.Sub(e.fetchedAt) >= e.ttl
}

func (e *entry) expired(now time.Time, maxAge time.Duration) bool {
	return now.Sub(e.fetchedAt) >= maxAge
}

// Cache resolves and caches service endpoints with per-entry TTLs. A stale
// entry (past its TTL but within maxAge) is still served immediately while a
// deduplicated background revalidation refreshes it; an entry past maxAge
// is treated as unusable and is fetched synchronously instead, since serving
// data that old would violate the cache's staleness policy regardless of
// how busy the resolver is.
type Cache struct {
	mu           sync.Mutex
	entries      map[string]*entry
	revalidating map[string]bool
	resolver     Resolver
	maxAge       time.Duration
}

// New builds a Cache backed by resolver, refusing to serve any entry older
// than maxAge without a synchronous refetch.
func New(resolver Resolver, maxAge time.Duration) *Cache {
	return &Cache{
		entries:      make(map[string]*entry),
		revalidating: make(map[string]bool),
		resolver:     resolver,
		maxAge:       maxAge,
	}
}

// Resolve returns the cached endpoints for service as of now. A fresh entry
// is returned as-is. A stale-but-not-expired entry is still returned
// immediately, with a deduplicated background revalidation triggered on the
// way out. A missing or hard-expired entry is fetched synchronously, since
// there is nothing safe to serve while that fetch is in flight.
func (c *Cache) Resolve(service string, now time.Time) ([]Endpoint, error) {
	c.mu.Lock()
	e, ok := c.entries[service]
	if ok && !e.expired(now, c.maxAge) {
		endpoints := e.endpoints
		if e.stale(now) {
			c.triggerRevalidateLocked(service, now)
		}
		c.mu.Unlock()
		return endpoints, nil
	}
	c.mu.Unlock()

	return c.fetchAndStore(service, now)
}

// SweepStale ranges every cached entry and triggers a deduplicated
// background revalidation for each one that is stale but not yet expired
// and does not already have a revalidation in flight. It returns the
// services newly triggered by this call, sorted for a deterministic result.
// A background janitor calling this periodically catches services that have
// gone quiet — no one has called Resolve for them since they went stale —
// so their next real request is not the one paying for a synchronous fetch.
func (c *Cache) SweepStale(now time.Time) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var triggered []string
	for service, e := range c.entries {
		if e.expired(now, c.maxAge) {
			continue // too old to trust; the next Resolve call will refetch synchronously
		}
		if e.stale(now) && c.triggerRevalidateLocked(service, now) {
			triggered = append(triggered, service)
		}
	}
	sort.Strings(triggered)
	return triggered
}

// triggerRevalidateLocked starts a background refresh for service unless one
// is already in flight. Must be called with c.mu held. It returns true if
// it actually started a new revalidation.
func (c *Cache) triggerRevalidateLocked(service string, now time.Time) bool {
	if c.revalidating[service] {
		return false
	}
	c.revalidating[service] = true

	go func() {
		defer func() {
			c.mu.Lock()
			delete(c.revalidating, service)
			c.mu.Unlock()
		}()
		c.fetchAndStore(service, now)
	}()
	return true
}

// fetchAndStore calls the resolver and, on success, stores the result under
// fetchedAt = now.
func (c *Cache) fetchAndStore(service string, now time.Time) ([]Endpoint, error) {
	endpoints, ttl, err := c.resolver(service)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.entries[service] = &entry{endpoints: endpoints, fetchedAt: now, ttl: ttl}
	c.mu.Unlock()

	return endpoints, nil
}

// isRevalidating reports whether service currently has a background
// revalidation in flight. It exists for tests to synchronize on
// revalidation completion without a timed sleep.
func (c *Cache) isRevalidating(service string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revalidating[service]
}
```

### The runnable demo

The demo resolves two services fresh, lets both go stale, sweeps them at
once, and then demonstrates the hard-expiry path forcing a synchronous
refetch. Every printed value comes directly from a function's own return,
never from re-reading state a background goroutine might still be writing,
so the output is identical on every run regardless of how the runtime
schedules the background revalidations.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/dns-service-discovery-ttl-cache"
)

func resolve(service string) ([]discovery.Endpoint, time.Duration, error) {
	switch service {
	case "orders-svc":
		return []discovery.Endpoint{{Addr: "10.0.1.5:8080"}}, 30 * time.Second, nil
	case "payments-svc":
		return []discovery.Endpoint{{Addr: "10.0.2.9:8080"}}, 30 * time.Second, nil
	}
	return nil, 0, fmt.Errorf("unknown service: %s", service)
}

func main() {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := discovery.New(resolve, 120*time.Second)

	eps, _ := c.Resolve("orders-svc", base)
	fmt.Printf("orders-svc @t=0: %v\n", eps)

	eps, _ = c.Resolve("payments-svc", base)
	fmt.Printf("payments-svc @t=0: %v\n", eps)

	// Both entries are now stale (ttl=30s) but well within maxAge=120s.
	// A periodic janitor sweep finds them and triggers revalidation.
	triggered := c.SweepStale(base.Add(40 * time.Second))
	fmt.Printf("swept stale @t=40: %v\n", triggered)

	// Far past maxAge: this entry is no longer trustworthy at all, so
	// Resolve fetches synchronously instead of serving anything stale.
	eps, _ = c.Resolve("orders-svc", base.Add(200*time.Second))
	fmt.Printf("orders-svc @t=200 (expired, refetched): %v\n", eps)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
orders-svc @t=0: [{10.0.1.5:8080}]
payments-svc @t=0: [{10.0.2.9:8080}]
swept stale @t=40: [orders-svc payments-svc]
orders-svc @t=200 (expired, refetched): [{10.0.1.5:8080}]
```

### Tests

The tests cover a fresh entry that must not refetch, a stale entry that must
serve immediately and revalidate exactly once, an expired entry that must
block on a synchronous fetch, `SweepStale` correctly declining to double-
trigger a revalidation already in flight (synchronized with a resolver that
blocks on a channel so the in-flight state is observed deterministically,
not guessed at with a sleep), and a concurrent-`Resolve` case proving 20
simultaneous callers against one stale service produce exactly one
revalidation, checked under `-race`.

Create `discovery_test.go`:

```go
package discovery

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitUntilNotRevalidating(t *testing.T, c *Cache, service string) {
	t.Helper()
	for i := 0; i < 1_000_000; i++ {
		if !c.isRevalidating(service) {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("revalidation for %q did not finish in time", service)
}

func TestResolveFreshEntryDoesNotRefetch(t *testing.T) {
	t.Parallel()

	var calls int32
	resolver := func(service string) ([]Endpoint, time.Duration, error) {
		atomic.AddInt32(&calls, 1)
		return []Endpoint{{Addr: "10.0.0.1:80"}}, 30 * time.Second, nil
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(resolver, 120*time.Second)

	c.Resolve("svc", base)
	c.Resolve("svc", base.Add(5*time.Second)) // still fresh, well under ttl

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("resolver calls = %d, want 1 (fresh entry must not refetch)", got)
	}
}

func TestResolveStaleServesImmediatelyAndRevalidates(t *testing.T) {
	t.Parallel()

	var calls int32
	resolver := func(service string) ([]Endpoint, time.Duration, error) {
		n := atomic.AddInt32(&calls, 1)
		return []Endpoint{{Addr: fmt.Sprintf("10.0.0.%d:80", n)}}, 30 * time.Second, nil
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(resolver, 120*time.Second)

	first, _ := c.Resolve("svc", base)
	if first[0].Addr != "10.0.0.1:80" {
		t.Fatalf("first Resolve = %v, want the initial fetch", first)
	}

	staleTime := base.Add(31 * time.Second)
	stale, _ := c.Resolve("svc", staleTime)
	if stale[0].Addr != "10.0.0.1:80" {
		t.Fatalf("stale Resolve = %v, want the OLD data served immediately", stale)
	}

	waitUntilNotRevalidating(t, c, "svc")

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("resolver calls = %d, want 2 (one initial fetch, one revalidation)", got)
	}

	fresh, _ := c.Resolve("svc", staleTime) // still within ttl of the revalidated entry
	if fresh[0].Addr != "10.0.0.2:80" {
		t.Fatalf("post-revalidation Resolve = %v, want the refreshed data", fresh)
	}
}

func TestResolveExpiredFetchesSynchronously(t *testing.T) {
	t.Parallel()

	var calls int32
	resolver := func(service string) ([]Endpoint, time.Duration, error) {
		n := atomic.AddInt32(&calls, 1)
		return []Endpoint{{Addr: fmt.Sprintf("10.0.0.%d:80", n)}}, 30 * time.Second, nil
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(resolver, 120*time.Second)

	c.Resolve("svc", base)
	got, _ := c.Resolve("svc", base.Add(200*time.Second)) // well past maxAge

	if got[0].Addr != "10.0.0.2:80" {
		t.Fatalf("expired Resolve = %v, want a fresh synchronous fetch", got)
	}
	if calls := atomic.LoadInt32(&calls); calls != 2 {
		t.Fatalf("resolver calls = %d, want 2", calls)
	}
}

func TestSweepStaleDedupesAgainstInFlightRevalidation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var calls int32
	resolver := func(service string) ([]Endpoint, time.Duration, error) {
		n := atomic.AddInt32(&calls, 1)
		if n > 1 {
			<-release // hold the revalidation open so we can observe it in flight
		}
		return []Endpoint{{Addr: fmt.Sprintf("10.0.0.%d:80", n)}}, 30 * time.Second, nil
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(resolver, 120*time.Second)

	c.Resolve("svc", base)
	staleTime := base.Add(31 * time.Second)
	c.Resolve("svc", staleTime) // triggers the in-flight revalidation, which now blocks on release

	for i := 0; i < 1_000_000 && !c.isRevalidating("svc"); i++ {
		runtime.Gosched()
	}
	if !c.isRevalidating("svc") {
		t.Fatalf("expected a revalidation to be in flight for svc")
	}

	triggered := c.SweepStale(staleTime)
	if len(triggered) != 0 {
		t.Fatalf("SweepStale() = %v, want none (a revalidation is already in flight)", triggered)
	}

	close(release)
	waitUntilNotRevalidating(t, c, "svc")
}

func TestConcurrentResolveDedupesRevalidation(t *testing.T) {
	t.Parallel()

	var calls int32
	resolver := func(service string) ([]Endpoint, time.Duration, error) {
		n := atomic.AddInt32(&calls, 1)
		return []Endpoint{{Addr: fmt.Sprintf("10.0.0.%d:80", n)}}, 30 * time.Second, nil
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := New(resolver, 120*time.Second)
	c.Resolve("svc", base)

	staleTime := base.Add(31 * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Resolve("svc", staleTime)
		}()
	}
	wg.Wait()

	waitUntilNotRevalidating(t, c, "svc")

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("resolver calls = %d, want exactly 2 (initial fetch + one deduped revalidation)", got)
	}
}
```

Run it:

```bash
go test -count=1 -race ./...
```

## Review

The cache is correct when a fresh entry never triggers a resolver call, a
stale entry always serves its current data immediately while at most one
revalidation runs for it at a time, and an entry past `maxAge` always blocks
on a synchronous fetch rather than serving data too old to trust. The bug
this design specifically avoids is checking `c.revalidating[service]` and
setting it to `true` as two separate steps — a read under one lock
acquisition, a write under another. With N concurrent stale hits for the
same service, splitting them would let every one of them observe the flag
still `false` before any of them set it, and every one of them would launch
its own redundant background fetch — the exact thundering-herd-of-
revalidations this design exists to prevent.
`TestConcurrentResolveDedupesRevalidation` fires 20 such callers at once
under `-race` and requires the resolver to have run exactly twice.

## Resources

- [sync.Mutex](https://pkg.go.dev/sync#Mutex)
- [RFC 1035: Domain Names — Implementation and Specification](https://www.rfc-editor.org/rfc/rfc1035) — the TTL semantics this exercise's `Cache` is modeled on.
- [Fastly: Stale-while-revalidate and stale-if-error](https://www.fastly.com/blog/stale-while-revalidate-stale-if-error-available-today) — the production caching pattern this exercise's `Resolve` implements.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-sliding-window-rate-limiter-log.md](33-sliding-window-rate-limiter-log.md) | Next: [35-hierarchical-quota-cascading-enforcement.md](35-hierarchical-quota-cascading-enforcement.md)
