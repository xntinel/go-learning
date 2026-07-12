# Exercise 34: Resolve Service Hostnames to Healthy Endpoints With TTL Cache and Graceful Degradation

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A sidecar or client-side load balancer resolving a logical service name —
whether it's reading SRV records, watching a Consul catalog, or just
tailing a health-status file behind a lightweight internal DNS setup —
has to answer one question under constant concurrent load: which endpoint
should I dial right now? Health checks run continuously and update shared
state; resolutions happen far more often and just need to read it. This is
the textbook case for `sync.RWMutex` rather than a plain mutex: many
concurrent readers cost nothing extra, while a write (a health update)
still gets exclusive access when it needs it. The interesting design
problem is what happens when *every* known endpoint has gone unhealthy —
failing every request outright is often worse than serving a client the
last address that was known to work, clearly labeled as stale. This module
builds that registry. It is self-contained: its own `go mod init`, code,
demo, and test.

## What you'll build

```text
svcdiscovery/                independent module: example.com/dns-service-discovery-with-staleness
  go.mod                       go 1.24
  svcdiscovery.go                package svcdiscovery; EndpointState; Registry; New() *Registry; UpdateHealth, Resolve
  cmd/demo/main.go               runnable demo walking healthy -> degraded -> all-unhealthy -> stale fallback
  svcdiscovery_test.go            per-tier resolution, stale fallback, empty registry, concurrent updates and resolves
```

- Implement: `Resolve() (Endpoint, bool, error)` — a tagless switch over the three-tier fallback (healthy, then degraded, then the stale cache), guarded by `sync.RWMutex` so concurrent health updates and concurrent resolutions never race.
- Test: resolution preferring healthy over degraded, degraded as the fallback when nothing is healthy, the stale-cache fallback once everything is unhealthy, an empty registry's error, and a concurrency subtest mixing writers and readers.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/34-dns-service-discovery-with-staleness/cmd/demo
cd go-solutions/03-control-flow/03-switch-statements/34-dns-service-discovery-with-staleness
go mod edit -go=1.24
```

### Why the stale cache must never be sorted under a read lock

The three-tier fallback itself is a small tagless switch:

```go
switch {
case len(healthy) > 0:
    return pickLowestAddr(healthy), false, nil
case len(degraded) > 0:
    return pickLowestAddr(degraded), false, nil
case len(r.staleCache) > 0:
    return r.staleCache[0], true, nil
default:
    return Endpoint{}, false, ErrNoEndpoints
}
```

`healthy` and `degraded` are freshly built local slices on every call to
`Resolve` — safe to sort in place, because no other goroutine can ever see
them. `r.staleCache`, in contrast, is a field on the shared `Registry`,
and `Resolve` only ever holds an `RLock` — a *shared* lock that lets
multiple goroutines call `Resolve` at the same time. Sorting
`r.staleCache` in place inside `Resolve` would mean two concurrent
`Resolve` calls both mutating the same backing array at once: a real data
race, invisible in casual testing but reliably caught by `go test -race`.
The fix is to keep `r.staleCache` pre-sorted at write time instead:
`rememberHealthy` re-sorts it every time a healthy endpoint is recorded,
but it only ever runs while `UpdateHealth` holds the *exclusive* write
lock, so there is only ever one goroutine touching that slice's backing
array at a time. By the time `Resolve` reads `r.staleCache[0]` under a
mere `RLock`, the slice is already sorted and nothing needs to mutate it.

This is the general lesson underneath the concurrency-hazard notes
attached to this lesson's other exercises: it's not enough to guard *every
individual read and write* of shared state with a lock — a helper function
that looks locally correct (sorting a slice before returning its first
element) can still be a bug the moment it operates on state another
goroutine can observe concurrently under a shared, non-exclusive lock.

Create `svcdiscovery.go`:

```go
// Package svcdiscovery resolves a logical service name to one of its
// healthy backend endpoints, the way a sidecar or client-side load
// balancer resolves SRV records (or a health-status file behind a simple
// internal DNS setup) into a concrete address to dial. Health checks and
// resolutions happen concurrently and continuously in a real system, so
// the registry is built around an RWMutex: many goroutines resolve at
// once (cheap, frequent, read-only), while health check results update the
// registry far less often (rare, but must never be blocked out indefinitely
// by a flood of readers, which is exactly the trade RWMutex is for).
package svcdiscovery

import (
	"errors"
	"sort"
	"sync"
)

// ErrNoEndpoints is returned when the registry has no endpoint to offer at
// all -- not even a stale one.
var ErrNoEndpoints = errors.New("svcdiscovery: no endpoints available")

// EndpointState is the closed set of health states a health check reports.
type EndpointState int

const (
	Healthy EndpointState = iota
	Degraded
	Unhealthy
)

// Endpoint is one backend address and its last known health.
type Endpoint struct {
	Addr  string
	State EndpointState
}

// Registry tracks the health of a set of endpoints and resolves the best
// one available, falling back to the last known-healthy snapshot when
// every current endpoint has gone unhealthy.
type Registry struct {
	mu         sync.RWMutex
	endpoints  map[string]Endpoint
	staleCache []Endpoint
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{endpoints: make(map[string]Endpoint)}
}

// UpdateHealth records addr's current health state. It takes the write
// lock, so it blocks (and is blocked by) both concurrent Resolve calls and
// other concurrent UpdateHealth calls -- health updates are rare enough
// that serializing them against reads is the right trade for correctness
// over raw throughput.
func (r *Registry) UpdateHealth(addr string, state EndpointState) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ep := Endpoint{Addr: addr, State: state}
	r.endpoints[addr] = ep
	if state == Healthy {
		r.rememberHealthy(ep)
	}
}

// rememberHealthy refreshes the stale-fallback cache with ep, replacing
// any older entry for the same address, and keeps the cache sorted by
// Addr. Called only while holding r.mu for writing, which is what makes it
// safe to re-sort staleCache in place: Resolve only ever reads it under an
// RLock and never sorts it itself, so there is exactly one goroutine ever
// mutating this slice's backing array at a time.
func (r *Registry) rememberHealthy(ep Endpoint) {
	filtered := make([]Endpoint, 0, len(r.staleCache)+1)
	for _, existing := range r.staleCache {
		if existing.Addr != ep.Addr {
			filtered = append(filtered, existing)
		}
	}
	filtered = append(filtered, ep)
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Addr < filtered[j].Addr })
	r.staleCache = filtered
}

// Resolve picks the best endpoint currently known: a healthy one if any
// exists, otherwise a degraded one, otherwise the most recently known
// healthy snapshot (reporting stale=true), and only fails outright when
// none of those three tiers has anything to offer. Ties within a tier are
// broken by sorting on Addr, so Resolve is deterministic for a fixed set of
// inputs even though the underlying map has no natural order.
func (r *Registry) Resolve() (ep Endpoint, stale bool, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var healthy, degraded []Endpoint
	for _, e := range r.endpoints {
		switch e.State {
		case Healthy:
			healthy = append(healthy, e)
		case Degraded:
			degraded = append(degraded, e)
		}
	}

	switch {
	case len(healthy) > 0:
		return pickLowestAddr(healthy), false, nil
	case len(degraded) > 0:
		return pickLowestAddr(degraded), false, nil
	case len(r.staleCache) > 0:
		// staleCache is kept sorted by rememberHealthy under the write
		// lock, so the first element is already the lowest Addr -- no
		// sort needed (and none allowed: this slice is shared, and we
		// only hold the read lock here).
		return r.staleCache[0], true, nil
	default:
		return Endpoint{}, false, ErrNoEndpoints
	}
}

// pickLowestAddr sorts eps -- a slice freshly built by this Resolve call
// and owned by no one else -- and returns its lowest-Addr element, so a
// fixed set of inputs always resolves to the same endpoint.
func pickLowestAddr(eps []Endpoint) Endpoint {
	sort.Slice(eps, func(i, j int) bool { return eps[i].Addr < eps[j].Addr })
	return eps[0]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	svcdiscovery "example.com/dns-service-discovery-with-staleness"
)

func main() {
	r := svcdiscovery.New()

	r.UpdateHealth("10.0.0.1:8080", svcdiscovery.Healthy)
	r.UpdateHealth("10.0.0.2:8080", svcdiscovery.Healthy)
	r.UpdateHealth("10.0.0.3:8080", svcdiscovery.Degraded)

	ep, stale, err := r.Resolve()
	fmt.Printf("resolve #1: %+v stale=%v err=%v\n", ep, stale, err)

	// The only healthy endpoint fails, leaving a degraded one.
	r.UpdateHealth("10.0.0.1:8080", svcdiscovery.Unhealthy)
	r.UpdateHealth("10.0.0.2:8080", svcdiscovery.Unhealthy)

	ep, stale, err = r.Resolve()
	fmt.Printf("resolve #2: %+v stale=%v err=%v\n", ep, stale, err)

	// Now every endpoint is unhealthy: fall back to the last known-healthy
	// snapshot instead of failing outright.
	r.UpdateHealth("10.0.0.3:8080", svcdiscovery.Unhealthy)

	ep, stale, err = r.Resolve()
	fmt.Printf("resolve #3: %+v stale=%v err=%v\n", ep, stale, err)

	// A brand-new registry with no history at all has nothing to fall
	// back to.
	empty := svcdiscovery.New()
	_, _, err = empty.Resolve()
	fmt.Println("resolve on empty registry, err:", err)
}
```

Run `go run ./cmd/demo`, expected output:

```
resolve #1: {Addr:10.0.0.1:8080 State:0} stale=false err=<nil>
resolve #2: {Addr:10.0.0.3:8080 State:1} stale=false err=<nil>
resolve #3: {Addr:10.0.0.1:8080 State:0} stale=true err=<nil>
resolve on empty registry, err: svcdiscovery: no endpoints available
```

### Tests

`TestResolvePrefersHealthyOverDegraded`,
`TestResolveFallsBackToDegradedWhenNoneHealthy`, and
`TestResolveFallsBackToStaleCacheWhenAllUnhealthy` each drive one tier of
the fallback in isolation. `TestResolveFailsOnEmptyRegistry` checks the
fail-closed error. `TestConcurrentUpdatesAndResolves` fires writer and
reader goroutines at the registry simultaneously; since the exact
interleaving is nondeterministic, it only asserts that `Resolve` never
panics and never returns a zero-value endpoint alongside a nil error,
relying on `go test -race` to catch any lock-discipline bug.

Create `svcdiscovery_test.go`:

```go
package svcdiscovery

import (
	"errors"
	"sync"
	"testing"
)

func TestResolvePrefersHealthyOverDegraded(t *testing.T) {
	t.Parallel()

	r := New()
	r.UpdateHealth("b", Healthy)
	r.UpdateHealth("a", Degraded)

	ep, stale, err := r.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stale {
		t.Fatal("stale = true, want false: a healthy endpoint exists")
	}
	if ep.Addr != "b" {
		t.Fatalf("Addr = %q, want %q", ep.Addr, "b")
	}
}

func TestResolveFallsBackToDegradedWhenNoneHealthy(t *testing.T) {
	t.Parallel()

	r := New()
	r.UpdateHealth("a", Degraded)
	r.UpdateHealth("b", Unhealthy)

	ep, stale, err := r.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stale {
		t.Fatal("stale = true, want false: a degraded endpoint is still current data, not stale")
	}
	if ep.Addr != "a" {
		t.Fatalf("Addr = %q, want %q", ep.Addr, "a")
	}
}

func TestResolveFallsBackToStaleCacheWhenAllUnhealthy(t *testing.T) {
	t.Parallel()

	r := New()
	r.UpdateHealth("a", Healthy)
	r.UpdateHealth("a", Unhealthy) // a goes unhealthy, but was remembered while healthy

	ep, stale, err := r.Resolve()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stale {
		t.Fatal("stale = false, want true: no current endpoint is healthy or degraded")
	}
	if ep.Addr != "a" {
		t.Fatalf("Addr = %q, want %q", ep.Addr, "a")
	}
}

func TestResolveFailsOnEmptyRegistry(t *testing.T) {
	t.Parallel()

	r := New()
	_, _, err := r.Resolve()
	if !errors.Is(err, ErrNoEndpoints) {
		t.Fatalf("err = %v, want errors.Is match for ErrNoEndpoints", err)
	}
}

// TestConcurrentUpdatesAndResolves drives many goroutines updating health
// for distinct endpoints while other goroutines resolve concurrently. The
// RWMutex makes every individual call race-free; the invariant this test
// checks is that Resolve never panics and always returns either a valid
// endpoint or ErrNoEndpoints, never a torn or inconsistent result.
func TestConcurrentUpdatesAndResolves(t *testing.T) {
	t.Parallel()

	r := New()
	const writers = 10
	const readers = 10

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			addr := "addr"
			states := []EndpointState{Healthy, Degraded, Unhealthy}
			r.UpdateHealth(addr, states[i%len(states)])
		}(i)
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ep, _, err := r.Resolve()
			if err != nil && !errors.Is(err, ErrNoEndpoints) {
				t.Errorf("Resolve() unexpected error: %v", err)
			}
			if err == nil && ep.Addr == "" {
				t.Error("Resolve() returned a zero-value endpoint with no error")
			}
		}()
	}
	wg.Wait()
}
```

Verify with:

```bash
go test -race -count=1 ./...
```

## Review

The registry is correct when `Resolve` always prefers a healthy endpoint
over a degraded one, always prefers a degraded one over the stale cache,
and only reports `stale=true` when the stale cache is the tier that
answered; it's correct under concurrency specifically because the one
slice `Resolve` reads without holding the exclusive lock (`staleCache`) is
never mutated anywhere except under that exclusive lock, and never sorted
at read time. Carry this forward: when you introduce an `RWMutex` to let
reads proceed concurrently, audit every field a reader touches for any
operation — not just assignment, but sorting, appending, or any other
in-place mutation — that could run while only the shared read lock is
held, and move that mutation to whichever path already holds the write
lock.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless (expressionless) switch form.
- [sync.RWMutex](https://pkg.go.dev/sync#RWMutex) — many concurrent readers, exclusive writers.
- [Consul: Health Checks](https://developer.hashicorp.com/consul/docs/services/usage/checks) — health-state-driven service discovery in a real, widely-deployed system.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-wal-checkpoint-decision-engine.md](33-wal-checkpoint-decision-engine.md) | Next: [35-singleflight-request-deduplicator.md](35-singleflight-request-deduplicator.md)
