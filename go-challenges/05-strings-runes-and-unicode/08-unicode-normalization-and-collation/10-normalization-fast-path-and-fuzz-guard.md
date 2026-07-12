# Exercise 10: Hot-Path Fast Path and a Fuzz Guard for the Normalizer

Correctness must not cost throughput. In a high-QPS service the overwhelming
majority of inputs are already ASCII or already NFC, so paying a full transform and
its allocation on every request is waste. This final module adds a fast path that
short-circuits the already-normal case with zero allocation, and a Go fuzz harness
that guards the two invariants the fast path must never break: idempotency and
no-panic on arbitrary bytes.

This module is fully self-contained: its own `go mod init`, its own demo, tests,
benchmarks, and fuzz target. It uses `golang.org/x/text`.

## What you'll build

```text
foldfast/                   independent module: example.com/foldfast
  go.mod                    requires golang.org/x/text
  foldfast.go               FastNFC (QuickSpan short-circuit), FoldKey (ASCII fast path)
  cmd/demo/main.go          show the fast path returns the input unchanged
  foldfast_test.go          zero-alloc fast path; fuzz idempotency + no panic; benchmarks
```

Files: `foldfast.go`, `cmd/demo/main.go`, `foldfast_test.go`.
Implement: `FastNFC` using `norm.NFC.QuickSpanString`/`IsNormalString` to skip the transform, and `FoldKey` with an ASCII-lowercase fast path over the pooled fold chain.
Test: `AllocsPerRun` shows the fast path allocates nothing on already-normal input; a `testing.F` fuzz target asserts `FoldKey(FoldKey(x)) == FoldKey(x)` and `norm.NFC.String` never panics on arbitrary bytes; benchmarks quantify the win.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get golang.org/x/text
```

### The fast path, and why fuzzing guards it

`norm.NFC.IsNormalString(s)` reports whether `s` is already NFC;
`norm.NFC.QuickSpanString(s)` returns the length of the already-normal prefix. Both
let a hot path detect "there is nothing to do" and return the input unchanged with
zero allocation. `FastNFC` uses `QuickSpanString`: if the whole string is already
normal (`n == len(s)`), return it as-is; only otherwise pay for `norm.NFC.String`.
When production traffic is overwhelmingly already-NFC, this turns the common case
into a cheap scan.

`FoldKey` gets an analogous fast path at the fold level: a string that is pure
ASCII with no uppercase letters has no case to fold and no accent to strip, so it is
already its own fold key — return it directly, no transform, no allocation. Only
strings with a high byte or an uppercase letter take the full pooled
decompose/strip/recompose chain from Exercise 4.

The danger of a fast path is that it is a *second* code path, and a second code path
can disagree with the first. If `FoldKey` returns `s` unchanged on the fast path but
the slow path would have changed it, the key is wrong and two records silently
diverge. The invariant that catches this is **idempotency**: `FoldKey(FoldKey(x))`
must equal `FoldKey(x)` for every input — the output of a fold is already folded, so
re-folding is a no-op, and if the fast path ever returns something the slow path
would fold further, idempotency breaks. A Go fuzz target asserts exactly this over
arbitrary strings, including invalid UTF-8, and simultaneously asserts that
`norm.NFC.String` never panics on random bytes. Fuzzing is the right tool here
because the input space is enormous and adversarial, and the property is simple and
universal.

Create `foldfast.go`:

```go
package foldfast

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// FastNFC returns the NFC form of s, skipping the transform (and its allocation)
// when s is already normal. QuickSpanString returns the length of the already-NFC
// prefix; if that covers the whole string, s is returned unchanged.
func FastNFC(s string) string {
	if norm.NFC.QuickSpanString(s) == len(s) {
		return s
	}
	return norm.NFC.String(s)
}

func newAccentFold() transform.Transformer {
	return transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
}

var foldPool = sync.Pool{New: func() any { return newAccentFold() }}

// isASCIIFolded reports whether s is pure ASCII with no uppercase letter, meaning
// it has nothing to case-fold and no combining mark to strip: it is its own key.
func isASCIIFolded(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x80 || (c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// FoldKey returns a case- and accent-insensitive search key. The fast path returns
// already-folded ASCII input unchanged with zero allocation; other input takes the
// full pooled decompose/strip/recompose chain. Safe for concurrent use.
func FoldKey(s string) (string, error) {
	if isASCIIFolded(s) {
		return s, nil
	}
	t := foldPool.Get().(transform.Transformer)
	defer foldPool.Put(t)
	out, _, err := transform.String(t, strings.ToLower(s))
	if err != nil {
		return "", err
	}
	return out, nil
}
```

### The runnable demo

The demo shows the fast path returning the exact same string it was given for an
already-normal ASCII key, and the slow path folding an accented, mixed-case input.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/foldfast"
)

func main() {
	fast, _ := foldfast.FoldKey("already-folded-key")
	fmt.Printf("fast path: %q\n", fast)

	slow, _ := foldfast.FoldKey("Caf" + string(rune(0x00C9))) // CafÉ (NFC)
	fmt.Printf("slow path: %q\n", slow)

	fmt.Printf("FastNFC leaves NFC unchanged: %v\n",
		foldfast.FastNFC("plain") == "plain")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast path: "already-folded-key"
slow path: "cafe"
FastNFC leaves NFC unchanged: true
```

### Tests

`TestFastPathNoAlloc` uses `testing.AllocsPerRun` to prove the fast paths allocate
nothing on already-normal input. `FuzzFoldKey` is the guard: for any input, folding
twice equals folding once, and `norm.NFC.String` does not panic. The benchmarks
quantify the fast-path win.

Create `foldfast_test.go`:

```go
package foldfast

import (
	"testing"

	"golang.org/x/text/unicode/norm"
)

func TestFastPathNoAlloc(t *testing.T) {
	// AllocsPerRun must not run in a parallel test (it needs a stable GOMAXPROCS).
	if a := testing.AllocsPerRun(100, func() {
		_, _ = FoldKey("already-folded-search-key")
	}); a != 0 {
		t.Fatalf("FoldKey fast path allocated %v times, want 0", a)
	}
	if a := testing.AllocsPerRun(100, func() {
		_ = FastNFC("an already normal ascii string")
	}); a != 0 {
		t.Fatalf("FastNFC fast path allocated %v times, want 0", a)
	}
}

func TestFastPathAgreesWithSlowPath(t *testing.T) {
	t.Parallel()

	// An accented input must NOT take the fast path and must fold fully.
	got, err := FoldKey("Caf" + string(rune(0x00C9))) // CafÉ
	if err != nil {
		t.Fatalf("FoldKey: %v", err)
	}
	if got != "cafe" {
		t.Fatalf("FoldKey(CafÉ) = %q, want cafe", got)
	}
	// FastNFC of an NFD string must still normalize.
	nfd := "cafe" + string(rune(0x0301))
	if FastNFC(nfd) != norm.NFC.String(nfd) {
		t.Fatal("FastNFC failed to normalize NFD input")
	}
}

func FuzzFoldKey(f *testing.F) {
	f.Add("café")
	f.Add("STRAßE")
	f.Add("already-folded")
	f.Add(string([]byte{0xff, 0xfe, 0x00}))
	f.Add("")

	f.Fuzz(func(t *testing.T, s string) {
		k1, err := FoldKey(s)
		if err != nil {
			return // a rejected input is acceptable; only success must be idempotent
		}
		k2, err := FoldKey(k1)
		if err != nil {
			t.Fatalf("second FoldKey(%q) errored: %v", k1, err)
		}
		if k1 != k2 {
			t.Fatalf("FoldKey not idempotent: %q -> %q -> %q", s, k1, k2)
		}
		// NFC must be idempotent and must never panic on arbitrary bytes.
		n := norm.NFC.String(s)
		if norm.NFC.String(n) != n {
			t.Fatalf("NFC not idempotent for %q", s)
		}
	})
}

func BenchmarkFoldKeyFastPath(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = FoldKey("already-folded-search-key")
	}
}

func BenchmarkFoldKeySlowPath(b *testing.B) {
	in := "R" + string(rune(0x00C9)) + "SUM" + string(rune(0x00C9)) // RÉSUMÉ (NFC)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = FoldKey(in)
	}
}
```

## Review

The fast path is correct only if it is indistinguishable from the slow path, and
this module proves that two ways: `TestFastPathAgreesWithSlowPath` checks that
accented input still folds to the ASCII key (it must not slip through the fast
path), and `FuzzFoldKey` asserts idempotency across an adversarial input space,
which is precisely the property a divergent fast path would violate. `TestFastPathNoAlloc`
confirms the optimization is real — zero allocations when there is nothing to do.
The mistakes to avoid: gating the fast path on a check that is cheaper than correct
(an ASCII-with-no-uppercase test is both), and shipping a fast path with no fuzz
guard — the whole point of the second code path is that it is easy to get subtly
wrong, so the invariant test is not optional. This closes the lesson: you now have a
correct fold and NFC pipeline that also stays cheap on the traffic that dominates
production.

## Resources

- [`golang.org/x/text/unicode/norm`](https://pkg.go.dev/golang.org/x/text/unicode/norm) — `IsNormalString`, `QuickSpanString`, `String`.
- [`testing` fuzzing](https://pkg.go.dev/testing#F) — `testing.F`, `F.Add`, `F.Fuzz`.
- [Go Fuzzing](https://go.dev/security/fuzz/) — writing and running fuzz targets.
- [`testing.AllocsPerRun`](https://pkg.go.dev/testing#AllocsPerRun) — measuring allocations on the fast path.

---

Back to [09-streaming-normalization-for-ingestion.md](09-streaming-normalization-for-ingestion.md) | Next: [../09-strings-builder-performance/00-concepts.md](../09-strings-builder-performance/00-concepts.md)
