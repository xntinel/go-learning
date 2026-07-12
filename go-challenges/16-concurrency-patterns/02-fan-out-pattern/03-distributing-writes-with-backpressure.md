# Exercise 3: Distributing Writes with Backpressure

Fan-out has a write-side dual. Instead of computing results and merging them, you take a high-rate stream of writes and spread them across a pool of workers that each call a slow sink — a database, an object store, a message queue. The danger is memory: if the producer outruns the sink, an unbounded queue grows until the process dies. This module distributes writes across N workers and uses an unbuffered channel as a backpressure valve so the amount of work in flight is bounded by the worker count, never by how fast the producer runs.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
distribute.go        Item, Sink (injected target), Distributor.Run (bounded fan-out + backpressure)
cmd/
  demo/
    main.go          push 40 rows through 4 workers, report that peak in-flight <= workers
distribute_test.go   all-writes-land, in-flight bound, error aggregation, cancel stops early
```

- Files: `distribute.go`, `cmd/demo/main.go`, `distribute_test.go`.
- Implement: the `Sink` interface and `(*Distributor).Run(ctx, items <-chan Item) error`.
- Test: every item is written, peak concurrent writes never exceed the worker count, write errors are aggregated, and a cancelled context stops the pipeline before draining a huge stream.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/02-fan-out-pattern/03-distributing-writes-with-backpressure/cmd/demo && cd go-solutions/16-concurrency-patterns/02-fan-out-pattern/03-distributing-writes-with-backpressure
```

### Why an unbuffered channel is the backpressure valve

Backpressure is the property that a slow consumer slows the producer down instead of letting work pile up. The mechanism here is one design decision: the internal `jobs` channel is unbuffered. A send on an unbuffered channel does not complete until a receiver is ready, so `jobs <- item` blocks until some worker has finished its previous write and looped back to receive. While all `workers` goroutines are busy inside `Sink.Write`, the forwarding loop is parked on that send and stops pulling from `items`; the upstream producer, sending on `items`, then blocks on its own send. The blocking propagates all the way back to the source. The amount of data in memory at any instant is therefore bounded — at most one item per worker in flight plus the one parked in the forwarder — regardless of whether the producer can generate a million rows a second.

Add a buffer to `jobs` and you move the bound, you do not remove it: a buffer of 1000 simply lets 1000 items accumulate before backpressure engages, which is sometimes what you want (to smooth bursts) and sometimes a slow memory leak. The unbuffered choice is the strictest, most predictable bound, and it is the right default until a measurement says a buffer earns its memory.

Two more requirements round out the design. Writes can fail, and one failed write must not tear down the whole pipeline, so each worker records its error under a mutex and `Run` returns `errors.Join` of all of them after the stream drains — the happy path joins nothing and returns `nil`. And the pipeline must be cancellable: the forwarding loop selects on `ctx.Done()` both while waiting for the next item and while waiting to hand one to a worker, so a stuck or slow sink can never pin the caller forever. On cancellation `Run` stops forwarding, closes `jobs` so the workers drain and exit, waits for them, and folds the context error into the joined result.

Create `distribute.go`:

```go
package distribute

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Item is one unit of work to be written to the downstream sink.
type Item struct {
	Key   string
	Value []byte
}

// Sink is the downstream write target (a database, an object store, a queue).
// It is injected so the distributor stays independent of any concrete backend.
type Sink interface {
	Write(ctx context.Context, item Item) error
}

// Distributor fans a stream of items out across a fixed pool of workers, each of
// which calls the same Sink. It is the write-side dual of fan-out: instead of
// merging results back, it absorbs a high-rate producer and spreads the writes.
type Distributor struct {
	Workers int
	Sink    Sink
}

// Run reads items until the channel is closed (or ctx is cancelled) and writes
// each one through the sink using d.Workers goroutines.
//
// Backpressure is the point of this design. The internal jobs channel is
// UNBUFFERED, so a send blocks until some worker is ready to receive. When every
// worker is busy in Sink.Write, Run stops pulling from items, which in turn makes
// the upstream producer block on its own send. Memory in flight is therefore
// bounded by the number of workers, not by how fast the producer runs, so a fast
// producer feeding a slow sink can never build an unbounded backlog.
//
// Errors are aggregated: a failing write does not stop the pipeline; every write
// error is collected and returned as one joined error after the stream drains.
func (d *Distributor) Run(ctx context.Context, items <-chan Item) error {
	workers := d.Workers
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan Item) // unbuffered: this is the backpressure valve

	var (
		mu   sync.Mutex
		errs []error
	)
	record := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for item := range jobs {
				if err := d.Sink.Write(ctx, item); err != nil {
					record(fmt.Errorf("write %q: %w", item.Key, err))
				}
			}
		}()
	}

	// Forward items to the workers, but abandon the forward loop the moment the
	// context is cancelled so a stuck sink cannot pin the caller forever.
	var forwardErr error
forward:
	for {
		select {
		case item, ok := <-items:
			if !ok {
				break forward
			}
			select {
			case jobs <- item:
			case <-ctx.Done():
				forwardErr = ctx.Err()
				break forward
			}
		case <-ctx.Done():
			forwardErr = ctx.Err()
			break forward
		}
	}
	close(jobs)
	wg.Wait()

	if forwardErr != nil {
		errs = append(errs, forwardErr)
	}
	return errors.Join(errs...)
}
```

Trace the shutdown carefully, because closing `jobs` at the wrong time is the classic bug. `Run` is the sole sender on `jobs`, so it is the only goroutine allowed to close it, and it does so exactly once, after the forwarding loop has broken — whether the loop broke because `items` closed or because the context was cancelled. The `close(jobs)` then cascades through every worker's `for item := range jobs`, each worker returns and calls `wg.Done`, and `wg.Wait` unblocks. Only then does `Run` assemble the joined error. There is no separate closer goroutine here because, unlike the first exercise, `Run` itself is the goroutine that owns the channel's lifetime.

### The runnable demo

The demo pushes 40 rows through 4 workers backed by a `slowSink` that sleeps on every write and records the peak number of concurrent writes. The summary line is what proves backpressure works: with an unbuffered valve and four workers, the peak in-flight count settles at the worker count, never above it.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/distributing-writes"
)

// slowSink is a stand-in for a rate-limited write target. It tracks the peak
// number of concurrent writes so the demo can show backpressure holding the
// in-flight count at the worker bound.
type slowSink struct {
	written  atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
}

func (s *slowSink) Write(ctx context.Context, item distribute.Item) error {
	n := s.inFlight.Add(1)
	for {
		old := s.peak.Load()
		if n <= old || s.peak.CompareAndSwap(old, n) {
			break
		}
	}
	defer s.inFlight.Add(-1)
	time.Sleep(2 * time.Millisecond)
	s.written.Add(1)
	return nil
}

func main() {
	const workers = 4

	items := make(chan distribute.Item)
	go func() {
		defer close(items)
		for i := range 40 {
			items <- distribute.Item{Key: fmt.Sprintf("row-%02d", i)}
		}
	}()

	sink := &slowSink{}
	d := &distribute.Distributor{Workers: workers, Sink: sink}

	if err := d.Run(context.Background(), items); err != nil {
		fmt.Println("error:", err)
		return
	}

	fmt.Printf("workers=%d written=%d peak_in_flight=%d\n", workers, sink.written.Load(), sink.peak.Load())
	fmt.Printf("backpressure held: peak_in_flight <= workers is %v\n", sink.peak.Load() <= workers)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
workers=4 written=40 peak_in_flight=4
backpressure held: peak_in_flight <= workers is true
```

### Tests

The tests pin the four guarantees. `TestDistributorWritesEverything` checks that all 100 items reach the sink. `TestDistributorBoundsInFlight` is the backpressure proof: a `gaugeSink` tracks peak concurrent writes with atomics, and the test asserts the peak never exceeds the worker bound (and is at least two, so the test would catch an accidental serialization). `TestDistributorAggregatesErrors` fails two keys and asserts both are named in the joined error while the other eight still land. `TestDistributorStopsOnCancel` feeds a 100000-item stream, cancels after a few milliseconds, and asserts the pipeline stopped early with `context.Canceled` rather than draining the whole stream.

Create `distribute_test.go`:

```go
package distribute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// gaugeSink counts total writes and tracks the peak number of concurrent
// Write calls, so a test can prove the in-flight count never exceeds the
// worker bound (the observable signature of working backpressure).
type gaugeSink struct {
	written  atomic.Int64
	inFlight atomic.Int64
	peak     atomic.Int64
	failKeys map[string]bool
}

func (s *gaugeSink) Write(ctx context.Context, item Item) error {
	n := s.inFlight.Add(1)
	for {
		old := s.peak.Load()
		if n <= old || s.peak.CompareAndSwap(old, n) {
			break
		}
	}
	defer s.inFlight.Add(-1)

	select {
	case <-time.After(time.Millisecond):
	case <-ctx.Done():
		return ctx.Err()
	}

	if s.failKeys[item.Key] {
		return fmt.Errorf("disk full")
	}
	s.written.Add(1)
	return nil
}

// feed is a cancellable producer: it stops the moment ctx is cancelled, so it
// never leaks when the distributor abandons the stream under backpressure.
func feed(ctx context.Context, n int) <-chan Item {
	ch := make(chan Item)
	go func() {
		defer close(ch)
		for i := range n {
			select {
			case ch <- Item{Key: fmt.Sprintf("k%03d", i), Value: []byte("v")}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

func TestDistributorWritesEverything(t *testing.T) {
	t.Parallel()

	s := &gaugeSink{}
	d := &Distributor{Workers: 4, Sink: s}
	if err := d.Run(context.Background(), feed(context.Background(), 100)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := s.written.Load(); got != 100 {
		t.Fatalf("wrote %d items, want 100", got)
	}
}

func TestDistributorBoundsInFlight(t *testing.T) {
	t.Parallel()

	const workers = 3
	s := &gaugeSink{}
	d := &Distributor{Workers: workers, Sink: s}
	if err := d.Run(context.Background(), feed(context.Background(), 200)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if peak := s.peak.Load(); peak > workers {
		t.Fatalf("peak in-flight %d exceeded worker bound %d: backpressure is not holding", peak, workers)
	}
	if peak := s.peak.Load(); peak < 2 {
		t.Fatalf("peak in-flight %d shows no real parallelism", peak)
	}
}

func TestDistributorAggregatesErrors(t *testing.T) {
	t.Parallel()

	s := &gaugeSink{failKeys: map[string]bool{"k001": true, "k002": true}}
	d := &Distributor{Workers: 4, Sink: s}
	err := d.Run(context.Background(), feed(context.Background(), 10))
	if err == nil {
		t.Fatal("expected a joined error, got nil")
	}
	if !strings.Contains(err.Error(), "k001") || !strings.Contains(err.Error(), "k002") {
		t.Fatalf("joined error must mention both failed keys, got: %v", err)
	}
	if w := s.written.Load(); w != 8 {
		t.Fatalf("wrote %d items, want 8 (10 minus 2 failures)", w)
	}
}

func TestDistributorStopsOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	s := &gaugeSink{}
	d := &Distributor{Workers: 2, Sink: s}

	var wg sync.WaitGroup
	wg.Add(1)
	var err error
	go func() {
		defer wg.Done()
		err = d.Run(ctx, feed(ctx, 100000))
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	wg.Wait()

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
	if w := s.written.Load(); w >= 100000 {
		t.Fatalf("cancel did not stop the pipeline early: wrote %d", w)
	}
}
```

## Review

The design is correct when the in-flight bound holds, every item is accounted for, and shutdown is clean. The bound is the headline property: the gauge test proves at most `workers` writes overlap, which is the observable signature of an unbuffered backpressure valve doing its job — if someone buffered `jobs`, the peak would climb past the worker count and the test would fail. Shutdown is correct because `Run` is the only sender and the only closer of `jobs`, closes it exactly once after the forward loop ends, and joins errors only after `wg.Wait`. Cancellation is real because both legs of the forwarding select watch `ctx.Done()`, so neither a slow producer nor a stuck sink can wedge the caller.

Common mistakes for this feature. The first is buffering the internal channel to "go faster": it only defers backpressure until the buffer fills, and a large buffer is an unbounded-memory bug wearing a performance costume. The second is closing `jobs` from a worker or before the forward loop ends, which panics a still-sending forwarder or drops items; only the single owner closes, and only after forwarding stops. The third is failing fast on the first write error and abandoning the rest of the stream, when a write pipeline almost always wants to attempt every item and report the failures. The fourth is forwarding without watching the context, so a sink that wedges forever takes the caller down with it.

## Resources

- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines) — the bounded pipeline and explicit cancellation this write distributor is built from.
- [`errors.Join`](https://pkg.go.dev/errors#Join) — the aggregator `Run` returns; `nil` when no write failed.
- [Go Blog: Rob Pike — Concurrency is not parallelism](https://go.dev/blog/waza-talk) — the framing of channels as the coordination primitive that makes backpressure a structural property, not a manual throttle.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [02-parallel-record-enrichment.md](02-parallel-record-enrichment.md)
