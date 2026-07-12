# Exercise 7: Composing an HTTP Handler from a Variadic Middleware Chain

Every Go web service composes cross-cutting concerns — logging, auth, recovery,
rate limiting — as a chain of middleware wrapped around a base handler. The
idiomatic API is variadic: `Chain(h, mw...)`. You build it, and you pin the one
property that makes or breaks it: the execution order is a documented contract,
not an accident of the loop direction.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
mwchain/                   independent module: example.com/mwchain
  go.mod                   go 1.25
  chain.go                 type Middleware; Chain(h http.Handler, mw ...Middleware)
  cmd/
    demo/
      main.go              runnable demo: chain three middleware, observe order
  chain_test.go            spy-based order test, zero-middleware, splat equivalence
```

- Files: `chain.go`, `cmd/demo/main.go`, `chain_test.go`.
- Implement: `Chain(h http.Handler, mw ...Middleware) http.Handler` applying middleware outermost-first (`mw[0]` wraps the rest).
- Test: a spy that appends its name to a shared slice proves the order; `Chain(h)` with no middleware is an equivalent handler; a runtime-built `[]Middleware` splatted yields identical order.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Variadic-of-functions and why iteration direction is the contract

A `Middleware` is a decorator: `func(http.Handler) http.Handler`. `Chain` takes a
base handler and a variadic list of them, and the "same kind of thing" here is a
function, so variadic-of-functions is the right shape. The whole implementation is
a loop that wraps the handler once per middleware — but the *direction* of that
loop is an observable API decision.

The contract chosen here is outermost-first: `Chain(h, A, B, C)` produces
`A(B(C(h)))`, so on an incoming request `A` runs first (its pre-work), then `B`,
then `C`, then the handler, and on the way out they unwind in reverse. That reads
top-to-bottom the way the call site lists them, which is the least surprising
contract and the one `net/http` ecosystems converge on. To build `A(B(C(h)))` you
must wrap from the *inside out*, so you iterate the slice in reverse:

```go
for i := len(mw) - 1; i >= 0; i-- {
	h = mw[i](h)
}
```

Iterate forward instead and you would get `C(B(A(h)))` — the reverse order, a
different and equally "valid-looking" contract that would silently run your auth
middleware after your logging middleware. The only way to keep this honest is a
test that observes the order directly; a spy middleware that appends its name to a
shared slice does exactly that.

The zero-middleware case falls out for free: `Chain(h)` never enters the loop and
returns `h` unchanged — an equivalent handler. That is the empty-variadic path,
and it must be tested, not assumed.

Create `chain.go`:

```go
// chain.go
package mwchain

import "net/http"

// Middleware decorates an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain wraps h with the given middleware, outermost-first: Chain(h, A, B, C)
// yields A(B(C(h))), so A runs first on the way in. With no middleware it returns
// h unchanged.
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
```

### The runnable demo

The demo builds three middleware that print when they run, chains them, and serves
one request through an `httptest.NewRecorder`, so you watch the outermost-first
order on the way in and its reverse on the way out.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/mwchain"
)

func tag(name string) mwchain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Printf("enter %s\n", name)
			next.ServeHTTP(w, r)
			fmt.Printf("exit %s\n", name)
		})
	}
}

func main() {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Println("handler")
		w.WriteHeader(http.StatusOK)
	})

	h := mwchain.Chain(base, tag("auth"), tag("log"), tag("gzip"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
enter auth
enter log
enter gzip
handler
exit gzip
exit log
exit auth
```

### Tests

`TestChainOrder` records the order into a shared slice as each middleware enters,
proving the outermost-first contract deterministically. `TestChainSplatEquivalent`
builds the middleware list at runtime and splats it, proving the splat form matches
the inline form. `TestChainNoMiddleware` pins the empty-variadic path.

Create `chain_test.go`:

```go
// chain_test.go
package mwchain

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// record returns middleware that appends its name to *order when it runs.
func record(name string, order *[]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*order = append(*order, name)
			next.ServeHTTP(w, r)
		})
	}
}

func serve(h http.Handler) {
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestChainOrder(t *testing.T) {
	t.Parallel()

	var order []string
	base := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	})

	h := Chain(base, record("auth", &order), record("log", &order), record("gzip", &order))
	serve(h)

	want := []string{"auth", "log", "gzip", "handler"}
	if !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestChainNoMiddleware(t *testing.T) {
	t.Parallel()

	var called bool
	base := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})

	h := Chain(base)
	serve(h)

	if !called {
		t.Fatal("Chain(h) with no middleware must serve the base handler")
	}
}

func TestChainSplatEquivalent(t *testing.T) {
	t.Parallel()

	var inlineOrder, splatOrder []string
	inlineBase := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		inlineOrder = append(inlineOrder, "handler")
	})
	splatBase := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		splatOrder = append(splatOrder, "handler")
	})

	serve(Chain(inlineBase, record("a", &inlineOrder), record("b", &inlineOrder)))

	built := []Middleware{record("a", &splatOrder), record("b", &splatOrder)}
	serve(Chain(splatBase, built...))

	if !slices.Equal(inlineOrder, splatOrder) {
		t.Fatalf("splat order %v != inline order %v", splatOrder, inlineOrder)
	}
}

func ExampleChain() {
	var order []string
	base := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	})

	h := Chain(base, record("auth", &order), record("log", &order))
	serve(h)

	fmt.Println(order)
	// Output: [auth log handler]
}
```

## Review

`Chain` is correct when `Chain(h, A, B, C)` runs `A` outermost and `Chain(h)`
returns an equivalent handler. The load-bearing detail is the reverse-iteration
loop: it is what makes `mw[0]` the outer wrapper, and the spy test is the only way
to keep that contract from silently flipping during a refactor. The general lesson
is that with any variadic-of-functions pipeline — middleware, validation rules,
option appliers — the iteration order is part of your public API and deserves an
explicit, order-observing test. Run `go test -race`.

## Resources

- [`net/http`: `Handler` and `HandlerFunc`](https://pkg.go.dev/net/http#Handler)
- [`net/http/httptest`: `NewRecorder` and `NewRequest`](https://pkg.go.dev/net/http/httptest)
- [Go Blog: composable middleware patterns](https://go.dev/blog/context)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-sql-in-clause-args-builder.md](06-sql-in-clause-args-builder.md) | Next: [08-validator-combinator-errorsjoin.md](08-validator-combinator-errorsjoin.md)
