# Exercise 4: ENABLE_PUSH Negotiation

Whether a server may push at all is negotiated, not assumed. The client controls it with the `SETTINGS_ENABLE_PUSH` parameter inside a SETTINGS frame, and this module parses that frame, validates the value, and tracks the resulting push-enabled state — refusing a push rather than committing a PROTOCOL_ERROR when the client has turned push off. This is exactly the mechanism today's browsers use to disable push: they send `ENABLE_PUSH=0` in their first SETTINGS frame.

This module is fully self-contained: its own `go mod init`, all code inline, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
enable-push/
  go.mod
  settings.go          SettingID, ErrorCode, ConnError, Setting, ParseSettings
  negotiator.go        Negotiator, ApplySettings, PushEnabled, MayPush, ErrPushRefused
  negotiator_test.go   initial state, disable/re-enable, invalid value, stream 0, framing
  cmd/demo/main.go     negotiate ENABLE_PUSH off then on, then two error cases
```

- Files: `settings.go`, `negotiator.go`, `negotiator_test.go`, `cmd/demo/main.go`.
- Implement: `ParseSettings([]byte) ([]Setting, error)` and a `Negotiator` with `ApplySettings(streamID uint32, payload []byte) error`, `PushEnabled() bool`, and `MayPush(resource string) error`.
- Test: the initial state enables push; `ENABLE_PUSH=0` disables and `=1` re-enables; a value above 1 is a PROTOCOL_ERROR; SETTINGS off stream 0 is a PROTOCOL_ERROR; a non-multiple-of-6 payload is a FRAME_SIZE_ERROR; the last value in a frame wins; unrelated settings are ignored.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p enable-push/cmd/demo && cd enable-push
go mod init example.com/enable-push
go mod edit -go=1.26
```

### Parsing a SETTINGS frame payload

A SETTINGS frame payload (RFC 9113 §6.5) is a flat sequence of 6-byte entries: a 2-byte identifier followed by a 4-byte value, both big-endian. `ParseSettings` walks the payload in 6-byte strides and returns the decoded entries. Its one hard validation is structural: if the payload length is not a multiple of 6 the frame is malformed, and the spec mandates a FRAME_SIZE_ERROR connection error — not a stream reset, a connection teardown. Modeling that as a `*ConnError` carrying the error code lets the caller send the right GOAWAY; the error type is what distinguishes a connection error that kills every stream from a stream error that resets one.

The error machinery is deliberately small but typed. `ErrorCode` is the RFC 9113 §7 error-code enumeration with a `String` method so diagnostics read `PROTOCOL_ERROR` instead of `1`, and `ConnError` pairs a code with a human reason. The two codes this module produces are `PROTOCOL_ERROR` (0x01) for semantic violations and `FRAME_SIZE_ERROR` (0x06) for the malformed-length case.

Create `settings.go`:

```go
package enablepush

import (
	"encoding/binary"
	"fmt"
)

// SettingID identifies an HTTP/2 SETTINGS parameter (RFC 9113 §6.5.2).
type SettingID uint16

const (
	// SettingEnablePush is SETTINGS_ENABLE_PUSH (0x02).
	SettingEnablePush SettingID = 0x02
)

// ErrorCode is an HTTP/2 error code (RFC 9113 §7).
type ErrorCode uint32

const (
	// ErrCodeProtocol is PROTOCOL_ERROR (0x01).
	ErrCodeProtocol ErrorCode = 0x01
	// ErrCodeFrameSize is FRAME_SIZE_ERROR (0x06).
	ErrCodeFrameSize ErrorCode = 0x06
)

func (c ErrorCode) String() string {
	switch c {
	case ErrCodeProtocol:
		return "PROTOCOL_ERROR"
	case ErrCodeFrameSize:
		return "FRAME_SIZE_ERROR"
	default:
		return fmt.Sprintf("ERROR_0x%02x", uint32(c))
	}
}

// ConnError is a connection error (RFC 9113 §5.4.1): the connection must be
// closed with a GOAWAY carrying Code.
type ConnError struct {
	Code   ErrorCode
	Reason string
}

func (e *ConnError) Error() string {
	return fmt.Sprintf("connection error %s: %s", e.Code, e.Reason)
}

// Setting is one decoded SETTINGS parameter.
type Setting struct {
	ID    SettingID
	Value uint32
}

// ParseSettings decodes a SETTINGS frame payload into its parameters. Each
// parameter is exactly 6 bytes: a 2-byte identifier and a 4-byte value
// (RFC 9113 §6.5.1). A payload whose length is not a multiple of 6 is a
// FRAME_SIZE_ERROR connection error.
func ParseSettings(payload []byte) ([]Setting, error) {
	if len(payload)%6 != 0 {
		return nil, &ConnError{
			Code:   ErrCodeFrameSize,
			Reason: fmt.Sprintf("SETTINGS payload length %d is not a multiple of 6", len(payload)),
		}
	}
	out := make([]Setting, 0, len(payload)/6)
	for off := 0; off < len(payload); off += 6 {
		out = append(out, Setting{
			ID:    SettingID(binary.BigEndian.Uint16(payload[off : off+2])),
			Value: binary.BigEndian.Uint32(payload[off+2 : off+6]),
		})
	}
	return out, nil
}
```

### Tracking the negotiated state

The `Negotiator` holds one bit of connection state: whether push is currently allowed. It starts `true` because the initial value of `SETTINGS_ENABLE_PUSH` is 1 — push is permitted until the client says otherwise — and a fresh `Negotiator` must reflect that. `ApplySettings` is the entry point for every received SETTINGS frame, and it enforces two rules before touching the state. First, SETTINGS must arrive on stream 0; a SETTINGS frame on any other stream is a PROTOCOL_ERROR, because settings are connection-scoped and a non-zero stream is a framing violation. Second, each `SETTINGS_ENABLE_PUSH` value must be 0 or 1; the parameter is a boolean encoded in 32 bits, and any other value is a PROTOCOL_ERROR. Only after both checks pass does the negotiator update its bit, and it applies every `ENABLE_PUSH` entry in the frame in order so the last one wins — a frame is allowed to carry the same identifier more than once.

The crucial design choice is what happens on a rejected frame: the state does not change. If `ApplySettings` returns a `ConnError`, the previously negotiated value still stands, so a malformed SETTINGS frame cannot silently flip push on or off — it forces the caller to close the connection, which is the only correct response to a connection error. `MayPush` is the read side the push path calls before sending a PUSH_PROMISE: when push is disabled it returns an `*ErrPushRefused` so the caller skips the push entirely rather than sending it and committing the very PROTOCOL_ERROR the negotiation exists to prevent. Refusing a push you are not allowed to send is correct behavior; sending it and apologizing afterward is a connection error.

Create `negotiator.go`:

```go
package enablepush

import "fmt"

// Negotiator tracks the push-enabled state negotiated by the client's
// SETTINGS_ENABLE_PUSH parameter. The initial value of SETTINGS_ENABLE_PUSH is
// 1, so a fresh Negotiator reports push as enabled until told otherwise
// (RFC 9113 §6.5.2).
type Negotiator struct {
	pushEnabled bool
}

// NewNegotiator returns a Negotiator in the protocol's initial state: push
// enabled.
func NewNegotiator() *Negotiator {
	return &Negotiator{pushEnabled: true}
}

// PushEnabled reports whether the client currently permits server push.
func (n *Negotiator) PushEnabled() bool {
	return n.pushEnabled
}

// ApplySettings applies one received SETTINGS frame. streamID is the frame's
// stream identifier; a SETTINGS frame MUST be sent on stream 0, so any other
// value is a PROTOCOL_ERROR (RFC 9113 §6.5). Each SETTINGS_ENABLE_PUSH value
// other than 0 or 1 is a PROTOCOL_ERROR. Only the last ENABLE_PUSH value in
// the frame takes effect.
func (n *Negotiator) ApplySettings(streamID uint32, payload []byte) error {
	if streamID != 0 {
		return &ConnError{
			Code:   ErrCodeProtocol,
			Reason: fmt.Sprintf("SETTINGS frame on stream %d, must be stream 0", streamID),
		}
	}
	settings, err := ParseSettings(payload)
	if err != nil {
		return err
	}
	for _, s := range settings {
		if s.ID != SettingEnablePush {
			continue
		}
		if s.Value > 1 {
			return &ConnError{
				Code:   ErrCodeProtocol,
				Reason: fmt.Sprintf("SETTINGS_ENABLE_PUSH value %d, must be 0 or 1", s.Value),
			}
		}
		n.pushEnabled = s.Value == 1
	}
	return nil
}

// ErrPushRefused is returned by MayPush when the client has disabled push.
type ErrPushRefused struct {
	Resource string
}

func (e *ErrPushRefused) Error() string {
	return fmt.Sprintf("push of %s refused: client set SETTINGS_ENABLE_PUSH=0", e.Resource)
}

// MayPush reports whether the server is permitted to send a PUSH_PROMISE for
// resource. When push is disabled it returns an *ErrPushRefused so the caller
// skips the push instead of committing a PROTOCOL_ERROR by sending it anyway.
func (n *Negotiator) MayPush(resource string) error {
	if !n.pushEnabled {
		return &ErrPushRefused{Resource: resource}
	}
	return nil
}
```

### The runnable demo

The demo builds single-parameter SETTINGS payloads with a small helper, then walks the negotiation: the client disables push, a push attempt is refused, the client re-enables push, and finally two error cases — an out-of-range value and a SETTINGS frame on the wrong stream — both surface as PROTOCOL_ERROR connection errors.

Create `cmd/demo/main.go`:

```go
package main

import (
	"encoding/binary"
	"errors"
	"fmt"

	push "example.com/enable-push"
)

// settingsPayload builds a SETTINGS frame payload carrying one
// SETTINGS_ENABLE_PUSH parameter with the given value.
func settingsPayload(value uint32) []byte {
	p := make([]byte, 6)
	binary.BigEndian.PutUint16(p[0:2], uint16(push.SettingEnablePush))
	binary.BigEndian.PutUint32(p[2:6], value)
	return p
}

func main() {
	n := push.NewNegotiator()
	fmt.Printf("initial push enabled: %v\n", n.PushEnabled())

	// Client disables push mid-connection.
	if err := n.ApplySettings(0, settingsPayload(0)); err != nil {
		fmt.Println("apply:", err)
	}
	fmt.Printf("after ENABLE_PUSH=0: %v\n", n.PushEnabled())

	// A push attempt is now refused rather than sent.
	if err := n.MayPush("/style.css"); err != nil {
		fmt.Println("refused:", err)
	}

	// Client re-enables push.
	if err := n.ApplySettings(0, settingsPayload(1)); err != nil {
		fmt.Println("apply:", err)
	}
	fmt.Printf("after ENABLE_PUSH=1: %v\n", n.PushEnabled())
	fmt.Printf("may push /style.css: %v\n", n.MayPush("/style.css") == nil)

	// An out-of-range value is a connection error.
	err := n.ApplySettings(0, settingsPayload(2))
	var ce *push.ConnError
	if errors.As(err, &ce) {
		fmt.Printf("invalid value -> %s\n", ce.Code)
	}

	// A SETTINGS frame on a non-zero stream is also a connection error.
	err = n.ApplySettings(3, settingsPayload(0))
	if errors.As(err, &ce) {
		fmt.Printf("wrong stream -> %s\n", ce.Code)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial push enabled: true
after ENABLE_PUSH=0: false
refused: push of /style.css refused: client set SETTINGS_ENABLE_PUSH=0
after ENABLE_PUSH=1: true
may push /style.css: true
invalid value -> PROTOCOL_ERROR
wrong stream -> PROTOCOL_ERROR
```

The negotiation tracks the client's intent exactly: push starts allowed, goes off when `ENABLE_PUSH=0` arrives, comes back when `ENABLE_PUSH=1` does, and both malformed frames are rejected as connection errors without disturbing the negotiated state.

### Tests

The tests cover the initial-enabled state, a disable-then-re-enable cycle, the out-of-range value producing a PROTOCOL_ERROR, SETTINGS off stream 0 producing a PROTOCOL_ERROR, a non-multiple-of-6 payload producing a FRAME_SIZE_ERROR, the last-value-wins rule when one frame carries two `ENABLE_PUSH` entries, and an unrelated setting leaving push untouched. `TestInvalidValueIsProtocolError` also asserts the state is unchanged after a rejected frame.

Create `negotiator_test.go`:

```go
package enablepush

import (
	"encoding/binary"
	"errors"
	"testing"
)

func enablePushPayload(value uint32) []byte {
	p := make([]byte, 6)
	binary.BigEndian.PutUint16(p[0:2], uint16(SettingEnablePush))
	binary.BigEndian.PutUint32(p[2:6], value)
	return p
}

func TestInitialStateEnablesPush(t *testing.T) {
	t.Parallel()
	n := NewNegotiator()
	if !n.PushEnabled() {
		t.Fatal("a fresh Negotiator must report push enabled (initial value 1)")
	}
	if err := n.MayPush("/x.css"); err != nil {
		t.Fatalf("MayPush = %v, want nil", err)
	}
}

func TestDisableThenReenable(t *testing.T) {
	t.Parallel()
	n := NewNegotiator()

	if err := n.ApplySettings(0, enablePushPayload(0)); err != nil {
		t.Fatalf("ApplySettings(0): %v", err)
	}
	if n.PushEnabled() {
		t.Fatal("push must be disabled after ENABLE_PUSH=0")
	}
	var refused *ErrPushRefused
	if err := n.MayPush("/style.css"); !errors.As(err, &refused) {
		t.Fatalf("MayPush err = %v, want *ErrPushRefused", err)
	}

	if err := n.ApplySettings(0, enablePushPayload(1)); err != nil {
		t.Fatalf("ApplySettings(1): %v", err)
	}
	if !n.PushEnabled() {
		t.Fatal("push must be enabled after ENABLE_PUSH=1")
	}
}

func TestInvalidValueIsProtocolError(t *testing.T) {
	t.Parallel()
	n := NewNegotiator()
	err := n.ApplySettings(0, enablePushPayload(2))
	var ce *ConnError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConnError", err)
	}
	if ce.Code != ErrCodeProtocol {
		t.Fatalf("code = %s, want PROTOCOL_ERROR", ce.Code)
	}
	if !n.PushEnabled() {
		t.Fatal("a rejected SETTINGS frame must not change the negotiated state")
	}
}

func TestSettingsMustBeStreamZero(t *testing.T) {
	t.Parallel()
	n := NewNegotiator()
	err := n.ApplySettings(1, enablePushPayload(0))
	var ce *ConnError
	if !errors.As(err, &ce) || ce.Code != ErrCodeProtocol {
		t.Fatalf("err = %v, want PROTOCOL_ERROR ConnError", err)
	}
}

func TestNonMultipleOfSixIsFrameSizeError(t *testing.T) {
	t.Parallel()
	_, err := ParseSettings([]byte{0x00, 0x02, 0x00, 0x00, 0x00})
	var ce *ConnError
	if !errors.As(err, &ce) || ce.Code != ErrCodeFrameSize {
		t.Fatalf("err = %v, want FRAME_SIZE_ERROR ConnError", err)
	}
}

func TestLastValueWins(t *testing.T) {
	t.Parallel()
	n := NewNegotiator()
	// One SETTINGS frame carrying ENABLE_PUSH=1 then ENABLE_PUSH=0.
	payload := append(enablePushPayload(1), enablePushPayload(0)...)
	if err := n.ApplySettings(0, payload); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if n.PushEnabled() {
		t.Fatal("the last ENABLE_PUSH value in the frame must win (0)")
	}
}

func TestUnknownSettingsIgnored(t *testing.T) {
	t.Parallel()
	n := NewNegotiator()
	// SETTINGS_HEADER_TABLE_SIZE (0x01) with some value: must not affect push.
	p := make([]byte, 6)
	binary.BigEndian.PutUint16(p[0:2], 0x01)
	binary.BigEndian.PutUint32(p[2:6], 4096)
	if err := n.ApplySettings(0, p); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if !n.PushEnabled() {
		t.Fatal("an unrelated setting must not disable push")
	}
}
```

## Review

The negotiator is correct when a rejected SETTINGS frame leaves the push-enabled bit exactly as it was — `TestInvalidValueIsProtocolError` is the guard for that, and it would catch an implementation that updated the state before validating. Confirm the initial state is enabled (initial value 1), that SETTINGS is required on stream 0, and that the multiple-of-6 length check fires before any per-entry parsing. The two mistakes this design rules out are caching the decision (a later SETTINGS frame can change it, so the push path must call `MayPush` every time) and sending a forbidden push and treating the resulting error as recoverable (it is a connection error; refuse the push up front instead). Both `ParseSettings` and `ApplySettings` return typed `*ConnError` values so the caller can map each to the correct GOAWAY code.

## Resources

- [RFC 9113 §6.5 — SETTINGS](https://httpwg.org/specs/rfc9113.html#SETTINGS) — the frame format, the stream-0 rule, and the multiple-of-6 length requirement.
- [RFC 9113 §6.5.2 — Defined Settings](https://httpwg.org/specs/rfc9113.html#SettingValues) — SETTINGS_ENABLE_PUSH, its initial value, and the 0-or-1 constraint.
- [Chrome: removing HTTP/2 push](https://developer.chrome.com/blog/removing-push/) — the engineering post-mortem; browsers now send ENABLE_PUSH=0, the exact negotiation this module handles.
- [`encoding/binary`](https://pkg.go.dev/encoding/binary) — `BigEndian.Uint16`/`Uint32`, used to decode the 6-byte settings entries.

---

Back to [03-push-tracker.md](03-push-tracker.md) | Next: [05-rst-stream-cancel.md](05-rst-stream-cancel.md)
