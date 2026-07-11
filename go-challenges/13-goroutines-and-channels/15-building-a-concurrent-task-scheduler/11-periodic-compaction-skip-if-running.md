# Exercise 11: Non-Overlapping Periodic Runner: Skip the Tick While Busy

**Level: Intermediate**

A background compaction job (or a replication-lag sampler) must run on a fixed
interval, but two runs must never overlap: if a run is still going when the next
tick fires, that tick has to be skipped and counted, not stacked into a second
concurrent run. The naive `for range ticker.C { go compact() }` does the opposite
-- under a slow disk it launches a new compaction on every tick, piling up
goroutines and doubling the I/O the job was meant to smooth out. This module
builds a runner that owns a ticker plus a single in-flight guard, so at most one
run executes at any instant and the missed ticks show up in the metrics.

This module is self-contained: its own module, a `periodic` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
periodic/                    independent module: example.com/periodic
  go.mod                     go 1.26
  periodic.go                Runner: ticker + single in-flight guard + cancel
  cmd/demo/main.go           runnable demo: skip-while-busy, failures counted, idempotent Stop
  periodic_test.go           overlap invariant, fast-run cadence, failures, Stop semantics, goleak
```

- Files: `periodic.go`, `cmd/demo/main.go`, `periodic_test.go`.
- Implement: `New(interval, run) *Runner`, `(*Runner).Start()`, `(*Runner).Stop(ctx) error`, `(*Runner).Stats() Stats`, with `type RunFunc func(ctx context.Context) error` and `type Stats struct{ Runs, Skipped, Failures int64 }`.
- Test: ticks arriving mid-run are counted as `Skipped` and never overlap (a shared atomic never exceeds 1); a fast run executes about once per tick; an erroring run increments `Failures` without stopping; `Stop` blocks for the in-flight run, is idempotent, and returns promptly on an already-cancelled ctx without abandoning accounting; goleak confirms the ticker goroutine is gone.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/periodic/cmd/demo
cd ~/go-exercises/periodic
go mod init example.com/periodic
go mod edit -go=1.26
go get go.uber.org/goleak
go mod tidy
```

### One guard, decided at the tick, not inside the run

The whole exercise turns on a single question asked at every tick: is a run
already in flight? The answer is a `sync/atomic.Bool` used as a guard. The tick
handler does `inflight.CompareAndSwap(false, true)`. If the swap succeeds the
guard was free, so it launches the run; if it fails a run is already active, so it
records a skip and returns. The guard is released -- `inflight.Store(false)` --
only *after* `RunFunc` returns, in a deferred statement on the run goroutine.

That ordering is the overlap invariant. Because the guard is set true before the
run starts and set false only after it finishes, any tick that fires during the
run finds the guard held and is turned away. No second run can begin until the
first has fully returned. The result is that a shared counter incremented on entry
to `RunFunc` and decremented on exit can never read greater than 1 -- and that is
exactly what the test asserts, structurally, rather than by hoping the timing
lines up.

Three design points make the runner production-correct:

1. **The ticker is owned by one goroutine.** `time.NewTicker` is created inside
   the loop goroutine and stopped in that same goroutine's `defer`. Nothing else
   ever touches it, so there is no data race on the ticker and no question about
   who stops it.
2. **The run happens on its own goroutine, not on the loop goroutine.** If the
   loop ran `RunFunc` inline it could not receive the next tick until the run
   finished, and `time.Ticker` would silently coalesce the ticks it missed -- you
   would lose the skip count. Launching the run on a separate goroutine keeps the
   loop free to receive every tick and account for each one as a run or a skip.
3. **`Stop` is bounded by a context.** Stopping cancels the runner context (which
   both halts the ticker loop and signals a cooperating `RunFunc`) and then waits
   for the in-flight run, but only up to the caller's `ctx`. If the run is wedged,
   `Stop` returns `ctx.Err()` instead of hanging the deploy forever; the run's
   accounting still lands when it eventually finishes.

Create `periodic.go`:

```go
package periodic

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// RunFunc is the periodic job. It should observe ctx so that Stop can bound the
// wait for an in-flight run; a job that ignores ctx runs to completion.
type RunFunc func(ctx context.Context) error

// Stats is a coherent snapshot of the runner's counters.
type Stats struct {
	Runs, Skipped, Failures int64
}

// ErrStopped is the cancellation cause set on the runner context when Stop runs.
var ErrStopped = errors.New("periodic: runner stopped")

// Runner drives RunFunc on a fixed interval with a single in-flight guard: at
// most one run executes at any instant. A tick that arrives while a run is still
// going is skipped and counted, never stacked into a second concurrent run.
type Runner struct {
	interval time.Duration
	run      RunFunc

	ctx    context.Context
	cancel context.CancelCauseFunc

	startOnce sync.Once
	stopOnce  sync.Once
	started   atomic.Bool
	loopDone  chan struct{} // closed when the ticker goroutine exits

	inflight atomic.Bool // the single in-flight guard: true iff a run is active
	wg       sync.WaitGroup

	runs     atomic.Int64
	skipped  atomic.Int64
	failures atomic.Int64
}

// New builds a stopped runner. interval must be > 0.
func New(interval time.Duration, run RunFunc) *Runner {
	if interval <= 0 {
		panic("periodic: interval must be > 0")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Runner{
		interval: interval,
		run:      run,
		ctx:      ctx,
		cancel:   cancel,
		loopDone: make(chan struct{}),
	}
}

// Start begins ticking. It is idempotent: extra calls are no-ops.
func (r *Runner) Start() {
	r.startOnce.Do(func() {
		r.started.Store(true)
		go r.loop()
	})
}

// loop owns the ticker; it is the only goroutine that touches it.
func (r *Runner) loop() {
	defer close(r.loopDone)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.tick()
		}
	}
}

// tick launches a run only if the guard is free; otherwise it records a skip.
func (r *Runner) tick() {
	if !r.inflight.CompareAndSwap(false, true) {
		r.skipped.Add(1)
		return
	}
	r.wg.Add(1)
	go func() {
		// Release the guard only after RunFunc returns, so the next tick can
		// never start a second run that overlaps this one.
		defer r.wg.Done()
		defer r.inflight.Store(false)
		err := r.run(r.ctx)
		r.runs.Add(1)
		if err != nil {
			r.failures.Add(1)
		}
	}()
}

// Stop stops ticking and waits for the in-flight run, bounded by ctx. It is
// idempotent and safe to call before Start. If ctx expires first it returns
// ctx.Err() promptly; the in-flight run keeps its accounting and a later Stop
// (or the run's own completion) drains it.
func (r *Runner) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() { r.cancel(ErrStopped) })

	done := make(chan struct{})
	go func() {
		if r.started.Load() {
			<-r.loopDone
		}
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns a snapshot built from atomic loads.
func (r *Runner) Stats() Stats {
	return Stats{
		Runs:     r.runs.Load(),
		Skipped:  r.skipped.Load(),
		Failures: r.failures.Load(),
	}
}
```

The `Stop` waiter deserves a second look. It joins on `loopDone` (the ticker
goroutine's exit) only when `started` is true, so `Stop` is safe on a runner that
was never started. All `wg.Add` calls happen inside `tick`, which runs on the loop
goroutine before it closes `loopDone`; the waiter reads `loopDone` before calling
`wg.Wait`, so `Add` never races `Wait`. That is the `WaitGroup` contract, honored
by construction rather than by luck.

### The runnable demo

The demo is deterministic by design: it prints booleans and the peak-concurrency
count, never the raw skip total (which is timing-dependent). Part 1 gates one run
open and waits until at least one tick has been skipped, tracking the maximum
number of concurrent runs -- which the guard pins at 1. Part 2 runs an always-
failing job and confirms every run is counted as a failure while the schedule
keeps going. Part 3 shows a second `Stop` returning cleanly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/periodic"
)

func main() {
	// Part 1: a slow run held open by a gate. Ticks that arrive while the run is
	// in flight are skipped, and at no instant do two runs overlap.
	var concurrent, maxConcurrent atomic.Int64
	started := make(chan struct{}, 1)
	gate := make(chan struct{})

	slow := periodic.New(2*time.Millisecond, func(ctx context.Context) error {
		n := concurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if n <= old || maxConcurrent.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-gate
		concurrent.Add(-1)
		return nil
	})
	slow.Start()

	<-started // the first run is now gated open
	for slow.Stats().Skipped == 0 {
		time.Sleep(time.Millisecond) // wait until at least one tick is skipped
	}
	close(gate)
	_ = slow.Stop(context.Background())
	s1 := slow.Stats()

	fmt.Println("slow run, ticks arriving mid-run:")
	fmt.Printf("  at least one run:        %v\n", s1.Runs >= 1)
	fmt.Printf("  at least one skip:       %v\n", s1.Skipped >= 1)
	fmt.Printf("  max concurrent runs:     %d\n", maxConcurrent.Load())

	// Part 2: a run that always fails increments Failures without stopping.
	failing := periodic.New(2*time.Millisecond, func(ctx context.Context) error {
		return errors.New("compaction failed")
	})
	failing.Start()
	for failing.Stats().Runs < 3 {
		time.Sleep(time.Millisecond)
	}
	_ = failing.Stop(context.Background())
	s2 := failing.Stats()

	fmt.Println("failing run:")
	fmt.Printf("  runs recorded:           %v\n", s2.Runs >= 3)
	fmt.Printf("  every run counted fail:  %v\n", s2.Failures == s2.Runs)

	// Part 3: Stop is idempotent; a second Stop returns without panicking.
	err := failing.Stop(context.Background())
	fmt.Println("idempotent stop:")
	fmt.Printf("  second stop error:       %v\n", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
slow run, ticks arriving mid-run:
  at least one run:        true
  at least one skip:       true
  max concurrent runs:     1
failing run:
  runs recorded:           true
  every run counted fail:  true
idempotent stop:
  second stop error:       <nil>
```

### Tests

`TestNoOverlapSkipsCounted` is the core assertion: it holds one run open, waits
for several skips to accumulate, and checks a shared atomic incremented on entry
and decremented on exit of `RunFunc` never exceeds 1. `TestFastRunRunsPerTick`
proves a fast run advances about once per tick -- runs accumulate and never fall
behind skips. `TestFailuresCountedScheduleContinues` shows an erroring run
increments `Failures` on every run without stopping the schedule.
`TestStopIdempotent` calls `Stop` twice and also on a never-started runner.
`TestStopCancelledCtxReturnsPromptly` calls `Stop` with an already-cancelled ctx
while a run is wedged, expects `context.Canceled` promptly, then drains cleanly to
prove accounting was not abandoned. `TestNoGoroutineLeak` uses goleak to confirm
the ticker goroutine and every run goroutine are gone after `Stop`.

Create `periodic_test.go`:

```go
package periodic

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestNoOverlapSkipsCounted holds one run open with a gate. Every tick that
// arrives while the run is in flight must be counted as Skipped, and a shared
// atomic incremented on entry and decremented on exit of RunFunc must never read
// greater than 1 -- the overlap invariant, asserted structurally.
func TestNoOverlapSkipsCounted(t *testing.T) {
	var concurrent atomic.Int32
	var overlap atomic.Bool
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	r := New(time.Millisecond, func(ctx context.Context) error {
		if concurrent.Add(1) > 1 {
			overlap.Store(true)
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		concurrent.Add(-1)
		return nil
	})
	r.Start()

	<-started // the first run is now gated open
	for r.Stats().Skipped < 3 {
		time.Sleep(time.Millisecond) // ticks fire every 1ms while the run blocks
	}
	close(release)
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if overlap.Load() {
		t.Fatal("two runs overlapped: the in-flight guard failed")
	}
	st := r.Stats()
	if st.Skipped < 3 {
		t.Fatalf("Skipped = %d, want >= 3", st.Skipped)
	}
	if st.Runs < 1 {
		t.Fatalf("Runs = %d, want >= 1", st.Runs)
	}
}

// TestFastRunRunsPerTick checks that a fast RunFunc executes about once per tick:
// runs accumulate and never fall behind skips.
func TestFastRunRunsPerTick(t *testing.T) {
	r := New(2*time.Millisecond, func(ctx context.Context) error { return nil })
	r.Start()
	for r.Stats().Runs < 5 {
		time.Sleep(time.Millisecond)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	st := r.Stats()
	if st.Runs < 5 {
		t.Fatalf("Runs = %d, want >= 5", st.Runs)
	}
	if st.Skipped > st.Runs {
		t.Fatalf("Skipped %d exceeded Runs %d for a fast RunFunc", st.Skipped, st.Runs)
	}
}

// TestFailuresCountedScheduleContinues asserts a RunFunc returning an error
// increments Failures on every run without stopping the schedule.
func TestFailuresCountedScheduleContinues(t *testing.T) {
	r := New(time.Millisecond, func(ctx context.Context) error {
		return errors.New("boom")
	})
	r.Start()
	for r.Stats().Runs < 5 {
		time.Sleep(time.Millisecond)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	st := r.Stats()
	if st.Runs < 5 {
		t.Fatalf("schedule stopped early: Runs = %d, want >= 5", st.Runs)
	}
	if st.Failures != st.Runs {
		t.Fatalf("Failures = %d, Runs = %d; want equal", st.Failures, st.Runs)
	}
}

// TestStopIdempotent proves Stop can be called twice, and can be called on a
// runner that was never started, without error or panic.
func TestStopIdempotent(t *testing.T) {
	r := New(time.Millisecond, func(ctx context.Context) error { return nil })
	r.Start()
	for r.Stats().Runs < 1 {
		time.Sleep(time.Millisecond)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	r2 := New(time.Millisecond, func(ctx context.Context) error { return nil })
	if err := r2.Stop(context.Background()); err != nil {
		t.Fatalf("Stop without Start: %v", err)
	}
}

// TestStopCancelledCtxReturnsPromptly holds a run open, then calls Stop with an
// already-cancelled ctx. Stop must return context.Canceled promptly and must not
// abandon accounting: a later clean Stop drains the in-flight run.
func TestStopCancelledCtxReturnsPromptly(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	r := New(time.Millisecond, func(ctx context.Context) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	r.Start()
	<-started // a run is now in flight and blocked

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Stop is called
	if err := r.Stop(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop(cancelled) = %v, want context.Canceled", err)
	}

	// Accounting is not abandoned: release the run and drain cleanly.
	close(release)
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("clean Stop: %v", err)
	}
	if r.Stats().Runs < 1 {
		t.Fatalf("in-flight run was not accounted: Runs = %d", r.Stats().Runs)
	}
}

// TestNoGoroutineLeak confirms the ticker goroutine and every run goroutine are
// gone after a clean Stop.
func TestNoGoroutineLeak(t *testing.T) {
	defer goleak.VerifyNone(t)

	r := New(time.Millisecond, func(ctx context.Context) error { return nil })
	r.Start()
	for r.Stats().Runs < 3 {
		time.Sleep(time.Millisecond)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
```

## Review

"Correct" here means two runs never overlap and every missed tick is accounted
for. The overlap guarantee comes from a single `atomic.Bool` guard set true at the
tick via `CompareAndSwap` and released only after `RunFunc` returns: a tick that
arrives mid-run finds the guard held, records a skip, and starts nothing. Because
release strictly follows completion, a counter incremented on entry and
decremented on exit of `RunFunc` can never exceed 1, which `TestNoOverlapSkipsCounted`
asserts structurally rather than by timing. `Stop` cancels the runner context to
halt the ticker loop and signal a cooperating run, then waits for the in-flight
run bounded by the caller's context -- idempotent via `sync.Once`, safe before
`Start`, and prompt on an already-cancelled ctx without dropping the run's
accounting. goleak proves the ticker goroutine is gone afterward. The production
bug this prevents is the `for range ticker.C { go job() }` pileup: under a slow
dependency it stacks goroutines and multiplies the I/O the periodic job existed to
smooth, until the box falls over -- exactly when it was already under stress.

## Resources

- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) -- delivers ticks on a channel and drops rather than queues when the receiver is slow; why the loop, not the ticker, must count skips.
- [`sync/atomic`](https://pkg.go.dev/sync/atomic) -- `atomic.Bool.CompareAndSwap` as the in-flight guard and `atomic.Int64` for torn-read-free counters.
- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) -- cancel with a documented cause so a cooperating run learns why it was asked to stop.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- fails a test if the ticker or run goroutines outlive Stop.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-outbox-microbatch-size-or-linger-flush.md](10-outbox-microbatch-size-or-linger-flush.md) | Next: [12-webhook-fair-share-tenant-dispatch.md](12-webhook-fair-share-tenant-dispatch.md)
