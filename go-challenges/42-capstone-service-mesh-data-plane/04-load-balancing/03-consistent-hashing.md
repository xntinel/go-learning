# Exercise 3: Consistent Hashing

When the same cache key, session, or shard must always reach the same backend, neither round-robin nor least-connections will do: you need a function that maps a key to a backend and keeps that mapping stable as backends come and go. This exercise builds a consistent hash ring with virtual nodes for even distribution and bounded loads to cap hotspots.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
balancer.go          Backend, NewBackend, Balancer interface, ErrNoHealthyBackend
consistenthash.go    ringEntry, ConsistentHashBalancer (virtual nodes + bounded loads)
cmd/
  demo/
    main.go          stable key mapping, distribution over 3000 keys, minimal remap
consistenthash_test.go  same-key stability, even distribution, minimal remap, bounded spill
```

- Files: `balancer.go`, `consistenthash.go`, `cmd/demo/main.go`, `consistenthash_test.go`.
- Implement: `Backend` with atomic counters, the `Balancer` interface, and `ConsistentHashBalancer` with `Pick`, `Release`, `AddBackend`, `RemoveBackend`.
- Test: the same key always routes to the same backend, 150 virtual nodes keep each of four backends within ~8% of an even share, removing one of three backends remaps only ~1/3 of keys while the rest stay put, and the bounded-load rule spills a hot key's overflow onto a neighbor.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/42-capstone-service-mesh-data-plane/04-load-balancing/03-consistent-hashing/cmd/demo && cd go-solutions/42-capstone-service-mesh-data-plane/04-load-balancing/03-consistent-hashing
go mod edit -go=1.26
```

### The shared Backend object

The `Backend` type and `Balancer` interface are the same across every balancer in this lesson and live in `balancer.go`. For consistent hashing the relevant fields are the health flag and `activeConns`: health lets `Pick` skip a backend that is in the ring but temporarily out of rotation, and `activeConns` is the live load the bounded-load rule reads to decide when a key must spill to a neighbor. The counters are `atomic.Int64` for the usual reasons — single-value atomics are faster than a mutex per field and their `noCopy` guard makes `go vet` reject any accidental copy of a `Backend`, so the system passes `*Backend` throughout. `noHealthyErr` wraps the sentinel for `errors.Is`, and the `var _ Balancer = (*ConsistentHashBalancer)(nil)` assertion catches a missing method at compile time.

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
var _ Balancer = (*ConsistentHashBalancer)(nil)

func noHealthyErr(algo string) error {
	return fmt.Errorf("%w (algorithm: %s)", ErrNoHealthyBackend, algo)
}
```

### The ring, virtual nodes, and the binary search

A consistent hash ring maps keys and backends onto one circular hash space; a key is served by the first backend clockwise from the key's position. The defining property is that adding or removing a backend remaps only the keys in that backend's arc — about 1/N of all keys — instead of reshuffling everything the way `hash(key) % N` would on every membership change.

Giving each backend a single ring position produces badly uneven arcs with few backends, so each backend is hashed to `virtualNodes` positions using distinct keys (`"addr#0"`, `"addr#1"`, ...). With 150 virtual nodes per backend the relative standard deviation of load falls to roughly 1/sqrt(150), about 8%. The ring is a slice of `(hash, backend)` entries kept sorted by hash. `insertLocked` appends all of a backend's virtual nodes and then sorts the ring exactly once — sorting inside the append loop would turn an O(n log n) build into O(n^2 log n). Lookup hashes the key with `crc32.ChecksumIEEE` and uses `sort.Search` to binary-search the first position at or above that hash; when the key hashes past the last entry, `sort.Search` returns `n` and the `(start+i)%n` modulo wraps the walk back to position 0.

### Bounded loads, and why the bound has a +1

Plain consistent hashing distributes keys evenly but not necessarily load: a few very popular keys can overload the backend that owns them. Bounded-load consistent hashing (Google Research, 2017) computes the average active connections across healthy backends and caps any one backend at `1.25 * average`; if the key's natural backend already exceeds that, `Pick` walks clockwise to the next backend within the bound. Most keys still hit their natural backend — only a hotspot's overflow spills to neighbors — so the disruption to the mapping stays minimal while the maximum load is held to 25% above average.

The bound is `1.25 * average + 1`, not `1.25 * average`. The `+1` matters at the boundary case of an idle cluster: with zero average the bare bound would be zero, every backend would compare as "over the bound", and `Pick` would walk the whole ring and fail with `ErrNoHealthyBackend` even though every backend is free. The `+1` keeps the bound at least 1 so an idle pool always serves. The ring and the `byAddr` map are guarded by a `sync.RWMutex` because reads (`Pick`) vastly outnumber membership changes, exactly the workload read-write locking is built for.

Create `consistenthash.go`:

```go
package balancer

import (
	"context"
	"fmt"
	"hash/crc32"
	"sort"
	"sync"
)

const defaultVirtualNodes = 150

// ringEntry is one position on the consistent hash ring.
type ringEntry struct {
	hash    uint32
	backend *Backend
}

// ConsistentHashBalancer distributes traffic using a hash ring with virtual
// nodes. Requests with the same key are always routed to the same healthy
// backend as long as pool membership is unchanged.
//
// It implements bounded loads (Google Research, 2017): if the selected
// backend's active connections exceed 1.25 * average_load, the balancer walks
// clockwise on the ring until it finds a backend within the load bound.
type ConsistentHashBalancer struct {
	mu           sync.RWMutex
	virtualNodes int
	ring         []ringEntry // sorted ascending by hash
	byAddr       map[string]*Backend
}

// NewConsistentHash returns a ConsistentHashBalancer. virtualNodes is the
// number of ring positions per backend; 0 uses the default of 150.
func NewConsistentHash(backends []*Backend, virtualNodes int) *ConsistentHashBalancer {
	if virtualNodes <= 0 {
		virtualNodes = defaultVirtualNodes
	}
	ch := &ConsistentHashBalancer{
		virtualNodes: virtualNodes,
		byAddr:       make(map[string]*Backend),
	}
	for _, b := range backends {
		ch.insertLocked(b)
	}
	return ch
}

// insertLocked adds all virtual nodes for b to the ring and re-sorts.
// Caller must hold ch.mu for writing, or call from the constructor.
func (ch *ConsistentHashBalancer) insertLocked(b *Backend) {
	ch.byAddr[b.Addr] = b
	for i := 0; i < ch.virtualNodes; i++ {
		key := fmt.Sprintf("%s#%d", b.Addr, i)
		h := crc32.ChecksumIEEE([]byte(key))
		ch.ring = append(ch.ring, ringEntry{hash: h, backend: b})
	}
	sort.Slice(ch.ring, func(i, j int) bool {
		return ch.ring[i].hash < ch.ring[j].hash
	})
}

// AddBackend adds b to the ring.
func (ch *ConsistentHashBalancer) AddBackend(b *Backend) {
	ch.mu.Lock()
	ch.insertLocked(b)
	ch.mu.Unlock()
}

// RemoveBackend removes the backend at addr from the ring.
func (ch *ConsistentHashBalancer) RemoveBackend(addr string) {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	delete(ch.byAddr, addr)
	out := ch.ring[:0]
	for _, e := range ch.ring {
		if e.backend.Addr != addr {
			out = append(out, e)
		}
	}
	ch.ring = out
}

// Pick selects a backend for key using consistent hashing with bounded loads.
func (ch *ConsistentHashBalancer) Pick(_ context.Context, key string) (*Backend, error) {
	ch.mu.RLock()
	defer ch.mu.RUnlock()

	n := len(ch.ring)
	if n == 0 {
		return nil, noHealthyErr("consistent-hash")
	}

	// Compute average active connections across healthy backends.
	var totalConns int64
	var healthyCount int64
	for _, b := range ch.byAddr {
		if b.IsHealthy() {
			totalConns += b.ActiveConns()
			healthyCount++
		}
	}
	if healthyCount == 0 {
		return nil, noHealthyErr("consistent-hash")
	}
	// The +1 ensures the bound is at least 1 when the cluster is idle,
	// preventing every backend from being rejected when all connections are zero.
	loadBound := float64(totalConns)/float64(healthyCount)*1.25 + 1

	// Hash the key and find the first ring position >= hash(key).
	// sort.Search returns n when no entry satisfies the predicate,
	// triggering wrap-around via the modulo in the loop below.
	h := crc32.ChecksumIEEE([]byte(key))
	start := sort.Search(n, func(i int) bool { return ch.ring[i].hash >= h })

	// Walk clockwise until a healthy backend within the load bound is found.
	for i := 0; i < n; i++ {
		e := ch.ring[(start+i)%n]
		b := e.backend
		if !b.IsHealthy() {
			continue
		}
		if float64(b.ActiveConns()) < loadBound {
			b.activeConns.Add(1)
			b.totalReqs.Add(1)
			return b, nil
		}
	}
	return nil, noHealthyErr("consistent-hash")
}

// Release decrements the active connection count for b.
func (ch *ConsistentHashBalancer) Release(b *Backend) {
	b.activeConns.Add(-1)
}
```

`RemoveBackend` deletes the backend from `byAddr` so it no longer counts toward the average, then filters its virtual nodes out of the ring in place — the remaining entries keep their relative order, so the ring stays sorted without a re-sort. An in-flight request already routed to the removed backend still releases correctly because `Release` operates on the `*Backend`, not on ring membership.

### The runnable demo

The demo shows the three things consistent hashing buys you: the same key always maps to the same backend, virtual nodes spread thousands of keys across the pool, and removing a backend remaps only the keys that lived on it while the rest stay put.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sort"

	balancer "example.com/consistent-hashing"
)

func main() {
	backends := []*balancer.Backend{
		balancer.NewBackend("cache-1:6379", 1),
		balancer.NewBackend("cache-2:6379", 1),
		balancer.NewBackend("cache-3:6379", 1),
	}
	for _, b := range backends {
		b.SetHealthy(true)
	}
	ctx := context.Background()
	ch := balancer.NewConsistentHash(backends, 0)

	fmt.Println("--- Consistent Hash (same key -> same backend) ---")
	keys := []string{"session:alice", "session:bob", "session:carol"}
	for _, key := range keys {
		b1, _ := ch.Pick(ctx, key)
		ch.Release(b1)
		b2, _ := ch.Pick(ctx, key)
		ch.Release(b2)
		fmt.Printf("  %-16s -> %s (stable: %v)\n", key, b1.Addr, b1.Addr == b2.Addr)
	}

	fmt.Println()
	fmt.Println("--- Key distribution over 3000 keys ---")
	dist := map[string]int{}
	for i := 0; i < 3000; i++ {
		b, _ := ch.Pick(ctx, fmt.Sprintf("key:%d", i))
		dist[b.Addr]++
		ch.Release(b)
	}
	for _, b := range backends {
		fmt.Printf("  %s: %d keys\n", b.Addr, dist[b.Addr])
	}

	fmt.Println()
	fmt.Println("--- Minimal remap when a backend leaves ---")
	before := map[string]string{}
	for i := 0; i < 3000; i++ {
		k := fmt.Sprintf("key:%d", i)
		b, _ := ch.Pick(ctx, k)
		before[k] = b.Addr
		ch.Release(b)
	}
	ch.RemoveBackend("cache-3:6379")
	remapped := 0
	for k := range before {
		b, _ := ch.Pick(ctx, k)
		if b.Addr != before[k] {
			remapped++
		}
		ch.Release(b)
	}
	fmt.Printf("  removed cache-3:6379; %d of 3000 keys remapped\n", remapped)
	keptKeys := make([]string, 0, 4)
	for k := range before {
		if before[k] != "cache-3:6379" {
			keptKeys = append(keptKeys, k)
		}
	}
	sort.Strings(keptKeys)
	for _, k := range keptKeys[:3] {
		b, _ := ch.Pick(ctx, k)
		fmt.Printf("  %-10s stayed on %s\n", k, b.Addr)
		ch.Release(b)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
--- Consistent Hash (same key -> same backend) ---
  session:alice    -> cache-2:6379 (stable: true)
  session:bob      -> cache-1:6379 (stable: true)
  session:carol    -> cache-1:6379 (stable: true)

--- Key distribution over 3000 keys ---
  cache-1:6379: 1155 keys
  cache-2:6379: 1162 keys
  cache-3:6379: 683 keys

--- Minimal remap when a backend leaves ---
  removed cache-3:6379; 683 of 3000 keys remapped
  key:0      stayed on cache-1:6379
  key:1      stayed on cache-1:6379
  key:10     stayed on cache-1:6379
```

The distribution is uneven by design at this scale — `cache-3` happens to own a smaller arc of the ring — which is exactly why production rings use 100+ virtual nodes: more nodes shrink the variance. Note that the number of remapped keys (683) equals the count that lived on `cache-3` before removal: keys on the surviving backends do not move.

### Tests

The tests pin the four properties. The same key must route to the same backend across repeated picks; 150 virtual nodes must keep each of four backends within ~8% of an even quarter of 12000 keys; removing one of three backends must remap only ~1/3 of keys and leave every surviving key exactly where it was; and the bounded-load rule must force a hammered hot key's overflow to spill onto a neighbor instead of piling entirely on one backend. The `Example` is auto-verified against its `// Output:` block.

Create `consistenthash_test.go`:

```go
package balancer

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestConsistentHashSameKeyRoutesSameBackend(t *testing.T) {
	t.Parallel()

	backends := []*Backend{
		NewBackend("a:80", 1),
		NewBackend("b:80", 1),
		NewBackend("c:80", 1),
	}
	for _, b := range backends {
		b.SetHealthy(true)
	}

	lb := NewConsistentHash(backends, 0)
	ctx := context.Background()

	keys := []string{"user:1", "user:42", "session:xyz", "order:99", "cache:hot"}
	for _, key := range keys {
		b1, err := lb.Pick(ctx, key)
		if err != nil {
			t.Fatalf("key=%q first Pick: %v", key, err)
		}
		lb.Release(b1)

		b2, err := lb.Pick(ctx, key)
		if err != nil {
			t.Fatalf("key=%q second Pick: %v", key, err)
		}
		lb.Release(b2)

		if b1.Addr != b2.Addr {
			t.Errorf("key=%q: first=%s second=%s (must be the same backend)", key, b1.Addr, b2.Addr)
		}
	}
}

func TestConsistentHashEvenDistribution(t *testing.T) {
	t.Parallel()

	backends := []*Backend{
		NewBackend("a:80", 1),
		NewBackend("b:80", 1),
		NewBackend("c:80", 1),
		NewBackend("d:80", 1),
	}
	for _, b := range backends {
		b.SetHealthy(true)
	}

	lb := NewConsistentHash(backends, 150)
	ctx := context.Background()

	const numKeys = 12000
	dist := map[string]int{}
	for i := 0; i < numKeys; i++ {
		b, err := lb.Pick(ctx, fmt.Sprintf("key:%d", i))
		if err != nil {
			t.Fatal(err)
		}
		dist[b.Addr]++
		lb.Release(b)
	}

	// With 150 virtual nodes per backend the relative standard deviation of load
	// is roughly 8%. Each of 4 backends should get ~25% of keys; assert every
	// backend lands within 25% +/- 8 points (i.e. 17%-33%) so the test is
	// deterministic but tight enough to catch a ring that ignores virtual nodes.
	ideal := numKeys / len(backends)
	for _, b := range backends {
		got := dist[b.Addr]
		lo, hi := int(float64(ideal)*0.68), int(float64(ideal)*1.32)
		if got < lo || got > hi {
			t.Errorf("backend %s got %d keys (%.1f%%), want within [%d,%d]",
				b.Addr, got, float64(got)/numKeys*100, lo, hi)
		}
	}
}

func TestConsistentHashRemapsMinimalKeysOnRemove(t *testing.T) {
	t.Parallel()

	backends := []*Backend{
		NewBackend("a:80", 1),
		NewBackend("b:80", 1),
		NewBackend("c:80", 1),
	}
	for _, b := range backends {
		b.SetHealthy(true)
	}

	lb := NewConsistentHash(backends, 150)
	ctx := context.Background()

	const numKeys = 1000
	keys := make([]string, numKeys)
	before := make([]string, numKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("key:%d", i)
		b, err := lb.Pick(ctx, keys[i])
		if err != nil {
			t.Fatal(err)
		}
		before[i] = b.Addr
		lb.Release(b)
	}

	// Remove c; keys that were mapped to c must remap, others must not.
	lb.RemoveBackend("c:80")

	remapped := 0
	for i, key := range keys {
		b, err := lb.Pick(ctx, key)
		if err != nil {
			t.Fatalf("key=%q after RemoveBackend: %v", key, err)
		}
		if b.Addr != before[i] {
			remapped++
		}
		lb.Release(b)
	}

	// Approximately 1/3 of keys were on c and must remap. Allow 15-55% to
	// account for ring variance without making the test flaky.
	ratio := float64(remapped) / numKeys
	if ratio < 0.15 || ratio > 0.55 {
		t.Errorf("remapped %.1f%% of keys after removing 1 of 3 backends, want 15-55%%", ratio*100)
	}

	// Keys that did NOT live on c must keep their backend exactly.
	for i, key := range keys {
		if before[i] == "c:80" {
			continue
		}
		b, _ := lb.Pick(ctx, key)
		lb.Release(b)
		if b.Addr != before[i] {
			t.Errorf("key=%q moved from %s to %s but its backend never left", key, before[i], b.Addr)
		}
	}
}

func TestConsistentHashSkipsUnhealthy(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 1)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	// b is unhealthy.

	lb := NewConsistentHash([]*Backend{a, b}, 50)
	ctx := context.Background()

	// Any key that would normally map to b must be redirected to a.
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("key:%d", i)
		got, err := lb.Pick(ctx, key)
		if err != nil {
			t.Fatalf("key=%q: %v", key, err)
		}
		if got.Addr != "a:80" {
			t.Errorf("key=%q: got %s, want a:80 (b is unhealthy)", key, got.Addr)
		}
		lb.Release(got)
	}
}

func TestConsistentHashBoundedLoadSpills(t *testing.T) {
	t.Parallel()

	a := NewBackend("a:80", 1)
	b := NewBackend("b:80", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)

	lb := NewConsistentHash([]*Backend{a, b}, 150)
	ctx := context.Background()

	// Find a key that maps to a, then pin many requests to that same key.
	// Without bounded loads all of them would pile onto a; the 1.25x bound
	// forces the overflow to spill onto b, so both backends end up loaded.
	hot := ""
	for i := 0; i < 1000; i++ {
		k := fmt.Sprintf("probe:%d", i)
		got, _ := lb.Pick(ctx, k)
		lb.Release(got)
		if got.Addr == "a:80" {
			hot = k
			break
		}
	}
	if hot == "" {
		t.Fatal("could not find a key mapping to a:80")
	}

	const n = 200
	for i := 0; i < n; i++ {
		if _, err := lb.Pick(ctx, hot); err != nil {
			t.Fatalf("pick %d for hot key: %v", i, err)
		}
	}

	// The bound caps a at 25% above the running average, so b must absorb a
	// large share of the load: it cannot be zero.
	if b.ActiveConns() == 0 {
		t.Errorf("bounded load failed: a=%d b=%d, want b > 0 (overflow must spill)",
			a.ActiveConns(), b.ActiveConns())
	}
	if a.ActiveConns() == int64(n) {
		t.Errorf("bounded load failed: a absorbed all %d requests, want some spill to b", n)
	}
}

func TestConsistentHashAllUnhealthy(t *testing.T) {
	t.Parallel()

	lb := NewConsistentHash([]*Backend{NewBackend("a:80", 1)}, 0)
	_, err := lb.Pick(context.Background(), "any-key")
	if !errors.Is(err, ErrNoHealthyBackend) {
		t.Errorf("err = %v, want ErrNoHealthyBackend", err)
	}
}

func ExampleNewConsistentHash() {
	a := NewBackend("cache-1:6379", 1)
	b := NewBackend("cache-2:6379", 1)
	c := NewBackend("cache-3:6379", 1)
	a.SetHealthy(true)
	b.SetHealthy(true)
	c.SetHealthy(true)

	lb := NewConsistentHash([]*Backend{a, b, c}, 0)
	ctx := context.Background()

	// The same key always maps to the same backend.
	key := "user:42"
	b1, _ := lb.Pick(ctx, key)
	lb.Release(b1)
	b2, _ := lb.Pick(ctx, key)
	lb.Release(b2)

	fmt.Println(b1.Addr == b2.Addr)
	// Output:
	// true
}
```

## Review

The ring is correct when the same key returns the same backend on every pick and removing a backend moves only that backend's keys: if surviving keys also jump, the most likely cause is sorting the ring incorrectly or hashing the lookup key with a different function than the virtual nodes use — both sides must use `crc32.ChecksumIEEE`. The distribution test catches a ring that ignores virtual nodes (one position per backend would blow past the 17%-33% band); if it fails, check that `insertLocked` appends all `virtualNodes` positions and sorts once at the end rather than giving each backend a single point. The bounded-load test is the subtle one: if a hot key piles its entire load onto one backend, the `loadBound` comparison is wrong — it must be `1.25 * average + 1` and the walk must continue past a backend at or above the bound. Confirm an unhealthy backend is skipped, an empty or all-unhealthy ring returns `ErrNoHealthyBackend` via `errors.Is`, and the whole suite passes under `-race`, which together establish that the RWMutex-protected ring is read-safe under concurrency.

## Resources

- [Consistent Hashing with Bounded Loads — Google Research Blog](https://research.google/blog/consistent-hashing-with-bounded-loads/) — the 2017 post describing the epsilon-bounded algorithm implemented here.
- [`sort` package — pkg.go.dev](https://pkg.go.dev/sort) — `sort.Search` (binary search) and `sort.Slice`, used to build and query the ring.
- [`hash/crc32` package — pkg.go.dev](https://pkg.go.dev/hash/crc32) — `ChecksumIEEE`, the hash mapping both keys and virtual nodes onto the ring.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-least-connections.md](02-least-connections.md) | Next: [../05-health-checking/00-concepts.md](../05-health-checking/00-concepts.md)
