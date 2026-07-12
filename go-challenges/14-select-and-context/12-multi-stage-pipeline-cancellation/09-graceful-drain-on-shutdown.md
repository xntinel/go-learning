# Exercise 9: Graceful Drain — Stop Intake, Finish In-Flight Within A Deadline

When a worker deployment receives `SIGTERM`, dropping in-flight jobs is often
unacceptable — a half-written record, a half-published batch. Graceful shutdown
splits into two distinct actions: *stop accepting* new work, and *let in-flight
work finish* under a bounded budget. The trap is cancelling the very context the
workers run on, which kills them mid-write. This module builds a `Processor` whose
`Shutdown` signals intake to stop, derives a drain context with `context.WithoutCancel` so
the drain survives the shutdown, bounds it with a `WithTimeoutCause` deadline, and
reports a typed cause when stragglers overrun the budget.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
drain/                       module example.com/drain
  go.mod
  drain.go                   type Processor; Submit, Shutdown(deadline); ErrDrainDeadline
  cmd/
    demo/
      main.go                submits work, shuts down, reports processed vs dropped
  drain_test.go              intake stops, in-flight finishes, straggler hits deadline, no leak
```

Files: `drain.go`, `cmd/demo/main.go`, `drain_test.go`.
Implement: `Processor` with `Submit(item) bool`, `Shutdown(drainBudget) error`
that stops intake via a broadcast `done` channel (safe against a `Submit` racing
`Shutdown`), drains in-flight work under a `WithoutCancel` + `WithTimeoutCause`
context, and returns a typed cause if work overruns the budget.
Test: intake rejects after shutdown starts, concurrent Submit during Shutdown never
panics, already-accepted items complete, a straggler that overruns the budget is
cancelled with a deadline `Cause`, and no goroutine leaks.
Verify: `go test -count=1 -race ./...`

### Separate "stop accepting" from "stop working"

The `Processor` owns an intake channel and a pool of workers. Three pieces of state
make graceful drain work:

- A `closing` flag (guarded by a mutex) that `Submit` checks: once shutdown has
  begun, `Submit` returns `false` and the item is rejected. This is "stop
  accepting" — no new work enters the pipeline.
- A `done` channel, closed exactly once by `Shutdown`, that broadcasts "intake is
  ending" to every goroutine. This is the concurrency contract that makes the
  design safe: `Submit` may be called concurrently with `Shutdown` (the realistic
  case — a `SIGTERM` handler goroutine calls `Shutdown` while in-flight request
  handlers still call `Submit`), so the intake channel must never be closed while a
  writer might still be sending. Closing `intake` from `Shutdown` would violate this
  chapter's cardinal rule — only the sole writer may close a channel — and would
  panic a racing `Submit` with "send on closed channel". Instead `intake` is never
  closed; `done` is closed and every send and every worker receive selects on it.
- A `sync.WaitGroup` counting in-flight items, so `Shutdown` can wait for the work
  already accepted to finish.

`Submit` does `wg.Add(1)` under the mutex only after confirming `closing` is false,
then performs the send inside a `select` that also watches `done`:

```go
select {
case p.intake <- item:
	return true
case <-p.done:
	p.wg.Done() // shutdown won the race; un-count and reject
	return false
}
```

Because every `wg.Add(1)` happens under the mutex while `closing` is still false,
and `Shutdown` sets `closing` under the same mutex before it ever calls `wg.Wait`,
no `Add` can race the `Wait`. A `Submit` that loses the race to a concurrent
`Shutdown` selects the `done` arm, undoes its `wg.Add(1)`, and rejects — it never
touches a closed channel.

`Shutdown(drainBudget)` runs the sequence:

1. Set `closing = true` and `close(done)` so `Submit` rejects (or unwinds a
   racing send) and every worker's `select` sees `done` and returns once it has no
   in-flight item left to finish.
2. Derive the *drain context*. This is the crux. The workers must keep running
   during the drain, but the thing that triggered shutdown (a cancelled parent
   context from `SIGTERM`) is exactly what would stop them. `context.WithoutCancel(parent)`
   returns a context that carries the parent's values but is *not* cancelled when
   the parent is — so the drain is insulated from the shutdown that started it.
   Then `context.WithTimeoutCause(drainCtx, drainBudget, ErrDrainDeadline)` bounds
   how long the drain may take and carries a typed reason if it overruns.
3. Wait for the `WaitGroup` in a goroutine, signalling completion on a channel.
   `select` over that channel and `drainCtx.Done()`: whichever wins decides the
   outcome — clean drain, or deadline hit with stragglers still running.
4. Read the outcome from `context.Cause(drainCtx)`: `nil` on a clean drain, or
   `ErrDrainDeadline` when the budget elapsed with work still running. In a real
   deployment you also thread `drainCtx` down to the work function so a straggler
   sees `drainCtx.Done()` and aborts mid-operation; `context.AfterFunc(drainCtx,
   ...)` is the hook for firing a hard-cancel or a "still draining" alert at the
   deadline.

Using `WithoutCancel` instead of a fresh `context.Background()` matters because it
preserves request-scoped values (trace IDs, tenant tags) so the drained work is
still observable under the same trace, while shedding only the cancellation. A bare
`Background()` would lose that context.

Create `drain.go`:

```go
package drain

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrDrainDeadline is the cause when the drain budget elapses before in-flight
// work finished.
var ErrDrainDeadline = errors.New("drain: budget exceeded, stragglers cancelled")

// Processor accepts items and processes them with a pool of workers. On Shutdown
// it stops accepting new items but drains in-flight work under a bounded budget.
type Processor struct {
	intake  chan int
	done    chan struct{} // closed once by Shutdown to signal end of intake
	work    func(ctx context.Context, item int)
	wg      sync.WaitGroup // counts in-flight items
	workers sync.WaitGroup // counts worker goroutines

	mu      sync.Mutex
	closing bool
}

// New starts a Processor with n workers applying work to each item.
func New(ctx context.Context, n int, work func(ctx context.Context, item int)) *Processor {
	p := &Processor{
		intake: make(chan int),
		done:   make(chan struct{}),
		work:   work,
	}
	p.workers.Add(n)
	for range n {
		go func() {
			defer p.workers.Done()
			for {
				select {
				case item := <-p.intake:
					p.work(ctx, item)
					p.wg.Done()
				case <-p.done:
					return
				}
			}
		}()
	}
	return p
}

// Submit offers an item. It returns false if the Processor is shutting down.
// Submit is safe to call concurrently with Shutdown: it never sends on a closed
// channel, because intake is never closed and the send selects on done.
func (p *Processor) Submit(item int) bool {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return false
	}
	p.wg.Add(1)
	p.mu.Unlock()

	select {
	case p.intake <- item:
		return true
	case <-p.done:
		p.wg.Done() // shutdown won the race; un-count and reject
		return false
	}
}

// Shutdown stops intake and drains in-flight work, waiting up to drainBudget.
// It returns nil on a clean drain or ErrDrainDeadline if the budget elapsed with
// work still running.
func (p *Processor) Shutdown(parent context.Context, drainBudget time.Duration) error {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil
	}
	p.closing = true
	close(p.done)
	p.mu.Unlock()

	// Drain context survives the parent's cancellation but is bounded by the budget.
	drainCtx, cancel := context.WithTimeoutCause(
		context.WithoutCancel(parent), drainBudget, ErrDrainDeadline,
	)
	defer cancel()

	done := make(chan struct{})
	go func() {
		p.wg.Wait() // all accepted items processed
		close(done)
	}()

	select {
	case <-done:
		p.workers.Wait() // clean drain: every worker has finished, let them exit
		return nil
	case <-drainCtx.Done():
		// Budget elapsed with stragglers still running. Do NOT wait on the
		// workers here or Shutdown would block on the very goroutines that
		// overran; return the cause and let the stragglers finish on their own.
		return context.Cause(drainCtx)
	}
}
```

The worker passes the constructor `ctx` to `work`; `Shutdown` bounds how long it
waits for in-flight items but does not, in this minimal version, reach into a
running `work` call — threading `drainCtx` all the way down to `work` is the next
refinement a real deployment makes. The contract this `Processor` proves is the
one that matters for graceful shutdown: intake stops the instant `Shutdown`
begins, every already-accepted item is waited on, and the drain budget bounds that
wait with a typed cause (`ErrDrainDeadline`) when work overruns.

### The runnable demo

The demo submits ten quick items across three workers, shuts down with a generous
budget, and reports how many were processed versus rejected. Because shutdown
happens after all submits, every accepted item drains cleanly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/drain"
)

func main() {
	var processed atomic.Int64
	p := drain.New(context.Background(), 3, func(ctx context.Context, item int) {
		time.Sleep(2 * time.Millisecond)
		processed.Add(1)
	})

	accepted := 0
	for i := 0; i < 10; i++ {
		if p.Submit(i) {
			accepted++
		}
	}

	err := p.Shutdown(context.Background(), 500*time.Millisecond)

	// Any submit after shutdown is rejected.
	rejected := !p.Submit(999)

	fmt.Printf("accepted=%d processed=%d rejected_after_shutdown=%v err=%v\n",
		accepted, processed.Load(), rejected, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
accepted=10 processed=10 rejected_after_shutdown=true err=<nil>
```

### Tests

`TestIntakeStopsOnShutdown` asserts `Submit` returns false once shutdown begins.
`TestConcurrentSubmitDuringShutdown` hammers `Submit` from a goroutine while
`Shutdown` runs, across many iterations under `-race`; it is the regression test for
the send-on-closed-channel panic — with the naive `close(intake)` design it panics,
with the `done`-channel protocol it never does. `TestInFlightCompletes` asserts
every accepted item is processed by the time `Shutdown` returns nil.
`TestStragglerHitsDeadline` uses a work function that blocks until released, shuts
down with a tiny budget, and asserts `Shutdown` returns `ErrDrainDeadline` via
`context.Cause`. `TestNoLeakAfterShutdown` checks the goroutine baseline.

Create `drain_test.go`:

```go
package drain

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIntakeStopsOnShutdown(t *testing.T) {
	t.Parallel()

	p := New(context.Background(), 2, func(ctx context.Context, item int) {})
	if !p.Submit(1) {
		t.Fatal("Submit before shutdown returned false")
	}
	if err := p.Shutdown(context.Background(), time.Second); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
	if p.Submit(2) {
		t.Fatal("Submit after shutdown returned true; intake did not stop")
	}
}

func TestConcurrentSubmitDuringShutdown(t *testing.T) {
	t.Parallel()

	// Submit racing Shutdown is the realistic graceful-shutdown case: a SIGTERM
	// handler calls Shutdown while request handlers still call Submit. A design
	// that closed intake would panic here with "send on closed channel"; the
	// done-channel protocol must make every racing Submit either succeed or
	// cleanly reject. Many iterations widen the interleaving window under -race.
	for range 50 {
		p := New(context.Background(), 3, func(ctx context.Context, item int) {})

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				p.Submit(i) // must never panic, whoever wins the race
			}
		}()

		if err := p.Shutdown(context.Background(), time.Second); err != nil {
			t.Fatalf("Shutdown err = %v, want nil", err)
		}
		wg.Wait()
	}
}

func TestInFlightCompletes(t *testing.T) {
	t.Parallel()

	var processed atomic.Int64
	p := New(context.Background(), 3, func(ctx context.Context, item int) {
		time.Sleep(time.Millisecond)
		processed.Add(1)
	})

	const n = 30
	for i := 0; i < n; i++ {
		p.Submit(i)
	}
	if err := p.Shutdown(context.Background(), time.Second); err != nil {
		t.Fatalf("Shutdown err = %v, want nil", err)
	}
	if processed.Load() != n {
		t.Fatalf("processed = %d, want %d (in-flight work dropped)", processed.Load(), n)
	}
}

func TestStragglerHitsDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	started := make(chan struct{}, 1)
	p := New(context.Background(), 1, func(ctx context.Context, item int) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // block past the drain budget
	})

	p.Submit(1)
	<-started // ensure the item is in-flight before shutting down

	err := p.Shutdown(context.Background(), 20*time.Millisecond)
	if !errors.Is(err, ErrDrainDeadline) {
		close(release)
		t.Fatalf("Shutdown err = %v, want ErrDrainDeadline", err)
	}
	close(release) // let the straggler finish so its goroutine exits
}

func TestNoLeakAfterShutdown(t *testing.T) {
	before := runtime.NumGoroutine()

	p := New(context.Background(), 4, func(ctx context.Context, item int) {})
	for i := 0; i < 20; i++ {
		p.Submit(i)
	}
	if err := p.Shutdown(context.Background(), time.Second); err != nil {
		t.Fatalf("Shutdown err = %v", err)
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if runtime.NumGoroutine() > before+2 {
		t.Fatalf("leak: before=%d after=%d", before, runtime.NumGoroutine())
	}
}
```

`TestStragglerHitsDeadline` releases the blocked worker after asserting the
deadline so the worker goroutine exits and the test does not leak it.
`TestNoLeakAfterShutdown` runs serially because it reads the global goroutine count.

## Review

The `Processor` is correct when `Submit` rejects after shutdown begins, a `Submit`
racing a concurrent `Shutdown` never panics (intake is never closed; `done` is the
only channel `Shutdown` closes, and both the send and the worker receive select on
it), every already-accepted item finishes before a clean `Shutdown` returns nil, a
straggler that overruns the budget makes `Shutdown` return `ErrDrainDeadline` via
`context.Cause`, and no goroutine leaks. The design decision that carries the
lesson is deriving the drain context with `context.WithoutCancel(parent)` rather
than reusing `parent` (which a `SIGTERM` cancel would have already stopped) or a
bare `Background()` (which would drop request-scoped values). The budget comes from
`WithTimeoutCause` so the reason for a cutoff is data, not a generic
`DeadlineExceeded`. Run `go test -race`.

## Resources

- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) — a child that is not cancelled when its parent is, for a drain that survives shutdown.
- [`context.WithTimeoutCause`](https://pkg.go.dev/context#WithTimeoutCause) — a bounded drain whose expiry carries a typed cause.
- [`context.AfterFunc`](https://pkg.go.dev/context#AfterFunc) — running a hard-cancel or cleanup when a context is done.

---

Back to [08-retry-with-backoff-stage.md](08-retry-with-backoff-stage.md) | Next: [10-cancellation-cause-observability.md](10-cancellation-cause-observability.md)
