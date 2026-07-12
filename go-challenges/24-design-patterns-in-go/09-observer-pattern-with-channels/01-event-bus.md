# Exercise 1: The Event Bus

An event bus is the observer pattern made concrete: publishers send typed events, subscribers receive them on their own channels, and neither side knows how many of the other exists. This exercise builds a complete bus — `Subscribe`, `Publish`, `Close`, and two narrow accessors — and pins its contract with tests, including a `-race` test that proves the fan-out cannot panic with "send on closed channel" while subscribers cancel concurrently.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bus.go               EventBus, Event, Subscribe, Publish, Close, accessors
cmd/
  demo/
    main.go          register two subscribers, publish, unsubscribe one, observe
bus_test.go          contract tests + a -race concurrent publish/cancel sweep
```

- Files: `bus.go`, `cmd/demo/main.go`, `bus_test.go`.
- Implement: `EventBus` with `Subscribe(eventType string, bufSize int) (<-chan Event, func())`, `Publish(Event) error`, `Close()`, `SubscriberCount(string) int`, and `EventLog() []Event`.
- Test: `bus_test.go` checks delivery, drop-on-full, idempotent cancel, the error contracts, fan-out to many subscribers, and concurrent publish/cancel under the race detector.
- Verify: `go test -race ./...`

### The shape of the bus, and the one hazard that drives its design

The bus holds a `map[string][]chan Event`: each event type maps to the slice of channels subscribed to it. `Subscribe` makes a buffered channel, appends it under the lock, and returns the receive-only end plus a `cancel` closure. `Publish` looks up the slice for the event's type and hands the event to each channel with a non-blocking send. `Close` shuts the whole bus. That is the entire structure; the difficulty is entirely in one interaction.

The hazard is that this design both closes subscriber channels (so `for range` exits when a subscriber cancels) and sends to them non-blockingly (so one slow subscriber cannot stall the publisher). Those two facts collide: it is a panic to send on a closed channel, and a non-blocking send does **not** take the `default` arm when the channel is closed — the send case is selected and it panics. So if `Publish` snapshots the subscriber slice, releases the lock, and only then sends, a concurrent `cancel` or `Close` can close a channel in that gap and the next send panics with "send on closed channel."

The fix is to perform the non-blocking fan-out while still holding the lock that `cancel` and `Close` take to close channels. Because every send has a `default` arm, the loop never blocks, so holding the lock across it is bounded by the number of subscribers and cannot deadlock. And because closing a channel also needs that lock, no channel can be closed between the moment `Publish` selects it and the moment it sends. "Send" and "close" become mutually exclusive, and the panic is structurally impossible rather than merely unlikely. The cost is that `Publish` serializes with `Subscribe`/`cancel`/`Close`; for a bus this is the right trade, and it is why a handler must never publish synchronously on its own goroutine (it would re-enter the held lock and deadlock).

The second subtlety is closing a channel exactly once. `cancel` should be safe to call twice, and a bus-wide `Close` also closes every subscriber channel, so an unsubscribe racing a shutdown could close the same channel twice and panic. A `sync.Once` collapses repeated cancels to one, and each close path first checks, under the lock, that the channel is still in the slice before closing it. Whoever removes it first owns the close; the other finds it absent and does nothing.

Create `bus.go`:

```go
package observer

import (
	"errors"
	"sync"
	"time"
)

// Well-known event types carried on the bus.
const (
	OrderPlaced    = "order.placed"
	OrderShipped   = "order.shipped"
	OrderCancelled = "order.cancelled"
)

var (
	ErrUnknownEvent = errors.New("observer: unknown event type")
	ErrBusClosed    = errors.New("observer: bus is closed")
)

// Event is the unit published on the bus. Payload is untyped because one bus
// routes many event types by string key; subscribers assert the concrete type.
type Event struct {
	Type    string
	Payload any
	Time    time.Time
}

// EventBus fans typed events out to per-subscriber channels. It is safe for
// concurrent Subscribe, Publish, Close, and cancel.
type EventBus struct {
	mu          sync.Mutex
	subscribers map[string][]chan Event
	closed      bool
	log         []Event
}

func NewEventBus() *EventBus {
	return &EventBus{subscribers: make(map[string][]chan Event)}
}

// Subscribe returns a receive-only channel for eventType and a cancel function.
// bufSize is clamped to at least 1 so a single non-blocking Publish can always
// land one event. Subscribing to an empty type, or to a closed bus, returns an
// already-closed channel and a no-op cancel, so the caller's for-range exits at
// once instead of blocking forever.
func (b *EventBus) Subscribe(eventType string, bufSize int) (<-chan Event, func()) {
	if eventType == "" {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	if bufSize < 1 {
		bufSize = 1
	}
	ch := make(chan Event, bufSize)

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch, func() {}
	}
	b.subscribers[eventType] = append(b.subscribers[eventType], ch)
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			subs := b.subscribers[eventType]
			for i, s := range subs {
				if s == ch {
					b.subscribers[eventType] = append(subs[:i], subs[i+1:]...)
					close(ch)
					return
				}
			}
		})
	}
	return ch, cancel
}

// Publish records the event in the log and delivers it to every subscriber of
// its type with a non-blocking send. A subscriber whose buffer is full silently
// drops the event. The fan-out runs under the lock so a channel cannot be closed
// by cancel or Close between selecting it and sending to it.
func (b *EventBus) Publish(event Event) error {
	if event.Type == "" {
		return ErrUnknownEvent
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	b.log = append(b.log, event)
	for _, ch := range b.subscribers[event.Type] {
		select {
		case ch <- event:
		default:
		}
	}
	return nil
}

// Close marks the bus closed and closes every subscriber channel exactly once.
// It is idempotent. Buffered events already in a channel are still drained by
// the subscriber's for-range before it sees the close.
func (b *EventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, subs := range b.subscribers {
		for _, ch := range subs {
			close(ch)
		}
	}
	b.subscribers = make(map[string][]chan Event)
}

// SubscriberCount reports the number of live subscribers for eventType.
func (b *EventBus) SubscriberCount(eventType string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers[eventType])
}

// EventLog returns a copy of every event accepted by Publish, in order.
func (b *EventBus) EventLog() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Event, len(b.log))
	copy(out, b.log)
	return out
}
```

`Subscribe` returns the receive-only side so a caller cannot accidentally send on a subscriber channel; only the bus sends. `cancel`'s `sync.Once` plus the membership check make the close happen exactly once even if `cancel` is called twice or races `Close`. `Publish` holds the lock across the non-blocking fan-out, which is what makes the "send on closed channel" panic structurally impossible. `EventLog` copies its slice so a caller cannot mutate the bus's internal log.

### The runnable demo

The demo is written to be deterministic: it publishes into buffered channels and then drains them in a fixed order from `main`, so the printed lines do not depend on goroutine scheduling. It registers two subscribers, publishes two orders, drains each subscriber, unsubscribes one, publishes a third order, and shows that the cancelled subscriber's channel is closed while the live one still receives.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/event-bus"
)

type OrderPlacedPayload struct {
	ID    string
	Email string
	Total float64
}

func main() {
	bus := observer.NewEventBus()
	defer bus.Close()

	inventory, cancelInv := bus.Subscribe(observer.OrderPlaced, 10)
	analytics, cancelAna := bus.Subscribe(observer.OrderPlaced, 10)
	fmt.Printf("subscribers for %q: %d\n", observer.OrderPlaced, bus.SubscriberCount(observer.OrderPlaced))

	orders := []OrderPlacedPayload{
		{ID: "ORD-001", Email: "alice@example.com", Total: 149.97},
		{ID: "ORD-002", Email: "bob@example.com", Total: 29.99},
	}
	for _, p := range orders {
		_ = bus.Publish(observer.Event{Type: observer.OrderPlaced, Payload: p, Time: time.Now()})
	}

	for range orders {
		p := (<-inventory).Payload.(OrderPlacedPayload)
		fmt.Printf("[Inventory] reducing stock for %s\n", p.ID)
	}
	for range orders {
		p := (<-analytics).Payload.(OrderPlacedPayload)
		fmt.Printf("[Analytics] recording $%.2f for %s\n", p.Total, p.ID)
	}

	cancelAna()
	fmt.Printf("after cancel, subscribers: %d\n", bus.SubscriberCount(observer.OrderPlaced))

	_ = bus.Publish(observer.Event{
		Type:    observer.OrderPlaced,
		Payload: OrderPlacedPayload{ID: "ORD-003", Email: "charlie@example.com", Total: 9.99},
		Time:    time.Now(),
	})

	fmt.Printf("[Inventory] reducing stock for %s\n", (<-inventory).Payload.(OrderPlacedPayload).ID)
	if _, ok := <-analytics; !ok {
		fmt.Println("[Analytics] channel closed after cancel")
	}

	cancelInv()
	fmt.Printf("events logged: %d\n", len(bus.EventLog()))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
subscribers for "order.placed": 2
[Inventory] reducing stock for ORD-001
[Inventory] reducing stock for ORD-002
[Analytics] recording $149.97 for ORD-001
[Analytics] recording $29.99 for ORD-002
after cancel, subscribers: 1
[Inventory] reducing stock for ORD-003
[Analytics] channel closed after cancel
events logged: 3
```

### Tests

The tests pin every clause of the contract. Delivery, drop-on-full, and idempotent cancel cover the channel mechanics; the two error tests pin `Publish`'s rejections; the fan-out test proves every subscriber of a type receives the event; and `TestConcurrentPublishAndCancel` hammers `Publish`, `Subscribe`, and `cancel` from many goroutines so the race detector would catch the "send on closed channel" panic the lock is there to prevent. `TestSubscribe_IgnoresUnknownEvent` proves a subscriber only hears its own type while the bus still logs the foreign event.

Create `bus_test.go`:

```go
package observer

import (
	"sync"
	"testing"
	"time"
)

func TestSubscribe_ReceivesPublishedEvents(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	ch, cancel := bus.Subscribe(OrderPlaced, 4)
	defer cancel()

	want := Event{Type: OrderPlaced, Payload: "ORD-1", Time: time.Now()}
	if err := bus.Publish(want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.Type != want.Type || got.Payload != want.Payload {
			t.Errorf("event = %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestPublish_DropsWhenSubscriberBufferFull(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	ch, cancel := bus.Subscribe(OrderPlaced, 1)
	defer cancel()

	if err := bus.Publish(Event{Type: OrderPlaced, Payload: 1, Time: time.Now()}); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	if err := bus.Publish(Event{Type: OrderPlaced, Payload: 2, Time: time.Now()}); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}

	if got := len(ch); got != 1 {
		t.Errorf("buffered events = %d, want 1 (second send dropped)", got)
	}
	if len(bus.EventLog()) != 2 {
		t.Errorf("EventLog len = %d, want 2", len(bus.EventLog()))
	}
}

func TestCancel_RemovesSubscriberAndClosesChannel(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	ch, cancel := bus.Subscribe(OrderPlaced, 1)

	if bus.SubscriberCount(OrderPlaced) != 1 {
		t.Errorf("SubscriberCount before cancel = %d, want 1", bus.SubscriberCount(OrderPlaced))
	}

	cancel()
	cancel() // idempotent: must not panic or double-close.

	if bus.SubscriberCount(OrderPlaced) != 0 {
		t.Errorf("SubscriberCount after cancel = %d, want 0", bus.SubscriberCount(OrderPlaced))
	}

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel to be closed after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for close")
	}
}

func TestPublish_RejectsAfterClose(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	bus.Close()
	bus.Close() // idempotent.

	if err := bus.Publish(Event{Type: OrderPlaced, Payload: "x", Time: time.Now()}); err != ErrBusClosed {
		t.Errorf("err = %v, want ErrBusClosed", err)
	}
}

func TestPublish_RejectsEmptyType(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	defer bus.Close()

	if err := bus.Publish(Event{Type: "", Payload: "x", Time: time.Now()}); err != ErrUnknownEvent {
		t.Errorf("err = %v, want ErrUnknownEvent", err)
	}
}

func TestSubscribe_IgnoresUnknownEvent(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	defer bus.Close()

	ch, cancel := bus.Subscribe(OrderPlaced, 1)
	defer cancel()

	if err := bus.Publish(Event{Type: OrderShipped, Payload: "ship", Time: time.Now()}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if got := len(ch); got != 0 {
		t.Errorf("subscriber received %d events for a foreign type, want 0", got)
	}
	if len(bus.EventLog()) != 1 {
		t.Errorf("EventLog len = %d, want 1", len(bus.EventLog()))
	}
}

func TestMultipleSubscribers_AllReceiveEvent(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	defer bus.Close()

	const n = 5
	chans := make([]<-chan Event, n)
	cancels := make([]func(), n)
	for i := range n {
		chans[i], cancels[i] = bus.Subscribe(OrderPlaced, 1)
	}

	_ = bus.Publish(Event{Type: OrderPlaced, Payload: "x", Time: time.Now()})

	var wg sync.WaitGroup
	for i, ch := range chans {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case _, ok := <-ch:
				if !ok {
					t.Errorf("subscriber %d channel closed prematurely", i)
				}
			case <-time.After(time.Second):
				t.Errorf("subscriber %d timeout", i)
			}
		}()
	}
	wg.Wait()

	for _, c := range cancels {
		c()
	}
}

func TestConcurrentPublishAndCancel(t *testing.T) {
	t.Parallel()

	bus := NewEventBus()
	defer bus.Close()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 500 {
				_ = bus.Publish(Event{Type: OrderPlaced, Payload: j, Time: time.Now()})
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, cancel := bus.Subscribe(OrderPlaced, 1)
				cancel()
			}
		}()
	}
	wg.Wait()
}
```

## Review

The bus is correct when publishing and closing are mutually exclusive. The proof is structural: `Publish` runs its non-blocking fan-out while holding `b.mu`, and both `cancel` and `Close` need `b.mu` to close a channel, so no channel is ever closed between the `select` that picks it and the send that delivers to it — the "send on closed channel" panic cannot occur. `TestConcurrentPublishAndCancel` is the empirical check: run it under `-race` and a snapshot-then-unlock-then-send design would panic within a few iterations, while the locked design passes every time.

Common mistakes for this feature. The first is the snapshot-then-send fan-out: copying the subscriber slice, releasing the lock for "concurrency," and sending afterward reintroduces exactly the panic the lock prevents — a non-blocking send to a channel a concurrent `cancel` just closed does not take `default`, it panics. The second is closing a channel twice: a `cancel` that closes unconditionally will double-close when called twice or when it races `Close`, so the close must be guarded by `sync.Once` and gated on the channel still being registered. The third is returning the bidirectional `chan Event` instead of `<-chan Event`, which lets a buggy subscriber send on its own channel and corrupt the contract. Confirm all of `go test -race ./...` passes; the drop-on-full and foreign-type tests use `len(ch)` to read the buffer occupancy deterministically rather than racing a timeout.

## Resources

- [Observer pattern](https://refactoring.guru/design-patterns/observer) — the classic intent and structure the channel design realizes.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — closing a channel to signal end-of-stream and the receiver idioms that react to it.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels, non-blocking sends with `select`/`default`, and the close semantics this bus relies on.
- [`sync` package](https://pkg.go.dev/sync) — `Mutex` and `Once`, the two primitives that make the fan-out and the exactly-once close safe.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-safe-subscribe-unsubscribe.md](02-safe-subscribe-unsubscribe.md)
