# Exercise 9: CORS preflight and baseline security headers

A browser SPA calling your API cross-origin triggers CORS preflight, and every API
should send a baseline of security headers. This module builds a `CORS` middleware
that validates the `Origin` against an allowlist and short-circuits `OPTIONS`
preflight with 204, plus a `SecurityHeaders` middleware setting the standard set.

Fully self-contained: its own `go mod init`, demo, and tests. Nothing here imports
another exercise.

## What you'll build

```text
corsmw/                      independent module: example.com/corsmw
  go.mod                     go 1.26
  middleware.go              CORS(allowlist) + SecurityHeaders(csp)
  cmd/demo/main.go           runnable demo: preflight 204, normal GET passthrough
  middleware_test.go         preflight short-circuit, disallowed origin, GET passthrough, headers
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `CORS(allowed []string)` validating `Origin`, setting `Access-Control-Allow-*` and `Vary: Origin`, short-circuiting `OPTIONS` with 204 (never calling `next`); `SecurityHeaders(csp string)` setting `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, and CSP.
- Test: an `OPTIONS` with an allowed origin returns 204 with correct `Access-Control-Allow-*` and `Vary: Origin`, and `next` is not called; a disallowed origin echoes no `Allow-Origin`; a normal `GET` passes through with security headers present.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/corsmw/cmd/demo
cd ~/go-exercises/corsmw
go mod init example.com/corsmw
go mod edit -go=1.26
```

### Preflight, the allowlist, and header-before-body ordering

A browser making a "non-simple" cross-origin request (custom headers, `PUT`,
`DELETE`) first sends an `OPTIONS` *preflight* asking whether the real request is
allowed. The server must answer it *without* running the real handler: set the
`Access-Control-Allow-*` headers and return `204 No Content`. That is a
short-circuit — the preflight middleware writes 204 and returns, never calling
`next`. Handling `OPTIONS` as a normal request (falling through to the handler)
either 404s the preflight or runs business logic for a probe that carries no body,
both wrong.

Origin validation is an *allowlist*, never a reflect-everything. Echoing back
whatever `Origin` the client sent (`Access-Control-Allow-Origin: <origin>`
unconditionally) effectively disables the same-origin policy and is a real
vulnerability. The middleware checks the request's `Origin` against a configured
set and only sets `Allow-Origin` when it matches. Because the response now varies
by the request's `Origin`, you must add `Vary: Origin` so caches do not serve one
origin's allowed response to another — `Header().Add` (not `Set`) appends to any
existing `Vary`.

Both CORS and `SecurityHeaders` write *response headers*, which must be set before
any body is written and before the status is committed — the write-once contract
again. `SecurityHeaders` sets the low-controversy baseline: `X-Content-Type-Options:
nosniff` (stop MIME sniffing), `X-Frame-Options: DENY` (block clickjacking via
framing), `Referrer-Policy: no-referrer` (do not leak URLs), and a caller-supplied
`Content-Security-Policy`.

Create `middleware.go`:

```go
package corsmw

import (
	"net/http"
	"slices"
)

type Handler = http.Handler

type Middleware func(Handler) Handler

// CORS validates Origin against allowed and answers OPTIONS preflight with 204.
func CORS(allowed []string) Middleware {
	allowSet := slices.Clone(allowed)
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && slices.Contains(allowSet, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
				// Response depends on the request Origin, so caches must vary on it.
				w.Header().Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent) // preflight: short-circuit
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets a baseline of response security headers.
func SecurityHeaders(csp string) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", csp)
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

	"example.com/corsmw"
)

func main() {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "api response")
	})
	handler := corsmw.CORS([]string{"https://app.example.com"})(final)

	// Preflight from an allowed origin.
	pre := httptest.NewRecorder()
	preReq := httptest.NewRequest(http.MethodOptions, "/", nil)
	preReq.Header.Set("Origin", "https://app.example.com")
	handler.ServeHTTP(pre, preReq)
	fmt.Printf("preflight: %d allow-origin=%s\n",
		pre.Code, pre.Header().Get("Access-Control-Allow-Origin"))

	// Normal GET from the allowed origin.
	get := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/", nil)
	getReq.Header.Set("Origin", "https://app.example.com")
	handler.ServeHTTP(get, getReq)
	fmt.Printf("get: %d body=%q\n", get.Code, get.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
preflight: 204 allow-origin=https://app.example.com
get: 200 body="api response"
```

### Tests

`TestPreflightShortCircuits` proves an allowed-origin `OPTIONS` returns 204 with
the `Allow-*` headers and `Vary: Origin`, and that `next` never runs.
`TestDisallowedOrigin` proves a non-allowlisted origin gets no `Allow-Origin`.
`TestNormalRequestPassesThrough` proves a `GET` reaches `next`.
`TestSecurityHeadersSet` proves the baseline headers, including `nosniff` and the
configured CSP.

Create `middleware_test.go`:

```go
package corsmw

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const allowedOrigin = "https://app.example.com"

func TestPreflightShortCircuits(t *testing.T) {
	t.Parallel()

	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", allowedOrigin)
	CORS([]string{allowedOrigin})(final).ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", rec.Code)
	}
	if called {
		t.Fatal("preflight ran next; it must short-circuit")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
		t.Fatalf("allow-origin = %q, want %q", got, allowedOrigin)
	}
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("Vary = %q, want it to contain Origin", got)
	}
}

func TestDisallowedOrigin(t *testing.T) {
	t.Parallel()

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	CORS([]string{allowedOrigin})(final).ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("allow-origin = %q, want empty for a disallowed origin", got)
	}
}

func TestNormalRequestPassesThrough(t *testing.T) {
	t.Parallel()

	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", allowedOrigin)
	CORS([]string{allowedOrigin})(final).ServeHTTP(rec, req)

	if !called {
		t.Fatal("normal GET did not reach next")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
}

func TestSecurityHeadersSet(t *testing.T) {
	t.Parallel()

	const csp = "default-src 'self'"
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	SecurityHeaders(csp)(final).ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != csp {
		t.Errorf("CSP = %q, want %q", got, csp)
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
}
```

## Review

CORS is correct when it validates against an allowlist (never reflects an arbitrary
origin), sets `Vary: Origin` so caches stay honest, and short-circuits `OPTIONS`
with 204 before the handler runs — `TestPreflightShortCircuits` pins all three,
including the `called` flag that proves the preflight never reached `next`.
`TestDisallowedOrigin` guards the security boundary: a non-allowlisted origin must
receive *no* `Allow-Origin`, or you have effectively disabled the same-origin
policy. `SecurityHeaders` writes its headers before the body because headers are
immutable once the response starts. Both are header-only middlewares — they add
context and return control, so the happy path always calls `next` exactly once
(except the preflight, which is a deliberate short-circuit).

## Resources

- [MDN: CORS](https://developer.mozilla.org/en-US/docs/Web/HTTP/CORS) — preflight, the `Access-Control-*` headers, and `Vary: Origin`.
- [net/http#Header.Add](https://pkg.go.dev/net/http#Header.Add) — appending `Vary: Origin` without clobbering existing values.
- [slices#Contains](https://pkg.go.dev/slices#Contains) — the allowlist membership check.
- [OWASP Secure Headers Project](https://owasp.org/www-project-secure-headers/) — the baseline security headers and their rationale.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-request-timeout-middleware.md](08-request-timeout-middleware.md) | Next: [10-conditional-path-scoped-middleware.md](10-conditional-path-scoped-middleware.md)
