# Exercise 8: Enforce a 'Max N Characters' Field Limit in an HTTP Handler

A display-name field advertises a character limit — "2 to 50 characters" — and a
handler must enforce it before the value reaches a database or another service. Measured
in bytes, the limit is wrong for every non-ASCII user: it rejects legitimate names and
lets crafted ones through. This module enforces the limit in runes, returns typed errors
suitable for a 422, and documents the honest place where even the rune count stops
matching what a user perceives.

## What you'll build

```text
namecheck/                 independent module: example.com/namecheck
  go.mod                   go 1.26
  namecheck.go             ValidateDisplayName, sentinel errors, limits
  cmd/
    demo/
      main.go              accepts a 50-CJK name, rejects too-long/empty
  namecheck_test.go        rune limit vs byte bug, empty, control, combining-mark note
```

Files: `namecheck.go`, `cmd/demo/main.go`, `namecheck_test.go`.
Implement: `ValidateDisplayName(name string) error` over rune-count limits, wrapping sentinels.
Test: 50-rune CJK passes the 50-char limit (byte check would fail); empty/whitespace rejected; control char rejected; invalid UTF-8 rejected; combining-mark over-count documented.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/04-string-iteration-bytes-vs-runes/08-rune-count-limit-validator/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/04-string-iteration-bytes-vs-runes/08-rune-count-limit-validator
```

### The validation pipeline, and where runes stop being enough

The checks run in an order that fails cheap and safe first. Validity comes first:
`utf8.ValidString` rejects malformed input before anything else looks at it, because a
"character count" of invalid UTF-8 is meaningless and the value must not persist anyway.
Then `strings.TrimSpace` removes surrounding whitespace and an empty result is rejected —
a name of only spaces is not a name. A control-character scan (`unicode.IsControl`)
rejects embedded `\t`, `\x00`, bidi overrides, and similar, which are valid UTF-8 but do
not belong in a display name. Only then is the length measured, in runes, with
`utf8.RuneCountInString`, against the min and max.

Using `utf8.RuneCountInString` rather than `len` is the entire point. A 50-character CJK
name is 150 bytes; `len(name) > 50` would reject it, and the mirror error — a byte cap
enforced with a rune count — would let 50 CJK runes overflow a `varchar(50)`. Count in
the unit the limit is written in.

Each failure wraps a package sentinel with `%w`, so the handler maps any of them to a
422 with `errors.Is` while the message carries specifics for logs. The senior extension
is stated plainly in the code and locked by a test: `utf8.RuneCountInString` counts *code
points*, not what a human calls a character. The string `"e"` followed by the combining
acute accent U+0301 renders as one glyph `é` but is two runes; a family emoji joined by
zero-width joiners is one glyph and many runes. If your product's "50 characters" must
mean 50 *perceived* characters, rune counting is still wrong, and the fix is grapheme
segmentation — `golang.org/x/text/unicode/norm` to first normalize, or a grapheme
library such as `github.com/rivo/uniseg`. This validator counts runes deliberately and
documents that ceiling rather than pretending it does not exist.

Create `namecheck.go`:

```go
// namecheck.go
package namecheck

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Rune-count limits for a display name. These are CHARACTER limits, so they are
// measured with utf8.RuneCountInString, not len.
const (
	MinRunes = 2
	MaxRunes = 50
)

var (
	ErrInvalidUTF8 = errors.New("display name is not valid UTF-8")
	ErrEmpty       = errors.New("display name is empty")
	ErrControlChar = errors.New("display name contains a control character")
	ErrTooShort    = errors.New("display name is too short")
	ErrTooLong     = errors.New("display name is too long")
)

// ValidateDisplayName enforces a rune-counted length limit and basic content
// rules, returning an error that wraps one of the package sentinels (map to a
// 422). Length is measured in runes so the limit means the same thing for ASCII
// and non-ASCII input.
func ValidateDisplayName(name string) error {
	if !utf8.ValidString(name) {
		return fmt.Errorf("validate display name: %w", ErrInvalidUTF8)
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("validate display name: %w", ErrEmpty)
	}
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return fmt.Errorf("validate display name: %w", ErrControlChar)
		}
	}
	n := utf8.RuneCountInString(trimmed)
	if n < MinRunes {
		return fmt.Errorf("validate display name: %d runes < %d: %w", n, MinRunes, ErrTooShort)
	}
	if n > MaxRunes {
		return fmt.Errorf("validate display name: %d runes > %d: %w", n, MaxRunes, ErrTooLong)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
// cmd/demo/main.go
package main

import (
	"fmt"
	"strings"

	"example.com/namecheck"
)

func main() {
	cjk50 := strings.Repeat("中", 50) // 50 runes, 150 bytes
	fmt.Printf("50-CJK name: bytes=%d ok=%v\n", len(cjk50), namecheck.ValidateDisplayName(cjk50) == nil)

	fmt.Println("byte check would say:", len(cjk50) <= namecheck.MaxRunes)

	for _, s := range []string{"Jo", "   ", strings.Repeat("x", 51)} {
		fmt.Printf("%-6q -> %v\n", s, namecheck.ValidateDisplayName(s))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
50-CJK name: bytes=150 ok=true
byte check would say: false
"Jo"   -> <nil>
"   "  -> validate display name: display name is empty
"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" -> validate display name: 51 runes > 50: display name is too long
```

### Tests

The decisive test accepts a 50-rune CJK name against the 50-character limit and shows a
byte-based check rejecting the same input — the exact bug a `len` limit creates. The
combining-mark test asserts and comments the known ceiling: `RuneCountInString` returns 2
for one perceived character.

Create `namecheck_test.go`:

```go
// namecheck_test.go
package namecheck

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRuneLimitAcceptsCJKThatByteCheckWouldReject(t *testing.T) {
	t.Parallel()
	name := strings.Repeat("中", 50) // 50 runes, 150 bytes
	if err := ValidateDisplayName(name); err != nil {
		t.Fatalf("50-rune CJK name rejected: %v", err)
	}
	// The bug this avoids: a byte-length limit rejects the same valid name.
	if len(name) <= MaxRunes {
		t.Fatal("test premise broken: CJK name should exceed the byte budget")
	}
	byteCheckRejects := len(name) > MaxRunes
	if !byteCheckRejects {
		t.Error("expected a byte-based check to (wrongly) reject the name")
	}
}

func TestValidateDisplayNameRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrEmpty},
		{"whitespace only", "   \t ", ErrEmpty},
		{"too short", "a", ErrTooShort},
		{"too long", strings.Repeat("x", 51), ErrTooLong},
		{"control char", "ab\x07cd", ErrControlChar},
		{"invalid utf8", "ab\xffcd", ErrInvalidUTF8},
	}
	for _, tc := range cases {
		err := ValidateDisplayName(tc.in)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: ValidateDisplayName(%q) = %v, want errors.Is %v", tc.name, tc.in, err, tc.want)
		}
	}
}

func TestValidateDisplayNameAccepts(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"Jo", "café user", "中文名字", "  trimmed to fit  "} {
		if err := ValidateDisplayName(s); err != nil {
			t.Errorf("ValidateDisplayName(%q) = %v, want nil", s, err)
		}
	}
}

// TestCombiningMarkKnownLimitation locks the documented ceiling: rune count is
// not grapheme count. "e" + U+0301 renders as one character "é" but counts as
// two runes. When a product needs perceived-character limits, switch to grapheme
// segmentation (golang.org/x/text/unicode/norm or a uniseg library).
func TestCombiningMarkKnownLimitation(t *testing.T) {
	t.Parallel()
	const combined = "e\u0301" // "e" + combining acute U+0301; renders as one "é"
	if got := utf8.RuneCountInString(combined); got != 2 {
		t.Fatalf("rune count = %d, want 2 (the known over-count)", got)
	}
	// It is one glyph to a human, yet the validator counts it as two runes.
	if utf8.RuneCountInString(combined) == 1 {
		t.Fatal("rune count unexpectedly matched grapheme count")
	}
}

func ExampleValidateDisplayName() {
	fmt.Println(ValidateDisplayName("Jo"))
	fmt.Println(errors.Is(ValidateDisplayName(""), ErrEmpty))
	// Output:
	// <nil>
	// true
}
```

## Review

The validator is correct when the limit means "characters" for everyone: a 50-rune CJK
name passes while a `len` check would reject it, and the empty, too-short, too-long,
control-character, and invalid-UTF-8 paths each wrap the right sentinel so a handler
branches with `errors.Is` and returns 422. The order matters — validate UTF-8 before
counting, since counting garbage is meaningless. The honest limitation, asserted by
`TestCombiningMarkKnownLimitation`, is that `utf8.RuneCountInString` counts code points,
not graphemes; a combining sequence or emoji ZWJ cluster is several runes for one visible
character, and a product that must count what a human sees reaches for grapheme
segmentation. The mistake to avoid is either half of the byte/rune confusion: a byte
limit for a character rule, or a rune limit guarding a byte-capped store.

## Resources

- [unicode/utf8: RuneCountInString, ValidString](https://pkg.go.dev/unicode/utf8) — the character count and up-front validity check.
- [Go Blog: Text normalization in Go](https://go.dev/blog/normalization) — where runes stop matching perceived characters and normalization begins.
- [golang.org/x/text/unicode/norm](https://pkg.go.dev/golang.org/x/text/unicode/norm) — normalization, the first step toward grapheme-correct counting.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-rune-aware-pii-redaction.md](07-rune-aware-pii-redaction.md) | Next: [09-fixed-width-column-formatter.md](09-fixed-width-column-formatter.md)
