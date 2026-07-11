# Exercise 2: The HTTP Middleware Chain

HTTP middleware is the decorator pattern applied to Go's single-method `http.Handler` interface: a middleware takes the next handler and returns a new one that runs code before and after delegating. This exercise builds the `Middleware` type, a `Chain` combinator, three concrete middlewares (request ID, logging, panic recovery), and the ordering test that proves a chain runs outside-in.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
middleware.go        Middleware, Chain, RequestID, RequestIDFrom,
                     Logging (with statusRecorder), Recoverer
cmd/
  demo/
    main.go          chain Recoverer, RequestID, Logging over a handler
                     and drive a normal request and a panicking one
middleware_test.go   chain ordering, identity chain, request-ID propagation,
                     status logging, panic-to-500, recovery outside logging
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Middleware` as `func(http.Handler) http.Handler`, `Chain` that makes the first middleware outermost, and the `RequestID`, `Logging`, and `Recoverer` middlewares.
- Test: a three-layer chain runs `A-in, B-in, C-in, handler, C-out, B-out, A-out`; the request ID reaches the context and the response header; logging reports `METHOD PATH STATUS`; a panicking handler becomes a 500; recovery placed outside logging still produces the 500 and suppresses the log line.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p http-middleware/cmd/demo && cd http-middleware
go mod init example.com/http-middleware
```

### Why `func(http.Handler) http.Handler` is the whole pattern

A `http.Handler` is anything with `ServeHTTP(http.ResponseWriter, *http.Request)`. A middleware is a function that takes one handler and returns another: `type Middleware func(http.Handler) http.Handler`. The returned handler closes over the original (`next`), does something before calling `next.ServeHTTP`, and something after. Because the returned value is itself a `http.Handler`, a middleware can wrap a handler that is itself the output of another middleware, and the whole stack is still one `http.Handler` you can hand to any router. That is the decorator pattern with the interface fixed to `http.Handler` and the "constructor" expressed as a function instead of a struct.

`Chain` exists so callers list middlewares in the natural reading order — outermost first — rather than nesting constructor calls by hand. The implementation applies the slice in reverse: it wraps the handler with the last middleware first, then the second-to-last, and so on, so that `mws[0]` ends up as the outermost wrapper and therefore the first to see a request. If you applied the slice forward instead, the list would read inside-out, which is the single most common source of "my middleware runs in the wrong order" confusion. Writing the reversal once, in `Chain`, fixes the order for every caller.

### Reading order off the trace

Every middleware has the mirror-image shape from the concepts file: code before `next.ServeHTTP` runs on the way in; code after runs on the way out. With three tracing middlewares chained as `Chain(handler, A, B, C)`, a single request produces `A:in, B:in, C:in, handler, C:out, B:out, A:out`. The "in" labels descend in list order because `A` is outermost; the "out" labels ascend because the call stack unwinds from the innermost handler back out. The ordering test asserts that exact string, which is the most direct possible proof that `Chain` nests layers correctly. This is also why order is a behavioral choice: `Recoverer` must be outside `Logging` to catch a panic that unwinds through it, and `RequestID` must be outside `Logging` so the ID exists by the time the log line is written.

### The three concrete middlewares

`RequestID` takes an ID generator so tests can make it deterministic. On each request it generates an ID, sets it on the `X-Request-ID` response header, stores it in the request context with an unexported key type, and forwards a shallow-copied request via `r.WithContext(ctx)`. The unexported `ctxKey` type is the standard guard against context-key collisions: no other package can construct a value of that type, so no other package can accidentally overwrite or read this entry. `RequestIDFrom` is the typed accessor that hides the `ctx.Value` type assertion.

`Logging` must report the response status, which the handler writes and the middleware does not otherwise see. The trick is a tiny `statusRecorder` that embeds the real `http.ResponseWriter` and overrides only `WriteHeader` to remember the code before delegating. Embedding means the recorder satisfies `http.ResponseWriter` for free — every method except `WriteHeader` is promoted from the embedded value — so the handler underneath is unaffected. The recorder defaults its status to `http.StatusOK`, because a handler that writes a body without an explicit `WriteHeader` implicitly sends 200.

`Recoverer` wraps the delegation in a deferred `recover`. If an inner layer panics, the deferred function catches it and writes a 500 instead of letting the panic crash the server's goroutine. Because it sits at the outside of the chain, it guards not only the handler but every middleware between it and the handler — which is exactly why a panic that unwinds through `Logging` reaches `Recoverer` and is turned into a clean 500.

Create `middleware.go`:

```go
package middleware

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// Middleware decorates an http.Handler: it takes the next handler and returns a
// new handler that adds behavior before and after delegating to it.
type Middleware func(http.Handler) http.Handler

// Chain wraps h with mws so that mws[0] is the outermost layer: it sees the
// request first and the response last. Applying the slice in reverse makes the
// first middleware the last one applied, hence the outermost wrapper.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID assigns an ID from gen to each request, sets it on the
// X-Request-ID response header, and stores it in the request context before
// delegating. Placing it outside logging lets the log line carry the ID.
func RequestID(gen func() string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := gen()
			w.Header().Set("X-Request-ID", id)
			ctx := context.WithValue(r.Context(), requestIDKey, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDFrom returns the request ID stored by RequestID, or "" if absent.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusRecorder remembers the status code written to the underlying
// ResponseWriter so a logging middleware can report it after the handler runs.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Logging writes "METHOD PATH STATUS" to w after the wrapped handler responds.
// It runs after the handler, so this code is on the "way out" of the chain.
func Logging(w io.Writer) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: rw, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			fmt.Fprintf(w, "%s %s %d\n", r.Method, r.URL.Path, rec.status)
		})
	}
}

// Recoverer catches a panic from any inner layer and turns it into a 500 so a
// single bad handler cannot crash the server. It belongs at the outside of the
// chain so it also guards the middlewares between it and the handler.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo chains `Recoverer`, `RequestID`, and `Logging` over a handler that echoes the request ID for any path except `/panic`, where it panics. It drives one normal request and one panicking request through `httptest.NewRecorder`, which runs the handler synchronously in the same goroutine, so the output is fully deterministic. Note what the panicking request shows: the `X-Request-ID` header is still set (because `RequestID` ran before the handler panicked), the status is 500 (because `Recoverer` caught the panic), and `Logging` printed nothing for it (because the panic unwound past `Logging`'s post-handler line).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"example.com/http-middleware"
)

func main() {
	var n int
	gen := func() string {
		n++
		return fmt.Sprintf("req-%d", n)
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("boom")
		}
		fmt.Fprintf(w, "hello %s", middleware.RequestIDFrom(r.Context()))
	})

	// Recoverer outermost (guards everything), then RequestID (so the ID exists
	// before logging and the handler), then Logging, then the handler.
	h := middleware.Chain(final,
		middleware.Recoverer,
		middleware.RequestID(gen),
		middleware.Logging(os.Stdout),
	)

	for _, path := range []string{"/", "/panic"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Printf("%s -> status=%d body=%q reqID=%s\n",
			path, rec.Code, rec.Body.String(), rec.Header().Get("X-Request-ID"))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET / 200
/ -> status=200 body="hello req-1" reqID=req-1
/panic -> status=500 body="" reqID=req-2
```

The `GET / 200` line is `Logging`'s output, printed during the first request's unwind, before the demo's own summary line for `/`. The `/panic` request has no `Logging` line because the panic skipped it, yet still carries `reqID=req-2` because `RequestID` set the header before delegating.

### Tests

The tests use `httptest.NewRecorder` and `httptest.NewRequest` so every handler runs synchronously in the test goroutine — no server, no goroutine, no race when the test reads what the handlers recorded. The ordering test is the centerpiece: it chains three tracing middlewares and asserts the exact `in`/`out` sequence. The rest pin each concrete middleware, and the last test pins the behavioral consequence of order — recovery outside logging still yields a 500 while the log line is suppressed.

Create `middleware_test.go`:

```go
package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// trace records "label:in" before delegating and "label:out" after, so a chain
// of traces reveals the exact order layers run on the way in and the way out.
func trace(log *[]string, label string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*log = append(*log, label+":in")
			next.ServeHTTP(w, r)
			*log = append(*log, label+":out")
		})
	}
}

func TestChain_OrdersLayersOutsideIn(t *testing.T) {
	t.Parallel()

	var log []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log = append(log, "handler")
		w.WriteHeader(http.StatusOK)
	})

	h := Chain(handler, trace(&log, "A"), trace(&log, "B"), trace(&log, "C"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	got := strings.Join(log, ",")
	want := "A:in,B:in,C:in,handler,C:out,B:out,A:out"
	if got != want {
		t.Errorf("order =\n  %s\nwant\n  %s", got, want)
	}
}

func TestChain_EmptyIsIdentity(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "bare")
	})

	rec := httptest.NewRecorder()
	Chain(handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Body.String() != "bare" {
		t.Errorf("body = %q, want bare", rec.Body.String())
	}
}

func TestRequestID_SetsHeaderAndContext(t *testing.T) {
	t.Parallel()

	var seen string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	})

	h := Chain(handler, RequestID(func() string { return "fixed-id" }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen != "fixed-id" {
		t.Errorf("context ID = %q, want fixed-id", seen)
	}
	if got := rec.Header().Get("X-Request-ID"); got != "fixed-id" {
		t.Errorf("header X-Request-ID = %q, want fixed-id", got)
	}
}

func TestLogging_ReportsMethodPathStatus(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	h := Chain(handler, Logging(&buf))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/widgets", nil))

	if got := strings.TrimSpace(buf.String()); got != "POST /widgets 418" {
		t.Errorf("log = %q, want POST /widgets 418", got)
	}
}

func TestRecoverer_TurnsPanicInto500(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	h := Chain(handler, Recoverer)

	rec := httptest.NewRecorder()
	// Must not propagate the panic to the caller.
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestRecoverer_OutsideLoggingStillResponds(t *testing.T) {
	t.Parallel()

	var buf strings.Builder
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	// Recoverer is outermost, so it catches the panic that unwinds through
	// Logging. Logging's own line never prints because the panic skips its
	// post-handler code, which is exactly why recovery must wrap logging.
	h := Chain(handler, Recoverer, Logging(&buf))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("log = %q, want empty (panic skipped the log line)", buf.String())
	}
}
```

## Review

The chain is correct when the ordering test prints `A:in, B:in, C:in, handler, C:out, B:out, A:out`. If `Chain` applied its middlewares forward instead of in reverse, the trace would invert and the test would fail immediately — that single assertion is the proof the combinator nests layers the way the list reads. Confirm `RequestID` uses an unexported `ctxKey` type for its context key; a string key would risk a silent collision with another package's key. Confirm `statusRecorder` embeds `http.ResponseWriter` rather than reimplementing it, so `Write` and `Header` are promoted unchanged and only `WriteHeader` is intercepted, and that its default status is 200 so a handler that never calls `WriteHeader` still logs the right code.

The last test captures the lesson that order is behavior. With `Recoverer` outside `Logging`, a panic unwinds up through `Logging` — skipping the `fmt.Fprintf` that would have logged the line — and is caught by `Recoverer`, which writes the 500. Swap the two so logging is outside recovery and the panic is caught lower down, the log line runs, and the visible behavior changes even though neither middleware's code changed. Tests that drive the handler with `httptest.NewRecorder` keep this deterministic: everything runs in one goroutine, so reading the recorded slice or buffer after `ServeHTTP` returns never races a handler still finishing in the background, which it could if the test used a live `httptest.Server`.

## Resources

- [`http.Handler` and `http.HandlerFunc`](https://pkg.go.dev/net/http#Handler) — the single-method interface the whole pattern decorates.
- [Writing middleware in Go (Mat Ryer)](https://medium.com/@matryer/writing-middleware-in-golang-and-how-go-makes-it-so-much-fun-4375c1246e81) — the canonical explanation of the `func(http.Handler) http.Handler` idiom and chaining.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest`, used to exercise handlers synchronously in tests.
- [`context.WithValue`](https://pkg.go.dev/context#WithValue) — request-scoped values and why context keys should be an unexported type.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-instrumented-repository.md](01-instrumented-repository.md) | Next: [03-generic-function-decorators.md](03-generic-function-decorators.md)
