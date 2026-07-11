# Exercise 2: Draining In-Flight HTTP Requests With Server.Shutdown

Stopping the HTTP listener is the first phase of every graceful shutdown, and the
one with the sharpest edges. This module builds an `HTTPService` wrapper that runs
`ListenAndServe`, treats `http.ErrServerClosed` as the normal stop signal, and
drains in-flight requests with a fresh, bounded shutdown context — the exact shape
production `main()` code needs.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
httpdrain/                 module example.com/httpdrain
  go.mod                   go 1.26
  service.go               type HTTPService; NewHTTPService, Start, Shutdown
  cmd/
    demo/
      main.go              serve, fire an in-flight request, drain it
  service_test.go          drain-completes, drain-times-out, ErrServerClosed swallowed
```

Files: `service.go`, `cmd/demo/main.go`, `service_test.go`.
Implement: `NewHTTPService(name, *http.Server)` with `Start() error` (swallows `http.ErrServerClosed`) and `Shutdown(timeout) error` (fresh `context.WithTimeout` rooted at `Background`).
Test: a request slower than the drain budget still completes with 200; a handler that ignores its context makes `Shutdown` return `context.DeadlineExceeded`; a clean stop is not an error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/httpdrain/cmd/demo
cd ~/go-exercises/httpdrain
go mod init example.com/httpdrain
```

## Why the shutdown context must be fresh

`ListenAndServe` blocks until the server stops, then returns an error. When
`Shutdown` is called, that error is `http.ErrServerClosed` — the normal signal
that a graceful stop began, not a failure. `Start` runs `ListenAndServe` in a
goroutine and treats `http.ErrServerClosed` as success; any *other* error (a bind
failure on a taken port, for instance) is fatal and must surface. Swallowing the
sentinel is not optional: if a clean stop looks like a crash, every deploy logs a
false error and, in a supervised setup, a clean stop would cancel the whole
service.

`Shutdown` is where the drain budget lives, and where the single most common
production bug hides. `server.Shutdown(ctx)` stops accepting new connections and
waits for in-flight handlers to return; if `ctx` expires first it force-closes the
lingering connections and returns `context.DeadlineExceeded`. The context passed
in must be built from `context.WithTimeout(context.Background(), timeout)` — a
fresh timer rooted at `Background`. It must not derive from the already-cancelled
root context that triggered shutdown, because that context is `Done()` the instant
shutdown begins: derive from it and the drain gets zero milliseconds and
force-closes every connection at once, which is the opposite of graceful. Rooting
the drain at `Background` is what gives in-flight requests their promised window.

One honest caveat the tests make concrete: `Shutdown` waits for handlers, it does
not cancel them. A handler that ignores `r.Context()` and blocks past the budget
is force-closed at the connection level and `Shutdown` returns
`context.DeadlineExceeded`, but the goroutine keeps running until it returns on
its own. `Shutdown` bounds the wait; it does not reach inside a handler.

Create `service.go`:

```go
package httpdrain

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// HTTPService wraps an http.Server with named lifecycle methods so a shutdown
// coordinator can start it and drain it uniformly with other components.
type HTTPService struct {
	name   string
	server *http.Server
}

// NewHTTPService returns an HTTPService wrapping server under name.
func NewHTTPService(name string, server *http.Server) *HTTPService {
	return &HTTPService{name: name, server: server}
}

// Name returns the service name.
func (s *HTTPService) Name() string { return s.name }

// Start runs ListenAndServe in a goroutine and returns a channel that reports
// the terminal error. http.ErrServerClosed is the normal signal that Shutdown
// was called and is normalized to nil; any other error is fatal.
func (s *HTTPService) Start() <-chan error {
	errCh := make(chan error, 1)
	go func() {
		err := s.server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()
	return errCh
}

// Serve runs the server on an existing listener, which lets a caller learn the
// bound address (useful on port 0). Like Start, it normalizes
// http.ErrServerClosed to nil so a clean stop is not reported as an error.
func (s *HTTPService) Serve(ln net.Listener) error {
	err := s.server.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and waits up to timeout for in-flight
// requests to drain. The context is rooted at Background, not the cancelled
// root context, so the drain gets its full budget.
func (s *HTTPService) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := s.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("http service %s shutdown: %w", s.name, err)
	}
	return nil
}
```

## The runnable demo

The demo serves a handler that sleeps 200ms honoring its request context, fires
one request in a goroutine, then drains with a generous 2s budget so the in-flight
request completes with 200 before the server stops. It serves on a `127.0.0.1:0`
listener via the exported `Serve` so it can print the bound address.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"example.com/httpdrain"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(200 * time.Millisecond):
			fmt.Fprintln(w, "done")
		case <-r.Context().Done():
			http.Error(w, "cancelled", http.StatusGatewayTimeout)
		}
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	addr := ln.Addr().String()
	svc := httpdrain.NewHTTPService("api", &http.Server{Handler: mux})

	errCh := make(chan error, 1)
	go func() { errCh <- svc.Serve(ln) }()

	respCh := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/work")
		if err != nil {
			respCh <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		respCh <- resp.StatusCode
	}()

	time.Sleep(20 * time.Millisecond)
	if err := svc.Shutdown(2 * time.Second); err != nil {
		fmt.Println("shutdown:", err)
		return
	}
	fmt.Println("in-flight request status:", <-respCh)
	<-errCh
	fmt.Println("server stopped cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-flight request status: 200
server stopped cleanly
```

## Tests

The tests use `httptest`-style listeners on `127.0.0.1:0`. `TestDrainCompletes`
fires a request slower than nothing-in-particular and drains with a budget longer
than the handler: `Shutdown` returns `nil` and the request finishes 200.
`TestDrainTimesOut` uses a handler that ignores its context and drains with a
budget shorter than the handler: `Shutdown` returns `context.DeadlineExceeded`
(asserted via `errors.Is`). `TestCleanStartStop` proves a clean `Serve` returns
`nil`, not `http.ErrServerClosed`.

Create `service_test.go`:

```go
package httpdrain

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// serve starts an HTTPService on a fresh loopback listener and returns its
// address and the terminal-error channel from Serve.
func serve(t *testing.T, svc *HTTPService) (string, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- svc.Serve(ln) }()
	return ln.Addr().String(), errCh
}

func TestDrainCompletes(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	})
	svc := NewHTTPService("api", &http.Server{Handler: mux})
	addr, errCh := serve(t, svc)

	statusCh := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + addr + "/")
		if err != nil {
			statusCh <- 0
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		statusCh <- resp.StatusCode
	}()

	time.Sleep(20 * time.Millisecond) // let the request reach the handler
	if err := svc.Shutdown(2 * time.Second); err != nil {
		t.Fatalf("Shutdown with generous budget: %v; want nil", err)
	}
	if got := <-statusCh; got != http.StatusOK {
		t.Fatalf("in-flight request status = %d, want 200", got)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Serve returned %v; want nil after clean stop", err)
	}
}

func TestDrainTimesOut(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		<-block // ignore r.Context(); block until released
		w.WriteHeader(http.StatusOK)
	})
	svc := NewHTTPService("api", &http.Server{Handler: mux})
	addr, errCh := serve(t, svc)

	go http.Get("http://" + addr + "/") //nolint:errcheck // fire-and-forget
	time.Sleep(20 * time.Millisecond)

	err := svc.Shutdown(50 * time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want context.DeadlineExceeded", err)
	}
	close(block) // release the wedged handler so the goroutine can exit
	<-errCh
}

func TestCleanStartStop(t *testing.T) {
	t.Parallel()

	svc := NewHTTPService("api", &http.Server{Handler: http.NewServeMux()})
	_, errCh := serve(t, svc)
	time.Sleep(10 * time.Millisecond)

	if err := svc.Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown: %v; want nil", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("Serve err = %v; want nil (ErrServerClosed normalized)", err)
	}
}

func ExampleHTTPService_Name() {
	svc := NewHTTPService("payments", &http.Server{})
	println(svc.Name() == "payments")
	// Output:
}
```

## Review

The service is correct when a clean stop is indistinguishable from success and a
stuck client is bounded. `Start`/`Serve` normalizing `http.ErrServerClosed` to
`nil` is what makes a graceful stop not look like a crash — `TestCleanStartStop`
pins that. The drain budget rooted at `context.Background()` is what gives
in-flight requests their window: `TestDrainCompletes` proves a slow-but-honest
request survives, `TestDrainTimesOut` proves a client that ignores cancellation is
force-closed with `context.DeadlineExceeded` rather than hanging forever. The
mistakes to avoid: deriving the shutdown context from the cancelled root (zero
budget), passing `context.Background()` with no timeout (unbounded hang), and
treating `http.ErrServerClosed` as fatal. Run `go test -race` to confirm the serve
goroutine and the request goroutine are clean.

## Resources

- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — the drain semantics, force-close on deadline, and the note that it waits rather than cancels.
- [http.ErrServerClosed](https://pkg.go.dev/net/http#pkg-variables) — the sentinel returned by ListenAndServe/Serve after Shutdown.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — the fresh, bounded context the drain must use.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-worker-lifecycle-cancellation.md](01-worker-lifecycle-cancellation.md) | Next: [03-app-reverse-order-orchestrator.md](03-app-reverse-order-orchestrator.md)
