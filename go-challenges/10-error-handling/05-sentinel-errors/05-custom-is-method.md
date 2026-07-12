# Exercise 5: A Sentinel Family Via A Custom Is Method

Sometimes one sentinel should stand for a whole *class* of failures — "any 4xx",
"any 5xx" — without callers enumerating every code. This exercise builds a typed
`APIError` that implements `Is(target error) bool` so `errors.Is(err, ErrClientError)`
matches any 4xx and `errors.Is(err, ErrServerError)` matches any 5xx, following
the documented rule that a custom `Is` must be a shallow comparison and must not
call `Unwrap`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
apierr/                       independent module: example.com/apierr
  go.mod                      go 1.26
  apierr.go                   ErrClientError/ErrServerError; APIError with Error and Is methods
  cmd/
    demo/
      main.go                 classifies a slice of API errors by category
  apierr_test.go              code table (4xx/5xx), through-wrap match, unrelated-sentinel
```

- Files: `apierr.go`, `cmd/demo/main.go`, `apierr_test.go`.
- Implement: `APIError{Code, Message}` with `Error()` and `Is(target error) bool` where `Is` maps 4xx to `ErrClientError` and 5xx to `ErrServerError`, comparing shallowly.
- Test: a table over 400/404/409/500/503 asserting client/server membership, a `%w`-wrapped match, and a non-match against an unrelated sentinel.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/05-sentinel-errors/05-custom-is-method/cmd/demo
cd go-solutions/10-error-handling/05-sentinel-errors/05-custom-is-method
```

### One category sentinel, many members

Without a custom `Is`, a caller who wanted to know "is this any client error?"
would have to write `code == 400 || code == 404 || code == 409 || ...`, which is
both incomplete and re-invented at every call site. The category sentinel solves
this: `ErrClientError` and `ErrServerError` are ordinary `errors.New` values that
carry no code themselves, and the `APIError` type decides whether it belongs to a
category by implementing `Is`.

How `errors.Is(err, ErrClientError)` resolves is the mechanism to understand.
`errors.Is` walks the chain, and at each error it first checks `err == target`,
then, if `err` has an `Is(error) bool` method, calls `err.Is(target)`. Our
`APIError.Is` receives the target sentinel and answers based on its own code:
`return e.Code >= 400 && e.Code < 500` for `ErrClientError`. The documented
contract is strict about two things. First, the method must compare *shallowly* —
it looks only at `e` and the target, never recursing into what `e` wraps, because
`errors.Is` already does the walking; a custom `Is` that calls `Unwrap` would
double-traverse and can report wrong matches. Second, it should return `false`
for any target it does not recognize, so it composes cleanly with other matchers
in the chain.

Because the framework walks the chain, category matching survives wrapping for
free: `fmt.Errorf("handling: %w", &APIError{Code: 404})` still answers
`errors.Is(err, ErrClientError) == true`, because `errors.Is` unwraps to the
`APIError` and calls its `Is`.

Create `apierr.go`:

```go
package apierr

import (
	"errors"
	"fmt"
)

// Category sentinels. They carry no code; membership is decided by APIError.Is.
var (
	ErrClientError = errors.New("client error")
	ErrServerError = errors.New("server error")
)

// APIError is a typed error carrying an HTTP-style status code.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api: %d %s", e.Code, e.Message)
}

// Is implements category matching for errors.Is. It compares SHALLOWLY against
// the two category sentinels and must NOT call Unwrap: errors.Is already walks
// the chain and invokes this method at each link. Any other target is not ours.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrClientError:
		return e.Code >= 400 && e.Code < 500
	case ErrServerError:
		return e.Code >= 500 && e.Code < 600
	}
	return false
}
```

### The runnable demo

The demo classifies a mixed slice of API errors — one bare, one wrapped — into
"client" (do not retry) and "server" (retry) using only the category sentinels.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/apierr"
)

func main() {
	errs := []error{
		&apierr.APIError{Code: 400, Message: "bad json"},
		fmt.Errorf("db: %w", &apierr.APIError{Code: 503, Message: "unavailable"}),
	}
	for _, err := range errs {
		switch {
		case errors.Is(err, apierr.ErrClientError):
			fmt.Printf("client (retry=no): %v\n", err)
		case errors.Is(err, apierr.ErrServerError):
			fmt.Printf("server (retry=yes): %v\n", err)
		default:
			fmt.Printf("unknown: %v\n", err)
		}
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
client (retry=no): api: 400 bad json
server (retry=yes): db: api: 503 unavailable
```

### Tests

The table walks representative codes and asserts each lands in exactly one
category. `TestMatchesThroughWrap` proves the category survives `%w`, and
`TestUnrelatedSentinel` proves `Is` returns `false` for a sentinel it does not
own.

Create `apierr_test.go`:

```go
package apierr

import (
	"errors"
	"fmt"
	"testing"
)

func TestCategoryMatching(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code   int
		client bool
		server bool
	}{
		{400, true, false},
		{404, true, false},
		{409, true, false},
		{500, false, true},
		{503, false, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("code_%d", tt.code), func(t *testing.T) {
			t.Parallel()
			err := &APIError{Code: tt.code, Message: "x"}
			if got := errors.Is(err, ErrClientError); got != tt.client {
				t.Fatalf("Is(ErrClientError) = %v, want %v", got, tt.client)
			}
			if got := errors.Is(err, ErrServerError); got != tt.server {
				t.Fatalf("Is(ErrServerError) = %v, want %v", got, tt.server)
			}
		})
	}
}

func TestMatchesThroughWrap(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("handling request: %w", &APIError{Code: 404, Message: "not found"})
	if !errors.Is(err, ErrClientError) {
		t.Fatal("category match must survive %w wrapping")
	}
	if errors.Is(err, ErrServerError) {
		t.Fatal("404 must not match ErrServerError")
	}
}

func TestUnrelatedSentinel(t *testing.T) {
	t.Parallel()

	other := errors.New("unrelated")
	err := &APIError{Code: 500, Message: "boom"}
	if errors.Is(err, other) {
		t.Fatal("Is must return false for a sentinel it does not own")
	}
}

func ExampleAPIError_Is() {
	err := &APIError{Code: 404, Message: "missing"}
	fmt.Println(errors.Is(err, ErrClientError))
	fmt.Println(errors.Is(err, ErrServerError))
	// Output:
	// true
	// false
}
```

## Review

The type is correct when a single `errors.Is(err, ErrClientError)` classifies any
4xx and a single `errors.Is(err, ErrServerError)` any 5xx, with the match
surviving `%w` because the framework walks the chain and calls `Is` at the
`APIError` link. The one rule not to break is the shallow-comparison contract: an
`Is` method that recurses into `Unwrap` double-traverses the tree and can match
things it should not — let `errors.Is` do the walking and answer only about
`this` error and the target. Return `false` for unrecognized targets so the
method composes with other matchers.

## Resources

- [`errors.Is`](https://pkg.go.dev/errors#Is) — documents the `Is(error) bool` method contract and the chain walk.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — custom `Is` methods and error categories.
- [`net/http` status constants](https://pkg.go.dev/net/http#pkg-constants) — the 4xx/5xx code ranges the categories model.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-classify-retryable-vs-terminal.md](04-classify-retryable-vs-terminal.md) | Next: [06-context-cancellation-sentinels.md](06-context-cancellation-sentinels.md)
