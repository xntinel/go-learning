# Exercise 3: Fan-Out with a Slow-Consumer Policy

In a fan-out, one message goes to many subscribers, and the moment they read at different speeds you have a problem: a single slow consumer must not be allowed to stall the publisher or the fast consumers behind it. The standard answer is a bounded per-subscriber buffer plus an explicit overflow policy that decides what to sacrifice when that buffer fills — drop the oldest queued message, drop the newest arrival, or disconnect the laggard entirely. This exercise builds a fan-out hub where each subscriber carries its own bounded queue and its own policy, so one slow reader degrades only its own stream. It is a self-contained module: it imports nothing from the other exercises and ships its own demo and tests.

## What you'll build

```text
fanout.go            Hub, Subscriber, OverflowPolicy; bounded per-sub queues
cmd/
  demo/
    main.go          three subscribers, one fast stream, three different policies
fanout_test.go       drop-oldest, drop-newest, disconnect, and a no-loss race test
```

- Files: `fanout.go`, `cmd/demo/main.go`, `fanout_test.go`.
- Implement: `Hub` (`Subscribe`, `Publish`, `Close`) and `Subscriber` (`Receive`, `Drain`, `Dropped`, `Closed`, `Close`) with the three overflow policies.
- Test: each policy keeps the right messages when overrun; a roomy buffer under concurrent publishers loses nothing.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p slow-consumer-policy/cmd/demo && cd slow-consumer-policy
go mod init example.com/fanout
```

### Why per-subscriber buffers, and what the three policies trade

If a hub delivered by blocking until every subscriber accepted a message, the slowest subscriber would set the pace for everyone — a single stuck reader would freeze the whole broadcast. The fix is to give each subscriber a private bounded queue and let the publisher hand off into that queue without ever waiting. As long as a subscriber keeps up, its queue stays shallow and it sees every message. When it falls behind, its queue fills, and at that moment the policy decides what gives. The publisher is never blocked and the other subscribers never notice.

The three policies encode three different opinions about which data matters when you cannot keep all of it:

- Drop-oldest favors freshness. When the queue is full, the oldest queued message is evicted to make room for the new arrival. A subscriber that reads sporadically always sees the most recent state and loses the stale middle. This is the right choice for telemetry, dashboards, and last-value caches, where an old reading is worthless once a newer one exists.
- Drop-newest favors order and the head of the stream. When full, the new arrival is discarded and the queue keeps the messages it already had. A subscriber sees an unbroken prefix of the stream and misses the tail. This suits a consumer that must process messages strictly in sequence and would rather have a contiguous-but-incomplete view than a gap-filled-but-recent one.
- Disconnect favors the system over the laggard. When full, the subscriber is closed: it stops receiving, its `Receive` returns "closed" once drained, and the hub forgets it. This is the policy of a server protecting itself — a client too slow to keep up is cut loose rather than allowed to accumulate unbounded memory or degrade everyone. It is what production brokers do to a consumer whose send buffer has been full past a deadline.

The crucial property all three share is locality: the policy applies to one subscriber's queue under that subscriber's own lock, so a slow consumer's overflow never touches the publisher's latency or any other subscriber's data.

### The delivery path and the condition variable

Each `Subscriber` owns a slice used as a FIFO queue, a capacity, a policy, a `dropped` counter, a `closed` flag, and a `sync.Cond` bound to its own mutex. `Hub.Publish` snapshots the subscriber set under a read lock and calls `deliver` on each; `deliver` takes that subscriber's lock and runs the policy. When there is room, it appends and signals the condition variable so a blocked `Receive` wakes. When there is not, the policy branch runs: drop-oldest shifts the queue down and overwrites the tail, drop-newest just bumps the counter and discards, disconnect sets `closed` and broadcasts so any blocked `Receive` returns.

`Receive` is the blocking reader, and it is the place the condition variable earns its keep: it waits in a `for` loop while the queue is empty and the subscriber is open, so it consumes no CPU and cannot miss a wakeup. When it returns it either hands back the head of the queue and `true`, or — if the subscriber was closed and the queue is drained — the zero message and `false`. `Drain` is the non-blocking companion that snapshots and clears the queue in one shot, which is what the policy tests use to inspect exactly what survived an overrun.

Create `fanout.go`:

```go
package fanout

import "sync"

// OverflowPolicy decides what a Subscriber does when its bounded queue is full.
type OverflowPolicy int

const (
	// DropOldest evicts the oldest queued message to make room for the newest.
	DropOldest OverflowPolicy = iota
	// DropNewest discards the incoming message and keeps the existing queue.
	DropNewest
	// Disconnect closes the subscriber; it receives nothing further.
	Disconnect
)

// Message is a fan-out record. Seq identifies it within a stream.
type Message struct {
	Seq     int
	Payload []byte
}

// Subscriber is one consumer with a private bounded queue and overflow policy.
type Subscriber struct {
	mu       sync.Mutex
	cond     *sync.Cond
	queue    []Message
	capacity int
	policy   OverflowPolicy
	dropped  int
	closed   bool
}

func newSubscriber(policy OverflowPolicy, capacity int) *Subscriber {
	if capacity < 1 {
		capacity = 1
	}
	s := &Subscriber{capacity: capacity, policy: policy}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// deliver enqueues m, applying the overflow policy when the queue is full.
func (s *Subscriber) deliver(m Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	if len(s.queue) < s.capacity {
		s.queue = append(s.queue, m)
		s.cond.Signal()
		return
	}
	switch s.policy {
	case DropOldest:
		copy(s.queue, s.queue[1:])  // shift everything down by one
		s.queue[len(s.queue)-1] = m // newest takes the tail slot
		s.dropped++
	case DropNewest:
		s.dropped++ // discard the arrival; queue is unchanged
	case Disconnect:
		s.dropped++
		s.closed = true
		s.cond.Broadcast()
	}
}

// Receive blocks until a message is available or the subscriber is closed. It
// returns (msg, true) for a delivered message, or (zero, false) once the
// subscriber is closed and its queue is fully drained.
func (s *Subscriber) Receive() (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for len(s.queue) == 0 && !s.closed {
		s.cond.Wait()
	}
	if len(s.queue) == 0 { // closed and drained
		return Message{}, false
	}
	m := s.queue[0]
	s.queue = s.queue[1:]
	return m, true
}

// Drain removes and returns every queued message without blocking.
func (s *Subscriber) Drain() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.queue))
	copy(out, s.queue)
	s.queue = s.queue[:0]
	return out
}

// Dropped returns how many messages this subscriber has lost to its policy.
func (s *Subscriber) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Closed reports whether the subscriber has been closed.
func (s *Subscriber) Closed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Close marks the subscriber closed and wakes any blocked Receive.
func (s *Subscriber) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.cond.Broadcast()
}

// Hub broadcasts each published message to every registered subscriber.
type Hub struct {
	mu   sync.RWMutex
	subs map[*Subscriber]struct{}
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[*Subscriber]struct{})}
}

// Subscribe registers a new subscriber with the given policy and queue capacity.
func (h *Hub) Subscribe(policy OverflowPolicy, capacity int) *Subscriber {
	s := newSubscriber(policy, capacity)
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Publish delivers m to every subscriber. It never blocks on a slow subscriber:
// each applies its own overflow policy independently.
func (h *Hub) Publish(m Message) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		s.deliver(m)
	}
}

// Close closes every subscriber, unblocking all pending Receive calls.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		s.Close()
	}
}
```

### The runnable demo

The demo subscribes three readers with capacity 3 and three different policies, then publishes a six-message burst that none of them reads during the burst, so every queue overruns. Trace it: drop-oldest ends holding the last three (`3 4 5`) and counts three drops; drop-newest holds the first three (`0 1 2`) and also counts three; disconnect fills with `0 1 2`, then the fourth message trips it closed (one drop counted) and the rest are ignored.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/fanout"
)

func main() {
	hub := fanout.NewHub()
	oldest := hub.Subscribe(fanout.DropOldest, 3)
	newest := hub.Subscribe(fanout.DropNewest, 3)
	disc := hub.Subscribe(fanout.Disconnect, 3)

	for i := 0; i < 6; i++ {
		hub.Publish(fanout.Message{Seq: i})
	}

	report := func(name string, s *fanout.Subscriber) {
		var seqs []int
		for _, m := range s.Drain() {
			seqs = append(seqs, m.Seq)
		}
		fmt.Printf("%-11s kept %v (dropped %d, closed=%v)\n", name, seqs, s.Dropped(), s.Closed())
	}
	report("drop-oldest", oldest)
	report("drop-newest", newest)
	report("disconnect", disc)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
drop-oldest kept [3 4 5] (dropped 3, closed=false)
drop-newest kept [0 1 2] (dropped 3, closed=false)
disconnect  kept [0 1 2] (dropped 1, closed=true)
```

### Tests

`TestDropOldestKeepsNewest` and `TestDropNewestKeepsOldest` overrun a capacity-3 queue with five messages and assert exactly which three survive and that two drops are counted. `TestDisconnectClosesLaggard` confirms the disconnect policy closes the subscriber on overflow while preserving the messages it had already queued. `TestConcurrentFanOutNoLoss` gives every subscriber a roomy buffer, runs several publishers concurrently against a blocking `Receive` consumer, and asserts no message is lost — the property that the per-subscriber lock and condition variable exist to guarantee and that only `-race` can certify.

Create `fanout_test.go`:

```go
package fanout

import (
	"sync"
	"testing"
)

func seqs(ms []Message) []int {
	out := make([]int, len(ms))
	for i, m := range ms {
		out[i] = m.Seq
	}
	return out
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDropOldestKeepsNewest(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	s := hub.Subscribe(DropOldest, 3)
	for i := 0; i < 5; i++ {
		hub.Publish(Message{Seq: i})
	}
	if got := seqs(s.Drain()); !equal(got, []int{2, 3, 4}) {
		t.Fatalf("kept %v, want [2 3 4]", got)
	}
	if s.Dropped() != 2 {
		t.Fatalf("dropped %d, want 2", s.Dropped())
	}
}

func TestDropNewestKeepsOldest(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	s := hub.Subscribe(DropNewest, 3)
	for i := 0; i < 5; i++ {
		hub.Publish(Message{Seq: i})
	}
	if got := seqs(s.Drain()); !equal(got, []int{0, 1, 2}) {
		t.Fatalf("kept %v, want [0 1 2]", got)
	}
	if s.Dropped() != 2 {
		t.Fatalf("dropped %d, want 2", s.Dropped())
	}
}

func TestDisconnectClosesLaggard(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	s := hub.Subscribe(Disconnect, 3)
	for i := 0; i < 5; i++ {
		hub.Publish(Message{Seq: i})
	}
	if !s.Closed() {
		t.Fatal("subscriber should be closed after overflow")
	}
	if got := seqs(s.Drain()); !equal(got, []int{0, 1, 2}) {
		t.Fatalf("kept %v, want [0 1 2]", got)
	}
}

func TestConcurrentFanOutNoLoss(t *testing.T) {
	t.Parallel()

	hub := NewHub()
	const publishers = 8
	const each = 25
	const total = publishers * each

	sub := hub.Subscribe(DropOldest, total*2) // roomy: no message can be dropped

	got := make(chan int, 1)
	go func() {
		n := 0
		for {
			if _, ok := sub.Receive(); !ok {
				got <- n
				return
			}
			n++
		}
	}()

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				hub.Publish(Message{Seq: p*each + i})
			}
		}(p)
	}
	wg.Wait()
	sub.Close() // drains remaining, then Receive returns false

	if n := <-got; n != total {
		t.Fatalf("received %d, want %d", n, total)
	}
	if sub.Dropped() != 0 {
		t.Fatalf("dropped %d, want 0", sub.Dropped())
	}
}
```

## Review

The hub is correct when a slow subscriber's overflow is invisible to everyone else. Confirm `Publish` never blocks: it holds only a read lock on the subscriber set and hands each message into a queue whose policy guarantees the call returns without waiting. Confirm each policy keeps the right data — drop-oldest the newest tail, drop-newest the oldest prefix — by checking exactly which sequence numbers survive an overrun, not merely the queue length. Confirm `Receive` waits in a `for` loop so it cannot miss a wakeup and returns `false` only once the subscriber is both closed and drained, which is what lets the no-loss test count every message and then terminate cleanly.

The mistakes to avoid: signaling the condition variable on the drop-oldest and drop-newest branches is wrong, because the queue length does not increase, so there is nothing new for a waiter to consume — a `Receive` only ever blocks on an empty queue, which by definition is not full. Closing on the disconnect branch without broadcasting would leave a blocked `Receive` hung forever. Reusing the publisher's goroutine to run a blocking delivery, instead of a bounded queue, reintroduces the very head-of-line blocking the bounded queue exists to prevent. The three policy tests plus the concurrent no-loss test under `-race` together certify the locality property: one slow reader degrades only its own stream.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — the `Wait`/`Signal`/`Broadcast` primitive behind the blocking `Receive`; `Wait` must sit in a loop that re-checks the predicate.
- [NATS Go client: Slow Consumers](https://pkg.go.dev/github.com/nats-io/nats.go#hdr-Slow_Consumers) — how a production broker detects a consumer that cannot keep up, raises a slow-consumer error, and protects the system.
- [Kafka producer `buffer.memory` and `block.on.buffer.full`](https://kafka.apache.org/documentation/#producerconfigs) — the block-versus-drop decision a producer faces when its send buffer fills, the same trade-off these policies encode.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-wildcard-subscriptions.md](02-wildcard-subscriptions.md)
