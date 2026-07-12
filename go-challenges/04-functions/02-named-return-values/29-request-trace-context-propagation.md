# Exercise 29: Distributed Trace Context Propagation to Response

A distributed trace is only useful if every response — success, validation
failure, or backend error — carries the trace id that ties it back to the
request that produced it. Attaching that header at each individual return
statement is the kind of repetition that eventually has an exception: the
one return statement added later that forgets. This exercise builds a
`Handle` that stamps the trace header from a single deferred closure keyed
on the named `resp` result, so every exit path gets it for free.

**Nivel: Intermedio** — validacion rapida (tres casos: id de cliente, id generado, error).

## What you'll build

```text
trace/                      independent module: example.com/trace
  go.mod
  trace.go                   Request; Response; Handler.Handle (deferred trace header stamp)
  cmd/demo/
    main.go                  runnable demo: client-supplied id, generated id, backend error
  trace_test.go               header present and correct on all three exit paths
```

- Files: `trace.go`, `cmd/demo/main.go`, `trace_test.go`.
- Implement: `(*Handler) Handle(req Request) (resp Response, err error)` whose deferred closure sets `resp.Headers["X-Trace-Id"]` on every exit, generating a new id via an injected `genID func() string` when the request did not supply one.
- Test: a request with a client trace id keeps it; one without gets a generated id; both the empty-path and backend-error failure exits still carry the header.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### One stamp, every exit, keyed on the named response

```go
defer func() {
    if resp.Headers == nil {
        resp.Headers = make(map[string]string)
    }
    resp.Headers[traceHeader] = traceID
}()

if req.Path == "" {
    return Response{}, errors.New("empty path")
}
```

`traceID` is resolved once, before the deferred closure is registered:
either the client's own id, or one from `genID` if the request didn't carry
one. Because `resp` is a named result, the deferred closure runs after each
return statement has copied its `Response` value into `resp` — including the
zero-value `Response{}` on a failure exit — and can attach the header to
whatever it finds, allocating the map first if that return statement left it
nil. The `id` generator is injected as a field on `Handler` rather than
called directly, the same way a clock would be, so tests can make trace id
assignment deterministic instead of asserting against a random UUID.

Create `trace.go`:

```go
package trace

import "errors"

// Request is an inbound request. TraceID is empty when the caller did not
// supply one and a new one must be generated.
type Request struct {
	TraceID string
	Path    string
}

// Response is what Handle produces. Headers always carries a trace id once
// Handle returns, on every exit path.
type Response struct {
	Body    string
	Headers map[string]string
}

const traceHeader = "X-Trace-Id"

// Handler dispatches requests to a backend and stamps every response with a
// distributed trace id.
type Handler struct {
	// genID produces a new trace id when the request did not carry one.
	// Injected so tests can make id generation deterministic.
	genID func() string
}

// NewHandler builds a Handler that generates ids with genID.
func NewHandler(genID func() string) *Handler {
	return &Handler{genID: genID}
}

// Handle processes req and stamps a trace id into resp.Headers.
//
// resp is a named result: a single deferred closure runs after every return
// statement has copied its value into resp, so it can attach the trace
// header whether the request path below succeeded, failed validation, or
// hit the backend error branch — one place decides the header, not one copy
// of it per return statement.
func (h *Handler) Handle(req Request) (resp Response, err error) {
	traceID := req.TraceID
	if traceID == "" {
		traceID = h.genID()
	}
	defer func() {
		if resp.Headers == nil {
			resp.Headers = make(map[string]string)
		}
		resp.Headers[traceHeader] = traceID
	}()

	if req.Path == "" {
		return Response{}, errors.New("empty path")
	}
	if req.Path == "/boom" {
		return Response{}, errors.New("backend unavailable")
	}
	return Response{Body: "ok: " + req.Path}, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/trace"
)

func main() {
	n := 0
	genID := func() string {
		n++
		return fmt.Sprintf("gen-%d", n)
	}
	h := trace.NewHandler(genID)

	resp, err := h.Handle(trace.Request{TraceID: "client-abc", Path: "/users"})
	fmt.Printf("with client trace id: body=%q err=%v trace=%s\n", resp.Body, err, resp.Headers["X-Trace-Id"])

	resp, err = h.Handle(trace.Request{Path: "/users"})
	fmt.Printf("without client trace id: body=%q err=%v trace=%s\n", resp.Body, err, resp.Headers["X-Trace-Id"])

	resp, err = h.Handle(trace.Request{Path: "/boom"})
	fmt.Printf("backend error: body=%q err=%v trace=%s\n", resp.Body, err, resp.Headers["X-Trace-Id"])
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
with client trace id: body="ok: /users" err=<nil> trace=client-abc
without client trace id: body="ok: /users" err=<nil> trace=gen-1
backend error: body="" err=backend unavailable trace=gen-2
```

### Tests

Create `trace_test.go`:

```go
package trace

import "testing"

func genIDStub(id string) func() string {
	return func() string { return id }
}

func TestHandlePreservesClientTraceID(t *testing.T) {
	t.Parallel()

	h := NewHandler(genIDStub("should-not-be-used"))
	resp, err := h.Handle(Request{TraceID: "client-1", Path: "/users"})
	if err != nil {
		t.Fatalf("Handle: unexpected error: %v", err)
	}
	if got := resp.Headers[traceHeader]; got != "client-1" {
		t.Fatalf("trace header = %q, want client-1", got)
	}
}

func TestHandleGeneratesTraceIDWhenMissing(t *testing.T) {
	t.Parallel()

	h := NewHandler(genIDStub("generated-1"))
	resp, err := h.Handle(Request{Path: "/users"})
	if err != nil {
		t.Fatalf("Handle: unexpected error: %v", err)
	}
	if got := resp.Headers[traceHeader]; got != "generated-1" {
		t.Fatalf("trace header = %q, want generated-1", got)
	}
}

func TestHandleStampsTraceIDOnErrorPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  Request
	}{
		{"empty path", Request{TraceID: "t1", Path: ""}},
		{"backend error", Request{TraceID: "t2", Path: "/boom"}},
	}

	h := NewHandler(genIDStub("unused"))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := h.Handle(tc.req)
			if err == nil {
				t.Fatal("Handle: want error, got nil")
			}
			if got := resp.Headers[traceHeader]; got != tc.req.TraceID {
				t.Fatalf("trace header = %q, want %q even on error", got, tc.req.TraceID)
			}
		})
	}
}
```

## Review

`Handle` is correct when every response — success, empty-path failure, and
backend-error failure — carries `X-Trace-Id`, and that id is the client's own
when supplied or the generator's otherwise. The deferred closure is what
makes this uniform: it runs once, after `resp` has been set by whichever
return statement fired, so a third failure branch added later inherits the
header for free instead of needing its own line to set it. The mistake to
avoid is resolving `traceID` *inside* the deferred closure by re-reading
`req.TraceID` — that reads correctly on the happy path, but on a version of
this function that mutates `req` before returning (say, redacting a field)
it would silently pick up the wrong value; resolving `traceID` once, before
the defer is registered, avoids depending on `req`'s state at return time.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [W3C Trace Context](https://www.w3.org/TR/trace-context/)
- [OpenTelemetry: Context propagation](https://opentelemetry.io/docs/concepts/context-propagation/)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-operation-duration-percentile-metric.md](28-operation-duration-percentile-metric.md) | Next: [30-range-loop-err-shadowing-defer-trap.md](30-range-loop-err-shadowing-defer-trap.md)
