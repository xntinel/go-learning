# Exercise 1: Server Options

This exercise builds the canonical functional-options constructor: a `Server` whose every setting is a small validating function, whose defaults are overridden in call order, and whose presets are themselves options. It is the shape you will reach for whenever a Go constructor needs to grow optional, validated configuration without breaking existing callers.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
server.go              Server, Option, New, sentinel errors, With* options, presets, accessors
cmd/
  demo/
    main.go            build a server through options, then show an invalid port being rejected
server_test.go         defaults, per-option validation, order invariants, preset-then-override, accessors
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: the `Option func(*Server) error` type, `New(opts ...Option) (*Server, error)`, one `WithX` builder per field, two presets (`WithProductionDefaults`, `WithDevelopmentDefaults`), and read-only accessors.
- Test: `server_test.go` pins the defaults, every validator's sentinel, the order invariants (later wins, preset then override), short-circuit on the first failure, and that accessors reflect state.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/01-functional-options-deep-dive/01-server-options/cmd/demo && cd go-solutions/24-design-patterns-in-go/01-functional-options-deep-dive/01-server-options
```

### Why this shape

The package is a library, not a program: there is no `main` in the package root. The artifact is verified by `go test`, and `cmd/demo` is a separate `package main` that consumes the exported API the way a real caller would.

`New` does exactly two things in order, and that order is the whole pattern. It allocates a `Server` with every field set to a default, then ranges over the options and applies each one. Because defaults are written first, an option always overrides the default; because options run in slice order, a later option overrides an earlier one. When an option returns an error, `New` wraps it with `fmt.Errorf("server: %w", err)` and returns immediately — the constructor short-circuits rather than applying the rest, so a half-configured value never escapes. The `%w` keeps each option's sentinel reachable through `errors.Is`.

Each `WithX` is a closure factory: it captures the caller's argument and returns an `Option` that validates, then assigns. Out-of-range inputs return a package-level sentinel wrapped with `%w` so the bad value shows in the message while the sentinel stays matchable. The presets are the elegant part — `WithProductionDefaults` is just an `Option` whose body applies a bundle of other options through the shared `apply` helper, so the constructor cannot distinguish a preset from a single option and a failure inside a preset propagates exactly like any other.

The fields are unexported. `cmd/demo` lives in a different package and can only reach the exported accessors (`Host`, `Port`, `ReadTimeout`, ...), so the only way to read a server is through them and the only way to write one is through `New`. That is the encapsulation the pattern is designed to give you.

Create `server.go`:

```go
package server

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Option configures a Server. It receives the partially-built value and either
// mutates it or returns a validation error. Returning error (not bool or panic)
// is what lets presets compose options and surface failures through one channel.
type Option func(*Server) error

// Server is configured only through New and read only through accessors; every
// field stays unexported so a validated value cannot be mutated after building.
type Server struct {
	host         string
	port         int
	readTimeout  time.Duration
	writeTimeout time.Duration
	maxConns     int
	logger       *slog.Logger
}

var (
	ErrInvalidPort = errors.New("port must be between 1 and 65535")
	ErrEmptyHost   = errors.New("host must not be empty")
	ErrBadTimeout  = errors.New("timeout must be positive")
	ErrBadMaxConns = errors.New("max connections must be at least 1")
	ErrNilLogger   = errors.New("logger must not be nil")
)

// New applies defaults first, then the options in order, so a later option
// overrides an earlier one and an explicit option overrides a preset. It stops
// at the first failing option and wraps the cause so callers use errors.Is.
func New(opts ...Option) (*Server, error) {
	s := &Server{
		host:         "0.0.0.0",
		port:         8080,
		readTimeout:  5 * time.Second,
		writeTimeout: 10 * time.Second,
		maxConns:     100,
		logger:       slog.Default(),
	}
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
	}
	return s, nil
}

func WithHost(host string) Option {
	return func(s *Server) error {
		if host == "" {
			return ErrEmptyHost
		}
		s.host = host
		return nil
	}
}

func WithPort(port int) Option {
	return func(s *Server) error {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%w: got %d", ErrInvalidPort, port)
		}
		s.port = port
		return nil
	}
}

func WithReadTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d <= 0 {
			return fmt.Errorf("read %w: got %s", ErrBadTimeout, d)
		}
		s.readTimeout = d
		return nil
	}
}

func WithWriteTimeout(d time.Duration) Option {
	return func(s *Server) error {
		if d <= 0 {
			return fmt.Errorf("write %w: got %s", ErrBadTimeout, d)
		}
		s.writeTimeout = d
		return nil
	}
}

func WithMaxConns(n int) Option {
	return func(s *Server) error {
		if n < 1 {
			return fmt.Errorf("%w: got %d", ErrBadMaxConns, n)
		}
		s.maxConns = n
		return nil
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(s *Server) error {
		if l == nil {
			return ErrNilLogger
		}
		s.logger = l
		return nil
	}
}

// apply runs a bundle of options in order, short-circuiting on the first error.
// Presets share this so a preset is itself just an Option.
func apply(s *Server, opts ...Option) error {
	for _, opt := range opts {
		if err := opt(s); err != nil {
			return err
		}
	}
	return nil
}

// WithProductionDefaults is a preset: an Option whose body applies a bundle of
// other options. The constructor cannot tell it apart from a single option.
func WithProductionDefaults() Option {
	return func(s *Server) error {
		return apply(s,
			WithReadTimeout(30*time.Second),
			WithWriteTimeout(30*time.Second),
			WithMaxConns(1000),
		)
	}
}

func WithDevelopmentDefaults() Option {
	return func(s *Server) error {
		return apply(s,
			WithHost("localhost"),
			WithReadTimeout(120*time.Second),
			WithMaxConns(10),
		)
	}
}

func (s *Server) Host() string                { return s.host }
func (s *Server) Port() int                   { return s.port }
func (s *Server) ReadTimeout() time.Duration  { return s.readTimeout }
func (s *Server) WriteTimeout() time.Duration { return s.writeTimeout }
func (s *Server) MaxConns() int               { return s.maxConns }
func (s *Server) Logger() *slog.Logger        { return s.logger }
```

### The runnable demo

The demo builds a server through a mix of explicit options and the production preset, prints the assembled state through the accessors, then constructs a second server with a deliberately invalid port to show that `New` returns an error rather than a misconfigured value. Note the ordering: `WithMaxConns(250)` is passed before `WithProductionDefaults()`, and because the preset runs last it raises the limit to 1000 — a concrete demonstration of "later options win".

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"example.com/funcoptions"
)

func main() {
	s, err := server.New(
		server.WithHost("api.example.com"),
		server.WithPort(8443),
		server.WithWriteTimeout(30*time.Second),
		server.WithMaxConns(250),
		server.WithProductionDefaults(),
	)
	if err != nil {
		log.Fatalf("server.New: %v", err)
	}

	fmt.Printf("listening on %s:%d\n", s.Host(), s.Port())
	fmt.Printf("read=%s write=%s max=%d\n", s.ReadTimeout(), s.WriteTimeout(), s.MaxConns())
	fmt.Printf("logger=%T\n", s.Logger())

	_, err = server.New(server.WithPort(99999))
	if err == nil {
		fmt.Fprintln(os.Stderr, "expected error for invalid port")
		os.Exit(1)
	}
	fmt.Printf("invalid port rejected: %v\n", err)
}
```

The import path is the module path `example.com/funcoptions`; the package it names is `server`, so the demo calls `server.New`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
listening on api.example.com:8443
read=30s write=30s max=1000
logger=*slog.Logger
invalid port rejected: server: port must be between 1 and 65535: got 99999
```

### Tests

The tests are written in `package server` (white-box) so they can read the unexported fields directly while still exercising the exported constructor. They pin the defaults, each validator's sentinel via `errors.Is`, both order invariants, the short-circuit behavior, and that the accessors mirror the configured state.

Create `server_test.go`:

```go
package server

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()

	s, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if s.host != "0.0.0.0" || s.port != 8080 || s.maxConns != 100 {
		t.Fatalf("defaults wrong: %+v", s)
	}
	if s.readTimeout != 5*time.Second || s.writeTimeout != 10*time.Second {
		t.Fatalf("default timeouts wrong: read=%s write=%s", s.readTimeout, s.writeTimeout)
	}
	if s.logger == nil {
		t.Fatal("default logger must not be nil")
	}
}

func TestWithPortRejectsOutOfRange(t *testing.T) {
	t.Parallel()

	for _, port := range []int{0, -1, 65536, 99999} {
		_, err := New(WithPort(port))
		if !errors.Is(err, ErrInvalidPort) {
			t.Errorf("WithPort(%d): err = %v, want ErrInvalidPort", port, err)
		}
	}
}

func TestWithPortAcceptsValid(t *testing.T) {
	t.Parallel()

	s, err := New(WithPort(443))
	if err != nil {
		t.Fatalf("WithPort(443) error = %v", err)
	}
	if s.port != 443 {
		t.Fatalf("port = %d, want 443", s.port)
	}
}

func TestWithHostRejectsEmpty(t *testing.T) {
	t.Parallel()

	_, err := New(WithHost(""))
	if !errors.Is(err, ErrEmptyHost) {
		t.Fatalf("err = %v, want ErrEmptyHost", err)
	}
}

func TestReadTimeoutRejectsZeroOrNegative(t *testing.T) {
	t.Parallel()

	for _, d := range []time.Duration{0, -1, -time.Second} {
		_, err := New(WithReadTimeout(d))
		if !errors.Is(err, ErrBadTimeout) {
			t.Errorf("WithReadTimeout(%s): err = %v, want ErrBadTimeout", d, err)
		}
	}
}

func TestWithMaxConnsRejectsZero(t *testing.T) {
	t.Parallel()

	_, err := New(WithMaxConns(0))
	if !errors.Is(err, ErrBadMaxConns) {
		t.Fatalf("err = %v, want ErrBadMaxConns", err)
	}
}

func TestLaterOptionsWin(t *testing.T) {
	t.Parallel()

	s, err := New(WithPort(3000), WithPort(4000))
	if err != nil {
		t.Fatal(err)
	}
	if s.port != 4000 {
		t.Fatalf("port = %d, want 4000 (last option wins)", s.port)
	}
}

func TestPresetThenOverride(t *testing.T) {
	t.Parallel()

	s, err := New(WithProductionDefaults(), WithPort(443))
	if err != nil {
		t.Fatal(err)
	}
	if s.maxConns != 1000 {
		t.Fatalf("maxConns = %d, want 1000 from preset", s.maxConns)
	}
	if s.port != 443 {
		t.Fatalf("port = %d, want 443 from explicit override", s.port)
	}
}

func TestNewStopsAtFirstInvalidOption(t *testing.T) {
	t.Parallel()

	_, err := New(WithHost("api.example.com"), WithReadTimeout(-1))
	if !errors.Is(err, ErrBadTimeout) {
		t.Fatalf("err = %v, want ErrBadTimeout", err)
	}
}

func TestDevelopmentDefaultsSetHost(t *testing.T) {
	t.Parallel()

	s, err := New(WithDevelopmentDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if s.host != "localhost" {
		t.Fatalf("host = %q, want localhost", s.host)
	}
	if s.maxConns != 10 {
		t.Fatalf("maxConns = %d, want 10", s.maxConns)
	}
}

func TestWithLoggerRejectsNil(t *testing.T) {
	t.Parallel()

	_, err := New(WithLogger(nil))
	if !errors.Is(err, ErrNilLogger) {
		t.Fatalf("err = %v, want ErrNilLogger", err)
	}
}

func TestAccessorsReflectState(t *testing.T) {
	t.Parallel()

	l := quietLogger()
	s, err := New(WithHost("h"), WithPort(9090), WithMaxConns(7), WithLogger(l))
	if err != nil {
		t.Fatal(err)
	}
	if s.Host() != "h" || s.Port() != 9090 || s.MaxConns() != 7 {
		t.Errorf("accessors wrong: host=%q port=%d max=%d", s.Host(), s.Port(), s.MaxConns())
	}
	if s.Logger() != l {
		t.Error("logger not installed")
	}
}
```

## Review

The constructor is correct when defaults are written before the option loop and each option overrides in slice order; if you find a server on its default port after passing `WithPort`, you applied a default after the options, which is the one structural bug this pattern invites. Each validator must return its package-level sentinel (wrapped with `%w` when it carries the bad value) so `errors.Is` matches regardless of message wording — a validator that returns `fmt.Errorf("bad port")` will pass an eyeball check and fail every test that asserts the sentinel. `TestLaterOptionsWin` and `TestPresetThenOverride` together prove the ordering contract, and `TestNewStopsAtFirstInvalidOption` proves the short-circuit: a failed option aborts construction instead of letting a half-configured value escape. Confirm the fields stay unexported and `cmd/demo` reads only through accessors; the day a field is exported so the demo can read it, every holder can mutate the value past its validator. All of this holding under `go test -race ./...` is the signal the module is sound.

## Resources

- [Dave Cheney: Functional Options for Friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the article that named and popularized the pattern.
- [Rob Pike: Self-referential functions and the design of options](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the original idea the pattern grew from.
- [Uber Go Style Guide: Functional Options](https://github.com/uber-go/guide/blob/master/style.md#functional-options) — when to reach for options and how to name them.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — the matcher the sentinel-error contract is built on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-required-and-aggregated-options.md](02-required-and-aggregated-options.md)
