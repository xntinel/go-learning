# Exercise 28: Connection Pool Acquire With Latency Metrics

**Nivel: Avanzado** — validacion normal (tabla de casos, incluye borde o concurrencia).

A connection pool that only returns `(*Conn, error)` hides the single most
useful signal for diagnosing production slowness: how long callers actually
waited. A pool running fine and a pool one query away from falling over both
return a valid connection — the difference only shows up in the wait time.
This exercise builds `Pool.Acquire(ctx) (conn *Conn, acquireTime
time.Duration, error)`, using an injected clock so both the fast-path and
the exhaustion-path latency are asserted deterministically, with no real
sleeping in the test suite.

This module is fully self-contained: its own `go mod init`, all code inline,
its own demo and tests.

## What you'll build

```text
pgpool/                     independent module: example.com/postgres-pool-acquire-metrics
  go.mod                    go 1.24
  pgpool.go                 package pgpool; Clock; Conn; Pool; NewPool; Acquire(ctx) (conn,acquireTime,error); Release
  cmd/
    demo/
      main.go               fast acquire, exhaustion under a real short timeout, acquire after release
  pgpool_test.go            fake clock; fast success elapsed; exhaustion elapsed+error; release then acquire; concurrent acquire/release (-race)
```

- Files: `pgpool.go`, `cmd/demo/main.go`, `pgpool_test.go`.
- Implement: `(*Pool).Acquire(ctx context.Context) (conn *Conn, acquireTime time.Duration, err error)` backed by a buffered channel of `*Conn`, measuring elapsed time with an injected `Clock` so it never depends on real wall-clock timing in tests, and reporting a non-nil error (with `acquireTime` still populated) when `ctx` is done before a connection frees up.
- Test: a connection available immediately reports the exact fake-clock delta; draining the pool and acquiring against an already-canceled context deterministically hits the exhaustion path and reports both the elapsed wait and a non-nil error; releasing a connection makes a subsequent `Acquire` succeed; many goroutines acquiring and releasing concurrently never observe a negative `acquireTime` and pass under `-race`.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/pgpool/cmd/demo
cd ~/go-exercises/pgpool
go mod init example.com/postgres-pool-acquire-metrics
go mod edit -go=1.24
```

### Why the pool needs a clock you can control

A test that wants to assert "acquiring under contention takes noticeably
longer than acquiring with a free connection" is tempted to reach for
`time.Sleep` and `time.Now()`. That makes the test slow and, worse,
flaky: a loaded CI runner can make a "fast" acquire take longer than a
hardcoded threshold meant to represent "slow", failing a passing
implementation. Injecting a `Clock` interface removes real time from the
picture entirely — the test controls exactly what `Now()` returns on each
call, so "5ms elapsed" and "2s elapsed" are assertions about the code's
logic, not about the machine running the test.

The exhaustion path gets the same treatment from the other direction: instead
of waiting out a real timeout to prove `Acquire` gives up correctly, the
test passes an **already-canceled** `context.Context`. The `select` inside
`Acquire` then fires on `ctx.Done()` on its very first evaluation — the
exhaustion behavior is exercised with zero real elapsed time, while the
*reported* `acquireTime` still reflects exactly one fake-clock step, proving
the metric is computed on every return path, not just the success one.

Create `pgpool.go`:

```go
package pgpool

import (
	"context"
	"fmt"
	"time"
)

// Clock abstracts time.Now so tests can drive Acquire's latency measurement
// deterministically instead of depending on real elapsed wall-clock time.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Conn is a placeholder for a real *sql.Conn / pgx connection.
type Conn struct {
	ID int
}

// Pool is a fixed-size connection pool backed by a buffered channel, which
// is itself safe for concurrent use -- no separate mutex is needed to guard
// the pool of available connections.
type Pool struct {
	conns chan *Conn
	clock Clock
}

// NewPool creates a pool of size connections using the real wall clock.
func NewPool(size int) *Pool {
	return newPool(size, realClock{})
}

func newPool(size int, clock Clock) *Pool {
	conns := make(chan *Conn, size)
	for i := 0; i < size; i++ {
		conns <- &Conn{ID: i}
	}
	return &Pool{conns: conns, clock: clock}
}

// Acquire waits for an available connection, returning it along with how
// long the wait took and, if the context is done before a connection frees
// up, a non-nil error reporting pool exhaustion. acquireTime is measured
// even on the failure path, so callers can distinguish "fast success",
// "slow success" (contention worth alerting on), and "gave up after Xms"
// (the pool is undersized for current load).
func (p *Pool) Acquire(ctx context.Context) (conn *Conn, acquireTime time.Duration, err error) {
	start := p.clock.Now()
	select {
	case c := <-p.conns:
		return c, p.clock.Now().Sub(start), nil
	case <-ctx.Done():
		return nil, p.clock.Now().Sub(start), fmt.Errorf("acquire connection: pool exhausted: %w", ctx.Err())
	}
}

// Release returns conn to the pool. It must be called exactly once per
// successful Acquire.
func (p *Pool) Release(conn *Conn) {
	p.conns <- conn
}
```

### The runnable demo

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"time"

	pgpool "example.com/postgres-pool-acquire-metrics"
)

func main() {
	pool := pgpool.NewPool(2)

	conn1, wait, err := pool.Acquire(context.Background())
	fmt.Printf("acquire 1: got=%t err=%v (fast, no contention)\n", conn1 != nil, err)
	_ = wait

	conn2, _, err := pool.Acquire(context.Background())
	fmt.Printf("acquire 2: got=%t err=%v\n", conn2 != nil, err)

	// The pool is now exhausted: both connections are checked out. A third
	// Acquire against a short-lived context must give up and report it.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	conn3, wait, err := pool.Acquire(ctx)
	fmt.Printf("acquire 3: got=%t waited>=20ms=%t err!=nil=%t\n", conn3 != nil, wait >= 20*time.Millisecond, err != nil)

	pool.Release(conn1)
	conn4, _, err := pool.Acquire(context.Background())
	fmt.Printf("acquire 4 (after release): got=%t err=%v\n", conn4 != nil, err)
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
acquire 1: got=true err=<nil> (fast, no contention)
acquire 2: got=true err=<nil>
acquire 3: got=false waited>=20ms=true err!=nil=true
acquire 4 (after release): got=true err=<nil>
```

### Tests

Create `pgpool_test.go`:

```go
package pgpool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock returns strictly increasing timestamps, one step apart, driven
// by an atomic counter so it is safe to call from many goroutines at once
// without ever needing to sleep in a test.
type fakeClock struct {
	base time.Time
	step time.Duration
	n    atomic.Int64
}

func newFakeClock(step time.Duration) *fakeClock {
	return &fakeClock{base: time.Unix(0, 0), step: step}
}

func (c *fakeClock) Now() time.Time {
	i := c.n.Add(1) - 1
	return c.base.Add(time.Duration(i) * c.step)
}

func TestAcquireFastSuccessReportsElapsed(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(5 * time.Millisecond)
	pool := newPool(1, clock)

	conn, elapsed, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conn == nil {
		t.Fatal("conn = nil, want a connection")
	}
	if elapsed != 5*time.Millisecond {
		t.Fatalf("elapsed = %v, want 5ms", elapsed)
	}
}

func TestAcquireExhaustedPoolReportsErrorAndElapsed(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(2 * time.Second)
	pool := newPool(1, clock)

	// Drain the only connection so the pool is exhausted.
	if _, _, err := pool.Acquire(context.Background()); err != nil {
		t.Fatalf("draining acquire: %v", err)
	}

	// An already-canceled context makes the exhaustion path deterministic:
	// the select fires on ctx.Done() immediately, no real waiting involved.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	conn, elapsed, err := pool.Acquire(ctx)
	if conn != nil {
		t.Fatalf("conn = %v, want nil on exhaustion", conn)
	}
	if err == nil {
		t.Fatal("want a non-nil error when the pool is exhausted")
	}
	if elapsed != 2*time.Second {
		t.Fatalf("elapsed = %v, want 2s (one fake-clock step)", elapsed)
	}
}

func TestAcquireAfterReleaseSucceeds(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Millisecond)
	pool := newPool(1, clock)

	conn, _, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	pool.Release(conn)

	_, _, err = pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
}

func TestAcquireConcurrentIsRaceFreeAndNonNegative(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Microsecond)
	pool := newPool(3, clock)

	const workers = 20
	var wg sync.WaitGroup
	var negativeElapsed atomic.Bool
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, elapsed, err := pool.Acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if elapsed < 0 {
				negativeElapsed.Store(true)
			}
			pool.Release(conn)
		}()
	}
	wg.Wait()

	if negativeElapsed.Load() {
		t.Fatal("observed a negative acquireTime under concurrent access")
	}
}
```

## Review

`Acquire` is correct when `acquireTime` is populated on *every* return
path, not just the happy one — a pool that only times successful acquires
cannot tell you how long it kept callers waiting before it gave up, which
is precisely the number an on-call engineer needs during an incident.
`TestAcquireExhaustedPoolReportsErrorAndElapsed` is the load-bearing test:
it proves the exhaustion path still measures elapsed time using the
already-canceled-context trick, with no reliance on real timeouts.
`TestAcquireConcurrentIsRaceFreeAndNonNegative` then shows the design scales
to real contention — the channel itself serializes access to the connection
pool, so no additional mutex is needed around it, and the atomic-counter
fake clock proves the reported durations stay sane under concurrent load.

The mistake to avoid is computing `acquireTime` only inside the success
branch and returning a zero-value duration on the timeout branch — that
looks harmless until an on-call dashboard reports "0ms to exhaustion"
during an actual pool-exhaustion incident, hiding the exact information the
metric exists to surface.

## Resources

- [context.Context](https://pkg.go.dev/context#Context) — the `Done()`/`Err()` pattern used to detect pool exhaustion.
- [database/sql: connection pooling](https://pkg.go.dev/database/sql#DB.SetMaxOpenConns) — the real-world pool this exercise's `Acquire` models.
- [pgxpool.Pool.Acquire](https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool#Pool.Acquire) — a production Postgres pool with the same context-bounded acquire shape.

---

Back to [00-concepts.md](00-concepts.md) | Prev: [27-ldap-directory-attribute-extract.md](27-ldap-directory-attribute-extract.md) | Next: [29-rate-limit-quota-display.md](29-rate-limit-quota-display.md)
