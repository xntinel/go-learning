# Exercise 8: Case-Insensitive Comparison for Headers and Email Local Parts

HTTP header names are case-insensitive; so is the domain of an email address, and
many systems dedupe local parts case-insensitively too. The wrong way to compare
them is to lowercase both sides — it misses `K` (`U+212A KELVIN SIGN`) and
mishandles locale-specific letters. The right way is `strings.EqualFold`, which
applies Unicode simple case-folding for equality.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
match/                      independent module: example.com/match
  go.mod                    go 1.25
  internal/match/match.go   FoldEqual, foldOrbit, lowerBytesEqual (the wrong way)
  internal/match/match_test.go EqualFold vs byte-lowering + SimpleFold orbit
  cmd/demo/main.go          runnable: compare headers and a spoofed key
```

Files: `internal/match/match.go`, `internal/match/match_test.go`,
`cmd/demo/main.go`.
Implement: `FoldEqual(a, b string) bool` over `strings.EqualFold`; `foldOrbit(r
rune) []rune` enumerating a rune's fold orbit with `unicode.SimpleFold`;
`lowerBytesEqual(a, b string) bool` as the deliberately-wrong contrast.
Test: header equality, `K` vs `U+212A`, Turkish-i caveat, Greek final sigma, and
that `foldOrbit` cycles back to the start.
Verify: `go test -count=1 -race ./...`

### Why EqualFold, and what it does not do

`strings.EqualFold(a, b)` reports whether `a` and `b` are equal under Unicode
simple case-folding, comparing rune by rune without allocating a lowercased copy
of either side. It is the correct tool for protocol tokens: `Content-Type` equals
`content-type`, and — the case that breaks naive lowering — `K` (`U+212A`, the
Kelvin sign, a distinct code point that folds to `k`) equals `k`. Lowercasing
bytes would leave the three-byte `U+212A` untouched and declare them unequal.

`unicode.SimpleFold(r)` is the primitive underneath: it walks the *fold orbit* of
a rune — the set of code points that fold together — returning the next one and
eventually cycling back to where it started. The orbit of `k` is `k` (`U+006B`),
`K` (`U+212A`), `K` (`U+004B`); the orbit of lower-case sigma includes the Greek
final sigma `ς`, which is why `EqualFold("ς", "σ")` is true. Enumerating the orbit
until it returns to the start is how you discover every equivalent form of a
character.

The boundary to respect: `EqualFold` does *not* apply locale-specific rules. The
Turkish dotless `ı` (`U+0131`) does not fold to ASCII `I` under simple folding, so
`EqualFold("I", "ı")` is false. That is correct for protocol tokens (you do not
want a Turkish locale to change how a header name matches) but wrong for
human-locale UI text, which needs `golang.org/x/text/collate`. The contrast helper
`lowerBytesEqual` lowercases only ASCII bytes and is shown precisely so a test can
prove it disagrees with `EqualFold` on `U+212A` — the concrete evidence that
byte-lowering is not Unicode-correct.

Create `internal/match/match.go`:

```go
package match

import (
	"strings"
	"unicode"
)

// FoldEqual reports whether a and b are equal under Unicode simple case-folding.
// Correct for protocol tokens (HTTP header names, email domains); not
// locale-aware.
func FoldEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

// foldOrbit returns every code point equivalent to r under simple case-folding,
// starting at r and following unicode.SimpleFold until it cycles back.
func foldOrbit(r rune) []rune {
	orbit := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		orbit = append(orbit, f)
	}
	return orbit
}

// lowerBytesEqual is the WRONG way: it lowercases only ASCII bytes, so it misses
// non-ASCII fold pairs like U+212A (Kelvin sign) and k. Kept to contrast with
// FoldEqual in a test.
func lowerBytesEqual(a, b string) bool {
	return lowerASCII(a) == lowerASCII(b)
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/match/internal/match"
)

func main() {
	fmt.Printf("headers equal: %v\n", match.FoldEqual("Content-Type", "content-type"))

	// U+212A KELVIN SIGN folds to 'k': a spoofed token using it still matches.
	fmt.Printf("Kelvin sign folds to k: %v\n", match.FoldEqual("\u212A", "k"))

	// The orbit of 'k' shows every equivalent code point.
	fmt.Printf("orbit of 'k': %U\n", match.Orbit('k'))
}
```

`Orbit` is the exported accessor over the unexported `foldOrbit` so the demo (a
separate `package main`) can reach it.

Add the accessor:

Add to `internal/match/match.go`:

```go
// Orbit exposes foldOrbit for demos and callers outside the package.
func Orbit(r rune) []rune { return foldOrbit(r) }
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
headers equal: true
Kelvin sign folds to k: true
orbit of 'k': [U+006B U+212A U+004B]
```

The orbit shows three code points that all fold together: `k`, the Kelvin sign,
and ASCII `K` — the spoofing surface that makes fold-aware comparison matter.

### Tests

Create `internal/match/match_test.go`:

```go
package match

import (
	"fmt"
	"testing"
)

func TestFoldEqual(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"header case", "Content-Type", "content-type", true},
		{"ascii K vs k", "K", "k", true},
		{"kelvin vs k", "\u212A", "k", true},   // U+212A KELVIN SIGN
		{"greek final sigma", "ς", "σ", true},  // ς vs σ
		{"turkish dotless i", "I", "ı", false}, // not locale-folded
		{"different", "etag", "e-tag", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FoldEqual(tc.a, tc.b); got != tc.want {
				t.Fatalf("FoldEqual(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestByteLoweringDisagrees(t *testing.T) {
	t.Parallel()
	// EqualFold matches the Kelvin sign to 'k'; ASCII byte-lowering does not.
	// This pins WHY byte-lowering is not Unicode-correct.
	const kelvin = "\u212A" // U+212A KELVIN SIGN
	if !FoldEqual(kelvin, "k") {
		t.Fatal("FoldEqual should match Kelvin sign to k")
	}
	if lowerBytesEqual(kelvin, "k") {
		t.Fatal("lowerBytesEqual should NOT match Kelvin sign to k")
	}
}

func TestFoldOrbitCycles(t *testing.T) {
	t.Parallel()
	orbit := foldOrbit('k')
	if len(orbit) != 3 {
		t.Fatalf("orbit of 'k' = %v, want 3 code points", orbit)
	}
	// Applying SimpleFold len(orbit) times returns to the start.
	r := 'k'
	for range orbit {
		r = simpleFoldStep(r)
	}
	if r != 'k' {
		t.Fatalf("SimpleFold cycle did not return to 'k', got %U", r)
	}
}

func ExampleFoldEqual() {
	fmt.Println(FoldEqual("ETag", "etag"))
	// Output: true
}
```

`simpleFoldStep` is a tiny wrapper so the test does not import `unicode`
directly:

Add to `internal/match/match.go`:

```go
// simpleFoldStep is one step of the fold orbit, used by tests.
func simpleFoldStep(r rune) rune { return unicode.SimpleFold(r) }
```

## Review

`FoldEqual` is correct when it matches protocol tokens case-insensitively
*including* non-ASCII fold pairs like `U+212A`/`k`, and when it deliberately does
*not* apply locale rules (the Turkish-i case is pinned as `false`). The mistake
this replaces is lowercasing bytes or even whole strings for comparison: the
`lowerBytesEqual` contrast proves it misses the Kelvin sign, which is a real
spoofing vector for header and identifier matching. For human-locale text, reach
for `x/text/collate`, not this. Run `go test -race`.

## Resources

- [`strings.EqualFold`](https://pkg.go.dev/strings#EqualFold)
- [`unicode.SimpleFold`](https://pkg.go.dev/unicode#SimpleFold)
- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)
- [RFC 9110 §5.1: Field names are case-insensitive](https://www.rfc-editor.org/rfc/rfc9110#name-field-names)

---

Back to [00-concepts.md](00-concepts.md) | Next: [09-byte-offset-to-line-column.md](09-byte-offset-to-line-column.md)
