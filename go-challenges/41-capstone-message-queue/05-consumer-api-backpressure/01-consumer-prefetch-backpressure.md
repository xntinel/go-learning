# Exercise 1: Prefetch Consumer with Backpressure

The prefetch consumer is the spine of the consumer side of a message queue: a background goroutine per partition continuously fills a bounded channel, the application's `Poll` drains it, and the capacity of that channel is the entire backpressure mechanism. This module builds the consumer end to end against an in-memory broker, with offset management, `Seek`, two delivery modes, auto-commit, and lag reporting, so the whole thing runs offline.

This module is fully self-contained. It begins with its own `go mod init`, defines the `Broker` interface and an in-memory implementation, and ships its own demo, table tests, and runnable example. Nothing here imports any other exercise.

## What you'll build

```text
broker.go            Broker interface + InMemoryBroker (in-memory partition log)
consumer.go          Consumer: New, Poll, Seek, CommitSync/Async, Pause/Resume, Lag, Close
cmd/
  demo/
    main.go          consume five orders, commit, report lag, seek to beginning
consumer_test.go     backpressure, pause, seek, delivery-mode, auto-commit tests
example_test.go      ExampleNew (external consumer_test package)
```

- Files: `broker.go`, `consumer.go`, `cmd/demo/main.go`, `consumer_test.go`, `example_test.go`.
- Implement: the `Broker` interface plus `InMemoryBroker`, and the `Consumer` with `Poll`, `Seek`/`SeekToBeginning`/`SeekToEnd`, `CommitSync`/`CommitAsync`, `Pause`/`Resume`, `Lag`, and `Close`.
- Test: backpressure bounds the fetch position, `Pause` halts the fetcher, `Seek` repositions safely, delivery modes commit at the right time, and auto-commit flushes on a tick and on `Close`.
- Verify: `go test -race ./... && go run ./cmd/demo`

Set up the module:

```bash
mkdir -p go-solutions/41-capstone-message-queue/05-consumer-api-backpressure/01-consumer-prefetch-backpressure/cmd/demo && cd go-solutions/41-capstone-message-queue/05-consumer-api-backpressure/01-consumer-prefetch-backpressure
```

### The Broker interface: a seam for the network

The `Consumer` depends on the message store through one small interface, `Broker`. In production a `Broker` wraps a TCP connection to the server; in this module `InMemoryBroker` satisfies it with a plain in-memory log, which is what lets the entire consumer be exercised under the race detector with no sockets and no goroutine timing tricks. The interface has exactly four methods: `Fetch` (block until at least one record is available at an offset, then return up to `maxCount` of them), `LatestOffset` (the next offset to be written, used for lag and seek-to-end), `CommitOffset` (durably store the next-to-fetch offset), and `CommittedOffset` (read it back, or `-1` if nothing was ever committed).

The one subtlety that makes the whole design work is in `InMemoryBroker.Fetch`: it polls its in-memory slice every 5 ms inside a `select` that also watches `ctx.Done()`. That is what lets the consumer wrap each fetch in a `context.WithTimeout` and have the call actually return when the timeout fires, rather than blocking forever on an empty partition. A real network broker would block on a socket read with a deadline; the in-memory poll loop is the offline stand-in for that deadline.

Create `broker.go`:

```go
package consumer

import (
	"context"
	"sync"
	"time"
)

// Broker is the minimal interface the Consumer requires from the underlying
// message store. In production a Broker wraps a TCP connection to the server.
// In tests InMemoryBroker is used.
type Broker interface {
	// Fetch returns up to maxCount records starting at offset in partition.
	// It blocks until at least one record is available or ctx is cancelled.
	Fetch(ctx context.Context, partition int, offset int64, maxCount int) ([]*Record, error)

	// LatestOffset returns the offset of the next record to be written to
	// partition (current log length).
	LatestOffset(partition int) (int64, error)

	// CommitOffset durably stores offset as the committed position for
	// partition. The offset is the next record to fetch after a restart (i.e.
	// last processed offset + 1).
	CommitOffset(partition int, offset int64) error

	// CommittedOffset returns the last durably committed offset for partition,
	// or -1 if no offset has been committed yet.
	CommittedOffset(partition int) (int64, error)
}

// InMemoryBroker is a thread-safe, in-memory Broker intended for tests and
// demos. All records are held in memory with no persistence across process
// restarts.
type InMemoryBroker struct {
	mu        sync.RWMutex
	records   map[int][]*Record // keyed by partition
	committed map[int]int64     // keyed by partition
}

// NewInMemoryBroker creates an InMemoryBroker with no pre-loaded partitions.
// Partitions are created automatically when the first record is appended.
func NewInMemoryBroker() *InMemoryBroker {
	return &InMemoryBroker{
		records:   make(map[int][]*Record),
		committed: make(map[int]int64),
	}
}

// Append adds a record to partition. It sets r.Offset to the current log
// length before appending, so records are offset-indexed from 0.
func (b *InMemoryBroker) Append(partition int, key, value []byte) *Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := &Record{
		Partition: partition,
		Offset:    int64(len(b.records[partition])),
		Key:       key,
		Value:     value,
		Timestamp: time.Now().UTC(),
	}
	b.records[partition] = append(b.records[partition], r)
	return r
}

// Fetch implements Broker. It polls the in-memory log every 5 ms until at
// least one record is available at offset or ctx is cancelled.
func (b *InMemoryBroker) Fetch(ctx context.Context, partition int, offset int64, maxCount int) ([]*Record, error) {
	for {
		b.mu.RLock()
		recs := b.records[partition]
		b.mu.RUnlock()

		if int64(len(recs)) > offset {
			end := int(offset) + maxCount
			if end > len(recs) {
				end = len(recs)
			}
			return recs[int(offset):end], nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// LatestOffset implements Broker.
func (b *InMemoryBroker) LatestOffset(partition int) (int64, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return int64(len(b.records[partition])), nil
}

// CommitOffset implements Broker.
func (b *InMemoryBroker) CommitOffset(partition int, offset int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.committed[partition] = offset
	return nil
}

// CommittedOffset implements Broker.
func (b *InMemoryBroker) CommittedOffset(partition int) (int64, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	off, ok := b.committed[partition]
	if !ok {
		return -1, nil
	}
	return off, nil
}
```

### The Consumer: prefetch buffers, backpressure, and epoch-safe seek

`New` loads each partition's committed offset to choose a starting position (a partition with no committed offset starts at 0), allocates one `partitionBuffer` per partition, and launches one `fetchLoop` goroutine per partition plus an optional auto-commit goroutine. A `partitionBuffer` is the prefetch channel together with two pieces of lock-free control state: an `atomic.Bool` pause flag and an `atomic.Int64` epoch counter.

`fetchLoop` is the heart of the design and three mechanisms live inside it. First, **backpressure**: each record is handed to the buffer with `case buf.ch <- r:`, and the fetch position is advanced only *inside* that successful-send branch, so a record that never makes it into a full buffer never advances the position and can never be silently skipped. When the buffer is full the send simply blocks and the goroutine stalls until `Poll` frees a slot. Second, the **bounded fetch context**: every `Broker.Fetch` runs under `context.WithTimeout(c.ctx, fetchMaxWait)`, so an empty partition returns `context.DeadlineExceeded` after at most 100 ms and the loop restarts, re-reading the pause flag and the position. That is what makes `Pause` and `Seek` take effect promptly even when no records are flowing. Third, **epoch safety**: the loop snapshots `buf.epoch` right after reading the position, and before every send it checks whether the epoch still matches; a `Seek` bumps the epoch, so an in-flight batch fetched at the old position is discarded rather than delivered out of order.

`Poll` drains the buffers round-robin with a non-blocking `select` per partition, accumulating up to `MaxPollRecords` and returning early on its timeout. After collecting a batch it records the highest `offset + 1` per partition in `pendingCommit` -- never the fetch position, which is ahead of what the caller has seen. In `AtMostOnce` mode it commits that progress to the broker *before* returning, so a caller crash loses records but never reprocesses them; in the default `AtLeastOnce` mode it only tracks the progress, leaving the durable commit to `CommitSync`, `CommitAsync`, or the auto-commit loop. `Seek` writes the new position under the mutex, bumps the epoch so the fetcher drops any stale in-flight batch, then drains whatever was already buffered. `Close` cancels the context, waits up to five seconds for every goroutine to exit, and flushes pending auto-commit so a clean shutdown does not lose tracked progress.

Create `consumer.go`:

```go
package consumer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors returned by the Consumer API.
var (
	ErrClosed           = errors.New("consumer: closed")
	ErrUnknownPartition = errors.New("consumer: unknown partition")
	ErrSeekOutOfRange   = errors.New("consumer: offset out of range")
)

// Record is a single message delivered to the application by Poll.
type Record struct {
	Topic     string
	Partition int
	Offset    int64
	Key       []byte
	Value     []byte
	Timestamp time.Time
}

// DeliveryMode controls when offsets are committed relative to processing.
type DeliveryMode int

const (
	// AtLeastOnce commits offsets after Poll returns records to the caller.
	// If the caller crashes between receiving and processing, those records are
	// re-delivered on the next consumer start. Use for workloads where
	// duplicate delivery is tolerable but message loss is not.
	AtLeastOnce DeliveryMode = iota

	// AtMostOnce commits offsets before returning records to the caller.
	// If the caller crashes after receiving but before processing, those
	// records are never re-delivered. Use when duplicate delivery is more
	// harmful than message loss.
	AtMostOnce
)

// Config holds all Consumer configuration.
type Config struct {
	// MaxPollRecords is the maximum number of records returned by a single
	// Poll call. Default: 500.
	MaxPollRecords int

	// FetchBufferSize is the capacity of the per-partition prefetch buffer.
	// When the buffer is at capacity the background fetch goroutine blocks on
	// its channel send, which is the backpressure mechanism.
	// Default: 1000.
	FetchBufferSize int

	// AutoCommit enables automatic offset commits on a fixed interval.
	AutoCommit bool

	// AutoCommitInterval is the interval between auto-commits when AutoCommit
	// is true. Default: 5 seconds.
	AutoCommitInterval time.Duration

	// Mode is the delivery guarantee mode.
	Mode DeliveryMode
}

// partitionBuffer is the prefetch buffer and control state for one partition.
type partitionBuffer struct {
	ch    chan *Record
	pause atomic.Bool  // true -> fetchLoop idles instead of fetching
	epoch atomic.Int64 // bumped by Seek; fetchLoop discards stale batches
}

// Consumer fetches records from assigned partitions using a background
// goroutine per partition that continuously fills a bounded prefetch buffer.
// Poll drains from those buffers. When a buffer is full the goroutine blocks
// on its channel send -- that is the backpressure mechanism; no explicit flow
// control is needed.
type Consumer struct {
	broker     Broker
	cfg        Config
	topic      string
	partitions []int

	mu        sync.RWMutex
	positions map[int]int64 // next offset to fetch per partition
	buffers   map[int]*partitionBuffer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// pendingCommit holds the highest (offset+1) returned by Poll per
	// partition, used by auto-commit and flushPendingCommit.
	commitMu      sync.Mutex
	pendingCommit map[int]int64
}

// New creates a Consumer that reads topic from the given partitions. Committed
// offsets are loaded from the broker to set the starting fetch position of
// each partition; a partition with no committed offset starts at 0.
func New(broker Broker, topic string, partitions []int, cfg Config) (*Consumer, error) {
	if broker == nil {
		return nil, errors.New("consumer: broker must not be nil")
	}
	if topic == "" {
		return nil, errors.New("consumer: topic must not be empty")
	}
	if len(partitions) == 0 {
		return nil, errors.New("consumer: at least one partition is required")
	}
	if cfg.MaxPollRecords <= 0 {
		cfg.MaxPollRecords = 500
	}
	if cfg.FetchBufferSize <= 0 {
		cfg.FetchBufferSize = 1000
	}
	if cfg.AutoCommitInterval <= 0 {
		cfg.AutoCommitInterval = 5 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Consumer{
		broker:        broker,
		cfg:           cfg,
		topic:         topic,
		partitions:    partitions,
		positions:     make(map[int]int64, len(partitions)),
		buffers:       make(map[int]*partitionBuffer, len(partitions)),
		pendingCommit: make(map[int]int64, len(partitions)),
		ctx:           ctx,
		cancel:        cancel,
	}

	for _, p := range partitions {
		committed, err := broker.CommittedOffset(p)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("consumer: load committed offset for partition %d: %w", p, err)
		}
		start := int64(0)
		if committed >= 0 {
			start = committed
		}
		c.positions[p] = start
		c.buffers[p] = &partitionBuffer{ch: make(chan *Record, cfg.FetchBufferSize)}
	}

	for _, p := range partitions {
		c.wg.Add(1)
		go c.fetchLoop(p)
	}
	if cfg.AutoCommit {
		c.wg.Add(1)
		go c.autoCommitLoop()
	}

	return c, nil
}

// fetchLoop continuously fills the prefetch buffer for partition p.
//
// Backpressure: when buf.ch is at capacity, the channel send blocks. The
// goroutine stalls until Poll drains a slot -- no additional signaling needed.
//
// Epoch safety: Seek bumps buf.epoch. If epoch changes between when we read
// the fetch position and when we attempt to buffer a record, we discard the
// entire batch and re-read the updated position on the next iteration.
//
// Bounded fetch context: each Broker.Fetch call uses a context that times out
// after fetchMaxWait. This ensures the loop re-checks the pause flag and the
// current fetch position at least that often, so Seek and Pause take effect
// within fetchMaxWait even when the broker has no records to deliver.
func (c *Consumer) fetchLoop(p int) {
	defer c.wg.Done()
	buf := c.buffers[p]
	const (
		batchSize    = 10
		fetchMaxWait = 100 * time.Millisecond
	)

	for {
		// Honour pause: yield the goroutine instead of fetching.
		if buf.pause.Load() {
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
				continue
			}
		}

		c.mu.RLock()
		offset := c.positions[p]
		c.mu.RUnlock()

		epochBefore := buf.epoch.Load()

		// A per-fetch context bounds how long we wait for records. When
		// the timeout fires the loop restarts and re-reads position and
		// pause, picking up any Seek or Pause that arrived mid-fetch.
		fetchCtx, fetchCancel := context.WithTimeout(c.ctx, fetchMaxWait)
		records, err := c.broker.Fetch(fetchCtx, p, offset, batchSize)
		fetchCancel()

		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				continue // timeout: re-check pause and position
			}
			if errors.Is(err, context.Canceled) {
				return // consumer closed
			}
			// Transient broker error: back off and retry.
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}

		for _, r := range records {
			// Seek was called while we were fetching; discard stale batch.
			if buf.epoch.Load() != epochBefore {
				break
			}
			select {
			case buf.ch <- r:
				// Advance position only after the record is safely in the buffer.
				c.mu.Lock()
				if r.Offset+1 > c.positions[p] {
					c.positions[p] = r.Offset + 1
				}
				c.mu.Unlock()
			case <-c.ctx.Done():
				return
			}
		}
	}
}

// autoCommitLoop periodically commits pendingCommit offsets.
func (c *Consumer) autoCommitLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.AutoCommitInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.flushPendingCommit()
		}
	}
}

// flushPendingCommit commits all tracked pending offsets to the broker.
func (c *Consumer) flushPendingCommit() {
	c.commitMu.Lock()
	toCommit := make(map[int]int64, len(c.pendingCommit))
	for p, off := range c.pendingCommit {
		toCommit[p] = off
	}
	c.commitMu.Unlock()

	for p, off := range toCommit {
		_ = c.broker.CommitOffset(p, off)
	}
}

// Poll blocks for up to timeout and returns the next batch of records (up to
// cfg.MaxPollRecords). It returns nil without error when the timeout elapses
// before any records become available.
//
// Delivery-mode semantics:
//   - AtLeastOnce (default): offsets are tracked but not committed here;
//     commit them later with CommitSync or AutoCommit.
//   - AtMostOnce: offsets are committed to the broker before returning, so
//     the caller can never reprocess these records even if it crashes.
func (c *Consumer) Poll(timeout time.Duration) ([]*Record, error) {
	select {
	case <-c.ctx.Done():
		return nil, ErrClosed
	default:
	}

	deadline := time.Now().Add(timeout)
	var out []*Record

	for len(out) < c.cfg.MaxPollRecords {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}

		// Non-blocking drain from every partition buffer (round-robin).
		got := false
		for _, p := range c.partitions {
			if len(out) >= c.cfg.MaxPollRecords {
				break
			}
			select {
			case r := <-c.buffers[p].ch:
				out = append(out, r)
				got = true
			default:
			}
		}
		if got {
			continue
		}

		// Nothing available yet; yield for a short interval.
		wait := 5 * time.Millisecond
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-time.After(wait):
		case <-c.ctx.Done():
			if len(out) > 0 {
				break
			}
			return nil, ErrClosed
		}
	}

	if len(out) == 0 {
		return nil, nil
	}

	// Track the highest (offset+1) returned per partition for auto-commit
	// and CommitSync callers.
	c.commitMu.Lock()
	for _, r := range out {
		if r.Offset+1 > c.pendingCommit[r.Partition] {
			c.pendingCommit[r.Partition] = r.Offset + 1
		}
	}
	c.commitMu.Unlock()

	if c.cfg.Mode == AtMostOnce {
		// Commit before returning; a caller crash after this point loses
		// records but never reprocesses them.
		c.commitMu.Lock()
		toCommit := make(map[int]int64, len(c.pendingCommit))
		for p, off := range c.pendingCommit {
			toCommit[p] = off
		}
		c.commitMu.Unlock()
		for p, off := range toCommit {
			if err := c.broker.CommitOffset(p, off); err != nil {
				return nil, fmt.Errorf("consumer: at-most-once pre-commit partition %d: %w", p, err)
			}
		}
	}

	return out, nil
}

// CommitSync durably commits offsets and blocks until the broker acknowledges.
// offsets maps partition -> next-to-fetch offset (last processed offset + 1).
func (c *Consumer) CommitSync(offsets map[int]int64) error {
	select {
	case <-c.ctx.Done():
		return ErrClosed
	default:
	}
	for p, off := range offsets {
		if err := c.broker.CommitOffset(p, off); err != nil {
			return fmt.Errorf("consumer: CommitSync partition %d: %w", p, err)
		}
	}
	return nil
}

// CommitAsync commits offsets in a background goroutine and invokes cb with
// the result. cb may be nil.
func (c *Consumer) CommitAsync(offsets map[int]int64, cb func(error)) {
	go func() {
		err := c.CommitSync(offsets)
		if cb != nil {
			cb(err)
		}
	}()
}

// Committed returns the last durably committed offset for partition, or -1 if
// no offset has been committed yet.
func (c *Consumer) Committed(partition int) (int64, error) {
	if _, ok := c.buffers[partition]; !ok {
		return -1, fmt.Errorf("%w: %d", ErrUnknownPartition, partition)
	}
	return c.broker.CommittedOffset(partition)
}

// Position returns the next offset that will be fetched for partition.
// Position is always >= the committed offset and >= the position returned by
// any previous call (absent a Seek).
func (c *Consumer) Position(partition int) (int64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	off, ok := c.positions[partition]
	if !ok {
		return -1, fmt.Errorf("%w: %d", ErrUnknownPartition, partition)
	}
	return off, nil
}

// Seek sets the consumer's fetch position for partition to offset. The next
// Poll call returns records starting at or after offset. Stale records already
// in the prefetch buffer are drained. Seek does not commit offset; call
// CommitSync afterward if the new position should survive a restart.
func (c *Consumer) Seek(partition int, offset int64) error {
	if offset < 0 {
		return fmt.Errorf("%w: %d", ErrSeekOutOfRange, offset)
	}
	c.mu.Lock()
	if _, ok := c.positions[partition]; !ok {
		c.mu.Unlock()
		return fmt.Errorf("%w: %d", ErrUnknownPartition, partition)
	}
	c.positions[partition] = offset
	c.mu.Unlock()

	buf := c.buffers[partition]
	// Bump epoch so fetchLoop discards any batch it fetched before this Seek.
	buf.epoch.Add(1)
	// Drain records that were buffered before the Seek.
	for {
		select {
		case <-buf.ch:
		default:
			return nil
		}
	}
}

// SeekToBeginning seeks partitions to offset 0. The next Poll for those
// partitions will re-read from the earliest available record.
func (c *Consumer) SeekToBeginning(partitions ...int) error {
	for _, p := range partitions {
		if err := c.Seek(p, 0); err != nil {
			return err
		}
	}
	return nil
}

// SeekToEnd seeks partitions to the current latest offset so that only
// records written after this call are delivered.
func (c *Consumer) SeekToEnd(partitions ...int) error {
	for _, p := range partitions {
		latest, err := c.broker.LatestOffset(p)
		if err != nil {
			return fmt.Errorf("consumer: SeekToEnd partition %d: %w", p, err)
		}
		if err := c.Seek(p, latest); err != nil {
			return err
		}
	}
	return nil
}

// Pause halts background fetching for the given partitions without discarding
// already-buffered records. Poll continues to drain buffered records. The
// fetcher resumes when Resume is called.
func (c *Consumer) Pause(partitions ...int) error {
	for _, p := range partitions {
		buf, ok := c.buffers[p]
		if !ok {
			return fmt.Errorf("%w: %d", ErrUnknownPartition, p)
		}
		buf.pause.Store(true)
	}
	return nil
}

// Resume restarts background fetching for the given partitions.
func (c *Consumer) Resume(partitions ...int) error {
	for _, p := range partitions {
		buf, ok := c.buffers[p]
		if !ok {
			return fmt.Errorf("%w: %d", ErrUnknownPartition, p)
		}
		buf.pause.Store(false)
	}
	return nil
}

// Lag returns the approximate number of unprocessed records per partition:
// broker latest offset - consumer position. A negative value indicates the
// consumer's position exceeds the broker's latest offset, which should not
// occur under normal operation.
func (c *Consumer) Lag() (map[int]int64, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make(map[int]int64, len(c.partitions))
	for _, p := range c.partitions {
		latest, err := c.broker.LatestOffset(p)
		if err != nil {
			return nil, fmt.Errorf("consumer: Lag partition %d: %w", p, err)
		}
		result[p] = latest - c.positions[p]
	}
	return result, nil
}

// Close stops all background goroutines and waits up to 5 seconds for them to
// exit. If AutoCommit is enabled, pending offsets are flushed synchronously
// after all goroutines stop.
func (c *Consumer) Close() error {
	c.cancel()

	done := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		return errors.New("consumer: Close timed out waiting for goroutines")
	}

	if c.cfg.AutoCommit {
		c.flushPendingCommit()
	}
	return nil
}
```

### The runnable demo

The demo seeds a partition with five orders, consumes them in batches of three (so the round-robin drain and `MaxPollRecords` cap are both visible), commits the processed offset by hand in the at-least-once style, reports the resulting lag, then seeks back to the beginning to show that a replay re-delivers offset 0.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"log"
	"time"

	"example.com/consumer"
)

func main() {
	broker := consumer.NewInMemoryBroker()

	// Seed partition 0 with five order messages.
	messages := []string{"order-001", "order-002", "order-003", "order-004", "order-005"}
	for _, msg := range messages {
		broker.Append(0, nil, []byte(msg))
	}

	c, err := consumer.New(broker, "orders", []int{0}, consumer.Config{
		MaxPollRecords:  3,
		FetchBufferSize: 10,
		Mode:            consumer.AtLeastOnce,
	})
	if err != nil {
		log.Fatalf("create consumer: %v", err)
	}
	defer func() {
		if err := c.Close(); err != nil {
			log.Printf("close: %v", err)
		}
	}()

	fmt.Println("--- consuming messages ---")
	var consumed []*consumer.Record
	deadline := time.Now().Add(5 * time.Second)
	for len(consumed) < len(messages) && time.Now().Before(deadline) {
		batch, err := c.Poll(500 * time.Millisecond)
		if err != nil {
			log.Fatalf("poll: %v", err)
		}
		for _, r := range batch {
			fmt.Printf("partition=%d offset=%d value=%s\n", r.Partition, r.Offset, r.Value)
		}
		consumed = append(consumed, batch...)
	}

	// Manual commit after processing all records (at-least-once pattern).
	if len(consumed) > 0 {
		last := consumed[len(consumed)-1]
		if err := c.CommitSync(map[int]int64{last.Partition: last.Offset + 1}); err != nil {
			log.Fatalf("commit: %v", err)
		}
		committed, _ := c.Committed(0)
		fmt.Printf("committed offset: %d\n", committed)
	}

	// Consumer lag after processing.
	lag, _ := c.Lag()
	for p, l := range lag {
		fmt.Printf("partition %d lag: %d\n", p, l)
	}

	// Seek to beginning to reprocess (useful for replay scenarios).
	fmt.Println("--- seeking to beginning ---")
	if err := c.SeekToBeginning(0); err != nil {
		log.Fatalf("seek: %v", err)
	}
	batch, err := c.Poll(500 * time.Millisecond)
	if err != nil {
		log.Fatalf("poll after seek: %v", err)
	}
	if len(batch) > 0 {
		fmt.Printf("first record after seek: offset=%d value=%s\n", batch[0].Offset, batch[0].Value)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
--- consuming messages ---
partition=0 offset=0 value=order-001
partition=0 offset=1 value=order-002
partition=0 offset=2 value=order-003
partition=0 offset=3 value=order-004
partition=0 offset=4 value=order-005
committed offset: 5
partition 0 lag: 0
--- seeking to beginning ---
first record after seek: offset=0 value=order-001
```

### Tests

The tests are same-package (`package consumer`) so they can read the unexported `positions` map through `Position` and assert on the fetcher's internal progress. `TestBackpressureBufferBlocks` is the load-bearing one: with a buffer of size 2 and five available records, the fetcher can advance the position to at most 2 (records 0 and 1 buffered) plus one batch-boundary slack before it blocks on the full channel, proving the buffer caps in-memory prefetch. `TestPauseHaltsFetcher` pauses immediately, waits past `fetchMaxWait` so any in-flight fetch has timed out, then appends records and asserts the position never moves until `Resume`. `TestSeekRepositionsConsumer` and the seek-to-end/beginning tests pin the epoch-safe reposition, and `TestAtMostOnceCommitsBeforeReturn` plus `TestAutoCommitPeriodicFlush` pin the two commit timings.

Create `consumer_test.go`:

```go
package consumer

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// mustNew creates a Consumer and registers t.Cleanup to close it.
func mustNew(t *testing.T, broker Broker, topic string, partitions []int, cfg Config) *Consumer {
	t.Helper()
	c, err := New(broker, topic, partitions, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// pollUntil collects records from c until want records are received or
// deadline passes.
func pollUntil(t *testing.T, c *Consumer, want int, deadline time.Time) []*Record {
	t.Helper()
	var out []*Record
	for len(out) < want && time.Now().Before(deadline) {
		batch, err := c.Poll(100 * time.Millisecond)
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		out = append(out, batch...)
	}
	return out
}

func TestNewRejectsNilBroker(t *testing.T) {
	t.Parallel()
	_, err := New(nil, "t", []int{0}, Config{})
	if err == nil {
		t.Fatal("expected error for nil broker")
	}
}

func TestNewRejectsEmptyTopic(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	_, err := New(b, "", []int{0}, Config{})
	if err == nil {
		t.Fatal("expected error for empty topic")
	}
}

func TestNewRejectsEmptyPartitions(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	_, err := New(b, "t", nil, Config{})
	if err == nil {
		t.Fatal("expected error for nil partitions")
	}
}

func TestNewLoadsCommittedOffset(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	// Simulate a pre-existing committed offset (consumer restart scenario).
	_ = b.CommitOffset(0, 3)

	c := mustNew(t, b, "restart", []int{0}, Config{FetchBufferSize: 100})

	pos, err := c.Position(0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 3 {
		t.Errorf("position = %d, want 3 (resumed from committed offset)", pos)
	}
}

func TestPollReturnsBatchedRecords(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	b.Append(0, nil, []byte("msg-0"))
	b.Append(0, nil, []byte("msg-1"))
	b.Append(0, nil, []byte("msg-2"))

	c := mustNew(t, b, "events", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	records := pollUntil(t, c, 3, time.Now().Add(2*time.Second))
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	for i, r := range records {
		if r.Offset != int64(i) {
			t.Errorf("records[%d].Offset = %d, want %d", i, r.Offset, i)
		}
	}
}

func TestPollReturnsNilOnTimeout(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	c := mustNew(t, b, "empty", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 10})

	start := time.Now()
	got, err := c.Poll(50 * time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no records, got %d", len(got))
	}
	if elapsed < 40*time.Millisecond {
		t.Errorf("Poll returned too early: %v", elapsed)
	}
}

// TestBackpressureBufferBlocks verifies that the fetch goroutine stops
// advancing the position once the prefetch buffer is full. A buffer of size 2
// with 5 available records means the fetcher can advance position to at most 2
// (having buffered records 0 and 1) before blocking on the full channel.
func TestBackpressureBufferBlocks(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	for i := range 5 {
		b.Append(0, nil, []byte{byte(i)})
	}

	// Buffer capacity 2: the fetcher will block after buffering 2 records.
	c := mustNew(t, b, "bp", []int{0}, Config{
		MaxPollRecords:  10,
		FetchBufferSize: 2,
	})

	// Allow the fetcher to run, fill the buffer, and block.
	time.Sleep(150 * time.Millisecond)

	pos, err := c.Position(0)
	if err != nil {
		t.Fatal(err)
	}
	// Position must not exceed 3: the fetcher advanced for the two buffered
	// records (positions 0 and 1 -> position = 2) plus at most one batch
	// boundary overshoot.
	if pos > 3 {
		t.Errorf("position = %d, want <= 3: backpressure not applied", pos)
	}
}

// TestPauseHaltsFetcher verifies that a paused consumer does not advance its
// fetch position even when new records become available on the broker.
//
// Design note: the fetchLoop uses a bounded fetch context (fetchMaxWait =
// 100 ms). A Pause call therefore takes effect within 100 ms: the in-flight
// Broker.Fetch times out, the loop restarts, sees pause=true, and enters
// the idle spin. We wait 150 ms after pausing before adding records to ensure
// the fetcher is truly idle before the assertion window begins.
func TestPauseHaltsFetcher(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()

	c := mustNew(t, b, "pause", []int{0}, Config{
		MaxPollRecords:  10,
		FetchBufferSize: 100,
	})

	// Pause immediately. Even if the fetchLoop raced into Broker.Fetch before
	// seeing the pause flag, that call times out after 100 ms and the loop
	// then sees pause=true and idles.
	if err := c.Pause(0); err != nil {
		t.Fatal(err)
	}

	// Wait > fetchMaxWait (100 ms) so any in-flight fetch has timed out and
	// the fetcher is spinning in the pause-idle loop.
	time.Sleep(150 * time.Millisecond)

	// Add records now. The fetcher is idle and must not call Broker.Fetch.
	b.Append(0, nil, []byte("paused-1"))
	b.Append(0, nil, []byte("paused-2"))

	// Give the fetcher time to misbehave.
	time.Sleep(150 * time.Millisecond)

	pos, _ := c.Position(0)
	if pos != 0 {
		t.Errorf("position = %d while paused, want 0", pos)
	}

	batch, _ := c.Poll(20 * time.Millisecond)
	if len(batch) != 0 {
		t.Errorf("got %d records while paused, want 0", len(batch))
	}

	// Resume and verify records are now fetched.
	if err := c.Resume(0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pos, _ := c.Position(0)
		if pos >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	pos, _ = c.Position(0)
	t.Errorf("position = %d after resume, want >= 2", pos)
}

func TestCommitSync(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	b.Append(0, nil, []byte("x"))
	b.Append(0, nil, []byte("y"))

	c := mustNew(t, b, "commit", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	records := pollUntil(t, c, 2, time.Now().Add(2*time.Second))
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}

	last := records[len(records)-1]
	if err := c.CommitSync(map[int]int64{0: last.Offset + 1}); err != nil {
		t.Fatalf("CommitSync: %v", err)
	}

	committed, err := c.Committed(0)
	if err != nil {
		t.Fatal(err)
	}
	if committed != 2 {
		t.Errorf("committed = %d, want 2", committed)
	}
}

func TestCommitAsyncInvokesCallback(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	c := mustNew(t, b, "async", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	var wg sync.WaitGroup
	wg.Add(1)
	var cbErr error
	c.CommitAsync(map[int]int64{0: 5}, func(err error) {
		cbErr = err
		wg.Done()
	})
	wg.Wait()

	if cbErr != nil {
		t.Fatalf("CommitAsync callback error: %v", cbErr)
	}
	committed, _ := c.Committed(0)
	if committed != 5 {
		t.Errorf("committed = %d, want 5", committed)
	}
}

func TestSeekRepositionsConsumer(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	for i := range 6 {
		b.Append(0, nil, []byte{byte(i)})
	}

	c := mustNew(t, b, "seek", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	// Consume the first 3 records.
	_ = pollUntil(t, c, 3, time.Now().Add(2*time.Second))

	if err := c.Seek(0, 0); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	pos, _ := c.Position(0)
	if pos != 0 {
		t.Errorf("position after Seek(0,0) = %d, want 0", pos)
	}

	// Next poll must start from offset 0 again.
	var first *Record
	deadline := time.Now().Add(2 * time.Second)
	for first == nil && time.Now().Before(deadline) {
		batch, err := c.Poll(100 * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) > 0 {
			first = batch[0]
		}
	}
	if first == nil {
		t.Fatal("no record after seek")
	}
	if first.Offset != 0 {
		t.Errorf("first record after seek: offset = %d, want 0", first.Offset)
	}
}

func TestSeekToEndSkipsExistingRecords(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	for range 4 {
		b.Append(0, nil, []byte("old"))
	}

	c := mustNew(t, b, "seek-end", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	if err := c.SeekToEnd(0); err != nil {
		t.Fatal(err)
	}

	// A record written after the seek must be the first one delivered.
	b.Append(0, nil, []byte("new"))

	var records []*Record
	deadline := time.Now().Add(2 * time.Second)
	for len(records) == 0 && time.Now().Before(deadline) {
		batch, err := c.Poll(100 * time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, batch...)
	}
	if len(records) == 0 {
		t.Fatal("no records after seek-to-end")
	}
	if string(records[0].Value) != "new" {
		t.Errorf("first record value = %q, want \"new\"", records[0].Value)
	}
}

func TestSeekToBeginningReprocesses(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	b.Append(0, nil, []byte("first"))
	b.Append(0, nil, []byte("second"))

	c := mustNew(t, b, "seek-begin", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	_ = pollUntil(t, c, 2, time.Now().Add(2*time.Second))

	if err := c.SeekToBeginning(0); err != nil {
		t.Fatal(err)
	}

	var first *Record
	deadline := time.Now().Add(2 * time.Second)
	for first == nil && time.Now().Before(deadline) {
		batch, _ := c.Poll(100 * time.Millisecond)
		if len(batch) > 0 {
			first = batch[0]
		}
	}
	if first == nil || first.Offset != 0 {
		t.Errorf("after SeekToBeginning: first offset = %v (want 0)", first)
	}
}

func TestPositionAndCommittedAreDistinct(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	b.Append(0, nil, []byte("a"))
	b.Append(0, nil, []byte("b"))
	b.Append(0, nil, []byte("c"))

	c := mustNew(t, b, "distinct", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	// Poll without committing.
	_ = pollUntil(t, c, 3, time.Now().Add(2*time.Second))

	pos, _ := c.Position(0)
	committed, _ := c.Committed(0)

	if pos == 0 {
		t.Error("position must have advanced after Poll")
	}
	// No CommitSync was called; broker committed offset must be -1 (fresh) or 0.
	if committed != -1 && committed != 0 {
		t.Errorf("committed = %d without explicit CommitSync, want -1 or 0", committed)
	}
	if pos <= committed {
		t.Errorf("position (%d) must be > committed (%d)", pos, committed)
	}
}

func TestLagDecreasesAsRecordsAreConsumed(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	for range 4 {
		b.Append(0, nil, nil)
	}

	c := mustNew(t, b, "lag", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	lagBefore, err := c.Lag()
	if err != nil {
		t.Fatal(err)
	}

	_ = pollUntil(t, c, 4, time.Now().Add(2*time.Second))

	lagAfter, err := c.Lag()
	if err != nil {
		t.Fatal(err)
	}

	if lagAfter[0] >= lagBefore[0] {
		t.Errorf("lag did not decrease: before=%d after=%d", lagBefore[0], lagAfter[0])
	}
}

func TestUnknownPartitionErrors(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	c := mustNew(t, b, "unknown", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	tests := []struct {
		name string
		fn   func() error
	}{
		{"Committed", func() error { _, err := c.Committed(99); return err }},
		{"Position", func() error { _, err := c.Position(99); return err }},
		{"Seek", func() error { return c.Seek(99, 0) }},
		{"Pause", func() error { return c.Pause(99) }},
		{"Resume", func() error { return c.Resume(99) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.fn()
			if !errors.Is(err, ErrUnknownPartition) {
				t.Errorf("%s: err = %v, want ErrUnknownPartition", tt.name, err)
			}
		})
	}
}

func TestCloseFlushesAutoCommit(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	b.Append(0, nil, []byte("data"))

	c, err := New(b, "close", []int{0}, Config{
		MaxPollRecords:     10,
		FetchBufferSize:    100,
		AutoCommit:         true,
		AutoCommitInterval: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Poll the record so it is tracked in pendingCommit.
	_ = pollUntil(t, c, 1, time.Now().Add(2*time.Second))

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close must flush pending auto-commit.
	committed, _ := b.CommittedOffset(0)
	if committed != 1 {
		t.Errorf("after Close: committed = %d, want 1", committed)
	}
}

func TestAtMostOnceCommitsBeforeReturn(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	b.Append(0, nil, []byte("once"))

	c := mustNew(t, b, "amo", []int{0}, Config{
		MaxPollRecords:  10,
		FetchBufferSize: 100,
		Mode:            AtMostOnce,
	})

	records := pollUntil(t, c, 1, time.Now().Add(2*time.Second))
	if len(records) == 0 {
		t.Fatal("no records received")
	}

	// AtMostOnce: Poll commits before returning.
	committed, _ := b.CommittedOffset(0)
	if committed != 1 {
		t.Errorf("AtMostOnce: committed = %d after Poll, want 1", committed)
	}
}

// TestAutoCommitPeriodicFlush pins the auto-commit contract independently of
// Close: with a short AutoCommitInterval, polling a record and waiting a few
// intervals must make the broker report a committed offset without any
// explicit CommitSync call.
func TestAutoCommitPeriodicFlush(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	b.Append(0, nil, []byte("auto"))

	c := mustNew(t, b, "autocommit", []int{0}, Config{
		MaxPollRecords:     10,
		FetchBufferSize:    100,
		AutoCommit:         true,
		AutoCommitInterval: 50 * time.Millisecond,
	})

	// Poll the record so its offset enters pendingCommit.
	_ = pollUntil(t, c, 1, time.Now().Add(2*time.Second))

	// Wait for at least one auto-commit tick to fire (no CommitSync involved).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		committed, _ := b.CommittedOffset(0)
		if committed >= 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	committed, _ := b.CommittedOffset(0)
	t.Errorf("auto-commit did not flush: committed = %d, want >= 1", committed)
}

func TestConcurrentPollIsSafe(t *testing.T) {
	t.Parallel()
	b := NewInMemoryBroker()
	for range 200 {
		b.Append(0, nil, []byte("concurrent"))
	}

	c := mustNew(t, b, "conc", []int{0}, Config{MaxPollRecords: 10, FetchBufferSize: 100})

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_, err := c.Poll(10 * time.Millisecond)
				if err != nil && !errors.Is(err, ErrClosed) {
					t.Errorf("Poll error: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}
```

Create `example_test.go`:

```go
package consumer_test

import (
	"fmt"

	"example.com/consumer"
)

// ExampleNew demonstrates creating a Consumer and reading its initial
// per-partition positions. The broker has no committed offsets, so both
// partitions start at position 0.
func ExampleNew() {
	b := consumer.NewInMemoryBroker()
	c, err := consumer.New(b, "orders", []int{0, 1}, consumer.Config{
		MaxPollRecords:  100,
		FetchBufferSize: 1000,
		Mode:            consumer.AtLeastOnce,
	})
	if err != nil {
		fmt.Printf("error: %v\n", err)
		return
	}
	defer c.Close()

	pos0, _ := c.Position(0)
	pos1, _ := c.Position(1)
	fmt.Printf("partition 0 position: %d\n", pos0)
	fmt.Printf("partition 1 position: %d\n", pos1)
	// Output:
	// partition 0 position: 0
	// partition 1 position: 0
}
```

## Review

The design is correct when three independent mechanisms each hold. Backpressure: the fetch position advances only inside the `case buf.ch <- r:` branch, so a full buffer stalls the fetcher without losing or double-counting a record -- `TestBackpressureBufferBlocks` confirms the position stops climbing. Promptness: every `Broker.Fetch` runs under a `fetchMaxWait` timeout, so `Pause` and `Seek` are observed within that window even on an idle partition -- the indefinite-`Fetch` bug instead hangs `TestSeekRepositionsConsumer` once the partition drains. Ordering under `Seek`: the epoch is bumped before the buffer is drained and checked before every send, so a batch fetched at the old position is discarded rather than delivered after the seek.

The commit rule is the other half of correctness. Position (where the fetcher is) and committed offset (what the broker knows is durable) are distinct, and auto-commit tracks `pendingCommit` -- the highest offset returned by `Poll` -- never the position, so a restart never skips a buffered-but-unprocessed record. `AtLeastOnce` commits after the caller processes (duplicates possible, no loss); `AtMostOnce` commits before returning (loss possible, no duplicates); exactly-once requires idempotent application logic layered on at-least-once. Confirm every test passes under `go test -race ./...`, which exercises the `RWMutex`, the two atomics, and the channel sends together.

## Resources

- [Apache Kafka Consumer design](https://kafka.apache.org/documentation/#theconsumer) -- the pull-based model, prefetch, offset commits, and delivery guarantees this module mirrors.
- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) -- the bounded-fetch primitive that makes `Pause` and `Seek` take effect on an idle partition.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) -- `atomic.Bool` and `atomic.Int64`, used for the pause flag and the epoch counter without holding the mutex.
- [Go Memory Model](https://go.dev/ref/mem) -- the happens-before rules that make the channel-as-buffer handoff safe under the race detector.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-credit-based-flow-control.md](02-credit-based-flow-control.md)
