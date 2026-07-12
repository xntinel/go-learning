# Exercise 6: Nested Maps: per-tenant per-endpoint counters with lazy inner-map init

A metrics aggregator keyed by (tenant, endpoint) is naturally a nested map
`map[string]map[string]int64`. This exercise builds it, confronts the classic
"assignment to entry in nil map" trap that fires when you forget to allocate the
inner map, flattens the two levels into sorted rows, and weighs the nested design
against a flat composite struct key.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
metrics/                   independent module: example.com/metrics
  go.mod
  metrics.go               Aggregator over map[string]map[string]int64; New, Inc, Get, Snapshot (sorted rows)
  cmd/
    demo/
      main.go              runnable demo: count requests per tenant/endpoint, print sorted rows
  metrics_test.go          lazy alloc, accumulation, isolation, sorted snapshot, nil-inner-map panic, -race
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: `Inc(tenant, endpoint)` that lazily allocates the inner map on first touch, `Get`, and `Snapshot` returning rows sorted by (tenant, endpoint).
- Test: first `Inc` for a new tenant allocates (no panic), repeated `Inc` accumulates, tenants are isolated, `Snapshot` is sorted, a direct write without init reproduces the nil-map panic, and concurrent `Inc` is race-free.
- Verify: `go test -count=1 -race ./...`

### Why the inner map must be lazily allocated

`map[string]map[string]int64` is a map whose values are themselves maps. Reading
an absent outer key yields the value type's zero — and the zero value of a map
type is `nil`, not an empty map. So `counts[tenant]` for a never-seen tenant is a
`nil` map, and `counts[tenant][endpoint]++` would try to write to that nil inner
map and panic with `assignment to entry in nil map`. This is the single most
common nested-map bug. The fix is to check-and-allocate before the inner write:

```go
inner, ok := counts[tenant]
if !ok {
	inner = make(map[string]int64)
	counts[tenant] = inner
}
inner[endpoint]++
```

Note the outer level does *not* have this problem for the increment itself,
because we never write `counts[tenant] = ...` a scalar; the danger is exclusively
the inner map. Also note the reads are safe: `Get` can range or index a nil inner
map without panicking (a nil map reads as empty), so only the write path needs the
guard.

`Snapshot` flattens the two levels into a slice of `Row{Tenant, Endpoint, Count}`
and sorts it deterministically by (tenant, endpoint) using `slices.SortFunc` with
`cmp.Or(cmp.Compare(a.Tenant, b.Tenant), cmp.Compare(a.Endpoint, b.Endpoint))` —
`cmp.Or` returns the first non-zero comparison, giving a clean multi-key sort.

This is also the moment to weigh the alternative: a flat
`map[struct{ Tenant, Endpoint string }]int64` has no inner map to forget, allocates
one table instead of one-per-tenant, and cannot hit this trap. The nested form
earns its keep only when you frequently operate on a whole tenant's endpoints at
once (enumerate, delete a tenant wholesale); otherwise prefer the composite key,
covered in the idempotency-guard exercise.

Create `metrics.go`:

```go
package metrics

import (
	"cmp"
	"slices"
	"sync"
)

// Row is one flattened metric: a (tenant, endpoint) count.
type Row struct {
	Tenant   string
	Endpoint string
	Count    int64
}

// Aggregator counts requests per (tenant, endpoint) in a nested map.
type Aggregator struct {
	mu     sync.Mutex
	counts map[string]map[string]int64
}

// New returns an empty Aggregator.
func New() *Aggregator {
	return &Aggregator{counts: make(map[string]map[string]int64)}
}

// Inc adds one to the counter for (tenant, endpoint), lazily allocating the
// tenant's inner map on first touch.
func (a *Aggregator) Inc(tenant, endpoint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	inner, ok := a.counts[tenant]
	if !ok {
		inner = make(map[string]int64)
		a.counts[tenant] = inner
	}
	inner[endpoint]++
}

// Get returns the current count for (tenant, endpoint); zero if unseen.
func (a *Aggregator) Get(tenant, endpoint string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts[tenant][endpoint] // nil inner map reads as zero, no panic
}

// Snapshot flattens all counters into rows sorted by (tenant, endpoint).
func (a *Aggregator) Snapshot() []Row {
	a.mu.Lock()
	defer a.mu.Unlock()
	var rows []Row
	for tenant, inner := range a.counts {
		for endpoint, count := range inner {
			rows = append(rows, Row{Tenant: tenant, Endpoint: endpoint, Count: count})
		}
	}
	slices.SortFunc(rows, func(x, y Row) int {
		return cmp.Or(
			cmp.Compare(x.Tenant, y.Tenant),
			cmp.Compare(x.Endpoint, y.Endpoint),
		)
	})
	return rows
}
```

### The runnable demo

The demo counts a handful of requests across two tenants and prints the sorted
snapshot rows.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metrics"
)

func main() {
	a := metrics.New()
	a.Inc("acme", "/login")
	a.Inc("acme", "/login")
	a.Inc("acme", "/logout")
	a.Inc("globex", "/login")

	for _, r := range a.Snapshot() {
		fmt.Printf("%s %s %d\n", r.Tenant, r.Endpoint, r.Count)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acme /login 2
acme /logout 1
globex /login 1
```

The rows are sorted by tenant, then by endpoint within a tenant, because
`Snapshot` sorts with `cmp.Or` over the two keys; the map's own iteration order is
never exposed.

### Tests

`TestFirstIncAllocates` proves the first `Inc` for a new tenant does not panic —
the lazy allocation working. `TestIncAccumulates` and `TestTenantsIsolated` pin the
counting semantics. `TestSnapshotSorted` locks the multi-key order.
`TestNilInnerMapPanics` reproduces the trap directly on a raw nested map and
asserts the panic message, documenting exactly what the lazy guard prevents.
`TestConcurrentInc` proves the mutex serializes concurrent increments under
`-race`.

Create `metrics_test.go`:

```go
package metrics

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestFirstIncAllocates(t *testing.T) {
	t.Parallel()

	a := New()
	a.Inc("newtenant", "/x") // must not panic on the nil inner map
	if got := a.Get("newtenant", "/x"); got != 1 {
		t.Fatalf("Get = %d, want 1", got)
	}
}

func TestIncAccumulates(t *testing.T) {
	t.Parallel()

	a := New()
	a.Inc("t", "/x")
	a.Inc("t", "/x")
	a.Inc("t", "/x")
	if got := a.Get("t", "/x"); got != 3 {
		t.Fatalf("Get = %d, want 3", got)
	}
}

func TestTenantsIsolated(t *testing.T) {
	t.Parallel()

	a := New()
	a.Inc("a", "/x")
	a.Inc("b", "/x")
	if got := a.Get("a", "/x"); got != 1 {
		t.Fatalf("a count = %d, want 1", got)
	}
	if got := a.Get("b", "/x"); got != 1 {
		t.Fatalf("b count = %d, want 1", got)
	}
}

func TestSnapshotSorted(t *testing.T) {
	t.Parallel()

	a := New()
	a.Inc("globex", "/login")
	a.Inc("acme", "/logout")
	a.Inc("acme", "/login")

	got := a.Snapshot()
	want := []Row{
		{"acme", "/login", 1},
		{"acme", "/logout", 1},
		{"globex", "/login", 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNilInnerMapPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic writing to a nil inner map")
		}
		if !strings.Contains(fmt.Sprint(r), "nil map") {
			t.Fatalf("panic = %v, want a nil-map message", r)
		}
	}()

	raw := map[string]map[string]int64{}
	raw["tenant"]["endpoint"] = 1 // inner map is nil: panics
}

func TestConcurrentInc(t *testing.T) {
	t.Parallel()

	a := New()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.Inc(fmt.Sprintf("tenant-%d", i%4), "/x")
		}(i)
	}
	wg.Wait()

	var total int64
	for _, r := range a.Snapshot() {
		total += r.Count
	}
	if total != 200 {
		t.Fatalf("total increments = %d, want 200", total)
	}
}

func ExampleAggregator_Snapshot() {
	a := New()
	a.Inc("acme", "/login")
	a.Inc("acme", "/login")
	fmt.Println(a.Snapshot())
	// Output: [{acme /login 2}]
}
```

## Review

The aggregator is correct when `Inc` always allocates the inner map before writing
to it: `TestFirstIncAllocates` is the live proof that the lazy guard works, and
`TestNilInnerMapPanics` shows exactly the crash you get without it. Counting is
per-(tenant, endpoint) and tenants are isolated because each has its own inner map.
`Snapshot` is deterministic through `cmp.Or`-driven multi-key sorting, never
through map order. The design lesson is the trade-off: the nested map costs a lazy
guard and one allocation per tenant; a flat `map[struct{Tenant, Endpoint string}]int64`
avoids both and cannot hit the nil-inner-map trap, and is the better default unless
you need whole-tenant operations. Run `go test -race` to confirm the mutex guards
the nested writes.

## Resources

- [Go blog: Go maps in action](https://go.dev/blog/maps) — nested maps and lazy inner-map init.
- [cmp.Or](https://pkg.go.dev/cmp#Or) and [cmp.Compare](https://pkg.go.dev/cmp#Compare) — multi-key comparisons.
- [slices.SortFunc](https://pkg.go.dev/slices#SortFunc) — sorting the flattened rows.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-maps-package-config-snapshots.md](05-maps-package-config-snapshots.md) | Next: [07-nil-map-failure-modes.md](07-nil-map-failure-modes.md)
