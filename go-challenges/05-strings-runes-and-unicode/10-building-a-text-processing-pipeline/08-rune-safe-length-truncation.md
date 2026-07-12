# Exercise 8: Truncate Fields to a Length Budget Without Corrupting UTF-8

A `varchar(64)` column or a search-field cap is a limit on characters, and the
naive `s[:64]` cuts on a byte offset — which can split a multi-byte rune into
invalid UTF-8 and corrupt the stored value. This module builds a length enforcer
that truncates on rune boundaries, appends an ellipsis only when it actually cut,
and is honest about the difference between runes and user-perceived graphemes.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
truncate/                 independent module: example.com/truncate
  go.mod                  go 1.26
  truncate.go             Runes(s, max) string; Bytes(s, max) string
  cmd/
    demo/
      main.go             runnable demo over ASCII, multi-byte, and emoji input
  truncate_test.go        boundary, validity, and grapheme-caveat tests
```

- Files: `truncate.go`, `cmd/demo/main.go`, `truncate_test.go`.
- Implement: `Runes(s string, max int) string` (at most `max` runes of content plus an ellipsis when truncated, counting with `utf8.RuneCountInString` and cutting on a rune boundary) and `Bytes(s string, max int) string` (largest rune-boundary prefix that fits in `max` bytes, using `utf8.DecodeRuneInString`).
- Test: ASCII under/over the limit; a string whose byte-slice at the limit would split a multi-byte rune, asserting the result is valid UTF-8 and within budget; exactly-at-limit yields the input unchanged with no ellipsis; an emoji-with-modifier input documenting rune-count vs grapheme behavior.
- Verify: `go test -count=1 -race ./...`

### Cutting on a rune boundary, not a byte offset

`s[:n]` slices at byte offset `n`. If byte `n` lands in the middle of a multi-byte
rune — `é` is two bytes, an emoji is four — the prefix ends with a truncated lead
byte and is no longer valid UTF-8. Stored in a column or re-encoded to JSON, that
is a corrupted value. The fix is to measure and cut in units the limit is actually
about.

`Runes(s, max)` counts characters with `utf8.RuneCountInString`. If the string is
already within budget it is returned untouched — no ellipsis, because nothing was
removed. Otherwise it walks the string with `for i := range s`, where the index `i`
is always the byte offset of a rune boundary, stops once it has emitted `max`
runes, and slices there. The slice never lands mid-rune, so the result is valid
UTF-8. The ellipsis (`…`, one rune) is appended to signal truncation happened; it
is added beyond the `max`-rune content budget, so the caller reserves for it if the
hard cap must include the marker.

`Bytes(s, max)` enforces a byte budget instead — the shape you need for a
byte-limited fixed buffer or a column measured in bytes. It steps rune by rune with
`utf8.DecodeRuneInString`, which returns each rune and its byte `size` (equivalently
`utf8.RuneLen(r)`), and stops before any rune would push the total past `max`. The
returned prefix is the largest rune-boundary prefix that fits, always valid UTF-8.

The honest limit is graphemes. A *rune* is a Unicode code point, but a
user-perceived character — a grapheme cluster — can be several runes: an emoji with
a skin-tone modifier, a flag, a ZWJ sequence, a base letter with stacked combining
marks. `Runes` never corrupts UTF-8, but truncating "é" written decomposed as e + U+0301 (two runes: a base letter
plus a combining acute mark) to one rune keeps the base letter and drops the mark —
valid UTF-8, but a different perceived character. If your budget is "at most N
*perceived* characters," you need a grapheme segmentation library; this function
guarantees runes, and the test documents that boundary explicitly.

Create `truncate.go`:

```go
package truncate

import "unicode/utf8"

// ellipsis marks that truncation removed content.
const ellipsis = "…"

// Runes returns at most max runes of s. If s is longer, it is cut on a rune
// boundary (never splitting a multi-byte rune) and an ellipsis is appended.
// A non-positive max yields just the ellipsis for a non-empty s.
func Runes(s string, max int) string {
	if max < 0 {
		max = 0
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i] + ellipsis
		}
		count++
	}
	// Unreachable: the length guard above already handled s within budget.
	return s
}

// Bytes returns the largest rune-boundary prefix of s that fits in max bytes.
// The result is always valid UTF-8 and never splits a rune.
func Bytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	end := 0
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		if i+size > max {
			break
		}
		i += size
		end = i
	}
	return s[:end]
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/truncate"
)

func main() {
	// Byte-slicing at 4 splits the 'é' (bytes 3-4) in "café society",
	// leaving a lone lead byte: the naive prefix is invalid UTF-8.
	title := "café society"
	naive := title[:4]
	fmt.Printf("naive bytes valid: %v\n", utf8.ValidString(naive))

	fmt.Printf("Runes(%q, 4) = %q\n", title, truncate.Runes(title, 4))
	fmt.Printf("Runes(%q, 20) = %q\n", title, truncate.Runes(title, 20))
	fmt.Printf("Bytes(%q, 5) = %q\n", title, truncate.Bytes(title, 5))

	combo := "e\u0301 done"
	got := truncate.Runes(combo, 1)
	fmt.Printf("Runes(combo,1) = %q valid=%v\n", got, utf8.ValidString(got))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
naive bytes valid: false
Runes("café society", 4) = "café…"
Runes("café society", 20) = "café society"
Bytes("café society", 5) = "café"
Runes(combo,1) = "e…" valid=true
```

### Tests

`TestRunes` covers under-limit (unchanged, no ellipsis), exactly-at-limit
(unchanged), and over-limit on a string where a byte cut would split `é`, asserting
both validity and rune budget. `TestBytes` asserts the byte-budget prefix is valid
and within budget. `TestGraphemeCaveat` documents that a two-rune emoji cluster
truncated to one rune keeps the base emoji — valid UTF-8, one rune, but a different
grapheme.

Create `truncate_test.go`:

```go
package truncate

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

func TestRunes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under limit", "hello", 10, "hello"},
		{"exactly at limit", "hello", 5, "hello"},
		{"ascii over", "hello world", 5, "hello…"},
		{"multibyte over", "café society", 4, "café…"},
		{"empty", "", 3, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Runes(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("Runes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("Runes produced invalid UTF-8: %q", got)
			}
		})
	}
}

func TestRunesNeverSplitsRune(t *testing.T) {
	t.Parallel()

	// "café" has 'é' as two bytes at offset 3-4; every rune-count cut stays valid.
	s := "café society"
	for max := 0; max <= utf8.RuneCountInString(s); max++ {
		got := Runes(s, max)
		if !utf8.ValidString(got) {
			t.Fatalf("Runes(%q, %d) = %q is invalid UTF-8", s, max, got)
		}
	}
}

func TestBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under limit", "café", 10, "café"},
		{"cut before multibyte", "café", 4, "caf"}, // 'é' would push to byte 5
		{"cut includes multibyte", "café", 5, "café"},
		{"ascii", "hello", 3, "hel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Bytes(tt.in, tt.max)
			if got != tt.want {
				t.Fatalf("Bytes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("Bytes produced invalid UTF-8: %q", got)
			}
			if len(got) > tt.max {
				t.Fatalf("Bytes(%q, %d) = %q exceeds byte budget", tt.in, tt.max, got)
			}
		})
	}
}

func TestGraphemeCaveat(t *testing.T) {
	t.Parallel()

	// "é" (e + U+0301) is one grapheme but two runes: base letter + combining acute.
	combo := "e\u0301"
	if n := utf8.RuneCountInString(combo); n != 2 {
		t.Fatalf("emoji rune count = %d, want 2 (base + combining mark)", n)
	}
	// Truncating to one rune keeps the base letter: valid UTF-8, one rune, but a
	// different perceived character than the full cluster.
	got := Runes(combo, 1)
	if !utf8.ValidString(got) {
		t.Fatalf("Runes(emoji, 1) = %q is invalid UTF-8", got)
	}
	if utf8.RuneCountInString(got) != 2 { // base letter + the ellipsis rune
		t.Fatalf("Runes(emoji, 1) = %q, want base emoji plus ellipsis", got)
	}
}

func ExampleRunes() {
	fmt.Printf("%q\n", Runes("café society", 4))
	// Output: "café…"
}
```

## Review

The truncator is correct when it never emits invalid UTF-8 and respects its stated
unit: `Runes` cuts on a rune boundary and returns the input unchanged when it is
within budget (no gratuitous ellipsis), and `Bytes` returns the largest
rune-boundary prefix within the byte budget. The mistake this replaces is `s[:n]`
on a byte offset, which splits a multi-byte rune and corrupts the value. Be
explicit about the grapheme caveat: rune truncation is safe for UTF-8 but a
two-rune emoji cluster can still be cut in half at the perceived-character level,
so document whether your budget is runes or graphemes and reach for a segmentation
library when it must be the latter. Run `go test -race` to confirm.

## Resources

- [unicode/utf8.RuneCountInString](https://pkg.go.dev/unicode/utf8#RuneCountInString) — counting runes, not bytes.
- [unicode/utf8.DecodeRuneInString and RuneLen](https://pkg.go.dev/unicode/utf8#DecodeRuneInString) — stepping rune by rune with byte sizes.
- [unicode/utf8.ValidString](https://pkg.go.dev/unicode/utf8#ValidString) — asserting the result never splits a rune.
- [Unicode Standard Annex #29: Text Segmentation](https://unicode.org/reports/tr29/) — grapheme clusters, the perceived-character unit runes do not capture.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [07-unicode-caseless-username-canonicalization.md](07-unicode-caseless-username-canonicalization.md) | Next: [09-pii-redaction-transform.md](09-pii-redaction-transform.md)
