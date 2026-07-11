# Exercise 8: Ship a goroutine-count watchdog that flags unbounded growth under load

A slow goroutine leak does not announce itself — the count creeps up over hours until
the service OOMs. This exercise builds the observability hook you register at startup:
a watchdog that samples the goroutine count on an interval, tracks a high-water mark,
and fires an alert when the count grows past a threshold, so a leak is caught long
before memory runs out.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
watchdog/                 independent module: example.com/watchdog
  go.mod
  watchdog.go             Watchdog: Start (samples on a ticker), HighWater, Baseline, Alerts; Live
  cmd/demo/main.go         a leaking workload trips the alert
  watchdog_test.go         bounded load stays near baseline; leak trips the alert; cancel stops the loop
```

- Files: `watchdog.go`, `cmd/demo/main.go`, `watchdog_test.go`.
- Implement: `New(interval, threshold)`; `Start(ctx)` that snapshots a baseline and samples `pprof.Lookup("goroutine").Count()` on a ticker in its own goroutine, updating a high-water mark and sending on an alert channel once the count exceeds `baseline+threshold`; the loop exits on `ctx.Done()`.
- Test: a bounded workload keeps the high-water near baseline and never alerts; a leaking workload trips the alert with a count above baseline; cancelling stops the loop with no leaked watchdog goroutine.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/watchdog/cmd/demo
cd ~/go-exercises/watchdog
go mod init example.com/watchdog
```

### What the watchdog measures, and why a high-water mark

The signature of a leak is the goroutine count trending upward while load is steady.
The watchdog samples that count on an interval and keeps two derived signals. The
*high-water mark* is the largest count seen since start — a monotone record that
survives transient dips, so a leak that grows and momentarily pauses is still visible.
The *alert* fires once when the count first exceeds `baseline + threshold`: the
baseline is captured at `Start` (so the service's steady-state goroutines do not
count against it), and the threshold is the growth you are willing to tolerate before
paging someone. The alert is sent on a buffered channel and guarded by an
`atomic.Bool` so it fires exactly once, not on every subsequent sample.

The sampling loop lives in its own goroutine driven by a `time.Ticker`, and it exits
on `ctx.Done()`. That last point is the discipline the lesson enforces on itself: an
observability hook that leaks its own goroutine is worse than useless, so `Start`
takes a context and the loop returns when it is cancelled — a fact the third test
verifies by watching the process's goroutine count return to where it was before
`Start`.

The count comes from `pprof.Lookup("goroutine").Count()`; `runtime.NumGoroutine()`
reports the same live count and is exposed as `Live` for callers that just want a
number. The high-water update is a compare-and-swap loop so a future multi-sampler
design stays correct under `-race`.

Create `watchdog.go`:

```go
package watchdog

import (
	"context"
	"runtime"
	"runtime/pprof"
	"sync/atomic"
	"time"
)

// Watchdog samples the live goroutine count on an interval, tracks a high-water
// mark, and alerts once the count exceeds baseline+threshold.
type Watchdog struct {
	interval  time.Duration
	threshold int64
	baseline  int64
	highWater atomic.Int64
	alerted   atomic.Bool
	alert     chan int64
}

// New returns a Watchdog that samples every interval and alerts when the count
// grows more than threshold above the baseline captured at Start.
func New(interval time.Duration, threshold int) *Watchdog {
	return &Watchdog{
		interval:  interval,
		threshold: int64(threshold),
		alert:     make(chan int64, 1),
	}
}

// Start captures the baseline and begins sampling until ctx is cancelled. It
// returns immediately; the sampling loop runs in its own goroutine that exits on
// cancel, so it does not leak.
func (w *Watchdog) Start(ctx context.Context) {
	w.baseline = count()
	w.highWater.Store(w.baseline)
	go w.loop(ctx)
}

func (w *Watchdog) loop(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.sample()
		}
	}
}

func (w *Watchdog) sample() {
	n := count()
	for {
		hw := w.highWater.Load()
		if n <= hw || w.highWater.CompareAndSwap(hw, n) {
			break
		}
	}
	if n > w.baseline+w.threshold && w.alerted.CompareAndSwap(false, true) {
		select {
		case w.alert <- n:
		default:
		}
	}
}

// HighWater is the largest goroutine count observed since Start.
func (w *Watchdog) HighWater() int64 { return w.highWater.Load() }

// Baseline is the goroutine count captured at Start.
func (w *Watchdog) Baseline() int64 { return w.baseline }

// Alerts delivers the count at the moment growth first crossed the threshold.
func (w *Watchdog) Alerts() <-chan int64 { return w.alert }

// Live reports the number of goroutines that currently exist.
func Live() int { return runtime.NumGoroutine() }

func count() int64 { return int64(pprof.Lookup("goroutine").Count()) }
```

### The runnable demo

The demo starts a watchdog with a small interval and a low threshold, then leaks 100
goroutines parked on a channel. The alert fires with a count above the baseline; the
demo prints that fact, then releases the goroutines and cancels.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/watchdog"
)

func main() {
	w := watchdog.New(time.Millisecond, 20)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	block := make(chan struct{})
	for range 100 {
		go func() { <-block }()
	}

	n := <-w.Alerts()
	fmt.Println("alert fired:", n > w.Baseline())

	close(block)
	cancel()
	fmt.Println("watchdog can read live count:", watchdog.Live() > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
alert fired: true
watchdog can read live count: true
```

### Tests

`TestBoundedStaysNearBaseline` runs a bounded workload (goroutines spawned and joined
in small batches) and asserts the watchdog never alerts and its high-water stays under
the threshold. `TestLeakTripsAlert` leaks 100 goroutines and asserts the alert fires
with a count above baseline. `TestCancelStopsLoop` cancels the context and asserts the
watchdog's own goroutine exits. These tests observe the process-wide goroutine count,
so they run sequentially, not in parallel.

Create `watchdog_test.go`:

```go
package watchdog

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestBoundedStaysNearBaseline(t *testing.T) {
	w := New(time.Millisecond, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	for range 20 {
		var wg sync.WaitGroup
		for range 5 {
			wg.Add(1)
			go func() { defer wg.Done() }()
		}
		wg.Wait()
	}
	time.Sleep(20 * time.Millisecond)

	select {
	case <-w.Alerts():
		t.Fatal("watchdog alerted on a bounded workload")
	default:
	}
	if w.HighWater() > w.Baseline()+w.threshold {
		t.Errorf("high-water %d exceeded baseline+threshold %d", w.HighWater(), w.Baseline()+w.threshold)
	}
}

func TestLeakTripsAlert(t *testing.T) {
	w := New(time.Millisecond, 20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)

	block := make(chan struct{})
	for range 100 {
		go func() { <-block }()
	}

	select {
	case n := <-w.Alerts():
		if n <= w.Baseline() {
			t.Errorf("alert count %d not above baseline %d", n, w.Baseline())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not alert on goroutine growth")
	}
	close(block) // release the leaked goroutines
}

func TestCancelStopsLoop(t *testing.T) {
	before := Live()
	w := New(time.Millisecond, 10)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	time.Sleep(10 * time.Millisecond)
	cancel()

	for range 200 {
		if Live() <= before {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("watchdog goroutine leaked: before=%d after=%d", before, Live())
}
```

## Review

The watchdog is correct when it is quiet under bounded load and loud under a leak, and
when it cleans up after itself. The baseline captured at `Start` is what lets the
threshold mean "growth" rather than "absolute count," so the bounded test's transient
batches of five goroutines stay well under a threshold of fifty and never alert. The
`atomic.Bool` guard makes the alert fire once, not once per tick, which is what you
want paging a human. And `TestCancelStopsLoop` enforces the rule that an observability
hook must not become the leak it hunts: after cancel, the sampling goroutine returns
and the process count falls back to baseline. Because all three tests read the global
goroutine count, they must not run in parallel — a neighbor's leaked goroutines would
skew another's baseline. Run `go test -race` to confirm the high-water CAS and the
alert flag are race-free.

## Resources

- [`runtime/pprof.Profile.Count`](https://pkg.go.dev/runtime/pprof#Profile.Count) — the goroutine count the watchdog samples.
- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the same live count, exposed as `Live`.
- [`time.Ticker`](https://pkg.go.dev/time#Ticker) — the interval driver, stopped on cancel to free its resources.

---

Prev: [07-diagnose-wedged-worker-pool.md](07-diagnose-wedged-worker-pool.md) | Back to [00-concepts.md](00-concepts.md) | Next: [09-dump-stacks-on-shutdown-timeout.md](09-dump-stacks-on-shutdown-timeout.md)
