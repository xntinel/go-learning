# Exercise 10: Modern Extraction With errors.AsType[E] (Go 1.26)

Go 1.26 added `errors.AsType[E error](err error) (E, bool)`, the generic
return-value form of `errors.As`. Instead of declaring a pointer temporary and
passing its address, you get the matched error back directly. This exercise
refactors a handler that extracts two typed errors — a `*ValidationError` and an
`*AppError` — from the classic `As` form to `AsType`, and proves the two forms
recover the exact same concrete pointer.

This module is fully self-contained: its own `go mod init`, demo, and tests. It
requires the Go 1.26 toolchain, so the module pins `go 1.26`.

## What you'll build

```text
astypegeneric/                  independent module: example.com/astypegeneric
  go.mod                        go 1.26 (errors.AsType)
  astype.go                     ValidationError, AppError, Describe using errors.AsType
  astype_test.go                AsType (matched, true) / (nil, false); equivalence with As
  cmd/demo/main.go              runnable demo extracting both typed errors
```

Files: `astype.go`, `astype_test.go`, `cmd/demo/main.go`.
Implement: `Describe(err)` that extracts a `*ValidationError` and an `*AppError` with `errors.AsType`, no pointer temporaries.
Test: `errors.AsType[*ValidationError](err)` returns `(ve, true)` with populated fields when present and `(nil, false)` otherwise; assert equivalence with the classic `errors.As` form on the same input (same concrete pointer).
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module (Go 1.26 required):

```bash
mkdir -p go-solutions/10-error-handling/03-errors-is-and-errors-as/10-astype-generic-matching/cmd/demo
cd go-solutions/10-error-handling/03-errors-is-and-errors-as/10-astype-generic-matching
go mod edit -go=1.26
```

### The pointer-out form and its ergonomics cost

The classic `errors.As` reads a value out through a pointer: you declare a
temporary of the target type, take its address, and pass it. Extracting two typed
errors in one function means two temporaries and two `if errors.As(...)` blocks:

```go
var ve *ValidationError
if errors.As(err, &ve) {
	// use ve
}
var ae *AppError
if errors.As(err, &ae) {
	// use ae
}
```

The temporaries carry no information — they exist only because `As` needs a place
to write. Go 1.26's `errors.AsType` removes them. Its signature is
`func AsType[E error](err error) (E, bool)`: you pass the error and a type
parameter naming the concrete error type you want, and it returns the matched value
directly along with an `ok`. The same extraction becomes:

```go
if ve, ok := errors.AsType[*ValidationError](err); ok {
	// use ve
}
if ae, ok := errors.AsType[*AppError](err); ok {
	// use ae
}
```

Each `ve` and `ae` is scoped to its own `if`, there is no free-floating temporary,
and the type you are asking for is written once, at the call site, as a type
argument. The Go docs recommend preferring `AsType` for most uses for exactly this
reason.

`AsType` is not a different algorithm — it obeys the same tree traversal and the
same custom-`As` method rules as `As`; the docs state that `As` is equivalent to
`AsType` but sets its target argument rather than returning the match. So the two
forms recover the identical concrete pointer from the identical input, which is
what the equivalence test asserts. The one hard requirement is the toolchain:
`AsType` exists only from Go 1.26, so the module pins `go 1.26`. On an older
toolchain the build fails with an undefined-symbol error — expected, and the
reason for the version pin.

`Describe` is the refactored call site: it extracts a `*ValidationError` first
(client-input problem), then an `*AppError` (application fault with a code), then
falls back to a generic description — all with `AsType`, no temporaries.

Create `astype.go`:

```go
package astypegeneric

import (
	"errors"
	"fmt"
)

// ValidationError is a client-input problem carrying the offending field.
type ValidationError struct {
	Field string
}

func (e *ValidationError) Error() string { return "validation failed on " + e.Field }

// AppError is an application fault carrying a bounded code.
type AppError struct {
	Code string
	Err  error
}

func (e *AppError) Error() string {
	if e.Err != nil {
		return e.Code + ": " + e.Err.Error()
	}
	return e.Code
}

func (e *AppError) Unwrap() error { return e.Err }

// Describe extracts each typed error with errors.AsType — no pointer temporaries —
// and returns a short human description.
func Describe(err error) string {
	if err == nil {
		return "ok"
	}
	if ve, ok := errors.AsType[*ValidationError](err); ok {
		return fmt.Sprintf("invalid field %q", ve.Field)
	}
	if ae, ok := errors.AsType[*AppError](err); ok {
		return fmt.Sprintf("app error code %q", ae.Code)
	}
	return "unknown error"
}
```

### The runnable demo

The demo runs `Describe` over a wrapped validation error, a wrapped app error, and
a plain error, showing each extraction.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/astypegeneric"
)

func main() {
	ve := fmt.Errorf("bind: %w", &astypegeneric.ValidationError{Field: "email"})
	ae := fmt.Errorf("call: %w", &astypegeneric.AppError{Code: "db_unavailable"})
	plain := errors.New("something else")

	fmt.Println(astypegeneric.Describe(ve))
	fmt.Println(astypegeneric.Describe(ae))
	fmt.Println(astypegeneric.Describe(plain))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
invalid field "email"
app error code "db_unavailable"
unknown error
```

### Tests

`TestAsTypeMatches` asserts `AsType` returns `(ve, true)` with the populated field
when the chain contains a `*ValidationError`. `TestAsTypeMisses` asserts it returns
`(nil, false)` when it does not. `TestEquivalentToAs` asserts `AsType` and the
classic `As` recover the exact same concrete pointer from the same input.

Create `astype_test.go`:

```go
package astypegeneric

import (
	"errors"
	"fmt"
	"testing"
)

func TestAsTypeMatches(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("bind: %w", &ValidationError{Field: "email"})

	ve, ok := errors.AsType[*ValidationError](err)
	if !ok {
		t.Fatal("AsType[*ValidationError] = _, false; want true")
	}
	if ve.Field != "email" {
		t.Fatalf("ve.Field = %q, want email", ve.Field)
	}
}

func TestAsTypeMisses(t *testing.T) {
	t.Parallel()
	err := errors.New("plain")

	ve, ok := errors.AsType[*ValidationError](err)
	if ok {
		t.Fatal("AsType[*ValidationError] = _, true; want false")
	}
	if ve != nil {
		t.Fatalf("AsType returned %v on miss, want nil", ve)
	}
}

func TestEquivalentToAs(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("call: %w", &AppError{Code: "timeout"})

	// Classic pointer-out form.
	var classic *AppError
	okClassic := errors.As(err, &classic)

	// Generic return-value form.
	generic, okGeneric := errors.AsType[*AppError](err)

	if okClassic != okGeneric {
		t.Fatalf("ok mismatch: As=%v AsType=%v", okClassic, okGeneric)
	}
	if classic != generic {
		t.Fatalf("As and AsType recovered different pointers: %p vs %p", classic, generic)
	}
}

func ExampleDescribe() {
	err := fmt.Errorf("bind: %w", &ValidationError{Field: "email"})
	fmt.Println(Describe(err))
	// Output: invalid field "email"
}
```

## Review

The refactor is correct when every extraction site reads
`if x, ok := errors.AsType[*T](err); ok` with no free-floating pointer temporary,
and the behavior is identical to the `As` form it replaced — same traversal, same
custom-`As` rules, same recovered pointer, which `TestEquivalentToAs` pins by
comparing the two pointers directly. The mistake to avoid is assuming `AsType`
changes matching semantics; it does not, it only changes the calling convention.
The hard requirement is the toolchain: `AsType` is Go 1.26, so the module pins
`go 1.26` and the build fails on older toolchains — which is the honest signal that
this is a 1.26 feature, not something to backport. Run `go test -race`.

## Resources

- [errors.AsType](https://pkg.go.dev/errors#AsType) — the generic return-value form (Go 1.26).
- [errors.As](https://pkg.go.dev/errors#As) — the classic pointer-out form and the equivalence note.
- [Go 1.26 release notes](https://go.dev/doc/go1.26) — the addition of `errors.AsType`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-queue-consumer-ack-nack-dlq.md](09-queue-consumer-ack-nack-dlq.md) | Next: [../04-custom-error-types/00-concepts.md](../04-custom-error-types/00-concepts.md)
