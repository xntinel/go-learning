# Exercise 6: Request Validation Pipeline: Building a Closure Slice from Rules

A validation engine compiles a slice of rule specs into a `[]func(Request) error`
by capturing each rule's parameters in a closure, then runs the chain and
aggregates failures with `errors.Join`. Building a slice of closures from a loop
is the second classic capture trap after goroutines: get it wrong and every
compiled rule reports the last spec. You build the correct per-rule capture and a
buggy shared-spec version side by side, and prove which one names the right field.

## What you'll build

```text
validate/                    independent module: example.com/validate
  go.mod                     go 1.26
  validate.go                Request, Rule; Compile (per-rule capture), CompileShared (buggy), Run
  cmd/
    demo/
      main.go                runnable demo: compile rules, validate a request, print errors
  validate_test.go           right-field-per-rule, shared-bug demonstration, join order
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `Compile(rules)` returning one closure per rule that captures that rule's field/bound/message; `CompileShared(rules)` (the anti-pattern that captures the loop variable by pointer so all closures share one spec); `Run(validators, req)` aggregating with `errors.Join`.
- Test: feed requests that violate specific rules and assert the aggregated error names the RIGHT field per rule; demonstrate the shared version misreports; a valid request returns nil; `errors.Join` preserves order and unwrapping.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/validate/cmd/demo
cd ~/go-exercises/validate
go mod init example.com/validate
go mod edit -go=1.26
```

### Why each closure must capture its own rule

A rule spec names a field, a minimum length, and a message. `Compile` turns each
spec into a `func(Request) error` that checks that one field. The closures are
stored in a slice and run later, so they must each carry their own spec. On a
`go 1.26` module the `range` variable is per-iteration, so capturing `rule`
directly is correct — `Compile` does exactly that.

To make the failure mode concrete rather than hypothetical, `CompileShared`
reproduces the bug on purpose in a way the language change does NOT fix: it takes
the address of a single shared spec variable and every closure reads through that
pointer, so all compiled rules observe whatever value the pointer last held.
This is the shape the per-iteration variable cannot save you from — an explicit
shared pointer, or a `for i := 0; ...` loop over `rules[i]` where a helper
captures a shared `i`. It stands in for the real bug: a slice of validators that
all report the last rule's field, so a client that violated the "email" rule gets
told "password too short."

`Run` executes every validator against the request and joins the non-nil errors
with `errors.Join`, which preserves order and yields an error whose `Unwrap()
[]error` lets `errors.Is` match any wrapped sentinel. Each rule wraps a package
sentinel `ErrTooShort` with `%w` so callers can branch on the failure kind while
still reading the field name from the message.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
)

// ErrTooShort is the sentinel every length rule wraps, so callers can match the
// failure kind with errors.Is regardless of which field failed.
var ErrTooShort = errors.New("value too short")

// Request is the payload being validated: a map of field name to value.
type Request map[string]string

// Rule specifies a minimum length for one field.
type Rule struct {
	Field  string
	MinLen int
}

// Validator checks one aspect of a Request.
type Validator func(Request) error

// Compile turns each rule into its own closure that captures that rule. Correct:
// each validator reports its own field.
func Compile(rules []Rule) []Validator {
	validators := make([]Validator, 0, len(rules))
	for _, rule := range rules {
		validators = append(validators, func(req Request) error {
			if len(req[rule.Field]) < rule.MinLen {
				return fmt.Errorf("field %q: need >= %d chars: %w", rule.Field, rule.MinLen, ErrTooShort)
			}
			return nil
		})
	}
	return validators
}

// CompileShared is the anti-pattern: every closure reads through a pointer to one
// shared spec, so all validators report whatever spec the pointer last held. The
// per-iteration loop variable does NOT fix an explicit shared pointer.
func CompileShared(rules []Rule) []Validator {
	validators := make([]Validator, 0, len(rules))
	var shared Rule
	for _, rule := range rules {
		shared = rule
		validators = append(validators, func(req Request) error {
			if len(req[shared.Field]) < shared.MinLen {
				return fmt.Errorf("field %q: need >= %d chars: %w", shared.Field, shared.MinLen, ErrTooShort)
			}
			return nil
		})
	}
	return validators
}

// Run applies every validator and joins the failures in order.
func Run(validators []Validator, req Request) error {
	var errs []error
	for _, v := range validators {
		if err := v(req); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

### The runnable demo

The demo compiles three rules, validates a request that violates two of them, and
prints the joined error — showing each failing rule naming its own field.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/validate"
)

func main() {
	rules := []validate.Rule{
		{Field: "username", MinLen: 3},
		{Field: "password", MinLen: 8},
		{Field: "email", MinLen: 5},
	}
	validators := validate.Compile(rules)

	req := validate.Request{"username": "ab", "password": "secret12", "email": "x"}
	if err := validate.Run(validators, req); err != nil {
		fmt.Println(err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
field "username": need >= 3 chars: value too short
field "email": need >= 5 chars: value too short
```

### Tests

`TestEachRuleReportsOwnField` compiles rules and asserts a request violating a
specific rule produces an error naming that rule's field. `TestSharedCompile
MisreportsField` demonstrates the anti-pattern: `CompileShared` makes every
validator report the last rule's field, so a violation of the first field is
misattributed. `TestValidRequestReturnsNil` and the sentinel check round it out.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func rules() []Rule {
	return []Rule{
		{Field: "username", MinLen: 3},
		{Field: "password", MinLen: 8},
		{Field: "email", MinLen: 5},
	}
}

func TestEachRuleReportsOwnField(t *testing.T) {
	t.Parallel()

	validators := Compile(rules())

	cases := []struct {
		name  string
		req   Request
		field string
	}{
		{"short username", Request{"username": "a", "password": "longenough", "email": "a@b.c"}, "username"},
		{"short password", Request{"username": "abc", "password": "x", "email": "a@b.c"}, "password"},
		{"short email", Request{"username": "abc", "password": "longenough", "email": "a"}, "email"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Run(validators, tc.req)
			if err == nil {
				t.Fatalf("want error naming %q, got nil", tc.field)
			}
			if !errors.Is(err, ErrTooShort) {
				t.Errorf("error does not wrap ErrTooShort: %v", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error = %q, want it to name field %q", err.Error(), tc.field)
			}
		})
	}
}

func TestSharedCompileMisreportsField(t *testing.T) {
	t.Parallel()

	correct := Compile(rules())
	shared := CompileShared(rules())

	// Violate ONLY the username rule; email is valid (5 chars, min 5).
	req := Request{"username": "a", "password": "longenough", "email": "a@b.c"}

	// The correct compile catches it and names username.
	cerr := Run(correct, req)
	if cerr == nil || !strings.Contains(cerr.Error(), "username") {
		t.Fatalf("correct compile error = %v, want it to name username", cerr)
	}

	// The shared compile has every validator checking the LAST rule (email),
	// which is valid, so it silently MISSES the username violation. That is the
	// bug: a shared-capture validator chain lets an invalid request through.
	if serr := Run(shared, req); serr != nil {
		t.Fatalf("shared compile error = %v, want nil (it wrongly checks only the last rule)", serr)
	}
}

func TestValidRequestReturnsNil(t *testing.T) {
	t.Parallel()

	validators := Compile(rules())
	req := Request{"username": "abc", "password": "longenough", "email": "a@b.c"}
	if err := Run(validators, req); err != nil {
		t.Fatalf("valid request returned error: %v", err)
	}
}

func ExampleRun() {
	validators := Compile([]Rule{{Field: "name", MinLen: 2}})
	err := Run(validators, Request{"name": "x"})
	fmt.Println(err)
	// Output: field "name": need >= 2 chars: value too short
}
```

## Review

The engine is correct when each compiled rule reports its own field, a valid
request returns nil, and every failure wraps `ErrTooShort` so `errors.Is`
matches. `TestEachRuleReportsOwnField` is the capture guard, and
`TestSharedCompileMisreportsField` is its foil — it demonstrates the exact bug
that an explicit shared reference (which the 1.22 change does not fix) produces,
so the difference between capturing per-rule and capturing a shared cell is
encoded in the suite. `errors.Join` preserving order is what makes the demo's two
lines appear in rule order. Run `go test -race` even though this is
single-threaded; the validators are pure and must stay so.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — ordered aggregation with multi-error `Unwrap`.
- [`fmt.Errorf` with `%w`](https://pkg.go.dev/fmt#Errorf) — wrapping a sentinel so `errors.Is` matches.
- [Go spec: For statements and range clause](https://go.dev/ref/spec#For_statements) — per-iteration loop variables.

---

Back to [00-concepts.md](00-concepts.md) | Next: [07-token-bucket-closure-state.md](07-token-bucket-closure-state.md)
