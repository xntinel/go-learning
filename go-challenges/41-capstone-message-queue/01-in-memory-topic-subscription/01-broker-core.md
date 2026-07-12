# Exercise 1: Broker, Topics, and Subscriptions

This exercise builds the whole core of an in-memory message broker in one self-contained package: an append-only offset log per topic, a blocking `Poll` built on `sync.Cond`, at-least-once delivery with a visibility timeout, broadcast and competing-consumer dispatch, topic-level backpressure, and a `Broker` that ties topics and subscriptions together with a clean shutdown path. Every later exercise stands on the model you assemble here, but this module imports nothing from them — it begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests.

## What you'll build

```text
mq.go                Broker, Topic, Subscription, Message; Publish/Poll/Ack/Nack
cmd/
  demo/
    main.go          create a topic, publish five events, poll and acknowledge them
mq_test.go           offset uniqueness under load, broadcast fan-out, ack/nack,
                     backpressure, deletion unblocking pollers, competing consumers
```

- Files: `mq.go`, `cmd/demo/main.go`, `mq_test.go`.
- Implement: `Broker` (`CreateTopic`, `DeleteTopic`, `Subscribe`, `Close`), `Topic` (`Publish`, `Stats`), and `Subscription` (`Poll`, `Ack`, `Nack`).
- Test: concurrent publishers get unique monotonic offsets; broadcast delivers every message to every subscriber; `Ack` stops redelivery while `Nack` forces it; a full topic rejects; deleting a topic unblocks a blocked `Poll`; a competing-consumer group loses and duplicates nothing.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/01-in-memory-topic-subscription/01-broker-core/cmd/demo && cd go-solutions/41-capstone-message-queue/01-in-memory-topic-subscription/01-broker-core
```

### The log, the offset, and why the lock owns both

A `Topic` is an ordered, append-only slice of `*Message` guarded by a single `sync.Mutex`. There is no second lock anywhere in this package: the subscription's cursor and its in-flight delivery records live under the *topic's* mutex too. Collapsing all per-topic and per-subscription state behind one lock is the single most important design decision in the file, because it makes lock ordering trivially correct — there is only one lock, so there is no order to get wrong and no AB-BA cycle to deadlock on.

The offset is assigned in the same critical section as the append: `msg.Offset = int64(len(t.messages))` runs immediately before `t.messages = append(...)`, both under `t.mu`. Two publishers racing into `Publish` are serialized by the mutex, so each observes a distinct length and claims a distinct offset. There is no atomic counter to keep in sync with the slice and no window in which the offset and the slice position can disagree.

A topic carries two condition variables, both bound to the same mutex. `cond` is signaled by publishers after every append; pollers wait on it for new data. `space` is signaled by acknowledgments after capacity is freed; publishers wait on it when the topic is full under the blocking policy. One mutex, two conditions, two directions of flow control — and because both conditions share the mutex, the timer goroutines and acknowledgment paths can wake waiters without any extra synchronization.

Create `mq.go`:

```go
package mq

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrTopicNotFound = errors.New("mq: topic not found")
	ErrTopicExists   = errors.New("mq: topic already exists")
	ErrBrokerClosed  = errors.New("mq: broker is closed")
	ErrTopicFull     = errors.New("mq: topic is full")
	ErrInvalidOffset = errors.New("mq: invalid offset")
	ErrAlreadyAcked  = errors.New("mq: message already acknowledged")
)

// DeliveryMode controls how messages are dispatched to subscribers sharing a name.
type DeliveryMode int

const (
	// Broadcast delivers every message to each named subscription independently.
	Broadcast DeliveryMode = iota
	// CompetingConsumers delivers each message to exactly one subscription in a
	// group sharing the same name (consumer-group semantics).
	CompetingConsumers
)

// MessageState is the delivery lifecycle of a message within one subscription.
type MessageState int

const (
	StatePending MessageState = iota
	StateDelivered
	StateAcknowledged
)

// BackpressurePolicy controls what Publish does when a topic is at capacity.
type BackpressurePolicy int

const (
	BackpressureBlock  BackpressurePolicy = iota // block until space is freed
	BackpressureReject                           // return ErrTopicFull immediately
)

// Message is a record stored in a Topic. Once published its Offset and Topic are
// fixed; treat the rest as read-only after publishing.
type Message struct {
	ID        string
	Topic     string
	Key       []byte
	Value     []byte
	Headers   map[string]string
	Timestamp time.Time
	Offset    int64
}

// Size returns the approximate in-memory footprint of the message in bytes.
func (m *Message) Size() int {
	n := len(m.ID) + len(m.Topic) + len(m.Key) + len(m.Value)
	for k, v := range m.Headers {
		n += len(k) + len(v)
	}
	return n
}

// TopicOptions configures a Topic at creation time.
type TopicOptions struct {
	MaxMessages int                // 0 = unlimited
	MaxBytes    int                // 0 = unlimited
	FullPolicy  BackpressurePolicy // default BackpressureBlock
}

// Topic is an ordered, append-only log of messages.
type Topic struct {
	name       string
	opts       TopicOptions
	mu         sync.Mutex
	cond       *sync.Cond // signaled after every Publish; pollers wait on this
	space      *sync.Cond // signaled after Ack frees capacity; publishers wait on this
	messages   []*Message
	totalBytes int
	closed     bool
}

func newTopic(name string, opts TopicOptions) *Topic {
	t := &Topic{name: name, opts: opts}
	t.cond = sync.NewCond(&t.mu)
	t.space = sync.NewCond(&t.mu)
	return t
}

// Publish appends msg to the topic, assigns its offset, and returns that offset.
// If msg.Timestamp is zero it is set to time.Now().UTC(). If msg.ID is empty a
// random 32-character hex string is assigned. Under BackpressureBlock a full
// topic parks the caller until an Ack frees space; under BackpressureReject it
// returns ErrTopicFull at once.
func (t *Topic) Publish(msg *Message) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for !t.closed && t.isFull(msg) {
		if t.opts.FullPolicy == BackpressureReject {
			return 0, ErrTopicFull
		}
		t.space.Wait()
	}
	if t.closed {
		return 0, ErrBrokerClosed
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now().UTC()
	}
	if msg.ID == "" {
		msg.ID = randHex(16)
	}
	msg.Offset = int64(len(t.messages))
	msg.Topic = t.name
	t.messages = append(t.messages, msg)
	t.totalBytes += msg.Size()
	t.cond.Broadcast()
	return msg.Offset, nil
}

// isFull reports whether appending msg would exceed configured limits.
// Must be called with t.mu held.
func (t *Topic) isFull(msg *Message) bool {
	if t.opts.MaxMessages > 0 && len(t.messages) >= t.opts.MaxMessages {
		return true
	}
	if t.opts.MaxBytes > 0 && t.totalBytes+msg.Size() > t.opts.MaxBytes {
		return true
	}
	return false
}

// GetMessage returns the message at offset. Returns ErrInvalidOffset if out of range.
func (t *Topic) GetMessage(offset int64) (*Message, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.getMessageLocked(offset)
}

// getMessageLocked is the internal helper; must be called with t.mu held.
func (t *Topic) getMessageLocked(offset int64) (*Message, error) {
	if offset < 0 || offset >= int64(len(t.messages)) {
		return nil, fmt.Errorf("%w: %d", ErrInvalidOffset, offset)
	}
	return t.messages[offset], nil
}

// Len returns the current message count.
func (t *Topic) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.messages)
}

// TopicStats is a snapshot of a topic's current metrics.
type TopicStats struct {
	MessageCount int
	TotalBytes   int
	OldestAt     time.Time
	NewestAt     time.Time
}

// Stats returns a snapshot of the topic's current metrics.
func (t *Topic) Stats() TopicStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := TopicStats{
		MessageCount: len(t.messages),
		TotalBytes:   t.totalBytes,
	}
	if len(t.messages) > 0 {
		s.OldestAt = t.messages[0].Timestamp
		s.NewestAt = t.messages[len(t.messages)-1].Timestamp
	}
	return s
}

// close signals all waiters to exit. Must be called with t.mu held.
func (t *Topic) close() {
	t.closed = true
	t.cond.Broadcast()
	t.space.Broadcast()
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
```

### The blocking Poll, and the at-least-once machinery

`Poll` is the heart of the subscriber side, and reading it carefully repays the effort. It locks the topic once and loops. On each iteration it first bails out if the topic was closed (so a deleted topic unblocks every poller with `ErrBrokerClosed`), then it serves any *expired* in-flight messages, then any *new* messages from the cursor, and only if both come up empty does it compute the remaining time and wait. The order matters: serving expired redeliveries before new messages is what guarantees a repeatedly-Nacked message can never be starved by a flood of fresh traffic.

The wait itself is the `sync.Cond` plus `time.AfterFunc` pattern from the concepts. `cond.Wait()` atomically releases `t.mu` and suspends, so the timer callback — which acquires the very same `t.mu` to broadcast — cannot deadlock against the sleeping waiter. The timer is stopped the instant `Wait` returns, whether it returned because a publisher broadcast, because the deadline fired, or spuriously; the loop then re-checks every condition from the top, which is why `Wait` lives inside a `for`, never an `if`.

Delivery state is a small map from offset to a `deliveryRecord{state, deliveredAt}`. `fetchNewLocked` advances the cursor and stamps each delivered message with the current time. `collectExpiredLocked` scans the in-flight records and returns any whose `deliveredAt` is older than the visibility TTL, refreshing the stamp so the consumer gets a clean window to acknowledge the redelivery. `Ack` flips a record to acknowledged (and signals `space`, releasing any publisher blocked on backpressure). `Nack` sets `deliveredAt` to the zero `time.Time`, which makes `now.Sub(deliveredAt)` astronomically large so the very next `collectExpiredLocked` treats the message as expired and redelivers it without waiting out the full timeout.

Append to `mq.go`:

```go
// SubscriptionOptions configures a Subscription at creation time.
type SubscriptionOptions struct {
	Mode              DeliveryMode
	VisibilityTimeout time.Duration // 0 -> 30 seconds
	StartOffset       int64         // negative -> 0 (earliest)
}

// deliveryRecord tracks the in-flight state of one message in one subscription.
type deliveryRecord struct {
	state       MessageState
	deliveredAt time.Time
}

// Subscription is a named cursor over a Topic.
// In Broadcast mode, each distinct name gets an independent cursor.
// In CompetingConsumers mode, subscriptions sharing a name share one cursor:
// the topic mutex serializes Poll calls so each message goes to exactly one caller.
type Subscription struct {
	name          string
	topic         *Topic
	visTTL        time.Duration
	currentOffset int64
	records       map[int64]*deliveryRecord
}

func newSubscription(name string, topic *Topic, opts SubscriptionOptions) *Subscription {
	ttl := opts.VisibilityTimeout
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	start := opts.StartOffset
	if start < 0 {
		start = 0
	}
	return &Subscription{
		name:          name,
		topic:         topic,
		visTTL:        ttl,
		currentOffset: start,
		records:       make(map[int64]*deliveryRecord),
	}
}

// Name returns the subscription's name.
func (s *Subscription) Name() string { return s.name }

// Poll returns up to maxMessages messages. It re-delivers expired messages first,
// then fetches new ones from the current offset. It blocks for up to timeout if no
// messages are available. Returns nil, nil on timeout. Returns nil, ErrBrokerClosed
// if the topic was deleted while Poll was blocking.
func (s *Subscription) Poll(maxMessages int, timeout time.Duration) ([]*Message, error) {
	deadline := time.Now().Add(timeout)

	s.topic.mu.Lock()
	defer s.topic.mu.Unlock()

	for {
		if s.topic.closed {
			return nil, ErrBrokerClosed
		}
		if msgs := s.collectExpiredLocked(); len(msgs) > 0 {
			return msgs, nil
		}
		if msgs := s.fetchNewLocked(maxMessages); len(msgs) > 0 {
			return msgs, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}
		// time.AfterFunc fires a goroutine that wakes all waiters after the deadline.
		// cond.Wait atomically releases s.topic.mu while suspended, so the timer
		// goroutine can acquire it without deadlock.
		timer := time.AfterFunc(remaining, func() {
			s.topic.mu.Lock()
			s.topic.cond.Broadcast()
			s.topic.mu.Unlock()
		})
		s.topic.cond.Wait()
		timer.Stop()
	}
}

// collectExpiredLocked returns messages whose visibility timeout has elapsed.
// It resets their deliveredAt so the subscriber gets a fresh window before the
// next redelivery. Must be called with s.topic.mu held.
func (s *Subscription) collectExpiredLocked() []*Message {
	now := time.Now()
	var msgs []*Message
	for off, rec := range s.records {
		if rec.state != StateDelivered {
			continue
		}
		if now.Sub(rec.deliveredAt) <= s.visTTL {
			continue
		}
		rec.deliveredAt = now
		if msg, err := s.topic.getMessageLocked(off); err == nil {
			msgs = append(msgs, msg)
		}
	}
	return msgs
}

// fetchNewLocked fetches up to n messages starting at currentOffset and advances
// the cursor. Must be called with s.topic.mu held.
func (s *Subscription) fetchNewLocked(n int) []*Message {
	var msgs []*Message
	for int(s.currentOffset) < len(s.topic.messages) && len(msgs) < n {
		off := s.currentOffset
		msg := s.topic.messages[off]
		s.records[off] = &deliveryRecord{state: StateDelivered, deliveredAt: time.Now()}
		s.currentOffset++
		msgs = append(msgs, msg)
	}
	return msgs
}

// Ack marks a message as permanently acknowledged. Acknowledged messages are never
// redelivered. It also signals the backpressure condition variable so blocked
// publishers can retry Publish.
func (s *Subscription) Ack(offset int64) error {
	s.topic.mu.Lock()
	defer s.topic.mu.Unlock()

	rec, ok := s.records[offset]
	if !ok {
		return fmt.Errorf("%w: %d", ErrInvalidOffset, offset)
	}
	if rec.state == StateAcknowledged {
		return fmt.Errorf("%w: %d", ErrAlreadyAcked, offset)
	}
	rec.state = StateAcknowledged
	s.topic.space.Broadcast()
	return nil
}

// Nack immediately expires the visibility window of a message, causing it to be
// redelivered on the very next Poll without waiting for the full visibility timeout.
// Setting deliveredAt to the zero time makes now.Sub(deliveredAt) enormous, so
// collectExpiredLocked treats it as expired on the next call.
func (s *Subscription) Nack(offset int64) error {
	s.topic.mu.Lock()
	defer s.topic.mu.Unlock()

	rec, ok := s.records[offset]
	if !ok {
		return fmt.Errorf("%w: %d", ErrInvalidOffset, offset)
	}
	rec.deliveredAt = time.Time{} // zero time -> age is enormous -> expired immediately
	s.topic.cond.Broadcast()
	return nil
}
```

### The Broker: topic lifecycle and subscription registry

The `Broker` is a thin manager around a map of topics and a nested map of subscriptions. Its own `sync.RWMutex` guards only the maps, never the per-topic state; the two locks never nest in a way that can cycle, because broker methods that touch a topic's lock (`DeleteTopic`, `Close`) take the topic lock briefly and release it before returning. `Subscribe` encodes the broadcast-versus-competing distinction in one place: in `CompetingConsumers` mode a repeated `subName` returns the *existing* `*Subscription`, so every caller in the group shares one cursor; in `Broadcast` mode each call mints a fresh, independent cursor. `DeleteTopic` and `Close` both close the underlying topic, which broadcasts on both condition variables and flips `closed`, so every blocked `Poll` and every blocked `Publish` returns promptly instead of hanging.

Append to `mq.go`:

```go
// Broker manages named topics and their subscriptions.
type Broker struct {
	mu     sync.RWMutex
	topics map[string]*Topic
	subs   map[string]map[string]*Subscription // topicName -> subName -> sub
	closed bool
}

// NewBroker creates a new in-memory Broker.
func NewBroker() *Broker {
	return &Broker{
		topics: make(map[string]*Topic),
		subs:   make(map[string]map[string]*Subscription),
	}
}

// CreateTopic creates a named topic. Returns ErrTopicExists if the name is taken.
func (b *Broker) CreateTopic(name string, opts TopicOptions) (*Topic, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrBrokerClosed
	}
	if _, ok := b.topics[name]; ok {
		return nil, fmt.Errorf("%w: %s", ErrTopicExists, name)
	}
	t := newTopic(name, opts)
	b.topics[name] = t
	b.subs[name] = make(map[string]*Subscription)
	return t, nil
}

// DeleteTopic closes all subscriptions on a topic and removes it from the broker.
// Any Poll blocked on the topic will return ErrBrokerClosed.
func (b *Broker) DeleteTopic(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	t, ok := b.topics[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTopicNotFound, name)
	}
	t.mu.Lock()
	t.close()
	t.mu.Unlock()

	delete(b.topics, name)
	delete(b.subs, name)
	return nil
}

// Subscribe creates a Subscription on topicName.
//
// In CompetingConsumers mode, multiple calls with the same subName return the
// same *Subscription (shared cursor). Concurrent Poll calls on that shared value
// are serialized by the topic mutex so each message reaches exactly one caller.
//
// In Broadcast mode, each call creates an independent cursor regardless of subName.
func (b *Broker) Subscribe(topicName, subName string, opts SubscriptionOptions) (*Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, ErrBrokerClosed
	}
	t, ok := b.topics[topicName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTopicNotFound, topicName)
	}
	topicSubs := b.subs[topicName]
	if opts.Mode == CompetingConsumers {
		if existing, ok := topicSubs[subName]; ok {
			return existing, nil
		}
	}
	sub := newSubscription(subName, t, opts)
	topicSubs[subName] = sub
	return sub, nil
}

// ListTopics returns the names of all live topics.
func (b *Broker) ListTopics() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.topics))
	for name := range b.topics {
		names = append(names, name)
	}
	return names
}

// ListSubscriptions returns the subscription names registered for the given topic.
func (b *Broker) ListSubscriptions(topicName string) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	subs, ok := b.subs[topicName]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTopicNotFound, topicName)
	}
	names := make([]string, 0, len(subs))
	for name := range subs {
		names = append(names, name)
	}
	return names, nil
}

// Close shuts down the broker and unblocks all waiting Polls.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for _, t := range b.topics {
		t.mu.Lock()
		t.close()
		t.mu.Unlock()
	}
}
```

### The runnable demo

The demo walks the minimal lifecycle: create a topic, register a broadcast subscriber, publish five events, then poll the batch and acknowledge each one. Each published message gets a random 32-hex-character ID and its `Topic` set to `"events"` (6 bytes); the values `event-0` through `event-4` are 7 bytes each, so every message reports `Size()` of 32+6+7 = 45 bytes and five of them total 225.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/mq"
)

func main() {
	b := mq.NewBroker()
	defer b.Close()

	topic, err := b.CreateTopic("events", mq.TopicOptions{})
	if err != nil {
		log.Fatal(err)
	}

	sub, err := b.Subscribe("events", "logger", mq.SubscriptionOptions{
		Mode: mq.Broadcast,
	})
	if err != nil {
		log.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		off, err := topic.Publish(&mq.Message{
			Value: []byte(fmt.Sprintf("event-%d", i)),
		})
		if err != nil {
			log.Fatalf("Publish: %v", err)
		}
		fmt.Printf("published event-%d at offset %d\n", i, off)
	}

	msgs, err := sub.Poll(10, time.Second)
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range msgs {
		fmt.Printf("received [%d]: %s\n", m.Offset, m.Value)
		if err := sub.Ack(m.Offset); err != nil {
			log.Printf("Ack %d: %v", m.Offset, err)
		}
	}

	stats := topic.Stats()
	fmt.Printf("stats: %d messages, %d bytes\n", stats.MessageCount, stats.TotalBytes)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
published event-0 at offset 0
published event-1 at offset 1
published event-2 at offset 2
published event-3 at offset 3
published event-4 at offset 4
received [0]: event-0
received [1]: event-1
received [2]: event-2
received [3]: event-3
received [4]: event-4
stats: 5 messages, 225 bytes
```

### Tests

The tests pin down the contract that the prose claims. `TestPublishAssignsUniqueMonotonicOffsets` hammers one topic with ten concurrent publishers and asserts every offset is distinct — the property that the under-lock offset assignment exists to guarantee. `TestBroadcastDeliversAllMessagesToEachSubscriber` confirms fan-out. `TestAckPreventsRedelivery` and `TestNackTriggersImmediateRedelivery` exercise the two ends of the visibility-timeout machine. `TestTopicFullRejectMode` checks backpressure. `TestTopicDeletionUnblocksPollers` proves a blocked `Poll` returns when its topic is deleted. `TestCompetingConsumersNoLoss` is the strongest one: it publishes 100 messages, polls a five-member group concurrently, and asserts each offset is delivered exactly once across the whole group — no loss, no duplication. The whole file runs under `-race`, which is the gate that catches the data races and lock-ordering deadlocks the type system cannot.

Create `mq_test.go`:

```go
package mq

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestPublishAssignsUniqueMonotonicOffsets(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	topic, err := b.CreateTopic("offsets", TopicOptions{})
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 10
	const msgsEach = 100
	offsets := make(chan int64, goroutines*msgsEach)
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < msgsEach; j++ {
				off, err := topic.Publish(&Message{Value: []byte("x")})
				if err != nil {
					t.Errorf("Publish: %v", err)
					return
				}
				offsets <- off
			}
		}()
	}
	wg.Wait()
	close(offsets)

	seen := make(map[int64]bool)
	for off := range offsets {
		if seen[off] {
			t.Fatalf("duplicate offset %d", off)
		}
		seen[off] = true
	}
	total := goroutines * msgsEach
	if len(seen) != total {
		t.Fatalf("got %d unique offsets, want %d", len(seen), total)
	}
}

func TestBroadcastDeliversAllMessagesToEachSubscriber(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	topic, _ := b.CreateTopic("broadcast", TopicOptions{})

	const subCount = 3
	const msgCount = 20
	subs := make([]*Subscription, subCount)
	for i := range subs {
		sub, err := b.Subscribe("broadcast", fmt.Sprintf("sub-%d", i), SubscriptionOptions{
			Mode: Broadcast,
		})
		if err != nil {
			t.Fatal(err)
		}
		subs[i] = sub
	}

	for i := 0; i < msgCount; i++ {
		if _, err := topic.Publish(&Message{Value: []byte(fmt.Sprintf("msg-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}

	for _, sub := range subs {
		msgs, err := sub.Poll(msgCount, 100*time.Millisecond)
		if err != nil {
			t.Fatalf("sub %s: %v", sub.Name(), err)
		}
		if len(msgs) != msgCount {
			t.Fatalf("sub %s: got %d messages, want %d", sub.Name(), len(msgs), msgCount)
		}
		for i, m := range msgs {
			if m.Offset != int64(i) {
				t.Fatalf("sub %s: msgs[%d].Offset = %d, want %d", sub.Name(), i, m.Offset, i)
			}
		}
	}
}

func TestAckPreventsRedelivery(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	topic, _ := b.CreateTopic("ack", TopicOptions{})
	sub, _ := b.Subscribe("ack", "consumer", SubscriptionOptions{
		Mode:              Broadcast,
		VisibilityTimeout: 30 * time.Millisecond,
	})

	if _, err := topic.Publish(&Message{Value: []byte("payload")}); err != nil {
		t.Fatal(err)
	}

	msgs, err := sub.Poll(1, 100*time.Millisecond)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("first Poll: msgs=%v err=%v", msgs, err)
	}
	if err := sub.Ack(msgs[0].Offset); err != nil {
		t.Fatal(err)
	}

	// Sleep past the visibility timeout; the acknowledged message must not reappear.
	time.Sleep(60 * time.Millisecond)
	second, err := sub.Poll(1, 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("acknowledged message was redelivered: %+v", second[0])
	}
}

func TestNackTriggersImmediateRedelivery(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	topic, _ := b.CreateTopic("nack", TopicOptions{})
	sub, _ := b.Subscribe("nack", "consumer", SubscriptionOptions{
		Mode:              Broadcast,
		VisibilityTimeout: 10 * time.Second, // long so only Nack can trigger redelivery
	})

	if _, err := topic.Publish(&Message{Value: []byte("hello")}); err != nil {
		t.Fatal(err)
	}

	first, _ := sub.Poll(1, 100*time.Millisecond)
	if len(first) != 1 {
		t.Fatal("expected 1 message on first Poll")
	}
	if err := sub.Nack(first[0].Offset); err != nil {
		t.Fatal(err)
	}

	second, _ := sub.Poll(1, 100*time.Millisecond)
	if len(second) != 1 {
		t.Fatal("expected redelivery after Nack; got none")
	}
	if second[0].Offset != first[0].Offset {
		t.Fatalf("redelivered offset %d != original %d", second[0].Offset, first[0].Offset)
	}
}

func TestTopicFullRejectMode(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	topic, _ := b.CreateTopic("full", TopicOptions{
		MaxMessages: 2,
		FullPolicy:  BackpressureReject,
	})

	if _, err := topic.Publish(&Message{Value: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := topic.Publish(&Message{Value: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	_, err := topic.Publish(&Message{Value: []byte("c")})
	if !errors.Is(err, ErrTopicFull) {
		t.Fatalf("err = %v, want ErrTopicFull", err)
	}
}

func TestTopicDeletionUnblocksPollers(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.CreateTopic("ephemeral", TopicOptions{})
	sub, _ := b.Subscribe("ephemeral", "s1", SubscriptionOptions{Mode: Broadcast})

	done := make(chan error, 1)
	go func() {
		_, err := sub.Poll(1, 5*time.Second)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	if err := b.DeleteTopic("ephemeral"); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, ErrBrokerClosed) {
			t.Fatalf("Poll after deletion: unexpected error %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Poll did not unblock after topic deletion")
	}
}

func TestMessageSize(t *testing.T) {
	t.Parallel()

	m := &Message{
		ID:      "abc",
		Topic:   "t",
		Key:     []byte("k"),
		Value:   []byte("hello"),
		Headers: map[string]string{"x": "y"},
	}
	got := m.Size()
	want := len("abc") + len("t") + len("k") + len("hello") + len("x") + len("y")
	if got != want {
		t.Fatalf("Size() = %d, want %d", got, want)
	}
}

func TestBrokerListTopicsAndSubscriptions(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	b.CreateTopic("alpha", TopicOptions{})
	b.CreateTopic("beta", TopicOptions{})
	b.Subscribe("alpha", "s1", SubscriptionOptions{Mode: Broadcast})
	b.Subscribe("alpha", "s2", SubscriptionOptions{Mode: Broadcast})

	if got := len(b.ListTopics()); got != 2 {
		t.Fatalf("ListTopics: got %d, want 2", got)
	}
	subs, err := b.ListSubscriptions("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 2 {
		t.Fatalf("ListSubscriptions(alpha): got %d, want 2", len(subs))
	}
	_, err = b.ListSubscriptions("nonexistent")
	if !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("expected ErrTopicNotFound, got %v", err)
	}
}

func TestCreateTopicDuplicate(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	if _, err := b.CreateTopic("dup", TopicOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := b.CreateTopic("dup", TopicOptions{})
	if !errors.Is(err, ErrTopicExists) {
		t.Fatalf("err = %v, want ErrTopicExists", err)
	}
}

func TestDeleteNonexistentTopic(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	err := b.DeleteTopic("ghost")
	if !errors.Is(err, ErrTopicNotFound) {
		t.Fatalf("err = %v, want ErrTopicNotFound", err)
	}
}

func TestTopicStats(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	topic, _ := b.CreateTopic("stats", TopicOptions{})

	s0 := topic.Stats()
	if s0.MessageCount != 0 || s0.TotalBytes != 0 {
		t.Fatalf("empty stats: %+v", s0)
	}

	topic.Publish(&Message{Value: []byte("hello")})
	topic.Publish(&Message{Value: []byte("world")})

	s2 := topic.Stats()
	if s2.MessageCount != 2 {
		t.Fatalf("MessageCount = %d, want 2", s2.MessageCount)
	}
	if s2.OldestAt.IsZero() || s2.NewestAt.IsZero() {
		t.Fatalf("timestamps not set: %+v", s2)
	}
	if s2.OldestAt.After(s2.NewestAt) {
		t.Fatalf("OldestAt > NewestAt: %v vs %v", s2.OldestAt, s2.NewestAt)
	}
}

func TestCompetingConsumersNoLoss(t *testing.T) {
	t.Parallel()

	b := NewBroker()
	topic, _ := b.CreateTopic("work", TopicOptions{})

	const msgCount = 100
	for i := 0; i < msgCount; i++ {
		if _, err := topic.Publish(&Message{Value: []byte(fmt.Sprintf("job-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 5
	group := make([]*Subscription, workers)
	for i := range group {
		sub, err := b.Subscribe("work", "group-1", SubscriptionOptions{Mode: CompetingConsumers})
		if err != nil {
			t.Fatal(err)
		}
		group[i] = sub
	}

	var mu sync.Mutex
	seen := make(map[int64]int)
	var wg sync.WaitGroup
	for _, sub := range group {
		wg.Add(1)
		go func(s *Subscription) {
			defer wg.Done()
			for {
				msgs, err := s.Poll(8, 50*time.Millisecond)
				if err != nil {
					t.Errorf("Poll: %v", err)
					return
				}
				if len(msgs) == 0 {
					return // drained: no message arrived within the timeout
				}
				mu.Lock()
				for _, m := range msgs {
					seen[m.Offset]++
				}
				mu.Unlock()
			}
		}(sub)
	}
	wg.Wait()

	if len(seen) != msgCount {
		t.Fatalf("got %d distinct offsets, want %d", len(seen), msgCount)
	}
	for off, n := range seen {
		if n != 1 {
			t.Fatalf("offset %d delivered %d times, want exactly 1", off, n)
		}
	}
}

// ExampleNewBroker shows the minimal publish-then-poll round trip.
func ExampleNewBroker() {
	b := NewBroker()
	topic, _ := b.CreateTopic("greetings", TopicOptions{})
	sub, _ := b.Subscribe("greetings", "printer", SubscriptionOptions{Mode: Broadcast})

	topic.Publish(&Message{Value: []byte("hello")})
	msgs, _ := sub.Poll(1, time.Second)
	fmt.Printf("received: %s\n", msgs[0].Value)
	// Output: received: hello
}
```

## Review

The broker is correct when one lock owns everything per topic. Confirm the subscription has no mutex of its own: the cursor and the in-flight records are only ever touched while `s.topic.mu` is held, which is what keeps lock ordering trivial and the race detector quiet. Confirm the offset is assigned under that lock, immediately before the append, so the concurrent-publisher test produces 1000 distinct offsets rather than a smaller number with collisions. Confirm `Poll` waits inside a `for` loop that re-checks closed-then-expired-then-new on every wakeup, never inside a bare `if`, and that the `time.AfterFunc` timer is stopped as soon as `Wait` returns so no stray goroutine outlives the call.

The mistakes to watch for are the ones from the concepts file made concrete here. Forgetting `t.cond.Broadcast()` at the end of `Publish` leaves the broadcast test hanging until the runner kills it. Serving new messages before expired ones lets a flood starve a Nacked message, which the redelivery test would expose if the order were swapped. Returning a bare `fmt.Errorf` instead of wrapping with `%w` makes the `errors.Is` assertions in the backpressure and not-found tests fail. The whole file passing under `go test -race` is the real proof: it is what certifies the single-lock design has no data race and no lock-ordering deadlock.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — the condition-variable API; the `Wait`/`Broadcast` contract is the foundation of the blocking poll.
- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) — the standard way to wake a condition variable after a deadline without a busy loop.
- [Apache Kafka Design: The Log](https://kafka.apache.org/documentation/#design_log) — the authoritative description of the append-only offset log and consumer-group model this exercise is modeled after.
- [Amazon SQS Visibility Timeout](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-visibility-timeout.html) — the at-least-once redelivery mechanism `Poll`/`Ack`/`Nack` implement.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-wildcard-subscriptions.md](02-wildcard-subscriptions.md)
