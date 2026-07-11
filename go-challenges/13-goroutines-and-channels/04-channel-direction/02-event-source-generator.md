# Exercise 2: A Change-Feed Source That Hands Back a Receive-Only Stream

The generator pattern is the canonical directional API: a function that owns its
channel, spawns the producing goroutine, closes on completion, and returns
`<-chan Event` so callers can only drain it. This is how every change-data-capture
feed, log tailer, and event source in a backend is shaped — including `time.Tick`
and `context.Done` in the stdlib.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
feed/                        independent module: example.com/feed
  go.mod                     go 1.26
  feed.go                    type Event; Source(events []Event) <-chan Event
  cmd/
    demo/
      main.go                runnable demo: drain a change feed
  feed_test.go               ordered delivery, close-on-done, empty source
```

Files: `feed.go`, `cmd/demo/main.go`, `feed_test.go`.
Implement: `Source(events []Event) <-chan Event` — owns a channel, produces in a goroutine, closes when done, returns the receive-only end.
Test: full ordered delivery, the channel is closed after drain, an empty source closes immediately.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/feed/cmd/demo
cd ~/go-exercises/feed
go mod init example.com/feed
```

### Why the return type is the contract

`Source` creates a bidirectional `chan Event`, keeps that reference for its own
goroutine, and returns it narrowed to `<-chan Event`. The caller receives a value
it can only drain. It cannot `close` the stream — `close(ch)` on a `<-chan Event`
is a compile error — and it cannot inject fake events by sending. The entire
lifecycle of the channel, production and the single close, lives inside `Source`.

The producing goroutine uses `defer close(out)`, so the channel is closed exactly
once when production finishes, whether the input had a thousand events or none.
That `defer` is the "owner closes" rule made concrete: `Source` is the sender, so
`Source` closes.

Because there is a single producer writing to an unbuffered channel and a single
consumer draining it, delivery is strictly ordered: event *i+1* is not sent until
the consumer has received event *i*. Ordered delivery is a property of the
single-producer/single-consumer topology, not something you have to enforce.

The failure mode to keep in mind: if a caller stops draining before the source is
done, the producing goroutine parks on its next send forever — a goroutine leak.
A finite source like this one drains fully in tests; a long-lived source needs a
cancellation path, which Exercise 6 (or-done) adds. Here the contract is the pure
generator: finite input, full drain, single close.

Create `feed.go`:

```go
package feed

// Event is one record on the change feed.
type Event struct {
	ID      int
	Payload string
}

// Source returns a receive-only stream of events. It owns the underlying
// channel: a goroutine writes every event and then closes the channel. The
// caller receives <-chan Event and therefore cannot close or send on it.
func Source(events []Event) <-chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		for _, e := range events {
			out <- e
		}
	}()
	return out
}
```

### The runnable demo

The demo drains the returned stream with `for range` and stops when `Source`
closes it. It never touches `close` — it cannot.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/feed"
)

func main() {
	stream := feed.Source([]feed.Event{
		{ID: 1, Payload: "row-inserted"},
		{ID: 2, Payload: "row-updated"},
		{ID: 3, Payload: "row-deleted"},
	})

	for e := range stream {
		fmt.Printf("event %d: %s\n", e.ID, e.Payload)
	}
	fmt.Println("stream closed")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
event 1: row-inserted
event 2: row-updated
event 3: row-deleted
stream closed
```

### Tests

`TestSourceDeliversAllEventsInOrder` drains the stream and asserts the exact
ordered slice — order is guaranteed by the single-producer topology.
`TestSourceClosesChannelWhenDone` drains fully and then asserts a further
two-value receive reports `ok == false`, proving the owner closed the channel.
`TestEmptySourceClosesImmediately` proves the empty-input path still closes.

Create `feed_test.go`:

```go
package feed

import (
	"testing"
	"time"
)

func drain(ch <-chan Event) []Event {
	var got []Event
	for e := range ch {
		got = append(got, e)
	}
	return got
}

func TestSourceDeliversAllEventsInOrder(t *testing.T) {
	t.Parallel()

	want := []Event{
		{ID: 1, Payload: "a"},
		{ID: 2, Payload: "b"},
		{ID: 3, Payload: "c"},
	}
	got := drain(Source(want))
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSourceClosesChannelWhenDone(t *testing.T) {
	t.Parallel()

	ch := Source([]Event{{ID: 1, Payload: "only"}})
	if _, ok := <-ch; !ok {
		t.Fatal("first receive should deliver the event")
	}
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("second receive should report closed, got a value")
		}
	case <-time.After(time.Second):
		t.Fatal("channel never closed")
	}
}

func TestEmptySourceClosesImmediately(t *testing.T) {
	t.Parallel()

	got := drain(Source(nil))
	if len(got) != 0 {
		t.Fatalf("empty source produced %d events, want 0", len(got))
	}
}
```

## Review

The source is correct when it owns and closes exactly once and hands back only a
receive-only view. The close-on-done test is the one that matters: a source that
forgets `defer close(out)` would leave `for range` consumers blocked forever, and
this test catches that as a timeout. The return type `<-chan Event` is doing real
work — it is what makes "a caller closes the feed" a compile error rather than a
production panic. Run `go test -race` to confirm the single handoff between the
producer goroutine and the drainer is clean.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the generator pattern and why sources return receive-only channels.
- [`time.Tick`](https://pkg.go.dev/time#Tick) — a stdlib generator that returns `<-chan Time`, the exact shape built here.
- [Go spec: Close](https://go.dev/ref/spec#Close) — the semantics of `close` and the two-value receive that detects it.

---

Prev: [01-streaming-etl-pipeline.md](01-streaming-etl-pipeline.md) | Back to [00-concepts.md](00-concepts.md) | Next: [03-batching-sink-writer.md](03-batching-sink-writer.md)
