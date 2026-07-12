# Exercise 3: The RFC 7541 Huffman Codec

HPACK string literals may be Huffman-coded with the static canonical tree of RFC 7541 Appendix B — a fixed code optimized for HTTP header characters. This module implements both directions from the full Appendix B table, including the two rules that make decoding safe: bounded EOS-prefix padding and rejection of the EOS symbol in the body.

This module is fully self-contained: its own `go mod init`, no external dependencies, its own demo and tests. The tests pin it against the real Huffman-coded values published in RFC 7541 Appendix C.

## What you'll build

```text
huffcodec/
  go.mod
  huffman.go             Encode, EncodedLen, Decode, the Appendix B table
  huffman_test.go        RFC Appendix C vectors, all-byte round-trip, malformed
  cmd/demo/main.go        encode/decode three header values; reject two bad streams
```

- Files: `huffman.go`, `huffman_test.go`, `cmd/demo/main.go`.
- Implement: `Encode(string) []byte`, `EncodedLen(string) int`, `Decode([]byte) (string, error)`.
- Test: the RFC 7541 Appendix C Huffman vectors encode and decode exactly; every byte 0-255 round-trips; over-long padding and an EOS symbol in the body are rejected; valid 1-7 bit padding is accepted.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The table, the bit-packer, and the two decode safety rules

`huffmanCodes` is the complete RFC 7541 Appendix B table: 256 byte entries plus the EOS symbol at index 256, each a `(code, nbits)` pair. The most common HTTP characters get 5-bit codes (`0`-`9`, lowercase vowels and a few consonants), and rarity costs bits up to a 30-bit EOS. Encoding is a big-endian bit accumulator: shift each symbol's code in, flush whole bytes as they fill, and at the end pad the final partial byte with the most-significant bits of EOS — which are all ones — to reach a byte boundary. `EncodedLen` sums the bit lengths and rounds up so a caller can size a buffer without encoding twice.

Decoding walks the input one bit at a time. Because the code is prefix-free, the moment the accumulated `(nbits, code)` pair matches a table entry it is unambiguously that symbol: emit it and reset. Two rules guard the tail. First, the EOS symbol (256) must never be produced from the body; if the walk ever completes EOS, the stream is corrupt. Second, after the last byte at most 7 bits may remain, and those bits must be a strict prefix of EOS — all ones. Eight or more leftover bits means a whole symbol was dropped or injected as fake padding; leftover bits that are not all ones mean the padding is not EOS-derived. Both are decode errors. The lookup map is built once from the table, keyed by `(nbits, code)`, so each bit is an O(1) probe.

Create `huffman.go`:

```go
package huffcodec

import (
	"errors"
	"strings"
)

// ErrInvalidHuffman is returned when a Huffman-coded sequence cannot be decoded
// under RFC 7541: it contains the EOS symbol, has padding longer than 7 bits, or
// its final padding bits are not the most-significant bits of EOS (all ones).
var ErrInvalidHuffman = errors.New("huffcodec: invalid huffman code")

// huffmanCodes is the canonical RFC 7541 Appendix B code table: one entry
// per byte value 0..255 plus the EOS symbol at index 256.
var huffmanCodes = [257]struct {
	code  uint32
	nbits uint8
}{
	{0x1ff8, 13},
	{0x7fffd8, 23},
	{0xfffffe2, 28},
	{0xfffffe3, 28},
	{0xfffffe4, 28},
	{0xfffffe5, 28},
	{0xfffffe6, 28},
	{0xfffffe7, 28},
	{0xfffffe8, 28},
	{0xffffea, 24},
	{0x3ffffffc, 30},
	{0xfffffe9, 28},
	{0xfffffea, 28},
	{0x3ffffffd, 30},
	{0xfffffeb, 28},
	{0xfffffec, 28},
	{0xfffffed, 28},
	{0xfffffee, 28},
	{0xfffffef, 28},
	{0xffffff0, 28},
	{0xffffff1, 28},
	{0xffffff2, 28},
	{0x3ffffffe, 30},
	{0xffffff3, 28},
	{0xffffff4, 28},
	{0xffffff5, 28},
	{0xffffff6, 28},
	{0xffffff7, 28},
	{0xffffff8, 28},
	{0xffffff9, 28},
	{0xffffffa, 28},
	{0xffffffb, 28},
	{0x14, 6},
	{0x3f8, 10},
	{0x3f9, 10},
	{0xffa, 12},
	{0x1ff9, 13},
	{0x15, 6},
	{0xf8, 8},
	{0x7fa, 11},
	{0x3fa, 10},
	{0x3fb, 10},
	{0xf9, 8},
	{0x7fb, 11},
	{0xfa, 8},
	{0x16, 6},
	{0x17, 6},
	{0x18, 6},
	{0x0, 5},
	{0x1, 5},
	{0x2, 5},
	{0x19, 6},
	{0x1a, 6},
	{0x1b, 6},
	{0x1c, 6},
	{0x1d, 6},
	{0x1e, 6},
	{0x1f, 6},
	{0x5c, 7},
	{0xfb, 8},
	{0x7ffc, 15},
	{0x20, 6},
	{0xffb, 12},
	{0x3fc, 10},
	{0x1ffa, 13},
	{0x21, 6},
	{0x5d, 7},
	{0x5e, 7},
	{0x5f, 7},
	{0x60, 7},
	{0x61, 7},
	{0x62, 7},
	{0x63, 7},
	{0x64, 7},
	{0x65, 7},
	{0x66, 7},
	{0x67, 7},
	{0x68, 7},
	{0x69, 7},
	{0x6a, 7},
	{0x6b, 7},
	{0x6c, 7},
	{0x6d, 7},
	{0x6e, 7},
	{0x6f, 7},
	{0x70, 7},
	{0x71, 7},
	{0x72, 7},
	{0xfc, 8},
	{0x73, 7},
	{0xfd, 8},
	{0x1ffb, 13},
	{0x7fff0, 19},
	{0x1ffc, 13},
	{0x3ffc, 14},
	{0x22, 6},
	{0x7ffd, 15},
	{0x3, 5},
	{0x23, 6},
	{0x4, 5},
	{0x24, 6},
	{0x5, 5},
	{0x25, 6},
	{0x26, 6},
	{0x27, 6},
	{0x6, 5},
	{0x74, 7},
	{0x75, 7},
	{0x28, 6},
	{0x29, 6},
	{0x2a, 6},
	{0x7, 5},
	{0x2b, 6},
	{0x76, 7},
	{0x2c, 6},
	{0x8, 5},
	{0x9, 5},
	{0x2d, 6},
	{0x77, 7},
	{0x78, 7},
	{0x79, 7},
	{0x7a, 7},
	{0x7b, 7},
	{0x7ffe, 15},
	{0x7fc, 11},
	{0x3ffd, 14},
	{0x1ffd, 13},
	{0xffffffc, 28},
	{0xfffe6, 20},
	{0x3fffd2, 22},
	{0xfffe7, 20},
	{0xfffe8, 20},
	{0x3fffd3, 22},
	{0x3fffd4, 22},
	{0x3fffd5, 22},
	{0x7fffd9, 23},
	{0x3fffd6, 22},
	{0x7fffda, 23},
	{0x7fffdb, 23},
	{0x7fffdc, 23},
	{0x7fffdd, 23},
	{0x7fffde, 23},
	{0xffffeb, 24},
	{0x7fffdf, 23},
	{0xffffec, 24},
	{0xffffed, 24},
	{0x3fffd7, 22},
	{0x7fffe0, 23},
	{0xffffee, 24},
	{0x7fffe1, 23},
	{0x7fffe2, 23},
	{0x7fffe3, 23},
	{0x7fffe4, 23},
	{0x1fffdc, 21},
	{0x3fffd8, 22},
	{0x7fffe5, 23},
	{0x3fffd9, 22},
	{0x7fffe6, 23},
	{0x7fffe7, 23},
	{0xffffef, 24},
	{0x3fffda, 22},
	{0x1fffdd, 21},
	{0xfffe9, 20},
	{0x3fffdb, 22},
	{0x3fffdc, 22},
	{0x7fffe8, 23},
	{0x7fffe9, 23},
	{0x1fffde, 21},
	{0x7fffea, 23},
	{0x3fffdd, 22},
	{0x3fffde, 22},
	{0xfffff0, 24},
	{0x1fffdf, 21},
	{0x3fffdf, 22},
	{0x7fffeb, 23},
	{0x7fffec, 23},
	{0x1fffe0, 21},
	{0x1fffe1, 21},
	{0x3fffe0, 22},
	{0x1fffe2, 21},
	{0x7fffed, 23},
	{0x3fffe1, 22},
	{0x7fffee, 23},
	{0x7fffef, 23},
	{0xfffea, 20},
	{0x3fffe2, 22},
	{0x3fffe3, 22},
	{0x3fffe4, 22},
	{0x7ffff0, 23},
	{0x3fffe5, 22},
	{0x3fffe6, 22},
	{0x7ffff1, 23},
	{0x3ffffe0, 26},
	{0x3ffffe1, 26},
	{0xfffeb, 20},
	{0x7fff1, 19},
	{0x3fffe7, 22},
	{0x7ffff2, 23},
	{0x3fffe8, 22},
	{0x1ffffec, 25},
	{0x3ffffe2, 26},
	{0x3ffffe3, 26},
	{0x3ffffe4, 26},
	{0x7ffffde, 27},
	{0x7ffffdf, 27},
	{0x3ffffe5, 26},
	{0xfffff1, 24},
	{0x1ffffed, 25},
	{0x7fff2, 19},
	{0x1fffe3, 21},
	{0x3ffffe6, 26},
	{0x7ffffe0, 27},
	{0x7ffffe1, 27},
	{0x3ffffe7, 26},
	{0x7ffffe2, 27},
	{0xfffff2, 24},
	{0x1fffe4, 21},
	{0x1fffe5, 21},
	{0x3ffffe8, 26},
	{0x3ffffe9, 26},
	{0xffffffd, 28},
	{0x7ffffe3, 27},
	{0x7ffffe4, 27},
	{0x7ffffe5, 27},
	{0xfffec, 20},
	{0xfffff3, 24},
	{0xfffed, 20},
	{0x1fffe6, 21},
	{0x3fffe9, 22},
	{0x1fffe7, 21},
	{0x1fffe8, 21},
	{0x7ffff3, 23},
	{0x3fffea, 22},
	{0x3fffeb, 22},
	{0x1ffffee, 25},
	{0x1ffffef, 25},
	{0xfffff4, 24},
	{0xfffff5, 24},
	{0x3ffffea, 26},
	{0x7ffff4, 23},
	{0x3ffffeb, 26},
	{0x7ffffe6, 27},
	{0x3ffffec, 26},
	{0x3ffffed, 26},
	{0x7ffffe7, 27},
	{0x7ffffe8, 27},
	{0x7ffffe9, 27},
	{0x7ffffea, 27},
	{0x7ffffeb, 27},
	{0xffffffe, 28},
	{0x7ffffec, 27},
	{0x7ffffed, 27},
	{0x7ffffee, 27},
	{0x7ffffef, 27},
	{0x7fffff0, 27},
	{0x3ffffee, 26},
	{0x3fffffff, 30},
}

const eosSymbol = 256

type decKey struct {
	nbits uint8
	code  uint32
}

// decTable maps each (bit length, code) pair to its symbol. The HPACK code is
// prefix-free, so a match at the current bit length is unambiguous.
var decTable = func() map[decKey]uint16 {
	m := make(map[decKey]uint16, 257)
	for sym := 0; sym < 257; sym++ {
		e := huffmanCodes[sym]
		m[decKey{e.nbits, e.code}] = uint16(sym)
	}
	return m
}()

// Encode Huffman-codes s per RFC 7541 Appendix B. The final byte is padded with
// the most-significant bits of the EOS symbol (all ones).
func Encode(s string) []byte {
	var out []byte
	var acc uint64
	var nb uint
	for i := 0; i < len(s); i++ {
		e := huffmanCodes[s[i]]
		acc = acc<<uint(e.nbits) | uint64(e.code)
		nb += uint(e.nbits)
		for nb >= 8 {
			nb -= 8
			out = append(out, byte(acc>>nb))
		}
	}
	if nb > 0 {
		pad := 8 - nb
		out = append(out, byte(acc<<pad)|byte((1<<pad)-1))
	}
	return out
}

// EncodedLen returns the number of bytes Encode(s) produces without allocating.
func EncodedLen(s string) int {
	bits := 0
	for i := 0; i < len(s); i++ {
		bits += int(huffmanCodes[s[i]].nbits)
	}
	return (bits + 7) / 8
}

// Decode reverses Encode. It rejects a stream that contains EOS, that has more
// than 7 bits of trailing padding, or whose padding bits are not all ones.
func Decode(data []byte) (string, error) {
	var sb strings.Builder
	var acc uint32
	var nb uint8
	for _, b := range data {
		for i := 7; i >= 0; i-- {
			acc = acc<<1 | uint32((b>>uint(i))&1)
			nb++
			if nb > 30 {
				return "", ErrInvalidHuffman
			}
			if sym, ok := decTable[decKey{nb, acc}]; ok {
				if sym == eosSymbol {
					return "", ErrInvalidHuffman
				}
				sb.WriteByte(byte(sym))
				acc = 0
				nb = 0
			}
		}
	}
	if nb >= 8 {
		return "", ErrInvalidHuffman
	}
	if nb > 0 && acc != (uint32(1)<<nb)-1 {
		return "", ErrInvalidHuffman
	}
	return sb.String(), nil
}
```

### The runnable demo

The demo Huffman-codes three real header values, shows the byte savings, decodes them back, and then feeds the decoder two malformed streams — one with a whole extra byte of "padding" and one that decodes to EOS — to show both are rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/huffcodec"
)

func main() {
	for _, s := range []string{"www.example.com", "no-cache", "private"} {
		enc := huffcodec.Encode(s)
		dec, _ := huffcodec.Decode(enc)
		fmt.Printf("%-16q %2d -> %2d bytes  % x  decoded %q\n",
			s, len(s), len(enc), enc, dec)
	}

	// Padding longer than 7 bits is a decoding error (RFC 7541 section 5.2).
	_, err := huffcodec.Decode([]byte{0x1f, 0xff})
	fmt.Printf("over-long padding rejected: %v\n", errors.Is(err, huffcodec.ErrInvalidHuffman))

	// A stream that decodes to the EOS symbol is rejected.
	_, err = huffcodec.Decode([]byte{0xff, 0xff, 0xff, 0xff})
	fmt.Printf("EOS in input rejected:      %v\n", errors.Is(err, huffcodec.ErrInvalidHuffman))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
"www.example.com" 15 -> 12 bytes  f1 e3 c2 e5 f2 3a 6b a0 ab 90 f4 ff  decoded "www.example.com"
"no-cache"        8 ->  6 bytes  a8 eb 10 64 9c bf  decoded "no-cache"
"private"         7 ->  5 bytes  ae c3 77 1a 4b  decoded "private"
over-long padding rejected: true
EOS in input rejected:      true
```

`www.example.com` shrinks from 15 to 12 bytes and `private` from 7 to 5 — the 20-30% typical of HPACK Huffman coding. These exact byte sequences are the ones printed in RFC 7541 Appendix C.

### Tests

`TestRFCAppendixCVectors` is the authority: it pins ten real Huffman-coded values from RFC 7541 Appendix C (`www.example.com`, `no-cache`, dates, a full URL, `gzip`, and more) in both directions, so any single wrong table entry fails the suite. `TestRoundTripAllBytes` proves every byte 0-255 survives a round-trip, `TestRoundTripStrings` fuzzes 2000 random strings and checks `EncodedLen` against the real length, and the malformed tests pin over-long padding, EOS-in-body, and acceptance of valid short padding.

Create `huffman_test.go`:

```go
package huffcodec

import (
	"encoding/hex"
	"errors"
	"math/rand"
	"testing"
)

// RFC 7541 Appendix C vectors: real Huffman-coded header values from the spec.
var rfcVectors = []struct {
	plain string
	hexed string
}{
	{"www.example.com", "f1e3c2e5f23a6ba0ab90f4ff"},
	{"no-cache", "a8eb10649cbf"},
	{"custom-key", "25a849e95ba97d7f"},
	{"custom-value", "25a849e95bb8e8b4bf"},
	{"302", "6402"},
	{"private", "aec3771a4b"},
	{"Mon, 21 Oct 2013 20:13:21 GMT", "d07abe941054d444a8200595040b8166e082a62d1bff"},
	{"https://www.example.com", "9d29ad171863c78f0b97c8e9ae82ae43d3"},
	{"307", "640eff"},
	{"gzip", "9bd9ab"},
}

func TestRFCAppendixCVectors(t *testing.T) {
	t.Parallel()
	for _, v := range rfcVectors {
		want, err := hex.DecodeString(v.hexed)
		if err != nil {
			t.Fatal(err)
		}
		got := Encode(v.plain)
		if !bytesEqual(got, want) {
			t.Errorf("Encode(%q) = %x, want %x", v.plain, got, want)
		}
		back, err := Decode(want)
		if err != nil {
			t.Errorf("Decode(%x): %v", want, err)
		}
		if back != v.plain {
			t.Errorf("Decode(%x) = %q, want %q", want, back, v.plain)
		}
	}
}

func TestRoundTripAllBytes(t *testing.T) {
	t.Parallel()
	for b := 0; b < 256; b++ {
		s := string([]byte{byte(b)})
		back, err := Decode(Encode(s))
		if err != nil || back != s {
			t.Errorf("byte %d round-trip: got %q, err %v", b, back, err)
		}
	}
}

func TestRoundTripStrings(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewSource(42))
	for n := 0; n < 2000; n++ {
		buf := make([]byte, r.Intn(48))
		for i := range buf {
			buf[i] = byte(r.Intn(256))
		}
		s := string(buf)
		if EncodedLen(s) != len(Encode(s)) {
			t.Fatalf("EncodedLen mismatch for %q", s)
		}
		back, err := Decode(Encode(s))
		if err != nil || back != s {
			t.Fatalf("round-trip %q: got %q, err %v", s, back, err)
		}
	}
}

func TestRejectOverlongPadding(t *testing.T) {
	t.Parallel()
	// "a" is 0x1f (5 code bits + 3 padding ones); a trailing all-ones byte adds
	// 11 bits of padding, which exceeds the 7-bit maximum.
	if _, err := Decode([]byte{0x1f, 0xff}); !errors.Is(err, ErrInvalidHuffman) {
		t.Fatalf("err = %v, want ErrInvalidHuffman", err)
	}
}

func TestRejectEOSInInput(t *testing.T) {
	t.Parallel()
	// 32 one-bits decode to the 30-bit EOS symbol, which must never appear.
	if _, err := Decode([]byte{0xff, 0xff, 0xff, 0xff}); !errors.Is(err, ErrInvalidHuffman) {
		t.Fatalf("err = %v, want ErrInvalidHuffman", err)
	}
}

func TestValidPaddingAccepted(t *testing.T) {
	t.Parallel()
	got, err := Decode([]byte{0x1f}) // "a" with 3 bits of valid EOS-prefix padding
	if err != nil || got != "a" {
		t.Fatalf("Decode(0x1f) = %q, %v; want \"a\", nil", got, err)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

## Review

The codec is correct when the Appendix C vectors match exactly and the two padding rules hold. Confirm the encoder pads with one-bits (EOS prefix) and not zeros, confirm the decoder rejects the EOS symbol and any padding that is 8+ bits or not all ones, and confirm a legitimate 1-7 bit pad still decodes. The frequent mistakes are padding with zeros, accepting a full byte of trailing ones as "padding" (which silently swallows a dropped symbol), and building a slow per-bit linear scan instead of a prefix-free table probe. The Appendix C vectors are what turn "looks right" into "is right."

## Resources

- [RFC 7541 Appendix B - Huffman Code](https://httpwg.org/specs/rfc7541.html#huffman.code) — the canonical table this module encodes from, code and bit length per symbol.
- [RFC 7541 section 5.2 - String Literal Representation](https://httpwg.org/specs/rfc7541.html#string.literal.representation) — the length prefix, the Huffman flag, and the padding rule.
- [RFC 7541 Appendix C - Examples](https://httpwg.org/specs/rfc7541.html#request.examples) — the worked requests and responses whose Huffman bytes the tests pin against.
- [HuffmanDecode in golang.org/x/net/http2/hpack](https://cs.opensource.google/go/x/net/+/master:http2/hpack/huffman.go) — the production codec, including its EOS and padding checks.

---

Back to [00-concepts.md](00-concepts.md) | Next: [04-dynamic-table.md](04-dynamic-table.md)
