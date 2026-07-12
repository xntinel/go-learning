# Exercise 10: Connection-Pool Lease Checkout Actor

**Level: Intermediate**

A backend fronts a fixed set of expensive resources — say database connections.
Callers check one out, use it, and return it; when all are busy, further checkouts
must wait in FIFO order until one is released, honouring a context deadline. The
naive fix reaches for a `sync.Mutex` plus a condition variable and quickly grows
races around cancellation and lost connections. This exercise builds the actor
version: a single goroutine owns the free list and the queue of waiting reply
channels, so there is no mutex on the pool and a released connection is handed to
the longest-waiting caller.

This module is self-contained: its own module, a `leasepool` package, a demo, and
tests. Nothing here imports another exercise.

## What you'll build

```text
leasepool/                   independent module: example.com/leasepool
  go.mod                     go 1.26
  leasepool.go               Pool actor owning freeConns + a []chan *Lease waiter queue
  cmd/demo/main.go           runnable demo: exhaust the pool, queue a waiter, release
  leasepool_test.go          distinct-conns, blocking, FIFO, deadline-deregister, shutdown, conservation
```

- Files: `leasepool.go`, `cmd/demo/main.go`, `leasepool_test.go`.
- Implement: `type Lease struct{ Conn int }` with `func (l *Lease) Release()`; `type Pool`; `func New(size int) *Pool`; `func (p *Pool) Run()`; `func (p *Pool) Acquire(ctx context.Context) (*Lease, error)`; `func (p *Pool) Available() int`; `func (p *Pool) Shutdown()`; `var ErrShuttingDown error`.
- Test: acquiring up to `size` returns distinct conns and drains `Available` to 0; the extra Acquire blocks until a Release; releases dispatch to waiters in FIFO order; a timed-out Acquire returns `context.DeadlineExceeded` and deregisters its waiter with no leaked conn; `Shutdown` frees all waiters with `ErrShuttingDown` and joins `Run`; every conn is conserved under churn.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go get go.uber.org/goleak
go mod tidy
```

### One goroutine owns the free list and the waiter queue

The whole design turns on a single ownership rule: exactly one goroutine — the
`Run` loop — ever touches `free` (the list of idle connection ids) and `waiters`
(the queue of reply channels for blocked callers). Both are ordinary local
variables inside `Run`. Because no other goroutine reads or writes them, they need
no mutex, and `go test -race` proves it. Every state change is a message on a
channel:

1. **Acquire** sends a freshly made, buffered cap-1 reply channel on `acquires`.
   If a connection is free the loop pops it and sends a `*Lease` straight back on
   that reply channel; otherwise it appends the reply channel to `waiters` and the
   caller blocks on the receive.
2. **Release** sends the connection id back on `releases`. The loop pops the oldest
   waiter — `waiters[0]`, the longest-waiting caller — and hands it the connection.
   If there is no waiter, the connection returns to `free`. This is what makes
   dispatch strictly FIFO: append to the tail, serve from the head.
3. **Cancel** (a timed-out Acquire) sends its reply channel on `cancels` so the
   loop can deregister it.

The reply channel is buffered with capacity one for the reason every request-reply
actor is: if a caller times out and stops receiving, a Release that already picked
that waiter must still complete its send. With a cap-1 buffer the loop drops the
lease into the buffer and moves on; the abandoned channel is garbage-collected. An
unbuffered reply would wedge the loop forever the first time a waiter left early.

### The cancellation race is where connections leak

The subtle bug this exercise pins down is the interleaving between a Release and a
deadline firing. A caller is queued as `waiters[i]`. Its context deadline expires,
so it wants to leave. But between the deadline firing and the loop hearing about
it, a Release may have already popped this exact waiter and dropped a `*Lease` into
its cap-1 buffer. If the caller simply returned and dropped its reply channel, that
connection would be lost forever — `Available` would silently decay until the pool
is dead.

The fix keeps recovery inside the loop, the one place that owns the accounting. On
`cancels`, the loop searches `waiters` for the reply channel. If it is still there,
no Release touched it: remove it, done. If it is *not* there, a Release already
delivered a lease into its buffer — the loop drains that buffer (`<-reply`) and
re-dispatches the recovered connection to the next real waiter, or back to `free`.
The timed-out caller never drains its own reply channel, so exactly one goroutine
(the loop) ever consumes it. That single-consumer discipline is what makes the
recovery race-free and keeps the connection count exact.

`Acquire` threads the context through both sends: the enqueue send selects on
`ctx.Done()` and `quit`, and the reply wait selects on `ctx.Done()`, `quit`, and
the reply. On a deadline it returns `context.Cause(ctx)` — `context.DeadlineExceeded`
for a timeout — after handing the deregistration to the loop.

Create `leasepool.go`:

```go
package leasepool

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrShuttingDown is returned to any caller blocked on Acquire when the pool is
// shut down.
var ErrShuttingDown = errors.New("leasepool: shutting down")

// Lease is a checked-out connection. The caller must call Release exactly when
// it is done; Release is idempotent and routes the connection back through the
// actor loop, never through a mutex.
type Lease struct {
	Conn int

	pool     *Pool
	released atomic.Bool
}

// Release returns the connection to the pool. Calling it more than once is a
// no-op, so a defer plus an explicit release cannot double-return a connection.
func (l *Lease) Release() {
	if l.released.Swap(true) {
		return
	}
	select {
	case l.pool.releases <- l.Conn:
	case <-l.pool.quit:
	}
}

// snapshot is the loop's answer to a state query: free connections and queued
// waiters, read by the one goroutine that also mutates them.
type snapshot struct {
	free    int
	waiting int
}

// Pool fronts a fixed set of connections. A single actor goroutine owns the free
// list and the FIFO waiter queue; there is no mutex on that state.
type Pool struct {
	size int

	acquires chan chan *Lease // request: caller hands in a cap-1 reply channel
	releases chan int         // a connection coming back
	cancels  chan chan *Lease // deregister a waiter by its reply channel
	queries  chan chan snapshot

	quit    chan struct{}
	done    chan struct{}
	closing atomic.Bool
}

// New builds a pool of size connections numbered 0..size-1. Start it with go
// p.Run() before calling Acquire.
func New(size int) *Pool {
	return &Pool{
		size:     size,
		acquires: make(chan chan *Lease),
		releases: make(chan int),
		cancels:  make(chan chan *Lease),
		queries:  make(chan chan snapshot),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Run is the actor loop. It is the only goroutine that touches free or waiters,
// so those need no lock. Every mutation is a message: an acquire, a release, a
// cancel, or a query.
func (p *Pool) Run() {
	defer close(p.done)

	free := make([]int, p.size)
	for i := range p.size {
		free[i] = i
	}
	var waiters []chan *Lease

	for {
		select {
		case reply := <-p.acquires:
			if len(free) > 0 {
				conn := free[0]
				free = free[1:]
				reply <- p.newLease(conn)
			} else {
				waiters = append(waiters, reply)
			}

		case conn := <-p.releases:
			if len(waiters) > 0 {
				w := waiters[0]
				waiters = waiters[1:]
				w <- p.newLease(conn) // hand to the longest-waiting caller
			} else {
				free = append(free, conn)
			}

		case reply := <-p.cancels:
			found := false
			for i, w := range waiters {
				if w == reply {
					waiters = append(waiters[:i], waiters[i+1:]...)
					found = true
					break
				}
			}
			if !found {
				// A release already delivered a lease into this waiter's cap-1
				// buffer before it cancelled. Recover that connection so it is
				// not lost, and give it to the next real waiter or the free list.
				if lease := <-reply; lease != nil {
					if len(waiters) > 0 {
						w := waiters[0]
						waiters = waiters[1:]
						w <- p.newLease(lease.Conn)
					} else {
						free = append(free, lease.Conn)
					}
				}
			}

		case rc := <-p.queries:
			rc <- snapshot{free: len(free), waiting: len(waiters)}

		case <-p.quit:
			for _, w := range waiters {
				w <- nil // unblock every waiter with a shutting-down signal
			}
			return
		}
	}
}

func (p *Pool) newLease(conn int) *Lease {
	return &Lease{Conn: conn, pool: p}
}

// Acquire checks out a connection, blocking in FIFO order when all are busy. It
// honours ctx: if the deadline fires while waiting, it returns the context cause
// and deregisters its waiter so no connection is leaked to a caller that left.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	reply := make(chan *Lease, 1) // cap-1: a late dispatch never blocks the loop
	select {
	case p.acquires <- reply:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	case <-p.quit:
		return nil, ErrShuttingDown
	}

	select {
	case lease := <-reply:
		if lease == nil {
			return nil, ErrShuttingDown
		}
		return lease, nil
	case <-ctx.Done():
		// Hand the deregistration to the loop. If a lease was already dropped in
		// our buffer, the loop recovers it; we never drain reply ourselves here.
		select {
		case p.cancels <- reply:
		case <-p.quit:
		}
		return nil, context.Cause(ctx)
	case <-p.quit:
		return nil, ErrShuttingDown
	}
}

// Available reports how many connections are currently free. The count is read
// inside the loop, so it is never torn against a concurrent acquire or release.
func (p *Pool) Available() int {
	return p.query().free
}

// Waiting reports how many callers are queued for a connection. Like Available,
// it is read inside the loop, so it observes FIFO enqueue order without racing.
func (p *Pool) Waiting() int {
	return p.query().waiting
}

func (p *Pool) query() snapshot {
	rc := make(chan snapshot, 1)
	select {
	case p.queries <- rc:
	case <-p.quit:
		return snapshot{}
	}
	select {
	case s := <-rc:
		return s
	case <-p.quit:
		return snapshot{}
	}
}

// Shutdown stops the loop, unblocks every waiting Acquire with ErrShuttingDown,
// and joins Run. It is safe to call more than once.
func (p *Pool) Shutdown() {
	if p.closing.Swap(true) {
		return
	}
	close(p.quit)
	<-p.done
}
```

### The runnable demo

The demo exhausts a pool of two, queues a third caller, then releases one lease so
the queued caller is handed the freed connection through the loop. It synchronizes
on `Waiting()` and on a done channel so the printed order is deterministic.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/leasepool"
)

func main() {
	p := leasepool.New(2)
	go p.Run()
	defer p.Shutdown()

	ctx := context.Background()

	// Check out both connections; Available drops to zero.
	a, _ := p.Acquire(ctx)
	b, _ := p.Acquire(ctx)
	fmt.Printf("acquired %d and %d, available=%d\n", a.Conn, b.Conn, p.Available())

	// A third caller must wait until a connection is released. Start it, then
	// release a: the waiter is handed the freed connection through the loop.
	got := make(chan int, 1)
	finished := make(chan struct{})
	go func() {
		l, _ := p.Acquire(ctx)
		got <- l.Conn
		l.Release()
		close(finished)
	}()
	for p.Waiting() != 1 {
	}
	fmt.Printf("third caller queued, waiting=%d\n", p.Waiting())

	a.Release()
	fmt.Printf("waiter received conn %d\n", <-got)

	<-finished
	b.Release()
	fmt.Printf("all released, available=%d\n", p.Available())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquired 0 and 1, available=0
third caller queued, waiting=1
waiter received conn 0
all released, available=2
```

### Tests

`TestAcquireDistinctThenExhausted` checks out all `size` connections, asserting
each is distinct and that `Available` reaches 0, then releases and confirms the
count returns to `size`. `TestExtraAcquireBlocksUntilRelease` shows the `size+1`-th
Acquire blocks while the pool is full and unblocks the moment a lease is released.
`TestReleasesDispatchFIFO` enqueues four waiters one at a time — gated on
`Waiting()` so their queue order is known — then releases through a strict handoff
chain and asserts the recorded service order matches the enqueue order.
`TestDeadlineDeregistersWaiterNoLeak` is the core one: waiter A (short deadline) is
enqueued ahead of patient waiter B; when A times out with `context.DeadlineExceeded`
it must deregister, and the single held connection, when released, must reach B and
never vanish. `TestShutdownFreesWaiters` blocks three callers and asserts
`Shutdown` frees every one with `ErrShuttingDown` and joins `Run`.
`TestConnConservationUnderChurn` hammers the pool with 300 callers, half on tight
deadlines, and asserts no connection is ever held by two callers and every
connection is back at the end. `TestMain` wraps everything in `goleak` so a leaked
`Run` goroutine fails the suite.

Create `leasepool_test.go`:

```go
package leasepool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// waitFor spins on cond until it holds or the deadline passes, so a test can
// synchronize on an actor state change without sleeping on a guessed duration.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met before deadline")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAcquireDistinctThenExhausted(t *testing.T) {
	t.Parallel()
	const size = 4
	p := New(size)
	go p.Run()
	defer p.Shutdown()

	ctx := context.Background()
	seen := make(map[int]bool)
	leases := make([]*Lease, size)
	for i := range size {
		l, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire %d error = %v", i, err)
		}
		if seen[l.Conn] {
			t.Fatalf("connection %d handed out twice", l.Conn)
		}
		seen[l.Conn] = true
		leases[i] = l
	}
	if got := p.Available(); got != 0 {
		t.Fatalf("Available = %d, want 0 when exhausted", got)
	}
	for _, l := range leases {
		l.Release()
	}
	waitFor(t, func() bool { return p.Available() == size })
}

func TestExtraAcquireBlocksUntilRelease(t *testing.T) {
	t.Parallel()
	const size = 2
	p := New(size)
	go p.Run()
	defer p.Shutdown()

	ctx := context.Background()
	a, _ := p.Acquire(ctx)
	_, _ = p.Acquire(ctx)
	if got := p.Available(); got != 0 {
		t.Fatalf("Available = %d, want 0", got)
	}

	got := make(chan *Lease, 1)
	go func() {
		l, _ := p.Acquire(ctx)
		got <- l
	}()
	waitFor(t, func() bool { return p.Waiting() == 1 })

	select {
	case <-got:
		t.Fatal("Acquire returned while pool was exhausted")
	default:
	}

	a.Release()
	select {
	case l := <-got:
		if l == nil {
			t.Fatal("blocked Acquire returned nil lease")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Acquire never unblocked after Release")
	}
}

func TestReleasesDispatchFIFO(t *testing.T) {
	t.Parallel()
	p := New(1)
	go p.Run()
	defer p.Shutdown()

	ctx := context.Background()
	held, _ := p.Acquire(ctx) // hold the only connection

	const w = 4
	served := make(chan int, w)
	for i := range w {
		go func() {
			l, err := p.Acquire(ctx)
			if err != nil {
				return
			}
			served <- i
			l.Release() // hand off to the next waiter, forming a strict chain
		}()
		// Enqueue exactly one waiter at a time so the queue order is known.
		waitFor(t, func() bool { return p.Waiting() == i+1 })
	}

	held.Release() // starts the chain: oldest waiter first
	for k := range w {
		if got := <-served; got != k {
			t.Fatalf("FIFO violated: position %d served waiter %d", k, got)
		}
	}
}

func TestDeadlineDeregistersWaiterNoLeak(t *testing.T) {
	t.Parallel()
	p := New(1)
	go p.Run()
	defer p.Shutdown()

	held, _ := p.Acquire(context.Background()) // hold the only connection

	// Waiter A times out; it is enqueued first.
	ctxA, cancelA := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelA()
	errA := make(chan error, 1)
	go func() {
		_, err := p.Acquire(ctxA)
		errA <- err
	}()
	waitFor(t, func() bool { return p.Waiting() == 1 })

	// Waiter B is patient; enqueued behind A.
	bConn := make(chan int, 1)
	go func() {
		l, err := p.Acquire(context.Background())
		if err != nil {
			return
		}
		bConn <- l.Conn
		l.Release()
	}()
	waitFor(t, func() bool { return p.Waiting() == 2 })

	if err := <-errA; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out Acquire error = %v, want DeadlineExceeded", err)
	}
	// A must have deregistered itself, leaving only B queued.
	waitFor(t, func() bool { return p.Waiting() == 1 })

	// The single connection is still held; releasing it must reach B, not vanish.
	held.Release()
	select {
	case c := <-bConn:
		if c != 0 {
			t.Fatalf("waiter B got conn %d, want 0", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection lost: patient waiter B never served after A timed out")
	}
	waitFor(t, func() bool { return p.Available() == 1 })
}

func TestShutdownFreesWaiters(t *testing.T) {
	t.Parallel()
	p := New(1)
	go p.Run()

	held, _ := p.Acquire(context.Background()) // hold the only connection
	_ = held

	const w = 3
	errs := make(chan error, w)
	for range w {
		go func() {
			_, err := p.Acquire(context.Background())
			errs <- err
		}()
	}
	waitFor(t, func() bool { return p.Waiting() == w })

	p.Shutdown() // must unblock every waiter and join Run

	for range w {
		if err := <-errs; !errors.Is(err, ErrShuttingDown) {
			t.Fatalf("waiter error = %v, want ErrShuttingDown", err)
		}
	}
	p.Shutdown() // idempotent
}

func TestConnConservationUnderChurn(t *testing.T) {
	t.Parallel()
	const size = 4
	p := New(size)
	go p.Run()
	defer p.Shutdown()

	var held [size]atomic.Bool // detects the same conn handed to two callers
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Go(func() {
			// Half the callers use a tight deadline to exercise the cancel path.
			ctx := context.Background()
			var cancel context.CancelFunc
			if i%2 == 0 {
				ctx, cancel = context.WithTimeout(ctx, time.Duration(i%5)*time.Millisecond)
				defer cancel()
			}
			l, err := p.Acquire(ctx)
			if err != nil {
				return
			}
			if held[l.Conn].Swap(true) {
				t.Errorf("connection %d checked out by two callers at once", l.Conn)
			}
			held[l.Conn].Store(false)
			l.Release()
		})
	}
	wg.Wait()

	// Every connection must be back: none duplicated, none lost.
	waitFor(t, func() bool { return p.Available() == size })
}
```

## Review

Correct here means three invariants hold at once: connections handed out are
distinct while checked out, waiters are served oldest-first, and no connection is
ever duplicated or lost across cancellation and shutdown. All three follow from the
single ownership rule — only `Run` touches `free` and `waiters` — so there is no
mutex and `-race` has nothing to flag. FIFO falls out of appending waiters at the
tail and serving from the head; the cancellation-leak bug is closed by keeping
recovery inside the loop, which drains any lease a Release had already dropped into
a departing waiter's cap-1 buffer and re-dispatches it. `TestConnConservationUnderChurn`
proves conservation by asserting the same connection is never held twice and that
`Available` returns to `size` after 300 racing callers, half of them timing out;
`TestDeadlineDeregistersWaiterNoLeak` proves the exact interleaving where a patient
waiter must still be served after an earlier one leaves. The production bug this
pattern prevents is the slow connection-pool death: a mutex-and-condvar pool where
a cancelled waiter drops a connection it was already assigned, so capacity silently
erodes until every request blocks forever.

## Resources

- [Go spec: Channel types](https://go.dev/ref/spec#Channel_types) — the `chan chan *Lease` request-reply shape the pool is built on.
- [Go Memory Model](https://go.dev/ref/mem) — why single-goroutine ownership of the free list and waiter queue is race-free without a lock.
- [`context.Cause`](https://pkg.go.dev/context#Cause) — surfacing `DeadlineExceeded` as the reason a blocked Acquire was abandoned.
- [Effective Go: Share by communicating](https://go.dev/doc/effective_go#sharing) — the actor discipline that replaces the pool's mutex with a message loop.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [09-actor-self-observability.md](09-actor-self-observability.md) | Next: [11-control-plane-priority-actor.md](11-control-plane-priority-actor.md)
