# Exercise 2: Best-Effort Metrics Emitter That Drops Under Backpressure

A metrics/StatsD/log-shipping client must never block the request path. If the
shipping buffer is full because the network is slow, the correct behavior is to
drop the sample and count the drop, not to stall the handler that produced it. This
is the DROP overload policy, and it is a non-blocking try-send with a counter on
the `default` branch.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports another exercise.

## What you'll build

```text
emitter/                    independent module: example.com/emitter
  go.mod                    go 1.26
  emitter.go                type Emitter; Emit (try-send + drop counter), Dropped, Consume
  cmd/
    demo/
      main.go               emits past the buffer, ships some, reports drops
  emitter_test.go           drop-on-full, no-loss delivery, concurrent -race accounting
```

- Files: `emitter.go`, `cmd/demo/main.go`, `emitter_test.go`.
- Implement: `New(buffer int)`, `Emit(Sample) bool` (non-blocking try-send; increments a dropped counter and returns false when full), `Dropped() int64`, and `Consume(ctx, ship)` that ships buffered samples and flushes on cancel.
- Test: fill the buffer and assert further `Emit` calls return `false` without blocking and `Dropped()` equals the overflow; assert a running consumer receives every accepted sample and `received + dropped == emitted`; a concurrent `-race` accounting test.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/02-select-with-default/02-drop-when-full-telemetry-emitter/cmd/demo
cd go-solutions/14-select-and-context/02-select-with-default/02-drop-when-full-telemetry-emitter
go mod edit -go=1.26
```

### The hot path must never block

The invariant is that `Emit` is called from a request handler and must return in
O(1) no matter what the consumer is doing. A blocking send (`ch <- s`) would couple
the handler's latency to the shipping backend's latency: if the network stalls, the
buffer fills, and every subsequent handler blocks in `Emit` — a slow dependency
turns into a service-wide latency cliff. The non-blocking send breaks that
coupling: when the buffer is full, `Emit` takes `default`, increments `dropped`,
and returns `false` immediately. The handler proceeds; the metric is lost. That
trade is the entire point of best-effort telemetry — one lost sample is far cheaper
than one blocked request.

The counter is not optional. A drop with no counter is a silent data-loss bug: your
dashboards go quiet and you cannot tell whether traffic dropped or samples did.
`dropped` is an `atomic.Int64` because `Emit` runs concurrently from many handler
goroutines, and `Dropped()` may be read from a metrics-scrape goroutine at any
time. The channel itself is the only other shared state, and channel operations are
already synchronized, so the atomic counter plus the channel is the whole
concurrency surface — no mutex.

`Consume` is the shipping goroutine. It selects over the buffer and `ctx.Done()`;
on cancellation it performs a final non-blocking drain so samples already queued at
shutdown are still shipped rather than lost. That final drain is a `TryRecv` loop:
empty what is buffered, then return. Note the deliberate asymmetry — `Emit` drops
under *backpressure* (buffer full) while `Consume` drains under *shutdown* (buffer
being emptied); both are non-blocking, but they express opposite policies.

Create `emitter.go`:

```go
package emitter

import (
	"context"
	"sync/atomic"
)

// Sample is one telemetry data point.
type Sample struct {
	Metric string
	Value  int64
}

// Emitter is a best-effort telemetry client. Emit never blocks the caller: when
// the ship buffer is full it drops the sample and counts it.
type Emitter struct {
	ch      chan Sample
	dropped atomic.Int64
}

// New returns an Emitter whose ship buffer holds `buffer` samples. The buffer
// length is the burst tolerance: bursts up to this size are absorbed, beyond it
// samples are dropped.
func New(buffer int) *Emitter {
	return &Emitter{ch: make(chan Sample, buffer)}
}

// Emit is a non-blocking try-send. It returns true if the sample was buffered for
// shipping, or false if the buffer was full — in which case it drops the sample
// and increments the dropped counter. It never blocks.
func (e *Emitter) Emit(s Sample) bool {
	select {
	case e.ch <- s:
		return true
	default:
		e.dropped.Add(1)
		return false
	}
}

// Dropped reports how many samples have been dropped due to backpressure.
func (e *Emitter) Dropped() int64 {
	return e.dropped.Load()
}

// Consume ships buffered samples by calling ship for each, until ctx is done. On
// cancellation it performs a final non-blocking drain so already-queued samples
// are shipped rather than lost, then returns.
func (e *Emitter) Consume(ctx context.Context, ship func(Sample)) {
	for {
		select {
		case s := <-e.ch:
			ship(s)
		case <-ctx.Done():
			for {
				select {
				case s := <-e.ch:
					ship(s)
				default:
					return
				}
			}
		}
	}
}
```

### The runnable demo

The demo uses a small buffer and emits more than it can hold with no consumer
running, so some samples are dropped; then it starts a consumer to flush the rest
and reports the split. The numbers are deterministic: buffer 3, emit 5, so 3 are
accepted and 2 dropped.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/emitter"
)

func main() {
	e := emitter.New(3)

	accepted := 0
	for i := range 5 {
		if e.Emit(emitter.Sample{Metric: "req.latency", Value: int64(i)}) {
			accepted++
		}
	}
	fmt.Println("accepted:", accepted)
	fmt.Println("dropped:", e.Dropped())

	// Drain the buffered, accepted samples through a one-shot consume.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done: Consume flushes the buffer, then returns
	shipped := 0
	e.Consume(ctx, func(emitter.Sample) { shipped++ })
	fmt.Println("shipped:", shipped)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
accepted: 3
dropped: 2
shipped: 3
```

### Tests

`TestDropsWhenFull` fills the buffer, then asserts every overflow `Emit` returns
`false` without blocking (guarded by a done channel and a timeout so a regression to
a blocking send fails the test instead of hanging it) and that `Dropped()` equals
the overflow. `TestNoLossWhenConsumed` emits into a buffer large enough to hold
everything (so nothing drops), then cancels a context to flush and asserts
`received == emitted`. `TestConcurrentAccounting` runs many emitters concurrently
under `-race` and asserts the fundamental invariant `received + dropped == emitted`
with no lost or double-counted sample.

Create `emitter_test.go`:

```go
package emitter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDropsWhenFull(t *testing.T) {
	t.Parallel()

	const buffer, overflow = 4, 6
	e := New(buffer)

	for i := range buffer { // fill exactly to capacity
		if !e.Emit(Sample{Metric: "m", Value: int64(i)}) {
			t.Fatalf("Emit %d rejected before buffer was full", i)
		}
	}

	// Every further Emit must drop and must not block. Run them behind a timeout.
	done := make(chan int, 1)
	go func() {
		dropped := 0
		for range overflow {
			if !e.Emit(Sample{Metric: "m", Value: -1}) {
				dropped++
			}
		}
		done <- dropped
	}()

	select {
	case dropped := <-done:
		if dropped != overflow {
			t.Fatalf("dropped %d overflow Emits, want %d", dropped, overflow)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a full buffer; it must be non-blocking")
	}

	if got := e.Dropped(); got != overflow {
		t.Fatalf("Dropped() = %d, want %d", got, overflow)
	}
}

func TestNoLossWhenConsumed(t *testing.T) {
	t.Parallel()

	const n = 100
	e := New(n) // big enough that nothing drops

	for i := range n {
		if !e.Emit(Sample{Metric: "m", Value: int64(i)}) {
			t.Fatalf("Emit %d dropped despite room", i)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Consume flushes the full buffer, then returns
	received := 0
	e.Consume(ctx, func(Sample) { received++ })

	if received != n {
		t.Fatalf("received %d, want %d", received, n)
	}
	if got := e.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
}

func TestConcurrentAccounting(t *testing.T) {
	t.Parallel()

	const emitters, perEmitter = 8, 500
	const total = emitters * perEmitter
	e := New(64)

	var accepted atomic.Int64
	var wg sync.WaitGroup
	for range emitters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perEmitter {
				if e.Emit(Sample{Metric: "m", Value: int64(i)}) {
					accepted.Add(1)
				}
			}
		}()
	}
	wg.Wait()

	// All emits are done. Flush the buffer with a cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	received := 0
	e.Consume(ctx, func(Sample) { received++ })

	if int64(received) != accepted.Load() {
		t.Fatalf("received %d != accepted %d", received, accepted.Load())
	}
	if int64(received)+e.Dropped() != total {
		t.Fatalf("received %d + dropped %d != emitted %d", received, e.Dropped(), total)
	}
}
```

The concurrent test imports `sync/atomic` for the accepted counter, which is
incremented once per successful `Emit` and read only after `wg.Wait()`.

## Review

The emitter is correct when `Emit` never blocks and every sample is accounted for:
`received + dropped` must equal the number emitted, always. The delivery test proves
no sample is lost when there is room; the drop test proves overflow is counted, not
silently discarded; the concurrent test proves the atomic counter and the channel
together lose and double-count nothing under `-race`. The mistake to avoid is a
blocking send in `Emit` — it passes a single-threaded test but couples handler
latency to the shipping backend and stalls the whole service when the network is
slow; the timeout guard in `TestDropsWhenFull` is there specifically to catch that
regression. The second mistake is dropping without the counter, which the accounting
assertion makes impossible to hide.

## Resources

- [Go Specification: Select statements](https://go.dev/ref/spec#Select_statements) — the non-blocking send rule.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic#Int64) — `Int64.Add`/`Load` for the lock-free drop counter.
- [Go by Example: Non-Blocking Channel Operations](https://gobyexample.com/non-blocking-channel-operations) — try-send and drop-when-full.

---

Back to [00-concepts.md](00-concepts.md) | Next: [03-latest-value-config-mailbox.md](03-latest-value-config-mailbox.md)
