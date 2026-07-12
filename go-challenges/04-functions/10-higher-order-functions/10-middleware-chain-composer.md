# Exercise 10: Compose an HTTP-Style Middleware Chain

**Nivel: Intermedio** — validacion rapida (un test corto).

Every Go router worth using — chi, negroni, gorilla/mux's `Use` — is built on
the same higher-order idea: a middleware is a function that takes a handler
and returns a new handler. `Chain` folds a list of middlewares into one, and
the order they run in is part of the contract, not an implementation detail.
This module builds `Chain` without touching `net/http`, so the exercise runs
in milliseconds and the composition logic stays front and center.

## What you'll build

```text
chain/                     independent module: example.com/middleware-chain
  go.mod                   go 1.24
  chain.go                 type Handler, Middleware; func Chain; Prefixer; Suffixer
  chain_test.go            order-recording test + prefix/suffix wiring test + empty-chain test
```

- Files: `chain.go`, `chain_test.go`.
- Implement: `Chain(mws ...Middleware) Middleware` so `Chain(a, b, c)(h)` behaves like `a(b(c(h)))` — the first middleware is outermost, matching how `router.Use(a, b, c)` behaves in real frameworks.
- Test: an order-recording middleware proves the request/response asymmetry; a prefix/suffix pair proves the wiring; an empty chain proves the identity case.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/10-middleware-chain-composer
cd go-solutions/04-functions/10-higher-order-functions/10-middleware-chain-composer
go mod edit -go=1.24
```

### Functions returning functions taking functions

`Middleware` is `func(Handler) Handler` — a function whose input and output
are both functions. `Chain` is one level higher still: it takes a slice of
those and returns one. There is no loop over requests here, only a loop over
*wrapping*, executed once at setup time; the request path itself is nested
function calls, with zero allocation beyond the closures `Chain` builds.

To get `a(b(c(h)))` without deep nesting in the caller's code, `Chain` loops
backward over `mws`, wrapping the accumulator one layer at a time: `c` wraps
`h` first, then `b` wraps that, then `a` wraps that. `a`'s closure is built
last, so it is the one returned — making it the entry point, i.e. outermost.

Create `chain.go`:

```go
package chain

// Handler processes a request string and returns a response string.
type Handler func(req string) string

// Middleware wraps a Handler with additional behavior, producing a new Handler.
type Middleware func(Handler) Handler

// Chain composes middlewares so that Chain(a, b, c)(h) behaves like
// a(b(c(h))): the first middleware in the list is the outermost, matching
// the order a router registers them in. Built by looping backwards over
// mws and wrapping h one layer at a time, so the last middleware wraps h
// first and the first middleware wraps everything else last.
func Chain(mws ...Middleware) Middleware {
	return func(h Handler) Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

// Prefixer returns a Middleware that prepends tag to the request before
// calling next. This is request-phase work, so with Chain(a, b) the
// outermost middleware (a) prepends its tag last, ending up leftmost.
func Prefixer(tag string) Middleware {
	return func(next Handler) Handler {
		return func(req string) string {
			return next(tag + " " + req)
		}
	}
}

// Suffixer returns a Middleware that appends tag to the response after
// next returns. This is response-phase work, so with Chain(a, b) the
// innermost middleware (b) appends its tag first, ending up leftmost in
// the response.
func Suffixer(tag string) Middleware {
	return func(next Handler) Handler {
		return func(req string) string {
			return next(req) + " " + tag
		}
	}
}
```

### The ordering asymmetry

Middleware composition is an onion, and the request phase and response phase
peel it in opposite directions. On the way in, the outermost middleware's
code runs first — that is what "outermost" means. On the way out, it is the
last to return, so its post-`next` code runs *last*. The innermost middleware
is the mirror image: last in on the way down, first out on the way up.

`Prefixer` and `Suffixer` show this with a twist worth noticing: because
`Prefixer` mutates the request *before* recursing, each layer prepends onto
whatever the previous layer already produced. With `Chain(Prefixer("A"),
Prefixer("B"))`, `B` is innermost — it wraps the handler directly — so `B`'s
tag ends up glued to the request and `A`'s lands in front of that, giving
`"B A req"`, not `"A B req"`. `Suffixer` mirrors this on the way out: `B`
appends first (closest to the handler), `A` appends last, giving
`"req B A"`. The middleware closest to the handler is always closest to the
request/response in the resulting string — composition order and string
order are related but not identical, which is exactly why it pays to check
with a test instead of eyeballing it.

## Tests

`TestChainOrdering` is the real proof of the asymmetry: a recording
middleware logs a `"req"` entry before calling `next` and a `"resp"` entry
after `next` returns. With `Chain(A, B)` the log comes out
`[A:req, B:req, B:resp, A:resp]` — outermost-first going in, innermost-first
coming out. `TestChainPrefixSuffix` locks down the wiring with the exact
strings derived above, and `TestChainEmpty` checks that composing zero
middlewares returns the handler unchanged.

Create `chain_test.go`:

```go
package chain

import (
	"reflect"
	"testing"
)

// TestChainOrdering is the real proof of the request/response asymmetry: an
// order-recording middleware logs a "req" entry before calling next and a
// "resp" entry after next returns. With Chain(A, B), A is outermost.
func TestChainOrdering(t *testing.T) {
	t.Parallel()

	var log []string
	recorder := func(name string) Middleware {
		return func(next Handler) Handler {
			return func(req string) string {
				log = append(log, name+":req")
				resp := next(req)
				log = append(log, name+":resp")
				return resp
			}
		}
	}
	echo := func(req string) string { return req }

	Chain(recorder("A"), recorder("B"))(echo)("hello")

	want := []string{"A:req", "B:req", "B:resp", "A:resp"}
	if !reflect.DeepEqual(log, want) {
		t.Fatalf("execution order = %v, want %v", log, want)
	}
}

// TestChainPrefixSuffix proves Chain wires middlewares in the declared order
// rather than some other arrangement. Prefixer mutates the request before
// calling next, so each layer prepends to whatever the previous layer already
// produced: B is the innermost middleware (it wraps echo directly), so its
// tag ends up glued to the request, and A's tag ends up in front of that.
// Suffixer mirrors this on the way out: B appends to echo's result first
// (innermost runs closest to the handler), then A appends last.
func TestChainPrefixSuffix(t *testing.T) {
	t.Parallel()

	echo := func(req string) string { return req }

	if got, want := Chain(Prefixer("A"), Prefixer("B"))(echo)("req"), "B A req"; got != want {
		t.Errorf("prefix chain = %q, want %q", got, want)
	}
	if got, want := Chain(Suffixer("A"), Suffixer("B"))(echo)("req"), "req B A"; got != want {
		t.Errorf("suffix chain = %q, want %q", got, want)
	}
}

// TestChainEmpty documents the base case: composing zero middlewares must
// return the handler unchanged.
func TestChainEmpty(t *testing.T) {
	t.Parallel()

	echo := func(req string) string { return req }
	got := Chain()(echo)("req")
	if want := "req"; got != want {
		t.Errorf("Chain()(echo)(%q) = %q, want %q", "req", got, want)
	}
}
```

## Review

`Chain` is correct when the entry point it returns is the first middleware's
closure — that single placement decision in the backward loop is what makes
it outermost, and everything else follows from it. The order-recording test
is the one that actually matters: it pins the onion behavior every router
built this way relies on, independent of what any particular middleware does
with the request or response. The prefix/suffix test is a secondary, more
concrete check, and its exact strings are a good reminder that intuition
about nested closures is easy to get backward — worth verifying, not assuming.

## Resources

- [Go Wiki: Learn Server Programming](https://go.dev/doc/tutorial/web-service-gin) — handler and middleware shapes in idiomatic Go HTTP servers.
- [chi router: middleware stacking](https://github.com/go-chi/chi#middleware-handlers) — a real-world `Chain`-equivalent (`chi.Chain`) with the same outermost-first semantics.
- [Effective Go: Closures](https://go.dev/doc/effective_go#closures) — why `Middleware` closures capture `tag` and `next` correctly across the loop.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-backoff-with-jitter.md](09-backoff-with-jitter.md) | Next: [11-memoize-lookup-cache.md](11-memoize-lookup-cache.md)
