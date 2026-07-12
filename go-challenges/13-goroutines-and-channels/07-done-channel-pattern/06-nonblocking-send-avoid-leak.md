# Exercise 6: Never Leak a Producer: Non-Blocking Send Against Done

A telemetry emitter on a request's hot path cannot afford to block when its consumer falls behind —
blocking the hot path to record a metric is a self-inflicted outage. The idiom is a non-blocking
send: offer the value, and if the channel is full, drop it and move on; if `done` is closed, stop.
This exercise builds that three-outcome send — delivered, cancelled, or dropped — and contrasts it
with the naive blocking send that leaks the producer when the consumer walks away.

## What you'll build

```text
sheddingsend/                      independent module: example.com/sheddingsend
  go.mod
  emitter.go                       type Emitter; TrySend three-way select; Dropped() counter
  cmd/
    demo/
      main.go                      runnable demo: emit into a small buffer, watch drops
  emitter_test.go                  delivers, drops-when-full, returns-on-done; -race on the counter
```

Files: `emitter.go`, `cmd/demo/main.go`, `emitter_test.go`.
Implement: an `Emitter` wrapping a buffered channel and a `TrySend(v) Outcome` that selects `{ch <- v; <-done; default}` returning `Delivered`, `Cancelled`, or `Dropped`, counting drops with `atomic.Int64`.
Test: a ready buffer delivers; a full buffer drops without blocking; a closed `done` returns `Cancelled` even with no reader; the drop counter is race-free.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/07-done-channel-pattern/06-nonblocking-send-avoid-leak/cmd/demo
cd go-solutions/13-goroutines-and-channels/07-done-channel-pattern/06-nonblocking-send-avoid-leak
```

### The three-way select

The whole exercise is one select with three cases:

```go
select {
case e.ch <- v:
	return Delivered
case <-e.done:
	return Cancelled
default:
	e.dropped.Add(1)
	return Dropped
}
```

The `default` case is what makes the send non-blocking. `select` evaluates its cases; if the send
can proceed (the buffer has room or a receiver is waiting) it is taken and returns `Delivered`; if
`done` is closed it returns `Cancelled`; otherwise — the buffer is full and there is no ready
receiver — the `default` runs immediately, increments the drop counter, and returns `Dropped`
without ever blocking. That is load-shedding: under backpressure the emitter sheds the value rather
than stalling the caller.

The ordering of the `done` and `default` cases matters for intent but not for a full buffer: when
the buffer is full and `done` is not yet closed, only `default` is ready, so the value is dropped.
When `done` is closed, both the `done` case and possibly `default` are ready, and `select` treats a
ready non-default case as taking priority over `default` — the Go spec says `default` runs only if
*no other* case is ready — so a closed `done` reliably yields `Cancelled` rather than a silent drop.
That is the desired behavior: once cancelled, stop reporting, do not silently shed.

The drop counter is read and written from many goroutines (every hot-path caller), so it is an
`atomic.Int64`, not a plain int guarded by nothing. `Dropped()` reads it with `Load()`.

Contrast the anti-pattern: a producer that does a bare `e.ch <- v`. When the consumer stops reading
and the buffer fills, that send blocks forever and the producer goroutine — and the hot path calling
it — is stuck. The non-blocking send trades completeness (some values are lost) for liveness (the
hot path never blocks), which is the right trade for telemetry.

Create `emitter.go`:

```go
package sheddingsend

import "sync/atomic"

// Outcome reports what TrySend did with a value.
type Outcome int

const (
	Delivered Outcome = iota // sent into the buffer or to a ready receiver
	Cancelled                // done was closed; the emitter is shutting down
	Dropped                  // buffer full and no receiver: value shed
)

func (o Outcome) String() string {
	switch o {
	case Delivered:
		return "delivered"
	case Cancelled:
		return "cancelled"
	case Dropped:
		return "dropped"
	default:
		return "unknown"
	}
}

// Emitter offers values on a buffered channel without ever blocking the caller.
// It is safe for concurrent use by many producers.
type Emitter struct {
	ch      chan int
	done    <-chan struct{}
	dropped atomic.Int64
}

// NewEmitter returns an Emitter writing to ch, cancelled when done closes.
func NewEmitter(ch chan int, done <-chan struct{}) *Emitter {
	return &Emitter{ch: ch, done: done}
}

// TrySend offers v without blocking. It returns Delivered if v was accepted,
// Cancelled if done is closed, or Dropped if the buffer is full (shedding load).
func (e *Emitter) TrySend(v int) Outcome {
	select {
	case e.ch <- v:
		return Delivered
	case <-e.done:
		return Cancelled
	default:
		e.dropped.Add(1)
		return Dropped
	}
}

// Dropped reports how many values have been shed so far.
func (e *Emitter) Dropped() int64 {
	return e.dropped.Load()
}
```

### The runnable demo

The demo emits five values into a buffer of size two with no reader, so three are dropped.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/sheddingsend"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	ch := make(chan int, 2) // small buffer, no reader
	e := sheddingsend.NewEmitter(ch, done)

	for i := range 5 {
		fmt.Printf("emit %d: %s\n", i, e.TrySend(i))
	}
	fmt.Printf("dropped total: %d\n", e.Dropped())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
emit 0: delivered
emit 1: delivered
emit 2: dropped
emit 3: dropped
emit 4: dropped
dropped total: 3
```

### Tests

`TestSendDeliversWhenReady` sends into a buffer with room and expects `Delivered`.
`TestSendDropsWhenFull` fills the buffer, then asserts the next send returns `Dropped` without
blocking and bumps the counter. `TestSendReturnsOnDone` uses an unbuffered channel with no reader
and a closed `done`, and asserts the send returns `Cancelled` rather than dropping — the closed
`done` case outranks `default`. `TestConcurrentTrySend` hammers `TrySend` from many goroutines to
exercise the atomic counter under `-race`.

Create `emitter_test.go`:

```go
package sheddingsend

import (
	"sync"
	"testing"
)

func TestSendDeliversWhenReady(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)
	ch := make(chan int, 1)
	e := NewEmitter(ch, done)

	if got := e.TrySend(7); got != Delivered {
		t.Fatalf("TrySend = %s, want delivered", got)
	}
	if v := <-ch; v != 7 {
		t.Fatalf("buffered value = %d, want 7", v)
	}
}

func TestSendDropsWhenFull(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)
	ch := make(chan int, 1)
	e := NewEmitter(ch, done)

	e.TrySend(1) // fills the buffer
	if got := e.TrySend(2); got != Dropped {
		t.Fatalf("TrySend into full buffer = %s, want dropped", got)
	}
	if n := e.Dropped(); n != 1 {
		t.Fatalf("Dropped() = %d, want 1", n)
	}
}

func TestSendReturnsOnDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	ch := make(chan int) // unbuffered, no reader
	e := NewEmitter(ch, done)
	close(done)

	if got := e.TrySend(9); got != Cancelled {
		t.Fatalf("TrySend after done closed = %s, want cancelled", got)
	}
	if n := e.Dropped(); n != 0 {
		t.Fatalf("Dropped() = %d after cancel, want 0", n)
	}
}

func TestConcurrentTrySend(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	defer close(done)
	ch := make(chan int, 4)
	e := NewEmitter(ch, done)

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.TrySend(1)
		}()
	}
	wg.Wait()

	// Drain whatever landed in the buffer.
	delivered := 0
	for len(ch) > 0 {
		<-ch
		delivered++
	}
	if int64(delivered)+e.Dropped() != 100 {
		t.Fatalf("delivered %d + dropped %d != 100", delivered, e.Dropped())
	}
}
```

## Review

The emitter is correct when every send resolves to exactly one of the three outcomes and never
blocks the caller. The delivers/drops tests pin the buffer-room and buffer-full paths; the
returns-on-done test proves the closed `done` case outranks `default`, so cancellation is never
silently misreported as a drop. The concurrent test's invariant — delivered plus dropped equals the
number of attempts — is what `-race` guards: if the counter were a plain int, the race detector would
fire and the arithmetic would not add up. The anti-pattern to remember is the bare `ch <- v` on a hot
path: it blocks the producer forever once the consumer stops, which is the leak this whole idiom
exists to avoid. Non-blocking send buys liveness at the cost of completeness — the right trade for
telemetry, the wrong one for data you must not lose.

## Resources

- [Go Language Spec: Select statements (default case)](https://go.dev/ref/spec#Select_statements)
- [pkg.go.dev: sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64)
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-pipeline-stage-cancellation.md](05-pipeline-stage-cancellation.md) | Next: [07-broadcast-tee-subscribers.md](07-broadcast-tee-subscribers.md)
