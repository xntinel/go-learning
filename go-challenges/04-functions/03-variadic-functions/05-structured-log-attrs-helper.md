# Exercise 5: Structured-Logging Helper Wrapping slog's Variadic Key-Value Args

`slog`'s core ergonomic is a single `...any` carrying alternating key-value pairs:
`logger.Info("msg", "key1", v1, "key2", v2)`. You build a request-scoped helper
around it — `WithFields` to bind fields, `Log` as a level shim, and an HTTP
middleware that attaches `request_id`, `method`, and `status` — and you make the
`!BADKEY` failure mode concrete by asserting it in a test.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
reqlog/                    independent module: example.com/reqlog
  go.mod                   go 1.25
  reqlog.go                WithFields, Log, Middleware, LogRequests
  cmd/
    demo/
      main.go              runnable demo: structured lines, incl. an odd-arg line
  reqlog_test.go           JSON-capture tests: paired fields, !BADKEY, middleware
```

- Files: `reqlog.go`, `cmd/demo/main.go`, `reqlog_test.go`.
- Implement: `WithFields(l *slog.Logger, kv ...any) *slog.Logger` delegating to `l.With(kv...)`; `Log(l, level, msg, kv...)`; a `LogRequests` middleware capturing status.
- Test: capture JSON over a `bytes.Buffer`; paired args become fields; an odd trailing arg emits under `!BADKEY`; the middleware logs `request_id`/`method`/`status`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/03-variadic-functions/05-structured-log-attrs-helper/cmd/demo
cd go-solutions/04-functions/03-variadic-functions/05-structured-log-attrs-helper
go mod edit -go=1.25
```

### The key-value `...any` convention and its silent failure

`slog.Logger.With(args ...any)` and the `Info`/`Warn`/`Log` methods all take the
same `...any` of alternating keys and values. `WithFields` is a one-line pass-
through — `l.With(kv...)` — that exists to give call sites a named, discoverable
helper and a single spot to later inject defaults. `Log` is a thin level shim over
`l.Log(ctx, level, msg, kv...)` so a caller can pass the level as data. Both splat
their `kv` straight through; since `slog` only reads the arguments, the aliasing is
harmless.

The failure mode you must internalize is what happens on *odd* arity. When the
pairs do not balance — a lone trailing argument — `slog` does not panic or error.
It assigns the orphan to the reserved key `!BADKEY`:

```
WithFields(l, "a", 1, "orphan").Info("odd")
// {"level":"INFO","msg":"odd","a":1,"!BADKEY":"orphan"}
```

A malformed log line still emits, just with a garbage key, so a dropped value can
hide in production for weeks. Two defenses: assert the behavior once (the test
below does) so you recognize `!BADKEY` on sight, and enable `go vet`'s slog
analyzer, which flags a mismatched or odd key-value list at build time on calls it
recognizes (the direct `slog.Logger.With`/`Info` calls). Because vet keys on the
function name, it does not see through the `WithFields`/`Log` wrappers — which is
the general lesson about `...any`: wrapping it moves the check from the compiler
and vet onto your tests.

The middleware is the payoff: `LogRequests` wraps a handler, captures the response
status through a small `http.ResponseWriter` shim, and emits one structured line
per request with the request id (from `X-Request-Id`), method, and status. This is
the exact shape every Go web service uses.

Create `reqlog.go`:

```go
// reqlog.go
package reqlog

import (
	"context"
	"log/slog"
	"net/http"
)

// WithFields returns a logger that includes the given key-value pairs on every
// line. It delegates to slog's variadic With; keys and values must alternate.
func WithFields(l *slog.Logger, kv ...any) *slog.Logger {
	return l.With(kv...)
}

// Log emits msg at level with the given key-value pairs.
func Log(l *slog.Logger, level slog.Level, msg string, kv ...any) {
	l.Log(context.Background(), level, msg, kv...)
}

// Middleware is the standard net/http decorator shape.
type Middleware func(http.Handler) http.Handler

// statusRecorder captures the status code written by a handler; net/http does
// not expose it after the fact.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// LogRequests returns middleware that logs one structured line per request with
// request_id, method, and status.
func LogRequests(l *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			Log(l, slog.LevelInfo, "request",
				"request_id", r.Header.Get("X-Request-Id"),
				"method", r.Method,
				"status", rec.status,
			)
		})
	}
}
```

### The runnable demo

The demo strips the `time` attribute (via `ReplaceAttr`) so its output is
deterministic and reproducible; production logs keep the timestamp.

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"

	"example.com/reqlog"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	reqLogger := reqlog.WithFields(logger, "request_id", "req-42", "service", "orders")
	reqLogger.Info("handling request", "method", "GET", "path", "/orders")

	reqlog.Log(logger, slog.LevelWarn, "cache miss", "key", "orders:42")

	// An odd trailing arg lands under the reserved key "!BADKEY".
	reqlog.Log(logger, slog.LevelInfo, "malformed", "only_key")

	h := reqlog.LogRequests(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req.Header.Set("X-Request-Id", "req-99")
	h.ServeHTTP(httptest.NewRecorder(), req)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
{"level":"INFO","msg":"handling request","request_id":"req-42","service":"orders","method":"GET","path":"/orders"}
{"level":"WARN","msg":"cache miss","key":"orders:42"}
{"level":"INFO","msg":"malformed","!BADKEY":"only_key"}
{"level":"INFO","msg":"request","request_id":"req-99","method":"POST","status":201}
```

### Tests

Capturing over a `bytes.Buffer` with a JSON handler makes the log line an
assertable value. `TestOddArgBecomesBadKey` is the important one: it proves the
silent-degradation behavior so you never rely on `slog` to catch a mismatch for
you.

Create `reqlog_test.go`:

```go
// reqlog_test.go
package reqlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newBufLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	return slog.New(h), &buf
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	return m
}

func TestWithFieldsPairsBecomeFields(t *testing.T) {
	t.Parallel()

	l, buf := newBufLogger()
	WithFields(l, "request_id", "abc", "method", "GET").Info("handled")

	m := decode(t, buf)
	if m["request_id"] != "abc" {
		t.Errorf("request_id = %v, want abc", m["request_id"])
	}
	if m["method"] != "GET" {
		t.Errorf("method = %v, want GET", m["method"])
	}
	if m["msg"] != "handled" {
		t.Errorf("msg = %v, want handled", m["msg"])
	}
}

func TestOddArgBecomesBadKey(t *testing.T) {
	t.Parallel()

	l, buf := newBufLogger()
	WithFields(l, "a", 1, "orphan").Info("odd")

	m := decode(t, buf)
	if m["!BADKEY"] != "orphan" {
		t.Fatalf("expected orphan under !BADKEY, got %v", m["!BADKEY"])
	}
	if m["a"] != float64(1) {
		t.Errorf("a = %v, want 1", m["a"])
	}
}

func TestLogShim(t *testing.T) {
	t.Parallel()

	l, buf := newBufLogger()
	Log(l, slog.LevelWarn, "cache miss", "key", "orders:42")

	m := decode(t, buf)
	if m["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", m["level"])
	}
	if m["key"] != "orders:42" {
		t.Errorf("key = %v, want orders:42", m["key"])
	}
}

func TestLogRequestsMiddleware(t *testing.T) {
	t.Parallel()

	l, buf := newBufLogger()
	h := LogRequests(l)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	req.Header.Set("X-Request-Id", "req-7")
	h.ServeHTTP(httptest.NewRecorder(), req)

	m := decode(t, buf)
	if m["request_id"] != "req-7" {
		t.Errorf("request_id = %v, want req-7", m["request_id"])
	}
	if m["method"] != http.MethodPost {
		t.Errorf("method = %v, want POST", m["method"])
	}
	if m["status"] != float64(http.StatusCreated) {
		t.Errorf("status = %v, want 201", m["status"])
	}
}

func ExampleWithFields() {
	l, buf := newBufLogger()
	WithFields(l, "user", "alice").Info("login")
	// buf now holds one JSON line; print it verbatim.
	fmt.Print(buf.String())
	// Output: {"level":"INFO","msg":"login","user":"alice"}
}
```

## Review

The helpers are correct when a balanced key-value list becomes paired fields and
an odd list degrades to `!BADKEY` rather than failing loudly — both asserted over
captured JSON. The middleware is correct when it emits exactly one line per request
with the captured status, which requires the `statusRecorder` shim because
`net/http` does not surface the written code afterward. The senior lesson is that
`...any` moved correctness off the type checker: keep pairs balanced, run `go
vet`'s slog analyzer on the direct calls, and never assume a wrapper over `...any`
is analyzed for you. Run `go test -race`.

## Resources

- [`log/slog`: `Logger.With` and attribute handling](https://pkg.go.dev/log/slog#Logger.With)
- [`log/slog`: the `!BADKEY` behavior in `Logger.Log`](https://pkg.go.dev/log/slog#Logger.Log)
- [`go vet`: the slog analyzer](https://pkg.go.dev/golang.org/x/tools/go/analysis/passes/slog)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-typed-int-segment-key.md](04-typed-int-segment-key.md) | Next: [06-sql-in-clause-args-builder.md](06-sql-in-clause-args-builder.md)
