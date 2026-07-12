# Exercise 9: A Readiness Endpoint That Aggregates Injected Dependency Checkers

A readiness endpoint answers one question: can this instance serve traffic right
now? To answer it, the service asks each critical dependency — database, cache,
downstream API — whether it is healthy, and returns 200 only if all say yes. When
each dependency's health check is an injected `Checker` interface, the whole
endpoint becomes fakeable: a test simulates a dead database without a dead
database.

This module is fully self-contained, with its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
health/                     independent module: example.com/health
  go.mod                    module example.com/health
  health.go                 Checker interface; HealthChecker; concurrent per-check timeout; JSON Handler
  cmd/
    demo/
      main.go               wires three checkers (one failing) and prints the 503 report
  health_test.go            all-pass 200, any-fail 503, slow-check bounded by its timeout
```

- Files: `health.go`, `cmd/demo/main.go`, `health_test.go`.
- Implement: a `Checker` interface (`Check(ctx) error`); a `HealthChecker` holding a map of named checkers and a per-check timeout; it runs the checks concurrently, each under its own `context.WithTimeout`, aggregates failures with `errors.Join`, and exposes an `http.Handler` returning 200 or 503 with a JSON body of per-dependency status.
- Test: fakes returning nil / an error / a slow (context-timeout) result; assert overall 200 when all pass, 503 when any fails, correct per-dependency JSON, and that one slow dependency does not hang the check beyond its timeout.
- Verify: `go test -count=1 -race ./...`

### Injection makes each dependency independently fakeable

A `Checker` is a one-method interface: `Check(ctx) error`, returning nil for
healthy and an error for unhealthy. In production the database checker runs
`SELECT 1`, the cache checker pings Redis, the API checker calls a `/health`
endpoint. In tests each is a five-line fake returning exactly the outcome the test
wants. Because the `HealthChecker` holds a `map[string]Checker` injected at
construction, a test can build one with a healthy db, a broken cache, and a slow
downstream and assert the aggregate — no real infrastructure involved.

Three design decisions make this a real readiness endpoint rather than a toy.
First, the checks run *concurrently*: a serial loop would make the endpoint's
latency the sum of every dependency's latency, and a single slow dependency would
delay the whole answer. Running them in goroutines bounds the total time to the
slowest single check. Second, each check runs under its *own* `context.WithTimeout`,
so a hung dependency cannot block the endpoint indefinitely — after the per-check
timeout its context is cancelled, a well-behaved checker returns
`context.DeadlineExceeded`, and it is reported as unhealthy rather than hanging the
request. Third, failures are aggregated with `errors.Join`: the overall status is
unhealthy if the joined error is non-nil, and the per-dependency detail goes into
the JSON body so an operator can see *which* dependency failed, not just that
something did.

The JSON body uses a `map[string]string` for the per-check statuses, which
`encoding/json` marshals with keys in sorted order — so the output is deterministic
and the test can assert on the exact bytes. The endpoint returns
`http.StatusServiceUnavailable` (503) on failure, the conventional code for "not
ready", so a load balancer or Kubernetes readiness probe removes the instance from
rotation until it recovers.

Create `health.go`:

```go
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Checker reports whether one dependency is healthy. A nil error means healthy.
type Checker interface {
	Check(ctx context.Context) error
}

// HealthChecker aggregates named dependency checks. Each check runs
// concurrently under its own timeout.
type HealthChecker struct {
	checkers map[string]Checker
	timeout  time.Duration
}

// New builds a HealthChecker with a per-check timeout and the injected checkers.
func New(timeout time.Duration, checkers map[string]Checker) *HealthChecker {
	return &HealthChecker{checkers: checkers, timeout: timeout}
}

// Report is the JSON body returned by the handler.
type Report struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// run executes every checker concurrently, each under its own timeout, and
// returns the per-check statuses plus a joined error that is non-nil if any
// check failed.
func (h *HealthChecker) run(ctx context.Context) (map[string]string, error) {
	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(h.checkers))

	var wg sync.WaitGroup
	for name, c := range h.checkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, h.timeout)
			defer cancel()
			results <- result{name: name, err: c.Check(cctx)}
		}()
	}
	wg.Wait()
	close(results)

	statuses := make(map[string]string, len(h.checkers))
	var errs []error
	for r := range results {
		if r.err != nil {
			statuses[r.name] = r.err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", r.name, r.err))
			continue
		}
		statuses[r.name] = "ok"
	}
	return statuses, errors.Join(errs...)
}

// Handler returns an http.Handler that reports readiness: 200 with all checks
// "ok", or 503 with the failing detail.
func (h *HealthChecker) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statuses, err := h.run(r.Context())

		report := Report{Status: "ok", Checks: statuses}
		code := http.StatusOK
		if err != nil {
			report.Status = "unavailable"
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(report)
	})
}
```

### The runnable demo

The demo wires three checkers — a healthy db, a healthy cache, and a failing
downstream API — into the handler and serves one request through an in-memory
recorder, printing the 503 report.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/health"
)

// checkerFunc adapts a function to the Checker interface.
type checkerFunc func(context.Context) error

func (f checkerFunc) Check(ctx context.Context) error { return f(ctx) }

func main() {
	hc := health.New(50*time.Millisecond, map[string]health.Checker{
		"db":    checkerFunc(func(context.Context) error { return nil }),
		"cache": checkerFunc(func(context.Context) error { return nil }),
		"api":   checkerFunc(func(context.Context) error { return errors.New("api unreachable") }),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	hc.Handler().ServeHTTP(rec, req)

	fmt.Println("status:", rec.Code)
	fmt.Print(rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 503
{"status":"unavailable","checks":{"api":"api unreachable","cache":"ok","db":"ok"}}
```

### Tests

The tests drive each dependency's health independently through fakes. The all-pass
test asserts 200 and every check "ok". The any-fail test asserts 503 and that the
failing dependency's message appears in the body. The slow-check test injects a
checker that blocks until its context is cancelled and asserts the endpoint returns
503 (the slow check timed out) without the request taking anywhere near as long as
the checker would block on its own — bounded by the per-check timeout.

Create `health_test.go`:

```go
package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeChecker struct{ err error }

func (f fakeChecker) Check(context.Context) error { return f.err }

// blockingChecker blocks until its context is cancelled, then returns the
// context error — simulating a hung dependency that the timeout must bound.
type blockingChecker struct{}

func (blockingChecker) Check(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) Report {
	t.Helper()
	var r Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return r
}

func TestAllHealthy(t *testing.T) {
	t.Parallel()

	hc := New(50*time.Millisecond, map[string]Checker{
		"db":    fakeChecker{},
		"cache": fakeChecker{},
	})
	rec := httptest.NewRecorder()
	hc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	report := decode(t, rec)
	if report.Status != "ok" {
		t.Fatalf("report status = %q, want ok", report.Status)
	}
	for name, s := range report.Checks {
		if s != "ok" {
			t.Fatalf("check %q = %q, want ok", name, s)
		}
	}
}

func TestOneFailingReturns503(t *testing.T) {
	t.Parallel()

	hc := New(50*time.Millisecond, map[string]Checker{
		"db":    fakeChecker{},
		"cache": fakeChecker{err: errors.New("connection refused")},
	})
	rec := httptest.NewRecorder()
	hc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	report := decode(t, rec)
	if report.Status != "unavailable" {
		t.Fatalf("report status = %q, want unavailable", report.Status)
	}
	if report.Checks["cache"] != "connection refused" {
		t.Fatalf("cache check = %q, want %q", report.Checks["cache"], "connection refused")
	}
	if report.Checks["db"] != "ok" {
		t.Fatalf("db check = %q, want ok", report.Checks["db"])
	}
}

func TestSlowCheckBoundedByTimeout(t *testing.T) {
	t.Parallel()

	hc := New(20*time.Millisecond, map[string]Checker{
		"db":         fakeChecker{},
		"downstream": blockingChecker{},
	})
	rec := httptest.NewRecorder()

	start := time.Now()
	hc.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	// The blocking checker would hang forever; the per-check timeout bounds it.
	if elapsed > time.Second {
		t.Fatalf("request took %v; the timeout did not bound the slow check", elapsed)
	}
	report := decode(t, rec)
	if report.Checks["downstream"] == "ok" {
		t.Fatal("slow downstream should be reported unhealthy")
	}
}
```

## Review

The endpoint is correct when readiness is the conjunction of every injected check:
200 only if `errors.Join` returns nil, 503 otherwise, with the per-dependency detail
in a sorted-key JSON body. Concurrency plus a per-check `context.WithTimeout` is
what makes it production-grade — `TestSlowCheckBoundedByTimeout` proves a hung
dependency is reported unhealthy instead of hanging the probe. The mistake to avoid
is running the checks serially (latency becomes the sum, not the max) or omitting
the per-check timeout (one dead dependency blocks the endpoint forever). Because the
checkers are injected, none of these properties needs a real database to test. Run
`go test -race` to confirm the concurrent checks and the shared result channel are
race-free.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining multiple failures into one error.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for driving the handler without a network.
- [Kubernetes: readiness probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/) — why a readiness endpoint returns 503 to leave rotation.

---

Back to [08-synctest-time-dependent-timeout.md](08-synctest-time-dependent-timeout.md) | Next: [10-avoid-global-state-injected-httpclient.md](10-avoid-global-state-injected-httpclient.md)
