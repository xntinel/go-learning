# 15. Custom HTTP Transport

`net/http` ships a working `http.Transport` and an `http.Client` on top of it, but a production service needs more: fixed headers on every outgoing call, retry with body replaying, observable timing, and a connection pool that will not exhaust the kernel's socket table. All of this sits at the `http.RoundTripper` interface, which is the single seam between `http.Client` and the wire.

```text
transport/
  go.mod
  transport.go
  transport_test.go
  cmd/demo/main.go
```

The package implements three composable middleware types — `HeaderTransport`, `RetryTransport`, and `LoggingTransport` — plus a constructor for a production-tuned base `*http.Transport`. The test file is the verification; there is no eyeball-only `main`.

## Concepts

### The RoundTripper Interface

`http.RoundTripper` is a single-method interface:

```go
type RoundTripper interface {
	RoundTrip(*Request) (*Response, error)
}
```

`http.Client` calls `RoundTrip` for every HTTP request. The Go documentation pins two contracts as mandatory:

1. `RoundTrip` must not modify the passed `*http.Request` or its body.
2. `RoundTrip` must close the request body before returning an error.

Contract 1 is why every middleware must clone the request before setting headers on it. Contract 2 is why retry middleware must explicitly close each failed response body before the next attempt.

### The Default Transport and Why to Tune It

`http.DefaultTransport` is the shared `*http.Transport` used by `http.DefaultClient`. Its zero-value defaults are not zero: `MaxIdleConns` is 100 and `IdleConnTimeout` is 90 s, but `MaxIdleConnsPerHost` defaults to only 2, which starves high-concurrency backends. Production services override at least:

- `MaxIdleConnsPerHost`: match expected concurrency per host (10-20 is common).
- `MaxConnsPerHost`: a hard cap on simultaneous connections.
- `TLSHandshakeTimeout` and `ResponseHeaderTimeout`: explicit deadlines so a slow server does not hold a goroutine forever.
- `ForceAttemptHTTP2: true`: negotiate HTTP/2 for persistent, multiplexed connections.

### Cloning Requests Before Mutation

`(*http.Request).Clone(ctx)` returns a shallow copy with an independent header map. Modifying the clone's headers does not affect the original. The body is not copied — Clone copies the `io.Reader` reference. Middleware that must read or replace the body must do so separately.

### Retry Safety: GetBody and Body Replay

A retry is safe only when the request body can be replayed. `http.NewRequest` sets `GetBody` automatically for `*strings.Reader`, `*bytes.Reader`, and `*bytes.Buffer`; for all other readers `GetBody` is nil. A well-behaved retry transport:

1. Checks `GetBody` before a second send.
2. Calls `GetBody()` to get a fresh `io.ReadCloser` for each attempt.
3. Gives up — no retry — when `Body != nil` and `GetBody == nil`.

GET and HEAD requests have no body; retrying them is always safe.

### Composing Middleware Chains

Middleware transports compose by wrapping:

```
LoggingTransport -> RetryTransport -> HeaderTransport -> *http.Transport
```

The outermost wrapper runs first; the innermost layer calls the base transport. The `http.Client` holds only the outermost transport; the rest of the chain is invisible to it.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/transport/cmd/demo
cd ~/go-exercises/transport
go mod init example.com/transport
```

### Exercise 1: Base Transport and Middleware Types

Create `transport.go`:

```go
package transport

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

// NewBaseTransport returns an *http.Transport tuned for production use:
// connection pool bounded per host, explicit TLS and header timeouts, and
// HTTP/2 probing enabled. Share one transport for the lifetime of the process.
func NewBaseTransport() *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		MaxConnsPerHost:       20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}

// HeaderTransport is an http.RoundTripper that injects a fixed set of headers
// into every outgoing request without modifying the caller's *http.Request.
type HeaderTransport struct {
	Inner   http.RoundTripper
	Headers map[string]string
}

// RoundTrip clones the request, adds the configured headers, and delegates.
func (t *HeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.Headers {
		clone.Header.Set(k, v)
	}
	return t.Inner.RoundTrip(clone)
}

// RetryTransport retries a request on server errors (HTTP 5xx) and transport
// errors using truncated exponential backoff.
//
// Retry is only safe for requests that can replay the body. If Body is
// non-nil and GetBody is nil, the transport gives up after the first attempt.
type RetryTransport struct {
	Inner      http.RoundTripper
	MaxRetries int
	BaseDelay  time.Duration
}

// RoundTrip sends the request and retries up to MaxRetries additional times
// on a transport error or a 5xx response. The last error response is returned
// to the caller undrained so it can inspect the body.
func (t *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var (
		resp *http.Response
		err  error
	)
	for attempt := 0; attempt <= t.MaxRetries; attempt++ {
		if attempt > 0 {
			// A non-nil body without GetBody cannot be replayed.
			if req.Body != nil && req.GetBody == nil {
				break
			}
			if req.GetBody != nil {
				req.Body, err = req.GetBody()
				if err != nil {
					return nil, fmt.Errorf("transport: refresh body: %w", err)
				}
			}
			delay := t.BaseDelay * (1 << uint(attempt-1))
			time.Sleep(delay)
		}
		resp, err = t.Inner.RoundTrip(req)
		if err != nil {
			continue
		}
		if resp.StatusCode < 500 {
			return resp, nil
		}
		// Drain the body so the connection can be reused, then retry —
		// unless this is the final attempt, in which case return the
		// response undrained so the caller can read or inspect it.
		if attempt < t.MaxRetries {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			resp = nil
		}
	}
	return resp, err
}

// LoggingTransport is an http.RoundTripper that logs the HTTP method, URL,
// status code, and elapsed time for every request.
type LoggingTransport struct {
	Inner  http.RoundTripper
	Logger *log.Logger
}

// RoundTrip delegates to the inner transport and logs the outcome.
func (t *LoggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.Inner.RoundTrip(req)
	if err != nil {
		t.Logger.Printf("%s %s err=%v elapsed=%s", req.Method, req.URL, err, time.Since(start))
		return nil, err
	}
	t.Logger.Printf("%s %s status=%d elapsed=%s", req.Method, req.URL, resp.StatusCode, time.Since(start))
	return resp, nil
}
```

The middleware types share the decorator pattern: each wraps an `Inner http.RoundTripper`, adds behavior, and delegates. They compose by nesting in the `http.Client.Transport` field.

### Exercise 2: Tests and Example

Create `transport_test.go`:

```go
package transport

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHeaderTransportAddsHeaders(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, r.Header.Get("X-App"))
	}))
	defer ts.Close()

	tr := &HeaderTransport{
		Inner:   http.DefaultTransport,
		Headers: map[string]string{"X-App": "sentinel"},
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := strings.TrimSpace(string(body)); got != "sentinel" {
		t.Fatalf("body = %q, want sentinel", got)
	}
}

func TestHeaderTransportDoesNotMutateRequest(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tr := &HeaderTransport{
		Inner:   http.DefaultTransport,
		Headers: map[string]string{"X-App": "sentinel"},
	}
	// Call RoundTrip directly to isolate the immutability contract.
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := req.Header.Get("X-App"); got != "" {
		t.Fatalf("original request mutated: X-App = %q, want empty", got)
	}
}

func TestRetryTransportRetriesOn5xx(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		count int
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	tr := &RetryTransport{
		Inner:      http.DefaultTransport,
		MaxRetries: 2,
		BaseDelay:  time.Millisecond,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mu.Lock()
	total := count
	mu.Unlock()
	if total != 3 {
		t.Fatalf("server called %d times, want 3", total)
	}
}

func TestRetryTransportReturnsLastResponseAfterMaxRetries(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	tr := &RetryTransport{
		Inner:      http.DefaultTransport,
		MaxRetries: 2,
		BaseDelay:  time.Millisecond,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestLoggingTransportLogsMethodAndStatus(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	tr := &LoggingTransport{
		Inner:  http.DefaultTransport,
		Logger: logger,
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	logged := buf.String()
	for _, want := range []string{"GET", "201"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q does not contain %q", logged, want)
		}
	}
}

func TestComposedChainPassesHeadersAndLogs(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Echo", r.Header.Get("X-Request-ID"))
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	var logBuf strings.Builder
	logger := log.New(&logBuf, "", 0)
	chain := &LoggingTransport{
		Inner: &RetryTransport{
			Inner: &HeaderTransport{
				Inner:   http.DefaultTransport,
				Headers: map[string]string{"X-Request-ID": "abc-123"},
			},
			MaxRetries: 1,
			BaseDelay:  time.Millisecond,
		},
		Logger: logger,
	}
	client := &http.Client{Transport: chain}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if echo := resp.Header.Get("X-Echo"); echo != "abc-123" {
		t.Fatalf("X-Echo = %q, want abc-123", echo)
	}
	if !strings.Contains(logBuf.String(), "200") {
		t.Fatalf("log %q does not contain status 200", logBuf.String())
	}
}

// ExampleHeaderTransport shows how HeaderTransport injects a User-Agent header
// into every outgoing request.
func ExampleHeaderTransport() {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, r.Header.Get("User-Agent"))
	}))
	defer ts.Close()

	client := &http.Client{
		Transport: &HeaderTransport{
			Inner:   http.DefaultTransport,
			Headers: map[string]string{"User-Agent": "myapp/1.0"},
		},
	}
	resp, err := client.Get(ts.URL)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(body)))
	// Output: myapp/1.0
}
```

Your turn: add `TestLoggingTransportLogsOnError` — wrap a fake `http.RoundTripper` that always returns `errors.New("connection refused")` inside a `LoggingTransport`, make a request via `RoundTrip`, and assert that the log buffer contains `"err="`.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"example.com/transport"
)

func main() {
	// A local test server stands in for a real backend.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		fmt.Fprintf(w, "received: %s", ua)
	}))
	defer ts.Close()

	logger := log.New(os.Stdout, "http: ", 0)
	chain := &transport.LoggingTransport{
		Inner: &transport.RetryTransport{
			Inner: &transport.HeaderTransport{
				Inner:   transport.NewBaseTransport(),
				Headers: map[string]string{"User-Agent": "demo/1.0"},
			},
			MaxRetries: 2,
			BaseDelay:  10 * time.Millisecond,
		},
		Logger: logger,
	}
	client := &http.Client{Transport: chain}

	resp, err := client.Get(ts.URL)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Println(strings.TrimSpace(string(body)))
}
```

Run with `go run ./cmd/demo`. The log line and the echoed User-Agent appear on stdout.

## Common Mistakes

### Mutating the Caller's Request

Wrong: `req.Header.Set("User-Agent", "myapp/1.0")` inside `RoundTrip`. This breaks the `http.RoundTripper` contract and causes data races when the same `*http.Request` is reused across goroutines.

Fix: clone first — `clone := req.Clone(req.Context())` — and operate on the clone only.

### Retrying Without Replaying the Body

Wrong: retry a POST request without calling `GetBody` to reconstruct the body for each attempt. The second send has an already-consumed `io.Reader` and sends an empty body.

Fix: check `req.GetBody != nil` before any retry. If `GetBody` is nil and the body is non-nil, give up after the first attempt.

### Leaking the Response Body Between Retries

Wrong: discard the failed 5xx response without draining the body, then issue the next request. The connection cannot be returned to the pool; each retry opens a new connection until the pool is exhausted.

Fix: `io.Copy(io.Discard, resp.Body); resp.Body.Close()` before each retry. Return the last response undrained so the caller can read it.

### Wrapping DefaultTransport in Production Middleware

Wrong: use `http.DefaultTransport` as the innermost transport in a production chain. `DefaultTransport` has `MaxIdleConnsPerHost == 2`, which creates head-of-line blocking at the pool level under high concurrency, and is shared by all callers in the process.

Fix: construct a dedicated `*http.Transport` per client via `NewBaseTransport()` and tune it for the target workload.

## Verification

From `~/go-exercises/transport`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `go test` is the verification — there is no program to eyeball.

## Summary

- `http.RoundTripper` is the single-method seam between `http.Client` and the wire; implementing it is the only extension point for HTTP client behavior.
- Middleware transports compose by wrapping an `Inner http.RoundTripper`; the outermost wrapper runs first.
- Always clone the request (`req.Clone`) before setting headers; never mutate the caller's request.
- Retry is safe only for nil-body requests or requests where `GetBody` is non-nil; drain and close each failed response body before the next attempt.
- Tune `*http.Transport` explicitly: `MaxIdleConnsPerHost`, `MaxConnsPerHost`, `TLSHandshakeTimeout`, `ResponseHeaderTimeout`, and `ForceAttemptHTTP2` all have production-relevant defaults that may need adjustment.

## What's Next

Next: [Reverse Proxy with Header Manipulation](../16-reverse-proxy-header-manipulation/16-reverse-proxy-header-manipulation.md).

## Resources

- [net/http: RoundTripper](https://pkg.go.dev/net/http#RoundTripper)
- [net/http: Transport](https://pkg.go.dev/net/http#Transport)
- [net/http: Request.Clone](https://pkg.go.dev/net/http#Request.Clone)
- [Go Blog: HTTP/2](https://go.dev/blog/http2)
- [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments)
