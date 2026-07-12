# Exercise 2: Bridging Per-Connection Message Streams

A real server does not hold its inbound channels in a slice. Connections open over time, each one produces its own `<-chan Message`, and somewhere a single consumer — an ordered writer, a sequential log, a checkpointer — has to process every message from every connection as a flat stream. This exercise builds that: a `Hub` whose connections register dynamically onto a stream-of-streams, and a bridge that flattens them so one consumer drains each connection's messages as a contiguous, ordered block before the next connection's begin.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
hub.go               Message, Hub, NewHub, Open, Connections, Shutdown, Bridge[T]
cmd/
  demo/
    main.go          three connections register in turn; one consumer prints the flat stream
hub_test.go          ordered-block delivery, concurrent registration, done cancellation, empty shutdown
```

- Files: `hub.go`, `cmd/demo/main.go`, `hub_test.go`.
- Implement: `Message`, `Hub` with `NewHub`, `(*Hub).Open(done) chan<- Message`, `(*Hub).Connections() <-chan (<-chan Message)`, `(*Hub).Shutdown()`, and the generic `Bridge[T any]`.
- Test: `hub_test.go` proves sequential registration yields exact ordered blocks, concurrent registration delivers every message with each connection contiguous, closing `done` terminates the bridge, and shutting down a hub with no connections closes the output.
- Verify: `go test -run 'TestHub|TestBridge' -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/12-bridge-channel-pattern/02-connection-stream-bridge/cmd/demo && cd go-solutions/16-concurrency-patterns/12-bridge-channel-pattern/02-connection-stream-bridge
```

### Why registration over a channel, and what the serialization buys

The `Hub` turns "a connection exists" into "a channel is on the outer stream." `Open` creates an unbuffered `chan Message`, hands its receive end to the bridge by sending it on the hub's outer channel, and returns its send end to the caller. Because the outer channel is unbuffered, `Open` blocks until the bridge actually receives the connection — and the bridge only receives the next connection after it has fully drained the current one. That single fact is the design's backbone: connections are *serialized* by the bridge, so each connection's messages land in the flat output as one contiguous, in-order block, and the next connection cannot even start streaming until the current one closes.

That is the right contract when a "connection" is a bounded, ordered batch — a request and its response frames, a transaction's write set, a session that must be journaled atomically — and a single downstream consumer must see each batch whole and in order. It is deliberately the *wrong* contract for long-lived connections that should all make progress at once; that is a fan-in, and mixing the two up is the classic mistake the concepts file warns about. Here the serialization is the feature: it is what lets the ordered consumer treat the flattened stream as a clean sequence of per-connection blocks without any per-message connection tagging or reordering buffer.

`Shutdown` closes the outer channel, which is how the bridge learns no more connections will ever arrive: it drains the last connection, sees the closed outer channel, and closes its output so the consumer's `range` ends. Cancellation through `done` is the other exit — it unblocks a connection stuck mid-`Open` and the bridge stuck mid-forward alike.

Create `hub.go`:

```go
package connbridge

// Message is one item produced by a connection. Conn identifies the originating
// connection; Seq is its position within that connection's stream.
type Message struct {
	Conn int
	Seq  int
}

// Hub registers per-connection message channels dynamically and exposes them as
// a single stream-of-streams for a bridge to flatten.
type Hub struct {
	conns chan (<-chan Message)
}

// NewHub returns a ready hub. The outer channel is unbuffered so registration
// blocks until the bridge takes each connection, which is what serializes
// connections into contiguous blocks in the flattened output.
func NewHub() *Hub {
	return &Hub{conns: make(chan (<-chan Message))}
}

// Open registers a new connection and returns the channel the caller writes its
// messages to. The caller closes that channel to end the connection. Open blocks
// until the bridge receives the connection, or returns early if done is closed.
func (h *Hub) Open(done <-chan struct{}) chan<- Message {
	c := make(chan Message)
	select {
	case h.conns <- c:
	case <-done:
	}
	return c
}

// Connections is the stream-of-streams a bridge consumes: each value is one
// connection's message channel, in registration order.
func (h *Hub) Connections() <-chan (<-chan Message) {
	return h.conns
}

// Shutdown closes the outer channel, signaling that no more connections will
// register. The bridge drains the current connection, sees the close, and ends.
func (h *Hub) Shutdown() {
	close(h.conns)
}

// Bridge flattens a stream of connection channels into a single channel,
// draining each connection fully before the next so messages arrive as
// contiguous per-connection blocks. The owning goroutine is the only sender on
// out and closes it on every exit; closing done terminates from either select.
func Bridge[T any](done <-chan struct{}, conns <-chan (<-chan T)) <-chan T {
	out := make(chan T)
	go func() {
		defer close(out)
		for {
			var conn <-chan T
			select {
			case <-done:
				return
			case c, ok := <-conns:
				if !ok {
					return
				}
				conn = c
			}
			if conn == nil {
				continue
			}
			for v := range conn {
				select {
				case <-done:
					return
				case out <- v:
				}
			}
		}
	}()
	return out
}
```

The type juggling in `Open` is worth a glance: `c` is a bidirectional `chan Message`, sent onto `h.conns` (whose element type is `<-chan Message`, the receive end) and returned to the caller as `chan<- Message` (the send end). The same channel value is handed to the two parties with the directional types each is allowed to use, which is the idiomatic way Go splits a channel's ownership between a producer and a consumer.

### The runnable demo

The demo registers three connections from a single producer goroutine, one after another, each emitting two messages and then closing. A single consumer ranges the bridged output. Because registration is sequential and the bridge drains each connection fully, the output is three clean blocks in registration order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connbridge"
)

func main() {
	done := make(chan struct{})
	defer close(done)

	hub := connbridge.NewHub()

	go func() {
		for c := 0; c < 3; c++ {
			ch := hub.Open(done)
			for s := 0; s < 2; s++ {
				ch <- connbridge.Message{Conn: c, Seq: s}
			}
			close(ch)
		}
		hub.Shutdown()
	}()

	for m := range connbridge.Bridge(done, hub.Connections()) {
		fmt.Printf("conn=%d seq=%d\n", m.Conn, m.Seq)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
conn=0 seq=0
conn=0 seq=1
conn=1 seq=0
conn=1 seq=1
conn=2 seq=0
conn=2 seq=1
```

### Tests

`TestHubDeliversConnectionsInOrder` registers sequentially and asserts the exact flattened order. `TestHubConcurrentRegistration` launches many connections at once and proves the serialization holds under contention: every message is delivered, and each connection's messages form one contiguous, in-order block even though the *order of the blocks* is now scheduler-decided. `TestBridgeHonorsDone` streams an unbounded connection and proves closing `done` closes the output. `TestHubShutdownWithNoConnections` shuts down an idle hub and asserts the output closes empty.

Create `hub_test.go`:

```go
package connbridge

import (
	"sync"
	"testing"
	"time"
)

func TestHubDeliversConnectionsInOrder(t *testing.T) {
	t.Parallel()

	const conns, perConn = 3, 2
	hub := NewHub()

	done := make(chan struct{})
	defer close(done)

	go func() {
		for c := 0; c < conns; c++ {
			ch := hub.Open(done)
			for s := 0; s < perConn; s++ {
				ch <- Message{Conn: c, Seq: s}
			}
			close(ch)
		}
		hub.Shutdown()
	}()

	var got []Message
	for m := range Bridge(done, hub.Connections()) {
		got = append(got, m)
	}

	if len(got) != conns*perConn {
		t.Fatalf("got %d messages, want %d", len(got), conns*perConn)
	}
	i := 0
	for c := 0; c < conns; c++ {
		for s := 0; s < perConn; s++ {
			if got[i] != (Message{Conn: c, Seq: s}) {
				t.Fatalf("position %d: got %+v, want conn=%d seq=%d", i, got[i], c, s)
			}
			i++
		}
	}
}

func TestHubConcurrentRegistration(t *testing.T) {
	t.Parallel()

	const conns, perConn = 12, 50
	hub := NewHub()

	done := make(chan struct{})
	defer close(done)

	var wg sync.WaitGroup
	for c := 0; c < conns; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := hub.Open(done)
			for s := 0; s < perConn; s++ {
				ch <- Message{Conn: c, Seq: s}
			}
			close(ch)
		}()
	}
	go func() {
		wg.Wait()
		hub.Shutdown()
	}()

	var got []Message
	for m := range Bridge(done, hub.Connections()) {
		got = append(got, m)
	}

	if len(got) != conns*perConn {
		t.Fatalf("got %d messages, want %d", len(got), conns*perConn)
	}

	// Each connection must appear as one contiguous, in-order block. Scan the
	// flattened stream, splitting on connection change, and validate each block.
	counts := make(map[int]int)
	blocks := 0
	for i := 0; i < len(got); {
		c := got[i].Conn
		blocks++
		s := 0
		for i < len(got) && got[i].Conn == c {
			if got[i].Seq != s {
				t.Fatalf("conn %d: out-of-order seq at block offset %d: got %d, want %d", c, s, got[i].Seq, s)
			}
			s++
			i++
		}
		if s != perConn {
			t.Fatalf("conn %d block has %d messages, want %d (block not contiguous)", c, s, perConn)
		}
		counts[c]++
	}
	if blocks != conns {
		t.Fatalf("got %d connection blocks, want %d", blocks, conns)
	}
	for c := 0; c < conns; c++ {
		if counts[c] != 1 {
			t.Fatalf("conn %d appeared in %d blocks, want exactly 1", c, counts[c])
		}
	}
}

func TestBridgeHonorsDone(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	done := make(chan struct{})

	go func() {
		ch := hub.Open(done)
		for i := 0; ; i++ {
			select {
			case ch <- Message{Conn: 0, Seq: i}:
			case <-done:
				return
			}
		}
	}()

	out := Bridge(done, hub.Connections())
	got := 0
	for m := range out {
		got++
		if m.Seq >= 4 {
			close(done)
		}
	}
	if got < 5 {
		t.Fatalf("read %d messages before cancel, want at least 5", got)
	}
}

func TestHubShutdownWithNoConnections(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	hub.Shutdown()

	done := make(chan struct{})
	defer close(done)

	out := Bridge(done, hub.Connections())
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("expected closed output for an idle hub")
		}
	case <-time.After(time.Second):
		t.Fatal("Bridge did not close after Shutdown with no connections")
	}
}
```

## Review

The hub is correct when the bridge's serialization is observable as structure in the output. With sequential registration the flattened stream is exact per-connection blocks in registration order; with concurrent registration the block *order* is scheduler-decided but each connection is still one contiguous, in-order run of all its messages, every message delivered exactly once — that is the property the contiguity scan in `TestHubConcurrentRegistration` pins. Confirm that `Shutdown` closes the output for an idle hub and that closing `done` terminates a live, unbounded connection, both under `-race`.

Common mistakes for this feature. Buffering the outer `conns` channel breaks the serialization that makes the blocks contiguous: with a buffer, two connections can be registered before the first is drained, and their messages interleave. Forgetting that `Open` must select on `done` strands a connection goroutine forever when the consumer cancels before taking it. Closing the per-connection channel from the bridge instead of the connection owner is a double-close waiting to happen — the producer that created the channel closes it, and the bridge only ever reads. And calling `Shutdown` before the last connection has drained is fine here precisely because the bridge finishes the current connection before noticing the closed outer channel; closing the outer channel does not truncate an in-flight connection.

## Resources

- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-in, explicit `done` cancellation, and the channel-ownership discipline this hub follows.
- [Go Concurrency Patterns (Rob Pike, Google I/O 2012)](https://go.dev/blog/io2013-talk-concurrency) — the talk on composing servers from channels handed between producers and consumers.
- [The Go Memory Model](https://go.dev/ref/mem) — why the channel send in `Open` and the receive in the bridge establish the happens-before edge that keeps registration race-free.

---

Back to [01-bridge-core.md](01-bridge-core.md) | Next: [03-dynamic-pipeline-stages.md](03-dynamic-pipeline-stages.md)
