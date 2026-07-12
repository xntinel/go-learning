# Exercise 5: RST_STREAM Cancellation

A client refuses an unwanted push by sending RST_STREAM on the promised stream, and the server must stop sending on that stream and free its slot the instant the frame lands. This module encodes and decodes RST_STREAM frames and reconciles a per-connection registry of promised streams against incoming cancellations, so a pushed stream the client already has in cache costs one tiny frame instead of a whole response body.

This module is fully self-contained: its own `go mod init`, all code inline, its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
rst-stream-cancel/
  go.mod
  frame.go             FrameType, ErrorCode, EncodeRSTStream, DecodeRSTStream, FrameError
  registry.go          Registry, Promise, HandleRST, Complete, Active, Cancelled
  rststream_test.go    round-trip, stream-0 reject, length reject, cancel frees slot, idempotence
  cmd/demo/main.go     promise three pushes, cancel two via RST_STREAM, complete one
```

- Files: `frame.go`, `registry.go`, `rststream_test.go`, `cmd/demo/main.go`.
- Implement: `EncodeRSTStream(streamID uint32, code ErrorCode) []byte`, `DecodeRSTStream([]byte) (uint32, ErrorCode, error)`, and a `Registry` with `Promise`, `HandleRST`, `Complete`, `Active`, `Cancelled`.
- Test: encode/decode round-trips; RST_STREAM on stream 0 is a PROTOCOL_ERROR and a wrong-length payload is a FRAME_SIZE_ERROR; a cancel frees exactly one slot; an unknown stream and a double cancel do not change the count; concurrent cancels are race-free.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### A fixed-size frame with two framing rules

RST_STREAM (type `0x03`, RFC 9113 §6.4) is the simplest HTTP/2 frame: a 9-byte header followed by exactly a 4-byte error code, so its total size is always 13 bytes. `EncodeRSTStream` writes that fixed layout — length 4, type `0x03`, no flags, the masked 31-bit stream ID, then the 32-bit error code. `DecodeRSTStream` enforces the two rules the spec attaches to the frame, and the distinction between them is the whole point. A payload length other than 4 is a FRAME_SIZE_ERROR: the frame is structurally malformed. A stream ID of 0 is a PROTOCOL_ERROR: the frame is well-formed but semantically illegal, because RST_STREAM acts on a specific stream and stream 0 is the connection-control stream that has no state to reset. Both are connection errors, but they carry different codes, so the decoder returns a `*FrameError` naming which one it is.

The error codes themselves come from RFC 9113 §7. The two a client uses to refuse a push are `CANCEL` (`0x08`), meaning the stream is no longer needed, and `REFUSED_STREAM` (`0x07`), meaning the stream was never processed. The registry treats either the same way — a cancellation is a cancellation — but the decoder preserves the exact code so a server can log why a push was refused.

Create `frame.go`:

```go
package rststream

import (
	"encoding/binary"
	"fmt"
)

// FrameType identifies an HTTP/2 frame type (RFC 9113 §6).
type FrameType uint8

// FrameRSTStream is the frame type byte for RST_STREAM (RFC 9113 §6.4).
const FrameRSTStream FrameType = 0x03

// ErrorCode is an HTTP/2 error code (RFC 9113 §7).
type ErrorCode uint32

const (
	// ErrCodeNo is NO_ERROR (0x00).
	ErrCodeNo ErrorCode = 0x00
	// ErrCodeProtocol is PROTOCOL_ERROR (0x01).
	ErrCodeProtocol ErrorCode = 0x01
	// ErrCodeRefusedStream is REFUSED_STREAM (0x07).
	ErrCodeRefusedStream ErrorCode = 0x07
	// ErrCodeCancel is CANCEL (0x08): the endpoint no longer wants the stream.
	ErrCodeCancel ErrorCode = 0x08
)

func (c ErrorCode) String() string {
	switch c {
	case ErrCodeNo:
		return "NO_ERROR"
	case ErrCodeProtocol:
		return "PROTOCOL_ERROR"
	case ErrCodeRefusedStream:
		return "REFUSED_STREAM"
	case ErrCodeCancel:
		return "CANCEL"
	default:
		return fmt.Sprintf("ERROR_0x%02x", uint32(c))
	}
}

// EncodeRSTStream serializes a RST_STREAM frame for streamID carrying code into
// a freshly allocated 13-byte slice: a 9-byte frame header plus a 4-byte error
// code (RFC 9113 §6.4). The payload length is always exactly 4.
func EncodeRSTStream(streamID uint32, code ErrorCode) []byte {
	buf := make([]byte, 13)
	// 3-byte payload length: always 4.
	buf[0] = 0
	buf[1] = 0
	buf[2] = 4
	// Type and flags (RST_STREAM defines no flags).
	buf[3] = byte(FrameRSTStream)
	buf[4] = 0
	// Reserved(1) + stream identifier(31).
	binary.BigEndian.PutUint32(buf[5:9], streamID&0x7fffffff)
	// Error code (32-bit).
	binary.BigEndian.PutUint32(buf[9:13], uint32(code))
	return buf
}

// DecodeRSTStream parses a RST_STREAM frame from buf, returning the stream ID
// and error code. It enforces the two framing rules from RFC 9113 §6.4: the
// payload length is exactly 4 (otherwise FRAME_SIZE_ERROR), and the stream
// identifier is non-zero (otherwise PROTOCOL_ERROR). The returned error is a
// *FrameError naming the offending error code.
func DecodeRSTStream(buf []byte) (streamID uint32, code ErrorCode, err error) {
	if len(buf) < 9 {
		return 0, 0, &FrameError{Code: ErrCodeFrameSize, Reason: "RST_STREAM frame shorter than 9-byte header"}
	}
	payloadLen := int(buf[0])<<16 | int(buf[1])<<8 | int(buf[2])
	if payloadLen != 4 || len(buf) < 13 {
		return 0, 0, &FrameError{Code: ErrCodeFrameSize, Reason: fmt.Sprintf("RST_STREAM payload length %d, must be 4", payloadLen)}
	}
	if FrameType(buf[3]) != FrameRSTStream {
		return 0, 0, &FrameError{Code: ErrCodeProtocol, Reason: fmt.Sprintf("frame type 0x%02x is not RST_STREAM", buf[3])}
	}
	streamID = binary.BigEndian.Uint32(buf[5:9]) & 0x7fffffff
	if streamID == 0 {
		return 0, 0, &FrameError{Code: ErrCodeProtocol, Reason: "RST_STREAM on stream 0"}
	}
	code = ErrorCode(binary.BigEndian.Uint32(buf[9:13]))
	return streamID, code, nil
}

// ErrCodeFrameSize is FRAME_SIZE_ERROR (0x06).
const ErrCodeFrameSize ErrorCode = 0x06

// FrameError is a framing-level connection error (RFC 9113 §5.4.1).
type FrameError struct {
	Code   ErrorCode
	Reason string
}

func (e *FrameError) Error() string {
	return fmt.Sprintf("RST_STREAM framing error %s: %s", e.Code, e.Reason)
}
```

### Reconciling the active-push count

The `Registry` is the connection-level bookkeeping that turns an incoming RST_STREAM into a freed concurrency slot. Each promised stream moves through a tiny lifecycle: `Promise` records it as in-flight and bumps the active count; then exactly one terminal event fires — either `HandleRST` because the client cancelled it, or `Complete` because the server finished sending it with END_STREAM — and the active count drops by one. The reason the count matters is that a pushed stream counts against `SETTINGS_MAX_CONCURRENT_STREAMS`, so a cancellation that does not promptly free its slot leaves the server believing it is closer to the concurrency ceiling than it is, and eventually refusing pushes it should allow.

Three properties make this safe under the concurrency it will actually face — `Promise` runs on a handler goroutine while `HandleRST` runs on the connection's frame reader. First, every method takes the mutex, so the count and the state map move together. Second, both terminal transitions are guarded on the current state being `statePromised`, which makes them idempotent: a duplicate RST_STREAM, or a `Complete` arriving after a cancel, finds the stream already in a terminal state and does nothing, so the count can never be double-decremented or driven negative. Third, an RST_STREAM for a stream the registry has never seen returns `false` and changes nothing, modeling a server that has already forgotten a closed stream rather than escalating a late frame into a crash. `HandleRST` returns a boolean precisely so the caller knows whether a live push was actually cancelled and a slot freed.

Create `registry.go`:

```go
package rststream

import (
	"fmt"
	"sync"
)

// pushState is the lifecycle of a single promised (pushed) stream.
type pushState int

const (
	statePromised  pushState = iota // PUSH_PROMISE sent, response in flight
	stateCancelled                  // client sent RST_STREAM on this stream
	stateClosed                     // server finished sending (END_STREAM)
)

// Registry tracks the promised streams of one connection and reconciles the
// active-push count when a client cancels an unwanted push with RST_STREAM.
// A pushed stream counts against SETTINGS_MAX_CONCURRENT_STREAMS, so freeing a
// slot promptly on cancellation is what lets the next push proceed
// (RFC 9113 §5.1.2, §6.4). The zero value is not valid; use NewRegistry.
type Registry struct {
	mu     sync.Mutex
	states map[uint32]pushState
	active int
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{states: make(map[uint32]pushState)}
}

// Promise records that a PUSH_PROMISE was sent for the even-numbered streamID,
// incrementing the active-push count. It is idempotent per stream: promising
// the same stream twice does not double-count.
func (r *Registry) Promise(streamID uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.states[streamID]; ok {
		return
	}
	r.states[streamID] = statePromised
	r.active++
}

// HandleRST applies an incoming RST_STREAM for streamID. It returns true if the
// frame cancelled a live promised stream (so the server must stop sending on
// it and a slot was freed), and false if the stream was unknown or already
// finished. A RST_STREAM for a stream the server never promised is ignored
// here rather than escalated, matching a server that has already forgotten a
// closed stream.
func (r *Registry) HandleRST(streamID uint32, code ErrorCode) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.states[streamID]
	if !ok || st != statePromised {
		return false
	}
	r.states[streamID] = stateCancelled
	r.active--
	return true
}

// Complete marks a promised stream as finished by the server (END_STREAM),
// freeing its slot. It is a no-op if the stream was already cancelled or
// closed, so a cancel followed by a late completion does not double-decrement.
func (r *Registry) Complete(streamID uint32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st, ok := r.states[streamID]; ok && st == statePromised {
		r.states[streamID] = stateClosed
		r.active--
	}
}

// Active reports how many promised streams are still in flight.
func (r *Registry) Active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Cancelled reports whether streamID was cancelled by the client.
func (r *Registry) Cancelled(streamID uint32) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.states[streamID] == stateCancelled
}

// String renders the registry state for diagnostics.
func (r *Registry) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprintf("active=%d streams=%d", r.active, len(r.states))
}
```

### The runnable demo

The demo promises three pushes on even streams 2, 4, and 6, then plays the role of both sides of a cancellation: the client encodes RST_STREAM CANCEL for streams 4 and 6, the server decodes each frame and reconciles its registry, the surviving push on stream 2 completes normally, and finally a deliberately corrupted frame (stream ID zeroed) shows the stream-0 framing rule firing.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	rst "example.com/rst-stream-cancel"
)

func main() {
	reg := rst.NewRegistry()

	// Server promises three resources on even stream IDs.
	for _, id := range []uint32{2, 4, 6} {
		reg.Promise(id)
	}
	fmt.Printf("after 3 promises: active=%d\n", reg.Active())

	// The client already has /app.js (stream 4) and /logo.png (stream 6) in
	// cache, so it cancels those two pushes with RST_STREAM CANCEL. The server
	// encodes nothing here; it receives and decodes the client's frames.
	for _, id := range []uint32{4, 6} {
		frame := rst.EncodeRSTStream(id, rst.ErrCodeCancel)
		sid, code, err := rst.DecodeRSTStream(frame)
		if err != nil {
			fmt.Println("decode:", err)
			continue
		}
		freed := reg.HandleRST(sid, code)
		fmt.Printf("RST_STREAM stream=%d code=%s -> freed=%v\n", sid, code, freed)
	}
	fmt.Printf("after 2 cancels: active=%d\n", reg.Active())

	// The surviving push (stream 2) completes normally.
	reg.Complete(2)
	fmt.Printf("after completing stream 2: active=%d\n", reg.Active())

	fmt.Printf("stream 4 cancelled: %v\n", reg.Cancelled(4))
	fmt.Printf("stream 2 cancelled: %v\n", reg.Cancelled(2))

	// A RST_STREAM on stream 0 is a framing error.
	bad := rst.EncodeRSTStream(2, rst.ErrCodeCancel)
	bad[5], bad[6], bad[7], bad[8] = 0, 0, 0, 0
	if _, _, err := rst.DecodeRSTStream(bad); err != nil {
		fmt.Println("stream 0 frame:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
after 3 promises: active=3
RST_STREAM stream=4 code=CANCEL -> freed=true
RST_STREAM stream=6 code=CANCEL -> freed=true
after 2 cancels: active=1
after completing stream 2: active=0
stream 4 cancelled: true
stream 2 cancelled: false
stream 0 frame: RST_STREAM framing error PROTOCOL_ERROR: RST_STREAM on stream 0
```

The active count tracks reality at each step: three in flight, two freed by cancellation, the last freed by completion, ending at zero. Stream 4 is marked cancelled while the completed stream 2 is not, and the corrupted frame is rejected as a PROTOCOL_ERROR.

### Tests

The tests pin the encode/decode round-trip and the two framing rejections, then the registry behaviors: a cancel frees exactly one slot and marks the stream cancelled; an RST_STREAM for an unknown stream is ignored; a double cancel does not double-decrement; a `Complete` after a cancel is a no-op. `TestConcurrentPromiseAndCancel` promises a hundred streams and cancels them from a hundred goroutines, asserting the final count is zero under `-race`.

Create `rststream_test.go`:

```go
package rststream

import (
	"errors"
	"sync"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	frame := EncodeRSTStream(4, ErrCodeCancel)
	if len(frame) != 13 {
		t.Fatalf("frame length = %d, want 13", len(frame))
	}
	if frame[3] != byte(FrameRSTStream) {
		t.Fatalf("type = 0x%02x, want 0x03", frame[3])
	}
	id, code, err := DecodeRSTStream(frame)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if id != 4 || code != ErrCodeCancel {
		t.Fatalf("decoded id=%d code=%s, want 4 CANCEL", id, code)
	}
}

func TestDecodeRejectsStreamZero(t *testing.T) {
	t.Parallel()
	frame := EncodeRSTStream(0, ErrCodeCancel)
	_, _, err := DecodeRSTStream(frame)
	var fe *FrameError
	if !errors.As(err, &fe) || fe.Code != ErrCodeProtocol {
		t.Fatalf("err = %v, want PROTOCOL_ERROR", err)
	}
}

func TestDecodeRejectsWrongLength(t *testing.T) {
	t.Parallel()
	frame := EncodeRSTStream(2, ErrCodeCancel)
	frame[2] = 5 // claim a 5-byte payload
	_, _, err := DecodeRSTStream(frame)
	var fe *FrameError
	if !errors.As(err, &fe) || fe.Code != ErrCodeFrameSize {
		t.Fatalf("err = %v, want FRAME_SIZE_ERROR", err)
	}
}

func TestCancelFreesSlot(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Promise(2)
	reg.Promise(4)
	if reg.Active() != 2 {
		t.Fatalf("active = %d, want 2", reg.Active())
	}
	if !reg.HandleRST(4, ErrCodeCancel) {
		t.Fatal("HandleRST(4) should report a cancelled live stream")
	}
	if reg.Active() != 1 {
		t.Fatalf("active = %d after cancel, want 1", reg.Active())
	}
	if !reg.Cancelled(4) {
		t.Fatal("stream 4 should be marked cancelled")
	}
}

func TestRSTOnUnknownStreamIsIgnored(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Promise(2)
	if reg.HandleRST(8, ErrCodeCancel) {
		t.Fatal("HandleRST on an unknown stream should return false")
	}
	if reg.Active() != 1 {
		t.Fatalf("active = %d, want 1 (unknown RST must not change count)", reg.Active())
	}
}

func TestDoubleCancelDoesNotDoubleDecrement(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Promise(2)
	reg.HandleRST(2, ErrCodeCancel)
	// A duplicate RST_STREAM must not drive the count negative.
	if reg.HandleRST(2, ErrCodeCancel) {
		t.Fatal("second HandleRST should return false")
	}
	if reg.Active() != 0 {
		t.Fatalf("active = %d, want 0", reg.Active())
	}
}

func TestCompleteAfterCancelIsNoOp(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	reg.Promise(2)
	reg.HandleRST(2, ErrCodeCancel)
	reg.Complete(2) // late completion of an already-cancelled stream
	if reg.Active() != 0 {
		t.Fatalf("active = %d, want 0", reg.Active())
	}
}

func TestConcurrentPromiseAndCancel(t *testing.T) {
	t.Parallel()
	reg := NewRegistry()
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := uint32(2 * (i + 1))
		reg.Promise(id)
		wg.Add(1)
		go func() {
			defer wg.Done()
			reg.HandleRST(id, ErrCodeCancel)
		}()
	}
	wg.Wait()
	if reg.Active() != 0 {
		t.Fatalf("active = %d after %d cancels, want 0", reg.Active(), n)
	}
}
```

## Review

The registry is correct when every terminal transition is idempotent and guarded by the mutex — `TestDoubleCancelDoesNotDoubleDecrement` and `TestCompleteAfterCancelIsNoOp` are the guards, and `TestConcurrentPromiseAndCancel` under `-race` is what proves the count and the map never tear apart under concurrent access. On the framing side, keep the two rejection codes distinct: a wrong length is FRAME_SIZE_ERROR and a zero stream is PROTOCOL_ERROR, and conflating them sends the wrong GOAWAY code. The bug this module exists to prevent is the slow leak of the active-push count: increment on `Promise`, decrement on exactly one of `HandleRST` or `Complete`, never on both, and never twice — otherwise the count drifts and the server starts refusing legitimate pushes for the rest of the connection.

## Resources

- [RFC 9113 §6.4 — RST_STREAM](https://httpwg.org/specs/rfc9113.html#RST_STREAM) — the fixed 4-byte payload, the stream-0 rule, and the frame's role in aborting a stream.
- [RFC 9113 §7 — Error Codes](https://httpwg.org/specs/rfc9113.html#ErrorCodes) — CANCEL, REFUSED_STREAM, PROTOCOL_ERROR, FRAME_SIZE_ERROR.
- [RFC 9113 §5.1.2 — Stream Concurrency](https://httpwg.org/specs/rfc9113.html#StreamConcurrency) — why a cancelled push must free its slot against SETTINGS_MAX_CONCURRENT_STREAMS.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — the lock keeping the registry's count and state map consistent.

---

Back to [04-enable-push-negotiation.md](04-enable-push-negotiation.md) | Next: [06-promised-stream-sequencing.md](06-promised-stream-sequencing.md)
