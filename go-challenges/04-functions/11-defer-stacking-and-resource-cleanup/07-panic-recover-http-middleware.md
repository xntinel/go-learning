# Exercise 7: Panic-Recovery Middleware — 500 Without Swallowing ErrAbortHandler

A handler that panics must not take the whole server down, but a recovery
middleware that swallows *every* panic is worse: it hides real bugs and breaks the
server's own abort protocol. This module builds an `http.Handler` middleware that
turns a panic into a 500, logs the value with a stack trace, and crucially
re-panics on `http.ErrAbortHandler` so the server's abort semantics survive.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
recovermw/                  independent module: example.com/recovermw
  go.mod
  recovermw/recovermw.go     Recoverer middleware; statusRecorder wrapper
  cmd/demo/main.go           middleware over a panicking handler and a 418 handler
  recovermw/recovermw_test.go  panic -> 500 + keeps serving; ErrAbortHandler re-panics; passthrough
```

- Files: `recovermw/recovermw.go`, `cmd/demo/main.go`, `recovermw/recovermw_test.go`.
- Implement: `Recoverer(logger)` returning an `func(http.Handler) http.Handler` whose deferred `recover()` writes a 500 (only if no header was written yet), logs the value plus `debug.Stack()`, and re-panics on `http.ErrAbortHandler`.
- Test: a handler that panics with a string yields 500, the log contains the value, and the next request still serves; a handler that panics with `http.ErrAbortHandler` makes the middleware re-panic (asserted via `recover` in the test) and does not write 500; a normal handler passes through with its own status.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/07-panic-recover-http-middleware/recovermw go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/07-panic-recover-http-middleware/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/07-panic-recover-http-middleware
```

### The recover boundary on a real request path

The middleware wraps the next handler in a `defer func() { if r := recover(); r != nil { ... } }()`.
Because a `defer` runs while a panic unwinds, this closure is guaranteed to run
whether the handler returns normally or panics — that is what makes it a valid
recover boundary. Three details make it production-grade rather than a naive
"catch everything":

- **Re-panic on `http.ErrAbortHandler`.** The `net/http` server panics with this
  exact sentinel when a handler wants to abort the connection without a normal
  response; the server's own outer defer recognizes it and suppresses the usual
  stack-trace logging. If your middleware recovers it and writes a 500, you have
  broken that protocol and turned an intentional abort into a bogus response. So
  the boundary compares the recovered value to `http.ErrAbortHandler` and, on a
  match, re-panics it unchanged. This is the general discipline: recover
  selectively, and re-raise the values you must not handle.

- **Log the value with `debug.Stack()`.** A recovered panic that is silently
  turned into a 500 is a bug you will never find. The boundary logs the recovered
  value and the stack captured by `runtime/debug.Stack()`, so the failure leaves a
  trace even though the request got a clean error response.

- **Write the 500 only if nothing was written yet.** A handler can panic *after*
  it has already called `WriteHeader` or written part of the body. Calling
  `WriteHeader(500)` then would be a no-op with a "superfluous WriteHeader"
  warning, and the client would have already received a different status. The
  standard `http.ResponseWriter` does not expose whether a header was written, so
  the middleware wraps it in a small `statusRecorder` that tracks it and only
  writes the 500 when nothing has gone out.

Create `recovermw/recovermw.go`:

```go
package recovermw

import (
	"log"
	"net/http"
	"runtime/debug"
)

// statusRecorder wraps a ResponseWriter to remember whether a header or body was
// already written, so the recovery boundary does not double-write.
type statusRecorder struct {
	http.ResponseWriter
	wrote bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Recoverer builds middleware that converts a handler panic into a 500, logs the
// value with a stack trace, and re-panics on http.ErrAbortHandler so the server's
// abort semantics are preserved.
func Recoverer(logger *log.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := &statusRecorder{ResponseWriter: w}
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				if rec == http.ErrAbortHandler {
					// The server uses this sentinel to abort the connection.
					// Do not swallow it; let it propagate.
					panic(rec)
				}
				logger.Printf("panic recovered: %v\n%s", rec, debug.Stack())
				if !rw.wrote {
					rw.WriteHeader(http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}
```

### The runnable demo

The demo sends its recovery log to `io.Discard` (the stack trace is
non-deterministic) and prints only the resulting status codes, driving the
middleware with `httptest` records so no real server is needed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	"example.com/recovermw/recovermw"
)

func main() {
	logger := log.New(io.Discard, "", 0)
	mw := recovermw.Recoverer(logger)

	panicky := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	panicky.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println("status:", rec.Code)

	teapot := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec2 := httptest.NewRecorder()
	teapot.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println("status:", rec2.Code)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 500
status: 418
```

### Tests

`httptest` drives the middleware. The first test asserts the 500, checks the log
buffer contains the recovered value, and shows the same middleware serving a
second request afterward. The second test asserts the `http.ErrAbortHandler`
re-panic by recovering it in the test. The third confirms passthrough.

Create `recovermw/recovermw_test.go`:

```go
package recovermw

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecovererConvertsPanicTo500(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	mw := Recoverer(logger)

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom in handler")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "boom in handler") {
		t.Fatalf("log = %q, want it to contain the recovered value", buf.String())
	}

	// The middleware keeps serving: a second, healthy request works.
	ok := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec2 := httptest.NewRecorder()
	ok.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", rec2.Code)
	}
}

func TestRecovererReRaisesErrAbortHandler(t *testing.T) {
	t.Parallel()

	logger := log.New(&bytes.Buffer{}, "", 0)
	mw := Recoverer(logger)

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	rec := httptest.NewRecorder()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected ErrAbortHandler to propagate, but it was swallowed")
		}
		if r != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler", r)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want the default 200 (no 500 written)", rec.Code)
		}
	}()

	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRecovererPassesThroughNormalHandler(t *testing.T) {
	t.Parallel()

	logger := log.New(&bytes.Buffer{}, "", 0)
	mw := Recoverer(logger)

	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
	if rec.Body.String() != "short and stout" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "short and stout")
	}
}
```

`httptest.NewRecorder` defaults `Code` to 200, so a test asserting "no 500 was
written" checks the code stayed at that default.

## Review

The middleware is correct when a panicking handler yields a 500, the recovered
value and stack are logged, and the server keeps serving; when an
`http.ErrAbortHandler` panic propagates unchanged rather than becoming a 500; and
when a normal handler passes through with its own status and body untouched. The
mistake that makes a recovery middleware dangerous is recovering indiscriminately:
swallowing `http.ErrAbortHandler` breaks connection aborts, and swallowing a
genuine bug without logging the stack makes it invisible. The boundary must be
selective — re-raise what it cannot legitimately handle, log everything it does —
and it must avoid double-writing the response, which is why the `statusRecorder`
tracks whether output has already begun. Run `go test -race`.

## Resources

- [net/http: ErrAbortHandler](https://pkg.go.dev/net/http#pkg-variables)
- [runtime/debug: Stack](https://pkg.go.dev/runtime/debug#Stack)
- [net/http/httptest: ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder)
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-latency-timing-defer-and-arg-eval.md](06-latency-timing-defer-and-arg-eval.md) | Next: [08-cleanup-stack-lifo-rollback.md](08-cleanup-stack-lifo-rollback.md)
