# Exercise 34: Graceful Shutdown: Coordinating N Goroutines When Some Panic

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A server with N long-lived background goroutines - a cache warmer, a
metrics flusher, a connection drainer - needs a shutdown path that waits
for every single one of them to actually finish before the process exits,
so in-flight cleanup (flushing a buffer, draining connections gracefully)
has a chance to run to completion. The failure mode that makes this hard is
not "shutdown takes too long" - it is a worker whose shutdown-path cleanup
panics (a nil buffer it assumed was still populated, a double-close on a
connection another worker already tore down): if that panic is not
isolated, it crashes the process mid-shutdown, and if the coordinator does
not account for it, `Shutdown` can end up waiting forever for a goroutine
that already died without ever reporting back. This module builds
`Coordinator`, which cancels every worker's context on `Shutdown` and
blocks until every one of them has returned - success, error, or panic -
before reporting a complete, per-worker outcome. It is fully self-contained:
its own module, demo, and tests.

## What you'll build

```text
shutdown/                   independent module: example.com/shutdown
  go.mod                     go 1.24
  shutdown.go                  Worker, Outcome, Coordinator, Start, Shutdown, runWorker
  cmd/
    demo/
      main.go                runnable demo: 3 workers, one panics during shutdown cleanup
  shutdown_test.go              outcomes in registration order, Shutdown waits for a slow worker, empty
```

Files: `shutdown.go`, `cmd/demo/main.go`, `shutdown_test.go`.
Implement: `Start(ctx context.Context, workers []Worker) *Coordinator` launching one goroutine per worker under a derived, cancellable context; `Coordinator.Shutdown() []Outcome` that cancels that context and blocks until every worker's `Run` has returned, isolating each worker's panic in `runWorker` so it can never take down the coordinator or a sibling worker.
Test: three workers - clean, panicking during shutdown, and returning a plain error during shutdown - asserting outcomes come back in registration order regardless of goroutine completion order; a worker that deliberately delays its return past cancellation, proving `Shutdown` truly blocks for it rather than returning as soon as the context is cancelled; no workers at all.
Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/03-control-flow/08-panic-and-recover/34-graceful-shutdown-coordinator/cmd/demo
cd go-solutions/03-control-flow/08-panic-and-recover/34-graceful-shutdown-coordinator
go mod edit -go=1.24
```

### Why Shutdown blocks on a result channel instead of just calling cancel and returning

`Start` launches a single internal goroutine that fans out to one goroutine
per worker, waits on a `sync.WaitGroup` for every one of them, and only then
sends the completed `[]Outcome` down a buffered channel. `Shutdown` itself
does two things in sequence: call `cancel()` to signal every worker's
context, then receive from that channel. Calling `cancel()` alone would
only *ask* the workers to stop - a worker's `Run` might do real cleanup work
after observing `<-ctx.Done()` (flushing a buffer, waiting for connections
to drain) that takes a nontrivial amount of time, and a `Shutdown` that
returned immediately after `cancel()` would hand the caller a promise
("shutdown initiated") rather than a fact ("shutdown complete, here is what
happened to each worker"). Blocking on the result channel is what makes
`Shutdown` mean "every worker has actually finished," which is the only
meaning that lets a caller safely proceed to, say, close the listener or
exit the process next.

Each worker's `Run` executes inside `runWorker`, which is the recover
boundary: a panic during shutdown-path cleanup - the failure this exercise
specifically targets, not a panic during normal operation - is isolated to
that one worker's own goroutine and converted into that worker's `Outcome`,
exactly like every other goroutine-spawning pattern in this chapter. What
is specific to a *coordinator* is the ordering guarantee: outcomes are
written into a slice pre-indexed by each worker's registration position
(the same technique used for the tracing module's child spans), so the
report a caller sees always lists workers in the order they were started,
never in whatever order their goroutines happened to actually return -
which is exactly what you want when correlating a shutdown report against
a fixed list of named subsystems in a runbook.

Create `shutdown.go`:

```go
package shutdown

import (
	"context"
	"fmt"
	"sync"
)

// Worker is one background goroutine's shutdown-aware body: it must return
// once ctx is cancelled, and may return an error - or, the case this
// package defends against, panic while cleaning up its own resources
// during shutdown (a nil buffer it assumed was still open, a double-close
// on a connection another worker already tore down).
type Worker struct {
	Name string
	Run  func(ctx context.Context) error
}

// Outcome is what one worker did when the coordinator shut it down.
type Outcome struct {
	Name     string
	Err      error
	Panicked bool
}

// Coordinator runs N workers concurrently and, on Shutdown, cancels every
// worker's context and waits for every one of them to actually return -
// whether cleanly, with an error, or by panicking - before reporting back.
type Coordinator struct {
	cancel context.CancelFunc
	done   chan []Outcome
}

// Start launches every worker in its own goroutine, each receiving a
// context derived from ctx. A worker's shutdown-path panic is isolated to
// that worker's own goroutine - it can never take down the coordinator or
// a sibling worker - so Shutdown always returns a complete outcome for
// every worker instead of hanging on one that failed to unwind cleanly.
func Start(ctx context.Context, workers []Worker) *Coordinator {
	ctx, cancel := context.WithCancel(ctx)
	c := &Coordinator{
		cancel: cancel,
		done:   make(chan []Outcome, 1),
	}

	go func() {
		outcomes := make([]Outcome, len(workers))
		var wg sync.WaitGroup
		wg.Add(len(workers))
		for i, w := range workers {
			i, w := i, w
			go func() {
				defer wg.Done()
				outcomes[i] = runWorker(ctx, w)
			}()
		}
		wg.Wait()
		c.done <- outcomes
	}()

	return c
}

// Shutdown cancels every worker's context and blocks until all of them have
// returned, then reports each worker's outcome in worker-registration
// order - never in whatever order the goroutines happened to finish - so
// callers get a stable, reproducible report.
func (c *Coordinator) Shutdown() []Outcome {
	c.cancel()
	return <-c.done
}

// runWorker is the recover boundary: exactly one worker's untrusted Run,
// running in its own goroutine for the coordinator's entire lifetime.
func runWorker(ctx context.Context, w Worker) (outcome Outcome) {
	outcome = Outcome{Name: w.Name}
	defer func() {
		if r := recover(); r != nil {
			outcome.Panicked = true
			if e, ok := r.(error); ok {
				outcome.Err = fmt.Errorf("worker %q panicked: %w", w.Name, e)
				return
			}
			outcome.Err = fmt.Errorf("worker %q panicked: %v", w.Name, r)
		}
	}()
	if err := w.Run(ctx); err != nil {
		outcome.Err = fmt.Errorf("worker %q failed: %w", w.Name, err)
	}
	return outcome
}
```

### The runnable demo

Three workers: `cache-warmer` shuts down cleanly, `metrics-flusher` panics
on a nil pointer while flushing its buffer, and `conn-drainer` returns an
ordinary error after the grace period expires with connections still open.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/shutdown"
)

func main() {
	workers := []shutdown.Worker{
		{
			Name: "cache-warmer",
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				return nil
			},
		},
		{
			Name: "metrics-flusher",
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				var buf *struct{ Data []byte }
				fmt.Println(len(buf.Data)) // nil pointer dereference
				return nil
			},
		},
		{
			Name: "conn-drainer",
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				return fmt.Errorf("5 connections still active after grace period")
			},
		},
	}

	coord := shutdown.Start(context.Background(), workers)
	outcomes := coord.Shutdown()

	for _, o := range outcomes {
		status := "ok"
		if o.Err != nil {
			status = o.Err.Error()
		}
		fmt.Printf("%s: %s\n", o.Name, status)
	}
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
cache-warmer: ok
metrics-flusher: worker "metrics-flusher" panicked: runtime error: invalid memory address or nil pointer dereference
conn-drainer: worker "conn-drainer" failed: 5 connections still active after grace period
```

### Tests

`TestShutdownCollectsAllOutcomesInRegistrationOrder` covers all three
outcome shapes (success, panic, plain error) and asserts registration
order. `TestShutdownWaitsForSlowWorkers` is the liveness check that matters
most: a worker holds past cancellation until explicitly released, and the
test asserts `Shutdown` is still blocked (via a short bounded wait) before
release and returns promptly with the correct outcome after.

Create `shutdown_test.go`:

```go
package shutdown

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownCollectsAllOutcomesInRegistrationOrder(t *testing.T) {
	workers := []Worker{
		{Name: "a", Run: func(ctx context.Context) error { <-ctx.Done(); return nil }},
		{Name: "b", Run: func(ctx context.Context) error {
			<-ctx.Done()
			panic(errors.New("flush failed"))
		}},
		{Name: "c", Run: func(ctx context.Context) error {
			<-ctx.Done()
			return errors.New("still draining")
		}},
	}

	coord := Start(context.Background(), workers)
	outcomes := coord.Shutdown()

	if len(outcomes) != 3 {
		t.Fatalf("len(outcomes) = %d, want 3", len(outcomes))
	}
	if outcomes[0].Name != "a" || outcomes[0].Err != nil || outcomes[0].Panicked {
		t.Fatalf("outcomes[0] = %+v, want a clean success", outcomes[0])
	}
	if outcomes[1].Name != "b" || !outcomes[1].Panicked || !strings.Contains(outcomes[1].Err.Error(), "flush failed") {
		t.Fatalf("outcomes[1] = %+v, want a panic wrapping flush failed", outcomes[1])
	}
	if outcomes[2].Name != "c" || outcomes[2].Panicked || !strings.Contains(outcomes[2].Err.Error(), "still draining") {
		t.Fatalf("outcomes[2] = %+v, want a plain error, not a panic", outcomes[2])
	}
}

func TestShutdownWaitsForSlowWorkers(t *testing.T) {
	release := make(chan struct{})
	var flag int32

	workers := []Worker{
		{
			Name: "slow",
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				<-release // does not return until told to
				atomic.StoreInt32(&flag, 1)
				return nil
			},
		},
	}

	coord := Start(context.Background(), workers)

	done := make(chan []Outcome, 1)
	go func() { done <- coord.Shutdown() }()

	// Shutdown must still be blocked: the worker has not been released yet.
	select {
	case <-done:
		t.Fatal("Shutdown returned before the slow worker finished")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case outcomes := <-done:
		if len(outcomes) != 1 || outcomes[0].Err != nil {
			t.Fatalf("outcomes = %+v, want 1 clean outcome", outcomes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown never returned after the slow worker was released")
	}

	if atomic.LoadInt32(&flag) != 1 {
		t.Fatal("worker cleanup never ran")
	}
}

func TestShutdownNoWorkers(t *testing.T) {
	coord := Start(context.Background(), nil)
	outcomes := coord.Shutdown()
	if len(outcomes) != 0 {
		t.Fatalf("outcomes = %v, want empty", outcomes)
	}
}
```

## Review

`Coordinator` is correct when `Shutdown` reports a true fact - every worker
has actually returned - rather than an optimistic guess made right after
firing `cancel()`, and when a shutdown-path panic in any one worker can
never prevent the coordinator from reporting on the rest. The ordering
guarantee is easy to get wrong in a subtle way: appending each worker's
`Outcome` to a shared slice as its goroutine finishes compiles fine and
passes a quick manual test, but produces a report whose order silently
depends on which worker happened to shut down fastest - exactly the kind
of nondeterminism that looks fine in development and then produces a
confusing, differently-ordered report every time it runs in CI or
production.

## Resources

- [context package](https://pkg.go.dev/context) — `context.WithCancel` and `ctx.Done()`, the cancellation signal every worker's shutdown path watches for.
- [Go Blog: Defer, Panic, and Recover](https://go.dev/blog/defer-panic-and-recover) — the per-worker recover boundary in `runWorker`.
- [sync.WaitGroup](https://pkg.go.dev/sync#WaitGroup) — coordinating the wait for every worker goroutine before the coordinator reports back.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [33-pipelined-stages-cascade-stop.md](33-pipelined-stages-cascade-stop.md) | Next: [../09-range-over-integers-and-functions/00-concepts.md](../09-range-over-integers-and-functions/00-concepts.md)
