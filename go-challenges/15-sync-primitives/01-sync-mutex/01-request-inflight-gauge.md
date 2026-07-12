# Exercise 1: In-flight request gauge for HTTP middleware

Every backend service exports a "requests currently in flight" number — it feeds
autoscaling, load-shedding, and the concurrency panel on a dashboard. It is the
smallest useful piece of shared, longer-than-a-request state a service has, and
it is exactly a mutex-guarded integer wrapped as middleware. This module builds
that gauge: increment on request entry, decrement via `defer` on exit, read for
`/metrics`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
inflight/                    independent module: example.com/inflight
  go.mod                     go 1.26
  gauge.go                   type Gauge; Inc, Dec, Value, Middleware
  cmd/
    demo/
      main.go                runnable demo: drive the middleware, print in-flight
  gauge_test.go              contention Inc/Dec test, httptest middleware test, Example
```

- Files: `gauge.go`, `cmd/demo/main.go`, `gauge_test.go`.
- Implement: a `Gauge` (mutex + int64) with `Inc()`, `Dec()`, `Value() int64`, and `Middleware(http.Handler) http.Handler` that increments on entry and decrements on exit.
- Test: N goroutines each doing `perG` Inc/Dec pairs assert final `Value()==0`; a concurrent httptest drive asserts the gauge is never negative inside the handler and returns to 0.
- Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/15-sync-primitives/01-sync-mutex/01-request-inflight-gauge/cmd/demo
cd go-solutions/15-sync-primitives/01-sync-mutex/01-request-inflight-gauge
```

### Why a mutex and not just the raw int

A concurrency gauge is read and written from every request goroutine at once. A
plain `n++` from two goroutines is a data race: `++` is a load, an add, and a
store, and two goroutines can interleave those and lose an update, so the gauge
drifts and eventually reports nonsense. Guarding `Inc`, `Dec`, and `Value` with
one mutex makes each a critical section and gives the reader a happens-before
edge to every prior write, so `/metrics` sees a coherent value.

The middleware is where the discipline shows. It increments on entry and, the
important part, decrements with `defer`, so the count is corrected even if the
downstream handler panics. Without the `defer`, a single panicking handler would
leak a permanent +1 and the gauge would climb forever. The gauge itself does no
slow work under the lock: `Inc`/`Dec` touch one integer and release, so the lock
is never on the request's critical path for more than a couple of instructions —
the tail-latency rule applied to its simplest case.

The receiver is a pointer on every method. That is not stylistic: a value
receiver would copy the `Gauge` (and its mutex) on every call, so the
"critical section" would guard a throwaway copy and synchronize nothing. `go
vet` `copylocks` would flag it, and `-race` would catch the resulting drift.

Create `gauge.go`:

```go
package inflight

import (
	"net/http"
	"sync"
)

// Gauge tracks the number of in-flight requests. Its zero value is ready to use.
type Gauge struct {
	mu sync.Mutex
	n  int64
}

// Inc records one more in-flight request.
func (g *Gauge) Inc() {
	g.mu.Lock()
	g.n++
	g.mu.Unlock()
}

// Dec records one fewer in-flight request.
func (g *Gauge) Dec() {
	g.mu.Lock()
	g.n--
	g.mu.Unlock()
}

// Value returns the current in-flight count.
func (g *Gauge) Value() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

// Middleware wraps h so that every request in flight through h is counted on g.
// It decrements via defer, so the count is corrected even if h panics.
func (g *Gauge) Middleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.Inc()
		defer g.Dec()
		h.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo drives three sequential requests through the middleware and prints the
in-flight count observed inside each handler (always 1, since they do not
overlap) and the count after all of them return (0).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/inflight"
)

func main() {
	var g inflight.Gauge
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("in-flight during request: %d\n", g.Value())
		w.WriteHeader(http.StatusOK)
	}))

	for range 3 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		h.ServeHTTP(rec, req)
	}

	fmt.Printf("in-flight after all requests: %d\n", g.Value())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-flight during request: 1
in-flight during request: 1
in-flight during request: 1
in-flight after all requests: 0
```

### Tests

`TestGaugeReturnsToZero` is the contention proof: N goroutines each do `perG`
`Inc`/`Dec` pairs, and because the pairs are balanced the final value must be
exactly 0. If `Inc`/`Dec` were unsynchronized, lost updates would leave a
nonzero residue and `-race` would fire. `TestMiddlewareNeverNegative` drives the
middleware concurrently with `httptest.NewRecorder`; inside each handler it
reads the live gauge, records the maximum with an `atomic.Int64`, and asserts
the value is at least 1 (this request is in flight) and never negative, then
asserts the gauge is back to 0 after all requests drain.

Create `gauge_test.go`:

```go
package inflight

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGaugeReturnsToZero(t *testing.T) {
	t.Parallel()

	var g Gauge
	const goroutines, perG = 200, 500

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			for range perG {
				g.Inc()
				g.Dec()
			}
		}()
	}
	wg.Wait()

	if got := g.Value(); got != 0 {
		t.Fatalf("Value() = %d after balanced Inc/Dec, want 0", got)
	}
}

func TestMiddlewareNeverNegative(t *testing.T) {
	t.Parallel()

	var g Gauge
	var maxSeen atomic.Int64
	h := g.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := g.Value()
		if v < 1 {
			t.Errorf("in-flight = %d inside handler, want >= 1", v)
		}
		for {
			old := maxSeen.Load()
			if v <= old || maxSeen.CompareAndSwap(old, v) {
				break
			}
		}
		w.WriteHeader(http.StatusOK)
	}))

	const requests = 100
	var wg sync.WaitGroup
	wg.Add(requests)
	for range requests {
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)
		}()
	}
	wg.Wait()

	if got := g.Value(); got != 0 {
		t.Fatalf("Value() = %d after all requests drained, want 0", got)
	}
	if maxSeen.Load() < 1 {
		t.Fatalf("max in-flight observed = %d, want >= 1", maxSeen.Load())
	}
}

func ExampleGauge() {
	var g Gauge
	g.Inc()
	g.Inc()
	g.Dec()
	fmt.Println(g.Value())
	// Output: 1
}
```

## Review

The gauge is correct when every mutation is balanced and every access is under
the lock. The zero-return test is the sharpest check: `Inc`/`Dec` pairs must net
to exactly 0, and any lost update from an unguarded `++`/`--` shows up as a
nonzero residue plus a `-race` report. The middleware test proves the `defer
g.Dec()` actually runs and that a reader inside a handler never sees a negative
or missing count.

The mistakes to avoid are the pointer-receiver regression — a value receiver
copies the mutex and silently desynchronizes, caught by `go vet` `copylocks` and
`-race` — and forgetting `defer` on the decrement, which leaks a permanent +1
the first time a handler panics. Keep the critical section to the single integer
operation; the gauge must never do work under its lock. Run `go test -race` to
confirm.

## Resources

- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — Lock/Unlock and the "must not be copied" contract.
- [`net/http` Handler and HandlerFunc](https://pkg.go.dev/net/http#Handler) — the middleware shape used here.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for driving handlers in tests.
- [Data Race Detector](https://go.dev/doc/articles/race_detector) — how `-race` finds the lost-update bug.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-labeled-metrics-registry.md](02-labeled-metrics-registry.md)
