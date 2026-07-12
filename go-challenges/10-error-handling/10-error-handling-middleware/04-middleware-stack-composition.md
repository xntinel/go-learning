# Exercise 4: Compose middleware as an ordered stack

Middleware only works if the order is deliberate and correct. This exercise builds
a `Middleware = func(http.Handler) http.Handler` type and a `Chain` combinator
that wraps a handler so `mws[0]` ends up *outermost*, then proves the ordering is
deterministic and that only an outermost `Recoverer` catches a panic thrown by an
inner middleware.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
mwchain/                     independent module: example.com/mwchain
  go.mod                     go 1.26
  chain.go                   type Middleware; Chain(mws...); Recoverer, RequestID, tracing middleware
  cmd/
    demo/
      main.go                runnable demo: prints the outer-to-inner entry order
  chain_test.go              order-recording test; inner-panic caught only by outer Recoverer
```

Files: `chain.go`, `cmd/demo/main.go`, `chain_test.go`.
Implement: `Middleware`, `Chain(mws ...Middleware) Middleware` such that `mws[0]`
is outermost, and a few real middlewares that record their entry order.
Test: assert execution order is outer-to-inner on the way in; a case where an
inner middleware panics and only an outermost `Recoverer` yields a 500.
Verify: `go test -count=1 -race ./...`

### Why order is a correctness property, not a preference

A middleware is a function from handler to handler: `func(http.Handler)
http.Handler`. To compose several you wrap the innermost handler with each in
turn. The only real decision is *which direction* the list reads. The convention
this exercise adopts — and the one every mainstream Go router uses — is that the
first middleware in the list is the *outermost*: it runs first on the way in and
last on the way out, wrapping everything after it. That reads naturally: `Chain(
Recoverer, RequestID, Logger)` puts `Recoverer` on the outside, exactly where a
panic-catcher belongs.

To make `mws[0]` outermost, `Chain` must apply the list in *reverse* when wrapping.
Wrapping is inside-out: `final = mws[0](mws[1](mws[2](handler)))`. If you fold
left-to-right you get the opposite nesting, so you iterate from the last element
to the first, wrapping the accumulator each time. Getting this backwards is a
classic bug: the stack compiles, requests succeed, and then one day a panic in an
inner middleware escapes because `Recoverer` was accidentally innermost.

That last point is the whole reason order is a *correctness* property. `Recoverer`
can only catch panics thrown by code it wraps. If an authentication or logging
middleware panics and `Recoverer` is inside it, the panic sails past to the
`http.Server`'s connection handler and the client gets a dropped connection
instead of a 500. Put `Recoverer` outermost and it catches panics from every
middleware and the handler alike. The test below proves this directly: the same
inner-panicking middleware yields a clean 500 when `Recoverer` is outermost and an
escaped panic when it is not.

The exercise records order by threading a `*[]string` through the request context
and having each middleware append its name on entry. That is a test scaffold, but
`RequestID` (minting an id into the context) and a tracing middleware (recording a
span name) are the real shapes these take in production.

Create `chain.go`:

```go
package mwchain

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Middleware wraps an http.Handler, returning a handler that adds behavior
// around it.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares so that mws[0] is the OUTERMOST layer: it runs
// first on the way in and last on the way out. It returns a single Middleware.
func Chain(mws ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		// Wrap inside-out: iterate from the last middleware to the first so
		// mws[0] ends up on the outside.
		h := final
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

type ctxKey int

const trailKey ctxKey = 0

// record appends name to the ordered trail stored in the request context.
func record(r *http.Request, name string) {
	if trail, ok := r.Context().Value(trailKey).(*[]string); ok {
		*trail = append(*trail, name)
	}
}

// WithTrail seeds an empty trail slice in the context so downstream middleware
// can record their entry order. Outermost in tests.
func WithTrail(trail *[]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), trailKey, trail)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Named returns a middleware that records its name on entry. Stands in for real
// middleware (RequestID, Logger, tracing) whose ordering we want to pin.
func Named(name string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			record(r, name)
			next.ServeHTTP(w, r)
		})
	}
}

// Recoverer catches panics from everything it wraps and writes a 500. Only
// useful if it is outermost.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.ErrorContext(r.Context(), "recovered", "panic", rec, "stack", string(debug.Stack()))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo chains three recording middlewares around a handler and prints the trail,
demonstrating that `mws[0]` runs first.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/mwchain"
)

func main() {
	var trail []string

	stack := mwchain.Chain(
		mwchain.WithTrail(&trail),
		mwchain.Named("recoverer"),
		mwchain.Named("request-id"),
		mwchain.Named("logger"),
	)

	handler := stack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		panic(err)
	}
	resp.Body.Close()

	fmt.Println("entry order:", trail)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
entry order: [recoverer request-id logger]
```

### Tests

`TestChainOrderOuterToInner` asserts the recorded trail is exactly outer-to-inner.
`TestRecovererMustBeOutermost` runs the *same* inner-panicking middleware in two
configurations: with `Recoverer` outermost the response is a clean 500; with
`Recoverer` innermost the panic escapes `Chain` (caught by the test's own recover),
proving order is a correctness property.

Create `chain_test.go`:

```go
package mwchain

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func init() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestChainOrderOuterToInner(t *testing.T) {
	t.Parallel()

	var trail []string
	stack := Chain(
		WithTrail(&trail),
		Named("a"),
		Named("b"),
		Named("c"),
	)
	handler := stack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	got := fmt.Sprint(trail)
	if want := "[a b c]"; got != want {
		t.Fatalf("entry order = %s, want %s", got, want)
	}
}

// panicMW is a middleware that panics on the way in, simulating a buggy inner
// middleware.
func panicMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("inner middleware bug")
	})
}

func TestRecovererMustBeOutermost(t *testing.T) {
	t.Parallel()

	// Recoverer outermost: it wraps panicMW, so the panic becomes a 500.
	outer := Chain(Recoverer, panicMW)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	outer.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("outermost Recoverer: status = %d, want 500", rec.Code)
	}

	// Recoverer innermost: panicMW wraps Recoverer, so the panic escapes.
	inner := Chain(panicMW, Recoverer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec2 := httptest.NewRecorder()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic to escape when Recoverer is innermost")
			}
		}()
		inner.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if rec2.Code == http.StatusInternalServerError {
		t.Fatal("innermost Recoverer should not have written a 500")
	}
}

func ExampleChain() {
	var trail []string
	stack := Chain(WithTrail(&trail), Named("first"), Named("second"))
	h := stack(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println(trail)
	// Output: [first second]
}
```

## Review

`Chain` is correct when the first argument is the outermost layer — the entry
trail reads in list order and the demo prints `[recoverer request-id logger]`. The
subtle bug the reverse-iteration guards against is folding the list the wrong way,
which silently inverts the stack; `TestChainOrderOuterToInner` catches it. The
deeper lesson is `TestRecovererMustBeOutermost`: the identical panic is a tidy 500
when `Recoverer` wraps the offender and an escaped process-threatening panic when
it does not. That is why "Recoverer outermost" is a rule, not a style preference —
order determines whether your panic safety net is actually under the trapeze. Run
`-race`; middleware that reads and writes the shared trail must be clean (the tests
drive one request at a time, so the trail slice is not shared across goroutines).

## Resources

- [`net/http#Handler`](https://pkg.go.dev/net/http#Handler) — the interface every middleware transforms.
- [Go blog: Routing Enhancements for Go 1.22](https://go.dev/blog/routing-enhancements) — routing and handler composition in the stdlib mux.
- [`context#WithValue`](https://pkg.go.dev/context#WithValue) — request-scoped values threaded through middleware.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-problem-json-error-response.md](05-problem-json-error-response.md)
