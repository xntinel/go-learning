# Exercise 5: Normalize to NFC to Stop Canonical-Equivalence Collisions

Unicode lets the same visible text be encoded more than one way, and two
byte-different spellings of "café" compare and hash unequal — so a `UNIQUE`
constraint or a dedup map lets both through. This module builds the NFC
normalization transform that collapses canonical-equivalent spellings to one
storage form before any value becomes a key.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise. It uses `golang.org/x/text/unicode/norm`; the gate fetches it.

## What you'll build

```text
nfckey/                   independent module: example.com/nfckey
  go.mod                  go 1.26 (requires golang.org/x/text)
  nfckey.go               NormalizeNFC transform; DedupKeys helper
  cmd/
    demo/
      main.go             runnable demo of the composed/decomposed café collision
  nfckey_test.go          byte-difference, dedup, and idempotence tests
```

- Files: `nfckey.go`, `cmd/demo/main.go`, `nfckey_test.go`.
- Implement: `NormalizeNFC(string) string` (a `Transform` wrapping `norm.NFC.String`) and `DedupKeys([]string) []string` that normalizes then deduplicates preserving first-seen order.
- Test: the two byte-different café encodings are unequal before normalization and byte-equal after; a `map`-based dedup over a slice containing both collapses to one entry only after normalization; NFC is idempotent.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/05-nfc-normalization-for-dedup-keys/cmd/demo
cd go-solutions/05-strings-runes-and-unicode/10-building-a-text-processing-pipeline/05-nfc-normalization-for-dedup-keys
go get golang.org/x/text/unicode/norm
```

### The collision, precisely

"café" can end in one precomposed code point, U+00E9 (`é`, UTF-8 bytes `c3 a9`),
or in the base letter `e` (U+0065) followed by a combining acute accent U+0301
(bytes `65 cc 81`). Both render identically. Both are legitimate Unicode. But
`"café" == "café"` is `false`, their `map` keys differ, and a database
`UNIQUE(username)` sees two distinct values. A signup form, a copy-paste from a
word processor, and a mobile keyboard can each emit a different form for the same
name, so this is not theoretical — it is the "duplicate that looks identical in
every log" bug.

Normalization Form C (NFC) is the composed canonical form: it recomposes base +
combining sequences into precomposed code points where one exists. `norm.NFC` is a
`norm.Form` value; `norm.NFC.String(s)` returns the NFC form of `s`. After
normalizing both spellings to NFC they are byte-equal, so they collide correctly
in a map or a unique index. NFC is idempotent — `norm.NFC.String` of an already-NFC
string returns it unchanged — which is what makes it safe to apply on every write
without drift.

The rule for the codebase: normalize to NFC at the boundary, before any value
becomes a dedup input, a map key, a cache key, or an indexed/unique column. Store
the NFC form. Then equality means what a human means by "the same text."

Create `nfckey.go`:

```go
package nfckey

import "golang.org/x/text/unicode/norm"

// NormalizeNFC is a Transform that rewrites its input into Unicode Normalization
// Form C, the composed canonical form used as the storage canonical.
func NormalizeNFC(s string) string {
	return norm.NFC.String(s)
}

// DedupKeys normalizes each key to NFC and returns the unique keys in first-seen
// order. Canonical-equivalent spellings collapse to a single entry.
func DedupKeys(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		n := NormalizeNFC(k)
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/nfckey"
)

func main() {
	// composed ends in one code point U+00E9; decomposed ends in e + U+0301.
	composed := "café"
	decomposed := "café"

	fmt.Printf("raw equal: %v\n", composed == decomposed)
	fmt.Printf("raw bytes: % x vs % x\n", []byte(composed), []byte(decomposed))

	fmt.Printf("NFC equal: %v\n",
		nfckey.NormalizeNFC(composed) == nfckey.NormalizeNFC(decomposed))

	unique := nfckey.DedupKeys([]string{composed, decomposed, "tea"})
	fmt.Printf("dedup count: %d\n", len(unique))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
raw equal: false
raw bytes: 63 61 66 c3 a9 vs 63 61 66 65 cc 81
NFC equal: true
dedup count: 2
```

### Tests

`TestCanonicalEquivalence` pins the whole bug and its fix: the two spellings are
unequal raw and equal after NFC. `TestDedupCollapsesAfterNFC` proves a map-based
dedup lets both through when unnormalized but collapses them once normalized.
`TestIdempotent` checks `NFC(NFC(s)) == NFC(s)` across a sample.

Create `nfckey_test.go`:

```go
package nfckey

import (
	"fmt"
	"testing"
)

// composed ends in U+00E9; decomposed ends in e + combining acute U+0301.
const (
	composed   = "café"
	decomposed = "café"
)

func TestCanonicalEquivalence(t *testing.T) {
	t.Parallel()

	if composed == decomposed {
		t.Fatal("test fixtures are not byte-different; the collision cannot be shown")
	}
	if NormalizeNFC(composed) != NormalizeNFC(decomposed) {
		t.Fatalf("NFC did not unify %q and %q", composed, decomposed)
	}
}

func TestNaiveDedupMissesCollision(t *testing.T) {
	t.Parallel()

	// Without normalization, a map keyed on raw strings keeps both.
	raw := map[string]struct{}{}
	for _, k := range []string{composed, decomposed} {
		raw[k] = struct{}{}
	}
	if len(raw) != 2 {
		t.Fatalf("raw dedup size = %d, want 2 (the bug)", len(raw))
	}
}

func TestDedupCollapsesAfterNFC(t *testing.T) {
	t.Parallel()

	got := DedupKeys([]string{composed, decomposed, "tea", "tea"})
	if len(got) != 2 {
		t.Fatalf("DedupKeys size = %d (%q), want 2", len(got), got)
	}
	// First-seen order is preserved and the survivor is the NFC form.
	if got[0] != NormalizeNFC(composed) {
		t.Fatalf("first key = %q, want the NFC café", got[0])
	}
	if got[1] != "tea" {
		t.Fatalf("second key = %q, want %q", got[1], "tea")
	}
}

func TestIdempotent(t *testing.T) {
	t.Parallel()

	for _, s := range []string{composed, decomposed, "plain", "日本語", ""} {
		once := NormalizeNFC(s)
		twice := NormalizeNFC(once)
		if once != twice {
			t.Fatalf("NFC not idempotent for %q: %q vs %q", s, once, twice)
		}
	}
}

func ExampleNormalizeNFC() {
	fmt.Println(NormalizeNFC("café") == NormalizeNFC("café"))
	// Output: true
}
```

## Review

The normalization is correct when canonical-equivalent spellings become
byte-equal after NFC and a dedup keyed on the NFC form collapses them, while the
raw-keyed dedup demonstrably does not. The mistake to avoid is deduping or
enforcing uniqueness on raw user text: NFC vs NFD duplicates slip past a `UNIQUE`
index or a map key precisely because they render identically, so nobody notices
until two "identical" rows appear. Normalize to NFC at the boundary and store that
form; because NFC is idempotent, applying it on every write is safe and drift-free.
Run `go test -race` to confirm the map access in `DedupKeys` is clean.

## Resources

- [golang.org/x/text/unicode/norm](https://pkg.go.dev/golang.org/x/text/unicode/norm) — `Form`, `NFC`, `NFD`, and `String`/`Bytes`.
- [Unicode Standard Annex #15: Normalization Forms](https://unicode.org/reports/tr15/) — the definition of canonical equivalence and NFC/NFD.
- [The Go Blog: text normalization in Go](https://go.dev/blog/normalization) — why and how to normalize with `x/text`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [04-utf8-validation-and-repair.md](04-utf8-validation-and-repair.md) | Next: [06-diacritic-folding-slug-generator.md](06-diacritic-folding-slug-generator.md)
