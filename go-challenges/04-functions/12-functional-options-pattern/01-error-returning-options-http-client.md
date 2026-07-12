# Exercise 1: HTTP API Client with Error-Returning Options

An HTTP API client is the archetypal place functional options belong: stable
defaults (a timeout, a retryable-status set, a user agent), optional overrides,
strict validation, and an injectable `*http.Client` so tests can point it at a
local server. This module builds that client with error-returning options and a
constructor that is the single validation boundary.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports another
exercise.

## What you'll build

```text
apiclient/                       independent module: example.com/apiclient
  go.mod                         go 1.26
  internal/httpx/
    client.go                    Client, Option func(*Client) error, New, WithHTTPClient,
                                 WithTimeout, WithUserAgent, WithRetryStatus, Request, Do, ShouldRetry
    client_test.go               httptest server, invalid-config table, no-mutation, retry-set tests
  cmd/
    demo/
      main.go                    runnable demo against a local httptest server
```

- Files: `internal/httpx/client.go`, `cmd/demo/main.go`, `internal/httpx/client_test.go`.
- Implement: a `Client` built by `New(baseURL, opts...) (*Client, error)` that seeds defaults, applies validating options, and defensively copies the `*http.Client`.
- Test: an `httptest.NewServer` round trip asserting the User-Agent and a retryable 502; a table of invalid configurations asserted with `errors.Is`; a proof the caller's client is not mutated; a proof `WithRetryStatus` sets exactly the given codes.
- Verify: `go test -count=1 ./...`

### Why error-returning options here

The client has four options and every one of them can be given invalid input:
`WithHTTPClient(nil)`, `WithTimeout(0)`, `WithUserAgent("  ")`, and
`WithRetryStatus(42)` are all mistakes a caller can make. A total
`func(*Client)` option type would have no way to report them, so the bad value
would survive into the client and fail much later, far from its cause. Using
`type Option func(*Client) error` keeps `New` the one place invalid
configuration is caught, and the caller has exactly one thing to handle:
`(*Client, error)`.

The constructor follows the lifecycle contract exactly. It parses and validates
the base URL, seeds defaults (a 2-second timeout, a default retryable set of the
transient 5xx codes plus 429, a development user agent), then applies the options
in order, returning `(nil, err)` the instant any option fails. There is no
half-built client that can escape.

### The defensive copy

`WithHTTPClient` and `WithTimeout` both illustrate the aliasing trap. If
`WithHTTPClient` stored the caller's `*http.Client` directly and `WithTimeout`
then set `Timeout` on it, the caller's own client — which they may still be using
elsewhere — would silently acquire a new timeout. Both options instead shallow-copy
the `http.Client` struct before mutating a field and store the copy. A shallow
copy is correct here: the field being changed is the value field `Timeout`, and
the shared `Transport` pointer is intentionally left shared so connection pooling
still works.

### Request building

`Request` resolves a possibly-relative path against the base URL with
`url.URL.ResolveReference`, which is the correct way to join a base and a path
(it handles absolute paths, query strings, and `..` the way a browser would).
`Do` sends it through the configured client, and `ShouldRetry` simply consults
the retryable set — a caller decides what to do with a retryable status, the
client only classifies.

Create `internal/httpx/client.go`:

```go
package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNilHTTPClient is returned by WithHTTPClient when given a nil client.
var ErrNilHTTPClient = errors.New("http client is nil")

// Client is a small HTTP API client with a fixed base URL, a user agent, and a
// set of HTTP status codes it classifies as retryable.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	userAgent  string
	retryable  map[int]bool
}

// Option configures a Client during construction and may reject invalid input.
type Option func(*Client) error

// New builds a Client for baseURL, applying opts after seeding defaults. It is
// the single validation boundary: any option error aborts construction.
func New(baseURL string, opts ...Option) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("base URL must include scheme and host: %q", baseURL)
	}

	c := &Client{
		baseURL:    parsed,
		httpClient: &http.Client{Timeout: 2 * time.Second},
		userAgent:  "apiclient/dev",
		retryable: map[int]bool{
			http.StatusTooManyRequests:    true,
			http.StatusBadGateway:         true,
			http.StatusServiceUnavailable: true,
			http.StatusGatewayTimeout:     true,
		},
	}

	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// WithHTTPClient replaces the underlying client, shallow-copying it so later
// mutations do not touch the caller's client.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(c *Client) error {
		if httpClient == nil {
			return ErrNilHTTPClient
		}
		clone := *httpClient
		c.httpClient = &clone
		return nil
	}
}

// WithTimeout sets the per-request timeout. It clones the current client so the
// caller's client (if injected) is never mutated.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) error {
		if timeout <= 0 {
			return fmt.Errorf("timeout must be positive, got %s", timeout)
		}
		clone := *c.httpClient
		clone.Timeout = timeout
		c.httpClient = &clone
		return nil
	}
}

// WithUserAgent sets the User-Agent header, trimming and rejecting empties.
func WithUserAgent(userAgent string) Option {
	return func(c *Client) error {
		userAgent = strings.TrimSpace(userAgent)
		if userAgent == "" {
			return fmt.Errorf("user agent is required")
		}
		c.userAgent = userAgent
		return nil
	}
}

// WithRetryStatus replaces the retryable status set with exactly the given
// codes, each validated to the 100-599 range.
func WithRetryStatus(statusCodes ...int) Option {
	return func(c *Client) error {
		retryable := make(map[int]bool, len(statusCodes))
		for _, code := range statusCodes {
			if code < 100 || code > 599 {
				return fmt.Errorf("invalid HTTP status code: %d", code)
			}
			retryable[code] = true
		}
		c.retryable = retryable
		return nil
	}
}

// Request builds a GET request resolving path against the base URL.
func (c *Client) Request(ctx context.Context, path string) (*http.Request, error) {
	rel, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("parse path: %w", err)
	}
	endpoint := c.baseURL.ResolveReference(rel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	return req, nil
}

// Do builds and sends the request for path.
func (c *Client) Do(ctx context.Context, path string) (*http.Response, error) {
	req, err := c.Request(ctx, path)
	if err != nil {
		return nil, err
	}
	return c.httpClient.Do(req)
}

// ShouldRetry reports whether statusCode is in the retryable set.
func (c *Client) ShouldRetry(statusCode int) bool {
	return c.retryable[statusCode]
}

// UserAgent returns the configured user agent (exported for demos).
func (c *Client) UserAgent() string { return c.userAgent }
```

### The runnable demo

The demo stands up an in-process `httptest.Server` that returns 502, points the
client at it with the server's own client, and prints the status and the retry
decision. Because everything runs in one process against a local listener, the
output is fully deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/apiclient/internal/httpx"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("server saw User-Agent: %s\n", r.Header.Get("User-Agent"))
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "retry later")
	}))
	defer srv.Close()

	client, err := httpx.New(
		srv.URL,
		httpx.WithHTTPClient(srv.Client()),
		httpx.WithTimeout(time.Second),
		httpx.WithUserAgent("orders/1.2"),
		httpx.WithRetryStatus(http.StatusBadGateway),
	)
	if err != nil {
		panic(err)
	}

	resp, err := client.Do(context.Background(), "/health")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	fmt.Printf("status: %d\n", resp.StatusCode)
	fmt.Printf("should retry: %t\n", client.ShouldRetry(resp.StatusCode))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
server saw User-Agent: orders/1.2
status: 502
should retry: true
```

### Tests

The test suite is the point of the module. `TestClientAppliesOptionsAndSendsRequest`
runs against a real local server and proves the User-Agent header arrives, the
502 comes back, and `ShouldRetry(502)` is true — an end-to-end check of request
building, headers, transport, and classification.
`TestNewRejectsInvalidConfiguration` is table-driven over the four ways
construction can fail, asserting the nil-client case with `errors.Is` against the
sentinel. `TestOptionsDoNotMutateCallerHTTPClient` proves the defensive copy: the
caller's client keeps its 10-second timeout while the built client gets 250 ms.
`TestWithRetryStatusReplacesSet` is the added case: `WithRetryStatus(429, 503)`
makes both retryable while 500 is not.

Create `internal/httpx/client_test.go`:

```go
package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientAppliesOptionsAndSendsRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); got != "orders/1.2" {
			t.Errorf("User-Agent = %q, want orders/1.2", got)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "retry later")
	}))
	t.Cleanup(server.Close)

	client, err := New(
		server.URL,
		WithHTTPClient(server.Client()),
		WithTimeout(time.Second),
		WithUserAgent("orders/1.2"),
		WithRetryStatus(http.StatusBadGateway),
	)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(context.Background(), "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	if !client.ShouldRetry(resp.StatusCode) {
		t.Fatalf("ShouldRetry(%d) = false, want true", resp.StatusCode)
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		opts    []Option
		wantErr error
	}{
		{name: "missing host", baseURL: "api.example.com"},
		{name: "nil client", baseURL: "https://api.example.com", opts: []Option{WithHTTPClient(nil)}, wantErr: ErrNilHTTPClient},
		{name: "zero timeout", baseURL: "https://api.example.com", opts: []Option{WithTimeout(0)}},
		{name: "empty user agent", baseURL: "https://api.example.com", opts: []Option{WithUserAgent("   ")}},
		{name: "out of range retry status", baseURL: "https://api.example.com", opts: []Option{WithRetryStatus(42)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.baseURL, tt.opts...)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want errors.Is %v", err, tt.wantErr)
			}
		})
	}
}

func TestOptionsDoNotMutateCallerHTTPClient(t *testing.T) {
	t.Parallel()

	caller := &http.Client{Timeout: 10 * time.Second}

	client, err := New(
		"https://api.example.com",
		WithHTTPClient(caller),
		WithTimeout(250*time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}

	if caller.Timeout != 10*time.Second {
		t.Fatalf("caller client mutated to %s, want 10s", caller.Timeout)
	}
	if client.httpClient.Timeout != 250*time.Millisecond {
		t.Fatalf("built client timeout = %s, want 250ms", client.httpClient.Timeout)
	}
}

func TestWithRetryStatusReplacesSet(t *testing.T) {
	t.Parallel()

	client, err := New(
		"https://api.example.com",
		WithRetryStatus(http.StatusTooManyRequests, http.StatusServiceUnavailable),
	)
	if err != nil {
		t.Fatal(err)
	}

	if !client.ShouldRetry(http.StatusTooManyRequests) {
		t.Error("429 should be retryable")
	}
	if !client.ShouldRetry(http.StatusServiceUnavailable) {
		t.Error("503 should be retryable")
	}
	if client.ShouldRetry(http.StatusInternalServerError) {
		t.Error("500 should not be retryable after replacing the set")
	}
}
```

## Review

The client is correct when `New` is the only place a bad configuration can be
created and when no option ever mutates state the caller still owns. The two
traps this module drills are the reasons it uses `func(*Client) error` rather
than `func(*Client)`: an option that can receive a nil client or a zero timeout
must be able to reject it at the constructor boundary, and an option that
overrides a caller-supplied `*http.Client` must shallow-copy it first.
`TestNewRejectsInvalidConfiguration` proves the first, `TestOptionsDoNotMutateCallerHTTPClient`
the second. The `httptest.NewServer` test keeps the round trip deterministic
while still exercising the real transport, so a regression in request building or
header setting fails loudly rather than in production.

## Resources

- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis)
- [net/http Client](https://pkg.go.dev/net/http#Client)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)
- [net/url URL.ResolveReference](https://pkg.go.dev/net/url#URL.ResolveReference)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-db-pool-options.md](02-db-pool-options.md)
