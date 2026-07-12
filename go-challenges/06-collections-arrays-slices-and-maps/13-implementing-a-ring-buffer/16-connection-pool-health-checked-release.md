# Exercise 16: Bounded Connection Pool with Health-Checked Release

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

`database/sql`'s `MaxIdleConns`, pgxpool, and go-redis's connection pool all
solve the same problem: dialing a new TCP connection, a new TLS handshake, a
new authentication round trip, is expensive, so a handful of live
connections are kept around between requests and handed out on demand. The
shape underneath is a ring buffer of connections instead of samples or
timestamps -- `Acquire` pops from the tail, `Release` pushes back at the
head, and the fixed capacity bounds how many idle connections a process
holds open at once, exactly the way this lesson's core ring bounds any other
resource.

What is unique to a connection pool, and not something the plain ring
buffer this lesson opened with ever has to think about, is that the thing
being pooled can go bad while nobody is looking. A database connection can
be dropped by the server, killed by a load balancer's idle timeout, or left
in a broken state by a query that panicked halfway through. A ring buffer of
integers never needs to ask "is this element still good" -- an `int` cannot
go stale. A ring buffer of connections absolutely does, and a pool that
returns a connection to circulation without checking is a pool that
guarantees every future caller eventually gets handed something that is
already broken.

This module builds `Pool[T]`, a bounded idle-connection pool that
health-checks every connection before trusting it again, over the `Conn`
interface any resource with a `Healthy() bool` and a `Close() error` can
satisfy.

This module is fully self-contained: its own `go mod init`, a reusable
package, and its tests. Nothing here imports another exercise.

## What you'll build

```text
connpool/                 module example.com/connpool
  go.mod                   go 1.24
  connpool.go              Conn interface; Pool[T]; New, Acquire, Release, Len, Cap;
                           two sentinel errors
  connpool_test.go          config validation, empty-pool Acquire, round-trip,
                           unhealthy-on-release, full-pool overflow, FIFO order,
                           the naive-release contrast, concurrency, ExamplePool_Acquire
```

- Files: `connpool.go`, `connpool_test.go`.
- Implement: the `Conn` interface (`Healthy() bool`, `Close() error`); `New[T Conn](capacity int) (*Pool[T], error)` rejecting a non-positive capacity with `ErrInvalidCapacity`; `(*Pool[T]).Acquire() (T, error)` popping the oldest idle connection or returning `ErrPoolEmpty`; `(*Pool[T]).Release(conn T) error` health-checking `conn` before storing it, closing and discarding it if unhealthy or if the pool is already at capacity.
- Test: config validation; `Acquire` on an empty pool; a healthy round trip that never closes the connection; an unhealthy `Release` that closes and discards; a `Release` at capacity that closes the extra connection instead of leaking it; FIFO ordering across two idle connections; the naive-release contrast; `Pool[T]` is safe for concurrent use; and `ExamplePool_Acquire` as the runnable demonstration.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
go mod edit -go=1.24
```

### Returning a resource to a ring is not the same as returning it safely

Every prior module's `Push` accepted whatever it was handed and stored it.
A connection pool cannot afford that, because "whatever it was handed" might
be a connection that already failed mid-request. The naive `Release` looks
like ordinary ring bookkeeping, because it is nothing but ring bookkeeping:

```go
func naiveRelease(p *Pool[T], conn T) {
    // ring bookkeeping, correctly...
    p.items[p.head] = conn
    p.head = (p.head + 1) % len(p.items)
    p.size++
    // ...but conn.Healthy() was never called.
}
```

It compiles, and on a healthy connection it behaves identically to the
correct version -- which is exactly why this bug survives code review and a
happy-path integration test. The failure only appears once a connection
actually breaks in production: that connection gets released, the naive
path stores it without a second thought, and the very next `Acquire` hands
it straight back out. The caller tries to use it, it fails again, gets
released again, and recirculates forever. One bad connection does not just
sit uselessly in the pool -- it actively poisons every future request that
happens to draw it, indefinitely, because nothing in the naive path ever
removes it from rotation.

`Release` in this package checks `conn.Healthy()` first and calls
`conn.Close()` instead of storing anything that fails the check. There is a
second, quieter version of the same discipline at the opposite boundary:
when the pool is already holding `Cap()` idle connections and one more is
released, the correct move is not to silently drop the reference -- doing
that would leak the connection's underlying socket or file descriptor,
since nothing else in the program still holds a pointer to it and nothing
ever called `Close`. A resource pool's "ring is full" branch has to close
what it discards, not just forget it; a plain `Ring[T]` of value types never
has anything to close, which is precisely why this detail is new to this
module and not something an earlier one needed to name.

Create `connpool.go`:

```go
// Package connpool implements a bounded idle-connection pool over a fixed
// ring: a small, fixed number of live connections handed out to callers and
// returned when they are done, exactly the shape behind database/sql's
// MaxIdleConns, pgxpool, and go-redis's connection pool.
//
// It exists to get one detail right that a hand-rolled pool routinely gets
// wrong: a connection handed back to the pool must be health-checked before
// it is trusted again. Skipping that check lets one broken connection
// recirculate forever -- every Acquire after it hands the caller a
// connection that will fail, which returns it again, which hands it out
// again. See the package tests for a side-by-side demonstration.
package connpool

import (
	"errors"
	"fmt"
	"sync"
)

// Sentinel errors returned by New and Acquire. Callers should test for them
// with errors.Is rather than by comparing error strings.
var (
	// ErrInvalidCapacity means the requested pool capacity was not positive.
	ErrInvalidCapacity = errors.New("connpool: capacity must be positive")
	// ErrPoolEmpty means Acquire found no idle connection available.
	ErrPoolEmpty = errors.New("connpool: no idle connection available")
)

// Conn is anything a Pool can hold: a live resource that can report its own
// health and be closed when it is no longer wanted.
type Conn interface {
	// Healthy reports whether the connection is still usable.
	Healthy() bool
	// Close releases the connection's underlying resource (socket, file
	// descriptor, ...). It is called exactly once per connection that is
	// created and never returned healthy.
	Close() error
}

// Pool is a bounded ring of idle connections of type T. Acquire removes and
// returns the least-recently-released connection; Release returns one to
// the pool after checking its health.
//
// Pool is safe for concurrent use by multiple goroutines.
type Pool[T Conn] struct {
	mu    sync.Mutex
	items []T
	head  int
	tail  int
	size  int
}

// New returns a Pool holding up to capacity idle connections at once. It
// returns ErrInvalidCapacity if capacity is not positive.
func New[T Conn](capacity int) (*Pool[T], error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidCapacity, capacity)
	}
	return &Pool[T]{items: make([]T, capacity)}, nil
}

// Cap reports the maximum number of idle connections this Pool holds.
func (p *Pool[T]) Cap() int { return len(p.items) }

// Len reports how many idle connections are currently in the pool.
func (p *Pool[T]) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}

// Acquire removes and returns the least-recently-released idle connection.
// It returns ErrPoolEmpty if none is available; the caller is expected to
// dial a new connection itself in that case. Acquire never blocks -- there
// is no queue of waiters, unlike the sync.Cond-based bounded queue elsewhere
// in this lesson.
func (p *Pool[T]) Acquire() (T, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.size == 0 {
		var zero T
		return zero, ErrPoolEmpty
	}
	conn := p.items[p.tail]
	var zero T
	p.items[p.tail] = zero // drop the reference to the connection we just handed out
	p.tail = (p.tail + 1) % len(p.items)
	p.size--
	return conn, nil
}

// Release returns conn to the pool after checking its health. An unhealthy
// connection is closed and discarded instead of being stored: recirculating
// a broken connection would make every future Acquire hand out a connection
// that is already known to fail. If the pool is already holding Cap() idle
// connections, conn is closed and discarded too, rather than silently
// dropping the reference and leaking its underlying resource -- a pool sits
// at capacity by design once callers are returning connections faster than
// they are being acquired.
func (p *Pool[T]) Release(conn T) error {
	if !conn.Healthy() {
		return conn.Close()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.size == len(p.items) {
		return conn.Close()
	}
	p.items[p.head] = conn
	p.head = (p.head + 1) % len(p.items)
	p.size++
	return nil
}
```

### Using it

Construct one `Pool[T]` per resource type at startup, sized to the maximum
number of idle connections you want held open, and let request handlers
`Acquire` at the start of their work and `Release` at the end -- typically
from a `defer`. `Pool[T]` locks internally, so it is safe to share one value
across every handler goroutine without additional synchronization.

`Acquire`'s contract is deliberately non-blocking: `ErrPoolEmpty` means "no
idle connection right now," not "wait for one." A caller that receives it is
expected to dial a fresh connection itself -- the pool is an optimization
for reuse, not a hard cap on concurrency, which is what distinguishes it
from the blocking bounded queue built with `sync.Cond` elsewhere in this
lesson. `Release`'s contract is what this module is really about: a caller
never needs to health-check a connection itself before returning it, and
never needs to remember to `Close` an extra connection at capacity --
`Release` does both, unconditionally, every time.

`ExamplePool_Acquire` in the test file is the runnable demonstration of this
module: `go test` executes it and compares its stdout against the
`// Output:` comment, so the usage shown there cannot drift from the code.

### Tests

`TestNewRejectsNonPositiveCapacity` and `TestAcquireOnEmptyPoolReturnsErrPoolEmpty`
cover construction and the empty-pool boundary.
`TestReleaseThenAcquireRoundTrips` confirms a healthy connection survives a
round trip untouched -- `Close` must never run on it.
`TestReleaseUnhealthyConnectionIsClosedAndDiscarded` and
`TestReleaseAtCapacityClosesTheExtraConnection` pin the two discard paths:
a broken connection never enters the pool, and an extra connection at
capacity is closed rather than silently dropped.
`TestAcquireOrdersOldestFirst` checks FIFO reuse across two idle
connections.

`TestNaiveReleaseRecirculatesBrokenConnection` is the heart of the module.
`naiveRelease` is unexported and unreachable from the package API; it runs
the identical broken-connection-then-`Acquire` sequence through both paths
and shows the naive one hands the same broken connection right back out
while `Pool.Release` closes it and the next `Acquire` correctly reports the
pool empty. `TestPoolIsSafeForConcurrentUse` runs sixteen goroutines
acquiring and releasing against one shared, pre-populated `Pool` and
confirms `Len()` never exceeds `Cap()` under `-race`.

Create `connpool_test.go`:

```go
package connpool

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeConn is a test double for a pooled connection: Healthy is toggled by
// the test, and Close records that it ran so a test can assert a broken
// connection was actually released instead of silently dropped.
type fakeConn struct {
	id      int
	healthy bool
	closed  bool
}

func (c *fakeConn) Healthy() bool { return c.healthy }
func (c *fakeConn) Close() error  { c.closed = true; return nil }

// naiveRelease is Pool.Release as it is usually written the first time: it
// stores whatever connection it is given back into the ring without ever
// calling Healthy. It is unexported and unreachable from the package API;
// it exists only so the tests can pin what it gets wrong. Once a broken
// connection is released through it, that same broken connection is what
// the next Acquire hands out.
func naiveRelease[T Conn](p *Pool[T], conn T) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.size == len(p.items) {
		return
	}
	p.items[p.head] = conn
	p.head = (p.head + 1) % len(p.items)
	p.size++
}

func TestNewRejectsNonPositiveCapacity(t *testing.T) {
	t.Parallel()
	for _, capacity := range []int{0, -1} {
		if _, err := New[*fakeConn](capacity); !errors.Is(err, ErrInvalidCapacity) {
			t.Errorf("New(%d) error = %v, want ErrInvalidCapacity", capacity, err)
		}
	}
}

func TestAcquireOnEmptyPoolReturnsErrPoolEmpty(t *testing.T) {
	t.Parallel()

	p, err := New[*fakeConn](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("Acquire on empty pool: err = %v, want ErrPoolEmpty", err)
	}
}

func TestReleaseThenAcquireRoundTrips(t *testing.T) {
	t.Parallel()

	p, err := New[*fakeConn](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &fakeConn{id: 1, healthy: true}
	if err := p.Release(c); err != nil {
		t.Fatalf("Release: %v", err)
	}
	got, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got != c {
		t.Fatalf("Acquire returned a different connection than was released")
	}
	if c.closed {
		t.Fatal("healthy connection was closed on Release; it should stay open for reuse")
	}
}

func TestReleaseUnhealthyConnectionIsClosedAndDiscarded(t *testing.T) {
	t.Parallel()

	p, err := New[*fakeConn](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	broken := &fakeConn{id: 1, healthy: false}
	if err := p.Release(broken); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !broken.closed {
		t.Fatal("unhealthy connection was not closed on Release")
	}
	if p.Len() != 0 {
		t.Fatalf("Len() = %d, want 0: an unhealthy connection must not enter the pool", p.Len())
	}
	if _, err := p.Acquire(); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("Acquire after releasing only an unhealthy connection: err = %v, want ErrPoolEmpty", err)
	}
}

func TestReleaseAtCapacityClosesTheExtraConnection(t *testing.T) {
	t.Parallel()

	p, err := New[*fakeConn](1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	kept := &fakeConn{id: 1, healthy: true}
	extra := &fakeConn{id: 2, healthy: true}
	if err := p.Release(kept); err != nil {
		t.Fatalf("Release(kept): %v", err)
	}
	if err := p.Release(extra); err != nil {
		t.Fatalf("Release(extra): %v", err)
	}
	if !extra.closed {
		t.Fatal("extra connection released at capacity was not closed; its socket leaked")
	}
	if kept.closed {
		t.Fatal("the connection already in the pool must not be closed by a later Release")
	}
	if p.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", p.Len())
	}
}

func TestAcquireOrdersOldestFirst(t *testing.T) {
	t.Parallel()

	p, err := New[*fakeConn](3)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first := &fakeConn{id: 1, healthy: true}
	second := &fakeConn{id: 2, healthy: true}
	_ = p.Release(first)
	_ = p.Release(second)

	got, err := p.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.id != first.id {
		t.Fatalf("Acquire() returned id %d, want %d (oldest released first)", got.id, first.id)
	}
}

// TestNaiveReleaseRecirculatesBrokenConnection is the whole point of the
// module: given the identical sequence of a broken Release followed by an
// Acquire, the naive path hands the same broken connection right back out,
// while Pool.Release closes it and Acquire correctly reports the pool
// empty.
func TestNaiveReleaseRecirculatesBrokenConnection(t *testing.T) {
	t.Parallel()

	naivePool, err := New[*fakeConn](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	broken := &fakeConn{id: 1, healthy: false}
	naiveRelease(naivePool, broken)

	got, err := naivePool.Acquire()
	if err != nil {
		t.Fatalf("Acquire after naiveRelease: %v", err)
	}
	if got.Healthy() {
		t.Fatal("test setup error: connection should be unhealthy")
	}
	// This is the bug: the naive path handed back a connection it never
	// checked, and Acquire faithfully returns it anyway.

	correctPool, err := New[*fakeConn](2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	brokenToo := &fakeConn{id: 2, healthy: false}
	if err := correctPool.Release(brokenToo); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := correctPool.Acquire(); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("Acquire after Release of a broken connection: err = %v, want ErrPoolEmpty", err)
	}
}

func TestPoolIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	p, err := New[*fakeConn](8)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 8; i++ {
		_ = p.Release(&fakeConn{id: i, healthy: true})
	}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.Acquire()
			if err != nil {
				return
			}
			_ = p.Release(c)
		}()
	}
	wg.Wait()

	if p.Len() > p.Cap() {
		t.Fatalf("Len() = %d exceeds Cap() = %d after concurrent use", p.Len(), p.Cap())
	}
}

// ExamplePool_Acquire is the runnable demonstration of this module: go test
// executes it and compares its stdout against the Output comment below.
func ExamplePool_Acquire() {
	p, err := New[*fakeConn](2)
	if err != nil {
		panic(err)
	}

	_ = p.Release(&fakeConn{id: 1, healthy: true})
	_ = p.Release(&fakeConn{id: 2, healthy: false}) // closed and discarded, not stored

	c, err := p.Acquire()
	if err != nil {
		panic(err)
	}
	fmt.Println("acquired id", c.id, "healthy", c.healthy)

	if _, err := p.Acquire(); errors.Is(err, ErrPoolEmpty) {
		fmt.Println("second acquire: pool empty")
	}

	// Output:
	// acquired id 1 healthy true
	// second acquire: pool empty
}
```

## Review

`Pool[T]` is correct when a connection returned through `Release` is either
genuinely reusable or closed -- never simply stored on faith. `Release` gets
that right with two checks: `Healthy()` before anything else, and a
capacity check that also closes rather than drops. The bug this module
exists to name is treating `Release` as pure ring bookkeeping, storing
whatever `Acquire` will later hand back out without ever asking whether it
still works; one connection that goes bad in production then poisons every
future request that draws it, indefinitely, because nothing ever takes it
out of rotation. Around that core, `New` rejects a non-positive capacity
with `ErrInvalidCapacity`, `Acquire` on an empty pool returns `ErrPoolEmpty`
rather than blocking, and `Pool[T]` locks internally so it is safe to share
across every handler goroutine. `ExamplePool_Acquire` is the executable
documentation: `go test` verifies its output. Run
`go test -count=1 -race ./...`.

## Resources

- [`database/sql`: `DB.SetMaxIdleConns`](https://pkg.go.dev/database/sql#DB.SetMaxIdleConns) — the standard library's own bounded idle-connection pool, and the `Cap()`-at-capacity discard this module models.
- [pgxpool](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool) — a production connection pool that health-checks connections before handing them out.
- [`io.Closer`](https://pkg.go.dev/io#Closer) — the standard interface this module's `Conn.Close` mirrors, for resources that must be released exactly once.
- [Go Wiki: CodeReviewComments — interfaces](https://go.dev/wiki/CodeReviewComments#interfaces) — why `Conn` is a small, consumer-defined interface rather than a concrete `*sql.DB`-shaped type.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [15-idempotency-key-dedup-window.md](15-idempotency-key-dedup-window.md) | Next: [17-lock-free-spsc-atomic-ring.md](17-lock-free-spsc-atomic-ring.md)
