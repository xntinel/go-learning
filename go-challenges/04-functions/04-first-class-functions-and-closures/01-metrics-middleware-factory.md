# Exercise 1: Per-Route Metrics Middleware as a Closure Factory

The single most common shape of first-class-function code in a Go backend is the
middleware factory: a function that takes dependencies and returns an
`http.Handler` wrapper. This module builds a per-route request-count middleware
exactly the way it ships in production — a `Metrics(counter, route)` factory that
returns a closure capturing the counter and the route, wrapping the next handler
in another closure that captures `next`. The counter is a mutex-guarded map whose
`Snapshot` returns a defensive copy.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
metricsmw/                 independent module: example.com/metricsmw
  go.mod                   go 1.26
  middleware.go            Counter, MapCounter, Middleware, Metrics factory
  cmd/
    demo/
      main.go              wires Metrics into an http.ServeMux and drives requests
  middleware_test.go       per-route, route-separation, concurrency, snapshot-copy
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: a `Metrics(counter Counter, route string) Middleware` factory returning a closure that increments `counter` for `route` then calls `next`, and a `MapCounter` whose `Snapshot` returns a copy of its internal map.
- Test: increment per route N times and assert `Snapshot()[route] == N`; two routes stay separate; 64 concurrent requests through one handler total correctly under `-race`; mutating a returned snapshot does not corrupt internal state.
- Verify: `go test -count=1 -race ./...`

### The chain of closures

`Metrics` is a *factory*: it takes the dependencies (a `Counter` and a `route`)
and returns a `Middleware`, which is itself a function `func(Handler) Handler`.
The returned `Middleware` is a closure that captures `counter` and `route`. When
you apply it to a `next` handler, it returns yet another closure — the actual
`http.HandlerFunc` — that captures `next`. So there are two nested closures, each
capturing exactly what the layer below it needs. That nesting is what
"first-class functions plus closures" looks like in real HTTP plumbing: the outer
closure holds configuration bound at wiring time, the inner closure holds the
handler bound at mount time, and neither needs a struct.

The counter is where the concurrency lives. An HTTP server calls a handler from
many goroutines at once, so `Inc` reads and writes a shared map and must be
synchronized. The closure itself is stateless and needs no lock; the *state it
captures* — the `Counter` — carries the mutex. `MapCounter.Inc` takes the lock,
bumps the count, and releases it.

`Snapshot` is the subtle part. Returning the internal `counts` map directly would
hand callers a live reference to private state: a caller iterating or, worse,
mutating that map would race with `Inc` and could corrupt the counts. So
`Snapshot` allocates a fresh map and copies each entry under the lock, returning
something the caller can read and modify freely without touching the live
counters. The snapshot-copy test proves this by mutating one snapshot and
checking the next is unaffected.

Create `middleware.go`:

```go
package metricsmw

import (
	"net/http"
	"sync"
)

// Counter records a hit for a route. Implementations must be safe for
// concurrent use, since an http.Handler runs on many goroutines at once.
type Counter interface {
	Inc(route string)
}

// MapCounter is a concurrency-safe route -> count map.
type MapCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func NewMapCounter() *MapCounter {
	return &MapCounter{counts: make(map[string]int)}
}

func (m *MapCounter) Inc(route string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[route]++
}

// Snapshot returns a copy of the counts. Callers may read or mutate the result
// without affecting the live counters.
func (m *MapCounter) Snapshot() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.counts))
	for k, v := range m.counts {
		out[k] = v
	}
	return out
}

// Handler is an alias so the Middleware type reads cleanly.
type Handler = http.Handler

// Middleware wraps a Handler, returning a new Handler.
type Middleware func(Handler) Handler

// Metrics returns a Middleware that increments counter for route on every
// request before delegating to next. The returned Middleware captures counter
// and route; the handler it produces captures next.
func Metrics(counter Counter, route string) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Inc(route)
			next.ServeHTTP(w, r)
		})
	}
}
```

### The runnable demo

The demo wires two routes into an `http.ServeMux`, each wrapped by its own
`Metrics` middleware, then drives three in-process requests with `httptest` so
the output is deterministic. Two hits on `/users` and one on `/orders` produce a
stable snapshot.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/metricsmw"
)

func main() {
	counter := metricsmw.NewMapCounter()
	mux := http.NewServeMux()
	mux.Handle("/users", metricsmw.Metrics(counter, "/users")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "users")
	})))
	mux.Handle("/orders", metricsmw.Metrics(counter, "/orders")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "orders")
	})))

	for _, route := range []string{"/users", "/orders", "/users"} {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
	}

	snap := counter.Snapshot()
	fmt.Printf("/users=%d\n", snap["/users"])
	fmt.Printf("/orders=%d\n", snap["/orders"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/users=2
/orders=1
```

### Tests

The tests cover the four properties that matter. `TestMetricsIncrementsPerRoute`
drives the same handler three times and asserts the count. `TestMetricsSeparates`
proves two routes accumulate independently. `TestMetricsConcurrent` fires 64
goroutines at one handler and asserts the total is exactly 64 — this is the test
that fails under `-race` if the counter's mutex is missing.
`TestSnapshotDoesNotMutateInternalState` is the defensive-copy proof: it mutates
a returned snapshot and checks the next snapshot is unaffected.

Create `middleware_test.go`:

```go
package metricsmw

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func okHandler() Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "ok")
	})
}

func drive(h Handler, route string) {
	req := httptest.NewRequest(http.MethodGet, route, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
}

func TestMetricsIncrementsPerRoute(t *testing.T) {
	t.Parallel()
	counter := NewMapCounter()
	handler := Metrics(counter, "/users")(okHandler())

	for range 3 {
		drive(handler, "/users")
	}

	if got := counter.Snapshot()["/users"]; got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
}

func TestMetricsSeparatesRoutes(t *testing.T) {
	t.Parallel()
	counter := NewMapCounter()
	users := Metrics(counter, "/users")(okHandler())
	orders := Metrics(counter, "/orders")(okHandler())

	for range 2 {
		drive(users, "/users")
	}
	for range 5 {
		drive(orders, "/orders")
	}

	snap := counter.Snapshot()
	if snap["/users"] != 2 {
		t.Fatalf("users = %d, want 2", snap["/users"])
	}
	if snap["/orders"] != 5 {
		t.Fatalf("orders = %d, want 5", snap["/orders"])
	}
}

func TestMetricsConcurrent(t *testing.T) {
	t.Parallel()
	counter := NewMapCounter()
	handler := Metrics(counter, "/api")(okHandler())

	const requests = 64
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			drive(handler, "/api")
		}()
	}
	wg.Wait()

	if got := counter.Snapshot()["/api"]; got != requests {
		t.Fatalf("count = %d, want %d", got, requests)
	}
}

func TestSnapshotDoesNotMutateInternalState(t *testing.T) {
	t.Parallel()
	counter := NewMapCounter()
	handler := Metrics(counter, "/x")(okHandler())
	drive(handler, "/x")

	first := counter.Snapshot()
	first["/x"] = 999
	first["/injected"] = 7

	second := counter.Snapshot()
	if second["/x"] != 1 {
		t.Fatalf("internal count corrupted: got %d, want 1", second["/x"])
	}
	if _, ok := second["/injected"]; ok {
		t.Fatal("mutating snapshot leaked a key into internal state")
	}
}

func ExampleMetrics() {
	counter := NewMapCounter()
	handler := Metrics(counter, "/health")(okHandler())
	drive(handler, "/health")
	fmt.Println(counter.Snapshot()["/health"])
	// Output: 1
}
```

## Review

The middleware is correct when the count for a route equals the number of
requests routed through that route's wrapper, and never leaks or corrupts across
routes or goroutines. The two structural traps are the ones the tests target:
forgetting the mutex, which `TestMetricsConcurrent` exposes only under `-race`,
and returning the internal map from `Snapshot`, which `TestSnapshotDoes...`
exposes by mutating the copy. Note that the closure needs no lock of its own — it
is the captured `Counter` that carries the mutex, because the closure is
stateless and the state it shares across goroutines lives in the counter. Run
`go test -race` to confirm both.

## Resources

- [pkg.go.dev: net/http Handler and HandlerFunc](https://pkg.go.dev/net/http#HandlerFunc) — the adapter that turns a func into a Handler.
- [pkg.go.dev: net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for in-process handler tests.
- [Go Blog: Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — why the old `route := route` capture is no longer needed.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-middleware-chain-compose.md](02-middleware-chain-compose.md)
