# Exercise 1: PUSH_PROMISE Frame Encoding

A PUSH_PROMISE frame is how a server announces an intended push: it travels on the client's request stream, names the even-numbered stream that will carry the pushed response, and carries the HPACK-compressed headers of the synthetic request that response answers. This first module is responsible for framing only — it takes an already-compressed header block and produces the exact bytes that go on the wire.

Every module in this lesson is fully self-contained: it begins with its own `go mod init`, defines all the code it needs inline, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
push-promise-frame/
  go.mod
  push.go              FrameType, FramePushPromise, FlagEndHeaders, PushPromiseFrame, Encode
  push_test.go         byte-layout, undersized-buffer, reserved-bit masking
  cmd/demo/main.go     encode one PUSH_PROMISE and dump the wire bytes
```

- Files: `push.go`, `push_test.go`, `cmd/demo/main.go`.
- Implement: `PushPromiseFrame` with `Encode(p []byte) (int, error)`, plus the `FrameType`, `FramePushPromise`, and `FlagEndHeaders` constants.
- Test: the encoded length, type, flags, and both stream-ID fields match the wire layout; an undersized buffer returns an error; the reserved high bit is masked off.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Separating framing from compression

`Encode` writes two fixed prefixes and then copies a caller-supplied header block. The first prefix is the 9-byte HTTP/2 frame header (RFC 9113 §4.1): a 3-byte big-endian payload length, a 1-byte type, a 1-byte flags field, and a 4-byte field whose top bit is reserved and whose low 31 bits are the stream identifier. The second prefix is the 4-byte field unique to PUSH_PROMISE: a reserved bit plus the 31-bit promised stream ID. After those 13 bytes comes the HPACK header block verbatim.

The payload length is `4 + len(HeaderBlock)` — the promised-ID field plus the header block — and the total frame is `9 + payloadLen`. `Encode` checks the destination buffer can hold the whole frame and returns an error rather than writing a truncated frame, because a short write on a binary protocol corrupts every following frame on the connection. Both 32-bit stream-ID fields are masked with `& 0x7fffffff` before being written: RFC 9113 §4.1 defines the high bit as reserved and requires it to be zero on send, and masking guarantees that even if a caller passes an ID with that bit set, the wire stays conformant. The `END_HEADERS` flag is set unconditionally because this encoder handles the single-frame case where the entire header block fits in one PUSH_PROMISE; a real multi-frame block would clear it on all but the last CONTINUATION frame.

Create `push.go`:

```go
package push

import (
	"encoding/binary"
	"fmt"
)

// FrameType identifies an HTTP/2 frame type (RFC 9113 §6).
type FrameType uint8

const (
	// FramePushPromise is the frame type byte for PUSH_PROMISE (RFC 9113 §6.6).
	FramePushPromise FrameType = 0x05

	// FlagEndHeaders signals that the header block is complete in this frame.
	FlagEndHeaders uint8 = 0x04
)

// PushPromiseFrame is a PUSH_PROMISE frame ready to encode to the wire.
type PushPromiseFrame struct {
	// AssociatedStreamID is the client-initiated stream on which the promise
	// is delivered. It must be odd and non-zero (RFC 9113 §6.6).
	AssociatedStreamID uint32

	// PromisedStreamID is the server-initiated stream that will carry the
	// pushed response. It must be even and non-zero.
	PromisedStreamID uint32

	// HeaderBlock is the HPACK-compressed request headers for the pushed
	// resource. The caller is responsible for HPACK encoding; this type
	// handles framing only.
	HeaderBlock []byte
}

// Encode serializes f into p using the HTTP/2 frame wire format (RFC 9113 §4.1
// and §6.6). p must be large enough to hold the frame. Encode returns the
// number of bytes written and an error if p is too small.
//
// Wire layout:
//
//	+-----------------------------------------------+
//	|                 Length (24)                   |
//	+---------------+---------------+---------------+
//	|   Type (8)    |   Flags (8)   |
//	+-+-------------+---------------+-------------------------------+
//	|R|          Stream Identifier (31)                             |
//	+=+=============================================================+
//	|R|               Promised Stream ID (31)                       |
//	+---------------------------------------------------------------+
//	|                 Header Block Fragment (..)                    |
//	+---------------------------------------------------------------+
func (f PushPromiseFrame) Encode(p []byte) (int, error) {
	payloadLen := 4 + len(f.HeaderBlock) // 4 bytes: reserved(1) + promised ID(31)
	frameLen := 9 + payloadLen           // 9-byte frame header + payload
	if len(p) < frameLen {
		return 0, fmt.Errorf("push: buffer too small: need %d bytes, have %d", frameLen, len(p))
	}

	// 3-byte payload length, big-endian.
	p[0] = byte(payloadLen >> 16)
	p[1] = byte(payloadLen >> 8)
	p[2] = byte(payloadLen)
	// Frame type.
	p[3] = byte(FramePushPromise)
	// Flags: END_HEADERS (single-frame header block assumed).
	p[4] = FlagEndHeaders
	// Reserved(1) + AssociatedStreamID(31), big-endian.
	binary.BigEndian.PutUint32(p[5:9], f.AssociatedStreamID&0x7fffffff)
	// Reserved(1) + PromisedStreamID(31), big-endian.
	binary.BigEndian.PutUint32(p[9:13], f.PromisedStreamID&0x7fffffff)
	// Header block.
	n := copy(p[13:], f.HeaderBlock)
	return 9 + 4 + n, nil
}
```

### The runnable demo

The demo encodes one PUSH_PROMISE with a two-byte header block — `0x82` is the HPACK static-table entry for `:method GET` and `0x84` is `:path /`, both indexed representations — then prints the raw wire bytes and decodes each header field back out so the layout is visible.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	push "example.com/push-promise-frame"
)

func main() {
	// 0x82 = :method GET (HPACK static table entry 2, indexed representation).
	// 0x84 = :path /   (HPACK static table entry 4, indexed representation).
	hblock := []byte{0x82, 0x84}
	frame := push.PushPromiseFrame{
		AssociatedStreamID: 1,
		PromisedStreamID:   2,
		HeaderBlock:        hblock,
	}
	buf := make([]byte, 256)
	n, err := frame.Encode(buf)
	if err != nil {
		fmt.Printf("frame encode error: %v\n", err)
		return
	}
	fmt.Printf("PUSH_PROMISE wire bytes (%d total): %x\n", n, buf[:n])
	fmt.Printf("  payload length:    %d\n", int(buf[0])<<16|int(buf[1])<<8|int(buf[2]))
	fmt.Printf("  type:              0x%02x (PUSH_PROMISE)\n", buf[3])
	fmt.Printf("  flags:             0x%02x (END_HEADERS)\n", buf[4])
	fmt.Printf("  assoc stream ID:   %d\n", int(buf[5])<<24|int(buf[6])<<16|int(buf[7])<<8|int(buf[8]))
	fmt.Printf("  promised stream:   %d\n", int(buf[9])<<24|int(buf[10])<<16|int(buf[11])<<8|int(buf[12]))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
PUSH_PROMISE wire bytes (15 total): 000006050400000001000000028284
  payload length:    6
  type:              0x05 (PUSH_PROMISE)
  flags:             0x04 (END_HEADERS)
  assoc stream ID:   1
  promised stream:   2
```

The 15 bytes are the 9-byte header, the 4-byte promised-ID field, and the 2-byte header block. The payload length field reads 6 — the 4-byte promised ID plus the 2 header bytes — exactly the value `Encode` computed.

### Tests

The tests pin the byte layout field by field, confirm an undersized buffer is refused rather than truncated, and confirm the reserved high bit is masked on both stream-ID fields even when a caller sets it.

Create `push_test.go`:

```go
package push

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestPushPromiseFrameEncode(t *testing.T) {
	t.Parallel()

	hblock := []byte{0x82, 0x84} // simplified HPACK: :method GET, :path /index.html
	f := PushPromiseFrame{
		AssociatedStreamID: 1,
		PromisedStreamID:   2,
		HeaderBlock:        hblock,
	}

	buf := make([]byte, 256)
	n, err := f.Encode(buf)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	payloadLen := 4 + len(hblock)
	wantLen := 9 + payloadLen
	if n != wantLen {
		t.Fatalf("Encode returned %d bytes, want %d", n, wantLen)
	}

	// 3-byte length field.
	gotLen := int(buf[0])<<16 | int(buf[1])<<8 | int(buf[2])
	if gotLen != payloadLen {
		t.Fatalf("length field = %d, want %d", gotLen, payloadLen)
	}
	// Frame type 0x05.
	if buf[3] != byte(FramePushPromise) {
		t.Fatalf("type = 0x%02x, want 0x05", buf[3])
	}
	// END_HEADERS flag.
	if buf[4]&FlagEndHeaders == 0 {
		t.Fatalf("END_HEADERS not set in flags byte 0x%02x", buf[4])
	}
	// Associated stream ID in bytes [5:9].
	sid := binary.BigEndian.Uint32(buf[5:9]) & 0x7fffffff
	if sid != 1 {
		t.Fatalf("associated stream ID = %d, want 1", sid)
	}
	// Promised stream ID in bytes [9:13].
	pid := binary.BigEndian.Uint32(buf[9:13]) & 0x7fffffff
	if pid != 2 {
		t.Fatalf("promised stream ID = %d, want 2", pid)
	}
	// Header block starts at byte 13.
	if !bytes.Equal(buf[13:n], hblock) {
		t.Fatalf("header block = %v, want %v", buf[13:n], hblock)
	}
}

func TestEncodeBufferTooSmall(t *testing.T) {
	t.Parallel()

	f := PushPromiseFrame{
		AssociatedStreamID: 1,
		PromisedStreamID:   2,
		HeaderBlock:        []byte{0x82},
	}
	_, err := f.Encode(make([]byte, 5))
	if err == nil {
		t.Fatal("want error for undersized buffer")
	}
}

func TestReservedBitMaskedOnEncode(t *testing.T) {
	t.Parallel()

	// Set the reserved high bit on both IDs; Encode must mask it to zero.
	f := PushPromiseFrame{
		AssociatedStreamID: 0x80000001,
		PromisedStreamID:   0x80000002,
		HeaderBlock:        []byte{0x82},
	}
	buf := make([]byte, 64)
	if _, err := f.Encode(buf); err != nil {
		t.Fatal(err)
	}
	if buf[5]&0x80 != 0 {
		t.Fatalf("reserved bit set in associated stream field: 0x%02x", buf[5])
	}
	if buf[9]&0x80 != 0 {
		t.Fatalf("reserved bit set in promised stream field: 0x%02x", buf[9])
	}
}
```

## Review

The encoder is correct when the byte at offset 3 is `0x05`, the `END_HEADERS` bit is set in the flags, the length field equals `4 + len(HeaderBlock)`, and the two stream-ID fields round-trip through `binary.BigEndian.Uint32` masked with `0x7fffffff`. The mistakes worth guarding against are off-by-one offsets (the promised ID lives at bytes 9–12, after the 9-byte header, not inside it), writing into a buffer that is too small (always check `len(p)` first and return an error), and forgetting to mask the reserved bit so a caller's stray high bit leaks onto the wire. Running the suite under `-race` is cheap here because the encoder holds no state, but it keeps the module consistent with the rest of the lesson.

## Resources

- [RFC 9113 §6.6 — PUSH_PROMISE](https://httpwg.org/specs/rfc9113.html#PUSH_PROMISE) — the authoritative frame format, including the promised-stream-ID field and the END_HEADERS flag.
- [RFC 9113 §4.1 — Frame Format](https://httpwg.org/specs/rfc9113.html#FrameHeader) — the 9-byte frame header and the reserved-bit rule for stream-ID fields.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `BigEndian.PutUint32`, the helper behind the stream-ID fields.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-push-policy.md](02-push-policy.md)
