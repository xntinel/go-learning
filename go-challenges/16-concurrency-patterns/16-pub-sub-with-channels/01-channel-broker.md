# Exercise 1: A Mutex-Guarded Topic Broker

This is the workhorse pub/sub: a generic broker that holds its subscribers in a map behind a `sync.RWMutex`, delivers each published event to every subscriber of the topic, and never lets one slow subscriber stall a publisher. The whole exercise turns on one discipline — sending an event and closing a subscriber channel must be mutually exclusive — which is the fix for the send-on-closed-channel panic that the obvious design walks straight into.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
pubsub.go            Broker[T], Subscription[T], Event[T], New, Subscribe, Unsubscribe, Publish, Close
cmd/
  demo/
    main.go          deliver to two topics, then flood an undrained subscriber to force drops
pubsub_test.go       delivery, topic filtering, drop accounting, and a concurrent publish/unsubscribe race
```

- Files: `pubsub.go`, `cmd/demo/main.go`, `pubsub_test.go`.
- Implement: `Broker[T]` with `New[T](buffer int)`, `Subscribe(topics ...string)`, `Unsubscribe(id int)`, `Publish(topic string, payload T) (int, error)`, `Close()`, and `SubscriberCount(topic string)`, plus `Subscription[T]` with `C()`, `ID()`, and `Dropped()`.
- Test: `pubsub_test.go` pins delivery to every subscriber, topic filtering, multi-topic delivery, the drop-on-full-buffer policy with a counter, idempotent `Close`, and a concurrent publish/unsubscribe loop that must not panic under `-race`.
- Verify: `go test -run 'TestPublish|TestTopic|TestMulti|TestUnsubscribe|TestClose|TestSubscribe|TestSlow' -race ./...`

Set up the module:

```bash
mkdir -p channel-broker/cmd/demo && cd channel-broker
go mod init example.com/channel-broker
```

### Why send and close must be mutually exclusive

The data structure is unremarkable: a `map[int]*Subscription` for lookup by id, a `map[string]map[int]struct{}` to find the subscribers of a topic without scanning, and a `sync.RWMutex` over both. Subscribe and unsubscribe take the write lock because they mutate the maps; `SubscriberCount` takes the read lock because it only reads. The interesting decision is what lock `Publish` holds, and for how long.

The naive design takes the read lock, copies the list of target subscribers into a local slice, releases the lock, and then sends to each one. The instinct behind it is sound in general — do not hold a lock across a potentially slow operation — but here it is fatal. Releasing the lock before the send opens a window in which another goroutine acquires the write lock, runs `Unsubscribe`, and closes a target's channel. The send that follows then executes `ch <- evt` on a closed channel, which panics. The bug is timing-dependent, so it passes a single-threaded test and a lightly loaded demo, then crashes a server under concurrent subscribe/unsubscribe churn. That is the worst kind of bug: invisible until load.

The fix is to make a send and a close mutually exclusive, and a `sync.RWMutex` already gives the tool to do it. Close a channel only under the write lock (which `Unsubscribe` and `Close` both hold), and perform the send only under the read lock. A read lock and a write lock can never be held at the same time, so a send can never coincide with a close, and the panic becomes structurally impossible rather than merely unlikely. The cost is that the send now happens with the read lock held — so it must not block, or it would hold the lock open and starve writers. That is why the send is non-blocking: a `select` with a `default`. If the subscriber's buffer has room the event goes in; if it is full the event is dropped and counted, and either way the read lock is released immediately. The drop policy is not a compromise forced by the locking; it is the deliberate slow-subscriber policy that lets `Publish` guarantee it never blocks, with the `Dropped()` counter making the loss observable rather than silent.

Create `pubsub.go`:

```go
package pubsub

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrClosed is returned by Subscribe and Publish after the broker is closed.
var ErrClosed = errors.New("pubsub: broker closed")

// ErrUnknownSub is returned by Unsubscribe for an id that is not registered.
var ErrUnknownSub = errors.New("pubsub: unknown subscriber")

// Event is a published message tagged with the topic it was published on.
type Event[T any] struct {
	Topic   string
	Payload T
}

// Subscription is a handle to one subscriber's delivery channel.
type Subscription[T any] struct {
	id      int
	ch      chan Event[T]
	topics  map[string]struct{}
	dropped atomic.Int64
}

// C returns the receive-only channel the subscriber ranges over. The broker
// closes it on Unsubscribe or Close, which ends the range loop.
func (s *Subscription[T]) C() <-chan Event[T] { return s.ch }

// ID returns the subscription's broker-assigned identifier.
func (s *Subscription[T]) ID() int { return s.id }

// Dropped reports how many events were discarded for this subscriber because
// its buffer was full when an event was published.
func (s *Subscription[T]) Dropped() int64 { return s.dropped.Load() }

// Broker is an in-process topic publish/subscribe broker. Each subscriber owns
// a buffered channel; Publish delivers without blocking and drops (counting the
// drop) when a subscriber's buffer is full, so one slow subscriber never stalls
// a publisher or another subscriber.
type Broker[T any] struct {
	mu      sync.RWMutex
	subs    map[int]*Subscription[T]
	byTopic map[string]map[int]struct{}
	nextID  int
	buffer  int
	closed  bool
	once    sync.Once
}

// New returns a broker whose per-subscriber channels are buffered to depth
// buffer. A negative buffer is treated as zero.
func New[T any](buffer int) *Broker[T] {
	if buffer < 0 {
		buffer = 0
	}
	return &Broker[T]{
		subs:    make(map[int]*Subscription[T]),
		byTopic: make(map[string]map[int]struct{}),
		buffer:  buffer,
	}
}

// Subscribe registers a subscriber for the given topics and returns its handle.
// It returns ErrClosed if the broker is closed.
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

// Unsubscribe removes the subscriber and closes its channel, ending its range
// loop. It returns ErrUnknownSub if the id is not registered. The close happens
// under the write lock, so it can never coincide with a Publish send.
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

// Publish delivers payload to every subscriber of topic and returns the number
// of subscribers that accepted it. A subscriber whose buffer is full does not
// receive the event; its Dropped counter is incremented instead. The send runs
// under the read lock and is non-blocking, so Publish never blocks and can
// never send on a channel that Unsubscribe is closing. It returns ErrClosed if
// the broker is closed.
func (b *Broker[T]) Publish(topic string, payload T) (delivered int, err error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return 0, ErrClosed
	}
	evt := Event[T]{Topic: topic, Payload: payload}
	for id := range b.byTopic[topic] {
		sub := b.subs[id]
		select {
		case sub.ch <- evt:
			delivered++
		default:
			sub.dropped.Add(1)
		}
	}
	return delivered, nil
}

// Close stops the broker, closes every subscriber channel exactly once, and is
// idempotent. After Close, Subscribe and Publish return ErrClosed.
func (b *Broker[T]) Close() error {
	b.once.Do(func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.closed = true
		for id, sub := range b.subs {
			close(sub.ch)
			delete(b.subs, id)
		}
		b.byTopic = make(map[string]map[int]struct{})
	})
	return nil
}

// SubscriberCount returns the number of subscribers currently registered for
// topic.
func (b *Broker[T]) SubscriberCount(topic string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.byTopic[topic])
}
```

Three details carry the weight. `Publish` holds the read lock across the whole delivery loop, not just the lookup, which is the entire point: the lookup and the send are one critical section that excludes any close. The send is `select { case sub.ch <- evt: ... default: ... }`, so a full buffer takes the `default` branch and the loop moves on instead of blocking under the lock. And `Close` is wrapped in a `sync.Once` and deletes each subscription as it closes it, so a second `Close`, or a `Close` racing an `Unsubscribe`, can never close the same channel twice — the `Once` runs the body at most once, and the write lock serializes it against `Unsubscribe`.

### The runnable demo

The demo makes both halves of the contract concrete and does it deterministically, so its output is reproducible. It subscribes one client to two topics, publishes one event to each, and drains them in order to show topic-tagged delivery. Then it subscribes a second client that nobody reads, floods its topic past the buffer depth, and prints the buffered count alongside the drop count to show the slow-subscriber policy in action.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/channel-broker"
)

func main() {
	b := pubsub.New[string](4)
	defer b.Close()

	// One subscriber on two topics; drained synchronously in publish order.
	sub, _ := b.Subscribe("orders", "payments")
	d1, _ := b.Publish("orders", "book")
	d2, _ := b.Publish("payments", "card")
	fmt.Printf("delivered orders=%d payments=%d\n", d1, d2)
	for i := 0; i < 2; i++ {
		evt := <-sub.C()
		fmt.Printf("recv %s/%s\n", evt.Topic, evt.Payload)
	}

	// A second subscriber nobody drains: its buffer of 4 fills and the rest drop.
	slow, _ := b.Subscribe("metrics")
	for i := 0; i < 6; i++ {
		b.Publish("metrics", fmt.Sprintf("m%d", i))
	}
	fmt.Printf("slow buffered=%d dropped=%d\n", len(slow.C()), slow.Dropped())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered orders=1 payments=1
recv orders/book
recv payments/card
slow buffered=4 dropped=2
```

### Tests

The tests pin every clause of the contract, and one of them is the reason the locking is shaped the way it is. `TestPublishDeliversToAllSubscribers` and `TestTopicFilter` establish the basic fan-out and that an event only reaches subscribers of its topic. `TestMultiTopicSubscriber` is the converted "subscribe one client to two topics" case: a single subscription on two topics receives a tagged event from each. `TestSlowSubscriberDropsAndCounts` is the policy test — a buffer of two, five publishes, no reader, exactly two delivered and three dropped — and `TestSlowSubscriberDoesNotBlockFastOne` shows a fast and a slow subscriber both receive a burst that fits in the buffer. `TestUnsubscribeClosesChannel`, `TestCloseStopsPublishes`, and `TestSubscribeAfterCloseFails` pin the lifecycle, including that `Close` is idempotent. The centerpiece is `TestPublishUnsubscribeRace`: it runs eight publishers in a tight loop while the main goroutine subscribes and unsubscribes two hundred times, all concurrently. On the broken design that sends after releasing the lock, this test panics with "send on closed channel"; on the design here it passes cleanly under `-race`, which is the whole proof that send and close are mutually exclusive.

Create `pubsub_test.go`:

```go
package pubsub

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPublishDeliversToAllSubscribers(t *testing.T) {
	t.Parallel()

	b := New[int](4)
	defer b.Close()

	a, _ := b.Subscribe("orders")
	c, _ := b.Subscribe("orders")

	n, err := b.Publish("orders", 42)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("delivered = %d, want 2", n)
	}

	for _, sub := range []*Subscription[int]{a, c} {
		select {
		case evt := <-sub.C():
			if evt.Topic != "orders" || evt.Payload != 42 {
				t.Errorf("got %+v, want orders/42", evt)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d did not receive", sub.ID())
		}
	}
}

func TestTopicFilter(t *testing.T) {
	t.Parallel()

	b := New[string](2)
	defer b.Close()

	orders, _ := b.Subscribe("orders")
	payments, _ := b.Subscribe("payments")

	if _, err := b.Publish("orders", "book"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish("payments", "card"); err != nil {
		t.Fatal(err)
	}

	if evt := <-orders.C(); evt.Payload != "book" {
		t.Errorf("orders: got %q, want book", evt.Payload)
	}
	if evt := <-payments.C(); evt.Payload != "card" {
		t.Errorf("payments: got %q, want card", evt.Payload)
	}
	if len(orders.C()) != 0 {
		t.Errorf("orders received a payments event")
	}
}

func TestMultiTopicSubscriber(t *testing.T) {
	t.Parallel()

	b := New[string](4)
	defer b.Close()

	sub, _ := b.Subscribe("orders", "payments")
	if _, err := b.Publish("orders", "book"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish("payments", "card"); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		select {
		case evt := <-sub.C():
			got[evt.Topic] = evt.Payload
		case <-time.After(time.Second):
			t.Fatal("did not receive both events")
		}
	}
	if got["orders"] != "book" || got["payments"] != "card" {
		t.Fatalf("got %v, want orders=book payments=card", got)
	}
}

func TestSlowSubscriberDropsAndCounts(t *testing.T) {
	t.Parallel()

	b := New[int](2)
	defer b.Close()

	sub, _ := b.Subscribe("x")

	delivered := 0
	for i := 0; i < 5; i++ {
		d, err := b.Publish("x", i)
		if err != nil {
			t.Fatal(err)
		}
		delivered += d
	}
	if delivered != 2 {
		t.Fatalf("delivered = %d, want 2 (buffer depth)", delivered)
	}
	if got := sub.Dropped(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
}

func TestSlowSubscriberDoesNotBlockFastOne(t *testing.T) {
	t.Parallel()

	b := New[int](16)
	defer b.Close()

	fast, _ := b.Subscribe("x")
	slow, _ := b.Subscribe("x")

	for i := 0; i < 5; i++ {
		if _, err := b.Publish("x", i); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	for i := 0; i < 5; i++ {
		if evt := <-fast.C(); evt.Payload != i {
			t.Fatalf("fast: got %d, want %d", evt.Payload, i)
		}
	}
	// The slow subscriber's events are still queued; none were lost.
	if got := slow.Dropped(); got != 0 {
		t.Fatalf("slow dropped = %d, want 0", got)
	}
	if got := len(slow.C()); got != 5 {
		t.Fatalf("slow buffered = %d, want 5", got)
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	t.Parallel()

	b := New[int](1)
	defer b.Close()

	sub, _ := b.Subscribe("x")
	if err := b.Unsubscribe(sub.ID()); err != nil {
		t.Fatal(err)
	}
	if err := b.Unsubscribe(sub.ID()); !errors.Is(err, ErrUnknownSub) {
		t.Fatalf("second Unsubscribe = %v, want ErrUnknownSub", err)
	}
	if _, ok := <-sub.C(); ok {
		t.Fatal("expected closed channel after Unsubscribe")
	}
}

func TestCloseStopsPublishes(t *testing.T) {
	t.Parallel()

	b := New[int](1)
	sub, _ := b.Subscribe("x")

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}
	if _, err := b.Publish("x", 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
	if _, ok := <-sub.C(); ok {
		t.Fatal("expected closed channel after Close")
	}
}

func TestSubscribeAfterCloseFails(t *testing.T) {
	t.Parallel()

	b := New[int](0)
	b.Close()

	if _, err := b.Subscribe("x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
}

func TestPublishUnsubscribeRace(t *testing.T) {
	b := New[int](8)
	defer b.Close()

	const workers = 8
	stop := make(chan struct{})
	var pubWG sync.WaitGroup
	for i := 0; i < workers; i++ {
		pubWG.Add(1)
		go func() {
			defer pubWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := b.Publish("x", 1); errors.Is(err, ErrClosed) {
						return
					}
				}
			}
		}()
	}

	// Churn subscriptions concurrently with the publishers. On a broker that
	// sends after releasing the lock, this panics on a closed channel.
	var drainWG sync.WaitGroup
	for i := 0; i < 200; i++ {
		sub, err := b.Subscribe("x")
		if err != nil {
			t.Fatal(err)
		}
		drainWG.Add(1)
		go func() {
			defer drainWG.Done()
			for range sub.C() {
			}
		}()
		if err := b.Unsubscribe(sub.ID()); err != nil {
			t.Fatal(err)
		}
	}

	close(stop)
	pubWG.Wait()
	drainWG.Wait()
}
```

## Review

The broker is correct when an event reaches every subscriber of its topic and no others, when a full buffer drops and counts rather than blocking, and when no concurrent mix of publish, subscribe, unsubscribe, and close ever panics. The drop test is the one that pins the policy: a buffer of two and five publishes must leave exactly two delivered and three dropped, deterministically, because no one is reading. The lifecycle tests must show a closed channel after both `Unsubscribe` and `Close`, a second `Close` returning nil, and `Publish` and `Subscribe` returning `ErrClosed` afterward. The race test is the proof of the central design choice: it must pass under `go test -race` with no "send on closed channel" panic, which is only true because the send sits under the read lock and every close sits under the write lock.

Common mistakes for this feature. Sending after releasing the read lock is the headline bug — it reads like an optimization and panics under churn; keep the send inside the read-locked critical section. Making that in-lock send blocking instead of non-blocking trades the panic for a stall: a full buffer would hold the read lock open and starve every writer, so the send must be a `select` with a `default`. Closing a subscriber channel in both `Unsubscribe` and `Close` double-closes it and panics; delete the subscription from the map as you close it so the later `Close` cannot find it. And dropping an event without incrementing `Dropped()` turns a visible policy into silent data loss — the counter is what makes the drop an engineering decision instead of a bug.

## Resources

- [Go Blog: Advanced Go Concurrency Patterns](https://go.dev/blog/advanced-go-concurrency-patterns) — the one-inbound-N-outbound figure and the channel-and-select vocabulary this broker is built from.
- [`sync` package](https://pkg.go.dev/sync) — `RWMutex` semantics: read and write locks are mutually exclusive, which is what makes send-and-close safe here.
- [Go Blog: Share Memory By Communicating](https://go.dev/blog/codelab-share) — why Go prefers passing values over channels to sharing memory, the model this broker follows for delivery.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-domain-event-bus.md](02-domain-event-bus.md)
