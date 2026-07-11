# Exercise 2: A Diagnostic That Surfaces the Normalizer's Blind Spots

Before you decide the stdlib-only normalizer from Exercise 1 is "good enough", you
run it against the inputs you are worried about and read the result. This module is
that diagnostic: a small, testable probe that feeds real problem inputs through the
normalizer and prints rune counts and same-key verdicts, turning two theoretical
limitations into observable facts an engineer can point at in a design review.

This module is fully self-contained: its own `go mod init`, its own bundled copy of
the normalizer, its own demo and tests. It gates alone with no external dependency.

## What you'll build

```text
gapprobe/                      independent module: example.com/gapprobe
  go.mod                       stdlib only
  probe.go                     Normalize (bundled) + printProbe(w io.Writer)
  cmd/demo/main.go             runs printProbe against os.Stdout
  probe_test.go                golden-output test + behavior invariants
```

Files: `probe.go`, `cmd/demo/main.go`, `probe_test.go`.
Implement: `printProbe(w io.Writer)` that runs the stdlib-only `Normalize` over `İSTANBUL`, `STRAßE`, and an NFC/NFD `café` pair, printing input, output, rune counts, and a same-key verdict.
Test: capture `printProbe` into a `bytes.Buffer` and assert the exact locale line (`out="istanbul"`), the unchanged German line (`out == in`), and `same key? false`; plus direct invariants on `Normalize`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/gapprobe/cmd/demo
cd ~/go-exercises/gapprobe
go mod init example.com/gapprobe
```

### Why a probe, and why it takes an io.Writer

A diagnostic that prints to `os.Stdout` inside `main` cannot be tested; a
diagnostic whose body writes to an injected `io.Writer` can. So the real work lives
in `printProbe(w io.Writer)`, `main` passes `os.Stdout`, and the test passes a
`bytes.Buffer` and asserts on the captured bytes. This is the standard shape for
any "print a report" helper you want under test.

The inputs are constructed from explicit code points rather than typed as accented
literals, so their normalization form is unambiguous and the test cannot drift with
an editor's encoding: `İSTANBUL` starts with U+0130 (Latin capital I with dot
above), `STRAßE` contains U+00DF (`ß`), the NFC `café` ends in the single code
point U+00E9, and the NFD `café` ends in `e` + U+0301 (combining acute). The probe
surfaces two distinct failures of the stdlib-only path:

- **Locale blindness.** `Normalize("İSTANBUL")` returns `istanbul`. For a Turkish
  user that is the wrong fold (Turkish lowercases dotless `I` to `ı`), and `ß` in
  `STRAßE` survives instead of folding to `ss` — the output equals the input. A
  locale-blind `strings.ToLower` cannot do either; that needs `x/text/cases`
  (Exercise 5).
- **NFC/NFD asymmetry.** The NFC `café` has no separate combining mark, so the
  accent survives; the NFD `café` loses its mark. The two therefore produce
  *different* keys — `same key? false` — which is exactly the duplicate-account,
  phantom-cache-miss failure the boundary-NFC recipe in Exercises 3 and 4 fixes.

Create `probe.go`:

```go
package probe

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Normalize is the stdlib-only search normalizer from Exercise 1, bundled here so
// this module stands alone: lowercase, then drop Latin combining marks.
func Normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x0300 && r <= 0x036F {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// printProbe runs Normalize over inputs chosen to expose its two blind spots and
// writes a human-readable diagnostic to w. Inputs are built from explicit code
// points so their normalization form is unambiguous.
func printProbe(w io.Writer) {
	istanbul := string(rune(0x0130)) + "STANBUL"   // U+0130 (dotted capital I)
	strasse := "STRA" + string(rune(0x00DF)) + "E" // contains ß (U+00DF)
	cafeNFC := "caf" + string(rune(0x00E9))        // precomposed é (U+00E9)
	cafeNFD := "cafe" + string(rune(0x0301))       // e + combining acute

	fmt.Fprintln(w, "locale gap: strings.ToLower is locale-blind")
	for _, in := range []string{istanbul, strasse} {
		out := Normalize(in)
		fmt.Fprintf(w, "  in=%q out=%q runes %d->%d\n",
			in, out, utf8.RuneCountInString(in), utf8.RuneCountInString(out))
	}

	fmt.Fprintln(w, "nfc-vs-nfd asymmetry: precomposed input is not decomposed")
	na, nb := Normalize(cafeNFC), Normalize(cafeNFD)
	fmt.Fprintf(w, "  nfc=%q -> %q\n", cafeNFC, na)
	fmt.Fprintf(w, "  nfd=%q -> %q\n", cafeNFD, nb)
	fmt.Fprintf(w, "  same key? %v\n", na == nb)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"os"

	"example.com/gapprobe" // package name is "probe"
)

func main() {
	probe.PrintProbe(os.Stdout)
}
```

`printProbe` is unexported so same-package tests can reach it; the demo needs an
exported entry point, so add a thin exported wrapper. Add it to `probe.go`:

Append to `probe.go`:

```go
// PrintProbe is the exported entry point for the cmd/demo binary.
func PrintProbe(w io.Writer) { printProbe(w) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
locale gap: strings.ToLower is locale-blind
  in="İSTANBUL" out="istanbul" runes 8->8
  in="STRAßE" out="straße" runes 6->6
nfc-vs-nfd asymmetry: precomposed input is not decomposed
  nfc="café" -> "café"
  nfd="café" -> "cafe"
  same key? false
```

Read the two problem lines: `İSTANBUL` collapsed its dotted-I to a plain `i`
(locale bug), `STRAßE` only lowercased to `straße` while the `ß` never folded to
`ss`, and the NFC and NFD spellings of `café` produced different keys.

### Tests

The oracle is not the whole formatted blob — it is the load-bearing facts inside
it, expressed as ASCII-only assertions so they cannot drift with encoding. The
golden lines are rebuilt in the test from the same rune-constructed inputs, so
their bytes match `printProbe` exactly; alongside them, three direct invariants on
`Normalize` state what the probe is really claiming.

Create `probe_test.go`:

```go
package probe

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestProbeGoldenLines(t *testing.T) {
	t.Parallel()

	istanbul := string(rune(0x0130)) + "STANBUL"
	strasse := "STRA" + string(rune(0x00DF)) + "E"

	var buf bytes.Buffer
	printProbe(&buf)
	out := buf.String()

	// İSTANBUL folds to a plain-ASCII "istanbul": the locale bug is visible.
	wantIstanbul := fmt.Sprintf("  in=%q out=%q runes 8->8", istanbul, "istanbul")
	if !strings.Contains(out, wantIstanbul) {
		t.Fatalf("missing istanbul line %q in:\n%s", wantIstanbul, out)
	}
	// STRAßE only lowercases: the ß survives instead of folding to ss, so the
	// rune count is unchanged at 6 and "ss" never appears.
	wantStrasse := fmt.Sprintf("  in=%q out=%q runes 6->6", strasse, strings.ToLower(strasse))
	if !strings.Contains(out, wantStrasse) {
		t.Fatalf("missing straße line %q in:\n%s", wantStrasse, out)
	}
	// The NFC/NFD café pair does not collapse to one key.
	if !strings.Contains(out, "same key? false") {
		t.Fatalf("expected NFC/NFD mismatch verdict in:\n%s", out)
	}
}

func TestNormalizeInvariants(t *testing.T) {
	t.Parallel()

	istanbul := string(rune(0x0130)) + "STANBUL"
	strasse := "STRA" + string(rune(0x00DF)) + "E"
	cafeNFC := "caf" + string(rune(0x00E9))
	cafeNFD := "cafe" + string(rune(0x0301))

	if got := Normalize(istanbul); got != "istanbul" {
		t.Errorf("Normalize(İSTANBUL) = %q, want istanbul (locale-blind fold)", got)
	}
	if got := Normalize(strasse); got != strings.ToLower(strasse) || !strings.ContainsRune(got, 0x00DF) {
		t.Errorf("Normalize(STRAßE) = %q, want the ß preserved (not folded to ss)", got)
	}
	if Normalize(cafeNFC) == Normalize(cafeNFD) {
		t.Error("NFC and NFD café unexpectedly produced the same key")
	}
}

func Example() {
	istanbul := string(rune(0x0130)) + "STANBUL"
	cafeNFC := "caf" + string(rune(0x00E9))
	cafeNFD := "cafe" + string(rune(0x0301))
	fmt.Println(Normalize(istanbul))
	fmt.Println(Normalize(cafeNFC) == Normalize(cafeNFD))
	// Output:
	// istanbul
	// false
}
```

## Review

The probe is correct when its printed facts match `Normalize`'s real behavior:
`İSTANBUL` folds to `istanbul` (the locale gap), `STRAßE` comes out unchanged (the
`ß` never folds), and the NFC/NFD `café` pair reports `same key? false` (the
asymmetry). The test asserts those exact facts rather than a pixel-perfect blob, so
it stays robust while still pinning the lines that matter. The point of the module
is judgment: a diagnostic like this is what you run before committing to the
zero-dependency path, and the moment its output shows inputs you actually receive
in production, you have your justification for adopting `x/text` in the modules that
follow.

## Resources

- [`unicode/utf8`](https://pkg.go.dev/unicode/utf8) — `RuneCountInString`, used to make decomposition visible.
- [`fmt` verbs](https://pkg.go.dev/fmt#hdr-Printing) — `%q` quotes a string in Go syntax, keeping printable runes literal.
- [The Go Blog: Text normalization in Go](https://go.dev/blog/normalization) — why NFC vs NFD equality is a real production hazard.

---

Back to [01-stdlib-only-normalizer.md](01-stdlib-only-normalizer.md) | Next: [03-canonical-nfc-at-the-storage-boundary.md](03-canonical-nfc-at-the-storage-boundary.md)
