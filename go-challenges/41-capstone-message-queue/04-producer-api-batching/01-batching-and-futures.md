# Exercise 1: Batching and Futures

A producer that ships one record per request is latency-bound; one that coalesces records into batches and returns a `Future` for each is throughput-bound. This exercise builds that core: an accumulator that groups records per topic-partition, two flush triggers (a size threshold and a linger timer), a background sender bounded by a concurrency limit, and a clean shutdown that never loses a record or panics on a closed channel.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
producer.go          BrokerSender, Producer, Future, RecordBatch, Config
  Producer.Send      accumulate a record, arm the linger timer, size-flush when full
  Producer.Flush     push every pending batch to the sender now
  Producer.Close     flush, drain timers, close the channel, wait for the sender
cmd/
  demo/
    main.go          send 9 records, watch them coalesce into 3 batches
producer_test.go     flush triggers, future resolution, timeout, concurrent sends
```

- Files: `producer.go`, `cmd/demo/main.go`, `producer_test.go`.
- Implement: `Producer` with `Send`, `Flush`, `Close`, `Metrics`, plus `Future` with `Get` and `OnComplete`.
- Test: size flush fires before linger, linger flush fires on a slow stream, `Close` flushes the tail, a future times out, and 1000 concurrent sends all resolve.
- Verify: `go test -race ./...`

### How the accumulator and the sender stay decoupled

The producer runs two roles concurrently and the entire design exists to keep them from blocking each other. The accumulator is the `Send` path: it takes a short mutex, finds or creates the in-progress batch for the record's topic-partition, appends the record, and decides whether that batch is now full. The sender is one background goroutine that reads finished batches off a buffered channel and dispatches each to the broker inside its own goroutine, with a counting semaphore (`inflight`) capping how many run at once. The mutex guards only the accumulator's map of in-progress batches; the sender never touches that map, so the lock is held for microseconds and the two roles almost never contend.

The one rule that makes this safe is that the mutex is never held across a channel send. Inside `Send`, the code decides under the lock whether a batch became ready, stores it in a local `ready` variable, unlocks, and only then does `readyCh <- ready`. If the lock were held while the channel was full, no other goroutine could `Send` and the producer would wedge. Read the `Send` body with that in mind: every `p.readyCh <- ...` sits after a `p.mu.Unlock()`, never before it.

### Two triggers and the race between them

A batch ships for one of two reasons. The size trigger lives at the bottom of `Send`: each append adds `len(value)` to `SizeBytes`, and once that reaches `MaxBatchBytes` the batch is pulled from the map and queued immediately, so a fast stream never buffers more than one batch worth of bytes per partition. The time trigger is a `time.AfterFunc` armed when the first record of a new batch arrives; if the batch never fills, the callback fires after `LingerMs`, pulls the batch, and queues it, so a slow stream waits at most the linger interval.

These two can race: the timer may fire at the same instant a size flush is removing the same batch. Both paths take the mutex and both check whether the batch is still in `p.batches`. Whoever calls `delete` first owns the batch; the other sees `ok == false` and does nothing. That is why the timer callback re-checks the map under the lock instead of trusting that the batch it was armed for still exists.

### Why Close is ordered the way it is

The hardest bug in this whole design hides in shutdown, and it turns on `time.Timer.Stop`. `Stop` returns `true` when it cancelled the timer before the callback ran (the callback will never run) and `false` when the timer had already fired (the callback is running or queued and will run). The producer tracks live callbacks with `timerWg`: `Add(1)` before arming, `Done` from the callback's `defer`. Every place that stops a timer writes `if t.Stop() { p.timerWg.Done() }`, because a `true` return means the callback that would have called `Done` is never going to.

`Close` then runs in a fixed order whose every step is load-bearing: set `closed` so new sends are rejected, `Flush` the pending batches, `timerWg.Wait()` so no already-fired callback can still be queued to send, then `close(readyCh)`, then `wg.Wait()` for the sender to finish the last batch. Reorder `close(readyCh)` before `timerWg.Wait()` and an in-flight callback sends on a closed channel and panics; that is the canonical mistake this ordering exists to prevent.

### Futures resolve exactly once

`Send` returns a `*Future` wrapping a channel buffered to one. `resolve` does a non-blocking send, so the first result is stored and any later resolve is dropped silently, which means the sender never blocks while resolving and a double resolve cannot panic. `Get` waits on the result or a timeout; `OnComplete` reads the single result in a goroutine and runs a callback. One `Send` makes one future appended to one batch, and `resolveAll` walks that batch's futures once, handing record `i` the offset `baseOffset + i` returned by the broker.

Create `producer.go`:

```go
package producer

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// BrokerSender is the network boundary the producer calls. Tests inject a mock;
// a real deployment dials the broker. It returns the base offset the broker
// assigned to the first record in the batch.
type BrokerSender interface {
	Send(batch *RecordBatch) (baseOffset int64, err error)
}

// TopicPartition uniquely identifies a log stream.
type TopicPartition struct {
	Topic     string
	Partition int32
}

// RecordMetadata is the per-record acknowledgment a Future resolves to.
type RecordMetadata struct {
	Topic     string
	Partition int32
	Offset    int64
	Timestamp time.Time
}

type sendResult struct {
	meta *RecordMetadata
	err  error
}

// Future resolves when the batch containing its record is acknowledged.
type Future struct {
	ch chan sendResult
}

func newFuture() *Future { return &Future{ch: make(chan sendResult, 1)} }

// resolve stores the first result and drops any later one. Non-blocking, so the
// sender never stalls and a double resolve cannot panic.
func (f *Future) resolve(r sendResult) {
	select {
	case f.ch <- r:
	default:
	}
}

// Get blocks until the future resolves or the timeout elapses.
func (f *Future) Get(timeout time.Duration) (*RecordMetadata, error) {
	select {
	case r := <-f.ch:
		return r.meta, r.err
	case <-time.After(timeout):
		return nil, ErrTimeout
	}
}

// OnComplete runs fn in a new goroutine when the future resolves. Each future
// must be consumed by exactly one of Get or OnComplete.
func (f *Future) OnComplete(fn func(*RecordMetadata, error)) {
	go func() {
		r := <-f.ch
		fn(r.meta, r.err)
	}()
}

// Sentinel errors.
var (
	ErrTimeout       = errors.New("producer: future timed out")
	ErrClosed        = errors.New("producer: closed")
	ErrMessageTooBig = errors.New("producer: message exceeds MaxBatchBytes")
	ErrBadConfig     = errors.New("producer: invalid configuration")
)

// Record is a single user message before batching.
type Record struct {
	Key     []byte
	Value   []byte
	Headers map[string]string
}

// RecordBatch accumulates records destined for one TopicPartition.
// Exported fields are read by BrokerSender implementations.
type RecordBatch struct {
	TP         TopicPartition
	Records    []Record
	SizeBytes  int
	FirstAdded time.Time
	futures    []*Future
}

// resolveAll hands record i the offset baseOffset+i, or the error to every
// future on failure.
func (b *RecordBatch) resolveAll(baseOffset int64, err error) {
	now := time.Now().UTC()
	for i, f := range b.futures {
		if err != nil {
			f.resolve(sendResult{err: err})
			continue
		}
		f.resolve(sendResult{meta: &RecordMetadata{
			Topic:     b.TP.Topic,
			Partition: b.TP.Partition,
			Offset:    baseOffset + int64(i),
			Timestamp: now,
		}})
	}
}

// ProducerMetrics is a point-in-time snapshot of producer statistics.
type ProducerMetrics struct {
	MessagesSent int64
	BytesSent    int64
	BatchesSent  int64
	ErrorCount   int64
}

// Config holds the tunable parameters for a Producer.
type Config struct {
	// MaxBatchBytes triggers a size flush when reached (default 16 KiB).
	MaxBatchBytes int
	// LingerMs is how long a partial batch waits before shipping (default 5 ms).
	LingerMs int
	// MaxInFlight caps concurrent in-flight batches (default 5).
	MaxInFlight int
}

func (c *Config) applyDefaults() {
	if c.MaxBatchBytes <= 0 {
		c.MaxBatchBytes = 16 * 1024
	}
	if c.LingerMs <= 0 {
		c.LingerMs = 5
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 5
	}
}

// Producer accumulates records into batches and dispatches them asynchronously.
type Producer struct {
	cfg    Config
	broker BrokerSender

	mu      sync.Mutex
	batches map[TopicPartition]*RecordBatch
	timers  map[TopicPartition]*time.Timer
	closed  bool
	metrics ProducerMetrics

	timerWg  sync.WaitGroup    // tracks in-flight AfterFunc callbacks
	readyCh  chan *RecordBatch // finished batches awaiting dispatch
	inflight chan struct{}     // semaphore: capacity = MaxInFlight
	wg       sync.WaitGroup    // tracks sender + per-batch goroutines
}

// NewProducer creates a Producer and starts its background sender.
func NewProducer(cfg Config, broker BrokerSender) (*Producer, error) {
	if broker == nil {
		return nil, fmt.Errorf("%w: broker must not be nil", ErrBadConfig)
	}
	cfg.applyDefaults()
	p := &Producer{
		cfg:      cfg,
		broker:   broker,
		batches:  make(map[TopicPartition]*RecordBatch),
		timers:   make(map[TopicPartition]*time.Timer),
		readyCh:  make(chan *RecordBatch, 256),
		inflight: make(chan struct{}, cfg.MaxInFlight),
	}
	p.wg.Add(1)
	go p.sender()
	return p, nil
}

// Send enqueues a record for batched delivery and returns a Future that
// resolves when the batch is acknowledged or fails.
func (p *Producer) Send(topic string, partition int32, key, value []byte, headers map[string]string) (*Future, error) {
	if len(value) > p.cfg.MaxBatchBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrMessageTooBig, len(value), p.cfg.MaxBatchBytes)
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}

	tp := TopicPartition{Topic: topic, Partition: partition}
	batch, exists := p.batches[tp]
	if !exists {
		batch = &RecordBatch{TP: tp, FirstAdded: time.Now()}
		p.batches[tp] = batch

		linger := time.Duration(p.cfg.LingerMs) * time.Millisecond
		p.timerWg.Add(1)
		p.timers[tp] = time.AfterFunc(linger, func() {
			defer p.timerWg.Done()
			p.mu.Lock()
			b, ok := p.batches[tp]
			if ok {
				delete(p.batches, tp)
				delete(p.timers, tp)
			}
			p.mu.Unlock()
			if ok {
				p.readyCh <- b
			}
		})
	}

	batch.Records = append(batch.Records, Record{Key: key, Value: value, Headers: headers})
	batch.SizeBytes += len(value)
	f := newFuture()
	batch.futures = append(batch.futures, f)

	var ready *RecordBatch
	if batch.SizeBytes >= p.cfg.MaxBatchBytes {
		if t, ok := p.timers[tp]; ok {
			if t.Stop() {
				// Stopped before firing: the callback will never run, so we
				// must release its WaitGroup token ourselves.
				p.timerWg.Done()
			}
			delete(p.timers, tp)
		}
		delete(p.batches, tp)
		ready = batch
	}
	p.mu.Unlock()

	if ready != nil {
		p.readyCh <- ready
	}
	return f, nil
}

// Flush ships every accumulated batch immediately, bypassing linger.
func (p *Producer) Flush() {
	p.mu.Lock()
	ready := make([]*RecordBatch, 0, len(p.batches))
	for tp, b := range p.batches {
		if t, ok := p.timers[tp]; ok {
			if t.Stop() {
				p.timerWg.Done()
			}
			delete(p.timers, tp)
		}
		ready = append(ready, b)
		delete(p.batches, tp)
	}
	p.mu.Unlock()

	for _, b := range ready {
		p.readyCh <- b
	}
}

// Close flushes pending batches, waits for timer callbacks and the sender to
// finish, then shuts down. Safe to call more than once.
func (p *Producer) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	p.Flush()
	p.timerWg.Wait() // no callback can still be queued to send
	close(p.readyCh)
	p.wg.Wait()
	return nil
}

// Metrics returns a snapshot of producer statistics.
func (p *Producer) Metrics() ProducerMetrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.metrics
}

// sender drains readyCh, dispatching each batch under the inflight semaphore.
func (p *Producer) sender() {
	defer p.wg.Done()
	for batch := range p.readyCh {
		p.inflight <- struct{}{} // acquire: blocks at MaxInFlight
		p.wg.Add(1)
		go func(b *RecordBatch) {
			defer p.wg.Done()
			defer func() { <-p.inflight }()
			p.dispatch(b)
		}(batch)
	}
}

// dispatch sends one batch to the broker and resolves its futures.
func (p *Producer) dispatch(b *RecordBatch) {
	baseOffset, err := p.broker.Send(b)
	p.mu.Lock()
	if err != nil {
		p.metrics.ErrorCount++
	} else {
		p.metrics.BatchesSent++
		p.metrics.MessagesSent += int64(len(b.Records))
		p.metrics.BytesSent += int64(b.SizeBytes)
	}
	p.mu.Unlock()
	b.resolveAll(baseOffset, err)
}
```

### The runnable demo

The demo sends nine 10-byte records to one partition with `MaxBatchBytes: 30`, so a size flush fires after every third record and the nine records coalesce into three batches. `MaxInFlight: 1` serializes dispatch, which makes the broker's output order deterministic. The broker assigns each batch a base offset equal to the running record count, and the futures report per-record offsets `baseOffset + i`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/batching-and-futures"
)

// demoBroker assigns each batch a base offset equal to the running count.
type demoBroker struct{ nextOffset int64 }

func (b *demoBroker) Send(batch *producer.RecordBatch) (int64, error) {
	base := b.nextOffset
	b.nextOffset += int64(len(batch.Records))
	fmt.Printf("broker: tp=%s/%d records=%d bytes=%d baseOffset=%d\n",
		batch.TP.Topic, batch.TP.Partition, len(batch.Records), batch.SizeBytes, base)
	return base, nil
}

func main() {
	broker := &demoBroker{}
	p, err := producer.NewProducer(producer.Config{
		MaxBatchBytes: 30,
		LingerMs:      50,
		MaxInFlight:   1,
	}, broker)
	if err != nil {
		log.Fatal(err)
	}

	const n = 9
	futures := make([]*producer.Future, n)
	for i := 0; i < n; i++ {
		f, err := p.Send("orders", 0, nil, []byte("0123456789"), nil)
		if err != nil {
			log.Fatal(err)
		}
		futures[i] = f
	}

	if err := p.Close(); err != nil {
		log.Fatal(err)
	}

	first, _ := futures[0].Get(time.Second)
	last, _ := futures[n-1].Get(time.Second)
	fmt.Printf("first record offset=%d, last record offset=%d\n", first.Offset, last.Offset)

	m := p.Metrics()
	fmt.Printf("messages=%d batches=%d bytes=%d errors=%d\n",
		m.MessagesSent, m.BatchesSent, m.BytesSent, m.ErrorCount)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
broker: tp=orders/0 records=3 bytes=30 baseOffset=0
broker: tp=orders/0 records=3 bytes=30 baseOffset=3
broker: tp=orders/0 records=3 bytes=30 baseOffset=6
first record offset=0, last record offset=8
messages=9 batches=3 bytes=90 errors=0
```

### Tests

The tests pin the behaviors that define the producer. The flush-trigger tests prove a full batch ships on size before the long linger could fire, and a single record ships on linger after the interval. `TestCloseFlushesPending` proves no record is stranded at shutdown. `TestFutureTimesOut` proves `Get` honors its deadline when the broker hangs. `TestConcurrentSends` runs 1000 sends across 20 goroutines and asserts every future resolves and every record reaches the broker, which is the property `-race` exists to stress.

Create `producer_test.go`:

```go
package producer

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// nopBroker accepts every batch and assigns sequential offsets.
type nopBroker struct {
	mu     sync.Mutex
	offset int64
}

func (b *nopBroker) Send(batch *RecordBatch) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	base := b.offset
	b.offset += int64(len(batch.Records))
	return base, nil
}

// mockBroker records every batch it receives for later assertions.
type mockBroker struct {
	mu      sync.Mutex
	batches []*RecordBatch
	offset  int64
}

func (m *mockBroker) Send(b *RecordBatch) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := &RecordBatch{TP: b.TP, Records: append([]Record(nil), b.Records...), SizeBytes: b.SizeBytes}
	m.batches = append(m.batches, cp)
	base := m.offset
	m.offset += int64(len(b.Records))
	return base, nil
}

func (m *mockBroker) batchCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.batches)
}

func (m *mockBroker) totalRecords() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.batches {
		n += len(b.Records)
	}
	return n
}

func TestNewProducerRejectsNilBroker(t *testing.T) {
	t.Parallel()

	if _, err := NewProducer(Config{}, nil); !errors.Is(err, ErrBadConfig) {
		t.Fatalf("err = %v, want ErrBadConfig", err)
	}
}

func TestSendAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	p, _ := NewProducer(Config{LingerMs: 1}, &nopBroker{})
	_ = p.Close()

	if _, err := p.Send("t", 0, nil, []byte("late"), nil); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
}

func TestMessageTooBigRejectedAtSend(t *testing.T) {
	t.Parallel()

	p, _ := NewProducer(Config{MaxBatchBytes: 10, LingerMs: 1}, &nopBroker{})
	defer p.Close()

	if _, err := p.Send("t", 0, nil, make([]byte, 11), nil); !errors.Is(err, ErrMessageTooBig) {
		t.Errorf("err = %v, want ErrMessageTooBig", err)
	}
}

func TestSizeFlushFiresBeforeLinger(t *testing.T) {
	t.Parallel()

	broker := &mockBroker{}
	// Linger is huge, so only a size flush can ship a batch this quickly.
	p, _ := NewProducer(Config{MaxBatchBytes: 30, LingerMs: 60000}, broker)

	for i := 0; i < 3; i++ {
		if _, err := p.Send("t", 0, nil, []byte("0123456789"), nil); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for broker.batchCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if broker.batchCount() != 1 {
		t.Fatalf("batchCount = %d, want 1 (size flush)", broker.batchCount())
	}
	_ = p.Close()
}

func TestLingerFlushFiresAfterDelay(t *testing.T) {
	t.Parallel()

	broker := &mockBroker{}
	p, _ := NewProducer(Config{MaxBatchBytes: 1 << 20, LingerMs: 10}, broker)

	if _, err := p.Send("t", 0, nil, []byte("hello"), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	_ = p.Close()

	if broker.batchCount() != 1 {
		t.Errorf("batchCount = %d, want 1 (linger flush)", broker.batchCount())
	}
}

func TestCloseFlushesPending(t *testing.T) {
	t.Parallel()

	broker := &mockBroker{}
	p, _ := NewProducer(Config{MaxBatchBytes: 1 << 20, LingerMs: 60000}, broker)

	const n = 5
	futures := make([]*Future, n)
	for i := range futures {
		f, err := p.Send("t", 0, nil, []byte(fmt.Sprintf("msg-%d", i)), nil)
		if err != nil {
			t.Fatal(err)
		}
		futures[i] = f
	}
	_ = p.Close()

	for i, f := range futures {
		if _, err := f.Get(time.Second); err != nil {
			t.Errorf("futures[%d].Get() = %v, want nil", i, err)
		}
	}
	if broker.totalRecords() != n {
		t.Errorf("totalRecords = %d, want %d", broker.totalRecords(), n)
	}
}

func TestFutureResolvesWithOffset(t *testing.T) {
	t.Parallel()

	p, _ := NewProducer(Config{MaxBatchBytes: 1 << 20, LingerMs: 60000}, &nopBroker{})

	f0, _ := p.Send("orders", 2, nil, []byte("a"), nil)
	f1, _ := p.Send("orders", 2, nil, []byte("b"), nil)
	p.Flush()

	m0, err := f0.Get(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	m1, err := f1.Get(2 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if m0.Topic != "orders" || m0.Partition != 2 {
		t.Errorf("m0 = %+v, want topic=orders partition=2", m0)
	}
	if m1.Offset != m0.Offset+1 {
		t.Errorf("offsets = %d,%d, want consecutive", m0.Offset, m1.Offset)
	}
	_ = p.Close()
}

func TestOnCompleteFires(t *testing.T) {
	t.Parallel()

	p, _ := NewProducer(Config{MaxBatchBytes: 1 << 20, LingerMs: 1}, &nopBroker{})

	done := make(chan *RecordMetadata, 1)
	f, _ := p.Send("t", 0, nil, []byte("x"), nil)
	f.OnComplete(func(m *RecordMetadata, err error) { done <- m })

	select {
	case m := <-done:
		if m == nil {
			t.Fatal("OnComplete got nil metadata")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnComplete never fired")
	}
	_ = p.Close()
}

func TestFutureTimesOut(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	broker := &funcBroker{fn: func(_ *RecordBatch) (int64, error) {
		<-release
		return 0, nil
	}}
	p, _ := NewProducer(Config{MaxBatchBytes: 1 << 20, LingerMs: 1}, broker)

	f, _ := p.Send("t", 0, nil, []byte("hello"), nil)
	if _, err := f.Get(50 * time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
	close(release)
	_ = p.Close()
}

type funcBroker struct {
	fn func(*RecordBatch) (int64, error)
}

func (b *funcBroker) Send(batch *RecordBatch) (int64, error) { return b.fn(batch) }

func TestConcurrentSends(t *testing.T) {
	t.Parallel()

	broker := &mockBroker{}
	p, _ := NewProducer(Config{MaxBatchBytes: 256, LingerMs: 5}, broker)

	const goroutines, perG = 20, 50
	futures := make([]*Future, goroutines*perG)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				f, err := p.Send("events", 0, nil, []byte(fmt.Sprintf("m-%d-%d", g, i)), nil)
				if err != nil {
					t.Errorf("Send: %v", err)
					return
				}
				futures[g*perG+i] = f
			}
		}(g)
	}
	wg.Wait()
	_ = p.Close()

	for i, f := range futures {
		if f == nil {
			continue
		}
		if _, err := f.Get(5 * time.Second); err != nil {
			t.Errorf("futures[%d].Get() = %v", i, err)
		}
	}
	if broker.totalRecords() != goroutines*perG {
		t.Errorf("totalRecords = %d, want %d", broker.totalRecords(), goroutines*perG)
	}
}

func ExampleProducer_Send() {
	p, _ := NewProducer(Config{LingerMs: 1}, &nopBroker{})
	f, _ := p.Send("greetings", 0, nil, []byte("hi"), nil)
	meta, _ := f.Get(time.Second)
	fmt.Printf("topic=%s partition=%d offset=%d\n", meta.Topic, meta.Partition, meta.Offset)
	_ = p.Close()
	// Output: topic=greetings partition=0 offset=0
}
```

## Review

The producer is correct when the two roles never block each other and shutdown never races. Confirm that every `p.readyCh <- ...` sits after the mutex is released, that the size-flush path and the timer callback both re-check the map under the lock so exactly one of them owns a contested batch, and that `Close` runs `Flush`, `timerWg.Wait`, `close(readyCh)`, `wg.Wait` in that order. A test run of `TestConcurrentSends` under `-race` exercises all of it at once: 1000 records, 20 producers, size and linger both firing, and a clean `Close`, with every future resolving and the record count matching.

The mistakes worth naming. Holding the mutex across `p.readyCh <- ready` deadlocks the moment the channel fills, which is why the ready batch is captured in a local and sent after the unlock. Discarding the result of `t.Stop` leaves `timerWg` permanently short by one whenever a timer is cancelled before firing, so `Close` hangs; the `if t.Stop() { p.timerWg.Done() }` guard at every stop site is mandatory. Calling `close(readyCh)` before `timerWg.Wait` lets an already-fired callback send on a closed channel and panic. And resolving a future more than once is prevented structurally: one `Send` makes one future on one batch, and the buffered-to-one channel drops any extra resolve rather than blocking or panicking.

## Resources

- [`sync` package](https://pkg.go.dev/sync) — `Mutex` and `WaitGroup`, and the happens-before guarantees the producer relies on for race-free shutdown.
- [`time.AfterFunc`](https://pkg.go.dev/time#AfterFunc) — the linger timer primitive and the `Timer.Stop` return-value contract that `Close` depends on.
- [Kafka Producer Configs](https://kafka.apache.org/documentation/#producerconfigs) — `batch.size`, `linger.ms`, and `max.in.flight.requests.per.connection`, the production analogues of this `Config`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-retry-backoff-jitter.md](02-retry-backoff-jitter.md)
