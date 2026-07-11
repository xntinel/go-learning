# Exercise 3: Graceful shutdown that preempts buffered work with a drain deadline

A queue consumer under Kubernetes receives SIGTERM and has a bounded budget to
finish before SIGKILL. This exercise builds that shutdown path: a worker loop that
processes work while open, and on cancellation stops taking new work, drains
what is in flight until a deadline, then aborts and reports what it dropped. The
timing is tested deterministically under a `testing/synctest` bubble.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
shutdown/                    module example.com/shutdown
  go.mod
  drainer.go                 type Job; type Result; ErrDrainDeadline; Run; NotifyShutdown
  cmd/
    demo/
      main.go                queues jobs, self-signals SIGTERM, prints processed/dropped
  drainer_test.go            synctest: processes-while-open, drains-within, deadline-abort
```

Files: `drainer.go`, `cmd/demo/main.go`, `drainer_test.go`.
Implement: `Run(ctx, work, drain, process) Result` — process work while `ctx` is
open; on `ctx.Done()` snapshot the buffered work, drain it under a
`context.WithTimeoutCause` deadline, and abort the rest. `NotifyShutdown()` wraps
`signal.NotifyContext` for the production entry point.
Test: work is processed while open; buffered items drain within the deadline
(`Cause` nil); past the deadline the loop returns and reports the dropped count
with `Cause == ErrDrainDeadline`.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/shutdown/cmd/demo
cd ~/go-exercises/shutdown
go mod init example.com/shutdown
```

### Shutdown is priority on the time axis

The shutdown signal must preempt buffered work — you stop pulling *new* items the
moment `ctx` is done — but you do not want to drop in-flight items you could
finish cheaply. The production shape is a two-phase loop:

- **Normal phase:** a strict `ctx.Done()` check, then a blocking `select` over
  `ctx.Done()` and the work channel. While open, process items. A closed work
  channel (`ok == false`) means the producer is finished and we return cleanly.
- **Drain phase:** entered on `ctx.Done()`. Snapshot the number of items
  currently buffered (`len(work)`), then drain exactly that many, but bounded by a
  `context.WithTimeoutCause` deadline. The deadline is checked *strictly first*
  each iteration so it preempts remaining work rather than racing it. When the
  deadline fires, the items not yet processed are counted as dropped and the
  reason — `ErrDrainDeadline` — is recoverable via `context.Cause`.

Snapshotting `len(work)` is what makes the drain terminate: without it, a drain
over an open (never-closed) channel would block until the deadline every time,
even when it had already emptied the buffer. By draining exactly the count that
was buffered at shutdown, a clean drain finishes early with `Cause == nil`, and
only an over-budget drain reports the deadline cause.

Why `WithTimeoutCause` rather than a bare `WithTimeout`? Because the caller needs
to distinguish "drained cleanly" (`Cause` nil, dropped 0) from "hit the deadline
and abandoned 4 items" (`Cause == ErrDrainDeadline`, dropped 4). `context.Cause`
returns the specific cause you attached, where `ctx.Err()` would only say
`DeadlineExceeded`.

Create `drainer.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"os/signal"
	"syscall"
	"time"
)

// ErrDrainDeadline is the cause attached to the drain context; it surfaces via
// context.Cause when the drain budget is exhausted before the buffer empties.
var ErrDrainDeadline = errors.New("shutdown: drain deadline exceeded")

// Job is a unit of queued work.
type Job struct {
	ID int
}

// Result summarizes a Run: how many jobs were processed, how many were dropped
// when the drain deadline fired, and the cause of the drain ending (nil for a
// clean drain, ErrDrainDeadline for a timed-out one).
type Result struct {
	Processed int
	Dropped   int
	Cause     error
}

// NotifyShutdown returns a context cancelled on SIGINT or SIGTERM, plus a stop
// function to release the signal handler. This is the production entry point;
// tests inject a plain cancellable context instead.
func NotifyShutdown() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// Run processes work until ctx is cancelled or the work channel is closed. On
// cancellation it drains the buffered work under a drain-duration deadline, then
// aborts, reporting how many jobs it dropped.
func Run(ctx context.Context, work <-chan Job, drain time.Duration, process func(Job)) Result {
	processed := 0
	for {
		// Shutdown strictly preempts taking new work.
		select {
		case <-ctx.Done():
			return finishDrain(processed, work, drain, process)
		default:
		}
		select {
		case <-ctx.Done():
			return finishDrain(processed, work, drain, process)
		case j, ok := <-work:
			if !ok {
				return Result{Processed: processed}
			}
			process(j)
			processed++
		}
	}
}

// finishDrain drains the items buffered at shutdown, bounded by drain, and
// returns the full Result.
func finishDrain(processed int, work <-chan Job, drain time.Duration, process func(Job)) Result {
	dctx, cancel := context.WithTimeoutCause(context.Background(), drain, ErrDrainDeadline)
	defer cancel()

	remaining := len(work) // snapshot: only drain what is already in flight
	for i := range remaining {
		// The drain deadline strictly preempts the remaining buffered work.
		select {
		case <-dctx.Done():
			return Result{Processed: processed, Dropped: remaining - i, Cause: context.Cause(dctx)}
		default:
		}
		select {
		case <-dctx.Done():
			return Result{Processed: processed, Dropped: remaining - i, Cause: context.Cause(dctx)}
		case j := <-work:
			process(j)
			processed++
		}
	}
	return Result{Processed: processed}
}
```

### The runnable demo

The demo wires the production path: `NotifyShutdown` for the context, a queue of
jobs, and — to make the demo terminate on its own — a goroutine that sends the
process its own SIGTERM once the work is queued. Because the jobs are handled
instantly and the drain budget is a full second, all three drain cleanly.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"example.com/shutdown"
)

func main() {
	ctx, stop := shutdown.NotifyShutdown()
	defer stop()

	work := make(chan shutdown.Job, 3)
	for i := range 3 {
		work <- shutdown.Job{ID: i}
	}

	// Simulate the orchestrator sending SIGTERM after work is queued.
	go func() {
		p, _ := os.FindProcess(os.Getpid())
		_ = p.Signal(syscall.SIGTERM)
	}()

	res := shutdown.Run(ctx, work, time.Second, func(j shutdown.Job) {
		_ = j // pretend to handle the job
	})
	fmt.Printf("processed=%d dropped=%d\n", res.Processed, res.Dropped)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
processed=3 dropped=0
```

### Tests

The drain deadline is real time, which would make an assertion on "dropped 1 of 5"
flaky under the wall clock. `testing/synctest` runs the code in a bubble where
`time.Sleep` and the `context` deadline timer advance on a fake clock only when
every goroutine is durably blocked, so a 100 ms budget against a 30 ms-per-job
handler produces an exact, instant, deterministic result.
`TestProcessesWhileOpen` needs no clock and runs outside a bubble;
`TestDrainWithinDeadline` and `TestDrainDeadlineExceeded` run inside one.

Create `drainer_test.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestProcessesWhileOpen(t *testing.T) {
	t.Parallel()

	work := make(chan Job, 3)
	for i := range 3 {
		work <- Job{ID: i}
	}
	close(work) // producer finished: Run drains the buffer then returns cleanly

	processed := 0
	res := Run(context.Background(), work, time.Second, func(Job) { processed++ })
	if res.Processed != 3 || res.Dropped != 0 || res.Cause != nil {
		t.Fatalf("Result = %+v, want Processed=3 Dropped=0 Cause=nil", res)
	}
}

func TestDrainWithinDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		work := make(chan Job, 3)
		for i := range 3 {
			work <- Job{ID: i}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // already shutting down: go straight to drain

		res := Run(ctx, work, time.Second, func(Job) {
			time.Sleep(30 * time.Millisecond) // 3*30ms = 90ms < 1s budget
		})
		if res.Processed != 3 || res.Dropped != 0 || res.Cause != nil {
			t.Fatalf("Result = %+v, want Processed=3 Dropped=0 Cause=nil", res)
		}
	})
}

func TestDrainDeadlineExceeded(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		work := make(chan Job, 5)
		for i := range 5 {
			work <- Job{ID: i}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		// 30ms per job against a 100ms budget: jobs finish at 30,60,90,120...
		// The job started at t=90 runs to 120; at t=120 the deadline (100) has
		// fired, so the 5th job is dropped.
		res := Run(ctx, work, 100*time.Millisecond, func(Job) {
			time.Sleep(30 * time.Millisecond)
		})
		if res.Processed != 4 || res.Dropped != 1 {
			t.Fatalf("Result = %+v, want Processed=4 Dropped=1", res)
		}
		if !errors.Is(res.Cause, ErrDrainDeadline) {
			t.Fatalf("Cause = %v, want ErrDrainDeadline", res.Cause)
		}
	})
}
```

## Review

The shutdown path is correct when a clean drain and a timed-out drain are
distinguishable: `TestDrainWithinDeadline` finishes all work with `Cause == nil`
and `Dropped == 0`, while `TestDrainDeadlineExceeded` abandons the last job with
`Dropped == 1` and `Cause == ErrDrainDeadline`. The two mistakes to avoid are an
*unbounded* drain — looping over the work channel until empty, which a slow
producer keeps alive past the shutdown budget — and racing the deadline instead
of preempting it: check `dctx.Done()` in its own strict `select` before the
blocking receive, or a full buffer and an expired deadline both being ready lets
`select` pick the work case at random and overrun the budget. The `len(work)`
snapshot is load-bearing: it bounds the drain to what was in flight at shutdown so
a clean drain returns early rather than always waiting the full budget. In
production, `NotifyShutdown` supplies the context; the tests inject a plain
cancelled context so the drain phase is exercised deterministically under
`synctest`.

## Resources

- [`os/signal.NotifyContext`](https://pkg.go.dev/os/signal#NotifyContext) — a context cancelled by SIGINT/SIGTERM for graceful shutdown.
- [`context.WithTimeoutCause`](https://pkg.go.dev/context#WithTimeoutCause) and [`context.Cause`](https://pkg.go.dev/context#Cause) — a recoverable deadline reason.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the bubble and fake clock that make the drain deadline deterministic.

---

Back to [02-fair-priority-drain.md](02-fair-priority-drain.md) | Next: [04-error-channel-precedence.md](04-error-channel-precedence.md)
