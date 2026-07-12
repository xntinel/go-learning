# Exercise 7: The Middleware That Turned a Panic into a 200

An HTTP recovery middleware wraps the next handler and `recover()`s a panic. It
shipped catching the panic but writing no terminal response, so a handler that
blew up reported success: the recorder's default 200 stood. You will reproduce
with an `httptest` request against a panicking handler, see the 200, and fix the
recover branch to log and write a 500 exactly once.

## What you'll build

```text
recovery/                  module example.com/recovery
  go.mod
  recovery.go              Middleware(logger, next); statusWriter tracks the committed header
  cmd/demo/
    main.go                runnable demo: an httptest server whose handler panics
  recovery_test.go         panic->500, pass-through, no-double-write, Example (all -race)
```

- Files: `recovery.go`, `cmd/demo/main.go`, `recovery_test.go`.
- Implement: `Middleware(logger, next)` that wraps `next` in a `statusWriter` recording whether a header was committed, `recover()`s in a `defer`, logs with `slog`, and writes HTTP 500 exactly once when nothing was written yet.
- Test: a panicking handler yields `500` and a sanitized body (no panic detail); a normal handler passes through unchanged; a handler that commits a header before panicking is not overwritten (no superfluous `WriteHeader`).
- Verify: `go test -count=1 -race ./...`.

### The artifact and the planted bug

The middleware is a panic boundary: a handler that panics should be contained and
reported as a 500, not crash the process and not report success. The version that
shipped recovered the panic and stopped there:

```go
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered", "err", rec)
				// BUG: recovered, but no terminal response is written.
				// The ResponseRecorder's default status (200) stands, so the
				// caller sees success for a request that panicked.
			}
		}()
		next.ServeHTTP(w, r)
	})
}
```

The `recover()` runs, but the branch writes nothing. An `http.ResponseWriter` that
was never written defaults to `200 OK`, so the client — and any monitoring keyed
off status — records a success for a request that blew up. The incident is hidden,
not contained. This passes review because the recover *is* there; the missing part
is that a recover must fully own the response after it fires.

The failing test reads:

```text
--- FAIL: TestRecoversPanicAs500 (0.00s)
    recovery_test.go:39: Code = 200, want 500
```

The fix wraps the `ResponseWriter` in a small `statusWriter` that records whether
a status line was committed, then in the recover branch writes a 500 and a
sanitized body *only if nothing was written yet*. Guarding on "already committed"
is what prevents a superfluous second `WriteHeader` when a handler had already
streamed a status before panicking.

Create `recovery.go`:

```go
package recovery

import (
	"log/slog"
	"net/http"
)

// statusWriter wraps an http.ResponseWriter and records whether the status line
// has been committed, so the recover branch can avoid a superfluous WriteHeader.
type statusWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.WriteHeader(http.StatusOK)
	}
	return s.ResponseWriter.Write(b)
}

// Middleware wraps next so that a panic is recovered, logged, and turned into a
// 500 written exactly once. If the handler already committed a status before
// panicking, the committed status is left intact (it cannot be un-sent).
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic recovered", "err", rec, "method", r.Method, "path", r.URL.Path)
				if !sw.wroteHeader {
					sw.WriteHeader(http.StatusInternalServerError)
					_, _ = sw.Write([]byte("internal server error\n"))
				}
			}
		}()
		next.ServeHTTP(sw, r)
	})
}
```

The recover branch now fully owns the outcome: it logs the panic with request
context, and if the handler had not yet committed a status it synthesizes a 500
with a sanitized body — never the panic detail, which could leak internals. If the
handler *had* already sent a status (a streamed response that panicked mid-body),
the `statusWriter` guard skips the write instead of emitting a superfluous
`WriteHeader` that the net/http server would log as an error.

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

	"example.com/recovery"
)

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("simulated handler failure")
	})

	srv := httptest.NewServer(recovery.Middleware(logger, panicky))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/orders/1")
	if err != nil {
		fmt.Println("request error:", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("status=%d body=%q\n", resp.StatusCode, string(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
status=500 body="internal server error\n"
```

### Tests

`TestRecoversPanicAs500` is the reproducer: request a handler that panics and
assert `rec.Code == 500` with a sanitized body. `TestPassesThroughNonPanic`
asserts a normal handler's status and body are untouched.
`TestDoesNotOverwriteCommittedStatus` sends a handler that commits a 200 and then
panics, and asserts the recover branch does not overwrite it — the guard against a
double `WriteHeader`.

Create `recovery_test.go`:

```go
package recovery

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRecoversPanicAs500(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom: nil map write")
	})
	mw := Middleware(discardLogger(), h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("Code = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("body leaked panic detail: %q", rec.Body.String())
	}
	if got := rec.Body.String(); got != "internal server error\n" {
		t.Fatalf("body = %q, want the sanitized message", got)
	}
}

func TestPassesThroughNonPanic(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})
	mw := Middleware(discardLogger(), h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	mw.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("Code = %d, want 201", rec.Code)
	}
	if rec.Body.String() != "created" {
		t.Fatalf("body = %q, want \"created\"", rec.Body.String())
	}
}

func TestDoesNotOverwriteCommittedStatus(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom after the header was committed")
	})
	mw := Middleware(discardLogger(), h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	mw.ServeHTTP(rec, req)

	// The status was already committed; recover must not attempt a second
	// WriteHeader (which net/http would log as superfluous).
	if rec.Code != http.StatusOK {
		t.Fatalf("Code = %d, want 200 (committed status must be left intact)", rec.Code)
	}
}

func ExampleMiddleware() {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})
	mw := Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), h)

	rec := httptest.NewRecorder()
	mw.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println(rec.Code)
	// Output: 500
}
```

## Review

The middleware is correct when a panicking handler yields a 500 with a sanitized
body, a normal handler passes through untouched, and a handler that already
committed a status is not overwritten. The recover branch must own the control
flow after it fires: swallowing the panic and letting the default 200 stand
reports success for a failed request, which hides the incident from every alert
keyed off status. Writing the 500 only when nothing was committed, tracked by the
`statusWriter`, keeps the middleware from emitting a superfluous second header on a
streamed response. Never write the panic value into the body — log it with context
and return a generic message. Run under `-race`, since middleware executes on every
request goroutine.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — recovering at a stable boundary.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for exercising handlers.
- [log/slog](https://pkg.go.dev/log/slog) — structured logging of the recovered panic.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-worker-select-cancel-leak.md](06-worker-select-cancel-leak.md) | Next: [08-labeled-break-batch-scan.md](08-labeled-break-batch-scan.md)
