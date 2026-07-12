# Exercise 1: Adapt error-returning handlers to http.Handler

`http.Handler` has no error return, but idiomatic Go handlers want one. This
exercise builds the adapter that reconciles the two: a `Handler = func(w, r)
error` type and a `WithError` wrapper that runs the handler and, on a non-nil
error, routes to a single response-writing function. This is the contract the
whole chapter rests on — handlers return errors, one boundary writes responses.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
erradapter/                  independent module: example.com/erradapter
  go.mod                     go 1.26
  adapter.go                 type Handler; WithError adapter; writeError (single writer)
  cmd/
    demo/
      main.go                runnable demo: one ok handler, one failing handler
  adapter_test.go            httptest.NewRecorder tests: nil -> handler wrote, err -> adapter wrote
```

Files: `adapter.go`, `cmd/demo/main.go`, `adapter_test.go`.
Implement: a `Handler` function type, `WithError(Handler) http.Handler`, and a
single `writeError` that the adapter calls on a non-nil error.
Test: assert the adapter never writes when `err == nil` (the handler's own
response stands) and always writes when `err != nil` (the handler wrote nothing).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/10-error-handling-middleware/01-handler-error-adapter/cmd/demo
cd go-solutions/10-error-handling/10-error-handling-middleware/01-handler-error-adapter
```

### Why an adapter at all

`net/http` fixes the handler signature as `ServeHTTP(w http.ResponseWriter, r
*http.Request)` — no error return. That is a deliberate design choice: the
framework cannot know how *you* want to render a failure, so it forces you to
write the response yourself. The cost is that every handler ends up with its own
`if err != nil { http.Error(...); return }` scattered through it, and those
copies drift — one returns text, another JSON, one forgets the `return` and keeps
writing. The adapter removes the drift by letting handlers *return* the error and
concentrating the write in one place.

The type is `type Handler func(w http.ResponseWriter, r *http.Request) error`.
`WithError` turns one into a real `http.Handler` by wrapping it in an
`http.HandlerFunc`: it calls the handler, and if the returned error is non-nil it
calls the single `writeError` function. The invariant that makes this safe is a
contract with the handler: a handler either fully writes its success response and
returns `nil`, or it writes *nothing* and returns an error. It must never do
both. If it writes a partial body and then returns an error, the status is
already committed and `writeError` cannot change it — this exercise's tests pin
exactly that: `nil` means the handler's write stands untouched, non-nil means the
adapter is the writer.

For this first module `writeError` is intentionally minimal — it uses
`http.Error`, the stdlib one-liner that sets `Content-Type: text/plain`, writes
the status, and writes the message. Exercise 2 replaces it with sentinel-to-status
mapping and a JSON body; Exercise 5 replaces that with RFC 9457 problem+json. The
point here is the *shape*: one type, one adapter, one writer.

Create `adapter.go`:

```go
package erradapter

import "net/http"

// Handler is an HTTP handler that returns an error instead of writing its own
// failure response. The contract: return nil after fully writing a success
// response, or return a non-nil error having written nothing.
type Handler func(w http.ResponseWriter, r *http.Request) error

// WithError adapts a Handler to http.Handler. On a non-nil error it routes to
// the single response-writing function; on nil it leaves the handler's own
// response untouched.
func WithError(h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			writeError(w, r, err)
		}
	})
}

// writeError is the single place that turns an error into a response. Later
// exercises replace this body with status mapping and a JSON/problem body; here
// it is the stdlib one-liner so the adapter's shape is the whole lesson.
func writeError(w http.ResponseWriter, _ *http.Request, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
```

### The runnable demo

The demo wires two handlers behind the adapter into a mux and drives them with an
in-process `httptest` server, so `go run ./cmd/demo` produces deterministic
output you can diff. `/ok` writes a body and returns nil; `/fail` returns an error
and writes nothing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"example.com/erradapter"
)

func main() {
	mux := http.NewServeMux()
	mux.Handle("/ok", erradapter.WithError(func(w http.ResponseWriter, r *http.Request) error {
		fmt.Fprint(w, "hello")
		return nil
	}))
	mux.Handle("/fail", erradapter.WithError(func(w http.ResponseWriter, r *http.Request) error {
		return errors.New("boom")
	}))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/ok", "/fail"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			panic(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%s -> %d %q\n", path, resp.StatusCode, string(body))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/ok -> 200 "hello"
/fail -> 500 "boom\n"
```

(`http.Error` appends a newline, which is why the failing body is `"boom\n"`.)

### Tests

The tests use `httptest.NewRecorder`, which captures the status, headers, and
body an `http.Handler` writes. `TestNilErrorLeavesHandlerResponse` proves the
adapter does *not* write when the handler returns nil — the recorded status and
body are exactly what the handler wrote. `TestNonNilErrorAdapterWrites` proves the
adapter writes the failure when the handler returns an error having written
nothing. The table pins both halves of the contract.

Create `adapter_test.go`:

```go
package erradapter

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNilErrorLeavesHandlerResponse(t *testing.T) {
	t.Parallel()

	h := WithError(func(w http.ResponseWriter, r *http.Request) error {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, "made it")
		return nil
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (handler's own write)", rec.Code)
	}
	if rec.Body.String() != "made it" {
		t.Fatalf("body = %q, want %q", rec.Body.String(), "made it")
	}
}

func TestNonNilErrorAdapterWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		wantSub string
	}{
		{"simple", errors.New("boom"), "boom"},
		{"wrapped", fmt.Errorf("load: %w", errors.New("deep")), "deep"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := WithError(func(w http.ResponseWriter, r *http.Request) error {
				return tc.err // handler writes nothing
			})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if got := rec.Body.String(); !contains(got, tc.wantSub) {
				t.Fatalf("body = %q, want it to contain %q", got, tc.wantSub)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func ExampleWithError() {
	h := WithError(func(w http.ResponseWriter, r *http.Request) error {
		fmt.Fprint(w, "ok")
		return nil
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	fmt.Println(rec.Code, rec.Body.String())
	// Output: 200 ok
}
```

## Review

The adapter is correct when its behavior is a clean function of the handler's
return: `nil` means "the handler already wrote the response, do nothing", and
non-nil means "the handler wrote nothing, I am the writer". The two tests pin
those two states; if `TestNilErrorLeavesHandlerResponse` ever sees a 500, the
adapter is writing when it should not, which is the bug that produces double
responses in production. Keep the contract one-directional: a handler that both
writes a partial body and returns an error violates it, and no adapter can fix a
response whose status is already committed. The single `writeError` here is a
placeholder for the richer boundary the next exercises build; the value of this
module is the type and the wrapper, not the body it writes.

## Resources

- [`net/http#Handler`](https://pkg.go.dev/net/http#Handler) — the `ServeHTTP` contract and `http.HandlerFunc`.
- [`net/http#Error`](https://pkg.go.dev/net/http#Error) — the stdlib single-writer used here.
- [`net/http/httptest#NewRecorder`](https://pkg.go.dev/net/http/httptest#NewRecorder) — capturing a handler's response in tests.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-sentinel-status-mapping.md](02-sentinel-status-mapping.md)
