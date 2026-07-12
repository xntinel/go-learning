# 12. HTTP Client Instrumentation

When an HTTP request is slow, total latency tells you nothing useful. `net/http/httptrace` exposes hooks at every phase of the request lifecycle — DNS lookup, TCP connect, TLS handshake, first response byte, connection reuse — so you can pin the bottleneck to a specific phase. The hard part is wiring those hooks into a reusable `http.RoundTripper` that works transparently for every request, including reused keep-alive connections where the TCP and DNS phases are zero.

```text
clienttrace/
  go.mod
  transport.go
  collector.go
  transport_test.go
  cmd/demo/main.go
```

The package exposes an `InstrumentedTransport` that wraps any `http.RoundTripper` and calls a callback with a `RequestTiming` value after each round trip. A `Collector` accumulates timings and computes per-phase aggregate statistics.

## Concepts

### The `httptrace.ClientTrace` Hook Model

`httptrace.ClientTrace` is a struct of optional callback fields. Each field corresponds to a moment in the request lifecycle. You embed a pointer to the trace in the request's context with `httptrace.WithClientTrace`, and the `http.Transport` calls the hooks at the right moment:

```
DNSStart / DNSDone
ConnectStart / ConnectDone
TLSHandshakeStart / TLSHandshakeDone
GotConn            (reports whether a connection was reused)
GotFirstResponseByte
```

The hooks fire in the goroutine that owns the transport round trip, so they share the same stack and can write to local variables without a mutex.

### Wrapping `http.RoundTripper`

`http.RoundTripper` has one method:

```go
RoundTrip(*http.Request) (*http.Response, error)
```

A wrapper receives the request before the underlying transport sees it, injects the trace context, calls the inner transport, and collects timings from the hooks. The wrapper must not buffer or alter the response body — the contract of `RoundTripper` forbids modifying the request after calling the inner `RoundTrip`.

Mutating `req` in place is forbidden. `req.WithContext` returns a new `*http.Request` with the modified context, leaving the original unchanged.

### Connection Reuse and Zero-Duration Phases

When a keep-alive connection is reused, `ConnectStart` and `ConnectDone` do not fire. The phase duration stays at zero (the zero value of `time.Duration`). Guard every `time.Since` call by checking whether the start time was actually set:

```go
if !connectStart.IsZero() {
    timing.Connect = time.Since(connectStart)
}
```

`GotConn` fires for both new and reused connections. Its `GotConnInfo.Reused` field distinguishes the two cases, making it the canonical way to detect connection reuse.

### Aggregate Statistics: Min, Max, Mean, P99

A single request's timing is a data point; a fleet's timing needs aggregates. P99 (the 99th percentile) is the standard SLO boundary — it captures the worst outlier without being skewed by the median. Compute it by sorting a copy of the duration slice and indexing at `int(float64(n-1) * 0.99)`.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/33-tcp-udp-and-networking/12-http-client-instrumentation/12-http-client-instrumentation/cmd/demo
cd go-solutions/33-tcp-udp-and-networking/12-http-client-instrumentation/12-http-client-instrumentation
```

This is a library, not a program. Verification is with `go test`.

### Exercise 1: RequestTiming and InstrumentedTransport

Create `transport.go`:

```go
package clienttrace

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"time"
)

// RequestTiming holds the per-phase latency for a single HTTP request.
// Phases that do not fire (e.g. DNS and Connect on a reused connection)
// retain their zero value.
type RequestTiming struct {
	URL     string
	Reused  bool
	DNS     time.Duration
	Connect time.Duration
	TLS     time.Duration
	TTFB    time.Duration // time from request start to first response byte
	Total   time.Duration // time from request start to RoundTrip return
}

// InstrumentedTransport wraps an http.RoundTripper and calls onTiming
// synchronously after each round trip with the collected RequestTiming.
type InstrumentedTransport struct {
	inner    http.RoundTripper
	onTiming func(RequestTiming)
}

// NewInstrumentedTransport returns an *InstrumentedTransport.
// inner is the underlying transport; nil defaults to http.DefaultTransport.
// onTiming is called after every RoundTrip; a nil onTiming is a no-op.
func NewInstrumentedTransport(inner http.RoundTripper, onTiming func(RequestTiming)) *InstrumentedTransport {
	if inner == nil {
		inner = http.DefaultTransport
	}
	return &InstrumentedTransport{inner: inner, onTiming: onTiming}
}

// RoundTrip implements http.RoundTripper. It injects a ClientTrace into
// the request context, delegates to the inner transport, and fires onTiming.
func (t *InstrumentedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	timing := RequestTiming{URL: req.URL.String()}
	var (
		dnsStart     time.Time
		connectStart time.Time
		tlsStart     time.Time
		start        = time.Now()
	)

	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			timing.Reused = info.Reused
		},
		DNSStart: func(_ httptrace.DNSStartInfo) {
			dnsStart = time.Now()
		},
		DNSDone: func(_ httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				timing.DNS = time.Since(dnsStart)
			}
		},
		ConnectStart: func(_, _ string) {
			connectStart = time.Now()
		},
		ConnectDone: func(_, _ string, _ error) {
			if !connectStart.IsZero() {
				timing.Connect = time.Since(connectStart)
			}
		},
		TLSHandshakeStart: func() {
			tlsStart = time.Now()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			if !tlsStart.IsZero() {
				timing.TLS = time.Since(tlsStart)
			}
		},
		GotFirstResponseByte: func() {
			timing.TTFB = time.Since(start)
		},
	}

	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := t.inner.RoundTrip(req)
	timing.Total = time.Since(start)
	if t.onTiming != nil {
		t.onTiming(timing)
	}
	return resp, err
}
```

`req.WithContext` clones the request with the trace attached. The hooks capture start times in closure variables; `Total` is set after the inner `RoundTrip` returns.

### Exercise 2: Aggregate Metrics with Collector

Create `collector.go`:

```go
package clienttrace

import (
	"sort"
	"sync"
	"time"
)

// Stats holds aggregate statistics for one timing phase.
type Stats struct {
	Count int
	Min   time.Duration
	Max   time.Duration
	Mean  time.Duration
	P99   time.Duration
}

// Collector accumulates RequestTiming records and computes aggregate stats.
// It is safe for concurrent use.
type Collector struct {
	mu    sync.Mutex
	items []RequestTiming
}

// Record adds t to the collector.
func (c *Collector) Record(t RequestTiming) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, t)
}

// Count returns the number of recorded timings.
func (c *Collector) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// Summary returns per-phase aggregate statistics.
// Phase keys: "dns", "connect", "tls", "ttfb", "total".
func (c *Collector) Summary() map[string]Stats {
	c.mu.Lock()
	items := make([]RequestTiming, len(c.items))
	copy(items, c.items)
	c.mu.Unlock()

	pick := func(f func(RequestTiming) time.Duration) []time.Duration {
		out := make([]time.Duration, len(items))
		for i, it := range items {
			out[i] = f(it)
		}
		return out
	}
	return map[string]Stats{
		"dns":     calcStats(pick(func(t RequestTiming) time.Duration { return t.DNS })),
		"connect": calcStats(pick(func(t RequestTiming) time.Duration { return t.Connect })),
		"tls":     calcStats(pick(func(t RequestTiming) time.Duration { return t.TLS })),
		"ttfb":    calcStats(pick(func(t RequestTiming) time.Duration { return t.TTFB })),
		"total":   calcStats(pick(func(t RequestTiming) time.Duration { return t.Total })),
	}
}

// calcStats computes aggregate statistics for a slice of durations.
// An empty slice returns a zero Stats.
func calcStats(durations []time.Duration) Stats {
	n := len(durations)
	if n == 0 {
		return Stats{}
	}
	sorted := make([]time.Duration, n)
	copy(sorted, durations)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	idx := int(float64(n-1) * 0.99)
	return Stats{
		Count: n,
		Min:   sorted[0],
		Max:   sorted[n-1],
		Mean:  sum / time.Duration(n),
		P99:   sorted[idx],
	}
}
```

### Exercise 3: Test Suite

Create `transport_test.go`:

```go
package clienttrace

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newEchoServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	}))
}

func drainClose(resp *http.Response) {
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestRoundTripCapturesTimings(t *testing.T) {
	t.Parallel()

	srv := newEchoServer()
	defer srv.Close()

	var got RequestTiming
	tr := NewInstrumentedTransport(nil, func(rt RequestTiming) { got = rt })
	client := &http.Client{Transport: tr}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	drainClose(resp)

	if got.Total <= 0 {
		t.Errorf("Total = %v, want > 0", got.Total)
	}
	if got.TTFB <= 0 {
		t.Errorf("TTFB = %v, want > 0", got.TTFB)
	}
	if got.Connect < 0 {
		t.Errorf("Connect = %v, want >= 0", got.Connect)
	}
	if got.DNS < 0 {
		t.Errorf("DNS = %v, want >= 0", got.DNS)
	}
	if got.TLS != 0 {
		t.Errorf("TLS = %v, want 0 for plain HTTP", got.TLS)
	}
	if got.Reused {
		t.Errorf("Reused = true, want false on first request to fresh server")
	}
}

func TestRoundTripMarksReusedConnection(t *testing.T) {
	// Not parallel: uses a dedicated transport to isolate the connection pool.
	srv := newEchoServer()
	defer srv.Close()

	inner := &http.Transport{}
	var timings []RequestTiming
	tr := NewInstrumentedTransport(inner, func(rt RequestTiming) {
		timings = append(timings, rt)
	})
	client := &http.Client{Transport: tr}

	for i := 0; i < 2; i++ {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		drainClose(resp)
	}

	if len(timings) != 2 {
		t.Fatalf("want 2 timings, got %d", len(timings))
	}
	if timings[0].Reused {
		t.Errorf("first request: Reused = true, want false")
	}
	if !timings[1].Reused {
		t.Errorf("second request: Reused = false, want true")
	}
	if timings[1].Connect != 0 {
		t.Errorf("second request Connect = %v, want 0 on reused connection", timings[1].Connect)
	}
}

func TestRoundTripForwardsResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "instrumented")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "body")
	}))
	defer srv.Close()

	tr := NewInstrumentedTransport(nil, nil)
	client := &http.Client{Transport: tr}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("StatusCode = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Test"); got != "instrumented" {
		t.Errorf("X-Test = %q, want instrumented", got)
	}
}

func TestCollectorRecordAndSummary(t *testing.T) {
	t.Parallel()

	var c Collector
	for i := 0; i < 4; i++ {
		c.Record(RequestTiming{
			Total: time.Duration(i+1) * time.Millisecond,
			TTFB:  time.Duration(i) * time.Millisecond,
		})
	}

	if got := c.Count(); got != 4 {
		t.Fatalf("Count = %d, want 4", got)
	}
	s := c.Summary()
	if s["total"].Count != 4 {
		t.Errorf("total.Count = %d, want 4", s["total"].Count)
	}
	if s["total"].Min != time.Millisecond {
		t.Errorf("total.Min = %v, want 1ms", s["total"].Min)
	}
	if s["total"].Max != 4*time.Millisecond {
		t.Errorf("total.Max = %v, want 4ms", s["total"].Max)
	}
	wantMean := (1 + 2 + 3 + 4) * time.Millisecond / 4
	if s["total"].Mean != wantMean {
		t.Errorf("total.Mean = %v, want %v", s["total"].Mean, wantMean)
	}
}

func TestCalcStatsEmpty(t *testing.T) {
	t.Parallel()

	s := calcStats(nil)
	if s.Count != 0 {
		t.Errorf("Count = %d, want 0", s.Count)
	}
}

func TestCalcStatsSingleElement(t *testing.T) {
	t.Parallel()

	s := calcStats([]time.Duration{5 * time.Millisecond})
	if s.Min != 5*time.Millisecond || s.Max != 5*time.Millisecond || s.P99 != 5*time.Millisecond {
		t.Errorf("single-element stats wrong: %+v", s)
	}
}

func ExampleInstrumentedTransport() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	var got RequestTiming
	tr := NewInstrumentedTransport(nil, func(rt RequestTiming) { got = rt })
	client := &http.Client{Transport: tr}

	resp, err := client.Get(srv.URL)
	if err != nil {
		fmt.Println("error:", err)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	fmt.Println(got.Total > 0)
	fmt.Println(got.TTFB > 0)
	// Output:
	// true
	// true
}
```

Your turn: add `TestRoundTripNilOnTiming` that calls `NewInstrumentedTransport(nil, nil)`, makes a request, and asserts the request succeeds and the response status is 200. The test verifies that a nil `onTiming` callback does not panic.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"

	"example.com/clienttrace"
)

func main() {
	var col clienttrace.Collector
	tr := clienttrace.NewInstrumentedTransport(nil, col.Record)
	client := &http.Client{Transport: tr}

	urls := []string{
		"https://go.dev",
		"https://go.dev",
		"https://pkg.go.dev",
	}

	for _, u := range urls {
		resp, err := client.Get(u)
		if err != nil {
			log.Printf("GET %s: %v", u, err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		fmt.Printf("GET %-30s status=%d\n", u, resp.StatusCode)
	}

	fmt.Printf("\nRequests recorded: %d\n", col.Count())
	for phase, s := range col.Summary() {
		if s.Count == 0 {
			continue
		}
		fmt.Printf("  %-7s  min=%-8v mean=%-8v p99=%-8v max=%v\n",
			phase, s.Min, s.Mean, s.P99, s.Max)
	}
}
```

Run the demo with real network access:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Mutating `req` Directly

Wrong: writing `req.Header.Set(...)` before calling the inner transport, then calling the original `req` in the inner transport.

The `http.RoundTripper` contract forbids modifying the request after calling the inner `RoundTrip`. Any mutation must happen on a clone. `req.WithContext(ctx)` is a shallow clone specifically designed for this purpose.

Fix: always clone with `req = req.WithContext(...)` before setting headers or attaching a trace, then pass the clone to the inner transport.

### Using `time.Since(start)` Without a Zero Check

Wrong:

```go
DNSDone: func(_ httptrace.DNSDoneInfo) {
    timing.DNS = time.Since(dnsStart) // dnsStart may be zero
},
```

If `DNSStart` never fired (numeric IP address, reused connection), `dnsStart` is the zero `time.Time`. `time.Since` of the zero value returns a large positive duration — the time since the epoch — which is nonsense.

Fix:

```go
DNSDone: func(_ httptrace.DNSDoneInfo) {
    if !dnsStart.IsZero() {
        timing.DNS = time.Since(dnsStart)
    }
},
```

### Calling the Callback Before Closing the Response

Wrong: calling `t.onTiming(timing)` inside the `RoundTrip` before the caller has had a chance to read the response body. If `TTFB` is set but the body is not yet read, the "total" metric misses body transfer time.

This lesson measures `Total` as the time until `RoundTrip` returns, which is response-headers-received, not body-transfer-complete. That is the standard definition and is what the stdlib's own trace uses. If you need body transfer time, wrap `resp.Body` with a counting reader and compute the duration in its `Close` method.

### Sharing Start Times Across Concurrent Requests

Wrong: storing `dnsStart`, `connectStart`, etc. as struct fields of the transport and mutating them from the hooks. The transport is shared across concurrent requests, so hooks race.

Fix: declare start times as local variables inside `RoundTrip`. Each call to `RoundTrip` gets its own stack frame and its own closure over those local variables.

## Verification

From `~/go-exercises/clienttrace`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The test output confirms:
- Phase durations are non-negative for all requests.
- `Reused = false` on the first request, `true` on the second.
- `Connect = 0` on the reused connection.
- `TLS = 0` for plain HTTP.
- The instrumented transport does not alter status codes or headers.
- The collector's aggregate statistics match the input data.

## Summary

- `httptrace.ClientTrace` provides optional hook fields for every phase of the HTTP request lifecycle; unhook phases retain their zero value.
- Attach a trace to the request context with `httptrace.WithClientTrace`; the underlying transport calls the hooks.
- Wrap `http.RoundTripper` to instrument every request without changing the call site. Clone the request with `req.WithContext` before attaching the trace.
- Guard every `time.Since(startTime)` call with an `!startTime.IsZero()` check; unhook phases (DNS and Connect on reused connections) must not produce spurious durations.
- Store closure variables (`dnsStart`, `connectStart`, `tlsStart`) as locals in `RoundTrip`, not as struct fields, to avoid data races across concurrent requests.
- P99 is the standard aggregate boundary for SLO measurement; sort a copy of the duration slice and index at `int(float64(n-1) * 0.99)`.

## What's Next

Next: [gRPC Streaming](../13-grpc-streaming/13-grpc-streaming.md).

## Resources

- [net/http/httptrace](https://pkg.go.dev/net/http/httptrace) — complete hook reference and type signatures
- [Go Blog: Introducing HTTP Tracing](https://go.dev/blog/http-tracing) — canonical motivation and usage walkthrough
- [net/http.RoundTripper](https://pkg.go.dev/net/http#RoundTripper) — interface contract and restrictions on implementors
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — test server for local HTTP round trips
- [Go Code Review Comments: Contexts](https://go.dev/wiki/CodeReviewComments#contexts) — correct context propagation
