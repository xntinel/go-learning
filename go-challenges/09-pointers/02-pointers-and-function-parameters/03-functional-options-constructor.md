# Exercise 3: Functional Options — Building a Server Config with func(*Config) Parameters

Nearly every Go server or client library — `grpc`, `zap`, `redis`, the standard
`http.Server` wrappers people write — constructs with functional options:
`New(addr, opts...)` where each option is a closure that mutates a private config
through a pointer. This exercise builds that pattern from scratch so you see exactly
why the pointer parameter is what makes it work.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
server/                     independent module: example.com/server
  go.mod
  server.go                 serverConfig; Option = func(*serverConfig); WithTimeout/WithTLS/WithMaxConns; NewServer
  cmd/
    demo/
      main.go               builds a server with a few options and prints the config
  server_test.go            defaults, per-option isolation, last-wins ordering, no-nil-panic
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: an `Option func(*serverConfig)`, three `With...` options, and a `NewServer(addr, opts ...Option)` that starts from defaults and folds each option over a pointer to its config.
- Test: zero options yields defaults, each option sets exactly its field, a later option overrides an earlier one, and no options never panics.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/02-pointers-and-function-parameters/03-functional-options-constructor/cmd/demo
cd go-solutions/09-pointers/02-pointers-and-function-parameters/03-functional-options-constructor
```

### Why the option is a func(*Config)

`NewServer` builds one `serverConfig` seeded with defaults, then loops over the
options calling each with the *address* of that config. Each option is a closure
that captured its argument at construction time (`WithTimeout(5*time.Second)`
returns a `func(*serverConfig)` that sets `c.timeout = 5*time.Second`). Because
every option receives `&cfg` — the *same* address — their writes accumulate into
one config. That is the load-bearing detail: if the option took `serverConfig` by
value instead of `*serverConfig`, each option would mutate a throwaway copy and
`NewServer` would see none of the changes. The pointer parameter is what lets a
list of independent closures collaboratively build a single value.

The fold gives three properties for free. Defaults live in one place (the seed), so
`NewServer(addr)` with no options is fully specified. Options are independent — each
touches only its field — so adding a new one never breaks existing callers
(backward-compatible extension). And ordering is well-defined: options apply
left-to-right, so a later `WithTimeout` overrides an earlier one, which is exactly
the "last wins" behavior users expect from a config list.

Create `server.go`:

```go
package server

import "time"

type serverConfig struct {
	addr     string
	timeout  time.Duration
	tls      bool
	maxConns int
}

// Option mutates a serverConfig through a pointer. Every functional option is a
// value of this type; that is why they compose.
type Option func(*serverConfig)

// WithTimeout sets the request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *serverConfig) { c.timeout = d }
}

// WithTLS enables or disables TLS.
func WithTLS(enabled bool) Option {
	return func(c *serverConfig) { c.tls = enabled }
}

// WithMaxConns caps concurrent connections.
func WithMaxConns(n int) Option {
	return func(c *serverConfig) { c.maxConns = n }
}

const (
	defaultTimeout  = 30 * time.Second
	defaultMaxConns = 256
)

// Server is the constructed artifact.
type Server struct {
	cfg serverConfig
}

// NewServer builds a Server from an address and zero or more options. It seeds
// defaults, then folds each option over a pointer to the one config.
func NewServer(addr string, opts ...Option) *Server {
	cfg := serverConfig{
		addr:     addr,
		timeout:  defaultTimeout,
		maxConns: defaultMaxConns,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Server{cfg: cfg}
}

// Exported accessors so cmd/demo (a separate package) and tests can read the
// otherwise-private config.
func (s *Server) Addr() string           { return s.cfg.addr }
func (s *Server) Timeout() time.Duration { return s.cfg.timeout }
func (s *Server) TLS() bool              { return s.cfg.tls }
func (s *Server) MaxConns() int          { return s.cfg.maxConns }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/server"
)

func main() {
	s := server.NewServer("0.0.0.0:8443",
		server.WithTLS(true),
		server.WithTimeout(5*time.Second),
		server.WithMaxConns(1024),
	)
	fmt.Printf("addr=%s timeout=%s tls=%v maxConns=%d\n",
		s.Addr(), s.Timeout(), s.TLS(), s.MaxConns())

	// No options: documented defaults.
	d := server.NewServer("127.0.0.1:80")
	fmt.Printf("defaults: timeout=%s tls=%v maxConns=%d\n",
		d.Timeout(), d.TLS(), d.MaxConns())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
addr=0.0.0.0:8443 timeout=5s tls=true maxConns=1024
defaults: timeout=30s tls=false maxConns=256
```

### Tests

Create `server_test.go`:

```go
package server

import (
	"fmt"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	s := NewServer("addr")
	if s.Timeout() != defaultTimeout {
		t.Errorf("Timeout() = %s, want %s", s.Timeout(), defaultTimeout)
	}
	if s.MaxConns() != defaultMaxConns {
		t.Errorf("MaxConns() = %d, want %d", s.MaxConns(), defaultMaxConns)
	}
	if s.TLS() {
		t.Errorf("TLS() = true, want false by default")
	}
}

func TestEachOptionSetsOnlyItsField(t *testing.T) {
	t.Parallel()
	s := NewServer("addr", WithTLS(true))
	if !s.TLS() {
		t.Errorf("WithTLS did not enable TLS")
	}
	// Untouched fields keep their defaults.
	if s.Timeout() != defaultTimeout || s.MaxConns() != defaultMaxConns {
		t.Errorf("WithTLS disturbed other fields: %+v", s.cfg)
	}
}

func TestLastOptionWins(t *testing.T) {
	t.Parallel()
	s := NewServer("addr",
		WithTimeout(1*time.Second),
		WithTimeout(9*time.Second),
	)
	if s.Timeout() != 9*time.Second {
		t.Fatalf("Timeout() = %s, want the later 9s", s.Timeout())
	}
}

func TestNoOptionsDoesNotPanic(t *testing.T) {
	t.Parallel()
	// A nil options slice must be safe: the fold loop simply runs zero times.
	var opts []Option
	s := NewServer("addr", opts...)
	if s.Addr() != "addr" {
		t.Fatalf("Addr() = %q, want addr", s.Addr())
	}
}

func ExampleNewServer() {
	s := NewServer("localhost:443", WithTLS(true), WithMaxConns(10))
	fmt.Printf("%s tls=%v maxConns=%d\n", s.Addr(), s.TLS(), s.MaxConns())
	// Output: localhost:443 tls=true maxConns=10
}
```

## Review

The pattern is correct when the options mutate the *internal* config and not a copy
of it — which is exactly what `opt(&cfg)` guarantees. If an option ever took its
config by value, `TestLastOptionWins` and `TestEachOptionSetsOnlyItsField` would
both fail, because the writes would vanish into throwaway copies. The three
properties to preserve as the API grows: defaults stay in the seed (so no-option
construction is complete), each option touches only its field (so options stay
independent and additive), and order is left-to-right (so "last wins" is
predictable). This is why the pattern scales to dozens of options across library
versions without breaking callers. Run `go test -race`; construction is
single-goroutine, so this mainly confirms the accessors and fold compile cleanly.

## Resources

- [Self-referential functions and the design of options (Rob Pike)](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the origin of the pattern.
- [Functional options for friendly APIs (Dave Cheney)](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the canonical write-up used by most Go libraries.
- [Go Spec: Function types](https://go.dev/ref/spec#Function_types) — the `func(*T)` type behind `Option`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-patch-semantics-optional-pointer-fields.md](02-patch-semantics-optional-pointer-fields.md) | Next: [04-repository-scan-into-pointers.md](04-repository-scan-into-pointers.md)
