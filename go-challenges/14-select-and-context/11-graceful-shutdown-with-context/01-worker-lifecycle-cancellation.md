# Exercise 1: A Background Worker That Stops On Context Cancellation

Every long-lived background job in a backend service — a metrics flusher, a cache
refresher, a queue consumer — is built from the same atomic unit: a goroutine that
runs until its context is cancelled and then signals that it has stopped. This
module builds that unit as a reusable `Worker` type with a bounded `Wait`, so a
stuck job can never block shutdown forever.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. It imports no other exercise.

## What you'll build

```text
worker/                    module example.com/worker
  go.mod                   go 1.26
  worker.go                type Worker; NewWorker, Start, Wait, Name
  cmd/
    demo/
      main.go              start a ticking worker, cancel, watch it stop
  worker_test.go           exits-on-cancel, wait-times-out, name round-trip
```

Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
Implement: `NewWorker(name string, work func(context.Context)) *Worker` with `Start(ctx)`, `Wait(timeout) error`, and `Name() string`.
Test: a ticking worker increments a counter and stops on cancel; a worker ignoring its context makes `Wait` return a timeout error; `Name` round-trips.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/14-select-and-context/11-graceful-shutdown-with-context/01-worker-lifecycle-cancellation/cmd/demo
cd go-solutions/14-select-and-context/11-graceful-shutdown-with-context/01-worker-lifecycle-cancellation
```

## Why the stopped channel and the bounded Wait

A worker has two jobs beyond doing its work: it must exit when told to, and it
must let the shutdown coordinator observe that it has exited. `Start` launches
exactly one goroutine that runs the supplied `work` function; the function is
expected to return when its context is cancelled. The goroutine `defer`s a
`close(w.stopped)`, so the `stopped` channel becomes readable the instant the work
returns — whether it returned cleanly or the goroutine is unwinding.

`Wait` is the bounded half of the contract. It `select`s on `w.stopped` against
`time.After(timeout)`. If the worker exits within the budget, `Wait` returns
`nil`. If it does not — because the work ignores its context, or is genuinely
wedged — `Wait` returns a non-nil error after the budget elapses and lets the
shutdown proceed. This is the single most important property for graceful
shutdown: a stuck worker degrades to a bounded delay plus an honest error, never
an unbounded hang. The whole process budget is only safe because every worker's
`Wait` is bounded.

`work` receives the context; whether it honors it is the work's responsibility.
A correct worker selects on `ctx.Done()`. The `Wait` timeout exists precisely to
contain the workers that do not.

Create `worker.go`:

```go
package worker

import (
	"context"
	"fmt"
	"time"
)

// Worker is a background goroutine that runs a work function until its context
// is cancelled. It is the atomic unit every long-lived background job is built
// from: a metrics flusher, a cache refresher, a queue consumer.
type Worker struct {
	name    string
	work    func(ctx context.Context)
	stopped chan struct{}
}

// NewWorker returns a Worker that will run work when Start is called. work is
// expected to return when its context is cancelled.
func NewWorker(name string, work func(ctx context.Context)) *Worker {
	return &Worker{name: name, work: work, stopped: make(chan struct{})}
}

// Name returns the worker's name, used in shutdown logs and errors.
func (w *Worker) Name() string { return w.name }

// Start launches the work goroutine. The goroutine runs work(ctx) and closes
// the stopped channel when work returns, so Wait can observe the exit.
func (w *Worker) Start(ctx context.Context) {
	go func() {
		defer close(w.stopped)
		w.work(ctx)
	}()
}

// Wait blocks until the worker exits or timeout elapses. It returns nil on a
// clean exit and a non-nil error if the worker did not stop within the budget,
// so a stuck worker becomes a bounded delay rather than an unbounded hang.
func (w *Worker) Wait(timeout time.Duration) error {
	select {
	case <-w.stopped:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("worker %s did not stop within %v", w.name, timeout)
	}
}
```

## The runnable demo

The demo starts a worker that increments a counter on a fast ticker, lets it run
briefly, cancels, and confirms it stopped and did real work.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/worker"
)

func main() {
	var ticks atomic.Int64
	w := worker.NewWorker("ticker", func(ctx context.Context) {
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ticks.Add(1)
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	time.Sleep(30 * time.Millisecond)
	cancel()

	if err := w.Wait(time.Second); err != nil {
		fmt.Println("shutdown error:", err)
		return
	}
	fmt.Printf("worker %s stopped cleanly after doing work: %v\n", w.Name(), ticks.Load() > 0)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker ticker stopped cleanly after doing work: true
```

## Tests

The tests pin the three contract properties, all hermetic and signal-free.
`TestWorkerExitsOnCancel` runs a ticking worker, cancels, and asserts `Wait`
returns `nil` and the counter advanced, proving both that it stopped and that it
ran. `TestWorkerWaitTimesOut` starts a worker that deliberately ignores its
context and sleeps well past the budget; `Wait(30ms)` must return a non-nil error,
proving the budget bounds a stuck worker. `TestWorkerNameReturned` asserts `Name`
round-trips.

Create `worker_test.go`:

```go
package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerExitsOnCancel(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	w := NewWorker("counter", func(ctx context.Context) {
		tk := time.NewTicker(2 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				calls.Add(1)
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)

	time.Sleep(20 * time.Millisecond)
	cancel()

	if err := w.Wait(500 * time.Millisecond); err != nil {
		t.Fatalf("Wait after cancel: %v; want nil", err)
	}
	if calls.Load() == 0 {
		t.Fatal("worker did no work before stopping")
	}
}

func TestWorkerWaitTimesOut(t *testing.T) {
	t.Parallel()

	w := NewWorker("stuck", func(ctx context.Context) {
		// Deliberately ignore ctx to simulate a wedged worker.
		time.Sleep(500 * time.Millisecond)
	})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()

	if err := w.Wait(30 * time.Millisecond); err == nil {
		t.Fatal("Wait: want timeout error for a stuck worker, got nil")
	}
}

func TestWorkerNameReturned(t *testing.T) {
	t.Parallel()

	w := NewWorker("metrics", func(context.Context) {})
	if got := w.Name(); got != "metrics" {
		t.Fatalf("Name() = %q, want %q", got, "metrics")
	}
}

func ExampleWorker() {
	done := make(chan struct{})
	w := NewWorker("once", func(ctx context.Context) {
		<-ctx.Done()
		close(done)
	})

	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	cancel()
	_ = w.Wait(time.Second)
	<-done
	println() // discarded; keep example runnable
	// Output:
}
```

## Review

The worker is correct when three things hold. First, `Start` runs `work` exactly
once and `close(stopped)` runs on return, so `Wait` unblocks precisely when the
work goroutine exits — not before, not after. Second, `Wait` is bounded: a worker
that never returns turns into a `timeout` error after the budget, never an
infinite block, which is the property the whole process grace budget depends on.
Third, honoring the context is the work's job, and `TestWorkerWaitTimesOut`
proves the design survives a worker that shirks it. The mistake to avoid is
calling `Wait` on a worker whose `Start` was never called: `stopped` is never
closed and `Wait` blocks for the full timeout every time. Run `go test -race` to
confirm the counter and channel are accessed cleanly across the goroutine
boundary.

## Resources

- [context.Context and cancellation](https://pkg.go.dev/context#Context) — the `Done()` channel every worker selects on.
- [sync/atomic.Int64](https://pkg.go.dev/sync/atomic#Int64) — the race-safe counter the tests use to prove work happened.
- [time.After and select](https://go.dev/ref/spec#Select_statements) — the bounded-wait idiom that caps a stuck worker.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-http-graceful-drain.md](02-http-graceful-drain.md)
