# Exercise 7: Pin a stable, non-stuttering wrapped log line for an HTTP error responder

The log line an on-call engineer reads is a product surface, and a wrapped error's
message is that surface. If every layer prefixes `failed to`, the line degrades
into `failed to handle: failed to get user: failed to query: no rows`. This
exercise builds a layered handler-service-repo error whose wrap sites add only the
operation and the salient id, produces a clean breadcrumb log line, and pins that
exact line — plus a regression guard that fails the moment any layer reintroduces
stutter.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
responder/                     independent module: example.com/responder
  go.mod                       go 1.24
  responder.go                 ErrNoRows; repoFind/serviceGetUser/HandleGetUser layered wraps; LogLine
  responder_test.go            exact breadcrumb equality; chain depth; innermost sentinel; no-stutter guard
  cmd/
    demo/
      main.go                  prints the breadcrumb log line for a missing user
```

- Files: `responder.go`, `cmd/demo/main.go`, `responder_test.go`.
- Implement: three layered functions each wrapping with `%w` and operation+id context (no `failed to`/`error:`), and a `LogLine(err)` that renders the message.
- Test: `err.Error()` equals the exact expected breadcrumb (outermost first, cause last, single-colon separators); walking `errors.Unwrap` reaches `ErrNoRows` at the expected depth; a regression test asserts `failed to: failed to` never appears.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/02-fmt-errorf-and-error-wrapping/07-error-message-hygiene-contract/cmd/demo
cd go-solutions/10-error-handling/02-fmt-errorf-and-error-wrapping/07-error-message-hygiene-contract
```

### The breadcrumb is the contract

Three layers each wrap the one below with `%w` and their own operation plus the
id. The repository names the query and id; the service names the logical operation
and id; the handler names the route. None of them prefixes `failed to` or
`error:`, and none repeats the id in a way that stutters:

- repo: `query getUserByID id=42: %w(ErrNoRows)`
- service: `get user 42: %w(repo error)`
- handler: `GET /users/42: %w(service error)`

Rendered outermost-first with single-colon separators, the full line for id `42`
is exactly:

```
GET /users/42: get user 42: query getUserByID id=42: no rows
```

That string is the artifact under test. Unlike every other module in this lesson —
where asserting on `err.Error()` would be a mistake — here the stable log line
*is* the deliverable, so the test asserts exact string equality on purpose. The
value of pinning it is that a well-meaning refactor that reintroduces `failed to`
at the service layer, or double-colons, or reorders the layers, turns the test red
immediately. A dedicated guard also asserts the substring `failed to: failed to`
never appears, catching the specific stutter regression even if the exact-equality
assertion is later relaxed.

Beneath the message, the chain is still fully inspectable: `%w` at every layer
means `errors.Is(err, ErrNoRows)` finds the sentinel, and walking `errors.Unwrap`
three times reaches it, pinning the chain depth.

Create `responder.go`:

```go
package responder

import (
	"errors"
	"fmt"
)

// ErrNoRows is the root cause sentinel at the bottom of the chain.
var ErrNoRows = errors.New("no rows")

// repoFind is the repository layer: names the query and the id, wraps the cause.
func repoFind(id string) error {
	return fmt.Errorf("query getUserByID id=%s: %w", id, ErrNoRows)
}

// serviceGetUser is the service layer: names the logical operation and the id.
func serviceGetUser(id string) error {
	if err := repoFind(id); err != nil {
		return fmt.Errorf("get user %s: %w", id, err)
	}
	return nil
}

// HandleGetUser is the handler layer: names the route.
func HandleGetUser(id string) error {
	if err := serviceGetUser(id); err != nil {
		return fmt.Errorf("GET /users/%s: %w", id, err)
	}
	return nil
}

// LogLine renders the error as the breadcrumb log string, outermost operation
// first and the root cause last.
func LogLine(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/responder"
)

func main() {
	err := responder.HandleGetUser("42")
	fmt.Println(responder.LogLine(err))
	fmt.Printf("is ErrNoRows=%v\n", errors.Is(err, responder.ErrNoRows))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
GET /users/42: get user 42: query getUserByID id=42: no rows
is ErrNoRows=true
```

### Tests

The exact-equality test is the contract. The unwrap-depth test pins that the chain
is exactly three wraps above the sentinel. The stutter guard catches the specific
`failed to: failed to` regression.

Create `responder_test.go`:

```go
package responder

import (
	"errors"
	"strings"
	"testing"
)

const wantLine = "GET /users/42: get user 42: query getUserByID id=42: no rows"

func TestLogLineIsStableBreadcrumb(t *testing.T) {
	t.Parallel()

	got := LogLine(HandleGetUser("42"))
	if got != wantLine {
		t.Fatalf("LogLine =\n  %q\nwant\n  %q", got, wantLine)
	}
}

func TestChainDepthReachesSentinel(t *testing.T) {
	t.Parallel()

	err := HandleGetUser("42")

	// Walk the single-Unwrap chain to its leaf. errors.Is would match on the
	// very first step (it traverses the whole chain), so count identity steps
	// instead: handler -> service -> repo -> ErrNoRows is three Unwrap steps.
	depth := 0
	var leaf error
	for e := errors.Unwrap(err); e != nil; e = errors.Unwrap(e) {
		depth++
		leaf = e
	}
	if depth != 3 {
		t.Fatalf("unwrap depth to leaf = %d, want 3", depth)
	}
	if leaf != ErrNoRows {
		t.Fatalf("leaf of the chain = %v, want ErrNoRows", leaf)
	}
	if !errors.Is(err, ErrNoRows) {
		t.Fatal("errors.Is must still find ErrNoRows at the bottom")
	}
}

func TestNoStutter(t *testing.T) {
	t.Parallel()

	line := LogLine(HandleGetUser("42"))
	if strings.Contains(line, "failed to: failed to") {
		t.Fatalf("log line reintroduced stutter: %q", line)
	}
	if strings.Contains(line, "error: error:") {
		t.Fatalf("log line reintroduced stutter: %q", line)
	}
}

func TestLogLineNilIsEmpty(t *testing.T) {
	t.Parallel()

	if got := LogLine(nil); got != "" {
		t.Fatalf("LogLine(nil) = %q, want empty", got)
	}
}
```

## Review

The responder is correct when the rendered line is exactly the breadcrumb and the
chain underneath still resolves to `ErrNoRows`. This is the one module where string
equality is the right assertion, because the message is the deliverable rather than
an incidental byproduct — everywhere else in this lesson, comparing `err.Error()`
is the anti-pattern. The two failure modes the tests guard are opposite in
character: a *cosmetic* regression (a layer adds `failed to`, or a stray extra
colon) is caught by exact equality and the stutter guard, while a *structural*
regression (a layer drops `%w` for `%v`) is caught by `TestChainDepthReachesSentinel`,
because a severed link would change the unwrap depth or make `errors.Is` fail. A
good wrap satisfies both at once: readable line on top, inspectable chain beneath.

## Resources

- [Go Code Review Comments: error strings](https://go.dev/wiki/CodeReviewComments#error-strings) — no capitalization, no punctuation, no `failed to`.
- [fmt.Errorf](https://pkg.go.dev/fmt#Errorf) — the `%w` wraps that build the chain.
- [errors package](https://pkg.go.dev/errors) — `Unwrap` and `Is` over the chain.
- [Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — adding context as errors propagate.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-handler-defer-wrap-named-return.md](06-handler-defer-wrap-named-return.md) | Next: [08-status-error-struct-vs-wraperror.md](08-status-error-struct-vs-wraperror.md)
