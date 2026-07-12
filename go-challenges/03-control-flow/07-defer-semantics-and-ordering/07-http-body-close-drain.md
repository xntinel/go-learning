# Exercise 7: Downstream Client — Defer Body Close + Drain for Connection Reuse

A client calling a downstream service must, on *every* path, drain and close
`resp.Body` — otherwise the underlying TCP connection is not returned to the
keep-alive pool, and under load the transport runs out of connections. This
exercise builds the correct `defer func(){ io.Copy(io.Discard, resp.Body); resp.Body.Close() }()`
pattern, handles the non-2xx branch (which still must close), and proves with a
connection-wrapping `Transport` that `Close` fires on every branch including the
early error return.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
downstream/                  independent module: example.com/downstream
  go.mod                     module example.com/downstream
  downstream.go              ErrStatus, Fetch (drain+close on every path)
  cmd/
    demo/
      main.go                runnable demo: fetch a 200 body from an httptest server
  downstream_test.go         2xx body decoded; non-2xx still closes; Close fires on every path
```

- Files: `downstream.go`, `cmd/demo/main.go`, `downstream_test.go`.
- Implement: `Fetch(ctx, c *http.Client, url string) ([]byte, error)` that builds a request with `http.NewRequestWithContext`, does it, and in a deferred closure drains with `io.Copy(io.Discard, resp.Body)` then closes — before the status check, so it runs on the non-2xx early return too.
- Test (`httptest.Server` + a Close-recording `Transport`): a 2xx returns the decoded body; a 5xx returns `ErrStatus` but still closes the body; `Close` is called exactly once on both paths.
- Verify: `go test -count=1 -race ./...`

### Why draining, not just closing, returns the connection

The Go HTTP transport reuses a TCP connection for the next request only if the
current response body is *fully consumed and closed*. Closing without reading the
remaining bytes tells the transport the connection is in an unknown state, so it
closes the socket instead of pooling it. Do that on a hot path and every request
opens a fresh connection: you pay a TCP (and TLS) handshake per call, file
descriptors climb, and eventually `Transport.MaxConnsPerHost` or the OS limit
throttles you. The fix is to drain the body — `io.Copy(io.Discard, resp.Body)` —
before closing, so the transport sees a cleanly finished response and returns the
connection to the idle pool.

Two placement rules make this correct on every path. First, register the deferred
drain-and-close *immediately* after `c.Do` succeeds and *before* the status check,
so it runs even when the function returns early on a non-2xx status. A common bug
is to check the status first and `return` on 5xx before the `defer` is registered
— the error path then leaks the body. Second, use a closure, because the cleanup
is two calls (drain then close) that must run in order; a bare `defer resp.Body.Close()`
skips the drain.

`Fetch` treats any non-2xx as an error via the `ErrStatus` sentinel, but the
deferred cleanup still drains and closes that body, because a 4xx/5xx response has
a body too (an error document) and the connection is just as reusable once it is
drained.

Create `downstream.go`:

```go
package downstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrStatus reports a non-2xx response. Callers match it with errors.Is.
var ErrStatus = errors.New("unexpected status")

// Fetch GETs url and returns the body on a 2xx. On every path — success, non-2xx,
// and read error — it drains and closes resp.Body so the keep-alive connection is
// returned to the pool.
func Fetch(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}

	// Registered before the status check, so it runs on the non-2xx early return
	// too. Draining before Close is what lets the transport reuse the connection.
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: %d", ErrStatus, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}
```

### The runnable demo

The demo starts an `httptest` server that returns a small JSON body, fetches it,
and prints the result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/downstream"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	body, err := downstream.Fetch(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		fmt.Println("fetch:", err)
		return
	}
	fmt.Printf("body: %s\n", body)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
body: {"status":"ok"}
```

### Tests

The tests wrap the client's `Transport` in a `closeRecorder` that replaces each
response body with a `trackedBody` counting `Close` calls, so we can prove `Close`
fires on both the 2xx and 5xx paths. `TestFetchOK` decodes a 200 body.
`TestFetchNon2xx` asserts `ErrStatus` and that the body was still closed.

Create `downstream_test.go`:

```go
package downstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// trackedBody counts Close calls on a response body.
type trackedBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b *trackedBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

// closeRecorder wraps a RoundTripper and swaps in a trackedBody per response.
type closeRecorder struct {
	rt     http.RoundTripper
	closed *atomic.Int32
}

func (c *closeRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &trackedBody{ReadCloser: resp.Body, closed: c.closed}
	return resp, nil
}

func newTrackingClient(base *http.Client, closed *atomic.Int32) *http.Client {
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	return &http.Client{Transport: &closeRecorder{rt: rt, closed: closed}}
}

func TestFetchOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "payload-body")
	}))
	defer srv.Close()

	var closed atomic.Int32
	client := newTrackingClient(srv.Client(), &closed)

	body, err := Fetch(t.Context(), client, srv.URL)
	if err != nil {
		t.Fatalf("Fetch() = %v", err)
	}
	if string(body) != "payload-body" {
		t.Fatalf("body = %q, want payload-body", body)
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("Close called %d times, want 1", got)
	}
}

func TestFetchNon2xxStillCloses(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var closed atomic.Int32
	client := newTrackingClient(srv.Client(), &closed)

	_, err := Fetch(t.Context(), client, srv.URL)
	if !errors.Is(err, ErrStatus) {
		t.Fatalf("Fetch() = %v, want ErrStatus", err)
	}
	if got := closed.Load(); got != 1 {
		t.Fatalf("Close called %d times on the error path, want 1", got)
	}
}

func TestFetchBadURL(t *testing.T) {
	t.Parallel()

	// A malformed URL fails before any request goes out; no body to close.
	_, err := Fetch(context.Background(), http.DefaultClient, "://not-a-url")
	if err == nil {
		t.Fatal("expected an error for a malformed URL")
	}
}
```

## Review

The client is correct when `resp.Body` is drained and closed on every path — 2xx,
non-2xx, and read error — and `TestFetchNon2xxStillCloses` is the case that catches
the usual bug: checking the status and returning *before* registering the cleanup,
which leaks the error-response body. Two rules: register the deferred
drain-and-close immediately after `c.Do` returns without error and before any
status branch, and drain (`io.Copy(io.Discard, ...)`) before `Close` so the
connection is reusable, not just released. The Close-recording `Transport` is the
reusable technique for proving cleanup fires without a real network — you wrap the
body and count. In production the same `Fetch` shape sits under every outbound API
call, and skipping the drain is the quiet cause of "why is my service opening a new
connection per request".

## Resources

- [`net/http` Response](https://pkg.go.dev/net/http#Response) — "The client must close the response body" and the reuse contract.
- [`http.Client.Do` / `http.NewRequestWithContext`](https://pkg.go.dev/net/http#Client.Do) — issuing the request with a context.
- [`io.Copy` / `io.Discard`](https://pkg.go.dev/io#Copy) — draining the body cheaply.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — the in-memory downstream server.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-request-latency-timer.md](06-request-latency-timer.md) | Next: [08-mutex-unlock-scope.md](08-mutex-unlock-scope.md)
