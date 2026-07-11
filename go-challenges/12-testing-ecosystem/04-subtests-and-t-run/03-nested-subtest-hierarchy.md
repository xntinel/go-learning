# Exercise 3: Grouping Cases with Nested t.Run

Real validation suites have structure: happy-path cases, per-rule rejection
cases, boundary cases. Nesting `t.Run` mirrors that structure into the test names,
so the tree you read in the output matches the tree in your head — and each level
becomes a `-run` target. This exercise builds a signup-DTO validation pipeline and
tests it with two-level nesting, pins the slash-joined name with `t.Name()`, and
shows the `#01` suffix Go appends to duplicate sibling names.

This module is fully self-contained: its own `go mod init`, validator, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
signupdto/                  independent module: example.com/signupdto
  go.mod                    go 1.26
  signup.go                 type Signup; func Validate(Signup) error; sentinel errors
  cmd/
    demo/
      main.go               runnable demo: validate signups
  signup_test.go            two-level t.Run nesting; t.Name() assertions; duplicate-name #01
```

- Files: `signup.go`, `cmd/demo/main.go`, `signup_test.go`.
- Implement: `Validate(Signup) error` enforcing username, password length, and
  email shape with sentinel errors.
- Test: `t.Run("valid", …)` and `t.Run("invalid", …)` parents each with children;
  assert `t.Name()` yields the slash path; include a duplicate name to see `#01`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/signupdto/cmd/demo
cd ~/go-exercises/signupdto
go mod init example.com/signupdto
```

### How nesting maps to names and filters

Each `t.Run` pushes a segment onto the subtest name. A child of
`t.Run("invalid", …)` that is itself `t.Run("short_password", …)` has the full
name `TestValidate/invalid/short_password`. `t.Name()` returns that exact string
at runtime, which is how a subtest can log its position or (rarely) branch on it.
Because the name is built by joining segments with `/`, it lines up one-to-one with
the `/`-split regexp that `-run` uses: `-run 'TestValidate/invalid'` runs every
child of the `invalid` group and nothing under `valid`, and
`-run 'TestValidate/invalid/short_password$'` runs exactly the one leaf.

Two sibling subtests with the *same* name would be ambiguous, so the runner
disambiguates: the first keeps the name, the second becomes `name#01`, the third
`name#02`. This is a smell, not a feature — a `#01` in your output means you have
two cases you cannot address independently with `-run`. The test below creates a
duplicate on purpose so you can *see* the suffix, then explains why you should
avoid it. The validator itself follows the sentinel-error idiom so the `invalid`
children can assert a specific rule with `errors.Is`.

Create `signup.go`:

```go
package signupdto

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var (
	ErrUsernameRequired = errors.New("username is required")
	ErrPasswordTooShort = errors.New("password must be at least 8 characters")
	ErrEmailMalformed   = errors.New("email is malformed")
)

// Signup is the inbound account-creation DTO.
type Signup struct {
	Username string
	Password string
	Email    string
}

// Validate enforces the signup rules in a fixed order, returning the first
// violation wrapped with %w for errors.Is matching.
func Validate(s Signup) error {
	if strings.TrimSpace(s.Username) == "" {
		return fmt.Errorf("username: %w", ErrUsernameRequired)
	}
	if utf8.RuneCountInString(s.Password) < 8 {
		return fmt.Errorf("password: %w", ErrPasswordTooShort)
	}
	at := strings.IndexByte(s.Email, '@')
	if at <= 0 || at == len(s.Email)-1 {
		return fmt.Errorf("email %q: %w", s.Email, ErrEmailMalformed)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/signupdto"
)

func main() {
	inputs := []signupdto.Signup{
		{Username: "ada", Password: "correcthorse", Email: "ada@example.com"},
		{Username: "bo", Password: "short", Email: "bo@example.com"},
		{Username: "cy", Password: "correcthorse", Email: "cy-at-example"},
	}
	for _, s := range inputs {
		if err := signupdto.Validate(s); err != nil {
			fmt.Printf("%s: %v\n", s.Username, err)
		} else {
			fmt.Printf("%s: accepted\n", s.Username)
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
ada: accepted
bo: password: password must be at least 8 characters
cy: email "cy-at-example": email is malformed
```

### Tests

`TestValidate` nests two levels: `valid` and `invalid` groups, each with named
leaf children. Each leaf asserts `t.Name()` equals its expected slash path, so the
nesting-to-name mapping is itself under test. `TestDuplicateNames` deliberately
runs two subtests with the same name to demonstrate the `#01` suffix; both pass,
but their names differ. The cases run serially so the `#01` assignment (first
keeps the name, second gets `#01`) is deterministic.

Create `signup_test.go`:

```go
package signupdto

import (
	"errors"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		cases := []struct {
			name string
			in   Signup
		}{
			{"standard", Signup{Username: "ada", Password: "correcthorse", Email: "ada@example.com"}},
			{"long_password", Signup{Username: "bo", Password: "correcthorsebatterystaple", Email: "bo@x.io"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				want := "TestValidate/valid/" + tc.name
				if got := t.Name(); got != want {
					t.Fatalf("t.Name() = %q, want %q", got, want)
				}
				if err := Validate(tc.in); err != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tc.in, err)
				}
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		cases := []struct {
			name string
			in   Signup
			want error
		}{
			{"empty_username", Signup{Username: "", Password: "correcthorse", Email: "a@b.co"}, ErrUsernameRequired},
			{"short_password", Signup{Username: "ada", Password: "short", Email: "a@b.co"}, ErrPasswordTooShort},
			{"bad_email", Signup{Username: "ada", Password: "correcthorse", Email: "nope"}, ErrEmailMalformed},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				want := "TestValidate/invalid/" + tc.name
				if got := t.Name(); got != want {
					t.Fatalf("t.Name() = %q, want %q", got, want)
				}
				if err := Validate(tc.in); !errors.Is(err, tc.want) {
					t.Fatalf("Validate(%+v) = %v, want errors.Is %v", tc.in, err, tc.want)
				}
			})
		}
	})
}

func TestDuplicateNames(t *testing.T) {
	// Two siblings with the same name: the first keeps "dup", the second is
	// disambiguated to "dup#01". This is why unique names matter for -run.
	var names []string
	for range 2 {
		t.Run("dup", func(t *testing.T) {
			names = append(names, t.Name())
		})
	}
	want := []string{"TestDuplicateNames/dup", "TestDuplicateNames/dup#01"}
	if len(names) != len(want) {
		t.Fatalf("got %d subtests, want %d", len(names), len(want))
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("name[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}
```

## Review

Nesting is documentation that executes: `TestValidate/invalid/short_password` in
the output tells you exactly which rule broke without reading the test body, and
`-run 'TestValidate/invalid'` re-runs just the rejection cases. The `t.Name()`
assertions make the nesting-to-name contract explicit, and `TestDuplicateNames`
shows the `#01` suffix you never want in real code — if you see it, two of your
cases share a name and cannot be filtered apart. Keep the shared `cases` tables
outside the leaf callbacks so setup is not repeated per case, and let each leaf be
just its assertion.

## Resources

- [testing.T.Run — pkg.go.dev](https://pkg.go.dev/testing#T.Run)
- [testing.T.Name — pkg.go.dev](https://pkg.go.dev/testing#T.Name)
- [go test flags (-run) — cmd/go](https://pkg.go.dev/cmd/go#hdr-Testing_flags)

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-subtest-cleanup-isolation.md](04-subtest-cleanup-isolation.md)
