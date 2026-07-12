# Exercise 9: Readiness Aggregator: Short-Circuit vs Collect-All Health Checks

A `/readyz` endpoint behind a load balancer decides whether an instance receives
traffic. It runs a set of named dependency checks and returns 200 or 503. This module
contrasts the two legitimate control-flow shapes for aggregation: fail-fast, which
returns on the first failure of a critical check, and collect-all, which runs every
check and reports the full picture. Choosing between them is a real design decision,
and both are built from `if`s.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
readiness/                  independent module: example.com/readiness
  go.mod                    go 1.26
  readiness.go              Check, Handler(mode, checks...), JSON report, errors.Join
  cmd/
    demo/
      main.go               run a healthy set and a failing set, print status+body
  readiness_test.go         200 all-ok; fail-fast short-circuits; collect-all lists all
```

- Files: `readiness.go`, `cmd/demo/main.go`, `readiness_test.go`.
- Implement: a readiness `http.Handler` supporting a fail-fast mode (return early 503 on the first failure) and a collect-all mode (accumulate failures into a report); write 200 when all pass, else 503 with a per-check JSON summary; aggregate failures with `errors.Join`.
- Test: all healthy returns 200 and reports every check ok; fail-fast stops after the first failure (a run counter proves later checks did not run); collect-all returns 503 listing both failures (membership via `errors.Is`); a check exceeding the context deadline is reported failed, not hung.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/01-if-else-and-init-statements/09-readiness-health-aggregator/cmd/demo
cd go-solutions/03-control-flow/01-if-else-and-init-statements/09-readiness-health-aggregator
```

## Two shapes, chosen by what the caller needs

A check is `type Check struct { Name string; Run func(context.Context) error }`. The
handler runs a slice of them, and the mode selects the control-flow shape:

- **Fail-fast** is right when one critical dependency being down means the instance is
  useless, and running the rest wastes time (or piles latency onto a probe that has a
  tight deadline). The loop is `for _, c := range checks { if err := c.Run(ctx); err != nil { return early with 503 } }`.
  The moment a check fails, later checks do not run — the test proves this with a run
  counter that stays below the total.
- **Collect-all** is right when an operator wants the full picture: which of the
  dependencies are down, all at once. Each `if` appends the failure to a report
  instead of returning: `if err := c.Run(ctx); err != nil { failures = append(...) }`.
  Overall status is "unavailable" if any failed, and the response lists every failing
  check.

Both write a JSON body with per-check status via `json.NewEncoder(w).Encode`, and both
set the HTTP status with `WriteHeader` before writing the body (order matters:
`WriteHeader` after the first `Write` is ignored). In collect-all mode the individual
errors are bundled with `errors.Join`, so a caller can test membership with
`errors.Is(joined, ErrDBDown)` — the joined error preserves each member for matching.

Each check runs under the request context, which the handler derives with a timeout
(`context.WithTimeout`). A check that respects the context returns
`context.DeadlineExceeded` when the deadline passes, and the aggregator reports it as
a failed check rather than hanging the probe. This is why a real readiness check must
plumb the context into every dependency call.

Create `readiness.go`:

```go
package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Mode selects the aggregation shape.
type Mode int

const (
	FailFast Mode = iota
	CollectAll
)

// Check is a named readiness probe.
type Check struct {
	Name string
	Run  func(context.Context) error
}

type checkResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type report struct {
	Status  string        `json:"status"`
	Results []checkResult `json:"results"`
}

// Handler returns a /readyz handler that runs checks in the given mode with a
// per-request timeout.
func Handler(mode Mode, timeout time.Duration, checks ...Check) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		var results []checkResult
		var failed []error

		for _, c := range checks {
			if err := c.Run(ctx); err != nil {
				results = append(results, checkResult{Name: c.Name, OK: false, Error: err.Error()})
				failed = append(failed, err)
				if mode == FailFast {
					break // stop at the first failure
				}
				continue
			}
			results = append(results, checkResult{Name: c.Name, OK: true})
		}

		status := http.StatusOK
		statusText := "ready"
		if len(failed) > 0 {
			status = http.StatusServiceUnavailable
			statusText = "unavailable"
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(report{Status: statusText, Results: results})
	})
}

// Aggregate joins check failures so a caller can match members with errors.Is.
// It is exported for callers that run checks directly rather than over HTTP.
func Aggregate(errs ...error) error { return errors.Join(errs...) }
```

### The runnable demo

The demo runs a healthy set (200) and a set with two failing checks in collect-all
mode (503), printing the status and body of each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"example.com/readiness"
)

func ok(name string) readiness.Check {
	return readiness.Check{Name: name, Run: func(context.Context) error { return nil }}
}
func down(name string, err error) readiness.Check {
	return readiness.Check{Name: name, Run: func(context.Context) error { return err }}
}

func probe(h http.Handler) string {
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		return err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

func main() {
	healthy := readiness.Handler(readiness.CollectAll, time.Second,
		ok("db"), ok("cache"))
	fmt.Println("healthy   :", probe(healthy))

	degraded := readiness.Handler(readiness.CollectAll, time.Second,
		down("db", errors.New("dial timeout")), ok("cache"), down("queue", errors.New("no leader")))
	fmt.Println("collect-all:", probe(degraded))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
healthy   : 200 {"status":"ready","results":[{"name":"db","ok":true},{"name":"cache","ok":true}]}
collect-all: 503 {"status":"unavailable","results":[{"name":"db","ok":false,"error":"dial timeout"},{"name":"cache","ok":true},{"name":"queue","ok":false,"error":"no leader"}]}
```

### Tests

The tests use `httptest.NewRecorder` to drive the handler directly. All-healthy
returns 200 and reports every check ok. Fail-fast uses a run counter to prove later
checks did not run after the first failure. Collect-all returns 503 and its aggregated
error contains both failing sentinels (checked with `errors.Is`). A check that blocks
until the context deadline is reported failed, not hung.

Create `readiness_test.go`:

```go
package readiness

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

var (
	errDBDown    = errors.New("db down")
	errQueueDown = errors.New("queue down")
)

func okCheck(name string, counter *atomic.Int64) Check {
	return Check{Name: name, Run: func(context.Context) error {
		if counter != nil {
			counter.Add(1)
		}
		return nil
	}}
}

func failCheck(name string, err error, counter *atomic.Int64) Check {
	return Check{Name: name, Run: func(context.Context) error {
		if counter != nil {
			counter.Add(1)
		}
		return err
	}}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) report {
	t.Helper()
	var rep report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return rep
}

func serve(h http.Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.ServeHTTP(rec, req)
	return rec
}

func TestAllHealthyReturns200(t *testing.T) {
	t.Parallel()
	h := Handler(CollectAll, time.Second, okCheck("db", nil), okCheck("cache", nil))
	rec := serve(h)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	rep := decode(t, rec)
	if rep.Status != "ready" || len(rep.Results) != 2 {
		t.Fatalf("report = %+v, want ready with 2 results", rep)
	}
	for _, r := range rep.Results {
		if !r.OK {
			t.Fatalf("check %s not ok", r.Name)
		}
	}
}

func TestFailFastShortCircuits(t *testing.T) {
	t.Parallel()
	var runs atomic.Int64
	h := Handler(FailFast, time.Second,
		failCheck("db", errDBDown, &runs),
		okCheck("cache", &runs),
		okCheck("queue", &runs),
	)
	rec := serve(h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if runs.Load() != 1 {
		t.Fatalf("ran %d checks, want 1 (fail-fast stops at first failure)", runs.Load())
	}
}

func TestCollectAllListsEveryFailure(t *testing.T) {
	t.Parallel()
	var runs atomic.Int64
	h := Handler(CollectAll, time.Second,
		failCheck("db", errDBDown, &runs),
		okCheck("cache", &runs),
		failCheck("queue", errQueueDown, &runs),
	)
	rec := serve(h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	if runs.Load() != 3 {
		t.Fatalf("ran %d checks, want 3 (collect-all runs all)", runs.Load())
	}
	rep := decode(t, rec)
	failing := 0
	for _, r := range rep.Results {
		if !r.OK {
			failing++
		}
	}
	if failing != 2 {
		t.Fatalf("failing checks = %d, want 2", failing)
	}
}

func TestAggregateMembershipViaErrorsIs(t *testing.T) {
	t.Parallel()
	joined := Aggregate(errDBDown, errQueueDown)
	if !errors.Is(joined, errDBDown) || !errors.Is(joined, errQueueDown) {
		t.Fatalf("joined error lost a member: %v", joined)
	}
}

func TestCheckHonoringDeadlineIsReportedFailed(t *testing.T) {
	t.Parallel()
	slow := Check{Name: "slow", Run: func(ctx context.Context) error {
		<-ctx.Done() // respects cancellation instead of hanging
		return ctx.Err()
	}}
	h := Handler(CollectAll, 10*time.Millisecond, slow)
	rec := serve(h)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	rep := decode(t, rec)
	if len(rep.Results) != 1 || rep.Results[0].OK {
		t.Fatalf("slow check should be reported failed: %+v", rep)
	}
}
```

## Review

The aggregator is correct when all-healthy is 200, any failure is 503, fail-fast stops
at the first failure (proven by the run counter), collect-all runs every check and
lists them all, and a check that respects the context deadline is reported failed
rather than hanging the probe. The mistakes to avoid are calling `WriteHeader` after
the first `Write` (the status is then ignored — 200 leaks out on a 503), choosing
collect-all when a tight-deadline liveness probe should fail fast (or vice versa), and
writing checks that ignore the context so a stuck dependency hangs the whole endpoint.
`errors.Join` is the idiomatic aggregator: it bundles failures while keeping each
matchable by `errors.Is`, which is what lets a caller act on a specific dependency
being down.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join)
- [Kubernetes: Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- [encoding/json.Encoder](https://pkg.go.dev/encoding/json#Encoder)
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-token-bucket-admission.md](08-token-bucket-admission.md) | Next: [10-idempotency-key-gate.md](10-idempotency-key-gate.md)
