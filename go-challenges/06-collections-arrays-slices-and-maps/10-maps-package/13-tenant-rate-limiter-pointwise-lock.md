# Exercise 13: Per-Tenant Rate Limiter: Point Lock, Not Whole-Map Clone

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

An API gateway's per-tenant token-bucket limiter is the Envoy local-rate-limit
and quota-manager shape: every incoming request calls `Allow` for its tenant on
the hot path, thousands of times a second, spread across every tenant sharing
the gateway. `00-concepts.md`'s warning that "none of it is synchronized" cuts
the other way here than it did for the read-heavy registry in an earlier
module: a rate limiter's map is a write-heavy, high-cardinality structure, one
mutation per request, and the naive way to make it goroutine-safe -- clone the
whole thing under a lock before touching any tenant -- is the exact pattern
that turns a limiter meant to keep tenants independent into a bottleneck that
makes every tenant wait on every other tenant's requests.

`maps.Clone` under a `sync.Mutex` before mutating one entry is a completely
correct way to make a map safe for concurrent use. It is also, for this
specific workload, the wrong correct answer: the clone's cost is proportional
to the number of tenants the limiter tracks, not to the one tenant a given
request is about, and the lock guarding the clone is held for that entire
copy, which means every other tenant's `Allow` call queues up behind it. A
gateway with ten thousand tenants pays for copying all ten thousand bucket
pointers on every single request, to look at one. The fix is not a different
data structure; it is locking at the right granularity: one mutex per tenant
bucket, and a much smaller lock around the tenant map itself that is held only
long enough to find or create that one bucket.

This module builds `quota`, a `Limiter` whose `Allow` locks only the tenant it
is deciding about, whose `Snapshot` is the one place a full `maps.Clone` is
the right call because reporting is not the hot path, and whose tests prove
the allocation gap with `testing.AllocsPerRun` and the safety with `-race`.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
quota/                     module example.com/quota
  go.mod                   go 1.24
  quota.go                 bucket, Limiter; New, Allow, Refill, Snapshot
  quota_test.go            Allow/Refill/Snapshot tables, allowViaFullClone
                          contrast, concurrent-tenants race test, Example
```

- Files: `quota.go`, `quota_test.go`.
- Implement: `type Limiter struct{...}`; `New(rate, burst int) *Limiter`; `(*Limiter) Allow(tenant string) bool` locking only the one bucket; `(*Limiter) Refill()` restoring `rate` tokens per tenant, capped at `burst`; `(*Limiter) Snapshot() map[string]int` -- the one call site where a full `maps.Clone` is the correct choice.
- Test: burst exhaustion and independence across tenants; a zero-burst limiter that always denies; refill capped at burst and a no-op at rate zero; `Snapshot` returning an independent map; the `allowViaFullClone` contrast proven correct-but-costlier via `testing.AllocsPerRun`; fifty tenants hammering `Allow`/`Refill`/`Snapshot` concurrently under `-race`; `ExampleLimiter_Allow` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/quota
cd ~/go-exercises/quota
go mod init example.com/quota
go mod edit -go=1.24
```

### One mutex per bucket, not one mutex for the map

The version that looks safest at a glance is also the one that serializes
every tenant behind every other tenant:

```go
func allowViaFullClone(l *Limiter, tenant string) bool {
    l.mu.Lock()
    cloned := maps.Clone(l.buckets)   // copies every tenant's bucket pointer
    l.mu.Unlock()

    b := cloned[tenant]
    b.mu.Lock()
    defer b.mu.Unlock()
    if b.tokens <= 0 {
        return false
    }
    b.tokens--
    return true
}
```

This is not incorrect. `maps.Clone` of a `map[string]*bucket` copies the
pointers, not the buckets, so the entry `allowViaFullClone` ends up
decrementing is still the real, shared `*bucket` -- the token count is
accounted correctly. What is wrong is the cost: the clone visits every key in
`l.buckets` regardless of which single tenant the request is about, and the
lock guarding `l.buckets` is held for that entire O(n) copy. A tenant with a
completely empty queue still has to wait for the gateway to finish cloning
ten thousand pointers on someone else's behalf before its own `Allow` call
can even start.

The fix is granularity, not correctness: give every bucket its own `sync.Mutex`,
and let `Limiter`'s own lock guard only the *map's structure* -- inserting a
new tenant's bucket the first time it is seen -- never the token count inside
any bucket:

```go
func (l *Limiter) bucketFor(tenant string) *bucket {
    l.mu.RLock()
    b, ok := l.buckets[tenant]
    l.mu.RUnlock()
    if ok {
        return b
    }
    // slow path: create it under the write lock, checked again below
}
```

Once `bucketFor` returns, `Allow` never touches `l.mu` again -- it locks only
the `*bucket` it was handed, an operation whose cost is the same whether the
limiter tracks ten tenants or ten million.

Create `quota.go`:

```go
// Package quota implements a per-tenant token-bucket rate limiter, the
// Envoy local-rate-limit / API-gateway quota-manager shape: every incoming
// request calls Allow for its tenant on the hot path, thousands of times a
// second, across every tenant sharing the gateway.
package quota

import (
	"maps"
	"sync"
)

// bucket is one tenant's token count and the mutex that protects it. Every
// Allow call for a given tenant locks only this mutex, never the Limiter's
// own lock, so tenants never wait on each other.
type bucket struct {
	mu     sync.Mutex
	tokens int
}

// Limiter is a per-tenant token-bucket rate limiter.
//
// Limiter is safe for concurrent use by multiple goroutines. Its own mutex
// guards only the tenant-to-bucket map's structure (creating a bucket the
// first time a tenant is seen); the token count inside each bucket is
// guarded by that bucket's own mutex, acquired only while that one tenant's
// request is being decided. Two goroutines calling Allow for two different
// tenants never contend for the same lock.
type Limiter struct {
	rate, burst int
	mu          sync.RWMutex
	buckets     map[string]*bucket
}

// New returns a Limiter that grants each tenant up to burst tokens and
// restores rate tokens per call to Refill, capped at burst.
//
// A non-positive rate or burst is not an error: it clamps to zero, which
// means a burst of zero denies every request for every tenant (a valid,
// if extreme, "hard block" configuration) and a rate of zero means a
// depleted bucket never recovers until the caller reconfigures the Limiter.
func New(rate, burst int) *Limiter {
	if rate < 0 {
		rate = 0
	}
	if burst < 0 {
		burst = 0
	}
	return &Limiter{rate: rate, burst: burst, buckets: make(map[string]*bucket)}
}

// bucketFor returns tenant's bucket, creating one seeded at full burst the
// first time tenant is seen. The Limiter's lock is held only long enough to
// look up or insert the *bucket pointer, an O(1) operation independent of
// how many tenants the Limiter already tracks.
func (l *Limiter) bucketFor(tenant string) *bucket {
	l.mu.RLock()
	b, ok := l.buckets[tenant]
	l.mu.RUnlock()
	if ok {
		return b
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok = l.buckets[tenant]; ok { // lost the race to create it; use the winner's bucket
		return b
	}
	b = &bucket{tokens: l.burst}
	l.buckets[tenant] = b
	return b
}

// Allow reports whether tenant may proceed, consuming one token if so.
//
// Allow locks only tenant's own bucket, never the whole tenant map and
// never every other tenant's bucket: its cost does not grow with the
// number of tenants the Limiter is tracking. A tenant seen for the first
// time starts with a full burst of tokens.
func (l *Limiter) Allow(tenant string) bool {
	b := l.bucketFor(tenant)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// Refill restores rate tokens to every tenant currently tracked, capped at
// burst. It never invents new tenants and performs no I/O or timing of its
// own; the caller decides when a refill tick happens (a time.Ticker driven
// from outside this package, or a test calling Refill directly), which is
// what keeps Limiter's own logic free of time.Now.
func (l *Limiter) Refill() {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, b := range l.buckets {
		b.mu.Lock()
		if b.tokens += l.rate; b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.mu.Unlock()
	}
}

// Snapshot returns each tracked tenant's current token count, for
// reporting or metrics. Unlike Allow, Snapshot legitimately pays for a full
// maps.Clone: it is not on the request hot path, and cloning the pointer
// map first lets it read every bucket's token count without holding the
// Limiter's own lock while it does so.
//
// The returned map is a fresh allocation; mutating it does not affect the
// Limiter.
func (l *Limiter) Snapshot() map[string]int {
	l.mu.RLock()
	cloned := maps.Clone(l.buckets)
	l.mu.RUnlock()

	out := make(map[string]int, len(cloned))
	for tenant, b := range cloned {
		b.mu.Lock()
		out[tenant] = b.tokens
		b.mu.Unlock()
	}
	return out
}
```

### Using it

Construct one `Limiter` at startup with `New(rate, burst)` and share it across
every request-handling goroutine; call `Allow(tenant)` per request and
`Refill()` from whatever drives your tick -- a `time.Ticker` in `main`, a cron
job, a test. `Snapshot()` is the deliberate exception to "never clone the
whole map": a metrics endpoint or an admin dashboard calls it far less often
than `Allow`, so paying for one full clone there, in exchange for never
holding `Limiter`'s lock while reading every bucket's token count
individually, is the right trade in that one place and nowhere else.

`ExampleLimiter_Allow` is the runnable demonstration of this module: `go
test` executes it and compares its stdout against the `// Output:` comment.

```go
func ExampleLimiter_Allow() {
	l := New(1, 2)

	fmt.Println(l.Allow("tenant-a"))
	fmt.Println(l.Allow("tenant-a"))
	fmt.Println(l.Allow("tenant-a")) // burst of 2 exhausted

	l.Refill()
	fmt.Println(l.Allow("tenant-a")) // one token restored

	// Output:
	// true
	// true
	// false
	// true
}
```

### Tests

The first five tests are the table, spread across named functions rather than
one big `[]struct`, because each pins a distinct piece of behavior: burst
exhaustion, tenant independence, a zero-burst hard block, refill capped at
burst, and a zero rate that never restores tokens. `TestSnapshotIsIndependentOfLimiter`
and `TestSnapshotOfEmptyLimiterIsEmpty` cover `Snapshot`'s aliasing contract
and its empty-input edge case. `allowViaFullClone` is the unexported
antipattern; `TestAllowViaFullCloneAllocatesMoreAsTenantMapGrows` first
confirms it agrees with `Allow` on the answer, then uses
`testing.AllocsPerRun` to assert the property that matters -- point-locked
allocates strictly less than full-clone once the tenant map is non-trivial --
never a specific count, and explains inline why it cannot call `t.Parallel`.
`TestConcurrentTenantsUnderRace` is the concurrency test the type's doc
comment promises: fifty goroutines, fifty distinct tenants, hammering `Allow`,
`Refill`, and `Snapshot` at once, run under `-race`.

Create `quota_test.go`:

```go
package quota

import (
	"fmt"
	"maps"
	"sync"
	"testing"
)

func TestAllowGrantsUpToBurstThenDenies(t *testing.T) {
	t.Parallel()

	l := New(1, 3)
	for i := range 3 {
		if !l.Allow("tenant-a") {
			t.Fatalf("Allow #%d for tenant-a = false, want true (burst not yet exhausted)", i)
		}
	}
	if l.Allow("tenant-a") {
		t.Fatal("Allow after burst exhausted = true, want false")
	}
}

func TestAllowTracksTenantsIndependently(t *testing.T) {
	t.Parallel()

	l := New(1, 1)
	if !l.Allow("tenant-a") {
		t.Fatal("first Allow for tenant-a = false, want true")
	}
	if !l.Allow("tenant-b") {
		t.Fatal("tenant-b denied because tenant-a's bucket is empty; tenants must not share state")
	}
	if l.Allow("tenant-a") {
		t.Fatal("tenant-a should be exhausted after its one token")
	}
}

func TestAllowWithZeroBurstAlwaysDenies(t *testing.T) {
	t.Parallel()

	l := New(5, 0)
	if l.Allow("tenant-a") {
		t.Fatal("Allow with burst=0 = true, want false for every request")
	}
}

func TestRefillRestoresTokensCappedAtBurst(t *testing.T) {
	t.Parallel()

	l := New(2, 3)
	l.Allow("tenant-a")
	l.Allow("tenant-a")
	l.Allow("tenant-a") // exhausted: 0 tokens left

	l.Refill() // +2, capped at 3
	got := l.Snapshot()["tenant-a"]
	if got != 2 {
		t.Fatalf("Snapshot()[tenant-a] after one refill = %d, want 2", got)
	}

	l.Refill() // +2 would be 4, capped at burst=3
	got = l.Snapshot()["tenant-a"]
	if got != 3 {
		t.Fatalf("Snapshot()[tenant-a] after second refill = %d, want 3 (capped at burst)", got)
	}
}

func TestRefillWithZeroRateNeverRestores(t *testing.T) {
	t.Parallel()

	l := New(0, 1)
	l.Allow("tenant-a")
	l.Refill()
	if l.Allow("tenant-a") {
		t.Fatal("Allow after Refill with rate=0 = true, want false: rate 0 must never restore tokens")
	}
}

func TestSnapshotIsIndependentOfLimiter(t *testing.T) {
	t.Parallel()

	l := New(1, 2)
	l.Allow("tenant-a")
	snap := l.Snapshot()

	snap["tenant-a"] = 999
	if got := l.Snapshot()["tenant-a"]; got != 1 {
		t.Fatalf("mutating a returned Snapshot changed the Limiter's own state: got %d, want 1", got)
	}
}

func TestSnapshotOfEmptyLimiterIsEmpty(t *testing.T) {
	t.Parallel()

	if snap := New(1, 1).Snapshot(); len(snap) != 0 {
		t.Fatalf("Snapshot() of a fresh Limiter = %v, want empty", snap)
	}
}

// allowViaFullClone is the version of Allow a first draft reaches for: clone
// the entire tenant-to-bucket map before touching the one tenant the
// request is actually about. It is technically safe -- maps.Clone of a
// map[string]*bucket copies the pointers, so the bucket it decrements is
// still the real one -- but every call pays for an allocation proportional
// to the number of tenants the Limiter tracks, and holds the Limiter's own
// lock for that entire clone, serializing every other tenant's request
// behind it. It is never exported and never reachable from the package
// API; it exists so the tests can pin the cost it pays.
func allowViaFullClone(l *Limiter, tenant string) bool {
	l.mu.Lock()
	cloned := maps.Clone(l.buckets)
	l.mu.Unlock()

	b, ok := cloned[tenant]
	if !ok {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}

// TestAllowViaFullCloneAllocatesMoreAsTenantMapGrows proves the point-locked
// Allow and the full-clone version agree on the answer, then shows why the
// point-locked version is the one shipped: the exact allocation count is a
// runtime detail and is not asserted, but that the full-clone version needs
// strictly more allocations once the tenant map is non-trivial holds across
// toolchains, because it always pays for the whole map regardless of which
// single tenant a request is about.
//
// This test deliberately does not call t.Parallel: testing.AllocsPerRun
// panics when run from a parallel test, because a concurrent goroutine
// allocating in the background would corrupt its measurement.
func TestAllowViaFullCloneAllocatesMoreAsTenantMapGrows(t *testing.T) {
	l := New(1000, 1000)
	for i := range 500 {
		l.Allow(fmt.Sprintf("tenant-%d", i))
	}

	if got, want := l.Allow("tenant-0"), allowViaFullClone(l, "tenant-1"); got != true || want != true {
		t.Fatalf("Allow = %v, allowViaFullClone = %v, want both true (plenty of burst left)", got, want)
	}

	pointLocked := testing.AllocsPerRun(50, func() {
		l.Allow("tenant-2")
	})
	fullClone := testing.AllocsPerRun(50, func() {
		allowViaFullClone(l, "tenant-2")
	})
	if !(pointLocked < fullClone) {
		t.Fatalf("allocations: point-locked = %v, full-clone = %v; want point-locked < full-clone", pointLocked, fullClone)
	}
}

// TestConcurrentTenantsUnderRace exercises the concurrency contract in the
// Limiter doc comment: many goroutines, many distinct tenants, calling
// Allow and Refill at once. Run with -race, this is what catches a
// regression to a single map-wide lock guarding token counts too.
func TestConcurrentTenantsUnderRace(t *testing.T) {
	t.Parallel()

	l := New(1, 4)
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(tenant string) {
			defer wg.Done()
			for range 4 {
				l.Allow(tenant)
			}
			l.Refill()
			_ = l.Snapshot()
		}(fmt.Sprintf("tenant-%d", i))
	}
	wg.Wait()

	snap := l.Snapshot()
	if len(snap) != 50 {
		t.Fatalf("Snapshot() tracked %d tenants, want 50", len(snap))
	}
}

// ExampleLimiter_Allow is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleLimiter_Allow() {
	l := New(1, 2)

	fmt.Println(l.Allow("tenant-a"))
	fmt.Println(l.Allow("tenant-a"))
	fmt.Println(l.Allow("tenant-a")) // burst of 2 exhausted

	l.Refill()
	fmt.Println(l.Allow("tenant-a")) // one token restored

	// Output:
	// true
	// true
	// false
	// true
}
```

## Review

`Limiter` is correct when `Allow` never lets a tenant exceed its burst and
never lets one tenant's exhaustion affect another's -- the first five tests
pin exactly that, including the zero-burst and zero-rate edges. What
separates the shipped `Allow` from `allowViaFullClone` is not correctness,
which `TestAllowViaFullCloneAllocatesMoreAsTenantMapGrows` confirms both
share, but the blast radius of the lock each one takes: point-locking a
single bucket costs the same whether the limiter tracks ten tenants or ten
million, while cloning the whole map costs more as the tenant count grows and
serializes every other tenant behind that copy. `Snapshot` is the deliberate
exception -- reporting is not the hot path, so a full `maps.Clone` there is
the right call, made explicitly rather than by accident. `Limiter`'s own
mutex guards only the tenant map's structure; each bucket's mutex guards only
its own token count, which is what lets `TestConcurrentTenantsUnderRace` run
clean under `-race` with fifty tenants hammering the limiter at once. Run
`go test -count=1 -race ./...`.

## Resources

- [`maps` package: None of it is synchronized](00-concepts.md) — the concept this module answers with per-bucket locking instead of a bigger lock.
- [`maps.Clone`](https://pkg.go.dev/maps#Clone) — the primitive `Snapshot` uses deliberately, once, and `allowViaFullClone` uses on every call.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the map-structure lock, held only for lookup/insert, never for the token accounting.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — the allocation probe, and its restriction against parallel tests.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-named-route-table-generic-constraint.md](12-named-route-table-generic-constraint.md) | Next: [14-chunk-store-integrity-equalfunc.md](14-chunk-store-integrity-equalfunc.md)
