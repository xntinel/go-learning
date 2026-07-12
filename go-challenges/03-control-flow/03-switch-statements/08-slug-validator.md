# Exercise 8: Validate a Resource Slug With a Rune-Range Switch

Before a service persists a URL-safe identifier — a tenant slug, a bucket name, a
project handle — it has to validate the string character by character. This
module builds that validator as a `range`-over-string loop with a tagless switch
that classifies each rune by range, enforcing the real rule: lowercase letters
and digits, plus `-` as an interior separator only.

This module is fully self-contained: its own `go mod init`, code, demo, and
tests.

## What you'll build

```text
slug/                      independent module: example.com/slug-validator
  go.mod                   go 1.24
  slug.go                  ValidateSlug(s) error; typed errors
  cmd/
    demo/
      main.go              runnable demo over valid and invalid slugs
  slug_test.go             valid/invalid table with reported index + round-trip check
```

- Files: `slug.go`, `cmd/demo/main.go`, `slug_test.go`.
- Implement: `ValidateSlug(s string) error` using `range` over the string and a tagless switch on rune ranges, rejecting with the offending rune and index.
- Test: a table of valid and invalid slugs asserted with `errors.Is` against typed errors and the reported index, plus a round-trip check on accepted slugs.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Ranging over runes, classifying by range

`for i, r := range s` iterates a string by *rune*, giving the byte index `i` and
the decoded rune `r`. That is exactly what a slug validator wants: it can classify
each rune with a tagless switch (`r >= 'a' && r <= 'z'`, `r >= '0' && r <= '9'`,
`r == '-'`, else reject) and report the byte index of the first offending
character. Ranging by rune rather than indexing bytes means a multi-byte rune —
an accented letter, an emoji — is decoded and rejected as one unit at its true
position, instead of being mistaken for several stray bytes.

The `-` rule is where the state lives. A dash is legal only in the interior: not
leading (`-abc`), not trailing (`abc-`), and not doubled (`a--b`). Leading is
caught by `i == 0`, doubled by tracking whether the previous rune was a dash, and
trailing by checking that flag after the loop ends. This is the case where the
concepts file's note applies: it would be tempting to `fallthrough` from the
digit case into the letter case since they share "reset the dash flag", but that
hides intent and `fallthrough` is unconditional; the clean move is to set the flag
in both cases (or, as here, keep the shared reset explicit) rather than reach for
`fallthrough`.

Three typed errors let callers distinguish *why* a slug was rejected:
`ErrEmptySlug`, `ErrInvalidRune` (an out-of-range character), and
`ErrDashPosition` (a misplaced `-`). Each wraps with `%w` so `errors.Is` works,
and the message carries the offending rune (`%q`) and index for a useful 4xx
response body.

Create `slug.go`:

```go
package slug

import (
	"errors"
	"fmt"
)

// Typed validation errors, asserted by callers with errors.Is.
var (
	ErrEmptySlug    = errors.New("empty slug")
	ErrInvalidRune  = errors.New("invalid character in slug")
	ErrDashPosition = errors.New("misplaced dash in slug")
)

// ValidateSlug reports whether s is a valid URL-safe slug: lowercase ASCII
// letters and digits, with '-' allowed only as an interior separator (not
// leading, trailing, or doubled). It ranges by rune and classifies each with a
// tagless switch, reporting the offending rune and byte index.
func ValidateSlug(s string) error {
	if s == "" {
		return ErrEmptySlug
	}
	prevDash := false
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			prevDash = false
		case r >= '0' && r <= '9':
			prevDash = false
		case r == '-':
			if i == 0 {
				return fmt.Errorf("%w: leading dash at index %d", ErrDashPosition, i)
			}
			if prevDash {
				return fmt.Errorf("%w: doubled dash at index %d", ErrDashPosition, i)
			}
			prevDash = true
		default:
			return fmt.Errorf("%w: %q at index %d", ErrInvalidRune, r, i)
		}
	}
	if prevDash {
		return fmt.Errorf("%w: trailing dash", ErrDashPosition)
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

	"example.com/slug-validator"
)

func main() {
	for _, s := range []string{"my-service", "a1", "x-y-z", "-lead", "trail-", "a--b", "Bucket", "café"} {
		if err := slug.ValidateSlug(s); err != nil {
			fmt.Printf("%-12q REJECT: %v\n", s, err)
		} else {
			fmt.Printf("%-12q OK\n", s)
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
"my-service" OK
"a1"         OK
"x-y-z"      OK
"-lead"      REJECT: misplaced dash in slug: leading dash at index 0
"trail-"     REJECT: misplaced dash in slug: trailing dash
"a--b"       REJECT: misplaced dash in slug: doubled dash at index 2
"Bucket"     REJECT: invalid character in slug: 'B' at index 0
"café"       REJECT: invalid character in slug: 'é' at index 3
```

### Tests

`TestValidateSlug` covers valid slugs and every invalid category — leading,
trailing, and doubled dash; uppercase; a Unicode letter; a space; and empty —
asserting the typed error with `errors.Is` and, for the positional errors, that
the reported index appears in the message. `TestAcceptedRoundTrip` confirms every
accepted slug is returned unchanged (a validator must not mutate its input).

Create `slug_test.go`:

```go
package slug

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSlug(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        string
		wantErr   error
		wantIndex string // substring the message must contain, "" to skip
	}{
		{name: "simple", in: "my-service"},
		{name: "alnum", in: "a1"},
		{name: "multi dash", in: "x-y-z"},
		{name: "empty", in: "", wantErr: ErrEmptySlug},
		{name: "leading dash", in: "-abc", wantErr: ErrDashPosition, wantIndex: "index 0"},
		{name: "trailing dash", in: "abc-", wantErr: ErrDashPosition},
		{name: "doubled dash", in: "a--b", wantErr: ErrDashPosition, wantIndex: "index 2"},
		{name: "uppercase", in: "Abc", wantErr: ErrInvalidRune, wantIndex: "index 0"},
		{name: "unicode letter", in: "café", wantErr: ErrInvalidRune, wantIndex: "index 3"},
		{name: "space", in: "a b", wantErr: ErrInvalidRune, wantIndex: "index 1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateSlug(tc.in)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateSlug(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateSlug(%q) err = %v, want errors.Is %v", tc.in, err, tc.wantErr)
			}
			if tc.wantIndex != "" && !strings.Contains(err.Error(), tc.wantIndex) {
				t.Fatalf("ValidateSlug(%q) err = %q, want it to contain %q", tc.in, err.Error(), tc.wantIndex)
			}
		})
	}
}

func TestAcceptedRoundTrip(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"my-service", "a1", "x-y-z", "tenant-42-eu-west"} {
		if err := ValidateSlug(s); err != nil {
			t.Fatalf("ValidateSlug(%q) = %v, want nil", s, err)
		}
		// Validation is idempotent: an accepted slug is still accepted on a
		// second pass, so persisting and re-reading it never flips the verdict.
		if err := ValidateSlug(s); err != nil {
			t.Fatalf("ValidateSlug(%q) second pass = %v, want nil", s, err)
		}
	}
}
```

## Review

The validator is correct when it accepts exactly the lowercase-alphanumeric slugs
with interior single dashes and rejects everything else at the right index.
Ranging by rune is what makes the Unicode-letter rejection land at the character's
true byte index rather than fragmenting a multi-byte rune, and the dash state
(`prevDash`, the `i == 0` check, the post-loop trailing check) is what encodes the
interior-only rule. The exercise's `fallthrough` lesson is the point to carry:
the digit and letter cases share a small body, and the clean way to share it is a
comma-style grouping or an explicit repeat, never `fallthrough`, which would run
the next case body unconditionally and obscure the intent.

## Resources

- [Go Specification: For statements with range](https://go.dev/ref/spec#For_range) — ranging a string yields rune and byte index.
- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the tagless form used for rune classification.
- [Go blog: Strings, bytes, runes and characters](https://go.dev/blog/strings) — why ranging by rune matters for non-ASCII input.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-domain-error-to-status.md](07-domain-error-to-status.md) | Next: [09-maintenance-window-gate.md](09-maintenance-window-gate.md)
