# Exercise 8: Record Why a Worker Stopped With context.WithCancelCause

When a background component stops, `ctx.Err()` tells you only the coarse reason:
`context.Canceled` or `context.DeadlineExceeded`. That is not enough to debug a
production shutdown — was it an operator-requested stop, an upstream dependency
failure, or a deadline? `context.WithCancelCause` lets the code that cancels
attach a specific, machine-readable *cause*, and `context.Cause` retrieves it.
This exercise builds a component that records why it stopped.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
causestop/                 independent module: example.com/causestop
  go.mod
  worker.go                Worker; New, Start, StopWithCause, Reason; sentinel causes
  cmd/
    demo/
      main.go              runnable demo: operator stop vs. deadline, each with its reason
  worker_test.go           operator-cause, deadline-cause, err-vs-cause tests
```

- Files: `worker.go`, `cmd/demo/main.go`, `worker_test.go`.
- Implement: a `Worker` whose run loop is driven by a cause-carrying context; `StopWithCause(err)` cancels with that cause; the worker records `context.Cause(ctx)` as its stop reason so teardown is observable.
- Test: cancelling with a sentinel cause makes `context.Cause` return it while `ctx.Err()` is `context.Canceled`; a deadline parent makes both `Cause` and `Err` be `context.DeadlineExceeded`; the recorded reason matches the cause.
- Verify: `go test -count=1 -race ./...`

### Cause versus Err, and how they combine

`context.WithCancelCause(parent)` returns a context and a
`context.CancelCauseFunc` — a cancel function that takes an `error`. Calling
`cancel(err)` cancels the context and stores `err` as its cause. Two retrieval
functions then differ:

- `ctx.Err()` returns the standard coarse reason: `context.Canceled` when the
  context was cancelled (by `cancel`, with or without a cause), or
  `context.DeadlineExceeded` when a deadline fired.
- `context.Cause(ctx)` returns the specific cause: the `err` you passed to
  `cancel(err)` if that is what stopped it; otherwise it falls back to `ctx.Err()`
  (so an uncaused cancel gives `context.Canceled`, and a deadline gives
  `context.DeadlineExceeded`).

The interesting case is a *deadline parent*. If you build
`ctx, cancel := context.WithCancelCause(parent)` where `parent` came from
`context.WithTimeout`, and the parent's deadline fires before anyone calls
`cancel`, then the cancellation propagates from the parent: `ctx.Err()` becomes
`context.DeadlineExceeded`, and because no explicit cause was set on the child,
`context.Cause(ctx)` also reports `context.DeadlineExceeded`. So the same code path
— "read `context.Cause` when `ctx.Done()` fires" — yields your sentinel on an
operator stop and the standard deadline error on a timeout, with no branching.

The worker uses this directly. Its run loop blocks on `<-ctx.Done()`, and when it
wakes it records `context.Cause(ctx)` into a mutex-guarded `reason` field that
`Reason()` reads. `StopWithCause(err)` calls the cause-carrying cancel and then
waits on the done channel, so by the time it returns the reason is recorded.
Distinct sentinel causes — operator stop, upstream failure — let an operator read
the exact reason from logs or a status endpoint.

Create `worker.go`:

```go
package causestop

import (
	"context"
	"errors"
	"sync"
)

// Sentinel causes describe why the worker was stopped. They are machine-readable
// so operators and tests can match on them with errors.Is.
var (
	ErrOperatorStop = errors.New("operator requested stop")
	ErrUpstreamDown = errors.New("upstream dependency failed")
)

// Worker runs until its context is cancelled, then records the cancellation
// cause so shutdown is observable.
type Worker struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}

	mu     sync.Mutex
	reason error
}

// New builds a Worker whose lifecycle is bounded by parent. Passing a
// context.WithTimeout parent gives the worker a deadline whose expiry is
// reported as context.DeadlineExceeded.
func New(parent context.Context) *Worker {
	ctx, cancel := context.WithCancelCause(parent)
	return &Worker{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// Start launches the run loop.
func (w *Worker) Start() {
	go w.run()
}

func (w *Worker) run() {
	defer close(w.done)
	<-w.ctx.Done()
	cause := context.Cause(w.ctx)
	w.mu.Lock()
	w.reason = cause
	w.mu.Unlock()
}

// StopWithCause cancels the worker with the given cause and waits for it to
// stop. After it returns, Reason reports cause.
func (w *Worker) StopWithCause(cause error) {
	w.cancel(cause)
	<-w.done
}

// Reason reports why the worker stopped: the cause passed to StopWithCause, or
// context.DeadlineExceeded if a deadline parent fired first. It returns nil
// while the worker is still running.
func (w *Worker) Reason() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

// Err reports the coarse context error (context.Canceled or
// context.DeadlineExceeded), independent of the specific cause.
func (w *Worker) Err() error {
	return w.ctx.Err()
}
```

### The runnable demo

The demo stops one worker with an operator cause and lets another expire on a
deadline, printing each worker's recorded reason.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/causestop"
)

func main() {
	// Worker A: stopped by an operator.
	a := causestop.New(context.Background())
	a.Start()
	a.StopWithCause(causestop.ErrOperatorStop)
	fmt.Println("worker A reason:", a.Reason())

	// Worker B: bounded by a deadline that fires on its own.
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	b := causestop.New(parent)
	b.Start()
	time.Sleep(30 * time.Millisecond) // let the deadline fire
	b.StopWithCause(causestop.ErrOperatorStop)
	fmt.Println("worker B reason:", b.Reason())
}
```

Because worker B's parent deadline fires before `StopWithCause` is called, the
cause is fixed at `context.DeadlineExceeded` — the later `StopWithCause` is a
harmless no-op (a context can only be cancelled once, and the first cause wins).

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker A reason: operator requested stop
worker B reason: context deadline exceeded
```

### Tests

`TestOperatorCause` proves the split between coarse and specific: after
`StopWithCause(ErrOperatorStop)`, `Reason()` and `context.Cause` report the
sentinel while `Err()` reports `context.Canceled`. `TestUpstreamCause` shows a
different sentinel flows through unchanged. `TestDeadlineCause` proves the deadline
path: a `context.WithTimeout` parent that fires on its own makes both `Reason()`
and `Err()` be `context.DeadlineExceeded`, with no explicit cancel. Each asserts
with `errors.Is` so wrapping stays robust.

Create `worker_test.go`:

```go
package causestop

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestOperatorCause(t *testing.T) {
	t.Parallel()

	w := New(context.Background())
	w.Start()
	w.StopWithCause(ErrOperatorStop)

	if !errors.Is(w.Reason(), ErrOperatorStop) {
		t.Fatalf("Reason() = %v, want ErrOperatorStop", w.Reason())
	}
	if !errors.Is(w.Err(), context.Canceled) {
		t.Fatalf("Err() = %v, want context.Canceled", w.Err())
	}
}

func TestUpstreamCause(t *testing.T) {
	t.Parallel()

	w := New(context.Background())
	w.Start()
	w.StopWithCause(ErrUpstreamDown)

	if !errors.Is(w.Reason(), ErrUpstreamDown) {
		t.Fatalf("Reason() = %v, want ErrUpstreamDown", w.Reason())
	}
}

func TestDeadlineCause(t *testing.T) {
	t.Parallel()

	parent, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	w := New(parent)
	w.Start()

	// Wait for the deadline to fire and the worker to record its reason.
	<-w.done

	if !errors.Is(w.Reason(), context.DeadlineExceeded) {
		t.Fatalf("Reason() = %v, want context.DeadlineExceeded", w.Reason())
	}
	if !errors.Is(w.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err() = %v, want context.DeadlineExceeded", w.Err())
	}
}
```

## Review

The worker is correct when the two views of a shutdown stay distinct and both are
right. `context.Cause` reports the specific reason — your sentinel on a
`StopWithCause`, `context.DeadlineExceeded` on a deadline — while `ctx.Err()`
reports the coarse `context.Canceled`/`context.DeadlineExceeded`. That split is
what makes teardown observable: an operator reading the recorded `Reason()` learns
*why*, not just *that*, the component stopped. The trap this exercise inoculates
against is reaching only for `ctx.Err()` and losing the diagnostic — every stop
looks identical in the logs. The `reason` field is written by the run goroutine
and read by `Reason()` on another goroutine, so it is mutex-guarded; `-race`
confirms it. Run `go test -count=1 -race ./...`.

## Resources

- [`context.WithCancelCause`](https://pkg.go.dev/context#WithCancelCause) — cancelling with an attached cause.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — retrieving the specific cause versus `ctx.Err()`.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching sentinel causes robustly through wrapping.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-deadline-bounded-stop.md](07-deadline-bounded-stop.md) | Next: [09-once-guarded-closer.md](09-once-guarded-closer.md)
