# Exercise 4: Idempotent Producer

Retries make delivery at-least-once: if a broker commits a batch but the acknowledgment is lost, the producer retries and a naive broker appends the records twice. This exercise closes that gap. The producer stamps every batch with a stable `(ProducerID, BaseSeq)` key, and the broker deduplicates on that key so a retried batch is acknowledged from memory instead of being committed again. The result is exactly-once delivery over an unreliable network.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
idempotent.go        Producer, DedupBroker, NaiveBroker, RecordBatch
  Producer.Send      accumulate, freezing BaseSeq and bumping the per-partition seq
  Producer.Flush     deliver each batch with retry; BaseSeq is stable across tries
  DedupBroker.Send    dedup on (pid, partition, BaseSeq); a retry is a no-op append
cmd/
  demo/
    main.go          the same lost-ack scenario against dedup vs naive brokers
idempotent_test.go   exactly-once under lost acks, the naive double-append, sequences
```

- Files: `idempotent.go`, `cmd/demo/main.go`, `idempotent_test.go`.
- Implement: `Producer` (Send, Flush, Close, Metrics), `DedupBroker`, and `NaiveBroker`.
- Test: a lost ack against the dedup broker commits each record once, the same scenario against the naive broker double-commits, and per-partition sequences are independent and stable across retries.
- Verify: `go test -race ./...`

### The problem retries create

Picture the failure that idempotence exists to fix. The producer sends a batch; the broker appends it durably and assigns offsets; then the acknowledgment is dropped on the way back. The producer waited for an ack, never got one, and cannot tell "the commit failed" apart from "the commit succeeded but the ack was lost." Its only safe move is to retry. A broker that simply appends whatever arrives now holds the records twice, and the stream is corrupted in a way no downstream consumer can untangle. This is the gap between at-least-once and exactly-once, and no amount of network reliability closes it, because the ambiguous case is fundamental.

### Sequence numbers, frozen at enqueue

The fix is to make the broker able to recognize a batch it has already committed. Each producer instance carries a Producer ID, unique for its lifetime. For every topic-partition it keeps a monotonically increasing sequence counter, and when a batch is first created it captures the counter's current value as its `BaseSeq`. The pair `(ProducerID, BaseSeq)` is a stable identity for that exact batch.

The discipline that makes this work is in one line of `Send`: the per-partition counter is incremented when a record is enqueued, never when it is acknowledged. So a batch's `BaseSeq` is frozen the instant the batch is created, and every retry of that batch carries the identical key. Increment on acknowledgment instead and a retry of an unacknowledged batch would be handed a fresh, higher `BaseSeq`, the broker would not recognize the duplicate, and the record would commit twice, which defeats the entire scheme. The whole correctness argument rests on `BaseSeq` being assigned once, at enqueue, and never changing.

### The broker's dedup table

The broker keeps, keyed by `(ProducerID, partition, BaseSeq)`, the base offset it assigned the first time it committed that batch. When a batch arrives whose key is already in the table, the broker knows it is a retry of something already durable: it skips the append entirely and returns the original offset. The lost ack is now harmless. The first delivery committed the records and recorded the key; the retry, even though the producer believes the first attempt failed, is absorbed by the dedup table and acknowledged with the offset the records already have. Each record exists exactly once.

`DedupBroker.Send` models the lost-ack case precisely. It commits the batch and records the key first, then, if it is configured to drop this acknowledgment, returns a transient error anyway, exactly as if the commit had succeeded but the reply vanished. The producer's retry hits the dedup branch and succeeds. `NaiveBroker.Send` is identical minus the dedup table, so under the same dropped ack it appends a second time and its log ends up holding every dropped batch twice. The two brokers side by side are the lesson: idempotence is a property the broker provides, and the producer's job is only to keep the key stable so the broker can use it. This is exactly Kafka's idempotent producer (`enable.idempotence=true`), where the broker tracks the last sequence per producer and partition and rejects or ignores duplicates.

Create `idempotent.go`:

```go
package idempotent

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// TopicPartition identifies a log stream.
type TopicPartition struct {
	Topic     string
	Partition int32
}

// Record is one message inside a batch.
type Record struct {
	Key   []byte
	Value []byte
}

// RecordBatch carries the idempotence key (ProducerID, BaseSeq) the broker
// deduplicates on. BaseSeq is frozen when the batch is created.
type RecordBatch struct {
	TP         TopicPartition
	Records    []Record
	SizeBytes  int
	ProducerID int64
	BaseSeq    int64
}

// BrokerSender is the network boundary. It returns the base offset assigned to
// the batch's first record.
type BrokerSender interface {
	Send(batch *RecordBatch) (baseOffset int64, err error)
}

// Sentinel errors.
var (
	ErrClosed    = errors.New("idempotent: producer closed")
	ErrBadConfig = errors.New("idempotent: invalid configuration")
)

// Config tunes the producer.
type Config struct {
	// Retries is the number of retry attempts after the first try (default 3).
	Retries int
	// RetryBackoffMs is the backoff before each retry (default 10 ms).
	RetryBackoffMs int
}

func (c *Config) applyDefaults() {
	if c.Retries < 0 {
		c.Retries = 3
	}
	if c.RetryBackoffMs <= 0 {
		c.RetryBackoffMs = 10
	}
}

// Metrics is a snapshot of producer counters.
type Metrics struct {
	Records          int64
	BatchesCommitted int64
	Retries          int64
}

// pidCounter hands each Producer a distinct id.
var pidCounter atomic.Int64

// Producer accumulates records per partition and delivers them with retry,
// stamping each batch with a stable idempotence key.
type Producer struct {
	pid    int64
	cfg    Config
	broker BrokerSender

	mu      sync.Mutex
	seq     map[TopicPartition]int64
	batches map[TopicPartition]*RecordBatch
	closed  bool
	metrics Metrics
}

// NewProducer creates a Producer with a unique ProducerID.
func NewProducer(cfg Config, broker BrokerSender) (*Producer, error) {
	if broker == nil {
		return nil, fmt.Errorf("%w: broker must not be nil", ErrBadConfig)
	}
	cfg.applyDefaults()
	return &Producer{
		pid:     pidCounter.Add(1),
		cfg:     cfg,
		broker:  broker,
		seq:     make(map[TopicPartition]int64),
		batches: make(map[TopicPartition]*RecordBatch),
	}, nil
}

// ProducerID returns this producer's idempotence id.
func (p *Producer) ProducerID() int64 { return p.pid }

// Send accumulates a record into its partition's pending batch. The batch's
// BaseSeq is frozen at creation; the per-partition sequence is bumped here, at
// enqueue, so a retried batch keeps the same key.
func (p *Producer) Send(topic string, partition int32, key, value []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	tp := TopicPartition{Topic: topic, Partition: partition}
	b, ok := p.batches[tp]
	if !ok {
		b = &RecordBatch{TP: tp, ProducerID: p.pid, BaseSeq: p.seq[tp]}
		p.batches[tp] = b
	}
	b.Records = append(b.Records, Record{Key: key, Value: value})
	b.SizeBytes += len(value)
	p.seq[tp]++
	return nil
}

// Flush delivers every pending batch, retrying on transient failure. A retried
// batch carries the same (ProducerID, BaseSeq) it had on the first attempt.
func (p *Producer) Flush() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return ErrClosed
	}
	pending := make([]*RecordBatch, 0, len(p.batches))
	for tp, b := range p.batches {
		pending = append(pending, b)
		delete(p.batches, tp)
	}
	p.mu.Unlock()

	for _, b := range pending {
		if err := p.deliver(b); err != nil {
			return err
		}
	}
	return nil
}

func (p *Producer) deliver(b *RecordBatch) error {
	var lastErr error
	for attempt := 0; attempt <= p.cfg.Retries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(p.cfg.RetryBackoffMs) * time.Millisecond)
			p.addMetric(func(m *Metrics) { m.Retries++ })
		}
		if _, err := p.broker.Send(b); err == nil {
			p.addMetric(func(m *Metrics) {
				m.BatchesCommitted++
				m.Records += int64(len(b.Records))
			})
			return nil
		} else {
			lastErr = err
		}
	}
	return lastErr
}

func (p *Producer) addMetric(fn func(*Metrics)) {
	p.mu.Lock()
	fn(&p.metrics)
	p.mu.Unlock()
}

// Metrics returns a snapshot of the counters.
func (p *Producer) Metrics() Metrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.metrics
}

// Close flushes pending batches, then rejects further sends.
func (p *Producer) Close() error {
	if err := p.Flush(); err != nil {
		return err
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	return nil
}

// batchKey is the broker-side idempotence key.
type batchKey struct {
	pid       int64
	partition int32
	baseSeq   int64
}

// DedupBroker commits each unique (pid, partition, BaseSeq) once. A retry of an
// already-committed batch returns the original offset without re-appending. It
// can simulate dropAcks lost acknowledgments to exercise the retry path.
type DedupBroker struct {
	mu            sync.Mutex
	log           map[int32][]Record
	applied       map[batchKey]int64
	committedSeqs []int64
	duplicateHits int
	dropAcks      int
	transient     error
}

// NewDedupBroker builds a DedupBroker that drops the next dropAcks acks.
func NewDedupBroker(dropAcks int) *DedupBroker {
	return &DedupBroker{
		log:       make(map[int32][]Record),
		applied:   make(map[batchKey]int64),
		dropAcks:  dropAcks,
		transient: errors.New("idempotent: ack lost in transit"),
	}
}

func (b *DedupBroker) Send(batch *RecordBatch) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := batchKey{pid: batch.ProducerID, partition: batch.TP.Partition, baseSeq: batch.BaseSeq}
	if off, ok := b.applied[key]; ok {
		// Already committed: this is a retry of a batch whose ack was lost.
		b.duplicateHits++
		return off, nil
	}

	baseOffset := int64(len(b.log[batch.TP.Partition]))
	b.log[batch.TP.Partition] = append(b.log[batch.TP.Partition], batch.Records...)
	b.applied[key] = baseOffset
	b.committedSeqs = append(b.committedSeqs, batch.BaseSeq)

	if b.dropAcks > 0 {
		// Committed durably, but the acknowledgment is lost on the way back.
		b.dropAcks--
		return 0, b.transient
	}
	return baseOffset, nil
}

// LogLen returns the number of committed records on a partition.
func (b *DedupBroker) LogLen(partition int32) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.log[partition])
}

// DuplicateHits returns how many retries the dedup table absorbed.
func (b *DedupBroker) DuplicateHits() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.duplicateHits
}

// CommittedBaseSeqs returns the BaseSeq of each batch committed once, in order.
func (b *DedupBroker) CommittedBaseSeqs() []int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]int64(nil), b.committedSeqs...)
}

// NaiveBroker has no dedup table: it appends every batch it receives, so a
// retried batch is committed a second time. It exists to show the bug.
type NaiveBroker struct {
	mu        sync.Mutex
	log       map[int32][]Record
	dropAcks  int
	transient error
}

// NewNaiveBroker builds a NaiveBroker that drops the next dropAcks acks.
func NewNaiveBroker(dropAcks int) *NaiveBroker {
	return &NaiveBroker{
		log:       make(map[int32][]Record),
		dropAcks:  dropAcks,
		transient: errors.New("idempotent: ack lost in transit"),
	}
}

func (b *NaiveBroker) Send(batch *RecordBatch) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	baseOffset := int64(len(b.log[batch.TP.Partition]))
	b.log[batch.TP.Partition] = append(b.log[batch.TP.Partition], batch.Records...)
	if b.dropAcks > 0 {
		b.dropAcks--
		return 0, b.transient
	}
	return baseOffset, nil
}

// LogLen returns the number of committed records on a partition.
func (b *NaiveBroker) LogLen(partition int32) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.log[partition])
}
```

### The runnable demo

The demo runs the identical scenario, three records with the first acknowledgment dropped, against both brokers. The dedup broker absorbs the retry and its log holds three records; the naive broker commits the batch twice and its log holds six. The contrast is the whole point: the producer code is the same, and only the broker's dedup table makes delivery exactly-once.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"

	"example.com/idempotent-producer"
)

func sendThree(p *idempotent.Producer) {
	for i := 0; i < 3; i++ {
		if err := p.Send("orders", 0, nil, []byte(fmt.Sprintf("order-%d", i))); err != nil {
			log.Fatal(err)
		}
	}
}

func main() {
	dedup := idempotent.NewDedupBroker(1) // drop the first ack
	p, err := idempotent.NewProducer(idempotent.Config{Retries: 3, RetryBackoffMs: 1}, dedup)
	if err != nil {
		log.Fatal(err)
	}
	sendThree(p)
	if err := p.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("=== dedup broker ===")
	fmt.Println("committed records:", dedup.LogLen(0))
	fmt.Println("duplicate retries absorbed:", dedup.DuplicateHits())
	fmt.Println("producer retries:", p.Metrics().Retries)

	naive := idempotent.NewNaiveBroker(1) // drop the first ack
	q, err := idempotent.NewProducer(idempotent.Config{Retries: 3, RetryBackoffMs: 1}, naive)
	if err != nil {
		log.Fatal(err)
	}
	sendThree(q)
	if err := q.Close(); err != nil {
		log.Fatal(err)
	}
	fmt.Println("=== naive broker ===")
	fmt.Println("committed records:", naive.LogLen(0))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
=== dedup broker ===
committed records: 3
duplicate retries absorbed: 1
producer retries: 1
=== naive broker ===
committed records: 6
```

### Tests

The tests pin the guarantee and its failure mode. `TestExactlyOnceUnderLostAck` drops one ack and asserts the dedup broker committed exactly three records, absorbed one duplicate, and the producer retried once. `TestNaiveBrokerDoubleCommits` runs the same scenario and asserts the naive log holds six, documenting the bug idempotence fixes. `TestStableBaseSeqAcrossRetries` and `TestIndependentPartitionSequences` pin that sequences are per-partition and frozen at enqueue. `TestConcurrentSends` stresses the accumulator under `-race`.

Create `idempotent_test.go`:

```go
package idempotent

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func sendN(t *testing.T, p *Producer, topic string, partition int32, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := p.Send(topic, partition, nil, []byte(fmt.Sprintf("v-%d", i))); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
}

func TestExactlyOnceUnderLostAck(t *testing.T) {
	t.Parallel()

	broker := NewDedupBroker(1) // drop the first ack
	p, _ := NewProducer(Config{Retries: 3, RetryBackoffMs: 1}, broker)

	sendN(t, p, "orders", 0, 3)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := broker.LogLen(0); got != 3 {
		t.Errorf("LogLen = %d, want 3 (exactly once)", got)
	}
	if got := broker.DuplicateHits(); got != 1 {
		t.Errorf("DuplicateHits = %d, want 1", got)
	}
	m := p.Metrics()
	if m.Retries != 1 || m.Records != 3 || m.BatchesCommitted != 1 {
		t.Errorf("metrics = %+v, want retries=1 records=3 committed=1", m)
	}
}

func TestNaiveBrokerDoubleCommits(t *testing.T) {
	t.Parallel()

	broker := NewNaiveBroker(1) // drop the first ack
	p, _ := NewProducer(Config{Retries: 3, RetryBackoffMs: 1}, broker)

	sendN(t, p, "orders", 0, 3)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := broker.LogLen(0); got != 6 {
		t.Errorf("LogLen = %d, want 6 (naive double-commit)", got)
	}
}

func TestStableBaseSeqAcrossRetries(t *testing.T) {
	t.Parallel()

	broker := NewDedupBroker(0)
	p, _ := NewProducer(Config{Retries: 3, RetryBackoffMs: 1}, broker)

	sendN(t, p, "orders", 0, 3)
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	sendN(t, p, "orders", 0, 2)
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}

	// First batch starts at seq 0, second at seq 3 (per-record increment).
	if got := broker.CommittedBaseSeqs(); !reflect.DeepEqual(got, []int64{0, 3}) {
		t.Errorf("committed BaseSeqs = %v, want [0 3]", got)
	}
}

func TestIndependentPartitionSequences(t *testing.T) {
	t.Parallel()

	broker := NewDedupBroker(0)
	p, _ := NewProducer(Config{Retries: 1, RetryBackoffMs: 1}, broker)

	sendN(t, p, "orders", 0, 2)
	sendN(t, p, "orders", 1, 2)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	if got := broker.LogLen(0); got != 2 {
		t.Errorf("partition 0 LogLen = %d, want 2", got)
	}
	if got := broker.LogLen(1); got != 2 {
		t.Errorf("partition 1 LogLen = %d, want 2", got)
	}
}

func TestSendAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	p, _ := NewProducer(Config{Retries: 0}, NewDedupBroker(0))
	_ = p.Close()
	if err := p.Send("t", 0, nil, []byte("late")); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
}

func TestConcurrentSends(t *testing.T) {
	t.Parallel()

	broker := NewDedupBroker(0)
	p, _ := NewProducer(Config{Retries: 1, RetryBackoffMs: 1}, broker)

	const goroutines, perG = 16, 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if err := p.Send("events", int32(g%4), nil, []byte("x")); err != nil {
					t.Errorf("Send: %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	total := 0
	for part := int32(0); part < 4; part++ {
		total += broker.LogLen(part)
	}
	if total != goroutines*perG {
		t.Errorf("total committed = %d, want %d", total, goroutines*perG)
	}
}
```

## Review

The producer is correct when `BaseSeq` is assigned once and never moves. Confirm that `Send` increments the per-partition sequence at enqueue, that a batch captures `BaseSeq` at creation, and that `deliver` retries the same `*RecordBatch` so the key is identical on every attempt. Against the dedup broker a dropped ack yields exactly three committed records and one absorbed duplicate; against the naive broker the same run commits six, which is the precise bug the dedup key removes. The stable-sequence test proves consecutive batches start at the right offsets, and the per-partition test proves the sequences do not bleed across partitions.

The mistakes to avoid. Incrementing the sequence on acknowledgment rather than on enqueue gives a retried batch a fresh key, the broker fails to recognize the duplicate, and exactly-once silently degrades to at-least-once; the increment belongs in `Send`. Building a new `RecordBatch` for a retry, instead of resending the same one, has the same effect, so `deliver` loops over the original batch. And assuming a transient error means the commit failed is the misconception the whole exercise corrects: a lost ack can follow a successful commit, so the producer must retry and the broker must dedup, because only the pair makes the ambiguous case safe.

## Resources

- [Kafka idempotent producer (KIP-98)](https://cwiki.apache.org/confluence/display/KAFKA/KIP-98+-+Exactly+Once+Delivery+and+Transactional+Messaging) — the design this exercise models: producer id, per-partition sequence numbers, and broker-side dedup.
- [`enable.idempotence`](https://kafka.apache.org/documentation/#producerconfigs_enable.idempotence) — the production switch and the guarantees it makes about duplicates and ordering.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, used to hand each producer a distinct id without a mutex.

---

Back to [00-concepts.md](00-concepts.md) | Next: [../05-consumer-api-backpressure/00-concepts.md](../05-consumer-api-backpressure/00-concepts.md)
