# Exercise 2: ASCII-Folding Slugs: Transliterate Accents Instead of Dropping Them

The drop policy from Exercise 1 turns `café` into `caf`. The production
alternative is to *fold*: decompose the accented letter, drop the combining mark,
and keep the base letter, so `café résumé` becomes `cafe-resume`. This is the
pre-pass a real slug pipeline runs before the ASCII loop.

This module is fully self-contained: its own `go mod init`, its own dependency on
`golang.org/x/text`, its own demo and tests.

## What you'll build

```text
fold/                       independent module: example.com/fold
  go.mod                    go 1.25; requires golang.org/x/text
  internal/fold/fold.go     Fold(s) string; Slug(s) string (fold then ASCII)
  internal/fold/fold_test.go transliteration table + limitation pins
  cmd/demo/main.go          runnable: accented title -> folded slug
```

Files: `internal/fold/fold.go`, `internal/fold/fold_test.go`, `cmd/demo/main.go`.
Implement: `Fold(s string) string` that NFD-decomposes, removes nonspacing marks
(`unicode.Mn`), and recomposes with NFC; `Slug(s string) string` that folds then
runs the ASCII code-point loop.
Test: `résumé`->`resume`, `naïve`->`naive`, `Zürich`->`Zurich`, plus pins that
`ß` is not folded and `中文` has no decomposition.
Verify: `go test -count=1 -race ./...`

### How NFD-then-drop-Mn works, and where it stops

An accented Latin letter like `é` (`U+00E9`) has a *canonical decomposition*: NFD
rewrites it as the base letter `e` (`U+0065`) followed by `U+0301 COMBINING ACUTE
ACCENT`, which is a nonspacing mark in Unicode category `Mn`. If you then remove
every `Mn` rune and recompose with NFC, you are left with `e`. Chain that over a
whole string and `café résumé` becomes `cafe resume`. The pipeline is exactly
three transformers composed with `transform.Chain`:

```
norm.NFD  ->  runes.Remove(runes.In(unicode.Mn))  ->  norm.NFC
```

`transform.String` runs the chain over the input and returns `(result, n, err)`;
the error is non-nil only on a malformed transformer state, not on ordinary text,
but you check it because ignoring a `transform` error is how silent truncation
sneaks in.

The boundary this exercise pins is where folding *stops*. `ß` (`U+00DF`) has no
canonical decomposition to `ss` — that is a *compatibility* concern, not a
canonical one — so NFD leaves it untouched and `Straße` stays `Straße`, not
`Strasse`. Turning `ß` into `ss` requires an explicit special-case map, which is
a deliberate policy decision, so this module pins that `ß` is *not* folded to
document the limitation. Likewise `中文` has no Latin decomposition at all: no
`Mn` marks to remove, nothing changes, and a slug pipeline still needs a drop or
transliteration map for non-Latin scripts. Folding is best-effort, not a
bijection.

`Slug` wires the fold in front of the same ASCII loop from Exercise 1: fold first
so `é` becomes `e` and survives, then run the ASCII classifier. `café résumé`
now yields `cafe-resume` instead of `caf-rsum`.

Set up the module:

```bash
mkdir -p ~/go-exercises/fold/internal/fold ~/go-exercises/fold/cmd/demo
cd ~/go-exercises/fold
go mod init example.com/fold
go get golang.org/x/text
```

Create `internal/fold/fold.go`:

```go
package fold

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Fold transliterates accented Latin letters to their ASCII base by decomposing
// (NFD), removing nonspacing marks (unicode.Mn), and recomposing (NFC). Runes
// without a canonical decomposition (ß, non-Latin scripts) pass through
// unchanged: folding is best-effort, not lossless.
func Fold(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		return s // transform failed on malformed state; leave input intact
	}
	return out
}

// Slug folds accents to ASCII first, then runs the ASCII code-point loop, so
// "café résumé" becomes "cafe-resume" rather than "caf-rsum".
func Slug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lastWasSeparator := true
	for _, r := range Fold(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z':
			b.WriteRune(unicode.ToLower(r))
			lastWasSeparator = false
		case r >= '0' && r <= '9':
			b.WriteRune(r)
			lastWasSeparator = false
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if !lastWasSeparator {
				b.WriteRune('-')
				lastWasSeparator = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
```

The gate resolves `golang.org/x/text` from the module cache; in your own checkout
run `go mod tidy` after adding the import to pin the version in `go.mod`.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fold/internal/fold"
)

func main() {
	for _, s := range []string{"café résumé", "Zürich", "Straße", "中文"} {
		fmt.Printf("%q -> fold %q -> slug %q\n", s, fold.Fold(s), fold.Slug(s))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"café résumé" -> fold "cafe resume" -> slug "cafe-resume"
"Zürich" -> fold "Zurich" -> slug "zurich"
"Straße" -> fold "Straße" -> slug "strae"
"中文" -> fold "中文" -> slug ""
```

Note `Straße` folds to `Straße` (the `ß` survives) but the ASCII slug loop then
drops the non-ASCII `ß`, yielding `strae` — a concrete reminder that folding and
the ASCII filter are two separate steps with two separate policies.

### Tests

Create `internal/fold/fold_test.go`:

```go
package fold

import (
	"fmt"
	"testing"
)

func TestFold(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"café résumé": "cafe resume",
		"naïve":       "naive",
		"Zürich":      "Zurich",
		"élan":        "elan",
		"plain ascii": "plain ascii",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := Fold(in); got != want {
				t.Fatalf("Fold(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestFoldDoesNotHandleEszett(t *testing.T) {
	t.Parallel()
	// Pins the limitation: ß has no canonical decomposition, so NFD-folding
	// leaves it. Turning it into "ss" needs an explicit special-case map.
	if got := Fold("Straße"); got != "Straße" {
		t.Fatalf("Fold(Straße) = %q, want unchanged Straße", got)
	}
}

func TestFoldLeavesNonLatin(t *testing.T) {
	t.Parallel()
	// Pins that non-Latin scripts have no Mn decomposition: unchanged.
	if got := Fold("中文"); got != "中文" {
		t.Fatalf("Fold(中文) = %q, want unchanged 中文", got)
	}
}

func TestSlug(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"café résumé": "cafe-resume",
		"Zürich":      "zurich",
		"中文 report":   "report",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := Slug(in); got != want {
				t.Fatalf("Slug(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func ExampleFold() {
	fmt.Println(Fold("café résumé"))
	// Output: cafe resume
}
```

## Review

`Fold` is correct when accented Latin folds to its ASCII base and everything else
survives untouched. The two traps are treating folding as lossless (it is not:
`ß`, `ø`, and non-Latin scripts pass through, which is why the limitation is
pinned by tests, not hidden) and confusing this with case-folding or control
stripping — normalization is a third, orthogonal concern. Ignoring the
`transform.String` error is the quiet failure mode: a malformed state would
otherwise return a silently truncated string. Run `go test -race`; the transform
chain is stateless per call, so there is nothing to race, but the check confirms
it.

## Resources

- [Go Blog: Text normalization in Go](https://go.dev/blog/normalization)
- [`golang.org/x/text/runes`](https://pkg.go.dev/golang.org/x/text/runes)
- [`golang.org/x/text/unicode/norm`](https://pkg.go.dev/golang.org/x/text/unicode/norm)
- [`golang.org/x/text/transform`](https://pkg.go.dev/golang.org/x/text/transform)

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-utf8-sanitize-ingest-boundary.md](03-utf8-sanitize-ingest-boundary.md)
