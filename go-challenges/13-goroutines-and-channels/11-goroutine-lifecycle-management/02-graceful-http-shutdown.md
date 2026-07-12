# Exercise 2: Graceful HTTP Server Shutdown on SIGINT/SIGTERM

Every Go service has the same teardown path in `main`: start the HTTP server in a
goroutine, block until a termination signal arrives, then drain in-flight requests
within a deadline before exiting. Get it wrong and you either drop live requests
on deploy or hang forever behind one slow client. This exercise builds that path
as a testable `Run` function.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
gracefulhttp/              independent module: example.com/gracefulhttp
  go.mod
  server.go                Run(ctx, srv, ln, grace); ErrShutdownTimeout
  cmd/
    demo/
      main.go              runnable demo: serve, request, cancel, drain
  server_test.go           in-flight drain, shutdown-timeout, clean-close tests
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `Run(ctx, srv, ln, grace)` that serves in a goroutine, waits on `ctx.Done()`, then calls `srv.Shutdown` with a `grace`-bounded context; returns `nil` on clean shutdown and wraps `ErrShutdownTimeout` when the drain deadline is exceeded.
- Test: an in-flight request drains before shutdown returns; a handler slower than `grace` makes `Run` return `ErrShutdownTimeout` wrapping `context.DeadlineExceeded`; `Serve` returning `http.ErrServerClosed` is treated as the normal path.
- Verify: `go test -count=1 -race ./...`

### The teardown handshake, step by step

`http.Server` gives you two halves of the lifecycle. `srv.Serve(ln)` (or the
address-binding `srv.ListenAndServe`) runs the accept loop; it blocks until the
server stops, and on a clean stop it returns the sentinel `http.ErrServerClosed`,
*not* a real error. `srv.Shutdown(ctx)` performs the graceful stop: it closes the
listeners so no new connections are accepted, then waits for active requests to
finish — bounded by `ctx`. If `ctx` is cancelled before the requests drain,
`Shutdown` returns `ctx.Err()` (`context.DeadlineExceeded`) and leaves the
straggling connections alone.

`Run` wires these together with three moving parts:

1. It launches `srv.Serve(ln)` in a goroutine and captures its return into a
   buffered `errCh` (buffer 1, so the goroutine can always send and exit even if
   nobody is reading yet — an unbuffered channel here would leak the goroutine
   when `Serve` returns after `Run` has moved on).
2. It `select`s between `errCh` (the server failed to start, e.g. the port is
   taken) and `ctx.Done()` (a signal asked us to stop). If startup fails, return
   that error immediately. If the context fires, proceed to drain.
3. It calls `srv.Shutdown` with a fresh `context.WithTimeout(context.Background(),
   grace)` — a *new* context, because the parent is already cancelled and would
   give `Shutdown` zero time. After `Shutdown` returns, it reads `errCh` to reap
   the now-returning `Serve` goroutine, then reports: `nil` on a clean drain, or
   `ErrShutdownTimeout` wrapping the deadline error on a timeout.

Note the wrapping: `fmt.Errorf("%w: %w", ErrShutdownTimeout, err)` wraps *two*
errors, so callers can match either the domain sentinel `ErrShutdownTimeout` or
the underlying `context.DeadlineExceeded` with `errors.Is`.

Create `server.go`:

```go
package gracefulhttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ErrShutdownTimeout is returned when in-flight requests do not drain within
// the grace period passed to Run.
var ErrShutdownTimeout = errors.New("graceful shutdown timed out")

// Run serves srv on ln until ctx is cancelled, then drains in-flight requests
// within grace. It returns nil on a clean shutdown, the server's startup error
// if Serve fails immediately, or ErrShutdownTimeout (wrapping the deadline
// error) if the drain does not complete in time.
func Run(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case err := <-errCh:
		// Serve returned before any signal: clean close or a real startup error.
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()

	err := srv.Shutdown(shutdownCtx)
	<-errCh // reap the Serve goroutine, which now returns http.ErrServerClosed
	if err != nil {
		return fmt.Errorf("%w: %w", ErrShutdownTimeout, err)
	}
	return nil
}
```

### Wiring it to real signals

In production `Run` is fed a context derived from the OS signals that orchestrators
send on deploy or scale-down — `SIGINT` (Ctrl-C) and `SIGTERM` (Kubernetes,
systemd). `signal.NotifyContext` builds exactly that context: it is cancelled the
first time one of the named signals arrives. The real `main` looks like this
(shown for reference; the demo below uses a manually cancelled context so its
output is deterministic):

```go
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", ":8080")
	if err != nil {
		log.Fatal(err)
	}
	if err := gracefulhttp.Run(ctx, srv, ln, 15*time.Second); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
}
```

### The runnable demo

The demo makes the teardown observable without needing an actual signal: it
serves on an ephemeral port, issues one request, then cancels the context to
trigger the drain, and reports the outcome.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"example.com/gracefulhttp"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	srv := &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- gracefulhttp.Run(ctx, srv, ln, 5*time.Second) }()

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Println("request:", err)
		return
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("GET /healthz -> %d %s\n", resp.StatusCode, body)

	cancel() // stand in for SIGTERM
	if err := <-runErr; err != nil {
		fmt.Println("shutdown error:", err)
		return
	}
	fmt.Println("server shut down cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /healthz -> 200 ok
server shut down cleanly
```

### Tests

`TestDrainsInFlightRequest` is the property that matters most on deploy: a request
that is already being handled when shutdown starts must *complete*, not get cut
off. The handler signals that it has started (closing a `started` channel), then
sleeps well within the generous grace period; the test launches the request,
waits for `started`, cancels the context, and asserts both that `Run` returns
`nil` and that the request came back `200`. `TestShutdownTimeout` is the opposite
corner: a handler that sleeps past the short grace period forces `Shutdown` to
give up, and the test asserts `Run` returns an error that `errors.Is` both
`ErrShutdownTimeout` and `context.DeadlineExceeded`. `TestCleanCloseWithoutTraffic`
checks the no-traffic path returns `nil`, proving `http.ErrServerClosed` is
handled as success, not failure.

Create `server_test.go`:

```go
package gracefulhttp

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func newTestServer(t *testing.T, h http.Handler) (net.Listener, *http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln, &http.Server{Handler: h}, "http://" + ln.Addr().String()
}

func TestDrainsInFlightRequest(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		io.WriteString(w, "done")
	})
	ln, srv, base := newTestServer(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, srv, ln, 5*time.Second) }()

	type result struct {
		code int
		body string
	}
	reqCh := make(chan result, 1)
	go func() {
		resp, err := http.Get(base + "/slow")
		if err != nil {
			reqCh <- result{code: -1}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		reqCh <- result{code: resp.StatusCode, body: string(b)}
	}()

	<-started // request is now in-flight
	cancel()  // begin graceful shutdown

	if err := <-runErr; err != nil {
		t.Fatalf("Run returned %v, want nil (clean drain)", err)
	}
	got := <-reqCh
	if got.code != http.StatusOK || got.body != "done" {
		t.Fatalf("in-flight request = %d %q, want 200 %q", got.code, got.body, "done")
	}
}

func TestShutdownTimeout(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/stuck", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(500 * time.Millisecond)
	})
	ln, srv, base := newTestServer(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, srv, ln, 20*time.Millisecond) }()

	go func() { http.Get(base + "/stuck") }() //nolint:errcheck // request is cut off by shutdown

	<-started
	cancel()

	err := <-runErr
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("Run err = %v, want ErrShutdownTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

func TestCleanCloseWithoutTraffic(t *testing.T) {
	t.Parallel()

	ln, srv, _ := newTestServer(t, http.NewServeMux())
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- Run(ctx, srv, ln, time.Second) }()

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
}
```

## Review

The server teardown is correct when three things line up. A request in flight at
shutdown drains to completion, which `TestDrainsInFlightRequest` proves by reading
a `200` back after cancellation. A drain that overruns the grace period is
reported, not silently swallowed — `Run` wraps `ErrShutdownTimeout` and the
underlying `context.DeadlineExceeded`, both matchable with `errors.Is`. And the
clean close returns `nil`, because `http.ErrServerClosed` from `Serve` is the
success sentinel, not an error. The two traps this exercise inoculates against are
calling `Shutdown` with no deadline (which hangs the whole process on one slow
client) and logging `ErrServerClosed` as a failure on every clean restart. Run
`go test -count=1 -race ./...`; the `-race` flag matters because `Run`, the
`Serve` goroutine, and the request goroutines all touch the server concurrently.

## Resources

- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — graceful shutdown semantics and the context deadline.
- [`http.ErrServerClosed`](https://pkg.go.dev/net/http#pkg-variables) — the sentinel `Serve`/`ListenAndServe` return on a clean stop.
- [`signal.NotifyContext`](https://pkg.go.dev/os/signal#NotifyContext) — deriving a cancellable context from `SIGINT`/`SIGTERM`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-start-stop-worker.md](01-start-stop-worker.md) | Next: [03-errgroup-bounded-fanout.md](03-errgroup-bounded-fanout.md)
