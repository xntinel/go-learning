# Exercise 2: Access-Log Middleware Wrapping http.ResponseWriter

Every production HTTP service needs structured access logs: for each request, the
status code and the number of bytes written. But `http.ResponseWriter` does not
expose what status was sent — once your handler calls `WriteHeader(503)`, that
number is gone. The standard fix is a wrapper struct that *embeds the
`http.ResponseWriter` interface* and overrides only `WriteHeader` and `Write` to
capture what flows through, inheriting `Header()` by promotion.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
accesslog/                  independent module: example.com/accesslog
  go.mod                    module example.com/accesslog
  accesslog.go              recorder wrapping http.ResponseWriter; AccessLog middleware
  cmd/
    demo/
      main.go               drive a wrapped handler and print captured status/bytes
  accesslog_test.go         httptest-driven tests: default 200, explicit code, byte count, guard
```

Files: `accesslog.go`, `cmd/demo/main.go`, `accesslog_test.go`.
Implement: a `responseRecorder` struct embedding `http.ResponseWriter`, overriding
`WriteHeader(int)` and `Write([]byte) (int, error)` to capture the status and byte
count, and an `AccessLog(next http.Handler, record func(status, bytes int))
http.Handler` middleware.
Test: status defaults to 200 when the handler never calls `WriteHeader`; equals
the explicit code when it does; captured byte count equals `len(body)`; the
embedded `Header()` still works via promotion; a second `WriteHeader` does not
overwrite the first.
Verify: `go test -count=1 -race ./...`

### Why embed the interface instead of reimplementing it

`http.ResponseWriter` has three methods: `Header() http.Header`,
`Write([]byte) (int, error)`, and `WriteHeader(statusCode int)`. A logging
middleware only cares about two of them — it wants to observe the status code and
count bytes — and has no business reimplementing `Header()`, which just returns
the underlying header map. Embedding the interface expresses exactly that: the
recorder *is* an `http.ResponseWriter` by forwarding through the embedded value,
and you override only `WriteHeader` and `Write`. `Header()` is promoted
unchanged, so the handler's `w.Header().Set(...)` still reaches the real header
map. If you had declared a named field instead of embedding, you would have to
write a pass-through `Header()` by hand, and every future method the interface
grows would be one more forwarder to maintain.

### The two things worth capturing correctly

Two details separate a correct recorder from a subtly broken one. First, the
*default status*. Go's `net/http` sends `200 OK` implicitly the first time a
handler writes a body without having called `WriteHeader`. Your recorder must
model that: initialize the captured status to `http.StatusOK`, and if `Write` is
called before any `WriteHeader`, treat it as an implicit 200. Otherwise a handler
that just writes a body would log a status of `0`.

Second, the *double-WriteHeader guard*. The real `http.ResponseWriter` ignores a
second `WriteHeader` call (and logs a warning). If your recorder blindly stored
the second code, your access log would disagree with the bytes actually sent. So
the override records the status only on the first call, mirroring the real
behavior. A `wroteHeader` bool tracks whether the header has been committed.

Create `accesslog.go`:

```go
package accesslog

import "net/http"

// responseRecorder embeds the http.ResponseWriter interface so it satisfies the
// interface by promotion, overriding only WriteHeader and Write to capture the
// status code and byte count. Header() is inherited from the embedded value.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

// WriteHeader records the status on the first call only, mirroring net/http,
// then forwards to the embedded writer.
func (r *responseRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(code)
}

// Write commits an implicit 200 the first time it is called without a prior
// WriteHeader, exactly as net/http does, and accumulates the byte count.
func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// AccessLog wraps next, capturing the response status and byte count and passing
// them to record after the handler returns.
func AccessLog(next http.Handler, record func(status, bytes int)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		record(rec.status, rec.bytes)
	})
}
```

### The runnable demo

The demo wraps a tiny handler that writes a `201 Created` and a short body, drives
it with an `httptest.ResponseRecorder`, and prints the captured status and byte
count that a real access log would emit.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/accesslog"
)

func main() {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created\n")
	})

	wrapped := accesslog.AccessLog(handler, func(status, bytes int) {
		fmt.Printf("access: status=%d bytes=%d\n", status, bytes)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/orders", nil)
	wrapped.ServeHTTP(rec, req)

	fmt.Printf("content-type: %s\n", rec.Header().Get("Content-Type"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
access: status=201 bytes=8
content-type: text/plain
```

### Tests

The tests drive the wrapped handler with `httptest` and assert each captured
value. Because the tests live in the same package, they can also construct a bare
`responseRecorder` to check that `Header()` is promoted from the embedded writer
and that a double `WriteHeader` is guarded.

Create `accesslog_test.go`:

```go
package accesslog

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func drive(t *testing.T, h http.Handler) (status, bytes int) {
	t.Helper()
	var gotStatus, gotBytes int
	wrapped := AccessLog(h, func(s, b int) { gotStatus, gotBytes = s, b })
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	return gotStatus, gotBytes
}

func TestDefaultsToStatusOK(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handler writes nothing and never calls WriteHeader.
	})
	status, bytes := drive(t, h)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if bytes != 0 {
		t.Fatalf("bytes = %d, want 0", bytes)
	}
}

func TestImplicitOKOnWrite(t *testing.T) {
	t.Parallel()

	body := "hello, world"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	})
	status, bytes := drive(t, h)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (implicit)", status)
	}
	if bytes != len(body) {
		t.Fatalf("bytes = %d, want %d", bytes, len(body))
	}
}

func TestCapturesExplicitStatus(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	status, _ := drive(t, h)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
}

func TestHeaderIsPromoted(t *testing.T) {
	t.Parallel()

	under := httptest.NewRecorder()
	rec := &responseRecorder{ResponseWriter: under, status: http.StatusOK}
	// Header() is not overridden; it must reach the embedded writer's map.
	rec.Header().Set("X-Trace", "abc123")
	if got := under.Header().Get("X-Trace"); got != "abc123" {
		t.Fatalf("promoted Header() did not reach embedded writer: %q", got)
	}
}

func TestDoubleWriteHeaderKeepsFirst(t *testing.T) {
	t.Parallel()

	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusMovedPermanently)
	rec.WriteHeader(http.StatusInternalServerError)
	if rec.status != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want first code %d", rec.status, http.StatusMovedPermanently)
	}
}
```

## Review

The wrapper is correct when it captures 200 by default, the explicit code when
the handler sets one, an implicit 200 the moment a body is written, and a byte
count equal to what was actually written — and when a second `WriteHeader` is
ignored, matching `net/http`. The mistakes it guards against: forgetting the
implicit-200 rule (a body-only handler would log status 0); overwriting the
status on a second `WriteHeader` (the log would disagree with the wire); and
declaring a named field instead of embedding, which forces you to hand-write a
`Header()` forwarder for no reason. Run `go test -race` to be sure the recorder is
safe if the middleware is ever exercised concurrently across requests.

## Resources

- [net/http: ResponseWriter](https://pkg.go.dev/net/http#ResponseWriter) — the three methods and the implicit-200 rule for `Write`.
- [net/http: Handler and HandlerFunc](https://pkg.go.dev/net/http#Handler) — the middleware shape used here.
- [net/http/httptest: NewRecorder and NewRequest](https://pkg.go.dev/net/http/httptest#NewRecorder) — driving a handler in tests.

---

Prev: [01-json-patch-anonymous-meta.md](01-json-patch-anonymous-meta.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-safe-cache-embedded-mutex.md](03-safe-cache-embedded-mutex.md)
