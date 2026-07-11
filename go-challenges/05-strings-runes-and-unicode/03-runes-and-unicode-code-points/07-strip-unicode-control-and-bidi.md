# Exercise 7: Trojan Source Defense: Stripping Zero-Width and Bidi Control Runes

Zero-width and bidirectional control characters are valid UTF-8 and invisible,
which is exactly what makes them dangerous: a username with a zero-width space
renders identically to a real one but is a different byte string, and a bidi
override in an audit-log field can make the logged text display in a different
order than it was written (a Trojan-Source attack). This is the sanitizer you run
on identifiers and log fields at the trust boundary.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
sanitize/                   independent module: example.com/sanitize
  go.mod                    go 1.25
  internal/sanitize/sanitize.go StripDangerous(s) (string, bool)
  internal/sanitize/sanitize_test.go strip/preserve tables + re-scan
  cmd/demo/main.go          runnable: strip a spoofed identifier
```

Files: `internal/sanitize/sanitize.go`, `internal/sanitize/sanitize_test.go`,
`cmd/demo/main.go`.
Implement: `StripDangerous(s string) (string, bool)` removing format (`unicode.Cf`)
and control (`unicode.Cc`) runes, returning the cleaned string and whether
anything was stripped.
Test: plain ASCII unchanged and `false`; a bidi override stripped and `true`; a
zero-width space between letters removed; accented text preserved; re-scan the
output for any dangerous rune.
Verify: `go test -count=1 -race ./...`

### Which runes are dangerous, and why one category covers them

The Unicode category `Cf` (Format) contains every zero-width character
(`U+200B`..`U+200D`, `U+FEFF`) and every bidirectional control (`U+202A`..`U+202E`
for embeddings/overrides, `U+2066`..`U+2069` for isolates). `unicode.Bidi_Control`
is the bidi subset of `Cf`, so checking `unicode.Is(unicode.Cf, r)` already covers
the Trojan-Source runes. `Cc` (Control) covers the C0/C1 control characters — a
`NUL`, a `BEL`, an escape — that have no place in an identifier or a single log
field either. So the rule is simple and complete: strip any rune in `Cf` or `Cc`,
keep everything else.

Crucially, this does *not* touch legitimate text. A precomposed `é` (`U+00E9`) is
category `Ll` (lowercase letter); a combining accent `U+0301` is `Mn` (nonspacing
mark). Neither is `Cf` or `Cc`, so accented names survive untouched. That is the
whole point: the sanitizer removes the invisible attack surface without mangling
the visible content. The function returns a second value — whether anything was
stripped — so a caller can log or flag a name that contained hidden characters,
which is itself a useful signal.

Note the scope: this is orthogonal to normalization (Exercise 2) and to
case-folding. Stripping a bidi override does not compose or decompose anything,
and NFD-folding a name does not remove a zero-width space. A hardened pipeline
runs all three deliberately.

Set up the module:

```bash
mkdir -p ~/go-exercises/sanitize/internal/sanitize ~/go-exercises/sanitize/cmd/demo
cd ~/go-exercises/sanitize
go mod init example.com/sanitize
```

Create `internal/sanitize/sanitize.go`:

```go
package sanitize

import (
	"strings"
	"unicode"
)

// StripDangerous removes format (Cf) and control (Cc) runes — zero-width
// characters, bidi overrides, and C0/C1 controls — from s, returning the cleaned
// string and whether anything was removed. Visible text (letters, marks, digits,
// punctuation) is preserved.
func StripDangerous(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	stripped := false
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) {
			stripped = true
			continue
		}
		b.WriteRune(r)
	}
	if !stripped {
		return s, false
	}
	return b.String(), true
}
```

When nothing is stripped, the original string is returned directly rather than a
rebuilt copy — a small allocation win on the common (clean) path.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sanitize/internal/sanitize"
)

func main() {
	// "adm\u202ein" uses a right-to-left override to spoof an identifier.
	spoof := "adm\u202ein"
	clean, changed := sanitize.StripDangerous(spoof)
	fmt.Printf("spoof:  %q (len=%d)\n", spoof, len(spoof))
	fmt.Printf("clean:  %q (len=%d) stripped=%v\n", clean, len(clean), changed)

	// Zero-width space hidden between letters.
	zwsp := "ac\u200bme"
	clean2, changed2 := sanitize.StripDangerous(zwsp)
	fmt.Printf("zwsp:   %q -> %q stripped=%v\n", zwsp, clean2, changed2)

	// Legitimate accented text is untouched.
	name := "rené"
	clean3, changed3 := sanitize.StripDangerous(name)
	fmt.Printf("name:   %q -> %q stripped=%v\n", name, clean3, changed3)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
spoof:  "adm\u202ein" (len=8)
clean:  "admin" (len=5) stripped=true
zwsp:   "ac\u200bme" -> "acme" stripped=true
name:   "rené" -> "rené" stripped=false
```

The spoof is 8 bytes (the override `U+202E` encodes to 3 bytes) but cleans to the
5-byte `admin`; the accented name is returned unchanged with `stripped=false`.

### Tests

Create `internal/sanitize/sanitize_test.go`:

```go
package sanitize

import (
	"fmt"
	"testing"
	"unicode"
)

func TestStripDangerous(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{"plain ascii", "admin", "admin", false},
		{"accented preserved", "rené café", "rené café", false},
		{"rtl override", "adm\u202ein", "admin", true},
		{"zero width space", "ac\u200bme", "acme", true},
		{"zero width joiner", "a\u200db", "ab", true},
		{"bom", "\ufefftext", "text", true},
		{"null byte", "a\x00b", "ab", true},
		{"bidi isolate", "x\u2066y\u2069z", "xyz", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, changed := StripDangerous(tc.in)
			if got != tc.want || changed != tc.wantChanged {
				t.Fatalf("StripDangerous(%q) = %q,%v; want %q,%v", tc.in, got, changed, tc.want, tc.wantChanged)
			}
			// The output must contain no dangerous rune, ever.
			for _, r := range got {
				if unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Cc, r) {
					t.Fatalf("output %q still contains dangerous rune %U", got, r)
				}
			}
		})
	}
}

func TestStripDangerousReturnsOriginalWhenClean(t *testing.T) {
	t.Parallel()
	in := "unchanged"
	got, changed := StripDangerous(in)
	if changed || got != in {
		t.Fatalf("StripDangerous(%q) = %q,%v; want unchanged,false", in, got, changed)
	}
}

func ExampleStripDangerous() {
	clean, stripped := StripDangerous("adm\u202ein")
	fmt.Printf("%q %v\n", clean, stripped)
	// Output: "admin" true
}
```

## Review

`StripDangerous` is correct when its output contains no `Cf` or `Cc` rune (the
test re-scans to prove it), when it flags whether it changed anything, and when it
leaves visible text — including accented letters and combining marks — untouched.
The mistake this defends against is trusting that valid UTF-8 is safe UTF-8: a
zero-width or bidi rune is perfectly valid and perfectly hostile. The second
mistake is folding this into normalization; they are separate steps. Run
`go test -race`.

## Resources

- [Trojan Source: Invisible Vulnerabilities (CVE-2021-42574)](https://trojansource.codes/)
- [`unicode` categories (Cf, Cc, Bidi_Control)](https://pkg.go.dev/unicode#pkg-variables)
- [`unicode.Is`](https://pkg.go.dev/unicode#Is)
- [Unicode UAX #9: Bidirectional Algorithm](https://www.unicode.org/reports/tr9/)

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-case-fold-header-compare.md](08-case-fold-header-compare.md)
