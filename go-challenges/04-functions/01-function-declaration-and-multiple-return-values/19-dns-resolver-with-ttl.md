# Exercise 19: DNS Resolution With TTL Propagation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A DNS name is rarely one hop: `www.example.com` might `CNAME` to
`edge.example.com`, which `CNAME`s again to a terminal `A` record with the
actual IPs. Each link in that chain carries its own TTL, and the answer as
a whole can only be trusted for as long as the *shortest-lived* link —
caching the outer TTL and ignoring an inner record that expires sooner
serves stale IPs. This exercise builds a concurrency-safe
`Resolver.Resolve(host) (ips []string, ttl time.Duration, error)` that
follows the chain, propagates the minimum TTL, and caches the result with
an injected clock so the remaining TTL counts down deterministically.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
dnsresolver/                 independent module: example.com/dns-resolver-with-ttl
  go.mod                     go 1.24
  dnsresolver.go              package dnsresolver; Record, Resolver, Resolve(host) (ips,ttl,error); mutex-protected cache
  cmd/
    demo/
      main.go                 chained resolve, a cache countdown, and a concurrent resolve of several names
  dnsresolver_test.go         min-TTL propagation; cache countdown; cache expiry/refetch; nxdomain; loop detection; -race
```

- Files: `dnsresolver.go`, `cmd/demo/main.go`, `dnsresolver_test.go`.
- Implement: `(*Resolver).Resolve(host string) (ips []string, ttl time.Duration, err error)`, following `CNAME` records to a terminal `A` record, propagating the minimum TTL across the chain, and caching the answer under an injected clock (`now func() time.Time`) protected by a mutex.
- Test: a three-link chain returns the minimum TTL of the three records; resolving the same name again after the injected clock advances returns the correctly decremented remaining TTL; an absent name, and a `CNAME` loop, both return distinct sentinel-wrapped errors; concurrent `Resolve` calls from many goroutines are race-free (`-race`).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/01-function-declaration-and-multiple-return-values/19-dns-resolver-with-ttl/cmd/demo
cd go-solutions/04-functions/01-function-declaration-and-multiple-return-values/19-dns-resolver-with-ttl
go mod edit -go=1.24
```

### Why the chain's TTL is a minimum, not the last hop's

Caching the terminal record's TTL and ignoring the `CNAME` links above it
is the mistake this exercise exists to catch. If `www` has a 300s TTL
`CNAME` to `edge`, which has a 60s TTL `CNAME` to `origin`'s 30s-TTL `A`
record, the overall answer for `www` is only as fresh as the *shortest*
link — 30 seconds — because if `edge`'s record changes in 60 seconds, the
chain from `www` is stale even though `www`'s own `CNAME` record still has
270 seconds left to live:

```go
if !haveTTL || rec.TTL < minTTL {
    minTTL = rec.TTL
    haveTTL = true
}
```

`Resolve` also has to be safe when many goroutines resolve concurrently —
a real resolver serves lookups from many request handlers at once. The
cache read (`is this still fresh`) and the cache write (`store the new
answer`) are check-then-act: both must happen under the same lock
acquisition, or two goroutines could both miss the cache and both walk the
chain redundantly (correct but wasteful), or worse, one could read a
half-written cache entry. This resolver holds one `sync.Mutex` for the
entire `Resolve` call so the check and the act are never split.

Create `dnsresolver.go`:

```go
package dnsresolver

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrNXDomain is returned when a name has no record at all.
var ErrNXDomain = errors.New("nxdomain")

// ErrResolutionLoop is returned when a CNAME chain revisits a name it
// already followed.
var ErrResolutionLoop = errors.New("cname resolution loop")

// ErrChainTooLong is returned when a CNAME chain exceeds maxChainDepth
// without reaching a terminal A record — a runaway or misconfigured zone.
var ErrChainTooLong = errors.New("cname chain too long")

const maxChainDepth = 8

// Record is one zone entry: either a CNAME pointing at another name, or a
// terminal set of A-record IPs. Real DNS servers never mix the two on one
// name, and neither does this one.
type Record struct {
	CNAME string
	IPs   []string
	TTL   time.Duration
}

type cacheEntry struct {
	ips       []string
	ttl       time.Duration
	expiresAt time.Time
}

// Resolver resolves names against an in-memory zone, following CNAME
// chains and caching the terminal answer under the chain's minimum TTL —
// the answer can never outlive the shortest-lived link that produced it.
// It is safe for concurrent use.
type Resolver struct {
	mu      sync.Mutex
	records map[string]Record
	cache   map[string]cacheEntry
	now     func() time.Time
}

// NewResolver builds a Resolver over records, using now as the injectable
// clock (pass time.Now in production, a fixed func in tests).
func NewResolver(records map[string]Record, now func() time.Time) *Resolver {
	return &Resolver{
		records: records,
		cache:   make(map[string]cacheEntry),
		now:     now,
	}
}

// Resolve returns the terminal IPs for host, the remaining time-to-live of
// that answer, and an error for nxdomain, a resolution loop, or a chain
// that exceeds maxChainDepth. A cache hit returns the TTL remaining until
// expiry, not the record's original TTL, so a caller that resolves the
// same name twice sees the clock ticking down.
func (r *Resolver) Resolve(host string) (ips []string, ttl time.Duration, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if entry, ok := r.cache[host]; ok && now.Before(entry.expiresAt) {
		return entry.ips, entry.expiresAt.Sub(now), nil
	}

	ips, minTTL, err := r.followChain(host)
	if err != nil {
		return nil, 0, err
	}

	r.cache[host] = cacheEntry{ips: ips, ttl: minTTL, expiresAt: now.Add(minTTL)}
	return ips, minTTL, nil
}

// followChain walks CNAME records starting at host until it reaches a
// terminal A record, propagating the minimum TTL seen along the way.
func (r *Resolver) followChain(host string) ([]string, time.Duration, error) {
	visited := make(map[string]bool)
	current := host
	var minTTL time.Duration
	haveTTL := false

	for depth := 0; depth <= maxChainDepth; depth++ {
		if visited[current] {
			return nil, 0, fmt.Errorf("resolve %q: %w at %q", host, ErrResolutionLoop, current)
		}
		visited[current] = true

		rec, ok := r.records[current]
		if !ok {
			return nil, 0, fmt.Errorf("resolve %q: %w for %q", host, ErrNXDomain, current)
		}

		if !haveTTL || rec.TTL < minTTL {
			minTTL = rec.TTL
			haveTTL = true
		}

		if rec.CNAME != "" {
			current = rec.CNAME
			continue
		}
		return rec.IPs, minTTL, nil
	}
	return nil, 0, fmt.Errorf("resolve %q: %w", host, ErrChainTooLong)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"example.com/dns-resolver-with-ttl"
)

func main() {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	clockNow := start
	clock := func() time.Time { return clockNow }

	records := map[string]dnsresolver.Record{
		"www.example.com":  {CNAME: "edge.example.com", TTL: 300 * time.Second},
		"edge.example.com": {CNAME: "origin.example.com", TTL: 60 * time.Second},
		"origin.example.com": {
			IPs: []string{"203.0.113.10", "203.0.113.11"},
			TTL: 30 * time.Second,
		},
	}
	resolver := dnsresolver.NewResolver(records, clock)

	ips, ttl, err := resolver.Resolve("www.example.com")
	if err != nil {
		fmt.Println("resolve error:", err)
		return
	}
	sort.Strings(ips)
	fmt.Printf("first resolve:  ips=%v ttl=%s (min of 300s,60s,30s)\n", ips, ttl)

	// Advance the injected clock by 10s and resolve again: the cached
	// answer's remaining TTL must have ticked down accordingly.
	clockNow = clockNow.Add(10 * time.Second)
	ips, ttl, err = resolver.Resolve("www.example.com")
	if err != nil {
		fmt.Println("resolve error:", err)
		return
	}
	sort.Strings(ips)
	fmt.Printf("after 10s:      ips=%v ttl=%s\n", ips, ttl)

	// Concurrent resolution of several names; goroutine output is
	// collected and sorted before printing so it stays deterministic.
	names := []string{"www.example.com", "edge.example.com", "origin.example.com"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	lines := make([]string, 0, len(names))
	for _, name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			ips, _, err := resolver.Resolve(name)
			sort.Strings(ips)
			mu.Lock()
			lines = append(lines, fmt.Sprintf("%s -> ips=%v err=%v", name, ips, err))
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	sort.Strings(lines)
	for _, line := range lines {
		fmt.Println(line)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
first resolve:  ips=[203.0.113.10 203.0.113.11] ttl=30s (min of 300s,60s,30s)
after 10s:      ips=[203.0.113.10 203.0.113.11] ttl=20s
edge.example.com -> ips=[203.0.113.10 203.0.113.11] err=<nil>
origin.example.com -> ips=[203.0.113.10 203.0.113.11] err=<nil>
www.example.com -> ips=[203.0.113.10 203.0.113.11] err=<nil>
```

### Tests

Create `dnsresolver_test.go`:

```go
package dnsresolver

import (
	"errors"
	"sort"
	"sync"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestResolveChainPropagatesMinTTL(t *testing.T) {
	t.Parallel()
	records := map[string]Record{
		"www":    {CNAME: "edge", TTL: 300 * time.Second},
		"edge":   {CNAME: "origin", TTL: 60 * time.Second},
		"origin": {IPs: []string{"10.0.0.1"}, TTL: 30 * time.Second},
	}
	r := NewResolver(records, fixedClock(time.Unix(0, 0)))

	ips, ttl, err := r.Resolve("www")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(ips) != 1 || ips[0] != "10.0.0.1" {
		t.Fatalf("ips = %v, want [10.0.0.1]", ips)
	}
	if ttl != 30*time.Second {
		t.Fatalf("ttl = %s, want 30s (the minimum along the chain)", ttl)
	}
}

func TestResolveCacheTTLCountsDown(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	clockNow := now
	clock := func() time.Time { return clockNow }

	records := map[string]Record{
		"host": {IPs: []string{"10.0.0.1"}, TTL: 100 * time.Second},
	}
	r := NewResolver(records, clock)

	_, ttl, err := r.Resolve("host")
	if err != nil || ttl != 100*time.Second {
		t.Fatalf("first resolve: ttl=%s err=%v, want 100s, nil", ttl, err)
	}

	clockNow = clockNow.Add(40 * time.Second)
	_, ttl, err = r.Resolve("host")
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if ttl != 60*time.Second {
		t.Fatalf("ttl = %s, want 60s remaining after 40s elapsed", ttl)
	}
}

func TestResolveCacheExpiresAndRefetches(t *testing.T) {
	t.Parallel()
	clockNow := time.Unix(0, 0)
	clock := func() time.Time { return clockNow }

	records := map[string]Record{
		"host": {IPs: []string{"10.0.0.1"}, TTL: 10 * time.Second},
	}
	r := NewResolver(records, clock)

	if _, _, err := r.Resolve("host"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	clockNow = clockNow.Add(11 * time.Second)
	_, ttl, err := r.Resolve("host")
	if err != nil {
		t.Fatalf("resolve after expiry: %v", err)
	}
	if ttl != 10*time.Second {
		t.Fatalf("ttl = %s after cache expiry, want a fresh 10s", ttl)
	}
}

func TestResolveNXDomain(t *testing.T) {
	t.Parallel()
	r := NewResolver(map[string]Record{}, fixedClock(time.Unix(0, 0)))

	_, _, err := r.Resolve("ghost.example.com")
	if !errors.Is(err, ErrNXDomain) {
		t.Fatalf("err = %v, want ErrNXDomain", err)
	}
}

func TestResolveDetectsLoop(t *testing.T) {
	t.Parallel()
	records := map[string]Record{
		"a": {CNAME: "b", TTL: time.Second},
		"b": {CNAME: "a", TTL: time.Second},
	}
	r := NewResolver(records, fixedClock(time.Unix(0, 0)))

	_, _, err := r.Resolve("a")
	if !errors.Is(err, ErrResolutionLoop) {
		t.Fatalf("err = %v, want ErrResolutionLoop", err)
	}
}

func TestResolveConcurrentIsRaceFree(t *testing.T) {
	t.Parallel()
	records := map[string]Record{
		"www":    {CNAME: "edge", TTL: 300 * time.Second},
		"edge":   {CNAME: "origin", TTL: 60 * time.Second},
		"origin": {IPs: []string{"10.0.0.1", "10.0.0.2"}, TTL: 30 * time.Second},
	}
	r := NewResolver(records, fixedClock(time.Unix(0, 0)))

	const workers = 20
	var wg sync.WaitGroup
	results := make([][]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ips, _, err := r.Resolve("www")
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			sort.Strings(ips)
			results[i] = ips
		}(i)
	}
	wg.Wait()

	want := []string{"10.0.0.1", "10.0.0.2"}
	for i, got := range results {
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("worker %d: ips = %v, want %v", i, got, want)
		}
	}
}
```

Note `TestResolveConcurrentIsRaceFree` uses `t.Errorf` (not `t.Fatalf`)
inside the goroutines — `Fatalf` calls `runtime.Goexit`, which must only
happen on the test's own goroutine, never inside a spawned one.

## Review

`Resolve` is correct when the propagated TTL is always the minimum across
every link in the chain, never just the terminal record's own TTL, and
when the cache's remaining TTL genuinely counts down against the injected
clock rather than being re-issued at the full value on every call.
`TestResolveChainPropagatesMinTTL` and `TestResolveCacheTTLCountsDown` are
the load-bearing tests: the first catches a resolver that only looks at
the last hop's TTL, the second catches one that caches the *original* TTL
instead of an absolute expiry time. `TestResolveConcurrentIsRaceFree`
proves the check-then-act on the cache — read the entry, decide fresh or
stale, possibly write a new one — never splits across the mutex boundary
under real concurrent load.

The mistake to avoid is computing the cache's remaining TTL by subtracting
from the record's stored TTL a second time on every read (`ttl -=
timeSinceLastRead`) — that compounds rounding and drifts from the truth.
Store an absolute `expiresAt` once, at write time, and always derive the
remaining TTL as `expiresAt.Sub(now)` at read time instead.

## Resources

- [RFC 1035: Domain Names — Implementation and Specification](https://www.rfc-editor.org/rfc/rfc1035) — the CNAME chain and per-record TTL model this exercise simulates.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the lock protecting the cache's check-then-act.
- [The Go Memory Model](https://go.dev/ref/mem) — why a shared cache map needs a mutex (or another synchronization primitive) for concurrent reads and writes to be race-free.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-schema-migration-compatibility-check.md](18-schema-migration-compatibility-check.md) | Next: [20-env-var-lookup-with-source.md](20-env-var-lookup-with-source.md)
