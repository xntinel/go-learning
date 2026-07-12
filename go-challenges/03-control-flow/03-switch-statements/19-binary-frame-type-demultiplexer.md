# Exercise 19: Demultiplex Binary Protocol Frames by Type and Length

**Nivel: Intermedio** — validacion rapida (un test corto).

A message queue consumer reading a custom binary protocol sees one thing
first: a single type byte, then whatever payload follows it. Everything
downstream — which struct to decode into, how big the payload is allowed to
be — depends on getting that one byte's dispatch right. This module builds
the demultiplexer as an expression switch on the type byte, with each case
enforcing its own frame's min/max payload size before decoding, and a
`default` that never guesses a shape for a type it wasn't taught about. It
is self-contained: its own `go mod init`, code, demo, and test.

## What you'll build

```text
framedemux/                 independent module: example.com/binary-frame-type-demultiplexer
  go.mod                     go 1.24
  framedemux.go               package framedemux; PingFrame, DataFrame, AckFrame, CloseFrame; Demux(raw) (any, error)
  cmd/demo/main.go            runnable demo over one frame of each type plus three malformed frames
  framedemux_test.go          table over every valid frame, three size violations, an unknown type, and a short frame
```

- Implement: `Demux(raw []byte) (any, error)` — an expression switch on `raw[0]`, one case per frame type, each validating its payload's length before returning the decoded struct.
- Test: a table covering all four frame types (including a zero-length `CloseFrame` reason), three size-boundary violations, an unrecognized type byte, and an empty slice.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why the length check lives inside each case, not before the switch

It would be tempting to validate `len(raw)` once, up front, and then switch.
That doesn't work here because "valid length" is a property of the *type*,
not of the frame in general: a `PingFrame` must have zero payload bytes, a
`DataFrame` needs 1 to 1024, an `AckFrame` needs exactly 4 (a big-endian
`uint32`), and a `CloseFrame` allows 0 to 256. There is no single length rule
to check before you know which case you're in, so each case owns its own
bounds check immediately after entering — that is also why every case
returns its own `fmt.Errorf` wrapping `ErrInvalidFrame` with the specific
numbers involved, instead of a single generic message for every shape
mismatch.

The only check that happens before the switch is `len(raw) < 1`: with zero
bytes there is no type byte to switch on at all, so that failure is
categorically different (`ErrShortFrame`) from "I read the type byte and
didn't like the payload that followed" (`ErrInvalidFrame`). The `default`
case is the safety net for a byte this version of the protocol has never
seen — a future frame type from a newer producer, a corrupted stream — and
it fails with a named sentinel rather than attempting to decode payload
bytes as if they matched a type they don't.

Create `framedemux.go`:

```go
// Package framedemux decodes binary protocol frames from a queue consumer's
// byte stream. Each frame is a one-byte type tag followed by a
// variable-length payload; an expression switch on the type byte routes to
// the unmarshaler for that frame kind and enforces its min/max payload size.
package framedemux

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Frame type tags, as they appear on the wire.
const (
	TypePing  byte = 0x01
	TypeData  byte = 0x02
	TypeAck   byte = 0x03
	TypeClose byte = 0x04
)

// ErrShortFrame marks a frame with no type byte at all.
var ErrShortFrame = errors.New("framedemux: frame shorter than 1 byte")

// ErrInvalidFrame marks a recognized frame type whose payload size is
// outside that type's valid range.
var ErrInvalidFrame = errors.New("framedemux: invalid frame payload")

// ErrUnknownFrameType marks a type byte this demultiplexer has no
// unmarshaler for.
var ErrUnknownFrameType = errors.New("framedemux: unknown frame type")

// PingFrame is a liveness check; it never carries a payload.
type PingFrame struct{}

// DataFrame carries an application payload between 1 and 1024 bytes.
type DataFrame struct{ Payload []byte }

// AckFrame acknowledges a sequence number; its payload is always exactly
// 4 bytes, a big-endian uint32.
type AckFrame struct{ SeqNum uint32 }

// CloseFrame ends a session with an optional human-readable reason, capped
// at 256 bytes so a misbehaving peer can't force an unbounded allocation.
type CloseFrame struct{ Reason string }

// Demux reads the type byte from raw and returns the decoded frame as one
// of PingFrame, DataFrame, AckFrame, or CloseFrame. It never guesses a
// shape for an unknown type or a size that doesn't fit the type's contract.
func Demux(raw []byte) (any, error) {
	if len(raw) < 1 {
		return nil, ErrShortFrame
	}
	typ := raw[0]
	payload := raw[1:]

	switch typ {
	case TypePing:
		if len(payload) != 0 {
			return nil, fmt.Errorf("%w: ping carries %d byte payload, want 0", ErrInvalidFrame, len(payload))
		}
		return PingFrame{}, nil
	case TypeData:
		if len(payload) < 1 || len(payload) > 1024 {
			return nil, fmt.Errorf("%w: data payload %d bytes, want 1-1024", ErrInvalidFrame, len(payload))
		}
		return DataFrame{Payload: payload}, nil
	case TypeAck:
		if len(payload) != 4 {
			return nil, fmt.Errorf("%w: ack payload %d bytes, want 4", ErrInvalidFrame, len(payload))
		}
		return AckFrame{SeqNum: binary.BigEndian.Uint32(payload)}, nil
	case TypeClose:
		if len(payload) > 256 {
			return nil, fmt.Errorf("%w: close reason %d bytes, want <= 256", ErrInvalidFrame, len(payload))
		}
		return CloseFrame{Reason: string(payload)}, nil
	default:
		return nil, fmt.Errorf("%w: 0x%02x", ErrUnknownFrameType, typ)
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	framedemux "example.com/binary-frame-type-demultiplexer"
)

func main() {
	frames := [][]byte{
		{0x01},
		{0x02, 'h', 'i'},
		{0x03, 0x00, 0x00, 0x00, 0x2a},
		{0x04, 'b', 'y', 'e'},
		{0x02},
		{0x09, 0xff},
		{},
	}

	for _, raw := range frames {
		decoded, err := framedemux.Demux(raw)
		if err != nil {
			fmt.Printf("% x -> error: %v\n", raw, err)
			continue
		}
		fmt.Printf("% x -> %#v\n", raw, decoded)
	}
}
```

Run `go run ./cmd/demo`, expected output:

```
01 -> framedemux.PingFrame{}
02 68 69 -> framedemux.DataFrame{Payload:[]uint8{0x68, 0x69}}
03 00 00 00 2a -> framedemux.AckFrame{SeqNum:0x2a}
04 62 79 65 -> framedemux.CloseFrame{Reason:"bye"}
02 -> error: framedemux: invalid frame payload: data payload 0 bytes, want 1-1024
09 ff -> error: framedemux: unknown frame type: 0x09
 -> error: framedemux: frame shorter than 1 byte
```

### Tests

`TestDemux` runs a table over one valid frame of each type — including a
`CloseFrame` with an empty reason, which is legal — three payload-size
violations (a ping that isn't empty, a data frame with no payload, an ack
with the wrong length), an unrecognized type byte, and a fully empty input.

Create `framedemux_test.go`:

```go
package framedemux

import (
	"errors"
	"reflect"
	"testing"
)

func TestDemux(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     []byte
		want    any
		wantErr error
	}{
		{"ping", []byte{0x01}, PingFrame{}, nil},
		{"data", []byte{0x02, 'h', 'i'}, DataFrame{Payload: []byte("hi")}, nil},
		{"ack", []byte{0x03, 0x00, 0x00, 0x00, 0x2a}, AckFrame{SeqNum: 42}, nil},
		{"close with reason", []byte{0x04, 'b', 'y', 'e'}, CloseFrame{Reason: "bye"}, nil},
		{"close empty reason", []byte{0x04}, CloseFrame{Reason: ""}, nil},
		{"ping with payload rejected", []byte{0x01, 0x00}, nil, ErrInvalidFrame},
		{"data empty payload rejected", []byte{0x02}, nil, ErrInvalidFrame},
		{"ack wrong length rejected", []byte{0x03, 0x00}, nil, ErrInvalidFrame},
		{"unknown type", []byte{0x09, 0xff}, nil, ErrUnknownFrameType},
		{"empty frame", []byte{}, nil, ErrShortFrame},
	}

	for _, tc := range tests {
		got, err := Demux(tc.raw)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s: Demux(% x) error = %v, want errors.Is match for %v", tc.name, tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: Demux(% x) unexpected error: %v", tc.name, tc.raw, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Demux(% x) = %#v, want %#v", tc.name, tc.raw, got, tc.want)
		}
	}
}
```

Verify with:

```bash
go test -count=1 ./...
```

## Review

The demultiplexer is correct when each frame type's own size contract is
checked inside its own case (never a single blanket length check before the
switch), when a too-short input to even read a type byte fails differently
from a recognized type with a bad payload, and when an unrecognized type
byte never falls through to decoding payload bytes as if they matched a
shape they don't. Carry this forward: whenever a dispatch's validation rule
differs per case, the validation belongs inside the case, not hoisted above
the switch where it would have to average out over rules that don't agree.

## Resources

- [Go Specification: Switch statements](https://go.dev/ref/spec#Switch_statements) — the expression switch form.
- [encoding/binary](https://pkg.go.dev/encoding/binary) — fixed-width integer decoding used for the ack frame's sequence number.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [18-cache-eviction-policy-router.md](18-cache-eviction-policy-router.md) | Next: [20-queue-priority-level-inheritance.md](20-queue-priority-level-inheritance.md)
