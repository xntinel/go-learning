# Exercise 1: Connection Pool — Borrow and Return With defer conn.Close()

A connection pool is the canonical `defer` artifact: a caller acquires a
connection, does work, and returns it with `defer conn.Close()` so the return
happens on every exit path — success, error, or panic. This module builds a
small, concurrency-safe pool with a buffered-channel free list, an idempotent
`Close`, and a context-bounded `Acquire`, and then hardens it with a test that
proves it rejects acquisitions after the pool is closed.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
connpool/                       independent module: example.com/connpool
  go.mod
  internal/pool/pool.go         Pool, Conn; New, Acquire, TryAcquire, Close; idempotent Conn.Close
  cmd/demo/main.go              runnable demo: acquire two, return them, watch Available
  internal/pool/pool_test.go    borrow/return, blocks-until-release, context cancel,
                                32-worker -race safety, rejects-after-close, try-acquire-exhausted
```

- Files: `internal/pool/pool.go`, `cmd/demo/main.go`, `internal/pool/pool_test.go`.
- Implement: a `Pool` with a buffered-channel free list, a blocking `Acquire(ctx) (*Conn, error)` bounded by the context, a non-blocking `TryAcquire(ctx) (*Conn, error)` that returns `ErrPoolExhausted` when the free list is empty, an idempotent `Conn.Close()` guarded by `atomic.Bool.CompareAndSwap`, and `Available()`.
- Test: acquire/release counting, an `Acquire` that blocks until a `Close`, a context-deadline cancel returning `DeadlineExceeded`, a 32-worker concurrent run under `-race`, an `Acquire`-after-`Close` that returns `ErrPoolClosed`, and a `TryAcquire` on a drained pool that returns `ErrPoolExhausted`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/01-connection-pool-defer-return/internal/pool go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/01-connection-pool-defer-return/cmd/demo
cd go-solutions/04-functions/11-defer-stacking-and-resource-cleanup/01-connection-pool-defer-return
```

### The free list is a buffered channel, and that is correct

The original version of this lesson described `Acquire` as "blocking on a
condition variable" and listed "using a channel instead of a `sync.Cond`" as a
mistake. That prose disagreed with the code, which uses a channel — and the code
is right. A buffered channel of capacity `N`, pre-filled with `N` connections, is
a complete free-list-with-blocking-and-cancellation in one primitive:

- Borrowing is `<-p.notify`: it hands back a connection if one is buffered, and
  blocks the goroutine (durably, cheaply) if none is, waking exactly one waiter
  when a connection is returned. That is precisely the semantics a `sync.Cond`
  gives you, without the manual `Lock`/`Wait`/`Signal` choreography and without
  the classic `Cond` hazards (a missed `Signal`, or a `Wait` that forgets to
  re-check its predicate in a loop).
- Cancellation composes for free: because the borrow is a channel receive, you
  put it in a `select` alongside `<-ctx.Done()`, and a request-scoped deadline
  cancels a blocked `Acquire` with no extra machinery. Doing the same with a
  `sync.Cond` requires bolting a timer onto `Wait`, which the standard `Cond` has
  no direct support for.

Where would a `sync.Cond` version differ? You would hold a `sync.Mutex`, keep the
free list in a slice, and write `for len(free) == 0 && !closed { cond.Wait() }`
to reacquire the lock and re-check the predicate on every wake; `release` would
append to the slice and call `cond.Signal()`. That is a legitimate design and the
right one when the wait predicate is more complex than "is there a free item"
(for example, "is there a free item *and* is the circuit closed *and* is this
caller's priority high enough"). For a plain free list, the channel is simpler,
harder to get wrong, and cancellation-ready. This exercise uses the channel.

The one channel subtlety to respect: closing the `notify` channel is how `Close`
tells blocked and future `Acquire` calls that the pool is gone. A receive from a
closed channel returns the zero value with `ok == false` — but only *after* any
buffered items have been drained. So a pool closed while it still has free
connections buffered will hand those out first and only then report
`ErrPoolClosed`. The rejects-after-close test below drains the pool before
closing so the "closed" signal is what the next `Acquire` actually observes.

### Two acquire modes: block, or fail fast with ErrPoolExhausted

Real pools expose both a blocking and a non-blocking borrow. `Acquire` blocks
until a connection frees up or the context is done — the right default when the
caller can wait and has bounded its own patience with a deadline. But some call
sites must not block at all: a health check that has to answer in single-digit
milliseconds, a best-effort cache warmer, or a load-shedding path that would
rather reject a request immediately than let a queue of goroutines pile up on an
exhausted pool. For them, `TryAcquire` does a non-blocking receive — a `select`
with a `default` case — and returns `ErrPoolExhausted` the instant the free list
is empty, so the caller can shed load or fall back instead of parking a
goroutine. This is why `ErrPoolExhausted` is a real, returned sentinel and not
decoration: it is the signal a fail-fast borrower matches with `errors.Is`.

`TryAcquire` still checks the context first, so a caller that passes an
already-cancelled context gets the context error rather than a spurious
connection; and it still observes a closed pool through the `ok` flag on the
receive, returning `ErrPoolClosed` exactly as `Acquire` does.

### Idempotent Close is the exactly-once invariant

`Conn.Close` may plausibly be called twice: once by a `defer` and once
explicitly, or by two goroutines that both think they own the connection. Returning
the same connection to the pool twice would over-fill the free list and hand one
physical connection to two callers at once — a correctness disaster. So `Close`
transitions an `atomic.Bool` from `false` to `true` with `CompareAndSwap`; exactly
one caller wins and performs the single return-to-pool, and every later call is a
no-op that returns `nil`.

Create `internal/pool/pool.go`:

```go
package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

// Sentinel errors callers match with errors.Is.
var (
	ErrPoolExhausted = errors.New("pool exhausted")
	ErrPoolClosed    = errors.New("pool closed")
)

// Conn is a borrowed connection. The borrower returns it with Close, which is
// idempotent: a second call (from a stray defer, or a racing goroutine) is a
// no-op rather than a double return-to-pool.
type Conn struct {
	id     int
	pool   *Pool
	closed atomic.Bool
}

func (c *Conn) ID() int { return c.id }

// Do runs work on the connection unless it has already been returned.
func (c *Conn) Do(work func() error) error {
	if c.closed.Load() {
		return errors.New("conn closed")
	}
	return work()
}

// Close returns the connection to the pool exactly once.
func (c *Conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.pool.release(c)
}

func (c *Conn) reset() { c.closed.Store(false) }

// Pool is a fixed-size connection pool. Its free list is a buffered channel of
// capacity == size; Acquire receives from it, release sends back to it.
type Pool struct {
	mu       sync.Mutex
	notify   chan *Conn
	closed   bool
	nextID   int
	capacity int
}

// New builds a pool with capacity connections, all initially free.
func New(capacity int) *Pool {
	if capacity <= 0 {
		capacity = 1
	}
	p := &Pool{
		capacity: capacity,
		notify:   make(chan *Conn, capacity),
	}
	for range capacity {
		c := &Conn{id: p.nextID, pool: p}
		p.nextID++
		p.notify <- c
	}
	return p
}

// Acquire returns a free connection, blocking until one is available or the
// context is done. After Close (with the free list drained) it returns
// ErrPoolClosed.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	select {
	case c, ok := <-p.notify:
		if !ok {
			return nil, ErrPoolClosed
		}
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TryAcquire returns a free connection without blocking. If the context is
// already done it returns the context error; if the pool is closed it returns
// ErrPoolClosed; if no connection is currently free it returns ErrPoolExhausted
// immediately instead of waiting. It is the fail-fast counterpart to Acquire.
func (p *Pool) TryAcquire(ctx context.Context) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case c, ok := <-p.notify:
		if !ok {
			return nil, ErrPoolClosed
		}
		return c, nil
	default:
		return nil, ErrPoolExhausted
	}
}

// release returns a connection to the free list. It is unexported: the caller
// only ever sees Conn.Close.
func (p *Pool) release(c *Conn) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrPoolClosed
	}
	c.reset()
	p.notify <- c
	return nil
}

// Close shuts the pool. It is idempotent and wakes every blocked Acquire.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	close(p.notify)
	return nil
}

// Available reports how many connections are currently free.
func (p *Pool) Available() int { return len(p.notify) }
```

`release` holds the mutex across the send so a concurrent `Close` cannot close the
channel between the `p.closed` check and the send (which would panic on a closed
channel); once `p.closed` is observed true under the lock, `release` returns
`ErrPoolClosed` instead of sending.

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"

	"example.com/connpool/internal/pool"
)

func main() {
	p := pool.New(2)
	defer p.Close()

	ctx := context.Background()
	c1, _ := p.Acquire(ctx)
	c2, _ := p.Acquire(ctx)
	fmt.Println("available after 2 acquires:", p.Available())

	// Pool is exhausted; the non-blocking borrow fails fast instead of parking.
	if _, err := p.TryAcquire(ctx); err != nil {
		fmt.Println("try-acquire on exhausted pool:", err)
	}

	_ = c1.Close()
	fmt.Println("available after 1 return:", p.Available())

	_ = c2.Close()
	fmt.Println("available after 2 returns:", p.Available())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
available after 2 acquires: 0
try-acquire on exhausted pool: pool exhausted
available after 1 return: 1
available after 2 returns: 2
```

### Tests

The four original tests are kept verbatim in substance: acquire/release counting,
an `Acquire` that blocks until a `Close` returns the connection, a context
deadline that cancels a blocked `Acquire` with `DeadlineExceeded`, and a
32-worker concurrent run whose real purpose is to give the race detector
something to find. Added on top is `TestPoolRejectsAcquireAfterClose`, which
drains the single connection first so that the post-`Close` `Acquire` observes the
closed channel (not a buffered leftover) and returns `ErrPoolClosed`; and
`TestPoolTryAcquireReturnsExhaustedWhenDrained`, which borrows the only
connection and then proves the non-blocking `TryAcquire` returns
`ErrPoolExhausted` rather than parking.

Create `internal/pool/pool_test.go`:

```go
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolAcquiresAndReleases(t *testing.T) {
	t.Parallel()

	p := New(2)
	defer p.Close()

	if p.Available() != 2 {
		t.Fatalf("Available() = %d, want 2", p.Available())
	}

	ctx := context.Background()
	c1, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := p.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available() != 0 {
		t.Fatalf("Available() = %d, want 0", p.Available())
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}
	if p.Available() != 1 {
		t.Fatalf("Available() = %d, want 1", p.Available())
	}
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}
	if p.Available() != 2 {
		t.Fatalf("Available() = %d, want 2", p.Available())
	}
}

func TestPoolBlocksUntilRelease(t *testing.T) {
	t.Parallel()

	p := New(1)
	defer p.Close()

	c1, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c2, err := p.Acquire(context.Background())
		if err != nil {
			t.Errorf("Acquire = %v", err)
			return
		}
		_ = c2.Close()
	}()

	time.Sleep(20 * time.Millisecond)
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second Acquire did not return after release")
	}
}

func TestPoolAcquireRespectsContextCancel(t *testing.T) {
	t.Parallel()

	p := New(1)
	defer p.Close()

	c1, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = p.Acquire(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestPoolIsSafeUnderConcurrentAcquire(t *testing.T) {
	t.Parallel()

	p := New(4)
	defer p.Close()

	const workers = 32
	var handled int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Acquire(context.Background())
			if err != nil {
				t.Errorf("Acquire = %v", err)
				return
			}
			defer c.Close()
			if err := c.Do(func() error { atomic.AddInt64(&handled, 1); return nil }); err != nil {
				t.Errorf("Do = %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&handled); got != workers {
		t.Fatalf("handled = %d, want %d", got, workers)
	}
}

func TestPoolRejectsAcquireAfterClose(t *testing.T) {
	t.Parallel()

	p := New(1)
	// Drain the single free connection so the closed-channel signal, not a
	// buffered leftover, is what the next Acquire observes.
	c, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = c

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = p.Acquire(context.Background())
	if !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("err = %v, want ErrPoolClosed", err)
	}
}

func TestPoolTryAcquireReturnsExhaustedWhenDrained(t *testing.T) {
	t.Parallel()

	p := New(1)
	defer p.Close()

	// First TryAcquire succeeds: the pool has one free connection.
	c, err := p.TryAcquire(context.Background())
	if err != nil {
		t.Fatalf("first TryAcquire = %v, want nil", err)
	}

	// Pool is now drained; the non-blocking TryAcquire must fail fast.
	if _, err := p.TryAcquire(context.Background()); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("TryAcquire on drained pool = %v, want ErrPoolExhausted", err)
	}

	// Returning the connection makes TryAcquire succeed again.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.TryAcquire(context.Background()); err != nil {
		t.Fatalf("TryAcquire after return = %v, want nil", err)
	}
}

func TestPoolCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	p := New(1)
	if err := p.Close(); err != nil {
		t.Fatalf("first Close = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

func Example() {
	p := New(1)
	defer p.Close()

	c, _ := p.Acquire(context.Background())
	fmt.Println("id:", c.ID())
	_ = c.Close()
	fmt.Println("available:", p.Available())
	// Output:
	// id: 0
	// available: 1
}
```

## Review

The pool is correct when three invariants hold. First, `Available()` equals the
number of connections not currently borrowed, which the acquire/release test
checks directly. Second, a blocked `Acquire` wakes exactly when a `Close` returns
a connection, which `TestPoolBlocksUntilRelease` proves and which the buffered
channel gives you for free. Third, `Close` on a `Conn` is exactly-once: the
`atomic.Bool.CompareAndSwap` means a stray second `defer conn.Close()` cannot
return the same connection twice — remove that guard and the concurrent test
under `-race`, or a double-close, would corrupt the free list.

The trap to avoid in the rejects-after-close behavior is forgetting that a closed
buffered channel drains its buffer before signalling closed; a test that closes a
full pool and expects `ErrPoolClosed` immediately is testing the wrong thing.
Bound `Acquire` with the caller's context so a request that is cancelled stops
waiting for a connection that may never come; the pool never polices how long a
borrower holds a connection, so the deadline on `Acquire` is the only backstop
against a slow borrower starving everyone else. Where blocking is unacceptable —
a latency-critical health check, a load-shedding path — reach for `TryAcquire`
and treat `ErrPoolExhausted` as the fail-fast signal to shed or fall back rather
than queue behind an exhausted pool. Run `go test -race` — the
32-worker test exists precisely to let the detector prove the channel and the
atomic actually serialize access.

## Resources

- [Go Language Specification: Defer statements](https://go.dev/ref/spec#Defer_statements)
- [sync/atomic: Bool.CompareAndSwap](https://pkg.go.dev/sync/atomic#Bool.CompareAndSwap)
- [sync.Cond](https://pkg.go.dev/sync#Cond) — the alternative free-list primitive discussed above.
- [Go Blog: Go Concurrency Patterns](https://go.dev/blog/pipelines) — channels as synchronization.

---

Back to [00-concepts.md](00-concepts.md) | Next: [02-tx-manager-defer-rollback.md](02-tx-manager-defer-rollback.md)
