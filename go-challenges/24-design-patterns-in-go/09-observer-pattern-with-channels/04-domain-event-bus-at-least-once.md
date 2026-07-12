# Exercise 4: A Domain-Event Bus with At-Least-Once Delivery

The earlier buses chose availability over delivery: a full subscriber drops the event. A domain-event bus that drives money-moving side effects (charge a card, decrement stock, write an audit row) cannot drop. This exercise builds the opposite policy as a senior would: each subscriber owns a bounded queue drained by its own worker goroutine, publishers block on a full queue (backpressure, not loss), a handler that fails is redelivered up to a retry budget (at-least-once with an explicit ack), and `Shutdown` drains every queued event and joins every worker so the process leaves no goroutine behind. The two `-race` tests prove concurrent publish/subscribe is safe and that shutdown both finishes all committed work and leaks nothing.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bus.go               Bus, Event, Handler; Subscribe, Publish, Shutdown, Stats
cmd/
  demo/
    main.go          two subscribers, one flaky; publish, drain, print counters
bus_test.go          retry/ack accounting + -race concurrent pub/sub + clean drain
```

- Files: `bus.go`, `cmd/demo/main.go`, `bus_test.go`.
- Implement: `Bus` with `Subscribe(name, topic string, queueSize, maxRetry int, h Handler) error`, `Publish(topic string, payload any) error`, `Shutdown()`, and `Stats() []Stats`.
- Test: `bus_test.go` proves retry-until-ack, retry-budget exhaustion, that every committed event is drained on shutdown with no goroutine leak, and that concurrent publish/subscribe/shutdown is race-free.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/24-design-patterns-in-go/09-observer-pattern-with-channels/04-domain-event-bus-at-least-once/cmd/demo && cd go-solutions/24-design-patterns-in-go/09-observer-pattern-with-channels/04-domain-event-bus-at-least-once
```

### Backpressure instead of loss, and where the goroutines live

The design inverts the drop policy. Each subscriber gets a buffered channel of a fixed size — its bounded queue — and a dedicated worker goroutine that pulls events off the queue and runs the handler. `Publish` does a *blocking* send into each matching subscriber's queue. When a queue is full, the send blocks, and that blocking is the backpressure: a slow consumer slows down its publishers rather than silently losing their events. Because every subscriber has its own queue and its own worker, a slow consumer on one topic does not stall a fast consumer on another; backpressure is per-subscriber.

The hazard with blocking sends is shutdown. If `Shutdown` simply closed every queue to tell the workers to stop, a publisher blocked on `queue <- ev` could panic with "send on closed channel" the instant the queue closes. So this bus never closes the queues. Instead it signals shutdown two ways. A `closing` channel, closed once in `Shutdown`, gives every blocked publisher an escape arm in its `select`; and a `context.Context`, cancelled after publishers have drained away, tells each worker to switch into a final drain-and-exit loop. The worker never relies on a closed queue to terminate, so no send can ever race a close.

### The shutdown handshake: settle publishers, then drain workers

`Shutdown` runs an ordered handshake so that "graceful drain" is a real guarantee and not a hope:

1. Under the lock, set `closed = true` (new `Publish` calls now fail fast) and `close(closing)` (publishers blocked on a full queue wake on their `select`'s `<-closing` arm and return `ErrBusClosed`).
2. `pubWg.Wait()` — every in-flight `Publish` has either finished enqueueing or bailed out. After this returns, no goroutine is touching any queue, so every subscriber's buffer is now stable.
3. `cancel()` — each worker's `select` takes its `<-ctx.Done()` arm and enters a loop that drains everything still buffered (a `select` with a `default` that returns when the queue is empty), processing each event through the handler.
4. `wg.Wait()` — block until every worker goroutine has returned.

The ordering is the whole point. Because publishers are fully settled (step 2) *before* the workers are told to drain (step 3), the buffers cannot grow while they are being drained, so every event that a successful `Publish` enqueued is processed before `Shutdown` returns. And because `Shutdown` joins the workers with `wg.Wait()` (step 4), the moment it returns is a proof that no worker goroutine survives — the deterministic, non-flaky way to assert "no goroutine leak." `pubWg` counts publishers and `wg` counts workers; the publisher count is incremented under the lock while `closed` is still false, and `Shutdown` sets `closed` under that same lock before waiting, so the mutex orders every `Add` before the `Wait` and the classic WaitGroup misuse cannot occur.

### At-least-once with an explicit ack

A handler returns `error`. Returning `nil` is the ack: the event succeeded and the worker moves on. Returning non-nil asks for redelivery, and the worker retries the same event up to the subscriber's `maxRetry` budget. If the budget is exhausted the event is counted as failed (a real bus would route it to a dead-letter queue) and the worker moves on rather than blocking the topic forever. This is *at-least-once*, not exactly-once: a handler can apply a side effect and then fail — or succeed but have its ack lost in a crash — and be retried, so it may run more than once for one event. The contract a handler must therefore honor is idempotency, which is why `deliver` counts handler *invocations* (`Delivered`, including retries) separately from successful events (`Acked`) and dead-lettered ones (`Failed`).

Create `bus.go`:

```go
package eventbus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

var (
	// ErrBusClosed is returned by Publish and Subscribe after Shutdown.
	ErrBusClosed = errors.New("eventbus: bus is closed")
	// ErrNoHandler is returned by Subscribe when the handler is nil.
	ErrNoHandler = errors.New("eventbus: nil handler")
)

// Event is one domain event routed by topic. ID is assigned by the bus in
// publish order, so consumers and tests can reason about delivery.
type Event struct {
	ID      int64
	Topic   string
	Payload any
}

// Handler processes one event. Returning nil acks the event; returning a non-nil
// error asks the bus to redeliver it, up to the subscriber's retry budget.
// Because a handler may apply a side effect before it fails and then be retried,
// delivery is at-least-once and a handler must be idempotent.
type Handler func(context.Context, Event) error

type subscriber struct {
	name     string
	topic    string
	queue    chan Event
	handler  Handler
	maxRetry int

	delivered atomic.Int64 // handler invocations, including retries
	acked     atomic.Int64 // events that eventually succeeded
	failed    atomic.Int64 // events that exhausted the retry budget
}

// Bus is an in-process domain-event bus. Each subscriber owns a bounded queue
// drained by its own worker goroutine, so a slow consumer applies backpressure
// to publishers of its topic without stalling other subscribers. Shutdown drains
// every buffered event and joins all workers, leaving no goroutine behind.
type Bus struct {
	mu      sync.Mutex
	subs    []*subscriber
	closed  bool
	closing chan struct{} // closed by Shutdown to release blocked publishers

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // worker goroutines
	pubWg  sync.WaitGroup // in-flight Publish calls
	nextID atomic.Int64
}

// New returns a running bus.
func New() *Bus {
	ctx, cancel := context.WithCancel(context.Background())
	return &Bus{
		closing: make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Subscribe registers a consumer of topic with a bounded queue of queueSize and a
// retry budget of maxRetry redeliveries, then starts its worker goroutine.
// queueSize is clamped to at least 1 and maxRetry to at least 0. Subscribing to a
// closed bus returns ErrBusClosed; a nil handler returns ErrNoHandler.
func (b *Bus) Subscribe(name, topic string, queueSize, maxRetry int, h Handler) error {
	if h == nil {
		return ErrNoHandler
	}
	if queueSize < 1 {
		queueSize = 1
	}
	if maxRetry < 0 {
		maxRetry = 0
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBusClosed
	}
	s := &subscriber{
		name:     name,
		topic:    topic,
		queue:    make(chan Event, queueSize),
		handler:  h,
		maxRetry: maxRetry,
	}
	b.subs = append(b.subs, s)
	b.wg.Add(1)
	b.mu.Unlock()

	go b.work(s)
	return nil
}

// work pulls events from one subscriber's queue. On ctx cancellation it drains
// every remaining buffered event and returns, so the worker never depends on the
// queue being closed and can never race a send against a close.
func (b *Bus) work(s *subscriber) {
	defer b.wg.Done()
	for {
		select {
		case ev := <-s.queue:
			b.deliver(s, ev)
		case <-b.ctx.Done():
			for {
				select {
				case ev := <-s.queue:
					b.deliver(s, ev)
				default:
					return
				}
			}
		}
	}
}

// deliver runs the handler, retrying on error up to maxRetry redeliveries. The
// first success acks; an exhausted budget dead-letters (counted as failed).
func (b *Bus) deliver(s *subscriber, ev Event) {
	for attempt := 0; ; attempt++ {
		s.delivered.Add(1)
		if err := s.handler(b.ctx, ev); err == nil {
			s.acked.Add(1)
			return
		}
		if attempt >= s.maxRetry {
			s.failed.Add(1)
			return
		}
	}
}

// Publish enqueues a new event on topic to every matching subscriber. The send to
// each bounded queue blocks while that queue is full, so a slow consumer pushes
// backpressure onto the publisher rather than dropping events. A concurrent
// Shutdown releases a blocked Publish, which then returns ErrBusClosed.
func (b *Bus) Publish(topic string, payload any) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBusClosed
	}
	b.pubWg.Add(1)
	defer b.pubWg.Done()
	ev := Event{ID: b.nextID.Add(1), Topic: topic, Payload: payload}
	var targets []*subscriber
	for _, s := range b.subs {
		if s.topic == topic {
			targets = append(targets, s)
		}
	}
	b.mu.Unlock()

	for _, s := range targets {
		select {
		case s.queue <- ev:
		case <-b.closing:
			return ErrBusClosed
		}
	}
	return nil
}

// Shutdown stops accepting new events, releases any publisher blocked on a full
// queue, waits for in-flight publishes to settle, then signals every worker to
// drain its remaining buffered events and exit. It blocks until all workers have
// returned, so it both finishes buffered work and leaves no goroutine behind. It
// is idempotent.
func (b *Bus) Shutdown() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	close(b.closing)
	b.mu.Unlock()

	b.pubWg.Wait() // no publisher touches a queue after this returns
	b.cancel()     // workers drain their now-stable buffers and exit
	b.wg.Wait()    // join every worker: proof that none leaked
}

// Stats is a snapshot of one subscriber's delivery counters.
type Stats struct {
	Name      string
	Delivered int64 // handler invocations, including retries
	Acked     int64 // events that eventually succeeded
	Failed    int64 // events that exhausted their retry budget
}

// Stats returns a snapshot of every subscriber's counters, in subscription order.
func (b *Bus) Stats() []Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Stats, len(b.subs))
	for i, s := range b.subs {
		out[i] = Stats{
			Name:      s.name,
			Delivered: s.delivered.Load(),
			Acked:     s.acked.Load(),
			Failed:    s.failed.Load(),
		}
	}
	return out
}
```

`Bus` holds a `sync.Mutex`, two `sync.WaitGroup`s, and an `atomic.Int64`, none of which may be copied, so the bus is always used through the `*Bus` that `New` returns; `go vet`'s copylocks check enforces this. The counters on `subscriber` are `atomic.Int64` because the worker writes them while `Stats` reads them concurrently. `Publish` snapshots the matching subscribers under the lock and then releases it before the blocking sends, so a slow consumer never holds the lock — only its own queue's capacity bounds the publisher.

### The runnable demo

The demo is deterministic because it reads counters only after `Shutdown` has joined the workers, so the printed numbers do not depend on scheduling. It registers a `stock` consumer that always succeeds and a `ledger` consumer rigged to fail the first delivery of every event and succeed on retry, then publishes three orders and shuts down. The counters show at-least-once at work: the ledger handler was invoked twice per event (one failure, one success) but each event was acked exactly once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"example.com/domain-event-bus"
)

func main() {
	bus := eventbus.New()

	var mu sync.Mutex
	var stockProcessed int
	_ = bus.Subscribe("stock", "order.created", 4, 2, func(_ context.Context, _ eventbus.Event) error {
		mu.Lock()
		stockProcessed++
		mu.Unlock()
		return nil
	})

	// A ledger consumer that fails the first attempt of every event, then
	// succeeds on retry, exercising at-least-once redelivery.
	var amu sync.Mutex
	attempts := map[int64]int{}
	_ = bus.Subscribe("ledger", "order.created", 4, 3, func(_ context.Context, ev eventbus.Event) error {
		amu.Lock()
		attempts[ev.ID]++
		n := attempts[ev.ID]
		amu.Unlock()
		if n == 1 {
			return errors.New("temporary ledger error")
		}
		return nil
	})

	for _, id := range []string{"ORD-1", "ORD-2", "ORD-3"} {
		_ = bus.Publish("order.created", id)
	}

	bus.Shutdown() // drains every buffered event, then joins all workers

	for _, s := range bus.Stats() {
		fmt.Printf("%-7s delivered=%d acked=%d failed=%d\n", s.Name, s.Delivered, s.Acked, s.Failed)
	}
	fmt.Printf("stock processed %d orders\n", stockProcessed)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stock   delivered=3 acked=3 failed=0
ledger  delivered=6 acked=3 failed=0
stock processed 3 orders
```

### Tests

`TestDelivery_RetriesUntilAck` and `TestDelivery_ExhaustsRetryBudget` pin the at-least-once accounting: the first handler fails twice then succeeds (three invocations, one ack), the second always fails (`maxRetry+1` invocations, one dead-letter). `TestShutdown_DrainsAllCommittedEvents` publishes far more events than a tiny queue can hold, so `Publish` must block on backpressure; after `Shutdown` every committed event must have been processed, and a timeout guard turns a leaked or wedged worker into a test failure instead of a hang. `TestPublish_AfterShutdown` pins the closed-bus contract. `TestConcurrentPublishAndSubscribe` runs four publishers and four late subscribers against the bus under `-race`, then shuts down with a timeout — the race detector catches any unsynchronized access and the timeout catches a shutdown that fails to join its workers.

Create `bus_test.go`:

```go
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDelivery_RetriesUntilAck(t *testing.T) {
	t.Parallel()

	bus := New()
	var attempts atomic.Int64
	if err := bus.Subscribe("flaky", "t", 4, 5, func(context.Context, Event) error {
		if attempts.Add(1) < 3 {
			return errors.New("transient")
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := bus.Publish("t", "x"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	bus.Shutdown()

	s := bus.Stats()[0]
	if s.Delivered != 3 || s.Acked != 1 || s.Failed != 0 {
		t.Fatalf("stats = %+v, want delivered=3 acked=1 failed=0", s)
	}
}

func TestDelivery_ExhaustsRetryBudget(t *testing.T) {
	t.Parallel()

	bus := New()
	_ = bus.Subscribe("broken", "t", 4, 2, func(context.Context, Event) error {
		return errors.New("always fails")
	})

	if err := bus.Publish("t", "x"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	bus.Shutdown()

	s := bus.Stats()[0]
	if s.Delivered != 3 || s.Acked != 0 || s.Failed != 1 {
		t.Fatalf("stats = %+v, want delivered=3 (1 try + 2 retries) acked=0 failed=1", s)
	}
}

func TestShutdown_DrainsAllCommittedEvents(t *testing.T) {
	t.Parallel()

	bus := New()
	var got atomic.Int64
	// Queue of 2 with fast handler: Publish must block on backpressure, and the
	// drain on Shutdown must still process every committed event.
	_ = bus.Subscribe("c", "t", 2, 0, func(context.Context, Event) error {
		got.Add(1)
		return nil
	})

	const n = 500
	for i := 0; i < n; i++ {
		if err := bus.Publish("t", i); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	done := make(chan struct{})
	go func() { bus.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not return: a worker leaked or wedged")
	}

	if got.Load() != n {
		t.Fatalf("delivered %d events, want %d", got.Load(), n)
	}
}

func TestPublish_AfterShutdown(t *testing.T) {
	t.Parallel()

	bus := New()
	_ = bus.Subscribe("c", "t", 1, 0, func(context.Context, Event) error { return nil })
	bus.Shutdown()
	bus.Shutdown() // idempotent

	if err := bus.Publish("t", "x"); err != ErrBusClosed {
		t.Fatalf("Publish after Shutdown = %v, want ErrBusClosed", err)
	}
	if err := bus.Subscribe("late", "t", 1, 0, func(context.Context, Event) error { return nil }); err != ErrBusClosed {
		t.Fatalf("Subscribe after Shutdown = %v, want ErrBusClosed", err)
	}
}

func TestSubscribe_RejectsNilHandler(t *testing.T) {
	t.Parallel()

	bus := New()
	defer bus.Shutdown()
	if err := bus.Subscribe("c", "t", 1, 0, nil); err != ErrNoHandler {
		t.Fatalf("Subscribe(nil) = %v, want ErrNoHandler", err)
	}
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	t.Parallel()

	bus := New()
	var delivered atomic.Int64
	handler := func(context.Context, Event) error {
		delivered.Add(1)
		return nil
	}
	_ = bus.Subscribe("base", "t", 8, 0, handler)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 250 {
				_ = bus.Publish("t", i)
			}
		}()
	}
	for s := range 4 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_ = bus.Subscribe(fmt.Sprintf("late-%d", id), "t", 4, 1, handler)
		}(s)
	}
	wg.Wait()

	done := make(chan struct{})
	go func() { bus.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown hung after concurrent publish/subscribe")
	}
	if delivered.Load() == 0 {
		t.Fatal("expected at least some deliveries")
	}
}
```

## Review

The bus is correct when shutdown both finishes committed work and joins every worker, and when delivery is at-least-once. The drain guarantee is structural and rests on ordering: `Shutdown` closes `closing` and `pubWg.Wait()`s before it `cancel()`s, so every publisher has settled and the per-subscriber buffers are frozen before the workers are told to drain them — every event a successful `Publish` enqueued is therefore processed. The no-leak guarantee is the `wg.Wait()` at the end of `Shutdown`: workers are joined, never abandoned, so `TestShutdown_DrainsAllCommittedEvents` and `TestConcurrentPublishAndSubscribe` can use a timeout to convert a leaked worker into a failure rather than a hang. At-least-once shows up in the counters: `Delivered` tallies handler invocations including retries, while `Acked` tallies events, so a handler that fails once and then succeeds reads as two deliveries and one ack.

Common mistakes for this feature. The first is closing the subscriber queues from `Shutdown` to stop the workers: a publisher blocked on a full queue then panics with "send on closed channel," which is exactly why the workers terminate on context cancellation and the queues are never closed. The second is cancelling the workers before publishers have settled, which lets a late successful send land in a buffer the worker has already drained past, silently losing a committed event; the `pubWg.Wait()` before `cancel()` is what freezes the buffers first. The third is asserting "no goroutine leak" with `runtime.NumGoroutine`, which is racy and flaky; joining the workers with a `WaitGroup` inside `Shutdown` makes the absence of a leak deterministic, and the tests assert it with a timeout. Confirm `go test -race ./...` passes; the race detector is what proves the concurrent publish/subscribe path has no unsynchronized access.

## Resources

- [`context` package](https://pkg.go.dev/context) — cancellation propagation, the signal this bus uses to tell workers to drain and exit.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — joining the worker and publisher goroutines, the basis of the deterministic no-leak guarantee.
- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — bounded queues, backpressure, and shutting a pipeline down cleanly.
- [Designing distributed systems: at-least-once delivery](https://www.cloudflare.com/learning/ddos/glossary/message-queue/) — why redelivery plus idempotency, not exactly-once, is the practical delivery contract.

---

Back to [03-non-blocking-fanout.md](03-non-blocking-fanout.md) | Next: [05-order-events-fanout.md](05-order-events-fanout.md)
