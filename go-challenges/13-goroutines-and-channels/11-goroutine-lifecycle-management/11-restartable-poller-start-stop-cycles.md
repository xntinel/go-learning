# Exercise 11: A Restartable Background Poller: Stop, Then Start Again Cleanly

**Level: Intermediate**

A queue-consumer poller in a real service is not started once and left running.
Admission control pauses it under overload and resumes it when pressure drops, and
operators toggle it through a feature flag; over a process's lifetime it stops and
starts dozens of times. The naive lifecycle component works the first time and
leaks on the second: it reuses a stop or done channel that a previous cycle already
closed, or it orphans the old goroutine when a new Start launches. This exercise
builds a poller that survives an unbounded number of Stop-then-Start cycles with no
leak and no double-close.

This module is self-contained: its own module, a `poller` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
poller/                      independent module: example.com/poller
  go.mod                     go 1.26
  poller.go                  type Poller; New, Start, Stop, Processed; fresh channels per cycle
  cmd/demo/main.go           runnable demo: three Stop-then-Start cycles, leak-free
  poller_test.go             leak-free cycles, processed-per-cycle, misuse guards, concurrent race
```

- Files: `poller.go`, `cmd/demo/main.go`, `poller_test.go`.
- Implement: `New(poll PollFunc, interval time.Duration) *Poller`, `Start(ctx context.Context) error`, `Stop() error`, `Processed() int64`; plus `type PollFunc func(ctx context.Context) (processed int, err error)` and sentinels `ErrAlreadyRunning`, `ErrNotRunning`.
- Test: N sequential cycles leave the goroutine count at baseline every time; each cycle polls so `Processed` strictly increases; a second Start while live returns `ErrAlreadyRunning`; Stop while idle (or twice) returns `ErrNotRunning`; concurrent Start/Stop is race-clean and never panics.
- Verify: `go test -count=1 -race ./...`

Set up the module. This module uses `go.uber.org/goleak`, so `go mod tidy`
resolves it before the first test run:

```bash
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### One cycle owns one set of channels

The failure this component is built to avoid is *state that leaks between cycles*.
A stop channel and a done channel encode a single handshake: the owner closes stop
to say "leave," the goroutine closes done to say "I left." Those channels are
one-shot. Once closed they can never be reused — closing a closed channel panics,
and a receive on a closed channel returns immediately forever, so a second cycle
that inherited them would "leave" the instant it started. The rule that makes
restart safe is therefore: **each cycle allocates its own stop and done channels,
and Stop tears down exactly the cycle it belongs to.**

The `Poller` keeps `stop`, `done`, and a `running` bool, all guarded by a mutex.
The protocol is:

1. `Start` takes the lock. If `running` is already true, a cycle is live, so it
   returns `ErrAlreadyRunning` and launches nothing — this is what stops a second
   Start from orphaning the first goroutine. Otherwise it makes *fresh* `stop` and
   `done` channels, stores them, sets `running = true`, and launches `run` with
   those exact channels passed as arguments.
2. `run` selects over its `stop`, `ctx.Done()`, and a ticker. It `defer close(done)`
   so that whichever way it leaves, the owner waiting on this cycle's done unblocks.
   On a tick it calls `poll` and adds the processed count to an atomic total.
3. `Stop` takes the lock. If `running` is false, nothing is live, so it returns
   `ErrNotRunning` — that covers both "never started" and "already stopped." Otherwise
   it snapshots the current `stop`/`done` into locals, sets `running = false` and
   nils the fields *under the lock*, then releases the lock and does
   `close(stop); <-done`.

Two details carry the correctness. First, the channels are passed to `run` as
arguments and snapshotted into locals in `Stop`, so a concurrent Start that
installs a new pair cannot make Stop close or wait on the wrong cycle's channels.
Second, `Stop` releases the mutex before `<-done`. Holding it across the wait would
deadlock the moment `poll` needed to touch shared state, and even here it is the
discipline that keeps Stop from blocking Start's critical section. Clearing
`running` under the lock is what makes a second Stop a clean `ErrNotRunning`
instead of a double-close panic. The cumulative counter is an `atomic.Int64`
because the run goroutine writes it while `Processed` reads it on the caller's
goroutine — `-race` would flag anything looser.

Create `poller.go`:

```go
package poller

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors for lifecycle misuse across restart cycles.
var (
	ErrAlreadyRunning = errors.New("poller: already running")
	ErrNotRunning     = errors.New("poller: not running")
)

// PollFunc performs one poll of an upstream queue and reports how many items it
// processed. It receives the context Start was called with, so a slow poll is
// cancellable.
type PollFunc func(ctx context.Context) (processed int, err error)

// Poller runs a background loop that calls poll on an interval. It is
// restartable: every Start begins a fresh cycle with its own stop and done
// channels and its own goroutine, and every Stop tears exactly that cycle down.
// Processed accumulates across every cycle for the life of the Poller.
type Poller struct {
	poll     PollFunc
	interval time.Duration

	mu      sync.Mutex
	running bool
	stop    chan struct{} // owner -> goroutine: close to ask this cycle to leave
	done    chan struct{} // goroutine -> owner: closed when this cycle has left

	total atomic.Int64
}

// New returns an idle Poller. It launches no goroutine until Start is called.
func New(poll PollFunc, interval time.Duration) *Poller {
	return &Poller{poll: poll, interval: interval}
}

// Start begins a new poll cycle. It allocates fresh stop and done channels so
// that no state survives from a previous cycle, then launches the run loop.
// If a cycle is already live it returns ErrAlreadyRunning and starts nothing,
// which is what prevents a second Start from orphaning the first goroutine.
func (p *Poller) Start(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running {
		return ErrAlreadyRunning
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	p.stop = stop
	p.done = done
	p.running = true
	go p.run(ctx, stop, done)
	return nil
}

// run is one cycle's work loop. It closes done on the way out so the Stop that
// owns this cycle unblocks. It leaves on an explicit Stop or on ctx
// cancellation, and accumulates processed counts on every tick.
func (p *Poller) run(ctx context.Context, stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := p.poll(ctx)
			if err == nil && n > 0 {
				p.total.Add(int64(n))
			}
		}
	}
}

// Stop ends the current cycle: it signals the cycle's goroutine to leave and
// waits for it to actually exit before returning. Stopping an idle Poller, or a
// second Stop within one cycle, returns ErrNotRunning rather than closing an
// already-closed channel. Snapshotting the channels under the lock and clearing
// the running flag makes Stop safe to race against Start and against itself.
func (p *Poller) Stop() error {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return ErrNotRunning
	}
	stop, done := p.stop, p.done
	p.running = false
	p.stop = nil
	p.done = nil
	p.mu.Unlock()

	close(stop) // signal: leave
	<-done      // wait: confirm this cycle's goroutine returned
	return nil
}

// Processed reports the cumulative number of items processed across every cycle
// this Poller has run. It is safe to call at any time.
func (p *Poller) Processed() int64 {
	return p.total.Load()
}
```

### The runnable demo

The demo runs three Stop-then-Start cycles against a synchronization-driven poll:
each poll signals on a channel, so the demo waits for a real poll to happen before
stopping the cycle rather than sleeping for one. It then exercises the misuse
guards and confirms the goroutine count returned to baseline. Every line of output
is a boolean fact, so the demo is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"example.com/poller"
)

func main() {
	// A synchronization-driven poll: it signals each call on polled and reports
	// one processed item, so the demo can wait for a real poll instead of sleeping.
	polled := make(chan struct{}, 1)
	poll := func(ctx context.Context) (int, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return 1, nil
	}

	p := poller.New(poll, time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Three Stop-then-Start cycles. Each cycle waits for at least one real poll,
	// so Processed is guaranteed to advance before the cycle is stopped.
	for cycle := 1; cycle <= 3; cycle++ {
		before := p.Processed()
		if err := p.Start(context.Background()); err != nil {
			fmt.Printf("cycle %d: start failed: %v\n", cycle, err)
			return
		}
		<-polled // a poll actually ran this cycle
		if err := p.Stop(); err != nil {
			fmt.Printf("cycle %d: stop failed: %v\n", cycle, err)
			return
		}
		fmt.Printf("cycle %d: processed increased: %v\n", cycle, p.Processed() > before)
	}

	// Misuse is rejected, not panicked on.
	_ = p.Start(context.Background())
	fmt.Println("double Start rejected:", errors.Is(p.Start(context.Background()), poller.ErrAlreadyRunning))
	_ = p.Stop()
	fmt.Println("Stop while idle rejected:", errors.Is(p.Stop(), poller.ErrNotRunning))

	fmt.Println("goroutines back to baseline:", waitBaseline(baseline))
}

// waitBaseline reports whether the live goroutine count returned to baseline
// within a generous window; a goroutine may still be mid-exit right after Stop.
func waitBaseline(baseline int) bool {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cycle 1: processed increased: true
cycle 2: processed increased: true
cycle 3: processed increased: true
double Start rejected: true
Stop while idle rejected: true
goroutines back to baseline: true
```

### Tests

The suite registers `goleak.VerifyTestMain(m)` in `TestMain`, so the whole run
fails if any cycle's goroutine is still parked at the end — the rigorous version of
the leak check. `TestCyclesAreLeakFree` runs twenty Stop-then-Start cycles and
asserts, after each one, that `runtime.NumGoroutine()` returned to the baseline
captured before the loop; a cycle that reused a channel or orphaned its goroutine
would push the count up and fail. `TestProcessedIncreasesEachCycle` waits for a
real poll each cycle and asserts the cumulative counter strictly increases, proving
every cycle genuinely runs the loop. `TestStartRejectsWhileRunning` and
`TestStopRejectsWhenIdle` pin the two misuse guards, including that a second Stop
returns `ErrNotRunning` rather than panicking on a double close.
`TestConcurrentStartStop` fires fifty goroutines each doing twenty Start/Stop
rounds; under `-race` it is the test that would expose any unsynchronized channel
handoff, and it asserts the only outcomes are `nil`, `ErrAlreadyRunning`, or
`ErrNotRunning` — never a panic. The polls are synchronization-driven (a buffered
signal channel), so no assertion depends on a sleep.

Create `poller_test.go`:

```go
package poller

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// Fail the run if any cycle's goroutine outlives the tests.
	goleak.VerifyTestMain(m)
}

// signalPoll returns a PollFunc that reports one processed item per call and
// pushes to polled on every call, so a test can wait for a real poll instead of
// sleeping for one.
func signalPoll() (PollFunc, chan struct{}) {
	polled := make(chan struct{}, 1)
	return func(ctx context.Context) (int, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return 1, nil
	}, polled
}

// waitBaseline waits until the live goroutine count drops back to at most
// baseline, giving a just-stopped goroutine time to finish exiting.
func waitBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("goroutine count %d did not return to baseline %d", runtime.NumGoroutine(), baseline)
}

// TestCyclesAreLeakFree runs many Stop-then-Start cycles and asserts the live
// goroutine count returns to baseline after every one: no cycle orphans its
// goroutine.
func TestCyclesAreLeakFree(t *testing.T) {
	poll, polled := signalPoll()
	p := New(poll, time.Millisecond)

	baseline := runtime.NumGoroutine()
	for range 20 {
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-polled
		if err := p.Stop(); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		waitBaseline(t, baseline)
	}
}

// TestProcessedIncreasesEachCycle proves each cycle actually polls: the
// cumulative counter strictly increases from one cycle to the next.
func TestProcessedIncreasesEachCycle(t *testing.T) {
	poll, polled := signalPoll()
	p := New(poll, time.Millisecond)

	for cycle := range 5 {
		before := p.Processed()
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("cycle %d Start: %v", cycle, err)
		}
		<-polled
		if err := p.Stop(); err != nil {
			t.Fatalf("cycle %d Stop: %v", cycle, err)
		}
		if after := p.Processed(); after <= before {
			t.Fatalf("cycle %d: Processed did not increase: before=%d after=%d", cycle, before, after)
		}
	}
}

// TestStartRejectsWhileRunning proves a second Start within a live cycle is
// rejected rather than orphaning the first goroutine.
func TestStartRejectsWhileRunning(t *testing.T) {
	poll, _ := signalPoll()
	p := New(poll, time.Millisecond)

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	if err := p.Start(context.Background()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Start = %v, want ErrAlreadyRunning", err)
	}
}

// TestStopRejectsWhenIdle proves Stop on an idle Poller and a second Stop within
// one cycle both return ErrNotRunning, never a double-close panic.
func TestStopRejectsWhenIdle(t *testing.T) {
	poll, _ := signalPoll()
	p := New(poll, time.Millisecond)

	if err := p.Stop(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Stop while idle = %v, want ErrNotRunning", err)
	}

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := p.Stop(); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("second Stop = %v, want ErrNotRunning", err)
	}
}

// TestConcurrentStartStop hammers Start and Stop from many goroutines at once.
// It must be race-clean and never panic on a closed channel; the only legal
// outcomes are nil, ErrAlreadyRunning, and ErrNotRunning.
func TestConcurrentStartStop(t *testing.T) {
	poll, _ := signalPoll()
	p := New(poll, time.Millisecond)

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			for range 20 {
				if err := p.Start(context.Background()); err != nil && !errors.Is(err, ErrAlreadyRunning) {
					t.Errorf("Start = %v", err)
				}
				if err := p.Stop(); err != nil && !errors.Is(err, ErrNotRunning) {
					t.Errorf("Stop = %v", err)
				}
			}
		})
	}
	wg.Wait()

	// Drain any cycle the storm left live, then confirm no goroutine leaked.
	_ = p.Stop()
	waitBaseline(t, baseline)
}
```

## Review

The poller is correct when a cycle owns its channels and a restart shares nothing
with the cycle before it. Allocating a fresh `stop`/`done` pair in every `Start`
and passing them by value to `run` is the invariant: no channel is ever closed
twice and no cycle can be signalled by another cycle's owner. `Stop` is
signal-then-wait — `close(stop)` then `<-done` — with the flag cleared and channels
snapshotted under the lock, which is what makes a second Stop a clean
`ErrNotRunning` and a concurrent Start/Stop race-safe. `TestCyclesAreLeakFree` and
`goleak.VerifyTestMain` prove the leak-freedom the whole exercise is about: twenty
cycles, baseline goroutine count every time. The production bug this pattern
prevents is the one that passes a casual single-cycle test and dies slowly under a
feature flag that toggles the poller all day — a reused closed channel that panics
on the second Stop, or an orphaned goroutine per restart that climbs
`NumGoroutine()` until the box is OOM-killed. Run `go test -count=1 -race ./...`;
`-race` and goleak together turn both failure modes from invisible to caught.

## Resources

- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) -- close-as-broadcast and why a closed channel is one-shot.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector used from `TestMain` to fail on any surviving goroutine.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) -- the lock-free counter shared between the run goroutine and `Processed`.
- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) -- the live count a per-cycle leak test asserts against baseline.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-outbox-relay-drain-on-stop.md](10-outbox-relay-drain-on-stop.md) | Next: [12-run-group-coordinated-component-shutdown.md](12-run-group-coordinated-component-shutdown.md)
