# Exercise 6: Avoid The Typed-Nil Interface Trap In A Repository

The single most confusing bug in Go interface code: a function returns `error`,
the concrete error pointer is nil, and yet `err != nil` at the call site is true.
Callers see a failure that never happened. This exercise reproduces the trap in a
repository validation path, shows the correct constructor, and builds a guard that
inspects the pointer, not just the interface.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
typednil/                   independent module: example.com/typednil
  go.mod                    module path
  typednil.go               AppError; ValidateBuggy (trap) vs ValidateCorrect; IsNilError guard
  cmd/
    demo/
      main.go               runnable demo showing the trap and the fix
  typednil_test.go          the trap is non-nil; the fix is nil; the guard detects a nil pointer
```

Files: `typednil.go`, `cmd/demo/main.go`, `typednil_test.go`.
Implement: `AppError`, `ValidateBuggy` (returns a typed-nil pointer), `ValidateCorrect` (returns the nil literal), `IsNilError` (comma-ok plus a `p == nil` check), and a reflect-based diagnostic.
Test: the buggy path is wrongly non-nil, the correct path is nil, and the guard treats a nil-pointer interface as no error.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/typednil/cmd/demo
cd ~/go-exercises/typednil
go mod init example.com/typednil
```

### Why the interface is non-nil

An interface value is two words: a type descriptor and a data pointer. It equals
`nil` only when *both* are nil. When `ValidateBuggy` does
`var e *AppError` (a nil pointer) and then `return e`, the return converts the
concrete `*AppError` into an `error` interface — which sets the type word to
`*AppError` and the data word to the nil pointer. The type word is non-nil, so the
interface is non-nil, even though the pointer inside it is nil. Every caller writing
the idiomatic `if err != nil { return err }` now propagates a phantom failure.

`ValidateCorrect` avoids it by returning the untyped `nil` literal on the success
path. There is no concrete type to stamp into the type word, so the interface is a
true nil. This is the rule: a function returning `error` must `return nil`, never
`return someNilTypedPointer`. If your control flow builds up a `*AppError` variable,
convert to the interface only on the failure branch.

When you are handed an `error` from code you do not control and must decide whether
it is "really" nil, guard the pointer. `IsNilError` uses the comma-ok form
`p, ok := err.(*AppError)` — which *succeeds* with `p == nil` on the trapped value —
and then explicitly checks `p == nil`. Guarding only the interface (`err != nil`)
is exactly what fails. The reflect-based variant generalizes the check to any
pointer type and is useful as a diagnostic, though the real fix is always to return
`nil` at the source rather than to guard everywhere.

Create `typednil.go`:

```go
package typednil

import "reflect"

// AppError is a domain error with a pointer receiver.
type AppError struct {
	Code string
}

func (e *AppError) Error() string { return "app error: " + e.Code }

// Record is a row a repository would save.
type Record struct {
	ID   string
	Name string
}

// ValidateBuggy is WRONG. It builds a *AppError variable and returns it, so on the
// success path it returns a typed-nil pointer widened to a non-nil error interface.
func ValidateBuggy(r Record) error {
	var e *AppError
	if r.Name == "" {
		e = &AppError{Code: "empty_name"}
	}
	return e // even when e is nil, the returned error interface is non-nil
}

// ValidateCorrect returns the untyped nil literal on success, so the interface is
// truly nil.
func ValidateCorrect(r Record) error {
	if r.Name == "" {
		return &AppError{Code: "empty_name"}
	}
	return nil
}

// IsNilError reports whether err is a real nil or the typed-nil trap: an interface
// holding a nil *AppError. It guards the pointer, not just the interface.
func IsNilError(err error) bool {
	if err == nil {
		return true
	}
	if p, ok := err.(*AppError); ok && p == nil {
		return true
	}
	return false
}

// IsNilViaReflect is a diagnostic generalization: true if err is nil or holds a
// nil pointer of any type. Prefer fixing the source over reaching for this.
func IsNilViaReflect(err error) bool {
	if err == nil {
		return true
	}
	v := reflect.ValueOf(err)
	return v.Kind() == reflect.Pointer && v.IsNil()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/typednil"
)

func main() {
	valid := typednil.Record{ID: "1", Name: "alice"}

	buggy := typednil.ValidateBuggy(valid)
	fmt.Printf("ValidateBuggy: err != nil = %v (phantom failure)\n", buggy != nil)
	fmt.Printf("IsNilError(buggy) = %v (guard sees the truth)\n", typednil.IsNilError(buggy))

	correct := typednil.ValidateCorrect(valid)
	fmt.Printf("ValidateCorrect: err != nil = %v\n", correct != nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ValidateBuggy: err != nil = true (phantom failure)
IsNilError(buggy) = true (guard sees the truth)
ValidateCorrect: err != nil = false
```

### Tests

`TestBuggyValidateIsWronglyNonNil` pins the trap so a future refactor cannot hide
it; `TestCorrectValidateIsNil` proves the fix; the guard tests prove that comma-ok
success with a nil pointer is treated as no-error.

Create `typednil_test.go`:

```go
package typednil

import (
	"fmt"
	"testing"
)

func TestBuggyValidateIsWronglyNonNil(t *testing.T) {
	t.Parallel()
	err := ValidateBuggy(Record{ID: "1", Name: "alice"})
	// The trap: the pointer is nil but the interface is not.
	if err == nil {
		t.Fatal("expected the typed-nil trap: interface should be non-nil")
	}
	if p, ok := err.(*AppError); !ok || p != nil {
		t.Fatalf("expected a nil *AppError inside the interface, got %v (%T)", err, err)
	}
}

func TestCorrectValidateIsNil(t *testing.T) {
	t.Parallel()
	if err := ValidateCorrect(Record{ID: "1", Name: "alice"}); err != nil {
		t.Fatalf("ValidateCorrect on a valid record returned %v, want nil", err)
	}
	if err := ValidateCorrect(Record{ID: "2", Name: ""}); err == nil {
		t.Fatal("ValidateCorrect on an invalid record returned nil, want error")
	}
}

func TestIsNilErrorDetectsTypedNil(t *testing.T) {
	t.Parallel()
	buggy := ValidateBuggy(Record{Name: "ok"})
	if !IsNilError(buggy) {
		t.Fatal("IsNilError should treat a nil *AppError interface as no error")
	}
	if !IsNilViaReflect(buggy) {
		t.Fatal("IsNilViaReflect should detect the nil pointer")
	}

	real := ValidateCorrect(Record{Name: ""})
	if IsNilError(real) {
		t.Fatal("IsNilError should report a real error as not-nil")
	}
}

func ExampleIsNilError() {
	// The buggy path yields a non-nil interface, but the guard sees the nil pointer.
	buggy := ValidateBuggy(Record{Name: "alice"})
	fmt.Println(buggy != nil, IsNilError(buggy))
	// Output: true true
}
```

## Review

The trap is correct to reproduce and the fix is correct to prefer: a function typed
to return `error` returns the `nil` literal on success, so callers' `err != nil`
means what it says. `TestBuggyValidateIsWronglyNonNil` freezes the failure mode as a
teaching artifact, while `IsNilError` shows the only reliable runtime guard —
comma-ok to the concrete pointer followed by an explicit `p == nil`, because the
comma-ok alone succeeds on the trapped value. Reach for the guard only when you
cannot fix the source; the real cure is `return nil`. Run `go test -race` to confirm
both the trap and the guard.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error)
- [Go Tour: Type assertions](https://go.dev/tour/methods/15)
- [reflect.Value.IsNil](https://pkg.go.dev/reflect#Value.IsNil)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-domain-error-to-http-status.md](05-domain-error-to-http-status.md) | Next: [07-event-dispatcher-type-switch.md](07-event-dispatcher-type-switch.md)
