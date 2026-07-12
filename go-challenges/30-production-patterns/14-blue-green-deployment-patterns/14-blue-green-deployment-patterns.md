# 14. Blue-Green Deployment Patterns

Blue-green deployment keeps two backend environments alive simultaneously — "blue" (current) and "green" (new) — and atomically shifts traffic between them. The hard part is not the proxy itself but the invariants around it: the switch must be atomic (no request sees a partial state), health must be validated before any traffic moves, and rollback must work without locks blocking the hot path. This lesson builds a `bluegreen` package that models those constraints in pure Go using `sync/atomic` and `net/http/httputil`.

```text
bluegreen/
  go.mod
  bluegreen.go
  bluegreen_test.go
  cmd/demo/main.go
```

## Concepts

### The Data Plane vs. the Control Plane

A production blue-green proxy has two logically separate planes. The **data plane** handles every incoming request and must be as fast as possible — ideally lock-free. The **control plane** handles operator commands (switch, set-canary, rollback) and can tolerate latency. Mixing them with a single `sync.RWMutex` on every request is a common mistake: under high concurrency the mutex becomes a bottleneck. The better design stores the hot-path routing state in an `atomic.Pointer[T]` so the data plane reads never block.

### Atomic Routing State

`sync/atomic.Pointer[T]` (added in Go 1.19) provides a lock-free, type-safe way to swap a pointer. The pattern:

```go
type routingState struct {
	activeURL     *url.URL
	canaryURL     *url.URL
	canaryPercent int // 0 = no canary
}

var state atomic.Pointer[routingState]
```

`state.Store(newState)` replaces the pointer in one hardware instruction; `state.Load()` reads it. Any request in flight still holds a reference to the old state and completes normally. This is safe-pointer replacement: Go's garbage collector keeps the old state alive until the last goroutine holding it finishes.

### Health Check Before Switch

Switching to an unhealthy backend is worse than not switching at all: it causes a full-traffic outage instead of a partial one. The contract enforced here is:

1. A `Switch("green")` or `SetCanary(n)` call performs a synchronous health check against the target's `/health` endpoint.
2. If the check fails, the state is not updated and an error is returned to the caller.
3. A background watcher continuously re-checks the active backend and rolls back to "blue" if it fails.

### httputil.ReverseProxy and the Rewrite Field

`httputil.NewSingleHostReverseProxy` uses the deprecated `Director` field internally. The current API uses the `Rewrite` field (introduced in Go 1.20) on `httputil.ReverseProxy` directly:

```go
proxy := &httputil.ReverseProxy{
	Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.SetXForwarded()
	},
}
```

`Rewrite` is called after hop-by-hop headers have been stripped and after unparsable query parameters have been removed, which makes it safer than `Director`. The `Rewrite` function must not be set at the same time as `Director`.

### Canary Routing

A canary sends a small percentage of traffic to the new version before a full switch. The routing decision must be per-request, not per-connection, so it is made in `ServeHTTP` on every call. The implementation uses a simple `rand.IntN(100) < canaryPercent` check. Because the routing state is atomic, the percentage can be changed while requests are in flight without a mutex.

### Trade-offs and Failure Modes

- **Sticky sessions**: the simple percentage-based canary is stateless; the same user may hit blue on one request and green on the next. For user-visible features this requires session affinity (a cookie or header) at the proxy layer — not implemented here.
- **Split-brain during rollback**: if the watcher and an operator call `Switch("blue")` concurrently, the second store wins. This is safe because both stores write a valid state.
- **Health check latency**: the synchronous health check in `Switch` adds latency to the control path. The timeout (configurable via `WithHealthTimeout`) must be short enough to not block operators.

## Exercises

This is a library, not a program. Verify it with `go test`.

### Exercise 1: Routing State and the Proxy Type

Create `bluegreen.go`:

```go
package bluegreen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by Switch and SetCanary.
var (
	ErrUnknownTarget  = errors.New("bluegreen: unknown target name")
	ErrUnhealthy      = errors.New("bluegreen: target failed health check")
	ErrInvalidPercent = errors.New("bluegreen: canary percent must be 0-100")
)

// routingState is an immutable snapshot of the routing configuration.
// The proxy swaps the whole struct atomically; readers never see a partial update.
type routingState struct {
	activeURL     *url.URL
	activeName    string
	canaryURL     *url.URL
	canaryName    string
	canaryPercent int // 0 means no canary; 100 means full switch
}

// Proxy is a reverse proxy that routes traffic between a blue and a green backend.
// The zero value is not valid; use New.
type Proxy struct {
	blueURL   *url.URL
	blueName  string
	greenURL  *url.URL
	greenName string

	state atomic.Pointer[routingState] // hot path; lock-free

	healthPath    string
	healthTimeout time.Duration
	logger        *slog.Logger
}

// Option configures a Proxy.
type Option func(*Proxy) error

// WithHealthPath sets the path used for health checks (default: /health).
func WithHealthPath(path string) Option {
	return func(p *Proxy) error {
		if path == "" {
			return fmt.Errorf("%w: health path must not be empty", ErrUnhealthy)
		}
		p.healthPath = path
		return nil
	}
}

// WithHealthTimeout sets the HTTP client timeout for health checks (default: 2s).
func WithHealthTimeout(d time.Duration) Option {
	return func(p *Proxy) error {
		if d <= 0 {
			return fmt.Errorf("%w: health timeout must be positive", ErrUnhealthy)
		}
		p.healthTimeout = d
		return nil
	}
}

// WithLogger sets the structured logger (default: slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(p *Proxy) error {
		if l == nil {
			return fmt.Errorf("%w: logger must not be nil", ErrUnhealthy)
		}
		p.logger = l
		return nil
	}
}

// New creates a Proxy that routes traffic to blueAddr by default.
// blueAddr and greenAddr must be valid URLs (e.g. "http://localhost:9001").
func New(blueAddr, greenAddr string, opts ...Option) (*Proxy, error) {
	bu, err := url.Parse(blueAddr)
	if err != nil {
		return nil, fmt.Errorf("bluegreen: parse blue URL: %w", err)
	}
	gu, err := url.Parse(greenAddr)
	if err != nil {
		return nil, fmt.Errorf("bluegreen: parse green URL: %w", err)
	}

	p := &Proxy{
		blueURL:       bu,
		blueName:      "blue",
		greenURL:      gu,
		greenName:     "green",
		healthPath:    "/health",
		healthTimeout: 2 * time.Second,
		logger:        slog.Default(),
	}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			return nil, err
		}
	}

	// Start with blue active, no canary.
	p.state.Store(&routingState{
		activeURL:  bu,
		activeName: "blue",
	})
	return p, nil
}

// makeProxy returns an httputil.ReverseProxy that forwards requests to target.
// It uses the Rewrite field (Go 1.20+) instead of the deprecated Director.
func makeProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
		},
	}
}

// ServeHTTP implements http.Handler. It is lock-free on the hot path.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := p.state.Load()
	target := s.activeURL
	name := s.activeName

	// Canary: route a fraction of traffic to the green backend.
	if s.canaryPercent > 0 && s.canaryURL != nil {
		if rand.IntN(100) < s.canaryPercent {
			target = s.canaryURL
			name = s.canaryName
		}
	}

	w.Header().Set("X-Routed-To", name)
	makeProxy(target).ServeHTTP(w, r)
}

// Switch atomically moves 100% of traffic to "blue" or "green".
// It performs a health check before switching and returns ErrUnhealthy if the
// check fails — the active backend is not changed in that case.
func (p *Proxy) Switch(target string) error {
	var targetURL *url.URL
	var targetName string
	switch target {
	case "blue":
		targetURL, targetName = p.blueURL, p.blueName
	case "green":
		targetURL, targetName = p.greenURL, p.greenName
	default:
		return fmt.Errorf("%w: %q", ErrUnknownTarget, target)
	}

	if err := p.checkHealth(targetURL); err != nil {
		return fmt.Errorf("%w: %s", ErrUnhealthy, err)
	}

	old := p.state.Load().activeName
	p.state.Store(&routingState{
		activeURL:  targetURL,
		activeName: targetName,
	})
	p.logger.Info("bluegreen: switched", "from", old, "to", targetName)
	return nil
}

// SetCanary routes canaryPercent (0-100) of traffic to the green backend.
// 0 disables canary routing. 100 is equivalent to Switch("green").
// Returns ErrInvalidPercent for out-of-range values and ErrUnhealthy if
// the green backend fails its health check.
func (p *Proxy) SetCanary(canaryPercent int) error {
	if canaryPercent < 0 || canaryPercent > 100 {
		return fmt.Errorf("%w: got %d", ErrInvalidPercent, canaryPercent)
	}
	if canaryPercent == 100 {
		return p.Switch("green")
	}
	if canaryPercent > 0 {
		if err := p.checkHealth(p.greenURL); err != nil {
			return fmt.Errorf("%w: green: %s", ErrUnhealthy, err)
		}
	}

	cur := p.state.Load()
	p.state.Store(&routingState{
		activeURL:     cur.activeURL,
		activeName:    cur.activeName,
		canaryURL:     p.greenURL,
		canaryName:    p.greenName,
		canaryPercent: canaryPercent,
	})
	p.logger.Info("bluegreen: canary set", "percent", canaryPercent)
	return nil
}

// Rollback immediately routes 100% of traffic back to blue.
// It does not perform a health check — rollback must always succeed.
func (p *Proxy) Rollback() {
	old := p.state.Load().activeName
	p.state.Store(&routingState{
		activeURL:  p.blueURL,
		activeName: p.blueName,
	})
	p.logger.Info("bluegreen: rollback", "from", old, "to", "blue")
}

// ActiveName returns the name of the currently active backend ("blue" or "green").
func (p *Proxy) ActiveName() string {
	return p.state.Load().activeName
}

// CanaryPercent returns the current canary percentage (0 if no canary is active).
func (p *Proxy) CanaryPercent() int {
	return p.state.Load().canaryPercent
}

// StartWatcher launches a background goroutine that polls the active backend's
// health every interval. If the active backend is "green" and fails, it calls
// Rollback automatically. The goroutine exits when ctx is cancelled.
func (p *Proxy) StartWatcher(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s := p.state.Load()
				if err := p.checkHealth(s.activeURL); err != nil {
					p.logger.Warn("bluegreen: active backend unhealthy", "backend", s.activeName, "err", err)
					if s.activeName == "green" {
						p.Rollback()
					}
				}
			}
		}
	}()
}

// checkHealth performs a GET request to the backend's health endpoint.
func (p *Proxy) checkHealth(target *url.URL) error {
	client := &http.Client{Timeout: p.healthTimeout}
	healthURL := target.JoinPath(p.healthPath)
	resp, err := client.Get(healthURL.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d", resp.StatusCode)
	}
	return nil
}
```

The atomic swap in `Switch` and `SetCanary` stores a completely new `routingState` value; a request that loaded the old state mid-flight holds a valid pointer and completes normally. No lock is held during the actual proxying.

### Exercise 2: Tests

Create `bluegreen_test.go`:

```go
package bluegreen

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newBackend starts a test HTTP server that responds to / and /health.
// If healthy is false the /health endpoint returns 503.
func newBackend(t *testing.T, name string, healthy *bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend=%s", name)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !*healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return httptest.NewServer(mux)
}

func TestNewDefaultsToBlue(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, err := New(blue.URL, green.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.ActiveName(); got != "blue" {
		t.Fatalf("ActiveName = %q, want blue", got)
	}
}

func TestSwitchToGreen(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	if err := p.Switch("green"); err != nil {
		t.Fatalf("Switch green: %v", err)
	}
	if got := p.ActiveName(); got != "green" {
		t.Fatalf("ActiveName = %q, want green", got)
	}
}

func TestSwitchRejectsUnhealthyTarget(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, false
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	err := p.Switch("green")
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("err = %v, want ErrUnhealthy", err)
	}
	// Active must still be blue.
	if got := p.ActiveName(); got != "blue" {
		t.Fatalf("ActiveName = %q, want blue after failed switch", got)
	}
}

func TestSwitchRejectsUnknownTarget(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	err := p.Switch("purple")
	if !errors.Is(err, ErrUnknownTarget) {
		t.Fatalf("err = %v, want ErrUnknownTarget", err)
	}
}

func TestRollback(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	_ = p.Switch("green")
	p.Rollback()
	if got := p.ActiveName(); got != "blue" {
		t.Fatalf("ActiveName = %q, want blue after rollback", got)
	}
}

func TestSetCanaryRejectsInvalidPercent(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	for _, pct := range []int{-1, 101, 200} {
		if err := p.SetCanary(pct); !errors.Is(err, ErrInvalidPercent) {
			t.Fatalf("SetCanary(%d): err = %v, want ErrInvalidPercent", pct, err)
		}
	}
}

func TestSetCanaryAt100SwitchesToGreen(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	if err := p.SetCanary(100); err != nil {
		t.Fatalf("SetCanary(100): %v", err)
	}
	if got := p.ActiveName(); got != "green" {
		t.Fatalf("ActiveName = %q, want green after SetCanary(100)", got)
	}
	if got := p.CanaryPercent(); got != 0 {
		t.Fatalf("CanaryPercent = %d, want 0 after full promotion", got)
	}
}

func TestSetCanaryRejectsUnhealthyGreen(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, false
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	if err := p.SetCanary(20); !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("SetCanary(20) with unhealthy green: err = %v, want ErrUnhealthy", err)
	}
}

func TestServeHTTPRoutesToActiveBackend(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("X-Routed-To"); got != "blue" {
		t.Fatalf("X-Routed-To = %q, want blue", got)
	}
}

func TestWithHealthTimeoutRejectsNonPositive(t *testing.T) {
	t.Parallel()

	_, err := New("http://localhost:9001", "http://localhost:9002", WithHealthTimeout(-1))
	if err == nil {
		t.Fatal("expected error for negative health timeout")
	}
}

func ExampleProxy_Switch() {
	blueHealthy, greenHealthy := true, true
	blue := newMockBackend("blue", &blueHealthy)
	green := newMockBackend("green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, _ := New(blue.URL, green.URL)
	fmt.Println(p.ActiveName())
	_ = p.Switch("green")
	fmt.Println(p.ActiveName())
	p.Rollback()
	fmt.Println(p.ActiveName())
	// Output:
	// blue
	// green
	// blue
}

// newMockBackend is a helper for the Example function (cannot call t.Helper outside a test).
func newMockBackend(name string, healthy *bool) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend=%s", name)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !*healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	return httptest.NewServer(mux)
}

// Your turn: add TestWatcherRollsBackOnUnhealthyGreen. Start a proxy with green
// active, mark the green backend unhealthy, call StartWatcher with a short interval,
// wait for two intervals, and assert that ActiveName() == "blue".
// Use time.Sleep sparingly; prefer a poll loop with a deadline.

func TestWithHealthTimeout(t *testing.T) {
	t.Parallel()

	blueHealthy, greenHealthy := true, true
	blue := newBackend(t, "blue", &blueHealthy)
	green := newBackend(t, "green", &greenHealthy)
	defer blue.Close()
	defer green.Close()

	p, err := New(blue.URL, green.URL, WithHealthTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("New with WithHealthTimeout: %v", err)
	}
	if err := p.Switch("green"); err != nil {
		t.Fatalf("Switch green: %v", err)
	}
}
```

### Exercise 3: The Demo Program

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"example.com/bluegreen"
)

func startBackend(name, addr string, healthy *bool) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"backend": name, "time": time.Now().Format(time.RFC3339)})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !*healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("backend %s: %v", name, err)
		}
	}()
}

func main() {
	blueHealthy := true
	greenHealthy := true
	startBackend("blue", ":9001", &blueHealthy)
	startBackend("green", ":9002", &greenHealthy)
	time.Sleep(100 * time.Millisecond)

	p, err := bluegreen.New("http://localhost:9001", "http://localhost:9002")
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartWatcher(ctx, 3*time.Second)

	// Control API
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"active":         p.ActiveName(),
			"canary_percent": p.CanaryPercent(),
		})
	})
	mux.HandleFunc("POST /switch", func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if err := p.Switch(target); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "switched to %s\n", target)
	})
	mux.HandleFunc("POST /canary", func(w http.ResponseWriter, r *http.Request) {
		pct, _ := strconv.Atoi(r.URL.Query().Get("percent"))
		if err := p.SetCanary(pct); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Fprintf(w, "canary set to %d%%\n", pct)
	})
	mux.HandleFunc("POST /rollback", func(w http.ResponseWriter, r *http.Request) {
		p.Rollback()
		fmt.Fprintln(w, "rolled back to blue")
	})

	go func() {
		log.Println("control API on :8081")
		if err := http.ListenAndServe(":8081", mux); err != nil {
			log.Fatal(err)
		}
	}()

	log.Println("proxy on :8080 (active: blue)")
	if err := http.ListenAndServe(":8080", p); err != nil {
		log.Fatal(err)
	}
}
```

Run the demo with:

```bash
go run ./cmd/demo &
sleep 1

# Proxy routes to blue
curl -s localhost:8080/ | grep backend

# Status
curl -s localhost:8081/status

# Start canary: 30% to green
curl -s -X POST "localhost:8081/canary?percent=30"

# Full switch to green
curl -s -X POST "localhost:8081/switch?target=green"
curl -s localhost:8081/status

# Rollback
curl -s -X POST localhost:8081/rollback
curl -s localhost:8081/status

kill %1
```

## Common Mistakes

### Using sync.RWMutex on the Hot Path

Wrong: wrapping `ServeHTTP` with `p.mu.RLock()` / `p.mu.RUnlock()` to read the active backend.

What happens: under high concurrency the mutex becomes a bottleneck. All goroutines block waiting for the read lock whenever a write is in progress, even though reads vastly outnumber writes.

Fix: store the routing state in an `atomic.Pointer[routingState]`. The data plane calls `p.state.Load()` — a single hardware instruction — and the old state remains valid for any in-flight request until GC collects it.

### Using the Deprecated Director Field

Wrong:

```go
proxy := httputil.NewSingleHostReverseProxy(target) // uses Director internally
```

What happens: `Director` is called after hop-by-hop headers have been removed, but a malicious client can inject headers via the `Connection` header that `Director` cannot reliably strip. X-Forwarded-* headers from clients are also preserved by default, enabling IP spoofing.

Fix: construct the `ReverseProxy` directly with the `Rewrite` field:

```go
proxy := &httputil.ReverseProxy{
	Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.SetXForwarded()
	},
}
```

### Not Validating Health Before Switching

Wrong: calling `p.state.Store(newState)` without a prior health check.

What happens: if green is misconfigured or still starting, 100% of traffic immediately starts receiving errors or timeouts. The blast radius is the entire production traffic, not a small canary slice.

Fix: always check the target's health endpoint and return `ErrUnhealthy` before storing the new state. Rollback must not perform a health check (blue must always be assumed to be the last known good state).

### Switching and Rolling Back Concurrently Without Atomics

Wrong: storing the active backend in a plain `*url.URL` field and reading it without synchronization.

What happens: a data race. The Go race detector catches this immediately, and it can produce torn reads on 32-bit platforms or under certain CPU memory models.

Fix: `atomic.Pointer[T]` provides sequentially consistent loads and stores without explicit locking.

## Verification

From `~/go-exercises/bluegreen`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `ExampleProxy_Switch` function is verified automatically by `go test` against its `// Output:` comment.

## Summary

- Two live environments (blue=stable, green=new) allow instant rollback by a single atomic pointer swap.
- Store routing state in `atomic.Pointer[routingState]` so `ServeHTTP` is lock-free on the hot path.
- Use `httputil.ReverseProxy.Rewrite` (not `Director`) for safe header handling.
- Health-check the target before any switch or canary promotion; never health-check before rollback.
- A background watcher auto-rolls back to blue when the active green fails its health check.
- The control plane (Switch, SetCanary, Rollback) can be slow; the data plane (ServeHTTP) must be fast.

## What's Next

Next: [Lambda Handler Patterns](../../31-cloud-native-go/01-lambda-handler-patterns/01-lambda-handler-patterns.md).

## Resources

- [net/http/httputil.ReverseProxy](https://pkg.go.dev/net/http/httputil#ReverseProxy) — current Rewrite field documentation and ProxyRequest type.
- [sync/atomic.Pointer](https://pkg.go.dev/sync/atomic#Pointer) — type-safe generic atomic pointer added in Go 1.19.
- [Blue-Green Deployments (Martin Fowler)](https://martinfowler.com/bliki/BlueGreenDeployment.html) — original definition and trade-offs.
- [Canary Releases (Martin Fowler)](https://martinfowler.com/bliki/CanaryRelease.html) — gradual traffic shifting rationale.
- [Go Blog: Backward Compatibility and GODEBUG](https://go.dev/blog/godebug) — context for why httputil.Director is kept but deprecated.
