# Exercise 7: Aggregate Field Failures With errors.Join

A validation endpoint that returns the *first* bad field forces the client
through a frustrating fix-one-resubmit-repeat loop. This exercise builds a
validator that accumulates one wrapped sentinel per failing field and combines
them with `errors.Join`, so the API reports every violation at once while callers
can still match any single cause with `errors.Is`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
validate/                     independent module: example.com/validate
  go.mod                      go 1.26
  validate.go                 ErrRequired/ErrTooLong/ErrBadFormat; Validate returns errors.Join
  cmd/
    demo/
      main.go                 validates a bad and a good payload
  validate_test.go            multi-failure match, valid->nil, Unwrap() []error traversal
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `Validate(Signup)` that appends `fmt.Errorf("field: %w", ErrX)` per failing rule and returns `errors.Join(errs...)`.
- Test: a payload failing three rules where `errors.Is` finds each sentinel; a valid payload returning `nil`; a check that the joined value exposes `Unwrap() []error` and the line count matches the failure count.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/05-sentinel-errors/07-join-validation-pipeline/cmd/demo
cd go-solutions/10-error-handling/05-sentinel-errors/07-join-validation-pipeline
```

### Why errors.Join fits accumulation exactly

Field validation is naturally a *set* of independent failures, and `errors.Join`
is built for exactly that shape. Two of its behaviors make the code trivial.
First, `errors.Join` discards `nil` inputs, and returns `nil` when every input is
`nil` (or when there are none). So the validator can build a slice, append one
wrapped sentinel per broken rule, and unconditionally `return errors.Join(errs...)`
— a clean payload yields a genuine `nil`, and no special-case "if no errors
return nil" branch is needed. Second, the value it returns implements
`Unwrap() []error` (the slice form, distinct from the single-error `Unwrap`), and
`errors.Is`/`errors.As` traverse that whole tree. That is what lets a caller
still ask `errors.Is(joined, ErrRequired)` and get a true answer even though the
joined value wraps several errors — the framework depth-first-walks every child.

Each field error is wrapped with `%w` so the field name rides along in the
message (`email: bad format`) while the sentinel stays matchable. The joined
error's `Error()` is the newline-separated concatenation of the children, so the
number of lines equals the number of violations — a property the test asserts
directly.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrRequired  = errors.New("required")
	ErrTooLong   = errors.New("too long")
	ErrBadFormat = errors.New("bad format")
)

// Signup is a request payload to validate.
type Signup struct {
	Email    string
	Username string
	Bio      string
}

// Validate accumulates one wrapped sentinel per failing field and returns them
// combined with errors.Join. A fully valid payload returns nil, because
// errors.Join of all-nil inputs is nil. Callers match any single cause with
// errors.Is over the joined value.
func Validate(s Signup) error {
	var errs []error

	switch {
	case s.Email == "":
		errs = append(errs, fmt.Errorf("email: %w", ErrRequired))
	case !strings.Contains(s.Email, "@"):
		errs = append(errs, fmt.Errorf("email: %w", ErrBadFormat))
	}

	switch {
	case s.Username == "":
		errs = append(errs, fmt.Errorf("username: %w", ErrRequired))
	case len(s.Username) > 20:
		errs = append(errs, fmt.Errorf("username: %w", ErrTooLong))
	}

	if len(s.Bio) > 160 {
		errs = append(errs, fmt.Errorf("bio: %w", ErrTooLong))
	}

	return errors.Join(errs...)
}
```

### The runnable demo

The demo validates one payload that breaks three rules (printing every violation
at once and matching two sentinels) and one clean payload (returning `nil`).

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"strings"

	"example.com/validate"
)

func main() {
	bad := validate.Signup{Email: "nope", Username: "", Bio: strings.Repeat("x", 200)}
	if err := validate.Validate(bad); err != nil {
		fmt.Println("invalid signup:")
		fmt.Println(err)
		fmt.Println("required?", errors.Is(err, validate.ErrRequired))
		fmt.Println("bad format?", errors.Is(err, validate.ErrBadFormat))
	}

	good := validate.Signup{Email: "alice@example.com", Username: "alice", Bio: "hi"}
	fmt.Println("valid signup err:", validate.Validate(good))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
invalid signup:
email: bad format
username: required
bio: too long
required? true
bad format? true
valid signup err: <nil>
```

### Tests

`TestAccumulatesAllFailures` triggers three rules and asserts each sentinel is
found via `errors.Is` and the joined `Error()` has one line per failure.
`TestValidPayloadReturnsNil` pins the all-nil-is-nil property.
`TestUnwrapMultiTraversal` asserts the joined value exposes `Unwrap() []error`.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestAccumulatesAllFailures(t *testing.T) {
	t.Parallel()

	err := Validate(Signup{Email: "no-at", Username: "", Bio: strings.Repeat("x", 200)})
	if err == nil {
		t.Fatal("expected validation errors, got nil")
	}
	if !errors.Is(err, ErrBadFormat) {
		t.Errorf("want ErrBadFormat in %v", err)
	}
	if !errors.Is(err, ErrRequired) {
		t.Errorf("want ErrRequired in %v", err)
	}
	if !errors.Is(err, ErrTooLong) {
		t.Errorf("want ErrTooLong in %v", err)
	}
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), err.Error())
	}
}

func TestValidPayloadReturnsNil(t *testing.T) {
	t.Parallel()

	if err := Validate(Signup{Email: "a@b.com", Username: "alice", Bio: "hi"}); err != nil {
		t.Fatalf("valid payload: %v", err)
	}
}

func TestUnwrapMultiTraversal(t *testing.T) {
	t.Parallel()

	err := Validate(Signup{Email: "", Username: "alice"}) // only email required
	if !errors.Is(err, ErrRequired) {
		t.Fatal("want ErrRequired")
	}
	if errors.Is(err, ErrTooLong) {
		t.Fatal("no field is too long")
	}

	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatal("errors.Join value should implement Unwrap() []error")
	}
	if n := len(joined.Unwrap()); n != 1 {
		t.Fatalf("Unwrap() returned %d errors, want 1", n)
	}
}

func ExampleValidate() {
	err := Validate(Signup{Email: "", Username: "bob"})
	fmt.Println(err)
	fmt.Println(errors.Is(err, ErrRequired))
	// Output:
	// email: required
	// true
}
```

## Review

The validator is correct when a clean payload yields a real `nil` (not an empty
non-nil error) and a dirty one yields a value that both reports every violation
in its message and answers `errors.Is` for each cause — which works only because
`errors.Join` produces an `Unwrap() []error` tree that `errors.Is` traverses. The
trap is hand-rolling an aggregate with a single `Unwrap() error`: it hides all
but one cause from `errors.Is`. Let `errors.Join` build the tree, and return it
unconditionally so the nil-collapsing behavior gives you the empty case for free.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — combining errors, nil-dropping, and `Unwrap() []error`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — tree traversal over joined errors.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — where `errors.Join` and multi-unwrap were introduced.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-context-cancellation-sentinels.md](06-context-cancellation-sentinels.md) | Next: [08-fs-config-loader.md](08-fs-config-loader.md)
