# Exercise 1: Map a Domain Error Tree to HTTP Responses with errors.AsType

The single most common place a boundary translator lives is an HTTP handler that
must turn whatever error bubbled up from the domain into a status code and a
problem+json body. This exercise builds that mapper with `errors.AsType`, so each
case is one line that both classifies the error and reads the fields it needs.

This module is fully self-contained: it declares its own module, defines the
domain error types, the mapper, a demo, and tests. Nothing here imports any other
exercise.

## What you'll build

```text
httperr/                   independent module: example.com/httperr
  go.mod                   go 1.26 (errors.AsType needs it)
  httperr.go               domain error types + Problem + ToHTTP mapper
  cmd/
    demo/
      main.go              wraps a domain error tree, prints status + problem JSON
  httperr_test.go          table-driven trees, if-chain + tree-order tests, Example
```

Files: `httperr.go`, `cmd/demo/main.go`, `httperr_test.go`.
Implement: four domain error types (`ValidationError`, `NotFoundError`, `ConflictError`, `RateLimitError`), an RFC 9457 `Problem`, and `ToHTTP(err error) (int, Problem)` that matches each type with `errors.AsType` and reads its fields.
Test: table-driven wrapped trees asserting status and detail, an if-chain ordering test, an `errors.AsType` depth-first tree-order test over a shared interface, and an `Example` with `// Output:`.
Verify: `go test -count=1 -race ./...`

Set up the module. `errors.AsType` requires Go 1.26, so pin the language version:

```bash
mkdir -p ~/go-exercises/httperr/cmd/demo
cd ~/go-exercises/httperr
go mod init example.com/httperr
go mod edit -go=1.26
```

### Why the mapper is the natural home for AsType

The error a handler receives is a tree. A `sql: no rows in result set` gets
wrapped by the repository into a `*NotFoundError`, wrapped by the service with
`fmt.Errorf("load account %s: %w", id, err)`, and wrapped again by the transport
layer. The handler must not care about that shape; it must ask two things of each
node it recognizes: *what status does this map to*, and *what fields do I put in
the body*. `errors.AsType` answers both at once. `errors.AsType[*NotFoundError](err)`
walks the tree, and on a hit returns the concrete `*NotFoundError` — so the very
next line reads `nf.Kind` and `nf.ID` with no second assertion. With the old
`errors.As` you would declare `var nf *NotFoundError`, pass `&nf`, and only then
read its fields; the value-returning form removes that ceremony from every case.

Two design decisions are worth stating. First, order matters — but the order that
matters here is the if-chain, not the tree. `ToHTTP` tries the most specific
mappings first and falls through to a generic 500. Each `errors.AsType` call
matches a single concrete type, and a well-formed tree holds at most one error of
each type, so when a tree could satisfy two cases the winner is fixed by which
`if` runs first — the mapper's ordering, not depth-first tree traversal.
Swapping the children of the `errors.Join` in `TestIfChainOrderWins` does not
change the result, which is exactly what that test pins down. (AsType's own
first-match-in-tree-order semantics only become observable when one behavioral
interface matches two errors at different depths; `TestAsTypeTreeOrderFirstMatch`
isolates that separate property.)
Second, the unknown case never leaks internals: an unrecognized error becomes a
500 with a fixed `"an internal error occurred"` detail, not the raw error string,
so a `connection reset by peer` from deep in the stack never reaches the client.

The body type is a subset of RFC 9457 problem details (`type`, `title`, `status`,
`detail`). `RateLimitError` also carries `RetryAfterSeconds`, which the mapper
copies into a non-serialized `RetryAfter` field so the caller can set the
`Retry-After` header — a concrete example of payoff (1): the typed value hands you
a domain field that shapes the transport response.

Create `httperr.go`:

```go
// Package httperr translates a wrapped domain error tree into transport
// concerns: an RFC 9457 problem detail and an HTTP status code.
package httperr

import (
	"errors"
	"fmt"
	"net/http"
)

// ValidationError reports a single invalid input field.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation: field %q: %s", e.Field, e.Reason)
}

// NotFoundError reports that a resource of Kind with ID does not exist.
type NotFoundError struct {
	Kind string
	ID   string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("not found: %s %q", e.Kind, e.ID)
}

// ConflictError reports that an operation lost an optimistic-concurrency race.
type ConflictError struct {
	Resource string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflict: %s already exists", e.Resource)
}

// RateLimitError reports that an upstream or the caller exceeded a quota. It
// carries the number of seconds after which a retry may succeed.
type RateLimitError struct {
	RetryAfterSeconds int
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("rate limited: retry after %ds", e.RetryAfterSeconds)
}

// Problem is an RFC 9457 problem-details body. RetryAfter is non-zero only for
// 429 responses and is meant to be echoed into the Retry-After header.
type Problem struct {
	Type       string `json:"type"`
	Title      string `json:"title"`
	Status     int    `json:"status"`
	Detail     string `json:"detail"`
	RetryAfter int    `json:"-"`
}

// ToHTTP maps err's tree to a status code and problem detail. It inspects the
// tree with errors.AsType, most specific type first, so it can read each typed
// error's fields directly. Unrecognized trees collapse to 500 with a generic
// detail that does not leak internals.
func ToHTTP(err error) (int, Problem) {
	if err == nil {
		return http.StatusOK, Problem{Status: http.StatusOK, Title: "OK"}
	}

	if ve, ok := errors.AsType[*ValidationError](err); ok {
		return http.StatusBadRequest, Problem{
			Type:   "https://errors.example.com/validation",
			Title:  http.StatusText(http.StatusBadRequest),
			Status: http.StatusBadRequest,
			Detail: fmt.Sprintf("field %q is invalid: %s", ve.Field, ve.Reason),
		}
	}

	if nf, ok := errors.AsType[*NotFoundError](err); ok {
		return http.StatusNotFound, Problem{
			Type:   "https://errors.example.com/not-found",
			Title:  http.StatusText(http.StatusNotFound),
			Status: http.StatusNotFound,
			Detail: fmt.Sprintf("%s %q does not exist", nf.Kind, nf.ID),
		}
	}

	if ce, ok := errors.AsType[*ConflictError](err); ok {
		return http.StatusConflict, Problem{
			Type:   "https://errors.example.com/conflict",
			Title:  http.StatusText(http.StatusConflict),
			Status: http.StatusConflict,
			Detail: fmt.Sprintf("%s already exists", ce.Resource),
		}
	}

	if rl, ok := errors.AsType[*RateLimitError](err); ok {
		return http.StatusTooManyRequests, Problem{
			Type:       "https://errors.example.com/rate-limit",
			Title:      http.StatusText(http.StatusTooManyRequests),
			Status:     http.StatusTooManyRequests,
			Detail:     fmt.Sprintf("rate limit exceeded; retry after %ds", rl.RetryAfterSeconds),
			RetryAfter: rl.RetryAfterSeconds,
		}
	}

	return http.StatusInternalServerError, Problem{
		Type:   "about:blank",
		Title:  http.StatusText(http.StatusInternalServerError),
		Status: http.StatusInternalServerError,
		Detail: "an internal error occurred",
	}
}
```

### The runnable demo

The demo builds a not-found tree the way it would arrive at a handler — repository
error, wrapped by a service, wrapped by the transport layer — maps it, and prints
the status and the serialized problem body. It then maps a rate-limit tree to
show the `RetryAfter` field surviving the translation into a header value.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"example.com/httperr"
)

func main() {
	// A wrapped tree as it would arrive at an HTTP handler: a repository
	// not-found, wrapped by a service, wrapped by the transport layer.
	repoErr := &httperr.NotFoundError{Kind: "account", ID: "acct_42"}
	svcErr := fmt.Errorf("load account acct_42: %w", repoErr)
	edgeErr := fmt.Errorf("GET /accounts/acct_42: %w", svcErr)

	status, problem := httperr.ToHTTP(edgeErr)
	fmt.Printf("status: %d\n", status)

	body, _ := json.MarshalIndent(problem, "", "  ")
	os.Stdout.Write(body)
	fmt.Println()

	// A rate-limit tree carries a field the handler echoes into a header.
	rl := fmt.Errorf("call billing: %w", &httperr.RateLimitError{RetryAfterSeconds: 30})
	rlStatus, rlProblem := httperr.ToHTTP(rl)
	fmt.Printf("status: %d Retry-After: %d\n", rlStatus, rlProblem.RetryAfter)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
status: 404
{
  "type": "https://errors.example.com/not-found",
  "title": "Not Found",
  "status": 404,
  "detail": "account \"acct_42\" does not exist"
}
status: 429 Retry-After: 30
```

### Tests

The table drives wrapped trees of each type through `ToHTTP` and asserts both the
status and the detail string, proving the mapper reaches typed errors buried
under two layers of `fmt.Errorf`. The unknown-error row confirms the 500 fallthrough
does not echo the raw message. `TestIfChainOrderWins` builds an `errors.Join` of a
validation error and a not-found error and asserts 400 — and asserts it stays 400
when the two children are swapped — because the outcome is fixed by the order of
the `if` blocks in `ToHTTP` (it tries `*ValidationError` before `*NotFoundError`),
not by tree traversal. `TestAsTypeTreeOrderFirstMatch` isolates the other,
genuinely tree-ordered property: when a single behavioral interface matches two
errors at different depths, `AsType` returns the shallower (first-visited) one.
The `Example` locks the primary not-found mapping to an exact line.

Create `httperr_test.go`:

```go
package httperr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestToHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantDetail string
	}{
		{
			name:       "validation deep in tree",
			err:        fmt.Errorf("create user: %w", fmt.Errorf("decode body: %w", &ValidationError{Field: "email", Reason: "not an address"})),
			wantStatus: http.StatusBadRequest,
			wantDetail: `field "email" is invalid: not an address`,
		},
		{
			name:       "not found wrapped twice",
			err:        fmt.Errorf("edge: %w", fmt.Errorf("svc: %w", &NotFoundError{Kind: "account", ID: "acct_42"})),
			wantStatus: http.StatusNotFound,
			wantDetail: `account "acct_42" does not exist`,
		},
		{
			name:       "conflict",
			err:        fmt.Errorf("insert: %w", &ConflictError{Resource: "user email"}),
			wantStatus: http.StatusConflict,
			wantDetail: "user email already exists",
		},
		{
			name:       "rate limit reads RetryAfter field",
			err:        &RateLimitError{RetryAfterSeconds: 15},
			wantStatus: http.StatusTooManyRequests,
			wantDetail: "rate limit exceeded; retry after 15s",
		},
		{
			name:       "unknown collapses to 500 without leaking",
			err:        errors.New("connection reset by peer"),
			wantStatus: http.StatusInternalServerError,
			wantDetail: "an internal error occurred",
		},
		{
			name:       "nil is 200",
			err:        nil,
			wantStatus: http.StatusOK,
			wantDetail: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotStatus, gotProblem := ToHTTP(tc.err)
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %d, want %d", gotStatus, tc.wantStatus)
			}
			if gotProblem.Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", gotProblem.Detail, tc.wantDetail)
			}
		})
	}
}

// TestIfChainOrderWins shows that when one tree satisfies two cases, the winner
// is decided by ToHTTP's if-chain order, NOT by tree traversal order. ToHTTP
// tries *ValidationError before *NotFoundError, so the result is 400 no matter
// which order the Join children appear in — both sub-tests prove it.
func TestIfChainOrderWins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		joined error
	}{
		{
			name:   "validation first in Join",
			joined: errors.Join(&ValidationError{Field: "id", Reason: "empty"}, &NotFoundError{Kind: "account", ID: ""}),
		},
		{
			name:   "not-found first in Join",
			joined: errors.Join(&NotFoundError{Kind: "account", ID: ""}, &ValidationError{Field: "id", Reason: "empty"}),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, _ := ToHTTP(tc.joined)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (if-chain tries *ValidationError first)", status, http.StatusBadRequest)
			}
		})
	}
}

// leveled is a behavioral interface: it embeds error, so it satisfies AsType's
// [E error] constraint, and more than one error in a tree can implement it. That
// is the only situation in which AsType's tree-order first-match rule is
// observable — a plain concrete type appears at most once per tree.
type leveled interface {
	error
	Severity() string
}

// tierError implements leveled and wraps another error, so a chain of tierErrors
// places multiple interface matches at different depths of one tree.
type tierError struct {
	tier string
	err  error
}

func (e *tierError) Error() string    { return e.tier + ": " + e.err.Error() }
func (e *tierError) Severity() string { return e.tier }
func (e *tierError) Unwrap() error    { return e.err }

// TestAsTypeTreeOrderFirstMatch demonstrates the genuinely tree-ordered property
// that ToHTTP's concrete-type if-chain never exercises: when two errors at
// different depths both satisfy the same interface, AsType walks the tree
// depth-first from the root and returns the first (shallowest) match.
func TestAsTypeTreeOrderFirstMatch(t *testing.T) {
	t.Parallel()
	inner := &tierError{tier: "inner", err: errors.New("root cause")}
	outer := &tierError{tier: "outer", err: inner}

	got, ok := errors.AsType[leveled](outer)
	if !ok {
		t.Fatal("AsType[leveled] found no match in a tree of leveled errors")
	}
	if got.Severity() != "outer" {
		t.Errorf("Severity() = %q, want %q (AsType returns the shallowest match first)", got.Severity(), "outer")
	}
}

func ExampleToHTTP() {
	err := fmt.Errorf("handler: %w", &NotFoundError{Kind: "order", ID: "o_9"})
	status, problem := ToHTTP(err)
	fmt.Println(status, problem.Detail)
	// Output: 404 order "o_9" does not exist
}
```

## Review

The mapper is correct when every recognized type resolves to its status
regardless of how deep it sits in the tree, and when nothing unrecognized reaches
the client verbatim. The most likely mistake is a pointer/value slip in a type
argument: these error structs use pointer receivers and are wrapped as pointers,
so the argument must be `*NotFoundError`, not `NotFoundError` — the latter is a
compile error here because the value type does not satisfy `error`. A subtler
trap is case ordering: if you reorder the `if` blocks, a tree that could match two
types resolves to whichever you test first, which is exactly what
`TestIfChainOrderWins` guards. Confirm correctness with `go test -race ./...`;
the `Example` and the table together prove both the status mapping and the field
extraction, and the demo's JSON body shows the problem detail a client would see.

## Resources

- [errors package (AsType, As, Is)](https://pkg.go.dev/errors) — the exact signatures and tree-traversal semantics.
- [errors.Join](https://pkg.go.dev/errors#Join) — how multi-error trees are built and traversed depth-first.
- [RFC 9457: Problem Details for HTTP APIs](https://www.rfc-editor.org/rfc/rfc9457) — the `type`/`title`/`status`/`detail` body this mapper emits.
- [net/http status constants](https://pkg.go.dev/net/http#pkg-constants) — `StatusText` and the numeric codes used here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retry-classification-interface-match.md](02-retry-classification-interface-match.md)
