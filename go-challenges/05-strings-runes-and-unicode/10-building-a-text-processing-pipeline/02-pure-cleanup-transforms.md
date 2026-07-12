# Exercise 2: Implement the Four Field-Cleanup Transforms

With the composition core in hand, this module builds the four concrete
transforms every text field passes through, each as an independent, adversarially
tested pure function. Testing them in isolation is what makes a later composition
bug localizable: if the assembled output is wrong, you know which single stage
misbehaved.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
transforms/               independent module: example.com/transforms
  go.mod                  go 1.26
  transforms.go           DecodeEntities, RemoveControls, Lowercase, CollapseWhitespace
  cmd/
    demo/
      main.go             runnable demo showing each transform and the order rationale
  transforms_test.go      per-transform adversarial table tests
```

- Files: `transforms.go`, `cmd/demo/main.go`, `transforms_test.go`.
- Implement: `DecodeEntities` (HTML entity decode), `RemoveControls` (drop C0/C1 control runes except `\n` and `\t`, built rune-by-rune with `strings.Builder` and `Grow`), `Lowercase`, and `CollapseWhitespace` (`Fields`+`Join`).
- Test: per-transform tables with adversarial inputs — named, decimal, and hex HTML entities; embedded `NUL` and other control bytes; tabs and newlines preserved by `RemoveControls`; runs of mixed Unicode whitespace collapsed.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/02-pure-cleanup-transforms/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/02-pure-cleanup-transforms
```

### What each transform does, and why order matters

`DecodeEntities` wraps `html.UnescapeString`, which decodes HTML entities in text
that was already extracted from HTML. It handles named entities (`&amp;` → `&`,
`&Eacute;` → `É`), decimal numeric references (`&#39;` → `'`), and hex numeric
references (`&#x20AC;` → `€`). This is entity decoding, not HTML parsing — it does
not strip tags or handle malformed markup, and the input contract is
"already-extracted text." It runs first in the chain so that every later stage
sees real characters, not literal `&`, `E`, `a`, ... If lowercasing ran before
decoding, `&Eacute;` would be mangled before it ever became `É`.

`RemoveControls` drops C0 and C1 control characters — the invisible bytes like
`NUL` (U+0000), `BEL` (U+0007), and the C1 range — while deliberately preserving
`\n` (newline) and `\t` (tab), which are legitimate structural whitespace that the
whitespace-collapse stage will handle. It is built rune-by-rune with
`strings.Builder`: `Grow(len(s))` reserves the backing array once (an upper bound,
since the output is never longer than the input in bytes), then `WriteRune`
appends the kept runes. This is the standard filtering shape; the alternative,
`out += string(r)` in a loop, is quadratic. `RemoveControls` runs before
`CollapseWhitespace` so that a control byte cannot survive into a field that has
already been "collapsed" — otherwise an invisible `NUL` sits in the middle of an
indexed value.

`Lowercase` is `strings.ToLower`: Unicode-aware, locale-independent lowering. It
produces a case-normalized value suitable for a case-insensitive search field.

`CollapseWhitespace` is `strings.Join(strings.Fields(s), " ")`. `strings.Fields`
splits around runs of Unicode whitespace (as defined by `unicode.IsSpace`), which
handles ASCII spaces, tabs, newlines, and exotic spaces like the no-break space
alike, and drops the empty fields, so re-joining with a single ASCII space
collapses any run and trims the ends.

Create `transforms.go`:

```go
package transforms

import (
	"html"
	"strings"
	"unicode"
)

// DecodeEntities decodes HTML entities (named, decimal, and hex numeric) in text
// that was already extracted from HTML. It is not an HTML parser.
func DecodeEntities(s string) string {
	return html.UnescapeString(s)
}

// RemoveControls drops C0/C1 control runes, preserving newline and tab. It builds
// the result rune-by-rune with a preallocated strings.Builder to stay linear.
func RemoveControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Lowercase applies Unicode-aware, locale-independent lowercasing.
func Lowercase(s string) string {
	return strings.ToLower(s)
}

// CollapseWhitespace replaces every run of Unicode whitespace with a single
// space and trims the ends.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/transforms"
)

func main() {
	entity := "CAF&Eacute; &amp; &#39;tea&#39; &#x20AC;"
	fmt.Printf("DecodeEntities: %q\n", transforms.DecodeEntities(entity))

	controls := "clean\x00text\x07here\twith\nlines"
	fmt.Printf("RemoveControls: %q\n", transforms.RemoveControls(controls))

	fmt.Printf("Lowercase: %q\n", transforms.Lowercase("GoLang HTTP"))

	spaced := "  many\t\tspaces\n\ncollapse  "
	fmt.Printf("CollapseWhitespace: %q\n", transforms.CollapseWhitespace(spaced))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
DecodeEntities: "CAFÉ & 'tea' €"
RemoveControls: "cleantexthere\twith\nlines"
Lowercase: "golang http"
CollapseWhitespace: "many spaces collapse"
```

### Tests

Each transform gets its own table with adversarial rows so that a failure points
at exactly one stage. `TestDecodeEntities` covers named, decimal, and hex forms.
`TestRemoveControls` proves the C0/C1 bytes are dropped while `\n` and `\t`
survive. `TestCollapseWhitespace` collapses mixed Unicode whitespace (including a
no-break space, U+00A0) and trims. `TestLowercase` covers ASCII and a non-ASCII
letter.

Create `transforms_test.go`:

```go
package transforms

import (
	"fmt"
	"testing"
	"unicode/utf8"
)

func TestDecodeEntities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{"named amp", "a &amp; b", "a & b"},
		{"named accent", "CAF&Eacute;", "CAFÉ"},
		{"decimal apostrophe", "Go&#39;s", "Go's"},
		{"hex euro", "love &#x20AC;", "love €"},
		{"no entities", "plain text", "plain text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := DecodeEntities(tt.in); got != tt.want {
				t.Fatalf("DecodeEntities(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRemoveControls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{"nul dropped", "a\x00b", "ab"},
		{"bell dropped", "a\x07b", "ab"},
		{"c1 dropped", "ab", "ab"},
		{"tab preserved", "a\tb", "a\tb"},
		{"newline preserved", "a\nb", "a\nb"},
		{"mixed", "x\x00y\tz\n", "xy\tz\n"},
		{"no controls", "clean text", "clean text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := RemoveControls(tt.in)
			if got != tt.want {
				t.Fatalf("RemoveControls(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("RemoveControls produced invalid UTF-8: %q", got)
			}
		})
	}
}

func TestLowercase(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{"ascii", "HELLO World", "hello world"},
		{"accent", "CAFÉ", "café"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Lowercase(tt.in); got != tt.want {
				t.Fatalf("Lowercase(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCollapseWhitespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{"runs", "a   b", "a b"},
		{"tabs and newlines", "a\t\tb\n\nc", "a b c"},
		{"nbsp", "a  b", "a b"},
		{"trim ends", "  a b  ", "a b"},
		{"empty", "   ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CollapseWhitespace(tt.in); got != tt.want {
				t.Fatalf("CollapseWhitespace(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func ExampleRemoveControls() {
	fmt.Printf("%q\n", RemoveControls("a\x00b\tc"))
	// Output: "ab\tc"
}
```

## Review

The four transforms are correct when each satisfies its table in isolation:
entities decode in all three forms, control bytes vanish except `\n` and `\t`,
lowercasing is Unicode-aware, and whitespace runs collapse with the ends trimmed.
The reason each has its own test rather than one end-to-end assertion is
localizability — an aggregate failure would not tell you which stage broke. Watch
two traps: `RemoveControls` must preserve `\n` and `\t` (they are structural, not
noise) and must stay linear via `strings.Builder.Grow`, not quadratic
concatenation; and `CollapseWhitespace` relies on `strings.Fields` splitting on
*Unicode* whitespace, so a no-break space collapses too. Run `go test -race` to
keep the module honest.

## Resources

- [html.UnescapeString](https://pkg.go.dev/html#UnescapeString) — decodes named, decimal, and hex HTML entities.
- [unicode.IsControl](https://pkg.go.dev/unicode#IsControl) — the C0/C1 control-character predicate.
- [strings.Builder](https://pkg.go.dev/strings#Builder) — `Grow` and `WriteRune` for linear rune-by-rune building.
- [strings.Fields](https://pkg.go.dev/strings#Fields) — splits around runs of Unicode whitespace.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-ordered-transform-pipeline.md](01-ordered-transform-pipeline.md) | Next: [03-jsonl-streaming-ingester.md](03-jsonl-streaming-ingester.md)
