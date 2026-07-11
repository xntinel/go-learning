# 10. Prometheus Metrics Exposition

Prometheus scrapes your Go process at `/metrics` on a regular interval. The hard
part is not registering one counter — it is building an instrumentation layer that
is correct under concurrency, uses the right metric type for the semantics, avoids
high-cardinality label explosions, and is testable without running a real Prometheus
server. This lesson builds a complete HTTP middleware stack with a private registry
so tests are hermetic and do not leak state between runs.

```text
metrics/
  go.mod
  metrics.go          -- Registry, metric constructors, Middleware, Server
  metrics_test.go     -- table-driven tests via testutil.ToFloat64 and CollectAndCount
  cmd/demo/main.go    -- runnable demo: go run ./cmd/demo
```

## Concepts

### The four metric types and when to use each

**Counter** (`prometheus.CounterVec`) is strictly monotonically increasing. It
counts events — requests served, errors emitted, bytes sent. It never decreases.
`Reset()` does not exist on a counter; if a process restarts the counter restarts
from zero, which Prometheus handles via `rate()` and `increase()`.

**Gauge** (`prometheus.GaugeVec`) represents a current value that can go up or
down. It measures state — goroutines running, queue depth, active connections,
temperature. A gauge's last value is meaningful on its own; a counter's raw value
is not (only the rate matters).

**Histogram** (`prometheus.HistogramVec`) distributes observations across
pre-defined buckets. Each `Observe(v)` call increments every bucket whose upper
bound is >= v, increments `_count` by 1, and adds v to `_sum`. Prometheus can
then compute `histogram_quantile()` server-side. Histograms are cumulative: the
bucket at 500 ms includes all observations <= 500 ms, not just those in the
500 ms band. Choose buckets that bracket your SLOs (e.g., 10 ms p50, 100 ms p95,
500 ms p99) — wrong buckets make `histogram_quantile()` inaccurate.

**Summary** is similar to histogram but computes quantiles client-side over a
sliding time window. Summaries are harder to aggregate across replicas because
quantiles do not add; histograms aggregate correctly. In new code, prefer
histograms unless you need high-precision quantiles from a single process.

### Private registries and test isolation

The global `prometheus.DefaultRegisterer` accumulates metrics for the lifetime of
the process. Tests that register metrics into the global registry interfere with
each other: the second `TestFoo` panics with "metric already registered".

The fix is a private `*prometheus.Registry`:

```go
reg := prometheus.NewRegistry()
reg.MustRegister(myCounter, myHistogram)
handler := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
```

Each test creates its own `Metrics` value (with its own registry), so tests are
hermetic and parallel-safe.

### Label cardinality

Labels split a metric into independent time series. `status_code=200` and
`status_code=404` are separate series. High-cardinality labels — user IDs,
request IDs, email addresses — create one series per unique value. At millions of
unique values Prometheus runs out of memory and scrapes become slow. The rule:
labels must come from a small, bounded set (HTTP methods, 3xx/4xx/5xx status
classes, endpoint names). Never use free-form strings or IDs as label values.

### Histogram buckets and SLOs

`prometheus.DefBuckets` (0.005 s through 10 s) is a reasonable default but
seldom matches a real SLO. For an API with a p99 target of 200 ms, use buckets
that have good resolution around 200 ms:

```go
Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5, 1.0, 2.5}
```

Prometheus interpolates linearly inside buckets, so a bucket boundary far from
the SLO threshold produces an inaccurate quantile estimate.

### The responseWriter wrapper

`http.ResponseWriter` does not expose the status code after `WriteHeader` is
called. A thin wrapper records it so middleware can label metrics with the actual
HTTP status:

```go
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
```

Without this wrapper every request is labeled `200` because that is what
`net/http` uses when `WriteHeader` is never called explicitly.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/metrics/cmd/demo
cd ~/go-exercises/metrics
go mod init example.com/metrics
go get github.com/prometheus/client_golang@v1.20.0
```

This is a library with a demo program; the verification is `go test`, not `go run`.

### Exercise 1: Private registry and metric constructors

Create `metrics.go`:

```go
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds all Prometheus collectors for one service instance.
// Use New to construct; each instance owns a private registry so tests
// do not interfere with each other.
type Metrics struct {
	reg *prometheus.Registry

	// HTTP instrumentation
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestsInFlight *prometheus.GaugeVec

	// Business metrics
	ordersCreated *prometheus.CounterVec
	usersCreated  *prometheus.CounterVec
}

// New creates a Metrics instance with its own private registry.
// Prometheus's Go and process collectors are NOT added so that tests
// remain hermetic (the default collectors add noise).
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		reg: reg,

		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "http",
				Name:      "requests_total",
				Help:      "Total number of HTTP requests partitioned by method, path, and status.",
			},
			[]string{"method", "path", "status"},
		),

		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: "http",
				Name:      "request_duration_seconds",
				Help:      "HTTP request latency in seconds.",
				// Buckets chosen around a p99 SLO of 500 ms.
				Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
			},
			[]string{"method", "path"},
		),

		requestsInFlight: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "http",
				Name:      "requests_in_flight",
				Help:      "Number of HTTP requests currently being processed.",
			},
			[]string{"path"},
		),

		ordersCreated: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "app",
				Name:      "orders_created_total",
				Help:      "Total orders created, partitioned by status.",
			},
			[]string{"status"},
		),

		usersCreated: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: "app",
				Name:      "users_created_total",
				Help:      "Total users registered.",
			},
			[]string{},
		),
	}

	reg.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.requestsInFlight,
		m.ordersCreated,
		m.usersCreated,
	)

	return m
}

// Handler returns an HTTP handler that serves the Prometheus exposition format
// for this instance's private registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

// Registry returns the underlying registry (used in tests with testutil).
func (m *Metrics) Registry() *prometheus.Registry {
	return m.reg
}

// RecordOrderCreated increments the business counter for a created order.
func (m *Metrics) RecordOrderCreated(status string) {
	m.ordersCreated.WithLabelValues(status).Inc()
}

// RecordUserCreated increments the user registration counter.
func (m *Metrics) RecordUserCreated() {
	m.usersCreated.WithLabelValues().Inc()
}
```

### Exercise 2: Middleware

Add the middleware and the response writer wrapper to `metrics.go`. Append to the
same file (same package):

```go
// responseWriter wraps http.ResponseWriter to capture the status code written
// by the handler. Without this wrapper all requests appear as status 200
// because http.ResponseWriter does not expose the code after WriteHeader.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Middleware returns an http.Handler that instruments the given handler with
// the three HTTP metrics: requests_total, request_duration_seconds, and
// requests_in_flight. path is the canonical route pattern (e.g. "/api/orders").
func (m *Metrics) Middleware(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight := m.requestsInFlight.WithLabelValues(path)
		inFlight.Inc()
		defer inFlight.Dec()

		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)

		elapsed := time.Since(start).Seconds()
		status := strconv.Itoa(rw.statusCode)
		m.requestsTotal.WithLabelValues(r.Method, path, status).Inc()
		m.requestDuration.WithLabelValues(r.Method, path).Observe(elapsed)
	})
}
```

### Exercise 3: Tests

Create `metrics_test.go`. Tests use `testutil.ToFloat64` for scalar assertions
and `testutil.CollectAndCompare` for full text-format checks. Each test creates
its own `*Metrics` so registration never collides:

```go
package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// nopHandler returns a handler that always responds with the given status code.
func nopHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	})
}

func TestRequestsTotal_IncrementOnSuccess(t *testing.T) {
	t.Parallel()

	m := New()
	h := m.Middleware("/api/orders", nopHandler(http.StatusOK))

	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	got := testutil.ToFloat64(m.requestsTotal.WithLabelValues("GET", "/api/orders", "200"))
	if got != 1.0 {
		t.Errorf("http_requests_total{GET,/api/orders,200} = %v, want 1", got)
	}
}

func TestRequestsTotal_LabelVariants(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		method    string
		path      string
		code      int
		statusStr string
		want      float64
	}{
		{"POST 201", http.MethodPost, "/api/users", http.StatusCreated, "201", 1},
		{"GET 404", http.MethodGet, "/api/items", http.StatusNotFound, "404", 1},
		{"GET 500", http.MethodGet, "/api/broken", http.StatusInternalServerError, "500", 1},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			m := New()
			h := m.Middleware(tc.path, nopHandler(tc.code))
			req := httptest.NewRequest(tc.method, tc.path, nil)
			h.ServeHTTP(httptest.NewRecorder(), req)

			got := testutil.ToFloat64(
				m.requestsTotal.WithLabelValues(tc.method, tc.path, tc.statusStr),
			)
			if got != tc.want {
				t.Errorf("requests_total{%s,%s,%s} = %v, want %v",
					tc.method, tc.path, tc.statusStr, got, tc.want)
			}
		})
	}
}

func TestRequestsInFlight_ZeroAfterCompletion(t *testing.T) {
	t.Parallel()

	m := New()
	h := m.Middleware("/api/orders", nopHandler(http.StatusOK))

	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	got := testutil.ToFloat64(m.requestsInFlight.WithLabelValues("/api/orders"))
	if got != 0 {
		t.Errorf("requests_in_flight after completion = %v, want 0", got)
	}
}

func TestRequestDuration_ObservationRecorded(t *testing.T) {
	t.Parallel()

	m := New()
	h := m.Middleware("/api/orders", nopHandler(http.StatusOK))

	req := httptest.NewRequest(http.MethodGet, "/api/orders", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	// The histogram's _count child must be 1 after one request.
	// CollectAndCompare checks the full text representation.
	// We use CollectAndCount to assert exactly 1 observation was recorded.
	count := testutil.CollectAndCount(m.requestDuration, "http_request_duration_seconds")
	if count == 0 {
		t.Error("http_request_duration_seconds: no observations recorded")
	}
}

func TestRecordOrderCreated(t *testing.T) {
	t.Parallel()

	m := New()
	m.RecordOrderCreated("success")
	m.RecordOrderCreated("success")
	m.RecordOrderCreated("failed")

	gotSuccess := testutil.ToFloat64(m.ordersCreated.WithLabelValues("success"))
	gotFailed := testutil.ToFloat64(m.ordersCreated.WithLabelValues("failed"))

	if gotSuccess != 2 {
		t.Errorf("orders_created_total{success} = %v, want 2", gotSuccess)
	}
	if gotFailed != 1 {
		t.Errorf("orders_created_total{failed} = %v, want 1", gotFailed)
	}
}

func TestRecordUserCreated(t *testing.T) {
	t.Parallel()

	m := New()
	m.RecordUserCreated()
	m.RecordUserCreated()
	m.RecordUserCreated()

	got := testutil.ToFloat64(m.usersCreated.WithLabelValues())
	if got != 3 {
		t.Errorf("users_created_total = %v, want 3", got)
	}
}

func TestMetricsHandler_RespondsOK(t *testing.T) {
	t.Parallel()

	m := New()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain prefix", ct)
	}
}

func ExampleMetrics_RecordOrderCreated() {
	m := New()
	m.RecordOrderCreated("success")
	m.RecordOrderCreated("success")

	v := testutil.ToFloat64(m.ordersCreated.WithLabelValues("success"))
	fmt.Printf("orders_created_total{success} = %.0f\n", v)
	// Output:
	// orders_created_total{success} = 2
}
```

Your turn: add `TestMultipleRequests_CounterAccumulates` that fires three GET
requests through the middleware and asserts `requests_total` equals 3. Use
`t.Parallel()` and a fresh `*Metrics` per test.

### Exercise 4: Demo program

Create `cmd/demo/main.go`. It exercises only the exported API:

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"example.com/metrics"
)

func main() {
	m := metrics.New()

	ordersHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate work
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"orders":[]}`)
		m.RecordOrderCreated("success")
	})

	usersHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"users":[]}`)
		m.RecordUserCreated()
	})

	mux := http.NewServeMux()
	mux.Handle("/api/orders", m.Middleware("/api/orders", ordersHandler))
	mux.Handle("/api/users", m.Middleware("/api/users", usersHandler))
	mux.Handle("/metrics", m.Handler())

	addr := "localhost:8080"
	log.Printf("listening on http://%s", addr)
	log.Printf("metrics at  http://%s/metrics", addr)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
```

Run the demo and exercise the endpoints in another terminal:

```bash
go run ./cmd/demo &
curl http://localhost:8080/api/orders
curl http://localhost:8080/api/orders
curl http://localhost:8080/api/users
curl http://localhost:8080/metrics | grep -E 'http_requests_total|app_orders'
```

## Common Mistakes

**Wrong: registering metrics into the global registry in tests.**

```go
// Wrong — panics on second test run: "already registered"
var counter = prometheus.NewCounter(prometheus.CounterOpts{Name: "x", Help: "x"})
func init() { prometheus.MustRegister(counter) }
```

What happens: the `init` function runs once per test binary, but if multiple test
functions each import a package that calls `MustRegister` for the same name, the
second registration panics. Tests in parallel amplify the problem.

Fix: construct metrics inside a `New()` function that creates a private
`*prometheus.Registry` and registers into it. Each test calls `New()` and gets
its own isolated instance.

**Wrong: using request URL as a label value.**

```go
// Wrong — unbounded cardinality
m.requestsTotal.WithLabelValues(r.Method, r.URL.Path, status).Inc()
```

What happens: `/api/orders/1`, `/api/orders/2`, ... each create a distinct time
series. At scale, Prometheus memory usage explodes. Cardinality of 10 000 series
for one metric is a common incident.

Fix: use the canonical route pattern, not the actual URL:

```go
// Fix: path is "/api/orders", not r.URL.Path
m.requestsTotal.WithLabelValues(r.Method, path, status).Inc()
```

Pass `path` as a parameter to the middleware constructor so the value is fixed
per route registration.

**Wrong: forgetting the responseWriter wrapper.**

```go
// Wrong — status is always "200" because net/http defaults to 200
// when WriteHeader is never called explicitly by the handler.
func (m *Metrics) Middleware(path string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		m.requestsTotal.WithLabelValues(r.Method, path, "200").Inc() // always 200
	})
}
```

Fix: wrap `w` in a `responseWriter` that overrides `WriteHeader` to record the
status code. Use `http.StatusOK` as the initial value so handlers that never call
`WriteHeader` are correctly recorded as 200.

**Wrong: choosing histogram buckets from the defaults without review.**

```go
// Wrong for an API with a 100 ms SLO
Buckets: prometheus.DefBuckets // coarsest boundary near 100 ms is 0.1 s = 100 ms
                                // but next is 0.25 s — poor resolution for p99
```

Fix: define buckets that bracket your SLO boundary with at least two or three
buckets in the region of interest. For a 100 ms p99 target:

```go
Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 1.0}
```

## Verification

From `~/go-exercises/metrics`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the primary verification — there is no program
to eyeball.

To manually inspect the Prometheus exposition output:

```bash
go run ./cmd/demo &
sleep 1
curl -s http://localhost:8080/api/orders
curl -s http://localhost:8080/api/orders
curl -s http://localhost:8080/metrics | grep -E '^(http_requests_total|app_orders)'
kill %1
```

Expected lines in the output (values will vary):

```text
http_requests_total{method="GET",path="/api/orders",status="200"} 2
app_orders_created_total{status="success"} 2
```

## Summary

- Use a private `*prometheus.Registry` per service instance; tests create one
  per test case for hermetic isolation.
- Counter for events that only go up (requests, errors); Gauge for current state
  (in-flight, queue depth); Histogram for distributions (latency, payload size).
- Pass the canonical route pattern as a middleware parameter, never `r.URL.Path`,
  to avoid unbounded label cardinality.
- Wrap `http.ResponseWriter` to capture the status code; without the wrapper all
  requests are labeled `200`.
- Size histogram buckets around your SLO thresholds so `histogram_quantile()` is
  accurate.
- `testutil.ToFloat64` and `testutil.CollectAndCount` enable deterministic
  assertions without a running Prometheus server.

## What's Next

Next: [OpenTelemetry Collector Integration](../11-opentelemetry-collector-integration/11-opentelemetry-collector-integration.md).

## Resources

- [pkg.go.dev: prometheus/client_golang/prometheus](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus) — complete API reference for Counter, Gauge, Histogram, Registry.
- [pkg.go.dev: prometheus/client_golang/prometheus/promhttp](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/promhttp) — Handler, HandlerFor, instrumentation middleware.
- [pkg.go.dev: prometheus/client_golang/prometheus/testutil](https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/testutil) — ToFloat64, CollectAndCompare, CollectAndCount.
- [Prometheus: metric types](https://prometheus.io/docs/concepts/metric_types/) — authoritative definitions of Counter, Gauge, Histogram, Summary and when to use each.
- [Prometheus: metric and label naming](https://prometheus.io/docs/practices/naming/) — naming conventions, cardinality guidelines, unit suffixes.
