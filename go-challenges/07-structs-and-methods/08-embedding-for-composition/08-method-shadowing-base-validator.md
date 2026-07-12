# Exercise 8: Layered Validation and the No-Virtual-Dispatch Rule

Embedding looks enough like inheritance that engineers expect a base method to
call the outer override. It does not — promotion has no virtual dispatch. This
exercise builds a layered validator that gets the layering right by explicitly
calling the embedded base, and proves the base never dispatches upward.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
validate/                  independent module: example.com/validate
  go.mod                   go 1.26
  validate.go              BaseValidator.Validate; CreateUserRequest shadows + calls base explicitly
  cmd/
    demo/
      main.go              runnable demo: validate a bad request, print the joined error
  validate_test.go         errors.Join aggregation; base.Validate does not dispatch to the outer
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: a `BaseValidator` with `Validate() error`, embedded in `CreateUserRequest` which defines its own `Validate` that runs field checks and explicitly calls `r.BaseValidator.Validate()` (the manual super-call), aggregating with `errors.Join`.
- Test: `CreateUserRequest.Validate` aggregates outer and base errors (each found via `errors.Is`); calling `Validate` through a stored `BaseValidator` runs only the base logic, pinning no-virtual-dispatch.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/08-embedding-for-composition/08-method-shadowing-base-validator/cmd/demo
cd go-solutions/07-structs-and-methods/08-embedding-for-composition/08-method-shadowing-base-validator
```

### Promotion is not virtual dispatch — so call the base by hand

In an inheritance language, a base class method that internally calls `validate()`
would dispatch to a subclass override. Go's embedding does no such thing. A method
declared on `BaseValidator` operates on a `BaseValidator` and only ever calls
`BaseValidator`'s methods; it has no knowledge that it might be embedded in
something that redefines `Validate`. There is no `this` that could point at the
outer type. So the "template method" pattern — base orchestrates, subclass fills in
— simply does not exist here.

What you do instead inverts the control. The *outer* type is in charge. `CreateUserRequest`
declares its own `Validate`, which shadows the promoted `BaseValidator.Validate`.
Inside it, `CreateUserRequest.Validate` runs its field-specific checks and then
*explicitly* calls `r.BaseValidator.Validate()` — the manual equivalent of a
`super.validate()` call — folding the base's result in with `errors.Join`. The
outer method decides when and whether to invoke the base; the base never reaches
up. This is the correct mental model for all embedding: the outer composes and
delegates; the inner is a component, not a parent.

The demonstration that makes it concrete: assign the embedded base to a plain
`BaseValidator` variable and call `Validate` on it. Even though that value came out
of a `CreateUserRequest`, the call runs `BaseValidator.Validate` only — it does not
find its way to `CreateUserRequest.Validate`. The static type is `BaseValidator`,
and there is no dynamic dispatch to the outer method. Pinning that in a test is the
whole point of the exercise.

`errors.Join` is the right aggregation tool: it combines several errors into one
whose `Is`/`As` traversal finds each wrapped sentinel, so a caller can test for any
specific failure while a human sees them all.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels callers can match with errors.Is.
var (
	ErrRequired     = errors.New("required")
	ErrInvalidEmail = errors.New("invalid email")
	ErrTooShort     = errors.New("too short")
)

// BaseValidator holds the shared checks every request runs. Its Validate operates
// only on BaseValidator; it never dispatches to an embedding type's method.
type BaseValidator struct {
	Name string
}

// Validate runs the base-level checks.
func (b BaseValidator) Validate() error {
	if strings.TrimSpace(b.Name) == "" {
		return fmt.Errorf("name: %w", ErrRequired)
	}
	return nil
}

// CreateUserRequest embeds BaseValidator and shadows Validate to add its own
// field checks, then explicitly calls the base — the manual super-call.
type CreateUserRequest struct {
	BaseValidator
	Email    string
	Password string
}

// Validate runs the request's own checks and folds in the base's result via
// errors.Join. Note the explicit r.BaseValidator.Validate(): promotion would not
// call the base for us, and the base would never call up into this method.
func (r CreateUserRequest) Validate() error {
	var errs []error
	if err := r.BaseValidator.Validate(); err != nil {
		errs = append(errs, err)
	}
	if !strings.Contains(r.Email, "@") {
		errs = append(errs, fmt.Errorf("email %q: %w", r.Email, ErrInvalidEmail))
	}
	if len(r.Password) < 8 {
		errs = append(errs, fmt.Errorf("password: %w", ErrTooShort))
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo validates a request that fails every check and prints the joined error.
`errors.Join` renders each error on its own line, so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/validate"
)

func main() {
	req := validate.CreateUserRequest{
		BaseValidator: validate.BaseValidator{Name: ""},
		Email:         "not-an-email",
		Password:      "short",
	}
	err := req.Validate()
	fmt.Println(err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name: required
email "not-an-email": invalid email
password: too short
```

### Tests

`TestValidateAggregates` validates a fully-invalid request and asserts `errors.Is`
finds each of the three sentinels in the joined result — the base's `ErrRequired`
alongside the outer's `ErrInvalidEmail` and `ErrTooShort`, proving the explicit
super-call folded the base in. `TestValidValidates` confirms a good request returns
nil. `TestNoVirtualDispatch` is the conceptual pin: it stores the embedded value in
a `BaseValidator` variable and asserts that calling `Validate` on it runs only the
base logic — it detects `ErrRequired` but never `ErrInvalidEmail`, because there is
no dispatch to `CreateUserRequest.Validate`.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"testing"
)

func TestValidateAggregates(t *testing.T) {
	t.Parallel()
	req := CreateUserRequest{
		BaseValidator: BaseValidator{Name: ""},
		Email:         "bad",
		Password:      "x",
	}
	err := req.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []error{ErrRequired, ErrInvalidEmail, ErrTooShort} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v; got %v", want, err)
		}
	}
}

func TestValidValidates(t *testing.T) {
	t.Parallel()
	req := CreateUserRequest{
		BaseValidator: BaseValidator{Name: "Alice"},
		Email:         "alice@example.com",
		Password:      "longenough",
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("valid request should pass: %v", err)
	}
}

func TestNoVirtualDispatch(t *testing.T) {
	t.Parallel()
	// Email and Password here would trip ErrInvalidEmail and ErrTooShort in the
	// outer CreateUserRequest.Validate, but not in the base's Validate.
	req := CreateUserRequest{
		BaseValidator: BaseValidator{Name: ""},
		Email:         "bad",
		Password:      "x",
	}

	// Call Validate through the embedded base's static type. There is no virtual
	// dispatch: this runs BaseValidator.Validate, NOT CreateUserRequest.Validate.
	var base BaseValidator = req.BaseValidator
	err := base.Validate()

	if !errors.Is(err, ErrRequired) {
		t.Errorf("base.Validate should report ErrRequired; got %v", err)
	}
	if errors.Is(err, ErrInvalidEmail) || errors.Is(err, ErrTooShort) {
		t.Errorf("base.Validate must NOT dispatch to the outer method; got %v", err)
	}
}
```

## Review

The validator is correct when the outer `Validate` both runs its own checks and
folds in the base's, which `TestValidateAggregates` proves by matching all three
sentinels through `errors.Join`. The concept the lesson is really about is what
`TestNoVirtualDispatch` pins: a base method invoked through a `BaseValidator` value
never calls the outer override, because embedding has no dynamic dispatch. If you
came expecting a "template method" where the base orchestrates and the outer fills
in gaps, invert it — the outer orchestrates and calls the base explicitly. Match
sentinels with `errors.Is`, not string comparison, so the contract survives message
changes.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining multiple errors into one whose `Is`/`As` finds each.
- [Go Specification: Selectors](https://go.dev/ref/spec#Selectors) — method promotion and the absence of dynamic dispatch through embedded fields.
- [Effective Go: Embedding](https://go.dev/doc/effective_go#embedding) — embedding as composition, not subclassing.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-ambiguous-selector-diamond.md](09-ambiguous-selector-diamond.md)
