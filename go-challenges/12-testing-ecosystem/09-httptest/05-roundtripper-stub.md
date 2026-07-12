# Exercise 5: Inject transport failures with a stub http.RoundTripper

Retry and backoff logic is hard to test against a real server: you cannot reliably
make a loopback socket time out or return a connection error on demand. The
`http.RoundTripper` seam solves this — implement a one-method stub transport and
you can script an exact sequence of `503`s, `429`s, connection errors, and
deadline failures with zero servers. This module builds a retrying client and
tests it entirely through a stubbed transport, then contrasts it with a real
server to justify the trade-off.

## What you'll build

```text
retryclient/                    independent module: example.com/roundtripper-stub
  go.mod                        go 1.26
  retry.go                      RoundTripFunc; Client with a retry loop; ErrExhausted
  cmd/
    demo/
      main.go                   drives the retry loop with a scripted stub transport
  retry_test.go                 stub sequences: retry-then-succeed, exhausted, deadline; real-server variant
```

- Files: `retry.go`, `cmd/demo/main.go`, `retry_test.go`.
- Implement: `type RoundTripFunc func(*http.Request) (*http.Response, error)` with a `RoundTrip` method; a `Client.Get` that retries on `5xx`/`429`/connection errors up to `maxAttempts`, does *not* retry on context cancellation, and returns `ErrExhausted` when out of attempts.
- Test: drive the loop with a slice of scripted responses/errors; assert the retry count, that cancellation short-circuits (`errors.Is(context.DeadlineExceeded)`), and that the final failure wraps `ErrExhausted`; add one real-`httptest.Server` variant.
- Verify: `go test -count=1 -race ./...`

### The transport seam and when to use it

`http.Client` never touches a socket directly; it delegates to its `Transport`,
an `http.RoundTripper`:

```go
type RoundTripper interface {
	RoundTrip(*http.Request) (*http.Response, error)
}
```

A function adapter turns any closure into a `RoundTripper`:

```go
type RoundTripFunc func(*http.Request) (*http.Response, error)
func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
```

Wire that into `&http.Client{Transport: stub}` and every `client.Do` calls your
closure. Now you can return whatever you like: a `503`, a `429`, an
`io.NopCloser` wrapping a truncated body, a raw `errors.New("connection refused")`,
or a `context.DeadlineExceeded`. This is the ideal way to test retry logic
*deterministically* — you script the exact failure sequence and assert the client's
reaction. The one discipline: a returned `*http.Response` must be fully formed.
Set `StatusCode`, a non-nil `Header`, and a non-nil `Body`
(`io.NopCloser(strings.NewReader(...))`); a nil `Body` panics when the client reads
it.

The trade-off against a real `httptest.Server` is fidelity. A stub does not
serialize real headers, parse a real status line, or model connection behavior —
it hands your client an in-memory struct. So: stub the transport to test *your
client's reaction* to responses and failures (retries, backoff, error mapping);
use a real server when the bytes on the wire, TLS, or connection semantics are what
you are verifying. The final test in this module runs the same retry loop against a
real server precisely to show both sides.

The retry policy: retry on `5xx` and `429` (transient), and on transport errors
(connection failures) — but *not* on context cancellation or deadline, which mean
the caller gave up and retrying would be wrong. When attempts run out, return a
final error wrapping `ErrExhausted`.

Create `retry.go`:

```go
package retryclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// RoundTripFunc adapts a function into an http.RoundTripper, so a test can inject
// a scripted transport without a server.
type RoundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip implements http.RoundTripper.
func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ErrExhausted is returned (wrapped) when every attempt failed.
var ErrExhausted = errors.New("retryclient: retries exhausted")

// Client retries idempotent GETs on transient failures.
type Client struct {
	httpc       *http.Client
	maxAttempts int
}

// New builds a Client. A nil httpc uses http.DefaultClient; maxAttempts < 1 is
// treated as 1.
func New(httpc *http.Client, maxAttempts int) *Client {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Client{httpc: httpc, maxAttempts: maxAttempts}
}

// Get fetches url, retrying transient failures up to maxAttempts. It does not
// retry on context cancellation or deadline.
func (c *Client) Get(ctx context.Context, url string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, fmt.Errorf("retryclient: new request: %w", err)
		}

		resp, err := c.httpc.Do(req)
		if err != nil {
			// Cancellation/deadline is terminal: the caller gave up.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusOK:
			if readErr != nil {
				return nil, fmt.Errorf("retryclient: read body: %w", readErr)
			}
			return body, nil
		case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
			lastErr = fmt.Errorf("transient status %d", resp.StatusCode)
			continue
		default:
			return nil, fmt.Errorf("retryclient: non-retryable status %d", resp.StatusCode)
		}
	}
	return nil, fmt.Errorf("retryclient: after %d attempts: %w (last: %v)", c.maxAttempts, ErrExhausted, lastErr)
}
```

### The demo

The demo scripts a stub that fails twice with `503` then succeeds, and prints the
result and attempt count.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"example.com/roundtripper-stub"
)

func resp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func main() {
	attempts := 0
	script := []*http.Response{resp(503, ""), resp(503, ""), resp(200, "payload")}
	stub := retryclient.RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		i := attempts
		attempts++
		return script[i], nil
	})

	c := retryclient.New(&http.Client{Transport: stub}, 3)
	body, err := c.Get(context.Background(), "http://svc.internal/data")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("attempts: %d\n", attempts)
	fmt.Printf("body: %s\n", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
attempts: 3
body: payload
```

### Tests

The stub tests script exact failure sequences. `TestRetryThenSucceed` returns two
`503`s then a `200` and asserts three attempts and the final body.
`TestExhausted` returns `503` every time and asserts the error wraps `ErrExhausted`
after exactly `maxAttempts` calls. `TestConnectionErrorRetried` returns a transport
error each time (retried) and checks the attempt count. `TestDeadlineNotRetried`
returns `context.DeadlineExceeded` and asserts the client stops immediately without
retrying. `TestRetryAgainstRealServer` runs the same loop against a real
`httptest.Server` to show wire fidelity.

Create `retry_test.go`:

```go
package retryclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func resp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestRetryThenSucceed(t *testing.T) {
	t.Parallel()

	var calls int
	script := []*http.Response{resp(503, ""), resp(503, ""), resp(200, "ok")}
	stub := RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		i := calls
		calls++
		return script[i], nil
	})

	c := New(&http.Client{Transport: stub}, 3)
	body, err := c.Get(t.Context(), "http://svc.test/")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestExhausted(t *testing.T) {
	t.Parallel()

	var calls int
	stub := RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return resp(503, ""), nil
	})

	c := New(&http.Client{Transport: stub}, 3)
	_, err := c.Get(t.Context(), "http://svc.test/")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want it to wrap ErrExhausted", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}

func TestConnectionErrorRetried(t *testing.T) {
	t.Parallel()

	var calls int
	stub := RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("dial tcp: connection refused")
	})

	c := New(&http.Client{Transport: stub}, 4)
	_, err := c.Get(t.Context(), "http://svc.test/")
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("err = %v, want it to wrap ErrExhausted", err)
	}
	if calls != 4 {
		t.Fatalf("calls = %d, want 4", calls)
	}
}

func TestDeadlineNotRetried(t *testing.T) {
	t.Parallel()

	var calls int
	stub := RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, context.DeadlineExceeded
	})

	c := New(&http.Client{Transport: stub}, 5)
	_, err := c.Get(t.Context(), "http://svc.test/")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 (deadline is not retried)", calls)
	}
}

func TestRetryAgainstRealServer(t *testing.T) {
	t.Parallel()

	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 2 {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.Client(), 3)
	body, err := c.Get(t.Context(), srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("body = %q, want ok", body)
	}
	if got := n.Load(); got != 3 {
		t.Fatalf("server hits = %d, want 3", got)
	}
}
```

As a "your turn" addition, script a `429` followed by a `200` and assert it is
retried (the policy treats `429` as transient).

## Review

The stub transport is the right tool when the thing under test is the client's
*policy*: how many times it retries, which statuses it treats as transient, and
whether it correctly refuses to retry a canceled request. The four stub tests pin
each of those precisely, with an exact call count — something a real server makes
awkward. `TestDeadlineNotRetried` is the subtle one: a client that retried a
canceled request would waste work and possibly duplicate a side effect, so the
loop must treat `context` errors as terminal. The real-server test is the honesty
check: the same policy must hold when the bytes actually cross a socket, and it
also demonstrates when you would prefer a server over a stub — when wire behavior,
not client policy, is the subject.

## Resources

- [net/http `RoundTripper`](https://pkg.go.dev/net/http#RoundTripper) — the transport interface behind every `Client`.
- [net/http `Client.Transport`](https://pkg.go.dev/net/http#Client) — how a client delegates round-trips.
- [io `NopCloser`](https://pkg.go.dev/io#NopCloser) — wrapping a `Reader` as a no-op-close `ReadCloser` for a stubbed body.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-testing-outbound-client.md](04-testing-outbound-client.md) | Next: [06-tls-server-client.md](06-tls-server-client.md)
