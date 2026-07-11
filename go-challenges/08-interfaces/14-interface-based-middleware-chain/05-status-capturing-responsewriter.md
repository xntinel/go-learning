# Exercise 5: Wrapping http.ResponseWriter to capture status and bytes

Access logs and RED metrics need the response's status code and byte count, but
`http.ResponseWriter` exposes neither after the fact — you must wrap it. This
module builds a status-capturing wrapper and, critically, keeps the optional
`http.Flusher` interface alive through `http.NewResponseController`, so streaming
handlers behind the wrapper still work.

Fully self-contained: its own `go mod init`, demo, and tests. Nothing here imports
another exercise.

## What you'll build

```text
capturewriter/               independent module: example.com/capturewriter
  go.mod                     go 1.26
  middleware.go              responseWriter wrapper (status+size, Unwrap) + Capture middleware
  cmd/demo/main.go           runnable demo printing captured status and size
  middleware_test.go         default-200, explicit-404, idempotent WriteHeader, Flush survives
```

- Files: `middleware.go`, `cmd/demo/main.go`, `middleware_test.go`.
- Implement: a `responseWriter` wrapping `http.ResponseWriter` that records the status (defaulting to 200 on first `Write`) and total bytes, exposes `Status()`/`Size()`, makes `WriteHeader` idempotent, and provides `Unwrap()` so `http.NewResponseController` reaches the real `Flush`.
- Test: a body with no explicit `WriteHeader` captures 200 and `len(body)`; `WriteHeader(404)` then `Write` captures 404 and ignores a second `WriteHeader`; a streaming handler flushes through the wrapper and `httptest.ResponseRecorder.Flushed` is true.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/capturewriter/cmd/demo
cd ~/go-exercises/capturewriter
go mod init example.com/capturewriter
go mod edit -go=1.26
```

### Capturing status without breaking Flusher

To know the status code an access-log middleware needs a wrapper: a struct that
embeds `http.ResponseWriter`, overrides `WriteHeader` to remember the code, and
overrides `Write` to count bytes and default the status to 200 when the handler
writes a body without an explicit `WriteHeader` (mirroring the server's own
implicit-200 behavior). Making `WriteHeader` idempotent — record and forward only
the first call — is defensive: a downstream layer that mistakenly calls it twice
gets the write-once contract enforced by the wrapper instead of a corrupt response.

The senior detail is what wrapping *breaks*. The concrete `http.ResponseWriter`
the server hands a handler also implements `http.Flusher` (needed for SSE and
chunked streaming), `http.Hijacker` (websocket upgrades), and more. Your wrapper
struct only has the methods you wrote, so `w.(http.Flusher)` inside a streaming
handler now fails — flushing silently stops, and an SSE endpoint buffers forever.
Before Go 1.20 you fixed this by hand-forwarding every optional method. Since Go
1.20 the correct tool is `http.NewResponseController(w)`: it reaches *through*
wrappers by calling an `Unwrap() http.ResponseWriter` method to find the real
`Flush`, `Hijack`, or `SetWriteDeadline`. So the wrapper only needs to add one
method — `Unwrap()` returning the embedded writer — and every optional interface
survives. The demo and test flush through `http.NewResponseController` to prove it.

Create `middleware.go`:

```go
package capturewriter

import "net/http"

type Handler = http.Handler

type Middleware func(Handler) Handler

// responseWriter wraps an http.ResponseWriter to record the status code and the
// number of bytes written, for access logs and RED metrics.
type responseWriter struct {
	http.ResponseWriter
	status      int
	size        int
	wroteHeader bool
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	// Default to 200: a handler that writes a body without WriteHeader commits 200.
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return // idempotent: honor only the first WriteHeader
	}
	rw.status = code
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	rw.wroteHeader = true // first Write implicitly commits the status
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// Unwrap exposes the underlying writer so http.NewResponseController can reach
// optional interfaces (Flusher, Hijacker, deadline setters) through the wrapper.
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

func (rw *responseWriter) Status() int { return rw.status }
func (rw *responseWriter) Size() int   { return rw.size }

// Capture wraps next so a supplied callback receives the final status and size
// after the handler returns. This is the seam an access log or metrics layer uses.
func Capture(record func(status, size int)) Middleware {
	return func(next Handler) Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := newResponseWriter(w)
			next.ServeHTTP(rw, r)
			record(rw.Status(), rw.Size())
		})
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/capturewriter"
)

func main() {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "resource created")
	})

	var gotStatus, gotSize int
	handler := capturewriter.Capture(func(status, size int) {
		gotStatus, gotSize = status, size
	})(final)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/things", nil))

	fmt.Printf("captured status=%d size=%d\n", gotStatus, gotSize)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
captured status=201 size=16
```

### Tests

`TestDefaultStatus200` proves a body with no explicit `WriteHeader` captures 200
and the correct size. `TestExplicitStatusAndIdempotent` proves an explicit 404 is
captured and a second `WriteHeader(500)` is ignored. `TestFlushSurvivesWrapping`
is the key one: a streaming handler calls `http.NewResponseController(w).Flush()`
through the wrapper, and `httptest.ResponseRecorder.Flushed` becomes true — proof
the optional `Flusher` survived because `Unwrap` reached the real recorder.

Create `middleware_test.go`:

```go
package capturewriter

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDefaultStatus200(t *testing.T) {
	t.Parallel()

	var status, size int
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello") // no explicit WriteHeader
	})
	Capture(func(st, sz int) { status, size = st, sz })(final).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if size != len("hello") {
		t.Errorf("size = %d, want %d", size, len("hello"))
	}
}

func TestExplicitStatusAndIdempotent(t *testing.T) {
	t.Parallel()

	var status, size int
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.WriteHeader(http.StatusInternalServerError) // must be ignored
		fmt.Fprint(w, "missing")
	})
	rec := httptest.NewRecorder()
	Capture(func(st, sz int) { status, size = st, sz })(final).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if status != http.StatusNotFound {
		t.Errorf("captured status = %d, want 404", status)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("recorder code = %d, want 404 (second WriteHeader must be ignored)", rec.Code)
	}
	if size != len("missing") {
		t.Errorf("size = %d, want %d", size, len("missing"))
	}
}

func TestFlushSurvivesWrapping(t *testing.T) {
	t.Parallel()

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: tick\n\n")
		// Reach the real Flusher through the wrapper via the response controller.
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("Flush through wrapper failed: %v", err)
		}
	})

	rec := httptest.NewRecorder()
	Capture(func(int, int) {})(final).ServeHTTP(
		rec, httptest.NewRequest(http.MethodGet, "/stream", nil))

	if !rec.Flushed {
		t.Fatal("recorder not flushed: Unwrap did not preserve the Flusher interface")
	}
}
```

## Review

The wrapper is correct when `Status()` reports 200 for an un-`WriteHeader`ed body,
reports the first explicit code otherwise, ignores subsequent `WriteHeader` calls,
and when `Size()` equals the total bytes written. The test that separates a real
implementation from a broken one is `TestFlushSurvivesWrapping`: a wrapper without
`Unwrap` compiles and captures status fine, but silently kills streaming, and only
the `Flushed` assertion catches it. `http.NewResponseController` is the modern
answer to a whole category of "my SSE endpoint stopped flushing after I added a
logging middleware" bugs — always give a `ResponseWriter` wrapper an `Unwrap`
method. Do not hand-forward `Flush`/`Hijack` yourself; the controller does it for
every optional interface at once.

## Resources

- [net/http#ResponseController](https://pkg.go.dev/net/http#ResponseController) — reaches Flush/Hijack/deadlines through wrappers via `Unwrap`.
- [net/http#NewResponseController](https://pkg.go.dev/net/http#NewResponseController) — constructs the controller for a (possibly wrapped) writer.
- [net/http#Flusher](https://pkg.go.dev/net/http#Flusher) — the optional interface streaming handlers depend on.
- [net/http/httptest#ResponseRecorder](https://pkg.go.dev/net/http/httptest#ResponseRecorder) — its `Flushed` field the test asserts on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-panic-recovery-middleware.md](04-panic-recovery-middleware.md) | Next: [06-request-id-context-middleware.md](06-request-id-context-middleware.md)
