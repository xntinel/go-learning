# Exercise 10: Request IDs — uint64 and Byte-Array Encoding Round-Trips

Correlation and request ids cross every boundary: generated as a counter, logged as
text, parsed back on the next hop. This module builds the codec — a `uint64` counter
formatted as fixed-width zero-padded hex and parsed back with `bitSize 64`, plus a
16-byte id encoded to and from hex with strict length checks — and shows why the width
and the leading zeros matter.

This module is fully self-contained: its own `go mod init`, all code inline, its own
demo and tests. Nothing here imports another exercise.

## What you'll build

```text
requestid/                 independent module: example.com/requestid
  go.mod                   go 1.26
  requestid.go             FormatCounter, ParseCounter, Encode16, Decode16
  cmd/
    demo/
      main.go              format/parse a counter, round-trip a 16-byte id
  requestid_test.go        round-trips (0, MaxUint64), leading zeros, ErrRange, bad length
```

- Files: `requestid.go`, `cmd/demo/main.go`, `requestid_test.go`.
- Implement: `FormatCounter(uint64)` (16-char zero-padded hex), `ParseCounter(string)` (`bitSize 64`), `Encode16([16]byte)`, `Decode16(string)` (strict length).
- Test: round-trip `0` and `math.MaxUint64`; fixed-width output preserves leading zeros; a 65-bit value is rejected with `ErrRange`; a non-hex or wrong-length input errors.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/02-variables-types-and-constants/03-basic-types/10-request-id-hex-encoding/cmd/demo
cd go-solutions/02-variables-types-and-constants/03-basic-types/10-request-id-hex-encoding
```

### Why uint64, and why fixed width

A request-id counter should be `uint64`, not `int`. It is a non-negative identifier
whose whole point is that it can grow without becoming negative, and `uint64` gives the
full 64-bit space with a defined wire width; `int` is platform-sized and signed, so it
neither guarantees the range nor pins the binary layout. Formatting uses hex because it
is compact and case-stable, and the format is *fixed width* — 16 hex characters,
zero-padded — for a concrete operational reason: fixed-width zero-padded ids sort
lexicographically in the same order as numerically, so a log sorted as text is also
sorted by id, and columns line up in a dump. `FormatCounter` builds this by hex-encoding
with `strconv.FormatUint` and left-padding to 16, so `42` becomes `000000000000002a`
rather than a ragged `2a`.

Parsing back uses `strconv.ParseUint(s, 16, 64)` — the `bitSize 64` is what makes a value
that does not fit a `uint64` an `ErrRange` error rather than a silently accepted wider
number. A 17-hex-digit string like `10000000000000000` is `2^64`, one past the type, and
the parse must reject it. The 16-byte id (the trace-id shape) is handled the same way as
in Exercise 1 but with an explicit codec: `Encode16` hex-encodes the array, and `Decode16`
checks the decoded length is exactly 16 before filling the array, so a wrong-length hex
string is an error, not a truncated or zero-padded id.

Create `requestid.go`:

```go
package requestid

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrLength marks a 16-byte id whose decoded length is wrong.
var ErrLength = errors.New("id must be 16 bytes")

// FormatCounter renders a uint64 as 16-character zero-padded lowercase hex, so
// ids sort lexicographically in numeric order.
func FormatCounter(n uint64) string {
	s := strconv.FormatUint(n, 16)
	if len(s) < 16 {
		s = strings.Repeat("0", 16-len(s)) + s
	}
	return s
}

// ParseCounter parses hex back into a uint64. bitSize 64 makes an out-of-range
// value an error rather than a silently wider number.
func ParseCounter(s string) (uint64, error) {
	n, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("parse counter %q: %w", s, err)
	}
	return n, nil
}

// Encode16 hex-encodes a 16-byte id.
func Encode16(id [16]byte) string {
	return hex.EncodeToString(id[:])
}

// Decode16 parses hex into a 16-byte id, rejecting any other length.
func Decode16(s string) ([16]byte, error) {
	var id [16]byte
	decoded, err := hex.DecodeString(s)
	if err != nil {
		return id, fmt.Errorf("decode id %q: %w", s, err)
	}
	if len(decoded) != len(id) {
		return id, fmt.Errorf("%w: got %d", ErrLength, len(decoded))
	}
	copy(id[:], decoded)
	return id, nil
}
```

### The runnable demo

The demo formats a small counter (showing the leading zeros), parses it back, formats
`MaxUint64`, and round-trips a 16-byte id.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"math"

	"example.com/requestid"
)

func main() {
	id := requestid.FormatCounter(42)
	fmt.Println("counter 42:", id)

	n, err := requestid.ParseCounter(id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("parsed back:", n)

	fmt.Println("max:", requestid.FormatCounter(math.MaxUint64))

	var raw [16]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	enc := requestid.Encode16(raw)
	fmt.Println("16-byte id:", enc)

	dec, err := requestid.Decode16(enc)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("round-trip ok:", dec == raw)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
counter 42: 000000000000002a
parsed back: 42
max: ffffffffffffffff
16-byte id: 000102030405060708090a0b0c0d0e0f
round-trip ok: true
```

### Tests

`TestCounterRoundTrip` round-trips `0`, `math.MaxUint64`, and a mid-range value through
`FormatCounter`/`ParseCounter`. `TestFixedWidth` asserts the output is always 16
characters with leading zeros preserved. `TestParseRejectsOverflow` feeds a 65-bit hex
value and asserts `strconv.ErrRange` — the proof that `bitSize 64` is doing its job — and
a non-hex value asserts `ErrSyntax`. `TestDecode16` round-trips a 16-byte id and rejects a
wrong-length input with `ErrLength`.

Create `requestid_test.go`:

```go
package requestid

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"testing"
)

func TestCounterRoundTrip(t *testing.T) {
	t.Parallel()

	for _, n := range []uint64{0, 1, 42, 1 << 32, math.MaxUint64} {
		s := FormatCounter(n)
		got, err := ParseCounter(s)
		if err != nil {
			t.Fatalf("ParseCounter(%q) error %v", s, err)
		}
		if got != n {
			t.Fatalf("round-trip %d -> %q -> %d", n, s, got)
		}
	}
}

func TestFixedWidth(t *testing.T) {
	t.Parallel()

	if got := FormatCounter(0); got != "0000000000000000" {
		t.Fatalf("FormatCounter(0) = %q, want 16 zeros", got)
	}
	if got := FormatCounter(255); got != "00000000000000ff" {
		t.Fatalf("FormatCounter(255) = %q, want leading zeros preserved", got)
	}
	if got := FormatCounter(math.MaxUint64); len(got) != 16 {
		t.Fatalf("FormatCounter(max) length = %d, want 16", len(got))
	}
}

func TestParseRejectsOverflow(t *testing.T) {
	t.Parallel()

	// 0x10000000000000000 is 2^64: one past uint64, 65 bits.
	if _, err := ParseCounter("10000000000000000"); !errors.Is(err, strconv.ErrRange) {
		t.Fatalf("65-bit value error = %v, want ErrRange", err)
	}
	if _, err := ParseCounter("nothex"); !errors.Is(err, strconv.ErrSyntax) {
		t.Fatalf("non-hex error = %v, want ErrSyntax", err)
	}
}

func TestDecode16(t *testing.T) {
	t.Parallel()

	var raw [16]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	enc := Encode16(raw)
	got, err := Decode16(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Fatalf("round-trip mismatch: %x != %x", got, raw)
	}

	if _, err := Decode16("001122"); !errors.Is(err, ErrLength) {
		t.Fatalf("short id error = %v, want ErrLength", err)
	}
}

func ExampleFormatCounter() {
	fmt.Println(FormatCounter(42))
	// Output: 000000000000002a
}
```

## Review

The codec is correct when every value round-trips and every malformed input is an error.
`TestCounterRoundTrip` covers the endpoints `0` and `math.MaxUint64` that a narrower type
or a signed one would mishandle, and `TestParseRejectsOverflow` proves the `bitSize 64`
argument rejects a 65-bit value with `ErrRange` instead of accepting a clamped or wrapped
number. Fixed-width formatting is not cosmetic: `TestFixedWidth` pins the leading zeros
that keep ids lexicographically sortable and log columns aligned. The 16-byte codec's
strict length check is the same discipline as the trace id in Exercise 1 — a wrong length
is a rejected id, never a silently padded one.

## Resources

- [strconv: FormatUint / ParseUint](https://pkg.go.dev/strconv#ParseUint) — hex formatting and `bitSize`-checked parsing.
- [encoding/hex](https://pkg.go.dev/encoding/hex) — `EncodeToString`/`DecodeString` for the byte-array id.
- [Go Specification: Numeric types](https://go.dev/ref/spec#Numeric_types) — why `uint64` gives the full non-negative range.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../04-constants-and-iota/00-concepts.md](../04-constants-and-iota/00-concepts.md)
