# Exercise 6: Bounding Parallel Tests Against a Limited Connection Pool

When you flip a whole suite to `t.Parallel()`, the tests do not just contend for
CPU — they contend for the scarce shared resources the code touches: a database
connection pool with a fixed cap, ephemeral ports, file descriptors. Exceed the
cap and you get failures that look like logic bugs but are pure contention. The
fix is a package-level semaphore sized to the resource, so parallelism speeds the
suite up without ever exceeding what the pool can serve. This module builds that.

This module is fully self-contained: its own module, demo, and tests.

## What you'll build

```text
poolbound/                  independent module: example.com/poolbound
  go.mod
  pool.go                   Pool with a hard max; Repo.Fetch; ErrPoolExhausted
  cmd/
    demo/
      main.go               runnable demo: fill the pool, observe exhaustion
  pool_test.go              parallel tests bounded by a package-level semaphore
```

Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
Implement: a `Pool` that errors with `ErrPoolExhausted` past `max` concurrent
acquisitions, and a `Repo` whose `Fetch` acquires, works, and releases.
Test: parallel tests that each acquire a package-level weighted semaphore
(sized to the pool) via `t.Cleanup`, asserting `ErrPoolExhausted` never occurs.
Verify: `go test -count=1 -race ./...`

### Why -parallel and GOMAXPROCS are the wrong knob alone

The default parallelism is `GOMAXPROCS`, i.e. CPU count. But the scarce resource is
almost never CPU — it is the pool of `max` connections your test DB allows, or the
handful of ephemeral ports a test server can bind. If `max` is 4 but the CI box has
16 cores, the default lets 16 parallel tests each try to grab a connection and the
5th one gets `ErrPoolExhausted`. You could clamp with `-parallel 4`, but that is
coarse: it bounds *tests*, not *acquisitions*, so a test that grabs two connections
still overshoots, and a package with both pool-touching and pool-free tests is
throttled unnecessarily. The precise instrument counts the thing that is actually
limited: a weighted semaphore sized to the pool, acquired only by tests that touch
the pool.

### The semaphore, and why release goes in t.Cleanup

A counting semaphore admits at most N holders at once; here N equals the pool's
`max`. The standard library way is a buffered channel of capacity N: sending on it
acquires a slot (blocking when full), receiving releases one. (The named library
`golang.org/x/sync/semaphore` offers a weighted, context-aware version with the
same idea; the buffered channel keeps this module dependency-free.) The semaphore
is a *package-level* variable so it is shared across every parallel test in the
package — that is the whole point, since the pool is shared too.

Release must happen even if the test fails a `t.Fatal`, so it goes in `t.Cleanup`,
not a `defer` after the assertions (a `t.Fatal` runs the test's cleanups but skips
the rest of the function body, so a trailing `defer` after the fatal never runs —
`t.Cleanup` always does). Acquire before touching the pool; register the release;
then do the work.

The unbounded variant, for contrast (do not use it): if the parallel tests skip
the semaphore and just call `repo.Fetch` concurrently, then with more parallel
tests than `max` connections, some `Fetch` calls return `ErrPoolExhausted`
intermittently — a flake that appears only when the CI machine is busy enough to
run enough tests at once. The semaphore removes the flake by construction.

Create `pool.go`:

```go
package poolbound

import (
	"context"
	"errors"
	"strconv"
	"sync"
)

// ErrPoolExhausted is returned when all connections are in use.
var ErrPoolExhausted = errors.New("connection pool exhausted")

// Pool models a resource with a hard concurrency cap, like a database driver's
// connection pool. Acquiring past max fails instead of blocking, so overshoot
// is visible rather than hidden.
type Pool struct {
	mu     sync.Mutex
	active int
	max    int
}

// NewPool returns a pool allowing max concurrent connections.
func NewPool(max int) *Pool {
	return &Pool{max: max}
}

// Conn is a checked-out connection; call Release to return it.
type Conn struct {
	pool *Pool
}

// Acquire checks out a connection or returns ErrPoolExhausted if the cap is hit.
func (p *Pool) Acquire(ctx context.Context) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active >= p.max {
		return nil, ErrPoolExhausted
	}
	p.active++
	return &Conn{pool: p}, nil
}

// Release returns the connection to the pool.
func (c *Conn) Release() {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	c.pool.active--
}

// InUse reports how many connections are currently checked out.
func (p *Pool) InUse() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.active
}

// Repo is a repository backed by a Pool.
type Repo struct {
	pool *Pool
}

// NewRepo returns a Repo over the given pool.
func NewRepo(pool *Pool) *Repo {
	return &Repo{pool: pool}
}

// Fetch acquires a connection, "queries", and releases. It returns
// ErrPoolExhausted if no connection is available.
func (r *Repo) Fetch(ctx context.Context, id int) (string, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Release()
	return "row-" + strconv.Itoa(id), nil
}
```

### The runnable demo

The demo shows the pool refusing the (max+1)-th concurrent acquisition, which is
the failure the semaphore exists to prevent in tests.

Create `cmd/demo/main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"example.com/poolbound"
)

func main() {
	pool := poolbound.NewPool(2)
	ctx := context.Background()

	c1, _ := pool.Acquire(ctx)
	c2, _ := pool.Acquire(ctx)
	fmt.Printf("in use: %d\n", pool.InUse())

	_, err := pool.Acquire(ctx)
	fmt.Println("third acquire:", errors.Is(err, poolbound.ErrPoolExhausted))

	c1.Release()
	c2.Release()
	fmt.Printf("after release: %d\n", pool.InUse())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in use: 2
third acquire: true
after release: 0
```

The pool holds two connections, refuses the third with `ErrPoolExhausted` (so
`errors.Is` prints `true`), and returns to zero in use once both are released.

### Tests

`poolMax` is the pool cap; the package-level `sem` is a buffered channel of the
same capacity. Each parallel test sends on `sem` to acquire a slot, registers
`t.Cleanup` to receive (release), and only then calls `repo.Fetch`. Because at
most `poolMax` tests hold a slot at once, the pool never sees more than `poolMax`
concurrent acquisitions, so `ErrPoolExhausted` never occurs. `TestPoolRejectsOvershoot`
checks the pool's own contract directly.

Create `pool_test.go`:

```go
package poolbound

import (
	"context"
	"errors"
	"testing"
)

const poolMax = 4

// Package-level semaphore sized to the shared pool: it bounds how many parallel
// tests touch the pool at once, regardless of GOMAXPROCS or -parallel.
var (
	pool = NewPool(poolMax)
	sem  = make(chan struct{}, poolMax)
)

// acquireSlot bounds this test against the pool and releases via Cleanup, which
// runs even if the test calls t.Fatal.
func acquireSlot(t *testing.T) {
	t.Helper()
	sem <- struct{}{}
	t.Cleanup(func() { <-sem })
}

func TestRepoBounded(t *testing.T) {
	t.Parallel()

	repo := NewRepo(pool)
	for i := range 32 {
		t.Run("query", func(t *testing.T) {
			t.Parallel()
			acquireSlot(t) // never exceed poolMax concurrent pool users

			got, err := repo.Fetch(t.Context(), i)
			if err != nil {
				t.Fatalf("Fetch(%d): %v (pool exhausted means the bound failed)", i, err)
			}
			if got == "" {
				t.Fatalf("Fetch(%d): empty result", i)
			}
		})
	}
}

func TestPoolRejectsOvershoot(t *testing.T) {
	t.Parallel()

	p := NewPool(1)
	ctx := context.Background()
	c, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := p.Acquire(ctx); !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("second acquire = %v, want ErrPoolExhausted", err)
	}
	c.Release()
	if _, err := p.Acquire(ctx); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}
```

## Review

The bound is correct when the package-level semaphore's capacity equals the pool's
`max`, and every parallel test that touches the pool acquires a slot before use and
releases it in `t.Cleanup`. The evidence: `TestRepoBounded` fans out 32 parallel
subtests against a pool of 4 and never sees `ErrPoolExhausted`, because the
semaphore caps concurrent pool users at 4 no matter how many subtests the runner
schedules. Remove the `acquireSlot` call and, on a machine with enough cores, the
suite starts failing intermittently with `ErrPoolExhausted` — the contention flake
the bound exists to kill.

The mental model: bound parallelism against the *scarce resource*, not against CPU.
`-parallel` and `GOMAXPROCS` are blunt; a semaphore sized to the pool is precise
because it counts exactly the acquisitions that can overflow.

## Resources

- [`golang.org/x/sync/semaphore`](https://pkg.go.dev/golang.org/x/sync/semaphore) — the weighted, context-aware semaphore this pattern generalizes to.
- [go command Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags) — `-parallel` and `-p`, the coarse bounds.
- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — release that runs even after `t.Fatal`.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [05-race-on-parallel-ttl-cache.md](05-race-on-parallel-ttl-cache.md) | Next: [07-parallel-cleanup-ordering-and-context.md](07-parallel-cleanup-ordering-and-context.md)
