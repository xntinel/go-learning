# Exercise 2: An Access-Log ResponseWriter that Captures Status and Bytes

The middleware primitive every backend ships: a struct that embeds
`http.ResponseWriter`, overrides `WriteHeader` to capture the status code and
`Write` to count bytes, and exposes `Status()`/`BytesWritten()` for the access-log
and metrics layer. This is the canonical delegation-plus-override wrapper.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
accesslog/                  independent module: example.com/accesslog
  go.mod                    go 1.26
  accesslog.go              type responseWriter (embeds http.ResponseWriter); Middleware
  cmd/
    demo/
      main.go               drive a wrapped handler with httptest, print captured status/bytes
  accesslog_test.go         default-200, explicit status, second-WriteHeader ignored, byte count
```

- Files: `accesslog.go`, `cmd/demo/main.go`, `accesslog_test.go`.
- Implement: a `responseWriter` embedding `http.ResponseWriter` that captures the status (default 200) and counts body bytes, plus a `Middleware(record)` that reports them after the handler returns.
- Test: `Status()` defaults to 200 when the handler only writes, reflects an explicit `WriteHeader(404)`, ignores a second `WriteHeader`, and `BytesWritten()` equals the response length; a static assertion that the wrapper still satisfies `http.ResponseWriter`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/08-interfaces/05-interface-composition-and-embedding/02-instrumented-responsewriter/cmd/demo
cd go-solutions/08-interfaces/05-interface-composition-and-embedding/02-instrumented-responsewriter
```

### Why embed, and the two behaviors that must match the stdlib

Embedding `http.ResponseWriter` gives the wrapper all three of its methods for
free; you then shadow exactly the two you care about. `WriteHeader` is overridden
to record the code before delegating — and it must delegate, or no header is sent.
`Write` is overridden to count bytes, and it must reproduce a rule the stdlib
enforces implicitly: the first `Write` on a response with no explicit status sends
a `200 OK` header. So your `Write` checks whether a header was written and, if not,
calls `WriteHeader(http.StatusOK)` first — otherwise the captured status is a
meaningless zero for the most common handler shape (a handler that just writes a
body). The second rule is that a *second* `WriteHeader` is a no-op in `net/http`
(it logs "superfluous WriteHeader call"); your wrapper mirrors that by ignoring
subsequent calls, so the captured status is the first one, which is what the client
actually received.

Everything else — `Header()`, and any optional interfaces — stays promoted or
absent. (This wrapper deliberately does *not* preserve `http.Flusher`; Exercise 3
fixes exactly that gap with `Unwrap`.)

Create `accesslog.go`:

```go
package accesslog

import "net/http"

// responseWriter wraps an http.ResponseWriter to capture the status code and the
// number of body bytes written, for an access log or metrics middleware. It
// embeds the interface so Header and the rest stay promoted, and overrides only
// WriteHeader and Write.
type responseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func newResponseWriter(w http.ResponseWriter) *responseWriter {
	// Default to 200: a handler that only calls Write still returns 200 OK.
	return &responseWriter{ResponseWriter: w, status: http.StatusOK}
}

// WriteHeader records the first status code and delegates. Subsequent calls are
// ignored, matching net/http's "superfluous WriteHeader" semantics.
func (w *responseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Write counts bytes and, on the first write with no explicit header, sends 200.
func (w *responseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

// Status returns the captured status code (200 if the handler never set one).
func (w *responseWriter) Status() int { return w.status }

// BytesWritten returns the number of body bytes written.
func (w *responseWriter) BytesWritten() int { return w.bytes }

// Middleware wraps h so that after it returns, record is called with the
// response's status code and body byte count.
func Middleware(record func(status, bytes int)) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := newResponseWriter(w)
			h.ServeHTTP(rw, r)
			record(rw.Status(), rw.BytesWritten())
		})
	}
}
```

### The runnable demo

The demo wraps two handlers — one that sets an explicit status, one that only
writes — and prints what the access-log callback observed.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/accesslog"
)

func main() {
	var status, bytes int
	mw := accesslog.Middleware(func(s, b int) { status, bytes = s, b })

	explicit := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		fmt.Fprint(w, "short and stout")
	}))
	explicit.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Printf("explicit: status=%d bytes=%d\n", status, bytes)

	writeOnly := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	writeOnly.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Printf("write-only: status=%d bytes=%d\n", status, bytes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
explicit: status=418 bytes=15
write-only: status=200 bytes=2
```

### Tests

Create `accesslog_test.go`:

```go
package accesslog

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The wrapper must still satisfy http.ResponseWriter.
var _ http.ResponseWriter = (*responseWriter)(nil)

func TestStatusAndBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		handler    http.HandlerFunc
		wantStatus int
		wantBytes  int
	}{
		{
			name: "write only defaults to 200",
			handler: func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "body")
			},
			wantStatus: http.StatusOK,
			wantBytes:  4,
		},
		{
			name: "explicit 404",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, "missing")
			},
			wantStatus: http.StatusNotFound,
			wantBytes:  7,
		},
		{
			name: "second WriteHeader ignored",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, "x")
			},
			wantStatus: http.StatusCreated,
			wantBytes:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotStatus, gotBytes int
			mw := Middleware(func(s, b int) { gotStatus, gotBytes = s, b })
			h := mw(tc.handler)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %d, want %d", gotStatus, tc.wantStatus)
			}
			if gotBytes != tc.wantBytes {
				t.Errorf("bytes = %d, want %d", gotBytes, tc.wantBytes)
			}
		})
	}
}

func TestUnderlyingReceivesStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	rw := newResponseWriter(rec)
	rw.WriteHeader(http.StatusNotFound)
	fmt.Fprint(rw, "nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("underlying recorder code = %d, want 404", rec.Code)
	}
	if rec.Body.String() != "nope" {
		t.Fatalf("body = %q, want nope", rec.Body.String())
	}
}

func ExampleMiddleware() {
	mw := Middleware(func(status, bytes int) {
		fmt.Printf("%d %d\n", status, bytes)
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "nope")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	// Output: 404 4
}
```

## Review

The wrapper is correct when the two shadowed methods agree with `net/http`'s own
behavior: the default status is 200 (not 0), the first `WriteHeader` wins, and the
byte count equals what reached the underlying writer. `TestUnderlyingReceivesStatus`
is the guard against the most damaging mistake — overriding `WriteHeader` to
capture the code but forgetting to call `w.ResponseWriter.WriteHeader(code)`, which
records a correct-looking status while sending the client a bare 200. Note what
this wrapper does *not* do: because it embeds only the `http.ResponseWriter`
interface, it silently drops `http.Flusher` and `http.Hijacker`. That is the bug
Exercise 3 exists to fix.

## Resources

- [`http.ResponseWriter`](https://pkg.go.dev/net/http#ResponseWriter) — the `WriteHeader`/`Write` contract this wrapper overrides.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for driving handlers in tests.
- [Go Specification: Struct types](https://go.dev/ref/spec#Struct_types) — embedded fields and method promotion.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-memconn-readwritecloser.md](01-memconn-readwritecloser.md) | Next: [03-responsecontroller-forwarding.md](03-responsecontroller-forwarding.md)
