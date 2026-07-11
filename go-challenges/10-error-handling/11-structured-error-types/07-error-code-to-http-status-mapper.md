# Exercise 7: Mapping Domain Errors To HTTP Status In One Place

When every handler hardcodes its own status codes, the mapping drifts: one place
returns 404 for a missing row, another returns 400, a third leaks a 500. This
module centralizes the translation in one `StatusFromError` that middleware
calls, so a domain error deterministically becomes the right 4xx and an unknown
error becomes a *logged* 500 — never a silent success.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
statusmap/                 independent module: example.com/statusmap
  go.mod                   go 1.26
  statusmap.go             sentinels; StatusCoder; ValidationError; StatusFromError
  cmd/
    demo/
      main.go              runnable demo: map several errors to statuses
  statusmap_test.go        table: ValidationError->422, ErrNotFound->404, StatusCoder->own, boom->500+log
```

- Files: `statusmap.go`, `cmd/demo/main.go`, `statusmap_test.go`.
- Implement: `StatusFromError(err) (status int, shouldLog bool)` using `errors.As` for `*ValidationError` -> 422, a `StatusCoder` interface (`HTTPStatus() int`), `errors.Is` against `ErrNotFound`/`ErrConflict`/`ErrUnauthorized`, and a default of 500 flagged for logging.
- Test: `(*ValidationError)` -> 422; wrapped `ErrNotFound` -> 404; a `StatusCoder` type -> its own code; `fmt.Errorf("boom")` -> 500 with `shouldLog == true`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/statusmap/cmd/demo
cd ~/go-exercises/statusmap
go mod init example.com/statusmap
go mod edit -go=1.26
```

### One function, four strategies, a safe default

`StatusFromError` tries four strategies in order, each matching a different way a
domain can express its intent:

1. `errors.As` for `*ValidationError` -> 422. A validation failure is a concrete
   type we own, so we extract it by type.
2. `errors.As` for a `StatusCoder` (`HTTPStatus() int`). Some domain errors carry
   their own status — a `RateLimitError` that knows it is a 429 — so the mapper
   asks the error itself rather than hardcoding a table entry.
3. `errors.Is` against sentinels: `ErrNotFound` -> 404, `ErrConflict` -> 409,
   `ErrUnauthorized` -> 401. These are identity-based categories wrapped anywhere
   in the chain.
4. Default: 500, and `shouldLog = true`. This is the critical property. An error
   the mapper does not recognize is a *gap*, and a gap must be loud: it returns a
   server error and signals that the caller should log it. It must never fall
   through to a 200, and it must never silently swallow the error.

Returning `shouldLog` (rather than logging inside the mapper) keeps the function
pure and testable: the middleware decides how to log, the mapper decides only
*that* an unknown error is worth logging. Ordering matters — check the concrete
`*ValidationError` before the `StatusCoder` interface so a type that happened to
satisfy both would be treated as a validation error first; here they are
disjoint, but the order documents intent.

Create `statusmap.go`:

```go
package statusmap

import (
	"errors"
	"fmt"
	"net/http"
)

// Category sentinels, wrapped anywhere in a chain and matched with errors.Is.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnauthorized = errors.New("unauthorized")
)

// StatusCoder is implemented by domain errors that declare their own HTTP
// status.
type StatusCoder interface {
	HTTPStatus() int
}

type FieldError struct {
	Code  string
	Field string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Code }

type ValidationError struct {
	Errors []*FieldError
}

func (e *ValidationError) Error() string { return fmt.Sprintf("%d invalid field(s)", len(e.Errors)) }

// StatusFromError translates any error to an HTTP status. shouldLog is true only
// when the error is unrecognized and defaulted to 500, so a mapping gap is never
// silent.
func StatusFromError(err error) (status int, shouldLog bool) {
	if err == nil {
		return http.StatusOK, false
	}

	var ve *ValidationError
	if errors.As(err, &ve) {
		return http.StatusUnprocessableEntity, false
	}

	var sc StatusCoder
	if errors.As(err, &sc) {
		return sc.HTTPStatus(), false
	}

	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, false
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, false
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized, false
	}

	return http.StatusInternalServerError, true
}
```

### The runnable demo

The demo defines a `StatusCoder` (a rate-limit error that reports 429) and maps a
handful of errors to show each strategy firing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net/http"

	"example.com/statusmap"
)

type rateLimitError struct{}

func (rateLimitError) Error() string   { return "rate limited" }
func (rateLimitError) HTTPStatus() int { return http.StatusTooManyRequests }

func main() {
	cases := []error{
		&statusmap.ValidationError{Errors: []*statusmap.FieldError{{Code: "required", Field: "name"}}},
		fmt.Errorf("load user: %w", statusmap.ErrNotFound),
		rateLimitError{},
		fmt.Errorf("disk exploded"),
	}
	for _, err := range cases {
		status, log := statusmap.StatusFromError(err)
		fmt.Printf("%d log=%v\n", status, log)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
422 log=false
404 log=false
429 log=false
500 log=true
```

Only the last line flags logging. The rate-limit error resolves via the
`StatusCoder` branch (429, a recognized outcome), so `shouldLog` is false; only
the fully unrecognized `disk exploded` defaults to 500 and asks to be logged.

### Tests

The table drives each strategy and asserts both the status and the `shouldLog`
flag. The unknown-error row is the important one: it must be 500 *and* flag
logging, so a future unmapped error type cannot slip through as an unlogged
server error.

Create `statusmap_test.go`:

```go
package statusmap

import (
	"fmt"
	"net/http"
	"testing"
)

type teapotError struct{}

func (teapotError) Error() string   { return "i am a teapot" }
func (teapotError) HTTPStatus() int { return http.StatusTeapot }

func TestStatusFromError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		err     error
		want    int
		wantLog bool
	}{
		{
			name: "validation",
			err:  &ValidationError{Errors: []*FieldError{{Code: "required", Field: "name"}}},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "wrapped not found",
			err:  fmt.Errorf("load: %w", ErrNotFound),
			want: http.StatusNotFound,
		},
		{
			name: "wrapped conflict",
			err:  fmt.Errorf("insert: %w", ErrConflict),
			want: http.StatusConflict,
		},
		{
			name: "status coder",
			err:  teapotError{},
			want: http.StatusTeapot,
		},
		{
			name:    "unknown",
			err:     fmt.Errorf("boom"),
			want:    http.StatusInternalServerError,
			wantLog: true,
		},
		{
			name: "nil",
			err:  nil,
			want: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, log := StatusFromError(tt.err)
			if got != tt.want {
				t.Fatalf("status = %d, want %d", got, tt.want)
			}
			if log != tt.wantLog {
				t.Fatalf("shouldLog = %v, want %v", log, tt.wantLog)
			}
		})
	}
}
```

An `Example` verified against its `// Output:` comment:

```go
// statusmap_example_test.go
package statusmap

import (
	"fmt"
)

func ExampleStatusFromError() {
	status, log := StatusFromError(fmt.Errorf("unmapped"))
	fmt.Println(status, log)
	// Output: 500 true
}
```

## Review

The mapper is correct when each error kind resolves to a fixed status regardless
of how deep it is wrapped — `fmt.Errorf("load: %w", ErrNotFound)` is a 404 — and
when the one thing you cannot enumerate, the unknown error, defaults to 500 *and*
sets `shouldLog`. That default-plus-log is the whole safety argument: a mapping
gap surfaces as a logged server error, never as an unlogged 500 and never as a
200. Keep the mapper pure (return `shouldLog`, do not log inside it) so it is
trivially testable, and centralize it so no handler ever writes a status code
directly. Run `go test -race`.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) and [`errors.Is`](https://pkg.go.dev/errors#Is) — type extraction vs. sentinel identity.
- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusUnprocessableEntity`, `StatusNotFound`, and friends.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — the wrapping model the mapper relies on.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-validation-http-422-handler.md](06-validation-http-422-handler.md) | Next: [08-db-constraint-to-field-error.md](08-db-constraint-to-field-error.md)
