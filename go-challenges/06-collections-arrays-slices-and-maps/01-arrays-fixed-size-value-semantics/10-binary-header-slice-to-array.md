# Exercise 10: Parsing a Fixed-Length Wire Header via []byte-to-[N]byte Conversion

Reading a fixed frame off the wire is where the Go 1.20 slice-to-array conversion
earns its place: `[4]byte(buf[:4])` copies four bytes into a comparable magic, and
`encoding/binary.BigEndian` decodes the integer fields. But the conversion panics on
a short slice, so the mandatory length guard is the whole point. This exercise
builds a header parser that checks the length before converting and proves the raw
conversion panics without it.

This module is fully self-contained. It begins with its own `go mod init`, defines
every type it needs, and ships its own demo and tests. Nothing here imports any
other exercise.

## What you'll build

```text
wireheader/                  independent module: example.com/wireheader
  go.mod
  header.go                  Header{Magic [4]byte; Version uint16; Length uint32}; ParseHeader; ErrShortBuffer/ErrBadMagic
  cmd/
    demo/
      main.go                runnable demo: parse a valid frame, reject a truncated one
  header_test.go             valid parse, truncated errors (no panic), raw-conversion panics, magic mismatch
```

- Files: `header.go`, `cmd/demo/main.go`, `header_test.go`.
- Implement: `ParseHeader(buf []byte) (Header, error)` reading a `[4]byte` magic, a `uint16` version, and a `uint32` length via `[4]byte(buf[:4])` and `binary.BigEndian`, guarded by a length check; sentinel errors `ErrShortBuffer` and `ErrBadMagic`.
- Test: a valid frame parses; a truncated buffer errors instead of panicking; the raw `[4]byte(short)` conversion panics (recover-based); a magic mismatch is rejected.
- Verify: `go test -count=1 -race ./...`

### Why the length guard is mandatory

The frame layout is fixed: 4 bytes of magic, 2 bytes of version (big-endian), 4
bytes of length (big-endian) — 10 bytes total. `Magic` is a `[4]byte` because a
protocol magic is exactly four bytes and comparing it against the expected value
with `==` is the natural check; a comparable array is perfect here.

The Go 1.20 conversion `[4]byte(buf[:4])` copies the first four bytes of the slice
into a `[4]byte` value. It is concise and idiomatic — but it **panics** if the
slice is shorter than four bytes. The pointer form `(*[4]byte)(buf)` aliases instead
of copying and panics on the same condition. This is the single most important
safety rule at a wire boundary: an attacker (or a buggy peer, or a partial read)
can hand you a buffer shorter than the header, and an unguarded conversion turns
that into a panic that takes down the goroutine — a remote denial of service.

So `ParseHeader` checks `len(buf) < HeaderSize` and returns `ErrShortBuffer`
*before* any conversion. Only after the guard does it convert
`[4]byte(buf[0:4])` for the magic and decode the integers with
`binary.BigEndian.Uint16(buf[4:6])` and `binary.BigEndian.Uint32(buf[6:10])`. The
sentinel errors are wrapped with `%w` (via `fmt.Errorf`) so callers can match them
with `errors.Is`. The magic is compared with `==` against the expected `[4]byte`;
a mismatch returns `ErrBadMagic`.

The tests include a deliberate demonstration that the *unguarded* conversion panics,
using `recover`, to justify why the guard is not optional. That is the honest way to
document a panic condition: show it, then show the code that prevents it.

Create `header.go`:

```go
package wireheader

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// HeaderSize is the fixed on-wire header length: 4 (magic) + 2 (version) + 4 (length).
const HeaderSize = 10

// Magic is the expected 4-byte protocol magic ("WIRE").
var Magic = [4]byte{'W', 'I', 'R', 'E'}

// Sentinel errors, matchable with errors.Is.
var (
	ErrShortBuffer = errors.New("wireheader: buffer shorter than header")
	ErrBadMagic    = errors.New("wireheader: bad magic")
)

// Header is a parsed fixed-length wire header.
type Header struct {
	Magic   [4]byte
	Version uint16
	Length  uint32
}

// ParseHeader reads a HeaderSize-byte header from buf. It guards the length before
// any slice-to-array conversion, so a truncated buffer returns ErrShortBuffer
// instead of panicking.
func ParseHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, fmt.Errorf("have %d bytes: %w", len(buf), ErrShortBuffer)
	}
	var h Header
	// Safe now: the length is guaranteed >= 4 by the guard above.
	h.Magic = [4]byte(buf[0:4])
	h.Version = binary.BigEndian.Uint16(buf[4:6])
	h.Length = binary.BigEndian.Uint32(buf[6:10])

	if h.Magic != Magic {
		return Header{}, fmt.Errorf("got %q: %w", h.Magic, ErrBadMagic)
	}
	return h, nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/binary"
	"errors"
	"fmt"

	"example.com/wireheader"
)

func main() {
	// Build a valid frame: magic "WIRE", version 1, length 4096.
	buf := make([]byte, wireheader.HeaderSize)
	copy(buf[0:4], []byte("WIRE"))
	binary.BigEndian.PutUint16(buf[4:6], 1)
	binary.BigEndian.PutUint32(buf[6:10], 4096)

	h, err := wireheader.ParseHeader(buf)
	if err != nil {
		panic(err)
	}
	fmt.Printf("magic=%q version=%d length=%d\n", h.Magic, h.Version, h.Length)

	// A truncated buffer errors instead of panicking.
	_, err = wireheader.ParseHeader(buf[:5])
	fmt.Printf("truncated: short=%v\n", errors.Is(err, wireheader.ErrShortBuffer))

	// A frame with the wrong magic is rejected.
	bad := make([]byte, wireheader.HeaderSize)
	copy(bad[0:4], []byte("XXXX"))
	_, err = wireheader.ParseHeader(bad)
	fmt.Printf("bad magic: %v\n", errors.Is(err, wireheader.ErrBadMagic))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
magic="WIRE" version=1 length=4096
truncated: short=true
bad magic: true
```

### Tests

`TestParseValid` builds a well-formed frame and asserts the parsed magic, version,
and length. `TestParseTruncated` feeds buffers shorter than the header and asserts
`ParseHeader` returns `ErrShortBuffer` (matched with `errors.Is`) and does not
panic. `TestRawConversionPanics` demonstrates with `recover` that the unguarded
`[4]byte(short)` conversion panics, justifying the guard. `TestBadMagic` asserts a
frame with the wrong magic returns `ErrBadMagic`.

Create `header_test.go`:

```go
package wireheader

import (
	"encoding/binary"
	"errors"
	"testing"
)

func validFrame(t *testing.T, version uint16, length uint32) []byte {
	t.Helper()
	buf := make([]byte, HeaderSize)
	copy(buf[0:4], Magic[:])
	binary.BigEndian.PutUint16(buf[4:6], version)
	binary.BigEndian.PutUint32(buf[6:10], length)
	return buf
}

func TestParseValid(t *testing.T) {
	t.Parallel()

	h, err := ParseHeader(validFrame(t, 7, 65535))
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if h.Magic != Magic {
		t.Fatalf("magic = %q, want %q", h.Magic, Magic)
	}
	if h.Version != 7 {
		t.Fatalf("version = %d, want 7", h.Version)
	}
	if h.Length != 65535 {
		t.Fatalf("length = %d, want 65535", h.Length)
	}
}

func TestParseTruncated(t *testing.T) {
	t.Parallel()

	full := validFrame(t, 1, 1)
	for n := 0; n < HeaderSize; n++ {
		_, err := ParseHeader(full[:n])
		if !errors.Is(err, ErrShortBuffer) {
			t.Fatalf("ParseHeader(%d bytes) err = %v, want ErrShortBuffer", n, err)
		}
	}
}

func TestRawConversionPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("[4]byte(short) should panic on a slice shorter than 4")
		}
	}()
	short := []byte{1, 2} // only 2 bytes
	_ = [4]byte(short)    // panics: cannot convert, slice too short
}

func TestBadMagic(t *testing.T) {
	t.Parallel()

	buf := validFrame(t, 1, 1)
	buf[0] = 'X' // corrupt the magic
	_, err := ParseHeader(buf)
	if !errors.Is(err, ErrBadMagic) {
		t.Fatalf("err = %v, want ErrBadMagic", err)
	}
}
```

## Review

The parser is correct when it decodes a well-formed frame to the right magic,
version, and length, and — the load-bearing property — returns `ErrShortBuffer`
rather than panicking on any buffer shorter than the 10-byte header.
`TestParseTruncated` sweeps every short length from 0 to 9; `TestRawConversionPanics`
shows what happens without the guard, which is exactly why the guard exists. The
`[4]byte` magic being comparable makes the `== Magic` check trivial, and the
sentinel errors wrapped with `%w` let callers branch with `errors.Is`. The mistake
to never make: converting `[N]byte(s)` or `(*[N]byte)(s)` at a wire boundary without
first checking `len(s) >= N`. Run `go test -race` to confirm the valid parse, the
truncation errors, the deliberate panic, and the magic rejection.

## Resources

- [Go Specification: Conversions from slice to array or array pointer](https://go.dev/ref/spec#Conversions_from_slice_to_array_or_array_pointer) — the Go 1.20+ conversion and its panic condition.
- [encoding/binary](https://pkg.go.dev/encoding/binary) — `BigEndian.Uint16`/`Uint32` and `PutUint16`/`PutUint32`.
- [errors.Is](https://pkg.go.dev/errors#Is) — matching wrapped sentinel errors.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-fixed-window-rate-limiter.md](09-fixed-window-rate-limiter.md) | Next: [11-array-value-semantics-config-snapshot.md](11-array-value-semantics-config-snapshot.md)
