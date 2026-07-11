# Exercise 23: Context Deadline Enforcer Using context.AfterFunc Cleanup

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

Enforcing a deadline means running an escalation action — aborting a request,
freeing a reserved slot — exactly when the deadline passes, but never when the
guarded operation already finished on time. `context.AfterFunc(ctx, func(){ ... })`
registers that escalation as an anonymous callback and hands back a `stop func()
bool` so the winning path can be told apart from the losing one. This module builds
that enforcer and proves the race between "operation finished" and "deadline
arrived" resolves correctly either way.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
deadline/                     module example.com/deadline
  go.mod
  deadline.go                   Enforcer, Enforce (AfterFunc + stop), Complete, TimedOut, Done
  deadline_test.go               complete-before-deadline, deadline-fires-once, complete-after-deadline
  cmd/demo/main.go              one on-time path, one deadline path
```

- Files: `deadline.go`, `deadline_test.go`, `cmd/demo/main.go`.
- Implement: `Enforce(ctx, onTimeout)` registering an anonymous timeout action via `context.AfterFunc`, guarded by `sync.Once` so it runs at most once; `Complete()` calling `stop()` and reporting whether it won the race; `TimedOut()` and `Done()`.
- Test: completing before the (simulated) deadline prevents the timeout action and later cancellation has no further effect; the deadline arriving runs the timeout action exactly once; completing after the deadline already fired returns false and does not re-run the action. Under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/deadline/cmd/demo
cd ~/go-exercises/deadline
go mod init example.com/deadline
go mod edit -go=1.24
```

### `stop()`'s boolean is the whole race

`Enforce` registers `onTimeout` with `context.AfterFunc`, which starts it in its own
goroutine the moment `ctx` is done, and returns `stop func() bool`. `stop()` reports
true if it deregistered the callback *before* it started — meaning the guarded
operation finished first and the deadline never mattered — and false if the callback
had already started or already finished, meaning the deadline won. `Complete` is the
"operation finished" path: it calls `stop()` and returns exactly what `stop()`
reported, so a caller can tell which side of the race it was on.

Both the `AfterFunc` callback and `Complete` can end up running the escalation logic
— whichever happens first — so the actual work inside the `AfterFunc` callback is
wrapped in a `sync.Once`, and `Complete` closes the same `done` channel through that
same `Once` guard rather than assuming it "won." In this module `ctx` is accepted as
a plain `context.Context` so tests can simulate the deadline arriving by calling
`cancel()` themselves — deterministically — instead of racing a real timer; in
production, `ctx` would come from `context.WithDeadline` or `context.WithTimeout`,
and the same `stop()`/`Once` interaction applies unchanged.

Create `deadline.go`:

```go
package deadline

import (
	"context"
	"sync"
	"sync/atomic"
)

// Enforcer ties a timeout action to a context's lifetime. In production ctx
// comes from context.WithDeadline or context.WithTimeout; here it is
// accepted as a plain context.Context so tests can simulate the deadline
// arriving deterministically by calling cancel() themselves, instead of
// racing a real timer.
type Enforcer struct {
	stop     func() bool
	once     sync.Once
	timedOut atomic.Bool
	done     chan struct{}
}

// Enforce registers onTimeout — an anonymous escalation action such as
// aborting a request or freeing a slot — to run via context.AfterFunc when
// ctx's deadline passes. It returns an *Enforcer whose Complete method must
// be called if the guarded operation finishes before the deadline, so the
// escalation never fires for work that already succeeded.
func Enforce(ctx context.Context, onTimeout func()) *Enforcer {
	e := &Enforcer{done: make(chan struct{})}
	e.stop = context.AfterFunc(ctx, func() {
		e.once.Do(func() {
			e.timedOut.Store(true)
			onTimeout()
			close(e.done)
		})
	})
	return e
}

// Complete reports that the guarded operation finished before the
// deadline. It calls Stop() on the AfterFunc registration first: Stop
// returns true if it deregistered the timeout callback before the callback
// started (the operation won the race) and false if the callback had
// already started or already run (the deadline won). Either way, Complete
// makes sure Done() closes exactly once.
func (e *Enforcer) Complete() bool {
	stopped := e.stop()
	e.once.Do(func() { close(e.done) })
	return stopped
}

// TimedOut reports whether the deadline action ran.
func (e *Enforcer) TimedOut() bool { return e.timedOut.Load() }

// Done is closed once the race between Complete and the deadline callback
// is resolved. context.AfterFunc does not wait for its callback to finish,
// so callers that need to observe the outcome must wait on Done.
func (e *Enforcer) Done() <-chan struct{} { return e.done }
```

### The runnable demo

The demo shows both paths: one enforcer completed before its context is ever
cancelled (it wins the race), and one whose context is cancelled to simulate a
deadline arriving, waiting on `Done` because `AfterFunc` does not block.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/deadline"
)

func main() {
	// Path 1: the operation finishes before the deadline. In production
	// ctx would come from context.WithDeadline; here we simulate the
	// deadline never arriving by simply not cancelling.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	e1 := deadline.Enforce(ctx1, func() { fmt.Println("escalation: should not print") })
	fmt.Println("stopped in time:", e1.Complete())
	fmt.Println("timed out:", e1.TimedOut())

	// Path 2: the deadline arrives first (simulated via cancel).
	ctx2, cancel2 := context.WithCancel(context.Background())
	e2 := deadline.Enforce(ctx2, func() { fmt.Println("escalation: timeout action ran") })
	cancel2()
	<-e2.Done()
	fmt.Println("timed out:", e2.TimedOut())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
stopped in time: true
timed out: false
escalation: timeout action ran
timed out: true
```

### Tests

`TestCompleteBeforeDeadlinePreventsTimeout` completes first and checks the timeout
action never runs, even after a later cancellation. `TestDeadlineArrivingRunsTimeoutExactlyOnce`
cancels first, waits on `Done` — because `AfterFunc` does not block until its
callback finishes — and checks the timeout action ran exactly once.
`TestCompleteAfterDeadlineReturnsFalse` cancels first, waits for the timeout to run,
then checks `Complete` reports it lost the race and the action did not run twice.

Create `deadline_test.go`:

```go
package deadline

import (
	"context"
	"fmt"
	"testing"
)

func TestCompleteBeforeDeadlinePreventsTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fired := false
	e := Enforce(ctx, func() { fired = true })

	if stopped := e.Complete(); !stopped {
		t.Fatal("Complete() = false, want true (operation won the race)")
	}
	if e.TimedOut() {
		t.Fatal("TimedOut() = true, want false")
	}

	cancel() // must not fire onTimeout: the hook was already deregistered
	if fired {
		t.Fatal("onTimeout ran after Complete had already stopped it")
	}
}

func TestDeadlineArrivingRunsTimeoutExactlyOnce(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	count := 0
	e := Enforce(ctx, func() { count++ })

	cancel()
	<-e.Done() // AfterFunc does not wait; synchronize explicitly

	if !e.TimedOut() {
		t.Fatal("TimedOut() = false, want true")
	}
	if count != 1 {
		t.Fatalf("onTimeout ran %d times, want exactly 1", count)
	}
}

func TestCompleteAfterDeadlineReturnsFalse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	count := 0
	e := Enforce(ctx, func() { count++ })

	cancel()
	<-e.Done()

	if stopped := e.Complete(); stopped {
		t.Fatal("Complete() = true after the deadline already fired, want false")
	}
	if count != 1 {
		t.Fatalf("onTimeout ran %d times, want exactly 1 (no double invocation)", count)
	}
}

func ExampleEnforce() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := Enforce(ctx, func() {})
	fmt.Println(e.Complete(), e.TimedOut())
	// Output: true false
}
```

## Review

The enforcer is correct when the timeout action runs at most once regardless of
which side wins: `TestCompleteBeforeDeadlinePreventsTimeout` proves the operation's
own completion can permanently disarm the deadline callback, and
`TestCompleteAfterDeadlineReturnsFalse` proves the reverse — a late `Complete` cannot
undo or repeat an escalation that already happened. The `sync.Once` inside the
`AfterFunc` callback is what makes that safe even though both `Complete` and the
callback could, in principle, run concurrently; without it, a `Complete` racing the
callback's start could see stale state and the escalation could run twice. Waiting
on `Done()` in the tests — rather than asserting immediately after `cancel()` — is
required because `context.AfterFunc` starts its callback in a new goroutine and does
not wait for it, exactly as with `context.AfterFunc` used for any other cleanup.

## Resources

- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc)
- [context.WithDeadline](https://pkg.go.dev/context#WithDeadline)
- [sync.Once](https://pkg.go.dev/sync#Once)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-stream-buffer-iife.md](22-stream-buffer-iife.md) | Next: [24-tenant-worker-partition.md](24-tenant-worker-partition.md)
