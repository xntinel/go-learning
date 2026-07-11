# Exercise 5: Accumulate all request-validation failures with errors.Join

An API that validates a request one field at a time and returns on the first
problem forces the client into a frustrating round-trip loop: fix the name,
resubmit, learn the email is bad, resubmit, learn the age is out of range. This
exercise builds a `ValidateCreateUser` that checks every field independently and
returns `errors.Join` of one wrapped sentinel per invalid field, so a single
response carries every problem — and every sentinel stays reachable by
`errors.Is`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
validate/                      independent module: example.com/validate
  go.mod                       go 1.24
  validate.go                  ErrRequired, ErrInvalidEmail, ErrOutOfRange; ValidateCreateUser -> errors.Join
  validate_test.go             all-invalid finds all three; valid -> nil; one-field; Unwrap asymmetry
  cmd/
    demo/
      main.go                  validates a fully-invalid and a valid request
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `ValidateCreateUser(req)` checking name, email, and age independently, appending one `%w`-wrapped sentinel per invalid field and returning `errors.Join` of them.
- Test: an all-invalid request has all three sentinels reachable via `errors.Is`; the joined message lists all three field messages separated by newlines; a valid request returns `nil`; a one-field-invalid request matches only that sentinel; `errors.Unwrap(joined)` is `nil`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/validate/cmd/demo
cd ~/go-exercises/validate
go mod init example.com/validate
```

### Accumulate, do not short-circuit

The validator never returns early. It appends to a `[]error` — one entry per
failing field, each wrapping the field's sentinel with `%w` and a short label — and
finishes by returning `errors.Join(errs...)`. `errors.Join` does the rest of the
work: it discards `nil` operands, returns `nil` when the slice is empty or all
`nil` (so a valid request naturally yields no error), and otherwise returns a
value whose `Error()` is the members' messages joined by newlines and whose
`Unwrap() []error` exposes every member to the `errors.Is` tree walk.

That last property is what keeps the accumulated error useful to a caller: a
middleware can `errors.Is(err, ErrInvalidEmail)` to decide whether to surface a
"check your email" hint, independently of the other failures. And because
`errors.Join` produces an `Unwrap() []error` and not an `Unwrap() error`,
`errors.Unwrap(joined)` returns `nil` — the same asymmetry as multiple `%w`. You
inspect it with `errors.Is`, never with a manual unwrap loop.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
)

// Per-field sentinels. A caller branches on whichever fields it cares about.
var (
	ErrRequired     = errors.New("required")
	ErrInvalidEmail = errors.New("invalid email")
	ErrOutOfRange   = errors.New("out of range")
)

type CreateUser struct {
	Name  string
	Email string
	Age   int
}

// ValidateCreateUser checks every field independently and joins the failures, so
// the caller learns every problem at once rather than one per round-trip.
func ValidateCreateUser(req CreateUser) error {
	var errs []error
	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fmt.Errorf("name: %w", ErrRequired))
	}
	if !strings.Contains(req.Email, "@") {
		errs = append(errs, fmt.Errorf("email %q: %w", req.Email, ErrInvalidEmail))
	}
	if req.Age < 0 || req.Age > 130 {
		errs = append(errs, fmt.Errorf("age %d: %w", req.Age, ErrOutOfRange))
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
	bad := validate.ValidateCreateUser(validate.CreateUser{Name: "", Email: "nope", Age: 200})
	fmt.Println("all-invalid error:")
	fmt.Println(bad)
	fmt.Printf("is ErrRequired=%v is ErrInvalidEmail=%v is ErrOutOfRange=%v\n",
		errors.Is(bad, validate.ErrRequired),
		errors.Is(bad, validate.ErrInvalidEmail),
		errors.Is(bad, validate.ErrOutOfRange))

	ok := validate.ValidateCreateUser(validate.CreateUser{Name: "alice", Email: "a@b.co", Age: 30})
	fmt.Printf("valid error is nil: %v\n", ok == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all-invalid error:
name: required
email "nope": invalid email
age 200: out of range
is ErrRequired=true is ErrInvalidEmail=true is ErrOutOfRange=true
valid error is nil: true
```

### Tests

The all-invalid test proves every sentinel is reachable through the joined value's
tree walk, and counts the newlines to confirm all three messages are present. The
valid test proves `errors.Join` collapses to `nil`. The one-field test proves the
other sentinels are absent. The asymmetry test pins `errors.Unwrap(joined) == nil`.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateAllInvalid(t *testing.T) {
	t.Parallel()

	err := ValidateCreateUser(CreateUser{Name: "", Email: "nope", Age: 200})

	if !errors.Is(err, ErrRequired) {
		t.Errorf("want errors.Is ErrRequired in %v", err)
	}
	if !errors.Is(err, ErrInvalidEmail) {
		t.Errorf("want errors.Is ErrInvalidEmail in %v", err)
	}
	if !errors.Is(err, ErrOutOfRange) {
		t.Errorf("want errors.Is ErrOutOfRange in %v", err)
	}
	if got := strings.Count(err.Error(), "\n"); got != 2 {
		t.Fatalf("newline count = %d, want 2 (three messages joined)", got)
	}
}

func TestValidateValidIsNil(t *testing.T) {
	t.Parallel()

	err := ValidateCreateUser(CreateUser{Name: "alice", Email: "a@b.co", Age: 30})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestValidateOneFieldInvalid(t *testing.T) {
	t.Parallel()

	err := ValidateCreateUser(CreateUser{Name: "alice", Email: "bad", Age: 30})

	if !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("want errors.Is ErrInvalidEmail in %v", err)
	}
	if errors.Is(err, ErrRequired) {
		t.Error("did not expect ErrRequired for a valid name")
	}
	if errors.Is(err, ErrOutOfRange) {
		t.Error("did not expect ErrOutOfRange for a valid age")
	}
}

func TestValidateJoinUnwrapAsymmetry(t *testing.T) {
	t.Parallel()

	err := ValidateCreateUser(CreateUser{Name: "", Email: "bad", Age: 200})

	// errors.Join yields Unwrap() []error, so errors.Unwrap returns nil.
	if errors.Unwrap(err) != nil {
		t.Fatal("errors.Unwrap should return nil for an errors.Join result")
	}
}
```

## Review

The validator is correct when a valid request yields `nil` and an invalid one
yields a value in which every failing field's sentinel is reachable by
`errors.Is`. The design payoff over a first-invalid-wins validator is entirely on
the client's side: one response, every problem. The subtle correctness point is
relying on `errors.Join`'s own contract rather than special-casing the empty
slice — `errors.Join()` and `errors.Join(nil, nil)` both return `nil`, so the
happy path needs no `if len(errs) == 0` guard. As with multiple `%w`, resist the
urge to inspect the result with a manual `errors.Unwrap` loop; the asymmetry test
is there to keep that mistake out of the codebase.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — nil-discarding, newline formatting, `Unwrap() []error`.
- [errors package](https://pkg.go.dev/errors) — `Is` tree walk over joined errors.
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — wrapping each field sentinel with `%w`.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — `errors.Join`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-fanout-multi-w-partial-failure.md](04-fanout-multi-w-partial-failure.md) | Next: [06-handler-defer-wrap-named-return.md](06-handler-defer-wrap-named-return.md)
