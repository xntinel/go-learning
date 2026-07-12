# Exercise 6: Panic-recovery HTTP middleware that logs stack, counts, and returns 500

Every technique so far assumes the error travels a `return`. A panic does not: it
unwinds past every `if err != nil`, and if nothing catches it the goroutine — and
often the process — dies. This module builds an `http.Handler` middleware that
recovers panics at the request edge, logs the stack with the request context,
increments a panic counter, and turns the panic into a clean 500 — so a panic
becomes just another observed error instead of an invisible crash.

This module is fully self-contained: its own `go mod init`, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
panicmw/                     independent module: example.com/panicmw
  go.mod                     go 1.25
  recover.go                 Recoverer middleware: defer recover, log debug.Stack, count, http 500
  cmd/
    demo/
      main.go                runnable demo: a panicking route and a healthy route
  recover_test.go            httptest: panic -> 500 + stack logged + counter++; healthy -> pass-through
```

- Files: `recover.go`, `cmd/demo/main.go`, `recover_test.go`.
- Implement: a `Recoverer(logger, counter)` that returns middleware `func(http.Handler) http.Handler`; on a panic it recovers, logs `runtime/debug.Stack` at error level with the request context, increments an `atomic.Int64` panic counter, and writes `http.Error(w, ..., 500)`.
- Test: wrap a panicking handler; drive it with `httptest.NewRecorder`; assert status 500, the log holds the stack and the panic value, the counter is 1; assert a non-panicking handler passes through untouched with the counter at 0.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/14-error-observability/06-panic-recovery-observability/cmd/demo
cd go-solutions/10-error-handling/14-error-observability/06-panic-recovery-observability
go mod edit -go=1.25
```

### Why the boundary must observe, not just recover

A bare `defer func() { recover() }()` stops the crash but hides the bug — the
request returns a blank 500 and no one ever learns a panic happened. The
recover-and-*observe* boundary does four things in its deferred function, in
order:

1. Capture the stack *now*, with `runtime/debug.Stack()`. This must happen inside
   the deferred function during the panic unwind, because that is when the stack
   still points at the panic site; capture it later and it is gone. `debug.Stack`
   returns the current goroutine's stack as bytes.
2. Log at error level through `ErrorContext` with the request's context, so the
   correlation handler (Exercise 3) can stamp the `request_id`/`trace_id` onto the
   panic line — you want to know *which* request panicked. Log the recovered value
   and the stack as attributes.
3. Increment a panic counter (`atomic.Int64`). Panics are a distinct, high-signal
   metric: any nonzero rate is worth an alert, separate from ordinary 5xx.
4. Translate to a 500 with `http.Error`, so the client gets a clean response
   instead of a dropped connection.

The subtle part is *when* you can still write the 500. If the wrapped handler
already wrote a status and some body before panicking, the header is committed and
`http.Error`'s `WriteHeader(500)` is a no-op that logs a "superfluous WriteHeader"
warning — you cannot un-send bytes. That is a real limitation, not a bug to fix
here: the middleware still recovers, logs, and counts; it just cannot always
rewrite an already-started response. The demo and tests panic before any write, so
the 500 lands cleanly, which is the common case.

`recover()` only catches a panic in the *same goroutine*. If the handler spawns
`go doWork()` and that goroutine panics, this middleware cannot see it — that
goroutine needs its own deferred recover. The middleware guards the request
goroutine, which is the edge it owns.

Create `recover.go`:

```go
package panicmw

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync/atomic"
)

// Recoverer returns middleware that turns a panic in the request goroutine into
// an observed 500: it logs the stack with the request context, increments the
// counter, and writes a clean error response.
func Recoverer(logger *slog.Logger, panics *atomic.Int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				panics.Add(1)
				logger.ErrorContext(r.Context(), "panic recovered",
					"panic", fmt.Sprint(rec),
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ctxKey and WithRequestID let a caller correlate the panic line (used by the demo).
type ctxKey int

const requestIDKey ctxKey = 0

// WithRequestID attaches a request id for correlation on the panic log line.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}
```

### The runnable demo

The demo builds a tiny mux with a healthy route and a route that panics, wraps it
in `Recoverer`, and drives both with `httptest` so the output is self-contained.
It prints each route's status and the final panic count; logs go to a discard
buffer so stdout is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"example.com/panicmw"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "healthy")
	})
	mux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})

	var panics atomic.Int64
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) // discard for demo
	h := panicmw.Recoverer(logger, &panics)(mux)

	for _, path := range []string{"/ok", "/boom"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		fmt.Printf("%s -> %d\n", path, rec.Code)
	}
	fmt.Printf("panics=%d\n", panics.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/ok -> 200
/boom -> 500
panics=1
```

### Tests

The tests drive the wrapped handler with `httptest.NewRecorder`. `TestPanicBecomes500`
asserts the status is 500, the counter incremented, and the log buffer holds both
the panic value and a recognizable stack frame. `TestHealthyPassThrough` asserts a
non-panicking handler returns its own status and body with the counter still 0 —
proving the middleware is transparent on the happy path.

Create `recover_test.go`:

```go
package panicmw

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"log/slog"
)

func TestPanicBecomes500(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var panics atomic.Int64
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	h := Recoverer(logger, &panics)(panicky)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if panics.Load() != 1 {
		t.Fatalf("panic counter = %d, want 1", panics.Load())
	}
	logs := buf.String()
	if !strings.Contains(logs, "kaboom") {
		t.Fatalf("log missing panic value; got %q", logs)
	}
	if !strings.Contains(logs, "panic recovered") {
		t.Fatalf("log missing message; got %q", logs)
	}
	// debug.Stack output names this package's frames.
	if !strings.Contains(logs, "panicmw") {
		t.Fatalf("log missing stack trace; got %q", logs)
	}
}

func TestHealthyPassThrough(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var panics atomic.Int64
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "fine")
	})
	h := Recoverer(logger, &panics)(ok)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ok", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418 (pass-through)", rec.Code)
	}
	if rec.Body.String() != "fine" {
		t.Fatalf("body = %q, want fine", rec.Body.String())
	}
	if panics.Load() != 0 {
		t.Fatalf("panic counter = %d, want 0 on healthy path", panics.Load())
	}
	if buf.Len() != 0 {
		t.Fatalf("healthy path logged %q, want nothing", buf.String())
	}
}
```

## Review

The middleware is correct when it is invisible on success and loud on panic.
Invisible: `TestHealthyPassThrough` proves the handler's own 418 and body reach
the client and nothing is logged or counted. Loud: `TestPanicBecomes500` proves a
panic turns into a 500 with the panic value, the message, and a real stack frame
in the log, and the counter at 1. If any of those four is missing, a panic would
be silently swallowed or invisibly crash.

Two limits to keep honest, both stated in the code above. First, `recover` only
catches the *same* goroutine — a panic in a handler-spawned goroutine needs its
own boundary; the process still dies otherwise. Second, if the handler already
started writing the response before panicking, the 500 cannot overwrite the
committed header; the recover, log, and count still happen, but the status is
whatever was already sent. Place this middleware outermost so it wraps everything,
and pair its counter with an alert — any panic rate above zero is worth a look.

## Resources

- [`net/http` middleware pattern](https://pkg.go.dev/net/http#Handler) — the `func(http.Handler) http.Handler` shape and `HandlerFunc`.
- [`runtime/debug.Stack`](https://pkg.go.dev/runtime/debug#Stack) — capturing the current goroutine's stack during a panic.
- [Go blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the recover-in-defer contract and same-goroutine rule.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-otel-span-error-recording.md](07-otel-span-error-recording.md)
