# Exercise 8: Compose Request Validators as First-Class Predicate Functions

Request validation is a pipeline of first-class predicate functions. This module
defines `type Validator[T any] func(T) error` and `Combine[T](vs ...Validator[T])`
that runs each validator and aggregates the failures with `errors.Join`. Framed
as validating a decoded `CreateUser` request, it shows validators stored in a
slice, composed into one, and every failure recoverable with `errors.Is` against
a sentinel — the shape real API input validation takes.

This module is fully self-contained.

## What you'll build

```text
validator/                 independent module: example.com/validator
  go.mod                   go 1.26
  validator.go             Validator[T], Combine[T], CreateUser + field checks
  cmd/
    demo/
      main.go              validates a good and a triple-invalid request
  validator_test.go        valid, each-field-invalid, joined, empty-is-noop
```

- Files: `validator.go`, `cmd/demo/main.go`, `validator_test.go`.
- Implement: `Validator[T any]`, `Combine[T](vs ...Validator[T]) Validator[T]` aggregating with `errors.Join`, and three field checks over `CreateUser` each wrapping a package sentinel with `%w`.
- Test: valid input yields nil; each individually-invalid field yields an error matched by `errors.Is`; multiple invalid fields yield a joined error where every sentinel is matchable; `Combine()` with no validators passes everything.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/08-validator-pipeline/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/08-validator-pipeline
```

### Predicates as values, aggregation as composition

A `Validator[T]` is just a function from a value to an error (or nil). Because it
is a first-class value, you can put validators in a slice, pass them around, and
compose them. `Combine` is the composition: it returns a new `Validator[T]` that
runs each supplied validator, collects the non-nil errors, and joins them with
`errors.Join`. The generic parameter keeps the whole pipeline typed to your
concrete request struct — no `any`, no assertions.

Two properties make this pipeline production-grade. First, **it reports all
failures at once**, not just the first. A form-validation endpoint that returns
"name is required" then, after the user fixes it, "invalid email" is a bad
experience; `errors.Join` bundles every failure so the client can fix them
together. Second, **every failure stays matchable.** Each field check wraps a
package-level sentinel with `%w` (`fmt.Errorf("%w: %q", ErrInvalidEmail, email)`),
and `errors.Join` preserves those chains, so `errors.Is(joined, ErrInvalidEmail)`
is true even when three failures are bundled. That is what lets a caller branch on
*which* validations failed rather than string-matching a message.

The empty case is meaningful: `Combine()` with no validators returns a validator
that always passes, because `errors.Join()` with no non-nil errors returns nil.
That makes `Combine` safe to call with a dynamically assembled slice of rules.

Create `validator.go`:

```go
package validator

import (
	"errors"
	"fmt"
	"strings"
)

// Validator checks a value and reports why it is invalid, or nil if it is valid.
type Validator[T any] func(T) error

// Combine runs every validator and joins their errors with errors.Join. A
// Combine of zero validators accepts everything (returns a nil error).
func Combine[T any](vs ...Validator[T]) Validator[T] {
	return func(v T) error {
		var errs []error
		for _, check := range vs {
			if err := check(v); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// CreateUser is a decoded request body awaiting validation.
type CreateUser struct {
	Name  string
	Email string
	Age   int
}

var (
	ErrNameRequired = errors.New("name is required")
	ErrInvalidEmail = errors.New("invalid email")
	ErrAgeRange     = errors.New("age out of range")
)

// NameNonEmpty rejects a blank or whitespace-only name.
func NameNonEmpty(u CreateUser) error {
	if strings.TrimSpace(u.Name) == "" {
		return ErrNameRequired
	}
	return nil
}

// EmailShape rejects an email without a local part, an '@', or a dotted domain.
func EmailShape(u CreateUser) error {
	at := strings.IndexByte(u.Email, '@')
	if at <= 0 || at == len(u.Email)-1 || strings.IndexByte(u.Email[at+1:], '.') < 0 {
		return fmt.Errorf("%w: %q", ErrInvalidEmail, u.Email)
	}
	return nil
}

// AgeInRange rejects an age outside [18, 120].
func AgeInRange(u CreateUser) error {
	if u.Age < 18 || u.Age > 120 {
		return fmt.Errorf("%w: %d", ErrAgeRange, u.Age)
	}
	return nil
}
```

### The runnable demo

The demo validates one good request and one request that fails all three checks,
printing the joined error (one failure per line, as `errors.Join` formats it).

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/validator"
)

func main() {
	validate := validator.Combine(
		validator.NameNonEmpty,
		validator.EmailShape,
		validator.AgeInRange,
	)

	good := validator.CreateUser{Name: "Ada", Email: "ada@example.com", Age: 36}
	bad := validator.CreateUser{Name: "  ", Email: "nope", Age: 9}

	fmt.Printf("good: %v\n", validate(good))
	fmt.Println("bad:")
	fmt.Println(validate(bad))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: <nil>
bad:
name is required
invalid email: "nope"
age out of range: 9
```

### Tests

Create `validator_test.go`:

```go
package validator

import (
	"errors"
	"testing"
)

func TestValidCreateUser(t *testing.T) {
	t.Parallel()
	validate := Combine(NameNonEmpty, EmailShape, AgeInRange)
	u := CreateUser{Name: "Ada", Email: "ada@example.com", Age: 36}
	if err := validate(u); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
}

func TestEachFieldFailureIsMatchable(t *testing.T) {
	t.Parallel()
	validate := Combine(NameNonEmpty, EmailShape, AgeInRange)

	cases := []struct {
		name     string
		input    CreateUser
		sentinel error
	}{
		{"blank name", CreateUser{Name: " ", Email: "a@b.co", Age: 30}, ErrNameRequired},
		{"bad email", CreateUser{Name: "Ada", Email: "nope", Age: 30}, ErrInvalidEmail},
		{"young", CreateUser{Name: "Ada", Email: "a@b.co", Age: 10}, ErrAgeRange},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validate(tc.input)
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, want to match %v", err, tc.sentinel)
			}
		})
	}
}

func TestJoinedFailuresAllMatch(t *testing.T) {
	t.Parallel()
	validate := Combine(NameNonEmpty, EmailShape, AgeInRange)
	err := validate(CreateUser{Name: "", Email: "x", Age: 200})

	for _, sentinel := range []error{ErrNameRequired, ErrInvalidEmail, ErrAgeRange} {
		if !errors.Is(err, sentinel) {
			t.Fatalf("joined error does not match %v: %v", sentinel, err)
		}
	}
}

func TestCombineEmptyPassesEverything(t *testing.T) {
	t.Parallel()
	validate := Combine[CreateUser]()
	if err := validate(CreateUser{}); err != nil {
		t.Fatalf("empty Combine rejected input: %v", err)
	}
}
```

## Review

The pipeline is correct when a valid request passes, each field failure is
recoverable with `errors.Is` against its sentinel, a multi-field failure is a
joined error where *every* sentinel still matches, and `Combine()` with no rules
is a no-op that accepts everything. The two mechanics that make this work are
wrapping each sentinel with `%w` (so the chain survives) and using `errors.Join`
(so all failures are reported at once and each remains matchable). Validators are
plain function values held in a slice and composed — first-class predicates, not
methods on a validator object. Run `go test -race`.

## Resources

- [pkg.go.dev: errors.Join](https://pkg.go.dev/errors#Join) — aggregating multiple errors while keeping each matchable.
- [pkg.go.dev: errors.Is](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel through a join.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w` wrapping and unwrapping.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-circuit-breaker-closure.md](07-circuit-breaker-closure.md) | Next: [09-method-value-shutdown-registry.md](09-method-value-shutdown-registry.md)
