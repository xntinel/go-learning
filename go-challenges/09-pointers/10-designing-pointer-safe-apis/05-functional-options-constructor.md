# Exercise 5: Functional-Options Constructor Returning (*Server, error)

When a constructor grows past two or three parameters, positional arguments and
config structs both age badly. The functional-options pattern is the senior
default: `NewServer(opts ...Option)` where each `Option` is a small function that
mutates a private config. This module builds one that applies defaults, runs each
option, validates the assembled config, and returns a non-nil `*Server` on success
or `(nil, error)` on any invalid combination.

This module is fully self-contained: its own `go mod init`, its own types, its own
demo, and its own tests. Nothing here imports another exercise.

## What you'll build

```text
serveropts/                 independent module: example.com/serveropts
  go.mod                    go 1.25
  server.go                 Server; Option func(*config) error; WithTimeout/WithMaxConns/WithLogger; NewServer
  cmd/
    demo/
      main.go               build with defaults, then with options, then show a rejected option
  server_test.go            defaults, invalid option, valid option set, non-nil-iff-nil-err tests
```

- Files: `server.go`, `cmd/demo/main.go`, `server_test.go`.
- Implement: `NewServer(opts ...Option) (*Server, error)` where `Option` is `func(*config) error`; options for timeout, max connections, logger; defaults applied first, options next, validation last.
- Test: zero options yields a valid server with the documented defaults; `WithTimeout(-1)` returns `(nil, err)` via `errors.Is`; a conflicting option surfaces a wrapped error; a valid set returns a non-nil pointer whose fields match; the pointer is non-nil exactly when the error is nil.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/serveropts/cmd/demo
cd ~/go-exercises/serveropts
go mod init example.com/serveropts
go mod edit -go=1.25
```

### Why `Option` is `func(*config) error`, not `func(*config)`

The common form of the pattern is `func(*config)` — an option that cannot fail.
That is fine until an option needs to *validate* its argument. `WithTimeout(-1)`
is nonsense, and the only honest place to reject it is the option itself. Making
`Option` a `func(*config) error` lets each option validate its own input and
return an error the constructor propagates. The constructor runs the options in
order, and the first one that returns an error aborts construction with
`(nil, err)`. This keeps validation next to the thing being validated instead of a
giant switch in the constructor.

The `config` type is unexported. Callers never build a config directly; they pass
options, and the constructor owns assembly. This is deliberate encapsulation — the
`Server`'s configuration surface is exactly the set of `With…` functions you
export, so you can add fields without breaking callers and callers cannot reach
past your validation.

### Defaults, then options, then validate

The construction order matters. `NewServer` first fills a `config` with documented
defaults, then applies each option (which may overwrite a default or fail), then
runs a final whole-config validation that catches *combinations* no single option
could catch (for example, a read timeout longer than the total timeout). Only if
all three phases succeed does it build and return a non-nil `*Server`. The
contract mirrors every other constructor in this lesson: the returned pointer is
non-nil if and only if the error is nil, so a caller branches on `err` alone.

Errors are wrapped with `%w` so callers can match a specific cause with
`errors.Is`. `ErrInvalidTimeout` is the sentinel for a bad timeout; the final
cross-field check wraps `ErrInvalidConfig`.

Create `server.go`:

```go
package serveropts

import (
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	ErrInvalidTimeout = errors.New("invalid timeout")
	ErrInvalidConfig  = errors.New("invalid config")
)

// config is the private, option-assembled configuration. Callers never build it
// directly; they pass Options to NewServer.
type config struct {
	timeout  time.Duration
	maxConns int
	logger   io.Writer
}

// Option mutates the config and may reject its input.
type Option func(*config) error

// WithTimeout sets the request timeout. A non-positive timeout is rejected.
func WithTimeout(d time.Duration) Option {
	return func(c *config) error {
		if d <= 0 {
			return fmt.Errorf("%w: %s", ErrInvalidTimeout, d)
		}
		c.timeout = d
		return nil
	}
}

// WithMaxConns sets the maximum concurrent connections.
func WithMaxConns(n int) Option {
	return func(c *config) error {
		if n <= 0 {
			return fmt.Errorf("%w: maxConns must be positive, got %d", ErrInvalidConfig, n)
		}
		c.maxConns = n
		return nil
	}
}

// WithLogger sets the log sink.
func WithLogger(w io.Writer) Option {
	return func(c *config) error {
		if w == nil {
			return fmt.Errorf("%w: nil logger", ErrInvalidConfig)
		}
		c.logger = w
		return nil
	}
}

// Server is the constructed artifact. Its fields are read through accessors so
// the cmd/demo package (a separate package main) can observe them.
type Server struct {
	cfg config
}

// NewServer applies defaults, runs each option, validates the assembled config,
// and returns a non-nil *Server on success or (nil, error) on failure.
func NewServer(opts ...Option) (*Server, error) {
	cfg := config{
		timeout:  30 * time.Second,
		maxConns: 100,
		logger:   io.Discard,
	}
	for _, opt := range opts {
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}
	// Whole-config validation catches combinations no single option can.
	if cfg.timeout > time.Hour {
		return nil, fmt.Errorf("%w: timeout %s exceeds 1h ceiling", ErrInvalidConfig, cfg.timeout)
	}
	return &Server{cfg: cfg}, nil
}

func (s *Server) Timeout() time.Duration { return s.cfg.timeout }
func (s *Server) MaxConns() int          { return s.cfg.maxConns }
```

### The runnable demo

The demo builds three servers: one with defaults, one with two options applied,
and one that passes a rejected option so you can see the `(nil, error)` path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/serveropts"
)

func main() {
	def, _ := serveropts.NewServer()
	fmt.Printf("defaults: timeout=%s maxConns=%d\n", def.Timeout(), def.MaxConns())

	tuned, _ := serveropts.NewServer(
		serveropts.WithTimeout(5*time.Second),
		serveropts.WithMaxConns(500),
	)
	fmt.Printf("tuned:    timeout=%s maxConns=%d\n", tuned.Timeout(), tuned.MaxConns())

	_, err := serveropts.NewServer(serveropts.WithTimeout(-1))
	fmt.Printf("rejected: %v (is ErrInvalidTimeout: %t)\n", err, errors.Is(err, serveropts.ErrInvalidTimeout))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: timeout=30s maxConns=100
tuned:    timeout=5s maxConns=500
rejected: invalid timeout: -1ns (is ErrInvalidTimeout: true)
```

### Tests

The tests pin all four contract points. The defaults test asserts a zero-option
server has the documented defaults and a non-nil pointer. The invalid-option test
asserts `WithTimeout(-1)` returns `(nil, err)` matching `ErrInvalidTimeout`. The
cross-field test drives `WithTimeout(2*time.Hour)` past the whole-config ceiling
and asserts a wrapped `ErrInvalidConfig` with a nil pointer. The valid-set test
asserts the applied options are reflected in the fields. Every failure path also
asserts the pointer is nil, pinning "non-nil iff nil error."

Create `server_test.go`:

```go
package serveropts

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
	if s == nil {
		t.Fatal("server = nil on success")
	}
	if s.Timeout() != 30*time.Second {
		t.Fatalf("default timeout = %s, want 30s", s.Timeout())
	}
	if s.MaxConns() != 100 {
		t.Fatalf("default maxConns = %d, want 100", s.MaxConns())
	}
}

func TestInvalidOptionReturnsNilAndError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opt    Option
		target error
	}{
		{name: "negative timeout", opt: WithTimeout(-1), target: ErrInvalidTimeout},
		{name: "zero timeout", opt: WithTimeout(0), target: ErrInvalidTimeout},
		{name: "zero maxConns", opt: WithMaxConns(0), target: ErrInvalidConfig},
		{name: "nil logger", opt: WithLogger(nil), target: ErrInvalidConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, err := NewServer(tc.opt)
			if !errors.Is(err, tc.target) {
				t.Fatalf("err = %v, want %v", err, tc.target)
			}
			if s != nil {
				t.Fatalf("server = %+v, want nil on error", s)
			}
		})
	}
}

func TestCrossFieldValidation(t *testing.T) {
	t.Parallel()
	s, err := NewServer(WithTimeout(2 * time.Hour))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
	if s != nil {
		t.Fatal("server = non-nil, want nil on cross-field failure")
	}
}

func TestValidOptionSet(t *testing.T) {
	t.Parallel()
	s, err := NewServer(WithTimeout(5*time.Second), WithMaxConns(500))
	if err != nil {
		t.Fatal(err)
	}
	if s.Timeout() != 5*time.Second || s.MaxConns() != 500 {
		t.Fatalf("got timeout=%s maxConns=%d, want 5s/500", s.Timeout(), s.MaxConns())
	}
}

func ExampleNewServer() {
	s, err := NewServer(WithTimeout(5*time.Second), WithMaxConns(200))
	fmt.Println(err == nil, s.Timeout(), s.MaxConns())
	// Output: true 5s 200
}
```

## Review

The constructor is correct when it returns a non-nil `*Server` exactly when the
error is nil, with defaults filled, options applied in order, and the whole-config
check rejecting bad combinations. Putting validation *inside* each option is the
key move: `WithTimeout(-1)` fails at the option, not in a distant switch, so the
error names the offending input. The mistake to avoid is the `func(*config)`
(no-error) option shape, which forces you to either panic on bad input or defer all
validation to a monolithic check that has lost track of which option supplied the
bad value. The second mistake is returning a half-built server alongside an error;
keep the biconditional so callers never nil-check the pointer.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the canonical write-up of the pattern.
- [Uber Go Style Guide: functional options](https://github.com/uber-go/guide/blob/master/style.md#functional-options) — production conventions for it.
- [`fmt.Errorf` with `%w`](https://pkg.go.dev/fmt#Errorf) — wrapping a sentinel so `errors.Is` matches.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-patch-tri-state-pointer-fields.md](04-patch-tri-state-pointer-fields.md) | Next: [06-nil-guard-http-handler.md](06-nil-guard-http-handler.md)
