# Exercise 7: Composable Validation Rules Over a Request DTO

Request validation is a pipeline of small rules composed two ways: `All` collects every
violation (a signup form wants all field errors at once), `First` short-circuits (a
cheap guard on a hot path). This module builds the generic `Rule[T]` type and both
combinators, plus real rules for a signup DTO.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests.

## What you'll build

```text
validate/                   independent module: example.com/validate
  go.mod                    go 1.26
  validate.go               Rule[T], All, First; signup rules + sentinels
  cmd/
    demo/
      main.go               runnable demo: valid, collect-all, fail-fast
  validate_test.go          All-joins-all, First-short-circuits, order tests
```

Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: `type Rule[T any] func(T) error`, `All(rules ...Rule[T]) Rule[T]` joining every violation with `errors.Join`, `First(rules ...Rule[T]) Rule[T]` returning the first, and real rules for a signup DTO (non-empty name, email shape, age bounds, enum membership).
Test: a valid DTO passes both with nil; an invalid DTO with three violations makes `All` unwrap to all three via `errors.Is`; `First` returns only the first; `All` is order-independent, `First` is order-dependent; an empty rule set is a nil-returning no-op.
Verify: `go test -count=1 -race ./...`

### Two combinators, two operational choices

A `Rule[T]` is a function that returns nil if `T` is valid or an error describing the
violation. Rules are tiny and independent; combinators assemble them.

`All(rules...)` runs every rule and joins the violations with `errors.Join`. The
resulting error's `Unwrap() []error` lets `errors.Is` match any individual sentinel, so
the caller — a signup HTTP handler — can report every field the user got wrong in one
response. This is the right UX for a form: nobody wants to fix one field, resubmit, and
discover the next error. `All` is order-independent: the joined set is the same
regardless of rule order (though the display order follows rule order).

`First(rules...)` returns the first rule's error and stops. This is the right choice for
a cheap guard on a hot path — an idempotency-key check, a size limit — where you do not
want to run an expensive rule after a cheap one already rejected the input. `First` is
order-dependent by design: put the cheapest, most-likely-to-fail rules first.

Both combinators return a `Rule[T]`, so they compose: `All(First(cheapChecks...),
expensiveCheck)` is a valid rule. An empty rule set is a no-op that returns nil — the
identity, so `All()` and `First()` are safe to call on a dynamically-built slice that
might be empty.

The rules themselves are real: a non-empty trimmed name counted in runes (not bytes, so
multibyte names are handled), an RFC-ish email shape, an age within bounds, and
membership in an enum of allowed plans. Each returns a `%w`-wrapped sentinel so the test
and the caller can branch on the specific violation.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Rule validates a value, returning nil if valid or a violation error.
type Rule[T any] func(T) error

// All runs every rule and joins their violations. An empty set returns nil.
func All[T any](rules ...Rule[T]) Rule[T] {
	return func(v T) error {
		var errs []error
		for _, rule := range rules {
			if err := rule(v); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
}

// First runs rules in order and returns the first violation, or nil.
func First[T any](rules ...Rule[T]) Rule[T] {
	return func(v T) error {
		for _, rule := range rules {
			if err := rule(v); err != nil {
				return err
			}
		}
		return nil
	}
}

// Signup is the DTO under validation.
type Signup struct {
	Name  string
	Email string
	Age   int
	Plan  string
}

var (
	ErrEmptyName     = errors.New("name must not be empty")
	ErrBadEmail      = errors.New("email is malformed")
	ErrAgeOutOfRange = errors.New("age out of range")
	ErrUnknownPlan   = errors.New("unknown plan")
)

var validPlans = []string{"free", "pro", "enterprise"}

// NonEmptyName rejects a blank or whitespace-only name, counting runes.
func NonEmptyName(s Signup) error {
	if utf8.RuneCountInString(strings.TrimSpace(s.Name)) == 0 {
		return fmt.Errorf("%w", ErrEmptyName)
	}
	return nil
}

// ValidEmail applies a minimal RFC-ish shape check: one @, non-empty local and
// domain, and a dot in the domain.
func ValidEmail(s Signup) error {
	at := strings.IndexByte(s.Email, '@')
	if at <= 0 || at == len(s.Email)-1 {
		return fmt.Errorf("%w: %q", ErrBadEmail, s.Email)
	}
	domain := s.Email[at+1:]
	if strings.IndexByte(s.Email[at+1:], '@') != -1 || !strings.Contains(domain, ".") {
		return fmt.Errorf("%w: %q", ErrBadEmail, s.Email)
	}
	return nil
}

// AgeInRange bounds age to [13, 120].
func AgeInRange(s Signup) error {
	if s.Age < 13 || s.Age > 120 {
		return fmt.Errorf("%w: %d", ErrAgeOutOfRange, s.Age)
	}
	return nil
}

// KnownPlan checks enum membership.
func KnownPlan(s Signup) error {
	if !slices.Contains(validPlans, s.Plan) {
		return fmt.Errorf("%w: %q", ErrUnknownPlan, s.Plan)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/validate"
)

func main() {
	rules := validate.All(
		validate.NonEmptyName,
		validate.ValidEmail,
		validate.AgeInRange,
		validate.KnownPlan,
	)

	good := validate.Signup{Name: "Alice", Email: "alice@example.com", Age: 30, Plan: "pro"}
	fmt.Println("valid signup err:", rules(good))

	bad := validate.Signup{Name: "", Email: "nope", Age: 5, Plan: "gold"}
	fmt.Println("collect-all err:", rules(bad))

	guard := validate.First(validate.NonEmptyName, validate.ValidEmail)
	fmt.Println("fail-fast err:", guard(bad))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid signup err: <nil>
collect-all err: name must not be empty
email is malformed: "nope"
age out of range: 5
unknown plan: "gold"
fail-fast err: name must not be empty
```

### Tests

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidPassesBoth(t *testing.T) {
	t.Parallel()
	s := Signup{Name: "Alice", Email: "alice@example.com", Age: 30, Plan: "free"}
	all := All(NonEmptyName, ValidEmail, AgeInRange, KnownPlan)
	first := First(NonEmptyName, ValidEmail, AgeInRange, KnownPlan)
	if err := all(s); err != nil {
		t.Fatalf("All(valid) = %v, want nil", err)
	}
	if err := first(s); err != nil {
		t.Fatalf("First(valid) = %v, want nil", err)
	}
}

func TestAllJoinsEveryViolation(t *testing.T) {
	t.Parallel()
	s := Signup{Name: "", Email: "nope", Age: 5, Plan: "free"} // 3 violations
	err := All(NonEmptyName, ValidEmail, AgeInRange, KnownPlan)(s)
	for _, want := range []error{ErrEmptyName, ErrBadEmail, ErrAgeOutOfRange} {
		if !errors.Is(err, want) {
			t.Errorf("joined error missing %v; got %v", want, err)
		}
	}
	if errors.Is(err, ErrUnknownPlan) {
		t.Errorf("plan was valid; error should not include ErrUnknownPlan: %v", err)
	}
}

func TestFirstShortCircuits(t *testing.T) {
	t.Parallel()
	s := Signup{Name: "", Email: "nope", Age: 5, Plan: "gold"}
	err := First(NonEmptyName, ValidEmail, AgeInRange, KnownPlan)(s)
	if !errors.Is(err, ErrEmptyName) {
		t.Fatalf("First = %v, want only ErrEmptyName", err)
	}
	if errors.Is(err, ErrBadEmail) {
		t.Fatalf("First should not have run later rules: %v", err)
	}
}

func TestAllIsOrderIndependent(t *testing.T) {
	t.Parallel()
	s := Signup{Name: "", Email: "nope", Age: 5, Plan: "free"}
	a := All(NonEmptyName, ValidEmail, AgeInRange)(s)
	b := All(AgeInRange, ValidEmail, NonEmptyName)(s)
	for _, want := range []error{ErrEmptyName, ErrBadEmail, ErrAgeOutOfRange} {
		if errors.Is(a, want) != errors.Is(b, want) {
			t.Fatalf("All order changed membership of %v", want)
		}
	}
}

func TestFirstIsOrderDependent(t *testing.T) {
	t.Parallel()
	s := Signup{Name: "", Email: "nope", Age: 5, Plan: "free"}
	nameFirst := First(NonEmptyName, ValidEmail)(s)
	emailFirst := First(ValidEmail, NonEmptyName)(s)
	if !errors.Is(nameFirst, ErrEmptyName) {
		t.Errorf("name-first = %v, want ErrEmptyName", nameFirst)
	}
	if !errors.Is(emailFirst, ErrBadEmail) {
		t.Errorf("email-first = %v, want ErrBadEmail", emailFirst)
	}
}

func TestEmptyRuleSetIsNoOp(t *testing.T) {
	t.Parallel()
	s := Signup{}
	if err := All[Signup]()(s); err != nil {
		t.Errorf("All() = %v, want nil", err)
	}
	if err := First[Signup]()(s); err != nil {
		t.Errorf("First() = %v, want nil", err)
	}
}

func ExampleAll() {
	s := Signup{Name: "", Email: "bad", Age: 200, Plan: "free"}
	err := All(NonEmptyName, AgeInRange)(s)
	fmt.Println(errors.Is(err, ErrEmptyName), errors.Is(err, ErrAgeOutOfRange))
	// Output: true true
}
```

## Review

The pipeline is correct when the two combinators have opposite error semantics and both
are honest about order. `All` must run every rule and join with `errors.Join` so the
result unwraps — via `errors.Is` — to each sentinel that actually failed and to none
that passed; `TestAllJoinsEveryViolation` asserts exactly that, including the negative
(a valid plan must not appear). `First` must stop at the first failure, so a later rule
never runs — the order-dependence test proves swapping rule order swaps which sentinel
comes back. The empty-set identity matters for dynamically-assembled rule slices. Each
rule wraps its sentinel with `%w`; a rule that returned a bare `fmt.Errorf` string would
pass the compile but break every `errors.Is` branch, which is the single most common bug
in a validation layer.

## Resources

- [errors.Join and multi-error unwrapping](https://pkg.go.dev/errors#Join)
- [unicode/utf8.RuneCountInString](https://pkg.go.dev/unicode/utf8#RuneCountInString)
- [strings package](https://pkg.go.dev/strings)
- [slices.Contains](https://pkg.go.dev/slices#Contains)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-event-dispatch-table.md](06-event-dispatch-table.md) | Next: [08-memoize-and-lazy-init.md](08-memoize-and-lazy-init.md)
