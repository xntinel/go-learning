# Exercise 7: Use Must-Style Constructors for Package-Init Invariants That Must Fail the Build-Run

`regexp.MustCompile` and `template.Must` panic on failure instead of returning an
error, and that is correct — but only for package-level values whose invalidity
means the binary cannot function. This exercise builds both forms of the same
constructor: `NewMatcher` that returns an error for runtime input, and
`MustCompileMatcher` that panics for a compile-time-constant pattern, and pins the
rule for when each is appropriate.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
matcher/                     independent module: example.com/must-constructor-init-invariants
  go.mod
  matcher.go                 Matcher, NewMatcher (error), MustCompileMatcher (panic), Matches, sentinel
  cmd/
    demo/
      main.go                a package-level MustCompileMatcher and a runtime NewMatcher
  matcher_test.go            NewMatcher valid/invalid, MustCompile usable/panics-via-recover
```

- Files: `matcher.go`, `cmd/demo/main.go`, `matcher_test.go`.
- Implement: `NewMatcher(pattern string) (*Matcher, error)` and `MustCompileMatcher(pattern string) *Matcher`, plus `Matches`.
- Test: `NewMatcher` returns the sentinel for an invalid pattern and matches for a valid one; `MustCompileMatcher` returns a usable matcher for a valid pattern and panics for an invalid one (asserted via `recover`); and a comment documents that `Must` takes only compile-time-constant patterns.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/matcher/cmd/demo
cd ~/go-exercises/matcher
go mod init example.com/must-constructor-init-invariants
```

### When panic-at-init is correct, and when it is a latent crash

The choice between returning an error and panicking is a statement about where the
input comes from and what its invalidity means. A pattern written as a string
literal in the source — `var slugRe = MustCompileMatcher("^[a-z0-9-]+$")` — is
data the programmer controls, evaluated once when the package initializes. If it
is malformed, the binary is fundamentally broken: there is no sensible runtime
behavior, no user who can fix it, and no reason to let the process start and fail
later. Panicking at init is the correct failure mode because it makes the defect
impossible to ship — the program will not start, the crash points straight at the
bad literal, and it happens on every run including the developer's first.

The identical panic is a bug the moment the pattern comes from a request, a config
file, or any runtime source. Now a user's typo reaches `MustCompileMatcher` and
panics the process — a recoverable validation error converted into an outage.
That is why the two constructors coexist: `NewMatcher` returns
`(*Matcher, error)` for anything untrusted, and `MustCompileMatcher` is a thin
wrapper that calls `NewMatcher` and panics on error, used only with literals. The
wrapper is deliberately tiny and delegates all real work to the error-returning
form, mirroring how `regexp.MustCompile` wraps `regexp.Compile`.

Create `matcher.go`:

```go
package matcher

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidPattern is returned by NewMatcher when the pattern does not compile.
var ErrInvalidPattern = errors.New("invalid pattern")

// Matcher wraps a compiled regular expression.
type Matcher struct {
	re *regexp.Regexp
}

// NewMatcher compiles pattern, returning ErrInvalidPattern on failure. Use this
// for any pattern that comes from user input, a config file, or the network.
func NewMatcher(pattern string) (*Matcher, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidPattern, err)
	}
	return &Matcher{re: re}, nil
}

// MustCompileMatcher compiles pattern and panics on failure. Use it ONLY with a
// compile-time-constant pattern for a package-level value, where an invalid
// pattern means the binary is unshippable. Never call it with runtime input.
func MustCompileMatcher(pattern string) *Matcher {
	m, err := NewMatcher(pattern)
	if err != nil {
		panic(err)
	}
	return m
}

// Matches reports whether s matches the pattern.
func (m *Matcher) Matches(s string) bool {
	return m.re.MatchString(s)
}
```

### The runnable demo

The demo shows the two correct call sites side by side: a package-level
`MustCompileMatcher` with a literal pattern (init-time, trusted), and a runtime
`NewMatcher` handling a pattern that could have come from a user.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/must-constructor-init-invariants"
)

// slugMatcher is a package-level invariant: a bad literal here would panic at
// init, which is the correct failure mode for an unshippable binary.
var slugMatcher = matcher.MustCompileMatcher("^[a-z0-9-]+$")

func main() {
	fmt.Printf("valid slug: %t\n", slugMatcher.Matches("my-service-01"))
	fmt.Printf("bad slug:   %t\n", slugMatcher.Matches("Bad Slug!"))

	// A pattern from an untrusted source uses the error-returning form.
	userPattern := "("
	if _, err := matcher.NewMatcher(userPattern); err != nil {
		fmt.Printf("rejected user pattern: %v\n", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
valid slug: true
bad slug:   false
rejected user pattern: invalid pattern: error parsing regexp: missing closing ): `(`
```

### Tests

`TestMustPanicsOnInvalid` uses `recover` to assert that `MustCompileMatcher`
panics on a bad pattern, and that the recovered value carries `ErrInvalidPattern`.
`TestNewMatcher` covers the error-returning form for both a valid and an invalid
pattern.

Create `matcher_test.go`:

```go
package matcher

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewMatcher(t *testing.T) {
	t.Parallel()
	m, err := NewMatcher("^[a-z]+$")
	if err != nil {
		t.Fatalf("valid pattern rejected: %v", err)
	}
	if !m.Matches("abc") || m.Matches("ABC") {
		t.Fatal("Matches behaved wrong for ^[a-z]+$")
	}
	if _, err := NewMatcher("("); !errors.Is(err, ErrInvalidPattern) {
		t.Fatalf("invalid pattern err = %v, want ErrInvalidPattern", err)
	}
}

func TestMustUsableOnValid(t *testing.T) {
	t.Parallel()
	m := MustCompileMatcher("^[0-9]+$")
	if !m.Matches("42") || m.Matches("x") {
		t.Fatal("MustCompileMatcher produced a wrong matcher")
	}
}

func TestMustPanicsOnInvalid(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MustCompileMatcher did not panic on an invalid pattern")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, ErrInvalidPattern) {
			t.Fatalf("recovered %v, want an error wrapping ErrInvalidPattern", r)
		}
	}()
	// Note: the argument here is a literal only because this test deliberately
	// triggers the panic path. Production code must never pass runtime input to
	// MustCompileMatcher.
	_ = MustCompileMatcher("(")
}

func ExampleMustCompileMatcher() {
	m := MustCompileMatcher("^v[0-9]+$")
	fmt.Println(m.Matches("v2"), m.Matches("beta"))
	// Output: true false
}
```

## Review

The pair is correct when `NewMatcher` returns `ErrInvalidPattern` for a bad
pattern and a working matcher for a good one, and `MustCompileMatcher` panics for
a bad pattern and returns a usable matcher for a good one. The lesson is the rule
for choosing between them: `Must*` for a compile-time-constant package-level value
whose invalidity makes the binary unshippable, so it fails loudly at init; the
error-returning form for anything from a user, a config, or the network. Calling
`Must*` on runtime input is the mistake — it converts a recoverable validation
error into a process-killing panic.

## Resources

- [regexp.MustCompile](https://pkg.go.dev/regexp#MustCompile) — the canonical Must-style constructor this mirrors.
- [regexp.Compile](https://pkg.go.dev/regexp#Compile) — the error-returning form for runtime patterns.
- [Effective Go: Recover](https://go.dev/doc/effective_go#recover) — how the test recovers the panic.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-money-value-object-invariants.md](06-money-value-object-invariants.md) | Next: [08-builder-accumulate-errors.md](08-builder-accumulate-errors.md)
