# 20. Custom Wire Protocol

Designing a binary wire protocol forces you to confront problems that high-level abstractions hide: TCP delivers a byte stream without message boundaries, concurrent in-flight requests must be correlated, and receivers must detect corruption without trusting the sender. This lesson builds a compact binary framing protocol—magic bytes, version field, CRC32 integrity check, length-prefixed payload—and implements an encoder and decoder verified by a full test suite.

## Concepts

### TCP Is a Byte Stream, Not a Message Stream

TCP guarantees ordered, reliable delivery of bytes—not application-level messages. A single `Write` call on one end may arrive as multiple `Read` calls on the other, or several writes may coalesce into one read. The canonical solution is length-prefixed framing: every message carries its own byte count so the receiver reads exactly that many bytes before interpreting content.

### Frame Layout

A frame is a self-contained unit: it carries everything needed to parse it without external context.

```
offset  len  field
     0    2  magic (0xCA 0xFE) — protocol identifier
     2    1  version
     3    1  message type
     4    4  request ID (big-endian uint32)
     8    4  payload length (big-endian uint32)
    12    N  payload
  12+N    4  CRC32 checksum (big-endian uint32, over bytes 0..12+N-1)
```

The header is always 12 bytes; the minimum frame is 16 bytes (no payload). Magic bytes identify the protocol immediately so a client accidentally connecting to an HTTP port is rejected before any payload is read. The version field allows the framing layer to evolve: a receiver rejects frames with an unknown version rather than silently misinterpreting them.

Big-endian byte order (`encoding/binary.BigEndian`) is the standard for network protocols: the most significant byte travels first, matching network byte order (RFC 1700).

### CRC32 Integrity Checking

CRC32 detects single-bit corruption and short burst errors in transit. The checksum covers all frame bytes except the checksum field itself (bytes 0 through 12+N-1). A mismatch is a hard error: the frame is discarded and an error is returned to the caller.

CRC32 is not a cryptographic hash—it does not detect adversarial tampering. For integrity only (not authentication) it is the standard choice: fast, hardware-accelerated on modern CPUs, and universally supported.

### Partial Reads and io.ReadFull

A single `r.Read(buf)` call is not guaranteed to fill `buf`—the OS may return fewer bytes even when more are available. `io.ReadFull(r, buf)` reads exactly `len(buf)` bytes, blocking until the buffer is full or an error occurs. Every multi-byte read in the decoder must use `io.ReadFull`.

### Sentinel Errors and errors.Is

Callers distinguish error kinds with `errors.Is`, not string matching. Each protocol error is a package-level sentinel. Wrapping additional context with `%w` preserves the error chain: `errors.Is(err, ErrBadMagic)` still returns true when a message like "proto: bad magic bytes: got 0xFF" is attached.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/wirep
cd ~/go-exercises/wirep
go mod init example.com/wirep
```

This is a library, not a program. Verify it with `go test`, not `go run`.

### Exercise 1: Protocol Types and Encoder/Decoder

Create `proto/proto.go`:

```go
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// MessageType identifies the purpose of a frame.
type MessageType uint8

const (
	MsgGet  MessageType = 1
	MsgSet  MessageType = 2
	MsgDel  MessageType = 3
	MsgResp MessageType = 4
)

// Version1 is the only protocol version this package implements.
const Version1 = uint8(1)

// MaxPayloadSize is the largest payload the decoder will accept (16 MiB).
const MaxPayloadSize = uint32(16 << 20)

// Sentinel errors returned by Decode. Use errors.Is to check them.
var (
	ErrBadMagic        = errors.New("proto: bad magic bytes")
	ErrBadVersion      = errors.New("proto: unsupported version")
	ErrBadChecksum     = errors.New("proto: checksum mismatch")
	ErrPayloadTooLarge = errors.New("proto: payload exceeds maximum size")
)

// magic is the two-byte protocol identifier that opens every frame.
var magic = [2]byte{0xCA, 0xFE}

const (
	headerSize   = 12 // magic(2) + version(1) + type(1) + reqID(4) + payloadLen(4)
	checksumSize = 4
)

// Message is the decoded representation of a single wire frame.
type Message struct {
	Version   uint8
	Type      MessageType
	RequestID uint32
	Payload   []byte
}

// Encode writes msg to w as a single framed message.
//
// Frame layout: header (12 bytes) | payload (N bytes) | CRC32 (4 bytes).
// The CRC covers bytes 0..12+N-1 (everything before the checksum).
func Encode(w io.Writer, msg Message) error {
	if uint32(len(msg.Payload)) > MaxPayloadSize {
		return fmt.Errorf("%w: size %d", ErrPayloadTooLarge, len(msg.Payload))
	}
	frame := make([]byte, headerSize+len(msg.Payload)+checksumSize)
	copy(frame[0:2], magic[:])
	frame[2] = msg.Version
	frame[3] = byte(msg.Type)
	binary.BigEndian.PutUint32(frame[4:8], msg.RequestID)
	binary.BigEndian.PutUint32(frame[8:12], uint32(len(msg.Payload)))
	copy(frame[headerSize:], msg.Payload)
	checksum := crc32.ChecksumIEEE(frame[:headerSize+len(msg.Payload)])
	binary.BigEndian.PutUint32(frame[headerSize+len(msg.Payload):], checksum)
	_, err := w.Write(frame)
	return err
}

// Decode reads one framed message from r.
//
// It validates magic bytes, protocol version, payload size limit, and CRC32
// checksum before returning the message. Partial frames and corrupted frames
// are rejected with sentinel errors.
func Decode(r io.Reader) (Message, error) {
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return Message{}, fmt.Errorf("proto: read header: %w", err)
	}
	if hdr[0] != magic[0] || hdr[1] != magic[1] {
		return Message{}, ErrBadMagic
	}
	version := hdr[2]
	if version != Version1 {
		return Message{}, fmt.Errorf("%w: got %d", ErrBadVersion, version)
	}
	msgType := MessageType(hdr[3])
	reqID := binary.BigEndian.Uint32(hdr[4:8])
	payloadLen := binary.BigEndian.Uint32(hdr[8:12])
	if payloadLen > MaxPayloadSize {
		return Message{}, fmt.Errorf("%w: size %d", ErrPayloadTooLarge, payloadLen)
	}
	payload := make([]byte, payloadLen)
	if payloadLen > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return Message{}, fmt.Errorf("proto: read payload: %w", err)
		}
	}
	var crcBuf [checksumSize]byte
	if _, err := io.ReadFull(r, crcBuf[:]); err != nil {
		return Message{}, fmt.Errorf("proto: read checksum: %w", err)
	}
	stored := binary.BigEndian.Uint32(crcBuf[:])
	h := crc32.NewIEEE()
	h.Write(hdr)
	h.Write(payload)
	if h.Sum32() != stored {
		return Message{}, ErrBadChecksum
	}
	return Message{
		Version:   version,
		Type:      msgType,
		RequestID: reqID,
		Payload:   payload,
	}, nil
}
```

`Encode` builds the entire frame in a single allocation and writes it atomically to avoid partial writes. `Decode` uses `io.ReadFull` for every fixed-size read and rejects frames before allocating payload memory when the length field exceeds `MaxPayloadSize`.

### Exercise 2: Test the Codec

Create `proto/proto_test.go`:

```go
package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		msg  Message
	}{
		{"get", Message{Version: Version1, Type: MsgGet, RequestID: 1, Payload: []byte("mykey")}},
		{"set", Message{Version: Version1, Type: MsgSet, RequestID: 2, Payload: []byte("mykey\x00myval")}},
		{"del", Message{Version: Version1, Type: MsgDel, RequestID: 3, Payload: []byte("mykey")}},
		{"resp", Message{Version: Version1, Type: MsgResp, RequestID: 4, Payload: []byte("OK")}},
		{"empty payload", Message{Version: Version1, Type: MsgGet, RequestID: 5, Payload: nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := Encode(&buf, tc.msg); err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(&buf)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Version != tc.msg.Version || got.Type != tc.msg.Type || got.RequestID != tc.msg.RequestID {
				t.Errorf("header mismatch: got %+v, want %+v", got, tc.msg)
			}
			if !bytes.Equal(got.Payload, tc.msg.Payload) {
				t.Errorf("payload mismatch: got %q, want %q", got.Payload, tc.msg.Payload)
			}
		})
	}
}

func TestDecodeRejectsBadMagic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Encode(&buf, Message{Version: Version1, Type: MsgGet, RequestID: 1}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	data[0] ^= 0xFF // corrupt first magic byte
	_, err := Decode(bytes.NewReader(data))
	if !errors.Is(err, ErrBadMagic) {
		t.Errorf("err = %v, want ErrBadMagic", err)
	}
}

func TestDecodeRejectsChecksumMismatch(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Encode(&buf, Message{Version: Version1, Type: MsgSet, RequestID: 9, Payload: []byte("data")}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	data[12] ^= 0x01 // flip a bit in the payload (byte 12 is start of payload)
	_, err := Decode(bytes.NewReader(data))
	if !errors.Is(err, ErrBadChecksum) {
		t.Errorf("err = %v, want ErrBadChecksum", err)
	}
}

func TestDecodeRejectsBadVersion(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Encode(&buf, Message{Version: Version1, Type: MsgGet, RequestID: 1}); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	data[2] = 99 // unsupported version at byte offset 2
	_, err := Decode(bytes.NewReader(data))
	if !errors.Is(err, ErrBadVersion) {
		t.Errorf("err = %v, want ErrBadVersion", err)
	}
}

func TestDecodeRejectsOversizedPayload(t *testing.T) {
	t.Parallel()
	// Craft a header claiming a payload larger than MaxPayloadSize.
	// Decode must reject it before attempting to allocate the payload.
	hdr := make([]byte, headerSize)
	copy(hdr[0:2], magic[:])
	hdr[2] = Version1
	hdr[3] = byte(MsgGet)
	binary.BigEndian.PutUint32(hdr[4:8], 1)
	binary.BigEndian.PutUint32(hdr[8:12], MaxPayloadSize+1)
	_, err := Decode(bytes.NewReader(hdr))
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("err = %v, want ErrPayloadTooLarge", err)
	}
}

func TestMultipleMessagesOnStream(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		{Version: Version1, Type: MsgGet, RequestID: 10, Payload: []byte("k1")},
		{Version: Version1, Type: MsgSet, RequestID: 11, Payload: []byte("k1=v1")},
		{Version: Version1, Type: MsgResp, RequestID: 10, Payload: []byte("v1")},
	}
	var buf bytes.Buffer
	for _, m := range msgs {
		if err := Encode(&buf, m); err != nil {
			t.Fatalf("Encode: %v", err)
		}
	}
	for i, want := range msgs {
		got, err := Decode(&buf)
		if err != nil {
			t.Fatalf("Decode message %d: %v", i, err)
		}
		if got.RequestID != want.RequestID || got.Type != want.Type {
			t.Errorf("message %d: got %+v, want %+v", i, got, want)
		}
		if !bytes.Equal(got.Payload, want.Payload) {
			t.Errorf("message %d payload: got %q, want %q", i, got.Payload, want.Payload)
		}
	}
}

func ExampleEncode() {
	var buf bytes.Buffer
	if err := Encode(&buf, Message{
		Version:   Version1,
		Type:      MsgSet,
		RequestID: 7,
		Payload:   []byte("k=v"),
	}); err != nil {
		panic(err)
	}
	m, err := Decode(&buf)
	if err != nil {
		panic(err)
	}
	fmt.Printf("type=%d id=%d body=%s\n", m.Type, m.RequestID, m.Payload)
	// Output: type=2 id=7 body=k=v
}
```

Your turn: add `TestLargePayload` that encodes a message with a 64 KiB payload of repeating bytes (`bytes.Repeat([]byte{0xAB}, 64<<10)`), decodes it, and asserts that `bytes.Equal` holds.

### Exercise 3: Demo Program

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"

	"example.com/wirep/proto"
)

func main() {
	msgs := []proto.Message{
		{Version: proto.Version1, Type: proto.MsgGet, RequestID: 1, Payload: []byte("username")},
		{Version: proto.Version1, Type: proto.MsgSet, RequestID: 2, Payload: []byte("username\x00alice")},
		{Version: proto.Version1, Type: proto.MsgResp, RequestID: 1, Payload: []byte("alice")},
	}

	var buf bytes.Buffer
	for _, m := range msgs {
		if err := proto.Encode(&buf, m); err != nil {
			log.Fatalf("encode: %v", err)
		}
	}
	fmt.Printf("encoded %d bytes total\n", buf.Len())

	for i := range msgs {
		m, err := proto.Decode(&buf)
		if err != nil {
			log.Fatalf("decode message %d: %v", i, err)
		}
		fmt.Printf("msg %d: type=%d reqID=%d payload=%q\n", i+1, m.Type, m.RequestID, m.Payload)
	}
}
```

Run it with:

```bash
go run ./cmd/demo
```

## Common Mistakes

### Assuming a Single Read Fills the Buffer

Wrong: `n, err := r.Read(hdr)` followed by treating `hdr` as complete regardless of `n`.

What happens: `Read` may return fewer bytes than `len(hdr)` without error. The remaining bytes in `hdr` are stale zeros; the frame is silently misparsed.

Fix: `io.ReadFull(r, hdr)` loops internally until the buffer is full or an error occurs.

### Computing CRC Over Only the Payload

Wrong: `crc32.ChecksumIEEE(payload)` stored in the frame; header bytes are not covered.

What happens: a corrupted version field or request ID passes CRC validation undetected because the checksum does not cover those bytes.

Fix: compute the CRC over the entire frame content before the checksum (header + payload), exactly as `Encode` does: `crc32.ChecksumIEEE(frame[:headerSize+len(payload)])`.

### Checking the Wrong Error with String Matching

Wrong: `if err != nil && strings.Contains(err.Error(), "bad magic")`.

What happens: string matching breaks if the message changes across versions; it cannot distinguish wrapped errors.

Fix: `errors.Is(err, proto.ErrBadMagic)`. The sentinel is part of the package API and is stable across wrapping.

### Calling buf.Bytes() Then Writing More to buf

Wrong: `data := buf.Bytes()` after encoding, then appending another frame to the same `buf`. `data` shares the buffer's backing array; the next write may reallocate and invalidate `data`.

Fix: copy immediately (`snapshot := append([]byte(nil), buf.Bytes()...)`) or use `buf.Next(n)` to consume bytes in order without aliasing.

## Verification

From `~/go-exercises/wirep`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. There is no program to eyeball; the tests are the verification.

## Summary

- TCP is a byte stream: length-prefixed framing gives messages a boundary.
- A frame header carries magic bytes (protocol identification), version (forward compatibility), message type, request ID, and payload length.
- CRC32 computed over the full frame content before the checksum field detects in-transit corruption.
- `io.ReadFull` is mandatory for reading fixed-size fields; a bare `Read` is not guaranteed to fill the buffer.
- Sentinel errors wrapped with `%w` let callers use `errors.Is` without string matching.
- Version negotiation and request ID correlation are natural extensions once framing and integrity checking are in place.

## What's Next

Next: [TCP Load Balancer](../21-tcp-load-balancer/21-tcp-load-balancer.md).

## Resources

- [encoding/binary](https://pkg.go.dev/encoding/binary) — big-endian integer encoding; `PutUint32` and `Uint32` used in this lesson
- [hash/crc32](https://pkg.go.dev/hash/crc32) — CRC32 implementation; `ChecksumIEEE` and `NewIEEE` used in this lesson
- [io.ReadFull](https://pkg.go.dev/io#ReadFull) — guaranteed full-buffer reads from any `io.Reader`
- [Protocol Buffers wire format](https://protobuf.dev/programming-guides/encoding/) — reference binary framing design using varint length prefixes
- [Redis RESP3 specification](https://redis.io/docs/latest/develop/reference/protocol-spec/) — production text/binary wire protocol with versioning
