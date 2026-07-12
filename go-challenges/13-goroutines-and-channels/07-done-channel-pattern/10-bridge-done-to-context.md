# Exercise 10: Bridge a Done Channel to context.Context (and Back)

The through-line of the whole lesson: `context.Done()` returns a `<-chan struct{}` — a `Context` *is*
the done-channel pattern, standardized with a deadline, a value bag, and a cancellation reason. This
exercise builds the adapters that make that concrete, so a legacy library that speaks bare done
channels can drive a modern codebase that threads `Context` everywhere, and vice versa.

## What you'll build

```text
donebridge/                        independent module: example.com/donebridge
  go.mod
  bridge.go                        DoneFromContext, ContextFromDone, OnDone (context.AfterFunc)
  cmd/
    demo/
      main.go                      runnable demo: close a bare done, watch a derived context cancel
  bridge_test.go                   done<-ctx, ctx<-done, AfterFunc stop, no-leak; -race
```

Files: `bridge.go`, `cmd/demo/main.go`, `bridge_test.go`.
Implement: `DoneFromContext(ctx) <-chan struct{}` returning `ctx.Done()`; `ContextFromDone(parent, done) (context.Context, context.CancelFunc)` that cancels a derived context when the bare `done` closes; `OnDone(ctx, f) (stop func() bool)` wrapping `context.AfterFunc`.
Test: cancelling a context closes the channel from `DoneFromContext`; closing a bare `done` cancels the derived context with `context.Canceled`; `OnDone`'s stop prevents the callback; the bridge watcher does not leak on the not-cancelled path.
Verify: `go test -count=1 -race ./...`

### The two directions, and the stdlib's own bridge

`DoneFromContext` is a one-liner — `return ctx.Done()` — and that is the entire point: a context already
*is* a done channel, so exposing its cancellation to done-based code needs no machinery. Any code that
takes `done <-chan struct{}` can be handed `ctx.Done()` unchanged.

`ContextFromDone` is the interesting direction: given a bare `done` channel from some legacy component,
derive a cancellable `context.Context` that cancels when `done` closes. There is no stdlib call that
turns a channel into a context, so a small watcher goroutine selects on `done` and on the derived
context's own `Done()`; whichever fires first, the goroutine exits. If `done` closes, it calls `cancel`,
which cancels the context — so `ctx.Err()` becomes `context.Canceled`. The watcher also exits when the
caller invokes the returned `cancel` (which closes `ctx.Done()`), so it never leaks: on every path,
either `done` closes or the context is cancelled, and both unblock the select.

`OnDone` wraps `context.AfterFunc`, the stdlib's own bridge from a context's done channel to a callback.
`context.AfterFunc(ctx, f)` runs `f` in a new goroutine once `ctx` is done, and returns a `stop func() bool`
that unregisters `f`; `stop` returns true if the call prevented `f` from running. That return value is the
detail worth internalizing — it lets you cancel the registration on the not-cancelled path so nothing
lingers.

Create `bridge.go`:

```go
package donebridge

import "context"

// DoneFromContext exposes a context's cancellation as a bare done channel.
// A context already is a done channel, so this simply returns ctx.Done().
func DoneFromContext(ctx context.Context) <-chan struct{} {
	return ctx.Done()
}

// ContextFromDone derives a cancellable context from parent that is cancelled
// when the bare done channel closes. The returned cancel stops the derived
// context (and the watcher goroutine); call it to release resources.
func ContextFromDone(parent context.Context, done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel, _ := contextFromDone(parent, done)
	return ctx, cancel
}

// contextFromDone is ContextFromDone with an extra channel that closes when the
// watcher goroutine exits, so tests can prove the goroutine does not leak.
func contextFromDone(parent context.Context, done <-chan struct{}) (context.Context, context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel, stopped
}

// OnDone runs f in its own goroutine when ctx is cancelled or its deadline
// passes, returning a stop that unregisters f (stop reports true if it
// prevented f from running). It is a thin wrapper over context.AfterFunc,
// shown here to make explicit that the stdlib already bridges a context's
// done channel to a callback.
func OnDone(ctx context.Context, f func()) (stop func() bool) {
	return context.AfterFunc(ctx, f)
}
```

### The runnable demo

Legacy code closes a bare `done`; modern code observes the derived context cancel with a reason.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/donebridge"
)

func main() {
	done := make(chan struct{})
	ctx, cancel := donebridge.ContextFromDone(context.Background(), done)
	defer cancel()

	// A legacy component signals cancellation the old way.
	close(done)

	// Modern, context-based code observes it — with a reason.
	<-ctx.Done()
	fmt.Println("ctx cancelled:", ctx.Err())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
ctx cancelled: context canceled
```

### Tests

`TestDoneFromContextClosesOnCancel` cancels a context and asserts the channel from `DoneFromContext`
closes. `TestContextFromDoneCancels` closes a bare `done` and asserts the derived context's `Done()`
closes with `Err() == context.Canceled`. `TestOnDoneStopPreventsFire` registers a callback, stops it
before cancelling, and asserts the callback never runs and `stop` reported true. `TestNoGoroutineLeak`
uses the internal `contextFromDone` to prove the watcher exits on the not-cancelled path — when the caller
calls `cancel` and `done` never closes.

Create `bridge_test.go`:

```go
package donebridge

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDoneFromContextClosesOnCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	d := DoneFromContext(ctx)
	cancel()

	select {
	case <-d:
	case <-time.After(2 * time.Second):
		t.Fatal("done channel from context did not close after cancel")
	}
}

func TestContextFromDoneCancels(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	ctx, cancel := ContextFromDone(context.Background(), done)
	defer cancel()

	close(done)

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("derived context did not cancel after done closed")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

func TestOnDoneStopPreventsFire(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	fired := make(chan struct{})
	stop := OnDone(ctx, func() { close(fired) })

	if !stop() {
		t.Fatal("stop() = false, want true (it should have prevented the callback)")
	}
	cancel()

	select {
	case <-fired:
		t.Fatal("callback ran despite stop()")
	case <-time.After(50 * time.Millisecond):
		// good: the callback was unregistered before cancel
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	t.Parallel()

	done := make(chan struct{}) // never closed
	_, cancel, stopped := contextFromDone(context.Background(), done)

	cancel() // the not-cancelled-via-done path

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher goroutine leaked after cancel")
	}
}

func ExampleContextFromDone() {
	done := make(chan struct{})
	ctx, cancel := ContextFromDone(context.Background(), done)
	defer cancel()

	close(done)
	<-ctx.Done()
	fmt.Println(ctx.Err())
	// Output: context canceled
}
```

## Review

The adapters are correct when cancellation crosses the boundary in both directions: a cancelled context
closes the channel `DoneFromContext` returns, and a closed bare `done` cancels the context
`ContextFromDone` derives, with `Err()` reporting `context.Canceled`. The no-leak test is the one that
proves the bridge is production-safe: the watcher goroutine must exit even when `done` never closes, which
it does because calling `cancel` cancels the derived context and unblocks the watcher's select. `OnDone`
is the stdlib's own version of this bridge — `context.AfterFunc` turns a context's done channel into a
callback and hands back a `stop` you must be able to call on the happy path. Run `go test -race` to confirm
the watcher's `cancel` call and the caller's reads do not race. The lesson to carry forward: done channels
and `context.Context` are one primitive, so bridge them explicitly rather than building parallel
cancellation plumbing.

## Resources

- [pkg.go.dev: context (WithCancel, Context.Done, AfterFunc)](https://pkg.go.dev/context)
- [pkg.go.dev: context.AfterFunc](https://pkg.go.dev/context#AfterFunc)
- [Go Blog: Go Concurrency Patterns — Pipelines and cancellation](https://go.dev/blog/pipelines)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [09-ticker-poller-stop-on-done.md](09-ticker-poller-stop-on-done.md) | Next: [11-rolling-deploy-health-gate-preempt.md](11-rolling-deploy-health-gate-preempt.md)
