# Exercise 2: The typed-nil error return bug in a repository layer

This is the single most common real Go outage cause: a repository method returns
a concrete `*NotFoundError` that is nil on the happy path, so the `error`
interface is non-nil forever and every caller's `if err != nil` fires on
success. You write the buggy version, prove the failure with a test, then fix it
two ways.

## What you'll build

```text
repotypednil/              independent module: example.com/repotypednil
  go.mod                   go 1.26
  repo.go                  User; NotFoundError (+ Is); FindBuggy; Find; FindVar
  cmd/
    demo/
      main.go              shows FindBuggy's false-positive vs Find's clean nil
  repo_test.go             bug proof; fixed-nil proof; errors.As/Is on miss
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: `FindBuggy` (returns a concrete `*NotFoundError` variable — the trap), `Find` (returns an explicit `nil` literal), `FindVar` (returns an `error`-typed variable).
- Test: `FindBuggy` returns `err != nil` on success (documents the trap); `Find`/`FindVar` return `err == nil` on success; the miss path yields a `*NotFoundError` via `errors.As` and matches a sentinel via `errors.Is`.
- Verify: `go test -count=1 -race ./...`

### Why the bug is invisible until it isn't

`FindBuggy` declares `var notFound *NotFoundError` — a nil *concrete pointer*.
On the miss path it assigns a real error and returns it; correct. On the happy
path it returns that same `notFound` variable, still nil, through the `error`
return. Here is the trap: assigning a nil `*NotFoundError` to an `error` produces
an interface whose dynamic type is `*NotFoundError` and whose dynamic value is
nil. One word is non-nil, so the interface is not equal to nil. `err != nil` is
true even though nothing failed. The caller runs its error branch, logs a phantom
error, maybe returns a 500 on a request that succeeded, or dereferences the
"error" and panics one layer up.

The two fixes both restore the two-word invariant. `Find` returns the `nil`
literal directly on success — a `nil` literal assigned to `error` leaves both
words nil. `FindVar` declares `var err error` (interface type, not concrete) and
returns that; an `error` variable left at its zero value is a genuine nil
interface. Either is correct; the rule is simply "never return a concrete
pointer type through an interface return."

The miss path also demonstrates the two idiomatic error-inspection tools.
`errors.As(err, &target)` unwraps the chain looking for a `*NotFoundError` and
assigns it to `target` — that is how a handler recovers the structured error to
read its fields. `errors.Is(err, ErrNotFound)` matches against a sentinel;
`NotFoundError` opts in by implementing an `Is` method, so callers can test the
category without depending on the concrete type.

Create `repo.go`:

```go
package repotypednil

import (
	"errors"
	"fmt"
)

// ErrNotFound is the sentinel a caller can match with errors.Is without
// depending on the concrete *NotFoundError type.
var ErrNotFound = errors.New("not found")

type User struct {
	ID   string
	Name string
}

// NotFoundError is a structured domain error carrying the missing id.
type NotFoundError struct {
	ID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("user %q not found", e.ID)
}

// Is lets errors.Is(err, ErrNotFound) succeed for any *NotFoundError.
func (e *NotFoundError) Is(target error) bool {
	return target == ErrNotFound
}

var users = map[string]User{
	"u1": {ID: "u1", Name: "alice"},
	"u2": {ID: "u2", Name: "bob"},
}

// FindBuggy demonstrates the typed-nil trap. It declares a concrete pointer and
// returns it through the error result; on the happy path that pointer is nil,
// so the returned error interface wraps a (*NotFoundError)(nil) and is non-nil.
func FindBuggy(id string) (*User, error) {
	var notFound *NotFoundError
	u, ok := users[id]
	if !ok {
		notFound = &NotFoundError{ID: id}
		return nil, notFound
	}
	return &u, notFound // BUG: typed-nil *NotFoundError returned as error
}

// Find fixes the bug by returning an explicit nil literal on success. A nil
// literal assigned to error leaves both interface words nil.
func Find(id string) (*User, error) {
	u, ok := users[id]
	if !ok {
		return nil, &NotFoundError{ID: id}
	}
	return &u, nil
}

// FindVar fixes the bug by keeping the error as the interface type. A zero-value
// error variable is a genuine nil interface.
func FindVar(id string) (*User, error) {
	var err error
	u, ok := users[id]
	if !ok {
		err = &NotFoundError{ID: id}
		return nil, err
	}
	return &u, err
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/repotypednil"
)

func main() {
	// The trap: success, yet err != nil.
	_, buggyErr := repotypednil.FindBuggy("u1")
	fmt.Printf("FindBuggy(\"u1\"): err != nil? %v (typed-nil trap)\n", buggyErr != nil)

	// The fix: success, err is a genuine nil.
	_, err := repotypednil.Find("u1")
	fmt.Printf("Find(\"u1\"):      err != nil? %v\n", err != nil)

	// The miss path: a structured error recovered via errors.As.
	_, missErr := repotypednil.Find("ghost")
	var nf *repotypednil.NotFoundError
	if errors.As(missErr, &nf) {
		fmt.Printf("Find(\"ghost\"):   not found id=%q, is ErrNotFound? %v\n",
			nf.ID, errors.Is(missErr, repotypednil.ErrNotFound))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
FindBuggy("u1"): err != nil? true (typed-nil trap)
Find("u1"):      err != nil? false
Find("ghost"):   not found id="ghost", is ErrNotFound? true
```

### Tests

Create `repo_test.go`:

```go
package repotypednil

import (
	"errors"
	"testing"
)

// TestBuggyReturnsNonNilOnSuccess documents the trap: even on the happy path,
// FindBuggy's error is non-nil, and errors.As reveals it is a typed nil.
func TestBuggyReturnsNonNilOnSuccess(t *testing.T) {
	t.Parallel()

	u, err := FindBuggy("u1")
	if u == nil {
		t.Fatal("FindBuggy(u1) returned nil user")
	}
	if err == nil {
		t.Fatal("expected the typed-nil trap: err should be non-nil on success")
	}

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatal("errors.As should match the concrete *NotFoundError type")
	}
	if nf != nil {
		t.Fatalf("the wrapped pointer should itself be nil, got %+v", nf)
	}
}

func TestFixedReturnsNilOnSuccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		find func(string) (*User, error)
	}{
		{"Find returns nil literal", Find},
		{"FindVar returns error variable", FindVar},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, err := tc.find("u1")
			if err != nil {
				t.Fatalf("expected nil error on success, got %v", err)
			}
			if u == nil || u.Name != "alice" {
				t.Fatalf("expected user alice, got %+v", u)
			}
		})
	}
}

func TestMissPathErrorInspection(t *testing.T) {
	t.Parallel()

	_, err := Find("ghost")
	if err == nil {
		t.Fatal("expected an error for a missing id")
	}

	t.Run("errors.As recovers the structured error", func(t *testing.T) {
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatal("errors.As should populate *NotFoundError")
		}
		if nf == nil || nf.ID != "ghost" {
			t.Fatalf("expected id ghost, got %+v", nf)
		}
	})

	t.Run("errors.Is matches the sentinel", func(t *testing.T) {
		if !errors.Is(err, ErrNotFound) {
			t.Fatal("errors.Is should match ErrNotFound via the Is method")
		}
	})
}
```

## Review

The bug is correct-looking code that returns a concrete `*NotFoundError`
variable through an `error` result. `TestBuggyReturnsNonNilOnSuccess` proves the
consequence: on success the error is non-nil, and `errors.As` shows the wrapped
pointer is itself nil — a typed nil. The fixes restore the invariant that a
successful call returns a genuine nil interface: `Find` returns the `nil`
literal, `FindVar` returns a zero-value `error` variable, and
`TestFixedReturnsNilOnSuccess` confirms both. On the miss path, `errors.As`
recovers the structured error's `ID` field and `errors.Is` matches the sentinel
through `NotFoundError.Is`. Recognize this pattern on sight in code review: a
method whose named error result or a local is a concrete pointer type is a
typed-nil trap waiting to fire.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical writeup of this exact bug.
- [`errors` package](https://pkg.go.dev/errors) — `errors.As`, `errors.Is`, and the `Is`/`As` opt-in methods.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — wrapping, `%w`, and inspection.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-optional-dependency-null-object.md](03-optional-dependency-null-object.md)
