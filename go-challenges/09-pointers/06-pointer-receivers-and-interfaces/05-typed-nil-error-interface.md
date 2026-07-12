# Exercise 5: The Typed-Nil Trap: When a Returned *Error Is Not a nil error

The nastiest interface bug in Go returns no error and yet trips the caller's
`err != nil`. It happens when a function returns a nil concrete pointer typed into
the `error` interface: the interface is not nil, because its type word is set. This
module reproduces the trap in a validation function, fixes it, and pins both with
tests — including `errors.As` on a genuine failure.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
typednil/                   independent module: example.com/typednil
  go.mod                    go 1.25
  validate.go               ValidationError; validateBad (buggy) and Validate (fixed)
  cmd/
    demo/
      main.go               show the buggy path reports a phantom error, fixed path does not
  validate_test.go          buggy != nil, fixed == nil, errors.As on real failure
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: a `*ValidationError` error type, a buggy `validateBad` that returns a typed nil, and a fixed `Validate` that returns an explicit nil on success.
- Test: `validateBad` on valid input yields `err != nil` (the bug); `Validate` on valid input yields `err == nil`; on invalid input, `errors.As` extracts `*ValidationError` and its message is asserted.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/09-pointers/06-pointer-receivers-and-interfaces/05-typed-nil-error-interface/cmd/demo
cd go-solutions/09-pointers/06-pointer-receivers-and-interfaces/05-typed-nil-error-interface
go mod edit -go=1.25
```

### The (type, value) tuple and why nil is not nil

An `error` is an interface value, and an interface value is two words: a **type**
descriptor and a **value**. It is nil only when both words are nil. Consider the
buggy function:

```go
func validateBad(name string) error {
	var e *ValidationError // nil pointer, type *ValidationError
	if name == "" {
		e = &ValidationError{Field: "name", Msg: "required"}
	}
	return e // returns (type=*ValidationError, value=<the pointer>)
}
```

When `name` is non-empty, `e` stays nil, but `return e` converts the nil
`*ValidationError` into the `error` interface. The interface now carries
type = `*ValidationError` and value = nil. Because the type word is non-nil, the
interface is **not** nil. The caller writes the ordinary `if err != nil { ... }`
and it fires — reporting a validation failure that never happened. In a real
service this is the alert that pages on-call for an error whose `Error()` string is
often a confusing `<nil>`.

The fix is to never let a typed nil escape as an `error`. Return an explicit `nil`
on the success path, and construct the `*ValidationError` only when there is truly
an error:

```go
func Validate(name string) error {
	if name == "" {
		return &ValidationError{Field: "name", Msg: "required"}
	}
	return nil // untyped nil: interface is (type=nil, value=nil) == nil
}
```

Create `validate.go`:

```go
// validate.go
package typednil

import "fmt"

// ValidationError describes a single field validation failure. Error has a
// pointer receiver, so it is *ValidationError that satisfies the error interface.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: field %q %s", e.Field, e.Msg)
}

// validateBad demonstrates the typed-nil trap: it declares a *ValidationError
// and returns it. On the success path the pointer is nil but the returned error
// interface is (type=*ValidationError, value=nil), which is NOT a nil error.
func validateBad(name string) error {
	var e *ValidationError
	if name == "" {
		e = &ValidationError{Field: "name", Msg: "required"}
	}
	return e
}

// Validate is the correct version: it returns an explicit untyped nil on
// success, so the caller's err != nil behaves as expected.
func Validate(name string) error {
	if name == "" {
		return &ValidationError{Field: "name", Msg: "required"}
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"

	"example.com/typednil"
)

func main() {
	// Fixed version: valid input yields a genuinely nil error.
	if err := typednil.Validate("alice"); err != nil {
		fmt.Println("fixed, valid input: unexpected error:", err)
	} else {
		fmt.Println("fixed, valid input: err == nil (correct)")
	}

	// Fixed version: invalid input yields a real, inspectable error.
	err := typednil.Validate("")
	var ve *typednil.ValidationError
	if errors.As(err, &ve) {
		fmt.Printf("fixed, invalid input: field=%s msg=%s\n", ve.Field, ve.Msg)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fixed, valid input: err == nil (correct)
fixed, invalid input: field=name msg=required
```

### Tests

`TestBuggyPathReportsPhantomError` locks in the trap: valid input through
`validateBad` still yields `err != nil`. `TestFixedPathIsNil` shows `Validate`
returns a true nil. `TestErrorsAs` extracts the concrete type from a genuine
failure and asserts the message. The table covers valid and invalid inputs.

Create `validate_test.go`:

```go
// validate_test.go
package typednil

import (
	"errors"
	"testing"
)

func TestBuggyPathReportsPhantomError(t *testing.T) {
	t.Parallel()

	// Valid input: no failure occurred, yet the buggy function's returned error
	// is non-nil because of the typed-nil trap.
	err := validateBad("alice")
	if err == nil {
		t.Fatal("validateBad(\"alice\") == nil; expected the typed-nil phantom (err != nil)")
	}
}

func TestFixedPathIsNil(t *testing.T) {
	t.Parallel()

	if err := Validate("alice"); err != nil {
		t.Fatalf("Validate(\"alice\") = %v, want nil", err)
	}
}

func TestErrorsAs(t *testing.T) {
	t.Parallel()

	err := Validate("")
	if err == nil {
		t.Fatal("Validate(\"\") = nil, want a *ValidationError")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As did not extract *ValidationError from %v", err)
	}
	if ve.Field != "name" || ve.Msg != "required" {
		t.Fatalf("ValidationError = {%s %s}, want {name required}", ve.Field, ve.Msg)
	}
	const want = `validation: field "name" required`
	if ve.Error() != want {
		t.Fatalf("Error() = %q, want %q", ve.Error(), want)
	}
}

func TestValidateTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "alice", false},
		{"empty", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%q) err = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
		})
	}
}
```

## Review

The contrast is the whole lesson: `validateBad` and `Validate` differ only in
whether a typed nil can escape, and the tests show that difference producing
opposite `err != nil` results on identical valid input. The rule that prevents the
trap is mechanical — a function returning `error` must `return nil` (untyped) on
success, never a nil concrete pointer variable. When you do have a real failure,
`errors.As` walks the chain and binds the concrete `*ValidationError` so callers
can read its fields; that is the correct way to inspect a typed error, and it only
works because the success path returns a genuine nil.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of the typed-nil trap.
- [errors.As](https://pkg.go.dev/errors#As) — extracting a concrete error type from a chain.
- [The Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.Is`/`As` and wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-json-unmarshaler-pointer-receiver.md](04-json-unmarshaler-pointer-receiver.md) | Next: [06-repository-port-interface.md](06-repository-port-interface.md)
