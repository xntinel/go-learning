# Exercise 2: Make ValidationError A Real Error Tree With Unwrap() []error

A `ValidationError` that only *holds* a slice is a container. A `ValidationError`
that implements `Unwrap() []error` is a *tree* the `errors` package can walk — so
`errors.Is(err, ErrRequired)` becomes true when any field failed for that reason,
and `errors.AsType[*FieldError]` can reach a specific failure buried in a wrapped
request. This module makes that upgrade and pins the subtle Go 1.20 traversal
semantics, including the `errors.Unwrap` blind spot.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
errtree/                   independent module: example.com/errtree
  go.mod                   go 1.26
  errtree.go               sentinels ErrRequired/ErrConflict; FieldError(Unwrap() error); ValidationError(Unwrap() []error); SignupForm.Validate
  cmd/
    demo/
      main.go              runnable demo: errors.Is over the tree, AsType extraction
  errtree_test.go          Is true/false, AsType[*FieldError], errors.Unwrap == nil
```

- Files: `errtree.go`, `cmd/demo/main.go`, `errtree_test.go`.
- Implement: category sentinels `ErrRequired`/`ErrConflict`; a `FieldError` whose `Unwrap() error` returns its category cause; a `ValidationError` whose `Unwrap() []error` exposes the whole set; a `SignupForm.Validate` that builds the tree.
- Test: `errors.Is(err, ErrRequired)` true, `errors.Is(err, ErrConflict)` false; `errors.AsType[*FieldError](err)` extracts the first field error and its `Code`; `errors.Unwrap(err)` is `nil` because the type uses the `[]error` form.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/errtree/cmd/demo
cd ~/go-exercises/errtree
go mod init example.com/errtree
go mod edit -go=1.26
```

### Two Unwrap shapes, two behaviors

Go 1.20 extended the `errors` package with multi-error support. There are now two
`Unwrap` method shapes and they are treated very differently:

- `Unwrap() error` — the classic single-parent chain. `errors.Unwrap` (the
  function) follows it, and `errors.Is`/`errors.As` walk it.
- `Unwrap() []error` — a node with many children. `errors.Is`/`errors.As` walk it
  depth-first, pre-order over the slice. But `errors.Unwrap` (the function) does
  **not** follow it — it only knows the single-error method. So a type that
  implements `Unwrap() []error` reports `errors.Unwrap(x) == nil`.

This module uses both. Each `FieldError` implements `Unwrap() error` returning its
category sentinel (`ErrRequired`, `ErrConflict`), so `errors.Is(fieldErr,
ErrRequired)` is true. `ValidationError` implements `Unwrap() []error` returning
its field errors, so `errors.Is(validationErr, ErrRequired)` walks into every
child and is true when *any* field carries that cause. The combination is what
lets a caller ask "did anything in this request fail because a required field was
blank?" with one `errors.Is` call, and "give me the first field error" with
`errors.AsType[*FieldError]`.

The blind spot is deliberate and worth internalizing: because `ValidationError`
uses the `[]error` form, `errors.Unwrap(validationErr)` is `nil`. Code that walks
a chain with a manual `for err != nil { err = errors.Unwrap(err) }` loop will see
nothing. Multi-error trees are reached through `Is`/`As`, never through a linear
`Unwrap` walk.

Create `errtree.go`:

```go
package errtree

import (
	"fmt"
	"strings"
)

// Category sentinels. A FieldError wraps exactly one of these as its cause so
// errors.Is can classify the whole request by reason.
var (
	ErrRequired = fmt.Errorf("required")
	ErrConflict = fmt.Errorf("conflict")
)

type Code string

const (
	CodeRequired Code = "required"
	CodeConflict Code = "conflict"
)

// codeCause maps a Code to its category sentinel.
func codeCause(c Code) error {
	switch c {
	case CodeRequired:
		return ErrRequired
	case CodeConflict:
		return ErrConflict
	default:
		return nil
	}
}

// FieldError is one failure. cause is the category sentinel it wraps, reached
// through Unwrap() error so errors.Is(fieldErr, ErrRequired) works.
type FieldError struct {
	Code  Code
	Field string
	cause error
}

func newFieldError(code Code, field string) *FieldError {
	return &FieldError{Code: code, Field: field, cause: codeCause(code)}
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Code)
}

func (e *FieldError) Unwrap() error { return e.cause }

// ValidationError is an aggregate. Unwrap() []error (Go 1.20) makes the whole
// set participate in errors.Is/As tree walking.
type ValidationError struct {
	Errors []*FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, fe.Error())
	}
	return strings.Join(parts, "; ")
}

// Unwrap returns the field errors as []error. errors.Is/As walk these
// depth-first, pre-order; errors.Unwrap (single-error form) does NOT follow it.
func (e *ValidationError) Unwrap() []error {
	out := make([]error, len(e.Errors))
	for i, fe := range e.Errors {
		out[i] = fe
	}
	return out
}

// SignupForm is the request under validation.
type SignupForm struct {
	Username string
	Email    string
}

// Validate collects every failure into one ValidationError tree.
func (f *SignupForm) Validate() error {
	var errs []*FieldError
	if f.Username == "" {
		errs = append(errs, newFieldError(CodeRequired, "username"))
	}
	if f.Email == "" {
		errs = append(errs, newFieldError(CodeRequired, "email"))
	}
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/errtree"
)

func main() {
	f := &errtree.SignupForm{Username: "", Email: ""}
	err := f.Validate()

	fmt.Println("is required:", errors.Is(err, errtree.ErrRequired))
	fmt.Println("is conflict:", errors.Is(err, errtree.ErrConflict))

	if fe, ok := errors.AsType[*errtree.FieldError](err); ok {
		fmt.Println("first field:", fe.Field, fe.Code)
	}

	fmt.Println("errors.Unwrap is nil:", errors.Unwrap(err) == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
is required: true
is conflict: false
first field: username required
errors.Unwrap is nil: true
```

### Tests

The table nails the three claims: the aggregate is `ErrRequired` (a child wraps
it) and is not `ErrConflict` (no child does); `errors.AsType[*FieldError]` pulls
the first field error out of the tree and its `Code` is correct; and
`errors.Unwrap(err)` is `nil` precisely because the aggregate uses the `[]error`
form. A fourth test proves the single-error contrast: `errors.Unwrap` of a lone
`*FieldError` *does* return its sentinel cause.

Create `errtree_test.go`:

```go
package errtree

import (
	"errors"
	"testing"
)

func TestIsMatchesCategory(t *testing.T) {
	t.Parallel()

	err := (&SignupForm{Username: "", Email: ""}).Validate()

	if !errors.Is(err, ErrRequired) {
		t.Fatal("errors.Is(err, ErrRequired) = false, want true")
	}
	if errors.Is(err, ErrConflict) {
		t.Fatal("errors.Is(err, ErrConflict) = true, want false")
	}
}

func TestAsTypeExtractsFirstFieldError(t *testing.T) {
	t.Parallel()

	err := (&SignupForm{Username: "", Email: ""}).Validate()

	fe, ok := errors.AsType[*FieldError](err)
	if !ok {
		t.Fatal("errors.AsType[*FieldError] did not find a FieldError")
	}
	if fe.Field != "username" || fe.Code != CodeRequired {
		t.Fatalf("first field error = %s/%s, want username/required", fe.Field, fe.Code)
	}
}

func TestUnwrapIsNilForMultiError(t *testing.T) {
	t.Parallel()

	err := (&SignupForm{Username: "", Email: ""}).Validate()

	// The []error form is invisible to errors.Unwrap by design.
	if errors.Unwrap(err) != nil {
		t.Fatal("errors.Unwrap of a []error aggregate should be nil")
	}
}

func TestSingleErrorUnwrapFollowsCause(t *testing.T) {
	t.Parallel()

	fe := newFieldError(CodeConflict, "email")

	// A lone FieldError uses the single-error form, so errors.Unwrap DOES
	// reach its category sentinel.
	if errors.Unwrap(fe) != ErrConflict {
		t.Fatalf("errors.Unwrap(fieldErr) = %v, want ErrConflict", errors.Unwrap(fe))
	}
	if !errors.Is(fe, ErrConflict) {
		t.Fatal("errors.Is(fieldErr, ErrConflict) = false, want true")
	}
}

func TestValidateOK(t *testing.T) {
	t.Parallel()

	if err := (&SignupForm{Username: "a", Email: "b@c.d"}).Validate(); err != nil {
		t.Fatalf("valid form -> %v, want nil", err)
	}
}
```

An `Example` verified against its `// Output:` comment:

```go
// errtree_example_test.go
package errtree

import (
	"errors"
	"fmt"
)

func ExampleValidationError_treeWalk() {
	err := (&SignupForm{Username: "", Email: ""}).Validate()
	fmt.Println(errors.Is(err, ErrRequired), errors.Unwrap(err) == nil)
	// Output: true true
}
```

## Review

The upgrade is correct when `errors.Is(validationErr, ErrRequired)` is true —
proving `errors.Is` descended into the `[]error` children and each child's
`Unwrap() error` reached its sentinel — while `errors.Is(validationErr,
ErrConflict)` is false because no child wraps that cause. `errors.AsType[
*FieldError]` returning the first field error proves `As` walks the same tree.
The signature detail that catches people: `errors.Unwrap(validationErr)` is
`nil`, because the function form only follows `Unwrap() error`; the `[]error`
node is reachable by `Is`/`As` only. The classic mistake this module rules out is
giving an aggregate an `Unwrap() error` that returns just one child — that
silently drops the rest of the tree. Use `Unwrap() []error`. Run `go test -race`.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `Is`, `As`, `AsType`, `Join`, and `Unwrap`; note the two `Unwrap` method shapes.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping and the `Is`/`As` model the 1.20 tree extends.
- [RFC 9457: Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457.html) — the wire shape a walked tree feeds in later modules.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-structured-field-error-validator.md](01-structured-field-error-validator.md) | Next: [03-dotted-path-nested-validator.md](03-dotted-path-nested-validator.md)
