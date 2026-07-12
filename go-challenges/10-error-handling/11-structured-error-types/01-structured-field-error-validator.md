# Exercise 1: The Structured FieldError And ValidationError Tree

Every request that a service accepts is validated somewhere, and the shape of
what that validation returns is the difference between a client that can render a
form and a client that shows "400 Bad Request". This module builds the core
type: a `FieldError` with a machine `Code`, a `Field`, a `Message`, and typed
`Params`, aggregated into a `ValidationError` that collects *every* failure at
once, is queryable by field, and serializes to JSON.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo, its own tests. Nothing here imports any other exercise.

## What you'll build

```text
errstruct/                 independent module: example.com/errstruct
  go.mod                   go 1.26
  schema.go                Code enum; FieldError; ValidationError (Error/Unwrap); User.Validate; ByField; ToJSON
  cmd/
    demo/
      main.go              runnable demo: validate a bad User, look up by field, serialize
  schema_test.go           table tests: success, collects-all, ByField hit/miss, ToJSON, unwrap contract
```

- Files: `schema.go`, `cmd/demo/main.go`, `schema_test.go`.
- Implement: `FieldError{Code, Field, Message, Params}`, `ValidationError{Errors, Err}` with `Error()`/`Unwrap()`, `User.Validate` collecting all failures, `ByField(err, field)` via `errors.As`, and `ToJSON`.
- Test: valid user -> nil; three bad fields -> `errors.As` yields a `*ValidationError` with three errors; `ByField` hit and miss; `ToJSON` serialize and non-validation reject; the unwrap contract.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Why a typed Code and a Params map

The whole reason this type exists rather than a bag of strings is that a client
codes against `Code`, not against `Message`. `Code` is a typed string constant —
`CodeRequired`, `CodeMaxLen`, `CodeRange` — so a switch over it is exhaustive and
a rename is a compile error, not a silent runtime miss. `Params` carries the
typed data behind the message (`{"max": 50}`), so the frontend can render "at
most 50 characters" in its own words and its own language. `Message` is a
default human string, convenient for logs, but never the contract.

`ValidationError` holds a slice of `*FieldError` plus an optional `Err` for a
non-field failure (a whole-request problem that is not tied to one input). Its
`Error()` joins the field messages so a log line is readable, and its `Unwrap()`
returns `Err` so a non-field cause remains reachable by `errors.Is`/`errors.As`.
(Module 02 upgrades this to a full `Unwrap() []error` tree; here we keep the
original single-error unwrap so the two contracts can be contrasted.)

The critical behavior is in `Validate`: it does not return on the first bad
field. It appends a `*FieldError` for each failure and returns them all in one
`ValidationError`. A client gets the complete list and fixes the whole form in
one round trip.

Create `schema.go`:

```go
package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Code is a stable machine constant a client switches on. It never changes
// wording or locale, unlike Message.
type Code string

const (
	CodeRequired Code = "required"
	CodeMinLen   Code = "min_len"
	CodeMaxLen   Code = "max_len"
	CodePattern  Code = "pattern"
	CodeType     Code = "type"
	CodeRange    Code = "range"
)

// FieldError is one validation failure: a machine Code, the Field that failed,
// a human Message, and typed Params for interpolation.
type FieldError struct {
	Code    Code           `json:"code"`
	Field   string         `json:"field"`
	Message string         `json:"message"`
	Params  map[string]any `json:"params,omitempty"`
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Field, e.Message, e.Code)
}

// ValidationError aggregates every FieldError from one validation pass. Err
// carries a non-field failure (e.g. a whole-request problem) when present.
type ValidationError struct {
	Errors []*FieldError `json:"errors"`
	Err    error         `json:"-"`
}

func (e *ValidationError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	parts := make([]string, 0, len(e.Errors))
	for _, fe := range e.Errors {
		parts = append(parts, fe.Error())
	}
	return strings.Join(parts, "; ")
}

func (e *ValidationError) Unwrap() error {
	return e.Err
}

// User is the request payload under validation.
type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Age   int    `json:"age"`
}

// Validate collects EVERY failure into one ValidationError instead of returning
// the first, so a client can fix the whole form in one round trip.
func (u *User) Validate() error {
	var errs []*FieldError
	if u.Name == "" {
		errs = append(errs, &FieldError{Code: CodeRequired, Field: "name", Message: "name is required"})
	}
	if len(u.Name) > 50 {
		errs = append(errs, &FieldError{Code: CodeMaxLen, Field: "name", Message: "name too long", Params: map[string]any{"max": 50}})
	}
	if u.Email == "" {
		errs = append(errs, &FieldError{Code: CodeRequired, Field: "email", Message: "email is required"})
	}
	if u.Age < 0 || u.Age > 150 {
		errs = append(errs, &FieldError{Code: CodeRange, Field: "age", Message: "age out of range", Params: map[string]any{"min": 0, "max": 150}})
	}
	if len(errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: errs}
}

// ByField extracts the FieldError for a given field, or nil if the field did not
// fail. It uses errors.As so a wrapped ValidationError is still found.
func ByField(err error, field string) *FieldError {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return nil
	}
	for _, fe := range ve.Errors {
		if fe.Field == field {
			return fe
		}
	}
	return nil
}

// ToJSON serializes a validation error for the wire. A non-validation error is
// rejected rather than mangled.
func ToJSON(err error) ([]byte, error) {
	var ve *ValidationError
	if !errors.As(err, &ve) {
		return nil, fmt.Errorf("not a validation error")
	}
	return json.Marshal(ve)
}
```

### The runnable demo

The demo validates a deliberately broken `User`, proves the failures were
collected (not just the first), looks one up by field, and serializes the whole
set.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/errstruct"
)

func main() {
	u := &schema.User{Name: "", Email: "", Age: 200}
	err := u.Validate()

	fmt.Println("valid:", err == nil)

	var ve *schema.ValidationError
	if errors.As(err, &ve) {
		fmt.Println("failures:", len(ve.Errors))
	}

	if fe := schema.ByField(err, "email"); fe != nil {
		fmt.Printf("email -> %s\n", fe.Code)
	}

	data, _ := schema.ToJSON(err)
	fmt.Println(string(data))
}
```

Note the import path is the module path `example.com/errstruct`, but the package
name declared in `schema.go` is `schema`, so the demo refers to `schema.User`.

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid: false
failures: 3
email -> required
{"errors":[{"code":"required","field":"name","message":"name is required"},{"code":"required","field":"email","message":"email is required"},{"code":"range","field":"age","message":"age out of range","params":{"max":150,"min":0}}]}
```

The `params` object prints its keys sorted (`max` before `min`) because
`encoding/json` sorts map keys — which is what makes the serialization
deterministic and testable.

### Tests

The table proves the four behaviors that make this type worth having: a valid
input yields `nil`; a triple-bad input is extracted with `errors.As` as a
`*ValidationError` holding exactly three errors; `ByField` hits a failed field
and misses a passing one; `ToJSON` serializes a validation error and rejects a
non-validation one; and the unwrap contract holds.

Create `schema_test.go`:

```go
package schema

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSuccess(t *testing.T) {
	t.Parallel()

	u := &User{Name: "Alice", Email: "alice@example.com", Age: 30}
	if err := u.Validate(); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestValidateCollectsAllErrors(t *testing.T) {
	t.Parallel()

	u := &User{Name: "", Email: "", Age: 200}
	err := u.Validate()

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatal("err should be *ValidationError")
	}
	if len(ve.Errors) != 3 {
		t.Fatalf("len = %d, want 3, got %+v", len(ve.Errors), ve.Errors)
	}
}

func TestByFieldHit(t *testing.T) {
	t.Parallel()

	u := &User{Name: "", Email: "", Age: 30}
	err := u.Validate()

	fe := ByField(err, "email")
	if fe == nil {
		t.Fatal("ByField should return an error for email")
	}
	if fe.Code != CodeRequired {
		t.Fatalf("fe.Code = %q, want required", fe.Code)
	}
}

func TestByFieldMiss(t *testing.T) {
	t.Parallel()

	u := &User{Name: "Alice", Email: "", Age: 30}
	err := u.Validate()

	if fe := ByField(err, "name"); fe != nil {
		t.Fatalf("ByField should return nil for name, got %+v", fe)
	}
}

func TestToJSONSerializes(t *testing.T) {
	t.Parallel()

	u := &User{Name: "", Email: "", Age: 30}
	err := u.Validate()

	data, jerr := ToJSON(err)
	if jerr != nil {
		t.Fatal(jerr)
	}
	if !strings.Contains(string(data), `"field":"name"`) {
		t.Fatalf("data = %s, want it to include field:name", data)
	}
}

func TestToJSONRejectsNonValidationError(t *testing.T) {
	t.Parallel()

	if _, err := ToJSON(errors.New("boom")); err == nil {
		t.Fatal("expected error for non-validation input")
	}
}

func TestUnwrapContract(t *testing.T) {
	t.Parallel()

	// A field-only ValidationError has no non-field cause, so Unwrap is nil,
	// but errors.As still extracts the concrete *ValidationError.
	u := &User{Name: "", Email: "", Age: 30}
	err := u.Validate()

	if errors.Unwrap(err) != nil {
		t.Fatal("field-only ValidationError should Unwrap to nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) || len(ve.Errors) != 2 {
		t.Fatalf("errors.As did not extract the expected *ValidationError: %+v", ve)
	}
}
```

Finally an `Example` in its own file (so its `fmt` import stays isolated from the
table test) that `go test` verifies against its `// Output:` comment.

Create `schema_example_test.go`:

```go
package schema

import "fmt"

func ExampleByField() {
	u := &User{Name: "", Email: "x@y.z", Age: 30}
	fe := ByField(u.Validate(), "name")
	fmt.Println(fe.Code)
	// Output: required
}

func ExampleToJSON() {
	u := &User{Name: "", Email: "", Age: 30}
	data, _ := ToJSON(u.Validate())
	fmt.Println(string(data))
	// Output: {"errors":[{"code":"required","field":"name","message":"name is required"},{"code":"required","field":"email","message":"email is required"}]}
}
```

## Review

The type is correct when `Validate` is a pure function of the input that emits
one `*FieldError` per broken rule and never stops early: a `User` with a blank
name, blank email, and out-of-range age produces exactly three errors, and
`errors.As` extracts them as a `*ValidationError`. `ByField` is correct when it
returns the matching `*FieldError` for a failed field and `nil` for a field that
passed — and it must go through `errors.As`, not a type assertion, so a wrapped
error is still found. `ToJSON` refuses anything that is not a validation error
rather than emitting misleading JSON. The trap this module exists to close is the
first-error-only validator: fix one field, resubmit, discover the next. Run
`go test -race` to confirm nothing shares mutable state across the parallel
tests.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) — extracting a concrete error type across a wrap boundary.
- [`encoding/json`](https://pkg.go.dev/encoding/json) — `Marshal`, struct tags, and `omitempty`.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping and `Is`/`As`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-validationerror-unwrap-tree-traversal.md](02-validationerror-unwrap-tree-traversal.md)
