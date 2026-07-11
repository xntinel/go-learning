# Exercise 11: A Validation Pipeline as a Slice of Anonymous Functions

**Nivel: Intermedio** — validacion rapida (un test corto).

A signup handler often needs to check several independent things about a
request — a well-formed email, a minimum password length, a minimum age —
and report every problem at once instead of stopping at the first one. This
module builds that check as a slice of small anonymous functions, each one a
single rule, run in order by one generic `Validate`.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
signupvalidate/               module example.com/signupvalidate
  go.mod
  validate.go                 SignupRequest; rules ([]func(SignupRequest) error); Validate
  validate_test.go            all-pass case, each rule failing individually, all three failing
```

- Files: `validate.go`, `validate_test.go`.
- Implement: a package-level `rules` slice of anonymous `func(SignupRequest) error` values, one per check (email, password length, age); `Validate(req) []error` that runs every rule and collects the non-nil results.
- Test: a request that satisfies every rule yields no errors; a request failing exactly one rule yields exactly one error; a request failing all three yields three errors.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/signupvalidate
cd ~/go-exercises/signupvalidate
go mod init example.com/signupvalidate
go mod edit -go=1.24
```

### Why a slice of function values, not a chain of ifs

A hand-written chain of `if` statements couples "what to check" with "how to
report it," and adding a rule means editing the function body. Storing each
rule as an anonymous function in a slice inverts that: the rule *is* the
function value, `Validate` only knows how to call one and collect its error,
and adding a fourth check for age minimum or password entropy is one more
entry in the table — no change to the loop that runs them. Each literal
closes over nothing but its own logic; the request itself always arrives as
the parameter, not a captured variable, so every rule sees the exact request
under test.

Create `validate.go`:

```go
package signupvalidate

import (
	"fmt"
	"strings"
)

// SignupRequest is the raw input for a new-account signup.
type SignupRequest struct {
	Email    string
	Password string
	Age      int
}

// rules is a table of independent checks, each an anonymous function value.
// Every rule inspects the request and returns a non-nil error when it fails;
// Validate runs all of them and collects every failure instead of stopping
// at the first one, so a caller can report every problem in one response.
var rules = []func(SignupRequest) error{
	func(r SignupRequest) error {
		if !strings.Contains(r.Email, "@") {
			return fmt.Errorf("email %q: missing @", r.Email)
		}
		return nil
	},
	func(r SignupRequest) error {
		if len(r.Password) < 8 {
			return fmt.Errorf("password: must be at least 8 characters, got %d", len(r.Password))
		}
		return nil
	},
	func(r SignupRequest) error {
		if r.Age < 13 {
			return fmt.Errorf("age %d: must be at least 13", r.Age)
		}
		return nil
	},
}

// Validate runs every rule against req and returns the collected failures in
// rule order. A nil-length result means req passed every rule.
func Validate(req SignupRequest) []error {
	var errs []error
	for _, rule := range rules {
		if err := rule(req); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
```

### Tests

`TestValidateAllPass` confirms a fully valid request yields no errors.
`TestValidateEachRule` is table-driven over four cases: one failure per rule
in isolation, then all three at once, asserting the exact error count each
time.

Create `validate_test.go`:

```go
package signupvalidate

import "testing"

func TestValidateAllPass(t *testing.T) {
	t.Parallel()
	req := SignupRequest{Email: "a@b.com", Password: "longenough", Age: 20}
	if errs := Validate(req); len(errs) != 0 {
		t.Fatalf("Validate() = %v, want no errors", errs)
	}
}

func TestValidateEachRule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		req     SignupRequest
		wantLen int
	}{
		{"bad email", SignupRequest{Email: "nope", Password: "longenough", Age: 20}, 1},
		{"short password", SignupRequest{Email: "a@b.com", Password: "short", Age: 20}, 1},
		{"too young", SignupRequest{Email: "a@b.com", Password: "longenough", Age: 10}, 1},
		{"all three fail", SignupRequest{Email: "nope", Password: "x", Age: 5}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Validate(tc.req); len(got) != tc.wantLen {
				t.Fatalf("Validate(%+v) = %d errors (%v), want %d", tc.req, len(got), got, tc.wantLen)
			}
		})
	}
}
```

## Review

The important property is that `Validate` never needs to change when a rule
changes: `rules` is data, and each entry is a self-contained anonymous
function that owns exactly one check and one error message. The table test
proves both that passing rules stay silent and that failing rules are
additive — three broken fields produce three errors, not one. This is the
same shape as request-body validation in a real HTTP handler, just without
the transport code.

## Resources

- [Function types](https://go.dev/ref/spec#Function_types)
- [errors package](https://pkg.go.dev/errors)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-sort-ranking-anonymous-comparator.md](10-sort-ranking-anonymous-comparator.md) | Next: [12-iife-lookup-table-order-transitions.md](12-iife-lookup-table-order-transitions.md)
