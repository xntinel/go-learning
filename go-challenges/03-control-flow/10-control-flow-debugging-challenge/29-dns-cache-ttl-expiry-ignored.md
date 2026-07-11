# Exercise 29: DNS Service Discovery Cache Never Respects TTL Expiry

**Nivel: Intermedio** — validacion rapida (un test corto).

A service-discovery resolver that caches DNS answers exists to avoid
paying a network round trip on every single call, but the TTL that
comes back with each answer is not a suggestion — it is the backend
operator's promise about how long that address is expected to stay
valid. A cache that stores the answer but never actually checks
whether it has aged past its TTL is not a cache with a bug in its
eviction policy; it is a cache with no eviction policy at all, silently
promoted to "cache forever." The failure mode is specific and painful:
a deployment scales a backend down, DNS starts returning a different
set of addresses, and every process still holding the stale cached
entry keeps sending traffic at a target that no longer answers,
indistinguishable from a network partition until someone thinks to
check the resolver's cache age. This module is fully self-contained:
its own `go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
resolver/                    independent module: example.com/dns-cache-ttl-expiry-ignored
  go.mod                      go 1.21
  resolver.go                  Answer, Lookup, Cache, Resolve
  cmd/
    demo/
      main.go                  runnable demo: a backend address that changes once the TTL expires
  resolver_test.go              TTL-boundary table, plus a lookup-error-does-not-cache case
```

- Files: `resolver.go`, `cmd/demo/main.go`, `resolver_test.go`.
- Implement: `Cache.Resolve(host string) (Answer, error)` that returns the cached answer only while it is within its TTL, and re-resolves otherwise.
- Test: a table over well-within-TTL, exactly-at-the-boundary, and well-past-TTL, each asserting the exact lookup count and returned address; a further case asserting a failed lookup is never cached.
- Verify: `go test -count=1 ./...`.

```bash
mkdir -p ~/go-exercises/dns-cache-ttl-expiry-ignored/cmd/demo
cd ~/go-exercises/dns-cache-ttl-expiry-ignored
go mod init example.com/dns-cache-ttl-expiry-ignored
```

### Why storing the TTL is not the same as checking it

The version that shipped first stores every field the answer carries,
including the TTL — which makes the bug easy to miss on a code read,
since the TTL is right there in the struct — but never compares it
against anything:

```go
// BUG: an entry, once cached, is returned forever. The TTL is stored but
// never compared against how long the entry has actually been cached.
func (c *Cache) Resolve(host string) (Answer, error) {
	c.mu.Lock()
	e, ok := c.entries[host]
	c.mu.Unlock()

	if ok {
		return e.answer, nil
	}

	ans, err := c.lookup(host)
	if err != nil {
		return Answer{}, err
	}
	c.mu.Lock()
	c.entries[host] = entry{answer: ans, fetchedAt: c.now()}
	c.mu.Unlock()
	return ans, nil
}
```

`fetchedAt` is even recorded on every write, which makes this
particularly easy to skim past in review — it looks like the
ingredients for an expiry check are all present. They are just never
combined into one: the `if ok` branch returns immediately on any hit,
regardless of how long ago the entry was fetched relative to its own
`TTL`. In a short-lived integration test this looks completely correct,
because the whole test runs faster than any realistic TTL. The failure
only appears under the condition the cache exists to handle in the
first place — enough wall-clock time passing that the cached answer is
supposed to have expired, which a real clock and a short test run will
essentially never wait for. That is exactly why the fix, and the test
that pins it, inject the clock instead of calling `time.Now()`
directly: correctness here is entirely about a temporal boundary, and
a boundary can only be tested precisely by controlling time exactly,
not by hoping a `time.Sleep` in a test happens to straddle it.

```go
if ok && c.now().Sub(e.fetchedAt) < e.answer.TTL {
	return e.answer, nil
}
```

Create `resolver.go`:

```go
package resolver

import (
	"sync"
	"time"
)

// Answer is a resolved set of addresses along with how long they may be
// cached, mirroring a DNS response's TTL.
type Answer struct {
	Addrs []string
	TTL   time.Duration
}

// Lookup performs the actual (possibly expensive, possibly network-bound)
// resolution for host.
type Lookup func(host string) (Answer, error)

type entry struct {
	answer    Answer
	fetchedAt time.Time
}

// Cache is a TTL-aware DNS/service-discovery resolver cache. now is
// injected so tests and demos can control elapsed time exactly instead of
// depending on a real clock.
type Cache struct {
	mu      sync.Mutex
	lookup  Lookup
	entries map[string]entry
	now     func() time.Time
}

// NewCache creates a Cache backed by lookup, using now to read the current
// time.
func NewCache(lookup Lookup, now func() time.Time) *Cache {
	return &Cache{lookup: lookup, entries: make(map[string]entry), now: now}
}

// Resolve returns the cached answer for host if it is still within its
// TTL; otherwise it calls lookup, caches the fresh answer, and returns it.
func (c *Cache) Resolve(host string) (Answer, error) {
	c.mu.Lock()
	e, ok := c.entries[host]
	c.mu.Unlock()

	if ok && c.now().Sub(e.fetchedAt) < e.answer.TTL {
		return e.answer, nil
	}

	ans, err := c.lookup(host)
	if err != nil {
		return Answer{}, err
	}

	c.mu.Lock()
	c.entries[host] = entry{answer: ans, fetchedAt: c.now()}
	c.mu.Unlock()

	return ans, nil
}
```

### The runnable demo

A fake clock stands in for wall time: the demo resolves once, checks
again well within the 30-second TTL (cache hit, address unchanged),
then jumps the clock past the TTL right as the simulated backend
scales down to a new address.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/dns-cache-ttl-expiry-ignored"
)

func main() {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	now := func() time.Time { return clock }

	lookups := 0
	lookup := func(host string) (resolver.Answer, error) {
		lookups++
		if lookups == 1 {
			return resolver.Answer{Addrs: []string{"10.0.0.1"}, TTL: 30 * time.Second}, nil
		}
		return resolver.Answer{Addrs: []string{"10.0.0.9"}, TTL: 30 * time.Second}, nil
	}

	c := resolver.NewCache(lookup, now)

	a, _ := c.Resolve("payments.svc")
	fmt.Println("t=0s:", a.Addrs, "lookups so far:", lookups)

	clock = base.Add(10 * time.Second)
	a, _ = c.Resolve("payments.svc")
	fmt.Println("t=10s (within TTL):", a.Addrs, "lookups so far:", lookups)

	clock = base.Add(31 * time.Second)
	a, _ = c.Resolve("payments.svc")
	fmt.Println("t=31s (TTL expired, backend scaled down to a new address):", a.Addrs, "lookups so far:", lookups)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
t=0s: [10.0.0.1] lookups so far: 1
t=10s (within TTL): [10.0.0.1] lookups so far: 1
t=31s (TTL expired, backend scaled down to a new address): [10.0.0.9] lookups so far: 2
```

### Tests

`TestResolveTable` is a table over three points relative to the TTL
boundary — well within it, exactly at it, and well past it — each
asserting both the exact lookup count and the returned address, using
a fake clock advanced by an exact `time.Duration` rather than a real
sleep. `TestResolveDoesNotCacheOnLookupError` is a further case: a
failed lookup must never populate the cache with a zero-value answer,
or the next call would wrongly serve empty addresses instead of
retrying.

Create `resolver_test.go`:

```go
package resolver

import (
	"errors"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time         { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestResolveTable(t *testing.T) {
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		advance     time.Duration // clock advance before the second Resolve call
		wantLookups int
		wantAddr    string
	}{
		{
			name:        "well within TTL: cache hit, no second lookup",
			advance:     5 * time.Second,
			wantLookups: 1,
			wantAddr:    "10.0.0.1",
		},
		{
			name:        "exactly at TTL boundary: expired, re-resolves",
			advance:     30 * time.Second,
			wantLookups: 2,
			wantAddr:    "10.0.0.9",
		},
		{
			name:        "well past TTL: expired, re-resolves",
			advance:     60 * time.Second,
			wantLookups: 2,
			wantAddr:    "10.0.0.9",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			clock := &fakeClock{t: base}
			lookups := 0
			lookup := func(host string) (Answer, error) {
				lookups++
				if lookups == 1 {
					return Answer{Addrs: []string{"10.0.0.1"}, TTL: 30 * time.Second}, nil
				}
				return Answer{Addrs: []string{"10.0.0.9"}, TTL: 30 * time.Second}, nil
			}

			c := NewCache(lookup, clock.now)

			if _, err := c.Resolve("payments.svc"); err != nil {
				t.Fatalf("first Resolve() error = %v, want nil", err)
			}

			clock.advance(tc.advance)

			ans, err := c.Resolve("payments.svc")
			if err != nil {
				t.Fatalf("second Resolve() error = %v, want nil", err)
			}
			if lookups != tc.wantLookups {
				t.Fatalf("lookups = %d, want %d", lookups, tc.wantLookups)
			}
			if len(ans.Addrs) != 1 || ans.Addrs[0] != tc.wantAddr {
				t.Fatalf("addrs = %v, want [%s]", ans.Addrs, tc.wantAddr)
			}
		})
	}
}

func TestResolveDoesNotCacheOnLookupError(t *testing.T) {
	t.Parallel()

	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}

	errBoom := errors.New("dns: nxdomain")
	calls := 0
	lookup := func(host string) (Answer, error) {
		calls++
		if calls == 1 {
			return Answer{}, errBoom
		}
		return Answer{Addrs: []string{"10.0.0.5"}, TTL: 30 * time.Second}, nil
	}

	c := NewCache(lookup, clock.now)

	if _, err := c.Resolve("flaky.svc"); !errors.Is(err, errBoom) {
		t.Fatalf("first Resolve() error = %v, want %v", err, errBoom)
	}

	ans, err := c.Resolve("flaky.svc")
	if err != nil {
		t.Fatalf("second Resolve() error = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (a failed lookup must not be cached)", calls)
	}
	if len(ans.Addrs) != 1 || ans.Addrs[0] != "10.0.0.5" {
		t.Fatalf("addrs = %v, want [10.0.0.5]", ans.Addrs)
	}
}
```

Run: `go test -count=1 ./...`.

## Review

`Resolve` is correct when a cached answer is returned only while
strictly less time has elapsed than its own `TTL`, and a fresh lookup
happens at the boundary and beyond — proven by advancing an injected
clock to exact points relative to that boundary, not by racing a real
timer. The mistake this design avoids is storing every field an answer
carries, including the one meant to bound its lifetime, without ever
reading that field back: `fetchedAt` sitting right next to `TTL` in the
struct makes the omission easy to miss on a casual read, because the
data needed for the check is right there — it is simply never
compared against anything. The fix is a single boolean expression,
`ok && now().Sub(fetchedAt) < TTL`, but it is the entire difference
between a cache that ages entries out as designed and one that quietly
never expires anything at all.

## Resources

- [RFC 1035, Domain Names — Implementation and Specification](https://www.rfc-editor.org/rfc/rfc1035) — the TTL field's meaning as a cache lifetime, not a display hint.
- [time.Time.Sub](https://pkg.go.dev/time#Time.Sub) — computing elapsed duration for a TTL comparison.
- [Kubernetes DNS-based service discovery](https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/) — why clients that cache resolved addresses past their TTL keep routing to endpoints removed during a scale-down.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-bloom-filter-inverted-assumption.md](28-bloom-filter-inverted-assumption.md) | Next: [30-lease-renewal-expiry-off-by-one.md](30-lease-renewal-expiry-off-by-one.md)
