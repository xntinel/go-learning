# Exercise 3: http.ErrAbortHandler — Recover Without Turning an Abort Into a 500

Not every panic that reaches your recovery middleware is a server error. When a
streaming handler gives up on a half-written response — a client that
disconnected, an upstream that died mid-stream — the idiomatic way to abort is
`panic(http.ErrAbortHandler)`, a sentinel that also tells `net/http` to suppress
its stack-trace log. A recovery middleware that treats that like any other panic
turns a deliberate, expected abort into a fake 500 and an error-level stack in
your logs. This module builds a middleware that special-cases the sentinel: on an
abort it logs at debug level and re-panics so `net/http` performs its own silent
abort; every other panic follows the normal 500 path.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
abortmw/                   independent module: example.com/abortmw
  go.mod                   go 1.26
  abortmw.go               Middleware(next, logger): abort -> re-panic; else 500
  cmd/
    demo/
      main.go              runnable demo counting error- vs debug-level logs
  abortmw_test.go          slog sink: abort emits no 500/error; normal panic does
```

Files: `abortmw.go`, `cmd/demo/main.go`, `abortmw_test.go`.
Implement: `Middleware(next http.Handler, logger *slog.Logger) http.Handler` that detects `http.ErrAbortHandler` with `errors.Is`, logs it at `Debug` and re-panics, and sends a 500 with an `Error`-level stack for anything else.
Test: a slog capture sink; assert an aborting handler produces no 500 body and no `Error`-level log, a normal panic produces a 500 and one `Error` log, and a wrapped abort (`fmt.Errorf("...: %w", http.ErrAbortHandler)`) is still detected via `errors.Is`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/03-abort-handler-sentinel/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/03-abort-handler-sentinel
```

### Why re-panic instead of return

`http.ErrAbortHandler` is defined as `var ErrAbortHandler = errors.New("net/http: abort Handler")`. Panicking with it does two things: it aborts the
response to the client (like any panic from `ServeHTTP`), and it *suppresses*
`net/http`'s stack-trace log for that specific panic. The runtime and `net/http`
have already agreed this is a deliberate abort, not a crash. Your middleware's job
is to not undo that agreement.

The recover path first checks whether the recovered value is an error that
`errors.Is` matches to `http.ErrAbortHandler`. Using `errors.Is` rather than `==`
matters: a handler may wrap the sentinel (`fmt.Errorf("upstream vanished: %w", http.ErrAbortHandler)`) to add context, and the wrapped value must still be
recognized as an abort. When it matches, the middleware logs at *debug* level (an
abort is routine, not an error) and re-panics the original value. Re-panicking
hands the abort back to `net/http`'s own recover, which performs the silent abort
it was designed for. If instead you `return` here, the response is left in an
inconsistent half-written state without the connection being properly aborted, and
you have swallowed a signal the HTTP stack was relying on.

Every other panic is a genuine server error: capture the stack with
`debug.Stack()` immediately, log at `Error` level, and write the standard 500.
The two paths are mutually exclusive and the classification is a single
`errors.Is` check — that is the whole discipline.

Create `abortmw.go`:

```go
package abortmw

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Middleware recovers panics from next. An http.ErrAbortHandler panic (possibly
// wrapped) is logged at debug and re-panicked so net/http performs its own
// silent abort; every other panic becomes a 500 with an error-level stack log.
func Middleware(next http.Handler, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				logger.Debug("handler aborted response", "path", r.URL.Path)
				panic(rec) // let net/http perform its designed silent abort
			}
			stack := debug.Stack()
			logger.Error("handler panic", "path", r.URL.Path, "value", rec, "stack", string(stack))
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}()
		next.ServeHTTP(w, r)
	})
}
```

### The runnable demo

The demo captures logs into an in-memory sink so its output is deterministic. It
drives two requests through a real `httptest` server: `/stream` flushes a few SSE
bytes and then aborts with `http.ErrAbortHandler`, while `/boom` panics with a
plain error. After both, it prints how many logs landed at each level — one debug
(the abort), one error (the real panic).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"

	"example.com/abortmw"
)

type sink struct {
	mu      sync.Mutex
	records []slog.Record
}

func (s *sink) Enabled(context.Context, slog.Level) bool { return true }
func (s *sink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	s.records = append(s.records, r.Clone())
	s.mu.Unlock()
	return nil
}
func (s *sink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *sink) WithGroup(string) slog.Handler      { return s }
func (s *sink) count(level slog.Level) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.records {
		if r.Level == level {
			n++
		}
	}
	return n
}

func main() {
	s := &sink{}
	logger := slog.New(s)

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic(fmt.Errorf("database connection lost"))
	})

	srv := httptest.NewServer(abortmw.Middleware(mux, logger))
	defer srv.Close()

	for _, path := range []string{"/stream", "/boom"} {
		resp, err := http.Get(srv.URL + path)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	fmt.Printf("error-level logs: %d\n", s.count(slog.LevelError))
	fmt.Printf("debug-level logs: %d\n", s.count(slog.LevelDebug))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
error-level logs: 1
debug-level logs: 1
```

### Tests

The tests capture `slog` output through a test sink so the assertions are on what
the middleware *decided*, not on racy client-side reads of an aborted connection.
`TestAbortIsNotA500` drives an aborting handler and asserts no `Error`-level log
and no 500 status. `TestNormalPanicIsA500` drives a plain panic and asserts a 500
plus exactly one `Error`-level log. `TestWrappedAbortDetected` wraps the sentinel
with `%w` and asserts it is still recognized as an abort (proving `errors.Is`, not
`==`).

Create `abortmw_test.go`:

```go
package abortmw

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type sink struct {
	mu      sync.Mutex
	records []slog.Record
}

func (s *sink) Enabled(context.Context, slog.Level) bool { return true }
func (s *sink) Handle(_ context.Context, r slog.Record) error {
	s.mu.Lock()
	s.records = append(s.records, r.Clone())
	s.mu.Unlock()
	return nil
}
func (s *sink) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *sink) WithGroup(string) slog.Handler      { return s }
func (s *sink) count(level slog.Level) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.records {
		if r.Level == level {
			n++
		}
	}
	return n
}

func drive(t *testing.T, h http.Handler) (status int, gotResponse bool) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL)
	if err != nil {
		return 0, false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, true
}

func TestAbortIsNotA500(t *testing.T) {
	t.Parallel()

	s := &sink{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: partial\n\n")
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	})

	status, got := drive(t, Middleware(h, slog.New(s)))

	if s.count(slog.LevelError) != 0 {
		t.Fatalf("error-level logs = %d, want 0 for an abort", s.count(slog.LevelError))
	}
	if s.count(slog.LevelDebug) == 0 {
		t.Fatal("expected a debug-level log for the abort")
	}
	if got && status == http.StatusInternalServerError {
		t.Fatalf("abort produced a 500; it must not")
	}
}

func TestNormalPanicIsA500(t *testing.T) {
	t.Parallel()

	s := &sink{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(fmt.Errorf("boom"))
	})

	status, got := drive(t, Middleware(h, slog.New(s)))

	if !got || status != http.StatusInternalServerError {
		t.Fatalf("status = %d (got=%v), want 500", status, got)
	}
	if s.count(slog.LevelError) != 1 {
		t.Fatalf("error-level logs = %d, want 1", s.count(slog.LevelError))
	}
}

func TestWrappedAbortDetected(t *testing.T) {
	t.Parallel()

	s := &sink{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		panic(fmt.Errorf("upstream vanished: %w", http.ErrAbortHandler))
	})

	status, got := drive(t, Middleware(h, slog.New(s)))

	if s.count(slog.LevelError) != 0 {
		t.Fatalf("wrapped abort logged at error level %d times; errors.Is should catch it", s.count(slog.LevelError))
	}
	if got && status == http.StatusInternalServerError {
		t.Fatalf("wrapped abort produced a 500; errors.Is should have detected it")
	}
}
```

## Review

The middleware is correct when the abort path and the server-error path never
overlap: `errors.Is(err, http.ErrAbortHandler)` selects the abort, which logs at
debug and re-panics, and everything else logs at error and writes a 500. The
tests assert on the captured `slog` records rather than on the client's view of an
aborted connection, because a re-panicked abort closes the connection and the
client-side read is inherently racy — the deterministic truth is what level the
middleware logged at and whether it wrote a 500. The trap this closes is the most
common streaming bug: treating `http.ErrAbortHandler` as a generic panic, so your
error rate and logs fill with 500s that were really deliberate client aborts. Note
the detection is `errors.Is`, so a wrapped abort still routes correctly.

## Resources

- [net/http.ErrAbortHandler](https://pkg.go.dev/net/http#ErrAbortHandler) — the sentinel and its log-suppression behavior.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a possibly-wrapped sentinel.
- [http.Flusher](https://pkg.go.dev/net/http#Flusher) — flushing a streaming response before the abort.
- [log/slog Levels](https://pkg.go.dev/log/slog#Level) — routing an abort to debug and a real panic to error.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-classify-runtime-panics.md](04-classify-runtime-panics.md)
