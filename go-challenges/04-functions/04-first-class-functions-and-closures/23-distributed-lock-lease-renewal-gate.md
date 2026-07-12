# Exercise 23: Distributed Lock Lease Renewal with Auto-Extend Closure

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye concurrencia).

A distributed lock's lease has a time-to-live so a crashed holder cannot lock
a resource forever, but a long-running critical section must actively renew
the lease before it expires or it loses the lock mid-work. `NewLeaseManager`
returns three closures sharing one captured map of lock IDs to expiry times,
and `WithLease` is the higher-order function that wraps any critical section
with automatic acquire-then-release, handing the caller a `renew` callback
to extend the lease exactly when it needs to.

## What you'll build

```text
lease/                     independent module: example.com/distributed-lock-lease-renewal
  go.mod                   go 1.24
  lease.go                 NewLeaseManager, WithLease higher-order wrapper
  cmd/
    demo/
      main.go               a two-step job that renews its lease mid-way
  lease_test.go             table test: acquire/renew/expire, WithLease, concurrency
```

- Files: `lease.go`, `cmd/demo/main.go`, `lease_test.go`.
- Implement: `NewLeaseManager(ttl time.Duration, now func() time.Time) (acquire, renew func(string) bool, release func(string))`, all three closing over one mutex-guarded `map[string]time.Time`; `WithLease(acquire, renew, release, lockID string, critical func(renewFn func() bool) error) error`.
- Test: a table walks acquire, refusal while held, renew, refusal after renew, and reacquire after full expiry; a second test proves renew fails on an unheld or already-expired lock; `WithLease` tests cover the happy path and the already-held fast-fail; a concurrency test fires 200 goroutines at one lock ID and asserts exactly one wins under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/04-first-class-functions-and-closures/23-distributed-lock-lease-renewal-gate/cmd/demo
cd go-solutions/04-functions/04-first-class-functions-and-closures/23-distributed-lock-lease-renewal-gate
go mod edit -go=1.24
```

### Three closures, one captured lease table

`NewLeaseManager` declares a single `leases map[string]time.Time` and a
mutex, then returns `acquire`, `renew`, and `release` all closing over both.
`acquire` succeeds if the lock has no recorded lease, or the recorded lease
has already expired according to the injected clock; either way it writes a
fresh expiry `now() + ttl`. `renew` only succeeds if the lock is *currently*
held and not yet expired — it cannot be used to grab a lock it doesn't
already hold, which is what stops a stale renewal from resurrecting a lock
some other owner has since acquired. Every one of the three operations does
its comparison and its map write inside one lock acquisition; if `acquire`
read the expiry, released the lock, and then wrote a new one, two goroutines
racing for the same `lockID` could both observe "not held" and both write,
and both would believe they hold an exclusive lock. The concurrency test
proves that cannot happen.

`WithLease` is the higher-order function that turns the three primitives
into the shape application code actually wants: acquire once, run the
critical section, always release — even if the critical section returns an
error — and hand the critical section a `renewFn` closure it can call as
often as it needs. This is dependency injection in the other direction from
the clock-injection modules elsewhere in this lesson: instead of injecting
time into the function under test, `WithLease` injects a *capability*
(renewal) into the caller's own code, so the caller decides when renewal is
needed without ever touching the lease map directly.

Create `lease.go`:

```go
package lease

import (
	"errors"
	"sync"
	"time"
)

// ErrLockHeld is returned by WithLease when lockID is already held by
// another owner and has not yet expired.
var ErrLockHeld = errors.New("lease: lock is already held")

// NewLeaseManager returns three closures — acquire, renew, release — sharing
// one captured map of lockID to expiry, guarded by a mutex. acquire admits
// the caller if the lock is free or its previous lease has expired; renew
// extends an already-held, not-yet-expired lease; release drops it
// immediately. now is injected so tests advance a fake clock instead of
// sleeping.
//
// Every operation's check-then-act (read the current expiry, compare to now,
// write the new expiry) happens inside one critical section, so two
// goroutines racing to acquire the same lockID can never both succeed.
func NewLeaseManager(ttl time.Duration, now func() time.Time) (acquire func(lockID string) bool, renew func(lockID string) bool, release func(lockID string)) {
	var mu sync.Mutex
	leases := make(map[string]time.Time)

	acquire = func(lockID string) bool {
		mu.Lock()
		defer mu.Unlock()
		if expiry, held := leases[lockID]; held && now().Before(expiry) {
			return false
		}
		leases[lockID] = now().Add(ttl)
		return true
	}

	renew = func(lockID string) bool {
		mu.Lock()
		defer mu.Unlock()
		expiry, held := leases[lockID]
		if !held || !now().Before(expiry) {
			return false
		}
		leases[lockID] = now().Add(ttl)
		return true
	}

	release = func(lockID string) {
		mu.Lock()
		defer mu.Unlock()
		delete(leases, lockID)
	}

	return acquire, renew, release
}

// WithLease is a higher-order function that wraps a critical section with
// automatic lease acquisition and release. It acquires lockID, fails fast
// with ErrLockHeld if that fails, and otherwise runs critical, passing it a
// renewFn it can call as often as it needs to extend the lease before ttl
// runs out. The lease is always released when critical returns, whether or
// not it returned an error.
func WithLease(acquire func(string) bool, renew func(string) bool, release func(string), lockID string, critical func(renewFn func() bool) error) error {
	if !acquire(lockID) {
		return ErrLockHeld
	}
	defer release(lockID)

	return critical(func() bool { return renew(lockID) })
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	"example.com/distributed-lock-lease-renewal"
)

func main() {
	clockNow := time.Unix(0, 0)
	clock := func() time.Time { return clockNow }

	acquire, renew, release := lease.NewLeaseManager(10*time.Second, clock)

	err := lease.WithLease(acquire, renew, release, "job:nightly-export", func(renewFn func() bool) error {
		fmt.Println("step 1: exporting batch A")
		clockNow = clockNow.Add(8 * time.Second)

		if !renewFn() {
			return fmt.Errorf("could not renew lease before step 2")
		}
		fmt.Println("lease renewed before it expired")

		clockNow = clockNow.Add(8 * time.Second)
		fmt.Println("step 2: exporting batch B")
		return nil
	})
	if err != nil {
		fmt.Println("job failed:", err)
		return
	}
	fmt.Println("job completed, lease released")

	fmt.Println("re-acquire after release:", acquire("job:nightly-export"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
step 1: exporting batch A
lease renewed before it expired
step 2: exporting batch B
job completed, lease released
re-acquire after release: true
```

### Tests

Create `lease_test.go`:

```go
package lease

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fakeClock(start time.Time) (now func() time.Time, advance func(time.Duration)) {
	cur := start
	now = func() time.Time { return cur }
	advance = func(d time.Duration) { cur = cur.Add(d) }
	return now, advance
}

func TestAcquireRenewExpireSequence(t *testing.T) {
	now, advance := fakeClock(time.Unix(0, 0))
	acquire, renew, release := NewLeaseManager(10*time.Second, now)

	tests := []struct {
		name    string
		advance time.Duration
		op      func() bool
		want    bool
	}{
		{"first acquire succeeds", 0, func() bool { return acquire("lock-a") }, true},
		{"second acquire refused (still held)", 0, func() bool { return acquire("lock-a") }, false},
		{"renew before expiry succeeds", 5 * time.Second, func() bool { return renew("lock-a") }, true},
		{"acquire still refused after renew", 0, func() bool { return acquire("lock-a") }, false},
		{"acquire after full expiry (past renewed ttl) succeeds", 11 * time.Second, func() bool { return acquire("lock-a") }, true},
	}

	for _, tc := range tests {
		advance(tc.advance)
		if got := tc.op(); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	release("lock-a")
	if !acquire("lock-a") {
		t.Fatal("acquire after release: got false, want true")
	}
}

func TestRenewFailsForUnheldOrExpiredLock(t *testing.T) {
	now, advance := fakeClock(time.Unix(0, 0))
	_, renew, _ := NewLeaseManager(5*time.Second, now)

	if renew("never-acquired") {
		t.Fatal("renew on a lock nobody acquired: got true, want false")
	}

	acquire, renew2, _ := NewLeaseManager(5*time.Second, now)
	acquire("expired-lock")
	advance(6 * time.Second) // past the 5s ttl
	if renew2("expired-lock") {
		t.Fatal("renew on an already-expired lock: got true, want false")
	}
}

func TestWithLeaseRunsCriticalSectionAndReleasesOnReturn(t *testing.T) {
	now, advance := fakeClock(time.Unix(0, 0))
	acquire, renew, release := NewLeaseManager(10*time.Second, now)

	renewedOK := false
	err := WithLease(acquire, renew, release, "job", func(renewFn func() bool) error {
		advance(5 * time.Second)
		renewedOK = renewFn()
		return nil
	})

	if err != nil {
		t.Fatalf("WithLease returned error: %v", err)
	}
	if !renewedOK {
		t.Fatal("renewFn inside critical section: got false, want true")
	}
	if !acquire("job") {
		t.Fatal("acquire after WithLease returns: got false, want true (lease must be released)")
	}
}

func TestWithLeaseFailsFastWhenAlreadyHeld(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0))
	acquire, renew, release := NewLeaseManager(10*time.Second, now)

	acquire("busy") // held by someone else

	called := false
	err := WithLease(acquire, renew, release, "busy", func(func() bool) error {
		called = true
		return nil
	})

	if err != ErrLockHeld {
		t.Fatalf("err = %v, want ErrLockHeld", err)
	}
	if called {
		t.Fatal("critical section ran despite lock being held")
	}
}

func TestAcquireConcurrentOnlyOneWinner(t *testing.T) {
	now, _ := fakeClock(time.Unix(0, 0))
	acquire, _, _ := NewLeaseManager(time.Minute, now)

	const attempts = 200
	var wg sync.WaitGroup
	var won atomic.Int32
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if acquire("shared-lock") {
				won.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := won.Load(); got != 1 {
		t.Fatalf("winners = %d, want exactly 1", got)
	}
}
```

Verify: `go test -count=1 -race ./...`

## Review

The first table is the full lease lifecycle: acquire, refusal while held,
renew, refusal even after renewing (the lease is extended, not released),
and a successful reacquire only once the (renewed) lease has actually fully
expired. The second test is the edge case an auto-renewal system must get
right: `renew` must refuse on a lock nobody holds and on one that has already
expired, or a stale renewal call could silently resurrect a lock that was
never really this caller's to extend. The two `WithLease` tests cover the
higher-order wrapper's contract — release always happens, and an already-held
lock fails fast without ever calling the critical section. The concurrency
test is the one that would fail if `acquire`'s check and write were two
separate locked operations instead of one: 200 goroutines racing for the same
`lockID` must produce exactly one winner, every run, under `-race`.

## Resources

- [pkg.go.dev: sync.Mutex](https://pkg.go.dev/sync#Mutex) — guarding the shared lease map's full check-then-act.
- [etcd docs: Lease](https://etcd.io/docs/v3.5/learning/api/#lease-api) — the production distributed-lock primitive (TTL plus keep-alive renewal) this exercise models.
- [pkg.go.dev/sync/atomic](https://pkg.go.dev/sync/atomic) — the counter the concurrency test uses to count winners across goroutines.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [22-audit-log-factory-with-tenant-context.md](22-audit-log-factory-with-tenant-context.md) | Next: [24-feature-flag-evaluator-compiled-rules.md](24-feature-flag-evaluator-compiled-rules.md)
