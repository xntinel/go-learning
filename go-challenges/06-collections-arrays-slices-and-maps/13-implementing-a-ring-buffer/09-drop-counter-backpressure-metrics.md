# Exercise 9: Overwrite-Oldest Ring with a Dropped-Message Counter

An overwrite-oldest ring silently discards data by design — and silent data loss is
exactly what you must be able to see. This module instruments the ring so every
overwrite increments an atomic `dropped` counter you can export as a metric, and adds
a non-overwriting `TryPush` that returns `ErrFull` (drop-newest) so the caller
chooses the backpressure policy. It is the observability hook that turns an invisible
loss into a number on a dashboard.

Self-contained: its own module, the instrumented ring, a demo, and `-race` tests.

## What you'll build

```text
dropring/                  independent module: example.com/dropring
  go.mod                   go 1.24
  dropring.go              DropRing[T]: Push (counts drops), TryPush (ErrFull), Dropped
  cmd/
    demo/
      main.go              overwrite variant vs TryPush variant, drop counts
  dropring_test.go         -race: Push drop count exact, TryPush ErrFull, concurrent counter
```

Files: `dropring.go`, `cmd/demo/main.go`, `dropring_test.go`.
Implement: `DropRing[T]` with `Push` (overwrite-oldest, `dropped++` on each overwrite), `TryPush` (returns `ErrFull` when full, drops nothing), `Pop`, `Dropped() uint64`.
Test: push past capacity on `Push` and assert `Dropped()` equals the number of overwrites; on `TryPush` assert `ErrFull` once full and `Dropped()` stays consistent; race the counter under concurrent producers.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/dropring/cmd/demo
cd ~/go-exercises/dropring
go mod init example.com/dropring
go mod edit -go=1.24
```

### Two policies, one type, the caller decides

The same buffer exposes both backpressure policies so the caller picks per call site:

- `Push` is overwrite-oldest (drop-front). When full it evicts the oldest, writes the
  new value, and increments `dropped`. Correct for telemetry where the newest sample
  matters and you accept losing stale history — but now the loss is *counted*.
- `TryPush` is reject-newest (drop-back). When full it changes nothing and returns
  `ErrFull`. Correct when the caller would rather handle the rejection (retry later,
  shed load explicitly, apply its own backpressure) than lose the oldest committed
  item.

Exposing both, rather than baking one in, is the senior move: the data structure
provides mechanism, the caller supplies policy. A metrics pipeline uses `Push` and
scrapes `Dropped()`; a work dispatcher uses `TryPush` and reacts to `ErrFull`.

### Why the counter is atomic

`Dropped()` is read by a metrics goroutine while producers call `Push`. If `dropped`
were a plain `uint64`, that concurrent read/write is a data race even though it "looks
like just a counter." `sync/atomic.Uint64` makes `Add` and `Load` well-defined under
concurrency without taking the ring's mutex — the metrics scrape does not contend with
producers on the lock. The counter lives outside the mutex-protected region precisely
so an observability read never stalls the hot path. The overwrite itself happens under
the mutex (it mutates `head`/`tail`/`size`), and the `dropped.Add(1)` is called on the
overwrite branch; because `Add` is atomic, it is correct whether or not the mutex is
held, and keeping it lock-free on the read side is the win.

### Monotonic counter semantics

`Dropped()` is monotonic and cumulative — it only ever grows, like a Prometheus
counter. It reports total drops since construction, not "drops right now." A dashboard
takes the rate of this counter (`rate(dropped[1m])`) to see how fast you are shedding.
`TryPush` rejections are *not* counted as drops here, because nothing was lost — the
caller still holds the item and decides what to do. Conflating "I overwrote your data"
with "I declined your item" would make the metric lie, so keep them distinct.

Create `dropring.go`:

```go
package dropring

import (
	"errors"
	"sync"
	"sync/atomic"
)

// ErrFull is returned by TryPush when the buffer has no free slot.
var ErrFull = errors.New("dropring: buffer is full")

// ErrEmpty is returned by Pop when the buffer is empty.
var ErrEmpty = errors.New("dropring: buffer is empty")

// DropRing is a concurrency-safe ring that exposes both backpressure policies:
// Push overwrites the oldest (counting each drop), TryPush rejects the newest.
type DropRing[T any] struct {
	mu      sync.Mutex
	data    []T
	head    int
	tail    int
	size    int
	dropped atomic.Uint64
}

// New returns a DropRing with the given capacity (clamped to >= 1).
func New[T any](capacity int) *DropRing[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &DropRing[T]{data: make([]T, capacity)}
}

// Push adds v, overwriting the oldest element when full and counting the drop.
func (d *DropRing[T]) Push(v T) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.size == len(d.data) {
		// Overwrite the oldest: advance tail past it and count the loss.
		var zero T
		d.data[d.tail] = zero
		d.tail = (d.tail + 1) % len(d.data)
		d.size--
		d.dropped.Add(1)
	}
	d.data[d.head] = v
	d.head = (d.head + 1) % len(d.data)
	d.size++
}

// TryPush adds v only if there is room, returning ErrFull otherwise. It never
// overwrites and never increments the drop counter.
func (d *DropRing[T]) TryPush(v T) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.size == len(d.data) {
		return ErrFull
	}
	d.data[d.head] = v
	d.head = (d.head + 1) % len(d.data)
	d.size++
	return nil
}

// Pop removes and returns the oldest element, or ErrEmpty if empty.
func (d *DropRing[T]) Pop() (T, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var zero T
	if d.size == 0 {
		return zero, ErrEmpty
	}
	v := d.data[d.tail]
	d.data[d.tail] = zero
	d.tail = (d.tail + 1) % len(d.data)
	d.size--
	return v, nil
}

// Dropped returns the cumulative number of elements overwritten by Push. It is
// monotonic and safe to read concurrently with Push.
func (d *DropRing[T]) Dropped() uint64 { return d.dropped.Load() }

// Len returns the current element count (a hint under concurrency).
func (d *DropRing[T]) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.size
}
```

Note the `Push` overwrite branch decrements `size` and then the tail-of-function
increments it, so `size` nets back to `cap`; writing it this way keeps the eviction
and the write as two clear steps rather than one clever combined index update.

### The runnable demo

The demo shows both policies side by side: push 10 into a cap-3 overwrite ring and
read the drop count, then `TryPush` 10 into a fresh cap-3 ring and count the
rejections the caller saw.

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"

	"example.com/dropring"
)

func main() {
	// Overwrite-oldest: drops are counted by the ring.
	ow := dropring.New[int](3)
	for i := range 10 {
		ow.Push(i)
	}
	fmt.Printf("overwrite: len=%d dropped=%d\n", ow.Len(), ow.Dropped())

	// Reject-newest: the caller counts its own rejections.
	tp := dropring.New[int](3)
	rejected := 0
	for i := range 10 {
		if errors.Is(tp.TryPush(i), dropring.ErrFull) {
			rejected++
		}
	}
	fmt.Printf("trypush:   len=%d rejected=%d dropped=%d\n", tp.Len(), rejected, tp.Dropped())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
overwrite: len=3 dropped=7
trypush:   len=3 rejected=7 dropped=0
```

Ten pushes into a cap-3 overwrite ring drop the first seven, and the ring counts
them: `dropped=7`. Ten `TryPush` calls fill three slots and reject the other seven;
those rejections are the caller's to count (`rejected=7`), and because `TryPush`
never loses anything from the ring, its own `Dropped()` stays 0.

### Tests

The tests pin the exact drop accounting: N pushes into a cap-C overwrite ring drop
exactly `N - C`; `TryPush` returns `ErrFull` exactly once the buffer is full and never
increments `Dropped`; and the atomic counter is race-clean under concurrent producers,
with the total drops matching `totalPushed - capacity` once every producer has
finished.

Create `dropring_test.go`:

```go
package dropring

import (
	"errors"
	"sync"
	"testing"
)

func TestPushCountsOverwrites(t *testing.T) {
	t.Parallel()
	d := New[int](3)
	for i := range 10 {
		d.Push(i)
	}
	if got := d.Dropped(); got != 7 {
		t.Fatalf("Dropped = %d, want 7 (10 pushes into cap 3)", got)
	}
	if d.Len() != 3 {
		t.Fatalf("Len = %d, want 3", d.Len())
	}
}

func TestTryPushRejectsWhenFullWithoutDropping(t *testing.T) {
	t.Parallel()
	d := New[int](3)
	for i := range 3 {
		if err := d.TryPush(i); err != nil {
			t.Fatalf("TryPush(%d) into empty slot: %v", i, err)
		}
	}
	// Now full: every further TryPush must return ErrFull and drop nothing.
	for i := range 5 {
		if err := d.TryPush(100 + i); !errors.Is(err, ErrFull) {
			t.Fatalf("TryPush when full: err = %v, want ErrFull", err)
		}
	}
	if got := d.Dropped(); got != 0 {
		t.Fatalf("Dropped = %d after TryPush rejections, want 0 (nothing was lost)", got)
	}
	// The three earliest values are intact and in order.
	for _, want := range []int{0, 1, 2} {
		v, err := d.Pop()
		if err != nil || v != want {
			t.Fatalf("Pop = (%d,%v), want (%d,nil)", v, err, want)
		}
	}
}

func TestDroppedCounterRaceClean(t *testing.T) {
	t.Parallel()
	d := New[int](8)
	const producers, perProd = 8, 1000
	var wg sync.WaitGroup
	for p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perProd {
				d.Push(p*perProd + i)
			}
		}()
	}
	// Concurrent metrics reader; -race proves Dropped/Push do not race.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = d.Dropped()
			}
		}
	}()
	wg.Wait()
	close(stop)

	total := uint64(producers * perProd)
	wantDropped := total - uint64(d.Len())
	if got := d.Dropped(); got != wantDropped {
		t.Fatalf("Dropped = %d, want %d (total %d minus remaining %d)",
			got, wantDropped, total, d.Len())
	}
}
```

## Review

The instrumented ring is correct when the drop accounting is exact and race-clean:
`Push` counts one drop per overwrite so `Dropped()` equals `pushed - Len()`, and
`TryPush` never counts a drop because it never loses anything — the rejected item stays
with the caller. `TestPushCountsOverwrites` pins the overwrite count; `TestTryPushRejectsWhenFullWithoutDropping`
proves the reject path leaves both the data and the counter untouched; `TestDroppedCounterRaceClean`
proves the atomic counter under a concurrent reader. The traps: making `dropped` a
plain `uint64` (a data race the moment a metrics goroutine reads it — use
`atomic.Uint64`), and counting `TryPush` rejections as drops (they are not losses, and
conflating them makes the metric lie). Export `Dropped()` as a monotonic counter and
alert on its rate, not its absolute value.

## Resources

- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Uint64) — `Uint64.Add` and `Uint64.Load` for a lock-free counter.
- [`errors` package](https://pkg.go.dev/errors) — `errors.Is` for `ErrFull` / `ErrEmpty`.
- [Prometheus: counter metric type](https://prometheus.io/docs/concepts/metric_types/#counter) — why a drop metric is a monotonic counter.

---

Back to [08-flight-recorder-crash-dump.md](08-flight-recorder-crash-dump.md) | Next: [10-range-over-func-iterator-and-benchmark.md](10-range-over-func-iterator-and-benchmark.md)
