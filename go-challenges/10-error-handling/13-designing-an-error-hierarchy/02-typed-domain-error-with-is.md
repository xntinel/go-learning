# Exercise 2: A Typed Domain Error That Matches Its Base via Is

Sentinels are perfect until a category needs to carry data — *which* user, *which*
field. This exercise builds a typed `*UserError` that carries structured context
yet still satisfies `errors.Is(err, ErrUser)`, by giving it a custom `Is` method.
The result is the best of both: callers that only need the category stay decoupled,
callers that need the id pay the coupling deliberately with `errors.As`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
typed-domain-error/                module example.com/typed-domain-error
  go.mod
  usererr.go                       ErrUser base; type UserError{Op,UserID,Field,Kind}; Is/Unwrap; NotFound/AlreadyExists/Invalid
  cmd/demo/main.go                 wrap a NotFound, then Is the base and As the type back out
  usererr_test.go                  Is matches base through wrapping; As reads UserID; Is rejects unrelated
```

- Files: `usererr.go`, `cmd/demo/main.go`, `usererr_test.go`.
- Implement: a base sentinel `ErrUser`, a `UserError` struct with `Error()`, `Unwrap() error`, and a shallow `Is(target error) bool`, plus constructors `NotFound(id)`, `AlreadyExists(id)`, `Invalid(id, field)`.
- Test: `errors.Is(NotFound("u1"), ErrUser)` is true via the custom `Is`; `errors.As` extracts `*UserError` and reads `UserID`; both survive a `fmt.Errorf("handler: %w", e)` wrap; `Is` returns false for an unrelated sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/typed-domain-error/cmd/demo
cd ~/go-exercises/typed-domain-error
go mod init example.com/typed-domain-error
```

### How a typed error opts into a category

The mechanism is the `Is` method. `errors.Is` walks the chain and, at every node,
compares to the target with `==` *and* calls the node's `Is(target error) bool` if
it has one. So `UserError` does not need to wrap `ErrUser` with `%w` to match it —
it defines `func (e *UserError) Is(target error) bool { return target == ErrUser }`
and any `*UserError`, wrapped or not, answers true to `errors.Is(err, ErrUser)`.
This keeps the base sentinel as the stable category surface while the concrete type
carries the payload.

Two rules make this safe. First, the `Is` method must be *shallow*: it decides only
whether *this* node matches `target`. It must never call `Unwrap` or recurse,
because `errors.Is` is already doing the walk; a recursive `Is` double-traverses and
can loop. Second, `Unwrap` and `Is` are independent tools. `Unwrap() error` returns
the wrapped *cause* (a lower-level error this one annotates) so `errors.Is`/`As`
continue past this node; `Is` decides category membership for this node. Here
`Unwrap` returns `cause`, which is `nil` for the plain constructors — a `nil`
`Unwrap` simply stops the walk, which is correct.

Extraction is the other half. A caller that needs the id — to log it, to build a
`Location` header, to retry a specific record — uses `errors.As(err, &ue)` (or
`errors.AsType[*UserError](err)`) to pull the concrete value out of an arbitrary
chain and read `ue.UserID`. That caller accepts coupling to `*UserError` because it
genuinely needs the data; the category caller does not, and `Is` keeps it free.

Create `usererr.go`:

```go
package userdom

import (
	"errors"
	"fmt"
)

// ErrUser is the category base. A *UserError matches it via its Is method,
// so callers can branch on the category without naming the concrete type.
var ErrUser = errors.New("user error")

// Kind names the specific failure. It drives Is-less callers that switch.
type Kind int

const (
	KindNotFound Kind = iota
	KindExists
	KindInvalid
)

func (k Kind) String() string {
	switch k {
	case KindNotFound:
		return "not found"
	case KindExists:
		return "already exists"
	case KindInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// UserError is a typed domain error carrying the operation, the user id, and
// (for validation) the offending field. It matches ErrUser via Is and can be
// extracted with errors.As so a caller can read UserID.
type UserError struct {
	Op     string
	UserID string
	Field  string
	Kind   Kind
	cause  error
}

func (e *UserError) Error() string {
	msg := fmt.Sprintf("%s: user %q %s", e.Op, e.UserID, e.Kind)
	if e.Field != "" {
		msg += ": field " + e.Field
	}
	if e.cause != nil {
		msg += ": " + e.cause.Error()
	}
	return msg
}

// Unwrap exposes the wrapped cause (may be nil) for errors.Is/As traversal.
func (e *UserError) Unwrap() error { return e.cause }

// Is is a shallow predicate: it only decides whether this node matches target.
// It never unwraps, because errors.Is already walks the chain.
func (e *UserError) Is(target error) bool { return target == ErrUser }

func NotFound(id string) *UserError {
	return &UserError{Op: "lookup", UserID: id, Kind: KindNotFound}
}

func AlreadyExists(id string) *UserError {
	return &UserError{Op: "create", UserID: id, Kind: KindExists}
}

func Invalid(id, field string) *UserError {
	return &UserError{Op: "validate", UserID: id, Field: field, Kind: KindInvalid}
}
```

### The runnable demo

The demo wraps a `NotFound` in a handler-level annotation with `%w`, then shows the
two questions a boundary asks: `errors.Is` for the category, `errors.As` for the
data — both answered correctly through the wrap.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/typed-domain-error"
)

func main() {
	err := fmt.Errorf("handler: %w", userdom.NotFound("u42"))

	fmt.Printf("error: %v\n", err)
	fmt.Printf("Is ErrUser: %v\n", errors.Is(err, userdom.ErrUser))

	var ue *userdom.UserError
	if errors.As(err, &ue) {
		fmt.Printf("extracted UserID=%q kind=%s\n", ue.UserID, ue.Kind)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
error: handler: lookup: user "u42" not found
Is ErrUser: true
extracted UserID="u42" kind=not found
```

### Tests

The tests pin the whole contract: the custom `Is` makes every constructor match the
base; `errors.As` reads `UserID` back out; both survive a `fmt.Errorf("%w")` wrap;
and `Is` returns false for an unrelated sentinel, proving the category is not a
catch-all.

Create `usererr_test.go`:

```go
package userdom

import (
	"errors"
	"fmt"
	"testing"
)

func TestConstructorsMatchBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  *UserError
		id   string
		kind Kind
	}{
		{"not found", NotFound("u1"), "u1", KindNotFound},
		{"already exists", AlreadyExists("u2"), "u2", KindExists},
		{"invalid", Invalid("u3", "email"), "u3", KindInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !errors.Is(tc.err, ErrUser) {
				t.Errorf("errors.Is(%v, ErrUser) = false; want true", tc.err)
			}
			if tc.err.UserID != tc.id {
				t.Errorf("UserID = %q; want %q", tc.err.UserID, tc.id)
			}
			if tc.err.Kind != tc.kind {
				t.Errorf("Kind = %v; want %v", tc.err.Kind, tc.kind)
			}
		})
	}
}

func TestIsAndAsSurviveWrapping(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("handler: %w", NotFound("u42"))

	if !errors.Is(wrapped, ErrUser) {
		t.Fatal("wrapped error no longer Is ErrUser")
	}

	var ue *UserError
	if !errors.As(wrapped, &ue) {
		t.Fatal("errors.As failed to extract *UserError from wrapped error")
	}
	if ue.UserID != "u42" {
		t.Fatalf("UserID = %q; want u42", ue.UserID)
	}
}

func TestIsRejectsUnrelated(t *testing.T) {
	t.Parallel()
	other := errors.New("db connection refused")
	if errors.Is(NotFound("u1"), other) {
		t.Fatal("NotFound matched an unrelated sentinel")
	}
}

func TestInvalidCarriesField(t *testing.T) {
	t.Parallel()
	var ue *UserError
	if !errors.As(Invalid("u1", "email"), &ue) {
		t.Fatal("As failed")
	}
	if ue.Field != "email" {
		t.Fatalf("Field = %q; want email", ue.Field)
	}
}

func Example() {
	err := AlreadyExists("u7")
	fmt.Println(errors.Is(err, ErrUser), err.UserID)
	// Output: true u7
}
```

## Review

The design is correct when one value answers both questions: `errors.Is(err,
ErrUser)` is true because the shallow `Is` method matches the base, and
`errors.As(err, &ue)` yields the concrete `*UserError` with its fields intact —
both still true after a `%w` wrap, because `errors.Is`/`As` walk the chain. The
trap to avoid is making `Is` do too much: if it calls `Unwrap` or compares
recursively, it fights the walk `errors.Is` is already performing. Keep `Is`
shallow (`return target == ErrUser`) and let `Unwrap` carry the cause. When you
find yourself adding a third or fourth field to `UserError` that only one caller
reads, that is the signal the type is doing a job a separate typed error should do —
but for a single category with an id and a field, this shape is exactly right.

## Resources

- [`errors.As`](https://pkg.go.dev/errors#As) — extraction by assignability, and the `As(any) bool` hook.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — the `==`/`Is` comparison at each node that makes a custom `Is` work.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — when to use a typed error versus a sentinel.

---

Back to [01-sentinel-error-hierarchy.md](01-sentinel-error-hierarchy.md) | Next: [03-repository-error-translation.md](03-repository-error-translation.md)
