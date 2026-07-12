# Exercise 1: The Chain type and the declared-order-is-outermost contract

Every HTTP service wires its cross-cutting layers as a chain of
`func(http.Handler) http.Handler` values around a final handler. This module
builds the reusable `Chain` type that composes them, and pins the one contract
that trips up every engineer at least once: the *first* middleware you declare is
the *outermost* wrapper.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
mwchain/                     independent module: example.com/mwchain
  go.mod                     go 1.26
  middleware.go              type Middleware; type Handler = http.Handler; Chain; NewChain; Then
  cmd/demo/main.go           runnable demo wiring two tag middlewares around a handler
  middleware_test.go         order-contract tests with httptest (empty chain, declared-order proof)
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `type Middleware func(Handler) Handler`, `type Handler = http.Handler`, a `Chain` struct, `NewChain(...Middleware)`, and `Then(Handler) Handler` that folds in reverse so the first declared middleware is outermost.
- Test: an empty chain runs the final handler untouched; a two-middleware chain that appends tags to a shared buffer must produce output in *declaration order*, proving outermost-first execution.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why Then folds in reverse

The chain has to turn a *list* of middlewares into a *nesting*. If you want
`NewChain(a, b, c).Then(final)` to execute `a` first, then `b`, then `c`, then
`final`, then unwind in reverse, the composed handler must be
`a(b(c(final)))` — `a` is the outermost wrapper. To build that from the slice you
apply the middlewares from the *last* index to the first: start with `final`, wrap
it in `c`, wrap that in `b`, wrap that in `a`. That is exactly the reverse fold in
`Then`. Declaring the loop forward (`a` first) would produce `c(b(a(final)))`,
making the last-declared middleware outermost — the opposite of what every reader
expects and the source of the classic "why did recover not catch that panic" bug.

The `type Handler = http.Handler` line is a type *alias*, not a new type. Because
it shares identity with `http.Handler`, any existing `http.Handler` (including an
`http.HandlerFunc` closure) satisfies `Middleware`'s parameter with no conversion.
Write it as `type Handler http.Handler` (no `=`) and you get a distinct type that
`http.Handler` values no longer satisfy, breaking every call site.

Create `middleware.go`:

```go
package mwchain

import "net/http"

// Handler is an alias for http.Handler. The `=` is load-bearing: it makes
// Handler the same type as http.Handler, so any http.Handler satisfies a
// Middleware parameter directly. Dropping the `=` would create a distinct type.
type Handler = http.Handler

// Middleware takes the next handler and returns a handler that wraps it.
type Middleware func(Handler) Handler

// Chain composes an ordered list of middlewares around a final handler.
type Chain struct {
	middlewares []Middleware
}

// NewChain builds a chain. The first middleware becomes the outermost wrapper.
func NewChain(mws ...Middleware) *Chain {
	// Copy so a later mutation of the caller's slice cannot alter the chain.
	cp := make([]Middleware, len(mws))
	copy(cp, mws)
	return &Chain{middlewares: cp}
}

// Then wraps h with every middleware, folding in reverse so the first-declared
// middleware ends up outermost (runs first on the way in, last on the way out).
func (c *Chain) Then(h Handler) Handler {
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		h = c.middlewares[i](h)
	}
	return h
}
```

### The runnable demo

The demo declares two tag middlewares. Each writes its tag to the response before
calling `next` and another tag after `next` returns. Because the first declared
middleware is outermost, its "before" tag comes first and its "after" tag comes
last, bracketing the whole request.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/mwchain"
)

func tag(before, after string) mwchain.Middleware {
	return func(next mwchain.Handler) mwchain.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, before)
			next.ServeHTTP(w, r)
			fmt.Fprint(w, after)
		})
	}
}

func main() {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "[handler]")
	})

	chain := mwchain.NewChain(
		tag("<outer ", " outer>"),
		tag("<inner ", " inner>"),
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	chain.Then(final).ServeHTTP(rec, req)

	fmt.Println(rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
<outer <inner [handler] inner> outer>
```

### Tests

`TestEmptyChainRunsFinalHandler` proves `Then` with no middlewares returns the
final handler untouched. `TestDeclaredOrderIsOutermost` is the contract test: two
middlewares append a tag to a shared `bytes.Buffer` before calling `next`, and the
buffer must read in declaration order (`a` then `b`), which is only true if the
first-declared middleware runs first. `TestBracketOrder` additionally pins the
after-`next` unwinding order, the mirror image of the entry order.

Create `middleware_test.go`:

```go
package mwchain

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func appendTag(buf *bytes.Buffer, tag string) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			buf.WriteString(tag)
			next.ServeHTTP(w, r)
		})
	}
}

func TestEmptyChainRunsFinalHandler(t *testing.T) {
	t.Parallel()

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "ok")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	NewChain().Then(final).ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("code = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", rec.Body.String())
	}
}

func TestDeclaredOrderIsOutermost(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf.WriteString("H")
	})

	chain := NewChain(appendTag(&buf, "a"), appendTag(&buf, "b"))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	chain.Then(final).ServeHTTP(rec, req)

	if got := buf.String(); got != "abH" {
		t.Fatalf("execution order = %q, want %q (first declared runs first)", got, "abH")
	}
}

func TestBracketOrder(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	bracket := func(tag string) Middleware {
		return func(next Handler) Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				buf.WriteString("<" + tag)
				next.ServeHTTP(w, r)
				buf.WriteString(tag + ">")
			})
		}
	}
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf.WriteString("H")
	})

	NewChain(bracket("a"), bracket("b")).Then(final).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := buf.String(); got != "<a<bHb>a>" {
		t.Fatalf("bracket order = %q, want %q", got, "<a<bHb>a>")
	}
}

func Example() {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "core")
	})
	wrap := func(s string) Middleware {
		return func(next Handler) Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, s)
				next.ServeHTTP(w, r)
			})
		}
	}
	rec := httptest.NewRecorder()
	NewChain(wrap("1"), wrap("2")).Then(final).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println(rec.Body.String())
	// Output: 12core
}
```

## Review

The chain is correct when `NewChain(a, b).Then(final)` executes `a` before `b`
before `final` on the way in, and unwinds `b` then `a` on the way out. The proof
is `TestDeclaredOrderIsOutermost`: if the buffer ever reads `"baH"` you folded the
slice forward and made the last-declared middleware outermost, which will later
put your recover boundary inside the handlers it must protect. The alias line is
the other load-bearing detail — change `type Handler = http.Handler` to the
non-alias form and the package stops compiling at the first `http.HandlerFunc`
you pass to a `Middleware`, because a distinct type does not satisfy the
parameter. Copying the variadic slice in `NewChain` is defensive: without it, a
caller who reuses and mutates their slice after building the chain could silently
change the chain's behavior.

## Resources

- [net/http#Handler](https://pkg.go.dev/net/http#Handler) — the one-method interface every middleware returns.
- [net/http#HandlerFunc](https://pkg.go.dev/net/http#HandlerFunc) — the adapter that turns a closure into a Handler.
- [Type declarations (alias vs defined type)](https://go.dev/ref/spec#Type_declarations) — why the `=` in `type Handler = http.Handler` changes identity.
- [justinas/alice](https://github.com/justinas/alice) — a minimal, idiomatic middleware-chaining library with the same reverse-fold.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-request-logging-middleware.md](02-request-logging-middleware.md)
