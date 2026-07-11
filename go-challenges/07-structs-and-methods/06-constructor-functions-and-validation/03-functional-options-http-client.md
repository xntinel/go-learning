# Exercise 3: Construct a Service HTTP Client with Functional Options and Constructor-Side Validation

A service client accumulates optional settings over its life — a timeout, a retry
count, a custom transport. Encoding them as positional arguments makes the
constructor rigid and the call site unreadable; encoding them as functional
options keeps defaults natural, the set order-independent, and validation in one
place. This exercise builds `NewClient(baseURL, opts...)` where each option
mutates a private struct and the constructor validates the assembled result once.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
svcclient/                   independent module: example.com/functional-options-http-client
  go.mod
  client.go                  Client, Option, NewClient, WithTimeout/WithRetries/WithHTTPClient, accessors
  cmd/
    demo/
      main.go                builds a default client and a customized one, prints their config
  client_test.go             defaults, each option observed, invalid inputs, aggregation, order independence
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: `NewClient(baseURL string, opts ...Option) (*Client, error)` plus `WithTimeout`, `WithRetries`, `WithHTTPClient`, and read-only accessors.
- Test: zero options yields documented defaults; each option is observed through an accessor; an invalid baseURL and a negative timeout return their sentinel; combined bad options aggregate; and applying the same options in two orders yields equal config.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/svcclient/cmd/demo
cd ~/go-exercises/svcclient
go mod init example.com/functional-options-http-client
```

### How the pattern works, and where validation goes

An `Option` is `func(*options)` — a function that mutates a private `options`
struct. `WithTimeout(d)` returns a closure that sets `o.timeout = d`. The
constructor starts from a struct pre-filled with defaults, applies each option in
turn, then validates the final assembled struct and builds the `*Client` from it.
Three properties fall out of this shape. Defaults are free: an option that is not
passed simply never runs, so the pre-filled value survives. Order does not matter:
each option writes a distinct field, so applying `WithTimeout` then `WithRetries`
lands in the same place as the reverse. And adding a new option later does not
break any existing call site, because the variadic signature is unchanged — the
backward-compatibility property a positional signature can never offer.

Validation happens once, after all options are applied, not inside each option.
An individual option cannot see the whole picture and validating per-option would
scatter the logic; the constructor validates the coherent final result and returns
`errors.Join` of every problem. Because the fields are private, the only way to
observe them is through accessors (`Timeout()`, `Retries()`, `BaseURL()`), which
is what keeps a constructed `Client` immutable and its invariants intact.

Create `client.go`:

```go
package svcclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

var (
	ErrInvalidBaseURL = errors.New("base URL is invalid")
	ErrInvalidTimeout = errors.New("timeout must not be negative")
	ErrInvalidRetries = errors.New("retries must not be negative")
)

const (
	defaultTimeout = 30 * time.Second
	defaultRetries = 3
)

type options struct {
	timeout    time.Duration
	retries    int
	httpClient *http.Client
}

// Option configures a Client. Each Option mutates the private options struct;
// the constructor validates the assembled result once.
type Option func(*options)

// WithTimeout sets the per-request timeout.
func WithTimeout(d time.Duration) Option {
	return func(o *options) { o.timeout = d }
}

// WithRetries sets the number of retry attempts.
func WithRetries(n int) Option {
	return func(o *options) { o.retries = n }
}

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(o *options) { o.httpClient = hc }
}

// Client is a validated service HTTP client. Its configuration is immutable
// after construction and readable only through accessors.
type Client struct {
	baseURL    *url.URL
	timeout    time.Duration
	retries    int
	httpClient *http.Client
}

// NewClient assembles a Client from baseURL and opts, applying defaults and
// validating the result. It returns errors.Join of every problem.
func NewClient(baseURL string, opts ...Option) (*Client, error) {
	o := options{
		timeout:    defaultTimeout,
		retries:    defaultRetries,
		httpClient: http.DefaultClient,
	}
	for _, opt := range opts {
		opt(&o)
	}

	var errs []error

	u, err := url.Parse(baseURL)
	if err != nil {
		errs = append(errs, fmt.Errorf("%w: %v", ErrInvalidBaseURL, err))
	} else if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		errs = append(errs, fmt.Errorf("%w: %q must be an absolute http(s) URL", ErrInvalidBaseURL, baseURL))
	}
	if o.timeout < 0 {
		errs = append(errs, fmt.Errorf("%w: %s", ErrInvalidTimeout, o.timeout))
	}
	if o.retries < 0 {
		errs = append(errs, fmt.Errorf("%w: %d", ErrInvalidRetries, o.retries))
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return &Client{
		baseURL:    u,
		timeout:    o.timeout,
		retries:    o.retries,
		httpClient: o.httpClient,
	}, nil
}

// BaseURL returns the canonical base URL string.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Timeout returns the configured per-request timeout.
func (c *Client) Timeout() time.Duration { return c.timeout }

// Retries returns the configured retry count.
func (c *Client) Retries() int { return c.retries }

// HTTPClient returns the underlying *http.Client.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }
```

### The runnable demo

The demo builds a default client and a customized one, printing each config
through the accessors so you can see the defaults and the overrides.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/functional-options-http-client"
)

func main() {
	def, _ := svcclient.NewClient("https://api.example.com")
	fmt.Printf("default: url=%s timeout=%s retries=%d\n",
		def.BaseURL(), def.Timeout(), def.Retries())

	tuned, _ := svcclient.NewClient("https://api.example.com",
		svcclient.WithTimeout(5*time.Second),
		svcclient.WithRetries(1),
	)
	fmt.Printf("tuned:   url=%s timeout=%s retries=%d\n",
		tuned.BaseURL(), tuned.Timeout(), tuned.Retries())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
default: url=https://api.example.com timeout=30s retries=3
tuned:   url=https://api.example.com timeout=5s retries=1
```

### Tests

`TestOrderIndependent` is the property that justifies the pattern: the same set of
options applied in two different orders must produce equal configuration. The
error tests confirm the constructor validates the assembled result and aggregates
failures rather than returning on the first.

Create `client_test.go`:

```go
package svcclient

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	t.Parallel()
	c, err := NewClient("https://api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 30*time.Second || c.Retries() != 3 {
		t.Fatalf("defaults wrong: timeout=%s retries=%d", c.Timeout(), c.Retries())
	}
	if c.HTTPClient() != http.DefaultClient {
		t.Fatal("default http client not applied")
	}
}

func TestOptionsObserved(t *testing.T) {
	t.Parallel()
	hc := &http.Client{}
	c, err := NewClient("https://api.example.com",
		WithTimeout(2*time.Second),
		WithRetries(5),
		WithHTTPClient(hc),
	)
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 2*time.Second || c.Retries() != 5 || c.HTTPClient() != hc {
		t.Fatalf("options not observed: %+v", c)
	}
}

func TestInvalidInputs(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		url  string
		opts []Option
		want error
	}{
		"relative url":     {"/just/a/path", nil, ErrInvalidBaseURL},
		"missing scheme":   {"api.example.com", nil, ErrInvalidBaseURL},
		"bad url":          {"http://[::1", nil, ErrInvalidBaseURL},
		"negative timeout": {"https://x.io", []Option{WithTimeout(-time.Second)}, ErrInvalidTimeout},
		"negative retries": {"https://x.io", []Option{WithRetries(-1)}, ErrInvalidRetries},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, err := NewClient(tc.url, tc.opts...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if c != nil {
				t.Fatal("client should be nil on error")
			}
		})
	}
}

func TestAggregatesBadOptions(t *testing.T) {
	t.Parallel()
	_, err := NewClient("nota url", WithTimeout(-1), WithRetries(-1))
	for _, want := range []error{ErrInvalidBaseURL, ErrInvalidTimeout, ErrInvalidRetries} {
		if !errors.Is(err, want) {
			t.Fatalf("joined err %v should include %v", err, want)
		}
	}
}

func TestOrderIndependent(t *testing.T) {
	t.Parallel()
	a, err := NewClient("https://x.io", WithTimeout(2*time.Second), WithRetries(4))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewClient("https://x.io", WithRetries(4), WithTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if a.Timeout() != b.Timeout() || a.Retries() != b.Retries() {
		t.Fatalf("order changed config: a=%+v b=%+v", a, b)
	}
}

func ExampleNewClient() {
	c, _ := NewClient("https://api.example.com", WithTimeout(5*time.Second))
	fmt.Printf("%s %s\n", c.BaseURL(), c.Timeout())
	// Output: https://api.example.com 5s
}
```

## Review

The client is correct when zero options yield the documented defaults, each option
is observable through an accessor, and the same options in any order produce equal
configuration. The pattern's payoff is exactly that order independence plus
backward-compatible growth: a new `WithX` never breaks a caller. The validation
discipline is to check the assembled result once and aggregate — not per-option,
which cannot see the whole config, and not fail-fast, which hides later problems.
Keeping the fields unexported and exposing only accessors is what makes a built
`Client` immutable, so its validated invariants cannot be mutated away.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — the canonical writeup of the pattern.
- [net/url.Parse](https://pkg.go.dev/net/url#Parse) — parsing and validating the base URL.
- [net/http.Client](https://pkg.go.dev/net/http#Client) — the transport the option overrides.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-env-config-loader-defaults.md](02-env-config-loader-defaults.md) | Next: [04-parse-dont-validate-primitives.md](04-parse-dont-validate-primitives.md)
