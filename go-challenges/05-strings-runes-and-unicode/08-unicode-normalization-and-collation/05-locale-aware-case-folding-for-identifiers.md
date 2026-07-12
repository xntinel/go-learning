# Exercise 5: Case Folding Usernames Without the Turkish-i / German-ss Bug

A username uniqueness check that lowercases with `strings.ToLower` has a latent
account-collision bug: `STRAßE` stays `straße` instead of folding to `strasse`, and
a Turkish user's `I`/`ı` distinction is silently mangled. This module builds the
canonicalizer identifiers actually want — `cases.Fold`, which is locale-independent
by design — and contrasts it with locale lowercasing so the difference is concrete.

This module is fully self-contained: its own `go mod init`, its own demo and tests.
It uses `golang.org/x/text/cases` and `golang.org/x/text/language`.

## What you'll build

```text
idcasefold/                 independent module: example.com/idcasefold
  go.mod                    requires golang.org/x/text
  fold.go                   CanonicalUsername via cases.Fold; locale contrasts
  cmd/demo/main.go          ToLower vs Fold vs Lower(tr) vs Lower(de)
  fold_test.go             ß->ss collision; Turkish dotless-i; the ToLower bug; idempotency
```

Files: `fold.go`, `cmd/demo/main.go`, `fold_test.go`.
Implement: `CanonicalUsername(string) string` using a package-level `cases.Fold()`; plus thin helpers exposing `cases.Lower(language.Turkish)` and `cases.Lower(language.German)` for contrast.
Test: `cases.Fold().String("STRAßE")` folds to `strasse` so two spellings collide; `cases.Lower(language.Turkish).String("I")` is dotless `ı` while `strings.ToLower("I")` is `i`; `Fold(Fold(x)) == Fold(x)`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/05-locale-aware-case-folding-for-identifiers/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/08-unicode-normalization-and-collation/05-locale-aware-case-folding-for-identifiers
go get golang.org/x/text
```

### Fold versus Lower, and why identifiers want Fold

Lowercasing and case folding are different operations for different jobs.
*Lowercasing* is a display transform and is deliberately locale-sensitive:
`cases.Lower(language.Turkish)` maps capital dotless `I` to dotless `ı` because
that is what a Turkish reader expects, and `cases.Lower(language.German)` keeps `ß`
as `ß`. That sensitivity is correct for rendering text to a human, but it is exactly
wrong for an *identifier* key, where you need one deterministic answer regardless of
whose locale the server or the user is in. If username uniqueness depended on the
request's locale, the same typed name could be free in one locale and taken in
another.

*Case folding* (`cases.Fold`) is the locale-independent operation built for caseless
matching. It folds `ß` to `ss`, so `STRAßE`, `Straße`, and `strasse` all reduce to
`strasse` and collide as one identifier. It has no locale parameter because it is
the same everywhere. That is the property an identity system needs:
`CanonicalUsername` is `cases.Fold().String(s)`, full stop, and two users cannot
register names that differ only by case or by a `ß`/`ss` spelling.

The contrast to internalize, and the classic bug: `strings.ToLower("I")` is `"i"`,
but `cases.Lower(language.Turkish).String("I")` is dotless `"ı"`. A service that
uses `strings.ToLower` for a Turkish user's identifier is applying the wrong locale
rule; a service that uses `cases.Lower(tag)` at all is making identifier equality
depend on locale. `cases.Fold` sidesteps both by not being locale-sensitive.

A `cases.Caser` (what `cases.Fold()` returns) is safe to share and call from many
goroutines, so a single package-level folder is fine — unlike the stateful
transformer chain in Exercise 4, no pool is needed.

Create `fold.go`:

```go
package idcasefold

import (
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// folder is a single shared, locale-independent case folder. A cases.Caser is
// safe for concurrent use, so one package-level value suffices.
var folder = cases.Fold()

// CanonicalUsername returns the locale-independent caseless key for a username.
// Two names that differ only by case or by a ß/ss spelling collapse to one key,
// so uniqueness checks and login lookups agree regardless of locale.
func CanonicalUsername(s string) string {
	return folder.String(s)
}

// LowerTurkish and LowerGerman expose locale lowercasing for contrast only. They
// are the wrong choice for identifier keys (they make equality locale-dependent);
// they exist here to demonstrate why.
func LowerTurkish(s string) string {
	return cases.Lower(language.Turkish).String(s)
}

func LowerGerman(s string) string {
	return cases.Lower(language.German).String(s)
}
```

### The runnable demo

The demo runs the German street and the Turkish capital `I` through four folds —
`strings.ToLower`, locale-independent `Fold`, and the Turkish and German locale
lowerers — so the divergences are side by side.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"strings"

	"example.com/idcasefold"
)

func main() {
	strasse := "STRA" + string(rune(0x00DF)) + "E" // STRAßE

	fmt.Printf("STRAßE ToLower       = %q\n", strings.ToLower(strasse))
	fmt.Printf("STRAßE Fold          = %q\n", idcasefold.CanonicalUsername(strasse))
	fmt.Printf("STRAßE Lower(de)     = %q\n", idcasefold.LowerGerman(strasse))
	fmt.Printf("I      ToLower       = %q\n", strings.ToLower("I"))
	fmt.Printf("I      Lower(tr)     = %q\n", idcasefold.LowerTurkish("I"))
	fmt.Printf("collide strasse==STRAßE via Fold: %v\n",
		idcasefold.CanonicalUsername("strasse") == idcasefold.CanonicalUsername(strasse))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
STRAßE ToLower       = "straße"
STRAßE Fold          = "strasse"
STRAßE Lower(de)     = "straße"
I      ToLower       = "i"
I      Lower(tr)     = "ı"
collide strasse==STRAßE via Fold: true
```

### Tests

The tests state the invariants directly: the `ß`/`ss` collision that `strings.ToLower`
misses, the Turkish dotless-`ı` that locale lowering produces where `strings.ToLower`
gives `i`, and idempotency of the fold.

Create `fold_test.go`:

```go
package idcasefold

import (
	"fmt"
	"strings"
	"testing"
)

func TestFoldCollapsesEszett(t *testing.T) {
	t.Parallel()

	strasse := "STRA" + string(rune(0x00DF)) + "E" // STRAßE
	if got := CanonicalUsername(strasse); got != "strasse" {
		t.Fatalf("CanonicalUsername(STRAßE) = %q, want strasse", got)
	}
	// The whole point: ß-spelling and ss-spelling collide as one identifier.
	if CanonicalUsername(strasse) != CanonicalUsername("strasse") {
		t.Fatal("STRAßE and strasse must fold to the same username key")
	}
	// strings.ToLower does NOT do this — the bug we are avoiding.
	if strings.ToLower(strasse) == "strasse" {
		t.Fatal("strings.ToLower unexpectedly folded ß; the bug is not real")
	}
}

func TestTurkishDotlessI(t *testing.T) {
	t.Parallel()

	dotless := string(rune(0x0131)) // ı
	if got := LowerTurkish("I"); got != dotless {
		t.Fatalf("LowerTurkish(I) = %q, want dotless ı", got)
	}
	// Locale-blind lowering gives a plain dotted i instead: the divergence that
	// silently corrupts Turkish identifiers.
	if strings.ToLower("I") != "i" {
		t.Fatalf("strings.ToLower(I) = %q, want i", strings.ToLower("I"))
	}
	if LowerTurkish("I") == strings.ToLower("I") {
		t.Fatal("Turkish lower and locale-blind lower must differ on I")
	}
}

func TestCaseInsensitiveUsername(t *testing.T) {
	t.Parallel()

	pairs := [][2]string{
		{"Alice", "alice"},
		{"BOB", "bob"},
		{"MixedCase", "mixedcase"},
	}
	for _, p := range pairs {
		if CanonicalUsername(p[0]) != CanonicalUsername(p[1]) {
			t.Fatalf("%q and %q should share a username key", p[0], p[1])
		}
	}
}

func TestFoldIsIdempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"STRA" + string(rune(0x00DF)) + "E",
		"Alice",
		"GROSSE",
	}
	for _, in := range inputs {
		once := CanonicalUsername(in)
		if twice := CanonicalUsername(once); twice != once {
			t.Fatalf("fold not idempotent: %q -> %q -> %q", in, once, twice)
		}
	}
}

func Example() {
	strasse := "STRA" + string(rune(0x00DF)) + "E"
	fmt.Println(CanonicalUsername(strasse) == CanonicalUsername("strasse"))
	fmt.Println(CanonicalUsername(strasse))
	// Output:
	// true
	// strasse
}
```

## Review

The canonicalizer is correct when identifier equality is locale-independent:
`CanonicalUsername` is `cases.Fold`, which folds `ß` to `ss` and case away without
consulting any locale, so `STRAßE`, `Straße`, and `strasse` collide as one key. The
mistakes the tests guard against are the two ways teams get this wrong:
`strings.ToLower` (locale-blind, so it misses the `ß`/`ss` fold and mishandles the
Turkish `I`) and `cases.Lower(tag)` (locale-*sensitive*, so it makes identifier
equality depend on whose locale is active). Neither is right for an identifier;
`Fold` is. The locale lowerers stay in the module only to demonstrate the
divergence, never as the username key. Exercise 6 goes one level further, wrapping
this kind of case mapping inside the full RFC 8265 validation pipeline.

## Resources

- [`golang.org/x/text/cases`](https://pkg.go.dev/golang.org/x/text/cases) — `Fold`, `Lower`, `Upper`, and the `Caser` type.
- [`golang.org/x/text/language`](https://pkg.go.dev/golang.org/x/text/language) — `Turkish`, `German`, `Und`, and `Tag`.
- [Unicode case folding (UTR #21 / DerivedCoreProperties)](https://www.unicode.org/reports/tr44/) — why folding is defined separately from lowercasing.

---

Back to [04-accent-folding-search-key.md](04-accent-folding-search-key.md) | Next: [06-precis-username-and-password-enforcement.md](06-precis-username-and-password-enforcement.md)
