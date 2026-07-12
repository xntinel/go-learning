# Exercise 32: DNS Service Discovery with TTL Cache and Refresh

**Nivel: Intermedio** — validacion rapida (un test corto).

A service client that re-resolves a downstream's DNS name on every single
request adds a resolver round trip to the hot path and hammers whatever
resolver sits in front of it; one that never re-resolves at all keeps
routing traffic to addresses long after a deploy has rotated them out.
Production clients cache resolution results for a bounded TTL and
explicitly invalidate the cache the moment a cached address is discovered
to be dead, rather than waiting out the remainder of the TTL. This module
builds that resolver, with the refresh written as a bounded loop instead
of a true infinite one, so a misbehaving lookup function cannot spin
forever.

This module is fully self-contained: its own `go mod init`, one test file,
one runnable demo.

## What you'll build

```text
resolver/                      module example.com/resolver
  go.mod                       go 1.24
  resolver.go                  Resolver; New(lookup, ttl, now); (*Resolver).Resolve(name); (*Resolver).MarkFailed(name)
  resolver_test.go                cache hit within TTL, refresh after expiry, MarkFailed forces refresh, recovers after one failure, bounded on permanent failure
  cmd/demo/
    main.go                      one name resolved cold, cached, TTL-expired, then marked failed
```

- Files: `resolver.go`, `resolver_test.go`, `cmd/demo/main.go`.
- Implement: `New(lookup func(string) ([]string, error), ttl time.Duration, now func() time.Time) *Resolver`; `(*Resolver).Resolve(name string) ([]string, error)` — a `for attempt := 0; attempt < r.maxRefreshAttempts; attempt++` loop checking cache freshness against `now()` and refreshing via `lookup` on a miss or a prior failure; `(*Resolver).MarkFailed(name string)` — evicts the cached entry to force the next `Resolve` to refresh.
- Test: repeated calls within the TTL hit the cache without calling `lookup` again; a call after the TTL expires triggers exactly one refresh; `MarkFailed` forces a refresh even with time left on the TTL; a `lookup` that fails once then succeeds recovers on the second attempt; a `lookup` that always fails returns an error after exactly `maxRefreshAttempts` calls.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/02-for-loops/32-dns-discovery-ttl-cache-refresh/cmd/demo
cd go-solutions/03-control-flow/02-for-loops/32-dns-discovery-ttl-cache-refresh
go mod edit -go=1.24
```

### Why a bounded `for` and not a true `for {}`

`Resolve`'s steady-state behavior reads like the classic infinite
check-then-refresh loop from the concepts lesson: check freshness, and if
stale, refresh, then loop back and check again. In the overwhelmingly
common case that second check always succeeds — a successful refresh sets
`expiresAt` to `now().Add(r.ttl)`, which is later than `now()` by
construction, so the very next pass through the loop returns immediately.
That means a bare `for {}` here would, in practice, almost always execute
exactly two passes. The reason this module writes it as a bounded `for
attempt := 0; attempt < r.maxRefreshAttempts; attempt++` anyway, rather
than trusting that "almost always" to hold, is the same bounded-budget
discipline that governs every loop reacting to an external system: if
`ttl` were misconfigured to zero, or `lookup` kept returning success but
`now` were somehow inconsistent with itself, the "refresh, recheck" cycle
could in principle repeat without making progress. The bound turns that
theoretical scenario into a handled error after a fixed number of
attempts instead of a hang, at the cost of nothing in the common case.

The other detail worth calling out is why a failed `lookup` deletes the
cache entry instead of leaving the old one in place: `delete(r.cache,
name)` on the error path means a resolver that failed to refresh does not
keep serving a *known-stale* answer just because deleting it looks more
conservative than keeping it. Combined with `MarkFailed` doing the same
delete from the caller's side (when the caller itself discovers the
cached address is unreachable, without waiting for the TTL), both paths
converge on one rule: the cache only ever holds an entry the resolver
currently trusts, never a fallback.

Create `resolver.go`:

```go
package resolver

import (
	"fmt"
	"time"
)

// entry is one cached DNS answer with the instant it stops being trusted.
type entry struct {
	addrs     []string
	expiresAt time.Time
}

// Resolver caches DNS lookups and refreshes them when the TTL expires or a
// caller reports the cached addresses have started failing.
type Resolver struct {
	lookup             func(name string) ([]string, error)
	ttl                time.Duration
	now                func() time.Time
	cache              map[string]entry
	maxRefreshAttempts int
}

// New builds a Resolver. lookup is the actual DNS (or service-discovery)
// call; ttl is how long a resolved answer is trusted; now is the injected
// clock, so tests can control expiry without a real wall-clock wait.
func New(lookup func(name string) ([]string, error), ttl time.Duration, now func() time.Time) *Resolver {
	return &Resolver{
		lookup:             lookup,
		ttl:                ttl,
		now:                now,
		cache:              make(map[string]entry),
		maxRefreshAttempts: 5,
	}
}

// Resolve returns the addresses for name, serving a cached answer if it is
// still within its TTL, and refreshing from lookup otherwise.
//
// The loop is written as a bounded "for" instead of a true infinite loop,
// even though its steady-state behavior is "check freshness, refresh if
// stale, then the freshness check succeeds": after a successful refresh the
// new entry's expiresAt is now()+ttl, which is always in the future, so the
// very next pass through the loop finds the cache fresh and returns
// immediately. One refresh is normally enough. maxRefreshAttempts exists
// purely as a safety bound for the case that should never happen in a
// correctly configured system -- a ttl of zero, or a clock that is out of
// sync with itself -- so a misconfiguration turns into a bounded error
// instead of a hot loop that never returns.
func (r *Resolver) Resolve(name string) ([]string, error) {
	var lastErr error

	for attempt := 0; attempt < r.maxRefreshAttempts; attempt++ {
		if e, ok := r.cache[name]; ok && r.now().Before(e.expiresAt) {
			return e.addrs, nil
		}

		addrs, err := r.lookup(name)
		if err != nil {
			lastErr = err
			delete(r.cache, name) // a stale entry must never be served after a failed refresh
			continue
		}

		r.cache[name] = entry{addrs: addrs, expiresAt: r.now().Add(r.ttl)}
		// loop back: the freshness check above will now succeed
	}

	if lastErr != nil {
		return nil, fmt.Errorf("resolver: resolve %q: %w", name, lastErr)
	}
	return nil, fmt.Errorf("resolver: resolve %q: exceeded %d refresh attempts", name, r.maxRefreshAttempts)
}

// MarkFailed evicts name's cached entry, forcing the next Resolve call to
// refresh even if the TTL has not yet expired. A caller uses this after
// discovering that a cached address is no longer reachable, without
// waiting out the remainder of the TTL.
func (r *Resolver) MarkFailed(name string) {
	delete(r.cache, name)
}
```

### The runnable demo

The demo resolves one name cold, again within its TTL (no new lookup),
again after the TTL has been advanced past expiry (one new lookup), and
finally after `MarkFailed` forces a refresh despite the TTL not having
expired.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/resolver"
)

func main() {
	t := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return t }

	calls := 0
	lookup := func(name string) ([]string, error) {
		calls++
		return []string{fmt.Sprintf("10.0.0.%d", calls)}, nil
	}

	r := resolver.New(lookup, time.Minute, clock)

	addrs, _ := r.Resolve("payments.internal")
	fmt.Printf("resolve #1 (cold cache): %v (lookups so far: %d)\n", addrs, calls)

	addrs, _ = r.Resolve("payments.internal")
	fmt.Printf("resolve #2 (within TTL): %v (lookups so far: %d)\n", addrs, calls)

	t = t.Add(2 * time.Minute)
	addrs, _ = r.Resolve("payments.internal")
	fmt.Printf("resolve #3 (TTL expired): %v (lookups so far: %d)\n", addrs, calls)

	r.MarkFailed("payments.internal")
	addrs, _ = r.Resolve("payments.internal")
	fmt.Printf("resolve #4 (marked failed): %v (lookups so far: %d)\n", addrs, calls)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
resolve #1 (cold cache): [10.0.0.1] (lookups so far: 1)
resolve #2 (within TTL): [10.0.0.1] (lookups so far: 1)
resolve #3 (TTL expired): [10.0.0.2] (lookups so far: 2)
resolve #4 (marked failed): [10.0.0.3] (lookups so far: 3)
```

### Tests

`TestResolveCachesWithinTTLWithoutRelooking` and
`TestResolveRefreshesAfterTTLExpires` establish the basic TTL contract.
`TestMarkFailedForcesRefreshBeforeTTLExpires` checks the explicit
eviction path. `TestResolveRecoversAfterOneFailedLookup` confirms a
transient failure does not poison future calls.
`TestResolveBoundedByMaxRefreshAttemptsWhenLookupAlwaysFails` is the one
that proves the loop actually terminates under a permanently broken
`lookup`, asserting `lookup` was called exactly `maxRefreshAttempts`
times — not more, and not fewer. A `manualClock` advanced only by
explicit test code keeps every assertion deterministic without a real
wall-clock wait.

Create `resolver_test.go`:

```go
package resolver

import (
	"errors"
	"slices"
	"testing"
	"time"
)

type manualClock struct{ t time.Time }

func (c *manualClock) Now() time.Time          { return c.t }
func (c *manualClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func TestResolveCachesWithinTTLWithoutRelooking(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	calls := 0
	lookup := func(string) ([]string, error) {
		calls++
		return []string{"10.0.0.1"}, nil
	}

	r := New(lookup, time.Minute, clock.Now)

	for i := 0; i < 3; i++ {
		addrs, err := r.Resolve("api.internal")
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if !slices.Equal(addrs, []string{"10.0.0.1"}) {
			t.Fatalf("Resolve() = %v, want [10.0.0.1]", addrs)
		}
	}
	if calls != 1 {
		t.Fatalf("lookup called %d times, want 1 (cache hit for the next 2 calls)", calls)
	}
}

func TestResolveRefreshesAfterTTLExpires(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	calls := 0
	lookup := func(string) ([]string, error) {
		calls++
		return []string{"10.0.0.1"}, nil
	}

	r := New(lookup, time.Minute, clock.Now)

	if _, err := r.Resolve("api.internal"); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	clock.Advance(time.Minute + time.Second)

	if _, err := r.Resolve("api.internal"); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("lookup called %d times, want 2 (TTL expired, must re-fetch)", calls)
	}
}

func TestMarkFailedForcesRefreshBeforeTTLExpires(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	calls := 0
	lookup := func(string) ([]string, error) {
		calls++
		return []string{"10.0.0.1"}, nil
	}

	r := New(lookup, time.Hour, clock.Now)

	if _, err := r.Resolve("api.internal"); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	r.MarkFailed("api.internal")

	if _, err := r.Resolve("api.internal"); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("lookup called %d times, want 2 (MarkFailed must force a refresh)", calls)
	}
}

func TestResolveRecoversAfterOneFailedLookup(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	calls := 0
	errUnreachable := errors.New("dns server unreachable")
	lookup := func(string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, errUnreachable
		}
		return []string{"10.0.0.2"}, nil
	}

	r := New(lookup, time.Minute, clock.Now)

	addrs, err := r.Resolve("api.internal")
	if err != nil {
		t.Fatalf("Resolve() error = %v, want nil (should recover on the second attempt)", err)
	}
	if !slices.Equal(addrs, []string{"10.0.0.2"}) {
		t.Fatalf("Resolve() = %v, want [10.0.0.2]", addrs)
	}
	if calls != 2 {
		t.Fatalf("lookup called %d times, want 2", calls)
	}
}

func TestResolveBoundedByMaxRefreshAttemptsWhenLookupAlwaysFails(t *testing.T) {
	t.Parallel()

	clock := &manualClock{t: time.Unix(0, 0)}
	calls := 0
	errDown := errors.New("dns server down")
	lookup := func(string) ([]string, error) {
		calls++
		return nil, errDown
	}

	r := New(lookup, time.Minute, clock.Now)

	_, err := r.Resolve("api.internal")
	if err == nil {
		t.Fatal("Resolve() error = nil, want an error")
	}
	if !errors.Is(err, errDown) {
		t.Fatalf("Resolve() error = %v, want wrapping %v", err, errDown)
	}
	if calls != r.maxRefreshAttempts {
		t.Fatalf("lookup called %d times, want exactly %d (bounded loop)", calls, r.maxRefreshAttempts)
	}
}
```

## Review

`Resolve` is correct when it never calls `lookup` for a name whose cached
entry is still fresh, always calls it after an expiry or a `MarkFailed`,
and always terminates -- with a wrapped error -- rather than looping
forever when `lookup` cannot succeed. The common mistake this design
avoids is writing the refresh as a genuine `for {}` on the assumption that
"a successful refresh always makes the next check pass," which is true in
the correctly configured case but leaves no defense against the
misconfigured one: a `ttl` of zero would make every refreshed entry
already expired by the time the loop rechecks it, and an unbounded loop
under that condition pins a goroutine forever instead of surfacing a
config error. Run `go test -count=1 ./...`.

## Resources

- [RFC 1035: Domain Names — Implementation and Specification](https://www.rfc-editor.org/rfc/rfc1035) — the TTL concept this cache mirrors.
- [Kubernetes DNS-based service discovery](https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/) — a production system where clients cache resolved endpoints and must handle stale entries.
- [Go Specification: For statements](https://go.dev/ref/spec#For_statements) — the bounded refresh loop and its two exits.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [31-hierarchical-quota-token-cascade.md](31-hierarchical-quota-token-cascade.md) | Next: [33-request-singleflight-deduplication.md](33-request-singleflight-deduplication.md)
