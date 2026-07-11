# Exercise 12: Connection Pool Whose Acquire Always Has an Exit and Never Loses Capacity

**Level: Intermediate**

A database connection pool hands out a fixed number of reusable connections; when all are checked out, `Acquire` must block until one is returned, but every caller carries a context deadline and must be able to give up. The tempting two-step design — take a permit, then in a separate step pull a connection — has a fatal race: a cancelled `Acquire` can consume a slot without ever returning a connection, permanently shrinking the pool until every future `Acquire` blocks forever. This exercise builds the correct design: acquisition is a single atomic `select` over an idle-connection channel versus `ctx.Done()`, and `Release` is a non-blocking return, so a timed-out waiter removes nothing from circulation and a returned connection can never wedge on the way back.

This module is self-contained: its own module, a `connpool` package, a demo, and tests.
Nothing here imports another exercise.

## What you'll build

```text
connpool/                    independent module: example.com/connpool
  go.mod                     go 1.26
  connpool.go                Pool with atomic Acquire (idle vs ctx.Done) and non-blocking Release
  cmd/demo/main.go           runnable demo: saturate, time out with no capacity loss, unblock a waiter, churn
  connpool_test.go           conservation under cancellation, one-release-wakes-one, churn conserves Cap, cause propagation
```

- Files: `connpool.go`, `cmd/demo/main.go`, `connpool_test.go`.
- Implement: `New(size, factory)`, `(*Pool).Acquire(ctx) (*Conn, error)`, `(*Pool).Release(*Conn)`, `(*Pool).Available() int`, `(*Pool).Cap() int`.
- Test: the pool's conservation invariant `idle + checked-out == Cap` holds on every path, including a cancelled `Acquire`; one `Release` unblocks exactly one waiter; heavy churn leaks and duplicates nothing; no goroutine leak.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connpool/cmd/demo
cd ~/go-exercises/connpool
go mod init example.com/connpool
go mod edit -go=1.26
go get go.uber.org/goleak
```

### Conservation is the invariant; the two-step design breaks it

A pool of `N` connections has exactly one invariant worth defending: at every instant, `idle + checked-out == N`. Nothing else matters — not throughput, not fairness — because the moment that sum drops below `N`, capacity has leaked, and a leaked-capacity pool degrades into a total hang. Once enough slots are gone, every `Acquire` blocks on connections that no longer exist, and no `Release` will ever arrive to wake them. This is a partial deadlock the Go runtime cannot see: the health check still returns 200 while request goroutines pile up on a pool that has quietly shrunk to zero.

The classic way to lose capacity is a two-step acquire. You model availability as a counting semaphore of `N` permits and the connections as a separate list. `Acquire` first takes a permit (blocking until one is free), then pulls a connection from the list. The bug lives in the gap between the two steps: a caller whose context expires *after* taking the permit but *before* pulling a connection returns an error, but its cleanup path is easy to get wrong — if the permit is not perfectly returned on every cancellation and panic branch, that slot is gone for good. Every extra step between "I am allowed to proceed" and "I hold a connection" is a window where a cancellation can strand a resource.

The fix is to collapse the two steps into one. Make the idle connections themselves the synchronization primitive: a buffered channel of `*Conn` with capacity `N`, pre-filled with `N` connections. Now acquisition is a single `select`:

```go
select {
case c := <-p.idle:   // took exactly one connection out of circulation
    return c, nil
case <-ctx.Done():     // took nothing; circulation is untouched
    return nil, context.Cause(ctx)
}
```

A `select` commits to exactly one branch, atomically. Either the receive fires and one connection leaves the channel, or the `ctx.Done()` branch fires and the channel is never touched. There is no intermediate state a cancellation can strand, because there is no intermediate step. A timed-out waiter provably removes nothing — the invariant is preserved by construction, not by careful cleanup.

`Release` is the mirror: a non-blocking send back into the same channel. Because the channel is buffered to `N` and the pool never has more than `N` connections in circulation, a legitimately owned connection always fits — the send can never block, so a returning connection can never wedge on the way back. A blocked `Release` would be its own partial deadlock (the returner parks forever); the capacity-`N` buffer rules it out. The one thing `Release` must guard is a *foreign* or *double* return: if the buffer is somehow full, the connection being returned is not one the pool owns, and accepting it would violate the invariant in the other direction (more than `N` in circulation). We treat a full buffer as a programming error and panic rather than silently corrupt the count.

`context.Cause(ctx)` (Go 1.20+) reports the specific error the context was cancelled with — `context.DeadlineExceeded` for a timeout, or whatever value a `context.WithCancelCause` caller supplied — which is strictly more informative than `ctx.Err()` when you need to know *why* a caller gave up.

Create `connpool.go`:

```go
// Package connpool implements a fixed-capacity connection pool whose Acquire
// always has an exit and never loses capacity. Acquisition is a single atomic
// select over an idle-connection channel versus ctx.Done(), so a cancelled or
// timed-out Acquire removes nothing from circulation. Release is a non-blocking
// return into a channel buffered to the pool size, so a returned connection can
// never wedge on the way back.
package connpool

import "context"

// Conn is an opaque handle owned by the caller between Acquire and Release.
type Conn struct{ ID int }

// Pool hands out a fixed number of reusable connections. Its invariant is
// conservation: idle + checked-out == Cap on every path, including cancellation.
type Pool struct {
	idle chan *Conn
	size int
}

// New builds a pool of size connections using factory to mint each one. The
// idle channel is buffered to size, so every connection always has a slot to
// return to and Release can never block.
func New(size int, factory func(id int) *Conn) *Pool {
	if size <= 0 {
		panic("connpool: size must be positive")
	}
	p := &Pool{idle: make(chan *Conn, size), size: size}
	for i := range size {
		p.idle <- factory(i)
	}
	return p
}

// Acquire blocks until a connection is idle or ctx is done. It is a single
// select: either it receives a connection (removing exactly one from idle) or
// it observes cancellation (removing nothing). There is no intermediate permit
// step that a cancellation could strand, so a timed-out waiter loses no
// capacity.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	select {
	case c := <-p.idle:
		return c, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// Release returns a connection to the pool. The send is non-blocking because
// idle is buffered to size and the pool never has more than size connections in
// circulation, so a legitimately owned connection always fits.
func (p *Pool) Release(c *Conn) {
	if c == nil {
		return
	}
	select {
	case p.idle <- c:
	default:
		// A full channel means c was not one of ours (double release or a
		// foreign handle). Dropping it protects the invariant.
		panic("connpool: Release of a connection the pool does not own")
	}
}

// Available reports the number of idle connections, for assertions.
func (p *Pool) Available() int { return len(p.idle) }

// Cap reports the fixed pool capacity.
func (p *Pool) Cap() int { return p.size }
```

### The runnable demo

The demo walks the whole lifecycle deterministically: it builds a pool of three, saturates it, proves a deadline-bounded `Acquire` returns without taking a slot, hands one connection to a blocked waiter, and finally runs a churn phase and confirms the pool ends exactly at capacity. All print order is sequential, so the output is identical on every run.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"example.com/connpool"
)

func main() {
	const size = 3
	p := connpool.New(size, func(id int) *connpool.Conn {
		return &connpool.Conn{ID: id}
	})
	fmt.Printf("new pool: cap=%d available=%d\n", p.Cap(), p.Available())

	// Saturate the pool: check out every connection.
	held := make([]*connpool.Conn, 0, size)
	for range size {
		c, err := p.Acquire(context.Background())
		if err != nil {
			panic(err)
		}
		held = append(held, c)
	}
	fmt.Printf("saturated: available=%d\n", p.Available())

	// A timed-out Acquire returns DeadlineExceeded and loses no capacity.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err := p.Acquire(ctx)
	cancel()
	fmt.Printf("acquire under deadline: err=%v available=%d\n",
		errors.Is(err, context.DeadlineExceeded), p.Available())

	// Releasing one connection unblocks exactly one waiter.
	got := make(chan *connpool.Conn, 1)
	go func() {
		c, err := p.Acquire(context.Background())
		if err != nil {
			panic(err)
		}
		got <- c
	}()
	p.Release(held[0]) // hand back conn 0
	woken := <-got
	fmt.Printf("waiter woke on released conn: id=%d\n", woken.ID)

	// Return everything still checked out plus the waiter's connection.
	p.Release(woken)
	p.Release(held[1])
	p.Release(held[2])
	fmt.Printf("all returned: available=%d cap=%d conserved=%v\n",
		p.Available(), p.Cap(), p.Available() == p.Cap())

	// Churn: many goroutines acquire then release; capacity is conserved.
	const workers, rounds = 8, 5000
	var churn sync.WaitGroup
	for range workers {
		churn.Go(func() {
			for range rounds {
				c, err := p.Acquire(context.Background())
				if err != nil {
					panic(err)
				}
				p.Release(c)
			}
		})
	}
	churn.Wait()
	fmt.Printf("after churn: available=%d conserved=%v\n",
		p.Available(), p.Available() == p.Cap())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
new pool: cap=3 available=3
saturated: available=0
acquire under deadline: err=true available=0
waiter woke on released conn: id=0
all returned: available=3 cap=3 conserved=true
after churn: available=3 conserved=true
```

### Tests

`TestTimedOutWaiterLosesNoCapacity` saturates the pool, then issues an `Acquire` under a 20 ms deadline and asserts it returns `context.DeadlineExceeded` promptly while `Available` stays at 0 — the timed-out waiter took nothing. `TestReleaseUnblocksExactlyOneWaiter` parks `Cap` waiters, releases one connection, and proves exactly one wakes: the `woke` channel is unbuffered and each woken waiter holds its connection on `<-release`, so no cascade can occur and the non-blocking "did a second one wake?" check is deterministic, not timing-dependent. `TestChurnConservesCapacity` runs twelve goroutines each doing 4000 acquire/release cycles and asserts `Available == Cap` at the end — no leaked or duplicated connection. `TestCancelledContextReturnsCause` checks that `Acquire` surfaces the exact cancellation cause via `context.Cause`. `TestMain` wraps everything in `goleak.VerifyTestMain` so any goroutine leak fails the run. Each hang-prone body runs under `guard`, a per-test watchdog that dumps every goroutine's stack on timeout instead of hanging silently.

Create `connpool_test.go`:

```go
package connpool

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func newPool(size int) *Pool {
	return New(size, func(id int) *Conn { return &Conn{ID: id} })
}

// guard runs fn and fails with a full goroutine dump if it does not finish in d,
// turning a partial deadlock into an actionable stack trace instead of a hang.
func guard(t *testing.T, d time.Duration, name string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("%s did not finish within %s: likely deadlock\n\n%s", name, d, buf[:n])
	}
}

// TestTimedOutWaiterLosesNoCapacity saturates the pool, then an Acquire under a
// short deadline must return context.DeadlineExceeded and leave Available at 0.
// This is the capacity-loss deadlock the design prevents: a timed-out waiter
// removes nothing from circulation.
func TestTimedOutWaiterLosesNoCapacity(t *testing.T) {
	t.Parallel()
	p := newPool(4)

	held := make([]*Conn, 0, p.Cap())
	for range p.Cap() {
		c, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("Acquire on unsaturated pool: %v", err)
		}
		held = append(held, c)
	}
	if p.Available() != 0 {
		t.Fatalf("saturated pool Available = %d, want 0", p.Available())
	}

	guard(t, 5*time.Second, "timed-out Acquire", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		c, err := p.Acquire(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Acquire err = %v, want DeadlineExceeded", err)
		}
		if c != nil {
			t.Errorf("Acquire returned conn %v on timeout, want nil", c)
		}
	})

	if p.Available() != 0 {
		t.Fatalf("after timed-out Acquire, Available = %d, want 0 (capacity lost)", p.Available())
	}

	for _, c := range held {
		p.Release(c)
	}
	if p.Available() != p.Cap() {
		t.Fatalf("after releasing all, Available = %d, want %d", p.Available(), p.Cap())
	}
}

// TestReleaseUnblocksExactlyOneWaiter saturates the pool and parks Cap waiters.
// Releasing one connection must wake exactly one of them. The proof is
// deterministic: the woke channel is unbuffered, and until we release another
// connection no second waiter can reach its send, so the non-blocking check is
// guaranteed empty rather than timing-dependent.
func TestReleaseUnblocksExactlyOneWaiter(t *testing.T) {
	t.Parallel()
	const size = 4
	p := newPool(size)

	held := make([]*Conn, 0, size)
	for range size {
		c, _ := p.Acquire(context.Background())
		held = append(held, c)
	}

	woke := make(chan struct{}) // unbuffered: a waiter blocks here until received
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range size {
		wg.Go(func() {
			c, err := p.Acquire(context.Background())
			if err != nil {
				t.Errorf("waiter Acquire: %v", err)
				return
			}
			woke <- struct{}{}
			<-release // hold the connection until the test tears down
			p.Release(c)
		})
	}

	guard(t, 5*time.Second, "one release wakes one waiter", func() {
		p.Release(held[0]) // release exactly one connection
		<-woke             // exactly one waiter proceeds

		// No other waiter can have woken: only one connection was released, the
		// waiter that took it is parked on <-release still holding it, and the
		// rest are parked inside Acquire. So this check is not a race.
		select {
		case <-woke:
			t.Errorf("a second waiter woke after a single release")
		default:
		}

		// Release the remaining connections; the other waiters drain in turn,
		// one per released connection.
		for i := 1; i < size; i++ {
			p.Release(held[i])
			<-woke
		}
		close(release) // let every waiter return its connection
	})

	wg.Wait()
	if p.Available() != p.Cap() {
		t.Fatalf("after all waiters released, Available = %d, want %d", p.Available(), p.Cap())
	}
}

// TestChurnConservesCapacity runs many goroutines each doing Acquire then
// Release thousands of times and asserts the pool ends with exactly Cap idle
// connections: none leaked, none duplicated. Runs under -race to shake out any
// ordering-dependent capacity loss.
func TestChurnConservesCapacity(t *testing.T) {
	t.Parallel()
	const (
		size    = 6
		workers = 12
		rounds  = 4000
	)
	p := newPool(size)

	guard(t, 30*time.Second, "churn", func() {
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				for range rounds {
					c, err := p.Acquire(context.Background())
					if err != nil {
						t.Errorf("churn Acquire: %v", err)
						return
					}
					p.Release(c)
				}
			})
		}
		wg.Wait()
	})

	if p.Available() != p.Cap() {
		t.Fatalf("after churn, Available = %d, want %d", p.Available(), p.Cap())
	}
}

// TestCancelledContextReturnsCause proves Acquire reports the specific
// cancellation cause via context.Cause, not a generic error.
func TestCancelledContextReturnsCause(t *testing.T) {
	t.Parallel()
	p := newPool(1)
	c, _ := p.Acquire(context.Background()) // saturate

	wantErr := errors.New("client gave up")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(wantErr)
	_, err := p.Acquire(ctx)
	if !errors.Is(err, wantErr) {
		t.Fatalf("Acquire err = %v, want %v", err, wantErr)
	}

	p.Release(c)
	if p.Available() != p.Cap() {
		t.Fatalf("Available = %d, want %d", p.Available(), p.Cap())
	}
}
```

## Review

Correct here means the conservation invariant `idle + checked-out == Cap` survives every path, and the guarantee comes entirely from collapsing acquisition into a single atomic `select`: because a `select` commits to exactly one branch, a cancelled `Acquire` either received a connection or touched nothing, never a stranded half-step, and the capacity-`Cap` buffer makes every `Release` a non-blocking return that cannot wedge. `TestTimedOutWaiterLosesNoCapacity` proves the cancellation path removes nothing (`Available` stays 0 after a deadline fires), `TestReleaseUnblocksExactlyOneWaiter` proves one return wakes exactly one waiter with a race-free negative check, and `TestChurnConservesCapacity` under `-race` plus `goleak` proves that millions of acquire/release pairs neither leak nor duplicate a connection. The production bug this prevents is the slow-motion pool death of a two-step "take a permit, then fetch a connection" design, where each cancelled request between the two steps leaks a slot until the pool starves to zero and every future `Acquire` hangs forever — a partial deadlock the Go runtime never reports because the rest of the service keeps running.

## Resources

- [Go Concurrency Patterns: Context](https://go.dev/blog/context) — cancellation and deadline propagation, the model every `Acquire` here obeys.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — recovering the specific reason a context was cancelled, more informative than `Err`.
- [Effective Go: Channels](https://go.dev/doc/effective_go#channels) — buffered channels as bounded resource pools, the core idiom behind the idle channel.
- [`go.uber.org/goleak`](https://pkg.go.dev/go.uber.org/goleak) — failing a test when a goroutine leaks, which is how a wedged `Acquire` would surface in CI.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [11-cond-bounded-log-buffer-no-lost-wakeup.md](11-cond-bounded-log-buffer-no-lost-wakeup.md) | Next: [13-dead-letter-feedback-cycle-break.md](13-dead-letter-feedback-cycle-break.md)
