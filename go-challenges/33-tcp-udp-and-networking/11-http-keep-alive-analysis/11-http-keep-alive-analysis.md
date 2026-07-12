# 11. HTTP Keep-Alive Analysis

HTTP/1.1 persistent connections are on by default in Go's `http.Transport`, but misunderstanding the rules for connection return, pool sizing, and idle eviction causes subtle production problems: file descriptor leaks, port exhaustion, and unexpected latency spikes on the first request after a quiet period. This lesson builds a connection reuse `Analyzer` backed by `net/http/httptrace`, exercises the pool configuration knobs, and pins the behavior with real tests against a local `httptest` server.

```text
keepalive/
  go.mod
  keepalive.go
  keepalive_test.go
  cmd/demo/main.go
```

## Concepts

### The Connection Pool in http.Transport

`http.Transport` maintains a pool of idle connections keyed by a `connectMethodKey` that encodes scheme, host, and port. When a request arrives the transport first tries to grab an idle connection from the pool. If none is available and `MaxConnsPerHost` has not been reached, it dials a new TCP connection. After the response body is fully consumed and closed, the transport offers the connection back to the pool.

Two capacity limits govern the pool:

- `MaxIdleConns` -- global cap across all hosts (default 100).
- `MaxIdleConnsPerHost` -- per-host cap (default is `DefaultMaxIdleConnsPerHost`, which equals 2). Exceeding this limit causes the transport to close the connection instead of pooling it. This is the most common misconfiguration for services that talk to a single backend at high concurrency.

### httptrace.GotConn: Observing Reuse

`net/http/httptrace` provides low-level hooks into the HTTP client lifecycle. The `GotConn` callback fires after the transport has obtained a connection, either from the pool or freshly dialed. `GotConnInfo.Reused` is `true` when the connection came from the pool.

Attach a `ClientTrace` to a request's context before calling `client.Do`:

```go
trace := &httptrace.ClientTrace{
	GotConn: func(info httptrace.GotConnInfo) {
		fmt.Println("reused:", info.Reused)
	},
}
ctx := httptrace.WithClientTrace(r.Context(), trace)
r = r.WithContext(ctx)
```

The trace is stored in the context, so it flows through redirects automatically.

### The Body-Drain Contract

A connection is returned to the idle pool only if the response body is fully read and then closed:

```go
io.Copy(io.Discard, resp.Body)
resp.Body.Close()
```

Closing the body without draining it leaves unread bytes in the socket buffer. The transport cannot safely reuse the connection and closes it instead. Every benchmarking exercise that omits this drain will see every request counted as new.

### DisableKeepAlives and Pool Eviction

`Transport.DisableKeepAlives = true` closes every connection immediately after use, guaranteeing a fresh TCP handshake for each request. This is useful for test isolation when you want each request to be measurably independent.

`Transport.IdleConnTimeout` starts a timer when a connection enters the idle pool. When the timer fires the connection is evicted and closed; the next request to that host must dial a new connection.

`Transport.CloseIdleConnections()` evicts the pool immediately and is the deterministic way to force a fresh dial without timing dependencies.

## Exercises

Set up the module:

```bash
mkdir -p go-solutions/33-tcp-udp-and-networking/11-http-keep-alive-analysis/11-http-keep-alive-analysis/cmd/demo
cd go-solutions/33-tcp-udp-and-networking/11-http-keep-alive-analysis/11-http-keep-alive-analysis
```

This is a library, not a program: there is no `main`. You verify it with `go test`.

### Exercise 1: The Analyzer Type

Create `keepalive.go`:

```go
package keepalive

import (
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Stats holds a snapshot of connection counts at a point in time.
type Stats struct {
	New    int
	Reused int
}

// Total returns the sum of new and reused connections.
func (s Stats) Total() int { return s.New + s.Reused }

// Analyzer wraps an http.Client and records connection reuse statistics
// via net/http/httptrace.GotConn. All exported methods are safe for
// concurrent use.
type Analyzer struct {
	mu     sync.Mutex
	stats  Stats
	client *http.Client
}

// New returns an Analyzer backed by rt. Pass nil to use a private
// *http.Transport with sensible defaults; the Analyzer shares no state
// with http.DefaultTransport.
func New(rt http.RoundTripper) *Analyzer {
	if rt == nil {
		rt = &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		}
	}
	return &Analyzer{client: &http.Client{Transport: rt}}
}

// Do executes req and records whether the underlying connection was reused
// from the idle pool or freshly dialed. The caller must drain and close the
// response body for the connection to return to the pool.
func (a *Analyzer) Do(req *http.Request) (*http.Response, error) {
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			a.mu.Lock()
			if info.Reused {
				a.stats.Reused++
			} else {
				a.stats.New++
			}
			a.mu.Unlock()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	return a.client.Do(req)
}

// Stats returns a point-in-time snapshot of connection counts.
func (a *Analyzer) Stats() Stats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

// Reset zeroes all connection counters without affecting the underlying
// transport's idle connection pool.
func (a *Analyzer) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stats = Stats{}
}
```

Defaults are set in the nil-transport branch; callers that pass their own `*http.Transport` retain full control over pool sizing and keep-alive settings. The mutex protects the stats fields because `GotConn` fires from the transport's internal goroutines.

### Exercise 2: Tests and the Example Function

Create `keepalive_test.go`:

```go
package keepalive

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// echoHandler returns a handler that replies 200 OK with an empty body.
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// doGet executes a GET request, drains the body, and closes it.
// Draining is required for the connection to return to the idle pool.
func doGet(t *testing.T, a *Analyzer, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := a.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func TestFirstRequestCreatesNewConnection(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(echoHandler())
	defer srv.Close()

	a := New(nil)
	doGet(t, a, srv.URL)

	s := a.Stats()
	if s.New != 1 || s.Reused != 0 {
		t.Fatalf("Stats = %+v, want New=1 Reused=0", s)
	}
}

func TestSequentialRequestsReuseConnection(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(echoHandler())
	defer srv.Close()

	a := New(nil)
	for i := 0; i < 3; i++ {
		doGet(t, a, srv.URL)
	}

	s := a.Stats()
	if s.New != 1 {
		t.Fatalf("Stats.New = %d, want 1 (single dial for 3 sequential requests)", s.New)
	}
	if s.Reused != 2 {
		t.Fatalf("Stats.Reused = %d, want 2", s.Reused)
	}
}

func TestDisableKeepAlivesNeverReuses(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(echoHandler())
	defer srv.Close()

	tr := &http.Transport{DisableKeepAlives: true}
	a := New(tr)

	const n = 3
	for i := 0; i < n; i++ {
		doGet(t, a, srv.URL)
	}

	s := a.Stats()
	if s.New != n {
		t.Fatalf("Stats.New = %d, want %d (new connection per request)", s.New, n)
	}
	if s.Reused != 0 {
		t.Fatalf("Stats.Reused = %d, want 0", s.Reused)
	}
}

func TestResetClearsCounters(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(echoHandler())
	defer srv.Close()

	a := New(nil)
	for i := 0; i < 3; i++ {
		doGet(t, a, srv.URL)
	}

	a.Reset()
	s := a.Stats()
	if s.New != 0 || s.Reused != 0 {
		t.Fatalf("Stats after Reset = %+v, want {New:0 Reused:0}", s)
	}
}

func TestStatsTotalIsSum(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(echoHandler())
	defer srv.Close()

	a := New(nil)
	for i := 0; i < 4; i++ {
		doGet(t, a, srv.URL)
	}

	s := a.Stats()
	if s.Total() != s.New+s.Reused {
		t.Fatalf("Total() = %d, want New+Reused = %d+%d", s.Total(), s.New, s.Reused)
	}
	if s.Total() != 4 {
		t.Fatalf("Total() = %d, want 4", s.Total())
	}
}

func ExampleStats_Total() {
	s := Stats{New: 1, Reused: 4}
	fmt.Printf("total=%d new=%d reused=%d\n", s.Total(), s.New, s.Reused)
	// Output: total=5 new=1 reused=4
}
```

Your turn: add `TestMaxIdleConnsPerHostLimitsPool`. Create two transports -- one with `MaxIdleConnsPerHost: 1` and one with `MaxIdleConnsPerHost: 10` -- then send five sequential requests to the same host with each, close idle connections between each request via `tr.CloseIdleConnections()`, and confirm both report `Reused = 0` after eviction (since both pools are cleared). This exercises the pool configuration path without introducing concurrency.

### Exercise 3: CLI Demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/keepalive"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	fmt.Println("--- keep-alive ON (default) ---")
	runSeries(srv.URL, nil, 5)

	fmt.Println("--- keep-alive OFF ---")
	tr := &http.Transport{DisableKeepAlives: true}
	runSeries(srv.URL, tr, 5)
}

func runSeries(url string, rt http.RoundTripper, n int) {
	a := keepalive.New(rt)
	for i := 0; i < n; i++ {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			log.Fatalf("NewRequest: %v", err)
		}
		resp, err := a.Do(req)
		if err != nil {
			log.Fatalf("Do: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	s := a.Stats()
	fmt.Printf("requests=%d new=%d reused=%d\n", s.Total(), s.New, s.Reused)
}
```

Run it with `go run ./cmd/demo` to see the keep-alive and no-keep-alive connection counts side by side.

## Common Mistakes

### Forgetting to Drain the Response Body

Wrong: close the body without reading it.

```go
resp, _ := client.Get(url)
resp.Body.Close()
```

What happens: the transport cannot verify the response stream is complete, so it closes the connection instead of returning it to the pool. Every subsequent request creates a new connection; `Stats.Reused` stays zero even for sequential requests to the same host.

Fix: drain with `io.Discard` before closing.

```go
io.Copy(io.Discard, resp.Body)
resp.Body.Close()
```

### MaxIdleConnsPerHost Left at the Default of 2

Wrong: sending 50 concurrent requests per second to a single microservice without tuning the transport.

What happens: `DefaultMaxIdleConnsPerHost` is 2. With 50 concurrent requests the pool holds at most 2 idle connections; the other 48 are closed after use, forcing new TCP dials on every goroutine that cannot grab an idle connection. File descriptor count grows and p99 latency spikes.

Fix: set `MaxIdleConnsPerHost` to match expected peak concurrency toward that host.

```go
tr := &http.Transport{
	MaxIdleConnsPerHost: 50,
	IdleConnTimeout:     90 * time.Second,
}
```

### Using http.DefaultTransport in Tests

Wrong: using `http.DefaultClient` or `http.DefaultTransport` when building an `Analyzer` in tests.

What happens: connections from a previous test case can already be in the pool. The first request in the new test sees `GotConn.Reused = true`, making the "first request creates a new connection" assertion fail non-deterministically.

Fix: allocate a fresh `*http.Transport` per test. Call `tr.CloseIdleConnections()` in a `defer` to clean up after the test.

```go
tr := &http.Transport{}
defer tr.CloseIdleConnections()
a := New(tr)
```

## Verification

From `~/go-exercises/keepalive`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. Then run the demo:

```bash
go run ./cmd/demo
```

Expected output shape:

```
--- keep-alive ON (default) ---
requests=5 new=1 reused=4
--- keep-alive OFF ---
requests=5 new=5 reused=0
```

Add your own test from the "Your turn" prompt before moving on.

## Summary

- `http.Transport` pools idle connections keyed by scheme + host + port; `MaxIdleConnsPerHost` (default 2) is the most commonly undertuned knob for single-backend services.
- `httptrace.ClientTrace.GotConn` fires after the transport acquires a connection; `GotConnInfo.Reused` is `true` for pool hits and `false` for fresh dials.
- The response body must be fully drained with `io.Discard` and then closed for the connection to return to the pool.
- `DisableKeepAlives: true` closes every connection after use; `CloseIdleConnections()` evicts the current pool without affecting future connections.
- `IdleConnTimeout` evicts connections idle longer than the configured duration; the next request to that host dials a new connection.

## What's Next

Next: [HTTP Client Instrumentation](../12-http-client-instrumentation/12-http-client-instrumentation.md).

## Resources

- [net/http/httptrace](https://pkg.go.dev/net/http/httptrace) -- GotConn and all other client lifecycle hooks
- [net/http.Transport](https://pkg.go.dev/net/http#Transport) -- full field documentation for pool knobs and keep-alive settings
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) -- local test servers for deterministic integration tests
- [RFC 9112 section 9.3 -- Persistence](https://www.rfc-editor.org/rfc/rfc9112#section-9.3) -- HTTP/1.1 persistent connections spec
