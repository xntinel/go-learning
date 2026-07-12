# Exercise 2: HTTP Panic-Recovery Middleware with Stack Capture

Every production Go service needs exactly one piece of infrastructure at its
request boundary: a middleware that turns a panic in any handler into a clean 500
instead of a crashed process, captures the stack for observability, and does not
drown the logs in noise from clients that hang up mid-request. This module builds
that canonical middleware.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
recovermw/                   independent module: example.com/recovermw
  go.mod                     go 1.26
  recovermw.go               Recover(*slog.Logger) middleware; wroteTracker
  cmd/
    demo/
      main.go                runnable demo: panicking handler -> 500, healthy -> 200
  recovermw_test.go          httptest: 500+stack logged; ErrAbortHandler pass-through; partial body preserved
```

Files: `recovermw.go`, `cmd/demo/main.go`, `recovermw_test.go`.
Implement: `Recover(logger *slog.Logger) func(http.Handler) http.Handler` that recovers a downstream panic, captures `runtime/debug.Stack`, logs once through `*slog.Logger`, writes a 500 only if nothing was written yet, and re-panics `http.ErrAbortHandler` without logging a stack.
Test: `httptest.NewRecorder` with a panicking handler asserts status 500 and a logged stack; an `http.ErrAbortHandler` panic re-panics with no log and no forced 500; a healthy handler passes through; a partial-body-then-panic handler keeps its already-sent status.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/08-panic-vs-error/02-panic-recovery-http-middleware/cmd/demo
cd go-solutions/10-error-handling/08-panic-vs-error/02-panic-recovery-http-middleware
go mod edit -go=1.26
```

### Why this is the one boundary that matters

The handler goroutine is where a request's work happens, and it is a legitimate
recovery boundary: a panic here should fail *this one request* with a 500 and let
the server keep serving everyone else. Without the middleware, an unrecovered
handler panic unwinds to the top of the handler goroutine and the Go runtime
terminates the whole process — every in-flight request dies. (Note the sharp
limit: this middleware protects the handler goroutine and nothing it spawns with
`go`. A panic in a goroutine the handler launches still crashes the process; that
is Exercise 3.)

Three details separate a real middleware from a naive one.

*Capture the stack at the recovery site.* `runtime/debug.Stack()` returns `[]byte`,
the formatted trace of the calling goroutine, and it is only meaningful *inside*
the deferred function — once the stack unwinds past the middleware the frames are
gone. Logging `fmt.Sprintf("%v", r)` alone throws away the one thing on-call needs.

*Do not corrupt a partial response.* A handler may have already called
`WriteHeader(200)` and streamed part of a body before it panicked. The status line
is already on the wire; you cannot change it. Writing `http.Error(w, ..., 500)`
now appends a second status and garbles the response. So the middleware wraps the
`ResponseWriter` in a tiny tracker that records whether anything was written, and
synthesizes the 500 *only if nothing has been written yet*.

*Special-case `http.ErrAbortHandler`.* This sentinel is how `net/http` (and
`httputil.ReverseProxy`) signal "abort this response quietly" — typically a client
that disconnected. The `net/http` server recognizes it and suppresses the usual
panic-stack log. A recovery middleware that logs every recovered value as a 500
would flood the logs with a stack trace for every routine client abort. The fix is
to detect it with `errors.Is` and re-panic it, letting the server's own handling
take over without a spurious error log.

Create `recovermw.go`:

```go
package recovermw

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// wroteTracker wraps a ResponseWriter and records whether the handler has already
// written a status or body. The middleware consults it so it never appends a 500
// to a response that is already partway out the door.
type wroteTracker struct {
	http.ResponseWriter
	wrote bool
}

func (w *wroteTracker) WriteHeader(code int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *wroteTracker) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

// Recover returns middleware that turns a panic in the wrapped handler into a
// 500, captures the stack for observability, and re-panics http.ErrAbortHandler
// so the net/http server can abort the response without a spurious stack log.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tw := &wroteTracker{ResponseWriter: w}
			defer func() {
				rv := recover()
				if rv == nil {
					return
				}
				// A client-abort panic: let the server handle it, no stack log.
				if err, ok := rv.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rv)
				}
				logger.Error("recovered panic in http handler",
					slog.Any("panic", rv),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())),
				)
				// Only synthesize a 500 if the handler wrote nothing yet.
				if !tw.wrote {
					http.Error(tw, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(tw, r)
		})
	}
}
```

### The runnable demo

The demo routes a panicking handler and a healthy handler through the middleware
using `httptest`, discarding the log so the output is deterministic (a real stack
trace is not reproducible).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"example.com/recovermw"
)

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mw := recovermw.Recover(logger)

	crash := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom: nil user in session")
	}))
	rec := httptest.NewRecorder()
	crash.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/crash", nil))
	fmt.Printf("panicking handler -> status %d\n", rec.Code)

	healthy := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "pong")
	}))
	rec2 := httptest.NewRecorder()
	healthy.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/health", nil))
	fmt.Printf("healthy handler -> status %d body %q\n", rec2.Code, rec2.Body.String())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
panicking handler -> status 500
healthy handler -> status 200 body "pong"
```

### Tests

The tests inject a `slog.Logger` writing to a `bytes.Buffer`, so a case can assert
whether a stack was logged. `TestPanicBecomes500` proves the crash path; the buffer
must contain the stack. `TestErrAbortHandlerPassesThrough` proves the sentinel
re-panics with nothing logged and no 500 forced. `TestHealthyPassesThrough` proves
the happy path is untouched. `TestPartialBodyNotCorrupted` proves the middleware
respects an already-sent status.

Create `recovermw_test.go`:

```go
package recovermw

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestPanicBecomes500(t *testing.T) {
	t.Parallel()
	logger, buf := newLogger()
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	log := buf.String()
	if !strings.Contains(log, "recovered panic") {
		t.Fatalf("log missing recovery message: %q", log)
	}
	// The stack must have been captured, not just the value.
	if !strings.Contains(log, "runtime/debug.Stack") && !strings.Contains(log, "goroutine") {
		t.Fatalf("log missing stack trace: %q", log)
	}
}

func TestErrAbortHandlerPassesThrough(t *testing.T) {
	t.Parallel()
	logger, buf := newLogger()
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	rec := httptest.NewRecorder()

	defer func() {
		rv := recover()
		if rv == nil {
			t.Fatal("expected ErrAbortHandler to be re-panicked")
		}
		err, ok := rv.(error)
		if !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Fatalf("re-panic value = %v, want ErrAbortHandler", rv)
		}
		if rec.Code == http.StatusInternalServerError {
			t.Fatal("must not force a 500 for a client abort")
		}
		if buf.Len() != 0 {
			t.Fatalf("must not log a stack for a client abort, got %q", buf.String())
		}
	}()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
}

func TestHealthyPassesThrough(t *testing.T) {
	t.Parallel()
	logger, buf := newLogger()
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}
	if buf.Len() != 0 {
		t.Fatalf("healthy request should log nothing, got %q", buf.String())
	}
}

func TestPartialBodyNotCorrupted(t *testing.T) {
	t.Parallel()
	logger, buf := newLogger()
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom after partial write")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	// The status was already 200 on the wire; the middleware must not overwrite it.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (already sent)", rec.Code)
	}
	if got := rec.Body.String(); got != "partial" {
		t.Fatalf("body = %q, want %q (no 500 appended)", got, "partial")
	}
	// It must still have logged the panic for observability.
	if !strings.Contains(buf.String(), "recovered panic") {
		t.Fatalf("panic after partial write must still be logged: %q", buf.String())
	}
}

func ExampleRecover() {
	logger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
	h := Recover(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println("status:", rec.Code)
	// Output: status: 500
}
```

## Review

The middleware is correct when a handler panic produces a 500 with a captured
stack, a healthy handler is untouched, and a client-abort panic is neither logged
nor turned into a 500. The two subtle guarantees are the ones teams get wrong in
production: the `wroteTracker` prevents appending a 500 to a response whose status
is already on the wire, and the `http.ErrAbortHandler` special-case keeps routine
client disconnects out of the error logs. Capturing `debug.Stack()` inside the
deferred function — not after it returns — is what makes the trace usable. Run
`go test -race` to confirm the wrapper is safe, and note the boundary's limit: it
does not reach goroutines the handler spawns, which is the subject of Exercise 3.

## Resources

- [`net/http.ErrAbortHandler`](https://pkg.go.dev/net/http#ErrAbortHandler) — the sentinel panic that suppresses stack-trace logging.
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack) — capture the calling goroutine's trace at the recovery site.
- [`log/slog`](https://pkg.go.dev/log/slog) — structured logging of the recovered value and stack.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for boundary tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-goroutine-panic-isolation-worker.md](03-goroutine-panic-isolation-worker.md)
