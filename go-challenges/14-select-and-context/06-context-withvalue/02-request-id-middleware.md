# Exercise 2: Request-ID Injection Middleware

The on-ramp for every value-based observability hook is a middleware at the edge
that guarantees each request has a correlation ID. This one reads an incoming
`X-Request-ID`, or mints a fresh one when it is absent, stores it in the request
context under an unexported key, echoes it back in the response, and exposes an
accessor so handlers can read it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
requestid/                   independent module: example.com/requestid
  go.mod
  requestid.go               Handler middleware; RequestIDFromContext; New, NewWithGen
  cmd/
    demo/
      main.go                shows mint-and-echo for one request
  requestid_test.go          absent-header mints+echoes; supplied preserved; distinct IDs; no-middleware false
```

Files: `requestid.go`, `cmd/demo/main.go`, `requestid_test.go`.
Implement: a middleware that reads or mints `X-Request-ID`, stores it in context under an unexported key, and sets it on the response; plus `RequestIDFromContext(ctx) (string, bool)`.
Test: an absent header yields a non-empty generated ID visible to the handler and echoed in the response; a supplied ID is preserved end to end; two header-less requests get distinct IDs.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/requestid/cmd/demo
cd ~/go-exercises/requestid
go mod init example.com/requestid
```

### Why a middleware, an unexported key, and an injectable generator

A request ID is the textbook request-scoped value: every log line, every
downstream call, every error report in the request should carry it, and no handler
should have to accept it as a parameter. The middleware sets it exactly once at the
edge and it is readable everywhere below via `RequestIDFromContext`. The key is an
unexported `struct{}` for the usual reason — no other package can read or overwrite
this slot.

Minting uses `crypto/rand.Read` into 16 bytes and `hex.EncodeToString`, giving a
128-bit random ID with negligible collision probability; that is why the
distinct-IDs test can assert two header-less requests differ. But a random ID is
un-assertable in a test, so the generator is injected: `New` wires the real
`randomID`, and `NewWithGen` lets a test supply a deterministic function. This is
the standard seam for testing anything that mints randomness or reads the clock —
depend on a function value, default it to the real thing, override it in tests.

The middleware writes the response header *before* calling the next handler, so the
ID is present on the response even if the handler writes its body immediately.
Storing into the context uses `r.WithContext`, which returns a shallow copy of the
request with the new context; the original request is never mutated, matching the
`http.Handler` contract.

Create `requestid.go`:

```go
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// HeaderName is the request/response header the middleware reads and echoes.
const HeaderName = "X-Request-ID"

// ctxKey is the unexported context-key type for the request ID.
type ctxKey struct{}

// randomID mints a 128-bit hex request ID. crypto/rand.Read never returns an
// error on the platforms Go supports, so the error is intentionally ignored.
func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Handler injects a request ID into the context and echoes it on the response.
type Handler struct {
	gen  func() string
	next http.Handler
}

// New wraps next with the production random-ID generator.
func New(next http.Handler) *Handler {
	return &Handler{gen: randomID, next: next}
}

// NewWithGen wraps next with an injected generator, so tests can assert the
// generated-ID path deterministically.
func NewWithGen(next http.Handler, gen func() string) *Handler {
	return &Handler{gen: gen, next: next}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(HeaderName)
	if id == "" {
		id = h.gen()
	}
	w.Header().Set(HeaderName, id)
	ctx := context.WithValue(r.Context(), ctxKey{}, id)
	h.next.ServeHTTP(w, r.WithContext(ctx))
}

// RequestIDFromContext returns the request ID injected by the middleware. The
// second return is false when no middleware ran.
func RequestIDFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}
```

### The demo

The demo drives one request without a header through the middleware and prints the
ID the handler saw and the ID echoed on the response — they are the same. The
generator is fixed so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/requestid"
)

func main() {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := requestid.RequestIDFromContext(r.Context())
		fmt.Printf("handler saw: %s\n", id)
	})

	h := requestid.NewWithGen(inner, func() string { return "generated-abc" })

	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	fmt.Printf("response echoed: %s\n", rec.Header().Get(requestid.HeaderName))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
handler saw: generated-abc
response echoed: generated-abc
```

### The tests

`httptest.NewRecorder` plus a hand-built `*http.Request` drives the middleware over
a probe handler that captures what it saw. The absent-header case uses `NewWithGen`
so the generated ID is assertable and checks both the handler-visible ID and the
echoed header. The supplied-header case proves end-to-end preservation. The
distinct-IDs case uses the real `New` and asserts two header-less requests produce
different non-empty IDs.

Create `requestid_test.go`:

```go
package requestid

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func probe(seen *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, _ := RequestIDFromContext(r.Context())
		*seen = id
		w.WriteHeader(http.StatusOK)
	})
}

func TestAbsentHeaderMintsAndEchoes(t *testing.T) {
	t.Parallel()

	var seen string
	h := NewWithGen(probe(&seen), func() string { return "gen-123" })

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen != "gen-123" {
		t.Fatalf("handler saw %q, want gen-123", seen)
	}
	if got := rec.Header().Get(HeaderName); got != "gen-123" {
		t.Fatalf("response header = %q, want gen-123", got)
	}
}

func TestSuppliedHeaderIsPreserved(t *testing.T) {
	t.Parallel()

	var seen string
	h := NewWithGen(probe(&seen), func() string { return "must-not-be-used" })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(HeaderName, "client-req-9")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "client-req-9" {
		t.Fatalf("handler saw %q, want client-req-9", seen)
	}
	if got := rec.Header().Get(HeaderName); got != "client-req-9" {
		t.Fatalf("response header = %q, want client-req-9", got)
	}
}

func TestGeneratedIDsAreDistinct(t *testing.T) {
	t.Parallel()

	serve := func() string {
		var seen string
		New(probe(&seen)).ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
		return seen
	}

	a, b := serve(), serve()
	if a == "" || b == "" {
		t.Fatalf("empty generated id: a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("expected distinct ids, got %q twice", a)
	}
}

func TestNoMiddlewareReturnsFalse(t *testing.T) {
	t.Parallel()

	if id, ok := RequestIDFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); ok {
		t.Fatalf("RequestIDFromContext without middleware = %q,true; want false", id)
	}
}
```

## Review

The middleware is correct when the ID the handler reads, the ID echoed on the
response, and the ID the client supplied are all the same value — the tests assert
that identity in each direction. The injectable generator is what makes the
generated-ID path testable at all; a hard-coded `crypto/rand` call would force the
test to accept "any non-empty string", losing the exact-value assertion. Two traps
to avoid: setting the response header *after* calling `next` (a handler that writes
its body first would flush before the header is set), and mutating the incoming
request instead of using `r.WithContext` (which would violate the handler contract
and race under concurrent use). Run `go test -race`; the middleware holds no shared
state, so concurrent requests through one `Handler` are clean.

## Resources

- [net/http Request.WithContext](https://pkg.go.dev/net/http#Request.WithContext) — the correct way to attach a context to a request.
- [crypto/rand](https://pkg.go.dev/crypto/rand) — `rand.Read` for cryptographically random bytes.
- [encoding/hex](https://pkg.go.dev/encoding/hex#EncodeToString) — turning random bytes into a header-safe string.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-type-safe-meta-carrier.md](01-type-safe-meta-carrier.md) | Next: [03-context-aware-slog-handler.md](03-context-aware-slog-handler.md)
