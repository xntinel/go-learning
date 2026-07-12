# Exercise 4: A Config Loader With Functional Options Mutating *Config

The functional-options pattern is how idiomatic Go constructors take optional
settings without a giant parameter list or a mutable public struct. Each option is
a `func(*Config)` that mutates the config in place; the constructor applies defaults
first, then each option in order. This exercise builds a server `Config` with
`WithTimeout`, `WithMaxConns`, `WithTLS`, and a validating option that can fail.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
serverconfig/             independent module: example.com/serverconfig
  go.mod
  config.go               Config; Option = func(*Config) error; New; With... options
  cmd/
    demo/
      main.go             build a config from defaults + a few options
  config_test.go          defaults, per-option mutation, last-writer-wins, error path
```

Files: `config.go`, `cmd/demo/main.go`, `config_test.go`.
Implement: a `Config` struct, `Option = func(*Config) error`, `New(opts ...Option) (*Config, error)` that seeds defaults then applies each option, and options `WithTimeout`, `WithMaxConns`, `WithTLS`, plus a validating `WithMaxConns` that rejects non-positive values.
Test: defaults applied with no options; each option mutates only its field; options compose in order (last-writer-wins on the same field); a failing option returns an error; `New` returns a non-nil `*Config` on success.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/05-pointers-to-structs/04-functional-options-config/cmd/demo
cd go-solutions/09-pointers/05-pointers-to-structs/04-functional-options-config
```

### Why options are `func(*Config)` and the constructor returns `*Config`

The pattern rests entirely on pointer receivers-by-argument. An `Option` is
`func(*Config) error`: it receives a pointer to the config being built and mutates
it in place. If the option took `Config` by value it would mutate a copy the
constructor discards, and no setting would stick — the same value-receiver trap as a
mutating method. The constructor allocates one `*Config`, seeds it with defaults, and
then threads that one pointer through every option in turn. Because they all mutate
the same struct, two options that touch the same field compose as last-writer-wins:
`New(WithMaxConns(10), WithMaxConns(20))` ends at 20. Order is significant and
intentional.

Returning `(*Config, error)` (not `Config`) matters for two reasons. First, the
config is meant to be handed to a server that holds it and reads it over its
lifetime — sharing one struct via a pointer is the point. Second, an option can fail
(a negative connection count, an unreadable TLS cert), so the constructor needs an
error return; the moment an option returns non-nil, `New` stops and reports it
rather than building a half-valid config. Making `Option` return `error` (rather than
a separate non-failing `func(*Config)`) keeps every option uniform and lets any of
them validate.

Create `config.go`:

```go
package serverconfig

import (
	"fmt"
	"time"
)

// Config is the server's runtime configuration. It is built once via New and then
// shared as *Config; callers read it over the server's lifetime.
type Config struct {
	Addr        string
	Timeout     time.Duration
	MaxConns    int
	TLSEnabled  bool
	TLSCertFile string
}

// Option mutates the Config under construction. It returns an error so an option
// can validate its input and abort New.
type Option func(*Config) error

// New seeds defaults, then applies each option in order. Later options override
// earlier ones on the same field (last-writer-wins). It returns the first error.
func New(opts ...Option) (*Config, error) {
	c := &Config{
		Addr:     ":8080",
		Timeout:  30 * time.Second,
		MaxConns: 100,
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// WithAddr sets the listen address.
func WithAddr(addr string) Option {
	return func(c *Config) error {
		c.Addr = addr
		return nil
	}
}

// WithTimeout sets the request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Config) error {
		c.Timeout = d
		return nil
	}
}

// WithMaxConns sets the connection cap and validates it is positive.
func WithMaxConns(n int) Option {
	return func(c *Config) error {
		if n <= 0 {
			return fmt.Errorf("max conns must be positive, got %d", n)
		}
		c.MaxConns = n
		return nil
	}
}

// WithTLS enables TLS with a cert file, validating the path is non-empty.
func WithTLS(certFile string) Option {
	return func(c *Config) error {
		if certFile == "" {
			return fmt.Errorf("tls cert file must not be empty")
		}
		c.TLSEnabled = true
		c.TLSCertFile = certFile
		return nil
	}
}
```

### The runnable demo

The demo builds a config from defaults plus three options, then prints the resulting
fields to show defaults survive where no option overrode them.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/serverconfig"
)

func main() {
	cfg, err := serverconfig.New(
		serverconfig.WithAddr(":9090"),
		serverconfig.WithTimeout(5*time.Second),
		serverconfig.WithMaxConns(250),
	)
	if err != nil {
		fmt.Println("config error:", err)
		return
	}
	fmt.Printf("addr=%s timeout=%s maxconns=%d tls=%v\n",
		cfg.Addr, cfg.Timeout, cfg.MaxConns, cfg.TLSEnabled)

	if _, err := serverconfig.New(serverconfig.WithMaxConns(-1)); err != nil {
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
addr=:9090 timeout=5s maxconns=250 tls=false
rejected: max conns must be positive, got -1
```

### Tests

The tests pin the four properties: defaults apply with no options, each option
mutates only its field, composing two options on one field is last-writer-wins, and
a failing option aborts `New` with the error. A nil-check confirms a successful
`New` returns a usable `*Config`.

Create `config_test.go`:

```go
package serverconfig

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDefaultsWhenNoOptions(t *testing.T) {
	t.Parallel()
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("New returned a nil *Config")
	}
	if c.Addr != ":8080" || c.Timeout != 30*time.Second || c.MaxConns != 100 {
		t.Fatalf("defaults wrong: %+v", c)
	}
	if c.TLSEnabled {
		t.Fatal("TLS should default to disabled")
	}
}

func TestEachOptionMutatesItsField(t *testing.T) {
	t.Parallel()
	c, err := New(
		WithAddr(":7000"),
		WithTimeout(2*time.Second),
		WithMaxConns(42),
		WithTLS("/certs/server.pem"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":7000" {
		t.Fatalf("Addr = %q", c.Addr)
	}
	if c.Timeout != 2*time.Second {
		t.Fatalf("Timeout = %s", c.Timeout)
	}
	if c.MaxConns != 42 {
		t.Fatalf("MaxConns = %d", c.MaxConns)
	}
	if !c.TLSEnabled || c.TLSCertFile != "/certs/server.pem" {
		t.Fatalf("TLS not applied: %+v", c)
	}
}

func TestOptionsComposeLastWriterWins(t *testing.T) {
	t.Parallel()
	c, err := New(WithMaxConns(10), WithMaxConns(20))
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxConns != 20 {
		t.Fatalf("MaxConns = %d, want 20 (last writer wins)", c.MaxConns)
	}
}

func TestFailingOptionAbortsNew(t *testing.T) {
	t.Parallel()
	c, err := New(WithMaxConns(-5))
	if err == nil {
		t.Fatal("expected error from negative max conns")
	}
	if c != nil {
		t.Fatal("New must return nil *Config on error")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Fatalf("error = %v, want it to mention positivity", err)
	}
}

func TestFailingOptionStopsLaterOptions(t *testing.T) {
	t.Parallel()
	// The failing option is second; the third must never run.
	_, err := New(
		WithAddr(":1"),
		WithTLS(""), // fails
		WithMaxConns(5),
	)
	if err == nil {
		t.Fatal("expected error from empty TLS cert")
	}
}

func ExampleNew() {
	c, _ := New(WithMaxConns(250), WithTimeout(5*time.Second))
	fmt.Println(c.MaxConns, c.Timeout)
	// Output: 250 5s
}
```

## Review

The constructor is correct when a no-option `New` yields the documented defaults,
each option changes exactly its field, two options on one field resolve to the last,
and a failing option aborts with a non-nil error and a nil config. The
last-writer-wins test is the one that proves options mutate a *shared* `*Config` in
sequence rather than each building its own.

The mistakes: defining `Option` as `func(Config)` (value), which mutates a discarded
copy so nothing sticks; forgetting to seed defaults before applying options, so an
unset field is a zero value instead of the intended default; and returning a
`Config` value instead of `*Config`, which loses the shared-config-over-lifetime
property and would force copies on every read. The `error` return on `Option` is
what lets an option validate; `New` stops at the first failure so a half-built
config never escapes.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the original write-up of this pattern.
- [Uber Go Style Guide: Functional Options](https://github.com/uber-go/guide/blob/master/style.md#functional-options) — idiomatic option signatures and when to use them.
- [Rob Pike: Self-referential functions and the design of options](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html) — the design rationale.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-slice-index-address-indexer.md](05-slice-index-address-indexer.md)
