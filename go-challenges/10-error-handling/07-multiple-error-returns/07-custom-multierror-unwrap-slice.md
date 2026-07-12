# Exercise 7: Build A Custom Aggregate Error Implementing Unwrap() []error

`errors.Join` is enough most of the time, but sometimes you need the aggregate to
carry metadata a bare join cannot — a count, a category, a stable error code, or an
inspectable slice. This exercise builds a hand-rolled `MultiError` and proves the
one method that makes `errors.Is`/`errors.As` traverse it: `Unwrap() []error`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
multierr/                  independent module: example.com/multierr
  go.mod                   go 1.26
  multierr.go              MultiError{Category, Errors}; Add; Err; Unwrap() []error
  cmd/
    demo/
      main.go              builds a MultiError and inspects it with errors.Is/As
  multierr_test.go         Is/As traversal; zero-member returns nil; slice access
```

- Files: `multierr.go`, `cmd/demo/main.go`, `multierr_test.go`.
- Implement: a `MultiError` with a `Category` and `Errors []error`, an `Error()` method, an `Unwrap() []error` method, an `Add` accumulator, and an `Err()` that returns nil when empty.
- Test: populate with wrapped sentinels — `errors.Is` finds each and `errors.As` extracts a typed member through the custom `Unwrap() []error`; a zero-member `MultiError` returns nil from `Err()`; direct `Errors`-slice access yields the members.
- Verify: `go test -count=1 -race ./...`

### Why the `Unwrap() []error` method is the whole trick

Since Go 1.20, `errors.Is` and `errors.As` walk a tree of errors, following any
error that exposes `Unwrap() error` or `Unwrap() []error`. `errors.Join`'s value
implements the slice form, which is why `Is`/`As` see its members. That mechanism
is not special to the stdlib type: *any* type with an `Unwrap() []error` method
gets the same traversal. So a custom aggregate becomes fully inspectable by
implementing that one method — no more, no less.

The reason to build your own instead of using `errors.Join` is metadata. A joined
error is opaque: it is exactly its members and nothing else. A `MultiError` can
carry a `Category` ("validation", "shutdown") the caller branches on, a count it
exposes without walking the tree, a stable code for an API response, or simply a
public `Errors` slice a caller can range over directly. You keep the `Is`/`As`
tree-walk *and* attach structure the join cannot hold.

Two correctness details this exercise pins. First, the zero-member case: an
accumulator that has collected nothing must report *untyped* nil, not a non-nil
`*MultiError` wrapping an empty slice — otherwise callers see a truthy error when
nothing failed (the typed-nil trap from Exercise 3). We handle that with an `Err()`
method that returns `nil` when `len(Errors) == 0`. Second, the receiver: `Error()`
and `Unwrap()` are on the pointer receiver, so the concrete member type is
`*MultiError` and `Err()` must return `error` (returning `*MultiError` directly
would reintroduce the typed-nil bug).

Create `multierr.go`:

```go
package multierr

import "strings"

// MultiError is a custom aggregate error that carries a Category alongside its
// members. Implementing Unwrap() []error makes errors.Is/errors.As traverse it,
// exactly as they traverse errors.Join.
type MultiError struct {
	Category string
	Errors   []error
}

// Add appends a non-nil error to the aggregate. Nil is ignored, mirroring
// errors.Join's nil-skipping so callers can Add unconditionally.
func (m *MultiError) Add(err error) {
	if err != nil {
		m.Errors = append(m.Errors, err)
	}
}

// Error renders the category and each member, one per line.
func (m *MultiError) Error() string {
	parts := make([]string, len(m.Errors))
	for i, e := range m.Errors {
		parts[i] = e.Error()
	}
	return m.Category + ": " + strings.Join(parts, "\n")
}

// Unwrap exposes the members so errors.Is and errors.As can walk them. This one
// method is what makes the custom type inspectable.
func (m *MultiError) Unwrap() []error {
	return m.Errors
}

// Err returns the aggregate as an error, or untyped nil when there are no members.
// Returning nil (not a *MultiError with an empty slice) avoids the typed-nil trap.
func (m *MultiError) Err() error {
	if len(m.Errors) == 0 {
		return nil
	}
	return m
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/multierr"
)

type ValidationError struct{ Field string }

func (e *ValidationError) Error() string { return "invalid " + e.Field }

var errQuota = errors.New("quota exceeded")

func main() {
	m := &multierr.MultiError{Category: "request"}
	m.Add(fmt.Errorf("check quota: %w", errQuota))
	m.Add(&ValidationError{Field: "email"})

	err := m.Err()
	fmt.Println(err)

	fmt.Println("is errQuota:", errors.Is(err, errQuota))

	var ve *ValidationError
	if errors.As(err, &ve) {
		fmt.Println("typed member field:", ve.Field)
	}

	empty := (&multierr.MultiError{Category: "request"}).Err()
	fmt.Println("empty == nil:", empty == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
request: check quota: quota exceeded
invalid email
is errQuota: true
typed member field: email
empty == nil: true
```

### Tests

`TestMultiErrorIsWalksMembers` proves `errors.Is` finds a wrapped sentinel through
the custom `Unwrap() []error`. `TestMultiErrorAsExtractsTyped` proves `errors.As`
pulls out a typed member the same way. `TestMultiErrorEmptyIsNil` proves an empty
aggregate's `Err()` is untyped nil. `TestMultiErrorSliceAccess` proves the public
`Errors` slice still yields the members for direct ranging.

Create `multierr_test.go`:

```go
package multierr

import (
	"errors"
	"fmt"
	"testing"
)

var errSentinel = errors.New("sentinel")

type typedErr struct{ code int }

func (e *typedErr) Error() string { return fmt.Sprintf("code %d", e.code) }

func TestMultiErrorIsWalksMembers(t *testing.T) {
	t.Parallel()

	m := &MultiError{Category: "test"}
	m.Add(fmt.Errorf("wrapped: %w", errSentinel))
	m.Add(errors.New("other"))

	err := m.Err()
	if !errors.Is(err, errSentinel) {
		t.Fatal("errors.Is did not find sentinel through Unwrap() []error")
	}
}

func TestMultiErrorAsExtractsTyped(t *testing.T) {
	t.Parallel()

	m := &MultiError{Category: "test"}
	m.Add(errors.New("plain"))
	m.Add(&typedErr{code: 42})

	var te *typedErr
	if !errors.As(m.Err(), &te) {
		t.Fatal("errors.As did not extract *typedErr")
	}
	if te.code != 42 {
		t.Errorf("code = %d, want 42", te.code)
	}
}

func TestMultiErrorEmptyIsNil(t *testing.T) {
	t.Parallel()

	m := &MultiError{Category: "test"}
	if err := m.Err(); err != nil {
		t.Fatalf("Err() = %v (%T), want untyped nil", err, err)
	}

	// Add(nil) must not create a member.
	m.Add(nil)
	if err := m.Err(); err != nil {
		t.Fatalf("Err() after Add(nil) = %v, want nil", err)
	}
}

func TestMultiErrorSliceAccess(t *testing.T) {
	t.Parallel()

	m := &MultiError{Category: "test"}
	m.Add(errSentinel)
	m.Add(&typedErr{code: 1})

	if len(m.Errors) != 2 {
		t.Fatalf("len(Errors) = %d, want 2", len(m.Errors))
	}
}
```

## Review

`MultiError` is correct when `errors.Is` and `errors.As` traverse it — which they
do only because `Unwrap() []error` is present; delete that method and the two tree
tests fail, which is the lesson. The empty-is-nil test guards the typed-nil trap:
`Err()` must return literal nil when there are no members, so build the aggregate
and call `Err()` rather than returning the `*MultiError` directly. Reach for this
type when you need to carry a category, count, or code the join cannot hold;
otherwise `errors.Join` is less code for the same tree walk. Run with `-race`.

## Resources

- [errors.Is and errors.As](https://pkg.go.dev/errors#Is) — the tree walk over `Unwrap() error` and `Unwrap() []error`.
- [Go 1.20 release notes: errors](https://go.dev/doc/go1.20#errors) — the addition of `Unwrap() []error` support.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-batch-partial-success.md](06-batch-partial-success.md) | Next: [08-unwrap-does-not-unwrap-join.md](08-unwrap-does-not-unwrap-join.md)
