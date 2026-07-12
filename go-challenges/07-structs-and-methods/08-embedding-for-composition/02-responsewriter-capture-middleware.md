# Exercise 2: Access-Log Middleware That Wraps http.ResponseWriter

Every production HTTP service logs the status and size of each response. The
status is not on the request — it is decided by the handler as it writes — so
middleware has to wrap `http.ResponseWriter` to observe it. This exercise builds
that wrapper by embedding, and does it without breaking streaming.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
accesslog/                 independent module: example.com/accesslog
  go.mod                   go 1.26
  accesslog.go             statusRecorder embeds http.ResponseWriter; Unwrap; LoggingMiddleware
  cmd/
    demo/
      main.go              runnable demo: request through the middleware, observe status/body
  accesslog_test.go        captured status, default-200, byte count, Flusher-still-works
```

- Files: `accesslog.go`, `cmd/demo/main.go`, `accesslog_test.go`.
- Implement: a `statusRecorder` embedding `http.ResponseWriter` that overrides `WriteHeader`/`Write` to capture status and bytes, exposes `Unwrap`, and a `LoggingMiddleware(logger, next)` that logs method/path/status/size/latency.
- Test: captured status equals what the handler wrote, default 200 when unset, byte count equals body length, and streaming still works through `http.NewResponseController`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/08-embedding-for-composition/02-responsewriter-capture-middleware/cmd/demo
cd go-solutions/07-structs-and-methods/08-embedding-for-composition/02-responsewriter-capture-middleware
```

### Capturing the status without breaking the writer

A handler tells the client its status by calling `w.WriteHeader(code)`, or by
calling `w.Write(body)` first, which implicitly sends a 200. Middleware that wants
to log the status must sit between the handler and the real writer and remember
what went by. You do that by embedding `http.ResponseWriter` in a `statusRecorder`
and overriding the two methods that carry the information. Everything else on
`http.ResponseWriter` — `Header()` — is promoted straight through to the real
writer, so the handler still sets headers normally.

Two details are load-bearing. First, the default-200: initialize the recorder's
`status` field to `http.StatusOK`, because a handler that only calls `Write` never
calls `WriteHeader`, yet the client receives 200. If you initialized to 0, your
log would report 0 for every plain `w.Write("hello")` handler. Second, guard
against multiple `WriteHeader` calls: only the first one counts on the wire, so
record only the first and forward every call (the real writer already warns on
duplicates).

The subtler hazard is that `http.ResponseWriter` is frequently more than its
interface. The concrete value a real server passes also implements
`http.Flusher` (streaming), `http.Hijacker` (websocket upgrades), and
`io.ReaderFrom`. A `statusRecorder` that embeds only `http.ResponseWriter` no
longer satisfies those optional interfaces, so a streaming handler that does
`w.(http.Flusher).Flush()` would fail once wrapped, and responses would buffer.
The modern remedy is an `Unwrap() http.ResponseWriter` method: `http.ResponseController`
walks the `Unwrap` chain to find `Flush`/`Hijack` on the underlying writer, so a
handler that uses `http.NewResponseController(w)` keeps working through your
wrapper. That is why `Flush` is not reimplemented here — `Unwrap` plus
`http.NewResponseController` is the correct pattern.

Create `accesslog.go`:

```go
package accesslog

import (
	"log/slog"
	"net/http"
	"time"
)

// statusRecorder wraps an http.ResponseWriter to capture the status code and the
// number of body bytes written. It embeds the ResponseWriter so Header() and any
// other methods are promoted, and exposes Unwrap so http.ResponseController can
// still find Flusher/Hijacker on the underlying writer.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	written     int
	wroteHeader bool
}

// WriteHeader records the first status only, then forwards every call.
func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write forwards to the embedded writer and counts the bytes. A handler that
// writes a body without calling WriteHeader implicitly sends 200.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += n
	return n, err
}

// Unwrap lets http.NewResponseController reach the underlying writer's optional
// interfaces (Flusher, Hijacker) that embedding alone would hide.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// LoggingMiddleware wraps next and logs one line per request after it completes,
// with the captured status, byte count, and latency.
func LoggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"duration", time.Since(start),
		)
	})
}
```

### The runnable demo

The demo wraps a handler that returns 201 with a body, sends one request through
the middleware, and prints the client-observed status and body. The logger is
discarded so the output is deterministic (the log line carries a non-deterministic
latency); the captured-status behavior is proven precisely in the tests.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"example.com/accesslog"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wrapped := accesslog.LoggingMiddleware(logger, handler)

	ts := httptest.NewServer(wrapped)
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	fmt.Println("status:", resp.StatusCode)
	fmt.Println("body:", string(body))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 201
body: hello
```

### Tests

Because the tests live in `package accesslog`, they can construct a
`statusRecorder` directly and inspect its captured fields. `TestRecorderCaptures`
drives a recorder over an `httptest.ResponseRecorder` and covers the explicit
`WriteHeader`, the default-200 (a handler that only writes a body), and the byte
count. `TestMiddlewareLogs` runs the full middleware against a real `httptest`
server and asserts the log buffer carries the status and size. `TestFlushStillWorks`
proves the wrapper did not break streaming: a handler that calls
`http.NewResponseController(w).Flush()` succeeds because `Unwrap` exposes the
underlying flusher.

Create `accesslog_test.go`:

```go
package accesslog

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecorderCaptures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantBytes  int
	}{
		{
			name: "explicit status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "nope")
			},
			wantStatus: http.StatusNotFound,
			wantBytes:  4,
		},
		{
			name: "default 200 when WriteHeader never called",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "hello")
			},
			wantStatus: http.StatusOK,
			wantBytes:  5,
		},
		{
			name: "no body no status",
			handler: func(w http.ResponseWriter, r *http.Request) {
			},
			wantStatus: http.StatusOK,
			wantBytes:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			tc.handler.ServeHTTP(rec, req)
			if rec.status != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.status, tc.wantStatus)
			}
			if rec.written != tc.wantBytes {
				t.Errorf("written = %d, want %d", rec.written, tc.wantBytes)
			}
		})
	}
}

func TestMiddlewareLogs(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
	})
	ts := httptest.NewServer(LoggingMiddleware(logger, handler))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := buf.String()
	if !strings.Contains(got, "status=201") {
		t.Errorf("log missing status=201; got %q", got)
	}
	if !strings.Contains(got, "bytes=5") {
		t.Errorf("log missing bytes=5; got %q", got)
	}
}

func TestFlushStillWorks(t *testing.T) {
	t.Parallel()
	// The handler routes the flush result through the response body so the test
	// observes it with a proper happens-before edge (no shared variable to race).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			_, _ = io.WriteString(w, "noflush")
			return
		}
		_, _ = io.WriteString(w, "flushed")
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := httptest.NewServer(LoggingMiddleware(logger, handler))
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if string(body) != "flushed" {
		t.Fatalf("Flush failed through the wrapper (body=%q); Unwrap not reaching the underlying Flusher", body)
	}
}
```

## Review

The wrapper is correct when the logged status always equals what reached the
client: it captures the first `WriteHeader`, defaults to 200 when the handler only
writes a body, and never counts more bytes than were written. The mistake that
looks fine until it doesn't is initializing `status` to 0 — every plain-body
handler then logs a false 0. The mistake that looks fine until a websocket or SSE
endpoint breaks is embedding `http.ResponseWriter` and stopping there: without
`Unwrap`, the wrapped writer no longer offers `Flush`/`Hijack`, and
`TestFlushStillWorks` is the guard that catches it. Run `go test -race`; the
middleware creates one recorder per request, so there is no shared mutable state
to race, and the test confirms it.

## Resources

- [`net/http.ResponseWriter`](https://pkg.go.dev/net/http#ResponseWriter) — the interface, and the note that concrete values may also be `Flusher`/`Hijacker`.
- [`net/http.NewResponseController`](https://pkg.go.dev/net/http#NewResponseController) — how the `Unwrap` chain exposes `Flush`/`Hijack` through wrappers.
- [`net/http.Flusher`](https://pkg.go.dev/net/http#Flusher) — the streaming interface a naive wrapper hides.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-slog-handler-embedding.md](03-slog-handler-embedding.md)
