# Exercise 2: Stop a for-select dispatch loop the bare break cannot

This is the single most common labeled-break production bug, in its natural
habitat. An event dispatcher reads typed events off a channel in a `for`-`select`
loop and switches on the event kind. A terminal `Shutdown` event must stop the
loop — but a bare `break` inside the `switch` only leaves the `switch`, and the
loop keeps running. You fix the hang with a labeled `break` on the `for`.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
eventloop/                 independent module: example.com/eventloop
  go.mod                   go 1.24
  eventloop.go             Kind, Event, Dispatcher; Run(ctx, events) stops on Shutdown
  cmd/
    demo/
      main.go              runnable demo: dispatch a few events, then Shutdown
  eventloop_test.go        stops on Shutdown, ignores trailing events, cancels on ctx
```

- Files: `eventloop.go`, `cmd/demo/main.go`, `eventloop_test.go`.
- Implement: a `Dispatcher.Run(ctx, <-chan Event)` that processes `Message` events, ignores `Heartbeat`, stops on a `Shutdown` event via a labeled `break`, and returns `ctx.Err()` if the context is cancelled first.
- Test: feed N messages, then a `Shutdown`, then trailing messages; assert only the first N were processed and `Run` returned; assert a cancelled context returns `context.Canceled`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/eventloop/cmd/demo
cd ~/go-exercises/eventloop
go mod init example.com/eventloop
go mod edit -go=1.24
```

### Why a bare break here is a silent hang

The loop has the classic server shape: `for { select { case ev := <-events: switch ev.Kind { ... } } }`. There are two `break` targets nested inside each
other that are *not* the `for`: the `select`, and the `switch`. When the
`Shutdown` case runs `break`, Go breaks the innermost enclosing construct — the
`switch` — and control falls straight back to the top of the `for`, which loops
again immediately. The service does not stop. Worse, because the `events` channel
usually has nothing more to send during a shutdown, the loop blocks in `select`
forever on a goroutine that will never exit, and on the next deploy the
orchestrator waits out the grace period and `SIGKILL`s the process.

The same trap sits on the `case ev, ok := <-events` receive: when the channel is
closed (`ok == false`), a bare `break` inside the `select` leaves only the
`select`, and the loop spins reading a closed channel. Both exits — the
`Shutdown` event and the closed channel — must be a labeled `break loop` that
names the `for`. The `ctx.Done()` case returns directly, which is also a correct
way to leave (a `return` is not scoped to the `select`).

Create `eventloop.go`:

```go
package eventloop

import "context"

// Kind classifies an event on the dispatch channel.
type Kind int

const (
	KindMessage Kind = iota
	KindHeartbeat
	KindShutdown
)

// Event is a single item on the dispatch channel.
type Event struct {
	Kind    Kind
	Payload string
}

// Dispatcher processes events until a Shutdown event, a closed channel, or a
// cancelled context stops it.
type Dispatcher struct {
	processed []string
}

// Run reads events until a Shutdown event arrives (or the channel closes, or ctx
// is cancelled). It returns ctx.Err() if the context is cancelled first, and nil
// on a clean stop. The labeled break is what actually leaves the loop; a bare
// break would leave only the select or the switch and spin forever.
func (d *Dispatcher) Run(ctx context.Context, events <-chan Event) error {
loop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				break loop // channel closed: leave the for, not just the select
			}
			switch ev.Kind {
			case KindShutdown:
				break loop // leave the for, not just the switch
			case KindMessage:
				d.processed = append(d.processed, ev.Payload)
			case KindHeartbeat:
				// liveness only; nothing to do
			}
		}
	}
	return nil
}

// Processed returns the payloads of the Message events handled so far.
func (d *Dispatcher) Processed() []string {
	return d.processed
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/eventloop"
)

func main() {
	events := make(chan eventloop.Event, 8)
	events <- eventloop.Event{Kind: eventloop.KindMessage, Payload: "order-1"}
	events <- eventloop.Event{Kind: eventloop.KindHeartbeat}
	events <- eventloop.Event{Kind: eventloop.KindMessage, Payload: "order-2"}
	events <- eventloop.Event{Kind: eventloop.KindShutdown}
	events <- eventloop.Event{Kind: eventloop.KindMessage, Payload: "order-3"} // after Shutdown

	var d eventloop.Dispatcher
	if err := d.Run(context.Background(), events); err != nil {
		fmt.Println("run error:", err)
	}

	fmt.Println("processed:", d.Processed())
	fmt.Println("stopped: true")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed: [order-1 order-2]
stopped: true
```

The `order-3` message sent after `Shutdown` is never processed: the labeled break
left the loop the moment the `Shutdown` event was read.

### Tests

`TestStopsOnShutdown` is the regression test for the gotcha: it enqueues two
messages, a `Shutdown`, then two more messages, and asserts `Run` returned and
processed exactly the first two. Against the naive bare-break version this test
fails — that version would either drain the trailing messages or spin forever.
`TestStopsOnClosedChannel` closes the channel and asserts a clean stop.
`TestContextCancelReturnsErr` cancels the context and asserts `context.Canceled`.

Create `eventloop_test.go`:

```go
package eventloop

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

func msg(p string) Event { return Event{Kind: KindMessage, Payload: p} }

func TestStopsOnShutdown(t *testing.T) {
	t.Parallel()

	events := make(chan Event, 6)
	events <- msg("a")
	events <- msg("b")
	events <- Event{Kind: KindShutdown}
	events <- msg("c") // must NOT be processed
	events <- msg("d")

	var d Dispatcher
	if err := d.Run(context.Background(), events); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got := d.Processed()
	want := []string{"a", "b"}
	if !slices.Equal(got, want) {
		t.Fatalf("processed = %v, want %v (trailing events after Shutdown must be ignored)", got, want)
	}
}

func TestStopsOnClosedChannel(t *testing.T) {
	t.Parallel()

	events := make(chan Event, 3)
	events <- msg("x")
	events <- msg("y")
	close(events)

	var d Dispatcher
	if err := d.Run(context.Background(), events); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	want := []string{"x", "y"}
	if !slices.Equal(d.Processed(), want) {
		t.Fatalf("processed = %v, want %v", d.Processed(), want)
	}
}

func TestContextCancelReturnsErr(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	events := make(chan Event) // never fed
	var d Dispatcher
	err := d.Run(ctx, events)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if len(d.Processed()) != 0 {
		t.Fatalf("processed = %v, want none", d.Processed())
	}
}

func ExampleDispatcher_Run() {
	events := make(chan Event, 3)
	events <- Event{Kind: KindMessage, Payload: "hello"}
	events <- Event{Kind: KindShutdown}
	events <- Event{Kind: KindMessage, Payload: "ignored"}

	var d Dispatcher
	_ = d.Run(context.Background(), events)
	fmt.Println(d.Processed())
	// Output: [hello]
}
```

## Review

The dispatcher is correct when `Run` stops at the first `Shutdown` event and does
not process anything enqueued after it. The way to break it is the bug this
exercise exists for: a bare `break` in the `Shutdown` case that leaves only the
`switch`, so the loop resumes and the trailing events get processed (or, on an
empty channel, the goroutine spins forever). `TestStopsOnShutdown` is the guard —
it asserts `[a, b]`, which the labeled-break version produces and the bare-break
version does not. The `ctx.Done()` case returns directly rather than breaking,
which is the other legitimate way to leave the loop; a `return` is never scoped to
the enclosing `select`. Run `go test -race` to confirm there is no data race on
`processed` (there is not — `Run` is single-goroutine here).

## Resources

- [Go Specification: Break statements](https://go.dev/ref/spec#Break_statements) — a labeled `break` names the `for`, not the `select`.
- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — how a `for`-`select` loop reads channels.
- [Go by Example: Select](https://gobyexample.com/select) — the `for`-`select` event-loop shape.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [01-matrix-labeled-break-search.md](01-matrix-labeled-break-search.md) | Next: [03-broker-reconnect-backoff-loop.md](03-broker-reconnect-backoff-loop.md)
