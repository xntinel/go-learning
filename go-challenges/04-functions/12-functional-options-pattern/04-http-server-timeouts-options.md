# Exercise 4: Production http.Server Builder with Graceful Shutdown

The zero value of `http.Server` is dangerous: it has no read or write timeouts, so
a single slow client can hold a connection open indefinitely (a Slowloris attack).
A production server builder should refuse to ship without the security-relevant
timeouts set. This module builds that constructor with options that enforce safe
defaults, plus a `Serve` method that shuts down gracefully when its context is
cancelled.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
httpserver/                      independent module: example.com/httpserver
  go.mod                         go 1.26
  server.go                      Server, Option, NewServer(http.Handler, ...Option),
                                 WithAddr, WithReadHeaderTimeout, WithReadTimeout,
                                 WithWriteTimeout, WithIdleTimeout, WithShutdownTimeout,
                                 WithErrorLog, Serve(ctx, net.Listener)
  cmd/
    demo/
      main.go                    binds an ephemeral port, serves one request, cancels, shuts down
  server_test.go                 asserts timeout fields, zero-ReadHeaderTimeout rejection, graceful shutdown
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `NewServer(handler, opts...) (*Server, error)` seeding safe timeout defaults, and `Serve(ctx, ln)` that serves until ctx is cancelled, then calls `Shutdown` with a bounded context.
- Test: assert configured timeout fields; assert a zero `ReadHeaderTimeout` is rejected; start `Serve` on an ephemeral listener, issue one request, cancel the context, and assert graceful shutdown returns nil.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/04-http-server-timeouts-options/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/04-http-server-timeouts-options
```

### Options that enforce safe defaults for a dangerous zero value

The lesson of this module is that options are not only for optionality — they are
also how you stop a caller from accidentally shipping the dangerous zero value.
`http.Server{}` leaves `ReadHeaderTimeout`, `ReadTimeout`, and `WriteTimeout` at
zero, which means "no limit". A server with no `ReadHeaderTimeout` will wait
forever for a client to finish sending headers, and a handful of such clients can
exhaust the server. So `NewServer` seeds a non-zero `ReadHeaderTimeout` by
default and, after the option loop, *rejects* a `ReadHeaderTimeout` that was
explicitly set to zero. The option pattern turns "safe by default, and you cannot
opt into the unsafe value by accident" into an enforceable contract.

The other timeouts (`ReadTimeout`, `WriteTimeout`, `IdleTimeout`) get sane
defaults too, and each option lets a caller tune them for their workload — a
long-poll endpoint needs a larger `WriteTimeout`, a metrics scraper a larger
`IdleTimeout`. `WithErrorLog` injects a `*log.Logger` so the server's internal
errors go where the service wants them, another small dependency-injection option.

### Serve and graceful shutdown

`Serve(ctx, ln)` runs `http.Server.Serve` in a goroutine and blocks on a select:
if the server errors on its own it returns that error, and if the context is
cancelled it calls `Shutdown` with a bounded child context. `Shutdown` stops
accepting new connections and waits for in-flight requests to finish, up to the
deadline. After `Shutdown` completes, the background `Serve` returns
`http.ErrServerClosed`, which is the *expected* signal of a clean stop, not a
failure — so `Serve` drains it and returns nil on a graceful shutdown. Taking a
`net.Listener` rather than binding internally is what makes the method testable:
the test binds an ephemeral `127.0.0.1:0` port and therefore knows the address to
send its request to.

Create `server.go`:

```go
package httpserver

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// Server wraps a configured *http.Server with a bounded shutdown.
type Server struct {
	srv             *http.Server
	shutdownTimeout time.Duration
}

// Option configures a Server and may reject invalid input.
type Option func(*Server) error

// NewServer builds a Server for handler with safe timeout defaults. A zero
// ReadHeaderTimeout is rejected to defend against slow-client attacks.
func NewServer(handler http.Handler, opts ...Option) (*Server, error) {
	s := &Server{
		srv: &http.Server{
			Addr:              ":8080",
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
		shutdownTimeout: 5 * time.Second,
	}

	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, err
		}
	}

	if s.srv.ReadHeaderTimeout <= 0 {
		return nil, fmt.Errorf("ReadHeaderTimeout must be positive to defend against slow-client attacks")
	}
	return s, nil
}

// WithAddr sets the listen address.
func WithAddr(addr string) Option {
	return func(s *Server) error {
		if addr == "" {
			return fmt.Errorf("addr is required")
		}
		s.srv.Addr = addr
		return nil
	}
}

// WithReadHeaderTimeout bounds how long the server waits for request headers.
func WithReadHeaderTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d <= 0 {
			return fmt.Errorf("ReadHeaderTimeout must be positive, got %s", d)
		}
		s.srv.ReadHeaderTimeout = d
		return nil
	}
}

// WithReadTimeout bounds the whole request read.
func WithReadTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d < 0 {
			return fmt.Errorf("ReadTimeout must be >= 0, got %s", d)
		}
		s.srv.ReadTimeout = d
		return nil
	}
}

// WithWriteTimeout bounds the response write.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d < 0 {
			return fmt.Errorf("WriteTimeout must be >= 0, got %s", d)
		}
		s.srv.WriteTimeout = d
		return nil
	}
}

// WithIdleTimeout bounds keep-alive idle time.
func WithIdleTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d < 0 {
			return fmt.Errorf("IdleTimeout must be >= 0, got %s", d)
		}
		s.srv.IdleTimeout = d
		return nil
	}
}

// WithShutdownTimeout bounds the graceful-shutdown wait.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d <= 0 {
			return fmt.Errorf("shutdown timeout must be positive, got %s", d)
		}
		s.shutdownTimeout = d
		return nil
	}
}

// WithErrorLog injects the logger the server uses for internal errors.
func WithErrorLog(l *log.Logger) Option {
	return func(s *Server) error {
		if l == nil {
			return fmt.Errorf("error log is nil")
		}
		s.srv.ErrorLog = l
		return nil
	}
}

// Serve runs the server on ln until ctx is cancelled, then shuts down within the
// shutdown timeout. A clean shutdown returns nil.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		if err := s.srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		<-errc // drain the ErrServerClosed that Serve returns after Shutdown
		return nil
	}
}

// Addr returns the configured listen address.
func (s *Server) Addr() string { return s.srv.Addr }

// ReadHeaderTimeout returns the configured header-read timeout.
func (s *Server) ReadHeaderTimeout() time.Duration { return s.srv.ReadHeaderTimeout }

// WriteTimeout returns the configured write timeout.
func (s *Server) WriteTimeout() time.Duration { return s.srv.WriteTimeout }
```

### The runnable demo

The demo binds an ephemeral port, serves one request against it, then cancels the
context to trigger a graceful shutdown — the exact lifecycle of a real service
reacting to SIGTERM.

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

	"example.com/httpserver"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})

	srv, err := httpserver.NewServer(handler,
		httpserver.WithReadHeaderTimeout(3*time.Second),
		httpserver.WithWriteTimeout(5*time.Second),
	)
	if err != nil {
		panic(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		panic(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Printf("status: %d body: %s\n", resp.StatusCode, body)

	cancel()
	if err := <-done; err != nil {
		panic(err)
	}
	fmt.Println("server: shut down cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200 body: ok
server: shut down cleanly
```

### Tests

`TestDefaultsAndOverrides` asserts the seeded defaults and a couple of overrides.
`TestRejectsZeroReadHeaderTimeout` proves the safety check fires.
`TestGracefulShutdown` is the integration test: it binds an ephemeral listener,
serves one request, cancels the context, and asserts `Serve` returns nil within a
deadline — proving the shutdown path drains cleanly and does not hang.

Create `server_test.go`:

```go
package httpserver

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(http.NotFoundHandler(),
		WithAddr("127.0.0.1:9000"),
		WithWriteTimeout(15*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}

	if srv.Addr() != "127.0.0.1:9000" {
		t.Errorf("Addr = %q, want 127.0.0.1:9000", srv.Addr())
	}
	if srv.ReadHeaderTimeout() != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %s, want 5s (default)", srv.ReadHeaderTimeout())
	}
	if srv.WriteTimeout() != 15*time.Second {
		t.Errorf("WriteTimeout = %s, want 15s", srv.WriteTimeout())
	}
}

func TestRejectsZeroReadHeaderTimeout(t *testing.T) {
	t.Parallel()

	_, err := NewServer(http.NotFoundHandler(), WithReadHeaderTimeout(0))
	if err == nil {
		t.Fatal("expected error for zero ReadHeaderTimeout, got nil")
	}
}

func TestGracefulShutdown(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	srv, err := NewServer(handler, WithShutdownTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve did not return within the shutdown deadline")
	}
}
```

## Review

The builder is correct when it cannot produce a server with the dangerous
zero-value timeouts: `TestRejectsZeroReadHeaderTimeout` proves the constructor
enforces the safe invariant rather than trusting the caller. The graceful path is
correct when `Serve` treats the post-`Shutdown` `http.ErrServerClosed` as success
and returns nil — `TestGracefulShutdown` fails if the drain hangs or if the error
leaks out as a failure. Taking a `net.Listener` in `Serve` is the small design
choice that makes the whole lifecycle testable on an ephemeral port with no fixed
address to collide on under parallel tests.

## Resources

- [net/http Server](https://pkg.go.dev/net/http#Server)
- [net/http Server.Shutdown](https://pkg.go.dev/net/http#Server.Shutdown)
- [net/http ErrServerClosed](https://pkg.go.dev/net/http#ErrServerClosed)
- [Cloudflare: The complete guide to Go net/http timeouts](https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-slog-logger-options.md](03-slog-logger-options.md) | Next: [05-retrier-backoff-options.md](05-retrier-backoff-options.md)
