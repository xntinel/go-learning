# Exercise 5: API Input Validation — UTF-8 Validity and Rune-Bounded Truncation

A user submits a display name or bio; it lands in a column with a *character* limit,
not a byte limit, and it might not even be valid UTF-8. This module builds the
validation a text boundary needs: reject invalid UTF-8 and control characters, and
truncate to N runes without splitting a multi-byte code point — the classic bug where
slicing by byte index corrupts the last character.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
textfield/                 independent module: example.com/textfield
  go.mod                   go 1.26
  textfield.go             ValidateName, TruncateRunes, Lengths
  cmd/
    demo/
      main.go              validates a name, reports lengths, truncates by rune
  textfield_test.go        byte-vs-rune lengths, invalid UTF-8, control char, rune-safe truncation
```

- Files: `textfield.go`, `cmd/demo/main.go`, `textfield_test.go`.
- Implement: `ValidateName` (trim, reject invalid UTF-8 and control chars), `TruncateRunes` (cut on rune boundaries), `Lengths` (byte count and rune count).
- Test: `"Hello, 世界"` is 13 bytes but 9 runes; invalid bytes are rejected; truncating `"日本語ですよ"` to 3 runes yields `"日本語"` with no `RuneError` and a correct byte boundary; a control character is rejected; truncation output is itself valid UTF-8.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/textfield/cmd/demo
cd ~/go-exercises/textfield
go mod init example.com/textfield
```

### Bytes for storage, runes for the limit

Two different length questions live at this boundary and must not be confused. The
database column and the wire framing care about *bytes* — `len(s)`. The product rule
"display names are at most 30 characters" cares about *runes* — `utf8.RuneCountInString(s)`.
For `"Hello, 世界"` these disagree: 13 bytes, 9 runes. Enforcing a rune limit with
`len` would reject a legitimately short name full of multi-byte characters, and
enforcing a byte limit with a rune count could overflow a byte-sized column. `Lengths`
returns both so the caller uses the right one for the right constraint.

Truncation is where the byte/rune distinction becomes a correctness bug rather than an
off-by-some. The tempting `s[:limit]` cuts at a byte index, and if that index falls in
the middle of a three-byte rune it leaves a dangling partial sequence that renders as
U+FFFD (`utf8.RuneError`) or corrupts the field. The correct truncation counts runes
and cuts at the *byte offset of the Nth rune*, which is exactly what ranging over a
string gives you: `for i := range s` yields `i` at each rune boundary. When the count
reaches the limit, `s[:i]` is a clean cut. The result is guaranteed valid UTF-8, which
the tests confirm with `utf8.ValidString`.

Validity itself is not assumed. `ValidateName` runs `utf8.ValidString` before anything
else, because inbound bytes reinterpreted as a string can be arbitrary garbage, and
storing invalid UTF-8 poisons every later read, log line, and re-encode. It then
rejects control characters with `unicode.IsControl`, which have no place in a display
name and are a common injection/spoofing vector.

Create `textfield.go`:

```go
package textfield

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sentinel errors for the text boundary.
var (
	ErrEmpty       = errors.New("empty after trimming")
	ErrInvalidUTF8 = errors.New("not valid UTF-8")
	ErrControl     = errors.New("contains a control character")
)

// ValidateName trims surrounding whitespace and rejects empty input, invalid
// UTF-8, and control characters. It returns the cleaned name on success.
func ValidateName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ErrEmpty
	}
	if !utf8.ValidString(s) {
		return "", ErrInvalidUTF8
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: %U", ErrControl, r)
		}
	}
	return s, nil
}

// TruncateRunes returns the first maxRunes runes of s, cutting on a rune
// boundary so no multi-byte code point is split. The result is valid UTF-8.
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	count := 0
	for i := range s { // i is the byte offset of each rune boundary
		if count == maxRunes {
			return s[:i]
		}
		count++
	}
	return s
}

// Lengths reports the byte length (for storage/wire) and the rune length (for
// user-facing character limits) of s.
func Lengths(s string) (bytes, runes int) {
	return len(s), utf8.RuneCountInString(s)
}
```

### The runnable demo

The demo validates a name with a multi-byte character, reports both lengths for a
mixed string, truncates a CJK string to three runes, and shows an invalid-UTF-8 input
being rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/textfield"
)

func main() {
	name, err := textfield.ValidateName("  José  ")
	if err != nil {
		fmt.Println("validate:", err)
		return
	}
	fmt.Println("name:", name)

	bytes, runes := textfield.Lengths("Hello, 世界")
	fmt.Printf("lengths: bytes=%d runes=%d\n", bytes, runes)

	trunc := textfield.TruncateRunes("日本語ですよ", 3)
	fmt.Printf("truncated: %s (bytes=%d)\n", trunc, len(trunc))

	_, err = textfield.ValidateName(string([]byte{0xff, 0xfe}))
	fmt.Println("invalid rejected:", errors.Is(err, textfield.ErrInvalidUTF8))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
name: José
lengths: bytes=13 runes=9
truncated: 日本語 (bytes=9)
invalid rejected: true
```

### Tests

`TestLengths` pins the byte-versus-rune disagreement for `"Hello, 世界"`. `TestValidateName`
covers the accept and reject paths: a clean name passes, invalid bytes fail with
`ErrInvalidUTF8`, a control character fails with `ErrControl`. `TestTruncateRunes` is
the correctness core: truncating `"日本語ですよ"` to 3 runes yields exactly `"日本語"`,
the result is valid UTF-8 (`utf8.ValidString`), contains no `RuneError`, and the first
rune still decodes cleanly with `utf8.DecodeRuneInString` — proving the cut landed on a
boundary. A byte-index truncation is shown to corrupt the same input, justifying the
rune-aware version.

Create `textfield_test.go`:

```go
package textfield

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLengths(t *testing.T) {
	t.Parallel()

	bytes, runes := Lengths("Hello, 世界")
	if bytes != 13 || runes != 9 {
		t.Fatalf("Lengths = %d bytes, %d runes; want 13, 9", bytes, runes)
	}
}

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr error
	}{
		{name: "ascii", in: "Alice", want: "Alice"},
		{name: "trimmed multibyte", in: "  José  ", want: "José"},
		{name: "invalid utf8", in: string([]byte{0xff, 0xfe}), wantErr: ErrInvalidUTF8},
		{name: "control char", in: "bad\x07name", wantErr: ErrControl},
		{name: "empty", in: "   ", wantErr: ErrEmpty},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := ValidateName(tt.in)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ValidateName(%q) error = %v, want %v", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateName(%q) unexpected error %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ValidateName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	t.Parallel()

	const in = "日本語ですよ"
	got := TruncateRunes(in, 3)
	if got != "日本語" {
		t.Fatalf("TruncateRunes = %q, want 日本語", got)
	}
	if len(got) != 9 {
		t.Fatalf("byte length = %d, want 9 (three 3-byte runes)", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncated output is not valid UTF-8")
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatal("truncated output contains RuneError")
	}
	if r, _ := utf8.DecodeRuneInString(got); r == utf8.RuneError {
		t.Fatal("first rune failed to decode")
	}

	// A naive byte-index cut corrupts the same input: byte 4 is mid-rune.
	if utf8.ValidString(in[:4]) {
		t.Fatal("expected in[:4] to be invalid UTF-8, proving byte cuts are unsafe")
	}
}

func TestTruncateShortStringUnchanged(t *testing.T) {
	t.Parallel()

	if got := TruncateRunes("hi", 10); got != "hi" {
		t.Fatalf("TruncateRunes short = %q, want hi", got)
	}
}

func ExampleTruncateRunes() {
	fmt.Println(TruncateRunes("日本語ですよ", 3))
	// Output: 日本語
}
```

## Review

The validation is correct when invalid UTF-8 never passes and truncation never
produces it. The decisive test is `TestTruncateRunes`: cutting `"日本語ですよ"` at three
runes gives nine bytes, valid UTF-8, no `RuneError` — while `in[:4]`, a byte-index cut,
is invalid, which is precisely the corruption the rune-aware version avoids. The two
length questions must stay separate: use the byte count for storage and framing and the
rune count for user-facing limits; conflating them is the most common bug this boundary
exists to prevent. Rejecting control characters and empty-after-trim keeps obviously
malformed names out before they reach storage.

## Resources

- [unicode/utf8 package](https://pkg.go.dev/unicode/utf8) — `ValidString`, `RuneCountInString`, `DecodeRuneInString`, `RuneError`.
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings) — why `range` decodes runes and byte-index slicing splits them.
- [unicode.IsControl](https://pkg.go.dev/unicode#IsControl) — the control-character class rejected here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [06-bytes-vs-string-wire.md](06-bytes-vs-string-wire.md)
