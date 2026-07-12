# Exercise 8: Build Lock-Free Request Metrics Middleware

Every HTTP service counts requests, tracks in-flight concurrency, and tallies
errors. On the request hot path a shared mutex around those counters is pure
contention; per-counter atomics are the right tool. This module builds a metrics
middleware on `atomic.Int64` counters, exposes an immutable `Snapshot`, and pins
the one contract that a gauge always gets wrong: the in-flight count must return to
zero on *every* exit path, including a recovered panic.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
reqmetrics/                independent module: example.com/reqmetrics
  go.mod                   module example.com/reqmetrics
  reqmetrics.go            Metrics (total/inflight/errors atomics); Middleware; Snapshot() value struct
  cmd/
    demo/
      main.go              runnable demo: fire requests through the middleware, print the snapshot
  reqmetrics_test.go       N concurrent requests under -race; panic subtest pins gauge-returns-to-zero
```

- Files: `reqmetrics.go`, `cmd/demo/main.go`, `reqmetrics_test.go`.
- Implement: a `Metrics` with `total`, `inflight`, `errors` as `atomic.Int64`; a `Middleware(next http.Handler) http.Handler`; a `Snapshot() Stats` returning a plain value struct.
- Test: fire N concurrent requests via `httptest`; assert `total==N`, `inflight==0`, `errors` matches injected failures, under `-race`; a subtest injects a recovered panic and asserts `inflight` returns to zero.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/09-pointer-aliasing-and-data-races/08-lockfree-request-metrics/cmd/demo
cd go-solutions/09-pointers/09-pointer-aliasing-and-data-races/08-lockfree-request-metrics
```

### Why atomics beat a mutex here, and the defer-decrement contract

Each metric is a single machine word incremented independently: `total` on entry,
`inflight` up on entry and down on exit, `errors` on a 5xx response. None of them
share an invariant with another — `total` need not agree with `inflight` at any
instant — so there is nothing for a mutex to protect *jointly*. A shared mutex
would serialize every request through one lock on the hottest path in the service;
per-counter `atomic.Int64.Add` lets requests increment in parallel with no
contention. This is the exact inverse of the compound-state cache in Exercise 6:
there the fields shared an invariant and demanded a lock; here they are independent
words and demand atomics.

The gauge is where the bug lives. `inflight` must be incremented once on entry and
decremented once on *every* return path. Handlers return in many ways: normally,
via an early `return` on a validation error, and via a panic that a recovery
middleware catches. If you write `m.inflight.Add(1)` at the top and
`m.inflight.Add(-1)` at the bottom, a panic between them skips the decrement and
the gauge leaks upward forever — it will read a growing "in-flight" count that
never comes back down, poisoning autoscaling and alerts. The fix is
`defer m.inflight.Add(-1)` immediately after the increment: `defer` runs during
stack unwinding, so it fires even when the handler panics. The panic subtest pins
exactly this: inject a handler that panics, recover it in the middleware, and assert
`inflight` is back to zero.

`Snapshot` returns a plain `Stats` value struct (not the `Metrics` with its live
atomics). Returning a copy of the *values* is the safe boundary: the caller gets an
immutable reading and cannot touch the live counters, and `Metrics` — which holds
atomics — is never copied, honoring the "do not copy a struct with a live atomic"
rule.

Create `reqmetrics.go`:

```go
package reqmetrics

import (
	"net/http"
	"sync/atomic"
)

// Metrics holds independent request counters. Each is a single word, so atomics
// (not a shared mutex) are the right primitive on the hot path. Metrics holds
// live atomics and must never be copied; always use *Metrics.
type Metrics struct {
	total    atomic.Int64
	inflight atomic.Int64
	errors   atomic.Int64
}

// Stats is an immutable snapshot returned to callers by value.
type Stats struct {
	Total    int64
	Inflight int64
	Errors   int64
}

func New() *Metrics {
	return &Metrics{}
}

// statusRecorder captures the status code the handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Middleware records total requests, an in-flight gauge, and errors. The gauge is
// decremented with defer so it returns to zero on every path, including a
// recovered panic.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.total.Add(1)
		m.inflight.Add(1)
		defer m.inflight.Add(-1)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				// A panic skips the post-ServeHTTP error count below, so count
				// it here and turn it into a 500 response.
				m.errors.Add(1)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(rec, r)
		// Reached only on a non-panic return; a 5xx status is one error.
		if rec.status >= 500 {
			m.errors.Add(1)
		}
	})
}

// Snapshot returns the current counters as an immutable value struct.
func (m *Metrics) Snapshot() Stats {
	return Stats{
		Total:    m.total.Load(),
		Inflight: m.inflight.Load(),
		Errors:   m.errors.Load(),
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/reqmetrics"
)

func main() {
	m := reqmetrics.New()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	fail := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	okHandler := m.Middleware(ok)
	failHandler := m.Middleware(fail)

	for range 3 {
		okHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	failHandler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

	s := m.Snapshot()
	fmt.Printf("total=%d inflight=%d errors=%d\n", s.Total, s.Inflight, s.Errors)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
total=4 inflight=0 errors=1
```

### Tests

`TestConcurrentRequests` fires N requests concurrently through the middleware with a
mix of success and injected 5xx failures, then asserts `total==N`, `inflight==0`,
and `errors` equals the injected count, under `-race`. `TestPanicStillDecrements`
is the contract test: a handler that panics is recovered by the middleware, and the
in-flight gauge must be back to zero afterward — proving the `defer` decrement fires
on the panic path.

Create `reqmetrics_test.go`:

```go
package reqmetrics

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestConcurrentRequests(t *testing.T) {
	t.Parallel()

	m := New()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	fail := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	okH := m.Middleware(ok)
	failH := m.Middleware(fail)

	const total = 200
	const failures = 50

	var wg sync.WaitGroup
	for i := range total {
		h := okH
		if i < failures {
			h = failH
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	wg.Wait()

	s := m.Snapshot()
	if s.Total != total {
		t.Errorf("total = %d, want %d", s.Total, total)
	}
	if s.Inflight != 0 {
		t.Errorf("inflight = %d, want 0", s.Inflight)
	}
	if s.Errors != failures {
		t.Errorf("errors = %d, want %d", s.Errors, failures)
	}
}

func TestPanicStillDecrements(t *testing.T) {
	t.Parallel()

	m := New()
	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("handler blew up")
	})
	h := m.Middleware(boom)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/boom", nil))

	s := m.Snapshot()
	if s.Inflight != 0 {
		t.Fatalf("inflight = %d after a recovered panic, want 0 (defer decrement missing)", s.Inflight)
	}
	if s.Errors != 1 {
		t.Fatalf("errors = %d after a panic, want 1", s.Errors)
	}
	if s.Total != 1 {
		t.Fatalf("total = %d, want 1", s.Total)
	}
}

func TestSuccessDoesNotCountError(t *testing.T) {
	t.Parallel()

	m := New()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := m.Middleware(ok)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if s := m.Snapshot(); s.Errors != 0 {
		t.Fatalf("errors = %d for a 200 response, want 0", s.Errors)
	}
}
```

## Review

The middleware is correct when `total` equals the number of requests, `errors`
equals the injected failures, and `inflight` returns to zero on every path — all
under a clean `-race` run. The two mistakes it pins are the primitive choice and
the gauge contract. Use atomics, not a shared mutex, because the counters are
independent words with no joint invariant; a mutex would serialize the hot path for
nothing. And decrement the gauge with `defer` right after the increment, so an
early return or a recovered panic still balances it — `TestPanicStillDecrements` is
the machine check that a naive top/bottom increment pair would fail. Note `Snapshot`
returns a value `Stats`, never the `Metrics` with its live atomics, honoring the
no-copy rule.

## Resources

- [`net/http` — Handler and HandlerFunc](https://pkg.go.dev/net/http#Handler) — the middleware shape.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for driving handlers in tests.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `Int64.Add`/`Load` for lock-free counters and gauges.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-idempotency-guard-cas.md](09-idempotency-guard-cas.md)
