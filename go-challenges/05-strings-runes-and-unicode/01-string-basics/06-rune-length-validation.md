# Exercise 6: Enforce display-name length and reject invalid UTF-8 on user input

"Max 50 characters" is a requirement every account-settings endpoint has, and
`len(name)` is the wrong way to check it. This module builds the display-name
validator a backend actually ships: a rune-counted length bound, a UTF-8
well-formedness gate, and a control-character reject, each with its own typed
error.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
displayname/                independent module: example.com/displayname
  go.mod                    go 1.26
  displayname.go            ValidateDisplayName + typed errors
  cmd/
    demo/
      main.go               validate a few names, print the verdicts
  displayname_test.go       ASCII/multibyte/emoji/invalid-UTF-8/control table
```

Files: `displayname.go`, `cmd/demo/main.go`, `displayname_test.go`.
Implement: `ValidateDisplayName(name string) error` returning `ErrTooShort`,
`ErrTooLong`, `ErrNotUTF8`, or `ErrControlChar`.
Test: ASCII within/over limit, a multibyte name that passes on rune-count but
would fail on `len`, a 4-byte code point, invalid UTF-8, and a control char.
Verify: `go test -count=1 -race ./...`

## Why rune count, UTF-8 validity, and control chars are three separate checks

The length rule is in *characters*, so it must be measured in runes.
`utf8.RuneCountInString(name)` counts code points; `len(name)` counts bytes. For
`café résumé` the byte count is 13 but the character count is 11, and for a CJK or
emoji name the gap is far larger. Enforcing "3 to 30 characters" on `len` would
reject a perfectly valid two-emoji name and accept an over-long ASCII one — the
exact bug this exercise exists to prevent. So the bound is checked on
`utf8.RuneCountInString`.

But rune-counting only makes sense on well-formed UTF-8. A `string` assembled from
client bytes can be invalid, and `utf8.RuneCountInString` counts each invalid byte
as one rune (a U+FFFD), which would let garbage sneak past a character bound. So
the first gate is `utf8.ValidString(name)`: reject non-UTF-8 outright with
`ErrNotUTF8` before any counting. Order matters — validate encoding, then count.

Control characters (`\n`, `\r`, `\t`, NUL, and the rest of the C0/C1 ranges) are
rejected with `ErrControlChar` because a display name is single-line human text:
a newline in a name is either a mistake or an injection attempt (a forged log line
or a broken CSV cell downstream). `unicode.IsControl(r)` classifies them. We trim
surrounding whitespace first so a name that is only spaces collapses to empty and
trips `ErrTooShort`, but we reject *interior* control characters.

Each failure is a distinct sentinel wrapped with `%w` so a handler can map it to a
specific HTTP 400 message and the test can assert it with `errors.Is`.

Create `displayname.go`:

```go
package displayname

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	minRunes = 3
	maxRunes = 30
)

var (
	ErrNotUTF8     = errors.New("name is not valid UTF-8")
	ErrTooShort    = errors.New("name is too short")
	ErrTooLong     = errors.New("name is too long")
	ErrControlChar = errors.New("name contains a control character")
)

// ValidateDisplayName enforces a character-count bound, UTF-8 well-formedness,
// and the absence of control characters. Surrounding whitespace is ignored.
func ValidateDisplayName(name string) error {
	if !utf8.ValidString(name) {
		return ErrNotUTF8
	}

	name = strings.TrimSpace(name)

	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w", ErrControlChar)
		}
	}

	switch n := utf8.RuneCountInString(name); {
	case n < minRunes:
		return fmt.Errorf("%w: %d < %d", ErrTooShort, n, minRunes)
	case n > maxRunes:
		return fmt.Errorf("%w: %d > %d", ErrTooLong, n, maxRunes)
	}
	return nil
}
```

## The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/displayname"
)

func main() {
	names := []string{"al", "café résumé", "日本語太郎", "ok name"}
	for _, n := range names {
		if err := displayname.ValidateDisplayName(n); err != nil {
			fmt.Printf("%-14q reject: %v\n", n, err)
		} else {
			fmt.Printf("%-14q accept\n", n)
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
"al"           reject: name is too short: 2 < 3
"café résumé"  accept
"日本語太郎"        accept
"ok name"      accept
```

## Tests

The table pins the character-versus-byte distinction directly: `café résumé`
passes at 11 runes even though its byte length exceeds it, and a 40-character
ASCII name fails. A name carrying a 4-byte code point (a CJK Extension B
ideograph) exercises the widest UTF-8 sequence. Invalid UTF-8 and an interior
newline are rejected with their own sentinels.

Create `displayname_test.go`:

```go
package displayname

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr error // nil means accept
	}{
		{"ascii ok", "alice", nil},
		{"too short", "al", ErrTooShort},
		{"only spaces", "   ", ErrTooShort},
		{"too long ascii", strings.Repeat("a", 40), ErrTooLong},
		{"multibyte ok", "café résumé", nil}, // 11 runes, more bytes
		{"cjk ok", "日本語太郎", nil},
		{"4-byte rune ok", "name 𠮷 tail", nil},
		{"invalid utf8", "bad\xffname", ErrNotUTF8},
		{"interior newline", "line1\nline2", ErrControlChar},
		{"tab injected", "a\tb name", ErrControlChar},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateDisplayName(tc.input)
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("ValidateDisplayName(%q) = %v, want nil", tc.input, err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateDisplayName(%q) = %v, want errors.Is %v", tc.input, err, tc.wantErr)
			}
		})
	}
}

func TestLenWouldRejectValidUnicode(t *testing.T) {
	t.Parallel()

	// A name that a naive len-based check would mis-handle: fewer runes than bytes.
	const name = "café résumé"
	if len(name) <= 11 {
		t.Fatalf("test premise wrong: len(%q) = %d, expected > 11", name, len(name))
	}
	if err := ValidateDisplayName(name); err != nil {
		t.Fatalf("rune-counted validator rejected a valid 11-char name: %v", err)
	}
}

func ExampleValidateDisplayName() {
	fmt.Println(ValidateDisplayName("al"))
	fmt.Println(ValidateDisplayName("alice"))
	// Output:
	// name is too short: 2 < 3
	// <nil>
}
```

## Review

The validator is correct when the length bound is measured in runes, not bytes, so
a multibyte name of 11 characters is accepted while a 40-character ASCII name is
rejected; when non-UTF-8 input is refused before any counting; and when interior
control characters are rejected. The three checks are ordered deliberately —
encoding validity first, then control characters, then the count — and each
failure carries a distinct sentinel so a handler maps it to a precise 400. The
`TestLenWouldRejectValidUnicode` case documents the whole reason the exercise
exists. Run `go test -race`; the validator is pure.

## Resources

- [utf8.RuneCountInString (pkg.go.dev)](https://pkg.go.dev/unicode/utf8#RuneCountInString)
- [utf8.ValidString (pkg.go.dev)](https://pkg.go.dev/unicode/utf8#ValidString)
- [unicode.IsControl (pkg.go.dev)](https://pkg.go.dev/unicode#IsControl)
- [The Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-substring-retention.md](05-substring-retention.md) | Next: [07-sanitize-valid-utf8.md](07-sanitize-valid-utf8.md)
