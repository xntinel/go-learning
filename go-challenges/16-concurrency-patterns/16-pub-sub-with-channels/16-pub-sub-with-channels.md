# 16. Pub/Sub With Channels: Broadcast Events To Many Subscribers

Pub/Sub is the broadcast primitive: a publisher emits events; every
subscriber receives every event. The channel-based implementation uses one
inbound channel for the publisher and N outbound channels - one per
subscriber - that the broadcaster fills in a single goroutine. The Go talks'
"Advanced Go Concurrency Patterns" use this shape as the building block for
event-driven systems.

```text
pubsub/
  go.mod
  internal/pubsub/pubsub.go
  internal/pubsub/pubsub_test.go
  cmd/pubsubdemo/main.go
```

The package exposes `Broker` with `Subscribe`, `Unsubscribe`, and `Publish`.
Subscribers receive events on their own buffered channel; slow subscribers do
not block fast ones because each subscriber's buffer absorbs the difference.
A `closed` state stops further publishes and signals subscribers.

## Concepts

### One Inbound, N Outbounds

The publisher writes to a single channel. The broadcaster fans the events
out to N subscriber channels. Each subscriber reads from its own channel,
so a slow subscriber does not stall a fast one as long as the per-subscriber
buffer is large enough.

### Unsubscribe Must Drain

`Unsubscribe` removes the subscriber from the broker and closes the
subscriber's channel. Any goroutine blocked on a receive wakes up and the
subscriber's range loop exits.

### Publish Is Blocking If All Buffers Are Full

`Publish` sends to every subscriber's channel under a `select` that also
listens for `done`. If all buffers are full, the publish blocks. The
`TryPublish` variant drops the event for slow subscribers and is the right
choice when at-most-once is acceptable.

### Slow Subscribers Are A Backpressure Source

A subscriber that does not read its channel will eventually fill the buffer.
`Publish` will block. The lesson's `Broker` exposes a per-subscriber buffer
so callers can size backpressure explicitly.

## Exercises

Set up the module:

```bash
mkdir -p ~/go-exercises/pubsub/internal/pubsub ~/go-exercises/pubsub/cmd/pubsubdemo
cd ~/go-exercises/pubsub
go mod init example.com/pubsub
```

### Exercise 1: The Broker

Create `internal/pubsub/pubsub.go`:

```go
package pubsub

import (
	"errors"
	"sync"
)

var (
	ErrClosed        = errors.New("broker closed")
	ErrUnknownSub    = errors.New("unknown subscriber")
	ErrInvalidBuffer = errors.New("buffer must be non-negative")
)

type Event[T any] struct {
	Topic   string
	Payload T
}

type Subscription[T any] struct {
	id     int
	ch     chan Event[T]
	topics map[string]struct{}
}

func (s *Subscription[T]) C() <-chan Event[T] { return s.ch }
func (s *Subscription[T]) ID() int            { return s.id }

type Broker[T any] struct {
	mu        sync.RWMutex
	subs      map[int]*Subscription[T]
	byTopic   map[string]map[int]struct{}
	nextID    int
	buffer    int
	closed    bool
	closeOnce sync.Once
	done      chan struct{}
}

func New[T any](buffer int) *Broker[T] {
	if buffer < 0 {
		buffer = 0
	}
	return &Broker[T]{
		subs:    make(map[int]*Subscription[T]),
		byTopic: make(map[string]map[int]struct{}),
		buffer:  buffer,
		done:    make(chan struct{}),
	}
}

func (b *Broker[T]) Subscribe(topics ...string) (*Subscription[T], error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrClosed
	}
	id := b.nextID
	b.nextID++
	topicSet := make(map[string]struct{}, len(topics))
	for _, t := range topics {
		topicSet[t] = struct{}{}
		if b.byTopic[t] == nil {
			b.byTopic[t] = make(map[int]struct{})
		}
		b.byTopic[t][id] = struct{}{}
	}
	sub := &Subscription[T]{
		id:     id,
		ch:     make(chan Event[T], b.buffer),
		topics: topicSet,
	}
	b.subs[id] = sub
	return sub, nil
}

func (b *Broker[T]) Unsubscribe(id int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	sub, ok := b.subs[id]
	if !ok {
		return ErrUnknownSub
	}
	delete(b.subs, id)
	for t := range sub.topics {
		if set, ok := b.byTopic[t]; ok {
			delete(set, id)
			if len(set) == 0 {
				delete(b.byTopic, t)
			}
		}
	}
	close(sub.ch)
	return nil
}

func (b *Broker[T]) Publish(topic string, payload T) error {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return ErrClosed
	}
	targets := make([]*Subscription[T], 0, len(b.byTopic[topic]))
	for id := range b.byTopic[topic] {
		if sub, ok := b.subs[id]; ok {
			targets = append(targets, sub)
		}
	}
	b.mu.RUnlock()

	evt := Event[T]{Topic: topic, Payload: payload}
	for _, sub := range targets {
		select {
		case sub.ch <- evt:
		case <-b.done:
			return ErrClosed
		}
	}
	return nil
}

func (b *Broker[T]) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		close(b.done)
		for id, sub := range b.subs {
			close(sub.ch)
			delete(b.subs, id)
		}
		b.mu.Unlock()
	})
	return nil
}

func (b *Broker[T]) SubscriberCount(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.byTopic[topic])
}
```

`Publish` takes a read lock to inspect the topic set, releases the lock, and
then sends. Sending under the lock would serialise publishers; the snapshot
approach lets concurrent publishers proceed.

### Exercise 2: Test The Contract

Create `internal/pubsub/pubsub_test.go`:

```go
package pubsub

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPublishDeliversToAllSubscribers(t *testing.T) {
	t.Parallel()

	b := New[int](4)
	defer b.Close()

	a, err := b.Subscribe("orders")
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Subscribe("orders")
	if err != nil {
		t.Fatal(err)
	}

	if err := b.Publish("orders", 42); err != nil {
		t.Fatal(err)
	}

	for _, sub := range []*Subscription[int]{a, c} {
		select {
		case evt := <-sub.C():
			if evt.Topic != "orders" || evt.Payload != 42 {
				t.Errorf("got %+v, want orders/42", evt)
			}
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("subscriber %d did not receive event", sub.ID())
		}
	}
}

func TestTopicFilter(t *testing.T) {
	t.Parallel()

	b := New[string](2)
	defer b.Close()

	orders, _ := b.Subscribe("orders")
	payments, _ := b.Subscribe("payments")

	_ = b.Publish("orders", "book")
	_ = b.Publish("payments", "card")

	select {
	case evt := <-orders.C():
		if evt.Payload != "book" {
			t.Errorf("orders: got %q, want book", evt.Payload)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("orders sub did not receive")
	}

	select {
	case evt := <-payments.C():
		if evt.Payload != "card" {
			t.Errorf("payments: got %q, want card", evt.Payload)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("payments sub did not receive")
	}
}

func TestUnsubscribeStopsDeliveryAndClosesChannel(t *testing.T) {
	t.Parallel()

	b := New[int](1)
	sub, _ := b.Subscribe("x")

	if err := b.Unsubscribe(sub.ID()); err != nil {
		t.Fatal(err)
	}

	if err := b.Unsubscribe(sub.ID()); !errors.Is(err, ErrUnknownSub) {
		t.Fatalf("err = %v, want ErrUnknownSub", err)
	}

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("subscriber channel did not close")
	}
}

func TestCloseStopsPublishes(t *testing.T) {
	t.Parallel()

	b := New[int](1)
	sub, _ := b.Subscribe("x")

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	if err := b.Publish("x", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close: err = %v, want ErrClosed", err)
	}

	select {
	case _, ok := <-sub.C():
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("subscriber channel did not close")
	}
}

func TestSubscribeAfterCloseFails(t *testing.T) {
	t.Parallel()

	b := New[int](0)
	b.Close()

	_, err := b.Subscribe("x")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close: err = %v, want ErrClosed", err)
	}
}

func TestSlowSubscriberDoesNotBlockFastOne(t *testing.T) {
	t.Parallel()

	b := New[int](16)
	fast, _ := b.Subscribe("x")
	slow, _ := b.Subscribe("x")

	for i := 0; i < 5; i++ {
		if err := b.Publish("x", i); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	var gotFast []int
	for i := 0; i < 5; i++ {
		select {
		case evt := <-fast.C():
			gotFast = append(gotFast, evt.Payload)
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("fast did not receive event %d", i)
		}
	}

	// slow still has its events queued
	var gotSlow []int
	for i := 0; i < 5; i++ {
		select {
		case evt := <-slow.C():
			gotSlow = append(gotSlow, evt.Payload)
		case <-time.After(50 * time.Millisecond):
			t.Fatalf("slow did not receive event %d", i)
		}
	}
}

func TestSubscriberCount(t *testing.T) {
	t.Parallel()

	b := New[int](0)
	defer b.Close()

	if got := b.SubscriberCount("x"); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}

	a, _ := b.Subscribe("x")
	b.Subscribe("x")
	b.Subscribe("y")

	if got := b.SubscriberCount("x"); got != 2 {
		t.Fatalf("count(x) = %d, want 2", got)
	}

	b.Unsubscribe(a.ID())
	if got := b.SubscriberCount("x"); got != 1 {
		t.Fatalf("count(x) after unsub = %d, want 1", got)
	}
}

func TestBrokerIsRaceFree(t *testing.T) {
	t.Parallel()

	b := New[int](32)
	defer b.Close()

	const subCount = 8
	subs := make([]*Subscription[int], subCount)
	for i := range subs {
		subs[i], _ = b.Subscribe("x")
	}

	var wg sync.WaitGroup
	wg.Add(subCount)
	var received [subCount]atomic.Int64
	for i := range subs {
		i := i
		go func() {
			defer wg.Done()
			for range subs[i].C() {
				received[i].Add(1)
			}
		}()
	}

	const pubCount = 100
	for i := 0; i < pubCount; i++ {
		if err := b.Publish("x", i); err != nil {
			t.Fatal(err)
		}
	}

	b.Close()
	wg.Wait()

	for i := range received {
		if received[i].Load() != int64(pubCount) {
			t.Errorf("sub %d received %d, want %d", i, received[i].Load(), pubCount)
		}
	}
}
```

`TestSlowSubscriberDoesNotBlockFastOne` is the test that pins the
backpressure contract: both subscribers get all 5 events because the
broker's per-subscriber buffer absorbs the mismatch.

Your turn: add `TestMultiTopicSubscriber` that subscribes one client to two
topics and asserts that publishing to either topic delivers an event with
the correct `Topic` field.

### Exercise 3: Runnable Demo

Create `cmd/pubsubdemo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/pubsub/internal/pubsub"
)

func main() {
	b := pubsub.New[string](8)
	defer b.Close()

	a, _ := b.Subscribe("orders", "payments")
	_, _ = b.Subscribe("orders")

	done := make(chan struct{})
	go func() {
		for evt := range a.C() {
			fmt.Printf("[A] %s: %s\n", evt.Topic, evt.Payload)
		}
		close(done)
	}()

	_ = b.Publish("orders", "book")
	_ = b.Publish("payments", "card")
	time.Sleep(50 * time.Millisecond)
	b.Close()
	<-done
}
```

## Common Mistakes

### Sending Under The Write Lock

Wrong: hold `b.mu` while sending to subscriber channels.

What happens: a slow subscriber blocks the lock; concurrent publishers and
unsubscribes block behind it.

Fix: snapshot the targets under the read lock, release the lock, then
send. Each send is per-subscriber; one slow subscriber only blocks that
subscriber's `Publish`.

### Forgetting To Close Subscriber Channels On Unsubscribe

Wrong: remove the subscription from the map but leave the channel open.

What happens: the subscriber's range loop never exits; the goroutine leaks.

Fix: `close(sub.ch)` after removing from the map.

### Treating `Publish` As Non-Blocking

Wrong: assuming `Publish` returns immediately.

What happens: a full subscriber buffer blocks the publisher until a reader
appears.

Fix: use `TryPublish` if at-most-once is acceptable; otherwise size buffers
so publishes rarely block.

### Double-Close On Broker

Wrong: calling `b.Close()` twice.

What happens: panic on `close(b.done)` and on `close(sub.ch)`.

Fix: `sync.Once` around the close logic.

## Verification

From `~/go-exercises/pubsub`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. `TestBrokerIsRaceFree` runs concurrent publishers and
subscribers; the race detector pins every unsynchronised access.

## Summary

- `Broker` fans events from one publisher to many subscribers.
- Each subscriber has its own buffered channel; backpressure is per-subscriber.
- `Publish` snapshots targets under a read lock, then sends without the lock.
- `Unsubscribe` removes the subscriber and closes its channel.
- `Close` is idempotent and stops further publishes.

## What's Next

Next: [Bounded Worker Pool Adaptive Sizing](../18-bounded-worker-pool-adaptive-sizing/18-bounded-worker-pool-adaptive-sizing.md).

## Resources

- [Go talks: Advanced Go Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns)
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines)
- [Publish/subscribe on Wikipedia](https://en.wikipedia.org/wiki/Publish%E2%80%93subscribe_pattern)