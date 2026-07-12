# Exercise 5: Map A Wrapped Domain Error To An HTTP Status Code

Every HTTP handler needs one honest function: given an error bubbling up from the
service layer, what status code and body does the client get? The service wraps its
errors with `%w` on the way up, so the mapper must extract typed domain errors from
a chain with `errors.As`, not a raw assertion that only sees the top.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
httperr/                    independent module: example.com/httperr
  go.mod                    module path
  httperr.go                domain error types + StatusFor(err) (int, string)
  cmd/
    demo/
      main.go               runnable demo mapping several wrapped errors
  httperr_test.go           wrapped-chain table, raw-assertion control, sql.ErrNoRows
```

Files: `httperr.go`, `cmd/demo/main.go`, `httperr_test.go`.
Implement: `ValidationError`, `NotFoundError`, `ConflictError`, and `StatusFor(err) (int, string)` using `errors.As` and `errors.Is(sql.ErrNoRows)`.
Test: a table of wrapped errors to expected status, a two-level-deep `ValidationError`, a control showing a raw assertion misses the wrapped value, and an unknown error mapping to 500.
Verify: `go test -count=1 -race ./...`

### Why errors.As, not a raw assertion

The service layer does not hand you a bare `*ValidationError`. It returns
`fmt.Errorf("create user: %w", fmt.Errorf("validate: %w", &ValidationError{...}))`
— two layers of context wrapping. A raw `ve, ok := err.(*ValidationError)` inspects
only the outermost error, which is a `*fmt.wrapError`, so it fails and the handler
falls through to `500` on what is really a `400`. `errors.As(err, &ve)` walks the
`Unwrap` chain and binds the first `*ValidationError` it finds at any depth. That is
the whole reason the standard library gives you `As`: typed extraction through
wrapping.

`StatusFor` checks the specific typed errors first, most-specific to least, then
falls back to a sentinel check for `sql.ErrNoRows` (a repository that returns it
raw should surface as `404`), and finally a `500` default so an unclassified error
is still a valid response rather than a panic. Ordering matters only in that each
`errors.As` targets a distinct concrete type, so they do not overlap; the
`sql.ErrNoRows` check is `errors.Is` because it is a sentinel value, not a type.
The `ValidationError` carries a `Field`, so the mapper can return a message
precise enough to be actionable — the point of typed errors over string matching.

Create `httperr.go`:

```go
package httperr

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
)

// ValidationError is a 400: a specific field failed validation.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed on %q: %s", e.Field, e.Reason)
}

// NotFoundError is a 404.
type NotFoundError struct {
	Resource string
	ID       string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s %q not found", e.Resource, e.ID)
}

// ConflictError is a 409.
type ConflictError struct {
	Resource string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s already exists", e.Resource)
}

// StatusFor maps an error (possibly wrapped) to an HTTP status code and a client
// message. It uses errors.As to reach typed domain errors through the wrap chain.
func StatusFor(err error) (int, string) {
	if err == nil {
		return http.StatusOK, "ok"
	}

	var ve *ValidationError
	if errors.As(err, &ve) {
		return http.StatusBadRequest, ve.Error()
	}

	var nfe *NotFoundError
	if errors.As(err, &nfe) {
		return http.StatusNotFound, nfe.Error()
	}

	var ce *ConflictError
	if errors.As(err, &ce) {
		return http.StatusConflict, ce.Error()
	}

	if errors.Is(err, sql.ErrNoRows) {
		return http.StatusNotFound, "not found"
	}

	return http.StatusInternalServerError, "internal error"
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"database/sql"
	"errors"
	"fmt"

	"example.com/httperr"
)

func main() {
	errs := []error{
		fmt.Errorf("create user: %w", fmt.Errorf("validate: %w", &httperr.ValidationError{Field: "email", Reason: "required"})),
		fmt.Errorf("load: %w", &httperr.NotFoundError{Resource: "user", ID: "42"}),
		fmt.Errorf("insert: %w", &httperr.ConflictError{Resource: "user"}),
		fmt.Errorf("query: %w", sql.ErrNoRows),
		errors.New("disk on fire"),
	}
	for _, err := range errs {
		code, msg := httperr.StatusFor(err)
		fmt.Printf("%d %s\n", code, msg)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
400 validation failed on "email": required
404 user "42" not found
409 user already exists
404 not found
500 internal error
```

### Tests

The `two levels deep` case proves `errors.As` traverses the chain; the control test
proves a raw assertion on the same wrapped value does *not* find it (so you can see
exactly what `errors.As` buys). The table covers each status and the `500` default.

Create `httperr_test.go`:

```go
package httperr

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestStatusFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"validation", &ValidationError{Field: "x", Reason: "bad"}, http.StatusBadRequest},
		{"validation wrapped 2 deep", fmt.Errorf("a: %w", fmt.Errorf("b: %w", &ValidationError{Field: "x", Reason: "bad"})), http.StatusBadRequest},
		{"not found", &NotFoundError{Resource: "user", ID: "1"}, http.StatusNotFound},
		{"conflict", &ConflictError{Resource: "user"}, http.StatusConflict},
		{"sql no rows wrapped", fmt.Errorf("q: %w", sql.ErrNoRows), http.StatusNotFound},
		{"unknown", errors.New("boom"), http.StatusInternalServerError},
		{"nil", nil, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := StatusFor(tc.err)
			if got != tc.want {
				t.Fatalf("StatusFor(%q) = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestRawAssertionMissesWrappedError(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("ctx: %w", &ValidationError{Field: "email", Reason: "required"})

	// A raw assertion inspects only the outer *fmt.wrapError and misses it.
	if _, ok := wrapped.(*ValidationError); ok {
		t.Fatal("raw assertion unexpectedly matched the wrapped error")
	}
	// errors.As walks the chain and finds it.
	var ve *ValidationError
	if !errors.As(wrapped, &ve) {
		t.Fatal("errors.As failed to find *ValidationError in the chain")
	}
	if ve.Field != "email" {
		t.Fatalf("Field = %q, want email", ve.Field)
	}
}

func ExampleStatusFor() {
	err := fmt.Errorf("create: %w", &ConflictError{Resource: "user"})
	code, msg := StatusFor(err)
	fmt.Println(code, msg)
	// Output: 409 user already exists
}
```

## Review

The mapper is correct when each domain error resolves to its status through any
depth of wrapping, and an unclassified error becomes a clean `500` rather than a
panic. `TestRawAssertionMissesWrappedError` is the teaching centerpiece: the raw
assertion and `errors.As` are run against the exact same value, and only `As`
succeeds — that is why production error mapping must never use `err.(*T)`. Because
each `errors.As` targets a distinct type, their order is a readability choice, not a
correctness one; but the `sql.ErrNoRows` check must be `errors.Is`, since it is a
sentinel, not a type. Run `go test -race` to confirm the whole table.

## Resources

- [errors.As](https://pkg.go.dev/errors#As)
- [sql.ErrNoRows](https://pkg.go.dev/database/sql#pkg-variables)
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-dynamic-json-walker.md](04-dynamic-json-walker.md) | Next: [06-typed-nil-interface-guard.md](06-typed-nil-interface-guard.md)
