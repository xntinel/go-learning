# Exercise 10: Applying a middleware only to matching paths

Some middlewares must not run everywhere: liveness and readiness probes
(`/healthz`, `/metrics`) must never require a token or count against a rate limit.
This module builds two higher-order combinators — `Skip` and `OnlyFor` — that wrap
an existing middleware to apply it conditionally, keeping the chain declarative.

Fully self-contained: its own `go mod init`, demo, and tests. Nothing here imports
another exercise.

## What you'll build

```text
condmw/                      independent module: example.com/condmw
  go.mod                     go 1.26
  middleware.go              Chain + RequireToken + Skip/OnlyFor combinators + PathPrefix
  cmd/demo/main.go           runnable demo: /healthz open, /api guarded
  middleware_test.go         skip on match, apply on miss, OnlyFor inverse, usable in a chain
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Skip(mw, pred)` returning a `Middleware` that runs `mw` only when `pred(r)` is false, `OnlyFor(mw, pred)` its inverse, and a `PathPrefix(prefixes...)` predicate helper.
- Test: `RequireToken` wrapped in `Skip(pred = path has prefix /healthz)` lets an unauthenticated `/healthz` through (200) but still rejects `/api` (401); the predicate is evaluated per request; `Skip` returns a value usable inside `NewChain`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/14-interface-based-middleware-chain/10-conditional-path-scoped-middleware/cmd/demo
cd go-solutions/08-interfaces/14-interface-based-middleware-chain/10-conditional-path-scoped-middleware
go mod edit -go=1.26
```

### A middleware that decides per request whether to apply another

`Skip` and `OnlyFor` are combinators: they take a `Middleware` and a predicate and
return a *new* `Middleware`. Because a middleware is just an interface-shaped value
(`func(Handler) Handler`), you can compose them like any other value. The trick is
that the conditional decision must happen *per request*, not once at wire time —
the same route table serves both `/healthz` and `/api`, so `Skip` cannot decide
statically whether to include the inner middleware. Instead, `Skip(mw, pred)`
builds *both* the wrapped handler (`mw(next)`) and the bare handler (`next`) up
front, then at request time inspects `pred(r)` and dispatches to one or the other.
That keeps the decision cheap (no re-wrapping per request) while remaining dynamic.

This is how you keep probes unauthenticated without duplicating the whole chain.
You wrap the auth middleware once — `Skip(RequireToken(validate),
PathPrefix("/healthz", "/metrics"))` — and drop it in the chain like any other
layer; requests to the probe paths bypass auth while everything else is guarded.
`OnlyFor` is the mirror: apply the middleware *only* when the predicate matches
(useful for "gzip only under `/downloads`"). Both keep the chain declarative — the
conditional logic lives in the combinator, not scattered as `if` statements inside
each handler.

Create `middleware.go`:

```go
package condmw

import (
	"net/http"
	"strings"
)

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

// Skip applies mw only when pred(r) is false. Requests matching pred bypass mw
// and go straight to next.
func Skip(mw Middleware, pred func(*http.Request) bool) Middleware {
	return func(next Handler) Handler {
		wrapped := mw(next) // build once; dispatch per request
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if pred(r) {
				next.ServeHTTP(w, r) // skip mw
				return
			}
			wrapped.ServeHTTP(w, r) // apply mw
		})
	}
}

// OnlyFor applies mw only when pred(r) is true; otherwise next runs bare.
func OnlyFor(mw Middleware, pred func(*http.Request) bool) Middleware {
	return Skip(mw, func(r *http.Request) bool { return !pred(r) })
}

// PathPrefix builds a predicate matching any request whose path has one of the
// given prefixes.
func PathPrefix(prefixes ...string) func(*http.Request) bool {
	return func(r *http.Request) bool {
		for _, p := range prefixes {
			if strings.HasPrefix(r.URL.Path, p) {
				return true
			}
		}
		return false
	}
}

// RequireToken is a sample guarded middleware to demonstrate conditional scoping.
func RequireToken(validate func(string) bool) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := r.Header.Get("Authorization")
			if tok == "" || !validate(tok) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

### The runnable demo

The demo guards a handler with auth but skips it for `/healthz`, then sends one
unauthenticated request to each path to show the probe is open and the API is
closed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/condmw"
)

func main() {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	})
	auth := condmw.RequireToken(func(tok string) bool { return tok == "Bearer good" })
	chain := condmw.NewChain(condmw.Skip(auth, condmw.PathPrefix("/healthz")))
	handler := chain.Then(final)

	probe := func(path string) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}

	fmt.Printf("/healthz (no token): %d\n", probe("/healthz"))
	fmt.Printf("/api (no token):     %d\n", probe("/api"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/healthz (no token): 200
/api (no token):     401
```

### Tests

`TestSkipBypassesOnMatch` proves an unauthenticated `/healthz` returns 200.
`TestSkipAppliesOnMiss` proves an unauthenticated `/api` still returns 401.
`TestOnlyForInverse` proves `OnlyFor` applies the middleware only on a match.
`TestSkipUsableInChain` proves the combinator returns a valid `Middleware` that
composes inside `NewChain`.

Create `middleware_test.go`:

```go
package condmw

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func validate(tok string) bool { return tok == "Bearer good" }

func okHandler() Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func do(h http.Handler, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

func TestSkipBypassesOnMatch(t *testing.T) {
	t.Parallel()

	h := Skip(RequireToken(validate), PathPrefix("/healthz"))(okHandler())
	if got := do(h, "/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz (no token) = %d, want 200 (auth skipped)", got)
	}
}

func TestSkipAppliesOnMiss(t *testing.T) {
	t.Parallel()

	h := Skip(RequireToken(validate), PathPrefix("/healthz"))(okHandler())
	if got := do(h, "/api"); got != http.StatusUnauthorized {
		t.Fatalf("/api (no token) = %d, want 401 (auth applied)", got)
	}
}

func TestOnlyForInverse(t *testing.T) {
	t.Parallel()

	// Apply auth ONLY under /api; /healthz runs bare.
	h := OnlyFor(RequireToken(validate), PathPrefix("/api"))(okHandler())
	if got := do(h, "/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200 (auth not applied here)", got)
	}
	if got := do(h, "/api"); got != http.StatusUnauthorized {
		t.Fatalf("/api (no token) = %d, want 401 (auth applied here)", got)
	}
}

func TestSkipUsableInChain(t *testing.T) {
	t.Parallel()

	chain := NewChain(Skip(RequireToken(validate), PathPrefix("/healthz")))
	h := chain.Then(okHandler())

	if got := do(h, "/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz via chain = %d, want 200", got)
	}
	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api with token via chain = %d, want 200", rec.Code)
	}
}
```

## Review

The combinator is correct when the predicate is evaluated *per request* and the
right handler runs each time: `TestSkipBypassesOnMatch` and `TestSkipAppliesOnMiss`
share one wrapped handler and get opposite results purely from the path, proving
the decision is dynamic, not wired once. Building `wrapped := mw(next)` up front and
dispatching on `pred(r)` at request time keeps the hot path cheap while staying
conditional. `OnlyFor` is just `Skip` with a negated predicate, which is why it
needs no separate implementation. The value of these combinators is declarative
scope: probe endpoints stay unauthenticated by wrapping the auth middleware once,
rather than smearing path checks through every handler.

## Resources

- [net/http#Handler](https://pkg.go.dev/net/http#Handler) — the interface value the combinators compose.
- [strings#HasPrefix](https://pkg.go.dev/strings#HasPrefix) — the path match behind `PathPrefix`.
- [net/http#Request.URL](https://pkg.go.dev/net/http#Request) — `r.URL.Path`, the field the predicate inspects.
- [justinas/alice](https://github.com/justinas/alice) — chaining reference; conditional application is a common extension of it.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-cors-and-security-headers-middleware.md](09-cors-and-security-headers-middleware.md) | Next: [../../09-pointers/01-pointer-basics/00-concepts.md](../../09-pointers/01-pointer-basics/00-concepts.md)
