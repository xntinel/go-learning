# Exercise 6: Per-API-Key Rate-Limit Middleware with Stale-Entry Eviction

In production, nobody rate-limits a service with one global bucket — you
limit *per client*, keyed by API key, so one noisy tenant cannot starve the
rest. That means a registry: a mutex-protected map of key to limiter, an
HTTP middleware returning 429 with `Retry-After`, and — the part everyone
forgets until the OOM page — a janitor that evicts entries for keys that
stopped calling. This is the mutex side of the proverb in its natural
habitat: shared registry state that never transfers ownership.

## What you'll build

```text
ratemw/                         independent module: example.com/ratemw
  go.mod
  ratemw/
    ratemw.go                   bucket (per-client token bucket), Registry
                                (map[string]*client + lastSeen), Middleware,
                                Sweep, Janitor with stop func
    ratemw_test.go              httptest key isolation, 401, exact-burst storm,
                                fake-clock eviction, janitor lifecycle, Example
  cmd/
    demo/
      main.go                   scripted tenant traffic on a manual clock
```

- Files: `ratemw/ratemw.go`, `ratemw/ratemw_test.go`, `cmd/demo/main.go`.
- Implement: a `Registry` with per-key buckets and `lastSeen` stamps, an `http.Handler` middleware (401 on missing key, 429 + `Retry-After` on deny), `Sweep` for TTL eviction, and a ticker `Janitor` with an idempotent stop.
- Test: same key exhausts its bucket to 429 while another key gets 200; 20 concurrent goroutines on one key admit exactly the burst; an injected clock drives eviction; the janitor stops cleanly.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/10-mutex-vs-channel/06-per-client-limiter-registry/ratemw go-solutions/15-sync-primitives/10-mutex-vs-channel/06-per-client-limiter-registry/cmd/demo
cd go-solutions/15-sync-primitives/10-mutex-vs-channel/06-per-client-limiter-registry
```

### Two-level locking: registry mutex, then bucket mutex

The registry holds `map[string]*client` under its own mutex; each client
holds a token bucket under *its* own mutex. The layering rule that keeps
this deadlock-free and fast: the registry lock is held only long enough to
find-or-create the entry and stamp `lastSeen`, never across the bucket's
token arithmetic. Lookup is a short critical section on the shared map;
admission is a short critical section on per-client state. Two tenants
hammering different keys contend only on the brief map lookup, not on each
other's buckets. Collapsing both into one lock (a single mutex around a
`map[string]float64` of token counts) works at small scale but serializes
every tenant's admission through one lock — the layered design is the same
number of lines and scales with tenant count.

Find-or-create must be a single critical section too. The tempting
"check with RLock, upgrade to Lock to insert" dance has a classic
check-then-act gap where two first requests for the same key both insert,
and one bucket (with its burst already partially spent) is silently
dropped. One plain mutex, one `if !ok { create }` under it, is correct and
— for a map operation this cheap — not the bottleneck.

### Retry-After is part of the contract

A bare 429 tells a well-behaved client nothing useful; RFC 6585 defines the
status and HTTP semantics give it the `Retry-After` header so the client can
back off *precisely* instead of hammering. The bucket computes it from its
own arithmetic: the deficit is `1 - tokens` and refill runs at `rate` per
second, so the wait is `(1-tokens)/rate` seconds, rounded up to a whole
second (the header takes integer seconds; rounding down would invite a
guaranteed second 429). Middleware that returns 429 without `Retry-After`
turns every polite client into an impolite one.

### The eviction problem: per-client maps are memory leaks by default

Every entry in the registry lives forever unless something deletes it. Under
real key churn — customers rotating credentials, or an attacker cycling
random keys precisely because each new key gets a fresh burst — the map
grows without bound. The fix is boring and mandatory: stamp `lastSeen` on
every lookup, and periodically `Sweep` entries idle longer than a TTL. The
sweep is a straight `for key, c := range` with `delete(r.clients, key)` —
deleting from a Go map during range iteration is explicitly allowed by the
spec. The `Janitor` wraps `Sweep` in a ticker goroutine, and because it
*starts a goroutine* it ships the Exercise 2 lifecycle kit: a stop function
that is idempotent (`sync.Once`) and that waits for the goroutine's `done`
before returning, so shutdown is observable, not hopeful.

The clock is injected exactly as in Exercise 1, which is what lets the
eviction test say "advance 10 minutes" instead of sleeping 10 minutes.

Create `ratemw/ratemw.go`:

```go
// Package ratemw provides per-API-key rate-limit middleware backed by a
// registry of token buckets with TTL eviction of idle clients.
package ratemw

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// bucket is a continuous-refill token bucket for one client.
type bucket struct {
	mu         sync.Mutex
	now        func() time.Time
	tokens     float64
	maxTokens  float64
	refillRate float64 // tokens per second; 0 disables refill
	lastRefill time.Time
}

// allow spends one token if available; otherwise it reports how long until
// the next token exists, for the Retry-After header.
func (b *bucket) allow() (ok bool, retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.tokens += now.Sub(b.lastRefill).Seconds() * b.refillRate
	if b.tokens > b.maxTokens {
		b.tokens = b.maxTokens
	}
	b.lastRefill = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	if b.refillRate <= 0 {
		return false, time.Hour // no refill: tell the client to go away
	}
	deficit := 1 - b.tokens
	return false, time.Duration(deficit / b.refillRate * float64(time.Second))
}

type client struct {
	b        *bucket
	lastSeen time.Time
}

// Registry tracks one bucket per API key plus a lastSeen stamp for eviction.
type Registry struct {
	mu      sync.Mutex
	now     func() time.Time
	clients map[string]*client
	burst   float64
	rate    float64
	idleTTL time.Duration
}

// NewRegistry creates a registry: each new key gets a bucket holding burst
// tokens refilling at ratePerSec; keys idle longer than idleTTL are evicted
// by Sweep.
func NewRegistry(burst, ratePerSec float64, idleTTL time.Duration) *Registry {
	return NewRegistryWithClock(burst, ratePerSec, idleTTL, time.Now)
}

// NewRegistryWithClock is NewRegistry with an injectable clock for tests,
// demos, and simulation.
func NewRegistryWithClock(burst, ratePerSec float64, idleTTL time.Duration, now func() time.Time) *Registry {
	return &Registry{
		now:     now,
		clients: make(map[string]*client),
		burst:   burst,
		rate:    ratePerSec,
		idleTTL: idleTTL,
	}
}

// lookup finds or creates the key's bucket and stamps lastSeen. Find-or-create
// is one critical section: no check-then-act gap can double-create a bucket.
func (r *Registry) lookup(key string) *bucket {
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.clients[key]
	if !ok {
		c = &client{b: &bucket{
			now:        r.now,
			tokens:     r.burst,
			maxTokens:  r.burst,
			refillRate: r.rate,
			lastRefill: r.now(),
		}}
		r.clients[key] = c
	}
	c.lastSeen = r.now()
	return c.b
}

// Len reports how many clients are currently tracked.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.clients)
}

// Sweep evicts clients idle longer than the TTL and returns how many were
// removed. Deleting from a map during range is allowed by the language spec.
func (r *Registry) Sweep() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := r.now().Add(-r.idleTTL)
	evicted := 0
	for key, c := range r.clients {
		if c.lastSeen.Before(cutoff) {
			delete(r.clients, key)
			evicted++
		}
	}
	return evicted
}

// Janitor runs Sweep every interval until the returned stop func is called.
// stop is idempotent and blocks until the janitor goroutine has exited.
func (r *Registry) Janitor(interval time.Duration) (stop func()) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				r.Sweep()
			case <-stopCh:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stopCh) })
		<-done
	}
}

// Middleware enforces the per-key limit: 401 for a missing key, 429 with a
// Retry-After header when the key's bucket is empty.
func (r *Registry) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := req.Header.Get("X-API-Key")
		if key == "" {
			http.Error(w, "missing X-API-Key", http.StatusUnauthorized)
			return
		}
		ok, retryAfter := r.lookup(key).allow()
		if !ok {
			secs := int(math.Ceil(retryAfter.Seconds()))
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, req)
	})
}
```

### The demo: a scripted multi-tenant day on a manual clock

Injected clock again, so the demo scripts an exact scenario: alice burns her
burst and gets a 429 with a precise `Retry-After: 1`, one virtual second
restores her, bob shows key isolation, and a ten-minute idle gap lets the
sweep reclaim both entries.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/ratemw/ratemw"
)

type clock struct {
	t time.Time
}

func (c *clock) Now() time.Time { return c.t }

func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func get(h http.Handler, key string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/orders", nil)
	req.Header.Set("X-API-Key", key)
	h.ServeHTTP(rec, req)
	return rec
}

func main() {
	clk := &clock{t: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	reg := ratemw.NewRegistryWithClock(2, 1, 5*time.Minute, clk.Now)
	h := reg.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	fmt.Println("alice #1:", get(h, "alice").Code)
	fmt.Println("alice #2:", get(h, "alice").Code)
	rec := get(h, "alice")
	fmt.Printf("alice #3: %d Retry-After=%s\n", rec.Code, rec.Header().Get("Retry-After"))

	clk.advance(time.Second)
	fmt.Println("1s later, alice:", get(h, "alice").Code)
	fmt.Println("bob (fresh key):", get(h, "bob").Code)
	fmt.Println("tracked clients:", reg.Len())

	clk.advance(10 * time.Minute)
	fmt.Printf("10m idle: swept=%d tracked=%d\n", reg.Sweep(), reg.Len())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alice #1: 200
alice #2: 200
alice #3: 429 Retry-After=1
1s later, alice: 200
bob (fresh key): 200
tracked clients: 2
10m idle: swept=2 tracked=0
```

### Tests

`httptest.NewRequest` plus `httptest.NewRecorder` exercise the middleware as
HTTP without a socket. The storm test is the registry's exact-N proof: 20
goroutines share one key with refill off, and across 100 total requests
exactly `burst` may see 200 — this catches both bucket races and the
double-create bug in `lookup` (a duplicated bucket would admit up to twice
the burst). The janitor test polls `Len` with a deadline (the ticker is
real; the idleness is virtual) and then proves `stop` is idempotent and
blocking.

Create `ratemw/ratemw_test.go`:

```go
package ratemw

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func do(h http.Handler, key string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	h.ServeHTTP(rec, req)
	return rec
}

func TestKeyIsolation(t *testing.T) {
	t.Parallel()

	reg := NewRegistryWithClock(2, 0, time.Minute, newFakeClock().Now)
	h := reg.Middleware(okHandler())

	for i := range 2 {
		if got := do(h, "tenant-a").Code; got != http.StatusOK {
			t.Fatalf("tenant-a request #%d: code = %d, want 200", i, got)
		}
	}
	rec := do(h, "tenant-a")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("tenant-a over burst: code = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After header")
	}
	// A different key has its own untouched bucket.
	if got := do(h, "tenant-b").Code; got != http.StatusOK {
		t.Fatalf("tenant-b: code = %d, want 200 (buckets must be per key)", got)
	}
}

func TestMissingKeyIsUnauthorized(t *testing.T) {
	t.Parallel()

	reg := NewRegistryWithClock(1, 0, time.Minute, newFakeClock().Now)
	if got := do(reg.Middleware(okHandler()), "").Code; got != http.StatusUnauthorized {
		t.Fatalf("missing key: code = %d, want 401", got)
	}
}

func TestConcurrentSameKeyExactBurst(t *testing.T) {
	t.Parallel()

	const burst = 20
	reg := NewRegistryWithClock(burst, 0, time.Minute, newFakeClock().Now)
	h := reg.Middleware(okHandler())

	var ok200 atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 5 {
				if do(h, "hot-tenant").Code == http.StatusOK {
					ok200.Add(1)
				}
			}
		})
	}
	wg.Wait()

	// Exactly burst admissions across 100 concurrent requests: catches
	// bucket races and the lookup double-create bug alike.
	if got := ok200.Load(); got != burst {
		t.Fatalf("admitted = %d, want exactly %d", got, burst)
	}
	if got := reg.Len(); got != 1 {
		t.Fatalf("tracked clients = %d, want 1 (no duplicate entries)", got)
	}
}

func TestSweepEvictsIdleClients(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	reg := NewRegistryWithClock(5, 0, time.Minute, clk.Now)
	h := reg.Middleware(okHandler())

	do(h, "old-1")
	do(h, "old-2")
	clk.Advance(2 * time.Minute) // both go idle past the TTL
	do(h, "fresh")

	if evicted := reg.Sweep(); evicted != 2 {
		t.Fatalf("Sweep evicted %d, want 2", evicted)
	}
	if got := reg.Len(); got != 1 {
		t.Fatalf("Len after sweep = %d, want 1 (only the fresh client)", got)
	}
}

func TestJanitorSweepsAndStops(t *testing.T) {
	t.Parallel()

	clk := newFakeClock()
	reg := NewRegistryWithClock(5, 0, time.Minute, clk.Now)
	h := reg.Middleware(okHandler())

	do(h, "ghost")
	clk.Advance(time.Hour) // idle far past the TTL

	stop := reg.Janitor(5 * time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for reg.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := reg.Len(); got != 0 {
		t.Fatalf("janitor never evicted: Len = %d, want 0", got)
	}

	stop()
	stop() // idempotent; both calls return only after the goroutine exited
}

func ExampleRegistry_Middleware() {
	reg := NewRegistry(1, 0, time.Minute)
	h := reg.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for range 2 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", "k")
		h.ServeHTTP(rec, req)
		fmt.Println(rec.Code)
	}
	// Output:
	// 200
	// 429
}
```

## Review

The bugs this artifact is prone to are all in the seams. A find-or-create
that releases the registry lock between the check and the insert
double-creates buckets, and the storm test's exact-burst assertion plus its
`Len() == 1` check will expose it. A registry without `Sweep` passes every
functional test and leaks memory in production — the eviction test only
exists because the fake clock makes "two minutes of idleness" free. A
janitor whose stop function does not wait on `done` returns while the
goroutine still runs, turning "stopped" into a lie that surfaces as a racy
sweep during shutdown. And a 429 without `Retry-After` is functionally
correct and operationally hostile.

Confirm with `go test -count=1 -race ./...` — the storm drives 100 real
`ServeHTTP` calls through both lock levels concurrently — and `go run
./cmd/demo` for the deterministic scripted day. Everything here composes
with the previous modules: swap the hand-rolled `bucket` for the Exercise 8
adapter and nothing above `lookup` changes.

## Resources

- [RFC 6585: 429 Too Many Requests](https://www.rfc-editor.org/rfc/rfc6585#section-4) — the status code's definition, including Retry-After.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder`, the socket-free middleware test kit.
- [The Go Programming Language Specification: For statements](https://go.dev/ref/spec#For_range) — deletion during map range is defined behavior.
- [net/http: Handler](https://pkg.go.dev/net/http#Handler) — the middleware contract.

---

Prev: [05-concurrency-contract-tests.md](05-concurrency-contract-tests.md) | Back to [00-concepts.md](00-concepts.md) | Next: [07-context-aware-wait.md](07-context-aware-wait.md)
