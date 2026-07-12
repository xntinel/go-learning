# Exercise 1: Binary Frame Codec

Everything a network message queue does travels as one shape on the wire: a length-prefixed frame whose fixed header carries a correlation ID and whose variable payload is itself a structured big-endian record. This exercise builds that wire codec — `Frame` with `Encode`/`Decode`, plus the `BinaryWriter`/`BinaryReader` pair that serializes payload fields — the self-delimiting, endianness-centralizing unit that every later exercise reads and writes.

This module is fully self-contained: it starts with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
api.go               APIKey, APIVersion, ErrorCode, sentinel errors, ErrorFromCode
frame.go             Frame, Encode (length-prefixed), Decode (io.ReadFull framing)
codec.go             BinaryWriter / BinaryReader (big-endian, error-accumulating)
cmd/
  demo/
    main.go          build a payload, frame it, decode it, read the payload back
mqframe_test.go      frame round-trips, malformed-length and truncated-body rejection,
                     codec round-trip across a net.Pipe, ErrorFromCode mapping
```

- Files: `api.go`, `frame.go`, `codec.go`, `cmd/demo/main.go`, `mqframe_test.go`.
- Implement: `Frame` with `Encode(io.Writer) error` and the package function `Decode(io.Reader) (*Frame, error)`; `BinaryWriter` and `BinaryReader`; `ErrorFromCode`.
- Test: round-trip a table of frames, reject a sub-header length with `ErrBadFrame`, reject a truncated body, round-trip the codec, and map error codes to sentinels.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/07-tcp-protocol-client/01-binary-frame-codec/cmd/demo && cd go-solutions/41-capstone-message-queue/07-tcp-protocol-client/01-binary-frame-codec
```

### Why a length prefix and a correlation ID, and why this exact layout

TCP is a byte stream, not a message stream: a single `Write` of one frame may arrive at the peer as several `Read` calls, and several frames may arrive coalesced into one. The receiver therefore cannot find message boundaries by reading "until the data stops" — it must be told, in band, how long each message is. A 4-byte big-endian length prefix does exactly that: the receiver issues one `io.ReadFull` for the 4 length bytes, learns the body size, then issues one more `io.ReadFull` for that many bytes. `io.ReadFull` is the right primitive precisely because it loops internally until it has filled the buffer or hit an error, absorbing the short reads that a stream protocol produces.

The header that follows the length carries a correlation ID. It is the field that lets one connection carry many concurrent requests: the client stamps each request with a unique ID and the server echoes it on the response, so a reader can match a reply to its waiting caller even when replies come back out of order. The full layout, following Kafka's convention that the length field counts the bytes after itself:

```text
offset  size  field
0       4     length (= headerLen + len(Payload), big-endian uint32)
4       2     API key (uint16)
6       2     API version (uint16)
8       4     correlation ID (int32)
12      N     payload
```

`headerLen` is the 8 fixed bytes after the length field (API key, version, correlation ID). `Decode` reads the length first and rejects anything below `headerLen` with the `ErrBadFrame` sentinel before it ever sizes a body slice, so a corrupt or hostile length cannot drive a huge allocation or an out-of-range slice. That guard is the difference between a reader that fails cleanly on bad input and one that panics or exhausts memory.

Create `api.go`:

```go
package mqframe

import (
	"errors"
	"fmt"
)

// APIKey identifies the protocol operation carried in every Frame header.
type APIKey uint16

const (
	APIProduce      APIKey = 0
	APIFetch        APIKey = 1
	APICommitOffset APIKey = 2
	APIFetchOffset  APIKey = 3
	APICreateTopic  APIKey = 4
	APIDeleteTopic  APIKey = 5
	APIListTopics   APIKey = 6
	APIMetadata     APIKey = 7
)

// APIVersion lets the server evolve a request format without breaking clients
// pinned to an older version.
type APIVersion uint16

// V0 is the initial protocol version.
const V0 APIVersion = 0

// ErrorCode is a 2-byte numeric error indicator returned in a response payload.
// A fixed-size code costs 2 bytes versus a variable-length string.
type ErrorCode int16

const (
	ErrCodeNone             ErrorCode = 0
	ErrCodeUnknownTopic     ErrorCode = 1
	ErrCodeInvalidPartition ErrorCode = 2
	ErrCodeOffsetOutOfRange ErrorCode = 3
	ErrCodeGroupNotFound    ErrorCode = 4
)

// Sentinel errors returned by codec and client methods. Callers compare with
// errors.Is.
var (
	ErrUnknownTopic     = errors.New("unknown topic")
	ErrInvalidPartition = errors.New("invalid partition")
	ErrOffsetOutOfRange = errors.New("offset out of range")
	ErrGroupNotFound    = errors.New("group not found")
	ErrBadFrame         = errors.New("malformed frame")
	ErrConnClosed       = errors.New("connection closed")
)

// ErrorFromCode maps a wire ErrorCode to a Go error for use with errors.Is.
func ErrorFromCode(c ErrorCode) error {
	switch c {
	case ErrCodeNone:
		return nil
	case ErrCodeUnknownTopic:
		return ErrUnknownTopic
	case ErrCodeInvalidPartition:
		return ErrInvalidPartition
	case ErrCodeOffsetOutOfRange:
		return ErrOffsetOutOfRange
	case ErrCodeGroupNotFound:
		return ErrGroupNotFound
	default:
		return fmt.Errorf("mqframe: unknown error code %d", c)
	}
}
```

Create `frame.go`:

```go
package mqframe

import (
	"encoding/binary"
	"fmt"
	"io"
)

// headerLen is the fixed part of a frame body, everything after the 4-byte
// length field: [2] API key, [2] API version, [4] correlation ID.
const headerLen = 8

// Frame is one protocol message, request or response.
//
// Wire format (Kafka length convention: the length field counts the bytes
// after itself, not including the 4-byte field):
//
//	[4]  length  = headerLen + len(Payload)
//	[2]  API key
//	[2]  API version
//	[4]  correlation ID
//	[N]  payload
type Frame struct {
	APIKey        APIKey
	APIVersion    APIVersion
	CorrelationID int32
	Payload       []byte
}

// Encode writes f to w as a complete, length-prefixed frame.
func (f *Frame) Encode(w io.Writer) error {
	length := headerLen + len(f.Payload)
	hdr := make([]byte, 4+headerLen)
	binary.BigEndian.PutUint32(hdr[0:], uint32(length))
	binary.BigEndian.PutUint16(hdr[4:], uint16(f.APIKey))
	binary.BigEndian.PutUint16(hdr[6:], uint16(f.APIVersion))
	binary.BigEndian.PutUint32(hdr[8:], uint32(f.CorrelationID))
	if _, err := w.Write(hdr); err != nil {
		return fmt.Errorf("mqframe: write header: %w", err)
	}
	if len(f.Payload) > 0 {
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("mqframe: write payload: %w", err)
		}
	}
	return nil
}

// Decode reads exactly one frame from r. It reads the 4-byte length, validates
// it against headerLen, then reads the body with a single io.ReadFull.
func Decode(r io.Reader) (*Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, fmt.Errorf("mqframe: read length: %w", err)
	}
	length := int(binary.BigEndian.Uint32(lenBuf[:]))
	if length < headerLen {
		return nil, fmt.Errorf("%w: length=%d", ErrBadFrame, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("mqframe: read frame body: %w", err)
	}
	return &Frame{
		APIKey:        APIKey(binary.BigEndian.Uint16(body[0:])),
		APIVersion:    APIVersion(binary.BigEndian.Uint16(body[2:])),
		CorrelationID: int32(binary.BigEndian.Uint32(body[4:])),
		Payload:       body[headerLen:],
	}, nil
}
```

Create `codec.go`:

```go
package mqframe

import (
	"encoding/binary"
	"io"
)

// BinaryWriter accumulates a big-endian byte payload. Every integer is
// big-endian; the endianness decision stays here, not at each call site.
type BinaryWriter struct {
	buf []byte
}

func (w *BinaryWriter) WriteInt8(v int8) {
	w.buf = append(w.buf, byte(v))
}

func (w *BinaryWriter) WriteInt16(v int16) {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(v))
	w.buf = append(w.buf, b[:]...)
}

func (w *BinaryWriter) WriteInt32(v int32) {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	w.buf = append(w.buf, b[:]...)
}

func (w *BinaryWriter) WriteInt64(v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	w.buf = append(w.buf, b[:]...)
}

// WriteString writes a 2-byte signed length then the UTF-8 bytes of s. Use -1
// as the length to signal a null string; pass that via a negative length only
// from nullable-field encoders, not here.
func (w *BinaryWriter) WriteString(s string) {
	w.WriteInt16(int16(len(s)))
	w.buf = append(w.buf, s...)
}

// WriteBytes writes a 4-byte signed length then b.
func (w *BinaryWriter) WriteBytes(b []byte) {
	w.WriteInt32(int32(len(b)))
	w.buf = append(w.buf, b...)
}

// Bytes returns the accumulated payload.
func (w *BinaryWriter) Bytes() []byte { return w.buf }

// BinaryReader decodes a big-endian byte stream. The first error is stored and
// returned by Err; every subsequent read becomes a no-op. This lets a caller
// decode a sequence of fields and check Err once at the end.
type BinaryReader struct {
	r   io.Reader
	err error
}

// NewBinaryReader wraps r for sequential big-endian reads.
func NewBinaryReader(r io.Reader) *BinaryReader { return &BinaryReader{r: r} }

func (r *BinaryReader) ReadInt8() int8 {
	if r.err != nil {
		return 0
	}
	var b [1]byte
	if _, r.err = io.ReadFull(r.r, b[:]); r.err != nil {
		return 0
	}
	return int8(b[0])
}

func (r *BinaryReader) ReadInt16() int16 {
	if r.err != nil {
		return 0
	}
	var b [2]byte
	if _, r.err = io.ReadFull(r.r, b[:]); r.err != nil {
		return 0
	}
	return int16(binary.BigEndian.Uint16(b[:]))
}

func (r *BinaryReader) ReadInt32() int32 {
	if r.err != nil {
		return 0
	}
	var b [4]byte
	if _, r.err = io.ReadFull(r.r, b[:]); r.err != nil {
		return 0
	}
	return int32(binary.BigEndian.Uint32(b[:]))
}

func (r *BinaryReader) ReadInt64() int64 {
	if r.err != nil {
		return 0
	}
	var b [8]byte
	if _, r.err = io.ReadFull(r.r, b[:]); r.err != nil {
		return 0
	}
	return int64(binary.BigEndian.Uint64(b[:]))
}

// ReadString reads a 2-byte signed length then that many bytes as a string.
// A negative length (null) returns an empty string without error.
func (r *BinaryReader) ReadString() string {
	n := r.ReadInt16()
	if r.err != nil || n < 0 {
		return ""
	}
	b := make([]byte, n)
	if _, r.err = io.ReadFull(r.r, b); r.err != nil {
		return ""
	}
	return string(b)
}

// ReadBytes reads a 4-byte signed length then that many bytes.
// A negative length (null) returns nil without error.
func (r *BinaryReader) ReadBytes() []byte {
	n := r.ReadInt32()
	if r.err != nil || n < 0 {
		return nil
	}
	b := make([]byte, n)
	if _, r.err = io.ReadFull(r.r, b); r.err != nil {
		return nil
	}
	return b
}

// Err returns the first read error encountered, or nil.
func (r *BinaryReader) Err() error { return r.err }
```

Read `Encode` and `Decode` as mirror images, and `BinaryWriter`/`BinaryReader` likewise. `Encode` lays the length and 8-byte header into one slice, writes it, then writes the payload; `Decode` reads the length, bounds-checks it, then fills the body with a single `io.ReadFull` and slices the header fields out by offset. The codec works the same way on payload contents: `WriteString` prefixes a 2-byte length so `ReadString` knows how many bytes to take, and `WriteBytes` prefixes a 4-byte length for the same reason on larger blobs. Because `BinaryReader` accumulates the first error and turns later reads into no-ops, a payload decoder reads `topic := r.ReadString(); part := r.ReadInt32(); val := r.ReadBytes()` straight down the page and checks `r.Err()` once, with no per-field error plumbing.

### The runnable demo

The demo builds a produce-request payload with the `BinaryWriter`, wraps it in a `Frame`, encodes the frame to a buffer, decodes it back, and reads the payload fields out with a `BinaryReader` — the full encode/decode path the network code will run, but in process.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"

	"example.com/mqframe"
)

func main() {
	// Build a produce payload: topic (string), partition (int32), value (bytes).
	var pw mqframe.BinaryWriter
	pw.WriteString("orders")
	pw.WriteInt32(3)
	pw.WriteBytes([]byte("ping"))

	req := mqframe.Frame{
		APIKey:        mqframe.APIProduce,
		APIVersion:    mqframe.V0,
		CorrelationID: 7,
		Payload:       pw.Bytes(),
	}

	var buf bytes.Buffer
	if err := req.Encode(&buf); err != nil {
		panic(err)
	}
	fmt.Printf("frame encoded: %d bytes (header=%d, payload=%d)\n",
		buf.Len(), buf.Len()-len(req.Payload), len(req.Payload))

	got, err := mqframe.Decode(&buf)
	if err != nil {
		panic(err)
	}
	fmt.Printf("frame decoded: apiKey=%d version=%d correlationID=%d\n",
		got.APIKey, got.APIVersion, got.CorrelationID)

	r := mqframe.NewBinaryReader(bytes.NewReader(got.Payload))
	topic := r.ReadString()
	partition := r.ReadInt32()
	value := r.ReadBytes()
	if err := r.Err(); err != nil {
		panic(err)
	}
	fmt.Printf("produce payload: topic=%s partition=%d value=%q\n", topic, partition, value)

	fmt.Printf("errorFromCode(1) = %v\n", mqframe.ErrorFromCode(mqframe.ErrCodeUnknownTopic))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
frame encoded: 32 bytes (header=12, payload=20)
frame decoded: apiKey=0 version=0 correlationID=7
produce payload: topic=orders partition=3 value="ping"
errorFromCode(1) = unknown topic
```

### Tests

The tests pin the codec's contract. `TestFrameRoundTrip` encodes a table of frames and decodes each, checking every field. `TestDecodeRejectsShortLength` feeds a length below `headerLen` and asserts `ErrBadFrame`. `TestDecodeRejectsTruncatedBody` declares a long length but supplies a short body and asserts `io.ReadFull` fails. `TestCodecOverPipe` writes a frame into one end of a `net.Pipe` and decodes it from the other, proving the streaming path handles reads split across the connection. `TestBinaryRoundTrip` and `TestErrorFromCode` cover the payload codec and the error mapping.

Create `mqframe_test.go`:

```go
package mqframe

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   Frame
	}{
		{"empty payload", Frame{APIKey: APIProduce, APIVersion: V0, CorrelationID: 1}},
		{"with payload", Frame{APIKey: APIFetch, APIVersion: V0, CorrelationID: 999, Payload: []byte("data")}},
		{"large id", Frame{APIKey: APIMetadata, APIVersion: V0, CorrelationID: 1<<30 - 1, Payload: []byte{0, 1, 2}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := tc.in.Encode(&buf); err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(&buf)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.APIKey != tc.in.APIKey || got.APIVersion != tc.in.APIVersion ||
				got.CorrelationID != tc.in.CorrelationID {
				t.Errorf("header: got %+v, want %+v", got, tc.in)
			}
			if !bytes.Equal(got.Payload, tc.in.Payload) {
				t.Errorf("payload: got %v, want %v", got.Payload, tc.in.Payload)
			}
		})
	}
}

func TestDecodeRejectsShortLength(t *testing.T) {
	t.Parallel()
	// length=3 is below headerLen=8; Decode must return ErrBadFrame.
	buf := bytes.NewBuffer([]byte{0, 0, 0, 3, 0, 0, 0, 0, 0, 0, 0, 0})
	if _, err := Decode(buf); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("err = %v, want ErrBadFrame", err)
	}
}

func TestDecodeRejectsTruncatedBody(t *testing.T) {
	t.Parallel()
	// length=20 but only 5 bytes of body; io.ReadFull must fail.
	buf := bytes.NewBuffer([]byte{0, 0, 0, 20, 1, 2, 3, 4, 5})
	_, err := Decode(buf)
	if err == nil {
		t.Fatal("Decode should fail on truncated body")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestCodecOverPipe(t *testing.T) {
	t.Parallel()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	want := Frame{APIKey: APIProduce, CorrelationID: 5, Payload: []byte("over the wire")}
	go func() {
		_ = want.Encode(c1)
	}()

	got, err := Decode(c2)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.CorrelationID != want.CorrelationID || !bytes.Equal(got.Payload, want.Payload) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBinaryRoundTrip(t *testing.T) {
	t.Parallel()

	var w BinaryWriter
	w.WriteInt8(42)
	w.WriteInt16(1000)
	w.WriteInt32(-99999)
	w.WriteInt64(1<<40 + 7)
	w.WriteString("hello")
	w.WriteBytes([]byte{1, 2, 3})

	r := NewBinaryReader(bytes.NewReader(w.Bytes()))
	if got := r.ReadInt8(); got != 42 {
		t.Errorf("Int8 = %d, want 42", got)
	}
	if got := r.ReadInt16(); got != 1000 {
		t.Errorf("Int16 = %d, want 1000", got)
	}
	if got := r.ReadInt32(); got != -99999 {
		t.Errorf("Int32 = %d, want -99999", got)
	}
	if got := r.ReadInt64(); got != 1<<40+7 {
		t.Errorf("Int64 = %d, want %d", got, 1<<40+7)
	}
	if got := r.ReadString(); got != "hello" {
		t.Errorf("String = %q, want hello", got)
	}
	if got := r.ReadBytes(); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("Bytes = %v, want [1 2 3]", got)
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err() = %v", err)
	}
}

func TestErrorFromCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code ErrorCode
		want error
	}{
		{ErrCodeNone, nil},
		{ErrCodeUnknownTopic, ErrUnknownTopic},
		{ErrCodeInvalidPartition, ErrInvalidPartition},
		{ErrCodeOffsetOutOfRange, ErrOffsetOutOfRange},
		{ErrCodeGroupNotFound, ErrGroupNotFound},
	}
	for _, tc := range cases {
		if got := ErrorFromCode(tc.code); !errors.Is(got, tc.want) {
			t.Errorf("ErrorFromCode(%d) = %v, want %v", tc.code, got, tc.want)
		}
	}
}
```

## Review

The codec is sound when `Encode` and `Decode` are exact mirrors and `Decode` validates before it trusts. Confirm that `Decode` reads the 4-byte length, rejects any value below `headerLen` with `ErrBadFrame` before sizing a slice, and fills the body with a single `io.ReadFull` so a body split across several TCP segments still decodes — the `net.Pipe` test exercises exactly that split. On the payload side, every `WriteString`/`WriteBytes` length prefix is matched by the corresponding read, and `BinaryReader.Err()` carries the first failure so a multi-field decoder needs only one error check.

Common mistakes for this feature. The first is sizing a body slice from an unvalidated length: a corrupt frame can claim a gigabyte, so the `length < headerLen` guard (and, in production, an upper bound) must run before `make([]byte, length)`. The second is reaching for `Read` instead of `io.ReadFull`: a bare `Read` on a stream may return fewer bytes than asked and a hand-rolled loop to top it up is exactly the bug `io.ReadFull` exists to remove. The third is checksumming or trusting payload fields without the length prefixes that bound them, which turns a short read into an out-of-range slice rather than a clean error.

## Resources

- [`io.ReadFull`](https://pkg.go.dev/io#ReadFull) — the primitive for reading exactly N bytes from a stream that may return short reads, and the `ErrUnexpectedEOF` it returns on a truncated body.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `BigEndian.PutUint16/32` and `Uint16/32`, the exact fixed-width helpers behind the frame header and the codec.
- [Kafka Protocol Guide](https://kafka.apache.org/protocol.html) — the length-prefix convention, correlation ID field, and API-key numbering this frame format follows.

---

Next: [02-multiplexed-client-server.md](02-multiplexed-client-server.md)
