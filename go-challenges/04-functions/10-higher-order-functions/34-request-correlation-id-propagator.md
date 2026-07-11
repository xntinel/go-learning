# Exercise 34: Request Correlation ID Factory and Propagator Middleware

**Nivel: Intermedio** — validacion rapida (un test corto).

Tracing a single request across logs from three different services only
works if every one of those services agrees on the same ID for that
request. `Middleware` is a factory that builds exactly that agreement
into a decorator: extract an incoming correlation ID if the caller
already supplied one, mint a fresh one via an injected generator if not,
thread it through `context.Context` for every downstream call to read,
and propagate it back out on the response.

## What you'll build

```text
correlate/                   independent module: example.com/correlate
  go.mod                     go 1.24
  correlate.go                 type Request, Response, Handler; func FromContext, Middleware
  correlate_test.go            propagate existing, generate when missing, preserve other headers, absence
  cmd/demo/
    main.go                  two requests: no header (generates), with header (reuses)
```

- Files: `correlate.go`, `correlate_test.go`, `cmd/demo/main.go`.
- Implement: `Request struct{ Headers map[string]string }`, `Response struct{ Headers map[string]string; Body string }`, `Handler func(ctx context.Context, req Request) Response`, `FromContext(ctx context.Context) (string, bool)`, and `Middleware(generate func() string) func(next Handler) Handler`.
- Test: an incoming request that already has the correlation header propagates that exact ID and never calls `generate`; a request without the header calls `generate` exactly once and that ID reaches both the handler's context and the response header; other response headers the handler set are preserved alongside the correlation header; `FromContext` on a plain context reports absence.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/correlate/cmd/demo
cd ~/go-exercises/correlate
go mod init example.com/correlate
go mod edit -go=1.24
```

### An unexported key type is the whole safety mechanism

`context.WithValue` accepts any comparable type as a key, which is
exactly the hazard: if this package used a plain `string` like
`"correlation-id"` as its context key, any other package doing the same
thing would silently collide, each overwriting or misreading the other's
value with no compiler error to catch it. `contextKey struct{}` closes
that hole — because the type itself is unexported, no other package can
construct a value of that type, so no other package can ever produce a
key that compares equal to `correlationIDKey`, no matter what string or
number it might otherwise have chosen. `FromContext` pairs with that: it
is the only sanctioned way to read the value back out, keeping the key
itself a private implementation detail that never needs to appear outside
this file.

`Middleware`'s shape — a factory (`func(generate) ...`) that returns a
decorator (`func(next Handler) Handler`) — is the standard middleware
form: the factory closes over configuration (here, the ID generator),
and the decorator it returns is what actually wraps a `Handler`, ready to
be composed with other middleware in a chain the same way Exercise 2's
HTTP middleware chain was.

Create `correlate.go`:

```go
package correlate

import "context"

// Request and Response model an HTTP-like exchange without depending on
// net/http, keeping this exercise a self-contained demonstration of the
// middleware pattern.
type Request struct {
	Headers map[string]string
}

type Response struct {
	Headers map[string]string
	Body    string
}

// Handler processes a Request under ctx and produces a Response.
type Handler func(ctx context.Context, req Request) Response

// HeaderName is the header carrying the correlation ID across a request.
const HeaderName = "X-Correlation-ID"

// contextKey is an unexported type so values this package stores in a
// context can never collide with a key from another package, even one
// also using a plain string as its key type.
type contextKey struct{}

var correlationIDKey = contextKey{}

// withCorrelationID returns a copy of ctx carrying id.
func withCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

// FromContext extracts the correlation ID stored in ctx, if any.
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(correlationIDKey).(string)
	return id, ok
}

// Middleware returns a factory-built decorator: it wraps a Handler so
// that every call either extracts an existing correlation ID from the
// request header, or mints a fresh one via generate, stores it in the
// request's context for the handler (and anything it calls) to read,
// and propagates it onto the response header so a caller — or the next
// hop in a call chain — can carry it forward too.
func Middleware(generate func() string) func(next Handler) Handler {
	return func(next Handler) Handler {
		return func(ctx context.Context, req Request) Response {
			id := req.Headers[HeaderName]
			if id == "" {
				id = generate()
			}

			ctx = withCorrelationID(ctx, id)
			resp := next(ctx, req)

			if resp.Headers == nil {
				resp.Headers = make(map[string]string)
			}
			resp.Headers[HeaderName] = id
			return resp
		}
	}
}
```

### The runnable demo

The demo wraps a handler that calls a simulated `downstream` function —
standing in for a call to another service further down the chain — to
show the correlation ID reaching code the middleware itself never
touches directly, purely through `ctx`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/correlate"
)

// downstream simulates a call further down the chain that also needs the
// correlation ID — reading it from ctx, the same way the handler that
// received the request would.
func downstream(ctx context.Context) {
	id, ok := correlate.FromContext(ctx)
	fmt.Printf("  downstream call sees correlation id=%q ok=%v\n", id, ok)
}

func main() {
	generated := 0
	generate := func() string {
		generated++
		return fmt.Sprintf("gen-%d", generated)
	}

	handler := correlate.Handler(func(ctx context.Context, req correlate.Request) correlate.Response {
		downstream(ctx)
		return correlate.Response{Body: "ok"}
	})

	withCorrelation := correlate.Middleware(generate)(handler)

	fmt.Println("request without an incoming correlation header:")
	resp1 := withCorrelation(context.Background(), correlate.Request{})
	fmt.Printf("  response header %s=%q\n", correlate.HeaderName, resp1.Headers[correlate.HeaderName])

	fmt.Println("request with an incoming correlation header:")
	resp2 := withCorrelation(context.Background(), correlate.Request{
		Headers: map[string]string{correlate.HeaderName: "client-supplied-id"},
	})
	fmt.Printf("  response header %s=%q\n", correlate.HeaderName, resp2.Headers[correlate.HeaderName])

	fmt.Printf("generate was called %d time(s)\n", generated)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request without an incoming correlation header:
  downstream call sees correlation id="gen-1" ok=true
  response header X-Correlation-ID="gen-1"
request with an incoming correlation header:
  downstream call sees correlation id="client-supplied-id" ok=true
  response header X-Correlation-ID="client-supplied-id"
generate was called 1 time(s)
```

`generate` runs exactly once across both requests — only the first
request lacked an incoming ID, so only it needed a freshly minted one.

### Tests

`TestMiddlewarePropagatesExistingHeaderWithoutGenerating` is the case
that matters most in a real system: a request arriving with an upstream
service's correlation ID must keep that exact ID, and `generate` must
never be called to override it. `TestMiddlewareGeneratesIDWhenHeaderMissing`
is the complementary case, checking the generated ID reaches both the
handler's context and the response header identically.
`TestMiddlewarePreservesOtherResponseHeaders` guards against a
too-eager implementation that replaces `resp.Headers` wholesale instead
of adding one key to whatever the handler already set.
`TestFromContextReportsAbsenceWhenNeverSet` pins down `FromContext`'s
contract on a context nobody has run through the middleware.

Create `correlate_test.go`:

```go
package correlate

import (
	"context"
	"testing"
)

func TestMiddlewarePropagatesExistingHeaderWithoutGenerating(t *testing.T) {
	t.Parallel()

	generateCalls := 0
	generate := func() string { generateCalls++; return "should-not-be-used" }

	var seenInContext string
	handler := Handler(func(ctx context.Context, req Request) Response {
		id, _ := FromContext(ctx)
		seenInContext = id
		return Response{}
	})

	withCorrelation := Middleware(generate)(handler)
	req := Request{Headers: map[string]string{HeaderName: "incoming-id"}}
	resp := withCorrelation(context.Background(), req)

	if generateCalls != 0 {
		t.Fatalf("generate called %d times, want 0 (header already had an ID)", generateCalls)
	}
	if seenInContext != "incoming-id" {
		t.Fatalf("handler saw correlation id %q in context, want %q", seenInContext, "incoming-id")
	}
	if resp.Headers[HeaderName] != "incoming-id" {
		t.Fatalf("response header = %q, want %q", resp.Headers[HeaderName], "incoming-id")
	}
}

func TestMiddlewareGeneratesIDWhenHeaderMissing(t *testing.T) {
	t.Parallel()

	generateCalls := 0
	generate := func() string { generateCalls++; return "generated-id" }

	var seenInContext string
	handler := Handler(func(ctx context.Context, req Request) Response {
		id, _ := FromContext(ctx)
		seenInContext = id
		return Response{}
	})

	withCorrelation := Middleware(generate)(handler)
	resp := withCorrelation(context.Background(), Request{})

	if generateCalls != 1 {
		t.Fatalf("generate called %d times, want 1", generateCalls)
	}
	if seenInContext != "generated-id" {
		t.Fatalf("handler saw correlation id %q in context, want %q", seenInContext, "generated-id")
	}
	if resp.Headers[HeaderName] != "generated-id" {
		t.Fatalf("response header = %q, want %q", resp.Headers[HeaderName], "generated-id")
	}
}

func TestMiddlewarePreservesOtherResponseHeaders(t *testing.T) {
	t.Parallel()

	handler := Handler(func(ctx context.Context, req Request) Response {
		return Response{Headers: map[string]string{"Content-Type": "text/plain"}, Body: "hi"}
	})

	withCorrelation := Middleware(func() string { return "id" })(handler)
	resp := withCorrelation(context.Background(), Request{})

	if resp.Headers["Content-Type"] != "text/plain" {
		t.Fatalf("Content-Type header = %q, want %q", resp.Headers["Content-Type"], "text/plain")
	}
	if resp.Headers[HeaderName] != "id" {
		t.Fatalf("%s header = %q, want %q", HeaderName, resp.Headers[HeaderName], "id")
	}
	if resp.Body != "hi" {
		t.Fatalf("Body = %q, want %q", resp.Body, "hi")
	}
}

func TestFromContextReportsAbsenceWhenNeverSet(t *testing.T) {
	t.Parallel()

	id, ok := FromContext(context.Background())
	if ok {
		t.Fatalf("FromContext() ok = true, want false (id = %q)", id)
	}
}
```

## Review

`Middleware` is correct because the "did the request already have an ID"
check happens exactly once, at the top, and every subsequent step —
storing it in `ctx`, propagating it to the response — uses that one
resolved `id` rather than re-deriving it. Reusing an unexported
`contextKey` type instead of a raw string is not a stylistic nicety here;
it is the only thing standing between this package's context value and a
silent collision with any other package's `context.WithValue` call using
the same string key. The nil-check before writing to `resp.Headers`
matters because a handler that never sets any response headers leaves
that map nil, and writing to a nil map panics — allocating it lazily,
only when the middleware actually needs to add a key, is what keeps a
header-free handler working without every handler being forced to
pre-allocate a map it may never use.

## Resources

- [context package](https://pkg.go.dev/context) — `WithValue`, `Context.Value`, and the package doc's own warning about using an unexported key type.
- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the factory-returns-decorator shape `Middleware` builds on.
- [W3C Trace Context](https://www.w3.org/TR/trace-context/) — a standardized correlation/trace ID propagation format for the same problem this exercise solves with a single custom header.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-schema-validate-transform-chain.md](33-schema-validate-transform-chain.md) | Next: [../11-defer-stacking-and-resource-cleanup/00-concepts.md](../11-defer-stacking-and-resource-cleanup/00-concepts.md)
