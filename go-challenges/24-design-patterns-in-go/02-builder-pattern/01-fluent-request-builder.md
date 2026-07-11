# Exercise 1: A Fluent, Validating Request Builder

An `*http.Request` is the classic case for a builder: a method, a URL, headers added one at a time, query parameters accumulated, an optional body, a client timeout, and several rules that only make sense across fields at once. This exercise builds a fluent `RequestBuilder` whose setters record problems as they go and whose `Build` validates everything in one place, returning either a finished request or one joined error that names every problem.

This module is fully self-contained. It starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
request.go           RequestBuilder, sentinel errors, New, fluent setters, Client, Build
cmd/
  demo/
    main.go          build a GET and a POST, then print three error cases
request_test.go      happy paths, each validator, error aggregation, reuse, context
```

- Files: `request.go`, `cmd/demo/main.go`, `request_test.go`.
- Implement: `RequestBuilder` with fluent setters (`Method`, `URL`, `Header`, `Query`, `Body`, `JSONBody`, `Timeout`), a `Client` accessor, and `Build() (*http.Request, error)`.
- Test: `request_test.go` pins the happy paths, every validator via its sentinel, the `errors.Join` aggregation, that a reused builder does not leak build-time errors, and that the product carries a context with no deadline.
- Verify: `go test -race -count=1 ./...`

Set up the module:

```bash
mkdir -p request-builder/cmd/demo && cd request-builder
go mod init example.com/request-builder
```

### Why setters record and Build decides

The builder owns the in-progress state and nothing else. Each setter mutates one field and returns the same pointer so calls chain, and a setter that is handed something obviously wrong — an unknown HTTP method, an empty header key, a non-positive timeout — does not panic or return early; it appends a sentinel-wrapped error to an internal slice and keeps going. Deferring judgment is the point. A chain that stopped at the first mistake would force the caller into a fix-recompile-discover-the-next loop, whereas a builder that collects everything can hand back the full picture in one `Build`.

`Build` is where the value is decided. It first copies the setter errors into a *fresh local slice* — this is the load-bearing detail — and then runs the checks that only make sense once the whole configuration is present: the URL is required and must parse and must be `http` or `https`; a body is illegal on `GET` and on `HEAD`. Each of these appends to the local slice, never to the builder's own slice. If anything was recorded, `Build` returns `fmt.Errorf("request: %w", errors.Join(errs...))`, a single error that still answers `errors.Is` for every individual cause. If the local slice is empty, it assembles the real `*http.Request`, attaches the query parameters and headers, and returns it.

Copying into a local slice is what makes the builder honestly reusable. If `Build` appended `ErrEmptyURL` to `b.errs`, a caller who built once without a URL, saw the error, set the URL, and built again would see the *stale* `ErrEmptyURL` re-reported forever. By keeping build-time errors local, the second `Build` is judged only on the builder's current state. The `TestBuild_IsReusableAndDoesNotLeakBuildErrors` test pins exactly this.

The sentinels carry no package prefix of their own (`errors.New("URL is required")`, not `"request: URL is required"`); the single `request:` prefix is added once, at the outer `fmt.Errorf` in `Build`. That keeps a single-error message reading `request: invalid HTTP method: "BREW"` rather than doubling the prefix.

### Why a Client accessor instead of a timeout getter

A timeout is not a property of an `*http.Request`; it belongs to the `*http.Client` that sends it. A builder that stored a timeout and then dropped it on the floor would be lying about its API. Rather than expose an awkward getter, the builder offers `Client() *http.Client`, which returns a client configured with the accumulated timeout — the value a caller actually uses, via `client.Do(req)`. The timeout is therefore genuinely consumed, and `cmd/demo` shows it by printing `client.Timeout`.

Create `request.go`:

```go
package request

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrEmptyURL        = errors.New("URL is required")
	ErrBodyOnGet       = errors.New("GET requests cannot carry a body")
	ErrBodyOnHead      = errors.New("HEAD requests cannot carry a body")
	ErrInvalidMethod   = errors.New("invalid HTTP method")
	ErrNegativeTimeout = errors.New("timeout must be positive")
	ErrBadScheme       = errors.New("URL scheme must be http or https")
	ErrEmptyHeaderKey  = errors.New("header key must not be empty")
)

var allowedMethods = map[string]bool{
	http.MethodGet:    true,
	http.MethodPost:   true,
	http.MethodPut:    true,
	http.MethodPatch:  true,
	http.MethodDelete: true,
	http.MethodHead:   true,
}

// RequestBuilder accumulates the inputs for one *http.Request. Setters record
// problems rather than failing fast; Build runs the cross-field checks and
// reports every problem together. It is mutable and not safe for concurrent use.
type RequestBuilder struct {
	method  string
	rawURL  string
	headers map[string]string
	query   url.Values
	body    io.Reader
	timeout time.Duration
	errs    []error
}

// New returns a builder seeded with sensible defaults: a GET request and a
// thirty-second client timeout.
func New() *RequestBuilder {
	return &RequestBuilder{
		method:  http.MethodGet,
		headers: make(map[string]string),
		query:   make(url.Values),
		timeout: 30 * time.Second,
	}
}

func (b *RequestBuilder) Method(m string) *RequestBuilder {
	m = strings.ToUpper(m)
	if !allowedMethods[m] {
		b.errs = append(b.errs, fmt.Errorf("%w: %q", ErrInvalidMethod, m))
		return b
	}
	b.method = m
	return b
}

func (b *RequestBuilder) URL(rawURL string) *RequestBuilder {
	b.rawURL = rawURL
	return b
}

func (b *RequestBuilder) Header(key, value string) *RequestBuilder {
	if key == "" {
		b.errs = append(b.errs, ErrEmptyHeaderKey)
		return b
	}
	b.headers[key] = value
	return b
}

func (b *RequestBuilder) Query(key, value string) *RequestBuilder {
	if key == "" {
		b.errs = append(b.errs, errors.New("query parameter key must not be empty"))
		return b
	}
	b.query.Add(key, value)
	return b
}

func (b *RequestBuilder) Body(body string) *RequestBuilder {
	b.body = bytes.NewBufferString(body)
	return b
}

func (b *RequestBuilder) JSONBody(json string) *RequestBuilder {
	b.body = bytes.NewBufferString(json)
	b.headers["Content-Type"] = "application/json"
	return b
}

func (b *RequestBuilder) Timeout(d time.Duration) *RequestBuilder {
	if d <= 0 {
		b.errs = append(b.errs, fmt.Errorf("%w: %s", ErrNegativeTimeout, d))
		return b
	}
	b.timeout = d
	return b
}

// Client returns an *http.Client configured with the accumulated timeout, the
// value a caller actually uses to send the built request via client.Do(req).
func (b *RequestBuilder) Client() *http.Client {
	return &http.Client{Timeout: b.timeout}
}

// Build validates the accumulated configuration and returns the request. The
// setter errors are copied into a fresh local slice so a build-time failure
// never sticks to the builder and poisons a later, valid Build.
func (b *RequestBuilder) Build() (*http.Request, error) {
	errs := append([]error(nil), b.errs...)

	if b.rawURL == "" {
		errs = append(errs, ErrEmptyURL)
	}

	var parsed *url.URL
	if b.rawURL != "" {
		u, err := url.Parse(b.rawURL)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("invalid URL: %w", err))
		case u.Scheme != "http" && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("%w: %q", ErrBadScheme, u.Scheme))
		default:
			parsed = u
		}
	}

	if b.body != nil {
		switch b.method {
		case http.MethodGet:
			errs = append(errs, ErrBodyOnGet)
		case http.MethodHead:
			errs = append(errs, ErrBodyOnHead)
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("request: %w", errors.Join(errs...))
	}

	if len(b.query) > 0 {
		q := parsed.Query()
		for k, vs := range b.query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		parsed.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(context.Background(), b.method, parsed.String(), b.body)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	for k, v := range b.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}
```

The two `switch` statements read as the cross-field rules they encode: a URL must parse and speak http(s), and a body is only legal on methods that carry one. Everything funnels into the single `errors.Join`, so a chain with three independent mistakes produces one error from which a test can still recover all three sentinels.

### The runnable demo

The demo builds two real requests — a GET whose query parameters `url.Values.Encode` sorts alphabetically, and a POST whose `Client()` carries the five-second timeout — then prints three error cases: an invalid method, a body on a GET, and a triple-fault chain that aggregates three sentinels into one multi-line error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/request-builder"
)

func main() {
	// A GET with query parameters and an auth header.
	req, err := request.New().
		URL("https://api.example.com/users").
		Query("page", "1").
		Query("limit", "10").
		Header("Authorization", "Bearer token123").
		Build()
	if err != nil {
		log.Fatalf("GET build: %v", err)
	}
	fmt.Printf("GET %s auth=%s\n", req.URL, req.Header.Get("Authorization"))

	// A POST with a JSON body and a per-request client timeout.
	b := request.New().
		Method("POST").
		URL("https://api.example.com/users").
		JSONBody(`{"name":"Alice"}`).
		Header("X-Request-ID", "abc-123").
		Timeout(5 * time.Second)
	req, err = b.Build()
	if err != nil {
		log.Fatalf("POST build: %v", err)
	}
	client := b.Client()
	fmt.Printf("POST %s ct=%s rid=%s client-timeout=%s\n",
		req.URL, req.Header.Get("Content-Type"), req.Header.Get("X-Request-ID"), client.Timeout)

	fmt.Println("--- error cases ---")

	if _, err := request.New().URL("https://example.com").Method("BREW").Build(); err != nil {
		fmt.Printf("invalid method: %v\n", err)
	}
	if _, err := request.New().URL("https://example.com").Body("payload").Build(); err != nil {
		fmt.Printf("GET with body: %v\n", err)
	}
	if _, err := request.New().Method("BREW").URL("ftp://host").Timeout(-time.Second).Build(); err != nil {
		fmt.Printf("aggregated: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET https://api.example.com/users?limit=10&page=1 auth=Bearer token123
POST https://api.example.com/users ct=application/json rid=abc-123 client-timeout=5s
--- error cases ---
invalid method: request: invalid HTTP method: "BREW"
GET with body: request: GET requests cannot carry a body
aggregated: request: invalid HTTP method: "BREW"
timeout must be positive: -1s
URL scheme must be http or https: "ftp"
```

The aggregated case prints three lines because `errors.Join` separates its causes with newlines; the three sentinels were recorded by the `Method` setter, the `Timeout` setter, and the scheme check inside `Build`, in that order.

### Tests

The suite pins each property the builder claims. The happy paths confirm a GET carries its query and auth and no body and that a lowercase `"post"` is normalised. One test confirms `Client()` carries the timeout. One test per validator asserts the matching sentinel via `errors.Is`. The aggregation test confirms a triple-fault chain keeps all three sentinels reachable. The reuse test is the regression guard for the local-slice fix. The context test confirms the product carries a non-nil context with no deadline, since an unsent request must not already be counting down.

Create `request_test.go`:

```go
package request

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestBuild_GETWithQueryAndAuth(t *testing.T) {
	t.Parallel()

	req, err := New().
		URL("https://api.example.com/users").
		Query("page", "1").
		Query("limit", "10").
		Header("Authorization", "Bearer token123").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if req.Method != http.MethodGet {
		t.Errorf("Method = %s, want GET", req.Method)
	}
	if got := req.URL.Query().Get("page"); got != "1" {
		t.Errorf("page = %q", got)
	}
	if got := req.URL.Query().Get("limit"); got != "10" {
		t.Errorf("limit = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer token123" {
		t.Errorf("Authorization = %q", got)
	}
	if req.Body != nil {
		t.Errorf("GET must have nil body, got %v", req.Body)
	}
}

func TestBuild_POSTWithJSONBody(t *testing.T) {
	t.Parallel()

	req, err := New().
		Method("post").
		URL("https://api.example.com/users").
		JSONBody(`{"name":"Alice"}`).
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if req.Method != http.MethodPost {
		t.Errorf("Method = %s, want POST", req.Method)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestBuild_ClientUsesTimeout(t *testing.T) {
	t.Parallel()

	c := New().URL("https://example.com").Timeout(5 * time.Second).Client()
	if c.Timeout != 5*time.Second {
		t.Errorf("client timeout = %s, want 5s", c.Timeout)
	}
}

func TestBuild_RejectsMissingURL(t *testing.T) {
	t.Parallel()

	if _, err := New().Build(); !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("err = %v, want ErrEmptyURL", err)
	}
}

func TestBuild_RejectsBodyOnGet(t *testing.T) {
	t.Parallel()

	if _, err := New().URL("https://example.com").Body("payload").Build(); !errors.Is(err, ErrBodyOnGet) {
		t.Fatalf("err = %v, want ErrBodyOnGet", err)
	}
}

func TestBuild_RejectsBodyOnHead(t *testing.T) {
	t.Parallel()

	_, err := New().Method("HEAD").URL("https://example.com").Body("x").Build()
	if !errors.Is(err, ErrBodyOnHead) {
		t.Fatalf("err = %v, want ErrBodyOnHead", err)
	}
}

func TestBuild_RejectsInvalidMethod(t *testing.T) {
	t.Parallel()

	_, err := New().Method("BREW").URL("https://example.com").Build()
	if !errors.Is(err, ErrInvalidMethod) {
		t.Fatalf("err = %v, want ErrInvalidMethod", err)
	}
}

func TestBuild_RejectsBadScheme(t *testing.T) {
	t.Parallel()

	if _, err := New().URL("ftp://example.com").Build(); !errors.Is(err, ErrBadScheme) {
		t.Fatalf("err = %v, want ErrBadScheme", err)
	}
}

func TestBuild_RejectsEmptyHeaderKey(t *testing.T) {
	t.Parallel()

	_, err := New().URL("https://example.com").Header("", "v").Build()
	if !errors.Is(err, ErrEmptyHeaderKey) {
		t.Fatalf("err = %v, want ErrEmptyHeaderKey", err)
	}
}

func TestBuild_AggregatesMultipleErrors(t *testing.T) {
	t.Parallel()

	_, err := New().Method("BREW").URL("ftp://host").Timeout(-time.Second).Build()
	if err == nil {
		t.Fatal("expected joined error, got nil")
	}
	for _, want := range []error{ErrInvalidMethod, ErrBadScheme, ErrNegativeTimeout} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v: got %v", want, err)
		}
	}
}

func TestBuild_IsReusableAndDoesNotLeakBuildErrors(t *testing.T) {
	t.Parallel()

	// A builder with no URL fails, but the build-time ErrEmptyURL must not
	// stick to the builder and poison a later, valid Build.
	b := New()
	if _, err := b.Build(); !errors.Is(err, ErrEmptyURL) {
		t.Fatalf("first Build: want ErrEmptyURL, got %v", err)
	}

	req, err := b.URL("https://example.com").Build()
	if err != nil {
		t.Fatalf("second Build after setting URL: %v", err)
	}
	if req.Method != http.MethodGet {
		t.Errorf("Method = %s, want GET", req.Method)
	}
}

func TestBuild_HasBackgroundContext(t *testing.T) {
	t.Parallel()

	req, err := New().URL("https://example.com").Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if req.Context() == nil {
		t.Fatal("built request must carry a context")
	}
	if _, ok := req.Context().Deadline(); ok {
		t.Fatal("an unsent request should carry no deadline")
	}
}
```

## Review

The builder is correct when setters only ever *record* and `Build` is the single decision point. Confirm that no setter returns an error or panics — they append a sentinel and return `b` — and that every cross-field rule (URL required, scheme http(s), body forbidden on GET and HEAD) lives in `Build`. The most important correctness check is reuse: a `Build` that fails must leave the builder able to succeed once the input is fixed, which holds only because the build-time errors go into a fresh local slice rather than `b.errs`; `TestBuild_IsReusableAndDoesNotLeakBuildErrors` is the guard, and removing the local copy makes it fail.

Common mistakes for this builder. The first is failing fast in setters, which forces the caller to fix one problem per build and tempts tests into brittle string matching; recording sentinels and joining at `Build` fixes both. The second is the double prefix: if a sentinel already says `"request: ..."` and `Build` wraps with `"request: %w"`, single-error messages read `request: request: ...`; keep the prefix only on the outer wrap. The third is storing a timeout the request cannot hold and never using it; exposing `Client()` makes the timeout a value the caller genuinely consumes. Run `go test -race -count=1 ./...` to confirm the whole contract, and remember the builder is not safe to share across goroutines — the next exercises show two ways to make construction safe.

## Resources

- [Effective Go: Constructors and composite literals](https://go.dev/doc/effective_go#composite_literals) — when a constructor earns its place over a bare struct literal.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — the standard-library function that combines several errors into one that still answers `errors.Is`.
- [The Go Blog: Errors are values](https://go.dev/blog/errors-are-values) — why treating errors as ordinary values lets you accumulate and aggregate them.
- [Builder in Go (Refactoring Guru)](https://refactoring.guru/design-patterns/builder/go/example) — the classic Builder pattern with a worked Go example.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-staged-builder.md](02-staged-builder.md)
