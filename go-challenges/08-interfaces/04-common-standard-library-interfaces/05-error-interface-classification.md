# Exercise 5: Custom error Types with Unwrap, errors.Is and errors.As

`error` is a single-method interface, but in a real service it is a
classification system. A repository wraps a low-level failure with `%w`; an HTTP
handler walks that chain with `errors.Is` and `errors.As` to pick a status code,
without ever matching on error strings. This module builds a sentinel, a typed
error, a wrapping domain error, and the handler that classifies them.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
domainerr/                  independent module: example.com/domainerr
  go.mod
  domainerr.go              ErrNotFound sentinel; ValidationError; RepoError (Unwrap); StatusFor
  cmd/
    demo/
      main.go               classifies a not-found and a validation failure
  domainerr_test.go         Is through layers, As extraction, Join, handler status mapping
```

- Files: `domainerr.go`, `cmd/demo/main.go`, `domainerr_test.go`.
- Implement: a sentinel `ErrNotFound`; a `ValidationError` carrying a field; a `RepoError` with `Unwrap() error`; and `StatusFor(err) int` mapping errors to HTTP status codes.
- Test: `errors.Is` finds a wrapped sentinel through multiple layers; `errors.As` extracts the typed `ValidationError` and its field; `errors.Join` groups two failures and `Is` matches either; `StatusFor` maps `ErrNotFound` to 404 and a `ValidationError` to 400.
- Verify: `go test -count=1 -race ./...`

### Sentinels, typed errors, and the Unwrap chain

There are two shapes of domain error and they answer different questions. A
*sentinel* like `ErrNotFound` answers "which category?" — you compare against it
with `errors.Is`. A *typed* error like `ValidationError` answers "which category,
plus what details?" — it carries a `Field`, and you pull it out with `errors.As`.
Both are found through arbitrarily deep wrapping because `errors.Is`/`As` walk the
chain built by `%w`.

`RepoError` is a wrapping domain error: it adds context (`Op`) and keeps the
cause reachable by implementing `Unwrap() error`. That one method is what lets
`errors.Is(err, ErrNotFound)` succeed even when the not-found sentinel is buried
three layers down. `fmt.Errorf("...: %w", err)` produces an anonymous wrapper with
the same `Unwrap` behaviour; use `%w` for ad-hoc context and a named type like
`RepoError` when callers need to inspect the wrapper's own fields.

`StatusFor` is the payoff and the anti-pattern killer. It classifies with
`errors.Is` for the sentinel and `errors.As` for the typed error — never `==` and
never a type assertion, both of which break the moment someone adds a `%w` layer.
Order matters: check the most specific classifications first, fall through to 500.
`errors.Join` (Go 1.20+) bundles independent failures — two field validations
failing at once — into one error whose chain contains both, so `errors.Is` matches
either and `StatusFor` still sees the validation error.

Create `domainerr.go`:

```go
package domainerr

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrNotFound is the sentinel for a missing resource.
var ErrNotFound = errors.New("resource not found")

// ValidationError is a typed error carrying the offending field.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: field %q: %s", e.Field, e.Msg)
}

// RepoError wraps a lower-level cause with the repository operation that failed.
// Its Unwrap makes the cause reachable by errors.Is / errors.As.
type RepoError struct {
	Op  string
	Err error
}

func (e *RepoError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *RepoError) Unwrap() error { return e.Err }

// LoadUser simulates a repository read that fails for a missing id, wrapping the
// sentinel in a RepoError so callers keep both the operation and the cause.
func LoadUser(id string) error {
	if id == "" || id == "missing" {
		return &RepoError{Op: "load user " + id, Err: ErrNotFound}
	}
	return nil
}

// StatusFor classifies a domain error into an HTTP status code using errors.Is
// and errors.As, so it keeps working no matter how deeply the error is wrapped.
func StatusFor(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}
```

### The runnable demo

The demo runs a not-found load and a validation failure through `StatusFor`,
printing the chosen status and the full error message so you can see the wrapping.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/domainerr"
)

func main() {
	notFound := domainerr.LoadUser("missing")
	fmt.Printf("status=%d err=%v\n", domainerr.StatusFor(notFound), notFound)

	invalid := fmt.Errorf("create order: %w", &domainerr.ValidationError{
		Field: "email",
		Msg:   "must not be empty",
	})
	fmt.Printf("status=%d err=%v\n", domainerr.StatusFor(invalid), invalid)

	ok := domainerr.LoadUser("u-1")
	fmt.Printf("status=%d err=%v\n", domainerr.StatusFor(ok), ok)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the sentinel's text is `resource not found`, so it appears at the tail of the wrapped message):

```
status=404 err=load user missing: resource not found
status=400 err=create order: validation: field "email": must not be empty
status=200 err=<nil>
```

### Tests

`TestIsThroughLayers` wraps `ErrNotFound` twice (a `RepoError` then a `%w`) and
asserts `errors.Is` still finds it. `TestAsExtractsField` pulls the
`*ValidationError` out of a wrapped chain and checks its `Field`.
`TestJoinMatchesEither` joins two errors and asserts `Is` matches both.
`TestStatusForMapping` is table-driven over the classification rules.

Create `domainerr_test.go`:

```go
package domainerr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestIsThroughLayers(t *testing.T) {
	t.Parallel()

	base := &RepoError{Op: "load user u-9", Err: ErrNotFound}
	wrapped := fmt.Errorf("handler: %w", base)

	if !errors.Is(wrapped, ErrNotFound) {
		t.Fatal("errors.Is failed to find ErrNotFound through two wrap layers")
	}
}

func TestAsExtractsField(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("create order: %w", &ValidationError{Field: "email", Msg: "required"})

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatal("errors.As failed to extract *ValidationError")
	}
	if ve.Field != "email" {
		t.Fatalf("ve.Field = %q, want %q", ve.Field, "email")
	}
}

func TestJoinMatchesEither(t *testing.T) {
	t.Parallel()

	other := errors.New("rate limited")
	joined := errors.Join(ErrNotFound, other)

	if !errors.Is(joined, ErrNotFound) {
		t.Error("joined error should match ErrNotFound")
	}
	if !errors.Is(joined, other) {
		t.Error("joined error should match the other failure")
	}
}

func TestStatusForMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, http.StatusOK},
		{"not found", LoadUser("missing"), http.StatusNotFound},
		{"wrapped not found", fmt.Errorf("x: %w", ErrNotFound), http.StatusNotFound},
		{"validation", &ValidationError{Field: "name", Msg: "too long"}, http.StatusBadRequest},
		{"wrapped validation", fmt.Errorf("y: %w", &ValidationError{Field: "n"}), http.StatusBadRequest},
		{"other", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		if got := StatusFor(tc.err); got != tc.want {
			t.Errorf("%s: StatusFor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func ExampleStatusFor() {
	fmt.Println(StatusFor(fmt.Errorf("repo: %w", ErrNotFound)))
	// Output: 404
}
```

## Review

The error layer is correct when classification survives wrapping: `errors.Is`
finds the sentinel and `errors.As` extracts the typed error no matter how many
`%w` layers sit on top, and `StatusFor` never inspects an error string. The
mistake this defends against is `err == ErrNotFound` or `err.(*ValidationError)`,
both of which silently stop matching the first time a caller adds context with
`%w`. `RepoError.Unwrap` is the hinge that makes the whole chain walkable; drop it
and `errors.Is(repoErr, ErrNotFound)` returns false. `errors.Join` shows the
multi-failure case a validation pipeline needs. Run `go test -race`.

## Resources

- [errors package](https://pkg.go.dev/errors) — `Is`, `As`, `Join`, `Unwrap`.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w`, `Is`, and `As` explained by the Go team.
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusBadRequest`, and friends.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-domain-stringer.md](04-domain-stringer.md) | Next: [06-json-marshaler-unmarshaler.md](06-json-marshaler-unmarshaler.md)
