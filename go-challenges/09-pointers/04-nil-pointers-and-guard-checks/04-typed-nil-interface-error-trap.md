# Exercise 4: The Typed-Nil Interface — Why Your Nil Error Isn't Nil

This is the single most expensive nil bug in Go backends. A repository method
returns `error`, the author declares a `*AppError` variable and returns it, and
every caller's `if err != nil` fires on the success path because a typed nil
pointer stored in an interface is not a nil interface. This module reproduces the
trap in a realistic `Find(id)` and then eliminates it.

This module is fully self-contained.

## What you'll build

```text
typednil/                 independent module: example.com/typednil
  go.mod                  go 1.24
  repo.go                 type AppError; FindBuggy (the trap); FindFixed (correct)
  cmd/
    demo/
      main.go             runnable demo: same nil pointer, two interface results
  repo_test.go            asserts buggy err != nil, fixed err == nil, errors.As works
```

Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
Implement: an `*AppError` error type; `FindBuggy` that returns a `*AppError` variable (typed nil on success); `FindFixed` that returns the untyped `nil` on success.
Test: `FindBuggy` on a valid id yields a non-nil `error` (documenting the trap); `FindFixed` yields `err == nil`; a genuinely-returned `*AppError` still unwraps via `errors.As` from the fixed path.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/04-nil-pointers-and-guard-checks/04-typed-nil-interface-error-trap/cmd/demo
cd go-solutions/09-pointers/04-nil-pointers-and-guard-checks/04-typed-nil-interface-error-trap
go mod edit -go=1.24
```

### Interfaces are a (type, value) pair

An `error` value is an interface, and an interface value carries two words: a
dynamic type and a dynamic value. It compares equal to `nil` only when both words
are nil. When you assign a nil `*AppError` to an `error`, the type word becomes
`*AppError` and only the value word is nil — so the interface is `(*AppError,
nil)`, which is not `(nil, nil)`, and `err != nil` is true.

The buggy `Find` is the everyday shape of this bug. The author declares the error
as its concrete type up front, assigns it only on failure, and returns it on
every path:

```go
func FindBuggy(id string) (string, error) {
	var appErr *AppError // nil *AppError
	if id == "" {
		appErr = &AppError{Code: "invalid", Message: "empty id"}
		return "", appErr
	}
	return "user-" + id, appErr // returns a nil *AppError as a non-nil error
}
```

On the success path `appErr` is a nil `*AppError`, but the return type is
`error`, so the nil pointer is boxed into a non-nil interface. Every caller that
writes `if err != nil` now takes the error branch on success — logging phantom
failures, returning 500s for 200s, or aborting a transaction that committed.

The fix is discipline at the return, not a `reflect` patch at the caller. Return
the untyped `nil` literal on success and the concrete pointer only when there is
a real error:

```go
func FindFixed(id string) (string, error) {
	if id == "" {
		return "", &AppError{Code: "invalid", Message: "empty id"}
	}
	return "user-" + id, nil // untyped nil -> a genuinely nil interface
}
```

Now the success path returns `(nil, nil)` as an interface, `err == nil` is true,
and a real failure returns a non-nil `*AppError` that `errors.As` and `errors.Is`
handle normally.

Create `repo.go`:

```go
package typednil

import "reflect"

// AppError is a domain error type carried as an error interface.
type AppError struct {
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Code + ": " + e.Message }

// FindBuggy demonstrates the typed-nil interface trap: it declares the error as
// the concrete *AppError and returns that variable on every path, so the success
// path returns a nil *AppError boxed into a NON-nil error interface.
func FindBuggy(id string) (string, error) {
	var appErr *AppError
	if id == "" {
		appErr = &AppError{Code: "invalid", Message: "empty id"}
		return "", appErr
	}
	return "user-" + id, appErr
}

// FindFixed returns the untyped nil literal on success, so the returned error
// interface is genuinely nil, and returns a concrete *AppError only on failure.
func FindFixed(id string) (string, error) {
	if id == "" {
		return "", &AppError{Code: "invalid", Message: "empty id"}
	}
	return "user-" + id, nil
}

// underlyingIsNil reports whether an error interface holds a nil pointer value.
// It exists ONLY to illustrate the (type, value) split; do not use this pattern
// to "fix" a typed-nil interface in real code — fix the returning function.
func underlyingIsNil(err error) bool {
	if err == nil {
		return false // the interface itself is nil; there is no underlying pointer
	}
	v := reflect.ValueOf(err)
	return v.Kind() == reflect.Ptr && v.IsNil()
}
```

### The runnable demo

The demo shows the same nil `*AppError` producing two different interface
results, and uses `underlyingIsNil` to expose why: the buggy interface is
non-nil even though the pointer inside it is nil.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/typednil"
)

func main() {
	_, buggy := typednil.FindBuggy("42")
	fmt.Printf("FindBuggy(42): err != nil is %v\n", buggy != nil)

	_, fixed := typednil.FindFixed("42")
	fmt.Printf("FindFixed(42): err != nil is %v\n", fixed != nil)

	var ae *typednil.AppError
	_, err := typednil.FindFixed("")
	fmt.Printf("FindFixed(\"\"): errors.As *AppError is %v\n", asAppError(err, &ae))
}

func asAppError(err error, target **typednil.AppError) bool {
	if e, ok := err.(*typednil.AppError); ok {
		*target = e
		return true
	}
	return false
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
FindBuggy(42): err != nil is true
FindFixed(42): err != nil is false
FindFixed(""): errors.As *AppError is true
```

### Tests

The first test documents the trap by asserting the buggy path's error *is*
non-nil on success (with a comment so no one "fixes" it into a passing green that
hides the bug). The second asserts the fixed path is genuinely nil, and the third
proves a real `*AppError` still unwraps from the fixed path via `errors.As`.

Create `repo_test.go`:

```go
package typednil

import (
	"errors"
	"testing"
)

// TestBuggyReturnsNonNilOnSuccess documents the typed-nil trap: FindBuggy hands
// back a nil *AppError boxed in a non-nil error interface on the success path.
// This asserting != nil is the bug, captured so a refactor cannot reintroduce it.
func TestBuggyReturnsNonNilOnSuccess(t *testing.T) {
	t.Parallel()

	_, err := FindBuggy("42")
	if err == nil {
		t.Fatal("expected the buggy typed-nil interface to be non-nil; trap changed")
	}
	if !underlyingIsNil(err) {
		t.Fatal("expected the underlying *AppError pointer to be nil")
	}
}

func TestFixedReturnsNilOnSuccess(t *testing.T) {
	t.Parallel()

	id, err := FindFixed("42")
	if err != nil {
		t.Fatalf("FindFixed(42) err = %v, want nil", err)
	}
	if id != "user-42" {
		t.Fatalf("id = %q, want user-42", id)
	}
}

func TestFixedReturnsRealErrorThatUnwraps(t *testing.T) {
	t.Parallel()

	_, err := FindFixed("")
	if err == nil {
		t.Fatal("FindFixed(\"\") err = nil, want *AppError")
	}
	var ae *AppError
	if !errors.As(err, &ae) {
		t.Fatalf("errors.As did not extract *AppError from %v", err)
	}
	if ae.Code != "invalid" {
		t.Fatalf("Code = %q, want invalid", ae.Code)
	}
}

func TestFixedNilIsComparable(t *testing.T) {
	t.Parallel()

	_, err := FindFixed("7")
	// The idiomatic caller check must work correctly on the fixed path.
	if err != nil {
		t.Fatal("idiomatic if err != nil fired on a success path")
	}
}
```

## Review

The lesson is correct when `FindBuggy("42")` returns a non-nil error and
`FindFixed("42")` returns a nil error, from the *same* logical success. The
`underlyingIsNil` check makes the mechanism explicit: the buggy interface is
non-nil yet its pointer word is nil, which is the definition of the trap. The
`errors.As` test guards against overcorrecting — the fix must still let genuine
`*AppError` values propagate.

The mistakes this immunizes against: declaring the return as a concrete pointer
type and returning it on the success path, and reaching for `reflect` to paper
over the symptom at the caller instead of returning untyped nil at the source.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of the (type, value) pair.
- [Go Blog: The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — how an interface stores a (type, value) pair.
- [errors package](https://pkg.go.dev/errors#As) — `errors.As` and `errors.Is` for genuine error values.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-patch-handler-pointer-optional-fields.md](03-patch-handler-pointer-optional-fields.md) | Next: [05-nil-map-slice-accumulator-guard.md](05-nil-map-slice-accumulator-guard.md)
