# Exercise 8: Fan-In Aggregator — Closing the Results Channel Exactly Once After All Producers Finish

A streaming aggregator reads from several partition producers concurrently, each
sending into one shared results channel, and a consumer ranges over that channel until
it closes. The coordination problem is *who closes the channel, and when*. Close it
from a producer and a peer still mid-send panics; never close it and the consumer
ranges forever. The canonical answer is a single waiter goroutine running `wg.Wait();
close(out)`. This module builds that fan-in.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
fanin/                     independent module: example.com/fanin
  go.mod                   go 1.25
  fanin.go                 Merge funnels N producers into one channel, closed once
  cmd/
    demo/
      main.go              runnable demo: 3 producers, sum the merged stream
  fanin_test.go            exact-count receipt; bounded-channel backpressure; ctx
```

- Files: `fanin.go`, `cmd/demo/main.go`, `fanin_test.go`.
- Implement: `Merge(ctx, bufSize, producers) <-chan int` — one goroutine per producer joined by a WaitGroup, a single waiter goroutine doing `wg.Wait(); close(out)`.
- Test: N producers each emitting M values, assert the consumer receives exactly N*M and the range loop exits; a bounded (small buffer) subtest proving backpressure does not deadlock the closer; a cancellation subtest.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### One waiter closes; producers never do

Every producer runs in its own goroutine and sends its values on the shared `out`
channel. A `WaitGroup` counts the producers. A *separate* goroutine — the only one
allowed to close `out` — does nothing but `wg.Wait(); close(out)`. Because it waits for
every producer's `Done` before closing, no producer is still sending when the close
happens, so there is no send-on-closed-channel panic. And because it *does* close once
all producers finish, the consumer's `range out` loop terminates cleanly instead of
blocking forever.

Why not close from the last producer to finish? Producers do not know which of them is
last, and coordinating "am I last?" is exactly what the WaitGroup already does — pushing
that logic into producers reinvents the counter and reintroduces the race. The
single-waiter pattern keeps the close decision in one place, ordered after all `Done`s.

Backpressure is worth thinking through. If `out` is unbuffered or small, a producer
blocks on send until the consumer reads. That is fine: the waiter goroutine is blocked
in `wg.Wait()` (not in a send), so it does not participate in the backpressure, and as
long as *someone* ranges over `out`, producers drain and finish, the waiter wakes, and
the channel closes. The one way to deadlock is to build the pipeline and never consume
it — then producers block forever on the first send, `Done` never runs, and the waiter
never closes. Always have a consumer ranging.

The producers honor `ctx` so a cancelled aggregation stops sending; a producer that
ignores cancellation keeps trying to send and can wedge if the consumer has stopped
reading.

Create `fanin.go`:

```go
package fanin

import (
	"context"
	"sync"
)

// Producer emits values on out. It must stop early if ctx is cancelled and must
// not close out.
type Producer func(ctx context.Context, out chan<- int)

// Merge runs every producer concurrently, funneling their values into a single
// channel that is closed exactly once, after all producers have finished. The
// caller ranges over the returned channel until it closes. bufSize sets the
// output channel's buffer (0 for unbuffered).
func Merge(ctx context.Context, bufSize int, producers []Producer) <-chan int {
	out := make(chan int, bufSize)

	var wg sync.WaitGroup
	for _, p := range producers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p(ctx, out)
		}()
	}

	// The sole closer: waits for every producer, then closes exactly once.
	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

// Emit returns a Producer that sends the given values, respecting cancellation.
func Emit(values ...int) Producer {
	return func(ctx context.Context, out chan<- int) {
		for _, v := range values {
			select {
			case out <- v:
			case <-ctx.Done():
				return
			}
		}
	}
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/fanin"
)

func main() {
	producers := []fanin.Producer{
		fanin.Emit(1, 2, 3),
		fanin.Emit(10, 20),
		fanin.Emit(100),
	}

	out := fanin.Merge(context.Background(), 4, producers)

	sum, count := 0, 0
	for v := range out { // exits when the waiter closes the channel
		sum += v
		count++
	}
	fmt.Printf("count=%d sum=%d\n", count, sum)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
count=6 sum=136
```

### Tests

`TestMergeReceivesAllValues` runs N producers each emitting M values and asserts the
consumer receives exactly N*M and the range loop exits (the channel closed).
`TestMergeUnbufferedBackpressure` sets `bufSize` to 0 so every send blocks on the
consumer, proving the pattern does not deadlock under full backpressure.
`TestMergeCancel` cancels the context and asserts the channel still closes (the waiter
runs after producers return early), so the consumer never hangs.

Create `fanin_test.go`:

```go
package fanin

import (
	"context"
	"testing"
)

func TestMergeReceivesAllValues(t *testing.T) {
	t.Parallel()

	const (
		n = 6  // producers
		m = 50 // values each
	)
	producers := make([]Producer, 0, n)
	for p := range n {
		vals := make([]int, 0, m)
		for i := range m {
			vals = append(vals, p*m+i)
		}
		producers = append(producers, Emit(vals...))
	}

	out := Merge(context.Background(), 8, producers)

	count := 0
	for range out {
		count++
	}
	if count != n*m {
		t.Fatalf("received %d values, want %d", count, n*m)
	}
}

func TestMergeUnbufferedBackpressure(t *testing.T) {
	t.Parallel()

	producers := []Producer{
		Emit(1, 2, 3),
		Emit(4, 5, 6),
	}
	// bufSize 0: every send blocks until the consumer reads. Must not deadlock.
	out := Merge(context.Background(), 0, producers)

	sum := 0
	for v := range out {
		sum += v
	}
	if sum != 21 {
		t.Fatalf("sum = %d, want 21", sum)
	}
}

func TestMergeCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before consuming

	producers := []Producer{Emit(1, 2, 3), Emit(4, 5, 6)}
	out := Merge(ctx, 0, producers)

	// Producers observe cancellation and return; the waiter still closes out, so
	// this range must terminate (however many values slipped through).
	for range out {
	}
}

func TestMergeNoProducers(t *testing.T) {
	t.Parallel()

	out := Merge(context.Background(), 0, nil)
	count := 0
	for range out {
		count++
	}
	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}
}
```

## Review

The aggregator is correct when the consumer receives exactly N*M values and the
`range` loop always terminates because the channel closes exactly once. The unbuffered
subtest proves the pattern survives full backpressure, and the cancel subtest proves
the channel still closes when producers exit early — both would hang if the close were
misplaced. That the whole thing is `-race` clean confirms no producer races the close.

The single rule to carry away: the close belongs to one dedicated waiter goroutine
running `wg.Wait(); close(out)`, never to a producer. If you ever find yourself writing
`close(out)` inside a producer, stop — that is the send-on-closed panic waiting to
happen. And never build the pipeline without a consumer, or the producers block on
their first send and nothing ever finishes.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the canonical fan-in and channel-closing patterns.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the join that gates the close.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — closing and ranging semantics.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-graceful-shutdown-inflight-drain.md](07-graceful-shutdown-inflight-drain.md) | Next: [09-reusable-waitgroup-paged-backfill.md](09-reusable-waitgroup-paged-backfill.md)
