# Exercise 6: Rate-limited tiered dispatcher under a token cap

An outbound API client with a global rate cap and two request tiers must let the
high tier jump the queue *without* exceeding the cap. This exercise composes rate
limiting with priority: emission is gated on a ticker token, and on each token a
priority peek chooses the high tier over the low tier. Priority reorders within
the cap; it never bypasses it.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
ratelimit/                   module example.com/ratelimit
  go.mod
  dispatch.go                type Item; func Dispatch(ctx, tick, hi, lo, out) error
  cmd/
    demo/
      main.go                real time.Ticker, drains hi before lo, prints emitted order
  dispatch_test.go           one-per-tick, hi-before-lo, no-emit-between-ticks, cancel
```

Files: `dispatch.go`, `cmd/demo/main.go`, `dispatch_test.go`.
Implement: `Dispatch(ctx, tick <-chan time.Time, hi, lo <-chan Item, out chan<- Item) error`
— on each tick, emit at most one item, preferring `hi` over `lo`; skip a tick when
nothing is ready; exit on `ctx.Done()`.
Test (with an injected tick channel): at most one item per tick; `hi` drained
before `lo` when both are ready; no emission between ticks even with items queued;
cancelling exits cleanly.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/08-select-priority-and-starvation/06-rate-limited-priority-dispatcher/cmd/demo
cd go-solutions/14-select-and-context/08-select-priority-and-starvation/06-rate-limited-priority-dispatcher
```

### Priority reorders within the cap, it does not escape it

The wrong mental model is "high priority means send it immediately." If a
high-tier item is emitted the moment it arrives, outside the token cadence, the
global rate cap is blown — which is exactly the abuse (or the upstream 429 storm)
the limiter exists to prevent. The correct composition keeps a single gate on
emission and lets priority decide *what* to emit each time the gate opens:

- The gate is a token. Here it is a `time.Ticker` tick; in production it is
  frequently a `golang.org/x/time/rate.Limiter` token (`limiter.Wait(ctx)` or
  `limiter.Allow()`), which also supports bursts. Either way, one token permits
  exactly one emission.
- On each token, apply the priority peek: try `hi` first, fall back to `lo`. The
  high tier is drained before the low tier, but only ever one item per token, so
  the emission rate never exceeds the cap.
- If no item is ready when a token arrives, the token is spent doing nothing (or,
  in a fancier design, banked). Emission happens *only* inside the tick case, so
  nothing is ever emitted between ticks even when both queues are backed up.

Injecting the tick channel (`<-chan time.Time`) rather than creating the ticker
inside `Dispatch` is what makes the behavior testable: a `time.Ticker`'s `C` is a
`<-chan time.Time`, and so is a plain channel the test writes to by hand. The test
sends ticks one at a time and observes exactly one emission per tick, with no
dependence on wall-clock timing.

Create `dispatch.go`:

```go
package ratelimit

import (
	"context"
	"time"
)

// Item is a request to be emitted under the rate cap.
type Item struct {
	Label string
	N     int
}

// Dispatch emits at most one item per tick to out, preferring hi over lo. It
// returns ctx.Err() when ctx is cancelled. Because emission happens only in the
// tick branch, the output rate can never exceed the tick rate, even when both
// input queues are saturated.
func Dispatch(ctx context.Context, tick <-chan time.Time, hi, lo <-chan Item, out chan<- Item) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			item, ok := pick(hi, lo)
			if !ok {
				continue // token spent with nothing to send
			}
			select {
			case out <- item:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// pick returns the next item, preferring hi, without blocking. It reports false
// when both queues are momentarily empty.
func pick(hi, lo <-chan Item) (Item, bool) {
	select {
	case v := <-hi:
		return v, true
	default:
	}
	select {
	case v := <-hi:
		return v, true
	case v := <-lo:
		return v, true
	default:
		return Item{}, false
	}
}
```

### The runnable demo

The demo uses a real `time.Ticker` at 10 ms and feeds three high items and two
low items. The dispatcher drains the high tier first, one per tick, then the low
tier — so the first five emissions are `hi hi hi lo lo`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/ratelimit"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hi := make(chan ratelimit.Item, 3)
	lo := make(chan ratelimit.Item, 2)
	for i := range 3 {
		hi <- ratelimit.Item{Label: "hi", N: i}
	}
	for i := range 2 {
		lo <- ratelimit.Item{Label: "lo", N: i}
	}

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	out := make(chan ratelimit.Item)
	go func() { _ = ratelimit.Dispatch(ctx, ticker.C, hi, lo, out) }()

	labels := make([]string, 0, 5)
	for range 5 {
		labels = append(labels, (<-out).Label)
	}
	fmt.Println("emitted:", labels)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
emitted: [hi hi hi lo lo]
```

### Tests

All tests inject a plain `chan time.Time` so ticks are delivered on demand, making
the assertions independent of wall-clock timing. `TestOnePerTickHighFirst` sends
five manual ticks and asserts the emission order `h0 h1 h2 l0 l1`.
`TestNoEmissionBetweenTicks` sends one tick, reads one item, then asserts the out
channel is empty even though more items are queued. `TestCancelExits` asserts
`Dispatch` returns `context.Canceled`.

Create `dispatch_test.go`:

```go
package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOnePerTickHighFirst(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hi := make(chan Item, 3)
	lo := make(chan Item, 2)
	for i := range 3 {
		hi <- Item{Label: "hi", N: i}
	}
	for i := range 2 {
		lo <- Item{Label: "lo", N: i}
	}

	tick := make(chan time.Time)
	out := make(chan Item)
	go func() { _ = Dispatch(ctx, tick, hi, lo, out) }()

	want := []string{"hi", "hi", "hi", "lo", "lo"}
	wantN := []int{0, 1, 2, 0, 1}
	for i := range want {
		tick <- time.Now()
		got := <-out
		if got.Label != want[i] || got.N != wantN[i] {
			t.Fatalf("emission %d = %s/%d, want %s/%d", i, got.Label, got.N, want[i], wantN[i])
		}
	}
}

func TestNoEmissionBetweenTicks(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hi := make(chan Item, 3)
	for i := range 3 {
		hi <- Item{Label: "hi", N: i}
	}
	lo := make(chan Item)

	tick := make(chan time.Time)
	out := make(chan Item)
	go func() { _ = Dispatch(ctx, tick, hi, lo, out) }()

	tick <- time.Now()
	if got := <-out; got.N != 0 {
		t.Fatalf("first emission N = %d, want 0", got.N)
	}
	// No further tick: nothing more must be emitted even though hi has items.
	select {
	case extra := <-out:
		t.Fatalf("emitted %+v between ticks, want nothing", extra)
	case <-time.After(30 * time.Millisecond):
		// correct: quiet until the next tick
	}
}

func TestCancelExits(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	out := make(chan Item)

	errc := make(chan error, 1)
	go func() { errc <- Dispatch(ctx, tick, make(chan Item), make(chan Item), out) }()

	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Dispatch did not exit on cancel")
	}
}
```

## Review

The dispatcher is correct when emission is gated strictly by the token: at most
one item per tick, high tier before low, and total silence between ticks even
under a full backlog. `TestNoEmissionBetweenTicks` is the anti-bypass proof — if
high priority could escape the cap, a queued high item would appear on `out`
before the next tick. The mistake to avoid is moving the `pick`/emit outside the
tick branch, or emitting more than one item per tick to "catch up," which blows
the rate cap the limiter exists to enforce. Injecting the tick channel keeps the
tests deterministic; in production swap the ticker for a
`golang.org/x/time/rate.Limiter` when you need bursts or a smooth
requests-per-second cap rather than a fixed cadence, applying the same per-token
priority peek.

## Resources

- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) and [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the token cadence and `Stop`.
- [`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate#Limiter) — the production rate limiter with bursts, for tiered dispatch.
- [Go spec: Select statements](https://go.dev/ref/spec#Select_statements) — the priority peek per token.

---

Back to [05-weighted-fair-scheduler.md](05-weighted-fair-scheduler.md) | Next: [07-backpressure-load-shed.md](07-backpressure-load-shed.md)
