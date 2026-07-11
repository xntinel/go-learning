# 5. Request ID Propagation

Every HTTP service produces log lines, but those lines become useful for debugging
only when you can group them by request. A request ID — a unique string generated
at the edge — threads through every log record, every downstream call, and every
error response for a single transaction. Implementing propagation correctly requires
three cooperating pieces: middleware that mints or preserves the ID, a context key
that carries it without leaking to other packages, and a transport wrapper that
forwards the ID to outgoing HTTP requests. Getting any one of those pieces wrong
silently breaks traceability.

```text
requestid/
  go.mod
  requestid.go
  logging.go
  requestid_test.go
  cmd/demo/main.go
```

## Concepts

### Why Context Values, Not Global State

A request ID belongs to a single request's lifetime. Storing it in a package-level
variable or thread-local equivalent (there is none in Go) forces serialization or
race conditions. `context.Context` carries per-request values without any
synchronization cost because a context is immutable: `context.WithValue` returns a
new context; the parent is unchanged. Handlers receive the context via
`*http.Request.Context()`, so the value travels with the request without any extra
wiring.

### Unexported Key Types Prevent Collisions

`context.WithValue` stores values in a key-value map using interface equality.
Using a bare `string` or `int` as a key means any package that uses the same
literal string retrieves the value — or worse, overwrites it. The fix is a private
struct type:

```go
type contextKey struct{}
```

Because `contextKey` is unexported, no other package can construct a value of
that type. Two packages each with their own `type contextKey struct{}` have
distinct key types and can never collide.

### Preserve Upstream IDs; Generate Only When Absent

A load balancer or API gateway may already have attached a request ID before the
request reaches your service. Overwriting it breaks the correlation chain across
the boundary. The middleware must check the `X-Request-ID` header first:

```
incoming header present  -> preserve it, write back to response header
incoming header absent   -> generate a new one, write to response header
```

`X-Request-ID` is the de facto standard header name used by nginx, AWS ALB, and
most API gateways; some systems use `X-Correlation-ID`. Both names are common in
practice but neither is an IETF standard.

### Generating IDs Without External Dependencies

The stdlib provides `crypto/rand` for cryptographically secure random bytes. A
16-byte random value formatted as a UUID v4 (`xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx`)
is unique enough for all practical purposes and compatible with tools that expect
UUID strings. No external package is required.

### Propagating IDs to Outgoing Requests

When service A calls service B, service B receives a fresh request with no
`X-Request-ID` header unless A explicitly sets it. The idiomatic Go solution is a
custom `http.RoundTripper` that reads the ID from the context of the outgoing
request and adds the header before handing off to the underlying transport. Because
`http.Client` accepts a `Transport` field, this requires no changes to call sites.

### The `slog.Handler` Injection Point

`log/slog` allows replacing the default handler with a custom one. A thin wrapper
that implements `slog.Handler` can inspect the context on every `Handle` call and
add the request ID as a structured attribute. This means every log record emitted
with `logger.InfoContext(ctx, ...)` automatically includes `"request_id"` without
the caller ever explicitly passing the ID to the log call.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/requestid/cmd/demo
cd ~/go-exercises/requestid
go mod init example.com/requestid
```

This is a library package. You verify it with `go test`.

### Exercise 1: Core Package — Context Key, ID Generator, Middleware, and Transport

Create `requestid.go`:

```go
package requestid

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
)

// HeaderName is the de facto standard header for request correlation.
const HeaderName = "X-Request-ID"

// contextKey is an unexported type used as the context key for request IDs.
// Its unexported nature prevents collisions with keys from other packages.
type contextKey struct{}

// NewID returns a random UUID v4 string using crypto/rand.
// It panics only if the OS random source is unavailable, which is unrecoverable.
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("requestid: crypto/rand unavailable: " + err.Error())
	}
	// Set version 4 and variant bits per RFC 4122 section 4.4.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16],
	)
}

// FromContext returns the request ID stored in ctx, or "" if none is set.
func FromContext(ctx context.Context) string {
	if id, ok := ctx.Value(contextKey{}).(string); ok {
		return id
	}
	return ""
}

// WithID returns a new context with the given request ID attached.
func WithID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// Middleware is an HTTP middleware that assigns or preserves a request ID on
// every incoming request and writes it to the response header.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderName)
		if id == "" {
			id = NewID()
		}
		ctx := WithID(r.Context(), id)
		w.Header().Set(HeaderName, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Transport is an http.RoundTripper that reads the request ID from the outgoing
// request's context and attaches it as a header before delegating to Base.
// If Base is nil, http.DefaultTransport is used.
type Transport struct {
	Base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if id := FromContext(req.Context()); id != "" {
		req = req.Clone(req.Context())
		req.Header.Set(HeaderName, id)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
```

`NewID` formats a UUID v4: version nibble `4` at byte 6, variant bits `10` at
byte 8, per RFC 4122 section 4.4. The result is 36 characters in the standard
`8-4-4-4-12` hex format. `req.Clone` produces an independent copy of the request
before header mutation, satisfying the `RoundTripper` contract that forbids
mutating the original request.

### Exercise 2: Context-Aware slog Handler

Create `logging.go`:

```go
package requestid

import (
	"context"
	"log/slog"
)

// NewHandler wraps inner so that every log record emitted with a context
// that carries a request ID automatically includes a "request_id" attribute.
func NewHandler(inner slog.Handler) slog.Handler {
	return &contextHandler{inner: inner}
}

type contextHandler struct {
	inner slog.Handler
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := FromContext(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.inner.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{inner: h.inner.WithGroup(name)}
}
```

`WithAttrs` and `WithGroup` must return a new `contextHandler` wrapping the
modified inner handler — returning the inner handler directly would lose the
injection on all child loggers.

### Exercise 3: Test the Contract

Create `requestid_test.go`:

```go
package requestid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewIDFormat(t *testing.T) {
	t.Parallel()

	id := NewID()
	parts := strings.Split(id, "-")
	if len(parts) != 5 {
		t.Fatalf("NewID() = %q, want 5 dash-separated groups", id)
	}
	lengths := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != lengths[i] {
			t.Errorf("NewID() group %d = %q (len %d), want len %d", i, p, len(p), lengths[i])
		}
	}
	if id[14] != '4' {
		t.Errorf("NewID() version nibble = %q, want '4'", id[14])
	}
}

func TestNewIDIsUnique(t *testing.T) {
	t.Parallel()

	a, b := NewID(), NewID()
	if a == b {
		t.Fatalf("NewID() returned duplicate IDs: %q", a)
	}
}

func TestWithIDAndFromContext(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if got := FromContext(ctx); got != "" {
		t.Fatalf("FromContext on empty context = %q, want empty", got)
	}

	ctx = WithID(ctx, "req-123")
	if got := FromContext(ctx); got != "req-123" {
		t.Fatalf("FromContext = %q, want %q", got, "req-123")
	}
}

func TestFromContextReturnsEmptyForMissingKey(t *testing.T) {
	t.Parallel()

	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("FromContext on background context = %q, want empty", got)
	}
}

func TestMiddlewareGeneratesIDWhenAbsent(t *testing.T) {
	t.Parallel()

	var capturedID string
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if capturedID == "" {
		t.Fatal("Middleware did not inject a request ID into the context")
	}
	if got := rr.Header().Get(HeaderName); got != capturedID {
		t.Errorf("response header %s = %q, want %q", HeaderName, got, capturedID)
	}
}

func TestMiddlewarePreservesUpstreamID(t *testing.T) {
	t.Parallel()

	const upstreamID = "upstream-abc-123"
	var capturedID string
	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = FromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, upstreamID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if capturedID != upstreamID {
		t.Errorf("context ID = %q, want %q (upstream should be preserved)", capturedID, upstreamID)
	}
	if got := rr.Header().Get(HeaderName); got != upstreamID {
		t.Errorf("response header = %q, want %q", got, upstreamID)
	}
}

func TestTransportPropagatesID(t *testing.T) {
	t.Parallel()

	const reqID = "trace-xyz-789"
	var receivedID string

	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedID = r.Header.Get(HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()

	transport := &Transport{Base: http.DefaultTransport}
	client := &http.Client{Transport: transport}

	ctx := WithID(context.Background(), reqID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if receivedID != reqID {
		t.Errorf("downstream received %s = %q, want %q", HeaderName, receivedID, reqID)
	}
}

func TestTransportSkipsHeaderWhenNoID(t *testing.T) {
	t.Parallel()

	var receivedHeader string
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader = r.Header.Get(HeaderName)
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()

	transport := &Transport{Base: http.DefaultTransport}
	client := &http.Client{Transport: transport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, downstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if receivedHeader != "" {
		t.Errorf("downstream received unexpected %s = %q", HeaderName, receivedHeader)
	}
}

func ExampleMiddleware() {
	const fixedID = "example-id-0001"

	handler := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := FromContext(r.Context())
		_, _ = w.Write([]byte(id))
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, fixedID)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	fmt.Print(rr.Body.String())
	// Output: example-id-0001
}

func ExampleFromContext() {
	ctx := WithID(context.Background(), "req-abc")
	fmt.Print(FromContext(ctx))
	// Output: req-abc
}
```

Your turn: add `TestNewHandlerInjectsRequestID` that creates a `bytes.Buffer`, wraps
a `slog.NewJSONHandler` with `NewHandler`, logs one message with a context that
carries a request ID, and asserts the buffer output contains `"request_id"`.

### Exercise 4: The Runnable Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"example.com/requestid"
)

func main() {
	handler := requestid.NewHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	logger := slog.New(handler)

	downstream := httptest.NewServer(requestid.Middleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			logger.InfoContext(ctx, "downstream: handling request", "path", r.URL.Path)
			fmt.Fprintf(w, "downstream saw id=%s", requestid.FromContext(ctx))
		}),
	))
	defer downstream.Close()

	client := &http.Client{Transport: &requestid.Transport{}}

	upstream := httptest.NewServer(requestid.Middleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			logger.InfoContext(ctx, "upstream: received request")

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, downstream.URL+"/data", nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			logger.InfoContext(ctx, "upstream: downstream responded", "status", resp.StatusCode)
			fmt.Fprintf(w, "upstream id=%s | %s", requestid.FromContext(ctx), body)
		}),
	))
	defer upstream.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL+"/", nil)
	req.Header.Set(requestid.HeaderName, "demo-fixed-id-0001")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("\nHTTP response: %s\n", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Both JSON log lines include `"request_id":"demo-fixed-id-0001"`. The final output
line prints `upstream id=demo-fixed-id-0001 | downstream saw id=demo-fixed-id-0001`,
confirming end-to-end propagation through two services.

## Common Mistakes

### Using a String Literal as the Context Key

Wrong:

```go
ctx = context.WithValue(ctx, "requestID", id)
```

What happens: any other package that calls `context.WithValue(ctx, "requestID", ...)`
overwrites the value, and any package that calls `ctx.Value("requestID")` retrieves
it. The `go vet` tool warns about this pattern with the message "should not use
built-in type string as key for value; define your own type to avoid collisions".

Fix: use an unexported struct type as the key, as shown in Exercise 1. The key type
is private to the package; no outside code can construct it.

### Mutating the Incoming Request's Headers in Transport.RoundTrip

Wrong:

```go
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set(HeaderName, FromContext(req.Context()))
	return http.DefaultTransport.RoundTrip(req)
}
```

What happens: the `RoundTripper` contract requires that the implementation not
modify the request or its headers after the first call to `RoundTrip`. Mutating
`req.Header` directly violates this and can cause data races when the client
retries or when the caller reuses the request.

Fix: call `req.Clone(req.Context())` first to get an independent copy, then set
the header on the clone. See Exercise 1.

### Returning the Inner Handler From WithAttrs Without Wrapping

Wrong:

```go
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.inner.WithAttrs(attrs)
}
```

What happens: the returned handler is the raw inner handler, not the
`contextHandler` wrapper. Subsequent log calls via this child logger never inject
the request ID because the injection code lives in `contextHandler.Handle`.

Fix: wrap the result in a new `contextHandler`:

```go
func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{inner: h.inner.WithAttrs(attrs)}
}
```

### Forgetting to Set the Response Header

Wrong: setting the ID in context but not calling `w.Header().Set(HeaderName, id)`.

What happens: the caller that initiated the request cannot confirm which ID was
assigned. Debugging requires re-running the request rather than correlating on the
ID already returned.

Fix: write the ID to the response header in the middleware before calling
`next.ServeHTTP`, as in Exercise 1. The header must be set before the first call
to `w.WriteHeader` or `w.Write`.

## Verification

From `~/go-exercises/requestid`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Run `go run ./cmd/demo` to observe structured log output with
the `request_id` field on every line.

## Summary

- A private `contextKey` type prevents key collisions between packages; never use a
  plain string or int as a context key for values that cross package boundaries.
- Middleware checks the incoming `X-Request-ID` header and preserves it if present,
  generating a new ID only when absent.
- `crypto/rand` produces cryptographically secure IDs without external dependencies;
  formatting 16 random bytes as UUID v4 is compatible with standard tooling.
- A custom `slog.Handler` wrapper injects the request ID on every `Handle` call,
  eliminating the need to pass the ID explicitly to each log statement.
- A custom `http.RoundTripper` reads the ID from the outgoing request's context and
  sets the header on a cloned request, respecting the `RoundTripper` mutation
  contract.
- `WithAttrs` and `WithGroup` in a handler wrapper must return a new instance of the
  wrapper, not the raw inner handler, or the injection is silently dropped.

## What's Next

Next: [Structured Error Responses](../06-structured-error-responses/06-structured-error-responses.md).

## Resources

- [context.WithValue](https://pkg.go.dev/context#WithValue) — key type requirements and the unexported-key pattern
- [log/slog Handler interface](https://pkg.go.dev/log/slog#Handler) — all four methods and their contracts
- [net/http RoundTripper](https://pkg.go.dev/net/http#RoundTripper) — mutation constraints and the clone pattern
- [crypto/rand](https://pkg.go.dev/crypto/rand) — `Read` for cryptographically secure random bytes
- [RFC 4122 section 4.4](https://www.rfc-editor.org/rfc/rfc4122#section-4.4) — UUID v4 version and variant bits
