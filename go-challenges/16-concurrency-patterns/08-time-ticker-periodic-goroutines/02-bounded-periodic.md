# Exercise 2: A Bounded Periodic Runner

Sometimes periodic work has a fixed budget: fire exactly N times, then stop on its own. This exercise builds `Bounded`, a ticker worker that counts its firings, terminates itself once it reaches the cap, and is still stoppable early from the outside. Keeping the cap in a separate type rather than bolting a "max ticks" knob onto the general runner keeps each type honest about what it does.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
bounded.go               Bounded: NewBounded, Start, Stop, Count, Done (self-terminating ticker)
cmd/
  demo/
    main.go              fire exactly five times, then report
bounded_test.go          stops after max, external Stop ends it early, count is exact
```

- Files: `bounded.go`, `cmd/demo/main.go`, `bounded_test.go`.
- Implement: `Bounded` with `NewBounded(d, max) *Bounded`, `Start(handler func(time.Time, int64))`, `Stop()`, `Count() int64`, and `Done() <-chan struct{}`.
- Test: `bounded_test.go` asserts the handler fires exactly `max` times with a `1..max` sequence, that an external `Stop` ends it before the cap, and that `Count` is exact.
- Verify: `go test -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/16-concurrency-patterns/08-time-ticker-periodic-goroutines/02-bounded-periodic/cmd/demo && cd go-solutions/16-concurrency-patterns/08-time-ticker-periodic-goroutines/02-bounded-periodic
```

### Two exits from one loop

The bounded runner has two ways to end, and the design has to make both clean. The first is reaching the cap: the receive loop increments an `atomic.Int64` counter on each tick, passes the new count to the handler, and if the count has reached `max` it stops its own ticker and returns. The second is an external `Stop` arriving before the cap, which closes the owned `done` channel and the `done` arm of the `select` returns. Either way the goroutine runs its deferred `wg.Done`, so `Done` and `Stop` both observe completion through the same wait group regardless of which exit fired.

The counter is an `atomic.Int64` because two parties read it: the loop increments it, and an external caller may call `Count` concurrently to observe progress. Incrementing first and then comparing against `max` (`n := count.Add(1); if n >= max`) guarantees the handler is invoked for counts `1` through `max` inclusive and never a `max+1`-th time. When the loop hits the cap it calls `ticker.Stop()` itself before returning, so a self-terminated runner leaves no ticker running; a later external `Stop` is then a harmless no-op because both the ticker `Stop` and the `done` close are idempotent — the ticker tolerates a repeated `Stop`, and the `sync.Once` guards the channel close.

The important subtlety is that self-termination does not close `done`, yet `Stop` called afterward must still be safe and must still return. It is: `Stop` runs its `once`-guarded body (closing a `done` that now has no receiver, which is fine) and then `wg.Wait`, which returns immediately because the goroutine already exited. That symmetry — every path ends by decrementing the wait group, and every observer waits on that wait group — is what keeps the two exits from interfering.

Create `bounded.go`:

```go
package bounded

import (
	"sync"
	"sync/atomic"
	"time"
)

// Bounded runs a handler on a fixed cadence for at most max firings, then stops
// itself. It can also be stopped early from the outside via Stop.
type Bounded struct {
	ticker *time.Ticker
	done   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
	count  atomic.Int64
	max    int64
}

// NewBounded creates a Bounded that ticks every d and fires at most max times.
func NewBounded(d time.Duration, max int64) *Bounded {
	return &Bounded{
		ticker: time.NewTicker(d),
		done:   make(chan struct{}),
		max:    max,
	}
}

// Start launches the worker. handler receives the tick time and the 1-based
// firing number. After the max-th firing the worker stops itself.
func (b *Bounded) Start(handler func(time.Time, int64)) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		for {
			select {
			case t := <-b.ticker.C:
				n := b.count.Add(1)
				handler(t, n)
				if n >= b.max {
					b.ticker.Stop()
					return
				}
			case <-b.done:
				return
			}
		}
	}()
}

// Stop halts the worker early and waits for it to exit. It is safe to call Stop
// after the worker has already self-terminated, and safe to call more than once.
func (b *Bounded) Stop() {
	b.once.Do(func() {
		b.ticker.Stop()
		close(b.done)
	})
	b.wg.Wait()
}

// Count returns the number of firings so far.
func (b *Bounded) Count() int64 { return b.count.Load() }

// Done returns a channel that is closed once the worker has exited, whether by
// reaching the cap or by an external Stop.
func (b *Bounded) Done() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	return done
}
```

### The runnable demo

The demo fires exactly five times at 20 ms spacing, printing each firing number, then blocks on `Done` and reports the total. Because the worker self-terminates at the cap, the output is fully deterministic: five `fire` lines in order, then the summary. The handler is the only goroutine touching the count during firing, and `main` reads it only after `Done` closes, which happens strictly after the worker has returned, so there is no data race.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/bounded-periodic"
)

func main() {
	b := bounded.NewBounded(20*time.Millisecond, 5)

	b.Start(func(t time.Time, n int64) {
		fmt.Printf("fire %d\n", n)
	})

	<-b.Done()
	fmt.Printf("bounded runner fired %d times\n", b.Count())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fire 1
fire 2
fire 3
fire 4
fire 5
bounded runner fired 5 times
```

### Tests

`TestStopsAfterMax` runs a fast bounded worker, waits on `Done` with a deadline, and asserts the handler fired exactly `max` times with the firing numbers arriving as the strict sequence `1, 2, 3, 4, 5`. `TestExternalStopEndsEarly` starts a worker with a large cap and a slow period, lets a firing happen, stops it from the outside, and asserts the count never reached the cap and stayed stable afterward. `TestCountStartsAtZero` asserts a fresh worker reports zero before any tick.

Create `bounded_test.go`:

```go
package bounded

import (
	"sync"
	"testing"
	"time"
)

func TestStopsAfterMax(t *testing.T) {
	t.Parallel()

	b := NewBounded(2*time.Millisecond, 5)
	defer b.Stop()

	var mu sync.Mutex
	var seq []int64
	b.Start(func(_ time.Time, n int64) {
		mu.Lock()
		seq = append(seq, n)
		mu.Unlock()
	})

	select {
	case <-b.Done():
	case <-time.After(time.Second):
		t.Fatal("Bounded did not stop after max firings")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seq) != 5 {
		t.Fatalf("got %d firings, want 5", len(seq))
	}
	for i, n := range seq {
		if n != int64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d", i, n, i+1)
		}
	}
}

func TestExternalStopEndsEarly(t *testing.T) {
	t.Parallel()

	b := NewBounded(5*time.Millisecond, 1000)
	fired := make(chan struct{}, 1)
	b.Start(func(_ time.Time, _ int64) {
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	<-fired // wait for at least one firing
	b.Stop()

	before := b.Count()
	if before >= 1000 {
		t.Fatalf("worker reached cap before external Stop: %d", before)
	}
	time.Sleep(30 * time.Millisecond)
	if after := b.Count(); after != before {
		t.Fatalf("count moved after Stop: before=%d after=%d", before, after)
	}
}

func TestCountStartsAtZero(t *testing.T) {
	t.Parallel()

	b := NewBounded(time.Hour, 5)
	defer b.Stop()
	if got := b.Count(); got != 0 {
		t.Fatalf("fresh Count() = %d, want 0", got)
	}
}
```

## Review

The bounded runner is correct when its two exit paths cannot interfere. The cap path increments the atomic counter, fires the handler, and self-terminates exactly at `max` by stopping its own ticker and returning; incrementing before comparing is what bounds the firings to `1..max` inclusive. The external path closes the owned `done` and returns through the second `select` arm. Both paths end in the deferred `wg.Done`, so `Count`, `Done`, and `Stop` all observe completion the same way no matter which fired, and a late external `Stop` after self-termination is a safe no-op because the ticker `Stop` is idempotent and the channel close is guarded by `sync.Once`.

Common mistakes for this feature. The first is comparing before incrementing, or using a non-atomic counter shared between the loop and `Count`; either gives an off-by-one cap or a data race the detector will flag. The second is letting the worker leave its ticker running after the cap — without the `b.ticker.Stop()` on the cap path, a self-terminated worker keeps a live ticker scheduling ticks no one reads, which on older Go versions was a leak. The third is asserting on timing for the cap test instead of waiting on `Done`: the count is exact and event-driven, so the test should synchronize on completion, not sleep and hope.

## Resources

- [`time.Ticker.Stop`](https://pkg.go.dev/time#Ticker.Stop) — stopping a ticker so it schedules no further ticks; note it does not close the channel.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) — `atomic.Int64`, the race-free counter shared between the loop and `Count`.
- [`sync.Once`](https://pkg.go.dev/sync#Once) — running the shutdown close exactly once so a repeated `Stop` cannot panic.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [01-periodic-runner.md](01-periodic-runner.md) | Next: [03-config-refresher.md](03-config-refresher.md)
