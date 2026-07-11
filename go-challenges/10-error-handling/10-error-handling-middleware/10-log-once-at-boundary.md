# Exercise 10: Log errors once at the boundary with a request-scoped logger

Duplicate, inconsistently-shaped log lines are the tax of logging errors wherever
they happen. This exercise fixes it: a `RequestID` middleware injects a correlation
id and a request-scoped `slog.Logger` into the context, the error boundary logs
the full error *exactly once* (with id, method, path, status), and handlers only
return errors — they never log.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
logonce/                     independent module: example.com/logonce
  go.mod                     go 1.26
  logonce.go                 RequestID mw; ctx logger; WithError boundary logs once; sentinels
  cmd/
    demo/
      main.go                runnable demo: one failing request, one log line with the id
  logonce_test.go            exactly one log line per failure; id in header and log; nested-mw no double log
```

Files: `logonce.go`, `cmd/demo/main.go`, `logonce_test.go`.
Implement: a `RequestID` middleware that mints/adopts a correlation id, stores a
`slog.With(...)` logger in the context, and echoes the id in a header; a
`WithError` boundary that logs the full error once with the request-scoped logger;
handlers that only return errors.
Test: drive one failing request through a `bytes.Buffer`-backed slog handler and
assert exactly one error line carrying the id that also appears in the response
header; a nested-middleware path asserts no double logging.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/logonce/cmd/demo
cd ~/go-exercises/logonce
go mod init example.com/logonce
```

### Why once, and why at the boundary

The most common logging antipattern in a service is logging an error where it
occurs *and* returning it. The repository logs "user not found", the service layer
logs "load user failed", the handler logs "request failed", and the boundary logs
it again — four lines for one failure, each with a different shape, none carrying
the request's correlation id, and all interleaved with other requests' lines so
you cannot tell which belong together. The discipline that fixes this is simple to
state and requires enforcing: **handlers and lower layers return errors and never
log them; the boundary logs each failed request exactly once.**

For that single line to be useful it needs request-scoped context: a correlation
id, the method, the path, and the mapped status. The `RequestID` middleware
establishes this. On the way in it reads an inbound `X-Request-Id` (so a
correlation id assigned by a gateway or client is preserved) or mints a fresh one,
builds a logger with that id already bound via `slog.With("request_id", id)`,
stores *both* the id and the logger in the request context, and echoes the id in
the response header. Everything downstream — including the boundary — pulls the
logger out of the context, so every line it emits automatically carries the id.
`slog.With` returns a logger that prepends the given attributes to every record, so
you bind the id once and never repeat it.

The boundary (`WithError`) is where the one log line is written. It runs the
handler; on a non-nil error it (a) pulls the request-scoped logger from the
context, (b) maps the error to a status, (c) logs the *full* error once at that
level with the status attached, and (d) writes the client response (generic for
5xx, per Exercise 5). Because the handler returned rather than logged, this is the
only line for the failure, and it carries the id the client also received — so an
operator can join the client-reported id straight to the server-side detail.

Middleware order makes this work: `RequestID` must run *before* the boundary and
any logging middleware so the id and logger are in the context when they run;
`Recoverer` stays outermost. The order is a deliberate, testable stack, which is
why the test asserts a single line even when the request passes through an extra
nested middleware.

Create `logonce.go`:

```go
package logonce

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrInvalidInput = errors.New("invalid input")
)

type ctxKey int

const (
	loggerKey ctxKey = iota
	requestIDKey
)

const requestIDHeader = "X-Request-Id"

// Logger pulls the request-scoped logger from the context, falling back to the
// default so it is never nil.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// RequestID adopts an inbound X-Request-Id or mints one, binds it to a
// request-scoped logger, stores both in the context, and echoes it in a header.
func RequestID(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			if id == "" {
				var b [8]byte
				_, _ = rand.Read(b[:])
				id = hex.EncodeToString(b[:])
			}
			logger := base.With("request_id", id)
			ctx := context.WithValue(r.Context(), loggerKey, logger)
			ctx = context.WithValue(ctx, requestIDKey, id)
			w.Header().Set(requestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Handler returns an error instead of writing its own failure response, and
// never logs.
type Handler func(w http.ResponseWriter, r *http.Request) error

// WithError is the boundary: it runs the handler and, on error, logs exactly
// once with the request-scoped logger, then writes the response.
func WithError(h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		status := statusFor(err)
		// The single, authoritative log line for this failed request.
		Logger(r.Context()).ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "status", status, "err", err)

		clientMsg := err.Error()
		if status >= 500 {
			clientMsg = "internal error"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": clientMsg})
	})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
```

### The runnable demo

The demo installs a JSON `slog` handler on stdout with the timestamp stripped for
determinism, wraps a failing handler in `RequestID`, sends a request with a fixed
inbound id, and shows the single log line carrying that id plus the response
header echoing it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	"log/slog"

	"example.com/logonce"
)

func main() {
	opts := &slog.HandlerOptions{
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}
	base := slog.New(slog.NewJSONHandler(os.Stdout, opts))

	failing := logonce.WithError(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("load user 7: %w", logonce.ErrNotFound)
	})
	handler := logonce.RequestID(base)(failing)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/user", nil)
	req.Header.Set("X-Request-Id", "req-42")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		panic(err)
	}
	resp.Body.Close()

	fmt.Println("response header X-Request-Id:", resp.Header.Get("X-Request-Id"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"ERROR","msg":"request failed","request_id":"req-42","method":"GET","path":"/user","status":404,"err":"load user 7: not found"}
response header X-Request-Id: req-42
```

One log line, carrying `req-42`, the same id the client got back in the header.

### Tests

`TestLogsExactlyOnce` backs the base logger with a `bytes.Buffer`, drives one
failing request, and asserts the buffer holds exactly one line, that it carries the
inbound id, and that the response header echoes the same id.
`TestNestedMiddlewareStillLogsOnce` inserts an extra pass-through middleware
between `RequestID` and the boundary and asserts the count is still one — proving
the log-once discipline is a property of *where* logging happens, not of the stack
depth. Both run under `-race`.

Create `logonce_test.go`:

```go
package logonce

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"log/slog"
)

func drive(t *testing.T, buf *bytes.Buffer, inboundID string, h http.Handler) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/user", nil)
	if inboundID != "" {
		req.Header.Set(requestIDHeader, inboundID)
	}
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func countLines(buf *bytes.Buffer) int {
	n := 0
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

func TestLogsExactlyOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))

	failing := WithError(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("load user 7: %w", ErrNotFound)
	})
	handler := RequestID(base)(failing)

	resp := drive(t, &buf, "req-42", handler)

	if got := countLines(&buf); got != 1 {
		t.Fatalf("log lines = %d, want exactly 1\n%s", got, buf.String())
	}

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("log line is not valid json: %v", err)
	}
	if rec["request_id"] != "req-42" {
		t.Fatalf("log request_id = %v, want req-42", rec["request_id"])
	}
	if rec["status"].(float64) != http.StatusNotFound {
		t.Fatalf("log status = %v, want 404", rec["status"])
	}
	if resp.Header.Get(requestIDHeader) != "req-42" {
		t.Fatalf("response header id = %q, want req-42", resp.Header.Get(requestIDHeader))
	}
}

func TestNestedMiddlewareStillLogsOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))

	// A pass-through middleware between RequestID and the boundary; it must not
	// add its own error log.
	passthrough := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
		})
	}

	failing := WithError(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("validate: %w", ErrInvalidInput)
	})
	handler := RequestID(base)(passthrough(failing))

	drive(t, &buf, "", handler) // no inbound id: one is minted

	if got := countLines(&buf); got != 1 {
		t.Fatalf("log lines = %d, want exactly 1\n%s", got, buf.String())
	}
}

func ExampleLogger() {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, nil))
	h := RequestID(base)(WithError(func(w http.ResponseWriter, r *http.Request) error {
		return ErrNotFound
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, "abc")
	h.ServeHTTP(rec, req)
	fmt.Println(rec.Header().Get(requestIDHeader), rec.Code)
	// Output: abc 404
}
```

## Review

The discipline this module pins is one failure, one line, carrying the correlation
id the client also holds. `TestLogsExactlyOnce` asserts all three: the count is
exactly one, the line carries the inbound id, and the response header echoes it —
so a client's reported id joins directly to the server log.
`TestNestedMiddlewareStillLogsOnce` proves the count does not grow with stack depth,
because logging lives only at the boundary. The bug this prevents is the four-line
smear where every layer logs the same failure differently and none carry the id;
the cure is a rule you enforce in review — lower layers `return`, they do not log.
`slog.With` binds the id once so every boundary line inherits it. Run `-race`: the
context-stored logger is read on the request goroutine, and a real stack would have
`Recoverer` outermost and `RequestID` before the boundary so the id is present when
both run.

## Resources

- [`log/slog#Logger.With`](https://pkg.go.dev/log/slog#Logger.With) — binding the correlation id once for every subsequent line.
- [`log/slog#Logger.ErrorContext`](https://pkg.go.dev/log/slog#Logger.ErrorContext) — the single context-aware error line at the boundary.
- [`context#WithValue`](https://pkg.go.dev/context#WithValue) — carrying the request-scoped logger and id.
- [Go blog: Structured Logging with slog](https://go.dev/blog/slog) — request-scoped loggers and attributes.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../11-structured-error-types/00-concepts.md](../11-structured-error-types/00-concepts.md)
