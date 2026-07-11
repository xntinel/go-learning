# Exercise 4: Prometheus-style counter registry (RWMutex map + atomic hot path)

Metric registries are the sharpest example of "let the lock protect the structure
and an atomic protect the value". A counter named `http_requests_total` is
incremented on nearly every request, but it is *registered* exactly once. So the
increment hot path should never touch the write lock: `Inc` takes a read lock only
long enough to find the existing `*atomic.Int64`, releases it, and does
`counter.Add(1)` lock-free. Only a first-seen metric name falls through to the
write lock to register a new counter. This exercise builds that registry and
benchmarks the hot path against a naive full-lock version.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
metricsreg/                  independent module: example.com/metricsreg
  go.mod                     module example.com/metricsreg
  registry.go                type Registry (map[string]*atomic.Int64); Inc, Value, Len
  cmd/
    demo/
      main.go                runnable demo: increment a few metrics, read them back
  registry_test.go           high-contention no-lost-updates test, Example, BenchmarkInc vs full-lock
```

Files: `registry.go`, `cmd/demo/main.go`, `registry_test.go`.
Implement: a `*Registry` mapping name → `*atomic.Int64`; `Inc(name)` finds an existing counter under `RLock` and does `Add(1)` lock-free, falling through to `Lock` + double-check only to register a new name; `Value(name)` and `Len`.
Test: many goroutines increment a shared set of names — no lost updates, size equals distinct names; a benchmark comparing the RLock+atomic path against a full-`Lock` variant.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/metricsreg/cmd/demo
cd ~/go-exercises/metricsreg
go mod init example.com/metricsreg
```

### Two locks for two jobs

There are two distinct pieces of shared state here, and they need different
protection. The *map* — which names exist and what pointer each maps to — is
mutated only when a new metric is registered, so an `RWMutex` guarding the map is
perfect: readers (increment lookups) dominate, registration is rare. The *value*
inside each entry is mutated on every increment, so it lives in an `atomic.Int64`
that needs no lock at all. Crucially, the map stores `*atomic.Int64` (a pointer),
not `atomic.Int64` by value: the pointer is stable once registered, so a goroutine
that found it under `RLock` can safely release the read lock and `Add` to it
afterward, even as other goroutines increment the same counter.

`Inc` therefore has a fast path and a slow path. Fast path: `RLock`, look up the
name, `RUnlock`; if found, `Add(1)` on the pointer with no lock held. This is the
steady state — every increment of an already-registered metric — and it never
contends on the write lock, so a thousand goroutines incrementing existing metrics
scale with the read lock, not against a serial write lock. Slow path (first sight
of a name only): `Lock`, re-check the map (another goroutine may have registered
it while we waited), create the counter if still absent, `Unlock`, then `Add`. The
double-check is the same discipline as the cache: without it, two goroutines
seeing a new name at once would each create a counter and one increment would be
lost.

`Value(name)` reads the counter under `RLock` (to find the pointer safely) and
returns `Load()`. `Len` reports the number of registered metrics under `RLock`.

Create `registry.go`:

```go
package metricsreg

import (
	"sync"
	"sync/atomic"
)

// Registry is a concurrency-safe counter registry. The RWMutex protects the map
// structure; each counter value is an atomic, so steady-state increments of an
// already-registered metric never touch the write lock.
type Registry struct {
	mu       sync.RWMutex
	counters map[string]*atomic.Int64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{counters: make(map[string]*atomic.Int64)}
}

// Inc increments the counter named name by one, registering it on first sight.
func (r *Registry) Inc(name string) {
	// Fast path: find an existing counter under the read lock, then Add
	// lock-free on the stable pointer.
	r.mu.RLock()
	c, ok := r.counters[name]
	r.mu.RUnlock()
	if ok {
		c.Add(1)
		return
	}

	// Slow path (first sight of name): register under the write lock,
	// double-checking in case another goroutine won the race.
	r.mu.Lock()
	if c, ok = r.counters[name]; !ok {
		c = new(atomic.Int64)
		r.counters[name] = c
	}
	r.mu.Unlock()

	c.Add(1)
}

// Value returns the current count for name and whether it is registered.
func (r *Registry) Value(name string) (int64, bool) {
	r.mu.RLock()
	c, ok := r.counters[name]
	r.mu.RUnlock()
	if !ok {
		return 0, false
	}
	return c.Load(), true
}

// Len reports the number of registered metrics.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.counters)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metricsreg"
)

func main() {
	r := metricsreg.NewRegistry()
	for range 3 {
		r.Inc("http_requests_total")
	}
	r.Inc("http_errors_total")

	reqs, _ := r.Value("http_requests_total")
	errs, _ := r.Value("http_errors_total")
	fmt.Printf("requests=%d errors=%d metrics=%d\n", reqs, errs, r.Len())

	if _, ok := r.Value("unknown"); !ok {
		fmt.Println("unknown metric: not registered")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests=3 errors=1 metrics=2
unknown metric: not registered
```

### Tests

`TestNoLostUpdates` is the correctness core: a set of goroutines each increment
every name in a shared set many times; the final `Value` of each name must equal
the exact expected total, proving no increment was lost to a race between the
read-lock lookup and the atomic add, or between two registrations. `Len` must
equal the number of distinct names. `BenchmarkInc` measures the RLock+atomic hot
path under `RunParallel`; `BenchmarkIncFullLock` measures a variant that holds the
write lock for the whole increment, so a benchmark run makes the contention
difference visible. The benchmarks must compile and run; the qualitative result is
that the atomic path scales while the full-lock path serializes.

Create `registry_test.go`:

```go
package metricsreg

import (
	"fmt"
	"sync"
	"testing"
)

func TestNoLostUpdates(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	names := []string{"a", "b", "c"}

	const goroutines = 50
	const perName = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perName {
				for _, n := range names {
					r.Inc(n)
				}
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * perName)
	for _, n := range names {
		got, ok := r.Value(n)
		if !ok {
			t.Fatalf("metric %q not registered", n)
		}
		if got != want {
			t.Fatalf("Value(%q) = %d, want %d (lost updates)", n, got, want)
		}
	}
	if r.Len() != len(names) {
		t.Fatalf("Len() = %d, want %d", r.Len(), len(names))
	}
}

func ExampleRegistry() {
	r := NewRegistry()
	r.Inc("hits")
	r.Inc("hits")
	v, _ := r.Value("hits")
	fmt.Println(v)
	// Output: 2
}

func BenchmarkInc(b *testing.B) {
	r := NewRegistry()
	r.Inc("warm") // pre-register so every op hits the fast path
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Inc("warm")
		}
	})
}

// fullLockRegistry increments while holding the exclusive lock for the whole
// operation, so every increment serializes. It is the baseline BenchmarkInc
// beats at high concurrency.
type fullLockRegistry struct {
	mu       sync.Mutex
	counters map[string]int64
}

func (r *fullLockRegistry) Inc(name string) {
	r.mu.Lock()
	r.counters[name]++
	r.mu.Unlock()
}

func BenchmarkIncFullLock(b *testing.B) {
	r := &fullLockRegistry{counters: map[string]int64{"warm": 0}}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.Inc("warm")
		}
	})
}
```

## Review

The registry is correct when the map is only ever mutated under `Lock` (with a
double-check on registration) and each value is an `atomic.Int64` incremented
lock-free through a stable pointer. `TestNoLostUpdates` is the proof: 50 goroutines
× 1000 increments × 3 names must land exactly, with no lost update. The mistakes to
avoid are storing counters by value instead of by pointer (a value copy would make
the atomic uncoordinated), incrementing under the read lock via a non-atomic field
(a race), and skipping the registration double-check (a lost first increment). The
benchmarks make the design's payoff concrete — run `go test -bench . -benchmem`
and compare `BenchmarkInc` against `BenchmarkIncFullLock` under `-cpu 8`.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, the lock-free counter value.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the map-structure lock.
- [Prometheus Go client `Counter`](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#Counter) — the real registry this models, which also separates registration from increment.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-copy-on-write-snapshot-vs-rwmutex.md](05-copy-on-write-snapshot-vs-rwmutex.md)
