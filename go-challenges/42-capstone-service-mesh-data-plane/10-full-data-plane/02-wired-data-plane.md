# Exercise 2: The Wired Data Plane

This is the integration capstone: one `DataPlane` that composes a validated config and backend pool, a pluggable set of policy components (health, rate limit, circuit breaker, metrics), the L7 middleware pipeline that runs them in the right order, an admin API on a separate port, and a startup/shutdown lifecycle with a drain timeout — all in one process.

This module is fully self-contained. It bundles its own configuration and routing core, the component interfaces with no-op defaults, the middleware pipeline, and the admin server in package `dataplane`, plus its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
dataplane.go      Config, Validate, Backend, component interfaces, no-op
                  defaults, DataPlane, New + functional options, SelectBackend,
                  UpdateConfig, Start/Shutdown lifecycle, proxyFor
pipeline.go       Middleware, Chain, peer-identity context, the five policy
                  middlewares, responseWriter, buildPipeline
admin.go          adminMux: GET /health /ready /clusters /config
cmd/
  demo/
    main.go       start the plane, proxy a real request, read admin, drain
dataplane_test.go each middleware in isolation, admin handlers, end-to-end
                  through a real upstream, mTLS→rate-limiter wiring, examples
```

- Files: `dataplane.go`, `pipeline.go`, `admin.go`, `cmd/demo/main.go`, `dataplane_test.go`.
- Implement: `New` with functional options, the middleware `Chain` and `buildPipeline`, the admin handlers, and the `Start`/`Shutdown` lifecycle.
- Test: each middleware against an `httptest.ResponseRecorder`, the admin handlers, an end-to-end request through a real `httptest.Server` upstream, and the mTLS-to-rate-limiter identity contract.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### How the pieces wire together

The `DataPlane` is an orchestrator, not a base class. It holds each policy behind an interface — `HealthChecker`, `RateLimiter`, `CircuitBreaker`, `MetricsRecorder` — and `New` installs a no-op implementation for any the caller did not supply, so the hot path never needs a nil check. Real implementations arrive through functional options (`WithRateLimiter`, `WithMetrics`, and so on); later options override earlier ones. This is the seam that lets the same `DataPlane` run with a stub in a test and a Prometheus recorder in production without changing a line of the pipeline.

`buildPipeline` composes the policies into one `http.Handler` with `Chain`, which applies its middlewares so the first listed is outermost. The order is deliberate and is the single most important design decision in the file: `mTLS → rate limit → timeout → circuit breaker → metrics → upstream`. mTLS runs first so the authenticated peer identity is in the request context before any policy that needs it. The rate limiter runs next and reads that identity from the context — it never touches the TLS state directly, which is exactly the decoupling that lets it be tested with a plain `httptest.NewRequest` whose context you set by hand. Metrics runs innermost, immediately around the upstream call, so the latency it observes is the upstream round-trip and not the cost of the outer policies. The `responseWriter` wrapper is what lets the circuit breaker and metrics see the status the upstream produced: it records the first `WriteHeader` and forwards it, so a 5xx from the upstream becomes a recorded failure while a 4xx policy rejection upstream of it does not.

`Start` brings the listeners up in dependency order — health checker first (it updates the pool through the backend flags), then the data-plane server, then the admin server — and blocks until the context is cancelled or a listener fails fatally. On cancellation it runs the reverse: it cancels the health checker and calls `Shutdown` on both servers with a `drainTimeout` deadline, so in-flight requests finish (or the deadline forces them) before the process exits. The admin server lives on its own port so management traffic never contends with request traffic and a firewall can restrict it to the control plane.

Create `dataplane.go`:

```go
package dataplane

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors for configuration validation.
var (
	ErrInvalidConfig    = errors.New("invalid config")
	ErrDuplicateCluster = errors.New("duplicate cluster name")
	ErrUnknownCluster   = errors.New("route references unknown cluster")
	ErrNoEndpoints      = errors.New("cluster has no endpoints")
)

// Cluster is an upstream cluster definition.
type Cluster struct {
	Name      string
	Endpoints []string
}

// Config is the data-plane configuration. Build it and call Validate before
// passing it to New or UpdateConfig.
type Config struct {
	ListenAddr string
	AdminAddr  string
	TLSConfig  *tls.Config
	Clusters   []Cluster
}

// Validate returns all configuration errors, not just the first. It uses
// errors.Join so callers can test individual errors with errors.Is.
func Validate(cfg Config) error {
	var errs []error
	if cfg.ListenAddr == "" {
		errs = append(errs, fmt.Errorf("%w: ListenAddr required", ErrInvalidConfig))
	}
	if cfg.AdminAddr == "" {
		errs = append(errs, fmt.Errorf("%w: AdminAddr required", ErrInvalidConfig))
	}
	seen := map[string]bool{}
	for _, c := range cfg.Clusters {
		if seen[c.Name] {
			errs = append(errs, fmt.Errorf("%w: %q", ErrDuplicateCluster, c.Name))
		}
		seen[c.Name] = true
		if len(c.Endpoints) == 0 {
			errs = append(errs, fmt.Errorf("%w: cluster %q", ErrNoEndpoints, c.Name))
		}
	}
	return errors.Join(errs...)
}

// HealthChecker is the interface the DataPlane uses to query backend liveness.
type HealthChecker interface {
	// Healthy returns true when addr is currently passing health checks.
	Healthy(addr string) bool
	// Start begins background health probing; it blocks until ctx is cancelled.
	Start(ctx context.Context)
}

// RateLimiter allows or rejects a request by authenticated peer identity.
type RateLimiter interface {
	Allow(identity string) bool
}

// CircuitBreaker controls whether upstream calls are permitted.
type CircuitBreaker interface {
	Allow() bool
	RecordSuccess()
	RecordFailure()
}

// MetricsRecorder is the observability hook called by the pipeline.
type MetricsRecorder interface {
	IncRequests(cluster string)
	IncErrors(cluster string)
	ObserveLatency(cluster string, d time.Duration)
	IncCircuitOpen(cluster string)
}

// Backend is an upstream endpoint with an atomic health flag.
type Backend struct {
	Addr    string
	healthy atomic.Bool
}

// Healthy returns the current health flag.
func (b *Backend) Healthy() bool { return b.healthy.Load() }

// SetHealthy updates the health flag. Called by the health checker's onChange
// callback when a probe result changes.
func (b *Backend) SetHealthy(v bool) { b.healthy.Store(v) }

// DataPlane owns and coordinates all data-plane components.
type DataPlane struct {
	cfg Config

	mu       sync.RWMutex
	backends map[string][]*Backend
	rrIdx    map[string]*atomic.Uint64

	health  HealthChecker
	rate    RateLimiter
	cb      CircuitBreaker
	metrics MetricsRecorder
	logger  *slog.Logger

	proxy *http.Server
	admin *http.Server
}

// New constructs a DataPlane from cfg. It validates cfg first and returns an
// error without allocating any resources if validation fails. Call Start to
// open listeners.
func New(cfg Config, opts ...Option) (*DataPlane, error) {
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	dp := &DataPlane{
		cfg:      cfg,
		backends: make(map[string][]*Backend, len(cfg.Clusters)),
		rrIdx:    make(map[string]*atomic.Uint64, len(cfg.Clusters)),
		logger:   slog.Default(),
	}
	for _, c := range cfg.Clusters {
		bs := make([]*Backend, 0, len(c.Endpoints))
		for _, ep := range c.Endpoints {
			b := &Backend{Addr: ep}
			b.healthy.Store(true) // optimistic until first health probe
			bs = append(bs, b)
		}
		dp.backends[c.Name] = bs
		dp.rrIdx[c.Name] = new(atomic.Uint64)
	}
	for _, o := range opts {
		o(dp)
	}
	// Install no-op defaults so the hot path never needs nil checks.
	if dp.health == nil {
		dp.health = noopHealth{}
	}
	if dp.rate == nil {
		dp.rate = noopRate{}
	}
	if dp.cb == nil {
		dp.cb = noopCB{}
	}
	if dp.metrics == nil {
		dp.metrics = noopMetrics{}
	}
	return dp, nil
}

// Option configures a DataPlane at construction time.
type Option func(*DataPlane)

// WithHealthChecker replaces the default no-op health checker.
func WithHealthChecker(h HealthChecker) Option { return func(dp *DataPlane) { dp.health = h } }

// WithRateLimiter replaces the default no-op rate limiter.
func WithRateLimiter(r RateLimiter) Option { return func(dp *DataPlane) { dp.rate = r } }

// WithCircuitBreaker replaces the default no-op circuit breaker.
func WithCircuitBreaker(cb CircuitBreaker) Option { return func(dp *DataPlane) { dp.cb = cb } }

// WithMetrics replaces the default no-op metrics recorder.
func WithMetrics(m MetricsRecorder) Option { return func(dp *DataPlane) { dp.metrics = m } }

// WithLogger replaces the default structured logger.
func WithLogger(l *slog.Logger) Option { return func(dp *DataPlane) { dp.logger = l } }

// SelectBackend returns the next healthy backend for cluster using round-robin.
// It returns an error when the cluster is unknown or all backends are unhealthy.
func (dp *DataPlane) SelectBackend(cluster string) (*Backend, error) {
	dp.mu.RLock()
	bs := dp.backends[cluster]
	ctr := dp.rrIdx[cluster]
	dp.mu.RUnlock()

	if len(bs) == 0 {
		return nil, fmt.Errorf("no backends for cluster %q", cluster)
	}
	n := uint64(len(bs))
	for i := uint64(0); i < n; i++ {
		idx := ctr.Add(1) % n
		if b := bs[idx]; b.Healthy() {
			return b, nil
		}
	}
	return nil, fmt.Errorf("no healthy backend for cluster %q", cluster)
}

// UpdateConfig replaces the active configuration atomically. Validate runs
// first; if it fails the live state is untouched.
func (dp *DataPlane) UpdateConfig(cfg Config) error {
	if err := Validate(cfg); err != nil {
		return err
	}
	nb := make(map[string][]*Backend, len(cfg.Clusters))
	nr := make(map[string]*atomic.Uint64, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		bs := make([]*Backend, 0, len(c.Endpoints))
		for _, ep := range c.Endpoints {
			b := &Backend{Addr: ep}
			b.healthy.Store(true)
			bs = append(bs, b)
		}
		nb[c.Name] = bs
		nr[c.Name] = new(atomic.Uint64)
	}
	dp.mu.Lock()
	dp.cfg = cfg
	dp.backends = nb
	dp.rrIdx = nr
	dp.mu.Unlock()
	return nil
}

// Start initializes all components in dependency order and blocks until ctx is
// cancelled. On cancellation it drains active connections for up to
// drainTimeout before returning.
//
// Startup order: health checker -> backend pool (already initialized by New) ->
// data-plane HTTP server -> admin HTTP server.
func (dp *DataPlane) Start(ctx context.Context, drainTimeout time.Duration) error {
	// 1. Launch health checker (it updates the backend pool via SetHealthy).
	hcCtx, hcCancel := context.WithCancel(ctx)
	defer hcCancel()
	go dp.health.Start(hcCtx)

	// 2. Build the data-plane HTTP server.
	handler := dp.buildPipeline("default")
	dp.proxy = &http.Server{
		Addr:      dp.cfg.ListenAddr,
		Handler:   handler,
		TLSConfig: dp.cfg.TLSConfig,
	}

	// 3. Build the admin HTTP server on a separate port.
	dp.admin = &http.Server{
		Addr:    dp.cfg.AdminAddr,
		Handler: dp.adminMux(),
	}

	// 4. Start both listeners.
	errc := make(chan error, 2)
	go func() {
		if dp.cfg.TLSConfig != nil {
			errc <- dp.proxy.ListenAndServeTLS("", "")
		} else {
			errc <- dp.proxy.ListenAndServe()
		}
	}()
	go func() { errc <- dp.admin.ListenAndServe() }()

	// 5. Wait for ctx cancellation or a fatal listen error.
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}

	// 6. Graceful shutdown: stop accepting new connections, drain in-flight.
	hcCancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), drainTimeout)
	defer shutCancel()
	adminErr := dp.admin.Shutdown(shutCtx)
	proxyErr := dp.proxy.Shutdown(shutCtx)
	return errors.Join(adminErr, proxyErr)
}

// --- no-op component implementations -----------------------------------------

type noopHealth struct{}

func (noopHealth) Healthy(string) bool       { return true }
func (noopHealth) Start(ctx context.Context) { <-ctx.Done() }

type noopRate struct{}

func (noopRate) Allow(string) bool { return true }

type noopCB struct{}

func (noopCB) Allow() bool    { return true }
func (noopCB) RecordSuccess() {}
func (noopCB) RecordFailure() {}

type noopMetrics struct{}

func (noopMetrics) IncRequests(string)                   {}
func (noopMetrics) IncErrors(string)                     {}
func (noopMetrics) ObserveLatency(string, time.Duration) {}
func (noopMetrics) IncCircuitOpen(string)                {}

// proxyFor returns a reverse proxy that forwards requests to b.
func proxyFor(b *Backend) http.Handler {
	target := &url.URL{Scheme: "http", Host: b.Addr}
	return httputil.NewSingleHostReverseProxy(target)
}
```

The sentinel `ErrUnknownCluster` is exported for callers that route by name and want a typed error; `Validate` does not raise it because an empty cluster list is a valid (if useless) config, but a router built on this type uses it to reject a route that names a cluster the config never declared.

Create `pipeline.go`:

```go
package dataplane

import (
	"context"
	"net/http"
	"time"
)

// contextKey is an unexported type for context keys scoped to this package.
// Using a named type prevents collisions with keys from other packages.
type contextKey string

const peerIdentityKey contextKey = "peer-identity"

// WithPeerIdentity stores the authenticated mTLS peer identity in ctx.
func WithPeerIdentity(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, peerIdentityKey, identity)
}

// PeerIdentity retrieves the authenticated peer identity from ctx. It returns an
// empty string when no identity was stored (a non-mTLS request).
func PeerIdentity(ctx context.Context) string {
	v, _ := ctx.Value(peerIdentityKey).(string)
	return v
}

// Middleware wraps an http.Handler with a single policy.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that middlewares[0] is outermost (runs first on
// the way in, last on the way out). The upstream handler h is the innermost.
func Chain(h http.Handler, ms ...Middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// mtlsMiddleware extracts the authenticated peer CN from the TLS state and
// stores it in the request context so downstream middleware can use it without
// re-inspecting the TLS state.
func mtlsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			identity := r.TLS.PeerCertificates[0].Subject.CommonName
			r = r.WithContext(WithPeerIdentity(r.Context(), identity))
		}
		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware rejects requests that exceed the per-identity rate limit.
// It reads the peer identity placed in the context by mtlsMiddleware, so it must
// run after mtlsMiddleware in the chain.
func rateLimitMiddleware(rl RateLimiter) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := PeerIdentity(r.Context())
			if !rl.Allow(identity) {
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// timeoutMiddleware cancels the request context after d. Upstream handlers that
// respect context cancellation will abort; the client receives a 504 from the
// reverse proxy.
func timeoutMiddleware(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// circuitBreakerMiddleware short-circuits the request when the circuit is open
// and records the outcome so the circuit breaker can track error rates.
func circuitBreakerMiddleware(cb CircuitBreaker, m MetricsRecorder, cluster string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cb.Allow() {
				m.IncCircuitOpen(cluster)
				http.Error(w, "circuit open", http.StatusServiceUnavailable)
				return
			}
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			if rw.status >= 500 {
				cb.RecordFailure()
			} else {
				cb.RecordSuccess()
			}
		})
	}
}

// metricsMiddleware records request count and upstream latency for every request
// that reaches it. Place it as the innermost middleware so it measures the true
// upstream round-trip time, not the time spent in outer policies.
func metricsMiddleware(m MetricsRecorder, cluster string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m.IncRequests(cluster)
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rw, r)
			m.ObserveLatency(cluster, time.Since(start))
			if rw.status >= 500 {
				m.IncErrors(cluster)
			}
		})
	}
}

// responseWriter captures the status code written by downstream handlers so that
// outer middleware can inspect the outcome.
type responseWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.status = code
		rw.written = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

// buildPipeline assembles the full middleware chain for cluster. The chain,
// outermost first:
//
//	mTLS extraction -> rate limit -> timeout -> circuit breaker -> metrics -> upstream proxy
//
// mTLS runs first so the peer identity is available to every downstream policy.
// metrics runs last (innermost, before upstream) so it measures upstream latency
// without the overhead of outer policies.
func (dp *DataPlane) buildPipeline(cluster string) http.Handler {
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := dp.SelectBackend(cluster)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		proxyFor(b).ServeHTTP(w, r)
	})

	return Chain(
		upstream,
		Middleware(mtlsMiddleware),
		rateLimitMiddleware(dp.rate),
		timeoutMiddleware(30*time.Second),
		circuitBreakerMiddleware(dp.cb, dp.metrics, cluster),
		metricsMiddleware(dp.metrics, cluster),
	)
}
```

`Chain` applies middlewares in reverse so the first element is outermost. The explicit `Middleware(mtlsMiddleware)` conversion is required because `mtlsMiddleware` is an unnamed `func(http.Handler) http.Handler`, not the named `Middleware` type — the other middlewares are already `Middleware` because their factory functions return that type.

Create `admin.go`:

```go
package dataplane

import (
	"encoding/json"
	"net/http"
)

// adminMux builds the admin HTTP mux. It runs on a separate port from the
// data-plane listener so admin traffic never contends with request traffic.
//
// Routes:
//
//	GET /health   -- liveness probe (always 200 while the process is alive)
//	GET /ready    -- readiness probe (200 when at least one backend is healthy)
//	GET /clusters -- upstream cluster status as JSON
//	GET /config   -- active configuration as JSON (TLS material omitted)
func (dp *DataPlane) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", dp.handleHealth)
	mux.HandleFunc("GET /ready", dp.handleReady)
	mux.HandleFunc("GET /clusters", dp.handleClusters)
	mux.HandleFunc("GET /config", dp.handleConfig)
	return mux
}

func (dp *DataPlane) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (dp *DataPlane) handleReady(w http.ResponseWriter, _ *http.Request) {
	dp.mu.RLock()
	defer dp.mu.RUnlock()
	for _, bs := range dp.backends {
		for _, b := range bs {
			if b.Healthy() {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ready"))
				return
			}
		}
	}
	http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
}

// clusterStatus is the JSON shape returned by GET /clusters.
type clusterStatus struct {
	Name      string           `json:"name"`
	Endpoints []endpointStatus `json:"endpoints"`
}

type endpointStatus struct {
	Addr    string `json:"addr"`
	Healthy bool   `json:"healthy"`
}

func (dp *DataPlane) handleClusters(w http.ResponseWriter, _ *http.Request) {
	dp.mu.RLock()
	defer dp.mu.RUnlock()
	out := make([]clusterStatus, 0, len(dp.cfg.Clusters))
	for _, c := range dp.cfg.Clusters {
		cs := clusterStatus{Name: c.Name}
		for _, b := range dp.backends[c.Name] {
			cs.Endpoints = append(cs.Endpoints, endpointStatus{
				Addr:    b.Addr,
				Healthy: b.Healthy(),
			})
		}
		out = append(out, cs)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// safeConfig is the JSON shape returned by GET /config. TLSConfig is omitted
// because it contains private key material.
type safeConfig struct {
	ListenAddr string    `json:"listen_addr"`
	AdminAddr  string    `json:"admin_addr"`
	Clusters   []Cluster `json:"clusters"`
}

func (dp *DataPlane) handleConfig(w http.ResponseWriter, _ *http.Request) {
	dp.mu.RLock()
	cfg := dp.cfg
	dp.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(safeConfig{
		ListenAddr: cfg.ListenAddr,
		AdminAddr:  cfg.AdminAddr,
		Clusters:   cfg.Clusters,
	})
}
```

The `"GET /path"` method-qualified pattern is a `ServeMux` feature from Go 1.22 on; the method prefix prevents `GET /clusters` from matching a `POST`. `GET /config` deliberately serializes a `safeConfig` rather than the raw `Config`, so the TLS private key material in `Config.TLSConfig` never leaves the process through the admin endpoint.

### The runnable demo

The demo starts the plane on loopback, waits for the admin liveness probe, proxies one real request through the full pipeline to a mock upstream, reads the readiness probe and the request counter, then cancels the context to drain and stop. It prints no addresses, timings, or timestamps, so the output is identical on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	"example.com/dataplane"
)

// demoMetrics is a minimal MetricsRecorder that counts requests.
type demoMetrics struct {
	requests atomic.Int64
}

func (m *demoMetrics) IncRequests(string)                   { m.requests.Add(1) }
func (m *demoMetrics) IncErrors(string)                     {}
func (m *demoMetrics) ObserveLatency(string, time.Duration) {}
func (m *demoMetrics) IncCircuitOpen(string)                {}

func main() {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "hello from upstream")
	}))
	defer upstream.Close()

	const (
		listenAddr = "127.0.0.1:18080"
		adminAddr  = "127.0.0.1:19902"
	)
	m := &demoMetrics{}
	dp, err := dataplane.New(
		dataplane.Config{
			ListenAddr: listenAddr,
			AdminAddr:  adminAddr,
			Clusters: []dataplane.Cluster{{
				Name:      "default",
				Endpoints: []string{upstream.Listener.Addr().String()},
			}},
		},
		dataplane.WithMetrics(m),
	)
	if err != nil {
		log.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dp.Start(ctx, 2*time.Second) }()

	if err := waitReady("http://"+adminAddr+"/health", 2*time.Second); err != nil {
		log.Fatalf("admin never came up: %v", err)
	}
	fmt.Println("data plane and admin listeners up")

	body, status := getWithRetry("http://"+listenAddr+"/", 2*time.Second)
	fmt.Printf("proxied request status: %d\n", status)
	fmt.Printf("upstream said: %s\n", strings.TrimSpace(body))

	_, ready := get("http://" + adminAddr + "/ready")
	fmt.Printf("admin /ready: %d\n", ready)
	fmt.Printf("metrics: requests=%d\n", m.requests.Load())

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
	fmt.Println("data plane stopped")
}

func get(url string) (string, int) {
	resp, err := http.Get(url)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func getWithRetry(url string, timeout time.Duration) (string, int) {
	deadline := time.Now().Add(timeout)
	for {
		body, status := get(url)
		if status != 0 {
			return body, status
		}
		if time.Now().After(deadline) {
			return "", 0
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitReady(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, status := get(url); status == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s", url)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
data plane and admin listeners up
proxied request status: 200
upstream said: hello from upstream
admin /ready: 200
metrics: requests=1
data plane stopped
```

### Tests

The tests exercise each middleware in isolation against a recorder, the admin handlers, an end-to-end request through a real upstream, and the mTLS-to-rate-limiter wiring contract — the integration property that the "your turn" prompt in the original lesson asked you to pin.

Create `dataplane_test.go`:

```go
package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// --- test doubles ------------------------------------------------------------

// countingMetrics records call counts for test assertions.
type countingMetrics struct {
	requests    atomic.Int64
	errors      atomic.Int64
	circuitOpen atomic.Int64
}

func (m *countingMetrics) IncRequests(string)                   { m.requests.Add(1) }
func (m *countingMetrics) IncErrors(string)                     { m.errors.Add(1) }
func (m *countingMetrics) ObserveLatency(string, time.Duration) {}
func (m *countingMetrics) IncCircuitOpen(string)                { m.circuitOpen.Add(1) }

// onceRate allows exactly one request, then rejects all subsequent ones.
type onceRate struct {
	used atomic.Bool
}

func (r *onceRate) Allow(string) bool {
	return r.used.CompareAndSwap(false, true)
}

// alwaysOpen simulates a permanently open circuit breaker (rejects everything).
type alwaysOpen struct{}

func (alwaysOpen) Allow() bool    { return false }
func (alwaysOpen) RecordSuccess() {}
func (alwaysOpen) RecordFailure() {}

// recordingRate records the identity string it was asked to allow.
type recordingRate struct {
	identity atomic.Value
}

func (r *recordingRate) Allow(id string) bool {
	r.identity.Store(id)
	return true
}

// --- helpers -----------------------------------------------------------------

func validCfg(endpoints ...string) Config {
	return Config{
		ListenAddr: "127.0.0.1:18080",
		AdminAddr:  "127.0.0.1:19902",
		Clusters:   []Cluster{{Name: "svc", Endpoints: endpoints}},
	}
}

func mustDP(t *testing.T, opts ...Option) *DataPlane {
	t.Helper()
	dp, err := New(validCfg("ep1:80", "ep2:80", "ep3:80"), opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return dp
}

// --- config validation -------------------------------------------------------

func TestValidateRejectsEmptyListenAddr(t *testing.T) {
	t.Parallel()
	err := Validate(Config{AdminAddr: ":9902"})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func TestValidateRejectsDuplicateCluster(t *testing.T) {
	t.Parallel()
	cfg := Config{
		ListenAddr: ":8080",
		AdminAddr:  ":9902",
		Clusters: []Cluster{
			{Name: "svc-a", Endpoints: []string{"127.0.0.1:9001"}},
			{Name: "svc-a", Endpoints: []string{"127.0.0.1:9002"}},
		},
	}
	if !errors.Is(Validate(cfg), ErrDuplicateCluster) {
		t.Fatal("expected ErrDuplicateCluster")
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	t.Parallel()
	cfg := Config{
		AdminAddr: ":9902",
		Clusters:  []Cluster{{Name: "svc-a"}},
	}
	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("ErrInvalidConfig missing from %v", err)
	}
	if !errors.Is(err, ErrNoEndpoints) {
		t.Errorf("ErrNoEndpoints missing from %v", err)
	}
}

// --- backend selection -------------------------------------------------------

func TestSelectBackendRoundRobin(t *testing.T) {
	t.Parallel()
	dp := mustDP(t)
	seen := map[string]int{}
	for i := 0; i < 9; i++ {
		b, err := dp.SelectBackend("svc")
		if err != nil {
			t.Fatalf("SelectBackend: %v", err)
		}
		seen[b.Addr]++
	}
	for addr, count := range seen {
		if count != 3 {
			t.Errorf("%s: count = %d, want 3", addr, count)
		}
	}
}

func TestSelectBackendSkipsUnhealthy(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80", "ep2:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dp.backends["svc"][0].SetHealthy(false)
	for i := 0; i < 4; i++ {
		b, err := dp.SelectBackend("svc")
		if err != nil {
			t.Fatalf("SelectBackend: %v", err)
		}
		if b.Addr != "ep2:80" {
			t.Errorf("got %q, want ep2:80", b.Addr)
		}
	}
}

func TestSelectBackendAllUnhealthyReturnsError(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dp.backends["svc"][0].SetHealthy(false)
	if _, err := dp.SelectBackend("svc"); err == nil {
		t.Fatal("expected error when all backends unhealthy")
	}
}

// --- pipeline middleware ------------------------------------------------------

func TestRateLimitMiddlewareAllowsFirst(t *testing.T) {
	t.Parallel()
	rl := &onceRate{}
	h := rateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", w.Code)
	}
}

func TestRateLimitMiddlewareRejectsSecond(t *testing.T) {
	t.Parallel()
	rl := &onceRate{}
	h := rateLimitMiddleware(rl)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i == 1 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("second request: status = %d, want 429", w.Code)
		}
	}
}

func TestCircuitBreakerMiddlewareRejectsWhenOpen(t *testing.T) {
	t.Parallel()
	m := &countingMetrics{}
	h := circuitBreakerMiddleware(alwaysOpen{}, m, "svc")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if m.circuitOpen.Load() != 1 {
		t.Fatalf("circuitOpen = %d, want 1", m.circuitOpen.Load())
	}
}

func TestMetricsMiddlewareRecordsRequest(t *testing.T) {
	t.Parallel()
	m := &countingMetrics{}
	h := metricsMiddleware(m, "svc")(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if m.requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", m.requests.Load())
	}
}

func TestMTLSMiddlewareNoIdentityWithoutTLS(t *testing.T) {
	t.Parallel()
	var got string
	h := mtlsMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = PeerIdentity(r.Context())
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got != "" {
		t.Fatalf("identity = %q, want empty for non-TLS request", got)
	}
}

// --- admin API ---------------------------------------------------------------

func TestAdminHealthAlwaysOK(t *testing.T) {
	t.Parallel()
	dp := mustDP(t)
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	dp.adminMux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestAdminReadyWhenBackendHealthy(t *testing.T) {
	t.Parallel()
	dp := mustDP(t)
	r := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	dp.adminMux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestAdminReadyUnavailableWhenAllUnhealthy(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dp.backends["svc"][0].SetHealthy(false)
	r := httptest.NewRequest(http.MethodGet, "/ready", nil)
	w := httptest.NewRecorder()
	dp.adminMux().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestAdminClustersReturnsJSON(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("ep1:80", "ep2:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	w := httptest.NewRecorder()
	dp.adminMux().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var cs []clusterStatus
	if err := json.NewDecoder(w.Body).Decode(&cs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cs) != 1 || len(cs[0].Endpoints) != 2 {
		t.Fatalf("unexpected shape: %+v", cs)
	}
}

func TestAdminConfigOmitsTLS(t *testing.T) {
	t.Parallel()
	dp := mustDP(t)
	r := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()
	dp.adminMux().ServeHTTP(w, r)
	var raw map[string]any
	if err := json.NewDecoder(w.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw["TLSConfig"]; present {
		t.Fatal("/config leaked TLS material")
	}
	if raw["listen_addr"] != "127.0.0.1:18080" {
		t.Fatalf("listen_addr = %v, want 127.0.0.1:18080", raw["listen_addr"])
	}
}

// --- integration -------------------------------------------------------------

// TestEndToEnd wires a real mock upstream through the full pipeline and verifies
// that requests arrive and metrics are recorded.
func TestEndToEnd(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "upstream ok")
	}))
	t.Cleanup(upstream.Close)

	m := &countingMetrics{}
	dp, err := New(
		Config{
			ListenAddr: "127.0.0.1:18081",
			AdminAddr:  "127.0.0.1:19903",
			Clusters:   []Cluster{{Name: "default", Endpoints: []string{upstream.Listener.Addr().String()}}},
		},
		WithMetrics(m),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h := dp.buildPipeline("default")
	r := httptest.NewRequest(http.MethodGet, "/ping", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if m.requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", m.requests.Load())
	}
}

// TestUpdateConfigAtomic verifies that UpdateConfig replaces the backend pool and
// that the new backend is selected immediately on the next call.
func TestUpdateConfigAtomic(t *testing.T) {
	t.Parallel()
	dp, err := New(validCfg("old:80"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := dp.UpdateConfig(Config{
		ListenAddr: "127.0.0.1:18080",
		AdminAddr:  "127.0.0.1:19902",
		Clusters:   []Cluster{{Name: "svc", Endpoints: []string{"new:80"}}},
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	b, err := dp.SelectBackend("svc")
	if err != nil {
		t.Fatalf("SelectBackend: %v", err)
	}
	if b.Addr != "new:80" {
		t.Fatalf("addr = %q, want new:80", b.Addr)
	}
}

// TestRateLimitIdentityPropagation pins the mTLS-to-rate-limiter wiring: an
// identity placed in the request context must reach the rate limiter unchanged.
func TestRateLimitIdentityPropagation(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	rr := &recordingRate{}
	dp, err := New(
		Config{
			ListenAddr: "127.0.0.1:18082",
			AdminAddr:  "127.0.0.1:19904",
			Clusters:   []Cluster{{Name: "default", Endpoints: []string{upstream.Listener.Addr().String()}}},
		},
		WithRateLimiter(rr),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	h := dp.buildPipeline("default")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r = r.WithContext(WithPeerIdentity(r.Context(), "frontend.prod.svc"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	got, _ := rr.identity.Load().(string)
	if got != "frontend.prod.svc" {
		t.Fatalf("rate limiter saw identity %q, want frontend.prod.svc", got)
	}
}

// TestStartShutdownDrains brings the real listeners up, proxies one request, and
// confirms a context cancellation drains and stops cleanly.
func TestStartShutdownDrains(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	t.Cleanup(upstream.Close)

	dp, err := New(Config{
		ListenAddr: "127.0.0.1:18083",
		AdminAddr:  "127.0.0.1:19905",
		Clusters:   []Cluster{{Name: "default", Endpoints: []string{upstream.Listener.Addr().String()}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dp.Start(ctx, 2*time.Second) }()

	if !waitFor(t, "http://127.0.0.1:19905/health", 2*time.Second) {
		t.Fatal("admin never became reachable")
	}
	resp, err := http.Get("http://127.0.0.1:18083/")
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil after clean drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
}

func waitFor(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// --- examples ----------------------------------------------------------------

func ExampleValidate() {
	err := Validate(Config{AdminAddr: ":9902"})
	fmt.Println(errors.Is(err, ErrInvalidConfig))
	// Output:
	// true
}

func ExamplePeerIdentity() {
	ctx := WithPeerIdentity(context.Background(), "frontend.prod.svc")
	fmt.Println(PeerIdentity(ctx))
	// Output:
	// frontend.prod.svc
}
```

## Review

The wired plane is correct when the chain order holds, the lifecycle drains, and the cross-component wiring carries identity end to end. The middleware tests pin each policy in isolation: `TestRateLimitMiddlewareRejectsSecond` proves the limiter rejects with 429, `TestCircuitBreakerMiddlewareRejectsWhenOpen` proves an open breaker returns 503 and counts the trip, and `TestMTLSMiddlewareNoIdentityWithoutTLS` proves a non-TLS request yields an empty identity rather than a panic. `TestEndToEnd` runs a real request through the assembled `buildPipeline` to a live upstream and checks both the 200 and the recorded request, which is the integration smoke test. `TestRateLimitIdentityPropagation` is the wiring contract that the original "your turn" asked for: an identity placed in the context reaches the rate limiter unchanged, so the mTLS-to-limiter seam cannot silently regress to a shared key. `TestStartShutdownDrains` is the lifecycle proof — real listeners up, one proxied request, then a context cancellation that returns from `Start` with no error. The two mistakes most likely to break this code are reordering the chain so metrics sits outside the policies (it then measures policy overhead and misses short-circuited requests) and calling `Close` instead of `Shutdown` (it severs in-flight connections instead of draining them); run the suite with `-race` to catch a status read that races the upstream write through `responseWriter`.

## Resources

- [net/http `ServeMux` routing patterns (Go 1.22)](https://pkg.go.dev/net/http#ServeMux) — the method-qualified patterns (`"GET /path"`) the admin mux relies on.
- [net/http/httputil `ReverseProxy`](https://pkg.go.dev/net/http/httputil#ReverseProxy) — the stdlib reverse proxy behind `proxyFor`, with `Director` and `ModifyResponse` hooks for header rewriting.
- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — the graceful-drain primitive that the lifecycle uses with a deadline instead of `Close`.
- [Envoy architecture: listeners and the filter chain](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/listeners/listeners) — the production reference for the middleware-chain model, where each HTTP filter corresponds to one `Middleware` here.

---

Back to [01-config-and-atomic-reload.md](01-config-and-atomic-reload.md) | Next: [Source Connectors](../../43-capstone-stream-processing-engine/01-source-connectors/00-concepts.md)
