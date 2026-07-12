# Exercise 1: Per-Request Metrics Counters in HTTP Middleware

Every backend eventually needs to answer "how many requests, how many errors, how
many bytes" without a lock on the request path. This exercise builds the counter
core of an HTTP metrics middleware with `atomic.Int64` fields, so that thousands of
concurrent requests each bump the counters lock-free and the totals come out
exactly right.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
reqmetrics/                independent module: example.com/reqmetrics
  go.mod
  metrics.go               type Metrics; RecordRequest, Snapshot, Reset, String, Middleware
  cmd/
    demo/
      main.go              records a few requests and prints the snapshot
  metrics_test.go          concurrent-exactness test, httptest middleware test, Example
```

- Files: `metrics.go`, `cmd/demo/main.go`, `metrics_test.go`.
- Implement: a `Metrics` with `atomic.Int64` counters for requests, errors, and bytes; `RecordRequest`, `Snapshot`, `Reset`, `String`, and an `http` `Middleware`.
- Test: 100 goroutines x 500 `RecordRequest` calls asserting every total is exact; an `httptest` round-trip through the middleware; an `Example` pinning `String`.
- Verify: `go test -count=1 -race ./...`

### Why atomics, not a mutex, here

Each counter is a single `int64` and each update is an independent `Add`. There is
no invariant tying requests, errors, and bytes together — they are three separate
tallies — so there is nothing for a mutex to protect that atomics do not already
handle. `RecordRequest` does three independent `Add` operations; a concurrent
caller interleaving between them cannot corrupt anything, because the only property
that matters is that each counter's total equals the number of increments applied
to it, and `atomic.Int64.Add` guarantees exactly that even under maximum
contention. This is the lost-update proof: with a plain `int64` and `n++`, two
goroutines that read the same value and both write back lose one increment; with
`Add(1)` every increment is applied.

`Snapshot` reads the three counters with three `Load`s. Note the honest caveat: the
three loads are not one atomic transaction, so a snapshot taken during heavy
traffic may catch `requests` already incremented for a request whose `bytes` have
not landed yet. For metrics that is completely acceptable — the numbers are
monotonic and converge — and it is exactly the "atomics protect one value, not a
multi-value transaction" trade-off from the concepts. If you needed a perfectly
coherent triple you would publish an immutable snapshot struct through an
`atomic.Pointer` (Exercise 8) or take a mutex.

The `Middleware` wraps a handler, captures the response status and byte count with a
small `http.ResponseWriter` shim, and records one request per call — a server error
(status >= 500) counts as an error. This is the real shape of a metrics middleware:
the counters are the hot part, kept lock-free.

Create `metrics.go`:

```go
package metrics

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics holds lock-free per-request counters for an HTTP service. Each field
// is an independent tally; there is no cross-field invariant, so atomics fit.
type Metrics struct {
	requests atomic.Int64
	errors   atomic.Int64
	bytes    atomic.Int64
}

func New() *Metrics { return &Metrics{} }

// RecordRequest tallies one request. isErr marks it as failed; size is the
// number of response bytes. Each counter is bumped with an independent Add.
func (m *Metrics) RecordRequest(isErr bool, size int64) {
	m.requests.Add(1)
	if isErr {
		m.errors.Add(1)
	}
	m.bytes.Add(size)
}

// Snapshot reads the three counters. The reads are independent, so under live
// traffic the triple is monotonic-but-not-instantaneous, which is fine for
// metrics.
func (m *Metrics) Snapshot() (requests, errors, bytes int64) {
	return m.requests.Load(), m.errors.Load(), m.bytes.Load()
}

// Reset zeroes the counters, e.g. at the start of a scrape window.
func (m *Metrics) Reset() {
	m.requests.Store(0)
	m.errors.Store(0)
	m.bytes.Store(0)
}

func (m *Metrics) String() string {
	r, e, b := m.Snapshot()
	return fmt.Sprintf("requests=%d errors=%d bytes=%d", r, e, b)
}

// Middleware wraps next, recording one request per call with its response size
// and whether the status was a server error (>= 500).
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		m.RecordRequest(rec.status >= 500, rec.bytes)
	})
}

// statusRecorder captures the status code and byte count of a response.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reqmetrics"
)

func main() {
	m := metrics.New()
	m.RecordRequest(false, 256)
	m.RecordRequest(true, 128)
	m.RecordRequest(false, 512)
	fmt.Println(m)

	r, e, b := m.Snapshot()
	fmt.Printf("snapshot: r=%d e=%d b=%d\n", r, e, b)

	m.Reset()
	fmt.Println("after reset:", m)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests=3 errors=1 bytes=896
snapshot: r=3 e=1 b=896
after reset: requests=0 errors=0 bytes=0
```

### Tests

`TestConcurrentExact` is the hard test: 100 goroutines each call `RecordRequest`
500 times, and every counter must come out exactly right — proof that no increment
was lost. `TestMiddleware` drives a real request through the middleware with
`httptest` and asserts the counters reflect the status and body size.

Create `metrics_test.go`:

```go
package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestConcurrentExact(t *testing.T) {
	t.Parallel()

	m := New()
	const goroutines = 100
	const perGoroutine = 500

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			for i := range perGoroutine {
				m.RecordRequest(g%5 == 0, int64(i))
			}
		}()
	}
	wg.Wait()

	r, e, b := m.Snapshot()
	if want := int64(goroutines * perGoroutine); r != want {
		t.Fatalf("requests = %d, want %d", r, want)
	}
	// Every 5th goroutine (g%5==0) records an error on each call.
	wantErrors := int64(goroutines/5) * int64(perGoroutine)
	if e != wantErrors {
		t.Fatalf("errors = %d, want %d", e, wantErrors)
	}
	// bytes = sum(i for i in 0..perGoroutine-1) per goroutine, over all.
	wantBytes := int64(goroutines) * int64(perGoroutine*(perGoroutine-1)/2)
	if b != wantBytes {
		t.Fatalf("bytes = %d, want %d", b, wantBytes)
	}
}

func TestMiddleware(t *testing.T) {
	t.Parallel()

	m := New()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boom" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		fmt.Fprint(w, "hello")
	}))

	for _, path := range []string{"/", "/boom"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	r, e, b := m.Snapshot()
	if r != 2 {
		t.Fatalf("requests = %d, want 2", r)
	}
	if e != 1 {
		t.Fatalf("errors = %d, want 1 (only /boom)", e)
	}
	if b != int64(len("hello")*2) {
		t.Fatalf("bytes = %d, want %d", b, len("hello")*2)
	}
}

func TestReset(t *testing.T) {
	t.Parallel()

	m := New()
	m.RecordRequest(true, 10)
	m.Reset()
	if got := m.String(); got != "requests=0 errors=0 bytes=0" {
		t.Fatalf("after Reset = %q", got)
	}
}

func ExampleMetrics_String() {
	m := New()
	m.RecordRequest(false, 100)
	fmt.Println(m)
	// Output: requests=1 errors=0 bytes=100
}
```

## Review

The counters are correct when each total equals the number of increments applied,
independent of scheduling — `TestConcurrentExact` is the ground truth for that, and
it must pass under `-race`. The two mistakes to avoid: do not add a mutex "to be
safe" around three independent `Add`s (it buys nothing and reintroduces the
contention you removed), and do not read `Snapshot` as if the three loads were one
instant — they are monotonic, not transactional, which is the right trade for
metrics. If you later needed a perfectly coherent triple, publish an immutable
snapshot through an `atomic.Pointer` instead of loading three counters.

## Resources

- [`sync/atomic` package](https://pkg.go.dev/sync/atomic) — `Int64.Add`, `Load`, `Store`.
- [`net/http` Handler and ResponseWriter](https://pkg.go.dev/net/http#ResponseWriter) — the interfaces the middleware wraps.
- [The Go Memory Model](https://go.dev/ref/mem) — why independent atomic adds never lose updates.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-drain-flag-readiness-gate.md](02-drain-flag-readiness-gate.md)
