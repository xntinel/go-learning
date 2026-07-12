# Exercise 24: Metrics Aggregator With Sharding, Batching, and Flush Policy

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A sharded metrics aggregator spreads incoming data points across independent
shards to avoid one lock serializing every write, and each shard batches
before flushing downstream. This module builds that aggregator through
options, checking that a batch can actually be assembled before a shard
starts rejecting new metrics — which means the batch size must never exceed
the per-shard queue depth.

## What you'll build

```text
metricsagg/                      independent module: example.com/metricsagg
  go.mod                         go 1.24
  metricsagg.go                  Metric, Aggregator, Option, New, WithShardCount,
                                  WithBatchSize, WithQueueDepth, WithFlushInterval,
                                  WithOnFlush, WithClock, Record, FlushDue,
                                  QueueLen, ShardCount
  cmd/
    demo/
      main.go                    records into one shard, flushes at batch size, then on staleness
  metricsagg_test.go              table test over options plus queue/flush behavior and concurrency
```

- Files: `metricsagg.go`, `cmd/demo/main.go`, `metricsagg_test.go`.
- Implement: `New(opts ...Option) (*Aggregator, error)` whose `Record` buffers per shard (rejecting once a shard is full) and whose `FlushDue` flushes a shard once it reaches batch size or holds a stale partial batch, validating that batch size never exceeds queue depth.
- Test: every option-validation case, `Record` rejecting a full shard, `FlushDue` triggering on both batch size and staleness, and a `-race` concurrency check across shards.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/12-functional-options-pattern/24-metrics-event-aggregator/cmd/demo
cd go-solutions/04-functions/12-functional-options-pattern/24-metrics-event-aggregator
go mod edit -go=1.24
```

### Why batch size must fit inside queue depth

`WithBatchSize` and `WithQueueDepth` are independent options — either can be
set without the other, in any order. `Record` only ever buffers into a
shard's queue and rejects once that queue reaches `queueDepth`; it never
flushes on its own. `FlushDue` is the only thing that empties a queue, and
it only does so once a shard reaches `batchSize` or goes stale. If
`batchSize` were allowed to exceed `queueDepth`, `Record` would start
rejecting new metrics before a shard ever reached the batch size `FlushDue`
is watching for — the batch threshold would be permanently unreachable.
`New` checks `batchSize > queueDepth` after every option has run, because
neither option's closure knows the other's value.

### Record buffers, FlushDue decides

This module deliberately separates *what accepts data* from *what decides
when to ship it*. `Record` is the hot path: hash the key to a shard index,
lock only that shard, append or reject. `FlushDue` is the periodic
maintenance path a caller runs on a schedule (a ticker, in production): it
checks every shard for either trigger — batch size reached, or a partial
batch sitting past the flush interval — and flushes those that qualify.
Keeping them separate is what makes `TestRecordRejectsWhenQueueFull`
meaningful: with the two combined, a shard could never actually reach queue
depth, because it would always auto-flush at batch size first.

### Firing the hook outside each shard's lock

`FlushDue` copies a shard's queue, clears it, and releases that shard's
lock *before* calling `onFlush` — the same discipline used for the TTL
cache's eviction hook earlier in this chapter. A hook that calls back into
the aggregator (to record its own metric about the flush, say) cannot
deadlock on the shard it was just flushed from.

Create `metricsagg.go`:

```go
package metricsagg

import (
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// Metric is a single recorded data point.
type Metric struct {
	Name  string
	Value float64
	At    time.Time
}

type shardState struct {
	mu        sync.Mutex
	queue     []Metric
	lastFlush time.Time
}

// Aggregator shards incoming metrics, batches them per shard, and flushes a
// shard either when its batch fills or when the flush interval elapses.
type Aggregator struct {
	shardCount    int
	batchSize     int
	queueDepth    int
	flushInterval time.Duration
	now           func() time.Time
	onFlush       func(shard int, batch []Metric)

	shards []*shardState
}

// Option configures an Aggregator and may reject invalid input.
type Option func(*Aggregator) error

// New seeds defaults, applies opts in order, then validates the cross-field
// invariant no single option could see: batch size must not exceed queue
// depth, or Record would start rejecting metrics before a shard's queue
// ever reached the batch size FlushDue watches for.
func New(opts ...Option) (*Aggregator, error) {
	a := &Aggregator{
		shardCount:    4,
		batchSize:     10,
		queueDepth:    100,
		flushInterval: time.Second,
		now:           time.Now,
		onFlush:       func(int, []Metric) {},
	}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}

	if a.batchSize > a.queueDepth {
		return nil, fmt.Errorf("batch size %d exceeds queue depth %d", a.batchSize, a.queueDepth)
	}

	a.shards = make([]*shardState, a.shardCount)
	now := a.now()
	for i := range a.shards {
		a.shards[i] = &shardState{lastFlush: now}
	}
	return a, nil
}

// WithShardCount sets how many independent shards route and batch metrics
// (>= 1).
func WithShardCount(n int) Option {
	return func(a *Aggregator) error {
		if n < 1 {
			return fmt.Errorf("shard count must be >= 1, got %d", n)
		}
		a.shardCount = n
		return nil
	}
}

// WithBatchSize sets how many metrics accumulate before a shard flushes
// immediately (>= 1).
func WithBatchSize(n int) Option {
	return func(a *Aggregator) error {
		if n < 1 {
			return fmt.Errorf("batch size must be >= 1, got %d", n)
		}
		a.batchSize = n
		return nil
	}
}

// WithQueueDepth caps how many metrics a shard buffers before Record starts
// rejecting new ones (>= 1).
func WithQueueDepth(n int) Option {
	return func(a *Aggregator) error {
		if n < 1 {
			return fmt.Errorf("queue depth must be >= 1, got %d", n)
		}
		a.queueDepth = n
		return nil
	}
}

// WithFlushInterval sets the maximum time a partial batch waits before
// FlushDue considers it stale (> 0).
func WithFlushInterval(d time.Duration) Option {
	return func(a *Aggregator) error {
		if d <= 0 {
			return fmt.Errorf("flush interval must be positive, got %s", d)
		}
		a.flushInterval = d
		return nil
	}
}

// WithOnFlush injects the callback invoked with each flushed batch.
func WithOnFlush(fn func(shard int, batch []Metric)) Option {
	return func(a *Aggregator) error {
		if fn == nil {
			return fmt.Errorf("onFlush is nil")
		}
		a.onFlush = fn
		return nil
	}
}

// WithClock injects the clock used to time flush intervals.
func WithClock(now func() time.Time) Option {
	return func(a *Aggregator) error {
		if now == nil {
			return fmt.Errorf("clock is nil")
		}
		a.now = now
		return nil
	}
}

func shardIndex(key string, n int) int {
	h := fnv.New32a()
	h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}

// Record routes m to key's shard and appends it to that shard's queue. If
// the shard is already at queue depth, Record rejects the metric rather
// than growing unbounded; it does not flush on its own. Flushing is
// FlushDue's job, run by a caller-driven loop (a ticker, in production).
func (a *Aggregator) Record(key string, m Metric) error {
	idx := shardIndex(key, a.shardCount)
	s := a.shards[idx]

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.queue) >= a.queueDepth {
		return fmt.Errorf("shard %d queue is full at depth %d", idx, a.queueDepth)
	}
	s.queue = append(s.queue, m)
	return nil
}

// FlushDue flushes every shard that has either reached the batch size or
// held a non-empty partial batch past the flush interval, returning the
// shard indices flushed in ascending order. The hook fires after each
// shard's lock is released, so a hook that calls back into the aggregator
// cannot deadlock on that shard's mutex.
func (a *Aggregator) FlushDue() []int {
	now := a.now()
	var flushed []int
	for i, s := range a.shards {
		s.mu.Lock()
		var toFlush []Metric
		sizeDue := len(s.queue) >= a.batchSize
		timeDue := len(s.queue) > 0 && now.Sub(s.lastFlush) >= a.flushInterval
		if sizeDue || timeDue {
			toFlush = append([]Metric(nil), s.queue...)
			s.queue = s.queue[:0]
			s.lastFlush = now
		}
		s.mu.Unlock()

		if toFlush != nil {
			a.onFlush(i, toFlush)
			flushed = append(flushed, i)
		}
	}
	return flushed
}

// QueueLen reports how many metrics are currently buffered in shard i.
func (a *Aggregator) QueueLen(i int) int {
	s := a.shards[i]
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// ShardCount reports how many shards the aggregator was built with.
func (a *Aggregator) ShardCount() int { return a.shardCount }
```

### The runnable demo

The demo records into the same key three times, one at a time, calling
`FlushDue` after each: the first call has nothing to do (below batch size),
the second flushes exactly at batch size, and the third — a lone leftover
metric — only flushes once the clock advances past the flush interval.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/metricsagg"
)

func main() {
	current := time.Unix(0, 0).UTC()
	clock := func() time.Time { return current }

	var flushedBatches [][]metricsagg.Metric
	a, err := metricsagg.New(
		metricsagg.WithShardCount(2),
		metricsagg.WithBatchSize(2),
		metricsagg.WithQueueDepth(5),
		metricsagg.WithFlushInterval(time.Minute),
		metricsagg.WithClock(clock),
		metricsagg.WithOnFlush(func(shard int, batch []metricsagg.Metric) {
			fmt.Printf("flushed shard %d, batch size %d\n", shard, len(batch))
			flushedBatches = append(flushedBatches, batch)
		}),
	)
	if err != nil {
		panic(err)
	}

	metric := func(v float64) metricsagg.Metric {
		return metricsagg.Metric{Name: "cpu.temp", Value: v, At: current}
	}

	// Below batch size: FlushDue has nothing to do yet.
	if err := a.Record("cpu.temp", metric(40)); err != nil {
		panic(err)
	}
	fmt.Printf("shards flushed while under batch size: %v\n", a.FlushDue())

	// Reaching batch size (2): the next FlushDue call flushes it.
	if err := a.Record("cpu.temp", metric(41)); err != nil {
		panic(err)
	}
	fmt.Printf("shards flushed once batch size is reached: %v\n", a.FlushDue())

	// A lone metric that never reaches batch size still flushes once stale.
	if err := a.Record("cpu.temp", metric(42)); err != nil {
		panic(err)
	}
	current = current.Add(90 * time.Second) // past the flush interval
	fmt.Printf("shards flushed by stale partial batch: %v\n", a.FlushDue())

	fmt.Printf("total flushes: %d\n", len(flushedBatches))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
shards flushed while under batch size: []
flushed shard 1, batch size 2
shards flushed once batch size is reached: [1]
flushed shard 1, batch size 1
shards flushed by stale partial batch: [1]
total flushes: 2
```

### Tests

`TestNewValidation` tables the batch-size/queue-depth invariant, including
the exact-boundary case where they are equal. `TestRecordRejectsWhenQueueFull`
proves a shard genuinely reaches queue depth and rejects further metrics
when nothing has flushed it. `TestFlushDueTriggersOnBatchSizeAndOnStaleness`
proves both triggers independently against an injected clock.
`TestConcurrentRecordAcrossShards` runs `-race` over 200 concurrent
`Record` calls spread across keys and shards.

Create `metricsagg_test.go`:

```go
package metricsagg

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		opts    []Option
		wantErr bool
	}{
		{name: "defaults only"},
		{name: "batch size exceeds queue depth", opts: []Option{
			WithBatchSize(20), WithQueueDepth(10),
		}, wantErr: true},
		{name: "batch size equal to queue depth is allowed", opts: []Option{
			WithBatchSize(10), WithQueueDepth(10),
		}},
		{name: "invalid shard count", opts: []Option{WithShardCount(0)}, wantErr: true},
		{name: "invalid batch size", opts: []Option{WithBatchSize(0)}, wantErr: true},
		{name: "invalid queue depth", opts: []Option{WithQueueDepth(0)}, wantErr: true},
		{name: "invalid flush interval", opts: []Option{WithFlushInterval(0)}, wantErr: true},
		{name: "nil onFlush rejected", opts: []Option{WithOnFlush(nil)}, wantErr: true},
		{name: "nil clock rejected", opts: []Option{WithClock(nil)}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(tt.opts...)
			if tt.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestRecordRejectsWhenQueueFull(t *testing.T) {
	t.Parallel()

	a, err := New(
		WithShardCount(1),
		WithBatchSize(2),
		WithQueueDepth(2),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Record("k", Metric{Name: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Record("k", Metric{Name: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Record("k", Metric{Name: "m"}); err == nil {
		t.Fatal("expected error when shard queue is at depth and nothing has flushed it")
	}
	if got := a.QueueLen(0); got != 2 {
		t.Fatalf("QueueLen(0) = %d, want 2 (the rejected metric must not have been appended)", got)
	}
}

func TestFlushDueTriggersOnBatchSizeAndOnStaleness(t *testing.T) {
	t.Parallel()

	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	current := base

	var mu sync.Mutex
	var flushedLens []int
	a, err := New(
		WithShardCount(1),
		WithBatchSize(2),
		WithQueueDepth(5),
		WithFlushInterval(time.Minute),
		WithClock(func() time.Time { return current }),
		WithOnFlush(func(shard int, batch []Metric) {
			mu.Lock()
			flushedLens = append(flushedLens, len(batch))
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Record("k", Metric{Name: "m"}); err != nil {
		t.Fatal(err)
	}
	if due := a.FlushDue(); len(due) != 0 {
		t.Fatalf("FlushDue() = %v below batch size and before staleness, want empty", due)
	}

	if err := a.Record("k", Metric{Name: "m"}); err != nil {
		t.Fatal(err)
	}
	if due := a.FlushDue(); len(due) != 1 || due[0] != 0 {
		t.Fatalf("FlushDue() = %v at batch size, want [0]", due)
	}

	if err := a.Record("k", Metric{Name: "m"}); err != nil {
		t.Fatal(err)
	}
	current = base.Add(90 * time.Second) // past flush interval, still below batch size
	if due := a.FlushDue(); len(due) != 1 || due[0] != 0 {
		t.Fatalf("FlushDue() = %v once stale, want [0]", due)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(flushedLens) != 2 || flushedLens[0] != 2 || flushedLens[1] != 1 {
		t.Fatalf("flushedLens = %v, want [2 1]", flushedLens)
	}
}

func TestConcurrentRecordAcrossShards(t *testing.T) {
	t.Parallel()

	a, err := New(
		WithShardCount(4),
		WithBatchSize(3),
		WithQueueDepth(200),
	)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("key-%d", i%7)
			if err := a.Record(key, Metric{Name: "m", Value: float64(i)}); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()

	total := 0
	for i := 0; i < a.ShardCount(); i++ {
		total += a.QueueLen(i)
	}
	if total != 200 {
		t.Fatalf("total queued across shards = %d, want 200", total)
	}
}
```

## Review

The aggregator is correct when a shard's batch threshold is always
reachable before its queue depth turns away new metrics, and when flushing
— whether triggered by size or by staleness — always happens under that
shard's own lock with the hook fired safely outside it. Separating
`Record` (accept) from `FlushDue` (decide and ship) is the general shape for
any batching system: the hot path stays a simple bounded buffer, and the
policy of *when* to drain it lives in one place that can inspect every
shard uniformly. The batch-size/queue-depth check is the same "one option's
value would make another option's effect unreachable" pattern seen with the
rate limiter's step interval and window earlier in this chapter — the fix
is always a check in the constructor, once every option has run.

## Resources

- [pkg.go.dev: hash/fnv](https://pkg.go.dev/hash/fnv)
- [Prometheus: pushgateway batching](https://github.com/prometheus/pushgateway)
- [Statsd protocol and batching](https://github.com/statsd/statsd/blob/master/docs/metric_types.md)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [23-message-dedupe-sliding-window.md](23-message-dedupe-sliding-window.md) | Next: [25-endpoint-health-checker.md](25-endpoint-health-checker.md)
