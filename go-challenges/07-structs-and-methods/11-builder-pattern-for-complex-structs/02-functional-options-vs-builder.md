# Exercise 2: Functional Options for an HTTP Client Constructor

The mutable builder is one answer to "many optional fields". Go's more idiomatic
answer for a *constructor* is functional options: one required argument plus a
variadic bag of `With*` closures. This module builds an HTTP client constructor
that way and shows why options are composable, reusable, and order-independent for
independent fields.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
optclient/                  independent module: example.com/optclient
  go.mod                    go 1.26
  client.go                 package optclient: Client, Option, NewClient, With* options
  cmd/
    demo/
      main.go               runnable demo: defaults, then a reused []Option slice
  client_test.go            table tests for defaults, each option, precedence, reuse
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: `NewClient(baseURL string, opts ...Option) (*Client, error)` with `type Option func(*config)`, plus `WithTimeout`, `WithRetries`, `WithUserAgent`, `WithTransport`. Defaults are applied first, then options left-to-right, then validation runs once.
- Test: no options yields defaults; each `With*` sets its field; a later `WithTimeout` wins over an earlier one; an invalid `baseURL` errors via `url.Parse`; one reusable `[]Option` builds two independent clients.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/optclient/cmd/demo
cd ~/go-exercises/optclient
go mod init example.com/optclient
```

### Why options instead of a builder here

A constructor has exactly one shape that options fit perfectly: a required
argument (the base URL) plus a bag of optionals. The signature
`NewClient(base, ...Option)` never has to change when you add a new knob — you
add a `WithX` function and every existing call site still compiles. Compare that
to widening a positional constructor (`NewClient(base, timeout, retries, ua, ...)`),
which breaks every caller each time it grows, or to a mutable builder, which reads
well for a long chain but cannot travel as a value. An `Option` is a first-class
value: you can build a `[]Option{WithTimeout(...), WithUserAgent(...)}` once and
reuse it to construct two independent clients, which is exactly how a service wires
a shared default profile and then customizes per dependency.

The construction order matters and is fixed by the constructor, not the caller.
`NewClient` starts from a `config` pre-filled with defaults, applies each option in
argument order, then validates once. Because defaults come first, the zero-option
call `NewClient(base)` already yields a valid client. Because options run
left-to-right, two options that touch the *same* field compose as "last wins" —
`WithTimeout(1s), WithTimeout(2s)` yields 2s — while options that touch
*independent* fields are order-independent. Validation lives after all options, so
it sees the final resolved config, never a half-applied one.

Create `client.go`:

```go
package optclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ErrInvalidBaseURL wraps any base-URL parse or scheme failure so callers can
// match it with errors.Is.
var ErrInvalidBaseURL = errors.New("invalid base URL")

// config is the private accumulation target. Only NewClient and the Option
// closures touch it; it never escapes.
type config struct {
	timeout   time.Duration
	retries   int
	userAgent string
	transport http.RoundTripper
}

// Option mutates the config. It is a first-class value: composable, reusable,
// and order-independent for independent fields.
type Option func(*config)

func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d }
}

func WithRetries(n int) Option {
	return func(c *config) { c.retries = n }
}

func WithUserAgent(ua string) Option {
	return func(c *config) { c.userAgent = ua }
}

func WithTransport(rt http.RoundTripper) Option {
	return func(c *config) { c.transport = rt }
}

// Client is the fully-owned product. Fields are unexported; exported accessors
// give cmd/demo read access without exposing mutable state.
type Client struct {
	base      *url.URL
	http      *http.Client
	retries   int
	userAgent string
}

func (c *Client) BaseURL() string        { return c.base.String() }
func (c *Client) Timeout() time.Duration { return c.http.Timeout }
func (c *Client) Retries() int           { return c.retries }
func (c *Client) UserAgent() string      { return c.userAgent }

// NewClient applies defaults, then the options in order, then validates once.
func NewClient(baseURL string, opts ...Option) (*Client, error) {
	cfg := config{
		timeout:   30 * time.Second,
		retries:   0,
		userAgent: "optclient/1.0",
		transport: http.DefaultTransport,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidBaseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q", ErrInvalidBaseURL, u.Scheme)
	}
	if cfg.retries < 0 {
		return nil, fmt.Errorf("%w: retries %d", ErrInvalidBaseURL, cfg.retries)
	}

	return &Client{
		base:      u,
		http:      &http.Client{Timeout: cfg.timeout, Transport: cfg.transport},
		retries:   cfg.retries,
		userAgent: cfg.userAgent,
	}, nil
}
```

### The runnable demo

The demo builds a default client, then reuses one `[]Option` slice to build two
independent clients, proving options travel as values.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/optclient"
)

func main() {
	def, _ := optclient.NewClient("https://api.example.com")
	fmt.Printf("default: timeout=%s retries=%d ua=%s\n",
		def.Timeout(), def.Retries(), def.UserAgent())

	profile := []optclient.Option{
		optclient.WithTimeout(2 * time.Second),
		optclient.WithRetries(3),
		optclient.WithUserAgent("checkout/2.1"),
	}
	a, _ := optclient.NewClient("https://a.example.com", profile...)
	b, _ := optclient.NewClient("https://b.example.com", profile...)
	fmt.Printf("a: %s timeout=%s retries=%d\n", a.BaseURL(), a.Timeout(), a.Retries())
	fmt.Printf("b: %s timeout=%s retries=%d\n", b.BaseURL(), b.Timeout(), b.Retries())

	if _, err := optclient.NewClient("://nope"); err != nil {
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
default: timeout=30s retries=0 ua=optclient/1.0
a: https://a.example.com timeout=2s retries=3
b: https://b.example.com timeout=2s retries=3
rejected: invalid base URL: parse "://nope": missing protocol scheme
```

### Tests

The table proves the four contracts: defaults on the zero-option call, each option
setting its field, last-wins composition when two options touch the same field, and
a reusable slice building two independent clients. A separate test asserts the
invalid-URL path matches `ErrInvalidBaseURL`.

Create `client_test.go`:

```go
package optclient

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestNewClientDefaults(t *testing.T) {
	t.Parallel()

	c, err := NewClient("https://api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 30*time.Second {
		t.Errorf("timeout = %s, want 30s", c.Timeout())
	}
	if c.Retries() != 0 {
		t.Errorf("retries = %d, want 0", c.Retries())
	}
	if c.UserAgent() != "optclient/1.0" {
		t.Errorf("userAgent = %q, want optclient/1.0", c.UserAgent())
	}
}

func TestNewClientEachOption(t *testing.T) {
	t.Parallel()

	c, err := NewClient("https://api.example.com",
		WithTimeout(5*time.Second),
		WithRetries(4),
		WithUserAgent("svc/9"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 5*time.Second {
		t.Errorf("timeout = %s, want 5s", c.Timeout())
	}
	if c.Retries() != 4 {
		t.Errorf("retries = %d, want 4", c.Retries())
	}
	if c.UserAgent() != "svc/9" {
		t.Errorf("userAgent = %q, want svc/9", c.UserAgent())
	}
}

func TestOptionsLastWins(t *testing.T) {
	t.Parallel()

	c, err := NewClient("https://api.example.com",
		WithTimeout(1*time.Second),
		WithTimeout(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 2*time.Second {
		t.Errorf("timeout = %s, want 2s (last option wins)", c.Timeout())
	}
}

func TestInvalidBaseURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		base string
	}{
		{"bad scheme", "ftp://example.com"},
		{"unparseable", "://nope"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewClient(tc.base); !errors.Is(err, ErrInvalidBaseURL) {
				t.Fatalf("NewClient(%q) err = %v, want ErrInvalidBaseURL", tc.base, err)
			}
		})
	}
}

func TestReusableOptionSlice(t *testing.T) {
	t.Parallel()

	profile := []Option{WithTimeout(7 * time.Second), WithRetries(2)}
	a, err := NewClient("https://a.example.com", profile...)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewClient("https://b.example.com", profile...)
	if err != nil {
		t.Fatal(err)
	}
	if a.BaseURL() == b.BaseURL() {
		t.Fatal("clients should have different base URLs")
	}
	if a.Timeout() != 7*time.Second || b.Timeout() != 7*time.Second {
		t.Fatalf("both should share the profile timeout: a=%s b=%s", a.Timeout(), b.Timeout())
	}
}

func ExampleNewClient() {
	c, _ := NewClient("https://example.com", WithRetries(3))
	fmt.Println(c.BaseURL(), c.Retries())
	// Output: https://example.com 3
}
```

## Review

Options are correct when the zero-option call is already valid and every knob is a
composable value. `TestNewClientDefaults` proves defaults are applied before any
option runs, which is what makes `NewClient(base)` valid. `TestOptionsLastWins`
proves the left-to-right apply order, so two options on the same field compose
predictably. `TestReusableOptionSlice` proves an `[]Option` is a reusable
first-class value that constructs independent clients — the property a mutable
builder does not have. Validation runs once, after all options, so `TestInvalidBaseURL`
sees the fully-resolved config and matches `ErrInvalidBaseURL` via `errors.Is`
because the constructor wraps with `%w`. The trade-off to remember: a builder reads
better for a long fluent chain and can enforce staging; options extend without
breaking the signature and travel as values.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the canonical write-up of this pattern.
- [net/http.Client](https://pkg.go.dev/net/http#Client) — the `Timeout` and `Transport` fields the constructor fills.
- [net/url.Parse](https://pkg.go.dev/net/url#Parse) — the base-URL validation the constructor runs once.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-immutable-value-builder-fork.md](03-immutable-value-builder-fork.md)
