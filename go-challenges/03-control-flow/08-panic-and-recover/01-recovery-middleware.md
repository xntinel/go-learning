# Exercise 1: HTTP Recovery Middleware: Panic to 500 with Structured Logs

The outermost boundary of every backend service is the request handler: one bad
request must never crash the process. This module builds the standard recovery
middleware — a `func(http.Handler) http.Handler` that defers `recover` at the top
of each request, turns any panic into a structured log plus a clean 500, and
hardens the two edges that trip people up: never leak the panic value into the
response body, and never write a 500 after the handler already sent headers.

This module is fully self-contained: its own module, its own demo, its own tests.

## What you'll build

```text
recovermw/                 independent module: example.com/recovermw
  go.mod                   go 1.26
  recovermw.go             PanicError (Error+Unwrap), statusWriter, Middleware
  cmd/
    demo/
      main.go              runnable demo: /ok returns 200, /boom recovers to 500
  recovermw_test.go        httptest 500/202/unwrap/string-panic/already-wrote cases
```

Files: `recovermw.go`, `cmd/demo/main.go`, `recovermw_test.go`.
Implement: `Middleware(next http.Handler) http.Handler`, `*PanicError` with `Error`+`Unwrap`, and a `statusWriter` that tracks whether a header was written.
Test: `httptest.NewServer(Middleware(handler))` asserting 500 on a panic, 202 pass-through, `PanicError` unwrap via `errors.Is`, a `panic("string")` still yielding 500, and a handler that writes 200 then panics not producing a duplicate `WriteHeader`.
Verify: `go test -count=1 -race ./...`

### Why the boundary is here and what it must not do

The middleware wraps `next.ServeHTTP` in a deferred `recover`. When the handler
panics, the deferred function catches it, builds a typed `*PanicError` capturing
the recovered value, the stack (`debug.Stack()` called first, before anything
else runs), and the request's method and path, logs the whole thing through
`slog` at error level, and writes the standard 500. Two disciplines separate a
toy from a production middleware.

First, the response body must be exactly `http.StatusText(http.StatusInternalServerError)`
— never the panic value. Leaking `%v` of a recovered panic into the body hands an
attacker your internal state, file paths, and sometimes secrets. The detail goes
to the log; the client gets the generic text.

Second, you cannot call `WriteHeader(500)` if the handler already wrote a header.
A handler that streams a 200 and then panics mid-body has already flushed the
status line; calling `http.Error` now produces a `superfluous WriteHeader call`
warning and a corrupt response. The fix is a thin `ResponseWriter` wrapper,
`statusWriter`, that records `wroteHeader` on the first `WriteHeader` or `Write`.
In the recover path, if `wroteHeader` is already set, you log but skip the 500 —
the response is already committed and the best you can do is abort it (the process
survives, which was the point).

`PanicError` implements `Unwrap`: if the recovered value is an `error`, `Unwrap`
returns it, so `errors.Is`/`errors.As` reach through the `PanicError` to the
original. A `panic("string")` still produces a valid `PanicError` (with `Value`
holding the string) and a 500 — the boundary is type-agnostic — but its `Unwrap`
returns `nil`, which is exactly why panicking with an error is better than a
string.

Create `recovermw.go`:

```go
package recovermw

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// PanicError carries a recovered panic across the HTTP boundary. It implements
// error and Unwrap so errors.Is/As reach the original value when it is an error.
type PanicError struct {
	Value  any
	Stack  []byte
	Method string
	Path   string
}

func (p *PanicError) Error() string {
	return fmt.Sprintf("panic in %s %s: %v", p.Method, p.Path, p.Value)
}

func (p *PanicError) Unwrap() error {
	if err, ok := p.Value.(error); ok {
		return err
	}
	return nil
}

// statusWriter wraps http.ResponseWriter to record whether a status header has
// been written, so the recover path never emits a duplicate WriteHeader.
type statusWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true // an implicit 200 is committed on first Write
	return w.ResponseWriter.Write(b)
}

// Middleware recovers panics from next, logs them with a stack via slog, and
// writes a standard 500 unless the handler already committed a response.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			pe := &PanicError{
				Value:  rec,
				Stack:  debug.Stack(),
				Method: r.Method,
				Path:   r.URL.Path,
			}
			slog.Error("handler panic",
				"method", pe.Method,
				"path", pe.Path,
				"err", pe.Error(),
				"stack", string(pe.Stack),
			)
			if sw.wroteHeader {
				return // response already committed; cannot write a clean 500
			}
			http.Error(sw, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}()
		next.ServeHTTP(sw, r)
	})
}
```

### The runnable demo

The demo wires two routes behind the middleware: `/ok` returns 200, `/boom`
panics. The server keeps running after the panic because the middleware recovered
it — that is the whole point of the boundary.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/recovermw"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		panic("intentional panic for the demo")
	})

	srv := httptest.NewServer(recovermw.Middleware(mux))
	defer srv.Close()

	for _, path := range []string{"/ok", "/boom"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			fmt.Println("request error:", err)
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s -> %d %q\n", path, resp.StatusCode, string(body))
	}
	fmt.Println("server still serving after the panic")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the `slog` error line for the recovered panic also prints to
stderr):

```
/ok -> 200 "ok\n"
/boom -> 500 "Internal Server Error\n"
server still serving after the panic
```

### Tests

The tests run the middleware behind a real `httptest.NewServer` so the full
`net/http` request path is exercised. `TestRecoversToInternalError` asserts a
panicking handler yields 500 with exactly the standard body and no leak of the
panic value. `TestPassThrough` proves the happy path (202) is untouched.
`TestPanicErrorUnwraps` proves `errors.Is` reaches the original error through the
`PanicError`. `TestNonErrorPanicStillRecovers` proves a `panic("string")` still
yields 500. `TestNoDuplicateWriteHeader` drives a handler that writes 200 and
then panics, asserting the client still sees 200 (the middleware did not stomp a
500 over the committed response).

Create `recovermw_test.go`:

```go
package recovermw

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRecoversToInternalError(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("kaboom secret detail"))
	})
	srv := httptest.NewServer(Middleware(h))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got, want := string(body), http.StatusText(http.StatusInternalServerError)+"\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if strings.Contains(string(body), "kaboom") {
		t.Fatalf("body leaked the panic value: %q", body)
	}
}

func TestPassThrough(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "queued")
	})
	srv := httptest.NewServer(Middleware(h))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
}

func TestPanicErrorUnwraps(t *testing.T) {
	t.Parallel()

	inner := errors.New("original")
	pe := &PanicError{Value: inner, Method: "GET", Path: "/x"}
	if !errors.Is(pe, inner) {
		t.Fatal("PanicError should unwrap to the original error")
	}
	if pe.Unwrap() != inner {
		t.Fatalf("Unwrap = %v, want %v", pe.Unwrap(), inner)
	}
}

func TestNonErrorPanicStillRecovers(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("bare string panic")
	})
	srv := httptest.NewServer(Middleware(h))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	// A string panic has no error to unwrap.
	pe := &PanicError{Value: "bare string panic"}
	if pe.Unwrap() != nil {
		t.Fatalf("Unwrap of string panic = %v, want nil", pe.Unwrap())
	}
}

func TestNoDuplicateWriteHeader(t *testing.T) {
	t.Parallel()

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "partial ")
		panic("panic after headers were sent")
	})
	srv := httptest.NewServer(Middleware(h))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The 200 was already committed; the middleware must not overwrite it with 500.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (header already written)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(string(body), "partial") {
		t.Fatalf("body = %q, want the partially-written response", body)
	}
}
```

## Review

The middleware is correct when a panicking handler always produces a 500 with the
standard body and nothing else, the happy path is untouched, and a panic that
arrives after the handler already committed a response does not corrupt it with a
second `WriteHeader`. The `statusWriter` is the load-bearing piece for that last
property: it flips `wroteHeader` on the first `WriteHeader` or `Write`, and the
recover path consults it before daring to write a 500. The three traps this
module closes are leaking the panic value into the body (assert the body never
contains the panic text), recovering silently (the `slog.Error` line makes every
recovery observable), and stomping a committed response. Note the boundary lives
at the top of the request and nowhere else — do not sprinkle `recover` inside the
handlers it wraps.

## Resources

- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the mechanism and the deferred-recover pattern.
- [net/http.Handler](https://pkg.go.dev/net/http#Handler) — the interface the middleware adapts.
- [runtime/debug.Stack](https://pkg.go.dev/runtime/debug#Stack) — capturing the goroutine stack at recovery time.
- [log/slog](https://pkg.go.dev/log/slog) — structured logging for the recovered panic.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-safe-goroutine-supervisor.md](02-safe-goroutine-supervisor.md)
