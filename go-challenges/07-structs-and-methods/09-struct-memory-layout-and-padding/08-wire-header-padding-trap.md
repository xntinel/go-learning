# Exercise 8: Why you cannot memcpy a Go struct onto the wire

A binary protocol header has an exact byte layout the peer expects. A Go struct
with the same fields has *padding* that the wire format does not. This module
builds a protocol header, serializes it with `encoding/binary` (which walks
fields and ignores padding), and proves the wire size differs from the in-memory
size — the reason you must never `memcpy` a Go struct onto a socket. It contrasts
with `structs.HostLayout` for the genuine C-ABI case.

This module is fully self-contained: its own `go mod init`, its own demo, its own
test.

## What you'll build

```text
wireheader/                independent module: example.com/wireheader
  go.mod                   go 1.26
  header.go                type Header; Encode/Decode via encoding/binary; HostHeader
  cmd/
    demo/
      main.go              encodes a header, prints wire size vs in-memory size
  header_test.go           binary.Size != unsafe.Sizeof; round-trip; golden bytes
```

- Files: `header.go`, `cmd/demo/main.go`, `header_test.go`.
- Implement: a `Header{Magic, Version, Flags, Length, Checksum}` with big-endian `Encode`/`Decode` using `encoding/binary`, a `WireSize` accessor, and a `HostHeader` marked with `structs.HostLayout` for the FFI case.
- Test: assert `binary.Size(Header{}) != unsafe.Sizeof(Header{})`, that encode-then-decode reproduces the header, and pin the exact big-endian bytes with a golden test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/08-wire-header-padding-trap/cmd/demo
cd go-solutions/07-structs-and-methods/09-struct-memory-layout-and-padding/08-wire-header-padding-trap
```

### The padding gap between memory and the wire

The header has five fields: `Magic uint32`, `Version uint8`, `Flags uint8`,
`Length uint32`, `Checksum uint32`. Their sizes sum to 4+1+1+4+4 = 14 bytes, and
that is exactly what the protocol puts on the wire: fourteen bytes, no gaps. But
in memory Go must align `Length` (a `uint32`) to a 4-byte boundary. After
`Magic` (offset 0..3), `Version` (offset 4), and `Flags` (offset 5), the next
free byte is offset 6 — not a multiple of 4 — so the compiler inserts two padding
bytes and puts `Length` at offset 8, `Checksum` at 12. The in-memory struct is
16 bytes. Two of them are padding that exists only to satisfy Go's alignment
rules and has no meaning in the protocol.

This is why `binary.Size(Header{})` (14) differs from `unsafe.Sizeof(Header{})`
(16), and it is the whole reason you cannot serialize by casting the struct to a
byte slice. `encoding/binary` does the right thing: `binary.Write` *walks the
fields* in declaration order and writes each field's bytes with no padding, and
`binary.Read` reverses that. It never `memcpy`s the struct, so the padding never
reaches the wire. If you instead did `unsafe`-cast the struct to `[]byte` you
would emit 16 bytes including the two padding bytes, whose contents are undefined,
and a 32-bit peer — where the padding falls differently — would misparse every
message.

There is one legitimate case for matching host memory layout exactly: mapping a C
struct over cgo/FFI, or `mmap`-ing a kernel structure. For that Go 1.23 added
`structs.HostLayout`: a zero-size marker field placed first (`_ structs
.HostLayout`) that tells the compiler to use the host C ABI layout and opts the
type out of any future Go-specific layout freedom. It does not change the size of
a struct that already matches C layout (our `HostHeader` is still 16 bytes), but
it documents intent and guarantees the match. Use it for FFI; use `encoding
/binary` for wire protocols. They solve different problems.

Create `header.go`:

```go
// Package wireheader shows that a Go struct's in-memory layout (with padding) is
// not its wire format: encoding/binary walks fields and ignores padding.
package wireheader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"structs"
)

// Header is a fixed binary protocol header. In memory it is 16 bytes (two bytes
// of padding before Length); on the wire it is 14 bytes with no padding.
type Header struct {
	Magic    uint32
	Version  uint8
	Flags    uint8
	Length   uint32
	Checksum uint32
}

// HostHeader is the same fields marked for host C ABI layout, for cgo/FFI or
// mmap of a C struct. The marker guarantees the layout matches the C compiler's.
type HostHeader struct {
	_        structs.HostLayout
	Magic    uint32
	Version  uint8
	Flags    uint8
	Length   uint32
	Checksum uint32
}

// ErrShortHeader is returned by Decode when the input is smaller than the wire size.
var ErrShortHeader = errors.New("wireheader: input shorter than wire size")

// WireSize is the encoded size in bytes (no padding), 14 on every platform.
func WireSize() int { return binary.Size(Header{}) }

// Encode serializes h in big-endian wire order, field by field.
func Encode(h Header) ([]byte, error) {
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, h); err != nil {
		return nil, fmt.Errorf("encode header: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode parses a big-endian header from b. It requires at least WireSize bytes.
func Decode(b []byte) (Header, error) {
	if len(b) < WireSize() {
		return Header{}, fmt.Errorf("decode: have %d bytes, need %d: %w", len(b), WireSize(), ErrShortHeader)
	}
	var h Header
	if err := binary.Read(bytes.NewReader(b), binary.BigEndian, &h); err != nil {
		return Header{}, fmt.Errorf("decode header: %w", err)
	}
	return h, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"unsafe"

	"example.com/wireheader"
)

func main() {
	h := wireheader.Header{Magic: 0xCAFEBABE, Version: 1, Flags: 2, Length: 100, Checksum: 0xDEADBEEF}

	fmt.Printf("in-memory (unsafe.Sizeof): %d bytes\n", unsafe.Sizeof(h))
	fmt.Printf("on the wire (binary.Size):  %d bytes\n", wireheader.WireSize())

	b, err := wireheader.Encode(h)
	if err != nil {
		panic(err)
	}
	fmt.Printf("encoded: %x\n", b)

	got, err := wireheader.Decode(b)
	if err != nil {
		panic(err)
	}
	fmt.Printf("decoded: magic=%x version=%d flags=%d length=%d checksum=%x\n",
		got.Magic, got.Version, got.Flags, got.Length, got.Checksum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (on a 64-bit platform):

```
in-memory (unsafe.Sizeof): 16 bytes
on the wire (binary.Size):  14 bytes
encoded: cafebabe010200000064deadbeef
decoded: magic=cafebabe version=1 flags=2 length=100 checksum=deadbeef
```

### Tests

The gap test makes the padding explicit. The round-trip test proves encode/decode
are inverses. The golden test pins the exact bytes so an accidental field
reorder or endianness change fails loudly. A short-input test exercises the
sentinel error.

Create `header_test.go`:

```go
package wireheader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"unsafe"
)

func TestWireSizeDiffersFromMemory(t *testing.T) {
	t.Parallel()

	wire := binary.Size(Header{})
	mem := int(unsafe.Sizeof(Header{}))
	if wire == mem {
		t.Fatalf("wire size %d unexpectedly equals in-memory size %d; padding should differ", wire, mem)
	}
	if wire != 14 {
		t.Errorf("wire size = %d, want 14", wire)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	want := Header{Magic: 0xCAFEBABE, Version: 3, Flags: 0x80, Length: 4096, Checksum: 0x01020304}
	b, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(b) != WireSize() {
		t.Fatalf("encoded %d bytes, want %d", len(b), WireSize())
	}
	got, err := Decode(b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != want {
		t.Errorf("round-trip: got %+v, want %+v", got, want)
	}
}

func TestGoldenBytes(t *testing.T) {
	t.Parallel()

	h := Header{Magic: 0xCAFEBABE, Version: 1, Flags: 2, Length: 100, Checksum: 0xDEADBEEF}
	want := []byte{
		0xCA, 0xFE, 0xBA, 0xBE, // Magic
		0x01,                   // Version
		0x02,                   // Flags
		0x00, 0x00, 0x00, 0x64, // Length = 100
		0xDE, 0xAD, 0xBE, 0xEF, // Checksum
	}
	got, err := Encode(h)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("golden bytes:\n got  %x\n want %x", got, want)
	}
}

func TestDecodeShortInput(t *testing.T) {
	t.Parallel()

	_, err := Decode([]byte{0xCA, 0xFE})
	if !errors.Is(err, ErrShortHeader) {
		t.Fatalf("Decode(short) error = %v, want ErrShortHeader", err)
	}
}

func TestHostHeaderMatchesCLayout(t *testing.T) {
	t.Parallel()
	// The HostLayout marker is zero-size and does not change this already
	// C-compatible layout; it documents and guarantees the host ABI match.
	if got := unsafe.Sizeof(HostHeader{}); got != unsafe.Sizeof(Header{}) {
		t.Errorf("HostHeader size = %d, want %d", got, unsafe.Sizeof(Header{}))
	}
}
```

## Review

The header is correct when the wire form is 14 bytes regardless of the 16-byte
in-memory struct, the golden bytes match the documented big-endian encoding, and
encode/decode round-trip exactly. The mistake this exercise exists to prevent is
serializing by `memcpy` or `unsafe`-casting the struct to bytes: that leaks Go's
two padding bytes into the stream, and because padding placement varies across
`GOARCH`, a message written on one architecture would misparse on another. Walk
fields with `encoding/binary` and pin the encoding with a golden test. Reserve
`structs.HostLayout` for the opposite need — matching a host C struct over
cgo/FFI or `mmap` — where host layout is exactly what you want.

## Resources

- [encoding/binary: Read, Write, Size](https://pkg.go.dev/encoding/binary) — field-by-field serialization that ignores padding.
- [structs.HostLayout](https://pkg.go.dev/structs#HostLayout) — the marker for host C ABI layout in cgo/FFI.
- [unsafe.Sizeof](https://pkg.go.dev/unsafe#Sizeof) — the in-memory size, with padding, that the wire size differs from.

---

Back to [07-compact-hot-path-record.md](07-compact-hot-path-record.md) | Next: [09-empty-and-trailing-zero-size-fields.md](09-empty-and-trailing-zero-size-fields.md)
