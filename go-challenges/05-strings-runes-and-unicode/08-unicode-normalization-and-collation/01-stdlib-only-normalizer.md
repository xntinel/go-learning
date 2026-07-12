# Exercise 1: The Stdlib-Only Search Normalizer (No External Deps)

Sometimes you cannot add `golang.org/x/text` — a vendoring freeze, a
supply-chain review, a tiny binary budget. This module builds the zero-dependency
search normalizer a service ships in that situation: lowercase plus a combining-
mark strip over the Latin block, correct for the inputs it is scoped to and honest
about the inputs it is not.

This module is fully self-contained: its own `go mod init`, its own package, demo,
and tests. Nothing here imports any other exercise, and it gates alone with no
external dependency.

## What you'll build

```text
normalizer/                       independent module: example.com/normalizer
  go.mod                          stdlib only
  internal/search/normalize.go    Normalize, stripCombiningMarks, isCombiningMark, IndexKey
  cmd/demo/main.go                runnable demo over accented sample inputs
  internal/search/normalize_test.go   the full preserved suite
```

Files: `internal/search/normalize.go`, `cmd/demo/main.go`, `internal/search/normalize_test.go`.
Implement: `Normalize` = `strings.ToLower` then a `strings.Builder` pass that drops runes in `U+0300..U+036F`, plus `IndexKey` as the public key function.
Test: `TestNormalizeLowercasesASCII`, table `TestNormalizeStripsCombiningMarks`, `TestNormalizeLeavesNFCAlone`, `TestNormalizeIsIdempotent`, property `TestIndexKeyMatchesAcrossCaseForNFDInput`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/01-stdlib-only-normalizer/internal/search
mkdir -p go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/01-stdlib-only-normalizer/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/01-stdlib-only-normalizer
```

### What this baseline gets right, and what it cannot

The task is an index key for case- and accent-insensitive search: two strings that
a user would consider "the same word" must map to the same key. `strings.ToLower`
handles case — and note it is *not* byte-based, it iterates runes and applies
Unicode lowercasing, so `strings.ToLower` of an upper-case accented word keeps the
accent instead of truncating. That closes the case axis for the BMP.

The accent axis is where the stdlib-only path draws its boundary. To drop an accent
you must remove its combining mark, and a combining mark only exists as a separate
rune when the string is in NFD (decomposed) form. This module strips exactly the
Latin combining block `U+0300..U+036F` — the range that holds combining acute,
grave, diaeresis, circumflex, tilde, and the rest of the common Latin accents. For
NFD Latin input (`cafe` + U+0301) it is correct and allocation-light. It has two
honest limits, both made observable by the diagnostic in the next module:

1. It is locale-blind. `strings.ToLower` cannot do Turkish dotless-i or German
   `ß` to `ss`; those need `golang.org/x/text/cases`.
2. It only sees *already-separate* marks. A precomposed NFC `é` (a single
   U+00E9) has no combining mark in the range, so the accent survives untouched.
   Collapsing NFC and NFD to one key needs an NFD pre-pass, which is
   `golang.org/x/text/unicode/norm` territory.

Scoping the artifact to "NFD Latin input, no locale rules" is what makes it a
legitimate zero-dependency choice rather than a broken one. Every NFD input in the
code below is written with explicit `\u` escapes (base letter + combining mark), so
the decomposition is unambiguous in source, and one test *documents* the NFC gap
rather than pretending it does not exist.

Create `internal/search/normalize.go`:

```go
package search

import "strings"

// Normalize returns a case- and (Latin) accent-folded key for search indexing
// and querying. It lowercases with strings.ToLower, then drops combining marks
// in the Latin block U+0300..U+036F. It is correct for NFD Latin input; it does
// not decompose precomposed NFC characters and is locale-blind. See IndexKey.
func Normalize(s string) string {
	s = strings.ToLower(s)
	return stripCombiningMarks(s)
}

func stripCombiningMarks(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isCombiningMark(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isCombiningMark reports whether r is in the Latin combining block. This is the
// stdlib-only stand-in for unicode.Is(unicode.Mn, r), which covers every script
// but lives in the (external) full-normalization path.
func isCombiningMark(r rune) bool {
	return r >= 0x0300 && r <= 0x036F
}

// IndexKey is the public search key: the normalized form of s. Store it as the
// index key and keep the original document alongside for display.
func IndexKey(s string) string {
	return Normalize(s)
}
```

### The runnable demo

The demo folds a handful of NFD accented sample terms the way an indexer would
before storing them. Printing the rune count of each input makes the decomposition
visible: the decomposed `café` is five runes that fold to the four-byte
ASCII key `cafe`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/normalizer/internal/search"
)

func main() {
	// NFD form: base letter + combining mark, written with explicit escapes.
	inputs := []string{
		"HELLO World",
		"café",    // cafe + combining acute
		"résumé", // resume with two acutes
		"Über",    // U + combining diaeresis + ber
	}
	for _, in := range inputs {
		fmt.Printf("%2d runes -> %q\n", len([]rune(in)), search.IndexKey(in))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
11 runes -> "hello world"
 5 runes -> "cafe"
 8 runes -> "resume"
 5 runes -> "uber"
```

### Tests

The suite is the contract. `TestNormalizeStripsCombiningMarks` is the core: NFD
inputs fold to the un-accented lowercase form. `TestNormalizeLeavesNFCAlone` is the
one that keeps the module honest — it asserts that a precomposed NFC accented word
is returned unchanged, documenting the gap instead of hiding it.
`TestIndexKeyMatchesAcrossCaseForNFDInput` is the property that matters for search:
the same NFD content in different cases yields one key.

Create `internal/search/normalize_test.go`:

```go
package search

import "testing"

func TestNormalizeLowercasesASCII(t *testing.T) {
	t.Parallel()

	if got := Normalize("HELLO World"); got != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}

func TestNormalizeStripsCombiningMarks(t *testing.T) {
	t.Parallel()

	// Inputs in NFD form: base rune + combining mark in U+0300..U+036F.
	tests := map[string]string{
		"café":    "cafe", // cafe + combining acute
		"CAFÉ":    "cafe",
		"naïve":   "naive",  // naive + combining diaeresis
		"Über":    "uber",   // Uber + combining diaeresis
		"résumé": "resume", // resume with two acutes
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got := Normalize(in)
			if got != want {
				t.Fatalf("Normalize(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestNormalizeLeavesNFCAlone(t *testing.T) {
	t.Parallel()

	// NFC precomposed (U+00E9) is a single code point, not base + combining
	// mark. The stdlib-only strip sees no mark in U+0300..U+036F, so the input
	// is returned as-is (after lowercasing). Documenting this is the point: a
	// full NFD pass via golang.org/x/text/unicode/norm is needed to make NFC
	// and NFD inputs collapse to the same key.
	got := Normalize("café")
	if got != "café" {
		t.Fatalf("Normalize(NFC caf\\u00e9) = %q, want caf\\u00e9 (no decomposition with stdlib-only path)", got)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{"Hello", "café", "Über", "résumé"}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			once := Normalize(in)
			twice := Normalize(once)
			if once != twice {
				t.Fatalf("Normalize(%q) = %q, then %q", in, once, twice)
			}
		})
	}
}

func TestIndexKeyMatchesAcrossCaseForNFDInput(t *testing.T) {
	t.Parallel()

	// The stdlib-only path covers NFD inputs; the same NFD content under
	// different cases must produce the same index key.
	if IndexKey("café") != IndexKey("CAFÉ") {
		t.Fatal("IndexKey should match across case for NFD inputs")
	}
	if IndexKey("über") != IndexKey("ÜBER") {
		t.Fatal("IndexKey should match across case for NFD inputs")
	}
}
```

## Review

The normalizer is correct when its key is a pure function of case and Latin
combining marks: `Normalize` lowercases, then drops exactly the runes in
`U+0300..U+036F`, and nothing else. The suite proves the in-scope behavior
(`TestNormalizeStripsCombiningMarks`, `TestIndexKeyMatchesAcrossCaseForNFDInput`)
and pins the two out-of-scope gaps rather than papering over them:
`TestNormalizeLeavesNFCAlone` asserts precomposed NFC survives, and the locale gap
is left to the diagnostic in Exercise 2. The mistake to avoid is believing this
baseline is a general Unicode normalizer — it is a deliberately narrow one, and
its value comes from knowing exactly where its boundary is. When your inputs stop
being NFD Latin, you move to the `x/text` chain in Exercise 4, not to a bigger
hand-rolled range table.

## Resources

- [`strings` package](https://pkg.go.dev/strings) — `ToLower`, `Builder`, `Builder.WriteRune`.
- [`unicode` package](https://pkg.go.dev/unicode) — the general categories, including `Mn` (nonspacing marks) that the range approximates.
- [UAX #15: Unicode Normalization Forms](https://unicode.org/reports/tr15/) — NFC vs NFD, and why precomposed input has no separate mark to strip.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-nfc-nfd-locale-gap-probe.md](02-nfc-nfd-locale-gap-probe.md)
