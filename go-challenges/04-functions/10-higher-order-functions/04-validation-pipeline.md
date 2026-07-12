# Exercise 4: Composing Field Validators into a Request-Validation Pipeline

At an API boundary you want to report every problem with a request at once, not
one per round-trip. This exercise builds a validation pipeline from first-class
rule factories, joining all failures with `errors.Join`.

## What you'll build

```text
validate/                    independent module: example.com/validate
  go.mod                     go 1.25
  validate.go                type Rule[T]; Validate; NonEmpty, MaxLen, Matches, InRange; FieldError
  validate_test.go           accumulate-all, all-valid, order-independence, fail-fast contrast
  cmd/demo/
    main.go                  validates a CreateUserRequest and prints all violations
```

- Files: `validate.go`, `validate_test.go`, `cmd/demo/main.go`.
- Implement: `Rule[T any] func(T) error`, `Validate[T](v T, rules ...Rule[T]) error` returning `errors.Join` of all failures, the rule factories, and a `FieldError` type.
- Test: a request violating three rules surfaces all three (`errors.Is`/`errors.As`); a valid request returns nil; rule order does not change which errors appear; a fail-fast variant for contrast; `FieldError` recovered via `errors.As`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/10-higher-order-functions/04-validation-pipeline/cmd/demo
cd go-solutions/04-functions/10-higher-order-functions/04-validation-pipeline
go mod edit -go=1.25
```

### Accumulate-all versus fail-fast

`Validate` runs every rule and collects each non-nil error, then returns
`errors.Join(errs...)`. `errors.Join` produces a single error whose `Unwrap()
[]error` exposes all the causes, so the caller recovers any of them with
`errors.Is` (against a sentinel) or `errors.As` (into a typed `FieldError`). If no
rule fails, every collected error is nil and `errors.Join()` returns nil — a valid
request validates to `nil` with no special-casing.

This is the correct default at a boundary. Fail-fast — return on the first failure —
makes a client fix one field, resubmit, discover the next problem, resubmit again.
Accumulate-all hands back the whole list in one response. The exercise includes a
fail-fast `ValidateFailFast` purely so a test can contrast the two: same rules, same
input, but fail-fast surfaces only the first violation while `Validate` surfaces all
of them. Choosing between them is a design decision you make per layer, not an
accident of how the loop is written.

Each rule is a factory: `NonEmpty(field, get)` returns a `Rule[T]` closed over the
field name and an accessor `get func(T) string` that pulls the value out of the
struct. This keeps the rules generic over the request type `T` and reusable across
requests — `NonEmpty("email", func(r Req) string { return r.Email })` for one type,
a different accessor for another. The failure carries a `FieldError` so callers can
render "which field, what rule" without string-parsing.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"regexp"
)

// Rule validates a value of type T, returning a non-nil error on failure.
type Rule[T any] func(T) error

// Validate runs every rule and returns errors.Join of all failures, so a caller
// sees every problem at once. It returns nil when all rules pass.
func Validate[T any](v T, rules ...Rule[T]) error {
	var errs []error
	for _, rule := range rules {
		if err := rule(v); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ValidateFailFast returns on the first failing rule. It exists to contrast with
// Validate; prefer Validate at an API boundary.
func ValidateFailFast[T any](v T, rules ...Rule[T]) error {
	for _, rule := range rules {
		if err := rule(v); err != nil {
			return err
		}
	}
	return nil
}

// FieldError names the field and rule that failed. Callers recover it with
// errors.As to build a structured 422 response.
type FieldError struct {
	Field string
	Rule  string
	Msg   string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Field, e.Msg, e.Rule)
}

// Sentinel errors let callers classify a failure by category with errors.Is.
var (
	ErrEmpty      = errors.New("value is empty")
	ErrTooLong    = errors.New("value too long")
	ErrNoMatch    = errors.New("value does not match pattern")
	ErrOutOfRange = errors.New("value out of range")
)

// NonEmpty fails when get(v) is the empty string.
func NonEmpty[T any](field string, get func(T) string) Rule[T] {
	return func(v T) error {
		if get(v) == "" {
			return fmt.Errorf("%w: %w", ErrEmpty, &FieldError{Field: field, Rule: "non_empty", Msg: "must not be empty"})
		}
		return nil
	}
}

// MaxLen fails when len(get(v)) exceeds n.
func MaxLen[T any](field string, n int, get func(T) string) Rule[T] {
	return func(v T) error {
		if got := len(get(v)); got > n {
			return fmt.Errorf("%w: %w", ErrTooLong,
				&FieldError{Field: field, Rule: "max_len", Msg: fmt.Sprintf("must be at most %d chars, got %d", n, got)})
		}
		return nil
	}
}

// Matches fails when get(v) does not match re.
func Matches[T any](field string, re *regexp.Regexp, get func(T) string) Rule[T] {
	return func(v T) error {
		if !re.MatchString(get(v)) {
			return fmt.Errorf("%w: %w", ErrNoMatch,
				&FieldError{Field: field, Rule: "matches", Msg: "invalid format"})
		}
		return nil
	}
}

// InRange fails when get(v) is outside [lo, hi].
func InRange[T any](field string, lo, hi int, get func(T) int) Rule[T] {
	return func(v T) error {
		if got := get(v); got < lo || got > hi {
			return fmt.Errorf("%w: %w", ErrOutOfRange,
				&FieldError{Field: field, Rule: "in_range", Msg: fmt.Sprintf("must be in [%d,%d], got %d", lo, hi, got)})
		}
		return nil
	}
}
```

Each failure wraps two things with `%w`: a category sentinel (`ErrEmpty`, ...) and
a `*FieldError`. `fmt.Errorf` with two `%w` verbs produces an error that
`errors.Is` matches against the sentinel *and* `errors.As` unpacks into the
`*FieldError`. That is how one returned value serves both a coarse `errors.Is`
classification and a structured field-level response.

### The runnable demo

The demo validates a `CreateUserRequest` that breaks three rules at once — empty
name, an over-long bio, and an out-of-range age — and prints every violation.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"regexp"

	"example.com/validate"
)

type CreateUserRequest struct {
	Name  string
	Email string
	Bio   string
	Age   int
}

func main() {
	emailRe := regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

	req := CreateUserRequest{
		Name:  "",                         // violates NonEmpty
		Email: "alice@example.com",        // ok
		Bio:   "this bio is far too long", // violates MaxLen(10)
		Age:   200,                        // violates InRange(0,150)
	}

	err := validate.Validate(req,
		validate.NonEmpty("name", func(r CreateUserRequest) string { return r.Name }),
		validate.Matches("email", emailRe, func(r CreateUserRequest) string { return r.Email }),
		validate.MaxLen("bio", 10, func(r CreateUserRequest) string { return r.Bio }),
		validate.InRange("age", 0, 150, func(r CreateUserRequest) int { return r.Age }),
	)

	if err == nil {
		fmt.Println("valid")
		return
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		fmt.Println("-", err)
		return
	}
	for _, e := range joined.Unwrap() {
		fmt.Println("-", e)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
- value is empty: name: must not be empty (non_empty)
- value too long: bio: must be at most 10 chars, got 24 (max_len)
- value out of range: age: must be in [0,150], got 200 (in_range)
```

The valid email rule contributes nothing, so exactly three lines print — the three
broken rules, in the order they were listed. Order affects only the order of the
lines, never which failures appear.

### Tests

The main test feeds a request that breaks three rules and asserts all three surface
via `errors.Is` against each sentinel. A separate test recovers the `*FieldError`
with `errors.As` to read the field name. The order-independence test shuffles the
rule order and asserts the same set of failures appears. The contrast test runs the
same input through `ValidateFailFast` and asserts only one error comes back.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"regexp"
	"testing"
)

type req struct {
	Name string
	Bio  string
	Age  int
}

func rules() []Rule[req] {
	return []Rule[req]{
		NonEmpty("name", func(r req) string { return r.Name }),
		MaxLen("bio", 10, func(r req) string { return r.Bio }),
		InRange("age", 0, 150, func(r req) int { return r.Age }),
	}
}

func TestValidateAccumulatesAllFailures(t *testing.T) {
	t.Parallel()

	bad := req{Name: "", Bio: "way too long a bio", Age: 999}
	err := Validate(bad, rules()...)
	if err == nil {
		t.Fatal("want an error for a request breaking three rules")
	}
	for _, want := range []error{ErrEmpty, ErrTooLong, ErrOutOfRange} {
		if !errors.Is(err, want) {
			t.Fatalf("errors.Is did not find %v in %v", want, err)
		}
	}
}

func TestValidateValidRequestReturnsNil(t *testing.T) {
	t.Parallel()

	ok := req{Name: "alice", Bio: "short", Age: 30}
	if err := Validate(ok, rules()...); err != nil {
		t.Fatalf("valid request returned %v, want nil", err)
	}
}

func TestValidateOrderIndependent(t *testing.T) {
	t.Parallel()

	bad := req{Name: "", Bio: "way too long a bio", Age: 999}
	forward := Validate(bad, rules()...)

	rs := rules()
	reversed := []Rule[req]{rs[2], rs[1], rs[0]}
	backward := Validate(bad, reversed...)

	for _, want := range []error{ErrEmpty, ErrTooLong, ErrOutOfRange} {
		if !errors.Is(forward, want) || !errors.Is(backward, want) {
			t.Fatalf("rule order changed which failures appear for %v", want)
		}
	}
}

func TestFieldErrorRecoveredWithAs(t *testing.T) {
	t.Parallel()

	err := Validate(req{Name: ""}, NonEmpty("name", func(r req) string { return r.Name }))
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("errors.As did not recover *FieldError from %v", err)
	}
	if fe.Field != "name" || fe.Rule != "non_empty" {
		t.Fatalf("FieldError = %+v, want field=name rule=non_empty", fe)
	}
}

func TestFailFastStopsAtFirst(t *testing.T) {
	t.Parallel()

	bad := req{Name: "", Bio: "way too long a bio", Age: 999}
	err := ValidateFailFast(bad, rules()...)
	if !errors.Is(err, ErrEmpty) {
		t.Fatalf("fail-fast err = %v, want first failure ErrEmpty", err)
	}
	// Fail-fast must NOT surface the later failures.
	if errors.Is(err, ErrTooLong) || errors.Is(err, ErrOutOfRange) {
		t.Fatalf("fail-fast surfaced a later failure: %v", err)
	}
}

func TestMatchesRule(t *testing.T) {
	t.Parallel()

	re := regexp.MustCompile(`^\d{3}$`)
	rule := Matches("code", re, func(r req) string { return r.Name })
	if err := rule(req{Name: "123"}); err != nil {
		t.Fatalf("Matches rejected a valid value: %v", err)
	}
	if err := rule(req{Name: "12x"}); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("Matches err = %v, want ErrNoMatch", err)
	}
}
```

## Review

The pipeline is correct when a request breaking three rules yields one error whose
`errors.Is` matches all three sentinels, and a valid request yields `nil` with no
special case (empty `errors.Join` is nil). Wrapping each failure with two `%w`
verbs — a category sentinel and a `*FieldError` — is what lets the same value serve
both a coarse `errors.Is` classification and a structured `errors.As` field
readout. Rule order changes only the order of the joined causes, never the set,
which the order-independence test pins. Fail-fast is the wrong default at a
boundary precisely because it hides the later failures, as the contrast test shows;
reserve it for deeper layers where a first failure makes later work meaningless.

## Resources

- [errors package](https://pkg.go.dev/errors) — `Join`, `Is`, `As`, and multi-`%w` wrapping.
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — wrapping multiple errors with `%w`.
- [regexp package](https://pkg.go.dev/regexp) — `MustCompile`, `Regexp.MatchString`.

---

Back to [03-composable-comparators.md](03-composable-comparators.md) | Next: [05-memoize-singleflight.md](05-memoize-singleflight.md)
