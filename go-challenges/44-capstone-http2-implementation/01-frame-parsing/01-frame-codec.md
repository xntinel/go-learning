# Exercise 1: The Frame Codec

A `Framer` wraps an `io.Reader`/`io.Writer` pair and turns the byte stream into typed HTTP/2 frames and back. This module implements the full read and write path for the six connection-critical frame types — DATA, SETTINGS, RST_STREAM, PING, GOAWAY, WINDOW_UPDATE — plus an `UnknownFrame` catch-all, validating every stream-id and length constraint along the way.

This module is fully self-contained: it begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
h2frame/
  go.mod
  frame.go              FrameType, FrameFlags, ErrCode, SettingID, the frame structs, Framer
  frame_test.go         round-trip every frame type; reject oversized/malformed frames
  cmd/demo/main.go      drive a full SETTINGS/DATA/PING/GOAWAY exchange over a buffer
```

- Files: `frame.go`, `frame_test.go`, `cmd/demo/main.go`.
- Implement: `Framer` with `ReadFrame` plus `WriteData`/`WriteSettings`/`WriteSettingsAck`/`WriteRSTStream`/`WritePing`/`WriteGoaway`/`WriteWindowUpdate`.
- Test: every frame type round-trips through a shared buffer; oversized, wrong-stream, and wrong-length frames return the matching sentinel error.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p h2frame/cmd/demo && cd h2frame
go mod init example.com/h2frame
go mod edit -go=1.26
```

## The type system and the framer

`frame.go` carries the whole codec. The 8-bit `FrameType` and `FrameFlags`, the 32-bit `ErrCode`, and the 16-bit `SettingID` are all named constants from RFC 9113 §11, so wire values read as protocol names rather than magic numbers. Each concrete frame struct embeds `FrameHeader`, which gives every frame a uniform `Header()` accessor through the `Frame` interface and lets the parser fill the shared fields once.

The read path is one function. `ReadFrame` calls `io.ReadFull` for the fixed 9 octets, decodes the header with three-octet shift arithmetic for the length and a masked `Uint32` for the stream id, rejects anything longer than `maxFrameSize` *before* allocating the payload, reads exactly `Length` more octets, and dispatches on the type. The dispatch default returns an `UnknownFrame` rather than an error — the RFC 9113 §4.1 forward-compatibility rule. Each per-type parser enforces its own constraints: SETTINGS, PING, and GOAWAY demand stream 0; RST_STREAM demands a non-zero stream; SETTINGS demands a length that is a multiple of 6; PING demands exactly 8 octets. A PADDED DATA frame whose pad length meets or exceeds the payload is rejected as a corrupt frame.

The write path mirrors it: each `Write*` method builds a `FrameHeader`, serializes the 9 octets with `writeFrameHeader`, and writes the payload. Because read and write share the same header encoding and the same constants, a value written by `WriteSettings` decodes back to an identical `SettingsFrame` — the property the tests assert end to end.

Create `frame.go`:

```go
package h2frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// FrameType is the 8-bit type field in an HTTP/2 frame header (RFC 9113 §11.2).
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

func (t FrameType) String() string {
	switch t {
	case FrameData:
		return "DATA"
	case FrameHeaders:
		return "HEADERS"
	case FramePriority:
		return "PRIORITY"
	case FrameRSTStream:
		return "RST_STREAM"
	case FrameSettings:
		return "SETTINGS"
	case FramePushPromise:
		return "PUSH_PROMISE"
	case FramePing:
		return "PING"
	case FrameGoaway:
		return "GOAWAY"
	case FrameWindowUpdate:
		return "WINDOW_UPDATE"
	case FrameContinuation:
		return "CONTINUATION"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", uint8(t))
	}
}

// FrameFlags is the 8-bit flags field in an HTTP/2 frame header.
type FrameFlags uint8

const (
	// FlagEndStream (0x1) marks the final DATA or HEADERS frame for a stream.
	FlagEndStream FrameFlags = 0x1
	// FlagACK (0x1) is the reply flag for SETTINGS and PING frames.
	FlagACK FrameFlags = 0x1
	// FlagEndHeaders (0x4) applies to HEADERS, PUSH_PROMISE, and CONTINUATION frames.
	FlagEndHeaders FrameFlags = 0x4
	// FlagPadded (0x8) applies to DATA, HEADERS, and PUSH_PROMISE frames.
	FlagPadded FrameFlags = 0x8
	// FlagPriority (0x20) is set on HEADERS frames that carry a priority block.
	FlagPriority FrameFlags = 0x20
)

// Has reports whether f includes the flag bit g.
func (f FrameFlags) Has(g FrameFlags) bool { return f&g != 0 }

// ErrCode is the 32-bit error code used in RST_STREAM and GOAWAY frames
// (RFC 9113 §7).
type ErrCode uint32

const (
	ErrCodeNo                 ErrCode = 0x0
	ErrCodeProtocol           ErrCode = 0x1
	ErrCodeInternal           ErrCode = 0x2
	ErrCodeFlowControl        ErrCode = 0x3
	ErrCodeSettingsTimeout    ErrCode = 0x4
	ErrCodeStreamClosed       ErrCode = 0x5
	ErrCodeFrameSize          ErrCode = 0x6
	ErrCodeRefusedStream      ErrCode = 0x7
	ErrCodeCancel             ErrCode = 0x8
	ErrCodeCompression        ErrCode = 0x9
	ErrCodeConnect            ErrCode = 0xa
	ErrCodeEnhanceYourCalm    ErrCode = 0xb
	ErrCodeInadequateSecurity ErrCode = 0xc
	ErrCodeHTTP11Required     ErrCode = 0xd
)

// SettingID is the 16-bit identifier for a SETTINGS parameter (RFC 9113 §6.5.2).
type SettingID uint16

const (
	SettingHeaderTableSize      SettingID = 0x1
	SettingEnablePush           SettingID = 0x2
	SettingMaxConcurrentStreams SettingID = 0x3
	SettingInitialWindowSize    SettingID = 0x4
	SettingMaxFrameSize         SettingID = 0x5
	SettingMaxHeaderListSize    SettingID = 0x6
)

// Setting is a single SETTINGS parameter (6 bytes on the wire).
type Setting struct {
	ID    SettingID
	Value uint32
}

// Sentinel errors returned by Framer methods.
var (
	ErrFrameSizeLimit = errors.New("h2frame: frame length exceeds maximum")
	ErrBadStreamID    = errors.New("h2frame: stream identifier violates RFC 9113 constraint")
	ErrPadTooLong     = errors.New("h2frame: pad length exceeds payload")
	ErrBadPayload     = errors.New("h2frame: payload length invalid for frame type")
)

// DefaultMaxFrameSize is the initial maximum frame payload size (RFC 9113 §6.5.2).
const DefaultMaxFrameSize uint32 = 16384

// FrameHeader is the fixed 9-byte prefix of every HTTP/2 frame.
type FrameHeader struct {
	// Length is the 24-bit payload size (0-16777215).
	Length uint32
	Type   FrameType
	Flags  FrameFlags
	// StreamID is the 31-bit stream identifier; the high reserved bit is masked off.
	StreamID uint32
}

// Frame is the common interface satisfied by every concrete frame type.
type Frame interface {
	Header() FrameHeader
}

// DataFrame carries request or response body data (RFC 9113 §6.1).
// When FlagPadded is set, the parser strips the pad bytes; Data contains
// only the application payload.
type DataFrame struct {
	FrameHeader
	Data []byte
}

func (f *DataFrame) Header() FrameHeader { return f.FrameHeader }

// SettingsFrame carries configuration parameters (RFC 9113 §6.5).
// A SETTINGS ACK has FlagACK set and an empty Settings slice.
type SettingsFrame struct {
	FrameHeader
	Settings []Setting
}

func (f *SettingsFrame) Header() FrameHeader { return f.FrameHeader }

// IsACK reports whether the frame is a SETTINGS acknowledgement.
func (f *SettingsFrame) IsACK() bool { return f.Flags.Has(FlagACK) }

// RSTStreamFrame terminates a stream with an error code (RFC 9113 §6.4).
type RSTStreamFrame struct {
	FrameHeader
	ErrCode ErrCode
}

func (f *RSTStreamFrame) Header() FrameHeader { return f.FrameHeader }

// PingFrame measures round-trip latency (RFC 9113 §6.7).
// A PING with FlagACK set is a reply; the Data field is echoed unchanged.
type PingFrame struct {
	FrameHeader
	Data [8]byte
}

func (f *PingFrame) Header() FrameHeader { return f.FrameHeader }

// IsACK reports whether the PING is a reply to a peer's PING.
func (f *PingFrame) IsACK() bool { return f.Flags.Has(FlagACK) }

// GoawayFrame signals graceful connection shutdown (RFC 9113 §6.8).
type GoawayFrame struct {
	FrameHeader
	LastStreamID uint32
	ErrCode      ErrCode
	DebugData    []byte
}

func (f *GoawayFrame) Header() FrameHeader { return f.FrameHeader }

// WindowUpdateFrame adjusts the flow-control window (RFC 9113 §6.9).
// StreamID 0 adjusts the connection window; non-zero adjusts a stream window.
type WindowUpdateFrame struct {
	FrameHeader
	Increment uint32 // 31-bit; a value of 0 is a PROTOCOL_ERROR
}

func (f *WindowUpdateFrame) Header() FrameHeader { return f.FrameHeader }

// UnknownFrame holds the raw payload of an unrecognized frame type.
// RFC 9113 §4.1: implementations must ignore and discard unknown frame types.
type UnknownFrame struct {
	FrameHeader
	Payload []byte
}

func (f *UnknownFrame) Header() FrameHeader { return f.FrameHeader }

// Framer reads and writes HTTP/2 frames. It is not safe for concurrent use.
type Framer struct {
	r            io.Reader
	w            io.Writer
	maxFrameSize uint32
	headerBuf    [9]byte
}

// NewFramer returns a Framer that writes to w and reads from r.
func NewFramer(w io.Writer, r io.Reader) *Framer {
	return &Framer{r: r, w: w, maxFrameSize: DefaultMaxFrameSize}
}

// SetMaxReadFrameSize changes the maximum payload length ReadFrame accepts.
// Values below DefaultMaxFrameSize are silently clamped up.
func (fr *Framer) SetMaxReadFrameSize(max uint32) {
	if max < DefaultMaxFrameSize {
		max = DefaultMaxFrameSize
	}
	fr.maxFrameSize = max
}

// ReadFrame reads the next frame from the underlying reader. The returned
// Frame value's slice fields (Data, DebugData, Payload) are valid only until
// the next call to ReadFrame.
func (fr *Framer) ReadFrame() (Frame, error) {
	if _, err := io.ReadFull(fr.r, fr.headerBuf[:]); err != nil {
		return nil, err
	}
	hdr := parseFrameHeader(fr.headerBuf)
	if hdr.Length > fr.maxFrameSize {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrFrameSizeLimit, hdr.Length, fr.maxFrameSize)
	}
	payload := make([]byte, hdr.Length)
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		return nil, err
	}
	return parseFrame(hdr, payload)
}

// WriteData writes a DATA frame on the given stream. Pass FlagEndStream to
// mark the final frame for the stream.
func (fr *Framer) WriteData(streamID uint32, flags FrameFlags, data []byte) error {
	return fr.writeFrame(FrameHeader{
		Length:   uint32(len(data)),
		Type:     FrameData,
		Flags:    flags,
		StreamID: streamID,
	}, data)
}

// WriteSettings writes a SETTINGS frame with the given parameters.
func (fr *Framer) WriteSettings(settings []Setting) error {
	payload := make([]byte, 6*len(settings))
	for i, s := range settings {
		binary.BigEndian.PutUint16(payload[i*6:], uint16(s.ID))
		binary.BigEndian.PutUint32(payload[i*6+2:], s.Value)
	}
	return fr.writeFrame(FrameHeader{
		Length: uint32(len(payload)),
		Type:   FrameSettings,
	}, payload)
}

// WriteSettingsAck writes a zero-length SETTINGS frame with FlagACK set.
func (fr *Framer) WriteSettingsAck() error {
	return fr.writeFrame(FrameHeader{Type: FrameSettings, Flags: FlagACK}, nil)
}

// WriteRSTStream writes a RST_STREAM frame on the given stream.
func (fr *Framer) WriteRSTStream(streamID uint32, code ErrCode) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], uint32(code))
	return fr.writeFrame(FrameHeader{
		Length:   4,
		Type:     FrameRSTStream,
		StreamID: streamID,
	}, payload[:])
}

// WritePing writes a PING frame. Set ack to true when replying to a peer's PING.
func (fr *Framer) WritePing(ack bool, data [8]byte) error {
	var flags FrameFlags
	if ack {
		flags = FlagACK
	}
	return fr.writeFrame(FrameHeader{Length: 8, Type: FramePing, Flags: flags}, data[:])
}

// WriteGoaway writes a GOAWAY frame and signals connection shutdown.
func (fr *Framer) WriteGoaway(lastStreamID uint32, code ErrCode, debugData []byte) error {
	payload := make([]byte, 8+len(debugData))
	binary.BigEndian.PutUint32(payload[0:], lastStreamID&0x7FFFFFFF)
	binary.BigEndian.PutUint32(payload[4:], uint32(code))
	copy(payload[8:], debugData)
	return fr.writeFrame(FrameHeader{
		Length: uint32(len(payload)),
		Type:   FrameGoaway,
	}, payload)
}

// WriteWindowUpdate writes a WINDOW_UPDATE frame. Use streamID 0 for the
// connection-level window.
func (fr *Framer) WriteWindowUpdate(streamID, increment uint32) error {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], increment&0x7FFFFFFF)
	return fr.writeFrame(FrameHeader{
		Length:   4,
		Type:     FrameWindowUpdate,
		StreamID: streamID,
	}, payload[:])
}

// writeFrame serializes hdr and payload and writes them to the underlying writer.
func (fr *Framer) writeFrame(hdr FrameHeader, payload []byte) error {
	var buf [9]byte
	writeFrameHeader(buf[:], hdr)
	if _, err := fr.w.Write(buf[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := fr.w.Write(payload)
		return err
	}
	return nil
}

// parseFrameHeader decodes the fixed 9-byte header from b.
func parseFrameHeader(b [9]byte) FrameHeader {
	return FrameHeader{
		Length:   uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]),
		Type:     FrameType(b[3]),
		Flags:    FrameFlags(b[4]),
		StreamID: binary.BigEndian.Uint32(b[5:]) & 0x7FFFFFFF,
	}
}

// writeFrameHeader encodes hdr into the first 9 bytes of buf.
func writeFrameHeader(buf []byte, hdr FrameHeader) {
	buf[0] = byte(hdr.Length >> 16)
	buf[1] = byte(hdr.Length >> 8)
	buf[2] = byte(hdr.Length)
	buf[3] = byte(hdr.Type)
	buf[4] = byte(hdr.Flags)
	binary.BigEndian.PutUint32(buf[5:], hdr.StreamID&0x7FFFFFFF)
}

// parseFrame dispatches payload parsing based on the frame type.
func parseFrame(hdr FrameHeader, payload []byte) (Frame, error) {
	switch hdr.Type {
	case FrameData:
		return parseDataFrame(hdr, payload)
	case FrameSettings:
		return parseSettingsFrame(hdr, payload)
	case FrameRSTStream:
		return parseRSTStreamFrame(hdr, payload)
	case FramePing:
		return parsePingFrame(hdr, payload)
	case FrameGoaway:
		return parseGoawayFrame(hdr, payload)
	case FrameWindowUpdate:
		return parseWindowUpdateFrame(hdr, payload)
	default:
		return &UnknownFrame{FrameHeader: hdr, Payload: payload}, nil
	}
}

// parseDataFrame handles DATA frames (RFC 9113 §6.1).
func parseDataFrame(hdr FrameHeader, payload []byte) (*DataFrame, error) {
	data := payload
	if hdr.Flags.Has(FlagPadded) {
		if len(payload) == 0 {
			return nil, fmt.Errorf("%w: DATA PADDED flag with empty payload", ErrBadPayload)
		}
		padLen := int(payload[0])
		if padLen >= len(payload) {
			return nil, fmt.Errorf("%w: pad %d >= payload %d", ErrPadTooLong, padLen, len(payload))
		}
		data = payload[1 : len(payload)-padLen]
	}
	return &DataFrame{FrameHeader: hdr, Data: data}, nil
}

// parseSettingsFrame handles SETTINGS frames (RFC 9113 §6.5).
func parseSettingsFrame(hdr FrameHeader, payload []byte) (*SettingsFrame, error) {
	if hdr.StreamID != 0 {
		return nil, fmt.Errorf("%w: SETTINGS must have stream ID 0, got %d", ErrBadStreamID, hdr.StreamID)
	}
	if hdr.Flags.Has(FlagACK) {
		if len(payload) != 0 {
			return nil, fmt.Errorf("%w: SETTINGS ACK payload must be empty", ErrBadPayload)
		}
		return &SettingsFrame{FrameHeader: hdr}, nil
	}
	if len(payload)%6 != 0 {
		return nil, fmt.Errorf("%w: SETTINGS length %d not a multiple of 6", ErrBadPayload, len(payload))
	}
	settings := make([]Setting, 0, len(payload)/6)
	for i := 0; i < len(payload); i += 6 {
		settings = append(settings, Setting{
			ID:    SettingID(binary.BigEndian.Uint16(payload[i:])),
			Value: binary.BigEndian.Uint32(payload[i+2:]),
		})
	}
	return &SettingsFrame{FrameHeader: hdr, Settings: settings}, nil
}

// parseRSTStreamFrame handles RST_STREAM frames (RFC 9113 §6.4).
func parseRSTStreamFrame(hdr FrameHeader, payload []byte) (*RSTStreamFrame, error) {
	if hdr.StreamID == 0 {
		return nil, fmt.Errorf("%w: RST_STREAM must have non-zero stream ID", ErrBadStreamID)
	}
	if len(payload) != 4 {
		return nil, fmt.Errorf("%w: RST_STREAM payload must be 4 bytes, got %d", ErrBadPayload, len(payload))
	}
	return &RSTStreamFrame{
		FrameHeader: hdr,
		ErrCode:     ErrCode(binary.BigEndian.Uint32(payload)),
	}, nil
}

// parsePingFrame handles PING frames (RFC 9113 §6.7).
func parsePingFrame(hdr FrameHeader, payload []byte) (*PingFrame, error) {
	if hdr.StreamID != 0 {
		return nil, fmt.Errorf("%w: PING must have stream ID 0, got %d", ErrBadStreamID, hdr.StreamID)
	}
	if len(payload) != 8 {
		return nil, fmt.Errorf("%w: PING payload must be 8 bytes, got %d", ErrBadPayload, len(payload))
	}
	f := &PingFrame{FrameHeader: hdr}
	copy(f.Data[:], payload)
	return f, nil
}

// parseGoawayFrame handles GOAWAY frames (RFC 9113 §6.8).
func parseGoawayFrame(hdr FrameHeader, payload []byte) (*GoawayFrame, error) {
	if hdr.StreamID != 0 {
		return nil, fmt.Errorf("%w: GOAWAY must have stream ID 0, got %d", ErrBadStreamID, hdr.StreamID)
	}
	if len(payload) < 8 {
		return nil, fmt.Errorf("%w: GOAWAY payload must be at least 8 bytes, got %d", ErrBadPayload, len(payload))
	}
	debug := make([]byte, len(payload)-8)
	copy(debug, payload[8:])
	return &GoawayFrame{
		FrameHeader:  hdr,
		LastStreamID: binary.BigEndian.Uint32(payload[0:]) & 0x7FFFFFFF,
		ErrCode:      ErrCode(binary.BigEndian.Uint32(payload[4:])),
		DebugData:    debug,
	}, nil
}

// parseWindowUpdateFrame handles WINDOW_UPDATE frames (RFC 9113 §6.9).
func parseWindowUpdateFrame(hdr FrameHeader, payload []byte) (*WindowUpdateFrame, error) {
	if len(payload) != 4 {
		return nil, fmt.Errorf("%w: WINDOW_UPDATE payload must be 4 bytes, got %d", ErrBadPayload, len(payload))
	}
	return &WindowUpdateFrame{
		FrameHeader: hdr,
		Increment:   binary.BigEndian.Uint32(payload) & 0x7FFFFFFF,
	}, nil
}
```

`parseFrameHeader` uses the three-octet shift rather than a four-octet read because the length field is 24 bits, not 32. `parseFrame` is a dispatch table whose default returns `UnknownFrame` rather than an error, which is exactly the forward-compatibility requirement of RFC 9113 §4.1.

## The runnable demo

The demo drives a complete frame exchange over a single in-memory buffer: an initial SETTINGS, its ACK, a DATA frame on stream 1, a PING, and a closing GOAWAY. Each frame is written and then read back, so the output is proof that read and write agree on every type.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"

	"example.com/h2frame"
)

func main() {
	// Simulate frame exchange over an in-memory pipe.
	var pipe bytes.Buffer
	fr := h2frame.NewFramer(&pipe, &pipe)

	// Write initial SETTINGS (client to server).
	settings := []h2frame.Setting{
		{ID: h2frame.SettingMaxConcurrentStreams, Value: 100},
		{ID: h2frame.SettingInitialWindowSize, Value: 65535},
	}
	if err := fr.WriteSettings(settings); err != nil {
		log.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		log.Fatal(err)
	}
	sf := f.(*h2frame.SettingsFrame)
	fmt.Printf("SETTINGS: %d parameter(s)\n", len(sf.Settings))
	for _, s := range sf.Settings {
		fmt.Printf("  id=0x%04x value=%d\n", uint16(s.ID), s.Value)
	}

	// ACK the SETTINGS (server to client).
	if err := fr.WriteSettingsAck(); err != nil {
		log.Fatal(err)
	}
	f, err = fr.ReadFrame()
	if err != nil {
		log.Fatal(err)
	}
	ack := f.(*h2frame.SettingsFrame)
	fmt.Printf("SETTINGS ACK: ack=%v\n", ack.IsACK())

	// DATA frame on stream 1.
	if err := fr.WriteData(1, h2frame.FlagEndStream, []byte("GET / HTTP/2")); err != nil {
		log.Fatal(err)
	}
	f, err = fr.ReadFrame()
	if err != nil {
		log.Fatal(err)
	}
	df := f.(*h2frame.DataFrame)
	fmt.Printf("DATA stream=%d end=%v payload=%q\n",
		df.StreamID,
		df.Flags.Has(h2frame.FlagEndStream),
		string(df.Data),
	)

	// PING exchange.
	var pingData [8]byte
	copy(pingData[:], "go-http2")
	if err := fr.WritePing(false, pingData); err != nil {
		log.Fatal(err)
	}
	f, err = fr.ReadFrame()
	if err != nil {
		log.Fatal(err)
	}
	pf := f.(*h2frame.PingFrame)
	fmt.Printf("PING data=%s\n", string(pf.Data[:]))

	// GOAWAY to close the connection.
	if err := fr.WriteGoaway(1, h2frame.ErrCodeNo, nil); err != nil {
		log.Fatal(err)
	}
	f, err = fr.ReadFrame()
	if err != nil {
		log.Fatal(err)
	}
	gf := f.(*h2frame.GoawayFrame)
	fmt.Printf("GOAWAY last=%d code=%d\n", gf.LastStreamID, gf.ErrCode)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
SETTINGS: 2 parameter(s)
  id=0x0003 value=100
  id=0x0004 value=65535
SETTINGS ACK: ack=true
DATA stream=1 end=true payload="GET / HTTP/2"
PING data=go-http2
GOAWAY last=1 code=0
```

## Tests

The tests are the verification — there is no `main` to eyeball for the library itself. Each round-trip test writes a frame and reads it back through a shared `bytes.Buffer`, asserting the decoded value equals what was written. The rejection tests hand-build raw frame headers that violate a constraint — an oversized length, SETTINGS on a non-zero stream, RST_STREAM on stream 0, a 5-octet PING — and assert that `ReadFrame` returns an error matching the right sentinel with `errors.Is`. `TestUnknownFrameTypePassesThrough` confirms an unrecognized type is preserved rather than rejected.

Create `frame_test.go`:

```go
package h2frame

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

// newPipeFramer returns a Framer whose reader and writer share a single
// bytes.Buffer. Writes go into the buffer; reads consume from the same buffer.
func newPipeFramer() (*Framer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return NewFramer(buf, buf), buf
}

func TestPingRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	var want [8]byte
	copy(want[:], "testdata")
	if err := fr.WritePing(false, want); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	pf, ok := f.(*PingFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *PingFrame", f)
	}
	if pf.Data != want {
		t.Errorf("data = %q, want %q", pf.Data[:], want[:])
	}
	if pf.IsACK() {
		t.Error("IsACK = true, want false")
	}
}

func TestPingACKRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	var want [8]byte
	copy(want[:], "pingback")
	if err := fr.WritePing(true, want); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	pf := f.(*PingFrame)
	if !pf.IsACK() {
		t.Error("IsACK = false, want true")
	}
	if pf.Data != want {
		t.Errorf("data = %q, want %q", pf.Data[:], want[:])
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	want := []Setting{
		{SettingMaxConcurrentStreams, 100},
		{SettingInitialWindowSize, 65535},
	}
	if err := fr.WriteSettings(want); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *SettingsFrame", f)
	}
	if sf.IsACK() {
		t.Error("IsACK = true, want false")
	}
	if len(sf.Settings) != len(want) {
		t.Fatalf("settings count = %d, want %d", len(sf.Settings), len(want))
	}
	for i, s := range sf.Settings {
		if s != want[i] {
			t.Errorf("settings[%d] = %+v, want %+v", i, s, want[i])
		}
	}
}

func TestSettingsACKRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	if err := fr.WriteSettingsAck(); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	sf, ok := f.(*SettingsFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *SettingsFrame", f)
	}
	if !sf.IsACK() {
		t.Error("IsACK = false, want true")
	}
	if len(sf.Settings) != 0 {
		t.Errorf("SETTINGS ACK should have no parameters, got %v", sf.Settings)
	}
}

func TestDataFrameRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	payload := []byte("hello, http2")
	if err := fr.WriteData(1, FlagEndStream, payload); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	df, ok := f.(*DataFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *DataFrame", f)
	}
	if !bytes.Equal(df.Data, payload) {
		t.Errorf("data = %q, want %q", df.Data, payload)
	}
	if !df.Flags.Has(FlagEndStream) {
		t.Error("END_STREAM flag not set")
	}
	if df.StreamID != 1 {
		t.Errorf("stream ID = %d, want 1", df.StreamID)
	}
}

func TestRSTStreamRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	if err := fr.WriteRSTStream(3, ErrCodeCancel); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	rf, ok := f.(*RSTStreamFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *RSTStreamFrame", f)
	}
	if rf.StreamID != 3 {
		t.Errorf("stream ID = %d, want 3", rf.StreamID)
	}
	if rf.ErrCode != ErrCodeCancel {
		t.Errorf("err code = %v, want ErrCodeCancel", rf.ErrCode)
	}
}

func TestGoawayRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	debug := []byte("server shutting down")
	if err := fr.WriteGoaway(7, ErrCodeProtocol, debug); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	gf, ok := f.(*GoawayFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *GoawayFrame", f)
	}
	if gf.LastStreamID != 7 {
		t.Errorf("lastStreamID = %d, want 7", gf.LastStreamID)
	}
	if gf.ErrCode != ErrCodeProtocol {
		t.Errorf("err code = %v, want ErrCodeProtocol", gf.ErrCode)
	}
	if !bytes.Equal(gf.DebugData, debug) {
		t.Errorf("debug = %q, want %q", gf.DebugData, debug)
	}
}

func TestWindowUpdateRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipeFramer()

	if err := fr.WriteWindowUpdate(0, 65535); err != nil {
		t.Fatal(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	wf, ok := f.(*WindowUpdateFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *WindowUpdateFrame", f)
	}
	if wf.Increment != 65535 {
		t.Errorf("increment = %d, want 65535", wf.Increment)
	}
}

func TestReadFrameRejectsOversizedPayload(t *testing.T) {
	t.Parallel()
	// Write only the 9-byte header claiming 32768 bytes (> DefaultMaxFrameSize).
	// ReadFrame returns ErrFrameSizeLimit before attempting to read the payload.
	var buf bytes.Buffer
	var raw [9]byte
	writeFrameHeader(raw[:], FrameHeader{Length: 32768, Type: FramePing})
	buf.Write(raw[:])

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadFrame()
	if !errors.Is(err, ErrFrameSizeLimit) {
		t.Errorf("err = %v, want error wrapping ErrFrameSizeLimit", err)
	}
}

func TestSettingsRejectsNonZeroStreamID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var raw [9]byte
	writeFrameHeader(raw[:], FrameHeader{Length: 0, Type: FrameSettings, StreamID: 1})
	buf.Write(raw[:])

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadFrame()
	if !errors.Is(err, ErrBadStreamID) {
		t.Errorf("err = %v, want error wrapping ErrBadStreamID", err)
	}
}

func TestRSTStreamRejectsZeroStreamID(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var raw [9]byte
	writeFrameHeader(raw[:], FrameHeader{Length: 4, Type: FrameRSTStream, StreamID: 0})
	buf.Write(raw[:])
	buf.Write([]byte{0, 0, 0, 1})

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadFrame()
	if !errors.Is(err, ErrBadStreamID) {
		t.Errorf("err = %v, want error wrapping ErrBadStreamID", err)
	}
}

func TestPingRejectsBadPayload(t *testing.T) {
	t.Parallel()
	// A PING frame's payload must be exactly 8 bytes (RFC 9113 §6.7). A frame
	// header claiming 5 bytes is malformed; parsePingFrame rejects it.
	var buf bytes.Buffer
	var raw [9]byte
	writeFrameHeader(raw[:], FrameHeader{Length: 5, Type: FramePing})
	buf.Write(raw[:])
	buf.Write([]byte{1, 2, 3, 4, 5})

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadFrame()
	if !errors.Is(err, ErrBadPayload) {
		t.Errorf("err = %v, want error wrapping ErrBadPayload", err)
	}
}

func TestUnknownFrameTypePassesThrough(t *testing.T) {
	t.Parallel()
	rawPayload := []byte{0xDE, 0xAD, 0xBE}
	var buf bytes.Buffer
	var raw [9]byte
	writeFrameHeader(raw[:], FrameHeader{Length: uint32(len(rawPayload)), Type: FrameType(0xFF)})
	buf.Write(raw[:])
	buf.Write(rawPayload)

	fr := NewFramer(&bytes.Buffer{}, &buf)
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	uf, ok := f.(*UnknownFrame)
	if !ok {
		t.Fatalf("ReadFrame returned %T, want *UnknownFrame", f)
	}
	if !bytes.Equal(uf.Payload, rawPayload) {
		t.Errorf("payload = %x, want %x", uf.Payload, rawPayload)
	}
}

func ExampleFramer_ping() {
	var buf bytes.Buffer
	fr := NewFramer(&buf, &buf)

	var data [8]byte
	copy(data[:], "go-http2")
	if err := fr.WritePing(false, data); err != nil {
		panic(err)
	}
	f, err := fr.ReadFrame()
	if err != nil {
		panic(err)
	}
	pf := f.(*PingFrame)
	fmt.Printf("type=%s ack=%v data=%s\n", pf.Type, pf.IsACK(), string(pf.Data[:]))
	// Output: type=PING ack=false data=go-http2
}
```

## Review

The codec is correct when every frame type round-trips byte-for-byte and every constraint violation surfaces the right sentinel. The subtle points are all in the header: read the length as 24 bits with shift arithmetic (a 32-bit read shifts the type and flags off by one), and mask the reserved high bit out of the stream id (a peer that sets it would otherwise make stream 0 fail every connection-level check). Reject the oversized frame *before* allocating its payload, or a hostile peer can force a huge allocation with a single 9-octet header. Keep the unknown-type default returning `UnknownFrame`, not an error, so protocol extensions do not trip a spurious connection error. The `-race` run with the round-trip and rejection tests is the proof.

## Resources

- [RFC 9113 §4.1 — Frame Format](https://www.rfc-editor.org/rfc/rfc9113#section-4.1) — the normative 9-octet header layout, field widths, and the unknown-type discard rule.
- [RFC 9113 §6 — Frame Definitions](https://www.rfc-editor.org/rfc/rfc9113#section-6) — payload layout, flags, and stream-id constraints for every frame type.
- [RFC 9113 §7 — Error Codes](https://www.rfc-editor.org/rfc/rfc9113#section-7) — the codes used in RST_STREAM and GOAWAY.
- [golang.org/x/net/http2 frame.go](https://cs.opensource.google/go/x/net/+/master:http2/frame.go) — the production Go framer; study its buffer reuse and write scheduling, but use the standard library here.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-headers-continuation.md](02-headers-continuation.md)
