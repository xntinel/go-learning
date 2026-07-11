# Exercise 7: A Public Error Contract: Sentinels and an Exported Error Type

How a package reports failure is as much a public contract as its function signatures.
This exercise builds the two complementary tools: exported sentinel errors matched by
identity with `errors.Is`, and an exported error type with unexported fields plus
accessor methods matched by shape with `errors.As`. It shows the trade-off between them
and how wrapping with `%w` preserves the chain so callers can still match either after
the error has travelled up several layers.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
accounts/                  independent module: example.com/accounts
  go.mod                   go 1.26
  accounts.go              ErrNotFound/ErrConflict sentinels; ValidationError type; Register/Lookup
  cmd/
    demo/
      main.go              triggers each error path, matches with Is/As
  accounts_test.go         package accounts (whitebox): construction of ValidationError
  contract_test.go         package accounts_test (blackbox): Is/As across a wrapped chain
```

- Files: `accounts.go`, `cmd/demo/main.go`, `accounts_test.go`, `contract_test.go`.
- Implement: exported `ErrNotFound`/`ErrConflict` sentinels; an exported `ValidationError` struct with unexported fields and exported `Field()`/`Error()` methods; functions that wrap a sentinel deep in a chain with `%w` and return a `ValidationError`.
- Test: white-box for construction; black-box for the contract, wrap a sentinel and assert `errors.Is` finds it, return a `ValidationError` and assert `errors.As` extracts it and `Field()` returns the offending field.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/accounts/cmd/demo
cd ~/go-exercises/accounts
go mod init example.com/accounts
go mod edit -go=1.26
```

### Sentinel vs. typed error, and why %w matters

A sentinel error is a package-level `var ErrX = errors.New(...)`. It carries no data
beyond its identity, and callers match it with `errors.Is(err, ErrX)`. It is the cheapest
possible contract: "this class of failure happened", nothing more. Use it when the mere
identity of the failure is all the caller needs, `ErrNotFound`, `ErrConflict`,
`io.EOF`.

A typed error is a struct implementing `error`. It carries structured data,
here `ValidationError` holds which field failed and why, and callers match it with
`errors.As(err, &target)`, which fills in the target if any error in the chain is of that
type, giving the caller access to the data through exported accessors. Use it when the
caller needs details to react (show the user which field to fix, decide whether to retry).
The fields are unexported (`field`, `msg`) so callers cannot mutate them or depend on the
struct's exact shape; they read through `Field()` and `Error()`, which is a stable
contract you can keep even if you restructure the internals.

The glue is `%w`. When an error travels up through layers, each layer wraps it with
`fmt.Errorf("context: %w", err)`. `%w` (as opposed to `%v`) records the wrapped error in
the chain, so `errors.Is` and `errors.As` can still find the original sentinel or typed
error at the top no matter how many layers added context. Wrap with `%v` instead and the
chain is severed: the message still reads fine, but `errors.Is`/`errors.As` can no longer
match, and callers are forced back to substring-matching. That is the single most common
way an error contract is silently broken.

Create `accounts.go`:

```go
package accounts

import (
	"errors"
	"fmt"
	"strings"
)

// Exported sentinels: matched by identity with errors.Is. They carry no data.
var (
	ErrNotFound = errors.New("accounts: not found")
	ErrConflict = errors.New("accounts: already exists")
)

// ValidationError is an exported typed error. Its fields are unexported, so
// callers read them through Field()/Error() and cannot depend on the struct's
// exact shape. Matched by shape with errors.As.
type ValidationError struct {
	field string
	msg   string
}

// Field returns the offending field name; the accessor is the stable contract.
func (e *ValidationError) Field() string { return e.field }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("accounts: validation failed on %q: %s", e.field, e.msg)
}

// newValidationError is unexported: callers get a ValidationError back from the
// package's functions, they never construct one.
func newValidationError(field, msg string) *ValidationError {
	return &ValidationError{field: field, msg: msg}
}

// Register validates its input and returns a *ValidationError on bad data.
func Register(email string) error {
	if email == "" {
		return newValidationError("email", "must not be empty")
	}
	if !strings.Contains(email, "@") {
		return newValidationError("email", "must contain @")
	}
	return nil
}

// Lookup wraps ErrNotFound several layers deep to show %w preserves the chain.
func Lookup(id string) error {
	if err := queryRow(id); err != nil {
		return fmt.Errorf("Lookup(%q): %w", id, err)
	}
	return nil
}

// queryRow simulates a storage layer that wraps the sentinel once itself, so the
// final chain has two wrapping layers over ErrNotFound.
func queryRow(id string) error {
	if id == "" {
		return fmt.Errorf("queryRow: %w", ErrNotFound)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/accounts"
)

func main() {
	// Sentinel path: matched by identity even through two wrapping layers.
	err := accounts.Lookup("")
	fmt.Printf("lookup err: %v\n", err)
	fmt.Printf("is ErrNotFound: %v\n", errors.Is(err, accounts.ErrNotFound))

	// Typed path: matched by shape, then read via the accessor.
	verr := accounts.Register("not-an-email")
	var ve *accounts.ValidationError
	if errors.As(verr, &ve) {
		fmt.Printf("validation field: %s\n", ve.Field())
	}

	// Happy path.
	fmt.Printf("register ok: %v\n", accounts.Register("ada@example.com") == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
lookup err: Lookup(""): queryRow: accounts: not found
is ErrNotFound: true
validation field: email
register ok: true
```

Note the message shows two layers of context (`Lookup(...)`, `queryRow:`) over the
sentinel's own text, and `errors.Is` still matches through both because each layer used
`%w`.

### Tests

The white-box test (`package accounts`) covers construction: it calls the unexported
`newValidationError` and reads the unexported `field` directly, proving same-package
access. The black-box test (`package accounts_test`) is the contract: it asserts
`errors.Is` finds the sentinel through the wrapped chain, asserts `errors.As` extracts the
`ValidationError` and `Field()` returns the offending field, and records that the
unexported `field` is not reachable from outside the package, forcing callers through the
accessor.

Create `accounts_test.go`:

```go
package accounts

import "testing"

// White-box: construction and the unexported field are reachable in-package.
func TestNewValidationError(t *testing.T) {
	t.Parallel()

	e := newValidationError("email", "must contain @")
	if e.field != "email" {
		t.Fatalf("field = %q, want email", e.field)
	}
	if e.Field() != "email" {
		t.Fatalf("Field() = %q, want email", e.Field())
	}
}
```

Create `contract_test.go`:

```go
package accounts_test

import (
	"errors"
	"testing"

	"example.com/accounts"
)

func TestSentinelMatchesThroughWrapping(t *testing.T) {
	t.Parallel()

	err := accounts.Lookup("") // wrapped twice over ErrNotFound
	if !errors.Is(err, accounts.ErrNotFound) {
		t.Fatalf("errors.Is could not find ErrNotFound in %v", err)
	}
}

func TestValidationErrorExtractedByAs(t *testing.T) {
	t.Parallel()

	err := accounts.Register("no-at-sign")
	var ve *accounts.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As could not extract *ValidationError from %v", err)
	}
	if ve.Field() != "email" {
		t.Fatalf("Field() = %q, want email", ve.Field())
	}
}

func TestRegisterHappyPath(t *testing.T) {
	t.Parallel()

	if err := accounts.Register("ada@example.com"); err != nil {
		t.Fatalf("Register valid email: %v", err)
	}
}

// The unexported field is not reachable from the external test package:
//
//	var ve *accounts.ValidationError
//	_ = ve.field   // ve.field undefined (cannot refer to unexported field field)
//
// Callers must go through the exported Field() accessor, which is the stable API.
```

## Review

The error contract is correct when `errors.Is(err, ErrNotFound)` still matches after
`Lookup` and `queryRow` have each wrapped the sentinel with `%w`, and when
`errors.As(err, &ve)` extracts the `ValidationError` and `ve.Field()` returns `"email"`.
The trade-off is the lesson: a sentinel is cheap and data-free, right when identity is
all the caller needs; a typed error carries structured data through unexported fields and
exported accessors, right when the caller must react to specifics. The `%w` discipline is
what keeps either matchable up the stack, switch any wrap to `%v` and both black-box
assertions fail while the demo's message still looks fine, which is exactly why you assert
the contract rather than eyeball the message. The commented `ve.field` line records the
compiler error that forces callers through `Field()`.

## Resources

- [`errors` package: Is, As, and wrapping](https://pkg.go.dev/errors) — the matching functions and the `%w` verb semantics.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — sentinels vs. typed errors, `Is`/`As`, and when to wrap.
- [`fmt.Errorf`](https://pkg.go.dev/fmt#Errorf) — the `%w` verb that records the wrapped error in the chain.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-functional-options-constructor.md](06-functional-options-constructor.md) | Next: [08-constructor-enforced-invariant.md](08-constructor-enforced-invariant.md)
