# Exercise 29: Request Router with Priority Chain and Fallback

**Nivel: Intermedio** — validacion rapida (un test corto).

A canary route, an internal-preview route, the stable production
route — real routing is rarely one flat table, it is an ordered
preference: try the more specific or higher-priority router first, and
only fall through to the next one if it declines. `Chain` builds a
single `Handler` out of an ordered list of `Router` values plus a
fallback, the same first-match-wins shape as Exercise 25's flag chain,
applied to dispatch instead of variant selection.

## What you'll build

```text
routerchain/                 independent module: example.com/routerchain
  go.mod                     go 1.24
  routerchain.go               type Request, Handler, Router; func Chain
  routerchain_test.go          priority order, fallthrough, fallback, empty chain
  cmd/demo/
    main.go                  dispatches three paths through a two-router chain
```

- Files: `routerchain.go`, `routerchain_test.go`, `cmd/demo/main.go`.
- Implement: `Request struct{ Path string }`, `Handler func(req Request) string`, `Router func(req Request) (Handler, bool)`, and `Chain(fallback Handler, routers ...Router) Handler`.
- Test: the first router that claims a request wins and lower-priority routers are never consulted; a declining router falls through to the next one; the fallback handles requests no router claims; a chain with no routers always uses the fallback.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Claiming a request is two decisions, not one

`Router` separates "can I handle this?" from "here is how" — it returns a
`Handler` only once it has decided to claim the request, rather than
handling it inline. That separation is what makes priority ordering
meaningful: `Chain` can ask each router, cheaply, whether it wants this
request without that router doing any real work, and stop asking the
moment one says yes. A router that instead always ran its handler and
returned some default response for non-matches would make "does this
apply" and "what is the result" indistinguishable, and a chain built from
such routers could never safely fall through to a lower-priority one —
every router would look like a match. The fallback is a plain `Handler`,
not a `Router`, because by construction it always applies: there is
nothing left to decline to once every router in the chain has said no.

Create `routerchain.go`:

```go
package routerchain

// Request is the minimal input a Router inspects to decide whether it
// can handle the request.
type Request struct {
	Path string
}

// Handler produces the response for a request a Router has claimed.
type Handler func(req Request) string

// Router inspects req and either claims it (returning a Handler and
// true) or declines (returning nil, false), leaving room for a
// lower-priority router to try.
type Router func(req Request) (Handler, bool)

// Chain tries routers in order — highest priority first — and returns
// the first Handler claimed. If no router claims the request, fallback
// handles it instead.
func Chain(fallback Handler, routers ...Router) Handler {
	return func(req Request) string {
		for _, route := range routers {
			if handler, ok := route(req); ok {
				return handler(req)
			}
		}
		return fallback(req)
	}
}
```

### The runnable demo

The demo chains a canary router ahead of the stable API router, so
`/canary/...` paths are claimed first even though both routers could, in
principle, be asked about any path.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/routerchain"
)

func main() {
	canaryRouter := routerchain.Router(func(req routerchain.Request) (routerchain.Handler, bool) {
		if strings.HasPrefix(req.Path, "/canary/") {
			return func(req routerchain.Request) string {
				return "canary-service: " + req.Path
			}, true
		}
		return nil, false
	})

	apiRouter := routerchain.Router(func(req routerchain.Request) (routerchain.Handler, bool) {
		if strings.HasPrefix(req.Path, "/api/") {
			return func(req routerchain.Request) string {
				return "stable-api: " + req.Path
			}, true
		}
		return nil, false
	})

	notFound := routerchain.Handler(func(req routerchain.Request) string {
		return "404: " + req.Path
	})

	dispatch := routerchain.Chain(notFound, canaryRouter, apiRouter)

	paths := []string{"/canary/checkout", "/api/users", "/health"}
	for _, p := range paths {
		fmt.Println(dispatch(routerchain.Request{Path: p}))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
canary-service: /canary/checkout
stable-api: /api/users
404: /health
```

`/canary/checkout` is claimed by the canary router before the API router
ever sees it. `/api/users` falls through the canary router (it declines)
to the API router. `/health` matches neither and reaches the fallback.

### Tests

`TestChainUsesHighestPriorityMatchingRouter` proves the ordering
guarantee that gives this exercise its name: when the first router
claims the request, the second is never even consulted.
`TestChainFallsBackToLowerPriorityRouter` and
`TestChainUsesFallbackWhenNoRouterMatches` cover the two ways a router
can decline — passing to the next router in the chain, or exhausting the
whole chain down to the fallback. `TestChainWithNoRoutersAlwaysUsesFallback`
pins down the degenerate case of a chain built with zero routers.

Create `routerchain_test.go`:

```go
package routerchain

import "testing"

func alwaysDeclines(req Request) (Handler, bool) { return nil, false }

func TestChainUsesHighestPriorityMatchingRouter(t *testing.T) {
	t.Parallel()

	secondCalled := false
	first := func(req Request) (Handler, bool) {
		return func(req Request) string { return "from-first" }, true
	}
	second := func(req Request) (Handler, bool) {
		secondCalled = true
		return func(req Request) string { return "from-second" }, true
	}

	dispatch := Chain(func(req Request) string { return "fallback" }, first, second)
	if got := dispatch(Request{Path: "/x"}); got != "from-first" {
		t.Fatalf("dispatch() = %q, want %q", got, "from-first")
	}
	if secondCalled {
		t.Fatal("second router was consulted even though the first claimed the request")
	}
}

func TestChainFallsBackToLowerPriorityRouter(t *testing.T) {
	t.Parallel()

	second := func(req Request) (Handler, bool) {
		return func(req Request) string { return "from-second" }, true
	}

	dispatch := Chain(func(req Request) string { return "fallback" }, alwaysDeclines, second)
	if got := dispatch(Request{Path: "/x"}); got != "from-second" {
		t.Fatalf("dispatch() = %q, want %q", got, "from-second")
	}
}

func TestChainUsesFallbackWhenNoRouterMatches(t *testing.T) {
	t.Parallel()

	dispatch := Chain(func(req Request) string { return "fallback: " + req.Path }, alwaysDeclines, alwaysDeclines)
	if got := dispatch(Request{Path: "/unknown"}); got != "fallback: /unknown" {
		t.Fatalf("dispatch() = %q, want %q", got, "fallback: /unknown")
	}
}

func TestChainWithNoRoutersAlwaysUsesFallback(t *testing.T) {
	t.Parallel()

	dispatch := Chain(func(req Request) string { return "fallback" })
	if got := dispatch(Request{Path: "/anything"}); got != "fallback" {
		t.Fatalf("dispatch() = %q, want %q", got, "fallback")
	}
}
```

## Review

`Chain` is correct because a router's `(Handler, bool)` return keeps
"deciding to handle" and "actually handling" as two separate steps — the
chain only ever calls the claimed `Handler` once it has stopped looking
for a better match, and it never runs a router's side effects speculatively
just to see whether it would have matched. The priority order is entirely
the caller's responsibility, encoded by the order of the `routers...`
arguments; `Chain` itself has no notion of "more specific wins," which
means two routers that both match the same path will silently prefer
whichever one is listed first — worth a comment at the call site, since
nothing in the types enforces that the given order is the intended one.

## Resources

- [Go spec: Function types](https://go.dev/ref/spec#Function_types) — the `Router`/`Handler` strategy-then-decorator shape.
- [net/http.ServeMux](https://pkg.go.dev/net/http#ServeMux) — a standard-library router with its own most-specific-pattern-wins priority rule.
- [Envoy: Route Matching](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/http/http_routing) — a production proxy with the same ordered-list-of-routes-plus-default model.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [28-capacity-limiter-with-backpressure.md](28-capacity-limiter-with-backpressure.md) | Next: [30-consensus-aggregate-with-quorum.md](30-consensus-aggregate-with-quorum.md)
