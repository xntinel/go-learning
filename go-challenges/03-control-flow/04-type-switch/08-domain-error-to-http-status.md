# Exercise 8: Map Domain Errors to HTTP Status Codes in a Handler

An HTTP handler catches an error from the domain layer and must translate it into
a status code and a safe client message. The errors are wrapped by the time they
reach the handler, so this is another place a bare type switch quietly fails and
`errors.As`/`errors.Is` are required. This module builds the error-mapping
middleware.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
errhttp/                     independent module: example.com/errhttp
  go.mod                     go 1.26
  errhttp.go                 typed domain errors; sentinels; ToStatus(err) (int, string)
  cmd/
    demo/
      main.go                runs an httptest handler and prints statuses
  errhttp_test.go            typed via errors.As, sentinels via errors.Is, unknown -> 500, no leak
```

- Files: `errhttp.go`, `cmd/demo/main.go`, `errhttp_test.go`.
- Implement: typed domain errors (`ValidationError`, `NotFoundError`,
  `ConflictError`), sentinels, and `ToStatus(err error) (int, string)` using
  `errors.As` for typed errors and `errors.Is` for sentinels.
- Test: each typed error (wrapped by `fmt.Errorf`) maps to its status via
  `errors.As`; sentinels map via `errors.Is`; an unknown error yields 500 with a
  generic message; a `ValidationError`'s field detail reaches the client while an
  internal error's does not.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/errhttp/cmd/demo
cd ~/go-exercises/errhttp
go mod init example.com/errhttp
```

## Why not a bare type switch here

The tempting shape is a type switch on the error:

```go
switch e := err.(type) { // BUG: misses wrapped errors
case *ValidationError:
	return http.StatusBadRequest, e.publicMessage()
case *NotFoundError:
	return http.StatusNotFound, "not found"
default:
	return http.StatusInternalServerError, "internal server error"
}
```

By the time a domain error reaches the handler it has usually been wrapped:
`fmt.Errorf("create user: %w", &ValidationError{...})`. The dynamic type is
`*fmt.wrapError`, so every concrete case is skipped and *every* error maps to 500
— including the validation errors the client needed to see. The correct mapping
uses `errors.As`, which unwraps until it finds a value assignable to the target,
and `errors.Is` for sentinels.

`ToStatus` returns both a status and a *client-safe* message. This split is the
security point: a `ValidationError` carries a field name and reason that are safe
and useful to return; an internal error's text (a SQL string, a stack detail) must
never reach the client, so the 500 branch returns a fixed generic message and
discards `err.Error()`. Leaking internal error text is a real vulnerability class,
and the mapper is where it is prevented.

Create `errhttp.go`:

```go
package errhttp

import (
	"errors"
	"fmt"
	"net/http"
)

// Typed domain errors carry client-safe detail.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: field %q %s", e.Field, e.Reason)
}

type NotFoundError struct{ Resource string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("not found: %s", e.Resource)
}

type ConflictError struct{ Detail string }

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict: %s", e.Detail)
}

// Sentinels for cross-cutting conditions.
var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

// ToStatus maps err to an HTTP status and a client-safe message. It uses
// errors.As / errors.Is so wrapped errors are still classified. Unknown errors
// map to 500 with a generic message and never leak internal detail.
func ToStatus(err error) (int, string) {
	if err == nil {
		return http.StatusOK, "OK"
	}

	var ve *ValidationError
	if errors.As(err, &ve) {
		return http.StatusBadRequest, fmt.Sprintf("invalid field %q: %s", ve.Field, ve.Reason)
	}
	var nfe *NotFoundError
	if errors.As(err, &nfe) {
		return http.StatusNotFound, fmt.Sprintf("%s not found", nfe.Resource)
	}
	var ce *ConflictError
	if errors.As(err, &ce) {
		return http.StatusConflict, "resource conflict"
	}

	switch {
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "forbidden"
	}

	// Unknown: never surface err.Error() to the client.
	return http.StatusInternalServerError, "internal server error"
}
```

## The runnable demo

The demo wires `ToStatus` into an `http.Handler` and drives it with `httptest` for
a few error shapes, printing the status line each returns.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"

	"example.com/errhttp"
)

func handlerFor(err error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status, msg := errhttp.ToStatus(err)
		w.WriteHeader(status)
		fmt.Fprint(w, msg)
	}
}

func main() {
	cases := []struct {
		name string
		err  error
	}{
		{"validation", fmt.Errorf("create user: %w", &errhttp.ValidationError{Field: "email", Reason: "is required"})},
		{"not found", fmt.Errorf("lookup: %w", &errhttp.NotFoundError{Resource: "order"})},
		{"unauthorized", errhttp.ErrUnauthorized},
		{"internal", fmt.Errorf("db: %w", fmt.Errorf("connection refused"))},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		handlerFor(c.err).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		fmt.Printf("%-13s -> %d %s\n", c.name, rec.Code, rec.Body.String())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
validation    -> 400 invalid field "email": is required
not found     -> 404 order not found
unauthorized  -> 401 unauthorized
internal      -> 500 internal server error
```

## Tests

The typed test wraps each domain error with `fmt.Errorf` and asserts the status
and client message come through `errors.As` despite the wrap. The sentinel test
covers `errors.Is`. The leak test asserts an internal error yields 500 with the
generic message and that the internal text does not appear in it.

Create `errhttp_test.go`:

```go
package errhttp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestToStatusTypedErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantInMsg  string
	}{
		{"validation", &ValidationError{Field: "email", Reason: "is required"}, http.StatusBadRequest, "email"},
		{"validation wrapped", fmt.Errorf("x: %w", &ValidationError{Field: "age", Reason: "too small"}), http.StatusBadRequest, "age"},
		{"not found", fmt.Errorf("x: %w", &NotFoundError{Resource: "order"}), http.StatusNotFound, "order"},
		{"conflict", fmt.Errorf("x: %w", &ConflictError{Detail: "dup key"}), http.StatusConflict, "conflict"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, msg := ToStatus(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if !strings.Contains(msg, tc.wantInMsg) {
				t.Errorf("msg = %q, want it to contain %q", msg, tc.wantInMsg)
			}
		})
	}
}

func TestToStatusSentinels(t *testing.T) {
	t.Parallel()
	if status, _ := ToStatus(fmt.Errorf("auth: %w", ErrUnauthorized)); status != http.StatusUnauthorized {
		t.Errorf("unauthorized status = %d, want 401", status)
	}
	if status, _ := ToStatus(ErrForbidden); status != http.StatusForbidden {
		t.Errorf("forbidden status = %d, want 403", status)
	}
}

func TestToStatusUnknownDoesNotLeak(t *testing.T) {
	t.Parallel()
	internal := fmt.Errorf("db query: %w", errors.New("SELECT secret FROM users"))
	status, msg := ToStatus(internal)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if strings.Contains(msg, "SELECT") || strings.Contains(msg, "secret") {
		t.Fatalf("client message leaked internal detail: %q", msg)
	}
	if msg != "internal server error" {
		t.Fatalf("msg = %q, want generic message", msg)
	}
}
```

## Review

The mapper is correct when a wrapped `*ValidationError` still yields 400 with its
field detail, when sentinels resolve via `errors.Is`, and when an unknown error
yields 500 with a fixed generic message that contains none of the internal text.
The bug this module exists to prevent is the bare type switch on the incoming
error: because handler errors are wrapped, the switch skips every concrete case
and collapses everything to 500, hiding the very validation errors clients need.
Use `errors.As` for typed errors and `errors.Is` for sentinels, and keep the 500
branch's message a constant so internal detail can never escape.

## Resources

- [errors.As](https://pkg.go.dev/errors#As)
- [errors.Is](https://pkg.go.dev/errors#Is)
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants)
- [net/http/httptest](https://pkg.go.dev/net/http/httptest)

---

Prev: [07-command-router-worker.md](07-command-router-worker.md) | Up: [00-concepts.md](00-concepts.md) | Next: [09-any-numeric-normalizer.md](09-any-numeric-normalizer.md)
