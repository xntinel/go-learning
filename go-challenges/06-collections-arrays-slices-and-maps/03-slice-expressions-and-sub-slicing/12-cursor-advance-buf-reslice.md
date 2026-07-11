# Exercise 12: A Binary Header Reader That Advances a Cursor by Reslicing

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A custom TCP framing layer or an internal RPC wire format typically opens
with a small binary header — a version byte, a message-type code, a payload
length — read field by field off the front of a `[]byte`. The idiomatic way
to track "how far have I read" in Go is not a separate offset variable: it is
the slice itself. Each read takes what it needs off the front and reslices,
`buf = buf[n:]`, so the cursor's position is implicit in `buf`'s own length.
The failure mode this exercise is really about is what a first-draft reader
does when a field does not fit: slicing before checking, instead of checking
before slicing, turns an ordinary truncated message into an unrecovered
panic instead of a typed error a caller can log and move past.

This exercise builds that reader as a small reusable package: a `Reader`
type wrapping a `[]byte`, four field-width read methods, and a `ReadHeader`
helper that chains them in wire order. Every read method checks its length
requirement before it ever slices, so a truncated field always reports a
clean `io.ErrUnexpectedEOF` and leaves the cursor exactly where it was.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
binreader/                 module example.com/binreader
  go.mod                   go 1.24
  binreader.go              Header; Reader with Uint8/Uint16/Uint32/Bytes/Remaining; New; ReadHeader
  binreader_test.go         full decode, truncation at every field boundary, short Bytes(n),
                            cursor advance, the naive-panic contrast, ExampleReadHeader
```

- Files: `binreader.go`, `binreader_test.go`.
- Implement: `New(buf []byte) *Reader` wrapping `buf` without copying it; each
  of `Uint8`, `Uint16`, `Uint32`, and `Bytes(n)` checks `len(r.buf) < n` before
  slicing, returns `io.ErrUnexpectedEOF` without consuming anything if the
  check fails, and otherwise reads and reslices `r.buf = r.buf[n:]`;
  `Remaining()` reports `len(r.buf)`; `ReadHeader(r *Reader) (Header, error)`
  chains the three fixed-width reads in wire order.
- Test: a full header-plus-payload decode; every truncation point across the
  header's three fields; `Bytes(n)` asked for more than remains after a valid
  header; the cursor's `Remaining()` shrinking by exactly each field's width;
  an unexported `uint32Naive` contrasted against `Reader.Uint32` on the same
  short input; and `ExampleReadHeader` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/binreader
cd ~/go-exercises/binreader
go mod init example.com/binreader
go mod edit -go=1.24
```

### Reslicing as a cursor, and checking length before slicing, not after

There are two common ways to track a read position into a byte buffer: keep
an explicit `pos int` alongside the original slice and index everything as
`buf[pos:pos+n]`, or keep no position at all and instead reassign the slice
itself after every read, `buf = buf[n:]`. The second is the idiomatic Go
shape for a one-directional binary reader, and it works because a slice
expression's length *is* a position: after `buf = buf[n:]`, `len(buf)` is
exactly the count of unconsumed bytes, no separate bookkeeping required. Every
read method's bounds check collapses to one comparison, `len(r.buf) < n`, and
that comparison is simultaneously "do I have enough data" and "how much is
left" — `len()` is the remaining budget, not just a length.

This is the same reslicing mechanic this lesson has used to isolate windows
and trim heads and tails, applied to a different purpose: instead of handing
the sub-slice to a caller, the reader keeps re-deriving `buf[n:]` as its own
new state on every call. Because reslicing is a header operation with no
copying, advancing the cursor through even a large buffer costs nothing —
`Bytes(n)` returns a *view* into the original array, not a copy, which is
correct for a value the caller decodes immediately but would need
`bytes.Clone` if kept past further reads (the ephemeral-buffer hazard from
earlier in this lesson applies here too).

The order of operations is what this exercise actually pins. A first-draft
implementation reaches for the field it needs and checks length afterward, or
not at all:

```go
func uint32Naive(buf []byte) (uint32, []byte) {
	v := binary.BigEndian.Uint32(buf[:4])   // panics if len(buf) < 4
	return v, buf[4:]
}
```

`buf[:4]` on a two-byte buffer panics with a slice-bounds-out-of-range
before `binary.BigEndian.Uint32` ever runs. A truncated message from a flaky
connection then crashes the goroutine reading it instead of surfacing as a
value the caller can check and log. `Reader.Uint32` reorders the two steps:
`len(r.buf) < 4` is checked *before* the slice expression, so the exact same
truncated input returns `io.ErrUnexpectedEOF` — the standard library's
sentinel for "started reading a fixed-size unit and ran out partway through"
— and leaves the cursor untouched. A failed read must never consume the
cursor; a caller that catches the error and inspects `Remaining()` should see
an honest, unconsumed count.

Create `binreader.go`:

```go
// Package binreader decodes a small binary protocol header -- the kind a
// custom TCP framing layer or an internal RPC wire format uses ahead of its
// payload -- by walking a []byte with a cursor that advances via reslicing.
package binreader

import (
	"encoding/binary"
	"io"
)

// Header is one decoded protocol header: a version byte, a message-type
// code, and the length in bytes of the payload that follows it on the wire.
type Header struct {
	Version    uint8
	MsgType    uint16
	PayloadLen uint32
}

// Reader decodes fixed-width fields from buf, advancing a cursor through it
// by reslicing: after every successful read, buf = buf[n:] drops the bytes
// just consumed, so len(buf) is always exactly the number of bytes left to
// decode. There is no separate offset field -- the slice length is the
// remaining budget.
//
// A Reader is not safe for concurrent use: every read method mutates the
// cursor. Callers that need to decode the same underlying bytes from
// multiple goroutines must construct one Reader per goroutine.
type Reader struct {
	buf []byte
}

// New wraps buf for cursor-style reading. It does not copy buf; Reader
// reslices the same backing array as it consumes it, so mutating buf through
// any other reference while decoding is still in progress is a data race on
// the bytes, not on the Reader itself.
func New(buf []byte) *Reader {
	return &Reader{buf: buf}
}

// Remaining reports how many unconsumed bytes are left in the cursor.
func (r *Reader) Remaining() int {
	return len(r.buf)
}

// Uint8 reads one byte and advances the cursor past it. If no byte remains,
// it returns io.ErrUnexpectedEOF and leaves the cursor untouched.
func (r *Reader) Uint8() (uint8, error) {
	if len(r.buf) < 1 {
		return 0, io.ErrUnexpectedEOF
	}
	v := r.buf[0]
	r.buf = r.buf[1:]
	return v, nil
}

// Uint16 reads a big-endian uint16 and advances the cursor past it. If fewer
// than 2 bytes remain, it returns io.ErrUnexpectedEOF and leaves the cursor
// untouched.
func (r *Reader) Uint16() (uint16, error) {
	if len(r.buf) < 2 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.BigEndian.Uint16(r.buf[:2])
	r.buf = r.buf[2:]
	return v, nil
}

// Uint32 reads a big-endian uint32 and advances the cursor past it. If fewer
// than 4 bytes remain, it returns io.ErrUnexpectedEOF and leaves the cursor
// untouched.
func (r *Reader) Uint32() (uint32, error) {
	if len(r.buf) < 4 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.BigEndian.Uint32(r.buf[:4])
	r.buf = r.buf[4:]
	return v, nil
}

// Bytes reads and returns the next n bytes, advancing the cursor past them.
// If fewer than n bytes remain, it returns io.ErrUnexpectedEOF and leaves
// the cursor untouched.
//
// The returned slice aliases r's underlying buffer; it is a view, not a
// copy. It is valid until the next call to any Reader method that advances
// the cursor over the same bytes cannot happen again, but a caller that
// retains it past further reads of buf through another reference, or past
// the buffer being reused (a pooled read buffer, for example), must
// bytes.Clone it first.
func (r *Reader) Bytes(n int) ([]byte, error) {
	if len(r.buf) < n {
		return nil, io.ErrUnexpectedEOF
	}
	v := r.buf[:n]
	r.buf = r.buf[n:]
	return v, nil
}

// ReadHeader decodes a Header (version, message type, payload length) from
// r, one field at a time, each field advancing the cursor left to right in
// wire order. It stops at the first field that does not fit and returns
// io.ErrUnexpectedEOF, so a caller can tell a truncated header from a
// malformed one.
func ReadHeader(r *Reader) (Header, error) {
	version, err := r.Uint8()
	if err != nil {
		return Header{}, err
	}
	msgType, err := r.Uint16()
	if err != nil {
		return Header{}, err
	}
	payloadLen, err := r.Uint32()
	if err != nil {
		return Header{}, err
	}
	return Header{Version: version, MsgType: msgType, PayloadLen: payloadLen}, nil
}
```

### Using it

`New` wraps a `[]byte` with no copying and no validation to reject — any
buffer, including `nil`, is a legal starting point, and every read method's
own length check is what turns a short buffer into a clean error rather than
a panic. Call the field-width read methods (`Uint8`, `Uint16`, `Uint32`,
`Bytes`) in wire order, or use `ReadHeader` for the specific three-field
header this package defines; either way, `Remaining()` always tells you
exactly how many bytes are left to decode.

The values `Bytes` returns are documented as views, not copies: `Reader`
never allocates to satisfy a read, which is what makes it cheap to run over
every message on a hot connection, but it also means a caller that keeps a
returned slice past the buffer being reused must clone it first — the same
ephemeral-buffer discipline this lesson applies to `bufio.Scanner.Bytes()`.
A `Reader` is not safe for concurrent use, since every read mutates its
cursor; a connection with one reader goroutine should own one `Reader`.

The module has no `main.go`, because a binary field reader is a library, not
a tool. Its executable demonstration is `ExampleReadHeader`: `go test` runs
it and compares its standard output against the `// Output:` comment, so the
usage shown here cannot drift away from the code.

```go
func ExampleReadHeader() {
	r := New(fullMessage)

	h, err := ReadHeader(r)
	if err != nil {
		panic(err)
	}
	fmt.Printf("header: %+v remaining=%d\n", h, r.Remaining())

	payload, err := r.Bytes(int(h.PayloadLen))
	if err != nil {
		panic(err)
	}
	fmt.Printf("payload: %q remaining=%d\n", payload, r.Remaining())

	short := New(fullMessage[:3])
	if _, err := ReadHeader(short); errors.Is(err, io.ErrUnexpectedEOF) {
		fmt.Println("truncated header:", err)
	}

	// Output:
	// header: {Version:1 MsgType:2 PayloadLen:5} remaining=5
	// payload: "hello" remaining=0
	// truncated header: unexpected EOF
}
```

### Tests

The full-decode test drives a header and its payload end to end, checking
`Remaining()` at each step to pin the cursor's arithmetic. The truncation
table hits every field boundary a real short read could stop at — nothing,
just the version byte, a partial `msgType`, and so on — and confirms each one
reports `io.ErrUnexpectedEOF`. The short-`Bytes` test is the boundary between
header and payload: a header that declares more payload than the buffer
actually holds. The cursor-advance test isolates `Remaining()`'s arithmetic
from decoding correctness by checking it shrinks by exactly each field's
width, not merely "some amount."

`uint32Naive` is unexported and unreachable from the package API; it exists
so `TestNaiveUint32PanicsOnShortBuffer` can pin the module's actual lesson
numerically: slicing before checking length panics on a two-byte buffer,
where `Reader.Uint32` on the identical input returns `io.ErrUnexpectedEOF`.
If a future edit reorders `Uint32`'s check to run after the slice, this is
the exact failure it would reintroduce.

Create `binreader_test.go`:

```go
package binreader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

// fullMessage is one complete header (version 1, msgType 2, payloadLen 5)
// followed by its 5-byte payload "hello": 1 + 2 + 4 + 5 = 12 bytes.
var fullMessage = []byte{
	0x01,       // version
	0x00, 0x02, // msgType
	0x00, 0x00, 0x00, 0x05, // payloadLen
	'h', 'e', 'l', 'l', 'o', // payload
}

func TestReadHeaderAndPayload(t *testing.T) {
	t.Parallel()

	r := New(fullMessage)
	if got := r.Remaining(); got != len(fullMessage) {
		t.Fatalf("Remaining() before read = %d, want %d", got, len(fullMessage))
	}

	h, err := ReadHeader(r)
	if err != nil {
		t.Fatalf("ReadHeader() unexpected error: %v", err)
	}
	want := Header{Version: 1, MsgType: 2, PayloadLen: 5}
	if h != want {
		t.Fatalf("ReadHeader() = %+v, want %+v", h, want)
	}
	if got := r.Remaining(); got != 5 {
		t.Fatalf("Remaining() after header = %d, want 5", got)
	}

	payload, err := r.Bytes(int(h.PayloadLen))
	if err != nil {
		t.Fatalf("Bytes(%d) unexpected error: %v", h.PayloadLen, err)
	}
	if !bytes.Equal(payload, []byte("hello")) {
		t.Fatalf("payload = %q, want %q", payload, "hello")
	}
	if got := r.Remaining(); got != 0 {
		t.Fatalf("Remaining() after payload = %d, want 0", got)
	}
}

func TestReadHeaderTruncatedAtEachFieldBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		buf  []byte
	}{
		{"empty buffer", nil},
		{"only version", fullMessage[:1]},
		{"version and partial msgType", fullMessage[:2]},
		{"version and msgType, no payloadLen", fullMessage[:3]},
		{"version, msgType, partial payloadLen", fullMessage[:5]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := New(tc.buf)
			_, err := ReadHeader(r)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("ReadHeader() err = %v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestBytesRejectsShortRemainder(t *testing.T) {
	t.Parallel()

	r := New(fullMessage)
	if _, err := ReadHeader(r); err != nil {
		t.Fatalf("ReadHeader() unexpected error: %v", err)
	}

	// Header declared 5 bytes of payload but ask for 6: too few remain.
	if _, err := r.Bytes(6); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Bytes(6) err = %v, want io.ErrUnexpectedEOF", err)
	}
	// A failed read must not consume the cursor.
	if got := r.Remaining(); got != 5 {
		t.Fatalf("Remaining() after failed Bytes = %d, want 5 (unconsumed)", got)
	}
}

func TestCursorAdvancesExactlyByFieldWidth(t *testing.T) {
	t.Parallel()

	r := New(fullMessage)
	start := r.Remaining()

	if _, err := r.Uint8(); err != nil {
		t.Fatal(err)
	}
	if got, want := r.Remaining(), start-1; got != want {
		t.Fatalf("Remaining() after Uint8 = %d, want %d", got, want)
	}

	if _, err := r.Uint16(); err != nil {
		t.Fatal(err)
	}
	if got, want := r.Remaining(), start-1-2; got != want {
		t.Fatalf("Remaining() after Uint16 = %d, want %d", got, want)
	}

	if _, err := r.Uint32(); err != nil {
		t.Fatal(err)
	}
	if got, want := r.Remaining(), start-1-2-4; got != want {
		t.Fatalf("Remaining() after Uint32 = %d, want %d", got, want)
	}
}

// uint32Naive is the shape a first draft of Uint32 usually takes: slice the
// four bytes it needs, then decode. It is never exported and never
// reachable from the package API; it exists only so the test below can pin
// what it gets wrong on a short buffer.
func uint32Naive(buf []byte) (uint32, []byte) {
	v := binary.BigEndian.Uint32(buf[:4])
	return v, buf[4:]
}

// TestNaiveUint32PanicsOnShortBuffer is the point of this module's
// contrast: checking length *after* slicing (or not at all) turns an
// ordinary truncated read into a panic instead of the io.ErrUnexpectedEOF a
// caller can handle. Reader.Uint32 checks len(r.buf) < 4 before it ever
// slices, so the same input that panics here returns a clean error there.
func TestNaiveUint32PanicsOnShortBuffer(t *testing.T) {
	t.Parallel()

	short := fullMessage[:2] // only 2 bytes: not enough for a uint32

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("uint32Naive(short) did not panic; want a slice-bounds panic")
		}
	}()
	_, _ = uint32Naive(short)
}

// ExampleReadHeader is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExampleReadHeader() {
	r := New(fullMessage)

	h, err := ReadHeader(r)
	if err != nil {
		panic(err)
	}
	fmt.Printf("header: %+v remaining=%d\n", h, r.Remaining())

	payload, err := r.Bytes(int(h.PayloadLen))
	if err != nil {
		panic(err)
	}
	fmt.Printf("payload: %q remaining=%d\n", payload, r.Remaining())

	short := New(fullMessage[:3])
	if _, err := ReadHeader(short); errors.Is(err, io.ErrUnexpectedEOF) {
		fmt.Println("truncated header:", err)
	}

	// Output:
	// header: {Version:1 MsgType:2 PayloadLen:5} remaining=5
	// payload: "hello" remaining=0
	// truncated header: unexpected EOF
}
```

## Review

The reader is correct when every field-read either advances the cursor by
exactly that field's width or leaves it completely untouched and reports
`io.ErrUnexpectedEOF` — there is no partial-consumption state in between. The
truncation table is the load-bearing test: it forces the bounds check in
every one of the four read methods to fire at least once, and
`TestNaiveUint32PanicsOnShortBuffer` pins the exact defect a reordered check
would reintroduce: slicing before validating length turns a routine
truncated read into an unrecovered panic. The design mistake this reader
also avoids is tracking position with a separate `pos int` alongside the
original, unresliced buffer — it works, but every read site then needs its
own `buf[pos:pos+n]` arithmetic and its own off-by-one risk, where reslicing
collapses "how much is left" into `len(buf)` for free. `Reader` is not safe
for concurrent use by design, since every read mutates its cursor.
`ExampleReadHeader` is the executable documentation: `go test` verifies its
output. Run `go test -count=1 -race ./...`.

## Resources

- [Go Specification: Slice expressions](https://go.dev/ref/spec#Slice_expressions)
- [`encoding/binary`](https://pkg.go.dev/encoding/binary)
- [`io.ErrUnexpectedEOF`](https://pkg.go.dev/io#ErrUnexpectedEOF)
- [`bytes.Clone`](https://pkg.go.dev/bytes#Clone) — what a caller must apply to `Bytes(n)`'s result before retaining it past the buffer's reuse.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-fixed-layout-record-offsets.md](11-fixed-layout-record-offsets.md) | Next: [13-chunked-transfer-partial-frame-retain.md](13-chunked-transfer-partial-frame-retain.md)
