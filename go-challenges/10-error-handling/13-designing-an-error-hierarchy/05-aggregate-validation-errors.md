# Exercise 5: Aggregating Field Violations with errors.Join

A validation endpoint that reports the first bad field, makes the user fix it, then
reports the second, is a bad endpoint. This exercise builds a `ValidateUser` that
collects *every* violation into one `errors.Join`, where each branch is a
`*FieldError` wrapping `ErrUserInvalid` — so a caller can confirm the category with
`errors.Is` and enumerate the individual field failures in one pass.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
aggregate-validation/              module example.com/aggregate-validation
  go.mod
  validate.go                      ErrUserInvalid; FieldError{Field,Msg}; ValidateUser; Fields()
  cmd/demo/main.go                 validate a bad user, print category + every field
  validate_test.go                 all violations collected; Is category; all-valid returns nil
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `ErrUserInvalid`, a `FieldError` whose `Unwrap` returns `ErrUserInvalid`, a `ValidateUser` returning `errors.Join` of one `*FieldError` per violation, and a `Fields(err)` helper that walks the join tree.
- Test: an input with multiple bad fields yields `errors.Is` `ErrUserInvalid` and exactly the expected field names; a valid input returns nil (guarding the all-nil `errors.Join` case).
- Verify: `go test -count=1 -race ./...`

### Why errors.Join is the right tool here

The other exercises return a single error per call. Validation is different: one
call can fail for several independent reasons at once, and the caller wants all of
them. `errors.Join(errs...)` builds an error whose `Unwrap() []error` returns every
branch, and `errors.Is`/`errors.As` traverse *all* of them. That is exactly the
shape validation needs: `errors.Is(joined, ErrUserInvalid)` is true (every branch
wraps it, so at least one matches), and a caller can walk the branches to render a
per-field error list.

Each branch is a `*FieldError` carrying the field name and a message. It joins the
category by implementing `Unwrap() error { return ErrUserInvalid }` — so
`errors.Is(fe, ErrUserInvalid)` is true — and it stays extractable so a caller can
read `Field`. Note the deliberate asymmetry: `FieldError.Unwrap` returns a single
error (its category), while the `errors.Join` result returns a slice; the `Fields`
helper handles both by type-switching on the two `Unwrap` shapes.

The subtle correctness point is the empty case. `errors.Join` returns `nil` when
every argument is nil or the slice is empty — so a valid user, which appends no
`FieldError`, produces `ValidateUser(...) == nil` for free. This is the guard the
brief warns about: if you built the aggregate error unconditionally (say, a struct
with a `[]error` field) you would have to remember to return nil when the slice is
empty. `errors.Join` does it for you, but only if you pass it the collected slice
rather than wrapping it yourself. The `Fields` helper mirrors the traversal
`errors.Is`/`As` do internally: it recurses through both `Unwrap() []error` and
`Unwrap() error`, collecting each `*FieldError`'s name in order.

Create `validate.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUserInvalid is the category every field violation belongs to. A caller can
// ask "was this a validation failure?" with a single errors.Is against it.
var ErrUserInvalid = errors.New("user invalid")

// FieldError is one field's violation. It wraps ErrUserInvalid via Unwrap, so
// errors.Is(fe, ErrUserInvalid) is true and errors.As pulls out the field name.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Msg)
}

func (e *FieldError) Unwrap() error { return ErrUserInvalid }

// User is the input under validation.
type User struct {
	ID    string
	Email string
	Age   int
}

// ValidateUser checks every field and returns errors.Join of one *FieldError per
// violation. It returns nil when the input is valid: errors.Join of an empty (or
// all-nil) slice is nil, which is exactly the "valid" signal.
func ValidateUser(u User) error {
	var errs []error
	if u.ID == "" {
		errs = append(errs, &FieldError{Field: "id", Msg: "must not be empty"})
	}
	if !strings.Contains(u.Email, "@") {
		errs = append(errs, &FieldError{Field: "email", Msg: "must contain @"})
	}
	if u.Age < 0 || u.Age > 150 {
		errs = append(errs, &FieldError{Field: "age", Msg: "must be between 0 and 150"})
	}
	return errors.Join(errs...)
}

// Fields extracts the field names of every violation in err, in order. It works
// whether err is a single *FieldError or an errors.Join of several.
func Fields(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if fe, ok := e.(*FieldError); ok {
			out = append(out, fe.Field)
			return
		}
		switch x := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range x.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			walk(x.Unwrap())
		}
	}
	walk(err)
	return out
}
```

### The runnable demo

The demo validates a user with three bad fields and prints the category check, the
collected field names, and the joined multi-line message — then validates a good
user to show the nil result.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/aggregate-validation"
)

func main() {
	bad := validate.User{ID: "", Email: "not-an-email", Age: 200}
	err := validate.ValidateUser(bad)

	fmt.Printf("valid? %v\n", err == nil)
	fmt.Printf("Is ErrUserInvalid: %v\n", errors.Is(err, validate.ErrUserInvalid))
	fmt.Printf("fields: %v\n", validate.Fields(err))
	fmt.Printf("message:\n%s\n", err)

	good := validate.User{ID: "u1", Email: "a@example.com", Age: 30}
	fmt.Printf("good valid? %v\n", validate.ValidateUser(good) == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid? false
Is ErrUserInvalid: true
fields: [id email age]
message:
id: must not be empty
email: must contain @
age: must be between 0 and 150
good valid? true
```

### Tests

The tests assert that a multi-violation input collects exactly the expected fields
in order, that the aggregate still matches the category via `errors.Is`, that
`errors.As` finds a `*FieldError`, and — the guard the brief calls out — that a
fully valid input returns nil rather than a non-nil empty aggregate.

Create `validate_test.go`:

```go
package validate

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestCollectsEveryViolation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		user   User
		fields []string
	}{
		{"all bad", User{ID: "", Email: "x", Age: 200}, []string{"id", "email", "age"}},
		{"email only", User{ID: "u1", Email: "x", Age: 30}, []string{"email"}},
		{"age only", User{ID: "u1", Email: "a@x.com", Age: -1}, []string{"age"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateUser(tc.user)
			if !errors.Is(err, ErrUserInvalid) {
				t.Fatalf("err = %v; want errors.Is ErrUserInvalid", err)
			}
			if got := Fields(err); !reflect.DeepEqual(got, tc.fields) {
				t.Fatalf("fields = %v; want %v", got, tc.fields)
			}
		})
	}
}

func TestValidUserReturnsNil(t *testing.T) {
	t.Parallel()
	err := ValidateUser(User{ID: "u1", Email: "a@example.com", Age: 30})
	if err != nil {
		t.Fatalf("valid user produced %v; want nil", err)
	}
}

func TestExtractsFieldError(t *testing.T) {
	t.Parallel()
	err := ValidateUser(User{ID: "", Email: "a@x.com", Age: 30})
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatal("errors.As did not find a *FieldError")
	}
	if fe.Field != "id" {
		t.Fatalf("first field = %q; want id", fe.Field)
	}
}

func Example() {
	err := ValidateUser(User{ID: "", Email: "x", Age: 30})
	fmt.Println(errors.Is(err, ErrUserInvalid), Fields(err))
	// Output: true [id email]
}
```

## Review

The aggregate is correct when three properties hold at once: the joined error
`errors.Is` the category (because every branch wraps `ErrUserInvalid`), a walk of
the branches yields every field name in order, and a valid input returns nil. That
last one is where aggregation quietly breaks — `errors.Join` of an empty slice
returns nil, so passing the collected slice straight to `errors.Join` gives you the
"valid" signal for free, whereas hand-rolling a `[]error` struct forces you to
remember the empty-is-nil case yourself. Reach for `errors.Join` specifically when
a single operation can fail several independent ways and the caller wants all of
them; for a single failure, a plain sentinel or typed error is less machinery.

## Resources

- [`errors.Join`](https://pkg.go.dev/errors#Join) — the `Unwrap() []error` aggregate and its nil behavior.
- [`errors.As`](https://pkg.go.dev/errors#As) — depth-first traversal across joined branches.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — `errors.Join` and multiple-`%w` wrapping.

---

Back to [04-http-problem-details-mapping.md](04-http-problem-details-mapping.md) | Next: [06-transient-vs-permanent-retry.md](06-transient-vs-permanent-retry.md)
