# Exercise 35: Distributed Lock Acquisition With Lease Token

**Nivel: Intermedio** — validacion rapida (un test corto).

A distributed lock that only returns `bool` cannot be released safely.
Once a process crashes and restarts, or a lock expires and is re-granted to
someone else, "release the lock" needs to mean "release it only if I am
still the one holding it" — and proving that requires a token, not just a
key name. This exercise builds `Locker.Acquire(key, ttl) (acquired bool,
leaseID string, error)` paired with a `Release(key, leaseID)` that only
succeeds when the caller presents the exact lease it was granted.

This module is fully self-contained: its own `go mod init`, all code
inline, its own demo and tests.

## What you'll build

```text
distlock/                  independent module: example.com/distributed-lock-acquire-lease
  go.mod                   go 1.24
  distlock.go              package distlock; Clock; Locker; NewLocker; Acquire(key,ttl) (acquired,leaseID,error); Release(key,leaseID) (released,error)
  cmd/
    demo/
      main.go              two workers contend for a lock; wrong-lease release; correct release; idempotent re-release
  distlock_test.go          free-key acquire; held-key contention; lease expiry with a fake clock; correct-lease release; wrong-lease release fails; never-held-key release is a no-op
```

- Files: `distlock.go`, `cmd/demo/main.go`, `distlock_test.go`.
- Implement: `(*Locker).Acquire(key string, ttl time.Duration) (acquired bool, leaseID string, err error)` granting a fresh, unique lease when a key is free or its previous lease has expired (via an injected `Clock`), and reporting `(false, "", nil)` — not an error — when the key is currently held; `Release(key, leaseID string) (released bool, err error)` succeeding only when `leaseID` matches the currently recorded lease.
- Test: acquiring a free key succeeds with a non-empty lease id; acquiring a held key is contention, not an error, and returns an empty lease id; advancing a fake clock past the TTL grants a new, different lease id; releasing with the correct lease succeeds and frees the key; releasing with the wrong lease token fails with an error while leaving the lock intact; releasing a key that was never held is an idempotent no-op.
- Verify: `go test -count=1 ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Why "release the lock" needs a token, not just a name

A lock API shaped as `Acquire(key) bool` / `Release(key)` looks complete
until a process holding the lock stalls past its TTL — a long GC pause, a
network partition, a container restart. The lock service, having no way to
know the original holder is still alive, correctly lets the TTL expire and
grants the lock to a second process. If the first process eventually wakes
up and calls `Release(key)`, it releases a lock it no longer owns — one now
protecting a completely different critical section for a completely
different process. This is the split-brain scenario every real distributed
lock (Redis's `SET key value NX PX` plus a Lua unlock script, etcd's lease
API, ZooKeeper's ephemeral sequential nodes) is built to prevent, and the
mechanism is always the same: `Acquire` hands back an opaque token, and
`Release` only takes effect when the caller presents that exact token back.

`leaseID` is that token. `Acquire` mints a fresh one on every successful
grant (including a re-grant after expiry), and `Release` compares it
against what is currently on record — a mismatch means "you are not the
current holder", full stop, regardless of whether the caller *used to be*
the holder. That single comparison is what makes it safe for a
resurrected process to call `Release` with its stale token: it fails
loudly instead of silently unlocking someone else's critical section.

Create `distlock.go`:

```go
package distlock

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Clock abstracts time.Now so lease expiry can be tested with exact,
// controlled time steps instead of real elapsed wall-clock time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type lockEntry struct {
	leaseID   string
	expiresAt time.Time
}

// Locker is an in-memory stand-in for a distributed lock service (e.g. a
// Redis SET NX PX / etcd lease). It is safe for concurrent use.
type Locker struct {
	mu    sync.Mutex
	locks map[string]lockEntry
	clock Clock
}

// NewLocker builds a Locker backed by the real wall clock.
func NewLocker() *Locker {
	return newLocker(realClock{})
}

func newLocker(clock Clock) *Locker {
	return &Locker{locks: make(map[string]lockEntry), clock: clock}
}

// Acquire attempts to take the lock identified by key for ttl. If the key
// is free (never locked, or its previous lease has expired), it grants the
// lock and returns a fresh, unique leaseID that the holder must present to
// Release later. If the key is currently held by an unexpired lease,
// Acquire returns (false, "", nil) -- contention is a normal outcome, not
// an error.
func (l *Locker) Acquire(key string, ttl time.Duration) (acquired bool, leaseID string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	if entry, held := l.locks[key]; held && now.Before(entry.expiresAt) {
		return false, "", nil
	}

	id, err := newLeaseID()
	if err != nil {
		return false, "", fmt.Errorf("acquire %q: %w", key, err)
	}
	l.locks[key] = lockEntry{leaseID: id, expiresAt: now.Add(ttl)}
	return true, id, nil
}

// Release frees key, but only if leaseID matches the lease currently
// recorded for it. This makes release idempotent and safe across process
// restarts: a process that crashed, was killed, and retries its release
// call with the same leaseID it was granted can call Release again without
// risk, while a stale holder whose lease already expired and was
// reacquired by someone else can never release a lock it no longer owns.
func (l *Locker) Release(key, leaseID string) (released bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, held := l.locks[key]
	if !held {
		return false, nil // already gone -- an idempotent no-op, not an error
	}
	if entry.leaseID != leaseID {
		return false, fmt.Errorf("release %q: lease token mismatch, caller is not the current holder", key)
	}
	delete(l.locks, key)
	return true, nil
}

func newLeaseID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate lease id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"time"

	distlock "example.com/distributed-lock-acquire-lease"
)

func main() {
	locker := distlock.NewLocker()

	acquired, lease1, err := locker.Acquire("job:nightly-report", 30*time.Second)
	fmt.Printf("worker A acquire: acquired=%t leaseID!=\"\"=%t err=%v\n", acquired, lease1 != "", err)

	// A second worker tries the same job while worker A still holds it.
	acquired, lease2, err := locker.Acquire("job:nightly-report", 30*time.Second)
	fmt.Printf("worker B acquire: acquired=%t leaseID=%q err=%v\n", acquired, lease2, err)

	// Worker B tries to release a lock it never held: no-op, not an error.
	released, err := locker.Release("job:nightly-report", "not-my-lease")
	fmt.Printf("worker B release (wrong lease): released=%t err!=nil=%t\n", released, err != nil)

	// Worker A releases with its real lease id: succeeds.
	released, err = locker.Release("job:nightly-report", lease1)
	fmt.Printf("worker A release (own lease):   released=%t err=%v\n", released, err)

	// Calling release again with the same lease id is an idempotent no-op.
	released, err = locker.Release("job:nightly-report", lease1)
	fmt.Printf("worker A release again:         released=%t err=%v\n", released, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
worker A acquire: acquired=true leaseID!=""=true err=<nil>
worker B acquire: acquired=false leaseID="" err=<nil>
worker B release (wrong lease): released=false err!=nil=true
worker A release (own lease):   released=true err=<nil>
worker A release again:         released=false err=<nil>
```

### Tests

Create `distlock_test.go`:

```go
package distlock

import (
	"testing"
	"time"
)

// fakeClock lets tests advance time by exact, deterministic steps instead
// of sleeping for real durations.
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestAcquireFreeKeySucceeds(t *testing.T) {
	t.Parallel()
	locker := newLocker(&fakeClock{now: time.Unix(1000, 0)})

	acquired, leaseID, err := locker.Acquire("job:a", 30*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatal("acquired = false, want true")
	}
	if leaseID == "" {
		t.Fatal("leaseID is empty, want a non-empty token")
	}
}

func TestAcquireHeldKeyIsContentionNotError(t *testing.T) {
	t.Parallel()
	locker := newLocker(&fakeClock{now: time.Unix(2000, 0)})

	if acquired, _, err := locker.Acquire("job:b", 30*time.Second); !acquired || err != nil {
		t.Fatalf("first acquire: acquired=%t err=%v, want true/nil", acquired, err)
	}

	acquired, leaseID, err := locker.Acquire("job:b", 30*time.Second)
	if acquired {
		t.Fatal("acquired = true, want false while the lock is still held")
	}
	if err != nil {
		t.Fatalf("contention must not be an error, got %v", err)
	}
	if leaseID != "" {
		t.Fatalf("leaseID = %q, want empty when not acquired", leaseID)
	}
}

func TestAcquireAfterExpiryGrantsANewLease(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{now: time.Unix(3000, 0)}
	locker := newLocker(clock)

	_, firstLease, err := locker.Acquire("job:c", 10*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	clock.Advance(11 * time.Second) // past the 10s TTL

	acquired, secondLease, err := locker.Acquire("job:c", 10*time.Second)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if !acquired {
		t.Fatal("acquired = false, want true once the previous lease expired")
	}
	if secondLease == firstLease {
		t.Fatal("expected a fresh lease id distinct from the expired one")
	}
}

func TestReleaseWithCorrectLeaseSucceeds(t *testing.T) {
	t.Parallel()
	locker := newLocker(&fakeClock{now: time.Unix(4000, 0)})

	_, leaseID, err := locker.Acquire("job:d", 30*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	released, err := locker.Release("job:d", leaseID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !released {
		t.Fatal("released = false, want true")
	}

	// The key is free again.
	acquired, _, err := locker.Acquire("job:d", 30*time.Second)
	if err != nil || !acquired {
		t.Fatalf("re-acquire after release: acquired=%t err=%v, want true/nil", acquired, err)
	}
}

func TestReleaseWithWrongLeaseFails(t *testing.T) {
	t.Parallel()
	locker := newLocker(&fakeClock{now: time.Unix(5000, 0)})

	_, leaseID, err := locker.Acquire("job:e", 30*time.Second)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	released, err := locker.Release("job:e", "some-other-token")
	if err == nil {
		t.Fatal("want an error when the lease token does not match the current holder")
	}
	if released {
		t.Fatal("released = true despite a mismatched lease token")
	}

	// The real holder can still release with the correct token.
	released, err = locker.Release("job:e", leaseID)
	if err != nil || !released {
		t.Fatalf("release with correct token: released=%t err=%v, want true/nil", released, err)
	}
}

func TestReleaseNeverHeldKeyIsIdempotentNoOp(t *testing.T) {
	t.Parallel()
	locker := newLocker(&fakeClock{now: time.Unix(6000, 0)})

	released, err := locker.Release("job:never-acquired", "any-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if released {
		t.Fatal("released = true, want false for a key that was never held")
	}
}
```

## Review

`Acquire` and `Release` are correct together when a lease token, once
issued, is the only thing that can ever release the lock it names — never
the key alone, never a guess, never "whoever calls `Release` first" wins.
`TestAcquireAfterExpiryGrantsANewLease` proves expiry actually rotates the
token rather than silently keeping the same one; `TestReleaseWithWrongLeaseFails`
is the load-bearing test for the whole exercise — it is the one assertion
that would fail if `Release` were simplified back down to `Release(key
string)`, the exact simplification that reintroduces the split-brain bug
this design exists to prevent.

The mistake to avoid is comparing lease tokens with anything other than
exact string equality — for example, checking only whether *a* lease token
was provided (`leaseID != ""`) instead of whether it matches the recorded
one. That would make `Release` effectively "release if anyone calls with
any non-empty string", which is no safer than not having a lease token at
all.

## Resources

- [Redis: Distributed locks with Redis](https://redis.io/docs/latest/develop/use/patterns/distributed-locks/) — the SET-NX-with-a-unique-value-then-compare-on-unlock pattern this exercise's lease token mirrors.
- [etcd: Lease](https://etcd.io/docs/latest/learning/api/#lease-api) — a production distributed lock service built around the same grant-a-token, expire-it, compare-on-release model.
- [Martin Kleppmann: How to do distributed locking](https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html) — why a naive lock-by-key-alone design breaks under process pauses and clock drift.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [34-tls-cert-verify-with-subject.md](34-tls-cert-verify-with-subject.md) | Next: [../02-named-return-values/00-concepts.md](../02-named-return-values/00-concepts.md)
