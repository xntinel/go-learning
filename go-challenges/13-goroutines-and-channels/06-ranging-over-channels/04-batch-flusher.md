# Exercise 4: Size-or-Timer Batch Flusher for DB Inserts

Writing one row per insert wastes a database. Batching amortizes the round trip:
accumulate items and flush them in groups. But a pure size trigger stalls the tail
when traffic is slow, so real flushers also flush on a timer to cap latency, and
always flush the final partial batch when the input closes. This exercise builds
that size-or-timer flusher.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
batchflusher/               independent module: example.com/batchflusher
  go.mod                    go 1.26
  flusher.go                type Flusher; Run(in) flushes on size OR interval
  cmd/
    demo/
      main.go               feed items, watch size and final flushes
  flusher_test.go           [3,3,1] batches, time-based flush, empty-closed, clone safety
```

Files: `flusher.go`, `cmd/demo/main.go`, `flusher_test.go`.
Implement: `Flusher[T]` with `Run(in <-chan T)` that appends to a batch, flushes when the batch reaches `maxSize` OR a `time.Ticker` fires, and flushes the final partial batch when `in` closes; the flush target is an injected `func([]T)`.
Test: seven items with `maxSize=3` flush as `[3,3,1]` including the final partial; a slow feed with a short ticker triggers a time-based flush of a partial batch; an empty-closed input flushes nothing; batches are cloned so later appends do not mutate a delivered batch.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/batchflusher/cmd/demo
cd ~/go-exercises/batchflusher
go mod init example.com/batchflusher
```

### The three flush triggers

The loop is a `for-select`, not a `range`, because it waits on two things at once:
the next item and the ticker.

- **Size** — after each append, if `len(batch) >= maxSize`, flush. This is the
  throughput trigger: under load, batches fill and flush back to back.
- **Timer** — a `time.NewTicker(interval)` fires periodically; on each tick, if
  the batch is non-empty, flush it even though it is not full. This is the latency
  ceiling: an item never waits longer than `interval` to be written. Use
  `time.NewTicker` with `defer ticker.Stop()`, never `time.Tick`, which leaks the
  underlying ticker in a long-lived consumer.
- **Close** — when `in` closes, the `ok` is false; flush the final partial batch
  and return. Forgetting this is the classic bug that silently drops the last
  fewer-than-maxSize items.

The aliasing trap: `flush` must receive a `slices.Clone` of the batch, not the
live slice. The flusher (a DB insert, an HTTP post) may hold the slice while the
consumer resets `batch = batch[:0]` and appends new items into the same backing
array — corrupting the batch the flusher is still reading. Cloning severs that
aliasing. After a flush we reset with `batch = batch[:0]` to reuse the capacity;
the clone is what makes reuse safe.

Create `flusher.go`:

```go
package batchflusher

import (
	"slices"
	"time"
)

// Flusher accumulates items and writes them in batches, flushing on size, on a
// timer, or when the input closes.
type Flusher[T any] struct {
	maxSize  int
	interval time.Duration
	flush    func([]T)
}

// New builds a Flusher. flush is the batch sink (e.g. a multi-row DB insert).
func New[T any](maxSize int, interval time.Duration, flush func([]T)) *Flusher[T] {
	return &Flusher[T]{maxSize: maxSize, interval: interval, flush: flush}
}

// Run drains in, flushing whenever the batch reaches maxSize or the interval
// elapses, and flushing the final partial batch when in is closed and drained.
func (f *Flusher[T]) Run(in <-chan T) {
	ticker := time.NewTicker(f.interval)
	defer ticker.Stop()

	var batch []T
	emit := func() {
		if len(batch) == 0 {
			return
		}
		f.flush(slices.Clone(batch)) // clone: the sink must not alias our buffer
		batch = batch[:0]
	}

	for {
		select {
		case v, ok := <-in:
			if !ok {
				emit() // final partial batch: never strand the tail
				return
			}
			batch = append(batch, v)
			if len(batch) >= f.maxSize {
				emit()
			}
		case <-ticker.C:
			emit() // latency ceiling: flush whatever has accumulated
		}
	}
}
```

### The runnable demo

The demo feeds seven items into a flusher with `maxSize=3` and a long interval (so
the timer never fires), then closes the channel. It prints each batch as the sink
receives it, showing `[3,3,1]` — two full batches and the final partial.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/batchflusher"
)

func main() {
	f := batchflusher.New(3, time.Hour, func(batch []int) {
		fmt.Printf("flush %v\n", batch)
	})

	in := make(chan int, 7)
	for i := 1; i <= 7; i++ {
		in <- i
	}
	close(in)

	f.Run(in)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
flush [1 2 3]
flush [4 5 6]
flush [7]
```

### Tests

`TestSizeAndFinalPartial` feeds seven items with `maxSize=3` and a long interval
and asserts the recorded batch sizes are exactly `[3,3,1]` — the final `1` proves
the close-flush. `TestTimerFlushesPartial` uses a short interval and feeds two
items slowly, asserting a partial batch is flushed by the timer before the input
closes. `TestEmptyClosedFlushesNothing` closes an empty input and asserts the sink
is never called. `TestCloneIsolation` captures a delivered batch and mutates the
consumer afterward to prove the sink's copy is independent.

Create `flusher_test.go`:

```go
package batchflusher

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// recorder is a thread-safe batch sink for assertions.
type recorder[T any] struct {
	mu      sync.Mutex
	batches [][]T
}

func (r *recorder[T]) sink(batch []T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batches = append(r.batches, batch)
}

func (r *recorder[T]) sizes() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, len(r.batches))
	for i, b := range r.batches {
		out[i] = len(b)
	}
	return out
}

func TestSizeAndFinalPartial(t *testing.T) {
	t.Parallel()
	var rec recorder[int]
	f := New(3, time.Hour, rec.sink) // long interval: timer never fires

	in := make(chan int, 7)
	for i := 1; i <= 7; i++ {
		in <- i
	}
	close(in)
	f.Run(in)

	got := rec.sizes()
	want := []int{3, 3, 1}
	if !slices.Equal(got, want) {
		t.Fatalf("batch sizes = %v, want %v", got, want)
	}
}

func TestTimerFlushesPartial(t *testing.T) {
	t.Parallel()
	var rec recorder[int]
	f := New(100, 20*time.Millisecond, rec.sink) // size never reached

	in := make(chan int)
	go func() {
		in <- 1
		in <- 2
		time.Sleep(80 * time.Millisecond) // let the ticker fire the partial batch
		close(in)
	}()
	f.Run(in)

	got := rec.sizes()
	if len(got) == 0 {
		t.Fatal("timer never flushed a partial batch")
	}
	if got[0] != 2 {
		t.Fatalf("first (timer) flush size = %d, want 2", got[0])
	}
}

func TestEmptyClosedFlushesNothing(t *testing.T) {
	t.Parallel()
	var rec recorder[int]
	f := New(3, time.Hour, rec.sink)

	in := make(chan int)
	close(in)
	f.Run(in)

	if n := len(rec.sizes()); n != 0 {
		t.Fatalf("flushes on empty-closed input = %d, want 0", n)
	}
}

func TestCloneIsolation(t *testing.T) {
	t.Parallel()
	var captured []int
	var once sync.Once
	f := New(2, time.Hour, func(batch []int) {
		once.Do(func() { captured = batch }) // keep the first delivered batch
	})

	in := make(chan int, 4)
	for _, v := range []int{1, 2, 3, 4} {
		in <- v
	}
	close(in)
	f.Run(in)

	// The captured batch must still read [1 2]; the second batch's appends must
	// not have mutated it (they would if the sink aliased the live buffer).
	if !slices.Equal(captured, []int{1, 2}) {
		t.Fatalf("captured batch = %v, want [1 2] (clone isolation broken)", captured)
	}
}

func ExampleFlusher_Run() {
	f := New(2, time.Hour, func(batch []int) {
		fmt.Println(batch)
	})
	in := make(chan int, 3)
	in <- 1
	in <- 2
	in <- 3
	close(in)
	f.Run(in)
	// Output:
	// [1 2]
	// [3]
}
```

## Review

The flusher is correct when every accepted item lands in exactly one batch, no
tail is stranded, and the sink never sees a mutated slice. The `[3,3,1]` assertion
is the sharpest: the trailing `1` is the whole point — a flusher that only flushes
on size would produce `[3,3]` and silently drop item seven. `TestCloneIsolation`
guards the aliasing bug that `-race` alone would not always catch, because the
corruption is a logical overwrite, not a data race. The timer test uses generous
margins (an 80 ms sleep against a 20 ms ticker) so it does not flake under the
`-race` slowdown. Keep `defer ticker.Stop()`: a `time.Tick` here would leak the
ticker for the life of every flusher.

## Resources

- [pkg.go.dev: time.NewTicker](https://pkg.go.dev/time#NewTicker) — the ticker that must be `Stop`ped; contrast with `time.Tick`.
- [pkg.go.dev: slices.Clone](https://pkg.go.dev/slices#Clone) — the shallow copy that severs batch aliasing.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — waiting on the item channel and the ticker at once.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [03-fan-in-merge.md](03-fan-in-merge.md) | Next: [05-rate-limited-consumer.md](05-rate-limited-consumer.md)
