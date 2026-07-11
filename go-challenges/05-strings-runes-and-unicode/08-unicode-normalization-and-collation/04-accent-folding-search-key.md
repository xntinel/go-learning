# Exercise 4: The Correct Fold-to-ASCII Search Key (NFD to strip Mn to NFC)

Exercise 1's normalizer had one honest blind spot: precomposed NFC input kept its
accent, so `café` typed as one code point and `café` typed as two produced
different keys. This module builds the production-grade replacement — the
decompose, drop-the-marks, recompose transformer chain — that closes that gap for
every script, not just Latin, and reuses one prebuilt transformer across all calls.

This module is fully self-contained: its own `go mod init`, its own demo and tests
and benchmark. It uses `golang.org/x/text`.

## What you'll build

```text
foldkey/                    independent module: example.com/foldkey
  go.mod                    requires golang.org/x/text
  foldkey.go                FoldKey via transform.Chain(NFD, Remove(Mn), NFC)
  cmd/demo/main.go          fold NFC and NFD spellings to one ASCII key
  foldkey_test.go           closes the NFC gap; NFC==NFD; reuse-safe; benchmark
```

Files: `foldkey.go`, `cmd/demo/main.go`, `foldkey_test.go`.
Implement: a package-level `transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)` and `FoldKey(string) (string, error)` applying it with `transform.String`.
Test: the NFC input that FAILED in Exercise 1 now folds to `cafe`; `FoldKey(NFC) == FoldKey(NFD)` for accented pairs; the transformer is safe to reuse across calls; a benchmark against a manual range strip.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/foldkey/cmd/demo
cd ~/go-exercises/foldkey
go mod init example.com/foldkey
go get golang.org/x/text
```

### The three-stage chain, and why each stage is needed

The goal is a key that ignores both case and accents and is identical no matter
which Unicode form the accent arrived in. The recipe:

```
transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
```

- `norm.NFD` decomposes every precomposed accented letter into a base letter plus
  a standalone combining mark. This is the stage Exercise 1 lacked: a precomposed
  `é` (U+00E9) has no mark to remove *until* it is decomposed into `e` + U+0301.
- `runes.Remove(runes.In(unicode.Mn))` drops every rune in general category **Mn**
  (nonspacing mark). Using the `unicode.Mn` category — not the Latin literal range
  `U+0300..U+036F` — is what makes this correct for Greek, Cyrillic, Vietnamese,
  and every other script with combining marks.
- `norm.NFC` recomposes what survives, so the base letters come back in canonical
  form (relevant when a base letter itself had multiple marks, only some removed).

`FoldKey` lowercases first (so the key is case-insensitive too) and then runs the
chain with `transform.String`. Two properties matter for using it in a service.
First, a `transform.Transformer` is *stateful* (it buffers partial multi-byte
sequences), so a single shared chain is **not** safe for concurrent use —
`transform.String` calls `Reset` on it, and two goroutines resetting and driving
the same chain at once is a data race (`go test -race` will prove it). Second,
constructing a fresh `Chain` on every call allocates, and a search key is on the
hot path. The pattern that satisfies both is a `sync.Pool` of chains: each call
borrows a transformer, uses it, and returns it, so transformers are reused under
load without ever being shared concurrently. This is the idiomatic way to reuse a
stateful `x/text` transformer in a concurrent service.

Create `foldkey.go`:

```go
package foldkey

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// newAccentFold builds a chain that decomposes to NFD, drops every nonspacing
// mark (category Mn), and recomposes to NFC. Each chain is stateful, so we pool
// them rather than sharing one.
func newAccentFold() transform.Transformer {
	return transform.Chain(
		norm.NFD,
		runes.Remove(runes.In(unicode.Mn)),
		norm.NFC,
	)
}

// foldPool hands out per-call transformers so FoldKey is concurrency-safe and
// still reuses chains under load instead of allocating one per call.
var foldPool = sync.Pool{New: func() any { return newAccentFold() }}

// FoldKey returns a case- and accent-insensitive search key. Unlike a manual
// U+0300..U+036F strip, it collapses precomposed NFC and decomposed NFD input to
// the same key, for any script. Safe to call from multiple goroutines.
func FoldKey(s string) (string, error) {
	t := foldPool.Get().(transform.Transformer)
	defer foldPool.Put(t)
	// transform.String resets t before use, so a pooled transformer is clean.
	out, _, err := transform.String(t, strings.ToLower(s))
	if err != nil {
		return "", err
	}
	return out, nil
}
```

### The runnable demo

The demo folds the exact NFC input that Exercise 1 could not handle, alongside its
NFD twin, and shows both land on the same ASCII key.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/foldkey"
)

func main() {
	nfc := "Caf" + string(rune(0x00E9))                        // Café precomposed (the Ex.1 gap)
	nfd := "Caf" + string(rune(0x0065)) + string(rune(0x0301)) // Cafe + combining acute

	ka, _ := foldkey.FoldKey(nfc)
	kb, _ := foldkey.FoldKey(nfd)
	fmt.Printf("NFC key: %q\n", ka)
	fmt.Printf("NFD key: %q\n", kb)
	fmt.Printf("same key: %v\n", ka == kb)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
NFC key: "cafe"
NFD key: "cafe"
same key: true
```

### Tests

The headline test is the one that Exercise 1 failed: a *precomposed* `café` now
folds to `cafe`. Then a table asserts `FoldKey(NFC) == FoldKey(NFD)` for several
pairs, a reuse test hammers the shared chain sequentially to prove `transform.String`
resets it correctly, and a benchmark quantifies the cost against the Latin-only
manual strip so the trade-off is explicit.

Create `foldkey_test.go`:

```go
package foldkey

import (
	"strings"
	"sync"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// manualStrip is the Exercise 1 Latin-only approach, kept for the benchmark and to
// demonstrate the gap this module closes.
func manualStrip(s string) string {
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

func TestClosesNFCGap(t *testing.T) {
	t.Parallel()

	nfc := "Caf" + string(rune(0x00E9)) // precomposed café: the input Ex.1 left alone
	got, err := FoldKey(nfc)
	if err != nil {
		t.Fatalf("FoldKey: %v", err)
	}
	if got != "cafe" {
		t.Fatalf("FoldKey(NFC café) = %q, want cafe", got)
	}
	// Prove the manual strip really did fail on this same input.
	if manualStrip(nfc) == "cafe" {
		t.Fatal("manual strip unexpectedly folded precomposed NFC; the gap is not real")
	}
}

func TestFoldKeyNFCEqualsNFD(t *testing.T) {
	t.Parallel()

	words := []string{
		"caf" + string(rune(0x00E9)),        // café
		"nai" + string(rune(0x0308)) + "ve", // naïve
		string(rune(0x00DC)) + "ber",        // Über
	}
	for _, w := range words {
		nfcKey, err := FoldKey(norm.NFC.String(w))
		if err != nil {
			t.Fatalf("FoldKey(NFC): %v", err)
		}
		nfdKey, err := FoldKey(norm.NFD.String(w))
		if err != nil {
			t.Fatalf("FoldKey(NFD): %v", err)
		}
		if nfcKey != nfdKey {
			t.Fatalf("FoldKey diverged: NFC %q vs NFD %q", nfcKey, nfdKey)
		}
	}
}

func TestChainIsReusable(t *testing.T) {
	t.Parallel()

	// Reuse pooled chains across many sequential calls; if transform.String did
	// not Reset a borrowed transformer, leftover state would corrupt later keys.
	inputs := []string{
		"caf" + string(rune(0x00E9)),
		"RESUME",
		"nai" + string(rune(0x0308)) + "ve",
		"plain",
	}
	want := []string{"cafe", "resume", "naive", "plain"}
	for range 3 {
		for i, in := range inputs {
			got, err := FoldKey(in)
			if err != nil {
				t.Fatalf("FoldKey(%q): %v", in, err)
			}
			if got != want[i] {
				t.Fatalf("FoldKey(%q) = %q, want %q", in, got, want[i])
			}
		}
	}
}

func TestConcurrentFold(t *testing.T) {
	t.Parallel()

	// Many goroutines folding at once must be race-free (proven under -race)
	// and must all agree on the key, because each borrows its own transformer.
	in := "R" + string(rune(0x00E9)) + "sum" + string(rune(0x00E9)) // Résumé
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := FoldKey(in)
			if err != nil || got != "resume" {
				t.Errorf("FoldKey = %q, err %v; want resume", got, err)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkFoldKey(b *testing.B) {
	in := "R" + string(rune(0x00E9)) + "sum" + string(rune(0x00E9)) // Résumé (NFC)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = FoldKey(in)
	}
}

func BenchmarkManualStrip(b *testing.B) {
	in := "Re" + string(rune(0x0301)) + "sume" + string(rune(0x0301)) // NFD
	b.ReportAllocs()
	for b.Loop() {
		_ = manualStrip(in)
	}
}
```

## Review

The fold key is correct when a base letter's accent never survives regardless of
the form it arrived in: `FoldKey` decomposes with `norm.NFD`, removes the `unicode.Mn`
category, and recomposes with `norm.NFC`, so a precomposed `é` and a decomposed
`e` + U+0301 both reduce to `e`. That is exactly the input the Exercise 1 manual
strip left untouched, which `TestClosesNFCGap` demonstrates from both directions.
The trade-off the benchmark makes explicit: the chain does real work (decompose and
recompose) and allocates, where the manual strip is a single cheap pass — but the
manual strip is only correct for already-decomposed Latin, so you pay the chain's
cost precisely when you need correctness beyond that. The concurrency test is the
subtle one: a `transform.Transformer` is stateful, so `FoldKey` borrows one per
call from a `sync.Pool` rather than sharing a single global chain — that reuses
transformers under load while staying race-free, which `TestConcurrentFold` proves
under `-race`.

## Resources

- [`golang.org/x/text/transform`](https://pkg.go.dev/golang.org/x/text/transform) — `Chain`, `String`, and the `Transformer` reset contract.
- [`golang.org/x/text/runes`](https://pkg.go.dev/golang.org/x/text/runes) — `Remove`, `In`.
- [`golang.org/x/text/unicode/norm`](https://pkg.go.dev/golang.org/x/text/unicode/norm) — `NFD`, `NFC`.
- [The Go Blog: Text normalization in Go](https://go.dev/blog/normalization) — the decompose/remove-marks/recompose pattern.

---

Back to [03-canonical-nfc-at-the-storage-boundary.md](03-canonical-nfc-at-the-storage-boundary.md) | Next: [05-locale-aware-case-folding-for-identifiers.md](05-locale-aware-case-folding-for-identifiers.md)
