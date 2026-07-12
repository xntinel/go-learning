# Exercise 6: A Retrying http.RoundTripper That Honors Retry-After and Rebuffers Bodies

The cleanest place to add retries to an HTTP client is a `RoundTripper` wrapper: it
sits under `http.Client` and every request flows through it. But doing it correctly
means confronting two traps that break naive wrappers — a drained request body on the
second attempt, and ignoring the server's `Retry-After` backpressure. This module
builds a transport that gets both right.

This module is fully self-contained: its own `go mod init`, all types inline, its
own demo and tests.

## What you'll build

```text
retryrt/                   independent module: example.com/retryrt
  go.mod                   go 1.26
  transport.go             Transport wrapping an http.RoundTripper with retries
  cmd/
    demo/
      main.go              runnable demo against an httptest server
  transport_test.go        tests: Retry-After honored; body replayed; bodies drained
```

Files: `transport.go`, `cmd/demo/main.go`, `transport_test.go`.
Implement: a `Transport` implementing `http.RoundTripper` that retries on 429/5xx, parses `Retry-After` (int seconds or `http.ParseTime`) to override backoff, rebuilds the body via `Request.GetBody`, and drains/closes discarded response bodies.
Test: an `httptest.Server` returning 503+`Retry-After: 1` then 200 (assert the transport waited ~1s of virtual delay); a POST whose body must be non-empty on the retried attempt; discarded bodies drained.
Verify: `go test -count=1 -race ./...`

```bash
mkdir -p go-solutions/10-error-handling/12-retry-patterns-with-backoff/06-retryable-http-transport/cmd/demo
cd go-solutions/10-error-handling/12-retry-patterns-with-backoff/06-retryable-http-transport
go mod edit -go=1.26
```

### Why a naive retrying transport corrupts POSTs

An `http.Request` body is an `io.ReadCloser`. The transport underneath reads it to
completion to send the request. If the request fails and you simply hand the *same*
`*http.Request` to `RoundTrip` again, its body reader is already at EOF — the retried
request is sent with an *empty body*. For a GET this is invisible; for a POST that
carries a JSON payload it is a silent data-loss bug that only manifests on the retry
path, which is exactly the path least exercised in tests.

`net/http` anticipates this with `Request.GetBody func() (io.ReadCloser, error)`. When
you build a request from a `bytes.Reader`, `bytes.Buffer`, or `strings.Reader`,
`http.NewRequest` populates `GetBody` for you; it returns a *fresh* reader over the
same bytes. A retrying transport must, before each resend, call `GetBody` and install
the fresh reader as `req.Body`. If `GetBody` is nil (the body is a non-replayable
stream), the request is *not* safely retryable and the transport must give up rather
than send an empty body.

The second trap is `Retry-After`. When a server returns 429 or 503 it may include a
`Retry-After` header telling you exactly how long to wait — either an integer number
of seconds (`Retry-After: 1`) or an HTTP-date (`Retry-After: Wed, 21 Oct 2025
07:28:00 GMT`, parsed with `http.ParseTime`). This is the server's explicit
backpressure and it must override your computed backoff. Parsing tries `strconv.Atoi`
first (the common integer form) and falls back to `http.ParseTime`.

Third, connection hygiene: every response you *discard* (a 503 you are about to
retry) must have its body drained and closed — `io.Copy(io.Discard, resp.Body)` then
`resp.Body.Close()` — or the underlying connection is not returned to the pool and
keep-alive is defeated, so each retry opens a fresh TCP connection.

To keep the tests fast and deterministic, the transport does not call `time.Sleep`
directly; it calls an injectable `wait func(ctx, d) error`. Production uses a real
timer; the test injects a recorder that returns immediately and captures the
durations, so a `Retry-After: 1` is asserted without a real one-second sleep. This is
the same clock-injection idea the final module generalizes.

Create `transport.go`:

```go
package retryrt

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Transport wraps a base RoundTripper with retries for idempotent-safe requests.
type Transport struct {
	Base        http.RoundTripper
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	// Wait sleeps for d or returns early if ctx ends. Injectable for tests.
	Wait func(ctx context.Context, d time.Duration) error
}

func (t *Transport) base() http.RoundTripper {
	if t.Base != nil {
		return t.Base
	}
	return http.DefaultTransport
}

func (t *Transport) wait(ctx context.Context, d time.Duration) error {
	if t.Wait != nil {
		return t.Wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// RoundTrip retries the request on 429/5xx, replaying the body via GetBody and
// honoring Retry-After. It never retries a request whose body cannot be replayed.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	var lastResp *http.Response
	var lastErr error

	for attempt := range t.MaxAttempts {
		// Rebuild the body for this attempt (except the first, which already has it).
		if attempt > 0 && req.Body != nil {
			if req.GetBody == nil {
				return nil, fmt.Errorf("cannot retry: request body has no GetBody")
			}
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("rebuild body: %w", err)
			}
			req.Body = body
		}

		resp, err := t.base().RoundTrip(req)
		if err != nil {
			lastErr = err
		} else {
			if !retryableStatus(resp.StatusCode) {
				return resp, nil
			}
			lastResp = resp
			lastErr = nil
		}

		if attempt == t.MaxAttempts-1 {
			break
		}

		delay := t.backoff(attempt)
		if lastResp != nil {
			if ra, ok := parseRetryAfter(lastResp.Header.Get("Retry-After")); ok {
				delay = ra
			}
			// Drain and close the discarded response so the connection is reused.
			_, _ = io.Copy(io.Discard, lastResp.Body)
			_ = lastResp.Body.Close()
			lastResp = nil
		}
		if err := t.wait(ctx, delay); err != nil {
			return nil, err
		}
	}

	if lastResp != nil {
		return lastResp, nil
	}
	return nil, lastErr
}

func (t *Transport) backoff(attempt int) time.Duration {
	d := float64(t.BaseDelay)
	for range attempt {
		d *= 2
	}
	if d > float64(t.MaxDelay) {
		d = float64(t.MaxDelay)
	}
	return time.Duration(d)
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// parseRetryAfter parses a Retry-After value: integer seconds or an HTTP-date.
func parseRetryAfter(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(v); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = 0
		}
		return d, true
	}
	return 0, false
}
```

### The runnable demo

The demo stands up an `httptest.Server` that returns 503 with `Retry-After: 1` on the
first hit and 200 afterward, wires a `Transport` with an instant `Wait` (so the demo
does not really sleep a second), and prints the status and how long the transport was
told to wait.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"time"

	"example.com/retryrt"
)

func main() {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()

	var waited time.Duration
	rt := &retryrt.Transport{
		MaxAttempts: 3,
		BaseDelay:   50 * time.Millisecond,
		MaxDelay:    time.Second,
		Wait: func(ctx context.Context, d time.Duration) error {
			waited += d // record instead of sleeping
			return nil
		},
	}
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()

	fmt.Printf("final status: %d\n", resp.StatusCode)
	fmt.Printf("server hits: %d\n", hits.Load())
	fmt.Printf("waited (from Retry-After): %v\n", waited)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
final status: 200
server hits: 2
waited (from Retry-After): 1s
```

### Tests

The tests inject a recording `Wait` so nothing sleeps for real. `TestRetryAfterHonored`
asserts the transport waited exactly the 1-second `Retry-After` (not its 50ms
computed backoff) and returned 200. `TestPOSTBodyReplayed` sends a POST with a byte
body; the server echoes the received length on each hit, and the test asserts the
body was non-empty on the *retried* attempt (proving `GetBody` worked).
`TestDiscardedBodyDrained` wraps the base transport to assert each discarded response
body was read to EOF and closed.

Create `transport_test.go`:

```go
package retryrt

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func recordingWait() (*Transport, *[]time.Duration) {
	var waits []time.Duration
	t := &Transport{
		MaxAttempts: 4,
		BaseDelay:   50 * time.Millisecond,
		MaxDelay:    time.Second,
		Wait: func(ctx context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	return t, &waits
}

func TestRetryAfterHonored(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt, waits := recordingWait()
	client := &http.Client{Transport: rt}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get err = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(*waits) != 1 {
		t.Fatalf("waits = %v, want exactly one wait", *waits)
	}
	if (*waits)[0] != time.Second {
		t.Fatalf("wait = %v, want 1s from Retry-After (not the 50ms backoff)", (*waits)[0])
	}
}

func TestPOSTBodyReplayed(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	var lengths []int64
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		<-mu
		lengths = append(lengths, n)
		mu <- struct{}{}
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt, _ := recordingWait()
	client := &http.Client{Transport: rt}

	payload := []byte(`{"amount":100}`)
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Do err = %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	<-mu
	got := append([]int64(nil), lengths...)
	mu <- struct{}{}
	if len(got) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(got))
	}
	for i, n := range got {
		if n != int64(len(payload)) {
			t.Fatalf("attempt %d: server saw body length %d, want %d (GetBody must replay body)", i, n, len(payload))
		}
	}
}

// countingRT wraps a base and asserts every response body it returns is later
// drained and closed by the retrying transport.
type countingRT struct {
	base   http.RoundTripper
	closed *atomic.Int32
}

type trackedBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b *trackedBody) Close() error {
	b.closed.Add(1)
	return b.ReadCloser.Close()
}

func (c *countingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(req)
	if resp != nil {
		resp.Body = &trackedBody{ReadCloser: resp.Body, closed: c.closed}
	}
	return resp, err
}

func TestDiscardedBodyDrainedAndClosed(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var closed atomic.Int32
	rt, _ := recordingWait()
	rt.Base = &countingRT{base: http.DefaultTransport, closed: &closed}
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get err = %v", err)
	}
	defer resp.Body.Close()

	// Two discarded 503 responses must have been closed by the transport.
	if closed.Load() < 2 {
		t.Fatalf("closed = %d, want >= 2 discarded bodies closed", closed.Load())
	}
}

func ExampleTransport() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	rt := &Transport{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Second,
		Wait: func(context.Context, time.Duration) error { return nil }}
	client := &http.Client{Transport: rt}
	resp, err := client.Get(srv.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(resp.StatusCode)
	// Output: 200
}
```

## Review

The transport is correct when a retried POST arrives at the server with its full body
on every attempt (the `GetBody` replay), when a `Retry-After: 1` produces exactly a
one-second wait rather than the computed 50ms backoff (server backpressure wins), and
when every discarded 503 body is drained and closed (connection reuse). The mistakes
this design forecloses: resending a request whose body reader is already at EOF
(empty body on retry), computing your own backoff over the server's `Retry-After`, and
leaking discarded response bodies. Run `go test -race`; the tests use an
`httptest.Server` and channels rather than real sleeps, so the whole suite is fast and
the injected `Wait` keeps timing deterministic.

## Resources

- [`net/http#Request`](https://pkg.go.dev/net/http#Request) — the `GetBody` field for replaying bodies.
- [`net/http#ParseTime`](https://pkg.go.dev/net/http#ParseTime) — parse an HTTP-date `Retry-After`.
- [`net/http#RoundTripper`](https://pkg.go.dev/net/http#RoundTripper) — the interface the transport implements.
- [MDN: Retry-After](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Retry-After) — the two header formats.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-idempotency-safe-retries.md](05-idempotency-safe-retries.md) | Next: [07-circuit-breaker-with-retry.md](07-circuit-breaker-with-retry.md)
