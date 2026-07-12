# Exercise 7: Test a middleware chain — auth, request-ID, panic recovery

Cross-cutting concerns — authentication, request correlation, panic recovery —
live in middleware that wraps every handler. This module builds three composable
middlewares and tests them two ways: each one in isolation with a spy next-handler
(to pin the responsibility boundary between middleware and handler), and the full
composed chain end-to-end.

## What you'll build

```text
middlewarechain/                independent module: example.com/middleware-chain
  go.mod                        go 1.26
  middleware.go                 Auth, RequestID, Recover, Chain; RequestIDFromContext
  cmd/
    demo/
      main.go                   runs the chain with a valid and an invalid token
  middleware_test.go            per-middleware unit tests + full-chain recorder and server tests
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Auth(token)` (401 on missing/wrong bearer token), `RequestID(gen)` (inject an ID into the context and the `X-Request-ID` response header), `Recover(logf)` (recover a panic, log it, return 500), and `Chain` to compose them.
- Test: per-middleware unit tests with a spy next-handler recording whether it ran and what context it saw; a recovery test whose handler panics; a full-chain test asserting `X-Request-ID` is present with and without a valid token.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/09-httptest/07-middleware-chain/cmd/demo
cd go-solutions/12-testing-ecosystem/09-httptest/07-middleware-chain
```

### The responsibility boundary is what you test

A middleware's job is to decide whether — and in what modified context — the next
handler runs. So the sharpest test of a middleware is a *spy* next-handler: a stub
that records whether it was called and what it saw. `Auth` must call next on a
valid token and must *not* call it on a bad one (returning 401 itself); the spy's
`called` flag proves both directions. `RequestID` must put an ID into the request
context and echo it in a response header; the spy reads
`RequestIDFromContext(r.Context())` to prove the value propagated. `Recover` must
turn a downstream panic into a 500 without crashing the process; a spy that panics
proves the `recover` caught it and the test survived.

Two idioms matter. First, the context key is an unexported named type
(`type ctxKey int`), never a bare string, so no other package can collide with or
read the key — the standard `context.WithValue` discipline. Second, each
middleware is a `func(http.Handler) http.Handler` adapter, and `Chain` applies
them so the *first* listed is the outermost wrapper. Order is a design decision
you can see in the tests: `Recover` outermost (it must catch panics from anything
inside), then `RequestID` (so the correlation header is set even on a 401), then
`Auth` closest to the handler.

Create `middleware.go`:

```go
package middleware

import (
	"context"
	"net/http"
)

type ctxKey int

const requestIDKey ctxKey = iota

// Middleware wraps a handler with a cross-cutting concern.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares so that the first listed is the outermost wrapper.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// Auth rejects requests without an exact "Bearer <token>" Authorization header.
func Auth(token string) Middleware {
	want := "Bearer " + token
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequestID ensures every request carries a correlation ID: it reuses an inbound
// X-Request-ID or generates one, stores it in the context, and echoes it in the
// response header.
func RequestID(gen func() string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" {
				id = gen()
			}
			w.Header().Set("X-Request-ID", id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDFromContext returns the correlation ID stored by RequestID.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey).(string)
	return id, ok
}

// Recover turns a downstream panic into a 500 and logs it via logf, so a single
// bad request cannot crash the server.
func Recover(logf func(format string, args ...any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logf("panic recovered: %v", rec)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
```

### The demo

The demo builds the full chain around a trivial handler and runs it (through a
recorder) with a valid and an invalid token.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/middleware-chain"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := middleware.RequestIDFromContext(r.Context())
		fmt.Fprintf(w, "handled %s", id)
	})

	chain := middleware.Chain(handler,
		middleware.Recover(func(format string, args ...any) {}),
		middleware.RequestID(func() string { return "req-fixed" }),
		middleware.Auth("s3cr3t"),
	)

	for _, tok := range []string{"s3cr3t", "wrong"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		chain.ServeHTTP(rec, req)
		fmt.Printf("token=%s status=%d reqID=%s\n", tok, rec.Code, rec.Header().Get("X-Request-ID"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
token=s3cr3t status=200 reqID=req-fixed
token=wrong status=401 reqID=req-fixed
```

### Tests

The per-middleware tests use a spy next-handler run against a recorder in the test
goroutine (so reading the spy after `ServeHTTP` needs no synchronization). The
chain tests assert the composite behavior, including that `X-Request-ID` is set
even on the 401 because `RequestID` sits outside `Auth`. One chain test runs
through a real `httptest.Server` to confirm the composition survives the wire.

Create `middleware_test.go`:

```go
package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// spy records whether it ran and the request-ID it saw.
type spy struct {
	called bool
	gotID  string
	hadID  bool
}

func (s *spy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.called = true
	s.gotID, s.hadID = RequestIDFromContext(r.Context())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func TestAuthAllowsValidToken(t *testing.T) {
	t.Parallel()

	next := &spy{}
	h := Auth("s3cr3t")(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !next.called {
		t.Fatal("next handler not called on valid token")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestAuthRejectsMissingToken(t *testing.T) {
	t.Parallel()

	next := &spy{}
	h := Auth("s3cr3t")(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if next.called {
		t.Fatal("next handler called despite missing token")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRequestIDInjectsContextAndHeader(t *testing.T) {
	t.Parallel()

	next := &spy{}
	h := RequestID(func() string { return "req-fixed" })(next)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !next.hadID || next.gotID != "req-fixed" {
		t.Fatalf("context id = %q (present=%v), want req-fixed", next.gotID, next.hadID)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "req-fixed" {
		t.Fatalf("X-Request-ID header = %q, want req-fixed", got)
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	t.Parallel()

	var logged bool
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := Recover(func(format string, args ...any) { logged = true })(panicking)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // must not crash the test

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !logged {
		t.Fatal("panic was not logged")
	}
}

func TestChainSetsRequestIDEvenOn401(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := RequestIDFromContext(r.Context())
		_, _ = w.Write([]byte(id))
	})
	chain := Chain(handler,
		Recover(func(format string, args ...any) {}),
		RequestID(func() string { return "req-fixed" }),
		Auth("s3cr3t"),
	)

	// Without a valid token: 401, but RequestID (outside Auth) still set the header.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	chain.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "req-fixed" {
		t.Fatalf("X-Request-ID on 401 = %q, want req-fixed", got)
	}
}

func TestChainEndToEndServer(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := RequestIDFromContext(r.Context())
		_, _ = w.Write([]byte("handled " + id))
	})
	chain := Chain(handler,
		Recover(func(format string, args ...any) {}),
		RequestID(func() string { return "req-fixed" }),
		Auth("s3cr3t"),
	)

	srv := httptest.NewServer(chain)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Request-ID"); got != "req-fixed" {
		t.Fatalf("X-Request-ID = %q, want req-fixed", got)
	}
}
```

As a "your turn" addition, add a middleware that records the response status and a
test that asserts it observes the 401 from `Auth` (a `ResponseWriter` wrapper that
captures the code passed to `WriteHeader`).

## Review

Middleware tests are about the boundary: the spy next-handler is what proves `Auth`
gates correctly (called on a good token, not called on a bad one) and that
`RequestID` actually threads its value into the downstream context. Running those
unit tests against a recorder in the test goroutine keeps the spy race-free without
locks. The chain tests pin the two composition decisions that matter in
production: `Recover` outermost so no handler panic escapes, and `RequestID`
outside `Auth` so every response — even a rejected one — carries a correlation ID
for your logs. The `-race` run matters here because the end-to-end server test
touches the chain from a serving goroutine; the design (no shared mutable state in
the middlewares) is what keeps it clean.

## Resources

- [net/http `Handler`](https://pkg.go.dev/net/http#Handler) — the interface middleware wraps.
- [context `WithValue`](https://pkg.go.dev/context#WithValue) — request-scoped values and the unexported-key idiom.
- [Go blog: `defer`, `panic`, and `recover`](https://go.dev/blog/defer-panic-and-recover) — the mechanism behind the recovery middleware.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-tls-server-client.md](06-tls-server-client.md) | Next: [08-sse-streaming-flush.md](08-sse-streaming-flush.md)
