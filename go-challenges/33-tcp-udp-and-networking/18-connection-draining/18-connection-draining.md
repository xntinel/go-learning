# 18. Connection Draining

Killing a server process immediately drops every in-flight request. Connection draining is the controlled alternative: stop accepting new traffic, wait for active handlers to finish, then force-close after a deadline. Load balancers poll a `/health` endpoint to learn when to stop sending traffic, so the server needs a three-state machine (healthy → draining → stopped) that drives both the health response and the in-flight gate. The subtle failure modes are the race between incoming requests and the drain CAS, a missing timeout that lets one stuck handler block a whole deploy, and the `/health` endpoint accidentally running through the in-flight middleware and delaying shutdown.

```text
drain/
  go.mod
  drain.go
  drain_test.go
  cmd/demo/main.go
```

## Concepts

### Three-Phase Shutdown

Connection draining follows three sequential phases:

1. Stop accepting — reject new non-health requests immediately on drain signal.
2. Wait for in-flight — a `sync.WaitGroup` tracks active handlers; drain blocks until the counter reaches zero or the timeout fires.
3. Force-close — `http.Server.Shutdown` closes the listener, drains idle keep-alive connections, and waits for any remaining active connections before returning.

`http.Server.Shutdown` covers phases 1 and 3 for HTTP at the transport level. The in-flight WaitGroup in phase 2 operates at the handler level, which is finer-grained: `Shutdown` waits for connections, not for individual handler goroutines to return. A raw TCP server has no built-in `Shutdown`; you must manage the WaitGroup yourself.

### In-Flight Tracking and the WaitGroup Race

The in-flight counter must be incremented before the handler body runs and decremented after it returns. The canonical placement is at the entry point of a middleware wrapper:

```go
s.inflight.Add(1)
defer s.inflight.Done()
// handler body executes here
```

Placing `Add` inside the handler body itself creates a window after the request is dispatched but before it is counted. Any code that checks `inflight.Wait() == 0` during that window will see a false-zero and proceed to shut down. Wrapping every route through a single `ServeHTTP` entry closes that window for all routes uniformly.

The `/health` endpoint must bypass the WaitGroup. If health checks are counted as in-flight, they delay shutdown — and during drain the health endpoint may itself be rejected, preventing the LB from learning the server state.

### Atomic State Transitions

State is stored in an `atomic.Int32`. Transitions use `CompareAndSwap` (CAS) rather than a mutex-guarded if-then-set:

```go
if !s.state.CompareAndSwap(int32(StateHealthy), int32(StateDraining)) {
	return ErrAlreadyDraining
}
```

A mutex-protected check-then-set avoids the data race but still fails: neither caller returns after the block, so both proceed to call `inflight.Wait()` and `s.srv.Shutdown` — the second `Shutdown` returns `http.ErrServerClosed`. CAS, combined with an immediate return on failure, ensures only one caller proceeds.

### Drain Timeout and the Select

The drain select is the backstop against stuck or deadlocked handlers:

```go
select {
case <-done:       // all in-flight handlers finished
case <-t.C:        // drain timeout elapsed
case <-ctx.Done(): // caller cancelled
}
```

Without the timeout, a single blocked handler would stall a deploy indefinitely. The timeout should be shorter than the load balancer's own connection-drain timeout so the process exits before the LB gives up and marks it failed.

### Health Check State Machine

Load balancers (AWS ALB, GCP, Nginx upstream health checks) poll the health endpoint on an interval (typically 5-30 s). The state machine drives the response:

| State | HTTP Status | Body |
|---|---|---|
| healthy | 200 OK | ok |
| draining | 503 | draining |
| stopped | 503 | stopped |

When the LB sees 503 it stops sending new connections. The drain phase must last at least one full poll interval before the server shuts down; otherwise some new requests land on an already-draining backend.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/drain/cmd/demo
cd ~/go-exercises/drain
go mod init example.com/drain
```

This is a library package verified with `go test`, not a standalone program.

### Exercise 1: State Machine and Server Type

Create `drain.go`:

```go
package drain

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// State represents the operational state of a draining server.
type State int32

const (
	StateHealthy  State = 0
	StateDraining State = 1
	StateStopped  State = 2
)

// String returns the label written to the /health response body.
func (s State) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateDraining:
		return "draining"
	default:
		return "stopped"
	}
}

// ErrAlreadyDraining is returned by Drain when the server is not in StateHealthy.
var ErrAlreadyDraining = errors.New("drain: server is not in the healthy state")

// Server is an HTTP server with in-flight request tracking, a /health endpoint
// for load balancer integration, and a Drain method for graceful shutdown.
type Server struct {
	srv      *http.Server
	mux      *http.ServeMux
	state    atomic.Int32
	inflight sync.WaitGroup
	timeout  time.Duration
}

// New creates a Server that will listen on addr.  drainTimeout caps how long
// Drain waits for in-flight requests before proceeding to force-close.
func New(addr string, drainTimeout time.Duration) *Server {
	s := &Server{
		mux:     http.NewServeMux(),
		timeout: drainTimeout,
	}
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s,
	}
	return s
}

// ServeHTTP implements http.Handler.  Requests to /health bypass in-flight
// tracking so health checks can always reach the state machine.
// All other requests during draining receive 503 Service Unavailable.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health" {
		s.serveHealth(w)
		return
	}
	if State(s.state.Load()) != StateHealthy {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	s.inflight.Add(1)
	defer s.inflight.Done()
	s.mux.ServeHTTP(w, r)
}

func (s *Server) serveHealth(w http.ResponseWriter) {
	st := State(s.state.Load())
	if st != StateHealthy {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, st.String())
		return
	}
	fmt.Fprint(w, "ok")
}

// HandleFunc registers a handler function on the underlying mux.
func (s *Server) HandleFunc(pattern string, fn http.HandlerFunc) {
	s.mux.HandleFunc(pattern, fn)
}

// State returns the current server state.
func (s *Server) State() State {
	return State(s.state.Load())
}

// ListenAndServe starts the server.  It returns http.ErrServerClosed on
// graceful shutdown.
func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Drain transitions the server from healthy to draining, waits for in-flight
// requests to finish (up to the drain timeout or ctx cancellation), then calls
// http.Server.Shutdown to close the listener and remaining connections.
// It returns ErrAlreadyDraining if the server was not in StateHealthy.
func (s *Server) Drain(ctx context.Context) error {
	if !s.state.CompareAndSwap(int32(StateHealthy), int32(StateDraining)) {
		return ErrAlreadyDraining
	}

	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()

	t := time.NewTimer(s.timeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	case <-ctx.Done():
	}

	s.state.Store(int32(StateStopped))
	return s.srv.Shutdown(context.Background())
}
```

The `/health` route is handled before the state check and WaitGroup increment so health checks never contribute to the in-flight count.

### Exercise 2: Tests

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestInitialStateIsHealthy(t *testing.T) {
	t.Parallel()
	s := New(":0", time.Second)
	if s.State() != StateHealthy {
		t.Fatalf("State() = %v, want %v", s.State(), StateHealthy)
	}
}

func TestHealthEndpointWhenHealthy(t *testing.T) {
	t.Parallel()
	s := New(":0", time.Second)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", resp.StatusCode, body)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want \"ok\"", body)
	}
}

func TestHealthEndpointWhenDraining(t *testing.T) {
	t.Parallel()
	s := New(":0", 5*time.Second)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Transition to draining without calling Drain (which also calls srv.Shutdown
	// and would close s.srv, not the httptest server's server).
	s.state.CompareAndSwap(int32(StateHealthy), int32(StateDraining))

	resp, err := ts.Client().Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %q", resp.StatusCode, body)
	}
	if string(body) != "draining" {
		t.Fatalf("body = %q, want \"draining\"", body)
	}
}

func TestNonHealthRouteRejectedWhenDraining(t *testing.T) {
	t.Parallel()
	s := New(":0", time.Second)
	ts := httptest.NewServer(s)
	defer ts.Close()

	s.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "echo")
	})

	s.state.CompareAndSwap(int32(StateHealthy), int32(StateDraining))

	resp, err := ts.Client().Get(ts.URL + "/echo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 for non-health route during drain", resp.StatusCode)
	}
}

func TestDrainWaitsForInflightRequests(t *testing.T) {
	t.Parallel()
	s := New(":0", 5*time.Second)
	ts := httptest.NewServer(s)
	defer ts.Close()

	handlerStarted := make(chan struct{})
	handlerRelease := make(chan struct{})
	s.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-handlerRelease
		fmt.Fprint(w, "done")
	})

	var reqWg sync.WaitGroup
	reqWg.Add(1)
	go func() {
		defer reqWg.Done()
		resp, err := ts.Client().Get(ts.URL + "/slow")
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-handlerStarted // inflight is now 1

	drainErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		drainErr <- s.Drain(ctx)
	}()

	// Poll until state transitions.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if s.State() != StateHealthy {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if s.State() != StateDraining {
		t.Fatalf("State() = %v, want %v after Drain called", s.State(), StateDraining)
	}

	close(handlerRelease)
	reqWg.Wait()

	if err := <-drainErr; err != nil {
		t.Fatalf("Drain() = %v, want nil", err)
	}
	if s.State() != StateStopped {
		t.Fatalf("State() = %v, want %v after Drain", s.State(), StateStopped)
	}
}

func TestDrainTimesOutOnStuckConnections(t *testing.T) {
	t.Parallel()
	s := New(":0", 50*time.Millisecond)

	// Simulate a stuck in-flight by incrementing the WaitGroup directly.
	// The matching Done is deferred so it runs after assertions.
	s.inflight.Add(1)
	defer s.inflight.Done()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Drain(ctx) //nolint:errcheck

	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("Drain returned in %v, want >= 50ms (drain timeout)", elapsed)
	}
	if s.State() != StateStopped {
		t.Fatalf("State() = %v, want %v after timeout", s.State(), StateStopped)
	}
}

func TestDrainReturnsErrAlreadyDraining(t *testing.T) {
	t.Parallel()
	s := New(":0", time.Second)
	s.state.Store(int32(StateDraining))

	err := s.Drain(context.Background())
	if !errors.Is(err, ErrAlreadyDraining) {
		t.Fatalf("err = %v, want ErrAlreadyDraining", err)
	}
}

func ExampleState_String() {
	fmt.Println(StateHealthy.String())
	fmt.Println(StateDraining.String())
	fmt.Println(StateStopped.String())
	// Output:
	// healthy
	// draining
	// stopped
}
```

Your turn: add `TestDrainReturnNilWhenNoInflight` that creates a `Server` with a 100 ms drain timeout, calls `Drain(context.Background())`, and asserts the returned error is nil and `s.State() == StateStopped`.

### Exercise 3: Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/drain"
)

func main() {
	srv := drain.New(":8080", 30*time.Second)

	srv.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond) // simulate work
		fmt.Fprintln(w, "hello")
	})

	go func() {
		log.Println("listening on :8080 (send SIGINT to drain)")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	log.Printf("signal %v received, draining (state: %v)", sig, srv.State())

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if err := srv.Drain(ctx); err != nil {
		log.Printf("drain error: %v", err)
		os.Exit(1)
	}
	log.Printf("server stopped cleanly (state: %v)", srv.State())
}
```

Run with `go run ./cmd/demo` and press Ctrl-C while a request is in flight to observe the drain sequence in the log output.

## Common Mistakes

### Health endpoint runs through the in-flight middleware

Wrong: routing `/health` through the same code path that calls `s.inflight.Add(1)`. During drain, health checks either get counted as in-flight (delaying shutdown) or are rejected with 503 before reaching the state check.

Fix: handle `/health` first in `ServeHTTP`, before the state guard and WaitGroup increment, as in the exercise. The health response reads from `atomic.Int32` directly and is always served regardless of drain state.

### CAS replaced with a mutex and if-then-set

Wrong:
```go
s.mu.Lock()
if s.state == StateHealthy {
	s.state = StateDraining
}
s.mu.Unlock()
```

What happens: the mutex prevents a data race, but neither caller returns early. Both proceed past the mutex block, call `inflight.Wait()` concurrently, then both call `s.srv.Shutdown`. The second `Shutdown` call returns `http.ErrServerClosed`.

Fix: use `atomic.Int32.CompareAndSwap` and return `ErrAlreadyDraining` immediately if the swap fails.

### Missing drain timeout

Wrong: blocking on `s.inflight.Wait()` with no timeout. A single handler that deadlocks (waiting on a database connection, a downstream RPC, or a channel that is never closed) prevents the server from ever exiting. The load balancer eventually times out on its own drain timeout and marks the deployment failed.

Fix: use a `select` with `time.NewTimer(s.timeout)` as the backstop. Size the drain timeout to be shorter than the load balancer's drain timeout.

### State set to stopped before http.Server.Shutdown returns

Wrong: storing `StateStopped` and returning before `http.Server.Shutdown` completes. If a caller checks `State() == StateStopped` as a proxy for "the server is done", they may proceed while the server still has open connections being drained by the HTTP layer.

Fix: call `s.srv.Shutdown` first, then store `StateStopped` only if `Shutdown` returns without error — or accept that `StateStopped` means "drain decision made" and document that `Drain` returning is the real signal. In the exercise, the store precedes `Shutdown` intentionally because the state machine drives request routing, not shutdown completion; the `Drain` return value is the authoritative signal.

## Verification

From `~/go-exercises/drain`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The `-race` flag is essential here: WaitGroup misuse, atomic state reads, and channel close races all surface under `-race` but not under plain `go test`.

## Summary

- Connection draining is a three-phase shutdown: stop accepting new requests, wait for in-flight handlers (with a timeout), then force-close via `http.Server.Shutdown`.
- `http.Server.Shutdown` handles graceful HTTP shutdown at the connection level; `sync.WaitGroup` tracks at the handler level.
- Use `atomic.Int32.CompareAndSwap` for state transitions to eliminate the TOCTOU race between concurrent `Drain` calls.
- The `/health` endpoint must bypass the in-flight middleware so load balancers always reach the state machine during drain.
- Size the drain timeout shorter than the load balancer's drain timeout to prevent LB-side timeouts during deployment.

## What's Next

Next: [Building a SOCKS5 Proxy](../19-building-a-socks5-proxy/19-building-a-socks5-proxy.md).

## Resources

- [net/http: Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown)
- [sync: WaitGroup](https://pkg.go.dev/sync#WaitGroup)
- [sync/atomic: Int32](https://pkg.go.dev/sync/atomic#Int32)
- [os/signal: NotifyContext](https://pkg.go.dev/os/signal#NotifyContext)
- [Kubernetes: Pod Termination and graceful shutdown](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination)
