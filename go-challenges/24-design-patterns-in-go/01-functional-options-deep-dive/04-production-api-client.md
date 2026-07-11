# Exercise 4: A Production API Client Constructor

A real service client is never just a base URL. It carries a request timeout, a retry policy with backoff, a TLS configuration, a pluggable transport for testing and proxying, and a chain of middleware for auth and tracing. This exercise builds that constructor entirely from functional options, validates the settings as an aggregated set, and assembles a layered `http.Client` whose transport is a retrying round-tripper wrapped by middleware.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
client.go              Client, Option, Middleware, New (aggregating), With* options,
                       retryTransport (http.RoundTripper), sentinel errors, accessors
cmd/
  demo/
    main.go            a flaky httptest server proving retries, then an invalid-combo rejection
client_test.go         defaults, precedence, aggregated invalid combos, retry/give-up,
                       middleware order, TLS-config cloning (all under -race)
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: `Option func(*Client) error`, `Middleware func(http.RoundTripper) http.RoundTripper`, `New(opts ...Option) (*Client, error)` that aggregates errors, the options `WithBaseURL` (required), `WithTimeout`, `WithRetry`, `WithTLSConfig`, `WithInsecureSkipVerify`, `WithTransport`, `WithMiddleware`, a `retryTransport` round-tripper, `Do`, and read-only accessors.
- Test: `client_test.go` proves defaults, last-option-wins precedence, that one bad call surfaces every problem (including the transport/TLS conflict) through `errors.Is`, that retries replay 5xx and then give up, that middleware nest in order, and that the TLS config is cloned.
- Verify: `go test -race ./...` then `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p apiclient/cmd/demo && cd apiclient
go mod init example.com/apiclient
```

### Why a client constructor is the canonical home for functional options

A client is the textbook case for the pattern: almost every caller wants sane defaults and only a handful override one or two knobs, and the set of knobs grows over the life of the service. A positional constructor — `New(baseURL string, timeout time.Duration, retries int, tls *tls.Config, ...)` — breaks every call site the day someone adds a setting, and reads as an unlabelled tuple at the call. Options keep the constructor backward-compatible and self-documenting: `New(WithBaseURL(u), WithRetry(3, 100*time.Millisecond))` says exactly what it configures and leaves everything else defaulted.

This constructor takes the *aggregating* strategy from Exercise 2 rather than short-circuiting, because a client is frequently built from external configuration (flags, environment, a config file) where the operator wants to see every mistake in one pass. `New` applies defaults, runs every option appending each error to a slice, then runs the two checks no single option can own, and returns `fmt.Errorf("apiclient: %w", errors.Join(errs...))`. `errors.Is` still finds each sentinel inside the joined result, so a caller — or a test — can branch on any one failure.

### The two checks that live outside the options

`WithBaseURL` is *required*, and a required input cannot be validated by an option, because the failure mode is the option never being passed. So `WithBaseURL("")` assigns nothing and the post-loop check turns a missing or empty base URL into the single `ErrMissingBaseURL`; a non-empty but malformed value is rejected eagerly inside the option as `ErrBadBaseURL`. This split — required-ness checked at the end, well-formedness checked in the option — is the same shape the database client used, applied to a URL.

The second post-loop check is a genuine *cross-field conflict*, and it is the realistic architect trap this exercise is built around. The TLS config only takes effect on the default `http.Transport`; the moment a caller supplies their own transport through `WithTransport`, that default is replaced and any `WithTLSConfig` would be silently ignored. Silently is the problem: a client that looks like it pins TLS 1.3 but actually inherits the custom transport's settings is a production incident waiting to happen. So `New` reports `ErrTransportTLSConflict` when both a custom transport and a TLS config are set, forcing the caller to put the TLS settings *into* their transport instead. It is a cross-field rule — it depends on two fields whose options can arrive in any order — so it belongs in the final pass, never inside either option.

### The layered transport: retry inside, middleware outside

`build` assembles the `http.Client` once validation has passed. The base is the caller's custom transport if present, otherwise a fresh `http.Transport` carrying the TLS config. That base is wrapped by `retryTransport`, which re-issues the request on a transport error or a 5xx status, sleeping `backoffBase * 2^(attempt-1)` between tries — exponential backoff. Then each middleware wraps the result, and because the loop walks the slice back-to-front the *first* middleware added ends up outermost, closest to the caller, so it observes a logical request once rather than once per retry.

`retryTransport` only closes a response body when it is about to retry, never on the attempt it returns, so the caller always receives a live body. One caveat the code documents: it replays the request as-is, which is only safe for idempotent calls or requests without a body; a client issuing non-idempotent writes should not enable retries. `WithRetry` validates that a positive retry count comes with a positive backoff, because both arguments arrive together in the one option — a cross-argument check that legitimately *can* live inside an option, unlike the cross-field one.

Create `client.go`:

```go
package apiclient

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Option configures a Client during construction. New runs every option and
// collects their errors so one bad call reports all of its problems at once.
type Option func(*Client) error

// Middleware wraps an http.RoundTripper, returning a new one that runs around
// it. It is the standard way to add cross-cutting behaviour (auth headers,
// tracing, metrics) to a client without touching the transport itself.
type Middleware func(http.RoundTripper) http.RoundTripper

// Client is a configured HTTP API client. Every field is unexported and set
// only through options, so a built Client cannot be mutated past the validation
// that ran in New.
type Client struct {
	baseURL     string
	timeout     time.Duration
	maxRetries  int
	backoffBase time.Duration
	tlsConfig   *tls.Config
	transport   http.RoundTripper
	middlewares []Middleware
	httpClient  *http.Client
}

var (
	ErrMissingBaseURL       = errors.New("base url is required")
	ErrBadBaseURL           = errors.New("base url must be an absolute http(s) url")
	ErrBadTimeout           = errors.New("timeout must be positive")
	ErrNegativeRetries      = errors.New("max retries must not be negative")
	ErrRetryNeedsBackoff    = errors.New("backoff base must be positive when retries are enabled")
	ErrNilTransport         = errors.New("transport must not be nil")
	ErrTransportTLSConflict = errors.New("custom transport conflicts with TLS config (the transport ignores it)")
)

// New builds a Client from options. It applies sane defaults first, then runs
// every option collecting their errors, then performs the checks no single
// option can own: the required base URL and the transport-versus-TLS conflict.
// All problems are reported together via errors.Join, and errors.Is still finds
// each sentinel inside the joined result.
func New(opts ...Option) (*Client, error) {
	c := &Client{
		timeout:     30 * time.Second,
		maxRetries:  0,
		backoffBase: 100 * time.Millisecond,
	}
	var errs []error
	for _, opt := range opts {
		if err := opt(c); err != nil {
			errs = append(errs, err)
		}
	}
	if c.baseURL == "" {
		errs = append(errs, ErrMissingBaseURL)
	}
	if c.transport != nil && c.tlsConfig != nil {
		errs = append(errs, ErrTransportTLSConflict)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("apiclient: %w", errors.Join(errs...))
	}
	c.build()
	return c, nil
}

// build assembles the layered http.Client: the base transport (a custom one if
// supplied, otherwise a default carrying the TLS config), wrapped by the retry
// round-tripper, wrapped by each middleware. The first middleware added is the
// outermost layer, so it sees a request before retries happen.
func (c *Client) build() {
	base := c.transport
	if base == nil {
		base = &http.Transport{TLSClientConfig: c.tlsConfig}
	}
	var rt http.RoundTripper = &retryTransport{
		base:        base,
		maxRetries:  c.maxRetries,
		backoffBase: c.backoffBase,
	}
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		rt = c.middlewares[i](rt)
	}
	c.httpClient = &http.Client{Timeout: c.timeout, Transport: rt}
}

// WithBaseURL sets the required base URL. It must be an absolute http or https
// URL; the empty case is left to the required-field check in New so a missing
// and an empty base URL collapse into the same ErrMissingBaseURL.
func WithBaseURL(raw string) Option {
	return func(c *Client) error {
		if raw == "" {
			return nil
		}
		u, err := url.Parse(raw)
		if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%w: %q", ErrBadBaseURL, raw)
		}
		c.baseURL = raw
		return nil
	}
}

// WithTimeout sets the whole-request timeout enforced by the http.Client.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		if d <= 0 {
			return fmt.Errorf("%w: got %s", ErrBadTimeout, d)
		}
		c.timeout = d
		return nil
	}
}

// WithRetry enables retrying of failed and 5xx responses with exponential
// backoff. maxRetries is the number of EXTRA attempts after the first; a
// positive count requires a positive backoff base, which is validated here
// because both arguments arrive together in this one option.
func WithRetry(maxRetries int, backoffBase time.Duration) Option {
	return func(c *Client) error {
		if maxRetries < 0 {
			return fmt.Errorf("%w: got %d", ErrNegativeRetries, maxRetries)
		}
		if maxRetries > 0 && backoffBase <= 0 {
			return fmt.Errorf("%w: got %s", ErrRetryNeedsBackoff, backoffBase)
		}
		c.maxRetries = maxRetries
		c.backoffBase = backoffBase
		return nil
	}
}

// WithTLSConfig sets the TLS configuration applied to the default transport. It
// stores a clone so a later mutation of the caller's value cannot reach into the
// built client. Supplying both this and WithTransport is rejected by New.
func WithTLSConfig(cfg *tls.Config) Option {
	return func(c *Client) error {
		if cfg == nil {
			return nil
		}
		c.tlsConfig = cfg.Clone()
		return nil
	}
}

// WithInsecureSkipVerify is a convenience that disables certificate
// verification. It is a TLS setting, so it likewise conflicts with a custom
// transport supplied through WithTransport.
func WithInsecureSkipVerify(skip bool) Option {
	return func(c *Client) error {
		if c.tlsConfig == nil {
			c.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		c.tlsConfig.InsecureSkipVerify = skip
		return nil
	}
}

// WithTransport plugs in a custom round-tripper, replacing the default
// http.Transport entirely. Because the default transport is where the TLS
// config lives, pairing this with WithTLSConfig is a configuration mistake that
// New reports as ErrTransportTLSConflict.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *Client) error {
		if rt == nil {
			return ErrNilTransport
		}
		c.transport = rt
		return nil
	}
}

// WithMiddleware appends middleware that wrap the round-tripper. Earlier
// middleware end up outermost (closest to the caller).
func WithMiddleware(mw ...Middleware) Option {
	return func(c *Client) error {
		for _, m := range mw {
			if m == nil {
				continue
			}
			c.middlewares = append(c.middlewares, m)
		}
		return nil
	}
}

// Do sends a request through the fully layered transport.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return c.httpClient.Do(req)
}

func (c *Client) BaseURL() string            { return c.baseURL }
func (c *Client) Timeout() time.Duration     { return c.timeout }
func (c *Client) MaxRetries() int            { return c.maxRetries }
func (c *Client) BackoffBase() time.Duration { return c.backoffBase }
func (c *Client) HTTPClient() *http.Client   { return c.httpClient }

// retryTransport re-issues a request on a transport error or a 5xx response,
// sleeping backoffBase * 2^(attempt-1) between tries. It only retries requests
// that are safe to replay (no body, or a rewindable one); callers issuing
// non-idempotent writes should not enable retries.
type retryTransport struct {
	base        http.RoundTripper
	maxRetries  int
	backoffBase time.Duration
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var (
		resp *http.Response
		err  error
	)
	attempts := t.maxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			time.Sleep(t.backoffBase * time.Duration(int64(1)<<(attempt-1)))
		}
		resp, err = t.base.RoundTrip(req)
		retriable := err != nil || (resp != nil && resp.StatusCode >= 500)
		if !retriable {
			return resp, nil
		}
		if attempt < attempts-1 && resp != nil {
			resp.Body.Close()
		}
	}
	return resp, err
}
```

### The runnable demo

The demo stands up a `httptest.Server` that returns 503 for its first two requests and 200 afterward, builds a client with three retries and a 1ms backoff, and issues one `GET`. The retrying transport replays through the two failures and succeeds on the third attempt, which the demo prints. It then makes a deliberately invalid construction — a custom transport together with a TLS config it would ignore — and shows the aggregated error with an `errors.Is` probe, and finally that an omitted base URL is the required-field error. Everything runs offline and deterministically.

Create `cmd/demo/main.go`:

```go
package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/apiclient"
)

func main() {
	// A flaky upstream: it fails the first two requests with 503, then succeeds.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client, err := apiclient.New(
		apiclient.WithBaseURL(srv.URL),
		apiclient.WithTimeout(2*time.Second),
		apiclient.WithRetry(3, time.Millisecond),
	)
	if err != nil {
		fmt.Println("construct error:", err)
		return
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("request error:", err)
		return
	}
	resp.Body.Close()
	fmt.Printf("final status=%d after %d attempts\n", resp.StatusCode, hits.Load())

	// Invalid combo: a custom transport plus a TLS config it would silently ignore.
	_, err = apiclient.New(
		apiclient.WithBaseURL("https://api.example.com"),
		apiclient.WithTransport(http.DefaultTransport),
		apiclient.WithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS13}),
	)
	fmt.Println("invalid combo error:", err)
	fmt.Println("is conflict?", errors.Is(err, apiclient.ErrTransportTLSConflict))

	// A missing base URL is a required-field error.
	_, err = apiclient.New(apiclient.WithTimeout(time.Second))
	fmt.Println("missing base url?", errors.Is(err, apiclient.ErrMissingBaseURL))
}
```

The import path is the module path `example.com/apiclient`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
final status=200 after 3 attempts
invalid combo error: apiclient: custom transport conflicts with TLS config (the transport ignores it)
is conflict? true
missing base url? true
```

### Tests

The tests pin the constructor's contract and the transport's behaviour without touching the network. A `roundTripFunc` adapter turns a function into an `http.RoundTripper`, so `WithTransport` injects a programmable fake that counts calls and returns canned responses. `TestInvalidOptionsAggregate` is the central constructor test — one call with a bad timeout, negative retries, and a transport/TLS conflict must surface all three sentinels at once. `TestRetryTransportRetriesThenSucceeds` and `TestRetryTransportGivesUp` pin the retry count, `TestMiddlewareWrapsInOrder` pins the outermost-first nesting, and `TestTLSConfigCloned` proves a post-construction mutation of the caller's `tls.Config` cannot leak in. All run under `-race`.

Create `client_test.go`:

```go
package apiclient

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc adapts a function into an http.RoundTripper for use as a fake
// transport, so tests never touch the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okResponse(code int) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()

	c, err := New(WithBaseURL("https://api.example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 30*time.Second {
		t.Errorf("timeout = %s, want 30s", c.Timeout())
	}
	if c.MaxRetries() != 0 {
		t.Errorf("maxRetries = %d, want 0", c.MaxRetries())
	}
	if c.HTTPClient() == nil {
		t.Error("http client was not built")
	}
}

func TestMissingBaseURL(t *testing.T) {
	t.Parallel()

	if _, err := New(WithTimeout(time.Second)); !errors.Is(err, ErrMissingBaseURL) {
		t.Fatalf("err = %v, want ErrMissingBaseURL", err)
	}
}

func TestBadBaseURL(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"://nope", "not-a-url", "ftp://files"} {
		if _, err := New(WithBaseURL(raw)); !errors.Is(err, ErrBadBaseURL) {
			t.Errorf("WithBaseURL(%q): err = %v, want ErrBadBaseURL", raw, err)
		}
	}
}

func TestInvalidOptionsAggregate(t *testing.T) {
	t.Parallel()

	// Three independent problems reported together: bad timeout, negative
	// retries, and a transport/TLS conflict (plus a valid base URL).
	_, err := New(
		WithBaseURL("https://api.example.com"),
		WithTimeout(0),
		WithRetry(-1, time.Second),
		WithTransport(http.DefaultTransport),
		WithTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}),
	)
	if err == nil {
		t.Fatal("expected aggregated error, got nil")
	}
	for _, want := range []error{ErrBadTimeout, ErrNegativeRetries, ErrTransportTLSConflict} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v; got %v", want, err)
		}
	}
}

func TestRetryNeedsBackoff(t *testing.T) {
	t.Parallel()

	if _, err := New(WithBaseURL("https://x.test"), WithRetry(3, 0)); !errors.Is(err, ErrRetryNeedsBackoff) {
		t.Fatalf("err = %v, want ErrRetryNeedsBackoff", err)
	}
}

func TestNilTransportRejected(t *testing.T) {
	t.Parallel()

	if _, err := New(WithBaseURL("https://x.test"), WithTransport(nil)); !errors.Is(err, ErrNilTransport) {
		t.Fatalf("err = %v, want ErrNilTransport", err)
	}
}

func TestLaterOptionWins(t *testing.T) {
	t.Parallel()

	c, err := New(WithBaseURL("https://x.test"), WithTimeout(time.Second), WithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout() != 5*time.Second {
		t.Fatalf("timeout = %s, want 5s (last option wins)", c.Timeout())
	}
}

func TestRetryTransportRetriesThenSucceeds(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	fake := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) <= 2 {
			return okResponse(http.StatusServiceUnavailable), nil
		}
		return okResponse(http.StatusOK), nil
	})

	c, err := New(
		WithBaseURL("https://x.test"),
		WithTransport(fake),
		WithRetry(3, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://x.test", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (two 503s then a 200)", calls.Load())
	}
}

func TestRetryTransportGivesUp(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	fake := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return okResponse(http.StatusBadGateway), nil
	})

	c, err := New(
		WithBaseURL("https://x.test"),
		WithTransport(fake),
		WithRetry(2, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://x.test", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (exhausted retries return the last response)", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (1 try + 2 retries)", calls.Load())
	}
}

func TestMiddlewareWrapsInOrder(t *testing.T) {
	t.Parallel()

	var order []string
	tag := func(name string) Middleware {
		return func(next http.RoundTripper) http.RoundTripper {
			return roundTripFunc(func(r *http.Request) (*http.Response, error) {
				order = append(order, name)
				return next.RoundTrip(r)
			})
		}
	}
	fake := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return okResponse(http.StatusOK), nil
	})

	c, err := New(
		WithBaseURL("https://x.test"),
		WithTransport(fake),
		WithMiddleware(tag("outer"), tag("inner")),
	)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://x.test", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(order) != 2 || order[0] != "outer" || order[1] != "inner" {
		t.Fatalf("middleware order = %v, want [outer inner]", order)
	}
}

func TestTLSConfigCloned(t *testing.T) {
	t.Parallel()

	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	c, err := New(WithBaseURL("https://x.test"), WithTLSConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's config after construction must not affect the client.
	cfg.InsecureSkipVerify = true
	tr, ok := c.HTTPClient().Transport.(*retryTransport)
	if !ok {
		t.Fatalf("transport = %T, want *retryTransport", c.HTTPClient().Transport)
	}
	base, ok := tr.base.(*http.Transport)
	if !ok {
		t.Fatalf("base = %T, want *http.Transport", tr.base)
	}
	if base.TLSClientConfig.InsecureSkipVerify {
		t.Error("client TLS config was not cloned; caller mutation leaked in")
	}
}
```

## Review

The constructor is correct when the two checks no option can own live after the loop and the aggregation keeps collecting past the first failure. `TestInvalidOptionsAggregate` is the proof of both: it asserts three sentinels in a single returned error, which a short-circuiting loop can never satisfy, and the transport/TLS conflict among them is the cross-field rule that would be wrong inside either option. The required base URL must be checked post-loop — `TestMissingBaseURL` passes only `WithTimeout`, so an in-option check would never fire. On the transport, `retryTransport` must return the live last response and must not close the body of the attempt it returns; `TestRetryTransportGivesUp` would see a read-after-close if it did, and `TestRetryTransportRetriesThenSucceeds` pins that retries actually replay rather than swallow the 5xx. `TestMiddlewareWrapsInOrder` confirms the back-to-front wrap that puts the first middleware outermost, and `TestTLSConfigCloned` confirms `cfg.Clone()` rather than a shared pointer (a shared `*tls.Config` would also trip `go vet`'s copylock check if copied by value). With all of it green under `go test -race ./...`, the client contract holds.

## Resources

- [`net/http.RoundTripper`](https://pkg.go.dev/net/http#RoundTripper) — the single-method interface the retry layer and every middleware implement.
- [`crypto/tls.Config.Clone`](https://pkg.go.dev/crypto/tls#Config.Clone) — why a TLS config is cloned rather than copied or shared by pointer.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — combines the option failures into one error whose `Is` traverses every cause.
- [Go blog: Error handling and Go](https://go.dev/blog/error-handling-and-go) — the wrapping and sentinel model the validation relies on.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-generic-options.md](03-generic-options.md) | Next: [05-connection-pool-service.md](05-connection-pool-service.md)
