# Exercise 3: Concurrency Limiter That Tracks GOMAXPROCS Updates

Now that the runtime re-reads the cgroup quota up to once per second, capacity is
dynamic — so a worker pool that reads `runtime.NumCPU()` once at startup, or
hardcodes a constant, is a bug. This exercise builds a resizable concurrency
limiter whose parallelism follows an injectable `GOMAXPROCS` provider, plus a
supervisor that re-reads it on a ticker exactly as the runtime re-reads the quota.

This module is fully self-contained: its own `go mod init`, all code inline, its
own demo and tests. Nothing here imports another exercise.

## What you'll build

```text
pool/                      independent module: example.com/pool
  go.mod                   go 1.25 (testing/synctest needs it)
  pool.go                  Limiter (broadcast semaphore), Supervise, DefaultProvider
  cmd/
    demo/
      main.go              acquire to capacity, block, release, resize
  pool_test.go             synctest resize + block/unblock tests; non-synctest smoke test
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: `Limiter` with `New(n int)`, `Acquire(ctx) error`, `Release()`, `Capacity() int`, `SetCapacity(n int)`; `DefaultProvider() int` returning `runtime.GOMAXPROCS(0)`; and `Supervise(ctx, provider func() int, interval time.Duration)` that re-reads the provider on a ticker and resizes the limiter.
- Test: under `testing/synctest`, flip a fake provider `4 -> 8 -> 2`, advance the bubble clock past each tick, and assert `Capacity()` converges; assert `Acquire` blocks at capacity and unblocks on `Release`; assert context cancellation stops the supervisor with no leaked goroutine; a non-synctest smoke test asserts `DefaultProvider()` equals `runtime.GOMAXPROCS(0)`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.25
```

### Why the capacity source is a seam, not a constant

The single design decision that makes this pool correct under Go 1.25 is that its
parallelism comes from a `func() int` provider, defaulting to
`func() int { return runtime.GOMAXPROCS(0) }`, rather than from `runtime.NumCPU()`
or a literal. `GOMAXPROCS(0)` reflects the runtime's container-aware effective
parallelism, and — because the runtime updates it live when the cgroup quota
changes — a pool that periodically re-reads it will grow when a VPA raises the
limit and shrink when the limit is lowered, without a restart. Injecting the
provider as a seam is also what makes the pool testable: the tests supply a fake
provider backed by an atomic, so no test depends on the real CPU count or a cgroup.

### A resizable semaphore

A plain buffered-channel semaphore cannot be resized: the buffer length is fixed at
creation. So the limiter is a small broadcast semaphore built from a mutex, an
in-flight counter, a capacity field, and a `notify` channel used purely as a wake
signal. `Acquire` takes the lock; if `inFlight < cap` it increments and returns;
otherwise it captures the current `notify` channel, unlocks, and blocks on a
`select` over that channel and `ctx.Done()`. `Release` decrements and *broadcasts*
by closing the current `notify` channel and installing a fresh one — closing a
channel wakes every goroutine blocked receiving on it, and each then re-checks the
condition under the lock. `SetCapacity` mutates `cap` and broadcasts too, so a
capacity increase immediately wakes waiters that can now proceed. This "close and
replace" broadcast is the standard way to build a condition wait that also composes
with `ctx.Done()` in a `select` (which `sync.Cond` cannot).

The one invariant to hold: every mutation of `cap` or `inFlight` happens under the
mutex, and every broadcast happens under the mutex too, so a waiter that reads
`notify` and a releaser that swaps it cannot race. The `-race` test exercises this
directly.

Create `pool.go`:

```go
// Package pool provides a concurrency limiter whose capacity is derived from
// GOMAXPROCS and can be resized live, tracking the Go 1.25 runtime's periodic
// re-reading of the cgroup CPU quota.
package pool

import (
	"context"
	"runtime"
	"sync"
	"time"
)

// DefaultProvider reports the current container-aware parallelism. It reads
// runtime.GOMAXPROCS(0) (the query form) rather than runtime.NumCPU, so it
// reflects the cgroup CPU limit, not the host core count.
func DefaultProvider() int {
	return runtime.GOMAXPROCS(0)
}

// Limiter is a resizable counting semaphore. Its capacity can change at runtime
// via SetCapacity, so a supervisor can keep it aligned with GOMAXPROCS.
type Limiter struct {
	mu       sync.Mutex
	cap      int
	inFlight int
	notify   chan struct{} // closed-and-replaced to wake waiters
}

// New returns a Limiter allowing n concurrent holders. n is clamped to a minimum
// of 1 so the limiter always admits at least one holder.
func New(n int) *Limiter {
	if n < 1 {
		n = 1
	}
	return &Limiter{cap: n, notify: make(chan struct{})}
}

// Acquire blocks until a slot is free or ctx is done, returning ctx.Err() in the
// latter case. On success the caller must call Release exactly once.
func (l *Limiter) Acquire(ctx context.Context) error {
	for {
		l.mu.Lock()
		if l.inFlight < l.cap {
			l.inFlight++
			l.mu.Unlock()
			return nil
		}
		wait := l.notify
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
			// Capacity changed or a slot was released; re-check under the lock.
		}
	}
}

// Release returns a slot and wakes any waiters.
func (l *Limiter) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
	l.broadcastLocked()
}

// Capacity reports the current maximum number of concurrent holders.
func (l *Limiter) Capacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cap
}

// SetCapacity changes the maximum number of concurrent holders and wakes waiters
// so any that can now proceed do. n is clamped to a minimum of 1.
func (l *Limiter) SetCapacity(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cap = n
	l.broadcastLocked()
}

// broadcastLocked wakes every waiter by closing the current notify channel and
// installing a fresh one. It must be called with l.mu held.
func (l *Limiter) broadcastLocked() {
	close(l.notify)
	l.notify = make(chan struct{})
}

// Supervise re-reads provider on each tick and resizes the limiter to match,
// mirroring how the runtime re-reads the cgroup quota up to once per second. It
// returns when ctx is done.
func (l *Limiter) Supervise(ctx context.Context, provider func() int, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.SetCapacity(provider())
		}
	}
}
```

### The runnable demo

The demo starts the limiter at the container-aware capacity, then narrows it to 2,
fills both slots, shows that a third `Acquire` under a short deadline is rejected
with `context.DeadlineExceeded`, and that a `Release` frees a slot again.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"example.com/pool"
)

func main() {
	l := pool.New(pool.DefaultProvider())
	fmt.Printf("initial capacity tracks GOMAXPROCS(0): %v\n", l.Capacity() == runtime.GOMAXPROCS(0))

	l.SetCapacity(2)
	ctx := context.Background()
	_ = l.Acquire(ctx)
	_ = l.Acquire(ctx)

	deadlineCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err := l.Acquire(deadlineCtx)
	fmt.Printf("acquire at capacity blocked: %v\n", errors.Is(err, context.DeadlineExceeded))

	l.Release()
	fmt.Printf("after release, slot available: %v\n", l.Acquire(ctx) == nil)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
initial capacity tracks GOMAXPROCS(0): true
acquire at capacity blocked: true
after release, slot available: true
```

### Tests

The time-based tests run inside a `testing/synctest` bubble so the supervisor's
ticker uses virtual time — a one-second interval is advanced instantly and
deterministically. `TestSupervisorTracksProvider` starts the supervisor with a fake
provider backed by an `atomic.Int64`, flips it `4 -> 8 -> 2`, and after each flip
sleeps just past the tick and calls `synctest.Wait()` so the supervisor's resize is
guaranteed complete before the assertion. Context cancellation via `defer cancel()`
stops the supervisor; `synctest.Test` reports a deadlock if that goroutine leaked,
so the test also proves clean shutdown. `TestAcquireBlocksAtCapacity` fills a
capacity-2 limiter, starts a third `Acquire` in a goroutine, and uses
`synctest.Wait()` plus a non-blocking `select` to assert it is parked, then that a
`Release` unblocks it. `TestDefaultProvider` is a plain (non-bubble) smoke test.

Create `pool_test.go`:

```go
package pool

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestSupervisorTracksProvider(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var procs atomic.Int64
		procs.Store(4)
		provider := func() int { return int(procs.Load()) }

		l := New(provider())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go l.Supervise(ctx, provider, time.Second)

		steps := []int{8, 2}
		for _, want := range steps {
			procs.Store(int64(want))
			// Advance past the next tick, then let the supervisor resize.
			time.Sleep(time.Second + time.Millisecond)
			synctest.Wait()
			if got := l.Capacity(); got != want {
				t.Fatalf("Capacity() = %d after provider -> %d", got, want)
			}
		}
	})
}

func TestAcquireBlocksAtCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New(2)
		ctx := t.Context()
		if err := l.Acquire(ctx); err != nil {
			t.Fatalf("first Acquire: %v", err)
		}
		if err := l.Acquire(ctx); err != nil {
			t.Fatalf("second Acquire: %v", err)
		}

		acquired := make(chan struct{})
		go func() {
			if err := l.Acquire(ctx); err == nil {
				close(acquired)
			}
		}()

		synctest.Wait()
		select {
		case <-acquired:
			t.Fatal("Acquire returned while at capacity")
		default:
		}

		l.Release()
		synctest.Wait()
		select {
		case <-acquired:
		default:
			t.Fatal("Acquire did not unblock after Release")
		}
	})
}

func TestDefaultProvider(t *testing.T) {
	if got, want := DefaultProvider(), runtime.GOMAXPROCS(0); got != want {
		t.Fatalf("DefaultProvider() = %d, want runtime.GOMAXPROCS(0) = %d", got, want)
	}
}

func ExampleLimiter() {
	l := New(3)
	fmt.Println(l.Capacity())
	l.SetCapacity(5)
	fmt.Println(l.Capacity())
	// Output:
	// 3
	// 5
}
```

## Review

The pool is correct when capacity is a single source of truth guarded by the mutex
and every waiter re-checks it after a wake. The classic bug is a lost wakeup: if
`Acquire` reads `l.notify` *after* unlocking instead of before, a `Release` between
the unlock and the read could close a channel the waiter never captured, and the
waiter sleeps forever. Capturing `wait := l.notify` under the lock, then selecting
on it after unlocking, closes that window — and the `-race` detector plus
`synctest`'s leaked-goroutine reporting will catch a regression. `SetCapacity` must
also broadcast, or a capacity increase would not wake waiters that could now
proceed; `TestSupervisorTracksProvider` exercises the resize path but you can prove
the wake path by growing capacity while an `Acquire` is parked.

The higher-level lesson is the one the tests encode structurally: nothing here reads
`runtime.NumCPU()`, and the capacity provider is injectable, so the pool tracks the
runtime's live `GOMAXPROCS`. Under `synctest`, keep the supervisor cancellable with
`defer cancel()` — a supervisor with no shutdown path leaks a goroutine and the
bubble reports a deadlock rather than a silent leak. Run `go test -race` to confirm
both the semaphore and the resize loop are race-free.

## Resources

- [`runtime.GOMAXPROCS`](https://pkg.go.dev/runtime#GOMAXPROCS) — the query form (`GOMAXPROCS(0)`) the capacity provider reads.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — the virtual-time bubble and `synctest.Wait` used to test the ticker-driven resize deterministically.
- [Go 1.25 release notes — periodic GOMAXPROCS updates](https://go.dev/doc/go1.25) — why capacity is now dynamic and re-read up to once per second.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [02-runtime-gomaxprocs-control.md](02-runtime-gomaxprocs-control.md) | Next: [../11-swiss-table-map-internals/00-concepts.md](../11-swiss-table-map-internals/00-concepts.md)
