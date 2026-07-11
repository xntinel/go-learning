# Exercise 4: Mapping Domain Errors to HTTP Status Codes

A layered backend produces domain errors deep in a repository and must translate
them to HTTP status codes at the edge. That translation is a policy, and a policy
belongs in one place, expressed as one table. This module builds `statusFor(err)
int`, which maps wrapped domain errors to status codes with `errors.Is`, and tests
it with cases that wrap through several `%w` layers to prove the unwrap works.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
errstatus/                independent module: example.com/errstatus
  go.mod                  go 1.26
  status.go               domain sentinels + statusFor(err) int
  cmd/
    demo/
      main.go             maps a few wrapped errors to codes, prints them
  status_test.go          table over {name,err,wantStatus} with deep wrap chains
```

- Files: `status.go`, `cmd/demo/main.go`, `status_test.go`.
- Implement: `statusFor(err) int` mapping `ErrNotFound` to 404, `ErrConflict` to 409, `ErrValidation` to 400, `context.DeadlineExceeded` to 504, `nil` to 200, and any unknown error to 500.
- Test: a table of `{name, err, wantStatus}` including errors wrapped through multiple `fmt.Errorf("%w")` layers, and a default row asserting an unknown error maps to 500.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/errstatus/cmd/demo
cd ~/go-exercises/errstatus
go mod init example.com/errstatus
```

### Why one table is the whole policy

The reason to centralize error-to-status mapping in one function is that the
alternative — every handler translating its own errors — spreads the policy across
the codebase and lets it drift, so the same `ErrNotFound` becomes 404 in one
handler and 500 in another. `statusFor` is the single authority, and its test
table is the readable encoding of the policy: one row per domain error plus the
default. Adding a new domain error is one sentinel, one `case`, and one row.

The mechanism that makes this robust is `errors.Is` walking the `%w` chain. A
`not found` raised in the data layer does not arrive at the edge bare; it arrives
wrapped: `fmt.Errorf("get user %d: %w", id, fmt.Errorf("query users: %w",
ErrNotFound))`. `statusFor` must see through both wraps, and `errors.Is(err,
ErrNotFound)` does exactly that — it returns true if the sentinel is anywhere in
the chain. The test deliberately builds two- and three-layer wraps so a regression
that only matched the top-level error would fail.

Two edges are worth pinning explicitly. A `nil` error maps to 200, because "no
error" is a successful response, and handling it here means callers do not each
special-case nil. And an *unknown* error — one that matches none of the sentinels —
maps to 500, the safe default: an error the policy does not recognize is a server
fault, not a client fault, and leaking its message to the client would be a bug in
a real service (the status is the contract; the message is logged, not returned).
`context.DeadlineExceeded` maps to 504 Gateway Timeout because a deadline blown
inside the server is a timeout the client should see as such.

The `switch` uses the `switch { case ... }` form (a switch on no value, with
boolean cases) rather than a type switch, because `errors.Is` is a predicate, not
a type assertion — the chain may hold a sentinel value several wraps deep, which a
`switch err.(type)` would miss entirely.

Create `status.go`:

```go
package errstatus

import (
	"context"
	"errors"
	"net/http"
)

// Domain sentinels raised by the service's inner layers.
var (
	ErrNotFound   = errors.New("resource not found")
	ErrConflict   = errors.New("resource conflict")
	ErrValidation = errors.New("validation failed")
)

// statusFor maps a (possibly wrapped) domain error to an HTTP status code.
// A nil error is 200; an unrecognized error is 500.
func statusFor(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrValidation):
		return http.StatusBadRequest
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// StatusFor is the exported wrapper so cmd/demo (a separate package) can call it.
func StatusFor(err error) int { return statusFor(err) }
```

### The runnable demo

The demo maps a mix of bare, wrapped, and unknown errors and prints the resulting
codes with `http.StatusText`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"example.com/errstatus"
)

func main() {
	errs := []error{
		nil,
		errstatus.ErrNotFound,
		fmt.Errorf("repo: %w", fmt.Errorf("query: %w", errstatus.ErrNotFound)),
		errstatus.ErrConflict,
		errstatus.ErrValidation,
		context.DeadlineExceeded,
		errors.New("disk on fire"),
	}
	for _, err := range errs {
		code := errstatus.StatusFor(err)
		fmt.Printf("%d %-22s <- %v\n", code, http.StatusText(code), err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
200 OK                     <- <nil>
404 Not Found              <- resource not found
404 Not Found              <- repo: query: resource not found
409 Conflict               <- resource conflict
400 Bad Request            <- validation failed
504 Gateway Timeout        <- context deadline exceeded
500 Internal Server Error  <- disk on fire
```

### The tests

The table encodes the policy. The `deep_wrap` row wraps `ErrNotFound` twice to
prove `errors.Is` unwraps; the `unknown` row proves the default is 500; the `nil`
row proves success maps to 200. A new domain error is one row plus one `case`.

Create `status_test.go`:

```go
package errstatus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestStatusFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"nil_ok", nil, http.StatusOK},
		{"not_found", ErrNotFound, http.StatusNotFound},
		{"deep_wrap", fmt.Errorf("repo: %w", fmt.Errorf("query: %w", ErrNotFound)), http.StatusNotFound},
		{"conflict", fmt.Errorf("insert: %w", ErrConflict), http.StatusConflict},
		{"validation", fmt.Errorf("bind: %w", ErrValidation), http.StatusBadRequest},
		{"deadline", fmt.Errorf("call downstream: %w", context.DeadlineExceeded), http.StatusGatewayTimeout},
		{"unknown", errors.New("something exploded"), http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := statusFor(tc.err); got != tc.wantStatus {
				t.Fatalf("statusFor(%v) = %d, want %d", tc.err, got, tc.wantStatus)
			}
		})
	}
}
```

## Review

The mapper is correct when every domain sentinel maps to its status *through any
depth of wrapping*, and the table's `deep_wrap` row is the proof — if it fails, the
code is matching on the top-level error instead of using `errors.Is` to unwrap.
The two structural choices to keep: the boolean `switch` (not a type switch,
because the target is a wrapped value not a type), and the 500 default (an
unrecognized error is a server fault, and its message must be logged, never
returned to the client).

The `StatusFor` exported wrapper exists only so the separate `cmd/demo` package can
reach the logic; the test uses the unexported `statusFor` directly because it is in
the same package. Keeping the real logic unexported and same-package-tested, with a
thin exported shim for the demo, is a common and deliberate structure.

## Resources

- [errors.Is](https://pkg.go.dev/errors#Is) — matching a target through the unwrap chain.
- [net/http status constants and StatusText](https://pkg.go.dev/net/http#pkg-constants) — the canonical code names.
- [context package](https://pkg.go.dev/context#pkg-variables) — `DeadlineExceeded` and `Canceled` as sentinel values.

---

Back to [00-concepts.md](00-concepts.md) | Next: [05-retry-classifier.md](05-retry-classifier.md)
