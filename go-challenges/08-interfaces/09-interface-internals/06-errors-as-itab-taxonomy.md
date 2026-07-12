# Exercise 6: Build A Service Error Taxonomy Resolved By errors.As At Runtime

Every service maps internal errors to HTTP status codes at the edge. Done well,
the mapping is a small function that walks the wrapped error chain and asks, at
each link, "is this a validation error? a conflict? the not-found sentinel?". That
question is an interface-satisfaction check performed at runtime — the same
machinery behind type assertions. This module builds the taxonomy and the mapper.

This module is fully self-contained: its own module, code, demo, and tests.

## What you'll build

```text
errtax/                     independent module: example.com/errtax
  go.mod                    go 1.26
  errtax.go                 ValidationError, ConflictError, ErrNotFound; StatusFor; layered wrap
  cmd/
    demo/
      main.go               wraps an error through repo->service->handler, maps to status
  errtax_test.go            chain-to-status table, errors.As target, sentinel survival, Join
```

- Files: `errtax.go`, `cmd/demo/main.go`, `errtax_test.go`.
- Implement: `ValidationError`, `ConflictError`, the `ErrNotFound` sentinel, layered wrapping with `%w`, and `StatusFor(error) int` using `errors.As`/`errors.Is`.
- Test: wrapped chains map to the expected status; `errors.As` populates a `*ValidationError` with the wrapped value; a sentinel survives multiple wrap layers via `errors.Is`; `errors.Join` multi-error matching.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### How errors.As resolves the taxonomy

An error chain built with `%w` is a linked list: each `fmt.Errorf("...: %w", err)`
produces a wrapper whose `Unwrap() error` returns the next link. `errors.As(err,
&target)` walks that chain and, at each link, asks whether the link's concrete
type is assignable to the type `*target` points at — an interface-satisfaction
check against the runtime type metadata. On the first match it assigns the
concrete value *through* the target pointer, using the same type machinery that
backs a type assertion. `errors.Is(err, sentinel)` walks the same chain but
compares each link to the sentinel by `==` (or an `Is` method), which is why a
package-level sentinel like `ErrNotFound` still matches after several wrap layers:
identity is preserved through wrapping.

`StatusFor` is the edge mapper. It tries the most specific concrete types first
with `errors.As` (a `*ValidationError` anywhere in the chain is a 400, a
`*ConflictError` a 409), then the sentinel with `errors.Is` (404), then falls back
to 500. Order matters only where a chain could match more than one arm; here each
error is one kind, so the arms are independent, but writing the specific checks
before the catch-all is the discipline that keeps the mapper honest. Because
`errors.As`/`errors.Is` traverse `Unwrap`, the mapper works no matter how many
layers — repository, service, handler — wrapped the original error, which is what
lets each layer add context with `%w` without the edge losing the ability to
classify it.

`errors.Join(a, b)` is the multi-error case: it builds an error whose `Unwrap()
[]error` returns both, and `errors.As`/`errors.Is` traverse the tree, so a joined
error matches if *any* branch does — useful when a validation pass accumulates
several failures.

Create `errtax.go`:

```go
package errtax

import (
	"errors"
	"fmt"
	"net/http"
)

// ErrNotFound is a sentinel returned when a resource does not exist. Callers
// match it with errors.Is even after it is wrapped.
var ErrNotFound = errors.New("not found")

// ValidationError reports a bad field in a request payload.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation failed on %q: %s", e.Field, e.Reason)
}

// ConflictError reports that an operation conflicts with existing state.
type ConflictError struct {
	Resource string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict on %s", e.Resource)
}

// StatusFor maps an error (possibly wrapped through several layers) to an HTTP
// status code by asking, in order, whether it is a validation error, a conflict,
// or the not-found sentinel, before falling back to 500.
func StatusFor(err error) int {
	if err == nil {
		return http.StatusOK
	}

	var ve *ValidationError
	if errors.As(err, &ve) {
		return http.StatusBadRequest
	}

	var ce *ConflictError
	if errors.As(err, &ce) {
		return http.StatusConflict
	}

	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}

	return http.StatusInternalServerError
}

// --- a layered stack that wraps errors with %w on the way up ---

// RepoLoad simulates a repository read that fails for a known id.
func RepoLoad(id string) error {
	switch id {
	case "missing":
		return fmt.Errorf("repo load %q: %w", id, ErrNotFound)
	case "dup":
		return fmt.Errorf("repo load %q: %w", id, &ConflictError{Resource: "user:" + id})
	case "bad":
		return fmt.Errorf("repo load %q: %w", id, &ValidationError{Field: "id", Reason: "empty"})
	default:
		return nil
	}
}

// ServiceLoad wraps the repository error with its own context.
func ServiceLoad(id string) error {
	if err := RepoLoad(id); err != nil {
		return fmt.Errorf("service load: %w", err)
	}
	return nil
}
```

### The runnable demo

The demo runs several ids through the two-layer stack and prints the HTTP status
each resolves to, showing that a doubly-wrapped error still classifies correctly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/errtax"
)

func main() {
	for _, id := range []string{"ok", "missing", "dup", "bad"} {
		err := errtax.ServiceLoad(id)
		fmt.Printf("id=%-8s status=%d err=%v\n", id, errtax.StatusFor(err), err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
id=ok       status=200 err=<nil>
id=missing  status=404 err=service load: repo load "missing": not found
id=dup      status=409 err=service load: repo load "dup": conflict on user:dup
id=bad      status=400 err=service load: repo load "bad": validation failed on "id": empty
```

### Tests

`TestStatusForChains` tabulates each id's doubly-wrapped chain against its expected
status. `TestErrorsAsPopulatesTarget` proves `errors.As` assigns the wrapped
concrete `*ValidationError` into a target pointer, and that the fields survived the
wrapping. `TestSentinelSurvivesWrapping` wraps `ErrNotFound` several layers deep
and asserts `errors.Is` still matches. `TestJoinMatching` builds a joined error and
shows `errors.As`/`errors.Is` match a branch.

Create `errtax_test.go`:

```go
package errtax

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestStatusForChains(t *testing.T) {
	t.Parallel()

	tests := []struct {
		id   string
		want int
	}{
		{"ok", http.StatusOK},
		{"missing", http.StatusNotFound},
		{"dup", http.StatusConflict},
		{"bad", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			t.Parallel()
			got := StatusFor(ServiceLoad(tc.id))
			if got != tc.want {
				t.Fatalf("StatusFor(%s) = %d, want %d", tc.id, got, tc.want)
			}
		})
	}
}

func TestErrorsAsPopulatesTarget(t *testing.T) {
	t.Parallel()

	err := ServiceLoad("bad") // wrapped *ValidationError, two layers deep

	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("errors.As did not find *ValidationError in %v", err)
	}
	if ve.Field != "id" || ve.Reason != "empty" {
		t.Fatalf("target = %+v, want Field=id Reason=empty", ve)
	}
}

func TestSentinelSurvivesWrapping(t *testing.T) {
	t.Parallel()

	err := ErrNotFound
	for i := range 5 {
		err = fmt.Errorf("layer %d: %w", i, err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("ErrNotFound did not survive 5 wrap layers")
	}
	if StatusFor(err) != http.StatusNotFound {
		t.Fatalf("deeply wrapped sentinel status = %d, want 404", StatusFor(err))
	}
}

func TestJoinMatching(t *testing.T) {
	t.Parallel()

	joined := errors.Join(
		&ValidationError{Field: "name", Reason: "required"},
		ErrNotFound,
	)

	if !errors.Is(joined, ErrNotFound) {
		t.Fatal("errors.Is should match ErrNotFound in a joined error")
	}
	var ve *ValidationError
	if !errors.As(joined, &ve) {
		t.Fatal("errors.As should find *ValidationError in a joined error")
	}
	// StatusFor tries ValidationError before the sentinel, so the joined error
	// resolves to 400.
	if got := StatusFor(joined); got != http.StatusBadRequest {
		t.Fatalf("StatusFor(joined) = %d, want 400", got)
	}
}
```

## Review

The taxonomy is correct when the edge mapper classifies an error regardless of how
deeply the layers wrapped it, and the tests pin exactly that: an id runs through
two `%w` layers and still resolves to the right status, and a sentinel survives
five. The mechanism to internalize is that `errors.As` performs a runtime
interface-satisfaction check down the `Unwrap` chain and assigns through your
target pointer, while `errors.Is` compares identity — so use `As` for typed errors
you need to read fields from, and `Is` for sentinels. Keep the specific `As`
checks before the sentinel and the 500 fallback, and never classify by string
matching on `Error()`. Run `go vet ./...`; it flags a `%w` used with the wrong
argument count and other `errors` misuse.

## Resources

- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w`, `errors.Is`, and `errors.As` and how they walk the chain.
- [errors.As](https://pkg.go.dev/errors#As) — the exact rule for matching and assigning through the target pointer.
- [errors.Join](https://pkg.go.dev/errors#Join) — multi-error construction and traversal.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-optional-interface-upgrade.md](05-optional-interface-upgrade.md) | Next: [07-reflect-vs-typeswitch-validator.md](07-reflect-vs-typeswitch-validator.md)
