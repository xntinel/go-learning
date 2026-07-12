# Exercise 6: A Pub/Sub Broker Without Subscriber Leaks

An in-process pub/sub broker is a leak magnet in two directions: the broadcast loop
blocks forever on a send to a slow or departed subscriber, and a subscriber whose
consumer goroutine is never released keeps that goroutine and its channel alive. This
exercise builds a broker that closes both holes — a non-blocking send with a drop
policy, and an `Unsubscribe`/`Close` that closes the subscriber channel so its
consumer exits — and proves it under `go.uber.org/goleak`.

This module is self-contained: its own `go mod init`, all code inline, its own demo
and tests. It imports `go.uber.org/goleak`.

## What you'll build

```text
broker/                      independent module: example.com/broker
  go.mod
  broker.go                  type Broker[T]; Subscribe, Unsubscribe, Publish, Close
  cmd/
    demo/
      main.go                runnable demo: two subscribers receive a publish
  broker_test.go             delivery, unsubscribe releases goroutine, slow subscriber, goleak
```

- Files: `broker.go`, `cmd/demo/main.go`, `broker_test.go`.
- Implement: a generic `Broker[T]` with per-subscriber buffered channels, a non-blocking `Publish` that drops to a full subscriber, an `Unsubscribe` that removes and closes, and a `Close` that unsubscribes everyone.
- Test: publish reaches all subscribers; unsubscribe releases the consumer goroutine (goleak); a slow subscriber never blocks the publisher; concurrent operations are race-free.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak@v1.3.0
```

### Non-blocking send with a drop policy

The dangerous broker does `sub.ch <- msg` to every subscriber under the broadcast
loop. One subscriber whose consumer is slow, wedged, or gone fills its channel and
then the send blocks — and because it is inside the loop holding the broadcast, it
blocks delivery to *everyone else too*. A slow subscriber becomes a
denial-of-service on the whole topic.

The fix is a per-subscriber buffered channel and a *non-blocking* send:
`select { case sub.ch <- msg: default: /* drop */ }`. If the subscriber's buffer has
room, it gets the message; if it is full, the message is dropped for that subscriber
and the loop moves on. The publisher never blocks, no matter how slow one consumer
is. Dropping is a deliberate policy choice — the alternative (unbounded buffering)
just moves the leak from goroutines to memory. For telemetry, logs, and cache
invalidations, drop-on-full is the right default; a subscriber that cannot keep up is
not allowed to stall the source.

### Unsubscribe closes the channel so the consumer exits

A subscriber is a channel plus, on the caller's side, a goroutine ranging that
channel. That goroutine's only exit path is the channel being *closed*. So
`Unsubscribe` removes the subscriber from the map and closes its channel; the
consumer's `for range ch` ends and the goroutine returns. A broker that removes the
subscriber but never closes the channel leaks the consumer. `Close` does the same for
every subscriber at once, which is how the broker guarantees no consumer outlives it.

The concurrency rule that keeps this safe: `Publish` sends under the same mutex that
`Unsubscribe`/`Close` hold while closing. That ordering is what prevents a
send-on-closed-channel panic — a send and a close can never interleave, because both
take the lock. The send being non-blocking means holding the lock across it costs
nothing.

Create `broker.go`:

```go
package broker

import (
	"errors"
	"sync"
)

// ErrClosed is returned by Subscribe after the broker is closed.
var ErrClosed = errors.New("broker: closed")

// Broker is an in-process pub/sub hub. Each subscriber has its own buffered
// channel; Publish never blocks on a slow subscriber.
type Broker[T any] struct {
	mu      sync.Mutex
	subs    map[int]chan T
	nextID  int
	bufSize int
	closed  bool
}

// New returns a broker whose subscriber channels each buffer bufSize messages.
func New[T any](bufSize int) *Broker[T] {
	return &Broker[T]{subs: make(map[int]chan T), bufSize: bufSize}
}

// Subscribe registers a new subscriber and returns its id and receive channel.
// The channel is closed by Unsubscribe or Close, which is how a consumer that
// ranges it learns to stop.
func (b *Broker[T]) Subscribe() (int, <-chan T, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, nil, ErrClosed
	}
	id := b.nextID
	b.nextID++
	ch := make(chan T, b.bufSize)
	b.subs[id] = ch
	return id, ch, nil
}

// Unsubscribe removes a subscriber and closes its channel. It is a no-op for an
// unknown id.
func (b *Broker[T]) Unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

// Publish delivers msg to every subscriber with room in its buffer and returns
// how many received it. A subscriber whose buffer is full is skipped (dropped),
// so a slow subscriber never blocks the publisher.
func (b *Broker[T]) Publish(msg T) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0
	}
	delivered := 0
	for _, ch := range b.subs {
		select {
		case ch <- msg:
			delivered++
		default:
			// subscriber buffer full: drop for this subscriber
		}
	}
	return delivered
}

// Close unsubscribes every subscriber, closing all channels so their consumers
// exit. It is idempotent.
func (b *Broker[T]) Close() {
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

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/broker"
)

func main() {
	b := broker.New[string](4)
	_, ch1, _ := b.Subscribe()
	_, ch2, _ := b.Subscribe()

	n := b.Publish("deploy finished")
	fmt.Println("delivered to:", n)
	fmt.Println("sub1:", <-ch1)
	fmt.Println("sub2:", <-ch2)

	b.Close()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered to: 2
sub1: deploy finished
sub2: deploy finished
```

### The tests

`TestMain` installs `goleak.VerifyTestMain`. `TestPublishReachesSubscribers` checks
fan-out delivery by reading each subscriber channel directly.
`TestUnsubscribeReleasesGoroutine` starts a real consumer goroutine and proves
`Unsubscribe` closing the channel lets it exit. `TestSlowSubscriberDoesNotBlock`
never reads a subscriber and asserts `Publish` still returns and drops once the buffer
is full. `TestReproduceSubscriberLeak` shows `goleak.Find` catching a consumer that
was never unsubscribed, then cleans it up. `TestConcurrentPublish` hammers the broker
under `-race` and joins every consumer via `Close`.

Create `broker_test.go`:

```go
package broker

import (
	"sync"
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestPublishReachesSubscribers(t *testing.T) {
	b := New[string](4)
	id1, ch1, _ := b.Subscribe()
	id2, ch2, _ := b.Subscribe()

	if n := b.Publish("hello"); n != 2 {
		t.Fatalf("Publish delivered to %d, want 2", n)
	}
	if got := <-ch1; got != "hello" {
		t.Fatalf("ch1 = %q, want hello", got)
	}
	if got := <-ch2; got != "hello" {
		t.Fatalf("ch2 = %q, want hello", got)
	}
	b.Unsubscribe(id1)
	b.Unsubscribe(id2)
}

func TestUnsubscribeReleasesGoroutine(t *testing.T) {
	b := New[int](4)
	id, ch, _ := b.Subscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range ch {
			// drain until Unsubscribe closes the channel
		}
	}()

	b.Publish(1)
	b.Unsubscribe(id) // closes ch, ending the consumer's range
	<-done            // the goroutine has exited
}

func TestSlowSubscriberDoesNotBlock(t *testing.T) {
	b := New[int](1) // buffer of one; we never read
	id, _, _ := b.Subscribe()

	if n := b.Publish(1); n != 1 {
		t.Fatalf("first Publish delivered %d, want 1", n)
	}
	// The buffer is now full; further publishes must drop, not block.
	for i := range 5 {
		if n := b.Publish(i); n != 0 {
			t.Fatalf("Publish to full subscriber delivered %d, want 0", n)
		}
	}
	b.Unsubscribe(id)
}

func TestReproduceSubscriberLeak(t *testing.T) {
	ignore := goleak.IgnoreCurrent()

	b := New[int](4)
	id, ch, _ := b.Subscribe()
	go func() {
		for range ch { // never unsubscribed in the wild: this consumer leaks
		}
	}()
	b.Publish(1)

	if err := goleak.Find(ignore); err == nil {
		t.Fatal("expected the never-unsubscribed consumer to leak")
	}

	// Clean up: Unsubscribe closes the channel, so the consumer exits.
	b.Unsubscribe(id)
	if err := goleak.Find(ignore); err != nil {
		t.Fatalf("consumer did not exit after Unsubscribe: %v", err)
	}
}

func TestConcurrentPublish(t *testing.T) {
	b := New[int](8)

	var consumers sync.WaitGroup
	for range 10 {
		_, ch, err := b.Subscribe()
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		consumers.Add(1)
		go func() {
			defer consumers.Done()
			for range ch {
				// drain until Close closes the channel
			}
		}()
	}

	var publishers sync.WaitGroup
	for range 5 {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			for i := range 100 {
				b.Publish(i)
			}
		}()
	}
	publishers.Wait()

	b.Close() // closes every subscriber channel, releasing all consumers
	consumers.Wait()
}
```

## Review

The broker is correct when no `Publish` can block on one subscriber and no subscriber
channel outlives its `Unsubscribe`/`Close`. The non-blocking `select`-with-`default`
send is what bounds the publisher; closing the channel is what releases the consumer.
`TestSlowSubscriberDoesNotBlock` proves the first, `TestUnsubscribeReleasesGoroutine`
and the `Close`-driven `TestConcurrentPublish` prove the second, and
`TestReproduceSubscriberLeak` shows what happens when you forget.

The mistakes to avoid: never do a blocking send to a subscriber inside a broadcast
loop; a single slow consumer would stall the topic. Never remove a subscriber without
closing its channel, or the consumer goroutine parks forever. And take the same lock
for `Publish`'s send and `Close`'s close, so a send-on-closed-channel panic is
impossible. Run under `-race` with concurrent publishers and subscribers to confirm
the map and channels are properly guarded.

## Resources

- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — `VerifyTestMain`, `Find`, `IgnoreCurrent`.
- [Go spec: select statements](https://go.dev/ref/spec#Select_statements) — the `default` case that makes a send non-blocking.
- [Go spec: close](https://go.dev/ref/spec#Close) — closing a channel ends a `for range` over it, the consumer's exit path.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-worker-pool-drain-on-cancel.md](05-worker-pool-drain-on-cancel.md) | Next: [07-ticker-timer-leak-poller.md](07-ticker-timer-leak-poller.md)
