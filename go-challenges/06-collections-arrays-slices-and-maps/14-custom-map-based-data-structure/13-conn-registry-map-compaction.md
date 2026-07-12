# Exercise 13: Connection Registry With Periodic Map Compaction

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A stateful L4 proxy, in the shape of Envoy's connection tracking, keeps a live
set of every open connection it is forwarding: an ID goes in when the
connection opens and comes out when it closes. Under sustained traffic that
set churns constantly -- thousands of connections open and close every
minute, and the natural way to track them is exactly what it sounds like, a
`map[string]struct{}` with `Register` and `Unregister` around it. The proxy
runs for weeks without restarting. Its steady-state connection count might be
a thousand, but at yesterday's traffic spike it briefly held a hundred
thousand. The operator watching the process's memory graph a week later sees
it still sized for that spike, long after every one of those connections
closed.

That is not a leak in the sense of a forgotten reference. Every entry that
was deleted really is gone from the map's logical contents, `Len()` reports
the true live count, and `range` never visits a stale key. The problem is
structural: a Go map's backing bucket array only grows, and `delete` never
shrinks it back down or returns it to the heap. A map that once held a
hundred thousand entries keeps that footprint allocated forever, even after
every single one of those entries is deleted, because nothing in the
language or runtime ever triggers a shrink. A registry that only calls
`delete` -- which is what `Register`/`Unregister` looks like it should be,
and is the version that ships first -- carries its peak-traffic memory for
the rest of the process's life.

The cure is not a smarter delete; there isn't one. It is periodically
rebuilding into a fresh map sized for what is actually still live, and
letting the old map, bucket array included, become garbage. This module
builds that as a package: a `Registry` that tracks accumulated deletes
against the number of currently live connections and, once that churn
crosses a configured ratio, rebuilds via `maps.Copy` into a map sized for
exactly what remains -- with a `Compactions` counter that proves the rebuild
actually happened, not merely that the algorithm on paper says it should
have.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
connregistry/            module example.com/connregistry
  go.mod                 go 1.24
  connregistry.go        Registry; New, Register, Unregister, Len, Compactions
  connregistry_test.go   idempotency, exact-threshold table, the no-compaction contrast,
                         concurrency, ExampleRegistry_Unregister
```

- Files: `connregistry.go`, `connregistry_test.go`.
- Implement: `New(churnRatio float64) (*Registry, error)` rejecting a non-positive ratio with `ErrInvalidChurnRatio`; `(*Registry) Register(id string)`, idempotent on a repeated id; `(*Registry) Unregister(id string)`, a no-op on an unknown id, that rebuilds the backing map via `maps.Copy` into a fresh map once accumulated deletes exceed `churnRatio` times the live count; `(*Registry) Len() int`; `(*Registry) Compactions() int`.
- Test: `Register` is idempotent; `Unregister` on an unknown id neither changes `Len` nor counts toward churn; `New` rejects a non-positive ratio via `errors.Is`; a small hand-traceable scenario pins the exact compaction count at each step; the unexported `registryNoCompact` contrast shows a delete-only registry's backing map identity never changing under heavy churn while `Registry`'s does, with `Compactions()` proving it; `Registry` is safe for concurrent use; and `ExampleRegistry_Unregister` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### A map that only shrinks logically never shrinks physically

`delete(m, k)` removes `k` from `m`'s logical contents immediately: `len(m)`
drops, `range` stops visiting it, `m[k]` reports absent. None of that touches
the bucket array the runtime allocated to hold `m`'s entries. The array was
sized to accommodate the map's peak occupancy, and nothing about deleting
individual keys ever triggers the runtime to reallocate a smaller one --
there is no rebalancing, no compaction, no shrink-on-delete anywhere in the
map implementation. The version that ships first looks entirely reasonable:

```go
// registryNoCompact — deletes are correct, but the map itself never shrinks.
type registryNoCompact struct {
    conns map[string]struct{}
}

func (r *registryNoCompact) register(id string)   { r.conns[id] = struct{}{} }
func (r *registryNoCompact) unregister(id string) { delete(r.conns, id) }
```

Every operation this type performs is individually correct. `register` adds
an entry; `unregister` removes one; `len(r.conns)` at any moment is exactly
the number of connections currently open. What is missing is anything that
ever creates a *new* map: `r.conns` is the same map value, backed by the
same bucket array, from the first `register` call through however many
millions of open/close cycles the proxy handles before it is finally
restarted. The array grows to match every traffic spike it ever sees and
never gives any of that back.

There are exactly two cures for an unbounded map, and this module is about
the second one: bound the size with an eviction policy (the subject of the
LRU and TTL cache exercises elsewhere in this lesson), or periodically
rebuild into a fresh map so the old bucket array becomes garbage. `Registry`
takes the rebuild approach, because a connection tracker has no natural
eviction policy -- every live connection genuinely needs to stay tracked
until it closes -- so the thing to bound is not which entries survive, but
how long a shrunk-but-not-rebuilt map is allowed to carry its old capacity.

Create `connregistry.go`:

```go
// Package connregistry tracks the set of currently open connection IDs for
// a stateful L4 proxy, in the shape of Envoy's connection tracking, under
// constant register/unregister churn.
//
// A Go map never returns its backing bucket array to the heap on delete, so
// a registry that only deletes keeps its peak footprint forever even after
// every connection it ever saw has closed. Registry periodically rebuilds
// into a fresh map once accumulated deletes cross a configured churn ratio,
// reclaiming that footprint instead of carrying it for the life of the
// process.
package connregistry

import (
	"errors"
	"fmt"
	"maps"
	"sync"
)

// ErrInvalidChurnRatio is returned by New when the requested churn ratio is
// not positive.
var ErrInvalidChurnRatio = errors.New("connregistry: churn ratio must be positive")

// Registry tracks a set of live connection IDs and periodically rebuilds
// its backing map to reclaim the bucket array that deleted entries leave
// behind.
//
// Registry is safe for concurrent use by multiple goroutines: every method
// takes the internal lock for the duration of its map access, which is the
// realistic shape of a connection tracker serving many goroutines each
// opening and closing their own connections.
type Registry struct {
	mu          sync.Mutex
	conns       map[string]struct{}
	churnRatio  float64
	deletes     int
	compactions int
}

// New returns a Registry that rebuilds its backing map once the number of
// deletes accumulated since the last rebuild exceeds churnRatio times the
// number of currently live entries. A churnRatio of 1.0 means "rebuild once
// you have deleted as many entries as are currently live"; a smaller ratio
// rebuilds more eagerly, a larger one more lazily. New returns
// ErrInvalidChurnRatio if churnRatio is not positive.
func New(churnRatio float64) (*Registry, error) {
	if churnRatio <= 0 {
		return nil, fmt.Errorf("%w: got %v", ErrInvalidChurnRatio, churnRatio)
	}
	return &Registry{
		conns:      make(map[string]struct{}),
		churnRatio: churnRatio,
	}, nil
}

// Register adds id to the set of live connections. Registering an id that
// is already present is a no-op.
func (r *Registry) Register(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[id] = struct{}{}
}

// Unregister removes id from the set of live connections. Unregistering an
// id that is not present is a no-op and does not count toward the churn
// ratio: only an actual deletion moves the registry toward a rebuild.
//
// After a deletion, Unregister checks whether accumulated deletes since the
// last rebuild have exceeded churnRatio times the number of entries still
// live, and if so rebuilds into a fresh map sized for exactly what remains,
// reclaiming the old map's bucket array.
func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.conns[id]; !ok {
		return
	}
	delete(r.conns, id)
	r.deletes++
	r.compactIfChurned()
}

// compactIfChurned rebuilds r.conns into a freshly allocated map, sized for
// its current contents, once r.deletes exceeds churnRatio times the live
// count. The caller must hold r.mu.
func (r *Registry) compactIfChurned() {
	threshold := r.churnRatio * float64(len(r.conns))
	if float64(r.deletes) <= threshold {
		return
	}
	fresh := make(map[string]struct{}, len(r.conns))
	maps.Copy(fresh, r.conns)
	r.conns = fresh
	r.deletes = 0
	r.compactions++
}

// Len reports the number of currently live connections.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

// Compactions reports how many times Registry has rebuilt its backing map
// to reclaim the bucket array left behind by deleted entries.
func (r *Registry) Compactions() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.compactions
}
```

### Using it

Construct one `Registry` per proxy worker with `New(churnRatio)`, then call
`Register`/`Unregister` from wherever connections open and close. The
churn ratio is the knob: a small ratio (say `0.25`) rebuilds aggressively and
keeps the bucket array close to the live set at the cost of more frequent
copies; a large ratio rebuilds rarely and tolerates more slack capacity
between rebuilds. `Register` and `Unregister` both take the internal lock
for their full duration, so the type's doc comment's concurrency promise --
safe for concurrent use -- holds even while one goroutine's `Unregister`
triggers a rebuild that another goroutine's `Register` must not observe
half-finished.

The `Example` below is the runnable demonstration of this module: `go test`
executes it and compares its standard output against the `// Output:`
comment, so the usage shown here cannot drift from the code that actually
runs.

```go
func ExampleRegistry_Unregister() {
	r, err := New(1.0)
	if err != nil {
		panic(err)
	}

	for _, id := range ids("conn", 4) {
		r.Register(id)
	}
	fmt.Println("live connections:", r.Len())

	for _, id := range ids("conn", 4) {
		r.Unregister(id)
	}
	fmt.Println("live connections after full churn:", r.Len())
	fmt.Println("compactions performed:", r.Compactions())

	// Output:
	// live connections: 4
	// live connections after full churn: 0
	// compactions performed: 2
}
```

### Tests

`TestRegisterIsIdempotent` and `TestUnregisterUnknownIsNoop` pin the two
edge cases at the API's boundary: a repeated `Register` does not inflate
`Len`, and an `Unregister` on an id that was never registered neither shrinks
`Len` nor counts toward the churn ratio. `TestNewRejectsNonPositiveChurnRatio`
checks the sentinel with `errors.Is`. `TestCompactionTriggersAtExactThreshold`
hand-traces a small scenario -- four connections, unregistered one at a
time, ratio `1.0` -- and asserts the exact `Compactions()` value after each
step; this is safe to pin exactly because the trigger is this package's own
deterministic arithmetic, not the runtime's unspecified append growth curve.

`TestCompactionReplacesBackingMap` is the heart of the module. It runs the
identical register/unregister sequence through both `registryNoCompact`
(unexported, unreachable from the package API) and `Registry`, and compares
the backing map's identity -- the address of its runtime header, via
`reflect.ValueOf(m).Pointer()` -- before and after heavy churn.
`registryNoCompact`'s identity never changes: it is the one map, one bucket
array, for its entire life. `Registry`'s does change, and `Compactions()`
being nonzero proves a rebuild actually ran rather than merely being implied
by the threshold math. `TestRegistryIsSafeForConcurrentUse` runs ten
goroutines each registering and immediately unregistering their own
connections under `-race`.

Create `connregistry_test.go`:

```go
package connregistry

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// registryNoCompact is the registry as it is usually written the first
// time: it registers and deletes connection IDs and nothing else. It is
// never exported and never reachable from the package API; it exists only
// so the tests can pin what it gets wrong -- because a Go map never
// returns its bucket array to the heap on delete, the map value it holds
// is the exact same one, with the exact same backing array, from the first
// Register through the heaviest churn.
type registryNoCompact struct {
	conns map[string]struct{}
}

func newRegistryNoCompact() *registryNoCompact {
	return &registryNoCompact{conns: make(map[string]struct{})}
}

func (r *registryNoCompact) register(id string)   { r.conns[id] = struct{}{} }
func (r *registryNoCompact) unregister(id string) { delete(r.conns, id) }

// mapIdentity returns the address of the runtime map header backing m, so
// tests can observe whether a map variable was rebuilt into a genuinely
// new map value rather than merely mutated in place.
func mapIdentity(m map[string]struct{}) uintptr {
	return reflect.ValueOf(m).Pointer()
}

func ids(prefix string, n int) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return out
}

func TestRegisterIsIdempotent(t *testing.T) {
	t.Parallel()

	r, err := New(1.0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Register("conn-1")
	r.Register("conn-1")
	r.Register("conn-1")
	if got := r.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 after registering the same id three times", got)
	}
}

func TestUnregisterUnknownIsNoop(t *testing.T) {
	t.Parallel()

	r, err := New(1.0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.Register("conn-1")
	r.Unregister("never-registered")
	if got := r.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1; unregistering an unknown id must not touch conn-1", got)
	}
	if got := r.Compactions(); got != 0 {
		t.Fatalf("Compactions() = %d, want 0; a no-op unregister must not count toward churn", got)
	}
}

func TestNewRejectsNonPositiveChurnRatio(t *testing.T) {
	t.Parallel()

	for _, ratio := range []float64{0, -1, -0.5} {
		if _, err := New(ratio); !errors.Is(err, ErrInvalidChurnRatio) {
			t.Errorf("New(%v) error = %v, want ErrInvalidChurnRatio", ratio, err)
		}
	}
}

// TestCompactionTriggersAtExactThreshold pins the compaction rule against a
// small, fully hand-traceable scenario: with churnRatio 1.0, four
// registered connections, unregistered one at a time. The algorithm is
// entirely this package's own, deterministic arithmetic -- not the
// runtime's append growth curve -- so its exact trigger points are a valid,
// stable thing to assert.
func TestCompactionTriggersAtExactThreshold(t *testing.T) {
	t.Parallel()

	r, err := New(1.0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, id := range ids("conn", 4) {
		r.Register(id)
	}

	wantCompactionsAfter := []int{0, 0, 1, 2}
	for i, id := range ids("conn", 4) {
		r.Unregister(id)
		if got := r.Compactions(); got != wantCompactionsAfter[i] {
			t.Fatalf("after unregistering %s: Compactions() = %d, want %d", id, got, wantCompactionsAfter[i])
		}
	}
	if got := r.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0 after unregistering every connection", got)
	}
}

// TestCompactionReplacesBackingMap is the heart of the module. It contrasts
// registryNoCompact, which only ever deletes, against Registry, which
// rebuilds under churn, on the identical register/unregister sequence.
// registryNoCompact's map identity never changes -- the same bucket array
// lives on regardless of how many entries were deleted from it. Registry's
// does change, and Compactions() proves the rebuild actually happened
// rather than merely being implied by the algorithm on paper.
func TestCompactionReplacesBackingMap(t *testing.T) {
	t.Parallel()

	churn := ids("conn", 200)

	naive := newRegistryNoCompact()
	for _, id := range churn {
		naive.register(id)
	}
	naiveBefore := mapIdentity(naive.conns)
	for _, id := range churn[:180] {
		naive.unregister(id)
	}
	naiveAfter := mapIdentity(naive.conns)
	if naiveBefore != naiveAfter {
		t.Fatal("registryNoCompact's backing map identity changed; it is not supposed to rebuild at all")
	}

	r, err := New(0.5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, id := range churn {
		r.Register(id)
	}
	before := mapIdentity(r.conns)
	for _, id := range churn[:180] {
		r.Unregister(id)
	}
	after := mapIdentity(r.conns)
	if before == after {
		t.Fatal("Registry's backing map identity never changed under heavy churn; expected at least one rebuild")
	}
	if got := r.Compactions(); got == 0 {
		t.Fatal("Compactions() = 0; expected at least one rebuild under this churn pattern")
	}
	if got := r.Len(); got != 20 {
		t.Fatalf("Len() = %d, want 20 live connections after unregistering 180 of 200", got)
	}
}

func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	r, err := New(0.5)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for g := range 10 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 50 {
				id := fmt.Sprintf("worker-%d-conn-%d", g, i)
				r.Register(id)
				r.Unregister(id)
			}
		}(g)
	}
	wg.Wait()

	if got := r.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0; every registered id was also unregistered", got)
	}
}

// ExampleRegistry_Unregister is the runnable demonstration of this module:
// go test executes it and compares its stdout against the Output comment
// below.
func ExampleRegistry_Unregister() {
	r, err := New(1.0)
	if err != nil {
		panic(err)
	}

	for _, id := range ids("conn", 4) {
		r.Register(id)
	}
	fmt.Println("live connections:", r.Len())

	for _, id := range ids("conn", 4) {
		r.Unregister(id)
	}
	fmt.Println("live connections after full churn:", r.Len())
	fmt.Println("compactions performed:", r.Compactions())

	// Output:
	// live connections: 4
	// live connections after full churn: 0
	// compactions performed: 2
}
```

## Review

`Registry` is correct when a proxy that has been running for weeks, through
every traffic spike it ever saw, holds a map sized for its *current* load,
not its historical peak. The trap is that this is invisible from the
package's logical behavior alone: `registryNoCompact` gets `Len`, `Register`,
and `Unregister` exactly right and still leaks, because a Go map's bucket
array only ever grows and `delete` cannot shrink it back. The fix is not a
different delete; it is periodically discarding the old map entirely --
`maps.Copy` into a freshly sized one -- once accumulated deletes cross
`churnRatio` times the live count, which `compactIfChurned` checks after
every successful `Unregister`. `TestCompactionReplacesBackingMap` proves the
rebuild is real by comparing the backing map's runtime identity before and
after, not merely trusting that the threshold arithmetic implies it.
`Registry` guards its state with a `sync.Mutex`, so `Register` and
`Unregister` -- including whichever one happens to trigger a rebuild -- are
safe to call from many goroutines at once. Run `go test -count=1 -race ./...`
to confirm the idempotency and no-op edge cases, the exact-threshold table,
the no-compaction contrast, and the concurrent-use test.

## Resources

- [`maps.Copy`](https://pkg.go.dev/maps#Copy) — the standard-library copy `Registry` uses to rebuild into a fresh map.
- [Go Wiki: Map access is not concurrency-safe](https://go.dev/doc/faq#atomic_maps) — why `Registry` guards every access with a mutex.
- [`reflect.Value.Pointer`](https://pkg.go.dev/reflect#Value.Pointer) — the identity check the tests use to prove a rebuild actually replaced the map.
- [Go source: `runtime/map.go`](https://go.dev/src/runtime/map.go) — the implementation detail that `delete` never shrinks a map's bucket array.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-tenant-override-resolver.md](12-tenant-override-resolver.md) | Next: [14-conn-stats-pointer-map.md](14-conn-stats-pointer-map.md)
