# Exercise 7: Deprecate Reverse's byte behavior and route callers to a rune-safe successor

`Reverse` reverses bytes, which corrupts any multi-byte UTF-8 string. The wrong
fix is to change `Reverse` to be rune-safe — that silently changes behavior for
existing callers — or to delete it, which forces a major bump. The right fix is a
migration: add `ReverseRunes` as the correct successor, mark `Reverse` with the
tooling-recognized `// Deprecated:` line, and keep `Reverse` working as a thin
shim so existing consumers still compile. Deprecation is a signal, not a removal.

This module is fully self-contained: its own `go mod init`, the library with both
functions, a demo, and tests that pin the byte-vs-rune contracts and assert the
deprecation marker is present.

## What you'll build

```text
publicstr/                 independent module: example.com/publicstr
  go.mod                   go 1.26
  strings.go               Reverse (Deprecated), ReverseRunes (successor)
  cmd/
    demo/
      main.go              runnable demo contrasting byte vs rune reversal
  strings_test.go          byte behavior preserved; rune round-trip; Deprecated marker present
```

- Files: `strings.go`, `cmd/demo/main.go`, `strings_test.go`.
- Implement: `ReverseRunes` (correct multi-byte handling) and a `Reverse` shim whose doc comment carries the exact `// Deprecated:` paragraph.
- Test: `ReverseRunes` reverses multi-byte input correctly (`"abç"` round-trips); byte `Reverse` keeps its documented byte behavior; a `go/doc` check that `Reverse`'s comment contains a `Deprecated:` paragraph so tooling flags uses.
- Verify: `go test -count=1 -race ./...`

### What the Deprecated convention actually does

`// Deprecated:` is not a comment style — it is a recognized marker. A doc comment
paragraph that *begins* with `Deprecated:` (a paragraph on its own, separated from
the rest by a blank comment line) is understood by `gopls`, flagged by staticcheck
as `SA1019` when a consumer uses the symbol, and rendered with a strikethrough and
a callout on pkg.go.dev. A developer who calls `Reverse` in an editor with `gopls`
sees the deprecation and the suggested successor without you sending a single
email. That is the whole mechanism: a machine-readable migration signal embedded in
the docs.

Crucially, the deprecated symbol *keeps working*. `Reverse` still compiles, still
reverses bytes, still passes its old tests — because deleting or changing it would
break consumers, and the entire reason to deprecate rather than remove is to avoid
that break. A common and safe shape is to keep the deprecated function delegating to
nothing (its behavior is unchanged) while the successor implements the corrected
behavior separately. Here `Reverse` retains byte reversal (its documented, if
flawed, contract) and `ReverseRunes` is the new, correct rune-aware function.
Consumers migrate at their own pace: nothing forces them off `Reverse` today, and
no major bump is required.

`ReverseRunes` handles multi-byte input by converting to `[]rune` first, so a
character like `ç` (two bytes in UTF-8) is treated as one unit. `"abç"` reverses to
`"çba"` and round-trips back to `"abç"` — whereas byte `Reverse("abç")` produces an
invalid UTF-8 sequence. The test contrasts the two directly, which is also the
documentation of *why* the successor exists.

Create `strings.go`:

```go
package publicstr

// Reverse returns s with its bytes in reverse order. It is not rune-safe:
// multi-byte UTF-8 sequences are corrupted.
//
// Deprecated: Reverse reverses bytes and corrupts multi-byte UTF-8. Use
// ReverseRunes for correct rune-aware reversal. Reverse remains only so existing
// callers continue to compile.
func Reverse(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// ReverseRunes returns s with its runes (Unicode code points) in reverse order,
// preserving multi-byte UTF-8 characters. It is the rune-safe successor to the
// deprecated Reverse.
func ReverseRunes(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
```

### The runnable demo

The demo shows the difference the deprecation exists to fix: byte reversal mangles
`"abç"`, rune reversal round-trips it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unicode/utf8"

	"example.com/publicstr"
)

func main() {
	const s = "abç"

	byteRev := publicstr.Reverse(s)
	fmt.Printf("Reverse(%q) valid-utf8=%v\n", s, utf8.ValidString(byteRev))

	runeRev := publicstr.ReverseRunes(s)
	fmt.Printf("ReverseRunes(%q) = %q, round-trips=%v\n",
		s, runeRev, publicstr.ReverseRunes(runeRev) == s)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
Reverse("abç") valid-utf8=false
ReverseRunes("abç") = "çba", round-trips=true
```

### Tests

The tests pin three contracts: `ReverseRunes` correctly reverses multi-byte input
and round-trips; byte `Reverse` still exhibits its documented byte behavior (so
migrating consumers are not surprised); and the `Deprecated:` paragraph is present
in `Reverse`'s doc comment, which is what makes tooling flag its use.

Create `strings_test.go`:

```go
package publicstr

import (
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReverseRunes(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"hello", "olleh"},
		{"abç", "çba"},
		{"go語", "語og"},
		{"", ""},
	}
	for _, tc := range cases {
		got := ReverseRunes(tc.in)
		if got != tc.want {
			t.Errorf("ReverseRunes(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("ReverseRunes(%q) produced invalid UTF-8", tc.in)
		}
		if ReverseRunes(got) != tc.in {
			t.Errorf("ReverseRunes(%q) does not round-trip", tc.in)
		}
	}
}

func TestReverseKeepsByteBehavior(t *testing.T) {
	t.Parallel()
	// The deprecated function must keep its documented byte behavior so existing
	// callers are not surprised by a silent change.
	if got := Reverse("hello"); got != "olleh" {
		t.Fatalf("Reverse(hello) = %q, want olleh", got)
	}
	// Byte reversal of multi-byte input is (documented to be) invalid UTF-8.
	if utf8.ValidString(Reverse("abç")) {
		t.Fatal("Reverse(abç) unexpectedly valid UTF-8; byte behavior changed")
	}
}

func TestReverseIsMarkedDeprecated(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "strings.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	pkg, err := doc.NewFromFiles(fset, []*ast.File{f}, "example.com/publicstr")
	if err != nil {
		t.Fatalf("doc.NewFromFiles: %v", err)
	}
	var reverseDoc string
	for _, fn := range pkg.Funcs {
		if fn.Name == "Reverse" {
			reverseDoc = fn.Doc
		}
	}
	if reverseDoc == "" {
		t.Fatal("Reverse has no doc comment")
	}
	// The convention: a paragraph beginning with "Deprecated:" so gopls,
	// staticcheck (SA1019), and pkg.go.dev flag uses.
	if !strings.Contains(reverseDoc, "\nDeprecated:") {
		t.Fatalf("Reverse doc lacks a 'Deprecated:' paragraph:\n%s", reverseDoc)
	}
}
```

## Review

The migration is correct when `ReverseRunes` reverses multi-byte input and round
trips while `Reverse` keeps its documented byte behavior — proving nothing changed
under existing callers — and when the `Deprecated:` paragraph is present so tooling
steers new uses to the successor. The mistake this exercise trains against is the
tempting "just fix `Reverse` to be rune-safe": that silently changes behavior for
every caller who (correctly, per the old docs) relied on byte reversal, which is a
breaking change disguised as a bug fix. Equally wrong is deleting `Reverse`, which
forces a major bump. The deprecation path costs you nothing at the type level and
lets consumers migrate on their own schedule. Note the marker must be its own
paragraph beginning with `Deprecated:` — the leading blank comment line is what
makes `go/doc` and staticcheck recognize it.

## Resources

- [Go Doc Comments: Deprecations](https://go.dev/doc/comment) — the exact `Deprecated:` paragraph convention.
- [staticcheck SA1019](https://staticcheck.dev/docs/checks#SA1019) — the check that flags uses of deprecated symbols.
- [`unicode/utf8`](https://pkg.go.dev/unicode/utf8) — `ValidString` and rune-boundary handling.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [06-shrink-the-surface-with-internal-packages.md](06-shrink-the-surface-with-internal-packages.md) | Next: [08-gate-releases-with-an-api-compat-check.md](08-gate-releases-with-an-api-compat-check.md)
