# Exercise 5: A NewHTTPClient Constructor That Rejects Invalid States

The cheapest place to stop an invalid object is the constructor. A client, pool,
or server built from a bad configuration should never come into existence — better
one guarded `(nil, error)` return than a half-built client that fails
unpredictably three calls later. This exercise builds `NewHTTPClient`, which
validates its base URL and timeout and refuses to produce a client from anything
malformed.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
clientctor/                  independent module: example.com/clientctor
  go.mod                     go 1.26
  client.go                  ClientConfig; Client; NewHTTPClient; BaseURL/Timeout accessors
  cmd/
    demo/
      main.go                runnable demo: one valid build, one rejected config
  client_test.go             invalid-config table + valid case + never (client,err)
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: `NewHTTPClient(cfg ClientConfig) (*Client, error)` that returns `(nil, error)` unless `baseURL` parses to an absolute `http`/`https` URL and `timeout > 0`.
- Test: a table of invalid configs (empty, relative, non-http scheme, unparseable, zero/negative timeout) each asserting a nil `*Client` and a specific error; a valid config asserting a non-nil client and nil error; assert the constructor never returns a non-nil client together with a non-nil error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/clientctor/cmd/demo
cd ~/go-exercises/clientctor
go mod init example.com/clientctor
```

### Make invalid states unrepresentable at the boundary

A client that holds a relative base URL is a latent bug: it will build fine, sit
in a struct, and only blow up when someone tries to resolve a request path against
it. The constructor is where that whole class of failure collapses to a single
guarded line. `NewHTTPClient` validates before it allocates, and on any invalid
input returns `(nil, error)` — so the rest of the program can assume that a
non-nil `*Client` is a *valid* client, always. That assumption is the entire value
of the pattern: downstream code never re-checks the base URL because a client
could not have been built with a bad one.

The base-URL validation has three independent guards. `url.Parse` catches
syntactically broken input (an unterminated IPv6 host, a stray control byte).
`u.IsAbs()` catches a relative URL like `api/v1` that has no scheme — a client
needs an absolute base to resolve paths against. The scheme check rejects
`ftp://` or `file://`: an HTTP client speaks HTTP. Each guard returns a distinct,
named error so the caller knows exactly what was wrong. The timeout guard rejects
non-positive durations, since a zero timeout means "no deadline" — the opposite of
what a caller setting a timeout wants.

The one invariant that ties it together: the constructor never returns a non-nil
client *and* a non-nil error. Either it built a valid client (`client, nil`) or it
refused (`nil, err`). A caller must be able to trust that a non-nil error means
"nothing was built"; returning a half-built client alongside an error would put
the "which do I trust?" decision back on every caller.

Create `client.go`:

```go
package clientctor

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// ClientConfig is the raw, unvalidated input to the constructor.
type ClientConfig struct {
	BaseURL string
	Timeout time.Duration
}

// Client is a validated HTTP client. Its fields are unexported so the only way
// to obtain one is through NewHTTPClient, which guarantees they are valid.
type Client struct {
	baseURL string
	timeout time.Duration
}

// BaseURL and Timeout expose the validated configuration for callers and demos.
func (c *Client) BaseURL() string        { return c.baseURL }
func (c *Client) Timeout() time.Duration { return c.timeout }

// NewHTTPClient validates cfg and returns a ready client, or (nil, error) if the
// base URL is not an absolute http/https URL or the timeout is not positive. It
// never returns a non-nil client together with a non-nil error.
func NewHTTPClient(cfg ClientConfig) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("baseURL is required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse baseURL %q: %w", cfg.BaseURL, err)
	}
	if !u.IsAbs() {
		return nil, fmt.Errorf("baseURL %q must be absolute", cfg.BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("baseURL scheme %q must be http or https", u.Scheme)
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive, got %s", cfg.Timeout)
	}
	return &Client{baseURL: cfg.BaseURL, timeout: cfg.Timeout}, nil
}
```

### The runnable demo

The demo builds one valid client and prints its settings, then attempts one
invalid config to show the `(nil, error)` refusal.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/clientctor"
)

func main() {
	c, err := clientctor.NewHTTPClient(clientctor.ClientConfig{
		BaseURL: "https://api.example.com",
		Timeout: 5 * time.Second,
	})
	if err != nil {
		fmt.Println("build failed:", err)
		return
	}
	fmt.Printf("built client: base=%s timeout=%s\n", c.BaseURL(), c.Timeout())

	_, err = clientctor.NewHTTPClient(clientctor.ClientConfig{
		BaseURL: "api/v1",
		Timeout: 5 * time.Second,
	})
	fmt.Println("relative base:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
built client: base=https://api.example.com timeout=5s
relative base: baseURL "api/v1" must be absolute
```

### Tests

The invalid-config table has one row per guard, each asserting a nil `*Client` and
an error whose message identifies the problem. The valid case asserts a non-nil
client with the configured settings and a nil error. A dedicated test asserts the
never-both invariant across every row.

Create `client_test.go`:

```go
package clientctor

import (
	"strings"
	"testing"
	"time"
)

func TestNewHTTPClientRejectsInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     ClientConfig
		wantSub string
	}{
		{"empty baseURL", ClientConfig{BaseURL: "", Timeout: time.Second}, "required"},
		{"relative URL", ClientConfig{BaseURL: "api/v1", Timeout: time.Second}, "absolute"},
		{"non-http scheme", ClientConfig{BaseURL: "ftp://host/x", Timeout: time.Second}, "scheme"},
		{"unparseable URL", ClientConfig{BaseURL: "http://[::1", Timeout: time.Second}, "parse baseURL"},
		{"zero timeout", ClientConfig{BaseURL: "https://host", Timeout: 0}, "timeout"},
		{"negative timeout", ClientConfig{BaseURL: "https://host", Timeout: -time.Second}, "timeout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c, err := NewHTTPClient(tt.cfg)
			if err == nil {
				t.Fatalf("NewHTTPClient(%+v) = nil error, want failure", tt.cfg)
			}
			if c != nil {
				t.Fatalf("client on error path = %+v, want nil", c)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %q, want it to contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestNewHTTPClientAcceptsValid(t *testing.T) {
	t.Parallel()

	c, err := NewHTTPClient(ClientConfig{BaseURL: "https://api.example.com", Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("NewHTTPClient = %v, want nil", err)
	}
	if c == nil {
		t.Fatal("client = nil on valid config")
	}
	if c.BaseURL() != "https://api.example.com" || c.Timeout() != 3*time.Second {
		t.Fatalf("client = %+v, want base and 3s timeout", c)
	}
}

// TestNeverClientAndError pins the invariant: no input yields both a non-nil
// client and a non-nil error.
func TestNeverClientAndError(t *testing.T) {
	t.Parallel()

	cfgs := []ClientConfig{
		{BaseURL: "", Timeout: time.Second},
		{BaseURL: "api/v1", Timeout: time.Second},
		{BaseURL: "https://ok", Timeout: 0},
		{BaseURL: "https://ok", Timeout: time.Second},
	}
	for _, cfg := range cfgs {
		c, err := NewHTTPClient(cfg)
		if c != nil && err != nil {
			t.Fatalf("both client and error for %+v", cfg)
		}
		if c == nil && err == nil {
			t.Fatalf("neither client nor error for %+v", cfg)
		}
	}
}
```

## Review

The constructor is correct when a non-nil `*Client` is always a valid one and a
non-nil error always means nothing was built. The invalid table proves each guard
independently; the never-both test proves the exclusivity invariant that lets
callers stop re-validating. Note the base-URL checks are ordered from cheapest and
most-fundamental (empty, unparseable) to most-specific (scheme), so the first
failure a caller sees is the most actionable.

The mistakes to avoid: returning a partially-built client alongside an error (the
caller cannot tell which to trust), and validating *after* allocation so an invalid
object briefly exists and might escape through an early return. Validate first,
allocate last. Keep the fields unexported so the constructor is the only door in.

## Resources

- [pkg.go.dev: net/url.Parse](https://pkg.go.dev/net/url#Parse) and [URL.IsAbs](https://pkg.go.dev/net/url#URL.IsAbs) — parsing and the absolute-URL check.
- [pkg.go.dev: time.Duration](https://pkg.go.dev/time#Duration) — the timeout type and its zero value semantics.
- [Effective Go: Errors](https://go.dev/doc/effective_go#errors) — returning errors from constructors instead of building invalid values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-cache-miss-error-value.md](04-cache-miss-error-value.md) | Next: [06-request-validation-guard-clauses.md](06-request-validation-guard-clauses.md)
