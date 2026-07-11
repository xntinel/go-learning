# Exercise 28: Capacity Gater with Backpressure Signaling

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A worker pool, a connection pool, an in-flight-request cap — all of them
are the same problem: at most N callers may hold a resource at once, and
the (N+1)th caller needs to know that immediately, not discover it after
queuing invisibly. `Limiter` exposes both answers to "no room right now":
`TryAcquire` signals backpressure synchronously via its return value,
while `Acquire` waits, but only as long as the caller's context allows.

## What you'll build

```text
caplimit/                    independent module: example.com/caplimit
  go.mod                     go 1.24
  caplimit.go                  type Limiter; func New; methods TryAcquire, Acquire, release
  caplimit_test.go             capacity respected, release frees a slot, Acquire blocks, cancellation, concurrency
  cmd/demo/
    main.go                  exhausts capacity, releases, and cancels a blocked waiter
```

- Files: `caplimit.go`, `caplimit_test.go`, `cmd/demo/main.go`.
- Implement: `Limiter` with `New(capacity int) *Limiter`, `func (l *Limiter) TryAcquire() (release func(), ok bool)`, and `func (l *Limiter) Acquire(ctx context.Context) (release func(), err error)`.
- Test: `TryAcquire` succeeds up to capacity and then reports backpressure with `ok=false`; calling `release` frees a slot for a subsequent `TryAcquire`; `Acquire` blocks while capacity is exhausted and unblocks once a slot is released; `Acquire` returns the context's error when cancelled while waiting; concurrent goroutines acquiring and releasing never exceed capacity at once.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/caplimit/cmd/demo
cd ~/go-exercises/caplimit
go mod init example.com/caplimit
go mod edit -go=1.24
```

### A buffered channel is a counting semaphore

`Limiter.sem` is a `chan struct{}` with capacity equal to the resource
limit, and that single buffered channel *is* the whole implementation: a
successful send claims one unit of capacity, and a receive on `release`
gives it back. `TryAcquire`'s `select` with a `default` case is what makes
claiming non-blocking — if the channel's buffer is full, the send case is
not ready, `default` fires instantly, and the caller gets an immediate
`ok=false` instead of stalling. `Acquire` runs the identical send but
races it against `ctx.Done()` in the same `select`, so a caller that is
willing to wait still cannot wait forever: whichever case becomes ready
first — capacity freeing up, or the context ending — wins, and no third
option (a bare, uncancellable channel operation) is on the table.

Because tokens in the channel are fungible — a `struct{}` carries no
identity — `release` does not need to know which specific "slot" it is
giving back; any successful receive shrinks the buffer's current count by
one, which is exactly the semantics a fixed-size resource pool needs.

Create `caplimit.go`:

```go
package caplimit

import "context"

// Limiter gates concurrent access to a resource with a fixed capacity,
// implemented as a counting semaphore over a buffered channel: sending a
// token claims one unit of capacity, receiving one releases it.
type Limiter struct {
	sem chan struct{}
}

// New builds a Limiter that allows at most capacity concurrent holders.
func New(capacity int) *Limiter {
	return &Limiter{sem: make(chan struct{}, capacity)}
}

// TryAcquire claims one unit of capacity without blocking. If none is
// available it signals backpressure immediately by returning ok=false
// instead of making the caller wait. When ok is true, release must be
// called exactly once to give the capacity back.
func (l *Limiter) TryAcquire() (release func(), ok bool) {
	select {
	case l.sem <- struct{}{}:
		return l.release, true
	default:
		return nil, false
	}
}

// Acquire blocks until capacity is available or ctx is done, whichever
// comes first. On success release must be called exactly once. On
// context cancellation it returns ctx.Err() instead of a release func,
// having claimed nothing.
func (l *Limiter) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case l.sem <- struct{}{}:
		return l.release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *Limiter) release() {
	<-l.sem
}
```

### The runnable demo

The demo exhausts a two-slot limiter with `TryAcquire`, shows the third
attempt getting an immediate backpressure signal, releases a slot to
unblock a waiting `Acquire` call, and finally shows a second `Acquire`
call returning immediately once its context is already cancelled.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/caplimit"
)

func main() {
	limiter := caplimit.New(2)

	release1, ok1 := limiter.TryAcquire()
	release2, ok2 := limiter.TryAcquire()
	_, ok3 := limiter.TryAcquire()
	fmt.Printf("acquire 1: ok=%v\n", ok1)
	fmt.Printf("acquire 2: ok=%v\n", ok2)
	fmt.Printf("acquire 3: ok=%v (backpressure — capacity exhausted)\n", ok3)

	// A blocked waiter: Acquire waits until release1 frees a slot.
	waiterAcquired := make(chan struct{})
	go func() {
		release, err := limiter.Acquire(context.Background())
		fmt.Printf("waiter: acquired=%v err=%v\n", release != nil, err)
		close(waiterAcquired)
	}()

	fmt.Println("releasing slot 1")
	release1()
	<-waiterAcquired

	// A cancelled waiter: capacity is exhausted again (release2 still
	// held, the previous waiter now holds the other slot), so this
	// Acquire call blocks until its context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := limiter.Acquire(ctx)
	fmt.Printf("cancelled waiter: err=%v\n", err)

	release2()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquire 1: ok=true
acquire 2: ok=true
acquire 3: ok=false (backpressure — capacity exhausted)
releasing slot 1
waiter: acquired=true err=<nil>
cancelled waiter: err=context canceled
```

### Tests

`TestTryAcquireRespectsCapacity` and `TestReleaseFreesCapacityForTryAcquire`
cover the non-blocking half. `TestAcquireBlocksUntilCapacityIsReleased`
proves `Acquire` actually waits — it asserts the call has *not* returned
after a short timeout, then asserts it *does* return once `release` is
called. `TestAcquireReturnsContextErrorWhenCancelled` is the backpressure
signal for callers willing to wait: an already-cancelled context makes
`Acquire` return immediately with `ctx.Err()` instead of blocking.
`TestAcquireNeverExceedsCapacityUnderConcurrency` runs twenty goroutines
against a capacity of three under `-race` and tracks the maximum number
of simultaneous holders using a full compare-and-swap retry loop — not a
plain load-then-store — so the "is this the new max" decision and the
update happen as one atomic step with no window for a concurrent
goroutine to slip through.

Create `caplimit_test.go`:

```go
package caplimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTryAcquireRespectsCapacity(t *testing.T) {
	t.Parallel()

	l := New(2)

	_, ok1 := l.TryAcquire()
	_, ok2 := l.TryAcquire()
	_, ok3 := l.TryAcquire()

	if !ok1 || !ok2 {
		t.Fatalf("ok1=%v ok2=%v, want both true (capacity is 2)", ok1, ok2)
	}
	if ok3 {
		t.Fatal("ok3=true, want false (capacity exhausted signals backpressure)")
	}
}

func TestReleaseFreesCapacityForTryAcquire(t *testing.T) {
	t.Parallel()

	l := New(1)

	release, ok := l.TryAcquire()
	if !ok {
		t.Fatal("first TryAcquire() = false, want true")
	}
	if _, ok := l.TryAcquire(); ok {
		t.Fatal("second TryAcquire() = true before release, want false")
	}

	release()

	if _, ok := l.TryAcquire(); !ok {
		t.Fatal("TryAcquire() after release = false, want true")
	}
}

func TestAcquireBlocksUntilCapacityIsReleased(t *testing.T) {
	t.Parallel()

	l := New(1)
	release, ok := l.TryAcquire()
	if !ok {
		t.Fatal("TryAcquire() = false, want true")
	}

	acquired := make(chan struct{})
	go func() {
		r, err := l.Acquire(context.Background())
		if err != nil {
			t.Errorf("Acquire() err = %v, want nil", err)
			return
		}
		defer r()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("Acquire() returned before capacity was released")
	case <-time.After(20 * time.Millisecond):
		// expected: still blocked
	}

	release()

	select {
	case <-acquired:
		// expected: unblocked once capacity was released
	case <-time.After(time.Second):
		t.Fatal("Acquire() did not unblock after release")
	}
}

func TestAcquireReturnsContextErrorWhenCancelled(t *testing.T) {
	t.Parallel()

	l := New(1)
	release, ok := l.TryAcquire()
	if !ok {
		t.Fatal("TryAcquire() = false, want true")
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := l.Acquire(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire() err = %v, want context.Canceled", err)
	}
}

func TestAcquireNeverExceedsCapacityUnderConcurrency(t *testing.T) {
	t.Parallel()

	const capacity = 3
	const workers = 20
	l := New(capacity)

	var current atomic.Int32
	var maxSeen atomic.Int32

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire(context.Background())
			if err != nil {
				t.Errorf("Acquire() err = %v, want nil", err)
				return
			}
			defer release()

			n := current.Add(1)
			defer current.Add(-1)

			// Check-then-act on maxSeen via a full CAS retry loop, so the
			// "is n the new max" decision and the update happen
			// atomically together — no window where a concurrent
			// goroutine could observe a stale max.
			for {
				m := maxSeen.Load()
				if n <= m {
					break
				}
				if maxSeen.CompareAndSwap(m, n) {
					break
				}
			}

			time.Sleep(time.Millisecond) // widen the window for a real violation to surface
		}()
	}
	wg.Wait()

	if got := maxSeen.Load(); got > capacity {
		t.Fatalf("maxSeen = %d, want at most %d", got, capacity)
	}
}
```

## Review

`Limiter` is correct because both entry points share one enforcement
mechanism — the channel's fixed buffer — instead of `TryAcquire` and
`Acquire` maintaining separate counters that could drift out of sync.
`TryAcquire`'s `default` case is what makes it genuinely non-blocking;
without it, a full channel send would simply wait, collapsing `TryAcquire`
into `Acquire` and eliminating the synchronous backpressure signal this
exercise is about. The concurrency test's compare-and-swap loop is the
same lesson as the earlier lock exercises: a `Load` followed later by a
separate `CompareAndSwap` (or worse, a `Store`) reintroduces a gap where
two goroutines can both decide they hold the new maximum — the loop
closes that gap by retrying the whole check-and-update as one step
whenever another goroutine wins the race first. Run `go test -race`,
since capacity enforcement across concurrent goroutines is the entire
point.

## Resources

- [context package](https://pkg.go.dev/context) — `Context.Done`, `Context.Err`, the cancellation this exercise's `Acquire` depends on.
- [sync/atomic package](https://pkg.go.dev/sync/atomic) — `CompareAndSwap`, the primitive behind the concurrency test's max-tracking loop.
- [golang.org/x/sync/semaphore](https://pkg.go.dev/golang.org/x/sync/semaphore) — a production weighted-semaphore package solving the same capacity-gating problem with a richer API.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-visitor-tree-traversal-injected.md](27-visitor-tree-traversal-injected.md) | Next: [29-router-priority-chain-with-fallback.md](29-router-priority-chain-with-fallback.md)
