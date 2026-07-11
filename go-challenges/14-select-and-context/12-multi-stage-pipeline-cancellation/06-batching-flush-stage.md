# Exercise 6: Batching Stage — Flush On Size Or Timer, Reusing time.NewTimer Correctly

Per-item writes are expensive: a bulk `INSERT` of 100 rows costs far less than 100
single-row inserts, and a batched broker publish amortizes the round-trip. A
batching stage coalesces individual items and flushes a batch when it reaches
`maxSize` *or* when a flush interval elapses (so a trickle of items is not held
forever), and it flushes whatever partial batch remains on shutdown. The trap is
timer management: this module reuses one `time.NewTimer` with the correct
Stop-drain-Reset dance, directly fixing the "`time.After` inside a loop" mistake.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
batch/                       module example.com/batch
  go.mod                     go 1.25 (testing/synctest needs it)
  batch.go                   func Batch(ctx, in, maxSize, interval) <-chan []int
  cmd/
    demo/
      main.go                size-triggered and timer-triggered flushes, printed
  batch_test.go              size flush, timer flush, final partial, synctest timer-reset proof
```

Files: `batch.go`, `cmd/demo/main.go`, `batch_test.go`.
Implement: `Batch(ctx, in, maxSize, interval) <-chan []int` — accumulate items and
emit a batch on reaching `maxSize` or on the flush timer firing, flushing the final
partial batch when `in` closes or `ctx` is cancelled.
Test: size-triggered full batches, timer-triggered partial batch when input pauses,
final partial flushed on close, and no timer misbehavior across cycles.
Verify: `go test -count=1 -race ./...`

Set up the module. The timer-reset test drives virtual time under a
`testing/synctest` bubble, which requires Go 1.25+, so pin the language version:

```bash
mkdir -p ~/go-exercises/batch/cmd/demo
cd ~/go-exercises/batch
go mod init example.com/batch
go mod edit -go=1.25
```

### The single-timer flush loop, and why Stop-drain-Reset

The stage keeps a growing `buf []int`. Its `select` has three arms: a receive from
`in`, a fire from the flush `timer.C`, and `ctx.Done()`. On a received item it
appends; if `buf` reached `maxSize` it flushes immediately (a full batch does not
wait for the timer). On a timer fire it flushes whatever partial batch has
accumulated — this is what bounds latency when items trickle in below the size
threshold. On `ctx.Done()` (or a closed `in`) it flushes the final partial batch
and returns.

The timer is created *once*, outside the loop, with `time.NewTimer(interval)`.
Reusing it is where the sharp edges live:

- After a flush, you want to restart the interval. `Reset` on a timer that has
  *already fired* but whose value you have *not yet drained* leaves a stale value
  in `timer.C`, so the next `select` wakes immediately on the old fire. The safe
  restart is: `Stop()`; if it returned `false` (already fired), drain the channel
  with a non-blocking `select { case <-timer.C: default: }`; then `Reset(interval)`.
- When the *timer arm itself* fires, its value is already consumed by the `select`,
  so you `Reset` without draining. The drain guard is only needed on the paths that
  restart the timer *without* having consumed a fire — i.e. the size-flush path.

Encapsulating this in a small `resetTimer` helper keeps the loop readable and the
dance in exactly one place. `time.After` inside the loop would allocate a fresh
timer every iteration and never stop the old ones; one reused timer is O(1)
allocations for the life of the stage.

The output batches are freshly allocated slices, not re-slices of `buf`: after a
flush the stage does `buf = nil` (or a fresh make) so the emitted slice is not
mutated by subsequent appends. Sending a re-slice of a buffer you keep appending to
is a classic data race the `-race` detector would flag.

Create `batch.go`:

```go
package batch

import (
	"context"
	"time"
)

// Batch coalesces items from in into batches, emitting a batch when it reaches
// maxSize or when interval elapses since the last flush, whichever comes first.
// The final partial batch is flushed when in closes or ctx is cancelled. Emitted
// slices are freshly allocated and safe for the receiver to retain.
func Batch(ctx context.Context, in <-chan int, maxSize int, interval time.Duration) <-chan []int {
	out := make(chan []int)
	go func() {
		defer close(out)

		buf := make([]int, 0, maxSize)
		timer := time.NewTimer(interval)
		defer timer.Stop()

		// resetTimer restarts the interval, draining a pending fire if the timer
		// had already fired without its value being consumed.
		resetTimer := func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(interval)
		}

		flush := func() bool {
			if len(buf) == 0 {
				return true
			}
			batch := make([]int, len(buf))
			copy(batch, buf)
			buf = buf[:0]
			select {
			case out <- batch:
				return true
			case <-ctx.Done():
				return false
			}
		}

		for {
			select {
			case v, ok := <-in:
				if !ok {
					flush() // final partial batch on normal end of input
					return
				}
				buf = append(buf, v)
				if len(buf) >= maxSize {
					if !flush() {
						return
					}
					resetTimer()
				}
			case <-timer.C:
				if !flush() {
					return
				}
				timer.Reset(interval) // fire already consumed by select; no drain
			case <-ctx.Done():
				flush()
				return
			}
		}
	}()
	return out
}
```

### The runnable demo

The demo sends five items fast (so a size-3 batch flushes immediately, then a
partial of two waits for the timer) and prints each emitted batch. It uses a real
short interval so you can watch both a size flush and a timer flush.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/batch"
)

func main() {
	in := make(chan int)
	go func() {
		defer close(in)
		for i := 1; i <= 5; i++ {
			in <- i
		}
		time.Sleep(60 * time.Millisecond) // let the timer flush the partial
	}()

	out := batch.Batch(context.Background(), in, 3, 30*time.Millisecond)

	n := 0
	for b := range out {
		n++
		fmt.Printf("batch %d: %v (len %d)\n", n, b, len(b))
	}
	fmt.Printf("total batches=%d\n", n)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
batch 1: [1 2 3] (len 3)
batch 2: [4 5] (len 2)
total batches=2
```

### Tests

`TestSizeFlush` feeds an exact multiple of `maxSize` and asserts full batches.
`TestTimerFlush` sends fewer than `maxSize` items then pauses, and asserts a partial
batch is emitted by the timer rather than being held. `TestFinalPartialOnClose`
closes the input mid-batch and asserts the remainder is flushed.
`TestManyCyclesNoItemLoss` runs many rapid size-flush cycles and asserts every item
is accounted for — no item is lost and no empty batch is emitted across the
Stop-drain-Reset path. `TestTimerResetAfterSizeFlush` drives the timer under a
`testing/synctest` bubble so the interval is exact, and pins that a size flush
re-arms the interval timer: the trailing partial item is emitted at exactly one
interval after the size flush, which a reset that failed to restart the timer would
delay until close.

Create `batch_test.go`:

```go
package batch

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestSizeFlush(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	go func() {
		defer close(in)
		for i := 1; i <= 6; i++ {
			in <- i
		}
	}()

	out := Batch(context.Background(), in, 3, time.Hour) // huge interval: only size flushes
	var batches [][]int
	for b := range out {
		batches = append(batches, b)
	}
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2: %v", len(batches), batches)
	}
	for i, b := range batches {
		if len(b) != 3 {
			t.Fatalf("batch %d len = %d, want 3", i, len(b))
		}
	}
}

func TestTimerFlush(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	go func() {
		defer close(in)
		in <- 1
		in <- 2 // below maxSize (5); only the timer can flush these
		time.Sleep(80 * time.Millisecond)
	}()

	out := Batch(context.Background(), in, 5, 20*time.Millisecond)
	first := <-out // blocks until the timer flushes the partial batch
	if len(first) != 2 {
		t.Fatalf("timer batch = %v, want 2 items", first)
	}
	for range out { // drain the rest
	}
}

func TestFinalPartialOnClose(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	go func() {
		defer close(in)
		in <- 1
		in <- 2
		in <- 3
		in <- 4 // 4th item: one full batch of 3 flushes, 1 remains
	}()

	out := Batch(context.Background(), in, 3, time.Hour)
	total := 0
	for b := range out {
		total += len(b)
	}
	if total != 4 {
		t.Fatalf("total items across batches = %d, want 4", total)
	}
}

func TestManyCyclesNoItemLoss(t *testing.T) {
	t.Parallel()

	in := make(chan int)
	go func() {
		defer close(in)
		for i := 0; i < 30; i++ {
			in <- i
		}
	}()

	out := Batch(context.Background(), in, 3, 5*time.Millisecond)
	total := 0
	for b := range out {
		if len(b) == 0 {
			t.Fatalf("emitted an empty batch")
		}
		total += len(b)
	}
	if total != 30 {
		t.Fatalf("total = %d, want 30 (a mishandled timer reset dropped items)", total)
	}
}

// TestTimerResetAfterSizeFlush drives the timer under a synctest bubble so the
// interval is exact. It proves the size-flush path re-arms the interval timer: a
// full batch flushes on size at t=0, and the trailing partial item can only leave
// the stage when the restarted timer fires exactly one interval later. A resetTimer
// that failed to restart the timer would strand item 4 until close, moving its
// emission from t=interval to t=2*interval, which the exact virtual-time assertion
// catches.
func TestTimerResetAfterSizeFlush(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const interval = 100 * time.Millisecond

		type stamped struct {
			batch []int
			at    time.Duration
		}

		in := make(chan int)
		out := Batch(context.Background(), in, 3, interval)
		start := time.Now()

		got := make(chan stamped, 8)
		go func() {
			for b := range out {
				got <- stamped{batch: b, at: time.Since(start)}
			}
			close(got)
		}()

		in <- 1
		in <- 2
		in <- 3 // size flush of [1 2 3] at t=0; resetTimer restarts the interval
		in <- 4 // partial; only the restarted timer can flush it

		time.Sleep(2 * interval) // virtual: fires the timer, returns immediately
		close(in)

		var batches []stamped
		for s := range got {
			batches = append(batches, s)
		}

		if len(batches) != 2 {
			t.Fatalf("got %d batches, want 2: %v", len(batches), batches)
		}
		if len(batches[0].batch) != 3 {
			t.Fatalf("first batch = %v, want 3 items", batches[0].batch)
		}
		if len(batches[1].batch) != 1 || batches[1].batch[0] != 4 {
			t.Fatalf("second batch = %v, want [4]", batches[1].batch)
		}
		if batches[1].at != interval {
			t.Fatalf("item 4 flushed at %v, want exactly %v (timer not reset after size flush)",
				batches[1].at, interval)
		}
	})
}
```

## Review

The batching stage is correct when a full batch flushes on size without waiting,
a partial batch flushes on the interval so trickle traffic is not stranded, the
final partial is flushed on close or cancel, and every size-flush cycle re-arms the
interval timer cleanly. The Stop-drain-Reset helper is the crux: `Reset` on a timer
whose earlier fire has not been drained can leave a stale value in `timer.C`, so the
next `select` wakes on the old fire instead of after a fresh interval; the
`Stop()`-then-drain guard removes that stale value on the size-flush path, where the
loop restarts the timer without having consumed a fire. `TestTimerResetAfterSizeFlush`
pins the restart under `testing/synctest`: the trailing item leaves exactly one
interval after a size flush, so a reset that dropped the `Reset` (stranding the item
until close) fails the exact virtual-time assertion. Emitting a copied slice rather
than a re-slice of `buf` keeps `-race` clean; sending `buf[:0:0]` re-slices would let
a later append mutate a batch the receiver still holds.

## Resources

- [`time.Timer.Reset`](https://pkg.go.dev/time#Timer.Reset) — the documented Stop-then-drain-then-Reset requirement.
- [`time.NewTimer`](https://pkg.go.dev/time#NewTimer) — one reusable timer versus a fresh `time.After` per iteration.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — virtual time for asserting the exact interval a reset timer fires at.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — stage shape and cancellation on the flush path.

---

Back to [05-bounded-errgroup-pipeline.md](05-bounded-errgroup-pipeline.md) | Next: [07-rate-limited-egress-stage.md](07-rate-limited-egress-stage.md)
