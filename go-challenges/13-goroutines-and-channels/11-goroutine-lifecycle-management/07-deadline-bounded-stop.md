# Exercise 7: Stop With a Shutdown Deadline: Wait, but Not Forever

Exercise 1 built a `Stop` that blocks on `<-done` unconditionally. That is correct
until one goroutine gets stuck draining — then the whole shutdown hangs behind it.
Production teardown needs the other half of the trade-off: wait for a clean drain,
but bail after a deadline so a single stuck worker cannot freeze the process. This
exercise upgrades `Stop` to `Stop(ctx)` that races the drain against the deadline.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
boundedstop/               independent module: example.com/boundedstop
  go.mod
  worker.go                Worker; Start, Stop(ctx), Wait; ErrStopTimeout, ErrNotStarted
  cmd/
    demo/
      main.go              runnable demo: a fast clean stop and a slow timed-out stop
  worker_test.go           fast-stop-returns-nil, slow-stop-times-out-but-eventually-drains
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: `Stop(ctx)` that signals stop, then `select`s `<-done` against `<-ctx.Done()`; returns `nil` on a timely drain, `ErrStopTimeout` when the deadline hits — leaving the goroutine to finish on its own (tracked via `Wait`), never blocking the caller past the deadline.
- Test: a fast-draining worker returns `nil`; a slow worker exceeding the deadline returns `ErrStopTimeout`, the caller unblocks within the deadline, and the detached goroutine still completes cleanly (no panic, no double-close).
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/boundedstop/cmd/demo
cd ~/go-exercises/boundedstop
go mod init example.com/boundedstop
```

### Racing the drain against the deadline

The worker keeps the Exercise 1 shape — a `run` loop selecting over a `stop`
channel, `ctx.Done()`, and a ticker — with one added twist to model a slow
teardown: when it receives the stop signal it performs a `drain` (a configurable
sleep standing in for flushing a buffer, committing an offset, closing a
connection) before returning. That drain is what can overrun a shutdown deadline.

The lifecycle handshake is the same as Exercise 1 but the *waiting* is different.
`Stop(ctx)` closes the stop channel to signal, then instead of a bare `<-done` it
does:

```go
select {
case <-done:
	return nil            // drained in time
case <-ctx.Done():
	return ErrStopTimeout // deadline hit; stop waiting, but do not kill anything
}
```

If the goroutine finishes its drain before the caller's deadline, `done` closes
and `Stop` returns `nil`. If the deadline fires first, `Stop` returns
`ErrStopTimeout` and — crucially — stops *waiting*. It does not, and cannot, kill
the goroutine; Go has no such primitive. The goroutine is now *detached*: no
longer waited on by `Stop`, but still tracked. It will finish its drain and close
`done` on its own a moment later. "Detached" must never mean "leaked," so the
worker keeps `done` reachable and exposes `Wait()` for a caller that wants to
confirm the straggler eventually finished (a test does exactly this; a real
shutdown might log it and move on).

Two details keep this race-safe. First, `run` is handed its `stop` and `done`
channels as parameters at `Start`, so `Stop` nilling the struct's `stop` field
(to make a second `Stop` return `ErrNotStarted`) does not disturb the running
goroutine, which holds its own reference. Second, the stop channel is closed
exactly once — `Stop` snapshots and nils the field under the mutex before closing
— so even the timed-out path never double-closes.

Create `worker.go`:

```go
package boundedstop

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrStopTimeout = errors.New("stop deadline exceeded before worker drained")
	ErrNotStarted  = errors.New("worker not started")
)

// Worker runs a ticking loop and, on stop, performs a drain of configurable
// duration before exiting. Stop(ctx) waits for that drain only until ctx's
// deadline.
type Worker struct {
	drain time.Duration

	mu     sync.Mutex
	stop   chan struct{}
	done   chan struct{}
	result int64
}

// New returns a worker whose teardown drain takes the given duration.
func New(drain time.Duration) *Worker {
	return &Worker{drain: drain}
}

// Start launches the run loop. A second Start without a Stop is a no-op guarded
// by the non-nil stop channel; here it simply returns (single-start callers).
func (w *Worker) Start(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return
	}
	w.stop = make(chan struct{})
	w.done = make(chan struct{})
	go w.run(ctx, w.stop, w.done)
}

func (w *Worker) run(ctx context.Context, stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			time.Sleep(w.drain) // slow teardown
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

// Stop signals the worker to leave and waits for it to drain, but only until
// ctx's deadline. It returns nil on a timely drain, ErrStopTimeout if the
// deadline hits first (the goroutine keeps draining and is reachable via Wait),
// or ErrNotStarted if the worker is not running.
func (w *Worker) Stop(ctx context.Context) error {
	w.mu.Lock()
	if w.stop == nil {
		w.mu.Unlock()
		return ErrNotStarted
	}
	stop, done := w.stop, w.done
	w.stop = nil
	w.mu.Unlock()

	close(stop)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ErrStopTimeout
	}
}

// Wait blocks until the run goroutine has fully exited. It is safe to call
// after a Stop that timed out, to confirm the detached goroutine finished.
func (w *Worker) Wait() {
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	if done != nil {
		<-done
	}
}
```

### The runnable demo

The demo runs two workers: a fast one that drains within the deadline (clean
stop), and a slow one whose drain overruns a short deadline (timed-out stop), then
waits for the slow one to finish to show it was detached, not lost.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/boundedstop"
)

func stopWithin(w *boundedstop.Worker, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return w.Stop(ctx)
}

func main() {
	fast := boundedstop.New(5 * time.Millisecond)
	fast.Start(context.Background())
	time.Sleep(30 * time.Millisecond)
	fmt.Println("fast stop clean:", stopWithin(fast, time.Second) == nil)

	slow := boundedstop.New(200 * time.Millisecond)
	slow.Start(context.Background())
	time.Sleep(30 * time.Millisecond)
	err := stopWithin(slow, 20*time.Millisecond)
	fmt.Println("slow stop timed out:", errors.Is(err, boundedstop.ErrStopTimeout))

	slow.Wait() // the detached goroutine still finishes
	fmt.Println("slow worker eventually drained")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
fast stop clean: true
slow stop timed out: true
slow worker eventually drained
```

### Tests

`TestFastStopReturnsNil` proves the timely path: a worker whose drain is far
shorter than the deadline stops cleanly. `TestSlowStopTimesOut` proves the whole
point of the exercise at once: a worker whose drain overruns a short deadline
makes `Stop(ctx)` return `ErrStopTimeout`; the caller unblocks *within* the
deadline (measured elapsed, not the full drain); and a follow-up `Wait` confirms
the detached goroutine still finishes cleanly with no panic and no double-close.
`TestStopNotStarted` proves stopping an unstarted worker is `ErrNotStarted`.

Create `worker_test.go`:

```go
package boundedstop

import (
	"context"
	"errors"
	"testing"
	"time"
)

func stopWithin(t *testing.T, w *Worker, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return w.Stop(ctx)
}

func TestFastStopReturnsNil(t *testing.T) {
	t.Parallel()

	w := New(2 * time.Millisecond)
	w.Start(context.Background())
	time.Sleep(20 * time.Millisecond)

	if err := stopWithin(t, w, time.Second); err != nil {
		t.Fatalf("Stop = %v, want nil", err)
	}
}

func TestSlowStopTimesOut(t *testing.T) {
	t.Parallel()

	const deadline = 20 * time.Millisecond
	w := New(200 * time.Millisecond) // drain far exceeds the deadline
	w.Start(context.Background())
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	err := stopWithin(t, w, deadline)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("Stop = %v, want ErrStopTimeout", err)
	}
	if elapsed > deadline+50*time.Millisecond {
		t.Fatalf("caller blocked %v, want it to unblock near the %v deadline", elapsed, deadline)
	}

	// The detached goroutine must still finish cleanly.
	doneCh := make(chan struct{})
	go func() { w.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatal("detached goroutine never finished draining")
	}
}

func TestStopNotStarted(t *testing.T) {
	t.Parallel()

	w := New(time.Millisecond)
	if err := stopWithin(t, w, time.Second); !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Stop = %v, want ErrNotStarted", err)
	}
}
```

## Review

The bounded stop is correct when it never blocks the caller past the deadline yet
never leaks the worker. `TestSlowStopTimesOut` pins both halves: `Stop` returns
`ErrStopTimeout` and the caller unblocks near the deadline (not after the full
200 ms drain), and `Wait` confirms the detached goroutine finished on its own —
detached, not leaked. The trap this exercise inoculates against is the
unconditional `<-done` from Exercise 1: correct in the common case, but a single
stuck drain hangs the entire shutdown behind it. Racing `<-done` against
`<-ctx.Done()` bounds the wait while still preferring a clean drain when one is
available. The second trap is a double-close on the stop channel across the two
`Stop` paths, avoided by snapshotting and nilling the field under the mutex before
the single `close`. Run `go test -count=1 -race ./...`.

## Resources

- [`context.WithTimeout`](https://pkg.go.dev/context#WithTimeout) — the deadline that bounds the drain wait.
- [`select`](https://go.dev/ref/spec#Select_statements) — racing the done channel against the deadline.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — the done-channel and cancellation patterns behind this.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [06-supervisor-restart-backoff.md](06-supervisor-restart-backoff.md) | Next: [08-cancel-cause-diagnostics.md](08-cancel-cause-diagnostics.md)
