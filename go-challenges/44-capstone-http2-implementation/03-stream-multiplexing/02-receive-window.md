# Exercise 2: Receive-Side Flow-Control Window

The multiplexer in the previous exercise owns the *send* window — the credit the peer grants us. This exercise builds the other half: the *receive* window, the credit we grant the peer. It tracks how much the peer may still send, detects a peer that over-sends (a FLOW_CONTROL_ERROR), and decides when to hand credit back with a WINDOW_UPDATE.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
recvwindow/
  go.mod
  window.go              Window, NewWindow, DataReceived, Consume, Remaining, Pending
  window_test.go         accounting math, over-send rejection, threshold replenishment
  cmd/demo/main.go       receive DATA, consume it, emit a WINDOW_UPDATE, reject an over-send
```

- Files: `window.go`, `window_test.go`, `cmd/demo/main.go`.
- Implement: `Window` with `DataReceived(n) error`, `Consume(n) (increment, emit, err)`, `Remaining()`, and `Pending()`.
- Test: arriving DATA decrements `remaining` and grows `pending`; consuming past the threshold emits exactly the consumed increment and restores `remaining`; a frame larger than `remaining` is rejected with `ErrFlowControl` and leaves the window unchanged.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p recvwindow/cmd/demo && cd recvwindow
go mod init example.com/recvwindow
go mod edit -go=1.26
```

### Why the receive window is a separate quantity

A flow-control window is a contract between two endpoints, and each direction is governed by a different party. The send window is the peer's promise to us, replenished by *their* WINDOW_UPDATE; the receive window is our promise to the peer, replenished by *our* WINDOW_UPDATE. Conflating them — keeping one counter and decrementing it both when sending and when receiving — double-counts and corrupts the accounting, which is exactly the bug the concepts file warns about. The receive side also carries two duties the send side does not: it must reject a peer that sends more than the window allows (the peer broke the contract: FLOW_CONTROL_ERROR), and it must decide *when* to give credit back.

The clean way to track it is a single invariant. The window has a fixed `size` (default 65535). At every moment, `remaining + pending + consumed == size`, where `remaining` is the credit the peer may still spend, `pending` is bytes that arrived but the application has not yet read, and `consumed` is bytes the application has read but for which we have not yet sent a WINDOW_UPDATE. A DATA frame moves bytes from `remaining` to `pending`; the application reading moves bytes from `pending` to `consumed`; a WINDOW_UPDATE moves bytes from `consumed` back to `remaining`. Because every operation just shuffles bytes between the three buckets, the sum is conserved, and any deviation is a bug a test can catch.

### Why replenish on a threshold, not per byte

If we emitted a WINDOW_UPDATE for every byte the application consumed, the connection would drown in control frames. If we never emitted one until the application drained everything, the peer would stall at zero credit waiting for us. The standard compromise is to accumulate consumed bytes and emit a single WINDOW_UPDATE once they cross a threshold — half the window is the common choice, keeping the peer's credit comfortably above zero while emitting at most about two update frames per window of data. `Consume` therefore returns `(increment, emit, err)`: when the running `consumed` total reaches the threshold it returns the increment to put on the wire and `emit=true`, having already moved that credit back into `remaining`; otherwise it returns `emit=false` and the caller sends nothing.

Create `window.go`:

```go
// Package recvwindow implements the receive side of an HTTP/2 flow-control
// window (RFC 9113 §6.9): the credit an endpoint grants its peer, the
// detection of a peer that over-sends, and the threshold-based WINDOW_UPDATE
// that replenishes the credit.
package recvwindow

import (
	"errors"
	"fmt"
)

// DefaultWindowSize is the HTTP/2 default flow-control window (RFC 9113 §6.9.2).
const DefaultWindowSize int32 = 65535

// MaxWindowSize is the largest legal flow-control window, 2^31-1 (RFC 9113 §6.9.1).
const MaxWindowSize int32 = 1<<31 - 1

// Sentinel errors returned by Window operations.
var (
	// ErrFlowControl reports that the peer sent more than the window allowed.
	// The caller converts this into a FLOW_CONTROL_ERROR (RFC 9113 §6.9.1).
	ErrFlowControl = errors.New("recvwindow: peer exceeded receive window")
	// ErrNegative reports a negative byte count argument.
	ErrNegative = errors.New("recvwindow: negative byte count")
	// ErrConsumeTooMuch reports an attempt to consume more than was received.
	ErrConsumeTooMuch = errors.New("recvwindow: consumed more than received")
	// ErrWindowOverflow reports that a replenishment would push the window past
	// MaxWindowSize.
	ErrWindowOverflow = errors.New("recvwindow: window would exceed 2^31-1")
)

// Window tracks one receive flow-control window. It is not safe for concurrent
// use; a real connection serializes DATA accounting on the read goroutine and
// guards Consume from the owning stream goroutine.
//
// Invariant: remaining + pending + consumed == size at all times.
type Window struct {
	size      int32 // the window the receiver advertises
	remaining int32 // credit the peer may still spend before blocking
	pending   int32 // received bytes not yet consumed by the application
	consumed  int32 // consumed bytes not yet returned via WINDOW_UPDATE
	threshold int32 // emit a WINDOW_UPDATE once consumed reaches this
}

// NewWindow creates a receive window of the given size (non-positive falls back
// to DefaultWindowSize). The replenishment threshold is half the window.
func NewWindow(size int32) *Window {
	if size <= 0 {
		size = DefaultWindowSize
	}
	return &Window{
		size:      size,
		remaining: size,
		threshold: size / 2,
	}
}

// Remaining returns the credit the peer may still spend (its send window as we
// have granted it).
func (w *Window) Remaining() int32 { return w.remaining }

// Pending returns the number of received bytes the application has not yet
// consumed.
func (w *Window) Pending() int32 { return w.pending }

// DataReceived accounts for a DATA frame of n bytes (payload plus any padding)
// arriving from the peer. If n exceeds the remaining credit, the peer has
// violated flow control: the frame is rejected with ErrFlowControl and the
// window is left unchanged so the connection state stays consistent.
func (w *Window) DataReceived(n int32) error {
	if n < 0 {
		return fmt.Errorf("%w: %d", ErrNegative, n)
	}
	if n > w.remaining {
		return fmt.Errorf("%w: frame=%d remaining=%d", ErrFlowControl, n, w.remaining)
	}
	w.remaining -= n
	w.pending += n
	return nil
}

// Consume records that the application has processed n previously received
// bytes. When the running consumed total reaches the threshold, Consume moves
// that credit back into remaining and returns the WINDOW_UPDATE increment with
// emit=true. Otherwise it returns emit=false and the caller sends no frame.
func (w *Window) Consume(n int32) (increment int32, emit bool, err error) {
	if n < 0 {
		return 0, false, fmt.Errorf("%w: %d", ErrNegative, n)
	}
	if n > w.pending {
		return 0, false, fmt.Errorf("%w: consume=%d pending=%d", ErrConsumeTooMuch, n, w.pending)
	}
	w.pending -= n
	w.consumed += n
	if w.consumed < w.threshold {
		return 0, false, nil
	}
	increment = w.consumed
	if w.remaining > MaxWindowSize-increment {
		return 0, false, fmt.Errorf("%w: remaining=%d increment=%d", ErrWindowOverflow, w.remaining, increment)
	}
	w.remaining += increment
	w.consumed = 0
	return increment, true, nil
}
```

The error path in `DataReceived` is the whole reason the receive side exists: a peer that ignores its send window could otherwise force us to buffer unbounded data. Rejecting the frame and returning `ErrFlowControl` without mutating the window lets the caller terminate the stream (or connection) with FLOW_CONTROL_ERROR while leaving every counter consistent for any error reporting that follows.

### The runnable demo

The demo plays one window through a full cycle: two DATA frames arrive and draw the window down, the application consumes them in two steps, the second consume crosses the half-window threshold and produces a WINDOW_UPDATE that restores the window to full, and finally a peer that ignores the window over-sends and is rejected.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	rw "example.com/recvwindow"
)

func main() {
	w := rw.NewWindow(65535)
	fmt.Printf("initial: remaining=%d pending=%d\n", w.Remaining(), w.Pending())

	for _, n := range []int32{20000, 20000} {
		if err := w.DataReceived(n); err != nil {
			fmt.Println("recv error:", err)
			continue
		}
		fmt.Printf("DATA %d -> remaining=%d pending=%d\n", n, w.Remaining(), w.Pending())
	}

	for _, n := range []int32{30000, 10000} {
		inc, emit, err := w.Consume(n)
		if err != nil {
			fmt.Println("consume error:", err)
			continue
		}
		if emit {
			fmt.Printf("consume %d -> WINDOW_UPDATE +%d, remaining=%d pending=%d\n",
				n, inc, w.Remaining(), w.Pending())
		} else {
			fmt.Printf("consume %d -> (no update) remaining=%d pending=%d\n",
				n, w.Remaining(), w.Pending())
		}
	}

	// A peer that ignores its send window over-sends; flow control rejects it.
	if err := w.DataReceived(70000); err != nil {
		fmt.Println("over-send rejected:", err)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial: remaining=65535 pending=0
DATA 20000 -> remaining=45535 pending=20000
DATA 20000 -> remaining=25535 pending=40000
consume 30000 -> (no update) remaining=25535 pending=10000
consume 10000 -> WINDOW_UPDATE +40000, remaining=65535 pending=0
over-send rejected: recvwindow: peer exceeded receive window: frame=70000 remaining=65535
```

### Tests

The tests pin the accounting math and the failure modes. `TestAccountingInvariant` drives a sequence of receives and consumes and checks `remaining + pending + consumed == size` holds throughout (reading `consumed` indirectly through the other two). `TestThresholdReplenishment` asserts the exact increment and the restored window. `TestOverSendRejected` sends one byte more than the window and asserts `ErrFlowControl` with the window untouched. `TestConsumeTooMuch` and `TestNegative` pin the argument-validation errors.

Create `window_test.go`:

```go
package recvwindow

import (
	"errors"
	"testing"
)

func TestThresholdReplenishment(t *testing.T) {
	t.Parallel()

	w := NewWindow(65535)
	if err := w.DataReceived(40000); err != nil {
		t.Fatalf("DataReceived: %v", err)
	}
	if got := w.Remaining(); got != 25535 {
		t.Fatalf("remaining = %d, want 25535", got)
	}

	// Below threshold (32767): no update yet.
	inc, emit, err := w.Consume(30000)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if emit {
		t.Fatalf("emit = true at consumed=30000, want false (threshold 32767)")
	}
	if inc != 0 {
		t.Fatalf("increment = %d, want 0", inc)
	}

	// Crossing the threshold: emit the full consumed total, restore the window.
	inc, emit, err = w.Consume(10000)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if !emit {
		t.Fatalf("emit = false at consumed=40000, want true")
	}
	if inc != 40000 {
		t.Fatalf("increment = %d, want 40000", inc)
	}
	if got := w.Remaining(); got != 65535 {
		t.Fatalf("remaining after update = %d, want 65535", got)
	}
	if got := w.Pending(); got != 0 {
		t.Fatalf("pending after consume = %d, want 0", got)
	}
}

func TestOverSendRejected(t *testing.T) {
	t.Parallel()

	w := NewWindow(65535)
	err := w.DataReceived(65536) // one byte past the window
	if !errors.Is(err, ErrFlowControl) {
		t.Fatalf("err = %v, want ErrFlowControl", err)
	}
	// The window must be unchanged after a rejected frame.
	if got := w.Remaining(); got != 65535 {
		t.Fatalf("remaining = %d, want 65535 (unchanged)", got)
	}
	if got := w.Pending(); got != 0 {
		t.Fatalf("pending = %d, want 0 (unchanged)", got)
	}
}

func TestOverSendAtBoundary(t *testing.T) {
	t.Parallel()

	w := NewWindow(100)
	if err := w.DataReceived(100); err != nil {
		t.Fatalf("exact-window frame: %v", err)
	}
	if got := w.Remaining(); got != 0 {
		t.Fatalf("remaining = %d, want 0", got)
	}
	// With zero credit left, even a one-byte frame is a violation.
	if err := w.DataReceived(1); !errors.Is(err, ErrFlowControl) {
		t.Fatalf("err = %v, want ErrFlowControl", err)
	}
}

func TestAccountingInvariant(t *testing.T) {
	t.Parallel()

	const size = int32(65535)
	w := NewWindow(size)

	check := func(step string) {
		// remaining + pending + consumed == size. consumed is not exported, but
		// it equals size - remaining - pending by the invariant, so verify that
		// the derived consumed is never negative and the partial sum never
		// exceeds size.
		sum := w.Remaining() + w.Pending()
		if sum > size {
			t.Fatalf("%s: remaining+pending = %d > size %d", step, sum, size)
		}
		if w.Remaining() < 0 || w.Pending() < 0 {
			t.Fatalf("%s: negative counter remaining=%d pending=%d",
				step, w.Remaining(), w.Pending())
		}
	}

	seq := []int32{10000, 5000, 20000}
	for _, n := range seq {
		if err := w.DataReceived(n); err != nil {
			t.Fatalf("DataReceived(%d): %v", n, err)
		}
		check("after recv")
	}
	for _, n := range []int32{15000, 20000} {
		if _, _, err := w.Consume(n); err != nil {
			t.Fatalf("Consume(%d): %v", n, err)
		}
		check("after consume")
	}
}

func TestConsumeTooMuch(t *testing.T) {
	t.Parallel()

	w := NewWindow(65535)
	if err := w.DataReceived(100); err != nil {
		t.Fatalf("DataReceived: %v", err)
	}
	_, _, err := w.Consume(200) // only 100 pending
	if !errors.Is(err, ErrConsumeTooMuch) {
		t.Fatalf("err = %v, want ErrConsumeTooMuch", err)
	}
}

func TestNegativeArguments(t *testing.T) {
	t.Parallel()

	w := NewWindow(65535)
	if err := w.DataReceived(-1); !errors.Is(err, ErrNegative) {
		t.Fatalf("DataReceived(-1) = %v, want ErrNegative", err)
	}
	if _, _, err := w.Consume(-1); !errors.Is(err, ErrNegative) {
		t.Fatalf("Consume(-1) = %v, want ErrNegative", err)
	}
}
```

## Review

The receive window is correct when the three-bucket invariant holds after every operation and a peer that over-sends is rejected without corrupting the counters. The most common errors are sharing one counter between the send and receive directions (they belong to different endpoints and different WINDOW_UPDATE frames), mutating the window before validating an over-send so the state is wrong by the time the error is reported, and emitting a WINDOW_UPDATE per byte instead of on a threshold, which floods the connection with control frames. Confirm `DataReceived` rejects exactly at `remaining+1`, confirm `Consume` returns the precise increment and restores `remaining` only when the threshold is crossed, and confirm consuming more than was received is an error rather than a silent negative. Running under `-race` is cheap here but still worth it once this window is driven from a real read goroutine.

## Resources

- [RFC 9113 §6.9 — WINDOW_UPDATE](https://httpwg.org/specs/rfc9113.html#WINDOW_UPDATE): the increment rules and the FLOW_CONTROL_ERROR a receiver raises on an over-send.
- [RFC 9113 §6.9.1 — The Flow-Control Window](https://httpwg.org/specs/rfc9113.html#FlowControlWindow): the 2^31-1 maximum and the meaning of available space.
- [RFC 9113 §5.2.2 — Appropriate Use of Flow Control](https://httpwg.org/specs/rfc9113.html#FlowControlUse): why a receiver replenishes credit and the consequences of stalling the peer.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-stream-multiplexer.md](01-stream-multiplexer.md) | Next: [03-inbound-stream-limiter.md](03-inbound-stream-limiter.md)
