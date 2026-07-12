# Exercise 2: HEADERS and CONTINUATION Reassembly

A compressed header block is one logical unit, but a sender may split it across a HEADERS frame followed by any number of CONTINUATION frames. This module reassembles that sequence into a single contiguous header block fragment, stripping the optional pad and priority fields, and enforces the strict ordering rule that makes the split safe.

This module is fully self-contained: it begins with its own `go mod init`, defines its own frame header machinery, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
h2hdr/
  go.mod
  header_block.go       FrameType/Flags, Priority, HeaderBlock, Framer.ReadHeaderBlock
  header_block_test.go  single block, multi-CONTINUATION, padded+priority, sequencing rejects
  cmd/demo/main.go      split a header block across HEADERS + 2 CONTINUATION, reassemble
```

- Files: `header_block.go`, `header_block_test.go`, `cmd/demo/main.go`.
- Implement: `Framer` with `WriteHeaders`/`WriteContinuation` and `ReadHeaderBlock` that concatenates fragments until END_HEADERS.
- Test: a single HEADERS block, a block split across two CONTINUATIONs, a padded+priority HEADERS payload, and rejection of wrong-type, wrong-stream, zero-stream, and over-long blocks.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/01-frame-parsing/02-headers-continuation/cmd/demo && cd go-solutions/44-capstone-http2-implementation/01-frame-parsing/02-headers-continuation
go mod edit -go=1.26
```

## Why reassembly is a protocol-level concern

HPACK decompression is stateful: each header block updates a dynamic table that the next block depends on. A decoder must therefore see a *complete* block, in order, before it decodes anything — a partial block would corrupt the table for the rest of the connection. RFC 9113 §4.3 makes the framing match that requirement: a HEADERS frame opens the block, and while its END_HEADERS flag is unset the block continues in CONTINUATION frames on the same stream until one of them sets END_HEADERS.

The ordering rule is absolute and is why this is a connection-level concern rather than a per-stream one. Once a HEADERS frame without END_HEADERS is seen, the *very next* frame on the whole connection must be a CONTINUATION on the same stream. A frame of any other type, or a CONTINUATION on a different stream, is a PROTOCOL_ERROR that the receiver must treat as a connection error (GOAWAY), because the header-decompression context for the entire connection is now ambiguous. `ReadHeaderBlock` encodes this directly: after the opening HEADERS it loops reading frames, and any frame that is not a same-stream CONTINUATION returns `ErrContinuation`.

Two more details live in the HEADERS payload itself. The PADDED flag prepends a one-octet Pad Length and appends that many padding octets, which `stripHeaders` removes from both ends after a bounds check (a pad length larger than the remaining payload is a PROTOCOL_ERROR). The PRIORITY flag prepends a 5-octet block — a 1-bit exclusive marker, a 31-bit stream dependency, and an 8-bit weight — which must be parsed off the front before the bare fragment remains; a payload too short to hold it is a FRAME_SIZE_ERROR. The priority scheme is deprecated by RFC 9113 §5.3, but the octets are still transmitted, so a conformant parser must still skip them correctly to find the header block. Finally, `maxBlockSize` caps the reassembled total: an attacker can otherwise stream unbounded CONTINUATION frames to exhaust memory, so the accumulator is checked after each append and rejected with `ErrHeaderListTooLong`.

Create `header_block.go`:

```go
// Package h2hdr reassembles an HTTP/2 header block that a peer may split
// across a HEADERS frame followed by zero or more CONTINUATION frames
// (RFC 9113 §6.2, §6.10, §4.3).
package h2hdr

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
	FrameContinuation FrameType = 0x9
)

// FrameFlags is the 8-bit flags field of an HTTP/2 frame.
type FrameFlags uint8

const (
	// FlagEndStream (0x1) marks the last frame the stream will carry.
	FlagEndStream FrameFlags = 0x1
	// FlagEndHeaders (0x4) marks the final fragment of a header block.
	FlagEndHeaders FrameFlags = 0x4
	// FlagPadded (0x8) means the HEADERS payload begins with a Pad Length octet.
	FlagPadded FrameFlags = 0x8
	// FlagPriority (0x20) means the HEADERS payload carries a 5-byte priority block.
	FlagPriority FrameFlags = 0x20
)

// Has reports whether f includes the flag bit g.
func (f FrameFlags) Has(g FrameFlags) bool { return f&g != 0 }

// frameHeader is the fixed 9-byte prefix of every frame.
type frameHeader struct {
	Length   uint32
	Type     FrameType
	Flags    FrameFlags
	StreamID uint32
}

// Sentinel errors. Each names the RFC 9113 condition it maps to so a caller
// can decide between a connection error (GOAWAY) and a stream error.
var (
	// ErrContinuation is a connection error (PROTOCOL_ERROR): a CONTINUATION did
	// not immediately follow a HEADERS/CONTINUATION on the same stream that
	// lacked END_HEADERS (RFC 9113 §6.10).
	ErrContinuation = errors.New("h2hdr: CONTINUATION sequence violated")
	// ErrZeroStreamID is a connection error: HEADERS/CONTINUATION require a
	// non-zero stream identifier (RFC 9113 §6.2, §6.10).
	ErrZeroStreamID = errors.New("h2hdr: HEADERS frame on stream 0")
	// ErrFrameSize is a FRAME_SIZE_ERROR: a padded or priority HEADERS payload
	// is too short to contain its mandatory fields (RFC 9113 §6.2).
	ErrFrameSize = errors.New("h2hdr: frame too short for its flags")
	// ErrPadTooLong is a connection error (PROTOCOL_ERROR): the pad length is at
	// least as large as the remaining payload (RFC 9113 §6.1).
	ErrPadTooLong = errors.New("h2hdr: pad length exceeds payload")
	// ErrHeaderListTooLong guards against a CONTINUATION flood: the accumulated
	// header block exceeded the configured maximum.
	ErrHeaderListTooLong = errors.New("h2hdr: assembled header block exceeds maximum")
)

// Priority carries the stream-dependency block of a HEADERS frame (RFC 9113 §6.3).
// The priority scheme is deprecated by RFC 9113 §5.3, but the fields are still
// transmitted and must be parsed to locate the header block fragment.
type Priority struct {
	StreamDep uint32
	Exclusive bool
	Weight    uint8
}

// HeaderBlock is the fully reassembled result: the concatenated header block
// fragment plus the stream-level facts decoded from the leading HEADERS frame.
type HeaderBlock struct {
	StreamID  uint32
	EndStream bool
	Priority  *Priority
	Fragment  []byte
}

// Framer reads and writes HEADERS and CONTINUATION frames over a byte stream.
type Framer struct {
	r            io.Reader
	w            io.Writer
	maxBlockSize int
	hdr          [9]byte
}

// NewFramer returns a Framer. maxBlockSize caps the reassembled header block;
// a non-positive value disables the cap.
func NewFramer(w io.Writer, r io.Reader, maxBlockSize int) *Framer {
	return &Framer{r: r, w: w, maxBlockSize: maxBlockSize}
}

func writeFrameHeader(buf []byte, h frameHeader) {
	buf[0] = byte(h.Length >> 16)
	buf[1] = byte(h.Length >> 8)
	buf[2] = byte(h.Length)
	buf[3] = byte(h.Type)
	buf[4] = byte(h.Flags)
	binary.BigEndian.PutUint32(buf[5:], h.StreamID&0x7FFFFFFF)
}

func parseFrameHeader(b [9]byte) frameHeader {
	return frameHeader{
		Length:   uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2]),
		Type:     FrameType(b[3]),
		Flags:    FrameFlags(b[4]),
		StreamID: binary.BigEndian.Uint32(b[5:]) & 0x7FFFFFFF,
	}
}

func (fr *Framer) writeFrame(h frameHeader, payload []byte) error {
	h.Length = uint32(len(payload))
	var buf [9]byte
	writeFrameHeader(buf[:], h)
	if _, err := fr.w.Write(buf[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := fr.w.Write(payload)
		return err
	}
	return nil
}

// WriteHeaders writes a HEADERS frame carrying fragment. When endStream is set,
// END_STREAM is flagged; when endHeaders is set, END_HEADERS is flagged so no
// CONTINUATION follows. This writer emits neither padding nor a priority block.
func (fr *Framer) WriteHeaders(streamID uint32, fragment []byte, endStream, endHeaders bool) error {
	var flags FrameFlags
	if endStream {
		flags |= FlagEndStream
	}
	if endHeaders {
		flags |= FlagEndHeaders
	}
	return fr.writeFrame(frameHeader{Type: FrameHeaders, Flags: flags, StreamID: streamID}, fragment)
}

// WriteContinuation writes a CONTINUATION frame carrying fragment. Set
// endHeaders on the final fragment of the block.
func (fr *Framer) WriteContinuation(streamID uint32, fragment []byte, endHeaders bool) error {
	var flags FrameFlags
	if endHeaders {
		flags |= FlagEndHeaders
	}
	return fr.writeFrame(frameHeader{Type: FrameContinuation, Flags: flags, StreamID: streamID}, fragment)
}

func (fr *Framer) readFrame() (frameHeader, []byte, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return frameHeader{}, nil, err
	}
	h := parseFrameHeader(fr.hdr)
	payload := make([]byte, h.Length)
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		return frameHeader{}, nil, err
	}
	return h, payload, nil
}

// stripHeaders removes optional padding and the optional priority block from a
// HEADERS payload, returning the bare header block fragment and the decoded
// priority (nil when absent).
func stripHeaders(h frameHeader, payload []byte) ([]byte, *Priority, error) {
	data := payload
	if h.Flags.Has(FlagPadded) {
		if len(data) < 1 {
			return nil, nil, fmt.Errorf("%w: HEADERS PADDED with empty payload", ErrFrameSize)
		}
		padLen := int(data[0])
		data = data[1:]
		if padLen > len(data) {
			return nil, nil, fmt.Errorf("%w: pad %d > remaining %d", ErrPadTooLong, padLen, len(data))
		}
		data = data[:len(data)-padLen]
	}
	var prio *Priority
	if h.Flags.Has(FlagPriority) {
		if len(data) < 5 {
			return nil, nil, fmt.Errorf("%w: HEADERS PRIORITY needs 5 bytes, have %d", ErrFrameSize, len(data))
		}
		raw := binary.BigEndian.Uint32(data[:4])
		prio = &Priority{
			StreamDep: raw & 0x7FFFFFFF,
			Exclusive: raw&0x80000000 != 0,
			Weight:    data[4],
		}
		data = data[5:]
	}
	return data, prio, nil
}

// ReadHeaderBlock reads one complete header block. It consumes a leading HEADERS
// frame and, while END_HEADERS is unset, the CONTINUATION frames that must
// immediately follow it on the same stream, concatenating every fragment.
// Any other frame type, a stream-id switch, or a stray CONTINUATION between an
// unterminated HEADERS and its END_HEADERS is a connection-level PROTOCOL_ERROR.
func (fr *Framer) ReadHeaderBlock() (HeaderBlock, error) {
	h, payload, err := fr.readFrame()
	if err != nil {
		return HeaderBlock{}, err
	}
	if h.Type != FrameHeaders {
		return HeaderBlock{}, fmt.Errorf("%w: header block must start with HEADERS, got %d", ErrContinuation, h.Type)
	}
	if h.StreamID == 0 {
		return HeaderBlock{}, ErrZeroStreamID
	}
	fragment, prio, err := stripHeaders(h, payload)
	if err != nil {
		return HeaderBlock{}, err
	}

	block := HeaderBlock{
		StreamID:  h.StreamID,
		EndStream: h.Flags.Has(FlagEndStream),
		Priority:  prio,
	}
	acc := append([]byte(nil), fragment...)

	for !h.Flags.Has(FlagEndHeaders) {
		ch, cpayload, err := fr.readFrame()
		if err != nil {
			return HeaderBlock{}, err
		}
		if ch.Type != FrameContinuation {
			return HeaderBlock{}, fmt.Errorf("%w: expected CONTINUATION, got frame type %d", ErrContinuation, ch.Type)
		}
		if ch.StreamID != h.StreamID {
			return HeaderBlock{}, fmt.Errorf("%w: CONTINUATION on stream %d, expected %d", ErrContinuation, ch.StreamID, h.StreamID)
		}
		acc = append(acc, cpayload...)
		if fr.maxBlockSize > 0 && len(acc) > fr.maxBlockSize {
			return HeaderBlock{}, fmt.Errorf("%w: %d > %d", ErrHeaderListTooLong, len(acc), fr.maxBlockSize)
		}
		h = ch
	}

	block.Fragment = acc
	return block, nil
}
```

## The runnable demo

The demo encodes a header block as three opaque fragments, writes them as one HEADERS frame (without END_HEADERS) and two CONTINUATION frames (the last with END_HEADERS), then reads the block back as a single reassembled fragment.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"fmt"
	"log"

	h2hdr "example.com/h2hdr"
)

func main() {
	var pipe bytes.Buffer
	fr := h2hdr.NewFramer(&pipe, &pipe, 1<<20)

	// A large header block is split across one HEADERS and two CONTINUATION
	// frames; the reader stitches it back into one contiguous fragment.
	parts := [][]byte{
		[]byte(":method GET "),
		[]byte(":path /index.html "),
		[]byte(":scheme https"),
	}
	if err := fr.WriteHeaders(1, parts[0], false, false); err != nil {
		log.Fatal(err)
	}
	if err := fr.WriteContinuation(1, parts[1], false); err != nil {
		log.Fatal(err)
	}
	if err := fr.WriteContinuation(1, parts[2], true); err != nil {
		log.Fatal(err)
	}

	block, err := fr.ReadHeaderBlock()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("stream=%d endStream=%v fragments=3 bytes=%d\n",
		block.StreamID, block.EndStream, len(block.Fragment))
	fmt.Printf("reassembled=%q\n", block.Fragment)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
stream=1 endStream=false fragments=3 bytes=43
reassembled=":method GET :path /index.html :scheme https"
```

## Tests

`TestSingleHeadersBlock` confirms a one-frame block (END_HEADERS set on the HEADERS itself) round-trips with no CONTINUATION. `TestHeadersWithContinuations` writes a HEADERS plus two CONTINUATIONs and asserts the reassembled fragment equals the concatenation in order. `TestPaddedPriorityHeaders` hand-builds a payload with both PADDED and PRIORITY set and asserts the bare fragment and the decoded `Priority` are recovered. The sequencing tests assert connection-level rejection: a wrong frame type after an unterminated HEADERS, a CONTINUATION on the wrong stream, a HEADERS on stream 0, an over-long block past `maxBlockSize`, and a PRIORITY payload too short to hold its 5 octets.

Create `header_block_test.go`:

```go
package h2hdr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func newPipe(maxBlock int) (*Framer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return NewFramer(buf, buf, maxBlock), buf
}

func TestSingleHeadersBlock(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe(0)

	want := []byte("\x82\x86\x84") // opaque HPACK-like fragment
	if err := fr.WriteHeaders(1, want, true, true); err != nil {
		t.Fatal(err)
	}
	block, err := fr.ReadHeaderBlock()
	if err != nil {
		t.Fatal(err)
	}
	if block.StreamID != 1 {
		t.Errorf("streamID = %d, want 1", block.StreamID)
	}
	if !block.EndStream {
		t.Error("EndStream = false, want true")
	}
	if !bytes.Equal(block.Fragment, want) {
		t.Errorf("fragment = %x, want %x", block.Fragment, want)
	}
}

func TestHeadersWithContinuations(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe(0)

	a, b, c := []byte("part-one-"), []byte("part-two-"), []byte("part-three")
	if err := fr.WriteHeaders(3, a, false, false); err != nil {
		t.Fatal(err)
	}
	if err := fr.WriteContinuation(3, b, false); err != nil {
		t.Fatal(err)
	}
	if err := fr.WriteContinuation(3, c, true); err != nil {
		t.Fatal(err)
	}

	block, err := fr.ReadHeaderBlock()
	if err != nil {
		t.Fatal(err)
	}
	want := append(append(append([]byte(nil), a...), b...), c...)
	if !bytes.Equal(block.Fragment, want) {
		t.Errorf("reassembled = %q, want %q", block.Fragment, want)
	}
	if block.EndStream {
		t.Error("EndStream = true, want false")
	}
}

func TestPaddedPriorityHeaders(t *testing.T) {
	t.Parallel()
	// Hand-build a HEADERS payload with PADDED and PRIORITY both set:
	//   Pad Length | E+StreamDep(4) | Weight(1) | fragment | padding
	fragment := []byte("blockdata")
	padding := []byte{0, 0, 0}
	payload := []byte{byte(len(padding))}
	var dep [4]byte
	binary.BigEndian.PutUint32(dep[:], 0x80000000|7) // exclusive, depends on stream 7
	payload = append(payload, dep[:]...)
	payload = append(payload, 200) // weight
	payload = append(payload, fragment...)
	payload = append(payload, padding...)

	var buf bytes.Buffer
	var raw [9]byte
	writeFrameHeader(raw[:], frameHeader{
		Length:   uint32(len(payload)),
		Type:     FrameHeaders,
		Flags:    FlagPadded | FlagPriority | FlagEndHeaders,
		StreamID: 5,
	})
	buf.Write(raw[:])
	buf.Write(payload)

	fr := NewFramer(&bytes.Buffer{}, &buf, 0)
	block, err := fr.ReadHeaderBlock()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(block.Fragment, fragment) {
		t.Errorf("fragment = %q, want %q", block.Fragment, fragment)
	}
	if block.Priority == nil {
		t.Fatal("Priority = nil, want decoded block")
	}
	if !block.Priority.Exclusive || block.Priority.StreamDep != 7 || block.Priority.Weight != 200 {
		t.Errorf("Priority = %+v, want {7 true 200}", *block.Priority)
	}
}

func TestContinuationWrongTypeRejected(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe(0)

	// HEADERS without END_HEADERS, then a DATA frame instead of CONTINUATION.
	if err := fr.WriteHeaders(1, []byte("x"), false, false); err != nil {
		t.Fatal(err)
	}
	var raw [9]byte
	writeFrameHeader(raw[:], frameHeader{Type: FrameData, StreamID: 1})
	fr.w.(*bytes.Buffer).Write(raw[:])

	_, err := fr.ReadHeaderBlock()
	if !errors.Is(err, ErrContinuation) {
		t.Errorf("err = %v, want ErrContinuation", err)
	}
}

func TestContinuationWrongStreamRejected(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe(0)

	if err := fr.WriteHeaders(1, []byte("x"), false, false); err != nil {
		t.Fatal(err)
	}
	if err := fr.WriteContinuation(99, []byte("y"), true); err != nil {
		t.Fatal(err)
	}
	_, err := fr.ReadHeaderBlock()
	if !errors.Is(err, ErrContinuation) {
		t.Errorf("err = %v, want ErrContinuation", err)
	}
}

func TestHeadersZeroStreamRejected(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe(0)
	if err := fr.WriteHeaders(0, []byte("x"), false, true); err != nil {
		t.Fatal(err)
	}
	_, err := fr.ReadHeaderBlock()
	if !errors.Is(err, ErrZeroStreamID) {
		t.Errorf("err = %v, want ErrZeroStreamID", err)
	}
}

func TestHeaderListTooLong(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe(8) // cap at 8 bytes

	if err := fr.WriteHeaders(1, []byte("12345"), false, false); err != nil {
		t.Fatal(err)
	}
	if err := fr.WriteContinuation(1, []byte("6789abc"), true); err != nil {
		t.Fatal(err)
	}
	_, err := fr.ReadHeaderBlock()
	if !errors.Is(err, ErrHeaderListTooLong) {
		t.Errorf("err = %v, want ErrHeaderListTooLong", err)
	}
}

func TestShortPriorityRejected(t *testing.T) {
	t.Parallel()
	// PRIORITY flag set but only 3 bytes of payload: FRAME_SIZE_ERROR.
	var buf bytes.Buffer
	var raw [9]byte
	writeFrameHeader(raw[:], frameHeader{
		Length:   3,
		Type:     FrameHeaders,
		Flags:    FlagPriority | FlagEndHeaders,
		StreamID: 1,
	})
	buf.Write(raw[:])
	buf.Write([]byte{0, 0, 0})

	fr := NewFramer(&bytes.Buffer{}, &buf, 0)
	_, err := fr.ReadHeaderBlock()
	if !errors.Is(err, ErrFrameSize) {
		t.Errorf("err = %v, want ErrFrameSize", err)
	}
}
```

## Review

The reassembler is correct when fragments concatenate in order and every break of the CONTINUATION rule surfaces as a connection-scoped error. The three things to get right: strip PADDED and PRIORITY *before* exposing the fragment (and bounds-check both, since a bad pad length or a short priority block are distinct error codes); require the next frame after an unterminated HEADERS to be a same-stream CONTINUATION, rejecting anything else as `ErrContinuation` rather than silently resyncing; and cap the accumulated block so a CONTINUATION flood cannot exhaust memory. Decoding a fragment before END_HEADERS arrives is the classic bug — it corrupts HPACK state for the whole connection — which is why `ReadHeaderBlock` only returns once the sequence is complete. The `-race` run with the multi-CONTINUATION and rejection tests is the proof.

## Resources

- [RFC 9113 §6.2 — HEADERS](https://www.rfc-editor.org/rfc/rfc9113#section-6.2) — the HEADERS payload layout, including the PADDED and PRIORITY fields.
- [RFC 9113 §6.10 — CONTINUATION](https://www.rfc-editor.org/rfc/rfc9113#section-6.10) — the CONTINUATION frame and the rule that it must immediately follow HEADERS/PUSH_PROMISE/CONTINUATION.
- [RFC 9113 §4.3 — Field Section Compression and Decompression](https://www.rfc-editor.org/rfc/rfc9113#section-4.3) — why a header block must be reassembled before HPACK decoding.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-frame-codec.md](01-frame-codec.md) | Next: [03-settings-validation.md](03-settings-validation.md)
