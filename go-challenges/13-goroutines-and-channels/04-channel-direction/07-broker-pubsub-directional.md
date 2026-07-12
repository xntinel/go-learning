# Exercise 7: A Pub/Sub Broker Exposing Directional Views Over Bidirectional Fields

The "directional channel as a struct field" instinct is a smell. This exercise
shows the correct pattern: the broker stores *bidirectional* `chan Event` values
internally — where it needs full control to send, receive, and close — and
exposes direction only through method signatures. `Subscribe() <-chan Event`
returns a receive-only view; `Publish(e Event)` is the only send path. Direction
is an encapsulation boundary, not a field type.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests.

## What you'll build

```text
broker/                      independent module: example.com/broker
  go.mod                     go 1.26
  broker.go                  type Broker; New; Subscribe() (<-chan Event, func());
                             Publish(Event); Close()
  cmd/
    demo/
      main.go                runnable demo: two subscribers, one publish
  broker_test.go             delivery, multi-subscriber, unsubscribe, close
```

Files: `broker.go`, `cmd/demo/main.go`, `broker_test.go`.
Implement: a `Broker` with a mutex-guarded subscriber registry, `Subscribe() (<-chan Event, func())`, `Publish(e Event)` with a non-blocking fan-out, and `Close()`.
Test: a subscriber receives a published event, multiple subscribers each get it, unsubscribe stops delivery, and the returned channel is receive-only.
Verify: `go test -count=1 -race ./...`

### Why the field is bidirectional and the method return is directional

The broker keeps a `map[int]chan Event` — plain bidirectional channels. It needs
them bidirectional: it *sends* to them in `Publish` and it *closes* them in
`Unsubscribe` and `Close`. If the field type were `chan<- Event` it could still
send, but it could not be received from for any internal use; if it were
`<-chan Event` it could not send at all. Bidirectional internally is the only type
that gives the broker every operation it needs.

Direction appears where it belongs — at the API. `Subscribe` creates a channel,
stores the bidirectional end in the registry, and returns the channel narrowed to
`<-chan Event` plus an unsubscribe function. The subscriber holds a receive-only
handle: it cannot `close` its subscription (a compile error) and cannot inject
events by sending. The only way into the broker is `Publish`, and the only way
out of a subscription is the returned unsubscribe closure. That is direction used
as an encapsulation boundary.

`Publish` fans out under an `RLock` so concurrent publishes proceed in parallel,
and it sends to each subscriber with a non-blocking `select { case ch <- e:
default: }`. The `default` implements a slow-subscriber drop policy: a subscriber
that is not keeping up simply misses events rather than blocking every publisher.
The registry mutation paths — `Subscribe`, the unsubscribe closure, and `Close`
— take the write lock, so a subscriber channel is never closed while a `Publish`
might be sending to it. That mutual exclusion is what prevents a send-on-closed
panic: `Publish` holds `RLock`, `Unsubscribe`/`Close` hold `Lock`, and the two
can never overlap.

Subscriber channels are buffered so that, absent a race with an active reader, a
single publish lands rather than being dropped by the non-blocking send — which
makes the delivery tests deterministic.

Create `broker.go`:

```go
package broker

import "sync"

// Event is a message fanned out to subscribers.
type Event struct {
	Topic string
	Body  string
}

// Broker is a concurrency-safe pub/sub hub. It stores bidirectional channels
// internally (so it can send to and close them) and exposes only directional
// views: Subscribe returns <-chan Event, Publish is the sole send path.
type Broker struct {
	mu     sync.RWMutex
	subs   map[int]chan Event
	nextID int
	buffer int
	closed bool
}

// New returns a Broker whose subscriber channels are buffered by bufferSize.
func New(bufferSize int) *Broker {
	return &Broker{subs: make(map[int]chan Event), buffer: bufferSize}
}

// Subscribe registers a new subscriber and returns a receive-only channel and
// an unsubscribe function. The subscriber cannot close or send on the channel.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	id := b.nextID
	b.nextID++
	ch := make(chan Event, b.buffer)
	b.subs[id] = ch
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
	return ch, unsub
}

// Publish fans e out to every current subscriber with a non-blocking send. A
// subscriber that is not keeping up misses the event rather than blocking the
// publisher.
func (b *Broker) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Close removes and closes every subscriber channel. Subsequent Subscribe calls
// return an already-closed channel.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}
```

The compile-fail contract: a subscriber cannot close its handle. `close(sub)`
where `sub` is the returned `<-chan Event` fails with `invalid operation: cannot
close receive-only channel`. Closing is the broker's job alone, done under the
write lock.

### The runnable demo

The demo registers two subscribers, publishes one event, and each subscriber
prints it. Publishing happens before any concurrent draining, so with buffered
channels both subscribers receive the event.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"

	"example.com/broker"
)

func main() {
	b := broker.New(4)
	s1, _ := b.Subscribe()
	s2, _ := b.Subscribe()

	b.Publish(broker.Event{Topic: "orders", Body: "created"})
	b.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for e := range s1 {
			fmt.Printf("s1: %s/%s\n", e.Topic, e.Body)
		}
	}()
	go func() {
		defer wg.Done()
		for e := range s2 {
			fmt.Printf("s2: %s/%s\n", e.Topic, e.Body)
		}
	}()
	wg.Wait()
	fmt.Println("broker closed")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
s1: orders/created
s2: orders/created
broker closed
```

The two `s1`/`s2` lines may print in either order; the broker closes each channel
after the single buffered event, so both loops end and "broker closed" prints
last.

### Tests

`TestSubscribeReceivesPublished` publishes one event and reads it back.
`TestMultipleSubscribersEachGetEvent` asserts every subscriber gets an
independent copy. `TestUnsubscribeStopsDelivery` unsubscribes and asserts the
handle is closed and receives no further events. `TestConcurrentPublishSubscribe`
runs publishers and subscribers concurrently under `-race`.

Create `broker_test.go`:

```go
package broker

import (
	"sync"
	"testing"
	"time"
)

func TestSubscribeReceivesPublished(t *testing.T) {
	t.Parallel()

	b := New(1)
	sub, unsub := b.Subscribe()
	defer unsub()

	b.Publish(Event{Topic: "t", Body: "hello"})
	select {
	case e := <-sub:
		if e.Body != "hello" {
			t.Fatalf("got body %q, want hello", e.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber never received the event")
	}
}

func TestMultipleSubscribersEachGetEvent(t *testing.T) {
	t.Parallel()

	b := New(1)
	subs := make([]<-chan Event, 3)
	for i := range subs {
		ch, _ := b.Subscribe()
		subs[i] = ch
	}

	b.Publish(Event{Topic: "t", Body: "fanout"})
	for i, ch := range subs {
		select {
		case e := <-ch:
			if e.Body != "fanout" {
				t.Fatalf("subscriber %d got %q, want fanout", i, e.Body)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d never received the event", i)
		}
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()

	b := New(1)
	sub, unsub := b.Subscribe()
	unsub()

	if _, ok := <-sub; ok {
		t.Fatal("channel should be closed after unsubscribe")
	}

	// A publish after unsubscribe must not panic and must not be delivered.
	b.Publish(Event{Topic: "t", Body: "late"})
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()

	b := New(8)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsub := b.Subscribe()
			defer unsub()
			b.Publish(Event{Topic: "t", Body: "x"})
			select {
			case <-ch:
			case <-time.After(time.Second):
			}
		}()
	}
	wg.Wait()
	b.Close()
}
```

## Review

The broker is correct when its subscribers can only receive, when publishing is
the only send path, and when the registry's lock discipline makes send-on-closed
impossible. The key design lesson is that the *field* is bidirectional and the
*direction lives in the method signatures* — the opposite of the "make the field
send-only" instinct that leaves you unable to fan out or close. The
unsubscribe-then-publish test proves the lock discipline holds: closing under the
write lock while `Publish` needs the read lock means the two never overlap, so a
late publish neither panics nor reaches a removed subscriber. Run `go test -race`
under concurrent publish/subscribe to confirm.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — why `close` on a receive-only handle is a compile error.
- [`sync.RWMutex`](https://pkg.go.dev/sync#RWMutex) — the read/write lock discipline that keeps `Publish` and `Close` from overlapping.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — idiomatic non-blocking sends with `select`/`default`.

---

Prev: [06-or-done-cancellation.md](06-or-done-cancellation.md) | Back to [00-concepts.md](00-concepts.md) | Next: [08-signal-shutdown-notify.md](08-signal-shutdown-notify.md)
