# Exercise 9: Fail the Suite on Leaked Goroutines — A Package-Scope Leak Gate in TestMain

A background worker, ticker, or server that a test forgets to stop leaks a
goroutine. No individual test notices, but the leak compounds and hides real
shutdown bugs. Because `TestMain` sees the exit code before the process ends, it
can enforce a package-scope invariant that no single test can: record a baseline
goroutine count before `m.Run()`, and fail the whole package if the count stays
above baseline afterward. This is the mechanism behind `go.uber.org/goleak`,
built here from the standard library.

This module is fully self-contained: its own `go mod init`, worker, demo, and
tests. Nothing here imports any other exercise.

## What you'll build

```text
leakgate/                      independent module: example.com/leakgate
  go.mod                       go 1.26
  worker.go                    Heartbeat: a stoppable background worker
  cmd/
    demo/
      main.go                  runnable demo: start, tick, stop cleanly
  worker_test.go               TestMain gates leaked goroutines after m.Run()
```

Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
Implement: `Heartbeat(ctx, interval, tick, wg)` — a worker that stops promptly on cancellation and signals via a `WaitGroup`.
Test: a `TestMain`/`run()` that captures `runtime.NumGoroutine()` before `m.Run()`, settles-and-polls after, and returns non-zero if goroutines leaked; a well-behaved test that starts and stops a worker.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/09-goroutine-leak-gate-after-run/cmd/demo
cd go-solutions/12-testing-ecosystem/22-testmain-setup-teardown/09-goroutine-leak-gate-after-run
```

### How the gate works, and why it must settle

The gate is three lines of logic wrapped in patience. Before any test runs,
capture `base := runtime.NumGoroutine()`. Run the suite. If it passed
(`code == 0`), check whether goroutines are still above `base`. If they are, print
the offending count to stderr and override the exit code to 1.

The subtlety is that goroutines exit *asynchronously*: when a test cancels its
worker's context, the worker does not stop at that instant — it wakes, runs its
deferred cleanup, and returns a moment later. If the gate reads the count the
instant `m.Run()` returns, a goroutine that is milliseconds from exiting looks like
a leak. So the gate must *settle*: poll `runtime.NumGoroutine()` a bounded number
of times, yielding with `runtime.Gosched()` and sleeping briefly between reads,
and only declare a leak if the count stays high through the whole window. This is
the same settle-and-retry that goleak does, and it is why the gate is inherently a
little flaky — you are racing goroutine teardown and must give it slack.

Two guardrails keep it honest. First, only tighten a *passing* run: if the suite
already failed (`code != 0`), report that, do not let the leak check mask or
replace a real test failure. Second, the well-behaved worker in this module stops
deterministically (it waits on a `WaitGroup`), so a clean suite reliably returns to
baseline and exits 0. A leaking variant — say a ticker started with no way to
cancel it — would keep the count above baseline through the whole settle window
and flip the suite red, which is exactly the bug you want caught.

### The well-behaved worker

`Heartbeat` is the artifact under watch: a background worker that ticks on an
interval and stops the moment its context is cancelled, signaling completion
through a `WaitGroup` so a test can *guarantee* it has exited before returning.
That guarantee is what keeps the suite under baseline.

Create `worker.go`:

```go
package leakgate

import (
	"context"
	"sync"
	"time"
)

// Heartbeat runs until ctx is cancelled, calling tick on every interval. It is a
// well-behaved worker: it stops promptly on cancellation, stops its ticker, and
// signals completion via wg.Done, so a caller that wg.Wait()s knows it has
// exited and left no goroutine behind.
func Heartbeat(ctx context.Context, interval time.Duration, tick func(), wg *sync.WaitGroup) {
	defer wg.Done()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
```

### The runnable demo

The demo starts the worker, lets it tick, cancels, and waits for it to exit — the
clean-shutdown shape that keeps the leak gate green.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"example.com/leakgate"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var ticks atomic.Int64

	fmt.Println("worker started")
	wg.Add(1)
	go leakgate.Heartbeat(ctx, time.Millisecond, func() { ticks.Add(1) }, &wg)

	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait() // guarantees the goroutine has exited

	fmt.Println("worker stopped cleanly")
	fmt.Printf("ticks observed: %v\n", ticks.Load() > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker started
worker stopped cleanly
ticks observed: true
```

### Tests

`TestMain` installs the leak gate via `run()`. `TestHeartbeatStopsCleanly` starts a
worker, observes at least one tick, cancels, and `wg.Wait()`s so the goroutine is
gone before the test returns — leaving the count at baseline. Because every test
here stops what it starts, the gate lets the suite exit 0.

Create `worker_test.go`:

```go
package leakgate

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func run(m *testing.M) int {
	base := runtime.NumGoroutine()
	code := m.Run()
	if code == 0 {
		if n, leaked := settle(base); leaked {
			fmt.Fprintf(os.Stderr, "goroutine leak: baseline %d, still running %d\n", base, n)
			return 1
		}
	}
	return code
}

// settle gives stragglers a bounded window to exit, then reports the final count
// and whether it stayed above baseline.
func settle(base int) (final int, leaked bool) {
	const attempts = 100
	for range attempts {
		runtime.Gosched()
		if n := runtime.NumGoroutine(); n <= base {
			return n, false
		}
		time.Sleep(10 * time.Millisecond)
	}
	runtime.GC()
	final = runtime.NumGoroutine()
	return final, final > base
}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func TestHeartbeatStopsCleanly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var ticks atomic.Int64

	wg.Add(1)
	go Heartbeat(ctx, time.Millisecond, func() { ticks.Add(1) }, &wg)

	// Let it tick at least once.
	deadline := time.Now().Add(time.Second)
	for ticks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	cancel()
	wg.Wait() // the worker has now exited; no goroutine leaks

	if ticks.Load() == 0 {
		t.Fatal("worker never ticked")
	}
}

func TestHeartbeatStopsImmediatelyIfCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the worker starts

	var wg sync.WaitGroup
	wg.Add(1)
	go Heartbeat(ctx, time.Hour, func() { t.Error("tick should not fire") }, &wg)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop on an already-cancelled context")
	}
}
```

## Review

The gate is correct when the baseline is captured before `m.Run()`, the post-run
check only tightens a passing suite, and the check *settles* — polling with
`runtime.Gosched()` and short sleeps — before declaring a leak, because goroutines
exit asynchronously. The well-behaved `Heartbeat` stops on cancellation and
signals through a `WaitGroup`, so the tests can `wg.Wait()` and guarantee the
goroutine is gone; that is what keeps the suite at baseline and green. A worker
started with no cancellation path would stay above baseline through the whole
window and flip the suite red — the exact leak this catches. This stdlib version
teaches the mechanism; real projects use `go.uber.org/goleak`, which hardens the
baseline (ignoring known runtime goroutines) and the settling to reduce the
inherent flakiness. Run `go test -race` to confirm the worker and its `WaitGroup`
handshake are race-free.

## Resources

- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the count the gate compares against a baseline.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — the production leak detector this exercise reimplements in miniature.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) — the cancellation signal a stoppable worker must watch.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [08-testing-os-exit-with-subprocess.md](08-testing-os-exit-with-subprocess.md) | Next: [../23-snapshot-approval-testing/00-concepts.md](../23-snapshot-approval-testing/00-concepts.md)
