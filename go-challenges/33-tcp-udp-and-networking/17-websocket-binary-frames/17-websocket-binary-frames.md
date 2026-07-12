# 17. WebSocket Binary Frames

WebSocket carries two frame types defined by RFC 6455: text (UTF-8 constrained) and binary (unconstrained). The hard parts are not the frame type flag itself but what happens after it: designing a self-delimiting binary protocol that a receiver can parse unambiguously, encoding floating-point values losslessly in network byte order, and keeping the framing layer decoupled from the transport so it can be tested with a `bytes.Buffer` before touching a real WebSocket connection.

This lesson builds a binary message protocol — a 1-byte type header followed by a 4-byte big-endian length prefix and a payload — and a stateful accumulator server that processes sensor readings and streams running-average responses. The framing code is pure stdlib; the connection to gorilla/websocket is explained in Concepts.

```text
wsbinary/
  go.mod
  frame.go
  sensor.go
  server.go
  frame_test.go
  cmd/demo/main.go
```

## Concepts

### Binary Frames vs Text Frames in WebSocket

RFC 6455 defines four data-carrying opcodes of interest: 0x1 (text), 0x2 (binary), 0x8 (close), 0x9 (ping), and 0xA (pong). Text frames must contain valid UTF-8; binary frames carry raw bytes with no encoding constraint. In Go, gorilla/websocket exposes these as `websocket.TextMessage` and `websocket.BinaryMessage` constants returned by `conn.ReadMessage()`.

Binary frames are the right choice when the payload is inherently binary (sensor values, images, compressed streams), when base64 encoding overhead is unacceptable, or when a custom binary protocol already defines its own wire format. Sending a structured binary message through a text frame by base64-encoding it adds roughly 33% size overhead and requires an extra encoding/decoding step on both ends.

### The Wire Protocol: Type Header and Length Prefix

A raw `[]byte` arriving in a WebSocket binary frame is ambiguous: the receiver does not know what kind of message it is or where it ends. Two primitives solve this:

- **Type byte**: the first byte of every payload identifies the message kind (sensor data, aggregation response, error, and so on). The receiver switches on this byte to dispatch to the right decoder.
- **Length prefix**: a 4-byte big-endian unsigned integer immediately after the type byte encodes the payload length. The receiver reads exactly that many bytes before processing anything.

This gives a 5-byte header: `[type uint8][length uint32]`. The receiver always reads 5 bytes first, then reads exactly `length` payload bytes. The protocol is self-delimiting: no sentinel bytes, no delimiter scanning.

The critical implementation detail is `io.ReadFull`. A single call to `r.Read(buf)` may legally return fewer bytes than requested even on a healthy connection, because TCP delivers data in segments. Always use `io.ReadFull(r, buf)` to block until exactly `len(buf)` bytes arrive.

### Big-Endian Encoding and Lossless Float64 Serialization

Network protocols use big-endian (most significant byte first), sometimes called network byte order. `encoding/binary.BigEndian.PutUint32` and `PutUint64` write big-endian integers directly into a byte slice; `Uint32` and `Uint64` read them back. These are the only correct tools for portable cross-platform binary protocols — never cast a `*float64` to a `*[8]byte` with unsafe, because the Go specification does not guarantee struct layout.

Float64 has no portable text representation that is lossless for all values. `strconv.FormatFloat(v, 'g', -1, 64)` round-trips common values but still falls short of full IEEE 754 precision in edge cases. The correct approach: `math.Float64bits(v)` extracts the IEEE 754 bit pattern as a `uint64`, which is then stored with `PutUint64`. On the other side, `math.Float64frombits(bits)` reconstructs the exact `float64`. The round-trip is exact: `math.Float64frombits(math.Float64bits(x)) == x` for all finite values, infinities, and NaN.

### Transport Abstraction: io.ReadWriter

The framing functions in this lesson operate on `io.Reader` and `io.Writer`, not on a `*websocket.Conn`. This is the correct separation of concerns: the binary protocol does not care whether the transport is a `bytes.Buffer`, a `net.Pipe()` end, or a real WebSocket binary-message writer.

In production with gorilla/websocket, each WebSocket message arrives via `conn.ReadMessage()` returning a `[]byte`, and is sent via `conn.WriteMessage(websocket.BinaryMessage, []byte)`. The framing functions here would be applied to those byte slices — the entire binary message is one WebSocket frame, and `ReadFrame`/`WriteFrame` parse the application-level envelope inside it.

Testing against `bytes.Buffer` means the protocol logic is exercised without a network stack, without a TLS handshake, and without goroutines that complicate failure modes. The `net.Pipe()` in `cmd/demo` shows the same code working over a real in-process connection.

### Ping/Pong and Close Handshake in Production

WebSocket control frames are not part of the application binary protocol; they are handled by the WebSocket library. In gorilla/websocket:

- `conn.SetPongHandler(func(string) error { ... })`: called each time a pong is received. The standard pattern is to extend the read deadline: `conn.SetReadDeadline(time.Now().Add(60 * time.Second))`.
- `conn.SetReadDeadline(time.Now().Add(deadline))`: if no pong arrives within the deadline, the next read returns a timeout error, which the server interprets as a dead connection.
- Close handshake: the initiator calls `conn.WriteControl(websocket.CloseMessage, msg, deadline)` with a close message built by `websocket.FormatCloseMessage(code, reason)`. The receiver's `ReadMessage` returns a `*websocket.CloseError`; the receiver echoes the close message and closes the underlying TCP connection. gorilla/websocket handles the echo automatically if you call `conn.Close()`.

These concerns are orthogonal to binary framing. The application server sends and receives binary application frames; the ping/pong and close frames are interleaved by the library transparently.

## Exercises

This is a library plus a demo binary: the library is verified with `go test`, the demo with `go run ./cmd/demo`.

### Exercise 1: Binary Frame Reader and Writer

Create `frame.go`:

```go
package wsbinary

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Message type constants used in the 1-byte type header.
const (
	TypeSensorData  byte = 0x01
	TypeAggResponse byte = 0x02
	TypeError       byte = 0xFF
)

// Sentinel errors for validation and length checks.
var (
	ErrPayloadTooLong = errors.New("wsbinary: payload exceeds maximum length")
)

// maxPayload is the upper bound on payload size (1 MiB).
const maxPayload = 1 << 20

// WriteFrame writes one framed binary message to w.
// Format: 1-byte type | 4-byte big-endian payload length | payload bytes.
func WriteFrame(w io.Writer, msgType byte, payload []byte) error {
	if len(payload) > maxPayload {
		return fmt.Errorf("%w: %d bytes", ErrPayloadTooLong, len(payload))
	}
	var hdr [5]byte
	hdr[0] = msgType
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := w.Write(payload)
		return err
	}
	return nil
}

// ReadFrame reads one framed binary message from r.
// It returns io.EOF when r is exhausted at a clean frame boundary.
func ReadFrame(r io.Reader) (msgType byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	msgType = hdr[0]
	plen := binary.BigEndian.Uint32(hdr[1:5])
	if plen > maxPayload {
		return 0, nil, fmt.Errorf("%w: %d bytes", ErrPayloadTooLong, plen)
	}
	payload = make([]byte, plen)
	if plen > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return msgType, payload, nil
}
```

The 5-byte header is written and read atomically to avoid partial-frame states. `io.ReadFull` blocks until exactly `n` bytes are available; `r.Read` alone may return any count from 1 to n.

### Exercise 2: Sensor Data and Aggregation Encoding

Create `sensor.go`:

```go
package wsbinary

import (
	"encoding/binary"
	"fmt"
	"math"
)

// SensorReading is one sensor measurement.
type SensorReading struct {
	SensorID  uint32
	Value     float64
	Timestamp int64 // Unix nanoseconds
}

const sensorSize = 20 // 4 (ID) + 8 (value bits) + 8 (timestamp)

// EncodeSensor serializes r into a 20-byte big-endian payload.
// The float64 is stored as its IEEE 754 bit pattern via math.Float64bits,
// ensuring a lossless round-trip.
func EncodeSensor(r SensorReading) []byte {
	buf := make([]byte, sensorSize)
	binary.BigEndian.PutUint32(buf[0:4], r.SensorID)
	binary.BigEndian.PutUint64(buf[4:12], math.Float64bits(r.Value))
	binary.BigEndian.PutUint64(buf[12:20], uint64(r.Timestamp))
	return buf
}

// DecodeSensor deserializes a SensorReading from payload.
// payload must be at least 20 bytes.
func DecodeSensor(payload []byte) (SensorReading, error) {
	if len(payload) < sensorSize {
		return SensorReading{}, fmt.Errorf(
			"wsbinary: sensor payload too short: %d < %d",
			len(payload), sensorSize,
		)
	}
	return SensorReading{
		SensorID:  binary.BigEndian.Uint32(payload[0:4]),
		Value:     math.Float64frombits(binary.BigEndian.Uint64(payload[4:12])),
		Timestamp: int64(binary.BigEndian.Uint64(payload[12:20])),
	}, nil
}

// AggResponse holds a running count and mean.
type AggResponse struct {
	Count uint32
	Mean  float64
}

const aggSize = 12 // 4 (count) + 8 (mean bits)

// EncodeAgg serializes a into a 12-byte big-endian payload.
func EncodeAgg(a AggResponse) []byte {
	buf := make([]byte, aggSize)
	binary.BigEndian.PutUint32(buf[0:4], a.Count)
	binary.BigEndian.PutUint64(buf[4:12], math.Float64bits(a.Mean))
	return buf
}

// DecodeAgg deserializes an AggResponse from payload.
// payload must be at least 12 bytes.
func DecodeAgg(payload []byte) (AggResponse, error) {
	if len(payload) < aggSize {
		return AggResponse{}, fmt.Errorf(
			"wsbinary: agg payload too short: %d < %d",
			len(payload), aggSize,
		)
	}
	return AggResponse{
		Count: binary.BigEndian.Uint32(payload[0:4]),
		Mean:  math.Float64frombits(binary.BigEndian.Uint64(payload[4:12])),
	}, nil
}
```

### Exercise 3: Stateful Server and Test Suite

Create `server.go`:

```go
package wsbinary

import (
	"fmt"
	"io"
)

// Accumulator tracks a running count and sum for computing a mean.
type Accumulator struct {
	count uint32
	sum   float64
}

// Add records value and returns the updated aggregation result.
func (a *Accumulator) Add(value float64) AggResponse {
	a.count++
	a.sum += value
	return AggResponse{Count: a.count, Mean: a.sum / float64(a.count)}
}

// Count returns the number of values recorded.
func (a *Accumulator) Count() uint32 { return a.count }

// Mean returns the running mean, or 0 if no values have been recorded yet.
func (a *Accumulator) Mean() float64 {
	if a.count == 0 {
		return 0
	}
	return a.sum / float64(a.count)
}

// Serve reads binary frames from rw, accumulates sensor values, and writes
// aggregation response frames back. It returns nil when the reader is
// exhausted (io.EOF at a clean frame boundary) and a non-nil error on any
// other read or write failure.
func Serve(rw io.ReadWriter) error {
	var acc Accumulator
	for {
		msgType, payload, err := ReadFrame(rw)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		switch msgType {
		case TypeSensorData:
			reading, decErr := DecodeSensor(payload)
			if decErr != nil {
				if werr := WriteFrame(rw, TypeError, []byte(decErr.Error())); werr != nil {
					return werr
				}
				continue
			}
			agg := acc.Add(reading.Value)
			if werr := WriteFrame(rw, TypeAggResponse, EncodeAgg(agg)); werr != nil {
				return werr
			}
		default:
			msg := fmt.Sprintf("unknown type 0x%02x", msgType)
			if werr := WriteFrame(rw, TypeError, []byte(msg)); werr != nil {
				return werr
			}
		}
	}
}
```

Create `frame_test.go`:

```go
package wsbinary

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"
)

// rwPair combines a reader and a writer into an io.ReadWriter for testing Serve
// with two separate buffers: input frames fed to Serve and output frames it writes.
type rwPair struct {
	r *bytes.Buffer
	w *bytes.Buffer
}

func (p rwPair) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p rwPair) Write(b []byte) (int, error) { return p.w.Write(b) }

func TestWriteReadFrameRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		msgType byte
		payload []byte
	}{
		{"sensor type", TypeSensorData, []byte{1, 2, 3, 4, 5}},
		{"agg type", TypeAggResponse, []byte{0x00, 0x00, 0x00, 0x01}},
		{"error type", TypeError, []byte("something went wrong")},
		{"empty payload", TypeSensorData, []byte{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tc.msgType, tc.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			gotType, gotPayload, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if gotType != tc.msgType {
				t.Errorf("type = 0x%02x, want 0x%02x", gotType, tc.msgType)
			}
			if !bytes.Equal(gotPayload, tc.payload) {
				t.Errorf("payload = %v, want %v", gotPayload, tc.payload)
			}
		})
	}
}

func TestWriteFramePayloadTooLong(t *testing.T) {
	t.Parallel()

	big := make([]byte, maxPayload+1)
	var buf bytes.Buffer
	err := WriteFrame(&buf, TypeSensorData, big)
	if !errors.Is(err, ErrPayloadTooLong) {
		t.Fatalf("err = %v, want ErrPayloadTooLong", err)
	}
}

func TestSensorRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   SensorReading
	}{
		{"positive", SensorReading{SensorID: 42, Value: math.Pi, Timestamp: 1_700_000_000}},
		{"negative value", SensorReading{SensorID: 0, Value: -273.15, Timestamp: -1}},
		{"zero value", SensorReading{}},
		{"max sensorID", SensorReading{SensorID: ^uint32(0), Value: 1.0, Timestamp: 0}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeSensor(EncodeSensor(tc.in))
			if err != nil {
				t.Fatalf("DecodeSensor: %v", err)
			}
			if got != tc.in {
				t.Errorf("decoded = %+v, want %+v", got, tc.in)
			}
		})
	}
}

func TestSensorDecodeTooShort(t *testing.T) {
	t.Parallel()

	if _, err := DecodeSensor(make([]byte, 3)); err == nil {
		t.Fatal("want error for short payload, got nil")
	}
}

func TestAggRoundTrip(t *testing.T) {
	t.Parallel()

	in := AggResponse{Count: 100, Mean: 3.14159}
	got, err := DecodeAgg(EncodeAgg(in))
	if err != nil {
		t.Fatalf("DecodeAgg: %v", err)
	}
	if got != in {
		t.Errorf("decoded = %+v, want %+v", got, in)
	}
}

func TestAccumulatorRunningMean(t *testing.T) {
	t.Parallel()

	var acc Accumulator
	for i, v := range []float64{10.0, 20.0, 30.0} {
		agg := acc.Add(v)
		if agg.Count != uint32(i+1) {
			t.Errorf("step %d: count = %d, want %d", i, agg.Count, i+1)
		}
	}
	if acc.Mean() != 20.0 {
		t.Errorf("mean = %v, want 20.0", acc.Mean())
	}
}

func TestAccumulatorZeroState(t *testing.T) {
	t.Parallel()

	var acc Accumulator
	if acc.Mean() != 0 {
		t.Errorf("mean of empty accumulator = %v, want 0", acc.Mean())
	}
	if acc.Count() != 0 {
		t.Errorf("count of empty accumulator = %d, want 0", acc.Count())
	}
}

func TestServeAggregatesReadings(t *testing.T) {
	t.Parallel()

	var in, out bytes.Buffer
	readings := []SensorReading{
		{SensorID: 1, Value: 10.0, Timestamp: 1000},
		{SensorID: 1, Value: 20.0, Timestamp: 2000},
		{SensorID: 1, Value: 30.0, Timestamp: 3000},
	}
	for _, r := range readings {
		if err := WriteFrame(&in, TypeSensorData, EncodeSensor(r)); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}

	if err := Serve(rwPair{&in, &out}); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	wantMean := []float64{10.0, 15.0, 20.0}
	for i := range readings {
		msgType, payload, err := ReadFrame(&out)
		if err != nil {
			t.Fatalf("ReadFrame %d: %v", i, err)
		}
		if msgType != TypeAggResponse {
			t.Errorf("frame %d: type = 0x%02x, want TypeAggResponse (0x%02x)", i, msgType, TypeAggResponse)
		}
		agg, err := DecodeAgg(payload)
		if err != nil {
			t.Fatalf("DecodeAgg %d: %v", i, err)
		}
		if agg.Count != uint32(i+1) {
			t.Errorf("frame %d: count = %d, want %d", i, agg.Count, i+1)
		}
		if agg.Mean != wantMean[i] {
			t.Errorf("frame %d: mean = %v, want %v", i, agg.Mean, wantMean[i])
		}
	}
}

func TestServeUnknownTypeReturnsError(t *testing.T) {
	t.Parallel()

	var in, out bytes.Buffer
	if err := WriteFrame(&in, 0x42, []byte("?")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	if err := Serve(rwPair{&in, &out}); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	msgType, _, err := ReadFrame(&out)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if msgType != TypeError {
		t.Errorf("type = 0x%02x, want TypeError (0x%02x)", msgType, TypeError)
	}
}

// ExampleWriteFrame shows the total wire size of a framed sensor reading.
// Header: 5 bytes (1 type + 4 length). Sensor payload: 20 bytes. Total: 25.
func ExampleWriteFrame() {
	var buf bytes.Buffer
	r := SensorReading{SensorID: 7, Value: 36.6, Timestamp: 0}
	_ = WriteFrame(&buf, TypeSensorData, EncodeSensor(r))
	fmt.Printf("frame size: %d bytes\n", buf.Len())
	// Output: frame size: 25 bytes
}
```

Your turn: add `TestServeShortSensorPayload` that writes a `TypeSensorData` frame with a 3-byte payload (too short for a valid `SensorReading`) and asserts that the response frame has type `TypeError`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"net"
	"time"

	"example.com/wsbinary"
)

func main() {
	server, client := net.Pipe()
	defer client.Close()

	go func() {
		defer server.Close()
		_ = wsbinary.Serve(server)
	}()

	readings := []wsbinary.SensorReading{
		{SensorID: 1, Value: 10.0, Timestamp: time.Now().UnixNano()},
		{SensorID: 1, Value: 20.0, Timestamp: time.Now().UnixNano()},
		{SensorID: 1, Value: 30.0, Timestamp: time.Now().UnixNano()},
	}

	for i, r := range readings {
		if err := wsbinary.WriteFrame(client, wsbinary.TypeSensorData, wsbinary.EncodeSensor(r)); err != nil {
			fmt.Printf("send error: %v\n", err)
			return
		}
		_, payload, err := wsbinary.ReadFrame(client)
		if err != nil {
			fmt.Printf("recv error: %v\n", err)
			return
		}
		agg, err := wsbinary.DecodeAgg(payload)
		if err != nil {
			fmt.Printf("decode error: %v\n", err)
			return
		}
		fmt.Printf("reading %d: count=%d  mean=%.2f\n", i+1, agg.Count, agg.Mean)
	}
}
```

Run the demo:

```bash
go run ./cmd/demo
```

Expected output (timestamps vary):

```text
reading 1: count=1  mean=10.00
reading 2: count=2  mean=15.00
reading 3: count=3  mean=20.00
```

## Common Mistakes

### Using r.Read Instead of io.ReadFull

Wrong: `n, err := r.Read(hdr[:])`. A healthy TCP connection may legally return 3 of the 5 requested bytes. The remaining 2 bytes appear in the next `Read`, so the type byte gets silently misinterpreted as a length byte.

Fix: `_, err := io.ReadFull(r, hdr[:])`. `io.ReadFull` loops until all bytes are available or an error occurs.

### Encoding Float64 as Text

Wrong: storing sensor value as `[]byte(strconv.FormatFloat(v, 'g', -1, 64))` in a binary frame. The decimal representation is variable-length, does not fit in a fixed-size slot, and loses precision for values with more than 15 significant digits.

Fix: `binary.BigEndian.PutUint64(buf, math.Float64bits(v))`. The round-trip `math.Float64frombits(math.Float64bits(x)) == x` is exact for all representable values.

### Ignoring the Message Type Byte

Wrong: treating every incoming binary frame as sensor data regardless of the type byte. If an error or response frame arrives out of order, `DecodeSensor` interprets the wrong bytes and returns garbage.

Fix: switch on `msgType` before decoding. The type byte is the receiver's only signal about which decoder to invoke.

### Writing a Partial Frame on Error

Wrong: writing the header successfully, then returning early if encoding the payload fails. The peer reads the 5-byte header and then blocks waiting for `length` payload bytes that never arrive, deadlocking the connection.

Fix: encode the full payload into a local buffer before calling `WriteFrame`. If encoding fails, write a `TypeError` response frame instead — the header and payload are written together in `WriteFrame`.

## Verification

From `~/go-exercises/wsbinary`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four commands must produce no output and exit 0. Add `TestServeShortSensorPayload` (from the "your turn" prompt above) and confirm it passes before calling the suite green.

## Summary

- WebSocket binary frames (opcode 0x2) carry unconstrained bytes; text frames require valid UTF-8 and add base64 overhead when the data is not text.
- A self-delimiting protocol needs a type byte and a length prefix; `io.ReadFull` is required to read a fixed number of bytes reliably over any stream.
- `encoding/binary.BigEndian` encodes integers in portable network byte order; `math.Float64bits`/`math.Float64frombits` give a lossless IEEE 754 round-trip for float64.
- Framing code that operates on `io.ReadWriter` is testable with `bytes.Buffer` without any network setup; the same code works over `net.Pipe()` and over gorilla/websocket connections.
- Ping/pong liveness detection and the WebSocket close handshake are configured on `*websocket.Conn` through `SetPongHandler` and `WriteControl`; they are orthogonal to the application binary protocol.

## What's Next

Next: [Connection Draining](../18-connection-draining/18-connection-draining.md).

## Resources

- [RFC 6455: The WebSocket Protocol](https://datatracker.ietf.org/doc/html/rfc6455) — sections 5.2 (frame format) and 5.5 (control frames define ping/pong/close)
- [encoding/binary](https://pkg.go.dev/encoding/binary) — BigEndian.PutUint32, Uint32, PutUint64, Uint64
- [math.Float64bits and math.Float64frombits](https://pkg.go.dev/math#Float64bits) — lossless float64 serialization
- [io.ReadFull](https://pkg.go.dev/io#ReadFull) — reads exactly len(buf) bytes; returns io.ErrUnexpectedEOF on partial reads
- [gorilla/websocket](https://pkg.go.dev/github.com/gorilla/websocket) — BinaryMessage, SetPongHandler, WriteControl, FormatCloseMessage
