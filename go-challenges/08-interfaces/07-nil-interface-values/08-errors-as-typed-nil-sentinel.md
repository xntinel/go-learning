# Exercise 8: errors.As/Is with typed-nil sentinels in an API error path

An HTTP handler maps domain errors to status codes with `errors.As` into a
`*DomainError` and `errors.Is` against sentinels. The trap: a source helper that
returns a typed-nil `*DomainError` as `error` makes `errors.As` succeed with a
nil target, and the mapper dereferences a nil pointer. You write the mapper,
reproduce the nil-deref, and fix the source.

## What you'll build

```text
apierr/                    independent module: example.com/apierr
  go.mod                   go 1.26
  apierr.go                DomainError; StatusFor; validateBuggy; validate; ErrUnauthorized
  cmd/
    demo/
      main.go              map NotFound/Validation/unauthorized/unknown to status
  apierr_test.go           buggy nil-deref (guarded); fixed mapping; errors.As/Is
```

- Files: `apierr.go`, `cmd/demo/main.go`, `apierr_test.go`.
- Implement: `StatusFor(err error) int` using `errors.Is` for a sentinel and `errors.As` into `*DomainError`; `validateBuggy` (returns a typed-nil `*DomainError` — the trap) and `validate` (returns a real nil interface).
- Test: `StatusFor(validateBuggy("ok"))` hits a nil-deref (recover-guarded); the fixed path maps NotFound to 404, Validation to 422, unauthorized to 401, unknown to 500; `errors.As` populates a non-nil `*DomainError` only when a real one is present.
- Verify: `go test -count=1 -race ./...`

### Why the typed nil re-enters through errors.As

`StatusFor` is the mapper: it converts any domain error to an HTTP status. It
first checks `errors.Is(err, ErrUnauthorized)` for a sentinel category, then uses
`errors.As(err, &de)` to recover a structured `*DomainError` and switch on its
`Kind`. This is the idiomatic shape, and it is safe — as long as the errors
flowing into it obey the two-word rule.

`validateBuggy` breaks that rule. It declares `var de *DomainError` and returns
it. On the happy path `de` is nil, but returning it through the `error` result
produces a non-nil interface wrapping a nil `*DomainError`. When that value
reaches `StatusFor`, `errors.As(err, &de)` finds a dynamic type of
`*DomainError`, reports a match, and assigns the nil pointer to `de`. `errors.As`
returned true, so the mapper enters the `switch de.Kind` branch and dereferences
a nil pointer — a panic on a request that actually succeeded validation.

The fix is at the source, not the call site. `validate` returns an explicit `nil`
on the happy path and a real `*DomainError` on failure, so a successful
validation produces a genuine nil interface, `errors.As` does not match, and
`StatusFor` falls through to 200. Guarding every `errors.As` result with an
extra nil check would be treating the symptom; the disease is a helper that emits
a typed nil.

Create `apierr.go`:

```go
package apierr

import (
	"errors"
	"net/http"
)

// ErrUnauthorized is a sentinel matched with errors.Is, independent of any
// concrete error type.
var ErrUnauthorized = errors.New("unauthorized")

type Kind int

const (
	KindNotFound Kind = iota
	KindValidation
	KindInternal
)

// DomainError is a structured domain error carrying a category.
type DomainError struct {
	Kind Kind
	Msg  string
}

func (e *DomainError) Error() string { return e.Msg }

// StatusFor maps a domain error to an HTTP status. It relies on the error obeying
// the two-word rule: a successful call must return a genuine nil interface.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	}

	var de *DomainError
	if errors.As(err, &de) {
		switch de.Kind {
		case KindNotFound:
			return http.StatusNotFound
		case KindValidation:
			return http.StatusUnprocessableEntity
		default:
			return http.StatusInternalServerError
		}
	}
	return http.StatusInternalServerError
}

// validateBuggy demonstrates the trap: it returns a concrete *DomainError that
// is nil on success, so the error interface is a typed nil and errors.As matches
// it with a nil target one layer up.
func validateBuggy(name string) error {
	var de *DomainError
	if name == "" {
		de = &DomainError{Kind: KindValidation, Msg: "name required"}
	}
	return de // BUG: typed-nil *DomainError on the happy path
}

// validate fixes the source by returning a real nil interface on success.
func validate(name string) error {
	if name == "" {
		return &DomainError{Kind: KindValidation, Msg: "name required"}
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/apierr"
)

func main() {
	cases := []struct {
		label string
		err   error
	}{
		{"success", nil},
		{"not found", &apierr.DomainError{Kind: apierr.KindNotFound, Msg: "user missing"}},
		{"validation", &apierr.DomainError{Kind: apierr.KindValidation, Msg: "bad input"}},
		{"unauthorized", fmt.Errorf("token expired: %w", apierr.ErrUnauthorized)},
		{"unknown", fmt.Errorf("disk on fire")},
	}
	for _, c := range cases {
		fmt.Printf("%-13s -> %d\n", c.label, apierr.StatusFor(c.err))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
success       -> 200
not found     -> 404
validation    -> 422
unauthorized  -> 401
unknown       -> 500
```

### Tests

Create `apierr_test.go`:

```go
package apierr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestBuggySourceCausesNilDeref documents the hazard: the typed-nil error from
// validateBuggy makes errors.As match with a nil target, so StatusFor panics.
func TestBuggySourceCausesNilDeref(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected a nil-pointer deref from the typed-nil source")
		}
	}()

	// name is non-empty, so validation "passes" but returns a typed nil.
	_ = StatusFor(validateBuggy("alice"))
}

func TestFixedSourceIsSafe(t *testing.T) {
	t.Parallel()

	if got := StatusFor(validate("alice")); got != http.StatusOK {
		t.Fatalf("validate success -> %d; want 200", got)
	}
	if got := StatusFor(validate("")); got != http.StatusUnprocessableEntity {
		t.Fatalf("validate failure -> %d; want 422", got)
	}
}

func TestStatusMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil is 200", nil, http.StatusOK},
		{"not found is 404", &DomainError{Kind: KindNotFound}, http.StatusNotFound},
		{"validation is 422", &DomainError{Kind: KindValidation}, http.StatusUnprocessableEntity},
		{"internal is 500", &DomainError{Kind: KindInternal}, http.StatusInternalServerError},
		{"wrapped unauthorized is 401", fmt.Errorf("bad token: %w", ErrUnauthorized), http.StatusUnauthorized},
		{"unknown is 500", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := StatusFor(tc.err); got != tc.want {
				t.Fatalf("StatusFor(%s) = %d; want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestErrorsAsPopulatesOnlyRealError(t *testing.T) {
	t.Parallel()

	// A real error: errors.As populates a non-nil target.
	err := fmt.Errorf("wrapped: %w", &DomainError{Kind: KindNotFound, Msg: "x"})
	var de *DomainError
	if !errors.As(err, &de) || de == nil {
		t.Fatalf("errors.As should populate a non-nil *DomainError, got %v", de)
	}

	// A fixed nil source: errors.As does not match.
	var de2 *DomainError
	if errors.As(validate("alice"), &de2) {
		t.Fatal("errors.As should not match a genuine nil error")
	}
}
```

## Review

The mapper is correct when the errors reaching it obey the two-word rule.
`StatusFor` uses `errors.Is` for the `ErrUnauthorized` sentinel and `errors.As`
into `*DomainError` for the structured categories, mapping NotFound to 404,
Validation to 422, unauthorized to 401, and everything else to 500 —
`TestStatusMapping` pins each. `TestBuggySourceCausesNilDeref` reproduces the
trap: `validateBuggy` emits a typed-nil `*DomainError`, `errors.As` matches with
a nil target, and the `switch de.Kind` dereferences nil. The fix is
`validate`, which returns a real nil interface on success, proven by
`TestFixedSourceIsSafe` and `TestErrorsAsPopulatesOnlyRealError`. The lesson:
fix the error *source*, not the `errors.As` call site.

## Resources

- [`errors` package](https://pkg.go.dev/errors) — `errors.As`, `errors.Is`, and `Unwrap`.
- [Go FAQ: Why is my nil error value not equal to nil?](https://go.dev/doc/faq#nil_error) — the typed-nil error, which this exercise pushes through `errors.As`.
- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusUnprocessableEntity`, and friends.

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-middleware-optional-hook.md](09-middleware-optional-hook.md)
