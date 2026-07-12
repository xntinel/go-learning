# Exercise 8: RED-method instrumentation (Rate, Errors, Duration) for an HTTP handler

RED — Rate, Errors, Duration — is the default metric set for a request-driven
service. This module builds middleware that records per-route request count, error
count by status class, and latency, exposed through `expvar` for scraping. The
lesson inside the lesson is cardinality discipline: bucket by the route *template*
and the status *class*, never by the raw path or a user id, or the series count
explodes and takes the metrics backend down.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
redmetrics/                  independent module: example.com/redmetrics
  go.mod                     go 1.25
  redmetrics.go              Metrics (expvar maps); Instrument(route, next) middleware; status-capturing writer
  cmd/
    demo/
      main.go                runnable demo: drive 200/404/500 through one route template
  redmetrics_test.go         per-class counts; two paths share one template counter (bounded cardinality)
```

- Files: `redmetrics.go`, `cmd/demo/main.go`, `redmetrics_test.go`.
- Implement: a `Metrics` holding `*expvar.Map`s for requests, errors, and duration; `Instrument(route string, next http.Handler) http.Handler` that wraps a status-capturing `ResponseWriter`, times the call, and records keyed by `route` and status class.
- Test: drive 200/404/500 through `httptest`; assert per-class request and error counts and that duration was recorded; assert two concrete paths under one route template share a single counter.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/14-error-observability/08-red-metrics-http-middleware/cmd/demo
cd go-solutions/10-error-handling/14-error-observability/08-red-metrics-http-middleware
go mod edit -go=1.25
```

### Why the route template, not the path

The single most important decision here is what the metric is keyed by. A counter
keyed by the concrete request path (`/users/8a3f-...`, `/users/9c2b-...`) mints a
new time series for every distinct id — and a busy service has millions of ids, so
you mint millions of series and the metrics backend runs out of memory. The fix is
to key by the *route template* the handler was registered under
(`/users/{id}`), which is a small, fixed set. The middleware therefore takes the
template as an explicit argument: the caller, which knows the template because it
registered the route, passes it in. Combined with the status *class* (`2xx`,
`4xx`, `5xx`) rather than the exact status code, the label space stays bounded no
matter how much traffic flows.

The three RED signals map onto three `expvar.Map`s:

- Requests, keyed by `"<route> <class>"` — the Rate signal (a scrape samples the
  counter over time to get requests/sec).
- Errors, keyed the same way but only incremented for `4xx`/`5xx` — the Errors
  signal.
- Duration, keyed by route, accumulating total nanoseconds — the Duration signal
  (paired with the request count, a scrape derives average latency; a real system
  would use a histogram, but a sum+count is enough to show the mechanism).

Capturing the status requires a small wrapper: `http.ResponseWriter` does not
expose the status after the fact, so `statusWriter` embeds it, records the code in
`WriteHeader`, and defaults to `200` (because a handler that writes a body without
calling `WriteHeader` implicitly sent `200`). The maps are allocated per-`Metrics`
with `new(expvar.Map).Init()` rather than the global `expvar.NewMap`, so multiple
instances (and parallel tests) do not collide on the global registry — the demo
shows how to publish them globally for a real `/debug/vars` scrape.

Create `redmetrics.go`:

```go
package redmetrics

import (
	"expvar"
	"fmt"
	"net/http"
	"time"
)

// Metrics holds RED counters as expvar maps keyed by bounded dimensions.
type Metrics struct {
	Requests *expvar.Map // key: "<route> <class>"  -> count
	Errors   *expvar.Map // key: "<route> <class>"  -> count (4xx/5xx only)
	Duration *expvar.Map // key: "<route>"          -> total nanoseconds
}

// NewMetrics allocates fresh, unpublished maps (safe to create many, e.g. in tests).
func NewMetrics() *Metrics {
	return &Metrics{
		Requests: new(expvar.Map).Init(),
		Errors:   new(expvar.Map).Init(),
		Duration: new(expvar.Map).Init(),
	}
}

// statusWriter captures the status code the handler wrote (default 200).
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write defaults the status to 200 if the handler writes a body without WriteHeader.
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func statusClass(code int) string {
	if code == 0 {
		code = http.StatusOK
	}
	return fmt.Sprintf("%dxx", code/100)
}

// Instrument wraps next, recording RED metrics keyed by the route template (not
// the raw path) and status class (not the exact code), keeping cardinality bounded.
func (m *Metrics) Instrument(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(sw, r)
		elapsed := time.Since(start)

		class := statusClass(sw.status)
		key := route + " " + class
		m.Requests.Add(key, 1)
		if sw.status >= 400 {
			m.Errors.Add(key, 1)
		}
		m.Duration.Add(route, int64(elapsed))
	})
}
```

### The runnable demo

The demo registers one route template `/users/{id}` and drives three requests
that return 200, 404, and 500, then prints the request map and the error map. Both
`/users/1` and `/users/2` go through the same instrumented handler, so their `2xx`
counts fold into one key — the cardinality point made visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/redmetrics"
)

func main() {
	m := redmetrics.NewMetrics()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/404":
			http.Error(w, "no", http.StatusNotFound)
		case "/users/500":
			http.Error(w, "boom", http.StatusInternalServerError)
		default:
			fmt.Fprintln(w, "ok")
		}
	})
	instrumented := m.Instrument("/users/{id}", handler)

	for _, path := range []string{"/users/1", "/users/2", "/users/404", "/users/500"} {
		rec := httptest.NewRecorder()
		instrumented.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	fmt.Println("requests:", m.Requests.String())
	fmt.Println("errors:", m.Errors.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
requests: {"/users/{id} 2xx": 2, "/users/{id} 4xx": 1, "/users/{id} 5xx": 1}
errors: {"/users/{id} 4xx": 1, "/users/{id} 5xx": 1}
```

### Tests

`TestREDCountsByClass` drives one of each status class and asserts the request and
error maps hold the right per-class counts and that a duration was recorded for the
route. `TestCardinalityBounded` is the headline test: it drives two different
concrete paths through one route template and asserts they collapse to a single
`2xx` counter of value 2 — proving raw paths do not each spawn a series.

Create `redmetrics_test.go`:

```go
package redmetrics

import (
	"expvar"
	"net/http"
	"net/http/httptest"
	"testing"
)

func intVal(t *testing.T, m *expvar.Map, key string) int64 {
	t.Helper()
	v := m.Get(key)
	if v == nil {
		return 0
	}
	iv, ok := v.(*expvar.Int)
	if !ok {
		t.Fatalf("key %q is %T, want *expvar.Int", key, v)
	}
	return iv.Value()
}

func statusHandler(code int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if code == http.StatusOK {
			w.Write([]byte("ok"))
			return
		}
		http.Error(w, http.StatusText(code), code)
	})
}

func TestREDCountsByClass(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	route := "/orders/{id}"

	for code, path := range map[int]string{
		http.StatusOK:                  "/orders/1",
		http.StatusNotFound:            "/orders/9",
		http.StatusInternalServerError: "/orders/7",
	} {
		rec := httptest.NewRecorder()
		m.Instrument(route, statusHandler(code)).ServeHTTP(
			rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	if got := intVal(t, m.Requests, route+" 2xx"); got != 1 {
		t.Fatalf("requests 2xx = %d, want 1", got)
	}
	if got := intVal(t, m.Errors, route+" 4xx"); got != 1 {
		t.Fatalf("errors 4xx = %d, want 1", got)
	}
	if got := intVal(t, m.Errors, route+" 5xx"); got != 1 {
		t.Fatalf("errors 5xx = %d, want 1", got)
	}
	// a 2xx is not an error
	if got := intVal(t, m.Errors, route+" 2xx"); got != 0 {
		t.Fatalf("errors 2xx = %d, want 0", got)
	}
	// duration was recorded for the route
	if m.Duration.Get(route) == nil {
		t.Fatalf("no duration recorded for %q", route)
	}
}

func TestCardinalityBounded(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	route := "/users/{id}"
	h := m.Instrument(route, statusHandler(http.StatusOK))

	for _, path := range []string{"/users/1", "/users/2", "/users/3"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	}

	// three distinct paths collapse into one template counter.
	if got := intVal(t, m.Requests, route+" 2xx"); got != 3 {
		t.Fatalf("template counter = %d, want 3 (paths must share one series)", got)
	}
	// and no per-path key leaked in.
	if got := intVal(t, m.Requests, "/users/1 2xx"); got != 0 {
		t.Fatalf("a raw-path series leaked in: %d", got)
	}
}
```

## Review

The middleware is correct when the label space is bounded and the counts are right.
`TestREDCountsByClass` pins the three signals — requests per class, errors only for
`4xx`/`5xx`, a recorded duration — and `TestCardinalityBounded` is the one that
matters most operationally: three distinct paths produce exactly one series, and no
raw-path key exists. If that test ever counts three series, someone keyed by the
path and lit the fuse on a cardinality explosion.

The status-capturing writer is the easy thing to get subtly wrong: default the
status to `200` (a handler may write a body without `WriteHeader`), and record it
in both `WriteHeader` and the `Write` fallback. Note the maps are per-`Metrics`
and unpublished, so tests do not collide on the global `expvar` registry; a real
service publishes them once with `expvar.Publish` for the `/debug/vars` scrape.
Duration here is a running sum for simplicity; production uses a histogram so you
can compute p99, not just the mean.

## Resources

- [The RED Method](https://grafana.com/blog/2018/08/02/the-red-method-how-to-instrument-your-services/) — Rate, Errors, Duration, and why they are the default service signals.
- [`expvar`](https://pkg.go.dev/expvar) — `Map`, `Int`, `Map.Add`, and publishing to `/debug/vars`.
- [Google SRE: Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/) — the four golden signals and label discipline.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-error-log-sampling.md](09-error-log-sampling.md)
