# Exercise 4: Wiring the Framework to OS Signals and an HTTP Server

This is the module every service's `main()` collapses into: `signal.NotifyContext`
as the root context, an HTTP server whose `Start` launches `Serve` in a goroutine
and whose `Stop` calls `Shutdown` to drain, and a non-critical ticker worker
alongside it. It ties the framework to the real world — a `SIGTERM` from the
orchestrator becomes a clean, draining shutdown.

## What you'll build

```text
httpsvc/                      independent module: example.com/httpsvc
  go.mod                      go 1.26
  svcframe.go                 core App + RunService + HTTPService (Serve/Shutdown wrapper)
  svcframe_test.go            in-flight request drains; Serve returns ErrServerClosed
  cmd/
    demo/
      main.go                 signal.NotifyContext main; self-signals; prints health + shutdown
```

Files: `svcframe.go`, `cmd/demo/main.go`, `svcframe_test.go`.
Implement: `HTTPService` whose `Start` runs `server.Serve(listener)` in a goroutine (tolerating `ErrServerClosed`) and whose `Stop` calls `server.Shutdown(ctx)`; a signal-driven `main()`.
Test: bind a listener on `127.0.0.1:0`, start the app, fire an in-flight request, cancel the root context, assert the request drained (`200`) and `Serve` returned `http.ErrServerClosed`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/14-building-a-context-aware-service-framework/04-signal-driven-graceful-http-shutdown/cmd/demo
cd go-solutions/14-select-and-context/14-building-a-context-aware-service-framework/04-signal-driven-graceful-http-shutdown
```

### The HTTP service shape

An `http.Server` maps onto the `Service` contract almost perfectly, but the two
halves must be split correctly. `Start` must not block, yet `server.Serve`
blocks until the server stops — so `Start` launches `Serve` in a goroutine and
returns. `Serve` returns `http.ErrServerClosed` on a clean shutdown, which is
*not* an error condition; the goroutine captures the return value in a buffered
channel so a test (or a supervising caller) can distinguish a clean stop from a
real bind/accept failure.

`Stop` calls `server.Shutdown(ctx)`, which stops accepting new connections and
waits for in-flight requests to finish, bounded by the context — and this is
exactly why the framework hands `Stop` a *fresh* budget rather than the cancelled
root context: `Shutdown` needs real time to drain.

Binding the listener explicitly (rather than letting `ListenAndServe` bind
internally) has two payoffs: a bind failure surfaces synchronously from `Start`
as a real error (so `address already in use` aborts boot instead of vanishing
into a goroutine), and the caller can read the actual address — essential when
binding `:0` to get an OS-assigned port in a test. `NewHTTPService` accepts an
optional pre-bound listener for exactly that; if none is given, `Start` binds
`server.Addr` itself.

Create `svcframe.go`:

```go
package svcframe

import (
	"context"
	"log/slog"
	"net"
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

// Config holds per-service lifecycle options.
type Config struct {
	Critical    bool
	StopTimeout time.Duration
}

type entry struct {
	svc    Service
	cfg    Config
	cancel context.CancelFunc
}

// App manages the lifecycle of registered services.
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

// Run starts services in order, blocks on ctx, stops in reverse.
func (a *App) Run(ctx context.Context) error {
	a.mu.Lock()
	entries := make([]entry, len(a.entries))
	copy(entries, a.entries)
	a.mu.Unlock()

	var started []entry
	for i := range entries {
		e := entries[i]
		svcCtx, cancel := context.WithCancel(ctx)
		e.cancel = cancel
		if err := e.svc.Start(svcCtx); err != nil {
			cancel()
			if e.cfg.Critical {
				a.stopAll(started)
				return err
			}
			a.logger.Warn("non-critical start failed", "name", e.svc.Name(), "err", err)
			continue
		}
		started = append(started, e)
	}

	<-ctx.Done()
	a.stopAll(started)
	return nil
}

func (a *App) stopAll(started []entry) {
	for i := len(started) - 1; i >= 0; i-- {
		e := started[i]
		e.cancel()
		stopCtx, cancel := context.WithTimeout(context.Background(), e.cfg.StopTimeout)
		if err := e.svc.Stop(stopCtx); err != nil {
			a.logger.Warn("stop error", "name", e.svc.Name(), "err", err)
		}
		cancel()
	}
}

// RunService adapts start/stop functions into a Service.
type RunService struct {
	name  string
	start func(ctx context.Context) error
	stop  func(ctx context.Context) error
}

// NewRunService builds a Service from start and stop functions; either may be nil.
func NewRunService(name string, start, stop func(ctx context.Context) error) *RunService {
	return &RunService{name: name, start: start, stop: stop}
}

func (r *RunService) Name() string { return r.name }

func (r *RunService) Start(ctx context.Context) error {
	if r.start == nil {
		return nil
	}
	return r.start(ctx)
}

func (r *RunService) Stop(ctx context.Context) error {
	if r.stop == nil {
		return nil
	}
	return r.stop(ctx)
}

// HTTPService adapts an *http.Server to the Service contract. Start launches
// Serve in a goroutine (a clean shutdown surfaces as http.ErrServerClosed, which
// is not treated as a failure); Stop drains via Shutdown.
type HTTPService struct {
	name   string
	server *http.Server
	ln     net.Listener
	errc   chan error
}

// NewHTTPService wraps server as a Service named name. If ln is non-nil the
// server serves it (useful for binding 127.0.0.1:0 in tests); otherwise Start
// binds server.Addr itself.
func NewHTTPService(name string, server *http.Server, ln net.Listener) *HTTPService {
	return &HTTPService{name: name, server: server, ln: ln, errc: make(chan error, 1)}
}

func (s *HTTPService) Name() string { return s.name }

func (s *HTTPService) Start(context.Context) error {
	if s.ln == nil {
		ln, err := net.Listen("tcp", s.server.Addr)
		if err != nil {
			return err // a bind failure aborts boot synchronously
		}
		s.ln = ln
	}
	go func() { s.errc <- s.server.Serve(s.ln) }()
	return nil
}

func (s *HTTPService) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// Addr reports the address the listener is bound to.
func (s *HTTPService) Addr() string { return s.ln.Addr().String() }

// ServeErr blocks until the Serve goroutine returns and reports its error. On a
// clean shutdown this is http.ErrServerClosed.
func (s *HTTPService) ServeErr() error { return <-s.errc }
```

### The runnable demo

A signal-driven `main()` normally blocks until you press Ctrl+C, which makes it
useless as a reproducible demo. So this one still uses `signal.NotifyContext`
(the real production wiring) but a helper goroutine self-sends `SIGTERM` after
performing one health check, so the whole lifecycle runs and exits
deterministically. The framework's own logs go to a discard logger; the demo
prints its own lines with `fmt` so the output is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/httpsvc"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("bind:", err)
		return
	}
	httpSvc := svcframe.NewHTTPService("http", &http.Server{Handler: mux}, ln)

	app := svcframe.New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	app.Register(httpSvc, svcframe.Config{Critical: true, StopTimeout: 5 * time.Second})
	app.Register(svcframe.NewRunService("metrics",
		func(ctx context.Context) error {
			go func() {
				t := time.NewTicker(time.Hour)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
					}
				}
			}()
			return nil
		},
		nil,
	), svcframe.Config{Critical: false, StopTimeout: 2 * time.Second})

	go func() {
		time.Sleep(100 * time.Millisecond)
		resp, err := http.Get("http://" + httpSvc.Addr() + "/health")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Printf("health: %s\n", b)
		}
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(syscall.SIGTERM) // stand in for the orchestrator's SIGTERM
	}()

	fmt.Println("listening")
	if err := app.Run(ctx); err != nil {
		fmt.Println("run error:", err)
		return
	}
	fmt.Println("clean shutdown")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
listening
health: ok
clean shutdown
```

### Tests

The test binds a real listener on `127.0.0.1:0`, so it needs no fixed port and
never collides with a running service. A handler blocks briefly to model an
in-flight request; the test fires that request, waits for it to be in the
handler, cancels the root context to trigger shutdown, and asserts two things:
the in-flight request completed with `200` (it was drained, not dropped), and the
`Serve` goroutine returned `http.ErrServerClosed` (a clean stop, not a hard
error).

Create `svcframe_test.go`:

```go
package svcframe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestGracefulHTTPShutdownDrainsInFlight(t *testing.T) {
	t.Parallel()

	inHandler := make(chan struct{})
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(inHandler)
		<-release // stay in-flight until the test releases us
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "done")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSvc := NewHTTPService("http", &http.Server{Handler: mux}, ln)

	app := New(testLogger())
	app.Register(httpSvc, Config{Critical: true, StopTimeout: 2 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- app.Run(ctx) }()

	// Fire an in-flight request.
	type result struct {
		code int
		err  error
	}
	reqDone := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + httpSvc.Addr() + "/slow")
		if err != nil {
			reqDone <- result{err: err}
			return
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		reqDone <- result{code: resp.StatusCode}
	}()

	<-inHandler // the request is now inside the handler
	cancel()    // trigger shutdown while the request is in-flight

	// Shutdown must wait for the in-flight request, so release it now.
	time.Sleep(20 * time.Millisecond)
	close(release)

	select {
	case r := <-reqDone:
		if r.err != nil {
			t.Fatalf("in-flight request failed: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("in-flight request code = %d, want 200", r.code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request never completed")
	}

	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: err = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return")
	}

	if err := httpSvc.ServeErr(); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v, want http.ErrServerClosed", err)
	}
}
```

## Review

The test proves the two properties that make an HTTP service safe to deploy on a
rolling update. First, the in-flight request returns `200`: `Shutdown` blocked
until the handler finished, so the request was drained, not dropped — this is the
whole reason `Stop` gets a real budget rather than the cancelled root context.
Second, `Serve` returned `http.ErrServerClosed`, which `Start`'s goroutine and
any supervisor must treat as a clean stop, never as a crash to restart. The
classic mistake is checking `if err := server.Serve(ln); err != nil` and logging
it as a fatal error on every clean shutdown; guard with
`errors.Is(err, http.ErrServerClosed)`. Run `go test -race`: the handler's
`inHandler`/`release` channels and the buffered `errc` are what keep the drain
handshake race-free.

## Resources

- [os/signal.NotifyContext](https://pkg.go.dev/os/signal#NotifyContext) — turning `SIGINT`/`SIGTERM` into a cancellable root context.
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — graceful drain semantics and its context bound.
- [net/http.ErrServerClosed](https://pkg.go.dev/net/http#ErrServerClosed) — the sentinel a clean `Serve`/`ListenAndServe` returns.
- [net.Listen](https://pkg.go.dev/net#Listen) — binding `127.0.0.1:0` for an OS-assigned test port.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-reverse-order-stop-with-budget.md](03-reverse-order-stop-with-budget.md) | Next: [05-readiness-and-health-aggregation.md](05-readiness-and-health-aggregation.md)
