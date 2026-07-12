# Exercise 3: Inbound Concurrent-Stream Limiter

`OpenStream` in the first exercise enforced the *peer's* concurrency limit on the streams *we* open. This exercise builds the mirror image: the limit *we* advertise, enforced on the streams the *peer* opens toward us. The hard part is the refusal — an excess stream is rejected with RST_STREAM carrying REFUSED_STREAM, a deliberately retry-safe code, while a malformed stream ID is a connection-level PROTOCOL_ERROR.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
inbound/
  go.mod
  limiter.go             Limiter, NewLimiter, Accept, Close, OpenCount, SetMax
  limiter_test.go        admit to the limit, refuse the excess, free a slot, reject bad IDs
  cmd/demo/main.go       admit two streams, refuse a third, close one, refuse a bad ID
```

- Files: `limiter.go`, `limiter_test.go`, `cmd/demo/main.go`.
- Implement: `Limiter` with `Accept(id) error`, `Close(id)`, `OpenCount() int`, and `SetMax(n)`.
- Test: streams are admitted up to the limit and the next is refused with `ErrRefusedStream`; closing a stream frees a slot; an even or non-increasing ID is rejected with `ErrProtocol`; a refused stream still consumes its ID.
- Verify: `go test -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### Two different rejections for two different faults

`SETTINGS_MAX_CONCURRENT_STREAMS` (RFC 9113 §5.1.2) is advertised by each endpoint and bounds the streams the peer may have open at once. When the peer opens one stream too many, that is not a protocol violation — the peer simply asked for more than we will grant right now — so the right answer is RST_STREAM with REFUSED_STREAM. That code carries a precise meaning: the stream was *not processed at all*, so the peer may safely retry it on a fresh connection. Answering with PROTOCOL_ERROR instead would imply the peer is broken and invites it to tear down the whole connection; silently dropping the stream would leave the peer waiting forever. The distinction is the entire reason REFUSED_STREAM exists.

A malformed stream ID is a different fault and gets a different answer. Client-initiated streams must use odd, non-zero IDs, and every new stream ID the peer opens must be strictly greater than any it has opened before (RFC 9113 §5.1.1) — IDs are monotonic and never reused. An even ID, a zero ID, or an ID less than or equal to one already seen is a malformed connection-level event: PROTOCOL_ERROR, signalled with `ErrProtocol`. The limiter checks ID validity first, then the concurrency limit, because a bad ID is a connection error regardless of how many streams are open.

### Why a refused stream still consumes its ID

The subtle bookkeeping rule is that refusing a stream does not un-spend its ID. The peer opened stream N; even though we immediately reset it, N is now used, and the next stream the peer opens must still be greater than N. So `Accept` advances the "highest seen" marker for any well-formed ID — accepted or refused alike — and only adds to the open set on acceptance. A protocol error (bad parity or a non-increasing ID) does *not* advance the marker, because such an event never legitimately opened a stream. Getting this wrong lets a peer's later valid stream be mis-rejected as non-increasing, or lets a refused ID be reused.

Create `limiter.go`:

```go
// Package inbound enforces an endpoint's advertised
// SETTINGS_MAX_CONCURRENT_STREAMS on streams opened by the peer (RFC 9113
// §5.1.2), refusing the excess with REFUSED_STREAM and rejecting malformed
// stream IDs with PROTOCOL_ERROR.
package inbound

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors returned by Accept. ErrRefusedStream maps to a stream-level
// RST_STREAM(REFUSED_STREAM); ErrProtocol maps to a connection-level
// PROTOCOL_ERROR.
var (
	ErrRefusedStream = errors.New("inbound: stream refused (max concurrent reached)")
	ErrProtocol      = errors.New("inbound: protocol error")
)

// Limiter admits peer-initiated streams up to a configurable maximum. It models
// a server receiving client streams, so it expects odd, strictly increasing
// stream IDs. It is safe for concurrent use.
type Limiter struct {
	mu       sync.Mutex
	max      uint32
	open     map[uint32]struct{}
	lastSeen uint32 // highest well-formed stream ID observed so far
}

// NewLimiter creates a Limiter that admits at most max concurrent peer streams.
func NewLimiter(max uint32) *Limiter {
	return &Limiter{max: max, open: make(map[uint32]struct{})}
}

// Accept admits a stream the peer opened with a HEADERS frame.
//
//   - A zero or even ID, or an ID not strictly greater than the highest seen,
//     is a connection-level PROTOCOL_ERROR (ErrProtocol). The state is unchanged.
//   - A well-formed ID that would exceed the concurrency limit is refused with
//     ErrRefusedStream; the ID is still consumed (the highest-seen marker
//     advances) but the stream is not opened.
//   - Otherwise the stream is opened and Accept returns nil.
func (l *Limiter) Accept(id uint32) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if id == 0 || id%2 == 0 {
		return fmt.Errorf("%w: stream %d is not a valid client ID (must be odd, non-zero)", ErrProtocol, id)
	}
	if id <= l.lastSeen {
		return fmt.Errorf("%w: stream %d not greater than last seen %d", ErrProtocol, id, l.lastSeen)
	}

	// The ID is well-formed and is now spent regardless of the outcome.
	l.lastSeen = id

	if uint32(len(l.open)) >= l.max {
		return fmt.Errorf("%w: stream %d (limit=%d)", ErrRefusedStream, id, l.max)
	}
	l.open[id] = struct{}{}
	return nil
}

// Close releases the slot held by an admitted stream. It is idempotent: closing
// an unknown or already-closed stream is a no-op.
func (l *Limiter) Close(id uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.open, id)
}

// OpenCount returns the number of currently admitted streams.
func (l *Limiter) OpenCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.open)
}

// SetMax changes the advertised limit, modelling a SETTINGS_MAX_CONCURRENT_STREAMS
// update. Lowering it below the current open count is legal: existing streams
// are allowed to finish, but new streams are refused until the count drops
// below the new limit (RFC 9113 §5.1.2).
func (l *Limiter) SetMax(max uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.max = max
}
```

`Accept` reads the clock of stream IDs and the size of the open set in one critical section, so two concurrent HEADERS frames can never both slip into the last free slot. The order of the three checks is the contract: parity and monotonicity (connection errors) before the limit (a stream error), and the highest-seen marker advanced only after the ID is proven well-formed.

### The runnable demo

The demo runs a limiter with room for two streams. The peer opens streams 1 and 3 (admitted), then 5 (refused — at the limit). It closes stream 1, freeing a slot, and opens 7 (admitted). Finally it sends an even ID and then a stale ID, both PROTOCOL_ERROR.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	inbound "example.com/inbound"
)

func classify(err error) string {
	switch {
	case err == nil:
		return "accepted"
	case errors.Is(err, inbound.ErrRefusedStream):
		return "REFUSED_STREAM"
	case errors.Is(err, inbound.ErrProtocol):
		return "PROTOCOL_ERROR"
	default:
		return "error: " + err.Error()
	}
}

func main() {
	lim := inbound.NewLimiter(2)

	for _, id := range []uint32{1, 3, 5} {
		err := lim.Accept(id)
		fmt.Printf("HEADERS stream %d -> %s (open=%d)\n", id, classify(err), lim.OpenCount())
	}

	lim.Close(1)
	fmt.Printf("close stream 1 (open=%d)\n", lim.OpenCount())

	for _, id := range []uint32{7, 8, 3} {
		err := lim.Accept(id)
		fmt.Printf("HEADERS stream %d -> %s (open=%d)\n", id, classify(err), lim.OpenCount())
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
HEADERS stream 1 -> accepted (open=1)
HEADERS stream 3 -> accepted (open=2)
HEADERS stream 5 -> REFUSED_STREAM (open=2)
close stream 1 (open=1)
HEADERS stream 7 -> accepted (open=2)
HEADERS stream 8 -> PROTOCOL_ERROR (open=2)
HEADERS stream 3 -> PROTOCOL_ERROR (open=2)
```

Stream 5 is refused but its ID is still spent, so when stream 7 arrives it is correctly greater than the highest seen. The even ID 8 is a protocol error and does *not* advance the marker, so the later stale ID 3 is still judged against 7.

### Tests

The tests pin admission, refusal, slot reuse, and ID validation. `TestAdmitUpToLimitThenRefuse` fills the limiter and asserts the next stream is `ErrRefusedStream`. `TestRefusedStreamStillConsumesID` refuses a stream and then proves a lower ID is rejected as non-increasing while the next-higher ID (after a close) is admitted. `TestCloseFreesSlot` admits to the limit, closes one, and admits the next. `TestMalformedIDs` checks even, zero, and non-increasing IDs all return `ErrProtocol`. `TestSetMaxLowerThenRaise` walks a SETTINGS change in both directions.

Create `limiter_test.go`:

```go
package inbound

import (
	"errors"
	"sync"
	"testing"
)

func TestAdmitUpToLimitThenRefuse(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(3)
	for _, id := range []uint32{1, 3, 5} {
		if err := lim.Accept(id); err != nil {
			t.Fatalf("Accept(%d) = %v, want nil", id, err)
		}
	}
	if got := lim.OpenCount(); got != 3 {
		t.Fatalf("OpenCount = %d, want 3", got)
	}
	if err := lim.Accept(7); !errors.Is(err, ErrRefusedStream) {
		t.Fatalf("Accept(7) = %v, want ErrRefusedStream", err)
	}
	// The refused stream must not have been added to the open set.
	if got := lim.OpenCount(); got != 3 {
		t.Fatalf("OpenCount after refusal = %d, want 3", got)
	}
}

func TestRefusedStreamStillConsumesID(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(1)
	if err := lim.Accept(1); err != nil {
		t.Fatalf("Accept(1): %v", err)
	}
	// At the limit: stream 3 is refused, but ID 3 is now spent.
	if err := lim.Accept(3); !errors.Is(err, ErrRefusedStream) {
		t.Fatalf("Accept(3) = %v, want ErrRefusedStream", err)
	}
	// A later stream with an ID <= 3 is now a protocol error.
	lim.Close(1)
	if err := lim.Accept(3); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Accept(3) again = %v, want ErrProtocol", err)
	}
	// The next-higher ID is admitted into the freed slot.
	if err := lim.Accept(5); err != nil {
		t.Fatalf("Accept(5) = %v, want nil", err)
	}
}

func TestCloseFreesSlot(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(2)
	if err := lim.Accept(1); err != nil {
		t.Fatalf("Accept(1): %v", err)
	}
	if err := lim.Accept(3); err != nil {
		t.Fatalf("Accept(3): %v", err)
	}
	if err := lim.Accept(5); !errors.Is(err, ErrRefusedStream) {
		t.Fatalf("Accept(5) = %v, want ErrRefusedStream", err)
	}
	lim.Close(1)
	if got := lim.OpenCount(); got != 1 {
		t.Fatalf("OpenCount after Close = %d, want 1", got)
	}
	if err := lim.Accept(7); err != nil {
		t.Fatalf("Accept(7) after Close = %v, want nil", err)
	}
}

func TestMalformedIDs(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(10)
	if err := lim.Accept(0); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Accept(0) = %v, want ErrProtocol", err)
	}
	if err := lim.Accept(2); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Accept(2) even = %v, want ErrProtocol", err)
	}
	if err := lim.Accept(5); err != nil {
		t.Fatalf("Accept(5): %v", err)
	}
	if err := lim.Accept(3); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Accept(3) non-increasing = %v, want ErrProtocol", err)
	}
	// A malformed ID must not advance the marker: 7 is still admissible.
	if err := lim.Accept(7); err != nil {
		t.Fatalf("Accept(7): %v", err)
	}
}

func TestSetMaxLowerThenRaise(t *testing.T) {
	t.Parallel()

	lim := NewLimiter(3)
	for _, id := range []uint32{1, 3, 5} {
		if err := lim.Accept(id); err != nil {
			t.Fatalf("Accept(%d): %v", id, err)
		}
	}
	// Lower the limit below the open count: existing streams remain, new ones
	// are refused.
	lim.SetMax(1)
	if got := lim.OpenCount(); got != 3 {
		t.Fatalf("OpenCount = %d, want 3 (existing streams allowed to finish)", got)
	}
	if err := lim.Accept(7); !errors.Is(err, ErrRefusedStream) {
		t.Fatalf("Accept(7) under lowered limit = %v, want ErrRefusedStream", err)
	}
	// Drain below the new limit, then a stream is admissible again.
	lim.Close(1)
	lim.Close(3)
	lim.Close(5)
	if err := lim.Accept(9); err != nil {
		t.Fatalf("Accept(9) after drain = %v, want nil", err)
	}
	// Raise the limit and admit more.
	lim.SetMax(5)
	if err := lim.Accept(11); err != nil {
		t.Fatalf("Accept(11) after raise = %v, want nil", err)
	}
}

func TestConcurrentAcceptRespectsLimit(t *testing.T) {
	t.Parallel()

	const max = 50
	lim := NewLimiter(max)

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	// 200 distinct odd IDs raced through Accept; never more than max admitted.
	for i := 0; i < 200; i++ {
		id := uint32(2*i + 1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := lim.Accept(id); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if admitted > max {
		t.Fatalf("admitted %d streams, want <= %d", admitted, max)
	}
	if got := lim.OpenCount(); got != admitted {
		t.Fatalf("OpenCount = %d, want %d (matches admitted)", got, admitted)
	}
}
```

The concurrent test does not assert an exact admitted count — racing goroutines may each see a different `lastSeen`, and a well-formed ID that loses the monotonicity race is rejected as a protocol error — but it does pin the safety property: the open set never exceeds `max`, and `OpenCount` always matches the number of `nil` returns.

## Review

The limiter is correct when the excess stream is refused with REFUSED_STREAM rather than dropped or treated as a protocol fault, when a malformed ID is a connection error, and when a refused but well-formed ID is still spent so monotonicity holds afterward. The most common errors are using PROTOCOL_ERROR for an over-limit stream (which is retry-hostile and may kill the connection), failing to advance the highest-seen marker on a refusal (which lets the ID be reused or mis-rejects a later valid stream), and checking the concurrency limit before validating the ID (a bad ID is a connection error no matter how many streams are open). Confirm the three checks run in the order parity, monotonicity, limit, and that `SetMax` lowering below the open count never force-closes a stream. The concurrent test under `-race` is what proves the single critical section actually serializes admission.

## Resources

- [RFC 9113 §5.1.2 — Stream Concurrency](https://httpwg.org/specs/rfc9113.html#StreamConcurrency): the `SETTINGS_MAX_CONCURRENT_STREAMS` limit and the REFUSED_STREAM response.
- [RFC 9113 §5.1.1 — Stream Identifiers](https://httpwg.org/specs/rfc9113.html#StreamIdentifiers): odd client IDs, monotonic allocation, and the no-reuse rule.
- [RFC 9113 §7 — Error Codes](https://httpwg.org/specs/rfc9113.html#ErrorCodes): the meanings of REFUSED_STREAM and PROTOCOL_ERROR and when each applies.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-receive-window.md](02-receive-window.md) | Next: [04-weighted-scheduler.md](04-weighted-scheduler.md)
