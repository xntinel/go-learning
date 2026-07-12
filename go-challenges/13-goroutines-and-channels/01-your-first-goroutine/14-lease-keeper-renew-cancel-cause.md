# Exercise 14: Distributed Lock Lease Keeper: One Renewer Goroutine, Cancel-Cause On Lease Loss

**Level: Advanced**

Holding a distributed lock (etcd, Consul, ZooKeeper, a Redis lease) means a
background goroutine must renew the lease on every tick for as long as you hold
it, and the holder must learn -- precisely and once -- if a renewal ever fails so
it can abort the critical section instead of acting on a lock it no longer owns.
The naive version leaks that keeper on the unhappy paths and gives the holder no
crisp signal of *why* it lost the lock. This exercise builds `Acquire`, which
launches exactly one renewer goroutine and hands back a `leaseCtx` that is
cancelled with cause `ErrLeaseLost` the instant a renew fails, plus a `release`
that stops the renewer and joins it before returning so no keeper outlives the
lock.

This module is self-contained: its own module, a `lease` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
lease/                       independent module: example.com/lease
  go.mod                     go 1.26
  lease.go                   ErrLeaseLost; Acquire(parent, ticks, renew) (leaseCtx, release)
  cmd/demo/main.go           runnable demo: two good renews, then a lost lease with its cause
  lease_test.go              alive-on-success, cause-on-failure, stop-after-failure, parent-cause, join+idempotent, once
```

- Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
- Implement: `var ErrLeaseLost error` and `Acquire(parent context.Context, ticks <-chan time.Time, renew func(ctx context.Context) error) (leaseCtx context.Context, release func())`.
- Test: success keeps `leaseCtx` alive and renews once per tick; the first renew failure cancels `leaseCtx` with cause `ErrLeaseLost`; no renew runs after the failure; a parent cancel propagates the *parent's* cause; `release` joins the keeper and is idempotent; the cause is delivered exactly once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak
go mod tidy
```

### One keeper, one owner, one cause

The three questions you ask before any `go` statement -- who joins it, how does it
terminate, what does it report -- all have sharp answers here, and getting them
right is the whole exercise.

*Who joins it.* `Acquire` launches exactly one goroutine. `release` is the join:
it closes a `stop` channel and then blocks on a `done` channel the keeper closes
as it returns. Because `release` waits for `done`, no keeper can outlive the lock
-- when `release` returns, the goroutine is gone. `release` wraps this in a
`sync.Once`, so calling it twice, or calling it after the lease was already lost,
neither double-closes `stop` nor panics: the second call is a no-op.

*How does it terminate.* The keeper's `select` has three arms, and every one of
them returns: `leaseCtx.Done()` (we cancelled on failure, or the parent was
cancelled), `stop` (release asked it to quit), and a receive from `ticks`. A
closed `ticks` returns without touching the lease. There is no arm without an
exit, so the goroutine cannot leak.

*What does it report.* The lease context is built with
`context.WithCancelCause(parent)`. On the first renew failure the keeper calls
`cancel(fmt.Errorf("%w: %w", ErrLeaseLost, err))` and returns immediately -- this
is the stop-after-first-failure invariant: once the lease is declared lost, no
further renew is attempted, ever. The holder observes the loss with
`<-leaseCtx.Done()` and reads the reason with `context.Cause(leaseCtx)`, which
`errors.Is(..., ErrLeaseLost)` matches. Context guarantees the cause is set
exactly once: the first `cancel` wins and every later `cancel` is ignored, so the
holder never sees a torn or double-reported reason.

Two subtleties make it correct under a real scheduler. First, a ready tick can
win the `select` even when `leaseCtx` is already done -- Go picks a ready arm at
random -- so after receiving a tick the keeper re-checks `ctx.Err()` and returns
without renewing if the lease is already gone. That is what stops a parent cancel
from racing one extra renew through. Second, when the parent is cancelled the
derived `leaseCtx` is cancelled *with the parent's cause*, not `ErrLeaseLost`: a
request abort and a lost lease are different failures, and the holder must be able
to tell them apart.

Create `lease.go`:

```go
package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrLeaseLost is the cancellation cause reported through leaseCtx when a renew
// fails. Callers detect it with errors.Is(context.Cause(leaseCtx), ErrLeaseLost).
var ErrLeaseLost = errors.New("lease lost")

// Acquire launches exactly one background renewer goroutine and returns a
// leaseCtx that stays alive while the lease is held. On each receive from ticks
// the renewer calls renew(leaseCtx). The FIRST renew error cancels leaseCtx with
// cause ErrLeaseLost (wrapping the renew error) and stops the renewer: no further
// renew is attempted. If parent is cancelled, leaseCtx is cancelled with the
// parent's cause (not ErrLeaseLost) and the renewer stops. If ticks is closed the
// renewer simply stops without touching leaseCtx.
//
// release stops the renewer and JOINS it before returning, so no keeper outlives
// the lock. release is idempotent and safe to call after the lease is already
// lost. ticks is injected so no real clock is needed.
func Acquire(parent context.Context, ticks <-chan time.Time, renew func(ctx context.Context) error) (leaseCtx context.Context, release func()) {
	ctx, cancel := context.WithCancelCause(parent)

	stop := make(chan struct{}) // closed by release to ask the renewer to stop
	done := make(chan struct{}) // closed by the renewer as it returns; the join point

	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				// Parent cancelled, or we already cancelled on failure/release.
				return
			case <-stop:
				return
			case _, ok := <-ticks:
				if !ok {
					return // ticks closed: stop the keeper, leave the lease as is
				}
				// Re-check after selecting: a ready tick can win the select even
				// when the lease is already gone. Never renew a lost lease.
				if ctx.Err() != nil {
					return
				}
				if err := renew(ctx); err != nil {
					cancel(fmt.Errorf("%w: %w", ErrLeaseLost, err))
					return // stop-after-first-failure: no renew is called again
				}
			}
		}
	}()

	var once sync.Once
	release = func() {
		once.Do(func() {
			close(stop) // ask the renewer to stop
			<-done      // JOIN: no keeper outlives the lock
			cancel(nil) // idempotent; releases the context's resources
		})
	}
	return ctx, release
}
```

### The runnable demo

The demo injects a buffered `ticks` channel and a `renew` that fails on its third
call, standing in for a lock backend that drops the lease. The keeper announces
each renew on a channel so the output is fully ordered: two good renews with the
lease alive, then the failing tick that cancels the lease, its cause read back,
and a clean join.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/lease"
)

func main() {
	// Injected ticks and a renew that fails on its third call, standing in for a
	// distributed lock backend (etcd/Consul) that drops the lease. Only the
	// renewer goroutine ever touches n, so no synchronization is needed on it.
	ticks := make(chan time.Time, 8)
	renewed := make(chan struct{}, 8) // renew announces each call so the demo stays ordered
	n := 0
	renew := func(ctx context.Context) error {
		n++
		defer func() { renewed <- struct{}{} }()
		if n == 3 {
			return errors.New("etcd: lease not found")
		}
		return nil
	}

	ctx, release := lease.Acquire(context.Background(), ticks, renew)
	defer release() // idempotent: safe even though we also release explicitly below

	for i := 1; i <= 2; i++ {
		ticks <- time.Now()
		<-renewed
		fmt.Printf("tick %d: renewed, lease alive = %v\n", i, ctx.Err() == nil)
	}

	ticks <- time.Now() // third renew fails
	<-renewed
	<-ctx.Done() // the failure cancelled leaseCtx; observe it exactly once
	fmt.Println("tick 3: renew failed")
	fmt.Printf("lease lost = %v\n", errors.Is(context.Cause(ctx), lease.ErrLeaseLost))
	fmt.Printf("cause = %v\n", context.Cause(ctx))

	release()
	fmt.Println("released; keeper joined")
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
tick 1: renewed, lease alive = true
tick 2: renewed, lease alive = true
tick 3: renew failed
lease lost = true
cause = lease lost: etcd: lease not found
released; keeper joined
```

### Tests

Each test drives ticks over a buffered channel and synchronizes on a `called`
signal the renewer sends, so one tick maps to exactly one observed renew with no
`time.Sleep`. `TestMain` runs `goleak.VerifyTestMain` so every test also proves
the single launched goroutine terminated. `TestRenewSucceedsKeepsLeaseAlive`
drives five good ticks and asserts five renews and `leaseCtx.Err() == nil`.
`TestRenewFailureCancelsWithCause` fails on tick three and asserts
`context.Cause` wraps `ErrLeaseLost` and `Err()` is `context.Canceled`.
`TestStopAfterFirstFailure` freezes the counter and then floods the buffer to
prove no renew runs after the loss. `TestParentCancelStopsRenewerWithParentCause`
asserts the parent's cause propagates and `ErrLeaseLost` does not.
`TestReleaseJoinsAndIsIdempotent` and `TestReleaseAfterLeaseLostIsSafe` call
`release` twice on both the live and lost paths. `TestCauseDeliveredExactlyOnce`
asserts the cause is stable after the first delivery.

Create `lease_test.go`:

```go
package lease

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain runs goleak after every test: it proves the single launched renewer
// goroutine has terminated in each case, whatever the exit path.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// countingRenew returns a renew that increments count on every call and, once
// count reaches failAt, returns an error. failAt <= 0 means it never fails.
// called signals each entry so a test can drive one tick and wait for exactly
// one renew, deterministically and without sleeping.
func countingRenew(count *atomic.Int64, called chan<- struct{}, failAt int64) func(context.Context) error {
	return func(ctx context.Context) error {
		n := count.Add(1)
		called <- struct{}{}
		if failAt > 0 && n >= failAt {
			return errors.New("backend: lease not found")
		}
		return nil
	}
}

func TestRenewSucceedsKeepsLeaseAlive(t *testing.T) {
	ticks := make(chan time.Time, 8)
	called := make(chan struct{}, 8)
	var count atomic.Int64

	ctx, release := Acquire(context.Background(), ticks, countingRenew(&count, called, 0))
	defer release()

	const n = 5
	for range n {
		ticks <- time.Now()
		<-called // one renew per tick, observed before we drive the next
	}

	if got := count.Load(); got != n {
		t.Fatalf("renew called %d times, want %d", got, n)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("leaseCtx.Err() = %v, want nil (lease still held)", err)
	}
	if cause := context.Cause(ctx); cause != nil {
		t.Fatalf("context.Cause = %v, want nil", cause)
	}
}

func TestRenewFailureCancelsWithCause(t *testing.T) {
	ticks := make(chan time.Time, 8)
	called := make(chan struct{}, 8)
	var count atomic.Int64

	ctx, release := Acquire(context.Background(), ticks, countingRenew(&count, called, 3))
	defer release()

	// Ticks 1 and 2 succeed; tick 3 fails.
	for range 3 {
		ticks <- time.Now()
		<-called
	}

	<-ctx.Done() // the failure cancelled leaseCtx
	if !errors.Is(context.Cause(ctx), ErrLeaseLost) {
		t.Fatalf("context.Cause = %v, want it to wrap ErrLeaseLost", context.Cause(ctx))
	}
	if got := ctx.Err(); !errors.Is(got, context.Canceled) {
		t.Fatalf("leaseCtx.Err() = %v, want context.Canceled", got)
	}
}

func TestStopAfterFirstFailure(t *testing.T) {
	ticks := make(chan time.Time, 8)
	called := make(chan struct{}, 8)
	var count atomic.Int64

	ctx, release := Acquire(context.Background(), ticks, countingRenew(&count, called, 2))
	defer release()

	// Tick 1 succeeds, tick 2 fails and stops the renewer.
	ticks <- time.Now()
	<-called
	ticks <- time.Now()
	<-called
	<-ctx.Done()

	frozen := count.Load()
	if frozen != 2 {
		t.Fatalf("renew count at failure = %d, want 2", frozen)
	}

	// Deliver more ticks into the buffer. The renewer has stopped, so it must
	// never drain them: the count stays frozen.
	for range 4 {
		ticks <- time.Now()
	}
	for range 100 {
		if count.Load() != frozen {
			t.Fatalf("renew called after lease loss: count = %d, want %d", count.Load(), frozen)
		}
		runtime.Gosched()
	}
}

func TestParentCancelStopsRenewerWithParentCause(t *testing.T) {
	ticks := make(chan time.Time, 8)
	called := make(chan struct{}, 8)
	var count atomic.Int64

	parent, cancelParent := context.WithCancelCause(context.Background())
	ctx, release := Acquire(parent, ticks, countingRenew(&count, called, 0))
	defer release()

	ticks <- time.Now() // one successful renew before cancel
	<-called

	sentinel := errors.New("request aborted")
	cancelParent(sentinel)

	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), sentinel) {
		t.Fatalf("context.Cause = %v, want parent cause %v", context.Cause(ctx), sentinel)
	}
	if errors.Is(context.Cause(ctx), ErrLeaseLost) {
		t.Fatalf("context.Cause wrongly reports ErrLeaseLost on parent cancel")
	}
}

func TestReleaseJoinsAndIsIdempotent(t *testing.T) {
	ticks := make(chan time.Time, 8)
	called := make(chan struct{}, 8)
	var count atomic.Int64

	ctx, release := Acquire(context.Background(), ticks, countingRenew(&count, called, 0))

	ticks <- time.Now()
	<-called

	release()
	release() // idempotent: no panic, no double-close

	// After release the keeper has been joined; leaseCtx is cancelled.
	if err := ctx.Err(); err == nil {
		t.Fatalf("leaseCtx.Err() = nil after release, want cancelled")
	}
}

func TestReleaseAfterLeaseLostIsSafe(t *testing.T) {
	ticks := make(chan time.Time, 8)
	called := make(chan struct{}, 8)
	var count atomic.Int64

	ctx, release := Acquire(context.Background(), ticks, countingRenew(&count, called, 1))

	ticks <- time.Now() // first renew fails
	<-called
	<-ctx.Done()

	// The lease is already lost; release must still join cleanly and not panic.
	release()
	release()
}

func TestCauseDeliveredExactlyOnce(t *testing.T) {
	ticks := make(chan time.Time, 8)
	called := make(chan struct{}, 8)
	var count atomic.Int64

	ctx, release := Acquire(context.Background(), ticks, countingRenew(&count, called, 1))
	defer release()

	ticks <- time.Now()
	<-called
	<-ctx.Done()

	first := context.Cause(ctx)
	if !errors.Is(first, ErrLeaseLost) {
		t.Fatalf("first cause = %v, want ErrLeaseLost", first)
	}
	// The cause is set once and is stable: deliver more ticks and re-read.
	for range 4 {
		ticks <- time.Now()
	}
	for range 100 {
		if context.Cause(ctx) != first {
			t.Fatalf("cause changed after first delivery: %v then %v", first, context.Cause(ctx))
		}
		runtime.Gosched()
	}
}
```

## Review

"Correct" here means three invariants hold simultaneously: cancel-cause
correctness (the holder observes the specific cause exactly once), leak-freedom of
the single launched goroutine on every exit path, and stop-after-first-failure (no
renew runs once the lease is declared lost). The `context.WithCancelCause` chain
guarantees the first, because context sets a cause exactly once and propagates a
parent's cause to the derived context untouched, so a lost lease
(`ErrLeaseLost`) and an aborted request (the parent's cause) are always
distinguishable. The `select`-with-three-returning-arms plus the `sync.Once`
join in `release` guarantee the second, which `goleak` verifies after every test.
The `return` immediately after `cancel`, together with the `ctx.Err()` re-check
after a tick wins the `select`, guarantees the third, which the frozen counter and
flooded buffer pin down. The production bug this prevents is the worst kind: a
holder that keeps executing a critical section against a lock it silently lost, or
a keeper goroutine that renews forever after the holder moved on -- a
split-brain write and a goroutine leak at once.

## Resources

- [context package](https://pkg.go.dev/context) -- `WithCancelCause`, `Cause`, and how a parent's cause propagates to derived contexts.
- [Go 1.21 release notes: context.WithCancelCause](https://go.dev/doc/go1.21#context) -- the cancel-with-cause API this exercise is built on.
- [go.uber.org/goleak](https://pkg.go.dev/go.uber.org/goleak) -- the leak detector that proves the single renewer terminated on every path.
- [Go Language Specification: Select statements](https://go.dev/ref/spec#Select_statements) -- why a ready arm is chosen at random, which is why the keeper re-checks `ctx.Err()` after a tick.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [13-request-hedging-backup-replica-leakfree.md](13-request-hedging-backup-replica-leakfree.md) | Next: [../02-channel-basics/00-concepts.md](../02-channel-basics/00-concepts.md)
