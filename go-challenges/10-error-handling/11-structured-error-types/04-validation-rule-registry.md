# Exercise 4: Composable, Reusable Validation Rules

Ad-hoc `if` checks scattered across every DTO drift: two handlers spell the same
rule differently, produce different codes, and the wire contract fragments. This
module refactors validation into a small reusable rule library — generic
constructors that each produce a stable `Code` — so a rule is declared once and
applied everywhere.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
rules/                     independent module: example.com/rules
  go.mod                   go 1.26
  rules.go                 type Rule[T]; Required/MinLen/MaxLen/Matches/Range; Check
  cmd/
    demo/
      main.go              runnable demo: apply a rule set to a field, print codes
  rules_test.go            per-constructor unit tests; two failing rules -> two errors
```

- Files: `rules.go`, `cmd/demo/main.go`, `rules_test.go`.
- Implement: `Rule[T]` (a func from a field name + value to `*FieldError` or `nil`); generic constructors `Required[T comparable]()`, `MinLen(n)`, `MaxLen(n)`, `Matches(re, code)`, `Range[T cmp.Ordered](lo, hi)`; and `Check` applying an ordered `[]Rule[T]` and collecting all failures.
- Test: each constructor in isolation (`Required("")` -> required, `MaxLen(3)` on `"abcd"` -> max_len with `Params["max"] == 3`, `Matches(emailRe)` on bad input -> pattern, `Range(0, 150)` on `200` -> range); a field with two failing rules yields two `*FieldError`s.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### A Rule is a typed closure, compiled once

A `Rule[T]` is `func(field string, value T) *FieldError` — it inspects one value
and returns a failure or `nil`. Constructors are generic so `Required` works for
any `comparable` type and `Range` for any `cmp.Ordered` type, while the string
rules (`MinLen`, `MaxLen`, `Matches`) are `Rule[string]`. Each constructor bakes
in a stable `Code`, so every place that uses `MaxLen` produces the identical
`max_len` code with the same `Params` shape — that consistency is the point of a
registry over inline `if`s.

Two production details matter. First, the regexp is compiled *once* with
`regexp.MustCompile` at package scope, never inside the rule closure — compiling a
pattern per call is a needless cost and `MustCompile` fails loudly at init if the
pattern is malformed. Second, `Check` does not short-circuit: it applies every
rule in the ordered slice and collects all failures, matching the collect-all
contract from module 01 at the single-field granularity.

Create `rules.go`:

```go
package rules

import (
	"cmp"
	"fmt"
	"regexp"
)

type Code string

const (
	CodeRequired Code = "required"
	CodeMinLen   Code = "min_len"
	CodeMaxLen   Code = "max_len"
	CodePattern  Code = "pattern"
	CodeRange    Code = "range"
)

type FieldError struct {
	Code   Code
	Field  string
	Params map[string]any
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Code)
}

// Rule inspects one value and returns a FieldError or nil.
type Rule[T any] func(field string, value T) *FieldError

// Required fails when value is the zero value of its type.
func Required[T comparable]() Rule[T] {
	return func(field string, value T) *FieldError {
		var zero T
		if value == zero {
			return &FieldError{Code: CodeRequired, Field: field}
		}
		return nil
	}
}

// MinLen fails when a string is shorter than n runes.
func MinLen(n int) Rule[string] {
	return func(field string, value string) *FieldError {
		if len([]rune(value)) < n {
			return &FieldError{Code: CodeMinLen, Field: field, Params: map[string]any{"min": n}}
		}
		return nil
	}
}

// MaxLen fails when a string is longer than n runes.
func MaxLen(n int) Rule[string] {
	return func(field string, value string) *FieldError {
		if len([]rune(value)) > n {
			return &FieldError{Code: CodeMaxLen, Field: field, Params: map[string]any{"max": n}}
		}
		return nil
	}
}

// Matches fails when value does not match re. The caller supplies the Code so a
// pattern rule can carry a domain-specific code.
func Matches(re *regexp.Regexp, code Code) Rule[string] {
	return func(field string, value string) *FieldError {
		if !re.MatchString(value) {
			return &FieldError{Code: code, Field: field}
		}
		return nil
	}
}

// Range fails when value is outside [lo, hi]. cmp.Ordered admits any ordered
// numeric or string type.
func Range[T cmp.Ordered](lo, hi T) Rule[T] {
	return func(field string, value T) *FieldError {
		if value < lo || value > hi {
			return &FieldError{Code: CodeRange, Field: field, Params: map[string]any{"min": lo, "max": hi}}
		}
		return nil
	}
}

// Check applies every rule in order and collects all failures (no
// short-circuit).
func Check[T any](field string, value T, rules ...Rule[T]) []*FieldError {
	var out []*FieldError
	for _, r := range rules {
		if fe := r(field, value); fe != nil {
			out = append(out, fe)
		}
	}
	return out
}
```

### The runnable demo

The demo declares an email regexp at package scope and applies a set of rules to
one bad field, printing the codes it collected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"regexp"

	"example.com/rules"
)

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func main() {
	fes := rules.Check("email", "not-an-email",
		rules.Required[string](),
		rules.MaxLen(5),
		rules.Matches(emailRe, rules.CodePattern),
	)
	for _, fe := range fes {
		fmt.Println(fe.Field, fe.Code)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
email max_len
email pattern
```

The `Required` rule passes ("not-an-email" is non-empty), `MaxLen(5)` fails (it
is longer than five), and `Matches` fails (no `@`/domain), so two failures are
collected in rule order.

### Tests

Each constructor is unit-tested in isolation: the positive case (a failure with
the expected `Code` and `Params`) and, where cheap, the negative case (`nil`).
The final test proves `Check` collects two failures from two failing rules rather
than stopping at the first.

Create `rules_test.go`:

```go
package rules

import (
	"regexp"
	"testing"
)

var testEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func TestRequired(t *testing.T) {
	t.Parallel()

	r := Required[string]()
	if fe := r("name", ""); fe == nil || fe.Code != CodeRequired {
		t.Fatalf("Required(\"\") = %+v, want required", fe)
	}
	if fe := r("name", "Alice"); fe != nil {
		t.Fatalf("Required(non-empty) = %+v, want nil", fe)
	}
}

func TestMaxLen(t *testing.T) {
	t.Parallel()

	fe := MaxLen(3)("name", "abcd")
	if fe == nil || fe.Code != CodeMaxLen {
		t.Fatalf("MaxLen(3)(abcd) = %+v, want max_len", fe)
	}
	if got, ok := fe.Params["max"].(int); !ok || got != 3 {
		t.Fatalf("Params[max] = %v, want 3", fe.Params["max"])
	}
}

func TestMinLen(t *testing.T) {
	t.Parallel()

	if fe := MinLen(3)("name", "ab"); fe == nil || fe.Code != CodeMinLen {
		t.Fatalf("MinLen(3)(ab) = %+v, want min_len", fe)
	}
	if fe := MinLen(3)("name", "abc"); fe != nil {
		t.Fatalf("MinLen(3)(abc) = %+v, want nil", fe)
	}
}

func TestMatches(t *testing.T) {
	t.Parallel()

	if fe := Matches(testEmailRe, CodePattern)("email", "nope"); fe == nil || fe.Code != CodePattern {
		t.Fatalf("Matches(bad) = %+v, want pattern", fe)
	}
	if fe := Matches(testEmailRe, CodePattern)("email", "a@b.co"); fe != nil {
		t.Fatalf("Matches(good) = %+v, want nil", fe)
	}
}

func TestRange(t *testing.T) {
	t.Parallel()

	fe := Range(0, 150)("age", 200)
	if fe == nil || fe.Code != CodeRange {
		t.Fatalf("Range(0,150)(200) = %+v, want range", fe)
	}
	if fe := Range(0, 150)("age", 30); fe != nil {
		t.Fatalf("Range(0,150)(30) = %+v, want nil", fe)
	}
}

func TestCheckCollectsAll(t *testing.T) {
	t.Parallel()

	fes := Check("email", "x",
		Required[string](),
		MaxLen(0),
		Matches(testEmailRe, CodePattern),
	)
	if len(fes) != 2 {
		t.Fatalf("Check collected %d, want 2: %+v", len(fes), fes)
	}
}
```

An `Example` verified against its `// Output:` comment:

```go
// rules_example_test.go
package rules

import "fmt"

func ExampleCheck() {
	for _, fe := range Check("age", 200, Range(0, 150)) {
		fmt.Println(fe.Field, fe.Code)
	}
	// Output: age range
}
```

## Review

The library is correct when each constructor produces exactly its stable `Code`
and the documented `Params` regardless of caller — `MaxLen(3)` always yields
`max_len` with `Params["max"] == 3` — and `Check` collects every failure in rule
order instead of returning at the first. The two production traps this module
closes: compiling the regexp inside the closure (compile once at package scope
with `MustCompile`) and short-circuiting the rule loop (a field can break two
rules at once; report both). Rules being generic (`Required[T comparable]`,
`Range[T cmp.Ordered]`) is what lets one definition serve every DTO. Run
`go test -race`.

## Resources

- [`regexp.MustCompile`](https://pkg.go.dev/regexp#MustCompile) — compile a pattern once at init; panics on a bad pattern.
- [`cmp.Ordered`](https://pkg.go.dev/cmp#Ordered) — the constraint that makes `Range` generic over ordered types.
- [Go spec: type parameters](https://go.dev/ref/spec#Type_parameter_declarations) — how the generic constructors are instantiated.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-dotted-path-nested-validator.md](03-dotted-path-nested-validator.md) | Next: [05-field-error-json-public-contract.md](05-field-error-json-public-contract.md)
