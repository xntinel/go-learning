# Exercise 6: A Request Validator Written as Flat Guard Clauses

Every backend endpoint runs the same shape of code at the top of its handler:
validate the request before touching the domain. Written well, it is a flat run of
early-return guard clauses, each checking one field and returning on the first
failure. Written badly, it is a pyramid of nested conditionals that hides which
condition failed. This exercise builds the flat version for a create-user request.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
reqvalidate/                 independent module: example.com/reqvalidate
  go.mod                     go 1.26
  validate.go                CreateUserRequest; ValidateCreateUser guard clauses
  cmd/
    demo/
      main.go                runnable demo: one valid, one invalid request
  validate_test.go           per-field failure + happy path + short-circuit order
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `ValidateCreateUser(req CreateUserRequest) error` checking name non-empty (and bounded length), email parseable, and age in range, as early-return guard clauses.
- Test: one passing case returning nil; one failing case per field whose message names the field; a short-circuit test proving the first failure wins when two fields are invalid.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/10-error-handling/01-error-interface-and-basic-patterns/06-request-validation-guard-clauses/cmd/demo
cd go-solutions/10-error-handling/01-error-interface-and-basic-patterns/06-request-validation-guard-clauses
```

### Why flat guards beat a nested pyramid

Guard clauses invert the nesting. Instead of "if this is ok, and if that is ok,
and if the other is ok, then succeed", you write "if this is broken, return; if
that is broken, return; ... fall through to success". The happy path ends up at the
bottom, unindented, and every failure is a one-line exit at the point it is
detected. Three properties fall out of that shape. First, the reader sees each
requirement as an isolated statement rather than tracing a branch tree. Second, the
failure order is explicit and total: whichever guard comes first wins, and there is
never ambiguity about which of two simultaneous problems is reported. Third,
short-circuiting is free — the function stops at the first failure and never
evaluates later, more expensive checks (parsing the email, counting runes) on input
already known to be bad.

The checks use real stdlib rather than hand-rolled equivalents.
`strings.TrimSpace` plus an emptiness check rejects a name that is blank or all
whitespace. `utf8.RuneCountInString` bounds the length in *runes*, not bytes, so a
name of accented or non-Latin characters is measured correctly (a byte count would
reject a valid short name written in a multibyte script). `net/mail.ParseAddress`
is the honest email check: it parses the address per RFC 5322 rather than matching
a fragile regex, and returns a descriptive error we wrap. The age range is a plain
comparison. Each guard's message names the field so the caller — and the API
consumer receiving a 400 — knows exactly what to fix.

The short-circuit order is a deliberate design decision, not an accident: name is
checked before email, so a request that is bad in both reports the name problem.
The test pins this so a later refactor cannot silently reorder the guards.

Create `validate.go`:

```go
package reqvalidate

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"
)

// CreateUserRequest is the inbound payload a create-user handler validates.
type CreateUserRequest struct {
	Name  string
	Email string
	Age   int
}

// ValidateCreateUser checks the request as flat guard clauses, returning on the
// first failure with an error that names the offending field, or nil if valid.
func ValidateCreateUser(req CreateUserRequest) error {
	if strings.TrimSpace(req.Name) == "" {
		return errors.New("name is required")
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		return fmt.Errorf("name too long: %d runes (max 100)", utf8.RuneCountInString(req.Name))
	}
	if _, err := mail.ParseAddress(req.Email); err != nil {
		return fmt.Errorf("invalid email %q: %w", req.Email, err)
	}
	if req.Age < 18 || req.Age > 150 {
		return fmt.Errorf("age %d out of range 18-150", req.Age)
	}
	return nil
}
```

### The runnable demo

The demo validates one good request and one that fails on email, printing the
outcome of each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/reqvalidate"
)

func main() {
	good := reqvalidate.CreateUserRequest{Name: "Alice", Email: "alice@example.com", Age: 30}
	if err := reqvalidate.ValidateCreateUser(good); err != nil {
		fmt.Println("unexpected:", err)
	} else {
		fmt.Println("valid request accepted")
	}

	bad := reqvalidate.CreateUserRequest{Name: "Bob", Email: "not-an-email", Age: 40}
	if err := reqvalidate.ValidateCreateUser(bad); err != nil {
		fmt.Println("rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid request accepted
rejected: invalid email "not-an-email": mail: missing '@' or angle-addr
```

### Tests

The table covers the happy path and one failure per field, asserting each error
names its field. The short-circuit test feeds a request invalid in two fields
(empty name and bad email) and asserts the *name* error wins, pinning the guard
order.

Create `validate_test.go`:

```go
package reqvalidate

import (
	"strings"
	"testing"
)

func TestValidateCreateUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     CreateUserRequest
		wantErr bool
		wantSub string
	}{
		{"valid", CreateUserRequest{Name: "Alice", Email: "alice@example.com", Age: 30}, false, ""},
		{"empty name", CreateUserRequest{Name: "  ", Email: "a@b.com", Age: 30}, true, "name"},
		{"long name", CreateUserRequest{Name: strings.Repeat("x", 101), Email: "a@b.com", Age: 30}, true, "too long"},
		{"bad email", CreateUserRequest{Name: "Alice", Email: "nope", Age: 30}, true, "email"},
		{"young age", CreateUserRequest{Name: "Alice", Email: "a@b.com", Age: 10}, true, "age"},
		{"old age", CreateUserRequest{Name: "Alice", Email: "a@b.com", Age: 200}, true, "age"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateCreateUser(tt.req)
			if tt.wantErr && err == nil {
				t.Fatalf("ValidateCreateUser(%+v) = nil, want error", tt.req)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidateCreateUser(%+v) = %v, want nil", tt.req, err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("err = %q, want it to contain %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestShortCircuitsOnFirstFailure feeds a request bad in two fields and asserts
// the first guard (name) wins, pinning the evaluation order.
func TestShortCircuitsOnFirstFailure(t *testing.T) {
	t.Parallel()

	err := ValidateCreateUser(CreateUserRequest{Name: "", Email: "also-bad", Age: 30})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Fatalf("err = %q, want the name failure to win", err.Error())
	}
	if strings.Contains(err.Error(), "email") {
		t.Fatalf("err = %q, later email guard should not have run", err.Error())
	}
}
```

## Review

The validator is correct when the happy path is the only branch returning nil and
each guard reports its own field. The short-circuit test is the one that captures
the guard-clause discipline: because name is checked first, a doubly-invalid
request reports the name, and reordering the guards would flip that. Keeping the
success path flat and unindented at the bottom is what makes the whole function
scannable.

The mistakes to avoid: nesting the checks into an `if ok { if ok2 { ... } }`
pyramid that hides which condition failed and obscures order; and matching email
with a hand-rolled regex instead of `net/mail.ParseAddress`, which is both wrong on
edge cases and reinvents stdlib. Wrap the parse error with `%w` so the underlying
`mail` error stays available to a caller that wants it.

## Resources

- [pkg.go.dev: net/mail.ParseAddress](https://pkg.go.dev/net/mail#ParseAddress) — RFC 5322 address parsing instead of a regex.
- [pkg.go.dev: unicode/utf8.RuneCountInString](https://pkg.go.dev/unicode/utf8#RuneCountInString) — counting runes, not bytes, for length limits.
- [Go Code Review Comments: Indent Error Flow](https://go.dev/wiki/CodeReviewComments#indent-error-flow) — keep the happy path un-indented; handle errors first.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-validated-client-constructor.md](05-validated-client-constructor.md) | Next: [07-domain-error-string-conventions.md](07-domain-error-string-conventions.md)
