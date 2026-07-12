# Exercise 9: Combined Round-Trip and Ordering Properties of a Base62 ID Codec

A base62 codec turns a `uint64` ID into a short, URL-safe string and back — the
kind of thing behind every shortened link and external reference. This final
exercise combines several property families on one artifact: a round-trip, an
output invariant, and an order-preservation property that a naive encoder silently
violates. It also gives the round-trip in `testing/quick` alongside the `rapid`
version, so the difference between "shrinks to a tiny counterexample" and "hands you
the raw failing input" is concrete.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
idcodec/                    independent module: example.com/idcodec
  go.mod                    go 1.26, requires pgregory.net/rapid
  idcodec.go                Encode, EncodeSortable, Decode over base62; alphabet + reverse table
  cmd/
    demo/
      main.go               runnable demo: encode, decode, compare sortable order
  idcodec_test.go           rapid round-trip/invariant/ordering + a testing/quick round-trip
```

Files: `idcodec.go`, `cmd/demo/main.go`, `idcodec_test.go`.
Implement: `Encode` (minimal-width base62), `EncodeSortable` (fixed 11-digit, zero-padded), and `Decode` with overflow rejection, plus a reverse-lookup table.
Test: rapid properties — round-trip for both encoders, an alphabet-and-length invariant, and order preservation for the sortable form — plus the same round-trip in `testing/quick`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/24-property-based-testing/09-roundtrip-and-bounds-id-codec/cmd/demo
cd go-solutions/12-testing-ecosystem/24-property-based-testing/09-roundtrip-and-bounds-id-codec
go mod edit -go=1.26
go get pgregory.net/rapid@latest
```

### Two encoders, three property families, and why ordering needs padding

The alphabet is the 62 digits `0-9A-Za-z`, in ascending ASCII order — that ordering
is load-bearing for the sortability property below. `Encode` produces the minimal
representation: no leading zeros, so `0` encodes to `"0"` and small numbers to short
strings. `Decode` walks the string, mapping each byte through a reverse-lookup table
and accumulating base-62, rejecting any non-alphabet byte and any string whose value
would overflow `uint64` (a robustness property, since arbitrary strings can exceed
the range). Eleven digits suffice for any `uint64` because 62^11 exceeds 2^64.

Three property families pin the codec:

Round-trip: `Decode(Encode(n)) == n` and `ok`, for every `uint64` including 0,
`math.MaxUint64`, and the powers-of-two and base boundaries (61, 62, 63) where
digit carrying is most error-prone. This is the canonical codec property.

Invariant: `Encode`'s output uses only the alphabet and has length between 1 and 11;
`EncodeSortable`'s output is exactly 11. Asserted directly on the output, regardless
of input.

Metamorphic ordering: this is the interesting one. If a codec claims its encoding is
lexicographically sortable — so a database or object store can sort by the string key
and get numeric order — then `a < b` must imply `Encode(a) < Encode(b)` *lexically*.
The minimal `Encode` **fails** this: `Encode(61)` is `"z"` and `Encode(62)` is
`"10"`, and `"z" > "1"` lexically, so `61 < 62` but `"z" > "10"`. The fix is fixed-width,
zero-padded encoding: `EncodeSortable` always emits 11 digits, so every encoding has
the same length and lexicographic comparison reduces to positional numeric comparison
— and because the alphabet is in ascending ASCII order, that equals numeric order. The
ordering property holds for `EncodeSortable` and would shrink to the tiny pair `(61,
62)` for the unpadded `Encode`. This is why sortable ID schemes (and lexicographic
key encodings generally) pad to a fixed width; skipping the padding is a real,
shipped bug.

Create `idcodec.go`:

```go
package idcodec

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// width is the number of base62 digits needed for any uint64: 62^11 > 2^64.
const width = 11

// rev maps an ASCII byte to its base62 digit value, or -1 if not in the alphabet.
var rev [128]int8

func init() {
	for i := range rev {
		rev[i] = -1
	}
	for i := 0; i < len(alphabet); i++ {
		rev[alphabet[i]] = int8(i)
	}
}

// Encode returns the minimal-width base62 encoding of n (no leading zeros).
func Encode(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [width]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = alphabet[n%62]
		n /= 62
	}
	return string(buf[i:])
}

// EncodeSortable returns the fixed-width, zero-padded base62 encoding of n. Because
// every output is exactly width digits over an ascending alphabet, lexicographic
// order of the encodings matches numeric order of the values.
func EncodeSortable(n uint64) string {
	var buf [width]byte
	for i := width - 1; i >= 0; i-- {
		buf[i] = alphabet[n%62]
		n /= 62
	}
	return string(buf[:])
}

// Decode parses a base62 string back to a uint64. It rejects (ok=false) any byte
// outside the alphabet and any value that would overflow uint64.
func Decode(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 128 || rev[c] < 0 {
			return 0, false
		}
		d := uint64(rev[c])
		if n > (^uint64(0)-d)/62 {
			return 0, false // would overflow uint64
		}
		n = n*62 + d
	}
	return n, true
}
```

### The runnable demo

The demo encodes a couple of IDs both ways and shows the ordering trap directly:
the minimal encoding of 61 versus 62 sorts wrong, the padded one sorts right.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/idcodec"
)

func main() {
	n := uint64(1234567890)
	enc := idcodec.Encode(n)
	dec, _ := idcodec.Decode(enc)
	fmt.Printf("%d -> %q -> %d\n", n, enc, dec)

	fmt.Printf("minimal:  61=%q 62=%q sorted-ok=%v\n",
		idcodec.Encode(61), idcodec.Encode(62),
		idcodec.Encode(61) < idcodec.Encode(62))
	fmt.Printf("sortable: 61=%q 62=%q sorted-ok=%v\n",
		idcodec.EncodeSortable(61), idcodec.EncodeSortable(62),
		idcodec.EncodeSortable(61) < idcodec.EncodeSortable(62))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
1234567890 -> "1LY7VK" -> 1234567890
minimal:  61="z" 62="10" sorted-ok=false
sortable: 61="0000000000z" 62="00000000010" sorted-ok=true
```

The `minimal` line is the bug the ordering property catches; the `sortable` line is
the fix.

### The property tests

The `uint64` generator mixes `rapid.Uint64()` (full range, including large values)
with a `SampledFrom` of hand-picked boundaries — 0, the base edges 61/62/63,
`MaxUint64`, and a power of two — so the carry-heavy cases are always hit. The
round-trip and invariant properties cover both encoders; the ordering property
covers `EncodeSortable`; a slice property confirms that sorting the numbers and
sorting their sortable encodings agree, using `slices.IsSorted`. Finally
`TestRoundTripQuick` states the same round-trip in `testing/quick`: it works, but if
it ever failed it would print a raw failing `uint64` with no minimization — the
contrast that makes rapid's shrinking concrete.

Create `idcodec_test.go`:

```go
package idcodec

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"pgregory.net/rapid"
)

func genU64() *rapid.Generator[uint64] {
	return rapid.OneOf(
		rapid.Uint64(),
		rapid.SampledFrom([]uint64{0, 1, 61, 62, 63, math.MaxUint64, 1 << 32}),
	)
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		n := genU64().Draw(t, "n")
		for _, enc := range []func(uint64) string{Encode, EncodeSortable} {
			s := enc(n)
			d, ok := Decode(s)
			if !ok || d != n {
				t.Fatalf("Decode(%q)=%d,%v want %d", s, d, ok, n)
			}
		}
	})
}

func TestInvariant(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		n := genU64().Draw(t, "n")
		e := Encode(n)
		if len(e) < 1 || len(e) > width {
			t.Fatalf("Encode(%d) length %d out of [1,%d]", n, len(e), width)
		}
		for i := 0; i < len(e); i++ {
			if !strings.ContainsRune(alphabet, rune(e[i])) {
				t.Fatalf("Encode(%d)=%q contains a non-alphabet byte", n, e)
			}
		}
		if len(EncodeSortable(n)) != width {
			t.Fatalf("EncodeSortable(%d) length %d, want %d", n, len(EncodeSortable(n)), width)
		}
	})
}

func TestSortableOrdering(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		a := genU64().Draw(t, "a")
		b := genU64().Draw(t, "b")
		if a < b && EncodeSortable(a) >= EncodeSortable(b) {
			t.Fatalf("order broken: %d<%d but %q>=%q",
				a, b, EncodeSortable(a), EncodeSortable(b))
		}
	})
}

func TestSortableSortsSlice(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		ns := rapid.SliceOfN(rapid.Uint64(), 0, 8).Draw(t, "ns")
		slices.Sort(ns)
		encs := make([]string, len(ns))
		for i, n := range ns {
			encs[i] = EncodeSortable(n)
		}
		if !slices.IsSorted(encs) {
			t.Fatalf("sorted %v but encodings %v are not sorted", ns, encs)
		}
	})
}

// TestRoundTripQuick states the same round-trip in testing/quick. It passes, but a
// failure would print a raw uint64 with no shrinking — the contrast with rapid.
func TestRoundTripQuick(t *testing.T) {
	t.Parallel()
	prop := func(n uint64) bool {
		d, ok := Decode(Encode(n))
		return ok && d == n
	}
	if err := quick.Check(prop, &quick.Config{MaxCount: 10000}); err != nil {
		t.Fatal(err)
	}
}

func ExampleEncodeSortable() {
	fmt.Println(EncodeSortable(61) < EncodeSortable(62))
	// Output: true
}
```

## Review

The codec is correct when both encoders round-trip every `uint64`, the outputs stay
within the alphabet and length bounds, and the sortable encoding preserves numeric
order lexically. Combining the families on one artifact is the lesson: a single type
carries a round-trip property, an invariant, and a metamorphic ordering property, and
each guards a different real failure — corruption, malformed output, and a
sort-by-key that returns rows out of order.

The mistakes to avoid are encoder-specific. First, do not assume a minimal encoding
is sortable: variable-length base62 sorts wrong at every power-of-base boundary, and
the ordering property shrinks straight to `(61, 62)` to prove it — pad to a fixed
width when the encoding must sort. Second, decode defensively: arbitrary strings can
contain non-alphabet bytes or exceed `uint64`, so `Decode` must reject rather than
silently wrap, and the overflow check is what makes that true. Third, keep the
alphabet in ascending ASCII order; a scrambled alphabet still round-trips but breaks
the ordering property, because lexicographic comparison then no longer tracks digit
value. Note the `testing/quick` round-trip passing here alongside rapid: same
property, but only rapid would hand you a minimal counterexample if it broke. Run
`go test -race`; the codec is pure and its reverse table is built once in `init`.

## Resources

- [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid) — `Uint64`, `SampledFrom`, `SliceOfN`, and `OneOf`.
- [`testing/quick`](https://pkg.go.dev/testing/quick) — the stdlib round-trip, and the absence of shrinking.
- [`slices` package](https://pkg.go.dev/slices) — `slices.Sort` and `slices.IsSorted` for the ordering property.

---

Back to [08-reproducibility-and-fuzz-bridge.md](08-reproducibility-and-fuzz-bridge.md) | Next: [../25-building-a-test-suite/00-concepts.md](../25-building-a-test-suite/00-concepts.md)
