# Exercise 7: Metrics registration guard: handler constructors that register collectors exactly once

`prometheus.MustRegister` and `database/sql.Register` panic on a duplicate
registration by design — and any constructor that can run more than once per
process (a handler wired onto three routes, built fresh in every test, rebuilt
on hot reload) will eventually hit that panic in production. This exercise
reproduces the outage in miniature with a stdlib-only duplicate-panic registry,
then defuses it with a `sync.Once` registration guard.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
once-registration-guard/      module: example.com/once-registration-guard
  go.mod
  metricsguard.go             Registry, Collector, RequestCounter, Instrumentation,
                              Default, NewHandler; Once-guarded MustRegister call
  cmd/
    demo/
      main.go                 runnable demo: three constructions, one registration
  metricsguard_test.go        concurrent constructions, shared counts, unguarded
                              control panic, Example
```

- Files: `metricsguard.go`, `cmd/demo/main.go`, `metricsguard_test.go`.
- Implement: a `Registry` whose `MustRegister` panics on a duplicate collector name (mirroring real registries); a `RequestCounter` collector; an `Instrumentation` whose `NewHandler(reg, body)` may be called any number of times but registers its collector exactly once via `sync.Once`; a package-level `Default` instrumentation plus a top-level `NewHandler` for the canonical wiring path.
- Test: 100 concurrent `NewHandler` calls cause no panic and exactly one registration; requests served through handlers from different constructions all increment the one shared collector; a control test proves the unguarded path panics on the second registration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p once-registration-guard/cmd/demo
cd once-registration-guard
go mod init example.com/once-registration-guard
```

### Why registries panic, and why constructors re-run

Real metric registries treat a duplicate registration as a programmer error, not
a runtime condition: two collectors with the same name would make the scrape
output ambiguous, so `MustRegister` panics rather than silently merging or
overwriting. That contract is sound — but it collides with how constructors
actually behave in a real service. `NewHandler` is called once per route that
uses the handler, once per test that spins up the mux, and once per hot-reload
cycle in a dev loop. If the constructor registers its collector inline, the
*second* construction panics, and the panic surfaces at the worst possible
moment: in the test suite it is a confusing cross-test failure; in production it
is a crash during route wiring or config reload.

The wrong fixes are common. Registering in `init()` works but couples
registration to import (the cost and the side effect happen even if the handler
is never used, and there is no error path). Checking a plain `bool` before
registering is a data race the moment two routes are wired concurrently. The
correct shape is a `sync.Once` owned by the instrumentation, wrapping exactly
the registration call: however many times the constructor runs, whatever
goroutines run it, the `Do` closure elects one winner, the losers block until
registration completes, and the happens-before edge from the closure's return
publishes the registered state to every constructor call that follows.

One design detail matters for safety: the collector itself is a *value field*
of `Instrumentation`, not a pointer created inside the closure. That way
`RequestsTotal()` is an atomic read that is valid at any time — there is no nil
window before the first construction, and no read of once-initialized state off
the `Do` path. Only the registration side effect is `Once`-guarded, because only
the registration is the thing that must happen exactly once.

Note what the guard deliberately freezes: the *first* registry passed to
`NewHandler` wins, and later constructions ignore their `reg` argument, exactly
like code that registers against a process-global default registerer. That is
the contract you want for one process-lifetime registry; if you genuinely need
per-registry registration (rare — usually only in test harnesses), you need one
`Instrumentation` per registry, which is why the type is instantiable and
`Default` is just a package-level instance.

Create `metricsguard.go`:

```go
// Package metricsguard shows how to guard collector registration with
// sync.Once so handler constructors can run any number of times against a
// registry that panics on duplicates.
package metricsguard

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
)

// Collector is anything a Registry can hold, keyed by name.
type Collector interface {
	Name() string
}

// Registry mirrors the contract of real metric registries: registering two
// collectors with the same name is a programmer error and panics.
type Registry struct {
	mu         sync.Mutex
	collectors map[string]Collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{collectors: make(map[string]Collector)}
}

// MustRegister adds c to the registry and panics if a collector with the same
// name is already registered — the prometheus.MustRegister contract.
func (r *Registry) MustRegister(c Collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.collectors[c.Name()]; dup {
		panic(fmt.Sprintf("metricsguard: duplicate collector registration %q", c.Name()))
	}
	r.collectors[c.Name()] = c
}

// Len reports how many collectors are registered.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.collectors)
}

// RequestCounter is a trivially small collector: a named atomic counter.
type RequestCounter struct {
	n atomic.Int64
}

// Name returns the fixed metric name.
func (c *RequestCounter) Name() string { return "http_requests_total" }

// Inc adds one observed request.
func (c *RequestCounter) Inc() { c.n.Add(1) }

// Value returns the current count.
func (c *RequestCounter) Value() int64 { return c.n.Load() }

// Instrumentation owns one shared request counter and registers it exactly
// once, no matter how many handlers are constructed from it. The counter is a
// value field, so reading it is always safe; only the registration side effect
// is Once-guarded.
type Instrumentation struct {
	once          sync.Once
	requests      RequestCounter
	registrations atomic.Int64
}

// NewHandler returns an instrumented handler that writes body with status 200.
// It may be called any number of times, from any goroutine; the shared counter
// is registered on reg exactly once (first registry wins, like a process-global
// default registerer).
func (in *Instrumentation) NewHandler(reg *Registry, body string) http.Handler {
	in.once.Do(func() {
		reg.MustRegister(&in.requests)
		in.registrations.Add(1)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		in.requests.Inc()
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	})
}

// Registrations reports how many times the registration closure ran; it must
// be 0 (never constructed) or 1.
func (in *Instrumentation) Registrations() int64 { return in.registrations.Load() }

// RequestsTotal reports the shared request count across every handler built
// from this Instrumentation.
func (in *Instrumentation) RequestsTotal() int64 { return in.requests.Value() }

// Default is the package-level instrumentation used by the top-level
// NewHandler, mirroring how promauto uses the default registerer.
var Default = &Instrumentation{}

// NewHandler constructs an instrumented handler against Default.
func NewHandler(reg *Registry, body string) http.Handler {
	return Default.NewHandler(reg, body)
}
```

### The runnable demo

The demo wires the same instrumentation onto two routes (three constructions
total — `/healthz` is built twice, as a mux rebuild would), serves a request
through each returned handler, and shows one registration and one shared count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	metricsguard "example.com/once-registration-guard"
)

func main() {
	reg := metricsguard.NewRegistry()

	health := metricsguard.NewHandler(reg, "ok")
	users := metricsguard.NewHandler(reg, "[]")
	healthAgain := metricsguard.NewHandler(reg, "ok") // rebuild: no duplicate panic

	for _, h := range []http.Handler{health, users, healthAgain} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	}

	fmt.Println("registrations:", metricsguard.Default.Registrations())
	fmt.Println("collectors in registry:", reg.Len())
	fmt.Println("requests_total:", metricsguard.Default.RequestsTotal())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
registrations: 1
collectors in registry: 1
requests_total: 3
```

### Tests

`TestConcurrentConstructionRegistersOnce` is the outage-shaped test: 100
goroutines construct handlers against a registry that would panic on the second
registration, and the test passes only because the `Once` elects one
registrant. `TestSharedCounterAcrossConstructions` proves handlers from
different constructions feed the same collector. The control test
`TestUnguardedDuplicatePanics` documents the failure mode the guard prevents:
registering two same-named collectors directly does panic. Each test builds its
own `Instrumentation` and `Registry` so they can run in parallel without
touching `Default`.

Create `metricsguard_test.go`:

```go
package metricsguard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestConcurrentConstructionRegistersOnce(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	var in Instrumentation

	const constructions = 100
	var wg sync.WaitGroup
	wg.Add(constructions)
	for i := range constructions {
		go func() {
			defer wg.Done()
			h := in.NewHandler(reg, fmt.Sprintf("route-%d", i))
			if h == nil {
				t.Error("NewHandler returned nil")
			}
		}()
	}
	wg.Wait()

	if got := in.Registrations(); got != 1 {
		t.Fatalf("Registrations() = %d, want exactly 1", got)
	}
	if got := reg.Len(); got != 1 {
		t.Fatalf("registry holds %d collectors, want 1", got)
	}
}

func TestSharedCounterAcrossConstructions(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	var in Instrumentation

	tests := []struct {
		name     string
		body     string
		requests int
	}{
		{name: "health route", body: "ok", requests: 2},
		{name: "users route", body: "[]", requests: 3},
		{name: "health rebuilt", body: "ok", requests: 1},
	}

	total := 0
	for _, tt := range tests {
		h := in.NewHandler(reg, tt.body)
		for range tt.requests {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", tt.name, rec.Code)
			}
			if rec.Body.String() != tt.body {
				t.Fatalf("%s: body = %q, want %q", tt.name, rec.Body.String(), tt.body)
			}
		}
		total += tt.requests
	}

	if got := in.RequestsTotal(); got != int64(total) {
		t.Fatalf("RequestsTotal() = %d, want %d (one shared collector)", got, total)
	}
	if got := in.Registrations(); got != 1 {
		t.Fatalf("Registrations() = %d, want 1", got)
	}
}

func TestUnguardedDuplicatePanics(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	var c1, c2 RequestCounter
	reg.MustRegister(&c1)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("second MustRegister with the same name did not panic")
		}
	}()
	reg.MustRegister(&c2) // same fixed name: must panic
}

func Example() {
	reg := NewRegistry()
	var in Instrumentation

	h := in.NewHandler(reg, "ok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	fmt.Println(rec.Code, in.Registrations(), in.RequestsTotal())
	// Output: 200 1 1
}
```

## Review

The guard is correct when construction count and registration count are fully
decoupled: 100 concurrent constructions, one registration, no panic. The
control test matters as much as the guarded ones — it proves the registry
really does enforce the duplicate panic, so the concurrent test is passing
because of the `Once`, not because the registry is lenient. Note the two
deliberate design choices: the counter is a value field so reading it never
depends on `Do` having run, and the first registry wins so the semantics match
a process-global default registerer. The classic mistakes here are registering
in the handler constructor with no guard (second test file to construct the
handler dies), guarding with an unsynchronized `bool` (a data race under
concurrent route wiring), and putting the registration in `init()` (no error
path, cost paid on unused imports, and ordering tied to the import graph). Run
`go test -count=1 -race` and confirm all tests pass with the detector on.

## Resources

- [sync.Once — pkg.go.dev](https://pkg.go.dev/sync#Once)
- [prometheus.MustRegister — pkg.go.dev](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus#MustRegister)
- [net/http/httptest — pkg.go.dev](https://pkg.go.dev/net/http/httptest)

---

Prev: [06-panic-safe-init.md](06-panic-safe-init.md) | Back to [00-concepts.md](00-concepts.md) | Next: [08-per-key-migration-once.md](08-per-key-migration-once.md)
