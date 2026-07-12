# Exercise 1: Weighted Round-Robin

Round-robin is the load balancer everyone reaches for first, and the smooth weighted variant from nginx is the version worth knowing: it sends each backend its configured share of traffic while spreading that share evenly instead of in bursts. This exercise builds it behind a reusable `Balancer` interface, with a `Backend` type whose connection counters are lock-free atomics.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
balancer.go          Backend, NewBackend, Balancer interface, ErrNoHealthyBackend
roundrobin.go        rrEntry, RoundRobinBalancer (smooth weighted round-robin)
cmd/
  demo/
    main.go          weighted picks over a 1:2:1 pool, then add/remove a backend
roundrobin_test.go   equal/weighted distribution, smooth spread, unhealthy skip, concurrency
```

- Files: `balancer.go`, `roundrobin.go`, `cmd/demo/main.go`, `roundrobin_test.go`.
- Implement: `Backend` with atomic counters, the `Balancer` interface, and `RoundRobinBalancer` with `Pick`, `Release`, `AddBackend`, `RemoveBackend`.
- Test: equal weights alternate, weights 3:1 deliver exactly 300:100, a single cycle spreads as a,a,b,a, an unhealthy backend gets no traffic, and 50 goroutines x 100 picks leave counts exact and active connections at zero.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/04-load-balancing/01-weighted-round-robin/cmd/demo && cd go-solutions/42-capstone-service-mesh-data-plane/04-load-balancing/01-weighted-round-robin
go mod edit -go=1.26
```

### The Backend object and why its counters are atomic

Every balancer in this lesson selects from a pool of `Backend` values, so the `Backend` type and the `Balancer` interface live in `balancer.go` and the algorithm lives in its own file. A `Backend` carries an address, a configured weight, a health flag, a live active-connection count, and a cumulative request total. The last three change from many goroutines at once.

The naive way to make a shared counter safe is a mutex per field; the better way is `atomic.Int64` and `atomic.Bool`, which are both faster for a single value and safer, because they embed a `noCopy` guard. That guard is what makes `go vet` reject any accidental copy of a `Backend` with "assignment copies lock value", and it is the reason the whole system passes `*Backend` and never a bare `Backend`. A backend is allocated once by `NewBackend` and starts unhealthy: health is a flag separate from pool membership, so a backend can be present but temporarily out of rotation without being removed.

`Pick` and `Release` form a strict pair. `Pick` increments the chosen backend's `activeConns` and `totalReqs`; `Release` decrements `activeConns`. The count is the live signal load-aware algorithms route on, so a `Pick` that is never matched by a `Release` silently poisons every later decision. `noHealthyErr` wraps the sentinel `ErrNoHealthyBackend` with `%w` so callers can use `errors.Is` instead of matching error strings, and the compile-time `var _ Balancer = (*RoundRobinBalancer)(nil)` assertion turns a missing method into a build error rather than a runtime surprise.

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
var _ Balancer = (*RoundRobinBalancer)(nil)

func noHealthyErr(algo string) error {
	return fmt.Errorf("%w (algorithm: %s)", ErrNoHealthyBackend, algo)
}
```

### Smooth weighted round-robin, phase by phase

The naive weighted round-robin repeats each backend in sequence as many times as its weight, which is correct in aggregate but bursts: weights A=3, B=1 produce A, A, A, B. Nginx's smooth variant keeps the same proportions while interleaving. Each backend gets a `currentWeight` that starts at zero, and every `Pick` runs three phases: add each healthy backend's configured weight to its `currentWeight`, select the backend with the highest `currentWeight` (ties broken by pool order), then subtract the total healthy weight from the winner. For A=3, B=1 the cycle becomes A, A, B, A — the light backend lands at position 3 rather than after a run of three.

The invariant that makes this correct is that after a complete cycle every `currentWeight` returns to its starting value, so each backend has received exactly its proportional share. The three phases are one indivisible step and `currentWeight` is written on every `Pick`, so `Pick` holds an exclusive `sync.Mutex`: a read lock would let two goroutines race on the same `currentWeight` field. Because `currentWeight` is only ever touched while holding that mutex, it is a plain `int64`, not an atomic — the lock already serializes it. The critical section is O(n) over the backend count (typically under a hundred), so the exclusive lock is not a bottleneck.

Create `roundrobin.go`:

```go
package balancer

import (
	"context"
	"sync"
)

// rrEntry pairs a backend with the smooth-WRR currentWeight.
// currentWeight is only ever accessed while holding RoundRobinBalancer.mu,
// so a plain int64 (not atomic) is correct here.
type rrEntry struct {
	b             *Backend
	currentWeight int64
}

// RoundRobinBalancer distributes traffic using the smooth weighted round-robin
// algorithm from nginx (commit 27e94984486058d73157038f7950a0a36ecc6e35).
// A backend with weight W receives W/(sum of all weights) of all traffic,
// spread evenly across picks rather than as a burst.
type RoundRobinBalancer struct {
	mu      sync.Mutex
	entries []rrEntry
}

// NewRoundRobin returns a RoundRobinBalancer with the given initial backends.
func NewRoundRobin(backends []*Backend) *RoundRobinBalancer {
	entries := make([]rrEntry, len(backends))
	for i, b := range backends {
		entries[i] = rrEntry{b: b}
	}
	return &RoundRobinBalancer{entries: entries}
}

// AddBackend adds b to the round-robin pool.
func (r *RoundRobinBalancer) AddBackend(b *Backend) {
	r.mu.Lock()
	r.entries = append(r.entries, rrEntry{b: b})
	r.mu.Unlock()
}

// RemoveBackend removes the backend with the given address from the pool.
func (r *RoundRobinBalancer) RemoveBackend(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.entries[:0]
	for _, e := range r.entries {
		if e.b.Addr != addr {
			out = append(out, e)
		}
	}
	r.entries = out
}

// Pick selects the next backend using smooth weighted round-robin.
// The key is ignored; every request is treated as stateless.
func (r *RoundRobinBalancer) Pick(_ context.Context, _ string) (*Backend, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var totalWeight int64
	for i := range r.entries {
		if r.entries[i].b.IsHealthy() {
			totalWeight += int64(r.entries[i].b.Weight)
		}
	}
	if totalWeight == 0 {
		return nil, noHealthyErr("round-robin")
	}

	// Phase 1: add each backend's configured weight to its currentWeight.
	for i := range r.entries {
		if r.entries[i].b.IsHealthy() {
			r.entries[i].currentWeight += int64(r.entries[i].b.Weight)
		}
	}

	// Phase 2: select the entry with the highest currentWeight.
	// Ties are broken by position (earlier-added backend wins).
	best := -1
	for i := range r.entries {
		if !r.entries[i].b.IsHealthy() {
			continue
		}
		if best == -1 || r.entries[i].currentWeight > r.entries[best].currentWeight {
			best = i
		}
	}
	if best == -1 {
		return nil, noHealthyErr("round-robin")
	}

	// Phase 3: subtract totalWeight from the winner to rebalance.
	r.entries[best].currentWeight -= totalWeight
	b := r.entries[best].b
	b.activeConns.Add(1)
	b.totalReqs.Add(1)
	return b, nil
}

// Release decrements the active connection count for b.
func (r *RoundRobinBalancer) Release(b *Backend) {
	b.activeConns.Add(-1)
}
```

`Pick` reads `totalWeight` first and returns `ErrNoHealthyBackend` when no backend is healthy, so the three phases never run on an empty pool. `RemoveBackend` filters in place with `r.entries[:0]`, reusing the backing array, and `AddBackend` appends a fresh `rrEntry` with `currentWeight` zero — a new backend joins the rotation without disturbing the others' accumulated weights.

### The runnable demo

The demo routes eight picks over a 1:2:1 pool so the proportional 2:4:2 split is visible, then exercises dynamic membership by adding a fourth backend and removing it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	balancer "example.com/weighted-round-robin"
)

func main() {
	backends := []*balancer.Backend{
		balancer.NewBackend("10.0.0.1:8080", 1),
		balancer.NewBackend("10.0.0.2:8080", 2),
		balancer.NewBackend("10.0.0.3:8080", 1),
	}
	for _, b := range backends {
		b.SetHealthy(true)
	}
	ctx := context.Background()

	fmt.Println("--- Smooth Weighted Round-Robin (weights 1:2:1) ---")
	rr := balancer.NewRoundRobin(backends)
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		b, err := rr.Pick(ctx, "")
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		fmt.Printf("  pick %d: %s\n", i+1, b.Addr)
		counts[b.Addr]++
		rr.Release(b)
	}
	fmt.Printf("  totals: 10.0.0.1=%d 10.0.0.2=%d 10.0.0.3=%d\n",
		counts["10.0.0.1:8080"], counts["10.0.0.2:8080"], counts["10.0.0.3:8080"])

	fmt.Println()
	fmt.Println("--- Dynamic membership ---")
	newB := balancer.NewBackend("10.0.0.4:8080", 1)
	newB.SetHealthy(true)
	rr.AddBackend(newB)
	fmt.Println("  added 10.0.0.4:8080")
	b, _ := rr.Pick(ctx, "")
	fmt.Printf("  next pick: %s\n", b.Addr)
	rr.Release(b)
	rr.RemoveBackend("10.0.0.4:8080")
	fmt.Println("  removed 10.0.0.4:8080")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
--- Smooth Weighted Round-Robin (weights 1:2:1) ---
  pick 1: 10.0.0.2:8080
  pick 2: 10.0.0.1:8080
  pick 3: 10.0.0.3:8080
  pick 4: 10.0.0.2:8080
  pick 5: 10.0.0.2:8080
  pick 6: 10.0.0.1:8080
  pick 7: 10.0.0.3:8080
  pick 8: 10.0.0.2:8080
  totals: 10.0.0.1=2 10.0.0.2=4 10.0.0.3=2

--- Dynamic membership ---
  added 10.0.0.4:8080
  next pick: 10.0.0.2:8080
  removed 10.0.0.4:8080
```

### Tests

The tests pin every property smooth WRR must have. Equal weights must strictly alternate; weights 3:1 must deliver exactly 300:100 over 400 picks; one cycle of 3:1 must spread as a,a,b,a (the observable difference from naive WRR); an unhealthy backend must receive nothing; an all-unhealthy pool must return `ErrNoHealthyBackend`; `AddBackend`/`RemoveBackend` must change the live rotation; and a 50-goroutine race test must leave the request total exact and every active-connection count at zero. The `Example` is auto-verified by `go test` against its `// Output:` block.

Create `roundrobin_test.go`:

```go
package balancer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestRoundRobinEqualWeights(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 1)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)

	lb := NewRoundRobin([]*Backend{a, b})
	ctx := context.Background()

	// Smooth WRR with equal weights alternates deterministically: a, b, a, b.
	want := []string{"a:80", "b:80", "a:80", "b:80"}
	for i, addr := range want {
		got, err := lb.Pick(ctx, "")
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		if got.Addr != addr {
			t.Errorf("Pick %d: got %s, want %s", i, got.Addr, addr)
		}
		lb.Release(got)
	}
}

func TestRoundRobinWeightedDistribution(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 3)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)

	lb := NewRoundRobin([]*Backend{a, b})
	ctx := context.Background()

	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		picked, err := lb.Pick(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		counts[picked.Addr]++
		lb.Release(picked)
	}

	// With weights 3:1, smooth WRR delivers exactly 300:100 over 400 picks.
	if counts["a:80"] != 300 || counts["b:80"] != 100 {
		t.Errorf("distribution: a=%d b=%d, want a=300 b=100", counts["a:80"], counts["b:80"])
	}
}

func TestRoundRobinSmoothSpread(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 3)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)

	lb := NewRoundRobin([]*Backend{a, b})
	ctx := context.Background()

	// One cycle of weights 3:1 under smooth WRR is a, a, b, a: the light backend
	// is interleaved at position 3 instead of bunched at the end (naive WRR would
	// produce a, a, a, b, a run of three before b). This exact sequence is the
	// observable difference between smooth and naive weighted round-robin.
	want := []string{"a:80", "a:80", "b:80", "a:80"}
	for i, addr := range want {
		got, err := lb.Pick(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if got.Addr != addr {
			t.Errorf("pick %d: got %s, want %s (cycle %v)", i, got.Addr, addr, want)
		}
		lb.Release(got)
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 1)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	// b is unhealthy; it must receive no traffic.

	lb := NewRoundRobin([]*Backend{a, b})
	ctx := context.Background()

	for i := 0; i < 10; i++ {
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

func TestRoundRobinAllUnhealthy(t *testing.T) {
	t.Parallel()

	lb := NewRoundRobin([]*Backend{NewBackend("a:80", 1)})
	_, err := lb.Pick(context.Background(), "")
	if !errors.Is(err, ErrNoHealthyBackend) {
		t.Errorf("err = %v, want ErrNoHealthyBackend", err)
	}
}

func TestRoundRobinAddRemove(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 1)
	a.SetHealthy(true)
	lb := NewRoundRobin([]*Backend{a})
	ctx := context.Background()

	// Add b; both backends should now receive traffic.
	b := NewBackend("b:80", 1)
	b.SetHealthy(true)
	lb.AddBackend(b)

	got := map[string]int{}
	for i := 0; i < 4; i++ {
		picked, err := lb.Pick(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		got[picked.Addr]++
		lb.Release(picked)
	}
	if got["a:80"] == 0 || got["b:80"] == 0 {
		t.Errorf("expected both backends to receive traffic after AddBackend: %v", got)
	}

	// Remove a; only b should receive traffic.
	lb.RemoveBackend("a:80")
	for i := 0; i < 4; i++ {
		picked, err := lb.Pick(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if picked.Addr != "b:80" {
			t.Errorf("after RemoveBackend(a): pick %d = %s, want b:80", i, picked.Addr)
		}
		lb.Release(picked)
	}
}

func TestRoundRobinConcurrent(t *testing.T) {
	t.Parallel()

	backends := make([]*Backend, 3)
	for i := range backends {
		backends[i] = NewBackend(fmt.Sprintf("host%d:80", i), 1)
		backends[i].SetHealthy(true)
	}

	lb := NewRoundRobin(backends)
	ctx := context.Background()

	const goroutines = 50
	const picksEach = 100

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

	// Every Pick incremented totalReqs exactly once; the sum must be exact.
	var total int64
	for _, b := range backends {
		total += b.TotalReqs()
		if c := b.ActiveConns(); c != 0 {
			t.Errorf("backend %s: active connections = %d after release, want 0", b.Addr, c)
		}
	}
	if want := int64(goroutines * picksEach); total != want {
		t.Errorf("total requests = %d, want %d", total, want)
	}
}

func ExampleNewRoundRobin() {
	a := NewBackend("10.0.0.1:8080", 1)
	b := NewBackend("10.0.0.2:8080", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)

	lb := NewRoundRobin([]*Backend{a, b})
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		picked, _ := lb.Pick(ctx, "")
		fmt.Println(picked.Addr)
		lb.Release(picked)
	}
	// Output:
	// 10.0.0.1:8080
	// 10.0.0.2:8080
	// 10.0.0.1:8080
	// 10.0.0.2:8080
}
```

## Review

The algorithm is correct when the distribution is exact and burst-free at once: weights 3:1 must produce precisely 300:100 over 400 picks, and the first cycle must read a,a,b,a rather than a,a,a,b. If the ratio is right but the spread is bunched, the three phases ran in the wrong order — the subtract must use the total healthy weight, and the winner is the maximum `currentWeight`, not the maximum configured weight. The single most common concurrency mistake here is giving `Pick` a read lock so callers can run in parallel; because `Pick` writes `currentWeight` every call, that is a data race the `-race` build flags immediately, and the fix is the exclusive `sync.Mutex` this module uses. Confirm an unhealthy backend receives zero traffic, an all-unhealthy pool returns `ErrNoHealthyBackend` (checked with `errors.Is`, not string matching), and the concurrent test leaves the request total exact and every active-connection count at zero — together with the race detector that establishes the lock discipline is sound.

## Resources

- [Smooth weighted round-robin balancing — nginx commit 27e94984](https://github.com/phusion/nginx/commit/27e94984486058d73157038f7950a0a36ecc6e35) — the canonical reference for the three-phase WRR algorithm implemented here.
- [`sync` package — pkg.go.dev](https://pkg.go.dev/sync) — `sync.Mutex` semantics and why an exclusive lock is required when `Pick` mutates shared state.
- [`sync/atomic` package — pkg.go.dev](https://pkg.go.dev/sync/atomic) — `atomic.Bool` and `atomic.Int64`, the lock-free counters on `Backend`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-least-connections.md](02-least-connections.md)
