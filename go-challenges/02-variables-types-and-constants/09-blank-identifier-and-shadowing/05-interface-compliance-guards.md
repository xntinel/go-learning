# Exercise 5: Compile-Time Interface Guards Across a Handler Set

An HTTP handler and middleware layer where every implementation is pinned with a
compile-time guard: `var _ http.Handler = (*JSONHandler)(nil)` and
`var _ Middleware = LoggingMiddleware`. The guards catch signature drift at build
time and cost nothing at runtime; an optional capability (`http.Flusher`) is
handled instead with a runtime comma-ok assertion.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
handlers/                       module: example.com/handlers
  go.mod
  handlers.go                   JSONHandler, HealthHandler, StreamHandler, LoggingMiddleware, guards
  cmd/
    demo/
      main.go                   mux + logging middleware, exercised via httptest
  handlers_test.go              handler behavior, flush-capability comma-ok both branches
```

- Files: `handlers.go`, `cmd/demo/main.go`, `handlers_test.go`.
- Implement: `JSONHandler`, `HealthHandler`, `StreamHandler` (optional flush via comma-ok), and `LoggingMiddleware`, each pinned by a compile-time guard.
- Test: httptest exercises of each handler, plus a flush-capability check proving the flush path runs only when the writer supports it and never panics when it does not.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/05-interface-compliance-guards/cmd/demo
cd go-solutions/02-variables-types-and-constants/09-blank-identifier-and-shadowing/05-interface-compliance-guards
```

### Why the nil-pointer guard, and where comma-ok takes over

`var _ http.Handler = (*JSONHandler)(nil)` asks the compiler to prove that
`*JSONHandler` implements `http.Handler`. `(*JSONHandler)(nil)` is a typed nil
pointer: it constructs nothing and allocates nothing, unlike `&JSONHandler{}`
which would build a value just to be discarded. The guard's payoff is timing: if
someone renames a parameter type on `ServeHTTP` or drops a return, the build fails
*at the guard line*, naming the type and the interface, instead of surfacing as a
confusing error at a distant `mux.Handle` call. The same guard on a `Middleware`
value (`var _ Middleware = LoggingMiddleware`) pins that a plain function still
matches the `func(http.Handler) http.Handler` shape.

Guards work for *required* conformance. An *optional* capability does not fit a
compile-time guard, because the type may or may not have it. `http.Flusher` is
the classic example: some `ResponseWriter`s can flush, some cannot. You discover
that at runtime with a comma-ok type assertion — `if f, ok := w.(http.Flusher);
ok { f.Flush() }`. Crucially, this is the *comma-ok* form: the single-value form
`f := w.(http.Flusher)` would panic on any writer that cannot flush. Discarding
the `ok` here would reintroduce exactly the panic the guard style is meant to
avoid.

Create `handlers.go`:

```go
package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Middleware wraps a handler with cross-cutting behavior.
type Middleware func(http.Handler) http.Handler

// JSONHandler serves a JSON object.
type JSONHandler struct {
	Payload map[string]any
}

var _ http.Handler = (*JSONHandler)(nil)

func (h *JSONHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.Payload)
}

// HealthHandler serves a liveness response.
type HealthHandler struct{}

var _ http.Handler = (*HealthHandler)(nil)

func (HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, "ok")
}

// StreamHandler writes a chunk and flushes only if the writer supports it.
type StreamHandler struct{}

var _ http.Handler = (*StreamHandler)(nil)

func (StreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "chunk")
	if f, ok := w.(http.Flusher); ok { // optional capability, comma-ok not single-value
		f.Flush()
	}
}

// LogDst receives middleware log lines; it discards by default.
var LogDst io.Writer = io.Discard

// LoggingMiddleware logs each request's method, path, and final status.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		fmt.Fprintf(LogDst, "%s %s -> %d\n", r.Method, r.URL.Path, rec.status)
	})
}

var _ Middleware = LoggingMiddleware

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
```

### The runnable demo

The demo builds a mux, wraps it in the logging middleware, and drives it with
`httptest` so the output is deterministic. `LogDst` is pointed at stdout, so each
request logs before its result prints.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"example.com/handlers"
)

func main() {
	handlers.LogDst = os.Stdout

	mux := http.NewServeMux()
	mux.Handle("/health", handlers.HealthHandler{})
	mux.Handle("/user", &handlers.JSONHandler{Payload: map[string]any{"id": "u1"}})

	wrapped := handlers.LoggingMiddleware(mux)

	for _, path := range []string{"/health", "/user"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		fmt.Printf("%s -> %d %s\n", path, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /health -> 200
/health -> 200 ok
GET /user -> 200
/user -> 200 {"id":"u1"}
```

### Tests

The flush tests are the interesting pair: one writer implements `http.Flusher`
(the flush path runs), one does not (the comma-ok is false, nothing panics).

Create `handlers_test.go`:

```go
package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// flushRecorder implements http.Flusher and counts flushes.
type flushRecorder struct {
	http.ResponseWriter
	flushed int
}

func (f *flushRecorder) Flush() { f.flushed++ }

// basicWriter is a ResponseWriter that does NOT implement http.Flusher.
type basicWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (b *basicWriter) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}

func (b *basicWriter) Write(p []byte) (int, error) { return b.body.Write(p) }

func (b *basicWriter) WriteHeader(code int) { b.status = code }

func TestJSONHandler(t *testing.T) {
	t.Parallel()

	h := &JSONHandler{Payload: map[string]any{"id": "u1"}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/user", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"id":"u1"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestHealthHandler(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	HealthHandler{}.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("health = %d %q", rec.Code, rec.Body.String())
	}
}

func TestStreamHandlerFlushesWhenSupported(t *testing.T) {
	t.Parallel()

	fr := &flushRecorder{ResponseWriter: httptest.NewRecorder()}
	StreamHandler{}.ServeHTTP(fr, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if fr.flushed != 1 {
		t.Fatalf("flush count = %d, want 1", fr.flushed)
	}
}

func TestStreamHandlerNoPanicWithoutFlusher(t *testing.T) {
	t.Parallel()

	bw := &basicWriter{}
	StreamHandler{}.ServeHTTP(bw, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if bw.body.String() != "chunk" {
		t.Fatalf("body = %q, want chunk", bw.body.String())
	}
}

func TestLoggingMiddleware(t *testing.T) {
	var buf bytes.Buffer
	LogDst = &buf
	defer func() { LogDst = io.Discard }()

	wrapped := LoggingMiddleware(HealthHandler{})
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if !strings.Contains(buf.String(), "GET /health -> 200") {
		t.Fatalf("log = %q", buf.String())
	}
}

func ExampleHealthHandler() {
	rec := httptest.NewRecorder()
	HealthHandler{}.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	fmt.Println(rec.Code, rec.Body.String())
	// Output: 200 ok
}
```

## Review

The guards are proven by the package building at all: break a `ServeHTTP`
signature and `go build` fails at the `var _ http.Handler = ...` line, not
downstream. The runtime tests confirm behavior, and the two stream tests prove the
comma-ok optional-interface pattern: `TestStreamHandlerFlushesWhenSupported` takes
the flush path with a `Flusher`, and `TestStreamHandlerNoPanicWithoutFlusher`
proves a non-flushing writer does not panic. The mistake to avoid is a
single-value assertion `w.(http.Flusher)` for an optional capability — it panics
on the first writer that lacks it. Use `&Impl{}` only when you truly need a value;
for a guard, the nil pointer allocates nothing.

## Resources

- [`net/http.Handler`](https://pkg.go.dev/net/http#Handler) — the interface the guards pin.
- [`net/http.Flusher`](https://pkg.go.dev/net/http#Flusher) — the optional capability checked via comma-ok.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for handler tests.
- [Go Code Review Comments: interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — where to assert conformance.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-idempotency-cache-comma-ok.md](06-idempotency-cache-comma-ok.md)
