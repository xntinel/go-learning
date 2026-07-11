# Exercise 18: Encoding and Decoding the 12-Byte DNS Message Header

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

Every DNS message — query or response, over UDP or TCP, from a stub
resolver, `dig`, or an authoritative nameserver — starts with the exact same
12-byte header defined in RFC 1035 section 4.1.1: a 16-bit ID, a 16-bit
bitfield packing QR/Opcode/AA/TC/RD/RA/RCODE, and four 16-bit section
counts. That fixed layout is a textbook case for `[12]byte`: the length is
not a convention, it is the wire format, so the type should say so. This
exercise builds `dnshdr`, a command that reads one hex-encoded header on
stdin and prints its decoded fields, using `encoding/binary.BigEndian` to
read and write the integer fields directly on slices of the array, and
hand-rolled bit-shifting to pack and unpack the flags byte.

The bitfield is where a naive implementation goes wrong silently: getting a
bit position off by one still compiles and often still "round-trips"
correctly (encode and decode agreeing with each other proves nothing if they
share the same bug), so this exercise also pins the exact on-wire byte
pattern against the RFC, not just against itself.

This module is fully self-contained: its own `go mod init`, an executable
command, and its tests. Nothing here imports another exercise.

## What you'll build

```text
dnsheader/                     module example.com/dnsheader
  go.mod                       go 1.24
  dnsheader.go                 package main — Header{ID, Flags, QDCount, ANCount, NSCount, ARCount}; Flags{QR,Opcode,AA,TC,RD,RA,RCODE}; Encode/Decode; ErrShortBuffer
  dnsheader_test.go            package main — round-trip table, truncated-buffer sweep, bit-level flag layout, opcode/rcode masking, run() end to end
  main.go                      package main — reads hex on stdin, prints decoded fields
```

- Files: `dnsheader.go`, `dnsheader_test.go`, `main.go`.
- Implement: `Encode(h Header) [HeaderSize]byte` packing `ID`, the flags bitfield, and the four section counts big-endian into a `[HeaderSize]byte`; `Decode(buf []byte) (Header, error)` that checks `len(buf) < HeaderSize` before converting `buf[:HeaderSize]` to `[HeaderSize]byte` and unpacking every field, returning `ErrShortBuffer` on a short buffer.
- Tool: `dnshdr` reads the whole of stdin (a fixed 12-byte record is never a stream), trims surrounding whitespace, hex-decodes it, and calls `Decode`. It takes no arguments. Exit 0 on success, exit 2 for a bad flag, an unexpected argument, non-hex input, or a decoded buffer shorter than 12 bytes, exit 1 for a stdin read failure.
- Test: a table of representative headers (query, authoritative response, truncated response, server-failure response, zero value) round-trips through `Encode`/`Decode`; every buffer length from 0 to 11 bytes returns `ErrShortBuffer` instead of panicking; the exact flags byte pattern for a hand-picked `Flags` value is asserted bit by bit; an out-of-range `Opcode`/`RCODE` is masked to its 4-bit wire width instead of corrupting neighboring bits; `run` end to end over `strings.Reader` and `bytes.Buffer`, including truncated and non-hex input.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dnsheader
cd ~/go-exercises/dnsheader
go mod init example.com/dnsheader
go mod edit -go=1.24
```

### Why the header is an array and the flags need bit-level tests

`HeaderSize` is 12 and never anything else — RFC 1035 is not describing a
minimum, it is describing the entire fixed structure that precedes the
variable-length question and resource-record sections. `[HeaderSize]byte`
makes that a compile-time fact: `Encode` cannot return the wrong number of
bytes, and there is no way to construct a "DNS header" value with 11 or 13
bytes. `encoding/binary.BigEndian` operates on `[]byte`, so `Encode` writes
into `buf[0:2]`, `buf[2:4]`, and so on — slicing a local array is free
because the array is addressable and the slice header just points at its
existing storage, no allocation. `Decode` mirrors this after the mandatory
length guard: `buf[:HeaderSize]` is only converted to the fixed array once
the length check has already proven it is safe, following the same
guard-then-convert discipline any wire boundary needs.

The bitfield is the part most likely to be wrong in a way tests don't catch
by accident. RFC 1035's second 16-bit word, most-significant-bit first, is
`QR(1) Opcode(4) AA(1) TC(1) RD(1) RA(1) Z(3, reserved) RCODE(4)`. A
round-trip test alone — encode a `Flags` value, decode it, compare — cannot
catch a bug where two bit positions are swapped consistently in both
directions, because encode and decode would still agree with each other.
The fix is to also assert the literal byte pattern for at least one known
`Flags` value against a hand-computed expectation, which is what
`TestFlagBitLayout` does: it sets every flag and picks all-ones for `Opcode`
and `RCODE`, computes the expected two bytes by hand from the bit layout
above, and checks `Encode` produces exactly that pattern. A second test,
`TestOpcodeAndRCODEAreMaskedToFourBits`, checks the boundary case of an
out-of-range 4-bit field: `Opcode` and `RCODE` are each masked with `0x0f`
on encode, so a caller who accidentally passes `0xff` gets the low nibble on
the wire rather than corrupting adjacent bits in the flags word.

Create `dnsheader.go`:

```go
// Command dnshdr reads one hex-encoded 12-byte DNS message header on stdin
// and prints its decoded fields, the fixed layout defined in RFC 1035
// section 4.1.1 that precedes every DNS query and response.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// HeaderSize is the fixed on-wire length of a DNS message header in bytes:
// every valid DNS message starts with exactly this many, which is why the
// wire form below is a [HeaderSize]byte array rather than a slice.
const HeaderSize = 12

// ErrShortBuffer is returned by Decode when buf is shorter than HeaderSize.
var ErrShortBuffer = errors.New("dnsheader: buffer shorter than 12-byte header")

// Flags is the parsed form of the second 16-bit word of the header: the
// QR/Opcode/AA/TC/RD/RA bitfield plus the 4-bit RCODE. RFC 1035 lays these
// bits out most-significant-bit first, starting at bit 15:
//
//	bit:   15 14 13 12 11 10  9  8  7  6  5  4  3  2  1  0
//	field: QR  ----Opcode---  AA TC RD RA  Z  Z  Z  RCODE---
type Flags struct {
	QR     bool  // false = query, true = response
	Opcode uint8 // 4 bits: 0 = standard query
	AA     bool  // authoritative answer
	TC     bool  // truncated
	RD     bool  // recursion desired
	RA     bool  // recursion available
	RCODE  uint8 // 4 bits: 0 = no error
}

// Header is the parsed 12-byte DNS message header.
type Header struct {
	ID      uint16
	Flags   Flags
	QDCount uint16 // number of entries in the question section
	ANCount uint16 // number of resource records in the answer section
	NSCount uint16 // number of name server resource records in the authority section
	ARCount uint16 // number of resource records in the additional section
}

// Encode packs h into the wire form: a fixed [HeaderSize]byte array, each
// field written big-endian per RFC 1035. Returning an array rather than a
// slice means the caller gets an independent 12-byte value with no shared
// backing store -- encoding two headers from the same Header value can
// never let one call's buffer alias the other's.
func Encode(h Header) [HeaderSize]byte {
	var buf [HeaderSize]byte
	binary.BigEndian.PutUint16(buf[0:2], h.ID)
	binary.BigEndian.PutUint16(buf[2:4], encodeFlags(h.Flags))
	binary.BigEndian.PutUint16(buf[4:6], h.QDCount)
	binary.BigEndian.PutUint16(buf[6:8], h.ANCount)
	binary.BigEndian.PutUint16(buf[8:10], h.NSCount)
	binary.BigEndian.PutUint16(buf[10:12], h.ARCount)
	return buf
}

// Decode parses a 12-byte DNS header from the front of buf. It checks the
// length before converting, so a truncated read (a short UDP datagram, a
// partial TCP frame) returns ErrShortBuffer instead of panicking.
func Decode(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, fmt.Errorf("have %d bytes: %w", len(buf), ErrShortBuffer)
	}
	// Safe now: the guard above guarantees at least HeaderSize bytes.
	arr := [HeaderSize]byte(buf[:HeaderSize])

	var h Header
	h.ID = binary.BigEndian.Uint16(arr[0:2])
	h.Flags = decodeFlags(binary.BigEndian.Uint16(arr[2:4]))
	h.QDCount = binary.BigEndian.Uint16(arr[4:6])
	h.ANCount = binary.BigEndian.Uint16(arr[6:8])
	h.NSCount = binary.BigEndian.Uint16(arr[8:10])
	h.ARCount = binary.BigEndian.Uint16(arr[10:12])
	return h, nil
}

// encodeFlags packs a Flags value into the 16-bit wire bitfield.
func encodeFlags(f Flags) uint16 {
	var v uint16
	if f.QR {
		v |= 1 << 15
	}
	v |= uint16(f.Opcode&0x0f) << 11
	if f.AA {
		v |= 1 << 10
	}
	if f.TC {
		v |= 1 << 9
	}
	if f.RD {
		v |= 1 << 8
	}
	if f.RA {
		v |= 1 << 7
	}
	// Bits 6-4 are the reserved Z field, always zero.
	v |= uint16(f.RCODE & 0x0f)
	return v
}

// decodeFlags unpacks the 16-bit wire bitfield into a Flags value.
func decodeFlags(v uint16) Flags {
	return Flags{
		QR:     v&(1<<15) != 0,
		Opcode: uint8((v >> 11) & 0x0f),
		AA:     v&(1<<10) != 0,
		TC:     v&(1<<9) != 0,
		RD:     v&(1<<8) != 0,
		RA:     v&(1<<7) != 0,
		RCODE:  uint8(v & 0x0f),
	}
}
```

### The tool

`dnshdr` reads the whole of stdin with `io.ReadAll` rather than streaming
it, because the input here is one fixed-size record, not an unbounded
sequence of records — unlike `merkleroot`'s line-at-a-time stdin, buffering
this input whole is the correct call, not a shortcut. `run` takes `args`, an
`io.Reader` for stdin, and an `io.Writer` for stdout, so a test drives it
with `strings.Reader` and `bytes.Buffer`. Three distinct failures all wrap
`errUsage` and map to exit code 2: a bad flag, hex-decoding failure, and
`Decode` rejecting a short buffer — a truncated capture is exactly as much a
"fix your input" problem as malformed hex is. Exit code 1 is reserved for a
genuine stdin read failure.

Create `main.go`:

```go
package main

import (
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// errUsage marks a bad flag, non-hex input, or a truncated buffer (fewer
// than 12 decoded bytes). main maps it to exit code 2; a stdin read failure
// maps to exit code 1.
var errUsage = errors.New("usage")

// run reads one hex-encoded 12-byte DNS header from stdin and writes its
// decoded fields as a single line to stdout. Reading the whole of stdin is
// correct here: the input is one fixed-size record, not an unbounded stream.
func run(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("dnshdr", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: dnshdr takes no arguments, only stdin", errUsage)
	}

	raw, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("reading stdin: %w", err)
	}
	text := strings.TrimSpace(string(raw))

	buf, err := hex.DecodeString(text)
	if err != nil {
		return fmt.Errorf("%w: input is not valid hex: %v", errUsage, err)
	}

	h, err := Decode(buf)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	fmt.Fprintf(stdout, "id=%#04x qr=%v opcode=%d aa=%v tc=%v rd=%v ra=%v rcode=%d qdcount=%d ancount=%d nscount=%d arcount=%d\n",
		h.ID, h.Flags.QR, h.Flags.Opcode, h.Flags.AA, h.Flags.TC, h.Flags.RD, h.Flags.RA, h.Flags.RCODE,
		h.QDCount, h.ANCount, h.NSCount, h.ARCount)
	return nil
}

func main() {
	flag.CommandLine.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: dnshdr < header.hex")
		fmt.Fprintln(os.Stderr, "reads one hex-encoded 12-byte DNS header from stdin, prints its fields.")
	}
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "dnshdr:", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
```

Run it:

```bash
printf '1a2b01000001000000000000\n' | go run .
printf '1a2b010000010000' | go run .
```

Expected output:

```text
id=0x1a2b qr=false opcode=0 aa=false tc=false rd=true ra=false rcode=0 qdcount=1 ancount=0 nscount=0 arcount=0
dnshdr: usage: have 8 bytes: dnsheader: buffer shorter than 12-byte header
```

The first line decodes a 24-character hex string (12 bytes) encoding a
standard query with `RD` set and one question; the flags byte `0x01` has
only bit 8 (`RD`) on. The second feeds 16 hex characters — 8 decoded bytes,
four short of the required 12 — and shows the exit-2 usage error `Decode`
produces, with both the actual and required lengths in the text, rather
than a panic on a truncated slice-to-array conversion.

### Tests

`TestRoundTrip` is the table over representative headers a real resolver
would see: a plain recursive query, a successful authoritative response, a
truncated response, a server-failure response with every count field
populated, and the zero-value header as a boundary case. `TestDecodeTruncated`
sweeps every buffer length from 0 through 11 and asserts `ErrShortBuffer` on
each, proving the guard covers every short length, not just zero.
`TestFlagBitLayout` is the bit-level check described above: it sets every
flag plus all-ones `Opcode`/`RCODE` and asserts the exact two encoded bytes
against a hand-derived expectation, which a round-trip-only test cannot
catch a transposed-bit bug with. `TestOpcodeAndRCODEAreMaskedToFourBits`
checks that an out-of-range 4-bit field value is masked down rather than
bleeding into neighboring bits. `TestRun` drives the command end to end: a
well-formed hex header against its exact decoded-fields line, a truncated
buffer, and input that is not valid hex at all.

Create `dnsheader_test.go`:

```go
package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// TestRoundTrip encodes and decodes a table of representative headers and
// asserts the decoded value equals the original -- a standard query, a
// successful response, a truncated response, and a server-failure error
// response.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		h    Header
	}{
		{
			name: "standard query with recursion desired",
			h: Header{
				ID:      0x1a2b,
				Flags:   Flags{RD: true},
				QDCount: 1,
			},
		},
		{
			name: "authoritative response, no error",
			h: Header{
				ID:      0x1a2b,
				Flags:   Flags{QR: true, AA: true, RD: true, RA: true},
				QDCount: 1,
				ANCount: 2,
			},
		},
		{
			name: "truncated response",
			h: Header{
				ID:      0xffff,
				Flags:   Flags{QR: true, TC: true, RD: true},
				QDCount: 1,
			},
		},
		{
			name: "server failure with all counts populated",
			h: Header{
				ID:      0x0001,
				Flags:   Flags{QR: true, Opcode: 2, RCODE: 2},
				QDCount: 1,
				ANCount: 0,
				NSCount: 3,
				ARCount: 1,
			},
		},
		{
			name: "zero-value header",
			h:    Header{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			buf := Encode(tc.h)
			got, err := Decode(buf[:])
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got != tc.h {
				t.Fatalf("round-trip = %+v, want %+v", got, tc.h)
			}
		})
	}
}

// TestDecodeTruncated sweeps every buffer length shorter than HeaderSize and
// asserts Decode returns ErrShortBuffer instead of panicking on the
// slice-to-array conversion.
func TestDecodeTruncated(t *testing.T) {
	t.Parallel()

	full := Encode(Header{ID: 1, QDCount: 1})
	for n := 0; n < HeaderSize; n++ {
		_, err := Decode(full[:n])
		if !errors.Is(err, ErrShortBuffer) {
			t.Fatalf("Decode(%d bytes) err = %v, want ErrShortBuffer", n, err)
		}
	}
}

// TestFlagBitLayout pins the exact bit positions of the flags word against
// a hand-computed byte pattern, so a transposed bit cannot silently pass the
// round-trip test above (round-tripping alone cannot catch a bug where
// encode and decode agree with each other but disagree with the RFC).
func TestFlagBitLayout(t *testing.T) {
	t.Parallel()

	h := Header{
		Flags: Flags{
			QR:     true,
			Opcode: 0xf, // only the low 4 bits should end up in the wire form
			AA:     true,
			TC:     false,
			RD:     true,
			RA:     false,
			RCODE:  0xf,
		},
	}
	buf := Encode(h)

	// Byte 2: QR(1) Opcode(4) AA(1) TC(1) RD(1) = 1 1111 1 0 1 = 0xfd
	if buf[2] != 0xfd {
		t.Fatalf("flags high byte = %#02x, want %#02x", buf[2], 0xfd)
	}
	// Byte 3: RA(1) Z(3, zero) RCODE(4) = 0 000 1111 = 0x0f
	if buf[3] != 0x0f {
		t.Fatalf("flags low byte = %#02x, want %#02x", buf[3], 0x0f)
	}
}

// TestOpcodeAndRCODEAreMaskedToFourBits asserts that an out-of-range value
// (more than 4 bits) is silently masked down on encode, matching the wire
// field width, rather than corrupting adjacent bits.
func TestOpcodeAndRCODEAreMaskedToFourBits(t *testing.T) {
	t.Parallel()

	h := Header{Flags: Flags{Opcode: 0xff, RCODE: 0xff}}
	buf := Encode(h)
	decoded, err := Decode(buf[:])
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if decoded.Flags.Opcode != 0x0f {
		t.Fatalf("Opcode = %#x, want %#x", decoded.Flags.Opcode, 0x0f)
	}
	if decoded.Flags.RCODE != 0x0f {
		t.Fatalf("RCODE = %#x, want %#x", decoded.Flags.RCODE, 0x0f)
	}
}

// TestRun exercises the command end to end over strings.Reader and
// bytes.Buffer: a well-formed hex header, a truncated one (exit-worthy usage
// error), and input that is not valid hex at all.
func TestRun(t *testing.T) {
	t.Parallel()

	full := Encode(Header{ID: 0x1a2b, Flags: Flags{RD: true}, QDCount: 1})

	cases := []struct {
		name    string
		stdin   string
		want    string
		wantErr bool
	}{
		{
			name:  "well-formed query header",
			stdin: hex.EncodeToString(full[:]) + "\n",
			want:  "id=0x1a2b qr=false opcode=0 aa=false tc=false rd=true ra=false rcode=0 qdcount=1 ancount=0 nscount=0 arcount=0\n",
		},
		{
			name:    "truncated hex is a usage error",
			stdin:   hex.EncodeToString(full[:8]),
			wantErr: true,
		},
		{
			name:    "not valid hex is a usage error",
			stdin:   "not-hex-at-all",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stdout bytes.Buffer
			err := run(nil, strings.NewReader(tc.stdin), &stdout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("run: want error, got nil")
				}
				if !errors.Is(err, errUsage) {
					t.Fatalf("run error = %v, want it to wrap errUsage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout.String() != tc.want {
				t.Fatalf("run stdout = %q, want %q", stdout.String(), tc.want)
			}
		})
	}
}
```

## Review

The codec is correct when `Decode(Encode(h))` reproduces `h` exactly for
every representative header shape, when every buffer shorter than 12 bytes
returns `ErrShortBuffer` instead of panicking, and — the check that a
round-trip alone cannot provide — when the raw encoded bytes match RFC
1035's bit layout exactly. The mistake this exercise exists to catch: a bit
position swapped consistently between `encodeFlags` and `decodeFlags` would
still pass every round-trip test, because both functions would agree with
each other while disagreeing with every other DNS implementation on the
wire; `TestFlagBitLayout` is the only test here that would catch that,
because it checks against an independently hand-derived byte pattern instead
of against the package's own inverse function. `dnshdr` treats both bad hex
and a too-short buffer as exit-2 usage errors, since both are input the
caller must fix, and reserves exit 1 for an actual stdin read failure. Run
`go test -count=1 -race ./...` to confirm the round-trip table, the
truncation sweep, the bit layout, the masking behavior, and `run`'s
end-to-end behavior.

## Resources

- [RFC 1035, section 4.1.1](https://www.rfc-editor.org/rfc/rfc1035#section-4.1.1) — the exact 12-byte header layout this module encodes and decodes.
- [encoding/binary](https://pkg.go.dev/encoding/binary) — `BigEndian.Uint16`/`PutUint16` used for every integer field.
- [Go Specification: Conversions from slice to array or array pointer](https://go.dev/ref/spec#Conversions_from_slice_to_array_or_array_pointer) — the Go 1.20+ conversion `Decode` relies on after its length guard.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching the wrapped `ErrShortBuffer` sentinel.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [17-merkle-root-fixed-hash-arrays.md](17-merkle-root-fixed-hash-arrays.md) | Next: [19-bitset-array-value-copy-trap.md](19-bitset-array-value-copy-trap.md)
