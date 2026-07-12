# Exercise 2: Startup readiness latch (wait for N dependencies)

Service boot is a fan-in: the process is not ready to accept traffic until every
dependency — the database, the cache, the message broker — has passed its probe.
A countdown latch expresses this directly: `Wait()` blocks until `N` `Done()`
calls have driven a counter to zero, at which point every waiter is released
together. This is the canonical `Broadcast` use case — one state change (the
counter reaching zero) satisfies many waiters at once, so `Signal` would be a
bug.

## What you'll build

```text
latch/                      independent module: example.com/latch
  go.mod                    module path example.com/latch
  latch.go                  type Latch: New, Done, Wait, Count (Broadcast at zero)
  cmd/
    demo/
      main.go               three probes counting down a readiness gate
  latch_test.go             fan-out block/release, idempotent Wait, overshoot safety
```

- Files: `latch.go`, `cmd/demo/main.go`, `latch_test.go`.
- Implement: a `Latch` with `New(n)`, `Done()` (decrement; `Broadcast` when it hits zero), `Wait()` (block while count > 0), and `Count()`.
- Test: `M` waiters all block until exactly `N` `Done()` calls; `Wait()` on a zeroed latch returns immediately; extra `Done()` past zero neither panics nor goes negative.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/06-sync-cond/02-countdown-latch/cmd/demo
cd go-solutions/15-sync-primitives/06-sync-cond/02-countdown-latch
```

### Why Broadcast, and why the overshoot guard matters

The predicate every waiter shares is "count == 0". When the last `Done()` drives
the counter to zero, that single transition satisfies EVERY parked `Wait()`
simultaneously — the DB-waiter, the traffic-gate-waiter, the health-check-waiter.
`Broadcast` wakes all of them; `Signal` would wake exactly one and leave the rest
parked forever, because no further `Done()` is coming to kick them. This is the
textbook case where the number of satisfiable waiters is more than one and known
only at runtime, so `Broadcast` is mandatory.

Two edge cases make the latch robust in production. First, `Wait()` on an
already-zero latch must return immediately — a late reader that checks readiness
after boot completed should not block. The `for count > 0` loop handles this for
free: the predicate is already false, so `Wait` is never entered. Second, `Done()`
called more times than `N` (a buggy probe firing twice) must not drive the counter
negative or panic — a negative counter would make the `count == 0` predicate
unreachable and wedge every future waiter. The guard is a single `if l.count == 0
{ return }` at the top of `Done()`.

Create `latch.go`:

```go
package latch

import "sync"

// Latch blocks Wait callers until Done has been called enough times to drive an
// initial count of N down to zero, then releases every waiter together.
type Latch struct {
	mu    sync.Mutex
	cond  *sync.Cond
	count int
}

// New returns a Latch that releases once Done has been called n times. A latch
// created with n <= 0 is already released.
func New(n int) *Latch {
	if n < 0 {
		n = 0
	}
	l := &Latch{count: n}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// Done records one completed dependency. The call that reaches zero wakes every
// waiter. Calls past zero are ignored so the count never goes negative.
func (l *Latch) Done() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count == 0 {
		return
	}
	l.count--
	if l.count == 0 {
		l.cond.Broadcast()
	}
}

// Wait blocks until the count reaches zero. It returns immediately if the latch
// is already released.
func (l *Latch) Wait() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.count > 0 {
		l.cond.Wait()
	}
}

// Count reports the number of Done calls still outstanding.
func (l *Latch) Count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}
```

### The runnable demo

The demo models three dependency probes counting down a readiness latch while the
main goroutine blocks in `Wait()`. Each probe finishes after a short, staggered
delay; only when all three have called `Done()` does `Wait()` return and the
service report ready.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/latch"
)

func main() {
	deps := []string{"database", "cache", "broker"}
	ready := latch.New(len(deps))

	var wg sync.WaitGroup
	for i, name := range deps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i+1) * 10 * time.Millisecond)
			fmt.Printf("probe ok: %s\n", name)
			ready.Done()
		}()
	}

	ready.Wait()
	fmt.Println("service ready: accepting traffic")
	wg.Wait()
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output (probe lines complete before the ready line, in dependency
order):

```
probe ok: database
probe ok: cache
probe ok: broker
service ready: accepting traffic
```

### Tests

`TestFanOutRelease` is the core proof: it spawns `M` waiter goroutines against a
latch of `N`, uses `synctest.Wait()` to confirm all `M` are durably blocked, then
calls `Done()` exactly `N` times and asserts that only the final `Done()` releases
every waiter at once. `TestWaitAfterZeroReturns` pins the idempotent fast path.
`TestOvershootIsSafe` fires extra `Done()` calls and asserts the count floors at
zero. The `-race` build confirms the counter and the waiter list are properly
synchronized.

Create `latch_test.go`:

```go
package latch

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestFanOutRelease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const n, waiters = 3, 5
		l := New(n)

		var released atomic.Int64
		for range waiters {
			go func() {
				l.Wait()
				released.Add(1)
			}()
		}

		synctest.Wait() // all waiters durably blocked on Cond.Wait
		if got := released.Load(); got != 0 {
			t.Fatalf("released = %d before any Done, want 0", got)
		}

		for i := range n {
			l.Done()
			synctest.Wait()
			want := int64(0)
			if i == n-1 {
				want = waiters // only the last Done releases everyone
			}
			if got := released.Load(); got != want {
				t.Fatalf("after %d Done calls, released = %d, want %d", i+1, got, want)
			}
		}
	})
}

func TestWaitAfterZeroReturns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		l := New(2)
		l.Done()
		l.Done() // count now zero

		done := make(chan struct{})
		go func() {
			l.Wait() // must not block
			close(done)
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("Wait blocked on an already-released latch")
		}
	})
}

func TestOvershootIsSafe(t *testing.T) {
	t.Parallel()

	l := New(1)
	l.Done()
	l.Done() // extra
	l.Done() // extra
	if got := l.Count(); got != 0 {
		t.Fatalf("Count = %d after overshoot, want 0 (never negative)", got)
	}
	l.Wait() // still returns immediately
}

func TestConcurrentDone(t *testing.T) {
	t.Parallel()

	const n = 100
	l := New(n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.Done()
		}()
	}
	wg.Wait()
	l.Wait()
	if got := l.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
}
```

## Review

The latch is correct when the release is all-or-nothing: no waiter proceeds until
the count reaches zero, and then every waiter proceeds. `TestFanOutRelease` pins
this exactly by asserting `released` stays at zero through the first `N-1`
`Done()` calls and jumps to `waiters` only on the last — a guarantee that rests on
`Broadcast` rather than `Signal`. The idempotent fast path (`Wait` on a zeroed
latch) and the overshoot guard (`Done` past zero is a no-op) are what make the
type safe to call from concurrent, possibly-buggy probes without wedging the gate.

The trap unique to this shape is using `Signal` in `Done()`: the tests would
still pass with a single waiter but deadlock the moment two goroutines wait, since
only one is ever woken. The `for count > 0` loop is also load-bearing for the
already-released case — an `if` would still work here, but the loop is the honest
default and matches every other `Cond` in this chapter.

## Resources

- [`sync.Cond.Broadcast`](https://pkg.go.dev/sync#Cond.Broadcast) — waking every waiter on a shared terminal condition.
- [`testing/synctest`](https://pkg.go.dev/testing/synctest) — asserting all waiters are durably blocked before releasing them.
- [`sync.WaitGroup`](https://pkg.go.dev/sync#WaitGroup) — the stdlib fan-in this latch generalizes (WaitGroup counts up-then-down; the latch lets many goroutines Wait on the zero crossing).

---

Back to [01-bounded-buffer.md](01-bounded-buffer.md) | Next: [03-connection-pool.md](03-connection-pool.md)
