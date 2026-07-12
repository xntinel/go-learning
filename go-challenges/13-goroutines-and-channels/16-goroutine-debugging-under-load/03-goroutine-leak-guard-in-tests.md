# Exercise 3: Build a test helper that fails when a unit leaks goroutines

A goroutine leak does not crash — it accumulates, silently, until the service runs
out of memory at 03:00. The place to catch it is the test suite. This exercise
builds a hand-rolled leak guard (the pattern `go.uber.org/goleak` implements): it
snapshots the goroutine baseline before a unit runs and, at cleanup, polls until
the count returns to baseline, failing the test if a goroutine outlived it.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
leakguard/                  independent module: example.com/leakguard
  go.mod
  leakguard.go              Check(t TB) snapshots baseline, fails at Cleanup if elevated; Live()
  cmd/demo/main.go          clean unit passes, leaked unit is flagged
  leakguard_test.go         positive (well-behaved) + negative (leaked) with a fake TB recorder
```

- Files: `leakguard.go`, `cmd/demo/main.go`, `leakguard_test.go`.
- Implement: `Check(t TB)` that reads the goroutine profile count as a baseline and registers a `Cleanup` that polls (after `runtime.GC`, with backoff) until the count returns to baseline, else calls `t.Errorf`.
- Test: a well-behaved worker passes the guard; a still-running goroutine is detected via a fake `TB` recorder that captures the failure.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/03-goroutine-leak-guard-in-tests/cmd/demo
cd go-solutions/13-goroutines-and-channels/16-goroutine-debugging-under-load/03-goroutine-leak-guard-in-tests
```

### Why a baseline diff and a poll, not a raw count

The naive leak check — assert `runtime.NumGoroutine()` equals some number — is
useless, because the test binary always has its own goroutines (the test runner, the
GC, `os/signal` handlers), and that number is neither stable nor known in advance.
The guard instead measures *change*: it snapshots the count when `Check` is called
and, at cleanup, compares against that baseline. Framework goroutines are present in
both snapshots and cancel out. If the count after the unit is greater than the
baseline, the difference is goroutines the unit started and did not stop.

The second subtlety is timing. When you cancel a context, the goroutines watching it
do not exit instantly — they have to be scheduled to observe `ctx.Done()` and run
their `defer`s. If the guard checked exactly once, immediately, it would flake on the
scheduler. So it polls: `runtime.GC()` (which nudges finalizers and gives the
scheduler a chance), check the count, and if still elevated, sleep a little and
retry, up to a bound. A well-behaved unit converges within a few iterations; a real
leak never does, and the guard fails.

To make the guard usable both from a real `*testing.T` and from a test that wants to
*verify the guard itself*, `Check` takes a small `TB` interface — `Helper`,
`Errorf`, and `Cleanup` — which `*testing.T` satisfies directly. The count comes
from `pprof.Lookup("goroutine").Count()`, which is the same source the goroutine
profile reports; `runtime.NumGoroutine()` would work equally well and is exposed as
`Live` for convenience.

Create `leakguard.go`:

```go
package leakguard

import (
	"runtime"
	"runtime/pprof"
	"time"
)

// TB is the subset of *testing.T the guard needs. *testing.T satisfies it, and a
// test can pass a fake to verify the guard's own failure path.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Cleanup(func())
}

// Check snapshots the current goroutine count and registers a cleanup that fails
// t if the count has not returned to that baseline. The cleanup polls after
// runtime.GC with a short backoff so a goroutine that is exiting is given time to
// finish before the guard reports a leak.
func Check(t TB) {
	t.Helper()
	baseline := goroutines()
	t.Cleanup(func() {
		for i := range 20 {
			runtime.GC()
			if now := goroutines(); now <= baseline {
				return // returned to baseline: no leak
			}
			time.Sleep(time.Duration(i+1) * time.Millisecond)
		}
		t.Errorf("goroutine leak: baseline=%d now=%d", baseline, goroutines())
	})
}

// Live reports the number of goroutines that currently exist.
func Live() int {
	return runtime.NumGoroutine()
}

func goroutines() int {
	return pprof.Lookup("goroutine").Count()
}
```

### The runnable demo

The demo defines a tiny `probe` that implements `leakguard.TB` so it can run the
cleanup by hand and read whether the guard fired. It runs the guard around a clean
unit (a worker that is started and joined) and around a leaky unit (a goroutine
parked on a channel that is never closed until after the check), and prints the
verdict for each.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/leakguard"
)

// probe is a fake TB that records cleanups and whether Errorf was called.
type probe struct {
	cleanups []func()
	failed   bool
}

func (p *probe) Helper()               {}
func (p *probe) Errorf(string, ...any) { p.failed = true }
func (p *probe) Cleanup(f func())      { p.cleanups = append(p.cleanups, f) }
func (p *probe) runCleanups() {
	for i := len(p.cleanups) - 1; i >= 0; i-- {
		p.cleanups[i]()
	}
}

func main() {
	// Clean unit: start a worker and join it before cleanup.
	clean := &probe{}
	leakguard.Check(clean)
	done := make(chan struct{})
	go func() { close(done) }()
	<-done
	clean.runCleanups()
	fmt.Println("clean unit leaked:", clean.failed)

	// Leaky unit: a goroutine that outlives the check.
	leaky := &probe{}
	stop := make(chan struct{})
	leakguard.Check(leaky)
	go func() { <-stop }()
	leaky.runCleanups()
	fmt.Println("leaky unit leaked:", leaky.failed)
	close(stop) // release so the process exits cleanly
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
clean unit leaked: false
leaky unit leaked: true
```

### Tests

`TestNoLeakPasses` runs the guard on a real `*testing.T` around a worker that stops
on context cancel and is joined; its cleanup runs after the test and must not fail.
`TestDetectsLeak` uses a fake `TB` recorder so it can spawn a goroutine that stays
parked, run the guard's cleanup, and assert the guard reported the leak — then
release the goroutine so the test process itself stays clean.

Create `leakguard_test.go`:

```go
package leakguard

import (
	"context"
	"fmt"
	"testing"
)

// recorder is a fake TB used to observe the guard's failure path.
type recorder struct {
	cleanups []func()
	failed   bool
}

func (r *recorder) Helper()               {}
func (r *recorder) Errorf(string, ...any) { r.failed = true }
func (r *recorder) Cleanup(f func())      { r.cleanups = append(r.cleanups, f) }
func (r *recorder) runCleanups() {
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
}

func TestNoLeakPasses(t *testing.T) {
	Check(t) // registers a real Cleanup on t; it must not fail

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
	}()
	cancel()
	<-done // join before the test (and thus the guard's cleanup) runs
}

func TestDetectsLeak(t *testing.T) {
	rec := &recorder{}
	stop := make(chan struct{})

	Check(rec)             // baseline snapshot
	go func() { <-stop }() // leaked: stays parked past the check
	rec.runCleanups()      // guard polls, count stays elevated -> Errorf

	if !rec.failed {
		t.Fatal("guard did not detect the leaked goroutine")
	}
	close(stop) // release so we do not actually leak in the test binary
}

func ExampleCheck() {
	rec := &recorder{}
	Check(rec)
	// no goroutine leaked between Check and cleanup
	rec.runCleanups()
	fmt.Println("leaked:", rec.failed)
	// Output: leaked: false
}
```

## Review

The guard is correct when it is silent for a unit that cleans up and loud for one
that does not, and the two tests pin exactly those cases. The design choices are
what make it trustworthy: the baseline diff cancels out the test runner's own
goroutines, so the guard does not depend on any absolute count; and the backoff poll
after `runtime.GC` gives a just-cancelled goroutine time to exit, so a correct unit
does not flake. The negative test deliberately spawns its leaked goroutine *after*
`Check` takes the baseline and releases it *after* asserting the failure, so the
guard sees the elevation but the test binary is left clean. If you skip the backoff
and check once, `TestNoLeakPasses` will flake whenever the cancelled worker has not
yet been scheduled. Run `go test -race` to confirm the guard's shared state is
handled correctly.

## Resources

- [`runtime/pprof.Profile.Count`](https://pkg.go.dev/runtime/pprof#Profile.Count) — the goroutine count the guard baselines against.
- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — cleanup registration, run in last-added-first order after the test.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — the production library this exercise reimplements, including its stack-filtering approach.

---

Prev: [02-expose-pprof-behind-admin-auth.md](02-expose-pprof-behind-admin-auth.md) | Back to [00-concepts.md](00-concepts.md) | Next: [04-pprof-labels-attribute-goroutines.md](04-pprof-labels-attribute-goroutines.md)
