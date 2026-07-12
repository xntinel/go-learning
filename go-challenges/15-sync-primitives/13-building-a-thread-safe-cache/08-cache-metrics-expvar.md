# Exercise 8: Cache Metrics Through expvar — Observability Without a Metrics Stack

A cache you cannot observe is a cache you cannot tune: is the hit ratio 95%
or 40%? Is the TTL evicting everything before anyone reads it? This exercise
instruments a TTL cache with the standard library's `expvar` package —
counters for hits, misses, and expiries plus derived gauges — so any service
that mounts `expvar.Handler()` exposes cache health as JSON at `/debug/vars`
with zero external dependencies.

## What you'll build

```text
metriccache/                     independent module: example.com/metriccache
  go.mod
  cache/
    cache.go                     Cache[V]: New(namespace, ttl), Set, Get (lazy expiry),
                                 Metrics {Hits, Misses, Expired *expvar.Int},
                                 expvar.Func gauges (entries, hit_ratio), Snapshot
    cache_test.go                counter accounting via fake clock; /debug/vars JSON
                                 decoded through httptest; 50-goroutine race test
  cmd/
    demo/
      main.go                    hit, miss, and TTL-expiry paths, then the Snapshot line
```

- Files: `cache/cache.go`, `cache/cache_test.go`, `cmd/demo/main.go`.
- Implement: a mutex-guarded TTL cache whose `Get` classifies every lookup as hit, miss, or expired-miss and bumps the matching `expvar.Int`; namespaced registration; `expvar.Func` gauges for live entry count and hit ratio.
- Test: exact counter accounting driven by an injected clock; the real `/debug/vars` payload decoded via `httptest` + `expvar.Handler()`; concurrent Set/Get proving counts stay exact under `-race`.
- Verify: `go test -count=1 -race ./...`

### Counters live outside the lock's story

`expvar.Int` is internally atomic — `Add` and `Value` are safe from any
goroutine — so the metrics do not need the cache's mutex. What *does* need
care is where the counting happens: the classification of a lookup (hit,
miss, or expired) is only meaningful under the mutex, because between
unlocking and counting another goroutine could delete or refresh the entry.
The rule this module follows: decide and count while holding the lock, since
`Add` is cheap and never blocks; keep anything slow (loaders, network) out.

Registration is the sharp edge of `expvar`. Its registry is a process-global
map and `expvar.NewInt` **panics** on a duplicate name — a deliberate design
choice, since two owners silently sharing a counter is worse than a crash at
startup. The constructor therefore takes a namespace (`"profile_cache"`,
`"session_cache"`) and prefixes every variable, turning the panic into a
loud, immediate signal that two caches collided. Tests must respect the same
rule: each test registers its own namespace because the registry cannot be
reset between tests.

The two derived variables use `expvar.Func`, which evaluates a closure at
scrape time. `entries` reports `len(c.items)` under the mutex — a gauge, a
point-in-time reading that can go down, unlike the monotonically increasing
counters. `hit_ratio` divides hits by lookups without any lock at all: it
reads two atomics that may be a beat apart, and that is fine — a ratio
scraped for dashboards does not need a consistent snapshot, and taking the
mutex during every scrape would make observability itself a source of
contention. That trade-off — approximate-but-cheap derived metrics — is the
norm in production instrumentation.

Expiry counting is tied to the lazy-deletion design: an expired entry is
discovered, deleted, and counted by the `Get` that trips over it, and the
same lookup also counts as a miss (the caller did not get a value). Keeping
`expired <= misses` as an invariant makes the numbers composable: `misses`
alone sizes the load your backing store sees, while `expired` tells you how
much of it the TTL caused.

Create `cache/cache.go`:

```go
// Package cache implements a TTL cache whose hit, miss, and expiry
// counters are published through expvar, so any process that mounts
// expvar.Handler() exposes cache health at /debug/vars for free.
package cache

import (
	"expvar"
	"fmt"
	"sync"
	"time"
)

type entry[V any] struct {
	value     V
	expiresAt time.Time
}

// Metrics holds the expvar-published counters for one cache instance.
// Registration happens once per namespace; expvar panics on duplicate
// names, so two caches must not share a namespace.
type Metrics struct {
	Hits    *expvar.Int
	Misses  *expvar.Int
	Expired *expvar.Int
}

// Cache is a mutex-guarded TTL cache with lazy expiration: an expired
// entry is evicted (and counted) by the Get that discovers it.
type Cache[V any] struct {
	mu    sync.Mutex
	items map[string]entry[V]
	ttl   time.Duration
	m     Metrics

	now func() time.Time // injected for deterministic tests
}

// New builds a Cache whose counters are published under
// "<namespace>.hits", "<namespace>.misses", "<namespace>.expired",
// plus an "<namespace>.entries" gauge and a "<namespace>.hit_ratio"
// derived variable. It panics if namespace was already registered —
// pick one name per cache per process.
func New[V any](namespace string, ttl time.Duration) *Cache[V] {
	c := &Cache[V]{
		items: make(map[string]entry[V]),
		ttl:   ttl,
		m: Metrics{
			Hits:    expvar.NewInt(namespace + ".hits"),
			Misses:  expvar.NewInt(namespace + ".misses"),
			Expired: expvar.NewInt(namespace + ".expired"),
		},
		now: time.Now,
	}
	expvar.Publish(namespace+".entries", expvar.Func(func() any {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.items)
	}))
	expvar.Publish(namespace+".hit_ratio", expvar.Func(func() any {
		h, m := c.m.Hits.Value(), c.m.Misses.Value()
		if h+m == 0 {
			return 0.0
		}
		return float64(h) / float64(h+m)
	}))
	return c
}

// Set stores value under key for the cache's TTL.
func (c *Cache[V]) Set(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = entry[V]{value: value, expiresAt: c.now().Add(c.ttl)}
}

// Get returns the live value for key. A missing key counts a miss; an
// expired key is deleted, counting both an expiry and a miss.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.items[key]
	if !ok {
		c.m.Misses.Add(1)
		var zero V
		return zero, false
	}
	if c.now().After(e.expiresAt) {
		delete(c.items, key)
		c.m.Expired.Add(1)
		c.m.Misses.Add(1)
		var zero V
		return zero, false
	}
	c.m.Hits.Add(1)
	return e.value, true
}

// Snapshot renders the counters as a stable one-line summary, handy
// for logs and demos without scraping /debug/vars.
func (c *Cache[V]) Snapshot() string {
	c.mu.Lock()
	n := len(c.items)
	c.mu.Unlock()
	return fmt.Sprintf("hits=%d misses=%d expired=%d entries=%d",
		c.m.Hits.Value(), c.m.Misses.Value(), c.m.Expired.Value(), n)
}
```

### The demo

Exercise all three lookup outcomes — a hit, a cold miss, and a TTL expiry —
then print the counter summary.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/metriccache/cache"
)

func main() {
	c := cache.New[string]("profile_cache", 50*time.Millisecond)

	c.Set("user:1", "alice")
	c.Set("user:2", "bob")

	if v, ok := c.Get("user:1"); ok {
		fmt.Println("hit:", v)
	}
	if _, ok := c.Get("user:9"); !ok {
		fmt.Println("miss: user:9")
	}

	time.Sleep(60 * time.Millisecond) // let both entries pass their TTL

	if _, ok := c.Get("user:2"); !ok {
		fmt.Println("expired: user:2")
	}

	fmt.Println(c.Snapshot())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hit: alice
miss: user:9
expired: user:2
hits=1 misses=2 expired=1 entries=1
```

The final line shows `entries=1`: `user:2` was evicted by the expired `Get`,
but `user:1` — equally past its TTL — is still in the map because nothing has
looked it up since. Lazy expiration means the entry count includes the
not-yet-collected dead; that is the design, not a bug.

### Tests

The accounting test drives an injected clock through the hit, miss, and
expiry paths and asserts exact counter values. The endpoint test is the one
worth copying into real services: it does not trust the counters' getters
but scrapes the actual `/debug/vars` JSON through `expvar.Handler()` and
`httptest`, decoding into `map[string]json.RawMessage` so each variable can
be compared as raw text — proving the wiring end to end, names included.
The race test hammers `Set`/`Get` from 50 goroutines and then checks a
conservation law: every `Get` counted exactly once, so `hits+misses` equals
the number of lookups.

Create `cache/cache_test.go`:

```go
package cache

import (
	"encoding/json"
	"expvar"
	"fmt"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Each test registers its own namespace: expvar's registry is global
// and panics on duplicates, so names must be unique per process.

func newTestCache(t *testing.T, ns string) (*Cache[string], func(time.Duration)) {
	t.Helper()
	c := New[string](ns, time.Minute)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	offset := time.Duration(0)
	c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return base.Add(offset)
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		offset += d
	}
	return c, advance
}

func TestCountersTrackHitsMissesExpiry(t *testing.T) {
	t.Parallel()

	c, advance := newTestCache(t, "test_counters")

	if _, ok := c.Get("absent"); ok {
		t.Fatal("Get(absent) = ok, want miss")
	}
	c.Set("k", "v")
	if v, ok := c.Get("k"); !ok || v != "v" {
		t.Fatalf("Get(k) = %q, %v; want v, true", v, ok)
	}
	advance(2 * time.Minute)
	if _, ok := c.Get("k"); ok {
		t.Fatal("Get(k) after TTL = ok, want expired miss")
	}

	tests := []struct {
		name string
		v    *expvar.Int
		want int64
	}{
		{"hits", c.m.Hits, 1},
		{"misses", c.m.Misses, 2},
		{"expired", c.m.Expired, 1},
	}
	for _, tt := range tests {
		if got := tt.v.Value(); got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestExpvarEndpointExposesCounters(t *testing.T) {
	t.Parallel()

	c, _ := newTestCache(t, "test_endpoint")
	c.Set("k", "v")
	c.Get("k")
	c.Get("nope")

	rec := httptest.NewRecorder()
	expvar.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/debug/vars", nil))

	var vars map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &vars); err != nil {
		t.Fatalf("decoding /debug/vars: %v", err)
	}
	for name, want := range map[string]string{
		"test_endpoint.hits":      "1",
		"test_endpoint.misses":    "1",
		"test_endpoint.entries":   "1",
		"test_endpoint.hit_ratio": "0.5",
	} {
		got, ok := vars[name]
		if !ok {
			t.Fatalf("/debug/vars missing %q", name)
		}
		if string(got) != want {
			t.Errorf("%s = %s, want %s", name, got, want)
		}
	}
}

func TestConcurrentAccessUnderRace(t *testing.T) {
	t.Parallel()

	c, _ := newTestCache(t, "test_race")
	c.Set("shared", "v")

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Set(fmt.Sprintf("k%d", i%10), "v")
			c.Get("shared")
			c.Get(fmt.Sprintf("k%d", i%10))
		}()
	}
	wg.Wait()

	total := c.m.Hits.Value() + c.m.Misses.Value()
	if total != 100 {
		t.Fatalf("hits+misses = %d, want 100 (every Get counted exactly once)", total)
	}
}

func ExampleCache_Snapshot() {
	c := New[int]("example_orders", time.Minute)
	c.Set("order:1", 99)
	c.Get("order:1")
	c.Get("order:2")
	fmt.Println(c.Snapshot())
	// Output: hits=1 misses=1 expired=0 entries=1
}
```

Run the gate:

```bash
gofmt -l . && go vet ./... && go test -count=1 -race ./...
```

## Review

The module's real lesson is the boundary between synchronization and
observability. Classification happens under the mutex because "was this a
hit?" is only answerable while the entry cannot change; the counters
themselves are atomics so scrapes never touch the lock; and the one derived
metric that must be exact (`entries`) takes the mutex while the one that
tolerates skew (`hit_ratio`) does not. When wiring this into a service,
mount the endpoint explicitly with `mux.Handle("/debug/vars",
expvar.Handler())` — importing `expvar` for side effects registers it only
on `http.DefaultServeMux`, which production services usually avoid, and
whichever way you mount it the endpoint belongs on an internal listener,
not the public one, since `/debug/vars` also exposes `cmdline` and full
`memstats`. The duplicate-name panic will surface the first time two
components pick the same namespace; treat it as the startup-time contract
check it is, not something to recover from.

## Resources

- [`expvar` package](https://pkg.go.dev/expvar) — `Int`, `Func`, `Publish`, `Handler`, and the duplicate-registration panic.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder`/`NewRequest`, used to scrape the endpoint in tests.
- [Prometheus: metric types](https://prometheus.io/docs/concepts/metric_types/) — the counter-versus-gauge vocabulary this module's variables map onto.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-stale-while-revalidate.md](07-stale-while-revalidate.md) | Next: [../14-contention-profiling/00-concepts.md](../14-contention-profiling/00-concepts.md)
