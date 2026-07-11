# Exercise 2: Fix The Repository That Returns A Non-Nil Error For Success

The most expensive one-line interface bug in Go: a repository method whose
success path returns a nil concrete pointer through an `error` return, so callers
see a failure that never happened. This module reproduces it, explains it from the
two-word layout, fixes it, and adds a diagnostic that can see what `== nil`
cannot.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
repo/                       independent module: example.com/repo
  go.mod                    go 1.26
  repo.go                   NotFoundError; Store.FindBuggy (trap) / FindFixed; IsTypedNil
  cmd/
    demo/
      main.go               prints the false-error vs the fixed path side by side
  repo_test.go              trap documented, fix asserted, errors.As, diagnostic
```

- Files: `repo.go`, `cmd/demo/main.go`, `repo_test.go`.
- Implement: a `Store` with `FindBuggy` (returns a typed nil on success) and `FindFixed` (returns interface nil), a concrete `*NotFoundError`, and an `IsTypedNil(error) bool` diagnostic using reflect.
- Test: the buggy path's success return is non-nil (the trap, documented); the fixed path's success return is `== nil`; a real `*NotFoundError` is still detected via `errors.As`; `IsTypedNil` sees the typed nil `== nil` cannot.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/repo/cmd/demo
cd ~/go-exercises/repo
go mod init example.com/repo
go mod edit -go=1.26
```

### The trap, from the two-word layout

An `error` value is `[ *itab ][ *data ]`. It is `== nil` only when both words are
nil. `FindBuggy` declares its internal error variable with the *concrete* type
`*NotFoundError`:

```go
var err *NotFoundError // nil pointer of a concrete type
```

On the success path `err` is never assigned, so it stays a nil `*NotFoundError`.
Returning it through the `error` return boxes it: the itab word is set to the
(error, `*NotFoundError`) pair, and only the data word is nil. The result is a
*non-nil* interface holding a nil pointer. The caller's `if err != nil` fires,
the request 500s, on-call gets paged, and the repository was healthy the whole
time. This is not a hypothetical — it is one of the most common real Go bugs,
which is why `go vet` has a dedicated `nilness`-style analysis and why the pattern
is worth burning into muscle memory.

`FindFixed` avoids it by never returning a typed nil: its success path returns the
untyped `nil` literal, whose interface header is both-words-nil, and its failure
path returns a genuinely populated `*NotFoundError`. The rule is mechanical:
functions that return `error` should return `nil` on success, never a nil value of
a concrete error type.

The diagnostic `IsTypedNil` shows the gap between what the language operator sees
and what the runtime metadata holds. `err == nil` compares the interface header
and reports false for a typed nil. `reflect.ValueOf(err)` reads the same `*_type`
the itab points at, sees the kind is a pointer, and `.IsNil()` reports that the
pointer is nil. Reflection can see the typed nil precisely because it inspects the
concrete value behind the interface rather than the interface header.

Create `repo.go`:

```go
package repo

import (
	"fmt"
	"reflect"
)

// NotFoundError is a concrete error type returned when a record is missing.
type NotFoundError struct {
	ID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("record %q not found", e.ID)
}

// Store is an in-memory record store.
type Store struct {
	data map[string]string
}

// NewStore builds a Store over the seed data.
func NewStore(seed map[string]string) *Store {
	return &Store{data: seed}
}

// FindBuggy demonstrates the typed-nil trap. Its error variable has the concrete
// type *NotFoundError; on the success path it stays a nil *NotFoundError, and
// returning it through the error interface yields a NON-nil interface value
// (type word set, data word nil), so callers checking err != nil see a false
// failure.
func (s *Store) FindBuggy(id string) (string, error) {
	var err *NotFoundError
	rec, ok := s.data[id]
	if !ok {
		err = &NotFoundError{ID: id}
		return "", err
	}
	return rec, err // BUG: on success err is a typed nil, not the interface nil.
}

// FindFixed returns the untyped nil literal on the success path, so the returned
// interface is genuinely nil.
func (s *Store) FindFixed(id string) (string, error) {
	rec, ok := s.data[id]
	if !ok {
		return "", &NotFoundError{ID: id}
	}
	return rec, nil
}

// IsTypedNil reports whether err is a non-nil interface wrapping a nil pointer
// (or other nilable kind). The == nil check cannot see this; reflect can, because
// it reads the concrete value behind the interface header.
func IsTypedNil(err error) bool {
	if err == nil {
		return false
	}
	v := reflect.ValueOf(err)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func:
		return v.IsNil()
	default:
		return false
	}
}
```

### The runnable demo

The demo calls both methods on a present key and prints, for each, whether the
returned error is `== nil` and whether `IsTypedNil` flags it. The buggy path
reports a non-nil error for a successful lookup; the fixed path reports nil.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/repo"
)

func main() {
	s := repo.NewStore(map[string]string{"u1": "alice"})

	_, errBuggy := s.FindBuggy("u1")
	fmt.Printf("FindBuggy(u1): err==nil? %v  typed-nil? %v\n",
		errBuggy == nil, repo.IsTypedNil(errBuggy))

	_, errFixed := s.FindFixed("u1")
	fmt.Printf("FindFixed(u1): err==nil? %v  typed-nil? %v\n",
		errFixed == nil, repo.IsTypedNil(errFixed))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
FindBuggy(u1): err==nil? false  typed-nil? true
FindFixed(u1): err==nil? true  typed-nil? false
```

The two lines are the whole lesson: for the *same successful lookup*, the buggy
path reports a non-nil error that `IsTypedNil` unmasks as a nil pointer, while the
fixed path reports a genuinely nil error.

### Tests

`TestBuggyReturnsFalseErrorOnSuccess` documents the trap: a present key still
produces a non-nil error from `FindBuggy`, and `IsTypedNil` confirms it is a nil
pointer in disguise. `TestFixedReturnsNilOnSuccess` asserts the fix: a present key
gives `err == nil`. `TestFixedDetectsNotFound` proves the fixed path still surfaces
a real miss and that `errors.As` populates a `*NotFoundError` target with the
wrapped concrete value. `TestBuggyStillReportsRealNotFound` shows the trap is only
on the success path — a genuine miss is a real error either way.

Create `repo_test.go`:

```go
package repo

import (
	"errors"
	"testing"
)

func TestBuggyReturnsFalseErrorOnSuccess(t *testing.T) {
	t.Parallel()

	s := NewStore(map[string]string{"u1": "alice"})
	rec, err := s.FindBuggy("u1")
	if rec != "alice" {
		t.Fatalf("FindBuggy(u1) rec = %q, want alice", rec)
	}
	// The trap: the value is found, yet err != nil because the interface wraps a
	// typed nil.
	if err == nil {
		t.Fatal("expected the buggy path to return a non-nil (typed-nil) error")
	}
	if !IsTypedNil(err) {
		t.Fatal("expected the buggy error to be a typed nil")
	}
}

func TestFixedReturnsNilOnSuccess(t *testing.T) {
	t.Parallel()

	s := NewStore(map[string]string{"u1": "alice"})
	rec, err := s.FindFixed("u1")
	if err != nil {
		t.Fatalf("FindFixed(u1) err = %v, want nil", err)
	}
	if IsTypedNil(err) {
		t.Fatal("fixed path should not return a typed nil")
	}
	if rec != "alice" {
		t.Fatalf("FindFixed(u1) rec = %q, want alice", rec)
	}
}

func TestFixedDetectsNotFound(t *testing.T) {
	t.Parallel()

	s := NewStore(map[string]string{"u1": "alice"})
	_, err := s.FindFixed("missing")
	if err == nil {
		t.Fatal("FindFixed(missing) should return an error")
	}

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("errors.As did not match *NotFoundError, got %T", err)
	}
	if nf.ID != "missing" {
		t.Fatalf("NotFoundError.ID = %q, want missing", nf.ID)
	}
}

func TestBuggyStillReportsRealNotFound(t *testing.T) {
	t.Parallel()

	s := NewStore(map[string]string{})
	_, err := s.FindBuggy("missing")

	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("errors.As did not match *NotFoundError, got %T", err)
	}
	if IsTypedNil(err) {
		t.Fatal("a real NotFoundError must not be a typed nil")
	}
}
```

## Review

The bug is one keyword away from the fix, and the tests pin both halves so a
future refactor cannot silently reintroduce it: `FindBuggy` on a present key
returns non-nil (the trap), `FindFixed` on a present key returns `== nil` (the
fix). The trap is not that `FindBuggy` fabricates an error object — it never
allocates one on the success path — it is that a nil *concrete pointer* becomes a
non-nil *interface*. The mental model to keep: an interface is nil only when both
its words are nil, and returning a typed nil sets the type word. Prefer returning
the untyped `nil` on success, and when you must funnel a concrete error variable
through an `error` return, assign it the interface value, not a nil pointer. Run
`go vet ./...`; its analyses flag several shapes of this mistake.

## Resources

- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the canonical explanation of this exact trap.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `errors.As`/`errors.Is` and the `%w` wrapping the fixed path relies on.
- [reflect.Value.IsNil](https://pkg.go.dev/reflect#Value.IsNil) — the diagnostic that sees the typed nil the `== nil` operator cannot.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-type-switch-dispatcher.md](01-type-switch-dispatcher.md) | Next: [03-interface-comparability-panic.md](03-interface-comparability-panic.md)
