# Exercise 3: Validation Rules as a Table of Valid/Invalid Inputs

A request validator is a contract with one happy path and many rejections. The
senior distinction is between asserting *that* a request was rejected and
asserting *which rule* rejected it — a validator that returns "too long" where it
should return "missing email" is broken but still returns an error. This module
builds `Validate(CreateUserRequest)` and tests it with a table that carries both a
`wantErr` bool and the expected sentinel, pinned with `errors.Is`.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
uservalidate/             independent module: example.com/uservalidate
  go.mod                  go 1.26
  validate.go             CreateUserRequest, Validate, sentinel errors
  cmd/
    demo/
      main.go             validates a few requests, prints the outcome
  validate_test.go        table over {name,input,wantErr,wantSentinel}
```

- Files: `validate.go`, `cmd/demo/main.go`, `validate_test.go`.
- Implement: `Validate(CreateUserRequest) error` checking required name, required and well-formed email, and a name length bound, returning wrapped sentinel errors.
- Test: a table of `{name, input, wantErr bool, wantSentinel error}` asserting `(err != nil) == wantErr` and, when an error is expected, `errors.Is(err, wantSentinel)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/uservalidate/cmd/demo
cd ~/go-exercises/uservalidate
go mod init example.com/uservalidate
```

### Why wantErr-bool and wantSentinel are different columns

The table has two expectation columns because they answer two questions. `wantErr
bool` answers "should this input be rejected at all" — it is enough for the happy
path (`wantErr: false`) and for a smoke test that nothing valid is spuriously
rejected. `wantSentinel error` answers "rejected *for which reason*", and it is
what keeps the validator honest as it grows. Without it, a refactor that made
every rejection return the same generic error would pass a `wantErr`-only table
while destroying the API's ability to tell a client what to fix.

The mechanism is `errors.Is` over a `%w` chain. Each rule returns a package-level
sentinel — `ErrMissingName`, `ErrMissingEmail`, `ErrEmailFormat`, `ErrNameTooLong`
— wrapped with `fmt.Errorf("field %q: %w", ...)` so the message carries context
while the sentinel stays matchable. The test asserts `errors.Is(err,
tc.wantSentinel)`, which walks the wrap chain and returns true when the sentinel
is anywhere in it. This is why you wrap with `%w` and not `%v`: `%v` flattens the
error to a string and severs the chain, so `errors.Is` would return false and the
identity would be lost.

Email format is checked with `net/mail.ParseAddress`, the standard library's RFC
5322 address parser, rather than a hand-rolled regex. Real backends reach for the
stdlib parser because email grammar is famously irregular and a regex either
rejects valid addresses or accepts invalid ones; `mail.ParseAddress` is the honest
tool. The rule order matters: check presence before format, so an empty email
reports `ErrMissingEmail` (actionable) rather than `ErrEmailFormat` (confusing).

Create `validate.go`:

```go
package uservalidate

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"
)

// Sentinel errors name each rejection so callers can branch with errors.Is.
var (
	ErrMissingName  = errors.New("name is required")
	ErrNameTooLong  = errors.New("name exceeds maximum length")
	ErrMissingEmail = errors.New("email is required")
	ErrEmailFormat  = errors.New("email is not a valid address")
)

const maxNameLen = 64

// CreateUserRequest is the inbound payload for creating a user.
type CreateUserRequest struct {
	Name  string
	Email string
}

// Validate reports the first rule the request violates, wrapping a sentinel so
// callers can identify the failure with errors.Is. A nil return means valid.
func Validate(r CreateUserRequest) error {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return fmt.Errorf("validate: %w", ErrMissingName)
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("validate: name %d chars: %w", len(name), ErrNameTooLong)
	}
	if strings.TrimSpace(r.Email) == "" {
		return fmt.Errorf("validate: %w", ErrMissingEmail)
	}
	if _, err := mail.ParseAddress(r.Email); err != nil {
		return fmt.Errorf("validate: %q: %w", r.Email, ErrEmailFormat)
	}
	return nil
}
```

### The runnable demo

The demo runs a few representative requests and prints valid/invalid with the
reason, so the sentinel messages are visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/uservalidate"
)

func main() {
	reqs := []uservalidate.CreateUserRequest{
		{Name: "Alice", Email: "alice@example.com"},
		{Name: "", Email: "bob@example.com"},
		{Name: strings.Repeat("x", 100), Email: "x@example.com"},
		{Name: "Carol", Email: ""},
		{Name: "Dave", Email: "not-an-email"},
	}
	for _, r := range reqs {
		if err := uservalidate.Validate(r); err != nil {
			fmt.Printf("invalid: %v\n", err)
		} else {
			fmt.Printf("valid:   %s <%s>\n", r.Name, r.Email)
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
valid:   Alice <alice@example.com>
invalid: validate: name is required
invalid: validate: name 100 chars: name exceeds maximum length
invalid: validate: email is required
invalid: validate: "not-an-email": email is not a valid address
```

### The tests

Each row carries a fully-built request and its expected outcome. The assertion is
two-stage: first `(err != nil) == tc.wantErr`, then, only when an error is
expected, `errors.Is(err, tc.wantSentinel)`. Splitting it this way means the happy
path (`wantSentinel: nil`) does not accidentally demand a sentinel match, and every
rejection is pinned to its exact cause.

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

	tests := []struct {
		name         string
		input        CreateUserRequest
		wantErr      bool
		wantSentinel error
	}{
		{"valid", CreateUserRequest{Name: "Alice", Email: "alice@example.com"}, false, nil},
		{"valid_with_display", CreateUserRequest{Name: "Bob", Email: "Bob <bob@example.com>"}, false, nil},
		{"missing_name", CreateUserRequest{Name: "", Email: "a@example.com"}, true, ErrMissingName},
		{"blank_name", CreateUserRequest{Name: "   ", Email: "a@example.com"}, true, ErrMissingName},
		{"name_too_long", CreateUserRequest{Name: strings.Repeat("x", 65), Email: "a@example.com"}, true, ErrNameTooLong},
		{"missing_email", CreateUserRequest{Name: "Alice", Email: ""}, true, ErrMissingEmail},
		{"bad_email", CreateUserRequest{Name: "Alice", Email: "nope"}, true, ErrEmailFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Validate(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate(%+v) error = %v, wantErr %v", tc.input, err, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, tc.wantSentinel) {
				t.Fatalf("Validate(%+v) = %v, want errors.Is %v", tc.input, err, tc.wantSentinel)
			}
		})
	}
}
```

## Review

The validator is correct when each rule returns its own sentinel wrapped with
`%w`, and the two-column table proves it. The trap is the temptation to assert only
`wantErr`: that column alone would pass even if `bad_email` returned
`ErrMissingName`, so the `wantSentinel` check with `errors.Is` is what makes the
test worth writing. If you ever switch a wrap from `%w` to `%v`, `errors.Is` breaks
and the sentinel rows fail — that failure is the test doing its job.

Rule ordering is part of the contract: presence before format, so an empty email
reports the actionable `ErrMissingEmail` rather than the vaguer `ErrEmailFormat`.
The `valid_with_display` row documents that `mail.ParseAddress` accepts a
`Name <addr>` display form, which is real RFC 5322 behavior and a decision worth
making consciously rather than discovering in production.

## Resources

- [errors package](https://pkg.go.dev/errors) — `errors.Is`, `errors.New`, and the `%w` unwrap contract.
- [net/mail.ParseAddress](https://pkg.go.dev/net/mail#ParseAddress) — the stdlib RFC 5322 address parser.
- [Go Blog: Working with Errors in Go 1.13](https://go.dev/blog/go1.13-errors) — sentinels, wrapping, and `Is`/`As`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-domain-error-to-http-status.md](04-domain-error-to-http-status.md)
