# Exercise 20: Distributed Lock With Lease — Release Under Context Deadline

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde).

A distributed lock is never really "held" the way an in-process
`sync.Mutex` is: it is a lease with a TTL, because the process holding it can
crash, get network-partitioned, or simply run long, and every other client
needs a bound on how long they will wait for a dead holder's lock to free
up. That changes what "release on defer" has to mean: the release itself
must be ownership-checked (you cannot release a lease you no longer hold)
and bounded by its own deadline (a network unlock call must not hang
forever), and a panic must not skip either.

## What you'll build

```text
distlock/                      independent module: example.com/distlock
  go.mod
  distlock/distlock.go          LockService (Acquire/Renew/Release/Holder); WithLock (defer bounded release)
  distlock/distlock_test.go     table of cases: success, contention, expiry-and-steal, renew, panic, dead context
  cmd/demo/main.go              runnable demo: acquire, work, release; then a contended acquire
```

- Files: `distlock/distlock.go`, `distlock/distlock_test.go`, `cmd/demo/main.go`.
- Implement: a `LockService` with an injected clock (`now func() time.Time`), `Acquire(owner string, ttl time.Duration) bool` that only fails when a different, non-expired owner holds it, `Renew(owner string, ttl time.Duration) bool` that fails once `owner` no longer holds the lock, `Release(ctx context.Context, owner string)` that is a no-op unless `owner` still holds it, and `Holder() string`; and `WithLock(ctx, svc, owner, ttl, work func(renew func() bool) error) (err error)` that acquires, defers a `recover` + bounded-context `Release` + re-`panic`, checks `ctx.Err()`, and runs `work`.
- Test: a fake, manually-advanced clock (no real sleeping); success releases the lock; a lock already held by another (non-expired) owner returns `ErrLockHeld` without running `work`; a lease left to expire mid-`work` is stolen by another owner and the original owner's deferred release is a no-op; a lease renewed mid-`work` survives a concurrent steal attempt; a panic mid-`work` still releases the lock before re-panicking; an already-cancelled caller context returns its error without running `work`, but the lock acquired just before that check is still released.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/distlock/distlock ~/go-exercises/distlock/cmd/demo
cd ~/go-exercises/distlock
go mod init example.com/distlock
go mod edit -go=1.24
```

### Release must check ownership, or the defer can steal a lock back

The bug this module is built to prevent: `owner1` acquires a 2-second lease,
its work runs long (or just doesn't renew), the lease expires, `owner2`
correctly steals it and starts its own critical section — and then
`owner1`'s deferred `Release(owner1)` fires, blindly clears the lock, and
`owner2` is now running unprotected with no lock at all. `Release`'s
`if s.owner == owner` guard is the fix: by the time `owner1`'s defer runs,
`s.owner` is `"owner2"`, not `"owner1"`, so the release is a no-op.
`TestWithLockLeaseExpiresAndDeferredReleaseIsANoOp` is written specifically
to catch a regression of this guard — remove the ownership check and that
test starts failing because `Holder()` comes back empty instead of
`"workerB"`.

### A dead clock, not a dead stopwatch

Every scenario that depends on "time passing" — a lease expiring, a renewal
extending it — is driven by `clock.Advance(d)` on a hand-rolled, mutex-guarded
`fakeClock`, never by `time.Sleep`. `LockService` takes `now func() time.Time`
instead of calling `time.Now()` directly, so a test can jump the clock
forward by exactly 3 seconds in zero wall-clock time and get an exact,
reproducible answer to "has this 2-second lease expired yet." The production
constructor just passes `time.Now` (see the demo); nothing about
`LockService` itself changes between test and production, only which clock
function it is handed.

### Two different deadlines, on purpose

`ctx` (the caller's budget for the whole operation) and `releaseGrace` (a
fixed bound on the release call inside the defer) are deliberately
independent. If `ctx` is already past its deadline — which is exactly
`TestWithLockReleasesEvenWhenCallerContextIsAlreadyDone` — the release still
needs its own window to run, because giving up on the release the instant
the caller's context expires would leak the lease. Building `releaseCtx` from
`context.Background()` rather than deriving it from the (possibly-expired)
`ctx` is what buys that independence.

Create `distlock/distlock.go`:

```go
package distlock

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrLockHeld is returned when Acquire cannot claim the lock because another
// owner currently holds a lease that has not yet expired.
var ErrLockHeld = errors.New("distlock: lock held by another owner")

// LockService is an in-memory stand-in for a distributed lock service (a
// Redis SET NX PX, a DynamoDB conditional item, or similar): a single
// record holding the current owner and its lease expiry.
type LockService struct {
	mu      sync.Mutex
	owner   string
	expires time.Time
	now     func() time.Time
}

// NewLockService builds a LockService that reads the current time from now,
// so tests can inject a fake clock instead of depending on wall time.
func NewLockService(now func() time.Time) *LockService {
	return &LockService{now: now}
}

// Acquire grants the lock to owner if it is free, already owned by owner, or
// its lease has expired. It fails only when a different owner holds a lease
// that has not yet expired -- the same "steal an expired lease" rule every
// real distributed lock (Redlock, DynamoDB lease locks) relies on.
func (s *LockService) Acquire(owner string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	if s.owner != "" && s.owner != owner && now.Before(s.expires) {
		return false
	}
	s.owner = owner
	s.expires = now.Add(ttl)
	return true
}

// Renew extends owner's lease by ttl. It fails (returns false) if owner no
// longer holds the lock -- for example because the lease already expired and
// another caller's Acquire stole it in the meantime.
func (s *LockService) Renew(owner string, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.owner != owner {
		return false
	}
	s.expires = s.now().Add(ttl)
	return true
}

// Release gives up the lock, but only if owner still holds it. This
// ownership check is what makes Release safe to call unconditionally from a
// defer: if the lease already expired and a different owner has since
// re-acquired it, Release is a no-op instead of stealing the lock back out
// from under its new, legitimate owner. ctx is accepted for signature parity
// with a real client, where Release is a network call (an UNLINK to Redis,
// a conditional DeleteItem to DynamoDB) that needs its own deadline; this
// in-memory simulation completes synchronously, so it does not consult ctx.
func (s *LockService) Release(ctx context.Context, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.owner == owner {
		s.owner = ""
		s.expires = time.Time{}
	}
}

// Holder reports who last successfully acquired (and has not since released
// or been displaced from) the lock; "" if nobody currently holds it.
func (s *LockService) Holder() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owner
}

// releaseGrace bounds the deferred release below: in a real client, Release
// is a network call (an UNLINK to Redis, a conditional DeleteItem to
// DynamoDB) that itself needs a deadline so a wedged network path cannot
// hold the goroutine hostage forever. It is deliberately independent of the
// caller's ctx, which may already be past its own deadline -- the release
// must still get its own bounded chance to run.
const releaseGrace = 2 * time.Second

// WithLock acquires the distributed lock for owner and runs work, which
// receives a renew closure it can call to extend the lease during
// long-running work. The deferred cleanup recovers a panic, releases the
// lock within its own bounded context (independent of ctx), and only then
// re-panics -- so the lease never outlives this call on any exit path, while
// Release's ownership check means a lease that already expired and was
// stolen by someone else is never incorrectly released out from under them.
func WithLock(ctx context.Context, svc *LockService, owner string, ttl time.Duration, work func(renew func() bool) error) (err error) {
	if !svc.Acquire(owner, ttl) {
		return ErrLockHeld
	}

	defer func() {
		r := recover()

		releaseCtx, cancel := context.WithTimeout(context.Background(), releaseGrace)
		defer cancel()
		svc.Release(releaseCtx, owner)

		if r != nil {
			panic(r)
		}
	}()

	if err := ctx.Err(); err != nil {
		return err
	}

	renew := func() bool { return svc.Renew(owner, ttl) }
	return work(renew)
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"example.com/distlock/distlock"
)

func main() {
	svc := distlock.NewLockService(time.Now)

	err := distlock.WithLock(context.Background(), svc, "worker-1", 5*time.Second, func(renew func() bool) error {
		fmt.Println("holder during work:", svc.Holder())
		return nil
	})
	fmt.Println("error:", err)
	fmt.Println("holder after work:", "\""+svc.Holder()+"\"")

	// A second acquisition attempt while the lock is held elsewhere fails fast.
	svc.Acquire("worker-2", 5*time.Second)
	err = distlock.WithLock(context.Background(), svc, "worker-3", 5*time.Second, func(renew func() bool) error {
		return nil
	})
	fmt.Println("error when already held:", err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
holder during work: worker-1
error: <nil>
holder after work: ""
error when already held: distlock: lock held by another owner
```

### Tests

Create `distlock/distlock_test.go`:

```go
package distlock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is an injected, manually-advanced clock: tests control exactly
// when time "passes," with no real sleeping and no flakiness.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestWithLockReleasesAfterSuccessfulWork(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	svc := NewLockService(clock.Now)

	err := WithLock(context.Background(), svc, "workerA", 5*time.Second, func(renew func() bool) error {
		if svc.Holder() != "workerA" {
			t.Fatalf("Holder() during work = %q, want workerA", svc.Holder())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := svc.Holder(); got != "" {
		t.Fatalf("Holder() after WithLock = %q, want empty", got)
	}
}

func TestWithLockFailsFastWhenAlreadyHeldByAnother(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	svc := NewLockService(clock.Now)
	if !svc.Acquire("workerA", 5*time.Second) {
		t.Fatal("setup: workerA should acquire the free lock")
	}

	ran := false
	err := WithLock(context.Background(), svc, "workerB", 5*time.Second, func(renew func() bool) error {
		ran = true
		return nil
	})

	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("err = %v, want ErrLockHeld", err)
	}
	if ran {
		t.Fatal("work must not run when the lock could not be acquired")
	}
	if got := svc.Holder(); got != "workerA" {
		t.Fatalf("Holder() = %q, want workerA (unchanged)", got)
	}
}

func TestWithLockLeaseExpiresAndDeferredReleaseIsANoOp(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	svc := NewLockService(clock.Now)

	err := WithLock(context.Background(), svc, "workerA", 2*time.Second, func(renew func() bool) error {
		// Simulate work that outlives its own TTL without renewing.
		clock.Advance(3 * time.Second)
		if !svc.Acquire("workerB", 2*time.Second) {
			t.Fatal("workerB should be able to steal the now-expired lease")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil (workerA's own work succeeded)", err)
	}
	// workerA's deferred Release must NOT have stolen the lock back: its
	// ownership check sees workerB now holds it, and does nothing.
	if got := svc.Holder(); got != "workerB" {
		t.Fatalf("Holder() = %q, want workerB (workerA's release must be a no-op)", got)
	}
}

func TestWithLockRenewPreventsAConcurrentSteal(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	svc := NewLockService(clock.Now)

	err := WithLock(context.Background(), svc, "workerA", 2*time.Second, func(renew func() bool) error {
		clock.Advance(3 * time.Second) // would have expired without renewal
		if !renew() {
			t.Fatal("renew should succeed while workerA still holds the lease")
		}
		if svc.Acquire("workerB", 2*time.Second) {
			t.Fatal("workerB must not steal a freshly-renewed lease")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := svc.Holder(); got != "" {
		t.Fatalf("Holder() = %q, want empty (workerA released its own lease)", got)
	}
}

func TestWithLockReleasesOnPanic(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	svc := NewLockService(clock.Now)

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic to propagate")
			}
		}()
		_ = WithLock(context.Background(), svc, "workerA", 5*time.Second, func(renew func() bool) error {
			panic("worker crashed mid-task")
		})
	}()

	if got := svc.Holder(); got != "" {
		t.Fatalf("Holder() after panic = %q, want empty", got)
	}
}

func TestWithLockReleasesEvenWhenCallerContextIsAlreadyDone(t *testing.T) {
	t.Parallel()

	clock := newFakeClock()
	svc := NewLockService(clock.Now)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ran := false
	err := WithLock(ctx, svc, "workerA", 5*time.Second, func(renew func() bool) error {
		ran = true
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("work must not run once the caller's context is already done")
	}
	// The lock was acquired before the context check ran, so the deferred
	// release must still have given it back.
	if got := svc.Holder(); got != "" {
		t.Fatalf("Holder() = %q, want empty (lock still released)", got)
	}
}
```

Verify:

```bash
go test -count=1 -race ./...
```

## Review

Six cases, three failure modes covered: normal completion (`TestWithLockReleasesAfterSuccessfulWork`),
contention on an unexpired lease (`TestWithLockFailsFastWhenAlreadyHeldByAnother`,
which asserts `work` never even runs), and three shapes of "something went
wrong mid-lease" — a stolen expired lease (verifying the release does *not*
un-steal it), a successful renewal (verifying the release *does* still work
normally once the lease is properly extended), and a panic (verifying
release plus re-panic). The context-already-done case closes the last gap:
`Acquire` happens before the `defer` and before the `ctx.Err()` check, so
even a caller that shows up with an already-expired budget still gets its
briefly-held lock released rather than leaked. The one invariant every case
shares is `Release`'s ownership check — delete that `if s.owner == owner`
guard and `TestWithLockLeaseExpiresAndDeferredReleaseIsANoOp` fails
immediately, because `workerA`'s defer would clear `workerB`'s legitimately
acquired lock.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [Redis: Distributed Locks with Redis (Redlock)](https://redis.io/docs/latest/develop/use/patterns/distributed-locks/) — the TTL-and-steal semantics this module models.
- [context package](https://pkg.go.dev/context) — `WithTimeout`, `WithCancel`, `Err`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [19-event-subscriber-cleanup-list.md](19-event-subscriber-cleanup-list.md) | Next: [21-write-ahead-log-rollback.md](21-write-ahead-log-rollback.md)
