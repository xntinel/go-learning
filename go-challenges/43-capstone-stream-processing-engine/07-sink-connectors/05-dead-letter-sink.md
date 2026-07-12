# Exercise 5: Dead-Letter Sink with Poison-Pill Isolation

Retrying forever assumes every failure is transient. Some are not: a malformed record that the destination rejects every time is a poison pill, and a sink that retries it forever blocks the whole pipeline behind one bad record. This exercise builds a dead-letter sink that retries transient failures, then — when a batch keeps failing — bisects it to isolate the genuine poison records into a dead-letter destination, so every healthy record is still delivered.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
sink.go                Record, BatchWriter, DeadLetterSink, deliver, tryPrimary
cmd/
  demo/
    main.go            deliver a batch with one poison record to a primary + DLQ
sink_test.go           healthy delivery, poison isolation, transient retry, cancel
```

- Files: `sink.go`, `cmd/demo/main.go`, `sink_test.go`.
- Implement: `DeadLetterSink` with `Write`, and the internal `deliver` (recursive bisection) and `tryPrimary` (bounded retry) helpers, plus the `BatchWriter` interface and `Metrics`.
- Test: `sink_test.go` proves a healthy batch is delivered, a poison record is quarantined while its healthy neighbors still land, a transient failure is retried rather than dead-lettered, and cancellation aborts promptly.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/43-capstone-stream-processing-engine/07-sink-connectors/05-dead-letter-sink/cmd/demo && cd go-solutions/43-capstone-stream-processing-engine/07-sink-connectors/05-dead-letter-sink
go mod edit -go=1.26
```

### The destination as an interface, and the two failure regimes

The sink does not know or care whether the destination is a file, an HTTP endpoint, or a database. It talks to a `BatchWriter` — anything with a `WriteBatch(ctx, records) error` method — and a `BatchWriterFunc` adapter lets a plain function satisfy it. The `DeadLetterSink` holds two `BatchWriter`s: a `Primary` and a `DeadLetter`. This indirection is what makes the sink testable with an in-memory destination and reusable across real ones.

The design hinges on distinguishing two failure regimes. A *transient* failure — the server is briefly overloaded, the connection blipped — clears on retry, so the right response is bounded exponential backoff. A *permanent* failure — the record is malformed and will be rejected forever — never clears, so retrying is futile and the right response is to quarantine the record and move on. The sink cannot tell the two apart from a single error, so it treats failures as transient up to `MaxRetries` and only after exhausting them concludes the record is poison.

Create `sink.go`:

```go
// Package sink provides a dead-letter sink: a batching, retrying connector that
// quarantines permanently-failing records to a dead-letter destination instead
// of stalling the whole pipeline.
package sink

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNilTarget is returned when the primary or dead-letter target is nil.
var ErrNilTarget = errors.New("sink: target must not be nil")

// Record is the unit of data flowing through the pipeline.
type Record struct {
	Key   []byte
	Value []byte
}

// BatchWriter is a destination that accepts a batch of records and either
// commits all of them or returns an error. A returned error is treated as a
// failure of the whole batch; the dead-letter sink decides whether to retry,
// split, or quarantine.
type BatchWriter interface {
	WriteBatch(ctx context.Context, records []Record) error
}

// BatchWriterFunc adapts a function to the BatchWriter interface.
type BatchWriterFunc func(ctx context.Context, records []Record) error

// WriteBatch calls f.
func (f BatchWriterFunc) WriteBatch(ctx context.Context, records []Record) error {
	return f(ctx, records)
}

// Metrics counts the outcomes of delivery.
type Metrics struct {
	Delivered    atomic.Int64 // records the primary target accepted
	DeadLettered atomic.Int64 // records quarantined after exhausting retries
	Retries      atomic.Int64 // retry attempts against the primary target
	Splits       atomic.Int64 // batch bisections during poison isolation
}

// Config configures a DeadLetterSink.
type Config struct {
	// Primary is the main destination. Required.
	Primary BatchWriter
	// DeadLetter receives records the primary permanently rejects. Required.
	DeadLetter BatchWriter
	// MaxRetries is the number of retry attempts for a failing batch before the
	// sink begins isolating the poison record(s). Defaults to 3.
	MaxRetries int
	// RetryBackoff is the initial backoff; it doubles each attempt, capped at
	// 30s. Defaults to 50ms.
	RetryBackoff time.Duration
}

// DeadLetterSink wraps a primary BatchWriter with retry and poison-pill
// isolation. A batch that keeps failing is bisected until the failing
// record(s) are isolated to size-one batches, which are routed to the
// dead-letter target. Healthy records in the same original batch are still
// delivered. The pipeline therefore never blocks on a single poison record.
type DeadLetterSink struct {
	cfg     Config
	mu      sync.Mutex
	metrics Metrics
}

// New constructs a DeadLetterSink, applying defaults for unset fields.
func New(cfg Config) (*DeadLetterSink, error) {
	if cfg.Primary == nil || cfg.DeadLetter == nil {
		return nil, ErrNilTarget
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 50 * time.Millisecond
	}
	return &DeadLetterSink{cfg: cfg}, nil
}

// Metrics returns the live outcome counters.
func (s *DeadLetterSink) Metrics() *Metrics { return &s.metrics }

// Write delivers a batch with at-least-once semantics for healthy records and
// quarantine for poison records. It is safe for concurrent use.
func (s *DeadLetterSink) Write(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliver(ctx, records)
}

// deliver tries the primary target with retries. On exhaustion it bisects the
// batch and recurses, isolating the poison record(s) into the dead-letter
// target. Callers must hold s.mu.
func (s *DeadLetterSink) deliver(ctx context.Context, batch []Record) error {
	if err := s.tryPrimary(ctx, batch); err == nil {
		s.metrics.Delivered.Add(int64(len(batch)))
		return nil
	} else if ctx.Err() != nil {
		return ctx.Err()
	}

	if len(batch) == 1 {
		// A single record that will not commit: quarantine it.
		if err := s.cfg.DeadLetter.WriteBatch(ctx, batch); err != nil {
			return err
		}
		s.metrics.DeadLettered.Add(1)
		return nil
	}

	// Bisect to isolate the poison record(s); healthy halves still go through.
	s.metrics.Splits.Add(1)
	mid := len(batch) / 2
	if err := s.deliver(ctx, batch[:mid]); err != nil {
		return err
	}
	return s.deliver(ctx, batch[mid:])
}

// tryPrimary attempts the primary write up to MaxRetries times with cancellable
// exponential backoff. Callers must hold s.mu.
func (s *DeadLetterSink) tryPrimary(ctx context.Context, batch []Record) error {
	backoff := s.cfg.RetryBackoff
	var err error
	for attempt := 0; attempt <= s.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			s.metrics.Retries.Add(1)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff = capDuration(backoff*2, 30*time.Second)
		}
		if err = s.cfg.Primary.WriteBatch(ctx, batch); err == nil {
			return nil
		}
	}
	return err
}

func capDuration(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}
```

### Binary-split poison isolation

The subtle part is what happens when a *batch* fails permanently. Many destinations reject an entire batch if any single record in it is invalid — a malformed JSON record fails the whole POST. Dead-lettering the entire failed batch would quarantine every healthy record alongside the one poison record, which is unacceptable: a single bad record would silently drop hundreds of good ones.

`deliver` solves this with recursive bisection. It first tries the primary with retries via `tryPrimary`. On success the whole batch is delivered. On permanent failure it checks the batch size: a size-one batch that still fails is the isolated poison record, so it goes to the dead-letter destination and `deliver` returns. A larger failing batch is split in half and each half is delivered recursively. The healthy half succeeds on its first `tryPrimary`; the poisoned half bisects again, and the recursion narrows until each poison record is alone in a size-one batch and dead-lettered individually. A batch of five with one poison record thus costs a handful of extra primary calls but delivers all four healthy records and quarantines exactly the one bad one. The `Splits` metric counts the bisections so you can see the isolation happening.

Context cancellation short-circuits the whole thing: `deliver` checks `ctx.Err()` right after a failed `tryPrimary`, and `tryPrimary`'s backoff wait selects on `ctx.Done()`, so a shutdown does not get stuck splitting a doomed batch.

Because both the retry path and the dead-letter path can re-deliver a record (a retry whose original actually landed, a record that succeeds on the primary after being counted), a dead-letter sink composes naturally with an idempotent downstream like the upsert sink from the previous exercise — the idempotent write absorbs the duplicates this sink can produce.

### The runnable demo

The demo delivers a five-record batch in which `order:bad` is poison. The primary (an in-memory store that rejects any batch containing a poison key) accepts the four healthy records across the bisected sub-batches, and `order:bad` lands in the dead-letter queue. Two splits happen: the original five-record batch, then the three-record half that still contains the poison.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"example.com/dead-letter-sink"
)

// store is an in-memory BatchWriter that rejects any batch containing a key
// listed in poison, modelling a destination that 4xx-rejects invalid records.
type store struct {
	mu       sync.Mutex
	poison   map[string]bool
	accepted []sink.Record
}

func (s *store) WriteBatch(_ context.Context, records []sink.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		if s.poison[string(r.Key)] {
			return fmt.Errorf("invalid record %q", r.Key)
		}
	}
	s.accepted = append(s.accepted, records...)
	return nil
}

type dlq struct {
	mu      sync.Mutex
	records []sink.Record
}

func (d *dlq) WriteBatch(_ context.Context, records []sink.Record) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.records = append(d.records, records...)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func run() error {
	primary := &store{poison: map[string]bool{"order:bad": true}}
	dead := &dlq{}
	s, err := sink.New(sink.Config{Primary: primary, DeadLetter: dead, MaxRetries: 2})
	if err != nil {
		return err
	}

	batch := []sink.Record{
		{Key: []byte("order:1")},
		{Key: []byte("order:2")},
		{Key: []byte("order:bad")}, // poison: malformed payload
		{Key: []byte("order:3")},
		{Key: []byte("order:4")},
	}
	if err := s.Write(context.Background(), batch); err != nil {
		return err
	}

	m := s.Metrics()
	fmt.Printf("delivered=%d dead-lettered=%d splits=%d\n",
		m.Delivered.Load(), m.DeadLettered.Load(), m.Splits.Load())
	fmt.Printf("primary accepted %d records, dlq holds %d\n",
		len(primary.accepted), len(dead.records))
	fmt.Printf("quarantined: %s\n", dead.records[0].Key)
	return nil
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
delivered=4 dead-lettered=1 splits=2
primary accepted 4 records, dlq holds 1
quarantined: order:bad
```

### Tests

`TestHealthyBatchDelivered` confirms a clean batch goes straight through with nothing dead-lettered. `TestPoisonRecordQuarantined` is the core property: a five-record batch with one poison record delivers all four healthy records and quarantines only the poison one, with `Splits` non-zero. `TestTransientFailureRetriedThenDelivered` proves a flaky primary that fails twice then succeeds is retried, not dead-lettered. `TestContextCancellationAborts` proves an always-failing primary with an hour-long backoff aborts promptly when the context is cancelled. `TestConcurrentWritesIsolatePoison` runs eight concurrent writers, each batch carrying the same shared poison key, and checks the totals under `-race`.

Create `sink_test.go`:

```go
package sink

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// collector is a BatchWriter that records every batch it accepts.
type collector struct {
	mu      sync.Mutex
	records []Record
}

func (c *collector) WriteBatch(_ context.Context, records []Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, records...)
	return nil
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.records)
}

func (c *collector) keys() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]bool, len(c.records))
	for _, r := range c.records {
		out[string(r.Key)] = true
	}
	return out
}

// poisonPrimary accepts a batch only if it contains no poison key. This models
// a destination that rejects a whole batch when one record is invalid, which is
// exactly the situation poison-pill isolation exists to handle.
type poisonPrimary struct {
	mu       sync.Mutex
	poison   map[string]bool
	accepted []Record
}

func (p *poisonPrimary) WriteBatch(_ context.Context, records []Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range records {
		if p.poison[string(r.Key)] {
			return fmt.Errorf("rejected batch containing poison key %q", r.Key)
		}
	}
	p.accepted = append(p.accepted, records...)
	return nil
}

func (p *poisonPrimary) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accepted)
}

func TestRejectsNilTarget(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{DeadLetter: &collector{}}); !errors.Is(err, ErrNilTarget) {
		t.Fatalf("err = %v, want ErrNilTarget", err)
	}
}

func TestHealthyBatchDelivered(t *testing.T) {
	t.Parallel()

	primary := &poisonPrimary{poison: map[string]bool{}}
	dlq := &collector{}
	s, err := New(Config{Primary: primary, DeadLetter: dlq, RetryBackoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	batch := []Record{{Key: []byte("a")}, {Key: []byte("b")}, {Key: []byte("c")}}
	if err := s.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if primary.count() != 3 {
		t.Fatalf("primary got %d records, want 3", primary.count())
	}
	if dlq.count() != 0 {
		t.Fatalf("dlq got %d records, want 0", dlq.count())
	}
	if got := s.metrics.Delivered.Load(); got != 3 {
		t.Fatalf("Delivered = %d, want 3", got)
	}
}

// TestPoisonRecordQuarantined is the core property: a batch with one poison
// record among many still delivers every healthy record and quarantines only
// the poison one.
func TestPoisonRecordQuarantined(t *testing.T) {
	t.Parallel()

	primary := &poisonPrimary{poison: map[string]bool{"bad": true}}
	dlq := &collector{}
	s, err := New(Config{
		Primary: primary, DeadLetter: dlq,
		MaxRetries: 2, RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	batch := []Record{
		{Key: []byte("a")}, {Key: []byte("b")}, {Key: []byte("bad")},
		{Key: []byte("c")}, {Key: []byte("d")},
	}
	if err := s.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}

	if primary.count() != 4 {
		t.Fatalf("primary got %d healthy records, want 4", primary.count())
	}
	if dlq.count() != 1 {
		t.Fatalf("dlq got %d records, want 1", dlq.count())
	}
	if !dlq.keys()["bad"] {
		t.Fatalf("dlq must contain the poison key, got %v", dlq.keys())
	}
	if got := s.metrics.DeadLettered.Load(); got != 1 {
		t.Fatalf("DeadLettered = %d, want 1", got)
	}
	if got := s.metrics.Splits.Load(); got == 0 {
		t.Fatal("Splits should be non-zero: the batch had to be bisected")
	}
}

// TestTransientFailureRetriedThenDelivered verifies a flaky primary that fails
// a fixed number of times before succeeding is retried, not dead-lettered.
func TestTransientFailureRetriedThenDelivered(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	fails := 2
	delivered := 0
	primary := BatchWriterFunc(func(_ context.Context, records []Record) error {
		mu.Lock()
		defer mu.Unlock()
		if fails > 0 {
			fails--
			return errors.New("transient")
		}
		delivered += len(records)
		return nil
	})
	dlq := &collector{}
	s, err := New(Config{
		Primary: primary, DeadLetter: dlq,
		MaxRetries: 5, RetryBackoff: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), []Record{{Key: []byte("a")}}); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("delivered = %d, want 1", delivered)
	}
	if dlq.count() != 0 {
		t.Fatalf("dlq got %d, want 0 (transient failure must not dead-letter)", dlq.count())
	}
	if got := s.metrics.Retries.Load(); got < 2 {
		t.Fatalf("Retries = %d, want >= 2", got)
	}
}

// TestContextCancellationAborts verifies a cancelled context stops the retry
// loop promptly rather than draining every backoff.
func TestContextCancellationAborts(t *testing.T) {
	t.Parallel()

	primary := BatchWriterFunc(func(_ context.Context, _ []Record) error {
		return errors.New("always fails")
	})
	dlq := &collector{}
	s, _ := New(Config{
		Primary: primary, DeadLetter: dlq,
		MaxRetries: 1000, RetryBackoff: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := s.Write(ctx, []Record{{Key: []byte("a")}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestConcurrentWritesIsolatePoison runs many concurrent Writes, each batch
// carrying one shared poison key, and asserts the totals add up with no races.
func TestConcurrentWritesIsolatePoison(t *testing.T) {
	t.Parallel()

	primary := &poisonPrimary{poison: map[string]bool{"bad": true}}
	dlq := &collector{}
	s, _ := New(Config{
		Primary: primary, DeadLetter: dlq,
		MaxRetries: 1, RetryBackoff: time.Millisecond,
	})

	const writers = 8
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			batch := []Record{
				{Key: []byte(fmt.Sprintf("g%d-a", w))},
				{Key: []byte("bad")},
				{Key: []byte(fmt.Sprintf("g%d-b", w))},
			}
			if err := s.Write(context.Background(), batch); err != nil {
				t.Errorf("Write: %v", err)
			}
		}(w)
	}
	wg.Wait()

	if got := primary.count(); got != writers*2 {
		t.Fatalf("primary delivered %d healthy records, want %d", got, writers*2)
	}
	if got := dlq.count(); got != writers {
		t.Fatalf("dlq quarantined %d records, want %d", got, writers)
	}
}
```

## Review

The sink is correct when a poison record costs exactly one quarantined record and never a healthy neighbor. Confirm `deliver` only dead-letters at batch size one — dead-lettering a larger failing batch is the bug poison isolation exists to prevent — and that it bisects otherwise so healthy halves still reach the primary. Confirm transient failures are retried with bounded backoff before the batch is ever considered poison, and that cancellation short-circuits both the retry wait and the recursion. The classic mistakes are quarantining a whole batch for one bad record, treating the first failure as permanent (so a transient blip dead-letters good data), and forgetting that retries and dead-lettering can both duplicate — which is why this sink belongs in front of an idempotent destination. The suite passing repeatedly under `go test -race ./...` establishes these properties.

## Resources

- [Kafka Connect: Dead Letter Queues](https://docs.confluent.io/platform/current/connect/concepts.html#dead-letter-queue) — the production DLQ pattern this exercise models.
- [AWS SQS dead-letter queues](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html) — quarantine-after-N-failures semantics in a managed queue.
- [Exponential Backoff And Jitter (AWS Builders' Library)](https://aws.amazon.com/builders-library/timeouts-retries-and-backoff-with-jitter/) — bounding retries so a permanent failure does not loop forever.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-idempotent-upsert-sink.md](04-idempotent-upsert-sink.md) | Next: [06-transactional-coordinator-sink.md](06-transactional-coordinator-sink.md)
