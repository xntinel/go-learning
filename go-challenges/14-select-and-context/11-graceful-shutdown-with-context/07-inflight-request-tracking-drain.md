# Exercise 7: Counting In-Flight Requests To Verify A Clean Drain

`Server.Shutdown` returning `nil` does not prove every handler finished — hijacked
connections, streaming responses, and background goroutines a handler spawned can
outlive it. A production service verifies drain completeness by *observing* an
in-flight gauge reach zero. This module instruments an `http.Server` with
`BaseContext`, `ConnContext`, and counting middleware, exposing `InFlight()` and a
`WaitIdle(ctx)` that blocks until the gauge is zero — and it shows the subtle truth
that `Shutdown` waits for handlers but does not cancel their contexts.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
inflight/                  module example.com/inflight
  go.mod                   go 1.26
  server.go                Server with in-flight gauge, InFlight, WaitIdle, CancelInFlight
  cmd/
    demo/
      main.go              load N requests, show gauge, drain, show gauge zero
  server_test.go           gauge reaches N then zero; ignored-ctx leaves residual; BaseContext cancel cuts handlers
```

Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
Implement: a `Server` wrapping `http.Server` with counting middleware, `InFlight() int64`, `WaitIdle(ctx) error`, `Shutdown(timeout) error`, and `CancelInFlight()` that cancels the `BaseContext`.
Test: N concurrent requests drive the gauge to N then to zero on a clean drain; a handler ignoring `r.Context()` leaves a residual after a timed-out `Shutdown`; cancelling the base context cuts handlers that `Shutdown` would not.
Verify: `go test -count=1 -race ./...`

## Why a gauge, and what BaseContext buys you

Counting middleware wraps the handler: increment an `atomic.Int64` on entry,
`defer` the decrement on exit. `InFlight()` reads it. That gauge is the ground
truth of how many handlers are actually executing right now — the number
`Shutdown`'s return value only *implies*. `WaitIdle(ctx)` polls the gauge until it
reads zero or `ctx` expires, returning a residual-count error on timeout. A real
drain asserts `WaitIdle` succeeds *after* `Shutdown` returns; if the gauge is still
non-zero, work leaked past the drain and the "clean shutdown" was a lie.

`BaseContext` and `ConnContext` are the two hooks that let you own the context
tree beneath every request. `BaseContext(net.Listener) context.Context` supplies
the root context for all connections; `ConnContext(ctx, net.Conn) context.Context`
derives a per-connection context from it. Every `r.Context()` descends from that
base. This exposes the senior gotcha the concepts stress: `Shutdown` waits for
in-flight handlers, it does *not* cancel `r.Context()`. A long-poll handler that
only watches its request context will block until it finishes naturally or
`Shutdown`'s deadline force-closes the connection — but the handler goroutine keeps
running, and the gauge stays up. To *actively* cut such handlers you cancel the
base context yourself. `CancelInFlight()` does exactly that, and a handler honoring
`r.Context()` then returns immediately — something `Shutdown` alone cannot make it
do. That is the mechanism most engineers do not know exists.

Create `server.go`:

```go
package inflight

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Server wraps an http.Server with an in-flight request gauge and a base context
// it can cancel to actively cut handlers that honor r.Context().
type Server struct {
	gauge      atomic.Int64
	srv        *http.Server
	baseCancel context.CancelFunc
}

// New builds a Server whose handler is wrapped in counting middleware and whose
// request contexts all descend from a base context New controls.
func New(h http.Handler) *Server {
	s := &Server{}
	base, cancel := context.WithCancel(context.Background())
	s.baseCancel = cancel
	s.srv = &http.Server{
		Handler:     s.instrument(h),
		BaseContext: func(net.Listener) context.Context { return base },
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context { return ctx },
	}
	return s
}

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.gauge.Add(1)
		defer s.gauge.Add(-1)
		next.ServeHTTP(w, r)
	})
}

// InFlight reports how many handlers are executing right now.
func (s *Server) InFlight() int64 { return s.gauge.Load() }

// Serve runs the server on ln, normalizing http.ErrServerClosed to nil.
func (s *Server) Serve(ln net.Listener) error {
	err := s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting connections and waits up to timeout for handlers to
// return. It does NOT cancel in-flight request contexts; use CancelInFlight for
// that.
func (s *Server) Shutdown(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// CancelInFlight cancels the base context, so every request context derived from
// it is cancelled. A handler honoring r.Context() returns immediately, which
// Shutdown alone cannot force.
func (s *Server) CancelInFlight() { s.baseCancel() }

// WaitIdle blocks until the in-flight gauge reads zero or ctx expires. On
// timeout it returns an error naming the residual count, proving the drain was
// incomplete.
func (s *Server) WaitIdle(ctx context.Context) error {
	t := time.NewTicker(time.Millisecond)
	defer t.Stop()
	for {
		if s.gauge.Load() == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("drain incomplete: %d requests still in flight: %w", s.gauge.Load(), ctx.Err())
		case <-t.C:
		}
	}
}
```

## The runnable demo

The demo fires three requests that block on a release channel, shows the gauge at
3, releases them, drains, and shows the gauge at 0.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"example.com/inflight"
)

func main() {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	srv := inflight.New(mux)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	addr := ln.Addr().String()
	go srv.Serve(ln) //nolint:errcheck

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + addr + "/")
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	for range 3 {
		<-started
	}
	fmt.Println("in-flight during load:", srv.InFlight())

	close(release)
	if err := srv.Shutdown(2 * time.Second); err != nil {
		fmt.Println("shutdown:", err)
		return
	}
	wg.Wait()
	_ = srv.WaitIdle(context.Background())
	fmt.Println("in-flight after drain:", srv.InFlight())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in-flight during load: 3
in-flight after drain: 0
```

## Tests

`TestDrainToZero` fires N requests that block on a release channel, asserts the
gauge reaches N, releases them, drains, and asserts the gauge is zero and
`WaitIdle` returns `nil`. `TestIgnoredContextLeavesResidual` fires a handler that
ignores `r.Context()` and blocks past a short `Shutdown` timeout: `Shutdown`
returns `context.DeadlineExceeded` and `WaitIdle` on a short context reports a
non-zero residual. `TestCancelInFlightCutsHandlers` proves the `BaseContext`
mechanism: a handler honoring `r.Context()` is cut by `CancelInFlight`, which
`Shutdown` alone would not do.

Create `server_test.go`:

```go
package inflight

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func serve(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		if err := s.Serve(ln); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	return ln.Addr().String()
}

func TestDrainToZero(t *testing.T) {
	t.Parallel()

	const n = 4
	started := make(chan struct{}, n)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	})
	srv := New(mux)
	addr := serve(t, srv)

	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get("http://" + addr + "/")
			if err != nil {
				return
			}
			codes[i] = resp.StatusCode
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}

	for range n {
		<-started
	}
	if got := srv.InFlight(); got != n {
		t.Fatalf("InFlight during load = %d, want %d", got, n)
	}

	close(release)
	if err := srv.Shutdown(2 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v; want nil", err)
	}
	wg.Wait()

	if err := srv.WaitIdle(context.Background()); err != nil {
		t.Fatalf("WaitIdle: %v; want nil", err)
	}
	if got := srv.InFlight(); got != 0 {
		t.Fatalf("InFlight after drain = %d, want 0", got)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, c)
		}
	}
}

func TestIgnoredContextLeavesResidual(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-release // ignores r.Context(); blocks past the Shutdown budget
		w.WriteHeader(http.StatusOK)
	})
	srv := New(mux)
	addr := serve(t, srv)

	go http.Get("http://" + addr + "/") //nolint:errcheck
	<-started

	err := srv.Shutdown(50 * time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown err = %v, want context.DeadlineExceeded", err)
	}

	wctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := srv.WaitIdle(wctx); err == nil {
		t.Fatal("WaitIdle = nil, want a residual error (handler still in flight)")
	}
	if got := srv.InFlight(); got == 0 {
		t.Fatal("InFlight = 0, want non-zero residual")
	}

	close(release) // let the wedged handler goroutine exit
}

func TestCancelInFlightCutsHandlers(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done() // honors the request context
		w.WriteHeader(http.StatusGatewayTimeout)
	})
	srv := New(mux)
	addr := serve(t, srv)

	go http.Get("http://" + addr + "/") //nolint:errcheck
	<-started
	if got := srv.InFlight(); got != 1 {
		t.Fatalf("InFlight = %d, want 1", got)
	}

	// Shutdown would only wait; CancelInFlight actively cuts the handler.
	srv.CancelInFlight()

	wctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.WaitIdle(wctx); err != nil {
		t.Fatalf("WaitIdle after CancelInFlight: %v; want nil", err)
	}
	_ = srv.Shutdown(time.Second)
}

func ExampleServer_InFlight() {
	srv := New(http.NewServeMux())
	println(srv.InFlight() == 0)
	// Output:
}
```

## Review

The gauge is what turns "Shutdown returned" into "the drain is provably complete."
`TestDrainToZero` shows the gauge rise to N under load and fall to zero after a
clean drain, with every request 200. `TestIgnoredContextLeavesResidual` is the
negative proof: a handler that ignores its context is force-closed at the
connection but keeps running, so `WaitIdle` correctly reports a residual — exactly
the leak that trusting `Shutdown`'s return would hide.
`TestCancelInFlightCutsHandlers` demonstrates the `BaseContext` lever that
`Shutdown` does not pull. The mistakes to avoid: trusting `Shutdown`'s `nil` as
proof of a clean drain, and assuming `Shutdown` cancels `r.Context()` — it waits,
it does not cut. Run `go test -race`; the gauge is shared across every handler
goroutine and the drain, so the atomic is load-bearing.

## Resources

- [net/http Server.BaseContext and ConnContext](https://pkg.go.dev/net/http#Server) — owning the context tree beneath every request.
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown) — waits for handlers but does not cancel their contexts.
- [sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64) — the race-safe in-flight gauge.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-phase-timeout-budget-and-exit-code.md](06-phase-timeout-budget-and-exit-code.md) | Next: [08-kubernetes-prestop-readiness-drain.md](08-kubernetes-prestop-readiness-drain.md)
