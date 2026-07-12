# Exercise 1: Stream Multiplexer

The multiplexer is the heart of HTTP/2: it owns every stream on one connection, drives each stream through the RFC 9113 §5.1 lifecycle, and holds the send-side flow-control windows at both the per-stream and connection level. This exercise builds that `mux` package — a `Mux` plus a `Stream` — as a pure in-memory engine you wire onto a real `net.Conn` in later lessons.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
mux/
  go.mod
  stream.go            FrameType, ErrCode, Frame, StreamState, stateTable, Stream
  mux.go               Mux: OpenStream, Deliver, ApplyWindowUpdate, UpdateInitialWindowSize, ResetStream
  mux_test.go          state transitions, flow-control blocking, max-concurrent, window update
  example_test.go      ExampleNewMux, ExampleMux_ApplyWindowUpdate
  cmd/demo/main.go     open streams, apply WINDOW_UPDATEs, change INITIAL_WINDOW_SIZE
```

- Files: `stream.go`, `mux.go`, `mux_test.go`, `example_test.go`, `cmd/demo/main.go`.
- Implement: `Stream` with the state machine, `ConsumeSendWindow`/`addSendWindow`, `Done`/`Recv`; `Mux` with `OpenStream`, `Deliver`, `ApplyWindowUpdate`, `ConsumeConnWindow`, `UpdateInitialWindowSize`, `ResetStream`, `Out`.
- Test: every legal transition reaches the right state and every illegal one returns `ErrInvalidTransition`; flow control blocks until credit is granted and unblocks on close; the concurrent-stream limit trips and is released; `INITIAL_WINDOW_SIZE` adjusts existing streams; overflow is rejected.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/44-capstone-http2-implementation/03-stream-multiplexing/01-stream-multiplexer/cmd/demo && cd go-solutions/44-capstone-http2-implementation/03-stream-multiplexing/01-stream-multiplexer
go mod edit -go=1.26
```

This package has no network in its tests — the multiplexer logic is pure in-memory, so the whole lifecycle and both flow-control levels are exercised without a socket. You wire it onto a `net.Conn` when you integrate with the rest of the HTTP/2 stack: a read goroutine calls `Deliver`, a write goroutine drains `Out()`.

### The frame vocabulary and the state table

`stream.go` starts with the small slice of the HTTP/2 wire vocabulary the multiplexer needs — the frame types it routes, the error codes it emits, the END_STREAM flag, and the window constants — then the `Stream` type itself. The state machine is encoded as `stateTable`, a map from `(state, event)` to the next state. This is the whole point of modelling it as data: a transition is legal exactly when the key is present, so `transition` is a single map lookup and the illegal transitions are the keys that are *absent*, which a table-driven test can enumerate directly. Client-initiated streams only ever start with `evSendHeaders` from `idle`; the reserved (push) states are added in the server-push lesson.

The concurrency design lives in the `Stream` fields. `mu` guards the state and both windows; `flowCond` is a `sync.Cond` tied to `mu` so a goroutine waiting for send credit can sleep and be woken atomically. `closed` is a channel closed exactly once (guarded by `closeOnce`) when the stream reaches `StateClosed`, so any number of observers can select on `Done()`. `recvCh` is the buffered channel the connection read loop pushes demultiplexed frames into.

Create `stream.go`:

```go
package mux

import (
	"errors"
	"fmt"
	"sync"
)

// FrameType identifies an HTTP/2 frame type per RFC 9113 §6.
type FrameType uint8

const (
	FrameData         FrameType = 0x0
	FrameHeaders      FrameType = 0x1
	FrameRSTStream    FrameType = 0x3
	FrameSettings     FrameType = 0x4
	FrameWindowUpdate FrameType = 0x8
)

func (ft FrameType) String() string {
	switch ft {
	case FrameData:
		return "DATA"
	case FrameHeaders:
		return "HEADERS"
	case FrameRSTStream:
		return "RST_STREAM"
	case FrameSettings:
		return "SETTINGS"
	case FrameWindowUpdate:
		return "WINDOW_UPDATE"
	default:
		return fmt.Sprintf("UNKNOWN(0x%02x)", uint8(ft))
	}
}

// ErrCode is an HTTP/2 error code per RFC 9113 §7.
type ErrCode uint32

const (
	ErrCodeNoError          ErrCode = 0x0
	ErrCodeProtocolError    ErrCode = 0x1
	ErrCodeFlowControlError ErrCode = 0x3
	ErrCodeStreamClosed     ErrCode = 0x5
	ErrCodeRefusedStream    ErrCode = 0x7
)

// FlagEndStream is set on the last DATA or HEADERS frame of a stream
// (RFC 9113 §6.1, §6.2).
const FlagEndStream uint8 = 0x1

// defaultInitialWindowSize is 65535 bytes per RFC 9113 §6.9.2.
const defaultInitialWindowSize int32 = 65535

// maxWindowSize is 2^31-1 per RFC 9113 §6.9.1.
const maxWindowSize int32 = 1<<31 - 1

// Frame is a parsed HTTP/2 frame as demultiplexed to a single stream.
type Frame struct {
	Type     FrameType
	Flags    uint8
	StreamID uint32
	Payload  []byte
}

// HasFlag reports whether the given flag bit is set.
func (f Frame) HasFlag(flag uint8) bool { return f.Flags&flag != 0 }

// Sentinel errors returned by stream and mux operations.
var (
	ErrInvalidTransition = errors.New("mux: invalid stream state transition")
	ErrStreamNotFound    = errors.New("mux: stream not found")
	ErrFlowControlLimit  = errors.New("mux: flow control window exceeded maximum")
	ErrMaxStreams        = errors.New("mux: maximum concurrent streams reached")
	ErrStreamClosed      = errors.New("mux: stream is closed")
)

// StreamState represents the HTTP/2 stream lifecycle per RFC 9113 §5.1.
type StreamState int

const (
	StateIdle StreamState = iota
	StateOpen
	StateHalfClosedLocal
	StateHalfClosedRemote
	StateClosed
)

func (st StreamState) String() string {
	switch st {
	case StateIdle:
		return "idle"
	case StateOpen:
		return "open"
	case StateHalfClosedLocal:
		return "half-closed(local)"
	case StateHalfClosedRemote:
		return "half-closed(remote)"
	case StateClosed:
		return "closed"
	default:
		return fmt.Sprintf("StreamState(%d)", int(st))
	}
}

// frameEvent drives the stream state machine.
type frameEvent int

const (
	evSendHeaders   frameEvent = iota // local sends HEADERS (client request)
	evSendEndStream                   // local sends DATA or HEADERS with END_STREAM
	evRecvEndStream                   // peer sends DATA or HEADERS with END_STREAM
	evSendRST                         // local sends RST_STREAM
	evRecvRST                         // peer sends RST_STREAM
)

func (e frameEvent) String() string {
	switch e {
	case evSendHeaders:
		return "send-HEADERS"
	case evSendEndStream:
		return "send-END_STREAM"
	case evRecvEndStream:
		return "recv-END_STREAM"
	case evSendRST:
		return "send-RST_STREAM"
	case evRecvRST:
		return "recv-RST_STREAM"
	default:
		return fmt.Sprintf("frameEvent(%d)", int(e))
	}
}

type stateKey struct {
	state StreamState
	event frameEvent
}

// stateTable encodes the HTTP/2 stream state machine for client-initiated
// streams per RFC 9113 §5.1. Each entry maps (current state, event) to the
// next state. Events not present in the table are protocol errors.
var stateTable = map[stateKey]StreamState{
	// idle: client sends request HEADERS -> open
	{StateIdle, evSendHeaders}: StateOpen,

	// open: either side completes or resets
	{StateOpen, evSendEndStream}: StateHalfClosedLocal,
	{StateOpen, evRecvEndStream}: StateHalfClosedRemote,
	{StateOpen, evSendRST}:       StateClosed,
	{StateOpen, evRecvRST}:       StateClosed,

	// half-closed(local): we have sent END_STREAM; waiting for peer to finish
	{StateHalfClosedLocal, evRecvEndStream}: StateClosed,
	{StateHalfClosedLocal, evRecvRST}:       StateClosed,
	{StateHalfClosedLocal, evSendRST}:       StateClosed,

	// half-closed(remote): peer has sent END_STREAM; we can still send
	{StateHalfClosedRemote, evSendEndStream}: StateClosed,
	{StateHalfClosedRemote, evSendRST}:       StateClosed,
	{StateHalfClosedRemote, evRecvRST}:       StateClosed,
}

// Stream represents a single HTTP/2 stream. All methods are safe for
// concurrent use.
type Stream struct {
	id uint32

	// mu protects state, sendWindow, and recvWindow.
	// flowCond is tied to mu via sync.NewCond(&s.mu).
	mu         sync.Mutex
	state      StreamState
	sendWindow int32
	recvWindow int32
	flowCond   *sync.Cond

	// closeOnce ensures s.closed is closed exactly once.
	closeOnce sync.Once
	closed    chan struct{}

	// recvCh buffers frames demultiplexed from the connection read loop.
	recvCh chan Frame
}

func newStream(id uint32, initialSendWindow, initialRecvWindow int32) *Stream {
	s := &Stream{
		id:         id,
		state:      StateIdle,
		sendWindow: initialSendWindow,
		recvWindow: initialRecvWindow,
		closed:     make(chan struct{}),
		recvCh:     make(chan Frame, 16),
	}
	s.flowCond = sync.NewCond(&s.mu)
	return s
}

// ID returns the stream identifier.
func (s *Stream) ID() uint32 { return s.id }

// State returns the current stream state. Safe to call concurrently.
func (s *Stream) State() StreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SendWindow returns the current per-stream send flow control credit.
func (s *Stream) SendWindow() int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendWindow
}

// transition advances the state machine by ev. Returns ErrInvalidTransition if
// the event is not valid in the current state. When the stream reaches
// StateClosed, Done() is closed and any goroutine blocked in ConsumeSendWindow
// is unblocked with ErrStreamClosed.
func (s *Stream) transition(ev frameEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next, ok := stateTable[stateKey{s.state, ev}]
	if !ok {
		return fmt.Errorf("%w: state=%s event=%s", ErrInvalidTransition, s.state, ev)
	}
	s.state = next
	if s.state == StateClosed {
		s.closeOnce.Do(func() { close(s.closed) })
		s.flowCond.Broadcast() // unblock ConsumeSendWindow
	}
	return nil
}

// ConsumeSendWindow blocks until n bytes of per-stream send credit are
// available or the stream closes. This is the per-stream half of HTTP/2 flow
// control (RFC 9113 §5.2). A real sender must also check the connection-level
// window via Mux.ConsumeConnWindow.
func (s *Stream) ConsumeSendWindow(n int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.sendWindow < n {
		if s.state == StateClosed {
			return fmt.Errorf("%w: stream %d", ErrStreamClosed, s.id)
		}
		s.flowCond.Wait()
	}
	s.sendWindow -= n
	return nil
}

// addSendWindow increases the per-stream send credit by delta.
// Called when a WINDOW_UPDATE frame is received for this stream.
func (s *Stream) addSendWindow(delta int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta > 0 && s.sendWindow > maxWindowSize-delta {
		return fmt.Errorf("%w: stream %d overflow", ErrFlowControlLimit, s.id)
	}
	s.sendWindow += delta
	s.flowCond.Broadcast()
	return nil
}

// deliver routes an incoming frame to the stream's receive channel.
// Silently drops the frame if the stream is already closed.
func (s *Stream) deliver(f Frame) {
	select {
	case s.recvCh <- f:
	case <-s.closed:
	}
}

// Recv returns the channel on which demultiplexed frames are delivered.
// Consumers should also select on Done() to detect stream close.
func (s *Stream) Recv() <-chan Frame { return s.recvCh }

// Done returns a channel closed when this stream reaches StateClosed.
func (s *Stream) Done() <-chan struct{} { return s.closed }
```

The `flowCond` is the key concurrency primitive. It is tied to `s.mu` via `sync.NewCond(&s.mu)`. `ConsumeSendWindow` calls `Wait()`, which atomically releases `s.mu` and suspends the goroutine. When `addSendWindow` or `transition` (to `StateClosed`) calls `Broadcast()`, all waiters wake, re-acquire `s.mu`, and re-evaluate the loop condition. The `for` loop (not `if`) handles both spurious wakeups and the case where the window was already sufficient before the goroutine even called `Wait()`.

### The connection multiplexer

`mux.go` owns the stream table and the connection-level send window. `OpenStream` allocates the next odd ID, enforces the peer's `SETTINGS_MAX_CONCURRENT_STREAMS` by refusing with `ErrMaxStreams`, and starts a tiny cleanup goroutine that releases the concurrency slot when the stream's `Done()` fires. `Deliver` takes only a read lock and pushes the frame into the stream's buffered channel, so the read goroutine that calls it stays fast. `ApplyWindowUpdate` routes a WINDOW_UPDATE to the connection window (stream ID 0) or to a specific stream. `UpdateInitialWindowSize` is the retroactive §6.9.2 adjustment: it applies the signed delta to every existing stream. `ResetStream` transitions the stream to closed *before* enqueuing the RST_STREAM frame, so the local state is already correct by the time the frame hits the wire.

Create `mux.go`:

```go
package mux

import (
	"fmt"
	"sync"
)

// Mux manages all HTTP/2 streams on a single connection. The zero value is not
// useful; use NewMux.
//
// Architecture: a single read goroutine calls Deliver for each incoming frame;
// a single write goroutine drains Out() and writes to the connection. Each
// stream has its own goroutine that reads from Stream.Recv().
type Mux struct {
	mu      sync.RWMutex
	streams map[uint32]*Stream

	// nextClientID is the next odd stream ID for client-initiated streams
	// (RFC 9113 §5.1.1).
	nextClientID uint32

	// openCount tracks non-closed streams for SETTINGS_MAX_CONCURRENT_STREAMS.
	openCount uint32

	// initialSendWindow is the current SETTINGS_INITIAL_WINDOW_SIZE value,
	// applied to new streams and adjusted retroactively on SETTINGS changes.
	initialSendWindow int32

	// maxConcurrentStreams is the SETTINGS_MAX_CONCURRENT_STREAMS limit.
	maxConcurrentStreams uint32

	// Connection-level flow control (RFC 9113 §5.2).
	connMu         sync.Mutex
	connSendWindow int32
	connFlowCond   *sync.Cond

	// outCh serializes all outbound frames from all streams onto the single
	// connection write goroutine. Buffered to absorb bursts.
	outCh chan Frame
}

// NewMux creates a multiplexer that allows at most maxConcurrent open streams.
func NewMux(maxConcurrent uint32) *Mux {
	m := &Mux{
		streams:              make(map[uint32]*Stream),
		nextClientID:         1,
		initialSendWindow:    defaultInitialWindowSize,
		maxConcurrentStreams: maxConcurrent,
		connSendWindow:       defaultInitialWindowSize,
		outCh:                make(chan Frame, 256),
	}
	m.connFlowCond = sync.NewCond(&m.connMu)
	return m
}

// OpenStream allocates a new client-initiated stream (odd ID, monotonically
// increasing). Returns ErrMaxStreams when the SETTINGS_MAX_CONCURRENT_STREAMS
// limit is reached (RFC 9113 §5.1.2).
func (m *Mux) OpenStream() (*Stream, error) {
	m.mu.Lock()

	if m.openCount >= m.maxConcurrentStreams {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w (limit=%d)", ErrMaxStreams, m.maxConcurrentStreams)
	}

	id := m.nextClientID
	m.nextClientID += 2 // odd IDs for client-initiated streams
	m.openCount++

	s := newStream(id, m.initialSendWindow, defaultInitialWindowSize)
	m.streams[id] = s
	m.mu.Unlock()

	// When the stream closes, release the concurrent-stream slot.
	go func() {
		<-s.Done()
		m.mu.Lock()
		if m.openCount > 0 {
			m.openCount--
		}
		m.mu.Unlock()
	}()

	return s, nil
}

// Deliver demultiplexes an incoming frame to the correct stream's receive
// channel. Returns ErrStreamNotFound for unknown stream IDs.
//
// This is called by the connection read goroutine. It must not block for long:
// a slow Deliver would prevent WINDOW_UPDATE and RST_STREAM frames from being
// processed, deadlocking all senders waiting on flow control.
func (m *Mux) Deliver(f Frame) error {
	m.mu.RLock()
	s, ok := m.streams[f.StreamID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: id=%d", ErrStreamNotFound, f.StreamID)
	}
	s.deliver(f)
	return nil
}

// ApplyWindowUpdate processes an incoming WINDOW_UPDATE frame. A streamID of 0
// targets the connection-level window; any other ID targets that stream's window
// (RFC 9113 §6.9).
func (m *Mux) ApplyWindowUpdate(streamID uint32, increment int32) error {
	if streamID == 0 {
		return m.addConnSendWindow(increment)
	}
	m.mu.RLock()
	s, ok := m.streams[streamID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: id=%d", ErrStreamNotFound, streamID)
	}
	return s.addSendWindow(increment)
}

// addConnSendWindow increases the connection-level send window.
func (m *Mux) addConnSendWindow(delta int32) error {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	if delta > 0 && m.connSendWindow > maxWindowSize-delta {
		return fmt.Errorf("%w: connection overflow", ErrFlowControlLimit)
	}
	m.connSendWindow += delta
	m.connFlowCond.Broadcast()
	return nil
}

// ConsumeConnWindow blocks until n bytes of connection-level send credit are
// available. A complete DATA send must consume both the per-stream window
// (via Stream.ConsumeSendWindow) and this connection window.
func (m *Mux) ConsumeConnWindow(n int32) {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	for m.connSendWindow < n {
		m.connFlowCond.Wait()
	}
	m.connSendWindow -= n
}

// ConnSendWindow returns the current connection-level send flow control credit.
func (m *Mux) ConnSendWindow() int32 {
	m.connMu.Lock()
	defer m.connMu.Unlock()
	return m.connSendWindow
}

// UpdateInitialWindowSize applies a SETTINGS_INITIAL_WINDOW_SIZE change. All
// existing streams' send windows are adjusted by the delta (RFC 9113 §6.9.2).
// A delta that would push any stream's window beyond 2^31-1 is a flow control
// error (FLOW_CONTROL_ERROR).
func (m *Mux) UpdateInitialWindowSize(newSize int32) error {
	if newSize < 0 || newSize > maxWindowSize {
		return fmt.Errorf("%w: initial window size %d out of range [0, %d]",
			ErrFlowControlLimit, newSize, maxWindowSize)
	}
	delta := newSize - m.initialSendWindow

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, s := range m.streams {
		if err := s.addSendWindow(delta); err != nil {
			return err
		}
	}
	m.initialSendWindow = newSize
	return nil
}

// ResetStream sends RST_STREAM for the given stream, immediately transitioning
// it to closed and enqueuing the wire frame for the write goroutine.
func (m *Mux) ResetStream(streamID uint32, code ErrCode) error {
	m.mu.RLock()
	s, ok := m.streams[streamID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: id=%d", ErrStreamNotFound, streamID)
	}
	if err := s.transition(evSendRST); err != nil {
		return err
	}
	// Encode the 4-byte error code payload (RFC 9113 §6.4).
	payload := [4]byte{
		byte(code >> 24),
		byte(code >> 16),
		byte(code >> 8),
		byte(code),
	}
	m.outCh <- Frame{
		Type:     FrameRSTStream,
		StreamID: streamID,
		Payload:  payload[:],
	}
	return nil
}

// Out returns the channel on which all outbound frames are queued. A single
// write goroutine must drain this channel and write frames to the connection.
func (m *Mux) Out() <-chan Frame { return m.outCh }

// StreamCount returns the total number of streams in the stream table.
func (m *Mux) StreamCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.streams)
}
```

`OpenStream` increments `openCount` under the write lock and starts a cleanup goroutine that decrements `openCount` when `s.Done()` fires. The `streams` map uses `sync.RWMutex`: `Deliver` is called on every incoming frame and takes only a read lock, keeping demultiplexing fast. `ResetStream` transitions the stream synchronously before enqueuing the RST_STREAM frame, so the state is already `StateClosed` by the time the frame hits the wire.

### The runnable demo

The demo opens three streams, applies a connection-level WINDOW_UPDATE and a per-stream one, then changes `SETTINGS_INITIAL_WINDOW_SIZE` and prints each stream's window so the retroactive delta is visible. Stream 1 ends at 144384, not 128000, because it also took the earlier +16384: `UpdateInitialWindowSize` applies the delta `128000 − 65535 = 62465` to the *current* window, which is the correct §6.9.2 behaviour. The final RST_STREAM on an idle stream prints the `ErrInvalidTransition` error, showing the state machine rejects an illegal reset.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	mux "example.com/mux"
)

func main() {
	m := mux.NewMux(100)

	fmt.Println("=== opening streams ===")
	streams := make([]*mux.Stream, 3)
	for i := range streams {
		s, err := m.OpenStream()
		if err != nil {
			log.Fatalf("OpenStream: %v", err)
		}
		streams[i] = s
		fmt.Printf("  stream %d: state=%s send_window=%d\n",
			s.ID(), s.State(), s.SendWindow())
	}

	fmt.Println("\n=== connection WINDOW_UPDATE (+32768) ===")
	before := m.ConnSendWindow()
	if err := m.ApplyWindowUpdate(0, 32768); err != nil {
		log.Fatalf("ApplyWindowUpdate(conn): %v", err)
	}
	fmt.Printf("  conn window: %d -> %d\n", before, m.ConnSendWindow())

	fmt.Println("\n=== stream 1 WINDOW_UPDATE (+16384) ===")
	streamBefore := streams[0].SendWindow()
	if err := m.ApplyWindowUpdate(streams[0].ID(), 16384); err != nil {
		log.Fatalf("ApplyWindowUpdate(stream): %v", err)
	}
	fmt.Printf("  stream %d window: %d -> %d\n",
		streams[0].ID(), streamBefore, streams[0].SendWindow())

	fmt.Println("\n=== SETTINGS_INITIAL_WINDOW_SIZE = 128000 ===")
	if err := m.UpdateInitialWindowSize(128000); err != nil {
		log.Fatalf("UpdateInitialWindowSize: %v", err)
	}
	for _, s := range streams {
		fmt.Printf("  stream %d send_window=%d\n", s.ID(), s.SendWindow())
	}

	fmt.Println("\n=== RST_STREAM on idle stream (expected error) ===")
	if err := m.ResetStream(streams[0].ID(), mux.ErrCodeNoError); err != nil {
		fmt.Printf("  %v\n", err)
	}

	fmt.Println("\n=== frame type strings ===")
	for _, ft := range []mux.FrameType{
		mux.FrameData,
		mux.FrameHeaders,
		mux.FrameRSTStream,
		mux.FrameSettings,
		mux.FrameWindowUpdate,
	} {
		fmt.Printf("  0x%02x = %s\n", uint8(ft), ft)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== opening streams ===
  stream 1: state=idle send_window=65535
  stream 3: state=idle send_window=65535
  stream 5: state=idle send_window=65535

=== connection WINDOW_UPDATE (+32768) ===
  conn window: 65535 -> 98303

=== stream 1 WINDOW_UPDATE (+16384) ===
  stream 1 window: 65535 -> 81919

=== SETTINGS_INITIAL_WINDOW_SIZE = 128000 ===
  stream 1 send_window=144384
  stream 3 send_window=128000
  stream 5 send_window=128000

=== RST_STREAM on idle stream (expected error) ===
  mux: invalid stream state transition: state=idle event=send-RST_STREAM

=== frame type strings ===
  0x00 = DATA
  0x01 = HEADERS
  0x03 = RST_STREAM
  0x04 = SETTINGS
  0x08 = WINDOW_UPDATE
```

### Tests

The tests are package-internal so they can drive `transition` and the unexported events directly. `TestStreamStateTransitions` is the table that pins both the legal paths and the illegal ones (an idle stream cannot be reset, a half-closed side cannot end again). The flow-control tests prove a zero-window consume blocks and then unblocks once credit is added, that closing the stream releases a blocked consumer with `ErrStreamClosed`, and that pushing a window past 2^31−1 is rejected. `TestMaxConcurrentStreamsReleasedOnClose` pins the cleanup-goroutine contract: a slot held at the limit is freed once the stream reaches closed and its `Done()` fires.

Create `mux_test.go`:

```go
package mux

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestStreamStateTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		events  []frameEvent
		want    StreamState
		wantErr bool
	}{
		{
			name:   "idle to open on send headers",
			events: []frameEvent{evSendHeaders},
			want:   StateOpen,
		},
		{
			name:   "open to half-closed-local on send end stream",
			events: []frameEvent{evSendHeaders, evSendEndStream},
			want:   StateHalfClosedLocal,
		},
		{
			name:   "open to half-closed-remote on recv end stream",
			events: []frameEvent{evSendHeaders, evRecvEndStream},
			want:   StateHalfClosedRemote,
		},
		{
			name:   "half-closed-local to closed on recv end stream",
			events: []frameEvent{evSendHeaders, evSendEndStream, evRecvEndStream},
			want:   StateClosed,
		},
		{
			name:   "half-closed-remote to closed on send end stream",
			events: []frameEvent{evSendHeaders, evRecvEndStream, evSendEndStream},
			want:   StateClosed,
		},
		{
			name:   "open to closed on send RST",
			events: []frameEvent{evSendHeaders, evSendRST},
			want:   StateClosed,
		},
		{
			name:   "open to closed on recv RST",
			events: []frameEvent{evSendHeaders, evRecvRST},
			want:   StateClosed,
		},
		{
			name:   "half-closed-local to closed on recv RST",
			events: []frameEvent{evSendHeaders, evSendEndStream, evRecvRST},
			want:   StateClosed,
		},
		{
			name:    "invalid: idle cannot send RST",
			events:  []frameEvent{evSendRST},
			wantErr: true,
		},
		{
			name:    "invalid: idle cannot recv RST",
			events:  []frameEvent{evRecvRST},
			wantErr: true,
		},
		{
			name:    "invalid: half-closed-local cannot send another END_STREAM",
			events:  []frameEvent{evSendHeaders, evSendEndStream, evSendEndStream},
			wantErr: true,
		},
		{
			name:    "invalid: half-closed-remote cannot receive another END_STREAM",
			events:  []frameEvent{evSendHeaders, evRecvEndStream, evRecvEndStream},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStream(1, defaultInitialWindowSize, defaultInitialWindowSize)
			var err error
			for _, ev := range tc.events {
				err = s.transition(ev)
				if err != nil {
					break
				}
			}
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("err = %v, want ErrInvalidTransition", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := s.State(); got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestStreamDoneClosedOnRST(t *testing.T) {
	t.Parallel()

	s := newStream(1, defaultInitialWindowSize, defaultInitialWindowSize)
	if err := s.transition(evSendHeaders); err != nil {
		t.Fatalf("transition to open: %v", err)
	}
	if err := s.transition(evSendRST); err != nil {
		t.Fatalf("transition to closed: %v", err)
	}

	select {
	case <-s.Done():
		// OK
	default:
		t.Fatal("Done() channel is not closed after StateClosed")
	}
}

func TestFlowControlBlocksUntilWindowGranted(t *testing.T) {
	t.Parallel()

	// Start with zero send window so the consume will block.
	s := newStream(1, 0, defaultInitialWindowSize)

	var wg sync.WaitGroup
	wg.Add(1)
	var consumeErr error
	go func() {
		defer wg.Done()
		consumeErr = s.ConsumeSendWindow(100)
	}()

	time.Sleep(5 * time.Millisecond)

	if err := s.addSendWindow(100); err != nil {
		t.Fatalf("addSendWindow: %v", err)
	}
	wg.Wait()

	if consumeErr != nil {
		t.Fatalf("ConsumeSendWindow: %v", consumeErr)
	}
	if got := s.SendWindow(); got != 0 {
		t.Fatalf("SendWindow = %d, want 0 after consuming 100", got)
	}
}

func TestFlowControlUnblocksWhenStreamCloses(t *testing.T) {
	t.Parallel()

	s := newStream(1, 0, defaultInitialWindowSize)
	if err := s.transition(evSendHeaders); err != nil {
		t.Fatalf("transition: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var consumeErr error
	go func() {
		defer wg.Done()
		consumeErr = s.ConsumeSendWindow(100)
	}()

	time.Sleep(5 * time.Millisecond)

	if err := s.transition(evSendRST); err != nil {
		t.Fatalf("transition to closed: %v", err)
	}
	wg.Wait()

	if !errors.Is(consumeErr, ErrStreamClosed) {
		t.Fatalf("err = %v, want ErrStreamClosed", consumeErr)
	}
}

func TestFlowControlWindowOverflow(t *testing.T) {
	t.Parallel()

	s := newStream(1, maxWindowSize, defaultInitialWindowSize)
	if err := s.addSendWindow(1); !errors.Is(err, ErrFlowControlLimit) {
		t.Fatalf("err = %v, want ErrFlowControlLimit", err)
	}
}

func TestMaxConcurrentStreams(t *testing.T) {
	t.Parallel()

	m := NewMux(2)
	if _, err := m.OpenStream(); err != nil {
		t.Fatalf("OpenStream 1: %v", err)
	}
	if _, err := m.OpenStream(); err != nil {
		t.Fatalf("OpenStream 2: %v", err)
	}

	_, err := m.OpenStream()
	if !errors.Is(err, ErrMaxStreams) {
		t.Fatalf("err = %v, want ErrMaxStreams", err)
	}
}

func TestMaxConcurrentStreamsReleasedOnClose(t *testing.T) {
	t.Parallel()

	m := NewMux(2)
	s1, err := m.OpenStream()
	if err != nil {
		t.Fatalf("OpenStream 1: %v", err)
	}
	if _, err := m.OpenStream(); err != nil {
		t.Fatalf("OpenStream 2: %v", err)
	}
	// At the limit: the third open is refused.
	if _, err := m.OpenStream(); !errors.Is(err, ErrMaxStreams) {
		t.Fatalf("OpenStream 3 = %v, want ErrMaxStreams", err)
	}

	// Cycle s1 through to closed and wait for Done.
	if err := s1.transition(evSendHeaders); err != nil {
		t.Fatalf("transition to open: %v", err)
	}
	if err := s1.transition(evSendRST); err != nil {
		t.Fatalf("transition to closed: %v", err)
	}
	<-s1.Done()

	// The cleanup goroutine decrements openCount asynchronously after Done(),
	// so poll until the freed slot becomes observable.
	deadline := time.After(time.Second)
	for {
		s, err := m.OpenStream()
		if err == nil {
			if s == nil {
				t.Fatal("OpenStream returned nil stream with nil error")
			}
			return
		}
		if !errors.Is(err, ErrMaxStreams) {
			t.Fatalf("OpenStream after close = %v, want nil or ErrMaxStreams", err)
		}
		select {
		case <-deadline:
			t.Fatal("slot not released within 1s after stream close")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestStreamIDsAreOddAndMonotonic(t *testing.T) {
	t.Parallel()

	m := NewMux(100)
	var prev uint32
	for i := 0; i < 5; i++ {
		s, err := m.OpenStream()
		if err != nil {
			t.Fatalf("OpenStream %d: %v", i, err)
		}
		if s.ID()%2 == 0 {
			t.Errorf("stream ID %d is even (client-initiated must be odd)", s.ID())
		}
		if s.ID() <= prev {
			t.Errorf("stream ID %d <= prev %d (must be strictly increasing)", s.ID(), prev)
		}
		prev = s.ID()
	}
}

func TestDeliverRoutesFrameToCorrectStream(t *testing.T) {
	t.Parallel()

	m := NewMux(10)
	s1, _ := m.OpenStream()
	s2, _ := m.OpenStream()

	f := Frame{Type: FrameData, StreamID: s1.ID(), Payload: []byte("hello")}
	if err := m.Deliver(f); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	select {
	case got := <-s1.Recv():
		if string(got.Payload) != "hello" {
			t.Fatalf("payload = %q, want %q", got.Payload, "hello")
		}
	default:
		t.Fatal("frame not in s1.Recv()")
	}

	select {
	case f := <-s2.Recv():
		t.Fatalf("s2 received unexpected frame: %+v", f)
	default:
		// OK: frame was not routed to s2
	}
}

func TestDeliverUnknownStreamID(t *testing.T) {
	t.Parallel()

	m := NewMux(10)
	err := m.Deliver(Frame{StreamID: 9999})
	if !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("err = %v, want ErrStreamNotFound", err)
	}
}

func TestUpdateInitialWindowSizeAdjustsExistingStreams(t *testing.T) {
	t.Parallel()

	m := NewMux(10)
	s1, _ := m.OpenStream()
	s2, _ := m.OpenStream()

	const newWindow = int32(128000)
	if err := m.UpdateInitialWindowSize(newWindow); err != nil {
		t.Fatalf("UpdateInitialWindowSize: %v", err)
	}

	if got := s1.SendWindow(); got != newWindow {
		t.Fatalf("s1.SendWindow = %d, want %d", got, newWindow)
	}
	if got := s2.SendWindow(); got != newWindow {
		t.Fatalf("s2.SendWindow = %d, want %d", got, newWindow)
	}

	// A new stream must also use the updated initial window.
	s3, _ := m.OpenStream()
	if got := s3.SendWindow(); got != newWindow {
		t.Fatalf("s3.SendWindow = %d, want %d", got, newWindow)
	}
}

func TestApplyWindowUpdateConnectionLevel(t *testing.T) {
	t.Parallel()

	m := NewMux(10)
	before := m.ConnSendWindow()

	if err := m.ApplyWindowUpdate(0, 32768); err != nil {
		t.Fatalf("ApplyWindowUpdate: %v", err)
	}

	want := before + 32768
	if got := m.ConnSendWindow(); got != want {
		t.Fatalf("ConnSendWindow = %d, want %d", got, want)
	}
}

func TestResetStreamClosesStreamAndEmitsFrame(t *testing.T) {
	t.Parallel()

	m := NewMux(10)
	s, _ := m.OpenStream()

	if err := s.transition(evSendHeaders); err != nil {
		t.Fatalf("transition to open: %v", err)
	}
	if err := m.ResetStream(s.ID(), ErrCodeNoError); err != nil {
		t.Fatalf("ResetStream: %v", err)
	}
	if got := s.State(); got != StateClosed {
		t.Fatalf("state = %s, want closed", got)
	}

	select {
	case <-s.Done():
		// OK
	default:
		t.Fatal("Done() not closed after ResetStream")
	}

	select {
	case f := <-m.Out():
		if f.Type != FrameRSTStream {
			t.Fatalf("frame type = %s, want RST_STREAM", f.Type)
		}
		if f.StreamID != s.ID() {
			t.Fatalf("frame stream ID = %d, want %d", f.StreamID, s.ID())
		}
	default:
		t.Fatal("no RST_STREAM frame in Out() after ResetStream")
	}
}

func TestConnectionWindowOverflow(t *testing.T) {
	t.Parallel()

	m := NewMux(10)
	// Fill the window to maximum.
	if err := m.ApplyWindowUpdate(0, maxWindowSize-defaultInitialWindowSize); err != nil {
		t.Fatalf("fill to max: %v", err)
	}
	// One more byte must overflow.
	if err := m.ApplyWindowUpdate(0, 1); !errors.Is(err, ErrFlowControlLimit) {
		t.Fatalf("err = %v, want ErrFlowControlLimit", err)
	}
}
```

Create `example_test.go` (package `mux_test` for exported-API-only access):

```go
package mux_test

import (
	"fmt"

	mux "example.com/mux"
)

func ExampleNewMux() {
	m := mux.NewMux(100)
	s, err := m.OpenStream()
	if err != nil {
		panic(err)
	}
	fmt.Printf("stream %d: state=%s window=%d\n", s.ID(), s.State(), s.SendWindow())
	// Output: stream 1: state=idle window=65535
}

func ExampleMux_ApplyWindowUpdate() {
	m := mux.NewMux(100)
	before := m.ConnSendWindow()
	_ = m.ApplyWindowUpdate(0, 32768) // stream 0 = connection level
	fmt.Printf("before=%d after=%d\n", before, m.ConnSendWindow())
	// Output: before=65535 after=98303
}
```

## Review

The multiplexer is correct when illegal transitions are rejected rather than silently applied, when a zero-window send blocks instead of going negative, and when the connection never deadlocks under the read goroutine. The most common errors are using `if` instead of `for` around `flowCond.Wait()` — a spurious wakeup then lets `ConsumeSendWindow` decrement a window it should not have — and forgetting that `UpdateInitialWindowSize` applies the delta to the *current* window of every existing stream, not the initial value, and leaves the connection window alone. Confirm `Deliver` only ever takes a read lock and pushes into the buffered `recvCh`, so the read goroutine stays fast; confirm `ResetStream` transitions to closed before enqueuing the frame, so local state never lags the wire; confirm a reused or out-of-range window increment is rejected with `ErrFlowControlLimit`. Running the suite under `-race` is what proves the `sync.Cond` hand-off and the cleanup goroutine are free of data races.

## Resources

- [RFC 9113 §5.1 — Stream States](https://httpwg.org/specs/rfc9113.html#StreamStates): the normative state machine diagram and transition rules.
- [RFC 9113 §5.2 — Flow Control](https://httpwg.org/specs/rfc9113.html#FlowControl): the semantics of per-stream and connection-level windows.
- [RFC 9113 §6.9 — WINDOW_UPDATE](https://httpwg.org/specs/rfc9113.html#WINDOW_UPDATE): frame format, increment rules, and error conditions.
- [pkg.go.dev/sync#Cond](https://pkg.go.dev/sync#Cond): `sync.Cond`, including the `for`-loop `Wait` pattern this code relies on.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-receive-window.md](02-receive-window.md)
