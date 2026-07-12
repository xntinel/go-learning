# Exercise 8: HTTP Handler-Returns-Error Adapter via Named Return

Standard `http.Handler` cannot return an error, so every handler ends up repeating
`http.Error(w, ..., 500); return` at each failure. The idiomatic fix is to let
business handlers have the signature `func(w, r) error` and put the
error-to-status mapping and logging in one adapter — a deferred closure that reads
the handler's outcome, exactly the named-return pattern applied at the HTTP
boundary.

This module is self-contained: its own `go mod init`, its own demo, its own tests.

## What you'll build

```text
handlererr/                 independent module: example.com/handlererr
  go.mod
  handlererr.go             Handler; ValidationError; Adapt (one map+log defer)
  cmd/demo/
    main.go                 runnable demo: drive handlers via httptest, print codes
  handlererr_test.go        nil->200, not-found->404, validation->400, other->500, one log line
```

- Files: `handlererr.go`, `cmd/demo/main.go`, `handlererr_test.go`.
- Implement: `Handler func(w, r) error`; `Adapt(logger, h) http.Handler` whose deferred closure maps the handler's error to a status (NotFound->404, `*ValidationError`->400, else 500) and logs method+path+status once.
- Test: `httptest` recorder/request; nil->200, a NotFound sentinel->404, a `*ValidationError` (via `errors.As`)->400, an arbitrary error->500; assert body and exactly one log line via an injected logger.
- Verify: `go test -count=1 -race ./...`

### One boundary, one map, one log line

An `http.HandlerFunc` returns nothing, so the adapter captures the business
handler's error in a local variable and reads it in a deferred closure. That local
plays exactly the role a named return plays elsewhere: it is set by the work, then
read by the defer on the way out.

```go
func Adapt(logger *slog.Logger, h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		defer func() {
			status := statusFor(err)
			if err != nil {
				http.Error(w, http.StatusText(status), status)
			}
			logger.Info("request",
				"method", r.Method, "path", r.URL.Path, "status", status)
		}()
		err = h(w, r)
	})
}
```

The deferred closure runs once, on every exit, and does three things in one place:
compute the status from the final error, write an error response if there was an
error, and log the method, path, and status. Centralizing it means a handler just
returns an error and never touches status codes or logging — and the day you add a
new error kind, you extend `statusFor` once rather than editing every handler.

`statusFor` is where `errors.Is` and `errors.As` do the mapping. A `NotFound`
sentinel maps to 404 via `errors.Is`; a `*ValidationError` maps to 400 via
`errors.As` (which reaches through wrapping to find the concrete type); everything
else is a 500. A successful handler returns nil, `statusFor(nil)` is 200, and the
defer writes no error body — the handler has already written its own response.

One honesty note about ordering: on the error path the handler must not have
written a body, or `http.Error`'s header write in the defer would be a
"superfluous WriteHeader" no-op. The convention is that an error-returning handler
returns *before* writing anything on failure; the adapter owns the failure
response. That is the whole point of the pattern.

Create `handlererr.go`:

```go
package handlererr

import (
	"errors"
	"log/slog"
	"net/http"
)

// ErrNotFound is a sentinel a handler returns for a missing resource.
var ErrNotFound = errors.New("not found")

// ValidationError is a typed error a handler returns for bad input. errors.As
// recovers it through wrapping so the adapter can map it to 400.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return "validation: " + e.Field + ": " + e.Msg
}

// Handler is a business handler that may return an error instead of writing a
// status code itself.
type Handler func(w http.ResponseWriter, r *http.Request) error

// statusFor maps a handler error to an HTTP status code.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		var ve *ValidationError
		if errors.As(err, &ve) {
			return http.StatusBadRequest
		}
		return http.StatusInternalServerError
	}
}

// Adapt turns an error-returning Handler into a standard http.Handler. A single
// deferred closure reads the handler's error, maps it to a status, writes the
// error response when needed, and logs the outcome exactly once at the boundary.
func Adapt(logger *slog.Logger, h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		defer func() {
			status := statusFor(err)
			if err != nil {
				http.Error(w, http.StatusText(status), status)
			}
			logger.Info("request",
				"method", r.Method, "path", r.URL.Path, "status", status)
		}()
		err = h(w, r)
	})
}
```

### The runnable demo

The demo wires four handlers, drives each with an in-memory `httptest` request (no
socket), and prints the resulting status code.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"example.com/handlererr"
)

func call(h http.Handler) int {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	h.ServeHTTP(rec, req)
	return rec.Code
}

func main() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ok := handlererr.Adapt(logger, func(w http.ResponseWriter, _ *http.Request) error {
		fmt.Fprint(w, "hello")
		return nil
	})
	missing := handlererr.Adapt(logger, func(http.ResponseWriter, *http.Request) error {
		return handlererr.ErrNotFound
	})
	invalid := handlererr.Adapt(logger, func(http.ResponseWriter, *http.Request) error {
		return &handlererr.ValidationError{Field: "email", Msg: "required"}
	})
	broken := handlererr.Adapt(logger, func(http.ResponseWriter, *http.Request) error {
		return fmt.Errorf("db down")
	})

	fmt.Println("ok:", call(ok))
	fmt.Println("missing:", call(missing))
	fmt.Println("invalid:", call(invalid))
	fmt.Println("broken:", call(broken))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ok: 200
missing: 404
invalid: 400
broken: 500
```

### Tests

The tests drive the adapter with `httptest` and assert the status code, the body,
and that exactly one log line is emitted. The logger writes to a `bytes.Buffer` so
the test can count lines.

Create `handlererr_test.go`:

```go
package handlererr

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func drive(t *testing.T, h Handler) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	Adapt(logger, h).ServeHTTP(rec, req)

	lines := strings.Count(strings.TrimSpace(logs.String()), "\n") + 1
	if strings.TrimSpace(logs.String()) == "" {
		lines = 0
	}
	if lines != 1 {
		t.Fatalf("emitted %d log lines, want exactly 1", lines)
	}
	return rec, logs.String()
}

func TestAdaptSuccess(t *testing.T) {
	t.Parallel()

	rec, _ := drive(t, func(w http.ResponseWriter, _ *http.Request) error {
		fmt.Fprint(w, "hello")
		return nil
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q, want hello", rec.Body.String())
	}
}

func TestAdaptNotFound(t *testing.T) {
	t.Parallel()

	rec, _ := drive(t, func(http.ResponseWriter, *http.Request) error {
		return ErrNotFound
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAdaptValidation(t *testing.T) {
	t.Parallel()

	rec, _ := drive(t, func(http.ResponseWriter, *http.Request) error {
		return fmt.Errorf("bad request: %w", &ValidationError{Field: "email", Msg: "required"})
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (errors.As should reach the wrapped *ValidationError)", rec.Code)
	}
}

func TestAdaptInternalError(t *testing.T) {
	t.Parallel()

	rec, _ := drive(t, func(http.ResponseWriter, *http.Request) error {
		return fmt.Errorf("db down")
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAdaptLogsStatus(t *testing.T) {
	t.Parallel()

	_, logLine := drive(t, func(http.ResponseWriter, *http.Request) error {
		return ErrNotFound
	})
	if !strings.Contains(logLine, "status=404") {
		t.Fatalf("log %q missing status=404", logLine)
	}
}

func ExampleAdapt() {
	logger := slog.New(slog.NewTextHandler(bytes.NewBuffer(nil), nil))
	h := Adapt(logger, func(http.ResponseWriter, *http.Request) error {
		return ErrNotFound
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println(rec.Code)
	// Output: 404
}
```

## Review

The adapter is correct when each error kind maps to its status (nil->200,
`ErrNotFound`->404, `*ValidationError`->400, anything else->500), the error body is
written by the adapter and not the handler, and exactly one log line is emitted per
request. The `errors.As` mapping is what makes the 400 case robust: the
`TestAdaptValidation` case returns a *wrapped* `*ValidationError`, and `errors.As`
still reaches it — `errors.Is` on a concrete type would not, which is the usual
mistake. The subtle discipline is response ownership: a failing handler must return
before writing a body so the adapter can set the status; a handler that writes then
returns an error produces a "superfluous WriteHeader" and a wrong status. Run
`go test -race`.

## Resources

- [`net/http.HandlerFunc`](https://pkg.go.dev/net/http#HandlerFunc)
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest)
- [`errors.As`](https://pkg.go.dev/errors#As)
- [`log/slog`](https://pkg.go.dev/log/slog)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-shadowed-named-return-bug.md](07-shadowed-named-return-bug.md) | Next: [09-result-struct-over-positional.md](09-result-struct-over-positional.md)
