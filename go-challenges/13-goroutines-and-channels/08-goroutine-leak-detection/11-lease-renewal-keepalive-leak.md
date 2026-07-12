# Exercise 11: A Distributed Lock Whose Keepalive Goroutine Never Leaks

**Level: Intermediate**

A distributed lock (etcd or Redis style) holds a lease and keeps it alive with a
background renewal goroutine that heartbeats on a cadence for as long as the lock is
held. `Acquire` returns a `Handle`; the caller must `Release` to unlock. The
production hazard is that every `Acquire` starts one renewal goroutine plus a timer,
so on a hot acquire/release path a `Release` that only signals stop -- or a renewal
loop with no reachable exit -- leaks one goroutine per lock taken until the pod OOMs.
This exercise builds the lock so `Release` deterministically cancels the renewal loop
and joins it, and proves the goroutine count stays flat across thousands of cycles.

This module is self-contained: its own module, a `leaselock` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
leaselock/                   independent module: example.com/leaselock
  go.mod                     go 1.26
  leaselock.go               Lock, Handle, Acquire/AcquireLeaky/Release/Err, ErrHeld
  cmd/demo/main.go           runnable demo: acquire, heartbeat, release, idempotency
  leaselock_test.go          flat-across-1000, join-not-signal, leaky-reproduced, ErrHeld
```

- Files: `leaselock.go`, `cmd/demo/main.go`, `leaselock_test.go`.
- Implement: `New(store, ttl) *Lock`, `(*Lock).Acquire(ctx, key) (*Handle, error)`, the deliberately buggy `(*Lock).AcquireLeaky`, `(*Handle).Release() error` (idempotent, joins), `(*Handle).Err() <-chan error`, and `var ErrHeld`.
- Test: 1000 acquire/release cycles stay goleak-clean and return to baseline; `Release` joins rather than signals; `Renew` is called before `Release`; `AcquireLeaky` reproduces a real leak then is cleaned up; double `Release` is a no-op; an already-held key returns `ErrHeld` and starts no second loop.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/11-lease-renewal-keepalive-leak/cmd/demo
cd go-solutions/13-goroutines-and-channels/08-goroutine-leak-detection/11-lease-renewal-keepalive-leak
go get go.uber.org/goleak
```

Set the `go` directive in `go.mod` to `go 1.26`.

### The ownership rule: the loop's lifetime belongs to the Handle, not the request

A lease renewal loop is a `for` loop that never ends on its own, so its exit must come
from outside -- and the question that decides correctness is *who owns that exit*. Two
owners are wrong. The first is *nobody*: a bare `for range ticker.C` loop with no stop
case runs until the process dies, so a service that acquires and releases a lock per
request accumulates one dead goroutine per request. The second is the *caller's
context*: binding the loop to the `ctx` passed into `Acquire` looks defensive, but on
the hot path callers pass `context.Background()`, whose `Done()` channel is `nil`, and a
`select` on a `nil` channel blocks that case forever -- the "exit" is unreachable and
`Release` has no handle on the caller's context to fix it.

The right owner is the **Handle**. `Acquire` derives a fresh context from
`context.Background()`, stores its `cancel` on the `Handle`, and starts the loop
watching *that* context's `Done()`. `Release` then owns the exit completely:

1. Call `cancel()` to make the loop's `ctx.Done()` case fire.
2. Receive on `done`, which the loop closes with `defer` as it returns. This is the
   JOIN: `Release` does not return until the goroutine has actually finished, so the
   moment `Release` returns there is provably no keepalive left running.
3. Drop the key from the held set so it can be reacquired.

Signalling stop is not the same as joining. A `Release` that calls `cancel()` and
returns immediately passes a functional test but leaves the goroutine mid-flight; a
leak check run right after still sees it, and the lease may still be renewed once more
after "release." Joining on `done` is what makes shutdown deterministic. `sync.Once`
wraps the whole sequence so a double `Release` is a safe no-op -- calling `cancel()`
twice is fine, but receiving on an already-drained `done` twice would block forever.

The loop reuses a single `time.Ticker` and never calls `time.After` per iteration: a
per-iteration `time.After` allocates a `Timer` that outlives the tick, so a hot loop
accumulates timer heap even when the goroutine count looks flat. One `Ticker`, stopped
with `defer`, is the fixed-cost shape.

Create `leaselock.go`:

```go
package leaselock

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrHeld is returned by Acquire when the key is already locked by a live handle.
var ErrHeld = errors.New("leaselock: key already held")

// Store is the backing lease store (etcd, Redis, ...). Renew extends the lease
// on key for another ttl; a non-nil error means the lease was lost.
type Store interface {
	Renew(ctx context.Context, key string, ttl time.Duration) error
}

// Lock hands out Handles that each own one lease-renewal goroutine. The held map
// records which keys currently have a live handle so a second Acquire fails fast
// instead of starting a competing renewal loop.
type Lock struct {
	store Store
	ttl   time.Duration

	mu   sync.Mutex
	held map[string]struct{}
}

// New builds a Lock over store. Each held key is heartbeated on a cadence derived
// from ttl (renew at ttl/2 so a single missed tick does not expire the lease).
func New(store Store, ttl time.Duration) *Lock {
	return &Lock{
		store: store,
		ttl:   ttl,
		held:  make(map[string]struct{}),
	}
}

// Handle owns exactly one renewal goroutine. Release cancels that goroutine's
// context and JOINS it (waits for done) so no keepalive outlives the lock.
type Handle struct {
	lock *Lock
	key  string

	cancel context.CancelFunc
	done   chan struct{}
	errc   chan error
	once   sync.Once
}

// cadence returns the heartbeat interval: ttl/2, floored so tiny ttls still tick.
func (l *Lock) cadence() time.Duration {
	d := l.ttl / 2
	if d <= 0 {
		d = time.Millisecond
	}
	return d
}

// Acquire records the key as held and starts a renewal loop with a guaranteed
// exit path. It returns ErrHeld if the key is already held. The returned Handle
// must be Released.
func (l *Lock) Acquire(ctx context.Context, key string) (*Handle, error) {
	l.mu.Lock()
	if _, ok := l.held[key]; ok {
		l.mu.Unlock()
		return nil, ErrHeld
	}
	l.held[key] = struct{}{}
	l.mu.Unlock()

	// The renewal context is derived from context.Background, not the caller's
	// ctx: the loop's lifetime is bound to the Handle (Release), not to whatever
	// request happened to acquire the lock.
	rctx, cancel := context.WithCancel(context.Background())
	h := &Handle{
		lock:   l,
		key:    key,
		cancel: cancel,
		done:   make(chan struct{}),
		errc:   make(chan error, 1),
	}

	go h.renew(rctx, l.store, key, l.ttl, l.cadence())
	return h, nil
}

// AcquireLeaky is the buggy version kept for the leak test. Its renewal loop is
// bound to the CALLER's context instead of the handle. On the hot acquire/release
// path callers pass context.Background(), whose Done() channel is nil, so the
// select's exit case can never fire -- the loop has no reachable exit and leaks
// one goroutine per call. Release cannot stop it, because Release cancels the
// handle's own context, which this loop never watches. Do not use in real code.
func (l *Lock) AcquireLeaky(ctx context.Context, key string) (*Handle, error) {
	l.mu.Lock()
	if _, ok := l.held[key]; ok {
		l.mu.Unlock()
		return nil, ErrHeld
	}
	l.held[key] = struct{}{}
	l.mu.Unlock()

	_, cancel := context.WithCancel(context.Background())
	h := &Handle{
		lock:   l,
		key:    key,
		cancel: cancel,
		done:   make(chan struct{}),
		errc:   make(chan error, 1),
	}
	// BUG: done is closed up front so Release's join returns instantly and
	// falsely reports a clean stop while the goroutine below keeps running.
	close(h.done)

	cadence := l.cadence()
	store, ttl := l.store, l.ttl
	go func() {
		ticker := time.NewTicker(cadence)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done(): // bound to the caller ctx; nil-Done on the hot path
				return
			case <-ticker.C:
				_ = store.Renew(context.Background(), key, ttl)
			}
		}
	}()
	return h, nil
}

// renew is the correct renewal loop. It reuses a single Ticker (never time.After
// per iteration) and exits on ctx.Done(), closing done last so Release can join.
func (h *Handle) renew(ctx context.Context, store Store, key string, ttl, cadence time.Duration) {
	defer close(h.done)

	ticker := time.NewTicker(cadence)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.Renew(ctx, key, ttl); err != nil {
				// Surface the failure without blocking: errc is buffered(1).
				select {
				case h.errc <- err:
				default:
				}
				return
			}
		}
	}
}

// Release cancels the renewal goroutine's context and waits for it to finish,
// then drops the key from the held set. It is idempotent: a second call is a
// no-op. Release is the only correct way to stop a Handle's keepalive.
func (h *Handle) Release() error {
	h.once.Do(func() {
		h.cancel()
		<-h.done // JOIN: do not return until the renewal goroutine has exited.
		h.lock.mu.Lock()
		delete(h.lock.held, h.key)
		h.lock.mu.Unlock()
	})
	return nil
}

// Err surfaces a renewal failure reported by the loop. It is buffered so a lost
// lease is observable after the fact even if no one was selecting at the time.
func (h *Handle) Err() <-chan error { return h.errc }
```

### The runnable demo

The demo uses a store that pings a channel on its first `Renew`, so it can wait for a
genuine heartbeat before releasing instead of sleeping. Every line prints a boolean,
so the output is fully deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"example.com/leaselock"
)

// signalStore counts renewals and pings a channel on the first one so the demo
// can wait for a real heartbeat deterministically instead of sleeping.
type signalStore struct {
	renews atomic.Int64
	first  chan struct{}
}

func (s *signalStore) Renew(ctx context.Context, key string, ttl time.Duration) error {
	if s.renews.Add(1) == 1 {
		close(s.first)
	}
	return nil
}

func main() {
	store := &signalStore{first: make(chan struct{})}
	lock := leaselock.New(store, 20*time.Millisecond)
	ctx := context.Background()

	h, err := lock.Acquire(ctx, "orders")
	fmt.Println("acquire ok:", err == nil)

	// Second acquire of a held key must fail fast and start no loop.
	_, err2 := lock.Acquire(ctx, "orders")
	fmt.Println("second acquire returns ErrHeld:", errors.Is(err2, leaselock.ErrHeld))

	// Wait for a genuine heartbeat, then release.
	<-store.first
	fmt.Println("renewed at least once before release:", store.renews.Load() >= 1)

	fmt.Println("release ok:", h.Release() == nil)
	fmt.Println("double release is a no-op:", h.Release() == nil)

	// After release the key is free again.
	h2, err3 := lock.Acquire(ctx, "orders")
	fmt.Println("re-acquire after release ok:", err3 == nil)
	fmt.Println("final release ok:", h2.Release() == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquire ok: true
second acquire returns ErrHeld: true
renewed at least once before release: true
release ok: true
double release is a no-op: true
re-acquire after release ok: true
final release ok: true
```

### Tests

`TestMain` installs `goleak.VerifyTestMain(m)` so the whole package is checked for
stray goroutines after every test finishes. `TestAcquireReleaseFlat` runs 1000
acquire/release cycles and polls the goroutine count back to its pre-loop baseline --
`NumGoroutine` is racy, so it polls with `runtime.GC()` rather than asserting equality
once. `TestRenewCalledBeforeRelease` proves the loop actually heartbeats by waiting for
a non-zero renew count before releasing. `TestReleaseJoinsRenewLoop` is the sharpest:
it parks the loop inside `Store.Renew`, then shows `Release` blocks until the loop can
finish -- a signal-only `Release` would return early and fail. `TestDoubleReleaseNoOp`
pins idempotency, `TestAlreadyHeldStartsNoSecondLoop` pins `ErrHeld` with a nil handle,
`TestRenewFailureSurfaced` pins `Err()`, and `TestAcquireLeakyLeaks` reproduces the
bug as a real leak with `goleak.Find(IgnoreCurrent())`, then cancels the caller context
to clean it up so `VerifyTestMain` stays green.

Create `leaselock_test.go`:

```go
package leaselock

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// countingStore records how many times Renew was called and can optionally fail.
type countingStore struct {
	renews atomic.Int64
	fail   atomic.Bool
}

func (s *countingStore) Renew(ctx context.Context, key string, ttl time.Duration) error {
	s.renews.Add(1)
	if s.fail.Load() {
		return errors.New("lease lost")
	}
	return nil
}

// waitBaseline polls NumGoroutine back down to base within a deadline. NumGoroutine
// is racy -- it counts goroutines mid-exit -- so a single-shot equality would flake.
func waitBaseline(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if runtime.NumGoroutine() <= base {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("goroutines did not return to baseline %d, current=%d", base, runtime.NumGoroutine())
}

// TestAcquireReleaseFlat runs many acquire/release cycles and proves the goroutine
// count returns to baseline. With VerifyTestMain, this pins steady-state leak-freedom.
func TestAcquireReleaseFlat(t *testing.T) {
	store := &countingStore{}
	lock := New(store, 4*time.Millisecond)
	ctx := context.Background()

	runtime.GC()
	base := runtime.NumGoroutine()

	for i := range 1000 {
		h, err := lock.Acquire(ctx, "k")
		if err != nil {
			t.Fatalf("cycle %d: Acquire: %v", i, err)
		}
		if err := h.Release(); err != nil {
			t.Fatalf("cycle %d: Release: %v", i, err)
		}
	}

	waitBaseline(t, base)
}

// TestRenewCalledBeforeRelease proves the renewal loop actually heartbeats: it
// waits for at least one Renew before releasing.
func TestRenewCalledBeforeRelease(t *testing.T) {
	store := &countingStore{}
	lock := New(store, 4*time.Millisecond)

	h, err := lock.Acquire(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()

	deadline := time.Now().Add(2 * time.Second)
	for store.renews.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("renewal loop never called Store.Renew")
		}
		time.Sleep(time.Millisecond)
	}
}

// blockingStore parks the renewal goroutine inside Renew until gate is closed,
// after signaling entered exactly once. It lets a test observe that Release JOINS
// (blocks until the loop finishes) rather than merely signaling stop and returning.
type blockingStore struct {
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (s *blockingStore) Renew(ctx context.Context, key string, ttl time.Duration) error {
	s.once.Do(func() { close(s.entered) })
	<-s.gate
	return nil
}

// TestReleaseJoinsRenewLoop proves Release joins, not just signals: while the loop
// is parked inside Renew, Release must block; only once the loop can finish does
// Release return. A signal-and-return Release would return early and fail here.
func TestReleaseJoinsRenewLoop(t *testing.T) {
	store := &blockingStore{entered: make(chan struct{}), gate: make(chan struct{})}
	lock := New(store, time.Millisecond)

	h, err := lock.Acquire(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	<-store.entered // the renewal goroutine is now parked inside Store.Renew

	released := make(chan struct{})
	go func() { _ = h.Release(); close(released) }()

	select {
	case <-released:
		t.Fatal("Release returned while the renewal goroutine was still running (signal, not join)")
	case <-time.After(100 * time.Millisecond):
		// Still blocked in the join, as required.
	}

	close(store.gate) // let Renew return; the loop then sees ctx.Done() and exits

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("Release never returned after the loop could exit")
	}
}

// TestDoubleReleaseNoOp proves Release is idempotent via sync.Once and that the
// key is reacquirable afterwards.
func TestDoubleReleaseNoOp(t *testing.T) {
	store := &countingStore{}
	lock := New(store, 4*time.Millisecond)

	h, err := lock.Acquire(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}

	h2, err := lock.Acquire(context.Background(), "k")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	_ = h2.Release()
}

// TestAlreadyHeldStartsNoSecondLoop proves a duplicate Acquire returns ErrHeld and
// a nil handle -- the fast-fail path returns before any goroutine is started.
func TestAlreadyHeldStartsNoSecondLoop(t *testing.T) {
	store := &countingStore{}
	lock := New(store, 4*time.Millisecond)

	h, err := lock.Acquire(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()

	h2, err := lock.Acquire(context.Background(), "k")
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("second Acquire err = %v, want ErrHeld", err)
	}
	if h2 != nil {
		t.Fatal("second Acquire returned a non-nil handle")
	}
}

// TestRenewFailureSurfaced proves a Store.Renew error reaches Err().
func TestRenewFailureSurfaced(t *testing.T) {
	store := &countingStore{}
	store.fail.Store(true)
	lock := New(store, 4*time.Millisecond)

	h, err := lock.Acquire(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()

	select {
	case e := <-h.Err():
		if e == nil {
			t.Fatal("expected a non-nil renewal error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("renewal failure was never surfaced on Err()")
	}
}

// TestAcquireLeakyLeaks reproduces the buggy version as a real, detectable leak
// with goleak.Find(IgnoreCurrent()), then cleans it up by cancelling the context
// the loop was (wrongly) bound to -- so VerifyTestMain still passes.
func TestAcquireLeakyLeaks(t *testing.T) {
	store := &countingStore{}
	lock := New(store, 4*time.Millisecond)

	ignore := goleak.IgnoreCurrent()

	// A cancellable ctx stands in for the caller. In production the hot path
	// passes context.Background(), whose nil Done() makes this an unstoppable leak.
	ctx, cancel := context.WithCancel(context.Background())
	h, err := lock.AcquireLeaky(ctx, "leaky")
	if err != nil {
		t.Fatal(err)
	}
	// Release reports success but cannot actually stop the caller-ctx-bound loop.
	_ = h.Release()

	var found error
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if found = goleak.Find(ignore); found != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("expected AcquireLeaky to leak a renewal goroutine, but goleak.Find found none")
	}

	// Clean up: cancel the ctx the loop was bound to so it can exit.
	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if goleak.Find(ignore) == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("leaked goroutine did not exit after cancel: %v", goleak.Find(ignore))
}
```

## Review

"Correct" here means: for every `Acquire` that returns a `Handle`, a matching `Release`
provably ends the renewal goroutine before it returns, and the process holds no more
goroutines afterward than before. The invariant that guarantees it is single ownership:
the loop watches a context whose `cancel` lives on the `Handle`, and `Release` both
fires that cancel and joins on the `done` channel the loop closes on its way out.
`TestReleaseJoinsRenewLoop` proves the join is real by parking the loop and showing
`Release` refuses to return until it can finish; `TestAcquireReleaseFlat` under
`goleak.VerifyTestMain` proves the count stays flat across a thousand cycles; and
`TestAcquireLeakyLeaks` proves the detector actually catches the bug by reproducing it.
The production bug this prevents is the slow-burn keepalive leak: a lock whose renewal
loop is bound to the caller's request context (nil `Done()` on the hot path) or whose
`Release` signals stop without joining leaks one goroutine per lock taken, invisible to
every functional test, until RSS climbs and the pod OOM-kills with no request to blame.

## Resources

- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) -- `VerifyTestMain`, `Find`, and `IgnoreCurrent`, the detector this exercise uses to prove leak-freedom.
- [`context.WithCancel`](https://pkg.go.dev/context#WithCancel) -- the cancel/Done pair that gives the renewal loop a reachable exit owned by the Handle.
- [`time.NewTicker`](https://pkg.go.dev/time#NewTicker) -- the single reused ticker that replaces per-iteration `time.After` and its accumulating timers.
- [`sync.Once`](https://pkg.go.dev/sync#Once) -- makes Release idempotent so a double unlock cannot double-join a drained channel.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-pprof-goroutine-dump-endpoint.md](10-pprof-goroutine-dump-endpoint.md) | Next: [12-outbox-relay-join-inflight-dispatch.md](12-outbox-relay-join-inflight-dispatch.md)
