# Exercise 2: Least-Connections

When request cost varies and you have no better signal than concurrency, least-connections is the right load balancer: it routes each request to the healthy backend currently handling the fewest in-flight requests, which steers traffic away from a slow or stalled backend automatically. This exercise builds it with a linear scan under a read lock and per-backend atomic counters.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
balancer.go          Backend, NewBackend, Balancer interface, ErrNoHealthyBackend
leastconn.go         LeastConnectionsBalancer (min active-connections scan)
cmd/
  demo/
    main.go          three held picks spread across backends, then dynamic membership
leastconn_test.go    min-conns selection, release tracking, even spread, concurrency
```

- Files: `balancer.go`, `leastconn.go`, `cmd/demo/main.go`, `leastconn_test.go`.
- Implement: `Backend` with atomic counters, the `Balancer` interface, and `LeastConnectionsBalancer` with `Pick`, `Release`, `AddBackend`, `RemoveBackend`.
- Test: picks the minimum, falls back to pool order on ties, spreads six picks evenly across three idle backends, tracks connection counts through release, skips unhealthy backends, and survives a 100-goroutine race with all counters returning to zero.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The shared Backend object

Least-connections needs no per-algorithm state beyond the pool itself, because the signal it routes on — active connections — already lives on each `Backend`. The `Backend` type and `Balancer` interface are therefore identical to every other balancer in this lesson and live in `balancer.go`. A `Backend` carries its address, weight, a health flag, and two atomic counters: `activeConns` (the live in-flight count) and `totalReqs` (a cumulative total). They are `atomic.Int64` rather than mutex-guarded fields because a single-value atomic is both faster and safer — the embedded `noCopy` guard makes `go vet` reject any accidental copy of a `Backend`, which is why the system passes `*Backend` everywhere and allocates each backend once with `NewBackend`.

The `Pick`/`Release` contract is what gives least-connections its signal. `Pick` increments the winner's `activeConns`; `Release` decrements it. The count is not bookkeeping for its own sake: it is exactly the quantity `Pick` minimizes over, so a `Pick` left without a matching `Release` makes a backend look permanently busy and removes it from consideration forever. `noHealthyErr` wraps the sentinel `ErrNoHealthyBackend` with `%w` for `errors.Is`, and the compile-time `var _ Balancer = (*LeastConnectionsBalancer)(nil)` assertion catches a missing method at build time.

Create `balancer.go`:

```go
package balancer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
)

// ErrNoHealthyBackend is returned when Pick cannot find a healthy backend.
var ErrNoHealthyBackend = errors.New("balancer: no healthy backend available")

// Backend describes a single upstream endpoint.
// It must be allocated with NewBackend and never copied after first use
// because it embeds atomic types.
type Backend struct {
	Addr   string
	Weight int

	healthy     atomic.Bool
	activeConns atomic.Int64
	totalReqs   atomic.Int64
}

// NewBackend returns a Backend with the given address and weight (minimum 1).
// The backend starts unhealthy; call SetHealthy(true) before routing traffic.
func NewBackend(addr string, weight int) *Backend {
	if weight <= 0 {
		weight = 1
	}
	return &Backend{Addr: addr, Weight: weight}
}

// SetHealthy marks the backend as healthy (true) or unhealthy (false).
func (b *Backend) SetHealthy(v bool) { b.healthy.Store(v) }

// IsHealthy reports whether the backend is currently healthy.
func (b *Backend) IsHealthy() bool { return b.healthy.Load() }

// ActiveConns returns the number of requests currently in flight to this backend.
func (b *Backend) ActiveConns() int64 { return b.activeConns.Load() }

// TotalReqs returns the cumulative number of requests routed to this backend.
func (b *Backend) TotalReqs() int64 { return b.totalReqs.Load() }

// Balancer selects an upstream backend for each incoming request.
type Balancer interface {
	// Pick selects a backend for the request. The key is used by consistent-
	// hash balancers to pin a request to a specific backend; other algorithms
	// may ignore it. The caller must call Release when the request completes.
	Pick(ctx context.Context, key string) (*Backend, error)

	// Release signals that a request to the given backend has completed.
	// It decrements the backend's active connection count.
	Release(b *Backend)

	// AddBackend adds b to the pool. Safe to call concurrently with Pick.
	AddBackend(b *Backend)

	// RemoveBackend removes the backend with the given address from the pool.
	// In-flight requests already routed to that backend are not interrupted;
	// their Release calls still decrement the backend's counter correctly.
	RemoveBackend(addr string)
}

// Compile-time interface satisfaction check.
var _ Balancer = (*LeastConnectionsBalancer)(nil)

func noHealthyErr(algo string) error {
	return fmt.Errorf("%w (algorithm: %s)", ErrNoHealthyBackend, algo)
}
```

### A linear scan, an RWMutex, and the bounded race

`Pick` scans every healthy backend and returns the one with the smallest `activeConns`, breaking ties by pool order so an idle pool degrades to round-robin-like behavior (the first backend keeps winning until it has more connections than the next). A linear scan beats a heap at the backend counts that matter: under a hundred backends the heap's larger constant factors lose, and a heap would also demand reordering work on every `Release` that the scan avoids entirely.

The concurrency model is what makes least-connections cheaper than smooth round-robin. `Pick` only reads the pool slice and then increments one backend's counter through an atomic; it never writes shared per-algorithm state under the lock. That lets the pool be guarded by a `sync.RWMutex` so many `Pick` callers hold the read lock at once, while `AddBackend`/`RemoveBackend` take the write lock. There is one deliberate race: between a `Pick` observing the minimum and its atomic increment, another goroutine may pick the same backend, so two requests can land on a backend that was the minimum by one. The error is bounded to a single extra connection and self-corrects on the next `Pick` — an acceptable trade for not serializing every selection. The concurrent test plus `-race` confirm the counters stay consistent regardless.

Create `leastconn.go`:

```go
package balancer

import (
	"context"
	"sync"
)

// LeastConnectionsBalancer routes each request to the healthy backend with the
// fewest active connections. Ties are broken by position in the pool (first
// added wins), which approximates round-robin when all backends are idle.
type LeastConnectionsBalancer struct {
	mu       sync.RWMutex
	backends []*Backend
}

// NewLeastConnections returns a LeastConnectionsBalancer with the given initial backends.
func NewLeastConnections(backends []*Backend) *LeastConnectionsBalancer {
	cp := make([]*Backend, len(backends))
	copy(cp, backends)
	return &LeastConnectionsBalancer{backends: cp}
}

// AddBackend adds b to the pool.
func (lc *LeastConnectionsBalancer) AddBackend(b *Backend) {
	lc.mu.Lock()
	lc.backends = append(lc.backends, b)
	lc.mu.Unlock()
}

// RemoveBackend removes the backend with the given address from the pool.
func (lc *LeastConnectionsBalancer) RemoveBackend(addr string) {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	out := lc.backends[:0]
	for _, b := range lc.backends {
		if b.Addr != addr {
			out = append(out, b)
		}
	}
	lc.backends = out
}

// Pick returns the healthy backend with the fewest active connections.
// The key is ignored.
func (lc *LeastConnectionsBalancer) Pick(_ context.Context, _ string) (*Backend, error) {
	lc.mu.RLock()
	defer lc.mu.RUnlock()

	var best *Backend
	var bestConns int64
	for _, b := range lc.backends {
		if !b.IsHealthy() {
			continue
		}
		conns := b.ActiveConns()
		if best == nil || conns < bestConns {
			best = b
			bestConns = conns
		}
	}
	if best == nil {
		return nil, noHealthyErr("least-connections")
	}
	best.activeConns.Add(1)
	best.totalReqs.Add(1)
	return best, nil
}

// Release decrements the active connection count for b.
func (lc *LeastConnectionsBalancer) Release(b *Backend) {
	b.activeConns.Add(-1)
}
```

Note the copy in `NewLeastConnections`: the constructor copies the caller's slice so a later mutation of the caller's slice header cannot disturb the pool. `RemoveBackend` filters in place with `lc.backends[:0]`, reusing the backing array, and never touches a removed backend's counters — an in-flight request already routed there still releases correctly because `Release` operates on the `*Backend` directly, not on pool membership.

### The runnable demo

The demo holds three picks without releasing, so each lands on a different backend as the previous pick raises that backend's count; then it frees one backend to show it becomes the next pick, and finally adds an idle backend that wins because it alone has zero connections.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	balancer "example.com/least-connections"
)

func main() {
	backends := []*balancer.Backend{
		balancer.NewBackend("10.0.0.1:8080", 1),
		balancer.NewBackend("10.0.0.2:8080", 1),
		balancer.NewBackend("10.0.0.3:8080", 1),
	}
	for _, b := range backends {
		b.SetHealthy(true)
	}
	ctx := context.Background()
	lc := balancer.NewLeastConnections(backends)

	fmt.Println("--- Least-Connections (route to the least-loaded backend) ---")
	// Hold three picks without releasing: each lands on a different backend
	// because the previous pick raised that backend's active connection count.
	var held []*balancer.Backend
	for i := 0; i < 3; i++ {
		b, _ := lc.Pick(ctx, "")
		fmt.Printf("  pick %d -> %s (active_conns=%d)\n", i+1, b.Addr, b.ActiveConns())
		held = append(held, b)
	}

	fmt.Println()
	fmt.Println("--- A freed backend becomes the least loaded ---")
	// Release only the first backend; the next pick must return it because it
	// is now the one healthy backend with zero active connections.
	lc.Release(held[0])
	fmt.Printf("  released %s (active_conns=%d)\n", held[0].Addr, held[0].ActiveConns())
	next, _ := lc.Pick(ctx, "")
	fmt.Printf("  next pick -> %s\n", next.Addr)

	fmt.Println()
	fmt.Println("--- Dynamic membership ---")
	// The original three backends each hold one connection; a brand-new idle
	// backend has zero, so least-connections routes the next request to it.
	newB := balancer.NewBackend("10.0.0.4:8080", 1)
	newB.SetHealthy(true)
	lc.AddBackend(newB)
	fmt.Println("  added 10.0.0.4:8080 (idle: zero active connections)")
	b, _ := lc.Pick(ctx, "")
	fmt.Printf("  next pick -> %s\n", b.Addr)

	// Drain everything.
	lc.Release(next)
	lc.Release(b)
	for _, h := range held[1:] {
		lc.Release(h)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
--- Least-Connections (route to the least-loaded backend) ---
  pick 1 -> 10.0.0.1:8080 (active_conns=1)
  pick 2 -> 10.0.0.2:8080 (active_conns=1)
  pick 3 -> 10.0.0.3:8080 (active_conns=1)

--- A freed backend becomes the least loaded ---
  released 10.0.0.1:8080 (active_conns=0)
  next pick -> 10.0.0.1:8080

--- Dynamic membership ---
  added 10.0.0.4:8080 (idle: zero active connections)
  next pick -> 10.0.0.4:8080
```

### Tests

The tests pin every property. `Pick` must return the minimum and fall back to pool order on a tie; six picks across three idle backends must spread exactly two each, because the algorithm always routes to a current minimum; counts must track precisely through `Release` back to zero; an unhealthy backend must be skipped; an all-unhealthy pool must return `ErrNoHealthyBackend`; and a 100-goroutine race test must leave every counter at zero. The `Example` is auto-verified against its `// Output:` block.

Create `leastconn_test.go`:

```go
package balancer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestLeastConnectionsPicksMinConns(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 1)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)

	lb := NewLeastConnections([]*Backend{a, b})
	ctx := context.Background()

	// First pick: both have 0 conns, tie broken by pool order; a wins.
	p1, err := lb.Pick(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if p1.Addr != "a:80" {
		t.Errorf("first pick = %s, want a:80 (tie broken by pool order)", p1.Addr)
	}
	// Do NOT release p1; a now has 1 active connection.

	// Second pick: a has 1 conn, b has 0; b wins.
	p2, err := lb.Pick(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if p2.Addr != "b:80" {
		t.Errorf("second pick = %s, want b:80 (fewer connections)", p2.Addr)
	}

	lb.Release(p1)
	lb.Release(p2)
}

func TestLeastConnectionsTracksRelease(t *testing.T) {
	t.Parallel()

	backends := []*Backend{
		NewBackend("a:80", 1),
		NewBackend("b:80", 1),
		NewBackend("c:80", 1),
	}
	for _, b := range backends {
		b.SetHealthy(true)
	}
	lb := NewLeastConnections(backends)
	ctx := context.Background()

	var held [6]*Backend
	for i := range held {
		p, err := lb.Pick(ctx, "")
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		held[i] = p
	}

	// Six picks across three idle backends spread two each: least-connections
	// always routes to a current minimum, so the spread must be exactly even.
	for _, b := range backends {
		if c := b.ActiveConns(); c != 2 {
			t.Errorf("backend %s: active connections = %d, want 2 (even spread)", b.Addr, c)
		}
	}

	// Total active connections must equal the number of held picks.
	var total int64
	for _, b := range backends {
		total += b.ActiveConns()
	}
	if total != int64(len(held)) {
		t.Errorf("total active connections = %d, want %d", total, len(held))
	}

	for _, p := range held {
		lb.Release(p)
	}

	// After releasing all, active connections must be zero.
	for _, b := range backends {
		if c := b.ActiveConns(); c != 0 {
			t.Errorf("backend %s: active connections = %d after release, want 0", b.Addr, c)
		}
	}
}

func TestLeastConnectionsSkipsUnhealthy(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 1)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	// b is unhealthy.

	lb := NewLeastConnections([]*Backend{a, b})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		got, err := lb.Pick(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != "a:80" {
			t.Errorf("Pick %d: got %s, want a:80 (b is unhealthy)", i, got.Addr)
		}
		lb.Release(got)
	}
}

func TestLeastConnectionsAllUnhealthy(t *testing.T) {
	t.Parallel()

	lb := NewLeastConnections([]*Backend{NewBackend("a:80", 1)})
	_, err := lb.Pick(context.Background(), "")
	if !errors.Is(err, ErrNoHealthyBackend) {
		t.Errorf("err = %v, want ErrNoHealthyBackend", err)
	}
}

func TestLeastConnectionsConcurrent(t *testing.T) {
	t.Parallel()

	backends := make([]*Backend, 5)
	for i := range backends {
		backends[i] = NewBackend(fmt.Sprintf("host%d:80", i), 1)
		backends[i].SetHealthy(true)
	}

	lb := NewLeastConnections(backends)
	ctx := context.Background()

	const goroutines = 100
	const picksEach = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < picksEach; j++ {
				b, err := lb.Pick(ctx, "")
				if err != nil {
					return
				}
				lb.Release(b)
			}
		}()
	}
	wg.Wait()

	// All active connections must be zero after all goroutines complete.
	for _, b := range backends {
		if c := b.ActiveConns(); c != 0 {
			t.Errorf("backend %s: active connections = %d after concurrent test, want 0", b.Addr, c)
		}
	}
}

func ExampleNewLeastConnections() {
	a := NewBackend("srv-a:80", 1)
	b := NewBackend("srv-b:80", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)

	lb := NewLeastConnections([]*Backend{a, b})
	ctx := context.Background()

	// First pick: both have 0 connections; tie, so a wins (first in pool).
	held, _ := lb.Pick(ctx, "")
	// Second pick: a has 1 conn, b has 0; b wins.
	next, _ := lb.Pick(ctx, "")
	fmt.Println(held.Addr, next.Addr)
	lb.Release(held)
	lb.Release(next)
	// Output:
	// srv-a:80 srv-b:80
}
```

## Review

The algorithm is correct when selection always tracks the live minimum: the second pick of a two-backend pool must move to the other backend once the first holds a connection, and six picks across three idle backends must split exactly two each. If the spread is uneven or a stale backend keeps winning, the most likely cause is a missing `Release` — `activeConns` only ever climbs, so the earliest-picked backends look permanently loaded and the routing skews; every successful `Pick` must be matched by exactly one `Release`, ideally deferred. The other classic error is copying a `Backend` (`*b`) to take a "snapshot" and releasing the copy; `go vet` flags the atomic copy, and the original count never drops. Confirm an unhealthy backend is skipped, an all-unhealthy pool returns `ErrNoHealthyBackend` via `errors.Is`, and the 100-goroutine test under `-race` returns every counter to zero, which together establish that the read-lock-plus-atomic model is sound despite the deliberately tolerated one-connection selection race.

## Resources

- [`sync` package — pkg.go.dev](https://pkg.go.dev/sync) — `sync.RWMutex` semantics and when read-write locking beats a plain mutex.
- [`sync/atomic` package — pkg.go.dev](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, the lock-free connection counter least-connections minimizes over.
- [NGINX: Choosing a load-balancing method](https://docs.nginx.com/nginx/admin-guide/load-balancer/http-load-balancer/#choosing-a-load-balancing-method) — where least-connections fits among production balancing methods.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-weighted-round-robin.md](01-weighted-round-robin.md) | Next: [03-consistent-hashing.md](03-consistent-hashing.md)
