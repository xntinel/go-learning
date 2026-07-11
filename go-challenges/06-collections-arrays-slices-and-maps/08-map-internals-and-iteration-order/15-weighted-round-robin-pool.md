# Exercise 15: Weighted Round-Robin Backend Pool -- Map Lookup, Stable Pick Order

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A client-side load balancer -- the shape both gRPC's and Envoy's
round-robin policy take -- holds a set of backend addresses, each with a
configured weight, and on every outgoing request picks the next one in a
rotation that is supposed to send traffic to each backend in proportion to
its weight. Two things have to be true of the data structure underneath
it: resolving a backend's weight has to be O(1), because it happens on
every request, and the *sequence* of picks has to be stable and fair over
time, because that sequence is the load balancing. A `map[string]int` gives
you the first property for free. It does not, and structurally cannot,
give you the second.

The trap is that a map looks like it should be able to do both. `for addr
:= range pool.backends { return addr }` compiles, it returns a real,
registered backend, and reading it in isolation looks like "pick one
backend from the pool" -- which is exactly what a round-robin `Next()`
sounds like it should do. What it actually returns is whatever the current
`range` clause's randomized starting position happens to land on first, and
Go randomizes that starting position on every single `range` statement, not
once per map. There is no rotation here at all: no memory of what was
picked last, no relationship between one call's answer and the next's, and
critically, no connection whatsoever to the weight values sitting in the
map next to each key. A backend configured to receive the overwhelming
majority of traffic gets exactly the same treatment as one configured for a
sliver of it, and the load balancer silently sends traffic in whatever
proportion the runtime's internal hash-table layout happens to produce
that run -- not the proportion an operator configured, and not something
anyone would catch by reading the code.

This module builds `Pool`: a weighted round-robin rotation that keeps the
map for O(1) weight lookups and pairs it with an explicit, maintained
`[]string` order slice that `Next` always walks -- never the map -- to make
every pick exactly reproducible and exactly proportional to weight.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
wrrpool/                  module example.com/wrrpool
  go.mod                  go 1.24
  pool.go                 Pool; New, Add, Remove, Weight, Len, Next; four sentinel errors
  pool_test.go            Add validation table, Remove/order maintenance, the exact
                          smooth-WRR sequence, the map-range contrast, concurrency,
                          ExamplePool_Next
```

- Files: `pool.go`, `pool_test.go`.
- Implement: `New() *Pool`; `(*Pool).Add(addr string, weight int) error` rejecting an empty address with `ErrEmptyAddress`, a non-positive weight with `ErrInvalidWeight`, or a repeat address with `ErrDuplicateAddress`; `Remove(addr string)`; `Weight(addr string) (int, bool)`; `Len() int`; `Next() (string, error)` advancing a smooth weighted round-robin rotation over the maintained order slice, returning `ErrEmptyPool` when nothing is registered.
- Test: `Add`'s validation table; `Remove` dropping an address from both the weight map and the order slice, confirmed by a run of `Next` that never returns it again; an empty pool's `Next` error; the exact, deterministic pick sequence for a three-backend pool, repeated across two full cycles; the map-range contrast proving the naive version returns more than one arbitrary address across repeated calls and never matches a configured weight share, while `Next` matches one exactly; concurrent `Next` from many goroutines under `-race`; and `ExamplePool_Next` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/wrrpool
cd ~/go-exercises/wrrpool
go mod init example.com/wrrpool
go mod edit -go=1.24
```

### A map gives you O(1) lookup; it cannot give you a rotation

Go randomizes map iteration order per `range` statement, deliberately, so
that no code can come to depend on an order the language never promised.
That is exactly the right behavior for a map used as a lookup table, and
exactly the wrong behavior for anything that reads as "pick the next item
in sequence." A rotation needs two things a map cannot supply on its own:
a fixed visiting order, and a place to remember where the last pick left
off. The naive `Next` reaches for the map anyway, because the map is
already sitting right there holding the backends:

```go
// looks like "pick one backend"; is actually "pick whatever the runtime's
// randomized starting slot lands on this call" -- unrelated to weight.
func nextNaive(backends map[string]int) string {
	for addr := range backends {
		return addr
	}
	return ""
}
```

This compiles and always returns a real backend, which is exactly why it
survives a quick manual check. What it cannot do is track weight, because
it never reads the map's *values* at all -- only whichever key the
randomized range happens to visit first. Two calls against the identical,
unchanged map can return two different backends, because the random
starting position is chosen fresh on every `range` statement; the pattern
of which backend "wins" each call ends up shaped by the runtime's internal
hash-table layout, not by any weight an operator configured.

The fix keeps the map -- `Weight` still needs O(1) lookup by address -- and
adds the piece a map cannot provide: an explicit `[]string` order slice,
appended to on every `Add` and spliced on every `Remove`, that `Next`
always iterates instead of ever ranging the map. Walking a fixed slice in a
fixed order is what makes the classic *smooth weighted round-robin*
algorithm (the one nginx uses) deterministic: every backend's running
weight increases by its configured weight on every call, the highest
running weight wins and is selected, and the total weight is then
subtracted from the winner. Over a span of calls equal to the sum of all
weights, every backend is picked exactly its configured number of times,
and the running state returns to exactly where it started -- a property
that only holds because the iteration order feeding it never changes from
call to call.

Create `pool.go`:

```go
// Package wrrpool implements a weighted round-robin backend pool, the
// client-side load-balancing policy gRPC and Envoy both ship: cycle through
// registered backends in a stable, fair rotation proportional to each
// backend's weight, while still resolving a backend's weight in O(1).
//
// It exists to get one detail right: map iteration order is randomized per
// range call, so a map alone cannot be the rotation's source of truth, even
// though it is exactly the right structure for the O(1) weight lookup half
// of the job. Pool pairs a map[string]int for weights with an explicit
// []string order slice, maintained on every Add and Remove, and Next always
// walks that slice -- never the map -- to decide who goes next. See the
// package tests for what ranging the map directly gets wrong.
package wrrpool

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Sentinel errors returned by Add and Next. Callers should test for them
// with errors.Is rather than by comparing error strings.
var (
	// ErrEmptyAddress means Add was called with an empty address.
	ErrEmptyAddress = errors.New("wrrpool: address must not be empty")
	// ErrInvalidWeight means Add was called with a non-positive weight.
	ErrInvalidWeight = errors.New("wrrpool: weight must be positive")
	// ErrDuplicateAddress means Add was called with an address already
	// registered in the pool.
	ErrDuplicateAddress = errors.New("wrrpool: address already registered")
	// ErrEmptyPool means Next was called on a pool with no backends.
	ErrEmptyPool = errors.New("wrrpool: pool has no backends")
)

// Pool is a weighted round-robin rotation over a set of backend addresses.
//
// Pool is safe for concurrent use by multiple goroutines: every method
// takes an internal mutex, so Add, Remove, and Next may be called from any
// number of request-handling goroutines without external synchronization.
type Pool struct {
	mu      sync.Mutex
	weights map[string]int // O(1) weight lookup by address
	current map[string]int // running smooth-weighted-round-robin state
	order   []string       // rotation's source of truth; never derived from a map range
}

// New returns an empty Pool ready to accept backends via Add.
func New() *Pool {
	return &Pool{weights: make(map[string]int), current: make(map[string]int)}
}

// Add registers addr with the given weight. It returns ErrEmptyAddress if
// addr is empty, ErrInvalidWeight if weight is not positive, or
// ErrDuplicateAddress if addr is already registered.
func (p *Pool) Add(addr string, weight int) error {
	if addr == "" {
		return ErrEmptyAddress
	}
	if weight <= 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidWeight, weight)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.weights[addr]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateAddress, addr)
	}
	p.weights[addr] = weight
	p.current[addr] = 0
	p.order = append(p.order, addr)
	return nil
}

// Remove unregisters addr. Removing an address not in the pool is a no-op.
func (p *Pool) Remove(addr string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.weights[addr]; !ok {
		return
	}
	delete(p.weights, addr)
	delete(p.current, addr)
	if idx := slices.Index(p.order, addr); idx >= 0 {
		p.order = slices.Delete(p.order, idx, idx+1)
	}
}

// Weight returns addr's configured weight and whether addr is registered,
// resolved in O(1) via the internal map.
func (p *Pool) Weight(addr string) (int, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.weights[addr]
	return w, ok
}

// Len returns the number of registered backends.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.order)
}

// Next advances the rotation and returns the next backend address, using
// the smooth weighted round-robin algorithm: every registered backend's
// running weight is increased by its configured weight, the backend with
// the highest running weight is selected, and the total of all configured
// weights is then subtracted from the selected backend's running weight.
// Over any span of calls equal to the sum of all weights, every backend is
// returned exactly its configured weight number of times, and the running
// state returns to its starting point -- the rotation is periodic and
// exactly fair, not merely fair on average.
//
// Next always iterates the order slice, never the weights map, so ties in
// running weight are broken the same way on every call: by the backend's
// position in registration order. It returns ErrEmptyPool if no backends
// are registered.
func (p *Pool) Next() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.order) == 0 {
		return "", ErrEmptyPool
	}
	total := 0
	best := p.order[0]
	for _, addr := range p.order {
		w := p.weights[addr]
		p.current[addr] += w
		total += w
		if p.current[addr] > p.current[best] {
			best = addr
		}
	}
	p.current[best] -= total
	return best, nil
}
```

### Using it

Build one `Pool` per upstream cluster with `New`, register every backend
with `Add(addr, weight)` as service discovery reports it, and call `Next`
once per outgoing request to get the address to dial. `Remove` takes a
backend out of rotation immediately -- both its weight entry and its slot
in the order slice -- so a backend pulled from service discovery stops
being picked on the very next call. Every method locks internally, so one
shared `*Pool` is safe to hand to every goroutine issuing requests; nothing
about its use requires the caller to add synchronization of its own.

`Weight` is the O(1) side of the contract: an operator dashboard, a metrics
exporter, or a debug endpoint can ask "what weight is this backend
configured with" without walking the rotation at all. `Next` is the O(N)
side, where N is the number of registered backends -- proportional to the
pool size, not to the weight values, and unavoidable for an algorithm that
has to look at every backend's running state to find the current maximum.

`ExamplePool_Next` in the `_test.go` is the runnable demonstration of the
whole API; `go test` executes it and compares its output against the
`// Output:` comment:

```go
p := New()
if err := p.Add("10.0.0.1:80", 3); err != nil {
	panic(err)
}
if err := p.Add("10.0.0.2:80", 1); err != nil {
	panic(err)
}

for range 4 { // one full cycle: total weight is 4
	addr, err := p.Next()
	if err != nil {
		panic(err)
	}
	fmt.Println(addr)
}
```

### Tests

`TestAddValidatesInput` is the table over `Add`'s rejections: an empty
address, a zero weight, a negative weight, and a duplicate address.
`TestRemoveMaintainsOrderAndWeight` registers three backends, removes one,
and confirms both that its weight lookup reports absent and that ten
subsequent `Next` calls never return it -- proving the order slice, not
just the weight map, was updated. `TestNextOnEmptyPool` and
`TestNextFollowsSmoothWeightedRotation` are the two edges of `Next` itself:
an empty pool returns `ErrEmptyPool`, and a three-backend pool with weights
3, 1, 1 produces the exact deterministic sequence `a, b, a, c, a`, checked
across two consecutive full cycles to confirm the running state truly
returns to its starting point.

`TestNaiveMapRangeIgnoresWeight` is the heart of the module.  `nextNaive`
is unexported and unreachable from the package API; the test pins two
properties that hold regardless of exactly how Go's randomized map
iteration happens to distribute across a particular set of keys (which is
not a documented contract this test relies on): repeated calls on an
unchanged map return more than one distinct address, so it is not even a
stable "always the same one," and over a hundred calls against a backend
configured for 97% of traffic, the naive count never lands on the exact
97 that `Next` is proven to hit deterministically.
`TestPoolIsSafeForConcurrentUse` drives many goroutines calling `Next`
concurrently against a shared pool under `-race`.

Create `pool_test.go`:

```go
package wrrpool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// nextNaive is the "Next" a first draft of this pool tends to ship with: it
// reads directly off the backend weight map and returns whatever the range
// clause visits first. It compiles, it always returns a registered
// backend, and it is never exported and never reachable from Pool; it
// exists so the tests can pin what it gets wrong -- it never even looks at
// the weight values it is handed.
func nextNaive(backends map[string]int) string {
	for addr := range backends {
		return addr
	}
	return ""
}

func TestAddValidatesInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		addr    string
		weight  int
		setup   func(*Pool)
		wantErr error
	}{
		{name: "empty address", addr: "", weight: 1, wantErr: ErrEmptyAddress},
		{name: "zero weight", addr: "10.0.0.1:80", weight: 0, wantErr: ErrInvalidWeight},
		{name: "negative weight", addr: "10.0.0.1:80", weight: -3, wantErr: ErrInvalidWeight},
		{
			name: "duplicate address", addr: "10.0.0.1:80", weight: 1,
			setup:   func(p *Pool) { _ = p.Add("10.0.0.1:80", 5) },
			wantErr: ErrDuplicateAddress,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := New()
			if tc.setup != nil {
				tc.setup(p)
			}
			if err := p.Add(tc.addr, tc.weight); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Add(%q, %d) error = %v, want %v", tc.addr, tc.weight, err, tc.wantErr)
			}
		})
	}
}

func TestRemoveIsNoOpForUnknownAddress(t *testing.T) {
	t.Parallel()

	p := New()
	p.Remove("never-added") // must not panic
	if got := p.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
}

func TestRemoveMaintainsOrderAndWeight(t *testing.T) {
	t.Parallel()

	p := New()
	mustAdd(t, p, "a", 1)
	mustAdd(t, p, "b", 1)
	mustAdd(t, p, "c", 1)

	p.Remove("b")
	if got := p.Len(); got != 2 {
		t.Fatalf("Len() after removing b = %d, want 2", got)
	}
	if _, ok := p.Weight("b"); ok {
		t.Fatal("Weight(b) after Remove: ok = true, want false")
	}
	for range 10 {
		addr, err := p.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if addr == "b" {
			t.Fatal("Next returned b after it was removed")
		}
	}
}

func TestNextOnEmptyPool(t *testing.T) {
	t.Parallel()

	p := New()
	if _, err := p.Next(); !errors.Is(err, ErrEmptyPool) {
		t.Fatalf("Next on empty pool: error = %v, want ErrEmptyPool", err)
	}
}

// TestNextFollowsSmoothWeightedRotation pins the exact, deterministic pick
// sequence for weights a=3, b=1, c=1 (total 5): a, b, a, c, a. This is not
// a statistical property -- smooth weighted round-robin is fully
// deterministic given a fixed iteration order, and the running state
// returns to its starting point after exactly one full cycle of five
// calls, which the test also checks by running a second cycle and getting
// the identical sequence back.
func TestNextFollowsSmoothWeightedRotation(t *testing.T) {
	t.Parallel()

	p := New()
	mustAdd(t, p, "a", 3)
	mustAdd(t, p, "b", 1)
	mustAdd(t, p, "c", 1)

	want := []string{"a", "b", "a", "c", "a"}
	for cycle := range 2 {
		for i, w := range want {
			got, err := p.Next()
			if err != nil {
				t.Fatalf("cycle %d, pick %d: %v", cycle, i, err)
			}
			if got != w {
				t.Fatalf("cycle %d, pick %d = %q, want %q", cycle, i, got, w)
			}
		}
	}
}

// TestNaiveMapRangeIgnoresWeight is the heart of the module. nextNaive
// never reads the weight values it is handed -- it cannot, by
// construction, track configured weight the way Next does. Two properties
// pin that down without relying on exactly how Go's randomized map
// iteration happens to distribute across a particular set of keys (which
// is not a documented contract and is not this test's business): first,
// repeated calls on an unchanged map return more than one distinct
// address, so it is not even a stable "always pick the same one" -- it
// gives an arbitrary, shifting answer instead of advancing a rotation.
// Second, over a span of calls equal to the heavy backend's configured
// share of total weight, the naive version comes nowhere near matching it
// exactly, while Next -- as TestNextFollowsSmoothWeightedRotation already
// pins for a smaller example -- always does, because it walks the
// maintained order slice with genuine running-weight state instead of
// asking a map for "one" element.
func TestNaiveMapRangeIgnoresWeight(t *testing.T) {
	t.Parallel()

	backends := map[string]int{"big": 97, "a": 1, "b": 1, "c": 1}

	seen := make(map[string]bool)
	naiveBigCount := 0
	const calls = 100
	for range calls {
		addr := nextNaive(backends)
		seen[addr] = true
		if addr == "big" {
			naiveBigCount++
		}
	}
	if len(seen) < 2 {
		t.Fatalf("nextNaive returned only %v across %d calls on an unchanged map; want more than one distinct address, since it advances nothing", seen, calls)
	}
	if naiveBigCount == 97 {
		t.Fatal("nextNaive coincidentally matched the exact configured weight; it does not track weight, this would be chance")
	}

	p := New()
	for addr, w := range backends {
		mustAdd(t, p, addr, w)
	}
	bigCount := 0
	for range 100 { // one full cycle: total weight is 100
		addr, err := p.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if addr == "big" {
			bigCount++
		}
	}
	if bigCount != 97 {
		t.Fatalf("Pool picked the heavy backend %d/100 times, want exactly 97 (its configured weight)", bigCount)
	}
}

func TestPoolIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	p := New()
	mustAdd(t, p, "a", 1)
	mustAdd(t, p, "b", 1)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if _, err := p.Next(); err != nil {
					t.Errorf("Next: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

func mustAdd(t *testing.T, p *Pool, addr string, weight int) {
	t.Helper()
	if err := p.Add(addr, weight); err != nil {
		t.Fatalf("Add(%q, %d): %v", addr, weight, err)
	}
}

// ExamplePool_Next demonstrates registering weighted backends and reading
// off one full, exactly fair rotation cycle.
func ExamplePool_Next() {
	p := New()
	if err := p.Add("10.0.0.1:80", 3); err != nil {
		panic(err)
	}
	if err := p.Add("10.0.0.2:80", 1); err != nil {
		panic(err)
	}

	for range 4 { // one full cycle: total weight is 4
		addr, err := p.Next()
		if err != nil {
			panic(err)
		}
		fmt.Println(addr)
	}

	// Output:
	// 10.0.0.1:80
	// 10.0.0.1:80
	// 10.0.0.2:80
	// 10.0.0.1:80
}
```

## Review

The pool is correct when `Next` reproduces the exact smooth weighted
round-robin sequence for its registered backends, every time, with no
dependence on how the runtime happens to lay out the underlying map. `Add`
rejects an empty address, a non-positive weight, and a duplicate address
with distinct sentinels, all checkable with `errors.Is`; `Remove` keeps the
weight map and the order slice in lockstep, so a removed backend
disappears from the rotation immediately, not eventually. The trap this
module isolates is subtler than most: `for addr := range pool.backends {
return addr }` compiles, always returns a real backend, and reads as
"pick one" -- but it never touches the weight values sitting in the same
map, so it cannot implement a weighted anything, and it has no memory
between calls, so it cannot implement a rotation either. `Next` fixes both
by walking a maintained `[]string` order slice with genuine running-weight
state, which is what makes the exact-count property in the tests possible
at all: over a span of calls equal to total weight, every backend comes
back exactly its configured number of times. `Pool` is safe for concurrent
use, guarded by a single mutex, and `ExamplePool_Next` is the executable
documentation `go test` verifies. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: For statements with range clause](https://go.dev/ref/spec#For_range) — the randomized iteration order this module works around.
- [`slices.Delete`](https://pkg.go.dev/slices#Delete) and [`slices.Index`](https://pkg.go.dev/slices#Index) — maintaining the order slice on `Remove`.
- [nginx: smooth weighted round-robin balancing](https://github.com/phusion/nginx/commit/27e94984486058d73157038f7950a0a36ecc636) — the algorithm `Next` implements.
- [gRPC load balancing](https://grpc.io/blog/grpc-load-balancing/) — the client-side round-robin shape this module's `Pool` is drawn from.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [14-resource-state-reconciler-diff.md](14-resource-state-reconciler-diff.md) | Next: [16-lazy-pattern-compile-cache.md](16-lazy-pattern-compile-cache.md)
