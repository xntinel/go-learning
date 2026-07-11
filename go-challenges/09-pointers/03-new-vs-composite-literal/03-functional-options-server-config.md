# Exercise 3: Functional-Options Constructor for an HTTP Server Config

The idiomatic alternative to `new(ServerConfig)` followed by a run of caller-side
field assignments is the functional-options constructor: start from a
composite-literal defaults struct and apply a variadic list of `Option` closures.
This exercise builds `NewServerConfig(opts ...Option) *ServerConfig` and its
options, and contrasts it against the `new`-then-assign anti-pattern.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
serverconfig/                 independent module: example.com/serverconfig
  go.mod                      go 1.26
  config.go                   ServerConfig, Option, NewServerConfig, WithReadTimeout/WithMaxConns/WithHost/WithTLS
  cmd/
    demo/
      main.go                 runnable demo: defaults, single override, composed overrides
  config_test.go              defaults test, per-option isolation, ordering, independence-per-call
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: `NewServerConfig` starting from a defaults literal and applying
variadic `Option` closures; `WithReadTimeout`, `WithMaxConns`, `WithHost`,
`WithTLS`; use `cmp.Or` for a "fallback unless zero" default.
Test: no options yields documented defaults; each `WithX` overrides exactly one
field; options compose in order and later options win; two calls return
independent pointers.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/serverconfig/cmd/demo
cd ~/go-exercises/serverconfig
go mod init example.com/serverconfig
```

### Why options beat new-then-assign

Consider the alternative this exercise replaces. Without options, every call site
that wants a non-default server writes `c := new(ServerConfig)` (or `&ServerConfig{}`)
and then a run of `c.ReadTimeout = ...; c.MaxConns = ...; c.Host = ...`. The
defaults live nowhere — each caller must remember them — and forgetting one field
leaves it at the Go zero value (`ReadTimeout` of 0 means "no timeout", a
foot-gun), not the sane default. The initialization logic is scattered across
every caller.

The functional-options constructor centralizes it. `NewServerConfig` starts from
one composite literal that holds the documented defaults, then applies each
`Option` — a `func(*ServerConfig)` closure that mutates exactly one field. A
caller writes `NewServerConfig(WithMaxConns(500))` and gets a config that is
default in every respect except `MaxConns`. The defaults live in one place; the
options are self-documenting; later options override earlier ones because they
run in slice order; and because the constructor allocates a fresh struct each
call, two calls never share state. `cmp.Or` (Go 1.22+) returns its first non-zero
argument, which is a clean way to express "use the supplied value, or fall back
to a default if it is the zero value" inside an option.

Create `config.go`:

```go
package serverconfig

import (
	"cmp"
	"time"
)

// ServerConfig holds the tunables for an HTTP server. The defaults live in
// NewServerConfig, not scattered across call sites.
type ServerConfig struct {
	Host         string
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	MaxConns     int
	TLS          bool
}

// Option mutates a ServerConfig during construction. Each option overrides one
// field; options run in order, so a later option wins over an earlier one.
type Option func(*ServerConfig)

// NewServerConfig builds a ServerConfig from a composite-literal defaults struct
// and applies each option in order. With no options it returns the documented
// defaults. Each call allocates a fresh, independent *ServerConfig.
func NewServerConfig(opts ...Option) *ServerConfig {
	c := &ServerConfig{
		Host:         "0.0.0.0",
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		MaxConns:     100,
		TLS:          false,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithHost sets the bind host, falling back to the default if h is empty.
func WithHost(h string) Option {
	return func(c *ServerConfig) {
		c.Host = cmp.Or(h, c.Host)
	}
}

// WithReadTimeout sets the read timeout.
func WithReadTimeout(d time.Duration) Option {
	return func(c *ServerConfig) {
		c.ReadTimeout = d
	}
}

// WithMaxConns sets the maximum concurrent connections.
func WithMaxConns(n int) Option {
	return func(c *ServerConfig) {
		c.MaxConns = n
	}
}

// WithTLS enables or disables TLS.
func WithTLS(enabled bool) Option {
	return func(c *ServerConfig) {
		c.TLS = enabled
	}
}
```

### The runnable demo

The demo prints the defaults, a single override, and a composed set of overrides
where a later option wins over an earlier one.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/serverconfig"
)

func main() {
	def := serverconfig.NewServerConfig()
	fmt.Printf("defaults: host=%s read=%s conns=%d tls=%v\n",
		def.Host, def.ReadTimeout, def.MaxConns, def.TLS)

	one := serverconfig.NewServerConfig(serverconfig.WithMaxConns(500))
	fmt.Printf("one:      host=%s read=%s conns=%d tls=%v\n",
		one.Host, one.ReadTimeout, one.MaxConns, one.TLS)

	many := serverconfig.NewServerConfig(
		serverconfig.WithHost("api.internal"),
		serverconfig.WithTLS(true),
		serverconfig.WithMaxConns(200),
		serverconfig.WithMaxConns(300), // later wins
	)
	fmt.Printf("many:     host=%s read=%s conns=%d tls=%v\n",
		many.Host, many.ReadTimeout, many.MaxConns, many.TLS)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: host=0.0.0.0 read=5s conns=100 tls=false
one:      host=0.0.0.0 read=5s conns=500 tls=false
many:     host=api.internal read=5s conns=300 tls=true
```

### Tests

The tests fix each property the constructor promises: the no-option defaults, that
`WithX` touches exactly one field, that ordering makes the later option win, and
that two constructions are independent allocations.

Create `config_test.go`:

```go
package serverconfig

import (
	"fmt"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Parallel()

	c := NewServerConfig()
	if c.Host != "0.0.0.0" {
		t.Errorf("Host = %q, want 0.0.0.0", c.Host)
	}
	if c.ReadTimeout != 5*time.Second {
		t.Errorf("ReadTimeout = %s, want 5s", c.ReadTimeout)
	}
	if c.WriteTimeout != 10*time.Second {
		t.Errorf("WriteTimeout = %s, want 10s", c.WriteTimeout)
	}
	if c.MaxConns != 100 {
		t.Errorf("MaxConns = %d, want 100", c.MaxConns)
	}
	if c.TLS {
		t.Error("TLS = true, want false")
	}
}

func TestEachOptionOverridesOneField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		opt    Option
		assert func(t *testing.T, c *ServerConfig)
	}{
		{"host", WithHost("db.local"), func(t *testing.T, c *ServerConfig) {
			if c.Host != "db.local" {
				t.Errorf("Host = %q, want db.local", c.Host)
			}
		}},
		{"read", WithReadTimeout(30 * time.Second), func(t *testing.T, c *ServerConfig) {
			if c.ReadTimeout != 30*time.Second {
				t.Errorf("ReadTimeout = %s, want 30s", c.ReadTimeout)
			}
		}},
		{"conns", WithMaxConns(999), func(t *testing.T, c *ServerConfig) {
			if c.MaxConns != 999 {
				t.Errorf("MaxConns = %d, want 999", c.MaxConns)
			}
		}},
		{"tls", WithTLS(true), func(t *testing.T, c *ServerConfig) {
			if !c.TLS {
				t.Error("TLS = false, want true")
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			def := NewServerConfig()
			c := NewServerConfig(tc.opt)
			tc.assert(t, c)
			// WriteTimeout is never touched by any option under test.
			if c.WriteTimeout != def.WriteTimeout {
				t.Errorf("WriteTimeout changed to %s; want default %s", c.WriteTimeout, def.WriteTimeout)
			}
		})
	}
}

func TestOptionsComposeInOrder(t *testing.T) {
	t.Parallel()

	c := NewServerConfig(
		WithMaxConns(1),
		WithMaxConns(2),
		WithMaxConns(3),
	)
	if c.MaxConns != 3 {
		t.Fatalf("MaxConns = %d, want 3 (last option wins)", c.MaxConns)
	}
}

func TestWithHostFallsBackWhenEmpty(t *testing.T) {
	t.Parallel()

	c := NewServerConfig(WithHost(""))
	if c.Host != "0.0.0.0" {
		t.Fatalf("Host = %q, want default 0.0.0.0 when empty passed", c.Host)
	}
}

func TestCallsAreIndependent(t *testing.T) {
	t.Parallel()

	a := NewServerConfig(WithMaxConns(1))
	b := NewServerConfig(WithMaxConns(2))
	if a == b {
		t.Fatal("two calls returned the same pointer; must be independent")
	}
	if a.MaxConns != 1 || b.MaxConns != 2 {
		t.Fatalf("configs share state: a=%d b=%d", a.MaxConns, b.MaxConns)
	}
}

func ExampleNewServerConfig() {
	c := NewServerConfig(WithHost("localhost"), WithTLS(true))
	fmt.Printf("%s conns=%d tls=%v\n", c.Host, c.MaxConns, c.TLS)
	// Output: localhost conns=100 tls=true
}
```

## Review

The constructor is correct when `NewServerConfig()` returns the documented
defaults, when each option changes exactly its own field and leaves the rest at
default (the table test asserts `WriteTimeout` never moves), when the last of
several conflicting options wins, and when two calls are distinct allocations that
do not share state. The one subtlety worth naming is `cmp.Or` in `WithHost`:
passing `""` must not blank out the default, and `cmp.Or(h, c.Host)` returns the
current value when `h` is the zero string, which `TestWithHostFallsBackWhenEmpty`
pins. The anti-pattern this replaces is `new(ServerConfig)` plus a stack of field
assignments at each call site, where the defaults live nowhere and a forgotten
field silently sits at the Go zero value — a `ReadTimeout` of 0 meaning "no
timeout" is the classic production incident.

## Resources

- [Rob Pike: Self-referential functions and the design of options](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the original functional-options pattern.
- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the canonical write-up of the idiom.
- [cmp.Or](https://pkg.go.dev/cmp#Or) — first non-zero argument, for default fallbacks.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-nullable-patch-dto.md](04-nullable-patch-dto.md)
