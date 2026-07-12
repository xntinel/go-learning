# Exercise 6: Size-or-time batch flusher for a write-behind buffer

A log shipper, a metrics buffer, a bulk database writer — all share one shape:
accumulate items and flush when *either* the batch is full *or* a timer fires,
whichever comes first. This is the canonical `select`-plus-timer production
pattern. This exercise builds a batcher over an input channel that flushes on
`maxSize` or on a reusable flush timer, loses nothing, and drains the final
partial batch on close.

## What you'll build

```text
batcher/                      module example.com/batcher
  go.mod
  batcher.go                  Batch[T](in, maxSize, interval, flush)
  cmd/demo/main.go            size-triggered flushes plus a final partial
  batcher_test.go             size-flush, time-flush, close-partial, no-loss stress
```

Files: `batcher.go`, `cmd/demo/main.go`, `batcher_test.go`.
Implement: `Batch[T any](in <-chan T, maxSize int, interval time.Duration, flush func([]T))`.
Test: burst over `maxSize` gives full batches; slow trickle gives a time-flush of exactly the buffered items; close flushes the partial; 500 items across many flushes are neither dropped nor duplicated.
Verify: `go test -count=1 -race ./...`

### Two triggers, one timer, one copy

`Batch` runs a loop with a `select` over two cases: an item arrived on `in`, or the
flush timer fired. On an item it appends to the current batch; if the batch has
reached `maxSize` it flushes immediately and re-arms the timer so the next batch
gets a full interval. On a timer fire it flushes whatever has accumulated (possibly
nothing) and re-arms. When `in` closes, it flushes the final partial batch and
returns. One `time.NewTimer(interval)` is created before the loop and reused; a
`time.After` per iteration would allocate a timer on every item.

Two details make it correct under concurrency. First, `flush` receives a *copy* of
the batch slice, not the internal buffer: after calling `flush`, the loop truncates
its buffer with `batch = batch[:0]` and keeps appending into the same backing
array, so handing `flush` the live slice would let the next appends overwrite data
the consumer is still reading. The copy severs that aliasing. Second, the size-flush
path re-arms the timer with the portable Stop-drain-Reset dance because the timer
has *not* fired yet (the flush was size-triggered), so a stale tick could otherwise
be pending; the time-flush path only needs a plain `Reset` because its fire was
already consumed by the `select` case, but it uses the same helper for uniformity.

The contract guarantees every item is flushed exactly once. A burst larger than
`maxSize` produces back-to-back full batches with no partial lost between them; a
trickle under `maxSize` is flushed by the timer; a close flushes the tail. Nothing
is dropped and nothing is duplicated, which the stress test verifies by counting
each value.

Create `batcher.go`:

```go
package batcher

import "time"

// Batch accumulates items from in and calls flush with a copy of the batch when
// it reaches maxSize or when interval elapses since the last flush, whichever
// comes first. On close of in it flushes any remaining items and returns. flush
// is called only with non-empty batches. One timer is created and reused.
func Batch[T any](in <-chan T, maxSize int, interval time.Duration, flush func([]T)) {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	batch := make([]T, 0, maxSize)

	rearm := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(interval)
	}

	doFlush := func() {
		if len(batch) == 0 {
			return
		}
		out := make([]T, len(batch))
		copy(out, batch)
		flush(out)
		batch = batch[:0]
	}

	for {
		select {
		case v, ok := <-in:
			if !ok {
				doFlush()
				return
			}
			batch = append(batch, v)
			if len(batch) >= maxSize {
				doFlush()
				rearm()
			}
		case <-timer.C:
			doFlush()
			timer.Reset(interval)
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/batcher"
)

func main() {
	in := make(chan string)
	go func() {
		for _, s := range []string{"a", "b", "c", "d", "e"} {
			in <- s
		}
		close(in)
	}()

	// maxSize 2, huge interval: only size-flushes, plus the final partial on close.
	batcher.Batch(in, 2, time.Hour, func(b []string) {
		fmt.Printf("flush %v\n", b)
	})
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flush [a b]
flush [c d]
flush [e]
```

### Tests

`TestSizeFlush` sends six items with `maxSize` 3 and a one-hour interval, so only
the size trigger can fire: exactly two full batches, no time-flush. `TestTimeFlush`
sends two items with a large `maxSize` and a short interval, so the timer flushes
the two buffered items. `TestClosePartial` sends three items with `maxSize` 10 and a
huge interval, so only the close flushes the partial tail. `TestNoLossManyFlushes`
streams 500 items through a small size and a short interval, mixing both triggers,
and asserts every value was flushed exactly once — the strongest correctness
property. All use a mutex-guarded collector because `flush` runs on the batcher
goroutine.

Create `batcher_test.go`:

```go
package batcher

import (
	"sync"
	"testing"
	"time"
)

func TestSizeFlush(t *testing.T) {
	t.Parallel()
	in := make(chan int)
	var mu sync.Mutex
	var batches [][]int
	done := make(chan struct{})
	go func() {
		Batch(in, 3, time.Hour, func(b []int) {
			mu.Lock()
			batches = append(batches, b)
			mu.Unlock()
		})
		close(done)
	}()
	for i := range 6 {
		in <- i
	}
	close(in)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2", len(batches))
	}
	for _, b := range batches {
		if len(b) != 3 {
			t.Fatalf("batch size %d, want 3", len(b))
		}
	}
}

func TestTimeFlush(t *testing.T) {
	t.Parallel()
	in := make(chan int)
	var mu sync.Mutex
	var got []int
	done := make(chan struct{})
	go func() {
		Batch(in, 100, 40*time.Millisecond, func(b []int) {
			mu.Lock()
			got = append(got, b...)
			mu.Unlock()
		})
		close(done)
	}()
	in <- 1
	in <- 2
	time.Sleep(120 * time.Millisecond) // let the timer flush

	mu.Lock()
	n := len(got)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("after time-flush got %d items, want 2", n)
	}
	close(in)
	<-done
}

func TestClosePartial(t *testing.T) {
	t.Parallel()
	in := make(chan int)
	var mu sync.Mutex
	var got []int
	done := make(chan struct{})
	go func() {
		Batch(in, 10, time.Hour, func(b []int) {
			mu.Lock()
			got = append(got, b...)
			mu.Unlock()
		})
		close(done)
	}()
	in <- 1
	in <- 2
	in <- 3
	close(in)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
}

func TestNoLossManyFlushes(t *testing.T) {
	t.Parallel()
	in := make(chan int)
	var mu sync.Mutex
	seen := make(map[int]int)
	done := make(chan struct{})
	go func() {
		Batch(in, 7, 5*time.Millisecond, func(b []int) {
			mu.Lock()
			for _, v := range b {
				seen[v]++
			}
			mu.Unlock()
		})
		close(done)
	}()
	const n = 500
	for i := range n {
		in <- i
	}
	close(in)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("saw %d distinct values, want %d", len(seen), n)
	}
	for v, c := range seen {
		if c != 1 {
			t.Fatalf("value %d flushed %d times, want 1", v, c)
		}
	}
}
```

## Review

The batcher is correct when every item is flushed exactly once and each `flush`
call sees an isolated slice. The two failure modes to guard against are aliasing —
passing the live `batch` slice to `flush` so the next `append` corrupts it, which
`-race` plus the no-loss count would expose — and losing the tail by returning on
close without a final `doFlush`. The size-flush path must re-arm the timer with the
Stop-drain-Reset guard because that fire has not happened yet; skipping it could
leave a stale tick that fires the next flush early. Run `go test -race` with the
concurrent producer to confirm the single batcher goroutine and the guarded
collector never race.

## Resources

- [`time.Timer.Reset`](https://pkg.go.dev/time#Timer.Reset) — re-arming the flush timer after each flush.
- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) — the reused one-shot timer.
- [Effective Go: channels](https://go.dev/doc/effective_go#channels) — the select-over-channels loop shape.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-retry-backoff-total-budget.md](05-retry-backoff-total-budget.md) | Next: [07-heartbeat-liveness-monitor.md](07-heartbeat-liveness-monitor.md)
