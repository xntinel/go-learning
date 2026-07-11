# Exercise 10: Handlers That Return An error, Adapted To http.Handler

`http.HandlerFunc` cannot return an error, so every stdlib handler has to write
its own status code and body inline — and error handling ends up copy-pasted into
every handler. This exercise defines a named `HandlerFunc` type that *does* return
an error and an `Adapt` function that turns it into an `http.Handler`, centralizing
the error-to-status mapping in one place.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It is the last exercise in the lesson.

## What you'll build

```text
adapter/                   independent module: example.com/adapter
  go.mod                   go 1.25
  adapter.go               type HandlerFunc; Adapt(HandlerFunc) http.Handler; ErrNotFound/ErrBadInput
  cmd/
    demo/
      main.go              mounts adapted handlers on a ServeMux and prints responses
  adapter_test.go          nil->200; ErrBadInput->400; ErrNotFound->404; unclassified->500; -race
```

- Files: `adapter.go`, `cmd/demo/main.go`, `adapter_test.go`.
- Implement: `type HandlerFunc func(http.ResponseWriter, *http.Request) error`; `Adapt(HandlerFunc) http.Handler` that calls it and maps a non-nil error via `errors.Is` on `ErrNotFound`/`ErrBadInput` to the right status with a JSON error body, defaulting to `500` with a generic body.
- Test: a handler returning nil leaves its written `200` untouched; returning `ErrBadInput` yields `400`; `ErrNotFound` yields `404`; an unclassified error yields `500` with a generic body (no internal leak); the adapter satisfies `http.Handler` mounted on a `ServeMux`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/adapter/cmd/demo
cd ~/go-exercises/adapter
go mod init example.com/adapter
go mod edit -go=1.25
```

### A named function type, adapted through a function value

`type HandlerFunc func(http.ResponseWriter, *http.Request) error` is a named
function type — a first-class value with a name, so it can carry documentation and
be passed around as a unit. It differs from the stdlib's `http.HandlerFunc` in one
crucial way: it *returns an error*. That lets a handler write
`return ErrNotFound` instead of hand-rolling `http.Error(w, ..., 404)`, and it
means every handler in the codebase reports failure the same way.

`Adapt` is the bridge. It takes a `HandlerFunc` value and returns an
`http.Handler` (via a closure wrapped in `http.HandlerFunc`) that calls the inner
handler and, on a non-nil error, does the status mapping *once*:

- `errors.Is(err, ErrBadInput)` -> `400`
- `errors.Is(err, ErrNotFound)` -> `404`
- anything else -> `500` with a *generic* body, because an unclassified error may
  contain internal detail (a SQL string, a file path) that must not leak to the
  client. The real error is still available to log server-side.

This is functions-as-values doing composition: the varying behavior (the specific
handler) is passed in as a value, and the fixed behavior (error mapping) lives in
the adapter. Adding a new handler does not duplicate the mapping; changing the
mapping does not touch any handler. Because `Adapt` returns an `http.Handler`, the
result mounts on a `ServeMux` like any other handler.

Create `adapter.go`:

```go
package adapter

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Sentinels a handler returns to select a status. A handler wraps these with %w
// (or returns them directly) and the adapter maps them.
var (
	ErrBadInput = errors.New("bad input")
	ErrNotFound = errors.New("not found")
)

// HandlerFunc is a handler that may return an error instead of writing the status
// itself. The adapter turns the error into the right HTTP response.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Adapt turns an error-returning HandlerFunc into a standard http.Handler,
// centralizing the error-to-status mapping.
func Adapt(h HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := h(w, r)
		if err == nil {
			return
		}
		switch {
		case errors.Is(err, ErrBadInput):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			// Do not leak internal detail; log err server-side in real code.
			writeError(w, http.StatusInternalServerError, "internal error")
		}
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
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
	"strings"

	"example.com/adapter"
)

func main() {
	mux := http.NewServeMux()
	mux.Handle("GET /ok", adapter.Adapt(func(w http.ResponseWriter, r *http.Request) error {
		_, _ = w.Write([]byte("hello"))
		return nil
	}))
	mux.Handle("GET /missing", adapter.Adapt(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("user 7: %w", adapter.ErrNotFound)
	}))
	mux.Handle("GET /boom", adapter.Adapt(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("dial tcp 10.0.0.1:5432: connection refused")
	}))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	for _, path := range []string{"/ok", "/missing", "/boom"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			panic(err)
		}
		buf := make([]byte, 256)
		n, _ := resp.Body.Read(buf)
		_ = resp.Body.Close()
		body := strings.TrimRight(string(buf[:n]), "\n")
		fmt.Printf("%-9s %d %s\n", path, resp.StatusCode, body)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
/ok       200 hello
/missing  404 {"error":"user 7: not found"}
/boom     500 {"error":"internal error"}
```

The columns line up because `%-9s` left-pads each path to nine runes. Note the
`/boom` body is the generic `"internal error"`, not the internal
`connection refused` detail — the adapter refuses to leak it.

### Tests

Create `adapter_test.go`:

```go
package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(h HandlerFunc, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	Adapt(h).ServeHTTP(rec, req)
	return rec
}

func TestAdaptNilLeavesResponse(t *testing.T) {
	t.Parallel()
	rec := serve(func(w http.ResponseWriter, r *http.Request) error {
		_, _ = w.Write([]byte("hello"))
		return nil
	}, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q, want hello", rec.Body.String())
	}
}

func TestAdaptBadInput(t *testing.T) {
	t.Parallel()
	rec := serve(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("field name: %w", ErrBadInput)
	}, "/")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAdaptNotFound(t *testing.T) {
	t.Parallel()
	rec := serve(func(w http.ResponseWriter, r *http.Request) error {
		return ErrNotFound
	}, "/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAdaptUnclassifiedIs500AndDoesNotLeak(t *testing.T) {
	t.Parallel()
	rec := serve(func(w http.ResponseWriter, r *http.Request) error {
		return fmt.Errorf("dial tcp 10.0.0.1:5432: connection refused")
	}, "/")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["error"] != "internal error" {
		t.Fatalf("body error = %q, want generic 'internal error'", body["error"])
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Fatal("adapter leaked the internal error detail to the client")
	}
}

func TestAdaptSatisfiesHandlerOnMux(t *testing.T) {
	t.Parallel()
	// If Adapt did not return an http.Handler, this would not compile.
	var _ http.Handler = Adapt(func(w http.ResponseWriter, r *http.Request) error {
		return nil
	})

	mux := http.NewServeMux()
	mux.Handle("GET /health", Adapt(func(w http.ResponseWriter, r *http.Request) error {
		_, _ = w.Write([]byte("ok"))
		return nil
	}))
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("mounted handler: code=%d body=%q", rec.Code, rec.Body.String())
	}
}
```

## Review

The adapter is correct when a `nil` return leaves the handler's own `200` and body
untouched, each sentinel maps to its status, and an unclassified error becomes a
`500` whose body is the generic `"internal error"` with no internal detail leaked.
`TestAdaptUnclassifiedIs500AndDoesNotLeak` is the load-bearing test: it asserts the
`connection refused` string never reaches the client, which is why the default
branch writes a generic body instead of `err.Error()`.

The value of the named `HandlerFunc` type plus `Adapt` is that the error mapping
lives in exactly one place. The mistake it replaces is every handler writing its
own `http.Error(w, ..., 404)`, which drifts over time until different handlers
return different bodies for the same failure. Let handlers `return` an error and
map it once. `TestAdaptSatisfiesHandlerOnMux` confirms the adapted value is a real
`http.Handler` by mounting it on a `ServeMux`.

## Resources

- [net/http.Handler](https://pkg.go.dev/net/http#Handler) — the interface `Adapt` returns.
- [net/http.HandlerFunc](https://pkg.go.dev/net/http#HandlerFunc) — the stdlib adapter this pattern extends with an error return.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching a returned error against the status sentinels.
- [Error handling and Go](https://go.dev/blog/error-handling-and-go) — the original argument for handlers that return errors.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-parse-hostport-chained-returns.md](09-parse-hostport-chained-returns.md) | Next: [11-hostport-config-parser.md](11-hostport-config-parser.md)
