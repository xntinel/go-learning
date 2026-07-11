# Exercise 3: GOAWAY and the Two-Phase Shutdown State Machine

`GOAWAY` is how an HTTP/2 endpoint says "I am closing — here is the last stream I will honor." This module builds `GoawayState`, the concurrency-safe state machine that records whether GOAWAY has been sent or received and with what last stream ID, and answers the one question the connection loop asks on every new request: may this stream be opened?

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
goaway.go              MaxStreamID, GoawayState, SendGoaway, ReceiveGoaway, MayOpenStream
cmd/
  demo/
    main.go            receive a GOAWAY, decide which streams may still open
goaway_test.go         may-open before/after receive, idempotent send, two-phase shutdown
```

- Files: `goaway.go`, `cmd/demo/main.go`, `goaway_test.go`.
- Implement: `GoawayState` with `SendGoaway`, `ReceiveGoaway`, `MayOpenStream`, `GoawaySent`, and `SentLastStreamID`.
- Test: `goaway_test.go` checks the may-open boundary before and after a received GOAWAY, the idempotency of `SendGoaway`, and the two-phase shutdown sequence.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p goaway-shutdown/cmd/demo && cd goaway-shutdown
go mod init example.com/goaway-shutdown
go mod edit -go=1.26
```

### What the last-stream-id promises

A `GOAWAY` frame's `LastStreamID` is a contract about what the sender did and did not process. Streams with IDs strictly above it are guaranteed untouched — the client may retry them on a fresh connection with no risk of double execution. Streams at or below it are in limbo: they may have completed, may be in flight, may have been dropped, and the client must reason about each from its own state. `GoawayState` tracks both directions of this contract independently, because a connection can be the sender and the receiver of a GOAWAY at the same time during a mutual shutdown. The receive side feeds `MayOpenStream`: once the peer has told us its last ID, we must not start a stream above that value, because the peer has promised not to process it — sending it anyway just wastes a round-trip before the inevitable refusal.

The send side is built for the two-phase shutdown of RFC 9113 §6.8. Phase 1 sends GOAWAY with `LastStreamID = MaxStreamID` (2^31-1, the largest legal stream ID) to announce intent while conceding that streams may still be arriving; after roughly a round-trip, phase 2 sends GOAWAY again with the real highest stream ID seen. `SendGoaway` is idempotent per phase — it returns true the first time and false afterward — so the connection loop can call it freely without sending two frames by accident, and `SentLastStreamID` reports the value the last successful send recorded. Every field is read and written under one mutex because the stream-creation goroutine and the shutdown goroutine touch this state concurrently, and a plain boolean read of `sentGoaway` from two goroutines is a data race with undefined results.

Create `goaway.go`:

```go
// Package goaway implements the HTTP/2 GOAWAY state machine (RFC 9113 §6.8):
// tracking whether GOAWAY has been sent or received and what the last stream
// ID was, and deciding whether a new stream may still be opened.
package goaway

import "sync"

// MaxStreamID is the sentinel used in the first phase of a graceful shutdown:
// send GOAWAY with LastStreamID = MaxStreamID to signal intent to close while
// acknowledging that streams may be in transit (RFC 9113 §6.8).
const MaxStreamID uint32 = 1<<31 - 1

// GoawayState tracks whether GOAWAY has been sent or received and what the
// last stream ID was. All methods are safe for concurrent use.
//
// The zero value is valid (no GOAWAY sent or received).
type GoawayState struct {
	mu          sync.Mutex
	sentGoaway  bool
	sentLastID  uint32
	recvdGoaway bool
	recvLastID  uint32
}

// SendGoaway records that we sent GOAWAY with the given last stream ID.
// Returns true on the first call; false on subsequent calls (idempotent).
func (g *GoawayState) SendGoaway(lastStreamID uint32) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sentGoaway {
		return false
	}
	g.sentGoaway = true
	g.sentLastID = lastStreamID
	return true
}

// ReceiveGoaway records that the peer sent GOAWAY with the given last stream ID.
func (g *GoawayState) ReceiveGoaway(lastStreamID uint32) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.recvdGoaway = true
	g.recvLastID = lastStreamID
}

// MayOpenStream reports whether the local side may open a new stream with
// the given ID. After receiving GOAWAY from the peer, streams with IDs above
// the peer's LastStreamID must not be started; they will not be processed.
func (g *GoawayState) MayOpenStream(streamID uint32) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.recvdGoaway {
		return true
	}
	return streamID <= g.recvLastID
}

// GoawaySent reports whether we have sent a GOAWAY frame.
func (g *GoawayState) GoawaySent() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sentGoaway
}

// SentLastStreamID returns the last stream ID from our GOAWAY frame.
// Meaningful only when GoawaySent() is true.
func (g *GoawayState) SentLastStreamID() uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sentLastID
}

// ResetForNextPhase clears the sent flag so the two-phase shutdown can send a
// second GOAWAY with the real last stream ID. RFC 9113 §6.8 permits a sender
// to emit GOAWAY more than once, each with a last ID no greater than the prior.
func (g *GoawayState) ResetForNextPhase() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sentGoaway = false
}
```

### The runnable demo

The demo plays the receive side: before any GOAWAY arrives every stream may open; after the peer sends GOAWAY with last ID 3, streams 1 and 3 may still open but 5 may not. It then plays the send side's two phases.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/goaway-shutdown"
)

func main() {
	var g goaway.GoawayState

	fmt.Println("before GOAWAY, may open stream 5:", g.MayOpenStream(5))

	g.ReceiveGoaway(3)
	fmt.Println("after GOAWAY(last=3), may open stream 1:", g.MayOpenStream(1))
	fmt.Println("after GOAWAY(last=3), may open stream 3:", g.MayOpenStream(3))
	fmt.Println("after GOAWAY(last=3), may open stream 5:", g.MayOpenStream(5))

	// Two-phase shutdown on the send side.
	g.SendGoaway(goaway.MaxStreamID)
	fmt.Println("phase 1 last id:", g.SentLastStreamID())
	g.ResetForNextPhase()
	g.SendGoaway(7)
	fmt.Println("phase 2 last id:", g.SentLastStreamID())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
before GOAWAY, may open stream 5: true
after GOAWAY(last=3), may open stream 1: true
after GOAWAY(last=3), may open stream 3: true
after GOAWAY(last=3), may open stream 5: false
phase 1 last id: 2147483647
phase 2 last id: 7
```

### Tests

`TestGoawayStateMayOpenStreamBeforeReceive` confirms everything is permitted until a GOAWAY arrives. `TestGoawayStateMayOpenStreamAfterReceive` pins the boundary: at or below the last ID is allowed, above it is refused. `TestGoawayStateSendIsIdempotent` proves a second `SendGoaway` without a reset returns false and preserves the first phase's last ID. `TestGoawayStateTwoPhaseShutdown` drives the phase-1-then-phase-2 sequence through `ResetForNextPhase`.

Create `goaway_test.go`:

```go
package goaway

import "testing"

func TestGoawayStateMayOpenStreamBeforeReceive(t *testing.T) {
	t.Parallel()
	var g GoawayState
	if !g.MayOpenStream(1) {
		t.Error("should allow all streams before GOAWAY received")
	}
	if !g.MayOpenStream(MaxStreamID) {
		t.Error("should allow MaxStreamID before GOAWAY received")
	}
}

func TestGoawayStateMayOpenStreamAfterReceive(t *testing.T) {
	t.Parallel()
	var g GoawayState
	g.ReceiveGoaway(3)
	if !g.MayOpenStream(1) {
		t.Error("stream 1 <= lastID 3: should be allowed")
	}
	if !g.MayOpenStream(3) {
		t.Error("stream 3 == lastID 3: should be allowed")
	}
	if g.MayOpenStream(5) {
		t.Error("stream 5 > lastID 3: should be refused")
	}
}

func TestGoawayStateSendIsIdempotent(t *testing.T) {
	t.Parallel()
	var g GoawayState
	if !g.SendGoaway(10) {
		t.Error("first SendGoaway should return true")
	}
	if !g.GoawaySent() {
		t.Error("GoawaySent() should be true after SendGoaway")
	}
	if g.SendGoaway(20) {
		t.Error("second SendGoaway should return false")
	}
	// First call's lastID is preserved.
	if got := g.SentLastStreamID(); got != 10 {
		t.Errorf("SentLastStreamID = %d, want 10", got)
	}
}

func TestGoawayStateTwoPhaseShutdown(t *testing.T) {
	t.Parallel()
	var g GoawayState

	// Phase 1: advertise MaxStreamID to stop new streams.
	if !g.SendGoaway(MaxStreamID) {
		t.Fatal("phase 1 SendGoaway should succeed")
	}
	if got := g.SentLastStreamID(); got != MaxStreamID {
		t.Fatalf("phase 1 last id = %d, want MaxStreamID", got)
	}

	// Phase 2: after a round-trip, send GOAWAY with the real last processed ID.
	g.ResetForNextPhase()
	if !g.SendGoaway(7) {
		t.Fatal("phase 2 SendGoaway should succeed after reset")
	}
	if got := g.SentLastStreamID(); got != 7 {
		t.Errorf("SentLastStreamID = %d, want 7 after phase 2", got)
	}
}
```

## Review

The state machine is correct when `MayOpenStream` uses the *received* last ID and the boundary is inclusive — stream IDs equal to the last ID are allowed, only strictly greater ones are refused, because the peer promised to process everything up to and including that value. The first mistake is comparing against the *sent* last ID instead of the received one; the two are independent and a mutual shutdown sets both. The second is reading `sentGoaway` or `recvLastID` without the mutex from the stream-creation goroutine, which races the shutdown goroutine's writes — every method here takes `g.mu`, and the race detector confirms it. The two-phase sequence is driven by the caller: `SendGoaway` models one phase and stays idempotent within it, and `ResetForNextPhase` is the explicit barrier between phase 1's `MaxStreamID` announcement and phase 2's real last ID.

## Resources

- [RFC 9113 §6.8 — GOAWAY](https://httpwg.org/specs/rfc9113.html#GOAWAY) — LastStreamID semantics, the two-phase shutdown, and the race-condition discussion.
- [RFC 9113 §5.1.1 — Stream Identifiers](https://httpwg.org/specs/rfc9113.html#StreamIdentifiers) — why stream IDs are monotonic and capped at 2^31-1.
- [sync.Mutex](https://pkg.go.dev/sync#Mutex) — the lock guarding the four state fields against the stream-creation/shutdown race.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-settings-negotiation.md](02-settings-negotiation.md) | Next: [04-ping-keepalive.md](04-ping-keepalive.md)
