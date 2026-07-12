# Exercise 5: A Readiness Gate That Aggregates Per-Dependency Health

Liveness asks "is the process alive?"; readiness asks "should this instance
receive traffic right now?". This module builds the readiness gate: an
aggregator that fans out a bounded health check across every critical dependency
and returns `503` until all of them report healthy, `200` afterwards — the exact
contract a Kubernetes readiness probe uses to hold load-balancer traffic off a
not-yet-warm instance.

## What you'll build

```text
readiness/                    independent module: example.com/readiness
  go.mod                      go 1.26
  svcframe.go                 App + HealthChecker; ReadyHandler with per-check timeout
  svcframe_test.go            503 until all healthy; per-check timeout bounds a hung dep; errors.Join
  cmd/
    demo/
      main.go                 flips a dep healthy after a delay; prints 503 then 200
```

Files: `svcframe.go`, `cmd/demo/main.go`, `svcframe_test.go`.
Implement: `HealthChecker` (`Check(ctx) error`); `App.ReadyHandler(perCheck)` that fans out checks concurrently, each bounded by `perCheck` via `select { case <-ctx.Done(): ...; case err := <-ch: ... }`, returning `503` until every critical dependency passes.
Test: table-driven with healthy / erroring / slow-past-timeout fakes; status flips `503 → 200` only when all critical checks pass; a hung check is bounded by `perCheck`; `errors.Join` surfaces every failing name.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/14-building-a-context-aware-service-framework/05-readiness-and-health-aggregation/cmd/demo
cd go-solutions/14-select-and-context/14-building-a-context-aware-service-framework/05-readiness-and-health-aggregation
```

### The aggregation model

A service *optionally* implements `HealthChecker`. The readiness handler walks
the registered critical services, and for each one that implements
`HealthChecker` it runs `Check` — concurrently, so N slow dependencies cost one
timeout, not N. Each check is bounded by its own `context.WithTimeout(ctx,
perCheck)`, and the result is collected through a `select` on the check's context
`Done` versus a buffered result channel. The buffer is load-bearing: when the
timeout wins the `select`, the goroutine still running `Check` must have
somewhere to send its eventual result so it can exit rather than leak — a
size-1 channel gives it that.

A critical service that does *not* implement `HealthChecker` is treated as ready
(there is nothing to check). The handler returns `200 "ready"` only when every
checked dependency passed; otherwise it returns `503` with the joined errors, so
the probe output names every dependency that is down. `errors.Join` is exactly
the right tool: it aggregates N errors into one whose `Error()` lists them all
and whose `errors.Is` matches any of them.

Create `svcframe.go`:

```go
package svcframe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Service is the single seam every component implements.
type Service interface {
	Name() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// HealthChecker is an optional capability: a service that implements it
// participates in the readiness gate.
type HealthChecker interface {
	Check(ctx context.Context) error
}

// Config holds per-service lifecycle options.
type Config struct {
	Critical    bool
	StopTimeout time.Duration
}

type entry struct {
	svc Service
	cfg Config
}

// App manages registered services and exposes a readiness gate over them.
type App struct {
	mu      sync.Mutex
	entries []entry
	logger  *slog.Logger
}

// New returns an App logging to logger; nil means slog.Default().
func New(logger *slog.Logger) *App {
	if logger == nil {
		logger = slog.Default()
	}
	return &App{logger: logger}
}

// Register appends a service with its config.
func (a *App) Register(svc Service, cfg Config) {
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 5 * time.Second
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, entry{svc: svc, cfg: cfg})
}

// checkAll fans out a bounded health check across every critical service that
// implements HealthChecker and joins their failures. Each check is bounded by
// perCheck independently, so one hung dependency cannot stall the whole probe.
func (a *App) checkAll(ctx context.Context, perCheck time.Duration) error {
	a.mu.Lock()
	targets := make([]entry, 0, len(a.entries))
	for _, e := range a.entries {
		if e.cfg.Critical {
			targets = append(targets, e)
		}
	}
	a.mu.Unlock()

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for _, e := range targets {
		hc, ok := e.svc.(HealthChecker)
		if !ok {
			continue // no health check registered: treated as ready
		}
		name := e.svc.Name()
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, perCheck)
			defer cancel()

			ch := make(chan error, 1) // buffered so a timed-out Check can still send and exit
			go func() { ch <- hc.Check(cctx) }()

			var err error
			select {
			case <-cctx.Done():
				err = fmt.Errorf("%s: %w", name, context.Cause(cctx))
			case e := <-ch:
				if e != nil {
					err = fmt.Errorf("%s: %w", name, e)
				}
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// ReadyHandler returns an http.Handler that reports readiness: 200 once every
// critical dependency passes its health check, 503 (with the joined failures)
// until then. Each check is bounded by perCheck.
func (a *App) ReadyHandler(perCheck time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := a.checkAll(r.Context(), perCheck); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
	})
}
```

### The runnable demo

The demo registers two dependencies: a database that is healthy immediately, and
a cache that reports unhealthy until it "warms up" after 150ms. It queries the
readiness handler before and after the warm-up and prints both status codes,
showing the `503 → 200` transition a load balancer would observe.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/readiness"
)

type dep struct {
	name  string
	ready *atomic.Bool
}

func (d *dep) Name() string                { return d.name }
func (d *dep) Start(context.Context) error { return nil }
func (d *dep) Stop(context.Context) error  { return nil }

func (d *dep) Check(context.Context) error {
	if d.ready.Load() {
		return nil
	}
	return errors.New("warming up")
}

func main() {
	var dbReady, cacheReady atomic.Bool
	dbReady.Store(true) // db is ready from the start

	app := svcframe.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.Register(&dep{name: "db", ready: &dbReady}, svcframe.Config{Critical: true})
	app.Register(&dep{name: "cache", ready: &cacheReady}, svcframe.Config{Critical: true})

	handler := app.ReadyHandler(200 * time.Millisecond)

	probe := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/readyz", nil)
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	fmt.Printf("before warm-up: %d\n", probe())

	go func() {
		time.Sleep(150 * time.Millisecond)
		cacheReady.Store(true)
	}()
	time.Sleep(200 * time.Millisecond)

	fmt.Printf("after warm-up: %d\n", probe())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before warm-up: 503
after warm-up: 200
```

### Tests

The tests use `httptest.NewRecorder` to drive the handler directly. A table
covers the combinations: all healthy (`200`), one erroring (`503` naming it), one
hung past the per-check timeout (`503`, and the request itself returns promptly
because the check — not the request — is bounded). A dedicated test asserts
`errors.Join` surfaces *every* failing dependency by checking the body contains
both names.

Create `svcframe_test.go`:

```go
package svcframe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeDep struct {
	name    string
	checkFn func(ctx context.Context) error
}

func (f *fakeDep) Name() string                { return f.name }
func (f *fakeDep) Start(context.Context) error { return nil }
func (f *fakeDep) Stop(context.Context) error  { return nil }

func (f *fakeDep) Check(ctx context.Context) error { return f.checkFn(ctx) }

func healthy(context.Context) error { return nil }

func erroring(msg string) func(context.Context) error {
	return func(context.Context) error { return errors.New(msg) }
}

func hung(ctx context.Context) error {
	<-ctx.Done() // never returns before the per-check timeout
	return ctx.Err()
}

func TestReadyHandlerStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		deps     []*fakeDep
		wantCode int
	}{
		{
			name:     "all healthy",
			deps:     []*fakeDep{{name: "db", checkFn: healthy}, {name: "cache", checkFn: healthy}},
			wantCode: 200,
		},
		{
			name:     "one erroring",
			deps:     []*fakeDep{{name: "db", checkFn: healthy}, {name: "cache", checkFn: erroring("down")}},
			wantCode: 503,
		},
		{
			name:     "one hung past timeout",
			deps:     []*fakeDep{{name: "db", checkFn: healthy}, {name: "cache", checkFn: hung}},
			wantCode: 503,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := New(testLogger())
			for _, d := range tt.deps {
				app.Register(d, Config{Critical: true})
			}
			handler := app.ReadyHandler(30 * time.Millisecond)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/readyz", nil)

			start := time.Now()
			handler.ServeHTTP(rec, req)
			elapsed := time.Since(start)

			if rec.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d", rec.Code, tt.wantCode)
			}
			// The hung check must be bounded by the 30ms per-check timeout, not
			// hang the whole request.
			if elapsed > time.Second {
				t.Fatalf("request took %v, per-check timeout was not applied", elapsed)
			}
		})
	}
}

func TestReadyHandlerJoinsAllFailures(t *testing.T) {
	t.Parallel()

	app := New(testLogger())
	app.Register(&fakeDep{name: "db", checkFn: erroring("db down")}, Config{Critical: true})
	app.Register(&fakeDep{name: "cache", checkFn: erroring("cache down")}, Config{Critical: true})
	handler := app.ReadyHandler(30 * time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "db") || !strings.Contains(body, "cache") {
		t.Fatalf("body = %q, want both failing dependency names", body)
	}
}

func TestNonCheckerCriticalIsReady(t *testing.T) {
	t.Parallel()

	// A critical service with no Check is treated as ready.
	app := New(testLogger())
	app.Register(NewPlain("db"), Config{Critical: true})
	handler := app.ReadyHandler(30 * time.Millisecond)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/readyz", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d, want 200 for a service without a health check", rec.Code)
	}
}

type plain struct{ name string }

// NewPlain is a Service with no HealthChecker.
func NewPlain(name string) Service { return &plain{name: name} }

func (p *plain) Name() string                { return p.name }
func (p *plain) Start(context.Context) error { return nil }
func (p *plain) Stop(context.Context) error  { return nil }
```

## Review

The gate is correct when the status is `200` if and only if every critical
dependency's `Check` returns `nil` within its budget, and `503` otherwise. The
two subtle properties the tests pin: a hung check is bounded by the *per-check*
timeout, so the `/readyz` request returns in tens of milliseconds even when a
dependency is wedged (the `elapsed` assertion), and `errors.Join` surfaces every
failing name rather than short-circuiting on the first — an operator reading the
probe output needs the full list. The buffered result channel inside `checkAll`
is what prevents a goroutine leak when the timeout wins the `select`; drop the
buffer and a hung `Check` leaks its goroutine on every probe. Do not conflate
this with liveness: returning `200` here means "route traffic", so returning it
before the dependencies are actually up is the outage this whole gate exists to
prevent. Run `go test -race` to confirm the concurrent check accounting is
race-free.

## Resources

- [Kubernetes: Configure Liveness, Readiness and Startup Probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) — the operational contract this gate implements.
- [errors.Join](https://pkg.go.dev/errors#Join) — aggregating multiple dependency failures into one error.
- [context.WithTimeout and context.Cause](https://pkg.go.dev/context#WithTimeout) — bounding each check independently.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — driving the handler without a real server.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-signal-driven-graceful-http-shutdown.md](04-signal-driven-graceful-http-shutdown.md) | Next: [06-supervised-restart-with-backoff.md](06-supervised-restart-with-backoff.md)
