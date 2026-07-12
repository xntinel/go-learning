# Exercise 8: Per-Request Outbound Latency via httptrace

When an inbound request is slow, the first question is *which upstream hop* was
slow: DNS, TCP connect, TLS handshake, or the server's think time (time to first
byte). `net/http/httptrace` answers this by installing hooks on the outbound
request's context, without changing call semantics or the response. This exercise
builds an instrumented fetch that captures those phase timings and tags them with
the request id, so upstream latency is attributable per inbound request in
observability.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
tracefetch/                independent module: example.com/tracefetch
  go.mod                   go 1.26
  trace.go                 TraceRecord; RequestIDFrom; InstrumentedFetch
  cmd/
    demo/
      main.go              fetch with a request id, print the timing record
  trace_test.go            hooks-fired + monotonic-durations + id-propagated test
```

Files: `trace.go`, `cmd/demo/main.go`, `trace_test.go`.
Implement: `InstrumentedFetch(ctx, client, url)` attaching `httptrace.WithClientTrace` to capture DNS, connect, TLS, and TTFB, tagging a `TraceRecord` with the request id from `ctx`.
Test: an in-process upstream; assert `GotConn` and `GotFirstResponseByte` fired, durations are non-negative, the record carries the propagated request id, and the response body/status are unchanged.
Verify: `go test -count=1 -race ./...`

## The design

`httptrace.WithClientTrace(ctx, trace)` returns a child context carrying a
`*httptrace.ClientTrace` whose function fields the transport invokes at each phase
of the round trip: `DNSStart`/`DNSDone`, `ConnectStart`/`ConnectDone`,
`TLSHandshakeStart`/`TLSHandshakeDone`, `GotConn` (a usable connection is in
hand), and `GotFirstResponseByte` (the server's first byte arrived). You record a
start instant and, in each `*Done`/`Got*` hook, compute the elapsed duration.
Because the trace rides the request's context, cancellation still applies
normally — the instrumentation is transparent to call semantics and does not
touch the response body or status.

Two honest caveats the tests must respect. For an in-process `httptest` server the
client dials `127.0.0.1` by IP over plain HTTP, so the `DNSStart`/`DNSDone` and
TLS hooks never fire — those durations stay zero, which is correct, not a bug. So
the test asserts on the hooks that always fire for any successful HTTP round trip:
`GotConn` and `GotFirstResponseByte`. The `Connect` duration and `TTFB` are
asserted only to be non-negative (they can legitimately be sub-microsecond on
loopback, so any `> 0` assertion would be flaky).

The request id is pulled from the same `ctx` (via `RequestIDFrom`) and stamped on
the record, which is the whole point: the emitted timing is joinable to the
inbound request that caused it.

Because the hooks run on the transport's goroutines while the main goroutine reads
the record after `client.Do` returns, the shared fields need synchronization for
`-race` cleanliness. `client.Do` returns only after `GotFirstResponseByte` has
fired, but a mutex around the writes and the final read keeps the race detector
satisfied without relying on that ordering subtlety.

Create `trace.go`:

```go
package tracefetch

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 0

// WithRequestID stores id in ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request id stored in ctx, or "" if none.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// TraceRecord is the per-request outbound timing, tagged with the request id so
// it is attributable to the inbound request that caused it.
type TraceRecord struct {
	RequestID    string
	DNS          time.Duration
	Connect      time.Duration
	TLS          time.Duration
	TTFB         time.Duration
	GotConn      bool
	GotFirstByte bool
}

// InstrumentedFetch performs a GET under a ClientTrace that captures DNS,
// connect, TLS, and time-to-first-byte, returning the timing record, the
// status code, and any error. The trace rides ctx, so cancellation still
// applies and the response is otherwise untouched.
func InstrumentedFetch(ctx context.Context, client *http.Client, url string) (*TraceRecord, int, error) {
	rec := &TraceRecord{RequestID: RequestIDFrom(ctx)}

	var mu sync.Mutex
	start := time.Now()
	var dnsStart, connectStart, tlsStart time.Time

	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			mu.Lock()
			dnsStart = time.Now()
			mu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			mu.Lock()
			rec.DNS = time.Since(dnsStart)
			mu.Unlock()
		},
		ConnectStart: func(network, addr string) {
			mu.Lock()
			connectStart = time.Now()
			mu.Unlock()
		},
		ConnectDone: func(network, addr string, err error) {
			mu.Lock()
			rec.Connect = time.Since(connectStart)
			mu.Unlock()
		},
		TLSHandshakeStart: func() {
			mu.Lock()
			tlsStart = time.Now()
			mu.Unlock()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			mu.Lock()
			rec.TLS = time.Since(tlsStart)
			mu.Unlock()
		},
		GotConn: func(httptrace.GotConnInfo) {
			mu.Lock()
			rec.GotConn = true
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			rec.GotFirstByte = true
			rec.TTFB = time.Since(start)
			mu.Unlock()
		},
	}

	ctx = httptrace.WithClientTrace(ctx, trace)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return rec, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return rec, 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	mu.Lock()
	out := *rec
	mu.Unlock()
	return &out, resp.StatusCode, nil
}
```

## The runnable demo

The demo fetches an in-process endpoint with a request id in context and prints
whether the connection and first-byte hooks fired, plus the propagated id.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/tracefetch"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	ctx := tracefetch.WithRequestID(context.Background(), "req-77")
	rec, status, err := tracefetch.InstrumentedFetch(ctx, http.DefaultClient, srv.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	fmt.Printf("status=%d request_id=%s got_conn=%v got_first_byte=%v ttfb_ok=%v\n",
		status, rec.RequestID, rec.GotConn, rec.GotFirstByte, rec.TTFB >= 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 request_id=req-77 got_conn=true got_first_byte=true ttfb_ok=true
```

## Tests

`TestTraceCapturesPhases` asserts the always-fired hooks (`GotConn`,
`GotFirstResponseByte`), non-negative `Connect` and `TTFB` durations, the
propagated request id, and that the fetch still returns `200` (the instrumentation
did not alter call semantics). `TestTraceRespectsCancellation` confirms the trace
rides the request context by cancelling before the fetch and asserting the call
fails — the trace does not somehow bypass cancellation.

Create `trace_test.go`:

```go
package tracefetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceCapturesPhases(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	ctx := WithRequestID(context.Background(), "req-77")
	rec, status, err := InstrumentedFetch(ctx, http.DefaultClient, srv.URL)
	if err != nil {
		t.Fatalf("InstrumentedFetch: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if !rec.GotConn {
		t.Fatal("GotConn hook did not fire")
	}
	if !rec.GotFirstByte {
		t.Fatal("GotFirstResponseByte hook did not fire")
	}
	if rec.Connect < 0 {
		t.Fatalf("Connect = %v, want non-negative", rec.Connect)
	}
	if rec.TTFB < 0 {
		t.Fatalf("TTFB = %v, want non-negative", rec.TTFB)
	}
	if rec.RequestID != "req-77" {
		t.Fatalf("RequestID = %q, want req-77 (id must propagate from ctx)", rec.RequestID)
	}
}

func TestTraceRespectsCancellation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the fetch: the traced request must still observe it

	_, _, err := InstrumentedFetch(ctx, http.DefaultClient, srv.URL)
	if err == nil {
		t.Fatal("expected error from a pre-cancelled context, got nil")
	}
}
```

## Review

The instrument is correct when it captures phase timings without changing the
request: the hooks only record, and the fetch returns the same status and body it
would without a trace. The honest test is the restrained one — assert the hooks
that always fire (`GotConn`, `GotFirstResponseByte`) and non-negative durations,
not the DNS/TLS hooks that never fire on a loopback HTTP server, and never assert
a strictly-positive loopback duration (it can be sub-microsecond). The request id
stamped on the record is what makes the timing joinable to the inbound request.
Run with `-race`: the hooks run on transport goroutines, so the record's fields
are guarded by a mutex.

## Resources

- [`net/http/httptrace.WithClientTrace`](https://pkg.go.dev/net/http/httptrace#WithClientTrace) — installing hooks on the request context.
- [`net/http/httptrace.ClientTrace`](https://pkg.go.dev/net/http/httptrace#ClientTrace) — the full set of phase hooks and their signatures.
- [`httptrace.GotConnInfo`](https://pkg.go.dev/net/http/httptrace#GotConnInfo) — connection-acquisition detail passed to `GotConn`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-deadline-budget-retry-client.md](07-deadline-budget-retry-client.md) | Next: [09-bounded-body-read-guard.md](09-bounded-body-read-guard.md)
