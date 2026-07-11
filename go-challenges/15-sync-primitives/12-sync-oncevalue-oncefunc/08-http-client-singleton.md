# Exercise 8: Shared HTTP Client: OnceValue vs init() vs Global Var

The incident behind this module is a classic: every handler built its own
`http.Client`, no connection was ever reused, and the box ran out of ephemeral
ports under load — the fix is one tuned client, constructed lazily and exactly
once, and this exercise builds it and proves the reuse.

## What you'll build

```text
outbound/                  independent module: example.com/outbound
  go.mod                   go mod init example.com/outbound
  outbound.go              New(build) over sync.OnceValue; Client(); newTunedClient with Transport tuning
  outbound_test.go         one-construction under 50 goroutines, pointer identity, connection-reuse proof, Example
  cmd/
    demo/
      main.go              runnable demo: identity across calls, transport settings, reuse against httptest
```

- Files: `outbound.go`, `outbound_test.go`, `cmd/demo/main.go`.
- Implement: `New(build func() *http.Client) func() *http.Client` over `sync.OnceValue`; package-level `Client = New(newTunedClient)`; `newTunedClient` with a tuned `http.Transport` (`MaxIdleConnsPerHost`, dial/TLS/idle timeouts).
- Test: constructor counter behind an injectable build func — 50 concurrent `Client()` calls, one construction, pointer identity; an `httptest.Server` counting distinct client source ports proves connections are reused; requests carry `t.Context()`.
- Verify: `go test -count=1 -race ./...` and `go run ./cmd/demo`.

Set up the module:

```bash
mkdir -p ~/go-exercises/outbound/cmd/demo
cd ~/go-exercises/outbound
go mod init example.com/outbound
```

### The incident, mechanically

`http.Client` and its `Transport` maintain a pool of idle keep-alive
connections. Build one client per request and each request dials a fresh TCP
connection (plus a TLS handshake against HTTPS backends), uses it once, and
abandons it — the pool dies with the client. Under load that is: added latency
on every call, a growing pile of sockets in `TIME_WAIT`, and eventually
`connect: cannot assign requested address` when the ephemeral port range is
exhausted. The default `http.DefaultClient` avoids the per-request pool but
has **no timeout** (`Timeout: 0` means wait forever) and a default
`MaxIdleConnsPerHost` of 2 — far too small for a service hammering one
upstream. The production answer is a single, tuned, shared client. The
question this module answers is *how to own that singleton*.

### OnceValue vs init() vs a plain package var

A plain `var client = newTunedClient()` or an `init()` runs at package
initialization: before `main`, before flags are parsed, before configuration
is loaded. If construction should read env or config (proxy settings, per-env
timeouts), it cannot — or worse, it silently reads defaults. Package-init
order across imports is defined but fragile to reason about, and the cost is
paid by every binary that links the package, including CLIs that never make
an outbound call. `sync.OnceValue` keeps everything the var gave you —
one shared instance, race-free publication — and moves construction to first
use: ordering-safe (config is loaded by then, or you warm it up explicitly in
`main`), free when unused, and testable, because the construction path is a
plain function you can wrap and count, as the tests here do via `New`.

The memory-model point deserves one sentence of respect: the once establishes
a happens-before edge from the completion of `newTunedClient` to every
`Client()` return, so all goroutines see the fully constructed client — the
`Transport`, its dialer, its pool — with no additional synchronization. A
hand-rolled lazy `var client *http.Client; if client == nil { client = ... }`
without a lock is a data race that mostly works until it corrupts a pool
under load; the once is the correct spelling of that intent.

The transport numbers are a starting point, not gospel: `MaxIdleConnsPerHost: 10`
suits a service talking to a handful of upstreams; a proxy hammering one
backend may want 100. What is not negotiable is *having* a `Timeout` on the
client (it bounds the whole exchange including body read) and dial/TLS
timeouts on the transport, because the zero values mean "hang forever on a
black-holed upstream".

Create `outbound.go`:

```go
// Package outbound owns the process-wide HTTP client for calls to upstream
// services: one tuned, shared client, constructed lazily and exactly once.
package outbound

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// New wraps build in sync.OnceValue: the returned getter constructs the
// client on first call and returns the same instance forever. Exported so
// tests (and other packages) can own their own counted instance.
func New(build func() *http.Client) func() *http.Client {
	return sync.OnceValue(build)
}

// Client returns the process-wide outbound client. First call constructs
// it; every later call returns the same *http.Client, whose Transport pools
// and reuses connections per host. Call it once from main after config is
// loaded if you want construction cost off the first request.
var Client = New(newTunedClient)

// newTunedClient builds the shared client. The zero-value alternatives are
// the incident: http.Client{} has no timeout, and the default transport
// keeps only 2 idle connections per host.
func newTunedClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout: 10 * time.Second, // whole exchange: connect, headers, body
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}
```

### The demo

The demo shows the three claims: `Client()` is identity-stable across calls,
the transport really carries the tuning, and — against a live
`httptest.Server` that records each request's source address — five
sequential requests arrive over one TCP connection.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/outbound"
)

func main() {
	c1 := outbound.Client()
	c2 := outbound.Client()
	fmt.Println("same client:", c1 == c2)

	tr := c1.Transport.(*http.Transport)
	fmt.Println("max idle conns per host:", tr.MaxIdleConnsPerHost)
	fmt.Println("client timeout:", c1.Timeout)

	ports := make(map[string]bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ports[r.RemoteAddr] = true // sequential requests: no lock needed
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()

	const requests = 5
	for range requests {
		resp, err := c1.Get(srv.URL)
		if err != nil {
			fmt.Println("get:", err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body) // drain so the conn returns to the pool
		_ = resp.Body.Close()
	}
	fmt.Printf("requests: %d, distinct source ports: %d\n", requests, len(ports))
	fmt.Println("connections reused:", len(ports) < requests)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
same client: true
max idle conns per host: 10
client timeout: 10s
requests: 5, distinct source ports: 1
connections reused: true
```

The drain-then-close on the body is not optional ceremony: a response body
that is not read to EOF cannot return its connection to the idle pool, and
the reuse silently disappears — a very common way to "have" a shared client
and still exhaust ports.

### Tests

`TestClientBuiltOnce` runs the counted constructor under 50 goroutines and
asserts one construction and pointer identity. `TestConnectionReuse` is the
behavioral proof of the fix: 20 sequential requests through one shared tuned
client against an `httptest.Server` land on at most 2 distinct source ports
(one, in practice; the bound leaves room for a scheduler-timed pool miss),
with each request built via `http.NewRequestWithContext(t.Context(), ...)`.

Create `outbound_test.go`:

```go
package outbound

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestClientBuiltOnce(t *testing.T) {
	t.Parallel()

	var builds atomic.Int64
	get := New(func() *http.Client {
		builds.Add(1)
		return newTunedClient()
	})

	clients := make([]*http.Client, 50)
	var wg sync.WaitGroup
	wg.Add(len(clients))
	for i := range clients {
		go func() {
			defer wg.Done()
			clients[i] = get()
		}()
	}
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Fatalf("constructor ran %d times under concurrency, want 1", got)
	}
	for i, c := range clients {
		if c != clients[0] {
			t.Fatalf("caller %d received a different *http.Client", i)
		}
	}
}

func TestConnectionReuse(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	ports := make(map[string]bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ports[r.RemoteAddr] = true
		mu.Unlock()
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()

	client := New(newTunedClient)()
	const requests = 20
	for i := range requests {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("request %d: drain: %v", i, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("request %d: close: %v", i, err)
		}
	}

	mu.Lock()
	distinct := len(ports)
	mu.Unlock()
	if distinct == 0 || distinct > 2 {
		t.Fatalf("%d requests used %d distinct source ports; want 1 (reuse), tolerating 2", requests, distinct)
	}
}

func TestTunedDefaults(t *testing.T) {
	t.Parallel()

	c := newTunedClient()
	if c.Timeout == 0 {
		t.Fatal("client has no timeout: a black-holed upstream hangs callers forever")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.Transport)
	}
	if tr.MaxIdleConnsPerHost <= 2 {
		t.Fatalf("MaxIdleConnsPerHost = %d, want more than the default 2", tr.MaxIdleConnsPerHost)
	}
}

func ExampleNew() {
	get := New(func() *http.Client { return &http.Client{} })
	fmt.Println(get() == get())
	// Output: true
}
```

Run the suite:

```bash
go test -count=1 -race ./...
```

## Review

The module holds when the two halves of the fix are separately proven: the
once half (one construction, pointer identity, under 50 goroutines and
`-race`) and the pooling half (20 requests, at most 2 source ports — delete
the body drain and watch that test fail, which is the fastest way to teach
the lesson). Review-time smells this module should train you to catch:
`http.Client{}` or `http.DefaultClient` on a production call path (no
timeout); a client constructed inside a handler or request-scoped struct;
`sync.OnceValue` invoked inline per call (`sync.OnceValue(build)()` — a
fresh once each time, construction on every request); and response bodies
closed without being drained. Note also what stays out of the singleton:
per-request concerns — context, tracing headers, retries — belong on the
request or in a wrapping `RoundTripper`, never baked into the shared client.

## Resources

- [net/http Client](https://pkg.go.dev/net/http#Client) — Timeout semantics and "Clients should be reused instead of created as needed".
- [net/http Transport](https://pkg.go.dev/net/http#Transport) — MaxIdleConnsPerHost, IdleConnTimeout, and the connection pool.
- [sync.OnceValue](https://pkg.go.dev/sync#OnceValue) — the lazy, happens-before-safe publication of the client.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-idempotent-close.md](07-idempotent-close.md) | Next: [09-metrics-registrar-oncefunc.md](09-metrics-registrar-oncefunc.md)
