# Exercise 2: Composing net/http Middleware into an Ordered Chain

A middleware is an `http.Handler -> http.Handler` decorator. A chain folds several
of them into one, applied in a defined order. This is the exact shape chi, negroni,
and every hand-rolled gateway use, and getting the order right is the whole game.

## What you'll build

```text
mwchain/                     independent module: example.com/mwchain
  go.mod                     go 1.25
  middleware.go              type Middleware; Chain; RequestID, AccessLog, Recover, RequireAuth
  middleware_test.go         ordering, recover->500, auth short-circuit, status capture
  cmd/demo/
    main.go                  wires a chain around a final handler and serves one request
```

- Files: `middleware.go`, `middleware_test.go`, `cmd/demo/main.go`.
- Implement: `Middleware func(http.Handler) http.Handler`, `Chain(mws ...Middleware) Middleware`, and the four concrete middlewares.
- Test: outermost-first ordering, `Recover` turns a panic into 500, `RequireAuth` returns 401 without calling the inner handler, `AccessLog` captures the real status the inner handler wrote.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### What Chain must guarantee about order

`Chain(a, b, c)(final)` must produce a handler that runs `a` outermost, then `b`,
then `c`, then `final`. "Outermost" means `a` sees the request first and the
response last: it wraps everything inside it. The natural way to build that is to
apply the middlewares in reverse, wrapping the final handler from the inside out:
start with `final`, wrap it in `c`, wrap that in `b`, wrap that in `a`. Iterating
the slice back to front and re-assigning `h = mws[i](h)` does exactly this. Iterate
front to back instead and you get the reverse order — `c` outermost — which is a
classic, silent bug: auth would run after logging, recovery would not cover the
middlewares above it.

Order matters for correctness, not taste. `Recover` must be near the outside so it
catches panics from everything below it. `RequestID` must run before `AccessLog` so
the log line can include the id. `RequireAuth` must run before the expensive inner
handler so an unauthenticated request is rejected cheaply. The chain is where you
encode those decisions once.

### Capturing the status code

`http.ResponseWriter` gives you no way to read back the status a handler wrote —
`WriteHeader` is write-only. To log the real status, `AccessLog` wraps the writer
in a small `statusRecorder` that remembers the first `WriteHeader` call. Two
subtleties: a handler that writes a body without calling `WriteHeader` implicitly
sends 200, so the recorder defaults to 200; and a handler must not be allowed to
set the status twice, so the recorder guards against a second `WriteHeader` to
avoid the "superfluous WriteHeader" warning and a wrong recorded code.

Create `middleware.go`:

```go
package mwchain

import (
	"context"
	"net/http"
)

// Middleware decorates an http.Handler with additional behavior.
type Middleware func(http.Handler) http.Handler

// Chain folds middlewares into one so that Chain(a, b, c)(final) runs a
// outermost, then b, then c, then final. It applies them in reverse so the
// first listed wraps all the others.
func Chain(mws ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		h := final
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDFrom returns the request id stored in ctx, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// RequestID injects a fixed-per-request id into the context. A real gateway
// would generate a random id; gen is injected so tests are deterministic.
func RequestID(gen func() string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), requestIDKey, gen())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// statusRecorder wraps a ResponseWriter to capture the status code and guard
// against a second WriteHeader.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.written {
		return
	}
	r.status = code
	r.written = true
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.written {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(b)
}

// AccessLog records the status the inner handler produced through logf.
func AccessLog(logf func(format string, args ...any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logf("%s %s -> %d id=%s", r.Method, r.URL.Path, rec.status, RequestIDFrom(r.Context()))
		})
	}
}

// Recover turns a panic in a downstream handler into a 500 instead of crashing
// the server goroutine.
func Recover(logf func(format string, args ...any)) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					logf("recovered panic: %v", v)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuth rejects any request without a bearer token with 401 and does not
// call the inner handler.
func RequireAuth() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			auth := r.Header.Get("Authorization")
			if len(auth) <= len(prefix) || auth[:len(prefix)] != prefix {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
```

The logging middlewares take a `logf` callback rather than importing a logger, so
tests can capture what would be logged. Now the demo.

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
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s\n", mwchain.RequestIDFrom(r.Context()))
	})

	stack := mwchain.Chain(
		mwchain.Recover(func(string, ...any) {}),
		mwchain.RequestID(func() string { return "req-123" }),
		mwchain.AccessLog(func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		}),
		mwchain.RequireAuth(),
	)
	handler := stack(final)

	// Authorized request.
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	fmt.Printf("status=%d body=%q\n", rec.Code, rec.Body.String())

	// Unauthorized request: RequireAuth short-circuits.
	req2 := httptest.NewRequest(http.MethodGet, "/orders", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	fmt.Printf("status=%d\n", rec2.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /orders -> 200 id=req-123
status=200 body="hello req-123\n"
GET /orders -> 401 id=req-123
status=401
```

Each `AccessLog` line prints before its `status=` line because the log callback
runs inside `ServeHTTP`, before `main` reads `rec.Code`. The unauthorized request
still gets a log line: `AccessLog` sits above `RequireAuth` in the chain, so it
logs after `RequireAuth` writes the 401 and returns. The precise ordering is what
the tests assert rather than eyeball.

### Tests

Ordering is asserted by having each middleware append its name to a shared slice,
then checking the recorded sequence is outermost-first. `Recover` is proven by a
handler that panics and asserting the recorder shows 500, not a crashed goroutine.
`RequireAuth` is proven to short-circuit with a sentinel bool that the inner
handler would flip if it ran.

Create `middleware_test.go`:

```go
package mwchain

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func recordOrder(name string, log *[]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*log = append(*log, name)
			next.ServeHTTP(w, r)
		})
	}
}

func TestChainRunsOutermostFirst(t *testing.T) {
	t.Parallel()

	var order []string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	})
	stack := Chain(
		recordOrder("a", &order),
		recordOrder("b", &order),
		recordOrder("c", &order),
	)
	stack(final).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"a", "b", "c", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	t.Parallel()

	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	h := Recover(func(string, ...any) {})(panicky)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestRequireAuthShortCircuits(t *testing.T) {
	t.Parallel()

	innerRan := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerRan = true
	})
	h := RequireAuth()(inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if innerRan {
		t.Fatal("inner handler ran despite missing bearer token")
	}
}

func TestRequireAuthPassesWithBearer(t *testing.T) {
	t.Parallel()

	innerRan := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerRan = true
		w.WriteHeader(http.StatusNoContent)
	})
	h := RequireAuth()(inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer abc")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !innerRan {
		t.Fatal("inner handler did not run with a valid bearer token")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestAccessLogCapturesInnerStatus(t *testing.T) {
	t.Parallel()

	var logged int
	logf := func(format string, args ...any) {
		// args: method, path, status, id
		logged = args[2].(int)
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	h := AccessLog(logf)(inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if logged != http.StatusTeapot {
		t.Fatalf("AccessLog captured %d, want %d", logged, http.StatusTeapot)
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("recorder saw %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestStatusRecorderGuardsDoubleWriteHeader(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rec, status: http.StatusOK}
	sr.WriteHeader(http.StatusCreated)
	sr.WriteHeader(http.StatusInternalServerError) // must be ignored

	if sr.status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (second WriteHeader must be ignored)", sr.status)
	}
}
```

## Review

The chain is correct when `Chain(a, b, c)(final)` runs `a`, `b`, `c`, `handler` in
that order — pinned by the append-to-slice test, not by reading the loop. The most
common defect is iterating the slice forward and inverting the order; the reverse
loop is what makes the first listed middleware outermost. `Recover` must sit high
enough to cover the handlers below it, and its test proves a panic becomes a 500
rather than a crashed goroutine. `RequireAuth` must reject before the inner handler
runs — the `innerRan` sentinel proves it. `AccessLog` must read the status the
handler actually wrote, which is why the `statusRecorder` exists and why it guards
against a second `WriteHeader`. Run `go test -race` since middlewares run
concurrently across requests in a real server.

## Resources

- [net/http package](https://pkg.go.dev/net/http) — `Handler`, `HandlerFunc`, `ResponseWriter`, `Error`.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder`, `NewRequest`.
- [context package](https://pkg.go.dev/context) — `WithValue` for request-scoped values.
- [Dave Cheney: Functional options for friendly APIs](https://dave.cheney.net/2014/10/17/functional-options-for-friendly-apis) — functions-as-configuration, the pattern behind composable middleware constructors.

---

Back to [01-retry-pipeline-combinators.md](01-retry-pipeline-combinators.md) | Next: [03-composable-comparators.md](03-composable-comparators.md)
