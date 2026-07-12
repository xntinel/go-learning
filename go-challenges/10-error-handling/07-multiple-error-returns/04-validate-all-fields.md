# Exercise 4: Validation Pipeline That Reports Every Invalid Field At Once

An API that validates a request body one field at a time forces the client into a
submit-fix-submit loop. This exercise builds the fail-complete alternative: a
validator that checks every field, collects each violation as a typed
`*FieldError`, and returns one `errors.Join` so the client learns everything wrong
in a single round-trip.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
validate/                  independent module: example.com/validate
  go.mod                   go 1.26
  validate.go              FieldError; CreateUserRequest; Validate() error
  cmd/
    demo/
      main.go              validates a request with three bad fields
  validate_test.go         all-bad, one-bad, valid; errors.As extraction
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: a `*FieldError{Field, Reason}` error type and `CreateUserRequest.Validate()` that checks every field, appends a `*FieldError` per violation, and returns `errors.Join(errs...)`.
- Test: three bad fields yield a `Join` naming all three; `errors.As` extracts the first `*FieldError`; a one-bad-field input surfaces exactly that field; valid input returns nil.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/07-multiple-error-returns/04-validate-all-fields/cmd/demo
cd go-solutions/10-error-handling/07-multiple-error-returns/04-validate-all-fields
```

### Why fail-complete, and why typed violations

Field validation is the canonical independent-failures case: the email being
malformed tells you nothing about whether the age is in range, so there is no
dependency that justifies stopping early. Returning on the first bad field is a
real production defect — it turns fixing a five-field form into five round-trips.
Fail-complete means: check every field, accumulate every violation, join, return.

The violations are a typed `*FieldError{Field, Reason}` rather than bare strings for
one concrete reason: the caller often needs the *structure*, not just the message.
An HTTP handler wants to build a `{"errors": [{"field": "...", "reason": "..."}]}`
body, and a `*FieldError` carries exactly those two pieces. Because each violation
is wrapped into the `Join`, `errors.As(agg, &fe)` pulls the first `*FieldError` back
out of the aggregate — the tree walk finds it. (Extracting *all* of them, to build
the full JSON array, is Exercise 8's flatten; here we prove the single-extraction
path works and that the aggregate names every field.)

`FieldError` implements `error` with a pointer receiver, so the concrete type stored
in the `Join` is `*FieldError`; that is the type `errors.As` must target
(`var fe *FieldError; errors.As(err, &fe)`).

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
)

// FieldError is a single field-level validation violation. It carries structure
// (Field, Reason) so a handler can render it, not just a message.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("field %q: %s", e.Field, e.Reason)
}

// CreateUserRequest is a request body to be validated as a whole.
type CreateUserRequest struct {
	Email    string
	Username string
	Age      int
}

// Validate checks every field and returns errors.Join of all violations, so the
// caller learns everything wrong at once. It returns nil when the request is valid.
func (r CreateUserRequest) Validate() error {
	var errs []error

	if !strings.Contains(r.Email, "@") {
		errs = append(errs, &FieldError{Field: "email", Reason: "must contain @"})
	}
	if len(r.Username) < 3 {
		errs = append(errs, &FieldError{Field: "username", Reason: "must be at least 3 characters"})
	}
	if r.Age < 18 || r.Age > 120 {
		errs = append(errs, &FieldError{Field: "age", Reason: "must be between 18 and 120"})
	}

	return errors.Join(errs...)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/validate"
)

func main() {
	req := validate.CreateUserRequest{Email: "not-an-email", Username: "ab", Age: 5}

	err := req.Validate()
	if err == nil {
		fmt.Println("valid")
		return
	}

	fmt.Println("validation failed:")
	fmt.Println(err)

	var fe *validate.FieldError
	if errors.As(err, &fe) {
		fmt.Printf("first violation field: %s\n", fe.Field)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
validation failed:
field "email": must contain @
field "username": must be at least 3 characters
field "age": must be between 18 and 120
first violation field: email
```

### Tests

`TestValidateAllBad` asserts the aggregate names all three fields and that
`errors.As` extracts a `*FieldError`. `TestValidateOneBad` mutates a single field
and asserts exactly that field surfaces and the others do not — proving the
validator did not short-circuit on the first check. `TestValidateValid` asserts a
good request returns nil. The one-bad case is the load-bearing test: it fails
loudly if someone rewrites `Validate` to `return` on the first violation.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateAllBad(t *testing.T) {
	t.Parallel()

	req := CreateUserRequest{Email: "nope", Username: "x", Age: 0}
	err := req.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}

	msg := err.Error()
	for _, field := range []string{"email", "username", "age"} {
		if !strings.Contains(msg, field) {
			t.Errorf("message %q missing field %q", msg, field)
		}
	}

	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatal("errors.As(err, *FieldError) = false, want true")
	}
	if fe.Field != "email" {
		t.Errorf("first extracted field = %q, want email", fe.Field)
	}
}

func TestValidateOneBad(t *testing.T) {
	t.Parallel()

	req := CreateUserRequest{Email: "a@b.com", Username: "alice", Age: 5}
	err := req.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "age") {
		t.Errorf("message %q missing field age", msg)
	}
	if strings.Contains(msg, "email") || strings.Contains(msg, "username") {
		t.Errorf("message %q should only mention age", msg)
	}

	var fe *FieldError
	if !errors.As(err, &fe) || fe.Field != "age" {
		t.Fatalf("extracted = %+v, want age", fe)
	}
}

func TestValidateValid(t *testing.T) {
	t.Parallel()

	req := CreateUserRequest{Email: "alice@example.com", Username: "alice", Age: 30}
	if err := req.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}
```

## Review

`Validate` is correct when a fully-invalid request names every bad field and a
valid one returns nil. The one-bad-field test is what guards the fail-complete
property against a future short-circuit: if someone changes the accumulation into
an early `return`, the "should only mention age" assertion still passes but the
all-bad test's "names all three" fails — so keep both. `errors.As` returns the
*first* `*FieldError`; that is enough to demonstrate typed extraction here, but a
handler that must render every violation flattens the tree (Exercise 8). Run with
`-race` for habit.

## Resources

- [errors.As](https://pkg.go.dev/errors#As) — first assignable match in the tree.
- [errors.Join](https://pkg.go.dev/errors#Join) — the aggregation primitive.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — custom error types and `%w`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-join-nil-semantics.md](03-join-nil-semantics.md) | Next: [05-close-all-resources.md](05-close-all-resources.md)
