# Exercise 5: Signup Handler: Rune-Length and Control-Char Username Validation

A username limit expressed in bytes is a bug: `len(s) <= 30` rejects a 15-letter
accented name and admits a 30-emoji name that is far longer visually. The
validator behind `POST /signup` must count *code points*, reject control and
non-printable runes, and refuse invalid UTF-8 — consistently, so no byte-length
bypass sneaks a hostile name into the identity table.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
account/                    independent module: example.com/account
  go.mod                    go 1.25
  internal/account/account.go ValidateUsername + sentinel errors
  internal/account/account_test.go rune-vs-byte and control-char tables
  cmd/demo/main.go          runnable: validate a batch of candidate names
```

Files: `internal/account/account.go`, `internal/account/account_test.go`,
`cmd/demo/main.go`.
Implement: `ValidateUsername(s string) error` enforcing a 3..30 code-point length,
rejecting control characters, non-printable runes, surrounding whitespace, and
invalid UTF-8, returning typed sentinel errors.
Test: ok ASCII, ok accented, too short, too long by runes, embedded null/control,
invalid UTF-8, surrounding spaces; assert with `errors.Is`.
Verify: `go test -count=1 -race ./...`

### The order of checks, and why length is in runes

The checks run in a deliberate order so the error is the most specific true one.
First `utf8.ValidString` — every later check assumes decodable input, and an
undecodable name is rejected outright with `ErrInvalidUTF8`. Next the
surrounding-whitespace check (`s != strings.TrimSpace(s)`), because leading or
trailing spaces are almost always a copy-paste artifact and a display hazard.
Then the length bounds, measured with `utf8.RuneCountInString`: 3 to 30 *code
points*. This is the crux — a name of 20 accented letters is 20 runes but may be
40 bytes, so a byte limit would wrongly reject it, while a 31-rune name is
rejected no matter how few bytes a naive check might see. Finally a rune loop
rejects any `unicode.IsControl` rune (a `NUL`, a `DEL`, an escape) or any
non-`unicode.IsPrint` rune (zero-width and format characters that are not
control but are still not printable).

Each failure returns a distinct package-level sentinel wrapped with `%w`, so the
handler can map `ErrTooShort` and `ErrTooLong` to a helpful `422` message and
`ErrControlChar`/`ErrInvalidUTF8` to a blunt `400`, all via `errors.Is`.

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/03-runes-and-unicode-code-points/05-username-rune-validation/internal/account go-solutions/05-strings-runes-and-unicode/03-runes-and-unicode-code-points/05-username-rune-validation/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/03-runes-and-unicode-code-points/05-username-rune-validation
```

Create `internal/account/account.go`:

```go
package account

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
	ErrInvalidUTF8   = errors.New("username is not valid UTF-8")
	ErrSurroundSpace = errors.New("username has leading or trailing whitespace")
	ErrTooShort      = errors.New("username is too short")
	ErrTooLong       = errors.New("username is too long")
	ErrControlChar   = errors.New("username contains a control character")
	ErrNotPrintable  = errors.New("username contains a non-printable rune")
)

// ValidateUsername enforces a 3..30 code-point length and rejects invalid UTF-8,
// surrounding whitespace, control characters, and non-printable runes.
func ValidateUsername(s string) error {
	if !utf8.ValidString(s) {
		return ErrInvalidUTF8
	}
	if s != strings.TrimSpace(s) {
		return ErrSurroundSpace
	}
	switch n := utf8.RuneCountInString(s); {
	case n < minRunes:
		return fmt.Errorf("%w: got %d runes, need at least %d", ErrTooShort, n, minRunes)
	case n > maxRunes:
		return fmt.Errorf("%w: got %d runes, allow at most %d", ErrTooLong, n, maxRunes)
	}
	for _, r := range s {
		switch {
		case unicode.IsControl(r):
			return fmt.Errorf("%w: %U", ErrControlChar, r)
		case !unicode.IsPrint(r):
			return fmt.Errorf("%w: %U", ErrNotPrintable, r)
		}
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
	"strings"

	"example.com/account/internal/account"
)

func main() {
	names := []string{
		"alice",
		"rené",
		strings.Repeat("é", 20), // 20 runes, 40 bytes: passes on runes
		"ab",
		"has\x00null",
		" spaced ",
	}
	for _, n := range names {
		if err := account.ValidateUsername(n); err != nil {
			fmt.Printf("reject %-14q %v\n", n, err)
		} else {
			fmt.Printf("accept %-14q (%d bytes)\n", n, len(n))
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
accept "alice"        (5 bytes)
accept "rené"         (5 bytes)
accept "éééééééééééééééééééé" (40 bytes)
reject "ab"           username is too short: got 2 runes, need at least 3
reject "has\x00null"  username contains a control character: U+0000
reject " spaced "     username has leading or trailing whitespace
```

The `strings.Repeat("é", 20)` name is 40 bytes but 20 runes: it passes the
rune-based length rule, proving the limit is code points, not bytes.

### Tests

Create `internal/account/account_test.go`:

```go
package account

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want error // nil for accept
	}{
		{"ok ascii", "alice", nil},
		{"ok accented", "rené", nil},
		{"ok long accented in bytes", strings.Repeat("é", 20), nil},
		{"too short", "ab", ErrTooShort},
		{"too long", strings.Repeat("a", 31), ErrTooLong},
		{"embedded null", "has\x00null", ErrControlChar},
		{"embedded escape", "a\x1bcd", ErrControlChar},
		{"invalid utf8", "ali\xffce", ErrInvalidUTF8},
		{"surrounding spaces", " alice ", ErrSurroundSpace},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateUsername(tc.in)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("ValidateUsername(%q) = %v, want nil", tc.in, err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("ValidateUsername(%q) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

func TestRuneLengthNotByteLength(t *testing.T) {
	t.Parallel()
	// 31 runes of accented text is far over 30 bytes, but the point is it is
	// rejected on RUNE count, and a 20-rune/40-byte name is accepted.
	if err := ValidateUsername(strings.Repeat("é", 31)); !errors.Is(err, ErrTooLong) {
		t.Fatalf("31 runes: got %v, want ErrTooLong", err)
	}
	if err := ValidateUsername(strings.Repeat("é", 20)); err != nil {
		t.Fatalf("20 runes/40 bytes: got %v, want accept", err)
	}
}

func ExampleValidateUsername() {
	fmt.Println(ValidateUsername("ab"))
	// Output: username is too short: got 2 runes, need at least 3
}
```

## Review

`ValidateUsername` is correct when the length bound is code points (so a 20-rune
40-byte name passes and a 31-rune name fails), when invalid UTF-8 and control or
non-printable runes are rejected before any of that matters, and when every
failure wraps a distinct sentinel so the handler can branch with `errors.Is`. The
central mistake is `len(s)` for the length limit; the second is checking
printability without first gating on `utf8.ValidString`, since decoding decisions
on invalid bytes are meaningless. Run `go test -race`.

## Resources

- [`utf8.RuneCountInString`](https://pkg.go.dev/unicode/utf8#RuneCountInString)
- [`unicode.IsControl`](https://pkg.go.dev/unicode#IsControl)
- [`unicode.IsPrint`](https://pkg.go.dev/unicode#IsPrint)
- [`errors.Is`](https://pkg.go.dev/errors#Is)

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-streaming-rune-decoder.md](06-streaming-rune-decoder.md)
