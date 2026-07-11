# Exercise 5: The Typed-Nil Error That Fails a Request on the Success Path

This is the most infamous bug in Go, and it is an interface bug: a validation
helper declares a typed error pointer, leaves it nil on the happy path, and
returns it as `error`. The returned interface is *not* nil — its type word is set —
so the handler's `if err != nil` fires and a perfectly valid request gets a 400.
This module builds the buggy helper and the corrected one side by side, and a test
that exposes the difference.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests.

## What you'll build

```text
validate/                     independent module: example.com/validate
  go.mod                      go 1.26
  validate.go                 ValidationError (error); validateBuggy (typed-nil trap); validateFixed
  handler.go                  SignupHandler using validateFixed -> 200/400
  cmd/
    demo/
      main.go                 runnable demo: buggy vs fixed on the same valid input
  validate_test.go            buggy returns non-nil on valid input; fixed returns nil; errors.As on the error path
```

- Files: `validate.go`, `handler.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: a `*ValidationError` satisfying `error`, a buggy `validateBuggy` that returns a typed nil, a correct `validateFixed` that returns literal `nil`, and a handler wired to the fixed version.
- Test: assert `validateBuggy` on valid input yields a non-nil `error` (the trap), `validateFixed` yields `err == nil`, and `errors.As` extracts `*ValidationError` on the error path.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/validate/cmd/demo
cd ~/go-exercises/validate
go mod init example.com/validate
```

### Why the buggy version fails on valid input

An interface value is two words: a type descriptor and a data pointer. `error` is
an interface. When a function declares `var e *ValidationError` and returns `e`,
the returned `error` has its type word set to `*ValidationError` and its value word
set to the nil pointer. That interface compares `!= nil` as *true*, because the
type word is non-nil — the interface holds "a nil `*ValidationError`", which is not
the same as "no error at all".

So this helper is wrong:

```go
// BUGGY: returns a typed nil on the happy path.
func validateBuggy(age int) error {
	var e *ValidationError   // e is nil
	if age < 0 {
		e = &ValidationError{Field: "age", Msg: "must be non-negative"}
	}
	return e                 // on the happy path, returns a non-nil interface
}
```

Call `validateBuggy(25)`: `age` is valid, `e` stays nil, and the returned `error`
interface is non-nil. In the handler, `if err != nil { http.Error(w, ..., 400) }`
fires and the valid signup is rejected with a 400. The request fails on the
success path — a bug that is maddening to diagnose because the value "looks nil" in
a debugger's field view but the interface is not.

The fix is to never assign a typed error pointer to a variable and return it on
the happy path. Return the literal `nil`:

```go
// FIXED: returns literal nil on success, a concrete error only on failure.
func validateFixed(age int) error {
	if age < 0 {
		return &ValidationError{Field: "age", Msg: "must be non-negative"}
	}
	return nil
}
```

`return nil` produces an `error` with both words nil, so `err != nil` is false and
the valid request proceeds. Both functions are in the module so a test can pin the
difference; production code keeps only the fixed shape.

Create `validate.go`:

```go
package validate

import "fmt"

// ValidationError is a concrete error type. It satisfies error implicitly.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Msg)
}

// validateBuggy demonstrates the typed-nil trap. On the happy path e stays nil
// but is returned through the error interface, so the returned error is NOT nil.
// This function exists to be tested against; do not write code this way.
func validateBuggy(age int) error {
	var e *ValidationError
	if age < 0 {
		e = &ValidationError{Field: "age", Msg: "must be non-negative"}
	}
	return e
}

// validateFixed returns literal nil on success and a concrete error only on
// failure, so the returned interface is genuinely nil for valid input.
func validateFixed(age int) error {
	if age < 0 {
		return &ValidationError{Field: "age", Msg: "must be non-negative"}
	}
	return nil
}
```

### The handler that would have been broken

Create `handler.go`. It uses `validateFixed`, so valid input gets 200. Wired to
`validateBuggy` instead, the identical valid request would get 400.

```go
package validate

import (
	"encoding/json"
	"errors"
	"net/http"
)

type signupRequest struct {
	Age int `json:"age"`
}

// SignupHandler validates the request and returns 200 on success, 400 on a
// validation error. It relies on validateFixed returning a genuinely nil error
// for valid input; the buggy version would 400 every valid signup.
func SignupHandler(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	if err := validateFixed(req.Age); err != nil {
		var ve *ValidationError
		if errors.As(err, &ve) {
			http.Error(w, ve.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("created"))
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

	"example.com/validate"
)

func main() {
	// A valid signup: age 25.
	body := `{"age":25}`

	req := httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(body))
	rec := httptest.NewRecorder()
	validate.SignupHandler(rec, req)

	fmt.Printf("valid signup -> %d\n", rec.Code)

	// An invalid signup: negative age.
	req = httptest.NewRequest(http.MethodPost, "/signup", strings.NewReader(`{"age":-3}`))
	rec = httptest.NewRecorder()
	validate.SignupHandler(rec, req)

	fmt.Printf("invalid signup -> %d (%s)\n", rec.Code, strings.TrimSpace(rec.Body.String()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid signup -> 200
invalid signup -> 400 (age: must be non-negative)
```

### Tests

`TestTypedNilTrap` is the exposé: it calls `validateBuggy(25)` on valid input and
asserts the returned `error` is *non-nil* — documenting the bug. `TestFixedIsNil`
asserts `validateFixed(25)` returns a genuinely nil error. `TestErrorPathExtracts`
confirms the error path still surfaces a `*ValidationError` via `errors.As`.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"testing"
)

func TestTypedNilTrap(t *testing.T) {
	t.Parallel()

	// Valid input, yet the buggy helper returns a NON-nil error interface: the
	// classic typed-nil trap. This is the bug, pinned so a "fix" that reverts
	// to the typed-nil pattern is caught.
	err := validateBuggy(25)
	if err == nil {
		t.Fatal("validateBuggy(25) returned nil; the typed-nil trap should make it non-nil")
	}
}

func TestFixedIsNil(t *testing.T) {
	t.Parallel()

	if err := validateFixed(25); err != nil {
		t.Fatalf("validateFixed(25) = %v, want nil", err)
	}
}

func TestErrorPathExtracts(t *testing.T) {
	t.Parallel()

	err := validateFixed(-1)
	if err == nil {
		t.Fatal("validateFixed(-1) = nil, want a validation error")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As failed to extract *ValidationError from %v", err)
	}
	if ve.Field != "age" {
		t.Fatalf("Field = %q, want age", ve.Field)
	}
}
```

## Review

The rule to internalize: an interface is a (type, value) pair, so a nil pointer
stored in an interface makes the interface non-nil. Never `return e` where `e` is
a typed nil error pointer; return the literal `nil` on the success path and
construct a concrete error only when there is one. `TestTypedNilTrap` deliberately
asserts the *broken* behavior so the trap is documented and a regression back to
the typed-nil pattern is caught, while `TestFixedIsNil` pins the correct behavior.
The subtle part is that this bug passes every unit test of `ValidationError`
itself, compiles cleanly, and only bites when the value flows through the `error`
interface — which is why it is a classic. On the error path, `errors.As` still
extracts the concrete type, so structured error handling is unaffected by the fix.
Run `go test -race` to confirm.

## Resources

- [The Go Blog: The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — an interface value is a (type, value) pair.
- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of this exact trap.
- [`errors.As`](https://pkg.go.dev/errors#As) — extracting a concrete error from a chain.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-interface-composition-reader-writer-deleter.md](06-interface-composition-reader-writer-deleter.md)
