# Exercise 2: Compose a Middleware Stack with a Higher-Order Chain

One middleware is a closure factory; a *stack* of them is function composition.
This module builds `Chain(mws ...Middleware) Middleware`, a higher-order function
that folds a list of middleware values into a single middleware. Getting the
composition order right is the whole point: the first-listed middleware must be
the outermost layer of the onion, running first on the way in and last on the way
out. You prove the order with a trace slice and prove short-circuiting with an
auth layer that never calls `next`.

This module is fully self-contained.

## What you'll build

```text
chainmw/                   independent module: example.com/chainmw
  go.mod                   go 1.26
  chain.go                 Middleware, Chain, RequireAuth, Tag
  cmd/
    demo/
      main.go              composes a two-layer chain and prints the onion order
  chain_test.go            order, empty-is-identity, short-circuit auth
```

- Files: `chain.go`, `cmd/demo/main.go`, `chain_test.go`.
- Implement: `Chain(mws ...Middleware) Middleware` applying the first listed outermost; a `Tag(label, *[]string)` observability middleware; a `RequireAuth(token)` middleware that writes 401 and does not call `next` when the bearer token is wrong.
- Test: a chain of tracing middlewares records exact onion order `A-before, B-before, handler, B-after, A-after`; `Chain()` with no args is the identity; a failing auth layer stops the chain so downstream markers never appear.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/chainmw/cmd/demo
cd ~/go-exercises/chainmw
go mod init example.com/chainmw
```

### Folding functions into one function

`Chain` returns a `Middleware`, so its result plugs in anywhere a single
middleware does. Internally it wraps the handler from the *last* middleware
inward to the first:

```
for i := len(mws) - 1; i >= 0; i-- {
	next = mws[i](next)
}
```

Walk it for `Chain(a, b, c)` applied to `h`. Start with `next = h`. `i=2`:
`next = c(h)`. `i=1`: `next = b(c(h))`. `i=0`: `next = a(b(c(h)))`. So `a` is the
outermost wrapper. At request time, `a` runs its pre-work, calls into `b`, which
runs its pre-work, calls into `c`, which calls `h`; then the post-work unwinds
`c`, `b`, `a`. First-listed is outermost — the intuitive reading, and the one
that lets you write `Chain(RequireAuth(tok), Metrics(...), handler)` and have auth
gate everything below it.

The empty case falls out for free: `Chain()` runs the loop zero times and returns
`next` unchanged, so it is the identity middleware. That matters because it lets
callers build a chain from a variable-length slice without special-casing "no
middlewares".

`Tag` is a tiny observability middleware that appends `label+"-before"` before
calling `next` and `label+"-after"` after. Because it captures a `*[]string`, the
same trace pointer threads through every layer, recording the exact execution
order — the mechanism the ordering test relies on. `RequireAuth` is the
short-circuit case: if the `Authorization` header does not match `Bearer <token>`
it writes 401 and *returns without calling `next`*, so nothing downstream runs.
That early return is how real auth, rate-limit, and CORS-preflight middlewares
abort a request.

Create `chain.go`:

```go
package chainmw

import "net/http"

// Middleware wraps an http.Handler, returning a new handler.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares into one. The first middleware listed runs
// outermost: for Chain(a, b, c) the request flows a -> b -> c -> handler and
// the response unwinds c -> b -> a. Chain() with no arguments is the identity.
func Chain(mws ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			next = mws[i](next)
		}
		return next
	}
}

// Tag records label+"-before" before calling next and label+"-after" after.
// It makes composition order observable through the shared trace slice.
func Tag(label string, trace *[]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*trace = append(*trace, label+"-before")
			next.ServeHTTP(w, r)
			*trace = append(*trace, label+"-after")
		})
	}
}

// RequireAuth short-circuits with 401 when the bearer token does not match. On
// rejection it does not call next, so nothing downstream runs.
func RequireAuth(token string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

### The runnable demo

The demo composes a `logging` layer outside a `metrics` layer around a handler,
serves one request, and prints the trace. The output is the onion: outer-before,
inner-before, handler, inner-after, outer-after.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/chainmw"
)

func main() {
	var trace []string
	stack := chainmw.Chain(
		chainmw.Tag("logging", &trace),
		chainmw.Tag("metrics", &trace),
	)
	handler := stack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, "handler")
		fmt.Fprintln(w, "ok")
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	for _, e := range trace {
		fmt.Println(e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
logging-before
metrics-before
handler
metrics-after
logging-after
```

### Tests

Create `chain_test.go`:

```go
package chainmw

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func serve(h http.Handler, setup func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if setup != nil {
		setup(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChainOnionOrder(t *testing.T) {
	t.Parallel()
	var trace []string
	stack := Chain(Tag("A", &trace), Tag("B", &trace))
	h := stack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, "handler")
	}))

	serve(h, nil)

	want := []string{"A-before", "B-before", "handler", "B-after", "A-after"}
	if !slices.Equal(trace, want) {
		t.Fatalf("order = %v, want %v", trace, want)
	}
}

func TestChainEmptyIsIdentity(t *testing.T) {
	t.Parallel()
	called := false
	h := Chain()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))

	serve(h, nil)

	if !called {
		t.Fatal("empty chain did not call the handler; it must be the identity")
	}
}

func TestChainShortCircuitsOnAuthFailure(t *testing.T) {
	t.Parallel()
	var trace []string
	stack := Chain(
		Tag("outer", &trace),
		RequireAuth("s3cret"),
		Tag("inner", &trace),
	)
	h := stack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, "handler")
	}))

	rec := serve(h, nil) // no Authorization header

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	want := []string{"outer-before", "outer-after"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v (inner and handler must not run)", trace, want)
	}
}

func TestChainAllowsAuthorizedRequest(t *testing.T) {
	t.Parallel()
	var trace []string
	stack := Chain(RequireAuth("s3cret"), Tag("inner", &trace))
	h := stack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace = append(trace, "handler")
	}))

	rec := serve(h, func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer s3cret")
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	want := []string{"inner-before", "handler", "inner-after"}
	if !slices.Equal(trace, want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
}
```

## Review

The chain is correct when `Chain(a, b, c)` nests as `a(b(c(h)))` — first-listed
outermost — so the ordering test sees `A-before, B-before, handler, B-after,
A-after`. The classic defect is folding the slice the wrong way (looping forward
instead of backward), which reverses the onion and, in a real stack, runs auth
inside the handler it was meant to gate. The empty-chain-is-identity property is
what lets callers pass a dynamically built `[]Middleware` without a special case,
and the short-circuit test proves an aborting middleware truly stops everything
below it by asserting the inner and handler markers never appear. Run
`go test -race`.

## Resources

- [pkg.go.dev: net/http Handler](https://pkg.go.dev/net/http#Handler) — the interface every middleware wraps.
- [pkg.go.dev: slices.Equal](https://pkg.go.dev/slices#Equal) — comparing the recorded trace to the expected order.
- [Go Blog: Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — per-iteration loop variables and closure capture.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-metrics-middleware-factory.md](01-metrics-middleware-factory.md) | Next: [03-rate-limiter-token-bucket-closure.md](03-rate-limiter-token-bucket-closure.md)
