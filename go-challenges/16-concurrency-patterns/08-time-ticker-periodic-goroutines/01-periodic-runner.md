# Exercise 1: The Periodic Runner

A periodic runner fires a handler on a fixed cadence and can be stopped cleanly at any moment. This exercise builds the canonical `time.Ticker` worker: a `select` over the tick channel and an owned `done` channel, with a `sync.WaitGroup` so `Stop` does not return until the goroutine has actually exited.

This module is fully self-contained. It begins with its own `go mod init`, defines every type it needs, and ships its own demo and tests. Nothing here imports any other exercise.

## What you'll build

```text
periodic.go              Periodic: New, Start, Reset, Stop, Done (ticker + done + WaitGroup)
cmd/
  demo/
    main.go              run a handler for five ticks, then stop cleanly
periodic_test.go         fires repeatedly, stops cleanly, Reset shortens the period,
                         Stop is idempotent, Stop unblocks the worker
```

- Files: `periodic.go`, `cmd/demo/main.go`, `periodic_test.go`.
- Implement: `Periodic` with `New(d) *Periodic`, `Start(handler func(time.Time))`, `Reset(d)`, `Stop()`, and `Done() <-chan struct{}`.
- Test: `periodic_test.go` asserts the handler fires repeatedly, stops firing after `Stop`, speeds up after `Reset`, survives a double `Stop`, and that `Stop` returns promptly.
- Verify: `go test -race ./...`

### How the receive loop and Stop fit together

The runner owns three pieces of state: a `*time.Ticker` for the cadence, a `done chan struct{}` it closes to signal shutdown, and a `sync.WaitGroup` that tracks the worker goroutine. `Start` adds one to the wait group and launches a goroutine whose body is the two-arm `select` loop. The first arm, `case t := <-p.ticker.C`, runs the handler on every tick. The second arm, `case <-p.done`, returns, which runs the deferred `wg.Done`. Notice the receive uses no `, ok` form: `Stop` never closes `ticker.C`, so an `ok` check there would be dead code, and the only legitimate way out of the loop is the `done` arm.

`Stop` does three things in a deliberate order. It stops the ticker so no further ticks are scheduled, closes `done` to wake the `select` even if the next tick is far off, then calls `wg.Wait()` to block until the goroutine has returned. That last step is what makes `Stop` a real synchronization point: when it returns, the handler is provably not running, so a caller may safely tear down whatever state the handler touched. The close of `done` is wrapped in a `sync.Once` so that calling `Stop` twice is harmless — a second `close` of an already-closed channel would panic, and `Once` makes the signal fire exactly once while still letting every caller wait.

`Reset` simply forwards to `(*time.Ticker).Reset`, which stops the current period and restarts the cadence at the new duration; the next tick arrives after the new period. `Done` returns a channel that closes once the worker has exited, which lets a caller block on shutdown completing without reaching into the unexported wait group — useful when something other than the caller of `Stop` (for example a handler that decides it is finished) triggers the stop.

Create `periodic.go`:

```go
package periodic

import (
	"sync"
	"time"
)

// Periodic runs a handler on a fixed cadence driven by a time.Ticker. It can be
// stopped cleanly: Stop halts the ticker, wakes the receive loop via an owned
// done channel, and waits for the goroutine to return before returning itself.
type Periodic struct {
	ticker *time.Ticker
	done   chan struct{}
	wg     sync.WaitGroup
	once   sync.Once
}

// New creates a Periodic that ticks every d. The handler is not run until Start.
func New(d time.Duration) *Periodic {
	return &Periodic{
		ticker: time.NewTicker(d),
		done:   make(chan struct{}),
	}
}

// Start launches the worker goroutine. handler is called once per tick with the
// tick time. Start returns immediately; the work happens in the background.
func (p *Periodic) Start(handler func(time.Time)) {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case t := <-p.ticker.C:
				handler(t)
			case <-p.done:
				return
			}
		}
	}()
}

// Reset changes the tick period. The next tick arrives after the new duration.
func (p *Periodic) Reset(d time.Duration) {
	p.ticker.Reset(d)
}

// Stop halts the ticker, signals the worker to exit, and blocks until it has
// fully returned. It is safe to call Stop more than once and from any goroutine
// except the worker's own handler.
func (p *Periodic) Stop() {
	p.once.Do(func() {
		p.ticker.Stop()
		close(p.done)
	})
	p.wg.Wait()
}

// Done returns a channel that is closed once the worker goroutine has exited.
func (p *Periodic) Done() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	return done
}
```

### The runnable demo

The demo fires a handler every 20 ms and prints the first five ticks, then stops cleanly. To keep the output deterministic and free of data races, the handler is the only goroutine that touches its counter, and it hands each tick number to `main` over a buffered channel with a non-blocking send. The buffer preserves order, so `main` always reads `1, 2, 3, 4, 5` regardless of scheduling jitter; any ticks that fire after `main` has its five are simply dropped by the non-blocking send and then halted by `Stop`.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/periodic-runner"
)

func main() {
	p := periodic.New(20 * time.Millisecond)

	ticks := make(chan int, 8)
	var n int
	p.Start(func(t time.Time) {
		n++
		select {
		case ticks <- n:
		default:
		}
	})

	for i := 1; i <= 5; i++ {
		fmt.Printf("received tick %d\n", <-ticks)
	}

	p.Stop()
	fmt.Println("stopped cleanly")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
received tick 1
received tick 2
received tick 3
received tick 4
received tick 5
stopped cleanly
```

### Tests

The tests pin the runner's contract. `TestFiresRepeatedly` runs a 5 ms ticker for a while and asserts the handler fired many times, using a wide tolerance band because wall-clock counts are inherently noisy. `TestStopsCleanly` records the count, stops, waits, and asserts the count did not move — proving `Stop` actually halts the handler. `TestResetShortensPeriod` starts slow, resets to a fast period, and asserts the firing rate jumped. `TestStopIsIdempotent` calls `Stop` twice and asserts neither call panics. `TestStopUnblocksWorker` asserts `Stop` returns within a deadline rather than hanging.

Create `periodic_test.go`:

```go
package periodic

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestFiresRepeatedly(t *testing.T) {
	t.Parallel()

	p := New(5 * time.Millisecond)
	defer p.Stop()

	var count atomic.Int64
	p.Start(func(time.Time) { count.Add(1) })

	time.Sleep(150 * time.Millisecond)

	if got := count.Load(); got < 5 {
		t.Fatalf("got %d ticks in 150ms with a 5ms period, want many", got)
	}
}

func TestStopsCleanly(t *testing.T) {
	t.Parallel()

	p := New(2 * time.Millisecond)
	var count atomic.Int64
	p.Start(func(time.Time) { count.Add(1) })

	time.Sleep(30 * time.Millisecond)
	p.Stop()

	before := count.Load()
	time.Sleep(30 * time.Millisecond)
	if after := count.Load(); after != before {
		t.Fatalf("handler ran after Stop: before=%d after=%d", before, after)
	}
}

func TestResetShortensPeriod(t *testing.T) {
	t.Parallel()

	p := New(100 * time.Millisecond)
	defer p.Stop()

	var count atomic.Int64
	p.Start(func(time.Time) { count.Add(1) })

	time.Sleep(60 * time.Millisecond)
	mid := count.Load()
	p.Reset(2 * time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	end := count.Load()

	if end-mid < 5 {
		t.Fatalf("after Reset(2ms) got only %d more ticks, want several", end-mid)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	p := New(5 * time.Millisecond)
	p.Start(func(time.Time) {})

	p.Stop()
	p.Stop() // must not panic on the second close.
}

func TestStopUnblocksWorker(t *testing.T) {
	t.Parallel()

	p := New(time.Second)
	started := make(chan struct{})
	var once atomic.Bool
	p.Start(func(time.Time) {
		if once.CompareAndSwap(false, true) {
			close(started)
		}
	})
	// Use a short period via Reset so the first tick arrives quickly.
	p.Reset(time.Millisecond)
	<-started

	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not return")
	}
}
```

## Review

The runner is correct when three properties hold together. First, the receive loop has exactly two arms and exits only through `done`: a `case t, ok := <-ticker.C` with an `ok` check would be dead code because `Stop` never closes the tick channel, and a `for range ticker.C` would block forever after `Stop` for the same reason. Second, `Stop` is ordered stop-ticker, close-`done`, `wg.Wait`, so when it returns the handler is provably idle; skipping the `wg.Wait` turns a clean shutdown into a use-after-free where the caller frees state the handler still touches. Third, the close of `done` is guarded by `sync.Once`, so a second `Stop` cannot panic on a double close while every caller still waits for the worker.

Common mistakes for this feature. The first is calling `Stop` from inside the handler: `Stop` waits on the wait group, and the handler is the very goroutine the wait group is tracking, so a handler that calls `Stop` deadlocks against itself — signal a separate channel and let another goroutine call `Stop`, or close `done` without waiting. The second is sharing a mutable counter between the handler and the asserting goroutine without synchronization; the tests use `atomic.Int64` precisely so the race detector stays quiet. The third is asserting an exact tick count: wall-clock cadence is noisy, so the tests use tolerance bands and relative comparisons rather than equality, which is the honest way to test a real ticker.

## Resources

- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the standard-library ticker, its `C` channel, and the documented fact that `Stop` does not close the channel.
- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) — constructor semantics, including that the duration must be greater than zero.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the primitive that makes `Stop` wait for the worker to exit.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-bounded-periodic.md](02-bounded-periodic.md)
