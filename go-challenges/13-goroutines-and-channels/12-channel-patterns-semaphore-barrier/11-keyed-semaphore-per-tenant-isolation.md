# Exercise 11: Per-Tenant Keyed Semaphore for Webhook Fan-Out Isolation

**Level: Intermediate**

A multi-tenant webhook service fans out events to customer endpoints. Guard the
whole fan-out with a single shared concurrency cap and one high-volume tenant
fills every slot, starving everyone else's deliveries behind it — a noisy-neighbor
outage that looks like a global slowdown. The fix is a *keyed* limiter: each
tenant gets its own counting semaphore, so its in-flight deliveries are bounded
independently and a hot tenant can never block a quiet one. This exercise builds
that limiter — a `map[string]chan struct{}` created lazily under a mutex — and
proves the per-key cap holds under `-race`.

This module is self-contained: its own module, a `keyedsem` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
keyedsem/                    independent module: example.com/keyedsem
  go.mod                     go 1.26
  keyedsem.go                Limiter: New, Acquire, Do, InFlight — one semaphore per key
  cmd/demo/main.go           runnable demo: a saturated hot tenant does not block a quiet one
  keyedsem_test.go           per-key peak <= cap, cross-key isolation, cancellation, release balance
```

- Files: `keyedsem.go`, `cmd/demo/main.go`, `keyedsem_test.go`.
- Implement: `New(perKey int) *Limiter`; `Acquire(ctx, key) (release func(), err error)`; `Do(ctx, key, fn) error`; `InFlight(key) int`.
- Test: each key's observed peak concurrency stays `<= perKey`; a saturated key does not block a different key; a saturated key with a cancelled context returns `ctx.Err()` and no release; `InFlight` returns to 0 after every release.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.26
```

### One semaphore per key, created lazily under a mutex

A buffered `chan struct{}` of capacity `n` is a counting semaphore: a send takes
a slot and blocks when full, a receive frees one. A *keyed* limiter is a map from
key to one such channel. The whole design rests on three rules.

1. **Lazy creation is the one place keys race.** The first `Acquire` for a key
   must create its semaphore; concurrent first-touches for the same key must not
   create two. So the map is guarded by a mutex, and `semFor` does a
   check-create-return under the lock. The lock is held only long enough to look
   up or insert the channel — never across the blocking send. Sending into the
   channel while holding the map lock would serialize every tenant behind one
   mutex and defeat the entire point.

2. **The blocking wait selects on the context.** Behind a webhook worker the
   downstream can be slow and a key can stay full. A bare send would park a
   goroutine forever after the caller gave up. `Acquire` therefore selects on the
   send *and* `ctx.Done()`, returning `ctx.Err()` with no slot taken when the
   context ends first. When the key is already saturated the send cannot proceed,
   so a cancelled context wins deterministically — that is what the cancellation
   test pins down.

3. **Release frees exactly one slot, and only once.** `Acquire` returns a closure
   that receives from the channel. Wrapping that receive in a `sync.Once` makes it
   idempotent: a double `release()` — easy to write with a `defer` plus an early
   manual call — cannot drain a slot the caller no longer holds and hand a phantom
   permit to another tenant. Balance is the invariant: release exactly as many
   times as you acquired, and `InFlight` (the channel's occupancy) returns to 0.

`InFlight(key)` reads `len(sem)`, the number of buffered elements, which equals
acquisitions not yet released. It reports 0 for a key never seen, so callers can
probe any tenant without materializing its semaphore.

Create `keyedsem.go`:

```go
// Package keyedsem bounds concurrency per key instead of globally. Each key
// gets its own counting semaphore (a buffered chan struct{} of capacity perKey),
// so a hot key saturating its own limiter cannot starve a quiet key.
package keyedsem

import (
	"context"
	"sync"
)

// Limiter hands each key an independent counting semaphore of capacity perKey.
// Semaphores are created lazily under mu on first use of a key.
type Limiter struct {
	mu     sync.Mutex
	perKey int
	sems   map[string]chan struct{}
}

// New returns a Limiter that bounds each key at perKey concurrent slots.
// A perKey below 1 is clamped to 1.
func New(perKey int) *Limiter {
	if perKey < 1 {
		perKey = 1
	}
	return &Limiter{perKey: perKey, sems: make(map[string]chan struct{})}
}

// semFor returns key's semaphore, creating it on first use. The map is the only
// shared mutable state; the channel itself is safe for concurrent send/receive.
func (l *Limiter) semFor(key string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sems[key]
	if !ok {
		s = make(chan struct{}, l.perKey)
		l.sems[key] = s
	}
	return s
}

// Acquire blocks until a slot for key is free or ctx is done. On success it
// returns a release that frees exactly one slot (idempotent: extra calls are
// no-ops, so it is safe to defer). On cancellation it returns a nil release and
// ctx.Err(), having acquired nothing.
func (l *Limiter) Acquire(ctx context.Context, key string) (release func(), err error) {
	sem := l.semFor(key)
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Do acquires a slot for key, runs fn, then releases. It returns ctx.Err() if it
// could not acquire; fn is not run in that case.
func (l *Limiter) Do(ctx context.Context, key string, fn func()) error {
	release, err := l.Acquire(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	fn()
	return nil
}

// InFlight reports the number of slots currently held for key, or 0 if the key
// has never been seen. It reads the semaphore's occupancy (len of the buffered
// channel), which equals acquisitions not yet released.
func (l *Limiter) InFlight(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.sems[key]
	if !ok {
		return 0
	}
	return len(s)
}
```

### The runnable demo

The demo saturates a hot tenant by holding both of its slots, then shows a quiet
tenant still delivers, that the hot tenant cannot grab a third slot under a
cancelled context, and that releasing drains it back to zero. Every step is
sequential, so the output is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/keyedsem"
)

func main() {
	l := keyedsem.New(2)
	ctx := context.Background()

	// Saturate the hot tenant by holding both of its slots.
	relHot1, _ := l.Acquire(ctx, "hot")
	relHot2, _ := l.Acquire(ctx, "hot")
	fmt.Printf("hot saturated: inflight=%d\n", l.InFlight("hot"))

	// A quiet tenant is unaffected: its own semaphore is still empty.
	err := l.Do(ctx, "quiet", func() {
		fmt.Printf("quiet delivered while hot is full: inflight=%d\n", l.InFlight("quiet"))
	})
	fmt.Printf("quiet Do err=%v\n", err)

	// The hot tenant cannot grab a third slot; an already-cancelled context
	// returns immediately with no slot acquired.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	rel, err := l.Acquire(cctx, "hot")
	fmt.Printf("hot third acquire: release==nil=%t err=%v\n", rel == nil, err)

	// Release the held slots; the hot tenant drains back to zero.
	relHot1()
	relHot2()
	fmt.Printf("hot drained: inflight=%d\n", l.InFlight("hot"))
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
hot saturated: inflight=2
quiet delivered while hot is full: inflight=1
quiet Do err=<nil>
hot third acquire: release==nil=true err=context canceled
hot drained: inflight=0
```

### Tests

`TestPerKeyPeakUnderRace` fans out 50 deliveries per tenant across two tenants
with `perKey=2`, tracking each tenant's live and peak concurrency with atomics,
and asserts each tenant's peak stayed `<= 2` — the trustworthy proof under `-race`
that the cap is per key, not global. `TestIsolationAcrossKeys` fills tenant A to
capacity by holding both its releases and asserts tenant B's `Acquire` still
succeeds, which a shared cap would block. `TestCancellationWhenSaturated`
saturates A, acquires A with an already-cancelled context, and asserts
`context.Canceled` with a nil release and no slot taken. `TestReleaseBalance`
acquires across keys, releases every held slot, and asserts every key's
`InFlight` returns to 0, including under a redundant double release.

Create `keyedsem_test.go`:

```go
package keyedsem

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPerKeyPeakUnderRace fans out many deliveries across two tenants and, via an
// atomic per-key peak tracker, asserts each tenant's observed peak concurrency
// stays within perKey. The cap is per key, not global, so both tenants may run
// perKey deliveries at once without either exceeding its own bound.
func TestPerKeyPeakUnderRace(t *testing.T) {
	t.Parallel()

	const perKey = 2
	l := New(perKey)
	tenants := []string{"A", "B"}

	live := map[string]*atomic.Int64{"A": {}, "B": {}}
	peak := map[string]*atomic.Int64{"A": {}, "B": {}}

	var wg sync.WaitGroup
	for _, tenant := range tenants {
		for range 50 {
			wg.Go(func() {
				_ = l.Do(context.Background(), tenant, func() {
					cur := live[tenant].Add(1)
					for {
						old := peak[tenant].Load()
						if cur <= old || peak[tenant].CompareAndSwap(old, cur) {
							break
						}
					}
					// Widen the overlap window so real concurrency is exercised.
					// The assertion below is a hard invariant, not timing-based.
					time.Sleep(time.Millisecond)
					live[tenant].Add(-1)
				})
			})
		}
	}
	wg.Wait()

	for _, tenant := range tenants {
		if pk := peak[tenant].Load(); pk > perKey {
			t.Fatalf("tenant %s peak concurrency = %d, want <= %d", tenant, pk, perKey)
		}
	}
}

// TestIsolationAcrossKeys fills tenant A to capacity by holding both its releases
// and asserts tenant B's Acquire still succeeds, because the two tenants own
// independent semaphores. A shared global cap would block B here.
func TestIsolationAcrossKeys(t *testing.T) {
	t.Parallel()

	l := New(2)
	ctx := context.Background()

	relA1, err := l.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire A #1 err = %v, want nil", err)
	}
	relA2, err := l.Acquire(ctx, "A")
	if err != nil {
		t.Fatalf("Acquire A #2 err = %v, want nil", err)
	}
	if got := l.InFlight("A"); got != 2 {
		t.Fatalf("InFlight(A) = %d, want 2", got)
	}

	// B has its own semaphore; a generous deadline turns a broken (shared-cap)
	// implementation into a fast failure instead of a hang.
	bctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	relB, err := l.Acquire(bctx, "B")
	if err != nil {
		t.Fatalf("Acquire B while A full err = %v, want nil (independent semaphores)", err)
	}
	relB()

	relA1()
	relA2()
}

// TestCancellationWhenSaturated saturates tenant A, then acquires A with an
// already-cancelled context: the send cannot proceed, so Acquire returns
// ctx.Err() and no release.
func TestCancellationWhenSaturated(t *testing.T) {
	t.Parallel()

	l := New(2)
	ctx := context.Background()
	relA1, _ := l.Acquire(ctx, "A")
	relA2, _ := l.Acquire(ctx, "A")
	defer relA1()
	defer relA2()

	cctx, cancel := context.WithCancel(ctx)
	cancel()

	rel, err := l.Acquire(cctx, "A")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire err = %v, want context.Canceled", err)
	}
	if rel != nil {
		t.Fatalf("Acquire returned non-nil release on cancellation")
	}
	if got := l.InFlight("A"); got != 2 {
		t.Fatalf("InFlight(A) = %d after failed acquire, want 2 (no slot taken)", got)
	}
}

// TestReleaseBalance acquires across several keys, releases every held slot, and
// asserts each key's InFlight returns to 0 — the balance invariant.
func TestReleaseBalance(t *testing.T) {
	t.Parallel()

	l := New(3)
	ctx := context.Background()
	keys := []string{"A", "B", "A", "C", "A"}

	var releases []func()
	for _, k := range keys {
		rel, err := l.Acquire(ctx, k)
		if err != nil {
			t.Fatalf("Acquire %s err = %v, want nil", k, err)
		}
		releases = append(releases, rel)
	}
	if got := l.InFlight("A"); got != 3 {
		t.Fatalf("InFlight(A) = %d, want 3", got)
	}

	for _, rel := range releases {
		rel()
	}
	for _, k := range []string{"A", "B", "C"} {
		if got := l.InFlight(k); got != 0 {
			t.Fatalf("InFlight(%s) = %d after release, want 0", k, got)
		}
	}

	// Release is idempotent: a second call must not drain a slot it does not hold.
	releases[0]()
	if got := l.InFlight("A"); got != 0 {
		t.Fatalf("InFlight(A) = %d after double release, want 0", got)
	}
}
```

## Review

The limiter is correct when each key's peak concurrency never exceeds `perKey`,
one saturated key cannot block a different key, a cancelled acquire on a full key
takes no slot, and `InFlight` balances to 0 after releases. The guarantee comes
from the structure: one buffered `chan struct{}` per key means the cap is
enforced by the channel's own capacity, the mutex protects only lazy creation of
the map entry (never the blocking send, so tenants do not serialize behind one
lock), the `select` on `ctx.Done()` makes the wait cancellable, and the
`sync.Once`-wrapped receive keeps release balanced even under a double call.
`TestPerKeyPeakUnderRace` proves the cap holds per key with an atomic peak tracker
under `-race`; `TestIsolationAcrossKeys` proves a hot tenant does not starve a
quiet one; the cancellation and balance tests pin the two invariants that make it
safe behind a request. The production bug this prevents is the noisy-neighbor
outage: a single global semaphore lets one high-volume tenant hold every slot and
stall every other tenant's webhooks behind it, with no error to point at — a
keyed semaphore isolates the blast radius to the tenant causing it.

## Resources

- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels as counting semaphores, the primitive keyed here.
- [pkg.go.dev: sync.Once](https://pkg.go.dev/sync#Once) — makes the returned release idempotent so a double call cannot corrupt the slot count.
- [pkg.go.dev: context](https://pkg.go.dev/context) — the cancellation contract that lets a request-scoped Acquire return instead of leaking a goroutine.
- [Go Blog: Pipelines and cancellation](https://go.dev/blog/pipelines) — fan-out with bounded concurrency and context cancellation in production shape.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [10-semaphore-vs-workerpool-tradeoffs.md](10-semaphore-vs-workerpool-tradeoffs.md) | Next: [12-admission-control-inflight-limiter.md](12-admission-control-inflight-limiter.md)
