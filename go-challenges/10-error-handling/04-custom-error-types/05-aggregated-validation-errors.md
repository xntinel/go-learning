# Exercise 5: Collecting Every Field Failure with Unwrap() []error

Config validation fails fast (Exercise 1); form and API validation should fail
*collect-all*, returning every field error at once so the UI can show them
together. This module builds a request-body validator that returns a
`ValidationErrors` implementing `Unwrap() []error`, so a single returned error
still lets `errors.Is`/`errors.As` find any individual sentinel or typed field
error inside the tree — and it pins the classic typed-nil bug.

This module is fully self-contained: its own module, code, demo, and tests.
Nothing here imports any other exercise.

## What you'll build

```text
formvalidate/              independent module: example.com/formvalidate
  go.mod                   go 1.24
  formvalidate.go          FieldError, ValidationErrors (Unwrap []error), Validate
  cmd/
    demo/
      main.go              validates a body failing three rules, prints all
  formvalidate_test.go     all-three listed, Is each sentinel, As a field, nil path
```

Files: `formvalidate.go`, `cmd/demo/main.go`, `formvalidate_test.go`.
Implement: package sentinels, a `*FieldError`, a `*ValidationErrors` with `Error()` and `Unwrap() []error`, and `Validate(SignupForm)` that collects every failure.
Test: a body failing three rules lists all three; `errors.Is` finds each sentinel in the joined tree; `errors.As` extracts a specific `*FieldError`; a valid body returns an untyped nil (not an empty typed value).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/04-custom-error-types/05-aggregated-validation-errors/cmd/demo
cd go-solutions/10-error-handling/04-custom-error-types/05-aggregated-validation-errors
go mod edit -go=1.24
```

### Fail-collect vs fail-fast

A config loaded once at startup should fail fast: the first bad value stops the
boot, and the operator fixes it. A user submitting a signup form should get *every*
problem at once — a UX that reports "email invalid", then after a resubmit "password
too short", then "username taken" is hostile. So `Validate` here runs every rule,
appends a `*FieldError` per failure to a slice, and returns them all bundled.

The bundling type, `ValidationErrors`, implements `Unwrap() []error` (Go 1.20+),
the multi-child form of unwrap. That single method is what makes the aggregate
*searchable*: `errors.Is` and `errors.As` traverse the `[]error` tree depth-first,
so `errors.Is(errs, ErrRequired)` finds a required-field failure no matter which
branch holds it, and `errors.As(errs, &fe)` pulls out the first `*FieldError`. You
get one error value at the API boundary and full programmatic access to every
failure inside it. (`errors.Join` produces an equivalent searchable tree; here we
build the type by hand so we also control `Error()`'s formatting and, critically,
the nil contract below.)

### The typed-nil contract

The most important line in this module is the one that returns `nil`. A tempting
but wrong shape is:

```go
// WRONG: returns a non-nil error even when there are no failures.
func Validate(f SignupForm) error {
	errs := &ValidationErrors{}
	// ... no failures appended ...
	return errs // interface holds (*ValidationErrors)(non-nil-type, nil-ish value)
}
```

Returning the struct pointer unconditionally means a valid form yields a non-nil
`error` interface (its dynamic type is `*ValidationErrors`), so every caller's
`if err != nil` fires on success. The fix is to return an *untyped* `nil`
explicitly when the slice is empty, and only wrap into `*ValidationErrors` when
there is at least one failure. The test `TestValidBodyReturnsNil` locks this in
with `err != nil` — the check that a typed-nil bug would break.

Create `formvalidate.go`:

```go
// Package formvalidate validates a signup form collect-all: every failed rule is
// returned together inside a *ValidationErrors whose Unwrap() []error keeps each
// failure searchable by errors.Is/As.
package formvalidate

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels name the category of each field failure.
var (
	ErrRequired = errors.New("required")
	ErrInvalid  = errors.New("invalid")
	ErrTooShort = errors.New("too short")
)

// FieldError is a single field's failure, carrying the field and a wrapped
// sentinel.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return fmt.Sprintf("%s: %v", e.Field, e.Err) }
func (e *FieldError) Unwrap() error { return e.Err }

// ValidationErrors bundles every field failure. Its Unwrap() []error makes each
// child reachable by errors.Is/As traversal.
type ValidationErrors struct {
	Errs []error
}

func (e *ValidationErrors) Error() string {
	parts := make([]string, len(e.Errs))
	for i, err := range e.Errs {
		parts[i] = err.Error()
	}
	return fmt.Sprintf("%d validation error(s): %s", len(e.Errs), strings.Join(parts, "; "))
}

// Unwrap returns the children so errors.Is/As can traverse the tree.
func (e *ValidationErrors) Unwrap() []error { return e.Errs }

// SignupForm is the request body under validation.
type SignupForm struct {
	Username string
	Email    string
	Password string
}

// Validate runs every rule and returns all failures bundled, or an UNTYPED nil
// when the form is valid. Returning a typed nil here would make callers' err !=
// nil fire on success.
func (f SignupForm) Validate() error {
	var errs []error

	if f.Username == "" {
		errs = append(errs, &FieldError{Field: "username", Err: ErrRequired})
	}
	if !strings.Contains(f.Email, "@") {
		errs = append(errs, &FieldError{Field: "email", Err: ErrInvalid})
	}
	if len(f.Password) < 8 {
		errs = append(errs, &FieldError{Field: "password", Err: ErrTooShort})
	}

	if len(errs) == 0 {
		return nil // untyped nil: no typed-nil trap
	}
	return &ValidationErrors{Errs: errs}
}
```

### The runnable demo

The demo validates a body that breaks all three rules and prints the aggregate
message, then shows that a specific sentinel is still findable inside the bundle.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/formvalidate"
)

func main() {
	form := formvalidate.SignupForm{Username: "", Email: "no-at-sign", Password: "short"}

	err := form.Validate()
	if err == nil {
		fmt.Println("form valid")
		return
	}

	fmt.Println(err.Error())
	fmt.Printf("has required failure: %v\n", errors.Is(err, formvalidate.ErrRequired))
	fmt.Printf("has invalid failure: %v\n", errors.Is(err, formvalidate.ErrInvalid))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
3 validation error(s): username: required; email: invalid; password: too short
has required failure: true
has invalid failure: true
```

### Tests

`TestCollectsAll` proves all three failures are present in the message.
`TestIsFindsEachSentinel` proves `errors.Is` traverses the `[]error` tree to each
sentinel. `TestAsExtractsFieldError` proves a specific typed `*FieldError` is still
reachable. `TestValidBodyReturnsNil` is the typed-nil guard: a fully valid form
must return an untyped nil.

Create `formvalidate_test.go`:

```go
package formvalidate

import (
	"errors"
	"strings"
	"testing"
)

func TestCollectsAll(t *testing.T) {
	t.Parallel()
	err := SignupForm{Username: "", Email: "bad", Password: "x"}.Validate()
	if err == nil {
		t.Fatal("expected failures, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"username", "email", "password"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing field %q", msg, want)
		}
	}
}

func TestIsFindsEachSentinel(t *testing.T) {
	t.Parallel()
	err := SignupForm{Username: "", Email: "bad", Password: "x"}.Validate()

	for _, want := range []error{ErrRequired, ErrInvalid, ErrTooShort} {
		if !errors.Is(err, want) {
			t.Errorf("errors.Is did not find %v in the joined tree", want)
		}
	}
}

func TestAsExtractsFieldError(t *testing.T) {
	t.Parallel()
	err := SignupForm{Username: "", Email: "ok@x.com", Password: "longenough"}.Validate()

	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatal("errors.As should extract a *FieldError from the aggregate")
	}
	if fe.Field != "username" {
		t.Errorf("fe.Field = %q; want username", fe.Field)
	}
}

func TestValidBodyReturnsNil(t *testing.T) {
	t.Parallel()
	err := SignupForm{Username: "ada", Email: "ada@x.com", Password: "longenough"}.Validate()
	// The typed-nil guard: a valid form must be an UNTYPED nil.
	if err != nil {
		t.Fatalf("valid form returned non-nil error: %v (%T)", err, err)
	}
}

func TestErrorCountInMessage(t *testing.T) {
	t.Parallel()
	err := SignupForm{Username: "", Email: "bad", Password: "x"}.Validate()
	if !strings.HasPrefix(err.Error(), "3 validation error(s)") {
		t.Errorf("message %q should report a count of 3", err.Error())
	}
}
```

## Review

The aggregate is correct when it behaves as one error at the boundary and a
searchable tree inside. `Unwrap() []error` is the whole mechanism:
`TestIsFindsEachSentinel` proves `errors.Is` reaches every branch, and
`TestAsExtractsFieldError` proves a typed child is still extractable — you lose
nothing by bundling. `TestValidBodyReturnsNil` is the one that catches the most
dangerous bug in this shape: returning `&ValidationErrors{}` unconditionally makes
a valid form report as failed because the interface holds a non-nil dynamic type;
returning an untyped `nil` when the slice is empty is the fix. Contrast this with
Exercise 1's fail-fast validator: same sentinels, different UX contract. Run
`go test -race` to confirm.

## Resources

- [errors: Join](https://pkg.go.dev/errors#Join) — the stdlib builder of an `Unwrap() []error` tree.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — the introduction of `Unwrap() []error` and multi-error `Is`/`As`.
- [errors: Is / As](https://pkg.go.dev/errors#Is) — depth-first traversal over both `Unwrap` forms.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-http-apierror-status-mapping.md](04-http-apierror-status-mapping.md) | Next: [06-retryable-error-classification.md](06-retryable-error-classification.md)
