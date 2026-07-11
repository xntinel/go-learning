# Exercise 7: AfterFunc — Hang Cleanup and Abort Metrics off Cancellation

When a request holds a resource — an advisory database lock, a leased slot, a
reserved connection — that resource must be released if the request is cancelled
before it finishes. The old way to arrange that was a babysitter goroutine:
`go func() { <-ctx.Done(); release() }()`. `context.AfterFunc` (Go 1.21) replaces
that whole pattern: it schedules a function to run on cancellation and returns a
`stop` you call on the success path so the cleanup does not double-fire.

This module is fully self-contained: its own `go mod init`, package, demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
lease/                     independent module: example.com/lease
  go.mod                   module example.com/lease
  lease.go                 Lease; Acquire(ctx, release, onAbort); (*Lease).Done()
  cmd/
    demo/
      main.go              normal completion vs abort; prints each path
  lease_test.go            releases-on-cancel, stop-prevents-hook, stop-after-fire
```

- Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
- Implement: `Acquire(ctx, release, onAbort)` that registers an `AfterFunc` to release the resource and emit an abort signal on cancel, and `Done()` that on normal completion calls the `stop` func and releases explicitly.
- Test: a cancel runs the release and calls `onAbort` with `context.Cause`; normal completion's `Done()` returns `true`, the abort hook never fires, and release happens exactly once; a `Done()` after the cancel already fired returns `false`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p lease/cmd/demo
cd lease
go mod init example.com/lease
```

### Why AfterFunc replaces the babysitter goroutine

The hand-rolled version of "release this on cancel" is a goroutine whose entire
job is to block on `<-ctx.Done()` and then run cleanup. It works, but it is a
goroutine per lease, it needs its own coordination to avoid releasing twice when
the work also finishes normally, and it is easy to leak if you forget to make it
exit on the success path.

`context.AfterFunc(ctx, f)` collapses all of that. It arranges for `f` to run —
on its own goroutine, managed by the runtime — when `ctx` is cancelled, and
returns a `stop func() bool`. Two facts define its contract. First, `f` runs at
most once, only if the context is cancelled. Second, `stop()` returns `true` if it
prevented `f` from running (the context had not been cancelled and `f` had not
started) and `false` otherwise (the context was already cancelled and `f` already
launched, or `stop` was already called). That boolean is exactly what the success
path needs: call `stop()`, and if it returns `true` you know the abort hook will
never fire and you own the release yourself.

The design here wires both paths to the *same* release, guarded by a `sync.Once`,
so the resource is freed exactly once no matter which path wins. On cancel, the
`AfterFunc` body calls `onAbort(context.Cause(ctx))` — capturing the diagnosable
reason, not the opaque `context.Canceled` — and then releases. On normal
completion, `Done()` calls `stop()` (cancelling the hook) and then releases. If a
cancel and a `Done()` race, the `Once` ensures the release runs a single time and
the second caller's release is a no-op. `onAbort` reads `context.Cause`, so a
caller who cancels with `context.WithCancelCause(parent)` and a custom cause gets
that cause in the metric.

Create `lease.go`:

```go
package lease

import (
	"context"
	"sync"
)

// Lease ties a released-once resource to a context's cancellation. On cancel it
// runs release and reports the cause via onAbort; on normal completion Done
// cancels that hook and releases explicitly. release runs exactly once.
type Lease struct {
	stop func() bool
	do   func()
	once sync.Once
}

// Acquire registers a cancellation hook on ctx. If ctx is cancelled before Done
// is called, onAbort is invoked with context.Cause(ctx) and release runs. The
// returned Lease's Done reports normal completion.
func Acquire(ctx context.Context, release func(), onAbort func(cause error)) *Lease {
	l := &Lease{}
	l.do = func() { l.once.Do(release) }
	l.stop = context.AfterFunc(ctx, func() {
		onAbort(context.Cause(ctx))
		l.do()
	})
	return l
}

// Done marks the work complete: it stops the abort hook and releases the
// resource. It returns true if it prevented the abort hook from running (the
// normal path) and false if the hook had already fired.
func (l *Lease) Done() bool {
	prevented := l.stop()
	l.do()
	return prevented
}
```

### The runnable demo

The demo shows both paths: a lease completed normally (the hook is prevented) and
a lease whose context is cancelled with a cause (the hook fires and reports the
cause). Each path prints deterministically because `main` synchronizes on the
release.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"example.com/lease"
)

func main() {
	// Normal completion: Done() prevents the abort hook.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	var releases1 atomic.Int64
	l1 := lease.Acquire(ctx1,
		func() { releases1.Add(1) },
		func(error) { fmt.Println("abort hook fired (normal path)") })
	prevented := l1.Done()
	fmt.Printf("normal path: prevented=%v releases=%d\n", prevented, releases1.Load())

	// Abort: cancelling with a cause fires the hook.
	errRequestCancelled := errors.New("request cancelled")
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	var releases2 atomic.Int64
	released := make(chan struct{})
	lease.Acquire(ctx2,
		func() { releases2.Add(1); close(released) },
		func(cause error) { fmt.Println("abort hook fired, cause:", cause) })
	cancel2(errRequestCancelled)
	<-released
	fmt.Printf("abort path: releases=%d\n", releases2.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
normal path: prevented=true releases=1
abort hook fired, cause: request cancelled
abort path: releases=1
```

### Tests

`TestReleasesOnCancel` cancels with a custom cause and asserts the release ran and
`onAbort` saw that cause. `TestStopPreventsHookOnSuccess` completes normally,
asserts `Done()` returned `true`, then cancels and confirms the hook never fired
and release happened exactly once. `TestStopAfterFire` cancels first, waits for the
hook, then asserts a late `Done()` returns `false` with release still single. The
release counter is an `atomic.Int64` because the hook runs on its own goroutine.

Create `lease_test.go`:

```go
package lease

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestReleasesOnCancel(t *testing.T) {
	t.Parallel()

	errShutdown := errors.New("shutting down")
	var releases atomic.Int64
	released := make(chan struct{}, 1)
	gotCause := make(chan error, 1)

	ctx, cancel := context.WithCancelCause(context.Background())
	Acquire(ctx,
		func() { releases.Add(1); released <- struct{}{} },
		func(cause error) { gotCause <- cause })

	cancel(errShutdown)

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("release did not run within 1s of cancel")
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("releases = %d, want 1", got)
	}
	select {
	case cause := <-gotCause:
		if !errors.Is(cause, errShutdown) {
			t.Fatalf("onAbort cause = %v, want errShutdown", cause)
		}
	case <-time.After(time.Second):
		t.Fatal("onAbort was not called")
	}
}

func TestStopPreventsHookOnSuccess(t *testing.T) {
	t.Parallel()

	var releases atomic.Int64
	abortFired := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	l := Acquire(ctx,
		func() { releases.Add(1) },
		func(error) { abortFired <- struct{}{} })

	if prevented := l.Done(); !prevented {
		t.Fatal("Done() = false, want true (hook should have been prevented)")
	}

	cancel() // too late: the hook was already stopped

	select {
	case <-abortFired:
		t.Fatal("abort hook fired after Done() prevented it")
	case <-time.After(50 * time.Millisecond):
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("releases = %d, want exactly 1", got)
	}
}

func TestStopAfterFire(t *testing.T) {
	t.Parallel()

	var releases atomic.Int64
	released := make(chan struct{}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	l := Acquire(ctx,
		func() { releases.Add(1); released <- struct{}{} },
		func(error) {})

	cancel()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("release did not run within 1s of cancel")
	}

	if prevented := l.Done(); prevented {
		t.Fatal("Done() = true after cancel already fired, want false")
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("releases = %d, want exactly 1", got)
	}
}
```

## Review

The hook is correct when release runs exactly once on either path and `Done()`'s
boolean tells the truth: `true` when it prevented the abort hook (normal
completion), `false` when the hook had already fired (cancel won the race). The
`sync.Once` around release is what makes a cancel-versus-`Done` race safe — drop
it and a job that finishes just as its context is cancelled releases twice. Read
`context.Cause(ctx)` in the hook, not `ctx.Err()`, so the abort metric records the
real reason. Because the hook runs on its own goroutine, every piece of state it
touches must be concurrency-safe (hence the `atomic.Int64` and buffered channels)
— run `go test -race` to prove it. Resist the urge to reintroduce the babysitter
goroutine; `AfterFunc` plus its `stop` is the whole mechanism.

## Resources

- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc) — the scheduled function and its `stop func() bool`.
- [context.Cause](https://pkg.go.dev/context#Cause) — the reason read inside the hook.
- [sync.Once](https://pkg.go.dev/sync#Once) — releasing the resource exactly once across both paths.

---

Back to [00-concepts.md](00-concepts.md) | Next: [08-detached-background-write.md](08-detached-background-write.md)
