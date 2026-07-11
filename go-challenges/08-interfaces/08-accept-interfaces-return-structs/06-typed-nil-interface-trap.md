# Exercise 6: Debug The Typed-Nil Interface Return Bug

The return-structs half of the rule has a sharp edge worth feeling directly. This
module reproduces a real incident: a validator that declares a concrete
`*ValidationError` return, returns a nil one on the happy path, and thereby produces a
non-nil `error` at every call site — silently reporting failures that never happened.

This module is fully self-contained. It has its own `go mod init`, defines every
type it needs, and ships its own demo and tests. Nothing here imports any other
exercise.

## What you'll build

```text
typednil/                   independent module: example.com/typednil
  go.mod                    go 1.26
  validate.go               ValidationError; ValidateBuggy (typed-nil bug) and ValidateFixed
  cmd/
    demo/
      main.go               prints how the two behave differently on valid input
  validate_test.go          pins the broken contract AND the corrected one
```

Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: a `ValidationError`; a `ValidateBuggy` that returns a concrete `*ValidationError` typed as `error` (typed nil on success); a `ValidateFixed` that returns the `error` interface as literal nil on success.
Test: assert `ValidateBuggy("ok")` unexpectedly yields `err != nil`, and diagnose with `reflect.ValueOf(err).IsNil()`; assert `ValidateFixed("ok")` yields `err == nil`; assert both surface a real `*ValidationError` on bad input via `errors.As`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/typednil/cmd/demo
cd ~/go-exercises/typednil
go mod init example.com/typednil
go mod edit -go=1.26
```

### Why a nil pointer becomes a non-nil error

An interface value is a pair: a dynamic type and a dynamic value. It equals nil only
when *both* halves are nil. `ValidateBuggy` declares a local `var vErr *ValidationError`,
leaves it nil on the happy path, and `return vErr`. Because the function's declared
return type is `error`, that return converts the `*ValidationError` (nil) into an
`error` whose dynamic *type* is `*ValidationError` and whose dynamic *value* is nil.
The type half is not nil, so the interface is not nil. Every caller's `if err != nil`
is true, and every caller acts on a validation failure that did not occur.

`ValidateFixed` does the one thing that avoids the whole class: on success it returns
the `error` interface as a literal `nil` (`return nil`), so both halves are nil and the
interface truly equals nil. The rule "return structs" read precisely means: do not let
a concrete typed nil escape through an interface return; return `nil` through the
interface, or hand back the concrete type and let the caller narrow. `reflect.ValueOf
(err).IsNil()` can *detect* the bad state after the fact — it reports true for the typed
nil — but it is a diagnostic, not a fix. The fix is to never build the typed nil.

Create `validate.go`. Both functions are real and tested; the buggy one is named so it
can be pinned, not corrected in place:

```go
package typednil

import "fmt"

// ValidationError describes why a value failed validation.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: %s %s", e.Field, e.Reason)
}

// ValidateBuggy has the typed-nil bug. It builds a *ValidationError variable, leaves
// it nil when name is valid, and returns it through the error interface. The returned
// error therefore has dynamic type *ValidationError and value nil: it is NOT nil.
func ValidateBuggy(name string) error {
	var vErr *ValidationError
	if name == "" {
		vErr = &ValidationError{Field: "name", Reason: "must not be empty"}
	}
	return vErr // BUG: a nil *ValidationError becomes a non-nil error.
}

// ValidateFixed returns the error interface as a literal nil on success, so the
// caller's err == nil check works. On failure it returns a real *ValidationError.
func ValidateFixed(name string) error {
	if name == "" {
		return &ValidationError{Field: "name", Reason: "must not be empty"}
	}
	return nil // correct: a true nil error interface.
}
```

### The runnable demo

The demo calls both functions with valid input and prints whether each reports an
error, making the divergence visible: the buggy one claims failure on valid input, the
fixed one does not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/typednil"
)

func main() {
	if err := typednil.ValidateBuggy("alice"); err != nil {
		fmt.Printf("buggy: reports error on valid input: %v\n", err)
	} else {
		fmt.Println("buggy: no error")
	}

	if err := typednil.ValidateFixed("alice"); err != nil {
		fmt.Printf("fixed: reports error on valid input: %v\n", err)
	} else {
		fmt.Println("fixed: no error")
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy: reports error on valid input: <nil>
fixed: no error
```

The `<nil>` in the buggy line is the tell: `%v` formats the nil dynamic value as
`<nil>`, yet the `err != nil` branch was taken — a non-nil error printing as nil.

### Tests

The tests pin *both* contracts so the failure mode is unforgettable.
`TestBuggyReturnsNonNilOnValidInput` asserts the surprising truth — the buggy function
returns a non-nil error for valid input — and uses `reflect.ValueOf(err).IsNil()` to
show the dynamic value really is nil. `TestFixedReturnsNilOnValidInput` asserts the
corrected behavior. Both invalid-input tests confirm a real `*ValidationError` is
surfaced via `errors.As`.

Create `validate_test.go`:

```go
package typednil

import (
	"errors"
	"reflect"
	"testing"
)

func TestBuggyReturnsNonNilOnValidInput(t *testing.T) {
	t.Parallel()
	err := ValidateBuggy("alice")
	// The incident: valid input, yet err != nil.
	if err == nil {
		t.Fatal("ValidateBuggy returned nil; expected the typed-nil bug to make it non-nil")
	}
	// Diagnosis: the dynamic value is nil even though the interface is not.
	if !reflect.ValueOf(err).IsNil() {
		t.Fatal("expected the dynamic value behind err to be nil (typed nil)")
	}
}

func TestBuggyReturnsRealErrorOnInvalidInput(t *testing.T) {
	t.Parallel()
	err := ValidateBuggy("")
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	if vErr.Field != "name" {
		t.Fatalf("Field = %q, want name", vErr.Field)
	}
}

func TestFixedReturnsNilOnValidInput(t *testing.T) {
	t.Parallel()
	if err := ValidateFixed("alice"); err != nil {
		t.Fatalf("ValidateFixed(valid) = %v, want nil", err)
	}
}

func TestFixedReturnsRealErrorOnInvalidInput(t *testing.T) {
	t.Parallel()
	err := ValidateFixed("")
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	if vErr.Reason != "must not be empty" {
		t.Fatalf("Reason = %q, want must not be empty", vErr.Reason)
	}
}
```

## Review

The lesson is correct when the buggy validator provably returns a non-nil error for
valid input while its dynamic value is nil (`reflect.ValueOf(err).IsNil()` true), and
the fixed validator returns a true nil. Pinning the broken contract in a test — rather
than just fixing it — is deliberate: the failure mode is subtle enough that it slips
past review, and a test that asserts "valid input yields non-nil error" makes the trap
concrete and permanent. The rule to carry away is narrow and absolute: never return a
concrete typed nil pointer through an interface return type. Return `nil` through the
interface, or return the concrete type and let the caller decide when to narrow. If you
find yourself reaching for `reflect...IsNil()` to paper over a nil check, the real fix
is upstream, at the return.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of this trap.
- [The Go Blog: The Laws of Reflection](https://go.dev/blog/laws-of-reflection) — the (type, value) structure of an interface value.
- [`errors.As`](https://pkg.go.dev/errors#As) — extracting a concrete error type from an error chain, used to confirm the real failure.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-retry-decorator.md](05-retry-decorator.md) | Next: [07-http-handler-service-interface.md](07-http-handler-service-interface.md)
