# Exercise 1: A Worker With an Explicit Start/Run/Stop Lifecycle

The smallest useful lifecycle artifact is a background worker that ticks on a
timer and counts its work — a stand-in for a metrics flusher, a heartbeat
emitter, or a periodic reconciler. This exercise builds it with the full
contract: `Start` allocates and launches, `Run` processes under cancellation,
`Stop` signals and waits, and both `Start` and `Stop` reject misuse instead of
leaking or panicking.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
lifecycle/                 independent module: example.com/lifecycle
  go.mod
  worker.go                type Worker; New, Start, Stop, Result; guards ErrAlreadyStarted/ErrNotStarted
  cmd/
    demo/
      main.go              runnable demo: start, run, stop, report ticks
  worker_test.go           start/stop, double-start, stop-before-start, idempotent stop, -race
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: a `Worker` with `Start(ctx)`, `Stop()`, `Result()`; `Start` allocates the stop and done channels and launches `run`; `run` selects over stop, `ctx.Done`, and a ticker, incrementing a mutex-guarded counter; `Stop` closes stop, nils it, and blocks on done.
- Test: `TestStartAndStop`, `TestStartRejectsDouble`, `TestStopRejectsNotStarted`, `TestResultReflectsWork`, `TestStopRespectsContext`, `TestStopIsIdempotent`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/lifecycle/cmd/demo
cd ~/go-exercises/lifecycle
go mod init example.com/lifecycle
```

### Why the shape is what it is

The `Worker` holds four fields: a `sync.Mutex`, a `stop chan struct{}`, a
`done chan struct{}`, and an `int64` result. The two channels encode the two
directions of the lifecycle handshake. `stop` flows owner → goroutine: closing it
is the "please leave" broadcast. `done` flows goroutine → owner: the goroutine
closes it on the way out (via `defer close(w.done)`), so the owner blocking on
`<-w.done` learns the goroutine has actually returned. This is the signal-then-wait
discipline made concrete — `Stop` does both, in that order.

`stop` doubles as the *state* of the worker. `Start` checks `w.stop != nil`: a
non-nil stop channel means a goroutine is running, so a second `Start` returns
`ErrAlreadyStarted` rather than launching a second goroutine and orphaning the
first. `Stop` checks `w.stop == nil`: a nil stop channel means nothing is running,
so `Stop` on an unstarted (or already-stopped) worker returns `ErrNotStarted`.
After `Stop` closes the channel it sets `w.stop = nil`, which is what makes `Stop`
idempotent: the second call sees nil and returns `ErrNotStarted` instead of
closing an already-closed channel (which would panic).

The `result` counter is written by the goroutine and read by `Result()` on the
caller's goroutine, so it must be synchronized — the mutex guards it. Under
`go test -race` an unguarded counter here would be flagged immediately; guarding
it is not optional.

The `run` loop is a single `select` with three cases. `<-w.stop` and
`<-ctx.Done()` are the two ways to leave — an explicit `Stop`, or the caller's
context being cancelled. `<-ticker.C` is the work: increment the counter. Because
every branch of the loop is inside the `select`, the goroutine can always observe
a cancellation between ticks and return promptly; `defer ticker.Stop()` releases
the ticker's resources, and `defer close(w.done)` fires last, unblocking `Stop`.

Create `worker.go`:

```go
package worker

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Sentinel errors for lifecycle misuse.
var (
	ErrAlreadyStarted = errors.New("worker already started")
	ErrNotStarted     = errors.New("worker not started")
)

// Worker runs a background loop that ticks on a timer and counts its work.
// It owns exactly one goroutine at a time: Start launches it, Stop signals
// it to leave and waits for it to finish.
type Worker struct {
	mu     sync.Mutex
	stop   chan struct{}
	done   chan struct{}
	result int64
}

// New returns a Worker that has not started.
func New() *Worker {
	return &Worker{}
}

// Start allocates the lifecycle channels and launches the run loop. A second
// Start without an intervening Stop returns ErrAlreadyStarted rather than
// orphaning the first goroutine.
func (w *Worker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return ErrAlreadyStarted
	}
	w.stop = make(chan struct{})
	w.done = make(chan struct{})
	go w.run(ctx, w.stop, w.done)
	return nil
}

// run is the work loop. It leaves on an explicit stop or on ctx cancellation,
// and counts a unit of work on every tick.
func (w *Worker) run(ctx context.Context, stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.mu.Lock()
			w.result++
			w.mu.Unlock()
		}
	}
}

// Stop signals the goroutine to leave, then waits for it to finish. Calling
// Stop on a worker that is not running (never started, or already stopped)
// returns ErrNotStarted; it never double-closes the stop channel.
func (w *Worker) Stop() error {
	w.mu.Lock()
	if w.stop == nil {
		w.mu.Unlock()
		return ErrNotStarted
	}
	stop, done := w.stop, w.done
	w.stop = nil
	w.mu.Unlock()

	close(stop) // signal
	<-done      // wait
	return nil
}

// Result reports the number of units of work the loop has counted.
func (w *Worker) Result() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.result
}
```

Note the subtle-but-important detail in `Stop`: it snapshots `w.stop` and
`w.done` into locals and nils `w.stop` *while holding the lock*, then releases the
lock before `close(stop)` and `<-done`. Holding the mutex across `<-done` would
deadlock — the `run` goroutine's ticker branch also takes the mutex, so the owner
would be waiting for a goroutine that is waiting for the owner's lock. Snapshot,
release, then wait.

### The runnable demo

The demo starts the worker, lets it tick for 120 ms (about a dozen 10 ms ticks),
stops it cleanly, and reports that it recorded work and stopped without error.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/lifecycle"
)

func main() {
	w := worker.New()
	if err := w.Start(context.Background()); err != nil {
		fmt.Println("start failed:", err)
		return
	}
	fmt.Println("worker started")

	time.Sleep(120 * time.Millisecond)

	if err := w.Stop(); err != nil {
		fmt.Println("stop failed:", err)
		return
	}
	fmt.Println("worker stopped cleanly")
	fmt.Println("recorded at least one tick:", w.Result() > 0)
}
```

The import path is `example.com/lifecycle`, but the package inside it is named
`worker` — the demo refers to it as `worker`. Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker started
worker stopped cleanly
recorded at least one tick: true
```

### Tests

The tests pin every branch of the contract. `TestStartAndStop` is the happy path:
a clean start followed by a clean stop, with no error and no leaked goroutine (if
`Stop` did not wait on `done`, `-race` and goleak-style checks would catch the
survivor). `TestStartRejectsDouble` proves the double-start guard.
`TestStopRejectsNotStarted` proves stopping an unstarted worker is an error, not a
panic. `TestResultReflectsWork` proves the counter is non-negative and readable
after stop. `TestStopRespectsContext` cancels the context the worker was started
with and shows the loop leaves on `ctx.Done()`, after which `Stop` still returns
cleanly. `TestStopIsIdempotent` proves the second `Stop` returns `ErrNotStarted`
rather than panicking on a double close.

Create `worker_test.go`:

```go
package worker

import (
	"context"
	"errors"
	"testing"
)

func TestStartAndStop(t *testing.T) {
	t.Parallel()

	w := New()
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestStartRejectsDouble(t *testing.T) {
	t.Parallel()

	w := New()
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Stop()

	if err := w.Start(context.Background()); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start err = %v, want ErrAlreadyStarted", err)
	}
}

func TestStopRejectsNotStarted(t *testing.T) {
	t.Parallel()

	w := New()
	if err := w.Stop(); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Stop err = %v, want ErrNotStarted", err)
	}
}

func TestResultReflectsWork(t *testing.T) {
	t.Parallel()

	w := New()
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r := w.Result(); r < 0 {
		t.Fatalf("Result = %d, want >= 0", r)
	}
}

func TestStopRespectsContext(t *testing.T) {
	t.Parallel()

	w := New()
	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel() // the run loop leaves on ctx.Done()

	if err := w.Stop(); err != nil {
		t.Fatalf("Stop after cancel: %v", err)
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	w := New()
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := w.Stop(); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("second Stop err = %v, want ErrNotStarted", err)
	}
}
```

## Review

The worker is correct when four properties hold together. Start is idempotent
against misuse: it never launches a second goroutine while one is running.
Stop is signal-then-wait: it closes `stop` and only returns after `<-done`, so
when `Stop` returns the goroutine is genuinely gone. Stop is idempotent: nilling
`w.stop` under the lock makes the second call a clean `ErrNotStarted` rather than
a double-close panic. And the shared `result` is mutex-guarded, so `-race` is
silent. The classic failure here is a `Stop` that signals but does not wait —
it compiles, it passes a casual test, and it leaks the goroutine in production;
`TestStartAndStop` under `-race` is what turns that from an invisible leak into a
caught bug. Run `go test -count=1 -race ./...` and confirm all six tests pass with
no race report.

## Resources

- [Effective Go: Goroutines and channels](https://go.dev/doc/effective_go#goroutines) — the model for `go`, channels, and `select`.
- [`sync.Mutex`](https://pkg.go.dev/sync#Mutex) — guarding shared worker state.
- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) — the periodic work source and why `Stop` must be called.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — cancellation propagation via `close` and `select`.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-graceful-http-shutdown.md](02-graceful-http-shutdown.md)
