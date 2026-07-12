# Exercise 8: A Read-Heavy Pointer Registry — sync.Map With Shared *Counter

A per-tenant metrics registry is read-mostly with disjoint keys: exactly the shape
`sync.Map` is built for. This module builds one where the values are `*Counter`
with `atomic.Int64` fields, uses `LoadOrStore` to get-or-create exactly one shared
counter per key under concurrency, and increments through that shared pointer.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
tenantmetrics/                independent module: example.com/tenantmetrics
  go.mod                      go 1.24
  registry.go                 Counter (atomic.Int64); Registry (sync.Map); Incr, Get, Snapshot
  registry_test.go            LoadOrStore shares one instance under -race; Range snapshots; disjoint keys independent
  cmd/demo/main.go            runnable demo incrementing two tenants concurrently
```

Files: `registry.go`, `registry_test.go`, `cmd/demo/main.go`.
Implement: a `Counter` with an `atomic.Int64`; a `Registry` over `sync.Map` whose
`Incr(key)` uses `LoadOrStore` to get-or-create one shared `*Counter` and adds to
it; `Get(key)`; `Snapshot()` via `Range`.
Test: concurrent `Incr` on one key sums to the total (one shared instance); `Range`
snapshots current values; disjoint keys accumulate independently. Run `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/08-pointers-in-slices-and-maps/08-sync-map-pointer-registry/cmd/demo
cd go-solutions/09-pointers/08-pointers-in-slices-and-maps/08-sync-map-pointer-registry
```

### Why sync.Map, and why the value must be a pointer

`sync.Map` trades generality for a specific win: it is optimized for two access
patterns — an entry is written once and read many times, or different goroutines
touch disjoint sets of keys. A per-tenant counter registry is both: each tenant's
counter is created once and then hammered with reads and increments, and different
request-handling goroutines mostly touch different tenants. For that shape,
`sync.Map` avoids the single-writer-lock contention a `sync.RWMutex` around a plain
map would suffer under many concurrent readers. (For a write-heavy map with
overlapping keys, the plain-map-plus-`RWMutex` is usually faster; `sync.Map` is not
a general upgrade.)

The get-or-create primitive is `LoadOrStore(key, value) (actual any, loaded bool)`.
If the key exists it returns the existing value and `loaded == true`; otherwise it
stores `value` and returns it with `loaded == false`. Under a race of goroutines
all calling `LoadOrStore(key, newCounter)`, the map guarantees exactly one stored
value wins, and every caller receives that same `actual` — the losers' freshly
allocated counters are discarded. That is the atomic get-or-create you need so all
goroutines converge on one instance.

The value must be a pointer. If you stored a `Counter` value, `LoadOrStore` would
hand each goroutine its own copy of a snapshot, and increments would be lost. By
storing `*Counter`, every goroutine that resolves the key gets the *same* pointer,
and they all mutate the one `atomic.Int64` behind it. The atomic is what makes the
shared mutation race-free without a per-entry lock: `c.n.Add(1)` is a single atomic
read-modify-write. `sync.Map` values are typed `any`, so `Load`/`LoadOrStore`
return `any` and you type-assert back to `*Counter`.

Create `registry.go`:

```go
package tenantmetrics

import (
	"sync"
	"sync/atomic"
)

// Counter is a shared, concurrency-safe counter. It is always held by pointer so
// every goroutine mutates the same atomic.
type Counter struct {
	n atomic.Int64
}

func (c *Counter) Add(delta int64) { c.n.Add(delta) }
func (c *Counter) Value() int64    { return c.n.Load() }

// Registry maps a tenant key to its shared *Counter using a sync.Map.
type Registry struct {
	m sync.Map // map[string]*Counter
}

func NewRegistry() *Registry { return &Registry{} }

// counter returns the single shared *Counter for key, creating it once. Under
// concurrency LoadOrStore guarantees all callers converge on one instance.
func (r *Registry) counter(key string) *Counter {
	if v, ok := r.m.Load(key); ok { // fast path: already present
		return v.(*Counter)
	}
	actual, _ := r.m.LoadOrStore(key, &Counter{})
	return actual.(*Counter)
}

// Incr adds delta to the tenant's counter through the shared pointer.
func (r *Registry) Incr(key string, delta int64) {
	r.counter(key).Add(delta)
}

// Get returns the current value for key, or 0 if absent.
func (r *Registry) Get(key string) int64 {
	if v, ok := r.m.Load(key); ok {
		return v.(*Counter).Value()
	}
	return 0
}

// Snapshot copies current counts into a plain map via Range.
func (r *Registry) Snapshot() map[string]int64 {
	out := make(map[string]int64)
	r.m.Range(func(k, v any) bool {
		out[k.(string)] = v.(*Counter).Value()
		return true
	})
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/tenantmetrics"
)

func main() {
	reg := tenantmetrics.NewRegistry()

	var wg sync.WaitGroup
	for _, tenant := range []string{"acme", "globex"} {
		for range 100 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				reg.Incr(tenant, 1)
			}()
		}
	}
	wg.Wait()

	snap := reg.Snapshot()
	fmt.Printf("acme=%d globex=%d\n", snap["acme"], snap["globex"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acme=100 globex=100
```

### Tests

`TestLoadOrStoreSharesSingleInstance` launches many goroutines that all `Incr` the
same key and asserts the final value equals the total number of increments —
possible only if they share one `*Counter`. `TestRangeSnapshotsCurrentValues`
populates several keys and asserts the `Snapshot`. `TestDisjointKeysIndependent`
confirms two keys accumulate separately. Everything runs under `-race`.

Create `registry_test.go`:

```go
package tenantmetrics

import (
	"sync"
	"testing"
)

func TestLoadOrStoreSharesSingleInstance(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	const goroutines = 200
	const perG = 50

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perG {
				reg.Incr("tenant", 1)
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perG)
	if got := reg.Get("tenant"); got != want {
		t.Fatalf("count = %d, want %d (all goroutines must share one *Counter)", got, want)
	}
}

func TestRangeSnapshotsCurrentValues(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Incr("a", 3)
	reg.Incr("b", 7)
	reg.Incr("a", 1)

	snap := reg.Snapshot()
	if snap["a"] != 4 || snap["b"] != 7 {
		t.Fatalf("snapshot = %v, want a=4 b=7", snap)
	}
}

func TestDisjointKeysIndependent(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Incr("x", 10)
	reg.Incr("y", 20)

	if reg.Get("x") != 10 || reg.Get("y") != 20 {
		t.Fatalf("x=%d y=%d, want 10 and 20", reg.Get("x"), reg.Get("y"))
	}
}
```

## Review

The registry is correct when concurrent creators of the same key converge on one
instance and all increments land on it. `TestLoadOrStoreSharesSingleInstance` is
the executable proof: 200 goroutines × 50 increments must total exactly 10000,
which holds only if `LoadOrStore` handed every goroutine the same `*Counter` —
storing a value instead would drop most of the writes. The pointer value is what
makes the shared `atomic.Int64` a single mutable instance; the atomic is what makes
concurrent `Add` race-free without a per-entry lock, which `-race` confirms. Reach
for `sync.Map` when the workload is read-mostly with disjoint keys, as a metrics or
rate-limiter registry is; reach for a plain map under `RWMutex` when writes with
overlapping keys dominate.

## Resources

- [`sync.Map`](https://pkg.go.dev/sync#Map) — the documented use cases and `LoadOrStore` semantics.
- [`sync.Map.LoadOrStore`](https://pkg.go.dev/sync#Map.LoadOrStore) — returns the existing value if present, else stores and returns the given one.
- [`atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — the lock-free counter behind each shared `*Counter`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-retry-scheduler-heap-of-pointers.md](09-retry-scheduler-heap-of-pointers.md)
