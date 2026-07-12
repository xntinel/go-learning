# Exercise 1: Canonicalize Usernames for Safe URL-Path Storage

The identity service accepts a username the user typed and must store a single
canonical form that is safe to place in a URL path (`/u/alice-smith`). This is a
canonicalization pipeline: an ordered sequence of `strings` transformations whose
order is part of the contract, pinned by an idempotency test.

This module is fully self-contained: its own `go mod init`, its own demo, and its
own tests. Nothing here imports another exercise.

## What you'll build

```text
usernorm/                       independent module: example.com/usernorm
  go.mod                        go 1.26
  username/
    normalize.go                Normalize pipeline + IsValid validator; ErrEmpty/ErrInvalid
    normalize_test.go           table tests + idempotency fixed-point test
  cmd/
    demo/
      main.go                   runnable demo over a handful of raw inputs
```

Files: `username/normalize.go`, `username/normalize_test.go`, `cmd/demo/main.go`.
Implement: `Normalize(raw string) (string, error)` (trim, lower, replace
separators, collapse, strip, trim) and `IsValid(s string) bool` for
already-canonical input.
Test: canonicalization table, empty rejection with `errors.Is`, validator
accept/reject, and `Normalize(Normalize(x)) == Normalize(x)`.
Verify: `go test -count=1 -race ./...`

### Why the order of operations is the spec

The canonical form is whatever the pipeline produces, so the pipeline *is* the
specification and its order is load-bearing. Trim before lower-casing so trailing
space never survives into the case step. Replace separators (`_ . @ -`) with
spaces, then collapse runs of whitespace to a single hyphen with
`strings.Fields` + `strings.Join`, so `a  b` and `a_b` both become `a-b`. Strip
everything outside `[a-z0-9-]` with a package-level regexp — declared once,
because compiling it per call would allocate on the intake path. Finally trim
leading and trailing hyphens so `---alice---` does not keep its edges. A very
long input is capped to `IsValid`'s 50-character bound and re-trimmed, so the
truncation itself cannot leave a dangling hyphen; without that cap a long raw
name would produce a canonical slug that `Normalize` accepts but `IsValid`
rejects on length.

`strings.ToLower` here is Unicode-aware per code point but not locale-aware; it
lower-cases `É` to `é`, and the subsequent strip removes any non-ASCII rune, so
the output is ASCII by construction. That is the right design *for a URL-path
slug*, and explicitly not identity canonicalization: two distinct human
identities can collapse to the same slug, so the slug is a display handle, not a
uniqueness key. `café` and `cafe` both normalize to `caf`-vs-`cafe`; treat
collisions at a higher layer.

Idempotency is the property that proves the ordering is coherent: feeding the
canonical form back through `Normalize` must return it unchanged. The fixed-point
test pins exactly that.

Create `username/normalize.go`:

```go
package username

import (
	"errors"
	"regexp"
	"strings"
)

// ErrEmpty is returned when the input has no usable characters.
var ErrEmpty = errors.New("username: empty input")

// ErrInvalid is returned when canonicalization leaves nothing valid.
var ErrInvalid = errors.New("username: no valid characters")

// allowedASCII matches every character NOT permitted in a canonical username.
// Declared once at package scope: the pattern never changes between calls.
var allowedASCII = regexp.MustCompile(`[^a-z0-9-]`)

// separators collapse to spaces before whitespace is folded to hyphens.
var separators = strings.NewReplacer("_", " ", "-", " ", ".", " ", "@", " ")

// Normalize canonicalizes a raw username into a URL-path-safe slug. The order of
// operations is the contract: trim, lower, replace, collapse, strip, trim.
func Normalize(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrEmpty
	}
	s = strings.ToLower(s)
	s = separators.Replace(s)
	s = strings.Join(strings.Fields(s), "-")
	s = allowedASCII.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	// Cap at IsValid's 50-char bound, then re-trim so truncation cannot leave a
	// trailing hyphen. This keeps the canonical form inside IsValid's length
	// contract, so Normalize's output always passes IsValid.
	if len(s) > 50 {
		s = strings.Trim(s[:50], "-")
	}
	if s == "" {
		return "", ErrInvalid
	}
	return s, nil
}

// IsValid reports whether s is already in canonical form: 1..50 characters, each
// a lowercase ASCII letter, digit, or hyphen. It does not canonicalize.
func IsValid(s string) bool {
	if s == "" || len(s) > 50 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/usernorm/username"
)

func main() {
	inputs := []string{"  Alice Smith  ", "ALICE_SMITH", "café", "  ---  "}
	for _, in := range inputs {
		got, err := username.Normalize(in)
		switch {
		case errors.Is(err, username.ErrEmpty):
			fmt.Printf("%-16q -> ErrEmpty\n", in)
		case errors.Is(err, username.ErrInvalid):
			fmt.Printf("%-16q -> ErrInvalid\n", in)
		default:
			fmt.Printf("%-16q -> %q (valid=%v)\n", in, got, username.IsValid(got))
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
"  Alice Smith  " -> "alice-smith" (valid=true)
"ALICE_SMITH"    -> "alice-smith" (valid=true)
"café"           -> "caf" (valid=true)
"  ---  "        -> ErrInvalid
```

### Tests

`TestNormalizeCanonicalizesInput` pins one output per input shape.
`TestNormalizeRejectsEmpty` asserts the sentinel with `errors.Is`.
`TestIsValid*` pin the validator both ways. `TestNormalizeIdempotent` is the
fixed-point property: normalizing the canonical form returns it unchanged.

Create `username/normalize_test.go`:

```go
package username

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizeCanonicalizesInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "  Alice  ", want: "alice"},
		{in: "Alice Smith", want: "alice-smith"},
		{in: "ALICE_SMITH", want: "alice-smith"},
		{in: "café", want: "caf"},
		{in: "中文", want: ""},
		{in: "a--b", want: "a-b"},
		{in: "  ---  ", want: ""},
		{in: "user_123", want: "user-123"},
		{in: strings.Repeat("a", 80), want: strings.Repeat("a", 50)},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := Normalize(tc.in)
			if err != nil && tc.want != "" {
				t.Fatalf("Normalize(%q) = (%q, %v), want %q", tc.in, got, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeRejectsEmpty(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"", "   ", "\t\n"} {
		if _, err := Normalize(in); !errors.Is(err, ErrEmpty) {
			t.Fatalf("Normalize(%q) err = %v, want ErrEmpty", in, err)
		}
	}
}

func TestIsValidAcceptsCanonicalForm(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"alice", "alice-smith", "user-123"} {
		if !IsValid(s) {
			t.Fatalf("IsValid(%q) = false, want true", s)
		}
	}
}

func TestIsValidRejectsBadInput(t *testing.T) {
	t.Parallel()

	bad := []string{"", "Alice", "alice smith", "café", "alice@smith", strings.Repeat("a", 51)}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			if IsValid(s) {
				t.Fatalf("IsValid(%q) = true, want false", s)
			}
		})
	}
}

func TestNormalizeIdempotent(t *testing.T) {
	t.Parallel()

	raw := []string{"  Alice Smith  ", "ALICE_SMITH", "café", "a--b", "user_123", "  Bob.Jones  ", strings.Repeat("very-long-name-", 8)}
	for _, in := range raw {
		once, err := Normalize(in)
		if err != nil {
			continue
		}
		twice, err := Normalize(once)
		if err != nil {
			t.Fatalf("Normalize(%q) errored on the canonical form: %v", once, err)
		}
		if once != twice {
			t.Fatalf("not idempotent for %q: Normalize=%q, Normalize^2=%q", in, once, twice)
		}
		if !IsValid(once) {
			t.Fatalf("canonical form %q from %q fails IsValid", once, in)
		}
	}
}

func ExampleNormalize() {
	s, _ := Normalize("  Alice_Smith  ")
	fmt.Println(s)
	// Output: alice-smith
}
```

## Review

The pipeline is correct when its output is a fixed point (idempotent) and always
passes `IsValid`, and when the order of steps is the one pinned by the table:
change trim-before-lower to lower-before-trim, or strip-before-collapse, and a
row breaks. The common trap is treating this slug as an identity key — it is not;
`café` and a Cyrillic look-alike can collapse together, so uniqueness belongs to
a higher layer that uses PRECIS or `x/text`, not `ToLower`. The other trap is
compiling `allowedASCII` inside `Normalize`; keep it at package scope so the
intake path does no per-call compile. Confirm with `go test -race` and a
`gofmt -l` that comes back empty.

## Resources

- [strings package](https://pkg.go.dev/strings) — TrimSpace, ToLower, Fields, Join, Trim, NewReplacer.
- [regexp package](https://pkg.go.dev/regexp) — MustCompile and package-level compilation.
- [golang.org/x/text/cases](https://pkg.go.dev/golang.org/x/text/cases) — correct Unicode casing when a slug is not enough.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-bearer-token-header-parser.md](02-bearer-token-header-parser.md)
