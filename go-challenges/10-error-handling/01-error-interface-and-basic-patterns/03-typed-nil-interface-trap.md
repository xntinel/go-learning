# Exercise 3: The (*Error)(nil) != nil Trap in a Validation Helper

This is the single most expensive Go-specific error bug, and it ships to
production because it compiles, passes a casual read, and only misbehaves on the
success path. A helper declared to return a concrete pointer type — here
`*ValidationError` — returns a typed nil on success, and the moment its caller
stores that result in an `error` the `if err != nil` fires anyway. This exercise
reproduces the bug in a `processOrder` path, then fixes it, with a test that pins
both.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
typednil/                    independent module: example.com/typednil
  go.mod                     go 1.26
  order.go                   ValidationError; validate; processOrderBuggy; processOrder
  cmd/
    demo/
      main.go                runnable demo: buggy vs fixed on a valid request
  order_test.go              test proving the bug, then pinning the fix
```

- Files: `order.go`, `cmd/demo/main.go`, `order_test.go`.
- Implement: `validate(req CreateOrderRequest) *ValidationError`; a buggy `processOrderBuggy` that returns `validate(req)` straight into an `error`; a fixed `processOrder` declared to return `error` that returns literal `nil` on success.
- Test: assert `processOrderBuggy` returns a non-nil error for a *valid* request (the bug); assert `processOrder` returns `err == nil` for a valid request and that `errors.As(err, &target)` succeeds for an invalid one.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/01-error-interface-and-basic-patterns/03-typed-nil-interface-trap/cmd/demo
cd go-solutions/10-error-handling/01-error-interface-and-basic-patterns/03-typed-nil-interface-trap
```

### Why a nil pointer is not a nil interface

An interface value is a pair of words: a type word and a value word. It is `nil`
only when *both* words are nil. When you assign a nil `*ValidationError` to an
`error`, the value word is nil but the type word is set to `*ValidationError` — so
the interface is non-nil. `validate` returning `nil` means "no validation error",
but the return type is `*ValidationError`, so that `nil` is a typed nil. The
buggy caller does `return validate(req)`, implicitly converting `*ValidationError`
to `error`; the resulting interface carries the type word and is therefore
non-nil even though the pointer inside is nil. Every caller's `if err != nil`
now treats a perfectly valid order as failed.

The fix is not a cast or a `reflect` check at the call site — those are band-aids.
The fix is a rule: a function whose result a caller treats as an error should be
*declared* to return `error`, and should `return nil` (a literal, untyped nil) on
the success path. That literal nil sets both interface words to nil, so
`err != nil` is correctly false. Keep the concrete pointer type internal to the
validator; convert to `error` only through an explicit `if err != nil { return err }`
that never lets a typed nil escape.

`processOrderBuggy` and `processOrder` are the wrong and right shapes side by side.
The buggy one returns the concrete result directly. The correct one checks the
concrete result and returns a literal `nil` when there is no error, so no typed nil
ever reaches the interface.

Create `order.go`:

```go
package typednil

import "fmt"

// CreateOrderRequest is the inbound payload a handler would validate.
type CreateOrderRequest struct {
	SKU      string
	Quantity int
}

// ValidationError is a concrete error type carrying the offending field.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed on %s: %s", e.Field, e.Reason)
}

// validate returns a *ValidationError describing the first problem, or a nil
// *ValidationError when the request is valid. Returning the concrete pointer is
// fine here; the danger is only in how a caller converts it to error.
func validate(req CreateOrderRequest) *ValidationError {
	if req.SKU == "" {
		return &ValidationError{Field: "SKU", Reason: "must not be empty"}
	}
	if req.Quantity <= 0 {
		return &ValidationError{Field: "Quantity", Reason: "must be positive"}
	}
	return nil
}

// processOrderBuggy demonstrates the trap: it returns the *ValidationError
// directly into an error return. On a valid request validate returns a nil
// *ValidationError, but the returned error interface is NON-nil because its type
// word is *ValidationError. Callers see if err != nil fire on success.
func processOrderBuggy(req CreateOrderRequest) error {
	return validate(req)
}

// processOrder is the fix: declared to return error, it checks the concrete
// result and returns a literal nil on success, so no typed nil reaches the
// interface. On failure it returns the non-nil *ValidationError, which converts
// to a genuinely non-nil error.
func processOrder(req CreateOrderRequest) error {
	if err := validate(req); err != nil {
		return err
	}
	return nil
}
```

### The runnable demo

The demo runs both variants against the same valid request so the discrepancy is
visible: the buggy path reports a failure that did not happen, the fixed path
reports success.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/typednil"
)

func main() {
	valid := typednil.CreateOrderRequest{SKU: "ABC-123", Quantity: 2}

	if err := typednil.ProcessOrderBuggy(valid); err != nil {
		fmt.Println("buggy:  reports FAILURE on a valid request (the trap)")
	} else {
		fmt.Println("buggy:  ok")
	}

	if err := typednil.ProcessOrder(valid); err != nil {
		fmt.Println("fixed:  unexpected error:", err)
	} else {
		fmt.Println("fixed:  ok on a valid request")
	}

	invalid := typednil.CreateOrderRequest{SKU: "", Quantity: 2}
	if err := typednil.ProcessOrder(invalid); err != nil {
		fmt.Println("fixed:  correctly rejects invalid request:", err)
	}
}
```

Because the demo lives in `package main` it can only see exported names, so the
two process functions are exported. Add these thin exported wrappers at the end of
`order.go`:

Append to `order.go`:

```go
// ProcessOrderBuggy and ProcessOrder are exported wrappers so the demo (a
// separate package main) can exercise both variants.
func ProcessOrderBuggy(req CreateOrderRequest) error { return processOrderBuggy(req) }
func ProcessOrder(req CreateOrderRequest) error      { return processOrder(req) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
buggy:  reports FAILURE on a valid request (the trap)
fixed:  ok on a valid request
fixed:  correctly rejects invalid request: validation failed on SKU: must not be empty
```

### Tests

The test first *demonstrates* the bug — asserting `processOrderBuggy` returns a
non-nil error for a valid request — so the failure mode is documented and pinned,
not merely described. Then it pins the fix: `processOrder` returns `err == nil`
for a valid request, and for an invalid one `errors.As` extracts the concrete
`*ValidationError`. A comment records why the interface is non-nil.

Create `order_test.go`:

```go
package typednil

import (
	"errors"
	"testing"
)

// TestBuggyVariantFiresOnSuccess documents the trap: a nil *ValidationError
// stored in an error interface is non-nil because the interface holds a
// (type, value) pair and only the value word is nil.
func TestBuggyVariantFiresOnSuccess(t *testing.T) {
	t.Parallel()

	err := processOrderBuggy(CreateOrderRequest{SKU: "ABC-123", Quantity: 2})
	if err == nil {
		t.Fatal("expected the buggy variant to (wrongly) return a non-nil error on a valid request")
	}
}

func TestFixedVariantSucceeds(t *testing.T) {
	t.Parallel()

	if err := processOrder(CreateOrderRequest{SKU: "ABC-123", Quantity: 2}); err != nil {
		t.Fatalf("processOrder on a valid request = %v, want nil", err)
	}
}

func TestFixedVariantRejectsInvalid(t *testing.T) {
	t.Parallel()

	err := processOrder(CreateOrderRequest{SKU: "", Quantity: 2})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As did not find *ValidationError in %v", err)
	}
	if ve.Field != "SKU" {
		t.Fatalf("ve.Field = %q, want SKU", ve.Field)
	}
}

func TestNilPointerIsNonNilInterface(t *testing.T) {
	t.Parallel()

	var typed *ValidationError // nil pointer
	var iface error = typed    // conversion sets the type word
	if iface == nil {
		t.Fatal("expected a nil *ValidationError stored in error to be non-nil")
	}
}
```

## Review

The trap is understood when you can state the rule without hedging: an interface
is nil only when both its type word and value word are nil, so a typed nil pointer
in an `error` is never nil. `TestNilPointerIsNonNilInterface` isolates that fact in
three lines; `TestBuggyVariantFiresOnSuccess` shows it causing a real success-path
regression; the two fixed-variant tests prove the cure. The cure is architectural,
not a `reflect.ValueOf(err).IsNil()` guard at the call site — declare the return as
`error` and return a literal `nil` on success.

The mistake to avoid is the tempting shortcut of "just check for nil pointer with
reflection" sprinkled at every call site; it treats the symptom and leaves the
next helper to re-introduce the bug. Keep concrete error types internal to the
producer and only ever hand `error` across the boundary, returning literal nil for
"no error".

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of the interface (type, value) pair.
- [The Go Programming Language Specification: Interface types](https://go.dev/ref/spec#Interface_types) — when an interface value is nil.
- [pkg.go.dev: errors.As](https://pkg.go.dev/errors#As) — extracting a concrete error type from an error chain.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-config-loader-zero-value.md](02-config-loader-zero-value.md) | Next: [04-cache-miss-error-value.md](04-cache-miss-error-value.md)
