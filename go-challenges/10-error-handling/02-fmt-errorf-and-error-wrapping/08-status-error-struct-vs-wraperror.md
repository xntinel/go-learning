# Exercise 8: Build a typed StatusError struct that both carries data and participates in the chain

Sometimes a caller needs more than a message and a cause — it needs *structured*
data, like the HTTP status a middleware should return. An opaque `*fmt.wrapError`
cannot carry that; a custom struct can. This exercise builds a `*StatusError` with
an exported `Code` and `Op` plus `Unwrap() error`, so `errors.As` hands a
middleware the code *and* `errors.Is` still finds the wrapped domain sentinel — and
contrasts it with a plain `fmt.Errorf` wrap where `errors.As(*StatusError)` fails.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
apierr/                        independent module: example.com/apierr
  go.mod                       go 1.24
  apierr.go                    ErrNotFound; *StatusError{Code,Op,Err} with Error/Unwrap; NewStatusError; HTTPStatus
  apierr_test.go               As extracts Code+Op; Is still finds sentinel; plain wrap has no Code; middleware mapping
  cmd/
    demo/
      main.go                  maps a StatusError and a plain error to HTTP statuses
```

- Files: `apierr.go`, `cmd/demo/main.go`, `apierr_test.go`.
- Implement: `*StatusError{Code, Op, Err}` with `Error()` and `Unwrap() error`, a `NewStatusError` constructor, and `HTTPStatus(err) int` that maps via `errors.As` (never string comparison), defaulting to 500.
- Test: `errors.As` extracts `*StatusError` with the right `Code`/`Op` while `errors.Is` still finds the wrapped sentinel; a plain `fmt.Errorf` wrap yields `errors.As(*StatusError) == false`; `HTTPStatus` returns the code for a `StatusError` (even when further wrapped) and 500 for an unmapped error.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/apierr/cmd/demo
cd ~/go-exercises/apierr
go mod init example.com/apierr
```

### Struct versus opaque wrapError

`*StatusError` implements both halves of the error contract. `Error()` renders a
message; `Unwrap() error` returns the wrapped cause so the struct participates in
the chain. That dual nature is the whole point: because it carries exported fields,
`errors.As(err, &se)` gives a middleware the `Code` and `Op` as *data*; because it
unwraps, `errors.Is(err, ErrNotFound)` still finds the domain sentinel underneath.
An opaque `fmt.Errorf("get user: %w", ErrNotFound)` gives you only the second
property — the message and the cause — so `errors.As(err, &se)` returns false. When
a caller needs to *read a field*, the struct wins; when it only needs the message
and cause, `fmt.Errorf` is lighter and sufficient.

`HTTPStatus` is the middleware: it maps any error to a status using `errors.As`,
not a string comparison. Crucially, `errors.As` traverses the chain, so a
`*StatusError` that has itself been wrapped further up (say `fmt.Errorf("handler:
%w", statusErr)`) is still found and still yields its `Code`. Any error that is not
a `*StatusError` anywhere in its chain falls through to `http.StatusInternalServerError`
(500). No branch inspects `err.Error()`.

Create `apierr.go`:

```go
package apierr

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrNotFound is a domain sentinel a StatusError can wrap.
var ErrNotFound = errors.New("not found")

// StatusError carries an HTTP status and operation alongside a wrapped cause. It
// implements Unwrap so errors.Is still finds that cause.
type StatusError struct {
	Code int
	Op   string
	Err  error
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %s (status %d)", e.Op, e.Err, e.Code)
}

func (e *StatusError) Unwrap() error {
	return e.Err
}

// NewStatusError wraps cause with an HTTP status and operation name.
func NewStatusError(code int, op string, cause error) *StatusError {
	return &StatusError{Code: code, Op: op, Err: cause}
}

// HTTPStatus maps any error to a status code via errors.As, never by comparing
// message strings. An error with no *StatusError in its chain is a 500.
func HTTPStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return http.StatusInternalServerError
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"net/http"

	"example.com/apierr"
)

func main() {
	statusErr := apierr.NewStatusError(http.StatusNotFound, "get user 42", apierr.ErrNotFound)
	fmt.Printf("StatusError: %v\n", statusErr)
	fmt.Printf("  HTTPStatus=%d is ErrNotFound=%v\n",
		apierr.HTTPStatus(statusErr), errors.Is(statusErr, apierr.ErrNotFound))

	wrapped := fmt.Errorf("handler: %w", statusErr)
	fmt.Printf("wrapped StatusError: HTTPStatus=%d\n", apierr.HTTPStatus(wrapped))

	plain := fmt.Errorf("get user: %w", apierr.ErrNotFound)
	var se *apierr.StatusError
	fmt.Printf("plain wrap: HTTPStatus=%d as *StatusError=%v\n",
		apierr.HTTPStatus(plain), errors.As(plain, &se))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
StatusError: get user 42: not found (status 404)
  HTTPStatus=404 is ErrNotFound=true
wrapped StatusError: HTTPStatus=404
plain wrap: HTTPStatus=500 as *StatusError=false
```

### Tests

The tests prove the struct's dual nature (`As` yields `Code`/`Op`, `Is` still finds
the sentinel), the contrast with a plain wrap, and the middleware mapping including
the deeper-wrapped case — all via `errors.As`/`errors.Is`, never string compare.

Create `apierr_test.go`:

```go
package apierr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestStatusErrorCarriesDataAndUnwraps(t *testing.T) {
	t.Parallel()

	err := NewStatusError(http.StatusNotFound, "get user 42", ErrNotFound)

	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatal("errors.As should extract *StatusError")
	}
	if se.Code != http.StatusNotFound {
		t.Fatalf("Code = %d, want 404", se.Code)
	}
	if se.Op != "get user 42" {
		t.Fatalf("Op = %q, want get user 42", se.Op)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("errors.Is should still find the wrapped sentinel")
	}
}

func TestPlainWrapHasNoStatusError(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("get user: %w", ErrNotFound)

	var se *StatusError
	if errors.As(err, &se) {
		t.Fatal("plain fmt.Errorf wrap should not be a *StatusError")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("plain wrap should still find the sentinel")
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, http.StatusOK},
		{"status error", NewStatusError(http.StatusForbidden, "op", ErrNotFound), http.StatusForbidden},
		{"wrapped status error", fmt.Errorf("handler: %w", NewStatusError(http.StatusNotFound, "op", ErrNotFound)), http.StatusNotFound},
		{"unmapped", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := HTTPStatus(tc.err); got != tc.want {
				t.Fatalf("HTTPStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
```

## Review

`*StatusError` is correct when it satisfies both queries a real middleware makes:
`errors.As` recovers the `Code` and `Op` even through further wrapping, and
`errors.Is` still finds the domain sentinel it wraps. The contrast test is the
teaching point — a plain `fmt.Errorf` wrap carries the same message and cause but
cannot answer `errors.As(*StatusError)`, so the choice between them is driven by
whether a caller needs structured data or just a cause. The rule the mapping test
enforces is that status selection goes through `errors.As`, never through
`strings.Contains(err.Error(), "not found")`; the latter is the fragile approach
that breaks the day someone rewords the message, and it is precisely what a typed
error exists to replace.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — recovering a typed error from a chain.
- [errors package](https://pkg.go.dev/errors) — `Is`/`As` and the `Unwrap` interface.
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusInternalServerError`.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — custom error types with `Unwrap`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-error-message-hygiene-contract.md](07-error-message-hygiene-contract.md) | Next: [../03-errors-is-and-errors-as/00-concepts.md](../03-errors-is-and-errors-as/00-concepts.md)
