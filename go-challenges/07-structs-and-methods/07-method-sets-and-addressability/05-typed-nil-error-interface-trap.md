# Exercise 5: The Typed-Nil Error Trap in an HTTP Handler

An interface value is a `(dynamic type, dynamic value)` pair, and it is nil only
when both halves are nil. Return a nil `*AppError` as an `error` and the interface
carries a non-nil type with a nil value, so `if err != nil` fires on success —
the handler emits a 500 for a request that actually worked. This module
reproduces that bug, fixes it, and pins the fix with an `httptest` handler test.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
apihandler/                    independent module: example.com/apihandler
  go.mod                       module path + go directive
  handler.go                   AppError; validateBuggy (trap) and validateFixed; HTTP handler
  cmd/
    demo/
      main.go                  show the trap firing, then the fix
  handler_test.go              trap test, fix test, httptest success path returns 200
```

- Files: `handler.go`, `cmd/demo/main.go`, `handler_test.go`.
- Implement: a `*AppError` with `Error()` on the pointer receiver and an HTTP status; a buggy validator returning a typed-nil `*AppError`; a fixed validator returning a literal `nil`; a handler using the fixed one.
- Test: prove the buggy function yields `err != nil`, the fixed one yields `err == nil`, and the handler's success path returns 200 (not a phantom 500).
- Verify: `go vet ./...`, `go test -count=1 -race ./...`.

### Why a typed-nil pointer is a non-nil error

`error` is an interface. A value of an interface type stores two words: the
concrete (dynamic) type and a pointer to the value. It compares equal to `nil`
only when both words are nil — that is, when nothing has ever been assigned to it.
The moment you assign a concrete typed value, even a nil pointer, the type word is
set, and the interface is no longer nil.

Now the trap. A validator declares a local `var e *AppError`, sets it only on the
failure path, and returns `e`:

```go
func validateBuggy(name string) error {
	var e *AppError
	if name == "" {
		e = &AppError{Status: 400, Msg: "name required"}
	}
	return e // e is (*AppError)(nil) on the happy path, widened to a non-nil error
}
```

On the happy path `e` is a nil `*AppError`. Returning it converts it to an `error`
interface whose type word is `*AppError` and whose value word is nil — a non-nil
interface. The caller's `if err != nil` fires, and a handler built on this
validator returns a 500 for a perfectly valid request. Worse, logging `err` may
print the result of `Error()` called on a nil pointer, which panics unless
`Error()` is written to tolerate a nil receiver.

The fix is to never widen a typed nil. Declare the function to return `error` and
return a literal `nil` on success, constructing the `*AppError` only where you
actually have an error:

```go
func validateFixed(name string) error {
	if name == "" {
		return &AppError{Status: 400, Msg: "name required"}
	}
	return nil // a literal nil interface: both words nil
}
```

`errors.As` recovers the concrete `*AppError` when you need its status code, and
because `validateFixed` returns a literal nil on success, `if err != nil` means
exactly "a real error occurred".

Create `handler.go`:

```go
package apihandler

import (
	"errors"
	"net/http"
)

// AppError is a domain error carrying an HTTP status. Error() is on the pointer
// receiver, so the concrete error type is *AppError.
type AppError struct {
	Status int
	Msg    string
}

func (e *AppError) Error() string { return e.Msg }

// validateBuggy demonstrates the typed-nil trap: it returns a typed-nil
// *AppError widened to error, so the returned interface is non-nil even when
// validation passed. Kept to prove the bug in a test; NOT used by the handler.
func validateBuggy(name string) error {
	var e *AppError
	if name == "" {
		e = &AppError{Status: http.StatusBadRequest, Msg: "name required"}
	}
	return e
}

// validateFixed returns a literal nil interface on success, so err != nil means
// a real error. This is the version the handler uses.
func validateFixed(name string) error {
	if name == "" {
		return &AppError{Status: http.StatusBadRequest, Msg: "name required"}
	}
	return nil
}

// Handler validates the "name" query parameter and responds. On a real error it
// maps the *AppError's status; on success it returns 200.
func Handler(w http.ResponseWriter, r *http.Request) {
	if err := validateFixed(r.URL.Query().Get("name")); err != nil {
		status := http.StatusInternalServerError
		var appErr *AppError
		if errors.As(err, &appErr) {
			status = appErr.Status
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
```

### The runnable demo

The demo calls both validators on valid input and prints whether each reports an
error, making the trap visible: the buggy one claims an error on valid input, the
fixed one does not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/apihandler"
)

func main() {
	// Valid input; neither should report an error, but the buggy one does.
	fmt.Printf("buggy valid: err != nil = %v\n", apihandler.ReportBuggy("alice"))
	fmt.Printf("fixed valid: err != nil = %v\n", apihandler.ReportFixed("alice"))
	fmt.Printf("fixed empty: err != nil = %v\n", apihandler.ReportFixed(""))
}
```

To let the demo (a separate `package main`) observe the trap without exporting the
validators, add two thin exported reporters. Append to `handler.go`:

```go
// ReportBuggy reports whether validateBuggy's result is non-nil (the trap).
func ReportBuggy(name string) bool { return validateBuggy(name) != nil }

// ReportFixed reports whether validateFixed's result is non-nil.
func ReportFixed(name string) bool { return validateFixed(name) != nil }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy valid: err != nil = true
fixed valid: err != nil = false
fixed empty: err != nil = true
```

### Tests

One test proves the trap (the buggy validator yields `err != nil` on valid input),
one proves the fix (the fixed validator yields `err == nil`), and an `httptest`
test drives the handler's success path and asserts a 200 rather than a phantom
500. A second handler case asserts the real-error path maps the `*AppError`
status.

Create `handler_test.go`:

```go
package apihandler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTypedNilTrapFires(t *testing.T) {
	t.Parallel()
	// Valid input: validation passed, yet the buggy validator's error is non-nil.
	if err := validateBuggy("alice"); err == nil {
		t.Fatal("expected the typed-nil trap: validateBuggy should return non-nil")
	}
}

func TestFixedValidatorReturnsNil(t *testing.T) {
	t.Parallel()
	if err := validateFixed("alice"); err != nil {
		t.Fatalf("validateFixed(valid) = %v, want nil", err)
	}
	err := validateFixed("")
	var appErr *AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest {
		t.Fatalf("validateFixed(\"\") = %v, want *AppError status 400", err)
	}
}

func TestHandlerSuccessReturns200(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/user?name=alice", nil)
	rec := httptest.NewRecorder()

	Handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (phantom 500 means the typed-nil trap)", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q, want ok", rec.Body.String())
	}
}

func TestHandlerBadRequestMapsStatus(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/user", nil) // no name
	rec := httptest.NewRecorder()

	Handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
```

## Review

The handler is correct when `err != nil` means a real error. The success-path test
asserting 200 is the proof: it passes only because `validateFixed` returns a
literal `nil` on success. Swap in `validateBuggy` and that test flips to a 500, because
the typed-nil `*AppError` widens to a non-nil interface and the handler takes the
error branch.

The mistake this module exists to prevent is returning a typed nil pointer as an
error — declaring a `var e *AppError`, leaving it nil on success, and returning
it. Return the interface type `error` and a literal `nil`; construct the concrete
error only where one exists. Use `errors.As` to recover the concrete type for
status mapping. If you must keep a typed pointer around, convert to `error` at the
boundary with an explicit nil check. Run `go vet` (its nilness-adjacent checks) and
`go test -race`.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of the typed-nil trap.
- [`errors.As`](https://pkg.go.dev/errors#As) — recovering the concrete `*AppError` from an error.
- [`net/http/httptest`](https://pkg.go.dev/net/http/httptest) — `NewRequest` and `NewRecorder` for driving a handler in a test.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-immutable-money-value-receivers.md](06-immutable-money-value-receivers.md)
