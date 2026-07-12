# Exercise 3: Bearer-token auth as a short-circuiting middleware

Authentication is the archetypal short-circuiting middleware: on a missing or
invalid token it must write a 401 and return *without* calling `next`, and on a
valid token it must call `next` exactly once. This module builds `RequireToken`
and pins both halves of that invariant.

Fully self-contained: its own `go mod init`, `Chain`, demo, and tests. Nothing
here imports another exercise.

## What you'll build

```text
authmw/                      independent module: example.com/authmw
  go.mod                     go 1.26
  middleware.go              Chain + RequireToken(validate func(string) bool)
  cmd/demo/main.go           runnable demo: one rejected request, one allowed
  middleware_test.go         reject path (401, next NOT called, WWW-Authenticate) and allow path
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `RequireToken` reading the `Authorization` header, rejecting missing/invalid tokens with `http.Error(w, ..., 401)` and a `WWW-Authenticate` header, and calling `next` exactly once only on success.
- Test: missing header rejects with 401 and the final handler is never called (a bool flag stays false); a valid header returns 200 and the final handler runs; the 401 response carries `WWW-Authenticate`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The short-circuit invariant

The entire contract of an auth middleware is a branch: validate the credential; on
failure, respond and return; on success, delegate to `next`. The failure branch
must *not* fall through to `next` — that is the short-circuit. Forgetting the
`return` after `http.Error` is a genuine security bug: the handler would run
anyway, and because `http.Error` already committed the 401, the handler's own
`WriteHeader` would print `superfluous response.WriteHeader call` and you would
serve protected content behind a 401 status. The success branch must call `next`
*exactly once*; zero calls would hang or empty the response for authenticated
users.

Two correctness details make this production-grade rather than a toy. First,
`http.Error` sets `Content-Type: text/plain`, writes the status, and writes the
message — so you must set any extra headers (like `WWW-Authenticate`) *before*
calling it, because headers cannot be added after the status is committed. The
`WWW-Authenticate: Bearer` header is what a spec-compliant 401 uses to tell the
client which scheme to authenticate with. Second, validation is injected as a
`func(string) bool` so the middleware carries no policy: the caller decides
whether a token is a static secret, a lookup, or a parsed JWT. In real code that
function would do a constant-time comparison or verify a signature; here it is a
seam.

Create `middleware.go`:

```go
package authmw

import "net/http"

type Handler = http.Handler

type Middleware func(Handler) Handler

type Chain struct{ middlewares []Middleware }

func NewChain(mws ...Middleware) *Chain {
	cp := make([]Middleware, len(mws))
	copy(cp, mws)
	return &Chain{middlewares: cp}
}

func (c *Chain) Then(h Handler) Handler {
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		h = c.middlewares[i](h)
	}
	return h
}

// RequireToken rejects requests whose Authorization header is missing or fails
// validate, writing 401 and returning without calling next. On success it calls
// next exactly once.
func RequireToken(validate func(token string) bool) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get("Authorization")
			if token == "" || !validate(token) {
				// Set headers BEFORE http.Error commits the status.
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return // short-circuit: do not call next
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/authmw"
)

func main() {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "secret data")
	})
	handler := authmw.NewChain(
		authmw.RequireToken(func(tok string) bool { return tok == "Bearer s3cret" }),
	).Then(final)

	// Rejected: no Authorization header.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Printf("no token:    %d\n", rec.Code)

	// Allowed: valid bearer token.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	handler.ServeHTTP(rec, req)
	fmt.Printf("valid token: %d %s\n", rec.Code, rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
no token:    401
valid token: 200 secret data
```

### Tests

`TestRejectsMissingHeader` proves the short-circuit: a request with no header gets
401 and the final handler's `called` flag stays false. `TestRejectsInvalidToken`
covers a present-but-wrong token. `TestAllowsValidToken` proves the success path
calls `next` and returns 200. `TestUnauthorizedSetsWWWAuthenticate` pins the
spec-correct header on the reject path.

Create `middleware_test.go`:

```go
package authmw

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func validate(tok string) bool { return tok == "Bearer good" }

func TestRejectsMissingHeader(t *testing.T) {
	t.Parallel()

	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	NewChain(RequireToken(validate)).Then(final).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("final handler ran on a rejected request (short-circuit broken)")
	}
}

func TestRejectsInvalidToken(t *testing.T) {
	t.Parallel()

	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	NewChain(RequireToken(validate)).Then(final).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("final handler ran with an invalid token")
	}
}

func TestAllowsValidToken(t *testing.T) {
	t.Parallel()

	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer good")
	NewChain(RequireToken(validate)).Then(final).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !called {
		t.Fatal("final handler did not run for a valid token")
	}
}

func TestUnauthorizedSetsWWWAuthenticate(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	NewChain(RequireToken(validate)).Then(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, req)

	if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
	}
}
```

## Review

The auth middleware is correct when the two branches are exact: on rejection it
sets `WWW-Authenticate`, writes 401 via `http.Error`, and *returns* so `next`
never runs; on success it calls `next` once and touches nothing else. The
highest-value test is `TestRejectsMissingHeader` asserting the `called` flag
stays false — that is the difference between a real auth gate and a decorative one
that logs a rejection but serves the content anyway. Setting `WWW-Authenticate`
*before* `http.Error` matters because `http.Error` commits the status; a header
added after would be silently dropped. Keep policy out of the middleware: injecting
`validate` lets the same layer guard a static token, a session lookup, or a JWT
verifier without change.

## Resources

- [net/http#Error](https://pkg.go.dev/net/http#Error) — writes the status and plain-text body; note it commits the header.
- [net/http#Request.Header](https://pkg.go.dev/net/http#Header.Get) — reading the `Authorization` header.
- [MDN: WWW-Authenticate](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/WWW-Authenticate) — the header a spec-correct 401 sends.
- [net/http#StatusUnauthorized](https://pkg.go.dev/net/http#StatusUnauthorized) — the 401 constant.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-request-logging-middleware.md](02-request-logging-middleware.md) | Next: [04-panic-recovery-middleware.md](04-panic-recovery-middleware.md)
