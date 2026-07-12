# Exercise 2: N-Bit-Prefix Integers With An Overflow Guard

Every number in HPACK — an index, a string length, a table-size update — rides on one integer encoding that begins mid-byte and spills into 7-bit continuation bytes (RFC 7541 section 5.1). This module implements that codec from scratch and, just as importantly, the decoder's mandatory bound against a peer that sends an unbounded run of continuation bytes.

This module is fully self-contained: its own `go mod init`, no external dependencies, its own demo and tests.

## What you'll build

```text
hpackint/
  go.mod
  int.go                 AppendInt, ReadInt, ErrOverflow, ErrTruncated
  int_test.go            RFC 5.1 vectors, round-trip, overflow + truncation
  cmd/demo/main.go        encode 10/1337/42; reject a malicious varint
```

- Files: `int.go`, `int_test.go`, `cmd/demo/main.go`.
- Implement: `AppendInt(dst, n, prefixBits, prefixVal)` and `ReadInt(data, prefixBits) (val, consumed, err)`.
- Test: the three RFC 7541 section 5.1 vectors encode and decode exactly; values round-trip across every prefix width; an endless continuation run and a dangling continuation byte are rejected.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### The encoding, and why the decoder is a security boundary

To encode `n` with an N-bit prefix: if `n < 2^N - 1` it fits in the prefix and the whole integer is one byte. Otherwise write `2^N - 1` into the prefix, subtract it, and emit the remainder 7 bits at a time, low bits first, setting the high continuation bit on every byte except the last. RFC 7541 section 5.1's worked examples are 10 with a 5-bit prefix (one byte `0x0a`), 1337 with a 5-bit prefix (`0x1f 0x9a 0x0a`), and 42 with an 8-bit prefix (`0x2a`). The `prefixVal` argument carries the high 8-N flag bits of the first byte so a caller can OR in a representation pattern; decoding masks those flag bits off and reads only the low N.

Decoding is where the danger is. The loop reads continuation bytes while the high bit is set, accumulating `value += (byte & 0x7f) << shift`. A hostile peer can make that loop run forever, or — worse — let the accumulator overflow into a small or negative number that the caller then trusts as a length or an allocation size. RFC 7541 section 5.1 requires a bound. The guard here caps values at the largest signed 32-bit integer, far above any real index or length, and it fires *before* the shift can wrap: once the shift reaches 28 bits, only the low 3 bits of the next continuation byte can still fit under the cap, so a larger byte is rejected immediately. A continuation bit with no following byte is a separate, equally hard error (`ErrTruncated`); a real decoder treats both as connection-fatal.

Create `int.go`:

```go
package hpackint

import "errors"

// ErrOverflow is returned when a decoded integer exceeds the implementation
// limit. RFC 7541 section 5.1 requires decoders to guard against integers that
// grow without bound, which is a denial-of-service vector otherwise.
var ErrOverflow = errors.New("hpackint: integer overflow")

// ErrTruncated is returned when a continuation byte is expected but the input
// ends.
var ErrTruncated = errors.New("hpackint: truncated integer")

// maxValue caps decoded integers at the largest signed 32-bit value, which is
// far above any legitimate header length, index, or table size.
const maxValue = 1<<31 - 1

// AppendInt encodes n with an N-bit prefix (RFC 7541 section 5.1) onto dst.
// prefixBits is N (1..8); prefixVal supplies the high 8-N flag bits of the
// first byte. n must be non-negative.
func AppendInt(dst []byte, n int, prefixBits uint8, prefixVal byte) []byte {
	k := (1 << prefixBits) - 1
	if n < k {
		return append(dst, prefixVal|byte(n))
	}
	dst = append(dst, prefixVal|byte(k))
	n -= k
	for n >= 128 {
		dst = append(dst, byte(n&127)|128)
		n >>= 7
	}
	return append(dst, byte(n))
}

// ReadInt decodes an N-bit-prefix integer from the front of data. It returns
// the value, the number of bytes consumed, and an error. The high flag bits of
// the first byte are ignored; only the low N bits participate. It rejects
// integers that would overflow maxValue and inputs that end mid-continuation.
func ReadInt(data []byte, prefixBits uint8) (val, consumed int, err error) {
	if len(data) == 0 {
		return 0, 0, ErrTruncated
	}
	k := (1 << prefixBits) - 1
	n := int(data[0]) & k
	if n < k {
		return n, 1, nil
	}
	m := 0
	i := 1
	for {
		if i >= len(data) {
			return 0, 0, ErrTruncated
		}
		b := data[i]
		i++
		// Reject before the shift can overflow: at m >= 28 only the low 3 bits
		// of the continuation byte can still fit under maxValue.
		if m >= 28 && (b&127) > 7 {
			return 0, 0, ErrOverflow
		}
		n += int(b&127) << m
		if n < 0 || n > maxValue {
			return 0, 0, ErrOverflow
		}
		m += 7
		if b&128 == 0 {
			return n, i, nil
		}
		if m > 35 {
			return 0, 0, ErrOverflow
		}
	}
}
```

### The runnable demo

The demo encodes the three RFC vectors and decodes them back, then feeds the decoder a deliberately malicious value — a prefix byte followed by a long run of `0xff` continuation bytes — and shows it is rejected as an overflow rather than looping or wrapping.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/hpackint"
)

func main() {
	for _, c := range []struct {
		n  int
		pb uint8
	}{{10, 5}, {1337, 5}, {42, 8}} {
		enc := hpackint.AppendInt(nil, c.n, c.pb, 0)
		v, consumed, _ := hpackint.ReadInt(enc, c.pb)
		fmt.Printf("%d with %d-bit prefix -> % x  (%d bytes); decoded %d\n",
			c.n, c.pb, enc, consumed, v)
	}

	// Security guard: an attacker-supplied run of continuation bytes is rejected
	// instead of looping or overflowing.
	evil := []byte{0x1f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	_, _, err := hpackint.ReadInt(evil, 5)
	fmt.Printf("malicious varint rejected: %v\n", errors.Is(err, hpackint.ErrOverflow))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
10 with 5-bit prefix -> 0a  (1 bytes); decoded 10
1337 with 5-bit prefix -> 1f 9a 0a  (3 bytes); decoded 1337
42 with 8-bit prefix -> 2a  (1 bytes); decoded 42
malicious varint rejected: true
```

### Tests

`TestRFCVectors` pins the three section 5.1 examples in both directions. `TestRoundTrip` sweeps representative values — including the cap itself — across prefix widths 4 through 8, asserting both the value and the byte count survive, and it deliberately sets all the high flag bits via `prefixVal` to prove the decoder masks them. `TestOverflowRejected` feeds an endless continuation run and `TestTruncatedRejected` feeds an empty input and a dangling continuation byte.

Create `int_test.go`:

```go
package hpackint

import (
	"errors"
	"testing"
)

func TestRFCVectors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n    int
		pb   uint8
		want []byte
	}{
		{10, 5, []byte{0x0a}},               // RFC 7541 C.1.1
		{1337, 5, []byte{0x1f, 0x9a, 0x0a}}, // RFC 7541 C.1.2
		{42, 8, []byte{0x2a}},               // RFC 7541 C.1.3
	}
	for _, c := range cases {
		got := AppendInt(nil, c.n, c.pb, 0)
		if !equal(got, c.want) {
			t.Errorf("AppendInt(%d, %d) = % x, want % x", c.n, c.pb, got, c.want)
		}
		v, consumed, err := ReadInt(c.want, c.pb)
		if err != nil || v != c.n || consumed != len(c.want) {
			t.Errorf("ReadInt(% x, %d) = %d, %d, %v; want %d, %d, nil",
				c.want, c.pb, v, consumed, err, c.n, len(c.want))
		}
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	values := []int{0, 1, 2, 30, 31, 32, 127, 128, 255, 256, 16383, 16384, 1 << 20, maxValue}
	for _, pb := range []uint8{4, 5, 6, 7, 8} {
		for _, n := range values {
			enc := AppendInt(nil, n, pb, 0xff<<pb)
			v, consumed, err := ReadInt(enc, pb)
			if err != nil {
				t.Fatalf("ReadInt(% x, %d): %v", enc, pb, err)
			}
			if v != n {
				t.Errorf("round-trip %d/%d: got %d", n, pb, v)
			}
			if consumed != len(enc) {
				t.Errorf("round-trip %d/%d: consumed %d, want %d", n, pb, consumed, len(enc))
			}
		}
	}
}

func TestOverflowRejected(t *testing.T) {
	t.Parallel()
	evil := []byte{0x1f}
	for i := 0; i < 12; i++ {
		evil = append(evil, 0xff)
	}
	if _, _, err := ReadInt(evil, 5); !errors.Is(err, ErrOverflow) {
		t.Fatalf("err = %v, want ErrOverflow", err)
	}
}

func TestTruncatedRejected(t *testing.T) {
	t.Parallel()
	if _, _, err := ReadInt(nil, 5); !errors.Is(err, ErrTruncated) {
		t.Errorf("empty: err = %v, want ErrTruncated", err)
	}
	if _, _, err := ReadInt([]byte{0x1f, 0x80}, 5); !errors.Is(err, ErrTruncated) {
		t.Errorf("dangling continuation: err = %v, want ErrTruncated", err)
	}
}

func equal(a, b []byte) bool {
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

The codec is correct when every value round-trips and the decoder rejects both malformed shapes. Confirm the encoder's one-byte fast path triggers exactly when `n < 2^N - 1`, confirm decoding masks the flag bits and reads only the low N, and confirm the overflow guard fires before the accumulator can wrap — a guard placed after the shift is already too late. The classic mistakes are looping on the continuation flag without a bound, comparing against the cap only after overflow has produced a negative number, and forgetting that a continuation byte with no successor is itself an error rather than an implicit zero.

## Resources

- [RFC 7541 section 5.1 - Integer Representation](https://httpwg.org/specs/rfc7541.html#integer.representation) — the encoding, the pseudocode, and the worked 10/1337/42 examples.
- [RFC 7541 section 6 - Binary Format](https://httpwg.org/specs/rfc7541.html#detailed.format) — where each representation states its prefix width (4, 5, 6, or 7 bits).
- [readVarInt in golang.org/x/net/http2/hpack](https://cs.opensource.google/go/x/net/+/master:http2/hpack/hpack.go) — the production decoder, including its overflow rejection.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-huffman-codec.md](03-huffman-codec.md)
