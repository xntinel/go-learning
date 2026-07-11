# Exercise 6: Request-ID propagation through context and headers

Trace correlation starts with a request id: one identifier attached to every log
line and echoed to the client so a support ticket can be tied to server logs. This
module builds a `RequestID` middleware that reuses an inbound id or generates one,
stores it on the request context under a typed key, and exposes a safe accessor.

Fully self-contained: its own `go mod init`, demo, and tests. Nothing here imports
another exercise.

## What you'll build

```text
requestid/                   independent module: example.com/requestid
  go.mod                     go 1.26
  middleware.go              RequestID middleware + typed ctx key + FromContext accessor
  cmd/demo/main.go           runnable demo printing the generated id and the echoed header
  middleware_test.go         generate, echo, preserve inbound, FromContext on bare ctx
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `RequestID` reading an inbound `X-Request-ID` or generating one via `crypto/rand`+`hex`, storing it under an unexported context key, setting it on the response header, and a `FromContext(ctx) (string, bool)` accessor.
- Test: no inbound header generates a non-empty id echoed on the response and retrievable in the handler; an inbound id is preserved unchanged; `FromContext` on a bare context returns `("", false)`; the id is stable within one request.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/requestid/cmd/demo
cd ~/go-exercises/requestid
go mod init example.com/requestid
go mod edit -go=1.26
```

### Typed context keys and the rewrap

Request-scoped values ride on the request's `context.Context`. The middleware
computes the id, stores it with `context.WithValue(r.Context(), key, id)`, and —
this step is easy to forget — rebuilds the request with `r = r.WithContext(ctx)`
before calling `next`, because `WithContext` returns a *shallow copy*; mutating
`r.Context()` in place is not possible, you must pass the new request down.

The key must be an *unexported named type*, never a bare string. If two packages
both used `ctx.Value("request_id")`, they would read and overwrite each other's
value in the same context — a silent cross-package collision. An unexported
`type ctxKey struct{}` (or `type ctxKey int`) is unique to this package: no other
package can construct a value of it, so the key cannot clash. Callers never touch
the key directly; they use the typed `FromContext` accessor, which does the
`Value` lookup and the comma-ok type assertion in one place and returns
`("", false)` when the id is absent (for example, in code paths that ran before or
outside the middleware).

Generating the id uses `crypto/rand.Read` into a byte slice and
`hex.EncodeToString`. `crypto/rand` (not `math/rand`) is the right choice: request
ids should be unguessable so a client cannot spoof another's trace, and since Go
1.24 `crypto/rand.Read` never fails on supported platforms, so the error path is a
belt-and-suspenders fallback rather than a live concern.

Create `middleware.go`:

```go
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type Handler = http.Handler

type Middleware func(Handler) Handler

// ctxKey is unexported so no other package can collide with this key.
type ctxKey struct{}

const headerName = "X-Request-ID"

// FromContext returns the request id stored by RequestID, or ("", false) if none.
func FromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKey{}).(string)
	return id, ok
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown" // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// RequestID reuses an inbound X-Request-ID or generates one, stores it on the
// request context, and echoes it on the response so clients can correlate.
func RequestID(next Handler) Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerName)
		if id == "" {
			id = newID()
		}
		w.Header().Set(headerName, id)
		ctx := context.WithValue(r.Context(), ctxKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
```

### The runnable demo

The demo runs one request with no inbound id, so the middleware generates one;
because the id is random, the demo prints only its *length* (32 hex chars) and
that the response header equals the id the handler saw, keeping the output
deterministic.

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
	var seenInHandler string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := requestid.FromContext(r.Context())
		if ok {
			seenInHandler = id
		}
	})

	rec := httptest.NewRecorder()
	requestid.RequestID(final).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/", nil))

	echoed := rec.Header().Get("X-Request-ID")
	fmt.Printf("generated id length: %d\n", len(seenInHandler))
	fmt.Printf("handler id == echoed header: %t\n", seenInHandler == echoed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
generated id length: 32
handler id == echoed header: true
```

### Tests

`TestGeneratesAndEchoes` proves a missing inbound header yields a non-empty id
that appears both in the handler's context and the response header, identically.
`TestPreservesInboundID` proves an inbound `X-Request-ID` is passed through
unchanged (never regenerated). `TestFromContextBareContext` proves the accessor
returns `("", false)` when no middleware ran. `TestIDStableWithinRequest` pins that
the same id is observed in the handler and on the response header.

Create `middleware_test.go`:

```go
package requestid

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGeneratesAndEchoes(t *testing.T) {
	t.Parallel()

	var inHandler string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inHandler, _ = FromContext(r.Context())
	})

	rec := httptest.NewRecorder()
	RequestID(final).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if inHandler == "" {
		t.Fatal("no id generated in context")
	}
	if got := rec.Header().Get("X-Request-ID"); got != inHandler {
		t.Fatalf("echoed header %q != context id %q", got, inHandler)
	}
}

func TestPreservesInboundID(t *testing.T) {
	t.Parallel()

	const inbound = "abc123-trace"
	var inHandler string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inHandler, _ = FromContext(r.Context())
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-ID", inbound)
	RequestID(final).ServeHTTP(rec, req)

	if inHandler != inbound {
		t.Fatalf("context id = %q, want inbound %q (must not regenerate)", inHandler, inbound)
	}
	if got := rec.Header().Get("X-Request-ID"); got != inbound {
		t.Fatalf("echoed header = %q, want %q", got, inbound)
	}
}

func TestFromContextBareContext(t *testing.T) {
	t.Parallel()

	id, ok := FromContext(context.Background())
	if ok || id != "" {
		t.Fatalf("FromContext(bare) = %q,%v; want \"\",false", id, ok)
	}
}

func TestIDStableWithinRequest(t *testing.T) {
	t.Parallel()

	var first, second string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first, _ = FromContext(r.Context())
		second, _ = FromContext(r.Context())
	})

	RequestID(final).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if first != second || first == "" {
		t.Fatalf("id not stable within request: %q vs %q", first, second)
	}
}
```

## Review

The middleware is correct when it reuses an inbound id verbatim, generates an
unguessable one otherwise, and makes the *same* value visible in three places: the
request context, the response header, and every downstream read. The two guardrails
worth internalizing: the key is an unexported type so no other package can collide
(a plain string key is the classic context bug), and the request is rewrapped with
`r.WithContext(ctx)` — skip that and the value never reaches the handler because
`WithContext` returns a copy. `FromContext` returning `("", false)` on a bare
context is what lets callers safely read the id in code paths that might run
without the middleware. Use `crypto/rand`, not `math/rand`, so ids cannot be
predicted and spoofed.

## Resources

- [context#WithValue](https://pkg.go.dev/context#WithValue) — attaching a request-scoped value; note the unexported-key guidance.
- [net/http#Request.WithContext](https://pkg.go.dev/net/http#Request.WithContext) — the shallow-copy rewrap that propagates the new context.
- [crypto/rand#Read](https://pkg.go.dev/crypto/rand#Read) — cryptographically secure random bytes for the id.
- [encoding/hex#EncodeToString](https://pkg.go.dev/encoding/hex#EncodeToString) — rendering the random bytes as a hex string.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-status-capturing-responsewriter.md](05-status-capturing-responsewriter.md) | Next: [07-per-client-rate-limit-middleware.md](07-per-client-rate-limit-middleware.md)
