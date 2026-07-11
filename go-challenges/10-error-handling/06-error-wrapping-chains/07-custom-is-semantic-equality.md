# Exercise 7: Custom Is Method: Match API Errors by Code, Not Identity

An upstream API returns errors with a `code` field: `"rate_limited"`,
`"not_found"`, `"forbidden"`. You want callers to classify on the code —
`errors.Is(err, &APIError{Code: "rate_limited"})` — and have it match *any* wrapped
API error with that code, not one specific instance. A custom `Is(target error)
bool` method makes `errors.Is` compare by code instead of by identity. This module
builds it and shows exactly why a naive `==` cannot do the same job.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
apierr/                     independent module: example.com/apierr
  go.mod                    go 1.24
  apierr.go                 *APIError with Error, Unwrap, and a custom Is method
  cmd/
    demo/
      main.go               runnable demo: match a deeply-wrapped API error by code
  apierr_test.go            code probe matches under wrapping; wrong code does not
```

Files: `apierr.go`, `cmd/demo/main.go`, `apierr_test.go`.
Implement: an `APIError` carrying a `Code` and an `Is(target error) bool` that matches on `Code`, so `errors.Is` matches by code across wrapping layers.
Test: wrap an `APIError{Code:"rate_limited"}` under two `%w` layers; assert `errors.Is` with a code-only probe matches and a different-code probe does not; a negative test proving naive `==` would fail.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/apierr/cmd/demo
cd ~/go-exercises/apierr
go mod init example.com/apierr
go mod edit -go=1.24
```

### Why == breaks and a custom Is fixes it

`errors.Is` has two ways to match at each node in the chain: it compares with `==`,
and — if the node implements `Is(target error) bool` — it calls that method. The
`==` path only works for *sentinels*: a single package-level `var` whose pointer
identity is stable. An `APIError` is not a sentinel; it is a value carrying data,
and every call site that constructs one (or that the API layer decodes from JSON)
is a *different* value. Two `&APIError{Code: "rate_limited"}` are different pointers,
so `err == &APIError{Code: "rate_limited"}` is false even before any wrapping — and
after wrapping, `err` is the wrapper anyway, so `==` is doubly wrong.

A custom `Is` method changes the question from "is this the same object?" to "does
this mean the same thing?". Our `Is` type-asserts the target to `*APIError` and
compares `Code`. Now `errors.Is(err, &APIError{Code: "rate_limited"})` walks the
chain, reaches the real `APIError`, calls its `Is` with the probe, and matches on
the code — regardless of how deeply it was wrapped or which instance it is.

The non-negotiable convention: **`Is` must be a shallow, single-level compare and
must not call `Unwrap`**. `errors.Is` already walks the whole tree and invokes your
`Is` at *every* node; if your `Is` also unwrapped and recursed, the tree would be
walked twice, and a self-referential or cyclic chain could loop forever. So `Is`
looks only at the receiver and the target — no traversal. `APIError` still
implements `Unwrap` to expose an optional underlying cause for the *outer* walk, but
`Is` itself never touches it.

Create `apierr.go`:

```go
package apierr

import "fmt"

// APIError is a structured upstream error carrying a machine-readable Code. It is
// value-like (many instances share a Code), so it matches by Code, not identity.
type APIError struct {
	Code    string
	Message string
	cause   error
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return "api error: " + e.Code
	}
	return fmt.Sprintf("api error %s: %s", e.Code, e.Message)
}

// Unwrap exposes an optional underlying cause for the errors.Is/As tree walk.
func (e *APIError) Unwrap() error { return e.cause }

// Is reports semantic equality by Code. It is a SHALLOW compare: it must not call
// Unwrap or otherwise walk the chain — errors.Is already does that and calls Is at
// each node.
func (e *APIError) Is(target error) bool {
	t, ok := target.(*APIError)
	return ok && t.Code == e.Code
}

// Wrap attaches a cause to an APIError, building a chain the outer walk traverses.
func (e *APIError) Wrap(cause error) *APIError {
	e.cause = cause
	return e
}
```

### The runnable demo

The demo builds a rate-limit API error, wraps it under two `%w` layers (as it would
be after bubbling through a client and a service), and shows that a code-only probe
still matches while a different-code probe does not.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/apierr"
)

func main() {
	root := &apierr.APIError{Code: "rate_limited", Message: "too many requests"}
	wrapped := fmt.Errorf("service call: %w", fmt.Errorf("http client: %w", root))

	fmt.Printf("is rate_limited: %v\n", errors.Is(wrapped, &apierr.APIError{Code: "rate_limited"}))
	fmt.Printf("is not_found:    %v\n", errors.Is(wrapped, &apierr.APIError{Code: "not_found"}))
	fmt.Printf("naive == probe:  %v\n", wrapped == &apierr.APIError{Code: "rate_limited"})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
is rate_limited: true
is not_found:    false
naive == probe:  false
```

### Tests

The test wraps an `APIError{Code:"rate_limited"}` under two `%w` layers and asserts a
code-only probe matches through them, that a different code does not, and — the
documentation case — that a naive `==` against a fresh probe is false. The last
assertion is the whole justification for writing a custom `Is`: without it, callers
would have no reliable way to classify a value-typed error.

Create `apierr_test.go`:

```go
package apierr

import (
	"errors"
	"fmt"
	"testing"
)

func TestCustomIsMatchesByCode(t *testing.T) {
	t.Parallel()

	root := &APIError{Code: "rate_limited", Message: "slow down"}
	wrapped := fmt.Errorf("service: %w", fmt.Errorf("client: %w", root))

	tests := []struct {
		name  string
		probe *APIError
		want  bool
	}{
		{"same code matches through wrapping", &APIError{Code: "rate_limited"}, true},
		{"different code does not match", &APIError{Code: "not_found"}, false},
		{"code match ignores Message", &APIError{Code: "rate_limited", Message: "anything"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := errors.Is(wrapped, tt.probe); got != tt.want {
				t.Errorf("errors.Is(wrapped, %+v) = %v, want %v", tt.probe, got, tt.want)
			}
		})
	}
}

func TestNaiveEqualityFails(t *testing.T) {
	t.Parallel()
	root := &APIError{Code: "rate_limited"}
	wrapped := fmt.Errorf("service: %w", root)

	// This is why the custom Is is needed: == against a fresh probe is always false,
	// both because pointers differ and because wrapped is the wrapper, not the root.
	if wrapped == error(&APIError{Code: "rate_limited"}) {
		t.Fatal("unexpected: == matched; custom Is would be unnecessary")
	}
	if !errors.Is(wrapped, &APIError{Code: "rate_limited"}) {
		t.Fatal("errors.Is should match by code where == cannot")
	}
}

func TestIsWithUnderlyingCause(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("connection reset")
	root := (&APIError{Code: "upstream_error"}).Wrap(sentinel)

	if !errors.Is(root, sentinel) {
		t.Error("outer walk should reach the wrapped cause via Unwrap")
	}
	if !errors.Is(root, &APIError{Code: "upstream_error"}) {
		t.Error("custom Is should still match by code")
	}
}

func ExampleAPIError_Is() {
	root := &APIError{Code: "forbidden"}
	wrapped := fmt.Errorf("guard: %w", root)
	fmt.Println(errors.Is(wrapped, &APIError{Code: "forbidden"}))
	// Output: true
}
```

## Review

The custom `Is` is correct when a code-only probe matches any wrapped `APIError`
with that code and a different code never does — proven through two `%w` layers so
depth is not a factor. The `TestNaiveEqualityFails` case documents the value the
method adds: `==` against a value-typed error is structurally hopeless, so without a
custom `Is` (or promoting `Code`s to real sentinels) callers could not classify at
all. The one rule you must not break is the shallow-compare convention: `Is` looks
only at the receiver and the target and never calls `Unwrap` — the `errors` package
does the walking, and an `Is` that also walks risks a double traversal or an
infinite loop on a cyclic chain.

## Resources

- [errors.Is](https://pkg.go.dev/errors#Is) — the `==`-or-`Is(target)` matching rule and tree walk.
- [errors package](https://pkg.go.dev/errors) — the `Is`/`As` method conventions (shallow, no Unwrap).
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — custom `Is` for semantic matching.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-context-cancellation-timeout-chain.md](08-context-cancellation-timeout-chain.md)
