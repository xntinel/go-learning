# Exercise 3: Fixed-size connection pool with blocking Acquire

A bounded resource pool is the backbone of every database or HTTP client layer:
a fixed set of expensive connections, handed out on `Acquire`, returned on
`Release`, and blocking callers when everything is checked out rather than opening
unbounded connections. This module builds that pool with a `Cond`, and it is the
cleanest illustration of `Signal` versus `Broadcast` in one type: `Release`
signals one waiter (one slot freed helps exactly one caller), while `Close`
broadcasts to wake every blocked `Acquire` so they return an error instead of
leaking forever.

## What you'll build

```text
pool/                       independent module: example.com/pool
  go.mod                    module path example.com/pool
  pool.go                   type Pool: New, Acquire, Release, Close; ErrPoolClosed
  cmd/
    demo/
      main.go               more workers than connections, all draining cleanly
  pool_test.go              block-until-release, Close wakes all, invariant stress
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a `Pool` of `*Conn` with `New(size)`, `Acquire() (*Conn, error)` (blocks until a connection is free or the pool is closed), `Release(*Conn)`, and `Close()`; a sentinel `ErrPoolClosed` wrapped so callers can match it with `errors.Is`.
- Test: `Acquire` beyond capacity blocks until a `Release`; `Close` wakes every blocked `Acquire` with `ErrPoolClosed`; a concurrent stress test asserts checked-out never exceeds size.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p go-solutions/15-sync-primitives/06-sync-cond/03-connection-pool/cmd/demo
cd go-solutions/15-sync-primitives/06-sync-cond/03-connection-pool
```

### Signal to hand off one slot, Broadcast to drain on shutdown

The pool holds a slice of free connections. `Acquire` waits on the predicate
"there is a free connection OR the pool is closed". When `Release` returns one
connection to the free list, exactly one blocked `Acquire` can proceed, and every
waiter is interchangeable — so `Release` calls `Signal`. Waking all of them on a
single freed connection would just make the losers re-check and re-park.

`Close` is different: it flips a terminal `closed` flag that satisfies EVERY
blocked `Acquire` at once (they must all wake, observe `closed`, and return
`ErrPoolClosed`). That is a `Broadcast`. Using `Signal` in `Close` would wake one
waiter and leave the rest parked forever — a goroutine leak on shutdown, which is
exactly the bug graceful shutdown is supposed to prevent. This split — `Signal`
on the incremental freeing, `Broadcast` on the terminal state — is the pattern
`database/sql`'s connection pool uses internally.

The predicate is a compound `for len(p.free) == 0 && !p.closed`. After `Wait`
returns, the code must re-check WHY it woke: if `p.closed`, return the error; only
otherwise pop a connection. Checking `closed` before popping is essential, because
`Close` and a late `Release` can race and the woken goroutine must honor the
terminal state.

`ErrPoolClosed` is a package-level sentinel created with `errors.New`. `Acquire`
returns it wrapped with `%w` via `fmt.Errorf` so callers get a contextual message
while still matching the sentinel with `errors.Is` — the standard error contract
for a library.

Create `pool.go`:

```go
package pool

import (
	"errors"
	"fmt"
	"sync"
)

// ErrPoolClosed is returned by Acquire once the pool has been closed.
var ErrPoolClosed = errors.New("pool: closed")

// Conn is a stand-in for an expensive pooled resource (a DB connection, a socket).
type Conn struct {
	ID int
}

// Pool hands out a fixed set of connections, blocking Acquire when all are
// checked out and waking blocked callers on Release or Close.
type Pool struct {
	mu     sync.Mutex
	cond   *sync.Cond
	free   []*Conn
	size   int
	closed bool
}

// New builds a pool of size connections (minimum 1), all initially free.
func New(size int) *Pool {
	if size < 1 {
		size = 1
	}
	p := &Pool{size: size, free: make([]*Conn, 0, size)}
	p.cond = sync.NewCond(&p.mu)
	for i := range size {
		p.free = append(p.free, &Conn{ID: i})
	}
	return p
}

// Acquire returns a free connection, blocking until one is released. It returns
// ErrPoolClosed if the pool is (or becomes) closed while waiting.
func (p *Pool) Acquire() (*Conn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.free) == 0 && !p.closed {
		p.cond.Wait()
	}
	if p.closed {
		return nil, fmt.Errorf("acquire: %w", ErrPoolClosed)
	}
	c := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	return c, nil
}

// Release returns a connection to the pool and wakes one blocked Acquire. A
// Release after Close is a no-op.
func (p *Pool) Release(c *Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.free = append(p.free, c)
	p.cond.Signal()
}

// Close marks the pool closed and wakes every blocked Acquire so it returns
// ErrPoolClosed instead of leaking.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
}

// Available reports how many connections are currently free.
func (p *Pool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.free)
}
```

### The runnable demo

The demo starts more workers than there are connections, so some `Acquire` calls
block until others `Release`. Every worker does a tiny unit of work and returns
its connection, so all workers complete and the pool ends fully stocked.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"
	"sync"
	"time"

	"example.com/pool"
)

func main() {
	p := pool.New(2)

	const workers = 6
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Acquire()
			if err != nil {
				return
			}
			defer p.Release(c)
			time.Sleep(5 * time.Millisecond) // simulate work on the connection
		}()
	}
	wg.Wait()

	fmt.Printf("all %d workers done; available = %d\n", workers, p.Available())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
all 6 workers done; available = 2
```

### Tests

`TestAcquireBlocksUntilRelease` checks out both connections, launches a third
`Acquire`, and uses `synctest.Wait()` to confirm it is durably parked; a single
`Release` then unblocks exactly one waiter. `TestCloseWakesAllWaiters` parks
several `Acquire` calls and asserts `Close` wakes every one of them with
`ErrPoolClosed` (matched via `errors.Is`) — and because `synctest.Test` reports a
deadlock if any goroutine never exits, a `Signal`-in-`Close` bug would fail the
test as a leak. `TestNeverExceedsSize` runs concurrent acquire/release under
`-race` with an atomic counter asserting the checked-out count never crosses the
pool size.

Create `pool_test.go`:

```go
package pool

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestAcquireBlocksUntilRelease(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := New(2)
		c1, _ := p.Acquire()
		_, _ = p.Acquire() // pool now empty

		got := make(chan *Conn, 1)
		go func() {
			c, err := p.Acquire()
			if err != nil {
				t.Errorf("Acquire returned error: %v", err)
			}
			got <- c
		}()

		synctest.Wait() // third Acquire durably blocked
		select {
		case <-got:
			t.Fatal("Acquire returned while the pool was exhausted")
		default:
		}

		p.Release(c1)
		synctest.Wait()
		select {
		case c := <-got:
			if c == nil {
				t.Fatal("Acquire returned a nil connection")
			}
		default:
			t.Fatal("Acquire did not return after Release")
		}
	})
}

func TestCloseWakesAllWaiters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		p := New(1)
		_, _ = p.Acquire() // exhaust

		const waiters = 4
		errs := make(chan error, waiters)
		for range waiters {
			go func() {
				_, err := p.Acquire()
				errs <- err
			}()
		}

		synctest.Wait() // all waiters durably blocked
		p.Close()
		synctest.Wait() // all waiters woken and returned

		for range waiters {
			select {
			case err := <-errs:
				if !errors.Is(err, ErrPoolClosed) {
					t.Fatalf("Acquire error = %v, want ErrPoolClosed", err)
				}
			default:
				t.Fatal("a blocked Acquire was not woken by Close")
			}
		}
	})
}

func TestNeverExceedsSize(t *testing.T) {
	t.Parallel()

	const size = 3
	p := New(size)

	var inUse, maxInUse atomic.Int64
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Acquire()
			if err != nil {
				return
			}
			n := inUse.Add(1)
			for {
				m := maxInUse.Load()
				if n <= m || maxInUse.CompareAndSwap(m, n) {
					break
				}
			}
			inUse.Add(-1)
			p.Release(c)
		}()
	}
	wg.Wait()

	if got := maxInUse.Load(); got > int64(size) {
		t.Fatalf("max checked-out = %d, want <= %d", got, size)
	}
	if got := p.Available(); got != size {
		t.Fatalf("available after drain = %d, want %d", got, size)
	}
}

func ExamplePool() {
	p := New(1)
	c, _ := p.Acquire()
	p.Release(c)
	p.Close()
	_, err := p.Acquire()
	fmt.Println(errors.Is(err, ErrPoolClosed))
	// Output: true
}
```

## Review

The pool is correct when three things hold. No caller ever holds a connection the
pool did not hand out, and the number checked out never exceeds `size` — pinned by
`TestNeverExceedsSize`, which depends on `Acquire` blocking rather than
over-issuing. A blocked `Acquire` always wakes: `Release` signals one waiter,
`Close` broadcasts to all. And shutdown never leaks a goroutine: every parked
`Acquire` returns `ErrPoolClosed` after `Close`, which `synctest.Test` verifies by
reporting a deadlock if any waiter is left parked. Run `go test -race` to confirm
the free-list mutation is fully serialized.

The defining mistake here is using `Signal` in `Close`. With one blocked waiter
the tests pass; with several, `Close` wakes one and the rest leak — the precise
failure `TestCloseWakesAllWaiters` and the synctest deadlock check exist to catch.
The mirror mistake is `Broadcast` in `Release`: it works but thundering-herds
every waiter awake on a single freed connection, so only one succeeds and the rest
burn a re-check. `Signal` for the incremental case, `Broadcast` for the terminal
case.

## Resources

- [`sync.Cond`](https://pkg.go.dev/sync#Cond) — `Signal` versus `Broadcast` on shared state.
- [`database/sql`](https://pkg.go.dev/database/sql) — the standard library's real connection pool, whose blocking-acquire semantics this mirrors.
- [`errors.Is`](https://pkg.go.dev/errors#Is) — matching a wrapped sentinel error.

---

Back to [02-countdown-latch.md](02-countdown-latch.md) | Next: [04-single-flight-cache.md](04-single-flight-cache.md)
