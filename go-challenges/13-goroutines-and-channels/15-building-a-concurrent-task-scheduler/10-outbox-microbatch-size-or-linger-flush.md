# Exercise 10: Micro-Batch Dispatcher: Flush by Size or Linger

**Level: Intermediate**

An outbox relay ships domain events to a batch-oriented sink — a bulk DB insert or an `S3 PutObjects` call — where one row per round-trip wastes the downstream and caps throughput. The naive fix, "flush every N events," starves latency when traffic is thin: the last few events sit forever waiting for the batch to fill. This module builds the size-or-time trigger that a Kafka producer's `linger.ms` implements: accumulate events and flush a batch when it reaches `maxBatch` or when a linger deadline elapses since the batch's first event, whichever comes first.

This module is self-contained: its own module, an `outbox` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
outbox/                      independent module: example.com/outbox
  go.mod                     go 1.26
  outbox.go                  Batcher: single dispatcher owns the buffer + one re-armable timer
  cmd/demo/main.go           runnable demo: fill by size, drain the tail on Close
  outbox_test.go             size-beats-linger, linger flush, conservation, serial flush, timer re-arm
```

- Files: `outbox.go`, `cmd/demo/main.go`, `outbox_test.go`.
- Implement: `New(maxBatch int, linger time.Duration, flush FlushFunc) *Batcher`, `(*Batcher).Add(ctx, e) error`, `(*Batcher).Flush(ctx) error`, `(*Batcher).Close(ctx) error`, `(*Batcher).Stats() Stats`.
- Test: a full batch flushes on size before the linger deadline; a partial batch flushes on linger; every Added event lands in exactly one batch; `FlushFunc` is never re-entered; the timer re-arms per batch.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/15-building-a-concurrent-task-scheduler/10-outbox-microbatch-size-or-linger-flush/cmd/demo
cd go-solutions/13-goroutines-and-channels/15-building-a-concurrent-task-scheduler/10-outbox-microbatch-size-or-linger-flush
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### One goroutine owns the buffer and the timer

The whole design turns on single ownership. The pending buffer (`[]Event`) and the linger timer are touched by exactly one goroutine — the dispatcher started in `New`. Producers never append to the slice or read the timer; they hand an event to the dispatcher over a channel and wait for an acknowledgement. Because one goroutine is the sole reader and writer of both, there is no lock, no data race is even structurally possible, and `FlushFunc` is invoked serially by construction: every call site of the internal `doFlush` lives inside the dispatcher's single `select` loop, so two flushes can never overlap.

The trigger logic has two rules and one trap. Rule one: the size trigger. When appending an event makes `len(pending) >= maxBatch`, flush immediately and synchronously, inside the dispatcher — do not wait for the timer. Rule two: the linger trigger. When an event is appended to an *empty* buffer (`len(pending) == 1`), arm the timer for `linger`. That deadline is measured from the batch's *first* event. The trap is re-arming: later events in the same batch must **not** reset the timer, or a steady trickle of Adds would push the deadline out forever and the batch would linger past its bound. The timer arms once per batch, on the first event; every flush — by size, by timer, or by `Close` — clears the buffer and disarms, and the next first event arms a fresh deadline.

The protocol the dispatcher runs, per `select` iteration:

1. **Add arrives.** Append the event. If it is the first, `Reset` the timer to `linger`. Acknowledge the producer. If the buffer is now full, flush it now.
2. **Flush request arrives.** Ship the current partial batch immediately (no-op if empty) and reply with the sink's error.
3. **Timer fires.** The linger deadline elapsed; ship the partial batch.
4. **Stop signal arrives.** Ship the final partial batch, then return so the goroutine joins.

Under Go 1.23+ timer semantics, `Stop` and `Reset` require no manual channel drain: once `Stop` returns, no stale value is delivered, so re-arming is a plain `Reset`. `doFlush` disarms with `Stop` before shipping, so a batch that fills by size does not later trigger a spurious empty timer flush.

Create `outbox.go`:

```go
package outbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned by Add and Flush after Close has begun.
var ErrClosed = errors.New("outbox: batcher closed")

// Event is a single domain event queued for shipment.
type Event any

// FlushFunc ships one accumulated batch to the sink. The dispatcher guarantees
// it is invoked serially: FlushFunc is never called concurrently with itself.
type FlushFunc func(ctx context.Context, batch []Event) error

// Stats is a snapshot of dispatcher totals.
type Stats struct {
	Batches, Events int64
}

type addReq struct {
	e     Event
	reply chan error
}

type flushReq struct {
	ctx   context.Context
	reply chan error
}

// Batcher accumulates events and flushes a batch when it reaches maxBatch or
// when linger elapses since the batch's first event, whichever comes first. A
// single dispatcher goroutine owns the pending buffer and the timer, so no lock
// guards them and FlushFunc runs serially.
type Batcher struct {
	maxBatch int
	linger   time.Duration
	flush    FlushFunc

	addCh   chan addReq
	flushCh chan flushReq
	stopCh  chan struct{}
	closed  chan struct{} // fences out Add once Close begins
	done    chan struct{} // closed when the dispatcher goroutine exits

	closeOnce sync.Once
	closeCtx  context.Context

	batches atomic.Int64
	events  atomic.Int64
}

// New starts the dispatcher goroutine. maxBatch must be >= 1 and linger > 0.
func New(maxBatch int, linger time.Duration, flush FlushFunc) *Batcher {
	if maxBatch < 1 {
		maxBatch = 1
	}
	if linger <= 0 {
		linger = time.Millisecond
	}
	b := &Batcher{
		maxBatch: maxBatch,
		linger:   linger,
		flush:    flush,
		addCh:    make(chan addReq),
		flushCh:  make(chan flushReq),
		stopCh:   make(chan struct{}),
		closed:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	go b.run()
	return b
}

func (b *Batcher) run() {
	defer close(b.done)

	pending := make([]Event, 0, b.maxBatch)
	// A stopped, re-armable timer. Under Go 1.23+ semantics Stop/Reset need no
	// manual channel drain: after Stop returns, no stale value is delivered.
	timer := time.NewTimer(b.linger)
	timer.Stop()
	armed := false
	defer timer.Stop()

	doFlush := func(ctx context.Context) error {
		if len(pending) == 0 {
			return nil
		}
		if armed {
			timer.Stop()
			armed = false
		}
		batch := pending
		pending = make([]Event, 0, b.maxBatch)
		err := b.flush(ctx, batch)
		b.batches.Add(1)
		b.events.Add(int64(len(batch)))
		return err
	}

	for {
		select {
		case req := <-b.addCh:
			pending = append(pending, req.e)
			if len(pending) == 1 {
				// Arm the linger deadline from the batch's FIRST event only;
				// later events in the same batch must not push the deadline out.
				timer.Reset(b.linger)
				armed = true
			}
			req.reply <- nil
			if len(pending) >= b.maxBatch {
				doFlush(context.Background())
			}
		case fr := <-b.flushCh:
			fr.reply <- doFlush(fr.ctx)
		case <-timer.C:
			armed = false
			doFlush(context.Background())
		case <-b.stopCh:
			doFlush(b.closeCtx)
			return
		}
	}
}

// Add enqueues e. It respects ctx while waiting for the dispatcher and returns
// ErrClosed once Close has begun. A nil return means e was accepted and will
// appear in exactly one flushed batch.
func (b *Batcher) Add(ctx context.Context, e Event) error {
	req := addReq{e: e, reply: make(chan error, 1)}
	select {
	case b.addCh <- req:
	case <-ctx.Done():
		return ctx.Err()
	case <-b.closed:
		return ErrClosed
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Flush force-ships the current partial batch now. It is a no-op if empty.
func (b *Batcher) Flush(ctx context.Context) error {
	fr := flushReq{ctx: ctx, reply: make(chan error, 1)}
	select {
	case b.flushCh <- fr:
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return ErrClosed
	}
	select {
	case err := <-fr.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting events, flushes the final partial batch, and joins the
// dispatcher goroutine. It is idempotent; ctx bounds the wait for the join.
func (b *Batcher) Close(ctx context.Context) error {
	b.closeOnce.Do(func() {
		b.closeCtx = ctx
		close(b.closed)
		close(b.stopCh)
	})
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns the running totals of batches and events flushed.
func (b *Batcher) Stats() Stats {
	return Stats{Batches: b.batches.Load(), Events: b.events.Load()}
}
```

### The runnable demo

The demo uses `maxBatch=3` and a long linger, so the size trigger fires first and the timer never elapses — the output is fully deterministic. Seven events produce two full batches by size and one tail batch drained by `Close`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/outbox"
)

func main() {
	ctx := context.Background()

	// maxBatch 3 with a long linger: the size trigger fires first and the
	// timer never elapses, so the demo is fully deterministic.
	b := outbox.New(3, 10*time.Second, func(_ context.Context, batch []outbox.Event) error {
		fmt.Printf("flush batch of %d: %v\n", len(batch), batch)
		return nil
	})

	for i := range 7 {
		if err := b.Add(ctx, i); err != nil {
			fmt.Println("add error:", err)
		}
	}

	// Close ships the final partial batch [6] and joins the dispatcher.
	if err := b.Close(ctx); err != nil {
		fmt.Println("close error:", err)
	}

	st := b.Stats()
	fmt.Printf("stats: batches=%d events=%d\n", st.Batches, st.Events)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flush batch of 3: [0 1 2]
flush batch of 3: [3 4 5]
flush batch of 1: [6]
stats: batches=3 events=7
```

### Tests

`TestSizeTriggerBeatsLinger` sets a 2 s linger and asserts a full batch of 3 signals its flush within 500 ms via a channel inside `FlushFunc` — no sleep — proving the size trigger does not wait for the deadline. `TestLingerFlushesPartial` adds one event under a 60 ms linger and asserts it ships on the timer. `TestConservation` adds 500 events across size and linger triggers plus a `Close` drain, then sorts the collected events and asserts they equal `0..499` exactly, so nothing is lost or duplicated, and cross-checks `Stats().Events`. `TestFlushNeverConcurrent` keeps an atomic `entered` gauge inside `FlushFunc`, hammered by eight concurrent Adders plus interleaved Flush calls, and asserts the max observed depth never exceeds 1. `TestTimerReArms` runs three single-event rounds and asserts each flushes on its own linger, proving the timer re-arms per batch. `TestClosedRejectsAdd` asserts `Add` returns `ErrClosed` after `Close` and that a second `Close` is a no-op. `TestMain` wraps the suite in `goleak.VerifyTestMain` so a leaked dispatcher goroutine fails the run.

Create `outbox_test.go`:

```go
package outbox

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestSizeTriggerBeatsLinger proves a full batch flushes on the size trigger
// without waiting for the linger deadline. linger is long; the flush must
// signal well before it could possibly elapse.
func TestSizeTriggerBeatsLinger(t *testing.T) {
	t.Parallel()

	const linger = 2 * time.Second
	flushed := make(chan int, 1)
	b := New(3, linger, func(_ context.Context, batch []Event) error {
		flushed <- len(batch)
		return nil
	})
	defer b.Close(context.Background())

	ctx := context.Background()
	for i := range 3 {
		if err := b.Add(ctx, i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}

	select {
	case n := <-flushed:
		if n != 3 {
			t.Fatalf("flushed batch size = %d, want 3", n)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("size-full batch did not flush before 500ms (linger is 2s)")
	}
}

// TestLingerFlushesPartial proves a partial batch ships after linger elapses.
func TestLingerFlushesPartial(t *testing.T) {
	t.Parallel()

	const linger = 60 * time.Millisecond
	flushed := make(chan []Event, 1)
	b := New(100, linger, func(_ context.Context, batch []Event) error {
		flushed <- slices.Clone(batch)
		return nil
	})
	defer b.Close(context.Background())

	if err := b.Add(context.Background(), "a"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	select {
	case batch := <-flushed:
		if len(batch) != 1 || batch[0] != "a" {
			t.Fatalf("linger batch = %v, want [a]", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("partial batch never flushed on linger")
	}
}

// TestConservation adds many events across size and linger triggers plus a
// final Close drain, then asserts every event appears in exactly one batch.
func TestConservation(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seen []int
	b := New(7, 15*time.Millisecond, func(_ context.Context, batch []Event) error {
		mu.Lock()
		for _, e := range batch {
			seen = append(seen, e.(int))
		}
		mu.Unlock()
		return nil
	})

	ctx := context.Background()
	const n = 500
	for i := range n {
		if err := b.Add(ctx, i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
		if i%50 == 0 {
			time.Sleep(time.Millisecond) // let some batches flush on linger
		}
	}
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	mu.Lock()
	got := slices.Clone(seen)
	mu.Unlock()
	slices.Sort(got)

	want := make([]int, n)
	for i := range n {
		want[i] = i
	}
	if !slices.Equal(got, want) {
		t.Fatalf("conservation violated: got %d events (deduped/sorted mismatch)", len(got))
	}

	st := b.Stats()
	if st.Events != n {
		t.Fatalf("Stats.Events = %d, want %d", st.Events, n)
	}
	if int(st.Events) != len(got) {
		t.Fatalf("Stats.Events=%d disagrees with collected=%d", st.Events, len(got))
	}
}

// TestFlushNeverConcurrent uses an entered gauge inside FlushFunc; a serial
// dispatcher must never let it exceed 1.
func TestFlushNeverConcurrent(t *testing.T) {
	t.Parallel()

	var entered atomic.Int32
	var maxSeen atomic.Int32
	b := New(4, 5*time.Millisecond, func(_ context.Context, _ []Event) error {
		cur := entered.Add(1)
		for {
			old := maxSeen.Load()
			if cur <= old || maxSeen.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(200 * time.Microsecond) // widen the window for overlap
		entered.Add(-1)
		return nil
	})

	// Hammer with concurrent Adds plus concurrent Flush calls to maximize the
	// chance of overlapping flushes if the dispatcher were not serial.
	var wg sync.WaitGroup
	ctx := context.Background()
	for range 8 {
		wg.Go(func() {
			for i := range 100 {
				_ = b.Add(ctx, i)
				if i%10 == 0 {
					_ = b.Flush(ctx)
				}
			}
		})
	}
	wg.Wait()
	if err := b.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if m := maxSeen.Load(); m > 1 {
		t.Fatalf("FlushFunc ran concurrently: max entered = %d, want 1", m)
	}
}

// TestTimerReArms proves the linger deadline is measured from each batch's
// first event and re-arms cleanly for the next batch, so consecutive partial
// batches each flush on their own linger rather than one lingering forever.
func TestTimerReArms(t *testing.T) {
	t.Parallel()

	const linger = 40 * time.Millisecond
	flushed := make(chan int, 8)
	b := New(100, linger, func(_ context.Context, batch []Event) error {
		flushed <- len(batch)
		return nil
	})
	defer b.Close(context.Background())

	ctx := context.Background()
	for round := range 3 {
		start := time.Now()
		if err := b.Add(ctx, round); err != nil {
			t.Fatalf("Add: %v", err)
		}
		select {
		case n := <-flushed:
			if n != 1 {
				t.Fatalf("round %d batch size = %d, want 1", round, n)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("round %d lingered %v, far past linger %v", round, elapsed, linger)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d never flushed; timer failed to re-arm", round)
		}
	}
}

// TestClosedRejectsAdd asserts Add returns ErrClosed after Close.
func TestClosedRejectsAdd(t *testing.T) {
	t.Parallel()

	b := New(4, 20*time.Millisecond, func(_ context.Context, _ []Event) error { return nil })
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Add(context.Background(), 1); err != ErrClosed {
		t.Fatalf("Add after Close = %v, want ErrClosed", err)
	}
	// Second Close is a no-op.
	if err := b.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
```

## Review

Correct here means three invariants hold simultaneously. First, a full batch flushes on size without waiting — guaranteed because the size check runs synchronously right after the append, inside the dispatcher, and `doFlush` disarms the timer. Second, a partial batch flushes exactly `linger` after its first event and never later — guaranteed because the timer is armed only when the buffer goes from empty to one, never reset by subsequent events, and re-armed fresh after every flush. Third, `FlushFunc` runs serially and no event is lost or duplicated — guaranteed by single ownership: one dispatcher goroutine is the sole reader and writer of the buffer and timer, so every flush is one call site inside one `select` loop, and every accepted `Add` is appended exactly once and drained exactly once, verified by summing the batches against the events added. The production bug this rules out is the trickle-starves-latency failure of a size-only batcher, where the tail of a slowing stream sits unshipped forever, plus its evil twin — a timer that resets on every Add and so never fires under steady load. Run `go test -count=1 -race ./...`.

## Resources

- [Go 1.23 timer changes](https://go.dev/doc/go1.23#timer-changes) -- why `Stop`/`Reset` no longer need a manual channel drain, which is what makes the re-armable timer here clean.
- [`time.Timer`](https://pkg.go.dev/time#Timer) -- the `Reset` and `Stop` contract for the single-owner linger timer.
- [Kafka producer `linger.ms`](https://kafka.apache.org/documentation/#producerconfigs_linger.ms) -- the industrial size-or-time batching trigger this exercise reproduces.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- asserting the dispatcher goroutine is joined and no goroutine leaks after `Close`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-scheduler-metrics-observability.md](09-scheduler-metrics-observability.md) | Next: [11-periodic-compaction-skip-if-running.md](11-periodic-compaction-skip-if-running.md)
