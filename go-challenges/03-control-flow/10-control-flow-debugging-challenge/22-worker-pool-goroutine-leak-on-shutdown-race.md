# Exercise 22: Worker Pool Goroutines Started After Shutdown Signal Leak Due to Synchronization Race

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A dispatcher that starts one goroutine per submitted job and uses a
labeled `break` to leave its dispatch loop the instant its context is
cancelled looks complete once it also calls `wg.Wait()` after the loop —
until `wg.Add(1)` is registered *inside* the spawned goroutine instead of
on the dispatcher goroutine before `go func(){...}()` runs. The dispatcher
can then reach `wg.Wait()` while some just-launched goroutines have not
yet executed their own `Add(1)`, so `Wait()` observes a counter that has
not caught up and returns immediately — the goroutine that "hadn't
registered yet" keeps running past the point graceful shutdown believed
everything had finished. This module is fully self-contained: its own
`go mod init`, all code inline, its own demo and tests.

## What you'll build

```text
workerpool/                 independent module: example.com/worker-pool-goroutine-leak-on-shutdown-race
  go.mod                     go 1.21
  workerpool.go               Pool, New, Submit, Dispatch
  cmd/
    demo/
      main.go                 runnable demo: 2000 jobs submitted right up to cancellation
  workerpool_test.go           a 2000-job race asserting none finish after Dispatch returns, plus a no-jobs cancel case
```

- Files: `workerpool.go`, `cmd/demo/main.go`, `workerpool_test.go`.
- Implement: `Pool` (`Submit`, `Dispatch`) that starts one goroutine per job until its context is cancelled, then waits for every started goroutine before returning.
- Test: submit a large burst of jobs right up to cancellation and assert every one finished by the time `Dispatch` returned; a second test asserts an already-cancelled context returns promptly with no jobs in flight.
- Verify: `go test -count=1 -race ./...`.

### Why Add has to happen before go, not inside it

The buggy shape puts the bookkeeping where the work happens, which reads
as tidy — the goroutine registers itself, then does its job, then
unregisters:

```go
case job := <-p.jobs:
	go func() {
		p.wg.Add(1) // BUG: racing with a concurrent wg.Wait() in the dispatcher
		defer p.wg.Done()
		job()
	}()
```

`go func(){...}()` only schedules the goroutine; it does not wait for it
to start running, let alone reach its first line. Control returns to the
dispatcher's `for`/`select` immediately, and if `ctx.Done()` fires on the
very next iteration, the labeled `break` leaves the loop and
`p.wg.Wait()` runs right away — possibly before the new goroutine has
executed `p.wg.Add(1)` at all. If no other job is currently in flight, the
counter `Wait()` observes is `0`, so it returns immediately, and *then*
the delayed goroutine calls `Add(1)` on a `WaitGroup` a `Wait()` has
already returned from — the exact misuse the standard library's own
`sync.WaitGroup` documentation calls out: "Note that calls with a
positive delta that occur when the counter is zero must happen before a
`Wait` is called." The dispatcher's caller believes shutdown is complete
and moves on to closing whatever resources the job might still touch,
while that one goroutine keeps running underneath it. The fix moves
`Add(1)` onto the dispatcher goroutine itself, executed synchronously
*before* `go func(){...}()`:

```go
case job := <-p.jobs:
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		job()
	}()
```

Now every `Add(1)` is sequenced on the same goroutine that later calls
`Wait()`, inside the same `for` loop — so by construction, every job that
was ever dispatched is already counted before the labeled `break` can even
be reached.

Create `workerpool.go`:

```go
package workerpool

import (
	"context"
	"sync"
)

// Pool dispatches submitted jobs onto their own goroutine each, until its
// context is cancelled, at which point it stops accepting new jobs and
// waits for every goroutine it already started to finish.
type Pool struct {
	jobs chan func()
	wg   sync.WaitGroup
}

// New creates an empty Pool.
func New() *Pool {
	return &Pool{jobs: make(chan func())}
}

// Submit hands job to the dispatcher. It blocks until Dispatch is ready to
// receive it or ctx is cancelled.
func (p *Pool) Submit(job func()) {
	p.jobs <- job
}

// Dispatch reads jobs and starts one goroutine per job until ctx is
// cancelled, then waits for every started goroutine to finish before
// returning. wg.Add is called synchronously on the dispatcher goroutine,
// before go func(){...}() -- not inside the new goroutine -- so that by
// the time a labeled break can reach wg.Wait(), every job that was ever
// started is already accounted for in the counter.
func (p *Pool) Dispatch(ctx context.Context) {
Loop:
	for {
		select {
		case job := <-p.jobs:
			p.wg.Add(1)
			go func() {
				defer p.wg.Done()
				job()
			}()
		case <-ctx.Done():
			break Loop
		}
	}
	p.wg.Wait()
}
```

### The runnable demo

The demo submits 2000 jobs right up to cancellation and prints how many
had finished by the time `Dispatch` returned — it must be all 2000.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/worker-pool-goroutine-leak-on-shutdown-race"
)

func main() {
	p := workerpool.New()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.Dispatch(ctx)
		close(done)
	}()

	const jobCount = 2000
	var finished int64
	for i := 0; i < jobCount; i++ {
		p.Submit(func() {
			atomic.AddInt64(&finished, 1)
		})
	}

	cancel()
	<-done // Dispatch has returned: every started goroutine has already finished

	fmt.Println("submitted:", jobCount, "finished by the time Dispatch returned:", finished)
}
```

```bash
go run ./cmd/demo
```

Expected output:

```text
submitted: 2000 finished by the time Dispatch returned: 2000
```

### Tests

`TestDispatchWaitsForEveryStartedJob` is the concurrency/edge case: a
2000-job burst submitted right up to the moment of cancellation, asserting
the finished count exactly matches the submitted count once `Dispatch`
returns. This is exactly the shape of test that is flaky in a
characteristic way on the buggy version — under `-race` and real
scheduling variance it fails often, though not on literally every run,
which is itself the signature of a synchronization race rather than a
deterministic off-by-one. `TestDispatchStopsAcceptingAfterCancel` pins the
simpler non-racy half of the contract: an already-cancelled context makes
`Dispatch` return promptly.

Create `workerpool_test.go`:

```go
package workerpool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestDispatchWaitsForEveryStartedJob is the concurrency/edge case: it
// submits a large burst of jobs right up to the moment of cancellation and
// asserts that every single one had finished by the time Dispatch returned.
// On the buggy version -- wg.Add(1) called from inside the new goroutine
// instead of before it starts -- Wait() can observe a zero counter and
// return while the last few goroutines have not yet registered themselves,
// so this assertion is flaky exactly the way the real leak is flaky.
func TestDispatchWaitsForEveryStartedJob(t *testing.T) {
	p := New()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		p.Dispatch(ctx)
		close(done)
	}()

	const jobCount = 2000
	var finished int64
	for i := 0; i < jobCount; i++ {
		p.Submit(func() {
			atomic.AddInt64(&finished, 1)
		})
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Dispatch did not return after cancellation")
	}

	if got := atomic.LoadInt64(&finished); got != jobCount {
		t.Fatalf("finished = %d by the time Dispatch returned, want %d (a goroutine leaked past shutdown)", got, jobCount)
	}
}

// TestDispatchStopsAcceptingAfterCancel checks the simpler, non-racy half
// of the contract: once ctx is cancelled, Dispatch returns promptly instead
// of hanging, even with no jobs in flight.
func TestDispatchStopsAcceptingAfterCancel(t *testing.T) {
	p := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		p.Dispatch(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Dispatch did not return promptly for an already-cancelled context")
	}
}
```

Run: `go test -count=1 -race ./...`.

## Review

`Dispatch` is correct when `wg.Wait()` never returns before every
goroutine it started has actually finished — proven under load with
thousands of jobs racing cancellation, not by a slow sequential test that
never gives the scheduler a chance to interleave `Add` and `Wait` badly.
The mistake this design avoids is registering a `WaitGroup` count from
inside the goroutine the count is meant to track: `go func(){...}()`
returns before the new goroutine runs a single instruction, so any
`Add(1)` placed inside it is racing against whatever the launching
goroutine does next. The fix is a rule with no exceptions: `Add` always
happens on the goroutine that could plausibly call `Wait()` next, and it
always happens *before* the corresponding `go` statement, never after.

## Resources

- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — "Note that calls with a positive delta that start when the counter is zero must happen before a Wait."
- [Go Race Detector](https://go.dev/doc/articles/race_detector) — running a synchronization-race test under `-race` and across multiple runs, since some races do not trigger every time.
- [Go Specification: Labeled statements](https://go.dev/ref/spec#Labeled_statements) — `break Label` terminates the labeled `for`, `switch`, or `select`, used here to leave the dispatch loop from inside a `select` case.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [21-circuit-breaker-half-open-guard-missing.md](21-circuit-breaker-half-open-guard-missing.md) | Next: [23-event-dispatch-panic-recovery-masks-failure.md](23-event-dispatch-panic-recovery-masks-failure.md)
