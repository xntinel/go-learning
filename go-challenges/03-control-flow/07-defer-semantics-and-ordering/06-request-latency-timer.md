# Exercise 6: HTTP Middleware — Deferred Latency Timer (Arg-Eval Trap)

Latency middleware is where the argument-evaluation rule of `defer` bites hardest.
`defer metrics.Observe(time.Since(start))` looks right and records garbage: the
`time.Since(start)` is evaluated at the `defer` statement — a few microseconds
after `start` was set — so every request reports a near-zero duration. This
exercise builds the correct middleware with a deferred *closure*, captures the
response status through a wrapped `ResponseWriter`, and proves both the fix and the
bug with `httptest`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
latency/                     independent module: example.com/latency
  go.mod                     module example.com/latency
  latency.go                 Metrics, statusRecorder, Timing (closure), TimingBroken (arg-eval)
  cmd/
    demo/
      main.go                runnable demo: time a sleeping handler through the middleware
  latency_test.go            observed >= sleep; broken variant records ~0; status captured
```

- Files: `latency.go`, `cmd/demo/main.go`, `latency_test.go`.
- Implement: `Timing(m Metrics, next http.Handler) http.Handler` that records `start`, wraps the `ResponseWriter` to capture the status, and defers a *closure* calling `m.Observe(time.Since(start), status)`; plus `TimingBroken` using the arg-eval form to demonstrate the bug.
- Test (`httptest`): a handler that sleeps a known duration yields an observed latency `>= sleep`; the broken variant records `~0`; a handler writing 503 has its status captured.
- Verify: `go test -count=1 -race ./...`

### Why the closure moves the measurement to return time

`defer`'s rule: the deferred call's *arguments* are evaluated at the `defer`
statement; only the *call* is postponed. So in `defer metrics.Observe(time.Since(start))`,
`time.Since(start)` runs immediately — right after `start := time.Now()` — and the
value it produces (essentially zero) is frozen and handed to `Observe` when the
call finally fires at return. The handler could take 800ms; the metric still says
zero. The same trap hits the status: if you pass `rw.status` as an argument, it is
read at the `defer` statement, *before* the handler runs, so it is always the
default 200 no matter what the handler writes.

The closure form fixes both. `defer func() { m.Observe(time.Since(start), rw.status) }()`
evaluates the *closure value* at the `defer` statement, but its body — the
`time.Since(start)` call and the `rw.status` read — runs at return, after the
handler has executed. Now the duration reflects the real handler time and the
status reflects what the handler actually wrote.

Capturing the status needs a wrapped `http.ResponseWriter`. The stdlib gives no
way to read back the code a handler passed to `WriteHeader`, so `statusRecorder`
embeds the real writer, defaults its `status` to `http.StatusOK` (the value the
net/http server uses when a handler writes a body without calling `WriteHeader`),
and overrides `WriteHeader` to record the code before delegating.

Create `latency.go`:

```go
package latency

import (
	"net/http"
	"time"
)

// Metrics receives one observation per request.
type Metrics interface {
	Observe(d time.Duration, status int)
}

// statusRecorder wraps http.ResponseWriter to capture the status code, which the
// stdlib otherwise does not expose after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Timing measures each request's latency correctly: the deferred CLOSURE reads
// time.Since(start) and the captured status at return time, after next runs.
func Timing(m Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			m.Observe(time.Since(start), rec.status)
		}()

		next.ServeHTTP(rec, r)
	})
}

// TimingBroken is the anti-pattern: the deferred call's arguments are evaluated
// at the defer statement (before next runs), so elapsed is ~0 and status is the
// default. (Writing it as the one-liner `defer m.Observe(time.Since(start), ...)`
// is flagged by `go vet` precisely because time.Since would not be deferred; the
// two-line form here has the same broken behavior without the vet diagnostic, and
// exists only to prove the bug in a test.)
func TimingBroken(m Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		elapsed := time.Since(start) // evaluated NOW: ~0, long before next runs
		status := rec.status         // read NOW: the default, not what next writes
		defer m.Observe(elapsed, status)

		next.ServeHTTP(rec, r)
	})
}
```

### The runnable demo

The demo wires the middleware around a handler that sleeps 10ms, drives it with an
`httptest` server, and prints whether the observed latency reflects the real sleep.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/latency"
)

// chanMetrics hands each observation to the main goroutine over a channel, which
// also synchronizes the read against the handler goroutine's write.
type chanMetrics struct {
	ch chan obs
}

type obs struct {
	d      time.Duration
	status int
}

func (m *chanMetrics) Observe(d time.Duration, status int) {
	m.ch <- obs{d: d, status: status}
}

func main() {
	m := &chanMetrics{ch: make(chan obs, 1)}

	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(latency.Timing(m, slow))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	resp.Body.Close()

	o := <-m.ch
	fmt.Printf("status=%d latency_ge_10ms=%v\n", o.status, o.d >= 10*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 latency_ge_10ms=true
```

### Tests

`TestTimingMeasuresRealLatency` drives a handler that sleeps 15ms and asserts the
observed duration is at least the sleep. `TestBrokenVariantRecordsZero` drives the
same handler through the arg-eval middleware and asserts the observed duration is
far below the sleep (the frozen near-zero). `TestStatusCaptured` asserts a 503
handler's status reaches the metric. A channel-backed fake synchronizes the
handler goroutine's `Observe` with the test's read, so `-race` stays clean.

Create `latency_test.go`:

```go
package latency

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type chanMetrics struct {
	ch chan obs
}

type obs struct {
	d      time.Duration
	status int
}

func newChanMetrics() *chanMetrics { return &chanMetrics{ch: make(chan obs, 1)} }

func (m *chanMetrics) Observe(d time.Duration, status int) { m.ch <- obs{d: d, status: status} }

func (m *chanMetrics) wait(t *testing.T) obs {
	t.Helper()
	select {
	case o := <-m.ch:
		return o
	case <-time.After(2 * time.Second):
		t.Fatal("Observe was never called")
		return obs{}
	}
}

func sleepHandler(d time.Duration, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		w.WriteHeader(status)
	})
}

func TestTimingMeasuresRealLatency(t *testing.T) {
	t.Parallel()

	const sleep = 15 * time.Millisecond
	m := newChanMetrics()
	srv := httptest.NewServer(Timing(m, sleepHandler(sleep, http.StatusOK)))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	o := m.wait(t)
	if o.d < sleep {
		t.Fatalf("observed latency = %v, want >= %v", o.d, sleep)
	}
}

func TestBrokenVariantRecordsZero(t *testing.T) {
	t.Parallel()

	const sleep = 15 * time.Millisecond
	m := newChanMetrics()
	srv := httptest.NewServer(TimingBroken(m, sleepHandler(sleep, http.StatusOK)))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	o := m.wait(t)
	// The arg-eval form froze time.Since(start) at ~0, long before the sleep.
	if o.d >= sleep {
		t.Fatalf("broken variant observed %v; expected a near-zero value below %v", o.d, sleep)
	}
}

func TestStatusCaptured(t *testing.T) {
	t.Parallel()

	m := newChanMetrics()
	srv := httptest.NewServer(Timing(m, sleepHandler(0, http.StatusServiceUnavailable)))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	o := m.wait(t)
	if o.status != http.StatusServiceUnavailable {
		t.Fatalf("captured status = %d, want %d", o.status, http.StatusServiceUnavailable)
	}
}
```

## Review

The middleware is correct when the observed duration reflects the handler's real
runtime and the status reflects what the handler wrote. `TestTimingMeasuresRealLatency`
and `TestBrokenVariantRecordsZero` are a matched pair: the same sleeping handler
records `>= 15ms` through the closure form and `~0` through the arg-eval form, which
is the whole lesson in two assertions. The rule to internalize: anything that must
be evaluated at *return* time — an elapsed duration, a final status, a row count —
belongs in the deferred closure's *body*, never in the deferred call's argument
list. The channel-backed metric is also a reusable pattern for testing middleware:
it synchronizes the handler-goroutine write with the test-goroutine read so the
race detector stays quiet.

## Resources

- [`time.Since`](https://pkg.go.dev/time#Since) — the elapsed-time helper whose evaluation timing is the trap.
- [`net/http` Handler and ResponseWriter](https://pkg.go.dev/net/http#Handler) — the middleware and status-capture surface.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — driving the middleware in-memory.
- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements) — argument evaluation at the defer statement.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-deferred-close-error-capture.md](05-deferred-close-error-capture.md) | Next: [07-http-body-close-drain.md](07-http-body-close-drain.md)
