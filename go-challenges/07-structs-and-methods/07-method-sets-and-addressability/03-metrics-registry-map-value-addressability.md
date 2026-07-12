# Exercise 3: In-Memory Metrics Registry — Why You Cannot Inc() a Map Value

A registry of named counters is the most common place addressability turns into a
compile error: `m[name].Inc()` does not compile when the map holds counter values
and `Inc` is a pointer method, because a map element has no address. This module
builds the registry the way that compiles — `map[string]*Counter` — and proves
the increments land under `-race`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
metrics/                       independent module: example.com/metrics
  go.mod                       module path + go directive
  metrics.go                   Counter (atomic body, pointer methods); Registry over map[string]*Counter
  cmd/
    demo/
      main.go                  register and increment named counters
  metrics_test.go              increment-through-pointer tests; concurrent -race test
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: a `Counter` with `Inc`/`Add`/`Value` on pointer receivers backed by `atomic.Int64`, and a `Registry` storing `map[string]*Counter` with a `Counter(name)` getter that returns `*Counter`.
- Test: increment named counters through the stored `*Counter` and assert totals; a concurrent test with many goroutines under `-race`; a documented block quoting the non-addressability compile error.
- Verify: `go vet ./...`, `go test -count=1 -race ./...`.

### Why the map must hold *Counter

`Counter.Inc()` mutates the counter, so it is a pointer method (and its body is an
`atomic.Int64`, which is non-copyable, forcing pointer receivers anyway). Now
consider the naive registry `map[string]Counter`. The call you want to write is
`m[name].Inc()`. The auto-address sugar expands that to `(&m[name]).Inc()` — it
needs the address of the map element. But a map may rehash and relocate its
entries when it grows, so the language forbids `&m[name]`. The result is a
compile error, not a runtime one:

```text
cannot call pointer method Inc on Counter
cannot take address of m[name] (map index expression of type Counter)
```

The fix is to store pointers: `map[string]*Counter`. Now `m[name]` yields a
`*Counter` — a pointer value, on which `Inc()` is a direct pointer-method call
with no address-taking required. The pointer is stable even when the map rehashes,
because the map moves the pointer, not the `Counter` it points at. A getter,
`Registry.Counter(name)`, creates the `*Counter` on first use and returns the same
pointer thereafter, so callers always increment the one counter behind that name.

Contrast the slice case to keep the rule straight: `s[i].Inc()` on a
`[]Counter` *does* compile, because a slice index expression is addressable and
`&s[i]` is legal. It is maps specifically — because of rehashing — that block the
pointer-method call. That is why metrics registries use `map[string]*Counter`.

Create `metrics.go`:

```go
package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Counter is a monotonic counter. Its body is an atomic.Int64 (non-copyable),
// so all methods take pointer receivers and it must be referenced as *Counter.
type Counter struct {
	n atomic.Int64
}

// Inc adds one and returns the new value.
func (c *Counter) Inc() int64 { return c.n.Add(1) }

// Add adds delta and returns the new value.
func (c *Counter) Add(delta int64) int64 { return c.n.Add(delta) }

// Value reads the current count.
func (c *Counter) Value() int64 { return c.n.Load() }

// Registry maps names to *Counter. Storing pointers is what makes
// reg.Counter(name).Inc() legal: a map element is not addressable, so a map of
// Counter values could not be incremented in place.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*Counter
}

// NewRegistry returns a ready *Registry.
func NewRegistry() *Registry {
	return &Registry{counters: make(map[string]*Counter)}
}

// Counter returns the *Counter for name, creating it on first use. The same
// pointer is returned for the same name, so all callers share one counter.
func (r *Registry) Counter(name string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok {
		c = &Counter{}
		r.counters[name] = c
	}
	return c
}

// Snapshot returns a name->value map sorted for stable output.
func (r *Registry) Snapshot() []struct {
	Name  string
	Value int64
} {
	r.mu.Lock()
	names := make([]string, 0, len(r.counters))
	for name := range r.counters {
		names = append(names, name)
	}
	out := make([]struct {
		Name  string
		Value int64
	}, 0, len(names))
	for _, name := range names {
		out = append(out, struct {
			Name  string
			Value int64
		}{name, r.counters[name].Value()})
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
```

### The runnable demo

The demo registers a couple of named counters and increments them through the
getter, then prints a sorted snapshot.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/metrics"
)

func main() {
	reg := metrics.NewRegistry()

	// reg.Counter(name) returns a *Counter, so Inc() is a direct pointer-method
	// call. A map[string]Counter would NOT allow reg.counters[name].Inc().
	reg.Counter("http_requests").Inc()
	reg.Counter("http_requests").Inc()
	reg.Counter("http_requests").Add(3)
	reg.Counter("errors").Inc()

	for _, s := range reg.Snapshot() {
		fmt.Printf("%s = %d\n", s.Name, s.Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
errors = 1
http_requests = 5
```

### Tests

The tests increment named counters through the stored `*Counter` and assert the
totals, then hammer one counter from many goroutines under `-race` to prove the
atomic body is correct. The non-addressability compile error is documented as a
commented block so the trap is visible without breaking the build.

Create `metrics_test.go`:

```go
package metrics

import (
	"fmt"
	"sync"
	"testing"
)

func TestRegistrySharesCounterByName(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Counter("hits").Inc()
	reg.Counter("hits").Add(4)

	if got := reg.Counter("hits").Value(); got != 5 {
		t.Fatalf("hits = %d, want 5", got)
	}
	if got := reg.Counter("misses").Value(); got != 0 {
		t.Fatalf("misses = %d, want 0", got)
	}

	// The value-map form would not compile:
	//   var m map[string]Counter
	//   m["hits"].Inc()
	// cannot call pointer method Inc on Counter; cannot take address of m["hits"]
}

func TestCounterConcurrentInc(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	c := reg.Counter("requests")

	const goroutines, per = 50, 1000
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				c.Inc()
			}
		}()
	}
	wg.Wait()

	if got := c.Value(); got != goroutines*per {
		t.Fatalf("count = %d, want %d", got, goroutines*per)
	}
}

func ExampleRegistry() {
	reg := NewRegistry()
	reg.Counter("a").Inc()
	reg.Counter("a").Inc()
	fmt.Println(reg.Counter("a").Value())
	// Output: 2
}
```

## Review

The registry is correct when incrementing a named counter mutates one shared
`*Counter`: `reg.Counter("hits")` returns the same pointer every time, so
`Inc`/`Add` accumulate. `TestCounterConcurrentInc` proves the atomic body under
`-race`; if the counter body were a plain `int64` with no synchronization, the
race detector would flag it and the total would be wrong.

The mistake this module exists to prevent is storing `map[string]Counter` and
trying `m[name].Inc()`. That does not compile — a map index expression is not
addressable, so the auto-address sugar cannot form `&m[name]` for the pointer
method. Store `map[string]*Counter` (or return the `*Counter` from a getter, as
here). Remember the asymmetry with slices: `s[i].Inc()` is fine because slice
elements are addressable; it is specifically the map, which may rehash, that
blocks it. Run `go vet` and `go test -race`; both must be clean.

## Resources

- [Go Language Specification: Address operators](https://go.dev/ref/spec#Address_operators) — the list of addressable operands, and why a map index is excluded.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — the atomic counter body and its non-copyable guarantee.
- [Go Language Specification: Index expressions](https://go.dev/ref/spec#Index_expressions) — map versus slice indexing semantics.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-mutex-guarded-counter-no-copy-receivers.md](04-mutex-guarded-counter-no-copy-receivers.md)
