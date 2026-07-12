# Exercise 7: Map Sentinel Domain Errors to HTTP Status Codes

The HTTP boundary of a service has to translate the domain layer's errors into
status codes: a "not found" becomes 404, a "conflict" becomes 409, a validation
failure becomes 422. This module builds that translator as a tagless switch whose
cases are `errors.Is` checks against domain sentinels — dispatch on *wrapped-error
identity*, which an expression switch fundamentally cannot express.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
httperr/                   independent module: example.com/domain-error-to-status
  go.mod                   go 1.24
  httperr.go               domain sentinels; StatusFor(err) int
  cmd/
    demo/
      main.go              runnable demo mapping wrapped errors to statuses
  httperr_test.go          table incl. deeply-wrapped errors, nil, unmapped -> 500
```

- Files: `httperr.go`, `cmd/demo/main.go`, `httperr_test.go`.
- Implement: domain sentinel errors and `StatusFor(err error) int` using a tagless switch of `errors.Is` checks with a `default` of 500.
- Test: a table mapping each sentinel (including one wrapped several layers deep) to its status, an unmapped error to 500, and nil handled explicitly.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/03-switch-statements/07-domain-error-to-status/cmd/demo
cd go-solutions/03-control-flow/03-switch-statements/07-domain-error-to-status
go mod edit -go=1.24
```

### Why this is a tagless switch, not a type switch

The domain layer returns errors wrapped with context: a repository does
`fmt.Errorf("load user %s: %w", id, ErrNotFound)`, and a service layer may wrap
that again. What reaches the HTTP boundary is a chain, and the thing that
identifies it is the *sentinel at the bottom of the chain*, reachable only with
`errors.Is`, which unwraps. An expression switch compares with `==`, so it would
match only the exact wrapped error value, never the sentinel underneath — it is
the wrong tool. A type switch (the next lesson's subject) dispatches on the
dynamic *type*, which is also wrong here: these sentinels are all the same type
(`*errors.errorString`), distinguished by identity, not type. The correct form is
a tagless switch whose cases are `errors.Is(err, ErrX)` predicates.

Because the sentinels are disjoint — an error cannot simultaneously be both
`ErrNotFound` and `ErrConflict` — case order does not affect correctness here.
That is worth stating explicitly *and* contrasting: if two predicates could match
the same error (say a broad `errors.Is(err, ErrValidation)` and a narrower
sub-sentinel wrapped by it), the narrower case would have to come first, exactly
like the range-overlap ordering in the retry classifier. Here they are disjoint,
so the order is free — but that is a property to verify, not assume.

`nil` is handled explicitly as the first case, returning 200: a `StatusFor(nil)`
should not fall through to the 500 default, because "no error" is success, not an
internal failure. The `default` is the fail-closed backstop: any error the
boundary does not recognize is a 500, never a 200, so an unmapped domain error can
never masquerade as success.

Create `httperr.go`:

```go
package httperr

import (
	"errors"
	"net/http"
)

// Domain sentinels the HTTP boundary knows how to translate.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrValidation   = errors.New("validation failed")
	ErrUnauthorized = errors.New("unauthorized")
	ErrRateLimited  = errors.New("rate limited")
)

// StatusFor maps a domain error to an HTTP status. It is a tagless switch of
// errors.Is checks, so it matches a sentinel no matter how many layers it is
// wrapped in. nil is success (200); an unrecognized error fails closed to 500.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrValidation):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrRateLimited):
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"net/http"

	"example.com/domain-error-to-status"
)

func main() {
	deepWrapped := fmt.Errorf("service: %w", fmt.Errorf("repo: %w", httperr.ErrNotFound))

	errs := []error{
		nil,
		httperr.ErrConflict,
		httperr.ErrValidation,
		deepWrapped,
		errors.New("disk on fire"),
	}
	for _, err := range errs {
		code := httperr.StatusFor(err)
		fmt.Printf("%3d %-25s <- %v\n", code, http.StatusText(code), err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
200 OK                        <- <nil>
409 Conflict                  <- conflict
422 Unprocessable Entity      <- validation failed
404 Not Found                 <- service: repo: not found
500 Internal Server Error     <- disk on fire
```

### Tests

`TestStatusFor` maps each sentinel to its status, including one wrapped several
layers deep to prove `errors.Is` unwraps through the chain, plus `nil` to 200 and
an unmapped error to 500. `TestOrderIndependence` re-checks that a deeply-wrapped
sentinel still resolves correctly, documenting that disjoint sentinels make order
irrelevant here.

Create `httperr_test.go`:

```go
package httperr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestStatusFor(t *testing.T) {
	t.Parallel()

	deep := fmt.Errorf("handler: %w", fmt.Errorf("service: %w", fmt.Errorf("repo: %w", ErrNotFound)))

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, http.StatusOK},
		{"not found", ErrNotFound, http.StatusNotFound},
		{"conflict", ErrConflict, http.StatusConflict},
		{"validation", ErrValidation, http.StatusUnprocessableEntity},
		{"unauthorized", ErrUnauthorized, http.StatusUnauthorized},
		{"rate limited", ErrRateLimited, http.StatusTooManyRequests},
		{"wrapped once", fmt.Errorf("wrap: %w", ErrConflict), http.StatusConflict},
		{"wrapped deep", deep, http.StatusNotFound},
		{"unmapped", errors.New("boom"), http.StatusInternalServerError},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := StatusFor(tc.err); got != tc.want {
				t.Fatalf("StatusFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestOrderIndependence(t *testing.T) {
	t.Parallel()

	// The sentinels are disjoint, so a wrapped error resolves to exactly one
	// status regardless of case order. This documents that invariant.
	err := fmt.Errorf("ctx: %w", ErrRateLimited)
	if got := StatusFor(err); got != http.StatusTooManyRequests {
		t.Fatalf("StatusFor(wrapped rate-limited) = %d, want %d", got, http.StatusTooManyRequests)
	}
}
```

## Review

The mapper is correct when a sentinel resolves to its status no matter how deeply
it is wrapped, `nil` is 200, and anything unrecognized is a fail-closed 500. The
`errors.Is` cases are what make wrapping transparent — `TestStatusFor`'s
three-layer `deep` error proves the boundary reads the sentinel at the bottom of
the chain, not the wrapper on top. The explicit `nil` case keeps "no error" from
falling into the 500 default, and the 500 default keeps an unmapped domain error
from ever masquerading as success. The contrast to keep in mind for the next
lesson: this is dispatch on error *identity* via `errors.Is`; a type switch
dispatches on dynamic *type*, and the two are not interchangeable.

## Resources

- [errors.Is](https://pkg.go.dev/errors#Is) — matches a target through a wrap chain.
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusNotFound`, `StatusConflict`, `StatusUnprocessableEntity`, and the rest.
- [Go blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — `%w` wrapping and `errors.Is`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-order-state-machine.md](06-order-state-machine.md) | Next: [08-slug-validator.md](08-slug-validator.md)
