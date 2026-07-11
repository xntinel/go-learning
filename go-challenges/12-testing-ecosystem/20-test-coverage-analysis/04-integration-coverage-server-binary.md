# Exercise 4: Collect coverage from a running server binary (GOCOVERDIR)

Unit-test coverage cannot see code exercised only by driving a compiled server as
a black box. Go 1.20 added the mechanism for that: `go build -cover` produces an
instrumented binary that writes raw coverage data to `GOCOVERDIR` as it exits,
and `go tool covdata` turns that data into percentages or a standard profile. This
module builds a small HTTP server with a graceful-shutdown path, drives it over
HTTP, and walks the build-run-convert workflow — the way you measure coverage of
an end-to-end acceptance test.

This module is fully self-contained: its own `go mod init`, a demo that runs the
server in-process, and tests that exercise the handlers and the shutdown path.

## What you'll build

```text
apisrv/                    independent module: example.com/apisrv
  go.mod
  server.go                Server: New, Addr, Run(ctx) with graceful Shutdown
  cmd/
    demo/
      main.go              signal-aware main: serve, self-check, graceful stop
  server_test.go           handler tests + a graceful-shutdown test
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: a `Server` that binds a listener, serves `GET /healthz` and `POST /echo`, and a `Run(ctx)` that serves until the context is cancelled, then calls `http.Server.Shutdown` with a timeout.
- Test: handler tests via the mux and a test that starts `Run`, drives it with a real client, cancels the context, and asserts a clean shutdown.
- Verify: `go test -count=1 -race ./...`, then the GOCOVERDIR workflow below.

Set up the module:

```bash
mkdir -p ~/go-exercises/apisrv/cmd/demo
cd ~/go-exercises/apisrv
go mod init example.com/apisrv
```

### Why graceful shutdown is a prerequisite for integration coverage

An instrumented binary flushes its coverage counters to `GOCOVERDIR` in a runtime
hook that fires as the process exits *normally* — when `main` returns or the
program calls `os.Exit`. A process killed with `SIGKILL` never runs that hook and
writes nothing. So integration coverage and graceful shutdown are the same
requirement viewed twice: to measure what an e2e test exercised, the server must
receive a stop signal, drain in-flight requests via `http.Server.Shutdown`, and
let `main` return so the runtime can write the data. A server that only dies when
force-killed cannot be coverage-measured end to end.

The `Server` here binds its listener in `New` (so the address is known before
serving and there is no readiness race), serves on it in a goroutine, and blocks
in `Run` on a `select` between the serve error and `ctx.Done()`. When the context
is cancelled — by a signal in production, or by the test cancelling it — `Run`
calls `Shutdown` with a bounded timeout so a slow client cannot hang the exit
forever. That is the exact path a `-cover` binary needs to flush.

Create `server.go`:

```go
package apisrv

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

// Server wraps an http.Server bound to a known address.
type Server struct {
	httpServer *http.Server
	ln         net.Listener
}

// New binds a listener on addr (use "127.0.0.1:0" for an arbitrary free port)
// and wires the routes. The listener is open on return, so clients may connect
// as soon as Run starts serving.
func New(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})
	mux.HandleFunc("POST /echo", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	})
	return &Server{
		httpServer: &http.Server{Handler: mux},
		ln:         ln,
	}, nil
}

// Addr returns the bound address, e.g. "127.0.0.1:54123".
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Run serves until ctx is cancelled, then shuts down gracefully with a bounded
// timeout. It returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.httpServer.Serve(s.ln) }()

	select {
	case err := <-errc:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	}
}
```

### The signal-aware demo

The demo is the production entrypoint in miniature: `signal.NotifyContext` gives a
context that cancels on `SIGINT`/`SIGTERM`, `Run` serves until then. To keep the
demo deterministic and self-terminating, it runs `Run` in a goroutine, drives the
server with a real client, then calls the `stop` function (which cancels the
context exactly as a signal would) and waits for a clean shutdown.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"example.com/apisrv"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s, err := apisrv.New("127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	base := "http://" + s.Addr()
	fmt.Println("healthz:", get(base+"/healthz"))
	fmt.Println("echo:", post(base+"/echo", "hello"))

	stop() // cancel the context as a signal would; triggers graceful shutdown
	if err := <-done; err != nil {
		log.Fatal(err)
	}
	fmt.Println("stopped gracefully")
}

func get(url string) string {
	resp, err := http.Get(url)
	if err != nil {
		return "error"
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func post(url, body string) string {
	resp, err := http.Post(url, "text/plain", strings.NewReader(body))
	if err != nil {
		return "error"
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
healthz: ok
echo: hello
stopped gracefully
```

### The tests

Handler behavior is tested directly through the mux with `httptest`; the
shutdown path is tested by starting `Run` on a real loopback listener, driving it
with an `http.Client`, cancelling the context, and asserting `Run` returns `nil`
within a deadline. That last test exercises the same graceful-exit path the
`-cover` binary depends on.

Create `server_test.go`:

```go
package apisrv

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHealthz(t *testing.T) {
	t.Parallel()
	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+s.Addr()+"/healthz", nil)
	rr := serveOnce(t, s, req)
	if rr.status != http.StatusOK || rr.body != "ok" {
		t.Fatalf("healthz = %d %q, want 200 \"ok\"", rr.status, rr.body)
	}
}

func TestEcho(t *testing.T) {
	t.Parallel()
	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+s.Addr()+"/echo",
		strings.NewReader("payload"))
	rr := serveOnce(t, s, req)
	if rr.status != http.StatusOK || rr.body != "payload" {
		t.Fatalf("echo = %d %q, want 200 \"payload\"", rr.status, rr.body)
	}
}

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()
	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Drive one request through the running server.
	resp, err := http.Get("http://" + s.Addr() + "/healthz")
	if err != nil {
		t.Fatalf("live request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("live healthz body = %q, want ok", body)
	}

	cancel() // like a signal: trigger graceful shutdown
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on clean shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

type recorded struct {
	status int
	body   string
}

// serveOnce runs the server, sends req with a real client, and returns the
// response, then shuts the server down.
func serveOnce(t *testing.T, s *Server, req *http.Request) recorded {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return recorded{status: resp.StatusCode, body: string(b)}
}
```

### The GOCOVERDIR integration-coverage workflow

Unit tests above run under `go test`. To measure the server the way an acceptance
test does — as a compiled black box — build it with `-cover`, run it with
`GOCOVERDIR` pointed at an existing directory, drive it, stop it, and convert the
raw data. First give the demo a real signal-driven lifetime by removing the
self-`stop()` for a live run, or drive a long-running variant; the workflow is:

```bash
# 1. Build an instrumented binary.
go build -cover -o app ./cmd/demo

# 2. Run it with a fresh, writable coverage directory.
mkdir -p covdir
GOCOVERDIR=covdir ./app
# (the demo self-drives and exits cleanly, flushing coverage on exit)

# 3. Read the percentage straight from the raw data.
go tool covdata percent -i=covdir
```

Expected output (abbreviated):

```
example.com/apisrv      coverage: 71.4% of statements
example.com/apisrv/cmd/demo  coverage: 88.9% of statements
```

Convert the raw data into a standard text profile so `go tool cover` can read it:

```bash
go tool covdata textfmt -i=covdir -o=integ.txt
go tool cover -func=integ.txt
```

The `integ.txt` profile now includes the request-handling blocks in `server.go`
that only the black-box driver executed — the `/healthz` and `/echo` handlers, the
`Serve`/`Shutdown` path — none of which a pure `httptest` unit run through the mux
would attribute to a compiled binary. Note the two different tools: `go tool
covdata` reads the raw `GOCOVERDIR` data; `go tool cover` reads the converted text
profile. Passing `covdir` to `go tool cover -func` fails because it is the wrong
format.

If `covdir` does not exist or is not writable, the binary writes nothing and
`covdata percent` reports no data — that is the classic "the integration test ran
zero code" false alarm; the fix is to create the directory first.

## Review

The server is correct when `/healthz` returns `200 "ok"`, `/echo` echoes its body,
and `Run` returns `nil` after its context is cancelled — the graceful path that a
`-cover` binary needs to flush coverage on exit. `TestGracefulShutdown` proves
that path directly: it drives a live request, cancels, and requires `Run` to
return `nil` before a deadline.

The mistakes to avoid are operational. Forgetting to create `GOCOVERDIR` (or
pointing it at an unwritable path) yields no data files and the wrong conclusion
that the run covered nothing. Force-killing the process skips the flush hook for
the same reason. And confusing the two tools — feeding a `GOCOVERDIR` directory to
`go tool cover` instead of `go tool covdata` — fails on a format mismatch. Run
`go test -race ./...` to confirm the serve/shutdown goroutines are clean.

## Resources

- [Code coverage for Go integration tests](https://go.dev/blog/integration-test-coverage) — the authoritative `go build -cover` + GOCOVERDIR + `covdata` walkthrough.
- [`go tool covdata`](https://pkg.go.dev/cmd/covdata) — `percent`, `textfmt`, `merge`, `func`.
- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — graceful shutdown semantics.
- [`os/signal.NotifyContext`](https://pkg.go.dev/os/signal#NotifyContext) — signal-cancelled contexts.

---

Back to [03-coverpkg-cross-package-service.md](03-coverpkg-cross-package-service.md) | Next: [05-merge-unit-integration-coverage.md](05-merge-unit-integration-coverage.md)
