# Exercise 4: A recover boundary that isolates handler panics

An unrecovered panic in one handler crashes the whole server by default. The fix
is a `recover()` boundary in the *outermost* middleware that converts a panic into
a 500 — but doing it correctly means respecting the write-once contract and
re-panicking `http.ErrAbortHandler`. This module builds that boundary.

Fully self-contained: its own `go mod init`, a minimal status-tracking writer,
demo, and tests. Nothing here imports another exercise.

## What you'll build

```text
recovermw/                   independent module: example.com/recovermw
  go.mod                     go 1.26
  middleware.go              Chain + Recoverer(*slog.Logger) with a wroteHeader tracker
  cmd/demo/main.go           runnable demo: a panicking handler yields 500, process survives
  middleware_test.go         panic -> 500; ErrAbortHandler re-panics; 200-then-panic keeps 200
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: `Recoverer(*slog.Logger)` wrapping `next` in a deferred `recover()`; on panic it logs the value plus `debug.Stack()` and writes a 500 *only if no header was written yet*; it re-panics `http.ErrAbortHandler`.
- Test: a panicking handler yields 500 and the test completes (no crash); a handler that panics with `http.ErrAbortHandler` is re-panicked; a handler that writes 200 then panics keeps status 200 with no second `WriteHeader`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/recovermw/cmd/demo
cd ~/go-exercises/recovermw
go mod init example.com/recovermw
go mod edit -go=1.26
```

### Why recover must be outermost, and the two traps

Panic recovery has to be the *outermost* middleware because a deferred `recover()`
only catches panics that unwind through its own goroutine's stack. If the recover
layer is inside the logger, a panic in the logger (or in any layer outside recover)
still propagates to the top of the goroutine and crashes the server. Outermost
means it sees panics from every inner layer and the handler.

The naive version — `defer func(){ if v := recover(); v != nil { http.Error(w,
"500", 500) } }()` — has two production bugs.

Trap one: **double-writing the header.** If the handler already called
`w.WriteHeader(200)` (or wrote a body, which implicitly commits 200) *before*
panicking, the status is already on the wire. Writing a 500 now emits
`http: superfluous response.WriteHeader call` and corrupts the response — the
client already received a 200. The fix is to track whether the header has been
written and only write the 500 when it has not. This module wraps `w` in a tiny
`trackingWriter` that flips a `wroteHeader` flag on the first `WriteHeader` or
`Write`, so the recover branch can check it.

Trap two: **swallowing `http.ErrAbortHandler`.** The `net/http` runtime uses this
exact sentinel value as a control signal: a handler that panics with
`http.ErrAbortHandler` is telling the server to abort the response silently, with
no log and no 500. A recover boundary that catches *every* value would swallow
that signal, hiding intentional aborts and defeating the mechanism. The fix is to
re-panic when the recovered value is `http.ErrAbortHandler`, letting the runtime
handle it.

Create `middleware.go`:

```go
package recovermw

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

type Handler = http.Handler

type Middleware func(Handler) Handler

type Chain struct{ middlewares []Middleware }

func NewChain(mws ...Middleware) *Chain {
	cp := make([]Middleware, len(mws))
	copy(cp, mws)
	return &Chain{middlewares: cp}
}

func (c *Chain) Then(h Handler) Handler {
	for i := len(c.middlewares) - 1; i >= 0; i-- {
		h = c.middlewares[i](h)
	}
	return h
}

// trackingWriter records whether the response header has been committed, so the
// recover boundary can avoid a second WriteHeader.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wroteHeader = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wroteHeader = true // the first Write implicitly commits 200
	return t.ResponseWriter.Write(b)
}

// Unwrap lets http.NewResponseController reach the real writer for Flush/Hijack.
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Recoverer converts a handler panic into a 500 (when the response has not yet
// started) and logs the value plus a stack trace. It re-panics
// http.ErrAbortHandler so the runtime's intentional-abort signal survives.
func Recoverer(logger *slog.Logger) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tw := &trackingWriter{ResponseWriter: w}
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					panic(v) // intentional abort: let the runtime handle it
				}
				logger.Error("panic recovered",
					"value", v,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				if !tw.wroteHeader {
					http.Error(tw, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(tw, r)
		})
	}
}
```

### The runnable demo

The demo drives a handler that dereferences a nil map, proving the panic becomes a
500 and the program keeps running (it prints a second line after the recovered
request).

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

	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]int
		m["x"] = 1 // panic: assignment to entry in nil map
	})

	handler := recovermw.NewChain(recovermw.Recoverer(logger)).Then(boom)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Printf("panicking handler -> %d\n", rec.Code)
	fmt.Println("server still running")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
panicking handler -> 500
server still running
```

### Tests

`TestPanicBecomes500` proves the boundary converts a panic to a 500 and the test
survives. `TestRePanicsAbortHandler` proves `http.ErrAbortHandler` is *not*
swallowed — the test's own deferred recover catches the re-panic.
`TestStatusPreservedWhenAlreadyWritten` is the write-once guard: a handler that
commits 200 then panics keeps 200, and the recover branch does not overwrite it.

Create `middleware_test.go`:

```go
package recovermw

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestPanicBecomes500(t *testing.T) {
	t.Parallel()

	boom := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	NewChain(Recoverer(discardLogger())).Then(boom).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
}

func TestRePanicsAbortHandler(t *testing.T) {
	t.Parallel()

	defer func() {
		v := recover()
		if v != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler re-panicked", v)
		}
	}()

	abort := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	NewChain(Recoverer(discardLogger())).Then(abort).ServeHTTP(rec, req)

	t.Fatal("ServeHTTP returned; ErrAbortHandler should have re-panicked")
}

func TestStatusPreservedWhenAlreadyWritten(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("after commit")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	NewChain(Recoverer(discardLogger())).Then(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (header already committed, no overwrite)", rec.Code)
	}
	if rec.Body.String() != "partial" {
		t.Fatalf("body = %q, want partial", rec.Body.String())
	}
}
```

## Review

The recover boundary is correct when it is outermost, when it never double-writes,
and when it re-panics `http.ErrAbortHandler`. `TestStatusPreservedWhenAlreadyWritten`
is the subtle one: once the handler commits a status the boundary must leave it
alone, so the `trackingWriter.wroteHeader` flag gates the 500. If that test shows a
500, your recover path is overwriting a committed response — the exact bug that
produces `superfluous response.WriteHeader call` in production logs.
`TestRePanicsAbortHandler` guards the sentinel: swallowing `http.ErrAbortHandler`
would break the runtime's intentional-abort path. Always log `debug.Stack()` with
the recovered value — a 500 with no stack is nearly useless during an incident.

## Resources

- [Recover](https://pkg.go.dev/builtin#recover) — the built-in that stops a panic unwinding.
- [net/http#ErrAbortHandler](https://pkg.go.dev/net/http#ErrAbortHandler) — the sentinel a recover boundary must re-panic.
- [runtime/debug#Stack](https://pkg.go.dev/runtime/debug#Stack) — the stack trace to log alongside the value.
- [Defer, Panic, and Recover (Go Blog)](https://go.dev/blog/defer-panic-and-recover) — the mechanics of recover in deferred functions.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-require-token-auth-middleware.md](03-require-token-auth-middleware.md) | Next: [05-status-capturing-responsewriter.md](05-status-capturing-responsewriter.md)
