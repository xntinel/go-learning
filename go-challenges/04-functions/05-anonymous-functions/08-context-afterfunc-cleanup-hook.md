# Exercise 8: Cancellation Cleanup Registered as an Anonymous Function via context.AfterFunc

When a resource's lifetime is tied to a request context — a lease, an open stream,
a gauge you incremented — you want cleanup to run when the context is cancelled, but
*not* twice if the request also completes normally. `context.AfterFunc(ctx, func(){
... })` registers an anonymous cleanup to run on cancellation and hands back a
`stop func() bool` to deregister it on the normal path. This module builds that
exactly-once lease and pins down the stop-versus-run race.

This module is fully self-contained. Nothing here imports another exercise.

## What you'll build

```text
lease/                        module example.com/lease
  go.mod
  lease.go                    Lease tied to a context; AfterFunc cleanup + stop; exactly-once release
  lease_test.go               cancel runs cleanup, normal-path stop==true, stop-after-cancel==false
  cmd/demo/main.go            normal-path and cancellation-path leases
```

- Files: `lease.go`, `lease_test.go`, `cmd/demo/main.go`.
- Implement: `Acquire(ctx, gauge)` registering an anonymous cleanup with `context.AfterFunc`, a `Release` that calls `stop()` and runs the cleanup, and a `sync.Once` guard so cleanup runs exactly once from whichever path wins.
- Test: cancelling the context runs the cleanup (synchronize on a channel — `AfterFunc` does not wait); on the normal path `Release` returns true and cleanup still ran once; `stop()` after cancellation returns false, and the gauge is never double-decremented.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/05-anonymous-functions/08-context-afterfunc-cleanup-hook/cmd/demo
cd go-solutions/04-functions/05-anonymous-functions/08-context-afterfunc-cleanup-hook
```

### AfterFunc registers a cleanup; stop deregisters it

`context.AfterFunc(ctx, f)` starts `f` in its own goroutine when `ctx` becomes done,
and returns `stop func() bool`. Calling `stop()` returns `true` if it deregistered
`f` before it started (cleanup will not run via the context) and `false` if `f` had
already been started or already stopped. Two facts drive the design. First,
`AfterFunc` does *not* wait for `f` to finish — so any test or shutdown path that
needs to observe the cleanup must synchronize explicitly; here the cleanup closes a
`done` channel that callers can wait on. Second, cleanup can be triggered from two
racing paths — cancellation and the normal `Release` — so it must be idempotent; a
`sync.Once` runs the decrement exactly once no matter which path wins, and an
`atomic.Bool` records that it happened.

`Release` on the normal path calls `stop()` to deregister the context hook, then
runs the cleanup itself (idempotent via the `Once`). Its boolean return echoes
`stop()`: true means the normal path won the race and prevented the context hook
from running; false means cancellation had already begun the cleanup.

Create `lease.go`:

```go
package lease

import (
	"context"
	"sync"
	"sync/atomic"
)

// Lease is a held resource tied to a context. Its cleanup runs exactly once,
// triggered by either context cancellation or an explicit Release.
type Lease struct {
	stop     func() bool
	once     sync.Once
	released atomic.Bool
	gauge    *atomic.Int64
	done     chan struct{}
}

// Acquire takes the resource, increments gauge, and registers an anonymous
// cleanup to run when ctx is done. Release deregisters that cleanup.
func Acquire(ctx context.Context, gauge *atomic.Int64) *Lease {
	gauge.Add(1)
	l := &Lease{gauge: gauge, done: make(chan struct{})}
	l.stop = context.AfterFunc(ctx, l.release)
	return l
}

// release is the idempotent cleanup: decrement the gauge once and signal done.
func (l *Lease) release() {
	l.once.Do(func() {
		l.gauge.Add(-1)
		l.released.Store(true)
		close(l.done)
	})
}

// Release runs cleanup on the normal path and deregisters the context hook. It
// returns true if it prevented the context hook from running.
func (l *Lease) Release() bool {
	stopped := l.stop()
	l.release()
	return stopped
}

// Released reports whether cleanup has run.
func (l *Lease) Released() bool { return l.released.Load() }

// Done is closed once cleanup has run; callers wait on it because AfterFunc does
// not block until the cleanup completes.
func (l *Lease) Done() <-chan struct{} { return l.done }
```

### The runnable demo

The demo shows both paths: one lease released on the normal path (stop wins), and
one released by cancelling its context (the `AfterFunc` cleanup fires, and we wait
on `Done` because it does not block).

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"sync/atomic"

	"example.com/lease"
)

func main() {
	var gauge atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())
	l := lease.Acquire(ctx, &gauge)
	fmt.Println("active leases:", gauge.Load())
	fmt.Println("stop prevented hook:", l.Release())
	fmt.Println("active leases:", gauge.Load())
	cancel() // no effect: hook was deregistered

	ctx2, cancel2 := context.WithCancel(context.Background())
	l2 := lease.Acquire(ctx2, &gauge)
	fmt.Println("active leases:", gauge.Load())
	cancel2()
	<-l2.Done()
	fmt.Println("released after cancel:", l2.Released())
	fmt.Println("active leases:", gauge.Load())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
active leases: 1
stop prevented hook: true
active leases: 0
active leases: 1
released after cancel: true
active leases: 0
```

### Tests

`TestCancelRunsCleanup` cancels the context and waits on `Done` — because
`AfterFunc` does not block — then asserts the lease released and the gauge returned
to zero. `TestNormalPathStops` asserts `Release` returns true on the normal path and
that a later cancel does not double-decrement. `TestStopAfterCancelIsFalse` cancels
first, waits for cleanup, then asserts `Release` returns false and the gauge is
still not double-decremented.

Create `lease_test.go`:

```go
package lease

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
)

func TestCancelRunsCleanup(t *testing.T) {
	t.Parallel()
	var gauge atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	l := Acquire(ctx, &gauge)
	if gauge.Load() != 1 {
		t.Fatalf("gauge = %d after Acquire, want 1", gauge.Load())
	}

	cancel()
	<-l.Done() // AfterFunc does not wait; synchronize explicitly

	if !l.Released() {
		t.Fatal("cleanup did not run after cancellation")
	}
	if gauge.Load() != 0 {
		t.Fatalf("gauge = %d after cleanup, want 0", gauge.Load())
	}
}

func TestNormalPathStops(t *testing.T) {
	t.Parallel()
	var gauge atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := Acquire(ctx, &gauge)

	if !l.Release() {
		t.Fatal("Release on the normal path did not prevent the context hook")
	}
	if !l.Released() || gauge.Load() != 0 {
		t.Fatalf("after Release: released=%v gauge=%d, want true and 0", l.Released(), gauge.Load())
	}

	cancel()
	if gauge.Load() != 0 {
		t.Fatalf("gauge double-decremented after cancel: %d", gauge.Load())
	}
}

func TestStopAfterCancelIsFalse(t *testing.T) {
	t.Parallel()
	var gauge atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	l := Acquire(ctx, &gauge)

	cancel()
	<-l.Done()

	if l.Release() {
		t.Fatal("Release after cancellation returned true, want false")
	}
	if gauge.Load() != 0 {
		t.Fatalf("gauge = %d, want 0 (no double decrement)", gauge.Load())
	}
}

func ExampleLease_Release() {
	var gauge atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := Acquire(ctx, &gauge)
	fmt.Println(l.Release(), l.Released())
	// Output: true true
}
```

## Review

The lease is correct when cleanup runs exactly once regardless of which path wins.
`TestCancelRunsCleanup` proves the `AfterFunc` literal fires on cancellation — and
must wait on `Done` first, because `AfterFunc` returns without waiting for the
cleanup, the single most common source of flaky cleanup tests. `TestNormalPathStops`
proves `Release` deregisters the hook (returns true) and that a subsequent cancel
does not double-decrement the gauge, which the `sync.Once` guarantees.
`TestStopAfterCancelIsFalse` proves the losing path sees `stop()==false`. Ignoring
the `stop` return — the mistake the concepts flag — would let cleanup run on both
paths, decrementing the gauge twice; the `Once` plus the `stop` call together make
the release idempotent and single.

## Resources

- [context.AfterFunc](https://pkg.go.dev/context#AfterFunc)
- [context.WithCancel](https://pkg.go.dev/context#WithCancel)
- [sync.Once](https://pkg.go.dev/sync#Once)
- [sync/atomic](https://pkg.go.dev/sync/atomic)

---

Back to [00-concepts.md](00-concepts.md) | Previous: [07-transaction-callback-runner.md](07-transaction-callback-runner.md) | Next: [09-shared-capture-race-fanout.md](09-shared-capture-race-fanout.md)
