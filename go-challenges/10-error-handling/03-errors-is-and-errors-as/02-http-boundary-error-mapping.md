# Exercise 2: Translate Domain Errors to HTTP Status at the Handler Boundary

The handler is the one place in a service where domain errors become transport
status codes. This exercise builds an `http.Handler` that calls a service through
an interface and maps each domain error to exactly one status — not-found to 404,
permission to 403, already-exists to 409, a validation error to 422 with a JSON
body — while defaulting everything unknown to 500 and logging the raw chain
instead of leaking it to the client.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
httpmap/                        independent module: example.com/httpmap
  go.mod                        go 1.25
  handler.go                    sentinels, ValidationError, ItemGetter, Handler.ServeHTTP
  handler_test.go               httptest table over injected error -> status + body
  cmd/demo/main.go              runnable demo hitting each mapped status
```

Files: `handler.go`, `handler_test.go`, `cmd/demo/main.go`.
Implement: a `Handler` wrapping an `ItemGetter` interface, mapping domain errors to status at ONE place using `errors.Is` for sentinels and `errors.As` for `*ValidationError`.
Test: `httptest.NewRecorder` + `httptest.NewRequest` over each injected error; assert `rec.Code` and that the 500 body does NOT contain the internal error text.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/03-errors-is-and-errors-as/02-http-boundary-error-mapping/cmd/demo
cd go-solutions/10-error-handling/03-errors-is-and-errors-as/02-http-boundary-error-mapping
go mod edit -go=1.25
```

### One boundary, one mapping, no leaks

The mapping from domain error to HTTP status lives in one function on the
handler. That is the whole discipline: no other layer knows about status codes,
and no `errors.Is` check for `ErrNotFound` appears anywhere except here. The
handler receives whatever error the service returns and answers three questions
in order. First, is it a known sentinel? `errors.Is` against `ErrNotFound`,
`ErrPermission`, `ErrAlreadyExists` picks 404, 403, 409. Second, is it a
validation failure carrying field detail? `errors.As` into `*ValidationError`
recovers the field and message so the handler can return 422 with a small JSON
body naming the bad field — the one case where returning error detail to the
client is safe, because a validation error is *about the client's input*. Third,
if it is neither, it is unknown: the handler logs the full raw chain server-side
and returns a bare 500 whose body says only "internal server error".

That last point is a security property, not a style choice. An unknown error can
carry a database DSN, a file path, an internal hostname — anything a lower layer
put in its message. Writing `err.Error()` into the response body leaks it to
whoever made the request. So the 500 branch logs the chain (where operators can
see it) and returns a fixed, information-free body. The test asserts exactly this:
the 500 response must not contain the injected internal text.

The service is reached through an `ItemGetter` interface with a single `Get`
method, which is what makes the handler testable: each test injects a fake
`ItemGetter` that returns the specific domain error under test, so every branch of
the mapping is exercised without a real service.

Create `handler.go`:

```go
package httpmap

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// Domain sentinels. The handler is the ONLY place that maps these to status.
var (
	ErrNotFound      = errors.New("not found")
	ErrPermission    = errors.New("permission denied")
	ErrAlreadyExists = errors.New("already exists")
)

// ValidationError carries client-facing field detail; it maps to 422 and its
// message is safe to return in the body.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return "validation failed on " + e.Field + ": " + e.Message
}

type Item struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
}

// ItemGetter is the service seam the handler depends on. Tests inject a fake.
type ItemGetter interface {
	Get(caller, id string) (Item, error)
}

type Handler struct {
	Service ItemGetter
	Logger  *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	caller := r.Header.Get("X-Caller")
	id := r.URL.Query().Get("id")

	it, err := h.Service.Get(caller, id)
	if err != nil {
		h.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(it)
}

// writeError is the single translation boundary from domain error to HTTP status.
func (h *Handler) writeError(w http.ResponseWriter, err error) {
	// Field-level validation is the one case whose detail is safe to return.
	var ve *ValidationError
	if errors.As(err, &ve) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"field":   ve.Field,
			"message": ve.Message,
		})
		return
	}

	switch {
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrPermission):
		http.Error(w, "forbidden", http.StatusForbidden)
	case errors.Is(err, ErrAlreadyExists):
		http.Error(w, "conflict", http.StatusConflict)
	default:
		// Unknown: log the raw chain, return an information-free body.
		h.Logger.Error("unhandled service error", slog.Any("err", err))
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
```

### The runnable demo

The demo wires the handler to a tiny fake service and issues four requests
through `httptest`, printing the status each domain error maps to.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"

	"example.com/httpmap"
)

type fakeService struct{ err error }

func (f fakeService) Get(caller, id string) (httpmap.Item, error) {
	if f.err != nil {
		return httpmap.Item{}, f.err
	}
	return httpmap.Item{ID: id, Owner: caller}, nil
}

func main() {
	cases := []struct {
		name string
		err  error
	}{
		{"ok", nil},
		{"missing", fmt.Errorf("get x: %w", httpmap.ErrNotFound)},
		{"forbidden", fmt.Errorf("get x: %w", httpmap.ErrPermission)},
		{"invalid", &httpmap.ValidationError{Field: "id", Message: "must not be empty"}},
		{"boom", errors.New("connection refused to db-primary.internal:5432")},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, c := range cases {
		h := &httpmap.Handler{Service: fakeService{err: c.err}, Logger: logger}
		req := httptest.NewRequest("GET", "/item?id=x", nil)
		req.Header.Set("X-Caller", "alice")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		fmt.Printf("%-10s -> %d %s\n", c.name, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ok         -> 200 {"id":"x","owner":"alice"}
missing    -> 404 not found
forbidden  -> 403 forbidden
invalid    -> 422 {"field":"id","message":"must not be empty"}
boom       -> 500 internal server error
```

### Tests

The test injects each domain error through the fake service and asserts the
status and a body substring. The most important row is `boom`: the injected error
message names an internal host, and the test asserts the 500 body does *not*
contain that string — proving the handler does not leak the raw chain.

Create `handler_test.go`:

```go
package httpmap

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeService struct{ err error }

func (f fakeService) Get(caller, id string) (Item, error) {
	if f.err != nil {
		return Item{}, f.err
	}
	return Item{ID: id, Owner: caller}, nil
}

func TestErrorMapping(t *testing.T) {
	t.Parallel()
	const secret = "db-primary.internal:5432"
	tests := []struct {
		name       string
		injected   error
		wantStatus int
		wantBody   string
	}{
		{"ok", nil, 200, `"id":"x"`},
		{"not found", fmt.Errorf("svc: %w", ErrNotFound), 404, "not found"},
		{"permission", fmt.Errorf("svc: %w", ErrPermission), 403, "forbidden"},
		{"already exists", fmt.Errorf("svc: %w", ErrAlreadyExists), 409, "conflict"},
		{"validation", &ValidationError{Field: "id", Message: "empty"}, 422, `"field":"id"`},
		{"unknown", errors.New("connection refused to " + secret), 500, "internal server error"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{Service: fakeService{err: tc.injected}, Logger: logger}
			req := httptest.NewRequest("GET", "/item?id=x", nil)
			req.Header.Set("X-Caller", "alice")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body %q does not contain %q", rec.Body.String(), tc.wantBody)
			}
			if tc.name == "unknown" && strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("500 body leaked internal detail %q", secret)
			}
		})
	}
}

func TestValidationBodyIsJSON(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &Handler{Service: fakeService{err: &ValidationError{Field: "email", Message: "bad"}}, Logger: logger}
	req := httptest.NewRequest("GET", "/item?id=x", nil)
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}

func Example() {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &Handler{Service: fakeService{err: fmt.Errorf("svc: %w", ErrNotFound)}, Logger: logger}
	req := httptest.NewRequest("GET", "/item?id=x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	fmt.Println(rec.Code)
	// Output: 404
}
```

## Review

The handler is correct when the mapping is total and lives in one place: every
domain error resolves to exactly one status, and adding a new sentinel means
adding one `case` here and nowhere else. The load-bearing property is that
`errors.As` for `*ValidationError` runs *before* the sentinel switch, because a
validation error is the only one whose detail is safe to surface — everything
that falls through to `default` is treated as untrusted and its body is fixed
text. The mistake to avoid is writing `err.Error()` into the 500 response "to
help debugging": that leaks internals to clients; log the chain with
`slog.Any("err", err)` server-side instead. Because the service is an interface,
the tests inject each error directly with `httptest` and never need a real
backend. Run `go test -race` to confirm the handler is safe under concurrent
requests.

## Resources

- [net/http](https://pkg.go.dev/net/http) — `Handler`, `ResponseWriter.WriteHeader`, `http.Error`.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder`, `NewRequest`.
- [errors.As](https://pkg.go.dev/errors#As) — recovering the typed `*ValidationError`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-service-layer-is-as.md](01-service-layer-is-as.md) | Next: [03-repository-driver-error-translation.md](03-repository-driver-error-translation.md)
