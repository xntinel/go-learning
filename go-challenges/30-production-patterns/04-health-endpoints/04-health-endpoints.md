# 4. Health Endpoints

Health endpoints are the signaling layer between your service and the infrastructure that manages it. Kubernetes, load balancers, and service meshes rely on two distinct probes: a liveness probe that tells the orchestrator whether the process is still alive, and a readiness probe that tells it whether the process can serve traffic right now. The hard part is not writing the HTTP handler — it is the architecture around concurrency, caching, timeouts, and startup sequencing that keeps the probes themselves from becoming a source of failure.

```text
health/
  go.mod
  health.go
  health_test.go
  cmd/demo/main.go
```

The package exposes a `Service` that registers `Checker` implementations, runs them concurrently with per-check timeouts, caches the aggregate result, and exposes `LivenessHandler` and `ReadinessHandler` as `http.HandlerFunc` values ready to mount on any mux.

## Concepts

### Liveness vs. Readiness

A liveness probe answers: "Is the process alive and not deadlocked?" It should be as cheap as possible — a single HTTP 200 from a handler that does nothing more than write a response. If liveness fails, the orchestrator kills and restarts the container. False positives are catastrophic: a restart under load is worse than degraded service.

A readiness probe answers: "Can this instance handle a new request right now?" It tests every downstream dependency the service needs to function: databases, caches, message brokers, external APIs. If readiness fails, the orchestrator stops routing traffic to this instance but leaves it running. The instance can recover on its own once dependencies come back.

Mixing the two into a single `/health` endpoint is the most common mistake in production Go services. A slow database query that causes `/health` to time out triggers a container restart, which makes the database load worse. Keep them separate.

### The Checker Interface

```go
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}
```

Each dependency implements this interface. `Check` returns nil when the dependency is available and a non-nil error otherwise. The interface is narrow enough to satisfy with a function adapter (`CheckerFunc`) — exactly the pattern `http.HandlerFunc` uses for `http.Handler`.

### Concurrency and Timeouts

Running checks sequentially means a single 5-second timeout blocks the entire probe. Run all checks concurrently with `sync.WaitGroup` and give each one an independent `context.WithTimeout` derived from the request context. If the request context is cancelled (the orchestrator gave up), every per-check context cancels automatically — no goroutine leak.

```go
ctx, cancel := context.WithTimeout(parent, s.checkTimeout)
defer cancel()
err := c.Check(ctx)
```

The per-check timeout must be smaller than the orchestrator's probe timeout (typically 1 s inside a 3 s probe period). A reasonable default is 500 ms per check.

### Result Caching

A liveness probe might run every 5 seconds with `failureThreshold: 3`. With ten dependency checks, that is 10 * 3 = 30 HTTP calls to your database in 15 seconds — just for health probing. Cache the aggregate result and serve it from memory if it is fresher than `cacheTTL`. Invalidation is simple: a time-based TTL of 2–5 seconds keeps probes responsive to failures without hammering dependencies.

The cache requires a `sync.RWMutex`: many concurrent probe requests read the cache simultaneously; only the goroutine that detects a stale cache acquires the write lock.

### Startup Grace Period

A freshly started container reaches its readiness check before it has finished loading configuration, warming caches, or running database migrations. Without a grace period, readiness fails immediately, the orchestrator never sends traffic, and the container is restarted in a loop.

Model the startup state with an unexported boolean protected by the same `sync.RWMutex`. Call `MarkReady()` after initialization completes. Until then, `ReadinessHandler` returns 503 with status "starting".

### HTTP Status Codes

Return 200 for healthy. Return 503 for unhealthy or not yet ready. Kubernetes interprets any status >= 400 as a probe failure. Avoid 500: it reads as a bug, not a health signal.

## Exercises

This is a library, not a program: there is no top-level `main`. You verify it with `go test`.

### Exercise 1: Types, Interface, and Service Constructor

Create `health.go`:

```go
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Status is the aggregate health state.
type Status string

const (
	StatusUp   Status = "up"
	StatusDown Status = "down"
)

// Checker is implemented by any dependency that can report its own health.
type Checker interface {
	Name() string
	Check(ctx context.Context) error
}

// CheckerFunc adapts a plain function to the Checker interface.
type CheckerFunc struct {
	name string
	fn   func(ctx context.Context) error
}

// NewCheckerFunc wraps fn as a Checker named name.
func NewCheckerFunc(name string, fn func(ctx context.Context) error) *CheckerFunc {
	return &CheckerFunc{name: name, fn: fn}
}

func (c *CheckerFunc) Name() string                    { return c.name }
func (c *CheckerFunc) Check(ctx context.Context) error { return c.fn(ctx) }

// ComponentResult holds the outcome of one Checker.
type ComponentResult struct {
	Name      string        `json:"name"`
	Status    Status        `json:"status"`
	LatencyMS int64         `json:"latency_ms"`
	Error     string        `json:"error,omitempty"`
	latency   time.Duration // unexported; LatencyMS is the JSON field
}

// AggregateResult is the JSON body returned by ReadinessHandler.
type AggregateResult struct {
	Status     Status            `json:"status"`
	Components []ComponentResult `json:"components,omitempty"`
	Timestamp  time.Time         `json:"timestamp"`
}

// Service runs health checks and exposes HTTP handlers.
type Service struct {
	checkers     []Checker
	checkTimeout time.Duration
	cacheTTL     time.Duration

	mu          sync.RWMutex
	lastCheck   time.Time
	lastResult  *AggregateResult
	startupDone bool
}

// New returns a Service with the given per-check timeout and cache TTL.
// Reasonable values: checkTimeout 500ms, cacheTTL 5s.
func New(checkTimeout, cacheTTL time.Duration) *Service {
	return &Service{
		checkTimeout: checkTimeout,
		cacheTTL:     cacheTTL,
	}
}

// Register adds c to the set of checks run by ReadinessHandler.
func (s *Service) Register(c Checker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkers = append(s.checkers, c)
}

// MarkReady signals that startup is complete; ReadinessHandler stops returning 503.
func (s *Service) MarkReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startupDone = true
}

// IsReady reports whether MarkReady has been called.
func (s *Service) IsReady() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.startupDone
}

// checkAll runs every registered Checker concurrently and returns the aggregate.
// It returns a cached result if one exists and is younger than cacheTTL.
func (s *Service) checkAll(parent context.Context) *AggregateResult {
	s.mu.RLock()
	if s.lastResult != nil && time.Since(s.lastCheck) < s.cacheTTL {
		r := s.lastResult
		s.mu.RUnlock()
		return r
	}
	s.mu.RUnlock()

	s.mu.RLock()
	checkers := s.checkers
	s.mu.RUnlock()

	results := make([]ComponentResult, len(checkers))
	var wg sync.WaitGroup

	for i, c := range checkers {
		wg.Add(1)
		go func(idx int, ch Checker) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, s.checkTimeout)
			defer cancel()

			start := time.Now()
			err := ch.Check(ctx)
			elapsed := time.Since(start)

			cr := ComponentResult{
				Name:      ch.Name(),
				Status:    StatusUp,
				LatencyMS: elapsed.Milliseconds(),
				latency:   elapsed,
			}
			if err != nil {
				cr.Status = StatusDown
				cr.Error = err.Error()
			}
			results[idx] = cr
		}(i, c)
	}
	wg.Wait()

	overall := StatusUp
	for _, r := range results {
		if r.Status == StatusDown {
			overall = StatusDown
			break
		}
	}

	agg := &AggregateResult{
		Status:     overall,
		Components: results,
		Timestamp:  time.Now().UTC(),
	}

	s.mu.Lock()
	s.lastResult = agg
	s.lastCheck = time.Now()
	s.mu.Unlock()

	return agg
}

// LivenessHandler returns 200 if the process is alive. It does not run dependency checks.
func (s *Service) LivenessHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"alive"}`+"\n")
}

// ReadinessHandler returns 503 during startup or when any dependency check fails,
// and 200 once all checks pass.
func (s *Service) ReadinessHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ready := s.startupDone
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"status":"starting"}`+"\n")
		return
	}

	agg := s.checkAll(r.Context())

	code := http.StatusOK
	if agg.Status == StatusDown {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(agg) //nolint:errcheck
}
```

The liveness handler never runs checks — it is intentionally trivial. The readiness handler delegates to `checkAll`, which manages caching and concurrent execution.

### Exercise 2: Test the Service Contract

Create `health_test.go`:

```go
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

var errUnhealthy = errors.New("dependency unavailable")

func alwaysOK(name string) *CheckerFunc {
	return NewCheckerFunc(name, func(_ context.Context) error { return nil })
}

func alwaysFail(name string) *CheckerFunc {
	return NewCheckerFunc(name, func(_ context.Context) error {
		return fmt.Errorf("%w: %s is down", errUnhealthy, name)
	})
}

func TestLivenessHandler(t *testing.T) {
	t.Parallel()

	svc := New(500*time.Millisecond, 5*time.Second)
	rec := httptest.NewRecorder()
	svc.LivenessHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("liveness status = %d, want 200", rec.Code)
	}
}

func TestReadinessHandler_StartupGrace(t *testing.T) {
	t.Parallel()

	svc := New(500*time.Millisecond, 5*time.Second)
	// MarkReady not called — service is still starting.
	rec := httptest.NewRecorder()
	svc.ReadinessHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 during startup", rec.Code)
	}
}

func TestReadinessHandler_AllHealthy(t *testing.T) {
	t.Parallel()

	svc := New(500*time.Millisecond, 5*time.Second)
	svc.Register(alwaysOK("db"))
	svc.Register(alwaysOK("cache"))
	svc.MarkReady()

	rec := httptest.NewRecorder()
	svc.ReadinessHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when all checks pass", rec.Code)
	}

	var agg AggregateResult
	if err := json.NewDecoder(rec.Body).Decode(&agg); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if agg.Status != StatusUp {
		t.Fatalf("aggregate status = %q, want %q", agg.Status, StatusUp)
	}
	if len(agg.Components) != 2 {
		t.Fatalf("components = %d, want 2", len(agg.Components))
	}
}

func TestReadinessHandler_OneFails(t *testing.T) {
	t.Parallel()

	svc := New(500*time.Millisecond, 5*time.Second)
	svc.Register(alwaysOK("cache"))
	svc.Register(alwaysFail("db"))
	svc.MarkReady()

	rec := httptest.NewRecorder()
	svc.ReadinessHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when any check fails", rec.Code)
	}

	var agg AggregateResult
	if err := json.NewDecoder(rec.Body).Decode(&agg); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if agg.Status != StatusDown {
		t.Fatalf("aggregate status = %q, want %q", agg.Status, StatusDown)
	}
}

func TestCheckerFunc_WrapsError(t *testing.T) {
	t.Parallel()

	svc := New(500*time.Millisecond, 5*time.Second)
	svc.Register(alwaysFail("downstream"))
	svc.MarkReady()

	rec := httptest.NewRecorder()
	svc.ReadinessHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	var agg AggregateResult
	if err := json.NewDecoder(rec.Body).Decode(&agg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The component error message must be present.
	if len(agg.Components) == 0 || agg.Components[0].Error == "" {
		t.Fatalf("expected error message in component, got: %+v", agg.Components)
	}
}

func TestCheckAll_CachesResult(t *testing.T) {
	t.Parallel()

	calls := 0
	svc := New(500*time.Millisecond, 10*time.Second)
	svc.Register(NewCheckerFunc("counter", func(_ context.Context) error {
		calls++
		return nil
	}))
	svc.MarkReady()

	ctx := context.Background()
	svc.checkAll(ctx)
	svc.checkAll(ctx) // second call must hit cache

	if calls != 1 {
		t.Fatalf("checker called %d times, want 1 (cache should have prevented second call)", calls)
	}
}

func TestCheckAll_CheckTimeout(t *testing.T) {
	t.Parallel()

	slow := NewCheckerFunc("slow", func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			return nil
		}
	})

	svc := New(50*time.Millisecond, 0) // zero TTL so cache never applies
	svc.Register(slow)
	svc.MarkReady()

	start := time.Now()
	rec := httptest.NewRecorder()
	svc.ReadinessHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("slow check was not cancelled: elapsed %s", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for timed-out check", rec.Code)
	}
}

func TestMarkReady_Idempotent(t *testing.T) {
	t.Parallel()

	svc := New(500*time.Millisecond, 5*time.Second)
	svc.MarkReady()
	svc.MarkReady() // must not panic or race
	if !svc.IsReady() {
		t.Fatal("IsReady should return true after MarkReady")
	}
}

func ExampleService_LivenessHandler() {
	svc := New(500*time.Millisecond, 5*time.Second)
	rec := httptest.NewRecorder()
	svc.LivenessHandler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	fmt.Println(rec.Code)
	fmt.Println(rec.Header().Get("Content-Type"))
	// Output:
	// 200
	// application/json
}

func ExampleService_ReadinessHandler_starting() {
	svc := New(500*time.Millisecond, 5*time.Second)
	// MarkReady not called — service is still starting.
	rec := httptest.NewRecorder()
	svc.ReadinessHandler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	fmt.Println(rec.Code)
	// Output:
	// 503
}
```

Your turn: add `TestReadinessHandler_NoCheckers` — create a `Service` with no registered checkers, call `MarkReady()`, send a GET to `ReadinessHandler`, and assert the response is 200 with `aggregate status == "up"`. A service with no dependencies is always ready.

### Exercise 3: The Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"example.com/health"
)

// tcpChecker probes whether a TCP address is reachable.
type tcpChecker struct {
	name string
	addr string
}

func (t *tcpChecker) Name() string { return t.name }
func (t *tcpChecker) Check(ctx context.Context) error {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", t.addr, err)
	}
	conn.Close()
	return nil
}

var errDiskFull = errors.New("disk usage above threshold")

func main() {
	svc := health.New(500*time.Millisecond, 5*time.Second)

	// Disk-space check via CheckerFunc.
	svc.Register(health.NewCheckerFunc("disk", func(_ context.Context) error {
		// Simulate: always healthy in demo.
		return nil
	}))

	// TCP reachability check (will fail if redis is not running — that is intentional).
	svc.Register(&tcpChecker{name: "redis", addr: "localhost:6379"})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", svc.LivenessHandler)
	mux.HandleFunc("GET /readyz", svc.ReadinessHandler)

	// Simulate async startup: mark ready after 1 second.
	go func() {
		log.Println("starting up...")
		time.Sleep(time.Second)
		svc.MarkReady()
		log.Println("ready")
	}()

	addr := "127.0.0.1:8080"
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}

	_ = errDiskFull // referenced to satisfy go vet
}
```

Run it with:

```bash
go run ./cmd/demo &
sleep 0.2
curl -s -w " HTTP %{http_code}\n" http://127.0.0.1:8080/healthz
curl -s -w " HTTP %{http_code}\n" http://127.0.0.1:8080/readyz  # 503 — still starting
sleep 1.2
curl -s -w " HTTP %{http_code}\n" http://127.0.0.1:8080/readyz  # 200 or 503 depending on redis
kill %1
```

## Common Mistakes

### Using One Endpoint for Both Probes

Wrong: a single `/health` that runs dependency checks. A slow or failing database causes liveness to fail, the orchestrator kills the container, and restarts make the database load worse.

Fix: separate `/healthz` (no dependency checks, always cheap) from `/readyz` (full dependency checks). Only readiness failure should stop traffic; only liveness failure should trigger a restart.

### No Per-Check Timeout

Wrong: calling `c.Check(ctx)` where `ctx` is the raw request context with no additional deadline. One slow dependency blocks the entire probe for the full HTTP server timeout (often 30 s).

Fix: derive a tighter context for each check:
```go
ctx, cancel := context.WithTimeout(parent, s.checkTimeout)
defer cancel()
```

### Registering Checks After `MarkReady`

Wrong: calling `Register` from multiple goroutines without coordination after the service is already serving traffic. The `checkers` slice is not safe for concurrent append.

Fix: all `Register` calls must happen before `MarkReady`, or the `Register` method must hold the write lock (as in this lesson).

### Returning 500 from a Health Handler

Wrong: `w.WriteHeader(http.StatusInternalServerError)` for a failing check. Some load balancers treat 5xx differently from 4xx, and 500 implies a bug in the handler, not a downstream outage.

Fix: use 503 Service Unavailable for all "unhealthy or not ready" states. The body JSON carries the details.

### Forgetting to Flush the Cache Between Tests

Wrong: tests that share a single `Service` instance with a non-zero `cacheTTL` will read each other's cached results. A check that should run sees the previous test's result.

Fix: create a new `Service` per test subcase, or set `cacheTTL` to zero in unit tests so every call to `checkAll` is fresh.

## Verification

From `~/go-exercises/health`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `-race` flag is mandatory: `checkAll` uses goroutines and a `sync.RWMutex`; the race detector catches any missed locking.

## Summary

- Liveness (`/healthz`) and readiness (`/readyz`) are distinct signals: one for process aliveness, one for traffic eligibility.
- The `Checker` interface decouples the health framework from specific dependencies; `CheckerFunc` adapts a closure without a named type.
- Run all checks concurrently with `sync.WaitGroup`; give each its own `context.WithTimeout` derived from the request context so cancellation propagates.
- Cache the aggregate result behind a `sync.RWMutex` to prevent probe traffic from hammering dependencies.
- Use a startup boolean protected by the same mutex; `ReadinessHandler` returns 503 until `MarkReady` is called.
- Always return 200 for healthy and 503 for unhealthy; never 500 from a health handler.

## What's Next

Next: [Request ID Propagation](../05-request-id-propagation/05-request-id-propagation.md).

## Resources

- [net/http package](https://pkg.go.dev/net/http) — `ResponseWriter`, `ServeMux`, handler signatures
- [context package](https://pkg.go.dev/context) — `WithTimeout`, `WithCancel`, cancellation propagation
- [sync package](https://pkg.go.dev/sync) — `RWMutex`, `WaitGroup`
- [Go Concurrency Patterns: Context](https://go.dev/blog/context) — authoritative explanation of context propagation and timeout chains
- [Kubernetes liveness and readiness probes](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/) — probe semantics, thresholds, and failure behavior
