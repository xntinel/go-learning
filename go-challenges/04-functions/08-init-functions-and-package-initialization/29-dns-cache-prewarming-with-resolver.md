# Exercise 29: DNS Resolver Cache Prewarmed at init with Common Service Endpoints and Derived TTLs

**Nivel: Intermedio** — validacion rapida (un test corto).

A service that talks to a handful of internal dependencies over DNS-resolved
hostnames pays a cold-lookup penalty on its first request to each one,
unless something resolves them ahead of time. This exercise validates the
list of well-known internal hostnames at init, derives a package-wide
minimum TTL from per-host overrides, and provides a `Cache` that eagerly
resolves and stores records for that host list through an injected
`Resolver` — so tests and the demo never depend on real DNS.

## What you'll build

```text
dnswarm/                    independent module: example.com/dnswarm
  go.mod                     module example.com/dnswarm
  dnswarm.go                   host validation + derived min TTL + Resolver + Cache (Prewarm/Lookup/IsExpired)
  cmd/
    demo/
      main.go                  prewarms three hosts with a fake resolver, checks expiry before/after
  dnswarm_test.go              validateHosts table + computeMinTTL + derived-TTL check + Cache prewarm/expiry
```

Files: `dnswarm.go`, `cmd/demo/main.go`, `dnswarm_test.go`.
Implement: `validateHosts([]string) error` rejecting an empty list, a blank entry, and a duplicate; `computeMinTTL(hosts, overrides, default) time.Duration`; `Cache.Prewarm(hosts []string, now time.Time) map[string]error` resolving every host eagerly and caching successes; `Cache.IsExpired(host string, now time.Time) bool`.
Test: `validateHosts` accepts a clean list and rejects each bad shape; `computeMinTTL` picks the smallest TTL across overrides and defaults; the package's own derived `DerivedMinTTL()` matches hand computation; `Prewarm` caches successes, reports failures per host without caching them, and `IsExpired` is false immediately after prewarm, true once the TTL elapses, and true for a host never prewarmed.
Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why prewarm, and why derive the TTL policy at init too

The list of internal hostnames a service depends on — `auth.internal`,
`billing.internal`, `search.internal` — is static configuration, exactly
like the regexp patterns and allow-lists earlier in this chapter. It is
worth validating at init for the same reason: an empty list, a blank
hostname, or an accidental duplicate is a configuration mistake that should
fail the instant the binary starts, not silently produce a cache that never
prewarms one of its supposed dependents.

The second static computation is `derivedMinTTL`: given a default TTL and a
map of per-host overrides (some services' DNS records rotate faster than
others), the smallest TTL across every configured host is the interval a
background refresher would need to re-check *something* — it is derived
once from the same configuration that drives per-host TTLs, not
independently hand-maintained and liable to drift out of sync with the
overrides map.

`Cache.Prewarm` itself does the actual eager resolution, through an
injected `Resolver` function rather than calling `net.LookupHost` directly.
That mirrors the `Clock` injection pattern used for rate limiting earlier in
this chapter: production code passes a resolver backed by real DNS, while
tests and the demo pass a fake table lookup, so a network hiccup or a slow
resolver can never make this package's tests flaky. `Prewarm` collects
per-host errors instead of stopping at the first failure, because one
unreachable dependency should not prevent prewarming the rest.

Create `dnswarm.go`:

```go
// dnswarm.go
// Package dnswarm prewarms a DNS cache for a fixed set of common internal
// service hostnames, and derives a package-wide minimum TTL from per-host
// overrides at init -- so the first real request to any of these services
// never pays a cold DNS lookup, and the cache's refresh policy is computed
// once instead of recomputed on every check.
package dnswarm

import (
	"fmt"
	"sync"
	"time"
)

// defaultTTL applies to any host without an explicit override.
const defaultTTL = 60 * time.Second

// hostTTLOverrides gives a shorter or longer TTL to specific hosts.
var hostTTLOverrides = map[string]time.Duration{
	"auth.internal":    30 * time.Second,
	"billing.internal": 120 * time.Second,
}

// commonHosts are the internal services prewarmed at startup.
var commonHosts = []string{"auth.internal", "billing.internal", "search.internal"}

// derivedMinTTL is the smallest TTL among all commonHosts (using each
// host's override, or defaultTTL when it has none), computed once at init.
// A background refresher would use this as its sweep interval.
var derivedMinTTL time.Duration

func init() {
	if err := validateHosts(commonHosts); err != nil {
		panic("dnswarm: " + err.Error())
	}
	derivedMinTTL = computeMinTTL(commonHosts, hostTTLOverrides, defaultTTL)
}

// validateHosts rejects an empty list and any blank or duplicate hostname.
// Extracted from init so tests can exercise it directly.
func validateHosts(hosts []string) error {
	if len(hosts) == 0 {
		return fmt.Errorf("host list is empty")
	}
	seen := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		if h == "" {
			return fmt.Errorf("host list contains a blank entry")
		}
		if _, dup := seen[h]; dup {
			return fmt.Errorf("duplicate host %q", h)
		}
		seen[h] = struct{}{}
	}
	return nil
}

// computeMinTTL returns the smallest TTL across hosts, using overrides[h]
// when present and def otherwise.
func computeMinTTL(hosts []string, overrides map[string]time.Duration, def time.Duration) time.Duration {
	min := def
	for _, h := range hosts {
		ttl := def
		if o, ok := overrides[h]; ok {
			ttl = o
		}
		if ttl < min {
			min = ttl
		}
	}
	return min
}

// DerivedMinTTL returns the package-wide minimum TTL computed at init.
func DerivedMinTTL() time.Duration { return derivedMinTTL }

// Record is one cached DNS resolution.
type Record struct {
	IPs        []string
	TTL        time.Duration
	ResolvedAt time.Time
}

// Resolver resolves a hostname to its IP addresses. Production code passes
// a function backed by net.LookupHost; tests and the demo pass a fake
// resolver so results are deterministic.
type Resolver func(host string) ([]string, error)

// Cache holds prewarmed DNS records, keyed by hostname.
type Cache struct {
	mu       sync.Mutex
	resolver Resolver
	entries  map[string]Record
}

// NewCache returns a Cache that resolves hosts using resolver.
func NewCache(resolver Resolver) *Cache {
	return &Cache{resolver: resolver, entries: make(map[string]Record)}
}

// Prewarm resolves every host in hosts right away, storing each result
// (with its configured TTL) as of now. It returns a map of host to error for
// any host that failed to resolve; hosts that succeeded are still cached.
func (c *Cache) Prewarm(hosts []string, now time.Time) map[string]error {
	errs := make(map[string]error)
	for _, h := range hosts {
		ips, err := c.resolver(h)
		if err != nil {
			errs[h] = err
			continue
		}
		ttl := defaultTTL
		if o, ok := hostTTLOverrides[h]; ok {
			ttl = o
		}
		c.mu.Lock()
		c.entries[h] = Record{IPs: ips, TTL: ttl, ResolvedAt: now}
		c.mu.Unlock()
	}
	return errs
}

// Lookup returns the cached record for host, if any.
func (c *Cache) Lookup(host string) (Record, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[host]
	return r, ok
}

// IsExpired reports whether host's cached record is missing or has outlived
// its TTL as of now.
func (c *Cache) IsExpired(host string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[host]
	if !ok {
		return true
	}
	return now.Sub(r.ResolvedAt) >= r.TTL
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"time"

	"example.com/dnswarm"
)

func fakeResolver(host string) ([]string, error) {
	table := map[string][]string{
		"auth.internal":    {"10.0.0.1"},
		"billing.internal": {"10.0.0.2"},
		"search.internal":  {"10.0.0.3"},
	}
	ips, ok := table[host]
	if !ok {
		return nil, fmt.Errorf("no such host %q", host)
	}
	return ips, nil
}

func main() {
	fmt.Println("derived min TTL:", dnswarm.DerivedMinTTL())

	cache := dnswarm.NewCache(fakeResolver)
	start := time.Unix(0, 0)
	errs := cache.Prewarm([]string{"auth.internal", "billing.internal", "search.internal"}, start)
	fmt.Println("prewarm errors:", len(errs))

	rec, _ := cache.Lookup("auth.internal")
	fmt.Println("auth.internal IPs:", rec.IPs, "TTL:", rec.TTL)

	fmt.Println("expired right after prewarm:", cache.IsExpired("auth.internal", start))

	later := start.Add(31 * time.Second) // past auth.internal's 30s TTL
	fmt.Println("expired after 31s:", cache.IsExpired("auth.internal", later))
	fmt.Println("billing expired after 31s (120s TTL):", cache.IsExpired("billing.internal", later))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
derived min TTL: 30s
prewarm errors: 0
auth.internal IPs: [10.0.0.1] TTL: 30s
expired right after prewarm: false
expired after 31s: true
billing expired after 31s (120s TTL): false
```

### Tests

Create `dnswarm_test.go`:

```go
// dnswarm_test.go
package dnswarm

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestValidateHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		hosts   []string
		wantErr string
	}{
		{"ok", []string{"a", "b"}, ""},
		{"empty list", nil, "empty"},
		{"blank entry", []string{"a", ""}, "blank"},
		{"duplicate", []string{"a", "a"}, "duplicate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateHosts(tc.hosts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestComputeMinTTL(t *testing.T) {
	t.Parallel()

	hosts := []string{"a", "b", "c"}
	overrides := map[string]time.Duration{"a": 10 * time.Second, "b": 5 * time.Second}
	got := computeMinTTL(hosts, overrides, 60*time.Second)
	if got != 5*time.Second {
		t.Fatalf("computeMinTTL = %v, want 5s", got)
	}
}

func TestDerivedMinTTLFromPackageConfig(t *testing.T) {
	t.Parallel()

	// auth.internal overrides to 30s, the smallest of the package's
	// configured hosts (30s, 120s, and defaultTTL 60s for search.internal).
	if got := DerivedMinTTL(); got != 30*time.Second {
		t.Fatalf("DerivedMinTTL() = %v, want 30s", got)
	}
}

func TestCachePrewarmAndExpiry(t *testing.T) {
	t.Parallel()

	resolver := func(host string) ([]string, error) {
		if host == "broken.internal" {
			return nil, fmt.Errorf("lookup failed")
		}
		return []string{"10.0.0.1"}, nil
	}
	c := NewCache(resolver)
	start := time.Unix(0, 0)

	errs := c.Prewarm([]string{"auth.internal", "broken.internal"}, start)
	if len(errs) != 1 || errs["broken.internal"] == nil {
		t.Fatalf("Prewarm errs = %v, want exactly one error for broken.internal", errs)
	}

	rec, ok := c.Lookup("auth.internal")
	if !ok {
		t.Fatal("expected auth.internal to be cached")
	}
	if rec.TTL != 30*time.Second {
		t.Fatalf("auth.internal TTL = %v, want 30s (its override)", rec.TTL)
	}

	if _, ok := c.Lookup("broken.internal"); ok {
		t.Fatal("broken.internal should not be cached after a resolve failure")
	}

	if c.IsExpired("auth.internal", start) {
		t.Fatal("record should not be expired immediately after prewarm")
	}
	if !c.IsExpired("auth.internal", start.Add(31*time.Second)) {
		t.Fatal("record should be expired after its 30s TTL has elapsed")
	}
	if c.IsExpired("unknown.internal", start) == false {
		t.Fatal("a host never prewarmed should report as expired")
	}
}
```

## Review

The design is correct when `validateHosts` catches every malformed shape of
the static host list before anything tries to prewarm it, when
`computeMinTTL` picks the true minimum across overrides and the default
(and the package's own `DerivedMinTTL()` matches a hand computation over its
real configuration), and when `Cache.Prewarm` clearly separates hosts that
resolved successfully (cached, with the right per-host TTL) from ones that
did not (reported in the returned error map, never cached). `IsExpired`
ties it together: false immediately after prewarming, true once `now`
crosses `ResolvedAt + TTL`, and true — not a panic, not a zero-value
record — for a host that was never prewarmed at all.

The mistake to avoid is calling real DNS resolution (`net.LookupHost`)
directly inside `Prewarm` or, worse, inside `init()` itself: that makes the
package's own startup depend on network reachability and turns every test
run into a flaky, environment-dependent one. Injecting `Resolver` keeps the
eager-resolution *behavior* testable while letting production code supply
`net.LookupHost` (adapted to this signature) as the real implementation.

## Resources

- [net.LookupHost](https://pkg.go.dev/net#LookupHost) — the real resolver a production `Resolver` would wrap.
- [Go spec: Package initialization](https://go.dev/ref/spec#Package_initialization) — why the static host list and derived TTL are validated in `init()`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-api-base-url-parser-and-validator.md](28-api-base-url-parser-and-validator.md) | Next: [30-circuit-breaker-thresholds-from-config.md](30-circuit-breaker-thresholds-from-config.md)
