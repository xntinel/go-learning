# Exercise 3: Goroutine-Leak Harness — Prove Every Stage Exits On Cancel

A stage that blocks forever on an unread send is invisible to functional tests: the
data still looks right, the test still passes, and the leaked goroutine only shows
up as a slow memory climb in production. The defense is a CI canary — a reusable
harness that snapshots `runtime.NumGoroutine()` before a stage runs, cancels and
drains it, then polls until the count settles back to baseline. This module builds
that harness and uses it to prove a correct stage exits and to demonstrate it
catching a deliberately broken one, including the classic 100-element
`TestFanOutStopsOnCancel` canary.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
leakharness/                 module example.com/leakharness
  go.mod
  stage.go                   Generate, FanOut (correct, select-guarded sends)
  leak.go                    func Settles(before, threshold, timeout) bool  (poll loop)
  cmd/
    demo/
      main.go                runs a stage, cancels, reports before/after goroutine counts
  leak_test.go               no-leak-after-cancel, FanOutStopsOnCancel (100 elems)
```

Files: `stage.go`, `leak.go`, `cmd/demo/main.go`, `leak_test.go`.
Implement: correct `Generate`/`FanOut` stages and a `Settles(before, threshold,
timeout)` poll-until-settle helper over `runtime.NumGoroutine()`.
Test: `TestNoGoroutineLeakAfterCancel` and the `TestFanOutStopsOnCancel` canary
(100-element source, cancel after 5 reads, drain, assert baseline).
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/03-goroutine-leak-harness/cmd/demo
cd go-solutions/14-select-and-context/12-multi-stage-pipeline-cancellation/03-goroutine-leak-harness
```

### Why poll-until-settle beats a fixed sleep

`runtime.NumGoroutine()` counts goroutines that exist *right now*. After you cancel
a pipeline and drain its output, the stage goroutines still have to be scheduled to
observe the cancel, run their deferred closes, and return — and that takes a
non-zero, non-deterministic amount of wall time. A test that does `cancel();
time.Sleep(20*time.Millisecond); check()` is betting that 20 ms is always enough.
On an idle laptop it is; on a CI box running 32 packages in parallel it sometimes
is not, and the test flakes.

The robust shape is to poll: read the count repeatedly with a tiny yield between
reads, and succeed as soon as it drops back to within a small threshold of the
baseline, giving up only after an overall timeout. This turns "wait long enough"
into "wait exactly until settled, up to a bound", which is both faster in the
common case and far less flaky. The threshold is small but non-zero because the
test runtime itself may hold a couple of transient goroutines (the race detector,
a background GC assist); requiring an exact match is itself a source of flakiness.

`runtime.Gosched()` between polls yields the processor so the stage goroutines get
a chance to run and exit before the next measurement, rather than the polling
goroutine spinning and starving them.

Create `leak.go`:

```go
package leakharness

import (
	"runtime"
	"time"
)

// Settles polls runtime.NumGoroutine until it returns to within threshold of
// before, yielding between polls. It returns true once settled, or false if the
// count has not settled within timeout. A small non-zero threshold absorbs the
// transient goroutines the test runtime itself may hold.
func Settles(before, threshold int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		runtime.Gosched()
		if runtime.NumGoroutine() <= before+threshold {
			return true
		}
		if time.Now().After(deadline) {
			return runtime.NumGoroutine() <= before+threshold
		}
		time.Sleep(time.Millisecond)
	}
}

// NumGoroutine exposes runtime.NumGoroutine so the demo can read the count
// without importing runtime itself.
func NumGoroutine() int { return runtime.NumGoroutine() }
```

### The stages under test

The harness needs real stages to exercise. `Generate` and `FanOut` here are the
correct, select-guarded versions: every send has a `ctx.Done()` escape, so a cancel
followed by a drain lets every goroutine exit. A *broken* stage — one that does a
bare `out <- v` with no select — would block forever on the unread send after a
cancel, and the harness would report the leak by never settling. That broken
variant is shown illustratively below (not compiled) so you can see exactly what
the canary catches.

Wrong (leaks on cancel — do not build this):

```go
func brokenGenerate(ctx context.Context, n int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := range n {
			out <- i // no select: blocks forever once the consumer stops reading
		}
	}()
	return out
}
```

Create `stage.go`:

```go
package leakharness

import (
	"context"
	"sync"
)

// Generate emits [0, n) with a select-guarded send so it exits on ctx cancel.
func Generate(ctx context.Context, n int) <-chan int {
	out := make(chan int)
	go func() {
		defer close(out)
		for i := range n {
			select {
			case out <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// FanOut runs workers workers over in, closing out once all workers exit.
func FanOut(ctx context.Context, in <-chan int, workers int, work func(int) int) <-chan int {
	out := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for v := range in {
				select {
				case out <- work(v):
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out
}
```

### The runnable demo

The demo runs a fan-out over a large source, cancels after reading a handful,
drains the rest, and reports whether the goroutine count settled back to baseline —
the same measurement the test makes, but as a visible report. It reads the count
through the library's exported `NumGoroutine` accessor so `main` stays to a single
import.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/leakharness"
)

func main() {
	before := leakharness.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	in := leakharness.Generate(ctx, 1000)
	out := leakharness.FanOut(ctx, in, 4, func(v int) int { return v * 2 })

	read := 0
	for range out {
		read++
		if read == 5 {
			cancel()
			break
		}
	}
	for range out { // drain so blocked sends unblock
	}

	settled := leakharness.Settles(before, 2, 100*time.Millisecond)
	after := leakharness.NumGoroutine()
	fmt.Printf("read=%d settled=%v after<=before+2=%v\n", read, settled, after <= before+2)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
read=5 settled=true after<=before+2=true
```

### Tests

`TestNoGoroutineLeakAfterCancel` is the general canary: snapshot the baseline, run
a stage, cancel, drain, and assert `Settles`. `TestFanOutStopsOnCancel` is the
specific 100-element canary — source of 100, cancel after 5 reads, drain the
fan-out, and assert the count returns to baseline within the poll timeout. Both
would fail (never settle) against the broken bare-send stage.

Create `leak_test.go`:

```go
package leakharness

import (
	"context"
	"testing"
	"time"
)

func TestNoGoroutineLeakAfterCancel(t *testing.T) {
	before := NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := Generate(ctx, 1000)
	cancel()
	for range in { // drain to unblock the generator
	}

	if !Settles(before, 2, 100*time.Millisecond) {
		t.Fatalf("goroutines did not settle: before=%d after=%d", before, NumGoroutine())
	}
}

func TestFanOutStopsOnCancel(t *testing.T) {
	before := NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := Generate(ctx, 100)
	out := FanOut(ctx, in, 4, func(v int) int { return v * 2 })

	read := 0
	for range out {
		read++
		if read == 5 {
			cancel()
			break
		}
	}
	for range out { // drain the rest
	}

	if !Settles(before, 2, 50*time.Millisecond) {
		t.Fatalf("fan-out leaked: before=%d after=%d", before, NumGoroutine())
	}
}

func TestSettlesReportsFailureWhenOverThreshold(t *testing.T) {
	t.Parallel()

	// A baseline of zero can never be met by a live process; Settles must give
	// up and report false rather than block forever.
	if Settles(0, 0, 5*time.Millisecond) {
		t.Fatal("Settles(0,0,...) returned true, but a live process has goroutines")
	}
}
```

`TestNoGoroutineLeakAfterCancel` and `TestFanOutStopsOnCancel` do not call
`t.Parallel()`: `NumGoroutine` is a process-global count, and a parallel sibling
spinning up goroutines would corrupt the baseline. Leak tests read a global, so
they run serially.

## Review

The harness is correct when it settles quickly for a well-behaved stage and refuses
to settle (returning false within the timeout) for a leaking one. The design points
that matter: poll with `runtime.Gosched()` rather than sleeping a fixed guess, use
a small non-zero threshold to absorb the runtime's own transient goroutines, and
run the leak tests serially because `NumGoroutine` is process-global. Prove the
canary actually catches leaks by temporarily swapping a stage for the bare-send
`brokenGenerate` above — the test should fail to settle. Run everything under
`-race`; a leak often coincides with a data race on the abandoned channel.

## Resources

- [`runtime.NumGoroutine`](https://pkg.go.dev/runtime#NumGoroutine) — the count the harness samples.
- [`runtime.Gosched`](https://pkg.go.dev/runtime#Gosched) — yielding so stage goroutines can run and exit between polls.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) — the production-grade leak detector this harness is a minimal stand-in for.

---

Back to [02-fan-out-worker-pool-waitgroup-close.md](02-fan-out-worker-pool-waitgroup-close.md) | Next: [04-fan-in-merge-sources.md](04-fan-in-merge-sources.md)
