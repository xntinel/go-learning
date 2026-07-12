# Exercise 3: SETTINGS Validation and the ACK Handshake

A SETTINGS frame is more than a list of identifier/value pairs: each parameter has a value range the RFC mandates, an out-of-range value maps to a *specific* error code, and every non-ACK SETTINGS frame must be acknowledged. This module parses and serializes SETTINGS, validates each parameter, and tracks the SETTINGS/ACK handshake.

This module is fully self-contained: it begins with its own `go mod init`, defines its own frame machinery, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
h2settings/
  go.mod
  settings.go           SettingID, Setting, SettingsFrame, SettingsError, Framer, Params
  settings_test.go      round-trip, ACK handshake, apply, per-parameter range rejects
  cmd/demo/main.go      announce settings, apply on the peer, ACK, refuse a bad value
```

- Files: `settings.go`, `settings_test.go`, `cmd/demo/main.go`.
- Implement: `Framer` with `WriteSettings`/`WriteAck`/`ReadSettings` and `PendingAcks`, plus `Params` with `DefaultParams` and `Apply`.
- Test: a SETTINGS frame round-trips; the ACK handshake clears the pending count; `Apply` updates effective params and ignores unknown ids; each out-of-range parameter is rejected with the right error code.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/01-frame-parsing/03-settings-validation/cmd/demo && cd go-solutions/44-capstone-http2-implementation/01-frame-parsing/03-settings-validation
go mod edit -go=1.26
```

## Validation carries a code, and the ACK is a state machine

The wire format is trivial — each parameter is a 2-octet id and a 4-octet value, the frame length must be a multiple of 6, and the stream id must be 0 — but the validation is where conformance lives. RFC 9113 §6.5.2 assigns each parameter a range *and* a distinct failure code: SETTINGS_ENABLE_PUSH outside {0, 1} is a PROTOCOL_ERROR, SETTINGS_INITIAL_WINDOW_SIZE above 2^31-1 is a FLOW_CONTROL_ERROR, and SETTINGS_MAX_FRAME_SIZE outside [16384, 16777215] is a PROTOCOL_ERROR. Returning a bare "invalid" loses the information the peer needs to build the matching GOAWAY, so `validateSetting` returns a `SettingsError` that carries both the wrapped sentinel (for `errors.Is`) and the `ErrCode` (for the wire). Unknown identifiers are not errors at all — RFC 9113 requires them to be ignored, which is what lets the protocol add parameters without breaking older peers.

Validation runs on both paths. `WriteSettings` refuses to transmit a frame that would force the peer into a connection error, catching the bug locally rather than on the wire; `ReadSettings` re-validates received parameters, because a conformant sender is not guaranteed and the receiver is the one that must react. A SETTINGS ACK is the degenerate case: the ACK flag set, an empty payload (a non-empty ACK is a FRAME_SIZE_ERROR), and no parameters.

The handshake is a small state machine. Each `WriteSettings` increments a `pending` counter; the peer must reply with an ACK, and `ReadSettings` decrements `pending` when it reads one. `PendingAcks` therefore reports how many of an endpoint's advertised parameter sets have not yet taken effect on the peer — the signal an implementation uses before, say, relying on a newly advertised window size. `Params` holds the *effective* configuration, seeded with the protocol defaults from RFC 9113 §6.5.2, and `Apply` folds a received frame's parameters into it, ignoring unknown ids exactly as the parse path does.

Create `settings.go`:

```go
// Package h2settings parses, validates, and acknowledges HTTP/2 SETTINGS
// frames (RFC 9113 §6.5). It enforces the per-parameter value ranges the RFC
// mandates and tracks the SETTINGS/ACK handshake.
package h2settings

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// SettingID is the 16-bit identifier of a SETTINGS parameter (RFC 9113 §6.5.2).
type SettingID uint16

const (
	SettingHeaderTableSize      SettingID = 0x1
	SettingEnablePush           SettingID = 0x2
	SettingMaxConcurrentStreams SettingID = 0x3
	SettingInitialWindowSize    SettingID = 0x4
	SettingMaxFrameSize         SettingID = 0x5
	SettingMaxHeaderListSize    SettingID = 0x6
)

// ErrCode is the HTTP/2 error code a SETTINGS violation maps to (RFC 9113 §7).
type ErrCode uint32

const (
	ErrCodeProtocol    ErrCode = 0x1
	ErrCodeFlowControl ErrCode = 0x3
	ErrCodeFrameSize   ErrCode = 0x6
)

// Bounds from RFC 9113 §6.5.2.
const (
	maxWindowSize  = 1<<31 - 1 // SETTINGS_INITIAL_WINDOW_SIZE ceiling
	minMaxFrameSz  = 1 << 14   // 16384, the floor for SETTINGS_MAX_FRAME_SIZE
	maxMaxFrameSz  = 1<<24 - 1 // 16777215, the ceiling for SETTINGS_MAX_FRAME_SIZE
	settingByteLen = 6         // every parameter is a 2-byte id + 4-byte value
)

// FrameType and flag values used by this package.
const (
	frameSettings = 0x4
	flagAck       = 0x1
)

// Sentinel errors. SettingsError additionally carries the RFC error code so a
// caller can build the matching GOAWAY frame.
var (
	ErrStreamID       = errors.New("h2settings: SETTINGS must use stream 0")
	ErrLength         = errors.New("h2settings: SETTINGS length is not a multiple of 6")
	ErrAckNonEmpty    = errors.New("h2settings: SETTINGS ACK must have empty payload")
	ErrEnablePush     = errors.New("h2settings: SETTINGS_ENABLE_PUSH must be 0 or 1")
	ErrWindowTooLarge = errors.New("h2settings: SETTINGS_INITIAL_WINDOW_SIZE exceeds 2^31-1")
	ErrMaxFrameSize   = errors.New("h2settings: SETTINGS_MAX_FRAME_SIZE out of [16384, 16777215]")
)

// SettingsError wraps one of the sentinels with the RFC 9113 error code that an
// endpoint must report when it rejects the frame.
type SettingsError struct {
	Err  error
	Code ErrCode
}

func (e *SettingsError) Error() string { return e.Err.Error() }
func (e *SettingsError) Unwrap() error { return e.Err }

func settingsErr(err error, code ErrCode) error { return &SettingsError{Err: err, Code: code} }

// Setting is one SETTINGS parameter on the wire.
type Setting struct {
	ID    SettingID
	Value uint32
}

// SettingsFrame is a decoded SETTINGS frame. An ACK carries no parameters.
type SettingsFrame struct {
	Ack      bool
	Settings []Setting
}

// validateSetting checks a single parameter against its RFC 9113 §6.5.2 range.
// Unknown identifiers are accepted (and later ignored) for forward compatibility.
func validateSetting(s Setting) error {
	switch s.ID {
	case SettingEnablePush:
		if s.Value > 1 {
			return settingsErr(fmt.Errorf("%w: got %d", ErrEnablePush, s.Value), ErrCodeProtocol)
		}
	case SettingInitialWindowSize:
		if s.Value > maxWindowSize {
			return settingsErr(fmt.Errorf("%w: got %d", ErrWindowTooLarge, s.Value), ErrCodeFlowControl)
		}
	case SettingMaxFrameSize:
		if s.Value < minMaxFrameSz || s.Value > maxMaxFrameSz {
			return settingsErr(fmt.Errorf("%w: got %d", ErrMaxFrameSize, s.Value), ErrCodeProtocol)
		}
	}
	return nil
}

// Framer reads and writes SETTINGS frames and tracks the ACK handshake.
type Framer struct {
	r       io.Reader
	w       io.Writer
	pending int // SETTINGS frames written but not yet acknowledged by the peer
	hdr     [9]byte
}

// NewFramer returns a Framer over w and r.
func NewFramer(w io.Writer, r io.Reader) *Framer { return &Framer{r: r, w: w} }

// PendingAcks reports how many locally sent SETTINGS frames await a peer ACK.
func (fr *Framer) PendingAcks() int { return fr.pending }

func writeHeader(buf []byte, length uint32, typ, flags byte, streamID uint32) {
	buf[0] = byte(length >> 16)
	buf[1] = byte(length >> 8)
	buf[2] = byte(length)
	buf[3] = typ
	buf[4] = flags
	binary.BigEndian.PutUint32(buf[5:], streamID&0x7FFFFFFF)
}

// WriteSettings validates each parameter, then writes a SETTINGS frame and
// records that one ACK is now outstanding. It refuses to transmit a frame that
// would force the peer into a connection error.
func (fr *Framer) WriteSettings(settings []Setting) error {
	for _, s := range settings {
		if err := validateSetting(s); err != nil {
			return err
		}
	}
	payload := make([]byte, settingByteLen*len(settings))
	for i, s := range settings {
		binary.BigEndian.PutUint16(payload[i*settingByteLen:], uint16(s.ID))
		binary.BigEndian.PutUint32(payload[i*settingByteLen+2:], s.Value)
	}
	var h [9]byte
	writeHeader(h[:], uint32(len(payload)), frameSettings, 0, 0)
	if _, err := fr.w.Write(h[:]); err != nil {
		return err
	}
	if _, err := fr.w.Write(payload); err != nil {
		return err
	}
	fr.pending++
	return nil
}

// WriteAck writes a zero-length SETTINGS frame with the ACK flag set, the
// required reply to a peer's non-ACK SETTINGS frame.
func (fr *Framer) WriteAck() error {
	var h [9]byte
	writeHeader(h[:], 0, frameSettings, flagAck, 0)
	_, err := fr.w.Write(h[:])
	return err
}

// ReadSettings reads and validates the next SETTINGS frame. An ACK clears one
// outstanding pending ACK. A non-ACK frame returns its parsed, range-checked
// parameters; the caller is expected to apply them and reply with WriteAck.
func (fr *Framer) ReadSettings() (*SettingsFrame, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return nil, err
	}
	length := uint32(fr.hdr[0])<<16 | uint32(fr.hdr[1])<<8 | uint32(fr.hdr[2])
	flags := fr.hdr[4]
	streamID := binary.BigEndian.Uint32(fr.hdr[5:]) & 0x7FFFFFFF

	if streamID != 0 {
		return nil, settingsErr(fmt.Errorf("%w: got %d", ErrStreamID, streamID), ErrCodeProtocol)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		return nil, err
	}

	if flags&flagAck != 0 {
		if length != 0 {
			return nil, settingsErr(fmt.Errorf("%w: length %d", ErrAckNonEmpty, length), ErrCodeFrameSize)
		}
		if fr.pending > 0 {
			fr.pending--
		}
		return &SettingsFrame{Ack: true}, nil
	}
	if length%settingByteLen != 0 {
		return nil, settingsErr(fmt.Errorf("%w: length %d", ErrLength, length), ErrCodeFrameSize)
	}

	settings := make([]Setting, 0, length/settingByteLen)
	for i := 0; i < len(payload); i += settingByteLen {
		s := Setting{
			ID:    SettingID(binary.BigEndian.Uint16(payload[i:])),
			Value: binary.BigEndian.Uint32(payload[i+2:]),
		}
		if err := validateSetting(s); err != nil {
			return nil, err
		}
		settings = append(settings, s)
	}
	return &SettingsFrame{Settings: settings}, nil
}

// Params is a peer's effective configuration, seeded with the RFC 9113 §6.5.2
// initial values and updated by applying received SETTINGS frames.
type Params struct {
	HeaderTableSize      uint32
	EnablePush           bool
	MaxConcurrentStreams uint32 // ^uint32(0) means "no limit advertised"
	InitialWindowSize    uint32
	MaxFrameSize         uint32
	MaxHeaderListSize    uint32 // ^uint32(0) means "no limit advertised"
}

// DefaultParams returns the protocol-default settings every connection starts
// from before any SETTINGS frame is exchanged.
func DefaultParams() Params {
	return Params{
		HeaderTableSize:      4096,
		EnablePush:           true,
		MaxConcurrentStreams: ^uint32(0),
		InitialWindowSize:    65535,
		MaxFrameSize:         minMaxFrameSz,
		MaxHeaderListSize:    ^uint32(0),
	}
}

// Apply folds a non-ACK SETTINGS frame into p. Unknown identifiers are ignored,
// as RFC 9113 §6.5.2 requires. Validation has already happened at parse time.
func (p *Params) Apply(sf *SettingsFrame) {
	for _, s := range sf.Settings {
		switch s.ID {
		case SettingHeaderTableSize:
			p.HeaderTableSize = s.Value
		case SettingEnablePush:
			p.EnablePush = s.Value == 1
		case SettingMaxConcurrentStreams:
			p.MaxConcurrentStreams = s.Value
		case SettingInitialWindowSize:
			p.InitialWindowSize = s.Value
		case SettingMaxFrameSize:
			p.MaxFrameSize = s.Value
		case SettingMaxHeaderListSize:
			p.MaxHeaderListSize = s.Value
		}
	}
}
```

## The runnable demo

The demo announces three settings, reads them on the (same-buffer) peer and folds them into a `Params` seeded with the defaults, replies with an ACK that clears the pending count, and finally shows `WriteSettings` refusing a bad SETTINGS_MAX_FRAME_SIZE before it ever reaches the wire.

Create `cmd/demo/main.go`:

```go
package main

import (
	"bytes"
	"errors"
	"fmt"
	"log"

	h2s "example.com/h2settings"
)

func main() {
	var pipe bytes.Buffer
	fr := h2s.NewFramer(&pipe, &pipe)

	// An endpoint announces its settings, the peer applies and acknowledges.
	if err := fr.WriteSettings([]h2s.Setting{
		{ID: h2s.SettingMaxConcurrentStreams, Value: 128},
		{ID: h2s.SettingInitialWindowSize, Value: 1 << 20},
		{ID: h2s.SettingEnablePush, Value: 0},
	}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("pending acks after send: %d\n", fr.PendingAcks())

	peer := h2s.DefaultParams()
	sf, err := fr.ReadSettings()
	if err != nil {
		log.Fatal(err)
	}
	peer.Apply(sf)
	fmt.Printf("applied: push=%v window=%d streams=%d\n",
		peer.EnablePush, peer.InitialWindowSize, peer.MaxConcurrentStreams)

	if err := fr.WriteAck(); err != nil {
		log.Fatal(err)
	}
	ack, err := fr.ReadSettings()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("ack received: %v, pending now: %d\n", ack.Ack, fr.PendingAcks())

	// A malformed parameter is refused before it ever reaches the wire.
	err = fr.WriteSettings([]h2s.Setting{{ID: h2s.SettingMaxFrameSize, Value: 100}})
	var se *h2s.SettingsError
	if errors.As(err, &se) {
		fmt.Printf("rejected bad MAX_FRAME_SIZE with code 0x%x\n", uint32(se.Code))
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```text
pending acks after send: 1
applied: push=false window=1048576 streams=128
ack received: true, pending now: 0
rejected bad MAX_FRAME_SIZE with code 0x1
```

## Tests

`TestSettingsRoundTrip` writes and reads a multi-parameter frame. `TestAckHandshake` walks the full cycle — write SETTINGS (pending = 1), read it on the peer, write an ACK, read the ACK (pending = 0). `TestApplyUpdatesParams` checks the defaults, applies a frame, and asserts an unknown id is ignored. The remaining tests assert each violation: ENABLE_PUSH = 2 rejected as PROTOCOL_ERROR, INITIAL_WINDOW_SIZE = 2^31 rejected as FLOW_CONTROL_ERROR, a hand-encoded under-floor MAX_FRAME_SIZE rejected on read, a length that is not a multiple of 6, a non-zero stream id, and an ACK that carries a payload — each verified both with `errors.Is` on the sentinel and `errors.As` on the `SettingsError` code.

Create `settings_test.go`:

```go
package h2settings

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func newPipe() (*Framer, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return NewFramer(buf, buf), buf
}

func TestSettingsRoundTrip(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe()

	want := []Setting{
		{SettingHeaderTableSize, 8192},
		{SettingMaxConcurrentStreams, 250},
		{SettingMaxFrameSize, 1 << 20},
	}
	if err := fr.WriteSettings(want); err != nil {
		t.Fatal(err)
	}
	sf, err := fr.ReadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if sf.Ack {
		t.Error("Ack = true, want false")
	}
	if len(sf.Settings) != len(want) {
		t.Fatalf("got %d settings, want %d", len(sf.Settings), len(want))
	}
	for i, s := range sf.Settings {
		if s != want[i] {
			t.Errorf("settings[%d] = %+v, want %+v", i, s, want[i])
		}
	}
}

func TestAckHandshake(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe()

	if err := fr.WriteSettings([]Setting{{SettingEnablePush, 0}}); err != nil {
		t.Fatal(err)
	}
	if fr.PendingAcks() != 1 {
		t.Fatalf("pending = %d, want 1", fr.PendingAcks())
	}
	if _, err := fr.ReadSettings(); err != nil { // peer reads the SETTINGS
		t.Fatal(err)
	}
	if err := fr.WriteAck(); err != nil {
		t.Fatal(err)
	}
	sf, err := fr.ReadSettings() // original endpoint reads the ACK
	if err != nil {
		t.Fatal(err)
	}
	if !sf.Ack {
		t.Error("Ack = false, want true")
	}
	if fr.PendingAcks() != 0 {
		t.Errorf("pending = %d, want 0", fr.PendingAcks())
	}
}

func TestApplyUpdatesParams(t *testing.T) {
	t.Parallel()
	p := DefaultParams()
	if !p.EnablePush || p.InitialWindowSize != 65535 || p.MaxFrameSize != 1<<14 {
		t.Fatalf("defaults wrong: %+v", p)
	}
	p.Apply(&SettingsFrame{Settings: []Setting{
		{SettingEnablePush, 0},
		{SettingInitialWindowSize, 131072},
		{SettingID(0xABCD), 999}, // unknown id must be ignored
	}})
	if p.EnablePush {
		t.Error("EnablePush = true, want false")
	}
	if p.InitialWindowSize != 131072 {
		t.Errorf("InitialWindowSize = %d, want 131072", p.InitialWindowSize)
	}
}

func TestRejectEnablePush(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe()
	err := fr.WriteSettings([]Setting{{SettingEnablePush, 2}})
	if !errors.Is(err, ErrEnablePush) {
		t.Fatalf("err = %v, want ErrEnablePush", err)
	}
	var se *SettingsError
	if !errors.As(err, &se) || se.Code != ErrCodeProtocol {
		t.Errorf("code = %v, want ErrCodeProtocol", err)
	}
}

func TestRejectWindowTooLarge(t *testing.T) {
	t.Parallel()
	fr, _ := newPipe()
	err := fr.WriteSettings([]Setting{{SettingInitialWindowSize, 1 << 31}})
	if !errors.Is(err, ErrWindowTooLarge) {
		t.Fatalf("err = %v, want ErrWindowTooLarge", err)
	}
	var se *SettingsError
	if !errors.As(err, &se) || se.Code != ErrCodeFlowControl {
		t.Errorf("code = %v, want ErrCodeFlowControl", err)
	}
}

func TestRejectMaxFrameSizeOnRead(t *testing.T) {
	t.Parallel()
	// Hand-encode a SETTINGS frame with MAX_FRAME_SIZE below the 16384 floor so
	// the read path's validation rejects it (the writer would refuse to send it).
	var payload [6]byte
	binary.BigEndian.PutUint16(payload[0:], uint16(SettingMaxFrameSize))
	binary.BigEndian.PutUint32(payload[2:], 1024)

	var buf bytes.Buffer
	var h [9]byte
	writeHeader(h[:], 6, frameSettings, 0, 0)
	buf.Write(h[:])
	buf.Write(payload[:])

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadSettings()
	if !errors.Is(err, ErrMaxFrameSize) {
		t.Fatalf("err = %v, want ErrMaxFrameSize", err)
	}
}

func TestRejectBadLength(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var h [9]byte
	writeHeader(h[:], 5, frameSettings, 0, 0) // 5 is not a multiple of 6
	buf.Write(h[:])
	buf.Write([]byte{0, 0, 0, 0, 0})

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadSettings()
	if !errors.Is(err, ErrLength) {
		t.Fatalf("err = %v, want ErrLength", err)
	}
}

func TestRejectNonZeroStream(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var h [9]byte
	writeHeader(h[:], 0, frameSettings, 0, 7)
	buf.Write(h[:])

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadSettings()
	if !errors.Is(err, ErrStreamID) {
		t.Fatalf("err = %v, want ErrStreamID", err)
	}
}

func TestRejectAckWithPayload(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	var h [9]byte
	writeHeader(h[:], 6, frameSettings, flagAck, 0)
	buf.Write(h[:])
	buf.Write(make([]byte, 6))

	fr := NewFramer(&bytes.Buffer{}, &buf)
	_, err := fr.ReadSettings()
	if !errors.Is(err, ErrAckNonEmpty) {
		t.Fatalf("err = %v, want ErrAckNonEmpty", err)
	}
}
```

## Review

The validator is correct when each out-of-range parameter is rejected with the exact code RFC 9113 §6.5.2 assigns, not a generic error — the peer needs that code to build its GOAWAY. Validate on both the write and read paths: writing catches your own bug locally, reading defends against a non-conformant sender. Keep unknown identifiers silently ignored, or the protocol cannot evolve. The handshake's one rule is that an ACK carries no parameters and an empty payload, and that the pending counter only ever moves to zero — never below — so a stray ACK does not underflow it. The `-race` run with the handshake and per-parameter rejection tests is the proof.

## Resources

- [RFC 9113 §6.5 — SETTINGS](https://www.rfc-editor.org/rfc/rfc9113#section-6.5) — the frame format, the multiple-of-6 length rule, and the ACK handshake.
- [RFC 9113 §6.5.2 — Defined Settings](https://www.rfc-editor.org/rfc/rfc9113#section-6.5.2) — each parameter, its default, its value range, and the error code for a violation.
- [RFC 9113 §7 — Error Codes](https://www.rfc-editor.org/rfc/rfc9113#section-7) — PROTOCOL_ERROR, FLOW_CONTROL_ERROR, and FRAME_SIZE_ERROR.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-headers-continuation.md](02-headers-continuation.md) | Next: [04-frame-size-guard.md](04-frame-size-guard.md)
