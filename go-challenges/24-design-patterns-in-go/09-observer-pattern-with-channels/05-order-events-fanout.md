# Exercise 5: Order-Events Fan-Out to Independent Consumers

This is the pattern made into a real feature. A checkout commits an order event and three back-office consumers must each react on their own schedule: an email sender confirms paid orders, an audit log records everything, and a metrics counter tallies events by type. They run as independent goroutines with independent bounded queues, so a slow email provider cannot hold up the audit log or the metrics, yet every committed event still reaches every consumer. The headline `-race` test publishes a thousand events from four goroutines against tiny queues — forcing real backpressure — and asserts all three consumers received every committed event and that shutdown drained and joined cleanly.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
orderevents.go       Publisher, OrderEvent; EmailSender, AuditLog, MetricsCounter
cmd/
  demo/
    main.go          publish a small order lifecycle, drain, print each consumer
orderevents_test.go  completeness under backpressure + -race concurrent publish
```

- Files: `orderevents.go`, `cmd/demo/main.go`, `orderevents_test.go`.
- Implement: `Publisher` with `Publish(typ, order string, amount int) (int64, error)`, `Shutdown()`, `Received(name string) int64`; consumers `EmailSender`, `AuditLog`, `MetricsCounter`.
- Test: `orderevents_test.go` proves every committed event reaches all three consumers under backpressure, the per-consumer domain outputs are complete, and concurrent publish plus shutdown is race-free with no leak.
- Verify: `go test -race ./...`

### Three consumers, three queues, one fan-out

The `Publisher` owns a fixed set of consumers, each a small struct: a name, a bounded queue (buffered channel), and an `apply` function that does the consumer's domain work. When the publisher is built, it starts one worker goroutine per consumer. `Publish` assigns the event a monotonic sequence number and does a *blocking* send into each consumer's queue. Blocking is deliberate: it is the backpressure. If the email sender's queue fills because the upstream SMTP service is slow, the publisher waits on that one send — but the audit and metrics queues, being separate channels drained by separate workers, keep flowing at their own pace. Each consumer gets its own buffer size at construction, so you can tune backpressure per consumer: a roomy audit queue, a tight email queue.

"All consumers see all committed events" is the property this buys, and it is worth stating precisely. A `Publish` call returns `(seq, nil)` only after the event has been enqueued into *every* consumer's queue. So for any event whose `Publish` returned `nil` — a committed event — that event is guaranteed to sit in all three queues, and the graceful drain on shutdown guarantees all three workers process it. The `consumer` wrapper counts every event it pulls from its queue in a `received` counter (separate from whatever the domain handler does with it), so a test can assert `Received("email") == Received("audit") == Received("metrics") == committed` and prove the fan-out lost nothing. The domain handlers then differ in what they *do*: the email sender acts only on `order.paid`, the audit log records every event, the metrics counter tallies by type — independent reactions to the same complete stream.

### The same settle-then-drain shutdown

Shutdown uses the identical handshake as a domain-event bus: close a `closing` channel so any publisher blocked on a full consumer queue escapes with `ErrClosed`; `pubWg.Wait()` so every in-flight publish settles and the queues stop growing; `cancel()` the context so each worker switches to a final drain-and-exit loop that empties its now-stable queue; then `wg.Wait()` to join every worker. The queues are never closed, so a blocking send can never race a close, and because publishers settle before the workers drain, every committed event is processed before `Shutdown` returns. The terminal `wg.Wait()` joins the three workers, which is the deterministic proof that shutdown leaks no goroutine.

Create `orderevents.go`:

```go
package orderevents

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Order event types committed by checkout.
const (
	EventCreated = "order.created"
	EventPaid    = "order.paid"
	EventShipped = "order.shipped"
)

// ErrClosed is returned by Publish after Shutdown has begun.
var ErrClosed = errors.New("orderevents: publisher is closed")

// OrderEvent is one committed change to an order. Seq is assigned by the
// publisher in commit order. Amount is in cents and is only meaningful for paid
// events.
type OrderEvent struct {
	Seq    int64
	Type   string
	Order  string
	Amount int
}

// consumer wraps one back-office subscriber: a bounded queue, the domain apply
// function, and a count of every event pulled off the queue.
type consumer struct {
	name     string
	queue    chan OrderEvent
	apply    func(OrderEvent)
	received atomic.Int64
}

// EmailSender sends a confirmation email for every paid order and records which
// orders it emailed, so completeness can be asserted.
type EmailSender struct {
	mu   sync.Mutex
	sent []string
}

func (e *EmailSender) handle(ev OrderEvent) {
	if ev.Type != EventPaid {
		return
	}
	e.mu.Lock()
	e.sent = append(e.sent, ev.Order)
	e.mu.Unlock()
}

// Sent returns a copy of the orders emailed so far.
func (e *EmailSender) Sent() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.sent))
	copy(out, e.sent)
	return out
}

// AuditLog records every event in commit order.
type AuditLog struct {
	mu      sync.Mutex
	entries []string
}

func (a *AuditLog) handle(ev OrderEvent) {
	a.mu.Lock()
	a.entries = append(a.entries, fmt.Sprintf("%d %s %s", ev.Seq, ev.Type, ev.Order))
	a.mu.Unlock()
}

// Entries returns a copy of the audit log.
func (a *AuditLog) Entries() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.entries))
	copy(out, a.entries)
	return out
}

// MetricsCounter tallies events by type.
type MetricsCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (m *MetricsCounter) handle(ev OrderEvent) {
	m.mu.Lock()
	if m.counts == nil {
		m.counts = make(map[string]int)
	}
	m.counts[ev.Type]++
	m.mu.Unlock()
}

// Count returns how many events of the given type were tallied.
func (m *MetricsCounter) Count(typ string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[typ]
}

// Publisher fans each committed order event out to three independent consumers,
// each draining its own bounded queue with its own worker goroutine. Shutdown
// drains every queue and joins every worker.
type Publisher struct {
	mu        sync.Mutex
	consumers []*consumer
	closed    bool
	closing   chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // worker goroutines
	pubWg  sync.WaitGroup // in-flight Publish calls
	seq    atomic.Int64
}

// NewPublisher wires the three consumers, each with its own queue size, and
// starts a worker per consumer. A queue size below 1 is clamped to 1.
func NewPublisher(email *EmailSender, audit *AuditLog, metrics *MetricsCounter, bufEmail, bufAudit, bufMetrics int) *Publisher {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Publisher{
		closing: make(chan struct{}),
		ctx:     ctx,
		cancel:  cancel,
	}
	p.add("email", bufEmail, email.handle)
	p.add("audit", bufAudit, audit.handle)
	p.add("metrics", bufMetrics, metrics.handle)
	return p
}

func (p *Publisher) add(name string, buf int, apply func(OrderEvent)) {
	if buf < 1 {
		buf = 1
	}
	c := &consumer{name: name, queue: make(chan OrderEvent, buf), apply: apply}
	p.consumers = append(p.consumers, c)
	p.wg.Add(1)
	go p.work(c)
}

// work drains one consumer's queue, counting every event received. On ctx
// cancellation it drains whatever is still buffered and returns; the queue is
// never closed, so a send can never race a close.
func (p *Publisher) work(c *consumer) {
	defer p.wg.Done()
	for {
		select {
		case ev := <-c.queue:
			c.received.Add(1)
			c.apply(ev)
		case <-p.ctx.Done():
			for {
				select {
				case ev := <-c.queue:
					c.received.Add(1)
					c.apply(ev)
				default:
					return
				}
			}
		}
	}
}

// Publish fans one event out to all three consumers and returns its sequence
// number. Each send blocks while that consumer's queue is full, so a slow
// consumer applies backpressure to the publisher without affecting how fast the
// others drain. It returns 0 and ErrClosed if the publisher is shutting down.
func (p *Publisher) Publish(typ, order string, amount int) (int64, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return 0, ErrClosed
	}
	p.pubWg.Add(1)
	defer p.pubWg.Done()
	ev := OrderEvent{Seq: p.seq.Add(1), Type: typ, Order: order, Amount: amount}
	consumers := p.consumers
	p.mu.Unlock()

	for _, c := range consumers {
		select {
		case c.queue <- ev:
		case <-p.closing:
			return 0, ErrClosed
		}
	}
	return ev.Seq, nil
}

// Received reports how many events the named consumer has pulled off its queue.
// It returns -1 for an unknown name.
func (p *Publisher) Received(name string) int64 {
	for _, c := range p.consumers {
		if c.name == name {
			return c.received.Load()
		}
	}
	return -1
}

// Shutdown stops accepting events, releases any blocked publisher, waits for
// in-flight publishes to settle, drains every consumer queue, and joins every
// worker before returning. It is idempotent.
func (p *Publisher) Shutdown() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.closing)
	p.mu.Unlock()

	p.pubWg.Wait() // queues are stable once every publisher has settled
	p.cancel()     // workers drain their queues and exit
	p.wg.Wait()    // join all three workers: no goroutine leak
}
```

The `consumers` slice is built entirely inside `NewPublisher` and never mutated afterward, so `Publish` and `Received` read it safely without the lock. Each consumer's `received` counter is an `atomic.Int64` because the worker increments it while `Received` reads it. `Publisher`, `EmailSender`, `AuditLog`, and `MetricsCounter` all embed a `sync.Mutex` or other no-copy field, so each is used through a pointer; `go vet`'s copylocks check enforces it.

### The runnable demo

The demo walks a small order lifecycle through the fan-out and reads results only after `Shutdown`, so the numbers are deterministic. Two orders are created, paid, and one shipped — five events. Every consumer receives all five; the email sender acts on the two paid events, the audit log records all five, and the metrics counter splits them by type.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/order-events-fanout"
)

func main() {
	email := &orderevents.EmailSender{}
	audit := &orderevents.AuditLog{}
	metrics := &orderevents.MetricsCounter{}

	// A tight email queue and roomy audit/metrics queues: per-consumer backpressure.
	pub := orderevents.NewPublisher(email, audit, metrics, 4, 16, 16)

	events := []struct {
		typ, order string
		amount     int
	}{
		{orderevents.EventCreated, "ORD-1", 0},
		{orderevents.EventPaid, "ORD-1", 14997},
		{orderevents.EventCreated, "ORD-2", 0},
		{orderevents.EventPaid, "ORD-2", 2999},
		{orderevents.EventShipped, "ORD-1", 0},
	}
	for _, e := range events {
		if _, err := pub.Publish(e.typ, e.order, e.amount); err != nil {
			panic(err)
		}
	}

	pub.Shutdown() // drains every queue, then joins all three workers

	fmt.Printf("audit entries: %d\n", len(audit.Entries()))
	fmt.Printf("emails sent:   %d\n", len(email.Sent()))
	fmt.Printf("metrics created=%d paid=%d shipped=%d\n",
		metrics.Count(orderevents.EventCreated),
		metrics.Count(orderevents.EventPaid),
		metrics.Count(orderevents.EventShipped))
	fmt.Printf("received email=%d audit=%d metrics=%d\n",
		pub.Received("email"), pub.Received("audit"), pub.Received("metrics"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
audit entries: 5
emails sent:   2
metrics created=2 paid=2 shipped=1
received email=5 audit=5 metrics=5
```

### Tests

`TestAllConsumersSeeCommittedEvents` is the completeness proof: it publishes a fixed lifecycle through tiny queues so the sends actually block, then after `Shutdown` asserts all three `Received` counts equal the number of committed events and that each consumer's domain output is complete (audit has every entry, email has exactly the paid ones, metrics sum to the total). `TestPublish_AfterShutdown` pins the closed contract. `TestConcurrentPublish_AllDelivered` is the headline `-race` test: four goroutines publish a thousand events through two-slot queues, counting the committed ones, then `Shutdown` runs under a timeout and every consumer must have received exactly the committed count — the race detector proves no unsynchronized access and the equality proves the fan-out dropped nothing under heavy backpressure.

Create `orderevents_test.go`:

```go
package orderevents

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAllConsumersSeeCommittedEvents(t *testing.T) {
	t.Parallel()

	email := &EmailSender{}
	audit := &AuditLog{}
	metrics := &MetricsCounter{}
	pub := NewPublisher(email, audit, metrics, 1, 1, 1) // tiny queues force backpressure

	events := []struct {
		typ, order string
	}{
		{EventCreated, "ORD-1"},
		{EventPaid, "ORD-1"},
		{EventCreated, "ORD-2"},
		{EventPaid, "ORD-2"},
		{EventShipped, "ORD-1"},
	}
	committed := 0
	for _, e := range events {
		if _, err := pub.Publish(e.typ, e.order, 0); err != nil {
			t.Fatalf("Publish %v: %v", e, err)
		}
		committed++
	}
	pub.Shutdown()

	for _, name := range []string{"email", "audit", "metrics"} {
		if got := pub.Received(name); got != int64(committed) {
			t.Errorf("%s received %d, want %d", name, got, committed)
		}
	}
	if got := len(audit.Entries()); got != committed {
		t.Errorf("audit entries = %d, want %d", got, committed)
	}
	if got := len(email.Sent()); got != 2 {
		t.Errorf("emails sent = %d, want 2 (the paid events)", got)
	}
	if got := metrics.Count(EventCreated) + metrics.Count(EventPaid) + metrics.Count(EventShipped); got != committed {
		t.Errorf("metrics total = %d, want %d", got, committed)
	}
}

func TestPublish_AfterShutdown(t *testing.T) {
	t.Parallel()

	pub := NewPublisher(&EmailSender{}, &AuditLog{}, &MetricsCounter{}, 1, 1, 1)
	pub.Shutdown()
	pub.Shutdown() // idempotent

	if _, err := pub.Publish(EventPaid, "ORD-9", 100); err != ErrClosed {
		t.Fatalf("Publish after Shutdown = %v, want ErrClosed", err)
	}
	if got := pub.Received("nope"); got != -1 {
		t.Fatalf("Received(unknown) = %d, want -1", got)
	}
}

func TestConcurrentPublish_AllDelivered(t *testing.T) {
	t.Parallel()

	email := &EmailSender{}
	audit := &AuditLog{}
	metrics := &MetricsCounter{}
	pub := NewPublisher(email, audit, metrics, 2, 2, 2) // tight queues -> real backpressure

	var committed atomic.Int64
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range 250 {
				if _, err := pub.Publish(EventPaid, fmt.Sprintf("w%d-%d", w, i), 100); err == nil {
					committed.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()

	done := make(chan struct{})
	go func() { pub.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown hung: a consumer worker leaked")
	}

	n := committed.Load()
	for _, name := range []string{"email", "audit", "metrics"} {
		if got := pub.Received(name); got != n {
			t.Errorf("%s received %d, want %d (all committed)", name, got, n)
		}
	}
	if got := int64(len(audit.Entries())); got != n {
		t.Errorf("audit entries = %d, want %d", got, n)
	}
	if got := int64(len(email.Sent())); got != n {
		t.Errorf("emails sent = %d, want %d (all are paid events)", got, n)
	}
}
```

## Review

The feature is correct when every committed event reaches every consumer and shutdown is clean. Completeness is the testable claim and follows from two facts: `Publish` returns `nil` only after enqueueing into all three queues, and the settle-then-drain `Shutdown` (close `closing`, `pubWg.Wait()`, `cancel()`, `wg.Wait()`) processes every queued event before returning — so `Received("email") == Received("audit") == Received("metrics") == committed`, exactly what `TestConcurrentPublish_AllDelivered` asserts under `-race` and real backpressure from two-slot queues. Independence is structural: three separate channels drained by three workers mean a slow email queue backs up only its own publishers' email send, never the audit or metrics flow. The clean shutdown is the terminal `wg.Wait()` joining all three workers, which the test guards with a timeout so a leak becomes a failure, not a hang.

Common mistakes for this feature. The first is sharing one queue across consumers, which couples them: a slow email handler then stalls audit and metrics too, defeating the independence the separate channels provide. The second is treating a non-blocking, drop-on-full send as "delivery" here — that is the right policy for telemetry but wrong for order events, where the completeness assertion would fail the moment a queue filled; blocking sends turn a full queue into backpressure, not loss. The third is closing the consumer queues to stop the workers, which panics any publisher blocked on a send; the workers stop on context cancellation instead, after publishers have settled, so a send never races a close. Confirm `go test -race ./...` passes; the concurrent test under the race detector is the real proof that the fan-out is both complete and free of data races.

## Resources

- [Go Concurrency Patterns: Pipelines and cancellation](https://go.dev/blog/pipelines) — fanning a stream out to independent stages and shutting them down without loss.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, the per-consumer received counter read concurrently with the worker that increments it.
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency) — goroutines, channels, and buffering, the primitives this fan-out is built from.
- [Enterprise Integration Patterns: Publish-Subscribe Channel](https://www.enterpriseintegrationpatterns.com/patterns/messaging/PublishSubscribeChannel.html) — the publish/subscribe intent this in-process fan-out realizes.

---

Back to [04-domain-event-bus-at-least-once.md](04-domain-event-bus-at-least-once.md) | Next: [../../25-iterators-and-modern-go/01-range-over-integers/00-concepts.md](../../25-iterators-and-modern-go/01-range-over-integers/00-concepts.md)
