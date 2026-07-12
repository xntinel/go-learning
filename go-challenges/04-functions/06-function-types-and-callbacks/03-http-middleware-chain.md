# Exercise 3: Composable HTTP Middleware Chain

Every Go HTTP service is a stack of middleware wrapping a handler: request-ID,
logging, panic recovery, timeouts, auth. This module builds the `Middleware` type
and the `Chain` combinator that composes them, plus four real middleware, and proves
the composition order with a recorder-driven test.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
mw/                         independent module: example.com/mw
  go.mod                    go 1.26
  mw.go                     Middleware, Chain, RequestID, AccessLog, Recover, Timeout
  cmd/
    demo/
      main.go               runnable demo driving the chain with httptest
  mw_test.go                order, panic-recovery, ctx propagation, identity tests
```

Files: `mw.go`, `cmd/demo/main.go`, `mw_test.go`.
Implement: `type Middleware func(http.Handler) http.Handler`, `Chain(mw ...Middleware) Middleware` so the first-listed runs outermost, and request-ID, access-log, panic-recover, and timeout middleware.
Test: order via markers appended by each middleware; a panicking handler yields 500; the request ID reaches the final handler through context; an empty chain is the identity.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/06-function-types-and-callbacks/03-http-middleware-chain/cmd/demo
cd go-solutions/04-functions/06-function-types-and-callbacks/03-http-middleware-chain
```

### The chain, and why order is the whole design

A `Middleware` takes a handler and returns a handler that does something before
and/or after delegating. `Chain(a, b, c)` must compose them so `a` is *outermost* —
`a` sees the request first and the response last, wrapping `b`, which wraps `c`,
which wraps your handler. The natural way to build that is to apply the middleware in
reverse: start from the final handler and wrap it with the last middleware, then the
second-to-last, and so on, so the first-listed ends up on the outside:

```go
func Chain(mws ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			final = mws[i](final)
		}
		return final
	}
}
```

Order is not cosmetic. Panic recovery must be *outer* so it catches a panic from
anything inside it; if recovery sits inside the handler, the panic has already
escaped. A timeout via `http.TimeoutHandler` belongs near the outside so it bounds
the whole downstream stack. Logging usually wraps close to the outside so it records
the final status. Getting the order wrong is the classic middleware bug: a chain
that mutates the shared `*http.Request` instead of deriving a new one via
`r.WithContext`, or a recovery that never fires because it was composed inside the
thing that panics.

The `http.HandlerFunc` adapter is what lets a plain function be an `http.Handler`:
`http.HandlerFunc` is a function type with a `ServeHTTP` method that calls the
receiver, so `http.HandlerFunc(f)` satisfies `http.Handler` with no struct.

Create `mw.go`:

```go
package mw

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Middleware wraps an http.Handler in another http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so the first argument is outermost: it sees the
// request first and the response last.
func Chain(mws ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		for i := len(mws) - 1; i >= 0; i-- {
			final = mws[i](final)
		}
		return final
	}
}

type ctxKey int

const requestIDKey ctxKey = iota

// RequestIDFrom returns the request ID injected by RequestID, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// RequestID injects a request ID into the context. The generator is injectable
// so tests are deterministic.
func RequestID(gen func() string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), requestIDKey, gen())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AccessLog logs one line per request with its request ID and duration.
func AccessLog(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			next.ServeHTTP(w, r)
			log.Info("request",
				"id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"dur", time.Since(start),
			)
		})
	}
}

// Recover turns a panic in a downstream handler into a 500 response.
func Recover(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic recovered", "value", fmt.Sprint(v))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds the downstream handler with http.TimeoutHandler.
func Timeout(d time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, "request timed out")
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"time"

	"example.com/mw"
)

func main() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s", mw.RequestIDFrom(r.Context()))
	})

	seq := 0
	stack := mw.Chain(
		mw.Recover(log),
		mw.Timeout(time.Second),
		mw.AccessLog(log),
		mw.RequestID(func() string { seq++; return fmt.Sprintf("req-%d", seq) }),
	)
	handler := stack(final)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	handler.ServeHTTP(rec, req)

	fmt.Printf("status=%d body=%q\n", rec.Code, rec.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status=200 body="hello req-1"
```

### Tests

`TestOrder` is the key one: two markers-appending middleware prove outer-to-inner on
the request path and inner-to-outer on the response path. `TestRecover` sends a
panicking handler through and asserts a 500. `TestRequestIDReachesHandler` asserts the
injected ID arrives via context. `TestEmptyChainIsIdentity` asserts `Chain()(h)`
behaves exactly like `h`.

Create `mw_test.go`:

```go
package mw

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// record appends a marker before and after delegating, so a chain's execution
// order is observable.
func record(events *[]string, before, after string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*events = append(*events, before)
			next.ServeHTTP(w, r)
			*events = append(*events, after)
		})
	}
}

func TestOrder(t *testing.T) {
	t.Parallel()
	var events []string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "handler")
	})
	stack := Chain(
		record(&events, "outer-in", "outer-out"),
		record(&events, "inner-in", "inner-out"),
	)
	stack(final).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	want := []string{"outer-in", "inner-in", "handler", "inner-out", "outer-out"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events[%d] = %q, want %q (full %v)", i, events[i], want[i], events)
		}
	}
}

func TestRecover(t *testing.T) {
	t.Parallel()
	panics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	handler := Recover(discardLog())(panics)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestRequestIDReachesHandler(t *testing.T) {
	t.Parallel()
	var got string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestIDFrom(r.Context())
	})
	handler := RequestID(func() string { return "fixed-id" })(final)

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if got != "fixed-id" {
		t.Fatalf("request id in handler = %q, want %q", got, "fixed-id")
	}
}

func TestEmptyChainIsIdentity(t *testing.T) {
	t.Parallel()
	called := false
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	Chain()(final).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !called {
		t.Fatal("empty chain did not call the handler; it must be the identity")
	}
}

func ExampleRequestIDFrom() {
	ctx := context.WithValue(context.Background(), requestIDKey, "abc")
	fmt.Println(RequestIDFrom(ctx))
	// Output: abc
}

func TestTimeoutPassesThrough(t *testing.T) {
	t.Parallel()
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	handler := Timeout(time.Second)(final)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (fast handler should not time out)", rec.Code)
	}
}
```

## Review

The chain is correct when the first-listed middleware is provably outermost: the
markers in `TestOrder` come out `outer-in, inner-in, handler, inner-out, outer-out`,
which is the whole contract. Recovery must be composed outside anything that can
panic, and `TestRecover` proves a panicking handler wrapped by `Recover` returns 500
instead of crashing the server. Data flows down through `r.WithContext`, never by
mutating the shared request — `RequestID` derives a new request and the final handler
reads the value back through the context. The empty-chain identity matters because a
`Chain()` with no middleware is a legitimate configuration (a route with no extra
behavior) and must still call the handler. Note `http.TimeoutHandler` writes its own
503 on timeout; the fast-handler test asserts a non-timed-out request passes its
status through untouched.

## Resources

- [net/http.Handler and HandlerFunc](https://pkg.go.dev/net/http#HandlerFunc)
- [net/http.TimeoutHandler](https://pkg.go.dev/net/http#TimeoutHandler)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)
- [context.WithValue](https://pkg.go.dev/context#WithValue)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-functional-options-constructor.md](02-functional-options-constructor.md) | Next: [04-func-type-implements-interface.md](04-func-type-implements-interface.md)
