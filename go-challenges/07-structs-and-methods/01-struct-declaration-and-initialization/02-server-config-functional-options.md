# Exercise 2: Configuring an HTTP Server With Functional Options

A server config with a dozen knobs is where the "giant positional constructor"
and the "mutable exported config struct" both fall apart. The idiomatic senior
answer is the functional-options pattern: defaults first, then a variadic list of
`Option` functions that override only what the caller names, with validation at
the end. This module builds that pattern around a `ServerConfig`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
serveropts/                 independent module: example.com/serveropts
  go.mod                    go 1.24
  server.go                 ServerConfig; Option; WithAddr/WithReadTimeout/WithMaxConns; NewServer; getters
  cmd/
    demo/
      main.go               builds a server with a subset of options, prints config
  server_test.go            defaults, overrides, ordering, validation via errors.Is
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: a `ServerConfig` (Addr, ReadTimeout, WriteTimeout, MaxConns, ShutdownGrace); an `Option` type `func(*ServerConfig) error`; `WithAddr`, `WithReadTimeout`, `WithMaxConns`; and `NewServer(opts ...Option) (*Server, error)` that sets defaults, applies options in order, then validates.
- Test: no options yields documented defaults; a subset overrides only named fields; options apply in order (last `WithAddr` wins); `WithMaxConns(0)` and a negative timeout return a validation error asserted with `errors.Is`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/02-server-config-functional-options/cmd/demo
cd go-solutions/07-structs-and-methods/01-struct-declaration-and-initialization/02-server-config-functional-options
go mod edit -go=1.24
```

### Why functional options

Three alternatives lose to functional options for a config with many fields.
A positional constructor `NewServer(addr string, rt, wt time.Duration, max int, grace time.Duration)`
is unreadable at the call site and breaks every caller when you add a knob. A
mutable exported `Config` struct passed in lets any caller mutate it after
construction and offers no place to validate. A `Config` with a `Validate()`
method the caller must remember to call pushes the invariant onto the caller.

Functional options fix all three. Each option is a small closure `func(*ServerConfig) error`
that mutates the in-progress config. `NewServer` sets sensible defaults first,
applies the options in the order given (so a later option overrides an earlier
one — last `WithAddr` wins), then validates once and returns an error the caller
must handle. Adding a knob is adding a `With…` function; existing callers are
untouched. Returning an `error` from each option lets an option reject a bad
value early, and `NewServer` joins any validation failures with `errors.Join`.

The config's fields are unexported so the only way to build a `Server` is through
`NewServer`, which guarantees defaults and validation ran. Exported getters expose
what the demo (a separate `package main`) needs to read.

Create `server.go`:

```go
package server

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidConfig is the sentinel every validation failure wraps.
var ErrInvalidConfig = errors.New("invalid server config")

// ServerConfig holds the tunable parameters of a Server. Fields are unexported
// so a Server can only be built through NewServer, which sets defaults and
// validates.
type ServerConfig struct {
	addr          string
	readTimeout   time.Duration
	writeTimeout  time.Duration
	maxConns      int
	shutdownGrace time.Duration
}

// Server is the constructed, validated result.
type Server struct {
	cfg ServerConfig
}

// Option mutates an in-progress config and may reject a bad value.
type Option func(*ServerConfig) error

// WithAddr sets the listen address.
func WithAddr(addr string) Option {
	return func(c *ServerConfig) error {
		if addr == "" {
			return fmt.Errorf("%w: addr must not be empty", ErrInvalidConfig)
		}
		c.addr = addr
		return nil
	}
}

// WithReadTimeout sets the read timeout. A negative value is rejected.
func WithReadTimeout(d time.Duration) Option {
	return func(c *ServerConfig) error {
		if d < 0 {
			return fmt.Errorf("%w: read timeout must not be negative", ErrInvalidConfig)
		}
		c.readTimeout = d
		return nil
	}
}

// WithMaxConns sets the connection cap. It must be positive.
func WithMaxConns(n int) Option {
	return func(c *ServerConfig) error {
		if n <= 0 {
			return fmt.Errorf("%w: max conns must be positive, got %d", ErrInvalidConfig, n)
		}
		c.maxConns = n
		return nil
	}
}

// NewServer applies defaults, then the options in order, then validates.
func NewServer(opts ...Option) (*Server, error) {
	cfg := ServerConfig{
		addr:          ":8080",
		readTimeout:   15 * time.Second,
		writeTimeout:  15 * time.Second,
		maxConns:      1024,
		shutdownGrace: 30 * time.Second,
	}
	var errs []error
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &Server{cfg: cfg}, nil
}

// Exported getters for callers in other packages.
func (s *Server) Addr() string                 { return s.cfg.addr }
func (s *Server) ReadTimeout() time.Duration   { return s.cfg.readTimeout }
func (s *Server) WriteTimeout() time.Duration  { return s.cfg.writeTimeout }
func (s *Server) MaxConns() int                { return s.cfg.maxConns }
func (s *Server) ShutdownGrace() time.Duration { return s.cfg.shutdownGrace }
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/serveropts"
)

func main() {
	s, err := server.NewServer(
		server.WithAddr(":9000"),
		server.WithReadTimeout(5*time.Second),
	)
	if err != nil {
		fmt.Println("build failed:", err)
		return
	}
	fmt.Printf("addr=%s read=%s write=%s max=%d grace=%s\n",
		s.Addr(), s.ReadTimeout(), s.WriteTimeout(), s.MaxConns(), s.ShutdownGrace())

	if _, err := server.NewServer(server.WithMaxConns(0)); err != nil {
		fmt.Println("rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
addr=:9000 read=5s write=15s max=1024 grace=30s
rejected: invalid server config: max conns must be positive, got 0
```

### Tests

The tests cover the four behaviors the pattern promises: defaults when no options
are passed, selective override (only named fields change, the rest keep their
defaults), last-writer-wins ordering, and validation rejecting bad values through
the `ErrInvalidConfig` sentinel with `errors.Is`.

Create `server_test.go`:

```go
package server

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	s, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr() != ":8080" || s.ReadTimeout() != 15*time.Second ||
		s.WriteTimeout() != 15*time.Second || s.MaxConns() != 1024 ||
		s.ShutdownGrace() != 30*time.Second {
		t.Fatalf("unexpected defaults: %+v", s.cfg)
	}
}

func TestSelectiveOverride(t *testing.T) {
	t.Parallel()
	s, err := NewServer(WithAddr(":9000"), WithMaxConns(64))
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr() != ":9000" {
		t.Fatalf("addr = %q, want :9000", s.Addr())
	}
	if s.MaxConns() != 64 {
		t.Fatalf("maxConns = %d, want 64", s.MaxConns())
	}
	// Untouched fields keep their defaults.
	if s.ReadTimeout() != 15*time.Second || s.ShutdownGrace() != 30*time.Second {
		t.Fatalf("defaults were disturbed: %+v", s.cfg)
	}
}

func TestOptionsApplyInOrder(t *testing.T) {
	t.Parallel()
	s, err := NewServer(WithAddr(":1"), WithAddr(":2"), WithAddr(":3"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr() != ":3" {
		t.Fatalf("addr = %q, want :3 (last wins)", s.Addr())
	}
}

func TestValidationRejectsBadValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opt  Option
	}{
		{"zero max conns", WithMaxConns(0)},
		{"negative max conns", WithMaxConns(-1)},
		{"negative read timeout", WithReadTimeout(-time.Second)},
		{"empty addr", WithAddr("")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewServer(tc.opt); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestErrorsJoinAggregates(t *testing.T) {
	t.Parallel()
	_, err := NewServer(WithMaxConns(0), WithReadTimeout(-time.Second))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}

func ExampleNewServer() {
	s, _ := NewServer(WithAddr(":443"))
	fmt.Printf("%s max=%d\n", s.Addr(), s.MaxConns())
	// Output: :443 max=1024
}
```

## Review

The pattern is correct when the three invariants hold: `NewServer()` with no
options returns exactly the documented defaults; each option touches only its own
field, so a partial override leaves every other field at its default; and a bad
value is rejected before a `Server` exists, never after. The `errors.Join` step is
what lets multiple bad options report together instead of only the first — a real
benefit when a config file supplies several wrong values at once. The failure mode
to avoid is exporting the config fields "for convenience," which reopens the door
to post-construction mutation and skipped validation; keeping them unexported and
building only through `NewServer` is what makes the invariant hold. Run
`go test -race` and `go vet`.

## Resources

- [Self-referential functions and the design of options](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — Rob Pike on the functional-options pattern.
- [Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — Dave Cheney's walkthrough.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — aggregating multiple option failures.
- [`net/http.Server`](https://pkg.go.dev/net/http#Server) — the real fields (`ReadTimeout`, `WriteTimeout`) this config mirrors.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-make-the-zero-value-useful.md](03-make-the-zero-value-useful.md)
