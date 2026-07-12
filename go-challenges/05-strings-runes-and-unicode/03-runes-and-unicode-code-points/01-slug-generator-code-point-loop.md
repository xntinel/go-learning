# Exercise 1: Slug Generator: A Code-Point Loop That Does Not Corrupt Input

Turning a human title into a URL-safe slug is the canonical rune loop: you range
over code points, keep the ones you want, and build the output without ever
splitting a multi-byte character. Get the loop wrong and you emit half a rune
into a URL and a database key.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
slug/                       independent module: example.com/slug
  go.mod                    go 1.25
  internal/slug/slug.go     Generate(title) string; ASCII code-point loop
  internal/slug/slug_test.go table-driven ASCII/non-ASCII/separator + idempotency
  cmd/demo/main.go          runnable: title -> slug
```

Files: `internal/slug/slug.go`, `internal/slug/slug_test.go`, `cmd/demo/main.go`.
Implement: `Generate(title string) string` ranging over runes, keeping ASCII
letters/digits, collapsing whitespace/`-`/`_` into single hyphens, lowercasing
with `unicode.ToLower`, dropping non-ASCII, built with `strings.Builder` and
trimmed of edge hyphens.
Test: table tests for ASCII, non-ASCII drop policy, separator collapse,
lowercasing, empty string, plus `TestGenerateIdempotent`.
Verify: `go test -count=1 -race ./...`

### Why a code-point loop, and why "drop" is a policy

`range` over the title decodes UTF-8 and hands you one code point at a time, so
you never see a fragment of a multi-byte character. The classifier is pure ASCII
arithmetic (`r >= 'a' && r <= 'z'`) because the target alphabet is ASCII; anything
outside it is either a separator to collapse or a rune to drop. A
`lastWasSeparator` flag collapses any run of spaces, hyphens, and underscores into
one hyphen, and `strings.Trim` removes the leading and trailing hyphens a run at
either edge would otherwise leave.

The load-bearing decision is what happens to `é`, `中`, and `ß`. This module's
policy is **drop** — a slug of `café` is `caf`. That is a real, defensible choice
(ASCII-only keys, no transliteration dependency), but it is a *choice*, and it is
lossy: `Über Größe` becomes `ber-gre`. So the drop policy is pinned by
`TestGenerateDropsNonASCII`. A future refactor to transliterate (Exercise 2) must
consciously change that test rather than silently altering every existing URL.
Case-folding uses `unicode.ToLower` on the rune, never `strings.ToLower` on the
string, and the output is assembled with `strings.Builder.WriteRune` so the whole
transform allocates one buffer instead of one string per rune.

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/03-runes-and-unicode-code-points/01-slug-generator-code-point-loop/internal/slug go-solutions/05-strings-runes-and-unicode/03-runes-and-unicode-code-points/01-slug-generator-code-point-loop/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/03-runes-and-unicode-code-points/01-slug-generator-code-point-loop
```

Create `internal/slug/slug.go`:

```go
package slug

import (
	"strings"
	"unicode"
)

// Generate converts a human title into a lowercase, hyphen-separated ASCII slug.
// Non-ASCII runes are dropped (a documented, tested policy); runs of whitespace,
// '-', and '_' collapse to a single hyphen; leading and trailing hyphens are
// trimmed.
func Generate(title string) string {
	var b strings.Builder
	b.Grow(len(title))

	lastWasSeparator := true
	for _, r := range title {
		switch {
		case isASCIILetter(r):
			b.WriteRune(unicode.ToLower(r))
			lastWasSeparator = false
		case isASCIIDigit(r):
			b.WriteRune(r)
			lastWasSeparator = false
		case unicode.IsSpace(r) || r == '-' || r == '_':
			if !lastWasSeparator {
				b.WriteRune('-')
				lastWasSeparator = true
			}
		default:
			// Non-ASCII runes are dropped. See Exercise 2 for the
			// transliterating alternative. This policy is pinned by a test.
		}
	}

	return strings.Trim(b.String(), "-")
}

func isASCIILetter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

func isASCIIDigit(r rune) bool {
	return r >= '0' && r <= '9'
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/slug/internal/slug"
)

func main() {
	titles := []string{
		"  Deploy v2: Rolling_Update  ",
		"Café Münchén",
	}
	for _, t := range titles {
		fmt.Printf("%q -> %q\n", t, slug.Generate(t))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"  Deploy v2: Rolling_Update  " -> "deploy-v2-rolling-update"
"Café Münchén" -> "caf-mnchn"
```

### Tests

The suite preserves the original ASCII, drop, separator, and lowercasing tables
and adds the idempotency invariant: a slug of a slug is the slug. That fixed-point
property is what makes a slug safe to re-derive (e.g. after a title edit) without
churning the URL.

Create `internal/slug/slug_test.go`:

```go
package slug

import (
	"fmt"
	"testing"
)

func TestGenerateASCII(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"Hello World":           "hello-world",
		"  Multiple   Spaces  ": "multiple-spaces",
		"already-slugified":     "already-slugified",
		"snake_case":            "snake-case",
		"":                      "",
		"a-b-c-d":               "a-b-c-d",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := Generate(in); got != want {
				t.Fatalf("Generate(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestGenerateLowercasesLetters(t *testing.T) {
	t.Parallel()
	if got := Generate("ABCdef"); got != "abcdef" {
		t.Fatalf("got %q, want %q", got, "abcdef")
	}
}

func TestGenerateDropsNonASCII(t *testing.T) {
	t.Parallel()
	// Pins the drop policy. Changing it (e.g. to transliterate) must update
	// this table consciously.
	tests := map[string]string{
		"café résumé": "caf-rsum",
		"中文 title":    "title",
		"Über Größe":  "ber-gre",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := Generate(in); got != want {
				t.Fatalf("Generate(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestGenerateCollapsesSeparators(t *testing.T) {
	t.Parallel()
	if got := Generate("a   b---c___d"); got != "a-b-c-d" {
		t.Fatalf("got %q, want %q", got, "a-b-c-d")
	}
}

func TestGenerateIdempotent(t *testing.T) {
	t.Parallel()
	inputs := []string{
		"Hello World",
		"café résumé",
		"  Deploy v2: Rolling_Update  ",
		"already-slugified",
		"",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			once := Generate(in)
			twice := Generate(once)
			if once != twice {
				t.Fatalf("Generate not idempotent: Generate(%q)=%q, Generate(that)=%q", in, once, twice)
			}
		})
	}
}

func ExampleGenerate() {
	fmt.Println(Generate("Hello, World!"))
	// Output: hello-world
}
```

## Review

`Generate` is correct when its output is always a valid ASCII slug — lowercase
letters, digits, and single interior hyphens with no edge hyphens — and when
running it twice changes nothing. The two mistakes that matter here are reaching
for `strings.ToLower` on the whole string (which would mishandle the non-ASCII it
is supposed to fold per rune) and building the result with `+=` concatenation
(quadratic; use the `Builder`). The drop policy is not a bug to fix but a
contract to honor: the pinned `TestGenerateDropsNonASCII` is what forces the next
engineer to decide, not stumble. Run `go test -race` to confirm nothing shares
state across the parallel subtests.

## Resources

- [Go Blog: Strings, bytes, runes and characters in Go](https://go.dev/blog/strings)
- [Go spec: For statements with range clause](https://go.dev/ref/spec#For_range)
- [`strings.Builder`](https://pkg.go.dev/strings#Builder)
- [`unicode.ToLower`](https://pkg.go.dev/unicode#ToLower)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-ascii-fold-transliteration.md](02-ascii-fold-transliteration.md)
