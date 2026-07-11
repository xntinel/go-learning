# Exercise 8: Request Validation Pipeline over a Variadic Set of Rules

A handler that rejects an invalid request body on the *first* bad field forces the
client into a guess-and-retry loop. The better contract reports every failure at
once. You build `Validate[T any](v T, rules ...func(T) error) error` — a variadic
list of rule functions aggregated with `errors.Join` — so a single call returns
all violations, each still discoverable with `errors.Is`.

This module is self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
validate/                  independent module: example.com/validate
  go.mod                   go 1.25
  validate.go              Validate[T]; sentinel errors; example rules for a struct
  cmd/
    demo/
      main.go              runnable demo: validate a good and a bad request
  validate_test.go         all-pass nil, multi-fail Join, errors.Is per sentinel
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `Validate[T any](v T, rules ...func(T) error) error` running every rule and joining failures with `errors.Join`; sentinel errors wrapped with `%w`.
- Test: all-pass returns nil; two failing rules produce a joined error where `errors.Is` finds each sentinel; non-nil only when at least one rule fails; rule order does not change which sentinels are reachable.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/validate/cmd/demo
cd ~/go-exercises/validate
go mod init example.com/validate
go mod edit -go=1.25
```

### Variadic-of-functions plus errors.Join

The rules are homogeneous — each is a `func(T) error` that inspects the value and
returns nil or a wrapped sentinel — so a variadic list of them is the natural
shape. `Validate` runs *every* rule (it does not stop at the first failure),
collects each result into a slice, and hands the whole slice to `errors.Join`. Two
properties of `errors.Join` make this compose cleanly: it drops nil arguments, and
it returns nil when every argument is nil. So the loop can append each rule's
result unconditionally, and `Validate` returns non-nil exactly when at least one
rule failed. No manual "did any fail?" bookkeeping is needed.

The generic `T` lets one `Validate` serve any value type — a `CreateUserRequest`,
a config struct, a query DTO — while each rule stays strongly typed to `T`. The
rules wrap package-level sentinels with `%w` (`fmt.Errorf("email: %w",
ErrRequired)`), which is what lets a handler downstream ask `errors.Is(err,
ErrRequired)` and branch, and lets a client-facing layer map each sentinel to a
field error. Because `errors.Join` builds a tree and `errors.Is` traverses it,
every wrapped sentinel remains reachable regardless of how many rules failed or in
what order they ran — the test pins that order-independence explicitly.

Create `validate.go`:

```go
// validate.go
package validate

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors let callers branch on the kind of failure with errors.Is.
var (
	ErrRequired = errors.New("required")
	ErrTooLong  = errors.New("too long")
	ErrFormat   = errors.New("invalid format")
)

// Validate runs every rule against v and aggregates failures with errors.Join.
// It returns nil only when all rules pass.
func Validate[T any](v T, rules ...func(T) error) error {
	errs := make([]error, 0, len(rules))
	for _, rule := range rules {
		errs = append(errs, rule(v))
	}
	return errors.Join(errs...)
}

// CreateUserRequest is an example payload with rules below.
type CreateUserRequest struct {
	Name  string
	Email string
}

// NameRequired fails with ErrRequired when Name is empty.
func NameRequired(r CreateUserRequest) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name: %w", ErrRequired)
	}
	return nil
}

// NameMaxLen fails with ErrTooLong when Name exceeds max runes.
func NameMaxLen(max int) func(CreateUserRequest) error {
	return func(r CreateUserRequest) error {
		if len([]rune(r.Name)) > max {
			return fmt.Errorf("name: %w", ErrTooLong)
		}
		return nil
	}
}

// EmailFormat fails with ErrFormat when Email lacks an '@'.
func EmailFormat(r CreateUserRequest) error {
	if !strings.Contains(r.Email, "@") {
		return fmt.Errorf("email: %w", ErrFormat)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"errors"
	"fmt"

	"example.com/validate"
)

func main() {
	rules := []func(validate.CreateUserRequest) error{
		validate.NameRequired,
		validate.NameMaxLen(20),
		validate.EmailFormat,
	}

	good := validate.CreateUserRequest{Name: "alice", Email: "alice@example.com"}
	fmt.Println("good:", validate.Validate(good, rules...))

	bad := validate.CreateUserRequest{Name: "", Email: "not-an-email"}
	err := validate.Validate(bad, rules...)
	fmt.Println("bad:", err)
	fmt.Println("is required:", errors.Is(err, validate.ErrRequired))
	fmt.Println("is format:", errors.Is(err, validate.ErrFormat))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: <nil>
bad: name: required
email: invalid format
is required: true
is format: true
```

Note the joined error prints as two lines: `errors.Join` renders each wrapped
error on its own line, which is exactly the multi-field report you want.

### Tests

`TestAllPassIsNil` proves `errors.Join` collapses all-nil to nil.
`TestMultiFailReachableSentinels` proves both failing sentinels are found by
`errors.Is`. `TestOrderIndependence` runs the same failing rules in a different
order and asserts the same sentinels remain reachable.

Create `validate_test.go`:

```go
// validate_test.go
package validate

import (
	"errors"
	"fmt"
	"testing"
)

func TestAllPassIsNil(t *testing.T) {
	t.Parallel()

	r := CreateUserRequest{Name: "alice", Email: "alice@example.com"}
	if err := Validate(r, NameRequired, NameMaxLen(20), EmailFormat); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}

func TestMultiFailReachableSentinels(t *testing.T) {
	t.Parallel()

	r := CreateUserRequest{Name: "", Email: "not-an-email"}
	err := Validate(r, NameRequired, EmailFormat)
	if err == nil {
		t.Fatal("Validate = nil, want an error")
	}
	if !errors.Is(err, ErrRequired) {
		t.Error("errors.Is(err, ErrRequired) = false, want true")
	}
	if !errors.Is(err, ErrFormat) {
		t.Error("errors.Is(err, ErrFormat) = false, want true")
	}
}

func TestSingleFail(t *testing.T) {
	t.Parallel()

	r := CreateUserRequest{Name: "alice", Email: "bad"}
	err := Validate(r, NameRequired, EmailFormat)
	if !errors.Is(err, ErrFormat) {
		t.Errorf("want ErrFormat, got %v", err)
	}
	if errors.Is(err, ErrRequired) {
		t.Error("ErrRequired should not be reachable when name is valid")
	}
}

func TestOrderIndependence(t *testing.T) {
	t.Parallel()

	r := CreateUserRequest{Name: "", Email: "bad"}
	forward := Validate(r, NameRequired, EmailFormat)
	reverse := Validate(r, EmailFormat, NameRequired)

	for _, sentinel := range []error{ErrRequired, ErrFormat} {
		if errors.Is(forward, sentinel) != errors.Is(reverse, sentinel) {
			t.Errorf("reachability of %v differs by rule order", sentinel)
		}
	}
}

func TestNoRulesIsNil(t *testing.T) {
	t.Parallel()

	if err := Validate(CreateUserRequest{}); err != nil {
		t.Fatalf("Validate with no rules = %v, want nil", err)
	}
}

func ExampleValidate() {
	r := CreateUserRequest{Name: "", Email: "bad"}
	err := Validate(r, NameRequired, EmailFormat)
	fmt.Println(err.Error())
	// Output:
	// name: required
	// email: invalid format
}
```

## Review

`Validate` is correct when it runs every rule, returns nil exactly when all pass,
and keeps each failing sentinel reachable through `errors.Is` no matter how many
failed or in what order. The two `errors.Join` properties — drop nils, nil-if-all-
nil — are what let the loop stay branchless and what make the empty-rules case
return nil for free. The mistake this exercise inoculates against is the early
`return err` on first failure, which throws away every later violation and degrades
the client's experience. Wrapping sentinels with `%w` is non-negotiable: it is what
makes the joined tree introspectable. Run `go test -race`.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join)
- [`errors.Is`](https://pkg.go.dev/errors#Is)
- [Go Blog: working with errors (wrapping with `%w`)](https://go.dev/blog/go1.13-errors)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-http-middleware-chain.md](07-http-middleware-chain.md) | Next: [09-hot-path-slice-vs-variadic.md](09-hot-path-slice-vs-variadic.md)
