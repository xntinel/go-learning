# Exercise 5: HTTP Middleware Built as a Closure Returning a Handler Literal

Every HTTP middleware in production Go is an anonymous function. A middleware is a
function that takes the next handler and returns a new handler — and that new
handler is a `http.HandlerFunc` literal closing over `next` and any captured
config. This module builds a real middleware stack: a `RequestID` middleware that
injects a correlation id into the request context, and a config-capturing `Timeout`
middleware, then chains them.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
middleware/                   module example.com/middleware
  go.mod
  middleware.go               RequestID, Timeout, Chain; RequestIDFromContext
  middleware_test.go          id reaches inner handler, chain order, timeout honored
  cmd/demo/main.go            drive a chained handler with httptest
```

- Files: `middleware.go`, `middleware_test.go`, `cmd/demo/main.go`.
- Implement: `RequestID(next http.Handler) http.Handler` returning a handler literal that injects an id into the context and echoes it in a header; `Timeout(d time.Duration) func(http.Handler) http.Handler` whose returned literal captures `d`; a `Chain` helper.
- Test: the injected id reaches the inner handler through the context and rides back on the response header; the chain composes in order; the timeout config captured by the literal is honored (fast handler passes, slow handler gets 503).
- Verify: `go test -count=1 -race ./...`

### A middleware is a closure returning a handler literal

`RequestID` has the plain constructor shape `func(next http.Handler) http.Handler`.
It returns an `http.HandlerFunc` — a function literal — that closes over `next`.
Each request that flows through reads or mints a request id, stores it in the
request context under an unexported key type (so no other package can collide with
it), echoes it in the `X-Request-ID` response header, and calls
`next.ServeHTTP(w, r.WithContext(ctx))`. The unexported `ctxKey` type is the
idiomatic guard against context-key collisions; `RequestIDFromContext` is the typed
accessor.

`Timeout` is the config-capturing variant: `Timeout(d time.Duration)` returns a
`func(http.Handler) http.Handler`, so the captured `d` is baked into the middleware
before any handler is wrapped. Its inner literal derives a `context.WithTimeout`
from the request, runs `next` against a *buffered* response writer in a goroutine,
and races the handler's completion against the context deadline. If the handler
finishes first, its buffered status/body are flushed to the real writer; if the
deadline fires first, the literal writes a 503. Buffering the downstream writer is
what keeps this race-free: the real `http.ResponseWriter` is touched by exactly one
goroutine, and the buffer by exactly the other, so there is no shared write to
race on.

Create `middleware.go`:

```go
package middleware

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 0

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// RequestID injects a correlation id into the request context and echoes it in
// the X-Request-ID response header. An incoming X-Request-ID is reused.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newID()
		}
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the id stored by RequestID, if present.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(code int) {
	if !b.wrote {
		b.status = code
		b.wrote = true
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}

// Timeout bounds each request to d. The returned middleware's handler literal
// captures d and serves next against a buffered writer, racing completion
// against the deadline.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()

			bw := &bufferedWriter{header: make(http.Header), status: http.StatusOK}
			done := make(chan struct{})
			go func() {
				next.ServeHTTP(bw, r.WithContext(ctx))
				close(done)
			}()

			select {
			case <-done:
				for k, vs := range bw.header {
					w.Header()[k] = vs
				}
				w.WriteHeader(bw.status)
				_, _ = w.Write(bw.body.Bytes())
			case <-ctx.Done():
				http.Error(w, "request timed out", http.StatusServiceUnavailable)
			}
		})
	}
}

// Chain wraps h with mws so that mws[0] is the outermost middleware.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
```

### The runnable demo

The demo chains `RequestID` (outermost) and `Timeout`, then drives the stack with
`httptest` so the output is deterministic. The request carries a fixed
`X-Request-ID`, so the id you see is stable.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/middleware"
)

func main() {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := middleware.RequestIDFromContext(r.Context())
		fmt.Fprintf(w, "handled request %s", id)
	})
	h := middleware.Chain(inner, middleware.RequestID, middleware.Timeout(time.Second))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-42")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	fmt.Println("status:", rec.Code)
	fmt.Println("header:", rec.Header().Get("X-Request-ID"))
	fmt.Println("body:", rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 200
header: req-42
body: handled request req-42
```

### Tests

`TestRequestIDReachesInner` asserts a supplied id flows through the context to the
inner handler and rides back on the response header. `TestRequestIDGenerated`
asserts an absent id is minted and the context value matches the response header.
`TestChainAndTimeoutFast` drives the full chain with a fast handler and expects a
200 whose body carries the id. `TestTimeoutHonorsConfig` proves the captured `d` is
honored: a slow handler under a tiny timeout returns 503.

Create `middleware_test.go`:

```go
package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func idEcho() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := RequestIDFromContext(r.Context())
		fmt.Fprint(w, id)
	})
}

func TestRequestIDReachesInner(t *testing.T) {
	t.Parallel()
	h := RequestID(idEcho())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Body.String(); got != "req-1" {
		t.Fatalf("inner handler saw id %q, want req-1", got)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "req-1" {
		t.Fatalf("response header id %q, want req-1", got)
	}
}

func TestRequestIDGenerated(t *testing.T) {
	t.Parallel()
	h := RequestID(idEcho())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if body == "" {
		t.Fatal("inner handler saw no generated id")
	}
	if hdr := rec.Header().Get("X-Request-ID"); hdr != body {
		t.Fatalf("response header %q != context id %q", hdr, body)
	}
}

func TestChainAndTimeoutFast(t *testing.T) {
	t.Parallel()
	h := Chain(idEcho(), RequestID, Timeout(time.Second))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "req-2" {
		t.Fatalf("body = %q, want req-2", got)
	}
}

func TestTimeoutHonorsConfig(t *testing.T) {
	t.Parallel()
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "late")
	})
	h := Timeout(2 * time.Millisecond)(slow)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func ExampleRequestID() {
	h := RequestID(idEcho())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", "req-9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Println(rec.Body.String())
	// Output: req-9
}
```

## Review

The middleware pattern is correct when each constructor returns a handler *literal*
that closes over exactly what it needs: `RequestID` over `next`, `Timeout` over both
`next` and the captured `d`. The tests prove the closed-over state does its job —
the id reaches the inner handler and the response header, the chain composes with
`RequestID` outermost, and the captured timeout turns a slow handler into a 503. The
one real hazard is the timeout implementation: writing to the same
`http.ResponseWriter` from both the handler goroutine and the timeout branch is a
data race, which is why the downstream handler writes to a private buffer and only
one goroutine ever touches the real writer. Run `-race` to confirm. Note the
unexported `ctxKey` type — using a string key here would risk a collision with
another package's context value.

## Resources

- [net/http: Handler and HandlerFunc](https://pkg.go.dev/net/http#Handler)
- [context.WithValue](https://pkg.go.dev/context#WithValue)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)
- [Go blog: Contexts and structs](https://go.dev/blog/context-and-structs)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-lazy-init-oncevalue-resource.md](04-lazy-init-oncevalue-resource.md) | Next: [06-errgroup-fanout-literals.md](06-errgroup-fanout-literals.md)
