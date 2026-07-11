# Exercise 2: Functional Options for a Server/Client Constructor

The most common constructor pattern in production Go clients and servers is the
functional-options pattern: `New(opts ...Option)` where each `Option` is a function
that mutates a private config. This module builds an HTTP-client config constructor
that way, and proves the property that makes the pattern worth it: adding an option
next year breaks no existing call site.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
httpclient/                 independent module: example.com/httpclient
  go.mod                    go 1.26
  client.go                 Config, Option, New, With* options, validation
  cmd/
    demo/
      main.go               runnable demo: defaults, subset, last-writer-wins
  client_test.go            defaults, subset, order, validation-error tests
```

Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
Implement: `type Option func(*Config)`, `New(opts ...Option) (*Client, error)` with defaults set first and one validation pass, and `WithTimeout`, `WithMaxConns`, `WithLogger`, `WithBaseURL`.
Test: zero options yields defaults; a subset changes only those fields; last-writer-wins for the same field; an out-of-range timeout or empty base URL returns a wrapped sentinel error; a brand-new option compiles against old call sites.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/httpclient/cmd/demo
cd ~/go-exercises/httpclient
go mod init example.com/httpclient
```

### Why options beat a giant struct literal

Imagine the alternative: an exported `Config` struct that every caller fills with a
literal. The day you add a `MaxConns` field, every call site either silently gets the
zero value (a subtle bug — zero connections) or must be edited. Required-versus-
optional is invisible: nothing tells a caller that `BaseURL` is mandatory but
`Timeout` has a default. And the call site `Config{30 * time.Second, 100, nil, "..."}`
with positional fields is unreadable.

Functional options fix all three. `New` sets sane defaults first, then applies each
option in order, then validates the assembled config exactly once. A call site reads
as `New(WithTimeout(5*time.Second), WithBaseURL("https://api.example.com"))` — self-
documenting, only the non-default fields mentioned. Adding `WithRetries` next year is
purely additive: no existing caller changes, because options are variadic. That
backward compatibility is why gRPC, the AWS SDK, and database drivers all use this
pattern.

Three details make it correct. Defaults go *before* the option loop, so an option can
override a default but an unspecified field still has one. Options apply in call
order, so two options that touch the same field are last-writer-wins — a property
callers can rely on and that a test should pin. Validation runs *once*, at the end,
on the fully assembled config, not inside each option — an individual option cannot
know whether a later option will fix an interim invalid state.

Create `client.go`:

```go
package httpclient

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// ErrInvalidConfig is the sentinel wrapping every validation failure, so callers
// can match with errors.Is.
var ErrInvalidConfig = errors.New("invalid client config")

// Config holds the assembled client configuration. It is unexported-field-only;
// callers build it through options, not a struct literal.
type Config struct {
	timeout  time.Duration
	maxConns int
	logger   *slog.Logger
	baseURL  string
}

// Option mutates a Config. It is the unit of the functional-options pattern.
type Option func(*Config)

// Client is the assembled artifact. Exported accessors expose config for tests
// and cmd/demo, which live in a different package.
type Client struct {
	cfg Config
}

func (c *Client) Timeout() time.Duration { return c.cfg.timeout }
func (c *Client) MaxConns() int          { return c.cfg.maxConns }
func (c *Client) BaseURL() string        { return c.cfg.baseURL }
func (c *Client) Logger() *slog.Logger   { return c.cfg.logger }

func defaultConfig() Config {
	return Config{
		timeout:  30 * time.Second,
		maxConns: 100,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		baseURL:  "",
	}
}

// WithTimeout sets the per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Config) { c.timeout = d }
}

// WithMaxConns caps the connection pool size.
func WithMaxConns(n int) Option {
	return func(c *Config) { c.maxConns = n }
}

// WithLogger installs a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Config) { c.logger = l }
}

// WithBaseURL sets the required base URL for every request.
func WithBaseURL(u string) Option {
	return func(c *Config) { c.baseURL = u }
}

// New assembles a Client: defaults first, options in call order, one validation.
func New(opts ...Option) (*Client, error) {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Client{cfg: cfg}, nil
}

func (c Config) validate() error {
	if c.baseURL == "" {
		return fmt.Errorf("%w: base URL is required", ErrInvalidConfig)
	}
	if c.timeout <= 0 {
		return fmt.Errorf("%w: timeout must be positive, got %s", ErrInvalidConfig, c.timeout)
	}
	if c.maxConns <= 0 {
		return fmt.Errorf("%w: maxConns must be positive, got %d", ErrInvalidConfig, c.maxConns)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/httpclient"
)

func main() {
	// Defaults plus the one required field.
	c, err := httpclient.New(httpclient.WithBaseURL("https://api.example.com"))
	if err != nil {
		panic(err)
	}
	fmt.Printf("defaults: timeout=%s maxConns=%d\n", c.Timeout(), c.MaxConns())

	// A subset of options; last-writer-wins on timeout.
	c2, _ := httpclient.New(
		httpclient.WithBaseURL("https://api.example.com"),
		httpclient.WithTimeout(10*time.Second),
		httpclient.WithTimeout(5*time.Second),
		httpclient.WithMaxConns(20),
	)
	fmt.Printf("custom:   timeout=%s maxConns=%d\n", c2.Timeout(), c2.MaxConns())

	// A validation failure: no base URL.
	if _, err := httpclient.New(); err != nil {
		fmt.Println("error:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
defaults: timeout=30s maxConns=100
custom:   timeout=5s maxConns=20
error: invalid client config: base URL is required
```

### Tests

The tests assert each property that justifies the pattern. `TestDefaults` builds with
only the required option and checks every default. `TestSubsetChangesOnlyThose` proves
an option touches exactly its field. `TestLastWriterWins` pins call-order semantics.
`TestValidation` is table-driven over invalid combinations, matching the sentinel with
`errors.Is`. `TestNewOptionKeepsOldCallSites` is the API-stability proof: it adds a
`withUserAgent` option and shows an old-style call still compiles and runs.

Create `client_test.go`:

```go
package httpclient

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	c, err := New(WithBaseURL("https://x"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Timeout() != 30*time.Second {
		t.Errorf("timeout = %s, want 30s", c.Timeout())
	}
	if c.MaxConns() != 100 {
		t.Errorf("maxConns = %d, want 100", c.MaxConns())
	}
	if c.Logger() == nil {
		t.Error("logger is nil; default should be a discard logger")
	}
}

func TestSubsetChangesOnlyThose(t *testing.T) {
	t.Parallel()
	c, err := New(WithBaseURL("https://x"), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Timeout() != 5*time.Second {
		t.Errorf("timeout = %s, want 5s", c.Timeout())
	}
	if c.MaxConns() != 100 {
		t.Errorf("maxConns changed to %d; only timeout was set", c.MaxConns())
	}
}

func TestLastWriterWins(t *testing.T) {
	t.Parallel()
	c, err := New(
		WithBaseURL("https://x"),
		WithTimeout(10*time.Second),
		WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Timeout() != 2*time.Second {
		t.Errorf("timeout = %s, want 2s (last writer)", c.Timeout())
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts []Option
	}{
		{"empty base URL", []Option{WithTimeout(time.Second)}},
		{"zero timeout", []Option{WithBaseURL("https://x"), WithTimeout(0)}},
		{"negative maxConns", []Option{WithBaseURL("https://x"), WithMaxConns(-1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tc.opts...)
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("err = %v, want wrap of ErrInvalidConfig", err)
			}
		})
	}
}

// TestNewOptionKeepsOldCallSites proves API stability: a new option is added and
// an existing call site compiles and runs unchanged.
func TestNewOptionKeepsOldCallSites(t *testing.T) {
	t.Parallel()
	// A hypothetical new option added in a later release.
	withUserAgent := func(ua string) Option {
		return func(c *Config) { _ = ua } // would set a new field in reality
	}
	// The old call site — no user agent — still valid.
	if _, err := New(WithBaseURL("https://x")); err != nil {
		t.Fatalf("old call site broke: %v", err)
	}
	// The new call site can add it.
	if _, err := New(WithBaseURL("https://x"), withUserAgent("svc/1.0")); err != nil {
		t.Fatalf("new call site: %v", err)
	}
}

func ExampleNew() {
	c, _ := New(WithBaseURL("https://api.example.com"), WithMaxConns(50))
	fmt.Printf("%s %d\n", c.Timeout(), c.MaxConns())
	// Output: 30s 50
}
```

## Review

The pattern is correct when three orderings hold: defaults are set before any option
runs (so an unset field still has a sane value), options apply in call order (so
last-writer-wins for the same field, which `TestLastWriterWins` pins), and validation
runs once on the assembled config (so an interim invalid state a later option would
have fixed is not rejected). The validation errors wrap `ErrInvalidConfig` with `%w`,
so `errors.Is` matches — do not return a bare `errors.New` string, or callers cannot
branch on the failure class. Keep `Config`'s fields unexported and expose exported
accessors: the demo and tests live in a different package for the demo, and forcing
access through methods is what stops a caller from bypassing the constructor. The
API-stability test is the whole point of the pattern; if adding an option ever forced
an edit to an existing call, you would have reinvented the struct literal.

## Resources

- [Self-referential functions and the design of options (Rob Pike)](https://commandcenter.blogspot.com/2014/01/self-referential-functions-and-design.html)
- [Functional options for friendly APIs (Dave Cheney)](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [net/http.Server](https://pkg.go.dev/net/http#Server)
- [errors.Is and %w wrapping](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-comparator-sort-pipeline.md](01-comparator-sort-pipeline.md) | Next: [03-http-middleware-chain.md](03-http-middleware-chain.md)
