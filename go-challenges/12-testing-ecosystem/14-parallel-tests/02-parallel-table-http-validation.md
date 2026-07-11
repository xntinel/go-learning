# Exercise 2: Table-Driven Parallel Subtests for a Request Validator

Every HTTP handler that accepts a JSON body validates it before touching the
database: required fields present, email well-formed, lengths within bounds. That
validation matrix grows to dozens of cases, and running each case as its own
parallel subtest turns a slow linear sweep into a concurrent one. This module
builds a `CreateUserRequest` validator and drives it with a table of parallel
subtests, on modern Go 1.22+ loop semantics.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
uservalidate/               independent module: example.com/uservalidate
  go.mod
  validate.go               CreateUserRequest; Validate; sentinel errors
  cmd/
    demo/
      main.go               runnable demo: validate a good and a bad request
  validate_test.go          table of cases, each a t.Parallel subtest
```

Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
Implement: `Validate(CreateUserRequest) error` checking required fields, email
shape via `net/mail.ParseAddress`, and length bounds, returning sentinel errors
wrapped with `%w`.
Test: one `TestValidate` whose table cases each run as a `t.Parallel()` subtest,
asserting accept and reject via `errors.Is`.
Verify: `go test -count=1 -race -shuffle=on ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/uservalidate/cmd/demo
cd ~/go-exercises/uservalidate
go mod init example.com/uservalidate
```

### Why sentinel errors, and why email via net/mail

A validator's callers need to branch on *which* rule failed — a handler maps a
missing field to one 400 message and a bad email to another. That means the
validator must return typed, comparable errors, not opaque strings. The idiom is
package-level sentinel errors (`ErrMissingEmail`, `ErrInvalidEmail`, ...) wrapped
with `%w` via `fmt.Errorf`, so a caller can write `errors.Is(err, ErrInvalidEmail)`
and get a stable classification even when the message carries the offending value.
The test asserts with the same `errors.Is`, which is what makes the test a
contract rather than a string-match.

For the email check, do not hand-roll a regex — email grammar is famously
underestimated. `net/mail.ParseAddress` implements RFC 5322 address parsing from
the standard library; if it returns an error, or if it parses the input into an
address whose `Address` field differs from the raw input (meaning the input
carried a display name or angle brackets), the field is not a bare address and we
reject it. Reusing the stdlib parser is both more correct and less code than any
regex you would write.

### Why parallel subtests here, and the loop-variable history

A validation matrix is embarrassingly parallel: each case is independent, reads
no shared mutable state, and constructs its own request value. Marking each
subtest `t.Parallel()` lets the whole table run concurrently, which matters once
the table is large or a case does real work (a case that hits a fake upstream, a
property-based sweep). The cases here are cheap, so treat the parallelism as a
demonstration of the pattern you will lean on when they are not.

The historical trap lived exactly in this shape. Before Go 1.22, the `range` loop
reused one `tc` variable; a `t.Parallel()` subtest captured it, and by the time
the paused subtests resumed, the loop had finished and every subtest saw the
*last* row. The suite went green while testing one case dozens of times. The fix
was `tc := tc`. Go 1.22 gives each iteration its own `tc`, so the shadow is dead
noise now — but the bug is why you should never write a pre-1.22 parallel table
without it, and never add it on a modern toolchain.

Create `validate.go`:

```go
package uservalidate

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// Sentinel errors let callers classify a failure with errors.Is.
var (
	ErrMissingName  = errors.New("name is required")
	ErrNameTooLong  = errors.New("name exceeds max length")
	ErrMissingEmail = errors.New("email is required")
	ErrInvalidEmail = errors.New("email is not a valid address")
	ErrWeakPassword = errors.New("password too short")
)

const (
	maxNameLen  = 64
	minPassword = 10
)

// CreateUserRequest is the decoded body of POST /users.
type CreateUserRequest struct {
	Name     string
	Email    string
	Password string
}

// Validate reports the first rule the request violates, wrapping a sentinel so
// callers can branch with errors.Is.
func Validate(r CreateUserRequest) error {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return fmt.Errorf("validate name: %w", ErrMissingName)
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("validate name (%d chars): %w", len(name), ErrNameTooLong)
	}
	if strings.TrimSpace(r.Email) == "" {
		return fmt.Errorf("validate email: %w", ErrMissingEmail)
	}
	if !validEmail(r.Email) {
		return fmt.Errorf("validate email %q: %w", r.Email, ErrInvalidEmail)
	}
	if len(r.Password) < minPassword {
		return fmt.Errorf("validate password (%d chars): %w", len(r.Password), ErrWeakPassword)
	}
	return nil
}

// validEmail accepts only a bare RFC 5322 address (no display name, no angle
// brackets), reusing the standard library parser instead of a regex.
func validEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return false
	}
	return addr.Address == s
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/uservalidate"
)

func main() {
	good := uservalidate.CreateUserRequest{
		Name: "Ada Lovelace", Email: "ada@example.com", Password: "correct-horse",
	}
	bad := uservalidate.CreateUserRequest{
		Name: "Bad", Email: "not-an-email", Password: "correct-horse",
	}

	if err := uservalidate.Validate(good); err != nil {
		fmt.Println("good:", err)
	} else {
		fmt.Println("good: accepted")
	}
	fmt.Println("bad:", uservalidate.Validate(bad))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
good: accepted
bad: validate email "not-an-email": email is not a valid address
```

The wrapped sentinel prints as its message text after the context prefix, because
`fmt.Errorf` with `%w` renders the wrapped error's `Error()` string; the
`errors.Is` machinery still matches `ErrInvalidEmail` underneath.

### Tests

`TestValidate` is one function with a `[]struct` table. Each iteration calls
`t.Run(tc.name, ...)` and, inside, `t.Parallel()` — so the whole matrix runs
concurrently. `wantErr` is a sentinel (or `nil`); the assertion uses `errors.Is`,
which both confirms acceptance (`err == nil`) and classifies each rejection. No
`tc := tc` appears — Go 1.22+ gives each iteration its own copy.

Create `validate_test.go`:

```go
package uservalidate

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		req     CreateUserRequest
		wantErr error // nil means accept
	}{
		{"valid", CreateUserRequest{"Ada Lovelace", "ada@example.com", "correct-horse"}, nil},
		{"empty name", CreateUserRequest{"  ", "ada@example.com", "correct-horse"}, ErrMissingName},
		{"name too long", CreateUserRequest{strings.Repeat("x", 65), "ada@example.com", "correct-horse"}, ErrNameTooLong},
		{"empty email", CreateUserRequest{"Ada", "", "correct-horse"}, ErrMissingEmail},
		{"malformed email", CreateUserRequest{"Ada", "not-an-email", "correct-horse"}, ErrInvalidEmail},
		{"email with display name", CreateUserRequest{"Ada", "Ada <ada@example.com>", "correct-horse"}, ErrInvalidEmail},
		{"weak password", CreateUserRequest{"Ada", "ada@example.com", "short"}, ErrWeakPassword},
	}

	// No `tc := tc` on Go 1.22+: each iteration gets its own tc, so the paused
	// parallel subtests below observe their own case, not the last one.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.req)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate(%s) = %v, want nil", tc.name, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate(%s) = %v, want errors.Is %v", tc.name, err, tc.wantErr)
			}
		})
	}
}

func TestValidateWrapsContext(t *testing.T) {
	t.Parallel()

	err := Validate(CreateUserRequest{"Ada", "bad", "correct-horse"})
	if !strings.Contains(err.Error(), `"bad"`) {
		t.Fatalf("error %q should carry the offending value", err)
	}
	if !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("error %v should wrap ErrInvalidEmail", err)
	}
}
```

## Review

The validator is correct when every rejection wraps the right sentinel with `%w`,
so `errors.Is` classifies it, and when the email check delegates to
`net/mail.ParseAddress` rather than a regex. The `email with display name` case is
the one that catches a naive parser: `Ada <ada@example.com>` *does* parse, but its
`Address` is `ada@example.com`, not the raw input, so `validEmail` rejects it —
the handler wanted a bare address, not a full mailbox spec.

The parallel-table shape is the reusable lesson: one function, a `[]struct` table,
`t.Run` per row, `t.Parallel()` inside. Confirm determinism with
`go test -race -count=5 -shuffle=on` — because each case owns its request value
and asserts only its own outcome, order and repetition change nothing. If you saw
every subtest report the same case, you would be on a pre-1.22 toolchain missing
the `tc := tc` shadow.

## Resources

- [`net/mail.ParseAddress`](https://pkg.go.dev/net/mail#ParseAddress) — RFC 5322 single-address parsing.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.
- [Fixing For Loops in Go 1.22](https://go.dev/blog/loopvar-preview) — the per-iteration loop variable and the parallel-subtest bug it removes.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-parallel-safe-counter.md](01-parallel-safe-counter.md) | Next: [03-shared-server-fixture-teardown.md](03-shared-server-fixture-teardown.md)
