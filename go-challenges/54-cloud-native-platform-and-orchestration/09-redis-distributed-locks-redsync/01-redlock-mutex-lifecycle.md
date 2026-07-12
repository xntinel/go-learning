# Exercise 1: Acquire and Release a Redlock-Style Lock

A singleton-job guard is the canonical use of a distributed lock: one lock name,
many pods, and only the holder runs the work this tick. This exercise builds that
guard over `go-redis` and `redsync`, with a blocking `RunExclusive` path and a
non-blocking `TryAcquire` path, and — the part that separates a toy from
production code — it distinguishes contention (a peer holds the lock) from
infrastructure failure (Redis is unreachable).

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
lockguard/                 independent module: example.com/lockguard
  go.mod                   go 1.26; requires redsync, go-redis, miniredis
  lock.go                  Guard, NewGuard, RunExclusive, TryAcquire, Lease; ErrContended; classify
  cmd/
    demo/
      main.go              embedded miniredis; leader acquires, peer skips, re-acquire
  lock_test.go             miniredis tests: contention, ErrTaken, re-acquire, unlock status, TryLock immediacy
```

Files: `lock.go`, `cmd/demo/main.go`, `lock_test.go`.
Implement: a `Guard` that wraps `redsync` over a `go-redis` client; `RunExclusive` (blocking, runs `fn` only when the lock is genuinely held) and `TryAcquire` (one attempt, returns a `Lease`); a `classify` helper mapping redsync errors to `ErrContended` versus a wrapped infrastructure error.
Test: with `miniredis`, assert a second holder sees `ErrContended`/`*redsync.ErrTaken`, a released lock is re-acquirable, `Unlock` returns `(true,nil)` for the holder and `(false,ErrLockAlreadyExpired)` after expiry, and `TryAcquire` returns immediately under contention.
Verify: `go test -count=1 -race ./...`

Set up the module. This lesson uses external modules, so fetch them:

```bash
mkdir -p go-solutions/54-cloud-native-platform-and-orchestration/09-redis-distributed-locks-redsync/01-redlock-mutex-lifecycle/cmd/demo
cd go-solutions/54-cloud-native-platform-and-orchestration/09-redis-distributed-locks-redsync/01-redlock-mutex-lifecycle
go mod edit -go=1.26
go get github.com/go-redsync/redsync/v4@latest
go get github.com/redis/go-redis/v9@latest
go get github.com/alicebob/miniredis/v2@latest
```

### Wiring redsync over go-redis

redsync does not talk to Redis directly; it talks to a `redis.Pool` interface.
The `goredis` adapter wraps a `go-redis` v9 client into that interface:
`goredis.NewPool(client)` takes a `redis.UniversalClient` (which `*redis.Client`
satisfies) and returns a pool, and `redsync.New(pool)` builds the `*Redsync`
factory that mints mutexes. A single `Redsync` is safe to share; each named lock
is a `*Mutex` you create per acquisition, because a `Mutex` carries single-owner
state (its random value, its validity deadline) and is not itself
concurrency-safe.

The mutex options set the lease policy. `WithExpiry` is the TTL — the lease
window. `WithTries` and `WithRetryDelay` govern the blocking `LockContext`: it
attempts up to `tries` times, sleeping `retryDelay` between attempts, before
giving up. `TryLockContext` ignores `tries` entirely — it is `LockContext` with
`tries = 1`, so it returns immediately whether or not the lock was free, which is
what a "am I the leader this tick?" check wants.

### Contention is not failure

The single most important line of production code here is `classify`. When
`LockContext` fails, redsync tells you *why*. On a single node, when the key is
already held by a peer, the last attempt returns a `*redsync.ErrTaken` whose
`Nodes` field lists the contended node indices (with N nodes it can instead be
`redsync.ErrFailed` when the time budget was blown). Either way, that is normal
contention: back off and try later, or skip this tick. But if Redis is
unreachable, the error is a connection or timeout error — a degraded
infrastructure signal that should page someone, not be silently treated as "a
peer has the lock". `classify` maps the first case to a sentinel `ErrContended`
and wraps everything else as an infrastructure error, so callers can branch with
`errors.Is`. `errors.As(err, &taken)` uses a `*redsync.ErrTaken` target because
redsync returns a pointer to that struct.

Create `lock.go`:

```go
package lockguard

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-redsync/redsync/v4"
	goredis "github.com/go-redsync/redsync/v4/redis/goredis/v9"
	goredislib "github.com/redis/go-redis/v9"
)

// ErrContended reports that a peer currently holds the lock. It is a normal,
// expected outcome under contention, distinct from an infrastructure failure.
var ErrContended = errors.New("lockguard: lock held by a peer")

// Guard wraps a redsync instance and mints named mutexes with a shared lease
// policy. A single Guard is safe to share across goroutines; each acquisition
// gets its own *redsync.Mutex.
type Guard struct {
	rs     *redsync.Redsync
	expiry time.Duration
	tries  int
	delay  time.Duration
}

// GuardOption configures a Guard's lease policy.
type GuardOption func(*Guard)

// WithExpiry sets the lock TTL (the lease window).
func WithExpiry(d time.Duration) GuardOption { return func(g *Guard) { g.expiry = d } }

// WithTries sets how many times the blocking LockContext retries before failing.
func WithTries(n int) GuardOption { return func(g *Guard) { g.tries = n } }

// WithRetryDelay sets the pause between blocking-acquire retries.
func WithRetryDelay(d time.Duration) GuardOption { return func(g *Guard) { g.delay = d } }

// NewGuard builds a Guard over a go-redis client. Pass a single client for an
// efficiency lock, or point redsync at several independent masters for quorum.
func NewGuard(client goredislib.UniversalClient, opts ...GuardOption) *Guard {
	g := &Guard{
		rs:     redsync.New(goredis.NewPool(client)),
		expiry: 8 * time.Second,
		tries:  3,
		delay:  100 * time.Millisecond,
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

func (g *Guard) newMutex(name string) *redsync.Mutex {
	return g.rs.NewMutex(name,
		redsync.WithExpiry(g.expiry),
		redsync.WithTries(g.tries),
		redsync.WithRetryDelay(g.delay),
	)
}

// classify maps a redsync acquisition error to either ErrContended (a peer holds
// the lock) or a wrapped infrastructure error (Redis unreachable, timeout).
func classify(err error) error {
	var taken *redsync.ErrTaken
	if errors.Is(err, redsync.ErrFailed) || errors.As(err, &taken) {
		return ErrContended
	}
	return fmt.Errorf("lockguard: lock infrastructure error: %w", err)
}

// RunExclusive acquires the named lock (retrying up to the configured tries),
// runs fn while the lease is held, and releases the lock afterward. fn runs only
// when the lock was genuinely acquired. It returns ErrContended if a peer holds
// the lock, or a wrapped error if Redis is unreachable.
func (g *Guard) RunExclusive(ctx context.Context, name string, fn func(context.Context) error) error {
	m := g.newMutex(name)
	if err := m.LockContext(ctx); err != nil {
		return classify(err)
	}
	defer func() {
		// Best-effort release: a (false, err) result is legitimate if the lease
		// already expired, and must not mask fn's own error.
		_, _ = m.UnlockContext(ctx)
	}()
	return fn(ctx)
}

// Lease is a held lock. Create one per acquisition; do not share it.
type Lease struct {
	mu *redsync.Mutex
}

// TryAcquire attempts the lock exactly once (no retry loop) and returns a Lease
// if it was acquired. It returns ErrContended when a peer holds the lock, which
// is how a per-tick leader election decides "not me this tick".
func (g *Guard) TryAcquire(ctx context.Context, name string) (*Lease, error) {
	m := g.newMutex(name)
	if err := m.TryLockContext(ctx); err != nil {
		return nil, classify(err)
	}
	return &Lease{mu: m}, nil
}

// Until reports the instant the lease is currently valid until.
func (l *Lease) Until() time.Time { return l.mu.Until() }

// Release best-effort releases the lock. A (false, err) result is legitimate: it
// means the lease had already expired or a newer holder took over.
func (l *Lease) Release(ctx context.Context) (bool, error) {
	return l.mu.UnlockContext(ctx)
}
```

### The runnable demo

The demo starts an embedded `miniredis` so it runs with no external Redis and its
output is reproducible. A leader acquires the singleton lock; a peer tick then
finds it contended and skips its work; the leader releases, and the next tick can
acquire again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"example.com/lockguard"
	"github.com/alicebob/miniredis/v2"
	goredislib "github.com/redis/go-redis/v9"
)

func main() {
	mr, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	defer mr.Close()

	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	defer client.Close()

	ctx := context.Background()
	g := lockguard.NewGuard(client, lockguard.WithExpiry(2*time.Second), lockguard.WithTries(1))

	lease, err := g.TryAcquire(ctx, "singleton:cron")
	if err != nil {
		panic(err)
	}
	fmt.Println("acquired singleton lock")

	err = g.RunExclusive(ctx, "singleton:cron", func(context.Context) error {
		fmt.Println("peer ran work") // must not happen: the leader holds the lock
		return nil
	})
	if errors.Is(err, lockguard.ErrContended) {
		fmt.Println("peer skipped: lock held by leader")
	}

	ok, _ := lease.Release(ctx)
	fmt.Printf("leader released: ok=%v\n", ok)

	lease2, err := g.TryAcquire(ctx, "singleton:cron")
	if err != nil {
		panic(err)
	}
	fmt.Println("re-acquired after release")
	_, _ = lease2.Release(ctx)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquired singleton lock
peer skipped: lock held by leader
leader released: ok=true
re-acquired after release
```

### Tests

The tests use `miniredis`, an in-process Redis that speaks the real protocol
(including the `EVAL` Lua that redsync's unlock relies on) and exposes
`FastForward` to expire keys deterministically. `TestRunExclusiveUnderContention`
holds the lock, then asserts a second `RunExclusive` returns `ErrContended` and
never runs `fn`. `TestContentionSurfacesErrTaken` pins the underlying error type
with `errors.As`. `TestReacquireAfterUnlock` proves a released name is free
again. `TestUnlockStatus` checks the `(bool, error)` contract: `(true, nil)` for
the real holder, and `(false, redsync.ErrLockAlreadyExpired)` once the key has
expired. `TestTryAcquireDoesNotRetry` gives the guard a long retry delay and many
tries, then asserts `TryAcquire` still returns almost instantly under contention —
proof it takes the single-attempt path.

Create `lock_test.go`:

```go
package lockguard

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redsync/redsync/v4"
	goredislib "github.com/redis/go-redis/v9"
)

func newTestClient(t *testing.T) (*miniredis.Miniredis, *goredislib.Client) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

func TestRunExclusiveUnderContention(t *testing.T) {
	t.Parallel()
	_, client := newTestClient(t)
	ctx := context.Background()
	g := NewGuard(client, WithExpiry(2*time.Second), WithTries(1))

	held := g.newMutex("job:rebuild")
	if err := held.LockContext(ctx); err != nil {
		t.Fatalf("first LockContext: %v", err)
	}

	err := g.RunExclusive(ctx, "job:rebuild", func(context.Context) error {
		t.Fatal("fn ran while a peer held the lock")
		return nil
	})
	if !errors.Is(err, ErrContended) {
		t.Fatalf("RunExclusive under contention = %v; want ErrContended", err)
	}
}

func TestContentionSurfacesErrTaken(t *testing.T) {
	t.Parallel()
	_, client := newTestClient(t)
	ctx := context.Background()
	g := NewGuard(client, WithExpiry(2*time.Second), WithTries(1))

	first, err := g.TryAcquire(ctx, "k")
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	defer first.Release(ctx)

	m := g.newMutex("k")
	err = m.TryLockContext(ctx)
	var taken *redsync.ErrTaken
	if !errors.As(err, &taken) {
		t.Fatalf("contended TryLock err = %v; want *redsync.ErrTaken", err)
	}
	if len(taken.Nodes) == 0 {
		t.Fatal("ErrTaken.Nodes is empty; want the contended node index")
	}
}

func TestReacquireAfterUnlock(t *testing.T) {
	t.Parallel()
	_, client := newTestClient(t)
	ctx := context.Background()
	g := NewGuard(client, WithExpiry(2*time.Second), WithTries(1))

	lease, err := g.TryAcquire(ctx, "singleton")
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if _, err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	again, err := g.TryAcquire(ctx, "singleton")
	if err != nil {
		t.Fatalf("re-acquire after unlock = %v; want success", err)
	}
	_, _ = again.Release(ctx)
}

func TestUnlockStatus(t *testing.T) {
	t.Parallel()
	mr, client := newTestClient(t)
	ctx := context.Background()
	g := NewGuard(client, WithExpiry(2*time.Second), WithTries(1))

	lease, err := g.TryAcquire(ctx, "singleton")
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if !lease.Until().After(time.Now()) {
		t.Fatalf("Until() = %v; want an instant in the future", lease.Until())
	}
	ok, err := lease.Release(ctx)
	if !ok || err != nil {
		t.Fatalf("Release by holder = %v,%v; want true,nil", ok, err)
	}

	stale, err := g.TryAcquire(ctx, "singleton")
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	mr.FastForward(3 * time.Second) // past the 2s expiry: the key is gone

	ok, err = stale.Release(ctx)
	if ok {
		t.Fatalf("Release after expiry = true; want false")
	}
	if !errors.Is(err, redsync.ErrLockAlreadyExpired) {
		t.Fatalf("Release after expiry err = %v; want ErrLockAlreadyExpired", err)
	}
}

func TestTryAcquireDoesNotRetry(t *testing.T) {
	t.Parallel()
	_, client := newTestClient(t)
	ctx := context.Background()
	// Long retry delay and many tries: a blocking Lock would take seconds.
	g := NewGuard(client, WithExpiry(5*time.Second), WithTries(10), WithRetryDelay(time.Second))

	held := g.newMutex("hot")
	if err := held.LockContext(ctx); err != nil {
		t.Fatalf("hold: %v", err)
	}

	start := time.Now()
	_, err := g.TryAcquire(ctx, "hot")
	if !errors.Is(err, ErrContended) {
		t.Fatalf("TryAcquire = %v; want ErrContended", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("TryAcquire took %v; expected an immediate return (no retry loop)", elapsed)
	}
}

func Example() {
	mr, _ := miniredis.Run()
	defer mr.Close()
	client := goredislib.NewClient(&goredislib.Options{Addr: mr.Addr()})
	defer client.Close()

	g := NewGuard(client, WithExpiry(time.Second), WithTries(1))
	err := g.RunExclusive(context.Background(), "cron:nightly", func(context.Context) error {
		fmt.Println("running exclusive work")
		return nil
	})
	fmt.Println("run error:", err)
	// Output:
	// running exclusive work
	// run error: <nil>
}
```

## Review

The guard is correct when `fn` runs only under a genuinely held lock and when the
two failure modes stay separated. Confirm the split: hold the lock, then assert
`RunExclusive` returns `ErrContended` and never enters `fn`; that is the
efficiency-lock behavior you want on a per-tick leader. The `Unlock` contract is
the second thing to get right — a `false` after expiry is not an error to hide, it
is the lease telling you it was already gone, and the test asserts exactly
`redsync.ErrLockAlreadyExpired` there. The common mistakes are calling the
retrying `LockContext` where `TryLockContext` was wanted (the timing test guards
against that), sharing one `*Mutex` across goroutines (each acquisition mints its
own), and folding a Redis outage into `ErrContended` (only `ErrFailed`/`ErrTaken`
map to contention; everything else is wrapped as infrastructure). Run
`go test -race` to shake out any accidental sharing of mutex state.

## Resources

- [go-redsync/redsync — README and API](https://github.com/go-redsync/redsync)
- [redsync v4 package reference](https://pkg.go.dev/github.com/go-redsync/redsync/v4)
- [Distributed Locks with Redis — official docs](https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/)
- [miniredis v2 — in-process Redis for Go tests](https://pkg.go.dev/github.com/alicebob/miniredis/v2)

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-lease-renewal-watchdog.md](02-lease-renewal-watchdog.md)
