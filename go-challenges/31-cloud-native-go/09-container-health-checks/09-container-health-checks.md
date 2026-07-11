# 9. Container Health Checks

Container orchestrators — Kubernetes, ECS, Docker Compose — route traffic to a container, restart it, or hold it in a warm-up phase based on HTTP health endpoints. Getting those endpoints wrong costs you: a pod that receives traffic before its database connection pool is ready returns 500s, and a pod stuck in a crash-loop that never marks itself unstarted keeps the old revision from rolling back. This lesson builds the full three-probe surface area (`/healthz`, `/readyz`, `/startupz`) as a standalone `package health`, tests every state transition, and wires graceful shutdown to the readiness gate.

```text
health/
  go.mod
  health.go
  health_test.go
  cmd/demo/main.go
```

## Concepts

### The Three Probe Kinds

Kubernetes defines three probe kinds, each with a distinct purpose:

- Startup probe (`/startupz`): the kubelet calls this probe only during the initial start-up window. It returns 503 until the application signals it has finished initializing (loaded config, applied DB migrations, etc.). Once it flips to 200 the kubelet switches to the liveness and readiness probes. Without this probe a slow-starting application would be killed by liveness before it is ready.
- Liveness probe (`/healthz`): the kubelet calls this periodically during normal operation. It answers the question "is the process still alive and not permanently stuck?" A liveness probe should be cheap: if the HTTP server responds at all, the process is alive. Checking external dependencies here is wrong — a flaky database should make the pod temporarily non-ready, not cause a restart.
- Readiness probe (`/readyz`): the kubelet calls this to decide whether to route new requests to the pod. It is the right place for dependency checks (database, cache, downstream services). A 503 response removes the pod from the Service's Endpoints slice; the pod keeps running.

### State Machine

A `HealthChecker` holds two boolean fields (`started`, `ready`) guarded by a `sync.RWMutex`:

```
started = false -> true   (one-way transition, after initialization)
ready   = false -> true   (two-way: set false on shutdown)
```

Liveness always returns 200 (the server is alive by definition of reaching the handler). Startup returns 200 only when `started == true`. Readiness returns 200 only when `ready == true` AND all registered dependency checks pass.

### Dependency Checks

Each dependency (database, cache, downstream HTTP) registers a `func(context.Context) error`. The readiness handler runs them all under the incoming request's context, which already carries the kubelet's deadline. If any check returns a non-nil error, the handler returns 503 with a JSON body naming which checks failed:

```json
{"status":"error","checks":{"db":"ok","cache":"connection refused"}}
```

Using `r.Context()` for the check calls is important: the kubelet configures a `timeoutSeconds` on each probe. The context carries that deadline, so checks that use `context.Context` (sql.DB, redis, net/http) respect it automatically.

### Graceful Shutdown

On SIGTERM the goal is: stop accepting new traffic first, drain in-flight requests second, then exit. The sequence is:

1. `signal.NotifyContext` cancels a context when SIGTERM or SIGINT arrives.
2. A goroutine waits on that context, calls `SetReady(false)` (kubelet stops routing within the next probe interval), then calls `(*http.Server).Shutdown` with a separate timeout context.
3. `Shutdown` waits for active connections to finish and then returns.

`signal.NotifyContext` is available since Go 1.16. `(*http.Server).Shutdown` is available since Go 1.8. Both are in the standard library; no third-party packages are needed.

### Thread Safety

Health state is read by HTTP handler goroutines and written by application goroutines (the initializer, the shutdown goroutine). A `sync.RWMutex` provides safe concurrent access: handlers take `RLock`, state mutations take `Lock`. Registering dependency checks is done at startup before the HTTP server starts, so `AddCheck` does not need to hold the mutex.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/health/cmd/demo
cd ~/go-exercises/health
go mod init example.com/health
```

This is a library, not a program: the package is `package health`, verified with `go test`.

### Exercise 1: The HealthChecker Type

Create `health.go`:

```go
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
)

// CheckFunc is a dependency check that returns nil when the dependency is
// healthy and a non-nil error describing the failure otherwise.
type CheckFunc func(ctx context.Context) error

// HealthChecker holds the health state for a service and exposes HTTP handlers
// for the three Kubernetes probe kinds.
type HealthChecker struct {
	mu      sync.RWMutex
	started bool
	ready   bool
	checks  map[string]CheckFunc
}

// New returns a HealthChecker with started and ready set to false.
func New() *HealthChecker {
	return &HealthChecker{
		checks: make(map[string]CheckFunc),
	}
}

// AddCheck registers a named dependency check. Call this before starting the
// HTTP server; it is not safe to call concurrently with handler calls.
func (h *HealthChecker) AddCheck(name string, fn CheckFunc) {
	h.checks[name] = fn
}

// SetStarted marks the application as past its initialization phase. After this
// call, StartupHandler returns 200.
func (h *HealthChecker) SetStarted(v bool) {
	h.mu.Lock()
	h.started = v
	h.mu.Unlock()
}

// SetReady marks the application as ready (v=true) or not ready (v=false) to
// handle traffic. Call SetReady(false) at the start of graceful shutdown.
func (h *HealthChecker) SetReady(v bool) {
	h.mu.Lock()
	h.ready = v
	h.mu.Unlock()
}

// LivenessHandler handles GET /healthz. It always returns 200 with
// {"status":"ok"}: if the HTTP server is responding, the process is alive.
func (h *HealthChecker) LivenessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
}

// StartupHandler handles GET /startupz. It returns 200 after SetStarted(true)
// and 503 before that.
func (h *HealthChecker) StartupHandler(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	started := h.started
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if !started {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not_started"}) //nolint:errcheck
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"}) //nolint:errcheck
}

// ReadinessHandler handles GET /readyz. It returns 503 when the service is not
// ready or when any registered dependency check fails, and 200 otherwise.
func (h *HealthChecker) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	ready := h.ready
	h.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "not_ready"}) //nolint:errcheck
		return
	}

	results := make(map[string]string, len(h.checks))
	allOK := true
	for name, fn := range h.checks {
		if err := fn(r.Context()); err != nil {
			results[name] = err.Error()
			allOK = false
		} else {
			results[name] = "ok"
		}
	}

	if !allOK {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
			"status": "error",
			"checks": results,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{ //nolint:errcheck
		"status": "ok",
		"checks": results,
	})
}

// Handler returns an http.ServeMux pre-wired with the three probe endpoints.
func (h *HealthChecker) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.LivenessHandler)
	mux.HandleFunc("/readyz", h.ReadinessHandler)
	mux.HandleFunc("/startupz", h.StartupHandler)
	return mux
}
```

`SetStarted` and `SetReady` are the only state mutations; handlers only read state via `RLock`. Dependency checks are registered once at start-up before the server runs, so `AddCheck` does not need a lock.

### Exercise 2: Test Every Probe State

Create `health_test.go`:

```go
package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// helpers

func newReq(t *testing.T, method, path string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return r
}

func code(h *HealthChecker, method, path string, t *testing.T) int {
	t.Helper()
	var handler http.HandlerFunc
	switch path {
	case "/healthz":
		handler = h.LivenessHandler
	case "/startupz":
		handler = h.StartupHandler
	case "/readyz":
		handler = h.ReadinessHandler
	default:
		t.Fatalf("unknown path %q", path)
	}
	rr := httptest.NewRecorder()
	handler(rr, newReq(t, method, path))
	return rr.Code
}

// liveness

func TestLivenessAlways200(t *testing.T) {
	t.Parallel()

	h := New()
	// liveness does not depend on started or ready state
	for _, ready := range []bool{false, true} {
		h.SetReady(ready)
		if got := code(h, http.MethodGet, "/healthz", t); got != http.StatusOK {
			t.Errorf("ready=%v: want 200, got %d", ready, got)
		}
	}
}

// startup

func TestStartupBefore(t *testing.T) {
	t.Parallel()

	h := New()
	if got := code(h, http.MethodGet, "/startupz", t); got != http.StatusServiceUnavailable {
		t.Errorf("want 503 before SetStarted, got %d", got)
	}
}

func TestStartupAfter(t *testing.T) {
	t.Parallel()

	h := New()
	h.SetStarted(true)
	if got := code(h, http.MethodGet, "/startupz", t); got != http.StatusOK {
		t.Errorf("want 200 after SetStarted(true), got %d", got)
	}
}

// readiness — state flag

func TestReadinessNotReadyByDefault(t *testing.T) {
	t.Parallel()

	h := New()
	if got := code(h, http.MethodGet, "/readyz", t); got != http.StatusServiceUnavailable {
		t.Errorf("want 503 before SetReady, got %d", got)
	}
}

func TestReadinessAfterSetReady(t *testing.T) {
	t.Parallel()

	h := New()
	h.SetReady(true)
	if got := code(h, http.MethodGet, "/readyz", t); got != http.StatusOK {
		t.Errorf("want 200 after SetReady(true), got %d", got)
	}
}

func TestReadinessResetToFalse(t *testing.T) {
	t.Parallel()

	h := New()
	h.SetReady(true)
	h.SetReady(false)
	if got := code(h, http.MethodGet, "/readyz", t); got != http.StatusServiceUnavailable {
		t.Errorf("want 503 after SetReady(false), got %d", got)
	}
}

// readiness — dependency checks

var errDB = errors.New("db: connection refused")

func TestReadinessDependencyFail(t *testing.T) {
	t.Parallel()

	h := New()
	h.SetReady(true)
	h.AddCheck("db", func(_ context.Context) error { return errDB })

	rr := httptest.NewRecorder()
	h.ReadinessHandler(rr, newReq(t, http.MethodGet, "/readyz"))

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 when db check fails, got %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "db") {
		t.Errorf("response body should mention failed check name; got %q", body)
	}
}

func TestReadinessDependencyOK(t *testing.T) {
	t.Parallel()

	h := New()
	h.SetReady(true)
	h.AddCheck("db", func(_ context.Context) error { return nil })
	h.AddCheck("cache", func(_ context.Context) error { return nil })

	if got := code(h, http.MethodGet, "/readyz", t); got != http.StatusOK {
		t.Errorf("want 200 when all checks pass, got %d", got)
	}
}

func TestReadinessMixedChecks(t *testing.T) {
	t.Parallel()

	h := New()
	h.SetReady(true)
	h.AddCheck("db", func(_ context.Context) error { return nil })
	h.AddCheck("cache", func(_ context.Context) error { return errDB })

	if got := code(h, http.MethodGet, "/readyz", t); got != http.StatusServiceUnavailable {
		t.Errorf("want 503 when one check fails, got %d", got)
	}
}

// handler mux

func TestHandlerRoutes(t *testing.T) {
	t.Parallel()

	h := New()
	h.SetStarted(true)
	h.SetReady(true)

	mux := h.Handler()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, tc := range []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusOK},
		{"/startupz", http.StatusOK},
		{"/readyz", http.StatusOK},
	} {
		resp, err := http.Get(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("GET %s: want %d, got %d", tc.path, tc.want, resp.StatusCode)
		}
	}
}

func ExampleNew() {
	h := New()
	rr := httptest.NewRecorder()
	r, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
	h.LivenessHandler(rr, r)
	fmt.Println(rr.Code)
	// Output:
	// 200
}
```

Your turn: add `TestReadinessCheckContext` — register a check that inspects whether its context is non-nil, call `ReadinessHandler`, and assert the check received a non-nil context. This pins the contract that the handler passes `r.Context()` to checks.

### Exercise 3: cmd/demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"example.com/health"
)

func main() {
	h := health.New()

	// Register synthetic dependency checks.
	h.AddCheck("db", func(_ context.Context) error {
		return nil // simulate healthy DB
	})
	h.AddCheck("cache", func(_ context.Context) error {
		return nil // simulate healthy cache
	})

	// Mark initialized after a brief simulated startup delay.
	go func() {
		time.Sleep(500 * time.Millisecond)
		h.SetStarted(true)
		h.SetReady(true)
		fmt.Println("startup complete: /startupz and /readyz now return 200")
	}()

	srv := &http.Server{
		Addr:    ":8080",
		Handler: h.Handler(),
	}

	// Graceful shutdown: on SIGTERM or SIGINT, mark not-ready, then drain.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		<-ctx.Done()
		fmt.Println("signal received: marking not-ready")
		h.SetReady(false)
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
	}()

	fmt.Println("listening on :8080")
	fmt.Println("  GET /healthz   -> liveness")
	fmt.Println("  GET /readyz    -> readiness")
	fmt.Println("  GET /startupz  -> startup")

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("ListenAndServe: %v", err)
	}
	fmt.Println("server stopped")
}
```

Run with `go run ./cmd/demo` and exercise the endpoints with `curl`:

```bash
curl -s http://localhost:8080/healthz    # always 200
curl -s http://localhost:8080/startupz   # 503 for ~0.5 s, then 200
curl -s http://localhost:8080/readyz     # 503 until startup, then 200
```

Send SIGINT (Ctrl-C) or `kill -TERM <pid>` to observe the graceful shutdown sequence.

## Common Mistakes

### Liveness Checks External Dependencies

Wrong: The `/healthz` handler pings the database. When the database is slow, the kubelet kills the pod with a liveness failure — restarting a perfectly healthy Go process does not fix a slow database, it makes the restart loop permanent.

Fix: Liveness answers only "is the Go process alive?" If the HTTP server can respond, liveness returns 200. External checks belong in readiness.

### Not Guarding Health State With a Mutex

Wrong: Two goroutines read and write `started bool` without synchronization. The Go race detector flags this as a data race at runtime even though the race window is small.

Fix: All reads and writes go through `sync.RWMutex`. Handlers use `RLock`/`RUnlock`; state mutations use `Lock`/`Unlock`. This is what `SetStarted`, `SetReady`, `StartupHandler`, and `ReadinessHandler` do in the lesson.

### Not Marking Readiness False Before Shutdown

Wrong: Shutdown is initiated immediately on SIGTERM. In-flight requests from Kubernetes still arrive for several seconds while the endpoint controller removes the pod from the Service's Endpoints slice.

Fix: Call `SetReady(false)` first. The next readiness probe (typically 1-5 seconds later) removes the pod from the load-balancer rotation. Only then call `Shutdown` to drain remaining in-flight requests.

### Passing a Background Context to Dependency Checks

Wrong: `fn(context.Background())` inside `ReadinessHandler`. The probe timeout configured in Kubernetes (e.g. `timeoutSeconds: 2`) is carried in the request context via `r.Context()`. Ignoring it causes the handler to hang past the probe deadline.

Fix: Pass `r.Context()` to every dependency check so they inherit the request deadline.

### Using `log.Fatal` After `ListenAndServe`

Wrong: `log.Fatal(srv.ListenAndServe())` — this calls `os.Exit(1)` when the server shuts down cleanly with `http.ErrServerClosed`, preventing any post-shutdown cleanup.

Fix: Check the error and distinguish `http.ErrServerClosed` (normal) from real failures:
```go
if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
	log.Fatalf("ListenAndServe: %v", err)
}
```

## Verification

From `~/go-exercises/health`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the verification — there is no program to eyeball.

## Summary

- Liveness (`/healthz`) checks only that the process is alive; it never checks external dependencies.
- Startup (`/startupz`) returns 503 until initialization is complete, preventing premature liveness failures during slow start-up.
- Readiness (`/readyz`) checks dependency health and returns 503 with a JSON body naming failed checks.
- Dependency checks receive `r.Context()` so they respect the probe's timeout deadline.
- A `sync.RWMutex` guards all reads and writes to health state.
- On SIGTERM: call `SetReady(false)`, then `(*http.Server).Shutdown`, in that order.
- `signal.NotifyContext` (Go 1.16+) is the idiomatic way to convert OS signals into context cancellation.

## What's Next

Next: [Prometheus Metrics Exposition](../10-prometheus-metrics-exposition/10-prometheus-metrics-exposition.md).

## Resources

- [net/http — pkg.go.dev](https://pkg.go.dev/net/http): `Server.Shutdown`, `ServeMux`, `ResponseWriter`
- [os/signal — pkg.go.dev](https://pkg.go.dev/os/signal): `NotifyContext` (Go 1.16+)
- [sync — pkg.go.dev](https://pkg.go.dev/sync): `RWMutex`
- [Kubernetes liveness, readiness, and startup probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)
- [net/http: Server.Shutdown — pkg.go.dev](https://pkg.go.dev/net/http#Server.Shutdown): graceful shutdown reference
