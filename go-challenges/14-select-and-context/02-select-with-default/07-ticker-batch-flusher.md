# Exercise 7: Size-and-Time Batch Flusher for Bulk Inserts / Log Shipping

Bulk-writing to a database or shipping spans to a collector is far cheaper per item
in batches, but you cannot wait forever to fill a batch or tail latency explodes.
The standard answer — the one an OpenTelemetry batch span processor implements — is
"flush when the batch reaches `maxSize` OR a timer fires, whichever comes first".
The timer path sweeps whatever is buffered with a non-blocking drain. This is a
`select` over the input channel, a ticker, and `ctx.Done()`.

This module is fully self-contained: its own `go mod init`, demo, and tests.

## What you'll build

```text
batcher/                    independent module: example.com/batcher
  go.mod                    go 1.26
  batcher.go                type Batcher[T]; Add, Run (select over in/ticker/ctx)
  cmd/
    demo/
      main.go               size-triggered flush, then a time-triggered flush
  batcher_test.go           size flush, time flush, final drain on cancel, -race
```

- Files: `batcher.go`, `cmd/demo/main.go`, `batcher_test.go`.
- Implement: `Batcher[T any]` with `New(maxSize, queue int, interval time.Duration, flush func([]T))`, `Add(T)`, and `Run(ctx)` that flushes on `maxSize` or on each tick, and does a final drain on cancellation.
- Test: feeding `maxSize` items flushes exactly one batch of `maxSize`; feeding fewer then waiting one interval flushes the partial batch; cancelling with items pending flushes the remainder so nothing is lost; a `-race` run with a concurrent feeder preserves the item count.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/02-select-with-default/07-ticker-batch-flusher/cmd/demo
cd go-solutions/14-select-and-context/02-select-with-default/07-ticker-batch-flusher
go mod edit -go=1.26
```

### Whichever comes first, and never lose the tail

`Run` owns a single batch slice and a ticker. Its `select` has three cases. The
input case appends an item and, if the batch has reached `maxSize`, flushes
immediately — this bounds *memory and payload size*. The ticker case flushes
whatever has accumulated — this bounds *latency*, so a trickle of items still gets
written within one interval rather than waiting indefinitely for the batch to fill.
The `ctx.Done()` case is the one people forget: on shutdown it performs a final
non-blocking drain of the input channel, appending and flushing everything still
buffered, so pending items are written rather than dropped. Without that final
drain, a SIGTERM mid-batch silently loses the half-full batch and whatever is queued
behind it.

The flush helper is a no-op on an empty batch, so a ticker that fires during an idle
period does not call the sink with nothing. After each flush it allocates a fresh
batch slice rather than reslicing to length zero, which avoids handing the sink a
slice whose backing array a later `append` would overwrite — the sink may retain the
slice (queue it, write it asynchronously), so it must own its memory.

Because all state (the batch) lives in the single `Run` goroutine and is only
touched there, no mutex is needed inside `Batcher`; the only cross-goroutine channel
is `in`, fed by `Add`. `Add` is a blocking send, which gives natural backpressure:
if the batcher cannot keep up and the input buffer fills, producers slow down rather
than growing memory without bound.

Create `batcher.go`:

```go
package batcher

import (
	"context"
	"time"
)

// Batcher accumulates items and flushes them in batches, whenever the batch
// reaches maxSize or the flush interval elapses, whichever comes first. On
// cancellation it flushes whatever remains so no item is lost.
type Batcher[T any] struct {
	in       chan T
	maxSize  int
	interval time.Duration
	flush    func([]T)
}

// New returns a Batcher. maxSize caps the batch length (and payload); interval
// bounds how long an item waits before being flushed; queue is the input buffer.
func New[T any](maxSize, queue int, interval time.Duration, flush func([]T)) *Batcher[T] {
	return &Batcher[T]{
		in:       make(chan T, queue),
		maxSize:  maxSize,
		interval: interval,
		flush:    flush,
	}
}

// Add enqueues an item for the next batch. It blocks if the input buffer is full,
// applying backpressure to the producer.
func (b *Batcher[T]) Add(item T) {
	b.in <- item
}

// Run processes items until ctx is done. It flushes on maxSize or on each tick,
// and performs a final drain-and-flush on cancellation. Call it in its own
// goroutine.
func (b *Batcher[T]) Run(ctx context.Context) {
	t := time.NewTicker(b.interval)
	defer t.Stop()

	batch := make([]T, 0, b.maxSize)
	doFlush := func() {
		if len(batch) == 0 {
			return
		}
		b.flush(batch)
		batch = make([]T, 0, b.maxSize)
	}

	for {
		select {
		case item := <-b.in:
			batch = append(batch, item)
			if len(batch) >= b.maxSize {
				doFlush()
			}
		case <-t.C:
			doFlush()
		case <-ctx.Done():
			// Final non-blocking drain: empty the input buffer, then flush.
			for {
				select {
				case item := <-b.in:
					batch = append(batch, item)
					if len(batch) >= b.maxSize {
						doFlush()
					}
				default:
					doFlush()
					return
				}
			}
		}
	}
}
```

### The runnable demo

The demo flushes once by size (feeding a full `maxSize` batch of 3) and once by time
(feeding 2 items and waiting past the interval), printing each flushed batch size.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"example.com/batcher"
)

func main() {
	var mu sync.Mutex
	var sizes []int
	flush := func(b []int) {
		mu.Lock()
		sizes = append(sizes, len(b))
		mu.Unlock()
	}

	b := batcher.New[int](3, 16, 20*time.Millisecond, flush)
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	// Size-triggered: a full batch of 3.
	for i := range 3 {
		b.Add(i)
	}
	// Time-triggered: 2 items, then wait past the interval.
	b.Add(100)
	b.Add(101)
	time.Sleep(60 * time.Millisecond)

	cancel()
	time.Sleep(10 * time.Millisecond) // let Run's final drain settle

	mu.Lock()
	total := 0
	for _, s := range sizes {
		total += s
	}
	fmt.Println("first batch (size-triggered):", sizes[0])
	fmt.Println("items flushed:", total)
	mu.Unlock()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (the first flush is the full size-3 batch; the rest arrive by the
timer and the final drain, and no item is ever lost):

```
first batch (size-triggered): 3
items flushed: 5
```

### Tests

The tests record every flushed batch under a mutex and use a non-blocking signal
channel to wake on a flush. `TestSizeTriggeredFlush` feeds exactly `maxSize` with a
one-hour interval (so only size or the final drain can flush) and asserts exactly one
batch of `maxSize`. `TestTimeTriggeredFlush` feeds fewer than `maxSize` with a short
interval and waits on the signal for the timed flush, asserting the partial batch.
`TestFinalDrainOnCancel` feeds a partial batch with a long interval, cancels, and
asserts the remainder is flushed (nothing lost). `TestConcurrentFeedNoLoss` feeds
from many goroutines under `-race` and asserts the total flushed count equals the
count fed.

Create `batcher_test.go`:

```go
package batcher

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recorder collects flushed batches and signals each flush without blocking Run.
type recorder struct {
	mu      sync.Mutex
	batches [][]int
	signal  chan int
}

func newRecorder() *recorder {
	return &recorder{signal: make(chan int, 256)}
}

func (r *recorder) flush(b []int) {
	cp := append([]int(nil), b...)
	r.mu.Lock()
	r.batches = append(r.batches, cp)
	r.mu.Unlock()
	select {
	case r.signal <- len(cp):
	default:
	}
}

func (r *recorder) total() (batches, items int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.batches {
		items += len(b)
	}
	return len(r.batches), items
}

func TestSizeTriggeredFlush(t *testing.T) {
	t.Parallel()

	const maxSize = 5
	r := newRecorder()
	b := New[int](maxSize, 16, time.Hour, r.flush) // interval never fires

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	for i := range maxSize {
		b.Add(i)
	}
	<-r.signal // wait for the size-triggered flush
	cancel()

	batches, items := r.total()
	if batches != 1 {
		t.Fatalf("flushed %d batches, want 1", batches)
	}
	if items != maxSize {
		t.Fatalf("flushed %d items, want %d", items, maxSize)
	}
}

func TestTimeTriggeredFlush(t *testing.T) {
	t.Parallel()

	r := newRecorder()
	b := New[int](100, 16, 10*time.Millisecond, r.flush) // maxSize never reached

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	b.Add(1)
	b.Add(2)
	b.Add(3)

	size := <-r.signal // the timed flush
	cancel()

	if size != 3 {
		t.Fatalf("timed flush of %d items, want 3", size)
	}
}

func TestFinalDrainOnCancel(t *testing.T) {
	t.Parallel()

	r := newRecorder()
	b := New[int](100, 16, time.Hour, r.flush) // neither size nor timer fires

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()

	for i := range 4 {
		b.Add(i)
	}
	cancel()
	<-done // Run's final drain has flushed and returned

	batches, items := r.total()
	if items != 4 {
		t.Fatalf("final drain flushed %d items, want 4 (nothing lost)", items)
	}
	if batches == 0 {
		t.Fatal("final drain produced no flush")
	}
}

func TestConcurrentFeedNoLoss(t *testing.T) {
	t.Parallel()

	const feeders, perFeeder = 8, 250
	const total = feeders * perFeeder

	r := newRecorder()
	b := New[int](32, 128, 5*time.Millisecond, r.flush)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()

	var wg sync.WaitGroup
	for range feeders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perFeeder {
				b.Add(i)
			}
		}()
	}
	wg.Wait()
	cancel()
	<-done

	if _, items := r.total(); items != total {
		t.Fatalf("flushed %d items, want %d (no loss)", items, total)
	}
}
```

## Review

The batcher is correct when it flushes on either trigger and loses nothing on
shutdown: a full `maxSize` batch flushes at once, a partial batch flushes within one
interval, and a cancel flushes the remainder. The defining invariant the tests pin
is conservation — the sum of all flushed batch lengths equals the number of items
fed — and `TestFinalDrainOnCancel` is what proves the `ctx.Done()` branch actually
drains rather than discarding. The classic bug is treating shutdown as `return`
without the final drain, which silently drops the last, half-full batch on every
graceful stop. The second is reslicing the batch to `batch[:0]` and reusing its
array while the sink still holds the previous slice — the fresh `make` per flush is
what makes the handed-off batch safe for an async sink to retain.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — multi-way `select` over input, ticker, and cancellation.
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — `NewTicker`, `Ticker.C`, `Ticker.Stop`, `Ticker.Reset`.
- [OpenTelemetry Batch Span Processor](https://opentelemetry.io/docs/specs/otel/trace/sdk/#batching-processor) — the size-or-interval batching this models.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-try-acquire-semaphore.md](08-try-acquire-semaphore.md)
