# 8. Periodic Goroutines: `time.Ticker` Done Right

A periodic goroutine fires a callback every N milliseconds. The right tool is
`time.NewTicker(d)` which exposes a `<-chan time.Time` and a `Stop()` method.
The naive alternative - `time.Sleep` in a loop - drifts over time and ignores
cancellation. The right consumer reads from the ticker's channel under a
`select` that also listens for `done`, so a `Stop` actually halts the loop.

```text
periodic/
  go.mod
  internal/periodic/periodic.go
  internal/periodic/periodic_test.go
  cmd/periodicdemo/main.go
```

The package exposes `Every` that returns a `*Ticker` wrapper which fires the
handler on every tick and supports cancellation. The lesson's tests use a
short period (1ms) and bound the count of firings so the test stays fast.

## Concepts

### `Ticker` Adjusts For Drift

`time.NewTicker(d)` fires roughly every `d`. If the handler takes 100ms and
the period is 50ms, the ticker drops missed ticks rather than queuing them
(this is a deliberate `Ticker` vs `Timer` distinction: a `Timer.Reset` while
the timer has already fired is fine; a `Ticker` never accumulates a backlog).
That makes it the right tool for "do this every N seconds, never mind the
drift".

### The Receiver Must Select On Done

The pattern is:

```go
for {
    select {
    case <-ticker.C:
        handler()
    case <-done:
        return
    }
}
```

Without `<-done`, the consumer cannot be stopped. `ticker.Stop()` closes
nothing; it just stops sending on the channel. A `for ... range ticker.C`
loop with no `done` exits when nothing sends, which can happen immediately
on stop - but in general you need the explicit signal so other code paths
return.

### `Stop` Does Not Close The Channel

`(*Ticker).Stop()` does NOT close `ticker.C`. From the Go source: "Stop does
not close the channel, to prevent a read from the channel succeeding
incorrectly." If your handler can race with stop, drain the channel once
after `Stop()` before exiting.

### `Reset` Replaces The Period

`(*Ticker).Reset(d)` changes the period. The standard idiom when stopping
is `defer t.Stop()` plus a single `for { select }` body; if you also need
to change the period at runtime, call `t.Reset(newD)` from inside the loop.

## Exercises

### Exercise 1: The Periodic Runner

A `Periodic` holds a ticker, a done channel, and a handler. `Start` spawns the
goroutine; `Stop` signals it.

Create `internal/periodic/periodic.go`:

```go
package periodic

import (
	"sync"
	"time"
)

type Periodic struct {
	ticker *time.Ticker
	done   chan struct{}
	wg     sync.WaitGroup
}

func New(d time.Duration) *Periodic {
	return &Periodic{
		ticker: time.NewTicker(d),
		done:   make(chan struct{}),
	}
}

func (p *Periodic) Start(handler func(time.Time)) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case t, ok := <-p.ticker.C:
				if !ok {
					return
				}
				handler(t)
			case <-p.done:
				return
			}
		}
	}()
}

func (p *Periodic) Stop() {
	p.ticker.Stop()
	close(p.done)
	p.wg.Wait()
}

func (p *Periodic) Reset(d time.Duration) {
	p.ticker.Reset(d)
}

// Done returns a channel that closes when the periodic goroutine has fully
// exited. Callers can use it to block on Stop returning without reaching into
// the unexported wait group.
func (p *Periodic) Done() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	return done
}
```

`Stop` does three things in order: stop the ticker so no more ticks arrive,
close `done` to wake the select, then wait for the goroutine to exit. Closing
`done` is the signal that the receiver uses to return.

### Exercise 2: Bounded Variant With Max Ticks

Sometimes the periodic work has a fixed budget: fire N times then stop. This
is exposed as a separate helper so the unbounded `Periodic` is not polluted
with a "max ticks" knob:

```go
package periodic

import (
	"sync"
	"sync/atomic"
	"time"
)

type Bounded struct {
	ticker *time.Ticker
	done   chan struct{}
	count  atomic.Int64
	max    int64
	wg     sync.WaitGroup
}

func NewBounded(d time.Duration, max int64) *Bounded {
	return &Bounded{
		ticker: time.NewTicker(d),
		done:   make(chan struct{}),
		max:    max,
	}
}

func (b *Bounded) Start(handler func(time.Time, int64)) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			select {
			case t, ok := <-b.ticker.C:
				if !ok {
					return
				}
				n := b.count.Add(1)
				handler(t, n)
				if n >= b.max {
					return
				}
			case <-b.done:
				return
			}
		}
	}()
}

func (b *Bounded) Stop() {
	b.ticker.Stop()
	close(b.done)
	b.wg.Wait()
}
```

The counter is `atomic.Int64` because both the loop and any external caller
might read it. The loop increments first, then checks against `max`, so the
handler is invoked exactly `max` times.

### Exercise 3: Test The Contract

Create `internal/periodic/periodic_test.go`:

```go
package periodic

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeriodicFiresApproximatelyNTimes(t *testing.T) {
	t.Parallel()

	p := New(5 * time.Millisecond)
	defer p.Stop()

	var count atomic.Int64
	p.Start(func(time.Time) {
		count.Add(1)
	})

	time.Sleep(120 * time.Millisecond)

	got := count.Load()
	if got < 15 || got > 35 {
		t.Fatalf("got %d ticks in 120ms with 5ms period, want ~24", got)
	}
}

func TestPeriodicStopsCleanly(t *testing.T) {
	t.Parallel()

	p := New(2 * time.Millisecond)
	var count atomic.Int64
	p.Start(func(time.Time) { count.Add(1) })

	time.Sleep(20 * time.Millisecond)
	p.Stop()

	after := count.Load()
	time.Sleep(30 * time.Millisecond)
	if count.Load() != after {
		t.Fatalf("handler ran after Stop: before=%d after=%d", after, count.Load())
	}
}

func TestPeriodicResetChangesPeriod(t *testing.T) {
	t.Parallel()

	p := New(50 * time.Millisecond)
	defer p.Stop()

	var count atomic.Int64
	p.Start(func(time.Time) { count.Add(1) })

	time.Sleep(60 * time.Millisecond)
	mid := count.Load()
	p.Reset(2 * time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	end := count.Load()

	if end-mid < 20 {
		t.Fatalf("after Reset(2ms) got only %d more ticks, want >= 20", end-mid)
	}
}

func TestBoundedStopsAfterMax(t *testing.T) {
	t.Parallel()

	b := NewBounded(2*time.Millisecond, 5)
	defer b.Stop()

	var mu sync.Mutex
	var got []int64
	b.Start(func(_ time.Time, n int64) {
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
	})

	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Bounded did not return after max ticks")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 5 {
		t.Fatalf("got %d firings, want 5", len(got))
	}
	for i, n := range got {
		if n != int64(i+1) {
			t.Fatalf("got[%d] = %d, want %d", i, n, i+1)
		}
	}
}

func TestPeriodicIsRaceFree(t *testing.T) {
	t.Parallel()

	p := New(time.Millisecond)
	defer p.Stop()

	var hits atomic.Int64
	p.Start(func(time.Time) {
		hits.Add(1)
	})

	time.Sleep(50 * time.Millisecond)
	if hits.Load() == 0 {
		t.Fatal("no ticks observed")
	}
}

func TestPeriodicStopUnblocksStart(t *testing.T) {
	t.Parallel()

	p := New(time.Second)
	started := make(chan struct{})
	p.Start(func(time.Time) {
		close(started)
	})
	<-started
	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Stop did not return")
	}
}
```

`TestPeriodicFiresApproximatelyNTimes` uses a tolerance band rather than an
exact count because wall-clock jitter makes the count nondeterministic. The
test pins the contract: "roughly N ticks in roughly N*period time".

Your turn: add `TestPeriodicStopIsIdempotent` that calls `Stop()` twice and
asserts neither call panics.

### Exercise 4: Runnable Demo

Create `cmd/periodicdemo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/periodic/internal/periodic"
)

func main() {
	p := periodic.New(50 * time.Millisecond)
	defer p.Stop()

	count := 0
	p.Start(func(t time.Time) {
		count++
		fmt.Printf("tick %d at %s\n", count, t.Format(time.RFC3339Nano))
		if count == 5 {
			p.Stop()
		}
	})

	<-p.Done()
}
```

## Common Mistakes

### Using `time.Sleep` In A Loop

Wrong: `for { work(); time.Sleep(d) }`.

What happens: drift accumulates (the work time is added on top of the
sleep), and there is no cancellation hook.

Fix: `time.NewTicker(d)` plus a `select` on the ticker channel and a `done`.

### Reading `<-ticker.C` Without A `done` Case

Wrong: `for t := range ticker.C { handler(t) }` and expecting `Stop` to
break the loop.

What happens: the goroutine blocks forever after `Stop()` because the
channel is not closed.

Fix: include `case <-done: return` in the select; close `done` from
`Stop()`.

### Closing `ticker.C`

Wrong: `close(p.ticker.C)` to "signal" the consumer.

What happens: `(*Ticker).Stop` panics if you close its channel. From the Go
source: `Stop` does not close the channel precisely to keep callers from
shooting themselves.

Fix: use a separate `done chan struct{}` you own, and close that.

### Forgetting `wg.Wait` In Stop

Wrong: `Stop()` returns before the goroutine has actually returned.

What happens: the caller thinks the worker is gone, but the handler is still
running. If `handler` touches state that the caller is about to free, that
is a use-after-free.

Fix: `defer p.wg.Wait()` inside `Stop` so the goroutine has fully exited
before `Stop` returns.

## Verification

From `~/go-exercises/periodic`:

```bash
test -z "$(gofmt -l .)"
go vet ./...
go build ./...
go test -count=1 -race ./...
```

All four must pass. The race detector finds data races between the handler
and any external counter reads.

## Summary

- `time.NewTicker(d)` returns a `*Ticker` with channel `C` and method `Stop`.
- The receiver's select must include `<-done` so `Stop` is effective.
- `Stop` does not close `C`; close your own `done` channel.
- `Ticker` drops missed ticks rather than queuing them; it does not drift
  the way a `Sleep` loop does.
- For a fixed-budget periodic, use `Bounded` with a counter.

## What's Next

Next: [Or-Channel Pattern](../09-or-channel-pattern/09-or-channel-pattern.md).

## Resources

- [pkg.go.dev: time.Ticker](https://pkg.go.dev/time#Ticker)
- [pkg.go.dev: time.Timer](https://pkg.go.dev/time#Timer)
- [Go Blog: Pitfalls of context.Value and tickers](https://go.dev/blog/context)