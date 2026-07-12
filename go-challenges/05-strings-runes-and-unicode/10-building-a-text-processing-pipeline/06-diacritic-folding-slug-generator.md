# Exercise 6: Generate Canonical Slugs by Stripping Diacritics

URLs, S3 object keys, and filenames want an ASCII-ish canonical key: "Résumé
Cafétéria" becomes "resume-cafeteria". This module builds that slug generator as a
normalization chain — decompose, drop combining marks, recompose — followed by
lowercasing and collapsing non-alphanumerics to single hyphens, and it is honest
about the one thing it does not do: romanize other scripts.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise. It uses `golang.org/x/text`; the gate fetches it.

## What you'll build

```text
slug/                     independent module: example.com/slug
  go.mod                  go 1.26 (requires golang.org/x/text)
  slug.go                 Slug(string) (string, error) via transform.Chain
  cmd/
    demo/
      main.go             runnable demo over accented, punctuated, and CJK input
  slug_test.go            slug-shape, error-checked, non-romanize, idempotence tests
```

- Files: `slug.go`, `cmd/demo/main.go`, `slug_test.go`.
- Implement: `Slug(string) (string, error)` chaining `norm.NFD`, `runes.Remove(runes.In(unicode.Mn))`, and `norm.NFC` with `transform.Chain`/`transform.String`, then lowercasing letters/digits and collapsing everything else to single hyphens with no leading, trailing, or doubled hyphen.
- Test: accented/mixed-case/punctuated inputs map to clean hyphen slugs; the `transform.String` error is checked, not discarded; a CJK input documents that diacritic stripping does not romanize; `Slug(Slug(s)) == Slug(s)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/06-diacritic-folding-slug-generator/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/06-diacritic-folding-slug-generator
go get golang.org/x/text/transform golang.org/x/text/runes golang.org/x/text/unicode/norm
```

### Decompose, drop marks, recompose

Stripping a diacritic is not a lookup table; it is a normalization. NFD
(Normalization Form D) decomposes each precomposed accented letter into its base
letter plus its combining marks: `é` (U+00E9) becomes `e` + U+0301 (combining
acute). The combining marks are exactly the runes in Unicode category `Mn` (Mark,
nonspacing), so `runes.Remove(runes.In(unicode.Mn))` deletes them, leaving the
bare base letters. A final NFC recomposes whatever remains into canonical form.
`transform.Chain` wires the three transformers into one, and `transform.String`
runs it and — importantly — returns an error. That error must be checked, not
discarded with `_`; a chain can fail on malformed input, and swallowing the error
hides bad data.

After stripping, the slug shaping is a single pass with a `strings.Builder`:
letters and digits are lowercased and appended; every other rune (space,
punctuation, symbol) becomes a hyphen, but only one hyphen per run and never at the
start. A final `TrimRight` removes a trailing hyphen. The result has no leading,
trailing, or doubled hyphens by construction.

The honest limit is the point of the exercise. This canonicalizes *Latin-script*
text: it strips accents because Latin accents are combining marks over a Latin
base. It does not transliterate. `Ø` (U+00D8, O with stroke) has no combining
mark to remove — the stroke is part of the letter — so it survives as `ø`. Greek,
Cyrillic, and Han characters are letters with no Latin base, so `日本語` passes
through unchanged. A slug generator that promised "ASCII output" would be lying;
this one promises "diacritics stripped, case-folded, hyphenated," and keeps that
promise.

Create `slug.go`:

```go
package slug

import (
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Slug produces a lowercase, hyphen-separated canonical key: it strips Latin
// diacritics (NFD, drop nonspacing marks, NFC), lowercases letters and digits,
// and collapses every other run of characters to a single hyphen. It does not
// transliterate non-Latin scripts.
func Slug(s string) (string, error) {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	stripped, _, err := transform.String(t, s)
	if err != nil {
		return "", fmt.Errorf("strip diacritics: %w", err)
	}

	var b strings.Builder
	b.Grow(len(stripped))
	prevHyphen := false
	for _, r := range stripped {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			prevHyphen = false
		default:
			if b.Len() > 0 && !prevHyphen {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-"), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/slug"
)

func main() {
	inputs := []string{
		"Résumé Cafétéria",
		"  Hello, World!  ",
		"a---b__c",
		"日本語",
		"ÀÉÎ Ø",
	}
	for _, in := range inputs {
		out, err := slug.Slug(in)
		if err != nil {
			fmt.Printf("%q -> error: %v\n", in, err)
			continue
		}
		fmt.Printf("%q -> %q\n", in, out)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"Résumé Cafétéria" -> "resume-cafeteria"
"  Hello, World!  " -> "hello-world"
"a---b__c" -> "a-b-c"
"日本語" -> "日本語"
"ÀÉÎ Ø" -> "aei-ø"
```

### Tests

The table covers the slug shapes, including collapsing runs of punctuation and
trimming the ends. `TestSlugShapes` asserts exact strings. `TestDoesNotRomanize`
documents the honest limit: a CJK string is unchanged, and `Ø` keeps its stroke.
`TestErrorIsReturned` confirms the happy path returns a nil error (the discard of
`transform.String`'s error is the mistake this guards against). `TestIdempotent`
proves `Slug(Slug(s)) == Slug(s)`.

Create `slug_test.go`:

```go
package slug

import (
	"fmt"
	"testing"
)

func TestSlugShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in, want string
	}{
		{"accents", "Résumé Cafétéria", "resume-cafeteria"},
		{"punctuation", "  Hello, World!  ", "hello-world"},
		{"runs collapse", "a---b__c", "a-b-c"},
		{"digits kept", "Order #42 v2", "order-42-v2"},
		{"all punctuation", "!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Slug(tt.in)
			if err != nil {
				t.Fatalf("Slug(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("Slug(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDoesNotRomanize(t *testing.T) {
	t.Parallel()

	// Diacritic stripping is not transliteration: CJK passes through, and a
	// stroked letter keeps its stroke because a stroke is not a combining mark.
	if got, _ := Slug("日本語"); got != "日本語" {
		t.Fatalf("Slug(CJK) = %q, want it unchanged", got)
	}
	if got, _ := Slug("Ø"); got != "ø" {
		t.Fatalf("Slug(Ø) = %q, want %q (stroke survives)", got, "ø")
	}
}

func TestErrorIsReturned(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"", "plain", "Ünïcödé"} {
		if _, err := Slug(s); err != nil {
			t.Fatalf("Slug(%q) unexpected error: %v", s, err)
		}
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"Résumé Cafétéria", "a---b", "日本語", ""} {
		once, err := Slug(s)
		if err != nil {
			t.Fatal(err)
		}
		twice, err := Slug(once)
		if err != nil {
			t.Fatal(err)
		}
		if once != twice {
			t.Fatalf("Slug not idempotent for %q: %q vs %q", s, once, twice)
		}
	}
}

func ExampleSlug() {
	s, _ := Slug("Résumé Cafétéria")
	fmt.Println(s)
	// Output: resume-cafeteria
}
```

## Review

The slug generator is correct when the normalization chain strips Latin
diacritics and the shaping pass yields lowercase, single-hyphen-separated output
with no leading, trailing, or doubled hyphen. The two mistakes to avoid are
discarding the `transform.String` error (it can fail on malformed input, and a
silent slug of bad data is worse than a surfaced error) and overselling the
transform as romanization — it strips accents, it does not transliterate Greek,
Cyrillic, Han, or a stroked `Ø`. Keep the input contract honest and the idempotence
test green. Run `go test -race` to confirm the builder path is clean.

## Resources

- [golang.org/x/text/transform: Chain, String](https://pkg.go.dev/golang.org/x/text/transform) — composing transformers and running them with an error return.
- [golang.org/x/text/runes: Remove, In](https://pkg.go.dev/golang.org/x/text/runes) — building the nonspacing-mark filter.
- [unicode.Mn and RangeTable](https://pkg.go.dev/unicode#pkg-variables) — the Mark, nonspacing category.
- [The Go Blog: text normalization in Go](https://go.dev/blog/normalization) — the NFD/remove/NFC diacritic-stripping recipe.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-nfc-normalization-for-dedup-keys.md](05-nfc-normalization-for-dedup-keys.md) | Next: [07-unicode-caseless-username-canonicalization.md](07-unicode-caseless-username-canonicalization.md)
