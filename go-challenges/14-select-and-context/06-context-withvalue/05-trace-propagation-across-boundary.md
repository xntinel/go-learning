# Exercise 5: Trace Propagation Across the HTTP Boundary

Context values die at the process edge. To make the godoc's "transits processes and
APIs" concrete, this exercise builds the two halves of a propagator: a
`http.RoundTripper` that reads the trace ID from an outbound request's context and
writes it to a header, and a server middleware that extracts that header back into
the receiving server's context. The trace crosses the wire as a header because a
context cannot.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
traceprop/                   independent module: example.com/traceprop
  go.mod
  traceprop.go               WithTraceID, TraceIDFromContext; Transport (RoundTripper);
                             Extract server middleware
  cmd/
    demo/
      main.go                seeds a trace, calls a test server, prints what it recovered
  traceprop_test.go          round-trips a trace client->server; no trace -> no header
```

Files: `traceprop.go`, `cmd/demo/main.go`, `traceprop_test.go`.
Implement: `WithTraceID`/`TraceIDFromContext`, a `Transport` `RoundTripper` that serializes the context trace into a header, and an `Extract` middleware that reads the header into the server context.
Test: an `httptest.NewServer` recovers the client-side trace; a request with no trace in context sends no header and the server sees an empty trace.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/06-context-withvalue/05-trace-propagation-across-boundary/cmd/demo
cd go-solutions/14-select-and-context/06-context-withvalue/05-trace-propagation-across-boundary
```

### Why a value cannot cross the wire on its own

A `context.Context` is an in-memory chain of Go values. It has no serialized form
and cannot travel over a socket, so the instant a request leaves the process, every
context value is gone. The only way a trace ID reaches the server is if the client
copies it *out* of the context and *into* a request header, and the server copies it
back *out* of the header and *into* a fresh context. That copy-out/copy-in pair is
exactly what a distributed-tracing propagator is; this exercise is a minimal one
over a single `traceparent`-style header.

The client half is a `RoundTripper` wrapper. `RoundTrip` must not mutate the request
it is given — the `http.RoundTripper` contract forbids it, and a shared request could
be retried — so it clones the request before adding the header. It reads the trace
from `req.Context()` and, only if present, sets the header; a request with no trace
sends no header, which the negative test asserts. Delegating to a configurable base
(defaulting to `http.DefaultTransport`) makes the wrapper composable with other
transports.

The server half is ordinary middleware: read the header, and if non-empty attach it
to the request context via `WithTraceID` so downstream handlers recover it through
`TraceIDFromContext` — the same accessor the client used, closing the loop. Note the
two contexts are entirely different objects on different machines; only the header
value connects them.

Create `traceprop.go`:

```go
package traceprop

import (
	"context"
	"net/http"
)

// TraceHeader is the wire header the trace ID is serialized into. Real systems
// use W3C "traceparent"; this is a minimal single-value stand-in.
const TraceHeader = "Traceparent"

type ctxKey struct{}

// WithTraceID attaches a trace ID to ctx.
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// TraceIDFromContext returns the trace ID, or "" and false if none is present.
func TraceIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

// Transport is a RoundTripper that serializes the outbound request's context
// trace ID into the TraceHeader. A nil Base uses http.DefaultTransport.
type Transport struct {
	Base http.RoundTripper
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if id, ok := TraceIDFromContext(req.Context()); ok && id != "" {
		// Clone before mutating: RoundTrip must not modify the caller's request.
		req = req.Clone(req.Context())
		req.Header.Set(TraceHeader, id)
	}
	return base.RoundTrip(req)
}

// Extract is server middleware that reads TraceHeader back into the request
// context, re-hydrating the trace on the receiving side of the boundary.
func Extract(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get(TraceHeader); id != "" {
			r = r.WithContext(WithTraceID(r.Context(), id))
		}
		next.ServeHTTP(w, r)
	})
}
```

### The demo

The demo stands up a real (loopback) `httptest.NewServer` running `Extract`, builds
a client whose transport is the wrapping `Transport`, seeds a trace into the
outbound request context, and prints the trace the server recovered — proving the
value crossed the boundary as a header.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/traceprop"
)

func main() {
	handler := traceprop.Extract(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := traceprop.TraceIDFromContext(r.Context())
		fmt.Fprintf(w, "server recovered trace: %s", id)
	}))
	srv := httptest.NewServer(handler)
	defer srv.Close()

	client := &http.Client{Transport: &traceprop.Transport{}}

	ctx := traceprop.WithTraceID(context.Background(), "trace-xyz")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := client.Do(req)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(string(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
server recovered trace: trace-xyz
```

### The tests

The positive test stands up an `httptest.NewServer` whose handler runs `Extract`
and records the recovered trace into a captured variable, then drives a client with
the wrapping `Transport` and asserts the recovered trace equals the seeded one. The
negative test seeds no trace and asserts both that the server saw an empty trace and
that no header was sent.

Create `traceprop_test.go`:

```go
package traceprop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTraceCrossesBoundary(t *testing.T) {
	t.Parallel()

	var recovered string
	var sawHeader string
	srv := httptest.NewServer(Extract(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(TraceHeader)
		id, _ := TraceIDFromContext(r.Context())
		recovered = id
	})))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{}}
	ctx := WithTraceID(context.Background(), "trace-42")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if sawHeader != "trace-42" {
		t.Fatalf("server header = %q, want trace-42", sawHeader)
	}
	if recovered != "trace-42" {
		t.Fatalf("server recovered = %q, want trace-42", recovered)
	}
}

func TestNoTraceSendsNoHeader(t *testing.T) {
	t.Parallel()

	var recovered string
	var sawHeader string
	srv := httptest.NewServer(Extract(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(TraceHeader)
		id, _ := TraceIDFromContext(r.Context())
		recovered = id
	})))
	defer srv.Close()

	client := &http.Client{Transport: &Transport{}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	if sawHeader != "" {
		t.Fatalf("server header = %q, want empty", sawHeader)
	}
	if recovered != "" {
		t.Fatalf("server recovered = %q, want empty", recovered)
	}
}
```

## Review

The propagator is correct when the trace the server recovers equals the trace the
client seeded, and when a request with no trace sends no header and the server sees
none. The conceptual point the tests make is that two entirely separate contexts —
one on the client, one on the server — are bridged only by a header value; nothing
about the context itself crosses the wire. Two implementation traps: mutating the
request inside `RoundTrip` instead of cloning it (which violates the transport
contract and can corrupt a retried request), and sending an empty header
unconditionally (which pollutes every request and defeats the "no trace means no
header" contract). Because the round trip goes over a real loopback socket, the test
also proves the header actually serializes rather than being read back in-process.

## Resources

- [net/http RoundTripper](https://pkg.go.dev/net/http#RoundTripper) — the contract, including "should not modify the request".
- [http.Request.Clone](https://pkg.go.dev/net/http#Request.Clone) — the correct way to derive a mutable copy in a RoundTripper.
- [W3C Trace Context](https://www.w3.org/TR/trace-context/) — the real `traceparent` header this exercise stands in for.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-auth-principal-guard.md](04-auth-principal-guard.md) | Next: [06-tenant-scoped-repository.md](06-tenant-scoped-repository.md)
