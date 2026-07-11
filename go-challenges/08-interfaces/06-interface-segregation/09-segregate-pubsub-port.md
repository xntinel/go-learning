# Exercise 9: Split a Broker Into Publisher and Subscriber So a Consumer Carries Only What It Uses

A message broker naturally has two sides: publish and subscribe. An ingestion
service only publishes; a background worker only subscribes. Handing both a fat
`Broker` gives each authority it must never use — the ingester could subscribe,
the worker could publish. This module splits the broker into a `Publisher` and a
`Subscriber`, so each consumer carries only its half, checked at compile time,
and ties the subscriber to graceful shutdown via context cancellation.

## What you'll build

```text
pubsub/                        independent module: example.com/pubsub
  go.mod                       go 1.24
  broker.go                    Publisher (Publish) and Subscriber (Subscribe); memBroker satisfies both
  worker.go                    worker depends on Subscriber only; drains until ctx cancel
  ingest.go                    ingester depends on Publisher only
  cmd/
    demo/
      main.go                  publisher sends, subscriber-worker receives, cancel stops it
  broker_test.go               var _ checks; worker gets published msgs; cancel stops loop; -race
```

Files: `broker.go`, `worker.go`, `ingest.go`, `cmd/demo/main.go`, `broker_test.go`.
Implement: `Publisher interface { Publish(ctx, topic, msg) error }`, `Subscriber interface { Subscribe(ctx, topic) <-chan Message }`, an in-memory `memBroker` satisfying both, and a worker that drains until cancellation.
Test: compile-time `var _` for both; a `Subscriber`-only worker receives what a `Publisher`-only producer sends; cancelling ctx stops the loop; run with `-race`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pubsub/cmd/demo
cd ~/go-exercises/pubsub
go mod init example.com/pubsub
go mod edit -go=1.24
```

### Two ports, one broker, no crossed authority

`Publisher` has one method, `Publish`; `Subscriber` has one method, `Subscribe`.
The in-memory `memBroker` satisfies both, but no consumer ever holds the concrete
type. The ingester's constructor takes a `Publisher`, so its code physically
cannot subscribe. The worker's constructor takes a `Subscriber`, so its code
physically cannot publish. This is least privilege for messaging: a worker that
accidentally publishes into the topic it consumes is a classic infinite-loop
incident, and the type system makes it unrepresentable here.

The channel direction is part of the segregation story. `Subscribe` returns a
`<-chan Message` — a receive-only channel. The worker can read from it but cannot
send into it or close it; only the broker, which holds the bidirectional channel
internally, controls the producing side. Channel direction is interface
segregation applied to a channel: the consumer gets exactly the capability it
needs (receive) and not the one it must not have (send/close).

Graceful shutdown is wired through context. `Subscribe` takes a `ctx`; when the
worker's context is cancelled, the broker stops delivering to that subscriber and
the worker's `range` over the channel ends, so the worker loop exits cleanly with
no leaked goroutine. This is the real shutdown shape: a background worker drains
its subscription until the process is asked to stop, then returns.

Create `broker.go`:

```go
package pubsub

import (
	"context"
	"sync"
)

// Message is a published payload on a topic.
type Message struct {
	Topic string
	Body  string
}

// Publisher is the write side of the broker: one method. An ingester depends on
// this and cannot subscribe.
type Publisher interface {
	Publish(ctx context.Context, topic, body string) error
}

// Subscriber is the read side: one method returning a receive-only channel. A
// worker depends on this and cannot publish.
type Subscriber interface {
	Subscribe(ctx context.Context, topic string) <-chan Message
}

type subscription struct {
	topic string
	ch    chan Message
	ctx   context.Context
}

// memBroker is an in-memory broker satisfying both Publisher and Subscriber.
type memBroker struct {
	mu   sync.Mutex
	subs []*subscription
}

// NewBroker returns a *memBroker as the concrete type; consumers narrow to
// Publisher or Subscriber.
func NewBroker() *memBroker {
	return &memBroker{}
}

// Publish fans body out to every live subscriber of topic. It drops to a
// subscriber whose context is done rather than blocking.
func (b *memBroker) Publish(_ context.Context, topic, body string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	live := b.subs[:0]
	for _, s := range b.subs {
		select {
		case <-s.ctx.Done():
			close(s.ch)
			continue // drop the dead subscription
		default:
		}
		select {
		case s.ch <- Message{Topic: topic, Body: body}:
		case <-s.ctx.Done():
			close(s.ch)
			continue
		}
		live = append(live, s)
	}
	b.subs = live
	return nil
}

// Subscribe registers a receive-only channel for topic. When ctx is cancelled
// the broker stops delivering and closes the channel, ending the worker's range.
func (b *memBroker) Subscribe(ctx context.Context, topic string) <-chan Message {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := &subscription{topic: topic, ch: make(chan Message, 16), ctx: ctx}
	b.subs = append(b.subs, s)
	return s.ch
}

var (
	_ Publisher  = (*memBroker)(nil)
	_ Subscriber = (*memBroker)(nil)
)
```

The delivery model above is deliberately simple (a buffered channel, drop-on-cancel);
a production broker adds acks, retries, and per-subscriber goroutines. The point
here is the port split, not the broker internals.

Create `worker.go`. The worker depends on `Subscriber` only:

```go
package pubsub

import (
	"context"
	"sync"
)

// Worker consumes messages from a topic. Its only dependency is Subscriber, so
// it has no Publish method reachable anywhere in its code.
type Worker struct {
	sub Subscriber

	mu       sync.Mutex
	received []string
}

// NewWorker wires a Subscriber into the worker.
func NewWorker(s Subscriber) *Worker {
	return &Worker{sub: s}
}

// Run subscribes and drains the topic until ctx is cancelled, at which point the
// broker closes the channel and the range ends. Returns the count consumed.
func (w *Worker) Run(ctx context.Context, topic string) int {
	ch := w.sub.Subscribe(ctx, topic)
	count := 0
	for msg := range ch {
		w.mu.Lock()
		w.received = append(w.received, msg.Body)
		w.mu.Unlock()
		count++
	}
	return count
}

// Received returns a copy of the bodies consumed so far.
func (w *Worker) Received() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, len(w.received))
	copy(out, w.received)
	return out
}
```

Create `ingest.go`. The ingester depends on `Publisher` only:

```go
package pubsub

import (
	"context"
	"fmt"
)

// Ingester accepts events and publishes them. Its only dependency is Publisher;
// it cannot subscribe to the topic it feeds.
type Ingester struct {
	pub   Publisher
	topic string
}

// NewIngester wires a Publisher and target topic.
func NewIngester(p Publisher, topic string) *Ingester {
	return &Ingester{pub: p, topic: topic}
}

// Ingest publishes one event body.
func (in *Ingester) Ingest(ctx context.Context, body string) error {
	if err := in.pub.Publish(ctx, in.topic, body); err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	return nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/pubsub"
)

func main() {
	broker := pubsub.NewBroker()

	// Worker sees the broker as a Subscriber only.
	worker := pubsub.NewWorker(broker)
	// Ingester sees the broker as a Publisher only.
	ingester := pubsub.NewIngester(broker, "events")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() { done <- worker.Run(ctx, "events") }()

	// Let the subscription register, then publish.
	time.Sleep(10 * time.Millisecond)
	_ = ingester.Ingest(ctx, "first")
	_ = ingester.Ingest(ctx, "second")
	time.Sleep(10 * time.Millisecond)

	cancel() // graceful shutdown: stops the worker loop
	// Nudge Publish so the broker observes the cancelled subscriber and closes it.
	_ = ingester.Ingest(context.Background(), "flush")

	count := <-done
	fmt.Printf("worker consumed %d messages\n", count)
	fmt.Printf("bodies: %v\n", worker.Received())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker consumed 2 messages
bodies: [first second]
```

### Tests

Create `broker_test.go`:

```go
package pubsub

import (
	"context"
	"testing"
	"time"
)

func TestWorkerReceivesPublishedMessages(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	worker := NewWorker(broker)          // Subscriber only
	ingester := NewIngester(broker, "t") // Publisher only

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- worker.Run(ctx, "t") }()

	time.Sleep(10 * time.Millisecond)
	if err := ingester.Ingest(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := ingester.Ingest(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	cancel()
	_ = ingester.Ingest(context.Background(), "flush") // let broker observe cancel

	count := <-done
	if count != 2 {
		t.Fatalf("consumed %d, want 2", count)
	}
	got := worker.Received()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("received %v, want [a b]", got)
	}
}

func TestCancelStopsWorkerLoop(t *testing.T) {
	t.Parallel()

	broker := NewBroker()
	worker := NewWorker(broker)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- worker.Run(ctx, "t") }()

	time.Sleep(10 * time.Millisecond)
	cancel()

	// Publish after cancel so the broker closes the cancelled subscription.
	_ = broker.Publish(context.Background(), "t", "ignored")

	select {
	case <-done:
		// worker exited cleanly
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestBrokerSatisfiesBothPorts(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	var p Publisher = b
	var s Subscriber = b
	if p == nil || s == nil {
		t.Fatal("broker should satisfy both Publisher and Subscriber")
	}
}
```

## Review

The split is correct when the worker holds a `Subscriber` and the ingester holds
a `Publisher`, so neither can perform the other's operation — the worker has no
`Publish` in scope, making the publish-into-your-own-topic loop a compile error
rather than an incident. Channel direction reinforces this: `Subscribe` returns
`<-chan Message`, a receive-only view, so the consumer cannot send or close. The
graceful-shutdown path is the production payoff: cancelling the context ends the
worker's `range` cleanly with no leaked goroutine, which the cancellation test
pins by asserting `Run` returns. Run `go test -race` because the broker fans
messages across goroutines and the worker's slice is mutex-guarded.

## Resources

- [context package (WithCancel)](https://pkg.go.dev/context)
- [Go Specification: Channel types (direction)](https://go.dev/ref/spec#Channel_types)
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels)
- [Go Code Review Comments: Interfaces](https://go.dev/wiki/CodeReviewComments#interfaces)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [08-health-check-aggregator.md](08-health-check-aggregator.md) | Next: [10-audit-and-split-object-store.md](10-audit-and-split-object-store.md)
