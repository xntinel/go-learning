# Exercise 2: Per-Call Timeout for an Outbound HTTP Dependency

The most common way a service breaks its SLO is by inheriting a slow dependency's
latency. This exercise builds the canonical guard: a downstream-client method that
derives a per-request timeout, threads it into the HTTP request so the transport
aborts the in-flight call at the deadline, always drains and closes the body, and
maps a deadline hit to a typed error while other failures stay distinct.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
upstream-client/                     independent module: example.com/upstream
  go.mod                             go 1.26
  client.go                          Client, CallUpstream(ctx, url); ErrUpstreamTimeout, ErrUpstreamStatus
  cmd/
    demo/
      main.go                        runnable demo: fast server OK, slow server times out
  client_test.go                     httptest slow/fast/500 handlers, body-close check, -race
```

- Files: `client.go`, `cmd/demo/main.go`, `client_test.go`.
- Implement: `Client` with a `PerCallTimeout`; `CallUpstream(ctx, url) ([]byte, error)` deriving a `WithTimeout`, using `http.NewRequestWithContext`, draining+closing the body, mapping `DeadlineExceeded` to `ErrUpstreamTimeout` and non-2xx to `ErrUpstreamStatus`.
- Test: slow handler yields `ErrUpstreamTimeout` (with `DeadlineExceeded` underneath) near the timeout, not the server delay; fast handler returns the body; a 500 is `ErrUpstreamStatus`, not a timeout; the body is always closed.
- Verify: `go test -count=1 -race ./...`

### Why the timeout must live on the request, not just the client

A `http.Client.Timeout` field caps the whole call, but it is a blunt instrument: it
is fixed at client-construction time and cannot vary per call or shrink to fit the
remaining request budget. The production pattern derives a fresh
`context.WithTimeout` from the *caller's* context for each call and attaches it with
`http.NewRequestWithContext`. That does two things the client-level timeout cannot.
First, it inherits the caller's deadline — if the incoming request has only 40ms of
budget left, `WithTimeout(ctx, 2*time.Second)` still fires in 40ms because the
parent wins, so the outbound call can never outlive the request that spawned it.
Second, it is what actually aborts the in-flight call: when the context is done, the
transport cancels the connection, unblocks the `Do`, and — critically — stops
consuming a connection from the pool. Passing a plain `http.NewRequest` would leave
the deadline as a lie: `ctx.Done()` would close, but the socket read would keep
running to completion.

`Do` returns an error whose chain includes `context.DeadlineExceeded` when the
context deadline fired mid-flight. We check `errors.Is(err, context.DeadlineExceeded)`
and translate it to a typed `ErrUpstreamTimeout`, wrapping the original with `%w` so
both the domain sentinel and the underlying `DeadlineExceeded` remain discoverable
by `errors.Is`. Any other transport error passes through wrapped but distinct. A
non-2xx status is a different failure class entirely — the server answered, it just
said no — so it maps to `ErrUpstreamStatus`, never to a timeout. Conflating a 500
with a timeout would send a retry-on-timeout policy chasing a deterministic error.

### Always drain and close the body

`resp.Body` must be closed or the connection leaks; and to let the transport reuse
the connection you should drain any unread remainder with `io.Copy(io.Discard, ...)`
before closing. We do both in a `defer` the instant we have a non-nil response, so
every return path — success, status error, read error — releases the connection.

Create `client.go`:

```go
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUpstreamTimeout is returned when the per-call deadline fires before the
// upstream responds. It wraps context.DeadlineExceeded.
var ErrUpstreamTimeout = errors.New("upstream call timed out")

// ErrUpstreamStatus is returned when the upstream answers with a non-2xx status.
// It is deliberately distinct from a timeout so retry policy can differ.
var ErrUpstreamStatus = errors.New("upstream returned error status")

// Client calls a downstream HTTP dependency with a per-call time budget.
type Client struct {
	HTTP           *http.Client
	PerCallTimeout time.Duration
}

// New returns a Client with the given per-call timeout and a default HTTP client.
func New(perCall time.Duration) *Client {
	return &Client{HTTP: &http.Client{}, PerCallTimeout: perCall}
}

// CallUpstream GETs url under a per-call deadline derived from ctx. It returns
// the response body on 2xx, ErrUpstreamStatus on non-2xx, and ErrUpstreamTimeout
// (wrapping context.DeadlineExceeded) when the deadline fires first.
func (c *Client) CallUpstream(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.PerCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %w", ErrUpstreamTimeout, err)
		}
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: %d", ErrUpstreamStatus, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// A deadline can fire mid-read too.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %w", ErrUpstreamTimeout, err)
		}
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}
```

### The runnable demo

The demo stands up two `httptest` servers — one that answers immediately and one
that sleeps past the client's 50ms budget — and calls both, printing the outcome.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/upstream"
)

func main() {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "pong")
	}))
	defer fast.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
			fmt.Fprint(w, "late")
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()

	c := upstream.New(50 * time.Millisecond)

	body, err := c.CallUpstream(context.Background(), fast.URL)
	fmt.Printf("fast: body=%q err=%v\n", body, err)

	start := time.Now()
	_, err = c.CallUpstream(context.Background(), slow.URL)
	fmt.Printf("slow: timeout=%v elapsed<200ms=%v\n",
		errors.Is(err, upstream.ErrUpstreamTimeout), time.Since(start) < 200*time.Millisecond)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast: body="pong" err=<nil>
slow: timeout=true elapsed<200ms=true
```

### Tests

The tests use `httptest.Server` handlers that watch their own request context so
they stop when the client aborts. `TestSlowUpstreamTimesOut` asserts the error
`errors.Is` both `ErrUpstreamTimeout` and `context.DeadlineExceeded`, and that the
call returns near the timeout (not the 500ms server delay).
`TestFastUpstreamReturnsBody` asserts the happy path. `TestErrorStatusIsNotTimeout`
asserts a 500 maps to `ErrUpstreamStatus` and is *not* a timeout. `TestBodyIsClosed`
wraps the transport in a `RoundTripper` that hands back a body recording its own
`Close`, proving every path closes it.

Create `client_test.go`:

```go
package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func slowServer(t *testing.T, d time.Duration) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(d):
			io.WriteString(w, "late")
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestSlowUpstreamTimesOut(t *testing.T) {
	t.Parallel()
	s := slowServer(t, 500*time.Millisecond)
	c := New(40 * time.Millisecond)

	start := time.Now()
	_, err := c.CallUpstream(context.Background(), s.URL)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("err = %v, want ErrUpstreamTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded in chain", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("call took %v, want near the 40ms timeout, not the server delay", elapsed)
	}
}

func TestFastUpstreamReturnsBody(t *testing.T) {
	t.Parallel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "pong")
	}))
	t.Cleanup(s.Close)

	c := New(time.Second)
	body, err := c.CallUpstream(context.Background(), s.URL)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if string(body) != "pong" {
		t.Fatalf("body = %q, want %q", body, "pong")
	}
}

func TestErrorStatusIsNotTimeout(t *testing.T) {
	t.Parallel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(s.Close)

	c := New(time.Second)
	_, err := c.CallUpstream(context.Background(), s.URL)
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("err = %v, want ErrUpstreamStatus", err)
	}
	if errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("err = %v, must NOT be reported as a timeout", err)
	}
}

// closeSpyBody records whether Close was called.
type closeSpyBody struct {
	io.Reader
	closed *atomic.Bool
}

func (b closeSpyBody) Close() error {
	b.closed.Store(true)
	return nil
}

// spyTransport returns a canned 200 whose body records Close.
type spyTransport struct {
	closed *atomic.Bool
}

func (tr spyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       closeSpyBody{Reader: strings.NewReader("ok"), closed: tr.closed},
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

func TestBodyIsClosed(t *testing.T) {
	t.Parallel()
	var closed atomic.Bool
	c := &Client{HTTP: &http.Client{Transport: spyTransport{closed: &closed}}, PerCallTimeout: time.Second}

	if _, err := c.CallUpstream(context.Background(), "http://example.invalid/"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !closed.Load() {
		t.Fatal("response body was not closed")
	}
}
```

## Review

The client is correct when a slow dependency cannot exceed the caller's budget. The
timeout has to be derived per call and attached with `http.NewRequestWithContext`;
that is what makes `Do` return early with `context.DeadlineExceeded` in its chain
rather than blocking on the socket, which `TestSlowUpstreamTimesOut` proves by
asserting the call returns near 40ms, not the server's 500ms. The three failure
classes must stay distinct: a timeout is `ErrUpstreamTimeout` (wrapping
`DeadlineExceeded`), a non-2xx is `ErrUpstreamStatus`, and neither is confused for
the other — `TestErrorStatusIsNotTimeout` enforces the separation so retry policy
keys off the right cause.

The mistakes to avoid: using `http.NewRequest` instead of the context form (the
deadline fires but the syscall keeps running); forgetting to close the body on the
error paths (leaked connections, which `TestBodyIsClosed` guards via a spy
transport); and relabeling a 500 as a timeout. Draining with `io.Copy(io.Discard,
body)` before `Close` lets the transport reuse the connection. Run `go test -race`;
the concurrent transport and the spy's atomic flag are checked under the detector.

## Resources

- [http.NewRequestWithContext](https://pkg.go.dev/net/http#NewRequestWithContext) — attaching a context so the transport aborts the in-flight call at the deadline.
- [http.Client.Do](https://pkg.go.dev/net/http#Client.Do) — its documented contract that a cancelled context returns an error and closes the body.
- [context.WithTimeout](https://pkg.go.dev/context#WithTimeout) — deriving the per-call budget that inherits the caller's deadline.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — standing up the slow/fast/500 servers used in the tests.

---

Back to [01-request-budget-toolkit.md](01-request-budget-toolkit.md) | Next: [03-db-query-deadline.md](03-db-query-deadline.md)
