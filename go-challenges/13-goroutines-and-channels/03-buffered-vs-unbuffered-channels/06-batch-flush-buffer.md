# Exercise 6: Size-or-Time Batching Buffer (flush on full buffer or ticker)

Per-write cost dominates when you insert rows one at a time or export metrics point by
point. The fix is batching: accumulate items and flush them together — a bulk insert,
a single export call — when the batch reaches a size limit *or* a time limit, whichever
comes first. The buffer capacity and the flush interval are the two knobs that trade
latency against throughput. This exercise builds that batcher and tests its timing
deterministically with `testing/synctest`.

This module is fully self-contained.

## What you'll build

```text
batcher/                     module: example.com/batcher
  go.mod                     go 1.26
  batcher.go                 type Batcher[T]; New, Submit, Run(ctx) (size-or-time flush)
  cmd/
    demo/
      main.go                submit 6 items with maxSize 3, print two full batches
  batcher_test.go            synctest: size-flush, ticker-flush, ctx-cancel final flush
```

- Files: `batcher.go`, `cmd/demo/main.go`, `batcher_test.go`.
- Implement: `Batcher[T]` with `Submit(item)`, and `Run(ctx)` that flushes a batch to a sink when it reaches `maxSize` OR a flush interval elapses, and flushes any remainder on `ctx` cancel.
- Test: feed exactly `maxSize` and assert one full-size batch flushes without waiting for the ticker; feed fewer and assert the ticker triggers a partial flush; cancel and assert the final partial batch is flushed (no loss).
- Verify: `go test -count=1 -race ./...`

Set up the module. `testing/synctest` needs Go 1.25+, so pin the language version:

```bash
mkdir -p ~/go-exercises/batcher/cmd/demo
cd ~/go-exercises/batcher
go mod init example.com/batcher
go mod edit -go=1.26
```

### Why one goroutine owns the pending slice, and capacity is the latency knob

The batcher is a single background goroutine (`Run`) that owns a `pending` slice —
because one owner means no lock on the hot accumulation path. Producers call `Submit`,
which sends into a buffered `in` channel; `Run` selects over three events: an incoming
item, a ticker tick, and context cancellation. On an item it appends and, if `pending`
has reached `maxSize`, flushes immediately (the *size* trigger). On a tick it flushes
whatever is pending (the *time* trigger), so a half-full batch does not wait forever
during a lull. On cancellation it flushes the remainder and returns — this is the
no-data-loss guarantee for shutdown: whatever was accepted but not yet flushed still
gets written.

The two knobs encode a latency/throughput trade-off. `maxSize` is the throughput knob:
larger batches amortize per-write overhead better but each item waits longer for the
batch to fill. `interval` is the latency ceiling: it bounds how long an item can sit
before it is flushed even if the batch never fills. A high-throughput metrics exporter
uses a large `maxSize` and a generous interval; a latency-sensitive path uses a small
`maxSize` and a tight interval. The `in` channel's own buffer (`maxSize`) is a small
smoother so `Submit` does not rendezvous with `Run` on every single item.

`flush` copies `pending` into a fresh slice before handing it to the sink, then resets
`pending` to length zero while keeping its capacity — so the next batch reuses the same
backing array and the sink never observes a slice the batcher will mutate underneath
it. `time.NewTicker` must be stopped (`defer ticker.Stop()`) or it leaks a runtime
timer.

Testing timing is the hard part, and `testing/synctest` makes it exact: inside a bubble
the ticker runs on a fake clock that only advances when every goroutine is durably
blocked, so "the ticker fired after one interval" is deterministic and instant rather
than a real sleep you hope is long enough. The sink writes each batch to a channel; the
test receives from it, which also synchronizes cleanly under `-race`.

Create `batcher.go`:

```go
package batcher

import (
	"context"
	"time"
)

// Batcher accumulates submitted items and flushes them to sink in batches, whenever
// the batch reaches maxSize or the flush interval elapses. A single Run goroutine
// owns the pending slice, so no lock is needed on the accumulation path.
type Batcher[T any] struct {
	in       chan T
	maxSize  int
	interval time.Duration
	sink     func([]T)
}

// New returns a batcher that flushes at maxSize items or every interval, whichever
// comes first, calling sink with each batch.
func New[T any](maxSize int, interval time.Duration, sink func([]T)) *Batcher[T] {
	return &Batcher[T]{
		in:       make(chan T, maxSize),
		maxSize:  maxSize,
		interval: interval,
		sink:     sink,
	}
}

// Submit hands an item to the batcher. It blocks only if the in-channel buffer is
// full (the Run loop is momentarily behind).
func (b *Batcher[T]) Submit(item T) { b.in <- item }

// Run drives the batcher until ctx is cancelled, at which point it flushes any
// remaining pending items so nothing accepted is lost.
func (b *Batcher[T]) Run(ctx context.Context) {
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()

	pending := make([]T, 0, b.maxSize)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		batch := make([]T, len(pending))
		copy(batch, pending)
		b.sink(batch)
		pending = pending[:0]
	}

	for {
		select {
		case item := <-b.in:
			pending = append(pending, item)
			if len(pending) >= b.maxSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			flush()
			return
		}
	}
}
```

### The runnable demo

The demo submits 6 items with `maxSize` 3 and a very long interval, so only the size
trigger fires: two full batches of 3, in order.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/batcher"
)

func main() {
	batches := make(chan []int, 8)
	b := batcher.New(3, time.Hour, func(batch []int) { batches <- batch })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	for i := 1; i <= 6; i++ {
		b.Submit(i)
	}

	b1 := <-batches
	b2 := <-batches
	fmt.Printf("batch1 size=%d %v\n", len(b1), b1)
	fmt.Printf("batch2 size=%d %v\n", len(b2), b2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch1 size=3 [1 2 3]
batch2 size=3 [4 5 6]
```

### Tests

`TestSizeFlush` runs in a bubble with a one-hour interval, submits exactly `maxSize`
items, and asserts a single full batch flushes on the size trigger — no ticker needed.
`TestTickerFlush` submits fewer than `maxSize`, advances virtual time by one interval,
and asserts the partial batch flushes on the time trigger; `synctest.Wait` pins the
background goroutine before the assertion. `TestContextCancelFlushesRemainder` submits a
partial batch and cancels the context, asserting the remainder is flushed with no loss.
Because the sink delivers batches over a channel, everything is race-clean.

Create `batcher_test.go`:

```go
package batcher

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

func TestSizeFlush(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		batches := make(chan []int, 4)
		b := New(3, time.Hour, func(batch []int) { batches <- batch })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go b.Run(ctx)

		for i := 1; i <= 3; i++ {
			b.Submit(i)
		}
		synctest.Wait()

		select {
		case batch := <-batches:
			if len(batch) != 3 {
				t.Fatalf("batch size = %d, want 3", len(batch))
			}
		default:
			t.Fatal("no batch flushed on reaching maxSize")
		}
	})
}

func TestTickerFlush(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		batches := make(chan []int, 4)
		b := New(100, time.Second, func(batch []int) { batches <- batch })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go b.Run(ctx)

		b.Submit(1)
		b.Submit(2)
		b.Submit(3)
		synctest.Wait() // Run has consumed all three and is blocked on select

		time.Sleep(time.Second) // fire the ticker in virtual time
		synctest.Wait()

		select {
		case batch := <-batches:
			if len(batch) != 3 {
				t.Fatalf("ticker batch size = %d, want 3", len(batch))
			}
		default:
			t.Fatal("ticker did not flush the partial batch")
		}
	})
}

func TestContextCancelFlushesRemainder(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		batches := make(chan []int, 4)
		b := New(10, time.Hour, func(batch []int) { batches <- batch })

		ctx, cancel := context.WithCancel(context.Background())
		go b.Run(ctx)

		b.Submit(1)
		b.Submit(2)
		synctest.Wait()

		cancel()
		synctest.Wait()

		select {
		case batch := <-batches:
			if len(batch) != 2 {
				t.Fatalf("final flush size = %d, want 2 (data lost on shutdown)", len(batch))
			}
		default:
			t.Fatal("remainder not flushed on context cancel")
		}
	})
}

func ExampleBatcher() {
	batches := make(chan []int, 4)
	b := New(3, time.Hour, func(batch []int) { batches <- batch })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	for i := 1; i <= 3; i++ {
		b.Submit(i)
	}
	fmt.Println(<-batches) // the size trigger flushes a full batch of 3, in order
	// Output: [1 2 3]
}
```

## Review

The batcher is correct when a batch flushes on the size trigger without waiting for
the ticker, on the ticker during a lull without waiting to fill, and on context cancel
so no accepted item is lost. Keeping the `pending` slice owned by the single `Run`
goroutine removes the need for a lock on the accumulation path; `flush` copying into a
fresh slice keeps the sink from racing a slice the batcher will reuse. The synctest
bubble is what makes the timing assertions exact instead of flaky: the ticker fires at
exactly one virtual interval, and `synctest.Wait` guarantees the background goroutine
has reacted before the test reads the result. Do not forget `ticker.Stop()` — a leaked
ticker is a leaked runtime timer.

## Resources

- [pkg.go.dev: time.NewTicker](https://pkg.go.dev/time#NewTicker) — the ticker and why `Stop` is mandatory.
- [pkg.go.dev: testing/synctest](https://pkg.go.dev/testing/synctest) — deterministic virtual-time testing of tickers and timeouts.
- [The Go Blog: Testing concurrent code with testing/synctest](https://go.dev/blog/synctest) — the bubble, the fake clock, and `synctest.Wait`.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [05-concurrency-limiter-token-buffer.md](05-concurrency-limiter-token-buffer.md) | Next: [07-graceful-shutdown-drain.md](07-graceful-shutdown-drain.md)
