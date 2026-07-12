# Exercise 3: HTTP Handler: Centralized Error-to-Status Mapping

Every service eventually needs one place that turns an internal error into an HTTP
status. Scatter that decision across handlers as ad-hoc string checks and it drifts;
centralize it in one classifier and every endpoint answers consistently. This
module builds that classifier and a handler that uses it: it logs the full wrapped
chain internally and writes only a sanitized status and message to the client.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
httpstatus/                 independent module: example.com/httpstatus
  go.mod                    go 1.24
  httpstatus.go             sentinels, *ValidationError, errorToStatus, Handler
  cmd/
    demo/
      main.go               runnable demo: drive the handler with several errors
  httpstatus_test.go        httptest.NewRecorder per branch; deeply-wrapped sentinel
```

Files: `httpstatus.go`, `cmd/demo/main.go`, `httpstatus_test.go`.
Implement: `errorToStatus(err)` mapping domain sentinels and a typed `*ValidationError` and `context.DeadlineExceeded` to statuses, and a `Handler` that classifies, logs the full chain, and writes the sanitized result.
Test: `httptest.NewRecorder` driving the handler with an injected error per branch, asserting status and body; a deeply-wrapped sentinel; the default-500 case.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/06-error-wrapping-chains/03-http-error-to-status-mapping/cmd/demo
cd go-solutions/10-error-handling/06-error-wrapping-chains/03-http-error-to-status-mapping
go mod edit -go=1.24
```

### One classifier, two error idioms

`errorToStatus` is where `errors.Is` and `errors.As` play their complementary
roles. Sentinels are matched with `errors.Is` — "is this ultimately a not-found /
conflict / unauthorized?" — because the answer is an identity question and the
status is fixed. The `*ValidationError` is matched with `errors.As`, because here
we need the *data*: the field name and message go into a 422 body so the client
knows which field to fix. `context.DeadlineExceeded` maps to 504 (an upstream took
too long); everything unclassified falls through to 500.

Order matters. `errors.As` for the typed validation error comes first so a
validation failure is never mis-classified by a sentinel check that happens to also
be true. After that the sentinel `errors.Is` checks run, then the context deadline,
then the default. Each `case` is exclusive, and the classifier returns both the
status code and a *sanitized* public message — never `err.Error()`, which could
carry internal detail.

The handler separates the two audiences explicitly: it logs the full `err` (the
whole chain, for the operator) and writes only the classifier's public message to
the client. This is the security boundary from the concepts file made concrete: the
internal chain is a debugging surface, the response body is a public surface, and
they must not be the same string.

Create `httpstatus.go`:

```go
package httpstatus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// Domain sentinels the transport layer classifies on.
var (
	ErrNotFound     = errors.New("resource not found")
	ErrConflict     = errors.New("resource conflict")
	ErrUnauthorized = errors.New("unauthorized")
)

// ValidationError is a typed error carrying which field failed and why. It is
// extracted with errors.As so its data can shape a 422 response body.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed on %q: %s", e.Field, e.Msg)
}

// errorToStatus maps an error to an HTTP status and a client-safe message.
func errorToStatus(err error) (int, string) {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return http.StatusUnprocessableEntity, fmt.Sprintf("invalid field %q: %s", ve.Field, ve.Msg)
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "upstream timeout"
	default:
		return http.StatusInternalServerError, "internal server error"
	}
}

// Handler runs Op and, on error, classifies it: it logs the full chain and writes
// only the sanitized status and message to the client.
type Handler struct {
	Op  func(r *http.Request) error
	Log *slog.Logger
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := h.Op(r); err != nil {
		status, msg := errorToStatus(err)
		h.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
		http.Error(w, msg, status)
		return
	}
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, "ok")
}
```

### The runnable demo

The demo drives the handler with `httptest` for one representative error per branch
and prints the resulting status codes, so you see the whole mapping at a glance. It
sends the log to `io.Discard` to keep the output focused on statuses.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"

	"example.com/httpstatus"
)

func main() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	cases := []struct {
		name string
		err  error
	}{
		{"ok", nil},
		{"not-found", fmt.Errorf("load: %w", httpstatus.ErrNotFound)},
		{"conflict", httpstatus.ErrConflict},
		{"validation", &httpstatus.ValidationError{Field: "email", Msg: "required"}},
		{"deadline", fmt.Errorf("upstream: %w", context.DeadlineExceeded)},
		{"unknown", fmt.Errorf("disk on fire")},
	}

	for _, c := range cases {
		h := &httpstatus.Handler{Op: func(r *http.Request) error { return c.err }, Log: log}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		fmt.Printf("%-11s -> %d\n", c.name, rec.Code)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ok          -> 200
not-found   -> 404
conflict    -> 409
validation  -> 422
deadline    -> 504
unknown     -> 500
```

### Tests

The table drives the handler with one injected error per branch through a
`httptest.NewRecorder`, asserting both the status code and that the body carries the
sanitized message (and, for validation, the field). The `deeply wrapped not found`
row wraps `ErrNotFound` under three `%w` layers to prove classification survives
depth. The `unknown` row proves the default-500 path returns a generic message and
does not echo the raw error.

Create `httpstatus_test.go`:

```go
package httpstatus

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerStatusMapping(t *testing.T) {
	t.Parallel()

	deep := fmt.Errorf("a: %w", fmt.Errorf("b: %w", fmt.Errorf("c: %w", ErrNotFound)))

	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantBodyHas string
	}{
		{"nil ok", nil, http.StatusOK, "ok"},
		{"not found", ErrNotFound, http.StatusNotFound, "not found"},
		{"deeply wrapped not found", deep, http.StatusNotFound, "not found"},
		{"conflict", ErrConflict, http.StatusConflict, "conflict"},
		{"unauthorized", ErrUnauthorized, http.StatusUnauthorized, "unauthorized"},
		{"validation", &ValidationError{Field: "email", Msg: "required"}, http.StatusUnprocessableEntity, "email"},
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), http.StatusGatewayTimeout, "timeout"},
		{"unknown default 500", fmt.Errorf("boom"), http.StatusInternalServerError, "internal server error"},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &Handler{Op: func(r *http.Request) error { return tt.err }, Log: log}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBodyHas) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), tt.wantBodyHas)
			}
		})
	}
}

func TestClientNeverSeesRawError(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	secret := fmt.Errorf("pq: password authentication failed for user 'admin'")
	h := &Handler{Op: func(r *http.Request) error { return secret }, Log: log}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("response body leaked internal error: %q", rec.Body.String())
	}
}

func ExampleHandler() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &Handler{Op: func(r *http.Request) error { return ErrConflict }, Log: log}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	fmt.Println(rec.Code)
	// Output: 409
}
```

## Review

The classifier is correct when each error idiom drives the intended status: sentinels
through `errors.Is`, the typed `*ValidationError` through `errors.As` (with its field
reaching the body), `context.DeadlineExceeded` to 504, and anything unrecognized to a
generic 500. The `deeply wrapped not found` case is the load-bearing one — if it ever
returns 500, some layer wrapped with `%v` and severed the chain, and the classifier
can no longer see the sentinel. The `TestClientNeverSeesRawError` case guards the other
half: the response body must be the sanitized message, never `err.Error()`, so an
internal detail like a database auth message can never reach the client. Keep the
`errors.As` branch first so a validation error is extracted before any sentinel check.

## Resources

- [errors package](https://pkg.go.dev/errors) — `Is` for sentinels, `As` for typed extraction.
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusConflict`, `StatusUnprocessableEntity`, `StatusGatewayTimeout`.
- [net/http/httptest](https://pkg.go.dev/net/http/httptest) — `NewRecorder` and `NewRequest` for handler tests.
- [context.DeadlineExceeded](https://pkg.go.dev/context#pkg-variables) — the deadline sentinel mapped to 504.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-validation-join-aggregate.md](04-validation-join-aggregate.md)
