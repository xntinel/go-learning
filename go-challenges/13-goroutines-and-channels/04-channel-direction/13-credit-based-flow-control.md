# Exercise 13: Credit-Based Flow Control With Opposing Directional Channels

**Level: Advanced**

A streaming RPC link must bound how much data is in flight so a fast sender cannot overrun a slow receiver: this is exactly the window mechanism inside HTTP/2 and gRPC. The naive fix, an unbounded buffer, just moves the overrun into memory and turns a latency problem into an out-of-memory crash; a fixed-size buffer without a handshake either drops frames or blocks the sender at an arbitrary point with no accounting. This exercise builds the credit handshake that HTTP/2 uses: the receiver grants a fixed window of credits up front and returns one credit per frame it finishes, and the sender must hold a credit before it may transmit. The two channels flow in opposite directions, and the compiler enforces which way each one goes.

This module is self-contained: its own module, a `credit` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
credit/                      independent module: example.com/credit
  go.mod                     go 1.26
  credit.go                  Sender(out chan<- Frame, credits <-chan int, ...)
                             and Receiver(in <-chan Frame, credits chan<- int, ...)
  cmd/demo/main.go           runnable demo: an 8-frame stream over a window of 3
  credit_test.go             in-flight bound, exactly-once in-order delivery, leak-free
```

- Files: `credit.go`, `cmd/demo/main.go`, `credit_test.go`.
- Implement: `Sender(out chan<- Frame, credits <-chan int, frames []Frame)` and `Receiver(in <-chan Frame, credits chan<- int, window int, process func(Frame))`.
- Test: frames in flight never exceed `window`; every frame is delivered exactly once in Seq order; the link terminates with no leaked goroutine for windows from 1 up.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/credit/cmd/demo
cd ~/go-exercises/credit
go mod init example.com/credit
go get go.uber.org/goleak
```

### Two channels, opposite directions, one backpressure loop

The whole subsystem is two goroutines joined by two channels that point in opposite directions. Frames flow forward: `out chan<- Frame` for the sender is send-only, `in <-chan Frame` for the receiver is receive-only. Credits flow backward: `credits <-chan int` for the sender is receive-only, `credits chan<- int` for the receiver is send-only. The same underlying data channel is narrowed to send-only at the sender's call boundary and to receive-only at the receiver's; the same underlying credit channel is narrowed the other way for each side. Neither goroutine can operate its channels in the wrong direction, because a receive from a `chan<- int` or a send on a `<-chan Frame` does not compile. Direction is what makes "the sender feeds and closes data, the receiver feeds and closes credits" an unforgeable structural rule rather than a comment.

The protocol is a strict credit accounting:

1. The receiver grants `window` credits up front by sending `window` ones on the credit channel, then enters its receive loop.
2. Before transmitting frame `i`, the sender receives one credit. If none is available it blocks — that block is the backpressure.
3. The receiver receives a frame, runs `process`, and only then returns one credit. The credit's return is the acknowledgement that this frame has left the window.
4. When the data channel drains, the receiver stops and closes the credit channel; when the sender has sent every frame, it closes the data channel.

The invariant this guarantees is precise. Let `S` be the number of frames the sender has transmitted and `P` the number `process` has finished. Every transmission consumes a credit and every completion returns one, starting from a pool of `window`, so the number of unspent credits is always `window + P - A` where `A` is the number acquired, and that quantity can never go negative because you cannot receive from an empty channel. Since `S <= A` and each return trails a distinct earlier acquire, `S - P <= window` holds at every instant. Frames in flight — transmitted but not yet processed — never exceed the window. That is the property a fast sender cannot violate no matter how far ahead it races.

One buffering constraint falls out of step 1: the credit channel must be buffered to at least `window`, so the receiver can deposit the entire initial grant without blocking and then move on to receiving frames. An unbuffered credit channel deadlocks for any window above 1, because the receiver would be stuck mid-grant while the sender is stuck trying to transmit a frame the receiver has not yet reached. The data channel, by contrast, may be unbuffered; a credit, not a buffer slot, is what licenses a transmission.

Create `credit.go`:

```go
package credit

// Frame is one unit of data on the streaming link.
type Frame struct{ Seq int }

// Sender transmits each frame only after acquiring a credit. out is send-only:
// the sender feeds it and closes it once every frame has been transmitted.
// credits is receive-only: the sender drains one credit before each frame and
// can never send on it. The compiler enforces both directions.
func Sender(out chan<- Frame, credits <-chan int, frames []Frame) {
	defer close(out)
	for _, f := range frames {
		<-credits // block until the receiver has granted a credit
		out <- f
	}
}

// Receiver grants an initial window of credits, then processes each incoming
// frame in Seq order and returns one credit per processed frame. in is
// receive-only; credits is send-only and is closed when in drains. credits must
// be buffered to at least window so the up-front grant never blocks.
func Receiver(in <-chan Frame, credits chan<- int, window int, process func(Frame)) {
	defer close(credits)
	for range window {
		credits <- 1 // grant the whole window before any frame arrives
	}
	for f := range in {
		process(f)
		credits <- 1 // return the credit the sender spent on this frame
	}
}
```

### The runnable demo

The demo streams eight frames over a window of three. Only `process` prints, and the single receiver goroutine calls it in Seq order, so the output is deterministic regardless of how the sender and receiver interleave underneath. The credit channel is buffered to the window; the data channel is unbuffered.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/credit"
)

func main() {
	const window = 3
	frames := make([]credit.Frame, 8)
	for i := range frames {
		frames[i] = credit.Frame{Seq: i}
	}

	// The data channel is unbuffered: a frame is "in flight" from the instant
	// the sender transmits it until process returns. The credit channel is
	// buffered to the window so the receiver can grant the whole window up front.
	data := make(chan credit.Frame)
	credits := make(chan int, window)

	go credit.Sender(data, credits, frames)

	process := func(f credit.Frame) {
		fmt.Printf("processed frame %d\n", f.Seq)
	}
	credit.Receiver(data, credits, window, process)

	fmt.Printf("done: %d frames delivered in order, window=%d\n", len(frames), window)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed frame 0
processed frame 1
processed frame 2
processed frame 3
processed frame 4
processed frame 5
processed frame 6
processed frame 7
done: 8 frames delivered in order, window=3
```

### Tests

The tests instrument in-flight depth without weakening the API. A relay goroutine sits on the sender's output: because that channel is unbuffered, the relay's receive rendezvouses with the sender's transmission, so incrementing an atomic counter there marks a frame as in flight the instant it leaves the sender; `process` decrements it. `run` returns the processing order and the peak in-flight value. `TestInFlightNeverExceedsWindow` asserts that peak never exceeds `window` across windows 1 through 6 while every frame arrives exactly once in Seq order. `TestSynchronousWindowOne` pins the fully lock-step case: window 1 must show a peak of exactly 1 and still deliver all frames. `TestWindowLargerThanFrameCount` grants more credits than there are frames and asserts the surplus is harmless. `TestEmptyStreamTerminates` checks a zero-frame stream still closes cleanly. `TestMain` wraps every test in a goleak check, so any parked sender, receiver, or relay goroutine fails the run — this is where a window of 1 and a deadlock-free credit accounting are proven together.

Create `credit_test.go`:

```go
package credit

import (
	"fmt"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// run wires an instrumented credit link: the sender feeds raw, a relay counts
// each frame as in flight the instant the sender's send rendezvouses (raw is
// unbuffered) and forwards it to in, and the receiver processes in Seq order.
// It returns the order frames were processed in and the peak in-flight count.
func run(window, count int) (order []int, maxInFlight int64) {
	frames := make([]Frame, count)
	for i := range frames {
		frames[i] = Frame{Seq: i}
	}

	var inFlight, peak atomic.Int64
	bumpPeak := func(cur int64) {
		for {
			m := peak.Load()
			if cur <= m || peak.CompareAndSwap(m, cur) {
				return
			}
		}
	}

	raw := make(chan Frame)
	in := make(chan Frame)
	credits := make(chan int, window)

	go func() {
		for f := range raw {
			bumpPeak(inFlight.Add(1))
			in <- f
		}
		close(in)
	}()

	go Sender(raw, credits, frames)

	process := func(f Frame) {
		order = append(order, f.Seq)
		inFlight.Add(-1)
	}
	Receiver(in, credits, window, process) // blocks until in drains

	return order, peak.Load()
}

func assertInOrder(t *testing.T, order []int, count int) {
	t.Helper()
	if len(order) != count {
		t.Fatalf("processed %d frames, want %d", len(order), count)
	}
	for i, seq := range order {
		if seq != i {
			t.Fatalf("frame at position %d had Seq %d, want %d (out of order or duplicated)", i, seq, i)
		}
	}
}

func TestInFlightNeverExceedsWindow(t *testing.T) {
	const count = 20
	for window := 1; window <= 6; window++ {
		t.Run(fmt.Sprintf("window=%d", window), func(t *testing.T) {
			order, maxInFlight := run(window, count)
			assertInOrder(t, order, count)
			if maxInFlight > int64(window) {
				t.Fatalf("peak in-flight %d exceeded window %d", maxInFlight, window)
			}
			if maxInFlight < 1 {
				t.Fatalf("peak in-flight %d, want at least 1", maxInFlight)
			}
		})
	}
}

func TestSynchronousWindowOne(t *testing.T) {
	// window 1 is fully synchronous: at most one frame may be in flight, so the
	// link degenerates to lock-step send/process and must still deliver all.
	order, maxInFlight := run(1, 12)
	assertInOrder(t, order, 12)
	if maxInFlight != 1 {
		t.Fatalf("window=1 peak in-flight %d, want exactly 1", maxInFlight)
	}
}

func TestWindowLargerThanFrameCount(t *testing.T) {
	// A window wider than the stream grants more credits than there are frames;
	// the surplus credits are simply never spent, and every frame still arrives.
	order, maxInFlight := run(100, 5)
	assertInOrder(t, order, 5)
	if maxInFlight > 5 {
		t.Fatalf("peak in-flight %d exceeded frame count 5", maxInFlight)
	}
}

func TestEmptyStreamTerminates(t *testing.T) {
	order, _ := run(3, 0)
	assertInOrder(t, order, 0)
}
```

## Review

Correct here means three properties hold together: at most `window` frames are ever in flight, every frame is processed exactly once in Seq order, and no goroutine is left parked when the stream ends. All three come from the credit accounting, not from timing — the sender blocks on `<-credits` until a credit exists, the receiver returns a credit only after `process` completes, and the pool starts at `window`, so `S - P <= window` is an invariant of the handshake that no scheduling order can break. The tests prove it by measuring peak in-flight through an unbuffered relay that rendezvouses with each transmission, sweeping windows from 1 (lock-step) up, and gating every case on goleak so a deadlock or a leaked goroutine fails rather than hangs. The opposing channel directions make the ownership structural: the sender can only feed and close data, the receiver can only feed and close credits, so the "who closes what" mistakes that cause send-on-closed panics simply do not compile. This is the pattern that keeps a real streaming server from letting one fast client exhaust its memory: bounded in-flight data enforced by a returned-credit window, exactly as HTTP/2 flow control works.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) -- the send-only and receive-only parameter types that make each side's role in the loop unforgeable.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) -- the ownership and close conventions the two directional channels encode.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector that proves the link terminates cleanly at every window size, including 1.
- [`golang.org/x/net/http2`](https://pkg.go.dev/golang.org/x/net/http2) -- Go's HTTP/2 implementation, whose windowed flow control this credit handshake models down to the returned-credit acknowledgement.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [12-bridge-nested-result-streams.md](12-bridge-nested-result-streams.md) | Next: [../05-waitgroup/00-concepts.md](../05-waitgroup/00-concepts.md)
