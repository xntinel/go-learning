# Exercise 5: Why defer Leaks Resources in Parallel Tests

The single most common teardown bug in Go tests is a `defer` in a test that spawns
parallel subtests. The parent function returns before the subtests run, so the
`defer` releases a shared resource while the subtests still need it. This exercise
makes the bug concrete with a bounded connection pool that tracks live usage, and
shows the `t.Cleanup` fix that releases at the right instant.

This module is fully self-contained: its own `go mod init`, its own demo, its own
tests. Nothing here imports any other exercise.

## What you'll build

```text
connpool/                    independent module: example.com/connpool
  go.mod                     go 1.24
  pool.go                    bounded Pool, Acquire/Release, live InUse counter
  cmd/
    demo/
      main.go                runnable demo: acquire, observe InUse, release
  pool_test.go               shared-pool parallel subtests with t.Cleanup release
```

- Files: `pool.go`, `cmd/demo/main.go`, `pool_test.go`.
- Implement: a `Pool` with a bounded capacity, `Acquire` returning a `Conn`, `Conn.Release`, and an `InUse` counter backed by `atomic.Int64`.
- Test: a parent test that shares one acquired connection across parallel subtests and releases it via `t.Cleanup`; a per-subtest variant where each subtest acquires and releases its own; a parent cleanup asserting the pool drained to zero.
- Verify: `go test -count=1 -race ./...`

Set up the module:

```bash
mkdir -p ~/go-exercises/connpool/cmd/demo
cd ~/go-exercises/connpool
go mod init example.com/connpool
go mod edit -go=1.24
```

### The bug: defer in a parent that spawns parallel subtests

Here is the shape that leaks, written as a comment so it does not compile into the
module (it is the wrong version):

```text
// BAD: defer releases the shared conn before the parallel subtests run.
func TestGroup(t *testing.T) {
	conn := pool.Acquire()
	defer conn.Release()        // fires when TestGroup returns

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()        // queued; runs AFTER TestGroup returns
			conn.Do(name)       // uses a conn that was already released
		})
	}
	// TestGroup returns here -> defer conn.Release() fires -> conn freed
	// THEN the parallel subtests finally run, on a released connection.
}
```

The mechanism is the one from the concepts file: calling `t.Run` with a subtest
that calls `t.Parallel()` does not run the subtest inline — it queues it and returns
immediately. The parent's `for` loop finishes, the parent function returns, and its
`defer conn.Release()` fires. Only *then* does the test runner resume the queued
parallel subtests, which now operate on a connection whose `Release` already ran.
With a real pool this manifests as a connection returned to the pool and handed to
another test while this one still holds it — a heisenbug that surfaces only under
`-parallel`.

The fix is `t.Cleanup`. A cleanup registered on the parent runs after *all* of its
subtests complete, so the shared connection stays live for the entire duration the
subtests need it and is released exactly once, at the end.

Create `pool.go`:

```go
package connpool

import "sync/atomic"

// Pool is a bounded connection pool. Acquire blocks when the pool is exhausted;
// Release returns a slot. InUse reports the live count.
type Pool struct {
	sem   chan struct{}
	inUse atomic.Int64
}

// New returns a pool with the given capacity.
func New(size int) *Pool {
	return &Pool{sem: make(chan struct{}, size)}
}

// Acquire takes a slot, blocking if the pool is at capacity.
func (p *Pool) Acquire() *Conn {
	p.sem <- struct{}{}
	p.inUse.Add(1)
	return &Conn{pool: p}
}

// InUse reports how many connections are currently checked out.
func (p *Pool) InUse() int64 {
	return p.inUse.Load()
}

// Conn is a checked-out connection. Release is idempotent.
type Conn struct {
	pool     *Pool
	released atomic.Bool
}

// Release returns the connection to the pool exactly once.
func (c *Conn) Release() {
	if c.released.Swap(true) {
		return
	}
	c.pool.inUse.Add(-1)
	<-c.pool.sem
}

// Do simulates using the connection for a unit of work.
func (c *Conn) Do(query string) string {
	return "ok:" + query
}
```

### The runnable demo

The demo acquires two connections from a pool of two, shows the live count, uses
one, then releases both.

Create `cmd/demo/main.go`:

```go
package main

import (
	"fmt"

	"example.com/connpool"
)

func main() {
	pool := connpool.New(2)

	c1 := pool.Acquire()
	c2 := pool.Acquire()
	fmt.Printf("in use: %d\n", pool.InUse())

	fmt.Println(c1.Do("SELECT 1"))

	c1.Release()
	c2.Release()
	fmt.Printf("in use after release: %d\n", pool.InUse())
}
```

Run it:

```bash
go run ./cmd/demo
```

Expected output:

```
in use: 2
ok:SELECT 1
in use after release: 0
```

### The tests

`TestSharedConnStaysLiveAcrossSubtests` is the corrected shape: the parent acquires
one connection, releases it via `t.Cleanup`, and the parallel subtests assert the
connection is still checked out (`InUse() >= 1`) while they run. Because the cleanup
runs after all subtests, the shared connection never disappears mid-subtest — the
exact failure the `defer` version would produce. `TestPerSubtestConn` shows the
other correct pattern, where each parallel subtest owns its connection and releases
it with `t.Cleanup`, and a parent cleanup asserts the pool fully drained.

Create `pool_test.go`:

```go
package connpool

import (
	"fmt"
	"testing"
)

func TestSharedConnStaysLiveAcrossSubtests(t *testing.T) {
	pool := New(8)
	conn := pool.Acquire()
	// t.Cleanup releases at TEST exit, after every parallel subtest completes.
	t.Cleanup(func() {
		conn.Release()
		if got := pool.InUse(); got != 0 {
			t.Errorf("pool in use = %d after release, want 0", got)
		}
	})

	for _, name := range []string{"reader-a", "reader-b", "reader-c", "reader-d"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// If a defer in the parent had released the shared conn early, this
			// would observe InUse() == 0. With t.Cleanup it stays checked out.
			if pool.InUse() < 1 {
				t.Errorf("shared connection was released before subtest %q ran", name)
			}
			_ = conn.Do(name)
		})
	}
}

func TestPerSubtestConn(t *testing.T) {
	pool := New(8)
	for _, name := range []string{"w1", "w2", "w3", "w4"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conn := pool.Acquire()
			t.Cleanup(conn.Release)
			_ = conn.Do(name)
		})
	}
	// Runs after all subtests (and their cleanups) complete.
	t.Cleanup(func() {
		if got := pool.InUse(); got != 0 {
			t.Errorf("pool leaked: in use = %d, want 0", got)
		}
	})
}

func ExamplePool() {
	p := New(1)
	c := p.Acquire()
	fmt.Println(p.InUse())
	c.Release()
	fmt.Println(p.InUse())
	// Output:
	// 1
	// 0
}
```

## Review

The corrected pattern is correct when the shared connection is observably live
throughout every parallel subtest and drained to zero exactly at test end —
`TestSharedConnStaysLiveAcrossSubtests` asserts both. The bug it prevents is the
canonical parallel-`defer` leak: the parent function returns as soon as it has
queued the subtests, so a `defer` there releases the resource before the subtests
run. `t.Cleanup` is the only hook that fires after the subtests, which is why every
shared fixture for parallel tests must use it. Run `go test -race`; the `atomic`
counter and the semaphore channel make the shared-pool access clean under the race
detector.

## Resources

- [`testing.T.Parallel`](https://pkg.go.dev/testing#T.Parallel) — how parallel subtests are queued and resumed after the parent returns.
- [`testing.T.Cleanup`](https://pkg.go.dev/testing#T.Cleanup) — runs after the test and all its subtests finish.
- [`sync/atomic.Int64`](https://pkg.go.dev/sync/atomic#Int64) — the race-free live counter.

---

Back to [00-concepts.md](00-concepts.md) | Previous: [04-lifo-teardown-order.md](04-lifo-teardown-order.md) | Next: [06-context-aware-worker-shutdown.md](06-context-aware-worker-shutdown.md)
