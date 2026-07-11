# Exercise 1: An HTTP Server That Embeds *http.Server

The most common production use of embedding is extending a standard-library type
without reimplementing it. Here you build a `Server` that embeds `*http.Server`,
adds a logger, and overrides `ListenAndServe`/`Shutdown` to log around the real
behavior while relying on promotion for everything else.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
embserver/                 independent module: example.com/embserver
  go.mod                   go 1.26
  server.go                type Server embeds *http.Server + *slog.Logger field; New; overrides
  cmd/
    demo/
      main.go              runnable demo: promotion + a live 202 request + shutdown
  server_test.go           promotion, injected/default logger, live request, shutdown, log contract
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `Server` embedding `*http.Server` with a named `*slog.Logger`; `New(addr, logger, handler)` with a nil-logger fallback and a `ReadHeaderTimeout`; overridden `ListenAndServe`/`Shutdown` that log and delegate.
- Test: promoted `Addr`/`Handler` reachable through the outer type, injected-logger identity, default-logger-when-nil, a live request returning 202, `Shutdown` tolerating `ErrServerClosed`, and a buffer-backed log-contract test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/embserver/cmd/demo
cd ~/go-exercises/embserver
go mod init example.com/embserver
```

### Why embed the pointer, and why the logger is named

`Server` embeds `*http.Server` — the pointer, not the value. `http.Server` holds
mutable, single-instance state (listeners, connection tracking, the shutdown
channel); embedding it by value would copy that state, and your overridden
`Shutdown` would operate on a copy the running server never sees. Embedding the
pointer means `s.Server` is the exact server that is listening. Promotion then
gives `Server` all of `http.Server`'s API for free: `s.Addr`, `s.Handler`,
`s.Serve`, `s.RegisterOnShutdown`, and the rest are reachable directly on the
outer type.

The logger, by contrast, is a *named* field, not embedded. If you embedded
`*slog.Logger`, every `slog.Logger` method (`Info`, `Warn`, `With`, `Enabled`, ...)
would promote onto `Server`, cluttering its API with logging methods that have
nothing to do with being a server. A named field keeps the surface clean: the
server logs internally, but `Server` is not itself a logger.

`New` takes a nil logger and falls back to `slog.Default()`, so callers who do not
care about logging still get a working server, and it sets a `ReadHeaderTimeout`
because a server with no header-read timeout is a slow-loris denial-of-service
target. The two overrides — `ListenAndServe` and `Shutdown` — shadow the promoted
methods, log a line, and then delegate to `s.Server`. That delegation is the whole
discipline: shadow to add behavior, then call the embedded field so the real work
still happens.

Create `server.go`:

```go
package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Server extends *http.Server with structured logging. It embeds the pointer so
// the outer methods operate on the exact server that is listening, and holds the
// logger as a named field so slog's API is not promoted onto the server.
type Server struct {
	*http.Server
	Logger *slog.Logger
}

// New builds a Server. A nil logger falls back to slog.Default(). A
// ReadHeaderTimeout is always set so the server is not a slow-loris target.
func New(addr string, logger *slog.Logger, handler http.Handler) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		Server: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
		Logger: logger,
	}
}

// ListenAndServe shadows the promoted method: it logs, then delegates to the
// embedded *http.Server so the real listen still happens.
func (s *Server) ListenAndServe() error {
	s.Logger.Info("server starting", "addr", s.Addr)
	return s.Server.ListenAndServe()
}

// Shutdown shadows the promoted method the same way: log, then delegate.
func (s *Server) Shutdown(ctx context.Context) error {
	s.Logger.Info("server shutting down")
	return s.Server.Shutdown(ctx)
}
```

### The runnable demo

The demo proves promotion (`s.Handler`, `s.ReadHeaderTimeout` are reached through
the outer type) and serves one real request returning 202. It uses `httptest` so
it needs no fixed port, and discards the logger so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	server "example.com/embserver"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "ok")
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := server.New(":8080", logger, handler)

	// Promoted from *http.Server, reached through the outer Server.
	fmt.Println("handler present:", s.Handler != nil)
	fmt.Println("read header timeout:", s.ReadHeaderTimeout)

	ts := httptest.NewServer(s.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	fmt.Println("status:", resp.StatusCode)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handler present: true
read header timeout: 5s
status: 202
```

### Tests

The tests pin every part of the embedding contract. `TestServerEmbedsHTTPServer`
is the core one: it reads `s.Addr` and `s.Handler` — fields of the embedded
`*http.Server` — through the outer `Server`, which only works because embedding
promotes them. The logger tests cover the injected-identity and nil-fallback
paths. `TestServerServesRequests` runs a real request through `httptest` and
expects 202. `TestServerShutdown` calls `Shutdown` on a server that never started
and tolerates a nil or `ErrServerClosed` result. `TestServerLoggerReceivesMessages`
backs the logger with a `bytes.Buffer` and asserts `Shutdown` actually logs,
pinning the logging contract rather than trusting it.

Create `server_test.go`:

```go
package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func noopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
}

func TestServerEmbedsHTTPServer(t *testing.T) {
	t.Parallel()
	s := New(":8080", discardLogger(), noopHandler())
	if s.Addr != ":8080" {
		t.Fatalf("promoted Addr = %q, want :8080", s.Addr)
	}
	if s.Handler == nil {
		t.Fatal("promoted Handler should be set")
	}
	if s.ReadHeaderTimeout == 0 {
		t.Fatal("promoted ReadHeaderTimeout should be non-zero")
	}
}

func TestServerUsesInjectedLogger(t *testing.T) {
	t.Parallel()
	logger := discardLogger()
	s := New(":8080", logger, noopHandler())
	if s.Logger != logger {
		t.Fatal("Logger should be the injected instance")
	}
}

func TestServerUsesDefaultLoggerWhenNil(t *testing.T) {
	t.Parallel()
	s := New(":8080", nil, noopHandler())
	if s.Logger == nil {
		t.Fatal("Logger should fall back to slog.Default()")
	}
}

func TestServerServesRequests(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "ok")
	})
	s := New(":0", discardLogger(), handler)

	ts := httptest.NewUnstartedServer(s.Handler)
	ts.Start()
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}

func TestServerShutdown(t *testing.T) {
	t.Parallel()
	s := New(":0", discardLogger(), noopHandler())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Shutdown = %v, want nil or ErrServerClosed", err)
	}
}

func TestServerLoggerReceivesMessages(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	s := New(":0", logger, noopHandler())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = s.Shutdown(ctx)

	if got := buf.String(); !strings.Contains(got, "server shutting down") {
		t.Fatalf("log did not record shutdown; got %q", got)
	}
}
```

## Review

The server is correct when the embedded `*http.Server` is the one that runs: the
overrides log and then delegate, never replace. The telltale of a broken override
is a `Shutdown` that logs and returns nil — it would pass a naive smoke test while
leaving connections open forever. Promotion is what `TestServerEmbedsHTTPServer`
proves: reading `s.Addr` compiles and returns the configured address only because
the embedded pointer's fields are promoted onto `Server`. Keep the logger a named
field, not embedded, so the server's API stays about serving; embedding the logger
would drag `Info`, `With`, and friends onto `Server` for no benefit. Run
`go test -race` to confirm the live request path is clean.

## Resources

- [`net/http.Server`](https://pkg.go.dev/net/http#Server) — the embedded type, its fields (`Addr`, `Handler`, `ReadHeaderTimeout`) and `ListenAndServe`/`Shutdown`.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — how a pointer to a struct is embedded and its methods promoted.
- [`log/slog`](https://pkg.go.dev/log/slog) — `slog.Default`, `slog.New`, and `slog.NewTextHandler`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-responsewriter-capture-middleware.md](02-responsewriter-capture-middleware.md)
