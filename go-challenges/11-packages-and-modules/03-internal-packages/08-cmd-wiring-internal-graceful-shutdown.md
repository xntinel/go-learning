# Exercise 8: Wire A Server From cmd/ Over Module-Root internal Packages

The standard Go service layout is a thin `cmd/server/main.go` that composes packages
living under a module-root `internal/`. That layout is not just organization — it is
an encapsulation strategy: because the handlers, config, and wiring are all under
`internal`, no downstream module can import or reuse your composition, so the service
is a leaf that nobody can accidentally depend on. Here you build that layout with a
signal-driven graceful shutdown, and you make the composition testable by extracting
the handler build into `internal/httpapi`.

This module is fully self-contained: its own module, its own demo, its own tests.
Nothing here imports any other exercise.

## What you'll build

```text
serverkit/                          module example.com/serverkit
  go.mod
  internal/config/config.go         Config; Load (env with defaults)
  internal/httpapi/httpapi.go       New(version) http.Handler with /healthz, /version
  internal/httpapi/httpapi_test.go  route tests + graceful-shutdown test
  cmd/server/main.go                thin entry: NotifyContext + Server.Shutdown (untested)
  cmd/demo/main.go                  runnable demo exercising the routes via httptest
```

- Files: `internal/config/config.go`, `internal/httpapi/httpapi.go`, `internal/httpapi/httpapi_test.go`, `cmd/server/main.go`, `cmd/demo/main.go`.
- Implement: an `httpapi.New` returning a routed `http.Handler`; a `config.Load`; a `cmd/server/main.go` that runs the server under `signal.NotifyContext` and shuts down gracefully.
- Test: route status/body via `httptest.NewServer`; the shutdown path by calling `Server.Shutdown` and asserting `Serve` returns `http.ErrServerClosed`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/serverkit/internal/config ~/go-exercises/serverkit/internal/httpapi ~/go-exercises/serverkit/cmd/server ~/go-exercises/serverkit/cmd/demo
cd ~/go-exercises/serverkit
go mod init example.com/serverkit
```

### Why cmd/ + internal/ is an encapsulation choice

A service is meant to be a terminal node in the dependency graph: something you
deploy, not something other modules import and build on. Putting every part of it —
handlers, config, wiring — under a module-root `internal/` enforces that. Another
module can depend on a shared library you publish, but it physically cannot import
`example.com/serverkit/internal/httpapi` to reuse your handler wiring, so your
composition stays yours to change. The `cmd/server/main.go` at the top is
deliberately thin: it reads config, builds the handler, runs the server, and handles
shutdown. All the logic worth testing is pushed down into `internal/httpapi`, which
is why `main` can stay a few untested lines while the routes and shutdown path are
covered.

The testability lever is extracting `httpapi.New` — a pure constructor that returns
an `http.Handler` — instead of building the mux inside `main`. A handler needs no
bound port to test: `httptest.NewServer` wraps it on a loopback address and you drive
it with a normal client. The graceful-shutdown behavior is tested separately by
running an `http.Server` on an ephemeral listener and calling `Shutdown`, then
asserting that `Serve` returns the sentinel `http.ErrServerClosed` — the signal that
the server stopped on request rather than crashing.

The shutdown wiring in `main` uses `signal.NotifyContext`, which returns a context
cancelled on `SIGINT`/`SIGTERM`. The server runs in a goroutine; when the context
fires, `main` calls `Server.Shutdown` with a bounded context so in-flight requests
drain but a hung connection cannot block shutdown forever. `ListenAndServe` returns
`http.ErrServerClosed` on a clean shutdown, which `main` treats as success rather
than an error.

Create `internal/config/config.go`:

```go
// Package config loads service configuration from the environment with defaults.
package config

import "os"

// Config is the service's runtime configuration.
type Config struct {
	Addr    string
	Version string
}

// Load reads APP_ADDR and APP_VERSION, applying defaults when unset.
func Load() Config {
	cfg := Config{Addr: ":8080", Version: "dev"}
	if v := os.Getenv("APP_ADDR"); v != "" {
		cfg.Addr = v
	}
	if v := os.Getenv("APP_VERSION"); v != "" {
		cfg.Version = v
	}
	return cfg
}
```

Create `internal/httpapi/httpapi.go`. `New` returns a routed handler using Go 1.22+
method-based patterns:

```go
// Package httpapi builds the service's HTTP handler. It is internal, so only
// example.com/serverkit may import it — the composition is not reusable downstream.
package httpapi

import (
	"fmt"
	"net/http"
)

// New returns the service handler: a health check and a version endpoint.
func New(version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, version)
	})
	return mux
}
```

Create `cmd/server/main.go`. This is the real, thin entry point. It compiles and is
correct, but it is not unit-tested — its logic is the composition the `internal`
packages already cover:

```go
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"example.com/serverkit/internal/config"
	"example.com/serverkit/internal/httpapi"
)

func main() {
	cfg := config.Load()
	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: httpapi.New(cfg.Version),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
		}
	}()
	log.Printf("listening on %s", cfg.Addr)

	<-ctx.Done()
	stop()
	log.Print("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
```

### The runnable demo

The demo exercises the routes through an in-process `httptest.Server`, so no port is
bound and the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/serverkit/internal/httpapi"
)

func main() {
	srv := httptest.NewServer(httpapi.New("v1.2.3"))
	defer srv.Close()

	for _, path := range []string{"/healthz", "/version"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			fmt.Println("request failed:", err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s -> %d %s", path, resp.StatusCode, string(body))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/healthz -> 200 ok
/version -> 200 v1.2.3
```

### Tests

The route tests drive the handler through `httptest.NewServer` — no port, no config.
`TestGracefulShutdown` runs an `http.Server` on an ephemeral loopback listener,
calls `Shutdown`, and asserts `Serve` returns `http.ErrServerClosed`, proving the
graceful-stop contract the `main` relies on.

Create `internal/httpapi/httpapi_test.go`:

```go
package httpapi

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRoutes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(New("v9"))
	defer srv.Close()

	cases := []struct {
		path     string
		wantCode int
		wantBody string
	}{
		{"/healthz", http.StatusOK, "ok\n"},
		{"/version", http.StatusOK, "v9\n"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != tc.wantCode {
				t.Errorf("GET %s status = %d, want %d", tc.path, resp.StatusCode, tc.wantCode)
			}
			if string(body) != tc.wantBody {
				t.Errorf("GET %s body = %q, want %q", tc.path, body, tc.wantBody)
			}
		})
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(New("v9"))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope status = %d, want 404", resp.StatusCode)
	}
}

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: New("v9")}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve returned %v, want http.ErrServerClosed", err)
	}
}
```

## Review

The layout is correct when `main` is thin and everything worth testing lives under
`internal`: `httpapi.New` is a pure constructor covered by route tests, and the
graceful-shutdown contract is proven by `Serve` returning `http.ErrServerClosed`
after `Shutdown`. Because all of this is under module-root `internal`, no other
module can import your handler wiring — the service is a leaf, as intended.

The traps: do not build the mux inside `main`, or you cannot test the routes without
binding a port and sending signals; extract `New` so a handler test needs no server
lifecycle. Treat `http.ErrServerClosed` as success, not failure — logging it as an
error on every clean shutdown is a classic false alarm. And bound the shutdown
context, so a single stuck connection cannot hold the process open forever.

## Resources

- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown) — graceful shutdown and `ErrServerClosed`.
- [`os/signal.NotifyContext`](https://pkg.go.dev/os/signal#NotifyContext) — a context cancelled on `SIGINT`/`SIGTERM`.
- [Organizing a Go module](https://go.dev/doc/modules/layout) — the `cmd/` + `internal/` service layout.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-export-test-white-box-internal.md](07-export-test-white-box-internal.md) | Next: [09-internal-does-not-isolate-peers.md](09-internal-does-not-isolate-peers.md)
