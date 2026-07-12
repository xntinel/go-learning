# Exercise 17: Distributed Lock Lease TTL Extension on Success

A distributed lock backed by a lease (a TTL another process trusts to decide
the holder is still alive) has a subtle timing bug waiting to happen: if the
lease is extended based on when the operation *started*, a slow operation can
run past its own extension and have the lock expire out from under it while
it is still working. This exercise builds a lease that is always extended
based on when the protected operation *finished*, using a deferred closure
keyed on a named `time.Time` result and an injected clock so the test never
depends on real wall-clock time.

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde de fallo del operador).

## What you'll build

```text
leaselock/                    independent module: example.com/leaselock
  go.mod
  leaselock.go                 Clock; FakeClock; Lock; WithLease (named expiresAt, deferred extension)
  cmd/demo/
    main.go                    runnable demo: success and failure both extend the lease
  leaselock_test.go             extension anchored on completion time, extends on failure, sequential extensions
```

- Files: `leaselock.go`, `cmd/demo/main.go`, `leaselock_test.go`.
- Implement: `(*Lock) WithLease(ttl time.Duration, fn func() error) (expiresAt time.Time, err error)` where a deferred closure computes `expiresAt` from the injected clock's time *after* `fn` runs, regardless of whether `fn` succeeded or failed.
- Test: the lease extension is anchored on the time `fn` finished (not started); a failing `fn` still extends the lease; sequential leases keep advancing `HeldUntil`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/02-named-return-values/17-distributed-lock-ttl-extension/cmd/demo
cd go-solutions/04-functions/02-named-return-values/17-distributed-lock-ttl-extension
go mod edit -go=1.24
```

### Extend from where the work actually ended, not where it began

`WithLease`'s named result `expiresAt` exists so the deferred closure can
compute it *after* `fn` has run, using whatever time the clock reports at
that moment:

```go
defer func() {
    expiresAt = l.clock.Now().Add(ttl)
    l.HeldUntil = expiresAt
}()

err = fn()
return
```

If the extension were instead computed before calling `fn` — `expiresAt :=
clock.Now().Add(ttl)` at the top of the function, with the defer just
persisting that stale value — a slow `fn` could run long enough that the
lease it "extended" at the start is already in the past by the time `fn`
returns, and another process could have already assumed the lock was free.
Anchoring the extension on completion time, through a named result a defer
can freely rewrite, closes that gap. The clock is injected as an interface so
the test can advance it deterministically instead of sleeping for real time.

Create `leaselock.go`:

```go
package leaselock

import (
	"sync"
	"time"
)

// Clock is an injected time source so tests never depend on wall-clock time
// or real sleeps.
type Clock interface {
	Now() time.Time
}

// FakeClock is a manually advanced Clock used by tests and the demo.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFakeClock returns a FakeClock starting at t.
func NewFakeClock(t time.Time) *FakeClock {
	return &FakeClock{now: t}
}

// Now returns the current fake time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the fake clock forward by d, simulating time spent doing work.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Lock is a distributed lock lease: while held, HeldUntil reports the
// deadline the lease is valid through.
type Lock struct {
	mu        sync.Mutex
	clock     Clock
	HeldUntil time.Time
}

// NewLock returns a lease-based lock that reads time from clock.
func NewLock(clock Clock) *Lock {
	return &Lock{clock: clock}
}

// WithLease runs fn while holding the lease, and extends the lease TTL to
// expiresAt = (completion time) + ttl before returning, regardless of
// whether fn succeeded, failed, or panicked.
//
// expiresAt is a named result specifically so the deferred closure can
// compute and assign it after fn has run: the lease must be extended based
// on when the operation *finished*, not when it started, or a slow operation
// could see its lock expire mid-flight even though the caller believed the
// lease was extended at the start. Anchoring the extension in a defer keyed
// on the named result is what makes that guarantee hold on every exit path.
func (l *Lock) WithLease(ttl time.Duration, fn func() error) (expiresAt time.Time, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	defer func() {
		expiresAt = l.clock.Now().Add(ttl)
		l.HeldUntil = expiresAt
	}()

	err = fn()
	return
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"errors"
	"fmt"
	"time"

	"example.com/leaselock"
)

func main() {
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := leaselock.NewFakeClock(t0)
	lock := leaselock.NewLock(clock)

	expiresAt, err := lock.WithLease(5*time.Second, func() error {
		clock.Advance(3 * time.Second) // simulate 3s of work
		return nil
	})
	fmt.Printf("success: err=%v expiresAt=%s heldUntil=%s\n",
		err, expiresAt.Format(time.RFC3339), lock.HeldUntil.Format(time.RFC3339))

	expiresAt2, err2 := lock.WithLease(5*time.Second, func() error {
		clock.Advance(2 * time.Second)
		return errors.New("operation failed")
	})
	fmt.Printf("failure: err=%v expiresAt=%s\n", err2, expiresAt2.Format(time.RFC3339))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
success: err=<nil> expiresAt=2024-01-01T00:00:08Z heldUntil=2024-01-01T00:00:08Z
failure: err=operation failed expiresAt=2024-01-01T00:00:10Z
```

### Tests

Create `leaselock_test.go`:

```go
package leaselock

import (
	"errors"
	"testing"
	"time"
)

func TestWithLeaseExtendsFromCompletionTime(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := NewFakeClock(t0)
	lock := NewLock(clock)

	expiresAt, err := lock.WithLease(5*time.Second, func() error {
		clock.Advance(3 * time.Second)
		return nil
	})
	if err != nil {
		t.Fatalf("WithLease: unexpected error: %v", err)
	}

	want := t0.Add(3 * time.Second).Add(5 * time.Second)
	if !expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v, want %v (anchored on completion time, not start time)", expiresAt, want)
	}
	if !lock.HeldUntil.Equal(want) {
		t.Fatalf("HeldUntil = %v, want %v", lock.HeldUntil, want)
	}
}

func TestWithLeaseExtendsEvenOnFailure(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := NewFakeClock(t0)
	lock := NewLock(clock)

	wantErr := errors.New("boom")
	expiresAt, err := lock.WithLease(5*time.Second, func() error {
		clock.Advance(2 * time.Second)
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}

	want := t0.Add(2 * time.Second).Add(5 * time.Second)
	if !expiresAt.Equal(want) {
		t.Fatalf("expiresAt = %v, want %v (lease extended despite failure)", expiresAt, want)
	}
}

func TestWithLeaseSequentialExtensions(t *testing.T) {
	t.Parallel()

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := NewFakeClock(t0)
	lock := NewLock(clock)

	_, err := lock.WithLease(5*time.Second, func() error {
		clock.Advance(1 * time.Second)
		return nil
	})
	if err != nil {
		t.Fatalf("first WithLease: %v", err)
	}
	first := lock.HeldUntil

	_, err = lock.WithLease(5*time.Second, func() error {
		clock.Advance(4 * time.Second)
		return nil
	})
	if err != nil {
		t.Fatalf("second WithLease: %v", err)
	}
	if !lock.HeldUntil.After(first) {
		t.Fatalf("HeldUntil did not advance across leases: %v -> %v", first, lock.HeldUntil)
	}
}
```

## Review

`WithLease` is correct when every exit — success or failure — extends the
lease based on the clock reading *after* `fn` has run, never before. The
named result `expiresAt` is what lets the deferred closure both compute and
report the final deadline in one place, instead of duplicating that logic at
every return point inside `fn`'s caller. The mistake to avoid is computing the
extension deadline before calling `fn` and only persisting it in the defer —
that reintroduces exactly the mid-operation expiry bug this pattern exists to
prevent. The injected `Clock` is what makes the test deterministic: no real
sleep, no flakiness, just an explicit `Advance` call standing in for elapsed
work time.

## Resources

- [Go Spec: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [`time.Time`](https://pkg.go.dev/time#Time)
- [Effective Go: Named result parameters](https://go.dev/doc/effective_go#named-results)

---

Back to [00-concepts.md](00-concepts.md) | Prev: [16-queue-backpressure-overflow-guard.md](16-queue-backpressure-overflow-guard.md) | Next: [18-db-statement-cache-eviction-guard.md](18-db-statement-cache-eviction-guard.md)
