# Exercise 4: Validation Pipeline: Aggregate Field Errors with errors.Join

A form validator should not stop at the first bad field — it should report *all*
of them so the client can fix them in one round trip. That is a job for
`errors.Join`, which combines independent sibling errors into one value whose tree
`errors.Is` can still search and whose children you can enumerate for a structured
422. This module builds that validator and shows why Join, not `%w`, is the right
tool when the failures are peers rather than a causal chain.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
validate/                   independent module: example.com/validate
  go.mod                    go 1.24
  validate.go               field sentinels; Validate returns errors.Join; FailedFields
  cmd/
    demo/
      main.go               runnable demo: valid and multi-invalid requests
  validate_test.go          table: nil on all-valid; each sentinel found; child count
```

Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: per-field checks that return a wrapped sentinel or nil, a `Validate` that returns `errors.Join(...)` of them, and a `FailedFields` helper that enumerates the joined children.
Test: all-valid returns nil; multiple invalid returns an error where `errors.Is` matches each field sentinel and `FailedFields` has the expected length; a mixed input yields only the failures (Join drops nils).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/06-error-wrapping-chains/04-validation-join-aggregate/cmd/demo
cd go-solutions/10-error-handling/06-error-wrapping-chains/04-validation-join-aggregate
go mod edit -go=1.24
```

### Join versus %w: peers versus a chain

Three bad fields in a form are *independent siblings*: an empty name did not cause
a bad email. That is precisely the shape `errors.Join` expresses — it builds one
error whose `Unwrap() []error` returns all the children, forming a tree rather than
a linked list. `%w` would be the wrong tool here: it expresses a single causal
chain ("this failed because of that"), and stringing unrelated field errors into a
chain would falsely imply causality and, worse, hide all but the outermost when a
naive caller only unwrapped once.

Two properties of `errors.Join` make it ergonomic for validation. First, it
**drops nils**: `errors.Join(nil, err, nil)` returns just `err`, so each field check
can return `nil` on success and you Join them unconditionally — no manual
"append-if-non-nil" bookkeeping. Second, if *every* argument is nil it returns
`nil`, so the zero-failure path is clean: a valid request produces a `nil` error,
exactly what callers expect.

Each field check wraps its sentinel with `%w` so `errors.Is` finds it — the field
error is itself a tiny one-link chain ("name: <sentinel>") for a readable message,
and Join assembles those into the tree. To enumerate the failures for a 422 body,
`FailedFields` type-asserts the joined value to `interface{ Unwrap() []error }` and
returns the slice — the correct way to inspect a joined error. Note you must **not**
use `errors.Unwrap` here: that function looks for `Unwrap() error` (singular) and
returns `nil` for a joined error.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
)

// Field sentinels. Each names one validation failure the client can act on.
var (
	ErrEmptyName = errors.New("name must not be empty")
	ErrBadEmail  = errors.New("email must contain @")
	ErrBadAge    = errors.New("age must be between 0 and 130")
)

// SignupRequest is the payload to validate.
type SignupRequest struct {
	Name  string
	Email string
	Age   int
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("name: %w", ErrEmptyName)
	}
	return nil
}

func validateEmail(email string) error {
	if !strings.Contains(email, "@") {
		return fmt.Errorf("email %q: %w", email, ErrBadEmail)
	}
	return nil
}

func validateAge(age int) error {
	if age < 0 || age > 130 {
		return fmt.Errorf("age %d: %w", age, ErrBadAge)
	}
	return nil
}

// Validate checks every field and joins the failures. It returns nil when all
// pass (errors.Join drops nils and returns nil if every argument is nil).
func Validate(r SignupRequest) error {
	return errors.Join(
		validateName(r.Name),
		validateEmail(r.Email),
		validateAge(r.Age),
	)
}

// FailedFields enumerates the individual field errors inside a joined validation
// error, for building a structured response. It uses the Unwrap() []error tree
// interface, NOT errors.Unwrap (which returns nil for joined errors).
func FailedFields(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	return []error{err}
}
```

### The runnable demo

The demo validates one clean request and one with two bad fields, printing the
failure count and whether each specific sentinel was found.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/validate"
)

func main() {
	good := validate.SignupRequest{Name: "Alice", Email: "alice@example.com", Age: 30}
	fmt.Printf("valid request error: %v\n", validate.Validate(good))

	bad := validate.SignupRequest{Name: "", Email: "not-an-email", Age: 30}
	err := validate.Validate(bad)
	fmt.Printf("failures: %d\n", len(validate.FailedFields(err)))
	fmt.Printf("is ErrEmptyName: %v\n", errors.Is(err, validate.ErrEmptyName))
	fmt.Printf("is ErrBadEmail:  %v\n", errors.Is(err, validate.ErrBadEmail))
	fmt.Printf("is ErrBadAge:    %v\n", errors.Is(err, validate.ErrBadAge))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid request error: <nil>
failures: 2
is ErrEmptyName: true
is ErrBadEmail:  true
is ErrBadAge:    false
```

### Tests

The table covers the three shapes: all-valid (nil), fully-invalid (all three
sentinels found, three children), and mixed (only the failing fields appear,
proving Join dropped the passing ones). Each row asserts the set of sentinels
`errors.Is` finds and the exact `FailedFields` length, so a regression that
accidentally chains with `%w` (collapsing the tree) or fails to drop nils is
caught.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		req          SignupRequest
		wantNil      bool
		wantFailures int
		wantIs       []error
		wantIsNot    []error
	}{
		{
			name:    "all valid",
			req:     SignupRequest{Name: "Alice", Email: "a@b.com", Age: 30},
			wantNil: true,
		},
		{
			name:         "all invalid",
			req:          SignupRequest{Name: " ", Email: "nope", Age: 999},
			wantFailures: 3,
			wantIs:       []error{ErrEmptyName, ErrBadEmail, ErrBadAge},
		},
		{
			name:         "mixed drops passing fields",
			req:          SignupRequest{Name: "", Email: "ok@x.com", Age: 200},
			wantFailures: 2,
			wantIs:       []error{ErrEmptyName, ErrBadAge},
			wantIsNot:    []error{ErrBadEmail},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tt.req)

			if tt.wantNil {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate = nil, want error")
			}
			if got := len(FailedFields(err)); got != tt.wantFailures {
				t.Errorf("FailedFields len = %d, want %d", got, tt.wantFailures)
			}
			for _, s := range tt.wantIs {
				if !errors.Is(err, s) {
					t.Errorf("errors.Is(err, %v) = false, want true", s)
				}
			}
			for _, s := range tt.wantIsNot {
				if errors.Is(err, s) {
					t.Errorf("errors.Is(err, %v) = true, want false", s)
				}
			}
		})
	}
}

func ExampleValidate() {
	err := Validate(SignupRequest{Name: "", Email: "bad", Age: 30})
	fmt.Println(len(FailedFields(err)), errors.Is(err, ErrEmptyName))
	// Output: 2 true
}
```

## Review

The validator is correct when a clean request yields a `nil` error and a dirty one
yields a joined error whose children are exactly the failed fields — no more, no
fewer. The `mixed` case is the discriminating one: it proves `errors.Join` dropped
the passing field's `nil`, so `FailedFields` returns two, not three. The reason to
reach for Join rather than `%w` is semantic, not cosmetic: field failures are peers,
and modeling them as a chain would both lie about causality and, for a caller that
unwraps once, hide all but one. Enumerate with the `Unwrap() []error` assertion, and
never with `errors.Unwrap`, which returns `nil` for a joined error and would make
`FailedFields` silently report a single failure.

## Resources

- [errors.Join](https://pkg.go.dev/errors#Join) — combining sibling errors; nil-dropping behavior.
- [errors package](https://pkg.go.dev/errors) — `Is` traversal over joined trees.
- [Go 1.20 release notes: errors.Join](https://go.dev/doc/go1.20#errors) — the introduction of `Unwrap() []error`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-multi-cause-wrapping.md](05-multi-cause-wrapping.md)
