# Exercise 4: Aggregate Field Validation Errors With errors.Join

A request validator that stops at the first bad field forces the user through one
round-trip per mistake. This exercise builds a validator that checks every field,
collects one `*FieldError` per failure, and returns `errors.Join(errs...)` so the
caller gets every problem at once — and proves that `errors.Is` and `errors.As`
walk the joined tree to find each sentinel and the first typed field error.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
validatejoin/                   independent module: example.com/validatejoin
  go.mod                        go 1.25
  validate.go                   sentinels, FieldError, ValidateSignup -> errors.Join
  validate_test.go              Is over the join tree, As to first FieldError, nil path
  cmd/demo/main.go              runnable demo printing every failure at once
```

Files: `validate.go`, `validate_test.go`, `cmd/demo/main.go`.
Implement: `ValidateSignup` collecting a `*FieldError` per invalid field and returning `errors.Join(errs...)`; `FieldError` implements `Error` and `Unwrap`.
Test: a request failing 3 fields; assert `errors.Is` finds `ErrRequired` and `ErrTooLong`; assert the joined `Error()` contains all three messages; assert `errors.Join` returns nil when every field is valid; assert `errors.As` extracts a `*FieldError`.
Verify: `go test -count=1 -race ./... && go vet ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Collect every failure, then Join

The anti-pattern is first-failure-wins: check name, `return` if empty; check
email, `return` if bad; and so on. A user with three bad fields fixes one,
resubmits, learns about the next, and so on — three round-trips for one form. The
fix is to check every field, accumulate one error per failure in a slice, and
return `errors.Join(errs...)` once.

`errors.Join` is built for exactly this. It takes a variadic list of errors,
skips the `nil` ones, and returns a single error whose `Error()` is the non-nil
messages joined by newlines and whose `Unwrap() []error` exposes the list. Two
properties make it the right return type. First, `errors.Join()` with no non-nil
inputs returns `nil`, so a valid request naturally yields a `nil` error without
any special-casing — you always `return errors.Join(errs...)` and success falls
out. Second, because it exposes `Unwrap() []error`, `errors.Is` and `errors.As`
traverse the joined tree depth-first: `errors.Is(joined, ErrRequired)` is true if
*any* leaf wraps `ErrRequired`, and `errors.As(joined, &fe)` binds the *first*
`*FieldError` it reaches.

Each leaf is a `*FieldError` that records which field failed and wraps a sentinel
describing the *kind* of failure — `ErrRequired`, `ErrTooLong`, `ErrInvalid`. The
`Unwrap() error` method on `*FieldError` is what lets `errors.Is` see through the
field error to the sentinel inside it. So the joined result supports two kinds of
query at once: "did any field fail because it was required?" via `errors.Is` on
the sentinel, and "give me the first failing field's detail" via `errors.As` into
`*FieldError`.

Create `validate.go`:

```go
package validatejoin

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinels describe the KIND of each field failure.
var (
	ErrRequired = errors.New("required")
	ErrTooLong  = errors.New("too long")
	ErrInvalid  = errors.New("invalid")
)

// FieldError names the failing field and wraps a kind sentinel.
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Err)
}

// Unwrap exposes the sentinel so errors.Is can match the kind.
func (e *FieldError) Unwrap() error { return e.Err }

type Signup struct {
	Name  string
	Email string
	Bio   string
}

// ValidateSignup checks every field and returns errors.Join of all failures, or
// nil when the request is valid.
func ValidateSignup(s Signup) error {
	var errs []error

	if strings.TrimSpace(s.Name) == "" {
		errs = append(errs, &FieldError{Field: "name", Err: ErrRequired})
	}
	if !strings.Contains(s.Email, "@") {
		errs = append(errs, &FieldError{Field: "email", Err: ErrInvalid})
	}
	if len(s.Bio) > 20 {
		errs = append(errs, &FieldError{Field: "bio", Err: ErrTooLong})
	}

	// Join returns nil when errs is empty or all-nil, so success falls out.
	return errors.Join(errs...)
}
```

### The runnable demo

The demo validates a request that fails all three checks and prints the aggregate,
then shows the sentinel and typed-field queries.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/validatejoin"
)

func main() {
	bad := validatejoin.Signup{
		Name:  "",
		Email: "not-an-email",
		Bio:   "this biography is definitely far too long to accept",
	}
	err := validatejoin.ValidateSignup(bad)

	fmt.Println("all failures:")
	fmt.Println(err)

	fmt.Println("is required missing:", errors.Is(err, validatejoin.ErrRequired))
	fmt.Println("is too long:", errors.Is(err, validatejoin.ErrTooLong))

	var fe *validatejoin.FieldError
	if errors.As(err, &fe) {
		fmt.Println("first bad field:", fe.Field)
	}

	good := validatejoin.Signup{Name: "alice", Email: "a@b.co", Bio: "hi"}
	fmt.Println("valid request err:", validatejoin.ValidateSignup(good))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all failures:
name: required
email: invalid
bio: too long
is required missing: true
is too long: true
first bad field: name
valid request err: <nil>
```

### Tests

The tests exercise the join tree. `TestJoinFindsAllSentinels` fails three fields
and asserts `errors.Is` finds `ErrRequired`, `ErrInvalid`, and `ErrTooLong`, and
that the aggregate string contains every message. `TestValidPassesNil` asserts a
valid request yields `nil`. `TestAsExtractsFirstField` asserts `errors.As` binds
the first `*FieldError`.

Create `validate_test.go`:

```go
package validatejoin

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestJoinFindsAllSentinels(t *testing.T) {
	t.Parallel()
	err := ValidateSignup(Signup{Name: "", Email: "nope", Bio: strings.Repeat("x", 30)})
	if err == nil {
		t.Fatal("want aggregate error, got nil")
	}

	for _, s := range []error{ErrRequired, ErrInvalid, ErrTooLong} {
		if !errors.Is(err, s) {
			t.Fatalf("errors.Is(err, %v) = false, want true", s)
		}
	}

	msg := err.Error()
	for _, want := range []string{"name: required", "email: invalid", "bio: too long"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("aggregate %q missing %q", msg, want)
		}
	}
	if strings.Count(msg, "\n") != 2 {
		t.Fatalf("want 3 newline-joined messages, got %q", msg)
	}
}

func TestValidPassesNil(t *testing.T) {
	t.Parallel()
	if err := ValidateSignup(Signup{Name: "alice", Email: "a@b.co", Bio: "hi"}); err != nil {
		t.Fatalf("valid request returned %v, want nil", err)
	}
}

func TestAsExtractsFirstField(t *testing.T) {
	t.Parallel()
	err := ValidateSignup(Signup{Name: "", Email: "a@b.co", Bio: "hi"})
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("errors.As(err, *FieldError) = false, want true")
	}
	if fe.Field != "name" {
		t.Fatalf("fe.Field = %q, want name", fe.Field)
	}
}

func Example() {
	err := ValidateSignup(Signup{Name: "", Email: "bad", Bio: "hi"})
	fmt.Println(errors.Is(err, ErrRequired), errors.Is(err, ErrInvalid))
	// Output: true true
}
```

## Review

The validator is correct when a valid request returns exactly `nil` and an invalid
one returns an aggregate that names every failure. The `errors.Join(errs...)`
return is what gives both: `Join` drops nils and returns `nil` when nothing
failed, and exposes `Unwrap() []error` so `errors.Is` finds each kind sentinel and
`errors.As` binds the first `*FieldError`. The mistake to avoid is the
first-failure-wins early return, which turns a single form into many round-trips;
collect into a slice and Join once. Note that `errors.Unwrap` (the free function)
would return `nil` on this joined error — it does not descend the list — so inspect
it with `errors.Is`/`errors.As`, never with `errors.Unwrap`. Run `go test -race`.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — nil-skipping, newline-joined aggregate with `Unwrap() []error`.
- [errors.Is](https://pkg.go.dev/errors#Is) — depth-first traversal over the join tree.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping and multi-error trees.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-repository-driver-error-translation.md](03-repository-driver-error-translation.md) | Next: [05-retry-classification-as-interface.md](05-retry-classification-as-interface.md)
