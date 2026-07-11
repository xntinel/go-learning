# Exercise 4: Frame-Size Enforcement and Error Scope

A frame's declared length must be checked before its payload is read, and a violation is not always fatal: RFC 9113 §4.2 says a size error scope depends on the frame. This module enforces a mutable `SETTINGS_MAX_FRAME_SIZE`, rejects frames too small to hold their mandatory fields, and classifies each violation as a connection error or a recoverable stream error.

This module is fully self-contained: it begins with its own `go mod init`, defines its own frame machinery, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
h2framesize/
  go.mod
  framesize.go          FrameType, SizeError, Guard.Check/SetMaxFrameSize, Reader.ReadFrame
  framesize_test.go     in-bounds accept; oversized/undersized; connection vs stream scope
  cmd/demo/main.go      read three frames; recover from a stream error, GOAWAY on a connection one
```

- Files: `framesize.go`, `framesize_test.go`, `cmd/demo/main.go`.
- Implement: `Guard` with `Check` and `SetMaxFrameSize`, and `Reader.ReadFrame` that enforces the guard and resynchronizes after a stream error.
- Test: an in-bounds frame is accepted; oversized HEADERS is a connection error; oversized DATA and undersized RST_STREAM are recoverable stream errors; a PING on stream 0 is always a connection error; the max is mutable within range.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p h2framesize/cmd/demo && cd h2framesize
go mod init example.com/h2framesize
go mod edit -go=1.26
```

## Two checks, one classification, and resynchronization

Every frame faces two size checks. The first is the maximum: a declared length above the current `SETTINGS_MAX_FRAME_SIZE` is a FRAME_SIZE_ERROR, and it is checked *before* the payload is read so a hostile 9-octet header cannot force a multi-megabyte allocation. The second is the minimum: several frame types have a fixed or floor length — PRIORITY is exactly 5, RST_STREAM and WINDOW_UPDATE exactly 4, PING exactly 8, GOAWAY at least 8 — and a frame too short to hold its mandatory fields is also a FRAME_SIZE_ERROR. A `rules` table encodes those per-type constraints so `Check` stays a single function.

The classification is the part most implementations get wrong. RFC 9113 §4.2 says a size error in a frame that could alter the state of the *entire connection* must be a connection error, and it lists exactly which: any frame carrying a field block (HEADERS, PUSH_PROMISE, CONTINUATION), a SETTINGS frame, and *any* frame on stream 0. Everything else — an oversized DATA frame, a wrong-length RST_STREAM on a live stream — is a stream error. `altersConnection` is that rule verbatim, and `SizeError.Connection` carries the verdict so a caller knows whether to send GOAWAY and close or send RST_STREAM and continue.

Continuing is what makes resynchronization necessary. After a stream-scoped size error the connection is still alive, so `ReadFrame` must discard the offending frame's payload with `io.CopyN(io.Discard, ...)` to stay byte-aligned, then the next `ReadFrame` reads cleanly. For a connection-scoped error there is no point discarding — the caller will GOAWAY and tear the connection down — so `ReadFrame` returns immediately without consuming the (possibly enormous) payload. The maximum is mutable: a peer's SETTINGS_MAX_FRAME_SIZE raises the limit through `SetMaxFrameSize`, which itself rejects any value outside the [16384, 16777215] range the parameter is allowed to take.

Create `framesize.go`:

```go
// Package h2framesize enforces the HTTP/2 frame-size rules of RFC 9113 §4.2.
// It rejects frames larger than the negotiated SETTINGS_MAX_FRAME_SIZE and
// frames too small to hold their mandatory fields, and it classifies each
// violation as a connection error or a stream error per RFC 9113 §5.4.
package h2framesize

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FrameType is the 8-bit type field of an HTTP/2 frame.
type FrameType uint8

const (
	FrameData         FrameType = 0x0
	FrameHeaders      FrameType = 0x1
	FramePriority     FrameType = 0x2
	FrameRSTStream    FrameType = 0x3
	FrameSettings     FrameType = 0x4
	FramePushPromise  FrameType = 0x5
	FramePing         FrameType = 0x6
	FrameGoaway       FrameType = 0x7
	FrameWindowUpdate FrameType = 0x8
	FrameContinuation FrameType = 0x9
)

// ErrCodeFrameSize is the HTTP/2 FRAME_SIZE_ERROR code (RFC 9113 §7).
const ErrCodeFrameSize uint32 = 0x6

// Frame-size bounds from RFC 9113 §6.5.2.
const (
	MinMaxFrameSize uint32 = 1 << 14   // 16384: the floor and protocol default
	MaxMaxFrameSize uint32 = 1<<24 - 1 // 16777215: the absolute ceiling
)

// ErrFrameSize is the sentinel every frame-size violation wraps so callers can
// match it with errors.Is.
var ErrFrameSize = errors.New("h2framesize: FRAME_SIZE_ERROR")

// ErrBadMaxFrameSize is returned by SetMaxFrameSize for an out-of-range value.
var ErrBadMaxFrameSize = errors.New("h2framesize: SETTINGS_MAX_FRAME_SIZE out of range")

// SizeError describes one frame-size violation. Connection is true when the
// offending frame could alter connection state, which RFC 9113 §4.2 requires be
// treated as a connection error (GOAWAY); otherwise it is a stream error
// (RST_STREAM) and the reader can keep going on other streams.
type SizeError struct {
	Type       FrameType
	StreamID   uint32
	Length     uint32
	Connection bool
	reason     string
}

func (e *SizeError) Error() string {
	scope := "stream"
	if e.Connection {
		scope = "connection"
	}
	return fmt.Sprintf("h2framesize: FRAME_SIZE_ERROR (%s): type=%d stream=%d len=%d: %s",
		scope, e.Type, e.StreamID, e.Length, e.reason)
}

func (e *SizeError) Unwrap() error { return ErrFrameSize }

// sizeRule captures the fixed-or-minimum payload length a frame type requires.
// fixed != 0 means the length must equal fixed exactly; otherwise min is a floor.
type sizeRule struct {
	fixed uint32
	min   uint32
}

var rules = map[FrameType]sizeRule{
	FramePriority:     {fixed: 5},
	FrameRSTStream:    {fixed: 4},
	FramePing:         {fixed: 8},
	FrameWindowUpdate: {fixed: 4},
	FrameGoaway:       {min: 8},
}

// altersConnection reports whether a size error on this frame must be a
// connection error: any frame carrying a field block (HEADERS, PUSH_PROMISE,
// CONTINUATION), a SETTINGS frame, or any frame on stream 0 (RFC 9113 §4.2).
func altersConnection(t FrameType, streamID uint32) bool {
	if streamID == 0 {
		return true
	}
	switch t {
	case FrameHeaders, FramePushPromise, FrameContinuation, FrameSettings:
		return true
	default:
		return false
	}
}

// Guard validates frame sizes against a mutable maximum.
type Guard struct {
	max uint32
}

// NewGuard returns a Guard initialized to the protocol-default maximum (16384).
func NewGuard() *Guard { return &Guard{max: MinMaxFrameSize} }

// MaxFrameSize reports the current limit.
func (g *Guard) MaxFrameSize() uint32 { return g.max }

// SetMaxFrameSize raises (or lowers) the limit after a peer advertises a new
// SETTINGS_MAX_FRAME_SIZE. Values outside [16384, 16777215] are rejected.
func (g *Guard) SetMaxFrameSize(v uint32) error {
	if v < MinMaxFrameSize || v > MaxMaxFrameSize {
		return fmt.Errorf("%w: %d", ErrBadMaxFrameSize, v)
	}
	g.max = v
	return nil
}

// Check validates one frame's declared length. It returns a *SizeError (which
// wraps ErrFrameSize) when the length exceeds the maximum or is too small for
// the frame type, and nil when the frame is acceptable.
func (g *Guard) Check(t FrameType, streamID, length uint32) error {
	if length > g.max {
		return &SizeError{
			Type: t, StreamID: streamID, Length: length,
			Connection: altersConnection(t, streamID),
			reason:     fmt.Sprintf("length %d exceeds max %d", length, g.max),
		}
	}
	if r, ok := rules[t]; ok {
		if r.fixed != 0 && length != r.fixed {
			return &SizeError{
				Type: t, StreamID: streamID, Length: length,
				Connection: altersConnection(t, streamID),
				reason:     fmt.Sprintf("length %d != required %d", length, r.fixed),
			}
		}
		if r.min != 0 && length < r.min {
			return &SizeError{
				Type: t, StreamID: streamID, Length: length,
				Connection: altersConnection(t, streamID),
				reason:     fmt.Sprintf("length %d below minimum %d", length, r.min),
			}
		}
	}
	return nil
}

// Reader reads frames from an underlying stream, enforcing the Guard.
type Reader struct {
	r     io.Reader
	guard *Guard
	hdr   [9]byte
}

// NewReader wraps r with a fresh Guard.
func NewReader(r io.Reader) *Reader { return &Reader{r: r, guard: NewGuard()} }

// Guard exposes the reader's guard so a caller can adjust the maximum.
func (rd *Reader) Guard() *Guard { return rd.guard }

// ReadFrame reads the next frame. On a valid frame it returns the type, stream
// id, and payload. On a stream-level size error it discards the offending
// frame's payload so the connection stays byte-aligned and the caller can read
// the next frame; on a connection-level size error it returns immediately
// without consuming the payload, since the caller will send GOAWAY and close.
func (rd *Reader) ReadFrame() (FrameType, uint32, []byte, error) {
	if _, err := io.ReadFull(rd.r, rd.hdr[:]); err != nil {
		return 0, 0, nil, err
	}
	length := uint32(rd.hdr[0])<<16 | uint32(rd.hdr[1])<<8 | uint32(rd.hdr[2])
	typ := FrameType(rd.hdr[3])
	streamID := binary.BigEndian.Uint32(rd.hdr[5:]) & 0x7FFFFFFF

	if err := rd.guard.Check(typ, streamID, length); err != nil {
		se := err.(*SizeError)
		if !se.Connection {
			// Stream error: skip the payload to resynchronize, then report.
			if _, derr := io.CopyN(io.Discard, rd.r, int64(length)); derr != nil {
				return 0, 0, nil, derr
			}
		}
		return typ, streamID, nil, se
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(rd.r, payload); err != nil {
		return 0, 0, nil, err
	}
	return typ, streamID, payload, nil
}

// WriteFrameHeader writes a 9-byte frame header into buf. Exposed for tests and
// callers that build raw frames.
func WriteFrameHeader(buf []byte, t FrameType, flags byte, streamID, length uint32) {
	buf[0] = byte(length >> 16)
	buf[1] = byte(length >> 8)
	buf[2] = byte(length)
	buf[3] = byte(t)
	buf[4] = flags
	binary.BigEndian.PutUint32(buf[5:], streamID&0x7FFFFFFF)
}
```

## The runnable demo

The demo feeds the reader three frames: a valid DATA frame, an oversized DATA frame on a stream (recoverable), and an oversized HEADERS frame header (connection-fatal). It prints how each is handled, recovering from the stream error and stopping at the connection error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"

	fs "example.com/h2framesize"
)

func main() {
	var buf bytes.Buffer

	// Frame 1: a valid DATA frame within the default 16384 limit.
	writeFrame(&buf, fs.FrameData, 1, []byte("payload-ok"))
	// Frame 2: an oversized DATA frame on stream 3 (stream error, recoverable).
	writeFrame(&buf, fs.FrameData, 3, make([]byte, 20000))
	// Frame 3: an oversized HEADERS frame header on stream 5 (connection error).
	var hdr [9]byte
	fs.WriteFrameHeader(hdr[:], fs.FrameHeaders, 0, 5, 20000)
	buf.Write(hdr[:])

	rd := fs.NewReader(&buf)
	fmt.Printf("max frame size: %d\n", rd.Guard().MaxFrameSize())

	for i := 1; i <= 3; i++ {
		typ, sid, payload, err := rd.ReadFrame()
		if err == nil {
			fmt.Printf("frame %d: type=%d stream=%d ok (%d bytes)\n", i, typ, sid, len(payload))
			continue
		}
		var se *fs.SizeError
		if !errors.As(err, &se) {
			log.Fatal(err)
		}
		scope := "stream"
		if se.Connection {
			scope = "connection"
		}
		fmt.Printf("frame %d: rejected stream=%d as %s error (code 0x%x)\n",
			i, se.StreamID, scope, fs.ErrCodeFrameSize)
		if se.Connection {
			fmt.Println("  -> GOAWAY and close")
			break
		}
		fmt.Println("  -> RST_STREAM, continue reading")
	}
}

func writeFrame(buf *bytes.Buffer, t fs.FrameType, streamID uint32, payload []byte) {
	var hdr [9]byte
	fs.WriteFrameHeader(hdr[:], t, 0, streamID, uint32(len(payload)))
	buf.Write(hdr[:])
	buf.Write(payload)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
max frame size: 16384
frame 1: type=0 stream=1 ok (10 bytes)
frame 2: rejected stream=3 as stream error (code 0x6)
  -> RST_STREAM, continue reading
frame 3: rejected stream=5 as connection error (code 0x6)
  -> GOAWAY and close
```

## Tests

`TestAcceptsInBoundsFrame` confirms a normal frame passes through with its payload. `TestOversizedHeadersIsConnectionError` asserts an over-limit HEADERS is connection-scoped and that the reader does not try to consume the absent payload. `TestOversizedDataIsStreamError` is the recovery test: an over-limit DATA frame is stream-scoped, its payload is discarded, and a following PING reads cleanly. `TestUndersizedRSTStreamIsStreamError` covers the minimum-length path with the same recovery. `TestUndersizedPingIsConnectionError` shows stream 0 forces connection scope regardless of type. `TestDynamicMaxFrameSize` raises the limit and accepts a frame that was previously too large, and `TestSetMaxFrameSizeRange` checks the bounds on the setter.

Create `framesize_test.go`:

```go
package h2framesize

import (
	"bytes"
	"errors"
	"testing"
)

func frame(t FrameType, streamID, length uint32) []byte {
	var h [9]byte
	WriteFrameHeader(h[:], t, 0, streamID, length)
	return h[:]
}

func TestAcceptsInBoundsFrame(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	buf.Write(frame(FrameData, 1, 5))
	buf.Write([]byte("hello"))

	rd := NewReader(&buf)
	typ, sid, payload, err := rd.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if typ != FrameData || sid != 1 || string(payload) != "hello" {
		t.Errorf("got type=%d stream=%d payload=%q", typ, sid, payload)
	}
}

func TestOversizedHeadersIsConnectionError(t *testing.T) {
	t.Parallel()
	// HEADERS length 20000 exceeds the default 16384 limit. Because HEADERS
	// carries a field block, the violation must be a connection error and the
	// reader must NOT try to consume the (absent) 20000-byte payload.
	var buf bytes.Buffer
	buf.Write(frame(FrameHeaders, 1, 20000))

	rd := NewReader(&buf)
	_, _, _, err := rd.ReadFrame()
	var se *SizeError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SizeError", err)
	}
	if !se.Connection {
		t.Error("Connection = false, want true for oversized HEADERS")
	}
	if !errors.Is(err, ErrFrameSize) {
		t.Error("error does not wrap ErrFrameSize")
	}
}

func TestOversizedDataIsStreamError(t *testing.T) {
	t.Parallel()
	// DATA on a non-zero stream is not connection-altering, so an oversized DATA
	// frame is a stream error; the reader discards the payload and keeps going.
	var buf bytes.Buffer
	big := 20000
	buf.Write(frame(FrameData, 3, uint32(big)))
	buf.Write(make([]byte, big))
	// A valid PING follows; it must still be readable after the stream error.
	buf.Write(frame(FramePing, 0, 8))
	buf.Write(make([]byte, 8))

	rd := NewReader(&buf)
	_, sid, _, err := rd.ReadFrame()
	var se *SizeError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SizeError", err)
	}
	if se.Connection {
		t.Error("Connection = true, want false for oversized DATA")
	}
	if sid != 3 {
		t.Errorf("stream = %d, want 3", sid)
	}
	typ, _, _, err := rd.ReadFrame()
	if err != nil {
		t.Fatalf("second ReadFrame failed: %v", err)
	}
	if typ != FramePing {
		t.Errorf("recovered frame type = %d, want PING", typ)
	}
}

func TestUndersizedRSTStreamIsStreamError(t *testing.T) {
	t.Parallel()
	// RST_STREAM must be exactly 4 bytes; 3 is a stream error. The reader
	// discards the 3 bytes and the following PING reads cleanly.
	var buf bytes.Buffer
	buf.Write(frame(FrameRSTStream, 5, 3))
	buf.Write([]byte{0, 0, 0})
	buf.Write(frame(FramePing, 0, 8))
	buf.Write(make([]byte, 8))

	rd := NewReader(&buf)
	_, _, _, err := rd.ReadFrame()
	var se *SizeError
	if !errors.As(err, &se) || se.Connection {
		t.Fatalf("err = %v, want non-connection *SizeError", err)
	}
	typ, _, _, err := rd.ReadFrame()
	if err != nil || typ != FramePing {
		t.Fatalf("recovery failed: type=%d err=%v", typ, err)
	}
}

func TestUndersizedPingIsConnectionError(t *testing.T) {
	t.Parallel()
	// PING is on stream 0, so any size error is a connection error.
	var buf bytes.Buffer
	buf.Write(frame(FramePing, 0, 9))
	buf.Write(make([]byte, 9))

	rd := NewReader(&buf)
	_, _, _, err := rd.ReadFrame()
	var se *SizeError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *SizeError", err)
	}
	if !se.Connection {
		t.Error("Connection = false, want true for PING on stream 0")
	}
}

func TestDynamicMaxFrameSize(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	buf.Write(frame(FrameHeaders, 1, 20000))
	buf.Write(make([]byte, 20000))

	rd := NewReader(&buf)
	if err := rd.Guard().SetMaxFrameSize(1 << 20); err != nil {
		t.Fatal(err)
	}
	typ, _, payload, err := rd.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame failed after raising max: %v", err)
	}
	if typ != FrameHeaders || len(payload) != 20000 {
		t.Errorf("got type=%d len=%d, want HEADERS/20000", typ, len(payload))
	}
}

func TestSetMaxFrameSizeRange(t *testing.T) {
	t.Parallel()
	g := NewGuard()
	if err := g.SetMaxFrameSize(1024); !errors.Is(err, ErrBadMaxFrameSize) {
		t.Errorf("low: err = %v, want ErrBadMaxFrameSize", err)
	}
	if err := g.SetMaxFrameSize(MaxMaxFrameSize + 1); !errors.Is(err, ErrBadMaxFrameSize) {
		t.Errorf("high: err = %v, want ErrBadMaxFrameSize", err)
	}
	if err := g.SetMaxFrameSize(1 << 16); err != nil {
		t.Errorf("valid: err = %v, want nil", err)
	}
	if g.MaxFrameSize() != 1<<16 {
		t.Errorf("max = %d, want %d", g.MaxFrameSize(), 1<<16)
	}
}
```

## Review

The guard is correct when the size check runs before any payload allocation and every violation carries the right scope. The classification is the crux: a size error escalates to a connection error only for field-block frames (HEADERS, PUSH_PROMISE, CONTINUATION), SETTINGS, or anything on stream 0 — escalating an oversized DATA frame would needlessly kill a connection that one RST_STREAM could fix, while failing to escalate a corrupt HEADERS leaves header decompression ambiguous. Recovery depends on discarding the payload of a stream-scoped error so the stream stays byte-aligned; skip that and the next frame reads garbage. The `-race` run, with the recover-after-stream-error and connection-error tests, is the proof.

## Resources

- [RFC 9113 §4.2 — Frame Size](https://www.rfc-editor.org/rfc/rfc9113#section-4.2) — the maximum, the must-receive 16384 floor, and the connection-vs-stream classification of a size error.
- [RFC 9113 §5.4 — Error Handling](https://www.rfc-editor.org/rfc/rfc9113#section-5.4) — connection errors (GOAWAY) versus stream errors (RST_STREAM).
- [RFC 9113 §6.5.2 — Defined Settings](https://www.rfc-editor.org/rfc/rfc9113#section-6.5.2) — the SETTINGS_MAX_FRAME_SIZE range that bounds the mutable maximum.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [03-settings-validation.md](03-settings-validation.md) | Next: [HPACK Header Compression](../02-hpack-header-compression/00-concepts.md)
